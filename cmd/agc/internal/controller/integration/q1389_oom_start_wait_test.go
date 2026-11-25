//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestV2_RunnerSet_ScaleSet_KilledWorkerIsClaimedAtItsStart (Q1389) takes the order dogfood
// measured through the real wiring: the runner is killed for memory before its JobStarted is
// handled, and the start arrives on the Failed pod. The only reconcile worker is parked, so
// the start's own pod event must be what claims the kill.
func TestV2_RunnerSet_ScaleSet_KilledWorkerIsClaimedAtItsStart(t *testing.T) {
	const ns, set, label, holdingSet = "v2-rs-ss-q1389", "ss-q1389", "linux-q1389", "ss-q1389-hold"
	createNSForAGC(t, ns)
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		holder.Close()
	})

	tmpl := newRunnerTemplate("tmpl", ns)
	tmpl.Spec.PodTemplate.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}
	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, tmpl))
	rs := newScaleSetRunnerSet(set, ns, "gw", label, 3)
	// The default TTL would do: this one shows the wait does not rest on it.
	rs.Spec.CompletedPodTTL = &metav1.Duration{}
	require.NoError(t, k8sClient.Create(ctx, rs))
	hold := newScaleSetRunnerSet(holdingSet, ns, "gw", "linux-q1389-hold", 1)
	t.Cleanup(func() {
		bg := context.Background()
		_ = k8sClient.Delete(bg, rs)
		_ = k8sClient.Delete(bg, hold)
		_ = k8sClient.Delete(bg, tmpl)
		_ = k8sClient.Delete(bg, &v2.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
	})

	shared := scaleSetClientsFor(srv)
	r, _ := startRunnerSetReconcilerWithScaleSetClients(t, func(s *v2.RunnerSet, gw *v2.ActionsGateway) (*scaleset.Client, error) {
		if s.Name != holdingSet {
			return shared(s, gw)
		}
		return scaleset.New(scaleset.Config{
			TokenProvider: stubProvider{},
			ConfigURL:     "https://github.com/acme",
			APIBase:       holder.URL,
			HTTPClient:    holder.Client(),
			PollClient:    holder.Client(),
		})
	}, func(*controller.RunnerSetReconciler) {})

	var ssID int
	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(label)
		ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register its scale set")
	waitForSetReadyReason(t, ns, set, metav1.ConditionTrue, v2.ReasonListenerActive)
	_, jobID := srv.EnqueueJob(ssID)

	var pod corev1.Pod
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: set}); err != nil {
			return false
		}
		for i := range pods.Items {
			if strings.HasPrefix(pods.Items[i].Name, "runner-") {
				pod = pods.Items[i]
				return true
			}
		}
		return false
	}, 20*time.Second, 50*time.Millisecond, "a worker pod must be provisioned for the assigned job")
	runner := pod.Annotations[provisioner.AnnotationRunnerName]
	require.NotEmpty(t, runner)
	// A sizing profile would stamp this; resolving one needs samples this tier has none of.
	base := pod.DeepCopy()
	pod.Annotations[provisioner.AnnotationSizingProfile] = "Throughput"
	require.NoError(t, k8sClient.Patch(ctx, &pod, client.MergeFrom(base)))

	require.NoError(t, k8sClient.Create(ctx, hold))
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the holding set's bootstrap never reached its stub, so nothing is parking the reconcile worker")
	}
	reconciles := r.ReconcileCountForTest()
	claimed := func() bool {
		var got corev1.Pod
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(&pod), &got); err != nil {
			return false
		}
		_, ok := got.Annotations[provisioner.AnnotationEvictionHandledAt]
		return ok
	}

	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(&pod), &pod))
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "runner", ContainerID: "containerd://killed",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 137, Reason: "OOMKilled", FinishedAt: metav1.Now()}}}}
	require.NoError(t, k8sClient.Status().Update(ctx, &pod))
	assert.Never(t, claimed, 2*time.Second, 50*time.Millisecond, "the kill waits for its start")

	require.True(t, srv.StartAssignedJob(ssID, jobID, runner))
	require.Eventually(t, claimed, 10*time.Second, 50*time.Millisecond,
		"the start is recorded on the Failed worker and its event claims the kill")
	var got corev1.Pod
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(&pod), &got))
	assert.Equal(t, jobID, got.Annotations[provisioner.AnnotationStartedJobID])
	require.Equal(t, reconciles, r.ReconcileCountForTest(),
		"a reconcile ran while the worker was meant to be parked, so this test did not pose its question")
}
