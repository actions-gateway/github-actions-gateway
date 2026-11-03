package provisioner_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

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
		EvictionRerunWithheld: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_prov_eviction_rerun_withheld_total",
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
	return s
}

func newProvisioner(c client.Client) *provisioner.Provisioner {
	p := provisioner.NewProvisioner(c, nil, nil)
	p.PollInterval = 1 * time.Millisecond
	p.EvictionRetryDelay = 1 * time.Millisecond
	return p
}

func newRG(name, ns string) *testGroup {
	return &testGroup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
}

// testGroup is the owner the classic-tier provisioning tests drive Handle through: a
// spec the testGroup adapter resolves the way an owner adapter does, defaulting from
// the Provisioner's process-wide fields.
type testGroup struct {
	metav1.ObjectMeta
	Spec testGroupSpec

	// events receives the group's owner-scoped provisioning Events; nil drops them.
	events runnercore.EventRecorder
}

type testGroupSpec struct {
	PodTemplate        corev1.PodTemplateSpec
	WorkerImage        string
	MaxWorkers         *int32
	PriorityTiers      []provisioner.TierThreshold
	MaxEvictionRetries *int32
	EvictionRetryDelay *metav1.Duration
	MaxQuotaRetries    *int32
	QuotaRetryDelay    *metav1.Duration
	CompletedPodTTL    *metav1.Duration
	MaxWorkerLifetime  *metav1.Duration
}

// provision provisions one worker for g the way the scale-set listener does for an
// assigned job. It returns once the pod exists.
func (g *testGroup) provision(ctx context.Context, p *provisioner.Provisioner, jobID string) error {
	return p.ProvisionScaleSetWorker(ctx, &testGroupTarget{g: g, p: p},
		provisioner.ScaleSetJob{JobID: jobID, JITConfig: "jit-" + jobID, RunID: "1"})
}

type testGroupTarget struct {
	g *testGroup
	p *provisioner.Provisioner
}

func (t *testGroupTarget) Key() client.ObjectKey {
	return client.ObjectKey{Namespace: t.g.Namespace, Name: t.g.Name}
}
func (t *testGroupTarget) OwnerRef() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "actions-gateway.com/v2alpha1", Kind: "RunnerSet",
		Name: t.g.Name, UID: t.g.UID, Controller: ptr.To(true),
	}
}
func (t *testGroupTarget) PodOwnerLabels() map[string]string {
	return map[string]string{provisioner.LabelRunnerSet: t.g.Name}
}
func (t *testGroupTarget) RecordEvent(eventtype, reason, action, note string) {
	if t.g.events != nil {
		t.g.events.Event(t.g.Namespace, t.g.Name, eventtype, reason, action, note)
	}
}
func (t *testGroupTarget) Ceiling(context.Context) (int32, bool) {
	return provisioner.WorkerCeilingFromTiers(t.g.Spec.PriorityTiers, t.g.Spec.MaxWorkers)
}
func (t *testGroupTarget) QuotaExhausted(ctx context.Context) (bool, string) {
	return provisioner.WorkerQuotaExhausted(ctx, t.p.Client, t.g.Namespace, &t.g.Spec.PodTemplate.Spec)
}
func (t *testGroupTarget) QuotaCapacity(context.Context, int32) (int32, bool) { return 0, false }
func (t *testGroupTarget) CapacityDeclined(context.Context) (bool, string)    { return false, "" }
func (t *testGroupTarget) DeclinedCapacity(context.Context, int32) (int32, bool) {
	return 0, false
}
func (t *testGroupTarget) ScaleUpLimit(context.Context) *provisioner.ScaleUpConfig { return nil }
func (t *testGroupTarget) Resolve(context.Context) (*provisioner.ResolvedSpec, error) {
	g, p := t.g, t.p
	spec := &provisioner.ResolvedSpec{
		PodTemplate:        g.Spec.PodTemplate,
		WorkerImage:        g.Spec.WorkerImage,
		MaxWorkers:         g.Spec.MaxWorkers,
		PriorityTiers:      g.Spec.PriorityTiers,
		MaxEvictionRetries: p.MaxEvictionRetries,
		EvictionRetryDelay: p.EvictionRetryDelay,
		MaxQuotaRetries:    p.MaxQuotaRetries,
		QuotaRetryDelay:    p.QuotaRetryDelay,
		CompletedPodTTL:    provisioner.CompletedPodTTLOrDefault(g.Spec.CompletedPodTTL),
		MaxWorkerLifetime:  provisioner.MaxWorkerLifetimeOrDefault(g.Spec.MaxWorkerLifetime),
		HTTPProxy:          p.HTTPProxy,
		HTTPSProxy:         p.HTTPSProxy,
		NoProxy:            p.NoProxy,
		ProxyTLSSecretName: p.ProxyTLSSecretName,
		SecurityProfile:    p.SecurityProfile,
	}
	if g.Spec.MaxEvictionRetries != nil {
		spec.MaxEvictionRetries = int(*g.Spec.MaxEvictionRetries)
	}
	if g.Spec.EvictionRetryDelay != nil && g.Spec.EvictionRetryDelay.Duration > 0 {
		spec.EvictionRetryDelay = g.Spec.EvictionRetryDelay.Duration
	}
	if g.Spec.MaxQuotaRetries != nil {
		spec.MaxQuotaRetries = int(*g.Spec.MaxQuotaRetries)
	}
	if g.Spec.QuotaRetryDelay != nil && g.Spec.QuotaRetryDelay.Duration > 0 {
		spec.QuotaRetryDelay = g.Spec.QuotaRetryDelay.Duration
	}
	return spec, nil
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

	require.NoError(t, rg.provision(ctx, p, "plan-abc-123"))

	pod := waitForPodCreated(ctx, t, fc, "team-a")
	assert.Equal(t, agcnames.ControllerName, pod.Labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, "mygroup", pod.Labels[provisioner.LabelRunnerSet])

	secret := findSecret(ctx, t, fc, "team-a", "job-")
	require.NotNil(t, secret)
	assert.Equal(t, []byte("plan-abc-123"), secret.Data["plan-id"])
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

	require.NoError(t, p.ProvisionScaleSetWorker(ctx, &testGroupTarget{g: rg, p: p},
		provisioner.ScaleSetJob{JobID: "plan-jit", JITConfig: jitBlob}))

	secret := findSecret(ctx, t, fc, "team-a", "job-")
	require.NotNil(t, secret)
	assert.Equal(t, []byte(jitBlob), secret.Data["jitconfig"],
		"worker Secret must carry the JIT blob under the 'jitconfig' key")
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
				Labels:    map[string]string{provisioner.LabelRunnerSet: "mygroup"},
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
	err := rg.provision(ctx, p, "plan-hold")
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
	rg.Spec.PriorityTiers = []provisioner.TierThreshold{
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
				Labels:    map[string]string{provisioner.LabelRunnerSet: "mygroup"},
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

	require.NoError(t, rg.provision(ctx, p, "plan-tier"))

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
}

func TestProvisioner_PriorityTiersCeiling(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	rg := newRG("mygroup", "team-a")
	rg.Spec.PriorityTiers = []provisioner.TierThreshold{
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
				Labels:    map[string]string{provisioner.LabelRunnerSet: "mygroup"},
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
	err := rg.provision(ctx, p, "plan-ceil")
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

	require.NoError(t, rg.provision(ctx, p, "plan-img"))

	pod := waitForPodCreated(ctx, t, fc, "team-a")
	runnerContainer := runnerOf(t, pod)
	assert.Equal(t, "my-custom-image:latest", runnerContainer.Image)
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

	require.NoError(t, rg.provision(ctx, p, "plan-gapfill"))

	pod := waitForPodCreated(ctx, t, fc, "team-a")
	runner := runnerOf(t, pod)
	assert.Equal(t, "my-custom-image:latest", runner.Image, "image-less runner must be gap-filled with the resolved worker image")
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

	require.NoError(t, rg.provision(ctx, p, "plan-nooverride"))

	pod := waitForPodCreated(ctx, t, fc, "team-a")
	runner := runnerOf(t, pod)
	assert.Equal(t, "tenant-image:pinned", runner.Image, "an explicitly-set runner image must not be overridden")
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

	require.NoError(t, rg.provision(ctx, p, "plan-reserved"))

	pod := waitForPodCreated(ctx, t, fc, "team-a")
	assert.Equal(t, "agc-worker", pod.Spec.ServiceAccountName)
	assert.False(t, pod.Spec.HostPID)
	assert.False(t, pod.Spec.HostNetwork)
	assert.False(t, pod.Spec.HostIPC)
	autoMount := pod.Spec.AutomountServiceAccountToken
	assert.NotNil(t, autoMount)
	assert.False(t, *autoMount)
	assert.Equal(t, corev1.RestartPolicyNever, pod.Spec.RestartPolicy)
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

	err := rg.provision(ctx, p, "plan-conflict")
	assert.Error(t, err)

	// Secret must be cleaned up even though pod creation failed.
	var secretList corev1.SecretList
	require.NoError(t, fc.List(ctx, &secretList, client.InNamespace("team-a")))
	for _, s := range secretList.Items {
		assert.NotContains(t, s.Name, "job-", "all job Secrets should be cleaned up on pod creation failure")
	}
}

func TestProvisioner_SecretMountedInPod(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")

	require.NoError(t, rg.provision(ctx, p, "plan-mount"))

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
}

// TestProvisioner_PriorityTiersSecondTier verifies that the second priority tier
// is assigned when active pods exceed the first tier's threshold.
func TestProvisioner_PriorityTiersSecondTier(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	rg := newRG("mygroup", "team-a")
	rg.Spec.PriorityTiers = []provisioner.TierThreshold{
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
				Labels:    map[string]string{provisioner.LabelRunnerSet: "mygroup"},
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

	require.NoError(t, rg.provision(ctx, p, "plan-tier2"))

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
}

// TestProvisioner_PriorityTiersBoundary pins the comparison semantics: exactly
// activePods == threshold falls through to the next tier (not the current one).
func TestProvisioner_PriorityTiersBoundary(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	rg := newRG("mygroup", "team-a")
	rg.Spec.PriorityTiers = []provisioner.TierThreshold{
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
				Labels:    map[string]string{provisioner.LabelRunnerSet: "mygroup"},
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

	require.NoError(t, rg.provision(ctx, p, "plan-boundary"))

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
				Labels: map[string]string{provisioner.LabelRunnerSet: "mygroup"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "running-1", Namespace: "team-a",
				Labels: map[string]string{provisioner.LabelRunnerSet: "mygroup"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pending-0", Namespace: "team-a",
				Labels: map[string]string{provisioner.LabelRunnerSet: "mygroup"},
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
	err := rg.provision(ctx, p, "plan-pending-ceil")
	// M3: ceiling is enforced because Pending pods count as active.
	assert.ErrorContains(t, err, "ceiling")

	var podList corev1.PodList
	require.NoError(t, fc.List(ctx, &podList, client.InNamespace("team-a")))
	assert.Len(t, podList.Items, 3, "no new pod should be created when Pending pods fill the ceiling")
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

	require.NoError(t, rg.provision(ctx, p, "plan-proxy"))

	pod := waitForPodCreated(ctx, t, fc, "team-a")

	envMap := make(map[string]string)
	for _, e := range pod.Spec.Containers[0].Env {
		envMap[e.Name] = e.Value
	}
	assert.Equal(t, "http://proxy.example.com:8080", envMap["HTTP_PROXY"])
	assert.Equal(t, "http://proxy.example.com:8080", envMap["HTTPS_PROXY"])
	assert.Equal(t, "localhost,127.0.0.1", envMap["NO_PROXY"])
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

	require.NoError(t, rg.provision(ctx, p, "plan-proxy-ca"))

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

	require.NoError(t, rg.provision(ctx, p, "plan-no-proxy-ca"))

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

	require.NoError(t, rg.provision(ctx, p, "plan-overwrite"))

	pod := waitForPodCreated(ctx, t, fc, "team-a")

	envMap := make(map[string]string)
	for _, e := range pod.Spec.Containers[0].Env {
		envMap[e.Name] = e.Value
	}
	assert.Equal(t, "http://real-proxy.example.com:8080", envMap["HTTP_PROXY"], "tenant HTTP_PROXY must be overwritten")
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

	require.NoError(t, rg.provision(ctx, p, "plan-quota-ok"))

	// Pod appears after the first (failed) attempt retries.
	waitForPodCreated(ctx, t, fc, "team-a")

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
	p.MaxQuotaRetries = 2
	p.QuotaRetryDelay = 1 * time.Millisecond

	rg := newRG("mygroup", "team-a")
	rg.events = rec

	err := rg.provision(ctx, p, "plan-quota-exhaust")
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

	zero := int32(0)
	rg := newRG("mygroup", "team-a")
	rg.events = rec
	rg.Spec.MaxQuotaRetries = &zero

	err := rg.provision(ctx, p, "plan-quota-disabled")
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

	err := rg.provision(ctx, p, "plan-nonquota")
	assert.Error(t, err)

	// No quota retries attempted.
	assert.Equal(t, float64(0), testutil.ToFloat64(m.QuotaRetries.WithLabelValues("team-a", "mygroup")))
	assert.Equal(t, float64(0), testutil.ToFloat64(m.QuotaRetriesExhausted.WithLabelValues("team-a", "mygroup")))
}

// runAndGetPod provisions one worker for rg the way the scale-set listener does and
// returns the created pod.
func runAndGetPod(ctx context.Context, t *testing.T, p *provisioner.Provisioner, fc client.Client, rg *testGroup, planID, ns string) *corev1.Pod {
	t.Helper()
	require.NoError(t, rg.provision(ctx, p, planID))
	return waitForPodCreated(ctx, t, fc, ns)
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
// carry the target's controller OwnerReference, so deleting the owner (or its
// namespace) cascade-deletes both — including objects
// orphaned by an AGC crash (Q95).
func TestProvisioner_SetsOwnerReferences(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")
	rg.UID = types.UID("rg-uid-123")

	require.NoError(t, rg.provision(ctx, p, "plan-ownerref"))

	// The worker Secret is handed to its pod once the pod exists (Q1154), so only
	// the pod carries the RunnerSet reference.
	pod := waitForPodCreated(ctx, t, fc, "team-a")

	for name, refs := range map[string][]metav1.OwnerReference{
		"pod": pod.OwnerReferences,
	} {
		require.Len(t, refs, 1, "%s must have exactly one ownerReference", name)
		ref := refs[0]
		assert.Equal(t, "actions-gateway.com/v2alpha1", ref.APIVersion, name)
		assert.Equal(t, "RunnerSet", ref.Kind, name)
		assert.Equal(t, "mygroup", ref.Name, name)
		assert.Equal(t, types.UID("rg-uid-123"), ref.UID, name)
		require.NotNil(t, ref.Controller, name)
		assert.True(t, *ref.Controller, "%s ownerReference must be a controller ref", name)
		assert.Nil(t, ref.BlockOwnerDeletion,
			"%s must not set blockOwnerDeletion (needs finalizer perms under OwnerReferencesPermissionEnforcement)", name)
	}
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
