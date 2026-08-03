// Package allowlist holds the GMC's PriorityClass admission allowlists. The
// worker-facing allowlist is the set of cluster-scoped PriorityClass names a tenant
// may cause a WORKER pod to reference, from ANY tenant-authorable surface —
// priorityTiers on a RunnerGroup / ActionsGateway, and podTemplate.spec.priorityClassName
// on a RunnerTemplate or a v1 runnerGroups[] entry (Q289). The infra-facing allowlist
// (Q284) is a SEPARATE instance gating spec.scheduling.priorityClassName on the
// EgressProxy and v2 ActionsGateway INFRA pods (--allowed-infra-priority-classes).
//
// The same PriorityClassAllowlist type backs both, but the two instances must stay
// DISJOINT — an infra class nameable from a worker pod would let a tenant lift its
// workers to infra priority and preempt other tenants' proxies. Disjointness is
// enforced at three points: Intersection powers the GMC startup check on the two
// flags; ApplyDynamicPair refuses a watched-CR update that would create an overlap
// against either static base; and a paired allowlist denies at READ time any name
// that reached both sets anyway, so no ordering or wiring mistake can turn the
// overlap into an admitted pod.
//
// Each allowlist is the union of a static base (a flag) and a dynamic set sourced
// from a watched PriorityClassAllowlist CR (Q188 worker, Q298 infra), so a platform
// admin can grow either without editing the flag and rolling out the GMC.
package allowlist

import (
	"sort"
	"sync"
)

// PriorityClassAllowlist is the effective set of cluster-scoped PriorityClass
// names a tenant may reference from one family of surfaces. It is the union of an
// immutable static set (from a GMC flag) and a dynamic set sourced from the watched
// PriorityClassAllowlist CR (Q188).
//
// The dynamic set is strictly ADDITIVE: it can only ever widen the allowlist
// beyond the static base, never narrow or replace it. This is the fail-safe
// design — a missing, deleted, or malformed CR leaves the static flag allowlist
// in force (the reconciler clears the dynamic set via SetDynamic(nil) in those
// cases), so a bad CR can never silently widen the guardrail nor strip a class
// the platform pinned via the flag.
//
// All methods are safe for concurrent use. The admission webhook reads the
// effective set on every ValidateCreate/ValidateUpdate while the reconciler
// replaces the dynamic set on watch events.
type PriorityClassAllowlist struct {
	// static is fixed at construction from the flag and never mutated, so it is
	// read without the lock.
	static map[string]bool

	mu sync.RWMutex
	// dynamic is the CR-sourced augmentation, replaced wholesale by SetDynamic.
	// nil until the reconciler first applies a CR.
	dynamic map[string]bool

	// counterpart is the OTHER PriorityClass allowlist (worker↔infra), linked once
	// at startup by Pair and never reassigned. A name present in both allowlists is
	// allowed by NEITHER: whichever layer let the overlap through, the class stops
	// being nameable rather than becoming nameable from a worker pod.
	counterpart *PriorityClassAllowlist
}

// Pair links the worker and infra allowlists so each denies any name the other
// also allows. Call it once at startup, before the manager serves admission, on
// the two instances the webhooks read; it is the read-time half of the
// disjointness invariant that Intersection checks on the flags and
// ApplyDynamicPair checks on every CR update.
func Pair(worker, infra *PriorityClassAllowlist) {
	worker.counterpart = infra
	infra.counterpart = worker
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

// Allowed reports whether name is on the effective allowlist (static ∪ dynamic)
// and NOT on the paired counterpart's. A name on both is denied here and there:
// it is exactly the worker/infra overlap the two allowlists exist to prevent, and
// denying is the only resolution that cannot escalate.
func (a *PriorityClassAllowlist) Allowed(name string) bool {
	return a.contains(name) && !a.counterpart.contains(name)
}

// contains reports membership in this allowlist's own sets, ignoring the
// counterpart. It is the non-recursive half of Allowed; a nil allowlist contains
// nothing, which is both the secure default for an unwired webhook and what makes
// an unpaired counterpart a no-op.
func (a *PriorityClassAllowlist) contains(name string) bool {
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

// AllowedPodPriorityClass reports whether a POD-LEVEL priorityClassName is
// permitted. It differs from Allowed in exactly one way: the empty string means
// "this pod names no PriorityClass" (the Kubernetes default) and is always
// permitted, so an empty allowlist forbids every *named* class without forbidding
// unprioritized pods.
//
// Use this for any field whose zero value means unset — RunnerTemplate /
// ClusterRunnerTemplate podTemplate.spec.priorityClassName, and the v1
// runnerGroups[].podTemplate.spec.priorityClassName that feeds it (Q289). Use
// Allowed for priorityTiers[].priorityClassName, where the name is required and an
// empty value is itself a misconfiguration.
func (a *PriorityClassAllowlist) AllowedPodPriorityClass(name string) bool {
	return name == "" || a.Allowed(name)
}

// Names returns the effective allowlist as a sorted, de-duplicated slice for
// deterministic admission-rejection messages. Names the counterpart also allows
// are excluded, so the list an operator reads in a rejection is the set Allowed
// actually admits.
func (a *PriorityClassAllowlist) Names() []string {
	if a == nil {
		return nil
	}
	own := a.ownNames()
	if a.counterpart == nil {
		return own
	}
	names := make([]string, 0, len(own))
	for _, n := range own {
		if !a.counterpart.contains(n) {
			names = append(names, n)
		}
	}
	return names
}

// ownNames returns this allowlist's own effective set (static ∪ dynamic), sorted
// and ignoring the counterpart. Intersection reads it rather than Names: the
// shared names Names filters out are precisely the ones a disjointness check is
// looking for.
func (a *PriorityClassAllowlist) ownNames() []string {
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

// SetDynamic replaces the dynamic (CR-sourced) set with names, augmenting the
// static base. Passing nil or empty clears the dynamic set, leaving only the
// static base in force — the fail-safe the reconciler invokes when the CR is
// absent or fails validation. Empty entries are dropped.
//
// Use ApplyDynamicPair to update a worker/infra pair: it is what checks the
// disjointness this method does not.
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

// Intersection returns the sorted set of names present in the effective sets of
// BOTH allowlists. It powers the GMC startup disjointness check (Q284): the
// worker allowlist (--allowed-priority-classes) and the infra allowlist
// (--allowed-infra-priority-classes) must not share a class. If they did, a class
// added to the infra set so an EgressProxy/ActionsGateway pod may name it would also
// be nameable from a worker pod — and any tenant could lift its workers to infra
// priority and preempt other tenants' proxy pods, inverting the very ordering the
// infra gate exists to protect. A non-empty result at startup is a misconfiguration
// the GMC refuses to boot on. A nil allowlist contributes nothing.
func Intersection(a, b *PriorityClassAllowlist) []string {
	if a == nil || b == nil {
		return nil
	}
	other := make(map[string]bool)
	for _, n := range b.ownNames() {
		other[n] = true
	}
	var shared []string
	for _, n := range a.ownNames() {
		if other[n] {
			shared = append(shared, n)
		}
	}
	sort.Strings(shared)
	return shared
}

// ApplyDynamicPair replaces the dynamic halves of the worker and infra allowlists
// from one watched PriorityClassAllowlist CR (Q188 worker, Q298 infra), refusing
// the update wholesale if the two effective sets would overlap.
//
// The CRD's CEL rule already rejects a CR whose own two lists intersect, but it
// cannot see the GMC's flags: a CR adding a class the OTHER surface's static flag
// already pins is the overlap that reaches here. On any overlap both dynamic sets
// are cleared and the shared names returned, so the GMC falls back to the two flag
// allowlists — which the startup check proved disjoint — rather than serving a
// partially applied pair.
//
// Both sets are cleared before either is applied, so no intermediate state is
// wider than the checked final pair: after the clear the effective sets are the
// two disjoint static bases, and each subsequent set moves one side to a value
// already proven disjoint from the other's final value. On the overlap path that
// same clear IS the fail-safe.
func ApplyDynamicPair(worker, infra *PriorityClassAllowlist, workerNames, infraNames []string) []string {
	shared := candidateOverlap(worker, workerNames, infra, infraNames)
	worker.SetDynamic(nil)
	infra.SetDynamic(nil)
	if len(shared) > 0 {
		return shared
	}
	worker.SetDynamic(workerNames)
	infra.SetDynamic(infraNames)
	return nil
}

// candidateOverlap returns the sorted names that would sit on both allowlists if
// each took the given dynamic set, static bases included.
func candidateOverlap(a *PriorityClassAllowlist, aNames []string, b *PriorityClassAllowlist, bNames []string) []string {
	other := toSet(bNames)
	for n := range b.static {
		other[n] = true
	}
	candidate := toSet(aNames)
	for n := range a.static {
		candidate[n] = true
	}
	var shared []string
	for n := range candidate {
		if other[n] {
			shared = append(shared, n)
		}
	}
	sort.Strings(shared)
	return shared
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
