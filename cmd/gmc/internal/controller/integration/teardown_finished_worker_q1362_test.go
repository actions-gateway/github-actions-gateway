//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// These pin Q1362 under the GMC's shipped RBAC. A worker pod whose job finished is in
// no RunnerSet job count, so the Q547 drain gate used to pass with it still present:
// the GMC deleted the AGC, the pod's only reaper, and the Q1328 hold then kept the
// workload NetworkPolicy, and the gateway's finalizer, until something else deleted
// the pod. On the release gate that was the autoscaler removing the node. The drain
// gate now also waits for every bound worker pod to carry a deletion timestamp.

// teardownTenant creates a marked tenant namespace holding gateway gwName and a
// RunnerSet setName bound to it that reports no jobs, starts the GMC as its
// ServiceAccount, and waits for the workload NetworkPolicy.
func teardownTenant(t *testing.T, ns, gwName, setName string, tweaks ...func(*controller.ActionsGatewayV2Reconciler)) (*gmcv2.ActionsGateway, types.NamespacedName) {
	t.Helper()
	installTenantResourceGuard(t)
	installGMCShippedRBAC(t)

	createNamespaceWithLabels(t, ns, map[string]string{gmcv2.TenantNamespaceMarkerLabel: gmcv2.TenantNamespaceMarkerValue})
	createGitHubAppSecret(t, ns, "github-app")
	ag := newV2GatewayWired(gwName, ns, "github-app", "")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })
	rs := newBoundRunnerSet(setName, ns)
	rs.Spec.GatewayRef.Name = gwName
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rs) })

	startGMCAsServiceAccount(t, tweaks...)
	npKey := types.NamespacedName{Namespace: ns, Name: gwName + "-workload"}
	gomega.NewWithT(t).Eventually(func() error {
		return k8sClient.Get(ctx, npKey, &networkingv1.NetworkPolicy{})
	}, 20*time.Second, 50*time.Millisecond).Should(gomega.Succeed())
	return ag, npKey
}

// teardownWorker creates a worker pod of setName and sets its phase. A finalizer
// stands in for the kubelet's grace period once the pod is deleted.
func teardownWorker(t *testing.T, ns, setName, name string, phase corev1.PodPhase, finalizers ...string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, Finalizers: finalizers,
			Labels: map[string]string{"actions-gateway/component": "workload", "actions-gateway.com/runner-set": setName},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}}},
	}
	require.NoError(t, k8sClient.Create(ctx, pod))
	pod.Status.Phase = phase
	require.NoError(t, k8sClient.Status().Update(ctx, pod))
	t.Cleanup(func() {
		var p corev1.Pod
		if k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &p) == nil {
			p.Finalizers = nil
			_ = k8sClient.Update(context.Background(), &p)
			_ = k8sClient.Delete(context.Background(), &p)
		}
	})
	return pod
}

// gatewayEventNote returns the note of an Event with reason about gateway gwName, or "".
func gatewayEventNote(ns, gwName, reason string) string {
	var list eventsv1.EventList
	if err := k8sClient.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return ""
	}
	for i := range list.Items {
		e := &list.Items[i]
		if e.Regarding.Kind == "ActionsGateway" && e.Regarding.Name == gwName && e.Reason == reason {
			return e.Note
		}
	}
	return ""
}

func agcGone(ns, gwName string) bool {
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: gwName + controller.AGCResourceSuffix}, &appsv1.Deployment{})
	return apierrors.IsNotFound(err)
}

func gatewayGone(ag *gmcv2.ActionsGateway) bool {
	return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(ag), &gmcv2.ActionsGateway{}))
}

// TestGMC_Teardown_WaitsForTheAGCToDeleteAFinishedWorker is the release gate's case:
// a gateway deleted seconds after its last job finished. Teardown keeps the AGC until
// it has deleted the finished pod, and then completes in a pass or two rather than
// waiting for the pod's node.
func TestGMC_Teardown_WaitsForTheAGCToDeleteAFinishedWorker(t *testing.T) {
	const ns, gwName, setName = "teardown-finished-tenant", "gw-finished", "finished-set"
	ag, npKey := teardownTenant(t, ns, gwName, setName)
	pod := teardownWorker(t, ns, setName, "finished-set-w1", corev1.PodSucceeded)
	g := gomega.NewWithT(t)

	require.NoError(t, k8sClient.Delete(ctx, ag))
	g.Eventually(func() string { return gatewayEventNote(ns, gwName, "WaitingForWorkerDrain") },
		20*time.Second, 100*time.Millisecond).Should(gomega.ContainSubstring("worker pods not yet deleted: "+pod.Name),
		"teardown must name the finished worker it is waiting on")
	g.Consistently(func() bool { return agcGone(ns, gwName) }, 3*time.Second, 100*time.Millisecond).Should(gomega.BeFalse(),
		"the AGC is the finished pod's only reaper, so it must outlive the wait")

	// The AGC's part: its gateway_deleted reap deletes the pod. A finished pod is
	// removed at once, with no kubelet to wait on.
	require.NoError(t, k8sClient.Delete(ctx, pod))
	g.Eventually(func() bool { return gatewayGone(ag) }, 20*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(),
		"once the AGC has deleted the finished pod the gateway finalizes without waiting for the node")
	require.True(t, apierrors.IsNotFound(k8sClient.Get(ctx, npKey, &networkingv1.NetworkPolicy{})))
}

// TestGMC_Teardown_RunningWorkerStaysHeldUntilGone: a worker still running job code
// holds teardown twice over. The drain gate keeps the AGC until it has deleted the
// pod, and the Q1328 hold keeps the workload NetworkPolicy while the pod terminates.
func TestGMC_Teardown_RunningWorkerStaysHeldUntilGone(t *testing.T) {
	const ns, gwName, setName = "teardown-running-tenant", "gw-running", "running-set"
	const grace = "test.actions-gateway.com/kubelet-grace"
	ag, npKey := teardownTenant(t, ns, gwName, setName)
	pod := teardownWorker(t, ns, setName, "running-set-w1", corev1.PodRunning, grace)
	g := gomega.NewWithT(t)

	require.NoError(t, k8sClient.Delete(ctx, ag))
	g.Eventually(func() string { return gatewayEventNote(ns, gwName, "WaitingForWorkerDrain") },
		20*time.Second, 100*time.Millisecond).Should(gomega.ContainSubstring(pod.Name))
	require.False(t, agcGone(ns, gwName), "the AGC must outlive a worker it has not deleted")

	require.NoError(t, k8sClient.Delete(ctx, pod))
	g.Eventually(func() bool { return agcGone(ns, gwName) }, 20*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(),
		"a deletion timestamp on every worker releases the drain gate")
	g.Consistently(func() error { return k8sClient.Get(ctx, npKey, &networkingv1.NetworkPolicy{}) },
		3*time.Second, 100*time.Millisecond).Should(gomega.Succeed(), "the workload NetworkPolicy outlives a terminating worker")
	require.False(t, gatewayGone(ag), "the gateway is held by its finalizer")

	var p corev1.Pod
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &p))
	p.Finalizers = nil
	require.NoError(t, k8sClient.Update(ctx, &p))
	g.Eventually(func() bool { return gatewayGone(ag) }, 20*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
}

// TestGMC_Teardown_FinishedWorkerWithNoAGCPastTheDeadline: with no AGC to delete the
// pod, the drain gate still gives up at its deadline, and the rest of the control
// plane goes. The finished pod then holds the workload NetworkPolicy, as any pod the
// AGC never reaped does (Q1328), until something deletes it. The clock runs ahead of
// the deadline from the start.
func TestGMC_Teardown_FinishedWorkerWithNoAGCPastTheDeadline(t *testing.T) {
	const ns, gwName, setName = "teardown-deadline-tenant", "gw-deadline", "deadline-set"
	ag, npKey := teardownTenant(t, ns, gwName, setName, func(r *controller.ActionsGatewayV2Reconciler) {
		r.Now = func() time.Time { return time.Now().Add(3 * time.Minute) }
	})
	pod := teardownWorker(t, ns, setName, "deadline-set-w1", corev1.PodSucceeded)
	g := gomega.NewWithT(t)

	require.NoError(t, k8sClient.Delete(ctx, ag))
	g.Eventually(func() string { return gatewayEventNote(ns, gwName, "WorkerDrainTimeout") },
		20*time.Second, 100*time.Millisecond).Should(gomega.ContainSubstring("worker pods not yet deleted: "+pod.Name),
		"past the deadline teardown names the worker it is leaving behind")
	g.Eventually(func() bool { return agcGone(ns, gwName) }, 20*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(),
		"the deadline releases the drain gate")
	g.Eventually(func() string { return gatewayEventNote(ns, gwName, "TeardownIncomplete") },
		20*time.Second, 100*time.Millisecond).Should(gomega.Satisfy(func(note string) bool {
		return strings.Contains(note, "held for worker pods") && strings.Contains(note, pod.Name)
	}), "the unreaped pod holds the workload NetworkPolicy")
	require.NoError(t, k8sClient.Get(ctx, npKey, &networkingv1.NetworkPolicy{}))

	require.NoError(t, k8sClient.Delete(ctx, pod))
	g.Eventually(func() bool { return gatewayGone(ag) }, 20*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
}
