package controller

import (
	"context"
	"errors"
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
// PriorityClassAllowlist CR and reconciles its two lists into the dynamic halves
// of the worker and infra PriorityClass admission allowlists (Q188 worker, Q298
// infra). This lets a platform admin add an allowed PriorityClass name to either
// surface without editing the GMC --allowed-priority-classes /
// --allowed-infra-priority-classes flags and rolling out the controller — the
// change takes effect on the next watch event, restart-free.
//
// The two allowlists must stay disjoint (Q284), and a CR edit is a route to an
// overlap the startup flag check cannot see. Both lists are therefore applied
// through allowlist.ApplyDynamicPair, which refuses the pair wholesale when the
// resulting effective sets would share a class.
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
// bad object must never widen it): on a missing/deleted CR, any invalid entry, or
// a worker/infra overlap, the reconciler clears BOTH dynamic sets so only the
// static flag allowlists remain in force, and logs the reason. A malformed list is
// rejected WHOLESALE — the valid subset is not partially applied — so a typo can
// never silently smuggle in a class alongside garbage, and neither list is applied
// when the other is bad. The CRD schema already constrains each entry to a DNS 1123
// subdomain and rejects an overlap between the object's own two lists, so those
// paths are now defence in depth rather than the primary gate; they still run,
// because an object written before a schema change (or through a path that bypassed
// validation) must not be trusted.
type PriorityClassAllowlistReconciler struct {
	client.Client
	// Name is the name of the watched cluster-scoped PriorityClassAllowlist.
	// Required.
	Name string
	// Allowlist is the shared WORKER allowlist the admission webhooks read. The
	// reconciler owns its dynamic half. Required.
	Allowlist *allowlist.PriorityClassAllowlist
	// InfraAllowlist is the shared INFRA allowlist gating
	// spec.scheduling.priorityClassName on EgressProxy and v2 ActionsGateway pods
	// (Q298). The reconciler owns its dynamic half and keeps it disjoint from
	// Allowlist. Required.
	InfraAllowlist *allowlist.PriorityClassAllowlist
}

// Reconcile reads the designated PriorityClassAllowlist and updates both dynamic
// allowlists. See the type doc for the fail-safe contract.
func (r *PriorityClassAllowlistReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var pca v2beta1.PriorityClassAllowlist
	key := types.NamespacedName{Name: r.Name}
	if err := r.Get(ctx, key, &pca); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// Absent/deleted object: fall back to the static flag allowlists.
			r.failSafe()
			log.Info("PriorityClassAllowlist not present; using the static flag allowlists only",
				"priorityClassAllowlist", r.Name,
				"effectiveAllowlist", r.Allowlist.Names(),
				"effectiveInfraAllowlist", r.InfraAllowlist.Names())
			return ctrl.Result{}, nil
		}
		// A transient read error (not NotFound): requeue rather than mutate the
		// allowlists on incomplete information. The previously applied dynamic sets
		// stay in force until we can read authoritative state.
		return ctrl.Result{}, fmt.Errorf("get PriorityClassAllowlist %s: %w", r.Name, err)
	}

	names, workerErr := parsePriorityClassNames(pca.Spec.AllowedPriorityClasses)
	infraNames, infraErr := parsePriorityClassNames(pca.Spec.AllowedInfraPriorityClasses)
	if err := errors.Join(workerErr, infraErr); err != nil {
		// Malformed contents: fail safe to the static flag allowlists and log a
		// warning. Do NOT requeue — the data is invalid, not transiently unreadable;
		// the next watch event (an admin fixing it) drives the retry. A bad entry on
		// either list withholds both, so a valid infra addition never rides in on an
		// object whose worker list is garbage.
		r.failSafe()
		log.Info("WARNING: PriorityClassAllowlist is invalid; ignoring it and using the static flag allowlists only",
			"priorityClassAllowlist", r.Name,
			"reason", err.Error(),
			"effectiveAllowlist", r.Allowlist.Names(),
			"effectiveInfraAllowlist", r.InfraAllowlist.Names())
		return ctrl.Result{}, nil
	}

	if shared := allowlist.ApplyDynamicPair(r.Allowlist, r.InfraAllowlist, names, infraNames); len(shared) > 0 {
		// The object's own two lists are kept disjoint by the CRD's CEL rule, so this
		// is an entry colliding with the OTHER surface's static flag, which CEL cannot
		// see. ApplyDynamicPair has already dropped both dynamic sets; refuse rather
		// than let a tenant reach infra priority from a worker pod.
		log.Info("WARNING: PriorityClassAllowlist would make the worker and infra allowlists intersect; "+
			"ignoring it and using the static flag allowlists only",
			"priorityClassAllowlist", r.Name,
			"sharedClasses", shared,
			"effectiveAllowlist", r.Allowlist.Names(),
			"effectiveInfraAllowlist", r.InfraAllowlist.Names())
		return ctrl.Result{}, nil
	}

	log.Info("applied PriorityClass allowlists from PriorityClassAllowlist",
		"priorityClassAllowlist", r.Name,
		"dynamicEntries", names,
		"dynamicInfraEntries", infraNames,
		"effectiveAllowlist", r.Allowlist.Names(),
		"effectiveInfraAllowlist", r.InfraAllowlist.Names())
	return ctrl.Result{}, nil
}

// failSafe drops both dynamic sets, leaving the two static flag allowlists — which
// the GMC startup check proved disjoint — as the effective guardrail.
func (r *PriorityClassAllowlistReconciler) failSafe() {
	r.Allowlist.SetDynamic(nil)
	r.InfraAllowlist.SetDynamic(nil)
}

// parsePriorityClassNames validates and normalises one of the CR's PriorityClass
// lists. Every entry must be a valid RFC 1123 DNS subdomain (the form Kubernetes
// requires of a PriorityClass name); any invalid entry fails the whole parse so
// the caller falls back to the static allowlists rather than partially applying
// malformed config. Blank entries are skipped, duplicates collapse, and the result
// is sorted so the logged allowlist is stable.
//
// An absent or empty list is valid and yields an empty list (the admin has
// explicitly added no dynamic classes), leaving the static base untouched.
func parsePriorityClassNames(entries []string) ([]string, error) {
	seen := make(map[string]bool)
	var names []string
	for _, entry := range entries {
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
	if r.Name == "" || r.Allowlist == nil || r.InfraAllowlist == nil {
		return fmt.Errorf("PriorityClassAllowlistReconciler requires Name, Allowlist, and InfraAllowlist")
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
