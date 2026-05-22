//go:build integration

package integration_test

import (
	"context"
	"net"
	"os"
	"sync"
	"testing"

	agcv1alpha1 "github.com/karlkfi/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/karlkfi/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/karlkfi/github-actions-gateway/gmc/internal/controller"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var (
	testEnv    *envtest.Environment
	k8sClient  client.Client
	testScheme *runtime.Scheme
	ctx        context.Context
	cancel     context.CancelFunc
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
	}

	cfg, err := testEnv.Start()
	if err != nil {
		panic(err)
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		panic(err)
	}

	exitCode := m.Run()
	_ = testEnv.Stop()
	cancel()
	os.Exit(exitCode)
}

// startGMCReconciler starts an ActionsGatewayReconciler for the duration of a test.
// Returns the IPRangeReconciler so tests that need to trigger manual reconciles can do so.
func startGMCReconciler(t *testing.T, ipFetcher controller.GitHubIPRangeFetcher) *controller.IPRangeReconciler {
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
	})
	require.NoError(t, err)

	if ipFetcher == nil {
		ipFetcher = &stubIPFetcher{cidrs: []net.IPNet{}}
	}

	err = (&controller.ActionsGatewayReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		IPFetcher:  ipFetcher,
		AGCImage:   "agc:test",
		ProxyImage: "proxy:test",
	}).SetupWithManager(mgr)
	require.NoError(t, err)

	ipRangeReconciler := &controller.IPRangeReconciler{
		Client:  mgr.GetClient(),
		Fetcher: ipFetcher,
	}

	go func() {
		_ = mgr.Start(mgrCtx)
	}()

	return ipRangeReconciler
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
