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
//
// These are recorded samples, and recorded samples rot: upstream owns them. The
// matcher never parses a message body — it switches on Reason and the reporting
// controller, and carries the body verbatim into the condition — so a reword can
// only change what an operator reads, not what the gate decides. That is why both
// taint spellings below must classify identically. What a reword CAN break is the
// Reason itself, and nothing in this file would notice; the live gate that does is
// `make test-autoscaler` (autoscaler_verdict_live_test.go, Q474).
const (
	// As emitted by cluster-autoscaler v1.36.1, measured 2026-07-28 against the live
	// harness. Note the bare "taint(s)": the taint's key and value are NOT in the
	// message, so the condition names the node-group ceiling but not the taint.
	caDeclineMsg = "pod didn't trigger scale-up: 1 max node group size reached, " +
		"1 node(s) had untolerated taint(s)"
	// The other spelling in the field, where the predicate's message carries the taint
	// inline. Kept as its own row because an operator reading the condition sees one
	// or the other depending on their autoscaler's release.
	caDeclineTaintNamedMsg = "pod didn't trigger scale-up: 1 max node group size reached, " +
		"2 node(s) had untolerated taint {dedicated: gpu}"
	caScaleUpMsg = "pod triggered scale-up: [{gpu-pool 1->2 (max: 8)}]"
	// As emitted by Karpenter v1.14.0, measured 2026-07-31 against the live
	// harness (karpenter_verdict_live_test.go). Only the "Failed to schedule
	// pod, " prefix is stable; the body varies with the failure shape, so both
	// measured shapes are rows and must classify identically.
	karpenterDecline = "Failed to schedule pod, incompatible requirements, key karpenter.sh/nodepool, " +
		"karpenter.sh/nodepool In [no-such-pool] not in karpenter.sh/nodepool In [standard]"
	karpenterDeclineNoInstanceMsg = "Failed to schedule pod, no instance type has enough resources, " +
		"requirements=karpenter.kwok.sh/kwoknodeclass In [default], karpenter.sh/capacity-type In [on-demand], " +
		"karpenter.sh/initialized In [true], karpenter.sh/nodepool In [standard], " +
		"karpenter.sh/registered In [true], kubernetes.io/os In [linux], " +
		"resources={\"cpu\":\"1000100m\",\"memory\":\"50Mi\",\"pods\":\"3\"}"
	karpenterNominateMsg = "Pod should schedule on: nodeclaim/standard-2hfk8"
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
			// than merely true: it names the node-group ceiling that stopped the
			// scale-up.
			wantDeclined: true, wantDetail: caDeclineMsg,
		},
		{
			// The same verdict in the spelling that carries the taint inline. The
			// matcher must not care which one it got.
			name:         "the same declination with the taint named inline",
			pod:          podWithScheduler(""),
			events:       []corev1.Event{legacyEvent(reasonNotTriggerScaleUp, "cluster-autoscaler", caDeclineTaintNamedMsg, t0)},
			wantDeclined: true, wantDetail: caDeclineTaintNamedMsg,
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
		{
			// The other measured shape of the same verdict (an oversized pod rather
			// than a pool-selector mismatch). The matcher must not care which it
			// got; the detail is capped like any condition message.
			name:         "the same declination in the no-instance-type spelling",
			pod:          podWithScheduler(""),
			events:       []corev1.Event{legacyEvent(reasonFailedScheduling, "karpenter", karpenterDeclineNoInstanceMsg, t0)},
			wantDeclined: true, wantDetail: karpenterDeclineNoInstanceMsg[:200] + "…",
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
			// A minute later, so it is a genuinely subsequent verdict rather than
			// the same loop's other half — see the window rows below.
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
			// The legacy recorder's one-second resolution collapses one loop's two
			// verdicts into a tie. Concurrent, not sequential. Fail open.
			name: "a same-instant declination and scale-up resolve open",
			pod:  podWithScheduler(""),
			events: []corev1.Event{
				legacyEvent(reasonTriggeredScaleUp, "cluster-autoscaler", caScaleUpMsg, t0),
				legacyEvent(reasonNotTriggerScaleUp, "cluster-autoscaler", caDeclineMsg, t0),
			},
			wantDeclined: false,
		},

		// --- one loop's two verdicts, at recorder resolutions that can tell
		// --- them apart (Q478) -----------------------------------------------
		{
			// The measured case, in the recorder generation that can resolve it. A
			// real cluster-autoscaler emitted exactly this pair for one pod 4ms
			// apart: round one found a scale-up plan, round two still could not
			// place the pod even with the upcoming node. A node IS arriving, so the
			// declination must not gate — and strict recency would have said it does.
			name: "a declination 4ms after a scale-up is the same loop, not a newer verdict",
			pod:  podWithScheduler(""),
			events: []corev1.Event{
				newStyleEvent(reasonTriggeredScaleUp, "cluster-autoscaler", caScaleUpMsg, t0),
				newStyleEvent(reasonNotTriggerScaleUp, "cluster-autoscaler", caDeclineMsg, t0.Add(4*time.Millisecond)),
			},
			wantDeclined: false,
		},
		{
			// The same shape in Karpenter's vocabulary, which is the arm that
			// actually runs on a microsecond recorder in the field.
			name: "karpenter declining just after nominating resolves open",
			pod:  podWithScheduler(""),
			events: []corev1.Event{
				newStyleEvent(reasonNominated, "karpenter", karpenterNominateMsg, t0),
				newStyleEvent(reasonFailedScheduling, "karpenter", karpenterDecline, t0.Add(120*time.Millisecond)),
			},
			wantDeclined: false,
		},
		{
			// The negative control for the window, and the reason it is a window
			// rather than "an acting event wins forever": a declination from a LATER
			// loop means the scale-up did not pan out, and it must still gate. Both
			// autoscalers' loop cadences are ~10s, so this is a wide margin.
			name: "a declination a full loop after a scale-up still gates",
			pod:  podWithScheduler(""),
			events: []corev1.Event{
				newStyleEvent(reasonTriggeredScaleUp, "cluster-autoscaler", caScaleUpMsg, t0),
				newStyleEvent(reasonNotTriggerScaleUp, "cluster-autoscaler", caDeclineMsg, t0.Add(10*time.Second)),
			},
			wantDeclined: true, wantDetail: caDeclineMsg,
		},
		{
			// Pins the boundary itself, with the row below, so the window cannot be
			// widened or narrowed silently.
			name: "a declination one millisecond past the window gates",
			pod:  podWithScheduler(""),
			events: []corev1.Event{
				newStyleEvent(reasonTriggeredScaleUp, "cluster-autoscaler", caScaleUpMsg, t0),
				newStyleEvent(reasonNotTriggerScaleUp, "cluster-autoscaler", caDeclineMsg,
					t0.Add(autoscalerConcurrencyWindow+time.Millisecond)),
			},
			wantDeclined: true, wantDetail: caDeclineMsg,
		},
		{
			name: "the same pair one millisecond inside the window resolves open",
			pod:  podWithScheduler(""),
			events: []corev1.Event{
				newStyleEvent(reasonTriggeredScaleUp, "cluster-autoscaler", caScaleUpMsg, t0),
				newStyleEvent(reasonNotTriggerScaleUp, "cluster-autoscaler", caDeclineMsg,
					t0.Add(autoscalerConcurrencyWindow-time.Millisecond)),
			},
			wantDeclined: false,
		},
		{
			// The window is measured from the NEWEST acting event, not the newest
			// event overall: an old scale-up must not shelter a declination that a
			// later scale-up did not answer.
			name: "an old scale-up does not shelter a declination outside the window",
			pod:  podWithScheduler(""),
			events: []corev1.Event{
				newStyleEvent(reasonTriggeredScaleUp, "cluster-autoscaler", caScaleUpMsg, t0),
				newStyleEvent(reasonNotTriggerScaleUp, "cluster-autoscaler", caDeclineMsg, t0.Add(time.Minute)),
				newStyleEvent(reasonNotTriggerScaleUp, "cluster-autoscaler", caDeclineTaintNamedMsg, t0.Add(2*time.Minute)),
			},
			wantDeclined: true, wantDetail: caDeclineTaintNamedMsg,
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
