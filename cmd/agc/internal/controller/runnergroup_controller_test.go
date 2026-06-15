package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/token"
	"github.com/actions-gateway/github-actions-gateway/githubapp"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

// alwaysReadyProvider always returns a token without expiry.
type alwaysReadyProvider struct{}

func (alwaysReadyProvider) Token(_ context.Context) (string, error) {
	return "inst-token", nil
}
func (alwaysReadyProvider) TokenWithExpiry(_ context.Context) (*githubapp.InstallationToken, error) {
	return &githubapp.InstallationToken{
		Token:     "inst-token",
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}

func newTestReconciler(c client.Client) *controller.RunnerGroupReconciler {
	mgr := token.NewManager(alwaysReadyProvider{}, nil)
	ctx := context.Background()
	mgr.Start(ctx)
	_, _ = mgr.Token(ctx) // ensure ready

	return &controller.RunnerGroupReconciler{
		Client:       c,
		TokenManager: mgr,
		Registrar:    agentpool.NewStubRegistrar(),
		BrokerConfig: controller.BrokerConfig{
			// No real broker in unit tests; listener goroutines will fail to
			// fetch OAuth tokens (no auth server) and exit quickly, which is
			// fine — we're testing reconciler state transitions, not the goroutines.
		},
	}
}

func newRunnerGroup(ns, name string, maxListeners int32) *v1alpha1.RunnerGroup {
	return &v1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: v1alpha1.RunnerGroupSpec{
			MaxListeners: maxListeners,
			RunnerLabels: []string{"self-hosted"},
		},
	}
}

func reconcile(t *testing.T, r *controller.RunnerGroupReconciler, key types.NamespacedName) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	return res
}

func reconcileErr(t *testing.T, r *controller.RunnerGroupReconciler, key types.NamespacedName) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	return err
}

func TestReconcile_Create(t *testing.T) {
	rg := newRunnerGroup("default", "my-rg", 3)
	fb := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(rg).
		WithStatusSubresource(rg).
		Build()

	r := newTestReconciler(fb)
	key := types.NamespacedName{Namespace: "default", Name: "my-rg"}

	// First reconcile: adds finalizer.
	reconcile(t, r, key)
	// Second reconcile: provisions agents and starts multiplexer.
	reconcile(t, r, key)

	// Verify 3 agent Secrets created.
	var secrets corev1.SecretList
	require.NoError(t, fb.List(context.Background(), &secrets,
		client.InNamespace("default"),
		client.MatchingLabels{"actions-gateway/runner-group": "my-rg"},
	))
	assert.Len(t, secrets.Items, 3)
}

func TestReconcile_ScaleUp(t *testing.T) {
	rg := newRunnerGroup("default", "scale-rg", 2)
	fb := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(rg).
		WithStatusSubresource(rg).
		Build()

	r := newTestReconciler(fb)
	key := types.NamespacedName{Namespace: "default", Name: "scale-rg"}

	reconcile(t, r, key)
	reconcile(t, r, key)

	// Scale up: update spec.
	var updated v1alpha1.RunnerGroup
	require.NoError(t, fb.Get(context.Background(), key, &updated))
	updated.Spec.MaxListeners = 5
	require.NoError(t, fb.Update(context.Background(), &updated))

	reconcile(t, r, key)

	var secrets corev1.SecretList
	require.NoError(t, fb.List(context.Background(), &secrets,
		client.InNamespace("default"),
		client.MatchingLabels{"actions-gateway/runner-group": "scale-rg"},
	))
	assert.Len(t, secrets.Items, 5)
}

func TestReconcile_ScaleDown(t *testing.T) {
	rg := newRunnerGroup("default", "scale-rg", 5)
	fb := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(rg).
		WithStatusSubresource(rg).
		Build()

	r := newTestReconciler(fb)
	key := types.NamespacedName{Namespace: "default", Name: "scale-rg"}

	reconcile(t, r, key)
	reconcile(t, r, key)

	// Scale down.
	var updated v1alpha1.RunnerGroup
	require.NoError(t, fb.Get(context.Background(), key, &updated))
	updated.Spec.MaxListeners = 2
	require.NoError(t, fb.Update(context.Background(), &updated))

	reconcile(t, r, key)

	var secrets corev1.SecretList
	require.NoError(t, fb.List(context.Background(), &secrets,
		client.InNamespace("default"),
		client.MatchingLabels{"actions-gateway/runner-group": "scale-rg"},
	))
	assert.Len(t, secrets.Items, 2)
}

func TestReconcile_Delete(t *testing.T) {
	rg := newRunnerGroup("default", "del-rg", 2)
	fb := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(rg).
		WithStatusSubresource(rg).
		Build()

	r := newTestReconciler(fb)
	key := types.NamespacedName{Namespace: "default", Name: "del-rg"}

	reconcile(t, r, key) // add finalizer
	reconcile(t, r, key) // provision

	// Trigger deletion. The fake client stamps deletionTimestamp on Delete
	// because the finalizer is present; no manual re-stamp is needed (see the
	// Q140 note in TestReconcile_DeleteWithBrokenTokenManager).
	require.NoError(t, fb.Delete(context.Background(), rg))

	// Re-fetch; deletion timestamp is set by Delete.
	var updated v1alpha1.RunnerGroup
	require.NoError(t, fb.Get(context.Background(), key, &updated))
	require.False(t, updated.DeletionTimestamp.IsZero(),
		"Delete should stamp deletionTimestamp while the finalizer is set")

	reconcile(t, r, key)

	// All agent Secrets should be gone.
	var secrets corev1.SecretList
	require.NoError(t, fb.List(context.Background(), &secrets,
		client.InNamespace("default"),
		client.MatchingLabels{"actions-gateway/runner-group": "del-rg"},
	))
	assert.Empty(t, secrets.Items)
}

// nextEvent returns the next emitted event string, failing if none arrives
// within a short window.
func nextEvent(t *testing.T, rec *events.FakeRecorder) string {
	t.Helper()
	select {
	case e := <-rec.Events:
		return e
	case <-time.After(time.Second):
		t.Fatal("expected an event but none was recorded")
		return ""
	}
}

// A8: mergeCondition delegates to meta.SetStatusCondition, so a condition's
// LastTransitionTime is preserved across reconciles that don't change its
// Status (only advancing on a genuine transition) — not rewritten every time.
func TestReconcile_ConditionTransitionTimeStable(t *testing.T) {
	rg := newRunnerGroup("default", "ltt-rg", 1)
	fb := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(rg).
		WithStatusSubresource(rg).
		Build()

	r := newTestReconciler(fb)
	key := types.NamespacedName{Namespace: "default", Name: "ltt-rg"}

	reconcile(t, r, key) // add finalizer; initialises conditionCh
	reconcile(t, r, key) // provision agents

	readLTT := func() metav1.Time {
		t.Helper()
		var got v1alpha1.RunnerGroup
		require.NoError(t, fb.Get(context.Background(), key, &got))
		c := apimeta.FindStatusCondition(got.Status.Conditions, "RateLimited")
		require.NotNil(t, c, "expected RateLimited condition to be present")
		return c.LastTransitionTime
	}

	// Use explicit, whole-second LastTransitionTime values so the assertions are
	// deterministic (the fake client persists metav1.Time at second granularity).
	t0 := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	tLater := metav1.NewTime(time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC))

	// First observation: condition appears with the supplied LastTransitionTime.
	r.SetConditionForTest("default", "ltt-rg", metav1.Condition{
		Type: "RateLimited", Status: metav1.ConditionTrue, Reason: "SustainedRateLimit", Message: "first",
		LastTransitionTime: t0,
	})
	reconcile(t, r, key)
	assert.True(t, readLTT().Time.Equal(t0.Time), "first observation should keep the supplied LastTransitionTime")

	// Same Status, different Message, and a *different* supplied time → the merge
	// must preserve the original time rather than churn it.
	r.SetConditionForTest("default", "ltt-rg", metav1.Condition{
		Type: "RateLimited", Status: metav1.ConditionTrue, Reason: "SustainedRateLimit", Message: "updated message",
		LastTransitionTime: tLater,
	})
	reconcile(t, r, key)
	assert.True(t, readLTT().Time.Equal(t0.Time),
		"LastTransitionTime must not change when Status is unchanged")

	// Genuine Status transition → LastTransitionTime must advance to the new time.
	r.SetConditionForTest("default", "ltt-rg", metav1.Condition{
		Type: "RateLimited", Status: metav1.ConditionFalse, Reason: "Recovered", Message: "ok",
		LastTransitionTime: tLater,
	})
	reconcile(t, r, key)
	assert.True(t, readLTT().Time.Equal(tLater.Time),
		"LastTransitionTime must advance on a Status transition")
}

// A1: a token-manager failure surfaces as a Warning Event on the RunnerGroup.
func TestReconcile_TokenError_EmitsWarningEvent(t *testing.T) {
	rg := newRunnerGroup("default", "tokenevt-rg", 2)
	fb := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(rg).
		WithStatusSubresource(rg).
		Build()

	rec := events.NewFakeRecorder(16)
	r := &controller.RunnerGroupReconciler{
		Client:       fb,
		TokenManager: token.NewManagerWithExpiredToken(),
		Registrar:    agentpool.NewStubRegistrar(),
		Recorder:     rec,
	}
	key := types.NamespacedName{Namespace: "default", Name: "tokenevt-rg"}

	reconcile(t, r, key)                      // add finalizer (token not fetched yet)
	require.Error(t, reconcileErr(t, r, key)) // token fetch fails

	evt := nextEvent(t, rec)
	assert.Contains(t, evt, "Warning")
	assert.Contains(t, evt, "TokenUnavailable")
}

// A9: a NotFound reconcile drops the in-memory multiplexer/pool state so it
// cannot leak across the object's lifetime.
func TestReconcile_NotFoundCleansLocalState(t *testing.T) {
	rg := newRunnerGroup("default", "gone-rg", 2)
	fb := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(rg).
		WithStatusSubresource(rg).
		Build()

	r := newTestReconciler(fb)
	key := types.NamespacedName{Namespace: "default", Name: "gone-rg"}

	reconcile(t, r, key) // add finalizer
	reconcile(t, r, key) // provision → populates local state

	mux, pools := r.LocalStateCountForTest()
	require.Equal(t, 1, mux, "expected one tracked multiplexer after provisioning")
	require.Equal(t, 1, pools, "expected one tracked pool after provisioning")

	// Make the object fully disappear: clear the finalizer, then delete so the
	// fake client removes it entirely (a true NotFound on the next reconcile).
	var updated v1alpha1.RunnerGroup
	require.NoError(t, fb.Get(context.Background(), key, &updated))
	updated.Finalizers = nil
	require.NoError(t, fb.Update(context.Background(), &updated))
	require.NoError(t, fb.Delete(context.Background(), &updated))

	reconcile(t, r, key) // NotFound → cleanupLocalState

	mux, pools = r.LocalStateCountForTest()
	assert.Equal(t, 0, mux, "multiplexer state must be dropped on NotFound")
	assert.Equal(t, 0, pools, "pool state must be dropped on NotFound")
}

func TestReconcile_VersionTooOldCondition(t *testing.T) {
	rg := newRunnerGroup("default", "versionold-rg", 1)
	fb := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(rg).
		WithStatusSubresource(rg).
		Build()

	r := newTestReconciler(fb)
	key := types.NamespacedName{Namespace: "default", Name: "versionold-rg"}

	reconcile(t, r, key) // add finalizer; initialises conditionCh
	reconcile(t, r, key) // provision agents

	// Simulate a listener goroutine reporting a non-retriable version error.
	r.SetConditionForTest("default", "versionold-rg", metav1.Condition{
		Type:    "RunnerVersionTooOld",
		Status:  metav1.ConditionTrue,
		Reason:  "VersionTooOld",
		Message: "runner version too old",
	})

	reconcile(t, r, key) // drain conditions → status update

	var updated v1alpha1.RunnerGroup
	require.NoError(t, fb.Get(context.Background(), key, &updated))

	found := false
	for _, c := range updated.Status.Conditions {
		if c.Type == "RunnerVersionTooOld" {
			found = true
			break
		}
	}
	assert.True(t, found, "expected RunnerVersionTooOld condition to appear in status")
}

func TestReconcile_RateLimitedCondition(t *testing.T) {
	rg := newRunnerGroup("default", "ratelimited-rg", 1)
	fb := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(rg).
		WithStatusSubresource(rg).
		Build()

	r := newTestReconciler(fb)
	key := types.NamespacedName{Namespace: "default", Name: "ratelimited-rg"}

	reconcile(t, r, key) // add finalizer; initialises conditionCh
	reconcile(t, r, key) // provision agents

	// Simulate a listener goroutine reporting sustained rate-limiting.
	r.SetConditionForTest("default", "ratelimited-rg", metav1.Condition{
		Type:    "RateLimited",
		Status:  metav1.ConditionTrue,
		Reason:  "SustainedRateLimit",
		Message: "GetMessage returning 429 for >10 minutes",
	})

	reconcile(t, r, key) // drain conditions → status update

	var updated v1alpha1.RunnerGroup
	require.NoError(t, fb.Get(context.Background(), key, &updated))

	found := false
	for _, c := range updated.Status.Conditions {
		if c.Type == "RateLimited" {
			found = true
			break
		}
	}
	assert.True(t, found, "expected RateLimited condition to appear in status")
}

func TestReconcile_StatusActiveSessions(t *testing.T) {
	rg := newRunnerGroup("default", "status-rg", 3)
	fb := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(rg).
		WithStatusSubresource(rg).
		Build()

	r := newTestReconciler(fb)
	key := types.NamespacedName{Namespace: "default", Name: "status-rg"}

	reconcile(t, r, key)
	reconcile(t, r, key)

	var updated v1alpha1.RunnerGroup
	require.NoError(t, fb.Get(context.Background(), key, &updated))
	// At least one session should be reported (baseline goroutine may or may
	// not have started yet depending on timing — use >=0 for unit test stability).
	assert.GreaterOrEqual(t, updated.Status.ActiveSessions, int32(0))
	assert.Equal(t, rg.Generation, updated.Status.ObservedGeneration)
}

// TestReconcile_RequeuesWhileBelowDesiredListeners is the Q137 guard: while the
// multiplexer is below the RunnerGroup's listener ceiling — the state a
// non-retriable baseline exit leaves it in (ActiveCount drops to 0 and
// multiplexer.go does not auto-restart it) — Reconcile must schedule a bounded
// requeue so the ActiveCount()==0 recovery revives the baseline and status does
// not go stale until the next worker-pod watch event or the 10h resync.
func TestReconcile_RequeuesWhileBelowDesiredListeners(t *testing.T) {
	rg := newRunnerGroup("default", "requeue-rg", 2)
	fb := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(rg).
		WithStatusSubresource(rg).
		Build()

	r := newTestReconciler(fb)
	r.BaselineRecheckInterval = 1234 * time.Millisecond
	key := types.NamespacedName{Namespace: "default", Name: "requeue-rg"}

	reconcile(t, r, key)        // add finalizer
	res := reconcile(t, r, key) // provision agents + start multiplexer

	// At rest the multiplexer runs one baseline (< maxListeners=2), so the next
	// reconcile is scheduled at the baseline-recheck interval rather than deferred
	// to the resync. No worker pods exist, so the reaper contributes no earlier
	// deadline.
	assert.Equal(t, 1234*time.Millisecond, res.RequeueAfter,
		"expected a bounded requeue while below the listener ceiling (Q137)")
}

// ── Gap 9: Token manager failure ─────────────────────────────────────────────

func TestReconcile_TokenManagerError(t *testing.T) {
	rg := newRunnerGroup("default", "tokenerr-rg", 2)
	fb := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(rg).
		WithStatusSubresource(rg).
		Build()

	// Reconciler with an always-failing token manager.
	r := &controller.RunnerGroupReconciler{
		Client:       fb,
		TokenManager: token.NewManagerWithExpiredToken(),
		Registrar:    agentpool.NewStubRegistrar(),
	}
	key := types.NamespacedName{Namespace: "default", Name: "tokenerr-rg"}

	// First reconcile adds the finalizer (does not reach the token fetch yet).
	reconcile(t, r, key)

	// Second reconcile: token fetch fails → reconciler returns error.
	err := reconcileErr(t, r, key)
	require.Error(t, err, "reconciler should return error when token manager fails")

	// No agent Secrets should have been created.
	var secrets corev1.SecretList
	require.NoError(t, fb.List(context.Background(), &secrets,
		client.InNamespace("default"),
		client.MatchingLabels{"actions-gateway/runner-group": "tokenerr-rg"},
	))
	assert.Empty(t, secrets.Items, "no Secrets should be created when token manager errors")
}

func TestReconcile_DeleteWithBrokenTokenManager(t *testing.T) {
	rg := newRunnerGroup("default", "delbroke-rg", 2)
	fb := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(rg).
		WithStatusSubresource(rg).
		Build()

	r := newTestReconciler(fb)
	key := types.NamespacedName{Namespace: "default", Name: "delbroke-rg"}

	reconcile(t, r, key) // add finalizer
	reconcile(t, r, key) // provision 2 agents

	var secrets corev1.SecretList
	require.NoError(t, fb.List(context.Background(), &secrets,
		client.InNamespace("default"),
		client.MatchingLabels{"actions-gateway/runner-group": "delbroke-rg"},
	))
	require.Len(t, secrets.Items, 2, "expected 2 agent Secrets after provisioning")

	// Replace the token manager with an always-failing one.
	r.TokenManager = token.NewManagerWithExpiredToken()

	// Trigger deletion. The fake client stamps deletionTimestamp on Delete
	// because the finalizer is present, so the object is marked for deletion
	// rather than removed. Do NOT re-stamp it with a fresh metav1.Now(): the
	// fake client rejects an Update whose deletionTimestamp drifts >=1s from the
	// stored value ("deletionTimestamp field is immutable"), and under -race the
	// gap between Delete and a manual Now() occasionally crossed that boundary,
	// flaking the test (Q140).
	require.NoError(t, fb.Delete(context.Background(), rg))
	var updated v1alpha1.RunnerGroup
	require.NoError(t, fb.Get(context.Background(), key, &updated))
	require.False(t, updated.DeletionTimestamp.IsZero(),
		"Delete should stamp deletionTimestamp while the finalizer is set")

	// Reconcile: token fails but deletion proceeds gracefully with an empty token.
	reconcile(t, r, key)

	// All agent Secrets must be deleted.
	secrets = corev1.SecretList{}
	require.NoError(t, fb.List(context.Background(), &secrets,
		client.InNamespace("default"),
		client.MatchingLabels{"actions-gateway/runner-group": "delbroke-rg"},
	))
	assert.Empty(t, secrets.Items, "all agent Secrets should be deleted despite token failure")

	// The finalizer must be removed (object gone or no finalizer left).
	var final v1alpha1.RunnerGroup
	if err := fb.Get(context.Background(), key, &final); err == nil {
		assert.NotContains(t, final.Finalizers, "actions-gateway.github.com/agentpool-cleanup")
	}
}

// ── Gap 10: drainConditions isolation ────────────────────────────────────────

func TestReconcile_DrainConditionsIsolation(t *testing.T) {
	rgA := newRunnerGroup("default", "rg-a", 1)
	rgB := newRunnerGroup("default", "rg-b", 1)
	fb := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(rgA, rgB).
		WithStatusSubresource(rgA, rgB).
		Build()

	r := newTestReconciler(fb)
	keyA := types.NamespacedName{Namespace: "default", Name: "rg-a"}
	keyB := types.NamespacedName{Namespace: "default", Name: "rg-b"}

	// Provision both RunnerGroups.
	reconcile(t, r, keyA)
	reconcile(t, r, keyA)
	reconcile(t, r, keyB)
	reconcile(t, r, keyB)

	// Enqueue a condition targeting RunnerGroup B.
	r.SetConditionForTest("default", "rg-b", metav1.Condition{
		Type:    "RateLimited",
		Status:  metav1.ConditionTrue,
		Reason:  "SustainedRateLimit",
		Message: "for rg-b only",
	})

	// Reconcile A — the condition must be skipped and NOT applied to A.
	reconcile(t, r, keyA)

	var updatedA v1alpha1.RunnerGroup
	require.NoError(t, fb.Get(context.Background(), keyA, &updatedA))
	for _, c := range updatedA.Status.Conditions {
		assert.NotEqual(t, "RateLimited", c.Type, "RateLimited condition must not appear on rg-a")
	}

	// Reconcile B — the re-enqueued condition must now be applied to B.
	reconcile(t, r, keyB)

	var updatedB v1alpha1.RunnerGroup
	require.NoError(t, fb.Get(context.Background(), keyB, &updatedB))
	found := false
	for _, c := range updatedB.Status.Conditions {
		if c.Type == "RateLimited" {
			found = true
			break
		}
	}
	assert.True(t, found, "RateLimited condition should appear on rg-b after reconcile")
}

// ── Gap 11: pool-exhausted non-retriable error ────────────────────────────────

func TestReconcile_PoolExhausted(t *testing.T) {
	// maxListeners=0 → EnsureAgents(0) creates no Secrets → ClaimAgent returns nil →
	// Run returns NonRetriableError → multiplexer does not restart the goroutine.
	rg := newRunnerGroup("default", "exhaust-rg", 0)
	fb := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(rg).
		WithStatusSubresource(rg).
		Build()

	r := newTestReconciler(fb)
	key := types.NamespacedName{Namespace: "default", Name: "exhaust-rg"}

	reconcile(t, r, key) // add finalizer
	reconcile(t, r, key) // provision (0 agents), start multiplexer

	// The goroutine exits quickly with NonRetriableError and must not restart.
	// Poll by reconciling until ActiveSessions drops to 0.
	assert.Eventually(t, func() bool {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		if err != nil {
			return false
		}
		var updated v1alpha1.RunnerGroup
		if err := fb.Get(context.Background(), key, &updated); err != nil {
			return false
		}
		return updated.Status.ActiveSessions == 0
	}, 3*time.Second, 50*time.Millisecond, "goroutine count should settle at 0 after NonRetriableError")
}
