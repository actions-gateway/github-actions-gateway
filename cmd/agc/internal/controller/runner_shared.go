package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/token"
	"github.com/actions-gateway/github-actions-gateway/broker"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
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

// channelEventRecorder implements listener.EventRecorder by pushing each event onto
// a buffered channel the reconciler drains. The send is non-blocking: a full channel
// drops the event rather than stalling the listener/provisioner goroutine, matching
// channelConditionUpdater. Events are an incident-visibility complement to the
// always-present metrics/conditions, so a dropped event under extreme backpressure
// is acceptable.
type channelEventRecorder struct {
	ch chan<- eventRecord
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
}

// workerPodPhaseChangePredicate restricts a worker-Pod watch to this project's
// worker pods (those carrying labelKey) and to the events that carry new status
// for the owning CR: Create, Delete, and phase-changing Updates. Generic events
// and non-phase Updates are dropped (status heartbeats don't change observable
// state). labelKey is LabelRunnerGroup (v1) or LabelRunnerSet (v2).
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
			return oldPod.Status.Phase != newPod.Status.Phase
		},
	}
}

// workerPodCounts is the per-owner count of worker pods by phase, returned
// alongside the reap result and written into the owner's status fields.
type workerPodCounts struct {
	active  int32 // PodRunning: a job is actively executing
	pending int32 // PodPending: pod spawned but not yet running
}

// reapWorkerPodsByLabel deletes worker pods (selected by labelKey == name in
// namespace) that the owning CR no longer needs: terminal pods older than ttl, and
// Pending pods older than deadline. It returns the time until the earliest
// retained pod becomes due (0 = none), pod counts by phase (for status), and any
// error. metrics may be nil; emitStuckPending (may be nil) records the owning-CR
// typed Event on a pending-deadline reap. Shared by both reconcilers' reapers so
// the reap logic is defined once.
func reapWorkerPodsByLabel(
	ctx context.Context,
	c client.Client,
	now time.Time,
	namespace, name, labelKey string,
	ttl, deadline time.Duration,
	log *slog.Logger,
	metrics *listener.Metrics,
	emitStuckPending func(podName string, deadline time.Duration),
) (time.Duration, workerPodCounts, error) {
	var pods corev1.PodList
	if err := c.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabels{labelKey: name},
	); err != nil {
		return 0, workerPodCounts{}, fmt.Errorf("reaper: list worker pods: %w", err)
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
			counts.active++
			continue
		case corev1.PodSucceeded, corev1.PodFailed, corev1.PodUnknown:
			due = podTerminalTime(pod).Add(ttl)
			reason = reapReasonCompletedTTL
		case corev1.PodPending:
			counts.pending++
			due = pod.CreationTimestamp.Add(deadline)
			reason = reapReasonPendingDeadline
		default:
			continue
		}

		if wait := due.Sub(now); wait > 0 {
			if next == 0 || wait < next {
				next = wait
			}
			continue
		}

		if err := c.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue
			}
			return next, counts, fmt.Errorf("reaper: delete worker pod %s: %w", pod.Name, err)
		}
		log.Info("reaped worker pod", "pod", pod.Name, "phase", pod.Status.Phase, "reason", reason)
		if metrics != nil {
			metrics.WorkerPodsReaped.WithLabelValues(namespace, name, reason).Inc()
		}
		if reason == reapReasonPendingDeadline && emitStuckPending != nil {
			emitStuckPending(pod.Name, deadline)
		}
	}
	return next, counts, nil
}

// assembleListenerConfig builds the listener.Config for a single goroutine bound
// to an already-claimed pool agent. The per-API parts — the job handler and admit
// gate (built from the v1 RunnerGroup or v2 RunnerSet provisioner Target) and the
// group identity — are passed in; everything else (broker client, lifecycle
// closures) is identical across both reconcilers, so it lives here.
func assembleListenerConfig(
	group, namespace string,
	brokerCfg BrokerConfig,
	condUpdater listener.ConditionUpdater,
	eventRecorder listener.EventRecorder,
	metrics *listener.Metrics,
	agent *agentpool.Agent,
	tokenManager *token.Manager,
	jobHandler listener.JobHandlerFunc,
	admit listener.AdmitFunc,
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
