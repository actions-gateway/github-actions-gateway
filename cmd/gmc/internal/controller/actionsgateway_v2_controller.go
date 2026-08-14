// +kubebuilder:rbac:groups=actions-gateway.com,resources=actionsgateways,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=actions-gateway.com,resources=actionsgateways/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=actions-gateway.com,resources=actionsgateways/finalizers,verbs=update
// The AGC control-plane children (Deployment/SA/RoleBinding/Service/NetworkPolicy/
// Secret) write verbs are already granted to the GMC ClusterRole by the v1
// ActionsGateway reconciler markers, which controller-gen aggregates into the same
// manager-role; the v2 reconciler reuses them. It reads the namespace
// security-profile label (namespaces get;list;watch already granted) and the
// referenced EgressProxy (granted above).
//
// Multi-gateway (M3b): each gateway's AGC needs cluster-scoped read of the
// cluster-scoped ClusterRunnerTemplate kind, which a namespaced RoleBinding cannot
// grant. The GMC creates a per-gateway ClusterRoleBinding to the shipped
// agc-clusterrunnertemplate-reader ClusterRole; it holds clusterrolebindings CRUD
// and `bind` only on that exact ClusterRole name, so it never gains the read itself
// nor can bind AGC SAs into arbitrary ClusterRoles.
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=bind,resourceNames=agc-clusterrunnertemplate-reader
//
// Managed AGC right-sizing (Q360): the per-gateway spec.agcAutoscaling opt-in has the
// GMC stamp a VerticalPodAutoscaler next to each AGC Deployment. The grant is
// unconditional because RBAC rules are declarative — a rule naming a group whose CRD is
// not installed is inert, and the reconciler degrades on the missing CRD rather than on
// a permission error. It grants nothing over pod resources directly: a VPA only ever
// resizes the workload it targets, and every object the GMC creates is scoped to a
// tenant namespace by the tenant-resource-guard admission policy.
// +kubebuilder:rbac:groups=autoscaling.k8s.io,resources=verticalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
//
// GHES private-CA trust (Q536): spec.githubCABundleRef names a tenant ConfigMap the
// reconciler reads to validate before mounting it on the AGC. `get` only, and no
// list/watch — the read is uncached, so no ConfigMap informer is started and the
// name-pinned scoping of the egress-allowlist watch is left intact. Strictly narrower
// than the secrets grant the credential path already holds.
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get

package controller

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

// ActionsGatewayV2Reconciler reconciles a v2alpha1 ActionsGateway into the
// per-tenant AGC control plane: the AGC Deployment/ServiceAccount/RoleBinding/
// Service, the AGC and workload NetworkPolicies, the metrics mTLS Secrets, and the
// AGC's egress wiring through the resolved EgressProxy. Every child carries a
// controller owner reference for clean cascade GC (§H.8).
//
// It is the v2 counterpart of the v1 ActionsGatewayReconciler, minus two
// responsibilities v2 moved elsewhere: the egress proxy pool is now a standalone
// EgressProxy (M2 reconciler) the gateway only references, and the namespace Pod
// Security Admission labels are stamped by the NamespacePSAReconciler from the
// namespace security-profile label (Q175) — this reconciler reads that label to
// thread SECURITY_PROFILE to the AGC but never stamps PSA. Single-gateway per
// namespace (M3a).
//
// The egress proxy is optional (Q168, §H.10): a gateway with a defaultProxyRef
// egresses through that EgressProxy (proxyMode Direct→Proxied), with stable
// per-tenant egress IPs; a gateway with no defaultProxyRef egresses directly
// (proxyMode Direct), still NetworkPolicy-restricted to DNS + GitHub CIDRs + the
// kube API server — only the per-tenant IP *identity* is lost, surfaced via the
// advisory EgressUnattributed condition. Restriction is never dropped.
type ActionsGatewayV2Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// AGCImage is the AGC container image injected into the Deployment.
	AGCImage string
	// AGCExtraEnv is the testing-only AGC_EXTRA_* passthrough (gated by the GMC's
	// --allow-agc-extra-env flag), forwarded verbatim to the AGC Deployment.
	AGCExtraEnv []corev1.EnvVar
	// APIServerCIDRs optionally scopes the AGC NetworkPolicy's apiserver egress
	// rule (Q145); empty keeps it any-destination (the secure default).
	APIServerCIDRs []string
	// IPCache supplies the current GitHub IP CIDRs for the direct-egress AGC and
	// workload NetworkPolicies (Q168). Shared with the IPRangeReconciler, which keeps
	// those policies current as GitHub rotates ranges. nil is tolerated (the direct
	// NetworkPolicies are created with no GitHub rule and patched on the next refresh).
	IPCache *IPRangeCache
	// Recorder emits Kubernetes Events on the ActionsGateway. May be nil in tests.
	Recorder events.EventRecorder
	// Reader is an uncached apiserver reader (mgr.GetAPIReader()) for the reads the
	// cache cannot serve: teardown verification, where the cached client would still
	// return a just-deleted child and make every clean teardown look incomplete, and
	// the githubCABundleRef ConfigMap, which the name-pinned ConfigMap informer does
	// not cover. nil (unit tests) falls back to the cached Client.
	Reader client.Reader
	// Now supplies the current time for the teardown worker-drain deadline. nil is
	// time.Now; tests inject to drive the deadline without sleeping.
	Now func() time.Time
}

func (r *ActionsGatewayV2Reconciler) nowFunc() func() time.Time {
	if r.Now != nil {
		return r.Now
	}
	return time.Now
}

// uncachedReader returns the uncached apiserver reader, falling back to the cached
// client when none is wired (unit tests use an uncached fake). Two callers need it:
// teardown verification, where the cache would still return a just-deleted child, and
// the githubCABundleRef resolution, whose tenant ConfigMap the GMC's name-pinned
// ConfigMap informer does not cover.
func (r *ActionsGatewayV2Reconciler) uncachedReader() client.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}

// githubCIDRs returns the current GitHub IP CIDRs from the shared cache, or nil when
// no cache is wired or it has not completed its first fetch. A nil/empty result makes
// the direct-egress NetworkPolicies omit the GitHub rule (fail-closed) until the
// IPRangeReconciler patches them.
func (r *ActionsGatewayV2Reconciler) githubCIDRs() []net.IPNet {
	if r.IPCache == nil {
		return nil
	}
	return r.IPCache.Snapshot()
}

// preserveExistingEgress copies an already-programmed NetworkPolicy's egress rules
// into desired, so a reconcile that runs before the IP-range cache is ready does not
// blank the direct-egress GitHub allowlist (Q246/Q61). If the policy does not exist
// yet it is left as built (fail-closed, no GitHub rule): no worker can egress that
// early, and IPRangeReconciler patches the rule in within seconds. Best-effort — a
// Get error other than NotFound is ignored, leaving the pre-fix behavior as the worst
// case rather than failing the whole reconcile.
func (r *ActionsGatewayV2Reconciler) preserveExistingEgress(ctx context.Context, namespace string, desired *networkingv1.NetworkPolicy) {
	var existing networkingv1.NetworkPolicy
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: desired.Name}, &existing); err != nil {
		return
	}
	if len(existing.Spec.Egress) > 0 {
		desired.Spec.Egress = existing.Spec.Egress
	}
}

// Reconcile drives a v2 ActionsGateway toward its desired AGC control plane.
func (r *ActionsGatewayV2Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ag gmcv2alpha1.ActionsGateway
	if err := r.Get(ctx, req.NamespacedName, &ag); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ag.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &ag)
	}

	if !controllerutil.ContainsFinalizer(&ag, gmcv2alpha1.ActionsGatewayFinalizer) {
		controllerutil.AddFinalizer(&ag, gmcv2alpha1.ActionsGatewayFinalizer)
		if err := r.Update(ctx, &ag); err != nil {
			return ctrl.Result{}, err
		}
		// Provisioning-start signal, emitted once per object lifecycle (the finalizer
		// is added exactly once) so `kubectl describe` shows when the AGC control plane
		// began provisioning.
		r.recordEvent(&ag, corev1.EventTypeNormal, "Provisioning", "Reconcile",
			"started provisioning the AGC control plane")
		return ctrl.Result{Requeue: true}, nil
	}

	// Credential check, by union member (Q196/Q197):
	switch ag.Spec.Credentials.Type {
	case gmcv2alpha1.CredentialTypeGitHubApp:
		// Possession model: the AGC mounts the GitHub App Secret; without it, do not
		// provision (CredentialUnavailable, fail closed).
		var credSecret corev1.Secret
		credErr := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: ag.Spec.GitHubAppSecretName()}, &credSecret)
		if credErr != nil && !apierrors.IsNotFound(credErr) {
			return ctrl.Result{}, credErr
		}
		if apierrors.IsNotFound(credErr) {
			return r.setNotReady(ctx, &ag, gmcv2alpha1.ConditionCredentialUnavailable, gmcv2alpha1.ReasonSecretNotFound,
				fmt.Sprintf("GitHub App Secret %q not found in namespace %q", ag.Spec.GitHubAppSecretName(), ag.Namespace))
		}
	case gmcv2alpha1.CredentialTypeWorkloadIdentity:
		// Delegation model (no in-cluster key, Q197/Q201): there is no GitHub App Secret
		// to check — the App key never enters the cluster. The AGC is provisioned with
		// the signer config env and a Vault-audience-scoped projected ServiceAccount
		// token (buildAGCDeploymentV2), and authenticates to Vault with that pod identity
		// (the operator binds the AGC ServiceAccount to its Vault role out of band). No
		// fail-closed branch: provisioning proceeds below.
	}

	// Resolve the control-plane egress proxy from defaultProxyRef (Q168, §H.10).
	// Unset ⇒ direct egress: the AGC reaches GitHub directly, still restricted by the
	// direct-egress NetworkPolicy to DNS + GitHub CIDRs + the kube API server. Set ⇒
	// proxied: the named EgressProxy must exist; a defaultProxyRef pointing at a
	// missing proxy is an operator error and still fails closed (ProxyNotFound).
	// A defaultProxyRef naming another namespace additionally needs provider consent
	// (M4, §H.9): the proxy must list this gateway's namespace in
	// spec.sharing.allowedNamespaces. Absent or empty sharing denies, so an operator
	// cannot reach a proxy by naming it.
	var proxy *gmcv2alpha1.EgressProxy
	if ag.Spec.DefaultProxyRef != nil {
		proxyNS := ag.Spec.DefaultProxyRef.Namespace
		if proxyNS == "" {
			proxyNS = ag.Namespace
		}
		var p gmcv2alpha1.EgressProxy
		proxyErr := r.Get(ctx, types.NamespacedName{Namespace: proxyNS, Name: ag.Spec.DefaultProxyRef.Name}, &p)
		if proxyErr != nil && !apierrors.IsNotFound(proxyErr) {
			return ctrl.Result{}, proxyErr
		}
		if apierrors.IsNotFound(proxyErr) {
			return r.setNotReady(ctx, &ag, gmcv2alpha1.ConditionDegraded, gmcv2alpha1.ReasonProxyNotFound,
				fmt.Sprintf("EgressProxy %q (defaultProxyRef) not found in namespace %q", ag.Spec.DefaultProxyRef.Name, proxyNS))
		}
		if proxyNS != ag.Namespace && !proxyShareGranted(&p, ag.Namespace) {
			return r.setNotReady(ctx, &ag, gmcv2alpha1.ConditionDegraded, gmcv2alpha1.ReasonProxyShareNotGranted,
				fmt.Sprintf("EgressProxy %q in namespace %q does not list namespace %q in spec.sharing.allowedNamespaces",
					ag.Spec.DefaultProxyRef.Name, proxyNS, ag.Namespace))
		}
		proxy = &p
	}

	// GHES private-CA trust (Q536): provisioning an AGC with a mount it cannot read
	// wedges the pod at ContainerCreating with no explanation, so an unresolvable
	// githubCABundleRef fails closed here instead.
	if caReason, caMsg, caErr := r.checkGitHubCABundle(ctx, &ag); caErr != nil {
		return ctrl.Result{}, caErr
	} else if caReason != "" {
		res, err := r.setNotReady(ctx, &ag, gmcv2alpha1.ConditionDegraded, caReason, caMsg)
		if err != nil {
			return res, err
		}
		// Polled rather than watched: the GMC's ConfigMap informer is pinned to one
		// object in its own namespace (buildCacheOptions), so watching a tenant
		// ConfigMap would mean a cluster-wide ConfigMap cache and the broad read grant
		// that scoping exists to avoid.
		res.RequeueAfter = githubCABundleReprobeInterval
		return res, nil
	}

	// Read the namespace's effective security profile (the source label the
	// NamespacePSAReconciler stamps PSA from) to thread SECURITY_PROFILE to the AGC.
	securityProfile, err := r.namespaceSecurityProfile(ctx, ag.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileResources(ctx, &ag, proxy, securityProfile); err != nil {
		return r.setDegraded(ctx, &ag, err)
	}

	// Managed AGC right-sizing (Q360). Reconciled after the control-plane children so
	// the autoscaler's targetRef names an already-created Deployment, and kept out of
	// reconcileResources' step machinery because its interesting outcome is not an
	// error: an opt-in the cluster cannot satisfy (no VerticalPodAutoscaler CRD) comes
	// back as state for updateStatus to surface, never as a Degraded gateway. A real
	// apply failure (RBAC, apiserver) still degrades like any other child.
	autoscaler, err := r.applyOrPruneAGCAutoscaler(ctx, &ag)
	if err != nil {
		return r.setDegraded(ctx, &ag, &provisioningError{step: "AGC autoscaler", err: err})
	}

	return r.updateStatus(ctx, &ag, proxy, autoscaler)
}

// reconcileResources creates or patches every AGC control-plane child, each
// owner-referenced to the ActionsGateway. Failures are wrapped with the failing
// step so setDegraded can name it (Q156).
// proxy is nil for direct egress (§H.10).
func (r *ActionsGatewayV2Reconciler) reconcileResources(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, proxy *gmcv2alpha1.EgressProxy, securityProfile string) (retErr error) {
	log := logf.FromContext(ctx)
	var current string
	step := func(name string) { current = name; log.V(1).Info("reconcileResources step", "step", name) }
	defer func() {
		if retErr != nil {
			retErr = &provisioningError{step: current, err: retErr}
		}
	}()

	step("ServiceAccounts")
	if err := r.applyServiceAccount(ctx, ag, buildAGCServiceAccountV2(ag)); err != nil {
		return fmt.Errorf("AGC ServiceAccount: %w", err)
	}
	if err := r.applyServiceAccount(ctx, ag, buildWorkerServiceAccountV2(ag)); err != nil {
		return fmt.Errorf("worker ServiceAccount: %w", err)
	}

	step("AGC RoleBinding")
	if err := r.applyRoleBinding(ctx, ag, buildAGCRoleBindingV2(ag)); err != nil {
		return fmt.Errorf("AGC RoleBinding: %w", err)
	}

	step("ClusterRunnerTemplate ClusterRoleBinding")
	if err := r.applyClusterRunnerTemplateReaderBinding(ctx, ag); err != nil {
		return fmt.Errorf("ClusterRunnerTemplate ClusterRoleBinding: %w", err)
	}

	step("metrics TLS certs")
	if err := r.ensureMetricsCerts(ctx, ag); err != nil {
		return fmt.Errorf("metrics TLS certs: %w", err)
	}

	step("AGC Service")
	if err := r.applyService(ctx, ag, buildAGCServiceV2(ag)); err != nil {
		return fmt.Errorf("AGC Service: %w", err)
	}

	// Direct egress (proxy == nil) adds the GitHub-CIDR allowlist to the AGC and
	// workload NetworkPolicies so the AGC and workers reach GitHub directly; the proxied
	// path leaves them reaching GitHub through the proxy (§H.10). Restriction is
	// preserved in both modes — egress is never opened beyond DNS + GitHub (+ kube API).
	step("NetworkPolicies")
	direct := proxy == nil
	githubCIDRs := r.githubCIDRs()
	// Cross-namespace proxies (M4, §H.9) add one egress peer per granted provider
	// namespace. Computed here rather than in the builders so both policies see the
	// same set, and so a read failure fails the step instead of silently emitting a
	// policy missing a tenant's proxy.
	remoteProxies, err := grantedRemoteProxies(ctx, r.Client, ag)
	if err != nil {
		return fmt.Errorf("resolve cross-namespace proxy grants: %w", err)
	}
	workloadNP := buildWorkloadNetworkPolicyV2(ag, githubCIDRs, direct, remoteProxies)
	agcNP := buildAGCNetworkPolicyV2(ag, r.APIServerCIDRs, githubCIDRs, direct, remoteProxies)
	// Q246/Q61: in direct egress the GitHub-CIDR allowlist on these two policies is
	// sourced from the IP-range cache. At GMC startup that cache is empty until
	// IPRangeReconciler's first api.github.com/meta fetch lands, so rebuilding an
	// already-programmed policy from it would strip the entire allowlist and
	// default-deny GitHub egress for the AGC and every worker until the fetch
	// completes — a window (live-measured at ~25s on a cold node) that widens under
	// node CPU pressure (Q247) and produced the "release-asset download times out"
	// symptom. When the cache is not yet ready, preserve the existing policy's egress
	// instead of blanking it; a not-yet-created policy is still created fail-closed
	// (no GitHub rule), which is safe — no worker exists that early and
	// IPRangeReconciler patches the rule in within seconds. The cache is empty only
	// before its first successful fetch: a /meta fetch always yields ranges, so an
	// empty snapshot unambiguously means "not fetched yet", never "GitHub has none".
	if direct && len(githubCIDRs) == 0 {
		r.preserveExistingEgress(ctx, ag.Namespace, workloadNP)
		r.preserveExistingEgress(ctx, ag.Namespace, agcNP)
	}
	if err := r.applyNetworkPolicy(ctx, ag, workloadNP); err != nil {
		return fmt.Errorf("workload NetworkPolicy: %w", err)
	}
	if err := r.applyNetworkPolicy(ctx, ag, agcNP); err != nil {
		return fmt.Errorf("AGC NetworkPolicy: %w", err)
	}

	step("AGC Deployment")
	dep := buildAGCDeploymentV2(ag, r.AGCImage, proxy, securityProfile, r.AGCExtraEnv)
	if err := r.applyDeployment(ctx, ag, dep); err != nil {
		return fmt.Errorf("AGC Deployment: %w", err)
	}
	return nil
}

// namespaceSecurityProfile returns the effective Pod Security Admission profile
// for the tenant namespace, read from its security-profile label (baseline when
// absent). The reconciler only reads it; the NamespacePSAReconciler owns stamping
// the PSA labels (Q175).
func (r *ActionsGatewayV2Reconciler) namespaceSecurityProfile(ctx context.Context, namespace string) (string, error) {
	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
		return "", fmt.Errorf("read namespace %q for security profile: %w", namespace, err)
	}
	return gmcv2alpha1.EffectiveSecurityProfile(ns.Labels[gmcv2alpha1.SecurityProfileLabel]), nil
}

// githubCABundleReprobeInterval is how often a gateway whose githubCABundleRef does
// not resolve re-reads the ConfigMap. Short enough that applying the missing
// ConfigMap converges while the operator is still watching, and it only runs while
// the gateway is failed closed.
const githubCABundleReprobeInterval = time.Minute

// checkGitHubCABundle resolves spec.githubCABundleRef. It returns a Degraded reason
// and message when the reference does not resolve, ("", "", nil) when it resolves or
// is unset, and an error only for an apiserver failure.
//
// The read is uncached: the GMC's ConfigMap informer is pinned to a single object in
// its own namespace, so it cannot serve a tenant ConfigMap.
//
// Parsing here rather than letting the AGC fail at startup is deliberate — a Degraded
// condition naming the ConfigMap is a better operator signal than a CrashLoopBackOff.
func (r *ActionsGatewayV2Reconciler) checkGitHubCABundle(ctx context.Context, ag *gmcv2alpha1.ActionsGateway) (string, string, error) {
	ref := ag.Spec.GitHubCABundleRef
	if ref == nil {
		return "", "", nil
	}
	var cm corev1.ConfigMap
	err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: ref.Name}, &cm)
	if apierrors.IsNotFound(err) {
		return gmcv2alpha1.ReasonCABundleNotFound,
			fmt.Sprintf("ConfigMap %q (githubCABundleRef) not found in namespace %q", ref.Name, ag.Namespace), nil
	}
	if err != nil {
		return "", "", err
	}
	bundle := cm.Data[githubCABundleKey]
	if bundle == "" {
		bundle = string(cm.BinaryData[githubCABundleKey])
	}
	if bundle == "" {
		return gmcv2alpha1.ReasonCABundleInvalid,
			fmt.Sprintf("ConfigMap %q (githubCABundleRef) has no %q key", ref.Name, githubCABundleKey), nil
	}
	if !x509.NewCertPool().AppendCertsFromPEM([]byte(bundle)) {
		return gmcv2alpha1.ReasonCABundleInvalid,
			fmt.Sprintf("ConfigMap %q key %q holds no PEM-encoded certificate", ref.Name, githubCABundleKey), nil
	}
	return "", "", nil
}

// ensureMetricsCerts ensures the per-tenant metrics mTLS bundle exists and is not
// near expiry, writing the server Secret (mounted into the AGC) and the scraper
// client Secret (published for monitoring). Mirrors v1's ensureMetricsCerts. A fresh
// issuance or a near-expiry rotation emits a Normal Event so the credential
// transition is visible in `kubectl describe`; the steady-state path (cert valid)
// returns early and emits nothing.
func (r *ActionsGatewayV2Reconciler) ensureMetricsCerts(ctx context.Context, ag *gmcv2alpha1.ActionsGateway) error {
	var serverSec corev1.Secret
	serverErr := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: metricsTLSSecretNameV2(ag)}, &serverSec)
	if serverErr != nil && !apierrors.IsNotFound(serverErr) {
		return serverErr
	}
	var clientSec corev1.Secret
	clientErr := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: metricsClientSecretNameV2(ag)}, &clientSec)
	if clientErr != nil && !apierrors.IsNotFound(clientErr) {
		return clientErr
	}

	transition := "issued"
	if !apierrors.IsNotFound(serverErr) && !apierrors.IsNotFound(clientErr) {
		transition = "rotated (near expiry)"
		if cert, err := parseCertPEM(serverSec.Data[corev1.TLSCertKey]); err == nil {
			if time.Until(cert.NotAfter) > metricsCertRenewBefore {
				return nil
			}
		}
	}

	bundle, err := generateMetricsCertsV2(ag.Namespace, agcNameV2(ag))
	if err != nil {
		return fmt.Errorf("generate metrics certs: %w", err)
	}
	if err := r.applyOwnedSecret(ctx, ag, buildMetricsTLSSecretV2(ag, bundle)); err != nil {
		return fmt.Errorf("metrics server Secret: %w", err)
	}
	if err := r.applyOwnedSecret(ctx, ag, buildMetricsClientSecretV2(ag, bundle)); err != nil {
		return fmt.Errorf("metrics client Secret: %w", err)
	}
	r.recordEvent(ag, corev1.EventTypeNormal, "MetricsCertificateIssued", "EnsureMetricsCerts",
		"%s per-tenant metrics mTLS certificate", transition)
	return nil
}

// recordEvent emits a Kubernetes Event on the ActionsGateway when a Recorder is
// wired. The Recorder may be nil in unit tests, so callers go through here rather
// than dereferencing it directly (mirrors the AGC reconcilers' recordEvent).
func (r *ActionsGatewayV2Reconciler) recordEvent(ag *gmcv2alpha1.ActionsGateway, eventtype, reason, action, note string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(ag, nil, eventtype, reason, action, note, args...)
}

// conditionStatusValue returns the current status of the named condition, or the
// empty string when it is absent. It reads the value before a SetStatusCondition
// mutation so callers can detect a genuine status transition and emit an Event only
// on change rather than on every reconcile.
func conditionStatusValue(conds []metav1.Condition, condType string) metav1.ConditionStatus {
	if c := meta.FindStatusCondition(conds, condType); c != nil {
		return c.Status
	}
	return ""
}

// boolConditionStatus maps a boolean to the corresponding metav1.ConditionStatus.
func boolConditionStatus(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// --- apply helpers ---
//
// All delegate to applyManagedChild (apply_helpers.go), the one shared
// CreateOrPatch code path. Like v1 (since Q394), the v2 reconciler stamps a
// controller owner reference on every namespaced child; the sole exception is the
// cluster-scoped applyClusterRunnerTemplateReaderBinding, which cannot be owned by
// a namespaced ActionsGateway (see the ownerRef policy on applyManagedChild).

func (r *ActionsGatewayV2Reconciler) applyServiceAccount(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, desired *corev1.ServiceAccount) error {
	return applyManagedChild(ctx, r.Client, r.Scheme, ag, &corev1.ServiceAccount{}, desired, nil)
}

func (r *ActionsGatewayV2Reconciler) applyRoleBinding(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, desired *rbacv1.RoleBinding) error {
	obj := &rbacv1.RoleBinding{}
	err := applyManagedChild(ctx, r.Client, r.Scheme, ag, obj, desired, func() error {
		// roleRef is immutable; on a roleRef change the binding must be recreated.
		if obj.ResourceVersion != "" && obj.RoleRef != desired.RoleRef {
			return errRoleRefImmutable
		}
		obj.RoleRef = desired.RoleRef
		obj.Subjects = desired.Subjects
		return nil
	})
	if errors.Is(err, errRoleRefImmutable) {
		if delErr := r.Delete(ctx, obj); delErr != nil && !apierrors.IsNotFound(delErr) {
			return delErr
		}
		if refErr := controllerutil.SetControllerReference(ag, desired, r.Scheme); refErr != nil {
			return refErr
		}
		return r.Create(ctx, desired)
	}
	return err
}

// applyClusterRunnerTemplateReaderBinding creates or patches the per-gateway
// ClusterRoleBinding granting the AGC SA cluster-scoped read of ClusterRunnerTemplate.
// No owner reference: a cluster-scoped object cannot be owned by a namespaced
// ActionsGateway (the apiserver rejects the cross-scope ref and never GCs it), so
// reconcileDelete removes it explicitly.
func (r *ActionsGatewayV2Reconciler) applyClusterRunnerTemplateReaderBinding(ctx context.Context, ag *gmcv2alpha1.ActionsGateway) error {
	desired := buildClusterRunnerTemplateReaderBinding(ag)
	obj := &rbacv1.ClusterRoleBinding{}
	err := applyManagedChild(ctx, r.Client, r.Scheme, nil, obj, desired, func() error {
		// roleRef is immutable; on a roleRef change the binding must be recreated.
		if obj.ResourceVersion != "" && obj.RoleRef != desired.RoleRef {
			return errRoleRefImmutable
		}
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

func (r *ActionsGatewayV2Reconciler) applyService(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, desired *corev1.Service) error {
	obj := &corev1.Service{}
	return applyManagedChild(ctx, r.Client, r.Scheme, ag, obj, desired, func() error {
		// Preserve server-assigned Spec fields (ClusterIP); set only managed fields.
		obj.Spec.Type = desired.Spec.Type
		obj.Spec.Selector = desired.Spec.Selector
		obj.Spec.Ports = desired.Spec.Ports
		return nil
	})
}

func (r *ActionsGatewayV2Reconciler) applyNetworkPolicy(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, desired *networkingv1.NetworkPolicy) error {
	obj := &networkingv1.NetworkPolicy{}
	return applyManagedChild(ctx, r.Client, r.Scheme, ag, obj, desired, func() error {
		obj.Spec = desired.Spec
		return nil
	})
}

// applyDeployment creates or patches the AGC Deployment. Replacing the whole Spec
// is safe here: no HPA targets the AGC (v2 moved the autoscaled proxy pool out to
// EgressProxy), so no other controller owns `.spec.replicas`. An HPA-targeted
// Deployment must not be applied this way — see assignHPATargetDeploymentSpec (Q283).
// The tolerated pod-template annotations survive the replace (Q552).
func (r *ActionsGatewayV2Reconciler) applyDeployment(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, desired *appsv1.Deployment) error {
	obj := &appsv1.Deployment{}
	return applyManagedChild(ctx, r.Client, r.Scheme, ag, obj, desired, func() error {
		assignManagedDeploymentSpec(&obj.Spec, desired.Spec)
		return nil
	})
}

func (r *ActionsGatewayV2Reconciler) applyOwnedSecret(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, desired *corev1.Secret) error {
	obj := &corev1.Secret{}
	return applyManagedChild(ctx, r.Client, r.Scheme, ag, obj, desired, func() error {
		obj.Type = desired.Type
		obj.Data = desired.Data
		return nil
	})
}

// --- status ---

// updateStatus reads the AGC Deployment readiness and writes the uniform v2
// status/condition contract: Ready + AGCAvailable, observedGeneration, and a
// cleared CredentialUnavailable + Degraded (provisioning reached here). It also
// records the egress mode (proxyMode Proxied/Direct) and the advisory
// EgressUnattributed condition (True only in direct mode, §H.10). proxy is nil for
// direct egress. autoscaler carries the managed-VPA outcome (Q360) for the advisory
// AGCAutoscalingUnavailable condition.
func (r *ActionsGatewayV2Reconciler) updateStatus(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, proxy *gmcv2alpha1.EgressProxy, autoscaler agcAutoscalerState) (ctrl.Result, error) {
	var dep appsv1.Deployment
	agcReady := false
	if err := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: agcNameV2(ag)}, &dep); err == nil {
		agcReady = dep.Status.ReadyReplicas >= 1
	}
	now := metav1.Now()
	gen := ag.Generation

	// Snapshot the pre-reconcile status of the conditions whose transitions we surface
	// as Events, before the set() closure mutates them in place. Emitting only on a
	// genuine change keeps `kubectl describe` free of per-reconcile churn.
	prevReady := conditionStatusValue(ag.Status.Conditions, gmcv2alpha1.ConditionReady)
	prevRunnerSets := conditionStatusValue(ag.Status.Conditions, gmcv2alpha1.ConditionRunnerSetsDegraded)
	prevDegraded := conditionStatusValue(ag.Status.Conditions, gmcv2alpha1.ConditionDegraded)
	prevAutoscaling := conditionStatusValue(ag.Status.Conditions, gmcv2alpha1.ConditionAGCAutoscalingUnavailable)
	prevCollision := conditionStatusValue(ag.Status.Conditions, gmcv2alpha1.ConditionScaleSetNameCollision)
	set := func(condType string, status bool, reason, msg string) {
		s := metav1.ConditionFalse
		if status {
			s = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
			Type: condType, Status: s, Reason: reason, Message: msg, LastTransitionTime: now, ObservedGeneration: gen,
		})
	}

	// Provisioning succeeded, so clear the abnormal conditions. The credential is
	// present for both methods — the mounted GitHub App Secret (possession) or the
	// projected Vault-auth identity (delegation, Q201).
	set(gmcv2alpha1.ConditionCredentialUnavailable, false, gmcv2alpha1.ReasonReconcileSucceeded, "credential is available")
	set(gmcv2alpha1.ConditionDegraded, false, gmcv2alpha1.ReasonReconcileSucceeded, "all AGC control-plane resources reconciled")

	// Egress mode (§H.10). Direct egress is an explicit, auditable status — not an
	// inferred absent field — paired with the advisory EgressUnattributed condition so
	// an operator sees at a glance that the AGC control plane has no per-tenant egress
	// IP identity (the property they opted out of by not setting defaultProxyRef). It
	// does not gate Ready: direct egress is a supported, NetworkPolicy-restricted mode.
	if proxy == nil {
		ag.Status.ProxyMode = gmcv2alpha1.ProxyModeDirect
		set(gmcv2alpha1.ConditionEgressUnattributed, true, gmcv2alpha1.ReasonDirectEgress,
			"no defaultProxyRef: AGC control-plane egress is direct (restricted to DNS + GitHub + the kube API) and has no per-tenant egress IP identity")
	} else {
		ag.Status.ProxyMode = gmcv2alpha1.ProxyModeProxied
		set(gmcv2alpha1.ConditionEgressUnattributed, false, gmcv2alpha1.ReasonProxiedEgress,
			fmt.Sprintf("AGC control-plane egress is attributed to EgressProxy %q", proxy.Name))
	}

	// Managed AGC right-sizing (Q360). Advisory like EgressUnattributed: it never gates
	// Ready, because a gateway whose VerticalPodAutoscaler could not be created is
	// fully functional — it is running on its agcResources sizing, just not being
	// right-sized. The unavailable state is what an operator sees instead of a silent
	// no-op when they opt in on a cluster with no VPA CRDs installed.
	autoscalingMsg := "spec.agcAutoscaling is not set: the AGC is sized by spec.agcResources"
	autoscalingReason := gmcv2alpha1.ReasonAGCAutoscalingDisabled
	switch {
	case autoscaler.unavailable:
		autoscalingReason = gmcv2alpha1.ReasonVPACRDNotInstalled
		autoscalingMsg = fmt.Sprintf("spec.agcAutoscaling is set but the %s CRD is not installed in this cluster, so no VerticalPodAutoscaler was created for %q; the AGC runs on its spec.agcResources sizing. Install the Kubernetes vertical-pod-autoscaler (CRDs + recommender/updater/admission-controller) or remove spec.agcAutoscaling",
			verticalPodAutoscalerGVK.GroupKind().String(), agcVPAName(ag))
	case autoscaler.requested:
		autoscalingReason = gmcv2alpha1.ReasonAGCAutoscalingActive
		autoscalingMsg = fmt.Sprintf("VerticalPodAutoscaler %q manages the AGC container's resource requests in updateMode %s; limits stay as stamped from spec.agcResources",
			agcVPAName(ag), agcVPAUpdateMode(ag.Spec.AGCAutoscaling))
	}
	set(gmcv2alpha1.ConditionAGCAutoscalingUnavailable, autoscaler.unavailable, autoscalingReason, autoscalingMsg)

	agcReason := gmcv2alpha1.ReasonAGCReady
	agcMsg := "AGC Deployment has a ready replica"
	if !agcReady {
		agcReason = gmcv2alpha1.ReasonAGCNotReady
		agcMsg = "AGC Deployment has no ready replica yet"
	}
	set(gmcv2alpha1.ConditionAGCAvailable, agcReady, agcReason, agcMsg)

	// RunnerSetsDegraded rolls the health of the RunnerSets bound to this gateway
	// (spec.gatewayRef) up to the gateway's single pane (Q304), the v2 counterpart of
	// v1's RunnerGroupsDegraded rollup. Advisory like the v1 rollup — it does NOT gate
	// Ready, since the AGC control plane can be healthy while a tenant's RunnerSet is
	// impaired.
	rs := r.evalRunnerSetHealth(ctx, ag)
	set(gmcv2alpha1.ConditionRunnerSetsDegraded, rs.degraded, rs.reason, rs.message)

	// ScaleSetNameCollision reports a scale-set name shared across the gateway's GitHub
	// scope that admission never got to reject (Q849) — see
	// evalScaleSetNameCollisions for which pairs those are and why this reports rather
	// than enforces. Advisory: it does not gate Ready. An unreadable inventory is left
	// as the last verdict rather than written False, so a scope is never reported clean
	// on a read that did not happen.
	collision := r.evalScaleSetNameCollisions(ctx, ag)
	if collision.observed {
		set(gmcv2alpha1.ConditionScaleSetNameCollision, collision.collided, collision.reason, collision.message)
	}

	readyReason := gmcv2alpha1.ReasonReady
	readyMsg := "AGC control plane is available"
	if !agcReady {
		readyReason = gmcv2alpha1.ReasonAGCNotReady
		readyMsg = "waiting for the AGC Deployment to become ready"
	}
	set(gmcv2alpha1.ConditionReady, agcReady, readyReason, readyMsg)

	ag.Status.ObservedGeneration = gen
	if err := r.Status().Update(ctx, ag); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	// Events for the meaningful transitions, emitted only after the status write lands
	// so a conflict-requeue does not double-fire (the next reconcile re-detects the
	// change against the freshly persisted status).
	if newReady := boolConditionStatus(agcReady); prevReady != newReady {
		if agcReady {
			r.recordEvent(ag, corev1.EventTypeNormal, gmcv2alpha1.ReasonReady, "Reconcile",
				"AGC control plane is available")
		} else {
			r.recordEvent(ag, corev1.EventTypeWarning, gmcv2alpha1.ReasonAGCNotReady, "Reconcile",
				"%s", readyMsg)
		}
	}
	// RunnerSetsDegraded rollup transition (Q304): a bound RunnerSet became impaired, or
	// the last impaired set recovered, on the gateway's single pane.
	if newRS := boolConditionStatus(rs.degraded); prevRunnerSets != newRS {
		etype := corev1.EventTypeNormal
		if rs.degraded {
			etype = corev1.EventTypeWarning
		}
		r.recordEvent(ag, etype, rs.reason, "Reconcile", "%s", rs.message)
	}
	// Degraded → recovered: provisioning failed on a prior reconcile and has now
	// succeeded (updateStatus is only reached when reconcileResources returned no error).
	if prevDegraded == metav1.ConditionTrue {
		r.recordEvent(ag, corev1.EventTypeNormal, gmcv2alpha1.ReasonReconcileSucceeded, "Reconcile",
			"AGC control-plane provisioning recovered")
	}
	// Managed autoscaling became (un)satisfiable (Q360): the operator opted in on a
	// cluster with no VPA CRD, or installed the autoscaler and the opt-in took effect.
	// A first-ever reconcile that lands on the (overwhelmingly common) not-opted-in
	// state is not a transition worth an Event — that would put one line of pure noise
	// on every gateway ever created — so the absent→False case is skipped and only a
	// genuine change of a previously recorded status, or the actionable unavailable
	// state itself, is announced.
	newAS := boolConditionStatus(autoscaler.unavailable)
	if firstObservation := prevAutoscaling == ""; prevAutoscaling != newAS && (!firstObservation || autoscaler.unavailable) {
		etype := corev1.EventTypeNormal
		if autoscaler.unavailable {
			etype = corev1.EventTypeWarning
		}
		r.recordEvent(ag, etype, autoscalingReason, "Reconcile", "%s", autoscalingMsg)
	}

	// A scale-set name became shared, or the last shared one was resolved (Q849). The
	// absent→False first observation is skipped like the autoscaling one above, but
	// absent→True is not: a gateway whose first-ever reconcile already sees the
	// collision is the upgrade case this exists for, and it is precisely the one nobody
	// is watching for a transition.
	if newCollision := boolConditionStatus(collision.collided); collision.observed && prevCollision != newCollision &&
		(prevCollision != "" || collision.collided) {
		etype := corev1.EventTypeNormal
		if collision.collided {
			etype = corev1.EventTypeWarning
		}
		r.recordEvent(ag, etype, collision.reason, "Reconcile", "%s", collision.message)
	}

	if !agcReady {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	if autoscaler.unavailable {
		// Re-probe for the CRD on a slow, bounded cadence. The opt-in is satisfiable the
		// moment an operator installs the vertical-pod-autoscaler, and nothing watchable
		// exists to signal that (the GMC deliberately does not watch CRDs), so the
		// gateway converges on its own — at a period that is a poll, not a hot loop.
		return ctrl.Result{RequeueAfter: agcAutoscalerReprobeInterval}, nil
	}
	return ctrl.Result{}, nil
}

// setNotReady records a fail-closed condition (CredentialUnavailable / ProxyNotFound)
// plus Ready=False with the same reason, before provisioning any children.
func (r *ActionsGatewayV2Reconciler) setNotReady(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, condType, reason, msg string) (ctrl.Result, error) {
	// Warn once per transition into the fail-closed state (credential Secret missing or
	// defaultProxyRef'd proxy absent), read before SetStatusCondition mutates the slice.
	if conditionStatusValue(ag.Status.Conditions, condType) != metav1.ConditionTrue {
		r.recordEvent(ag, corev1.EventTypeWarning, reason, "Reconcile", "%s", msg)
	}
	now := metav1.Now()
	gen := ag.Generation
	meta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
		Type: condType, Status: metav1.ConditionTrue, Reason: reason, Message: msg, LastTransitionTime: now, ObservedGeneration: gen,
	})
	meta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
		Type: gmcv2alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reason, Message: msg, LastTransitionTime: now, ObservedGeneration: gen,
	})
	ag.Status.ObservedGeneration = gen
	if err := r.Status().Update(ctx, ag); err != nil && !apierrors.IsConflict(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// setDegraded records Degraded=True naming the failing step and returns the cause
// so the work item is retried with backoff (mirrors the EgressProxy reconciler).
func (r *ActionsGatewayV2Reconciler) setDegraded(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, cause error) (ctrl.Result, error) {
	// Warn once per transition into Degraded, naming the failing provisioning step.
	if !meta.IsStatusConditionTrue(ag.Status.Conditions, gmcv2alpha1.ConditionDegraded) {
		r.recordEvent(ag, corev1.EventTypeWarning, gmcv2alpha1.ReasonProvisioningFailed, "Reconcile", "%s", cause.Error())
	}
	now := metav1.Now()
	gen := ag.Generation
	for _, t := range []string{gmcv2alpha1.ConditionDegraded, gmcv2alpha1.ConditionReady} {
		status := metav1.ConditionTrue
		if t == gmcv2alpha1.ConditionReady {
			status = metav1.ConditionFalse
		}
		meta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
			Type: t, Status: status, Reason: gmcv2alpha1.ReasonProvisioningFailed,
			Message: cause.Error(), LastTransitionTime: now, ObservedGeneration: gen,
		})
	}
	ag.Status.ObservedGeneration = gen
	if err := r.Status().Update(ctx, ag); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, cause
}

// runnerSetHealth carries the computed RunnerSetsDegraded rollup (Q304).
type runnerSetHealth struct {
	degraded bool
	reason   string
	message  string
}

// evalRunnerSetHealth aggregates the health of the RunnerSets bound to this
// ActionsGateway into the RunnerSetsDegraded rollup condition (Q304), the v2
// counterpart of v1's evalRunnerGroupHealth. The rollup is True when at least one
// bound set is impaired, naming the impaired sets and their tripped signals in the
// message so the operator can act from the gateway's single pane without inspecting
// each child. A read failure or zero bound sets yields a healthy (False) rollup —
// the absence of evidence is not an alarm, and other conditions already cover a
// broken gateway.
//
// Unlike v1 (where the GMC owns the RunnerGroups and selects them by owner labels),
// v2 RunnerSets are not owned by the gateway — they only reference it via
// spec.gatewayRef. The binding is resolved by listing the namespace and matching
// gatewayRef.name, the same scoping the AGC applies server-side (§H.16 #1).
func (r *ActionsGatewayV2Reconciler) evalRunnerSetHealth(ctx context.Context, ag *gmcv2alpha1.ActionsGateway) runnerSetHealth {
	var rsList gmcv2alpha1.RunnerSetList
	if err := r.List(ctx, &rsList, client.InNamespace(ag.Namespace)); err != nil {
		return runnerSetHealth{
			reason:  gmcv2alpha1.ReasonAllRunnerSetsHealthy,
			message: fmt.Sprintf("could not read bound RunnerSets: %v", err),
		}
	}
	var bound int
	var impaired []string
	for i := range rsList.Items {
		rs := &rsList.Items[i]
		if rs.Spec.GatewayRef.Name != ag.Name {
			continue
		}
		bound++
		if tripped := runnerSetImpairments(rs.Status.Conditions); len(tripped) > 0 {
			impaired = append(impaired, fmt.Sprintf("%s (%s)", rs.Name, strings.Join(tripped, ", ")))
		}
	}
	if len(impaired) == 0 {
		return runnerSetHealth{
			reason:  gmcv2alpha1.ReasonAllRunnerSetsHealthy,
			message: fmt.Sprintf("all %d bound RunnerSet(s) healthy", bound),
		}
	}
	return runnerSetHealth{
		degraded: true,
		reason:   gmcv2alpha1.ReasonRunnerSetsImpaired,
		message:  fmt.Sprintf("%d of %d RunnerSet(s) impaired: %s", len(impaired), bound, strings.Join(impaired, "; ")),
	}
}

// runnerSetImpairments returns the tripped health signals that mark a bound RunnerSet
// as impaired for the RunnerSetsDegraded rollup (Q304): it cannot serve jobs. A set is
// impaired on either of two independent axes:
//
//  1. Its Ready condition is present and not True for a non-transient reason — a
//     reference did not resolve, a runtime provisioning step failed, or a token could
//     not be obtained, which in v2's model all fold into Ready=False with a reason
//     (unlike v1, where these stand as their own abnormal-is-True conditions). The
//     benign startup reason NoActiveSessions (a healthy set before its first listener
//     comes up) is excluded.
//  2. Any of the abnormal-is-True impairing conditions is True (Q330). The shared
//     listener goroutines push Degraded (revoked/invalid credentials) and
//     RunnerVersionTooOld onto the RunnerSet independently of Ready, so a classic set
//     whose sessions are all rejected as unauthorized converges to the benign
//     Ready=NoActiveSessions while Degraded=True sits on its own condition — axis (1)
//     alone would miss it. gmcv2alpha1.ImpairingConditionTypes is the single source of
//     truth for this set (the v2 counterpart of v1's ImpairingConditionTypes), so it
//     stays in sync with WorkersUnschedulable and any future impairing condition.
//
// The advisory conditions (RateLimited, the WorkerQuota ladder, EgressUnattributed,
// PossibleReapBlockingSidecar) are deliberately excluded from that set — they are
// trade-off or throughput signals, not "the set is broken", and including them would
// flap the rollup on normal operation.
func runnerSetImpairments(conditions []metav1.Condition) []string {
	var tripped []string
	if ready := meta.FindStatusCondition(conditions, gmcv2alpha1.ConditionReady); ready != nil &&
		ready.Status != metav1.ConditionTrue && ready.Reason != gmcv2alpha1.ReasonNoActiveSessions {
		tripped = append(tripped, fmt.Sprintf("%s=%s", gmcv2alpha1.ConditionReady, ready.Reason))
	}
	for _, t := range gmcv2alpha1.ImpairingConditionTypes() {
		if meta.IsStatusConditionTrue(conditions, t) {
			tripped = append(tripped, t)
		}
	}
	return tripped
}

// workerDrainTimeout bounds how long v2 teardown holds the gateway open waiting for
// the AGC to reap its tenant's worker pods (Q547). The AGC only has to observe the
// deletion timestamp through its cache and issue the deletes, so this is generous for
// the healthy path; its real job is to bound the unhealthy one, where no AGC is
// running to reap and every gateway deletion would otherwise pay the full wait.
//
// It sits deliberately under the 2m `kubectl delete --timeout` the e2e teardowns (and
// operators, by convention) give a gateway: a wait that outlasts the caller's budget
// turns the bounded case into a failed delete rather than a slow one.
const workerDrainTimeout = 90 * time.Second

// undrainedRunnerSets returns "<name> (N active, M pending)" for each RunnerSet bound
// to ag that still reports worker pods, in the same list-and-match-gatewayRef shape
// evalRunnerSetHealth uses — v2 RunnerSets are not owned by the gateway, so there are
// no owner labels to select on. A read failure yields nothing: teardown must not wedge
// on an unreadable list, and the deadline would release it anyway.
func (r *ActionsGatewayV2Reconciler) undrainedRunnerSets(ctx context.Context, ag *gmcv2alpha1.ActionsGateway) []string {
	var rsList gmcv2alpha1.RunnerSetList
	if err := r.List(ctx, &rsList, client.InNamespace(ag.Namespace)); err != nil {
		logf.FromContext(ctx).Error(err, "could not read bound RunnerSets for the teardown worker drain")
		return nil
	}
	var undrained []string
	for i := range rsList.Items {
		rs := &rsList.Items[i]
		if rs.Spec.GatewayRef.Name != ag.Name {
			continue
		}
		if rs.Status.ActiveJobs+rs.Status.PendingJobs > 0 {
			undrained = append(undrained, fmt.Sprintf("%s (%d active, %d pending)",
				rs.Name, rs.Status.ActiveJobs, rs.Status.PendingJobs))
		}
	}
	return undrained
}

// reconcileDelete tears down the per-gateway control plane fail-closed (Q125,
// ported from v1's reconcileDelete via Q328): every child is deleted explicitly
// and the finalizer is NOT removed until each one is verifiably gone. The
// namespaced children carry a controller owner reference, so cascade GC would
// eventually remove them too — but a transient GC failure would never be retried
// or surfaced, so teardown does not trust it: a delete error or a lingering child
// (e.g. one held by another controller's finalizer) retains the finalizer, emits a
// TeardownIncomplete event, and requeues, so a live, credentialed AGC Deployment
// is never orphaned by a half-finished teardown. A NotFound is success (the
// desired end state) and every pass is idempotent, so already-deleted resources
// converge to clean. Because every child name is per-gateway (§H.16 #1), deleting
// one gateway touches only its own children, never a neighbor's.
//
// The ClusterRunnerTemplate ClusterRoleBinding is cluster-scoped and cannot carry
// an owner ref to a namespaced object, so this explicit delete is its ONLY
// cleanup. The metrics mTLS Secrets are left to owner-ref GC — the GMC
// deliberately holds no delete verb on secrets (mirrors v1's proxy TLS Secret).
// RunnerSets reference the gateway but are not owned by it, so they are not
// deleted — they degrade to Ready=False/GatewayNotFound via their own watch. Their
// worker pods do have to go, though, and only the AGC can reap them: teardown holds
// until it has (undrainedRunnerSets, Q547) before deleting the AGC out from under
// them.
func (r *ActionsGatewayV2Reconciler) reconcileDelete(ctx context.Context, ag *gmcv2alpha1.ActionsGateway) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Worker drain (Q547), before any child is deleted. The tenant's worker pods are
	// owned by RunnerSets, which survive gateway deletion by design, so this AGC is
	// their only reaper — and it reaps them itself once it observes the gateway's
	// deletion timestamp. Deleting its Deployment (and, moments later, its RoleBinding
	// and ServiceAccount) before it gets there strands the pods with their
	// do-not-disrupt annotations, pinning a billable node until the kubelet's
	// activeDeadlineSeconds fires up to maxWorkerLifetime later. So teardown waits for
	// the counts to reach zero — which happens as soon as the deletes are ISSUED, since
	// the AGC's reaper stops counting a pod that already carries a deletion timestamp.
	if undrained := r.undrainedRunnerSets(ctx, ag); len(undrained) > 0 {
		if r.nowFunc()().Before(ag.DeletionTimestamp.Add(workerDrainTimeout)) {
			r.recordEvent(ag, corev1.EventTypeNormal, "WaitingForWorkerDrain", "ReconcileDelete",
				"Holding gateway teardown until the AGC reaps its worker pods: %s",
				strings.Join(undrained, ", "))
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		// The AGC never got there — crashed, scaled to zero, or never healthy. Proceed
		// rather than hold the gateway forever, and name what is being left behind:
		// those pods now have no reaper and are bounded only by maxWorkerLifetime.
		r.recordEvent(ag, corev1.EventTypeWarning, "WorkerDrainTimeout", "ReconcileDelete",
			"Worker pods did not drain within %s of deletion; tearing down anyway, so any pod still "+
				"running is orphaned until its spec.maxWorkerLifetime deadline: %s",
			workerDrainTimeout, strings.Join(undrained, ", "))
	}

	var errs []error
	var lingering []string
	// del deletes one child and verifies it is gone. A delete error is collected;
	// a child that survives its (accepted) delete — held by a foreign finalizer —
	// is recorded as lingering so the finalizer is retained until it drains.
	del := func(obj client.Object, kind, ns, name string) {
		obj.SetNamespace(ns)
		obj.SetName(name)
		if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			log.Error(err, "failed to delete resource during teardown", "kind", kind, "namespace", ns, "name", name)
			errs = append(errs, fmt.Errorf("%s %s: %w", kind, name, err))
			return
		}
		if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, obj); err == nil {
			lingering = append(lingering, fmt.Sprintf("%s %s", kind, name))
		} else if client.IgnoreNotFound(err) != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", kind, name, err))
		}
	}

	ns := ag.Namespace
	// Managed VerticalPodAutoscaler (Q360). Owner-referenced, so GC would remove it too,
	// but teardown deletes and verifies it like every other child. On a cluster with no
	// autoscaling.k8s.io CRD there is nothing to tear down, and attempting the delete
	// would NoMatch-error and retain the cleanup finalizer forever — so the probe skips
	// it entirely. A probe failure is a real (transient) apiserver error and is collected.
	if installed, probeErr := r.vpaCRDInstalled(); probeErr != nil {
		errs = append(errs, fmt.Errorf("VerticalPodAutoscaler %s: %w", agcVPAName(ag), probeErr))
	} else if installed {
		vpa := &unstructured.Unstructured{}
		vpa.SetGroupVersionKind(verticalPodAutoscalerGVK)
		del(vpa, "VerticalPodAutoscaler", ns, agcVPAName(ag))
	}
	del(&rbacv1.ClusterRoleBinding{}, "ClusterRoleBinding", "", clusterRunnerTemplateReaderBindingName(ag))
	del(&appsv1.Deployment{}, "Deployment", ns, agcNameV2(ag))
	del(&corev1.Service{}, "Service", ns, agcNameV2(ag))
	del(&networkingv1.NetworkPolicy{}, "NetworkPolicy", ns, agcNameV2(ag))
	del(&networkingv1.NetworkPolicy{}, "NetworkPolicy", ns, workloadNPNameV2(ag))
	del(&rbacv1.RoleBinding{}, "RoleBinding", ns, agcNameV2(ag))
	del(&corev1.ServiceAccount{}, "ServiceAccount", ns, agcNameV2(ag))
	del(&corev1.ServiceAccount{}, "ServiceAccount", ns, workerSANameV2(ag))

	if len(errs) > 0 || len(lingering) > 0 {
		var detail []string
		if err := errors.Join(errs...); err != nil {
			detail = append(detail, err.Error())
		}
		if len(lingering) > 0 {
			detail = append(detail, "still present: "+strings.Join(lingering, ", "))
		}
		r.recordEvent(ag, corev1.EventTypeWarning, "TeardownIncomplete", "ReconcileDelete",
			"Gateway teardown could not confirm deletion of all owned resources in namespace %q; retaining the cleanup finalizer and requeuing until teardown is clean: %s",
			ns, strings.Join(detail, "; "))
		if len(errs) > 0 {
			// Returning the error requeues with backoff; the finalizer stays in place.
			return ctrl.Result{}, errors.Join(errs...)
		}
		// Every delete was accepted but a child is still draining (a foreign
		// finalizer holds it): poll until it is verifiably gone, like v1's
		// wait-for-RunnerGroups pass.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// All owned resources are confirmed gone — remove the finalizer so the CR can
	// be garbage-collected. Re-read first so the Update targets the current version.
	if err := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: ag.Name}, ag); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	controllerutil.RemoveFinalizer(ag, gmcv2alpha1.ActionsGatewayFinalizer)
	return ctrl.Result{}, r.Update(ctx, ag)
}

// SetupWithManager wires the v2 ActionsGateway reconciler: it owns the AGC
// Deployment (so its readiness change refreshes status), watches the credential
// Secret (metadata-only — no secret bodies cached), and watches EgressProxy so a
// gateway sitting Ready=False/ProxyNotFound flips when its defaultProxyRef'd proxy
// appears.
func (r *ActionsGatewayV2Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gmcv2alpha1.ActionsGateway{}).
		// Serialise reconciles explicitly, matching the v1 gateway reconciler
		// (Q328): the shared apply/cert helpers assume a single writer per
		// controller. Raising this requires auditing them for safety under
		// concurrent reconciles of distinct ActionsGateways first.
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Owns(&appsv1.Deployment{}).
		WatchesMetadata(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToActionsGateways),
		).
		Watches(
			&gmcv2alpha1.EgressProxy{},
			handler.EnqueueRequestsFromMapFunc(r.proxyToActionsGateways),
		).
		// Watch bound RunnerSets so a child's health change refreshes the parent's
		// RunnerSetsDegraded rollup (Q304). The predicate drops the high-frequency
		// status churn (activeSessions/pendingJobs) that cannot move the rollup, so the
		// gateway only re-reconciles when a set's impaired signature actually changes.
		// RunnerSet status carries no secret material, so caching it is cheap.
		Watches(
			&gmcv2alpha1.RunnerSet{},
			handler.EnqueueRequestsFromMapFunc(r.runnerSetToActionsGateway),
			builder.WithPredicates(runnerSetImpairmentChanged()),
		).
		Named("actionsgateway-v2").
		Complete(r)
}

// runnerSetToActionsGateway maps a RunnerSet event to the ActionsGateway it binds to
// via spec.gatewayRef (same namespace), so a child's health change refreshes the
// parent's RunnerSetsDegraded rollup promptly (Q304). A set with no gatewayRef.name
// maps to no request.
func (r *ActionsGatewayV2Reconciler) runnerSetToActionsGateway(_ context.Context, obj client.Object) []ctrl.Request {
	rs, ok := obj.(*gmcv2alpha1.RunnerSet)
	if !ok || rs.Spec.GatewayRef.Name == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: rs.Namespace, Name: rs.Spec.GatewayRef.Name}}}
}

// runnerSetImpairmentChanged restricts the RunnerSet watch to the events that can move
// the RunnerSetsDegraded rollup (Q304): create, delete, and updates that change a set's
// impaired signature (its tripped health signals). RunnerSet status is written on
// nearly every AGC reconcile (activeSessions/pendingJobs), so without this the GMC
// would reconcile on high-frequency churn that cannot change the rollup — the v2
// counterpart of v1's runnerGroupImpairingConditionsChanged.
func runnerSetImpairmentChanged() predicate.Predicate {
	signature := func(obj client.Object) (string, bool) {
		rs, ok := obj.(*gmcv2alpha1.RunnerSet)
		if !ok {
			return "", false
		}
		return strings.Join(runnerSetImpairments(rs.Status.Conditions), "|"), true
	}
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldSig, ok1 := signature(e.ObjectOld)
			newSig, ok2 := signature(e.ObjectNew)
			if !ok1 || !ok2 {
				return true
			}
			return oldSig != newSig
		},
	}
}

// secretToActionsGateways enqueues any v2 ActionsGateway in the Secret's namespace
// whose credentials.githubApp names it, so a credential Secret create/delete re-reconciles.
func (r *ActionsGatewayV2Reconciler) secretToActionsGateways(ctx context.Context, obj client.Object) []ctrl.Request {
	return r.gatewaysMatching(ctx, obj.GetNamespace(), func(ag *gmcv2alpha1.ActionsGateway) bool {
		return ag.Spec.GitHubAppSecretName() == obj.GetName()
	})
}

// proxyToActionsGateways enqueues any v2 ActionsGateway whose defaultProxyRef names
// this EgressProxy, in any namespace.
//
// The scan is cluster-wide rather than scoped to the proxy's namespace because a
// grant (or its withdrawal) has to reach consumers that by definition live
// elsewhere (M4, §H.9) — a namespace-scoped list would leave a revoked gateway
// sitting Ready with wiring it is no longer entitled to. It also fixes a latent
// mismatch in the same-namespace case: matching on name alone enqueued gateways for
// a same-named proxy in an unrelated namespace, so the namespace is now compared
// too.
func (r *ActionsGatewayV2Reconciler) proxyToActionsGateways(ctx context.Context, obj client.Object) []ctrl.Request {
	var list gmcv2alpha1.ActionsGatewayList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range list.Items {
		ag := &list.Items[i]
		ref := ag.Spec.DefaultProxyRef
		if ref == nil || ref.Name != obj.GetName() {
			continue
		}
		refNS := ref.Namespace
		if refNS == "" {
			refNS = ag.Namespace
		}
		if refNS != obj.GetNamespace() {
			continue
		}
		reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: ag.Namespace, Name: ag.Name,
		}})
	}
	return reqs
}

func (r *ActionsGatewayV2Reconciler) gatewaysMatching(ctx context.Context, ns string, match func(*gmcv2alpha1.ActionsGateway) bool) []ctrl.Request {
	var list gmcv2alpha1.ActionsGatewayList
	if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range list.Items {
		ag := &list.Items[i]
		if match(ag) {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ag.Namespace, Name: ag.Name}})
		}
	}
	return reqs
}
