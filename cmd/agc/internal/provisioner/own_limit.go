package provisioner

import (
	"fmt"
	"log/slog"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Own-limit evictions (Q1177): the PodFailed/Evicted shape the eviction arm keys on is
// written by the kubelet for two different causes, and only one of them is a
// disruption. Node pressure evicts whichever pod the kubelet ranks worst, so a fresh
// pod on a fresh node clears it. A pod that outgrows its OWN local-storage limit — the
// pod's total ephemeral-storage limit, one container's limit, or an emptyDir sizeLimit —
// is evicted by the same code path for a cause the tenant's job and template own, and a
// re-run gets the same limit and fails the same way. That is the storage analogue of
// OOMKilled, so it is withheld as own_limit before a budget slot is spent.
//
// Two independent kubelet signals separate them, measured on kind v1.35.5
// (docs/design/04-operational-flows.md §4.2 has the capture):
//
//   - Node pressure stamps a DisruptionTarget=True/TerminationByKubelet condition; the
//     three limit paths stamp none.
//   - The limit paths write one of three fixed message formats; node pressure writes
//     "The node was low on resource: …".
//
// Both must agree before a re-run is withheld. An Evicted pod that matches neither — a
// kubelet whose messages changed, or one too old to stamp the condition — keeps the
// eviction arm's re-run, so an unrecognised shape costs at most the futile re-runs it
// cost before this check existed, never a disruption's recovery.

// ownLimitMessageFragments are the fixed parts of the kubelet's three limit-eviction
// messages (pkg/kubelet/eviction/helpers.go at v1.35.5: podEphemeralStorageMessageFmt,
// containerEphemeralStorageMessageFmt, emptyDirMessageFmt).
var ownLimitMessageFragments = []string{
	"Pod ephemeral local storage usage exceeds the total limit of containers ",
	" exceeded its local ephemeral storage limit ",
	" exceeds the limit ",
}

// ownLimitEviction returns the kubelet's eviction message when pod was evicted for
// exceeding its own local-storage limit, and "" for any other pod — including a
// node-pressure eviction, which the eviction arm still recovers.
func ownLimitEviction(pod *corev1.Pod) string {
	if pod.Status.Phase != corev1.PodFailed || pod.Status.Reason != podReasonEvicted {
		return ""
	}
	if terminatedByKubeletPressure(pod) {
		return ""
	}
	msg := pod.Status.Message
	for _, f := range ownLimitMessageFragments {
		if strings.Contains(msg, f) {
			return msg
		}
	}
	return ""
}

// terminatedByKubeletPressure reports the condition the kubelet's node-pressure path
// stamps and its limit paths do not.
func terminatedByKubeletPressure(pod *corev1.Pod) bool {
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.Type == corev1.DisruptionTarget &&
			c.Status == corev1.ConditionTrue &&
			c.Reason == corev1.PodReasonTerminationByKubelet {
			return true
		}
	}
	return false
}

// rerunWithheldReasonOwnLimit is the reason label for an eviction the job caused by
// outgrowing its own limit.
const rerunWithheldReasonOwnLimit = "own_limit"

// withholdOwnLimitRerun surfaces an own-limit eviction in place of its re-run. No
// budget slot is reserved, so the run keeps both for a disruption that a re-run can
// actually fix.
func (p *Provisioner) withholdOwnLimitRerun(target Target, runID, tier string, log *slog.Logger, kubeletMessage string) {
	log.Info("worker pod evicted for exceeding its own limit; no automatic re-run",
		"runID", runID, "tier", tier, "kubeletMessage", kubeletMessage)
	p.recordRerunWithheld(target, runID, tier, recoveryCauseEviction, rerunWithheldReasonOwnLimit,
		fmt.Sprintf("worker pod for run %s was evicted for exceeding its own limit (%s), so no automatic re-run was requested: a re-run gets the same limit. Raise the runner's ephemeral-storage limit or emptyDir sizeLimit, or reduce what the job writes, then re-run it by hand", runID, strings.TrimSpace(kubeletMessage)))
}
