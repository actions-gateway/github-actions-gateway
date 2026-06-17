// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=actionsgateways,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=actionsgateways/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=actionsgateways/finalizers,verbs=update
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=runnergroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts;services,verbs=get;list;watch;create;update;patch;delete
// The namespace ResourceQuota (and any LimitRange) is platform-owned: the
// platform admin manages it on the tenant namespace and GAG operates within it.
// The GMC deliberately holds no resourcequotas/limitranges write verbs — dropping
// that grant is least privilege (Q122/Q130). See docs/design/05-security.md.
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update
// events: the reconciler emits Kubernetes Events on teardown and PSA-label
// failures. Both groups are required — the manager's new-style recorder
// (mgr.GetEventRecorder) writes events.k8s.io/v1 Events, so with only the core
// ("") grant every Event is silently 403'd (Q112; same class as the AGC Q95 fix).
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// Per-tenant RoleBindings reference the shipped agc-tenant-role ClusterRole.
// `bind` is scoped to that single name so a compromised GMC cannot wire its
// SA into arbitrary ClusterRoles. Without this, the api server would require
// the GMC to itself hold every permission inside agc-tenant-role (or escalate).
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=bind,resourceNames=agc-tenant-role
// Legacy cleanup: pre-v0.X installs created a per-tenant Role with the AGC
// permission set; reconcileDelete still removes those during upgrade.
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

package controller

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// ActionsGatewayReconciler reconciles ActionsGateway objects.
type ActionsGatewayReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// IPCache supplies cached GitHub IP CIDRs for the proxy NetworkPolicy
	// egress rule. It is populated and refreshed by IPRangeReconciler;
	// reads here are non-blocking and never perform network I/O. A nil
	// cache or an empty snapshot is tolerated — the periodic reconciler
	// will patch any NetworkPolicies that were created without CIDRs once
	// it completes its first fetch.
	//
	// Earlier revisions did a network fetch on every reconcile, which
	// serialised behind a single goroutine (MaxConcurrentReconciles=1)
	// and stalled the reconciler queue whenever the GitHub API was slow
	// or unreachable. The cache removes that blocking call.
	IPCache     *IPRangeCache
	AGCImage    string
	ProxyImage  string
	AGCExtraEnv []corev1.EnvVar // extra env vars forwarded to AGC pods (e.g. for tests)
	// Recorder emits Kubernetes Events on the reconciled ActionsGateway.
	// May be nil in unit tests; callers must nil-check before use.
	Recorder events.EventRecorder
}

// Reconcile reconciles an ActionsGateway CR.
func (r *ActionsGatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ag gmcv1alpha1.ActionsGateway
	if err := r.Get(ctx, req.NamespacedName, &ag); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion.
	if !ag.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &ag)
	}

	// Ensure finalizer.
	if !controllerutil.ContainsFinalizer(&ag, finalizerName) {
		controllerutil.AddFinalizer(&ag, finalizerName)
		if err := r.Update(ctx, &ag); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Check that the referenced GitHub App Secret exists. If it has been deleted,
	// set CredentialUnavailable and stop — do not attempt to reconcile child resources
	// with a missing credential reference.
	var credSecret corev1.Secret
	credErr := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: ag.Spec.GitHubAppRef.Name}, &credSecret)
	if credErr != nil && !apierrors.IsNotFound(credErr) {
		return ctrl.Result{}, credErr
	}
	if apierrors.IsNotFound(credErr) {
		return r.setCredentialUnavailable(ctx, &ag, fmt.Sprintf("Secret %q not found in namespace %q", ag.Spec.GitHubAppRef.Name, ag.Namespace))
	}

	// Read GitHub IP ranges from the in-memory cache. Empty until the
	// periodic IPRangeReconciler completes its first fetch; tolerated
	// because IPRangeReconciler will patch the NetworkPolicy when it does.
	var githubCIDRs []net.IPNet
	if r.IPCache != nil {
		githubCIDRs = r.IPCache.Snapshot()
	}

	proxyAddr := buildProxyServiceAddr(&ag)

	if err := r.reconcileResources(ctx, &ag, githubCIDRs, proxyAddr); err != nil {
		return ctrl.Result{}, err
	}

	return r.updateStatus(ctx, &ag)
}

func (r *ActionsGatewayReconciler) reconcileResources(ctx context.Context, ag *gmcv1alpha1.ActionsGateway, githubCIDRs []net.IPNet, proxyAddr string) error {
	// Debug step markers: this method applies ~12 child resources and on failure
	// returns a single wrapped error, so a tenant stalled on one step (e.g. a
	// quota-blocked Deployment) gives no signal about which step is stuck. The
	// markers stay at V(1) (Debug) so steady-state reconciles add no Info volume.
	log := logf.FromContext(ctx)
	step := func(name string) { log.V(1).Info("reconcileResources step", "step", name) }

	// 0. Stamp Pod Security Admission labels on the tenant namespace.
	step("namespace PSA labels")
	if err := r.applyNamespacePSA(ctx, ag); err != nil {
		return fmt.Errorf("namespace PSA labels: %w", err)
	}

	// 1 & 2. ServiceAccounts.
	step("ServiceAccounts")
	if err := r.applyServiceAccount(ctx, buildAGCServiceAccount(ag)); err != nil {
		return fmt.Errorf("AGC ServiceAccount: %w", err)
	}
	if err := r.applyServiceAccount(ctx, buildWorkerServiceAccount(ag)); err != nil {
		return fmt.Errorf("worker ServiceAccount: %w", err)
	}

	// 3 & 4. RoleBinding — binds the AGC SA to the shipped agc-tenant-role ClusterRole.
	// (Pre-v0.X installs created a per-tenant Role here; reconcileDelete still GCs it.)
	step("AGC RoleBinding")
	if err := r.applyRoleBinding(ctx, buildAGCRoleBinding(ag)); err != nil {
		return fmt.Errorf("AGC RoleBinding: %w", err)
	}

	// The namespace ResourceQuota is platform-owned (Q130): the platform admin
	// provisions it on the tenant namespace and GAG operates within it. The GMC
	// no longer creates or mutates a ResourceQuota.

	// 7a. Proxy TLS cert Secret — must exist before the proxy Deployment references it.
	step("proxy TLS cert")
	if err := r.ensureProxyCert(ctx, ag); err != nil {
		return fmt.Errorf("proxy TLS cert: %w", err)
	}

	// 7b. Metrics mTLS bundle — the server Secret is mounted by both the proxy
	// and AGC Deployments, so it must exist before either is applied.
	step("metrics TLS certs")
	if err := r.ensureMetricsCerts(ctx, ag); err != nil {
		return fmt.Errorf("metrics TLS certs: %w", err)
	}

	// 7 & 8. Proxy Deployment + Service (before NetworkPolicy so we can read ClusterIP).
	step("proxy Deployment + Service")
	if err := r.applyDeployment(ctx, ag, buildProxyDeployment(ag, r.ProxyImage)); err != nil {
		return fmt.Errorf("proxy Deployment: %w", err)
	}
	if err := r.applyService(ctx, buildProxyService(ag)); err != nil {
		return fmt.Errorf("proxy Service: %w", err)
	}

	// 5. NetworkPolicies. The workload policy targets the proxy by PodSelector
	// (kube-proxy rewrites ClusterIP → PodIP before NetworkPolicy enforcement,
	// so an ipBlock on the ClusterIP would silently drop all proxy-bound traffic).
	step("NetworkPolicies")
	if err := r.applyNetworkPolicy(ctx, buildProxyNetworkPolicy(ag, githubCIDRs)); err != nil {
		return fmt.Errorf("proxy NetworkPolicy: %w", err)
	}
	if err := r.applyNetworkPolicy(ctx, buildWorkloadNetworkPolicy(ag)); err != nil {
		return fmt.Errorf("workload NetworkPolicy: %w", err)
	}
	if err := r.applyNetworkPolicy(ctx, buildAGCNetworkPolicy(ag)); err != nil {
		return fmt.Errorf("AGC NetworkPolicy: %w", err)
	}
	// Delete the legacy single-policy "actions-gateway" NetworkPolicy left by previous
	// versions. Best-effort during steady-state reconcile: a failure here is logged by
	// deleteIfExists and retried on the next reconcile, so it must not fail provisioning.
	_ = r.deleteIfExists(ctx, &networkingv1.NetworkPolicy{}, ag.Namespace, "actions-gateway")

	// 9. PDB.
	step("PodDisruptionBudget")
	if err := r.applyPDB(ctx, buildPDB(ag)); err != nil {
		return fmt.Errorf("PodDisruptionBudget: %w", err)
	}

	// 10. HPA.
	step("HorizontalPodAutoscaler")
	if err := r.applyHPA(ctx, buildHPA(ag)); err != nil {
		return fmt.Errorf("HorizontalPodAutoscaler: %w", err)
	}

	// 11. AGC Deployment.
	step("AGC Deployment")
	if err := r.applyDeployment(ctx, ag, buildAGCDeployment(ag, r.AGCImage, proxyAddr, r.AGCExtraEnv)); err != nil {
		return fmt.Errorf("AGC Deployment: %w", err)
	}

	// 12. RunnerGroup CRs. Apply the desired set, then prune any owned
	// RunnerGroup no longer in the spec (scale-down / reorder, Q101).
	step("RunnerGroups")
	desired := make(map[string]struct{}, len(ag.Spec.RunnerGroups))
	for i, spec := range ag.Spec.RunnerGroups {
		name := runnerGroupName(ag, spec, i)
		desired[name] = struct{}{}
		if err := r.applyRunnerGroup(ctx, buildRunnerGroup(ag, spec, name)); err != nil {
			return fmt.Errorf("RunnerGroup %s: %w", name, err)
		}
	}
	if err := r.pruneRunnerGroups(ctx, ag, desired); err != nil {
		return fmt.Errorf("prune RunnerGroups: %w", err)
	}

	return nil
}

// runnerGroupName derives the RunnerGroup CR name for a spec entry. A non-empty
// first runner label yields a stable, content-derived name; an unlabeled entry
// falls back to an index-based name. Pruning (pruneRunnerGroups) keys on the
// owner labels rather than this name, so converging the desired set is correct
// even when an entry is removed or reordered.
func runnerGroupName(ag *gmcv1alpha1.ActionsGateway, spec agcv1alpha1.RunnerGroupSpec, i int) string {
	if len(spec.RunnerLabels) > 0 {
		return fmt.Sprintf("%s-%s", ag.Name, labelSafe(spec.RunnerLabels[0]))
	}
	return fmt.Sprintf("%s-%d", ag.Name, i)
}

// pruneRunnerGroups deletes RunnerGroup CRs owned by this ActionsGateway that
// are no longer in the desired set — i.e. an entry removed from
// spec.RunnerGroups, or a group orphaned by a reorder under index-based naming.
// Without this, a removed/reordered RunnerGroup keeps running its listeners and
// worker pods until the entire ActionsGateway is deleted (Q101).
//
// The desired set is keyed by RunnerGroup name, so pruning is reorder-safe: a
// group whose name still appears is retained regardless of its position. The
// owner-label selector matches reconcileDelete's, so it never touches a
// RunnerGroup belonging to another ActionsGateway in the same namespace.
//
// Like reconcileDelete (Q125), this fails closed: every delete error other than
// NotFound is collected and returned so the reconcile requeues rather than
// leaving an orphan running. A NotFound is success — the group is already gone.
func (r *ActionsGatewayReconciler) pruneRunnerGroups(ctx context.Context, ag *gmcv1alpha1.ActionsGateway, desired map[string]struct{}) error {
	var rgList agcv1alpha1.RunnerGroupList
	sel := labels.SelectorFromSet(map[string]string{"actions-gateway/owner-name": ag.Name, "actions-gateway/owner-ns": ag.Namespace})
	if err := r.List(ctx, &rgList, &client.ListOptions{Namespace: ag.Namespace, LabelSelector: sel}); err != nil {
		return err
	}
	var errs []error
	for i := range rgList.Items {
		rg := &rgList.Items[i]
		if _, ok := desired[rg.Name]; ok {
			continue
		}
		if err := r.Delete(ctx, rg); client.IgnoreNotFound(err) != nil {
			logf.FromContext(ctx).Error(err, "failed to prune orphaned RunnerGroup", "namespace", rg.Namespace, "name", rg.Name)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *ActionsGatewayReconciler) reconcileDelete(ctx context.Context, ag *gmcv1alpha1.ActionsGateway) (ctrl.Result, error) {
	// 1. Delete RunnerGroup CRs and wait for them to be gone.
	var rgList agcv1alpha1.RunnerGroupList
	sel := labels.SelectorFromSet(map[string]string{"actions-gateway/owner-name": ag.Name, "actions-gateway/owner-ns": ag.Namespace})
	if err := r.List(ctx, &rgList, &client.ListOptions{Namespace: ag.Namespace, LabelSelector: sel}); err != nil {
		return ctrl.Result{}, err
	}
	if len(rgList.Items) > 0 {
		for i := range rgList.Items {
			if err := r.Delete(ctx, &rgList.Items[i]); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	ns := ag.Namespace

	// Fail closed (Q125): collect every delete error and DO NOT remove the
	// finalizer while any teardown delete is unconfirmed. A NotFound is success
	// (the resource is already gone); any other error retains the finalizer and
	// requeues, so a transient API failure cannot orphan a live, credentialed
	// AGC Deployment + RoleBinding after offboarding. Each pass is idempotent:
	// already-deleted resources return NotFound and converge to clean.
	var errs []error
	del := func(obj client.Object, name string) {
		if err := r.deleteIfExists(ctx, obj, ns, name); err != nil {
			errs = append(errs, err)
		}
	}

	// 2. AGC Deployment.
	del(&appsv1.Deployment{}, agcAppName)
	// 3. HPA, PDB, proxy Service, proxy Deployment.
	// The proxy TLS cert Secret has an owner reference on the ActionsGateway CR; GC
	// handles its cleanup automatically when the CR is deleted, so no explicit delete
	// is needed here (and GMC does not have delete permission on secrets).
	del(&autoscalingv2.HorizontalPodAutoscaler{}, proxyServiceName)
	del(&policyv1.PodDisruptionBudget{}, proxyServiceName)
	del(&corev1.Service{}, proxyServiceName)
	del(&appsv1.Deployment{}, proxyServiceName)
	// 4. NetworkPolicies. The namespace ResourceQuota is platform-owned (Q130) —
	// the GMC neither creates nor deletes it. A ResourceQuota left over from a
	// pre-Q130 install (ownerRef → this ActionsGateway) is garbage-collected by
	// Kubernetes when the CR is deleted; the GMC holds no quota delete verb.
	del(&networkingv1.NetworkPolicy{}, npProxyName)
	del(&networkingv1.NetworkPolicy{}, npAGCName)
	del(&networkingv1.NetworkPolicy{}, npWorkloadName)
	del(&networkingv1.NetworkPolicy{}, "actions-gateway") // legacy
	// 5. RoleBinding, Role.
	del(&rbacv1.RoleBinding{}, agcSAName)
	del(&rbacv1.Role{}, agcSAName)
	// 6. ServiceAccounts.
	del(&corev1.ServiceAccount{}, agcSAName)
	del(&corev1.ServiceAccount{}, workerSAName)

	if len(errs) > 0 {
		err := errors.Join(errs...)
		if r.Recorder != nil {
			r.Recorder.Eventf(ag, nil, corev1.EventTypeWarning, "TeardownIncomplete", "ReconcileDelete",
				"Tenant teardown could not confirm deletion of all owned resources in namespace %q; retaining the cleanup finalizer and requeuing until teardown is clean: %v",
				ns, err)
		}
		// Returning the error requeues with backoff. The finalizer stays in place,
		// so the partially-torn-down tenant (including any live AGC Deployment) is
		// not abandoned.
		return ctrl.Result{}, err
	}

	// All owned resources are confirmed gone — remove the finalizer so the CR can
	// be garbage-collected.
	if err := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: ag.Name}, ag); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	controllerutil.RemoveFinalizer(ag, finalizerName)
	return ctrl.Result{}, r.Update(ctx, ag)
}

// deleteIfExists issues a best-effort delete of a single namespaced resource.
// A NotFound result is success (the resource is already gone — the desired
// teardown end state); any other error is logged and returned so the caller
// can decide whether to retain the finalizer and requeue (see reconcileDelete,
// Q125). Callers that treat the delete as fire-and-forget may ignore the
// return value.
func (r *ActionsGatewayReconciler) deleteIfExists(ctx context.Context, obj client.Object, ns, name string) error {
	obj.SetNamespace(ns)
	obj.SetName(name)
	if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
		logf.FromContext(ctx).Error(err, "failed to delete resource during teardown", "namespace", ns, "name", name)
		return err
	}
	return nil
}

func (r *ActionsGatewayReconciler) updateStatus(ctx context.Context, ag *gmcv1alpha1.ActionsGateway) (ctrl.Result, error) {
	var proxyDep appsv1.Deployment
	proxyReady := int32(0)
	if err := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: proxyServiceName}, &proxyDep); err == nil {
		proxyReady = proxyDep.Status.ReadyReplicas
	}

	var agcDep appsv1.Deployment
	agcReady := false
	if err := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: agcAppName}, &agcDep); err == nil {
		agcReady = agcDep.Status.ReadyReplicas >= 1
	}

	minReplicas := int32(2)
	if ag.Spec.Proxy.MinReplicas != nil {
		minReplicas = *ag.Spec.Proxy.MinReplicas
	}

	proxyAvailable := proxyReady >= minReplicas
	agcAvailable := agcReady

	now := metav1.Now()
	gen := ag.Generation

	setCondition := func(condType string, status bool, reason, msg string) {
		s := metav1.ConditionFalse
		if status {
			s = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             s,
			Reason:             reason,
			Message:            msg,
			LastTransitionTime: now,
			ObservedGeneration: gen,
		})
	}

	// Credential is present (checked before entering reconcileResources).
	setCondition("CredentialUnavailable", false, "SecretFound", "referenced Secret exists")
	setCondition("ProxyAvailable", proxyAvailable, "ProxyReady", fmt.Sprintf("%d/%d proxy pods ready", proxyReady, minReplicas))
	setCondition("AGCAvailable", agcAvailable, "AGCReady", "AGC deployment status")
	setCondition("Ready", proxyAvailable && agcAvailable, "AllAvailable", "all components are available")

	ag.Status.ProxyReadyReplicas = proxyReady
	ag.Status.ObservedGeneration = gen

	if err := r.Status().Update(ctx, ag); err != nil {
		if apierrors.IsConflict(err) {
			// Our in-memory ag is stale; requeue to re-Get and recompute rather
			// than silently dropping the status write.
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	// If not all components are ready yet, re-check after a short delay.
	// The Owns(&appsv1.Deployment{}) watch will trigger a faster reconcile when
	// Deployment status changes, but this requeue guards against missed events.
	if !proxyAvailable || !agcAvailable {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// setCredentialUnavailable sets CredentialUnavailable=True and Ready=False on the
// ActionsGateway. It returns RequeueAfter: 30s as a fallback so the reconciler
// periodically re-checks even if a watch event is missed (e.g. on controller
// restart). The WatchesMetadata watch on Secrets typically re-triggers faster.
func (r *ActionsGatewayReconciler) setCredentialUnavailable(ctx context.Context, ag *gmcv1alpha1.ActionsGateway, msg string) (ctrl.Result, error) {
	now := metav1.Now()
	gen := ag.Generation
	meta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
		Type:               "CredentialUnavailable",
		Status:             metav1.ConditionTrue,
		Reason:             "SecretNotFound",
		Message:            msg,
		LastTransitionTime: now,
		ObservedGeneration: gen,
	})
	meta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "CredentialUnavailable",
		Message:            msg,
		LastTransitionTime: now,
		ObservedGeneration: gen,
	})
	if err := r.Status().Update(ctx, ag); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// secretToActionsGateway maps a Secret event to any ActionsGateway in the same
// namespace that references the Secret by name. Used by WatchesMetadata so that
// creating or deleting a referenced credential Secret triggers reconciliation.
// The handler receives PartialObjectMetadata (name/namespace only — no .data),
// which is safe to cache; actual Secret content is read via a direct API server
// call in Reconcile (bypassing the cache via the manager's DisableFor setting).
func (r *ActionsGatewayReconciler) secretToActionsGateway(ctx context.Context, obj client.Object) []ctrl.Request {
	var agList gmcv1alpha1.ActionsGatewayList
	if err := r.List(ctx, &agList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for _, ag := range agList.Items {
		if ag.Spec.GitHubAppRef.Name == obj.GetName() {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: ag.Namespace,
				Name:      ag.Name,
			}})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *ActionsGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gmcv1alpha1.ActionsGateway{}).
		// The reconciler mutates per-tenant in-memory state and assumes a single
		// writer; serialise it explicitly rather than relying on the implicit
		// default. Raising this requires making reconcileResources/apply* helpers
		// safe under concurrent reconciles of distinct ActionsGateways first.
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Owns(&appsv1.Deployment{}).
		// WatchesMetadata caches only ObjectMeta (name, namespace, resourceVersion)
		// for Secrets — no .data ever enters the in-process cache. This gives
		// event-driven reconciliation on credential Secret create/delete without
		// buffering secret material in memory.
		WatchesMetadata(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToActionsGateway),
		).
		Named("actionsgateway").
		Complete(r)
}

// The apply* helpers below all use controllerutil.CreateOrPatch rather than a
// hand-rolled Get→mutate→Update. CreateOrPatch re-reads the object inside the
// call, runs the mutate closure to set only the controller-managed fields, and
// writes a minimal merge patch (creating the object when absent). This is
// conflict-safe: a concurrent writer touching unmanaged fields is neither
// clobbered (the patch only carries the fields we set) nor forces an avoidable
// IsConflict on this reconcile, which the previous full-object Update could.
// applyNamespacePSA stays on Server-Side Apply (it already detects and reports
// out-of-band PSA-label edits via field-manager conflicts).

// errRoleRefImmutable signals that an existing RoleBinding references a
// different (immutable) roleRef than desired, so it must be deleted and
// recreated rather than patched. It never escapes applyRoleBinding.
var errRoleRefImmutable = errors.New("rolebinding roleRef changed; recreate required")

// applyServiceAccount creates the ServiceAccount or patches its managed labels.
func (r *ActionsGatewayReconciler) applyServiceAccount(ctx context.Context, desired *corev1.ServiceAccount) error {
	obj := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		return nil
	})
	return err
}

func (r *ActionsGatewayReconciler) applyRoleBinding(ctx context.Context, desired *rbacv1.RoleBinding) error {
	obj := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		// roleRef is immutable. On upgrade (pre-v0.X bindings reference the
		// per-tenant Role; new bindings reference the agc-tenant-role
		// ClusterRole) the binding must be deleted and recreated — a patch would
		// be rejected. A non-empty ResourceVersion means obj already existed
		// (CreateOrPatch populated it from the live object).
		if obj.ResourceVersion != "" && obj.RoleRef != desired.RoleRef {
			return errRoleRefImmutable
		}
		obj.Labels = desired.Labels
		obj.RoleRef = desired.RoleRef
		obj.Subjects = desired.Subjects
		return nil
	})
	if errors.Is(err, errRoleRefImmutable) {
		if delErr := r.Delete(ctx, obj); delErr != nil && !apierrors.IsNotFound(delErr) {
			return delErr
		}
		return r.Create(ctx, desired)
	}
	return err
}

func (r *ActionsGatewayReconciler) applyNetworkPolicy(ctx context.Context, desired *networkingv1.NetworkPolicy) error {
	obj := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		obj.Spec = desired.Spec
		return nil
	})
	return err
}

// applyDeployment creates or patches a Deployment and sets an owner reference so
// that the Owns(&appsv1.Deployment{}) watch on the controller fires when the
// Deployment's status changes (e.g. ReadyReplicas increases after pod startup).
func (r *ActionsGatewayReconciler) applyDeployment(ctx context.Context, ag *gmcv1alpha1.ActionsGateway, desired *appsv1.Deployment) error {
	obj := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		obj.Spec = desired.Spec
		return controllerutil.SetControllerReference(ag, obj, r.Scheme)
	})
	return err
}

func (r *ActionsGatewayReconciler) applyService(ctx context.Context, desired *corev1.Service) error {
	obj := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		// Set only controller-managed Spec fields: ClusterIP and other
		// server-assigned fields on an existing Service must be preserved.
		obj.Spec.Type = desired.Spec.Type
		obj.Spec.Selector = desired.Spec.Selector
		obj.Spec.Ports = desired.Spec.Ports
		return nil
	})
	return err
}

func (r *ActionsGatewayReconciler) applyPDB(ctx context.Context, desired *policyv1.PodDisruptionBudget) error {
	obj := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		obj.Spec = desired.Spec
		return nil
	})
	return err
}

func (r *ActionsGatewayReconciler) applyHPA(ctx context.Context, desired *autoscalingv2.HorizontalPodAutoscaler) error {
	obj := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		obj.Spec = desired.Spec
		return nil
	})
	return err
}

func (r *ActionsGatewayReconciler) applyRunnerGroup(ctx context.Context, desired *agcv1alpha1.RunnerGroup) error {
	obj := &agcv1alpha1.RunnerGroup{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		obj.Spec = desired.Spec
		return nil
	})
	return err
}

// ensureProxyCert ensures the proxy TLS Secret exists and contains a cert valid for
// at least proxyCertRenewBefore. It generates (or re-generates) a self-signed cert
// when the Secret is missing, unparseable, or nearing expiry. RSA key generation is
// done by the GMC so the private key never leaves the cluster — it is stored in the
// Secret and mounted read-only into the proxy pod. The AGC receives only the public
// cert (tls.crt) via a separate Items-projected volume mount.
func (r *ActionsGatewayReconciler) ensureProxyCert(ctx context.Context, ag *gmcv1alpha1.ActionsGateway) error {
	var existing corev1.Secret
	getErr := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: proxyTLSSecretName}, &existing)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return getErr
	}

	// reason records why the cert is being (re)issued, for the Debug audit below.
	// This branch is otherwise silent: a renewal that runs on every reconcile (the
	// cert keeps landing within the renew-before window) gives no operator signal.
	// No key material is logged — only the Secret name, reason, and expiry.
	reason := "secret missing"
	if !apierrors.IsNotFound(getErr) {
		reason = "unparseable cert"
		if cert, err := parseCertPEM(existing.Data[corev1.TLSCertKey]); err == nil {
			if time.Until(cert.NotAfter) > proxyCertRenewBefore {
				return nil // cert is valid and not near expiry
			}
			reason = "near expiry"
		}
	}

	logf.FromContext(ctx).V(1).Info("issuing proxy TLS cert", "secret", proxyTLSSecretName, "reason", reason)

	certPEM, keyPEM, err := generateProxyCert(ag)
	if err != nil {
		return fmt.Errorf("generate proxy cert: %w", err)
	}

	return r.applyOwnedSecret(ctx, ag, buildProxyCertSecret(ag, certPEM, keyPEM))
}

// ensureMetricsCerts ensures the per-tenant metrics mTLS bundle exists and is not
// near expiry. It (re)generates a CA + server cert + scraper client cert and writes
// two Secrets: the server bundle (metricsTLSSecretName, mounted into the AGC and
// proxy) and the scraper client bundle (metricsClientSecretName, published for the
// monitoring stack). Both carry an owner reference on the ActionsGateway so they
// are garbage-collected on delete (the GMC has no Secret delete permission). The
// whole bundle is regenerated together when either Secret is missing or the server
// cert is within metricsCertRenewBefore of expiry — mirroring ensureProxyCert.
func (r *ActionsGatewayReconciler) ensureMetricsCerts(ctx context.Context, ag *gmcv1alpha1.ActionsGateway) error {
	var serverSec corev1.Secret
	serverErr := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: metricsTLSSecretName}, &serverSec)
	if serverErr != nil && !apierrors.IsNotFound(serverErr) {
		return serverErr
	}
	var clientSec corev1.Secret
	clientErr := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: metricsClientSecretName}, &clientSec)
	if clientErr != nil && !apierrors.IsNotFound(clientErr) {
		return clientErr
	}

	// reason records why the bundle is being (re)generated, for the Debug audit
	// below — mirroring ensureProxyCert. The renewal path is otherwise silent. No
	// key material is logged — only the Secret name, reason, and (implicit) expiry.
	reason := "secret missing"
	if !apierrors.IsNotFound(serverErr) && !apierrors.IsNotFound(clientErr) {
		reason = "unparseable cert"
		if cert, err := parseCertPEM(serverSec.Data[corev1.TLSCertKey]); err == nil {
			if time.Until(cert.NotAfter) > metricsCertRenewBefore {
				return nil // both Secrets present and the server cert is not near expiry
			}
			reason = "near expiry"
		}
	}

	logf.FromContext(ctx).V(1).Info("generating metrics mTLS bundle", "secret", metricsTLSSecretName, "reason", reason)

	bundle, err := generateMetricsCerts(ag)
	if err != nil {
		return fmt.Errorf("generate metrics certs: %w", err)
	}

	if err := r.applyOwnedSecret(ctx, ag, buildMetricsTLSSecret(ag, bundle)); err != nil {
		return fmt.Errorf("metrics server Secret: %w", err)
	}
	if err := r.applyOwnedSecret(ctx, ag, buildMetricsClientSecret(ag, bundle)); err != nil {
		return fmt.Errorf("metrics client Secret: %w", err)
	}
	return nil
}

// applyOwnedSecret creates or patches a Secret, setting an owner reference on
// the ActionsGateway so the Secret is GC'd on CR delete. Like the other apply*
// helpers it uses CreateOrPatch and writes only the controller-managed
// type/data/labels, so a concurrent writer is not clobbered.
func (r *ActionsGatewayReconciler) applyOwnedSecret(ctx context.Context, ag *gmcv1alpha1.ActionsGateway, desired *corev1.Secret) error {
	obj := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		obj.Type = desired.Type
		obj.Data = desired.Data
		return controllerutil.SetControllerReference(ag, obj, r.Scheme)
	})
	return err
}

// psaFieldManager is the Server-Side Apply field manager that owns the PSA
// label keys on tenant namespaces. A distinct manager (rather than the
// controller-runtime default) lets an out-of-band edit by an administrator
// be detected as a conflict on the next reconcile.
const psaFieldManager = "actionsgateway-controller-psa"

// TenantNamespaceMarkerLabel is the label a trusted administrator must apply to a
// tenant namespace to mark it as managed by the GMC. The namespace-psa-guard
// ValidatingAdmissionPolicy (config/admission-policy/namespace-psa-guard.yaml)
// denies the GMC ServiceAccount any namespace patch unless the existing namespace
// already carries this label set to "true" — confining the GMC's cluster-wide
// namespaces:patch grant to managed tenants so a compromised GMC cannot relabel
// kube-system PSA (k8s best-practices audit finding B2 / Queue Q56). The GMC never
// sets this label itself; doing so would defeat the control.
const TenantNamespaceMarkerLabel = "actions-gateway.github.com/tenant"

// applyNamespacePSA stamps Pod Security Admission labels on the tenant namespace
// using Server-Side Apply so the controller declares ownership only of the six
// PSA label keys. Other labels on the namespace are left untouched.
//
// The function first applies without ForceOwnership so a conflict surfaces if
// another manager (typically a human admin) has taken ownership of a PSA key
// — that conflict is emitted as a Warning Event on the ActionsGateway and the
// apply is retried with ForceOwnership to re-establish the controller's
// invariant. Without this, admin edits would silently round-trip every
// reconcile with no operator-visible signal.
func (r *ActionsGatewayReconciler) applyNamespacePSA(ctx context.Context, ag *gmcv1alpha1.ActionsGateway) error {
	profile := securityProfileOrDefault(ag.Spec.SecurityProfile)
	desired := corev1ac.Namespace(ag.Namespace).WithLabels(map[string]string{
		"pod-security.kubernetes.io/enforce":         profile,
		"pod-security.kubernetes.io/enforce-version": "latest",
		"pod-security.kubernetes.io/warn":            profile,
		"pod-security.kubernetes.io/warn-version":    "latest",
		"pod-security.kubernetes.io/audit":           profile,
		"pod-security.kubernetes.io/audit-version":   "latest",
	})

	err := r.Apply(ctx, desired, client.FieldOwner(psaFieldManager))
	if err == nil {
		return nil
	}
	if apierrors.IsForbidden(err) {
		// The namespace-psa-guard ValidatingAdmissionPolicy denied the patch —
		// almost always because the tenant namespace was never labeled as managed.
		// Surface an actionable signal rather than an opaque reconcile error; a
		// retry will not help until the operator applies the marker label.
		if r.Recorder != nil {
			r.Recorder.Eventf(ag, nil, corev1.EventTypeWarning, "NamespaceMarkerMissing", "ApplyPSALabels",
				"Cannot stamp Pod Security Admission labels on namespace %q: the admission policy denied the update. Label the namespace with %s=true to mark it a managed tenant namespace, then reconciliation will proceed: %v",
				ag.Namespace, TenantNamespaceMarkerLabel, err)
		}
		return err
	}
	if !apierrors.IsConflict(err) {
		return err
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(ag, nil, corev1.EventTypeWarning, "PSALabelsOverridden", "ReapplyPSALabels",
			"Reconciling Pod Security Admission labels on namespace %q to SecurityProfile=%q after detecting an out-of-band modification: %v",
			ag.Namespace, profile, err)
	}
	return r.Apply(ctx, desired, client.FieldOwner(psaFieldManager), client.ForceOwnership)
}

// labelSafe converts a string to a safe Kubernetes DNS label segment and appends
// a 7-char SHA-256 hex suffix so that distinct inputs always produce distinct
// outputs, even when they share the same sanitized prefix (e.g. "gpu/a100" vs
// "gpu_a100" both sanitize to "gpu-a100").
func labelSafe(s string) string {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(s)))[:7]
	out := make([]byte, 0, len(s))
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			out = append(out, c)
		} else if c >= 'A' && c <= 'Z' {
			out = append(out, c+32)
		} else {
			out = append(out, '-')
		}
	}
	seg := strings.Trim(string(out), "-")
	if len(seg) > 40 {
		seg = strings.TrimRight(seg[:40], "-")
	}
	if seg == "" {
		seg = "label"
	}
	return seg + "-" + hash
}
