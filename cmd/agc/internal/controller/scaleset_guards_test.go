package controller

import (
	"context"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Tests for the ConfigMap-backed GuardStore (Q606): the durable state the scale-set
// listener writes ahead of its message deletes, so a hard-killed AGC does not replay
// an assignment it concluded. The listener-side semantics (write-ahead ordering,
// retirement, the drained-queue sweep) are covered in the scalesetlistener package
// against a fake store; these cover the ConfigMap round-trip itself.

func guardScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = v2alpha1.AddToScheme(s)
	return s
}

func guardStoreFor(t *testing.T, rs *v2alpha1.RunnerSet) *scaleSetGuardStore {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(guardScheme()).Build()
	r := &RunnerSetReconciler{Client: c}
	return r.scaleSetGuardStore(rs)
}

func testRunnerSet() *v2alpha1.RunnerSet {
	return &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "tenant-a", Name: "linux-large", UID: types.UID("uid-1"),
	}}
}

// TestScaleSetGuardStore_LoadBeforeFirstSaveIsEmpty pins the first-start shape: no
// ConfigMap is not an error, it is the empty state — a brand-new RunnerSet must come
// up without anyone pre-creating anything.
func TestScaleSetGuardStore_LoadBeforeFirstSaveIsEmpty(t *testing.T) {
	s := guardStoreFor(t, testRunnerSet())
	state, err := s.Load(context.Background())
	require.NoError(t, err)
	assert.Empty(t, state.Completed)
	assert.Empty(t, state.Abandoned)
}

// TestScaleSetGuardStore_SaveRoundTrips covers create-then-update: the first save
// creates the ConfigMap, later saves replace its state, and Load returns what was
// last saved.
func TestScaleSetGuardStore_SaveRoundTrips(t *testing.T) {
	s := guardStoreFor(t, testRunnerSet())
	ctx := context.Background()

	require.NoError(t, s.Save(ctx, scalesetlistener.GuardState{
		Completed: []string{"job-a"}, Abandoned: []string{"job-b"},
	}))
	state, err := s.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"job-a"}, state.Completed)
	assert.Equal(t, []string{"job-b"}, state.Abandoned)

	require.NoError(t, s.Save(ctx, scalesetlistener.GuardState{Completed: []string{"job-c"}}))
	state, err = s.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"job-c"}, state.Completed)
	assert.Empty(t, state.Abandoned, "a save replaces the state, it does not merge")
}

// TestScaleSetGuardStore_ConfigMapCarriesOwnershipAndLabel pins the lifecycle wiring:
// the ConfigMap is controller-owned by its RunnerSet — deleting the set (or its
// namespace) garbage-collects the guards, which is the only delete path the AGC has —
// and labelled the way the set's worker pods are, so an operator can find it.
func TestScaleSetGuardStore_ConfigMapCarriesOwnershipAndLabel(t *testing.T) {
	rs := testRunnerSet()
	c := fake.NewClientBuilder().WithScheme(guardScheme()).Build()
	r := &RunnerSetReconciler{Client: c}
	s := r.scaleSetGuardStore(rs)

	require.NoError(t, s.Save(context.Background(), scalesetlistener.GuardState{Completed: []string{"job-a"}}))

	var cm corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: rs.Namespace, Name: scaleSetGuardConfigMapName(rs.Name)}, &cm))
	require.Len(t, cm.OwnerReferences, 1)
	assert.Equal(t, "RunnerSet", cm.OwnerReferences[0].Kind)
	assert.Equal(t, rs.Name, cm.OwnerReferences[0].Name)
	assert.Equal(t, rs.UID, cm.OwnerReferences[0].UID)
	assert.Equal(t, rs.Name, cm.Labels[provisioner.LabelRunnerSet])
}

// TestScaleSetGuardStore_LoadRejectsUnparseableState pins the failure direction for a
// corrupt ConfigMap: an error, not an empty read — an empty read would silently reopen
// the replay window, while the error keeps the listener down until the ConfigMap is
// deleted or repaired, with the remediation in the message.
func TestScaleSetGuardStore_LoadRejectsUnparseableState(t *testing.T) {
	rs := testRunnerSet()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: rs.Namespace, Name: scaleSetGuardConfigMapName(rs.Name),
		},
		Data: map[string]string{scaleSetGuardDataKey: "{not json"},
	}
	c := fake.NewClientBuilder().WithScheme(guardScheme()).WithObjects(cm).Build()
	r := &RunnerSetReconciler{Client: c}
	s := r.scaleSetGuardStore(rs)

	_, err := s.Load(context.Background())
	require.ErrorContains(t, err, "delete the ConfigMap")
}
