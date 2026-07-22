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
	require.Eventually(t, func() bool {
		var rs v2alpha1.RunnerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "measured-set"}, &rs); err != nil {
			return false
		}
		return len(rs.Status.SizingRecommendation) == 1 &&
			rs.Status.SizingProfileState == v2alpha1.SizingProfileStateActive
	}, 20*time.Second, 100*time.Millisecond, "measured-set should report sizingProfileState=Active")
	// The fresh set has no history: profile selected but not actuating.
	require.Eventually(t, func() bool {
		var rs v2alpha1.RunnerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "fresh-set"}, &rs); err != nil {
			return false
		}
		return rs.Status.SizingProfileState == v2alpha1.SizingProfileStateAwaitingSamples
	}, 20*time.Second, 100*time.Millisecond, "fresh-set should report sizingProfileState=AwaitingSamples")

	workerFor := func(set string) corev1.Pod {
		t.Helper()
		id := enqueueJobOnOwnerSession(15*time.Second, set, nil, broker.RunnerJobRequestBody{})
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
	runnerContainer := func(pod corev1.Pod) *corev1.Container {
		t.Helper()
		for i := range pod.Spec.Containers {
			if pod.Spec.Containers[i].Name == "runner" {
				return &pod.Spec.Containers[i]
			}
		}
		t.Fatal("worker pod must have a runner container")
		return nil
	}

	// Measured set: derived requests == limits (Guaranteed QoS), GPU untouched.
	runner := runnerContainer(workerFor("measured-set"))
	for _, rl := range []corev1.ResourceList{runner.Resources.Requests, runner.Resources.Limits} {
		assert.Equal(t, "500m", rl.Cpu().String(), "derived cpu")
		assert.Equal(t, "2Gi", rl.Memory().String(), "derived memory")
		gpu := rl["nvidia.com/gpu"]
		assert.Equal(t, "1", gpu.String(), "GPU must be byte-identical to the template")
	}

	// Fresh set: template's static values until the history is confident.
	runner = runnerContainer(workerFor("fresh-set"))
	assert.Equal(t, "2", runner.Resources.Requests.Cpu().String(), "fresh set keeps the template cpu")
	assert.Equal(t, "4Gi", runner.Resources.Requests.Memory().String(), "fresh set keeps the template memory")
}
