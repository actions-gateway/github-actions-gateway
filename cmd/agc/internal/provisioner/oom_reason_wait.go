package provisioner

import (
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// The out-of-memory class acting on a reporter's reason, where the gateway opts in. The
// operator-facing account is docs/design/04-operational-flows.md §4.2.

// OOMKillReasonsMode is what the out-of-memory class does with the reason a reporter
// writes on a killed worker's pod.
type OOMKillReasonsMode string

const (
	// OOMKillReasonsRecord repeats the reason in the failed job's record and changes no
	// action. The default, and what an unset, empty or unrecognised AGC_OOM_KILL_REASONS
	// resolves to.
	OOMKillReasonsRecord OOMKillReasonsMode = "Record"
	// OOMKillReasonsAct also waits for the runner's reason, and withholds the raise and
	// the re-run where the limit that ran out was above the runner's own.
	OOMKillReasonsAct OOMKillReasonsMode = "Act"
)

// ParseOOMKillReasonsMode maps the AGC_OOM_KILL_REASONS value onto a mode. Anything it
// does not recognise resolves to OOMKillReasonsRecord, so a GMC newer than the AGC image
// can only leave the actions as they were.
func ParseOOMKillReasonsMode(s string) OOMKillReasonsMode {
	if strings.EqualFold(strings.TrimSpace(s), string(OOMKillReasonsAct)) {
		return OOMKillReasonsAct
	}
	return OOMKillReasonsRecord
}

// oomReasonWait is how long after its pod ends a kill waits for its runner's reason: a
// guess at several times the reporter's delay, 10 s of which is its hold of an answer
// that rests on a container's end time.
const oomReasonWait = 30 * time.Second

// oomCounterReasons are the reasons a reporter reads off the cgroup counters, the only
// ones that end the wait or change an action. A reason from the kernel log alone can be a
// line a job wrote before the counters answered.
var oomCounterReasons = map[string]bool{
	"ContainerLimit": true, "PodLimit": true, "PodsLimit": true, "AbovePods": true, oomReasonUnknown: true,
}

// runnerOOMReason returns the counters' reason a reporter wrote for pod's runner, "" where
// there is none.
func runnerOOMReason(pod *corev1.Pod) string {
	for _, c := range pod.Status.Conditions {
		if container, reason, ok := oomReason(c); ok && container == runnerContainer && oomCounterReasons[reason] {
			return reason
		}
	}
	return ""
}

// oomReasonToActOn is the reason the class acts on for pod's kill, "" where it acts as it
// does with none.
func (p *Provisioner) oomReasonToActOn(pod *corev1.Pod) string {
	if p.OOMKillReasons != OOMKillReasonsAct {
		return ""
	}
	if reason := runnerOOMReason(pod); reason != oomReasonUnknown {
		return reason
	}
	return ""
}

// awaitingOOMReason returns when pod's wait for its runner's reason ends, and whether it
// is still waiting at now.
func (p *Provisioner) awaitingOOMReason(pod *corev1.Pod, now time.Time) (deadline time.Time, waiting bool) {
	if p.OOMKillReasons != OOMKillReasonsAct || !oomKilledAwaitingRecovery(pod) || runnerOOMReason(pod) != "" {
		return time.Time{}, false
	}
	if _, disrupted := disruptionAwaitingRecovery(pod); disrupted {
		return time.Time{}, false
	}
	deadline = PodTerminalTime(pod).Add(oomReasonWait)
	return deadline, now.Before(deadline)
}

// OOMReasonEndsWait reports whether pod, an unclaimed kill, has the reason its wait is
// for, the edge that starts its recovery.
func (p *Provisioner) OOMReasonEndsWait(pod *corev1.Pod) bool {
	return p.OOMKillReasons == OOMKillReasonsAct && oomKilledAwaitingRecovery(pod) && runnerOOMReason(pod) != ""
}

// aboveContainerAdvice is what a withheld re-run's Event tells the operator to do where
// reason names a limit above the runner's own, ok only for such a reason.
func aboveContainerAdvice(reason string) (advice string, ok bool) {
	switch reason {
	case "PodLimit":
		return "No limit was raised and no automatic re-run was requested, because the pod's own limit ran out, which a higher runner limit does not change. Raise the pod-level memory limit in the RunnerTemplate or reduce what the job uses; then re-run it by hand", true
	case "PodsLimit", "AbovePods":
		return "No limit was raised and no automatic re-run was requested, because memory ran out above the pod, which a higher limit does not change and which may be another pod's doing or the job's own. Look at the node's other pods and their use beside their requests; a re-run by hand may pass on another node", true
	}
	return "", false
}
