package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
	actionsgatewaygithubcomv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	webhookv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/v1alpha1"
	"github.com/go-logr/logr"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(actionsgatewaygithubcomv1alpha1.AddToScheme(scheme))
	utilruntime.Must(agcv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	// Leader-election timing knobs. Defaults match controller-runtime/client-go
	// (15s/10s/2s) so existing deployments behave identically. They are exposed
	// so operators can tune the failover/false-positive trade-off per cluster:
	// larger values tolerate slower apiservers (fewer spurious lease losses),
	// smaller values fail over faster (k8s audit F4). The invariant
	// LeaseDuration > RenewDeadline > RetryPeriod×1.2 is validated below.
	var leaderElectLeaseDuration, leaderElectRenewDeadline, leaderElectRetryPeriod time.Duration
	flag.DurationVar(&leaderElectLeaseDuration, "leader-elect-lease-duration", 15*time.Second,
		"Duration non-leader candidates wait before force-acquiring leadership.")
	flag.DurationVar(&leaderElectRenewDeadline, "leader-elect-renew-deadline", 10*time.Second,
		"Duration the acting leader retries refreshing leadership before giving up.")
	flag.DurationVar(&leaderElectRetryPeriod, "leader-elect-retry-period", 2*time.Second,
		"Interval between leader-election action attempts.")
	// ReleaseOnCancel makes the active manager step down voluntarily on SIGTERM
	// instead of holding the lease until it expires, so the standby takes over in
	// ~RetryPeriod rather than ~LeaseDuration. This closes the rollout reconcile
	// gap that terminationGracePeriodSeconds (10s) < LeaseDuration (15s) would
	// otherwise leave (k8s audit F3). Safe here: main() exits immediately once
	// mgr.Start returns, with no post-stop cleanup that could race the release.
	var leaderElectReleaseOnCancel bool
	flag.BoolVar(&leaderElectReleaseOnCancel, "leader-elect-release-on-cancel", true,
		"Release the leader lease on graceful shutdown for faster failover. "+
			"Only safe when the process exits promptly after the manager stops.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	var allowAgcExtraEnv bool
	flag.BoolVar(&allowAgcExtraEnv, "allow-agc-extra-env", false,
		"Forward AGC_EXTRA_* environment variables from the GMC pod to AGC Deployments. Intended for testing only.")
	var allowFloatingImageTags bool
	flag.BoolVar(&allowFloatingImageTags, "allow-floating-image-tags", false,
		"Permit non-digest-pinned AGC_IMAGE/PROXY_IMAGE references (floating tags). "+
			"Intended for dev/test only; production requires the name@sha256:<digest> form.")
	var enableTenantServiceMonitors bool
	flag.BoolVar(&enableTenantServiceMonitors, "enable-tenant-service-monitors", false,
		"Create a per-tenant Prometheus-Operator ServiceMonitor for the proxy and AGC mTLS "+
			"metrics ports (:8443). Off by default: requires the monitoring.coreos.com CRD "+
			"(Prometheus Operator) installed. When off, the metrics Services are still created "+
			"but nothing is wired to scrape them. The chart sets this from metrics.serviceMonitor.enabled.")
	var allowedPriorityClasses string
	flag.StringVar(&allowedPriorityClasses, "allowed-priority-classes", "",
		"Comma-separated allowlist of cluster-scoped PriorityClass names that tenant "+
			"RunnerGroups may reference in priorityTiers. The platform admin pre-creates "+
			"these classes and lists them here; the admission webhook rejects any other "+
			"name so a tenant cannot preempt other tenants' worker pods. Empty (default) "+
			"forbids all priorityTiers PriorityClass references.")
	// Default to production logging: structured JSON at info level, which log
	// aggregators can parse out of the box. Developers pass --zap-devel for
	// human-readable console logs at debug level when running locally. Keeping
	// JSON the default (rather than an opt-in flag the deploy must remember to
	// set) is the same right-by-default stance the AGC uses (cmd/agc/main.go).
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	// Bridge log/slog onto the same zap logger so the IPRange reconciler (which
	// logs through log/slog) emits the same structured JSON at the same level
	// source as the manager. Without this, slog.Default() is the stdlib TEXT
	// handler and the GMC would emit mixed JSON+text on one stream (k8s audit
	// F1). The named logger injected into the IPRange reconciler below inherits
	// this sink; the bridge also catches any unwired slog.Default() call site.
	slog.SetDefault(slog.New(logr.ToSlogHandler(ctrl.Log)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled by the chart's metrics.enabled value (default
	// true), which renders the --metrics-bind-address flag. The Metrics options
	// configure the server. More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The metrics-auth RBAC is shipped by the
		// chart (charts/actions-gateway/templates/rbac.yaml). More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	// Fail fast with a clear message on misordered leader-election timings rather
	// than letting controller-runtime surface client-go's terser error deep in
	// manager construction. Only meaningful when leader election is active.
	if enableLeaderElection {
		if err := validateLeaderElectionTimings(
			leaderElectLeaseDuration, leaderElectRenewDeadline, leaderElectRetryPeriod); err != nil {
			setupLog.Error(err, "invalid leader-election timing flags")
			os.Exit(1)
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "actions-gateway-gmc-leader",
		LeaseDuration:          &leaderElectLeaseDuration,
		RenewDeadline:          &leaderElectRenewDeadline,
		RetryPeriod:            &leaderElectRetryPeriod,
		// Two-layer Secret isolation:
		// 1. WatchesMetadata on the controller registers a metadata-only informer
		//    for Secrets (ObjectMeta only — no .data ever enters the cache), so
		//    the controller gets event-driven reconciliation on credential Secret
		//    create/delete without buffering secret material in memory.
		// 2. DisableFor here ensures r.Get() calls bypass the cache entirely and
		//    hit the API server directly, so the actual Secret contents are always
		//    read fresh and never persist in-process after the call returns.
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
		// LeaderElectionReleaseOnCancel makes the leader step down voluntarily when
		// the Manager ends, so the standby takes over in ~RetryPeriod instead of
		// waiting out LeaseDuration. Defaults to true (--leader-elect-release-on-cancel);
		// safe because main() exits immediately after mgr.Start returns, with no
		// post-stop cleanup that could race the lease release (k8s audit F3).
		LeaderElectionReleaseOnCancel: leaderElectReleaseOnCancel,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	agcImage, err := mustEnv("AGC_IMAGE")
	if err != nil {
		setupLog.Error(err, "missing required environment variable")
		os.Exit(1)
	}
	proxyImage, err := mustEnv("PROXY_IMAGE")
	if err != nil {
		setupLog.Error(err, "missing required environment variable")
		os.Exit(1)
	}

	// Require AGC_IMAGE/PROXY_IMAGE to be pinned by sha256 digest so a mutated
	// tag cannot silently swap the AGC or proxy code that runs inside a tenant's
	// gateway (supply-chain hardening; security plan M-19 follow-up). Dev/test
	// uses floating tags, so an explicit --allow-floating-image-tags opt-out is
	// offered — but the secure form stays the default.
	if allowFloatingImageTags {
		setupLog.Info("WARNING: --allow-floating-image-tags is set; AGC_IMAGE/PROXY_IMAGE digest pinning is NOT enforced (do not use in production)")
	} else {
		for _, img := range []struct{ name, ref string }{
			{"AGC_IMAGE", agcImage},
			{"PROXY_IMAGE", proxyImage},
		} {
			if err := validateImageDigest(img.name, img.ref); err != nil {
				setupLog.Error(err, "image reference is not digest-pinned")
				os.Exit(1)
			}
		}
	}

	// Bounded client for the GitHub meta IP-range fetch (Q138): an overall 60s
	// deadline plus a transport ResponseHeaderTimeout, so a slow api.github.com
	// cannot wedge the reconcile.
	httpClient := httpx.NewClientWithTimeout(60 * time.Second)

	// AGC_EXTRA_<NAME>=<VALUE> env vars on the GMC pod are forwarded verbatim to
	// each AGC Deployment the controller creates. Gate-flagged to prevent
	// accidental capability escalation in production deployments.
	var agcExtraEnv []corev1.EnvVar
	if allowAgcExtraEnv {
		for _, kv := range os.Environ() {
			const prefix = "AGC_EXTRA_"
			if !strings.HasPrefix(kv, prefix) {
				continue
			}
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) == 2 {
				agcExtraEnv = append(agcExtraEnv, corev1.EnvVar{
					Name:  strings.TrimPrefix(parts[0], prefix),
					Value: parts[1],
				})
			}
		}
	}

	// IPRangeCache is shared between the per-CR reconciler (read path) and
	// the periodic IPRangeReconciler (write path). This keeps the per-CR
	// reconcile from doing network I/O — previously every reconcile fetched
	// api.github.com/meta, serialised behind MaxConcurrentReconciles=1, and
	// could stall the queue when the API was slow.
	ipCache := &controller.IPRangeCache{}

	// Register the GMC's custom metrics. The managed-gateways collector lists
	// ActionsGateway CRs from the manager cache at scrape time, so it must be
	// given a cached reader.
	gmcMetrics := controller.NewMetrics(mgr.GetClient())

	if err := (&controller.ActionsGatewayReconciler{
		Client:                      mgr.GetClient(),
		Scheme:                      mgr.GetScheme(),
		IPCache:                     ipCache,
		AGCImage:                    agcImage,
		ProxyImage:                  proxyImage,
		AGCExtraEnv:                 agcExtraEnv,
		EnableTenantServiceMonitors: enableTenantServiceMonitors,
		Recorder:                    mgr.GetEventRecorder("actionsgateway-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "actionsgateway")
		os.Exit(1)
	}

	ipInterval := 24 * time.Hour
	if err := mgr.Add(&controller.IPRangeReconciler{
		Client:   mgr.GetClient(),
		Fetcher:  &controller.HTTPGitHubIPRangeFetcher{Client: httpClient},
		Cache:    ipCache,
		Interval: ipInterval,
		Log:      slog.New(logr.ToSlogHandler(ctrl.Log.WithName("ipranges"))),
		Metrics:  gmcMetrics,
	}); err != nil {
		setupLog.Error(err, "Failed to register IP range reconciler")
		os.Exit(1)
	}
	// nolint:goconst
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhookv1alpha1.SetupActionsGatewayWebhookWithManager(mgr, parseAllowedPriorityClasses(allowedPriorityClasses)); err != nil {
			setupLog.Error(err, "Failed to create webhook", "webhook", "ActionsGateway")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}
	// Gate readiness on the webhook server actually listening, not just on the
	// manager being up. Without this, a fresh GMC pod is briefly added to the
	// gmc-webhook-service endpoints before the admission webhook port is bound,
	// so the apiserver routes admission calls to a not-yet-serving pod and
	// every dependent `kubectl apply` times out for ~1s during each rollout.
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
			setupLog.Error(err, "Failed to set up webhook ready check")
			os.Exit(1)
		}
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// parseAllowedPriorityClasses splits the --allowed-priority-classes flag value
// (comma-separated PriorityClass names) into a slice, trimming whitespace and
// dropping empty entries. An empty or whitespace-only value yields a nil slice,
// which the validator treats as "no class permitted" (secure default).
func parseAllowedPriorityClasses(raw string) []string {
	var names []string
	for _, part := range strings.Split(raw, ",") {
		if name := strings.TrimSpace(part); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func mustEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("required environment variable %s is not set", name)
	}
	return v, nil
}

// leaderElectionJitterFactor mirrors client-go's leaderelection.JitterFactor:
// the renew deadline must exceed retryPeriod×JitterFactor or client-go rejects
// the config at manager construction. Kept in sync with the vendored constant.
const leaderElectionJitterFactor = 1.2

// validateLeaderElectionTimings enforces the same invariants client-go's
// leaderelection applies (LeaseDuration > RenewDeadline > RetryPeriod×1.2, all
// positive), but surfaces them at flag-parse time with an actionable message
// instead of as a terse error buried in manager construction. Only relevant
// when leader election is enabled.
func validateLeaderElectionTimings(lease, renew, retry time.Duration) error {
	if lease <= 0 || renew <= 0 || retry <= 0 {
		return fmt.Errorf("leader-election durations must be positive: "+
			"lease=%s renew=%s retry=%s", lease, renew, retry)
	}
	if lease <= renew {
		return fmt.Errorf("--leader-elect-lease-duration (%s) must be greater than "+
			"--leader-elect-renew-deadline (%s)", lease, renew)
	}
	if minRenew := time.Duration(leaderElectionJitterFactor * float64(retry)); renew <= minRenew {
		return fmt.Errorf("--leader-elect-renew-deadline (%s) must be greater than "+
			"--leader-elect-retry-period×%.1f (%s)", renew, leaderElectionJitterFactor, minRenew)
	}
	return nil
}

// digestPinnedRE matches an image reference pinned by a sha256 digest, e.g.
// "ghcr.io/org/agc:v1@sha256:<64 lowercase hex>". The digest is the trailing
// component of the reference, so the match is anchored to the end of the string.
var digestPinnedRE = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// validateImageDigest returns an error unless ref is pinned by a sha256 digest.
// Floating tags let a registry serve different bytes for the same reference, so
// the GMC rejects them by default for the images it injects into tenant
// gateways (overridable with --allow-floating-image-tags for dev/test).
func validateImageDigest(name, ref string) error {
	if !digestPinnedRE.MatchString(ref) {
		return fmt.Errorf("%s=%q is not digest-pinned; expected the form name@sha256:<64 hex digits> "+
			"(pass --allow-floating-image-tags to bypass in dev/test)", name, ref)
	}
	return nil
}
