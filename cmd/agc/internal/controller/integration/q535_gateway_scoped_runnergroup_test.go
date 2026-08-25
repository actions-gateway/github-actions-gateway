//go:build integration

package integration_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/token"
	agcnames "github.com/actions-gateway/github-actions-gateway/agc/names"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
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

// Q535: a gateway-scoped AGC must not reconcile the v1 RunnerGroups in its namespace.
//
// Q466 stopped the v1 AGC from reconciling v2 RunnerSets. The symmetric case stayed
// open: the v1 RunnerGroup reconciler was registered on every AGC, and a RunnerGroup
// names no gateway, so nothing scoped its informer the way spec.gatewayRef.name scopes
// the RunnerSet one. Measured live on the dogfood cluster, a migrated namespace ran two
// listener pools on one group at the same agentIndex — same agent Secret, same GitHub
// runner name — taking 409 from the broker on every CreateSession (153 in ~2.5 minutes)
// while both reconcilers wrote the same status.
//
// The gate is controller.ServesRunnerGroups, which cmd/agc/main.go and startAGC below
// both call. Weakening it puts the RunnerGroup reconciler back on the v2 AGC and the
// session-count assertions here fail.

// startAGC starts a manager wired as cmd/agc/main.go wires an AGC with this
// GATEWAY_NAME. Empty is the v1 singleton: it reconciles RunnerGroups and no
// RunnerSets. Non-empty is a per-gateway v2 AGC: it reconciles only its own gateway's
// RunnerSets, field-scoped server-side, and no RunnerGroups.
func startAGC(t *testing.T, gatewayName string) {
	t.Helper()
	mgrCtx, mgrCancel := context.WithCancel(ctx)

	skipNameValidation := true
	opts := ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		Client: client.Options{
			Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}},
		},
	}
	if gatewayName != "" {
		opts.Cache = cache.Options{ByObject: map[client.Object]cache.ByObject{
			&v2alpha1.RunnerSet{}: {Field: fields.OneTermEqualSelector("spec.gatewayRef.name", gatewayName)},
		}}
	}
	mgr, err := ctrl.NewManager(testEnv.Config, opts)
	require.NoError(t, err)

	tm := token.NewManager(stubProvider{}, nil)
	go tm.Start(mgrCtx)
	_, _ = tm.Token(mgrCtx)

	brokerCfg := controller.BrokerConfig{
		BrokerURL:        brokerStub.URL,
		RunnerVersion:    "2.335.1",
		RunnerOS:         "linux",
		UseV2Flow:        true,
		HTTPClient:       brokerStub.HTTPClient(),
		IdleThreshold:    500,
		RenewJobInterval: 50 * time.Millisecond,
	}
	reg := &brokerRegistrar{stub: brokerStub}

	// The two gates are independent, as they are in main.go: before Q535 a v2 AGC
	// registered the RunnerGroup reconciler as well as the RunnerSet one, and that is
	// the shape the assertions below have to be able to see.
	if controller.ServesRunnerGroups(gatewayName) {
		rg := &controller.RunnerGroupReconciler{
			Client:       mgr.GetClient(),
			TokenManager: tm,
			Registrar:    reg,
			AgentKeyType: agentpool.KeyTypeEd25519,
			BrokerConfig: brokerCfg,
		}
		require.NoError(t, rg.SetupWithManager(mgr))
	}
	if gatewayName != "" {
		p := provisioner.NewProvisioner(mgr.GetClient(), nil, slog.Default())
		p.PollInterval = 50 * time.Millisecond
		p.WorkerSA = agcnames.WorkerSAName
		p.DefaultWorkerImage = "runner:test"
		p.HTTPClient = brokerStub.HTTPClient()
		p.TokenFunc = stubProvider{}.Token

		rs := &controller.RunnerSetReconciler{
			Client:       mgr.GetClient(),
			APIReader:    mgr.GetAPIReader(),
			TokenManager: tm,
			Registrar:    reg,
			AgentKeyType: agentpool.KeyTypeEd25519,
			Provisioner:  p,
			GatewayName:  gatewayName,
			Sizing:       &sizingStub{},
			BrokerConfig: brokerCfg,
		}
		require.NoError(t, rs.SetupWithManager(mgr))
	}

	mgrDone := make(chan struct{})
	go func() { defer close(mgrDone); _ = mgr.Start(mgrCtx) }()
	t.Cleanup(func() { mgrCancel(); <-mgrDone })
}

// waitForGroupSessions blocks until the broker stub holds exactly want active sessions
// owned by the given RunnerGroup, and returns their IDs.
//
// A listener owns its session as its own registered runner name, which is
// kind-scoped: "<name>-<agentIndex>" for a RunnerGroup and "rs-<name>-<agentIndex>"
// for a RunnerSet (Q466, carried onto the wire by Q677). So this separates the two
// pools by kind as well as by name, and passing a RunnerGroup name here cannot
// match a RunnerSet's sessions.
func waitForGroupSessions(t *testing.T, group string, want int) []string {
	t.Helper()
	var got []string
	require.Eventually(t, func() bool {
		got = brokerStub.ActiveSessionsForOwner(group)
		return len(got) == want
	}, 20*time.Second, 100*time.Millisecond,
		"expected %d active broker sessions for RunnerGroup %s", want, group)
	return got
}

// TestQ535_GatewayScopedAGCDeclinesV1RunnerGroups runs the two AGCs a mid-migration
// namespace actually has — the v1 singleton and the migrated gateway's own — and
// asserts the v1 RunnerGroup keeps exactly one listener pool.
func TestQ535_GatewayScopedAGCDeclinesV1RunnerGroups(t *testing.T) {
	if m := serverMinor(t); m < 31 {
		t.Skipf("CRD field selectors (KEP-4358) are queryable only on k8s >= 1.31; apiserver is 1.%d", m)
	}

	const ns = "q535-coexist"
	// Distinct from other suites' names: the broker stub is shared and scopes session
	// owners by CR name, so a name another test also uses would be counted here.
	// setName differs from name only to keep the two readable apart; since Q677 the
	// owner filter separates them by kind anyway (see waitForGroupSessions).
	const name = "q535-migrated"
	const setName = "q535-set"
	createNSForAGC(t, ns)

	// The v1 tenant, running since before the migration, on the v1 AGC. One listener
	// keeps the discriminator sharp: the duplicate pool ran at the *same* agentIndex,
	// so the defect shows up as two sessions owned by "<group>-0".
	group := newRunnerGroup(ns, name, 1)
	require.NoError(t, k8sClient.Create(ctx, group))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), group) })
	startAGC(t, "")

	waitForAgentSecrets(t, ns, runnerGroupAgentSelector(name), 1)
	v1Sessions := waitForGroupSessions(t, name, 1)

	// The migration lands: same namespace, now a RunnerSet served by its own
	// gateway-scoped AGC. Both AGCs are live from here — the coexistence window.
	gw := newGatewayForSet("gw", ns, "")
	require.NoError(t, k8sClient.Create(ctx, gw))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), gw) })
	tmpl := newRunnerTemplate("tmpl", ns)
	require.NoError(t, k8sClient.Create(ctx, tmpl))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), tmpl) })
	set := newRunnerSet(setName, ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, set))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), set) })

	startAGC(t, "gw")
	waitForSetReadyReason(t, ns, setName, metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	// The defect, stated as a count: a second RunnerGroup reconciler opens its own
	// session at the same agent index, so the group holds two where it should hold one.
	require.Never(t, func() bool {
		return len(brokerStub.ActiveSessionsForOwner(name)) > 1
	}, 5*time.Second, 250*time.Millisecond,
		"the gateway-scoped AGC must not start a second listener on the v1 RunnerGroup")

	// Still the v1 AGC's own session, not one a second controller took over by winning
	// the race for the same agent index — the case a count alone cannot see.
	assert.Equal(t, v1Sessions, brokerStub.ActiveSessionsForOwner(name),
		"the v1 listener must be the same broker session the v1 AGC opened")

	// The v1 agent pool is the v1 AGC's alone.
	assert.Equal(t, []string{"agentpool-q535-migrated-0"},
		agentSecretNames(t, ns, runnerGroupAgentSelector(name)),
		"the v1 agent pool must be the v1 AGC's alone")

	// Liveness, not a second count of the pool: a doubled reconciler writes 1 here too
	// (each sees its own listener), so this says the v1 tenant is still serving while
	// the migrated gateway runs beside it — the rollback guarantee.
	require.Eventually(t, func() bool {
		var rg agcv1alpha1.RunnerGroup
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &rg); err != nil {
			return false
		}
		return rg.Status.ActiveSessions == 1
	}, 20*time.Second, 100*time.Millisecond,
		"the v1 RunnerGroup must keep serving while the v2 AGC is up")
}
