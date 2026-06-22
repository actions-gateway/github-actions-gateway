package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/token"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// runnerSetFinalizer is set on a RunnerSet so its in-process listener/agent state
// and agent Secrets are cleaned up before the CR is removed. On the v2
// actions-gateway.com domain, distinct from the v1 RunnerGroup finalizer so the
// two controllers never contend.
const runnerSetFinalizer = "actions-gateway.com/agentpool-cleanup"

// RunnerSetReconciler reconciles v2alpha1 RunnerSet objects in the AGC. It is the
// v2 counterpart of RunnerGroupReconciler: it resolves the RunnerSet's references
// (gatewayRef → ActionsGateway, templateRef → RunnerTemplate/ClusterRunnerTemplate,
// proxyRef → EgressProxy) at runtime via watch + enqueue, fails closed with a
// NotFound condition until they resolve (§H.7), and once resolved drives the same
// adaptive listener-goroutine pool the RunnerGroup controller does — provisioning
// ephemeral worker pods per acquired job, owner-referenced to the real RunnerSet
// through the provisioner Target seam.
//
// It keeps its own in-memory multiplexer/pool maps and condition channel, separate
// from the RunnerGroupReconciler's, so the v1 and v2 paths never share runtime
// state. v1 runtime semantics (job acquisition, ceilings, reaper, eviction/quota
// tunables) are preserved exactly — only the owner object and the source of the
// pod shape and proxy differ.
type RunnerSetReconciler struct {
	client.Client
	TokenManager *token.Manager
	Registrar    agentpool.Registrar
	BrokerConfig BrokerConfig
	Metrics      *listener.Metrics
	Log          *slog.Logger
	Provisioner  *provisioner.Provisioner
	AgentKeyType agentpool.KeyType

	// GatewayName scopes this AGC to a single ActionsGateway under multi-gateway
	// (§H.16 #1): it reconciles only the RunnerSets whose spec.gatewayRef.name
	// equals it. Set from the GATEWAY_NAME env the GMC stamps on the AGC Deployment.
	// The RunnerSet informer is field-selector-scoped to this value server-side
	// (cmd/agc/main.go), so a foreign set is normally never delivered; the guard in
	// Reconcile is defense-in-depth. Empty disables scoping (a single shared AGC
	// reconciles every RunnerSet — the pre-M3b behavior, still used by tests).
	GatewayName string

	// Recorder emits Kubernetes Events on the reconciled RunnerSet. May be nil in
	// unit tests; callers must nil-check before use.
	Recorder events.EventRecorder

	// Now is the clock used by the worker-pod reaper. Nil means time.Now.
	Now func() time.Time

	// BaselineRecheckInterval is the cadence at which a RunnerSet is requeued while
	// its multiplexer is below the desired listener count. Zero selects
	// defaultBaselineRecheckInterval.
	BaselineRecheckInterval time.Duration

	multiplexersMu sync.Mutex
	multiplexers   map[types.NamespacedName]*listener.Multiplexer
	poolsMu        sync.Mutex
	pools          map[types.NamespacedName]*agentpool.Pool

	conditionCh chan conditionUpdate

	reconcileCount atomic.Int64
}

// SetupWithManager registers the reconciler and the referent → RunnerSet watches
// that make reference resolution event-driven: when a referenced ActionsGateway,
// EgressProxy, RunnerTemplate, or ClusterRunnerTemplate is created (or changes),
// every RunnerSet that names it is re-reconciled so a NotFound condition flips to
// Ready the moment the referent syncs (§H.7). It also watches worker pods (for
// status/reaper, like the RunnerGroup controller).
func (r *RunnerSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Log == nil {
		r.Log = slog.Default()
	}
	r.ensureMaps()

	return ctrl.NewControllerManagedBy(mgr).
		For(&v2alpha1.RunnerSet{}).
		// Worker pods carry LabelRunnerSet; re-reconcile on their lifecycle events
		// so status.activeSessions and the reaper track pod phase transitions.
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.podToRunnerSet),
			builder.WithPredicates(runnerSetWorkerPodPredicate()),
		).
		// Referent watches: a RunnerSet sitting Ready=False/<Ref>NotFound flips the
		// moment its missing referent appears.
		Watches(
			&v2alpha1.ActionsGateway{},
			handler.EnqueueRequestsFromMapFunc(r.gatewayToRunnerSets),
		).
		Watches(
			&v2alpha1.EgressProxy{},
			handler.EnqueueRequestsFromMapFunc(r.proxyToRunnerSets),
		).
		Watches(
			&v2alpha1.RunnerTemplate{},
			handler.EnqueueRequestsFromMapFunc(r.templateToRunnerSets),
		).
		// ClusterRunnerTemplate (cluster-scoped): the AGC now holds a ClusterRoleBinding
		// to the shipped agc-clusterrunnertemplate-reader ClusterRole (created per
		// gateway by the GMC), so the informer establishes and a RunnerSet referencing
		// a ClusterRunnerTemplate flips Ready the moment it syncs (§H.7). The
		// namespace-scoped manager cache serves cluster-scoped kinds from a cluster-wide
		// informer.
		Watches(
			&v2alpha1.ClusterRunnerTemplate{},
			handler.EnqueueRequestsFromMapFunc(r.clusterTemplateToRunnerSets),
		).
		Named("runnerset").
		Complete(r)
}

func (r *RunnerSetReconciler) ensureMaps() {
	if r.multiplexers == nil {
		r.multiplexers = make(map[types.NamespacedName]*listener.Multiplexer)
	}
	if r.pools == nil {
		r.pools = make(map[types.NamespacedName]*agentpool.Pool)
	}
	if r.conditionCh == nil {
		r.conditionCh = make(chan conditionUpdate, 256)
	}
}

// Reconcile drives a RunnerSet: resolve references, and once they resolve, ensure
// the listener pool is running and worker pods are reaped.
func (r *RunnerSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.reconcileCount.Add(1)
	if r.Log == nil {
		r.Log = slog.Default()
	}
	r.ensureMaps()
	log := r.Log.With("namespace", req.Namespace, "name", req.Name)

	var rs v2alpha1.RunnerSet
	if err := r.Get(ctx, req.NamespacedName, &rs); err != nil {
		if apierrors.IsNotFound(err) {
			r.cleanupLocalState(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Scoping guard (§H.16 #1): never act on a RunnerSet that targets another
	// gateway. The informer is already field-scoped to GatewayName server-side, so
	// this only fires on a stale event (e.g. a gatewayRef edit racing the watch
	// filter); acting anyway would add this AGC's finalizer to, or drive status on,
	// a sibling gateway's set — the isolation boundary this milestone establishes.
	if r.GatewayName != "" && rs.Spec.GatewayRef.Name != r.GatewayName {
		r.cleanupLocalState(req.NamespacedName)
		return ctrl.Result{}, nil
	}

	r.drainConditions(&rs)

	if !rs.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, log, &rs)
	}

	if !controllerutil.ContainsFinalizer(&rs, runnerSetFinalizer) {
		controllerutil.AddFinalizer(&rs, runnerSetFinalizer)
		if err := r.Update(ctx, &rs); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 1. Resolve references. Fail closed: until every reference resolves, the
	// RunnerSet sits Ready=False/<Ref>NotFound with no listeners running, so no
	// worker pod is ever provisioned in the gap (§H.7). The referent watches
	// re-enqueue when a missing object appears.
	_, res := resolveRunnerSetRefs(ctx, r.Client, &rs)
	if res.err != nil {
		return ctrl.Result{}, res.err
	}
	if !res.resolved() {
		// Stop any running listeners — a reference that vanished must not keep
		// acquiring jobs (the per-job Resolve would fail them closed anyway, but
		// stopping avoids the churn and reflects reality).
		r.stopMultiplexer(req.NamespacedName)
		r.setReadyCondition(&rs, false, res.reason, res.message)
		rs.Status.ActiveSessions = 0
		rs.Status.ObservedGeneration = rs.Generation
		if err := r.Status().Update(ctx, &rs); err != nil && !apierrors.IsConflict(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// 2. Reap expired worker pods (terminal past completedPodTTL, Pending past
	// pendingPodDeadline). Runs before the token fetch so cleanup keeps working
	// during a GitHub outage.
	reapAfter, err := r.reapWorkerPods(ctx, log, &rs)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 3. Installation token for agent management. Process-wide (one GitHub App per
	// AGC); a failure affects every RunnerSet, so surface it and requeue.
	instToken, err := r.TokenManager.Token(ctx)
	if err != nil {
		log.Error("failed to get installation token", "error", err)
		r.recordEvent(&rs, corev1.EventTypeWarning, "TokenUnavailable", "GetToken",
			"failed to obtain GitHub App installation token: %v", err)
		r.setReadyCondition(&rs, false, v2alpha1.ReasonTokenUnavailable,
			fmt.Sprintf("failed to obtain GitHub App installation token: %v", err))
		if uerr := r.Status().Update(ctx, &rs); uerr != nil && !apierrors.IsConflict(uerr) {
			log.Error("failed to write Ready condition", "error", uerr)
		}
		return ctrl.Result{}, err
	}

	// 4. Ensure agent pool Secrets.
	pool := r.getOrCreatePool(req.NamespacedName, rs.Namespace, rs.Name, rs.Spec.RunnerLabels)
	if err := pool.EnsureAgents(ctx, rs.Spec.MaxListeners, instToken); err != nil {
		log.Error("EnsureAgents failed", "error", err)
		r.recordEvent(&rs, corev1.EventTypeWarning, "AgentPoolError", "EnsureAgents",
			"failed to provision agent Secrets: %v", err)
		return ctrl.Result{}, err
	}

	// 5. Start or update the multiplexer.
	mux := r.getOrCreateMultiplexer(ctx, req.NamespacedName, rs.DeepCopy(), pool)
	mux.SetMaxListeners(rs.Spec.MaxListeners)
	if mux.ActiveCount() == 0 && rs.Spec.MaxListeners > 0 {
		if startErr := mux.Start(ctx); startErr != nil {
			log.Warn("multiplexer restart failed", "error", startErr)
			r.recordEvent(&rs, corev1.EventTypeWarning, "ListenerStartFailed", "StartMultiplexer",
				"failed to restart listener goroutines: %v", startErr)
		}
	}

	// 6. Update status.
	active := mux.ActiveCount()
	rs.Status.ActiveSessions = active
	rs.Status.ObservedGeneration = rs.Generation
	if active > 0 {
		r.setReadyCondition(&rs, true, v2alpha1.ReasonListenerActive,
			fmt.Sprintf("references resolved; %d listener goroutine(s) running", active))
	} else {
		r.setReadyCondition(&rs, false, v2alpha1.ReasonNoActiveSessions,
			"references resolved; no listener goroutines are running")
	}
	if err := r.Status().Update(ctx, &rs); err != nil {
		return ctrl.Result{}, err
	}

	requeueAfter := reapAfter
	if rs.Spec.MaxListeners > 0 && mux.ActiveCount() < rs.Spec.MaxListeners {
		if interval := r.baselineRecheckInterval(); requeueAfter <= 0 || interval < requeueAfter {
			requeueAfter = interval
		}
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *RunnerSetReconciler) baselineRecheckInterval() time.Duration {
	if r.BaselineRecheckInterval > 0 {
		return r.BaselineRecheckInterval
	}
	return defaultBaselineRecheckInterval
}

// reconcileDelete stops goroutines, deletes agent Secrets, and removes the finalizer.
func (r *RunnerSetReconciler) reconcileDelete(ctx context.Context, log *slog.Logger, rs *v2alpha1.RunnerSet) (ctrl.Result, error) {
	key := types.NamespacedName{Namespace: rs.Namespace, Name: rs.Name}

	r.stopMultiplexer(key)

	pool := r.getPool(key)
	if pool != nil {
		instToken, err := r.TokenManager.Token(ctx)
		if err != nil {
			log.Warn("could not get token for pool cleanup; proceeding without deregistration", "error", err)
			instToken = ""
		}
		if err := pool.DeleteAll(ctx, instToken); err != nil {
			r.recordEvent(rs, corev1.EventTypeWarning, "AgentDeregistrationFailed", "Delete",
				"failed to deregister/delete agent Secrets: %v", err)
			return ctrl.Result{}, fmt.Errorf("pool.DeleteAll: %w", err)
		}
	}

	r.cleanupLocalState(key)

	controllerutil.RemoveFinalizer(rs, runnerSetFinalizer)
	if err := r.Update(ctx, rs); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// stopMultiplexer stops and drops the multiplexer for key, if present. Idempotent.
func (r *RunnerSetReconciler) stopMultiplexer(key types.NamespacedName) {
	r.multiplexersMu.Lock()
	if mux, ok := r.multiplexers[key]; ok {
		mux.Stop()
		delete(r.multiplexers, key)
	}
	r.multiplexersMu.Unlock()
}

// cleanupLocalState stops the multiplexer and drops the agent pool for key. Never
// touches the API server, so it is safe on both the deletion and NotFound paths.
func (r *RunnerSetReconciler) cleanupLocalState(key types.NamespacedName) {
	r.stopMultiplexer(key)
	r.poolsMu.Lock()
	delete(r.pools, key)
	r.poolsMu.Unlock()
}

func (r *RunnerSetReconciler) recordEvent(rs *v2alpha1.RunnerSet, eventtype, reason, action, note string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(rs, nil, eventtype, reason, action, note, args...)
}

func (r *RunnerSetReconciler) getOrCreatePool(key types.NamespacedName, namespace, name string, runnerLabels []string) *agentpool.Pool {
	r.poolsMu.Lock()
	defer r.poolsMu.Unlock()
	if p, ok := r.pools[key]; ok {
		return p
	}
	p := agentpool.NewPool(r.Client, namespace, name, r.BrokerConfig.RunnerVersion, runnerLabels, r.Registrar, r.AgentKeyType)
	if r.Metrics != nil {
		p.Metrics = r.Metrics
	}
	r.pools[key] = p
	return p
}

func (r *RunnerSetReconciler) getPool(key types.NamespacedName) *agentpool.Pool {
	r.poolsMu.Lock()
	defer r.poolsMu.Unlock()
	return r.pools[key]
}

// getOrCreateMultiplexer returns the multiplexer for the RunnerSet, creating and
// starting it if needed. The factory binds each listener goroutine to a
// runnerSetTarget so the provisioner own-refs the real RunnerSet and re-resolves
// its template/proxy per job.
func (r *RunnerSetReconciler) getOrCreateMultiplexer(ctx context.Context, key types.NamespacedName, rs *v2alpha1.RunnerSet, pool *agentpool.Pool) *listener.Multiplexer {
	r.multiplexersMu.Lock()
	defer r.multiplexersMu.Unlock()

	if mux, ok := r.multiplexers[key]; ok {
		return mux
	}

	condUpdater := &channelConditionUpdater{ch: r.conditionCh}
	brokerCfg := r.BrokerConfig
	target := &runnerSetTarget{
		client: r.Client,
		prov:   r.Provisioner,
		key:    key,
		uid:    rs.UID,
	}

	factory := func(index int) listener.Config {
		agent := pool.ClaimAgent()
		if agent == nil {
			return listener.Config{Group: rs.Name, Namespace: rs.Namespace}
		}
		return r.newListenerConfig(rs, target, pool, brokerCfg, condUpdater, agent)
	}

	muxLog := r.Log.With("namespace", rs.Namespace, "group", rs.Name)
	mux := listener.NewMultiplexer(factory, rs.Spec.MaxListeners, muxLog)
	if err := mux.Start(ctx); err != nil {
		r.Log.Error("failed to start multiplexer", "error", err)
	}
	r.multiplexers[key] = mux
	return mux
}

// newListenerConfig assembles the listener.Config for a single goroutine. It is
// the v2 counterpart of RunnerGroupReconciler.newListenerConfig, wiring the
// provisioner's owner-agnostic Handle/Admit against the RunnerSet Target and
// delegating the rest to the shared assembleListenerConfig.
func (r *RunnerSetReconciler) newListenerConfig(rs *v2alpha1.RunnerSet, target provisioner.Target, pool *agentpool.Pool, brokerCfg BrokerConfig, condUpdater listener.ConditionUpdater, agent *agentpool.Agent) listener.Config {
	jobHandler := listener.JobHandlerFunc(nil)
	admit := listener.AdmitFunc(nil)
	if r.Provisioner != nil {
		jobHandler = r.Provisioner.Handle(target)
		admit = r.Provisioner.Admit(target)
	}
	return assembleListenerConfig(rs.Name, rs.Namespace, brokerCfg, condUpdater, r.Metrics, agent, r.TokenManager, jobHandler, admit, pool)
}

// drainConditions reads pending listener-pushed condition updates and merges those
// for this RunnerSet into its status; others are re-enqueued.
func (r *RunnerSetReconciler) drainConditions(rs *v2alpha1.RunnerSet) {
	var skipped []conditionUpdate
	for {
		select {
		case upd := <-r.conditionCh:
			if upd.namespace == rs.Namespace && upd.name == rs.Name {
				meta.SetStatusCondition(&rs.Status.Conditions, upd.condition)
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
		}
	}
}

// setReadyCondition upserts the Ready condition and emits an Event on a genuine
// transition.
func (r *RunnerSetReconciler) setReadyCondition(rs *v2alpha1.RunnerSet, ready bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	prev := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionReady)
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type:               v2alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: rs.Generation,
	})
	if prev == nil || prev.Status != status {
		etype := corev1.EventTypeNormal
		if !ready {
			etype = corev1.EventTypeWarning
		}
		r.recordEvent(rs, etype, reason, "Reconcile", msg)
	}
}

// nowFunc returns the reaper clock: Now when set, time.Now otherwise.
func (r *RunnerSetReconciler) nowFunc() func() time.Time {
	if r.Now != nil {
		return r.Now
	}
	return time.Now
}

// reapWorkerPods deletes worker pods this RunnerSet no longer needs (terminal past
// completedPodTTL, Pending past pendingPodDeadline), mirroring the RunnerGroup
// reaper but filtering on LabelRunnerSet and reading the RunnerSet's tunables. It
// returns the time until the earliest retained pod becomes due (0 = none).
func (r *RunnerSetReconciler) reapWorkerPods(ctx context.Context, log *slog.Logger, rs *v2alpha1.RunnerSet) (time.Duration, error) {
	return reapWorkerPodsByLabel(ctx, r.Client, r.nowFunc()(), rs.Namespace, rs.Name,
		provisioner.LabelRunnerSet,
		provisioner.CompletedPodTTLOrDefault(rs.Spec.CompletedPodTTL),
		provisioner.PendingPodDeadlineOrDefault(rs.Spec.PendingPodDeadline),
		log, r.Metrics,
		func(podName string, deadline time.Duration) {
			r.recordEvent(rs, corev1.EventTypeWarning, "WorkerPodStuckPending", "ReapWorkerPods",
				"worker pod %s was Pending for more than %s and has been deleted; "+
					"check the template image and scheduling constraints", podName, deadline)
		})
}

// ReconcileCountForTest returns how many times Reconcile has run (integration tests).
func (r *RunnerSetReconciler) ReconcileCountForTest() int64 {
	return r.reconcileCount.Load()
}

// --- watch enqueue mappers ---

// podToRunnerSet maps a worker Pod event to its owning RunnerSet via LabelRunnerSet.
func (r *RunnerSetReconciler) podToRunnerSet(_ context.Context, obj client.Object) []ctrl.Request {
	name := obj.GetLabels()[provisioner.LabelRunnerSet]
	if name == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}

// gatewayToRunnerSets enqueues every RunnerSet in the gateway's namespace whose
// gatewayRef names it.
func (r *RunnerSetReconciler) gatewayToRunnerSets(ctx context.Context, obj client.Object) []ctrl.Request {
	return r.runnerSetsMatching(ctx, obj.GetNamespace(), func(rs *v2alpha1.RunnerSet) bool {
		return rs.Spec.GatewayRef.Name == obj.GetName()
	})
}

// proxyToRunnerSets enqueues every RunnerSet that resolves to this EgressProxy —
// either directly via proxyRef, or via the gateway's defaultProxyRef. The latter
// requires reading each set's gateway, so for simplicity (single-gateway parity)
// any unset-proxyRef set in the namespace is enqueued; the reconcile re-resolves.
func (r *RunnerSetReconciler) proxyToRunnerSets(ctx context.Context, obj client.Object) []ctrl.Request {
	return r.runnerSetsMatching(ctx, obj.GetNamespace(), func(rs *v2alpha1.RunnerSet) bool {
		if rs.Spec.ProxyRef != nil {
			return rs.Spec.ProxyRef.Name == obj.GetName()
		}
		// Unset proxyRef inherits the gateway's defaultProxyRef; re-reconcile to
		// re-resolve rather than reading the gateway here.
		return true
	})
}

// templateToRunnerSets enqueues every RunnerSet in the template's namespace whose
// templateRef names it (and is not a ClusterRunnerTemplate ref).
func (r *RunnerSetReconciler) templateToRunnerSets(ctx context.Context, obj client.Object) []ctrl.Request {
	return r.runnerSetsMatching(ctx, obj.GetNamespace(), func(rs *v2alpha1.RunnerSet) bool {
		return rs.Spec.TemplateRef.Kind != "ClusterRunnerTemplate" && rs.Spec.TemplateRef.Name == obj.GetName()
	})
}

// clusterTemplateToRunnerSets enqueues every RunnerSet whose templateRef is a
// ClusterRunnerTemplate naming this object. The referent is cluster-scoped (no
// namespace), so it lists from the manager cache — already scoped to this AGC's
// namespace and gateway — rather than filtering by the object's (empty) namespace.
func (r *RunnerSetReconciler) clusterTemplateToRunnerSets(ctx context.Context, obj client.Object) []ctrl.Request {
	var list v2alpha1.RunnerSetList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range list.Items {
		rs := &list.Items[i]
		if rs.Spec.TemplateRef.Kind == "ClusterRunnerTemplate" && rs.Spec.TemplateRef.Name == obj.GetName() {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rs.Namespace, Name: rs.Name}})
		}
	}
	return reqs
}

// runnerSetsMatching lists RunnerSets in ns and returns reconcile requests for
// those satisfying match.
func (r *RunnerSetReconciler) runnerSetsMatching(ctx context.Context, ns string, match func(*v2alpha1.RunnerSet) bool) []ctrl.Request {
	var list v2alpha1.RunnerSetList
	if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range list.Items {
		rs := &list.Items[i]
		if match(rs) {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rs.Namespace, Name: rs.Name}})
		}
	}
	return reqs
}

// runnerSetWorkerPodPredicate restricts the Pod watch to v2 worker pods and to
// events that carry new status information (the v2 binding of the shared
// workerPodPhaseChangePredicate, keyed on LabelRunnerSet).
func runnerSetWorkerPodPredicate() predicate.Predicate {
	return workerPodPhaseChangePredicate(provisioner.LabelRunnerSet)
}
