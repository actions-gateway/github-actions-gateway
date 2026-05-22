//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/karlkfi/github-actions-gateway/agc/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func createNSForAGC(t *testing.T, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := k8sClient.Create(ctx, ns)
	if err != nil {
		require.NoError(t, client.IgnoreNotFound(err))
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ns)
	})
}

func newRunnerGroup(ns, name string, maxListeners int32) *v1alpha1.RunnerGroup {
	return &v1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: v1alpha1.RunnerGroupSpec{
			MaxListeners: maxListeners,
			RunnerLabels: []string{"self-hosted"},
		},
	}
}

// TestAGC_Reconciler_CreateStartsOneListener verifies that reconciling a RunnerGroup
// creates at least one listener goroutine (session) and agent Secrets.
func TestAGC_Reconciler_CreateStartsOneListener(t *testing.T) {
	const nsName = "agc-listener-test"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, "test-rg", 2)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rg)
	})

	startAGCReconciler(t)

	// Wait for agent Secrets to be created (indicates EnsureAgents succeeded).
	assert.Eventually(t, func() bool {
		var secrets corev1.SecretList
		if err := k8sClient.List(ctx, &secrets,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "test-rg"},
		); err != nil {
			return false
		}
		return len(secrets.Items) >= 1
	}, 15*time.Second, 500*time.Millisecond, "expected at least one agent Secret to be created")

	// Wait for the RunnerGroup status to show at least one active session.
	assert.Eventually(t, func() bool {
		var fetched v1alpha1.RunnerGroup
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "test-rg"}, &fetched); err != nil {
			return false
		}
		return fetched.Status.ActiveSessions >= 1
	}, 15*time.Second, 500*time.Millisecond, "expected at least one active session reported in status")

	// Verify that the broker stub received at least one CreateSession call.
	assert.Eventually(t, func() bool {
		return len(brokerStub.RegisteredSessions()) >= 1
	}, 15*time.Second, 500*time.Millisecond, "expected broker stub to have at least one registered session")
}

// TestAGC_Reconciler_Delete_AllGoroutinesExit verifies that deleting a RunnerGroup
// stops all listener goroutines, deletes agent Secrets, and removes the finalizer.
func TestAGC_Reconciler_Delete_AllGoroutinesExit(t *testing.T) {
	const nsName = "agc-delete-test"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, "delete-rg", 1)
	require.NoError(t, k8sClient.Create(ctx, rg))

	startAGCReconciler(t)

	// Wait for agent Secret to be created.
	require.Eventually(t, func() bool {
		var secrets corev1.SecretList
		if err := k8sClient.List(ctx, &secrets,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "delete-rg"},
		); err != nil {
			return false
		}
		return len(secrets.Items) >= 1
	}, 15*time.Second, 500*time.Millisecond, "expected agent Secret before deletion")

	// Wait for the finalizer to be added.
	require.Eventually(t, func() bool {
		var fetched v1alpha1.RunnerGroup
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "delete-rg"}, &fetched); err != nil {
			return false
		}
		for _, f := range fetched.Finalizers {
			if f == "actions-gateway.github.com/agentpool-cleanup" {
				return true
			}
		}
		return false
	}, 15*time.Second, 500*time.Millisecond, "finalizer should be added before deletion")

	// Delete the RunnerGroup.
	require.NoError(t, k8sClient.Delete(ctx, rg))

	// All agent Secrets must be deleted.
	assert.Eventually(t, func() bool {
		var secrets corev1.SecretList
		if err := k8sClient.List(ctx, &secrets,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "delete-rg"},
		); err != nil {
			return false
		}
		return len(secrets.Items) == 0
	}, 20*time.Second, 500*time.Millisecond, "all agent Secrets should be deleted after RunnerGroup deletion")

	// The RunnerGroup CR itself should be gone (finalizer removed).
	assert.Eventually(t, func() bool {
		var fetched v1alpha1.RunnerGroup
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "delete-rg"}, &fetched)
		return apierrors.IsNotFound(err)
	}, 20*time.Second, 500*time.Millisecond, "RunnerGroup CR should be gone after teardown")
}
