//go:build e2e
// +build e2e

package e2e

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	agcnames "github.com/actions-gateway/github-actions-gateway/agc/names"
	gmcnames "github.com/actions-gateway/github-actions-gateway/gmc/names"
	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

const (
	gmcNamespace   = "gmc-system"
	infraNamespace = "e2e-infra"

	fakegithubServiceName = "fakegithub"
	fakegithubServicePort = "8080"

	agcName      = agcnames.ControllerName
	proxyName    = gmcnames.ProxyName
	workloadName = gmcnames.WorkloadNetworkPolicyName
)

var (
	gmcImage        string
	agcImage        string
	proxyImage      string
	fakegithubImage string
	workerImage     string

	shouldCleanupCertManager bool

	// testRSAKey is a fresh RSA-2048 key generated at suite startup.
	// It is used to populate every GitHub App Secret so the AGC can sign JWTs.
	testRSAKeyPEM []byte
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting gmc e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

// suiteData holds shared state that process 0 passes to all parallel processes.
type suiteData struct {
	GMCImage        string `json:"gmcImage"`
	AGCImage        string `json:"agcImage"`
	ProxyImage      string `json:"proxyImage"`
	FakegithubImage string `json:"fakegithubImage"`
	WorkerImage     string `json:"workerImage"`
	RSAKeyPEM       []byte `json:"rsaKeyPEM"`
}

var _ = SynchronizedBeforeSuite(
	// Runs ONCE on process 0: cluster setup and shared-state marshaling.
	func() []byte {
		// Fallback defaults match the local-registry naming the root Makefile
		// emits; `make e2e-up` overrides via env. Kind nodes pull these names
		// via scripts/kind-with-registry.sh's containerd config.
		gmcImg := envOrDefault("GMC_IMG", "localhost:5000/gmc:e2e")
		agcImg := envOrDefault("AGC_IMG", "localhost:5000/agc:e2e")
		proxyImg := envOrDefault("PROXY_IMG", "localhost:5000/proxy:e2e")
		fakegithubImg := envOrDefault("FAKEGITHUB_IMG", "localhost:5000/fakegithub:e2e")
		workerImg := envOrDefault("WORKER_IMG", "localhost:5000/worker:e2e")

		By("generating test RSA private key")
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		Expect(err).NotTo(HaveOccurred(), "generate test RSA key")
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		Expect(err).NotTo(HaveOccurred())
		rsaKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

		// Images are distributed via the local registry stood up by
		// scripts/kind-with-registry.sh; kind nodes pull on demand. No
		// per-image kind load step is needed here.

		// Populate package-level vars so setup helpers can reference them.
		gmcImage = gmcImg
		agcImage = agcImg
		proxyImage = proxyImg
		fakegithubImage = fakegithubImg
		workerImage = workerImg

		configureKubectlKubeRC()
		setupCertManager()
		setupMetricsServer()
		setupFakegithub()
		setupGMC()

		data, err := json.Marshal(suiteData{
			GMCImage:        gmcImg,
			AGCImage:        agcImg,
			ProxyImage:      proxyImg,
			FakegithubImage: fakegithubImg,
			WorkerImage:     workerImg,
			RSAKeyPEM:       rsaKeyPEM,
		})
		Expect(err).NotTo(HaveOccurred())
		return data
	},
	// Runs on ALL processes after process 0 finishes: populate package-level vars.
	func(data []byte) {
		var sd suiteData
		Expect(json.Unmarshal(data, &sd)).To(Succeed())
		gmcImage = sd.GMCImage
		agcImage = sd.AGCImage
		proxyImage = sd.ProxyImage
		fakegithubImage = sd.FakegithubImage
		workerImage = sd.WorkerImage
		testRSAKeyPEM = sd.RSAKeyPEM
	},
)

var _ = SynchronizedAfterSuite(
	func() { /* per-process teardown — nothing needed */ },
	// Runs ONCE on process 0 after all processes finish.
	func() {
		// E2E_SKIP_TEARDOWN leaves the GMC, fakegithub, and cert-manager in
		// place so the workflow's diagnostic step can dump real cluster state
		// before the kind cluster is deleted. Without this, teardownGMC's
		// `make undeploy` deletes everything before the workflow can inspect
		// it on failure, producing "No resources found" output that hides the
		// real cause of any test failure.
		//
		// The full kind cluster is still torn down by the workflow's
		// `Delete kind cluster` step regardless of this flag.
		if os.Getenv("E2E_SKIP_TEARDOWN") == "true" {
			_, _ = fmt.Fprintln(GinkgoWriter, "E2E_SKIP_TEARDOWN=true; leaving GMC/fakegithub/cert-manager in place for diagnostics")
			return
		}
		teardownGMC()
		teardownFakegithub()
		teardownCertManager()
	},
)

// setupGMC deploys the GMC controller and waits for it to be ready.
func setupGMC() {
	fakegithubBaseURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%s",
		fakegithubServiceName, infraNamespace, fakegithubServicePort)

	By("deploying GMC")
	cmd := exec.Command("make", "deploy",
		fmt.Sprintf("IMG=%s", gmcImage),
		fmt.Sprintf("AGC_IMAGE=%s", agcImage),
		fmt.Sprintf("PROXY_IMAGE=%s", proxyImage),
	)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "deploy GMC")

	By("enabling AGC_EXTRA_* forwarding and injecting fakegithub env vars")
	// --allow-agc-extra-env=true tells GMC to forward AGC_EXTRA_* env vars from its
	// own pod to the AGC Deployments it creates. This is required for e2e tests so
	// that AGC pods can reach fakegithub instead of real GitHub.
	// --allow-floating-image-tags=true lets the suite use locally-built tag refs
	// (e.g. localhost:5000/agc:e2e-...) instead of digest-pinned images, which the
	// GMC otherwise requires for AGC_IMAGE/PROXY_IMAGE.
	cmd = exec.Command("kubectl", "patch", "deployment", "gmc-controller-manager",
		"-n", gmcNamespace,
		"--type=json",
		`-p=[`+
			`{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--allow-agc-extra-env=true"},`+
			`{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--allow-floating-image-tags=true"}`+
			`]`,
	)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "patch GMC args to enable allow-agc-extra-env and allow-floating-image-tags")

	cmd = exec.Command("kubectl", "set", "env",
		"deployment/gmc-controller-manager",
		"-c", "manager",
		"-n", gmcNamespace,
		fmt.Sprintf("AGC_EXTRA_GITHUB_API_BASE_URL=%s", fakegithubBaseURL),
		fmt.Sprintf("AGC_EXTRA_GITHUB_BROKER_URL=%s", fakegithubBaseURL),
		fmt.Sprintf("AGC_EXTRA_STUB_AUTH_URL=%s/token", fakegithubBaseURL),
		fmt.Sprintf("AGC_EXTRA_STUB_BROKER_URL=%s", fakegithubBaseURL),
	)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "inject AGC_EXTRA env vars")

	// cert-manager takes ~30-60s to issue the webhook cert Secret on first install.
	// The GMC pod mounts it as a volume; without it the pod can't start and the
	// rollout stalls. Wait for the Secret before polling rollout status.
	By("waiting for webhook cert Secret to be issued by cert-manager")
	// cert-manager creates a Secret whose name matches Certificate.spec.secretName
	// ("webhook-server-cert"), NOT the Certificate CR name ("gmc-serving-cert").
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "secret", "webhook-server-cert", "-n", gmcNamespace)
		_, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "webhook-server-cert not yet available")
	}, 5*time.Minute, 5*time.Second).Should(Succeed())

	By("waiting for GMC controller to be ready")
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "rollout", "status",
			"deployment/gmc-controller-manager",
			"-n", gmcNamespace,
			"--timeout=30s",
		)
		_, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
	}, 5*time.Minute, 5*time.Second).Should(Succeed())
}

func teardownGMC() {
	By("undeploying GMC")
	cmd := exec.Command("make", "undeploy")
	_, _ = utils.Run(cmd)

	By("uninstalling CRDs")
	cmd = exec.Command("make", "uninstall")
	_, _ = utils.Run(cmd)
}

// setupFakegithub deploys the fakegithub Pod+Service in e2e-infra namespace.
func setupFakegithub() {
	By("creating " + infraNamespace + " namespace")
	cmd := exec.Command("kubectl", "create", "namespace", infraNamespace, "--dry-run=client", "-o", "yaml")
	nsYAML, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
	Expect(utils.ApplyManifest(nsYAML)).To(Succeed(), "apply namespace manifest")

	By("deploying fakegithub")
	manifest := fakegithubManifest(fakegithubImage, infraNamespace)
	Expect(utils.ApplyManifest(manifest)).To(Succeed(), "deploy fakegithub")

	By("waiting for fakegithub to be ready")
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "rollout", "status",
			"deployment/"+fakegithubServiceName,
			"-n", infraNamespace,
			"--timeout=2m",
		)
		_, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
	}, 3*time.Minute, 5*time.Second).Should(Succeed())
}

func teardownFakegithub() {
	By("removing " + infraNamespace + " namespace")
	cmd := exec.Command("kubectl", "delete", "namespace", infraNamespace, "--ignore-not-found")
	_, _ = utils.Run(cmd)
}

// setupMetricsServer installs the Kubernetes metrics-server (required for HPA).
func setupMetricsServer() {
	if os.Getenv("METRICS_SERVER_INSTALL_SKIP") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping metrics-server install (METRICS_SERVER_INSTALL_SKIP=true)\n")
		return
	}
	By("installing metrics-server")
	// Use the official release with --kubelet-insecure-tls for kind.
	const msURL = "https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml"
	cmd := exec.Command("kubectl", "apply", "-f", msURL)
	if _, err := utils.Run(cmd); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "warning: metrics-server install: %v\n", err)
		return
	}
	// Patch for kind (kubelet TLS is self-signed).
	cmd = exec.Command("kubectl", "patch", "deployment", "metrics-server",
		"-n", "kube-system",
		"--type=json",
		`-p=[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]`,
	)
	if _, err := utils.Run(cmd); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "warning: metrics-server patch: %v\n", err)
	}
}

func configureKubectlKubeRC() {
	if os.Getenv("KUBECTL_KUBERC") != "true" {
		By("disabling kubectl kuberc for test isolation")
		_ = os.Setenv("KUBECTL_KUBERC", "false")
	}
}

func setupCertManager() {
	if os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager install (CERT_MANAGER_INSTALL_SKIP=true)\n")
		return
	}
	By("checking if CertManager is already installed")
	if utils.IsCertManagerCRDsInstalled() {
		_, _ = fmt.Fprintf(GinkgoWriter, "CertManager already installed\n")
		return
	}
	shouldCleanupCertManager = true
	By("installing CertManager")
	Expect(utils.InstallCertManager()).To(Succeed(), "install CertManager")
}

func teardownCertManager() {
	if !shouldCleanupCertManager {
		return
	}
	By("uninstalling CertManager")
	utils.UninstallCertManager()
}

// envOrDefault returns the env var value or the given default.
func envOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// fakegithubManifest returns a YAML string with the Deployment and Service for fakegithub.
func fakegithubManifest(image, ns string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: fakegithub
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: fakegithub
  template:
    metadata:
      labels:
        app: fakegithub
    spec:
      containers:
      - name: fakegithub
        image: %s
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 8080
          name: http
        - containerPort: 9090
          name: control
---
apiVersion: v1
kind: Service
metadata:
  name: fakegithub
  namespace: %s
spec:
  selector:
    app: fakegithub
  ports:
  - name: http
    port: 8080
    targetPort: 8080
  - name: control
    port: 9090
    targetPort: 9090
`, ns, image, ns)
}

// stringReader wraps a string as an io.Reader for kubectl stdin.
func stringReader(s string) io.Reader { return strings.NewReader(s) }
