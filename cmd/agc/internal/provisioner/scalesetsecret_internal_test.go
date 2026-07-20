package provisioner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// The Q373 contract for the ScaleSet worker path: a JIT-config Secret is
// credential-bearing, so it must never outlive the worker pod that consumes it. Every
// exit of ProvisionScaleSetWorker that leaves no pod behind unstages the Secret it
// staged, and the steady-state Secret is reclaimed by CleanupScaleSetJob when the
// listener sees the job's terminal completion. Before the fix each of these paths
// leaked one Secret per job until the owning RunnerSet was deleted.

// secretExists reports whether the per-job scale-set Secret for jobID is present.
func secretExists(ctx context.Context, t *testing.T, c client.Client, ns, jobID string) bool {
	t.Helper()
	var s corev1.Secret
	err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: scaleSetSecretName(jobID)}, &s)
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

// scaleSetSecretTestTarget builds a stubTarget in team-a with the given spec.
func scaleSetSecretTestTarget(spec *ResolvedSpec) *stubTarget {
	return &stubTarget{key: client.ObjectKey{Namespace: "team-a", Name: "gpu"}, spec: spec}
}

// runningWorkerPod is an already-active worker pod carrying the owner label
// activePodCount selects on, so a MaxWorkers of 1 is already exhausted.
func runningWorkerPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "team-a",
			Labels:    map[string]string{LabelRunnerSet: "gpu"},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func int32Ptr(v int32) *int32 { return &v }

// TestProvisionScaleSetWorker_UnstagesSecretWhenCeilingHolds covers the concurrency-
// ceiling race exit: the listener gates capacity upstream, so a hold here is rare — but
// it is retried on every later poll, so a leaked Secret per hold compounds.
func TestProvisionScaleSetWorker_UnstagesSecretWhenCeilingHolds(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(runningWorkerPod("runner-gpu-existing")).Build()
	p := NewProvisioner(fc, nil, nil)

	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test", MaxWorkers: int32Ptr(1)})

	require.Error(t, p.ProvisionScaleSetWorker(ctx, target, "job-held", "eyJ4IjoxfQ=="),
		"the ceiling must hold with MaxWorkers=1 and one running worker")
	assert.False(t, secretExists(ctx, t, fc, "team-a", "job-held"),
		"a job held by the ceiling never gets a pod, so its Secret must not survive")
}

// TestProvisionScaleSetWorker_UnstagesSecretOnPodCreateError covers the exit where the
// pod could not be created at all (a rejected pod spec, a quota denial past its
// retries): the staged Secret has no consumer and must go.
func TestProvisionScaleSetWorker_UnstagesSecretOnPodCreateError(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					return apierrors.NewInternalError(errors.New("pod rejected"))
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	p := NewProvisioner(fc, nil, nil)

	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})

	require.Error(t, p.ProvisionScaleSetWorker(ctx, target, "job-nopod", "eyJ4IjoxfQ=="))
	assert.False(t, secretExists(ctx, t, fc, "team-a", "job-nopod"),
		"a Secret whose pod creation failed must be unstaged")
}

// TestProvisionScaleSetWorker_UnstagesSecretOnThrottleError covers the scale-up
// rate-limit exit (Q223): the wait is abandoned (an AGC shutdown cancels it), so this
// job never reaches pod creation — while the already-provisioned job's Secret, which a
// live pod mounts, must be left strictly alone.
func TestProvisionScaleSetWorker_UnstagesSecretOnThrottleError(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	p := NewProvisioner(fc, nil, nil)

	clock := newFakeClock()
	p.scaleUp = scaleUpLimiter{
		now:   clock.now,
		sleep: func(context.Context, time.Duration) error { return context.Canceled },
	}

	target := scaleSetSecretTestTarget(&ResolvedSpec{
		WorkerImage: "runner:test",
		ScaleUp:     &ScaleUpConfig{MaxPerSecond: 1, Burst: 1}, // one now, then throttle
	})

	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, "job-first", "eyJ4IjoxfQ=="))
	require.Error(t, p.ProvisionScaleSetWorker(ctx, target, "job-throttled", "eyJ4IjoxfQ=="),
		"the second job in the same instant must be throttled, and the wait errors")

	assert.False(t, secretExists(ctx, t, fc, "team-a", "job-throttled"),
		"a Secret abandoned in the scale-up throttle must be unstaged")
	assert.True(t, secretExists(ctx, t, fc, "team-a", "job-first"),
		"the running job's Secret must not be collateral damage")
}

// TestProvisionScaleSetWorker_ReplayKeepsAnotherDeliverysSecret is the guard rail on the
// unstage: a replayed job whose Secret already exists does NOT own it. An earlier
// delivery staged it and may already have a live worker pod mounting it, so a replay
// that then fails must leave the Secret alone — deleting it would strand that pod in
// ContainerCreating.
func TestProvisionScaleSetWorker_ReplayKeepsAnotherDeliverysSecret(t *testing.T) {
	ctx := context.Background()
	// The state an earlier delivery of job-replay left behind: its Secret, and the live
	// worker pod mounting it (which also exhausts MaxWorkers=1).
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(runningWorkerPod("runner-gpu-job-replay")).Build()
	p := NewProvisioner(fc, nil, nil)

	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test", MaxWorkers: int32Ptr(1)})
	require.NoError(t, fc.Create(ctx, p.buildSecret(target, scaleSetSecretName("job-replay"), "job-replay", "v", nil, "eyJ4IjoxfQ==")))

	// The replay re-stages (AlreadyExists, tolerated) and then hits the ceiling. The
	// Secret it found is not its to reclaim — the earlier delivery's pod mounts it.
	require.Error(t, p.ProvisionScaleSetWorker(ctx, target, "job-replay", "eyJ4IjoxfQ=="))
	assert.True(t, secretExists(ctx, t, fc, "team-a", "job-replay"),
		"a replay must not delete a Secret an earlier delivery's pod is mounting")
}

// TestCleanupScaleSetJob_ReclaimsAndIsIdempotent covers the steady-state reclaim point:
// the Secret survives provisioning (the pod mounts it) and is deleted when the listener
// reports the job terminally complete. A second call — a replayed completion message —
// is a no-op rather than an error.
func TestCleanupScaleSetJob_ReclaimsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	p := NewProvisioner(fc, nil, nil)

	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})

	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, "job-done", "eyJ4IjoxfQ=="))
	require.True(t, secretExists(ctx, t, fc, "team-a", "job-done"),
		"the Secret must outlive provisioning — the worker pod mounts it")

	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-done"))
	assert.False(t, secretExists(ctx, t, fc, "team-a", "job-done"),
		"a terminally completed job's Secret must be reclaimed")

	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-done"),
		"a replayed completion must be a no-op, not an error")
	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-never-existed"),
		"a completion for a job this process never provisioned must be a no-op")
}

// TestCleanupScaleSetJob_SurfacesDeleteErrors pins that a genuine API failure is
// reported to the listener (which logs it) rather than silently swallowed, so a
// persistent reclaim failure is diagnosable.
func TestCleanupScaleSetJob_SurfacesDeleteErrors(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "job-ss-x", errors.New("nope"))
			},
		}).Build()
	p := NewProvisioner(fc, nil, nil)

	require.Error(t, p.CleanupScaleSetJob(ctx, scaleSetSecretTestTarget(&ResolvedSpec{}), "job-x"))
}
