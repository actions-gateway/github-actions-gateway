// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=actionsgateways,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=actionsgateways/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=actionsgateways/finalizers,verbs=update
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=runnergroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete;bind;escalate
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

package controller

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	agcv1alpha1 "github.com/karlkfi/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/karlkfi/github-actions-gateway/gmc/api/v1alpha1"
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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// ActionsGatewayReconciler reconciles ActionsGateway objects.
type ActionsGatewayReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	IPFetcher   GitHubIPRangeFetcher
	AGCImage    string
	ProxyImage  string
	Log         *slog.Logger
	AGCExtraEnv []corev1.EnvVar // extra env vars forwarded to AGC pods (e.g. for tests)
}

// Reconcile reconciles an ActionsGateway CR.
func (r *ActionsGatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

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

	// Fetch GitHub IP ranges (best-effort; empty on error).
	var githubCIDRs []net.IPNet
	if r.IPFetcher != nil {
		cidrs, err := r.IPFetcher.FetchIPRanges(ctx)
		if err != nil {
			log.Error(err, "failed to fetch GitHub IP ranges; proceeding without CIDR egress rules")
		} else {
			githubCIDRs = cidrs
		}
	}

	proxyAddr := buildProxyServiceAddr(&ag)

	if err := r.reconcileResources(ctx, &ag, githubCIDRs, proxyAddr); err != nil {
		return ctrl.Result{}, err
	}

	return r.updateStatus(ctx, &ag)
}

func (r *ActionsGatewayReconciler) reconcileResources(ctx context.Context, ag *gmcv1alpha1.ActionsGateway, githubCIDRs []net.IPNet, proxyAddr string) error {
	// 0. Stamp Pod Security Admission labels on the tenant namespace.
	if err := r.applyNamespacePSA(ctx, ag); err != nil {
		return fmt.Errorf("namespace PSA labels: %w", err)
	}

	// 1 & 2. ServiceAccounts.
	if err := r.applyServiceAccount(ctx, buildAGCServiceAccount(ag)); err != nil {
		return fmt.Errorf("AGC ServiceAccount: %w", err)
	}
	if err := r.applyServiceAccount(ctx, buildWorkerServiceAccount(ag)); err != nil {
		return fmt.Errorf("worker ServiceAccount: %w", err)
	}

	// 3 & 4. Role + RoleBinding.
	if err := r.applyRole(ctx, buildAGCRole(ag)); err != nil {
		return fmt.Errorf("AGC Role: %w", err)
	}
	if err := r.applyRoleBinding(ctx, buildAGCRoleBinding(ag)); err != nil {
		return fmt.Errorf("AGC RoleBinding: %w", err)
	}

	// 6. ResourceQuota (only if spec.namespaceQuota is set).
	if len(ag.Spec.NamespaceQuota) > 0 {
		if err := r.applyResourceQuota(ctx, buildResourceQuota(ag)); err != nil {
			return fmt.Errorf("ResourceQuota: %w", err)
		}
	}

	// 7a. Proxy TLS cert Secret — must exist before the proxy Deployment references it.
	if err := r.ensureProxyCert(ctx, ag); err != nil {
		return fmt.Errorf("proxy TLS cert: %w", err)
	}

	// 7 & 8. Proxy Deployment + Service (before NetworkPolicy so we can read ClusterIP).
	if err := r.applyDeployment(ctx, ag, buildProxyDeployment(ag, r.ProxyImage)); err != nil {
		return fmt.Errorf("proxy Deployment: %w", err)
	}
	if err := r.applyService(ctx, buildProxyService(ag)); err != nil {
		return fmt.Errorf("proxy Service: %w", err)
	}

	// 5. NetworkPolicies — built after Service creation so we can embed the ClusterIP.
	proxyClusterIP := ""
	var proxySvc corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: proxyServiceName}, &proxySvc); err == nil {
		proxyClusterIP = proxySvc.Spec.ClusterIP
	}
	if err := r.applyNetworkPolicy(ctx, buildProxyNetworkPolicy(ag, githubCIDRs)); err != nil {
		return fmt.Errorf("proxy NetworkPolicy: %w", err)
	}
	if err := r.applyNetworkPolicy(ctx, buildWorkloadNetworkPolicy(ag, proxyClusterIP)); err != nil {
		return fmt.Errorf("workload NetworkPolicy: %w", err)
	}
	if err := r.applyNetworkPolicy(ctx, buildAGCNetworkPolicy(ag)); err != nil {
		return fmt.Errorf("AGC NetworkPolicy: %w", err)
	}
	// Delete the legacy single-policy "actions-gateway" NetworkPolicy left by previous versions.
	r.deleteIfExists(ctx, &networkingv1.NetworkPolicy{}, ag.Namespace, "actions-gateway")

	// 9. PDB.
	if err := r.applyPDB(ctx, buildPDB(ag)); err != nil {
		return fmt.Errorf("PodDisruptionBudget: %w", err)
	}

	// 10. HPA.
	if err := r.applyHPA(ctx, buildHPA(ag)); err != nil {
		return fmt.Errorf("HorizontalPodAutoscaler: %w", err)
	}

	// 11. AGC Deployment.
	if err := r.applyDeployment(ctx, ag, buildAGCDeployment(ag, r.AGCImage, proxyAddr, r.AGCExtraEnv)); err != nil {
		return fmt.Errorf("AGC Deployment: %w", err)
	}

	// 12. RunnerGroup CRs.
	for i, spec := range ag.Spec.RunnerGroups {
		name := fmt.Sprintf("%s-%d", ag.Name, i)
		if spec.RunnerLabels != nil && len(spec.RunnerLabels) > 0 {
			name = fmt.Sprintf("%s-%s", ag.Name, labelSafe(spec.RunnerLabels[0]))
		}
		if err := r.applyRunnerGroup(ctx, buildRunnerGroup(ag, spec, name)); err != nil {
			return fmt.Errorf("RunnerGroup %s: %w", name, err)
		}
	}

	return nil
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
	name := ag.Name

	// 2. AGC Deployment.
	r.deleteIfExists(ctx, &appsv1.Deployment{}, ns, agcAppName)
	// 3. HPA, PDB, proxy Service, proxy Deployment.
	// The proxy TLS cert Secret has an owner reference on the ActionsGateway CR; GC
	// handles its cleanup automatically when the CR is deleted, so no explicit delete
	// is needed here (and GMC does not have delete permission on secrets).
	r.deleteIfExists(ctx, &autoscalingv2.HorizontalPodAutoscaler{}, ns, proxyServiceName)
	r.deleteIfExists(ctx, &policyv1.PodDisruptionBudget{}, ns, proxyServiceName)
	r.deleteIfExists(ctx, &corev1.Service{}, ns, proxyServiceName)
	r.deleteIfExists(ctx, &appsv1.Deployment{}, ns, proxyServiceName)
	// 4. ResourceQuota, NetworkPolicies.
	r.deleteIfExists(ctx, &corev1.ResourceQuota{}, ns, "actions-gateway")
	r.deleteIfExists(ctx, &networkingv1.NetworkPolicy{}, ns, npProxyName)
	r.deleteIfExists(ctx, &networkingv1.NetworkPolicy{}, ns, npAGCName)
	r.deleteIfExists(ctx, &networkingv1.NetworkPolicy{}, ns, npWorkloadName)
	r.deleteIfExists(ctx, &networkingv1.NetworkPolicy{}, ns, "actions-gateway") // legacy
	// 5. RoleBinding, Role.
	r.deleteIfExists(ctx, &rbacv1.RoleBinding{}, ns, agcSAName)
	r.deleteIfExists(ctx, &rbacv1.Role{}, ns, agcSAName)
	// 6. ServiceAccounts.
	r.deleteIfExists(ctx, &corev1.ServiceAccount{}, ns, agcSAName)
	r.deleteIfExists(ctx, &corev1.ServiceAccount{}, ns, workerSAName)

	_ = name // used for label selector above

	// Remove finalizer.
	if err := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: ag.Name}, ag); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	controllerutil.RemoveFinalizer(ag, finalizerName)
	return ctrl.Result{}, r.Update(ctx, ag)
}

func (r *ActionsGatewayReconciler) deleteIfExists(ctx context.Context, obj client.Object, ns, name string) {
	obj.SetNamespace(ns)
	obj.SetName(name)
	if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
		logf.FromContext(ctx).Error(err, "failed to delete resource", "namespace", ns, "name", name)
	}
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

	setCondition("ProxyAvailable", proxyAvailable, "ProxyReady", fmt.Sprintf("%d/%d proxy pods ready", proxyReady, minReplicas))
	setCondition("AGCAvailable", agcAvailable, "AGCReady", "AGC deployment status")
	setCondition("Ready", proxyAvailable && agcAvailable, "AllAvailable", "all components are available")

	ag.Status.ProxyReadyReplicas = proxyReady
	ag.Status.ObservedGeneration = gen

	if err := r.Status().Update(ctx, ag); err != nil && !apierrors.IsConflict(err) {
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

// SetupWithManager sets up the controller with the Manager.
func (r *ActionsGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gmcv1alpha1.ActionsGateway{}).
		Owns(&appsv1.Deployment{}).
		Named("actionsgateway").
		Complete(r)
}

// applyServiceAccount creates or updates a ServiceAccount.
func (r *ActionsGatewayReconciler) applyServiceAccount(ctx context.Context, desired *corev1.ServiceAccount) error {
	var existing corev1.ServiceAccount
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Labels = desired.Labels
	return r.Update(ctx, &existing)
}

func (r *ActionsGatewayReconciler) applyRole(ctx context.Context, desired *rbacv1.Role) error {
	var existing rbacv1.Role
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Labels = desired.Labels
	existing.Rules = desired.Rules
	return r.Update(ctx, &existing)
}

func (r *ActionsGatewayReconciler) applyRoleBinding(ctx context.Context, desired *rbacv1.RoleBinding) error {
	var existing rbacv1.RoleBinding
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Labels = desired.Labels
	existing.RoleRef = desired.RoleRef
	existing.Subjects = desired.Subjects
	return r.Update(ctx, &existing)
}

func (r *ActionsGatewayReconciler) applyNetworkPolicy(ctx context.Context, desired *networkingv1.NetworkPolicy) error {
	var existing networkingv1.NetworkPolicy
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Labels = desired.Labels
	existing.Spec = desired.Spec
	return r.Update(ctx, &existing)
}

func (r *ActionsGatewayReconciler) applyResourceQuota(ctx context.Context, desired *corev1.ResourceQuota) error {
	var existing corev1.ResourceQuota
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Labels = desired.Labels
	existing.Spec = desired.Spec
	return r.Update(ctx, &existing)
}

// applyDeployment creates or updates a Deployment and sets an owner reference so
// that the Owns(&appsv1.Deployment{}) watch on the controller fires when the
// Deployment's status changes (e.g. ReadyReplicas increases after pod startup).
func (r *ActionsGatewayReconciler) applyDeployment(ctx context.Context, ag *gmcv1alpha1.ActionsGateway, desired *appsv1.Deployment) error {
	if err := controllerutil.SetControllerReference(ag, desired, r.Scheme); err != nil {
		return err
	}
	var existing appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Labels = desired.Labels
	existing.Spec = desired.Spec
	existing.OwnerReferences = desired.OwnerReferences
	return r.Update(ctx, &existing)
}

func (r *ActionsGatewayReconciler) applyService(ctx context.Context, desired *corev1.Service) error {
	var existing corev1.Service
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Labels = desired.Labels
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.Ports = desired.Spec.Ports
	return r.Update(ctx, &existing)
}

func (r *ActionsGatewayReconciler) applyPDB(ctx context.Context, desired *policyv1.PodDisruptionBudget) error {
	var existing policyv1.PodDisruptionBudget
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Labels = desired.Labels
	existing.Spec = desired.Spec
	return r.Update(ctx, &existing)
}

func (r *ActionsGatewayReconciler) applyHPA(ctx context.Context, desired *autoscalingv2.HorizontalPodAutoscaler) error {
	var existing autoscalingv2.HorizontalPodAutoscaler
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Labels = desired.Labels
	existing.Spec = desired.Spec
	return r.Update(ctx, &existing)
}

func (r *ActionsGatewayReconciler) applyRunnerGroup(ctx context.Context, desired *agcv1alpha1.RunnerGroup) error {
	var existing agcv1alpha1.RunnerGroup
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Labels = desired.Labels
	existing.Spec = desired.Spec
	return r.Update(ctx, &existing)
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

	if !apierrors.IsNotFound(getErr) {
		if cert, err := parseCertPEM(existing.Data[corev1.TLSCertKey]); err == nil {
			if time.Until(cert.NotAfter) > proxyCertRenewBefore {
				return nil // cert is valid and not near expiry
			}
		}
	}

	certPEM, keyPEM, err := generateProxyCert(ag)
	if err != nil {
		return fmt.Errorf("generate proxy cert: %w", err)
	}

	desired := buildProxyCertSecret(ag, certPEM, keyPEM)
	if err := controllerutil.SetControllerReference(ag, desired, r.Scheme); err != nil {
		return fmt.Errorf("set owner reference on proxy cert secret: %w", err)
	}
	if apierrors.IsNotFound(getErr) {
		return r.Create(ctx, desired)
	}
	existing.Data = desired.Data
	existing.Labels = desired.Labels
	existing.OwnerReferences = desired.OwnerReferences
	return r.Update(ctx, &existing)
}

// applyNamespacePSA stamps Pod Security Admission labels on the tenant namespace.
// The enforce/warn/audit levels are set to the ActionsGateway's SecurityProfile
// (defaulting to "baseline"). Callers must have namespaces get+update permission.
func (r *ActionsGatewayReconciler) applyNamespacePSA(ctx context.Context, ag *gmcv1alpha1.ActionsGateway) error {
	profile := ag.Spec.SecurityProfile
	if profile == "" {
		profile = "baseline"
	}
	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: ag.Namespace}, &ns); err != nil {
		return err
	}
	if ns.Labels == nil {
		ns.Labels = make(map[string]string)
	}
	ns.Labels["pod-security.kubernetes.io/enforce"] = profile
	ns.Labels["pod-security.kubernetes.io/enforce-version"] = "latest"
	ns.Labels["pod-security.kubernetes.io/warn"] = profile
	ns.Labels["pod-security.kubernetes.io/warn-version"] = "latest"
	ns.Labels["pod-security.kubernetes.io/audit"] = profile
	ns.Labels["pod-security.kubernetes.io/audit-version"] = "latest"
	return r.Update(ctx, &ns)
}

// labelSafe converts a string to a safe label value segment.
func labelSafe(s string) string {
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
	return string(out)
}
