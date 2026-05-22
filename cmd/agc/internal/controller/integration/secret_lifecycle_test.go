//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/karlkfi/github-actions-gateway/broker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestAGC_SecretLifecycle_CreatedOnJobAcquire verifies that acquiring a job through
// the fake broker causes the provisioner to create a job Secret in the namespace.
func TestAGC_SecretLifecycle_CreatedOnJobAcquire(t *testing.T) {
	const nsName = "agc-secret-create"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, "secret-create-rg", 2)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	startAGCReconcilerWithProvisioner(t, provisionerOptions{})

	// Wait for at least one session to register (listener is ready to receive jobs).
	var sessions []string
	require.Eventually(t, func() bool {
		sessions = brokerStub.RegisteredSessions()
		return len(sessions) >= 1
	}, 15*time.Second, 200*time.Millisecond, "a session must register before we can enqueue a job")

	// Enqueue a job — the provisioner will create a Secret and then a Pod.
	brokerStub.EnqueueJob(sessions[len(sessions)-1], broker.RunnerJobRequestBody{})

	// Assert a job Secret is created in the namespace.
	assert.Eventually(t, func() bool {
		var secrets corev1.SecretList
		if err := k8sClient.List(ctx, &secrets,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "secret-create-rg"},
		); err != nil {
			return false
		}
		for _, s := range secrets.Items {
			if strings.HasPrefix(s.Name, "job-") {
				return len(s.Data["payload"]) > 0
			}
		}
		return false
	}, 20*time.Second, 200*time.Millisecond,
		"a job Secret with non-empty payload must be created after job acquisition")
}

// TestAGC_SecretLifecycle_DeletedAfterPodCompletes verifies that the provisioner
// deletes the job Secret once the worker pod transitions to Succeeded.
func TestAGC_SecretLifecycle_DeletedAfterPodCompletes(t *testing.T) {
	const nsName = "agc-secret-delete"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, "secret-delete-rg", 2)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	startAGCReconcilerWithProvisioner(t, provisionerOptions{})

	// Wait for a session, then enqueue a job.
	var sessions []string
	require.Eventually(t, func() bool {
		sessions = brokerStub.RegisteredSessions()
		return len(sessions) >= 1
	}, 15*time.Second, 200*time.Millisecond)

	brokerStub.EnqueueJob(sessions[len(sessions)-1], broker.RunnerJobRequestBody{})

	// Wait for the job Secret and worker Pod to be created.
	var jobSecretName string
	require.Eventually(t, func() bool {
		var secrets corev1.SecretList
		if err := k8sClient.List(ctx, &secrets,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "secret-delete-rg"},
		); err != nil {
			return false
		}
		for _, s := range secrets.Items {
			if strings.HasPrefix(s.Name, "job-") {
				jobSecretName = s.Name
				return true
			}
		}
		return false
	}, 20*time.Second, 200*time.Millisecond, "job Secret should be created")

	// Wait for the worker Pod to be created.
	var podName string
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "secret-delete-rg"},
		); err != nil {
			return false
		}
		for _, p := range pods.Items {
			if strings.HasPrefix(p.Name, "runner-") {
				podName = p.Name
				return true
			}
		}
		return false
	}, 20*time.Second, 200*time.Millisecond, "worker Pod should be created")

	// Advance the Pod to Succeeded (envtest has no kubelet to do this automatically).
	var pod corev1.Pod
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: podName}, &pod))
	pod.Status.Phase = corev1.PodSucceeded
	require.NoError(t, k8sClient.Status().Update(ctx, &pod))

	// Assert the job Secret is deleted after pod completion.
	assert.Eventually(t, func() bool {
		var s corev1.Secret
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: jobSecretName}, &s)
		return err != nil // deleted when Get returns an error
	}, 20*time.Second, 200*time.Millisecond,
		"job Secret %q should be deleted after pod Succeeded", jobSecretName)

	// No orphaned job Secrets should remain.
	var remaining corev1.SecretList
	require.NoError(t, k8sClient.List(ctx, &remaining,
		client.InNamespace(nsName),
		client.MatchingLabels{"actions-gateway/runner-group": "secret-delete-rg"},
	))
	for _, s := range remaining.Items {
		assert.False(t, strings.HasPrefix(s.Name, "job-"),
			"no orphaned job Secrets should remain; found %q", s.Name)
	}

}
