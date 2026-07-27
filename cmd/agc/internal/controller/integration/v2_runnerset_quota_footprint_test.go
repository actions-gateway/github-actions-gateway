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

// Q453: the storage keys. The two tests below constrain ONLY a storage key — no
// compute key at all — so the condition can trip solely through the storage half of
// the footprint. They cover the two arithmetics separately, because they differ:
// requests.storage sums a declared quantity, persistentvolumeclaims counts objects.

// newRunnerTemplateWithEphemeralVolume builds a template carrying a per-worker
// generic ephemeral volume, the shape the reference Kata worker uses for its raw
// block device (deploy/dogfood-e2e/overlays/kata/resources.yaml). Kubernetes creates
// a real PVC per pod from it, charged against the namespace quota.
func newRunnerTemplateWithEphemeralVolume(name, ns, storage string) *v2alpha1.RunnerTemplate {
	tmpl := newRunnerTemplateWithCPURequest(name, ns, "100m")
	tmpl.Spec.PodTemplate.Spec.Volumes = []corev1.Volume{{
		Name: "docker-blk",
		VolumeSource: corev1.VolumeSource{
			Ephemeral: &corev1.EphemeralVolumeSource{
				VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(storage)},
						},
					},
				},
			},
		},
	}}
	return tmpl
}

// TestV2_RunnerSet_WorkerQuota_CountsEphemeralVolumeStorage proves requests.storage
// reaches the operator-visible condition. The worker's ephemeral volume asks 100Gi
// and the quota allows 50Gi, so a single worker already cannot be placed.
//
// This is the failure the compute-only footprint hid best: an exhausted storage
// quota does not reject the worker POD — the pod is admitted and the PVC create
// fails behind it, leaving the pod Pending with an unbound volume and the job
// claimed. The condition was False the whole time.
func TestV2_RunnerSet_WorkerQuota_CountsEphemeralVolumeStorage(t *testing.T) {
	const ns = "v2-rs-quota-storage"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplateWithEphemeralVolume("tmpl", ns, "100Gi")))
	rs := newRunnerSet("storage-set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))

	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "tight", Namespace: ns},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceRequestsStorage: resource.MustParse("50Gi"),
		}},
	}
	require.NoError(t, k8sClient.Create(ctx, quota))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), quota)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)

	waitForSetReadyReason(t, ns, "storage-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	require.Eventually(t, func() bool {
		c := setCondition(t, ns, "storage-set", v2alpha1.ConditionWorkerQuotaExceeded)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == "QuotaExhausted"
	}, 20*time.Second, 100*time.Millisecond,
		"a generic ephemeral volume's storage ask must count toward the worker footprint (Q453)")
}

// TestV2_RunnerSet_WorkerQuota_CountsPerWorkerClaims proves the PVC COUNT reaches the
// warning tier, where the count-versus-quantity distinction is observable: the quota
// allows 3 claims and each worker creates exactly 1, so one worker fits (Exceeded
// stays False) but the set cannot reach its maxWorkers of 6 (Pressure trips). A
// footprint deriving the count from the storage total, or omitting it, gets both
// halves wrong.
func TestV2_RunnerSet_WorkerQuota_CountsPerWorkerClaims(t *testing.T) {
	const ns = "v2-rs-quota-claims"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplateWithEphemeralVolume("tmpl", ns, "10Gi")))
	rs := newRunnerSet("claims-set", ns, "gw")
	rs.Spec.MaxWorkers = ptr.To(int32(6))
	require.NoError(t, k8sClient.Create(ctx, rs))

	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "tight", Namespace: ns},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourcePersistentVolumeClaims: resource.MustParse("3"),
		}},
	}
	require.NoError(t, k8sClient.Create(ctx, quota))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), quota)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)

	waitForSetReadyReason(t, ns, "claims-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	require.Eventually(t, func() bool {
		c := setCondition(t, ns, "claims-set", v2alpha1.ConditionWorkerQuotaPressure)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == "InsufficientQuotaHeadroom"
	}, 20*time.Second, 100*time.Millisecond,
		"6 workers needing 1 PVC each must not fit a 3-claim quota (Q453)")

	// The error tier must stay False: one worker's single claim fits in 3, and
	// tripping it here would report "no job can run" for a set that can run several.
	exceeded := setCondition(t, ns, "claims-set", v2alpha1.ConditionWorkerQuotaExceeded)
	require.NotNil(t, exceeded)
	require.Equal(t, metav1.ConditionFalse, exceeded.Status,
		"one worker's claim fits the quota; only scaling to the ceiling does not")
}
