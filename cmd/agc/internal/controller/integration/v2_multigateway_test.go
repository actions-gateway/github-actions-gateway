//go:build integration

package integration_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/token"
	agcnames "github.com/actions-gateway/github-actions-gateway/agc/names"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// These tests exercise the M3b multi-gateway scoping (Q167) against the real
// apiserver: each AGC's RunnerSet informer is field-selector-scoped to its own
// gateway (spec.gatewayRef.name), so N gateways in one namespace each reconcile
// only their own RunnerSets — the isolation boundary. They also prove the
// ClusterRunnerTemplate cluster-scoped read/watch now resolves (the M3a deferral).

// startScopedRunnerSetReconciler wires a RunnerSetReconciler whose manager cache
// scopes the RunnerSet informer to gatewayName via a server-side field selector —
// exactly as cmd/agc/main.go does when the GMC stamps GATEWAY_NAME on the AGC
// Deployment. This is the real isolation mechanism, not an in-process filter.
func startScopedRunnerSetReconciler(t *testing.T, gatewayName string) {
	t.Helper()
	mgrCtx, mgrCancel := context.WithCancel(ctx)

	skipNameValidation := true
	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&v2alpha1.RunnerSet{}: {
					Field: fields.OneTermEqualSelector("spec.gatewayRef.name", gatewayName),
				},
			},
		},
		Client: client.Options{
			Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}},
		},
	})
	require.NoError(t, err)

	tm := token.NewManager(stubProvider{}, nil)
	go tm.Start(mgrCtx)
	_, _ = tm.Token(mgrCtx)

	p := provisioner.NewProvisioner(mgr.GetClient(), nil, slog.Default())
	p.PollInterval = 50 * time.Millisecond
	p.WorkerSA = agcnames.WorkerSAName
	p.DefaultWorkerImage = "runner:test"
	p.HTTPClient = brokerStub.HTTPClient()
	p.TokenFunc = stubProvider{}.Token

	r := &controller.RunnerSetReconciler{
		Client:       mgr.GetClient(),
		TokenManager: tm,
		Registrar:    &brokerRegistrar{stub: brokerStub},
		AgentKeyType: agentpool.KeyTypeEd25519,
		Provisioner:  p,
		GatewayName:  gatewayName,
		BrokerConfig: controller.BrokerConfig{
			BrokerURL:        brokerStub.URL,
			RunnerVersion:    "2.335.1",
			RunnerOS:         "linux",
			UseV2Flow:        true,
			HTTPClient:       brokerStub.HTTPClient(),
			IdleThreshold:    500,
			RenewJobInterval: 50 * time.Millisecond,
		},
	}
	require.NoError(t, r.SetupWithManager(mgr))

	mgrDone := make(chan struct{})
	go func() { defer close(mgrDone); _ = mgr.Start(mgrCtx) }()
	t.Cleanup(func() { mgrCancel(); <-mgrDone })
}

func TestV2_MultiGateway_AGCScopedToItsGateway(t *testing.T) {
	if m := serverMinor(t); m < 31 {
		t.Skipf("CRD field selectors (KEP-4358) are queryable only on k8s >= 1.31; apiserver is 1.%d", m)
	}

	const ns = "v2-mg-scope"
	createNSForAGC(t, ns)

	// Two gateways in one namespace, each pointing at the shared proxy; one shared
	// template. Two RunnerSets, each targeting a different gateway.
	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw-a", ns, "shared")))
	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw-b", ns, "shared")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	require.NoError(t, k8sClient.Create(ctx, &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}}))
	require.NoError(t, k8sClient.Create(ctx, newRunnerSet("set-a", ns, "gw-a")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerSet("set-b", ns, "gw-b")))
	t.Cleanup(func() {
		for _, n := range []string{"set-a", "set-b"} {
			_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns}})
		}
	})

	// Only the gw-a AGC is running.
	startScopedRunnerSetReconciler(t, "gw-a")

	// gw-a's AGC drives its own set to Ready (references resolve, a listener starts).
	waitForSetReadyReason(t, ns, "set-a", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	// Isolation: gw-a's AGC must NEVER touch gw-b's RunnerSet — its informer is
	// field-scoped to gw-a, so set-b is invisible. No finalizer, no status condition
	// is ever written. This is the boundary this milestone establishes: one
	// gateway's AGC cannot act on another gateway's RunnerSets.
	require.Never(t, func() bool {
		var rs v2alpha1.RunnerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "set-b"}, &rs); err != nil {
			return false
		}
		return len(rs.Finalizers) > 0 || len(rs.Status.Conditions) > 0
	}, 4*time.Second, 250*time.Millisecond, "gw-a AGC must not act on gw-b's RunnerSet (scoping breach)")

	// Bringing up gw-b's AGC concurrently drives set-b to Ready — proving two
	// gateways run concurrently in one namespace, each scoped to its own sets.
	startScopedRunnerSetReconciler(t, "gw-b")
	waitForSetReadyReason(t, ns, "set-b", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)
}

func TestV2_RunnerSet_ResolvesClusterRunnerTemplate(t *testing.T) {
	const ns = "v2-rs-crt"
	createNSForAGC(t, ns)

	// A cluster-scoped ClusterRunnerTemplate supplies the worker pod shape; the AGC
	// now holds the cluster-scoped read (per-gateway ClusterRoleBinding) and watches
	// the kind, so a set referencing it resolves (the M3a deferral, closed in M3b).
	const crtName = "v2-rs-crt-golden"
	crt := &v2alpha1.ClusterRunnerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: crtName},
		Spec: v2alpha1.RunnerTemplateSpec{
			WorkerImage: "runner:test",
			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}},
			}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "shared")))
	require.NoError(t, k8sClient.Create(ctx, &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}}))
	require.NoError(t, k8sClient.Create(ctx, crt))
	rs := newRunnerSet("crt-set", ns, "gw")
	rs.Spec.TemplateRef = v2alpha1.ObjectRef{Name: crtName, Kind: "ClusterRunnerTemplate"}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), crt)
	})

	startRunnerSetReconciler(t)

	// References (gateway + cluster template + proxy) all resolve → Ready/ListenerActive.
	waitForSetReadyReason(t, ns, "crt-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)
}
