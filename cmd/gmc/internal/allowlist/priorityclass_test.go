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
