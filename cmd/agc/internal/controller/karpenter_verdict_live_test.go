//go:build karpenter

// Package controller's live Karpenter test (Q479) — the second arm of the
// drift gate whose cluster-autoscaler arm is autoscaler_verdict_live_test.go.
//
// This is the arm that NEEDS a live counterpart most: Karpenter's declination
// shares kube-scheduler's reason string, FailedScheduling, so the matcher's
// entire Karpenter arm is the reporter discrimination — and the failure mode
// is double-silent. If upstream ever stopped attributing its events, every
// Karpenter declination would be read as the scheduler's own noise, the gate
// would never close on any Karpenter cluster, and no test against recorded
// samples could notice, because the samples carry the old attribution forever.
//
// scripts/e2e/karpenter-cluster.sh stands the cluster up (a real Karpenter built
// from the pinned upstream tag, provisioning fake kwok nodes); `make
// test-karpenter` runs this. Same posture as the CA arm: not in `make check`
// or per-PR CI — it runs on the PRs that move the pins.
package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// nodePoolLabel is Karpenter's own pool label: it propagates to every node a
	// pool creates, so a test pod selecting on it is pinned to that pool the same
	// way the CA tests pin to a kwok node group.
	nodePoolLabel = "karpenter.sh/nodepool"
	// karpenterPool is the one NodePool scripts/e2e/karpenter-cluster.sh installs
	// (test/karpenter/nodepool.yaml): room to grow, so a pod selecting it is
	// nominated and rescued.
	karpenterPool = "standard"
	// karpenterPoolAbsent names no NodePool at all, so a pod selecting it is
	// declined as incompatible with every pool that does exist.
	karpenterPoolAbsent = "no-such-pool"
)

// karpenterClient connects to the Karpenter harness cluster.
func karpenterClient(t *testing.T) client.Client {
	t.Helper()
	return liveClient(t, "KARPENTER_KUBE_CONTEXT", "KARPENTER_CLUSTER", "gag-karpenter", "make karpenter-cluster")
}

// isKarpenterDeclination matches the event the matcher's Karpenter arm gates
// on: the shared reason string from a positively non-scheduler reporter.
func isKarpenterDeclination(e *corev1.Event) bool {
	return e.Reason == reasonFailedScheduling && !reportedByScheduler(e, defaultSchedulerName)
}

// awaitKarpenterDeclination parks a pod that no NodePool can satisfy and
// blocks until Karpenter has recorded its refusal, returning all events on the
// pod. Both declination tests start exactly here.
func awaitKarpenterDeclination(t *testing.T, c client.Client, ns string) (*corev1.Pod, []corev1.Event) {
	t.Helper()

	pod := createPendingPod(t, c, ns, "declined", map[string]string{nodePoolLabel: karpenterPoolAbsent}, "100m")
	evts := awaitPodEvent(t, c, pod, `a non-scheduler "FailedScheduling"`, isKarpenterDeclination)
	return pod, evts
}

// TestLiveKarpenter_DeclinationIsRecognized is the reporter-discrimination
// check proper: a pod selecting a NodePool that does not exist, and the
// assertion that the matcher reads a real Karpenter's refusal as a declination
// while kube-scheduler's identically-named event sits in the same list.
func TestLiveKarpenter_DeclinationIsRecognized(t *testing.T) {
	c := karpenterClient(t)
	ns := liveNamespace(t, c, "q479")

	pod, evts := awaitKarpenterDeclination(t, c, ns)

	require.True(t, hasSchedulerFailure(evts),
		"expected kube-scheduler's own FailedScheduling alongside Karpenter's; "+
			"without it this case does not exercise the reporter discrimination")

	declined, detail := autoscalerDeclination(pod, evts)

	assert.True(t, declined, "a real Karpenter refusal must close the gate")
	// Only the prefix is pinned. The body after it varies with the failure shape
	// — measured at v1.14.0: "incompatible requirements, key karpenter.sh/nodepool,
	// …" for this pod, "no instance type has enough resources, …" for an
	// oversized one — and the matcher never parses it, so pinning a shape here
	// would fail on vocabulary the gate is indifferent to.
	assert.Contains(t, detail, "Failed to schedule pod",
		"the condition message carries Karpenter's own text; a reworded prefix means "+
			"the recorded samples in autoscaler_verdict_test.go are now wrong")

	// The attribution the whole arm hangs on. The operator runbook tells its
	// reader to look for "a FailedScheduling whose source is karpenter rather
	// than default-scheduler" — that name is upstream's, and this is where a
	// change to it surfaces.
	e := karpenterVerdictEvent(t, evts)
	reporter := e.ReportingController
	if reporter == "" {
		reporter = e.Source.Component
	}
	assert.Equal(t, "karpenter", reporter, "Karpenter must still attribute its own events")
}

// TestLiveKarpenter_NominationKeepsTheGateOpen is the fail-open direction: a
// pod Karpenter is provisioning for must not be read as declined, and the
// rescue must be real — the pod lands on a node that did not exist when it was
// created.
func TestLiveKarpenter_NominationKeepsTheGateOpen(t *testing.T) {
	c := karpenterClient(t)
	ns := liveNamespace(t, c, "q479")

	// A leftover node from a previous run would place the pod immediately, with
	// no nomination at all — emptying the pool first is what makes a re-run
	// assert the same thing as the first run. NodeClaims, not Nodes, are the
	// deletion target: their termination flow removes the node and is Karpenter's
	// own bookkeeping, where a bare node delete leaves a claim to garbage-collect
	// on Karpenter's schedule.
	resetKarpenterPool(t, c)
	before := nodeNames(t, c)

	pod := createPendingPod(t, c, ns, "nominated", map[string]string{nodePoolLabel: karpenterPool}, "2")
	evts := awaitPodEvent(t, c, pod, `a "Nominated" event`, func(e *corev1.Event) bool {
		return e.Reason == reasonNominated
	})

	declined, detail := autoscalerDeclination(pod, evts)

	assert.False(t, declined,
		"a pod Karpenter is provisioning for must leave the gate open (detail was %q)", detail)

	scheduledOn := awaitScheduled(t, c, pod)
	assert.NotContains(t, before, scheduledOn,
		"the pod must land on a node the nomination produced, not on one that already existed")
}

// TestLiveKarpenter_RecorderGeneration pins WHICH recorder generation
// Karpenter's events arrive through, because two shipped behaviors depend on
// the answer: eventTime() must find a usable timestamp on whichever fields are
// populated, and the concurrency window's effect (Q478) differs at second
// versus microsecond resolution. Karpenter records through client-go's legacy
// recorder — FirstTimestamp/LastTimestamp set, EventTime null — the same
// generation as cluster-autoscaler. A failure here means upstream migrated
// generations: re-measure, update eventTime()'s comment and the runbook's
// event-listing guidance, and re-derive the window's live behavior.
func TestLiveKarpenter_RecorderGeneration(t *testing.T) {
	c := karpenterClient(t)
	ns := liveNamespace(t, c, "q479")

	_, evts := awaitKarpenterDeclination(t, c, ns)
	e := karpenterVerdictEvent(t, evts)

	assert.False(t, e.LastTimestamp.IsZero(),
		"legacy-recorder events carry LastTimestamp; without it eventTime() has nothing to order by")
	assert.True(t, e.EventTime.IsZero(),
		"a populated EventTime means Karpenter moved to the microsecond recorder generation")
	assert.False(t, eventTime(e).IsZero(),
		"the matcher must find a usable timestamp on a real Karpenter event")
}

// karpenterVerdictEvent returns the Karpenter declination event out of a pod's
// event list.
func karpenterVerdictEvent(t *testing.T, evts []corev1.Event) *corev1.Event {
	t.Helper()

	for i := range evts {
		if isKarpenterDeclination(&evts[i]) {
			return &evts[i]
		}
	}
	t.Fatal("no non-scheduler FailedScheduling event in the list")
	return nil
}

// resetKarpenterPool deletes the pool's NodeClaims and blocks until the nodes
// they backed are gone. Unstructured on purpose: importing Karpenter's API
// module to delete a test fixture would put an autoscaler's types on the AGC's
// dependency graph.
func resetKarpenterPool(t *testing.T, c client.Client) {
	t.Helper()

	ctx := context.Background()
	nodeClaim := &unstructured.Unstructured{}
	nodeClaim.SetAPIVersion("karpenter.sh/v1")
	nodeClaim.SetKind("NodeClaim")
	require.NoError(t, c.DeleteAllOf(ctx, nodeClaim, client.MatchingLabels{nodePoolLabel: karpenterPool}))

	deadline := time.Now().Add(verdictBudget)
	for {
		var nodes corev1.NodeList
		require.NoError(t, c.List(ctx, &nodes, client.MatchingLabels{nodePoolLabel: karpenterPool}))
		if len(nodes.Items) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("node pool %q still has %d nodes after %s", karpenterPool, len(nodes.Items), verdictBudget)
		}
		time.Sleep(2 * time.Second)
	}
}
