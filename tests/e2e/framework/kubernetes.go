// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package framework

import (
	"flag"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/errors"

	"path"

	"istio.io/istio/pilot/pkg/config/clusterregistry"
	"istio.io/istio/pilot/pkg/serviceregistry/kube"
	"istio.io/istio/pkg/log"
	"istio.io/istio/tests/util"

	"k8s.io/client-go/kubernetes"
)

const (
	yamlSuffix                  = ".yaml"
	istioInstallDir             = "install/kubernetes"
	istioAddonsDir              = "install/kubernetes/addons"
	nonAuthInstallFile          = "istio.yaml"
	authInstallFile             = "istio-auth.yaml"
	nonAuthInstallFileNamespace = "istio-one-namespace.yaml"
	authInstallFileNamespace    = "istio-one-namespace-auth.yaml"
	istioSystem                 = "istio-system"
	istioIngressServiceName     = "istio-ingress"
	defaultSidecarInjectorFile  = "istio-sidecar-injector.yaml"
	mixerValidatorFile          = "istio-mixer-validator.yaml"
	ingressCertsName            = "istio-ingress-certs"

	maxDeploymentRolloutTime    = 240 * time.Second
	mtlsExcludedServicesPattern = "mtlsExcludedServices:\\s*\\[(.*)\\]"
)

var (
	namespace    = flag.String("namespace", "", "Namespace to use for testing (empty to create/delete temporary one)")
	mixerHub     = flag.String("mixer_hub", os.Getenv("HUB"), "Mixer hub")
	mixerTag     = flag.String("mixer_tag", os.Getenv("TAG"), "Mixer tag")
	pilotHub     = flag.String("pilot_hub", os.Getenv("HUB"), "Pilot hub")
	pilotTag     = flag.String("pilot_tag", os.Getenv("TAG"), "Pilot tag")
	proxyHub     = flag.String("proxy_hub", os.Getenv("HUB"), "Proxy hub")
	proxyTag     = flag.String("proxy_tag", os.Getenv("TAG"), "Proxy tag")
	caHub        = flag.String("ca_hub", os.Getenv("HUB"), "Ca hub")
	caTag        = flag.String("ca_tag", os.Getenv("TAG"), "Ca tag")
	authEnable   = flag.Bool("auth_enable", false, "Enable auth")
	rbacEnable   = flag.Bool("rbac_enable", true, "Enable rbac")
	localCluster = flag.Bool("use_local_cluster", false,
		"Whether the cluster is local or not (i.e. the test is running within the cluster). If running on minikube, this should be set to true.")
	skipSetup           = flag.Bool("skip_setup", false, "Skip namespace creation and istio cluster setup")
	sidecarInjectorFile = flag.String("sidecar_injector_file", defaultSidecarInjectorFile, "Sidecar injector yaml file")
	clusterWide         = flag.Bool("cluster_wide", false, "Run cluster wide tests")
	withMixerValidator  = flag.Bool("with_mixer_validator", false, "Set up mixer validator")
	imagePullPolicy     = flag.String("image_pull_policy", "", "Specifies an override for the Docker image pull policy to be used")
	multiClusterDir     = flag.String("cluster_registry_dir", "", "Directory name for the cluster registry config")

	addons = []string{
		"zipkin",
	}
)

// KubeInfo gathers information for kubectl
type KubeInfo struct {
	Namespace string

	TmpDir  string
	yamlDir string

	inglock    sync.Mutex
	ingress    string
	ingressErr error

	localCluster     bool
	namespaceCreated bool
	AuthEnabled      bool
	RBACEnabled      bool

	// Extra services to be excluded from MTLS
	MTLSExcludedServices []string

	// Istioctl installation
	Istioctl *Istioctl
	// App Manager
	AppManager *AppManager

	// Release directory
	ReleaseDir string
	// Use baseversion if not empty.
	BaseVersion string

	// A map of app label values to the pods for that app
	appPods      map[string][]string
	appPodsMutex sync.Mutex

	KubeConfig       string
	KubeClient       kubernetes.Interface
	RemoteKubeConfig string
	RemoteKubeClient kubernetes.Interface
}

// newKubeInfo create a new KubeInfo by given temp dir and runID
// If baseVersion is not empty, will use the specified release of Istio instead of the local one.
func newKubeInfo(tmpDir, runID, baseVersion string) (*KubeInfo, error) {
	if *namespace == "" {
		if *clusterWide {
			*namespace = istioSystem
		} else {
			*namespace = runID
		}
	}
	yamlDir := filepath.Join(tmpDir, "yaml")
	i, err := NewIstioctl(yamlDir, *namespace, *namespace, *proxyHub, *proxyTag)
	if err != nil {
		return nil, err
	}

	// Download the base release if baseVersion is specified.
	var releaseDir string
	if baseVersion != "" {
		releaseDir, err = util.DownloadRelease(baseVersion, tmpDir)
		if err != nil {
			return nil, err
		}
		// Use istioctl from base version to inject the sidecar.
		i.localPath = filepath.Join(releaseDir, "/bin/istioctl")
		if err = os.Chmod(i.localPath, 0755); err != nil {
			return nil, err
		}
		i.defaultProxy = true
	} else {
		releaseDir = util.GetResourcePath("")
	}
	var kubeConfig, remoteKubeConfig string
	var kubeClient, remoteKubeClient kubernetes.Interface
	if *multiClusterDir != "" {
		// ClusterRegistiresDir indicates the Kubernetes cluster config should come from files versus KUBECONFIG
		// environmental variable.  The test config can be defined to use either a single cluster or 2 clusters
		var clusterStore *clusterregistry.ClusterStore
		clusterStore, err = clusterregistry.ReadClusters(*multiClusterDir)
		if clusterStore == nil {
			log.Errorf("Failed to clusters in the ClusterRegistriesDir %s\n", *multiClusterDir)
			return nil, err
		}
		if clusterStore != nil {
			kubeConfig = clusterStore.GetPilotAccessConfig()
			kubeConfig = path.Join(*multiClusterDir, kubeConfig)
			//				kubeConfig = kubeCfgFile
			if _, kubeClient, err = kube.CreateInterface(kubeConfig); err != nil {
				return nil, err
			}
			// Note only a single remote cluster is currently supported.
			clusters := clusterStore.GetPilotClusters()
			for _, cluster := range clusters {
				kubeconfig := clusterregistry.GetClusterAccessConfig(cluster)
				remoteKubeConfig = path.Join(*multiClusterDir, kubeconfig)
				log.Infof("Cluster name: %s, AccessConfigFile: %s", clusterregistry.GetClusterName(cluster), remoteKubeConfig)
				// Expecting only a single remote cluster so hard code this.  The code won't throw an error
				// if more than 2 clusters are defined in the config files, but will only use the last cluster parsed.
				if _, remoteKubeClient, err = kube.CreateInterface(remoteKubeConfig); err != nil {
					return nil, err
				}
			}
		}
	} else {
		tmpfile := *namespace + "_kubeconfig"
		tmpfile = path.Join(tmpDir, tmpfile)
		if err = util.GetKubeConfig(tmpfile); err != nil {
			return nil, err
		}
		kubeConfig = tmpfile
	}

	a := NewAppManager(tmpDir, *namespace, i, kubeConfig)

	log.Infof("Using release dir: %s", releaseDir)
	return &KubeInfo{
		Namespace:        *namespace,
		namespaceCreated: false,
		TmpDir:           tmpDir,
		yamlDir:          yamlDir,
		localCluster:     *localCluster,
		Istioctl:         i,
		AppManager:       a,
		AuthEnabled:      *authEnable,
		RBACEnabled:      *rbacEnable,
		ReleaseDir:       releaseDir,
		BaseVersion:      baseVersion,
		KubeConfig:       kubeConfig,
		KubeClient:       kubeClient,
		RemoteKubeConfig: remoteKubeConfig,
		RemoteKubeClient: remoteKubeClient,
	}, nil
}

// IstioSystemNamespace returns the namespace used for the Istio system components.
func (k *KubeInfo) IstioSystemNamespace() string {
	if *clusterWide {
		return istioSystem
	}
	return k.Namespace
}

// IstioIngressService returns the service name for the ingress service
func (k *KubeInfo) IstioIngressService() string {
	return istioIngressServiceName
}

// Setup set up Kubernetes prerequest for tests
func (k *KubeInfo) Setup() error {
	log.Infoa("Setting up kubeInfo setupSkip=", *skipSetup)
	var err error
	if err = os.Mkdir(k.yamlDir, os.ModeDir|os.ModePerm); err != nil {
		return err
	}

	if !*skipSetup {
		if err = k.deployIstio(); err != nil {
			log.Error("Failed to deploy Istio.")
			return err
		}

		if err = k.deployAddons(); err != nil {
			log.Error("Failed to deploy istio addons")
			return err
		}
		// Create the ingress secret.
		certDir := util.GetResourcePath("./tests/testdata/certs")
		certFile := filepath.Join(certDir, "cert.crt")
		keyFile := filepath.Join(certDir, "cert.key")
		if _, err = util.CreateTLSSecret(ingressCertsName, k.IstioSystemNamespace(), keyFile, certFile, k.KubeConfig); err != nil {
			log.Warn("Secret already exists")
		}
		if *withMixerValidator {
			// Run the script to set up the certificate.
			certGenerator := util.GetResourcePath("./install/kubernetes/webhook-create-signed-cert.sh")
			if _, err = util.Shell("%s --service istio-mixer-validator --secret istio-mixer-validator --namespace %s", certGenerator, k.Namespace); err != nil {
				return err
			}
		}
	}

	return nil
}

// PilotHub exposes the Docker hub used for the pilot image.
func (k *KubeInfo) PilotHub() string {
	return *pilotHub
}

// PilotTag exposes the Docker tag used for the pilot image.
func (k *KubeInfo) PilotTag() string {
	return *pilotTag
}

// ProxyHub exposes the Docker hub used for the proxy image.
func (k *KubeInfo) ProxyHub() string {
	return *proxyHub
}

// ProxyTag exposes the Docker tag used for the proxy image.
func (k *KubeInfo) ProxyTag() string {
	return *proxyTag
}

// ImagePullPolicy exposes the pull policy override used for Docker images. May be "".
func (k *KubeInfo) ImagePullPolicy() string {
	return *imagePullPolicy
}

// IngressOrFail lazily initialize ingress and fail test if not found.
func (k *KubeInfo) IngressOrFail(t *testing.T) string {
	gw, err := k.Ingress()
	if err != nil {
		t.Fatalf("Unable to get ingress: %v", err)
	}
	return gw
}

// Ingress lazily initialize ingress
func (k *KubeInfo) Ingress() (string, error) {
	k.inglock.Lock()
	defer k.inglock.Unlock()

	// Previously fetched ingress or failed.
	if k.ingressErr != nil || len(k.ingress) != 0 {
		return k.ingress, k.ingressErr
	}

	if k.localCluster {
		k.ingress, k.ingressErr = util.GetIngressPod(k.Namespace, k.KubeConfig)
	} else {
		k.ingress, k.ingressErr = util.GetIngress(k.Namespace, k.KubeConfig)
	}

	// So far we only do http ingress
	if len(k.ingress) > 0 {
		k.ingress = "http://" + k.ingress
	}

	return k.ingress, k.ingressErr
}

// Teardown clean up everything created by setup
func (k *KubeInfo) Teardown() error {
	log.Info("Cleaning up kubeInfo")

	if *skipSetup || *skipCleanup {
		return nil
	}

	if *useAutomaticInjection {
		testSidecarInjectorYAML := filepath.Join(k.TmpDir, "yaml", *sidecarInjectorFile)

		if err := util.KubeDelete(k.Namespace, testSidecarInjectorYAML, k.KubeConfig); err != nil {
			log.Errorf("Istio sidecar injector %s deletion failed", testSidecarInjectorYAML)
			return err
		}
	}

	if *clusterWide {
		// for cluster-wide, we can verify the uninstall
		istioYaml := nonAuthInstallFile
		if *authEnable {
			istioYaml = authInstallFile
		}

		testIstioYaml := filepath.Join(k.TmpDir, "yaml", istioYaml)

		if err := util.KubeDelete(k.Namespace, testIstioYaml, k.KubeConfig); err != nil {
			log.Infof("Safe to ignore resource not found errors in kubectl delete -f %s", testIstioYaml)
		}
	} else {
		if err := util.DeleteNamespace(k.Namespace, k.KubeConfig); err != nil {
			log.Errorf("Failed to delete namespace %s", k.Namespace)
			return err
		}
		if *multiClusterDir != "" {
			if err := util.DeleteNamespace(k.Namespace, k.RemoteKubeConfig); err != nil {
				log.Errorf("Failed to delete namespace %s on remote cluster", k.Namespace)
				return err
			}
		}

		// ClusterRoleBindings are not namespaced and need to be deleted separately
		if _, err := util.Shell("kubectl get --kubeconfig=%s clusterrolebinding -o jsonpath={.items[*].metadata.name}"+
			"|xargs -n 1|fgrep %s|xargs kubectl delete --kubeconfig=%s clusterrolebinding", k.KubeConfig,
			k.Namespace, k.KubeConfig); err != nil {
			log.Errorf("Failed to delete clusterrolebindings associated with namespace %s", k.Namespace)
			return err
		}

		// ClusterRoles are not namespaced and need to be deleted separately
		if _, err := util.Shell("kubectl get --kubeconfig=%s clusterrole -o jsonpath={.items[*].metadata.name}"+
			"|xargs -n 1|fgrep %s|xargs kubectl delete --kubeconfig=%s clusterrole", k.KubeConfig,
			k.Namespace, k.KubeConfig); err != nil {
			log.Errorf("Failed to delete clusterroles associated with namespace %s", k.Namespace)
			return err
		}
	}

	// confirm the namespace is deleted as it will cause future creation to fail
	maxAttempts := 120
	namespaceDeleted := false
	log.Infof("Deleting namespace %v", k.Namespace)
	for attempts := 1; attempts <= maxAttempts; attempts++ {
		namespaceDeleted, _ = util.NamespaceDeleted(k.Namespace, k.KubeConfig)
		if namespaceDeleted {
			break
		}
		time.Sleep(1 * time.Second)
	}

	if !namespaceDeleted {
		log.Errorf("Failed to delete namespace %s after %v seconds", k.Namespace, maxAttempts)
		return nil
	}

	log.Infof("Namespace %s deletion status: %v", k.Namespace, namespaceDeleted)

	return nil
}

// GetAppPods gets a map of app name to pods for that app. If pods are found, the results are cached.
func (k *KubeInfo) GetAppPods() map[string][]string {
	// Get a copy of the internal map.
	newMap := k.getAppPods()

	if len(newMap) == 0 {
		var err error
		if newMap, err = util.GetAppPods(k.Namespace, k.KubeConfig); err != nil {
			log.Errorf("Failed to get retrieve the app pods for namespace %s", k.Namespace)
		} else {
			// Copy the new results to the internal map.
			log.Infof("Fetched pods with the `app` label: %v", newMap)
			k.setAppPods(newMap)
		}
	}
	return newMap
}

// GetRoutes gets routes from the pod or returns error
func (k *KubeInfo) GetRoutes(app string) (string, error) {
	appPods := k.GetAppPods()
	if len(appPods[app]) == 0 {
		return "", errors.Errorf("missing pod names for app %q", app)
	}

	pod := appPods[app][0]

	routesURL := "http://localhost:15000/routes"
	routes, err := util.PodExec(k.Namespace, pod, "app", fmt.Sprintf("client -url %s", routesURL), true, k.KubeConfig)
	if err != nil {
		return "", errors.WithMessage(err, "failed to get routes")
	}

	return routes, nil
}

// getAppPods returns a copy of the appPods map. Should only be called by GetAppPods.
func (k *KubeInfo) getAppPods() map[string][]string {
	k.appPodsMutex.Lock()
	defer k.appPodsMutex.Unlock()

	return k.deepCopy(k.appPods)
}

// setAppPods sets the app pods with a copy of the given map. Should only be called by GetAppPods.
func (k *KubeInfo) setAppPods(newMap map[string][]string) {
	k.appPodsMutex.Lock()
	defer k.appPodsMutex.Unlock()

	k.appPods = k.deepCopy(newMap)
}

func (k *KubeInfo) deepCopy(src map[string][]string) map[string][]string {
	newMap := make(map[string][]string, len(src))
	for k, v := range src {
		newMap[k] = v
	}
	return newMap
}

func (k *KubeInfo) deployAddons() error {
	for _, addon := range addons {
		addonPath := filepath.Join(istioAddonsDir, fmt.Sprintf("%s.yaml", addon))
		baseYamlFile := filepath.Join(k.ReleaseDir, addonPath)
		content, err := ioutil.ReadFile(baseYamlFile)
		if err != nil {
			log.Errorf("Cannot read file %s", baseYamlFile)
			return err
		}

		if !*clusterWide {
			content = replacePattern(content, istioSystem, k.Namespace)
		}

		yamlFile := filepath.Join(k.TmpDir, "yaml", addon+".yaml")
		err = ioutil.WriteFile(yamlFile, content, 0600)
		if err != nil {
			log.Errorf("Cannot write into file %s", yamlFile)
		}

		if err := util.KubeApply(k.Namespace, yamlFile, k.KubeConfig); err != nil {
			log.Errorf("Kubectl apply %s failed", yamlFile)
			return err
		}
	}
	return nil
}

func (k *KubeInfo) deployIstio() error {
	istioYaml := nonAuthInstallFileNamespace
	if *clusterWide {
		if *authEnable {
			istioYaml = authInstallFile
		} else {
			istioYaml = nonAuthInstallFile
		}
	} else {
		if *authEnable {
			istioYaml = authInstallFileNamespace
		}
	}
	yamlDir := filepath.Join(istioInstallDir, istioYaml)
	baseIstioYaml := filepath.Join(k.ReleaseDir, yamlDir)
	testIstioYaml := filepath.Join(k.TmpDir, "yaml", istioYaml)

	if err := k.generateIstio(baseIstioYaml, testIstioYaml); err != nil {
		log.Errorf("Generating yaml %s failed", testIstioYaml)
		return err
	}

	if err := util.CreateNamespace(k.Namespace, k.KubeConfig); err != nil {
		log.Errorf("Unable to create namespace %s: %s", k.Namespace, err.Error())
		return err
	}

	if *multiClusterDir != "" {
		if err := util.CreateNamespace(k.Namespace, k.RemoteKubeConfig); err != nil {
			log.Errorf("Unable to create namespace %s on remote cluster: %s", k.Namespace, err.Error())
			return err
		}
	}

	if err := util.KubeApply(k.Namespace, testIstioYaml, k.KubeConfig); err != nil {
		log.Errorf("Istio core %s deployment failed", testIstioYaml)
		return err
	}

	if *withMixerValidator {
		baseMixerValidatorYaml := filepath.Join(k.ReleaseDir, istioInstallDir, mixerValidatorFile)
		_, err := os.Stat(baseMixerValidatorYaml)
		if err != nil && os.IsNotExist(err) {
			// Some old version may not have this file.
			log.Warnf("%s does not exist in install dir %s", mixerValidatorFile, istioInstallDir)
		} else {
			testMixerValidatorYaml := filepath.Join(k.TmpDir, "yaml", mixerValidatorFile)
			if err := k.generateIstio(baseMixerValidatorYaml, testMixerValidatorYaml); err != nil {
				log.Errorf("Generating yaml %s failed", testMixerValidatorYaml)
				return err
			}
			if err := util.KubeApply(k.Namespace, testMixerValidatorYaml, k.KubeConfig); err != nil {
				log.Errorf("Istio mixer validator %s deployment failed", testMixerValidatorYaml)
				return err
			}
		}
	}

	if *useAutomaticInjection {
		baseSidecarInjectorYAML := util.GetResourcePath(filepath.Join(istioInstallDir, *sidecarInjectorFile))
		testSidecarInjectorYAML := filepath.Join(k.TmpDir, "yaml", *sidecarInjectorFile)
		if err := k.generateSidecarInjector(baseSidecarInjectorYAML, testSidecarInjectorYAML); err != nil {
			log.Errorf("Generating sidecar injector yaml failed")
			return err
		}
		if err := util.KubeApply(k.Namespace, testSidecarInjectorYAML, k.KubeConfig); err != nil {
			log.Errorf("Istio sidecar injector %s deployment failed", testSidecarInjectorYAML)
			return err
		}
	}
	return util.CheckDeployments(k.Namespace, maxDeploymentRolloutTime, k.KubeConfig)
}

func updateInjectImage(name, module, hub, tag string, content []byte) []byte {
	image := []byte(fmt.Sprintf("%s: %s/%s:%s", name, hub, module, tag))
	r := regexp.MustCompile(fmt.Sprintf("%s: .*(\\/%s):.*", name, module))
	return r.ReplaceAllLiteral(content, image)
}

func updateInjectVersion(version string, content []byte) []byte {
	versionLine := []byte(fmt.Sprintf("version: %s", version))
	r := regexp.MustCompile("version: .*")
	return r.ReplaceAllLiteral(content, versionLine)
}

func (k *KubeInfo) generateSidecarInjector(src, dst string) error {
	content, err := ioutil.ReadFile(src)
	if err != nil {
		log.Errorf("Cannot read original yaml file %s", src)
		return err
	}

	if !*clusterWide {
		content = replacePattern(content, istioSystem, k.Namespace)
	}

	if *pilotHub != "" && *pilotTag != "" {
		content = updateImage("sidecar_injector", *pilotHub, *pilotTag, content)
		content = updateInjectVersion(*pilotTag, content)
		content = updateInjectImage("initImage", "proxy_init", *proxyHub, *proxyTag, content)
		content = updateInjectImage("proxyImage", "proxy", *proxyHub, *proxyTag, content)
	}

	err = ioutil.WriteFile(dst, content, 0600)
	if err != nil {
		log.Errorf("Cannot write into generate sidecar injector file %s", dst)
	}
	return err
}

func replacePattern(content []byte, src, dest string) []byte {
	r := []byte(dest)
	p := regexp.MustCompile(src)
	content = p.ReplaceAllLiteral(content, r)
	return content
}

func (k *KubeInfo) appendMtlsExcludedServices(content []byte) ([]byte, error) {
	if !k.AuthEnabled || len(k.MTLSExcludedServices) == 0 {
		// Nothing to do.
		return content, nil
	}

	re := regexp.MustCompile(mtlsExcludedServicesPattern)
	match := re.FindStringSubmatch(string(content))
	if len(match) == 0 {
		return nil, fmt.Errorf("failed to locate the mtlsExcludedServices section of the mesh config")
	}

	values := strings.Split(match[1], ",")
	for _, v := range k.MTLSExcludedServices {
		// Add surrounding quotes to the values.
		values = append(values, fmt.Sprintf("\"%s\"", v))
	}
	newValue := fmt.Sprintf("mtlsExcludedServices: [%s]", strings.Join(values, ","))
	return re.ReplaceAll(content, []byte(newValue)), nil
}

func (k *KubeInfo) generateIstio(src, dst string) error {
	content, err := ioutil.ReadFile(src)
	if err != nil {
		log.Errorf("Cannot read original yaml file %s", src)
		return err
	}

	if !*clusterWide {
		content = replacePattern(content, istioSystem, k.Namespace)
		// Customize mixer's configStoreURL to limit watching resources in the testing namespace.
		vs := url.Values{}
		vs.Add("ns", *namespace)
		content = replacePattern(content, "--configStoreURL=k8s://", "--configStoreURL=k8s://?"+vs.Encode())
	}

	// If mtlsExcludedServices is specified, replace it with the updated value
	content, err = k.appendMtlsExcludedServices(content)
	if err != nil {
		log.Errorf("Failed to replace mtlsExcludedServices: %v", err)
		return err
	}

	// Replace long refresh delays with short ones for the sake of tests.
	content = replacePattern(content, "connectTimeout: 10s", "connectTimeout: 1s")
	content = replacePattern(content, "drainDuration: 45s", "drainDuration: 2s")
	content = replacePattern(content, "parentShutdownDuration: 1m0s", "parentShutdownDuration: 3s")

	// A very flimsy and unreliable regexp to replace delays in ingress pod Spec
	content = replacePattern(content, "'30s' #discoveryRefreshDelay", "'1s' #discoveryRefreshDelay")
	content = replacePattern(content, "'10s' #connectTimeout", "'1s' #connectTimeout")
	content = replacePattern(content, "'45s' #drainDuration", "'2s' #drainDuration")
	content = replacePattern(content, "'1m0s' #parentShutdownDuration", "'3s' #parentShutdownDuration")

	if k.BaseVersion == "" {
		if *mixerHub != "" && *mixerTag != "" {
			content = updateImage("mixer", *mixerHub, *mixerTag, content)
		}
		if *pilotHub != "" && *pilotTag != "" {
			content = updateImage("pilot", *pilotHub, *pilotTag, content)
		}
		if *proxyHub != "" && *proxyTag != "" {
			//Need to be updated when the string "proxy" is changed as the default image name
			content = updateImage("proxy", *proxyHub, *proxyTag, content)
		}
		if *caHub != "" && *caTag != "" {
			//Need to be updated when the string "istio-ca" is changed
			content = updateImage("istio-ca", *caHub, *caTag, content)
		}
		if *imagePullPolicy != "" {
			content = updateImagePullPolicy(*imagePullPolicy, content)
		}
	}

	if *localCluster {
		content = []byte(strings.Replace(string(content), "LoadBalancer", "NodePort", 1))
	}

	err = ioutil.WriteFile(dst, content, 0600)
	if err != nil {
		log.Errorf("Cannot write into generated yaml file %s", dst)
	}
	return err
}

func updateImage(module, hub, tag string, content []byte) []byte {
	image := []byte(fmt.Sprintf("image: %s/%s:%s", hub, module, tag))
	r := regexp.MustCompile(fmt.Sprintf("image: .*(\\/%s):.*", module))
	return r.ReplaceAllLiteral(content, image)
}

func updateImagePullPolicy(policy string, content []byte) []byte {
	image := []byte(fmt.Sprintf("imagePullPolicy: %s", policy))
	r := regexp.MustCompile("imagePullPolicy:.*")
	return r.ReplaceAllLiteral(content, image)
}
