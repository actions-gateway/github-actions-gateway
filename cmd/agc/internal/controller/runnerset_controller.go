package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/token"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// runnerSetFinalizer is set on a RunnerSet so its in-process listener state is
// cleaned up before the CR is removed. The name predates the scale-set tier and is
// kept because every existing set already carries it.
const runnerSetFinalizer = "actions-gateway.com/agentpool-cleanup"

// RunnerSetReconciler reconciles v2 RunnerSet objects in the AGC. It is the
// v2 counterpart of RunnerGroupReconciler: it resolves the RunnerSet's references
// (gatewayRef → ActionsGateway, templateRef → RunnerTemplate/ClusterRunnerTemplate,
// proxyRef → EgressProxy) at runtime via watch + enqueue, fails closed with a
// NotFound condition until they resolve (§H.7), and once resolved drives one
// scale-set listener session per set — provisioning ephemeral worker pods per
// acquired job, owner-referenced to the real RunnerSet through the provisioner
// Target seam.
type RunnerSetReconciler struct {
	client.Client
	// APIReader is the manager's uncached reader, for the reads an informer cache must
	// not serve. Nil falls back to the cached client (tests).
	APIReader    client.Reader
	TokenManager *token.Manager
	Metrics      *runnercore.Metrics
	// ScaleSetMetrics holds the Prometheus counters for the scale-set listener. Nil
	// disables them.
	ScaleSetMetrics *scalesetlistener.Metrics
	Log             *slog.Logger
	Provisioner     *provisioner.Provisioner
	// ImageResolver reads the runner version out of the worker image in its registry
	// (Q988). Nil judges the image by its tag alone.
	ImageResolver RunnerImageResolver

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

	// EventReader reads Events straight from the apiserver, bypassing the controller
	// cache (production wires mgr.GetAPIReader()). It serves the capacity gate on a
	// cluster whose gateway reports nodeAutoscaling: Present (Q406, Q470), where the
	// gate reads a stuck worker pod's Events to learn whether the cluster autoscaler
	// declined to add a node for it.
	//
	// Uncached deliberately: Events are the highest-churn object in a busy cluster, so
	// an informer would impose an unbounded steady-state cost on every AGC to serve an
	// opt-in gate most sets never enable. The reads are instead field-selected to one
	// pod and only happen for pods already stuck past the scheduling grace.
	//
	// Nil (unit tests, or an AGC wired without it) makes that path fail open — the gate
	// never closes and intake is exactly today's behavior.
	EventReader client.Reader

	// Now is the clock used by the worker-pod reaper. Nil means time.Now.
	Now func() time.Time

	// Sizing supplies the measured worker sizing recommendation surfaced in
	// status.sizingRecommendation and judged by the SizingDrift condition (Q359
	// Phase 2). Production wires the usage.Sampler; nil (e.g. sampling disabled,
	// unit tests) leaves any persisted recommendation untouched.
	Sizing SizingSource

	// ProxyShareRecheckInterval is the cadence at which a RunnerSet whose egress
	// wiring turns on a projected proxy share is requeued, in either direction of the
	// grant. Zero selects defaultProxyShareRecheckInterval.
	ProxyShareRecheckInterval time.Duration

	// stopped is set by stopListeners when the manager's shutdown drain begins, and
	// is never cleared: a reconciler that has drained does not come back. Every
	// listener start path reads it, because a session opened after the drain is one
	// nothing will delete (Q968).
	stopped atomic.Bool

	// ownerRefsRestamped records, per RunnerSet, the UID restampConfigMapOwners last
	// finished for, so the read happens once per process.
	ownerRefsRestamped sync.Map

	// scaleSetListeners holds the running scale-set acquisition listener per
	// RunnerSet — one session per scale set.
	scaleSetListenersMu sync.Mutex
	scaleSetListeners   map[types.NamespacedName]*scaleSetListenerHandle

	// ScaleSetClientFactory builds the scale-set protocol client for a RunnerSet. Nil selects the production factory (buildScaleSetClient), which derives
	// the config URL/API base from the resolved gateway's githubURL and routes through
	// the per-tenant egress proxy. Tests inject a factory pointing at the scalesettest
	// fake so the wiring is exercised offline.
	ScaleSetClientFactory func(rs *v2.RunnerSet, gw *v2.ActionsGateway) (*scaleset.Client, error)

	// ScaleSetStubBaseURL re-points the scale-set bootstrap at a fake-GitHub stub,
	// for the deployed fake-GitHub e2e tier — the gateway's githubURL cannot name it,
	// being pinned to https by the CRD and the webhook. Empty in production: main()
	// sets it only from the STUB_AUTH_URL + STUB_BROKER_URL pair, which reaches a
	// GMC-provisioned AGC only under the testing-only --allow-agc-extra-env flag.
	ScaleSetStubBaseURL string

	conditionCh chan conditionUpdate

	// eventCh carries owner-scoped Kubernetes Events pushed from listener/provisioner
	// goroutines; drainEvents records them on the live RunnerSet each reconcile.
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

	reconcileCount atomic.Int64
}

// condUpdater returns a ConditionUpdater that enqueues onto conditionCh and wakes the
// reconciler (wakeCh) so the pushed condition is drained promptly (Q333).
func (r *RunnerSetReconciler) condUpdater() *channelConditionUpdater {
	return &channelConditionUpdater{ch: r.conditionCh, wake: r.wakeCh}
}

// eventRecorder returns an EventRecorder that enqueues onto eventCh and wakes the
// reconciler (wakeCh) so the pushed event is drained promptly (Q333).
func (r *RunnerSetReconciler) eventRecorder() *channelEventRecorder {
	return &channelEventRecorder{ch: r.eventCh, wake: r.wakeCh}
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

	// Export the worker-capacity conditions (Q303) as gauges from the cached client,
	// so a stalled set is alertable without kube-state-metrics — the v2 twin of the
	// v1 RunnerGroup registrations (Q319).
	registerRunnerSetCapacityMetrics(mgr.GetClient())
	registerRunnerSetEgressAuditMetrics(mgr.GetClient())

	// Drain listener goroutines inside the manager's graceful shutdown so SIGTERM
	// cannot kill the process mid-DELETE and leak GitHub-side sessions (Q222).
	if err := mgr.Add(&listenerShutdown{stop: r.stopListeners, log: r.Log, owner: "RunnerSet"}); err != nil {
		return fmt.Errorf("add listener shutdown runnable: %w", err)
	}

	b := ctrl.NewControllerManagedBy(mgr).
		// Bounded retry backoff so a reconcile error cannot strand the worker-pod
		// reaper's deadline (retryBackoffCap).
		WithOptions(controller.Options{RateLimiter: reconcileRateLimiter()}).
		For(&v2.RunnerSet{}).
		// Worker pods carry LabelRunnerSet; re-reconcile on their lifecycle events
		// so status.activeSessions and the reaper track pod phase transitions, and
		// recover a disrupted scale-set worker off the event itself (Q1029).
		Watches(
			&corev1.Pod{},
			r.workerPodHandler(),
			builder.WithPredicates(runnerSetWorkerPodPredicate()),
		).
		// Referent watches: a RunnerSet sitting Ready=False/<Ref>NotFound flips the
		// moment its missing referent appears.
		Watches(
			&v2.ActionsGateway{},
			handler.EnqueueRequestsFromMapFunc(r.gatewayToRunnerSets),
		).
		Watches(
			&v2.EgressProxy{},
			handler.EnqueueRequestsFromMapFunc(r.proxyToRunnerSets),
		).
		Watches(
			&v2.RunnerTemplate{},
			handler.EnqueueRequestsFromMapFunc(r.templateToRunnerSets),
		).
		// Reconcile when an admin changes a namespace ResourceQuota's .spec.hard so
		// the WorkerQuota{Pressure,Exceeded} conditions refresh promptly (Q82/Q326)
		// — mirrors the v1 RunnerGroup watch. The predicate ignores .status.used
		// churn; transient used drift is picked up by the worker-pod watch.
		Watches(
			&corev1.ResourceQuota{},
			handler.EnqueueRequestsFromMapFunc(r.quotaToRunnerSets),
			builder.WithPredicates(quotaHardChangedPredicate()),
		).
		// Wake the reconciler when a listener/provisioner goroutine pushes a condition
		// or event onto conditionCh/eventCh (Q333). Without this the pushed update sits
		// in the channel until the next worker-Pod event or the resync period drains it,
		// so an otherwise-idle RunnerSet could lag a status condition up to 10h. The
		// GenericEvents carry the owning RunnerSet's namespace/name, which
		// EnqueueRequestForObject maps straight to its reconcile.Request.
		WatchesRawSource(source.Channel(r.wakeCh, &handler.EnqueueRequestForObject{}))

	// ClusterRunnerTemplate (cluster-scoped) is watched only when GatewayName is set.
	// Only a GMC-provisioned AGC (which carries GATEWAY_NAME) holds the per-gateway
	// ClusterRoleBinding to agc-clusterrunnertemplate-reader; establishing this
	// cluster-scoped informer without that grant fails RBAC and aborts the manager's
	// cache sync. A RunnerSet referencing a ClusterRunnerTemplate flips Ready the
	// moment it syncs (§H.7). The namespace-scoped manager cache
	// serves cluster-scoped kinds from a cluster-wide informer.
	//
	// Only a test harness reaches this with an empty GatewayName — main.go
	// declines to register the reconciler on an unscoped AGC (Q466). The guard
	// stays so an unscoped harness never establishes the cluster-scoped informer.
	if r.GatewayName != "" {
		b = b.Watches(
			&v2.ClusterRunnerTemplate{},
			handler.EnqueueRequestsFromMapFunc(r.clusterTemplateToRunnerSets),
		)
	}

	return b.Named("runnerset").Complete(r)
}

func (r *RunnerSetReconciler) ensureMaps() {
	if r.scaleSetListeners == nil {
		r.scaleSetListeners = make(map[types.NamespacedName]*scaleSetListenerHandle)
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

// Reconcile drives a RunnerSet: resolve references, and once they resolve, ensure
// the scale-set listener is running and worker pods are reaped.
func (r *RunnerSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.reconcileCount.Add(1)
	if r.Log == nil {
		r.Log = slog.Default()
	}
	r.ensureMaps()
	log := r.Log.With("namespace", req.Namespace, "name", req.Name)

	var rs v2.RunnerSet
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
	r.drainEvents(&rs)

	// Ahead of the deletion check: removing the finalizer is what releases the set to the
	// garbage collector, and a set deleted across an upgrade reaches this AGC already
	// terminating.
	r.restampConfigMapOwners(ctx, log, &rs)

	if !rs.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &rs)
	}

	if !controllerutil.ContainsFinalizer(&rs, runnerSetFinalizer) {
		controllerutil.AddFinalizer(&rs, runnerSetFinalizer)
		if err := r.Update(ctx, &rs); err != nil {
			return ctrl.Result{}, err
		}
		// The Update fires the primary watch, which carries no predicate, so the
		// next reconcile is already queued.
		return ctrl.Result{}, nil
	}

	// The gateway going away takes this AGC with it, and it is the only reaper the
	// set's worker pods have. Checked before reference resolution so a set whose
	// template or proxy is also missing still reaps (Q547).
	terminating, err := gatewayTerminating(ctx, r.Client, &rs)
	if err != nil {
		return ctrl.Result{}, err
	}
	if terminating {
		return r.reconcileGatewayTerminating(ctx, log, &rs)
	}

	// Hand the set to the pending sweep where a previous process left re-runs parked in
	// its ledger (Q1244). Ahead of reference resolution, so a set whose template or proxy
	// is missing is still swept and its entries expire reported. Once per process per
	// set; a failed read retries.
	if r.Provisioner != nil {
		if err := r.Provisioner.AdoptPendingReruns(ctx, r.provisionerTarget(&rs)); err != nil {
			log.Warn("could not read the failure ledger for parked re-runs", "error", err)
		}
	}

	// 1. Resolve references. Fail closed: until every reference resolves, the
	// RunnerSet sits Ready=False/<Ref>NotFound with no listeners running, so no
	// worker pod is ever provisioned in the gap (§H.7). The referent watches
	// re-enqueue when a missing object appears.
	refs, res := resolveRunnerSetRefs(ctx, r.Client, r.APIReader, &rs)
	if res.err != nil {
		return ctrl.Result{}, res.err
	}
	if !res.resolved() {
		// Stop any running listeners — a reference that vanished must not keep
		// acquiring jobs (the per-job Resolve would fail them closed anyway, but
		// stopping avoids the churn and reflects reality).
		r.stopScaleSetListener(req.NamespacedName)
		// Distinguish a referent deleted out from under a previously-resolved set
		// (TemplateDeleted/ProxyDeleted, §H.8 degrade-not-block) from one that never
		// existed, then drop any status marker a plain NotFound invalidates so later
		// reconciles cannot upgrade from stale evidence.
		reason, message := vanishedReferentReason(&rs, res)
		clearStaleResolutionMarkers(&rs, reason)
		r.setReadyCondition(&rs, false, reason, message)
		// No template resolved, so there is no known reap-blocking sidecar: clear the
		// Q249 condition and gauge rather than leave a stale True/non-zero behind (e.g.
		// after the resolved template was deleted).
		r.setReapBlockingSidecarStatus(&rs, nil, nil)
		// Likewise clear the worker-capacity conditions (Q303): with no listeners running
		// no new worker pods are provisioned, so a previously-True quota/unschedulable
		// alarm must not linger behind the dominant Ready=False/<Ref>NotFound signal.
		r.clearWorkerCapacityConditions(&rs)
		rs.Status.ActiveSessions = 0
		rs.Status.ActiveJobs = 0
		rs.Status.PendingJobs = 0
		clearAdvertisedCapacity(&rs)
		rs.Status.ObservedGeneration = rs.Generation
		if err := r.Status().Update(ctx, &rs); err != nil && !apierrors.IsConflict(err) {
			return ctrl.Result{}, err
		}
		// Every other fail-closed reason names a watched referent, so its appearance
		// re-enqueues the set (§H.7). A projected proxy share is the exception: the AGC
		// may get that ConfigMap but not watch it (§H.9), so a grant arriving after the
		// reference produces no event and the set would sit here until the informer
		// resync. Poll it instead — the alternative is list/watch over every ConfigMap
		// in the tenant namespace, which RBAC cannot narrow to the labelled ones.
		if reason == v2.ReasonProxyShareNotGranted {
			return ctrl.Result{RequeueAfter: r.proxyShareRecheckInterval()}, nil
		}
		return ctrl.Result{}, nil
	}

	// References resolved: record the egress mode (Q168, §H.10). A nil resolved proxy
	// means direct egress — proxyMode Direct + the advisory EgressUnattributed
	// condition (True), so an operator sees the workload has no per-tenant egress IP
	// identity. It is advisory only and never gates Ready: direct egress is a
	// supported, NetworkPolicy-restricted mode. Set before any later status write so
	// every exit below persists it.
	r.setEgressMode(&rs, refs.proxy != nil)
	r.setEgressAuditStatus(&rs, refs.gateway, refs.proxy)

	// Record which rung of the optional-templateRef chain supplied the pod shape (Q172,
	// §H.4): the set's own templateRef, the gateway's defaultTemplateRef, or the single
	// cluster-default ClusterRunnerTemplate. Auditable in status.templateSource so an
	// operator sees whether a set runs on an explicit template or a default.
	rs.Status.TemplateSource = refs.templateSource

	// Warn (non-blocking) if the resolved template carries a regular sidecar that may
	// block worker-pod reaping (Q249): surface it as the PossibleReapBlockingSidecar
	// condition and the reap-blocking-sidecar gauge, gated by the self-exiting-sidecars
	// opt-out. Advisory only — it never gates Ready.
	r.setReapBlockingSidecarStatus(&rs, refs.template, refs.templateAnnotations)

	// Judge the runner version the effective worker image ships against GitHub's
	// enforced minimum (Q715). Advisory like the two above, and the only producer of
	// RunnerVersionTooOld on the ScaleSet tier — the protocol carries no runner version
	// at session creation, so the listener has no rejection to report.
	r.setRunnerVersionStatus(&rs, refs.template)

	// Surface the measured worker sizing recommendation and judge the template's
	// ask against it (Q359 Phase 2). Advisory only; the listener's status write
	// persists it.
	r.applySizingStatus(&rs, refs.template)

	// Report a Throughput profile cancelled by an admission-injected CPU limit (Q489)
	// — the one sizing conflict that rejects nothing, read off the worker pods the
	// profile built rather than inferred from any one policy object. Advisory, and a
	// no-op under every other profile. The existing worker-pod watch is what keeps it
	// current: the pod carrying the injected limit re-reconciles this set.
	r.applySizingProfileOverride(ctx, &rs, refs.template)

	// 2. Recover evicted scale-set workers (Q417). Runs BEFORE the reaper: the
	// evicted pod is the only record of which run to re-run, and the reaper deletes
	// terminal pods once completedPodTTL elapses. It is a no-op for a set with
	// nothing evicted. Deliberately not waited on — recovery waits out evictionRetryDelay
	// before calling GitHub, and a reconcile must not block on either.
	if _, err := r.Provisioner.RecoverEvictedScaleSetWorkers(ctx, r.provisionerTarget(&rs)); err != nil {
		// A failed scan must not stop the reconcile: the pods stay unclaimed, so
		// the next reconcile retries them. Worth surfacing, not worth requeuing for.
		log.Warn("scale-set eviction recovery scan failed", "error", err)
	}
	// 2b. Recover workers that were already gone when this process started (Q844) —
	// the preemption and drain victims whose pod, the only record of the disruption,
	// was deleted while no AGC was watching. Reads the in-flight set the listener
	// persisted; runs once per process per set, and here rather than later because
	// both the reaper below and the listener's own first poll retire the evidence it
	// reads.
	// Deliberately not waited on, for the same reason the scan above is not: the
	// recovery waits out evictionRetryDelay before calling GitHub.
	_ = r.recoverOrphanedScaleSetWorkers(ctx, log, &rs)

	// 3. Reap expired worker pods (terminal past completedPodTTL, Pending past
	// pendingPodDeadline). Runs before the token fetch so cleanup keeps working
	// during a GitHub outage. podCounts is the pod phase snapshot used to
	// populate status.activeJobs/pendingJobs.
	var observed observedRunner
	reapAfter, podCounts, err := r.reapWorkerPods(ctx, log, &rs, &observed)
	if err != nil {
		return ctrl.Result{}, err
	}

	result, err := r.reconcileScaleSetListener(ctx, log, &rs, refs, reapAfter, podCounts, observed)
	if err != nil {
		// controller-runtime discards a Result returned beside an error and warns
		// that the reconciler returned both; the error path already requeues with
		// backoff, so there is nothing to fold in.
		return ctrl.Result{}, err
	}
	return r.withProxyShareRecheck(result, refs), nil
}

// defaultProxyShareRecheckInterval is how often a RunnerSet re-reads a projected
// proxy share ConfigMap. The AGC's Role grants get on ConfigMaps and not list/watch
// (§H.9), so this is the only thing that bounds either direction of a grant: how
// long a withdrawn grant keeps a set Ready and acquiring, and how long a set sits
// ProxyShareNotGranted after the provider consents. Kubernetes RBAC has no label
// selector, so the watch the alternative needs cannot be scoped to the labelled
// share ConfigMaps — it would be list/watch over every ConfigMap in the tenant
// namespace. One minute matches the GMC's githubCABundleReprobeInterval, which polls
// a tenant ConfigMap it cannot watch for the same reason.
const defaultProxyShareRecheckInterval = time.Minute

// proxyShareRecheckInterval returns the configured share re-check cadence, or the
// default when unset.
func (r *RunnerSetReconciler) proxyShareRecheckInterval() time.Duration {
	if r.ProxyShareRecheckInterval > 0 {
		return r.ProxyShareRecheckInterval
	}
	return defaultProxyShareRecheckInterval
}

// withProxyShareRecheck folds the bounded share re-check into res for a set whose
// proxy resolved through a projection, taking whichever deadline is sooner. A no-op
// for a colocated proxy, which the EgressProxy watch already covers.
func (r *RunnerSetReconciler) withProxyShareRecheck(res ctrl.Result, refs *resolvedRefs) ctrl.Result {
	if !refs.resolvedThroughShare() {
		return res
	}
	if interval := r.proxyShareRecheckInterval(); res.RequeueAfter <= 0 || interval < res.RequeueAfter {
		res.RequeueAfter = interval
	}
	return res
}

// reconcileDelete stops the set's listener and removes the finalizer.
func (r *RunnerSetReconciler) reconcileDelete(ctx context.Context, rs *v2.RunnerSet) (ctrl.Result, error) {
	key := types.NamespacedName{Namespace: rs.Namespace, Name: rs.Name}

	r.cleanupLocalState(key)

	controllerutil.RemoveFinalizer(rs, runnerSetFinalizer)
	if err := r.Update(ctx, rs); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// cleanupLocalState stops the listener and drops the in-memory state for key. Never
// touches the API server, so it is safe on both the deletion and NotFound paths.
func (r *RunnerSetReconciler) cleanupLocalState(key types.NamespacedName) {
	r.stopScaleSetListener(key)
	r.pendingConds.forget(key)
	r.ownerRefsRestamped.Delete(key)
	// Drop the Q249 reap-blocking-sidecar gauge series so a deleted (or foreign-gateway)
	// RunnerSet does not leave a stale value behind.
	if r.Metrics != nil {
		r.Metrics.ReapBlockingSidecarTemplates.DeleteLabelValues(key.Namespace, key.Name)
	}
	// Same for the scale-set tier's capacity gauges (Q443): a deleted set that stopped
	// polling would otherwise report its last advertised capacity forever.
	r.ScaleSetMetrics.DeleteRunnerSet(key.Namespace, key.Name)
}

func (r *RunnerSetReconciler) recordEvent(rs *v2.RunnerSet, eventtype, reason, action, note string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(rs, nil, eventtype, reason, action, note, args...)
}

// drainConditions reads pending listener-pushed condition updates and merges those
// for this RunnerSet into its status; others are re-enqueued.
func (r *RunnerSetReconciler) drainConditions(rs *v2.RunnerSet) {
	key := types.NamespacedName{Namespace: rs.Namespace, Name: rs.Name}
	// Re-apply any conditions a prior reconcile drained but did not persist (dropping
	// those the live status now reflects), then drain fresh pushes over the top so the
	// latest push per type wins (Q333).
	r.pendingConds.apply(key, &rs.Status.Conditions)
	var skipped []conditionUpdate
	for {
		select {
		case upd := <-r.conditionCh:
			if upd.namespace == rs.Namespace && upd.name == rs.Name {
				meta.SetStatusCondition(&rs.Status.Conditions, upd.condition)
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
		}
	}
}

// drainEvents records pending owner-scoped Events on this RunnerSet; events for
// other RunnerSets are re-enqueued (mirroring drainConditions). Each event is
// consumed once, so it is never re-emitted on subsequent reconciles.
func (r *RunnerSetReconciler) drainEvents(rs *v2.RunnerSet) {
	var skipped []eventRecord
	for {
		select {
		case ev := <-r.eventCh:
			if ev.namespace == rs.Namespace && ev.name == rs.Name {
				r.recordEvent(rs, ev.eventtype, ev.reason, ev.action, ev.note)
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
		}
	}
}

// setReadyCondition upserts the Ready condition and emits an Event on a genuine
// transition.
func (r *RunnerSetReconciler) setReadyCondition(rs *v2.RunnerSet, ready bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	prev := conditionValue(rs.Status.Conditions, v2.ConditionReady)
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type:               v2.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: rs.Generation,
	})
	if prev.Status != status {
		etype := corev1.EventTypeNormal
		if !ready {
			etype = corev1.EventTypeWarning
		}
		r.recordEvent(rs, etype, reason, "Reconcile", msg)
	}
}

// setEgressMode records how this RunnerSet's worker egress reaches GitHub (Q168,
// §H.10): proxyMode Proxied + EgressUnattributed=False when a proxy resolved, or
// proxyMode Direct + EgressUnattributed=True when none did. The condition is advisory
// (it surfaces the per-tenant-IP-attribution trade an operator opted out of by not
// attaching a proxy) and does not gate Ready — direct egress is a supported,
// NetworkPolicy-restricted mode.
func (r *RunnerSetReconciler) setEgressMode(rs *v2.RunnerSet, proxied bool) {
	status := metav1.ConditionTrue
	mode := v2.ProxyModeDirect
	reason := v2.ReasonDirectEgress
	msg := "no proxyRef/defaultProxyRef: worker egress is direct (restricted to DNS + GitHub) and has no per-tenant egress IP identity"
	if proxied {
		status = metav1.ConditionFalse
		mode = v2.ProxyModeProxied
		reason = v2.ReasonProxiedEgress
		msg = "worker egress is attributed to the resolved EgressProxy's per-tenant IPs"
	}
	rs.Status.ProxyMode = mode
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type:               v2.ConditionEgressUnattributed,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: rs.Generation,
	})
}

// proxyAuditConnectionsWithSource is the EgressProxy.spec.auditLogging value whose
// records carry the source address the egress-audit join keys on (Q986).
const proxyAuditConnectionsWithSource = "ConnectionsWithSource"

// setEgressAuditStatus records whether this RunnerSet's worker egress is attributable
// to a tenant and a job (Q1069): the per-set twin of the gateway's
// EgressAuditUnattributed, read off the pool this set actually resolved — its own
// proxyRef before the gateway's defaultProxyRef — rather than the gateway's default.
// The worker half is the gateway's spec.auditLogging, the source half the resolved
// pool's. Advisory and abnormal-is-True like the gateway's; it never gates Ready.
func (r *RunnerSetReconciler) setEgressAuditStatus(rs *v2.RunnerSet, gw *v2.ActionsGateway, proxy *resolvedProxy) {
	reason, msg := runnerSetEgressAudit(gw, proxy)
	status := metav1.ConditionTrue
	if reason == v2.ReasonEgressAuditJoined {
		status = metav1.ConditionFalse
	}
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type:               v2.ConditionEgressAuditUnattributed,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: rs.Generation,
	})
}

// runnerSetEgressAudit reports the EgressAuditUnattributed reason and message for a
// set's gateway and resolved pool (nil for direct egress). Every reason but
// ReasonEgressAuditJoined names the half to turn on, and a direct-egress set splits on
// the gateway half so the reason alone says whether the tenant opted in.
func runnerSetEgressAudit(gw *v2.ActionsGateway, proxy *resolvedProxy) (reason, message string) {
	workerHalf := gw.Spec.AuditLogging == string(provisioner.WorkerAuditAddresses)
	if proxy == nil {
		if workerHalf {
			return v2.ReasonWorkerAuditWithoutProxy,
				fmt.Sprintf("ActionsGateway %q logs %s but this set has no proxyRef/defaultProxyRef: worker egress is direct, so no per-connection record exists to join the addresses to. Attach an EgressProxy that logs %s",
					gw.Name, provisioner.WorkerAuditAddresses, proxyAuditConnectionsWithSource)
		}
		return v2.ReasonDirectEgress,
			"no proxyRef/defaultProxyRef: worker egress is direct, so no per-connection record attributes it"
	}
	// An empty value is the CRD default, so report it as Off rather than as "".
	effective := func(v string) string {
		if v == "" {
			return string(provisioner.WorkerAuditOff)
		}
		return v
	}
	proxyHalf := proxy.auditLogging == proxyAuditConnectionsWithSource
	switch {
	case workerHalf && proxyHalf:
		return v2.ReasonEgressAuditJoined,
			fmt.Sprintf("ActionsGateway %q logs %s and EgressProxy %q logs %s: an egress audit record from this set's workers joins to this tenant and the job that held the address",
				gw.Name, provisioner.WorkerAuditAddresses, proxy.name, proxyAuditConnectionsWithSource)
	case proxyHalf:
		return v2.ReasonWorkerAuditDisabled,
			fmt.Sprintf("EgressProxy %q logs %s but ActionsGateway %q's spec.auditLogging is %q: the source addresses it records name no tenant or job. Set spec.auditLogging: %s on the gateway",
				proxy.name, proxyAuditConnectionsWithSource, gw.Name, effective(gw.Spec.AuditLogging), provisioner.WorkerAuditAddresses)
	case workerHalf:
		return v2.ReasonProxySourceAuditDisabled,
			fmt.Sprintf("ActionsGateway %q logs %s but EgressProxy %q, which this set's workers egress through, logs %q: no egress record carries a source address to join on. Set that proxy's spec.auditLogging: %s",
				gw.Name, provisioner.WorkerAuditAddresses, proxy.name, effective(proxy.auditLogging), proxyAuditConnectionsWithSource)
	default:
		return v2.ReasonEgressAuditDisabled,
			fmt.Sprintf("neither ActionsGateway %q's spec.auditLogging nor EgressProxy %q's is on, so this set's worker egress is not attributable to a tenant or a job", gw.Name, proxy.name)
	}
}

// setReapBlockingSidecarStatus surfaces whether the RunnerSet's resolved worker
// template carries a regular (non-native) sidecar that may keep the worker pod alive
// after the runner container exits — the Q247 stranding class reproduced by a
// docker:dind sidecar declared as a regular container (Q249). It sets the advisory
// PossibleReapBlockingSidecar condition (abnormal-is-True, never gates Ready) and the
// reap-blocking-sidecar gauge, both gated by the self-exiting-sidecars opt-out so an
// acknowledged sidecar fires none of them. A nil template (references unresolved)
// clears both. It is a warning, not enforcement: the fix is native sidecars, which the
// message and the admission warning steer operators toward.
func (r *RunnerSetReconciler) setReapBlockingSidecarStatus(rs *v2.RunnerSet, template *v2.RunnerTemplateSpec, annotations map[string]string) {
	names := v2.ReapBlockingSidecars(template, annotations)

	if r.Metrics != nil {
		r.Metrics.ReapBlockingSidecarTemplates.WithLabelValues(rs.Namespace, rs.Name).Set(float64(len(names)))
	}

	status := metav1.ConditionFalse
	reason := v2.ReasonNoReapBlockingSidecar
	msg := "no regular reap-blocking sidecar in the resolved worker template"
	if len(names) > 0 {
		status = metav1.ConditionTrue
		reason = v2.ReasonReapBlockingSidecar
		msg = fmt.Sprintf("worker pod sidecar container(s) %s are regular containers that may keep the pod from reaping after the runner exits, stranding the runner slot; declare them as native sidecars (restartPolicy: Always init containers, Kubernetes >= 1.29) or acknowledge them in the %s annotation",
			strings.Join(names, ", "), v2.SelfExitingSidecarsAnnotation)
	}
	prev := conditionValue(rs.Status.Conditions, v2.ConditionPossibleReapBlockingSidecar)
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type:               v2.ConditionPossibleReapBlockingSidecar,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: rs.Generation,
	})
	// Warn once on a genuine False→True transition so the misconfiguration lands in the
	// event stream, not only in status.
	if status == metav1.ConditionTrue && prev.Status != metav1.ConditionTrue {
		r.recordEvent(rs, corev1.EventTypeWarning, reason, "Reconcile", msg)
	}
}

// setRunnerVersionStatus judges the runner version this set's effective worker image
// declares against GitHub's enforced registration minimum, and publishes the verdict
// as RunnerVersionTooOld (Q715). It asks GitHub nothing: the scale-set protocol carries
// no runner version, so the listener never sees a rejection to report.
//
// A nil template means the references have not resolved; the image the set would run is
// unknown, so nothing is published rather than judging the AGC-wide default the set may
// never use.
func (r *RunnerSetReconciler) setRunnerVersionStatus(rs *v2.RunnerSet, template *v2.RunnerTemplateSpec) {
	if template == nil || r.Provisioner == nil {
		return
	}
	image := r.Provisioner.EffectiveWorkerImage(template.WorkerImage)
	// The wake fires from the resolver's goroutine after this reconcile has returned,
	// so it captures the key by value: rs is rewritten by the status update's decode.
	namespace, name := rs.Namespace, rs.Name
	lookup := imageLookup(r.ImageResolver, image, template.PodTemplate.Spec.ImagePullSecrets,
		func() { wakeReconciler(r.wakeCh, namespace, name) })
	cond := runnercore.WorkerRunnerVersionConditionWithRegistry(image, lookup, rs.Generation)

	prev := conditionValue(rs.Status.Conditions, cond.Type)
	meta.SetStatusCondition(&rs.Status.Conditions, cond)
	// Warn once on a genuine transition into too-old so the deadline lands in the event
	// stream while there is still time to change the image.
	if cond.Status == metav1.ConditionTrue && prev.Status != metav1.ConditionTrue {
		r.recordEvent(rs, corev1.EventTypeWarning, cond.Reason, "Reconcile", cond.Message)
	}
}

// nowFunc returns the reaper clock: Now when set, time.Now otherwise.
func (r *RunnerSetReconciler) nowFunc() func() time.Time {
	if r.Now != nil {
		return r.Now
	}
	return time.Now
}

// provisionerTarget returns the provisioner.Target adapter for rs — the seam the
// Provisioner builds worker pods, counts capacity, and records owner Events through.
// Every caller needs the identical adapter (the multiplexer path, the scale-set
// listener path, and the eviction-recovery pass), so it is constructed once here rather
// than re-literalled per call site.
func (r *RunnerSetReconciler) provisionerTarget(rs *v2.RunnerSet) *runnerSetTarget {
	return &runnerSetTarget{
		client: r.Client,
		reader: r.APIReader,
		prov:   r.Provisioner,
		key:    types.NamespacedName{Namespace: rs.Namespace, Name: rs.Name},
		uid:    rs.UID,
		events: r.eventRecorder(),
	}
}

// reapWorkerPods deletes worker pods this RunnerSet no longer needs (terminal past
// completedPodTTL, Pending past pendingPodDeadline, and Pending or Running past its own
// job's completion), mirroring the RunnerGroup reaper but filtering on LabelRunnerSet and
// reading the RunnerSet's tunables. It returns the time until the earliest retained
// pod becomes due (0 = none) and a pod phase count for status.activeJobs/pendingJobs.
func (r *RunnerSetReconciler) reapWorkerPods(ctx context.Context, log *slog.Logger, rs *v2.RunnerSet, observed *observedRunner) (time.Duration, workerPodCounts, error) {
	return reapWorkerPodsByLabel(ctx, r.Client, r.nowFunc()(), rs.Namespace, rs.Name,
		provisioner.LabelRunnerSet,
		provisioner.CompletedPodTTLOrDefault(rs.Spec.CompletedPodTTL),
		provisioner.PendingPodDeadlineOrDefault(rs.Spec.PendingPodDeadline),
		log, r.Metrics,
		reapHooks{
			emitStuckPending: func(podName string, deadline time.Duration) {
				r.recordEvent(rs, corev1.EventTypeWarning, "WorkerPodStuckPending", "ReapWorkerPods",
					"worker pod %s was Pending for more than %s and has been deleted; "+
						"check the template image and scheduling constraints", podName, deadline)
			},
			emitOrphanedRunning: func(podName string, grace time.Duration) {
				// Operator-visible: on the scale-set tier this is the "registered but
				// never got its job" worker, which held a concurrency slot and a node
				// for nothing until this reap (Q420).
				r.recordEvent(rs, corev1.EventTypeWarning, "WorkerPodOrphanedRunning", "ReapWorkerPods",
					"worker pod %s was still Running %s after its job completed and has been deleted; "+
						"the runner never received its job, or a container in the pod outlived it", podName, grace)
			},
			emitCompletedPending: func(podName string, grace time.Duration) {
				// Operator-visible: this is the scale-set tier's own failure mode — the
				// job completed while its worker was still Pending, so the completion
				// reclaimed the Secret the pod had not mounted yet (Q575).
				r.recordEvent(rs, corev1.EventTypeWarning, "WorkerPodCompletedPending", "ReapWorkerPods",
					"worker pod %s was still Pending %s after its job completed and has been deleted; "+
						"the job ended before the pod could start, so the pod had nothing to run", podName, grace)
			},
			emitLifetimeExceeded: func(podName string) {
				// Operator-visible: name the cause and the field to raise, so a
				// legitimately long job killed by the cap diagnoses itself (Q438).
				r.recordEvent(rs, corev1.EventTypeWarning, "WorkerPodLifetimeExceeded", "ReapWorkerPods",
					"worker pod %s was killed by the kubelet after exceeding the %s worker lifetime "+
						"(spec.maxWorkerLifetime) and has been deleted; if the job was legitimately "+
						"this long, raise spec.maxWorkerLifetime on this RunnerSet or set it to 0s "+
						"to disable the cap", podName,
					provisioner.MaxWorkerLifetimeOrDefault(rs.Spec.MaxWorkerLifetime))
			},
			deregisterRunner: r.deregisterScaleSetRunner(rs, log),
			recoverAbandoned: r.recoverAbandonedRun(rs),
			// v2 only: v1 is terminal and grows no status field for this (Q792).
			observeWorkerReport: observed.observe,
		})
}

// deregisterScaleSetRunner returns the reaper's deregistration hook for this RunnerSet:
// it removes the GitHub runner record a reaped worker pre-registered, closing the
// registration leak at its source (Q550). It returns nil for a set whose listener has
// not started, because the scale-set client is the listener's.
//
// Best-effort by construction: a record the ephemeral runner already removed is a
// no-op, a record still running a job must be kept (RunnerBusyError), and any other
// failure is logged and left to the start-up sweep. None of them may stop the reap —
// the pod is condemned either way, and a retained pod would hold its concurrency slot.
func (r *RunnerSetReconciler) deregisterScaleSetRunner(rs *v2.RunnerSet, log *slog.Logger) func(context.Context, string) {
	ssClient, scaleSetName := r.scaleSetClientFor(types.NamespacedName{Namespace: rs.Namespace, Name: rs.Name})
	if ssClient == nil {
		return nil
	}
	return func(ctx context.Context, runnerName string) {
		// The annotation is a pod field, writable by anyone who can patch the pod, and
		// the name resolves across the whole org or repo listing, so only a name this
		// set's listener minted is deregistered — the sweep's ownership test (Q1325).
		if !scalesetlistener.MintedBy(scaleSetName, runnerName) {
			log.Info("reaper: not deregistering a runner name this scale set did not mint",
				"runner", runnerName, "scaleSet", scaleSetName)
			return
		}
		deleted, err := ssClient.DeregisterRunnerByName(ctx, runnerName)
		switch {
		case err != nil:
			log.Debug("reaper: deregister worker runner record", "runner", runnerName, "error", err)
		case deleted:
			log.Info("reaper: deregistered worker runner record", "runner", runnerName)
		}
	}
}

// recoverAbandonedRun returns the reaper's abandoned-run recovery hook: force-cancel the
// run behind a worker pod the deadline reap just removed while it was still Pending, and
// queue it for automatic re-run once capacity returns (Q766). The recovery is deliberately
// not waited on — the force-cancel runs bounded on its own detached context, and a
// reconcile must not stall on GitHub.
func (r *RunnerSetReconciler) recoverAbandonedRun(rs *v2.RunnerSet) func(context.Context, *corev1.Pod) {
	if r.Provisioner == nil {
		return nil
	}
	return func(ctx context.Context, pod *corev1.Pod) {
		_ = r.Provisioner.RecoverAbandonedScaleSetWorker(ctx, r.provisionerTarget(rs), pod)
	}
}

// ReconcileCountForTest returns how many times Reconcile has run (integration tests).
func (r *RunnerSetReconciler) ReconcileCountForTest() int64 {
	return r.reconcileCount.Load()
}

// SetConditionForTest enqueues a condition update as if it came from a listener
// goroutine, exercising the same push path (including the Q333 reconciler wake) so
// integration tests can prove a pushed condition wakes an idle reconciler. Intended
// for use in tests only.
func (r *RunnerSetReconciler) SetConditionForTest(ns, name string, cond metav1.Condition) {
	if r.conditionCh == nil {
		return
	}
	r.condUpdater().SetCondition(ns, name, cond)
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

// workerPodHandler is the worker-pod watch's handler: the RunnerSet enqueue every
// event gets, with scale-set disruption recovery run off the event first (Q1029). Why
// the event rather than the reconcile it enqueues is on
// provisioner.RecoverDisruptedScaleSetWorker. Updates and deletes also snapshot a
// worker's terminal state for flaky-job detection (Q555), since the pod may be gone by
// the time its job's completion arrives.
func (r *RunnerSetReconciler) workerPodHandler() handler.EventHandler {
	enqueue := handler.EnqueueRequestsFromMapFunc(r.podToRunnerSet)
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.recoverDisruptedWorkerPod(ctx, e.Object)
			enqueue.Create(ctx, e, q)
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.snapshotWorkerPod(ctx, e.ObjectNew)
			r.recoverDisruptedWorkerPod(ctx, e.ObjectNew)
			enqueue.Update(ctx, e, q)
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.snapshotWorkerPod(ctx, e.Object)
			enqueue.Delete(ctx, e, q)
		},
		GenericFunc: enqueue.Generic,
	}
}

// snapshotWorkerPod hands a worker pod from a watch event to the provisioner's
// snapshot store, which keeps only scale-set workers at a terminal transition, with
// its set's target to read the egress proxy's readiness from the cache.
func (r *RunnerSetReconciler) snapshotWorkerPod(ctx context.Context, obj client.Object) {
	pod, ok := obj.(*corev1.Pod)
	if !ok || r.Provisioner == nil {
		return
	}
	var proxy provisioner.EgressProxyReader
	if set := pod.Labels[provisioner.LabelRunnerSet]; set != "" {
		proxy = &runnerSetTarget{client: r.Client, key: types.NamespacedName{Namespace: pod.Namespace, Name: set}}
	}
	r.Provisioner.SnapshotScaleSetWorker(ctx, proxy, pod)
}

// disruptedWorkerRecoveryBudget bounds the synchronous half of a recovery started off a
// watch event: two cached reads, the claim write with its conflict retries, and
// the retry-budget reservation. The GitHub re-run itself runs on handleEviction's own
// detached context, as it does for the scan.
const disruptedWorkerRecoveryBudget = 30 * time.Second

// recoverDisruptedWorkerPod starts the recovery of a scale-set worker pod that a watch
// event shows disrupted, off the reconcile queue. The pre-checks are the cheap half of
// the judge — a phase or a preemption marker the recovery arms could act on, and no
// claim yet — so the ordinary Pending→Running event costs nothing here. The rest runs
// on its own goroutine: the RunnerSet read is cached, the claim is a live patch, and a
// watch handler must not wait on either. Every recovery arm is at-most-once on the pod's
// claim annotation, so a pod the scan also sees is recovered once.
//
// The goroutine's context is detached from the handler's, which controller-runtime
// cancels the moment the handler returns (each delivery runs under its own WithCancel
// in pkg/internal/source/event_handler.go, measured on v0.24.1), and bounded instead.
func (r *RunnerSetReconciler) recoverDisruptedWorkerPod(ctx context.Context, obj client.Object) {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Labels[provisioner.LabelAcquisitionProtocol] != provisioner.AcquisitionProtocolScaleSet {
		return
	}
	if _, claimed := pod.Annotations[provisioner.AnnotationEvictionHandledAt]; claimed {
		return
	}
	if pod.Status.Phase != corev1.PodFailed && !provisioner.PreemptedByScheduler(pod) {
		return
	}
	// The event's object is the cache's; the claim mutates the pod it is handed.
	pod = pod.DeepCopy()
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), disruptedWorkerRecoveryBudget)
		defer cancel()
		var rs v2.RunnerSet
		key := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Labels[provisioner.LabelRunnerSet]}
		if err := r.Get(ctx, key, &rs); err != nil {
			r.Log.Debug("disrupted scale-set worker's RunnerSet is not readable; leaving it to the recovery scan",
				"namespace", key.Namespace, "name", key.Name, "podName", pod.Name, "error", err)
			return
		}
		if _, err := r.Provisioner.RecoverDisruptedScaleSetWorker(ctx, r.provisionerTarget(&rs), pod); err != nil {
			r.Log.Warn("scale-set worker disruption recovery off the pod watch failed; the recovery scan retries it",
				"namespace", key.Namespace, "name", key.Name, "podName", pod.Name, "error", err)
		}
	}()
}

// gatewayToRunnerSets enqueues every RunnerSet in the gateway's namespace whose
// gatewayRef names it.
func (r *RunnerSetReconciler) gatewayToRunnerSets(ctx context.Context, obj client.Object) []ctrl.Request {
	return r.runnerSetsMatching(ctx, obj.GetNamespace(), func(rs *v2.RunnerSet) bool {
		return rs.Spec.GatewayRef.Name == obj.GetName()
	})
}

// proxyToRunnerSets enqueues every RunnerSet that resolves to this EgressProxy —
// either directly via proxyRef, or via the gateway's defaultProxyRef. The latter
// requires reading each set's gateway, so for simplicity (single-gateway parity)
// any unset-proxyRef set in the namespace is enqueued; the reconcile re-resolves.
func (r *RunnerSetReconciler) proxyToRunnerSets(ctx context.Context, obj client.Object) []ctrl.Request {
	return r.runnerSetsMatching(ctx, obj.GetNamespace(), func(rs *v2.RunnerSet) bool {
		if rs.Spec.ProxyRef != nil {
			return rs.Spec.ProxyRef.Name == obj.GetName()
		}
		// Unset proxyRef inherits the gateway's defaultProxyRef; re-reconcile to
		// re-resolve rather than reading the gateway here.
		return true
	})
}

// templateToRunnerSets enqueues every RunnerSet in the template's namespace that
// could resolve to this namespaced RunnerTemplate: one whose templateRef names it
// directly, or — because an unset templateRef inherits the gateway's defaultTemplateRef
// (Q172) — any set with an unset templateRef in the namespace, which the reconcile
// re-resolves (mirrors proxyToRunnerSets' generous-enqueue-then-re-resolve, simpler
// than reading each set's gateway here).
func (r *RunnerSetReconciler) templateToRunnerSets(ctx context.Context, obj client.Object) []ctrl.Request {
	return r.runnerSetsMatching(ctx, obj.GetNamespace(), func(rs *v2.RunnerSet) bool {
		if rs.Spec.TemplateRef == nil {
			return true // may inherit gateway.defaultTemplateRef pointing at this template
		}
		return rs.Spec.TemplateRef.Kind != "ClusterRunnerTemplate" && rs.Spec.TemplateRef.Name == obj.GetName()
	})
}

// clusterTemplateToRunnerSets enqueues every RunnerSet that could resolve to this
// ClusterRunnerTemplate: one whose templateRef is a ClusterRunnerTemplate naming it,
// or — because an unset templateRef may inherit the gateway's defaultTemplateRef or
// fall through to the cluster-default ClusterRunnerTemplate (Q172) — any set with an
// unset templateRef, so a set sitting TemplateNotFound/AmbiguousDefault flips the
// moment a (default-marked) cluster template syncs. The referent is cluster-scoped (no
// namespace), so it lists from the manager cache — already scoped to this AGC's
// namespace and gateway — and the reconcile re-resolves the actual rung.
func (r *RunnerSetReconciler) clusterTemplateToRunnerSets(ctx context.Context, obj client.Object) []ctrl.Request {
	var list v2.RunnerSetList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range list.Items {
		rs := &list.Items[i]
		unset := rs.Spec.TemplateRef == nil
		named := rs.Spec.TemplateRef != nil && rs.Spec.TemplateRef.Kind == "ClusterRunnerTemplate" && rs.Spec.TemplateRef.Name == obj.GetName()
		if unset || named {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rs.Namespace, Name: rs.Name}})
		}
	}
	return reqs
}

// runnerSetsMatching lists RunnerSets in ns and returns reconcile requests for
// those satisfying match.
func (r *RunnerSetReconciler) runnerSetsMatching(ctx context.Context, ns string, match func(*v2.RunnerSet) bool) []ctrl.Request {
	var list v2.RunnerSetList
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
