package controller

import (
	"context"
	"fmt"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// nsPSAFieldManager is the Server-Side Apply field manager the NamespacePSAReconciler
// owns the six pod-security.kubernetes.io/* label keys with on v2 tenant namespaces.
// It is distinct from the v1 ActionsGatewayReconciler's psaFieldManager: v2 moves the
// PSA source-of-truth off the per-gateway CR onto the namespace, so a different
// controller owns the keys, and v1/v2 tenants live in separate namespaces (so the two
// managers never contend on the same object).
const nsPSAFieldManager = "gmc-namespace-psa"

// NamespacePSAReconciler stamps Pod Security Admission labels on managed v2 tenant
// namespaces from their actions-gateway.com/security-profile label. v2 relocates the
// security profile off the per-gateway ActionsGateway.spec onto the namespace (Q175 /
// appendix-h §H.16 #7): Pod Security Admission is a namespace-scoped control, so under
// multi-gateway-per-namespace two gateways would otherwise fight over the single
// namespace PSA label. The operator selects the profile via the namespace label; this
// reconciler reconciles it into the enforce/warn/audit labels. The
// gmc-namespace-security-profile-guard ValidatingAdmissionPolicy guards the operator's
// edits to that label (enum, no-silent-downgrade, privileged eligibility), so this
// reconciler trusts the label value and only stamps.
//
// It owns no children, sets no finalizer, and writes no status: a core Namespace is not
// owned by the GMC, and the only effect is the SSA-managed PSA label keys.
type NamespacePSAReconciler struct {
	client.Client
	// Recorder emits Kubernetes Events on the namespace when a PSA stamp is denied
	// (e.g. the namespace lost its tenant marker). May be nil in unit tests.
	Recorder events.EventRecorder
}

// Reconcile stamps the PSA labels on a managed v2 tenant namespace from its
// security-profile label. It is a no-op (and not requeued) for namespaces that are
// gone, being deleted, or no longer carry the v2 tenant marker — the watch predicate
// already scopes enqueues to v2 tenant namespaces, but the live object is re-checked
// here to avoid acting on a namespace whose marker was just removed.
func (r *NamespacePSAReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ns corev1.Namespace
	if err := r.Get(ctx, req.NamespacedName, &ns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ns.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if ns.Labels[gmcv2alpha1.TenantNamespaceMarkerLabel] != gmcv2alpha1.TenantNamespaceMarkerValue {
		// Marker removed between enqueue and reconcile; nothing to stamp.
		return ctrl.Result{}, nil
	}

	if err := r.applyNamespacePSA(ctx, &ns); err != nil {
		return ctrl.Result{}, err
	}
	log.V(1).Info("reconciled namespace Pod Security Admission labels",
		"namespace", ns.Name,
		"securityProfile", gmcv2alpha1.EffectiveSecurityProfile(ns.Labels[gmcv2alpha1.SecurityProfileLabel]))
	return ctrl.Result{}, nil
}

// applyNamespacePSA stamps the six Pod Security Admission labels on the tenant
// namespace via Server-Side Apply, declaring ownership only of those keys. The profile
// comes from the namespace's security-profile label (baseline default when absent). It
// mirrors the v1 ActionsGatewayReconciler.applyNamespacePSA conflict handling: a first
// apply without ForceOwnership surfaces an out-of-band admin edit as a conflict, which
// is re-applied with ForceOwnership to re-establish the controller's invariant.
func (r *NamespacePSAReconciler) applyNamespacePSA(ctx context.Context, ns *corev1.Namespace) error {
	profile := gmcv2alpha1.EffectiveSecurityProfile(ns.Labels[gmcv2alpha1.SecurityProfileLabel])
	desired := corev1ac.Namespace(ns.Name).WithLabels(map[string]string{
		"pod-security.kubernetes.io/enforce":         profile,
		"pod-security.kubernetes.io/enforce-version": "latest",
		"pod-security.kubernetes.io/warn":            profile,
		"pod-security.kubernetes.io/warn-version":    "latest",
		"pod-security.kubernetes.io/audit":           profile,
		"pod-security.kubernetes.io/audit-version":   "latest",
	})

	err := r.Apply(ctx, desired, client.FieldOwner(nsPSAFieldManager))
	if err == nil {
		return nil
	}
	if apierrors.IsForbidden(err) {
		// The namespace-psa-guard ValidatingAdmissionPolicy denied the patch — almost
		// always because the namespace lost its tenant marker. Surface an actionable
		// signal; a retry will not help until the marker is restored.
		if r.Recorder != nil {
			r.Recorder.Eventf(ns, nil, corev1.EventTypeWarning, "NamespaceMarkerMissing", "ApplyPSALabels",
				"Cannot stamp Pod Security Admission labels on namespace %q: the admission policy denied the update. Label the namespace with %s=%s to mark it a managed tenant namespace: %v",
				ns.Name, gmcv2alpha1.TenantNamespaceMarkerLabel, gmcv2alpha1.TenantNamespaceMarkerValue, err)
		}
		return fmt.Errorf("stamp PSA labels on namespace %q: %w", ns.Name, err)
	}
	if !apierrors.IsConflict(err) {
		return fmt.Errorf("stamp PSA labels on namespace %q: %w", ns.Name, err)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(ns, nil, corev1.EventTypeWarning, "PSALabelsOverridden", "ReapplyPSALabels",
			"Reconciling Pod Security Admission labels on namespace %q to securityProfile=%q after detecting an out-of-band modification: %v",
			ns.Name, profile, err)
	}
	return r.Apply(ctx, desired, client.FieldOwner(nsPSAFieldManager), client.ForceOwnership)
}

// SetupWithManager registers the reconciler, scoping its watch to managed v2 tenant
// namespaces so non-tenant namespaces (kube-system, etc.) never enqueue.
func (r *NamespacePSAReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}, builder.WithPredicates(v2TenantNamespacePredicate())).
		Named("namespace-psa").
		Complete(r)
}

// v2TenantNamespacePredicate matches only namespaces carrying the v2 tenant marker, so
// the reconciler reconciles exactly the namespaces whose PSA posture it owns. The
// security-profile label is intentionally NOT part of the predicate: an absent label is
// the valid baseline default the reconciler must still stamp.
func v2TenantNamespacePredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetLabels()[gmcv2alpha1.TenantNamespaceMarkerLabel] == gmcv2alpha1.TenantNamespaceMarkerValue
	})
}

var _ reconcile.Reconciler = &NamespacePSAReconciler{}
