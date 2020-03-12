package test

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/blang/semver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha3"
	"sigs.k8s.io/kind/pkg/cluster"

	"github.com/mesosphere/kubeaddons/pkg/api/v1beta1"
	"github.com/mesosphere/kubeaddons/pkg/catalog"
	"github.com/mesosphere/kubeaddons/pkg/repositories"
	"github.com/mesosphere/kubeaddons/pkg/repositories/git"
	"github.com/mesosphere/kubeaddons/pkg/repositories/local"
	"github.com/mesosphere/kubeaddons/pkg/test"
	"github.com/mesosphere/kubeaddons/pkg/test/cluster/kind"
	"github.com/mesosphere/kubeaddons/pkg/test/loadable"
)

const (
	kbaURL    = "https://github.com/mesosphere/kubernetes-base-addons"
	kbaRef    = "master"
	kbaRemote = "origin"

	controllerBundle         = "https://mesosphere.github.io/kubeaddons/bundle.yaml"
	defaultKubernetesVersion = "1.16.4"
	patchStorageClass        = `{"metadata": {"annotations":{"storageclass.kubernetes.io/is-default-class":"false"}}}`
)

var (
	cat       catalog.Catalog
	localRepo repositories.Repository
	groups    map[string][]v1beta1.AddonInterface
)

func init() {
	var err error
	localRepo, err = local.NewRepository("local", "../addons/")
	if err != nil {
		panic(err)
	}

	kbaRepo, err := git.NewRemoteRepository(kbaURL, kbaRef, kbaRemote)
	if err != nil {
		panic(err)
	}

	cat, err = catalog.NewCatalog(localRepo, kbaRepo)
	if err != nil {
		panic(err)
	}

	groups, err = test.AddonsForGroupsFile("groups.yaml", cat)
	if err != nil {
		panic(err)
	}

}

func TestValidateUnhandledAddons(t *testing.T) {
	unhandled, err := findUnhandled()
	if err != nil {
		t.Fatal(err)
	}

	if len(unhandled) != 0 {
		names := make([]string, len(unhandled))
		for _, addon := range unhandled {
			names = append(names, addon.GetName())
		}
		t.Fatal(fmt.Errorf("the following addons are not handled as part of a testing group: %+v", names))
	}
}

func TestKommanderGroup(t *testing.T) {
	if err := testgroup(t, "kommander"); err != nil {
		t.Fatal(err)
	}
}

// -----------------------------------------------------------------------------
// Private Functions
// -----------------------------------------------------------------------------

func testgroup(t *testing.T, groupname string) error {
	t.Logf("testing group %s", groupname)

	version, err := semver.Parse(defaultKubernetesVersion)
	if err != nil {
		return err
	}

	cluster, err := kind.NewCluster(version, cluster.CreateWithV1Alpha3Config(&v1alpha3.Cluster{}))
	if err != nil {
		// try to clean up in case cluster was created and reference available
		if cluster != nil {
			_ = cluster.Cleanup()
		}
		return err
	}
	defer cluster.Cleanup()

	if err := kubectl("apply", "-f", controllerBundle); err != nil {
		return err
	}

	addons := groups[groupname]
	for _, addon := range addons {
		overrides(addon)
	}

	wg := &sync.WaitGroup{}
	stop := make(chan struct{})
	go test.LoggingHook(t, cluster, wg, stop)

	addonDeployment, err := loadable.DeployAddons(t, cluster, addons...)
	if err != nil {
		return err
	}

	addonCleanup, err := loadable.CleanupAddons(t, cluster, addons...)
	if err != nil {
		return err
	}

	addonDefaults, err := loadable.WaitForAddons(t, cluster, addons...)
	if err != nil {
		return err
	}

	th := test.NewSimpleTestHarness(t)
	th.Load(loadable.ValidateAddons(addons...), addonDeployment, addonDefaults, addonCleanup)

	testFunc := func(t *testing.T) error {
		if err := kubectl("apply", "-f", "./artifacts/thanos-checker.yaml"); err != nil {
			return err
		}

		succeeded := false
		timeout := time.Now().Add(time.Minute * 1)
		for timeout.After(time.Now()) {
			job, err := cluster.Client().BatchV1().Jobs("default").Get("thanos-checker", metav1.GetOptions{})
			if err != nil {
				return err
			}
			if job.Status.Succeeded == 1 {
				succeeded = true
				break
			}
			time.Sleep(time.Second * 1)
		}

		if !succeeded {
			return fmt.Errorf("thanos checker job did not succeed within timeout")
		}
		t.Log("thanos checker job succeeded 🙃")
		return nil
	}
	th.Load(test.Loadable{Plan: test.DefaultPlan, Jobs: test.Jobs{testFunc}})

	defer th.Cleanup()
	th.Validate()
	th.Deploy()
	th.Default()

	close(stop)
	wg.Wait()

	return nil
}

func findUnhandled() ([]v1beta1.AddonInterface, error) {
	var unhandled []v1beta1.AddonInterface
	repo, err := local.NewRepository("base", "../addons")
	if err != nil {
		return unhandled, err
	}
	addons, err := repo.ListAddons()
	if err != nil {
		return unhandled, err
	}

	for _, revisions := range addons {
		addon := revisions[0]
		found := false
		for _, v := range groups {
			for _, r := range v {
				if r.GetName() == addon.GetName() {
					found = true
				}
			}
		}
		if !found {
			unhandled = append(unhandled, addon)
		}
	}

	return unhandled, nil
}

func kubectl(args ...string) error {
	cmd := exec.Command("kubectl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// -----------------------------------------------------------------------------
// Private - CI Values Overrides
// -----------------------------------------------------------------------------

// TODO: a temporary place to put configuration overrides for addons
// See: https://jira.mesosphere.com/browse/DCOS-62137
func overrides(addon v1beta1.AddonInterface) {
	if v, ok := addonOverrides[addon.GetName()]; ok {
		addon.GetAddonSpec().ChartReference.Values = &v
	}
}

var addonOverrides = map[string]string{
	"metallb": `
---
configInline:
  address-pools:
  - name: default
    protocol: layer2
    addresses:
    - "172.17.1.200-172.17.1.250"
`,
}
