package controller

import (
	"context"
	"log/slog"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The projected share ConfigMap is the AGC's one unwatchable referent (§H.9, Q999):
// the Role grants get and not list/watch, and RBAC has no label selector to narrow a
// watch to the labelled shares, so a bounded re-check is what gives revocation and
// late consent an event source. These pin the cadence to the sets that need it.

func TestProxyShareRecheckInterval(t *testing.T) {
	r := &RunnerSetReconciler{}
	assert.Equal(t, defaultProxyShareRecheckInterval, r.proxyShareRecheckInterval())
	r.ProxyShareRecheckInterval = 3 * time.Second
	assert.Equal(t, 3*time.Second, r.proxyShareRecheckInterval())
}

func TestWithProxyShareRecheck(t *testing.T) {
	const recheck = 30 * time.Second
	shared := &resolvedRefs{proxy: &resolvedProxy{caConfigMapName: "proxy-share-platform-egress-shared"}}
	colocated := &resolvedRefs{proxy: &resolvedProxy{tlsSecretName: egressProxyTLSSecretName("shared")}}
	direct := &resolvedRefs{}

	tests := []struct {
		name string
		refs *resolvedRefs
		in   time.Duration
		want time.Duration
	}{
		{"projected share with no other deadline takes the re-check", shared, 0, recheck},
		{"projected share keeps a sooner deadline", shared, 5 * time.Second, 5 * time.Second},
		{"projected share overrides a later deadline", shared, time.Hour, recheck},
		{"colocated proxy is left alone — the EgressProxy watch covers it", colocated, 0, 0},
		{"direct egress is left alone", direct, 0, 0},
		{"colocated proxy keeps its own deadline", colocated, time.Hour, time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &RunnerSetReconciler{ProxyShareRecheckInterval: recheck}
			got := r.withProxyShareRecheck(ctrl.Result{RequeueAfter: tc.in}, tc.refs)
			assert.Equal(t, tc.want, got.RequeueAfter)
		})
	}
}

// A set failing closed on a withdrawn or not-yet-granted share must carry the
// re-check out of Reconcile itself. Every other fail-closed reason names a watched
// referent and deliberately returns no requeue, so this asserts the requeue is
// reason-scoped rather than blanket.
func TestRunnerSetReconcile_ProxyShareNotGrantedRequeues(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	const ns = "team-a"
	rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
		rs.Finalizers = []string{runnerSetFinalizer} // skip the finalizer-add requeue
		rs.Spec.ProxyRef = &v2alpha1.ProxyObjectRef{Name: "shared", Namespace: "platform-egress"}
	})
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(rs, gwObj("gw", ns, ""), tmplObj("tmpl", ns)).
		WithStatusSubresource(rs).Build()

	r := &RunnerSetReconciler{Client: c, Log: slog.Default(), ProxyShareRecheckInterval: 42 * time.Second}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "set"}})
	require.NoError(t, err)

	var got v2alpha1.RunnerSet
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "set"}, &got))
	ready := meta.FindStatusCondition(got.Status.Conditions, v2alpha1.ConditionReady)
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionFalse, ready.Status)
	require.Equal(t, v2alpha1.ReasonProxyShareNotGranted, ready.Reason)
	assert.Equal(t, 42*time.Second, res.RequeueAfter,
		"a share the AGC cannot watch has no other event source to restore it")
}

// The sibling fail-closed reasons name watched referents, so re-checking them would
// be a poll the referent watch already covers. Pinned here because the requeue above
// sits one line from the shared return.
func TestRunnerSetReconcile_WatchedReferentDoesNotRequeue(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	const ns = "team-a"
	rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
		rs.Finalizers = []string{runnerSetFinalizer}
		rs.Spec.ProxyRef = &v2alpha1.ProxyObjectRef{Name: "missing"}
	})
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(rs, gwObj("gw", ns, ""), tmplObj("tmpl", ns)).
		WithStatusSubresource(rs).Build()

	r := &RunnerSetReconciler{Client: c, Log: slog.Default(), ProxyShareRecheckInterval: 42 * time.Second}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "set"}})
	require.NoError(t, err)

	var got v2alpha1.RunnerSet
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "set"}, &got))
	ready := meta.FindStatusCondition(got.Status.Conditions, v2alpha1.ConditionReady)
	require.NotNil(t, ready)
	require.Equal(t, v2alpha1.ReasonProxyNotFound, ready.Reason)
	assert.Zero(t, res.RequeueAfter, "the EgressProxy watch re-enqueues this one")
}
