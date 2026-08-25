package controller

import (
	"encoding/json"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/api/apisidecar"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
)

// workerReport is the wrapper's hand-back, written to the runner container's
// termination message and read here off the terminated container status (Q792). The
// shape is the wrapper's (cmd/worker); it is duplicated rather than shared because
// the worker is a separate module and this is a wire format between them, so a
// mismatch must surface as an unset field rather than as a build error that would
// couple the two release cadences.
type workerReport struct {
	RunnerVersion string `json:"runnerVersion"`
}

// observedRunner accumulates the newest runner version reported by this set's worker
// pods across one reap walk. Newest wins: a set whose workerImage changed has pods of
// both versions retained at once, and the current image is the one an operator is
// asking about.
type observedRunner struct {
	version string
	at      time.Time
}

// observe folds one terminal pod's report in, keeping the later of the two. A pod
// with no report, an unparseable one, or an empty version is ignored — absence is the
// honest answer and is what the wrapper deliberately writes when it could not detect
// a version.
func (o *observedRunner) observe(pod *corev1.Pod) {
	version, ok := workerReportVersion(pod)
	if !ok {
		return
	}
	at := provisioner.PodTerminalTime(pod)
	if o.version != "" && !at.After(o.at) {
		return
	}
	o.version, o.at = version, at
}

// workerReportVersion extracts the runner version from a terminated pod's runner
// container status.
//
// It reads the runner container by name rather than taking the first terminated
// container: a worker pod may carry tenant sidecars, and kubelet records a
// termination message for any of them that sets one.
func workerReportVersion(pod *corev1.Pod) (string, bool) {
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Name != apisidecar.RunnerContainerName {
			continue
		}
		if cs.State.Terminated == nil || cs.State.Terminated.Message == "" {
			return "", false
		}
		var report workerReport
		if err := json.Unmarshal([]byte(cs.State.Terminated.Message), &report); err != nil {
			// The termination message is tenant-writable, so anything can be in it.
			// Unparseable is the ordinary case, not an incident: no log, no event.
			return "", false
		}
		return report.RunnerVersion, report.RunnerVersion != ""
	}
	return "", false
}

// applyObservedRunnerVersion publishes the observation on status, leaving the field
// alone when this pass saw none.
//
// Deliberately sticky: a set whose terminal pods have all been reaped past their TTL
// keeps its last observation rather than reverting to empty, because the answer is a
// property of the image the set runs and not of which pods happen to be retained.
// Reverting would make the field flap on the reap cycle.
func applyObservedRunnerVersion(rs *v2alpha1.RunnerSet, observed observedRunner) {
	if observed.version == "" {
		return
	}
	rs.Status.ObservedRunnerVersion = observed.version
}
