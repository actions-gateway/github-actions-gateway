/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// +kubebuilder:rbac:groups=actions-gateway.com,resources=egressproxies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=actions-gateway.com,resources=egressproxies/status,verbs=get;update;patch
// CNI-native FQDN egress (Q208): in CiliumFQDN/CalicoFQDN mode the EgressProxy
// reconciler creates/patches/deletes a CiliumNetworkPolicy or Calico NetworkPolicy
// scoped to the GitHub FQDNs. These grants are no-ops on a cluster without the
// corresponding CRD installed (the default CIDR mode emits neither object).
// +kubebuilder:rbac:groups=cilium.io,resources=ciliumnetworkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=projectcalico.org,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// EgressProxy owns its proxy Deployment/Service/HPA/PDB/NetworkPolicy and the
// self-signed proxy TLS Secret via controller owner references (§H.8); the
// deployments/services/hpa/pdb/networkpolicies/secrets write verbs are already
// granted to the GMC ClusterRole by the ActionsGateway reconciler markers, which
// controller-gen aggregates into the same manager-role. EgressProxy carries NO
// finalizer: deletion degrades referrers rather than blocking, and owner-ref GC
// reclaims the children (§H.8) — so no egressproxies/finalizers grant.

package controller

import (
	"context"
	"fmt"
	"net"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// EgressProxyReconciler reconciles a v2alpha1 EgressProxy into a standalone proxy
// pool, owning the proxy Deployment/Service/HPA/PDB/NetworkPolicy and the
// self-signed proxy TLS Secret via controller owner references for clean cascade
// GC (§H.8). It is same-namespace only at this milestone (M2): cross-namespace
// sharing (spec.sharing.allowedNamespaces) is deferred to M4. The reconciler
// mirrors v1's inline ActionsGateway proxy-provisioning runtime semantics — the v2
// API re-shapes the surface without changing what the proxy pool does.
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

	if err := r.reconcileResources(ctx, &ep, githubCIDRs); err != nil {
		return r.setDegraded(ctx, &ep, err)
	}

	return r.updateStatus(ctx, &ep)
}

// reconcileResources creates or patches every child of the EgressProxy. Each
// failure is wrapped with the failing step so updateStatus/setDegraded can name it
// in the Degraded condition. The proxy TLS cert is ensured first so the Deployment
// can mount it on the very first reconcile.
func (r *EgressProxyReconciler) reconcileResources(ctx context.Context, ep *gmcv2alpha1.EgressProxy, githubCIDRs []net.IPNet) error {
	if err := r.ensureProxyCert(ctx, ep); err != nil {
		return &provisioningError{step: "ensure proxy TLS cert", err: err}
	}
	if err := r.applyDeployment(ctx, ep, buildEgressProxyDeployment(ep, r.ProxyImage)); err != nil {
		return &provisioningError{step: "apply proxy Deployment", err: err}
	}
	if err := r.applyService(ctx, ep, buildEgressProxyService(ep)); err != nil {
		return &provisioningError{step: "apply proxy Service", err: err}
	}
	if err := r.applyHPA(ctx, ep, buildEgressProxyHPA(ep)); err != nil {
		return &provisioningError{step: "apply proxy HPA", err: err}
	}
	if err := r.applyPDB(ctx, ep, buildEgressProxyPDB(ep)); err != nil {
		return &provisioningError{step: "apply proxy PDB", err: err}
	}
	if err := r.applyNetworkPolicy(ctx, ep, buildEgressProxyNetworkPolicy(ep, githubCIDRs)); err != nil {
		return &provisioningError{step: "apply proxy NetworkPolicy", err: err}
	}
	if err := r.reconcileFQDNPolicy(ctx, ep); err != nil {
		return &provisioningError{step: "reconcile FQDN egress policy", err: err}
	}
	return nil
}

// reconcileFQDNPolicy emits or removes the CNI-native FQDN egress policy (Q208) to
// match spec.egressPolicyMode. In CiliumFQDN/CalicoFQDN mode it applies the matching
// CNI policy and removes the other one; in CIDR mode (the default) or when the GMC is
// not managing this proxy's policy, it removes both. The opposite-mode/disabled
// removals make a mode switch converge cleanly. Deletes tolerate a missing object and
// a missing CRD (a CIDR-mode cluster need not have the Cilium/Calico CRDs installed),
// so the default path never fails on their absence.
func (r *EgressProxyReconciler) reconcileFQDNPolicy(ctx context.Context, ep *gmcv2alpha1.EgressProxy) error {
	managed := ep.Spec.ManagedNetworkPolicy == nil || *ep.Spec.ManagedNetworkPolicy
	mode := egressModeOf(ep.Spec)

	wantCilium := managed && mode == gmcv2alpha1.EgressPolicyModeCiliumFQDN
	wantCalico := managed && mode == gmcv2alpha1.EgressPolicyModeCalicoFQDN

	if wantCilium {
		if err := r.applyCNIPolicy(ctx, ep, buildEgressProxyCiliumNetworkPolicy(ep)); err != nil {
			return err
		}
	} else if err := r.deleteCNIPolicy(ctx, ep.Namespace, egressProxyFQDNPolicyName(ep), ciliumNetworkPolicyGVK); err != nil {
		return err
	}

	if wantCalico {
		if err := r.applyCNIPolicy(ctx, ep, buildEgressProxyCalicoNetworkPolicy(ep)); err != nil {
			return err
		}
	} else if err := r.deleteCNIPolicy(ctx, ep.Namespace, egressProxyFQDNPolicyName(ep), calicoNetworkPolicyGVK); err != nil {
		return err
	}
	return nil
}

// applyCNIPolicy creates or patches an unstructured CNI-native egress policy, writing
// only the controller-managed labels + spec and stamping a controller owner reference
// on the EgressProxy so the apiserver garbage-collects it on EgressProxy delete (§H.8).
func (r *EgressProxyReconciler) applyCNIPolicy(ctx context.Context, ep *gmcv2alpha1.EgressProxy, desired *unstructured.Unstructured) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(desired.GroupVersionKind())
	obj.SetNamespace(desired.GetNamespace())
	obj.SetName(desired.GetName())
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.SetLabels(desired.GetLabels())
		spec, found, err := unstructured.NestedFieldCopy(desired.Object, "spec")
		if err != nil || !found {
			return fmt.Errorf("desired CNI policy missing spec: %w", err)
		}
		if err := unstructured.SetNestedField(obj.Object, spec, "spec"); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(ep, obj, r.Scheme)
	})
	return err
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
	return r.applyOwnedSecret(ctx, ep, buildEgressProxyCertSecret(ep, certPEM, keyPEM))
}

// The apply* helpers mirror the ActionsGatewayReconciler pattern: CreateOrPatch
// re-reads the object, the mutate closure writes only the controller-managed
// fields, and every child gets a controller owner reference on the EgressProxy so
// the apiserver garbage-collects it when the EgressProxy is deleted (§H.8).

func (r *EgressProxyReconciler) applyDeployment(ctx context.Context, ep *gmcv2alpha1.EgressProxy, desired *appsv1.Deployment) error {
	obj := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		obj.Spec = desired.Spec
		return controllerutil.SetControllerReference(ep, obj, r.Scheme)
	})
	return err
}

func (r *EgressProxyReconciler) applyService(ctx context.Context, ep *gmcv2alpha1.EgressProxy, desired *corev1.Service) error {
	obj := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		// Preserve server-assigned Spec fields (ClusterIP); set only managed fields.
		obj.Spec.Type = desired.Spec.Type
		obj.Spec.Selector = desired.Spec.Selector
		obj.Spec.Ports = desired.Spec.Ports
		return controllerutil.SetControllerReference(ep, obj, r.Scheme)
	})
	return err
}

func (r *EgressProxyReconciler) applyHPA(ctx context.Context, ep *gmcv2alpha1.EgressProxy, desired *autoscalingv2.HorizontalPodAutoscaler) error {
	obj := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		obj.Spec = desired.Spec
		return controllerutil.SetControllerReference(ep, obj, r.Scheme)
	})
	return err
}

func (r *EgressProxyReconciler) applyPDB(ctx context.Context, ep *gmcv2alpha1.EgressProxy, desired *policyv1.PodDisruptionBudget) error {
	obj := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		obj.Spec = desired.Spec
		return controllerutil.SetControllerReference(ep, obj, r.Scheme)
	})
	return err
}

func (r *EgressProxyReconciler) applyNetworkPolicy(ctx context.Context, ep *gmcv2alpha1.EgressProxy, desired *networkingv1.NetworkPolicy) error {
	obj := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		obj.Spec = desired.Spec
		return controllerutil.SetControllerReference(ep, obj, r.Scheme)
	})
	return err
}

func (r *EgressProxyReconciler) applyOwnedSecret(ctx context.Context, ep *gmcv2alpha1.EgressProxy, desired *corev1.Secret) error {
	obj := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		obj.Type = desired.Type
		obj.Data = desired.Data
		return controllerutil.SetControllerReference(ep, obj, r.Scheme)
	})
	return err
}

// updateStatus reads the proxy Deployment's readiness and writes the uniform v2
// status/condition contract (§H.7): readyReplicas, observedGeneration, a Ready
// condition (True once readyReplicas ≥ minReplicas), and a cleared Degraded
// condition (the reconcile reached here, so provisioning succeeded).
func (r *EgressProxyReconciler) updateStatus(ctx context.Context, ep *gmcv2alpha1.EgressProxy) (ctrl.Result, error) {
	var dep appsv1.Deployment
	readyReplicas := int32(0)
	if err := r.Get(ctx, types.NamespacedName{Namespace: ep.Namespace, Name: proxyResourceName(ep)}, &dep); err == nil {
		readyReplicas = dep.Status.ReadyReplicas
	}

	minReplicas := egressProxyMinReplicas(ep)
	ready := readyReplicas >= minReplicas
	now := metav1.Now()
	gen := ep.Generation

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

	ep.Status.ReadyReplicas = readyReplicas
	ep.Status.ObservedGeneration = gen

	if err := r.Status().Update(ctx, ep); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	// The Owns(&appsv1.Deployment{}) watch refreshes status when pods become ready,
	// but a bounded requeue guards against a missed event while the pool scales up.
	if !ready {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// setDegraded records a Degraded=True condition naming the failing provisioning
// step and returns the underlying error so the work item is retried with backoff.
// Mirrors the ActionsGatewayReconciler's Q156 behavior on the v2 contract.
func (r *EgressProxyReconciler) setDegraded(ctx context.Context, ep *gmcv2alpha1.EgressProxy, cause error) (ctrl.Result, error) {
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
		Named("egressproxy").
		Complete(r)
}
