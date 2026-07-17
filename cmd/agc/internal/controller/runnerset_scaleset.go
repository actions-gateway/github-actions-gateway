package controller

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

// The ScaleSet acquisition tier (Q264 Option E, P3d). A RunnerSet with
// spec.acquisitionProtocol == ScaleSet is driven by one scalesetlistener.Listener —
// a single message-queue session per scale set that provisions one worker pod per
// assigned job (scalesetlistener.Listener docs) — instead of the classic
// pool/multiplexer many-acquirers path. This file holds the reconciler branch, the
// per-set listener lifecycle, and the scaleset.Client factory; the classic path is
// byte-for-byte unchanged, but is no longer the default — the field defaults to
// ScaleSet as of Q264 P5, and Classic is a deprecated explicit opt-in.

// defaultScaleSetMaxCapacity is the X-ScaleSetMaxCapacity a ScaleSet set advertises
// when it declares neither maxWorkers nor priorityTiers — the total number of jobs
// GitHub may keep assigned-but-unfinished for the set at once. It matches the
// maxListeners default (10) so an operator who sets nothing gets the same concurrency
// ceiling the classic path would have given them. Declaring maxWorkers/priorityTiers
// overrides it (the plan's "concurrency governed by maxWorkers/priorityTiers"): the
// advertised capacity is then the tier ceiling, so GitHub caps totalAssignedJobs there
// and the provisioner backstops per pod (§3 Reworked).
const defaultScaleSetMaxCapacity = 10

// scaleSetStatusSink adapts the reconciler's condition/event channels to the
// scalesetlistener's owner-bound sinks (Q325): the RunnerSet identity is closed over
// here, mirroring how ScaleSetMetrics.RecorderFor binds the metrics recorder. The
// underlying channel sends are non-blocking (channelConditionUpdater /
// channelEventRecorder drop on a full channel), as the listener requires.
type scaleSetStatusSink struct {
	key  types.NamespacedName
	cond *channelConditionUpdater
	ev   *channelEventRecorder
}

func (s *scaleSetStatusSink) SetCondition(cond metav1.Condition) {
	s.cond.SetCondition(s.key.Namespace, s.key.Name, cond)
}

func (s *scaleSetStatusSink) Event(eventtype, reason, action, note string) {
	s.ev.Event(s.key.Namespace, s.key.Name, eventtype, reason, action, note)
}

// scaleSetListenerHandle owns a running scale-set listener and the means to stop it:
// cancel tears down the poll loop (which deletes the session on the way out), and done
// closes when the loop has fully exited so teardown can wait for it (no goroutine leak).
type scaleSetListenerHandle struct {
	listener *scalesetlistener.Listener
	cancel   context.CancelFunc
	done     <-chan struct{}
}

// reconcileScaleSetListener drives a ScaleSet-protocol RunnerSet: it ensures exactly
// one listener session is running for the set (idempotent across reconciles — one
// session per scale set, §2.2) and publishes its accounting on status. It is entered
// only after references have resolved and worker pods have been reaped, so it receives
// the reap result and pod counts to fold into the requeue and status like the classic
// path does. It never touches the classic pool/multiplexer.
func (r *RunnerSetReconciler) reconcileScaleSetListener(ctx context.Context, log *slog.Logger, rs *v2alpha1.RunnerSet, refs *resolvedRefs, reapAfter time.Duration, podCounts workerPodCounts) (ctrl.Result, error) {
	key := types.NamespacedName{Namespace: rs.Namespace, Name: rs.Name}

	handle, err := r.ensureScaleSetListener(ctx, log, key, rs, refs)
	if err != nil {
		// The session could not be opened (auth/registration error). Stop acquiring,
		// surface it, and requeue — the referent watches or the next resync retry it.
		r.stopScaleSetListener(key)
		r.recordEvent(rs, corev1.EventTypeWarning, "ScaleSetListenerStartFailed", "StartScaleSetListener",
			"failed to start scale-set listener: %v", err)
		r.setReadyCondition(rs, false, v2alpha1.ReasonNoActiveSessions,
			fmt.Sprintf("scale-set listener failed to start: %v", err))
		rs.Status.ActiveSessions = 0
		rs.Status.ActiveJobs = podCounts.active
		rs.Status.PendingJobs = podCounts.pending
		rs.Status.ObservedGeneration = rs.Generation
		if uerr := r.Status().Update(ctx, rs); uerr != nil && !apierrors.IsConflict(uerr) {
			return ctrl.Result{}, uerr
		}
		return ctrl.Result{}, err
	}

	// The listener is running: one session per scale set. Publish its accounting.
	st := handle.listener.Status()
	rs.Status.ActiveSessions = 1
	rs.Status.ActiveJobs = podCounts.active
	rs.Status.PendingJobs = podCounts.pending
	rs.Status.ObservedGeneration = rs.Generation
	r.setReadyCondition(rs, true, v2alpha1.ReasonListenerActive,
		fmt.Sprintf("references resolved (template via %s); scale-set listener active (scaleSetID %d, %d job(s) assigned)",
			refs.templateSource, st.ScaleSetID, st.AssignedJobs))

	// Worker-capacity conditions (Q303), identical to the classic path: the ScaleSet
	// tier provisions the same worker pods (one per assigned job), so a namespace-quota
	// or scheduling stall must surface here too rather than hiding behind rising
	// pendingJobs with Ready=True.
	unschedRequeue := r.applyWorkerCapacityConditions(ctx, rs, refs.template)

	if err := r.Status().Update(ctx, rs); err != nil && !apierrors.IsConflict(err) {
		return ctrl.Result{}, err
	}
	requeueAfter := reapAfter
	if unschedRequeue > 0 && (requeueAfter <= 0 || unschedRequeue < requeueAfter) {
		requeueAfter = unschedRequeue
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// ensureScaleSetListener returns the running listener for key, starting one if none is
// running. Starting opens the session synchronously (so an auth/registration failure
// surfaces here rather than in a background goroutine); the poll loop then runs under a
// context derived from the manager-scoped reconcile ctx, so it lives until the set is
// deleted (stopScaleSetListener) or the manager shuts down (ctx cancel) — mirroring the
// classic multiplexer's lifecycle.
func (r *RunnerSetReconciler) ensureScaleSetListener(ctx context.Context, log *slog.Logger, key types.NamespacedName, rs *v2alpha1.RunnerSet, refs *resolvedRefs) (*scaleSetListenerHandle, error) {
	r.scaleSetListenersMu.Lock()
	defer r.scaleSetListenersMu.Unlock()
	if h, ok := r.scaleSetListeners[key]; ok {
		return h, nil
	}

	client, err := r.buildScaleSetClient(rs, refs.gateway)
	if err != nil {
		return nil, fmt.Errorf("build scale-set client: %w", err)
	}

	// The Target seam own-refs the real RunnerSet and re-resolves its template/proxy
	// per job — identical to the classic path, so worker pods GC and egress-proxy
	// wiring carry over unchanged (the App token never reaches the worker; §4 security).
	target := &runnerSetTarget{
		client: r.Client,
		prov:   r.Provisioner,
		key:    key,
		uid:    rs.UID,
		events: &channelEventRecorder{ch: r.eventCh},
	}

	// The scale set's single runs-on label is its name (CEL guarantees exactly one
	// runnerLabel for a ScaleSet set).
	scaleSetName := ""
	if len(rs.Spec.RunnerLabels) > 0 {
		scaleSetName = rs.Spec.RunnerLabels[0]
	}

	// Owner-bound sink for the listener's session-failure conditions and events
	// (Q325); the reconciler drains both channels on its next reconcile.
	sink := &scaleSetStatusSink{
		key:  key,
		cond: &channelConditionUpdater{ch: r.conditionCh},
		ev:   &channelEventRecorder{ch: r.eventCh},
	}

	l, err := scalesetlistener.New(scalesetlistener.Config{
		Client:       client,
		ScaleSetName: scaleSetName,
		OwnerName:    key.Namespace + "/" + key.Name,
		Provision: func(ctx context.Context, job scalesetlistener.Job) error {
			return r.Provisioner.ProvisionScaleSetWorker(ctx, target, job.JobID, job.JITConfig)
		},
		Capacity: r.scaleSetCapacityFunc(target),
		// Per-RunnerSet Prometheus recorder over the scale-set tier's counters
		// (Q264 P4 observability). Nil ScaleSetMetrics yields a nil recorder, which
		// the listener treats as metrics-disabled.
		Metrics:    r.ScaleSetMetrics.RecorderFor(key.Namespace, key.Name),
		Conditions: sink,
		Events:     sink,
		Log:        log,
	})
	if err != nil {
		return nil, err
	}

	// Derive the listener's context from the manager-scoped reconcile ctx (which is
	// cancelled on manager shutdown), so a stopped manager tears the loop down; the
	// stored cancel stops it on RunnerSet delete without waiting for shutdown.
	lctx, cancel := context.WithCancel(ctx)
	done, err := l.Start(lctx)
	if err != nil {
		cancel()
		return nil, err
	}
	h := &scaleSetListenerHandle{listener: l, cancel: cancel, done: done}
	r.scaleSetListeners[key] = h
	return h, nil
}

// scaleSetCapacityFunc returns the CapacityFunc advertised as X-ScaleSetMaxCapacity:
// the set's total worker ceiling (the max tier threshold, else maxWorkers, else the
// default). GitHub keeps totalAssignedJobs at or below this value, so it is the
// scale-set expression of the Q59 admission gate — concurrency governed by
// maxWorkers/priorityTiers, not maxListeners (§3 Reworked). The provisioner's own
// ceilingCheck backstops per pod, so a stale read never over-provisions.
func (r *RunnerSetReconciler) scaleSetCapacityFunc(target *runnerSetTarget) scalesetlistener.CapacityFunc {
	return func(ctx context.Context) int {
		ceiling, bounded := target.Ceiling(ctx)
		if !bounded {
			return defaultScaleSetMaxCapacity
		}
		return int(ceiling)
	}
}

// buildScaleSetClient builds the per-RunnerSet scale-set protocol client. Tests inject
// ScaleSetClientFactory to point it at the scalesettest fake; production derives the
// config URL and API base from the resolved gateway's githubURL and lets the client's
// defaults clone the proxy-patched http.DefaultTransport (so its Actions Service
// traffic routes through the per-tenant egress proxy like the classic path). It is
// built lazily here — well after main() patches http.DefaultTransport — so the default
// transport already carries the proxy CA (the proxy-client init-order rule).
func (r *RunnerSetReconciler) buildScaleSetClient(rs *v2alpha1.RunnerSet, gw *v2alpha1.ActionsGateway) (*scaleset.Client, error) {
	if r.ScaleSetClientFactory != nil {
		return r.ScaleSetClientFactory(rs, gw)
	}
	return scaleset.New(scaleset.Config{
		TokenProvider: r.TokenManager,
		ConfigURL:     gw.Spec.GitHubURL,
		APIBase:       scaleSetAPIBase(gw.Spec.GitHubURL),
	})
}

// scaleSetAPIBase returns the REST API base for a gateway's githubURL: empty (the
// client's public-GitHub default) for github.com, or the GHES "/api/v3" base for a
// GitHub Enterprise Server host. GHES also needs the JobAvailable→acquire path, which
// the client already handles by the one rule (§5a-U8).
func scaleSetAPIBase(githubURL string) string {
	u, err := url.Parse(githubURL)
	if err != nil || u.Host == "" {
		return ""
	}
	switch u.Host {
	case "github.com", "www.github.com", "api.github.com":
		return ""
	default:
		return u.Scheme + "://" + u.Host + "/api/v3"
	}
}

// stopScaleSetListener cancels and drops the scale-set listener for key, if present,
// waiting for its loop to exit (which deletes the session) so a re-created listener
// replays the queue to a fresh session rather than colliding on a live one. Idempotent.
func (r *RunnerSetReconciler) stopScaleSetListener(key types.NamespacedName) {
	r.scaleSetListenersMu.Lock()
	h, ok := r.scaleSetListeners[key]
	if ok {
		delete(r.scaleSetListeners, key)
	}
	r.scaleSetListenersMu.Unlock()
	if !ok {
		return
	}
	h.cancel()
	<-h.done
}
