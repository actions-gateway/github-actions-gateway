package runnercore

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The callbacks that connect an acquisition tier to the rest of the AGC. None of
// them names a wire protocol or an API version: the classic listener pool and the
// scale-set listener both report through the same sinks, and the provisioner —
// which owns the capacity gate — serves both.

// ConditionUpdater submits RunnerGroup condition updates to the reconciler.
// Implementations must be non-blocking.
type ConditionUpdater interface {
	SetCondition(namespace, name string, cond metav1.Condition)
}

// EventRecorder records a Kubernetes Event about the owning RunnerGroup/RunnerSet
// (identified by namespace/name). The reconciler drains these and records them on
// the live owner object, so job-lifecycle incidents surface in `kubectl describe`
// and event watchers — complementing the metrics/conditions that already track the
// same state. Like ConditionUpdater, implementations must be non-blocking (drop on
// a full channel) so a listener or provisioner goroutine never blocks on event
// delivery. action and note follow the client-go events API (the "what happened"
// verb and the human-readable message).
type EventRecorder interface {
	Event(namespace, name, eventtype, reason, action, note string)
}

// AdmitFunc gates job acquisition on available worker capacity (Q59). It is
// called after a job is delivered but before the acquisition tier claims it from
// GitHub. ok=false means there is no capacity: the caller skips the acquire,
// leaving the job queued at GitHub for redelivery to a sibling session — rather
// than claiming a job whose worker pod it cannot place, which would be cancelled
// when the unrenewed lock lapses. ok=true returns release, which the caller
// invokes exactly once when the reserved slot is freed (acquire failure or job
// completion) so the gate's in-flight count tracks only live jobs.
//
// reason is set only when ok=false and names which rung refused (an AdmitReason*
// constant); it becomes the `reason` label on
// actions_gateway_jobs_admission_rejected_total so an operator can tell "at the
// configured ceiling" from "out of namespace quota".
//
// The gate itself is the provisioner's in-memory per-owner reservation counter
// (provisioner.Provisioner.Admit), which is why this type is protocol-neutral
// even though the classic listener is its only caller today.
type AdmitFunc func(ctx context.Context) (release func(), ok bool, reason string)

// AdmitReason* are the AdmitFunc rejection reasons, used verbatim as the `reason`
// label of actions_gateway_jobs_admission_rejected_total. All mean the job was
// deliberately left queued at GitHub, but they call for different operator action:
// ceiling → raise maxWorkers/priorityTiers (or accept the throttling); quota →
// raise the namespace ResourceQuota (see the owner's WorkerQuotaExceeded condition
// for the binding resource); capacity → the cluster cannot place this owner's
// worker shape at all (see its WorkerCapacityDeclined condition for which signal
// said so), so restore the capacity or relax the shape; scaleup → the owner's own
// spec.scaleUp token bucket is empty, so raise maxPerSecond/burst or accept the
// ramp.
//
// scaleup is the one that clears on its own: the bucket refills at maxPerSecond, so
// a steady stream of these is the rate limit working rather than a stall. The other
// three clear only when something changes.
const (
	AdmitReasonCeiling  = "ceiling"
	AdmitReasonQuota    = "quota"
	AdmitReasonCapacity = "capacity"
	AdmitReasonScaleUp  = "scaleup"
)
