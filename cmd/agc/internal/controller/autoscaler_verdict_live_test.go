//go:build autoscaler

// Package controller's live cluster-autoscaler test (Q474).
//
// TestAutoscalerDeclination's table pins the matcher against RECORDED
// cluster-autoscaler messages. Recorded samples rot: upstream owns those strings,
// and a reword there does not break anything loudly on our side — an unrecognized
// vocabulary yields declined=false, which is exactly today's ungated behavior. The
// capacity gate would simply stop closing, on every elastic cluster, and every
// existing test would stay green.
//
// This file closes that by running the same matcher against events a REAL upstream
// cluster-autoscaler emits, in a kind cluster whose nodes are fakes made by kwok
// (scripts/autoscaler-cluster.sh stands it up; `make test-autoscaler` runs this).
// The autoscaler, its scheduler-framework evaluation, and its events are genuine;
// only the machines are not, which is the whole reason the harness fits on a laptop.
//
// It is deliberately NOT part of `make check` or per-PR CI: it costs a cluster, and
// what it detects is upstream drift, which arrives on upstream's schedule rather
// than on a pull request's. Run it when bumping CA_VERSION, and on the cadence
// docs/development/testing.md sets. The Karpenter arm of the same gate is
// karpenter_verdict_live_test.go (Q479), against its own cluster.
package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The node groups scripts/autoscaler-cluster.sh installs, via the templates in
// test/autoscaler/kwok-provider.yaml. Kept as constants here so a rename over there
// fails compilation-adjacent assertions rather than producing a mysterious timeout.
const (
	poolLabel = "gag-test/pool"
	// poolStandard has room to grow: a pod selecting it is rescued by a scale-up.
	poolStandard = "standard"
	// poolCapped tops out at one node, so a pod that does not fit the one node it
	// can create is declined for the node-group ceiling.
	poolCapped = "capped"
	// poolAbsent matches no node group at all.
	poolAbsent = "no-such-pool"
)

// A pod that consumes most of a 4-CPU template node, so one pod fills one node.
const bigPodCPU = "3500m"

// caClient connects to the cluster-autoscaler harness cluster.
func caClient(t *testing.T) client.Client {
	t.Helper()
	return liveClient(t, "AUTOSCALER_KUBE_CONTEXT", "AUTOSCALER_CLUSTER", "gag-autoscaler", "make autoscaler-cluster")
}

// awaitAutoscalerEvent blocks until cluster-autoscaler has recorded an event
// with reason against pod, and returns every event on the pod at that moment.
func awaitAutoscalerEvent(t *testing.T, c client.Client, pod *corev1.Pod, reason string) []corev1.Event {
	t.Helper()
	return awaitPodEvent(t, c, pod, "a "+reason+" event", func(e *corev1.Event) bool {
		return e.Reason == reason
	})
}

// TestLiveClusterAutoscaler_DeclinationIsRecognized is the drift detector proper: a
// pod no node group can ever satisfy, and the assertion that the matcher reads a real
// cluster-autoscaler's refusal as a declination.
//
// The scheduler's own FailedScheduling is on this pod too, so a pass also shows the
// matcher is not merely matching "some event exists".
func TestLiveClusterAutoscaler_DeclinationIsRecognized(t *testing.T) {
	c := caClient(t)
	ns := liveNamespace(t, c, "q474")

	pod := createPendingPod(t, c, ns, "declined", map[string]string{poolLabel: poolAbsent}, "100m")
	evts := awaitAutoscalerEvent(t, c, pod, reasonNotTriggerScaleUp)

	require.True(t, hasSchedulerFailure(evts),
		"expected kube-scheduler's own FailedScheduling alongside the autoscaler's verdict; "+
			"without it this case does not exercise the reporter discrimination")

	declined, detail := autoscalerDeclination(pod, evts)

	assert.True(t, declined, "a real NotTriggerScaleUp must close the gate")
	assert.Contains(t, detail, "pod didn't trigger scale-up",
		"the condition message carries the autoscaler's own text; a reworded prefix means "+
			"the operator docs quoting it are now wrong")

	// The reporter identity the FailedScheduling arm depends on. NotTriggerScaleUp is
	// unique to cluster-autoscaler and needs no reporter to be trusted, but Karpenter's
	// declination shares kube-scheduler's reason string and does — so if upstream ever
	// stops attributing its events, that arm silently stops recognizing anything.
	assert.Equal(t, "cluster-autoscaler", reporterOf(t, evts, reasonNotTriggerScaleUp),
		"cluster-autoscaler must still attribute its own events")
}

// TestLiveClusterAutoscaler_ScaleUpKeepsTheGateOpen is the check that actually
// matters (plan §9 step 3, in miniature): a gate that closed on a pod the autoscaler
// is rescuing would starve exactly the tenant this mode exists to protect.
//
// It asserts the outcome, not just the event — the pod must really land on a node
// that did not exist when it was created.
func TestLiveClusterAutoscaler_ScaleUpKeepsTheGateOpen(t *testing.T) {
	c := caClient(t)
	ns := liveNamespace(t, c, "q474")

	// Scale-down is disabled in this cluster, so a previous run leaves its fake nodes
	// behind — and an empty leftover node would place the pod with no scale-up at all,
	// turning the assertion below into a vacuous pass. Emptying the group first is what
	// makes the second run of this test assert the same thing as the first.
	resetPool(t, c, poolStandard)
	before := nodeNames(t, c)

	pod := createPendingPod(t, c, ns, "rescued", map[string]string{poolLabel: poolStandard}, "2")
	evts := awaitAutoscalerEvent(t, c, pod, reasonTriggeredScaleUp)

	declined, detail := autoscalerDeclination(pod, evts)

	assert.False(t, declined,
		"a pod the autoscaler is scaling up for must leave the gate open (detail was %q)", detail)

	scheduledOn := awaitScheduled(t, c, pod)
	assert.NotContains(t, before, scheduledOn,
		"the pod must land on a node the scale-up created, not on one that already existed")
}

// TestLiveClusterAutoscaler_CeilingIsNamed pins the one part of the message text the
// operator docs promise by name: when a node group is at its maximum,
// cluster-autoscaler says so, and that is what makes the condition actionable rather
// than merely true.
func TestLiveClusterAutoscaler_CeilingIsNamed(t *testing.T) {
	c := caClient(t)
	ns := liveNamespace(t, c, "q474")

	// Fill the single node the capped group can create, then ask for another.
	filler := createPendingPod(t, c, ns, "filler", map[string]string{poolLabel: poolCapped}, bigPodCPU)
	awaitScheduled(t, c, filler)

	pod := createPendingPod(t, c, ns, "overflow", map[string]string{poolLabel: poolCapped}, bigPodCPU)
	evts := awaitAutoscalerEvent(t, c, pod, reasonNotTriggerScaleUp)

	declined, detail := autoscalerDeclination(pod, evts)

	require.True(t, declined)
	assert.Contains(t, detail, "max node group size reached",
		"the node-group ceiling must still be named in the declination; "+
			"docs/design/04-operational-flows.md and the troubleshooting runbook quote it")
}

// reporterOf returns the reporting controller of the newest event with reason,
// preferring the new-style field and falling back to the legacy one exactly as
// reportedByScheduler does.
func reporterOf(t *testing.T, evts []corev1.Event, reason string) string {
	t.Helper()

	for i := range evts {
		e := &evts[i]
		if e.Reason != reason {
			continue
		}
		if e.ReportingController != "" {
			return e.ReportingController
		}
		return e.Source.Component
	}
	t.Fatalf("no event with reason %q", reason)
	return ""
}

// resetPool deletes every fake node cluster-autoscaler has created in one node group
// and blocks until they are gone. The kwok provider derives a group's current size
// from the Node objects in the cluster, so deleting them returns the group to zero and
// the next unschedulable pod gets a genuine scale-up. Only fake nodes carry the pool
// label, so a real kind node can never match this selector.
func resetPool(t *testing.T, c client.Client, pool string) {
	t.Helper()

	ctx := context.Background()
	require.NoError(t, c.DeleteAllOf(ctx, &corev1.Node{}, client.MatchingLabels{poolLabel: pool}))

	deadline := time.Now().Add(verdictBudget)
	for {
		var nodes corev1.NodeList
		require.NoError(t, c.List(ctx, &nodes, client.MatchingLabels{poolLabel: pool}))
		if len(nodes.Items) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("node group %q still has %d nodes after %s", pool, len(nodes.Items), verdictBudget)
		}
		time.Sleep(2 * time.Second)
	}
}
