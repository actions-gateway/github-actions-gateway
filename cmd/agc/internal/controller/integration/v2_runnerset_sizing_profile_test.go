//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/broker"
)

// TestV2_RunnerSet_BinpackProfileProvisionsDerivedResources drives the Phase 3
// acceptance path end to end: a Binpack RunnerSet with a confident persisted
// recommendation provisions worker pods with the DERIVED requests==limits
// (Guaranteed QoS) and the GPU ask byte-identical to the template, while a
// fresh set (no history) provisions the template's static values and reports
// sizingProfileState=AwaitingSamples.
func TestV2_RunnerSet_BinpackProfileProvisionsDerivedResources(t *testing.T) {
	const ns = "v2-rs-sizing-profile"
	createNSForAGC(t, ns)

	gw := newGatewayForSet("gw", ns, "")
	require.NoError(t, k8sClient.Create(ctx, gw))
	tmpl := newRunnerTemplate("tmpl", ns)
	tmpl.Spec.PodTemplate.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
			"nvidia.com/gpu":      resource.MustParse("1"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
			"nvidia.com/gpu":      resource.MustParse("1"),
		},
	}
	require.NoError(t, k8sClient.Create(ctx, tmpl))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), gw)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	r := startRunnerSetReconciler(t)
	// Seed a confident history for the measured set BEFORE creating it, so the
	// reconciler persists status.sizingRecommendation (the store Resolve reads)
	// ahead of the first job.
	r.Sizing.(*sizingStub).Set(types.NamespacedName{Namespace: ns, Name: "measured-set"},
		[]v2alpha1.ContainerSizingRecommendation{{
			Container: "runner",
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			},
			ObservedPeak: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("800m"),
				corev1.ResourceMemory: resource.MustParse("1536Mi"),
			},
			SampleCount: 25,
		}})

	binpack := &v2alpha1.WorkerSizing{Profile: v2alpha1.SizingProfileBinpack}
	measured := newRunnerSet("measured-set", ns, "gw")
	measured.Spec.Sizing = binpack
	require.NoError(t, k8sClient.Create(ctx, measured))
	fresh := newRunnerSet("fresh-set", ns, "gw")
	fresh.Spec.Sizing = binpack
	require.NoError(t, k8sClient.Create(ctx, fresh))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), measured)
		_ = k8sClient.Delete(context.Background(), fresh)
	})

	// The measured set persists the recommendation and reports the profile live.
	waitForSizingProfileState(t, ns, "measured-set", v2alpha1.SizingProfileStateActive)
	var measuredStatus v2alpha1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "measured-set"}, &measuredStatus))
	require.Len(t, measuredStatus.Status.SizingRecommendation, 1, "the recommendation should be persisted")
	// The fresh set has no history: profile selected but not actuating.
	waitForSizingProfileState(t, ns, "fresh-set", v2alpha1.SizingProfileStateAwaitingSamples)

	// Measured set: derived requests == limits (Guaranteed QoS), GPU untouched.
	runner := containerNamed(t, workerPodFor(t, ns, "measured-set"), "runner")
	for _, rl := range []corev1.ResourceList{runner.Resources.Requests, runner.Resources.Limits} {
		assert.Equal(t, "500m", rl.Cpu().String(), "derived cpu")
		assert.Equal(t, "2Gi", rl.Memory().String(), "derived memory")
		gpu := rl["nvidia.com/gpu"]
		assert.Equal(t, "1", gpu.String(), "GPU must be byte-identical to the template")
	}

	// Fresh set: template's static values until the history is confident.
	runner = containerNamed(t, workerPodFor(t, ns, "fresh-set"), "runner")
	assert.Equal(t, "2", runner.Resources.Requests.Cpu().String(), "fresh set keeps the template cpu")
	assert.Equal(t, "4Gi", runner.Resources.Requests.Memory().String(), "fresh set keeps the template memory")
}

// TestV2_RunnerSet_ThroughputProfileProvisionsBurstableResources covers the
// second history-based profile against the real apiserver. Throughput differs
// from Binpack in exactly the ways that make it Burstable rather than
// Guaranteed: requests come from the history, the CPU limit is REMOVED so jobs
// burst, and the memory limit is the observed peak scaled by the headroom
// percent. Both the default headroom and an explicit LimitHeadroomPercent are
// driven, since the field is inert unless the profile reads it.
func TestV2_RunnerSet_ThroughputProfileProvisionsBurstableResources(t *testing.T) {
	const ns = "v2-rs-sizing-throughput"
	createNSForAGC(t, ns)

	gw := newGatewayForSet("gw", ns, "")
	require.NoError(t, k8sClient.Create(ctx, gw))
	tmpl := newRunnerTemplate("tmpl", ns)
	tmpl.Spec.PodTemplate.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
			"nvidia.com/gpu":      resource.MustParse("1"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
			"nvidia.com/gpu":      resource.MustParse("1"),
		},
	}
	require.NoError(t, k8sClient.Create(ctx, tmpl))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), gw)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	r := startRunnerSetReconciler(t)
	// One history, two sets: the only difference is the headroom percent, so a
	// difference in the derived memory limit can only come from that field.
	history := []v2alpha1.ContainerSizingRecommendation{{
		Container: "runner",
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
		ObservedPeak: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("800m"),
			corev1.ResourceMemory: resource.MustParse("1536Mi"),
		},
		SampleCount: 25,
	}}
	for _, name := range []string{"default-headroom", "tuned-headroom"} {
		r.Sizing.(*sizingStub).Set(types.NamespacedName{Namespace: ns, Name: name}, history)
	}

	deflt := newRunnerSet("default-headroom", ns, "gw")
	deflt.Spec.Sizing = &v2alpha1.WorkerSizing{Profile: v2alpha1.SizingProfileThroughput}
	require.NoError(t, k8sClient.Create(ctx, deflt))
	headroom := int32(200)
	tuned := newRunnerSet("tuned-headroom", ns, "gw")
	tuned.Spec.Sizing = &v2alpha1.WorkerSizing{
		Profile:              v2alpha1.SizingProfileThroughput,
		LimitHeadroomPercent: &headroom,
	}
	require.NoError(t, k8sClient.Create(ctx, tuned))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), deflt)
		_ = k8sClient.Delete(context.Background(), tuned)
	})

	waitForSizingProfileState(t, ns, "default-headroom", v2alpha1.SizingProfileStateActive)
	waitForSizingProfileState(t, ns, "tuned-headroom", v2alpha1.SizingProfileStateActive)

	// Default headroom (150%): requests from the history, memory limit is the
	// 1536Mi observed peak x 1.5, and the CPU limit is gone so the job bursts.
	runner := containerNamed(t, workerPodFor(t, ns, "default-headroom"), "runner")
	assert.Equal(t, "500m", runner.Resources.Requests.Cpu().String(), "cpu request from the history")
	assert.Equal(t, "1Gi", runner.Resources.Requests.Memory().String(), "memory request from the history")
	_, hasCPULimit := runner.Resources.Limits[corev1.ResourceCPU]
	assert.False(t, hasCPULimit, "Throughput must remove the CPU limit so jobs burst (Burstable, not Guaranteed)")
	assert.Equal(t, "2304Mi", runner.Resources.Limits.Memory().String(), "memory limit is the observed peak x the default 150% headroom")
	gpuReq := runner.Resources.Requests["nvidia.com/gpu"]
	gpuLim := runner.Resources.Limits["nvidia.com/gpu"]
	assert.Equal(t, "1", gpuReq.String(), "GPU request must be byte-identical to the template")
	assert.Equal(t, "1", gpuLim.String(), "GPU limit must be byte-identical to the template")

	// Explicit 200% headroom moves only the memory limit.
	runner = containerNamed(t, workerPodFor(t, ns, "tuned-headroom"), "runner")
	assert.Equal(t, "500m", runner.Resources.Requests.Cpu().String(), "cpu request is unaffected by the headroom")
	assert.Equal(t, "3Gi", runner.Resources.Limits.Memory().String(), "memory limit is the observed peak x the configured 200% headroom")
}

// TestV2_RunnerSet_NodeShareProfileDividesTheNodeEnvelope covers the profile
// that needs no usage history at all — the property that makes it the one an
// operator can turn on from day one, and the reason it cannot be folded into
// the Binpack/Throughput confidence gate. It also pins the three behaviours
// unique to this path: only the runner container is resized, a template limit
// below the derived request is lifted rather than provisioning a pod the
// apiserver would reject, and maxRequests still clamps the derived value.
func TestV2_RunnerSet_NodeShareProfileDividesTheNodeEnvelope(t *testing.T) {
	const ns = "v2-rs-sizing-nodeshare"
	createNSForAGC(t, ns)

	gw := newGatewayForSet("gw", ns, "")
	require.NoError(t, k8sClient.Create(ctx, gw))
	tmpl := newRunnerTemplate("tmpl", ns)
	// The runner's template CPU limit (1) sits BELOW the share this envelope
	// derives (2), which is the case applyNodeShare has to lift.
	tmpl.Spec.PodTemplate.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
			"nvidia.com/gpu":      resource.MustParse("1"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("8Gi"),
			"nvidia.com/gpu":      resource.MustParse("1"),
		},
	}
	// A sidecar the profile must not touch: workersPerNode is the operator's
	// accounting for it, so resizing it too would double-count.
	tmpl.Spec.PodTemplate.Spec.Containers = append(tmpl.Spec.PodTemplate.Spec.Containers, corev1.Container{
		Name:  "dind",
		Image: "dind:test",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
	})
	require.NoError(t, k8sClient.Create(ctx, tmpl))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), gw)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)
	// Deliberately NO sizing history is seeded for either set.
	envelope := func() *v2alpha1.NodeShareSizing {
		return &v2alpha1.NodeShareSizing{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("8"),
				corev1.ResourceMemory: resource.MustParse("32Gi"),
			},
			WorkersPerNode: 4,
		}
	}
	shared := newRunnerSet("shared-node", ns, "gw")
	shared.Spec.Sizing = &v2alpha1.WorkerSizing{
		Profile:   v2alpha1.SizingProfileNodeShare,
		NodeShare: envelope(),
	}
	require.NoError(t, k8sClient.Create(ctx, shared))
	clamped := newRunnerSet("clamped-node", ns, "gw")
	clamped.Spec.Sizing = &v2alpha1.WorkerSizing{
		Profile:     v2alpha1.SizingProfileNodeShare,
		NodeShare:   envelope(),
		MaxRequests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1500m")},
	}
	require.NoError(t, k8sClient.Create(ctx, clamped))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), shared)
		_ = k8sClient.Delete(context.Background(), clamped)
	})

	// Active with zero samples — the whole point of the profile. Binpack and
	// Throughput would report AwaitingSamples in this exact state.
	waitForSizingProfileState(t, ns, "shared-node", v2alpha1.SizingProfileStateActive)
	var status v2alpha1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "shared-node"}, &status))
	require.Empty(t, status.Status.SizingRecommendation, "NodeShare must actuate with no recommendation at all")

	pod := workerPodFor(t, ns, "shared-node")
	runner := containerNamed(t, pod, "runner")
	// 8 cpu / 4 workers, 32Gi / 4 workers.
	assert.Equal(t, "2", runner.Resources.Requests.Cpu().String(), "cpu request is the node share")
	assert.Equal(t, "8Gi", runner.Resources.Requests.Memory().String(), "memory request is the node share")
	assert.Equal(t, "2", runner.Resources.Limits.Cpu().String(), "a template limit below the derived request must be lifted to it")
	assert.Equal(t, "8Gi", runner.Resources.Limits.Memory().String(), "a template limit at the derived request is left alone")
	gpuReq := runner.Resources.Requests["nvidia.com/gpu"]
	assert.Equal(t, "1", gpuReq.String(), "GPU must be byte-identical to the template")

	sidecar := containerNamed(t, pod, "dind")
	assert.Equal(t, "250m", sidecar.Resources.Requests.Cpu().String(), "the sidecar keeps its template cpu ask")
	assert.Equal(t, "512Mi", sidecar.Resources.Requests.Memory().String(), "the sidecar keeps its template memory ask")

	// maxRequests clamps the derived share, and the lift respects the clamp.
	waitForSizingProfileState(t, ns, "clamped-node", v2alpha1.SizingProfileStateActive)
	runner = containerNamed(t, workerPodFor(t, ns, "clamped-node"), "runner")
	assert.Equal(t, "1500m", runner.Resources.Requests.Cpu().String(), "maxRequests clamps the derived cpu share")
	assert.Equal(t, "1500m", runner.Resources.Limits.Cpu().String(), "the lifted limit follows the clamped request")
	assert.Equal(t, "8Gi", runner.Resources.Requests.Memory().String(), "memory has no clamp configured")
}

// TestV2_RunnerSet_ThroughputCPULimitInjectionIsReported covers Q489 end to end
// against a real apiserver, with a real LimitRange doing the injection: the AGC
// builds the runner container with no CPU limit, admission puts one back, nothing
// is rejected, and the set must report SizingProfileOverridden=True naming the pod
// that came back wrong.
//
// The detection reads the admitted pods, not the LimitRange, so this also proves the
// property that motivated that choice: the AGC needs no limitranges access at all —
// this suite's AGC client has none granted, and the LimitRange here is created by the
// test's admin client purely to make the apiserver mutate the pod.
func TestV2_RunnerSet_ThroughputCPULimitInjectionIsReported(t *testing.T) {
	const ns = "v2-rs-sizing-injected"
	createNSForAGC(t, ns)

	gw := newGatewayForSet("gw", ns, "")
	require.NoError(t, k8sClient.Create(ctx, gw))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), gw)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	r := startRunnerSetReconciler(t)
	history := []v2alpha1.ContainerSizingRecommendation{{
		Container: "runner",
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		Limits:       corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
		ObservedPeak: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1536Mi")},
		SampleCount:  25,
	}}
	r.Sizing.(*sizingStub).Set(types.NamespacedName{Namespace: ns, Name: "capped"}, history)

	// The namespace policy that cancels the profile. Declared before any job runs, so
	// the very first worker pod is admitted with the limit injected.
	lr := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-defaults", Namespace: ns},
		Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
			Type:    corev1.LimitTypeContainer,
			Default: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
		}}},
	}
	require.NoError(t, k8sClient.Create(ctx, lr))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), lr) })

	rs := newRunnerSet("capped", ns, "gw")
	rs.Spec.Sizing = &v2alpha1.WorkerSizing{Profile: v2alpha1.SizingProfileThroughput}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rs) })

	waitForSizingProfileState(t, ns, "capped", v2alpha1.SizingProfileStateActive)
	// Nothing built yet: the condition must not claim health it has not observed.
	waitForSizingOverride(t, ns, "capped", metav1.ConditionFalse, v2alpha1.ReasonAwaitingWorkerPods)

	// Run a job. The AGC builds the runner container with no CPU limit; the apiserver
	// admits it with one.
	pod := workerPodFor(t, ns, "capped")
	assert.Equal(t, v2alpha1.SizingProfileThroughput, pod.Annotations[provisioner.AnnotationSizingProfile],
		"a profile-derived pod must be marked as such")
	runner := containerNamed(t, pod, "runner")
	cpuLimit, hasCPULimit := runner.Resources.Limits[corev1.ResourceCPU]
	require.True(t, hasCPULimit, "the LimitRange should have injected a CPU limit the profile removed")
	assert.Equal(t, "2", cpuLimit.String())
	assert.Equal(t, "500m", runner.Resources.Requests.Cpu().String(), "the derived request still applies")

	// The worker-pod watch carries that pod back into a reconcile, and the set says so.
	waitForSizingOverride(t, ns, "capped", metav1.ConditionTrue, v2alpha1.ReasonCPULimitInjected)
	var overridden v2alpha1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "capped"}, &overridden))
	msg := meta.FindStatusCondition(overridden.Status.Conditions, v2alpha1.ConditionSizingProfileOverridden).Message
	assert.Contains(t, msg, pod.Name, "the message must name the pod that came back with the limit")

	// Advisory only: the set is still Ready and still serving.
	waitForSetReadyReason(t, ns, "capped", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)
}

// waitForSizingOverride blocks until the RunnerSet reports the expected
// SizingProfileOverridden status/reason.
func waitForSizingOverride(t *testing.T, ns, name string, wantStatus metav1.ConditionStatus, wantReason string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var rs v2alpha1.RunnerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &rs); err != nil {
			return false
		}
		c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionSizingProfileOverridden)
		return c != nil && c.Status == wantStatus && c.Reason == wantReason
	}, 20*time.Second, 100*time.Millisecond,
		"RunnerSet %s should report SizingProfileOverridden=%s/%s", name, wantStatus, wantReason)
}

// workerPodFor enqueues one job on the set's owner session and returns the
// worker pod the provisioner builds for it — the only way to observe what
// applySizingProfile actually produced, since the transform runs at pod build.
func workerPodFor(t *testing.T, ns, set string) corev1.Pod {
	t.Helper()
	id := enqueueJobOnSetSession(15*time.Second, set, nil, broker.RunnerJobRequestBody{})
	require.NotEmpty(t, id, "a session for %s should register", set)
	var pod corev1.Pod
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods,
			client.InNamespace(ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: set},
		); err != nil {
			return false
		}
		for _, p := range pods.Items {
			if strings.HasPrefix(p.Name, "runner-") {
				pod = p
				return true
			}
		}
		return false
	}, 20*time.Second, 50*time.Millisecond, "worker pod should be created for %s", set)
	return pod
}

// containerNamed returns the named container of a worker pod.
func containerNamed(t *testing.T, pod corev1.Pod, name string) *corev1.Container {
	t.Helper()
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	t.Fatalf("worker pod must have a %s container", name)
	return nil
}

// waitForSizingProfileState blocks until the RunnerSet reports the expected
// status.sizingProfileState.
func waitForSizingProfileState(t *testing.T, ns, name, want string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var rs v2alpha1.RunnerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &rs); err != nil {
			return false
		}
		return rs.Status.SizingProfileState == want
	}, 20*time.Second, 100*time.Millisecond, "%s should report sizingProfileState=%s", name, want)
}
