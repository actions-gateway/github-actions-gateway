// +kubebuilder:rbac:groups=actions-gateway.com,resources=egressproxies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=actions-gateway.com,resources=egressproxies/status,verbs=get;update;patch
// CNI-native FQDN egress (Q208/Q245): for an FQDN egressPolicyMode the EgressProxy
// reconciler creates/patches/deletes the CNI-native policy chosen by the operator
// --fqdn-policy-backend — a CiliumNetworkPolicy, a Calico NetworkPolicy, or a GKE
// FQDNNetworkPolicy — scoped to the GitHub FQDNs. These grants are no-ops on a cluster
// without the corresponding CRD installed (the default CIDR mode emits no such object).
// +kubebuilder:rbac:groups=cilium.io,resources=ciliumnetworkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=projectcalico.org,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.gke.io,resources=fqdnnetworkpolicies,verbs=get;list;watch;create;update;patch;delete
// EgressProxy owns its proxy Deployment/Service/HPA/PDB/NetworkPolicy and the
// self-signed proxy TLS Secret via controller owner references (§H.8); the
// deployments/services/hpa/pdb/networkpolicies/secrets write verbs — and the
// resourcequotas get;list;watch the quota watch needs (Q82/Q326) — are already
// granted to the GMC ClusterRole by the ActionsGateway reconciler markers, which
// controller-gen aggregates into the same manager-role. EgressProxy carries NO
// finalizer: deletion degrades referrers rather than blocking, and owner-ref GC
// reclaims the children (§H.8) — so no egressproxies/finalizers grant.
//
// Cross-namespace sharing (M4, §H.9) projects the proxy's public CA and connection
// facts into granted consumer namespaces as ConfigMaps, and deletes them when a
// grant is revoked — so the pre-existing `configmaps: get` grant widens to the full
// write set. The projected object carries no owner reference (the GC ignores a
// cross-namespace owner), which is why delete is needed rather than cascade GC.
// No `watch`: these reads go through the uncached APIReader, so no ConfigMap
// informer is started and the name-pinned egress-allowlist scoping stays intact.
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;create;update;patch;delete

package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// EgressProxyReconciler reconciles a v2alpha1 EgressProxy into a standalone proxy
// pool, owning the proxy Deployment/Service/HPA/PDB/NetworkPolicy and the
// self-signed proxy TLS Secret via controller owner references for clean cascade
// GC (§H.8). It also owns the cross-namespace sharing handshake (M4, §H.9): the CA
// projections into namespaces spec.sharing.allowedNamespaces consents to, and the
// ingress rules admitting them. The reconciler mirrors v1's inline ActionsGateway
// proxy-provisioning runtime semantics — the v2 API re-shapes the surface without
// changing what the proxy pool does.
type EgressProxyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// IPCache supplies cached GitHub IP CIDRs for the proxy NetworkPolicy egress
	// allowlist. Populated and refreshed by IPRangeReconciler; reads here never
	// perform network I/O. A nil cache or empty snapshot is tolerated — the
	// IPRangeReconciler patches any NetworkPolicy created without CIDRs once its
	// first fetch lands (mirrors the ActionsGatewayReconciler contract).
	IPCache    *IPRangeCache
	ProxyImage string
	// APIReader is the manager's uncached reader, used for every read of a projected
	// cross-namespace share ConfigMap (§H.9). It must not be the cached client: the
	// GMC pins its ConfigMap informer to one name in its own namespace
	// (buildCacheOptions), so a cached label-selected List would quietly return
	// nothing and the prune that revokes a withdrawn grant would find no projections
	// to delete. Nil falls back to the cached client for unit tests, which run
	// against a fake client with no such scoping.
	APIReader client.Reader
	// EnableServiceMonitor gates creation of the per-EgressProxy Prometheus-Operator
	// ServiceMonitor that scrapes the proxy's mTLS metrics port (Q324). The
	// monitoring.coreos.com CRD is an optional, operator-installed prerequisite, so
	// creating a ServiceMonitor unconditionally would fail on clusters without it.
	// Fed from the same --enable-tenant-service-monitors flag as the AGC path; when
	// false, any previously-created ServiceMonitor is pruned.
	EnableServiceMonitor bool
	// EgressStaleThreshold is the age past which the shared GitHub IP-range refresh
	// loop's last success trips the EgressRulesStale condition (Q157). Zero uses
	// DefaultEgressStaleThreshold. Shares the value the ActionsGatewayReconciler is
	// given so v1 and v2 proxies report staleness on the same window.
	EgressStaleThreshold time.Duration
	// Recorder emits Kubernetes Events on the EgressProxy (provisioning failure/recovery,
	// proxy-pool readiness transitions, and TLS-cert issuance/rotation) so they surface in
	// `kubectl describe`. May be nil in unit tests; callers go through recordEvent.
	Recorder events.EventRecorder
	// FQDNBackend is the operator-selected CNI/platform mechanism used to enforce an
	// FQDN egressPolicyMode intent (the GMC --fqdn-policy-backend flag). The zero value
	// is treated as FQDNBackendNone: FQDN intent emits no CNI-native policy (and is
	// rejected at admission). The deprecated CiliumFQDN/CalicoFQDN intents ignore this
	// field and always emit their namesake policy (Q245).
	FQDNBackend FQDNBackend
}

// Reconcile drives an EgressProxy toward its desired proxy pool. EgressProxy uses
// no finalizer (§H.8), so deletion is handled by owner-reference garbage collection
// of the children; a delete in flight simply returns without re-provisioning.
func (r *EgressProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ep gmcv2alpha1.EgressProxy
	if err := r.Get(ctx, req.NamespacedName, &ep); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// No finalizer: children carry an owner reference and are GC'd by the
	// apiserver. Do not re-provision an object that is being deleted.
	if !ep.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	var githubCIDRs []net.IPNet
	if r.IPCache != nil {
		githubCIDRs = r.IPCache.Snapshot()
	}

	// The GitHub host a referrer binds to this proxy lives on the referrer, not here,
	// so the hostname allowlists must be assembled from the referrer graph (Q506 #2).
	// A read failure degrades rather than emitting a policy missing a tenant's host.
	gitHubHosts, err := resolveReferrerGitHubHosts(ctx, r.Client, &ep)
	if err != nil {
		return r.setDegraded(ctx, &ep, &provisioningError{step: "resolve referrer GitHub hosts", err: err})
	}

	if err := r.reconcileResources(ctx, &ep, githubCIDRs, gitHubHosts); err != nil {
		return r.setDegraded(ctx, &ep, err)
	}

	return r.updateStatus(ctx, &ep, gitHubHosts)
}

// reconcileResources creates or patches every child of the EgressProxy. Each
// failure is wrapped with the failing step so updateStatus/setDegraded can name it
// in the Degraded condition. The proxy TLS cert is ensured first so the Deployment
// can mount it on the very first reconcile.
func (r *EgressProxyReconciler) reconcileResources(ctx context.Context, ep *gmcv2alpha1.EgressProxy, githubCIDRs []net.IPNet, gitHubHosts []string) error {
	if err := r.ensureProxyCert(ctx, ep); err != nil {
		return &provisioningError{step: "ensure proxy TLS cert", err: err}
	}
	if err := r.ensureMetricsCerts(ctx, ep); err != nil {
		return &provisioningError{step: "ensure metrics TLS certs", err: err}
	}
	if err := r.applyDeployment(ctx, ep, buildEgressProxyDeployment(ep, r.ProxyImage, gitHubHosts)); err != nil {
		return &provisioningError{step: "apply proxy Deployment", err: err}
	}
	if err := r.applyService(ctx, ep, buildEgressProxyService(ep)); err != nil {
		return &provisioningError{step: "apply proxy Service", err: err}
	}
	if err := r.reconcileHPA(ctx, ep); err != nil {
		return &provisioningError{step: "reconcile proxy HPA", err: err}
	}
	if err := r.applyPDB(ctx, ep, buildEgressProxyPDB(ep)); err != nil {
		return &provisioningError{step: "apply proxy PDB", err: err}
	}
	if err := r.applyNetworkPolicy(ctx, ep, buildEgressProxyNetworkPolicy(ep, githubCIDRs)); err != nil {
		return &provisioningError{step: "apply proxy NetworkPolicy", err: err}
	}
	if err := r.reconcileFQDNPolicy(ctx, ep, gitHubHosts); err != nil {
		return &provisioningError{step: "reconcile FQDN egress policy", err: err}
	}
	if err := r.applyOrPruneServiceMonitor(ctx, ep); err != nil {
		return &provisioningError{step: "reconcile proxy ServiceMonitor", err: err}
	}
	// Last: the projection publishes trust material a consumer acts on, so it runs
	// only once the cert, Service and NetworkPolicy it describes are in place.
	if err := r.reconcileProxyShares(ctx, ep); err != nil {
		return &provisioningError{step: "reconcile cross-namespace proxy shares", err: err}
	}
	return nil
}

// reconcileFQDNPolicy emits or removes the CNI-native FQDN egress policy (Q208/Q245) to
// match the (spec.egressPolicyMode, --fqdn-policy-backend) resolution. It applies the one
// resolved CNI policy (Cilium/Calico/GKE) and removes every other one sharing the FQDN
// policy name; in CIDR mode (the default), when FQDN intent has no operator backend, or
// when the GMC is not managing this proxy's policy, it removes all three. Those removals
// make a mode/backend switch converge cleanly. Deletes tolerate a missing object and a
// missing CRD (a CIDR-mode cluster need not have any FQDN CRD installed), so the default
// path never fails on their absence.
func (r *EgressProxyReconciler) reconcileFQDNPolicy(ctx context.Context, ep *gmcv2alpha1.EgressProxy, gitHubHosts []string) error {
	managed := ep.Spec.ManagedNetworkPolicy == nil || *ep.Spec.ManagedNetworkPolicy
	emitter := fqdnEmitNone
	if managed {
		emitter = resolveFQDNEmitter(egressModeOf(ep.Spec), r.FQDNBackend)
	}

	// Each CNI-native policy shares the "<ep>-proxy-fqdn" name, so at most one is applied
	// and the others are deleted — that convergence is what makes a mode/backend switch
	// clean. The GKE FQDNNetworkPolicy is additive-allow, so its fail-closed guarantee
	// relies on the base standard NetworkPolicy (always applied above) default-denying
	// GitHub egress; the GKE object only ever widens the union (Q245).
	policies := []struct {
		gvk   schema.GroupVersionKind
		build func(*gmcv2alpha1.EgressProxy, []string) *unstructured.Unstructured
		want  bool
	}{
		{ciliumNetworkPolicyGVK, buildEgressProxyCiliumNetworkPolicy, emitter == fqdnEmitCilium},
		{calicoNetworkPolicyGVK, buildEgressProxyCalicoNetworkPolicy, emitter == fqdnEmitCalico},
		{gkeFQDNNetworkPolicyGVK, buildEgressProxyGKEFQDNNetworkPolicy, emitter == fqdnEmitGKE},
	}
	for _, p := range policies {
		if p.want {
			if err := r.applyCNIPolicy(ctx, ep, p.build(ep, gitHubHosts)); err != nil {
				return err
			}
			continue
		}
		if err := r.deleteCNIPolicy(ctx, ep.Namespace, egressProxyFQDNPolicyName(ep), p.gvk); err != nil {
			return err
		}
	}
	return nil
}

// applyCNIPolicy creates or patches an unstructured CNI-native egress policy, writing
// only the controller-managed labels + spec and stamping a controller owner reference
// on the EgressProxy so the apiserver garbage-collects it on EgressProxy delete (§H.8).
func (r *EgressProxyReconciler) applyCNIPolicy(ctx context.Context, ep *gmcv2alpha1.EgressProxy, desired *unstructured.Unstructured) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(desired.GroupVersionKind())
	return applyManagedChild(ctx, r.Client, r.Scheme, ep, obj, desired, func() error {
		spec, found, err := unstructured.NestedFieldCopy(desired.Object, "spec")
		if err != nil || !found {
			return fmt.Errorf("desired CNI policy missing spec: %w", err)
		}
		return unstructured.SetNestedField(obj.Object, spec, "spec")
	})
}

// deleteCNIPolicy removes a CNI-native egress policy by GVK+name, tolerating both a
// missing object (already gone) and a missing CRD (the cluster does not run that CNI),
// so the default CIDR path is never blocked by the Cilium/Calico CRDs being absent.
func (r *EgressProxyReconciler) deleteCNIPolicy(ctx context.Context, namespace, name string, gvk schema.GroupVersionKind) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	if err := r.Delete(ctx, obj); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil
		}
		return err
	}
	return nil
}

// ensureProxyCert ensures the EgressProxy's proxy TLS Secret exists and holds a
// cert valid for at least proxyCertRenewBefore, (re)generating a self-signed cert
// when the Secret is missing, unparseable, or near expiry. Mirrors v1's
// ensureProxyCert; the private key never leaves the cluster.
func (r *EgressProxyReconciler) ensureProxyCert(ctx context.Context, ep *gmcv2alpha1.EgressProxy) error {
	secretName := egressProxyTLSSecretName(ep)
	var existing corev1.Secret
	getErr := r.Get(ctx, types.NamespacedName{Namespace: ep.Namespace, Name: secretName}, &existing)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return getErr
	}

	reason := "secret missing"
	if !apierrors.IsNotFound(getErr) {
		reason = "unparseable cert"
		if cert, err := parseCertPEM(existing.Data[corev1.TLSCertKey]); err == nil {
			if time.Until(cert.NotAfter) > proxyCertRenewBefore {
				return nil // valid and not near expiry
			}
			reason = "near expiry"
		}
	}

	logf.FromContext(ctx).V(1).Info("issuing EgressProxy TLS cert", "secret", secretName, "reason", reason)

	certPEM, keyPEM, err := generateEgressProxyCert(ep.Namespace, proxyResourceName(ep))
	if err != nil {
		return fmt.Errorf("generate EgressProxy cert: %w", err)
	}
	if err := r.applyOwnedSecret(ctx, ep, buildEgressProxyCertSecret(ep, certPEM, keyPEM)); err != nil {
		return err
	}
	// The steady-state path returns early above, so this Event fires only on an actual
	// issuance or rotation, not on every reconcile.
	r.recordEvent(ep, corev1.EventTypeNormal, "ProxyCertificateIssued", "EnsureProxyCert",
		"issued proxy TLS certificate (%s)", reason)
	return nil
}

// ensureMetricsCerts ensures the EgressProxy's metrics mTLS bundle exists and is not
// near expiry. It (re)generates a per-EgressProxy CA + server cert (SANs on the
// "<ep>-proxy" Service) + scraper client cert and writes two Secrets: the server
// bundle (egressProxyMetricsTLSSecretName, mounted into the proxy pod) and the scraper
// client bundle (egressProxyMetricsClientSecretName, published for the monitoring
// stack). Both carry a controller owner reference so they are GC'd on EgressProxy
// delete (§H.8). The whole bundle regenerates together when either Secret is missing
// or the server cert is within metricsCertRenewBefore of expiry — mirroring v1's
// ensureMetricsCerts and the EgressProxy's own ensureProxyCert. No key material is
// logged. The CA is per-EgressProxy, never shared with the AGC or another tenant.
func (r *EgressProxyReconciler) ensureMetricsCerts(ctx context.Context, ep *gmcv2alpha1.EgressProxy) error {
	var serverSec corev1.Secret
	serverErr := r.Get(ctx, types.NamespacedName{Namespace: ep.Namespace, Name: egressProxyMetricsTLSSecretName(ep)}, &serverSec)
	if serverErr != nil && !apierrors.IsNotFound(serverErr) {
		return serverErr
	}
	var clientSec corev1.Secret
	clientErr := r.Get(ctx, types.NamespacedName{Namespace: ep.Namespace, Name: egressProxyMetricsClientSecretName(ep)}, &clientSec)
	if clientErr != nil && !apierrors.IsNotFound(clientErr) {
		return clientErr
	}

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

	logf.FromContext(ctx).V(1).Info("generating EgressProxy metrics mTLS bundle",
		"secret", egressProxyMetricsTLSSecretName(ep), "reason", reason)

	// generateMetricsCertsV2 is keyed on (namespace, Service name) and is reused here
	// for the standalone proxy metrics listener; the "<ep>-proxy" Service name gives
	// the server cert the SANs the ServiceMonitor's serverName pins to.
	bundle, err := generateMetricsCertsV2(ep.Namespace, proxyResourceName(ep))
	if err != nil {
		return fmt.Errorf("generate metrics certs: %w", err)
	}
	if err := r.applyOwnedSecret(ctx, ep, buildEgressProxyMetricsTLSSecret(ep, bundle)); err != nil {
		return fmt.Errorf("metrics server Secret: %w", err)
	}
	if err := r.applyOwnedSecret(ctx, ep, buildEgressProxyMetricsClientSecret(ep, bundle)); err != nil {
		return fmt.Errorf("metrics client Secret: %w", err)
	}
	// The steady-state path returns early above, so this Event fires only on an actual
	// issuance or rotation, not on every reconcile.
	r.recordEvent(ep, corev1.EventTypeNormal, "MetricsCertificateIssued", "EnsureMetricsCerts",
		"issued metrics mTLS certificate (%s)", reason)
	return nil
}

// applyOrPruneServiceMonitor reconciles the per-EgressProxy ServiceMonitor (Q324)
// according to EnableServiceMonitor: when enabled it creates/patches the monitor
// (presenting this EgressProxy's scraper client bundle for mTLS); when disabled it
// best-effort deletes any existing one so flipping the flag off leaves nothing stale.
// If the monitoring.coreos.com CRD is not installed, the apply fails with a NoMatch
// error — downgraded to a Warning Event and a logged note rather than failing the
// whole reconcile, so a missing optional scrape prerequisite never blocks provisioning
// (a NoMatch on the delete path means there is nothing to prune).
func (r *EgressProxyReconciler) applyOrPruneServiceMonitor(ctx context.Context, ep *gmcv2alpha1.EgressProxy) error {
	if !r.EnableServiceMonitor {
		sm := &unstructured.Unstructured{}
		sm.SetGroupVersionKind(serviceMonitorGVK)
		sm.SetNamespace(ep.Namespace)
		sm.SetName(egressProxyMetricsServiceMonitorName(ep))
		if err := r.Delete(ctx, sm); err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			return err
		}
		return nil
	}

	if err := r.applyServiceMonitor(ctx, ep, buildEgressProxyServiceMonitor(ep)); err != nil {
		if meta.IsNoMatchError(err) {
			logf.FromContext(ctx).Info("skipping EgressProxy ServiceMonitor: monitoring.coreos.com CRD not installed",
				"name", egressProxyMetricsServiceMonitorName(ep))
			r.recordEvent(ep, corev1.EventTypeWarning, "ServiceMonitorCRDMissing", "ApplyServiceMonitor",
				"ServiceMonitor scraping is enabled but the monitoring.coreos.com ServiceMonitor CRD is not installed; install the Prometheus Operator to enable proxy metrics scraping. Skipping %q.",
				egressProxyMetricsServiceMonitorName(ep))
			return nil
		}
		return err
	}
	return nil
}

// applyServiceMonitor creates or patches the unstructured EgressProxy ServiceMonitor,
// writing only the controller-managed labels + spec and stamping a controller owner
// reference so the apiserver garbage-collects it on EgressProxy delete (§H.8). Mirrors
// the other apply* helpers and v1's applyServiceMonitor.
func (r *EgressProxyReconciler) applyServiceMonitor(ctx context.Context, ep *gmcv2alpha1.EgressProxy, desired *unstructured.Unstructured) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(serviceMonitorGVK)
	return applyManagedChild(ctx, r.Client, r.Scheme, ep, obj, desired, func() error {
		spec, _, _ := unstructured.NestedMap(desired.Object, "spec")
		return unstructured.SetNestedMap(obj.Object, spec, "spec")
	})
}

// recordEvent emits a Kubernetes Event on the EgressProxy when a Recorder is wired.
// The Recorder may be nil in unit tests, so callers go through here rather than
// dereferencing it directly (mirrors the AGC reconcilers' recordEvent).
func (r *EgressProxyReconciler) recordEvent(ep *gmcv2alpha1.EgressProxy, eventtype, reason, action, note string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(ep, nil, eventtype, reason, action, note, args...)
}

// The apply* helpers mirror the ActionsGatewayReconciler pattern: CreateOrPatch
// re-reads the object, the mutate closure writes only the controller-managed
// fields, and every child gets a controller owner reference on the EgressProxy so
// the apiserver garbage-collects it when the EgressProxy is deleted (§H.8).
//
// A whole-Spec overwrite is only safe where no other controller owns a subfield.
// That holds for the HPA, PDB and NetworkPolicy below (their specs are derived
// wholly from the EgressProxy spec, and none carries a scale subresource) but not
// for the Service (the apiserver assigns ClusterIP) nor the Deployment (the HPA
// owns `.spec.replicas`) — both of which assign fields selectively instead.

// applyDeployment creates or patches the proxy pool Deployment. It is the target
// of the pool's autoscaler — the managed HPA, or the operator's own when
// managedAutoscaling is false — so `.spec.replicas` is assigned selectively
// rather than by a blanket spec overwrite: see assignHPATargetDeploymentSpec
// (Q283) and its externally-scaled variant (Q173).
//
// spec.selector is immutable, so a pool provisioned before Q582 — whose selector
// still carries v1's `app: actions-gateway-proxy` alongside the identity label —
// cannot be patched onto the narrowed selector: the apiserver rejects the update
// and the reconcile wedges Degraded forever. Such a pool is deleted and recreated,
// mirroring applyRoleBinding's immutable-roleRef path. The delete is explicitly
// Background: Orphan would strand a ReplicaSet that keeps replacing pods no owner
// reclaims, and those pods hold the hostname anti-affinity slots the replacement
// needs. The replacement's pods still wait out the old pods' termination grace
// period on that anti-affinity, so the pool is briefly unavailable — a one-time
// cost, and one a migration never pays: gag-migrate creates the EgressProxy after
// the upgrade, so its pool is born with the narrowed selector.
func (r *EgressProxyReconciler) applyDeployment(ctx context.Context, ep *gmcv2alpha1.EgressProxy, desired *appsv1.Deployment) error {
	obj := &appsv1.Deployment{}
	err := applyManagedChild(ctx, r.Client, r.Scheme, ep, obj, desired, func() error {
		if obj.ResourceVersion != "" && !equality.Semantic.DeepEqual(obj.Spec.Selector, desired.Spec.Selector) {
			return errDeploymentSelectorImmutable
		}
		if egressProxyManagedAutoscaling(ep) {
			assignHPATargetDeploymentSpec(&obj.Spec, desired.Spec)
		} else {
			assignExternallyScaledDeploymentSpec(&obj.Spec, desired.Spec)
		}
		return nil
	})
	if errors.Is(err, errDeploymentSelectorImmutable) {
		if delErr := r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !apierrors.IsNotFound(delErr) {
			return delErr
		}
		if refErr := controllerutil.SetControllerReference(ep, desired, r.Scheme); refErr != nil {
			return refErr
		}
		// An AlreadyExists here means the delete has not yet drained from the
		// apiserver; the reconcile requeues and the create lands on the retry.
		return r.Create(ctx, desired)
	}
	return err
}

func (r *EgressProxyReconciler) applyService(ctx context.Context, ep *gmcv2alpha1.EgressProxy, desired *corev1.Service) error {
	obj := &corev1.Service{}
	return applyManagedChild(ctx, r.Client, r.Scheme, ep, obj, desired, func() error {
		// Preserve server-assigned Spec fields (ClusterIP); set only managed fields.
		obj.Spec.Type = desired.Spec.Type
		obj.Spec.Selector = desired.Spec.Selector
		obj.Spec.Ports = desired.Spec.Ports
		return nil
	})
}

// reconcileHPA applies the managed HPA or, when managedAutoscaling is false
// (Q173), deletes any previously managed one so the pool's scale is left to the
// operator's own autoscaler (KEDA, VPA, or a custom HPA) without the GMC's HPA
// fighting it over `.spec.replicas`. The managed "<ep>-proxy" HPA name stays
// reserved either way; the delete tolerates an absent object, so the
// bring-your-own path is a steady-state no-op.
func (r *EgressProxyReconciler) reconcileHPA(ctx context.Context, ep *gmcv2alpha1.EgressProxy) error {
	if egressProxyManagedAutoscaling(ep) {
		return r.applyHPA(ctx, ep, buildEgressProxyHPA(ep))
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Namespace: ep.Namespace, Name: proxyResourceName(ep)}}
	if err := r.Delete(ctx, hpa); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *EgressProxyReconciler) applyHPA(ctx context.Context, ep *gmcv2alpha1.EgressProxy, desired *autoscalingv2.HorizontalPodAutoscaler) error {
	obj := &autoscalingv2.HorizontalPodAutoscaler{}
	return applyManagedChild(ctx, r.Client, r.Scheme, ep, obj, desired, func() error {
		obj.Spec = desired.Spec
		return nil
	})
}

func (r *EgressProxyReconciler) applyPDB(ctx context.Context, ep *gmcv2alpha1.EgressProxy, desired *policyv1.PodDisruptionBudget) error {
	obj := &policyv1.PodDisruptionBudget{}
	return applyManagedChild(ctx, r.Client, r.Scheme, ep, obj, desired, func() error {
		obj.Spec = desired.Spec
		return nil
	})
}

func (r *EgressProxyReconciler) applyNetworkPolicy(ctx context.Context, ep *gmcv2alpha1.EgressProxy, desired *networkingv1.NetworkPolicy) error {
	obj := &networkingv1.NetworkPolicy{}
	return applyManagedChild(ctx, r.Client, r.Scheme, ep, obj, desired, func() error {
		obj.Spec = desired.Spec
		return nil
	})
}

func (r *EgressProxyReconciler) applyOwnedSecret(ctx context.Context, ep *gmcv2alpha1.EgressProxy, desired *corev1.Secret) error {
	obj := &corev1.Secret{}
	return applyManagedChild(ctx, r.Client, r.Scheme, ep, obj, desired, func() error {
		obj.Type = desired.Type
		obj.Data = desired.Data
		return nil
	})
}

// updateStatus reads the proxy Deployment's readiness and writes the uniform v2
// status/condition contract (§H.7): readyReplicas, observedGeneration, a Ready
// condition (True once readyReplicas ≥ minReplicas), and a cleared Degraded
// condition (the reconcile reached here, so provisioning succeeded).
func (r *EgressProxyReconciler) updateStatus(ctx context.Context, ep *gmcv2alpha1.EgressProxy, gitHubHosts []string) (ctrl.Result, error) {
	var dep appsv1.Deployment
	readyReplicas := int32(0)
	if err := r.Get(ctx, types.NamespacedName{Namespace: ep.Namespace, Name: proxyResourceName(ep)}, &dep); err == nil {
		readyReplicas = dep.Status.ReadyReplicas
	}

	// Readiness floor: with managed autoscaling, spec.minReplicas (the HPA floor).
	// With bring-your-own autoscaling (Q173) the external scaler owns the desired
	// count, so readiness is measured against the Deployment's own spec.replicas —
	// 0/0 ready is Ready (an intentional external scale-to-zero, not a wedge) and
	// a scale below spec.minReplicas never flaps Ready false.
	minReplicas := egressProxyMinReplicas(ep)
	if !egressProxyManagedAutoscaling(ep) && dep.Spec.Replicas != nil {
		minReplicas = *dep.Spec.Replicas
	}
	ready := readyReplicas >= minReplicas
	now := metav1.Now()
	gen := ep.Generation

	// Snapshot the pre-reconcile status of the conditions whose transitions we surface
	// as Events, before setCondition mutates them, so we emit only on a genuine change.
	prevReady := conditionStatusValue(ep.Status.Conditions, gmcv2alpha1.ConditionReady)
	prevDegraded := conditionStatusValue(ep.Status.Conditions, gmcv2alpha1.ConditionDegraded)
	prevEgressGap := conditionStatusValue(ep.Status.Conditions, gmcv2alpha1.ConditionGitHubEgressIncomplete)

	setCondition := func(condType string, status bool, reason, msg string) {
		s := metav1.ConditionFalse
		if status {
			s = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             s,
			Reason:             reason,
			Message:            msg,
			LastTransitionTime: now,
			ObservedGeneration: gen,
		})
	}

	setCondition(gmcv2alpha1.ConditionDegraded, false, gmcv2alpha1.ReasonReconcileSucceeded, "all child resources reconciled")
	readyReason := gmcv2alpha1.ReasonProxyReady
	if !ready {
		readyReason = gmcv2alpha1.ReasonProxyNotReady
	}
	setCondition(gmcv2alpha1.ConditionReady, ready, readyReason, fmt.Sprintf("%d/%d proxy pods ready", readyReplicas, minReplicas))

	// ProxyQuota{Pressure,Exceeded} (Q82) and EgressRulesStale (Q157), ported from the
	// v1 ActionsGateway onto the v2 standalone proxy pool. All advisory — they do NOT
	// gate Ready; the pool keeps serving at its current scale. Pressure/Exceeded are
	// mutually exclusive (error supersedes warning).
	qc := r.evalEgressProxyQuota(ctx, ep, &dep)
	setCondition(gmcv2alpha1.ConditionProxyQuotaPressure, qc.pressure, qc.pressureReason, qc.pressureMessage)
	setCondition(gmcv2alpha1.ConditionProxyQuotaExceeded, qc.exceeded, qc.exceededReason, qc.exceededMessage)
	es := r.evalEgressProxyEgressRulesStale(ep, now.Time)
	setCondition(gmcv2alpha1.ConditionEgressRulesStale, es.stale, es.reason, es.message)
	// GitHubEgressIncomplete (Q506 #3): the one GHES gap the GMC cannot close, named
	// rather than left to surface as a connect timeout with no cause.
	eg := evalGitHubEgressIncomplete(ep, gitHubHosts)
	setCondition(gmcv2alpha1.ConditionGitHubEgressIncomplete, eg.incomplete, eg.reason, eg.message)

	ep.Status.ReadyReplicas = readyReplicas
	ep.Status.ObservedGeneration = gen

	if err := r.Status().Update(ctx, ep); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	// Events for the meaningful transitions, emitted only after the status write lands
	// so a conflict-requeue does not double-fire.
	if newReady := boolConditionStatus(ready); prevReady != newReady {
		etype := corev1.EventTypeNormal
		if !ready {
			etype = corev1.EventTypeWarning
		}
		r.recordEvent(ep, etype, readyReason, "Reconcile", "%d/%d proxy pods ready", readyReplicas, minReplicas)
	}
	// The egress gap is an operator obligation, not a reconcile failure, so it is
	// announced once per transition rather than every reconcile.
	if newGap := boolConditionStatus(eg.incomplete); prevEgressGap != newGap {
		etype := corev1.EventTypeNormal
		if eg.incomplete {
			etype = corev1.EventTypeWarning
		}
		r.recordEvent(ep, etype, eg.reason, "Reconcile", "%s", eg.message)
	}
	// Degraded → recovered: a prior reconcile failed to provision and this one succeeded
	// (updateStatus is only reached when reconcileResources returned no error).
	if prevDegraded == metav1.ConditionTrue {
		r.recordEvent(ep, corev1.EventTypeNormal, gmcv2alpha1.ReasonReconcileSucceeded, "Reconcile",
			"proxy pool provisioning recovered")
	}

	// The Owns(&appsv1.Deployment{}) watch refreshes status when pods become ready,
	// but a bounded requeue guards against a missed event while the pool scales up.
	if !ready {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	// Re-check the egress posture on a bounded cadence: in CIDR mode nothing watches
	// the IP-range refresh loop, so a stall would never flip EgressRulesStale (Q157);
	// in the FQDN modes the CNI-native policy has no Owns() watch, so out-of-band
	// drift would never be repaired once the pool is Ready (Q326).
	if d := r.egressProxyEgressRecheckRequeue(ep); d > 0 {
		return ctrl.Result{RequeueAfter: d}, nil
	}
	return ctrl.Result{}, nil
}

// setDegraded records a Degraded=True condition naming the failing provisioning
// step and returns the underlying error so the work item is retried with backoff.
// Mirrors the ActionsGatewayReconciler's Q156 behavior on the v2 contract.
func (r *EgressProxyReconciler) setDegraded(ctx context.Context, ep *gmcv2alpha1.EgressProxy, cause error) (ctrl.Result, error) {
	// Warn once per transition into Degraded, naming the failing provisioning step.
	if !meta.IsStatusConditionTrue(ep.Status.Conditions, gmcv2alpha1.ConditionDegraded) {
		r.recordEvent(ep, corev1.EventTypeWarning, gmcv2alpha1.ReasonProvisioningFailed, "Reconcile", "%s", cause.Error())
	}
	now := metav1.Now()
	gen := ep.Generation
	meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
		Type:               gmcv2alpha1.ConditionDegraded,
		Status:             metav1.ConditionTrue,
		Reason:             gmcv2alpha1.ReasonProvisioningFailed,
		Message:            cause.Error(),
		LastTransitionTime: now,
		ObservedGeneration: gen,
	})
	meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
		Type:               gmcv2alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             gmcv2alpha1.ReasonProvisioningFailed,
		Message:            cause.Error(),
		LastTransitionTime: now,
		ObservedGeneration: gen,
	})
	ep.Status.ObservedGeneration = gen
	if err := r.Status().Update(ctx, ep); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		// Return the status error; the original cause is already recorded in-memory
		// and will be retried on the requeue.
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, cause
}

// SetupWithManager wires the EgressProxy reconciler: it reconciles EgressProxy
// objects and owns its non-secret children, so an owned Deployment's readiness
// change (or any child drift) re-triggers a reconcile. The proxy TLS Secret is
// deliberately NOT owned via a watch: a full Secret informer would buffer secret
// material in the in-process cache, which this project forbids (the
// ActionsGatewayReconciler uses a metadata-only Secret watch for the same reason).
// The Secret still carries a controller owner reference for GC, and ensureProxyCert
// re-creates it on the next reconcile if it is removed out-of-band.
func (r *EgressProxyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gmcv2alpha1.EgressProxy{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&networkingv1.NetworkPolicy{}).
		// Reconcile when an admin changes a namespace ResourceQuota's .spec.hard so
		// the ProxyQuota{Pressure,Exceeded} conditions refresh promptly (Q82/Q326)
		// — mirrors the v1 ActionsGateway watch. Full (not metadata-only): the
		// predicate needs .spec.hard; quotas are small and carry no secret material.
		Watches(
			&corev1.ResourceQuota{},
			handler.EnqueueRequestsFromMapFunc(r.quotaToEgressProxies),
			builder.WithPredicates(quotaHardChangedPredicate()),
		).
		// A referrer's githubURL feeds this proxy's hostname allowlists (Q506 #2), and
		// the binding is created by the referrer, not by the proxy — so a gateway or
		// RunnerSet applied after the proxy must requeue it, or a GHES tenant's host
		// never reaches the emitted policy. githubURL itself is immutable (§H.15), so
		// the events that matter are create/delete and a proxyRef change; the
		// generation predicate drops the status churn that would otherwise requeue
		// every proxy in the namespace.
		Watches(
			&gmcv2alpha1.ActionsGateway{},
			handler.EnqueueRequestsFromMapFunc(r.referrerToEgressProxies),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&gmcv2alpha1.RunnerSet{},
			handler.EnqueueRequestsFromMapFunc(r.referrerToEgressProxies),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Named("egressproxy").
		Complete(r)
}
