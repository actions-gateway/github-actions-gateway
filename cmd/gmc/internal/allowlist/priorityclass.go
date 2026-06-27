// Package allowlist holds the GMC's PriorityClass admission allowlist: the set
// of cluster-scoped PriorityClass names a tenant RunnerGroup may reference in
// priorityTiers. The allowlist is the union of a static base (the
// --allowed-priority-classes flag) and a dynamic set sourced from a watched
// ConfigMap (Q188), so a platform admin can grow the allowlist without editing
// the flag and rolling out the GMC.
package allowlist

import (
	"sort"
	"sync"
)

// PriorityClassAllowlist is the effective set of cluster-scoped PriorityClass
// names a tenant RunnerGroup may reference in priorityTiers. It is the union of
// an immutable static set (from the GMC --allowed-priority-classes flag) and a
// dynamic set sourced from a watched ConfigMap (Q188).
//
// The dynamic set is strictly ADDITIVE: it can only ever widen the allowlist
// beyond the static base, never narrow or replace it. This is the fail-safe
// design — a missing, deleted, or malformed ConfigMap leaves the static flag
// allowlist in force (the ConfigMap reconciler clears the dynamic set via
// SetDynamic(nil) in those cases), so a bad ConfigMap can never silently widen
// the guardrail nor strip a class the platform pinned via the flag.
//
// All methods are safe for concurrent use. The admission webhook reads the
// effective set on every ValidateCreate/ValidateUpdate while the ConfigMap
// reconciler replaces the dynamic set on watch events.
type PriorityClassAllowlist struct {
	// static is fixed at construction from the flag and never mutated, so it is
	// read without the lock.
	static map[string]bool

	mu sync.RWMutex
	// dynamic is the ConfigMap-sourced augmentation, replaced wholesale by
	// SetDynamic. nil until the reconciler first applies a ConfigMap.
	dynamic map[string]bool
}

// New returns an allowlist whose static base is staticNames (the
// --allowed-priority-classes flag value). The dynamic set starts empty, so the
// effective allowlist equals the static base until a ConfigMap is applied. A nil
// or empty staticNames yields an allowlist that permits nothing until the
// dynamic set is populated — the secure default (an unset flag forbids every
// priorityTiers PriorityClass reference).
func New(staticNames []string) *PriorityClassAllowlist {
	return &PriorityClassAllowlist{static: toSet(staticNames)}
}

// Allowed reports whether name is on the effective allowlist (static ∪ dynamic).
func (a *PriorityClassAllowlist) Allowed(name string) bool {
	if a == nil {
		return false
	}
	if a.static[name] {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.dynamic[name]
}

// Names returns the effective allowlist as a sorted, de-duplicated slice for
// deterministic admission-rejection messages.
func (a *PriorityClassAllowlist) Names() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	set := make(map[string]bool, len(a.static)+len(a.dynamic))
	for n := range a.static {
		set[n] = true
	}
	for n := range a.dynamic {
		set[n] = true
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// SetDynamic replaces the dynamic (ConfigMap-sourced) set with names, augmenting
// the static base. Passing nil or empty clears the dynamic set, leaving only the
// static base in force — the fail-safe the ConfigMap reconciler invokes when the
// ConfigMap is absent or fails validation. Empty entries are dropped.
func (a *PriorityClassAllowlist) SetDynamic(names []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dynamic = toSet(names)
}

// DynamicNames returns the current dynamic set as a sorted slice. It exists for
// observability (logging/tests); the effective allowlist callers should consult
// is Allowed/Names.
func (a *PriorityClassAllowlist) DynamicNames() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	names := make([]string, 0, len(a.dynamic))
	for n := range a.dynamic {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// toSet builds a set from names, dropping empty entries. Returns a non-nil empty
// map for nil/empty input so callers never see a nil dynamic set after a reset.
func toSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		if n != "" {
			set[n] = true
		}
	}
	return set
}
