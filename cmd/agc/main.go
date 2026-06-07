// Command agc is the Actions Gateway Controller (AGC).
// It reconciles RunnerGroup CRDs into adaptive listener goroutine pools that
// long-poll the GitHub Actions broker for incoming workflow jobs.
//
// GitHub App credentials are read from files under /etc/actions-gateway/github-app/
// (projected from a Kubernetes Secret by the GMC). Keys:
//
//	appId          - GitHub App numeric ID
//	installationId - Installation ID for the target org/repo
//	privateKey     - GitHub App private key in PEM format
//
// Flags:
//
//	--agent-key-type  rsa (default) | ed25519 (opt-in; loses session-key encryption)
//	--zap-devel       Opt into development-mode logging (console encoder, debug
//	                  level). The default is production logging: structured JSON
//	                  at info level. See zap.Options.BindFlags for the full set
//	                  (--zap-encoder, --zap-log-level, etc.).
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/token"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/transport"
	"github.com/actions-gateway/github-actions-gateway/githubapp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

func main() {
	if err := run(); err != nil {
		// run() configures the logger immediately after flag parsing; ctrl.Log
		// buffers and replays anything logged before SetLogger is called, so a
		// failure at any point in run() is still surfaced here.
		ctrl.Log.WithName("agc").Error(err, "startup failed")
		os.Exit(1)
	}
}

const (
	credsDir       = "/etc/actions-gateway/github-app"
	proxyCACertDir = "/etc/actions-gateway/proxy-ca"

	// metricsBindAddress pins the controller-runtime metrics server to a known
	// port instead of relying on the framework default (":8080" in
	// controller-runtime v0.24). The GMC's per-tenant AGC NetworkPolicy admits
	// Prometheus scrapes only on this port (metricsPort in
	// cmd/gmc/internal/controller/builder.go), so the listener and the policy
	// must agree by construction — an implicit default could drift out from
	// under the policy on a dependency bump and silently break (or, worse,
	// re-expose) metrics. Served over mTLS (see metricsCertDir) so only a
	// scraper holding a CA-signed client cert can read it (Q69).
	metricsBindAddress = ":8443"

	// metricsCertDir is where the GMC mounts the metrics mTLS server bundle
	// (ca.crt + tls.crt + tls.key). When present, the metrics endpoint is served
	// over HTTPS requiring a client cert signed by ca.crt. When absent (local
	// dev/test where the GMC has not mounted it), metrics fall back to plain
	// HTTP — mirroring the proxy's TLS-when-mounted pattern. The GMC always
	// mounts it in production, so the effective default there is mTLS.
	metricsCertDir = "/etc/actions-gateway/metrics-tls"

	// healthProbeBindAddress pins the controller-runtime health/ready endpoint
	// (/healthz, /readyz) to a known port. The GMC's buildAGCDeployment stamps
	// kubelet liveness/readiness/startup probes on this same port
	// (healthMetricsPort in cmd/gmc/internal/controller/builder.go), so the
	// listener and the probes must agree by construction. Unlike the metrics
	// port, this listener is plaintext and certless so the kubelet can reach it
	// without a client cert, and carries no sensitive data (healthz.Ping only).
	// The AGC NetworkPolicy needs no ingress rule for it — kubelet probes
	// originate from the node, which CNIs admit to local pods regardless of
	// policy (the same reason the GMC and proxy health ports carry no NP rule).
	healthProbeBindAddress = ":8081"
)

func run() error {
	// ── 0. Parse flags ───────────────────────────────────────────────────────
	agentKeyTypeFlag := flag.String("agent-key-type", "rsa",
		"Key type for new agent registrations: rsa (default) or ed25519 (opt-in; loses session-key encryption)")
	// Bind zap's logging flags (--zap-devel, --zap-encoder, --zap-log-level, …)
	// and default to production logging: structured JSON at info level, which log
	// aggregators can parse. The GMC stamps no logging args onto the AGC
	// Deployment, so this default is what actually ships in production — correct
	// by default rather than relying on an operator remembering to flip a flag
	// (the original hard-coded UseDevMode(true) emitted console logs in prod).
	// Developers pass --zap-devel for human-readable console logs at debug level
	// when running locally. Kept consistent with the GMC (cmd/gmc/cmd/main.go).
	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	agentKeyType := agentpool.KeyType(*agentKeyTypeFlag)
	switch agentKeyType {
	case agentpool.KeyTypeEd25519, agentpool.KeyTypeRSA:
	default:
		return fmt.Errorf("invalid --agent-key-type %q: must be ed25519 or rsa", agentKeyType)
	}

	// ── 0.5. Configure proxy TLS cert pinning ───────────────────────────────
	// The GMC mounts the proxy's self-signed TLS cert (public part only) at
	// proxyCACertDir/tls.crt. Adding it to the trust pool — alongside the
	// system roots — lets the AGC validate the proxy's self-signed cert without
	// losing the ability to validate upstream endpoints (api.github.com,
	// pipelinesghubeus*.actions.githubusercontent.com) over the proxy's CONNECT
	// tunnel. Go's http.Transport uses one TLSClientConfig for both the
	// AGC↔proxy hop and the AGC↔upstream-over-tunnel hop, so the pool must
	// satisfy both.
	//
	// Effective pinning: the proxy's hostname is *.svc.cluster.local, which no
	// public CA will issue a certificate for. Trusting both system roots and
	// the per-tenant proxy CA therefore preserves the property that only this
	// proxy's cert can validate for the proxy hostname.
	//
	// If the file is absent (local dev, no TLS proxy) we fall through and the
	// standard transport uses HTTP proxy as before.
	proxyCACert := filepath.Join(proxyCACertDir, "tls.crt")
	if certPEM, err := os.ReadFile(proxyCACert); err == nil {
		pool, err := transport.BuildProxyTrustPool(certPEM)
		if err != nil {
			return fmt.Errorf("build proxy trust pool from %s: %w", proxyCACert, err)
		}
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = &tls.Config{RootCAs: pool}
		http.DefaultTransport = t
	}

	// ── 1. Read credentials from mounted Secret files ────────────────────────
	appIDBytes, err := os.ReadFile(filepath.Join(credsDir, "appId"))
	if err != nil {
		return fmt.Errorf("read appId: %w", err)
	}
	appID, err := strconv.ParseInt(strings.TrimSpace(string(appIDBytes)), 10, 64)
	if err != nil {
		return fmt.Errorf("parse appId: %w", err)
	}

	installIDBytes, err := os.ReadFile(filepath.Join(credsDir, "installationId"))
	if err != nil {
		return fmt.Errorf("read installationId: %w", err)
	}
	installID, err := strconv.ParseInt(strings.TrimSpace(string(installIDBytes)), 10, 64)
	if err != nil {
		return fmt.Errorf("parse installationId: %w", err)
	}

	pemBytes, err := os.ReadFile(filepath.Join(credsDir, "privateKey"))
	if err != nil {
		return fmt.Errorf("read privateKey: %w", err)
	}

	// ── 2. Build token provider and manager ─────────────────────────────────
	creds := githubapp.Credentials{
		AppID:          appID,
		PrivateKeyPEM:  pemBytes,
		InstallationID: installID,
	}
	rawProvider, err := githubapp.NewInstallationTokenProvider(creds, nil)
	if err != nil {
		return fmt.Errorf("create token provider: %w", err)
	}
	expProvider, ok := rawProvider.(githubapp.ExpiringTokenProvider)
	if !ok {
		return fmt.Errorf("token provider does not implement ExpiringTokenProvider")
	}
	tokenMgr := token.NewManager(expProvider, nil)

	// ── 3. Build Prometheus metrics ──────────────────────────────────────────
	m := listener.NewMetrics()

	// ── 4. Build scheme ──────────────────────────────────────────────────────
	scheme := runtime.NewScheme()
	// clientgoscheme already includes corev1; no need to add it separately.
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}

	// ── 5. Start the controller-runtime manager ──────────────────────────────
	// Restrict the cache to POD_NAMESPACE so the AGC only watches resources in
	// its own tenant namespace. A cluster-scoped cache would require a
	// ClusterRole; a namespace-scoped cache works with the Role+RoleBinding
	// that GMC creates per tenant.
	namespace := os.Getenv("POD_NAMESPACE")
	cacheOpts := cache.Options{}
	if namespace != "" {
		cacheOpts.DefaultNamespaces = map[string]cache.Config{namespace: {}}
	}
	metricsOpts, err := buildMetricsOptions(metricsCertDir, ctrl.Log.WithName("metrics"))
	if err != nil {
		return fmt.Errorf("configure metrics server: %w", err)
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Cache:   cacheOpts,
		Metrics: metricsOpts,
		// Expose /healthz and /readyz so the kubelet can detect a wedged AGC and
		// restart it. The bind address must match the probe port the GMC stamps
		// on the AGC Deployment (see healthProbeBindAddress).
		HealthProbeBindAddress: healthProbeBindAddress,
		// Three-part Secret isolation (see plan/security.md §H-2):
		// 1. DisableFor here — all r.Get() and r.List() calls on Secrets bypass
		//    the controller-runtime cache and hit the API server directly, so
		//    Secret bodies never buffer in-process beyond the duration of a call.
		//    This covers the agentpool's metadata-only PartialObjectMetadataList
		//    too: its GVK resolves to Secret, which matches DisableFor.
		// 2. SetupWithManager registers no Watches or WatchesMetadata for Secrets
		//    — no Secret informer (full or metadata-only) is ever established, so
		//    nothing caches Secret data or metadata in the background.
		// 3. The AGC Role grants list (not watch) on Secrets — list is needed by
		//    the agentpool to enumerate its agent Secrets; watch was removed (Q26)
		//    because no Secret informer is established and granting it would be
		//    dead privilege. The agentpool lists metadata only and reads bodies
		//    per-name via Get (k8s-best-practices §B B4 / Q57), so bulk lists never
		//    transfer credential bodies; any read still requires live API server
		//    calls in the audit log.
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	// Register the health/ready checks the kubelet probes hit. healthz.Ping
	// returns ok once the manager's health server is listening (bound early in
	// mgr.Start, independently of the initial-token Runnable below), so a wedged
	// AGC is restarted rather than running invisibly. The AGC Deployment's
	// startupProbe gives cache-sync grace before liveness takes over — see the
	// probe comment in builder.go.
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add readyz check: %w", err)
	}

	// ── 6. Start token manager ───────────────────────────────────────────────
	ctx := ctrl.SetupSignalHandler()
	tokenMgr.Namespace = namespace
	tokenMgr.Metrics = m
	tokenMgr.Logger = ctrl.Log.WithName("token")
	tokenMgr.Start(ctx)

	// Acquire the first token as a manager Runnable rather than a blocking
	// pre-Start call. This matters for the health probes: mgr.Start binds the
	// /healthz + /readyz server (healthProbeBindAddress) early — within
	// cache-sync time of pod start — independently of this fetch. A blocking
	// pre-Start wait would instead leave the health endpoint unbound for the
	// whole token exchange (up to 2m), so the kubelet's probes would fail and
	// the AGC Deployment would never report ready replicas under a slow token
	// exchange — coupling rollout success to GitHub reachability at startup.
	//
	// Fail-fast is preserved: if the token cannot be obtained within the
	// deadline the Runnable returns an error, which stops the manager and makes
	// run() exit non-zero so the kubelet restarts the pod — the same outcome as
	// the previous blocking wait. Running the reconciler before the token is
	// ready is safe: token.Manager.Token blocks on the first fetch, and
	// RunnerGroupReconciler.Reconcile requeues on a Token() error, so no
	// reconcile acts on a missing token.
	//
	// Bookended by log lines so a stuck fetch is visible. 2 minutes is
	// deliberate: the in-loop backoff (5s → 10s → 20s → 40s → 60s) fits ~6
	// attempts in this budget, which absorbs slow-startup transients (kube-proxy
	// programming, Service endpoint sync, image pull contention on a 2-CPU
	// runner) that resolve in the 30–90s window. Beyond 2 minutes you're almost
	// certainly in persistent-failure territory where kubelet's CrashLoopBackOff
	// escalation produces equivalent restart cadence either way, and the
	// per-attempt error log lines already surface the cause.
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		ctrl.Log.Info("waiting for initial GitHub App token")
		tokenCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if _, err := tokenMgr.Token(tokenCtx); err != nil {
			return fmt.Errorf("initial token fetch: %w", err)
		}
		ctrl.Log.Info("initial token acquired")
		return nil
	})); err != nil {
		return fmt.Errorf("add initial-token runnable: %w", err)
	}

	// ── 7. Register reconciler ───────────────────────────────────────────────
	httpClient := &http.Client{Timeout: 60 * time.Second}
	prov := provisioner.NewProvisioner(mgr.GetClient(), m, nil)
	prov.WorkerSA = os.Getenv("WORKER_SERVICE_ACCOUNT")
	prov.HTTPProxy = os.Getenv("HTTP_PROXY")
	prov.HTTPSProxy = os.Getenv("HTTPS_PROXY")
	prov.NoProxy = os.Getenv("NO_PROXY")
	// PROXY_TLS_SECRET_NAME names the Secret holding the per-tenant
	// egress-proxy CA cert. The GMC sets this on the AGC Deployment so the
	// provisioner can project it (cert only, via Items) into every worker
	// pod. Empty (the default) disables the mount and is appropriate for
	// any deployment without a per-tenant egress proxy.
	prov.ProxyTLSSecretName = os.Getenv("PROXY_TLS_SECRET_NAME")
	// SECURITY_PROFILE mirrors the tenant's ActionsGateway.spec.securityProfile.
	// The GMC sets it on the AGC Deployment so the provisioner can scale the
	// secure-by-default worker SecurityContext to the namespace's PSA level.
	prov.SecurityProfile = os.Getenv("SECURITY_PROFILE")
	prov.HTTPClient = httpClient
	if img := os.Getenv("WORKER_IMAGE"); img != "" {
		prov.DefaultWorkerImage = img
	}
	prov.TokenFunc = tokenMgr.Token

	// Detect worker-pod completion off the shared Pod informer rather than
	// polling per session: one event handler serves every in-flight session, so
	// detection is near-immediate and no per-session ticker is spawned. Run it
	// as a manager Runnable so the handler is registered after the cache syncs.
	podWaiter := provisioner.NewInformerPodWaiter(mgr.GetCache(), nil)
	if err := mgr.Add(podWaiter); err != nil {
		return fmt.Errorf("add pod completion watcher: %w", err)
	}
	prov.Waiter = podWaiter

	// Choose registrar:
	//   GITHUB_ORG_URL set                  → GithubRegistrar (production)
	//   STUB_AUTH_URL / STUB_BROKER_URL set  → StubRegistrar with those URLs (testing)
	//   neither                              → error: GITHUB_ORG_URL is required
	var registrar agentpool.Registrar
	if orgURL := os.Getenv("GITHUB_ORG_URL"); orgURL != "" {
		groupID := 1
		if raw := os.Getenv("GITHUB_RUNNER_GROUP_ID"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				groupID = parsed
			}
		}
		registrar = &agentpool.GithubRegistrar{
			OrgURL:  orgURL,
			GroupID: groupID,
		}
	} else {
		stubAuthURL := os.Getenv("STUB_AUTH_URL")
		stubBrokerURL := os.Getenv("STUB_BROKER_URL")
		if stubAuthURL == "" || stubBrokerURL == "" {
			return fmt.Errorf("GITHUB_ORG_URL is required (for testing set both STUB_AUTH_URL and STUB_BROKER_URL)")
		}
		registrar = agentpool.NewStubRegistrarWithURLs(stubAuthURL, stubBrokerURL)
	}

	r := &controller.RunnerGroupReconciler{
		Client:       mgr.GetClient(),
		TokenManager: tokenMgr,
		Registrar:    registrar,
		Metrics:      m,
		Provisioner:  prov,
		AgentKeyType: agentKeyType,
		Recorder:     mgr.GetEventRecorder("runnergroup-controller"),
		BrokerConfig: controller.BrokerConfig{
			BrokerURL:     os.Getenv("GITHUB_BROKER_URL"),
			RunnerVersion: os.Getenv("GITHUB_RUNNER_VERSION"),
			RunnerOS:      os.Getenv("GITHUB_RUNNER_OS"),
			RunnerArch:    os.Getenv("GITHUB_RUNNER_ARCH"),
			UseV2Flow:     os.Getenv("GITHUB_USE_VSTS_FLOW") != "true",
		},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup reconciler: %w", err)
	}

	ctrl.Log.Info("starting AGC manager")
	return mgr.Start(ctx)
}
