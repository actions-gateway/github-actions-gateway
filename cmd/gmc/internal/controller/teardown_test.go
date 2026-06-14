package controller

import (
	"context"
	"errors"
	"testing"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// deletingAG returns an ActionsGateway that is mid-deletion: it carries the
// cleanup finalizer and a deletion timestamp, the precondition for reconcileDelete.
func deletingAG() *gmcv1alpha1.ActionsGateway {
	now := metav1.Now()
	return &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "tenant",
			Namespace:         "tenant-ns",
			UID:               "ag-uid-1",
			Finalizers:        []string{finalizerName},
			DeletionTimestamp: &now,
		},
	}
}

func hasFinalizer(ag *gmcv1alpha1.ActionsGateway) bool {
	for _, f := range ag.Finalizers {
		if f == finalizerName {
			return true
		}
	}
	return false
}

// TestReconcileDelete_FailClosedOnDeleteError verifies the Q125 fix: when a
// teardown delete returns a non-NotFound error, reconcileDelete must return an
// error (so the work requeues) and must NOT remove the finalizer — otherwise a
// transient API failure would orphan a live, credentialed AGC Deployment.
func TestReconcileDelete_FailClosedOnDeleteError(t *testing.T) {
	scheme := applyTestScheme(t)
	ag := deletingAG()

	// A live AGC Deployment that teardown will try (and fail) to delete.
	agcDep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: agcAppName, Namespace: ag.Namespace},
	}

	deleteErr := errors.New("apiserver unavailable")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag, agcDep).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				// Fail only the AGC Deployment delete; everything else passes through
				// so the test exercises the "some deletes succeed, one fails" path.
				if dep, ok := obj.(*appsv1.Deployment); ok && dep.GetName() == agcAppName {
					return deleteErr
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := applyTestReconciler(t, c, scheme)

	_, err := r.reconcileDelete(context.Background(), ag)
	require.Error(t, err, "a failed teardown delete must surface an error to requeue")

	// The finalizer must still be present on the live object.
	var fetched gmcv1alpha1.ActionsGateway
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: ag.Name}, &fetched))
	assert.True(t, hasFinalizer(&fetched), "finalizer must be retained while a delete is unconfirmed")

	// The orphan-prevention invariant: the AGC Deployment that failed to delete is
	// still present (not abandoned silently).
	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: agcAppName}, &dep),
		"the AGC Deployment must still exist after a failed delete")
}

// TestReconcileDelete_CleanTeardownRemovesFinalizer verifies that when every
// delete succeeds (or the resource is already NotFound), reconcileDelete removes
// the finalizer and converges — and that a repeated pass is idempotent.
func TestReconcileDelete_CleanTeardownRemovesFinalizer(t *testing.T) {
	scheme := applyTestScheme(t)
	ag := deletingAG()

	// Seed a couple of owned resources; the rest are already absent (NotFound),
	// which reconcileDelete must treat as success.
	agcDep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: agcAppName, Namespace: ag.Namespace}}
	agcSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: agcSAName, Namespace: ag.Namespace}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag, agcDep, agcSA).Build()
	r := applyTestReconciler(t, c, scheme)

	_, err := r.reconcileDelete(context.Background(), ag)
	require.NoError(t, err, "a clean teardown must not return an error")

	// Finalizer removed → the fake client garbage-collects the deleting object.
	var fetched gmcv1alpha1.ActionsGateway
	getErr := c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: ag.Name}, &fetched)
	if getErr == nil {
		assert.False(t, hasFinalizer(&fetched), "finalizer must be removed after a clean teardown")
	} else {
		require.True(t, client.IgnoreNotFound(getErr) == nil, "unexpected error fetching CR: %v", getErr)
	}

	// Idempotency: a second pass over an already-gone tenant still succeeds (every
	// delete is NotFound = success) — it must converge, not error.
	ag2 := deletingAG()
	_, err = r.reconcileDelete(context.Background(), ag2)
	require.NoError(t, err, "reconcileDelete must be idempotent over already-deleted resources")
}
