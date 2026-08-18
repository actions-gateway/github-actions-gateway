//go:build integration

package integration_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	agcnames "github.com/actions-gateway/github-actions-gateway/agc/names"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	v2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/logtest"
	webhookv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/v1alpha1"
	webhookv2alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/v2alpha1"
	webhookv2beta1 "github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/v2beta1"
	gmcnames "github.com/actions-gateway/github-actions-gateway/gmc/names"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/yaml"
)

// Shared resource name constants — single source of truth for all integration
// tests in this package. Import the canonical constants rather than repeating
// string literals so that a rename propagates automatically.
const (
	agcName      = agcnames.ControllerName
	workerSAName = agcnames.WorkerSAName
	proxyName    = gmcnames.ProxyName
	workloadName = gmcnames.WorkloadNetworkPolicyName
)

var (
	testEnv       *envtest.Environment
	k8sClient     client.Client
	testScheme    *runtime.Scheme
	ctx           context.Context
	cancel        context.CancelFunc
	webhookCancel context.CancelFunc
)

// egressTestAllowlist is the platform egress allowlist (Q242 G.1) the suite's
// EgressProxy validating webhook is wired to. Its static set is deliberately broad
// enough to permit every destination the reconciler tests request — golang.org
// covers proxy.golang.org; the CIDRs cover the requested 10.x and 199.36.153.x/30
// subnets — so those tests are not blocked by admission. The dedicated admission
// test (v2_egressproxy_admission_test.go) asserts rejection of destinations outside
// this set.
var egressTestAllowlist = allowlist.NewEgressDestination(
	[]string{"golang.org"},
	mustParseCIDRs("10.0.0.0/8", "199.36.153.0/24"),
)

// infraTestAllowlist is the infra-only PriorityClass allowlist (Q284) the suite wires
// into the EgressProxy and v2 ActionsGateway webhooks. It names one class the dedicated
// scheduling admission test may reference; off-allowlist rejection is asserted against
// this same set. Disjoint from the worker allowlist ("high") the RunnerSet webhook uses,
// mirroring the production disjointness invariant.
var infraTestAllowlist = allowlist.New([]string{"gag-infra-critical"})

// mustParseCIDRs parses CIDR strings for suite setup, panicking on a bad entry.
func mustParseCIDRs(ss ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(ss))
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			panic(fmt.Sprintf("parse CIDR %q: %v", s, err))
		}
		out = append(out, n)
	}
	return out
}

func TestMain(m *testing.M) {
	// Fulfill controller-runtime's root logger first. This suite always outlives
	// the 30-second mark at which an unfulfilled root logger dumps a goroutine
	// stack to stderr mid-run (Q455); see package logtest.
	logtest.Install()

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()

	testScheme = runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(testScheme)
	_ = agcv1alpha1.AddToScheme(testScheme)
	_ = gmcv1alpha1.AddToScheme(testScheme)
	_ = v2alpha1.AddToScheme(testScheme)
	// Register the v2beta1 hub so envtest's modifyConversionWebhooks recognizes the
	// five v2 kinds as convertible and redirects their CRD conversion to the local
	// webhook server (Q74). Both spoke and hub must be in the scheme for conversion.
	_ = v2beta1.AddToScheme(testScheme)

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			"../../../config/crd/bases",
			// The two out-of-module CRD sets are reached through testdata/ symlinks so
			// the reads resolve inside this module root and go keeps them as test-cache
			// inputs (Q902).
			"testdata/agc-crd",
			// The v2 (actions-gateway.com) CRDs live in the neutral api module —
			// the five tenant kinds plus the cluster-scoped PriorityClassAllowlist
			// the priorityclass-allowlist-guard policy uses as its paramKind (Q492).
			"testdata/crd",
			// Stub Cilium/Calico CRDs (Q208) so the FQDN-mode unstructured apply lands
			// against the test apiserver. Minimal preserve-unknown-fields schemas — real
			// clusters install the CNI's own CRDs.
			"testdata/cni-crds",
			// Stub monitoring.coreos.com ServiceMonitor CRD (Q324) so the tenant
			// ServiceMonitor apply lands against the test apiserver. Real clusters
			// install the Prometheus Operator's own CRD.
			"testdata/monitoring-crds",
			// Stub autoscaling.k8s.io VerticalPodAutoscaler CRD (Q360) so the managed
			// AGC autoscaler apply lands against the test apiserver. Real clusters
			// install the Kubernetes vertical-pod-autoscaler's own CRD. The
			// CRD-ABSENT half of the opt-in is unit-tested (a RESTMapper that matches
			// no kind) — it cannot be expressed here once the stub is installed.
			"testdata/autoscaling-crds",
		},
		ErrorIfCRDPathMissing: true,
		Scheme:                testScheme,
		// Install the GMC validating webhook into the test apiserver so admission
		// is exercised end-to-end (apiserver -> webhook -> CA), not just via direct
		// validator calls. envtest allocates a serving host/port + cert dir and
		// patches the CABundle into the ValidatingWebhookConfiguration on Start().
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{filepath.Join("..", "..", "..", "config", "webhook")},
		},
	}

	cfg, err := testEnv.Start()
	if err != nil {
		panic(err)
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		panic(err)
	}

	// Install the agc-tenant-role ClusterRole. In production this ships with
	// the Helm chart (charts/actions-gateway/templates/agc-tenant-role.yaml); in
	// envtest we install it programmatically so per-tenant RoleBindings can
	// actually grant their referenced permissions to impersonated SAs.
	if err := installAGCTenantClusterRole(ctx, k8sClient); err != nil {
		panic(err)
	}

	// Start the validating webhook server and block until it is actually serving.
	// The ValidatingWebhookConfiguration uses failurePolicy=Fail, so every
	// actionsgateways create/update in the suite is routed through this server —
	// if it is not ready first, those creates fail with a connection error that
	// looks like a rejection.
	if err := startValidatingWebhook(); err != nil {
		panic(err)
	}

	exitCode := m.Run()
	if webhookCancel != nil {
		webhookCancel()
	}
	_ = testEnv.Stop()
	cancel()
	os.Exit(exitCode)
}

// startValidatingWebhook starts a manager that serves only the ActionsGateway
// validating webhook against the envtest apiserver, then blocks until the
// webhook is reachable. The manager is mirrored on the production wiring
// (SetupActionsGatewayWebhookWithManager): no POD_NAMESPACE override (the
// defaults reserve kube-system/kube-public/gmc-system) and an empty
// PriorityClass allowlist (secure default — no integration ActionsGateway
// references a priorityTier).
func startValidatingWebhook() error {
	opts := testEnv.WebhookInstallOptions
	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    opts.LocalServingHost,
			Port:    opts.LocalServingPort,
			CertDir: opts.LocalServingCertDir,
		}),
	})
	if err != nil {
		return fmt.Errorf("create webhook manager: %w", err)
	}
	if err := webhookv1alpha1.SetupActionsGatewayWebhookWithManager(mgr, nil); err != nil {
		return fmt.Errorf("register validating webhook: %w", err)
	}
	// The envtest WebhookInstallOptions install the whole ValidatingWebhookConfiguration
	// (failurePolicy=Fail), including the v2 RunnerTemplate/ClusterRunnerTemplate
	// webhooks, so the same server must serve their paths or every runnertemplate
	// create in the suite would fail with a connection error.
	// A nil PriorityClass allowlist is the secure default (no named class permitted),
	// matching the v1 registration above. Suite templates name no PriorityClass; the
	// Q289 gate is exercised against a live, ConfigMap-backed allowlist in
	// priorityclass_allowlist_test.go.
	if err := webhookv2alpha1.SetupRunnerTemplateWebhooksWithManager(mgr, nil); err != nil {
		return fmt.Errorf("register RunnerTemplate webhooks: %w", err)
	}
	// The same ValidatingWebhookConfiguration also carries the EgressProxy webhook
	// (Q242 G.1, failurePolicy=Fail), so this server must serve its path or every
	// EgressProxy create in the suite would fail closed. It is wired to a fixed
	// static allowlist (egressTestAllowlist) broad enough to permit the destinations
	// the reconciler tests request; the dedicated admission test asserts off-allowlist
	// rejection against the same set. The FQDN backend is cilium (Q245) so an FQDN-intent
	// EgressProxy is admitted here (backend != none); the FQDN+none admission rejection is
	// covered by the webhook unit test, which can vary the backend freely.
	// Q284: also gate spec.scheduling.priorityClassName against the infra-only
	// allowlist. Wired to infraTestAllowlist so the dedicated scheduling admission test
	// can exercise both allow and deny against a known set.
	if err := webhookv2alpha1.SetupEgressProxyWebhookWithManager(mgr, egressTestAllowlist, controller.FQDNBackendCilium, infraTestAllowlist); err != nil {
		return fmt.Errorf("register EgressProxy webhook: %w", err)
	}
	// Q284: the new v2 ActionsGateway validating webhook. The installed
	// ValidatingWebhookConfiguration now carries vactionsgateway-v2alpha1.kb.io
	// (failurePolicy=Fail), so this server MUST serve its path or every v2
	// ActionsGateway create in the suite would fail closed. Gates
	// spec.scheduling.priorityClassName against the same infra allowlist.
	if err := webhookv2alpha1.SetupActionsGatewayWebhookWithManager(mgr, infraTestAllowlist); err != nil {
		return fmt.Errorf("register v2 ActionsGateway webhook: %w", err)
	}
	// The same ValidatingWebhookConfiguration also carries the RunnerSet webhook
	// (Q264 P3, failurePolicy=Fail), which enforces ScaleSet runnerLabel uniqueness
	// and gates priorityTiers against the PriorityClass allowlist (Q289); this
	// server must serve its path or every RunnerSet create in the suite would fail
	// closed. The static allowlist carries only the tier class the representative
	// migration fixture names (v2_migration_test.go): in production a migrated
	// tenant's tier class is necessarily already allowlisted, or the v1 gateway
	// naming it could never have been admitted. The gate's own semantics — including
	// the nil/secure default — are exercised in priorityclass_allowlist_test.go and
	// the webhook unit tests.
	if err := webhookv2alpha1.SetupRunnerSetWebhookWithManager(mgr, allowlist.New([]string{"high"})); err != nil {
		return fmt.Errorf("register RunnerSet webhook: %w", err)
	}
	// Q74: serve /convert for the five v2 hub kinds. envtest patches each convertible
	// CRD's spec.conversion to point at this same server, so a v2beta1<->v2alpha1
	// read/write round-trips through the real conversion path, not a fake client.
	if err := webhookv2beta1.SetupConversionWebhooksWithManager(mgr); err != nil {
		return fmt.Errorf("register conversion webhooks: %w", err)
	}

	var mgrCtx context.Context
	mgrCtx, webhookCancel = context.WithCancel(ctx)
	go func() { _ = mgr.Start(mgrCtx) }()

	return waitForWebhookReady(opts)
}

// waitForWebhookReady blocks until the validating webhook is serving and the
// apiserver can reach it. It first waits for the TLS listener to accept
// connections, then proves the full admission path end-to-end: with
// failurePolicy=Fail the apiserver rejects every actionsgateways create until
// it can both reach the webhook and trust its CA, so a known-good create that
// succeeds is the definitive readiness signal. Asserting readiness here (rather
// than retrying inside individual tests) keeps the per-test rejection
// assertions unambiguous: an error then means the webhook denied the request,
// not that it was not yet listening.
func waitForWebhookReady(opts envtest.WebhookInstallOptions) error {
	addr := net.JoinHostPort(opts.LocalServingHost, strconv.Itoa(opts.LocalServingPort))
	dialErr := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true,
		func(context.Context) (bool, error) {
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", addr,
				&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // local envtest serving cert, identity is irrelevant
			if err != nil {
				return false, nil
			}
			_ = conn.Close()
			return true, nil
		})
	if dialErr != nil {
		return fmt.Errorf("webhook TLS listener never came up at %s: %w", addr, dialErr)
	}

	const readinessNS = "gmc-webhook-readiness"
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: readinessNS}}
	if err := k8sClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create readiness namespace: %w", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), ns) }()

	readyErr := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true,
		func(context.Context) (bool, error) {
			probe := newActionsGateway("webhook-readiness-probe", readinessNS, "github-app")
			err := k8sClient.Create(ctx, probe)
			switch {
			case err == nil:
				_ = k8sClient.Delete(ctx, probe)
				return true, nil
			case apierrors.IsInvalid(err):
				// CRD/webhook validation reached us but rejected the probe object —
				// the path works; fail loudly rather than spin forever.
				return false, fmt.Errorf("readiness probe rejected as invalid: %w", err)
			default:
				// Webhook not reachable yet (connection refused / no endpoints) —
				// keep polling.
				return false, nil
			}
		})
	if readyErr != nil {
		return fmt.Errorf("validating webhook never became ready: %w", readyErr)
	}
	return nil
}

// gmcReconcilerOptions tunes startGMCReconcilerWithOptions for tests that need a
// shared IP-range cache they can manipulate (egress-staleness, Q157) or a custom
// EgressRulesStale threshold. The zero value reproduces startGMCReconciler.
type gmcReconcilerOptions struct {
	// ipCache, when non-nil, is shared with the reconciler so the test can stamp
	// LastRefresh directly. Nil allocates a fresh cache.
	ipCache *controller.IPRangeCache
	// egressThreshold overrides ActionsGatewayReconciler.EgressStaleThreshold; 0
	// leaves the reconciler's package default.
	egressThreshold time.Duration
}

// startGMCReconciler starts an ActionsGatewayReconciler for the duration of a test.
// Returns the IPRangeReconciler so tests that need to trigger manual reconciles can do so.
func startGMCReconciler(t *testing.T, ipFetcher controller.GitHubIPRangeFetcher) *controller.IPRangeReconciler {
	t.Helper()
	return startGMCReconcilerWithOptions(t, ipFetcher, gmcReconcilerOptions{})
}

// startGMCReconcilerWithOptions is startGMCReconciler with explicit knobs (see
// gmcReconcilerOptions).
func startGMCReconcilerWithOptions(t *testing.T, ipFetcher controller.GitHubIPRangeFetcher, opts gmcReconcilerOptions) *controller.IPRangeReconciler {
	t.Helper()
	mgrCtx, mgrCancel := context.WithCancel(ctx)
	t.Cleanup(mgrCancel)

	skipNameValidation := true
	syncPeriod := 2 * time.Second
	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		// Short sync period ensures the controller re-reconciles objects even when
		// no watch event fires (e.g. after a Secret referenced by an ActionsGateway
		// is deleted — no informer maps Secret deletions to ActionsGateway reconciles).
		Cache: cache.Options{SyncPeriod: &syncPeriod},
	})
	require.NoError(t, err)

	if ipFetcher == nil {
		ipFetcher = &stubIPFetcher{cidrs: []net.IPNet{}}
	}

	// Shared cache between the per-CR reconciler (reads) and the periodic
	// IPRangeReconciler (writes). Pre-populated so tests that assert on
	// proxy-NetworkPolicy CIDRs see them immediately on the very first
	// reconcile, mirroring the steady-state production behavior where
	// IPRangeReconciler's startup fetch has already run.
	ipCache := opts.ipCache
	if ipCache == nil {
		ipCache = &controller.IPRangeCache{}
	}
	if cidrs, fetchErr := ipFetcher.FetchIPRanges(ctx); fetchErr == nil {
		ipCache.Set(cidrs)
	}

	err = (&controller.ActionsGatewayReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		IPCache:              ipCache,
		AGCImage:             "agc:test",
		ProxyImage:           "proxy:test",
		EgressStaleThreshold: opts.egressThreshold,
	}).SetupWithManager(mgr)
	require.NoError(t, err)

	ipRangeReconciler := &controller.IPRangeReconciler{
		Client:  mgr.GetClient(),
		Fetcher: ipFetcher,
		Cache:   ipCache,
	}

	go func() {
		_ = mgr.Start(mgrCtx)
	}()

	return ipRangeReconciler
}

// agcTenantRoleRulesPath is the Helm chart fragment that single-sources the
// agc-tenant-role permission rules (Q143). The chart embeds it via .Files.Get in
// templates/agc-tenant-role.yaml; this suite reads the SAME file so the role
// granted in envtest is byte-identical to the one production ships — the
// RBAC-scope test can never silently drift from the deployed permission set.
// Reached through a testdata/ symlink for the same reason as the CRD paths above.
var agcTenantRoleRulesPath = filepath.Join(
	"testdata", "chartfiles", "agc-tenant-role-rules.yaml",
)

// installAGCTenantClusterRole installs the agc-tenant-role ClusterRole the Helm
// chart ships (charts/actions-gateway/templates/agc-tenant-role.yaml), loading
// its rules from the chart's single-source fragment (agcTenantRoleRulesPath).
// The production install applies it once at GMC install time; envtest needs the
// same singleton for per-tenant RoleBindings to grant any permission.
func installAGCTenantClusterRole(ctx context.Context, c client.Client) error {
	data, err := os.ReadFile(agcTenantRoleRulesPath)
	if err != nil {
		return fmt.Errorf("read agc-tenant-role rules fragment %s: %w", agcTenantRoleRulesPath, err)
	}
	var rules []rbacv1.PolicyRule
	if err := yaml.Unmarshal(data, &rules); err != nil {
		return fmt.Errorf("parse agc-tenant-role rules fragment %s: %w", agcTenantRoleRulesPath, err)
	}
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "agc-tenant-role"},
		Rules:      rules,
	}
	if err := c.Create(ctx, cr); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// startEgressProxyReconciler starts an EgressProxyReconciler for the duration of a
// test against the envtest apiserver. The optional ipCache is shared so a test can
// stamp GitHub CIDRs the proxy NetworkPolicy should pick up; a nil cache allocates a
// fresh (empty) one, which the reconciler tolerates. The FQDN backend is none — enough
// for the deprecated CiliumFQDN/CalicoFQDN modes (which pin their own backend) and the
// CIDR default; use startEgressProxyReconcilerWithBackend for FQDN-intent tests.
func startEgressProxyReconciler(t *testing.T, ipCache *controller.IPRangeCache) {
	t.Helper()
	startEgressProxyReconcilerWithBackend(t, ipCache, controller.FQDNBackendNone)
}

// startEgressProxyReconcilerWithServiceMonitor is startEgressProxyReconciler with the
// tenant ServiceMonitor toggle on (Q324), so a test can prove the per-EgressProxy
// ServiceMonitor is created against the stub monitoring.coreos.com CRD.
func startEgressProxyReconcilerWithServiceMonitor(t *testing.T, ipCache *controller.IPRangeCache) {
	t.Helper()
	syncPeriod := 2 * time.Second
	startEgressProxyReconcilerFull(t, ipCache, controller.FQDNBackendNone, &syncPeriod, true)
}

// startEgressProxyReconcilerWithBackend is startEgressProxyReconciler with an explicit
// FQDN egress backend (Q245), so a test can drive the FQDN intent through the operator's
// chosen mechanism (cilium/calico/gke).
func startEgressProxyReconcilerWithBackend(t *testing.T, ipCache *controller.IPRangeCache, backend controller.FQDNBackend) {
	t.Helper()
	syncPeriod := 2 * time.Second
	startEgressProxyReconcilerOpts(t, ipCache, backend, &syncPeriod)
}

// startEgressProxyReconcilerNoResync is startEgressProxyReconciler with the manager's
// default (effectively infinite) cache sync period instead of the suite's 2s resync,
// so a test can prove a reconcile was triggered by a watch event rather than by the
// periodic resync re-enqueueing every EgressProxy (Q326).
func startEgressProxyReconcilerNoResync(t *testing.T, ipCache *controller.IPRangeCache) {
	t.Helper()
	startEgressProxyReconcilerOpts(t, ipCache, controller.FQDNBackendNone, nil)
}

// startEgressProxyReconcilerOpts is the shared core: a nil syncPeriod keeps the
// manager's default resync behavior.
func startEgressProxyReconcilerOpts(t *testing.T, ipCache *controller.IPRangeCache, backend controller.FQDNBackend, syncPeriod *time.Duration) {
	t.Helper()
	startEgressProxyReconcilerFull(t, ipCache, backend, syncPeriod, false)
}

// startEgressProxyReconcilerFull is the underlying constructor; enableServiceMonitor
// toggles the per-EgressProxy ServiceMonitor provisioning (Q324).
func startEgressProxyReconcilerFull(t *testing.T, ipCache *controller.IPRangeCache, backend controller.FQDNBackend, syncPeriod *time.Duration, enableServiceMonitor bool) {
	t.Helper()
	mgrCtx, mgrCancel := context.WithCancel(ctx)
	t.Cleanup(mgrCancel)

	skipNameValidation := true
	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		Cache:                  cache.Options{SyncPeriod: syncPeriod},
	})
	require.NoError(t, err)

	if ipCache == nil {
		ipCache = &controller.IPRangeCache{}
	}

	err = (&controller.EgressProxyReconciler{
		Client:               mgr.GetClient(),
		APIReader:            mgr.GetAPIReader(),
		Scheme:               mgr.GetScheme(),
		IPCache:              ipCache,
		ProxyImage:           "proxy:test",
		FQDNBackend:          backend,
		EnableServiceMonitor: enableServiceMonitor,
		Recorder:             mgr.GetEventRecorder("egressproxy-controller"),
	}).SetupWithManager(mgr)
	require.NoError(t, err)

	go func() { _ = mgr.Start(mgrCtx) }()
}

type stubIPFetcher struct {
	mu    sync.Mutex
	cidrs []net.IPNet
}

func (f *stubIPFetcher) FetchIPRanges(_ context.Context) ([]net.IPNet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cidrs, nil
}

func (f *stubIPFetcher) SetCIDRs(cidrs []net.IPNet) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cidrs = cidrs
}
