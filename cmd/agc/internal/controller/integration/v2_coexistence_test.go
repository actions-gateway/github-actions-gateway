//go:build integration

package integration_test

import (
	"context"
	"log/slog"
	"sort"
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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// v1/v2 coexistence (Q466). A migration leaves the v1 RunnerGroup running beside the
// RunnerSet it became — same namespace, and in the common case the same name — because
// that is what makes rollback to v1 possible. Measured live on the dogfood cluster, the
// two then fought over one agent pool: identical Secret names, identical selector
// labels, identical GitHub runner names, and no owner reference to arbitrate, so the v1
// tenant sat in a permanent `secrets "agentpool-<name>-<index>" already exists` loop from
// the moment v2 came up.
//
// These suites run both reconcilers against the real apiserver, exactly as one AGC
// process does, and assert the property the rollback guarantee rests on: v1 keeps
// working while v2 is up, and keeps working after v2 goes away.

// startCoexistingReconcilers starts a RunnerGroup and a RunnerSet reconciler in one
// manager. No AGC is wired this way — the two are registered on different processes
// (Q535) — but running them in one is strictly harsher than the real coexistence
// window, and the properties asserted below are about the objects rather than the
// process layout: disjoint Secret names, labels, GitHub runner names and owner
// references keep the two pools separable however they are deployed.
func startCoexistingReconcilers(t *testing.T) {
	t.Helper()
	mgrCtx, mgrCancel := context.WithCancel(ctx)

	skipNameValidation := true
	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
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

	rg := &controller.RunnerGroupReconciler{
		Client:       mgr.GetClient(),
		TokenManager: tm,
		Registrar:    reg,
		AgentKeyType: agentpool.KeyTypeEd25519,
		BrokerConfig: brokerCfg,
	}
	require.NoError(t, rg.SetupWithManager(mgr))

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
		Sizing:       &sizingStub{},
		BrokerConfig: brokerCfg,
	}
	require.NoError(t, rs.SetupWithManager(mgr))

	mgrDone := make(chan struct{})
	go func() { defer close(mgrDone); _ = mgr.Start(mgrCtx) }()
	t.Cleanup(func() { mgrCancel(); <-mgrDone })
}

// agentSecretNames lists the agent Secrets in ns matching selector, sorted.
func agentSecretNames(t *testing.T, ns string, selector map[string]string) []string {
	t.Helper()
	var list corev1.SecretList
	require.NoError(t, k8sClient.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels(selector)))
	names := make([]string, 0, len(list.Items))
	for _, s := range list.Items {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return names
}

func runnerGroupAgentSelector(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": agcnames.ControllerName,
		"actions-gateway/runner-group": name,
	}
}

func runnerSetAgentSelector(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": agcnames.ControllerName,
		provisioner.LabelRunnerSet:     name,
	}
}

// waitForAgentSecrets blocks until exactly want agent Secrets match selector.
func waitForAgentSecrets(t *testing.T, ns string, selector map[string]string, want int) []string {
	t.Helper()
	var got []string
	require.Eventuallyf(t, func() bool {
		got = agentSecretNames(t, ns, selector)
		return len(got) == want
	}, 20*time.Second, 100*time.Millisecond,
		"expected %d agent Secrets for selector %v, last saw %v", want, selector, got)
	return got
}

// TestV2_Coexistence_SameNameGroupAndSetKeepSeparatePools is the end-to-end regression
// test: a v1 RunnerGroup and a v2 RunnerSet of the same name, reconciled side by side,
// then the v2 side removed as a rollback would remove it.
func TestV2_Coexistence_SameNameGroupAndSetKeepSeparatePools(t *testing.T) {
	const ns = "v2-coexist-pools"
	const name = "shared"
	createNSForAGC(t, ns)
	startCoexistingReconcilers(t)

	// The v1 tenant, running since before the migration.
	group := newRunnerGroup(ns, name, 2)
	require.NoError(t, k8sClient.Create(ctx, group))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), group) })
	groupSecrets := waitForAgentSecrets(t, ns, runnerGroupAgentSelector(name), 2)
	assert.Equal(t, []string{"agentpool-shared-0", "agentpool-shared-1"}, groupSecrets,
		"the v1 derivation must not move — v1 is the rollback target")

	// The migration lands: the same tenant, now as a RunnerSet of the same name.
	gw := newGatewayForSet("gw", ns, "")
	require.NoError(t, k8sClient.Create(ctx, gw))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), gw) })
	tmpl := newRunnerTemplate("tmpl", ns)
	require.NoError(t, k8sClient.Create(ctx, tmpl))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), tmpl) })
	set := newRunnerSet(name, ns, "gw")
	set.Spec.MaxListeners = 2
	require.NoError(t, k8sClient.Create(ctx, set))
	waitForSetReadyReason(t, ns, name, metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	setSecrets := waitForAgentSecrets(t, ns, runnerSetAgentSelector(name), 2)
	assert.Equal(t, []string{"agentpool-rs-shared-0", "agentpool-rs-shared-1"}, setSecrets)

	// The v1 pool is untouched: same Secrets, still exactly two, and the RunnerGroup
	// keeps reconciling rather than erroring on a name the RunnerSet took.
	assert.Equal(t, groupSecrets, agentSecretNames(t, ns, runnerGroupAgentSelector(name)),
		"the RunnerSet must not have adopted or overwritten the RunnerGroup's agents")
	require.Eventually(t, func() bool {
		var rg agcv1alpha1.RunnerGroup
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &rg); err != nil {
			return false
		}
		return rg.Status.ActiveSessions > 0
	}, 20*time.Second, 100*time.Millisecond, "the v1 RunnerGroup must still be running listeners while v2 is up")

	// Ownership is unambiguous: each agent Secret names exactly one owner, of the
	// right kind. That is what a human (or the garbage collector) reads to tell them
	// apart, and its absence is what left nothing to arbitrate.
	assertOwnedBy(t, ns, "agentpool-shared-0", "RunnerGroup", name)
	assertOwnedBy(t, ns, "agentpool-rs-shared-0", "RunnerSet", name)

	// Rollback: tear the v2 side down and confirm v1 is whole.
	require.NoError(t, k8sClient.Delete(ctx, set))
	require.Eventually(t, func() bool {
		return len(agentSecretNames(t, ns, runnerSetAgentSelector(name))) == 0
	}, 20*time.Second, 100*time.Millisecond, "the RunnerSet's agent Secrets should be cleaned up on delete")
	assert.Equal(t, groupSecrets, agentSecretNames(t, ns, runnerGroupAgentSelector(name)),
		"rolling back to v1 must leave the v1 tenant exactly as it was")
}

func assertOwnedBy(t *testing.T, ns, secret, kind, name string) {
	t.Helper()
	var sec corev1.Secret
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: secret}, &sec))
	require.Lenf(t, sec.OwnerReferences, 1, "agent Secret %s must name exactly one owner", secret)
	assert.Equal(t, kind, sec.OwnerReferences[0].Kind)
	assert.Equal(t, name, sec.OwnerReferences[0].Name)
}

// TestV2_Coexistence_LegacyAgentSecretsAreAdoptedOnlyWhenUnclaimed covers the upgrade
// path in both directions. A v2 install that predates the rename keeps its agents
// (adoption), while a coexisting v1 tenant's agents are left strictly alone — taking
// them would break the tenant the rollback guarantee protects.
func TestV2_Coexistence_LegacyAgentSecretsAreAdoptedOnlyWhenUnclaimed(t *testing.T) {
	const ns = "v2-coexist-adopt"
	createNSForAGC(t, ns)
	startCoexistingReconcilers(t)

	gw := newGatewayForSet("gw", ns, "")
	require.NoError(t, k8sClient.Create(ctx, gw))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), gw) })
	tmpl := newRunnerTemplate("tmpl", ns)
	require.NoError(t, k8sClient.Create(ctx, tmpl))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), tmpl) })

	t.Run("unclaimed legacy Secrets are carried across the rename", func(t *testing.T) {
		const name = "upgraded"
		legacy := writeLegacyAgentSecret(t, ns, name, 0)

		set := newRunnerSet(name, ns, "gw")
		require.NoError(t, k8sClient.Create(ctx, set))
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), set) })
		waitForSetReadyReason(t, ns, name, metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

		adopted := waitForAgentSecrets(t, ns, runnerSetAgentSelector(name), 1)
		require.Equal(t, []string{"agentpool-rs-upgraded-0"}, adopted)

		var sec corev1.Secret
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: adopted[0]}, &sec))
		assert.Equal(t, legacy.Data["agentId"], sec.Data["agentId"],
			"the existing GitHub registration must be preserved, not abandoned")

		var gone corev1.Secret
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: legacy.Name}, &gone)
		assert.True(t, client.IgnoreNotFound(err) == nil && err != nil,
			"the legacy copy must be removed, not left as a second copy of the key material")
	})

	t.Run("a live RunnerGroup's Secrets are left alone", func(t *testing.T) {
		const name = "claimed"
		group := newRunnerGroup(ns, name, 1)
		require.NoError(t, k8sClient.Create(ctx, group))
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), group) })
		groupSecrets := waitForAgentSecrets(t, ns, runnerGroupAgentSelector(name), 1)

		set := newRunnerSet(name, ns, "gw")
		require.NoError(t, k8sClient.Create(ctx, set))
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), set) })
		waitForSetReadyReason(t, ns, name, metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

		waitForAgentSecrets(t, ns, runnerSetAgentSelector(name), 1)
		assert.Equal(t, groupSecrets, agentSecretNames(t, ns, runnerGroupAgentSelector(name)),
			"the RunnerGroup's agents must survive a same-named RunnerSet coming up")
	})
}

// writeLegacyAgentSecret writes one agent Secret in the pre-Q466 shape — the shared
// derivation both APIs used — by running a RunnerGroup-scheme pool, which still writes
// exactly that layout.
func writeLegacyAgentSecret(t *testing.T, ns, name string, index int) *corev1.Secret {
	t.Helper()
	pool := agentpool.NewPool(k8sClient, ns, name, "2.335.1", []string{"self-hosted"},
		&brokerRegistrar{stub: brokerStub}, agentpool.KeyTypeEd25519)
	require.NoError(t, pool.EnsureAgents(ctx, int32(index+1), "inst-token"))

	var sec corev1.Secret
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Namespace: ns, Name: "agentpool-" + name + "-0",
	}, &sec))
	return &sec
}
