// Copyright Red Hat
package registeredcluster

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ghodss/yaml"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	apisv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/apis/v1alpha1"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterapiv1 "open-cluster-management.io/api/cluster/v1"
	manifestworkv1 "open-cluster-management.io/api/work/v1"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	authv1alpha1 "open-cluster-management.io/managed-serviceaccount/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kcpclient "github.com/kcp-dev/apimachinery/pkg/client"
	"github.com/kcp-dev/logicalcluster"
	"k8s.io/apimachinery/pkg/runtime"
	clusteradmapply "open-cluster-management.io/clusteradm/pkg/helpers/apply"
	clusteradmasset "open-cluster-management.io/clusteradm/pkg/helpers/asset"

	"github.com/stolostron/compute-operator/config"
	croconfig "github.com/stolostron/compute-operator/config"
	"github.com/stolostron/compute-operator/hack"
	"github.com/stolostron/compute-operator/pkg/helpers"
	"github.com/stolostron/compute-operator/test"

	singaporev1alpha1 "github.com/stolostron/compute-operator/api/singapore/v1alpha1"
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

const (
	// The compute workspace
	workspace string = "user-workspace"
	// The controller service account on the compute
	controllerComputeServiceAccount string = "compute-operator"
	// the namespace on the compute
	workingComputeNamespace string = "default"
	// The controller namespace
	controllerNamespace string = "controller-ns"
	// The compute organization
	computeOrganization string = "default"
	// The compute cluster name
	clusterName string = "root:" + computeOrganization + ":" + workspace
	// The registered cluster name
	registeredClusterName string = "registered-cluster"
	// The compute kubeconfig file
	adminComputeKubeconfigFile string = ".kcp/admin.kubeconfig"
	// The directory for test environment assets
	testEnvDir string = ".testenv"
	// the test environment kubeconfig file
	testEnvKubeconfigFile string = testEnvDir + "/testenv.kubeconfig"
	// the main executable
	controllerExecutable string = testEnvDir + "/manager"
	// The service account compute kubeconfig file
	saComputeKubeconfigFile string = testEnvDir + "/kubeconfig-" + controllerComputeServiceAccount + ".yaml"

	// The compute kubeconfig secret name on the controller cluster
	computeKubeconfigSecret string = "kcp-kubeconfig"
)

// Set it to true when using a actual cluster
var existingConfig bool = false

var (
	controllerRestConfig       *rest.Config
	r                          *RegisteredClusterReconciler
	computeContext             context.Context
	testEnv                    *envtest.Environment
	scheme                     = runtime.NewScheme()
	controllerManager          *exec.Cmd
	controllerRuntimeClient    client.Client
	controllerApplierBuilder   *clusteradmapply.ApplierBuilder
	computeServer              *exec.Cmd
	computeAdminApplierBuilder *clusteradmapply.ApplierBuilder
	computeVWApplierBuilder    *clusteradmapply.ApplierBuilder
	computeRuntimeClient       client.Client
	readerTest                 *clusteradmasset.ScenarioResourcesReader
	readerHack                 *clusteradmasset.ScenarioResourcesReader
	readerConfig               *clusteradmasset.ScenarioResourcesReader
	saComputeKubeconfigFileAbs string

	cancelCompute           chan bool = make(chan bool)
	cancelControllerManager chan bool = make(chan bool)
)

func TestAPIs(t *testing.T) {
	RegisterFailHandler(Fail)

	// fetch the current config
	suiteConfig, reporterConfig := GinkgoConfiguration()
	// adjust it
	suiteConfig.SkipStrings = []string{"NEVER-RUN"}
	reporterConfig.FullTrace = true
	RunSpecs(t,
		"Controller Suite",
		reporterConfig)
}

var _ = BeforeSuite(func() {
	logf.SetLogger(klog.NewKlogr())

	values := struct {
		KcpKubeconfigSecret string
		Organization        string
		Workspace           string
	}{
		KcpKubeconfigSecret: computeKubeconfigSecret,
		Organization:        computeOrganization,
		Workspace:           workspace,
	}

	// DeferCleanup(cleanup)

	By("bootstrapping test environment")
	err := clientgoscheme.AddToScheme(scheme)
	Expect(err).Should(BeNil())
	err = appsv1.AddToScheme(scheme)
	Expect(err).Should(BeNil())
	err = clusterapiv1.AddToScheme(scheme)
	Expect(err).Should(BeNil())
	err = singaporev1alpha1.AddToScheme(scheme)
	Expect(err).Should(BeNil())
	err = addonv1alpha1.AddToScheme(scheme)
	Expect(err).Should(BeNil())
	err = authv1alpha1.AddToScheme(scheme)
	Expect(err).Should(BeNil())
	err = manifestworkv1.AddToScheme(scheme)
	Expect(err).Should(BeNil())
	err = apisv1alpha1.AddToScheme(scheme)
	Expect(err).Should(BeNil())

	readerIDP := croconfig.GetScenarioResourcesReader()
	clusterRegistrarsCRD, err := getCRD(readerIDP, "crd/singapore.open-cluster-management.io_clusterregistrars.yaml")
	Expect(err).Should(BeNil())

	hubConfigsCRD, err := getCRD(readerIDP, "crd/singapore.open-cluster-management.io_hubconfigs.yaml")
	Expect(err).Should(BeNil())

	registeredClustersCRD, err := getCRD(readerIDP, "crd/singapore.open-cluster-management.io_registeredclusters.yaml")
	Expect(err).Should(BeNil())

	// Clean testEnv Directory
	os.RemoveAll(testEnvDir)
	err = os.MkdirAll(testEnvDir, 0700)
	Expect(err).To(BeNil())

	testEnv = &envtest.Environment{
		Scheme: scheme,
		CRDs: []*apiextensionsv1.CustomResourceDefinition{
			clusterRegistrarsCRD,
			hubConfigsCRD,
			registeredClustersCRD,
		},
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "test", "config", "crd", "external"),
		},
		ErrorIfCRDPathMissing:    true,
		AttachControlPlaneOutput: true,
		ControlPlaneStartTimeout: 1 * time.Minute,
		ControlPlaneStopTimeout:  1 * time.Minute,
		UseExistingCluster:       &existingConfig,
	}

	var hubKubeconfigString string
	var hubKubeconfig *rest.Config
	if *testEnv.UseExistingCluster {
		hubKubeconfigString, hubKubeconfig, err = persistAndGetRestConfig(*testEnv.UseExistingCluster)
		Expect(err).ToNot(HaveOccurred())
		testEnv.Config = hubKubeconfig
	}

	controllerRestConfig, err = testEnv.Start()
	Expect(err).ToNot(HaveOccurred())
	Expect(controllerRestConfig).ToNot(BeNil())

	if !*testEnv.UseExistingCluster {
		hubKubeconfigString, hubKubeconfig, err = persistAndGetRestConfig(*testEnv.UseExistingCluster)
		Expect(err).ToNot(HaveOccurred())
	}

	controllerRuntimeClient, err = client.New(controllerRestConfig, client.Options{Scheme: scheme})
	Expect(err).ToNot(HaveOccurred())
	Expect(controllerRuntimeClient).ToNot(BeNil())

	//Build hub applier
	controllerKubernetesClient := kubernetes.NewForConfigOrDie(controllerRestConfig)
	controllerAPIExtensionClient := apiextensionsclient.NewForConfigOrDie(controllerRestConfig)
	controllerDynamicClient := dynamic.NewForConfigOrDie(controllerRestConfig)
	controllerApplierBuilder = clusteradmapply.NewApplierBuilder().
		WithClient(controllerKubernetesClient, controllerAPIExtensionClient, controllerDynamicClient)

	readerTest = test.GetScenarioResourcesReader()
	readerHack = hack.GetScenarioResourcesReader()
	readerConfig = config.GetScenarioResourcesReader()
	// Clean kcp
	os.RemoveAll(".kcp")

	By(fmt.Sprintf("creation of namepsace %s", controllerNamespace), func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: controllerNamespace,
			},
		}
		err := controllerRuntimeClient.Create(context.TODO(), ns)
		Expect(err).To(BeNil())
	})

	By("Create a hubconfig secret", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-hub-kube-config",
				Namespace: controllerNamespace,
			},
			Data: map[string][]byte{
				"kubeconfig": []byte(hubKubeconfigString),
			},
		}
		err = controllerRuntimeClient.Create(context.TODO(), secret)
		Expect(err).To(BeNil())
	})

	var hubConfig *singaporev1alpha1.HubConfig
	By("Create a HubConfig", func() {
		hubConfig = &singaporev1alpha1.HubConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-hubconfig",
				Namespace: controllerNamespace,
			},
			Spec: singaporev1alpha1.HubConfigSpec{
				KubeConfigSecretRef: corev1.LocalObjectReference{
					Name: "my-hub-kube-config",
				},
			},
		}
		err := controllerRuntimeClient.Create(context.TODO(), hubConfig)
		Expect(err).To(BeNil())
	})

	go func() {
		defer GinkgoRecover()
		adminComputeKubeconfigFile, err := filepath.Abs(adminComputeKubeconfigFile)
		os.Setenv("KUBECONFIG", adminComputeKubeconfigFile)
		computeServer = exec.Command("kcp",
			"start",
		)

		// kcpServer.Stdout = os.Stdout
		// kcpServer.Stderr = os.Stderr
		err = computeServer.Start()
		Expect(err).To(BeNil())
	}()

	// Create workspace on compute server and enter in the ws
	By(fmt.Sprintf("creation of workspace %s", workspace), func() {
		Eventually(func() error {
			logf.Log.Info("create workspace")
			cmd := exec.Command("kubectl-kcp",
				"ws",
				"create",
				workspace,
				"--enter")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err = cmd.Run()
			if err != nil {
				logf.Log.Error(err, "while create workspace")
			}
			return err
		}, 60, 3).Should(BeNil())
	})

	// Create SA on compute server in workspace
	By(fmt.Sprintf("creation of SA %s in workspace %s", controllerComputeServiceAccount, workspace), func() {
		Eventually(func() error {
			logf.Log.Info("create service account")
			cmd := exec.Command("kubectl",
				"create",
				"serviceaccount",
				controllerComputeServiceAccount,
				"-n",
				workingComputeNamespace)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err = cmd.Run()
			if err != nil {
				logf.Log.Error(err, "while create managerServiceAccount")
			}
			return err
		}, 60, 3).Should(BeNil())
	})

	saComputeKubeconfigFileAbs, err = filepath.Abs(saComputeKubeconfigFile)
	Expect(err).To(BeNil())

	By(fmt.Sprintf("generate kubeconfig for sa %s in workspace %s", controllerComputeServiceAccount, workspace), func() {
		Eventually(func() error {
			logf.Log.Info(saComputeKubeconfigFile)
			adminComputeKubeconfigFile, err := filepath.Abs(adminComputeKubeconfigFile)
			os.Setenv("KUBECONFIG", adminComputeKubeconfigFile)
			cmd := exec.Command("../../build/generate_kubeconfig_from_sa.sh",
				controllerComputeServiceAccount,
				workingComputeNamespace,
				saComputeKubeconfigFileAbs)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err = cmd.Run()
			if err != nil {
				logf.Log.Error(err, "while generating sa kubeconfig")
			}
			return err
		}, 60, 3).Should(BeNil())
	})

	computeKubconfigData, err := ioutil.ReadFile(saComputeKubeconfigFile)
	Expect(err).ToNot(HaveOccurred())
	computeRestConfig, err := clientcmd.RESTConfigFromKubeConfig(computeKubconfigData)
	Expect(err).ToNot(HaveOccurred())
	computeRuntimeClient, err = client.New(computeRestConfig, client.Options{})
	Expect(err).ToNot(HaveOccurred())

	computeAdminKubconfigData, err := ioutil.ReadFile(adminComputeKubeconfigFile)
	Expect(err).ToNot(HaveOccurred())
	computeAdminRestConfig, err := clientcmd.RESTConfigFromKubeConfig(computeAdminKubconfigData)
	Expect(err).ToNot(HaveOccurred())

	computeContext = kcpclient.WithCluster(context.Background(), logicalcluster.New(clusterName))
	//Build compute admin applier
	computeAdminKubernetesClient := kubernetes.NewForConfigOrDie(computeAdminRestConfig)
	computeAdminAPIExtensionClient := apiextensionsclient.NewForConfigOrDie(computeAdminRestConfig)
	computeAdminDynamicClient := dynamic.NewForConfigOrDie(computeAdminRestConfig)
	computeAdminApplierBuilder = clusteradmapply.NewApplierBuilder().
		WithClient(computeAdminKubernetesClient, computeAdminAPIExtensionClient, computeAdminDynamicClient).
		WithContext(computeContext)

	// Create role for on compute server in workspace
	By(fmt.Sprintf("creation of role in workspace %s", workspace), func() {
		Eventually(func() error {
			logf.Log.Info("create role")
			computeApplier := computeAdminApplierBuilder.Build()
			files := []string{
				"compute/role.yaml",
			}
			_, err := computeApplier.ApplyDirectly(readerHack, nil, false, "", files...)
			if err != nil {
				logf.Log.Error(err, "while create role")
			}
			return err
		}, 60, 3).Should(BeNil())
	})

	// Create rolebinding for on compute server in workspace
	By(fmt.Sprintf("creation of rolebinding in workspace %s", workspace), func() {
		Eventually(func() error {
			logf.Log.Info("create role binding")
			computeApplier := computeAdminApplierBuilder.Build()
			files := []string{
				"compute/role_binding.yaml",
			}
			_, err := computeApplier.ApplyDirectly(readerHack, nil, false, "", files...)
			if err != nil {
				logf.Log.Error(err, "while create role binding")
			}
			if err != nil {
				logf.Log.Error(err, "while create role binding")
			}
			return err
		}, 60, 3).Should(BeNil())
	})

	By(fmt.Sprintf("apply resourceschema on workspace %s", workspace), func() {
		Eventually(func() error {
			logf.Log.Info("create resourceschema")
			computeApplier := computeAdminApplierBuilder.Build()
			files := []string{
				"apiresourceschema/singapore.open-cluster-management.io_registeredclusters.yaml",
			}
			_, err := computeApplier.ApplyCustomResources(readerConfig, nil, false, "", files...)
			if err != nil {
				logf.Log.Error(err, "while create role binding")
			}
			if err != nil {
				logf.Log.Error(err, "while applying resourceschema")
			}
			return err
		}, 60, 3).Should(BeNil())
	})

	By(fmt.Sprintf("apply APIExport on workspace %s", workspace), func() {
		Eventually(func() error {
			logf.Log.Info("create APIExport")
			computeApplier := computeAdminApplierBuilder.Build()
			files := []string{
				"compute/apiexport.yaml",
			}
			_, err := computeApplier.ApplyCustomResources(readerHack, nil, false, "", files...)
			if err != nil {
				logf.Log.Error(err, "while applying apiexport")
			}
			return err
		}, 60, 3).Should(BeNil())
	})

	// var computeVirtualWorkspaceRestConfig *rest.Config
	// By("waiting virtualworkspace", func() {
	// 	Eventually(func() error {
	// 		logf.Log.Info("waiting vitual workspace")
	// 		computeVirtualWorkspaceRestConfig, err = helpers.RestConfigForAPIExport(context.TODO(), computeRestConfig, "compute-apis", scheme)
	// 		return err
	// 	}, 60, 3).Should(BeNil())
	// })

	By(fmt.Sprintf("apply APIBinding on workspace %s", workspace), func() {
		Eventually(func() error {
			logf.Log.Info("create APIBinding")
			computeApplier := computeAdminApplierBuilder.Build()
			files := []string{
				"compute/apibinding.yaml",
			}
			_, err := computeApplier.ApplyCustomResources(readerHack, values, false, "", files...)
			// cmd := exec.Command("kubectl",
			// 	"apply",
			// 	"-f",
			// 	"../../test/resources/compute/apibinding.yaml")
			// cmd.Stdout = os.Stdout
			// cmd.Stderr = os.Stderr
			// err = cmd.Run()
			if err != nil {
				logf.Log.Error(err, "while applying APIBinding")
			}
			return err
		}, 60, 3).Should(BeNil())
	})

	var computeVWeRestConfig *rest.Config
	By("waiting virtualworkspace", func() {
		Eventually(func() error {
			logf.Log.Info("waiting vitual workspace")
			computeVWeRestConfig, err = helpers.RestConfigForAPIExport(context.TODO(), computeRestConfig, "compute-apis", scheme)
			return err
		}, 60, 3).Should(BeNil())
	})

	//Build compute admin applier
	computeVWKubernetesClient := kubernetes.NewForConfigOrDie(computeVWeRestConfig)
	computeVWAPIExtensionClient := apiextensionsclient.NewForConfigOrDie(computeVWeRestConfig)
	computeVWDynamicClient := dynamic.NewForConfigOrDie(computeVWeRestConfig)
	computeVWApplierBuilder = clusteradmapply.NewApplierBuilder().
		WithClient(computeVWKubernetesClient, computeVWAPIExtensionClient, computeVWDynamicClient).
		WithContext(computeContext)

	By("Create a kcpconfig secret", func() {
		b, err := ioutil.ReadFile(adminComputeKubeconfigFile)
		Expect(err).To(BeNil())
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      computeKubeconfigSecret,
				Namespace: controllerNamespace,
			},
			Data: map[string][]byte{
				"kubeconfig": b,
			},
		}
		err = controllerRuntimeClient.Create(context.TODO(), secret)
		Expect(err).To(BeNil())
	})

	By("Create the clusterRegistrar", func() {
		logf.Log.Info("apply clusterRegistrar")
		applier := controllerApplierBuilder.
			Build()
		files := []string{
			"resources/compute/clusterRegistrar.yaml",
		}

		_, err := applier.ApplyCustomResources(readerTest, values, false, "", files...)
		Expect(err).To(BeNil())
	})

	go func() {
		defer GinkgoRecover()
		logf.Log.Info("build controller")
		build := exec.Command("go",
			"build",
			"-o",
			controllerExecutable,
			"../../main.go")
		err := build.Run()
		Expect(err).To(BeNil())

		logf.Log.Info("run controller")
		os.Setenv("POD_NAME", "installer-pod")
		os.Setenv("POD_NAMESPACE", controllerNamespace)
		controllerManager = exec.Command(controllerExecutable,
			"manager",
			"--kubeconfig",
			testEnvKubeconfigFile,
			"--v=6",
		)

		controllerManager.Stdout = os.Stdout
		controllerManager.Stderr = os.Stderr
		err = controllerManager.Start()
		Expect(err).To(BeNil())
	}()

})

func cleanup() {
	if controllerManager != nil {
		By("tearing down the manager")
		logf.Log.Info("Process", "Args", controllerManager.Args)
		controllerManager.Process.Signal(os.Interrupt)
		Eventually(func() error {
			if err := controllerManager.Process.Signal(os.Interrupt); err != nil {
				logf.Log.Error(err, "while tear down the manager")
				return err
			}
			return nil
		}, 60, 3).Should(BeNil())
		controllerManager.Process.Signal(os.Interrupt)
		// controllerManager.Wait()
	}
	if computeServer != nil {
		By("tearing down the kcp")
		computeServer.Process.Signal(os.Interrupt)
		Eventually(func() error {
			if err := computeServer.Process.Signal(os.Interrupt); err != nil {
				logf.Log.Error(err, "while tear down the kcp")
				return err
			}
			return nil
		}, 60, 3).Should(BeNil())
		// computeServer.Wait()
	}
	if testEnv != nil {
		By("tearing down the test environment")
		err := testEnv.Stop()
		Expect(err).NotTo(HaveOccurred())
	}
}

var _ = AfterSuite(func() {
	cleanup()
})

var _ = Describe("Process registeredCluster: ", func() {
	It("Process registeredCluster", func() {
		var registeredCluster *singaporev1alpha1.RegisteredCluster
		By("Create the RegisteredCluster", func() {
			Eventually(func() error {
				logf.Log.Info("apply registeredCluster")
				cmd := exec.Command("kubectl",
					"apply",
					"-f",
					"../../test/resources/compute/registeredCluster.yaml")
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				err := cmd.Run()
				if err != nil {
					logf.Log.Error(err, "while applying registeredCluster")
				}
				return err
			}, 60, 3).Should(BeNil())
			registeredCluster = &singaporev1alpha1.RegisteredCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "registered-cluster",
					Namespace: workingComputeNamespace,
				},
				Spec: singaporev1alpha1.RegisteredClusterSpec{
					Location: "FakeKcpLocation",
				},
			}
			// err := hubRuntimeClient.Create(context.TODO(), registeredCluster)
			// Expect(err).To(BeNil())
		})
		var managedCluster *clusterapiv1.ManagedCluster
		By("Checking managedCluster", func() {
			Eventually(func() error {
				managedClusters := &clusterapiv1.ManagedClusterList{}

				if err := controllerRuntimeClient.List(context.TODO(),
					managedClusters,
					client.MatchingLabels{
						RegisteredClusterNamelabel:      registeredCluster.Name,
						RegisteredClusterNamespacelabel: registeredCluster.Namespace,
					}); err != nil {
					logf.Log.Info("Waiting managedCluster", "Error", err)
					return err
				}
				if len(managedClusters.Items) != 1 {
					return fmt.Errorf("Number of managedCluster found %d", len(managedClusters.Items))
				}
				managedCluster = &managedClusters.Items[0]
				return nil
			}, 60, 3).Should(BeNil())
		})
		By("Patching managecluster spec", func() {
			managedCluster.Spec.ManagedClusterClientConfigs = []clusterapiv1.ClientConfig{
				{
					URL:      "https://example.com:443",
					CABundle: []byte("cabbundle"),
				},
			}
			err := controllerRuntimeClient.Update(context.TODO(), managedCluster)
			Expect(err).Should(BeNil())
		})
		By("Updating managedcluster label", func() {
			managedCluster.ObjectMeta.Labels["clusterID"] = "8bcc855c-259f-46fd-adda-485ef99f2438"
			err := controllerRuntimeClient.Update(context.TODO(), managedCluster)
			Expect(err).Should(BeNil())
		})
		By("Patching managedcluster status", func() {

			// patch := client.MergeFrom(managedCluster.DeepCopy())
			managedCluster.Status.Conditions = []metav1.Condition{
				{
					Type:               clusterapiv1.ManagedClusterConditionAvailable,
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					Reason:             "Succeeded",
					Message:            "Managedcluster succeeded",
				},
				{
					Type:               clusterapiv1.ManagedClusterConditionJoined,
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					Reason:             "Joined",
					Message:            "Managedcluster joined",
				},
			}
			managedCluster.Status.Allocatable = clusterapiv1.ResourceList{
				clusterapiv1.ResourceCPU:    *apiresource.NewQuantity(2, apiresource.DecimalSI),
				clusterapiv1.ResourceMemory: *apiresource.NewQuantity(2, apiresource.DecimalSI),
			}
			managedCluster.Status.Capacity = clusterapiv1.ResourceList{
				clusterapiv1.ResourceCPU:    *apiresource.NewQuantity(1, apiresource.DecimalSI),
				clusterapiv1.ResourceMemory: *apiresource.NewQuantity(1, apiresource.DecimalSI),
			}
			managedCluster.Status.Version.Kubernetes = "1.19.2"
			managedCluster.Status.ClusterClaims = []clusterapiv1.ManagedClusterClaim{
				{
					Name:  "registeredCluster",
					Value: registeredCluster.Name,
				},
			}
			err := controllerRuntimeClient.Status().Update(context.TODO(), managedCluster)
			Expect(err).Should(BeNil())
		})
		By("Create managedcluster namespace", func() {
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: managedCluster.Name,
				},
			}
			err := controllerRuntimeClient.Create(context.TODO(), ns)
			Expect(err).To(BeNil())
		})
		importSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      managedCluster.Name + "-import",
				Namespace: managedCluster.Name,
			},
			Data: map[string][]byte{
				"crds.yaml":        []byte("my-crds.yaml"),
				"crdsv1.yaml":      []byte("my-crdsv1.yaml"),
				"crdsv1beta1.yaml": []byte("my-crdsv1beta1.yaml"),
				"import.yaml":      []byte("my-import.yaml"),
			},
		}
		By("Create import secret", func() {
			err := controllerRuntimeClient.Create(context.TODO(), importSecret)
			Expect(err).To(BeNil())
		})
		By("Checking registeredCluster ImportCommandRef", func() {
			Eventually(func() error {
				err := computeRuntimeClient.Get(computeContext,
					types.NamespacedName{
						Name:      registeredCluster.Name,
						Namespace: registeredCluster.Namespace,
					},
					registeredCluster)
				if err != nil {
					return err
				}
				if registeredCluster.Status.ImportCommandRef.Name != registeredCluster.Name+"-import" {
					return fmt.Errorf("Get %s instead of %s",
						registeredCluster.Status.ImportCommandRef.Name,
						registeredCluster.Name+"-import")
				}
				return nil
			}, 30, 1).Should(BeNil())
		})
		cm := &corev1.ConfigMap{}
		importCommand :=
			`echo "bXktY3Jkc3YxLnlhbWw=" | base64 --decode | kubectl apply -f - && ` +
				`sleep 2 && ` +
				`echo "bXktaW1wb3J0LnlhbWw=" | base64 --decode | kubectl apply -f -
`

		By("Checking import configMap", func() {
			Eventually(func() error {
				err := controllerRuntimeClient.Get(context.TODO(),
					types.NamespacedName{
						Name:      registeredCluster.Status.ImportCommandRef.Name,
						Namespace: registeredCluster.Namespace,
					},
					cm)
				if err != nil {
					return err
				}
				if cm.Data["importCommand"] != importCommand {
					return fmt.Errorf("invalid import expect %s, got %s", importCommand, cm.Data["importCommand"])
				}
				return nil
			}, 30, 1).Should(BeNil())
		})
		By("Checking registeredCluster status", func() {
			Eventually(func() error {
				err := controllerRuntimeClient.Get(context.TODO(),
					types.NamespacedName{
						Name:      registeredCluster.Name,
						Namespace: registeredCluster.Namespace,
					},
					registeredCluster)
				if err != nil {
					return err
				}

				if len(registeredCluster.Status.Conditions) == 0 {
					return fmt.Errorf("Expecting 1 condtions got 0")
				}
				if q, ok := registeredCluster.Status.Allocatable[clusterapiv1.ResourceCPU]; !ok {
					return fmt.Errorf("Expecting Allocatable ResourceCPU exists")
				} else {
					if v, ok := q.AsInt64(); !ok || v != 2 {
						return fmt.Errorf("Expecting Allocatable ResourceCPU equal 2, got %d", v)
					}
				}
				if q, ok := registeredCluster.Status.Capacity[clusterapiv1.ResourceCPU]; !ok {
					return fmt.Errorf("Expecting Allocatable ResourceCPU exists")
				} else {
					if v, ok := q.AsInt64(); !ok || v != 1 {
						return fmt.Errorf("Expecting Allocatable ResourceCPU equal 1, got %d", v)
					}
				}
				if registeredCluster.Status.Version.Kubernetes != "1.19.2" {
					return fmt.Errorf("Expecting Version 1.19.2, got %s", registeredCluster.Status.Version)
				}
				if len(registeredCluster.Status.ClusterClaims) != 1 {
					return fmt.Errorf("Expecting 1 ClusterClaim got 0")
				}
				if registeredCluster.Status.ClusterID == "" {
					return fmt.Errorf("Expecting clusterID to be not empty")
				}
				return nil
			}, 60, 1).Should(BeNil())
		})
		By("Checking managedclusteraddon", func() {
			Eventually(func() error {
				managedClusterAddon := &addonv1alpha1.ManagedClusterAddOn{}

				if err := controllerRuntimeClient.Get(context.TODO(),
					types.NamespacedName{
						Name:      ManagedClusterAddOnName,
						Namespace: managedCluster.Name,
					},
					managedClusterAddon); err != nil {
					logf.Log.Info("Waiting managedClusteraddon", "Error", err)
					return err
				}
				return nil
			}, 30, 1).Should(BeNil())
		})

		By("Checking managedserviceaccount", func() {
			Eventually(func() error {
				managed := &authv1alpha1.ManagedServiceAccount{}

				if err := controllerRuntimeClient.Get(context.TODO(),
					types.NamespacedName{
						Name:      ManagedServiceAccountName,
						Namespace: managedCluster.Name,
					},
					managed); err != nil {
					logf.Log.Info("Waiting managedserviceaccount", "Error", err)
					return err
				}
				return nil
			}, 30, 1).Should(BeNil())
		})

		By("Checking manifestwork", func() {
			Eventually(func() error {
				manifestwork := &manifestworkv1.ManifestWork{}

				err := controllerRuntimeClient.Get(context.TODO(),
					types.NamespacedName{
						Name:      ManagedServiceAccountName,
						Namespace: managedCluster.Name,
					},
					manifestwork)
				if err != nil {
					logf.Log.Info("Waiting manifestwork", "Error", err)
					return err
				}
				return nil
			}, 60, 5).Should(BeNil())
		})

		By("Patching manifestwork status", func() {

			manifestwork := &manifestworkv1.ManifestWork{}

			err := controllerRuntimeClient.Get(context.TODO(),
				types.NamespacedName{
					Name:      ManagedServiceAccountName,
					Namespace: managedCluster.Name,
				},
				manifestwork)
			Expect(err).Should(BeNil())

			manifestwork.Status.Conditions = []metav1.Condition{
				{
					Type:               manifestworkv1.WorkApplied,
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					Reason:             "Applied",
					Message:            "Manifestwork applied",
				},
			}
			err = controllerRuntimeClient.Update(context.TODO(), manifestwork)
			Expect(err).Should(BeNil())
		})

		By("Create managedserviceaccount secret", func() {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ManagedServiceAccountName,
					Namespace: managedCluster.Name,
				},
				Data: map[string][]byte{
					"token":  []byte("token"),
					"ca.crt": []byte("ca-cert"),
				},
			}
			err := controllerRuntimeClient.Create(context.TODO(), secret)
			Expect(err).To(BeNil())
		})

		By("Deleting registeredcluster", func() {
			Eventually(func() error {
				registeredCluster := &singaporev1alpha1.RegisteredCluster{}

				err := controllerRuntimeClient.Get(context.TODO(),
					types.NamespacedName{
						Name:      "registered-cluster",
						Namespace: workspace,
					},
					registeredCluster)
				if err != nil {
					return err
				}

				if err := controllerRuntimeClient.Delete(context.TODO(),
					registeredCluster); err != nil {
					logf.Log.Info("Waiting deletion of registeredcluster", "Error", err)
					return err
				}
				return nil
			}, 60, 1).Should(BeNil())
		})

	})

})

func getCRD(reader *clusteradmasset.ScenarioResourcesReader, file string) (*apiextensionsv1.CustomResourceDefinition, error) {
	b, err := reader.Asset(file)
	if err != nil {
		return nil, err
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(b, crd); err != nil {
		return nil, err
	}
	return crd, nil
}

func persistAndGetRestConfig(useExistingCluster bool) (string, *rest.Config, error) {
	var err error
	buf := new(strings.Builder)
	if useExistingCluster {
		cmd := exec.Command("kubectl", "config", "view", "--raw")
		cmd.Stdout = buf
		cmd.Stderr = buf
		err = cmd.Run()
	} else {
		var out io.Reader
		out, _, err = testEnv.ControlPlane.KubeCtl().Run("config", "view", "--raw")
		Expect(err).To(BeNil())
		_, err = io.Copy(buf, out)
	}
	if err := ioutil.WriteFile(testEnvKubeconfigFile, []byte(buf.String()), 0644); err != nil {
		return "", nil, err
	}

	hubKubconfigData, err := ioutil.ReadFile(testEnvKubeconfigFile)
	if err != nil {
		return "", nil, err
	}
	hubKubeconfig, err := clientcmd.RESTConfigFromKubeConfig(hubKubconfigData)
	if err != nil {
		return "", nil, err
	}
	return buf.String(), hubKubeconfig, err
}
