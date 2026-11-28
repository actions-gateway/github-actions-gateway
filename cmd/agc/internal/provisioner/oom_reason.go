package provisioner

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// The out-of-memory reporter's answer in a failed job's record. It writes a pod
// condition per killed container, whose reason names the limit that ran out
// (cmd/oomreporter/internal/controller/words.go). Whoever can write pods/status can write
// one too, so the record repeats it as words of its own beside a kill the pod's status
// shows, and no action rests on it. The limits on that are in
// docs/design/04-operational-flows.md §4.2.

// oomConditionPrefix starts the type of a reporter's condition; the container's name ends it.
const oomConditionPrefix = "oom.actions-gateway.com/"

// oomReasonUnknown is the reason that claims nothing, and any other replaces it.
const oomReasonUnknown = "Unknown"

// oomReasonWords is what a reporter's reason is repeated as. A reason that is not here is
// no answer.
var oomReasonWords = map[string]string{
	"ContainerLimit": "the container's own memory limit ran out, or those of the pod's containers together",
	"PodLimit":       "the memory limit in the pod's spec.resources ran out",
	"PodsLimit":      "the memory limit that the node's pods share ran out, or that of those of one QoS class",
	"AbovePods":      "memory ran out above the pods, which on an ordinary node is the node's",
	oomReasonUnknown: "which limit ran out is not known",
}

// maxEventNote is the longest note, in characters, the API server takes in an Event.
const maxEventNote = 1024

// oomReason returns the container a reporter's condition is about and its reason, ok only
// for a reason the record has words for.
func oomReason(c corev1.PodCondition) (container, reason string, ok bool) {
	container, ok = strings.CutPrefix(string(c.Type), oomConditionPrefix)
	if !ok {
		return "", "", false
	}
	want := corev1.ConditionTrue
	if c.Reason == oomReasonUnknown {
		want = corev1.ConditionUnknown
	}
	_, known := oomReasonWords[c.Reason]
	return container, c.Reason, known && c.Status == want
}

// OOMReasonAppeared reports whether after, a Failed worker, has a reporter's reason that
// before has not.
func OOMReasonAppeared(before, after *corev1.Pod) bool {
	if after.Status.Phase != corev1.PodFailed {
		return false
	}
	had := map[string]string{}
	for _, c := range before.Status.Conditions {
		if container, reason, ok := oomReason(c); ok {
			had[container] = reason
		}
	}
	for _, c := range after.Status.Conditions {
		if container, reason, ok := oomReason(c); ok && had[container] != reason {
			return true
		}
	}
	return false
}

// AddOOMReasons adds what a reporter's conditions on pod say to the snapshot that waits
// for its job's completion. The snapshot itself stays the one taken as the worker failed.
func (p *Provisioner) AddOOMReasons(pod *corev1.Pod) {
	if key, ok := snapshotKey(pod); ok {
		p.workerSnapshots.addOOMReasons(key, snapshotOf(pod, p.nowFn()))
	}
}

// addOOMReasons adds the reasons s read to the snapshot of its pod that is still held.
// One that has been taken may be being encoded.
func (w *workerSnapshots) addOOMReasons(key string, s *workerSnapshot) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if held := w.byID[key]; held != nil && held.Pod == s.Pod {
		for i := range held.Containers {
			c := &held.Containers[i]
			c.OOMReason = firstOOMReason(c.OOMReason, s.oomReasonOf(*c))
		}
	}
}

// carryOOMReasons keeps the reasons earlier read of s's containers, which came first.
func (s *workerSnapshot) carryOOMReasons(earlier *workerSnapshot) {
	for i := range s.Containers {
		c := &s.Containers[i]
		c.OOMReason = firstOOMReason(earlier.oomReasonOf(*c), c.OOMReason)
	}
}

// oomReasonOf returns s's reason for c, by its name and its ID, "" where c restarted, since
// a reporter's condition outlives the container it is about.
func (s *workerSnapshot) oomReasonOf(c containerSnapshot) string {
	for _, o := range s.Containers {
		if o.Name == c.Name && o.id == c.id && c.RestartCount == 0 {
			return o.OOMReason
		}
	}
	return ""
}

// firstOOMReason is the one of two reasons for one container that stands.
func firstOOMReason(first, then string) string {
	if first == "" || first == oomReasonUnknown && then != "" {
		return then
	}
	return first
}

// noteOOMReason keeps reason for the container, where its status shows it killed for
// memory and it never restarted.
func (s *workerSnapshot) noteOOMReason(container, reason string) {
	for i := range s.Containers {
		c := &s.Containers[i]
		if c.Name == container && c.RestartCount == 0 && c.oomKilled(c.Reason, c.ExitCode) {
			c.OOMReason = reason
		}
	}
}

// withOOMReasons returns faults with each kill's reason beside it. Faults are compared as
// text, so the words are added only where they are said.
func (s *workerSnapshot) withOOMReasons(faults []string) []string {
	said := make([]string, len(faults))
	for i, fault := range faults {
		said[i] = fault
		for _, c := range s.Containers {
			if words := oomReasonWords[c.OOMReason]; words != "" && fault == c.oomFault() {
				said[i] = fmt.Sprintf("%s (a pod condition says %s)", fault, words)
			}
		}
	}
	return said
}
