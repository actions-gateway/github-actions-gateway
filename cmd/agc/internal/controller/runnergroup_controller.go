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
	"github.com/actions-gateway/github-actions-gateway/agc/internal/token"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/tracing"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
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

// channelConditionUpdater implements listener.ConditionUpdater.
type channelConditionUpdater struct {
	ch chan<- conditionUpdate
}

func (u *channelConditionUpdater) SetCondition(namespace, name string, cond metav1.Condition) {
	select {
	case u.ch <- conditionUpdate{namespace: namespace, name: name, condition: cond}:
	default:
		// Drop if channel is full to avoid blocking listener goroutines.
	}
}

// RunnerGroupReconciler reconciles RunnerGroup objects.
type RunnerGroupReconciler struct {
	client.Client
	TokenManager *token.Manager
	Registrar    agentpool.Registrar
	BrokerConfig BrokerConfig
	Metrics      *listener.Metrics
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

	// in-process state; rebuilt from Secrets on restart.
	multiplexersMu sync.Mutex
	multiplexers   map[types.NamespacedName]*listener.Multiplexer
	poolsMu        sync.Mutex
	pools          map[types.NamespacedName]*agentpool.Pool

	conditionCh chan conditionUpdate

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
}

// SetupWithManager registers the reconciler with the controller-runtime manager.
func (r *RunnerGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
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

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.RunnerGroup{}).
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

// workerPodPredicate restricts the Pod watch to this project's worker pods and
// to the events that carry new information for the RunnerGroup's status:
//
//   - Create — a job was acquired and a worker pod started.
//   - Delete — a worker pod was removed (reaper, goroutine cleanup, ownerRef
//     GC, or manual kubectl delete).
//   - Update — only when the pod's phase changed (e.g. Running → Failed on
//     eviction, Running → Succeeded on completion). A terminal pod is retained
//     for the group's completedPodTTL before the reaper deletes it, so
//     eviction surfaces as a phase update first, then a delete; skipping
//     non-phase updates avoids reconcile churn from status heartbeats that do
//     not change the group's observable state.
//
// Generic events are ignored.
func workerPodPredicate() predicate.Predicate {
	hasLabel := func(obj client.Object) bool {
		_, ok := obj.GetLabels()[provisioner.LabelRunnerGroup]
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

// Reconcile is called by controller-runtime on RunnerGroup events.
func (r *RunnerGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	// Root span for the reconcile. Each RunnerGroup event is its own trace (there
	// is no inbound trace context); the provisioner's per-job spans form separate
	// traces driven off the listener goroutines, not children of this one. The
	// deferred closure stamps the span's error status from the named return.
	ctx, span := tracer.Start(ctx, "RunnerGroup.Reconcile", trace.WithAttributes(
		attribute.String("runnergroup.namespace", req.Namespace),
		attribute.String("runnergroup.name", req.Name),
	))
	defer func() {
		if retErr != nil {
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

	r.reconcileCount.Add(1)
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

	// 2. Drain pending condition updates from listener goroutines.
	r.drainConditions(ctx, &rg)

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
	// pod becomes due; it is propagated as RequeueAfter below.
	reapAfter, err := r.reapWorkerPods(ctx, log, &rg)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 5. Get installation token for agent management.
	instToken, err := r.TokenManager.Token(ctx)
	if err != nil {
		log.Error("failed to get installation token", "error", err)
		r.recordEvent(&rg, corev1.EventTypeWarning, "TokenUnavailable", "GetToken",
			"failed to obtain GitHub App installation token: %v", err)
		return ctrl.Result{}, err
	}

	// 6. Ensure agent pool Secrets.
	pool := r.getOrCreatePool(req.NamespacedName, rg.Namespace, rg.Name, rg.Spec.RunnerLabels)
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
	if mux.ActiveCount() == 0 && rg.Spec.MaxListeners > 0 {
		if startErr := mux.Start(ctx); startErr != nil {
			log.Warn("multiplexer restart failed", "error", startErr)
			r.recordEvent(&rg, corev1.EventTypeWarning, "ListenerStartFailed", "StartMultiplexer",
				"failed to restart listener goroutines: %v", startErr)
		}
	}

	// 8. Update status.
	rg.Status.ActiveSessions = mux.ActiveCount()
	rg.Status.ObservedGeneration = rg.Generation
	r.setReadyCondition(&rg, mux.ActiveCount() > 0)

	if err := r.Status().Update(ctx, &rg); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: reapAfter}, nil
}

// reconcileDelete handles RunnerGroup deletion: stop goroutines, delete Secrets, remove finalizer.
func (r *RunnerGroupReconciler) reconcileDelete(ctx context.Context, log *slog.Logger, rg *v1alpha1.RunnerGroup) (ctrl.Result, error) {
	key := types.NamespacedName{Namespace: rg.Namespace, Name: rg.Name}

	// Stop the multiplexer first so no new agents are claimed while we deregister.
	r.multiplexersMu.Lock()
	if mux, ok := r.multiplexers[key]; ok {
		mux.Stop()
		delete(r.multiplexers, key)
	}
	r.multiplexersMu.Unlock()

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

// cleanupLocalState stops and removes any in-memory multiplexer and agent pool
// for the given RunnerGroup. It never touches the API server, so it is safe on
// both the deletion path and a NotFound reconcile, and it is idempotent —
// calling it more than once for the same key is a no-op.
func (r *RunnerGroupReconciler) cleanupLocalState(key types.NamespacedName) {
	r.multiplexersMu.Lock()
	if mux, ok := r.multiplexers[key]; ok {
		mux.Stop()
		delete(r.multiplexers, key)
	}
	r.multiplexersMu.Unlock()

	r.poolsMu.Lock()
	delete(r.pools, key)
	r.poolsMu.Unlock()
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
func (r *RunnerGroupReconciler) getOrCreatePool(key types.NamespacedName, namespace, groupName string, runnerLabels []string) *agentpool.Pool {
	r.poolsMu.Lock()
	defer r.poolsMu.Unlock()
	if p, ok := r.pools[key]; ok {
		return p
	}
	p := agentpool.NewPool(r.Client, namespace, groupName, r.BrokerConfig.RunnerVersion, runnerLabels, r.Registrar, r.AgentKeyType)
	if r.Metrics != nil {
		p.Metrics = r.Metrics
	}
	r.pools[key] = p
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

	condCh := r.conditionCh
	condUpdater := &channelConditionUpdater{ch: condCh}
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
		// Use the per-agent broker URL from the JIT config (.runner serverUrl).
		// Fall back to the static BrokerConfig URL for non-JIT registrars (stubs).
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
		jobHandler := listener.JobHandlerFunc(nil)
		if r.Provisioner != nil {
			jobHandler = r.Provisioner.HandlerFor(rg)
		}
		return listener.Config{
			Group:         rg.Name,
			Namespace:     rg.Namespace,
			Agent:         agent,
			Broker:        bc,
			Conditions:    condUpdater,
			Metrics:       r.Metrics,
			RunnerOS:      brokerCfg.RunnerOS,
			JobHandler:    jobHandler,
			IdleThreshold: brokerCfg.IdleThreshold,
			RenewInterval: brokerCfg.RenewJobInterval,
			// Return the claimed agent to the pool on goroutine exit so the pool is
			// not exhausted after maxListeners total spawns (which would block the
			// permanent baseline from restarting).
			ReleaseAgent: func() { pool.ReleaseAgent(agent) },
			// Single-use JIT agent lifecycle (Q114): mark the agent's runner
			// record spent at job acquisition, and re-register it under the same
			// name when the goroutine self-heals. Both key on the stable agent
			// index, so the captured pointer staying behind a later recycle is
			// fine.
			MarkAgentConsumed: func() { pool.MarkConsumed(agent) },
			RecycleAgent: func(ctx context.Context) (*agentpool.Agent, error) {
				tok, err := r.TokenManager.Token(ctx)
				if err != nil {
					return nil, fmt.Errorf("installation token for agent recycle: %w", err)
				}
				return pool.Recycle(ctx, agent, tok)
			},
		}
	}

	mux := listener.NewMultiplexer(factory, rg.Spec.MaxListeners, r.Log)
	if err := mux.Start(ctx); err != nil {
		r.Log.Error("failed to start multiplexer", "error", err)
	}
	r.multiplexers[key] = mux
	return mux
}

// drainConditions reads pending condition updates and merges them into rg.Status.
// Updates for other RunnerGroups are collected and re-enqueued after the loop
// to avoid re-processing them in the current iteration.
func (r *RunnerGroupReconciler) drainConditions(_ context.Context, rg *v1alpha1.RunnerGroup) {
	var skipped []conditionUpdate
	for {
		select {
		case upd := <-r.conditionCh:
			if upd.namespace == rg.Namespace && upd.name == rg.Name {
				r.mergeCondition(rg, upd.condition)
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

// mergeCondition upserts a condition into rg.Status.Conditions keyed by Type.
// It delegates to meta.SetStatusCondition so LastTransitionTime advances only on
// an actual status transition rather than being rewritten on every reconcile.
func (r *RunnerGroupReconciler) mergeCondition(rg *v1alpha1.RunnerGroup, cond metav1.Condition) {
	meta.SetStatusCondition(&rg.Status.Conditions, cond)
}

func (r *RunnerGroupReconciler) setReadyCondition(rg *v1alpha1.RunnerGroup, ready bool) {
	status := metav1.ConditionFalse
	reason := "NoActiveSessions"
	msg := "No listener goroutines are running."
	if ready {
		status = metav1.ConditionTrue
		reason = "ListenerActive"
		msg = "At least one listener goroutine is running."
	}
	prev := meta.FindStatusCondition(rg.Status.Conditions, "Ready")
	r.mergeCondition(rg, metav1.Condition{
		Type:               "Ready",
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

// SetConditionForTest enqueues a condition update as if it came from a listener
// goroutine. Intended for use in unit tests only.
func (r *RunnerGroupReconciler) SetConditionForTest(ns, name string, cond metav1.Condition) {
	if r.conditionCh == nil {
		return
	}
	select {
	case r.conditionCh <- conditionUpdate{namespace: ns, name: name, condition: cond}:
	default:
	}
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
