package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
)

// PriorityClassAllowlistReconciler watches a single designated, cluster-scoped
// PriorityClassAllowlist CR and reconciles its contents into the dynamic half of
// the PriorityClass admission allowlist (Q188). This lets a platform admin add an
// allowed PriorityClass name without editing the GMC --allowed-priority-classes
// flag and rolling out the controller — the change takes effect on the next watch
// event, restart-free.
//
// The same object is the `paramKind` of the priorityclass-allowlist-guard
// ValidatingAdmissionPolicy, so the webhook and the policy read one source and
// cannot drift. It replaced a ConfigMap in Q492: a CORE-type paramKind is
// permanently broken for the rest of a kube-apiserver process once the set of
// bindings naming it goes empty for one refresh tick, which `helm uninstall` does
// (Q444). Background: docs/design/05-security.md (PriorityClass allowlist).
//
// The reconciler runs in EVERY GMC replica (NeedLeaderElection=false), not just
// the leader: the validating admission webhook is served by every ready replica,
// so each one must hold the current effective allowlist. The dynamic set is
// per-process in-memory state, not a cluster object, so there is nothing for the
// replicas to contend over.
//
// Fail-safe contract (the allowlist is a cross-tenant-isolation guardrail, so a
// bad object must never widen it): on a missing/deleted CR, or any invalid entry,
// the reconciler clears the dynamic set (Allowlist.SetDynamic(nil)) so only the
// static flag allowlist remains in force, and logs the reason. A malformed list is
// rejected WHOLESALE — the valid subset is not partially applied — so a typo can
// never silently smuggle in a class alongside garbage. The CRD schema already
// constrains each entry to a DNS 1123 subdomain, so the invalid-entry path is now
// defence in depth rather than the primary gate; it still runs, because an object
// written before a schema change (or through a path that bypassed validation)
// must not be trusted.
type PriorityClassAllowlistReconciler struct {
	client.Client
	// Name is the name of the watched cluster-scoped PriorityClassAllowlist.
	// Required.
	Name string
	// Allowlist is the shared allowlist the admission webhook reads. The
	// reconciler owns its dynamic half. Required.
	Allowlist *allowlist.PriorityClassAllowlist
}

// Reconcile reads the designated PriorityClassAllowlist and updates the dynamic
// allowlist. See the type doc for the fail-safe contract.
func (r *PriorityClassAllowlistReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var pca v2beta1.PriorityClassAllowlist
	key := types.NamespacedName{Name: r.Name}
	if err := r.Get(ctx, key, &pca); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// Absent/deleted object: fall back to the static flag allowlist.
			r.Allowlist.SetDynamic(nil)
			log.Info("PriorityClassAllowlist not present; using the static --allowed-priority-classes flag only",
				"priorityClassAllowlist", r.Name,
				"effectiveAllowlist", r.Allowlist.Names())
			return ctrl.Result{}, nil
		}
		// A transient read error (not NotFound): requeue rather than mutate the
		// allowlist on incomplete information. The previously applied dynamic set
		// stays in force until we can read authoritative state.
		return ctrl.Result{}, fmt.Errorf("get PriorityClassAllowlist %s: %w", r.Name, err)
	}

	names, err := parsePriorityClassAllowlist(&pca)
	if err != nil {
		// Malformed contents: fail safe to the static flag allowlist and log a
		// warning. Do NOT requeue — the data is invalid, not transiently
		// unreadable; the next watch event (an admin fixing it) drives the retry.
		r.Allowlist.SetDynamic(nil)
		log.Info("WARNING: PriorityClassAllowlist is invalid; ignoring it and using the static --allowed-priority-classes flag only",
			"priorityClassAllowlist", r.Name,
			"reason", err.Error(),
			"effectiveAllowlist", r.Allowlist.Names())
		return ctrl.Result{}, nil
	}

	r.Allowlist.SetDynamic(names)
	log.Info("applied PriorityClass allowlist from PriorityClassAllowlist",
		"priorityClassAllowlist", r.Name,
		"dynamicEntries", names,
		"effectiveAllowlist", r.Allowlist.Names())
	return ctrl.Result{}, nil
}

// parsePriorityClassAllowlist validates and normalises the PriorityClass names on
// the CR. Every entry must be a valid RFC 1123 DNS subdomain (the form Kubernetes
// requires of a PriorityClass name); any invalid entry fails the whole parse so
// the caller falls back to the static allowlist rather than partially applying
// malformed config. Blank entries are skipped, duplicates collapse, and the result
// is sorted so the logged allowlist is stable.
//
// An absent or empty list is valid and yields an empty list (the admin has
// explicitly added no dynamic classes), leaving the static base untouched.
func parsePriorityClassAllowlist(pca *v2beta1.PriorityClassAllowlist) ([]string, error) {
	seen := make(map[string]bool)
	var names []string
	for _, entry := range pca.Spec.AllowedPriorityClasses {
		name := strings.TrimSpace(entry)
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
// designated object so unrelated PriorityClassAllowlists never enqueue, and
// disabling leader election so the dynamic allowlist is maintained in every
// replica that serves the admission webhook.
func (r *PriorityClassAllowlistReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Name == "" || r.Allowlist == nil {
		return fmt.Errorf("PriorityClassAllowlistReconciler requires Name and Allowlist")
	}
	runUnconditionally := false
	return ctrl.NewControllerManagedBy(mgr).
		For(&v2beta1.PriorityClassAllowlist{}, builder.WithPredicates(r.namePredicate())).
		Named("priorityclass-allowlist").
		WithOptions(controller.Options{NeedLeaderElection: &runUnconditionally}).
		Complete(r)
}

// namePredicate matches only the single designated object, so the reconciler is
// enqueued for exactly the object whose contents it sources the dynamic allowlist
// from. The kind is cluster-scoped, so the name alone identifies it.
func (r *PriorityClassAllowlistReconciler) namePredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetName() == r.Name
	})
}

var _ reconcile.Reconciler = &PriorityClassAllowlistReconciler{}
