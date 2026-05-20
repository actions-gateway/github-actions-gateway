package provisioner_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"github.com/karlkfi/github-actions-gateway/agc/api/v1alpha1"
	"github.com/karlkfi/github-actions-gateway/agc/internal/listener"
	"github.com/karlkfi/github-actions-gateway/agc/internal/provisioner"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	}
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
			"system.github.run_id":   map[string]interface{}{"value": fmt.Sprintf("%d", runID)},
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
		done <- p.HandlerFor(rg)(ctx, "http://run-svc", "plan-abc-123", payload)
	}()

	// Wait for the pod to appear, then complete it.
	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)
	assert.Equal(t, "actions-gateway-agc", pod.Labels["app.kubernetes.io/managed-by"])
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

func TestProvisioner_DeletesSecretOnCompletion(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")

	done := make(chan error, 1)
	go func() {
		done <- p.HandlerFor(rg)(ctx, "", "plan-del", stubPayload(1))
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
	err := p.HandlerFor(rg)(ctx, "", "plan-hold", stubPayload(1))
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
		done <- p.HandlerFor(rg)(ctx, "", "plan-tier", stubPayload(1))
	}()

	require.Eventually(t, func() bool {
		return findPod(ctx, t, fc, "team-a") != nil
	}, 2*time.Second, 5*time.Millisecond)

	// Find the newly created pod (not one of existing-0..2).
	var list corev1.PodList
	require.NoError(t, fc.List(ctx, &list, client.InNamespace("team-a")))
	var newPod *corev1.Pod
	for i := range list.Items {
		name := list.Items[i].Name
		if name != "existing-0" && name != "existing-1" && name != "existing-2" {
			newPod = &list.Items[i]
			break
		}
	}
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
	err := p.HandlerFor(rg)(ctx, "", "plan-ceil", stubPayload(1))
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
		done <- p.HandlerFor(rg)(ctx, "", "plan-img", stubPayload(1))
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
		done <- p.HandlerFor(rg)(ctx, "", "plan-reserved", stubPayload(1))
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

func TestProvisioner_SecretCleanupOnPodCreateFailure(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	// Use a scheme that doesn't include Pods so pod creation returns an error.
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	// Register RunnerGroup but NOT pods explicitly — fake client still handles
	// core types, so we need a different approach: create a pod that conflicts.
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")

	// Pre-create a pod with the name that the provisioner will try to use,
	// causing a conflict on creation.
	planID := "plan-conflict"
	safePlan := "plan-conflict"
	podName := fmt.Sprintf("runner-%s-%s", "mygroup", safePlan)
	if len(podName) > 63 {
		podName = podName[:63]
	}
	conflictPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: "team-a"},
	}
	require.NoError(t, fc.Create(ctx, conflictPod))

	err := p.HandlerFor(rg)(ctx, "", planID, stubPayload(1))
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
		done <- p.HandlerFor(rg)(ctx, "", "plan-cancel", stubPayload(1))
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
		done <- p.HandlerFor(rg)(ctx, "", "PLAN/ID:with:COLONS/and/SLASHES", stubPayload(1))
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
		done <- p.HandlerFor(rg)(ctx, "", "plan-mount", stubPayload(1))
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
		done <- p.HandlerFor(rg)(ctx, "", "plan-evict", payload)
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
	runCycle := func(planID string) {
		t.Helper()
		// Mirror the pod name formula from provisioner.buildPod so we can target
		// the exact pod rather than relying on findPod (which may return the old
		// already-evicted pod from the prior cycle).
		podName := fmt.Sprintf("runner-%s-%s", "mygroup", planID)
		if len(podName) > 63 {
			podName = podName[:63]
		}

		done := make(chan error, 1)
		go func() { done <- p.HandlerFor(rg)(ctx, "", planID, payload) }()

		require.Eventually(t, func() bool {
			var pod corev1.Pod
			return fc.Get(ctx, types.NamespacedName{Namespace: "ns", Name: podName}, &pod) == nil
		}, 2*time.Second, 5*time.Millisecond)
		evictPod(ctx, t, fc, "ns", podName)
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
	go func() { done <- p.HandlerFor(rg)(ctx, "", "plan-5xx", payload) }()

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
	go func() { done <- p.HandlerFor(rg)(ctx, "", "plan-tier2", stubPayload(1)) }()

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
	go func() { done <- p.HandlerFor(rg)(ctx, "", "plan-boundary", stubPayload(1)) }()

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
	err := p.HandlerFor(rg)(ctx, "", "plan-pending-ceil", stubPayload(1))
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
	go func() { done <- p.HandlerFor(rg)(ctx, "", "plan-extdel", payload) }()

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

