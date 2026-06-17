package provisioner

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodWaiter blocks until a worker pod reaches a terminal phase. It abstracts the
// completion-detection mechanism so the Provisioner can be wired with either the
// event-driven [InformerPodWaiter] (production) or the poll fallback (unit tests
// with a fake client; see Provisioner.waitForPodCompletion).
type PodWaiter interface {
	// WaitForCompletion blocks until the named pod reaches a terminal phase or
	// ctx is cancelled. It returns the terminal phase and Status.Reason (used to
	// detect evictions). A pod that is deleted before reaching a terminal phase
	// is reported as PodSucceeded with an empty reason.
	WaitForCompletion(ctx context.Context, namespace, name string) (corev1.PodPhase, string, error)
}

// terminalPhase reports whether pod has reached a terminal phase, returning the
// phase and its Status.Reason. The set matches the legacy poll loop exactly:
// Succeeded, Failed, and Unknown are terminal; Pending and Running are not.
func terminalPhase(pod *corev1.Pod) (corev1.PodPhase, string, bool) {
	switch pod.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed, corev1.PodUnknown:
		return pod.Status.Phase, pod.Status.Reason, true
	default:
		return "", "", false
	}
}

// podResult is delivered to a blocked waiter when its pod resolves. It also
// carries the optional pod-creation latency observation so resolve can emit it
// exactly once per pod (on the first resolving event that still has waiters).
type podResult struct {
	phase  corev1.PodPhase
	reason string
	// namespace and latency carry the pod-creation-latency observation.
	// latencyValid is false when the pod never started (no container StartedAt),
	// in which case no observation is emitted.
	namespace    string
	latency      time.Duration
	latencyValid bool
}

// podCreationLatency returns the time from the pod's creation to the earliest
// container start (Running.StartedAt or Terminated.StartedAt). The boolean is
// false when no container has started yet, so the pod was scheduled but its
// runner image has not finished pulling — no meaningful latency to record.
func podCreationLatency(pod *corev1.Pod) (time.Duration, bool) {
	created := pod.CreationTimestamp.Time
	if created.IsZero() {
		return 0, false
	}
	var first time.Time
	for i := range pod.Status.ContainerStatuses {
		st := &pod.Status.ContainerStatuses[i].State
		var started time.Time
		switch {
		case st.Running != nil && !st.Running.StartedAt.IsZero():
			started = st.Running.StartedAt.Time
		case st.Terminated != nil && !st.Terminated.StartedAt.IsZero():
			started = st.Terminated.StartedAt.Time
		default:
			continue
		}
		if first.IsZero() || started.Before(first) {
			first = started
		}
	}
	if first.IsZero() {
		return 0, false
	}
	d := first.Sub(created)
	if d < 0 {
		d = 0
	}
	return d, true
}

// InformerPodWaiter detects worker-pod completion by registering a single event
// handler on the shared Pod informer maintained by the controller-runtime cache,
// rather than polling per session. One handler serves every in-flight session:
// each WaitForCompletion call registers a buffered channel keyed by pod, and the
// handler signals it when the informer observes the pod reaching a terminal phase
// (or being deleted).
//
// It implements manager.Runnable so the handler is registered after the cache
// has synced; wire it with mgr.Add and assign it to Provisioner.Waiter.
type InformerPodWaiter struct {
	// cache provides the shared Pod informer registered in Start.
	cache cache.Cache
	// reader serves the registration-time current-state read. In production it
	// is the same cache (a cache.Cache is a client.Reader served from the same
	// informer, so the read sees no more staleness than the events do); tests
	// inject a fake client.Reader here.
	reader client.Reader
	log    *slog.Logger

	// PodCreationLatency, when non-nil, is observed once per pod when the pod
	// resolves: the time from pod creation to its runner container starting
	// (scheduling + image pull). Optional so unit tests can omit it.
	PodCreationLatency *prometheus.HistogramVec

	mu      sync.Mutex
	waiters map[string]map[chan podResult]struct{} // key: "namespace/name"
}

// NewInformerPodWaiter returns an InformerPodWaiter backed by the manager cache.
// Pass mgr.GetCache(). A nil log defaults to slog.Default().
func NewInformerPodWaiter(c cache.Cache, log *slog.Logger) *InformerPodWaiter {
	if log == nil {
		log = slog.Default()
	}
	return &InformerPodWaiter{
		cache:   c,
		reader:  c,
		log:     log,
		waiters: make(map[string]map[chan podResult]struct{}),
	}
}

// Start registers the Pod event handler on the shared informer and blocks until
// ctx is cancelled. It satisfies sigs.k8s.io/controller-runtime/pkg/manager.Runnable.
func (w *InformerPodWaiter) Start(ctx context.Context) error {
	inf, err := w.cache.GetInformer(ctx, &corev1.Pod{})
	if err != nil {
		return fmt.Errorf("provisioner: get pod informer: %w", err)
	}
	reg, err := inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.onPodEvent(obj) },
		UpdateFunc: func(_, newObj any) { w.onPodEvent(newObj) },
		DeleteFunc: w.onPodDelete,
	})
	if err != nil {
		return fmt.Errorf("provisioner: add pod event handler: %w", err)
	}
	w.log.Info("pod completion watcher started")
	<-ctx.Done()
	_ = inf.RemoveEventHandler(reg)
	return nil
}

// NeedLeaderElection reports that the waiter must run on every replica, not only
// the leader: each AGC instance provisions its own pods and must observe their
// completion regardless of leader status.
func (w *InformerPodWaiter) NeedLeaderElection() bool { return false }

// WaitForCompletion implements PodWaiter.
func (w *InformerPodWaiter) WaitForCompletion(ctx context.Context, namespace, name string) (corev1.PodPhase, string, error) {
	key := namespace + "/" + name
	// Debug-level traces of the wait lifecycle: this loop is otherwise silent, so
	// a session stuck waiting on a pod that never reaches a terminal phase (missed
	// informer event, never-terminating pod) produces no output at all. The traces
	// stay at Debug so they add no volume at Info under thousands of sessions.
	log := w.log.With("namespace", namespace, "name", name)
	ch := make(chan podResult, 1)
	w.register(key, ch)
	defer w.deregister(key, ch)

	// Resolve immediately if the informer already holds the pod in a terminal
	// phase — this closes the race where the terminal event fires between the
	// pod's creation and this registration. A NotFound here means the cache has
	// not yet observed our just-issued Create (or the pod is already gone); in
	// both cases we wait for an event rather than concluding prematurely.
	var pod corev1.Pod
	switch err := w.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &pod); {
	case err == nil:
		if phase, reason, ok := terminalPhase(&pod); ok {
			log.Debug("pod already terminal at registration", "phase", phase, "reason", reason)
			return phase, reason, nil
		}
		log.Debug("registered for pod completion; pod not yet terminal", "phase", pod.Status.Phase)
	case apierrors.IsNotFound(err):
		// Not yet synced or already deleted — wait for an event.
		log.Debug("registered for pod completion; pod not yet in cache, awaiting event")
	default:
		return "", "", fmt.Errorf("provisioner: pod waiter cache get: %w", err)
	}

	select {
	case <-ctx.Done():
		log.Debug("pod wait cancelled before completion", "error", ctx.Err())
		return "", "", ctx.Err()
	case res := <-ch:
		log.Debug("pod completion observed", "phase", res.phase, "reason", res.reason)
		return res.phase, res.reason, nil
	}
}

func (w *InformerPodWaiter) register(key string, ch chan podResult) {
	w.mu.Lock()
	defer w.mu.Unlock()
	set := w.waiters[key]
	if set == nil {
		set = make(map[chan podResult]struct{})
		w.waiters[key] = set
	}
	set[ch] = struct{}{}
}

func (w *InformerPodWaiter) deregister(key string, ch chan podResult) {
	w.mu.Lock()
	defer w.mu.Unlock()
	set := w.waiters[key]
	if set == nil {
		return
	}
	delete(set, ch)
	if len(set) == 0 {
		delete(w.waiters, key)
	}
}

// resolve signals every waiter registered for key with res and removes them.
// Each waiter channel is buffered (size 1) and signalled at most once, so the
// non-blocking send never drops a result.
func (w *InformerPodWaiter) resolve(key string, res podResult) {
	w.mu.Lock()
	defer w.mu.Unlock()
	set := w.waiters[key]
	if set == nil {
		return
	}
	// Emit the pod-creation-latency observation here — guarded by the non-nil
	// waiter set — so it fires exactly once per pod (the first resolving event
	// that still has registered waiters), even though the informer delivers many
	// post-terminal update events.
	if w.PodCreationLatency != nil && res.latencyValid {
		w.PodCreationLatency.WithLabelValues(res.namespace).Observe(res.latency.Seconds())
	}
	for ch := range set {
		select {
		case ch <- res:
		default:
		}
		delete(set, ch)
	}
	delete(w.waiters, key)
}

// onPodEvent resolves waiters when an Add/Update brings a pod to a terminal phase.
func (w *InformerPodWaiter) onPodEvent(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	if phase, reason, ok := terminalPhase(pod); ok {
		latency, ok := podCreationLatency(pod)
		w.resolve(pod.Namespace+"/"+pod.Name, podResult{
			phase:        phase,
			reason:       reason,
			namespace:    pod.Namespace,
			latency:      latency,
			latencyValid: ok,
		})
	}
}

// onPodDelete resolves waiters when a pod is deleted before reaching a terminal
// phase, matching the legacy poll's "deleted externally → treat as completion".
func (w *InformerPodWaiter) onPodDelete(obj any) {
	pod := podFromDeleteObj(obj)
	if pod == nil {
		return
	}
	w.resolve(pod.Namespace+"/"+pod.Name, podResult{phase: corev1.PodSucceeded})
}

// podFromDeleteObj extracts the Pod from a DeleteFunc object, unwrapping the
// DeletedFinalStateUnknown tombstone the informer delivers when it missed the
// delete watch event.
func podFromDeleteObj(obj any) *corev1.Pod {
	switch v := obj.(type) {
	case *corev1.Pod:
		return v
	case toolscache.DeletedFinalStateUnknown:
		if pod, ok := v.Obj.(*corev1.Pod); ok {
			return pod
		}
	}
	return nil
}
