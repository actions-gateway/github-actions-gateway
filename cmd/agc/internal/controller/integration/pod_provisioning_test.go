//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	agcnames "github.com/actions-gateway/github-actions-gateway/agc/names"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// waitForWorkerPod polls until a Pod named "runner-*" appears in nsName with the
// given runner-group label, then returns it. Fails the test on timeout.
func waitForWorkerPod(t *testing.T, nsName, rgName string) corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": rgName},
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
	}, 20*time.Second, 50*time.Millisecond, "worker Pod should be created in %s", nsName)
	return pod
}

func TestAGC_PodProvisioning_CorrectSpec(t *testing.T) {
	const nsName = "agc-pod-spec"
	createNSForAGC(t, nsName)

	rg := &v1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-spec-rg", Namespace: nsName},
		Spec: v1alpha1.RunnerGroupSpec{
			MaxListeners: 2,
			RunnerLabels: []string{"self-hosted"},
			WorkerImage:  "custom-runner:v1.2.3",
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "runner",
						Image: "custom-runner:v1.2.3",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
					}},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	startAGCReconcilerWithProvisioner(t, provisionerOptions{})

	// Wait for this RunnerGroup's session, then enqueue a job. Scope to the owner
	// so a session another test left active on the shared stub is never picked.
	id := enqueueJobOnOwnerSession(15*time.Second, "pod-spec-rg", nil, broker.RunnerJobRequestBody{})
	require.NotEmpty(t, id, "a session for pod-spec-rg should register")

	pod := waitForWorkerPod(t, nsName, "pod-spec-rg")

	// Assert controller-enforced invariants.
	require.NotNil(t, pod.Spec.AutomountServiceAccountToken)
	assert.False(t, *pod.Spec.AutomountServiceAccountToken,
		"automountServiceAccountToken must be false")
	assert.Equal(t, agcnames.WorkerSAName, pod.Spec.ServiceAccountName)
	assert.False(t, pod.Spec.HostPID, "hostPID must be false")
	assert.False(t, pod.Spec.HostNetwork, "hostNetwork must be false")
	assert.False(t, pod.Spec.HostIPC, "hostIPC must be false")

	// Assert proxy env vars are injected.
	var runnerContainer *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "runner" {
			runnerContainer = &pod.Spec.Containers[i]
			break
		}
	}
	require.NotNil(t, runnerContainer, "runner container must exist in pod spec")

	envNames := map[string]bool{}
	for _, e := range runnerContainer.Env {
		envNames[e.Name] = true
	}
	assert.True(t, envNames["HTTP_PROXY"], "HTTP_PROXY must be injected")
	assert.True(t, envNames["HTTPS_PROXY"], "HTTPS_PROXY must be injected")
	assert.True(t, envNames["NO_PROXY"], "NO_PROXY must be injected")
}

func TestAGC_PodProvisioning_PriorityTiers(t *testing.T) {
	const nsName = "agc-pod-priority"
	createNSForAGC(t, nsName)

	// Create PriorityClass objects required for the tier test.
	for _, pc := range []struct {
		name  string
		value int32
	}{
		{"critical-test", 1000},
		{"standard-test", 100},
	} {
		pcObj := &schedulingv1.PriorityClass{
			ObjectMeta: metav1.ObjectMeta{Name: pc.name},
			Value:      pc.value,
		}
		err := k8sClient.Create(ctx, pcObj)
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			require.NoError(t, err, "failed to create PriorityClass %q", pc.name)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pcObj) })
	}

	rg := &v1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "priority-rg", Namespace: nsName},
		Spec: v1alpha1.RunnerGroupSpec{
			MaxListeners: 5,
			RunnerLabels: []string{"self-hosted"},
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}}},
			},
			PriorityTiers: []v1alpha1.PriorityTier{
				{PriorityClassName: "critical-test", Threshold: 2},
				{PriorityClassName: "standard-test", Threshold: 5},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	startAGCReconcilerWithProvisioner(t, provisionerOptions{})

	seen := map[string]bool{}

	// Enqueue 2 jobs — both pods should get "critical-test" PriorityClass. Scope to
	// this RunnerGroup's owner so each job lands on a priority-rg session.
	for i := 0; i < 2; i++ {
		id := enqueueJobOnOwnerSession(15*time.Second, "priority-rg", seen, broker.RunnerJobRequestBody{})
		require.NotEmpty(t, id, "a new priority-rg session should register")
		seen[id] = true
	}

	// Wait for 2 pods to be created.
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		_ = k8sClient.List(ctx, &pods,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "priority-rg"},
		)
		count := 0
		for _, p := range pods.Items {
			if strings.HasPrefix(p.Name, "runner-") {
				count++
			}
		}
		return count >= 2
	}, 20*time.Second, 50*time.Millisecond, "2 worker pods should be created")

	var pods corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &pods,
		client.InNamespace(nsName),
		client.MatchingLabels{"actions-gateway/runner-group": "priority-rg"},
	))

	var workerPods []corev1.Pod
	for _, p := range pods.Items {
		if strings.HasPrefix(p.Name, "runner-") {
			workerPods = append(workerPods, p)
		}
	}

	for _, p := range workerPods {
		assert.Equal(t, "critical-test", p.Spec.PriorityClassName,
			"pods 1 and 2 should have critical-test PriorityClass (threshold=2)")
	}
}

func TestAGC_PodProvisioning_MaxWorkersCeiling(t *testing.T) {
	const nsName = "agc-pod-ceiling"
	createNSForAGC(t, nsName)

	maxWorkers := int32(2)
	rg := &v1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "ceiling-rg", Namespace: nsName},
		Spec: v1alpha1.RunnerGroupSpec{
			MaxListeners: 3,
			RunnerLabels: []string{"self-hosted"},
			MaxWorkers:   &maxWorkers,
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}}},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	// Pre-create 2 pods with Pending status to saturate the maxWorkers=2 ceiling.
	// This simulates active jobs already occupying the slots before the reconciler starts.
	for i := 0; i < 2; i++ {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "preexisting-worker-" + string(rune('0'+i)),
				Namespace: nsName,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": agcnames.ControllerName,
					"actions-gateway/runner-group": "ceiling-rg",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}},
				// RestartPolicy must be set for standalone pods.
				RestartPolicy: corev1.RestartPolicyNever,
			},
		}
		require.NoError(t, k8sClient.Create(ctx, pod))
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pod) })

		// Set status to Pending so activePodCount counts it.
		pod.Status.Phase = corev1.PodPending
		require.NoError(t, k8sClient.Status().Update(ctx, pod))
	}

	startAGCReconcilerWithProvisioner(t, provisionerOptions{})

	acquiresBefore := brokerStub.AcquireJobCalls()
	// Scope to this RunnerGroup's owner so we never enqueue onto a session another
	// test left active on the shared stub.
	id := enqueueJobOnOwnerSession(15*time.Second, "ceiling-rg", nil, broker.RunnerJobRequestBody{})
	require.NotEmpty(t, id, "a session for ceiling-rg should register")

	// The provisioner must acquire the job, then back off due to the ceiling
	// without creating a worker pod. We assert on the monotonic acquire-job
	// counter rather than trying to catch the transient job Secret: the ceiling
	// path creates and immediately deletes that Secret within a single provision
	// call, so observing it is racy — especially now the controller's worker-Pod
	// watch pre-warms the Pod informer, which makes activePodCount's List return
	// instantly and shrinks the Secret's lifetime below the poll interval.
	require.Eventually(t, func() bool {
		return brokerStub.AcquireJobCalls() > acquiresBefore
	}, 10*time.Second, 25*time.Millisecond, "provisioner should acquire the job")

	require.Eventually(t, func() bool {
		var secrets corev1.SecretList
		_ = k8sClient.List(ctx, &secrets,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "ceiling-rg"},
		)
		for _, s := range secrets.Items {
			if strings.HasPrefix(s.Name, "job-") {
				return false
			}
		}
		return true
	}, 10*time.Second, 25*time.Millisecond, "provisioner should delete job Secret after ceiling check")

	// Assert: only the 2 pre-existing pods exist; no new "runner-*" pods were created.
	var pods corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &pods,
		client.InNamespace(nsName),
		client.MatchingLabels{"actions-gateway/runner-group": "ceiling-rg"},
	))
	newPods := 0
	for _, p := range pods.Items {
		if strings.HasPrefix(p.Name, "runner-") {
			newPods++
		}
	}
	assert.Equal(t, 0, newPods,
		"no new runner pods should be created when maxWorkers ceiling is reached")

	// Ceiling-lifts: advance one pre-existing pod to Succeeded to free a slot.
	var preexisting corev1.Pod
	require.NoError(t, k8sClient.Get(ctx,
		client.ObjectKey{Namespace: nsName, Name: "preexisting-worker-0"}, &preexisting))
	preexisting.Status.Phase = corev1.PodSucceeded
	require.NoError(t, k8sClient.Status().Update(ctx, &preexisting))

	// Enqueue a second job now that active pod count dropped to 1 (< maxWorkers=2).
	// The provisioner drops ceiling-blocked jobs, so we enqueue a fresh job. Scope
	// to this RunnerGroup's owner to avoid another test's lingering session.
	id = enqueueJobOnOwnerSession(15*time.Second, "ceiling-rg", nil, broker.RunnerJobRequestBody{})
	require.NotEmpty(t, id, "a session for ceiling-rg should still be active")

	// A new runner pod should now be created since the ceiling is no longer saturated.
	assert.Eventually(t, func() bool {
		var pl corev1.PodList
		_ = k8sClient.List(ctx, &pl,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "ceiling-rg"},
		)
		for _, p := range pl.Items {
			if strings.HasPrefix(p.Name, "runner-") {
				return true
			}
		}
		return false
	}, 20*time.Second, 50*time.Millisecond,
		"a runner pod should be created once the maxWorkers ceiling is lifted")
}
