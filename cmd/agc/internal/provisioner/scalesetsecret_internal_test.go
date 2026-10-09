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

	require.Error(t, p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{JobID: "job-held", JITConfig: "eyJ4IjoxfQ=="}),
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

	require.Error(t, p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{JobID: "job-nopod", JITConfig: "eyJ4IjoxfQ=="}))
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

	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{JobID: "job-first", JITConfig: "eyJ4IjoxfQ=="}))
	require.Error(t, p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{JobID: "job-throttled", JITConfig: "eyJ4IjoxfQ=="}),
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
	require.Error(t, p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{JobID: "job-replay", JITConfig: "eyJ4IjoxfQ=="}))
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

	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{JobID: "job-done", JITConfig: "eyJ4IjoxfQ=="}))
	require.True(t, secretExists(ctx, t, fc, "team-a", "job-done"),
		"the Secret must outlive provisioning — the worker pod mounts it")

	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-done", ""))
	assert.False(t, secretExists(ctx, t, fc, "team-a", "job-done"),
		"a terminally completed job's Secret must be reclaimed")

	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-done", ""),
		"a replayed completion must be a no-op, not an error")
	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-never-existed", ""),
		"a completion for a job this process never provisioned must be a no-op")
}

// TestCleanupScaleSetJob_StampsJobCompletion covers Q420: the reclaim point is also
// where a worker pod learns its job is over, which is the only thing that gives a
// still-Running scale-set worker a reap deadline. The stamp is set once — a replayed
// completion (a re-created session polls from cursor 0) must not push the deadline
// back — and a job with no pod is not an error.
func TestCleanupScaleSetJob_StampsJobCompletion(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	p := NewProvisioner(fc, nil, nil)
	completedAt := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return completedAt }

	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})
	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{JobID: "job-done", JITConfig: "eyJ4IjoxfQ=="}))

	podKey := client.ObjectKey{Namespace: "team-a", Name: scaleSetPodName("gpu", "job-done")}
	var pod corev1.Pod
	require.NoError(t, fc.Get(ctx, podKey, &pod))
	assert.NotContains(t, pod.Annotations, AnnotationJobCompletedAt,
		"a freshly provisioned worker has no completion stamp — its job is still assigned")

	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-done", ""))
	require.NoError(t, fc.Get(ctx, podKey, &pod))
	assert.Equal(t, completedAt.Format(time.RFC3339), pod.Annotations[AnnotationJobCompletedAt],
		"the terminal completion must stamp the worker pod with the time the job ended")

	// A replay lands later; the original stamp must survive it.
	p.now = func() time.Time { return completedAt.Add(time.Hour) }
	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-done", ""))
	require.NoError(t, fc.Get(ctx, podKey, &pod))
	assert.Equal(t, completedAt.Format(time.RFC3339), pod.Annotations[AnnotationJobCompletedAt],
		"a replayed completion must not push the reap deadline back")

	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-with-no-pod", ""),
		"a completion for a job whose pod was never created must be a no-op")
}

// TestCleanupScaleSetJob_SurfacesStampErrors pins that a failure to stamp the pod is
// reported rather than swallowed: an unstamped Running worker has no reap deadline, so
// a persistent failure is the Q420 leak returning silently.
func TestCleanupScaleSetJob_SurfacesStampErrors(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(runningWorkerPod(scaleSetPodName("gpu", "job-x"))).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "runner-gpu-job-x", errors.New("nope"))
			},
		}).Build()
	p := NewProvisioner(fc, nil, nil)

	require.Error(t, p.CleanupScaleSetJob(ctx, scaleSetSecretTestTarget(&ResolvedSpec{}), "job-x", ""))
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

	require.Error(t, p.CleanupScaleSetJob(ctx, scaleSetSecretTestTarget(&ResolvedSpec{}), "job-x", ""))
}

// provisionHolder provisions a scale-set worker for jobID whose runner is registered as
// "gpu-<jobID>", the way the listener names it, and returns that pod's key.
func provisionHolder(ctx context.Context, t *testing.T, p *Provisioner, target Target, jobID string) client.ObjectKey {
	t.Helper()
	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target,
		ScaleSetJob{JobID: jobID, JITConfig: "eyJ4IjoxfQ==", RunnerName: "gpu-" + jobID}))
	return client.ObjectKey{Namespace: "team-a", Name: scaleSetPodName("gpu", jobID)}
}

// setPhase moves a worker pod to phase, as the kubelet would.
func setPhase(ctx context.Context, t *testing.T, c client.Client, key client.ObjectKey, phase corev1.PodPhase) {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, c.Get(ctx, key, &pod))
	pod.Status.Phase = phase
	require.NoError(t, c.Status().Update(ctx, &pod))
}

func podAnnotations(ctx context.Context, t *testing.T, c client.Client, key client.ObjectKey) map[string]string {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, c.Get(ctx, key, &pod))
	return pod.Annotations
}

// TestCleanupScaleSetJob_ReclaimsTheRunnerThatHeldTheJob is the Q1151 defect as
// measured on dogfood: job-a's worker is running job-b, and job-a finished on job-b's
// worker. The completion names that runner, so it is job-b's worker that is stamped and
// loses its Secret, while job-a's worker — mid job — is left with neither.
func TestCleanupScaleSetJob_ReclaimsTheRunnerThatHeldTheJob(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithStatusSubresource(&corev1.Pod{}).Build()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})

	mintedForA := provisionHolder(ctx, t, p, target, "job-a")
	mintedForB := provisionHolder(ctx, t, p, target, "job-b")
	setPhase(ctx, t, fc, mintedForA, corev1.PodRunning)
	setPhase(ctx, t, fc, mintedForB, corev1.PodRunning)

	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-a", "gpu-job-b"))

	assert.NotContains(t, podAnnotations(ctx, t, fc, mintedForA), AnnotationJobCompletedAt,
		"the worker minted for job-a is running another job and must not get a reap deadline")
	assert.True(t, secretExists(ctx, t, fc, "team-a", "job-a"),
		"nor may it lose the Secret it mounts")
	assert.Contains(t, podAnnotations(ctx, t, fc, mintedForB), AnnotationJobCompletedAt,
		"the worker whose runner held job-a is the one whose job is over")
	assert.False(t, secretExists(ctx, t, fc, "team-a", "job-b"),
		"and its Secret is the one to reclaim")
}

// TestCleanupScaleSetJob_ReclaimsTheMintedSecretWhenTheHolderIsGone covers the
// ordinary case once the worker has already been collected: the runner is the one
// minted for the job, no pod carries its name any more, and the Secret must still go.
func TestCleanupScaleSetJob_ReclaimsTheMintedSecretWhenTheHolderIsGone(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})

	key := provisionHolder(ctx, t, p, target, "job-a")
	require.NoError(t, fc.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}))

	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-a", "gpu-job-a"))
	assert.False(t, secretExists(ctx, t, fc, "team-a", "job-a"),
		"a Secret whose worker is gone has no consumer and must be reclaimed")
}

// TestCleanupScaleSetJob_RunnerlessCompletionSparesABusyWorker covers a job that ended
// before any runner took it while the runner minted for it started another job. The
// JobStarted recorded that, so the completion leaves the busy worker alone.
func TestCleanupScaleSetJob_RunnerlessCompletionSparesABusyWorker(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithStatusSubresource(&corev1.Pod{}).Build()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})

	mintedForX := provisionHolder(ctx, t, p, target, "job-x")
	setPhase(ctx, t, fc, mintedForX, corev1.PodRunning)

	require.NoError(t, p.MarkScaleSetJobStarted(ctx, target, ScaleSetJob{JobID: "job-d", RunnerName: "gpu-job-x"}))
	assert.Equal(t, "job-d", podAnnotations(ctx, t, fc, mintedForX)[AnnotationStartedJobID])

	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-x", ""))
	assert.NotContains(t, podAnnotations(ctx, t, fc, mintedForX), AnnotationJobCompletedAt,
		"a worker whose runner is running job-d must not be reaped for job-x")
	assert.True(t, secretExists(ctx, t, fc, "team-a", "job-x"))
}

// TestCleanupScaleSetJob_RunnerlessCompletionReclaimsAnIdleWorker is the Q420 arm the
// fix must keep: a job that ended before any runner took it leaves its minted worker
// idle at "Listening for Jobs", and that worker is still stamped and loses its Secret.
func TestCleanupScaleSetJob_RunnerlessCompletionReclaimsAnIdleWorker(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithStatusSubresource(&corev1.Pod{}).Build()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})

	mintedForX := provisionHolder(ctx, t, p, target, "job-x")
	setPhase(ctx, t, fc, mintedForX, corev1.PodRunning)

	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-x", ""))
	assert.Contains(t, podAnnotations(ctx, t, fc, mintedForX), AnnotationJobCompletedAt,
		"an idle worker whose job is gone must still get a reap deadline")
	assert.False(t, secretExists(ctx, t, fc, "team-a", "job-x"))
}

// TestMarkScaleSetJobStarted_ClearsAStampFromARunnerlessCompletion covers the other
// order: the runnerless completion stamped an idle worker, and then its runner took a
// job. The start must lift the deadline, or the reaper kills that job five minutes on.
func TestMarkScaleSetJobStarted_ClearsAStampFromARunnerlessCompletion(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithStatusSubresource(&corev1.Pod{}).Build()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})

	mintedForX := provisionHolder(ctx, t, p, target, "job-x")
	setPhase(ctx, t, fc, mintedForX, corev1.PodRunning)
	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-x", ""))
	require.Contains(t, podAnnotations(ctx, t, fc, mintedForX), AnnotationJobCompletedAt)

	require.NoError(t, p.MarkScaleSetJobStarted(ctx, target, ScaleSetJob{JobID: "job-d", RunnerName: "gpu-job-x"}))
	ann := podAnnotations(ctx, t, fc, mintedForX)
	assert.NotContains(t, ann, AnnotationJobCompletedAt,
		"a worker whose runner started a job must lose a deadline set while it was idle")
	assert.Equal(t, "job-d", ann[AnnotationStartedJobID])

	require.NoError(t, p.MarkScaleSetJobStarted(ctx, target, ScaleSetJob{JobID: "job-d", RunnerName: "gpu-no-such-runner"}),
		"a start naming a runner with no worker is not an error")
}

// laggingCache returns a client that writes to fc but serves every Get and List from a
// snapshot of fc's pods taken now, the way the manager's informer cache serves a read
// issued microseconds after a patch it has not yet observed.
func laggingCache(ctx context.Context, t *testing.T, fc client.WithWatch) client.Client {
	t.Helper()
	var pods corev1.PodList
	require.NoError(t, fc.List(ctx, &pods))
	objs := make([]client.Object, 0, len(pods.Items))
	for i := range pods.Items {
		objs = append(objs, pods.Items[i].DeepCopy())
	}
	snap := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).Build()
	return interceptor.NewClient(fc, interceptor.Funcs{
		Get: func(ctx context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			return snap.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, _ client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return snap.List(ctx, list, opts...)
		},
	})
}

// TestCleanupScaleSetJob_RunnerlessCompletionReadsPastALaggingCache covers the batch
// the listener handles start-first: the start's patch has not reached the informer
// cache when the runnerless completion reads the minted worker microseconds later.
// Pods are cached (only Secrets are not, main.go), so the decision must be taken on an
// uncached read or the busy worker is stamped and its job reaped.
func TestCleanupScaleSetJob_RunnerlessCompletionReadsPastALaggingCache(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithStatusSubresource(&corev1.Pod{}).Build()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})

	mintedForX := provisionHolder(ctx, t, p, target, "job-x")
	setPhase(ctx, t, fc, mintedForX, corev1.PodRunning)
	p.Client = laggingCache(ctx, t, fc)
	p.APIReader = fc

	require.NoError(t, p.MarkScaleSetJobStarted(ctx, target, ScaleSetJob{JobID: "job-d", RunnerName: "gpu-job-x"}))
	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-x", ""))

	assert.NotContains(t, podAnnotations(ctx, t, fc, mintedForX), AnnotationJobCompletedAt,
		"a cache that has not seen the start must not get the busy worker stamped")
	assert.True(t, secretExists(ctx, t, fc, "team-a", "job-x"))
}

// TestMarkScaleSetJobStarted_ClearsAStampTheCacheHasNotSeen is the mirror order: the
// runnerless completion stamped an idle worker, the cache has not caught up, and the
// runner then starts a job. A decision taken on the cached pod sees no stamp to clear.
func TestMarkScaleSetJobStarted_ClearsAStampTheCacheHasNotSeen(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithStatusSubresource(&corev1.Pod{}).Build()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})

	mintedForX := provisionHolder(ctx, t, p, target, "job-x")
	setPhase(ctx, t, fc, mintedForX, corev1.PodRunning)
	p.Client = laggingCache(ctx, t, fc)
	p.APIReader = fc

	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-x", ""))
	require.Contains(t, podAnnotations(ctx, t, fc, mintedForX), AnnotationJobCompletedAt)

	require.NoError(t, p.MarkScaleSetJobStarted(ctx, target, ScaleSetJob{JobID: "job-d", RunnerName: "gpu-job-x"}))
	assert.NotContains(t, podAnnotations(ctx, t, fc, mintedForX), AnnotationJobCompletedAt,
		"a start must lift a stamp the cache has not caught up with")
}
