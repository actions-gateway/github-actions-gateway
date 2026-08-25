package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/githubapp"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// scaleSetSessionRetry is how long the reconciler waits before re-attempting a listener
// start that found the scale set's single session still held by a predecessor. Sized
// against what it is waiting for — the outgoing listener's bounded exit teardown — and
// deliberately flat: the wait ends at a moment nothing here can observe, so a ladder
// would keep growing past it and leave the set idle with the session already free.
const scaleSetSessionRetry = 2 * time.Second

// scaleSetSessionHeld reports whether err is the 409 CreateSession answers while another
// session exists for the scale set. One session per scale set is a protocol invariant,
// so an overlapping AGC — every rolling update — meets this on the way up.
func scaleSetSessionHeld(err error) bool {
	var conflict *scaleset.SessionConflictError
	return errors.As(err, &conflict)
}

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
	// client is the set's scale-set protocol client, kept so the reaper can deregister
	// a reaped worker's runner record through it (Q550) — the reaper runs outside the
	// listener and has no other route to GitHub.
	client *scaleset.Client
	cancel context.CancelFunc
	done   <-chan struct{}
	// runnerGroup is the GitHub runner group this listener registered its scale set
	// into, kept so a later reconcile can tell that the declared group changed. The
	// group is read once, at registration, so without this a live listener holds the
	// old boundary until the AGC restarts (Q712).
	runnerGroup string
	// capacity carries the most recent advertisement from the listener's poll loop to
	// the reconcile that publishes it on status (Q721). It hangs off the handle so it
	// is freed with the listener it belongs to, by the delete this map already does on
	// stop; a map of its own would need its own teardown and could outlive the set.
	capacity *capacityRecord
}

// capacityRecord is the one-value handoff from a listener's poll loop, which computes
// the capacity advertisement, to the reconcile goroutine, which writes it to status.
// The two run concurrently and at different cadences, so the value is guarded and only
// ever the latest one is kept: status reports the set's current intake, not a history.
//
// The zero value is ready to use and reports ok=false until the first poll, which is
// what keeps status.advertisedCapacity nil rather than a fabricated zero on a listener
// that has started but not yet polled.
type capacityRecord struct {
	mu   sync.Mutex
	adv  provisioner.CapacityAdvertisement
	seen bool
}

// record stores the advertisement this poll produced, replacing any earlier one.
func (c *capacityRecord) record(adv provisioner.CapacityAdvertisement) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.adv, c.seen = adv, true
}

// last returns the most recent advertisement and whether one has been made at all. A
// nil receiver reports ok=false, so a caller holding a handle from before this field
// existed (or a test building one by hand) reads "not yet advertised" rather than
// panicking.
func (c *capacityRecord) last() (provisioner.CapacityAdvertisement, bool) {
	if c == nil {
		return provisioner.CapacityAdvertisement{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.adv, c.seen
}

// scaleSetClientFor returns the scale-set client of the listener running for key, or
// nil when none is running. The reaper runs before the listener is ensured on any given
// reconcile, so a nil here is ordinary on the first pass for a set — the records a reap
// could not deregister then are collected by the next listener start's sweep.
func (r *RunnerSetReconciler) scaleSetClientFor(key types.NamespacedName) *scaleset.Client {
	r.scaleSetListenersMu.Lock()
	defer r.scaleSetListenersMu.Unlock()
	if h, ok := r.scaleSetListeners[key]; ok {
		return h.client
	}
	return nil
}

// scaleSetListenerRunnerGroup returns the runner group the listener running for key
// registered its scale set into, and whether a listener is running at all. The empty
// string is a meaningful value (GitHub's default group), hence the second result.
func (r *RunnerSetReconciler) scaleSetListenerRunnerGroup(key types.NamespacedName) (string, bool) {
	r.scaleSetListenersMu.Lock()
	defer r.scaleSetListenersMu.Unlock()
	if h, ok := r.scaleSetListeners[key]; ok {
		return h.runnerGroup, true
	}
	return "", false
}

// reconcileScaleSetListener drives a ScaleSet-protocol RunnerSet: it ensures exactly
// one listener session is running for the set (idempotent across reconciles — one
// session per scale set, §2.2) and publishes its accounting on status. It is entered
// only after references have resolved and worker pods have been reaped, so it receives
// the reap result and pod counts to fold into the requeue and status like the classic
// path does. It never touches the classic pool/multiplexer.
func (r *RunnerSetReconciler) reconcileScaleSetListener(ctx context.Context, log *slog.Logger, rs *v2alpha1.RunnerSet, refs *resolvedRefs, reapAfter time.Duration, podCounts workerPodCounts, observed observedRunner) (ctrl.Result, error) {
	key := types.NamespacedName{Namespace: rs.Namespace, Name: rs.Name}

	handle, err := r.ensureScaleSetListener(ctx, log, key, rs, refs)
	if scaleSetSessionHeld(err) {
		// A predecessor still holds the scale set's single session — the ordinary shape
		// of a rollout, where the outgoing AGC is finishing the teardown Q222 bounds
		// (reading outstanding conclusions, deleting their messages, then the session).
		// It is a wait, not a fault: reported as an error it would take the work queue's
		// exponential backoff, whose ceiling is far longer than the teardown it is
		// waiting on, so one collision idles the set long after the session is free.
		//
		// Status is left alone deliberately: the set does have an active session, the
		// predecessor's, so publishing NoActiveSessions here would flap Ready=False
		// through every rollout for a condition that resolves itself.
		r.stopScaleSetListener(key)
		log.Info("scale-set session still held by a predecessor; retrying shortly", "err", err)
		return ctrl.Result{RequeueAfter: scaleSetSessionRetry}, nil
	}
	if err != nil {
		// The session could not be opened (auth/registration error). Stop acquiring,
		// surface it, and requeue — the referent watches or the next resync retry it.
		//
		// A declared runner group that GitHub does not have gets its own reason: it is
		// operator misconfiguration of an isolation boundary, not a credential or
		// registration fault, and the set stays unregistered until it is fixed (Q712).
		reason := v2alpha1.ReasonNoActiveSessions
		if errors.Is(err, scalesetlistener.ErrRunnerGroupNotFound) {
			reason = v2alpha1.ReasonRunnerGroupNotFound
		}
		r.stopScaleSetListener(key)
		r.recordEvent(rs, corev1.EventTypeWarning, "ScaleSetListenerStartFailed", "StartScaleSetListener",
			"failed to start scale-set listener: %v", err)
		r.setReadyCondition(rs, false, reason,
			fmt.Sprintf("scale-set listener failed to start: %v", err))
		rs.Status.ActiveSessions = 0
		rs.Status.ActiveJobs = podCounts.active
		rs.Status.PendingJobs = podCounts.pending
		clearAdvertisedCapacity(rs)
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
	applyAdvertisedCapacity(rs, handle.capacity)
	applyObservedRunnerVersion(rs, observed)
	rs.Status.ObservedGeneration = rs.Generation
	r.setReadyCondition(rs, true, v2alpha1.ReasonListenerActive,
		fmt.Sprintf("references resolved (template via %s); scale-set listener active (scaleSetID %d, %d job(s) assigned)",
			refs.templateSource, st.ScaleSetID, st.AssignedJobs))

	r.applyRunnerLabelsCondition(rs, st.RegisteredLabels)

	// Worker-capacity conditions (Q303), identical to the classic path: the ScaleSet
	// tier provisions the same worker pods (one per assigned job), so a namespace-quota
	// or scheduling stall must surface here too rather than hiding behind rising
	// pendingJobs with Ready=True.
	unschedRequeue := r.applyWorkerCapacityConditions(ctx, rs, refs.template, refs.gateway)

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
	runnerGroup := resolveRunnerGroupName(rs, refs.gateway)

	// The scale set's group is settled at registration, so re-registering it under a
	// newly declared group means a fresh listener. Stopping the stale one here rather
	// than waiting for the next AGC rollout is what makes narrowing the boundary take
	// effect when the operator asks for it (Q712). Outside the lock: the stop waits for
	// the poll loop to exit.
	if group, ok := r.scaleSetListenerRunnerGroup(key); ok && group != runnerGroup {
		log.Info("scale-set runner group changed; restarting the listener",
			"from", group, "to", runnerGroup)
		r.stopScaleSetListener(key)
	}

	r.scaleSetListenersMu.Lock()
	defer r.scaleSetListenersMu.Unlock()
	if h, ok := r.scaleSetListeners[key]; ok {
		return h, nil
	}

	ssClient, err := r.buildScaleSetClient(rs, refs.gateway)
	if err != nil {
		return nil, fmt.Errorf("build scale-set client: %w", err)
	}

	// The Target seam own-refs the real RunnerSet and re-resolves its template/proxy
	// per job — identical to the classic path, so worker pods GC and egress-proxy
	// wiring carry over unchanged (the App token never reaches the worker; §4 security).
	target := r.provisionerTarget(rs)

	// The first runs-on label names the scale set; the rest are registered on it as
	// additional match targets (Q726).
	scaleSetName := scaleSetNameOf(rs)
	var extraLabels []string
	if len(rs.Spec.RunnerLabels) > 1 {
		extraLabels = rs.Spec.RunnerLabels[1:]
	}

	// Owner-bound sink for the listener's session-failure conditions and events
	// (Q325); the reconciler drains both channels on its next reconcile.
	sink := &scaleSetStatusSink{
		key:  key,
		cond: r.condUpdater(),
		ev:   r.eventRecorder(),
	}

	// Allocated before the listener so its Capacity closure can record into it from
	// the first poll, and stored on the handle below so the reconcile can read it.
	capacity := &capacityRecord{}

	l, err := scalesetlistener.New(scalesetlistener.Config{
		Client:       ssClient,
		ScaleSetName: scaleSetName,
		ExtraLabels:  extraLabels,
		OwnerName:    key.Namespace + "/" + key.Name,
		// The GitHub-side authorization boundary: which repositories may target this
		// set's runners (Q712).
		RunnerGroupName: runnerGroup,
		// Asked before each assigned job's JIT config is minted, so a job the ceiling
		// will reject registers no runner at GitHub (Q576). The ceiling verdict is
		// translated into the listener's own vocabulary; every other error travels
		// as-is, and the listener treats it as "provision anyway".
		CheckCapacity: func(ctx context.Context) error {
			err := r.Provisioner.CheckScaleSetCapacity(ctx, target)
			if provisioner.IsCeilingReached(err) {
				return fmt.Errorf("%w: %w", scalesetlistener.ErrCapacityUnavailable, err)
			}
			return err
		},
		Provision: func(ctx context.Context, job scalesetlistener.Job) error {
			err := r.Provisioner.ProvisionScaleSetWorker(ctx, target, provisioner.ScaleSetJob{
				JobID:     job.JobID,
				JITConfig: job.JITConfig,
				// The runner record is pre-registered before the pod exists and
				// outlives it, so the name goes onto the pod for the reaper to
				// deregister and the sweep to recognize as claimed (Q550).
				RunnerName: job.RunnerName,
				// The assignment message is the only place this tier learns which
				// workflow run a job belongs to, and the listener does not retain it —
				// so it goes onto the worker pod, where eviction recovery reads it back
				// after the pod is gone (Q417).
				Owner:      job.Owner,
				Repository: job.Repository,
				RunID:      job.RunID,
				JobName:    job.JobName,
			})
			// The authoritative ceiling check, losing the race the pre-check above
			// could not close: same translation, so the job is re-offered rather than
			// redelivered.
			if provisioner.IsCeilingReached(err) {
				return fmt.Errorf("%w: %w", scalesetlistener.ErrCapacityUnavailable, err)
			}
			return err
		},
		Cleanup: func(ctx context.Context, jobID string) error {
			return r.Provisioner.CleanupScaleSetJob(ctx, target, jobID)
		},
		// Per-RunnerSet ConfigMap persisting the concluded-job guards, so a hard-killed
		// AGC does not replay an assignment it concluded but had not yet deleted (Q606).
		Guards:   r.scaleSetGuardStore(rs),
		Capacity: r.scaleSetCapacityFunc(key, target, capacity),
		// Per-RunnerSet Prometheus recorder over the scale-set tier's counters
		// (Q264 P4 observability). Nil ScaleSetMetrics yields a nil recorder, which
		// the listener treats as metrics-disabled.
		Metrics: r.ScaleSetMetrics.RecorderFor(key.Namespace, key.Name),
		// The shared cross-tier poll-error counter, which is the classic tier's series
		// rather than this tier's own: a namespace-bound recorder over
		// actions_gateway_message_poll_errors_total, so poll health stays rate-able on
		// the same query after classic is removed (Q446).
		PollErrors: r.Metrics.PollErrors(key.Namespace),
		Conditions: sink,
		Events:     sink,
		// The sweep's safety check: which runner records a live worker pod is relying
		// on, so a record whose worker has not started yet — offline, and otherwise
		// indistinguishable from a stale one — is never collected (Q550).
		ClaimedRunnerNames: func(ctx context.Context) (map[string]struct{}, error) {
			return r.claimedRunnerNames(ctx, key)
		},
		Log: log,
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
	h := &scaleSetListenerHandle{
		listener: l, client: ssClient, cancel: cancel, done: done, runnerGroup: runnerGroup,
		capacity: capacity,
	}
	r.scaleSetListeners[key] = h
	return h, nil
}

// resolveRunnerGroupName returns the GitHub runner group a set's scale set belongs in:
// the set's own spec.runnerGroup, else its gateway's defaultRunnerGroup, else empty
// (GitHub's default group). Mirrors the templateRef/proxyRef inheritance chain (§H.4)
// so a tenant declares its GitHub-side boundary once, on the gateway.
func resolveRunnerGroupName(rs *v2alpha1.RunnerSet, gw *v2alpha1.ActionsGateway) string {
	if rs.Spec.RunnerGroup != "" {
		return rs.Spec.RunnerGroup
	}
	if gw != nil {
		return gw.Spec.DefaultRunnerGroup
	}
	return ""
}

// claimedRunnerNames returns the runner names stamped on this set's worker pods that
// are not already gone — the records the start-up sweep must leave alone (Q550).
//
// A pod in ANY phase claims its record, including Pending: that is the whole point, as
// a worker still waiting to be scheduled has an offline record that looks exactly like
// a stale one. Only a pod already being deleted releases its claim, since the reaper is
// deregistering it anyway.
func (r *RunnerSetReconciler) claimedRunnerNames(ctx context.Context, key types.NamespacedName) (map[string]struct{}, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(key.Namespace),
		client.MatchingLabels{provisioner.LabelRunnerSet: key.Name},
	); err != nil {
		return nil, fmt.Errorf("list worker pods: %w", err)
	}
	claimed := make(map[string]struct{}, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}
		if name := pod.Annotations[provisioner.AnnotationRunnerName]; name != "" {
			claimed[name] = struct{}{}
		}
	}
	return claimed, nil
}

// scaleSetCapacityFunc returns the CapacityFunc advertised as X-ScaleSetMaxCapacity —
// this tier's whole admission ladder, expressed as one integer per poll. GitHub keeps
// totalAssignedJobs at or below the advertised value, so a slot this function withholds
// is a job that is never assigned: the same outcome the classic tier's Admit produces by
// declining to claim, without spending a JIT runner record or a job lock to get there.
//
// provisioner.AdvertiseCapacity owns the rungs and their composition (the declared
// ceiling, then live namespace-ResourceQuota headroom, then placeability, then the
// opt-in scale-up rate limit), so the two tiers cannot drift apart the way they did
// while the quota rung was classic-only (Q443). This function
// only supplies the no-ceiling default and publishes the resulting accounting.
//
// The provisioner's own ceilingCheck still backstops per pod, so a stale read never
// over-provisions.
func (r *RunnerSetReconciler) scaleSetCapacityFunc(key types.NamespacedName, target *runnerSetTarget, rec *capacityRecord) scalesetlistener.CapacityFunc {
	advertise := r.Provisioner.AdvertiseCapacity(target, defaultScaleSetMaxCapacity)
	return func(ctx context.Context) int {
		adv := advertise(ctx)
		r.ScaleSetMetrics.SetAdvertisedCapacity(key.Namespace, key.Name, adv.Total)
		for reason, slots := range adv.Withheld {
			r.ScaleSetMetrics.SetCapacityWithheld(key.Namespace, key.Name, reason, slots)
		}
		// The same accounting on status, for a tenant who cannot read Prometheus (Q721).
		// Recorded here rather than published here: status belongs to the reconcile
		// goroutine, and a poll that wrote it directly would race every other status
		// write on the object.
		rec.record(adv)
		return int(adv.Total)
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
	// APIBase is stated rather than left empty for the public case: the client's own
	// default is the same value, and passing it explicitly keeps one resolver
	// answering "which GitHub API?" across the AGC and the GMC (Q506). GHES also
	// needs the JobAvailable→acquire path, which the client already handles by the
	// one rule (§5a-U8).
	//
	// The fake-GitHub re-point rewrites only the CONFIG URL, and the API base is then
	// derived from the result — so the stub path goes through that same resolver
	// rather than carrying a second copy of the GHES rule.
	configURL := gw.Spec.GitHubURL
	if stubURL, ok := scaleSetStubConfigURL(r.ScaleSetStubBaseURL, configURL); ok {
		configURL = stubURL
	}
	return scaleset.New(scaleset.Config{
		TokenProvider: r.TokenManager,
		ConfigURL:     configURL,
		APIBase:       githubapp.DeriveAPIBaseURL(configURL),
	})
}

// scaleSetStubConfigURL re-points a gateway's config URL at a fake-GitHub stub,
// keeping the org (or owner/repo) path — the client derives the runners REST prefix
// from it and rejects a pathless one outright, so dropping the path would wedge the
// bootstrap rather than merely misaddress it.
//
// It applies only to a gateway whose githubURL ALREADY names the stub's host:port,
// which is the whole scope of the rewrite — swapping that gateway's https scheme for
// the plaintext one the stub actually serves, since the CRD pattern and the webhook
// forbid writing http into the field. A gateway naming any other host is left alone,
// so pointing one at an unreachable host still fails to bootstrap on a cluster where
// the stub env is set. `E2E_AGC_ScaleSetRecovery` depends on exactly that: its
// subject is the recovery scan running on a set whose listener is NOT up, and a
// blanket rewrite silently started that listener and broke it.
func scaleSetStubConfigURL(stubBase, githubURL string) (string, bool) {
	if stubBase == "" {
		return "", false
	}
	stub, err := url.Parse(stubBase)
	if err != nil || stub.Host == "" {
		return "", false
	}
	gw, err := url.Parse(githubURL)
	if err != nil || gw.Host != stub.Host {
		return "", false
	}
	return stub.Scheme + "://" + stub.Host + gw.Path, true
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

// applyRunnerLabelsCondition publishes RunnerLabelsIncomplete: whether the scale set at
// GitHub carries every label the RunnerSet declares (Q726). registered is what the
// server reported when the listener ensured the set.
//
// It is compared on every reconcile rather than once at listener start, because the two
// ways the sets diverge arrive at different times. A GHES appliance below 3.21 drops the
// labels past the first at creation; an operator appending a label diverges later, and
// the listener does not restart for a spec change — the scale set is reused by name, so
// nothing re-registers and the new label never reaches GitHub. Re-deriving the verdict
// here catches the second the reconcile after the edit.
//
// Reporting is the whole remedy because there is no other one: a labels PATCH is
// accepted and discarded (Q793), so the divergence cannot be corrected in place at any
// point in the listener's life.
//
// Advisory: it never gates Ready. The set still serves every job targeting the labels
// that did register, and a job targeting one that did not simply stays queued at GitHub,
// which is a configuration mismatch for the operator to fix rather than an outage.
//
// A listener that has not ensured its scale set yet reports nil, which is not the same
// claim as "the server returned no labels" and must not be published as a shortfall —
// the condition is left untouched until there is an observation to publish.
func (r *RunnerSetReconciler) applyRunnerLabelsCondition(rs *v2alpha1.RunnerSet, registered []string) {
	if registered == nil {
		return
	}
	have := make(map[string]bool, len(registered))
	for _, l := range registered {
		have[l] = true
	}
	var missing []string
	for _, want := range rs.Spec.RunnerLabels {
		if !have[want] {
			missing = append(missing, want)
		}
	}

	status, reason, msg := metav1.ConditionFalse, v2alpha1.ReasonLabelsRegistered,
		fmt.Sprintf("scale set %q carries every declared runnerLabel", scaleSetNameOf(rs))
	if len(missing) > 0 {
		status, reason = metav1.ConditionTrue, v2alpha1.ReasonLabelsNotRegistered
		msg = fmt.Sprintf(
			"scale set %q does not carry runnerLabel(s) %v (registered: %v); jobs whose runs-on names them stay queued at GitHub. "+
				"Labels are fixed when the scale set is created and cannot be changed afterwards (the Actions Service accepts a "+
				"labels PATCH and discards it, measured 2026-08-24), and on GitHub Enterprise Server "+
				"below 3.21 the appliance drops all but the name label unless a site admin enables "+
				"DistributedTask.AllowRunnerScaleSetCustomLabels. See docs/operations/troubleshooting.md",
			scaleSetNameOf(rs), missing, registered)
	}

	was := meta.IsStatusConditionTrue(rs.Status.Conditions, v2alpha1.ConditionRunnerLabelsIncomplete)
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type:               v2alpha1.ConditionRunnerLabelsIncomplete,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: rs.Generation,
	})
	if len(missing) > 0 && !was {
		r.recordEvent(rs, corev1.EventTypeWarning, "RunnerLabelsNotRegistered", "EnsureScaleSet", "%s", msg)
	}
}

// scaleSetNameOf is the scale set's name at GitHub: the RunnerSet's first runnerLabel.
func scaleSetNameOf(rs *v2alpha1.RunnerSet) string {
	if len(rs.Spec.RunnerLabels) == 0 {
		return ""
	}
	return rs.Spec.RunnerLabels[0]
}

// applyAdvertisedCapacity publishes the listener's most recent capacity advertisement
// on status, so a tenant can read why intake is throttled without Prometheus (Q721).
//
// Before the first poll it leaves both fields at their zero values rather than writing
// an advertisement of 0, which would read as "intake fully withheld" — the opposite of
// "not yet advertised", and the reason advertisedCapacity is a pointer.
//
// The reasons are sorted so the rendered list is stable across reconciles: the
// advertisement carries them in a map, and an unsorted write would reorder the list on
// every reconcile, producing a status update (and a watch event) whenever Go's map
// iteration order changed rather than when the capacity did.
func applyAdvertisedCapacity(rs *v2alpha1.RunnerSet, rec *capacityRecord) {
	adv, ok := rec.last()
	if !ok {
		return
	}
	total := adv.Total
	rs.Status.AdvertisedCapacity = &total

	reasons := make([]string, 0, len(adv.Withheld))
	for reason := range adv.Withheld {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)

	withheld := make([]v2alpha1.WithheldCapacity, 0, len(reasons))
	for _, reason := range reasons {
		withheld = append(withheld, v2alpha1.WithheldCapacity{Reason: reason, Slots: adv.Withheld[reason]})
	}
	rs.Status.WithheldCapacity = withheld
}

// clearAdvertisedCapacity drops the published advertisement, for the paths that stop
// (or never start) a listener: an unresolved reference, a failed listener start, and
// gateway teardown. It is the capacity twin of zeroing activeSessions there — a set
// with no session advertises nothing, and leaving the last advertisement behind would
// report withheld slots against intake that no longer exists.
func clearAdvertisedCapacity(rs *v2alpha1.RunnerSet) {
	rs.Status.AdvertisedCapacity = nil
	rs.Status.WithheldCapacity = nil
}
