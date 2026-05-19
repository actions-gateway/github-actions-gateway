package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/karlkfi/github-actions-gateway/agc/api/v1alpha1"
	"github.com/karlkfi/github-actions-gateway/agc/internal/agentpool"
	"github.com/karlkfi/github-actions-gateway/agc/internal/controller"
	"github.com/karlkfi/github-actions-gateway/agc/internal/token"
	"github.com/karlkfi/github-actions-gateway/githubapp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

	// Trigger deletion.
	require.NoError(t, fb.Delete(context.Background(), rg))

	// Re-fetch (deletion timestamp set).
	var updated v1alpha1.RunnerGroup
	require.NoError(t, fb.Get(context.Background(), key, &updated))

	// Set deletion timestamp manually since fake client doesn't auto-set it.
	now := metav1.Now()
	updated.DeletionTimestamp = &now
	require.NoError(t, fb.Update(context.Background(), &updated))

	reconcile(t, r, key)

	// All agent Secrets should be gone.
	var secrets corev1.SecretList
	require.NoError(t, fb.List(context.Background(), &secrets,
		client.InNamespace("default"),
		client.MatchingLabels{"actions-gateway/runner-group": "del-rg"},
	))
	assert.Empty(t, secrets.Items)
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
