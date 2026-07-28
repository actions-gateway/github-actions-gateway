//go:build autoscaler

// Package controller's live autoscaler test (Q474).
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
// docs/development/testing.md sets.
package controller

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
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

// verdictBudget bounds every wait. cluster-autoscaler's scan interval is 10s (set in
// test/autoscaler/cluster-autoscaler.yaml), and a verdict needs the scheduler to fail
// the pod first, so the floor is ~2 loops; the rest is headroom for a loaded machine.
const verdictBudget = 3 * time.Minute

// liveClient connects to the harness cluster. It FAILS rather than skips when the
// cluster is absent: this file only compiles under an explicit build tag, so getting
// here means someone asked for the live check, and a drift detector that quietly
// skips itself is worse than no drift detector at all.
func liveClient(t *testing.T) client.Client {
	t.Helper()

	kubeContext := os.Getenv("AUTOSCALER_KUBE_CONTEXT")
	if kubeContext == "" {
		cluster := os.Getenv("AUTOSCALER_CLUSTER")
		if cluster == "" {
			cluster = "gag-autoscaler"
		}
		kubeContext = "kind-" + cluster
	}

	cfg, err := ctrlconfig.GetConfigWithContext(kubeContext)
	require.NoErrorf(t, err, "no kubeconfig for context %q — run `make autoscaler-cluster` first", kubeContext)

	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	require.NoErrorf(t, err, "could not reach context %q — run `make autoscaler-cluster` first", kubeContext)
	return c
}

// liveNamespace creates a namespace for one test and deletes it afterwards, so a
// re-run never inherits a previous run's pods (or the capacity they held).
func liveNamespace(t *testing.T, c client.Client) string {
	t.Helper()

	name := fmt.Sprintf("q474-%d", time.Now().UnixNano())
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	require.NoError(t, c.Create(context.Background(), ns))
	t.Cleanup(func() {
		// Best-effort: a failed cleanup must not mask the assertion that failed.
		_ = c.Delete(context.Background(), ns)
	})
	return name
}

// createPendingPod creates a pod pinned to one kwok node group. It requests real CPU
// so cluster-autoscaler's simulation has something to decide about, and runs `pause`
// because kwok fakes the container anyway — nothing is ever pulled or executed.
func createPendingPod(t *testing.T, c client.Client, ns, name, pool, cpu string) *corev1.Pod {
	t.Helper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{poolLabel: pool},
			Containers: []corev1.Container{{
				Name:  "pause",
				Image: "registry.k8s.io/pause:3.10",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
				},
			}},
		},
	}
	require.NoError(t, c.Create(context.Background(), pod))
	return pod
}

// podEventsLive reads one pod's Events exactly the way the gate does in production —
// uncached, namespaced, field-selected server-side to that pod's name AND UID (see
// RunnerSetReconciler.podEvents). Reading them any other way here would prove the
// matcher works on a list shape the AGC never actually receives.
func podEventsLive(t *testing.T, c client.Client, pod *corev1.Pod) []corev1.Event {
	t.Helper()

	var list corev1.EventList
	require.NoError(t, c.List(context.Background(), &list,
		client.InNamespace(pod.Namespace),
		client.MatchingFields{
			"involvedObject.name": pod.Name,
			"involvedObject.uid":  string(pod.UID),
		},
	))
	return list.Items
}

// awaitAutoscalerEvent blocks until cluster-autoscaler has recorded an event with
// reason against pod, and returns every event on the pod at that moment — including
// the kube-scheduler FailedScheduling that always accompanies a stuck pod, which is
// the whole point: the matcher has to pick the autoscaler's verdict out of a list it
// does not control.
func awaitAutoscalerEvent(t *testing.T, c client.Client, pod *corev1.Pod, reason string) []corev1.Event {
	t.Helper()

	deadline := time.Now().Add(verdictBudget)
	for {
		evts := podEventsLive(t, c, pod)
		for i := range evts {
			if evts[i].Reason == reason {
				logEvents(t, pod.Name, evts)
				return evts
			}
		}
		if time.Now().After(deadline) {
			logEvents(t, pod.Name, evts)
			t.Fatalf("cluster-autoscaler never recorded a %q event on pod %s within %s.\n"+
				"If the events above show the autoscaler DID evaluate this pod under a different "+
				"reason string, that is the drift this test exists to catch: reconcile "+
				"cmd/agc/internal/controller/autoscaler_verdict.go with upstream and update the "+
				"recorded samples in autoscaler_verdict_test.go.", reason, pod.Name, verdictBudget)
		}
		time.Sleep(2 * time.Second)
	}
}

// logEvents records what the cluster actually said. A drift failure is only
// actionable if the run shows the new vocabulary next to the expected one.
func logEvents(t *testing.T, podName string, evts []corev1.Event) {
	t.Helper()

	t.Logf("--- events on pod %s (%d) ---", podName, len(evts))
	for i := range evts {
		e := &evts[i]
		reporter := e.ReportingController
		if reporter == "" {
			reporter = e.Source.Component
		}
		t.Logf("  [%s] reason=%q reporter=%q at=%s: %s",
			e.Type, e.Reason, reporter, eventTime(e).Format(time.RFC3339), e.Message)
	}
}

// TestLiveClusterAutoscaler_DeclinationIsRecognized is the drift detector proper: a
// pod no node group can ever satisfy, and the assertion that the matcher reads a real
// cluster-autoscaler's refusal as a declination.
//
// The scheduler's own FailedScheduling is on this pod too, so a pass also shows the
// matcher is not merely matching "some event exists".
func TestLiveClusterAutoscaler_DeclinationIsRecognized(t *testing.T) {
	c := liveClient(t)
	ns := liveNamespace(t, c)

	pod := createPendingPod(t, c, ns, "declined", poolAbsent, "100m")
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
	c := liveClient(t)
	ns := liveNamespace(t, c)

	// Scale-down is disabled in this cluster, so a previous run leaves its fake nodes
	// behind — and an empty leftover node would place the pod with no scale-up at all,
	// turning the assertion below into a vacuous pass. Emptying the group first is what
	// makes the second run of this test assert the same thing as the first.
	resetPool(t, c, poolStandard)
	before := nodeNames(t, c)

	pod := createPendingPod(t, c, ns, "rescued", poolStandard, "2")
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
	c := liveClient(t)
	ns := liveNamespace(t, c)

	// Fill the single node the capped group can create, then ask for another.
	filler := createPendingPod(t, c, ns, "filler", poolCapped, bigPodCPU)
	awaitScheduled(t, c, filler)

	pod := createPendingPod(t, c, ns, "overflow", poolCapped, bigPodCPU)
	evts := awaitAutoscalerEvent(t, c, pod, reasonNotTriggerScaleUp)

	declined, detail := autoscalerDeclination(pod, evts)

	require.True(t, declined)
	assert.Contains(t, detail, "max node group size reached",
		"the node-group ceiling must still be named in the declination; "+
			"docs/design/04-operational-flows.md and the troubleshooting runbook quote it")
}

// hasSchedulerFailure reports whether kube-scheduler recorded its own placement
// failure on this pod — the ambiguous reason the matcher must not read as a
// declination.
func hasSchedulerFailure(evts []corev1.Event) bool {
	for i := range evts {
		e := &evts[i]
		if e.Reason == reasonFailedScheduling && reportedByScheduler(e, defaultSchedulerName) {
			return true
		}
	}
	return false
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

func nodeNames(t *testing.T, c client.Client) []string {
	t.Helper()

	var nodes corev1.NodeList
	require.NoError(t, c.List(context.Background(), &nodes))
	names := make([]string, 0, len(nodes.Items))
	for i := range nodes.Items {
		names = append(names, nodes.Items[i].Name)
	}
	return names
}

// awaitScheduled blocks until the pod has a node assigned, and returns that node's
// name.
func awaitScheduled(t *testing.T, c client.Client, pod *corev1.Pod) string {
	t.Helper()

	deadline := time.Now().Add(verdictBudget)
	key := client.ObjectKeyFromObject(pod)
	for {
		var got corev1.Pod
		err := c.Get(context.Background(), key, &got)
		if err != nil && !apierrors.IsNotFound(err) {
			require.NoError(t, err)
		}
		if got.Spec.NodeName != "" {
			return got.Spec.NodeName
		}
		if time.Now().After(deadline) {
			logEvents(t, pod.Name, podEventsLive(t, c, pod))
			t.Fatalf("pod %s was never scheduled within %s", pod.Name, verdictBudget)
		}
		time.Sleep(2 * time.Second)
	}
}
