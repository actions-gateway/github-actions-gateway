package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/token"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Owner-agnostic runtime machinery shared by the v1 RunnerGroup and v2 RunnerSet
// reconcilers. Both drive the same adaptive listener-goroutine pool and reap the
// same kind of worker pod; only the owning CR (and, for v2, the source of the pod
// shape and proxy) differs. Factoring the common pieces here keeps the two
// reconcilers in lockstep instead of letting copies drift.

// eventRecord is an owner-scoped Kubernetes Event pushed from a listener or
// provisioner goroutine to its reconciler via a channel. The reconciler records it
// on the live owner object on its next reconcile (drainEvents). It mirrors
// conditionUpdate: the goroutine that detects a job-lifecycle incident does not hold
// the live owner object the EventRecorder needs, so the event is routed back the
// same way condition updates are.
type eventRecord struct {
	namespace string
	name      string
	eventtype string
	reason    string
	action    string
	note      string
}

// channelEventRecorder implements runnercore.EventRecorder by pushing each event onto
// a buffered channel the reconciler drains. The send is non-blocking: a full channel
// drops the event rather than stalling the listener/provisioner goroutine, matching
// channelConditionUpdater. Events are an incident-visibility complement to the
// always-present metrics/conditions, so a dropped event under extreme backpressure
// is acceptable. After enqueuing, it wakes the reconciler (wake) so the pushed event
// is drained promptly rather than waiting for the next worker-Pod event or the resync
// period (Q333).
type channelEventRecorder struct {
	ch   chan<- eventRecord
	wake chan<- event.GenericEvent
}

func (e *channelEventRecorder) Event(namespace, name, eventtype, reason, action, note string) {
	select {
	case e.ch <- eventRecord{
		namespace: namespace,
		name:      name,
		eventtype: eventtype,
		reason:    reason,
		action:    action,
		note:      note,
	}:
	default:
		// Drop if the channel is full to avoid blocking the caller.
	}
	wakeReconciler(e.wake, namespace, name)
}

// wakeReconciler performs a best-effort, non-blocking send of an owner-scoped
// GenericEvent on wake so that a listener/provisioner-pushed condition or event wakes
// its reconciler promptly — instead of the pushed update sitting in conditionCh/eventCh
// until the next worker-Pod event or the resync drains it (Q333). A nil or full channel
// is a no-op: like the conditionCh/eventCh sends it mirrors, the wake is best-effort,
// and the update is still drained by the next natural reconcile. The GenericEvent
// carries only the owner's namespace/name (as PartialObjectMetadata), which the
// source.Channel handler (handler.EnqueueRequestForObject) maps straight to a
// reconcile.Request for that owner.
func wakeReconciler(wake chan<- event.GenericEvent, namespace, name string) {
	if wake == nil {
		return
	}
	select {
	case wake <- event.GenericEvent{Object: &metav1.PartialObjectMetadata{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}}:
	default:
		// Drop if the wake channel is full; the update is drained by the next reconcile.
	}
}

// pendingConditions makes a drained listener-pushed condition durable until the live
// owner object is observed to reflect it. A reconcile drains a condition from the
// channel (consuming it) and merges it into the object's status, but the subsequent
// status write can lose that merge: an IsConflict with a racing update is swallowed,
// and an early return may skip the write entirely. Because a listener-only condition
// (e.g. the ScaleSet Degraded/RateLimited signals, which the reconciler never
// re-derives) is pushed once, a lost drain would leave it missing until the next
// transition. This became easy to hit once a push wakes the reconciler promptly (Q333),
// landing the drain in a reconcile more likely to race a concurrent write.
//
// Entries are keyed by owner then condition type, so only the LATEST push per type is
// retained — preserving last-writer-wins and preventing a stale condition from
// resurrecting over a newer one. drainConditions re-applies each retained condition
// every reconcile and drops it once the live status reflects it. The reconciler is the
// sole writer of these listener conditions, so re-applying an unpersisted one never
// fights another writer.
type pendingConditions struct {
	mu sync.Mutex
	m  map[types.NamespacedName]map[string]metav1.Condition
}

// retain records cond as the latest unpersisted push for its owner and type.
func (p *pendingConditions) retain(key types.NamespacedName, cond metav1.Condition) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = make(map[types.NamespacedName]map[string]metav1.Condition)
	}
	byType := p.m[key]
	if byType == nil {
		byType = make(map[string]metav1.Condition)
		p.m[key] = byType
	}
	byType[cond.Type] = cond
}

// apply re-merges any still-unpersisted retained conditions for key into conds and drops
// those the live conds already reflect (matched on Status+Reason+Message — the fields a
// status write persists). Call it against the freshly-fetched status at the start of a
// drain so a condition an earlier reconcile failed to persist is retried here.
func (p *pendingConditions) apply(key types.NamespacedName, conds *[]metav1.Condition) {
	p.mu.Lock()
	defer p.mu.Unlock()
	byType := p.m[key]
	for t, cond := range byType {
		if cur := meta.FindStatusCondition(*conds, t); cur != nil &&
			cur.Status == cond.Status && cur.Reason == cond.Reason && cur.Message == cond.Message {
			delete(byType, t) // live status reflects it: persisted, stop retrying
			continue
		}
		meta.SetStatusCondition(conds, cond) // not yet persisted: re-apply for this reconcile
	}
	if len(byType) == 0 {
		delete(p.m, key)
	}
}

// forget drops all retained conditions for key. Call it when the owner is deleted or
// falls out of scope so no entry leaks after its last reconcile.
func (p *pendingConditions) forget(key types.NamespacedName) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, key)
}

// retryBackoffCap bounds the per-item requeue delay of both reconcilers' work
// queues, replacing the 1000s cap of client-go's default controller rate limiter.
//
// The worker-pod reaper's deadline is carried by nothing but the RequeueAfter of
// the reconcile that computed it: a pod sitting Pending emits no further watch
// event (workerPodPhaseChangePredicate passes phase changes, not status
// heartbeats), so no other wake-up is scheduled between the pod's creation and its
// pendingPodDeadline. controller-runtime discards RequeueAfter whenever the
// reconcile also returns an error (vendored v0.24.1,
// internal/controller/controller.go), so a run of reconcile errors — a
// Status().Update optimistic-lock conflict is the routine one, and one lands
// within a second of worker-pod creation — leaves the reap to the rate-limited
// retry alone. At the client-go default that retry escalates to 1000s, so a
// stuck-Pending worker could hold its concurrency-ceiling slot and its node
// reservation for ~17 minutes past the deadline, with the operator's
// WorkerPodStuckPending event delayed with it.
//
// 30s bounds how late a reap can be without making a genuinely failing reconcile
// hammer the API server; the exponential ramp below the cap is unchanged.
const retryBackoffCap = 30 * time.Second

// reconcileRateLimiter builds the shared work-queue rate limiter. It is
// client-go's DefaultTypedControllerRateLimiter with retryBackoffCap substituted
// for the 1000s per-item cap: same exponential ramp, same 10 qps overall token
// bucket.
func reconcileRateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Millisecond, retryBackoffCap),
		&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)
}

// workerPodPhaseChangePredicate restricts a worker-Pod watch to this project's
// worker pods (those carrying labelKey) and to the events that carry new status
// for the owning CR: Create, Delete, phase-changing Updates, and the Update where a
// pod newly becomes a scheduler-preemption victim. Generic events and every other
// Update are dropped (status heartbeats don't change observable state). labelKey is
// LabelRunnerGroup (v1) or LabelRunnerSet (v2).
//
// The preemption edge exists because that disruption changes no phase (Q497). The
// scheduler stamps a DisruptionTarget condition and deletes the victim, which can leave
// a Pending worker Pending for its whole termination grace period — so on the
// phase-change edge alone the first event the reconciler would see is the Delete, by
// which point the pod is out of the cache and the scale-set recovery scan has nothing
// left to read the workflow-run identity off. It fires at most once per pod (the
// condition is only added, never removed) and only for pods already carrying the
// worker label, so it adds no steady-state reconcile traffic.
func workerPodPhaseChangePredicate(labelKey string) predicate.Predicate {
	hasLabel := func(obj client.Object) bool {
		_, ok := obj.GetLabels()[labelKey]
		return ok
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return hasLabel(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return hasLabel(e.Object) },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !hasLabel(e.ObjectNew) {
				return false
			}
			oldPod, ok1 := e.ObjectOld.(*corev1.Pod)
			newPod, ok2 := e.ObjectNew.(*corev1.Pod)
			if !ok1 || !ok2 {
				return false
			}
			if oldPod.Status.Phase != newPod.Status.Phase {
				return true
			}
			return !provisioner.PreemptedByScheduler(oldPod) && provisioner.PreemptedByScheduler(newPod)
		},
	}
}

// workerPodCounts is the per-owner count of worker pods by phase, returned
// alongside the reap result and written into the owner's status fields.
type workerPodCounts struct {
	active  int32 // PodRunning: a job is actively executing
	pending int32 // PodPending: pod spawned but not yet running
}

// completedJobRunningGrace is how long a worker pod may still be Running after
// GitHub declared its job terminal (provisioner.AnnotationJobCompletedAt) before the
// reaper deletes it. A runner that actually ran the job reports completion and then
// exits within seconds, so anything still Running well past the grace is stuck: a
// scale-set worker that registered but never received its job, or a pod held open by
// a container that outlives the runner (see the reap-blocking-sidecar condition).
//
// It is a constant rather than a CRD knob because it measures runner shutdown, not
// tenant workload: the job is already over at GitHub either way, so the only thing a
// premature reap costs is the terminal pod's completedPodTTL inspection window. Five
// minutes is far above observed runner shutdown and still bounds the leak.
const completedJobRunningGrace = 5 * time.Minute

// completedJobPendingGrace is how long a worker pod may still be Pending after GitHub
// declared its job terminal before the reaper deletes it. Such a pod cannot run — its
// job is over — and usually cannot even start, because the completion reclaimed the
// JIT-config Secret it mounts (Q575).
//
// It is far shorter than completedJobRunningGrace because there is no runner shutdown to
// wait out: the only thing the grace buys is letting a pod that was already mid-start
// reach Running, so the Running arm and its own grace own it instead of this one.
const completedJobPendingGrace = 30 * time.Second

// reapHooks are the owner-bound side effects the shared reaper calls out to. Every
// field is optional; a nil hook is skipped. They are grouped rather than passed
// positionally because both reconcilers wire several and the list only grows.
type reapHooks struct {
	// emitStuckPending, emitOrphanedRunning and emitLifetimeExceeded record the
	// owning-CR typed Event for a pending-deadline, an orphaned-running and a
	// lifetime-cap reap respectively.
	emitStuckPending     func(podName string, deadline time.Duration)
	emitOrphanedRunning  func(podName string, grace time.Duration)
	emitLifetimeExceeded func(podName string)
	// emitCompletedPending records the Event for a pod reaped while still Pending
	// because its job had already completed (Q575).
	emitCompletedPending func(podName string, grace time.Duration)
	// deregisterRunner removes the GitHub runner record a scale-set worker registered,
	// called with the name from provisioner.AnnotationRunnerName just before the pod is
	// deleted (Q550). Only the scale-set tier wires it, and only a pod carrying the
	// annotation reaches it. Best-effort: it owns its own logging and its outcome never
	// changes whether the pod is reaped.
	deregisterRunner func(ctx context.Context, runnerName string)
	// recoverAbandoned force-cancels the run behind a worker pod the deadline reap just
	// removed while it was still Pending, and queues it for automatic re-run once
	// capacity returns (Q766). Called with the deleted pod, and only for a
	// pending_deadline reap — the one reason that means the job was acquired, never ran,
	// and is not already concluded at GitHub. Only the scale-set tier wires it: on
	// classic the goroutine that owns the pod performs the same recovery from the
	// informer's delete event. Best-effort and non-blocking; it owns its own logging.
	recoverAbandoned func(ctx context.Context, pod *corev1.Pod)
}

// reapTarget binds a reap to one owning CR: which worker pods it selects, where it
// reports, and the owner-bound side effects. Both reap entry points build one so the
// select-stamp-deregister-delete sequence is written once.
type reapTarget struct {
	c                         client.Client
	namespace, name, labelKey string
	log                       *slog.Logger
	metrics                   *runnercore.Metrics
	hooks                     reapHooks
}

func (t reapTarget) list(ctx context.Context) (*corev1.PodList, error) {
	var pods corev1.PodList
	if err := t.c.List(ctx, &pods,
		client.InNamespace(t.namespace),
		client.MatchingLabels{t.labelKey: t.name},
	); err != nil {
		return nil, fmt.Errorf("reaper: list worker pods: %w", err)
	}
	return &pods, nil
}

// delete stamps pod with reason and deletes it, deregistering its GitHub runner
// record first when it has one. It reports false with a nil error for a pod that
// vanished under it — already reaped, nothing to account for.
func (t reapTarget) delete(ctx context.Context, pod *corev1.Pod, reason string) (bool, error) {
	// Stamp the deletion as the AGC's own before issuing it, so neither tier's
	// graceful-deletion recovery reads a reaper delete as a disruption and re-runs
	// a job the AGC itself gave up on (Q502). Stamp-then-delete: a stamp that lands
	// without its delete only suppresses recovery for a pod the AGC had already
	// condemned, while the reverse order would leave a re-run trigger.
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[provisioner.AnnotationDeletionReason] = reason
	if err := t.c.Patch(ctx, pod, patch); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return false, nil
		}
		return false, fmt.Errorf("reaper: mark worker pod %s for deletion: %w", pod.Name, err)
	}
	// Deregister the pod's runner record before deleting the pod, so a delete that
	// fails still leaves the registration clean — the reverse order is what leaks
	// (Q550). A pod with no runner-name annotation (every classic worker) skips it.
	if runnerName := pod.Annotations[provisioner.AnnotationRunnerName]; runnerName != "" && t.hooks.deregisterRunner != nil {
		t.hooks.deregisterRunner(ctx, runnerName)
	}
	if err := t.c.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return false, nil
		}
		return false, fmt.Errorf("reaper: delete worker pod %s: %w", pod.Name, err)
	}
	t.log.Info("reaped worker pod", "pod", pod.Name, "phase", pod.Status.Phase, "reason", reason)
	if t.metrics != nil {
		// runner_set aliases name on scale-set reaps so the reap series join the
		// runner_set-labelled scaleset_* gauges; runner_group carries name on both
		// tiers unchanged (Q514).
		runnerSet := ""
		if t.labelKey == provisioner.LabelRunnerSet {
			runnerSet = t.name
		}
		t.metrics.WorkerPodsReaped.WithLabelValues(t.namespace, t.name, runnerSet, reason).Inc()
	}
	return true, nil
}

// reapAllWorkerPodsByLabel deletes every worker pod the owning CR still has, with no
// TTL, deadline or phase test — the teardown counterpart of reapWorkerPodsByLabel,
// used when the owning ActionsGateway is terminating and the AGC is about to stop
// being the pods' reaper (Q547). It returns how many deletes it issued. Pods already
// carrying a deletion timestamp are skipped: the kubelet finishes those with no
// controller involved, which is the state this whole path is trying to reach.
func reapAllWorkerPodsByLabel(
	ctx context.Context,
	c client.Client,
	namespace, name, labelKey, reason string,
	log *slog.Logger,
	metrics *runnercore.Metrics,
	hooks reapHooks,
) (int, error) {
	target := reapTarget{c: c, namespace: namespace, name: name, labelKey: labelKey, log: log, metrics: metrics, hooks: hooks}
	pods, err := target.list(ctx)
	if err != nil {
		return 0, err
	}
	var reaped int
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}
		deleted, err := target.delete(ctx, pod, reason)
		if err != nil {
			return reaped, err
		}
		if deleted {
			reaped++
		}
	}
	return reaped, nil
}

// reapWorkerPodsByLabel deletes worker pods (selected by labelKey == name in
// namespace) that the owning CR no longer needs: terminal pods older than ttl,
// Pending pods older than deadline, and pods still Pending or Running more than
// completedJobPendingGrace / completedJobRunningGrace after their job completed at
// GitHub. It returns the time until the earliest retained pod becomes due (0 = none),
// pod counts by phase (for status), and any error. metrics may be nil. Shared by both
// reconcilers' reapers so the reap logic is defined once.
func reapWorkerPodsByLabel(
	ctx context.Context,
	c client.Client,
	now time.Time,
	namespace, name, labelKey string,
	ttl, deadline time.Duration,
	log *slog.Logger,
	metrics *runnercore.Metrics,
	hooks reapHooks,
) (time.Duration, workerPodCounts, error) {
	target := reapTarget{c: c, namespace: namespace, name: name, labelKey: labelKey, log: log, metrics: metrics, hooks: hooks}
	pods, err := target.list(ctx)
	if err != nil {
		return 0, workerPodCounts{}, err
	}

	var next time.Duration
	var counts workerPodCounts
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}

		var due time.Time
		var reason string
		switch pod.Status.Phase {
		case corev1.PodRunning:
			// Running is normally a job executing, and counts as active either way —
			// the pod holds its concurrency slot until it is actually gone. It gets a
			// deadline only once its job is over at GitHub: an unstamped pod (every
			// classic pod, and every scale-set pod whose job is still assigned) is
			// retained with no deadline, exactly as before (Q420).
			counts.active++
			completedAt, ok := jobCompletedAt(pod)
			if !ok {
				continue
			}
			due = completedAt.Add(completedJobRunningGrace)
			reason = reapReasonOrphanedRunning
		case corev1.PodSucceeded, corev1.PodFailed, corev1.PodUnknown:
			due = provisioner.PodTerminalTime(pod).Add(ttl)
			reason = reapReasonCompletedTTL
			// A pod the kubelet killed for exceeding activeDeadlineSeconds is
			// retained and reaped exactly like any other terminal pod — only its
			// label and Event differ, so the lifetime cap firing is legible instead
			// of arriving as a mystery termination under completed_ttl (Q438).
			if pod.Status.Reason == podReasonDeadlineExceeded {
				reason = reapReasonLifetimeExceeded
			}
		case corev1.PodPending:
			counts.pending++
			// A Pending pod whose job is already over at GitHub will never start: its
			// JIT-config Secret is reclaimed on that completion, and a pod that has not
			// mounted yet cannot mount a Secret that is gone. Waiting out
			// pendingPodDeadline (10m by default) held a concurrency slot and a node for
			// a pod that had nothing to run, and reported it as a scheduling stall
			// (Q575). The stamp is the same one the Running arm reads.
			if completedAt, ok := jobCompletedAt(pod); ok {
				due = completedAt.Add(completedJobPendingGrace)
				reason = reapReasonCompletedPending
			} else {
				due = pod.CreationTimestamp.Add(deadline)
				reason = reapReasonPendingDeadline
			}
		default:
			continue
		}

		if wait := due.Sub(now); wait > 0 {
			if next == 0 || wait < next {
				next = wait
			}
			continue
		}

		deleted, err := target.delete(ctx, pod, reason)
		if err != nil {
			return next, counts, err
		}
		if !deleted {
			continue
		}
		if reason == reapReasonPendingDeadline && hooks.emitStuckPending != nil {
			hooks.emitStuckPending(pod.Name, deadline)
		}
		// The job this pod was created for was acquired and never ran, and — unlike a
		// completed_pending reap, which the branch above separates out by the
		// job-completed-at stamp — its run is still open at GitHub. Conclude it fast and
		// queue it for re-run (Q766). Passed the pod as the reaper still holds it: this
		// is the scale-set tier's only sighting of it, and the classic tier's own
		// recovery runs off the informer's delete event instead.
		if reason == reapReasonPendingDeadline && hooks.recoverAbandoned != nil {
			hooks.recoverAbandoned(ctx, pod)
		}
		if reason == reapReasonOrphanedRunning && hooks.emitOrphanedRunning != nil {
			hooks.emitOrphanedRunning(pod.Name, completedJobRunningGrace)
		}
		if reason == reapReasonLifetimeExceeded && hooks.emitLifetimeExceeded != nil {
			hooks.emitLifetimeExceeded(pod.Name)
		}
		if reason == reapReasonCompletedPending && hooks.emitCompletedPending != nil {
			hooks.emitCompletedPending(pod.Name, completedJobPendingGrace)
		}
	}
	return next, counts, nil
}

// jobCompletedAt reads the completion timestamp the provisioner stamps on a worker
// pod when its job goes terminal at GitHub. It reports false when the annotation is
// absent or unparseable — an unreadable value must never be treated as "completed
// long ago", which would reap a pod running a live job.
func jobCompletedAt(pod *corev1.Pod) (time.Time, bool) {
	v, ok := pod.Annotations[provisioner.AnnotationJobCompletedAt]
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// assembleListenerConfig builds the listener.Config for a single goroutine bound
// to an already-claimed pool agent. The per-API parts — the job handler and admit
// gate (built from the v1 RunnerGroup or v2 RunnerSet provisioner Target) and the
// group identity — are passed in; everything else (broker client, lifecycle
// closures) is identical across both reconcilers, so it lives here.
func assembleListenerConfig(
	group, namespace string,
	brokerCfg BrokerConfig,
	condUpdater runnercore.ConditionUpdater,
	eventRecorder runnercore.EventRecorder,
	metrics *runnercore.Metrics,
	agent *agentpool.Agent,
	tokenManager *token.Manager,
	jobHandler listener.JobHandlerFunc,
	admit runnercore.AdmitFunc,
	pool *agentpool.Pool,
) listener.Config {
	agentBrokerURL := agent.BrokerURL
	if agentBrokerURL == "" {
		agentBrokerURL = brokerCfg.BrokerURL
	}
	bc := &broker.Client{
		BrokerURL:     agentBrokerURL,
		RunnerVersion: brokerCfg.RunnerVersion,
		RunnerOS:      brokerCfg.RunnerOS,
		RunnerArch:    brokerCfg.RunnerArch,
		UseV2Flow:     brokerCfg.UseV2Flow,
		HTTPClient:    brokerCfg.HTTPClient,
	}
	return listener.Config{
		Group:             group,
		Namespace:         namespace,
		Agent:             agent,
		Broker:            bc,
		HTTPClient:        brokerCfg.HTTPClient,
		Conditions:        condUpdater,
		Events:            eventRecorder,
		Metrics:           metrics,
		RunnerOS:          brokerCfg.RunnerOS,
		JobHandler:        jobHandler,
		Admit:             admit,
		IdleThreshold:     brokerCfg.IdleThreshold,
		RenewInterval:     brokerCfg.RenewJobInterval,
		FanoutCompletion:  brokerCfg.FanoutCompletion,
		ReleaseAgent:      func() { pool.ReleaseAgent(agent) },
		MarkAgentConsumed: func() { pool.MarkConsumed(agent) },
		RecycleAgent: func(ctx context.Context) (*agentpool.Agent, error) {
			tok, err := tokenManager.Token(ctx)
			if err != nil {
				return nil, fmt.Errorf("installation token for agent recycle: %w", err)
			}
			return pool.Recycle(ctx, agent, tok)
		},
	}
}
