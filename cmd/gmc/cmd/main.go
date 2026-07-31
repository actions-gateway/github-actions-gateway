package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	v2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	actionsgatewaygithubcomv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
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
	// Register the v2alpha1 (actions-gateway.com) kinds so they are first-class in the
	// GMC's client scheme alongside v1alpha1. All five v2 kinds (the GMC-group
	// ActionsGateway/EgressProxy and the AGC-group RunnerSet/RunnerTemplate/
	// ClusterRunnerTemplate the GMC serves the validating webhook for) share one neutral
	// api module, so a single AddToScheme registers them all. M2 reconciles EgressProxy;
	// the ActionsGateway/RunnerSet reconcilers that consume the rest land in M3a.
	utilruntime.Must(v2alpha1.AddToScheme(scheme))
	// Register the v2beta1 kinds (Q74): the graduated, ScaleSet-only API and the
	// conversion-hub / storage version. Both v2alpha1 (spoke) and v2beta1 (hub) must be
	// in the scheme for the GMC-hosted conversion webhook to resolve each kind as
	// convertible and serve /convert.
	utilruntime.Must(v2beta1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// main wires the GMC process: it binds flags, resolves and validates the config
// surface, constructs the controller-runtime manager, and registers the
// controllers, webhooks, and health checks before starting the manager. The
// concern-scoped steps live in flags.go (addFlags), config.go (resolveConfig,
// resolveImages, the option builders), and wiring.go (newManager,
// registerControllers, registerWebhooks, setupHealthChecks) so each concern is
// test-reachable (Q367). The ordering here is load-bearing: config validation
// and manager construction precede image resolution and controller/webhook
// registration.
func main() {
	cfg := addFlags(flag.CommandLine)
	// Default to production logging: structured JSON at info level, which log
	// aggregators can parse out of the box. Developers pass --zap-devel for
	// human-readable console logs at debug level when running locally. Keeping JSON
	// the default is the same right-by-default stance the AGC uses.
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	// Bridge log/slog onto the same zap logger so the IPRange reconciler (which logs
	// through log/slog) emits the same structured JSON at the same level as the
	// manager. Without this, slog.Default() is the stdlib TEXT handler and the GMC
	// would emit mixed JSON+text on one stream (k8s audit F1). The named logger
	// injected into the IPRange reconciler inherits this sink; the bridge also
	// catches any unwired slog.Default() call site.
	slog.SetDefault(slog.New(logr.ToSlogHandler(ctrl.Log)))

	tlsOpts := tlsOptions(cfg.enableHTTP2)
	webhookServer := newWebhookServer(cfg, tlsOpts)
	metricsServerOptions := newMetricsServerOptions(cfg, tlsOpts)

	// Fail fast with a clear message on misordered leader-election timings rather
	// than letting controller-runtime surface client-go's terser error deep in
	// manager construction. Only meaningful when leader election is active.
	if cfg.enableLeaderElection {
		if err := validateLeaderElectionTimings(
			cfg.leaderElectLeaseDuration, cfg.leaderElectRenewDeadline, cfg.leaderElectRetryPeriod); err != nil {
			setupLog.Error(err, "invalid leader-election timing flags")
			os.Exit(1)
		}
	}

	// Resolve the cross-flag configuration (allowlists + disjointness check, egress
	// CIDR/FQDN parsing, POD_NAMESPACE, ConfigMap informer scoping). Any malformed
	// value fails closed here, before the manager connects to the API server.
	resolved, err := resolveConfig(cfg, os.Getenv)
	if err != nil {
		setupLog.Error(err, "invalid startup configuration")
		os.Exit(1)
	}

	mgr, err := newManager(cfg, resolved, metricsServerOptions, webhookServer)
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// Resolve the injected image config (AGC_IMAGE/PROXY_IMAGE + digest pinning,
	// AGC_EXTRA_* forwarding, WRAPPER_IMAGE). Kept after manager construction to
	// preserve the original startup ordering.
	images, err := resolveImages(cfg, os.Getenv, os.Environ)
	if err != nil {
		setupLog.Error(err, "invalid image configuration")
		os.Exit(1)
	}

	if err := registerControllers(mgr, cfg, resolved, images); err != nil {
		setupLog.Error(err, "Failed to register controllers")
		os.Exit(1)
	}

	enableWebhooks := os.Getenv("ENABLE_WEBHOOKS") != "false"
	if enableWebhooks {
		if err := registerWebhooks(mgr, resolved); err != nil {
			setupLog.Error(err, "Failed to register webhooks")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder

	if err := setupHealthChecks(mgr, enableWebhooks); err != nil {
		setupLog.Error(err, "Failed to set up health checks")
		os.Exit(1)
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

// parseAllowedEgressCIDRs splits the --allowed-egress-cidrs flag value
// (comma-separated CIDRs) into parsed networks, failing on any malformed entry so a
// typo fails startup rather than silently dropping a guardrail entry (Q242 G.1). An
// empty or whitespace-only value yields a nil slice — the secure default that, with
// an empty --allowed-egress-fqdns, forbids every non-GitHub destination.
func parseAllowedEgressCIDRs(raw string) ([]*net.IPNet, error) {
	var cidrs []*net.IPNet
	for _, part := range strings.Split(raw, ",") {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		_, n, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", entry, err)
		}
		cidrs = append(cidrs, n)
	}
	return cidrs, nil
}

// parseAPIServerCIDRs splits the --apiserver-cidrs flag value (comma-separated
// CIDRs) into a slice, trimming whitespace and dropping empty entries. Each
// remaining entry must parse as a CIDR; a malformed one returns an error so the
// GMC fails closed at startup rather than reconciling a NetworkPolicy the
// apiserver rejects or that silently mis-scopes AGC apiserver egress. An empty
// or whitespace-only value yields a nil slice, which keeps the AGC NetworkPolicy
// apiserver-egress rule any-destination (the secure default for Q145).
func parseAPIServerCIDRs(raw string) ([]string, error) {
	var cidrs []string
	for _, part := range strings.Split(raw, ",") {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(entry); err != nil {
			return nil, fmt.Errorf("invalid apiserver CIDR %q: %w", entry, err)
		}
		cidrs = append(cidrs, entry)
	}
	return cidrs, nil
}

// mustEnv reads the named environment variable via getenv (os.Getenv in
// production; a fake in tests) and returns an error if it is unset or empty.
func mustEnv(getenv func(string) string, name string) (string, error) {
	v := getenv(name)
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
