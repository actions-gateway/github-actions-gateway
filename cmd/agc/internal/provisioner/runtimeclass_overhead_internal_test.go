package provisioner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// overheadScheme registers core/v1 plus node.k8s.io/v1 so the fake client can serve
// RuntimeClass reads.
func overheadScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, nodev1.AddToScheme(s))
	return s
}

// kataRuntimeClass mirrors deploy/kata-ci/runtimeclass.yaml — the overhead the
// kata-deploy chart renders for kata-qemu.
func kataRuntimeClass() *nodev1.RuntimeClass {
	return &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "kata"},
		Handler:    "kata-qemu",
		Overhead: &nodev1.Overhead{PodFixed: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("160Mi"),
		}},
	}
}

// TestResolveWorkerPodSpec_ReadsRuntimeClassOverhead is the Q450 fix for the Kata
// half: the reference Kata worker names a runtimeClassName but declares no overhead
// of its own, so the 250m/160Mi is only discoverable from the cluster-scoped
// RuntimeClass. Without this read every Kata worker is under-counted by that much.
func TestResolveWorkerPodSpec_ReadsRuntimeClassOverhead(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(overheadScheme(t)).WithObjects(kataRuntimeClass()).Build()
	in := &corev1.PodSpec{
		RuntimeClassName: ptr.To("kata"),
		Containers:       []corev1.Container{{Name: WorkerContainerName}},
	}

	out := ResolveWorkerPodSpec(context.Background(), fc, in)

	require.NotNil(t, out)
	cpu := out.Overhead[corev1.ResourceCPU]
	assert.Equal(t, "250m", cpu.String())
	mem := out.Overhead[corev1.ResourceMemory]
	assert.Equal(t, "160Mi", mem.String())
	assert.Empty(t, in.Overhead, "the caller's spec must never be mutated — it is usually cache-owned")
}

// TestResolveWorkerPodSpec_DeclaredOverheadWins verifies a template-declared overhead
// short-circuits the read. That is sound rather than a shortcut: the RuntimeClass
// admission plugin rejects a pod whose declared overhead differs from its
// RuntimeClass's, so any declared value that survives admission IS the RuntimeClass's.
func TestResolveWorkerPodSpec_DeclaredOverheadWins(t *testing.T) {
	// A client with no RuntimeClass at all: if the declared value did not win, the
	// read would fail open to no overhead and the assertion below would catch it.
	fc := fake.NewClientBuilder().WithScheme(overheadScheme(t)).Build()
	in := &corev1.PodSpec{
		RuntimeClassName: ptr.To("kata"),
		Overhead:         corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
		Containers:       []corev1.Container{{Name: WorkerContainerName}},
	}

	out := ResolveWorkerPodSpec(context.Background(), fc, in)

	require.NotNil(t, out)
	cpu := out.Overhead[corev1.ResourceCPU]
	assert.Equal(t, "250m", cpu.String())
}

// TestResolveWorkerPodSpec_FailsOpen covers every way the resolution can come up
// empty. All of them must degrade to "no overhead" — the pre-Q450 answer — rather
// than erroring or blocking: overhead lives on a cluster-scoped object the tenant
// does not control, and starving a tenant over it would be worse than under-counting
// it. The unauthorized case is the one that matters operationally: an install whose
// AGC ClusterRole has not been updated with the runtimeclasses grant yet.
func TestResolveWorkerPodSpec_FailsOpen(t *testing.T) {
	tests := []struct {
		name   string
		scheme func(*testing.T) *runtime.Scheme
		objs   []client.Object
		spec   *corev1.PodSpec
	}{
		{
			name:   "no runtimeClassName",
			scheme: overheadScheme,
			spec:   &corev1.PodSpec{Containers: []corev1.Container{{Name: WorkerContainerName}}},
		},
		{
			name:   "empty runtimeClassName",
			scheme: overheadScheme,
			spec:   &corev1.PodSpec{RuntimeClassName: ptr.To("")},
		},
		{
			name:   "RuntimeClass does not exist",
			scheme: overheadScheme,
			spec:   &corev1.PodSpec{RuntimeClassName: ptr.To("kata")},
		},
		{
			name:   "RuntimeClass declares no overhead",
			scheme: overheadScheme,
			objs:   []client.Object{&nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "gvisor"}, Handler: "runsc"}},
			spec:   &corev1.PodSpec{RuntimeClassName: ptr.To("gvisor")},
		},
		{
			// The scheme omits node.k8s.io, so the Get fails the way a missing RBAC
			// rule or a lost cache would.
			name: "unauthorized read",
			scheme: func(t *testing.T) *runtime.Scheme {
				t.Helper()
				s := runtime.NewScheme()
				require.NoError(t, corev1.AddToScheme(s))
				return s
			},
			spec: &corev1.PodSpec{RuntimeClassName: ptr.To("kata")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := fake.NewClientBuilder().WithScheme(tt.scheme(t)).WithObjects(tt.objs...).Build()

			out := ResolveWorkerPodSpec(context.Background(), fc, tt.spec)

			require.NotNil(t, out)
			assert.Empty(t, out.Overhead, "an unresolvable overhead must degrade to zero, not error")
			assert.Same(t, tt.spec, out, "a no-op resolution must not copy the spec")
		})
	}
}

// TestResolveWorkerPodSpec_NilInputs verifies the two nil paths callers can hit when
// a template does not resolve, or before a client is wired.
func TestResolveWorkerPodSpec_NilInputs(t *testing.T) {
	assert.Nil(t, ResolveWorkerPodSpec(context.Background(), nil, nil))

	spec := &corev1.PodSpec{RuntimeClassName: ptr.To("kata")}
	assert.Same(t, spec, ResolveWorkerPodSpec(context.Background(), nil, spec),
		"a nil client must fail open, not panic")
}

// TestWorkerQuotaExhausted_ChargesRuntimeClassOverhead is the end-to-end assertion
// that the resolved overhead actually reaches the pre-claim gate — the consumer Q450
// names first. The quota below has exactly 1 CPU free and the containers ask for
// exactly 1, so the gate flips on the 250m of Kata overhead alone.
func TestWorkerQuotaExhausted_ChargesRuntimeClassOverhead(t *testing.T) {
	spec := &corev1.PodSpec{
		RuntimeClassName: ptr.To("kata"),
		Containers: []corev1.Container{{
			Name:      WorkerContainerName,
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
		}},
	}
	quota := cpuQuota("4", "3") // 1 CPU of headroom

	withoutRC := fake.NewClientBuilder().WithScheme(overheadScheme(t)).WithObjects(quota.DeepCopy()).Build()
	exhausted, _ := WorkerQuotaExhausted(context.Background(), withoutRC, "ns", spec)
	assert.False(t, exhausted, "1 CPU of headroom admits a 1-CPU pod when there is no overhead")

	withRC := fake.NewClientBuilder().WithScheme(overheadScheme(t)).
		WithObjects(quota.DeepCopy(), kataRuntimeClass()).Build()
	exhausted, detail := WorkerQuotaExhausted(context.Background(), withRC, "ns", spec)
	assert.True(t, exhausted, "the same pod no longer fits once its 250m of Kata pod overhead is charged")
	assert.Contains(t, detail, "requests.cpu")
}
