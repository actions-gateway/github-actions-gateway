//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// buildJobPayload creates a JSON-encoded AcquireJob-style payload with owner/repo/runID.
// The provisioner unmarshals this from the payload bytes to extract eviction retry info.
func buildJobPayload(owner, repo string, runID int64) []byte {
	b, _ := json.Marshal(map[string]interface{}{
		"run_id": runID,
		"variables": map[string]interface{}{
			"system.github.repository": map[string]string{"value": owner + "/" + repo},
			"system.github.run_id":     map[string]string{"value": "12345"},
		},
	})
	return b
}

// TestAGC_FailureRecovery_PodCrash_NoSecretLeak verifies that when a worker pod
// fails (non-eviction), the provisioner deletes the job Secret without leaking it
// and does not trigger an auto-rerun.
func TestAGC_FailureRecovery_PodCrash_NoSecretLeak(t *testing.T) {
	const nsName = "agc-crash-recovery"
	createNSForAGC(t, nsName)

	// Fake GitHub API to detect any unexpected rerun calls.
	var rerunCalls atomic.Int64
	fakeGitHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "rerun-failed-jobs") {
			rerunCalls.Add(1)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer fakeGitHub.Close()

	rg := newRunnerGroup(nsName, "crash-rg", 2)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rg) })

	startAGCReconcilerWithProvisioner(t, provisionerOptions{
		githubAPIURL:       fakeGitHub.URL,
		maxEvictionRetries: 1,
	})

	// Enqueue a job on crash-rg's own session. Scoping the enqueue to this
	// RunnerGroup's owner (via ActiveSessionsForOwner) rather than picking the last
	// entry of the global RegisteredSessions list keeps the job from landing on a
	// session another test left active on the shared broker stub — the cross-test
	// contention that flaked these tests (Q113). enqueueJobOnOwnerSession also
	// retries until an owner session is present, so it is immune to the picked
	// session having just idle-shut.
	require.Eventually(t, func() bool {
		return len(brokerStub.ActiveSessionsForOwner("crash-rg")) >= 1
	}, 15*time.Second, 1*time.Millisecond, "crash-rg session should register")
	sid := enqueueJobOnOwnerSession(15*time.Second, "crash-rg", map[string]bool{}, broker.RunnerJobRequestBody{})
	require.NotEmpty(t, sid, "should have found crash-rg session to enqueue on")

	// Wait for a worker pod to appear.
	pod := waitForWorkerPod(t, nsName, "crash-rg")

	// Simulate a non-eviction pod failure.
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Reason = "" // not "Evicted"
	require.NoError(t, k8sClient.Status().Update(ctx, &pod))

	// The provisioner should clean up the job Secret.
	assert.Eventually(t, func() bool {
		var secrets corev1.SecretList
		_ = k8sClient.List(ctx, &secrets,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "crash-rg"},
		)
		for _, s := range secrets.Items {
			if strings.HasPrefix(s.Name, "job-") {
				return false // still exists
			}
		}
		return true
	}, 20*time.Second, 50*time.Millisecond, "job Secret should be deleted after pod failure")

	// No rerun API call should have been made (non-eviction crash).
	assert.Equal(t, int64(0), rerunCalls.Load(),
		"rerun API must not be called for a non-eviction pod failure")
}

// TestAGC_FailureRecovery_EvictionTriggersRequeue verifies that when a worker pod is
// evicted, the provisioner calls the GitHub rerun API and then deletes the job Secret.
func TestAGC_FailureRecovery_EvictionTriggersRequeue(t *testing.T) {
	const nsName = "agc-eviction-recovery"
	createNSForAGC(t, nsName)

	// Fake GitHub API to capture rerun calls.
	var rerunCalls atomic.Int64
	fakeGitHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "rerun-failed-jobs") {
			rerunCalls.Add(1)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer fakeGitHub.Close()

	rg := newRunnerGroup(nsName, "eviction-rg", 2)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rg) })

	startAGCReconcilerWithProvisioner(t, provisionerOptions{
		githubAPIURL:       fakeGitHub.URL,
		maxEvictionRetries: 1,
	})

	// Wait for eviction-rg's own session (owner-scoped, not the global session
	// list — see crash-rg above, Q113), then set the AcquireJob response before
	// enqueuing so the provisioner reads the eviction retry info on pickup.
	require.Eventually(t, func() bool {
		return len(brokerStub.ActiveSessionsForOwner("eviction-rg")) >= 1
	}, 15*time.Second, 1*time.Millisecond, "eviction-rg session should register")

	// The job body in the broker will come back via AcquireJob; the payload to the
	// provisioner is the raw AcquireJobResponse from the broker fake. To embed the
	// runID, we set the AcquireJob response to include eviction retry info.
	brokerStub.SetAcquireJobResponse(map[string]interface{}{
		"plan":   map[string]string{"planId": "evict-plan-1"},
		"run_id": 12345,
		"variables": map[string]interface{}{
			"system.github.repository": map[string]string{"value": "owner/repo"},
			"system.github.run_id":     map[string]string{"value": "12345"},
		},
	})
	t.Cleanup(func() { brokerStub.SetAcquireJobResponse(nil) })
	sid := enqueueJobOnOwnerSession(15*time.Second, "eviction-rg", map[string]bool{}, broker.RunnerJobRequestBody{})
	require.NotEmpty(t, sid, "should have found eviction-rg session to enqueue on")

	// Wait for the worker pod.
	pod := waitForWorkerPod(t, nsName, "eviction-rg")

	// Simulate eviction.
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Reason = "Evicted"
	require.NoError(t, k8sClient.Status().Update(ctx, &pod))

	// The provisioner should call the rerun API and delete the job Secret.
	assert.Eventually(t, func() bool {
		return rerunCalls.Load() >= 1
	}, 20*time.Second, 50*time.Millisecond,
		"rerun API must be called after pod eviction")

	assert.Eventually(t, func() bool {
		var secrets corev1.SecretList
		_ = k8sClient.List(ctx, &secrets,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "eviction-rg"},
		)
		for _, s := range secrets.Items {
			if strings.HasPrefix(s.Name, "job-") {
				return false
			}
		}
		return true
	}, 20*time.Second, 50*time.Millisecond, "job Secret should be deleted after eviction")
}

// TestAGC_FailureRecovery_EvictionBudgetExhausted verifies that with maxEvictionRetries=0
// the provisioner suppresses the rerun API call.
func TestAGC_FailureRecovery_EvictionBudgetExhausted(t *testing.T) {
	const nsName = "agc-eviction-budget"
	createNSForAGC(t, nsName)

	var rerunCalls atomic.Int64
	fakeGitHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "rerun-failed-jobs") {
			rerunCalls.Add(1)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer fakeGitHub.Close()

	rg := newRunnerGroup(nsName, "budget-rg", 2)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rg) })

	startAGCReconcilerWithProvisioner(t, provisionerOptions{
		githubAPIURL:       fakeGitHub.URL,
		maxEvictionRetries: 0, // budget exhausted immediately
	})

	// Owner-scoped session wait + enqueue (see crash-rg / eviction-rg above, Q113).
	require.Eventually(t, func() bool {
		return len(brokerStub.ActiveSessionsForOwner("budget-rg")) >= 1
	}, 15*time.Second, 1*time.Millisecond, "budget-rg session should register")

	brokerStub.SetAcquireJobResponse(map[string]interface{}{
		"plan":   map[string]string{"planId": "budget-plan-1"},
		"run_id": 99999,
		"variables": map[string]interface{}{
			"system.github.repository": map[string]string{"value": "owner/repo"},
			"system.github.run_id":     map[string]string{"value": "99999"},
		},
	})
	t.Cleanup(func() { brokerStub.SetAcquireJobResponse(nil) })
	sid := enqueueJobOnOwnerSession(15*time.Second, "budget-rg", map[string]bool{}, broker.RunnerJobRequestBody{})
	require.NotEmpty(t, sid, "should have found budget-rg session to enqueue on")

	pod := waitForWorkerPod(t, nsName, "budget-rg")

	// Simulate eviction.
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Reason = "Evicted"
	require.NoError(t, k8sClient.Status().Update(ctx, &pod))

	// Wait for the Secret to be cleaned up (provisioner finished).
	assert.Eventually(t, func() bool {
		var secrets corev1.SecretList
		_ = k8sClient.List(ctx, &secrets,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "budget-rg"},
		)
		for _, s := range secrets.Items {
			if strings.HasPrefix(s.Name, "job-") {
				return false
			}
		}
		return true
	}, 20*time.Second, 50*time.Millisecond, "job Secret should be deleted when budget is exhausted")

	// With maxEvictionRetries=0, the rerun API must NOT be called.
	assert.Equal(t, int64(0), rerunCalls.Load(),
		"rerun API must not be called when maxEvictionRetries=0")
}
