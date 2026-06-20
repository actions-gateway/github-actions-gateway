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
// It does hold read-only access to resourcequotas so updateStatus can correlate
// the proxy pool's configured maxReplicas against the platform-owned quota and
// surface the ProxyQuotaPressure condition (Q82) — read-only preserves the
// least-privilege posture (it cannot create or mutate the quota).
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch
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
// servicemonitors: the GMC creates a per-tenant Prometheus-Operator
// ServiceMonitor for the proxy and AGC mTLS metrics ports (Q72). This grant is
// always present in the ClusterRole, but is only exercised when an operator
// opts in via metrics.serviceMonitor.enabled (--enable-tenant-service-monitors)
// AND the monitoring.coreos.com CRD is installed; the reconciler skips
// gracefully when the CRD is absent so the unused grant is harmless.
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete

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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
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
	// EnableTenantServiceMonitors gates creation of the per-tenant
	// Prometheus-Operator ServiceMonitors that scrape the proxy/AGC mTLS metrics
	// ports (Q72). Off by default: the monitoring.coreos.com CRD is an optional,
	// operator-installed prerequisite, so creating ServiceMonitors unconditionally
	// would break provisioning on clusters without it. The chart wires this from
	// metrics.serviceMonitor.enabled. When off, the metrics Services are still
	// created (they are always correct and dependency-free) and any
	// previously-created ServiceMonitors are pruned.
	EnableTenantServiceMonitors bool
	// APIServerCIDRs, when non-empty, scopes the AGC NetworkPolicy's apiserver
	// (443/6443) egress rule to these CIDRs via ipBlock peers instead of allowing
	// any destination (Q145). It is a platform/operator opt-in tightening wired
	// from the GMC's --apiserver-cidrs flag (chart value apiServerCIDRs); the
	// entries are validated as CIDRs at startup. Empty (the default) preserves the
	// any-destination behavior required where the post-DNAT apiserver IP is not
	// predictable. See buildAGCNetworkPolicy.
	APIServerCIDRs []string
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
		// Reconcile returns here before updateStatus, so without this the object's
		// conditions would go stale and never reflect the failure. Record a Degraded
		// condition naming the failing step before the early return (Q156).
		return r.setDegraded(ctx, &ag, err)
	}

	return r.updateStatus(ctx, &ag)
}

// provisioningError wraps a reconcileResources failure with the named step that
// was in progress when it failed, so Reconcile can surface that step in the
// Degraded condition before returning early (Q156). It Unwraps to the underlying
// error so errors.Is/As and IsConflict-style checks still see the cause.
type provisioningError struct {
	step string
	err  error
}

func (e *provisioningError) Error() string { return fmt.Sprintf("%s: %v", e.step, e.err) }
func (e *provisioningError) Unwrap() error { return e.err }

func (r *ActionsGatewayReconciler) reconcileResources(ctx context.Context, ag *gmcv1alpha1.ActionsGateway, githubCIDRs []net.IPNet, proxyAddr string) (retErr error) {
	// Debug step markers: this method applies ~12 child resources and on failure
	// returns a single wrapped error, so a tenant stalled on one step (e.g. a
	// quota-blocked Deployment) gives no signal about which step is stuck. The
	// markers stay at V(1) (Debug) so steady-state reconciles add no Info volume.
	//
	// step() also records the in-progress step name; the deferred wrap tags any
	// returned error with it so Reconcile can name the failing step in the
	// Degraded condition without parsing error strings (Q156).
	log := logf.FromContext(ctx)
	var current string
	step := func(name string) {
		current = name
		log.V(1).Info("reconcileResources step", "step", name)
	}
	defer func() {
		if retErr != nil {
			retErr = &provisioningError{step: current, err: retErr}
		}
	}()

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
	// AGC metrics Service — the AGC has no other Service; this exposes its mTLS
	// metrics port (:8443) so a ServiceMonitor can scrape it (Q72).
	if err := r.applyService(ctx, buildAGCService(ag)); err != nil {
		return fmt.Errorf("AGC Service: %w", err)
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
	if err := r.applyNetworkPolicy(ctx, buildAGCNetworkPolicy(ag, r.APIServerCIDRs)); err != nil {
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

	// 11b. Per-tenant ServiceMonitors for the proxy/AGC mTLS metrics ports (Q72).
	// Opt-in (EnableTenantServiceMonitors): applied when enabled, pruned when not.
	step("ServiceMonitors")
	if err := r.applyOrPruneServiceMonitors(ctx, ag); err != nil {
		return fmt.Errorf("ServiceMonitors: %w", err)
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
	// delServiceMonitor deletes a per-tenant ServiceMonitor (unstructured, Q72).
	// A missing monitoring.coreos.com CRD (the operator never opted in) yields a
	// NoMatch error, which is success here — there is nothing to tear down.
	delServiceMonitor := func(name string) {
		sm := &unstructured.Unstructured{}
		sm.SetGroupVersionKind(serviceMonitorGVK)
		if err := r.deleteIfExists(ctx, sm, ns, name); err != nil && !meta.IsNoMatchError(err) {
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
	del(&corev1.Service{}, agcAppName) // AGC metrics Service (Q72)
	del(&appsv1.Deployment{}, proxyServiceName)
	// Per-tenant ServiceMonitors (Q72). They carry an owner reference so the
	// garbage collector also removes them, but delete explicitly so teardown is
	// confirmed fail-closed like the rest. A missing monitoring.coreos.com CRD
	// (never opted in) is treated as success — there is nothing to delete.
	delServiceMonitor(proxyServiceMonitorName)
	delServiceMonitor(agcServiceMonitorName)
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

	// Reaching updateStatus means reconcileResources succeeded, so clear any
	// Degraded condition a prior failed reconcile left set (Q156). Credential is
	// present (checked before entering reconcileResources).
	setCondition(gmcv1alpha1.ConditionDegraded, false, gmcv1alpha1.ReasonReconcileSucceeded, "all child resources reconciled")
	setCondition(gmcv1alpha1.ConditionCredentialUnavailable, false, gmcv1alpha1.ReasonSecretFound, "referenced Secret exists")
	setCondition(gmcv1alpha1.ConditionProxyAvailable, proxyAvailable, gmcv1alpha1.ReasonProxyReady, fmt.Sprintf("%d/%d proxy pods ready", proxyReady, minReplicas))
	setCondition(gmcv1alpha1.ConditionAGCAvailable, agcAvailable, gmcv1alpha1.ReasonAGCReady, "AGC deployment status")
	// ProxyQuota{Pressure,Exceeded} are advisory (Q82) and do NOT gate Ready — the
	// pool keeps serving at its current scale. Pressure (warning) is predictive:
	// the pool can't grow to maxReplicas within current quota headroom. Exceeded
	// (error) is observed: replica creates are being rejected by quota now. They
	// are mutually exclusive (error supersedes warning).
	qc := r.evalProxyQuota(ctx, ag, &proxyDep)
	setCondition(gmcv1alpha1.ConditionProxyQuotaPressure, qc.pressure, qc.pressureReason, qc.pressureMessage)
	setCondition(gmcv1alpha1.ConditionProxyQuotaExceeded, qc.exceeded, qc.exceededReason, qc.exceededMessage)
	setCondition(gmcv1alpha1.ConditionReady, proxyAvailable && agcAvailable, gmcv1alpha1.ReasonAllAvailable, "all components are available")

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

// quotaCheck maps one proxy-footprint resource to the ResourceQuota hard key
// that constrains it. A footprint resource may be capped by more than one quota
// key (e.g. CPU requests are limited by both "requests.cpu" and the legacy
// "cpu" alias), so the slice can hold multiple entries per footprint key.
type quotaCheck struct {
	footprint corev1.ResourceName // key into the footprint ResourceList
	hardKey   corev1.ResourceName // ResourceQuota .Hard key it is measured against
}

var proxyQuotaChecks = []quotaCheck{
	{corev1.ResourcePods, corev1.ResourcePods},
	{corev1.ResourceRequestsCPU, corev1.ResourceRequestsCPU},
	{corev1.ResourceRequestsCPU, corev1.ResourceCPU}, // legacy alias for requests.cpu
	{corev1.ResourceRequestsMemory, corev1.ResourceRequestsMemory},
	{corev1.ResourceRequestsMemory, corev1.ResourceMemory}, // legacy alias for requests.memory
	{corev1.ResourceLimitsCPU, corev1.ResourceLimitsCPU},
	{corev1.ResourceLimitsMemory, corev1.ResourceLimitsMemory},
}

// proxyFootprint returns the quota footprint of `replicas` proxy pods: the
// per-replica requests/limits scaled by replicas, plus the pod count. Keys
// mirror ResourceQuota hard keys (pods, requests.cpu, requests.memory,
// limits.cpu, limits.memory). It is linear in replicas, so the *additional*
// demand to grow the pool from m to n pods is proxyFootprint(ag, n-m).
func proxyFootprint(ag *gmcv1alpha1.ActionsGateway, replicas int32) corev1.ResourceList {
	if replicas < 0 {
		replicas = 0
	}
	res := proxyResources(ag)
	out := corev1.ResourceList{
		corev1.ResourcePods: *resource.NewQuantity(int64(replicas), resource.DecimalSI),
	}
	if q, ok := res.Requests[corev1.ResourceCPU]; ok {
		out[corev1.ResourceRequestsCPU] = mulQuantity(q, int64(replicas))
	}
	if q, ok := res.Requests[corev1.ResourceMemory]; ok {
		out[corev1.ResourceRequestsMemory] = mulQuantity(q, int64(replicas))
	}
	if q, ok := res.Limits[corev1.ResourceCPU]; ok {
		out[corev1.ResourceLimitsCPU] = mulQuantity(q, int64(replicas))
	}
	if q, ok := res.Limits[corev1.ResourceMemory]; ok {
		out[corev1.ResourceLimitsMemory] = mulQuantity(q, int64(replicas))
	}
	return out
}

// mulQuantity returns q multiplied by n. n is bounded by the CRD's
// proxy.maxReplicas maximum (100), so repeated addition stays cheap and exact
// (resource.Quantity has no scalar-multiply primitive that preserves the
// canonical form across both DecimalSI and BinarySI).
func mulQuantity(q resource.Quantity, n int64) resource.Quantity {
	out := resource.Quantity{Format: q.Format}
	for i := int64(0); i < n; i++ {
		out.Add(q)
	}
	return out
}

func proxyMaxReplicas(ag *gmcv1alpha1.ActionsGateway) int32 {
	if ag.Spec.Proxy.MaxReplicas != nil {
		return *ag.Spec.Proxy.MaxReplicas
	}
	return 10
}

// proxyQuotaConditions carries the computed status of the two namespace-quota
// conditions for the proxy pool. They follow the project's two-tier convention
// (see docs/development/kubernetes-conventions.md): a warning tier
// (ProxyQuotaPressure) and an error tier (ProxyQuotaExceeded), mutually
// exclusive — the error supersedes the warning.
type proxyQuotaConditions struct {
	pressure        bool
	pressureReason  string
	pressureMessage string
	exceeded        bool
	exceededReason  string
	exceededMessage string
}

// evalProxyQuota computes the ProxyQuotaPressure (warning) and ProxyQuotaExceeded
// (error) conditions against the platform-owned namespace ResourceQuota (Q130).
// Both are advisory and do NOT gate Ready — the pool keeps serving at its current
// scale.
//
//   - ProxyQuotaExceeded (error): the proxy Deployment is *currently* having
//     replica creates rejected by quota — read from its own ReplicaFailure
//     condition, the authoritative runtime signal.
//   - ProxyQuotaPressure (warning): predictive. Given the current remaining quota
//     headroom (hard − used), the pool cannot grow from its current replica count
//     up to maxReplicas. This is dynamic (it moves as worker pods consume and
//     release quota), so it is a warning, not a page.
//
// The headroom check ignores quota scopes (e.g. PriorityClass-scoped quotas may
// not count the proxy pods); face-value hard/used is sufficient for an advisory
// signal and avoids false precision.
func (r *ActionsGatewayReconciler) evalProxyQuota(ctx context.Context, ag *gmcv1alpha1.ActionsGateway, proxyDep *appsv1.Deployment) proxyQuotaConditions {
	st := proxyQuotaConditions{
		pressureReason:  "QuotaHeadroomSufficient",
		pressureMessage: "namespace ResourceQuota admits scaling the proxy pool to maxReplicas",
		exceededReason:  "NoRejection",
		exceededMessage: "proxy replica creation is not being rejected by the namespace ResourceQuota",
	}

	// Error tier — observed rejection. The Deployment surfaces ReplicaFailure when
	// its ReplicaSet cannot create pods; a quota rejection carries an "exceeded
	// quota" message, which distinguishes it from other create failures (PSA,
	// image, scheduling).
	if proxyDep != nil {
		if c := findDeploymentCondition(proxyDep, appsv1.DeploymentReplicaFailure); c != nil &&
			c.Status == corev1.ConditionTrue && strings.Contains(c.Message, "exceeded quota") {
			st.exceeded = true
			st.exceededReason = "ReplicasRejected"
			st.exceededMessage = "proxy replica creation is being rejected by the namespace ResourceQuota: " + c.Message
		}
	}

	// Warning tier — predictive headroom check.
	var quotas corev1.ResourceQuotaList
	if err := r.List(ctx, &quotas, client.InNamespace(ag.Namespace)); err != nil {
		st.pressureReason = "QuotaUnknown"
		st.pressureMessage = fmt.Sprintf("could not read namespace ResourceQuota: %v", err)
	} else if len(quotas.Items) == 0 {
		st.pressureReason = "NoQuota"
		st.pressureMessage = "no namespace ResourceQuota constrains the proxy pool"
	} else {
		current := int32(0)
		if proxyDep != nil {
			current = proxyDep.Status.Replicas
		}
		additional := proxyMaxReplicas(ag) - current
		if additional > 0 {
			demand := proxyFootprint(ag, additional)
			st.pressure, st.pressureMessage = quotaHeadroomViolations(
				demand, quotas.Items, proxyQuotaChecks,
				"proxy cannot scale to maxReplicas with current quota headroom: ")
			if st.pressure {
				st.pressureReason = "InsufficientQuotaHeadroom"
			}
		}
	}

	// Error supersedes warning: while replicas are actively rejected, the
	// headroom warning is redundant noise. Keep the two conditions mutually
	// exclusive so each maps cleanly to one alert severity.
	if st.exceeded {
		st.pressure = false
		st.pressureReason = "Superseded"
		st.pressureMessage = "superseded by ProxyQuotaExceeded"
	}
	return st
}

// quotaHeadroomViolations reports whether `demand` exceeds the remaining headroom
// (hard − used) of any quota for any mapped resource, and a human-readable
// message. The bool is true when at least one resource is over headroom.
func quotaHeadroomViolations(demand corev1.ResourceList, quotas []corev1.ResourceQuota, checks []quotaCheck, msgPrefix string) (bool, string) {
	var violations []string
	for i := range quotas {
		q := &quotas[i]
		hard := q.Status.Hard
		if len(hard) == 0 {
			// Status not yet populated by the quota controller (e.g. just after
			// the admin edits .spec.hard); fall back to the requested cap.
			hard = q.Spec.Hard
		}
		for _, c := range checks {
			need, ok := demand[c.footprint]
			if !ok {
				continue
			}
			limit, ok := hard[c.hardKey]
			if !ok {
				continue
			}
			remaining := limit.DeepCopy()
			if u, ok := q.Status.Used[c.hardKey]; ok {
				remaining.Sub(u)
			}
			if need.Cmp(remaining) > 0 {
				violations = append(violations, fmt.Sprintf(
					"needs %s more %s but quota %q has %s free", need.String(), c.hardKey, q.Name, remaining.String()))
			}
		}
	}
	if len(violations) == 0 {
		return false, ""
	}
	return true, msgPrefix + strings.Join(violations, "; ")
}

// findDeploymentCondition returns the named Deployment status condition, or nil.
func findDeploymentCondition(dep *appsv1.Deployment, t appsv1.DeploymentConditionType) *appsv1.DeploymentCondition {
	for i := range dep.Status.Conditions {
		if dep.Status.Conditions[i].Type == t {
			return &dep.Status.Conditions[i]
		}
	}
	return nil
}

// resourceListEqual reports whether two ResourceLists hold the same keys with
// numerically equal quantities. It is used by the ResourceQuota watch predicate
// to ignore status-only churn (.status.used changes as pods come and go) and
// reconcile only when an admin changes a quota's .spec.hard. reflect.DeepEqual
// is unsuitable: resource.Quantity caches a formatted string in an unexported
// field, so equal values can compare unequal.
func resourceListEqual(a, b corev1.ResourceList) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || va.Cmp(vb) != 0 {
			return false
		}
	}
	return true
}

// setCredentialUnavailable sets CredentialUnavailable=True and Ready=False on the
// ActionsGateway. It returns RequeueAfter: 30s as a fallback so the reconciler
// periodically re-checks even if a watch event is missed (e.g. on controller
// restart). The WatchesMetadata watch on Secrets typically re-triggers faster.
func (r *ActionsGatewayReconciler) setCredentialUnavailable(ctx context.Context, ag *gmcv1alpha1.ActionsGateway, msg string) (ctrl.Result, error) {
	now := metav1.Now()
	gen := ag.Generation
	meta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
		Type:               gmcv1alpha1.ConditionCredentialUnavailable,
		Status:             metav1.ConditionTrue,
		Reason:             gmcv1alpha1.ReasonSecretNotFound,
		Message:            msg,
		LastTransitionTime: now,
		ObservedGeneration: gen,
	})
	meta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
		Type:               gmcv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             gmcv1alpha1.ReasonCredentialUnavailable,
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

// setDegraded records a Degraded=True condition (and Ready=False) naming the
// provisioning step that failed, then returns the original error so the reconcile
// still requeues with backoff. It is called on the reconcileResources error path,
// which returns before updateStatus — without it the object's conditions would go
// stale and never reflect the failure (Q156). The failing step is read from the
// provisioningError wrap; an unwrapped error falls back to a generic step label.
func (r *ActionsGatewayReconciler) setDegraded(ctx context.Context, ag *gmcv1alpha1.ActionsGateway, cause error) (ctrl.Result, error) {
	step := "reconcile"
	var pe *provisioningError
	if errors.As(cause, &pe) && pe.step != "" {
		step = pe.step
	}
	now := metav1.Now()
	gen := ag.Generation
	msg := fmt.Sprintf("provisioning failed at step %q: %v", step, cause)
	meta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
		Type:               gmcv1alpha1.ConditionDegraded,
		Status:             metav1.ConditionTrue,
		Reason:             gmcv1alpha1.ReasonProvisioningFailed,
		Message:            msg,
		LastTransitionTime: now,
		ObservedGeneration: gen,
	})
	meta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
		Type:               gmcv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             gmcv1alpha1.ReasonDegraded,
		Message:            msg,
		LastTransitionTime: now,
		ObservedGeneration: gen,
	})
	if err := r.Status().Update(ctx, ag); err != nil && !apierrors.IsConflict(err) {
		logf.FromContext(ctx).Error(err, "failed to write Degraded condition")
	}
	// Return the original provisioning error so controller-runtime requeues with
	// backoff (the status write above is best-effort observability, not the retry).
	return ctrl.Result{}, cause
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

// quotaToActionsGateways maps a ResourceQuota event to every ActionsGateway in
// the same namespace, so a platform admin changing the namespace quota's
// .spec.hard re-triggers reconciliation and refreshes the ProxyQuotaPressure
// condition (Q82) without waiting for the next unrelated reconcile.
func (r *ActionsGatewayReconciler) quotaToActionsGateways(ctx context.Context, obj client.Object) []ctrl.Request {
	var agList gmcv1alpha1.ActionsGatewayList
	if err := r.List(ctx, &agList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	reqs := make([]ctrl.Request, 0, len(agList.Items))
	for i := range agList.Items {
		reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: agList.Items[i].Namespace,
			Name:      agList.Items[i].Name,
		}})
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *ActionsGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Reconcile when an admin changes a namespace ResourceQuota's .spec.hard, but
	// not on the high-frequency .status.used churn as pods come and go — only the
	// hard cap feeds ProxyQuotaPressure (Q82).
	quotaHardChanged := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldQ, ok1 := e.ObjectOld.(*corev1.ResourceQuota)
			newQ, ok2 := e.ObjectNew.(*corev1.ResourceQuota)
			if !ok1 || !ok2 {
				return true
			}
			return !resourceListEqual(oldQ.Spec.Hard, newQ.Spec.Hard)
		},
	}
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
		// Full (not metadata-only) watch: the predicate needs .spec.hard. Quotas
		// are small and carry no secret material, so caching them is cheap.
		Watches(
			&corev1.ResourceQuota{},
			handler.EnqueueRequestsFromMapFunc(r.quotaToActionsGateways),
			builder.WithPredicates(quotaHardChanged),
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

// applyOrPruneServiceMonitors reconciles the two per-tenant ServiceMonitors
// (proxy + AGC, Q72) according to EnableTenantServiceMonitors:
//
//   - enabled: create/patch both, presenting the per-tenant scraper client
//     bundle for mTLS. If the monitoring.coreos.com CRD is not installed, the
//     apply fails with a NoMatch error — this is downgraded to a Warning Event
//     and a logged warning rather than failing the whole reconcile, so a missing
//     optional scrape prerequisite never blocks tenant provisioning.
//   - disabled (default): best-effort delete both, so flipping the flag off (or
//     never opting in) leaves no stale ServiceMonitors behind. A NoMatch error
//     (CRD absent) is success — there is nothing to prune.
func (r *ActionsGatewayReconciler) applyOrPruneServiceMonitors(ctx context.Context, ag *gmcv1alpha1.ActionsGateway) error {
	monitors := []struct {
		name    string
		appName string
		svcName string
	}{
		{proxyServiceMonitorName, proxyAppName, proxyServiceName},
		{agcServiceMonitorName, agcAppName, agcAppName},
	}

	if !r.EnableTenantServiceMonitors {
		for _, m := range monitors {
			sm := &unstructured.Unstructured{}
			sm.SetGroupVersionKind(serviceMonitorGVK)
			if err := r.deleteIfExists(ctx, sm, ag.Namespace, m.name); err != nil && !meta.IsNoMatchError(err) {
				return err
			}
		}
		return nil
	}

	for _, m := range monitors {
		if err := r.applyServiceMonitor(ctx, ag, buildMetricsServiceMonitor(ag, m.name, m.appName, m.svcName)); err != nil {
			if meta.IsNoMatchError(err) {
				// The monitoring.coreos.com CRD is not installed. Surface an
				// actionable signal but do not fail provisioning — the operator
				// opted into ServiceMonitors without (yet) installing the
				// Prometheus Operator.
				logf.FromContext(ctx).Info("skipping ServiceMonitor: monitoring.coreos.com CRD not installed", "name", m.name)
				if r.Recorder != nil {
					r.Recorder.Eventf(ag, nil, corev1.EventTypeWarning, "ServiceMonitorCRDMissing", "ApplyServiceMonitors",
						"Tenant ServiceMonitors are enabled but the monitoring.coreos.com ServiceMonitor CRD is not installed; install the Prometheus Operator to enable metrics scraping. Skipping %q.",
						m.name)
				}
				return nil
			}
			return err
		}
	}
	return nil
}

// applyServiceMonitor creates or patches an unstructured ServiceMonitor, setting
// the controller-managed labels + spec and an owner reference on the
// ActionsGateway so it is GC'd on delete. Mirrors the other apply* helpers:
// CreateOrPatch writes only the fields set in the mutate closure.
func (r *ActionsGatewayReconciler) applyServiceMonitor(ctx context.Context, ag *gmcv1alpha1.ActionsGateway, desired *unstructured.Unstructured) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(serviceMonitorGVK)
	obj.SetNamespace(desired.GetNamespace())
	obj.SetName(desired.GetName())
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.SetLabels(desired.GetLabels())
		spec, _, _ := unstructured.NestedMap(desired.Object, "spec")
		if err := unstructured.SetNestedMap(obj.Object, spec, "spec"); err != nil {
			return err
		}
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
