package provisioner_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	agcnames "github.com/actions-gateway/github-actions-gateway/agc/names"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newTestMetrics builds a Metrics with unregistered counters/histograms safe
// for per-test use (not added to the global Prometheus registry).
func newTestMetrics() *listener.Metrics {
	return &listener.Metrics{
		JobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "t_prov_job_duration_seconds",
		}, []string{"namespace", "runner_group"}),
		EvictionRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_prov_eviction_retries_total",
		}, []string{"namespace", "runner_group"}),
		EvictionRetriesExhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_prov_eviction_retries_exhausted_total",
		}, []string{"namespace", "runner_group"}),
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

// stubPayloadFull returns a payload with variables matching the GitHub Actions format,
// including owner/repo for eviction retry.
func stubPayloadFull(owner, repo string, runID int64) []byte {
	b, _ := json.Marshal(map[string]interface{}{
		"variables": map[string]interface{}{
			"system.github.run_id":     map[string]interface{}{"value": fmt.Sprintf("%d", runID)},
			"system.github.repository": map[string]interface{}{"value": owner + "/" + repo},
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
		done <- p.HandlerFor(rg)(ctx, "http://run-svc", "plan-abc-123", payload, "")
	}()

	// Wait for the pod to appear, then complete it.
	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)
	assert.Equal(t, agcnames.ControllerName, pod.Labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, "mygroup", pod.Labels["actions-gateway/runner-group"])

	secret := findSecret(ctx, t, fc, "team-a", "job-")
	require.NotNil(t, secret)
	assert.Equal(t, payload, secret.Data["payload"])
	assert.Equal(t, []byte("plan-abc-123"), secret.Data["plan-id"])

	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)

	// H1: JobDuration must have been observed after pod completion.
	assert.Equal(t, 1, testutil.CollectAndCount(m.JobDuration), "JobDuration histogram should have one observation")
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
		done <- p.HandlerFor(rg)(ctx, "", "plan-jit", stubPayload(7), jitBlob)
	}()

	require.Eventually(t, func() bool {
		return findSecret(ctx, t, fc, "team-a", "job-") != nil
	}, 2*time.Second, 5*time.Millisecond)

	secret := findSecret(ctx, t, fc, "team-a", "job-")
	require.NotNil(t, secret)
	assert.Equal(t, []byte(jitBlob), secret.Data["jitconfig"],
		"worker Secret must carry the JIT blob under the 'jitconfig' key")

	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)
	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
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
		done <- p.HandlerFor(rg)(ctx, "", "plan-nojit", stubPayload(8), "")
	}()

	require.Eventually(t, func() bool {
		return findSecret(ctx, t, fc, "team-a", "job-") != nil
	}, 2*time.Second, 5*time.Millisecond)

	secret := findSecret(ctx, t, fc, "team-a", "job-")
	require.NotNil(t, secret)
	_, present := secret.Data["jitconfig"]
	assert.False(t, present, "jitconfig key must be absent when no blob was provided")

	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)
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
		done <- p.HandlerFor(rg)(ctx, "", "plan-del", stubPayload(1), "")
	}()

	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

	pod := findPod(ctx, t, fc, "team-a")
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
	err := p.HandlerFor(rg)(ctx, "", "plan-hold", stubPayload(1), "")
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
		done <- p.HandlerFor(rg)(ctx, "", "plan-tier", stubPayload(1), "")
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
	err := p.HandlerFor(rg)(ctx, "", "plan-ceil", stubPayload(1), "")
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
		done <- p.HandlerFor(rg)(ctx, "", "plan-img", stubPayload(1), "")
	}()

	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)
	var runnerContainer *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "runner" {
			runnerContainer = &pod.Spec.Containers[i]
			break
		}
	}
	require.NotNil(t, runnerContainer)
	assert.Equal(t, "my-custom-image:latest", runnerContainer.Image)

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
		done <- p.HandlerFor(rg)(ctx, "", "plan-reserved", stubPayload(1), "")
	}()

	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)
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

	err := p.HandlerFor(rg)(ctx, "", "plan-conflict", stubPayload(1), "")
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
		done <- p.HandlerFor(rg)(ctx, "", "plan-cancel", stubPayload(1), "")
	}()

	// Wait for pod to be created, then cancel.
	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

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
		done <- p.HandlerFor(rg)(ctx, "", "PLAN/ID:with:COLONS/and/SLASHES", stubPayload(1), "")
	}()

	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)

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
		done <- p.HandlerFor(rg)(ctx, "", "plan-mount", stubPayload(1), "")
	}()

	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)

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
	var runner *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "runner" {
			runner = &pod.Spec.Containers[i]
			break
		}
	}
	require.NotNil(t, runner)

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
		done <- p.HandlerFor(rg)(ctx, "", "plan-evict", payload, "")
	}()

	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

	pod := findPod(ctx, t, fc, "team-a")
	evictPod(ctx, t, fc, "team-a", pod.Name)

	require.NoError(t, <-done)

	select {
	case path := <-rerunCalled:
		assert.Equal(t, "/repos/myorg/myrepo/actions/runs/99/rerun-failed-jobs", path)
	case <-time.After(2 * time.Second):
		t.Fatal("rerun API was not called within timeout")
	}

	// H1: EvictionRetries counter must be incremented once.
	assert.Equal(t, float64(1), testutil.ToFloat64(m.EvictionRetries.WithLabelValues("team-a", "mygroup")))
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
		go func() { done <- p.HandlerFor(rg)(ctx, "", planID, payload, "") }()

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
	// H5: provision returns only after handleEviction finishes, so these assertions
	// are race-free — no sleep needed.
	runCycle("plan-evict-2")
	assert.Equal(t, float64(1), testutil.ToFloat64(m.EvictionRetriesExhausted.WithLabelValues("ns", "mygroup")))
	assert.Equal(t, float64(1), testutil.ToFloat64(m.EvictionRetries.WithLabelValues("ns", "mygroup")))
	assert.Equal(t, 1, rerunCount, "rerun API should be called exactly once")
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
	go func() { done <- p.HandlerFor(rg)(ctx, "", "plan-5xx", payload, "") }()

	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

	pod := findPod(ctx, t, fc, "team-a")
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
	assert.Equal(t, float64(1), testutil.ToFloat64(m.EvictionRetries.WithLabelValues("team-a", "mygroup")))
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
	go func() { done <- p.HandlerFor(rg)(ctx, "", "plan-tier2", stubPayload(1), "") }()

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
	go func() { done <- p.HandlerFor(rg)(ctx, "", "plan-boundary", stubPayload(1), "") }()

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
	err := p.HandlerFor(rg)(ctx, "", "plan-pending-ceil", stubPayload(1), "")
	// M3: ceiling is enforced because Pending pods count as active.
	assert.ErrorContains(t, err, "ceiling")

	var podList corev1.PodList
	require.NoError(t, fc.List(ctx, &podList, client.InNamespace("team-a")))
	assert.Len(t, podList.Items, 3, "no new pod should be created when Pending pods fill the ceiling")
}

// TestProvisioner_PodDeletedExternallySucceeds verifies that an operator
// manually deleting the pod mid-run is treated as successful completion:
// provision returns nil and the rerun API is not called.
func TestProvisioner_PodDeletedExternallySucceeds(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	// Stub server to detect any unexpected rerun API calls.
	rerunCalls := make(chan struct{}, 5)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rerunCalls <- struct{}{}
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
	go func() { done <- p.HandlerFor(rg)(ctx, "", "plan-extdel", payload, "") }()

	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)

	// Simulate operator deleting the pod externally.
	require.NoError(t, fc.Delete(ctx, pod))

	// M4: provision must return nil (external deletion is treated as success).
	require.NoError(t, <-done)

	// The rerun API must not be called (not-found is not an eviction).
	select {
	case <-rerunCalls:
		t.Fatal("rerun API must not be called when pod is deleted externally")
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
		done <- p.HandlerFor(rg)(ctx, "", "plan-proxy", stubPayload(1), "")
	}()

	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)

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
		done <- p.HandlerFor(rg)(ctx, "", "plan-proxy-ca", stubPayload(1), "")
	}()

	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)

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

	var runner *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "runner" {
			runner = &pod.Spec.Containers[i]
			break
		}
	}
	require.NotNil(t, runner)

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
		done <- p.HandlerFor(rg)(ctx, "", "plan-no-proxy-ca", stubPayload(1), "")
	}()

	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)

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
// system.github.repository values do not reach the GitHub API. The rerun path
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
			go func() { done <- p.HandlerFor(rg)(ctx, "", "plan-adv-repo", payload, "") }()

			require.Eventually(t, func() bool {
				return findPod(ctx, t, fc, "team-a") != nil
			}, 2*time.Second, 5*time.Millisecond)

			pod := findPod(ctx, t, fc, "team-a")
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
		done <- p.HandlerFor(rg)(ctx, "", "plan-overwrite", stubPayload(1), "")
	}()

	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)

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
	go func() { done <- p.HandlerFor(rg)(ctx, "", "plan-zero-retry", payload, "") }()

	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

	pod := findPod(ctx, t, fc, "team-a")
	evictPod(ctx, t, fc, "team-a", pod.Name)
	require.NoError(t, <-done)

	// Rerun API must NOT be called.
	select {
	case <-rerunCalled:
		t.Fatal("rerun API should not be called when maxEvictionRetries=0")
	default:
	}

	// Exhausted counter must increment immediately.
	assert.Equal(t, float64(1), testutil.ToFloat64(m.EvictionRetriesExhausted.WithLabelValues("team-a", "mygroup")))
	assert.Equal(t, float64(0), testutil.ToFloat64(m.EvictionRetries.WithLabelValues("team-a", "mygroup")))
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
		go func() { done <- p.HandlerFor(rg)(ctx, "", planID, payload, "") }()
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
	assert.Equal(t, float64(1), testutil.ToFloat64(m.EvictionRetriesExhausted.WithLabelValues("ns", "mygroup")))
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
	go func() { done <- p.HandlerFor(rg)(ctx, "", "plan-delay", payload, "") }()

	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

	pod := findPod(ctx, t, fc, "team-a")
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
	go func() { done <- p.HandlerFor(rg)(ctx, "", "plan-quota-ok", payload, "") }()

	// Pod appears after the first (failed) attempt retries.
	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

	pod := findPod(ctx, t, fc, "team-a")
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
	p.MaxQuotaRetries = 2
	p.QuotaRetryDelay = 1 * time.Millisecond

	rg := newRG("mygroup", "team-a")

	err := p.HandlerFor(rg)(ctx, "", "plan-quota-exhaust", stubPayload(1), "")
	assert.Error(t, err)

	// No pod created; Secret cleaned up.
	assert.Nil(t, findPod(ctx, t, fc, "team-a"))
	assert.Nil(t, findSecret(ctx, t, fc, "team-a", "job-"))

	// 2 retries attempted (attempts 1 and 2 after the initial failure), then exhausted.
	assert.Equal(t, float64(2), testutil.ToFloat64(m.QuotaRetries.WithLabelValues("team-a", "mygroup")))
	assert.Equal(t, float64(1), testutil.ToFloat64(m.QuotaRetriesExhausted.WithLabelValues("team-a", "mygroup")))
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

	zero := int32(0)
	rg := newRG("mygroup", "team-a")
	rg.Spec.MaxQuotaRetries = &zero

	err := p.HandlerFor(rg)(ctx, "", "plan-quota-disabled", stubPayload(1), "")
	assert.Error(t, err)

	// No pod; no retry counters incremented.
	assert.Nil(t, findPod(ctx, t, fc, "team-a"))
	assert.Equal(t, float64(0), testutil.ToFloat64(m.QuotaRetries.WithLabelValues("team-a", "mygroup")))
	assert.Equal(t, float64(0), testutil.ToFloat64(m.QuotaRetriesExhausted.WithLabelValues("team-a", "mygroup")))
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

	err := p.HandlerFor(rg)(ctx, "", "plan-nonquota", stubPayload(1), "")
	assert.Error(t, err)

	// No quota retries attempted.
	assert.Equal(t, float64(0), testutil.ToFloat64(m.QuotaRetries.WithLabelValues("team-a", "mygroup")))
	assert.Equal(t, float64(0), testutil.ToFloat64(m.QuotaRetriesExhausted.WithLabelValues("team-a", "mygroup")))
}
