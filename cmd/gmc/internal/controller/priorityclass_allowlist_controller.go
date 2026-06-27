package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
)

// PriorityClassAllowlistConfigMapKey is the ConfigMap data key the
// PriorityClassAllowlistReconciler reads. Its value is a list of cluster-scoped
// PriorityClass names — newline-, comma-, or whitespace-separated — that augment
// the static --allowed-priority-classes flag allowlist (Q188).
const PriorityClassAllowlistConfigMapKey = "allowedPriorityClasses"

// PriorityClassAllowlistReconciler watches a single designated ConfigMap and
// reconciles its contents into the dynamic half of the PriorityClass admission
// allowlist (Q188). This lets a platform admin add an allowed PriorityClass name
// without editing the GMC --allowed-priority-classes flag and rolling out the
// controller — the change takes effect on the next watch event, restart-free.
//
// The reconciler runs in EVERY GMC replica (NeedLeaderElection=false), not just
// the leader: the validating admission webhook is served by every ready replica,
// so each one must hold the current effective allowlist. The dynamic set is
// per-process in-memory state, not a cluster object, so there is nothing for the
// replicas to contend over.
//
// Fail-safe contract (the allowlist is a cross-tenant-isolation guardrail, so a
// bad ConfigMap must never widen it): on a missing/deleted ConfigMap, a missing
// data key, or any invalid entry, the reconciler clears the dynamic set
// (Allowlist.SetDynamic(nil)) so only the static flag allowlist remains in force,
// and logs the reason. A malformed ConfigMap is rejected WHOLESALE — the valid
// subset is not partially applied — so a typo can never silently smuggle in a
// class alongside garbage.
type PriorityClassAllowlistReconciler struct {
	client.Client
	// ConfigMapName is the name of the watched ConfigMap. Required.
	ConfigMapName string
	// Namespace is the namespace the ConfigMap lives in — the GMC's own install
	// namespace, so only a platform admin (not a tenant) can write it. Required.
	Namespace string
	// Allowlist is the shared allowlist the admission webhook reads. The
	// reconciler owns its dynamic half. Required.
	Allowlist *allowlist.PriorityClassAllowlist
}

// Reconcile reads the designated ConfigMap and updates the dynamic allowlist.
// See the type doc for the fail-safe contract.
func (r *PriorityClassAllowlistReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: r.Namespace, Name: r.ConfigMapName}
	if err := r.Get(ctx, key, &cm); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// Absent/deleted ConfigMap: fall back to the static flag allowlist.
			r.Allowlist.SetDynamic(nil)
			log.Info("PriorityClass allowlist ConfigMap not present; using the static --allowed-priority-classes flag only",
				"configMap", key.String(),
				"effectiveAllowlist", r.Allowlist.Names())
			return ctrl.Result{}, nil
		}
		// A transient read error (not NotFound): requeue rather than mutate the
		// allowlist on incomplete information. The previously applied dynamic set
		// stays in force until we can read authoritative state.
		return ctrl.Result{}, fmt.Errorf("get PriorityClass allowlist ConfigMap %s: %w", key, err)
	}

	names, err := parsePriorityClassAllowlistConfigMap(&cm)
	if err != nil {
		// Malformed contents: fail safe to the static flag allowlist and log a
		// warning. Do NOT requeue — the data is invalid, not transiently
		// unreadable; the next watch event (an admin fixing it) drives the retry.
		r.Allowlist.SetDynamic(nil)
		log.Info("WARNING: PriorityClass allowlist ConfigMap is invalid; ignoring it and using the static --allowed-priority-classes flag only",
			"configMap", key.String(),
			"reason", err.Error(),
			"effectiveAllowlist", r.Allowlist.Names())
		return ctrl.Result{}, nil
	}

	r.Allowlist.SetDynamic(names)
	log.Info("applied PriorityClass allowlist from ConfigMap",
		"configMap", key.String(),
		"dynamicEntries", names,
		"effectiveAllowlist", r.Allowlist.Names())
	return ctrl.Result{}, nil
}

// parsePriorityClassAllowlistConfigMap extracts and validates the PriorityClass
// names from the ConfigMap. The value under PriorityClassAllowlistConfigMapKey is
// split on commas, newlines, and surrounding whitespace. Every entry must be a
// valid RFC 1123 DNS subdomain (the form Kubernetes requires of a PriorityClass
// name); any invalid entry fails the whole parse so the caller falls back to the
// static allowlist rather than partially applying malformed config.
//
// A missing data key is an error (the ConfigMap exists but carries nothing this
// reconciler can use — treated as malformed, fail-safe). A present-but-empty
// value is valid and yields an empty list (the admin has explicitly added no
// dynamic classes), leaving the static base untouched.
func parsePriorityClassAllowlistConfigMap(cm *corev1.ConfigMap) ([]string, error) {
	raw, ok := cm.Data[PriorityClassAllowlistConfigMapKey]
	if !ok {
		return nil, fmt.Errorf("missing required data key %q", PriorityClassAllowlistConfigMapKey)
	}

	seen := make(map[string]bool)
	var names []string
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	}) {
		name := strings.TrimSpace(field)
		if name == "" {
			continue
		}
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			return nil, fmt.Errorf("invalid PriorityClass name %q: %s", name, strings.Join(errs, "; "))
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// SetupWithManager registers the reconciler, scoping its watch to the single
// designated ConfigMap so unrelated ConfigMaps never enqueue, and disabling
// leader election so the dynamic allowlist is maintained in every replica that
// serves the admission webhook.
func (r *PriorityClassAllowlistReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.ConfigMapName == "" || r.Namespace == "" || r.Allowlist == nil {
		return fmt.Errorf("PriorityClassAllowlistReconciler requires ConfigMapName, Namespace, and Allowlist")
	}
	runUnconditionally := false
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}, builder.WithPredicates(r.configMapPredicate())).
		Named("priorityclass-allowlist").
		WithOptions(controller.Options{NeedLeaderElection: &runUnconditionally}).
		Complete(r)
}

// configMapPredicate matches only the single designated ConfigMap (by namespace
// and name), so the reconciler is enqueued for exactly the object whose contents
// it sources the dynamic allowlist from.
func (r *PriorityClassAllowlistReconciler) configMapPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == r.Namespace && obj.GetName() == r.ConfigMapName
	})
}

var _ reconcile.Reconciler = &PriorityClassAllowlistReconciler{}
