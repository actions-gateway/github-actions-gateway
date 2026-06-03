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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func statusTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, appsv1.AddToScheme(s))
	require.NoError(t, gmcv1alpha1.AddToScheme(s))
	return s
}

func newActionsGateway() *gmcv1alpha1.ActionsGateway {
	return &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant", Namespace: "tenant-ns"},
	}
}

// statusUpdateClient wraps a fake client, forcing every status subresource
// update to return the supplied error.
func statusUpdateClient(t *testing.T, ag *gmcv1alpha1.ActionsGateway, updErr error) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(statusTestScheme(t)).
		WithObjects(ag).
		WithStatusSubresource(ag).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
				return updErr
			},
		}).
		Build()
}

// A2: a conflict on Status().Update must requeue (re-Get and recompute) rather
// than being silently swallowed, which would drop the status write.
func TestUpdateStatus_RequeuesOnConflict(t *testing.T) {
	ag := newActionsGateway()
	conflict := apierrors.NewConflict(
		schema.GroupResource{Group: gmcv1alpha1.GroupVersion.Group, Resource: "actionsgateways"},
		ag.Name, errors.New("object was modified"))

	r := &ActionsGatewayReconciler{Client: statusUpdateClient(t, ag, conflict)}

	// Compare against the whole expected Result (reflection-based) rather than
	// reading the deprecated Result.Requeue field directly (staticcheck SA1019).
	res, err := r.updateStatus(context.Background(), ag)
	require.NoError(t, err, "conflict must not surface as an error")
	assert.Equal(t, ctrl.Result{Requeue: true}, res, "conflict must requeue updateStatus")

	res, err = r.setCredentialUnavailable(context.Background(), ag, "secret missing")
	require.NoError(t, err, "conflict must not surface as an error")
	assert.Equal(t, ctrl.Result{Requeue: true}, res, "conflict must requeue setCredentialUnavailable")
}

// A non-conflict error on Status().Update must still propagate as an error.
func TestUpdateStatus_PropagatesNonConflictError(t *testing.T) {
	ag := newActionsGateway()
	boom := errors.New("apiserver unavailable")

	r := &ActionsGatewayReconciler{Client: statusUpdateClient(t, ag, boom)}

	_, err := r.updateStatus(context.Background(), ag)
	require.Error(t, err)
	assert.False(t, apierrors.IsConflict(err))

	_, err = r.setCredentialUnavailable(context.Background(), ag, "secret missing")
	require.Error(t, err)
}
