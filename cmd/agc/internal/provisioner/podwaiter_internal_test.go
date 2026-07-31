package provisioner

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// lockedBuffer is a goroutine-safe io.Writer for capturing slog output emitted
// from the waiter's own goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

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

// preemptedPod is a pod carrying the DisruptionTarget condition kube-scheduler stamps on
// a preemption victim before deleting it. The phase is a parameter because the phase is
// precisely what this marker exists to be independent of.
func preemptedPod(ns, name string, phase corev1.PodPhase) *corev1.Pod {
	p := pod(ns, name, phase, "")
	p.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.DisruptionTarget,
		Status: corev1.ConditionTrue,
		Reason: corev1.PodReasonPreemptionByScheduler,
	}}
	return p
}

func TestInformerPodWaiter_TerminalBeforeWait(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodSucceeded, ""))

	out, err := w.WaitForCompletion(context.Background(), "ns", "p")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Phase != corev1.PodSucceeded || out.Reason != "" {
		t.Fatalf("got phase=%q reason=%q, want Succeeded/\"\"", out.Phase, out.Reason)
	}
}

func TestInformerPodWaiter_EventDrivenSucceeded(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodPending, ""))

	done := make(chan podResult, 1)
	go func() {
		out, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{outcome: out}
	}()

	// Let the goroutine register before the event fires.
	waitForRegistration(t, w, "ns/p")
	w.onPodEvent(pod("ns", "p", corev1.PodSucceeded, ""))

	res := mustResolve(t, done)
	if res.outcome.Phase != corev1.PodSucceeded {
		t.Fatalf("got phase=%q, want Succeeded", res.outcome.Phase)
	}
}

func TestInformerPodWaiter_EventDrivenEviction(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodRunning, ""))

	done := make(chan podResult, 1)
	go func() {
		out, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{outcome: out}
	}()

	waitForRegistration(t, w, "ns/p")
	w.onPodEvent(pod("ns", "p", corev1.PodFailed, "Evicted"))

	res := mustResolve(t, done)
	if res.outcome.Phase != corev1.PodFailed || res.outcome.Reason != "Evicted" {
		t.Fatalf("got phase=%q reason=%q, want Failed/Evicted", res.outcome.Phase, res.outcome.Reason)
	}
}

func TestInformerPodWaiter_DeleteResolvesSucceeded(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodRunning, ""))

	done := make(chan podResult, 1)
	go func() {
		out, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{outcome: out}
	}()

	waitForRegistration(t, w, "ns/p")
	w.onPodDelete(pod("ns", "p", corev1.PodRunning, ""))

	res := mustResolve(t, done)
	if res.outcome.Phase != corev1.PodSucceeded || res.outcome.Reason != "" {
		t.Fatalf("got phase=%q reason=%q, want Succeeded/\"\"", res.outcome.Phase, res.outcome.Reason)
	}
}

func TestInformerPodWaiter_DeleteTombstone(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodRunning, ""))

	done := make(chan podResult, 1)
	go func() {
		out, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{outcome: out}
	}()

	waitForRegistration(t, w, "ns/p")
	w.onPodDelete(toolscache.DeletedFinalStateUnknown{
		Key: "ns/p",
		Obj: pod("ns", "p", corev1.PodRunning, ""),
	})

	res := mustResolve(t, done)
	if res.outcome.Phase != corev1.PodSucceeded {
		t.Fatalf("got phase=%q, want Succeeded", res.outcome.Phase)
	}
}

// The waiter must carry the preemption marker out on the DELETE path, because that is
// the path a scheduler preemption almost always takes: the scheduler removes its victim
// by deleting it, and a victim that never got a container running (image still pulling —
// the shape Q423 reproduced) publishes no terminal phase at all. The informer's delete
// event carries the pod's last-known state, which is the only place the condition
// survives; a re-Get would race the object out of existence.
func TestInformerPodWaiter_DeleteCarriesPreemptionMarker(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodPending, ""))

	done := make(chan podResult, 1)
	go func() {
		out, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{outcome: out}
	}()

	waitForRegistration(t, w, "ns/p")
	w.onPodDelete(preemptedPod("ns", "p", corev1.PodPending))

	res := mustResolve(t, done)
	if !res.outcome.Preempted {
		t.Fatal("a deleted preemption victim resolved without its Preempted marker; the classic tier would not recover it")
	}
}

// The same, via the tombstone the informer delivers when it missed the delete watch
// event. An AGC whose watch dropped must still recover the displaced run.
func TestInformerPodWaiter_DeleteTombstoneCarriesPreemptionMarker(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodRunning, ""))

	done := make(chan podResult, 1)
	go func() {
		out, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{outcome: out}
	}()

	waitForRegistration(t, w, "ns/p")
	w.onPodDelete(toolscache.DeletedFinalStateUnknown{
		Key: "ns/p",
		Obj: preemptedPod("ns", "p", corev1.PodRunning),
	})

	if res := mustResolve(t, done); !res.outcome.Preempted {
		t.Fatal("a tombstoned preemption victim resolved without its Preempted marker")
	}
}

// A preemption whose container did exit resolves through the terminal-phase path
// instead, and must carry the marker there too. The phase itself is uninformative —
// Q423 measured Succeeded from a preemption — so the marker is the entire signal.
func TestInformerPodWaiter_TerminalPhaseCarriesPreemptionMarker(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodRunning, ""))

	done := make(chan podResult, 1)
	go func() {
		out, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{outcome: out}
	}()

	waitForRegistration(t, w, "ns/p")
	w.onPodEvent(preemptedPod("ns", "p", corev1.PodSucceeded))

	res := mustResolve(t, done)
	if res.outcome.Phase != corev1.PodSucceeded {
		t.Fatalf("got phase=%q, want Succeeded", res.outcome.Phase)
	}
	if !res.outcome.Preempted {
		t.Fatal("a preempted worker that exited 0 resolved without its Preempted marker; the phase alone cannot discriminate")
	}
}

// An undisturbed completion must not be flagged, or every finished job would be re-run.
func TestInformerPodWaiter_OrdinaryCompletionIsNotPreempted(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodSucceeded, ""))

	out, err := w.WaitForCompletion(context.Background(), "ns", "p")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Preempted {
		t.Fatal("an ordinary completion was reported as preempted")
	}
	if out.ExternallyDeleted {
		t.Fatal("an ordinary completion was reported as externally deleted")
	}
}

// deletingPod is a pod with a deletionTimestamp, in the given phase — the shape a
// graceful external removal (a drain, a `kubectl delete pod`) leaves while the kubelet
// tears the pod down.
func deletingPod(ns, name string, phase corev1.PodPhase) *corev1.Pod {
	p := pod(ns, name, phase, "")
	now := metav1.Now()
	p.DeletionTimestamp = &now
	return p
}

// A worker whose terminal phase publishes while its deletion is in flight must carry
// the mark out of the wait — it is the entire discriminator between a drained worker
// and a genuinely failed (or human-cancelled) one, which share the phase and the empty
// reason (Q459/Q502).
func TestInformerPodWaiter_TerminalPhaseCarriesDeletionMark(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodRunning, ""))

	done := make(chan podResult, 1)
	go func() {
		out, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{outcome: out}
	}()

	waitForRegistration(t, w, "ns/p")
	w.onPodEvent(deletingPod("ns", "p", corev1.PodFailed))

	res := mustResolve(t, done)
	if res.outcome.Phase != corev1.PodFailed || res.outcome.Reason != "" {
		t.Fatalf("got phase=%q reason=%q, want Failed/\"\"", res.outcome.Phase, res.outcome.Reason)
	}
	if !res.outcome.ExternallyDeleted {
		t.Fatal("a drained worker's terminal phase resolved without its deletion mark; the classic tier would not recover it")
	}
}

// The reaper stamps its own deletions before issuing them, and those must NOT read as
// external — the reaper deletes pods the AGC gave up on, and re-running them would turn
// cleanup into a re-run trigger (Q502).
func TestInformerPodWaiter_AGCOwnDeletionIsNotExternal(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodRunning, ""))

	done := make(chan podResult, 1)
	go func() {
		out, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{outcome: out}
	}()

	waitForRegistration(t, w, "ns/p")
	reaped := deletingPod("ns", "p", corev1.PodFailed)
	reaped.Annotations = map[string]string{AnnotationDeletionReason: "pending_deadline"}
	w.onPodEvent(reaped)

	if res := mustResolve(t, done); res.outcome.ExternallyDeleted {
		t.Fatal("a reaper-deleted worker was reported as externally deleted; the reaper would become a re-run trigger")
	}
}

// A pod deleted without ever publishing a terminal phase resolves through the delete
// path as before, with no deletion mark: Q459's decision gates recovery on the mark
// being present at terminal publish, and this is also the path the reaper's
// pending-deadline deletions resolve through.
func TestInformerPodWaiter_DeletePathDoesNotCarryDeletionMark(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodRunning, ""))

	done := make(chan podResult, 1)
	go func() {
		out, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{outcome: out}
	}()

	waitForRegistration(t, w, "ns/p")
	w.onPodDelete(deletingPod("ns", "p", corev1.PodRunning))

	res := mustResolve(t, done)
	if res.outcome.Phase != corev1.PodSucceeded || res.outcome.ExternallyDeleted {
		t.Fatalf("got phase=%q externallyDeleted=%v, want Succeeded/false: a pod that vanished "+
			"without a terminal phase has no reportable failure to re-run", res.outcome.Phase, res.outcome.ExternallyDeleted)
	}
}

// NotFound at registration (cache hasn't observed the just-created pod) must not
// resolve as success; the waiter must block until a real terminal event arrives.
func TestInformerPodWaiter_NotFoundThenTerminal(t *testing.T) {
	w := newTestWaiter() // empty reader → registration-time Get returns NotFound

	done := make(chan podResult, 1)
	go func() {
		out, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{outcome: out}
	}()

	waitForRegistration(t, w, "ns/p")

	// It must still be blocked — no premature success.
	select {
	case res := <-done:
		t.Fatalf("waiter resolved prematurely with phase=%q", res.outcome.Phase)
	case <-time.After(50 * time.Millisecond):
	}

	w.onPodEvent(pod("ns", "p", corev1.PodSucceeded, ""))
	res := mustResolve(t, done)
	if res.outcome.Phase != corev1.PodSucceeded {
		t.Fatalf("got phase=%q, want Succeeded", res.outcome.Phase)
	}
}

func TestInformerPodWaiter_ContextCancel(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodRunning, ""))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := w.WaitForCompletion(ctx, "ns", "p")
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
		out, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{outcome: out}
	}()

	waitForRegistration(t, w, "ns/p")
	w.onPodEvent(pod("ns", "p", corev1.PodRunning, "")) // still not terminal

	select {
	case res := <-done:
		t.Fatalf("waiter resolved on non-terminal event with phase=%q", res.outcome.Phase)
	case <-time.After(50 * time.Millisecond):
	}

	w.onPodEvent(pod("ns", "p", corev1.PodFailed, ""))
	if res := mustResolve(t, done); res.outcome.Phase != corev1.PodFailed {
		t.Fatalf("got phase=%q, want Failed", res.outcome.Phase)
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
			out, _ := w.WaitForCompletion(context.Background(), "ns", "p")
			results <- out.Phase
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

// A stuck/cancelled wait must leave a Debug trail: the loop is otherwise silent,
// so a session blocked on a pod that never terminates produces no output. This
// asserts the cancel-path Debug line is emitted (Q88, logging-audit Theme E).
func TestInformerPodWaiter_DebugLogsOnCancel(t *testing.T) {
	buf := &lockedBuffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w := &InformerPodWaiter{
		reader:  fake.NewClientBuilder().WithScheme(waiterScheme()).WithObjects(pod("ns", "p", corev1.PodRunning, "")).Build(),
		log:     log,
		waiters: make(map[string]map[chan podResult]struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := w.WaitForCompletion(ctx, "ns", "p")
		errCh <- err
	}()

	waitForRegistration(t, w, "ns/p")
	cancel()

	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not return after context cancel")
	}

	got := buf.String()
	if !strings.Contains(got, "pod wait cancelled before completion") {
		t.Fatalf("expected cancel-path debug log, got:\n%s", got)
	}
	if !strings.Contains(got, "namespace=ns") || !strings.Contains(got, "name=p") {
		t.Fatalf("expected namespace/name correlation fields in debug log, got:\n%s", got)
	}
}

// newLatencyHistogram returns an unregistered histogram with the production
// label set, for asserting pod-creation-latency observations in isolation.
func newLatencyHistogram() *prometheus.HistogramVec {
	return prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "test_pod_creation_latency_seconds",
		Buckets: []float64{0.5, 1, 5, 30},
	}, []string{"namespace"})
}

// startedPod builds a pod whose runner container started startedAfter the pod
// was created, in the given phase. A terminal phase carries Terminated.StartedAt;
// a Running phase carries Running.StartedAt.
func startedPod(ns, name string, phase corev1.PodPhase, startedAfter time.Duration) *corev1.Pod {
	created := metav1.Now()
	startedAt := metav1.NewTime(created.Add(startedAfter))
	state := corev1.ContainerState{}
	if phase == corev1.PodRunning {
		state.Running = &corev1.ContainerStateRunning{StartedAt: startedAt}
	} else {
		state.Terminated = &corev1.ContainerStateTerminated{StartedAt: startedAt}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: created},
		Status: corev1.PodStatus{
			Phase:             phase,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "runner", State: state}},
		},
	}
}

// A terminal pod that started must yield exactly one latency observation, emitted
// once even though the informer delivers further post-terminal update events.
func TestInformerPodWaiter_PodCreationLatencyObservedOnce(t *testing.T) {
	w := newTestWaiter(startedPod("ns", "p", corev1.PodPending, 3*time.Second))
	w.PodCreationLatency = newLatencyHistogram()

	done := make(chan podResult, 1)
	go func() {
		out, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{outcome: out}
	}()

	waitForRegistration(t, w, "ns/p")
	terminal := startedPod("ns", "p", corev1.PodSucceeded, 3*time.Second)
	w.onPodEvent(terminal)
	mustResolve(t, done)

	// A second post-terminal event must not double-count (no waiters remain).
	w.onPodEvent(terminal)

	if got := testutil.CollectAndCount(w.PodCreationLatency); got != 1 {
		t.Fatalf("got %d latency observations, want exactly 1", got)
	}
}

// A pod that reached a terminal phase without any container ever starting (no
// StartedAt) must not produce a latency observation.
func TestInformerPodWaiter_PodCreationLatencySkippedWhenNeverStarted(t *testing.T) {
	w := newTestWaiter(pod("ns", "p", corev1.PodPending, ""))
	w.PodCreationLatency = newLatencyHistogram()

	done := make(chan podResult, 1)
	go func() {
		out, _ := w.WaitForCompletion(context.Background(), "ns", "p")
		done <- podResult{outcome: out}
	}()

	waitForRegistration(t, w, "ns/p")
	w.onPodEvent(pod("ns", "p", corev1.PodFailed, "")) // no container ever started
	mustResolve(t, done)

	if got := testutil.CollectAndCount(w.PodCreationLatency); got != 0 {
		t.Fatalf("got %d latency observations, want 0 for a never-started pod", got)
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
