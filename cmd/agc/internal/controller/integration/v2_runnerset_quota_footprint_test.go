//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// Q450: a worker pod's ResourceQuota charge includes its native sidecars and its
// RuntimeClass pod overhead, neither of which appears in spec.containers. These
// tests prove the corrected footprint reaches the advisory conditions against a real
// apiserver — in particular that the RuntimeClass read works through the manager's
// cache for a CLUSTER-SCOPED kind, which a fake client cannot demonstrate.
//
// Both quotas below are sized so the condition can ONLY trip if the extra term is
// counted: the regular containers alone fit with room to spare.

// newRunnerTemplateWithSidecar builds a template whose expensive half is a native
// sidecar (an init container with restartPolicy: Always) — the shape the DinD daemon
// must use, since a regular-container sidecar strands the pod (Q249).
func newRunnerTemplateWithSidecar(name, ns, runnerCPU, sidecarCPU string) *v2alpha1.RunnerTemplate {
	tmpl := newRunnerTemplateWithCPURequest(name, ns, runnerCPU)
	tmpl.Spec.PodTemplate.Spec.InitContainers = []corev1.Container{{
		Name:          "dind",
		Image:         "docker:dind",
		RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(sidecarCPU)},
		},
	}}
	return tmpl
}

// TestV2_RunnerSet_WorkerQuota_CountsNativeSidecar proves the headline Q450 fix on
// the condition an operator actually reads. The runner asks 200m and the `dind`
// native sidecar asks 800m; the quota allows 500m. Counting containers alone reports
// 200m and leaves the condition False — the exact silent under-count that let the
// gate claim jobs whose pods the quota then rejected.
func TestV2_RunnerSet_WorkerQuota_CountsNativeSidecar(t *testing.T) {
	const ns = "v2-rs-quota-sidecar"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplateWithSidecar("tmpl", ns, "200m", "800m")))
	rs := newRunnerSet("sidecar-set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))

	// 500m: ample for the runner's 200m alone, far short of the 1000m the pod
	// actually costs once the sidecar is summed in.
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "tight", Namespace: ns},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("500m")}},
	}
	require.NoError(t, k8sClient.Create(ctx, quota))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), quota)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)

	waitForSetReadyReason(t, ns, "sidecar-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	require.Eventually(t, func() bool {
		c := setCondition(t, ns, "sidecar-set", v2alpha1.ConditionWorkerQuotaExceeded)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == "QuotaExhausted"
	}, 20*time.Second, 100*time.Millisecond,
		"a native sidecar's request must count toward the worker footprint (Q450)")
}

// TestV2_RunnerSet_WorkerQuota_CountsRuntimeClassOverhead proves the Kata half end to
// end: the template names a runtimeClassName but declares no overhead of its own, so
// the 250m is discoverable only by reading the cluster-scoped RuntimeClass. This also
// exercises the read path the fix depends on — a cluster-scoped Get through the
// manager cache, which routes to a separate cluster-wide informer.
func TestV2_RunnerSet_WorkerQuota_CountsRuntimeClassOverhead(t *testing.T) {
	const ns = "v2-rs-quota-overhead"
	createNSForAGC(t, ns)

	// Mirrors deploy/kata-ci/runtimeclass.yaml. Cluster-scoped, so it is shared
	// across the suite — use a test-unique name rather than "kata".
	rc := &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "kata-q450"},
		Handler:    "kata-qemu",
		Overhead: &nodev1.Overhead{PodFixed: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("160Mi"),
		}},
	}
	require.NoError(t, k8sClient.Create(ctx, rc))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rc) })

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	tmpl := newRunnerTemplateWithCPURequest("tmpl", ns, "500m")
	tmpl.Spec.PodTemplate.Spec.RuntimeClassName = ptr.To("kata-q450")
	require.NoError(t, k8sClient.Create(ctx, tmpl))
	rs := newRunnerSet("overhead-set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))

	// 600m: comfortably admits the container's 500m, but not the 750m the pod costs
	// once the Kata micro-VM's 250m of pod overhead is charged.
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "tight", Namespace: ns},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("600m")}},
	}
	require.NoError(t, k8sClient.Create(ctx, quota))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), quota)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)

	waitForSetReadyReason(t, ns, "overhead-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	require.Eventually(t, func() bool {
		c := setCondition(t, ns, "overhead-set", v2alpha1.ConditionWorkerQuotaExceeded)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == "QuotaExhausted"
	}, 20*time.Second, 100*time.Millisecond,
		"RuntimeClass pod overhead must count toward the worker footprint (Q450)")
}
