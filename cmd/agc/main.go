// Command agc is the Actions Gateway Controller (AGC).
// It reconciles RunnerGroup CRDs into adaptive listener goroutine pools that
// long-poll the GitHub Actions broker for incoming workflow jobs.
//
// The gateway authenticates to GitHub by one of two credential methods, selected
// by the GMC via the CREDENTIAL_TYPE env (see buildTokenProvider in credentials.go):
//
//   - GitHubApp (possession, the default): GitHub App credentials are read from
//     files under /etc/actions-gateway/github-app/ (projected from a Kubernetes
//     Secret by the GMC). Keys: appId, installationId, privateKey (PEM).
//   - WorkloadIdentity (delegation, Q201): no App private key in the cluster — the
//     App JWT is signed via Vault transit (githubapp/vaultsigner), with the AGC
//     proving its pod identity to Vault using a projected ServiceAccount token.
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
	"bytes"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/token"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/tracing"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/transport"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/usage"
	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/githubapp"
	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
	"github.com/go-logr/logr"
	uberzap "go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// version is the AGC build version, stamped as the OpenTelemetry service.version
// resource attribute. Overridable at build time via -ldflags "-X main.version=…".
var version = "dev"

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
	credsDir       = "/etc/actions-gateway/github-app" //nolint:gosec // G101: a mount-path constant, not a credential
	proxyCACertDir = "/etc/actions-gateway/proxy-ca"

	// githubCACertDir is where the GMC mounts the operator-supplied GitHub CA bundle
	// named by ActionsGateway.spec.githubCABundleRef (Q536); githubCAConfigMapEnv
	// carries the ConfigMap's name, which is both the opt-in signal and what the
	// provisioner needs to project the same bundle into worker pods.
	githubCACertDir      = "/etc/actions-gateway/github-ca"
	githubCACertFile     = "ca.crt"
	githubCAConfigMapEnv = "GITHUB_CA_CONFIGMAP_NAME"

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
	// (ca.crt + tls.crt + tls.key). See buildMetricsOptions for the
	// TLS-when-mounted behavior.
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

// slogDebugZapLevel is the zap core level that must be enabled for the AGC's
// hot-path slog.Debug lines (listener/provisioner/agentpool) to surface. The
// process logs through log/slog bridged onto controller-runtime's logr logger
// (slog.SetDefault(logr.ToSlogHandler(ctrl.Log)) below). go-logr maps slog.Debug
// (slog.LevelDebug, -4) to logr V(4), and zapr inverts logr V(n) to zap level -n
// (zapr.toZapLevel) — so a slog.Debug record is gated at zap level -4. That is
// BELOW zapcore.DebugLevel (-1): a LOG_LEVEL=debug that only set DebugLevel would
// surface controller-runtime's own V(0)/V(1) lines but silently drop every
// per-session/per-job slog.Debug line the knob exists to surface, leaving the
// e2e session diagnostics dump blind to why a job was never acquired (Q148). The
// proxy is unaffected — it logs through a native slog handler, not this bridge.
const slogDebugZapLevel = zapcore.Level(-4)

// zapLevelFromEnv maps the LOG_LEVEL env value (info|debug, default info) to a
// zap level override, or nil when no override is needed. "debug" enables down to
// slogDebugZapLevel so the AGC actually surfaces the per-session/per-job slog.Debug
// lines (logging-audit Theme F); "info" and "" return nil so the caller leaves
// zap.Options at its production default (info). Any other value is treated as
// info — the CRD enum already rejects out-of-range values before they reach the
// AGC, so this is only a defensive fallback. Mirrors cmd/proxy's logLevelFromEnv.
func zapLevelFromEnv(v string) zapcore.LevelEnabler {
	if strings.EqualFold(v, "debug") {
		lvl := uberzap.NewAtomicLevelAt(slogDebugZapLevel)
		return &lvl
	}
	return nil
}

// normalizeDebugLevel deepens a plain "debug" override to slogDebugZapLevel so
// that "debug" surfaces the hot-path slog.Debug lines regardless of whether it
// arrived via --zap-log-level=debug or LOG_LEVEL=debug. --zap-log-level=debug
// binds the level to zapcore.DebugLevel (-1), which surfaces controller-runtime's
// own V(0)/V(1) lines but NOT the V(4) slog.Debug lines (see slogDebugZapLevel);
// zapLevelFromEnv already lands at the right level. The override is returned
// unchanged when nil, not debug-enabled (info/warn/error), or already at least as
// deep as slogDebugZapLevel — so only an explicit DebugLevel is deepened (Q148).
func normalizeDebugLevel(lvl zapcore.LevelEnabler) zapcore.LevelEnabler {
	if lvl != nil && lvl.Enabled(zapcore.DebugLevel) && !lvl.Enabled(slogDebugZapLevel) {
		out := uberzap.NewAtomicLevelAt(slogDebugZapLevel)
		return &out
	}
	return lvl
}

func run() error {
	// ── 0. Parse flags ───────────────────────────────────────────────────────
	agentKeyTypeFlag := flag.String("agent-key-type", "rsa",
		"Key type for new agent registrations: rsa (default) or ed25519 (opt-in; loses session-key encryption)")
	// Bind zap's logging flags (--zap-devel, --zap-encoder, --zap-log-level, …)
	// and default to production logging: structured JSON at info level, which log
	// aggregators can parse. The GMC stamps no logging args onto the AGC
	// Deployment, so this default is what actually ships in production — correct
	// by default rather than relying on an operator remembering to flip a flag.
	// Developers pass --zap-devel for human-readable console logs at debug level
	// when running locally. Kept consistent with the GMC (cmd/gmc/cmd/main.go).
	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	// Snapshot the AGC's environment-variable config surface once, right after
	// flag parsing, so every downstream read is a struct-field access rather than
	// a scattered os.Getenv (Q367). loadConfig is raw reads only; each
	// erroring/side-effecting parse still happens at its point of use.
	cfg := loadConfig(os.Getenv)

	// LOG_LEVEL (info|debug, default info) is the per-tenant verbosity knob the
	// GMC threads from ActionsGateway.spec.logLevel (logging-audit Theme G),
	// mirroring how spec.securityProfile flows as SECURITY_PROFILE. BindFlags only
	// sets zapOpts.Level when --zap-log-level is passed explicitly, so applying
	// the env solely when Level is still nil lets a local developer's flag win;
	// the GMC never stamps logging flags onto the AGC Deployment, so in
	// production LOG_LEVEL is the sole level source.
	if zapOpts.Level == nil {
		if lvl := zapLevelFromEnv(cfg.LogLevel); lvl != nil {
			zapOpts.Level = lvl
		}
	}
	zapOpts.Level = normalizeDebugLevel(zapOpts.Level)
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	// Bridge log/slog onto the same zap logger. The listener, provisioner, and
	// agentpool packages log through log/slog; without this, slog.Default() is
	// the stdlib TEXT handler, so a single AGC pod would emit mixed zap-JSON +
	// stdlib-text on one stream that a log pipeline cannot parse (k8s audit F1).
	// Routing slog through ctrl.Log gives the whole process one JSON shape and
	// one level source (--zap-log-level). The named loggers injected below
	// inherit this same sink; the bridge also catches any slog.Default() call
	// site that is not explicitly wired.
	slog.SetDefault(slog.New(logr.ToSlogHandler(ctrl.Log)))

	agentKeyType := agentpool.KeyType(*agentKeyTypeFlag)
	switch agentKeyType {
	case agentpool.KeyTypeEd25519, agentpool.KeyTypeRSA:
	default:
		return fmt.Errorf("invalid --agent-key-type %q: must be ed25519 or rsa", agentKeyType)
	}

	// ── 0.4. Initialise OpenTelemetry tracing (opt-in, off by default) ───────
	// tracing.Init installs an OTLP exporter only when an OTLP endpoint is
	// configured (OTEL_EXPORTER_OTLP[_TRACES]_ENDPOINT) and OTEL_SDK_DISABLED is
	// not "true"; otherwise the global no-op provider stays in place and the
	// reconciler/provisioner spans are nearly free. Shutdown flushes buffered
	// spans on exit. Using context.Background() (not the signal context) keeps the
	// flush working after SIGTERM cancels mgr.Start.
	tracingShutdown, tracingEnabled, err := tracing.Init(context.Background(), version, slog.Default())
	if err != nil {
		return fmt.Errorf("init tracing: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracingShutdown(shutdownCtx); err != nil {
			ctrl.Log.WithName("tracing").Error(err, "tracing shutdown")
		}
	}()
	if tracingEnabled {
		ctrl.Log.Info("OpenTelemetry tracing enabled", "service", tracing.ServiceName)
	}

	// ── 0.5. Configure the outbound TLS trust pool ──────────────────────────
	// MUST run before any proxy-traversing client is built (the token provider,
	// the provisioner's GitHub client), so those clients pick up the proxy CA — an
	// ordering dependency that is invisible until it breaks.
	if err := configureTrustPool(proxyCACertDir, githubCACertDir, ctrl.Log.WithName("ca-trust")); err != nil {
		return err
	}

	// ── 1+2. Build the installation-token provider for the configured credential
	// method (possession/GitHubApp reads the mounted Secret files; delegation/
	// WorkloadIdentity signs the App JWT via Vault with no in-cluster key — Q201).
	// buildTokenProvider gates a non-HTTPS GitHub/Vault address on the dev/test
	// STUB_AUTH_URL signal, which only reaches a GMC-provisioned AGC via AGC_EXTRA_*
	// under the testing-only --allow-agc-extra-env flag; production AGCs keep HTTPS
	// mandatory.
	expProvider, err := buildTokenProvider(ctrl.Log.WithName("credentials"))
	if err != nil {
		return err
	}
	tokenMgr := token.NewManager(expProvider, nil)

	// ── 3. Build Prometheus metrics ──────────────────────────────────────────
	m := runnercore.NewMetrics()
	// The ScaleSet acquisition tier (Q264 Option E) emits its own counter series,
	// separate from the classic listener metrics; a Classic-only AGC simply never
	// increments them.
	sm := scalesetlistener.NewMetrics(ctrlmetrics.Registry)
	// Emit actions_gateway_build_info{component="agc",version=…} 1 so the running
	// binary version is correlatable straight from metrics during an incident
	// (Q318). Registered on the controller-runtime registry the AGC already
	// serves at /metrics.
	registerBuildInfo(ctrlmetrics.Registry, "agc", version)

	// ── 4. Build scheme ──────────────────────────────────────────────────────
	scheme, err := buildScheme()
	if err != nil {
		return err
	}

	// ── 5. Start the controller-runtime manager ──────────────────────────────
	// Restrict the cache to POD_NAMESPACE so the AGC only watches resources in
	// its own tenant namespace. A cluster-scoped cache would require a
	// ClusterRole; a namespace-scoped cache works with the Role+RoleBinding
	// that GMC creates per tenant.
	namespace := cfg.PodNamespace
	cacheOpts := cache.Options{}
	if namespace != "" {
		cacheOpts.DefaultNamespaces = map[string]cache.Config{namespace: {}}
	}
	// Multi-gateway scoping (§H.16 #1): when GATEWAY_NAME is set (the GMC stamps it
	// on every v2 AGC Deployment), restrict the RunnerSet informer to the sets this
	// gateway owns via a server-side field selector on spec.gatewayRef.name (a CRD
	// selectable field, KEP-4358). This is the isolation boundary: N AGC Deployments
	// in one namespace each watch and reconcile only their own gateway's RunnerSets,
	// so they never contend over the same objects. ByObject.Namespaces is left nil so
	// it inherits DefaultNamespaces (the tenant namespace). Empty GATEWAY_NAME leaves
	// the informer unscoped (a single AGC reconciles every RunnerSet — pre-M3b).
	gatewayName := cfg.GatewayName
	if gatewayName != "" {
		cacheOpts.ByObject = map[client.Object]cache.ByObject{
			&agcv2alpha1.RunnerSet{}: {
				Field: fields.OneTermEqualSelector("spec.gatewayRef.name", gatewayName),
			},
		}
	}
	metricsOpts, err := buildMetricsOptions(metricsCertDir, ctrl.Log.WithName("metrics"))
	if err != nil {
		return fmt.Errorf("configure metrics server: %w", err)
	}
	// Dedupe API server warnings to one log line per unique message per process.
	// The default handler logs every occurrence, and the v1alpha1/v2alpha1
	// deprecation warnings repeat on every RunnerGroup and RunnerSet read/write —
	// under reconcile churn they dominate the log (Q515).
	restCfg := ctrl.GetConfigOrDie()
	restCfg.WarningHandlerWithContext = logf.NewKubeAPIWarningLogger(
		logf.KubeAPIWarningLoggerOptions{Deduplicate: true})
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
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
	prov, err := setupProvisioner(mgr, cfg, m, tokenMgr)
	if err != nil {
		return err
	}

	// Worker usage sampler (Q359 Phase 2): a nil *Sampler is a safe no-op source,
	// so the RunnerSet reconciler below can consume it unconditionally.
	usageSampler, err := setupUsageSampler(mgr, cfg, namespace)
	if err != nil {
		return err
	}

	// Choose registrar (buildRegistrar):
	//   STUB_AUTH_URL + STUB_BROKER_URL set → StubRegistrar with those URLs (testing)
	//   GITHUB_ORG_URL set                  → GithubRegistrar (production)
	//   neither                             → error: GITHUB_ORG_URL is required
	//
	// The stub path is checked FIRST so an explicitly-configured fakegithub stub
	// wins even though a GMC-provisioned AGC now always carries GITHUB_ORG_URL
	// (threaded from the required spec.gitHubURL field). STUB_AUTH_URL /
	// STUB_BROKER_URL only reach a GMC-provisioned AGC via AGC_EXTRA_* with the
	// testing-only --allow-agc-extra-env flag, so production AGCs never have them
	// set and always fall through to the GithubRegistrar. To switch a stub-backed
	// AGC to real GitHub, unset the stub env (the e2e suite does exactly this).
	registrar, err := buildRegistrar(cfg)
	if err != nil {
		return err
	}

	if err := registerReconcilers(mgr, reconcilerDeps{
		gatewayName:  gatewayName,
		brokerCfg:    buildBrokerConfig(cfg),
		tokenMgr:     tokenMgr,
		registrar:    registrar,
		metrics:      m,
		scaleSet:     sm,
		prov:         prov,
		agentKeyType: agentKeyType,
		usageSampler: usageSampler,
		// Keyed off exactly the pair buildRegistrar's stub case uses, so the two
		// acquisition tiers cannot end up pointed at different backends.
		scaleSetStubURL: scaleSetStubBaseURL(cfg),
	}); err != nil {
		return err
	}

	ctrl.Log.Info("starting AGC manager")
	return mgr.Start(ctx)
}

// reconcilerDeps is the process-wide machinery the reconcilers share. The
// TokenManager, Registrar, Metrics and Provisioner are single instances per
// process (the provisioner Target seam own-refs whichever CR is being served).
type reconcilerDeps struct {
	gatewayName  string
	brokerCfg    controller.BrokerConfig
	tokenMgr     *token.Manager
	registrar    agentpool.Registrar
	metrics      *runnercore.Metrics
	scaleSet     *scalesetlistener.Metrics
	prov         *provisioner.Provisioner
	agentKeyType agentpool.KeyType
	usageSampler *usage.Sampler
	// scaleSetStubURL re-points the scale-set bootstrap at a fake-GitHub stub, for
	// the deployed fake-GitHub e2e tier. Empty in production — it is set only from
	// the same STUB_AUTH_URL + STUB_BROKER_URL pair that selects the classic tier's
	// StubRegistrar, which reaches a GMC-provisioned AGC only under the testing-only
	// --allow-agc-extra-env flag.
	scaleSetStubURL string
}

// registerReconcilers registers the reconcilers this AGC's role calls for. The two
// gates are complementary, so **each AGC process serves exactly one API**: the v1
// singleton (no GATEWAY_NAME) reconciles RunnerGroups, and a gateway-scoped AGC
// reconciles only its own gateway's RunnerSets. A migrated namespace therefore runs
// the v1 RunnerGroup and the v2 RunnerSet it became in separate processes even when
// they share a name, and neither can contend for the other's objects.
func registerReconcilers(mgr ctrl.Manager, deps reconcilerDeps) error {
	// Registered only on the v1 AGC — the mirror of the RunnerSet gate below (Q535).
	// ServesRunnerGroups carries the rationale.
	if controller.ServesRunnerGroups(deps.gatewayName) {
		r := &controller.RunnerGroupReconciler{
			Client:       mgr.GetClient(),
			Log:          slog.New(logr.ToSlogHandler(ctrl.Log.WithName("runnergroup"))),
			TokenManager: deps.tokenMgr,
			Registrar:    deps.registrar,
			Metrics:      deps.metrics,
			Provisioner:  deps.prov,
			AgentKeyType: deps.agentKeyType,
			Recorder:     mgr.GetEventRecorder("runnergroup-controller"),
			BrokerConfig: deps.brokerCfg,
		}
		if err := r.SetupWithManager(mgr); err != nil {
			return fmt.Errorf("setup reconciler: %w", err)
		}
	} else {
		ctrl.Log.Info("GATEWAY_NAME set; v1alpha1 RunnerGroup reconciler disabled "+
			"(RunnerGroups are served by the v1 AGC, which sets no GATEWAY_NAME)",
			"gateway", deps.gatewayName)
	}

	// v2 RunnerSet reconciler (M3a). The RunnerSet CRD (and the sibling
	// ActionsGateway/EgressProxy/RunnerTemplate kinds this reconciler watches) ships
	// in the opt-in actions-gateway-crds-v2 chart, so a v1-only install serves none
	// of them. Registering the reconciler there would leave its informer cache
	// unable to sync, and mgr.Start would exit(1) after the cache-sync deadline —
	// crash-looping the AGC (Q261). Detect the CRD once at startup and gate
	// registration on it, mirroring the GMC's v2 detection (Q228). On a v1-only
	// install the v1 RunnerGroup reconciler above runs normally; installing the v2
	// CRDs later requires an AGC restart to enable this reconciler.
	v2Enabled, err := controller.RunnerSetInstalled(mgr.GetRESTMapper())
	if err != nil {
		return fmt.Errorf("detect actions-gateway.com/v2alpha1 RunnerSet CRD: %w", err)
	}
	switch {
	case !v2Enabled && deps.gatewayName != "":
		// This combination leaves the process with no reconciler at all. Fail fast
		// rather than run a pod that passes its probes and reconciles nothing: the GMC
		// stamps GATEWAY_NAME only from a v2 ActionsGateway, which needs these CRDs to
		// exist, so this is a broken install rather than a supported mode.
		return fmt.Errorf("GATEWAY_NAME=%s is set but the actions-gateway.com/v2alpha1 RunnerSet CRD is "+
			"not installed: a gateway-scoped AGC serves RunnerSets only, so it would reconcile nothing "+
			"(install the actions-gateway-crds-v2 chart)", deps.gatewayName)

	case !v2Enabled:
		ctrl.Log.Info("actions-gateway.com/v2alpha1 RunnerSet CRD not installed; " +
			"v1-only mode, v2 RunnerSet reconciler disabled " +
			"(install the actions-gateway-crds-v2 chart and restart the AGC to enable it)")

	case deps.gatewayName == "":
		// A RunnerSet is always served by the AGC of the gateway its spec.gatewayRef
		// names, and the GMC stamps GATEWAY_NAME on every one of those AGC Deployments
		// (§H.16 #1). An AGC without it is the v1 singleton, and it must not reconcile
		// RunnerSets: during a v1→v2 migration the tenant namespace holds both, so an
		// unscoped RunnerSet reconciler here would run a second listener pool and a
		// second set of GitHub registrations for a set the migrated gateway's own AGC
		// is already serving — two controllers on one object, which no amount of
		// naming can separate. It is also what drove the v1 AGC to list the
		// cluster-scoped ClusterRunnerTemplate kind it holds no grant for, error-looping
		// on `clusterrunnertemplates … is forbidden` for the whole coexistence window
		// (Q466, measured live). Declining the work is the least-privilege fix: the v1
		// AGC needs no cluster-scoped grant because it has no cluster-scoped read to do.
		ctrl.Log.Info("no GATEWAY_NAME set; this AGC serves v1alpha1 RunnerGroups only " +
			"(v2 RunnerSets are reconciled by their own gateway's AGC)")

	default:
		ctrl.Log.Info("actions-gateway.com/v2alpha1 RunnerSet CRD detected; enabling v2 RunnerSet reconciler")

		rsr := &controller.RunnerSetReconciler{
			Client: mgr.GetClient(),
			// Uncached: the v1alpha1 RunnerGroup probe that gates adoption of pre-Q466
			// agent Secrets must not establish an informer for a kind a v2-only install
			// may not serve.
			APIReader:       mgr.GetAPIReader(),
			Log:             slog.New(logr.ToSlogHandler(ctrl.Log.WithName("runnerset"))),
			TokenManager:    deps.tokenMgr,
			Registrar:       deps.registrar,
			Metrics:         deps.metrics,
			ScaleSetMetrics: deps.scaleSet,
			Provisioner:     deps.prov,
			AgentKeyType:    deps.agentKeyType,
			GatewayName:     deps.gatewayName,
			Recorder:        mgr.GetEventRecorder("runnerset-controller"),
			EventReader:     mgr.GetAPIReader(),
			BrokerConfig:    deps.brokerCfg,
			Sizing:          deps.usageSampler,
			// Empty unless the same stub env that selected the StubRegistrar does, so
			// production always derives the scale-set endpoints from githubURL.
			ScaleSetStubBaseURL: deps.scaleSetStubURL,
		}
		if err := rsr.SetupWithManager(mgr); err != nil {
			return fmt.Errorf("setup runnerset reconciler: %w", err)
		}
	}
	return nil
}

// configureTrustPool extends the process-wide http.DefaultTransport trust pool
// with the CAs the GMC mounted for this gateway: the per-tenant egress proxy's
// self-signed cert at proxyDir/tls.crt, and the operator-supplied GitHub CA bundle
// at githubDir/ca.crt (Q536).
//
// Both are added alongside the system roots, never in place of them, so the AGC can
// validate the proxy's self-signed cert and a private-CA GHES appliance without
// losing the ability to validate upstream endpoints (api.github.com,
// pipelinesghubeus*.actions.githubusercontent.com) over the proxy's CONNECT tunnel.
// Go's http.Transport uses one TLSClientConfig for both the AGC↔proxy hop and the
// AGC↔upstream-over-tunnel hop, so the pool must satisfy both.
//
// Effective pinning: the proxy's hostname is *.svc.cluster.local, which no public
// CA will issue a certificate for. Trusting both system roots and the per-tenant
// proxy CA therefore preserves the property that only this proxy's cert can
// validate for the proxy hostname.
//
// The two sources differ in what an absent file means. The proxy CA is optional at
// runtime, so absent (and empty) is a logged no-op; any other read error — a
// permission-denied mount above all — is fatal, mirroring buildMetricsOptions,
// because a mounted-but-unreadable CA is a misconfiguration that would otherwise
// hide behind unrelated x509 failures later (Q520). The GitHub CA bundle is read
// only when GITHUB_CA_CONFIGMAP_NAME is set, which is the gateway's explicit
// opt-in, so there is no "not configured" reading of a failed or empty read and
// every one is fatal.
//
// With neither mounted (local dev, no TLS proxy, public GitHub) this is a no-op and
// the standard transport is left unchanged. It mutates the global transport, so it
// must run before any proxy-traversing client is constructed (AGC proxy-client init
// order, see the project memory).
func configureTrustPool(proxyDir, githubDir string, log logr.Logger) error {
	proxyCACert := filepath.Join(proxyDir, "tls.crt")
	proxyPEM, err := os.ReadFile(proxyCACert)
	switch {
	case err != nil && !os.IsNotExist(err):
		return fmt.Errorf("read proxy CA %s: %w", proxyCACert, err)
	case err != nil:
		log.Info("proxy CA cert absent; leaving the default transport unchanged (no TLS egress proxy)", "path", proxyCACert)
	case len(bytes.TrimSpace(proxyPEM)) == 0:
		// Empty file: tolerated, matching the worker entrypoint wrapper. Logged so
		// the mounted-but-empty case is distinguishable from the unmounted one.
		log.Info("proxy CA cert is empty; leaving the default transport unchanged", "path", proxyCACert)
		proxyPEM = nil
	}

	var githubPEM []byte
	githubCACert := filepath.Join(githubDir, githubCACertFile)
	if os.Getenv(githubCAConfigMapEnv) != "" {
		if githubPEM, err = os.ReadFile(githubCACert); err != nil {
			return fmt.Errorf("read GitHub CA bundle %s: %w", githubCACert, err)
		}
		if len(bytes.TrimSpace(githubPEM)) == 0 {
			return fmt.Errorf("GitHub CA bundle %s is empty: githubCABundleRef names it, so the appliance would not be trusted", githubCACert)
		}
	}

	pool, err := transport.BuildTrustPool(proxyPEM, githubPEM)
	if err != nil {
		return fmt.Errorf("build trust pool from %s, %s: %w", proxyCACert, githubCACert, err)
	}
	if pool == nil {
		return nil
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool}
	http.DefaultTransport = t
	return nil
}

// setupProvisioner builds the worker provisioner from config and registers its
// manager Runnables (the informer-backed pod-completion watcher and the
// eviction-counter sweeper). It returns the wired provisioner for the reconcilers
// to consume. The wrapper-delivery detection makes a live apiserver-version
// discovery call, so this must run after the manager is constructed.
func setupProvisioner(mgr ctrl.Manager, cfg agcConfig, m *runnercore.Metrics,
	tokenMgr *token.Manager) (*provisioner.Provisioner, error) {
	// Bounded client for the provisioner's GitHub REST calls (Q138): an overall
	// 60s deadline plus a transport ResponseHeaderTimeout, so a slow GitHub API
	// cannot wedge a reconcile.
	httpClient := httpx.NewClientWithTimeout(60 * time.Second)
	prov := provisioner.NewProvisioner(mgr.GetClient(), m,
		slog.New(logr.ToSlogHandler(ctrl.Log.WithName("provisioner"))))
	prov.WorkerSA = cfg.WorkerServiceAccount
	prov.HTTPProxy = cfg.HTTPProxy
	prov.HTTPSProxy = cfg.HTTPSProxy
	prov.NoProxy = cfg.NoProxy
	// PROXY_TLS_SECRET_NAME names the Secret holding the per-tenant egress-proxy CA
	// cert. The GMC sets this on the AGC Deployment so the provisioner can project
	// it (cert only, via Items) into every worker pod. Empty (the default) disables
	// the mount and is appropriate for any deployment without a per-tenant egress
	// proxy.
	prov.ProxyTLSSecretName = cfg.ProxyTLSSecretName
	// GITHUB_CA_CONFIGMAP_NAME names the ConfigMap holding the GHES appliance's CA
	// bundle (Q536). The GMC sets it from spec.githubCABundleRef so the provisioner
	// projects the same bundle the AGC itself trusts into every worker pod. Empty
	// (the default) disables the mount, which is right for public GitHub.
	prov.GitHubCAConfigMapName = cfg.GitHubCAConfigMap
	// SECURITY_PROFILE mirrors the tenant's ActionsGateway.spec.securityProfile.
	// The GMC sets it on the AGC Deployment so the provisioner can scale the
	// secure-by-default worker SecurityContext to the namespace's PSA level.
	prov.SecurityProfile = cfg.SecurityProfile
	prov.HTTPClient = httpClient
	// #784: the admission gate refuses to claim a job when the namespace
	// ResourceQuota has no headroom for its worker pod, instead of claiming it and
	// burning lock time in createPodWithQuotaRetry. ON by default; AGC_QUOTA_ADMISSION=false
	// reverts to the pre-#784 behaviour. Operator runbook: the quota-backpressure
	// section in docs/operations/troubleshooting.md.
	prov.DisableQuotaAdmission = cfg.QuotaAdmission == "false"
	if cfg.WorkerImage != "" {
		prov.DefaultWorkerImage = cfg.WorkerImage
	}
	// WRAPPER_IMAGE enables runtime wrapper injection (Q235): the runner image is
	// the unmodified upstream actions-runner (or any tenant image) and the GAG
	// wrapper is delivered into each worker pod. Empty disables injection (the
	// worker image must then carry the wrapper as its own entrypoint).
	if cfg.WrapperImage != "" {
		prov.WrapperImage = cfg.WrapperImage
		prov.UseImageVolume = useImageVolume(mgr.GetConfig(), cfg.WrapperDelivery)
	}
	prov.TokenFunc = tokenMgr.Token
	// Resolved through the shared helper, under the same STUB_AUTH_URL opt-in
	// buildTokenProvider uses, so the rerun endpoint cannot diverge from the
	// token exchange's (see provisioner.Provisioner.GitHubAPIURL, Q504).
	apiBaseURL, err := githubapp.ResolveAPIBaseURL(cfg.StubAuthURL != "")
	if err != nil {
		return nil, err
	}
	prov.GitHubAPIURL = apiBaseURL

	// Detect worker-pod completion off the shared Pod informer rather than polling
	// per session: one event handler serves every in-flight session, so detection
	// is near-immediate and no per-session ticker is spawned. Run it as a manager
	// Runnable so the handler is registered after the cache syncs.
	podWaiter := provisioner.NewInformerPodWaiter(mgr.GetCache(),
		slog.New(logr.ToSlogHandler(ctrl.Log.WithName("podwaiter"))))
	// Observe pod-creation latency (creation → runner container start) off the same
	// pod events, once per pod, for the headline pod-startup SLO.
	podWaiter.PodCreationLatency = m.PodCreationLatency
	if err := mgr.Add(podWaiter); err != nil {
		return nil, fmt.Errorf("add pod completion watcher: %w", err)
	}
	prov.Waiter = podWaiter

	// Periodically reclaim expired per-run eviction-retry counters so the map does
	// not grow unbounded over the process lifetime (Q141). The TTL is well beyond a
	// realistic run lifetime, so reclamation never refills a live run's retry budget
	// (the Q106 hard-cap invariant).
	if err := mgr.Add(provisioner.NewEvictionSweeper(prov)); err != nil {
		return nil, fmt.Errorf("add eviction-counter sweeper: %w", err)
	}

	// Re-run the runs force-cancelled behind a worker that never started, once the
	// owner can place a worker pod again (Q691). Deferred rather than immediate
	// because such a job was abandoned for want of capacity, and bounded by the same
	// per-run retry budget as every other disruption recovery.
	if err := mgr.Add(provisioner.NewAbandonedRerunSweeper(prov)); err != nil {
		return nil, fmt.Errorf("add abandoned-run re-run sweeper: %w", err)
	}
	return prov, nil
}

// setupUsageSampler builds and registers the worker CPU/memory usage sampler when
// enabled (Q359 Phase 1). It samples worker pods per RunnerSet × container from
// the metrics.k8s.io API and exports right-sizing series
// (docs/operations/worker-rightsizing.md), degrading gracefully at runtime when
// metrics-server is absent. WORKER_USAGE_SAMPLE_INTERVAL=0/off returns a nil
// sampler — a safe no-op source for the RunnerSet reconciler.
func setupUsageSampler(mgr ctrl.Manager, cfg agcConfig, namespace string) (*usage.Sampler, error) {
	usageInterval, usageEnabled, err := workerUsageSampleInterval(cfg.WorkerUsageSampleInterval)
	if err != nil {
		return nil, fmt.Errorf("parse WORKER_USAGE_SAMPLE_INTERVAL: %w", err)
	}
	if !usageEnabled {
		return nil, nil
	}
	mc, err := metricsclient.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("build metrics.k8s.io client: %w", err)
	}
	usageSampler := &usage.Sampler{
		Client:    mgr.GetClient(),
		Lister:    usage.NewClientsetLister(mc),
		Namespace: namespace,
		Interval:  usageInterval,
		Metrics:   usage.NewMetrics(ctrlmetrics.Registry),
		Log:       slog.New(logr.ToSlogHandler(ctrl.Log.WithName("usage"))),
	}
	if err := mgr.Add(usageSampler); err != nil {
		return nil, fmt.Errorf("add worker usage sampler: %w", err)
	}
	return usageSampler, nil
}

// buildScheme builds the AGC client scheme: the core Kubernetes types
// (clientgoscheme, which already includes corev1) plus the agc-group v1alpha1
// RunnerGroup kinds and the actions-gateway.com/v2alpha1 kinds the RunnerSet
// reconciler consumes. Kept as a standalone, test-reachable helper so scheme
// registration is verifiable without standing up a manager.
func buildScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	// clientgoscheme already includes corev1; no need to add it separately.
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	// Register the v2alpha1 (actions-gateway.com) kinds so they are first-class in
	// the AGC's client scheme alongside v1alpha1. M1 wires no reconciler for them;
	// the RunnerSet reconciler that consumes them lands in M3a.
	if err := agcv2alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return scheme, nil
}

// useImageVolume decides the worker-wrapper delivery mechanism (Q235). The
// WRAPPER_DELIVERY override wins ("imagevolume" or "init"); otherwise
// ("auto"/unset) it uses an OCI image volume when the API server is >= 1.33
// (where the ImageVolume feature is beta and on by default), falling back to an
// initContainer below that or when the version cannot be determined.
func useImageVolume(cfg *rest.Config, override string) bool {
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "imagevolume":
		return true
	case "init":
		return false
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		slog.Warn("wrapper delivery: discovery client unavailable; using initContainer", "error", err)
		return false
	}
	v, err := dc.ServerVersion()
	if err != nil {
		slog.Warn("wrapper delivery: server version unavailable; using initContainer", "error", err)
		return false
	}
	major, errMajor := strconv.Atoi(strings.TrimRight(v.Major, "+"))
	minor, errMinor := strconv.Atoi(strings.TrimRight(v.Minor, "+"))
	if errMajor != nil || errMinor != nil {
		slog.Warn("wrapper delivery: unparseable server version; using initContainer", "version", v.GitVersion)
		return false
	}
	useIV := major > 1 || (major == 1 && minor >= 33)
	slog.Info("wrapper delivery resolved", "imageVolume", useIV, "serverVersion", v.GitVersion)
	return useIV
}

// workerUsageSampleInterval parses the WORKER_USAGE_SAMPLE_INTERVAL env value
// into the worker usage sampler's polling period (Q359). Unset/empty selects
// the default (usage.DefaultSampleInterval); "0", "off", "false", or
// "disabled" turns the sampler off; anything else must be a Go duration ≥ 1s
// (sub-second polling only re-reads the same metrics-server sample and loads
// the API server for nothing).
func workerUsageSampleInterval(v string) (interval time.Duration, enabled bool, err error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return usage.DefaultSampleInterval, true, nil
	case "0", "off", "false", "disabled":
		return 0, false, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, false, err
	}
	if d < time.Second {
		return 0, false, fmt.Errorf("interval %s below 1s minimum", d)
	}
	return d, true, nil
}
