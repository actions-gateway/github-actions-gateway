//go:build e2e
// +build e2e

package e2e

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
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

	"github.com/karlkfi/github-actions-gateway/gmc/test/utils"
)

const (
	gmcNamespace   = "gmc-system"
	infraNamespace = "e2e-infra"

	fakegithubServiceName = "fakegithub"
	fakegithubServicePort = "8080"
)

var (
	gmcImage        string
	agcImage        string
	proxyImage      string
	fakegithubImage string

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

var _ = BeforeSuite(func() {
	gmcImage = envOrDefault("GMC_IMG", "gmc:e2e")
	agcImage = envOrDefault("AGC_IMG", "agc:e2e")
	proxyImage = envOrDefault("PROXY_IMG", "proxy:e2e")
	fakegithubImage = envOrDefault("FAKEGITHUB_IMG", "fakegithub:e2e")

	By("generating test RSA private key")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	Expect(err).NotTo(HaveOccurred(), "generate test RSA key")
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	Expect(err).NotTo(HaveOccurred())
	testRSAKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	By("loading images into kind cluster")
	for _, img := range []string{gmcImage, agcImage, proxyImage, fakegithubImage} {
		Expect(utils.LoadImageToKindClusterWithName(img)).To(Succeed(),
			"load image %s", img)
	}

	configureKubectlKubeRC()
	setupCertManager()
	setupMetricsServer()
	setupFakegithub()
	setupGMC()
})

var _ = AfterSuite(func() {
	teardownGMC()
	teardownFakegithub()
	teardownCertManager()
})

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

	By("injecting AGC_EXTRA env vars so AGC pods point to fakegithub")
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
        imagePullPolicy: Never
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
