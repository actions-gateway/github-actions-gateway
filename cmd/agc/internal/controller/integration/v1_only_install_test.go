//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/token"
	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// TestV1OnlyInstall_AGCComesUpClean is the acceptance test for Q261: an AGC
// pointed at a cluster with ONLY the v1alpha1 CRDs installed — the main
// actions-gateway chart WITHOUT the opt-in actions-gateway-crds-v2 chart — must
// come up clean rather than crash-looping. The actions-gateway.com/v2alpha1
// RunnerSet CRD is absent, so:
//
//   - RunnerSetInstalled reports false (so main() skips registering the v2
//     RunnerSetReconciler, whose informer cache would otherwise never sync and
//     make mgr.Start exit(1) after the ~2m cache-sync deadline), and
//   - the v1 RunnerGroup reconciler still comes up: its caches sync and the
//     manager stays running.
//
// It runs against its own envtest with the v2 CRD path omitted, separate from the
// shared suite (which installs all CRDs).
func TestV1OnlyInstall_AGCComesUpClean(t *testing.T) {
	v1Env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			"../../../config/crd",
			// Deliberately NO "testdata/crd": omitting the v2
			// (actions-gateway.com/v2alpha1) CRDs reproduces a v1-only install.
		},
		ErrorIfCRDPathMissing: true,
		Scheme:                testScheme,
	}
	cfg, err := v1Env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = v1Env.Stop() })

	c, err := client.New(cfg, client.Options{Scheme: testScheme})
	require.NoError(t, err)

	skipNameValidation := true
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		Client: client.Options{
			Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}},
		},
	})
	require.NoError(t, err)

	// Detection: the RunnerSet CRD is absent ⇒ false, with no error.
	installed, err := controller.RunnerSetInstalled(mgr.GetRESTMapper())
	require.NoError(t, err)
	require.False(t, installed, "expected the v2alpha1 RunnerSet CRD to be reported absent on a v1-only install")

	// Document the failure mode the gate avoids: listing the RunnerSet kind against
	// this apiserver returns a NoMatch — exactly what the RunnerSetReconciler's
	// informer would hit forever if it were registered, blocking cache sync until
	// mgr.Start gives up and exits(1).
	var rss agcv2alpha1.RunnerSetList
	listErr := c.List(ctx, &rss)
	require.Error(t, listErr)
	require.True(t, meta.IsNoMatchError(listErr), "expected NoMatch listing RunnerSet on a v1-only install, got: %v", listErr)

	// The manager still comes up: register only the v1 RunnerGroup reconciler (as a
	// gated main() would) and start it. Its caches sync against the v1-only
	// apiserver and it keeps running — a registered v2 RunnerSetReconciler would
	// instead wedge cache sync and exit(1).
	mgrCtx, mgrCancel := context.WithCancel(ctx)
	tm := token.NewManager(stubProvider{}, nil)
	go tm.Start(mgrCtx)
	_, _ = tm.Token(mgrCtx)

	r := &controller.RunnerGroupReconciler{
		Client:       mgr.GetClient(),
		TokenManager: tm,
		Registrar:    &brokerRegistrar{stub: brokerStub},
		BrokerConfig: controller.BrokerConfig{
			BrokerURL:     brokerStub.URL,
			RunnerVersion: "2.335.1",
			RunnerOS:      "linux",
			UseV2Flow:     true,
			HTTPClient:    brokerStub.HTTPClient(),
			IdleThreshold: 500,
		},
	}
	require.NoError(t, r.SetupWithManager(mgr))

	mgrDone := make(chan struct{})
	var startErr error
	go func() {
		defer close(mgrDone)
		startErr = mgr.Start(mgrCtx)
	}()
	t.Cleanup(func() {
		mgrCancel()
		<-mgrDone
	})

	// WaitForCacheSync returning true is the up-and-healthy signal: the v1 caches
	// synced. With a v2 reconciler registered this would block until the deadline.
	require.True(t, mgr.GetCache().WaitForCacheSync(mgrCtx), "expected v1 caches to sync on a v1-only install")

	// And it stays up: no early exit in a follow-on window.
	select {
	case <-mgrDone:
		t.Fatalf("manager exited early on a v1-only install: %v", startErr)
	case <-time.After(2 * time.Second):
	}
}

// TestRunnerSetInstalled_PresentAgainstApiserver asserts the positive path of the
// Q261 startup gate: the shared suite installs the actions-gateway.com/v2alpha1
// CRDs, so RunnerSetInstalled must report true against that apiserver's discovery
// — the signal main() uses to register the v2 RunnerSetReconciler.
func TestRunnerSetInstalled_PresentAgainstApiserver(t *testing.T) {
	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	require.NoError(t, err)

	installed, err := controller.RunnerSetInstalled(mgr.GetRESTMapper())
	require.NoError(t, err)
	require.True(t, installed, "expected the v2alpha1 RunnerSet CRD to be reported present when installed")
}
