package allowlist

import (
	"reflect"
	"sync"
	"testing"
)

func TestNew_StaticOnly(t *testing.T) {
	a := New([]string{"runner-standard", "runner-opportunistic", ""})
	if !a.Allowed("runner-standard") {
		t.Errorf("static class runner-standard should be allowed")
	}
	if a.Allowed("system-cluster-critical") {
		t.Errorf("class not in the static set must not be allowed")
	}
	// The empty entry must be dropped, not admitted as a class named "".
	if a.Allowed("") {
		t.Errorf("empty class name must never be allowed")
	}
	if got, want := a.Names(), []string{"runner-opportunistic", "runner-standard"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
}

func TestSetDynamic_AugmentsStatic(t *testing.T) {
	a := New([]string{"runner-standard"})
	a.SetDynamic([]string{"runner-bursty", "runner-batch"})

	for _, name := range []string{"runner-standard", "runner-bursty", "runner-batch"} {
		if !a.Allowed(name) {
			t.Errorf("class %q should be allowed (static ∪ dynamic)", name)
		}
	}
	if got, want := a.Names(), []string{"runner-batch", "runner-bursty", "runner-standard"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
	if got, want := a.DynamicNames(), []string{"runner-batch", "runner-bursty"}; !reflect.DeepEqual(got, want) {
		t.Errorf("DynamicNames() = %v, want %v", got, want)
	}
}

func TestSetDynamic_ClearFallsBackToStatic(t *testing.T) {
	a := New([]string{"runner-standard"})
	a.SetDynamic([]string{"runner-bursty"})
	if !a.Allowed("runner-bursty") {
		t.Fatalf("precondition: dynamic class should be allowed")
	}

	// Fail-safe reset: clearing the dynamic set must leave the static base in
	// force and never strip a statically-pinned class.
	a.SetDynamic(nil)
	if a.Allowed("runner-bursty") {
		t.Errorf("dynamic class must be gone after SetDynamic(nil)")
	}
	if !a.Allowed("runner-standard") {
		t.Errorf("static class must survive a dynamic reset")
	}
	if len(a.DynamicNames()) != 0 {
		t.Errorf("DynamicNames() should be empty after reset, got %v", a.DynamicNames())
	}
}

func TestSetDynamic_DoesNotMutateStatic(t *testing.T) {
	a := New([]string{"runner-standard"})
	a.SetDynamic([]string{"runner-bursty"})
	a.SetDynamic(nil)
	// Re-resetting must not have removed the static entry (static and dynamic
	// are independent sets).
	if !a.Allowed("runner-standard") {
		t.Errorf("static class must be unaffected by dynamic mutations")
	}
}

func TestNilAllowlist_PermitsNothing(t *testing.T) {
	var a *PriorityClassAllowlist
	if a.Allowed("anything") {
		t.Errorf("a nil allowlist must permit nothing (secure default)")
	}
	if a.Names() != nil {
		t.Errorf("a nil allowlist must return nil Names()")
	}
}

// TestConcurrentAccess exercises the RWMutex under the race detector: many
// readers (the admission path) overlapping with writers (the ConfigMap
// reconciler) must not race.
func TestConcurrentAccess(t *testing.T) {
	a := New([]string{"runner-standard"})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = a.Allowed("runner-standard")
				_ = a.Names()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				a.SetDynamic([]string{"runner-bursty"})
				a.SetDynamic(nil)
			}
		}(i)
	}
	wg.Wait()
	// Static base must survive the churn.
	if !a.Allowed("runner-standard") {
		t.Errorf("static class lost after concurrent churn")
	}
}

func TestAllowedPodPriorityClass(t *testing.T) {
	a := New([]string{"runner-standard"})

	// The empty string means "this pod names no PriorityClass" — always permitted, so
	// the secure default forbids named classes without forbidding ordinary pods.
	if !a.AllowedPodPriorityClass("") {
		t.Errorf("unset priorityClassName must be permitted")
	}
	if !a.AllowedPodPriorityClass("runner-standard") {
		t.Errorf("allowlisted class must be permitted")
	}
	if a.AllowedPodPriorityClass("system-cluster-critical") {
		t.Errorf("off-allowlist class must be rejected (Q289)")
	}

	// The dynamic (ConfigMap, Q188) half applies to pod-level references too.
	a.SetDynamic([]string{"runner-burst"})
	if !a.AllowedPodPriorityClass("runner-burst") {
		t.Errorf("dynamic class must be permitted")
	}
}

func TestAllowedPodPriorityClass_EmptyAllowlist(t *testing.T) {
	// The secure default: an unset --allowed-priority-classes flag.
	a := New(nil)
	if !a.AllowedPodPriorityClass("") {
		t.Errorf("unset priorityClassName must stay admissible under an empty allowlist")
	}
	for _, name := range []string{"system-cluster-critical", "system-node-critical", "anything"} {
		if a.AllowedPodPriorityClass(name) {
			t.Errorf("empty allowlist must reject %q", name)
		}
	}
}

func TestNilAllowlist_AllowedPodPriorityClass(t *testing.T) {
	// A nil allowlist is the zero-value validator's state; it must deny named classes
	// without panicking, and still admit the unset case.
	var a *PriorityClassAllowlist
	if !a.AllowedPodPriorityClass("") {
		t.Errorf("nil allowlist must still permit an unset priorityClassName")
	}
	if a.AllowedPodPriorityClass("system-cluster-critical") {
		t.Errorf("nil allowlist must permit nothing named")
	}
}

func TestIntersection(t *testing.T) {
	worker := New([]string{"runner-standard", "runner-burst"})
	infra := New([]string{"gag-infra-critical"})
	// Disjoint sets: no intersection, so the GMC boots.
	if shared := Intersection(worker, infra); len(shared) != 0 {
		t.Errorf("disjoint allowlists must not intersect, got %v", shared)
	}

	// A class on both surfaces is the priority-inversion trap; Intersection must
	// surface it (sorted) so the GMC startup check can refuse to boot.
	overlap := New([]string{"gag-infra-critical", "runner-standard"})
	shared := Intersection(worker, overlap)
	want := []string{"runner-standard"}
	if !reflect.DeepEqual(shared, want) {
		t.Errorf("Intersection = %v, want %v", shared, want)
	}

	// The dynamic set participates too: a ConfigMap-added class that collides is
	// caught, not just the static flag values.
	infra.SetDynamic([]string{"runner-burst"})
	if shared := Intersection(worker, infra); !reflect.DeepEqual(shared, []string{"runner-burst"}) {
		t.Errorf("Intersection must include dynamic-set collisions, got %v", shared)
	}
}

func TestIntersection_NilOperand(t *testing.T) {
	a := New([]string{"x"})
	if shared := Intersection(a, nil); shared != nil {
		t.Errorf("nil operand must contribute no intersection, got %v", shared)
	}
	if shared := Intersection(nil, a); shared != nil {
		t.Errorf("nil operand must contribute no intersection, got %v", shared)
	}
}

func TestPair_ClassOnBothAllowlistsIsAllowedByNeither(t *testing.T) {
	// The invariant the two allowlists exist to hold: an infra class must never be
	// nameable from a worker pod. Whatever let the overlap in — a flag pair the
	// startup check never saw, a future second writer of the dynamic sets — the read
	// path denies rather than escalates.
	worker := New([]string{"runner-standard"})
	infra := New([]string{"gag-infra-critical"})
	Pair(worker, infra)

	if !worker.Allowed("runner-standard") || !infra.Allowed("gag-infra-critical") {
		t.Fatalf("precondition: disjoint sets must each allow their own class")
	}

	infra.SetDynamic([]string{"runner-standard"})
	if worker.Allowed("runner-standard") {
		t.Errorf("a class the infra allowlist also allows must not be nameable from a worker pod")
	}
	if infra.Allowed("runner-standard") {
		t.Errorf("the conflicted class must be denied on the infra surface too, not merely relocated")
	}
	if infra.Allowed("gag-infra-critical") != true {
		t.Errorf("an unconflicted infra class must stay allowed")
	}
}

func TestPair_ConflictedNameLeavesRejectionMessages(t *testing.T) {
	worker := New([]string{"runner-standard", "shared"})
	infra := New([]string{"gag-infra-critical", "shared"})
	Pair(worker, infra)

	// Names() feeds the admission-rejection message, so it must list what Allowed
	// actually admits — not a class the pairing denies.
	if got, want := worker.Names(), []string{"runner-standard"}; !reflect.DeepEqual(got, want) {
		t.Errorf("worker Names() = %v, want %v", got, want)
	}
	if got, want := infra.Names(), []string{"gag-infra-critical"}; !reflect.DeepEqual(got, want) {
		t.Errorf("infra Names() = %v, want %v", got, want)
	}
	// Intersection must still SEE the overlap: it is the disjointness check, and a
	// check reading the filtered view would report clean exactly when it is not.
	if got, want := Intersection(worker, infra), []string{"shared"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Intersection = %v, want %v", got, want)
	}
}

func TestApplyDynamicPair_AppliesDisjointSets(t *testing.T) {
	worker := New([]string{"runner-standard"})
	infra := New([]string{"gag-infra-critical"})
	Pair(worker, infra)

	if shared := ApplyDynamicPair(worker, infra, []string{"runner-bursty"}, []string{"gag-infra-high"}); shared != nil {
		t.Fatalf("disjoint sets must apply, got conflict %v", shared)
	}
	for _, name := range []string{"runner-standard", "runner-bursty"} {
		if !worker.Allowed(name) {
			t.Errorf("worker allowlist should allow %q", name)
		}
	}
	for _, name := range []string{"gag-infra-critical", "gag-infra-high"} {
		if !infra.Allowed(name) {
			t.Errorf("infra allowlist should allow %q", name)
		}
	}
}

func TestApplyDynamicPair_RejectsOverlapWholesale(t *testing.T) {
	// The case the CRD's CEL rule cannot catch: the CR's infra list names a class the
	// WORKER flag already pins. CEL sees only the object, never the flags.
	worker := New([]string{"runner-standard"})
	infra := New([]string{"gag-infra-critical"})
	Pair(worker, infra)

	if shared := ApplyDynamicPair(worker, infra, []string{"runner-bursty"}, nil); shared != nil {
		t.Fatalf("precondition: the first apply must succeed, got %v", shared)
	}

	shared := ApplyDynamicPair(worker, infra, []string{"runner-bursty"}, []string{"runner-standard"})
	if want := []string{"runner-standard"}; !reflect.DeepEqual(shared, want) {
		t.Fatalf("ApplyDynamicPair = %v, want conflict %v", shared, want)
	}
	// Both dynamic sets drop: the pair is refused wholesale, so the previously valid
	// worker addition does not survive alongside a rejected infra one.
	if worker.Allowed("runner-bursty") {
		t.Errorf("a refused pair must not leave the worker dynamic set applied")
	}
	if infra.Allowed("runner-standard") {
		t.Errorf("the conflicting infra class must never become allowed")
	}
	// The static flag allowlists — proven disjoint at startup — remain in force.
	if !worker.Allowed("runner-standard") || !infra.Allowed("gag-infra-critical") {
		t.Errorf("a refused pair must fall back to the static flag allowlists, not empty ones")
	}
}

func TestApplyDynamicPair_RejectsOverlapWithinTheCR(t *testing.T) {
	// Defence in depth for the CRD CEL rule: an object stored before the rule
	// existed, or written through a path that skipped validation.
	worker := New(nil)
	infra := New(nil)
	Pair(worker, infra)

	shared := ApplyDynamicPair(worker, infra, []string{"both"}, []string{"both"})
	if want := []string{"both"}; !reflect.DeepEqual(shared, want) {
		t.Fatalf("ApplyDynamicPair = %v, want conflict %v", shared, want)
	}
	if worker.Allowed("both") || infra.Allowed("both") {
		t.Errorf("a class on both CR lists must be allowed by neither surface")
	}
}

func TestApplyDynamicPair_ClearsWhenBothEmpty(t *testing.T) {
	worker := New([]string{"runner-standard"})
	infra := New([]string{"gag-infra-critical"})
	Pair(worker, infra)
	ApplyDynamicPair(worker, infra, []string{"runner-bursty"}, []string{"gag-infra-high"})

	if shared := ApplyDynamicPair(worker, infra, nil, nil); shared != nil {
		t.Fatalf("clearing both sets cannot conflict, got %v", shared)
	}
	if worker.Allowed("runner-bursty") || infra.Allowed("gag-infra-high") {
		t.Errorf("an emptied CR must drop both dynamic sets")
	}
	if !worker.Allowed("runner-standard") || !infra.Allowed("gag-infra-critical") {
		t.Errorf("clearing the dynamic sets must not strip the static flag classes")
	}
}
