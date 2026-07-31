//go:build autoscaler || karpenter

// Shared plumbing for the live-autoscaler drift tests — the cluster-autoscaler
// harness (Q474, autoscaler_verdict_live_test.go) and the Karpenter harness
// (Q479, karpenter_verdict_live_test.go). Each runs against its own throwaway
// kind cluster under its own build tag; what they share is how a test reaches
// its cluster, parks a pod, and reads that pod's events back exactly the way
// the gate does in production.
package controller

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
)

// verdictBudget bounds every wait. Both autoscalers evaluate on a ~10s cadence
// (cluster-autoscaler's scan interval, Karpenter's provisioning batch bound),
// and a verdict needs the scheduler to fail the pod first, so the floor is a
// couple of loops; the rest is headroom for a loaded machine.
const verdictBudget = 3 * time.Minute

// liveClient connects to one harness's cluster, resolved from that harness's
// own environment (context override, cluster name, default). It FAILS rather
// than skips when the cluster is absent: these files only compile under an
// explicit build tag, so getting here means someone asked for the live check,
// and a drift detector that quietly skips itself is worse than no drift
// detector at all.
func liveClient(t *testing.T, contextEnv, clusterEnv, defaultCluster, standUp string) client.Client {
	t.Helper()

	kubeContext := os.Getenv(contextEnv)
	if kubeContext == "" {
		cluster := os.Getenv(clusterEnv)
		if cluster == "" {
			cluster = defaultCluster
		}
		kubeContext = "kind-" + cluster
	}

	cfg, err := ctrlconfig.GetConfigWithContext(kubeContext)
	require.NoErrorf(t, err, "no kubeconfig for context %q — run `%s` first", kubeContext, standUp)

	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	require.NoErrorf(t, err, "could not reach context %q — run `%s` first", kubeContext, standUp)
	return c
}

// liveNamespace creates a namespace for one test and deletes it afterwards, so
// a re-run never inherits a previous run's pods (or the capacity they held).
func liveNamespace(t *testing.T, c client.Client, prefix string) string {
	t.Helper()

	name := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	require.NoError(t, c.Create(context.Background(), ns))
	t.Cleanup(func() {
		// Best-effort: a failed cleanup must not mask the assertion that failed.
		_ = c.Delete(context.Background(), ns)
	})
	return name
}

// createPendingPod creates a pod pinned by nodeSelector. It requests real CPU
// so the autoscaler's simulation has something to decide about, and runs
// `pause` because kwok fakes the container anyway — nothing is ever pulled or
// executed.
func createPendingPod(t *testing.T, c client.Client, ns, name string, selector map[string]string, cpu string) *corev1.Pod {
	t.Helper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			NodeSelector: selector,
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

// podEventsLive reads one pod's Events exactly the way the gate does in
// production — uncached, namespaced, field-selected server-side to that pod's
// name AND UID (see RunnerSetReconciler.podEvents). Reading them any other way
// here would prove the matcher works on a list shape the AGC never actually
// receives.
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

// awaitPodEvent blocks until an event matching `match` has been recorded
// against pod, and returns every event on the pod at that moment — including
// the kube-scheduler FailedScheduling that always accompanies a stuck pod,
// which is the whole point: the matcher has to pick the autoscaler's verdict
// out of a list it does not control. want describes the awaited event for the
// drift-failure message.
func awaitPodEvent(t *testing.T, c client.Client, pod *corev1.Pod, want string, match func(*corev1.Event) bool) []corev1.Event {
	t.Helper()

	deadline := time.Now().Add(verdictBudget)
	for {
		evts := podEventsLive(t, c, pod)
		for i := range evts {
			if match(&evts[i]) {
				logEvents(t, pod.Name, evts)
				return evts
			}
		}
		if time.Now().After(deadline) {
			logEvents(t, pod.Name, evts)
			t.Fatalf("the autoscaler never recorded %s on pod %s within %s.\n"+
				"If the events above show it DID evaluate this pod under a different "+
				"vocabulary or attribution, that is the drift this test exists to catch: "+
				"reconcile cmd/agc/internal/controller/autoscaler_verdict.go with upstream "+
				"and update the recorded samples in autoscaler_verdict_test.go.", want, pod.Name, verdictBudget)
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

// awaitScheduled blocks until the pod has a node assigned, and returns that
// node's name.
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
