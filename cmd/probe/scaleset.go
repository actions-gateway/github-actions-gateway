// Investigation E (Q264): the runner-scale-set message-queue protocol.
//
// Set PROBE_SCALESET_TEST=true to run this scenario instead of the classic
// broker probe. It exercises the scale-set wire protocol end-to-end against
// real GitHub with only GitHub App credentials — no pre-registered agent, no
// broker URL — settling the Q264 protocol unknowns (auth chain, queue/message
// semantics, rate-limit headers):
//
//	installation token
//	  → POST {api}/orgs/{org}/actions/runners/registration-token
//	  → POST {api}/actions/runner-registration        (RemoteAuth hop)
//	  → GET  {svc}/_apis/runtime/runnergroups/?groupName=Default
//	  → POST {svc}/_apis/runtime/runnerscalesets      (throwaway scale set)
//	  → POST {svc}/_apis/runtime/runnerscalesets/{id}/sessions
//	  → GET  {messageQueueUrl}&lastMessageId=0        (one long-poll)
//	  → POST {svc}/_apis/runtime/runnerscalesets/{id}/acquirejobs  ([] and bogus id)
//	  → POST {svc}/_apis/runtime/runnerscalesets/{id}/generatejitconfig
//	  → cleanup: DELETE session, DELETE scale set
//
// # What drives the wire, and what the probe still asserts on its own
//
// Every call above is issued by the shipping scaleset package (Q362): the probe
// drives scaleset.Client, so a live run is evidence about the code GAG actually
// ships, and a divergence between the library and the wire — the single bug
// class this scenario exists to catch — cannot hide behind a probe-local
// dialect of the protocol.
//
// Three things stay deliberately outside the library, because delegating them
// would make the probe agree with itself instead of with GitHub:
//
//  1. Raw-wire reporting. wireLog observes every response through the client's
//     ResponseObserver hook and logs the status, the rate-limit/X-* headers, and
//     the long-poll latency the typed API collapses (a 202 becomes (nil, nil), a
//     404 becomes a typed error). The finding is what the wire did, not what the
//     client made of it.
//  2. The acquire route/token matrix (probeAcquireJobs). The library speaks one
//     construction; the probe measures it against the alternatives — the same
//     route under the admin JWT, ARC's acquirablejobs, and the queue-host route
//     family — so "the shipped route 404s but another one answers" is visible.
//  3. The delivered acquireJobUrl fallback (jobTest). The client always targets
//     the static _apis/runtime acquire route; a JobAvailable may carry its own
//     acquireJobUrl. The probe tries the client's construction first and, when it
//     fails where the delivered URL succeeds, logs that divergence explicitly.
//
// Required environment variables:
//
//	GITHUB_APP_ID              - GitHub App numeric ID
//	GITHUB_APP_PRIVATE_KEY     - Path to PEM file, or PEM literal
//	GITHUB_APP_INSTALLATION_ID - Installation ID for the target org
//	GITHUB_ORG_URL             - Org or repo URL (e.g. https://github.com/my-org)
//
// Optional:
//
//	PROBE_SCALESET_JOB_TEST    - "true" to keep polling the queue for a
//	                             JobAvailable (dispatch a workflow with
//	                             runs-on: <scale set name> while it waits) and
//	                             acquire it, observing JobAssigned. This is also
//	                             the Q417 verification run: each JobAssigned is
//	                             reported with an explicit verdict on whether it
//	                             carried the run identity scale-set eviction
//	                             recovery depends on (reportRunIdentity). Grep the
//	                             output for "run identity present" or "GAP".
//	PROBE_SCALESET_NAME        - Scale set name (default gag-probe-scaleset).
//	PROBE_SCALESET_CLEANUP     - "true" to skip the scenario and instead LIST every
//	                             scale set registered against the scope, then delete
//	                             the one named by PROBE_SCALESET_NAME. The listing is
//	                             the only way to see a scale set whose name nobody
//	                             recorded, which is the state every orphan is in
//	                             (Q344).
//	PROBE_SCALESET_PRUNE_PREFIX - With CLEANUP, also delete every scale set whose name
//	                             starts with this. The orphan sweep: an orphan's name
//	                             is the thing the operator does not have. Which listed
//	                             sets are orphans is the operator's call — nothing here
//	                             can see the cluster's RunnerSets.
//	PROBE_SCALESET_DRY_RUN     - With CLEANUP, report what would be deleted and delete
//	                             nothing. The listing runs either way.
//
// Everything the probe creates is deleted on exit. Tokens and the JIT config
// blob are never logged — only their lengths / top-level JSON keys.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/actions-gateway/github-actions-gateway/githubapp"
	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
)

// scalesetConfig holds the parsed environment for the scale-set scenario.
// It is deliberately smaller than probeConfig: the scale-set protocol needs no
// pre-registered agent, broker URL, or runner version to bootstrap.
type scalesetConfig struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPEM  []byte
	ConfigURL      string // org or repo URL the scale set registers against
	ScaleSetName   string
	GroupName      string
	JobTest        bool
	// CapacityTest (Investigation E2) polls the queue with
	// X-ScaleSetMaxCapacity 0 → 1 → 2 against pre-queued jobs to observe
	// whether assignment is capacity-gated and whether JobAvailable/an
	// explicit acquire step appears above capacity; then probes session
	// token refresh (PATCH) and delete/recreate message replay.
	CapacityTest bool
	// JITConfigFiles, when non-empty, are paths the probe writes freshly
	// minted encodedJITConfig blobs to (0600, one runner each) so a real
	// runner (docker run …/actions-runner run.sh --jitconfig) can register
	// against the probe's scale set while HoldSeconds keeps it alive.
	JITConfigFiles []string
	// HoldSeconds delays cleanup so externally started runners can register,
	// receive a job, and run under the probe's scale set.
	HoldSeconds int
	// Cleanup makes the probe only look up the scale set by name and delete
	// it — the recovery path for a scale set leaked by an interrupted run
	// (observed live: the admin JWT expired during a long hold, 401-ing the
	// deferred deletes). It first lists every scale set in the scope, which is
	// the only way to see one whose name nobody recorded (Q344).
	Cleanup bool
	// PrunePrefix, when set, extends Cleanup to delete every scale set whose
	// name starts with it, not just the one named exactly. This is the orphan
	// sweep: an orphan's name is the thing the operator does not have.
	PrunePrefix string
	// DryRun reports what Cleanup would delete and deletes nothing. The listing
	// is unconditional, so a bare Cleanup is already a safe way to look.
	DryRun bool
}

// parseScalesetConfig reads and validates the scale-set scenario environment
// from the injected getenv function (normally os.Getenv).
func parseScalesetConfig(getenv func(string) string) (scalesetConfig, error) {
	var cfg scalesetConfig

	appIDStr, err := mustEnv(getenv, "GITHUB_APP_ID")
	if err != nil {
		return scalesetConfig{}, err
	}
	if _, err := fmt.Sscan(appIDStr, &cfg.AppID); err != nil {
		return scalesetConfig{}, fmt.Errorf("parse GITHUB_APP_ID: %w", err)
	}
	installIDStr, err := mustEnv(getenv, "GITHUB_APP_INSTALLATION_ID")
	if err != nil {
		return scalesetConfig{}, err
	}
	if _, err := fmt.Sscan(installIDStr, &cfg.InstallationID); err != nil {
		return scalesetConfig{}, fmt.Errorf("parse GITHUB_APP_INSTALLATION_ID: %w", err)
	}
	pemValue, err := mustEnv(getenv, "GITHUB_APP_PRIVATE_KEY")
	if err != nil {
		return scalesetConfig{}, err
	}
	cfg.PrivateKeyPEM, err = loadPEM(pemValue)
	if err != nil {
		return scalesetConfig{}, fmt.Errorf("load GITHUB_APP_PRIVATE_KEY: %w", err)
	}
	cfg.ConfigURL, err = mustEnv(getenv, "GITHUB_ORG_URL")
	if err != nil {
		return scalesetConfig{}, err
	}
	cfg.ScaleSetName = getenv("PROBE_SCALESET_NAME")
	if cfg.ScaleSetName == "" {
		cfg.ScaleSetName = "gag-probe-scaleset"
	}
	// The runner group must admit the target repo — for a PUBLIC repo the
	// group needs allows_public_repositories=true, which Default denies by
	// default. Point this at a group that admits the repo for the job test.
	cfg.GroupName = getenv("PROBE_SCALESET_GROUP_NAME")
	if cfg.GroupName == "" {
		cfg.GroupName = "Default"
	}
	cfg.JobTest = getenv("PROBE_SCALESET_JOB_TEST") == "true"
	cfg.CapacityTest = getenv("PROBE_SCALESET_CAPACITY_TEST") == "true"
	if v := getenv("PROBE_SCALESET_JITCONFIG_FILES"); v != "" {
		for _, f := range strings.Split(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				cfg.JITConfigFiles = append(cfg.JITConfigFiles, f)
			}
		}
	}
	if v := getenv("PROBE_SCALESET_HOLD_SECONDS"); v != "" {
		if _, err := fmt.Sscan(v, &cfg.HoldSeconds); err != nil {
			return scalesetConfig{}, fmt.Errorf("parse PROBE_SCALESET_HOLD_SECONDS: %w", err)
		}
	}
	cfg.Cleanup = getenv("PROBE_SCALESET_CLEANUP") == "true"
	cfg.PrunePrefix = getenv("PROBE_SCALESET_PRUNE_PREFIX")
	cfg.DryRun = getenv("PROBE_SCALESET_DRY_RUN") == "true"
	return cfg, nil
}

// wireLog is the probe's scaleset.ResponseObserver. It turns every response the
// client receives into one log line carrying the status, latency, body size, and
// the diagnostic response headers.
//
// This is the probe's own evidence, and the reason driving the shipping client
// costs the scenario nothing: the typed API answers "what did the client make of
// the response", while these lines answer "what did GitHub actually send" —
// including the 202s GetMessage reports as (nil, nil) and the rate-limit headers
// (U4) no typed return carries.
// tag names the investigation the line belongs to. Four scenarios share this
// observer, so a hardcoded letter attributes every one of their wire lines to
// whichever scenario happened to write the helper.
type wireLog struct {
	log *slog.Logger
	tag string
}

// ObserveResponse implements scaleset.ResponseObserver.
func (w wireLog) ObserveResponse(info scaleset.ResponseInfo) {
	w.log.Info("INVESTIGATION-"+w.tag+": wire",
		"op", info.Op,
		"method", info.Method,
		"host", info.Host,
		"path", info.Path,
		"status", info.Status,
		"elapsed", info.Elapsed.Round(time.Millisecond).String(),
		"bodyLen", info.BodyLen,
		"headers", fmt.Sprintf("%v", diagnosticHeaders(info.Header)))
}

// diagnosticHeaders selects the response headers a protocol investigation cares
// about — every X-* extension header, the rate-limit family, and Retry-After —
// which together are the U4 rate-limit evidence.
func diagnosticHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-") || strings.Contains(lk, "ratelimit") || lk == "retry-after" {
			out[k] = strings.Join(v, ",")
		}
	}
	return out
}

// scalesetProbe carries the dependencies of the scale-set scenario. The client
// and its HTTP clients are injectable so the whole flow is unit-testable against
// an httptest stub, mirroring runProbe's structure.
type scalesetProbe struct {
	log *slog.Logger
	cfg scalesetConfig

	// client is the shipping protocol client — the thing under investigation.
	client *scaleset.Client
	// hc serves the handful of raw requests aimed at routes the client
	// deliberately does not speak (the queue-host acquire family, a delivered
	// acquireJobUrl). Every modelled call goes through client instead.
	hc *http.Client
	// jobTestTimeout bounds the optional live-job test's wait for a
	// JobAvailable (default 3 minutes; injectable for tests).
	jobTestTimeout time.Duration
}

// sessionOwnerName identifies the probe's message-queue session server-side.
const sessionOwnerName = "gag-probe"

// newScalesetProbe builds the scenario around a scaleset.Client wired to the
// probe's wire logger. apiBase is the REST API root (https://api.github.com in
// production; an httptest URL under test); hc and pollClient are the client's
// short-call and long-poll HTTP clients, and may be nil to take the library's
// egress-proxy-aware defaults.
func newScalesetProbe(logger *slog.Logger, cfg scalesetConfig, provider githubapp.TokenProvider,
	apiBase string, hc, pollClient *http.Client) (*scalesetProbe, error) {
	client, err := scaleset.New(scaleset.Config{
		TokenProvider: provider,
		ConfigURL:     cfg.ConfigURL,
		APIBase:       apiBase,
		HTTPClient:    hc,
		PollClient:    pollClient,
		Observer:      wireLog{log: logger, tag: "E"},
	})
	if err != nil {
		return nil, err
	}
	if hc == nil {
		hc = httpx.NewClient()
	}
	return &scalesetProbe{log: logger, cfg: cfg, client: client, hc: hc}, nil
}

// runScalesetProbe is the Investigation E entry point wired from run().
func runScalesetProbe(ctx context.Context, logger *slog.Logger, cfg scalesetConfig, provider githubapp.TokenProvider, apiBase string) error {
	p, err := newScalesetProbe(logger, cfg, provider, apiBase, nil, nil)
	if err != nil {
		return err
	}
	return p.run(ctx)
}

// cleanupOnly reports every scale set registered against the scope, then deletes the
// ones this run is asked to prune — recovery for a scale set leaked by an interrupted
// or 401-ed run.
//
// The listing comes first and is unconditional, because a leaked scale set outlives
// the cluster that made it and an operator chasing one usually cannot name it: a
// deleted ActionsGateway, a renamed runnerLabels[0], or an interrupted probe each
// strand a scale set that no by-name lookup reaches (Q344). Deciding which of the
// listed sets is an orphan is the operator's call — nothing here can see the cluster's
// RunnerSets — so the sweep is opt-in by prefix rather than automatic.
//
// Destructive against real GitHub. PROBE_SCALESET_DRY_RUN=true reports and deletes
// nothing.
func (p *scalesetProbe) cleanupOnly(ctx context.Context) error {
	all, err := p.client.ListRunnerScaleSets(ctx)
	if err != nil {
		return fmt.Errorf("list scale sets: %w", err)
	}
	p.log.Info("INVESTIGATION-E: cleanup — scale sets registered against this scope",
		"count", len(all))
	for _, ss := range all {
		p.log.Info("INVESTIGATION-E: cleanup — registered scale set",
			"id", ss.ID, "name", ss.Name, "runnerGroupId", ss.RunnerGroupID,
			"labels", labelNames(ss.Labels))
	}

	targets := make([]scaleset.RunnerScaleSet, 0, len(all))
	for _, ss := range all {
		if ss.Name == p.cfg.ScaleSetName ||
			(p.cfg.PrunePrefix != "" && strings.HasPrefix(ss.Name, p.cfg.PrunePrefix)) {
			targets = append(targets, ss)
		}
	}
	if len(targets) == 0 {
		p.log.Info("INVESTIGATION-E: cleanup — nothing matched",
			"name", p.cfg.ScaleSetName, "prunePrefix", p.cfg.PrunePrefix)
		return nil
	}
	for _, ss := range targets {
		if p.cfg.DryRun {
			p.log.Info("INVESTIGATION-E: cleanup — WOULD DELETE (dry run)",
				"id", ss.ID, "name", ss.Name)
			continue
		}
		if err := p.client.DeleteRunnerScaleSet(ctx, ss.ID); err != nil {
			return fmt.Errorf("delete scale set %d (%q): %w", ss.ID, ss.Name, err)
		}
		p.log.Info("INVESTIGATION-E: cleanup — scale set deleted", "id", ss.ID, "name", ss.Name)
	}
	return nil
}

func (p *scalesetProbe) run(ctx context.Context) error {
	// ── 1–3. Bootstrap: installation token → registration token → admin JWT ──
	// The client owns the two-hop chain and the admin JWT's lifecycle; Connect
	// forces it now so an auth failure surfaces here rather than mid-scenario.
	// The admin JWT is re-minted lazily before expiry, so the deferred cleanup
	// below survives a long hold without the probe re-minting anything itself
	// (the failure the standalone implementation had to work around).
	if err := p.client.Connect(ctx); err != nil {
		return err
	}
	p.log.Info("INVESTIGATION-E: admin connection established")

	if p.cfg.Cleanup {
		return p.cleanupOnly(ctx)
	}

	// ── 4. Resolve runner group (fall back to the default group id 1) ────────
	groupID := p.resolveRunnerGroup(ctx)

	// ── 5. Create the throwaway scale set ────────────────────────────────────
	ss, err := p.client.CreateRunnerScaleSet(ctx, scaleset.RunnerScaleSet{
		Name:          p.cfg.ScaleSetName,
		RunnerGroupID: groupID,
		Labels:        []scaleset.Label{{Name: p.cfg.ScaleSetName, Type: "System"}},
		RunnerSetting: &scaleset.RunnerSetting{Ephemeral: true},
	})
	if err != nil {
		return fmt.Errorf("create scale set: %w", err)
	}
	p.log.Info("INVESTIGATION-E: scale set created",
		"id", ss.ID, "name", ss.Name, "runnerGroupId", ss.RunnerGroupID)
	defer func() {
		dCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if delErr := p.client.DeleteRunnerScaleSet(dCtx, ss.ID); delErr != nil {
			p.log.Error("INVESTIGATION-E: delete scale set failed", "error", delErr)
		} else {
			p.log.Info("INVESTIGATION-E: scale set deleted", "id", ss.ID)
		}
	}()

	// ── 6. Create the message session ────────────────────────────────────────
	sess, err := p.createSession(ctx, ss.ID)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	defer func() {
		dCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if delErr := p.client.DeleteSession(dCtx, ss.ID, sess.SessionID); delErr != nil {
			p.log.Error("INVESTIGATION-E: delete session failed", "error", delErr)
		} else {
			p.log.Info("INVESTIGATION-E: session deleted", "sessionId", sess.SessionID)
		}
	}()

	if p.cfg.CapacityTest {
		// ── Investigation E2: capacity gating / refresh / replay ────────────
		p.capacityTest(ctx, ss.ID, sess)
	} else {
		// ── 7. One queue long-poll (U2: 202 semantics; U4: headers) ─────────
		p.pollQueueOnce(ctx, sess)

		// ── 8. acquirejobs shape probes (U2: partial-batch semantics) ───────
		p.probeAcquireJobs(ctx, ss.ID, sess)

		// ── 9. generatejitconfig ────────────────────────────────────────────
		if err := p.probeJITConfig(ctx, ss.ID); err != nil {
			p.log.Warn("INVESTIGATION-E: generatejitconfig failed", "error", err)
		}
	}

	// ── 10. Mint JIT configs for externally run runners ──────────────────────
	for i, path := range p.cfg.JITConfigFiles {
		if err := p.mintJITConfigToFile(ctx, ss.ID, i+1, path); err != nil {
			p.log.Warn("INVESTIGATION-E: mint JIT config failed", "index", i+1, "error", err)
		}
	}

	// ── 11. Optional live-job test ───────────────────────────────────────────
	if p.cfg.JobTest {
		p.jobTest(ctx, ss.ID, sess)
	}

	// ── 12. Optional hold: keep the scale set alive for external runners, ────
	//        observing the lifecycle messages they generate.
	if p.cfg.HoldSeconds > 0 {
		p.holdAndObserve(ctx, sess)
	}

	p.log.Info("INVESTIGATION-E: scenario complete; cleaning up")
	return nil
}

// resolveRunnerGroup looks up the configured runner group id, falling back to
// 1 (GitHub's default-group id) when the lookup errors or matches nothing — the
// fallback keeps the probe productive while wireLog still records the endpoint's
// real behaviour.
func (p *scalesetProbe) resolveRunnerGroup(ctx context.Context) int {
	id, ok, err := p.client.ResolveRunnerGroup(ctx, p.cfg.GroupName)
	if err != nil {
		p.log.Warn("INVESTIGATION-E: runnergroups lookup failed; falling back to group id 1", "error", err)
		return 1
	}
	if !ok {
		p.log.Info("INVESTIGATION-E: no runner group matched; falling back to group id 1",
			"name", p.cfg.GroupName)
		return 1
	}
	p.log.Info("INVESTIGATION-E: resolved runner group", "id", id, "name", p.cfg.GroupName)
	return id
}

// createSession opens the message-queue session and logs the shape of the queue
// URL it hands back — host, path, and query PARAMETER NAMES only, because the
// query values carry a signature and the response body carries the queue token.
func (p *scalesetProbe) createSession(ctx context.Context, scaleSetID int) (*scaleset.RunnerScaleSetSession, error) {
	sess, err := p.client.CreateSession(ctx, scaleSetID, sessionOwnerName)
	if err != nil {
		return nil, err
	}
	queueHost := sess.MessageQueueURL
	queuePath := ""
	queueParams := ""
	if qu, qerr := url.Parse(sess.MessageQueueURL); qerr == nil {
		queueHost = qu.Host
		queuePath = qu.Path
		var keys []string
		for k := range qu.Query() {
			keys = append(keys, k)
		}
		queueParams = strings.Join(keys, ",")
	}
	p.log.Info("INVESTIGATION-E: session created",
		"sessionId", sess.SessionID,
		"ownerName", sess.OwnerName,
		"queueHost", queueHost,
		"queuePath", queuePath,
		"queueParamNames", queueParams,
		"queueTokenLen", len(sess.MessageQueueAccessToken),
		"statistics", statsString(sess.Statistics))
	return sess, nil
}

// statsString renders a statistics snapshot for a log line, tolerating nil (the
// backend omits statistics on some responses).
func statsString(s *scaleset.RunnerScaleSetStatistic) string {
	if s == nil {
		return "<none>"
	}
	return fmt.Sprintf("%+v", *s)
}

// logQueueMessage emits one summarising log line for a queue message.
func (p *scalesetProbe) logQueueMessage(tag string, capacity int, msg *scaleset.RunnerScaleSetMessage) {
	p.log.Info("INVESTIGATION-E2: "+tag,
		"capacity", capacity,
		"messageId", msg.MessageID,
		"messageType", msg.MessageType,
		"statistics", statsString(msg.Statistics),
		"body", githubapp.SanitizeBody([]byte(msg.Body), 1024))
}

// pollQueueOnce issues a single message-queue long-poll. wireLog records the
// status, latency, and rate-limit headers (the U2/U4 evidence); this logs the
// decoded message when one arrives. A poll error is non-fatal — the wire line is
// the finding either way.
func (p *scalesetProbe) pollQueueOnce(ctx context.Context, sess *scaleset.RunnerScaleSetSession) {
	msg, err := p.client.GetMessage(ctx, sess, 1, 0)
	if err != nil {
		p.log.Warn("INVESTIGATION-E: queue poll returned error", "error", err)
		return
	}
	if msg == nil {
		p.log.Info("INVESTIGATION-E: queue long-poll returned no message")
		return
	}
	p.log.Info("INVESTIGATION-E: queue message",
		"messageId", msg.MessageID, "messageType", msg.MessageType,
		"statistics", statsString(msg.Statistics),
		"body", githubapp.SanitizeBody([]byte(msg.Body), 512))
}

// capacityTest is Investigation E2: with jobs pre-queued on the scale set's
// label, poll at capacity 0, 1, then 2 to observe whether assignment is
// strictly capacity-gated and whether a JobAvailable/acquire flow appears for
// jobs above the advertised capacity; then exercise the session token refresh
// (PATCH) and the delete/recreate replay behaviour. sess is updated in place
// when the session is refreshed or recreated so the caller's deferred cleanup
// deletes the live session.
func (p *scalesetProbe) capacityTest(ctx context.Context, scaleSetID int, sess *scaleset.RunnerScaleSetSession) {
	var cursor int64

	// Phases 1–3 — capacity 0, then 1, then 2 against the same queue: is
	// assignment strictly gated by the advertised capacity, and does the held
	// job follow once the gate widens? The cursor is not advanced past phase 3
	// — the replay test below polls from 0.
	for _, capacity := range []int{0, 1, 2} {
		msg, err := p.client.GetMessage(ctx, sess, capacity, cursor)
		switch {
		case err != nil:
			p.log.Warn("INVESTIGATION-E2: poll error", "capacity", capacity, "error", err)
		case msg == nil:
			p.log.Info("INVESTIGATION-E2: poll returned no message", "capacity", capacity)
		default:
			if capacity < 2 {
				cursor = msg.MessageID
			}
			p.logQueueMessage("poll delivered", capacity, msg)
		}
	}

	// Phase 4 — session token refresh (PATCH). RefreshSession mutates sess in
	// place, so capture what it replaced to report whether the token rotated.
	priorToken := sess.MessageQueueAccessToken
	priorSessionID := sess.SessionID
	if err := p.client.RefreshSession(ctx, scaleSetID, sess); err != nil {
		p.log.Warn("INVESTIGATION-E2: session refresh (PATCH) failed", "error", err)
	} else {
		p.log.Info("INVESTIGATION-E2: session refresh (PATCH)",
			"sameSessionId", sess.SessionID == priorSessionID,
			"tokenChanged", sess.MessageQueueAccessToken != priorToken,
			"newTokenLen", len(sess.MessageQueueAccessToken))
	}

	// Phase 5 — delete + recreate the session; does the message state replay?
	if err := p.client.DeleteSession(ctx, scaleSetID, sess.SessionID); err != nil {
		p.log.Warn("INVESTIGATION-E2: session delete for replay test failed", "error", err)
		return
	}
	newSess, err := p.createSession(ctx, scaleSetID)
	if err != nil {
		p.log.Warn("INVESTIGATION-E2: session recreate for replay test failed", "error", err)
		return
	}
	*sess = *newSess // the caller's deferred cleanup must delete the live session
	msg, err := p.client.GetMessage(ctx, sess, 2, 0)
	switch {
	case err != nil:
		p.log.Warn("INVESTIGATION-E2: replay poll error", "error", err)
	case msg == nil:
		p.log.Info("INVESTIGATION-E2: replay poll returned no message (no replay)")
	default:
		p.logQueueMessage("replay poll delivered (state replayed to fresh session)", 2, msg)
	}
}

// mintJITConfigToFile mints one JIT runner config and writes the encoded blob
// to path (0600) so an external runner can register with it. The blob is
// runner credentials — it is written to the file and never logged.
func (p *scalesetProbe) mintJITConfigToFile(ctx context.Context, scaleSetID, index int, path string) error {
	out, err := p.client.GenerateJITConfig(ctx, scaleSetID,
		fmt.Sprintf("%s-runner-%d", p.cfg.ScaleSetName, index), "_work")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(out.EncodedJITConfig), 0o600); err != nil {
		return fmt.Errorf("write JIT config: %w", err)
	}
	p.log.Info("INVESTIGATION-E2: JIT config minted to file",
		"runnerId", out.Runner.ID, "runnerName", out.Runner.Name, "path", path)
	return nil
}

// holdAndObserve keeps the scale set alive for HoldSeconds, polling the queue
// (capacity 2) and logging every message — the JobStarted/JobCompleted
// lifecycle generated by externally run runners.
func (p *scalesetProbe) holdAndObserve(ctx context.Context, sess *scaleset.RunnerScaleSetSession) {
	p.log.Info("INVESTIGATION-E2: holding scale set alive for external runners",
		"holdSeconds", p.cfg.HoldSeconds)
	deadline, cancel := context.WithTimeout(ctx, time.Duration(p.cfg.HoldSeconds)*time.Second)
	defer cancel()
	var cursor int64
	for {
		if deadline.Err() != nil {
			p.log.Info("INVESTIGATION-E2: hold window over")
			return
		}
		msg, err := p.client.GetMessage(deadline, sess, 2, cursor)
		if err != nil {
			if deadline.Err() != nil {
				p.log.Info("INVESTIGATION-E2: hold window over")
				return
			}
			p.log.Warn("INVESTIGATION-E2: hold poll error", "error", err)
			return
		}
		if msg == nil {
			p.log.Debug("INVESTIGATION-E2: hold poll empty")
			continue
		}
		cursor = msg.MessageID
		p.logQueueMessage("hold observed message", 2, msg)
	}
}

// probeAcquireJobs measures the acquire construction the shipping client uses
// against the alternatives it does not implement (U2 partial-batch semantics,
// and the route/token question the broker-host backend raised in Q264 §2a-3).
//
// This is where the probe deliberately does NOT just call the library. Cases 1–2
// are Client.AcquireJobs — the construction GAG ships, exercised for real. Cases
// 3–6 are the same operation built differently: the same route under the admin
// JWT, ARC's GET acquirablejobs, and the queue-host route family the observed
// messageQueueUrl belongs to. If the shipped route ever stops answering while an
// alternative does, that divergence shows up here as a status difference rather
// than as a silent production failure.
//
// Neither call can affect a real job: no job has been dispatched to this
// throwaway scale set.
func (p *scalesetProbe) probeAcquireJobs(ctx context.Context, scaleSetID int, sess *scaleset.RunnerScaleSetSession) {
	// The library's own construction, on an empty batch and on an id that was
	// never offered.
	for _, tc := range []struct {
		label string
		ids   []int64
	}{
		{"empty batch, client construction", nil},
		{"unknown id, client construction", []int64{9999999999}},
	} {
		won, err := p.client.AcquireJobs(ctx, scaleSetID, sess, tc.ids)
		if err != nil {
			p.log.Warn("INVESTIGATION-E: acquirejobs (client) failed", "case", tc.label, "error", err)
			continue
		}
		p.log.Info("INVESTIGATION-E: acquirejobs shape probe",
			"case", tc.label, "requested", fmt.Sprintf("%v", tc.ids), "won", fmt.Sprintf("%v", won))
	}

	ssPath := fmt.Sprintf("/_apis/runtime/runnerscalesets/%d", scaleSetID)
	unknownBatch, _ := json.Marshal([]int64{9999999999})

	// Alternatives on the Actions Service base, authorized with the admin JWT
	// rather than the queue token — the token half of the matrix (§2.5).
	for _, tc := range []struct {
		label  string
		method string
		path   string
		body   []byte
	}{
		{"unknown id, admin token", http.MethodPost, ssPath + "/acquirejobs", unknownBatch},
		{"acquirablejobs, admin token", http.MethodGet, ssPath + "/acquirablejobs", nil},
	} {
		status, body, err := p.client.RawServiceCall(ctx, tc.method, tc.path, tc.body)
		if err != nil {
			p.log.Warn("INVESTIGATION-E: acquirejobs request failed", "case", tc.label, "error", err)
			continue
		}
		p.log.Info("INVESTIGATION-E: acquirejobs shape probe",
			"case", tc.label, "status", status, "body", githubapp.SanitizeBody(body, 256))
	}

	// The observed queue URL is {broker}/scalesets/message — a route family
	// outside /_apis/runtime. Probe the acquire verb there too, with the queue
	// token that authorizes the queue.
	qBase := queueBase(sess.MessageQueueURL)
	for _, tc := range []struct {
		label string
		url   string
	}{
		{"unknown id, queue-base route", qBase + "/acquirejobs"},
		{"unknown id, queue-base route + api-version", qBase + "/acquirejobs?api-version=6.0-preview"},
	} {
		status, body, err := p.queueCall(ctx, http.MethodPost, tc.url, sess.MessageQueueAccessToken, unknownBatch)
		if err != nil {
			p.log.Warn("INVESTIGATION-E: acquirejobs request failed", "case", tc.label, "error", err)
			continue
		}
		p.log.Info("INVESTIGATION-E: acquirejobs shape probe",
			"case", tc.label, "status", status, "body", githubapp.SanitizeBody(body, 256))
	}
}

// queueCall issues one raw request to a message-queue-family URL with the
// session's queue token, returning the status and body.
//
// These are the only hand-built requests left in the scenario, and they are
// hand-built by definition: they target routes the client deliberately does not
// speak, which is the entire reason for comparing them against it.
func (p *scalesetProbe) queueCall(ctx context.Context, method, u, queueToken string, body []byte) (int, []byte, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+queueToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody, nil
}

// queueBase strips the trailing path segment and query from a message-queue
// URL, yielding the queue route family's base (e.g. …/scalesets/message →
// …/scalesets).
func queueBase(queueURL string) string {
	u, err := url.Parse(queueURL)
	if err != nil {
		return queueURL
	}
	u.RawQuery = ""
	if i := strings.LastIndex(u.Path, "/"); i > 0 {
		u.Path = u.Path[:i]
	}
	return u.String()
}

// probeJITConfig mints one JIT runner config and logs its shape — runner
// id/name and the decoded blob's top-level JSON keys only. The blob bundles
// runner credentials and is never logged.
//
// Decoding the blob is an assertion the library cannot make for the probe: the
// client returns it opaque, so its internal structure is only ever checked here.
func (p *scalesetProbe) probeJITConfig(ctx context.Context, scaleSetID int) error {
	out, err := p.client.GenerateJITConfig(ctx, scaleSetID, p.cfg.ScaleSetName+"-runner", "_work")
	if err != nil {
		return err
	}
	keys := "decode-failed"
	if decoded, decErr := base64.StdEncoding.DecodeString(out.EncodedJITConfig); decErr == nil {
		var blob map[string]json.RawMessage
		if jsonErr := json.Unmarshal(decoded, &blob); jsonErr == nil {
			var ks []string
			for k := range blob {
				ks = append(ks, k)
			}
			keys = strings.Join(ks, ",")
		}
	}
	p.log.Info("INVESTIGATION-E: generatejitconfig",
		"runnerId", out.Runner.ID,
		"runnerName", out.Runner.Name,
		"encodedLen", len(out.EncodedJITConfig),
		"decodedTopLevelKeys", keys)
	return nil
}

// jobTest waits up to 3 minutes for a JobAvailable on the queue (dispatch a
// workflow with runs-on: <scale set name> while it waits), acquires it as a
// batch of one, and reports whether a JobAssigned follows — the live half of
// U2. The acquired job is left to GitHub's unstarted-job timeout (no runner
// will run it); acceptable on the test org, mirroring the M1 probes.
func (p *scalesetProbe) jobTest(ctx context.Context, scaleSetID int, sess *scaleset.RunnerScaleSetSession) {
	p.log.Info("INVESTIGATION-E: job test — dispatch a workflow with runs-on: " + p.cfg.ScaleSetName + " NOW")
	timeout := p.jobTestTimeout
	if timeout == 0 {
		timeout = 3 * time.Minute
	}
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	lastMessageID := int64(0)
	for {
		if deadline.Err() != nil {
			p.log.Warn("INVESTIGATION-E: job test timeout — no JobAvailable within the test window")
			return
		}
		msg, err := p.client.GetMessage(deadline, sess, 1, lastMessageID)
		if err != nil {
			if deadline.Err() != nil {
				continue
			}
			p.log.Error("INVESTIGATION-E: job test poll failed", "error", err)
			return
		}
		if msg == nil {
			p.log.Debug("INVESTIGATION-E: job test poll empty")
			continue
		}
		lastMessageID = msg.MessageID
		p.log.Info("INVESTIGATION-E: job test message",
			"messageId", msg.MessageID, "messageType", msg.MessageType,
			"statistics", statsString(msg.Statistics),
			"body", githubapp.SanitizeBody([]byte(msg.Body), 1024))

		jobs, err := msg.Jobs()
		if err != nil {
			p.log.Warn("INVESTIGATION-E: job test message decode failed", "error", err)
			continue
		}
		if assigned := scaleset.AssignedJobs(jobs); len(assigned) > 0 {
			for _, a := range assigned {
				p.log.Info("INVESTIGATION-E: JobAssigned observed — batch acquire confirmed",
					"runnerRequestId", a.RunnerRequestID, "jobId", a.JobID)
				p.reportRunIdentity(a)
			}
			return
		}
		ids := scaleset.AvailableJobIDs(jobs)
		if len(ids) == 0 {
			continue
		}
		acquireURL := deliveredAcquireURL(jobs)
		if au, aerr := url.Parse(acquireURL); acquireURL != "" && aerr == nil {
			// Host and path only: a delivered URL may carry a signed query.
			p.log.Info("INVESTIGATION-E: JobAvailable carries acquireJobUrl",
				"host", au.Host, "path", au.Path)
		}
		p.acquireOffered(deadline, scaleSetID, sess, ids, acquireURL)
	}
}

// reportRunIdentity states, as a finding rather than as something to be read out of
// a body dump, whether the live assignment carried the workflow-run identity that
// scale-set eviction recovery depends on (Q417).
//
// Why the probe asserts this at all: recovery on this tier has no acquire payload to
// read identity from, so it reads back `ownerName`/`repositoryName`/`workflowRunId`
// off the assignment message and stamps them on the worker pod. Those fields are
// modelled from the official actions/scaleset client's JobMessageBase and
// corroborated by §2a-3's live `scaleSetAssignTime` observation, but GAG has never
// dumped these three specific fields off a live JobAssigned — so this is the
// verification the plan doc names as outstanding.
//
// The raw body is already logged one line above, which is the primary evidence. This
// exists because a truncated, redacted body blob is exactly the kind of evidence a
// reader has to squint at and then forgets to write down: the GAP verdict names the
// consequence, so a live run answers the question instead of merely containing the
// answer.
func (p *scalesetProbe) reportRunIdentity(a scaleset.JobMessage) {
	owner, repo, runID, ok := a.RunIdentity()
	if !ok {
		p.log.Warn("INVESTIGATION-E: GAP — JobAssigned carries no complete run identity; "+
			"scale-set eviction recovery cannot name a run to re-run for this job (Q417)",
			"jobId", a.JobID,
			"ownerName", a.OwnerName,
			"repositoryName", a.RepositoryName,
			"workflowRunId", a.WorkflowRunID)
		return
	}
	p.log.Info("INVESTIGATION-E: run identity present on JobAssigned — scale-set eviction recovery has a rerun target (Q417)",
		"jobId", a.JobID,
		"owner", owner,
		"repo", repo,
		"runId", runID,
		"jobDisplayName", a.JobDisplayName)
}

// deliveredAcquireURL returns the acquireJobUrl a JobAvailable entry carried, or
// "" when none did.
func deliveredAcquireURL(jobs []scaleset.JobMessage) string {
	for _, j := range jobs {
		if j.MessageType == scaleset.MessageTypeJobAvailable && j.AcquireJobURL != "" {
			return j.AcquireJobURL
		}
	}
	return ""
}

// acquireOffered claims ids through the shipping client and, only if that fails
// while the message carried its own acquireJobUrl, retries against the delivered
// URL and reports the difference.
//
// The order matters and is the point: Client.AcquireJobs always targets the
// static _apis/runtime route, but a JobAvailable may name a different one — and
// on the broker-host tenant the static route was observed to 404 (Q264 §2a-3).
// Trying the client's construction first means a live run answers "does the route
// GAG ships still work", and a fallback that succeeds is logged as a
// library-vs-wire DIVERGENCE rather than quietly papering over it.
func (p *scalesetProbe) acquireOffered(ctx context.Context, scaleSetID int,
	sess *scaleset.RunnerScaleSetSession, ids []int64, acquireURL string) {
	won, err := p.client.AcquireJobs(ctx, scaleSetID, sess, ids)
	if err == nil {
		p.log.Info("INVESTIGATION-E: job test acquirejobs (client construction)",
			"requested", fmt.Sprintf("%v", ids), "won", fmt.Sprintf("%v", won))
		return
	}
	p.log.Warn("INVESTIGATION-E: job test acquirejobs (client construction) failed",
		"requested", fmt.Sprintf("%v", ids), "error", err)
	if acquireURL == "" {
		return
	}
	payload, _ := json.Marshal(ids)
	status, body, fErr := p.queueCall(ctx, http.MethodPost, acquireURL, sess.MessageQueueAccessToken, payload)
	if fErr != nil {
		p.log.Error("INVESTIGATION-E: job test acquire via delivered acquireJobUrl failed", "error", fErr)
		return
	}
	if status >= 200 && status <= 299 {
		p.log.Warn("INVESTIGATION-E: DIVERGENCE — the delivered acquireJobUrl acquired the job "+
			"where the scaleset client's static _apis/runtime route did not",
			"status", status, "body", githubapp.SanitizeBody(body, 512))
		return
	}
	p.log.Info("INVESTIGATION-E: job test acquire via delivered acquireJobUrl also failed",
		"status", status, "body", githubapp.SanitizeBody(body, 512))
}
