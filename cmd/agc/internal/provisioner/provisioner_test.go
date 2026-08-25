package provisioner_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	agcnames "github.com/actions-gateway/github-actions-gateway/agc/names"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// errOnly adapts a JobHandlerFunc to return only its error, dropping the Q260
// pod-phase-proxy result these provisioning tests don't assert on (that result is
// covered directly in provisioner_result_test.go and the listener fan-out tests).
func errOnly(h listener.JobHandlerFunc) func(ctx context.Context, runServiceURL, planID string, payload []byte, jitConfig string) error {
	return func(ctx context.Context, runServiceURL, planID string, payload []byte, jitConfig string) error {
		_, err := h(ctx, runServiceURL, planID, payload, jitConfig)
		return err
	}
}

// newTestMetrics builds a Metrics with unregistered counters/histograms safe
// for per-test use (not added to the global Prometheus registry).
func newTestMetrics() *runnercore.Metrics {
	return &runnercore.Metrics{
		JobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "t_prov_job_duration_seconds",
		}, []string{"namespace", "runner_group"}),
		EvictionRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_prov_eviction_retries_total",
		}, []string{"namespace", "runner_group", "tier", "cause"}),
		EvictionRetriesExhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_prov_eviction_retries_exhausted_total",
		}, []string{"namespace", "runner_group", "tier", "cause"}),
		EvictionRerunFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_prov_eviction_rerun_failures_total",
		}, []string{"namespace", "runner_group", "tier", "cause", "reason"}),
		EvictionRecoveryIdentityUnknown: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_prov_eviction_recovery_identity_unknown_total",
		}, []string{"namespace", "runner_group", "cause"}),
		QuotaRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_prov_quota_retries_total",
		}, []string{"namespace", "runner_group"}),
		QuotaRetriesExhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_prov_quota_retries_exhausted_total",
		}, []string{"namespace", "runner_group"}),
	}
}

// quotaError returns the error the Kubernetes API server returns when a namespace
// ResourceQuota is exhausted — a 403 Forbidden with "exceeded quota" in the message.
func quotaError() error {
	return apierrors.NewForbidden(
		schema.GroupResource{Group: "", Resource: "pods"}, "pod",
		fmt.Errorf("exceeded quota: default-quota, requested: pods=1, used: pods=10, limited: pods=10"),
	)
}

// quotaPodCreateClient wraps a client.Client and returns a quota error for the
// first failCount Pod creates, then delegates to the underlying client.
type quotaPodCreateClient struct {
	client.Client
	failCount int
	calls     int
}

func (q *quotaPodCreateClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.Pod); ok {
		q.calls++
		if q.calls <= q.failCount {
			return quotaError()
		}
	}
	return q.Client.Create(ctx, obj, opts...)
}

// recordedEvent captures a single runnercore.EventRecorder.Event call.
type recordedEvent struct {
	namespace, name, eventtype, reason, action, note string
}

// fakeEventRecorder implements runnercore.EventRecorder, capturing events for
// assertions. Safe for concurrent use (the eviction path emits from goroutines).
type fakeEventRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (f *fakeEventRecorder) Event(namespace, name, eventtype, reason, action, note string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, recordedEvent{namespace, name, eventtype, reason, action, note})
}

// withReason returns the captured events whose Reason matches.
func (f *fakeEventRecorder) withReason(reason string) []recordedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedEvent
	for _, e := range f.events {
		if e.reason == reason {
			out = append(out, e)
		}
	}
	return out
}

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

func newProvisioner(c client.Client) *provisioner.Provisioner {
	p := provisioner.NewProvisioner(c, nil, nil)
	p.PollInterval = 1 * time.Millisecond
	p.EvictionRetryDelay = 1 * time.Millisecond
	return p
}

func newRG(name, ns string) *v1alpha1.RunnerGroup {
	return &v1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.RunnerGroupSpec{
			MaxListeners: 1,
			RunnerLabels: []string{"self-hosted"},
		},
	}
}

func stubPayload(runID int64) []byte {
	b, _ := json.Marshal(map[string]interface{}{"run_id": runID})
	return b
}

// stubPayloadFull returns a payload carrying a complete run identity in the shape a
// real AcquireJob response uses: the identity in the serialised `github` context,
// the job name as a variable. Deliberately NOT the variables-only shape the tests
// used to build — that shape is one GitHub never sends, and building it here is what
// let the classic tier ship unable to read a real payload's run identity (Q495).
// testdata/job_payload.json is the capture this mirrors.
func stubPayloadFull(owner, repo string, runID int64) []byte {
	b, _ := json.Marshal(map[string]interface{}{
		"variables": map[string]interface{}{
			"system.github.job": map[string]interface{}{"value": "build"},
		},
		"contextData": map[string]interface{}{
			"github": map[string]interface{}{
				"t": 2,
				"d": []map[string]interface{}{
					{"k": "run_id", "v": fmt.Sprintf("%d", runID)},
					{"k": "repository", "v": owner + "/" + repo},
					{"k": "workflow", "v": "CI"},
				},
			},
		},
	})
	return b
}

// evictPod sets the pod's phase to Failed with reason "Evicted".
func evictPod(ctx context.Context, t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &pod))
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Reason = "Evicted"
	require.NoError(t, c.Status().Update(ctx, &pod))
}

// preemptPod marks the named pod as kube-scheduler marks a preemption victim — the
// DisruptionTarget/PreemptionByScheduler condition — and moves it to phase in the same
// status write, which is what the classic path's single poll Get observes.
//
// The pod is left in the API rather than deleted because the scheduler's delete is
// graceful: the victim stays readable for its termination grace period, and it is that
// window the AGC observes it in.
func preemptPod(ctx context.Context, t *testing.T, c client.Client, ns, name string, phase corev1.PodPhase) {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &pod))
	pod.Status.Phase = phase
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:   corev1.DisruptionTarget,
		Status: corev1.ConditionTrue,
		Reason: corev1.PodReasonPreemptionByScheduler,
	})
	require.NoError(t, c.Status().Update(ctx, &pod))
}

// completePod transitions the named pod in the fake client to Succeeded.
func completePod(ctx context.Context, t *testing.T, c client.Client, ns, name string, phase corev1.PodPhase) {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &pod))
	pod.Status.Phase = phase
	require.NoError(t, c.Status().Update(ctx, &pod))
}

// findPod returns the first pod in the namespace (test helper for single-pod scenarios).
func findPod(ctx context.Context, t *testing.T, c client.Client, ns string) *corev1.Pod {
	t.Helper()
	var list corev1.PodList
	require.NoError(t, c.List(ctx, &list, client.InNamespace(ns)))
	if len(list.Items) == 0 {
		return nil
	}
	return &list.Items[0]
}

// waitForPodCreated polls until at least one pod exists in the namespace and
// returns it. Use this — not findPod — when the provisioner was just fired in a
// goroutine: provision() creates the Secret before the Pod, so an Eventually
// keyed on the Secret races with the Pod-Create call and findPod can still
// return nil. Eventually-on-Pod is strictly later than Secret-create, so a
// findSecret call after this helper is also race-free.
func waitForPodCreated(ctx context.Context, t *testing.T, c client.Client, ns string) *corev1.Pod {
	t.Helper()
	var pod *corev1.Pod
	require.Eventually(t, func() bool {
		pod = findPod(ctx, t, c, ns)
		return pod != nil
	}, 2*time.Second, 5*time.Millisecond, "pod must appear in namespace %s", ns)
	return pod
}

func findSecret(ctx context.Context, t *testing.T, c client.Client, ns, prefix string) *corev1.Secret {
	t.Helper()
	var list corev1.SecretList
	require.NoError(t, c.List(ctx, &list, client.InNamespace(ns)))
	for i := range list.Items {
		if len(list.Items[i].Name) >= len(prefix) && list.Items[i].Name[:len(prefix)] == prefix {
			return &list.Items[i]
		}
	}
	return nil
}

func TestProvisioner_CreatesPodAndSecret(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	m := newTestMetrics()
	p.Metrics = m

	rg := newRG("mygroup", "team-a")
	payload := stubPayload(42)

	// Run provisioner in background; complete pod immediately.
	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "http://run-svc", "plan-abc-123", payload, "")
	}()

	// Wait for the pod to appear, then complete it.
	pod := waitForPodCreated(ctx, t, fc, "team-a")
	assert.Equal(t, agcnames.ControllerName, pod.Labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, "mygroup", pod.Labels["actions-gateway/runner-group"])

	secret := findSecret(ctx, t, fc, "team-a", "job-")
	require.NotNil(t, secret)
	assert.Equal(t, payload, secret.Data["payload"])
	assert.Equal(t, []byte("plan-abc-123"), secret.Data["plan-id"])

	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

// TestProvisioner_ForwardsJITConfigIntoSecret verifies that the agent's
// encoded JIT config blob is copied verbatim into the worker Secret under the
// "jitconfig" key. The wrapper relies on this Secret data key to materialize
// .runner / .credentials / .credentials_rsaparams (Queue item 5a).
func TestProvisioner_ForwardsJITConfigIntoSecret(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")
	const jitBlob = "aGVsbG8tand0LWNvbmZpZw=="

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-jit", stubPayload(7), jitBlob)
	}()

	// Wait for the Pod (created strictly after the Secret) so the findSecret
	// below is race-free. See waitForPodCreated for context.
	pod := waitForPodCreated(ctx, t, fc, "team-a")

	secret := findSecret(ctx, t, fc, "team-a", "job-")
	require.NotNil(t, secret)
	assert.Equal(t, []byte(jitBlob), secret.Data["jitconfig"],
		"worker Secret must carry the JIT blob under the 'jitconfig' key")

	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

// fakeTarget is a minimal provisioner.Target for exercising ProvisionScaleSetWorker
// directly, without the controller-owned RunnerGroup/RunnerSet target adapters.
type fakeTarget struct {
	key    client.ObjectKey
	labels map[string]string
	spec   *provisioner.ResolvedSpec
}

func (f *fakeTarget) Key() client.ObjectKey { return f.key }
func (f *fakeTarget) OwnerRef() metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: "actions-gateway.com/v2alpha1", Kind: "RunnerSet", Name: f.key.Name, UID: types.UID("uid-" + f.key.Name)}
}
func (f *fakeTarget) PodOwnerLabels() map[string]string                       { return f.labels }
func (f *fakeTarget) Ceiling(context.Context) (int32, bool)                   { return 0, false }
func (f *fakeTarget) ScaleUpLimit(context.Context) *provisioner.ScaleUpConfig { return nil }
func (f *fakeTarget) QuotaExhausted(context.Context) (bool, string)           { return false, "" }
func (f *fakeTarget) QuotaCapacity(context.Context, int32) (int32, bool)      { return 0, false }
func (f *fakeTarget) CapacityDeclined(context.Context) (bool, string)         { return false, "" }
func (f *fakeTarget) DeclinedCapacity(context.Context, int32) (int32, bool) {
	return 0, false
}
func (f *fakeTarget) RecordEvent(_, _, _, _ string) {}
func (f *fakeTarget) Resolve(context.Context) (*provisioner.ResolvedSpec, error) {
	return f.spec, nil
}

// TestProvisioner_ScaleSetWorker_StagesJITAndSetsMode covers the Q264 scale-set
// provision path: a JIT-config Secret (the blob, no acquired payload) and a worker pod
// switched into scale-set mode (WORKER_MODE=scaleset → run.sh --jitconfig). It is
// fire-and-forget (no wait for completion) and idempotent per jobID.
func TestProvisioner_ScaleSetWorker_StagesJITAndSetsMode(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	target := &fakeTarget{
		key:    client.ObjectKey{Namespace: "team-a", Name: "gpu"},
		labels: map[string]string{"actions-gateway.com/runner-set": "gpu"},
		spec:   &provisioner.ResolvedSpec{WorkerImage: "runner:test"},
	}
	const jit = "eyJydW5uZXIiOnt9fQ=="

	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, provisioner.ScaleSetJob{JobID: "job-uuid-1", JITConfig: jit}))

	// The Secret carries the JIT blob and NO acquired payload.
	secret := findSecret(ctx, t, fc, "team-a", "job-ss-")
	require.NotNil(t, secret, "a scale-set job Secret must be staged")
	assert.Equal(t, []byte(jit), secret.Data["jitconfig"], "the JIT blob is staged under 'jitconfig'")
	_, hasPayload := secret.Data["payload"]
	assert.False(t, hasPayload, "a scale-set worker Secret carries no acquired payload")

	// The pod runs in scale-set mode and mounts its Secret.
	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)
	runner := runnerOf(t, pod)
	var mode string
	for _, e := range runner.Env {
		if e.Name == "WORKER_MODE" {
			mode = e.Value
		}
	}
	assert.Equal(t, "scaleset", mode, "the worker must run in scale-set mode (run.sh --jitconfig)")

	// Idempotent: replaying the same job is a no-op (deterministic names → AlreadyExists).
	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, provisioner.ScaleSetJob{JobID: "job-uuid-1", JITConfig: jit}))

	// A missing JIT config is rejected before any object is created.
	require.Error(t, p.ProvisionScaleSetWorker(ctx, target, provisioner.ScaleSetJob{JobID: "job-uuid-2", JITConfig: ""}))
}

// TestProvisioner_ScaleSetWorker_StampsRunnerName pins the pod as the durable record of
// its runner registration (Q550). The listener pre-registers the runner before the pod
// exists and then forgets the name; the reaper has to deregister that record long
// afterwards, so the name has to survive on the pod — as the run identity (Q417) and the
// reap deadline (Q420) already do. A job with no name stamps no annotation, keeping a
// pre-upgrade or classic worker exactly as it was.
func TestProvisioner_ScaleSetWorker_StampsRunnerName(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	target := &fakeTarget{
		key:    client.ObjectKey{Namespace: "team-a", Name: "gpu"},
		labels: map[string]string{"actions-gateway.com/runner-set": "gpu"},
		spec:   &provisioner.ResolvedSpec{WorkerImage: "runner:test"},
	}
	const jit = "eyJydW5uZXIiOnt9fQ=="

	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, provisioner.ScaleSetJob{
		JobID: "job-uuid-1", JITConfig: jit, RunnerName: "gpu-job-uuid-1",
	}))
	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)
	assert.Equal(t, "gpu-job-uuid-1", pod.Annotations[provisioner.AnnotationRunnerName],
		"the reaper deregisters the record by the name stamped here")

	// A fresh client, so findPod returns the unnamed job's pod rather than the first.
	fc2 := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	require.NoError(t, newProvisioner(fc2).ProvisionScaleSetWorker(ctx, target,
		provisioner.ScaleSetJob{JobID: "job-uuid-2", JITConfig: jit}))
	unnamed := findPod(ctx, t, fc2, "team-a")
	require.NotNil(t, unnamed)
	assert.NotContains(t, unnamed.Annotations, provisioner.AnnotationRunnerName,
		"a job with no runner name must stamp no annotation rather than an empty one")
}

// TestProvisioner_OmitsJITKeyWhenEmpty pins the contract that an empty
// jitConfig string does not create a Secret entry. Stub-registrar agents
// (used by integration tests against fakegithub) produce no JIT blob, and
// the wrapper treats a missing key as a no-op materialization step.
func TestProvisioner_OmitsJITKeyWhenEmpty(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-nojit", stubPayload(8), "")
	}()

	// Wait for the Pod (created strictly after the Secret) so the findSecret
	// below is race-free. See waitForPodCreated for context.
	pod := waitForPodCreated(ctx, t, fc, "team-a")

	secret := findSecret(ctx, t, fc, "team-a", "job-")
	require.NotNil(t, secret)
	_, present := secret.Data["jitconfig"]
	assert.False(t, present, "jitconfig key must be absent when no blob was provided")

	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

func TestProvisioner_DeletesSecretOnCompletion(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-del", stubPayload(1), "")
	}()

	pod := waitForPodCreated(ctx, t, fc, "team-a")
	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)

	// Secret must be gone.
	secret := findSecret(ctx, t, fc, "team-a", "job-")
	assert.Nil(t, secret, "job Secret should be deleted after pod completion")
}

func TestProvisioner_MaxWorkersHolds(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	// Pre-populate 3 running pods for the group.
	maxW := int32(3)
	rg := newRG("mygroup", "team-a")
	rg.Spec.MaxWorkers = &maxW

	existingPods := make([]client.Object, 3)
	for i := 0; i < 3; i++ {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("existing-%d", i),
				Namespace: "team-a",
				Labels:    map[string]string{"actions-gateway/runner-group": "mygroup"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		existingPods[i] = pod
	}
	fc := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithStatusSubresource(&corev1.Pod{}).
		WithObjects(existingPods...).
		WithStatusSubresource(existingPods...).
		Build()
	// Set pod status to Running on existing pods.
	for i := 0; i < 3; i++ {
		var pod corev1.Pod
		_ = fc.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: fmt.Sprintf("existing-%d", i)}, &pod)
		pod.Status.Phase = corev1.PodRunning
		_ = fc.Status().Update(ctx, &pod)
	}

	p := newProvisioner(fc)
	err := errOnly(p.HandlerFor(rg))(ctx, "", "plan-hold", stubPayload(1), "")
	assert.ErrorContains(t, err, "ceiling")

	// No new pod created, but the existing 3 are still there.
	var list corev1.PodList
	require.NoError(t, fc.List(ctx, &list, client.InNamespace("team-a")))
	assert.Len(t, list.Items, 3, "no new pod should be created when ceiling is reached")

	// Secret must be cleaned up.
	secret := findSecret(ctx, t, fc, "team-a", "job-")
	assert.Nil(t, secret, "Secret should be cleaned up when pod is held")
}

func TestProvisioner_PriorityTiersAssignment(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	rg := newRG("mygroup", "team-a")
	rg.Spec.PriorityTiers = []v1alpha1.PriorityTier{
		{PriorityClassName: "runner-critical", Threshold: 5},
		{PriorityClassName: "runner-standard", Threshold: 10},
	}

	// 3 active pods — below first tier threshold of 5.
	existingPods := make([]client.Object, 3)
	for i := 0; i < 3; i++ {
		existingPods[i] = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("existing-%d", i),
				Namespace: "team-a",
				Labels:    map[string]string{"actions-gateway/runner-group": "mygroup"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}
	fc := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithStatusSubresource(&corev1.Pod{}).
		WithObjects(existingPods...).
		WithStatusSubresource(existingPods...).
		Build()
	for i := 0; i < 3; i++ {
		var pod corev1.Pod
		_ = fc.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: fmt.Sprintf("existing-%d", i)}, &pod)
		pod.Status.Phase = corev1.PodRunning
		_ = fc.Status().Update(ctx, &pod)
	}

	p := newProvisioner(fc)

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-tier", stubPayload(1), "")
	}()

	// Wait specifically for a pod that is NOT one of the pre-existing ones.
	var newPod *corev1.Pod
	require.Eventually(t, func() bool {
		var list corev1.PodList
		if err := fc.List(ctx, &list, client.InNamespace("team-a")); err != nil {
			return false
		}
		for i := range list.Items {
			name := list.Items[i].Name
			if name != "existing-0" && name != "existing-1" && name != "existing-2" {
				newPod = &list.Items[i]
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond)
	require.NotNil(t, newPod, "a new pod should have been created")
	assert.Equal(t, "runner-critical", newPod.Spec.PriorityClassName)

	completePod(ctx, t, fc, "team-a", newPod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

func TestProvisioner_PriorityTiersCeiling(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	rg := newRG("mygroup", "team-a")
	rg.Spec.PriorityTiers = []v1alpha1.PriorityTier{
		{PriorityClassName: "runner-critical", Threshold: 5},
		{PriorityClassName: "runner-standard", Threshold: 10},
	}

	// 10 running pods — at the last tier ceiling.
	existingPods := make([]client.Object, 10)
	for i := 0; i < 10; i++ {
		existingPods[i] = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("existing-%d", i),
				Namespace: "team-a",
				Labels:    map[string]string{"actions-gateway/runner-group": "mygroup"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}
	fc := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithStatusSubresource(&corev1.Pod{}).
		WithObjects(existingPods...).
		WithStatusSubresource(existingPods...).
		Build()
	for i := 0; i < 10; i++ {
		var pod corev1.Pod
		_ = fc.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: fmt.Sprintf("existing-%d", i)}, &pod)
		pod.Status.Phase = corev1.PodRunning
		_ = fc.Status().Update(ctx, &pod)
	}

	p := newProvisioner(fc)
	err := errOnly(p.HandlerFor(rg))(ctx, "", "plan-ceil", stubPayload(1), "")
	assert.ErrorContains(t, err, "ceiling")

	var list corev1.PodList
	require.NoError(t, fc.List(ctx, &list, client.InNamespace("team-a")))
	assert.Len(t, list.Items, 10, "no new pod when at ceiling")
}

func TestProvisioner_WorkerImageFallback(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	p.DefaultWorkerImage = "my-custom-image:latest"

	rg := newRG("mygroup", "team-a")
	// WorkerImage intentionally empty.

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-img", stubPayload(1), "")
	}()

	pod := waitForPodCreated(ctx, t, fc, "team-a")
	runnerContainer := runnerOf(t, pod)
	assert.Equal(t, "my-custom-image:latest", runnerContainer.Image)

	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

// TestProvisioner_NamedImagelessRunnerGapFilled covers Q233: a tenant podTemplate
// may name the "runner" container but omit its image. The provisioner must fill the
// resolved worker image (otherwise the API server rejects the Pod with
// spec.containers[].image: Required value) without overriding a tenant-set image.
func TestProvisioner_NamedImagelessRunnerGapFilled(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	p.DefaultWorkerImage = "my-custom-image:latest"

	rg := newRG("mygroup", "team-a")
	// PodTemplate names the runner container but leaves its image empty.
	rg.Spec.PodTemplate.Spec.Containers = []corev1.Container{{Name: "runner"}}

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-gapfill", stubPayload(1), "")
	}()

	pod := waitForPodCreated(ctx, t, fc, "team-a")
	runner := runnerOf(t, pod)
	assert.Equal(t, "my-custom-image:latest", runner.Image, "image-less runner must be gap-filled with the resolved worker image")

	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

// TestProvisioner_NamedRunnerImageNotOverridden asserts the Q233 gap-fill never
// clobbers an image the tenant set explicitly on the runner container.
func TestProvisioner_NamedRunnerImageNotOverridden(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	p.DefaultWorkerImage = "my-custom-image:latest"

	rg := newRG("mygroup", "team-a")
	// PodTemplate names the runner container with an explicit image.
	rg.Spec.PodTemplate.Spec.Containers = []corev1.Container{{Name: "runner", Image: "tenant-image:pinned"}}

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-nooverride", stubPayload(1), "")
	}()

	pod := waitForPodCreated(ctx, t, fc, "team-a")
	runner := runnerOf(t, pod)
	assert.Equal(t, "tenant-image:pinned", runner.Image, "an explicitly-set runner image must not be overridden")

	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

func TestProvisioner_ReservedFieldsOverwritten(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	p.WorkerSA = "agc-worker"

	rg := newRG("mygroup", "team-a")
	// Tenant tries to set serviceAccountName — should be overwritten.
	rg.Spec.PodTemplate.Spec.ServiceAccountName = "tenant-sa"
	hostTrue := true
	rg.Spec.PodTemplate.Spec.HostPID = hostTrue
	rg.Spec.PodTemplate.Spec.HostNetwork = hostTrue

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-reserved", stubPayload(1), "")
	}()

	pod := waitForPodCreated(ctx, t, fc, "team-a")
	assert.Equal(t, "agc-worker", pod.Spec.ServiceAccountName)
	assert.False(t, pod.Spec.HostPID)
	assert.False(t, pod.Spec.HostNetwork)
	assert.False(t, pod.Spec.HostIPC)
	autoMount := pod.Spec.AutomountServiceAccountToken
	assert.NotNil(t, autoMount)
	assert.False(t, *autoMount)
	assert.Equal(t, corev1.RestartPolicyNever, pod.Spec.RestartPolicy)

	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

// failPodCreateClient wraps a client.Client and returns an error for Pod creates,
// simulating a Kubernetes admission rejection without depending on pod naming internals.
type failPodCreateClient struct {
	client.Client
}

func (f failPodCreateClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.Pod); ok {
		return fmt.Errorf("injected pod create failure")
	}
	return f.Client.Create(ctx, obj, opts...)
}

func TestProvisioner_SecretCleanupOnPodCreateFailure(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	// Wrap to fail all Pod creates; provisioner should still clean up the Secret.
	p := newProvisioner(failPodCreateClient{fc})

	rg := newRG("mygroup", "team-a")

	err := errOnly(p.HandlerFor(rg))(ctx, "", "plan-conflict", stubPayload(1), "")
	assert.Error(t, err)

	// Secret must be cleaned up even though pod creation failed.
	var secretList corev1.SecretList
	require.NoError(t, fc.List(ctx, &secretList, client.InNamespace("team-a")))
	for _, s := range secretList.Items {
		assert.NotContains(t, s.Name, "job-", "all job Secrets should be cleaned up on pod creation failure")
	}
}

func TestProvisioner_ContextCancellation(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx, cancel := context.WithCancel(context.Background())
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-cancel", stubPayload(1), "")
	}()

	// Wait for pod to be created, then cancel.
	waitForPodCreated(ctx, t, fc, "team-a")

	cancel()

	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("provisioner did not return after context cancellation")
	}
}

func TestProvisioner_PodNameDNSSafe(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("My/Group", "team-a")

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "PLAN/ID:with:COLONS/and/SLASHES", stubPayload(1), "")
	}()

	pod := waitForPodCreated(ctx, t, fc, "team-a")

	// Pod name must be a valid DNS label: lowercase, alphanumeric+hyphens, ≤63 chars.
	assert.LessOrEqual(t, len(pod.Name), 63)
	for _, c := range pod.Name {
		assert.True(t, (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-',
			"pod name %q contains invalid char %q", pod.Name, c)
	}

	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

func TestProvisioner_SecretMountedInPod(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-mount", stubPayload(1), "")
	}()

	pod := waitForPodCreated(ctx, t, fc, "team-a")

	// Assert Secret volume exists.
	var secretVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Secret != nil {
			secretVol = &pod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, secretVol, "pod should have a Secret volume")

	// Assert runner container has the volume mounted and env var set.
	runner := runnerOf(t, pod)

	var hasMount bool
	for _, m := range runner.VolumeMounts {
		if m.MountPath == "/run/secrets/job-payload" {
			hasMount = true
			break
		}
	}
	assert.True(t, hasMount, "runner container must mount Secret at /run/secrets/job-payload")

	var hasEnv bool
	for _, e := range runner.Env {
		if e.Name == "PAYLOAD_SECRET_PATH" && e.Value == "/run/secrets/job-payload" {
			hasEnv = true
			break
		}
	}
	assert.True(t, hasEnv, "runner container must have PAYLOAD_SECRET_PATH env var")

	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

func TestProvisioner_EvictionAutoRetry(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()

	// Capture the rerun API request.
	rerunCalled := make(chan string, 1) // receives the request URL path
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rerunCalled <- r.URL.Path
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p := newProvisioner(fc)
	m := newTestMetrics()
	p.Metrics = m
	p.TokenFunc = func(context.Context) (string, error) { return "test-token", nil }
	p.GitHubAPIURL = srv.URL
	p.HTTPClient = srv.Client()

	rg := newRG("mygroup", "team-a")
	payload := stubPayloadFull("myorg", "myrepo", 99)

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-evict", payload, "")
	}()

	pod := waitForPodCreated(ctx, t, fc, "team-a")

	// Job annotations must be stamped on the pod from the payload.
	assert.Equal(t, "99", pod.Annotations["actions-gateway.com/run-id"])
	assert.Equal(t, "myorg/myrepo", pod.Annotations["actions-gateway.com/repository"])
	assert.Equal(t, "build", pod.Annotations["actions-gateway.com/job-name"])
	assert.Equal(t, "CI", pod.Annotations["actions-gateway.com/workflow"])

	evictPod(ctx, t, fc, "team-a", pod.Name)

	require.NoError(t, <-done)

	select {
	case path := <-rerunCalled:
		assert.Equal(t, "/repos/myorg/myrepo/actions/runs/99/rerun-failed-jobs", path)
	case <-time.After(2 * time.Second):
		t.Fatal("rerun API was not called within timeout")
	}

	// H1: EvictionRetries counter must be incremented once.
	assert.Equal(t, float64(1), testutil.ToFloat64(m.EvictionRetries.WithLabelValues("team-a", "mygroup", "classic", "eviction")))
}

// TestProvisioner_PreemptionAutoRetry is the classic half of Q497. A worker displaced by
// a higher-priority priorityTiers tier is deleted by kube-scheduler, not failed by the
// kubelet, so it never carries Evicted and reached no recovery at all before this — the
// displaced run needed a manual re-run, which is exactly what the oversubscription claim
// says it should not.
//
// The victim here lands in Succeeded deliberately. Q423 measured that the terminal phase
// on this path is the interrupted container's own exit status, so a preempted worker can
// look, by phase alone, like a job that finished cleanly. Asserting the re-run fires
// anyway is asserting that the DisruptionTarget condition — not the phase — is what
// drives the decision.
func TestProvisioner_PreemptionAutoRetry(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()

	rerunCalled := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rerunCalled <- r.URL.Path
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p := newProvisioner(fc)
	m := newTestMetrics()
	p.Metrics = m
	p.TokenFunc = func(context.Context) (string, error) { return "test-token", nil }
	p.GitHubAPIURL = srv.URL
	p.HTTPClient = srv.Client()

	rg := newRG("mygroup", "team-a")
	payload := stubPayloadFull("myorg", "myrepo", 77)

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-preempt", payload, "")
	}()

	pod := waitForPodCreated(ctx, t, fc, "team-a")
	preemptPod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)

	require.NoError(t, <-done)

	select {
	case path := <-rerunCalled:
		assert.Equal(t, "/repos/myorg/myrepo/actions/runs/77/rerun-failed-jobs", path)
	case <-time.After(2 * time.Second):
		t.Fatal("a preempted worker's run was not re-run within timeout")
	}

	// Attributed to preemption, not to node pressure: an operator diagnosing a climbing
	// eviction rate goes looking at node memory and disk, which would be the wrong hunt.
	assert.Equal(t, float64(1), testutil.ToFloat64(m.EvictionRetries.WithLabelValues("team-a", "mygroup", "classic", "preemption")))
	assert.Equal(t, float64(0), testutil.ToFloat64(m.EvictionRetries.WithLabelValues("team-a", "mygroup", "classic", "eviction")))
}

// TestProvisioner_OrdinaryCompletionIsNotRetried is the negative that bounds the change
// above: a worker that simply finished — no disruption marker of any kind — must not be
// re-run, whatever its phase. Without this, "recover on preemption" could silently
// become "re-run everything that ends".
func TestProvisioner_OrdinaryCompletionIsNotRetried(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()

	var rerunCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rerunCount.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p := newProvisioner(fc)
	p.TokenFunc = func(context.Context) (string, error) { return "test-token", nil }
	p.GitHubAPIURL = srv.URL
	p.HTTPClient = srv.Client()

	rg := newRG("mygroup", "team-a")

	// A plain failure is the sharper case than a plain success: it is what a genuinely
	// broken job produces, and it shares a phase with a kubelet eviction.
	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-plain", stubPayloadFull("myorg", "myrepo", 78), "")
	}()
	pod := waitForPodCreated(ctx, t, fc, "team-a")
	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodFailed)
	require.NoError(t, <-done)

	assert.Equal(t, int64(0), rerunCount.Load(),
		"a job that failed on its own must not be re-run; only a disruption qualifies")
}

// TestProvisioner_EvictionRetryBudgetExhausted verifies that a second eviction
// for the same run_id on the same Provisioner instance does not trigger another
// rerun API call once MaxEvictionRetries is reached.
func TestProvisioner_EvictionRetryBudgetExhausted(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	var rerunCount int
	rerunCalls := make(chan struct{}, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rerunCount++
		rerunCalls <- struct{}{}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()

	// MaxEvictionRetries=1: first eviction retries, second exhausts the budget.
	p := newProvisioner(fc)
	m := newTestMetrics()
	p.Metrics = m
	rec := &fakeEventRecorder{}
	p.Events = rec
	p.MaxEvictionRetries = 1
	p.TokenFunc = func(context.Context) (string, error) { return "tok", nil }
	p.GitHubAPIURL = srv.URL
	p.HTTPClient = srv.Client()

	rg := newRG("mygroup", "ns")
	payload := stubPayloadFull("org", "repo", 7)

	// Helper: run one provision cycle with unique planID; returns after pod eviction.
	// Uses phase-based lookup to find the current active pod (ignoring prior-cycle
	// evicted pods that remain in the fake client with Phase=Failed).
	runCycle := func(planID string) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- errOnly(p.HandlerFor(rg))(ctx, "", planID, payload, "") }()

		var podToEvict *corev1.Pod
		require.Eventually(t, func() bool {
			var list corev1.PodList
			if err := fc.List(ctx, &list, client.InNamespace("ns")); err != nil {
				return false
			}
			for i := range list.Items {
				if list.Items[i].Status.Phase != corev1.PodFailed {
					podToEvict = &list.Items[i]
					return true
				}
			}
			return false
		}, 2*time.Second, 5*time.Millisecond, "active pod should appear for planID %s", planID)
		evictPod(ctx, t, fc, "ns", podToEvict.Name)
		require.NoError(t, <-done)
	}

	// First eviction (count 0 → 1): rerun API must be called.
	runCycle("plan-evict-1")
	select {
	case <-rerunCalls:
	case <-time.After(2 * time.Second):
		t.Fatal("expected rerun API call on first eviction")
	}

	// Second eviction (count 1 >= MaxEvictionRetries=1): budget exhausted, no API call.
	// H5: the budget check runs synchronously inside provision (only the GitHub calls
	// are detached — Q503), so these assertions are race-free — no sleep needed.
	runCycle("plan-evict-2")
	assert.Equal(t, float64(1), testutil.ToFloat64(m.EvictionRetriesExhausted.WithLabelValues("ns", "mygroup", "classic", "eviction")))
	assert.Equal(t, float64(1), testutil.ToFloat64(m.EvictionRetries.WithLabelValues("ns", "mygroup", "classic", "eviction")))
	assert.Equal(t, 1, rerunCount, "rerun API should be called exactly once")

	// Budget exhaustion records a Warning Event on the owner so the operator sees a
	// manual re-run is required, not just the metric (Q170).
	evs := rec.withReason("EvictionRetriesExhausted")
	require.Len(t, evs, 1)
	assert.Equal(t, corev1.EventTypeWarning, evs[0].eventtype)
	assert.Equal(t, "ns", evs[0].namespace)
	assert.Equal(t, "mygroup", evs[0].name)
	assert.Contains(t, evs[0].note, "manual re-run")
}

// TestProvisioner_EvictionRerunAPI5xx verifies that a 5xx response from the
// rerun API is non-fatal: provision still returns nil and the EvictionRetries
// counter is incremented.
func TestProvisioner_EvictionRerunAPI5xx(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	rerunPaths := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rerunPaths <- r.URL.Path
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	m := newTestMetrics()
	p.Metrics = m
	p.TokenFunc = func(context.Context) (string, error) { return "tok-5xx", nil }
	p.GitHubAPIURL = srv.URL
	p.HTTPClient = srv.Client()

	rg := newRG("mygroup", "team-a")
	payload := stubPayloadFull("org5xx", "repo5xx", 55)

	done := make(chan error, 1)
	go func() { done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-5xx", payload, "") }()

	pod := waitForPodCreated(ctx, t, fc, "team-a")
	evictPod(ctx, t, fc, "team-a", pod.Name)

	// H2: 5xx response is non-fatal — provision must return nil.
	require.NoError(t, <-done)

	// Rerun was attempted.
	select {
	case path := <-rerunPaths:
		assert.Equal(t, "/repos/org5xx/repo5xx/actions/runs/55/rerun-failed-jobs", path)
	case <-time.After(2 * time.Second):
		t.Fatal("rerun API was not called within timeout")
	}

	// EvictionRetries counter incremented even when the API returns 5xx.
	assert.Equal(t, float64(1), testutil.ToFloat64(m.EvictionRetries.WithLabelValues("team-a", "mygroup", "classic", "eviction")))

	// The re-run never landed, so the failure counter must say so (Q503). Eventually:
	// the GitHub calls run detached from provision, so the failure is recorded a
	// moment after the stub's response is read.
	assert.Eventually(t, func() bool {
		return testutil.ToFloat64(m.EvictionRerunFailures.WithLabelValues("team-a", "mygroup", "classic", "eviction", "api_error")) == 1
	}, 2*time.Second, 5*time.Millisecond, "a 5xx re-run must be surfaced as api_error")
}

// TestProvisioner_PriorityTiersSecondTier verifies that the second priority tier
// is assigned when active pods exceed the first tier's threshold.
func TestProvisioner_PriorityTiersSecondTier(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	rg := newRG("mygroup", "team-a")
	rg.Spec.PriorityTiers = []v1alpha1.PriorityTier{
		{PriorityClassName: "runner-critical", Threshold: 5},
		{PriorityClassName: "runner-standard", Threshold: 10},
	}

	// 6 active pods — above first threshold (5) but below second (10).
	existingPods := make([]client.Object, 6)
	for i := 0; i < 6; i++ {
		existingPods[i] = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("existing-%d", i),
				Namespace: "team-a",
				Labels:    map[string]string{"actions-gateway/runner-group": "mygroup"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}
	fc := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithStatusSubresource(&corev1.Pod{}).
		WithObjects(existingPods...).
		WithStatusSubresource(existingPods...).
		Build()
	for i := 0; i < 6; i++ {
		var pod corev1.Pod
		_ = fc.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: fmt.Sprintf("existing-%d", i)}, &pod)
		pod.Status.Phase = corev1.PodRunning
		_ = fc.Status().Update(ctx, &pod)
	}

	p := newProvisioner(fc)

	done := make(chan error, 1)
	go func() { done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-tier2", stubPayload(1), "") }()

	require.Eventually(t, func() bool {
		var list corev1.PodList
		_ = fc.List(ctx, &list, client.InNamespace("team-a"))
		return len(list.Items) > 6
	}, 2*time.Second, 5*time.Millisecond)

	var list corev1.PodList
	require.NoError(t, fc.List(ctx, &list, client.InNamespace("team-a")))
	var newPod *corev1.Pod
	existingNames := map[string]bool{}
	for i := 0; i < 6; i++ {
		existingNames[fmt.Sprintf("existing-%d", i)] = true
	}
	for i := range list.Items {
		if !existingNames[list.Items[i].Name] {
			newPod = &list.Items[i]
			break
		}
	}
	require.NotNil(t, newPod, "a new pod should have been created")
	// H4: 6 active pods is above threshold 5, so second tier "runner-standard" applies.
	assert.Equal(t, "runner-standard", newPod.Spec.PriorityClassName)

	completePod(ctx, t, fc, "team-a", newPod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

// TestProvisioner_PriorityTiersBoundary pins the comparison semantics: exactly
// activePods == threshold falls through to the next tier (not the current one).
func TestProvisioner_PriorityTiersBoundary(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	rg := newRG("mygroup", "team-a")
	rg.Spec.PriorityTiers = []v1alpha1.PriorityTier{
		{PriorityClassName: "runner-critical", Threshold: 5},
		{PriorityClassName: "runner-standard", Threshold: 10},
	}

	// Exactly 5 active pods — equal to first tier threshold, should fall to second tier.
	existingPods := make([]client.Object, 5)
	for i := 0; i < 5; i++ {
		existingPods[i] = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("boundary-%d", i),
				Namespace: "team-a",
				Labels:    map[string]string{"actions-gateway/runner-group": "mygroup"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}
	fc := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithStatusSubresource(&corev1.Pod{}).
		WithObjects(existingPods...).
		WithStatusSubresource(existingPods...).
		Build()
	for i := 0; i < 5; i++ {
		var pod corev1.Pod
		_ = fc.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: fmt.Sprintf("boundary-%d", i)}, &pod)
		pod.Status.Phase = corev1.PodRunning
		_ = fc.Status().Update(ctx, &pod)
	}

	p := newProvisioner(fc)

	done := make(chan error, 1)
	go func() { done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-boundary", stubPayload(1), "") }()

	require.Eventually(t, func() bool {
		var list corev1.PodList
		_ = fc.List(ctx, &list, client.InNamespace("team-a"))
		return len(list.Items) > 5
	}, 2*time.Second, 5*time.Millisecond)

	var list corev1.PodList
	require.NoError(t, fc.List(ctx, &list, client.InNamespace("team-a")))
	var newPod *corev1.Pod
	for i := range list.Items {
		name := list.Items[i].Name
		if len(name) < 8 || name[:8] != "boundary" {
			newPod = &list.Items[i]
			break
		}
	}
	require.NotNil(t, newPod, "a new pod should have been created at boundary")
	// H4: activePods == threshold (5 == 5) uses strict <, so it falls to second tier.
	assert.Equal(t, "runner-standard", newPod.Spec.PriorityClassName)

	completePod(ctx, t, fc, "team-a", newPod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

// TestProvisioner_PendingPodsCountTowardCeiling verifies that Pending pods are
// counted against the MaxWorkers ceiling, preventing over-provisioning.
func TestProvisioner_PendingPodsCountTowardCeiling(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	maxW := int32(3)
	rg := newRG("mygroup", "team-a")
	rg.Spec.MaxWorkers = &maxW

	// 2 Running + 1 Pending = 3 active pods, which equals MaxWorkers.
	existingPods := []client.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "running-0", Namespace: "team-a",
				Labels: map[string]string{"actions-gateway/runner-group": "mygroup"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "running-1", Namespace: "team-a",
				Labels: map[string]string{"actions-gateway/runner-group": "mygroup"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pending-0", Namespace: "team-a",
				Labels: map[string]string{"actions-gateway/runner-group": "mygroup"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodPending},
		},
	}
	fc := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithStatusSubresource(&corev1.Pod{}).
		WithObjects(existingPods...).
		WithStatusSubresource(existingPods...).
		Build()
	phases := []corev1.PodPhase{corev1.PodRunning, corev1.PodRunning, corev1.PodPending}
	names := []string{"running-0", "running-1", "pending-0"}
	for i, name := range names {
		var pod corev1.Pod
		_ = fc.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: name}, &pod)
		pod.Status.Phase = phases[i]
		_ = fc.Status().Update(ctx, &pod)
	}

	p := newProvisioner(fc)
	err := errOnly(p.HandlerFor(rg))(ctx, "", "plan-pending-ceil", stubPayload(1), "")
	// M3: ceiling is enforced because Pending pods count as active.
	assert.ErrorContains(t, err, "ceiling")

	var podList corev1.PodList
	require.NoError(t, fc.List(ctx, &podList, client.InNamespace("team-a")))
	assert.Len(t, podList.Items, 3, "no new pod should be created when Pending pods fill the ceiling")
}

// TestProvisioner_PodDeletedExternallySucceeds verifies that an operator
// manually deleting the pod is not read as an eviction: provision returns nil
// and the rerun API is not called. The pod here never ran a container, so the
// deletion is a removed-before-start shape: the job is abandoned and its run
// force-cancelled — the honest fast ending (Q683) — not re-run.
func TestProvisioner_PodDeletedExternallySucceeds(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	// Stub server recording every REST call so rerun and force-cancel are
	// distinguishable.
	apiCalls := make(chan string, 5)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls <- r.URL.Path
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	p.TokenFunc = func(context.Context) (string, error) { return "tok-del", nil }
	p.GitHubAPIURL = srv.URL
	p.HTTPClient = srv.Client()

	rg := newRG("mygroup", "team-a")
	// Use a payload with run_id so eviction handling would fire if triggered.
	payload := stubPayloadFull("org-del", "repo-del", 77)

	done := make(chan error, 1)
	go func() { done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-extdel", payload, "") }()

	pod := waitForPodCreated(ctx, t, fc, "team-a")

	// Simulate operator deleting the pod externally.
	require.NoError(t, fc.Delete(ctx, pod))

	// M4: provision must return nil (external deletion is treated as success).
	require.NoError(t, <-done)

	// The rerun API must not be called (not-found is not an eviction); the one
	// REST call is the abandoned run's force-cancel (Q683).
	select {
	case path := <-apiCalls:
		assert.Equal(t, "/repos/org-del/repo-del/actions/runs/77/force-cancel", path,
			"the only REST call for a worker deleted before it ran is the run's force-cancel")
	default:
		t.Fatal("the abandoned run must be force-cancelled (Q683)")
	}
	select {
	case path := <-apiCalls:
		t.Fatalf("unexpected extra REST call %s: rerun must not fire for an external delete", path)
	default:
	}

	// Secret must be cleaned up.
	secret := findSecret(ctx, t, fc, "team-a", "job-")
	assert.Nil(t, secret, "job Secret should be deleted after external pod deletion")
}

func TestBuildPod_InjectsProxyEnv(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	p.HTTPProxy = "http://proxy.example.com:8080"
	p.HTTPSProxy = "http://proxy.example.com:8080"
	p.NoProxy = "localhost,127.0.0.1"

	rg := newRG("mygroup", "team-a")

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-proxy", stubPayload(1), "")
	}()

	pod := waitForPodCreated(ctx, t, fc, "team-a")

	envMap := make(map[string]string)
	for _, e := range pod.Spec.Containers[0].Env {
		envMap[e.Name] = e.Value
	}
	assert.Equal(t, "http://proxy.example.com:8080", envMap["HTTP_PROXY"])
	assert.Equal(t, "http://proxy.example.com:8080", envMap["HTTPS_PROXY"])
	assert.Equal(t, "localhost,127.0.0.1", envMap["NO_PROXY"])

	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

// proxyCA* mirror the unexported provisioner constants. Kept in sync via the
// test below so a future rename of either side surfaces immediately.
const (
	testProxyCAVolumeName = "proxy-ca"
	testProxyCAMountPath  = "/etc/actions-gateway/proxy-ca"
	testProxyCAFileName   = "tls.crt"
)

// TestBuildPod_MountsProxyCASecret verifies that when Provisioner.ProxyTLSSecretName
// is set, the worker pod gets a Secret volume projecting only tls.crt at
// testProxyCAMountPath, with a matching read-only mount in the runner
// container, and PROXY_CA_CERT_PATH points at the cert. tls.key must never be
// projected because the worker has no use for the private key (the proxy
// holds it) and leaking it to every worker pod widens the blast radius of a
// runner compromise. Regression guard for Queue item 5h: Runner.Worker's
// outbound HTTPS through HTTPS_PROXY fails with UntrustedRoot without this
// mount.
func TestBuildPod_MountsProxyCASecret(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	p.HTTPSProxy = "https://actions-gateway-proxy.team-a.svc.cluster.local:8080"
	p.ProxyTLSSecretName = "actions-gateway-proxy-tls"

	rg := newRG("mygroup", "team-a")

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-proxy-ca", stubPayload(1), "")
	}()

	pod := waitForPodCreated(ctx, t, fc, "team-a")

	var caVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == testProxyCAVolumeName {
			caVol = &pod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, caVol, "proxy CA Secret volume must be present on the worker pod")
	require.NotNil(t, caVol.Secret, "proxy CA volume must be a Secret volume source")
	assert.Equal(t, "actions-gateway-proxy-tls", caVol.Secret.SecretName)

	require.Len(t, caVol.Secret.Items, 1,
		"only tls.crt must be projected — never tls.key — to keep the proxy private key off worker pods")
	assert.Equal(t, corev1.TLSCertKey, caVol.Secret.Items[0].Key)
	assert.Equal(t, testProxyCAFileName, caVol.Secret.Items[0].Path)

	runner := runnerOf(t, pod)

	var caMount *corev1.VolumeMount
	for i := range runner.VolumeMounts {
		if runner.VolumeMounts[i].Name == testProxyCAVolumeName {
			caMount = &runner.VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, caMount, "runner container must mount the proxy CA volume")
	assert.Equal(t, testProxyCAMountPath, caMount.MountPath)
	assert.True(t, caMount.ReadOnly, "proxy CA mount must be read-only")

	envMap := make(map[string]string)
	for _, e := range runner.Env {
		envMap[e.Name] = e.Value
	}
	assert.Equal(t, testProxyCAMountPath+"/"+testProxyCAFileName, envMap["PROXY_CA_CERT_PATH"],
		"PROXY_CA_CERT_PATH must point at the mounted cert so the worker wrapper can read it")

	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

// TestBuildPod_NoProxyCAWhenSecretNameEmpty verifies that the proxy-CA mount
// is skipped when ProxyTLSSecretName is empty (the default for tests and any
// deployment without the per-tenant egress proxy). PROXY_CA_CERT_PATH must be
// empty so the worker wrapper short-circuits the trust-store install.
func TestBuildPod_NoProxyCAWhenSecretNameEmpty(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	// ProxyTLSSecretName left empty.

	rg := newRG("mygroup", "team-a")

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-no-proxy-ca", stubPayload(1), "")
	}()

	pod := waitForPodCreated(ctx, t, fc, "team-a")

	for _, v := range pod.Spec.Volumes {
		assert.NotEqual(t, testProxyCAVolumeName, v.Name,
			"proxy CA volume must be absent when ProxyTLSSecretName is empty")
	}
	for _, c := range pod.Spec.Containers {
		for _, m := range c.VolumeMounts {
			assert.NotEqual(t, testProxyCAVolumeName, m.Name,
				"proxy CA mount must be absent when ProxyTLSSecretName is empty")
		}
		for _, e := range c.Env {
			if e.Name == "PROXY_CA_CERT_PATH" {
				assert.Empty(t, e.Value,
					"PROXY_CA_CERT_PATH must be empty when no proxy CA Secret is configured")
			}
		}
	}

	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

// TestProvisioner_RerunURLRejectsAdversarialRepository verifies that adversarial
// repository values from the payload do not reach the GitHub API. The rerun path
// is exercised end-to-end via pod eviction so the full owner/repo extraction and
// validation chain is covered.
func TestProvisioner_RerunURLRejectsAdversarialRepository(t *testing.T) {
	cases := []struct {
		name  string
		owner string
		repo  string
	}{
		// ".." passes the old regex (dots are allowed) but must be rejected by
		// the alphanumeric-first requirement to prevent path traversal.
		{"path traversal via dot-dot owner", "..", "myrepo"},
		{"path traversal via dot-dot repo", "myorg", ".."},
		{"semicolon in owner", "my;org", "myrepo"},
		{"space in repo name", "myorg", "my repo"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer goleak.VerifyNone(t)
			ctx := context.Background()

			rerunCalled := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rerunCalled <- struct{}{}
				w.WriteHeader(http.StatusCreated)
			}))
			defer srv.Close()

			fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
			p := newProvisioner(fc)
			p.TokenFunc = func(context.Context) (string, error) { return "tok", nil }
			p.GitHubAPIURL = srv.URL
			p.HTTPClient = srv.Client()

			rg := newRG("mygroup", "team-a")
			payload := stubPayloadFull(tc.owner, tc.repo, 42)

			done := make(chan error, 1)
			go func() { done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-adv-repo", payload, "") }()

			pod := waitForPodCreated(ctx, t, fc, "team-a")
			evictPod(ctx, t, fc, "team-a", pod.Name)
			require.NoError(t, <-done) // eviction is non-fatal

			// Rerun API must not be called for adversarial owner/repo values.
			select {
			case <-rerunCalled:
				t.Errorf("rerun API must not be called for adversarial owner=%q repo=%q", tc.owner, tc.repo)
			default:
			}
		})
	}
}

func TestBuildPod_OverwritesTenantProxyEnv(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	p.HTTPProxy = "http://real-proxy.example.com:8080"

	rg := newRG("mygroup", "team-a")
	// Tenant sets a bad HTTP_PROXY — provisioner must overwrite it.
	rg.Spec.PodTemplate.Spec.Containers = []corev1.Container{{
		Name: "runner",
		Env:  []corev1.EnvVar{{Name: "HTTP_PROXY", Value: "http://bad-proxy.example.com"}},
	}}

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-overwrite", stubPayload(1), "")
	}()

	pod := waitForPodCreated(ctx, t, fc, "team-a")

	envMap := make(map[string]string)
	for _, e := range pod.Spec.Containers[0].Env {
		envMap[e.Name] = e.Value
	}
	assert.Equal(t, "http://real-proxy.example.com:8080", envMap["HTTP_PROXY"], "tenant HTTP_PROXY must be overwritten")

	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

// TestProvisioner_RGMaxEvictionRetriesZero verifies that setting maxEvictionRetries:0
// on the RunnerGroup suppresses auto-retry: no rerun API call, exhausted metric incremented.
func TestProvisioner_RGMaxEvictionRetriesZero(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	rerunCalled := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rerunCalled <- struct{}{}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	m := newTestMetrics()
	p.Metrics = m
	p.TokenFunc = func(context.Context) (string, error) { return "tok", nil }
	p.GitHubAPIURL = srv.URL
	p.HTTPClient = srv.Client()

	zero := int32(0)
	rg := newRG("mygroup", "team-a")
	rg.Spec.MaxEvictionRetries = &zero
	payload := stubPayloadFull("org", "repo", 42)

	done := make(chan error, 1)
	go func() { done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-zero-retry", payload, "") }()

	pod := waitForPodCreated(ctx, t, fc, "team-a")
	evictPod(ctx, t, fc, "team-a", pod.Name)
	require.NoError(t, <-done)

	// Rerun API must NOT be called.
	select {
	case <-rerunCalled:
		t.Fatal("rerun API should not be called when maxEvictionRetries=0")
	default:
	}

	// Exhausted counter must increment immediately.
	assert.Equal(t, float64(1), testutil.ToFloat64(m.EvictionRetriesExhausted.WithLabelValues("team-a", "mygroup", "classic", "eviction")))
	assert.Equal(t, float64(0), testutil.ToFloat64(m.EvictionRetries.WithLabelValues("team-a", "mygroup", "classic", "eviction")))
}

// TestProvisioner_RGMaxEvictionRetriesOne verifies that maxEvictionRetries:1 on the
// RunnerGroup overrides the provisioner default: one retry fires, then budget exhausts.
func TestProvisioner_RGMaxEvictionRetriesOne(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	var rerunCount int
	rerunCalls := make(chan struct{}, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rerunCount++
		rerunCalls <- struct{}{}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	// Provisioner default is 2; RG overrides to 1.
	p := newProvisioner(fc)
	m := newTestMetrics()
	p.Metrics = m
	p.MaxEvictionRetries = 2
	p.TokenFunc = func(context.Context) (string, error) { return "tok", nil }
	p.GitHubAPIURL = srv.URL
	p.HTTPClient = srv.Client()

	one := int32(1)
	rg := newRG("mygroup", "ns")
	rg.Spec.MaxEvictionRetries = &one
	payload := stubPayloadFull("org", "repo", 77)

	runCycle := func(planID string) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- errOnly(p.HandlerFor(rg))(ctx, "", planID, payload, "") }()
		var podToEvict *corev1.Pod
		require.Eventually(t, func() bool {
			var list corev1.PodList
			if err := fc.List(ctx, &list, client.InNamespace("ns")); err != nil {
				return false
			}
			for i := range list.Items {
				if list.Items[i].Status.Phase != corev1.PodFailed {
					podToEvict = &list.Items[i]
					return true
				}
			}
			return false
		}, 2*time.Second, 5*time.Millisecond)
		evictPod(ctx, t, fc, "ns", podToEvict.Name)
		require.NoError(t, <-done)
	}

	// First eviction: retry fires (count 0 < 1).
	runCycle("plan-rg-retry-1")
	select {
	case <-rerunCalls:
	case <-time.After(2 * time.Second):
		t.Fatal("expected rerun API call on first eviction")
	}

	// Second eviction: budget exhausted (count 1 >= 1), no retry.
	runCycle("plan-rg-retry-2")
	assert.Equal(t, float64(1), testutil.ToFloat64(m.EvictionRetriesExhausted.WithLabelValues("ns", "mygroup", "classic", "eviction")))
	assert.Equal(t, 1, rerunCount, "rerun API should be called exactly once")
}

// TestProvisioner_RGEvictionRetryDelay verifies that evictionRetryDelay on the
// RunnerGroup overrides the provisioner-level delay.
func TestProvisioner_RGEvictionRetryDelay(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	rerunAt := make(chan time.Time, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rerunAt <- time.Now()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	p.EvictionRetryDelay = 0 // provisioner default is effectively zero in tests
	p.TokenFunc = func(context.Context) (string, error) { return "tok", nil }
	p.GitHubAPIURL = srv.URL
	p.HTTPClient = srv.Client()

	delay := metav1.Duration{Duration: 50 * time.Millisecond}
	rg := newRG("mygroup", "team-a")
	rg.Spec.EvictionRetryDelay = &delay
	payload := stubPayloadFull("org", "repo", 11)

	done := make(chan error, 1)
	go func() { done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-delay", payload, "") }()

	pod := waitForPodCreated(ctx, t, fc, "team-a")
	evictAt := time.Now()
	evictPod(ctx, t, fc, "team-a", pod.Name)
	require.NoError(t, <-done)

	select {
	case ts := <-rerunAt:
		assert.GreaterOrEqual(t, ts.Sub(evictAt), 40*time.Millisecond,
			"rerun should not fire before evictionRetryDelay elapses")
	case <-time.After(2 * time.Second):
		t.Fatal("rerun API was not called within timeout")
	}
}

// TestProvisioner_QuotaRetrySucceeds verifies that a single quota rejection at pod
// creation is retried and the job completes successfully once quota frees up.
func TestProvisioner_QuotaRetrySucceeds(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	// Fail the first pod create with a quota error; succeed on the second.
	qc := &quotaPodCreateClient{Client: fc, failCount: 1}

	p := newProvisioner(qc)
	m := newTestMetrics()
	p.Metrics = m
	p.QuotaRetryDelay = 1 * time.Millisecond

	rg := newRG("mygroup", "team-a")
	payload := stubPayload(1)

	done := make(chan error, 1)
	go func() { done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-quota-ok", payload, "") }()

	// Pod appears after the first (failed) attempt retries.
	pod := waitForPodCreated(ctx, t, fc, "team-a")
	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)

	// One quota retry recorded; none exhausted.
	assert.Equal(t, float64(1), testutil.ToFloat64(m.QuotaRetries.WithLabelValues("team-a", "mygroup")))
	assert.Equal(t, float64(0), testutil.ToFloat64(m.QuotaRetriesExhausted.WithLabelValues("team-a", "mygroup")))
}

// TestProvisioner_QuotaRetryExhausted verifies that after maxQuotaRetries failed
// attempts the provisioner gives up, increments the exhausted counter, and cleans up.
func TestProvisioner_QuotaRetryExhausted(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	// Always return a quota error — budget (default 5) will exhaust.
	qc := &quotaPodCreateClient{Client: fc, failCount: 100}

	p := newProvisioner(qc)
	m := newTestMetrics()
	p.Metrics = m
	rec := &fakeEventRecorder{}
	p.Events = rec
	p.MaxQuotaRetries = 2
	p.QuotaRetryDelay = 1 * time.Millisecond

	rg := newRG("mygroup", "team-a")

	err := errOnly(p.HandlerFor(rg))(ctx, "", "plan-quota-exhaust", stubPayload(1), "")
	assert.Error(t, err)

	// No pod created; Secret cleaned up.
	assert.Nil(t, findPod(ctx, t, fc, "team-a"))
	assert.Nil(t, findSecret(ctx, t, fc, "team-a", "job-"))

	// 2 retries attempted (attempts 1 and 2 after the initial failure), then exhausted.
	assert.Equal(t, float64(2), testutil.ToFloat64(m.QuotaRetries.WithLabelValues("team-a", "mygroup")))
	assert.Equal(t, float64(1), testutil.ToFloat64(m.QuotaRetriesExhausted.WithLabelValues("team-a", "mygroup")))

	// A Warning Event is recorded on the owner so quota exhaustion surfaces in
	// `kubectl describe`, not just the metric (Q170).
	evs := rec.withReason("QuotaRetriesExhausted")
	require.Len(t, evs, 1)
	assert.Equal(t, corev1.EventTypeWarning, evs[0].eventtype)
	assert.Equal(t, "team-a", evs[0].namespace)
	assert.Equal(t, "mygroup", evs[0].name)
	assert.Contains(t, evs[0].note, "ResourceQuota")
}

// TestProvisioner_QuotaRetryDisabled verifies that maxQuotaRetries:0 causes an
// immediate failure on quota rejection with no retries.
func TestProvisioner_QuotaRetryDisabled(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	qc := &quotaPodCreateClient{Client: fc, failCount: 1}

	p := newProvisioner(qc)
	m := newTestMetrics()
	p.Metrics = m
	rec := &fakeEventRecorder{}
	p.Events = rec

	zero := int32(0)
	rg := newRG("mygroup", "team-a")
	rg.Spec.MaxQuotaRetries = &zero

	err := errOnly(p.HandlerFor(rg))(ctx, "", "plan-quota-disabled", stubPayload(1), "")
	assert.Error(t, err)

	// No pod; no retry counters incremented.
	assert.Nil(t, findPod(ctx, t, fc, "team-a"))
	assert.Equal(t, float64(0), testutil.ToFloat64(m.QuotaRetries.WithLabelValues("team-a", "mygroup")))
	assert.Equal(t, float64(0), testutil.ToFloat64(m.QuotaRetriesExhausted.WithLabelValues("team-a", "mygroup")))
	// Retry disabled is a policy choice, not a budget failure: no Event (no spam).
	assert.Empty(t, rec.withReason("QuotaRetriesExhausted"))
}

// TestProvisioner_NonQuotaCreateFailureNoRetry verifies that a non-quota pod
// creation error (e.g. admission webhook rejection) is not retried.
func TestProvisioner_NonQuotaCreateFailureNoRetry(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	// failPodCreateClient returns a generic error, not a quota error.
	p := newProvisioner(failPodCreateClient{fc})
	m := newTestMetrics()
	p.Metrics = m
	p.MaxQuotaRetries = 5 // quota retry enabled, but should not fire

	rg := newRG("mygroup", "team-a")

	err := errOnly(p.HandlerFor(rg))(ctx, "", "plan-nonquota", stubPayload(1), "")
	assert.Error(t, err)

	// No quota retries attempted.
	assert.Equal(t, float64(0), testutil.ToFloat64(m.QuotaRetries.WithLabelValues("team-a", "mygroup")))
	assert.Equal(t, float64(0), testutil.ToFloat64(m.QuotaRetriesExhausted.WithLabelValues("team-a", "mygroup")))
}

// runAndGetPod fires the provisioner's job handler for rg in a goroutine,
// waits for the worker pod to be created, then completes it so the handler
// returns cleanly (keeping goleak happy). It returns the created pod.
func runAndGetPod(ctx context.Context, t *testing.T, p *provisioner.Provisioner, fc client.Client, rg *v1alpha1.RunnerGroup, planID, ns string) *corev1.Pod {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", planID, stubPayload(1), "")
	}()
	pod := waitForPodCreated(ctx, t, fc, ns)
	completePod(ctx, t, fc, ns, pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
	return pod
}

// TestBuildPod_BaselineSecurityDefaults verifies that under the default
// (baseline) profile the worker pod gets pod-level runAsNonRoot + seccomp
// RuntimeDefault, but NOT the per-container allowPrivilegeEscalation/cap-drop
// floor — baseline PSA permits in-job privilege escalation (sudo), and many CI
// jobs rely on it.
func TestBuildPod_BaselineSecurityDefaults(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	// SecurityProfile left empty — exercises the empty-string -> baseline path.

	pod := runAndGetPod(ctx, t, p, fc, newRG("mygroup", "team-a"), "plan-baseline", "team-a")

	require.NotNil(t, pod.Spec.SecurityContext)
	require.NotNil(t, pod.Spec.SecurityContext.RunAsNonRoot)
	assert.True(t, *pod.Spec.SecurityContext.RunAsNonRoot)
	// Q115: a numeric runAsUser must accompany runAsNonRoot so kubelet can
	// verify non-root against the runner image's non-numeric `USER runner`.
	require.NotNil(t, pod.Spec.SecurityContext.RunAsUser, "baseline must gap-fill a numeric runAsUser")
	assert.Equal(t, int64(1001), *pod.Spec.SecurityContext.RunAsUser)
	require.NotNil(t, pod.Spec.SecurityContext.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, pod.Spec.SecurityContext.SeccompProfile.Type)

	// No restricted-only container floor under baseline.
	sc := pod.Spec.Containers[0].SecurityContext
	if sc != nil {
		assert.Nil(t, sc.AllowPrivilegeEscalation, "baseline must not block in-job privilege escalation")
		assert.Nil(t, sc.Capabilities, "baseline must not drop capabilities")
	}
}

// TestBuildPod_RestrictedSecurityDefaults verifies that the restricted profile
// stamps the full PSA-restricted container floor so the namespace's PodSecurity
// admission accepts the pod.
func TestBuildPod_RestrictedSecurityDefaults(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	p.SecurityProfile = "restricted"

	pod := runAndGetPod(ctx, t, p, fc, newRG("mygroup", "team-a"), "plan-restricted", "team-a")

	require.NotNil(t, pod.Spec.SecurityContext)
	require.NotNil(t, pod.Spec.SecurityContext.RunAsNonRoot)
	assert.True(t, *pod.Spec.SecurityContext.RunAsNonRoot)
	// Q115: the numeric runAsUser is stamped at pod level on restricted too, so
	// every container inherits a kubelet-verifiable non-root UID.
	require.NotNil(t, pod.Spec.SecurityContext.RunAsUser, "restricted must gap-fill a numeric runAsUser")
	assert.Equal(t, int64(1001), *pod.Spec.SecurityContext.RunAsUser)

	sc := pod.Spec.Containers[0].SecurityContext
	require.NotNil(t, sc)
	require.NotNil(t, sc.AllowPrivilegeEscalation)
	assert.False(t, *sc.AllowPrivilegeEscalation)
	require.NotNil(t, sc.Capabilities)
	assert.Equal(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop)
	require.NotNil(t, sc.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type)
}

// TestBuildPod_PrivilegedSkipsSecurityDefaults verifies that the privileged
// profile stamps no SecurityContext defaults (so DinD/host-cap workloads can
// opt in via their PodTemplate), while resource defaults still apply.
func TestBuildPod_PrivilegedSkipsSecurityDefaults(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	p.SecurityProfile = "privileged"

	pod := runAndGetPod(ctx, t, p, fc, newRG("mygroup", "team-a"), "plan-priv", "team-a")

	assert.Nil(t, pod.Spec.SecurityContext, "privileged profile must not stamp pod SecurityContext")
	assert.Nil(t, pod.Spec.Containers[0].SecurityContext, "privileged profile must not stamp container SecurityContext")

	// Resource defaults apply on every profile.
	assert.Equal(t, defaultCPU(), pod.Spec.Containers[0].Resources.Requests.Cpu().String())
}

// TestBuildPod_ResourceDefaults verifies default CPU/memory requests+limits are
// stamped when the tenant omits them, yielding Guaranteed QoS.
func TestBuildPod_ResourceDefaults(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	pod := runAndGetPod(ctx, t, p, fc, newRG("mygroup", "team-a"), "plan-res", "team-a")

	res := pod.Spec.Containers[0].Resources
	assert.Equal(t, "500m", res.Requests.Cpu().String())
	assert.Equal(t, "1Gi", res.Requests.Memory().String())
	assert.Equal(t, "500m", res.Limits.Cpu().String())
	assert.Equal(t, "1Gi", res.Limits.Memory().String())
}

// TestBuildPod_TenantOverridesPreserved verifies the defaults are gap-fill only:
// a tenant's explicit runAsNonRoot:false and explicit resources both survive.
func TestBuildPod_TenantOverridesPreserved(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")
	runAsRoot := false
	rg.Spec.PodTemplate.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: &runAsRoot}
	rg.Spec.PodTemplate.Spec.Containers = []corev1.Container{{
		Name: "runner",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
		},
	}}

	pod := runAndGetPod(ctx, t, p, fc, rg, "plan-override", "team-a")

	require.NotNil(t, pod.Spec.SecurityContext.RunAsNonRoot)
	assert.False(t, *pod.Spec.SecurityContext.RunAsNonRoot, "tenant runAsNonRoot:false must be preserved")
	// Q115: with non-root opted out (a root-based image), we must NOT force the
	// runner UID — doing so would contradict the tenant's choice.
	assert.Nil(t, pod.Spec.SecurityContext.RunAsUser, "runAsUser must not be gap-filled when the tenant disabled runAsNonRoot")
	// Seccomp still gap-filled because the tenant left it unset.
	require.NotNil(t, pod.Spec.SecurityContext.SeccompProfile)
	// Tenant resources preserved; no default stamped over them.
	assert.Equal(t, "250m", pod.Spec.Containers[0].Resources.Requests.Cpu().String())
	assert.True(t, pod.Spec.Containers[0].Resources.Limits.Cpu().IsZero(), "tenant-set resources must not be overwritten with defaults")
}

// TestBuildPod_TenantRunAsUserPreserved verifies the Q115 runAsUser gap-fill is
// gap-fill only: a tenant that pins its own UID (e.g. a custom image whose user
// is not 1001) keeps it, and runAsNonRoot is still enforced.
func TestBuildPod_TenantRunAsUserPreserved(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")
	uid := int64(2000)
	rg.Spec.PodTemplate.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsUser: &uid}

	pod := runAndGetPod(ctx, t, p, fc, rg, "plan-uid", "team-a")

	require.NotNil(t, pod.Spec.SecurityContext.RunAsUser)
	assert.Equal(t, int64(2000), *pod.Spec.SecurityContext.RunAsUser, "tenant runAsUser must be preserved")
	// runAsNonRoot is still gap-filled to true alongside the tenant's UID.
	require.NotNil(t, pod.Spec.SecurityContext.RunAsNonRoot)
	assert.True(t, *pod.Spec.SecurityContext.RunAsNonRoot)
}

// TestBuildPod_RecommendedLabels verifies worker pods carry the recommended
// app.kubernetes.io/* labels for tooling interop.
func TestBuildPod_RecommendedLabels(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	pod := runAndGetPod(ctx, t, p, fc, newRG("mygroup", "team-a"), "plan-labels", "team-a")

	assert.Equal(t, "actions-runner", pod.Labels["app.kubernetes.io/name"])
	assert.Equal(t, "mygroup", pod.Labels["app.kubernetes.io/instance"])
	assert.Equal(t, "runner", pod.Labels["app.kubernetes.io/component"])
	assert.Equal(t, "actions-gateway", pod.Labels["app.kubernetes.io/part-of"])
	// Existing NetworkPolicy-matching label must be unchanged.
	assert.Equal(t, "workload", pod.Labels["actions-gateway/component"])
}

// TestBuildPod_DisruptionSafetyDefaults verifies every worker pod is stamped
// with the node-disruption-safety markers (Q218) so Karpenter consolidation,
// cluster-autoscaler scale-down, and the descheduler do not evict a pod
// mid-job and strand the CI run.
func TestBuildPod_DisruptionSafetyDefaults(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	pod := runAndGetPod(ctx, t, p, fc, newRG("mygroup", "team-a"), "plan-disrupt", "team-a")

	assert.Equal(t, "true", pod.Annotations["karpenter.sh/do-not-disrupt"])
	assert.Equal(t, "false", pod.Annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"])
	assert.Equal(t, "true", pod.Annotations["descheduler.alpha.kubernetes.io/prefer-no-eviction"])
	// The defaults must coexist with the job-metadata annotations, not clobber them.
	assert.Equal(t, "1", pod.Annotations["actions-gateway.com/run-id"])
}

// TestBuildPod_DisruptionSafetyTenantOverride verifies the disruption-safety
// markers are gap-fill only: a tenant that sets one of the keys in its
// PodTemplate metadata (e.g. opting a known-interruptible job back into
// eviction) keeps its explicit value, while the other keys still default.
func TestBuildPod_DisruptionSafetyTenantOverride(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")
	rg.Spec.PodTemplate.ObjectMeta.Annotations = map[string]string{
		"cluster-autoscaler.kubernetes.io/safe-to-evict": "true",
	}

	pod := runAndGetPod(ctx, t, p, fc, rg, "plan-disrupt-override", "team-a")

	assert.Equal(t, "true", pod.Annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"],
		"explicit tenant override must win")
	// Untouched keys still default.
	assert.Equal(t, "true", pod.Annotations["karpenter.sh/do-not-disrupt"])
	assert.Equal(t, "true", pod.Annotations["descheduler.alpha.kubernetes.io/prefer-no-eviction"])
	// Arbitrary template annotations are NOT copied onto the worker pod — only
	// the three disruption-safety keys are honored from the template.
	rg2 := newRG("othergroup", "team-b")
	rg2.Spec.PodTemplate.ObjectMeta.Annotations = map[string]string{"tenant.example.com/foo": "bar"}
	pod2 := runAndGetPod(ctx, t, p, fc, rg2, "plan-disrupt-arbitrary", "team-b")
	_, copied := pod2.Annotations["tenant.example.com/foo"]
	assert.False(t, copied, "arbitrary tenant PodTemplate annotations must not be stamped on the worker pod")
}

// defaultCPU returns the default worker CPU request as a string for assertions.
func defaultCPU() string { return "500m" }

// TestProvisioner_SetsOwnerReferences verifies the worker pod and job Secret
// carry a controller OwnerReference to the RunnerGroup, so deleting the
// RunnerGroup (or its namespace) cascade-deletes both — including objects
// orphaned by an AGC crash (Q95).
func TestProvisioner_SetsOwnerReferences(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")
	rg.UID = types.UID("rg-uid-123")

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-ownerref", stubPayload(1), "")
	}()

	pod := waitForPodCreated(ctx, t, fc, "team-a")
	secret := findSecret(ctx, t, fc, "team-a", "job-")
	require.NotNil(t, secret)

	for name, refs := range map[string][]metav1.OwnerReference{
		"pod":    pod.OwnerReferences,
		"secret": secret.OwnerReferences,
	} {
		require.Len(t, refs, 1, "%s must have exactly one ownerReference", name)
		ref := refs[0]
		assert.Equal(t, "actions-gateway.github.com/v1alpha1", ref.APIVersion, name)
		assert.Equal(t, "RunnerGroup", ref.Kind, name)
		assert.Equal(t, "mygroup", ref.Name, name)
		assert.Equal(t, types.UID("rg-uid-123"), ref.UID, name)
		require.NotNil(t, ref.Controller, name)
		assert.True(t, *ref.Controller, "%s ownerReference must be a controller ref", name)
		assert.Nil(t, ref.BlockOwnerDeletion,
			"%s must not set blockOwnerDeletion (needs finalizer perms under OwnerReferencesPermissionEnforcement)", name)
	}

	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

// TestProvisioner_DeletesPodOnCompletionWhenTTLZero pins the completedPodTTL=0s
// fast path: the provision goroutine deletes the worker pod itself in its
// cleanup step, without waiting for the reconciler's reaper (Q95).
func TestProvisioner_DeletesPodOnCompletionWhenTTLZero(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")
	rg.Spec.CompletedPodTTL = &metav1.Duration{Duration: 0}

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-ttl0", stubPayload(1), "")
	}()

	pod := waitForPodCreated(ctx, t, fc, "team-a")
	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)

	assert.Nil(t, findPod(ctx, t, fc, "team-a"),
		"worker pod must be deleted on completion when completedPodTTL is 0s")
	assert.Nil(t, findSecret(ctx, t, fc, "team-a", "job-"),
		"job Secret must be deleted on completion")
}

// TestProvisioner_RetainsPodOnCompletionByDefault pins the default retention
// contract: with completedPodTTL omitted the goroutine leaves the terminal pod
// in place (the reconciler's reaper deletes it after DefaultCompletedPodTTL),
// giving operators a window to inspect a failed pod (Q95).
func TestProvisioner_RetainsPodOnCompletionByDefault(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")

	done := make(chan error, 1)
	go func() {
		done <- errOnly(p.HandlerFor(rg))(ctx, "", "plan-retain", stubPayload(1), "")
	}()

	pod := waitForPodCreated(ctx, t, fc, "team-a")
	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)

	assert.NotNil(t, findPod(ctx, t, fc, "team-a"),
		"terminal worker pod must be retained for the reaper when completedPodTTL is unset")
	assert.Nil(t, findSecret(ctx, t, fc, "team-a", "job-"),
		"job Secret must still be deleted on completion")
}

// testGitHubCA* mirror the unexported provisioner constants for the GHES appliance's
// CA bundle, like testProxyCA* above.
const (
	testGitHubCAVolumeName = "github-ca"
	testGitHubCAMountPath  = "/etc/actions-gateway/github-ca"
	testGitHubCAFileName   = "ca.crt"
)

// TestBuildPod_MountsGitHubCABundle is the worker half of Q536: an AGC whose gateway
// set githubCABundleRef projects the same ConfigMap into every worker pod, so the
// runner's own calls to a private-CA GHES appliance — checkout, log and artifact
// upload — complete the handshake the control plane already can. A ConfigMap, not a
// Secret: the bundle is public certificate material.
func TestBuildPod_MountsGitHubCABundle(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	target := &fakeTarget{
		key:    client.ObjectKey{Namespace: "team-a", Name: "gpu"},
		labels: map[string]string{"actions-gateway.com/runner-set": "gpu"},
		spec: &provisioner.ResolvedSpec{
			WorkerImage:           "runner:test",
			GitHubCAConfigMapName: "ghes-ca",
		},
	}
	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target,
		provisioner.ScaleSetJob{JobID: "job-uuid-1", JITConfig: "eyJydW5uZXIiOnt9fQ=="}))

	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)

	var caVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == testGitHubCAVolumeName {
			caVol = &pod.Spec.Volumes[i]
		}
	}
	require.NotNil(t, caVol, "the GitHub CA bundle must be projected into the worker pod")
	require.NotNil(t, caVol.ConfigMap, "the bundle is public material and travels in a ConfigMap")
	assert.Equal(t, "ghes-ca", caVol.ConfigMap.Name)
	require.Len(t, caVol.ConfigMap.Items, 1, "the projection is pinned to the ca.crt key")
	assert.Equal(t, testGitHubCAFileName, caVol.ConfigMap.Items[0].Key)

	runner := runnerOf(t, pod)
	var caMount *corev1.VolumeMount
	for i := range runner.VolumeMounts {
		if runner.VolumeMounts[i].Name == testGitHubCAVolumeName {
			caMount = &runner.VolumeMounts[i]
		}
	}
	require.NotNil(t, caMount, "the runner container must mount the GitHub CA volume")
	assert.Equal(t, testGitHubCAMountPath, caMount.MountPath)
	assert.True(t, caMount.ReadOnly)

	envMap := make(map[string]string)
	for _, e := range runner.Env {
		envMap[e.Name] = e.Value
	}
	assert.Equal(t, testGitHubCAMountPath+"/"+testGitHubCAFileName, envMap["GITHUB_CA_CERT_PATH"],
		"GITHUB_CA_CERT_PATH must point at the mounted bundle so the wrapper can read it")
}

// TestBuildPod_NoGitHubCAWhenConfigMapNameEmpty is the negative half: a public-GitHub
// gateway needs no extra trust, so the pod must be exactly what it is today and
// GITHUB_CA_CERT_PATH must be empty so the wrapper skips the bundle.
func TestBuildPod_NoGitHubCAWhenConfigMapNameEmpty(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	target := &fakeTarget{
		key:    client.ObjectKey{Namespace: "team-a", Name: "gpu"},
		labels: map[string]string{"actions-gateway.com/runner-set": "gpu"},
		spec:   &provisioner.ResolvedSpec{WorkerImage: "runner:test"},
	}
	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target,
		provisioner.ScaleSetJob{JobID: "job-uuid-1", JITConfig: "eyJydW5uZXIiOnt9fQ=="}))

	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)
	for _, v := range pod.Spec.Volumes {
		assert.NotEqual(t, testGitHubCAVolumeName, v.Name,
			"no githubCABundleRef ⇒ no GitHub CA volume")
	}
	for _, e := range runnerOf(t, pod).Env {
		if e.Name == "GITHUB_CA_CERT_PATH" {
			assert.Empty(t, e.Value, "GITHUB_CA_CERT_PATH must be empty when no bundle is configured")
		}
	}
}
