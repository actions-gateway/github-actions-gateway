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

// PodOutcome is how a worker pod stopped being this session's concern: the terminal
// phase and Status.Reason (which together detect a kubelet eviction), plus whether the
// pod was a kube-scheduler preemption victim.
//
// Preempted travels alongside the phase rather than being read back off the pod later,
// because by the time provision() could re-Get the pod the scheduler's delete may have
// completed and the evidence is gone. The resolving event still holds the object — the
// informer's delete event carries the last-known pod — so the marker is captured at the
// only moment it is reliably available.
type PodOutcome struct {
	// Phase is the terminal phase, or PodSucceeded for a pod deleted before it
	// reached one.
	Phase corev1.PodPhase
	// Reason is Pod.Status.Reason; "Evicted" is the kubelet's node-pressure eviction.
	Reason string
	// Preempted reports a DisruptionTarget=True/PreemptionByScheduler condition, which
	// only kube-scheduler writes (Q497).
	Preempted bool
	// ExternallyDeleted reports that the pod carried a deletionTimestamp at the moment
	// its terminal phase published, and the deletion was not the AGC's own (see
	// AnnotationDeletionReason). It is the drained/deleted-worker discriminator Q459
	// measured: a human cancel and a genuine job failure publish the same
	// Failed/empty-reason shape with no deletion mark (Q502).
	ExternallyDeleted bool
}

// PodWaiter blocks until a worker pod reaches a terminal phase. It abstracts the
// completion-detection mechanism so the Provisioner can be wired with either the
// event-driven [InformerPodWaiter] (production) or the poll fallback (unit tests
// with a fake client; see Provisioner.waitForPodCompletion).
type PodWaiter interface {
	// WaitForCompletion blocks until the named pod reaches a terminal phase or
	// ctx is cancelled. A pod that is deleted before reaching a terminal phase is
	// reported as PodSucceeded with an empty reason.
	WaitForCompletion(ctx context.Context, namespace, name string) (PodOutcome, error)
}

// terminalPhase reports whether pod has reached a terminal phase, returning the
// outcome it resolves a waiter with. The set matches the legacy poll loop exactly:
// Succeeded, Failed, and Unknown are terminal; Pending and Running are not.
func terminalPhase(pod *corev1.Pod) (PodOutcome, bool) {
	switch pod.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed, corev1.PodUnknown:
		return PodOutcome{
			Phase:             pod.Status.Phase,
			Reason:            pod.Status.Reason,
			Preempted:         PreemptedByScheduler(pod),
			ExternallyDeleted: !pod.DeletionTimestamp.IsZero() && !deletedByAGC(pod),
		}, true
	default:
		return PodOutcome{}, false
	}
}

// podResult is delivered to a blocked waiter when its pod resolves. It also
// carries the optional pod-creation latency observation so resolve can emit it
// exactly once per pod (on the first resolving event that still has waiters).
type podResult struct {
	outcome PodOutcome
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
func (w *InformerPodWaiter) WaitForCompletion(ctx context.Context, namespace, name string) (PodOutcome, error) {
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
		if out, ok := terminalPhase(&pod); ok {
			log.Debug("pod already terminal at registration",
				"phase", out.Phase, "reason", out.Reason, "preempted", out.Preempted,
				"externallyDeleted", out.ExternallyDeleted)
			return out, nil
		}
		log.Debug("registered for pod completion; pod not yet terminal", "phase", pod.Status.Phase)
	case apierrors.IsNotFound(err):
		// Not yet synced or already deleted — wait for an event.
		log.Debug("registered for pod completion; pod not yet in cache, awaiting event")
	default:
		return PodOutcome{}, fmt.Errorf("provisioner: pod waiter cache get: %w", err)
	}

	select {
	case <-ctx.Done():
		log.Debug("pod wait cancelled before completion", "error", ctx.Err())
		return PodOutcome{}, ctx.Err()
	case res := <-ch:
		log.Debug("pod completion observed",
			"phase", res.outcome.Phase, "reason", res.outcome.Reason, "preempted", res.outcome.Preempted,
			"externallyDeleted", res.outcome.ExternallyDeleted)
		return res.outcome, nil
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
	if out, ok := terminalPhase(pod); ok {
		latency, valid := podCreationLatency(pod)
		w.resolve(pod.Namespace+"/"+pod.Name, podResult{
			outcome:      out,
			namespace:    pod.Namespace,
			latency:      latency,
			latencyValid: valid,
		})
	}
}

// onPodDelete resolves waiters when a pod is deleted before reaching a terminal
// phase, matching the legacy poll's "deleted externally → treat as completion".
//
// This is the path a preemption most often takes, and the only one that can observe it:
// the scheduler removes its victim by deleting it, and a victim held Pending (or killed
// before the informer sees its terminal phase) publishes no terminal phase at all. The
// delivered object is the pod's last-known state — including the DisruptionTarget
// condition the scheduler stamped before the delete — so the marker is read off it here
// rather than from a re-Get that would race the object's removal (Q497).
//
// ExternallyDeleted is deliberately NOT set here. Q459's decision gates drained-worker
// recovery on the deletion mark being present when a terminal phase publishes; a pod
// that vanished without one never ran its job to a reportable end, and this is also the
// path the reaper's pending-deadline deletions resolve through.
func (w *InformerPodWaiter) onPodDelete(obj any) {
	pod := podFromDeleteObj(obj)
	if pod == nil {
		return
	}
	w.resolve(pod.Namespace+"/"+pod.Name, podResult{
		outcome: PodOutcome{Phase: corev1.PodSucceeded, Preempted: PreemptedByScheduler(pod)},
	})
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
