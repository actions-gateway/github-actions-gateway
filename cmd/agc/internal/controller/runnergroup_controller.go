// Package controller implements the RunnerGroup reconciler.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/token"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/tracing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const finalizerName = "actions-gateway.github.com/agentpool-cleanup"

// tracer is the OpenTelemetry tracer for the reconciler. It resolves to the
// global provider, which is the no-op provider unless main.go's tracing.Init
// installed an exporter — so the reconcile span costs almost nothing when
// tracing is off.
var tracer = otel.Tracer(tracing.InstrumentationName)

// conditionUpdate is sent from listener goroutines to the reconciler via a channel.
type conditionUpdate struct {
	namespace string
	name      string
	condition metav1.Condition
}

// channelConditionUpdater implements runnercore.ConditionUpdater. After enqueuing, it
// wakes the reconciler (wake) so the pushed condition is drained promptly rather than
// waiting for the next worker-Pod event or the resync period (Q333).
type channelConditionUpdater struct {
	ch   chan<- conditionUpdate
	wake chan<- event.GenericEvent
}

func (u *channelConditionUpdater) SetCondition(namespace, name string, cond metav1.Condition) {
	select {
	case u.ch <- conditionUpdate{namespace: namespace, name: name, condition: cond}:
	default:
		// Drop if channel is full to avoid blocking listener goroutines.
	}
	wakeReconciler(u.wake, namespace, name)
}

// RunnerGroupReconciler reconciles RunnerGroup objects.
type RunnerGroupReconciler struct {
	client.Client
	TokenManager *token.Manager
	Registrar    agentpool.Registrar
	BrokerConfig BrokerConfig
	Metrics      *runnercore.Metrics
	Log          *slog.Logger
	Provisioner  *provisioner.Provisioner
	AgentKeyType agentpool.KeyType // defaults to KeyTypeRSA (the secure default) when empty

	// Recorder emits Kubernetes Events on the reconciled RunnerGroup so that
	// credential, agent-pool, and listener failures surface in `kubectl describe
	// runnergroup`. May be nil in unit tests; callers must nil-check before use.
	Recorder events.EventRecorder

	// Now is the clock used by the worker-pod reaper. Nil means time.Now;
	// tests inject a fixed clock to exercise TTL/deadline expiry.
	Now func() time.Time

	// stopped is set by stopListeners when the manager's shutdown drain begins, and is
	// never cleared. The classic tier's twin of the RunnerSet flag (Q968): a
	// multiplexer started after the drain leaks the broker sessions it opens.
	stopped atomic.Bool

	// BaselineRecheckInterval is the cadence at which a RunnerGroup is requeued
	// while its multiplexer is below the desired listener count, so the permanent
	// baseline is revived promptly after a non-retriable listener exit (Q137).
	// Zero selects defaultBaselineRecheckInterval; tests set a small value.
	BaselineRecheckInterval time.Duration

	// in-process state; rebuilt from Secrets on restart.
	multiplexersMu sync.Mutex
	multiplexers   map[types.NamespacedName]*listener.Multiplexer
	poolsMu        sync.Mutex
	pools          map[types.NamespacedName]*agentpool.Pool

	conditionCh chan conditionUpdate

	// eventCh carries owner-scoped Kubernetes Events pushed from listener/provisioner
	// goroutines; drainEvents records them on the live RunnerGroup each reconcile.
	eventCh chan eventRecord

	// wakeCh delivers owner-scoped GenericEvents to the source.Channel registered in
	// SetupWithManager, so a listener/provisioner-pushed condition or event (drained
	// in Reconcile by drainConditions/drainEvents) wakes the reconciler immediately
	// rather than waiting for the next worker-Pod event or the resync period (Q333).
	// Best-effort and never closed: senders use a non-blocking send (wakeReconciler),
	// and the source's reader goroutine stops when the manager's context is cancelled.
	wakeCh chan event.GenericEvent

	// pendingConds retains listener-pushed conditions drained but not yet persisted, so a
	// status-write conflict does not lose a one-shot listener condition (Q333).
	pendingConds pendingConditions

	// reconcileCount counts Reconcile invocations. Test-only observability (see
	// ReconcileCountForTest) — it lets integration tests assert that an external
	// event such as a worker Pod lifecycle event actually triggered a reconcile.
	reconcileCount atomic.Int64
}

// BrokerConfig holds the connection parameters for the broker client used by
// listener goroutines.
type BrokerConfig struct {
	BrokerURL     string
	RunnerVersion string
	RunnerOS      string
	RunnerArch    string
	UseV2Flow     bool
	HTTPClient    *http.Client
	// IdleThreshold is the number of consecutive empty polls before a burst
	// listener goroutine shuts down. 0 means the default (50).
	IdleThreshold int
	// RenewJobInterval is the cadence of the per-job RenewJob renewal loop.
	// 0 means the default (60s).
	RenewJobInterval time.Duration
	// FanoutCompletion enables the Q260 Option A: the winner of a fanned-out job fans
	// completejob out to every deduped sibling delivery on completion so GitHub does
	// not cancel the whole job at its ~15-minute unstarted-job timeout. ON by default;
	// the run service's per-delivery completion semantics were live-confirmed by the
	// re-route #5 dogfood experiment — see listener.Config.FanoutCompletion.
	FanoutCompletion bool
}

// condUpdater returns a ConditionUpdater that enqueues onto conditionCh and wakes the
// reconciler (wakeCh) so the pushed condition is drained promptly (Q333).
func (r *RunnerGroupReconciler) condUpdater() *channelConditionUpdater {
	return &channelConditionUpdater{ch: r.conditionCh, wake: r.wakeCh}
}

// eventRecorder returns an EventRecorder that enqueues onto eventCh and wakes the
// reconciler (wakeCh) so the pushed event is drained promptly (Q333).
func (r *RunnerGroupReconciler) eventRecorder() *channelEventRecorder {
	return &channelEventRecorder{ch: r.eventCh, wake: r.wakeCh}
}

func (r *RunnerGroupReconciler) ensureMaps() {
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.multiplexers == nil {
		r.multiplexers = make(map[types.NamespacedName]*listener.Multiplexer)
	}
	if r.pools == nil {
		r.pools = make(map[types.NamespacedName]*agentpool.Pool)
	}
	if r.conditionCh == nil {
		r.conditionCh = make(chan conditionUpdate, 256)
	}
	if r.eventCh == nil {
		r.eventCh = make(chan eventRecord, 256)
	}
	if r.wakeCh == nil {
		r.wakeCh = make(chan event.GenericEvent, 256)
	}
}

// SetupWithManager registers the reconciler with the controller-runtime manager.
func (r *RunnerGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.ensureMaps()
	// Route provisioner-side quota/eviction-retry-exhaustion Events for this v1 path
	// through the same channel the listener path uses (runnerGroupTarget is the only
	// Target the shared Provisioner constructs, and it is v1-only).
	if r.Provisioner != nil && r.Provisioner.Events == nil {
		r.Provisioner.Events = r.eventRecorder()
	}

	// Export the worker ResourceQuota conditions (Q82) and the WorkersUnschedulable
	// condition (Q157) as gauges from the cached client, so they can be alerted on
	// without kube-state-metrics.
	registerWorkerQuotaMetrics(mgr.GetClient())
	registerWorkersUnschedulableMetrics(mgr.GetClient())

	// Drain listener goroutines inside the manager's graceful shutdown so SIGTERM
	// cannot kill the process mid-DELETE and leak GitHub-side sessions (Q222).
	if err := mgr.Add(&listenerShutdown{stop: r.stopListeners, log: r.Log, owner: "RunnerGroup"}); err != nil {
		return fmt.Errorf("add listener shutdown runnable: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		// Bounded retry backoff so a reconcile error cannot strand the worker-pod
		// reaper's deadline (retryBackoffCap).
		WithOptions(controller.Options{RateLimiter: reconcileRateLimiter()}).
		For(&v1alpha1.RunnerGroup{}).
		// Reconcile when an admin changes a namespace ResourceQuota's .spec.hard so
		// the WorkerQuota{Pressure,Exceeded} conditions refresh promptly (Q82). The
		// predicate ignores .status.used churn; transient used drift from other
		// workloads is picked up by the worker-pod watch and the baseline recheck.
		Watches(
			&corev1.ResourceQuota{},
			handler.EnqueueRequestsFromMapFunc(r.quotaToRunnerGroups),
			builder.WithPredicates(quotaHardChangedPredicate()),
		).
		// Watch the worker pods this RunnerGroup's provisioner creates so that
		// pod lifecycle events — a job being acquired (pod created), a pod
		// reaching a terminal phase, eviction (phase → Failed), and deletion —
		// re-trigger a reconcile. Without this the controller only reconciles on
		// RunnerGroup writes, so status.ActiveSessions and any listener-pushed
		// conditions go stale between Generation bumps (k8s-best-practices §A
		// A3 / Q63), and the worker-pod reaper (Q95) would never see the phase
		// transitions that start its completedPodTTL clock. The watch reuses
		// the manager's shared Pod informer (the same one Q64's
		// InformerPodWaiter drives), so it adds no second cache.
		//
		// Deliberately Pods only: the A3 finding also names agent Secrets, but a
		// Secret watch would establish a Secret informer and cache Secret material
		// in-process, violating W3/H-2 (no Secret bodies in cache). The manager's
		// DisableFor[*corev1.Secret] and the absence of any Secret Watch are
		// load-bearing security properties, so the Secret half is intentionally
		// not implemented. The AGC Role's Secret rule therefore omits the watch
		// verb (Q26).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.podToRunnerGroup),
			builder.WithPredicates(workerPodPredicate()),
		).
		// Wake the reconciler when a listener/provisioner goroutine pushes a condition
		// or event onto conditionCh/eventCh (Q333). Without this the pushed update sits
		// in the channel until the next worker-Pod event or the resync period drains it,
		// so an otherwise-idle RunnerGroup could lag a status condition up to 10h. The
		// GenericEvents carry the owning RunnerGroup's namespace/name, which
		// EnqueueRequestForObject maps straight to its reconcile.Request.
		WatchesRawSource(source.Channel(r.wakeCh, &handler.EnqueueRequestForObject{})).
		Complete(r)
}

// podToRunnerGroup maps a worker Pod event to a reconcile request for the
// RunnerGroup that owns it. Worker pods carry the owning group's name in the
// provisioner.LabelRunnerGroup label and run in the group's namespace. A Pod
// without the label maps to no request (defence-in-depth; workerPodPredicate
// already filters these out).
func (r *RunnerGroupReconciler) podToRunnerGroup(_ context.Context, obj client.Object) []ctrl.Request {
	rgName := obj.GetLabels()[provisioner.LabelRunnerGroup]
	if rgName == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      rgName,
	}}}
}

// workerPodPredicate restricts the Pod watch to this project's worker pods and to
// the events that carry new information for the RunnerGroup's status (Create,
// Delete, phase-changing Update). See workerPodPhaseChangePredicate for the full
// rationale; this is the v1 binding keyed on LabelRunnerGroup.
func workerPodPredicate() predicate.Predicate {
	return workerPodPhaseChangePredicate(provisioner.LabelRunnerGroup)
}

// Reconcile is called by controller-runtime on RunnerGroup events.
func (r *RunnerGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	// Root span for the reconcile. Each RunnerGroup event is its own trace (there
	// is no inbound trace context); the provisioner's per-job spans form separate
	// traces driven off the listener goroutines, not children of this one. The
	// deferred closure stamps the span's error status from the named return.
	ctx, span := tracer.Start(ctx, "RunnerGroup.Reconcile", trace.WithAttributes(
		semconv.K8SNamespaceName(req.Namespace),
		attribute.String("gateway.runnergroup.name", req.Name),
	))
	defer func() {
		if retErr != nil {
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

	r.reconcileCount.Add(1)
	r.ensureMaps()
	log := r.Log.With("namespace", req.Namespace, "name", req.Name)

	// 1. Fetch the RunnerGroup.
	var rg v1alpha1.RunnerGroup
	if err := r.Get(ctx, req.NamespacedName, &rg); err != nil {
		if apierrors.IsNotFound(err) {
			// The object is gone (finalizer cleanup already completed, or it was
			// removed out from under us across a reconciler restart). Drop any
			// in-memory multiplexer/pool state for this key so it cannot leak.
			// Idempotent: a no-op when reconcileDelete already cleaned up.
			r.cleanupLocalState(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 2. Drain pending condition updates and Events from listener/provisioner goroutines.
	r.drainConditions(&rg)
	r.drainEvents(&rg)

	// 3. Handle deletion.
	if !rg.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, log, &rg)
	}

	// 4. Ensure finalizer.
	if !controllerutil.ContainsFinalizer(&rg, finalizerName) {
		controllerutil.AddFinalizer(&rg, finalizerName)
		if err := r.Update(ctx, &rg); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 4b. Reap expired worker pods (terminal past completedPodTTL, Pending past
	// pendingPodDeadline). Runs before the token fetch so cleanup keeps working
	// during a GitHub outage. reapAfter is the time until the earliest retained
	// pod becomes due; it is propagated as RequeueAfter below. counts is the
	// pod phase snapshot used to populate status.activeJobs/pendingJobs.
	reapAfter, podCounts, err := r.reapWorkerPods(ctx, log, &rg)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 4c. Diagnose worker pods stuck Pending because the scheduler cannot place
	// them (non-quota: no matching node / affinity / taints). Surfaced as the
	// WorkersUnschedulable condition (Q157) and folded into the requeue so it is
	// re-evaluated when a Pending pod crosses its grace. Computed from pods only, so
	// like the reaper it keeps working during a GitHub outage; the condition is
	// written with the rest of status at step 8.
	unsched := r.evalWorkersUnschedulable(ctx, &rg)

	// 5. Get installation token for agent management.
	instToken, err := r.TokenManager.Token(ctx)
	if err != nil {
		log.Error("failed to get installation token", "error", err)
		r.recordEvent(&rg, corev1.EventTypeWarning, "TokenUnavailable", "GetToken",
			"failed to obtain GitHub App installation token: %v", err)
		// Reconcile returns here before the normal status update, so surface the
		// failure as a CredentialUnavailable condition (not only the Event above)
		// before the early return (Q156). The error is returned so the reconcile
		// still requeues with backoff.
		return r.setCredentialUnavailable(ctx, &rg, err)
	}

	// 6. Ensure agent pool Secrets.
	pool := r.getOrCreatePool(req.NamespacedName, &rg)
	if err := pool.EnsureAgents(ctx, rg.Spec.MaxListeners, instToken); err != nil {
		log.Error("EnsureAgents failed", "error", err)
		r.recordEvent(&rg, corev1.EventTypeWarning, "AgentPoolError", "EnsureAgents",
			"failed to provision agent Secrets: %v", err)
		return ctrl.Result{}, err
	}

	// 7. Start or update the Multiplexer.
	// Pass a deep copy so the factory closure captures a snapshot that is not
	// subject to concurrent mutation by r.Status().Update below (which zeroes
	// the struct before writing the API response back into it).
	mux := r.getOrCreateMultiplexer(ctx, req.NamespacedName, rg.DeepCopy(), pool)
	mux.SetMaxListeners(rg.Spec.MaxListeners)
	// Restart the permanent baseline goroutine if all goroutines have exited
	// and at least one listener was requested. This recovers from the race where
	// the goroutine hit a pool-exhausted NonRetriableError at startup before
	// EnsureAgents finished populating the pool. Start is idempotent: when
	// ActiveCount is 0 only because a crashed baseline is waiting out its
	// restart backoff, this call is a no-op rather than stacking a second
	// permanent baseline (Q100).
	// Not measured, unlike the v2 scale-set twin: the same missing guard (Q968).
	if mux.ActiveCount() == 0 && rg.Spec.MaxListeners > 0 && !r.stopped.Load() {
		if startErr := mux.Start(ctx); startErr != nil {
			log.Warn("multiplexer restart failed", "error", startErr)
			r.recordEvent(&rg, corev1.EventTypeWarning, "ListenerStartFailed", "StartMultiplexer",
				"failed to restart listener goroutines: %v", startErr)
		}
	}

	// 8. Update status.
	rg.Status.ActiveSessions = mux.ActiveCount()
	rg.Status.ActiveJobs = podCounts.active
	rg.Status.PendingJobs = podCounts.pending
	rg.Status.ObservedGeneration = rg.Generation
	r.setReadyCondition(&rg, mux.ActiveCount() > 0)

	// Reaching here means the installation token was obtained, so clear any
	// CredentialUnavailable condition a prior token failure left set (Q156).
	r.mergeCondition(&rg, metav1.Condition{
		Type:               v1alpha1.ConditionCredentialUnavailable,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonCredentialAvailable,
		Message:            "GitHub App installation token obtained",
		ObservedGeneration: rg.Generation,
	})

	// Worker ResourceQuota conditions (Q82), advisory — they do not gate Ready.
	wq := r.evalWorkerQuota(ctx, &rg)
	r.mergeCondition(&rg, metav1.Condition{
		Type: conditionWorkerQuotaPressure, Status: boolConditionStatus(wq.pressure),
		Reason: wq.pressureReason, Message: wq.pressureMessage, ObservedGeneration: rg.Generation,
	})
	r.mergeCondition(&rg, metav1.Condition{
		Type: conditionWorkerQuotaExceeded, Status: boolConditionStatus(wq.exceeded),
		Reason: wq.exceededReason, Message: wq.exceededMessage, ObservedGeneration: rg.Generation,
	})

	// WorkersUnschedulable (Q157), impairing — it rolls up to the gateway. Emit a
	// Warning Event only on a genuine False→True transition, never every reconcile.
	wasUnsched := meta.IsStatusConditionTrue(rg.Status.Conditions, v1alpha1.ConditionWorkersUnschedulable)
	r.mergeCondition(&rg, metav1.Condition{
		Type: v1alpha1.ConditionWorkersUnschedulable, Status: boolConditionStatus(unsched.unschedulable),
		Reason: unsched.reason, Message: unsched.message, ObservedGeneration: rg.Generation,
	})
	if unsched.unschedulable && !wasUnsched {
		r.recordEvent(&rg, corev1.EventTypeWarning, "WorkersUnschedulable", "Reconcile", unsched.message)
	}

	// Judge the runner version the effective worker image ships against GitHub's
	// enforced minimum (Q715). Advisory, and it warns before GitHub's own rejection
	// does — that one arrives only once sessions have already started failing.
	r.setRunnerVersionStatus(&rg)

	if err := r.Status().Update(ctx, &rg); err != nil {
		return ctrl.Result{}, err
	}

	// Schedule the next reconcile. reapAfter (0 when no worker pods are retained)
	// covers the worker-pod reaper. Independently, while the multiplexer is below
	// its desired listener count — most importantly when a non-retriable listener
	// exit (e.g. a post-job agent-recycle failure) has left zero running
	// listeners — poll on a bounded interval so the ActiveCount()==0 recovery
	// above revives the permanent baseline promptly and status keeps tracking
	// reality. Without it nothing re-reconciles after such an exit until the next
	// worker-pod watch event or the 10h resync, leaving status.activeSessions and
	// Ready=True stale against a dead listener (Q137).
	requeueAfter := reapAfter
	// Re-check when the earliest still-Pending worker pod crosses its scheduling
	// grace, so WorkersUnschedulable flips without waiting for a phase-changing Pod
	// event (Q157).
	if unsched.requeueAfter > 0 && (requeueAfter <= 0 || unsched.requeueAfter < requeueAfter) {
		requeueAfter = unsched.requeueAfter
	}
	if rg.Spec.MaxListeners > 0 && mux.ActiveCount() < rg.Spec.MaxListeners {
		if interval := r.baselineRecheckInterval(); requeueAfter <= 0 || interval < requeueAfter {
			requeueAfter = interval
		}
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// defaultBaselineRecheckInterval is the cadence at which a RunnerGroup is
// requeued while its multiplexer is below the desired listener count, used when
// RunnerGroupReconciler.BaselineRecheckInterval is unset. 15s revives a dead
// permanent baseline well inside the e2e job budget while keeping reconcile
// churn modest at the per-tenant scale of a single AGC instance (Q137).
const defaultBaselineRecheckInterval = 15 * time.Second

// baselineRecheckInterval returns the configured baseline re-check cadence,
// defaulting when unset.
func (r *RunnerGroupReconciler) baselineRecheckInterval() time.Duration {
	if r.BaselineRecheckInterval > 0 {
		return r.BaselineRecheckInterval
	}
	return defaultBaselineRecheckInterval
}

// reconcileDelete handles RunnerGroup deletion: stop goroutines, delete Secrets, remove finalizer.
//
//nolint:dupl // v1 twin of RunnerSetReconciler.reconcileDelete; folds in when v1alpha1 retires
func (r *RunnerGroupReconciler) reconcileDelete(ctx context.Context, log *slog.Logger, rg *v1alpha1.RunnerGroup) (ctrl.Result, error) {
	key := types.NamespacedName{Namespace: rg.Namespace, Name: rg.Name}

	// Stop the multiplexer first so no new agents are claimed while we deregister.
	r.stopMultiplexer(key)

	// Delete agent Secrets.
	pool := r.getPool(key)
	if pool != nil {
		instToken, err := r.TokenManager.Token(ctx)
		if err != nil {
			log.Warn("could not get token for pool cleanup; proceeding without deregistration", "error", err)
			instToken = ""
		}
		if err := pool.DeleteAll(ctx, instToken); err != nil {
			r.recordEvent(rg, corev1.EventTypeWarning, "AgentDeregistrationFailed", "Delete",
				"failed to deregister/delete agent Secrets: %v", err)
			return ctrl.Result{}, fmt.Errorf("pool.DeleteAll: %w", err)
		}
	}

	// Drop any remaining in-memory state for this RunnerGroup.
	r.cleanupLocalState(key)

	controllerutil.RemoveFinalizer(rg, finalizerName)
	if err := r.Update(ctx, rg); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// stopMultiplexer stops and removes the in-memory multiplexer for key, if any.
func (r *RunnerGroupReconciler) stopMultiplexer(key types.NamespacedName) {
	r.multiplexersMu.Lock()
	if mux, ok := r.multiplexers[key]; ok {
		mux.Stop()
		delete(r.multiplexers, key)
	}
	r.multiplexersMu.Unlock()
}

// cleanupLocalState stops and removes any in-memory multiplexer and agent pool
// for the given RunnerGroup. It never touches the API server, so it is safe on
// both the deletion path and a NotFound reconcile, and it is idempotent —
// calling it more than once for the same key is a no-op.
func (r *RunnerGroupReconciler) cleanupLocalState(key types.NamespacedName) {
	r.stopMultiplexer(key)

	r.poolsMu.Lock()
	delete(r.pools, key)
	r.poolsMu.Unlock()

	r.pendingConds.forget(key)
}

// recordEvent emits a Kubernetes Event on the RunnerGroup when a Recorder is
// wired. The Recorder may be nil in unit tests, so callers go through here
// rather than dereferencing it directly.
func (r *RunnerGroupReconciler) recordEvent(rg *v1alpha1.RunnerGroup, eventtype, reason, action, note string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(rg, nil, eventtype, reason, action, note, args...)
}

// getOrCreatePool returns the Pool for the given RunnerGroup, creating it if needed.
// The owner reference is refreshed on every call, not only on creation: the pool is
// cached for the lifetime of the RunnerGroup key, so a group deleted and recreated
// under the same name must not leave the pool stamping the old UID (Q466).
func (r *RunnerGroupReconciler) getOrCreatePool(key types.NamespacedName, rg *v1alpha1.RunnerGroup) *agentpool.Pool {
	r.poolsMu.Lock()
	defer r.poolsMu.Unlock()
	p, ok := r.pools[key]
	if !ok {
		p = agentpool.NewPool(r.Client, rg.Namespace, rg.Name, r.BrokerConfig.RunnerVersion,
			rg.Spec.RunnerLabels, r.Registrar, r.AgentKeyType)
		if r.Metrics != nil {
			p.Metrics = r.Metrics
		}
		r.pools[key] = p
	}
	p.SetOwner(provisioner.RunnerGroupOwnerRef(rg))
	return p
}

func (r *RunnerGroupReconciler) getPool(key types.NamespacedName) *agentpool.Pool {
	r.poolsMu.Lock()
	defer r.poolsMu.Unlock()
	return r.pools[key]
}

// getOrCreateMultiplexer returns the Multiplexer for the given RunnerGroup, creating and starting it if needed.
func (r *RunnerGroupReconciler) getOrCreateMultiplexer(ctx context.Context, key types.NamespacedName, rg *v1alpha1.RunnerGroup, pool *agentpool.Pool) *listener.Multiplexer {
	r.multiplexersMu.Lock()
	defer r.multiplexersMu.Unlock()

	if mux, ok := r.multiplexers[key]; ok {
		return mux
	}

	condUpdater := r.condUpdater()
	brokerCfg := r.BrokerConfig

	factory := func(index int) listener.Config {
		agent := pool.ClaimAgent()
		if agent == nil {
			// Pool exhausted; return minimal config — goroutine will fail quickly.
			return listener.Config{
				Group:     rg.Name,
				Namespace: rg.Namespace,
			}
		}
		return r.newListenerConfig(rg, pool, brokerCfg, condUpdater, agent)
	}

	// Give the Multiplexer a logger scoped to this RunnerGroup so its
	// listener-lifecycle lines (spawn/restart/backoff) carry namespace/group
	// correlation; per-goroutine lines add agentIndex/sessionId beneath (Q87, Theme F).
	muxLog := r.Log.With("namespace", rg.Namespace, "group", rg.Name)
	mux := listener.NewMultiplexer(factory, rg.Spec.MaxListeners, muxLog)
	// Retain a completed job's planID claim until its terminal worker pod is
	// reaped (completedPodTTL), so a late GitHub redelivery of the same planID is
	// deduped rather than colliding on the lingering Completed pod (Q260 redelivery
	// residual). Zero completedPodTTL (pods reaped synchronously) leaves the
	// original delete-on-completion behavior.
	mux.ClaimLinger = provisioner.EffectiveCompletedPodTTL(rg)
	// The drain has run; starting here would open broker sessions nothing deletes
	// (Q968). The multiplexer is still returned and cached, unstarted, so the caller's
	// ActiveCount reads zero and it reports no listeners rather than failing.
	if r.stopped.Load() {
		return mux
	}
	if err := mux.Start(ctx); err != nil {
		r.Log.Error("failed to start multiplexer", "error", err)
	}
	r.multiplexers[key] = mux
	return mux
}

// newListenerConfig assembles the listener.Config for a single goroutine bound
// to the given already-claimed pool agent. Split out from the multiplexer
// factory so it can be unit-tested directly.
func (r *RunnerGroupReconciler) newListenerConfig(rg *v1alpha1.RunnerGroup, pool *agentpool.Pool, brokerCfg BrokerConfig, condUpdater runnercore.ConditionUpdater, agent *agentpool.Agent) listener.Config {
	jobHandler := listener.JobHandlerFunc(nil)
	admit := runnercore.AdmitFunc(nil)
	if r.Provisioner != nil {
		jobHandler = r.Provisioner.HandlerFor(rg)
		// Gate job acquisition on worker capacity before AcquireJob claims the
		// job from GitHub (Q59), so a job we cannot place is left queued for
		// redelivery instead of acquired-then-dropped.
		admit = r.Provisioner.AdmitFor(rg)
	}
	return assembleListenerConfig(rg.Name, rg.Namespace, brokerCfg, condUpdater, r.eventRecorder(), r.Metrics, agent, r.TokenManager, jobHandler, admit, pool)
}

// drainConditions reads pending condition updates and merges them into rg.Status.
// Updates for other RunnerGroups are collected and re-enqueued after the loop
// to avoid re-processing them in the current iteration.
func (r *RunnerGroupReconciler) drainConditions(rg *v1alpha1.RunnerGroup) {
	key := types.NamespacedName{Namespace: rg.Namespace, Name: rg.Name}
	// Re-apply any conditions a prior reconcile drained but did not persist (dropping
	// those the live status now reflects), then drain fresh pushes over the top so the
	// latest push per type wins (Q333).
	r.pendingConds.apply(key, &rg.Status.Conditions)
	var skipped []conditionUpdate
	for {
		select {
		case upd := <-r.conditionCh:
			if upd.namespace == rg.Namespace && upd.name == rg.Name {
				prev := meta.FindStatusCondition(rg.Status.Conditions, upd.condition.Type)
				if runnercore.DropListenerCondition(prev, upd.condition) {
					continue // refused: neither merged nor retained
				}
				r.mergeCondition(rg, upd.condition)
				r.pendingConds.retain(key, upd.condition)
			} else {
				skipped = append(skipped, upd)
			}
		default:
			goto done
		}
	}
done:
	for _, upd := range skipped {
		select {
		case r.conditionCh <- upd:
		default:
			// channel full — condition dropped (best-effort)
		}
	}
}

// drainEvents records pending owner-scoped Events on this RunnerGroup. Events for
// other RunnerGroups are re-enqueued after the loop (mirroring drainConditions) so
// they are recorded when their owner reconciles. Each event is consumed once, so
// this never re-emits the same Event on subsequent reconciles.
func (r *RunnerGroupReconciler) drainEvents(rg *v1alpha1.RunnerGroup) {
	var skipped []eventRecord
	for {
		select {
		case ev := <-r.eventCh:
			if ev.namespace == rg.Namespace && ev.name == rg.Name {
				r.recordEvent(rg, ev.eventtype, ev.reason, ev.action, ev.note)
			} else {
				skipped = append(skipped, ev)
			}
		default:
			goto done
		}
	}
done:
	for _, ev := range skipped {
		select {
		case r.eventCh <- ev:
		default:
			// channel full — event dropped (best-effort)
		}
	}
}

// mergeCondition upserts a condition into rg.Status.Conditions keyed by Type.
// It delegates to meta.SetStatusCondition so LastTransitionTime advances only on
// an actual status transition rather than being rewritten on every reconcile.
func (r *RunnerGroupReconciler) mergeCondition(rg *v1alpha1.RunnerGroup, cond metav1.Condition) {
	meta.SetStatusCondition(&rg.Status.Conditions, cond)
}

// setRunnerVersionStatus judges the runner version this group's effective worker image
// declares against GitHub's enforced registration minimum, and publishes the verdict as
// RunnerVersionTooOld (Q715). The classic listener sets the same condition when GitHub
// rejects a session, which only happens once the version already stopped working; this
// reads the image itself, so it warns ahead of that.
func (r *RunnerGroupReconciler) setRunnerVersionStatus(rg *v1alpha1.RunnerGroup) {
	if r.Provisioner == nil {
		return
	}
	cond := runnercore.WorkerRunnerVersionCondition(
		r.Provisioner.EffectiveWorkerImage(rg.Spec.WorkerImage), rg.Generation)

	prev := meta.FindStatusCondition(rg.Status.Conditions, cond.Type)
	// The two producers of this condition report different facts through one type. A
	// session-sourced VersionTooOld is GitHub rejecting agent.version, which is the
	// AGC's own pinned names.RunnerVersion and says nothing about the worker image; a
	// healthy image reading therefore does not refute it, and writing over it would
	// drop a live rejection from status.
	if prev != nil && prev.Status == metav1.ConditionTrue && prev.Reason == v1alpha1.ReasonVersionTooOld {
		return
	}
	r.mergeCondition(rg, cond)
	// Warn once on a genuine transition into too-old so the deadline lands in the event
	// stream while there is still time to change the image.
	if cond.Status == metav1.ConditionTrue && (prev == nil || prev.Status != metav1.ConditionTrue) {
		r.recordEvent(rg, corev1.EventTypeWarning, cond.Reason, "Reconcile", cond.Message)
	}
}

func (r *RunnerGroupReconciler) setReadyCondition(rg *v1alpha1.RunnerGroup, ready bool) {
	status := metav1.ConditionFalse
	reason := v1alpha1.ReasonNoActiveSessions
	msg := "No listener goroutines are running."
	if ready {
		status = metav1.ConditionTrue
		reason = v1alpha1.ReasonListenerActive
		msg = "At least one listener goroutine is running."
	}
	prev := meta.FindStatusCondition(rg.Status.Conditions, v1alpha1.ConditionReady)
	r.mergeCondition(rg, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: rg.Generation,
	})
	// Emit an Event only on a genuine Ready transition (or first observation),
	// never on every reconcile, to avoid event spam.
	if prev == nil || prev.Status != status {
		etype := corev1.EventTypeNormal
		if !ready {
			etype = corev1.EventTypeWarning
		}
		r.recordEvent(rg, etype, reason, "Reconcile", msg)
	}
}

// setCredentialUnavailable records a CredentialUnavailable=True condition on the
// RunnerGroup when the AGC cannot obtain a GitHub App installation token, then
// returns the original error so the reconcile still requeues with backoff. It is
// called on the token-fetch error path, which returns before the normal status
// update — without it the failure would surface only as a Kubernetes Event and
// not in status (Q156).
//
// The cause is the token-fetch error (which describes *why* the fetch failed, not
// any token material — no credential is logged or stored). On a status write
// conflict the original error is still returned so the reconcile retries.
func (r *RunnerGroupReconciler) setCredentialUnavailable(ctx context.Context, rg *v1alpha1.RunnerGroup, cause error) (ctrl.Result, error) {
	r.mergeCondition(rg, metav1.Condition{
		Type:               v1alpha1.ConditionCredentialUnavailable,
		Status:             metav1.ConditionTrue,
		Reason:             v1alpha1.ReasonTokenUnavailable,
		Message:            fmt.Sprintf("failed to obtain GitHub App installation token: %v", cause),
		ObservedGeneration: rg.Generation,
	})
	if err := r.Status().Update(ctx, rg); err != nil && !apierrors.IsConflict(err) {
		r.Log.Error("failed to write CredentialUnavailable condition", "error", err)
	}
	return ctrl.Result{}, cause
}

// SetConditionForTest enqueues a condition update as if it came from a listener
// goroutine, exercising the same push path (including the Q333 reconciler wake) so
// integration tests can prove a pushed condition wakes an idle reconciler. Intended
// for use in tests only.
func (r *RunnerGroupReconciler) SetConditionForTest(ns, name string, cond metav1.Condition) {
	if r.conditionCh == nil {
		return
	}
	r.condUpdater().SetCondition(ns, name, cond)
}

// ReconcileCountForTest returns the number of times Reconcile has been invoked.
// Intended for use in integration tests only — it lets a test detect when the
// controller has quiesced (count stops increasing) and then assert that an
// external event, such as a worker Pod lifecycle event delivered through the
// Pod watch, triggered a fresh reconcile.
func (r *RunnerGroupReconciler) ReconcileCountForTest() int64 {
	return r.reconcileCount.Load()
}

// LocalStateCountForTest returns the number of RunnerGroups for which the
// reconciler currently holds an in-memory multiplexer and the number for which
// it holds an agent pool. Intended for use in unit tests only — it lets tests
// assert that cleanupLocalState dropped the per-RunnerGroup state.
func (r *RunnerGroupReconciler) LocalStateCountForTest() (multiplexers, pools int) {
	r.multiplexersMu.Lock()
	multiplexers = len(r.multiplexers)
	r.multiplexersMu.Unlock()
	r.poolsMu.Lock()
	pools = len(r.pools)
	r.poolsMu.Unlock()
	return multiplexers, pools
}
