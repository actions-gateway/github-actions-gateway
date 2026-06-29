package controller

import (
	"context"
	"fmt"
	"net"
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

// EgressDestinationFQDNsConfigMapKey / EgressDestinationCIDRsConfigMapKey are the
// two ConfigMap data keys the EgressDestinationAllowlistReconciler reads (Q242 G.1).
// Their values are lists — newline-, comma-, or whitespace-separated — of FQDN host
// suffixes and CIDR ranges that AUGMENT the static --allowed-egress-fqdns /
// --allowed-egress-cidrs flag allowlists. Both keys are optional; a present-but-empty
// value contributes nothing, and a ConfigMap with neither key clears the dynamic set.
const (
	EgressDestinationFQDNsConfigMapKey = "fqdns"
	EgressDestinationCIDRsConfigMapKey = "cidrs"
)

// EgressDestinationAllowlistReconciler watches a single designated ConfigMap and
// reconciles its fqdns/cidrs lists into the dynamic half of the platform egress
// destination allowlist (Q242 G.1). This lets a platform admin grow the set of
// non-GitHub destinations a tenant EgressProxy may request without editing the GMC
// --allowed-egress-fqdns / --allowed-egress-cidrs flags and rolling out the
// controller — the change takes effect on the next watch event, restart-free.
//
// Like the PriorityClass allowlist reconciler it mirrors (Q188), it runs in EVERY
// GMC replica (NeedLeaderElection=false): the validating admission webhook is served
// by every ready replica, so each must hold the current effective allowlist, and the
// dynamic set is per-process in-memory state with nothing to contend over.
//
// Fail-safe contract (the allowlist is a cross-tenant-isolation guardrail, so a bad
// ConfigMap must never widen it): on a missing/deleted ConfigMap or any invalid
// entry the reconciler clears the dynamic set (Allowlist.SetDynamic(nil, nil)) so
// only the static flag allowlist remains in force, and logs the reason. A malformed
// ConfigMap is rejected WHOLESALE — the valid subset is not partially applied — so a
// typo can never silently smuggle in a destination alongside garbage.
type EgressDestinationAllowlistReconciler struct {
	client.Client
	// ConfigMapName is the name of the watched ConfigMap. Required.
	ConfigMapName string
	// Namespace is the namespace the ConfigMap lives in — the GMC's own install
	// namespace, so only a platform admin (not a tenant) can write it. Required.
	Namespace string
	// Allowlist is the shared allowlist the admission webhook reads. The reconciler
	// owns its dynamic half. Required.
	Allowlist *allowlist.EgressDestinationAllowlist
}

// Reconcile reads the designated ConfigMap and updates the dynamic allowlist.
// See the type doc for the fail-safe contract.
func (r *EgressDestinationAllowlistReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: r.Namespace, Name: r.ConfigMapName}
	if err := r.Get(ctx, key, &cm); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// Absent/deleted ConfigMap: fall back to the static flag allowlists.
			r.Allowlist.SetDynamic(nil, nil)
			log.Info("egress destination allowlist ConfigMap not present; using the static --allowed-egress-fqdns/--allowed-egress-cidrs flags only",
				"configMap", key.String(),
				"effectiveFQDNs", r.Allowlist.FQDNSuffixes(),
				"effectiveCIDRs", r.Allowlist.CIDRStrings())
			return ctrl.Result{}, nil
		}
		// A transient read error (not NotFound): requeue rather than mutate the
		// allowlist on incomplete information. The previously applied dynamic set
		// stays in force until we can read authoritative state.
		return ctrl.Result{}, fmt.Errorf("get egress destination allowlist ConfigMap %s: %w", key, err)
	}

	fqdns, cidrs, err := parseEgressDestinationAllowlistConfigMap(&cm)
	if err != nil {
		// Malformed contents: fail safe to the static flag allowlists and log a
		// warning. Do NOT requeue — the data is invalid, not transiently unreadable;
		// the next watch event (an admin fixing it) drives the retry.
		r.Allowlist.SetDynamic(nil, nil)
		log.Info("WARNING: egress destination allowlist ConfigMap is invalid; ignoring it and using the static --allowed-egress-fqdns/--allowed-egress-cidrs flags only",
			"configMap", key.String(),
			"reason", err.Error(),
			"effectiveFQDNs", r.Allowlist.FQDNSuffixes(),
			"effectiveCIDRs", r.Allowlist.CIDRStrings())
		return ctrl.Result{}, nil
	}

	dynamicCIDRStrings := make([]string, 0, len(cidrs))
	for _, c := range cidrs {
		dynamicCIDRStrings = append(dynamicCIDRStrings, c.String())
	}
	r.Allowlist.SetDynamic(fqdns, cidrs)
	log.Info("applied egress destination allowlist from ConfigMap",
		"configMap", key.String(),
		"dynamicFQDNs", fqdns,
		"dynamicCIDRs", dynamicCIDRStrings,
		"effectiveFQDNs", r.Allowlist.FQDNSuffixes(),
		"effectiveCIDRs", r.Allowlist.CIDRStrings())
	return ctrl.Result{}, nil
}

// parseEgressDestinationAllowlistConfigMap extracts and validates the FQDN suffixes
// and CIDRs from the ConfigMap. Each value is split on commas, newlines, and
// surrounding whitespace. Every FQDN entry must be a valid host suffix (a leading
// "*." wildcard is allowed) and every CIDR entry must parse; any invalid entry fails
// the whole parse so the caller falls back to the static allowlists rather than
// partially applying malformed config.
//
// Both keys are optional. A ConfigMap with neither key is valid and yields empty
// lists (the admin has added no dynamic destinations), leaving the static base
// untouched — distinct from the PriorityClass reconciler, which requires its single
// key, because here either dimension may legitimately be unused.
func parseEgressDestinationAllowlistConfigMap(cm *corev1.ConfigMap) ([]string, []*net.IPNet, error) {
	fqdns, err := parseFQDNSuffixList(cm.Data[EgressDestinationFQDNsConfigMapKey])
	if err != nil {
		return nil, nil, err
	}
	cidrs, err := parseCIDRList(cm.Data[EgressDestinationCIDRsConfigMapKey])
	if err != nil {
		return nil, nil, err
	}
	return fqdns, cidrs, nil
}

// parseFQDNSuffixList splits and validates a host-suffix list. Each entry, after an
// optional leading "*." is stripped, must be a valid RFC 1123 DNS subdomain.
func parseFQDNSuffixList(raw string) ([]string, error) {
	seen := make(map[string]bool)
	var out []string
	for _, field := range splitAllowlistFields(raw) {
		entry := strings.TrimSpace(field)
		if entry == "" {
			continue
		}
		bare := strings.TrimPrefix(strings.ToLower(entry), "*.")
		bare = strings.Trim(bare, ".")
		if errs := validation.IsDNS1123Subdomain(bare); len(errs) > 0 {
			return nil, fmt.Errorf("invalid FQDN suffix %q: %s", entry, strings.Join(errs, "; "))
		}
		if seen[bare] {
			continue
		}
		seen[bare] = true
		out = append(out, bare)
	}
	sort.Strings(out)
	return out, nil
}

// parseCIDRList splits and validates a CIDR list. Each entry must parse via
// net.ParseCIDR; the canonical masked network is kept.
func parseCIDRList(raw string) ([]*net.IPNet, error) {
	seen := make(map[string]bool)
	var out []*net.IPNet
	for _, field := range splitAllowlistFields(raw) {
		entry := strings.TrimSpace(field)
		if entry == "" {
			continue
		}
		_, n, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", entry, err)
		}
		if seen[n.String()] {
			continue
		}
		seen[n.String()] = true
		out = append(out, n)
	}
	return out, nil
}

// splitAllowlistFields splits a ConfigMap allowlist value on commas, newlines,
// carriage returns, tabs, and spaces.
func splitAllowlistFields(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
}

// SetupWithManager registers the reconciler, scoping its watch to the single
// designated ConfigMap so unrelated ConfigMaps never enqueue, and disabling leader
// election so the dynamic allowlist is maintained in every replica that serves the
// admission webhook.
func (r *EgressDestinationAllowlistReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.ConfigMapName == "" || r.Namespace == "" || r.Allowlist == nil {
		return fmt.Errorf("EgressDestinationAllowlistReconciler requires ConfigMapName, Namespace, and Allowlist")
	}
	runUnconditionally := false
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}, builder.WithPredicates(r.configMapPredicate())).
		Named("egress-destination-allowlist").
		WithOptions(controller.Options{NeedLeaderElection: &runUnconditionally}).
		Complete(r)
}

// configMapPredicate matches only the single designated ConfigMap (by namespace and
// name), so the reconciler is enqueued for exactly the object whose contents it
// sources the dynamic allowlist from.
func (r *EgressDestinationAllowlistReconciler) configMapPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == r.Namespace && obj.GetName() == r.ConfigMapName
	})
}

var _ reconcile.Reconciler = &EgressDestinationAllowlistReconciler{}
