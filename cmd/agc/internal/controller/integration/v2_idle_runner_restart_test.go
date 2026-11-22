//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// driveToRunningSince flips pod to Running with its runner container up since upAgo,
// the kubelet's part (envtest runs none).
func driveToRunningSince(t *testing.T, pod *corev1.Pod, upAgo time.Duration) {
	t.Helper()
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: provisioner.WorkerContainerName,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
			StartedAt: metav1.NewTime(time.Now().Add(-upAgo)),
		}},
	}}
	require.NoError(t, k8sClient.Status().Update(ctx, pod))
}

// TestV2_RunnerSet_ScaleSet_RestartReapsIdleRunner is Q1360's restart case. The
// listener's provisioned guard is in memory, so a restarted AGC replaying its queue can
// build a worker for a job another runner already took, and the job's start then names
// that other runner: nothing ever stamps the new worker, and before Q1360 it held its
// node until the 12h lifetime cap.
//
// The restart is modelled as Q435 models it, by creating the pods before the manager
// starts. Each carries a runner name this set's listener mints, the record is
// registered at the fake, and the runner container has been up past idleRunnerGrace
// with no start recorded. GitHub's answer is the only thing that differs: an idle
// runner is reaped, and one running a job the AGC never recorded — the state an outage
// across the job's start leaves — is kept.
func TestV2_RunnerSet_ScaleSet_RestartReapsIdleRunner(t *testing.T) {
	const ns = "v2-rs-ss-idle-restart"
	const label = "linux-idle-restart"
	const setName = "ss-idle-restart"
	const (
		idleRunner = label + "-7c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f"
		busyRunner = label + "-8d2e3f4a-5b6c-4d7e-9f8a-0b1c2d3e4f5a"
	)
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	srv.FailJITConfigName(idleRunner) // registers the record generatejitconfig left
	srv.FailJITConfigName(busyRunner)
	srv.SetRunnerBusy(busyRunner)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet(setName, ns, "gw", label, 3)
	// Keep every phase-based arm out of the way, so a pass cannot be another arm in
	// disguise.
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		bg := context.Background()
		_ = k8sClient.Delete(bg, rs)
		_ = k8sClient.Delete(bg, &v2.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(bg, &v2.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	// --- The previous AGC process's leftovers, staged before anything is running.

	withRunner := func(name string) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Labels[provisioner.LabelAcquisitionProtocol] = provisioner.AcquisitionProtocolScaleSet
			p.Annotations = map[string]string{provisioner.AnnotationRunnerName: name}
			require.NoError(t, k8sClient.Update(ctx, p))
			driveToRunningSince(t, p, time.Hour)
		}
	}
	createOrphanedV2WorkerPod(t, ns, setName, "worker-idle", withRunner(idleRunner))
	createOrphanedV2WorkerPod(t, ns, setName, "worker-busy", withRunner(busyRunner))

	// --- The restart.

	startRunnerSetReconcilerWithScaleSet(t, srv)

	require.Eventually(t, func() bool { return podGone(ns, "worker-idle") },
		30*time.Second, 100*time.Millisecond,
		"a restarted AGC must reap a worker whose runner GitHub reports idle past the grace")
	require.NotContains(t, srv.RegisteredRunners(), idleRunner,
		"the idle runner's record must be deleted with its pod")

	var busy corev1.Pod
	require.Eventually(t, func() bool {
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "worker-busy"}, &busy); err != nil {
			return false
		}
		return busy.Annotations[provisioner.AnnotationRunnerBusyAt] != ""
	}, 10*time.Second, 100*time.Millisecond,
		"a worker whose runner is running a job at GitHub must be kept, and the verdict stamped")
	require.Contains(t, srv.RegisteredRunners(), busyRunner,
		"a runner running a job must keep its record")
}
