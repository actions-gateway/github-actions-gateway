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
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	webhookv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/v1alpha1"
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

func TestMain(m *testing.M) {
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()

	testScheme = runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(testScheme)
	_ = agcv1alpha1.AddToScheme(testScheme)
	_ = gmcv1alpha1.AddToScheme(testScheme)

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			"../../../config/crd/bases",
			"../../../../agc/config/crd",
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

// startGMCReconciler starts an ActionsGatewayReconciler for the duration of a test.
// Returns the IPRangeReconciler so tests that need to trigger manual reconciles can do so.
func startGMCReconciler(t *testing.T, ipFetcher controller.GitHubIPRangeFetcher) *controller.IPRangeReconciler {
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
	ipCache := &controller.IPRangeCache{}
	if cidrs, fetchErr := ipFetcher.FetchIPRanges(ctx); fetchErr == nil {
		ipCache.Set(cidrs)
	}

	err = (&controller.ActionsGatewayReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		IPCache:    ipCache,
		AGCImage:   "agc:test",
		ProxyImage: "proxy:test",
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

// installAGCTenantClusterRole mirrors the agc-tenant-role ClusterRole the Helm
// chart ships (charts/actions-gateway/templates/agc-tenant-role.yaml). The
// production install applies it once at GMC install time; envtest needs the
// same singleton for per-tenant RoleBindings to grant any permission.
func installAGCTenantClusterRole(ctx context.Context, c client.Client) error {
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "agc-tenant-role"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch", "create", "delete"}},
			{APIGroups: []string{""}, Resources: []string{"pods/status"}, Verbs: []string{"get"}},
			{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list", "watch", "create", "delete"}},
			{APIGroups: []string{"actions-gateway.github.com"}, Resources: []string{"runnergroups"}, Verbs: []string{"get", "list", "watch", "update", "patch"}},
			{APIGroups: []string{"actions-gateway.github.com"}, Resources: []string{"runnergroups/status", "runnergroups/finalizers"}, Verbs: []string{"get", "update", "patch"}},
		},
	}
	if err := c.Create(ctx, cr); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
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
