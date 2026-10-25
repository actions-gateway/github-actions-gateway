package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

func TestIsTenantNamespace(t *testing.T) {
	assert.True(t, isTenantNamespace(map[string]string{TenantNamespaceMarkerLabel: "true"}), "v1 marker")
	assert.True(t, isTenantNamespace(map[string]string{
		gmcv2alpha1.TenantNamespaceMarkerLabel: gmcv2alpha1.TenantNamespaceMarkerValue,
	}), "v2 marker")
	assert.False(t, isTenantNamespace(map[string]string{TenantNamespaceMarkerLabel: "false"}), "v1 marker set false")
	assert.False(t, isTenantNamespace(map[string]string{gmcv2alpha1.TenantNamespaceMarkerLabel: "true"}), "v2 marker with the v1 value")
	assert.False(t, isTenantNamespace(nil), "no labels")
}

// TestTenantMarkerPredicate_PassesUnmark pins that removing a marker reaches the
// reconciler, which is what stops the namespace's Secret cache.
func TestTenantMarkerPredicate_PassesUnmark(t *testing.T) {
	p := tenantMarkerPredicate()
	ns := func(labels map[string]string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team", Labels: labels}}
	}
	tenant := map[string]string{TenantNamespaceMarkerLabel: "true"}

	assert.True(t, p.Create(event.CreateEvent{Object: ns(tenant)}))
	assert.False(t, p.Create(event.CreateEvent{Object: ns(nil)}))
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: ns(tenant), ObjectNew: ns(nil)}), "unmark")
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: ns(nil), ObjectNew: ns(tenant)}), "mark")
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: ns(nil), ObjectNew: ns(map[string]string{"x": "y"})}))
	assert.True(t, p.Delete(event.DeleteEvent{Object: ns(tenant)}))
	assert.False(t, p.Delete(event.DeleteEvent{Object: ns(nil)}))
}

func TestBuildTenantSecretReaderRoleBinding(t *testing.T) {
	rb := buildTenantSecretReaderRoleBinding("team", "gmc-controller-manager", "gmc-system")
	assert.Equal(t, "team", rb.Namespace)
	assert.Equal(t, TenantSecretReaderName, rb.Name)
	assert.Equal(t, "ClusterRole", rb.RoleRef.Kind)
	assert.Equal(t, TenantSecretReaderName, rb.RoleRef.Name)
	if assert.Len(t, rb.Subjects, 1) {
		assert.Equal(t, "ServiceAccount", rb.Subjects[0].Kind)
		assert.Equal(t, "gmc-controller-manager", rb.Subjects[0].Name)
		assert.Equal(t, "gmc-system", rb.Subjects[0].Namespace)
	}
}

func TestTenantSecretWatches_EnsureBeforeStartFails(t *testing.T) {
	w := &TenantSecretWatches{}
	assert.Error(t, w.Ensure("team"))
	assert.False(t, w.Running("team"))
}

// fakeSinkController records the sources a TenantSecretWatches hands it.
type fakeSinkController struct {
	controller.Controller
	watched []source.Source
}

func (f *fakeSinkController) Watch(src source.Source) error {
	f.watched = append(f.watched, src)
	return nil
}

func newTestSecretWatches(t *testing.T) (*TenantSecretWatches, *fakeSinkController) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	w := &TenantSecretWatches{
		Config:     &rest.Config{Host: "http://127.0.0.1:1"},
		HTTPClient: http.DefaultClient,
		Scheme:     scheme,
		Mapper:     mapper,
	}
	sink := &fakeSinkController{}
	w.AddSink(sink, func(context.Context, client.Object) []reconcile.Request { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Start(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	require.Eventually(t, func() bool { return w.Ensure("probe") == nil }, 5*time.Second, 10*time.Millisecond)
	w.Stop("probe")
	sink.watched = nil
	return w, sink
}

func TestTenantSecretWatches_EnsureIsIdempotentAndStopEnds(t *testing.T) {
	w, sink := newTestSecretWatches(t)

	require.NoError(t, w.Ensure("team"))
	require.NoError(t, w.Ensure("team"))
	assert.True(t, w.Running("team"))
	assert.Len(t, sink.watched, 1, "a second Ensure adds no source")

	w.Stop("team")
	assert.False(t, w.Running("team"))
	w.Stop("team") // stopping twice is a no-op
}

func TestTenantSecretAccessReconciler(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	tenant := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant", Labels: map[string]string{TenantNamespaceMarkerLabel: "true"}}}
	other := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant, other).Build()
	w, _ := newTestSecretWatches(t)
	r := &TenantSecretAccessReconciler{
		Client: c, Scheme: scheme,
		ServiceAccountName: "gmc-controller-manager", ServiceAccountNamespace: "gmc-system",
		Watches: w,
	}
	ctx := context.Background()
	req := func(name string) ctrl.Request { return ctrl.Request{NamespacedName: types.NamespacedName{Name: name}} }

	_, err := r.Reconcile(ctx, req("tenant"))
	require.NoError(t, err)
	var rb rbacv1.RoleBinding
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "tenant", Name: TenantSecretReaderName}, &rb))
	assert.Equal(t, "gmc-controller-manager", rb.Subjects[0].Name)
	assert.True(t, w.Running("tenant"))

	_, err = r.Reconcile(ctx, req("other"))
	require.NoError(t, err)
	err = c.Get(ctx, types.NamespacedName{Namespace: "other", Name: TenantSecretReaderName}, &rbacv1.RoleBinding{})
	assert.True(t, apierrors.IsNotFound(err), "no binding in an unmarked namespace; got %v", err)
	assert.False(t, w.Running("other"))

	// Unmarking stops the watch and leaves the binding (the guard denies its delete).
	tenant.Labels = nil
	require.NoError(t, c.Update(ctx, tenant))
	_, err = r.Reconcile(ctx, req("tenant"))
	require.NoError(t, err)
	assert.False(t, w.Running("tenant"))

	// A namespace that is gone stops its watch.
	require.NoError(t, w.Ensure("gone"))
	_, err = r.Reconcile(ctx, req("gone"))
	require.NoError(t, err)
	assert.False(t, w.Running("gone"))
}

// ctxSource records the context its Start receives.
type ctxSource struct{ ctx context.Context }

func (s *ctxSource) Start(ctx context.Context, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
	s.ctx = ctx
	return nil
}

// TestScopedSource_EndsWithNamespace pins that stopping a namespace's cache ends
// the source's context even while the controller's runs on.
func TestScopedSource_EndsWithNamespace(t *testing.T) {
	nsCtx, stopNS := context.WithCancel(context.Background())
	inner := &ctxSource{}
	s := &scopedSource{ctx: nsCtx, inner: inner}
	require.NoError(t, s.Start(context.Background(), nil))
	assert.Contains(t, s.String(), "tenant namespace")

	stopNS()
	select {
	case <-inner.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the source's context outlived its namespace")
	}
}
