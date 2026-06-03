package provisioner

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// These tests exercise InformerPodWaiter's registry, terminal detection, and
// race handling without a real informer: the registration-time current-state
// read is served by a fake client.Reader, and informer events are simulated by
// calling onPodEvent / onPodDelete directly. The Start path (registering on a
// real shared informer) is covered by the envtest integration suite.

func waiterScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	return s
}

// newTestWaiter builds an InformerPodWaiter whose registration-time read is
// served by a fake client seeded with objs. cache is left nil (Start is never
// called in these tests).
func newTestWaiter(objs ...client.Object) *InformerPodWaiter {
	reader := fake.NewClientBuilder().
		WithScheme(waiterScheme()).
		WithObjects(objs...).
		Build()
	return &InformerPodWaiter{
		cache:   nil,
		reader:  reader,
		log:     slog.Default(),
		waiters: make(map[string]map[chan podResult]struct{}),
	}
}

func pod(ns, name string, phase corev1.PodPhase, reason string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     corev1.PodStatus{Phase: phase, Reason: reason},
	}
}

func TestInformerPodWaiter_TerminalBeforeWait(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodSucceeded, ""))

	phase, reason, err := w.WaitForCompletion(context.Background(), "ns", "p")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if phase != corev1.PodSucceeded || reason != "" {
		t.Fatalf("got phase=%q reason=%q, want Succeeded/\"\"", phase, reason)
	}
}

func TestInformerPodWaiter_EventDrivenSucceeded(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodPending, ""))

	done := make(chan podResult, 1)
	go func() {
		ph, rs, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{ph, rs}
	}()

	// Let the goroutine register before the event fires.
	waitForRegistration(t, w, "ns/p")
	w.onPodEvent(pod("ns", "p", corev1.PodSucceeded, ""))

	res := mustResolve(t, done)
	if res.phase != corev1.PodSucceeded {
		t.Fatalf("got phase=%q, want Succeeded", res.phase)
	}
}

func TestInformerPodWaiter_EventDrivenEviction(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodRunning, ""))

	done := make(chan podResult, 1)
	go func() {
		ph, rs, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{ph, rs}
	}()

	waitForRegistration(t, w, "ns/p")
	w.onPodEvent(pod("ns", "p", corev1.PodFailed, "Evicted"))

	res := mustResolve(t, done)
	if res.phase != corev1.PodFailed || res.reason != "Evicted" {
		t.Fatalf("got phase=%q reason=%q, want Failed/Evicted", res.phase, res.reason)
	}
}

func TestInformerPodWaiter_DeleteResolvesSucceeded(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodRunning, ""))

	done := make(chan podResult, 1)
	go func() {
		ph, rs, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{ph, rs}
	}()

	waitForRegistration(t, w, "ns/p")
	w.onPodDelete(pod("ns", "p", corev1.PodRunning, ""))

	res := mustResolve(t, done)
	if res.phase != corev1.PodSucceeded || res.reason != "" {
		t.Fatalf("got phase=%q reason=%q, want Succeeded/\"\"", res.phase, res.reason)
	}
}

func TestInformerPodWaiter_DeleteTombstone(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodRunning, ""))

	done := make(chan podResult, 1)
	go func() {
		ph, rs, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{ph, rs}
	}()

	waitForRegistration(t, w, "ns/p")
	w.onPodDelete(toolscache.DeletedFinalStateUnknown{
		Key: "ns/p",
		Obj: pod("ns", "p", corev1.PodRunning, ""),
	})

	res := mustResolve(t, done)
	if res.phase != corev1.PodSucceeded {
		t.Fatalf("got phase=%q, want Succeeded", res.phase)
	}
}

// NotFound at registration (cache hasn't observed the just-created pod) must not
// resolve as success; the waiter must block until a real terminal event arrives.
func TestInformerPodWaiter_NotFoundThenTerminal(t *testing.T) {
	w := newTestWaiter() // empty reader → registration-time Get returns NotFound

	done := make(chan podResult, 1)
	go func() {
		ph, rs, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{ph, rs}
	}()

	waitForRegistration(t, w, "ns/p")

	// It must still be blocked — no premature success.
	select {
	case res := <-done:
		t.Fatalf("waiter resolved prematurely with phase=%q", res.phase)
	case <-time.After(50 * time.Millisecond):
	}

	w.onPodEvent(pod("ns", "p", corev1.PodSucceeded, ""))
	res := mustResolve(t, done)
	if res.phase != corev1.PodSucceeded {
		t.Fatalf("got phase=%q, want Succeeded", res.phase)
	}
}

func TestInformerPodWaiter_ContextCancel(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodRunning, ""))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := w.WaitForCompletion(ctx, "ns", "p")
		errCh <- err
	}()

	waitForRegistration(t, w, "ns/p")
	cancel()

	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("got err=%v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not return after context cancel")
	}

	// The waiter must have deregistered itself on exit.
	w.mu.Lock()
	_, present := w.waiters["ns/p"]
	w.mu.Unlock()
	if present {
		t.Fatal("waiter left a stale registry entry after cancel")
	}
}

// A non-terminal event must not wake a waiter; only terminal phases resolve.
func TestInformerPodWaiter_NonTerminalEventIgnored(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodPending, ""))

	done := make(chan podResult, 1)
	go func() {
		ph, rs, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{ph, rs}
	}()

	waitForRegistration(t, w, "ns/p")
	w.onPodEvent(pod("ns", "p", corev1.PodRunning, "")) // still not terminal

	select {
	case res := <-done:
		t.Fatalf("waiter resolved on non-terminal event with phase=%q", res.phase)
	case <-time.After(50 * time.Millisecond):
	}

	w.onPodEvent(pod("ns", "p", corev1.PodFailed, ""))
	if res := mustResolve(t, done); res.phase != corev1.PodFailed {
		t.Fatalf("got phase=%q, want Failed", res.phase)
	}
}

// Multiple waiters on the same pod (and an event for an unrelated pod) must all
// resolve correctly.
func TestInformerPodWaiter_MultipleWaiters(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodRunning, ""))

	const n = 5
	var wg sync.WaitGroup
	results := make(chan corev1.PodPhase, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ph, _, _ := w.WaitForCompletion(context.Background(), "ns", "p")
			results <- ph
		}()
	}

	// Wait until all n have registered.
	require := func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.waiters["ns/p"]) == n
	}
	deadline := time.Now().Add(2 * time.Second)
	for !require() {
		if time.Now().After(deadline) {
			t.Fatal("not all waiters registered")
		}
		time.Sleep(time.Millisecond)
	}

	// An unrelated pod's event must not wake them.
	w.onPodEvent(pod("ns", "other", corev1.PodSucceeded, ""))
	w.onPodEvent(pod("ns", "p", corev1.PodSucceeded, ""))

	wg.Wait()
	close(results)
	count := 0
	for ph := range results {
		if ph != corev1.PodSucceeded {
			t.Fatalf("got phase=%q, want Succeeded", ph)
		}
		count++
	}
	if count != n {
		t.Fatalf("resolved %d waiters, want %d", count, n)
	}
}

// waitForRegistration blocks until key has at least one registered waiter.
func waitForRegistration(t *testing.T, w *InformerPodWaiter, key string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		w.mu.Lock()
		n := len(w.waiters[key])
		w.mu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiter never registered for %q", key)
		}
		time.Sleep(time.Millisecond)
	}
}

func mustResolve(t *testing.T, done <-chan podResult) podResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not resolve")
		return podResult{}
	}
}
