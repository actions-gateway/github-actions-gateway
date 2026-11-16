package allowlist

import (
	"fmt"
	"sort"
	"strings"
)

// ClassValues is the cluster's PriorityClass values by name, and the name of the
// class marked globalDefault ("" when none is). A pod naming no class runs at the
// globalDefault class's value, so that class is reachable from every worker.
type ClassValues struct {
	Values        map[string]int32
	GlobalDefault string
}

// Inversion is one worker-reachable PriorityClass whose value is not below a
// control-plane class.
type Inversion struct {
	Worker            string
	WorkerValue       int32
	ControlPlane      string
	ControlPlaneValue int32
}

func (i Inversion) String() string {
	return fmt.Sprintf("worker class %q (%d) is not below control-plane class %q (%d)",
		i.Worker, i.WorkerValue, i.ControlPlane, i.ControlPlaneValue)
}

// Misordered returns every worker-reachable class whose value is at or above the
// lowest control-plane class, each paired with that lowest class, sorted by worker
// name (Q1351). A worker that outranks a controller can have the scheduler evict it,
// in any namespace.
//
// Worker-reachable is workerNames plus the globalDefault class; control plane is
// controlPlaneNames. A name with no PriorityClass object is skipped on both sides:
// the apiserver refuses a pod naming an absent class, so it cannot run at any value.
func Misordered(cv ClassValues, workerNames, controlPlaneNames []string) []Inversion {
	floorName, floor, ok := lowest(cv.Values, controlPlaneNames)
	if !ok {
		return nil
	}
	reachable := toSet(workerNames)
	if cv.GlobalDefault != "" {
		reachable[cv.GlobalDefault] = true
	}
	var out []Inversion
	for name := range reachable {
		v, exists := cv.Values[name]
		if exists && v >= floor {
			out = append(out, Inversion{Worker: name, WorkerValue: v, ControlPlane: floorName, ControlPlaneValue: floor})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Worker < out[j].Worker })
	return out
}

// CheckOrder runs Misordered over the worker and infra allowlists' own effective
// sets. It reads past the pairing, because a name on both sides is still a class a
// misconfigured pair would have offered.
func CheckOrder(cv ClassValues, worker, infra *PriorityClassAllowlist) []Inversion {
	return Misordered(cv, worker.ownNames(), infra.ownNames())
}

// MisorderedError joins inversions into one error, or returns nil for none.
func MisorderedError(inv []Inversion) error {
	if len(inv) == 0 {
		return nil
	}
	parts := make([]string, len(inv))
	for i, x := range inv {
		parts[i] = x.String()
	}
	return fmt.Errorf("PriorityClass order: %s; every class a worker can reach must sit below every "+
		"control-plane class, or a worker can preempt an AGC or egress proxy", strings.Join(parts, "; "))
}

// lowest returns the existing class in names with the smallest value, breaking a
// tie by name.
func lowest(values map[string]int32, names []string) (string, int32, bool) {
	var (
		best  string
		bestV int32
		found bool
	)
	for _, n := range names {
		v, ok := values[n]
		if !ok {
			continue
		}
		if !found || v < bestV || (v == bestV && n < best) {
			best, bestV, found = n, v, true
		}
	}
	return best, bestV, found
}
