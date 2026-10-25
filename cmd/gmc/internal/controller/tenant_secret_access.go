package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// TenantSecretReaderName names both the singleton ClusterRole the chart ships
// (get/list/watch on Secrets) and the RoleBinding to it that the GMC creates in
// each tenant namespace. The GMC holds `bind` on this one name and no
// cluster-wide Secret read, so it can read Secrets only where the binding
// exists; gmc-tenant-resource-guard confines the binding's creation to tenant
// namespaces (Q1188, docs/design/05-security.md §5.1).
const TenantSecretReaderName = "gmc-tenant-secret-reader" //nolint:gosec // G101: an RBAC object name, not a credential

// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=bind,resourceNames=gmc-tenant-secret-reader

// isTenantNamespace reports whether labels carry the v1 or the v2 tenant marker,
// the same two markers gmc-tenant-resource-guard accepts.
func isTenantNamespace(labels map[string]string) bool {
	return labels[TenantNamespaceMarkerLabel] == "true" ||
		labels[gmcv2alpha1.TenantNamespaceMarkerLabel] == gmcv2alpha1.TenantNamespaceMarkerValue
}

// secretMetadata is the object type every per-namespace Secret source watches:
// metadata only, so no Secret .data enters the cache.
func secretMetadata() *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}}
}

// TenantSecretWatches runs one metadata-only Secret cache per tenant namespace and
// fans its events into every registered controller. It replaces a cluster-wide
// Secret informer, which would need a cluster-wide list/watch grant: RBAC cannot
// tell a metadata list from a full-body one.
//
// It is a leader-elected manager.Runnable, so caches run only on the replica
// running the controllers they feed. A controller-runtime source cannot be
// removed from a controller, so Stop cancels the cache and leaves an inert source
// behind; a namespace re-marked later gets a fresh cache and source.
type TenantSecretWatches struct {
	Config     *rest.Config
	HTTPClient *http.Client
	Scheme     *runtime.Scheme
	Mapper     meta.RESTMapper

	mu      sync.Mutex
	baseCtx context.Context
	sinks   []secretSink
	running map[string]context.CancelFunc
}

type secretSink struct {
	ctrl    controller.Controller
	mapFunc handler.MapFunc
}

// AddSink registers a controller to receive Secret events from every tenant
// namespace, mapped to requests by mapFunc. Call it before the manager starts.
func (w *TenantSecretWatches) AddSink(c controller.Controller, mapFunc handler.MapFunc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sinks = append(w.sinks, secretSink{ctrl: c, mapFunc: mapFunc})
}

// Start records the manager's lifetime context and blocks until it ends, then
// stops every namespace cache.
func (w *TenantSecretWatches) Start(ctx context.Context) error {
	w.mu.Lock()
	w.baseCtx = ctx
	w.mu.Unlock()
	<-ctx.Done()
	w.mu.Lock()
	defer w.mu.Unlock()
	for ns, cancel := range w.running {
		cancel()
		delete(w.running, ns)
	}
	return nil
}

// Ensure starts the namespace's Secret cache and its sources if they are not
// already running. An informer that lists before the namespace's RoleBinding is
// authorized gets Forbidden and retries with backoff.
func (w *TenantSecretWatches) Ensure(namespace string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.baseCtx == nil {
		return errors.New("tenant Secret watches not started")
	}
	if _, ok := w.running[namespace]; ok {
		return nil
	}
	c, err := cache.New(w.Config, cache.Options{
		HTTPClient:        w.HTTPClient,
		Scheme:            w.Scheme,
		Mapper:            w.Mapper,
		DefaultNamespaces: map[string]cache.Config{namespace: {}},
	})
	if err != nil {
		return fmt.Errorf("build Secret cache for namespace %s: %w", namespace, err)
	}
	cacheCtx, cancel := context.WithCancel(w.baseCtx)
	for _, s := range w.sinks {
		mapFunc := s.mapFunc
		src := source.Kind(c, secretMetadata(), handler.TypedEnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj *metav1.PartialObjectMetadata) []reconcile.Request {
				return mapFunc(ctx, obj)
			}))
		if err := s.ctrl.Watch(&scopedSource{ctx: cacheCtx, inner: src}); err != nil {
			cancel()
			return fmt.Errorf("watch Secrets in namespace %s: %w", namespace, err)
		}
	}
	go func() {
		if err := c.Start(cacheCtx); err != nil {
			logf.FromContext(cacheCtx).Error(err, "tenant Secret cache stopped", "namespace", namespace)
		}
	}()
	if w.running == nil {
		w.running = map[string]context.CancelFunc{}
	}
	w.running[namespace] = cancel
	return nil
}

// Stop cancels the namespace's Secret cache, if one is running.
func (w *TenantSecretWatches) Stop(namespace string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if cancel, ok := w.running[namespace]; ok {
		cancel()
		delete(w.running, namespace)
	}
}

// Running reports whether the namespace has a Secret cache running.
func (w *TenantSecretWatches) Running(namespace string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.running[namespace]
	return ok
}

// scopedSource ends its inner source when ctx ends as well as when the controller
// does, so a stopped namespace cache leaves no source goroutine waiting on it. It
// is deliberately not a SyncingSource, so a controller starting while a
// namespace's RoleBinding is still propagating does not block on that sync.
type scopedSource struct {
	ctx   context.Context
	inner source.Source
}

// Start starts the inner source under a context cancelled by either parent.
func (s *scopedSource) Start(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
	ctx, cancel := context.WithCancel(ctx)
	context.AfterFunc(s.ctx, cancel)
	return s.inner.Start(ctx, q)
}

// String names the source in controller logs.
func (s *scopedSource) String() string {
	return fmt.Sprintf("tenant namespace %v", s.inner)
}

// TenantSecretAccessReconciler grants the GMC Secret reads in each tenant
// namespace and runs that namespace's Secret watch. For a namespace carrying
// either tenant marker it applies the gmc-tenant-secret-reader RoleBinding to
// the GMC's own ServiceAccount, then ensures the namespace's Secret cache. A
// namespace that loses its marker or is deleted has its cache stopped; its
// RoleBinding stays, because gmc-tenant-resource-guard denies the GMC a delete in
// a namespace that is no longer a tenant.
type TenantSecretAccessReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// ServiceAccountName and ServiceAccountNamespace identify the GMC's own
	// ServiceAccount, the RoleBinding's subject.
	ServiceAccountName      string
	ServiceAccountNamespace string
	Watches                 *TenantSecretWatches
}

// Reconcile applies the namespace's reader RoleBinding and Secret watch, or stops
// the watch when the namespace is gone or no longer a tenant.
func (r *TenantSecretAccessReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ns corev1.Namespace
	if err := r.Get(ctx, req.NamespacedName, &ns); err != nil {
		if client.IgnoreNotFound(err) == nil {
			r.Watches.Stop(req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ns.DeletionTimestamp.IsZero() || !isTenantNamespace(ns.Labels) {
		r.Watches.Stop(ns.Name)
		return ctrl.Result{}, nil
	}
	desired := buildTenantSecretReaderRoleBinding(ns.Name, r.ServiceAccountName, r.ServiceAccountNamespace)
	obj := &rbacv1.RoleBinding{}
	if err := applyManagedChild(ctx, r.Client, r.Scheme, nil, obj, desired, func() error {
		obj.RoleRef = desired.RoleRef
		obj.Subjects = desired.Subjects
		return nil
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply %s RoleBinding: %w", TenantSecretReaderName, err)
	}
	if err := r.Watches.Ensure(ns.Name); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// buildTenantSecretReaderRoleBinding binds the GMC's ServiceAccount to the
// gmc-tenant-secret-reader ClusterRole in one namespace. RoleRef is immutable,
// which suits a binding whose only valid target is that one ClusterRole.
func buildTenantSecretReaderRoleBinding(namespace, saName, saNamespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      TenantSecretReaderName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "actions-gateway",
				"app.kubernetes.io/component":  TenantSecretReaderName,
				"app.kubernetes.io/part-of":    "actions-gateway",
				"app.kubernetes.io/managed-by": "actions-gateway-gmc",
			},
		},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: TenantSecretReaderName},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: saName, Namespace: saNamespace}},
	}
}

// SetupWithManager registers the reconciler on tenant namespaces, including the
// update that removes a marker, and on its RoleBinding so a deleted binding is
// restored. Both watches are metadata-light: Namespaces carry no secret material,
// and the RoleBinding watch is metadata-only.
func (r *TenantSecretAccessReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.Add(r.Watches); err != nil {
		return fmt.Errorf("register tenant Secret watches: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}, builder.WithPredicates(tenantMarkerPredicate())).
		WatchesMetadata(
			&rbacv1.RoleBinding{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: obj.GetNamespace()}}}
			}),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetName() == TenantSecretReaderName
			})),
		).
		Named("tenant-secret-access").
		Complete(r)
}

// tenantMarkerPredicate passes namespace events where the namespace is, or was, a
// tenant, so removing a marker reaches the reconciler.
func tenantMarkerPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return isTenantNamespace(e.Object.GetLabels()) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return isTenantNamespace(e.ObjectOld.GetLabels()) || isTenantNamespace(e.ObjectNew.GetLabels())
		},
		DeleteFunc:  func(e event.DeleteEvent) bool { return isTenantNamespace(e.Object.GetLabels()) },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

var _ reconcile.Reconciler = &TenantSecretAccessReconciler{}
