//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/karlkfi/github-actions-gateway/agc/api/v1alpha1"
	"github.com/karlkfi/github-actions-gateway/broker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
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
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}},
				},
			},
		},
	}
}

// enqueueJobWhenSessionAvailable blocks until the broker stub has a new session beyond
// those in alreadySeen, then enqueues a job on the first new session ID. Returns the
// new session ID or "" if no new session appeared within the timeout.
func enqueueJobWhenSessionAvailable(timeout time.Duration, alreadySeen map[string]bool, payload broker.RunnerJobRequestBody) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, id := range brokerStub.RegisteredSessions() {
			if !alreadySeen[id] {
				brokerStub.EnqueueJob(id, payload)
				return id
			}
		}
		time.Sleep(1 * time.Millisecond)
	}
	return ""
}

// TestAGC_Reconciler_CreateStartsOneListener verifies that reconciling a RunnerGroup
// creates exactly maxListeners agent Secrets, starts one listener goroutine at rest,
// and surfaces the Ready=true condition.
func TestAGC_Reconciler_CreateStartsOneListener(t *testing.T) {
	const nsName = "agc-listener-test"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, "test-rg", 2)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rg)
	})

	startAGCReconciler(t)

	// Wait for exactly maxListeners (2) agent Secrets to be created.
	assert.Eventually(t, func() bool {
		var secrets corev1.SecretList
		if err := k8sClient.List(ctx, &secrets,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "test-rg"},
		); err != nil {
			return false
		}
		return len(secrets.Items) == 2
	}, 15*time.Second, 50*time.Millisecond, "expected exactly 2 agent Secrets (one per maxListeners)")

	// Wait for the RunnerGroup status to show exactly one active session at rest.
	assert.Eventually(t, func() bool {
		var fetched v1alpha1.RunnerGroup
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "test-rg"}, &fetched); err != nil {
			return false
		}
		return fetched.Status.ActiveSessions == 1
	}, 15*time.Second, 50*time.Millisecond, "expected exactly 1 active session at rest")

	// Wait for Ready=True condition.
	assert.Eventually(t, func() bool {
		var fetched v1alpha1.RunnerGroup
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "test-rg"}, &fetched); err != nil {
			return false
		}
		for _, cond := range fetched.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue {
				return true
			}
		}
		return false
	}, 15*time.Second, 50*time.Millisecond, "expected Ready=True condition on RunnerGroup")

	// Verify that the broker stub received at least one CreateSession call.
	assert.Eventually(t, func() bool {
		return len(brokerStub.RegisteredSessions()) >= 1
	}, 15*time.Second, 1*time.Millisecond, "expected broker stub to have at least one registered session")
}

// TestAGC_Reconciler_Delete_AllGoroutinesExit verifies that deleting a RunnerGroup
// stops all listener goroutines, deletes agent Secrets, removes the finalizer,
// and leaves no goroutine leaks.
func TestAGC_Reconciler_Delete_AllGoroutinesExit(t *testing.T) {
	const nsName = "agc-delete-test"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, "delete-rg", 1)
	require.NoError(t, k8sClient.Create(ctx, rg))

	cancelMgr, mgrDone := startAGCReconciler(t)

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
	}, 15*time.Second, 50*time.Millisecond, "expected agent Secret before deletion")

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
	}, 15*time.Second, 50*time.Millisecond, "finalizer should be added before deletion")

	// Wait for the listener goroutine to reach the poll loop so that deletion
	// is guaranteed to trigger a graceful DeleteSession (not a mid-startup kill).
	sessions := brokerStub.RegisteredSessions()
	for _, sid := range sessions {
		_ = brokerStub.WaitForFirstPoll(sid, 5*time.Second)
	}

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
	}, 20*time.Second, 50*time.Millisecond, "all agent Secrets should be deleted after RunnerGroup deletion")

	// The RunnerGroup CR itself should be gone (finalizer removed).
	assert.Eventually(t, func() bool {
		var fetched v1alpha1.RunnerGroup
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "delete-rg"}, &fetched)
		return apierrors.IsNotFound(err)
	}, 20*time.Second, 50*time.Millisecond, "RunnerGroup CR should be gone after teardown")

	// Stop the manager and verify no goroutine leaks. The multiplexer goroutines
	// already exited during reconcileDelete (mux.Stop() blocks until they finish),
	// so only infrastructure goroutines remain and those are in the ignore list.
	cancelMgr()
	<-mgrDone
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	goleak.VerifyNone(t,
		goleak.IgnoreAnyFunction("sigs.k8s.io/controller-runtime/pkg/internal/testing/process.(*State).Start.func1"),
		goleak.IgnoreTopFunction("k8s.io/client-go/tools/cache.(*Reflector).ListAndWatch"),
		goleak.IgnoreTopFunction("k8s.io/client-go/tools/cache.(*Reflector).watchHandler"),
		goleak.IgnoreTopFunction("k8s.io/client-go/tools/cache.handleAnyWatch"),
		goleak.IgnoreTopFunction("k8s.io/client-go/tools/cache.(*Reflector).startResync"),
		goleak.IgnoreTopFunction("k8s.io/client-go/util/workqueue.(*Type).processLoop"),
		goleak.IgnoreTopFunction("sigs.k8s.io/controller-runtime/pkg/controller/priorityqueue.(*priorityqueue[...]).handleReadyItems.func1.1"),
		goleak.IgnoreAnyFunction("net/http/httptest.(*Server).goServe.func1"),
		goleak.IgnoreAnyFunction("net/http.(*conn).serve"),
		goleak.IgnoreAnyFunction("golang.org/x/net/http2.(*clientConnReadLoop).run"),
		goleak.IgnoreAnyFunction("golang.org/x/net/http2.(*clientStream).writeRequest"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
	)
}

// TestAGC_Reconciler_JobAcquisitionCycle verifies the end-to-end job acquisition path:
// AcquireJob called, replacement listener spawned (briefly active=2+), RenewJob ticks at
// least once, and the session count returns to 1 once burst goroutines idle-shut.
func TestAGC_Reconciler_JobAcquisitionCycle(t *testing.T) {
	const nsName = "agc-job-cycle-test"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, "cycle-rg", 2)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	startAGCReconciler(t)

	// Wait for the permanent baseline session.
	require.Eventually(t, func() bool {
		return len(brokerStub.RegisteredSessions()) >= 1
	}, 15*time.Second, 1*time.Millisecond, "baseline session should be registered")

	acquireBefore := brokerStub.AcquireJobCalls()

	// Enqueue a job; the goroutine will call AcquireJob and spawn a replacement.
	sessions := brokerStub.RegisteredSessions()
	brokerStub.EnqueueJob(sessions[len(sessions)-1], broker.RunnerJobRequestBody{})

	// A replacement session should appear (burst goroutine spawned).
	require.Eventually(t, func() bool {
		return len(brokerStub.RegisteredSessions()) >= 2
	}, 15*time.Second, 1*time.Millisecond, "replacement listener should spawn after job acquisition")

	// AcquireJob must have been called at least once.
	assert.Eventually(t, func() bool {
		return brokerStub.AcquireJobCalls() > acquireBefore
	}, 10*time.Second, 5*time.Millisecond, "AcquireJob must be called after job delivery")

	// Burst goroutines idle-exit; active count drops back to 1.
	// RenewJob is verified in TestAGC_SecretLifecycle_DeletedAfterPodCompletes where
	// the provisioner keeps the job handler alive long enough for the 50ms renew tick.
	assert.Eventually(t, func() bool {
		return brokerStub.ActiveSessionCount() == 1
	}, 30*time.Second, 10*time.Millisecond,
		"burst listener goroutines should drain to 1 after job delivery")
}

// TestAGC_Reconciler_BurstSpawnsAdditionalListeners verifies that enqueueing jobs causes
// additional listener goroutines to spawn up to maxListeners.
func TestAGC_Reconciler_BurstSpawnsAdditionalListeners(t *testing.T) {
	const nsName = "agc-burst-test"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, "burst-rg", 3)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	startAGCReconciler(t)

	// Wait for the initial session (permanent baseline listener).
	require.Eventually(t, func() bool {
		return len(brokerStub.RegisteredSessions()) >= 1
	}, 15*time.Second, 1*time.Millisecond, "at least one session should be registered")

	seen := map[string]bool{}

	// Enqueue a job on the first session → spawns a second goroutine.
	firstID := enqueueJobWhenSessionAvailable(15*time.Second, seen, broker.RunnerJobRequestBody{})
	require.NotEmpty(t, firstID, "should have found a session to enqueue on")
	seen[firstID] = true

	// Wait for the second session to appear (spawned replacement).
	require.Eventually(t, func() bool {
		return len(brokerStub.RegisteredSessions()) >= 2
	}, 15*time.Second, 1*time.Millisecond, "second session should spawn after job delivery")

	// Enqueue a job on the second session → spawns a third goroutine.
	secondID := enqueueJobWhenSessionAvailable(15*time.Second, seen, broker.RunnerJobRequestBody{})
	require.NotEmpty(t, secondID, "should have found a second session to enqueue on")
	seen[secondID] = true

	// Wait for the third session to appear (another spawned replacement).
	require.Eventually(t, func() bool {
		return len(brokerStub.RegisteredSessions()) >= 3
	}, 15*time.Second, 1*time.Millisecond, "third session should spawn after second job delivery")

	// Wait for extra goroutines to drain. Burst goroutines idle-shut after
	// IdleThreshold consecutive empty polls and call DELETE /session.
	// ActiveSessionCount tracks #POST − #DELETE, reaching 1 when only the
	// permanent baseline remains.
	assert.Eventually(t, func() bool {
		return brokerStub.ActiveSessionCount() == 1
	}, 30*time.Second, 10*time.Millisecond,
		"extra listener goroutines should drain to 1 after jobs are delivered")
}

// TestAGC_Reconciler_ScaleMaxListeners verifies that updating maxListeners on a RunnerGroup
// updates the multiplexer's ceiling and creates additional agent Secrets.
func TestAGC_Reconciler_ScaleMaxListeners(t *testing.T) {
	const nsName = "agc-scale-test"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, "scale-rg", 2)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	startAGCReconciler(t)

	// Wait for the initial session and 2 agent Secrets.
	require.Eventually(t, func() bool {
		return len(brokerStub.RegisteredSessions()) >= 1
	}, 15*time.Second, 1*time.Millisecond, "initial session should be registered")

	require.Eventually(t, func() bool {
		var secrets corev1.SecretList
		if err := k8sClient.List(ctx, &secrets,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "scale-rg"},
		); err != nil {
			return false
		}
		return len(secrets.Items) == 2
	}, 15*time.Second, 50*time.Millisecond, "expected 2 agent Secrets before scaling")

	// Update maxListeners to 5 (retry on conflict — reconciler may be writing finalizer concurrently).
	require.Eventually(t, func() bool {
		var fetched v1alpha1.RunnerGroup
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "scale-rg"}, &fetched); err != nil {
			return false
		}
		fetched.Spec.MaxListeners = 5
		return k8sClient.Update(ctx, &fetched) == nil
	}, 5*time.Second, 25*time.Millisecond, "update RunnerGroup maxListeners to 5")

	// Assert exactly 5 agent Secrets after scale-up (3 new ones created).
	assert.Eventually(t, func() bool {
		var secrets corev1.SecretList
		if err := k8sClient.List(ctx, &secrets,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "scale-rg"},
		); err != nil {
			return false
		}
		return len(secrets.Items) == 5
	}, 15*time.Second, 50*time.Millisecond, "expected 5 agent Secrets after scaling to maxListeners=5")

	// Assert the RunnerGroup status reflects the updated generation.
	assert.Eventually(t, func() bool {
		var rg v1alpha1.RunnerGroup
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "scale-rg"}, &rg); err != nil {
			return false
		}
		return rg.Status.ObservedGeneration >= rg.Generation
	}, 15*time.Second, 50*time.Millisecond, "status ObservedGeneration should reflect the update")

	// Verify we can now burst to 5 sessions by enqueueing 4 more jobs.
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		id := enqueueJobWhenSessionAvailable(15*time.Second, seen, broker.RunnerJobRequestBody{})
		if id == "" {
			break
		}
		seen[id] = true
	}

	// After bursting, session count should reach at least 3 (greater than original ceiling of 2).
	assert.Eventually(t, func() bool {
		return len(brokerStub.RegisteredSessions()) >= 3
	}, 20*time.Second, 1*time.Millisecond,
		"session count should exceed the old maxListeners=2 ceiling after scaling to 5")
}

// TestAGC_Reconciler_ScaleDown verifies that reducing maxListeners deletes excess agent
// Secrets and leaves no goroutine leaks.
func TestAGC_Reconciler_ScaleDown(t *testing.T) {
	const nsName = "agc-scaledown-test"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, "scaledown-rg", 5)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	cancelMgr, mgrDone := startAGCReconciler(t)

	// Wait for 5 agent Secrets and the permanent baseline session.
	require.Eventually(t, func() bool {
		var secrets corev1.SecretList
		if err := k8sClient.List(ctx, &secrets,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "scaledown-rg"},
		); err != nil {
			return false
		}
		return len(secrets.Items) == 5
	}, 15*time.Second, 50*time.Millisecond, "expected 5 agent Secrets before scale-down")

	require.Eventually(t, func() bool {
		var fetched v1alpha1.RunnerGroup
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "scaledown-rg"}, &fetched); err != nil {
			return false
		}
		return fetched.Status.ActiveSessions >= 1
	}, 15*time.Second, 50*time.Millisecond, "permanent baseline session should be active")

	// Patch maxListeners from 5 to 2 (retry on conflict).
	require.Eventually(t, func() bool {
		var fetched v1alpha1.RunnerGroup
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "scaledown-rg"}, &fetched); err != nil {
			return false
		}
		fetched.Spec.MaxListeners = 2
		return k8sClient.Update(ctx, &fetched) == nil
	}, 5*time.Second, 25*time.Millisecond, "update RunnerGroup maxListeners to 2")

	// Assert exactly 2 agent Secrets remain (3 excess deleted by EnsureAgents).
	assert.Eventually(t, func() bool {
		var secrets corev1.SecretList
		if err := k8sClient.List(ctx, &secrets,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "scaledown-rg"},
		); err != nil {
			return false
		}
		return len(secrets.Items) == 2
	}, 15*time.Second, 50*time.Millisecond, "exactly 2 agent Secrets should remain after scale-down")

	// Active session count should settle at 1 (permanent baseline only; any burst
	// goroutines that held excess agents idle-exit at their next empty poll).
	// Use per-RunnerGroup Status.ActiveSessions (set by the multiplexer) rather than
	// the global broker stub counter, which can include sessions from other tests.
	assert.Eventually(t, func() bool {
		var fetched v1alpha1.RunnerGroup
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "scaledown-rg"}, &fetched); err != nil {
			return false
		}
		return fetched.Status.ActiveSessions == 1
	}, 15*time.Second, 50*time.Millisecond,
		"active session count should be 1 at rest after scale-down")

	// Verify no goroutine leaks after stopping the manager.
	cancelMgr()
	<-mgrDone
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	goleak.VerifyNone(t,
		goleak.IgnoreAnyFunction("sigs.k8s.io/controller-runtime/pkg/internal/testing/process.(*State).Start.func1"),
		goleak.IgnoreTopFunction("k8s.io/client-go/tools/cache.(*Reflector).ListAndWatch"),
		goleak.IgnoreTopFunction("k8s.io/client-go/tools/cache.(*Reflector).watchHandler"),
		goleak.IgnoreTopFunction("k8s.io/client-go/tools/cache.handleAnyWatch"),
		goleak.IgnoreTopFunction("k8s.io/client-go/tools/cache.(*Reflector).startResync"),
		goleak.IgnoreTopFunction("k8s.io/client-go/util/workqueue.(*Type).processLoop"),
		goleak.IgnoreTopFunction("sigs.k8s.io/controller-runtime/pkg/controller/priorityqueue.(*priorityqueue[...]).handleReadyItems.func1.1"),
		goleak.IgnoreAnyFunction("net/http/httptest.(*Server).goServe.func1"),
		goleak.IgnoreAnyFunction("net/http.(*conn).serve"),
		goleak.IgnoreAnyFunction("golang.org/x/net/http2.(*clientConnReadLoop).run"),
		goleak.IgnoreAnyFunction("golang.org/x/net/http2.(*clientStream).writeRequest"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
	)
}
