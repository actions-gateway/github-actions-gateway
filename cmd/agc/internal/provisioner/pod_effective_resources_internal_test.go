package provisioner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

// Q450: a worker pod's ResourceQuota charge is not the sum of spec.containers.
// These tests pin the four parts Kubernetes actually composes it from — regular
// containers, native sidecars, the init-container floor, and pod overhead — against
// the shapes this project ships, because the two ways to get this wrong have
// opposite and equally bad failure modes: under-count and the admission gate claims
// jobs whose pods the quota then rejects; over-count and a tenant is starved of
// capacity it paid for.

// sidecar returns an init container with restartPolicy: Always — a native sidecar,
// which is how the DinD daemon must be declared (Q249).
func sidecar(name string, req, lim corev1.ResourceList) corev1.Container {
	return corev1.Container{
		Name:          name,
		RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
		Resources:     corev1.ResourceRequirements{Requests: req, Limits: lim},
	}
}

// initContainer returns a plain (non-restarting) init container.
func initContainer(name string, req, lim corev1.ResourceList) corev1.Container {
	return corev1.Container{
		Name:      name,
		Resources: corev1.ResourceRequirements{Requests: req, Limits: lim},
	}
}

// rl builds a ResourceList from name/quantity pairs. A trailing unpaired name is
// ignored rather than panicking — the bound is on i+1 so the pair is always whole.
func rl(pairs ...string) corev1.ResourceList {
	out := corev1.ResourceList{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out[corev1.ResourceName(pairs[i])] = resource.MustParse(pairs[i+1])
	}
	return out
}

// assertFootprint checks the four quota keys of a single-pod footprint. An empty
// want string asserts the key is absent (the footprint omits zero-valued keys, which
// is what keeps a no-CPU-limit shape from being charged against limits.cpu).
func assertFootprint(t *testing.T, fp corev1.ResourceList, reqCPU, reqMem, limCPU, limMem string) {
	t.Helper()
	for _, c := range []struct {
		key  corev1.ResourceName
		want string
	}{
		{corev1.ResourceRequestsCPU, reqCPU},
		{corev1.ResourceRequestsMemory, reqMem},
		{corev1.ResourceLimitsCPU, limCPU},
		{corev1.ResourceLimitsMemory, limMem},
	} {
		got, ok := fp[c.key]
		if c.want == "" {
			assert.False(t, ok, "%s must be absent, got %v", c.key, got)
			continue
		}
		require.True(t, ok, "%s must be present", c.key)
		assert.Equal(t, c.want, got.String(), "%s", c.key)
	}
}

// TestWorkerFootprint_DinDReferenceShape is the regression test for the headline
// Q450 under-count. This is the measured shape from
// deploy/dogfood-e2e/overlays/dind/resources.yaml: the `dind` daemon is a native
// sidecar, so Kubernetes SUMS it with the runner container. Counting containers
// alone reported 3 CPU / 1Gi — under by the sidecar's entire ask (1 CPU / 3Gi
// requests, 4Gi memory limit), 25% of the pod's CPU and 75% of its memory.
func TestWorkerFootprint_DinDReferenceShape(t *testing.T) {
	spec := &corev1.PodSpec{
		InitContainers: []corev1.Container{
			sidecar("dind", rl("cpu", "1", "memory", "3Gi"), rl("memory", "4Gi")),
		},
		Containers: []corev1.Container{{
			Name:      WorkerContainerName,
			Resources: corev1.ResourceRequirements{Requests: rl("cpu", "3", "memory", "1Gi"), Limits: rl("memory", "3Gi")},
		}},
	}

	// runner 3 + dind 1 = 4 cpu; 1Gi + 3Gi = 4Gi; limits 3Gi + 4Gi = 7Gi. Neither
	// container declares a CPU limit, so limits.cpu stays absent — the shape the
	// sizing doc warns never to constrain.
	assertFootprint(t, WorkerFootprint(spec, 1), "4", "4Gi", "", "7Gi")

	// Linear in count: the worked example in docs/operations/resourcequota-sizing.md
	// sizes this tenant at maxWorkers 12.
	assertFootprint(t, WorkerFootprint(spec, 12), "48", "48Gi", "", "84Gi")
}

// TestWorkerFootprint_KataReferenceShape covers the other half of the finding on the
// shape from deploy/dogfood-e2e/overlays/kata/resources.yaml: a native sidecar AND
// RuntimeClass pod overhead. It also pins the overhead asymmetry — overhead is added
// in full to requests, but to limits only where a non-zero limit already exists.
func TestWorkerFootprint_KataReferenceShape(t *testing.T) {
	spec := &corev1.PodSpec{
		RuntimeClassName: ptr.To("kata"),
		Overhead:         rl("cpu", "250m", "memory", "160Mi"),
		InitContainers: []corev1.Container{
			sidecar("dind", rl("cpu", "2", "memory", "6Gi"), rl("cpu", "4", "memory", "8Gi")),
		},
		Containers: []corev1.Container{{
			Name:      WorkerContainerName,
			Resources: corev1.ResourceRequirements{Requests: rl("cpu", "2", "memory", "2Gi"), Limits: rl("cpu", "4", "memory", "4Gi")},
		}},
	}

	// requests: runner 2 + dind 2 + overhead 250m = 4250m cpu;
	//           2Gi + 6Gi + 160Mi = 8352Mi memory.
	// limits:   runner 4 + dind 4 + overhead 250m = 8250m cpu;
	//           4Gi + 8Gi + 160Mi = 12448Mi memory.
	assertFootprint(t, WorkerFootprint(spec, 1), "4250m", "8352Mi", "8250m", "12448Mi")
}

// TestPodEffectiveResources_OverheadLimitsAsymmetry pins the rule that is easiest to
// "simplify" wrongly: overhead always counts against requests, but against limits
// ONLY for a key the pod already limits. Adding overhead to an unlimited key would
// invent a limits.* charge the API server never makes, and the gate would then reject
// pods over a key the quota cannot constrain for this shape at all.
func TestPodEffectiveResources_OverheadLimitsAsymmetry(t *testing.T) {
	spec := &corev1.PodSpec{
		Overhead: rl("cpu", "250m", "memory", "160Mi"),
		Containers: []corev1.Container{{
			Name: WorkerContainerName,
			// Declares a memory limit but NO cpu limit — the recommended DinD posture.
			Resources: corev1.ResourceRequirements{Requests: rl("cpu", "1", "memory", "1Gi"), Limits: rl("memory", "2Gi")},
		}},
	}

	reqs := podEffectiveRequests(spec)
	cpu := reqs[corev1.ResourceCPU]
	assert.Equal(t, "1250m", cpu.String(), "overhead counts in full against requests")

	limits := podEffectiveLimits(spec)
	mem := limits[corev1.ResourceMemory]
	assert.Equal(t, "2208Mi", mem.String(), "overhead is added to a declared limit")
	_, hasCPU := limits[corev1.ResourceCPU]
	assert.False(t, hasCPU, "overhead must NOT create a cpu limit the pod never declared")
}

// TestPodEffectiveResources_PlainInitContainerIsAFloor verifies plain init containers
// keep contributing via max(), not sum — the part of the old behaviour that was
// already right and must stay right. Over-counting here would charge a tenant for
// short-lived setup steps that never run concurrently with the runner.
func TestPodEffectiveResources_PlainInitContainerIsAFloor(t *testing.T) {
	base := []corev1.Container{{
		Name:      WorkerContainerName,
		Resources: corev1.ResourceRequirements{Requests: rl("cpu", "2", "memory", "2Gi")},
	}}

	// A small init container is fully absorbed: the regular containers already ask
	// for more than it does.
	small := &corev1.PodSpec{
		InitContainers: []corev1.Container{initContainer("setup", rl("cpu", "500m"), nil)},
		Containers:     base,
	}
	assertFootprint(t, WorkerFootprint(small, 1), "2", "2Gi", "", "")

	// An init container that OUT-asks the regular containers raises the floor to its
	// own ask — but only for the resource it out-asks on.
	big := &corev1.PodSpec{
		InitContainers: []corev1.Container{initContainer("setup", rl("cpu", "8"), nil)},
		Containers:     base,
	}
	assertFootprint(t, WorkerFootprint(big, 1), "8", "2Gi", "", "")

	// The AGC's own wrapper init container declares no resources at all, so it must
	// contribute nothing — gap-fill deliberately skips init containers, matching
	// applyResourceDefaults.
	wrapper := &corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: wrapperInitName}},
		Containers:     base,
	}
	assertFootprint(t, WorkerFootprint(wrapper, 1), "2", "2Gi", "", "")
}

// TestPodEffectiveResources_SidecarRaisesTheInitFloor pins KEP-753's formula for the
// one case a simpler implementation gets wrong: a native sidecar declared BEFORE a
// plain init container is still running while that init container executes, so the
// floor it must clear is `sidecar + its own ask`, not its own ask alone.
func TestPodEffectiveResources_SidecarRaisesTheInitFloor(t *testing.T) {
	spec := &corev1.PodSpec{
		InitContainers: []corev1.Container{
			sidecar("dind", rl("cpu", "1"), nil),
			// Alone this would be absorbed by the 2-cpu runner; running alongside the
			// already-started sidecar it needs 1+2 = 3, which exceeds the
			// containers+sidecar sum of 2+1 = 3 only if it out-asks. Use 4 to make the
			// floor bind: 1 + 4 = 5 > 3.
			initContainer("migrate", rl("cpu", "4"), nil),
		},
		Containers: []corev1.Container{{
			Name:      WorkerContainerName,
			Resources: corev1.ResourceRequirements{Requests: rl("cpu", "2")},
		}},
	}

	// Sum path: runner 2 + sidecar 1 = 3. Init floor: sidecar 1 + migrate 4 = 5.
	// The pod is charged the max, 5 — not 3, and not the 7 a naive sum would give.
	assertFootprint(t, WorkerFootprint(spec, 1), "5", "", "", "")
}

// TestPodEffectiveResources_MultipleSidecarsAccumulate verifies native sidecars sum
// with each other as well as with the regular containers.
func TestPodEffectiveResources_MultipleSidecarsAccumulate(t *testing.T) {
	spec := &corev1.PodSpec{
		InitContainers: []corev1.Container{
			sidecar("dind", rl("cpu", "1", "memory", "3Gi"), nil),
			sidecar("proxy", rl("cpu", "500m", "memory", "256Mi"), nil),
		},
		Containers: []corev1.Container{{
			Name:      WorkerContainerName,
			Resources: corev1.ResourceRequirements{Requests: rl("cpu", "2", "memory", "1Gi")},
		}},
	}

	assertFootprint(t, WorkerFootprint(spec, 1), "3500m", "4352Mi", "", "")
}

// TestWorkerFootprint_NilSpec verifies the fail-open shape: an unresolved template
// yields a pods-only footprint rather than panicking. Every caller of the quota rung
// must fail open, so a nil spec has to be a legal input.
func TestWorkerFootprint_NilSpec(t *testing.T) {
	fp := WorkerFootprint(nil, 3)
	pods := fp[corev1.ResourcePods]
	assert.Equal(t, "3", pods.String())
	assert.Len(t, fp, 1, "a nil spec charges pod count only")
}
