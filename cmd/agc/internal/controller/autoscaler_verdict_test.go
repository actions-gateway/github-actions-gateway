package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The real messages the two autoscaler projects emit, so the matcher is exercised
// against the strings an operator will actually see rather than against placeholders.
const (
	caDeclineMsg = "pod didn't trigger scale-up: 1 max node group size reached, " +
		"2 node(s) had untolerated taint {dedicated: gpu}"
	caScaleUpMsg     = "pod triggered scale-up: [{gpu-pool 1->2 (max: 8)}]"
	karpenterDecline = "Failed to schedule pod, incompatible with nodepool \"default\", " +
		"daemonset overhead={\"cpu\":\"210m\"}, no instance type satisfied resources"
	karpenterNominateMsg = "Pod should schedule on: nodeclaim/default-9k4qz"
	schedulerFailMsg     = "0/3 nodes are available: 3 Insufficient cpu. " +
		"preemption: 0/3 nodes are available: 3 No preemption victims found for incoming pod."
)

// legacyEvent builds an Event as the legacy recorder writes it — Source.Component set,
// First/LastTimestamp set. Both cluster-autoscaler and Karpenter record this way.
func legacyEvent(reason, component, msg string, at time.Time) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: reason + "." + component, CreationTimestamp: metav1.NewTime(at)},
		Reason:         reason,
		Message:        msg,
		Source:         corev1.EventSource{Component: component},
		FirstTimestamp: metav1.NewTime(at),
		LastTimestamp:  metav1.NewTime(at),
	}
}

// newStyleEvent builds an Event as the events.k8s.io recorder writes it —
// ReportingController and EventTime set, the legacy fields left zero.
func newStyleEvent(reason, controller, msg string, at time.Time) corev1.Event {
	return corev1.Event{
		ObjectMeta:          metav1.ObjectMeta{Name: reason + "." + controller},
		Reason:              reason,
		Message:             msg,
		ReportingController: controller,
		EventTime:           metav1.NewMicroTime(at),
	}
}

func podWithScheduler(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "ns"},
		Spec:       corev1.PodSpec{SchedulerName: name},
	}
}

// TestAutoscalerDeclination is the whole safety argument of the elastic-cluster signal
// in one table. The asymmetry it encodes: a missed declination costs nothing (the gate
// stays open, which is today's behavior), while a wrongly-read one refuses jobs the
// cluster would have run. So every ambiguous row must resolve to "not declined".
func TestAutoscalerDeclination(t *testing.T) {
	t0 := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		pod          *corev1.Pod
		events       []corev1.Event
		wantDeclined bool
		wantDetail   string
	}{
		// --- the two recognized declinations ---------------------------------
		{
			name:   "cluster-autoscaler declined to scale up",
			pod:    podWithScheduler(""),
			events: []corev1.Event{legacyEvent(reasonNotTriggerScaleUp, "cluster-autoscaler", caDeclineMsg, t0)},
			// The per-node-group text is what makes the condition actionable rather
			// than merely true: it names the taint and the node-group ceiling.
			wantDeclined: true, wantDetail: caDeclineMsg,
		},
		{
			// NotTriggerScaleUp is unique to cluster-autoscaler, so unlike
			// FailedScheduling it does not need a reporter to be trustworthy.
			name:         "cluster-autoscaler declination with no reporter recorded",
			pod:          podWithScheduler(""),
			events:       []corev1.Event{legacyEvent(reasonNotTriggerScaleUp, "", caDeclineMsg, t0)},
			wantDeclined: true, wantDetail: caDeclineMsg,
		},
		{
			name:         "karpenter could not provision for the pod",
			pod:          podWithScheduler(""),
			events:       []corev1.Event{newStyleEvent(reasonFailedScheduling, "karpenter", karpenterDecline, t0)},
			wantDeclined: true, wantDetail: karpenterDecline,
		},
		{
			name:         "karpenter recorded through the legacy source field",
			pod:          podWithScheduler(""),
			events:       []corev1.Event{legacyEvent(reasonFailedScheduling, "karpenter", karpenterDecline, t0)},
			wantDeclined: true, wantDetail: karpenterDecline,
		},

		// --- the discrimination that matters ---------------------------------
		{
			// The load-bearing negative. FailedScheduling is kube-scheduler's own
			// reason for every ordinary transient placement failure; reading those as
			// declinations would gate a set on a cluster that was about to grow for it.
			name:         "an ordinary FailedScheduling from the scheduler is not a declination",
			pod:          podWithScheduler(""),
			events:       []corev1.Event{legacyEvent(reasonFailedScheduling, defaultSchedulerName, schedulerFailMsg, t0)},
			wantDeclined: false,
		},
		{
			name:         "the same, recorded through the new-style reporting controller",
			pod:          podWithScheduler(""),
			events:       []corev1.Event{newStyleEvent(reasonFailedScheduling, defaultSchedulerName, schedulerFailMsg, t0)},
			wantDeclined: false,
		},
		{
			// A cluster running a custom scheduler discriminates against THAT name:
			// the pod itself declares who places it.
			name:         "FailedScheduling from the pod's own non-default scheduler is not a declination",
			pod:          podWithScheduler("volcano"),
			events:       []corev1.Event{legacyEvent(reasonFailedScheduling, "volcano", schedulerFailMsg, t0)},
			wantDeclined: false,
		},
		{
			// Unattributable: it may or may not be the scheduler's, and an ambiguous
			// reason may only be read as a declination when we can positively tell it
			// was not.
			name:         "FailedScheduling with no reporter at all is not a declination",
			pod:          podWithScheduler(""),
			events:       []corev1.Event{legacyEvent(reasonFailedScheduling, "", schedulerFailMsg, t0)},
			wantDeclined: false,
		},

		// --- absence of a signal ---------------------------------------------
		{
			name: "no events at all", pod: podWithScheduler(""), events: nil, wantDeclined: false,
		},
		{
			name: "only unrelated events — an unrecognized autoscaler looks like none",
			pod:  podWithScheduler(""),
			events: []corev1.Event{
				legacyEvent("Scheduled", defaultSchedulerName, "Successfully assigned ns/worker", t0),
				legacyEvent("Preempted", defaultSchedulerName, "Preempted by ns/other", t0),
				legacyEvent("Unconsolidatable", "karpenter", "not all pods would schedule", t0),
			},
			wantDeclined: false,
		},

		// --- a declination the autoscaler has since superseded ----------------
		{
			// The other way this mode could starve a tenant: cluster-autoscaler
			// declines on one loop for a node group that is momentarily full, then
			// scales up on the next. Gating on the stale declination would refuse jobs
			// while the node it asked for was being created.
			name: "a scale-up after a declination reopens the gate",
			pod:  podWithScheduler(""),
			events: []corev1.Event{
				legacyEvent(reasonNotTriggerScaleUp, "cluster-autoscaler", caDeclineMsg, t0),
				legacyEvent(reasonTriggeredScaleUp, "cluster-autoscaler", caScaleUpMsg, t0.Add(time.Minute)),
			},
			wantDeclined: false,
		},
		{
			name: "but a declination after a scale-up still gates",
			pod:  podWithScheduler(""),
			events: []corev1.Event{
				legacyEvent(reasonTriggeredScaleUp, "cluster-autoscaler", caScaleUpMsg, t0),
				legacyEvent(reasonNotTriggerScaleUp, "cluster-autoscaler", caDeclineMsg, t0.Add(time.Minute)),
			},
			wantDeclined: true, wantDetail: caDeclineMsg,
		},
		{
			name: "karpenter nominating a node claim reopens the gate",
			pod:  podWithScheduler(""),
			events: []corev1.Event{
				newStyleEvent(reasonFailedScheduling, "karpenter", karpenterDecline, t0),
				newStyleEvent(reasonNominated, "karpenter", karpenterNominateMsg, t0.Add(30*time.Second)),
			},
			wantDeclined: false,
		},
		{
			// Event timestamps have one-second resolution, so a same-instant
			// declination and scale-up are genuinely ambiguous. Fail open.
			name: "a same-instant declination and scale-up resolve open",
			pod:  podWithScheduler(""),
			events: []corev1.Event{
				legacyEvent(reasonTriggeredScaleUp, "cluster-autoscaler", caScaleUpMsg, t0),
				legacyEvent(reasonNotTriggerScaleUp, "cluster-autoscaler", caDeclineMsg, t0),
			},
			wantDeclined: false,
		},
		{
			// Ordering must not depend on which recorder generation wrote which event:
			// reading only LastTimestamp would sort every new-style event at the zero
			// time and let a stale declination win.
			name: "a new-style scale-up outranks an older legacy declination",
			pod:  podWithScheduler(""),
			events: []corev1.Event{
				legacyEvent(reasonNotTriggerScaleUp, "cluster-autoscaler", caDeclineMsg, t0),
				newStyleEvent(reasonTriggeredScaleUp, "cluster-autoscaler", caScaleUpMsg, t0.Add(time.Minute)),
			},
			wantDeclined: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declined, detail := autoscalerDeclination(tt.pod, tt.events)

			assert.Equal(t, tt.wantDeclined, declined)
			if tt.wantDeclined {
				assert.Equal(t, tt.wantDetail, detail,
					"the autoscaler's own text must reach the condition message unaltered")
			} else {
				assert.Empty(t, detail, "a gate that is not declining carries no detail")
			}
		})
	}
}

// TestAutoscalerDeclination_TruncatesALongMessage keeps a verbose autoscaler message
// from making the condition unreadable; the condition message it feeds is itself
// prefixed, and a status field is not a log.
func TestAutoscalerDeclination_TruncatesALongMessage(t *testing.T) {
	long := "pod didn't trigger scale-up: " + strings.Repeat("1 node(s) had untolerated taint; ", 40)
	pod := podWithScheduler("")

	declined, detail := autoscalerDeclination(pod,
		[]corev1.Event{legacyEvent(reasonNotTriggerScaleUp, "cluster-autoscaler", long, time.Now())})

	assert.True(t, declined)
	assert.LessOrEqual(t, len(detail), 210, "a long autoscaler message must be truncated for the condition")
	assert.Contains(t, detail, "pod didn't trigger scale-up")
}
