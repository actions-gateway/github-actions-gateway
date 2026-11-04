package controller

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestRunnerSetReconcile_AdoptsParkedRerunsBeforeReferencesResolve: a set whose
// references do not resolve still has its failure ledger read for parked re-runs, so
// after a restart its entries are swept and expire reported rather than pruned
// unreported (Q1244).
func TestRunnerSetReconcile_AdoptsParkedRerunsBeforeReferencesResolve(t *testing.T) {
	ns := "team-a"
	rs := rsObj("set", ns, func(rs *v2.RunnerSet) {
		rs.Finalizers = []string{runnerSetFinalizer}
	})
	var ledgerReads int
	c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(rs).WithStatusSubresource(rs).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, w client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok && strings.HasPrefix(key.Name, "scaleset-failures-") {
					ledgerReads++
				}
				return w.Get(ctx, key, obj, opts...)
			},
		}).Build()

	r := &RunnerSetReconciler{Client: c, Log: slog.Default(), Provisioner: provisioner.NewProvisioner(c, nil, nil)}
	r.ensureMaps()
	// The owner-reference re-stamp reads the ledger too; only the adoption read counts here.
	r.ownerRefsRestamped.Store(types.NamespacedName{Namespace: ns, Name: "set"}, rs.UID)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "set"}})
	require.NoError(t, err)

	var got v2.RunnerSet
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "set"}, &got))
	ready := meta.FindStatusCondition(got.Status.Conditions, v2.ConditionReady)
	require.NotNil(t, ready)
	require.Equal(t, v2.ReasonGatewayNotFound, ready.Reason, "the reconcile must have taken the unresolved-references return")
	assert.Equal(t, 1, ledgerReads, "the ledger is read once, ahead of the early return")
}
