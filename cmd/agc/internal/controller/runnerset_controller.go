package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/token"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	// APIReader is the manager's uncached reader, used for the one read that must not
	// establish an informer: the v1alpha1 RunnerGroup probe that gates adoption of
	// pre-Q466 agent Secrets. A v2-only install may not serve v1alpha1 at all, and a
	// cached Get on an unserved kind wedges the manager's cache. Nil falls back to the
	// cached client (tests, where the CRD is always installed).
	APIReader    client.Reader
	TokenManager *token.Manager
	Registrar    agentpool.Registrar
	BrokerConfig BrokerConfig
	Metrics      *runnercore.Metrics
	// ScaleSetMetrics holds the Prometheus counters for the ScaleSet acquisition tier
	// (Q264 Option E). Nil disables scale-set observability (a Classic-only AGC needs
	// none); the classic Metrics field is unaffected either way.
	ScaleSetMetrics *scalesetlistener.Metrics
	Log             *slog.Logger
	Provisioner     *provisioner.Provisioner
	AgentKeyType    agentpool.KeyType

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

	// BaselineRecheckInterval is the cadence at which a RunnerSet is requeued while
	// its multiplexer is below the desired listener count. Zero selects
	// defaultBaselineRecheckInterval.
	BaselineRecheckInterval time.Duration

	// ProxyShareRecheckInterval is the cadence at which a RunnerSet whose egress
	// wiring turns on a projected proxy share is requeued, in either direction of the
	// grant. Zero selects defaultProxyShareRecheckInterval.
	ProxyShareRecheckInterval time.Duration

	multiplexersMu sync.Mutex
	multiplexers   map[types.NamespacedName]*listener.Multiplexer
	poolsMu        sync.Mutex
	pools          map[types.NamespacedName]*agentpool.Pool

	// scaleSetListeners holds the running scale-set acquisition listener per
	// ScaleSet-protocol RunnerSet (Q264 P3d) — one session per scale set. Classic
	// sets never appear here; the two acquisition tiers keep separate runtime state.
	scaleSetListenersMu sync.Mutex
	scaleSetListeners   map[types.NamespacedName]*scaleSetListenerHandle

	// ScaleSetClientFactory builds the scale-set protocol client for a ScaleSet-protocol
	// RunnerSet. Nil selects the production factory (buildScaleSetClient), which derives
	// the config URL/API base from the resolved gateway's githubURL and routes through
	// the per-tenant egress proxy. Tests inject a factory pointing at the scalesettest
	// fake so the wiring is exercised offline.
	ScaleSetClientFactory func(rs *v2alpha1.RunnerSet, gw *v2alpha1.ActionsGateway) (*scaleset.Client, error)

	// ScaleSetStubBaseURL re-points the scale-set bootstrap at a fake-GitHub stub,
	// for the deployed fake-GitHub e2e tier — the gateway's githubURL cannot name it,
	// being pinned to https by the CRD and the webhook. Empty in production: main()
	// sets it only from the STUB_AUTH_URL + STUB_BROKER_URL pair that also selects the
	// classic tier's StubRegistrar, which reaches a GMC-provisioned AGC only under the
	// testing-only --allow-agc-extra-env flag.
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

	// Drain listener goroutines inside the manager's graceful shutdown so SIGTERM
	// cannot kill the process mid-DELETE and leak GitHub-side sessions (Q222).
	if err := mgr.Add(&listenerShutdown{stop: r.stopListeners, log: r.Log, owner: "RunnerSet"}); err != nil {
		return fmt.Errorf("add listener shutdown runnable: %w", err)
	}

	b := ctrl.NewControllerManagedBy(mgr).
		// Bounded retry backoff so a reconcile error cannot strand the worker-pod
		// reaper's deadline (retryBackoffCap).
		WithOptions(controller.Options{RateLimiter: reconcileRateLimiter()}).
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

	// ClusterRunnerTemplate (cluster-scoped) is watched ONLY by a v2 AGC, gated on
	// GatewayName. Only a GMC-provisioned v2 AGC (which carries GATEWAY_NAME) holds
	// the per-gateway ClusterRoleBinding to agc-clusterrunnertemplate-reader; a v1
	// AGC is bound only to agc-tenant-role, which has no cluster-scoped grant.
	// Establishing this cluster-scoped informer without that grant fails RBAC and
	// aborts the shared manager's cache sync — crashing the AGC and taking the v1
	// RunnerGroup reconciler down with it. So a v1 AGC (no GATEWAY_NAME) never
	// registers it; a v2 AGC does, and a RunnerSet referencing a ClusterRunnerTemplate
	// flips Ready the moment it syncs (§H.7). The namespace-scoped manager cache
	// serves cluster-scoped kinds from a cluster-wide informer.
	//
	// Only a test harness reaches this with an empty GatewayName — main.go
	// declines to register the reconciler on an unscoped AGC (Q466). The guard
	// stays so an unscoped harness never establishes the cluster-scoped informer.
	if r.GatewayName != "" {
		b = b.Watches(
			&v2alpha1.ClusterRunnerTemplate{},
			handler.EnqueueRequestsFromMapFunc(r.clusterTemplateToRunnerSets),
		)
	}

	return b.Named("runnerset").Complete(r)
}

func (r *RunnerSetReconciler) ensureMaps() {
	if r.multiplexers == nil {
		r.multiplexers = make(map[types.NamespacedName]*listener.Multiplexer)
	}
	if r.pools == nil {
		r.pools = make(map[types.NamespacedName]*agentpool.Pool)
	}
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
	r.drainEvents(&rs)

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
		// stopping avoids the churn and reflects reality). Both tiers are stopped
		// defensively; only the one this set actually runs is present.
		r.stopMultiplexer(req.NamespacedName)
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
		if reason == v2alpha1.ReasonProxyShareNotGranted {
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
	// ask against it (Q359 Phase 2). Advisory only — set before the protocol
	// routing so both acquisition tiers persist it with their status writes.
	r.applySizingStatus(&rs, refs.template)

	// Report a Throughput profile cancelled by an admission-injected CPU limit (Q489)
	// — the one sizing conflict that rejects nothing, read off the worker pods the
	// profile built rather than inferred from any one policy object. Advisory, and a
	// no-op under every other profile. The existing worker-pod watch is what keeps it
	// current: the pod carrying the injected limit re-reconciles this set.
	r.applySizingProfileOverride(ctx, &rs, refs.template)

	// 2. Recover evicted scale-set workers (Q417). Runs BEFORE the reaper: the
	// evicted pod is the only record of which run to re-run, and the reaper deletes
	// terminal pods once completedPodTTL elapses. It is a no-op for a classic set
	// (whose pods carry no acquisition-protocol label) and for a set with nothing
	// evicted. Deliberately not waited on — recovery waits out evictionRetryDelay
	// before calling GitHub, and a reconcile must not block on either.
	if rs.Spec.AcquisitionProtocol == v2alpha1.AcquisitionProtocolScaleSet {
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
	}

	// 3. Reap expired worker pods (terminal past completedPodTTL, Pending past
	// pendingPodDeadline). Runs before the token fetch so cleanup keeps working
	// during a GitHub outage. podCounts is the pod phase snapshot used to
	// populate status.activeJobs/pendingJobs.
	var observed observedRunner
	reapAfter, podCounts, err := r.reapWorkerPods(ctx, log, &rs, &observed)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Route by acquisition protocol (Q264 P3d). A ScaleSet-protocol set is driven by a
	// single scale-set listener session per set instead of the classic pool/multiplexer
	// many-acquirers path; the field is immutable (P3a), so a set never switches tiers
	// live. Classic (deprecated; the default is ScaleSet as of P5) and an unset field
	// both fall through to the unchanged classic path below.
	if rs.Spec.AcquisitionProtocol == v2alpha1.AcquisitionProtocolScaleSet {
		res, err := r.reconcileScaleSetListener(ctx, log, &rs, refs, reapAfter, podCounts, observed)
		if err != nil {
			// controller-runtime discards a Result returned beside an error and warns
			// that the reconciler returned both; the error path already requeues with
			// backoff, so there is nothing to fold in.
			return ctrl.Result{}, err
		}
		return r.withProxyShareRecheck(res, refs), nil
	}

	// 4. Installation token for agent management. Process-wide (one GitHub App per
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

	// 5. Ensure agent pool Secrets. Carry any pre-Q466 Secrets across the rename first,
	// so an upgraded install keeps its existing agents instead of orphaning them.
	if err := r.adoptLegacyAgentSecrets(ctx, log, &rs); err != nil {
		log.Error("failed to adopt pre-existing agent Secrets", "error", err)
		return ctrl.Result{}, err
	}
	pool := r.getOrCreatePool(req.NamespacedName, &rs)
	if err := pool.EnsureAgents(ctx, rs.Spec.MaxListeners, instToken); err != nil {
		log.Error("EnsureAgents failed", "error", err)
		r.recordEvent(&rs, corev1.EventTypeWarning, "AgentPoolError", "EnsureAgents",
			"failed to provision agent Secrets: %v", err)
		// Reflect the failure in status: no listener can run without the agent Secrets, so
		// the set is Ready=False. Without this write the early return would leave a stale
		// Ready=True until the next reconcile, misreporting the set as healthy (Q308).
		r.setReadyCondition(&rs, false, v2alpha1.ReasonAgentProvisioningFailed,
			fmt.Sprintf("failed to provision agent Secrets: %v", err))
		if uerr := r.Status().Update(ctx, &rs); uerr != nil && !apierrors.IsConflict(uerr) {
			log.Error("failed to write Ready condition", "error", uerr)
		}
		return ctrl.Result{}, err
	}

	// 6. Start or update the multiplexer.
	mux := r.getOrCreateMultiplexer(ctx, req.NamespacedName, rs.DeepCopy(), pool)
	mux.SetMaxListeners(rs.Spec.MaxListeners)
	var startErr error
	if mux.ActiveCount() == 0 && rs.Spec.MaxListeners > 0 {
		if startErr = mux.Start(ctx); startErr != nil {
			log.Warn("multiplexer restart failed", "error", startErr)
			r.recordEvent(&rs, corev1.EventTypeWarning, "ListenerStartFailed", "StartMultiplexer",
				"failed to restart listener goroutines: %v", startErr)
		}
	}

	// 7. Update status.
	active := mux.ActiveCount()
	rs.Status.ActiveSessions = active
	rs.Status.ActiveJobs = podCounts.active
	rs.Status.PendingJobs = podCounts.pending
	applyObservedRunnerVersion(&rs, observed)
	rs.Status.ObservedGeneration = rs.Generation
	// A listener-start failure surfaces as Ready=False/ListenerStartFailed rather than
	// slipping through as the benign NoActiveSessions state (Q308).
	ready, reason, msg := readyConditionForListeners(active, startErr, refs.templateSource)
	r.setReadyCondition(&rs, ready, reason, msg)

	// Worker-capacity conditions (Q303, Q906): the two-tier WorkerQuota ladder,
	// WorkersUnschedulable, and WorkersNotStarting, so a stall shows as a condition
	// rather than only rising pendingJobs with Ready=True. All advisory — none gates
	// Ready. capacityRequeue folds into the requeue below and carries two deadlines:
	// the scheduling grace, after which WorkersUnschedulable can flip, and the shorter
	// startup re-check for a bound pod that has not yet declared itself either way.
	capacityRequeue := r.applyWorkerCapacityConditions(ctx, &rs, refs.template, refs.gateway)

	if err := r.Status().Update(ctx, &rs); err != nil {
		return ctrl.Result{}, err
	}

	requeueAfter := reapAfter
	if capacityRequeue > 0 && (requeueAfter <= 0 || capacityRequeue < requeueAfter) {
		requeueAfter = capacityRequeue
	}
	if rs.Spec.MaxListeners > 0 && mux.ActiveCount() < rs.Spec.MaxListeners {
		if interval := r.baselineRecheckInterval(); requeueAfter <= 0 || interval < requeueAfter {
			requeueAfter = interval
		}
	}
	// A resolved share stays resolved only until the provider withdraws it, and the
	// withdrawal deletes a ConfigMap nothing here watches. This poll is what bounds
	// how long the set keeps acquiring jobs onto egress it is no longer entitled to.
	return r.withProxyShareRecheck(ctrl.Result{RequeueAfter: requeueAfter}, refs), nil
}

func (r *RunnerSetReconciler) baselineRecheckInterval() time.Duration {
	if r.BaselineRecheckInterval > 0 {
		return r.BaselineRecheckInterval
	}
	return defaultBaselineRecheckInterval
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

// reconcileDelete stops goroutines, deletes agent Secrets, and removes the finalizer.
//
//nolint:dupl // v2 twin of RunnerGroupReconciler.reconcileDelete; folds in when v1alpha1 retires
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
	r.stopScaleSetListener(key)
	r.pendingConds.forget(key)
	r.poolsMu.Lock()
	delete(r.pools, key)
	r.poolsMu.Unlock()
	// Drop the Q249 reap-blocking-sidecar gauge series so a deleted (or foreign-gateway)
	// RunnerSet does not leave a stale value behind.
	if r.Metrics != nil {
		r.Metrics.ReapBlockingSidecarTemplates.DeleteLabelValues(key.Namespace, key.Name)
	}
	// Same for the scale-set tier's capacity gauges (Q443): a deleted set that stopped
	// polling would otherwise report its last advertised capacity forever.
	r.ScaleSetMetrics.DeleteRunnerSet(key.Namespace, key.Name)
}

func (r *RunnerSetReconciler) recordEvent(rs *v2alpha1.RunnerSet, eventtype, reason, action, note string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(rs, nil, eventtype, reason, action, note, args...)
}

// getOrCreatePool returns the Pool for the given RunnerSet, creating it if needed.
// The pool uses the RunnerSet naming scheme, which is disjoint from the RunnerGroup
// one so a same-named v1 group and v2 set can coexist through a migration; the owner
// reference is refreshed on every call so a set deleted and recreated under the same
// name cannot leave the pool stamping a dead UID (Q466).
func (r *RunnerSetReconciler) getOrCreatePool(key types.NamespacedName, rs *v2alpha1.RunnerSet) *agentpool.Pool {
	r.poolsMu.Lock()
	defer r.poolsMu.Unlock()
	p, ok := r.pools[key]
	if !ok {
		p = agentpool.NewRunnerSetPool(r.Client, rs.Namespace, rs.Name, r.BrokerConfig.RunnerVersion,
			rs.Spec.RunnerLabels, r.Registrar, r.AgentKeyType)
		if r.Metrics != nil {
			p.Metrics = r.Metrics
		}
		r.pools[key] = p
	}
	p.SetOwner(runnerSetOwnerRef(rs.Name, rs.UID))
	return p
}

// adoptLegacyAgentSecrets carries a RunnerSet's agent Secrets across the Q466 rename,
// so upgrading an install that already ran v2 neither orphans its agent Secrets nor
// leaks the GitHub runner records they hold. It is a no-op once the set has Secrets
// under the RunnerSet scheme, which is the steady state after the first reconcile.
//
// The gate is the reason this lives in the reconciler rather than in the pool: a
// legacy Secret is indistinguishable from a v1 RunnerGroup's, so adoption only
// proceeds when no RunnerGroup of the same name exists in the namespace. Taking a live
// RunnerGroup's Secrets would break the v1 tenant that the coexistence window exists to
// keep rollback-able.
func (r *RunnerSetReconciler) adoptLegacyAgentSecrets(ctx context.Context, log *slog.Logger, rs *v2alpha1.RunnerSet) error {
	n, err := agentpool.AdoptLegacyRunnerSetSecrets(ctx, r.Client, rs.Namespace, rs.Name,
		[]metav1.OwnerReference{runnerSetOwnerRef(rs.Name, rs.UID)},
		func(ctx context.Context) (bool, error) { return r.runnerGroupExists(ctx, rs.Namespace, rs.Name) },
	)
	if err != nil {
		return err
	}
	if n > 0 {
		log.Info("adopted pre-existing agent Secrets onto the runner-set naming scheme", "count", n)
	}
	return nil
}

// runnerGroupExists reports whether a v1alpha1 RunnerGroup of this name lives in the
// namespace — i.e. whether the legacy-named agent Secrets belong to a v1 tenant.
//
// The read goes through APIReader (uncached) when one is wired, because a v2-only
// install may not have the v1alpha1 CRD at all: a cached Get would try to start an
// informer for a kind the API server does not serve and wedge the manager's cache, the
// same failure mode the cluster-scoped ClusterRunnerTemplate watch is gated against.
// An absent CRD means no RunnerGroup can exist, so it answers false.
func (r *RunnerSetReconciler) runnerGroupExists(ctx context.Context, namespace, name string) (bool, error) {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var rg v1alpha1.RunnerGroup
	switch err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &rg); {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err), runtime.IsNotRegisteredError(err):
		return false, nil
	default:
		return false, err
	}
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

	condUpdater := r.condUpdater()
	brokerCfg := r.BrokerConfig
	target := r.provisionerTarget(rs)

	factory := func(index int) listener.Config {
		agent := pool.ClaimAgent()
		if agent == nil {
			return listener.Config{Group: rs.Name, Namespace: rs.Namespace}
		}
		return r.newListenerConfig(rs, target, pool, brokerCfg, condUpdater, agent)
	}

	muxLog := r.Log.With("namespace", rs.Namespace, "group", rs.Name)
	mux := listener.NewMultiplexer(factory, rs.Spec.MaxListeners, muxLog)
	// Retain a completed job's planID claim until its terminal worker pod is reaped
	// (completedPodTTL), so a late GitHub redelivery of the same planID is deduped
	// rather than colliding on the lingering Completed pod (Q260 redelivery
	// residual). Mirrors the RunnerGroup controller.
	mux.ClaimLinger = provisioner.CompletedPodTTLOrDefault(rs.Spec.CompletedPodTTL)
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
func (r *RunnerSetReconciler) newListenerConfig(rs *v2alpha1.RunnerSet, target provisioner.Target, pool *agentpool.Pool, brokerCfg BrokerConfig, condUpdater runnercore.ConditionUpdater, agent *agentpool.Agent) listener.Config {
	jobHandler := listener.JobHandlerFunc(nil)
	admit := runnercore.AdmitFunc(nil)
	if r.Provisioner != nil {
		jobHandler = r.Provisioner.Handle(target)
		admit = r.Provisioner.Admit(target)
	}
	return assembleListenerConfig(rs.Name, rs.Namespace, brokerCfg, condUpdater, r.eventRecorder(), r.Metrics, agent, r.TokenManager, jobHandler, admit, pool)
}

// drainConditions reads pending listener-pushed condition updates and merges those
// for this RunnerSet into its status; others are re-enqueued.
func (r *RunnerSetReconciler) drainConditions(rs *v2alpha1.RunnerSet) {
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
				prev := meta.FindStatusCondition(rs.Status.Conditions, upd.condition.Type)
				if runnercore.DropListenerCondition(prev, upd.condition) {
					continue // refused: neither merged nor retained
				}
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
func (r *RunnerSetReconciler) drainEvents(rs *v2alpha1.RunnerSet) {
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

// readyConditionForListeners computes the classic-path Ready condition from the number
// of running listener goroutines and any error returned when (re)starting them.
// Precedence: a running listener ⇒ Ready; otherwise a start failure ⇒
// Ready=False/ListenerStartFailed — a distinct, actionable reason rather than the benign
// NoActiveSessions state (Q308); otherwise Ready=False/NoActiveSessions. templateSource
// names the resolution rung in the healthy message (auditable in status).
func readyConditionForListeners(active int32, startErr error, templateSource string) (ready bool, reason, msg string) {
	switch {
	case active > 0:
		return true, v2alpha1.ReasonListenerActive,
			fmt.Sprintf("references resolved (template via %s); %d listener goroutine(s) running", templateSource, active)
	case startErr != nil:
		return false, v2alpha1.ReasonListenerStartFailed,
			fmt.Sprintf("listener goroutines failed to start: %v", startErr)
	default:
		return false, v2alpha1.ReasonNoActiveSessions,
			"references resolved; no listener goroutines are running"
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

// setEgressMode records how this RunnerSet's worker egress reaches GitHub (Q168,
// §H.10): proxyMode Proxied + EgressUnattributed=False when a proxy resolved, or
// proxyMode Direct + EgressUnattributed=True when none did. The condition is advisory
// (it surfaces the per-tenant-IP-attribution trade an operator opted out of by not
// attaching a proxy) and does not gate Ready — direct egress is a supported,
// NetworkPolicy-restricted mode.
func (r *RunnerSetReconciler) setEgressMode(rs *v2alpha1.RunnerSet, proxied bool) {
	status := metav1.ConditionTrue
	mode := v2alpha1.ProxyModeDirect
	reason := v2alpha1.ReasonDirectEgress
	msg := "no proxyRef/defaultProxyRef: worker egress is direct (restricted to DNS + GitHub) and has no per-tenant egress IP identity"
	if proxied {
		status = metav1.ConditionFalse
		mode = v2alpha1.ProxyModeProxied
		reason = v2alpha1.ReasonProxiedEgress
		msg = "worker egress is attributed to the resolved EgressProxy's per-tenant IPs"
	}
	rs.Status.ProxyMode = mode
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type:               v2alpha1.ConditionEgressUnattributed,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: rs.Generation,
	})
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
func (r *RunnerSetReconciler) setReapBlockingSidecarStatus(rs *v2alpha1.RunnerSet, template *v2alpha1.RunnerTemplateSpec, annotations map[string]string) {
	names := v2alpha1.ReapBlockingSidecars(template, annotations)

	if r.Metrics != nil {
		r.Metrics.ReapBlockingSidecarTemplates.WithLabelValues(rs.Namespace, rs.Name).Set(float64(len(names)))
	}

	status := metav1.ConditionFalse
	reason := v2alpha1.ReasonNoReapBlockingSidecar
	msg := "no regular reap-blocking sidecar in the resolved worker template"
	if len(names) > 0 {
		status = metav1.ConditionTrue
		reason = v2alpha1.ReasonReapBlockingSidecar
		msg = fmt.Sprintf("worker pod sidecar container(s) %s are regular containers that may keep the pod from reaping after the runner exits, stranding the runner slot; declare them as native sidecars (restartPolicy: Always init containers, Kubernetes >= 1.29) or acknowledge them in the %s annotation",
			strings.Join(names, ", "), v2alpha1.SelfExitingSidecarsAnnotation)
	}
	prev := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionPossibleReapBlockingSidecar)
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type:               v2alpha1.ConditionPossibleReapBlockingSidecar,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: rs.Generation,
	})
	// Warn once on a genuine False→True transition so the misconfiguration lands in the
	// event stream, not only in status.
	if status == metav1.ConditionTrue && (prev == nil || prev.Status != metav1.ConditionTrue) {
		r.recordEvent(rs, corev1.EventTypeWarning, reason, "Reconcile", msg)
	}
}

// setRunnerVersionStatus judges the runner version this set's effective worker image
// declares against GitHub's enforced registration minimum, and publishes the verdict
// as RunnerVersionTooOld (Q715). It asks GitHub nothing, so it reports on the ScaleSet
// tier too — where the acquisition protocol carries no runner version and the listener
// therefore never sees the rejection that produces the classic tier's condition.
//
// A nil template means the references have not resolved; the image the set would run is
// unknown, so nothing is published rather than judging the AGC-wide default the set may
// never use.
func (r *RunnerSetReconciler) setRunnerVersionStatus(rs *v2alpha1.RunnerSet, template *v2alpha1.RunnerTemplateSpec) {
	if template == nil || r.Provisioner == nil {
		return
	}
	cond := runnercore.WorkerRunnerVersionCondition(
		r.Provisioner.EffectiveWorkerImage(template.WorkerImage), rs.Generation)

	prev := meta.FindStatusCondition(rs.Status.Conditions, cond.Type)
	// The two producers of this condition report different facts through one type. A
	// session-sourced VersionTooOld is GitHub rejecting agent.version, which is the
	// AGC's own pinned names.RunnerVersion and says nothing about the worker image; a
	// healthy image reading therefore does not refute it, and writing over it would
	// drop a live rejection from status.
	if prev != nil && prev.Status == metav1.ConditionTrue && prev.Reason == v2alpha1.ReasonVersionTooOld {
		return
	}
	meta.SetStatusCondition(&rs.Status.Conditions, cond)
	// Warn once on a genuine transition into too-old so the deadline lands in the event
	// stream while there is still time to change the image.
	if cond.Status == metav1.ConditionTrue && (prev == nil || prev.Status != metav1.ConditionTrue) {
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
func (r *RunnerSetReconciler) provisionerTarget(rs *v2alpha1.RunnerSet) *runnerSetTarget {
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
func (r *RunnerSetReconciler) reapWorkerPods(ctx context.Context, log *slog.Logger, rs *v2alpha1.RunnerSet, observed *observedRunner) (time.Duration, workerPodCounts, error) {
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
// registration leak at its source (Q550). It returns nil for a set with no running
// scale-set listener — a classic set, or one whose listener has not started — because
// the scale-set client is the listener's, and there is no record to remove on the
// classic tier anyway.
//
// Best-effort by construction: a record the ephemeral runner already removed is a
// no-op, a record still running a job must be kept (RunnerBusyError), and any other
// failure is logged and left to the start-up sweep. None of them may stop the reap —
// the pod is condemned either way, and a retained pod would hold its concurrency slot.
func (r *RunnerSetReconciler) deregisterScaleSetRunner(rs *v2alpha1.RunnerSet, log *slog.Logger) func(context.Context, string) {
	ssClient := r.scaleSetClientFor(types.NamespacedName{Namespace: rs.Namespace, Name: rs.Name})
	if ssClient == nil {
		return nil
	}
	return func(ctx context.Context, runnerName string) {
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
// reconcile must not stall on GitHub — and is a no-op on a Classic-protocol set, whose
// pods carry no acquisition-protocol label.
func (r *RunnerSetReconciler) recoverAbandonedRun(rs *v2alpha1.RunnerSet) func(context.Context, *corev1.Pod) {
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

// templateToRunnerSets enqueues every RunnerSet in the template's namespace that
// could resolve to this namespaced RunnerTemplate: one whose templateRef names it
// directly, or — because an unset templateRef inherits the gateway's defaultTemplateRef
// (Q172) — any set with an unset templateRef in the namespace, which the reconcile
// re-resolves (mirrors proxyToRunnerSets' generous-enqueue-then-re-resolve, simpler
// than reading each set's gateway here).
func (r *RunnerSetReconciler) templateToRunnerSets(ctx context.Context, obj client.Object) []ctrl.Request {
	return r.runnerSetsMatching(ctx, obj.GetNamespace(), func(rs *v2alpha1.RunnerSet) bool {
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
	var list v2alpha1.RunnerSetList
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
