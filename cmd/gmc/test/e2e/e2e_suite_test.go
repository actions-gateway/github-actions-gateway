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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

	// managerDeployment is the manager Deployment rendered by the Helm chart
	// (namePrefix "gmc"); the failure dumps read its logs.
	managerDeployment = "gmc-controller-manager"

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
	wrapperImage    string
	// curlImage is the image used by the curl-based test pods (proxy
	// connectivity, cross-tenant isolation, metrics scrape). It defaults to the
	// upstream Docker Hub ref for local runs; CI overrides E2E_CURL_IMAGE to a
	// local-registry mirror so the kind nodes never pull from Docker Hub (which
	// flakes under anonymous rate limits — HTTP 429).
	curlImage string

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
	WrapperImage    string `json:"wrapperImage"`
	CurlImage       string `json:"curlImage"`
	RSAKeyPEM       []byte `json:"rsaKeyPEM"`
}

var _ = SynchronizedBeforeSuite(
	// Runs ONCE on process 0: cluster setup and shared-state marshaling.
	func() []byte {
		// Fallback defaults match the local-registry naming the root Makefile
		// emits; `make e2e-up` overrides via env. Kind nodes pull these names
		// via scripts/e2e/kind-with-registry.sh's containerd config. The host is the
		// literal IPv4 loopback (not localhost) so the host-side push target is
		// unambiguously IPv4 — matches the containerd certs.d mirror key.
		gmcImg := envOrDefault("GMC_IMG", "127.0.0.1:5000/gmc:e2e")
		agcImg := envOrDefault("AGC_IMG", "127.0.0.1:5000/agc:e2e")
		proxyImg := envOrDefault("PROXY_IMG", "127.0.0.1:5000/proxy:e2e")
		fakegithubImg := envOrDefault("FAKEGITHUB_IMG", "127.0.0.1:5000/fakegithub:e2e")
		workerImg := envOrDefault("WORKER_IMG", "127.0.0.1:5000/worker:e2e")
		wrapperImg := envOrDefault("WRAPPER_IMG", "127.0.0.1:5000/wrapper:e2e")
		curlImg := envOrDefault("E2E_CURL_IMAGE", "curlimages/curl:8.10.1")

		By("generating test RSA private key")
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		Expect(err).NotTo(HaveOccurred(), "generate test RSA key")
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		Expect(err).NotTo(HaveOccurred())
		rsaKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

		// Images are distributed via the local registry stood up by
		// scripts/e2e/kind-with-registry.sh; kind nodes pull on demand.

		// Populate package-level vars so setup helpers can reference them.
		gmcImage = gmcImg
		agcImage = agcImg
		proxyImage = proxyImg
		fakegithubImage = fakegithubImg
		workerImage = workerImg
		wrapperImage = wrapperImg
		curlImage = curlImg

		// Baseline runner-host GitHub reachability for the log (Q352). Non-fatal —
		// see logRunnerHostGitHubBaseline; the failure-time AfterEach in
		// github_egress_preflight_test.go does the authoritative attribution.
		logRunnerHostGitHubBaseline()

		configureKubectlKubeRC()
		setupCertManager()
		setupMetricsServer()
		setupV2CRDs()
		setupFakegithub()
		setupGMC()

		data, err := json.Marshal(suiteData{
			GMCImage:        gmcImg,
			AGCImage:        agcImg,
			ProxyImage:      proxyImg,
			FakegithubImage: fakegithubImg,
			WorkerImage:     workerImg,
			WrapperImage:    wrapperImg,
			CurlImage:       curlImg,
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
		wrapperImage = sd.WrapperImage
		curlImage = sd.CurlImage
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
		// Drain tenant namespaces BEFORE teardownGMC(): their finalizers can only
		// be cleared while the GMC/AGC controllers are still running (see the
		// function doc). teardownGMC() removes those controllers.
		drainTenantNamespaces()
		teardownGMC()
		teardownFakegithub()
		teardownCertManager()
	},
)

// setupGMC deploys the GMC controller via the Helm chart and waits for it to be
// ready. `make deploy` runs `helm upgrade --install` of charts/actions-gateway —
// the SAME chart published to the OCI registry — so the artifact we ship is the
// one e2e exercises (Q142). The chart sets allowFloatingImageTags=true (the
// dev/CI image refs are tag-only, not digest-pinned) and renders the gmc/agc/proxy
// image values from GMC_IMG/AGC_IMG/PROXY_IMG.
func setupGMC() {
	fakegithubBaseURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%s",
		fakegithubServiceName, infraNamespace, fakegithubServicePort)

	// A prior run under E2E_SKIP_TEARDOWN=true leaves this Deployment behind with
	// .spec.template.spec.containers[name="manager"].args owned by the
	// kubectl-patch field manager: the arg patch below claims the whole atomic
	// list. Helm 4 applies server-side, so `make deploy` below fails outright on
	// that field rather than overwriting it, before any spec runs. Deleting the
	// Deployment drops the stale ownership and Helm recreates it chart-owned.
	// fakegithub, cert-manager, and the tenant namespaces are untouched, so a
	// skipped teardown still leaves a failed run's state to inspect. Foreground
	// cascade so the fresh Deployment cannot adopt the old ReplicaSet mid-GC.
	By("deleting any leftover GMC Deployment so Helm reapplies it with chart-owned fields")
	cmd := exec.Command("kubectl", "delete", "deployment", managerDeployment,
		"-n", gmcNamespace, "--ignore-not-found", "--cascade=foreground")
	// Best-effort: a first run has no gmc-system namespace at all, and a delete
	// that genuinely fails resurfaces as the apply conflict on `make deploy`.
	_, _ = utils.Run(cmd)

	By("deploying GMC via the Helm chart")
	cmd = exec.Command("make", "deploy",
		fmt.Sprintf("GMC_IMG=%s", gmcImage),
		fmt.Sprintf("AGC_IMG=%s", agcImage),
		fmt.Sprintf("PROXY_IMG=%s", proxyImage),
		fmt.Sprintf("WRAPPER_IMG=%s", wrapperImage),
	)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "deploy GMC")

	By("enabling AGC_EXTRA_* forwarding and injecting fakegithub env vars")
	// --allow-agc-extra-env=true tells GMC to forward AGC_EXTRA_* env vars from its
	// own pod to the AGC Deployments it creates. This is required for e2e tests so
	// that AGC pods can reach fakegithub instead of real GitHub. It is an e2e-only
	// knob with no chart value, so it is patched in after the Helm install; the
	// digest-pin relaxation is already handled by the chart's
	// allowFloatingImageTags=true (set by `make deploy`).
	cmd = exec.Command("kubectl", "patch", "deployment", "gmc-controller-manager",
		"-n", gmcNamespace,
		"--type=json",
		`-p=[`+
			`{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--allow-agc-extra-env=true"}`+
			`]`,
	)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "patch GMC args to enable allow-agc-extra-env")

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
	// ("webhook-server-cert"), NOT the Certificate CR name ("serving-cert").
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

// setupV2CRDs installs the five v2 (actions-gateway.com) CRDs so the GMC's v2
// reconcilers and the v2 multi-gateway e2e have their kinds present. The main
// chart ships only the v1 CRDs; the v2 CRDs live in the actions-gateway-crds-v2
// chart, which operators install separately.
//
// The CRDs are applied from the chart RENDER (`helm template | kubectl apply
// --server-side`) rather than `helm install` or a raw `kubectl apply -f
// api/config/crd`. Two reasons:
//   - As of Q74 each v2 CRD carries a spec.conversion pointing at the GMC-hosted
//     /convert webhook (v2beta1 is served + storage/hub, v2alpha1 the served spoke),
//     so the deployment-specific conversion stanza must be present or the apiserver
//     silently prunes the ScaleSet-only-stripped RunnerSet fields on storage. Only the
//     chart carries that wiring — api/config/crd stays conversion-free.
//   - The two RunnerTemplate CRDs embed a full PodTemplateSpec (~600 KB each), so the
//     rendered chart (~2.5 MB) exceeds the 1 MiB Helm release-Secret limit — `helm
//     install` cannot store it. Server-side apply sidesteps that and the 256 KB
//     client-side apply ceiling.
//
// Rendered with .Release.Namespace = gmcNamespace so the conversion clientConfig +
// cert-manager annotation resolve to the GMC's namespace. Installed before the GMC so
// its v2 informers sync on first start; the conversion webhook is only exercised once
// v2 objects exist, by which point the GMC is Ready.
func setupV2CRDs() {
	By("installing the v2 (actions-gateway.com) CRDs from the actions-gateway-crds-v2 chart render")
	render := fmt.Sprintf(
		"set -o pipefail; helm template actions-gateway-crds-v2 %q --namespace %s | kubectl apply --server-side --force-conflicts -f -",
		v2CRDChartDir(), gmcNamespace)
	// G204: this one really does invoke a shell, so it is not covered by the
	// constant-binary gosec exclusion in .golangci.yml and is accepted by hand.
	// `render` interpolates only v2CRDChartDir() — an absolute path derived from
	// this file's own compile-time location and %q-quoted — and the gmcNamespace
	// package constant. Nothing from the environment, the cluster, or a fixture
	// reaches it. The shell is needed for `set -o pipefail` + the helm|kubectl pipe.
	cmd := exec.Command("bash", "-c", render) //nolint:gosec // G204: shell script built only from a compile-time path and a package const.
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "install v2 CRDs (helm template | kubectl apply)")
}

// v2CRDChartDir returns the absolute path to the actions-gateway-crds-v2 chart,
// derived from this file's compile-time location: <root>/cmd/gmc/test/e2e/. Resolving
// from the source file (rather than a CWD-relative path) keeps it correct whether the
// e2e binary runs from the package dir (`go test`) or CI's `ginkgo run` invocation.
func v2CRDChartDir() string {
	_, thisFile, _, ok := runtime.Caller(0)
	Expect(ok).To(BeTrue(), "resolve caller for v2 CRD chart path")
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..")
	return filepath.Join(root, "charts", "actions-gateway-crds-v2")
}

// tenantNamespaceMarker is the label every GMC-managed tenant namespace carries
// (see utils.CreateNamespace). drainTenantNamespaces selects on it to find the
// namespaces whose finalizers must be cleared before the controllers go away.
const tenantNamespaceMarker = "actions-gateway.github.com/tenant=true"

// drainTenantNamespaces deletes every GMC-managed tenant namespace and blocks
// until each has finished terminating. It MUST run before teardownGMC().
//
// Tenant namespaces carry RunnerGroup/RunnerSet objects whose
// actions-gateway(.github).com/agentpool-cleanup finalizers are cleared by the
// per-tenant AGC controllers, and an ActionsGateway whose gmc-cleanup finalizer
// is cleared by the GMC. teardownGMC() runs `make undeploy` (helm uninstall),
// which removes those controllers. A tenant namespace still Terminating at that
// point strands its finalizer with nothing left to clear it: the namespace
// never finishes deleting, and the next local back-to-back run's BeforeAll
// `create namespace` collides on the still-Terminating namespace (Q301). CI is
// unaffected — it deletes the whole kind cluster per run, so nothing is reused.
//
// A spec's own AfterAll issues DeleteNamespace with --wait=false, so most tenant
// namespaces are already Terminating by suite end; this both deletes any a
// failed or interrupted spec left behind and waits out the termination while the
// controllers are still up. The wait is best-effort and bounded: on timeout it
// logs and returns rather than failing the suite, so a genuinely stuck namespace
// never hangs teardown or turns a green run red (this is a local-loop hygiene
// step, not a correctness assertion).
func drainTenantNamespaces() {
	By("draining tenant namespaces before undeploy so agentpool-cleanup finalizers clear while the controllers are up")

	listNames := func() ([]string, error) {
		cmd := exec.Command("kubectl", "get", "namespaces",
			"-l", tenantNamespaceMarker,
			"-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`,
		)
		out, err := utils.Run(cmd)
		if err != nil {
			return nil, err
		}
		return utils.GetNonEmptyLines(out), nil
	}

	names, err := listNames()
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "drainTenantNamespaces: listing tenant namespaces failed: %v\n", err)
		return
	}
	if len(names) == 0 {
		return
	}

	// Request deletion of the whole set (idempotent — already-Terminating
	// namespaces are a no-op). --wait=false: the bounded poll below is our wait,
	// so a single stuck namespace can't consume the teardown budget on the
	// delete call itself.
	delArgs := append([]string{"delete", "namespace", "--ignore-not-found", "--wait=false"}, names...)
	if _, err := utils.Run(exec.Command("kubectl", delArgs...)); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "drainTenantNamespaces: delete request failed: %v\n", err)
	}

	deadline := time.Now().Add(3 * time.Minute)
	for {
		remaining, err := listNames()
		if err == nil && len(remaining) == 0 {
			return
		}
		if time.Now().After(deadline) {
			_, _ = fmt.Fprintf(GinkgoWriter,
				"drainTenantNamespaces: timed out waiting for tenant namespaces to terminate; still present: %v\n", remaining)
			return
		}
		time.Sleep(5 * time.Second)
	}
}

func teardownGMC() {
	By("undeploying GMC (helm uninstall)")
	// `make undeploy` runs `helm uninstall`. The CRDs carry
	// helm.sh/resource-policy: keep, so they survive the uninstall — that is
	// fine here: the whole kind cluster is deleted by the workflow's final step.
	cmd := exec.Command("make", "undeploy")
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
	//
	// Pinned (not /latest/) for reproducibility and so the CI e2e workflow can
	// pre-pull + kind-load the exact metrics-server image onto the nodes,
	// keeping kubelet off registry.k8s.io at test time (Q150). Bump deliberately
	// and keep in sync with METRICSSERVER_VERSION in
	// .github/workflows/e2e-reusable.yml.
	const metricsServerVersion = "v0.8.1"
	const msURL = "https://github.com/kubernetes-sigs/metrics-server/releases/download/" +
		metricsServerVersion + "/components.yaml"
	cmd := exec.Command("kubectl", "apply", "-f", msURL)
	if _, err := utils.Run(cmd); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "warning: metrics-server install: %v\n", err)
		return
	}
	// Patch for kind (kubelet TLS is self-signed).
	//
	// Do NOT add --metric-resolution here to make the HPA specs settle sooner.
	// Tried and reverted: 10s is metrics-server's minimum ACCEPTED value, but it
	// also has to exceed kubelet's housekeeping interval (10s by default), or
	// metrics-server keeps re-reading unchanged cAdvisor samples, discards them
	// as duplicate timestamps, and never serves usage at all. The result is not
	// a slower HPA but a dead one: on PR #874 both specs that gate on
	// ScalingActive=True (E2E_GMC_HPADrivesScaleUp and
	// E2E_AGC_SkippedJobIsRedeliveredAfterCapacityFrees) timed out at 300s and
	// 240s with "metrics-server not serving metrics".
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
        env:
        # Model the real GitHub broker's long-poll so an idle GET /message holds
        # the connection instead of spinning at network speed. Without it a
        # replacement listener hits its 50-empty-poll idle-shutdown within
        # milliseconds and the pool collapses to one listener mid-job, stranding
        # the next job (Q148). 30s keeps 50 empty polls (~25min) well clear of any
        # spec runtime while sitting safely under the AGC's 55s poll header timeout.
        - name: MESSAGE_LONGPOLL_HOLD
          value: "30s"
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
