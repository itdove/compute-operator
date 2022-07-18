// Copyright Red Hat
package test

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ghodss/yaml"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

	apimachineryclient "github.com/kcp-dev/apimachinery/pkg/client"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	authv1alpha1 "open-cluster-management.io/managed-serviceaccount/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kcp-dev/logicalcluster"
	clusteradmapply "open-cluster-management.io/clusteradm/pkg/helpers/apply"
	clusteradmasset "open-cluster-management.io/clusteradm/pkg/helpers/asset"

	croconfig "github.com/stolostron/compute-operator/config"
	"github.com/stolostron/compute-operator/pkg/helpers"
	"github.com/stolostron/compute-operator/resources"

	// testresources "github.com/stolostron/compute-operator/test"

	singaporev1alpha1 "github.com/stolostron/compute-operator/api/singapore/v1alpha1"
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

const (
	// The compute workspace
	workspace string = "my-ws"
	// The controller service account on the compute
	controllerComputeServiceAccount string = "compute-operator"
	// the namespace on the compute
	controllerComputeServiceAccountNamespace string = "sa-ws"
	// The compute organization
	computeOrganization string = "my-org"
	// The compute organization workspace
	organizationWorkspace string = "root:" + computeOrganization
	// The compute cluster workspace
	clusterWorkspace string = organizationWorkspace + ":" + workspace
	// The compute kubeconfig file
	adminComputeKubeconfigFile string = ".kcp/admin.kubeconfig"
	// The directory for test environment assets
	testEnvDir string = ".testenv"
	// the test environment kubeconfig file
	testEnvKubeconfigFile string = testEnvDir + "/testenv.kubeconfig"
	// The service account compute kubeconfig file
	saComputeKubeconfigFile string = testEnvDir + "/kubeconfig-" + controllerComputeServiceAccount + ".yaml"

	// The compute kubeconfig secret name on the controller cluster
	computeKubeconfigSecret string = "kcp-kubeconfig"
)

var (
	controllerRestConfig            *rest.Config
	organizationContext             context.Context
	testEnv                         *envtest.Environment
	computeServer                   *exec.Cmd
	computeAdminApplierBuilder      *clusteradmapply.ApplierBuilder
	organizationAdminApplierBuilder *clusteradmapply.ApplierBuilder
	readerTest                      *clusteradmasset.ScenarioResourcesReader
	readerResources                 *clusteradmasset.ScenarioResourcesReader
	readerConfig                    *clusteradmasset.ScenarioResourcesReader
	saComputeKubeconfigFileAbs      string
	computeAdminKubconfigData       []byte
)

func SetupCompute(scheme *runtime.Scheme, controllerNamespace string) (computeContext context.Context,
	testEnvKubeconfigFilePath string,
	controllerRuntimeClient client.Client,
	computeRuntimeWorkspaceClient client.Client) {
	logf.SetLogger(klog.NewKlogr())

	testEnvKubeconfigFilePath = testEnvKubeconfigFile
	// Generate readers for appliers
	readerTest = GetScenarioResourcesReader()
	readerResources = resources.GetScenarioResourcesReader()
	readerConfig = croconfig.GetScenarioResourcesReader()

	ginkgo.By("bootstrapping test environment")
	err := clientgoscheme.AddToScheme(scheme)
	gomega.Expect(err).Should(gomega.BeNil())
	err = appsv1.AddToScheme(scheme)
	gomega.Expect(err).Should(gomega.BeNil())
	err = clusterapiv1.AddToScheme(scheme)
	gomega.Expect(err).Should(gomega.BeNil())
	err = singaporev1alpha1.AddToScheme(scheme)
	gomega.Expect(err).Should(gomega.BeNil())
	err = addonv1alpha1.AddToScheme(scheme)
	gomega.Expect(err).Should(gomega.BeNil())
	err = authv1alpha1.AddToScheme(scheme)
	gomega.Expect(err).Should(gomega.BeNil())
	err = manifestworkv1.AddToScheme(scheme)
	gomega.Expect(err).Should(gomega.BeNil())
	err = apisv1alpha1.AddToScheme(scheme)
	gomega.Expect(err).Should(gomega.BeNil())

	// Get the CRDs
	clusterRegistrarsCRD, err := GetCRD(readerConfig, "crd/singapore.open-cluster-management.io_clusterregistrars.yaml")
	gomega.Expect(err).Should(gomega.BeNil())

	hubConfigsCRD, err := GetCRD(readerConfig, "crd/singapore.open-cluster-management.io_hubconfigs.yaml")
	gomega.Expect(err).Should(gomega.BeNil())

	registeredClustersCRD, err := GetCRD(readerConfig, "crd/singapore.open-cluster-management.io_registeredclusters.yaml")
	gomega.Expect(err).Should(gomega.BeNil())

	// set useExistingCluster, if set to true then the cluster with
	// the $KUBECONFIG will be used as target instead of the in memory envtest
	useExistingClusterEnvVar := os.Getenv("USE_EXISTING_CLUSTER")
	var existingCluster bool
	if len(useExistingClusterEnvVar) != 0 {
		existingCluster, err = strconv.ParseBool(useExistingClusterEnvVar)
		gomega.Expect(err).To(gomega.BeNil())
	}

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
		AttachControlPlaneOutput: false,
		ControlPlaneStartTimeout: 1 * time.Minute,
		ControlPlaneStopTimeout:  1 * time.Minute,
		UseExistingCluster:       &existingCluster,
	}

	// Clean testEnv Directory
	os.RemoveAll(testEnvDir)
	err = os.MkdirAll(testEnvDir, 0700)
	gomega.Expect(err).To(gomega.BeNil())

	// Set and save the testEnv.Config if using an existing cluster
	var hubKubeconfigString string
	var hubKubeconfig *rest.Config
	if *testEnv.UseExistingCluster {
		hubKubeconfigString, hubKubeconfig, err = PersistAndGetRestConfig(*testEnv.UseExistingCluster)
		gomega.Expect(err).ToNot(gomega.HaveOccurred())
		testEnv.Config = hubKubeconfig
	} else {
		os.Setenv("KUBECONFIG", "")
	}

	// Start the testEnv.
	controllerRestConfig, err = testEnv.Start()
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	gomega.Expect(controllerRestConfig).ToNot(gomega.BeNil())

	// Save the testenv kubeconfig
	if !*testEnv.UseExistingCluster {
		hubKubeconfigString, _, err = PersistAndGetRestConfig(*testEnv.UseExistingCluster)
		gomega.Expect(err).ToNot(gomega.HaveOccurred())
	}

	// Clean kcp
	os.RemoveAll(".kcp")

	// Launch KCP
	go func() {
		defer ginkgo.GinkgoRecover()
		adminComputeKubeconfigFile, err := filepath.Abs(adminComputeKubeconfigFile)
		gomega.Expect(err).To(gomega.BeNil())
		os.Setenv("KUBECONFIG", adminComputeKubeconfigFile)
		computeServer = exec.Command("kcp",
			"start",
			"-v=6",
		)

		// Create io.writer for kcp log
		kcpLogFile := os.Getenv("KCP_LOG")
		if len(kcpLogFile) == 0 {
			computeServer.Stdout = os.Stdout
			computeServer.Stderr = os.Stderr
		} else {
			os.MkdirAll(filepath.Dir(filepath.Clean(kcpLogFile)), 0700)
			f, err := os.OpenFile(filepath.Clean(kcpLogFile), os.O_WRONLY|os.O_CREATE, 0600)
			gomega.Expect(err).To(gomega.BeNil())
			defer f.Close()
			computeServer.Stdout = f
			computeServer.Stderr = f
		}

		err = computeServer.Start()
		gomega.Expect(err).To(gomega.BeNil())
	}()

	// Switch to system:admin context in order to create a kubeconfig allowing KCP API configuration.
	ginkgo.By("switch context system:admin", func() {
		gomega.Eventually(func() error {
			klog.Info("switch context")
			cmd := exec.Command("kubectl",
				"config",
				"use-context",
				"system:admin")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err = cmd.Run()
			if err != nil {
				klog.Error(err, "while switching context")
			}
			return err
		}, 60, 3).Should(gomega.BeNil())
	})

	ginkgo.By("reading the kcpkubeconfig", func() {
		gomega.Eventually(func() error {
			computeAdminKubconfigData, err = ioutil.ReadFile(adminComputeKubeconfigFile)
			return err
		}, 60, 3).Should(gomega.BeNil())
	})

	computeAdminRestConfig, err := clientcmd.RESTConfigFromKubeConfig(computeAdminKubconfigData)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())

	computeAdminRestConfig = apimachineryclient.NewClusterConfig(computeAdminRestConfig)

	// Create the kcp clients for the builder
	computeAdminKubernetesClient := kubernetes.NewForConfigOrDie(computeAdminRestConfig)
	computeAdminAPIExtensionClient := apiextensionsclient.NewForConfigOrDie(computeAdminRestConfig)
	computeAdminDynamicClient := dynamic.NewForConfigOrDie(computeAdminRestConfig)

	// Create a builder for the workspace
	computeAdminApplierBuilder = clusteradmapply.NewApplierBuilder().
		WithClient(computeAdminKubernetesClient,
			computeAdminAPIExtensionClient,
			computeAdminDynamicClient)
	// Create a builder for the organization
	organizationAdminApplierBuilder = clusteradmapply.NewApplierBuilder().
		WithClient(computeAdminKubernetesClient,
			computeAdminAPIExtensionClient,
			computeAdminDynamicClient)

	// Switch to root in order to create the organization workspace
	ginkgo.By("switch context root", func() {
		gomega.Eventually(func() error {
			klog.Info("switch context")
			cmd := exec.Command("kubectl",
				"config",
				"use-context",
				"root")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err = cmd.Run()
			if err != nil {
				klog.Error(err, "while switching context")
			}
			return err
		}, 30, 3).Should(gomega.BeNil())
	})

	// Create workspace on compute server and enter in the ws
	ginkgo.By(fmt.Sprintf("creation of organization %s", computeOrganization), func() {
		gomega.Eventually(func() error {
			klog.Info("create workspace")
			cmd := exec.Command("kubectl-kcp",
				"ws",
				"create",
				computeOrganization,
				"--enter")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err = cmd.Run()
			if err != nil {
				klog.Error(err, "while create organization")
			}
			return err
		}, 60, 3).Should(gomega.BeNil())
	})

	organizationContext = logicalcluster.WithCluster(context.Background(), logicalcluster.New(organizationWorkspace))

	//Build compute admin applier
	ginkgo.By(fmt.Sprintf("apply resourceschema on workspace %s", workspace), func() {
		gomega.Eventually(func() error {
			klog.Info("create resourceschema")
			computeApplier := organizationAdminApplierBuilder.WithContext(organizationContext).Build()
			files := []string{
				"apiresourceschema/singapore.open-cluster-management.io_registeredclusters.yaml",
			}
			_, err := computeApplier.ApplyCustomResources(readerConfig, nil, false, "", files...)
			if err != nil {
				klog.Error(err, "while applying resourceschema")
			}
			return err
		}, 60, 3).Should(gomega.BeNil())
	})

	ginkgo.By(fmt.Sprintf("apply APIExport on workspace %s", workspace), func() {
		gomega.Eventually(func() error {
			klog.Info("create APIExport")
			computeApplier := organizationAdminApplierBuilder.WithContext(organizationContext).Build()
			files := []string{
				"compute-templates/virtual-workspace/apiexport.yaml",
			}
			_, err := computeApplier.ApplyCustomResources(readerResources, nil, false, "", files...)
			if err != nil {
				klog.Error(err, "while applying apiexport")
			}
			return err
		}, 60, 3).Should(gomega.BeNil())
	})

	// Create SA on compute server in workspace
	ginkgo.By(fmt.Sprintf("creation of SA %s in workspace %s", controllerComputeServiceAccount, workspace), func() {
		gomega.Eventually(func() error {
			klog.Info("create namespace")
			computeApplier := computeAdminApplierBuilder.
				WithContext(organizationContext).Build()
			files := []string{
				"compute-templates/virtual-workspace/namespace.yaml",
			}
			values := struct {
				ControllerComputeServiceAccountNamespace string
			}{
				ControllerComputeServiceAccountNamespace: controllerComputeServiceAccountNamespace,
			}
			_, err := computeApplier.ApplyDirectly(readerResources, values, false, "", files...)
			if err != nil {
				klog.Error(err, "while create namespace")
			}
			return err
		}, 60, 3).Should(gomega.BeNil())
		gomega.Eventually(func() error {
			klog.Info("create service account")
			computeApplier := computeAdminApplierBuilder.
				WithContext(organizationContext).Build()
			files := []string{
				"compute-templates/virtual-workspace/service_account.yaml",
			}
			values := struct {
				ControllerComputeServiceAccountNamespace string
			}{
				ControllerComputeServiceAccountNamespace: controllerComputeServiceAccountNamespace,
			}
			_, err := computeApplier.ApplyDirectly(readerResources, values, false, "", files...)
			if err != nil {
				klog.Error(err, "while create namespace")
			}
			return err
		}, 60, 3).Should(gomega.BeNil())
	})

	// Generate the kubeconfig for the SA
	saComputeKubeconfigFileAbs, err = filepath.Abs(saComputeKubeconfigFile)
	gomega.Expect(err).To(gomega.BeNil())

	ginkgo.By(fmt.Sprintf("generate kubeconfig for sa %s in workspace %s", controllerComputeServiceAccount, workspace), func() {
		gomega.Eventually(func() error {
			klog.Info(saComputeKubeconfigFile)
			adminComputeKubeconfigFile, err := filepath.Abs(adminComputeKubeconfigFile)
			gomega.Expect(err).To(gomega.BeNil())
			os.Setenv("KUBECONFIG", adminComputeKubeconfigFile)
			cmd := exec.Command("../../build/generate_kubeconfig_from_sa.sh",
				controllerComputeServiceAccount,
				controllerComputeServiceAccountNamespace,
				saComputeKubeconfigFileAbs)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err = cmd.Run()
			if err != nil {
				klog.Error(err, "while generating sa kubeconfig")
			}
			return err
		}, 60, 3).Should(gomega.BeNil())
	})

	computeContext = logicalcluster.WithCluster(context.Background(), logicalcluster.New(clusterWorkspace))

	// Create role for on compute server in workspace
	ginkgo.By(fmt.Sprintf("creation of role in workspace %s", workspace), func() {
		gomega.Eventually(func() error {
			klog.Info("create role")
			computeApplier := computeAdminApplierBuilder.
				WithContext(organizationContext).Build()
			files := []string{
				"compute-templates/virtual-workspace/role.yaml",
			}
			_, err := computeApplier.ApplyDirectly(readerResources, nil, false, "", files...)
			if err != nil {
				klog.Error(err, "while create role")
			}
			return err
		}, 60, 3).Should(gomega.BeNil())
	})

	// Create rolebinding for on compute server in workspace
	ginkgo.By(fmt.Sprintf("creation of rolebinding in workspace %s", workspace), func() {
		gomega.Eventually(func() error {
			klog.Info("create role binding")
			computeApplier := computeAdminApplierBuilder.
				WithContext(organizationContext).Build()
			files := []string{
				"compute-templates/virtual-workspace/role_binding.yaml",
			}
			values := struct {
				ControllerComputeServiceAccountNamespace string
			}{
				ControllerComputeServiceAccountNamespace: controllerComputeServiceAccountNamespace,
			}
			_, err := computeApplier.ApplyDirectly(readerResources, values, false, "", files...)
			if err != nil {
				klog.Error(err, "while create role binding")
			}
			if err != nil {
				klog.Error(err, "while create role binding")
			}
			return err
		}, 60, 3).Should(gomega.BeNil())
	})

	// Create kcp runtime client for the controller
	// computeSAKubconfigData, err := ioutil.ReadFile(saComputeKubeconfigFile)
	// gomega.Expect(err).ToNot(gomega.HaveOccurred())
	// computeRestSAConfig, err := clientcmd.RESTConfigFromKubeConfig(computeSAKubconfigData)
	// gomega.Expect(err).ToNot(gomega.HaveOccurred())
	// computeRestSAConfig = apimachineryclient.NewClusterConfig(computeRestSAConfig)

	ginkgo.By("waiting virtualworkspace", func() {
		gomega.Eventually(func() error {
			klog.Info("waiting virtual workspace")
			_, err = helpers.RestConfigForAPIExport(organizationContext, computeAdminRestConfig, "compute-apis", scheme)
			return err
		}, 60, 3).Should(gomega.BeNil())
	})

	// Create workspace on compute server and enter in the ws
	ginkgo.By(fmt.Sprintf("creation of cluster workspace %s", workspace), func() {
		gomega.Eventually(func() error {
			klog.Info("create workspace")
			cmd := exec.Command("kubectl-kcp",
				"ws",
				"create",
				workspace,
				"--enter")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err = cmd.Run()
			if err != nil {
				klog.Error(err, "while create cluster workspace")
			}
			return err
		}, 60, 3).Should(gomega.BeNil())
	})

	// Create the APIBinding in the cluster workspace
	ginkgo.By(fmt.Sprintf("apply APIBinding on workspace %s", workspace), func() {
		gomega.Eventually(func() error {
			klog.Info("create APIBinding")
			computeApplier := computeAdminApplierBuilder.
				WithContext(computeContext).Build()
			files := []string{
				"compute-templates/workspace/apibinding.yaml",
			}
			// Values for the appliers
			values := struct {
				Organization string
			}{
				Organization: computeOrganization,
			}
			_, err := computeApplier.ApplyCustomResources(readerResources, values, false, "", files...)
			if err != nil {
				klog.Error(err, "while applying APIBinding")
			}
			return err
		}, 60, 3).Should(gomega.BeNil())
	})

	// Create a runtime client to retrieve information from the hub cluster
	controllerRuntimeClient, err = client.New(controllerRestConfig, client.Options{Scheme: scheme})
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	gomega.Expect(controllerRuntimeClient).ToNot(gomega.BeNil())

	// Create the controller namespace, that ns will hold the controller configuration.
	ginkgo.By(fmt.Sprintf("creation of namespace %s", controllerNamespace), func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: controllerNamespace,
			},
		}
		err := controllerRuntimeClient.Create(context.TODO(), ns)
		gomega.Expect(err).To(gomega.BeNil())
	})

	// Create the hub config secret for the controller
	ginkgo.By("Create a hubconfig secret", func() {
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
		gomega.Expect(err).To(gomega.BeNil())
	})

	// Create a hubConfig CR with that secret
	var hubConfig *singaporev1alpha1.HubConfig
	ginkgo.By("Create a HubConfig", func() {
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
		gomega.Expect(err).To(gomega.BeNil())
	})

	//Build compute admin applier
	ginkgo.By("Create a kcpconfig secret", func() {
		b, err := ioutil.ReadFile(saComputeKubeconfigFileAbs)
		gomega.Expect(err).To(gomega.BeNil())
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
		gomega.Expect(err).To(gomega.BeNil())
	})

	// Create clients for the hub
	controllerKubernetesClient := kubernetes.NewForConfigOrDie(controllerRestConfig)
	controllerAPIExtensionClient := apiextensionsclient.NewForConfigOrDie(controllerRestConfig)
	controllerDynamicClient := dynamic.NewForConfigOrDie(controllerRestConfig)
	// Create the hub applier
	controllerApplierBuilder := clusteradmapply.NewApplierBuilder().
		WithClient(controllerKubernetesClient, controllerAPIExtensionClient, controllerDynamicClient)

	// Create the clusterRegistrar with the reference of the kcpConfig Secret
	ginkgo.By("Create the clusterRegistrar", func() {
		klog.Info("apply clusterRegistrar")
		// Values for the appliers
		values := struct {
			KcpKubeconfigSecret string
		}{
			KcpKubeconfigSecret: computeKubeconfigSecret,
		}
		applier := controllerApplierBuilder.
			Build()
		files := []string{
			"resources/compute/clusterRegistrar.yaml",
		}

		_, err := applier.ApplyCustomResources(readerTest, values, false, "", files...)
		gomega.Expect(err).To(gomega.BeNil())
	})

	// Create the runtime client for the cluster workspace in order to check the registedcluster on kcp
	computeWorkspaceKubconfigData, err := ioutil.ReadFile(adminComputeKubeconfigFile)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	computeRestWorkspaceConfig, err := clientcmd.RESTConfigFromKubeConfig(computeWorkspaceKubconfigData)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	computeRuntimeWorkspaceClient, err = client.New(computeRestWorkspaceConfig, client.Options{Scheme: scheme})
	gomega.Expect(err).ToNot(gomega.HaveOccurred())

	return
}

func TearDownCompute() {
	cleanup()
}

func cleanup() {
	// Kill KCP
	if computeServer != nil {
		ginkgo.By("tearing down the kcp")
		computeServer.Process.Signal(os.Interrupt)
		gomega.Eventually(func() error {
			if err := computeServer.Process.Signal(os.Interrupt); err != nil {
				klog.Error(err, "while tear down the kcp")
				return err
			}
			return nil
		}, 60, 3).Should(gomega.BeNil())
		// computeServer.Wait()
	}
	if testEnv != nil {
		ginkgo.By("tearing down the test environment")
		err := testEnv.Stop()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
}

func GetCRD(reader *clusteradmasset.ScenarioResourcesReader, file string) (*apiextensionsv1.CustomResourceDefinition, error) {
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

func PersistAndGetRestConfig(useExistingCluster bool) (string, *rest.Config, error) {
	var err error
	buf := new(strings.Builder)
	if useExistingCluster {
		cmd := exec.Command("kubectl", "config", "view", "--raw")
		cmd.Stdout = buf
		cmd.Stderr = buf
		err = cmd.Run()
	} else {
		adminInfo := envtest.User{Name: "admin", Groups: []string{"system:masters"}}
		authenticatedUser, err := testEnv.AddUser(adminInfo, testEnv.Config)
		gomega.Expect(err).To(gomega.BeNil())
		kubectl, err := authenticatedUser.Kubectl()
		gomega.Expect(err).To(gomega.BeNil())
		var out io.Reader
		out, _, err = kubectl.Run("config", "view", "--raw")
		gomega.Expect(err).To(gomega.BeNil())
		_, err = io.Copy(buf, out)
		gomega.Expect(err).To(gomega.BeNil())
	}
	if err != nil {
		return "", nil, err
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
