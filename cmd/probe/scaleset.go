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
//	                             acquire it, observing JobAssigned.
//	PROBE_SCALESET_NAME        - Scale set name (default gag-probe-scaleset).
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
	// deferred deletes).
	Cleanup bool
}

// parseScalesetConfig reads and validates the scale-set scenario environment
// from the injected getenv function (normally os.Getenv).
func parseScalesetConfig(getenv func(string) string) (scalesetConfig, error) {
	var cfg scalesetConfig

	mustEnv := func(name string) (string, error) {
		v := getenv(name)
		if v == "" {
			return "", fmt.Errorf("required environment variable %s is not set", name)
		}
		return v, nil
	}

	appIDStr, err := mustEnv("GITHUB_APP_ID")
	if err != nil {
		return scalesetConfig{}, err
	}
	if _, err := fmt.Sscan(appIDStr, &cfg.AppID); err != nil {
		return scalesetConfig{}, fmt.Errorf("parse GITHUB_APP_ID: %w", err)
	}
	installIDStr, err := mustEnv("GITHUB_APP_INSTALLATION_ID")
	if err != nil {
		return scalesetConfig{}, err
	}
	if _, err := fmt.Sscan(installIDStr, &cfg.InstallationID); err != nil {
		return scalesetConfig{}, fmt.Errorf("parse GITHUB_APP_INSTALLATION_ID: %w", err)
	}
	pemValue, err := mustEnv("GITHUB_APP_PRIVATE_KEY")
	if err != nil {
		return scalesetConfig{}, err
	}
	cfg.PrivateKeyPEM, err = loadPEM(pemValue)
	if err != nil {
		return scalesetConfig{}, fmt.Errorf("load GITHUB_APP_PRIVATE_KEY: %w", err)
	}
	cfg.ConfigURL, err = mustEnv("GITHUB_ORG_URL")
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
	return cfg, nil
}

// scalesetProbe carries the dependencies of the scale-set scenario. APIBase and
// the HTTP clients are injectable so the whole flow is unit-testable against an
// httptest stub, mirroring runProbe's structure.
type scalesetProbe struct {
	log      *slog.Logger
	cfg      scalesetConfig
	provider tokenProvider

	// apiBase is the REST API root (https://api.github.com in production;
	// an httptest URL under test).
	apiBase string
	// hc serves the short one-shot calls; pollClient serves the ~50s
	// message-queue long-poll and needs the longer timeout.
	hc         *http.Client
	pollClient *http.Client
	// jobTestTimeout bounds the optional live-job test's wait for a
	// JobAvailable (default 3 minutes; injectable for tests).
	jobTestTimeout time.Duration
}

// scale-set wire types, local to the probe: this is investigation tooling for
// a protocol GAG does not (yet) speak in production, so the shapes live here,
// not in the production broker package. Field sets follow the official
// actions/scaleset client and the ARC gha-runner-scale-set client.

type adminConnection struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

type runnerScaleSet struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	RunnerGroupID int    `json:"runnerGroupId"`
}

type runnerScaleSetSession struct {
	SessionID               string          `json:"sessionId"`
	OwnerName               string          `json:"ownerName"`
	MessageQueueURL         string          `json:"messageQueueUrl"`
	MessageQueueAccessToken string          `json:"messageQueueAccessToken"`
	Statistics              json.RawMessage `json:"statistics"`
}

type runnerScaleSetMessage struct {
	MessageID   int64           `json:"messageId"`
	MessageType string          `json:"messageType"`
	Body        string          `json:"body"`
	Statistics  json.RawMessage `json:"statistics"`
}

type jitRunnerConfig struct {
	Runner struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"runner"`
	EncodedJITConfig string `json:"encodedJITConfig"`
}

// runScalesetProbe is the Investigation E entry point wired from run().
func runScalesetProbe(ctx context.Context, logger *slog.Logger, cfg scalesetConfig, provider tokenProvider, apiBase string) error {
	p := &scalesetProbe{
		log:      logger,
		cfg:      cfg,
		provider: provider,
		apiBase:  apiBase,
		hc:       httpx.NewClient(),
		// The queue long-poll holds ~50s before returning 202; bound the
		// whole call just past that.
		pollClient: &http.Client{Timeout: 75 * time.Second},
	}
	return p.run(ctx)
}

// mintAdminConnection runs the two bootstrap hops (installation token →
// registration token → RemoteAuth runner-registration) and returns a fresh
// admin connection. Called at start and again before cleanup after a long
// hold — the admin JWT was observed live to expire within ~17 minutes, so a
// connection minted at start 401s the deferred deletes.
func (p *scalesetProbe) mintAdminConnection(ctx context.Context) (*adminConnection, error) {
	installToken, err := p.provider.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("get installation token: %w", err)
	}
	regToken, err := p.registrationToken(ctx, installToken)
	if err != nil {
		return nil, fmt.Errorf("registration token: %w", err)
	}
	conn, err := p.adminConnection(ctx, regToken)
	if err != nil {
		return nil, fmt.Errorf("runner-registration hop: %w", err)
	}
	return conn, nil
}

// cleanupOnly looks the scale set up by name and deletes it — recovery for a
// leaked scale set from an interrupted or 401-ed run.
func (p *scalesetProbe) cleanupOnly(ctx context.Context, conn *adminConnection) error {
	var out struct {
		Count int `json:"count"`
		Value []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"value"`
	}
	status, err := p.svcCall(ctx, conn, http.MethodGet,
		"/_apis/runtime/runnerscalesets?name="+url.QueryEscape(p.cfg.ScaleSetName), nil, &out)
	if err != nil {
		return fmt.Errorf("lookup scale set %q: %w", p.cfg.ScaleSetName, err)
	}
	if out.Count == 0 {
		p.log.Info("INVESTIGATION-E: cleanup — no scale set with that name", "name", p.cfg.ScaleSetName, "status", status)
		return nil
	}
	for _, ss := range out.Value {
		dStatus, dErr := p.svcCall(ctx, conn, http.MethodDelete,
			fmt.Sprintf("/_apis/runtime/runnerscalesets/%d", ss.ID), nil, nil)
		if dErr != nil {
			return fmt.Errorf("delete scale set %d: %w", ss.ID, dErr)
		}
		p.log.Info("INVESTIGATION-E: cleanup — scale set deleted", "id", ss.ID, "name", ss.Name, "status", dStatus)
	}
	return nil
}

func (p *scalesetProbe) run(ctx context.Context) error {
	// ── 1–3. Bootstrap: installation token → registration token → admin JWT ──
	conn, err := p.mintAdminConnection(ctx)
	if err != nil {
		return err
	}
	p.log.Info("INVESTIGATION-E: admin connection established",
		"actionsServiceURL", conn.URL, "adminTokenLen", len(conn.Token))

	if p.cfg.Cleanup {
		return p.cleanupOnly(ctx, conn)
	}

	// ── 4. Resolve runner group (fall back to the default group id 1) ────────
	groupID := p.resolveRunnerGroup(ctx, conn)

	// ── 5. Create the throwaway scale set ────────────────────────────────────
	ss, err := p.createScaleSet(ctx, conn, groupID)
	if err != nil {
		return fmt.Errorf("create scale set: %w", err)
	}
	defer func() {
		dCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		status, delErr := p.svcCall(dCtx, conn, http.MethodDelete,
			fmt.Sprintf("/_apis/runtime/runnerscalesets/%d", ss.ID), nil, nil)
		if delErr != nil {
			p.log.Error("INVESTIGATION-E: delete scale set failed", "error", delErr)
		} else {
			p.log.Info("INVESTIGATION-E: scale set deleted", "id", ss.ID, "status", status)
		}
	}()

	// ── 6. Create the message session ────────────────────────────────────────
	sess, err := p.createSession(ctx, conn, ss.ID)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	defer func() {
		dCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		status, delErr := p.svcCall(dCtx, conn, http.MethodDelete,
			fmt.Sprintf("/_apis/runtime/runnerscalesets/%d/sessions/%s", ss.ID, sess.SessionID), nil, nil)
		if delErr != nil {
			p.log.Error("INVESTIGATION-E: delete session failed", "error", delErr)
		} else {
			p.log.Info("INVESTIGATION-E: session deleted", "sessionId", sess.SessionID, "status", status)
		}
	}()

	if p.cfg.CapacityTest {
		// ── Investigation E2: capacity gating / refresh / replay ────────────
		p.capacityTest(ctx, conn, ss.ID, sess)
	} else {
		// ── 7. One queue long-poll (U2: 202 semantics; U4: headers) ─────────
		if err := p.pollQueueOnce(ctx, sess); err != nil {
			// Non-fatal: the poll's status/headers are the finding either way.
			p.log.Warn("INVESTIGATION-E: queue poll returned error", "error", err)
		}

		// ── 8. acquirejobs shape probes (U2: empty batch + unknown id) ──────
		p.probeAcquireJobs(ctx, conn, ss.ID, sess)

		// ── 9. generatejitconfig ────────────────────────────────────────────
		if err := p.probeJITConfig(ctx, conn, ss.ID); err != nil {
			p.log.Warn("INVESTIGATION-E: generatejitconfig failed", "error", err)
		}
	}

	// ── 10. Mint JIT configs for externally run runners ──────────────────────
	for i, path := range p.cfg.JITConfigFiles {
		if err := p.mintJITConfigToFile(ctx, conn, ss.ID, i+1, path); err != nil {
			p.log.Warn("INVESTIGATION-E: mint JIT config failed", "index", i+1, "error", err)
		}
	}

	// ── 11. Optional live-job test ───────────────────────────────────────────
	if p.cfg.JobTest {
		p.jobTest(ctx, conn, ss.ID, sess)
	}

	// ── 12. Optional hold: keep the scale set alive for external runners, ────
	//        observing the lifecycle messages they generate.
	if p.cfg.HoldSeconds > 0 {
		p.holdAndObserve(ctx, sess)
		// The admin JWT expires within ~17 minutes (observed live); re-mint
		// the connection so the deferred deletes below don't 401.
		if fresh, mErr := p.mintAdminConnection(ctx); mErr != nil {
			p.log.Warn("INVESTIGATION-E: re-mint admin connection for cleanup failed", "error", mErr)
		} else {
			*conn = *fresh
		}
	}

	p.log.Info("INVESTIGATION-E: scenario complete; cleaning up")
	return nil
}

// pollOnce issues one message-queue long-poll with the given advertised
// capacity and cursor, returning the HTTP status and any decoded message.
func (p *scalesetProbe) pollOnce(ctx context.Context, sess *runnerScaleSetSession, capacity int, lastMessageID int64) (int, *runnerScaleSetMessage, error) {
	u := sess.MessageQueueURL
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	u += sep + fmt.Sprintf("lastMessageId=%d", lastMessageID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+sess.MessageQueueAccessToken)
	req.Header.Set("X-ScaleSetMaxCapacity", fmt.Sprintf("%d", capacity))
	resp, err := p.pollClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || len(body) == 0 {
		return resp.StatusCode, nil, nil
	}
	var msg runnerScaleSetMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return resp.StatusCode, nil, fmt.Errorf("decode message: %w", err)
	}
	return resp.StatusCode, &msg, nil
}

// logQueueMessage emits one summarising log line for a queue message.
func (p *scalesetProbe) logQueueMessage(tag string, capacity int, msg *runnerScaleSetMessage) {
	p.log.Info("INVESTIGATION-E2: "+tag,
		"capacity", capacity,
		"messageId", msg.MessageID,
		"messageType", msg.MessageType,
		"statistics", string(msg.Statistics),
		"body", githubapp.SanitizeBody([]byte(msg.Body), 1024))
}

// capacityTest is Investigation E2: with jobs pre-queued on the scale set's
// label, poll at capacity 0, 1, then 2 to observe whether assignment is
// strictly capacity-gated and whether a JobAvailable/acquire flow appears for
// jobs above the advertised capacity; then exercise the session token refresh
// (PATCH) and the delete/recreate replay behaviour. sess is updated in place
// when the session is refreshed or recreated so the caller's deferred cleanup
// deletes the live session.
func (p *scalesetProbe) capacityTest(ctx context.Context, conn *adminConnection, scaleSetID int, sess *runnerScaleSetSession) {
	var cursor int64

	// Phase 1 — capacity 0, jobs queued: does anything arrive?
	status, msg, err := p.pollOnce(ctx, sess, 0, cursor)
	if err != nil {
		p.log.Warn("INVESTIGATION-E2: capacity-0 poll error", "error", err)
	} else if msg == nil {
		p.log.Info("INVESTIGATION-E2: capacity-0 poll returned no message (jobs held)", "status", status)
	} else {
		cursor = msg.MessageID
		p.logQueueMessage("capacity-0 poll delivered", 0, msg)
	}

	// Phase 2 — capacity 1: exactly one assignment expected if gated.
	status, msg, err = p.pollOnce(ctx, sess, 1, cursor)
	if err != nil {
		p.log.Warn("INVESTIGATION-E2: capacity-1 poll error", "error", err)
	} else if msg == nil {
		p.log.Info("INVESTIGATION-E2: capacity-1 poll returned no message", "status", status)
	} else {
		cursor = msg.MessageID
		p.logQueueMessage("capacity-1 poll delivered", 1, msg)
	}

	// Phase 3 — capacity 2: the held job should follow if gating is dynamic.
	// (cursor is not advanced further — the replay test below polls from 0.)
	status, msg, err = p.pollOnce(ctx, sess, 2, cursor)
	if err != nil {
		p.log.Warn("INVESTIGATION-E2: capacity-2 poll error", "error", err)
	} else if msg == nil {
		p.log.Info("INVESTIGATION-E2: capacity-2 poll returned no message", "status", status)
	} else {
		p.logQueueMessage("capacity-2 poll delivered", 2, msg)
	}

	// Phase 4 — session token refresh (PATCH).
	var refreshed runnerScaleSetSession
	rStatus, rErr := p.svcCall(ctx, conn, http.MethodPatch,
		fmt.Sprintf("/_apis/runtime/runnerscalesets/%d/sessions/%s", scaleSetID, sess.SessionID), nil, &refreshed)
	if rErr != nil {
		p.log.Warn("INVESTIGATION-E2: session refresh (PATCH) failed", "status", rStatus, "error", rErr)
	} else {
		p.log.Info("INVESTIGATION-E2: session refresh (PATCH)",
			"status", rStatus,
			"sameSessionId", refreshed.SessionID == sess.SessionID,
			"tokenChanged", refreshed.MessageQueueAccessToken != sess.MessageQueueAccessToken,
			"newTokenLen", len(refreshed.MessageQueueAccessToken))
		if refreshed.MessageQueueAccessToken != "" {
			sess.MessageQueueAccessToken = refreshed.MessageQueueAccessToken
		}
		if refreshed.MessageQueueURL != "" {
			sess.MessageQueueURL = refreshed.MessageQueueURL
		}
	}

	// Phase 5 — delete + recreate the session; does the message state replay?
	dStatus, dErr := p.svcCall(ctx, conn, http.MethodDelete,
		fmt.Sprintf("/_apis/runtime/runnerscalesets/%d/sessions/%s", scaleSetID, sess.SessionID), nil, nil)
	if dErr != nil {
		p.log.Warn("INVESTIGATION-E2: session delete for replay test failed", "status", dStatus, "error", dErr)
		return
	}
	newSess, cErr := p.createSession(ctx, conn, scaleSetID)
	if cErr != nil {
		p.log.Warn("INVESTIGATION-E2: session recreate for replay test failed", "error", cErr)
		return
	}
	*sess = *newSess // the caller's deferred cleanup must delete the live session
	status, msg, err = p.pollOnce(ctx, sess, 2, 0)
	if err != nil {
		p.log.Warn("INVESTIGATION-E2: replay poll error", "error", err)
	} else if msg == nil {
		p.log.Info("INVESTIGATION-E2: replay poll returned no message (no replay)", "status", status)
	} else {
		p.logQueueMessage("replay poll delivered (state replayed to fresh session)", 2, msg)
	}
}

// mintJITConfigToFile mints one JIT runner config and writes the encoded blob
// to path (0600) so an external runner can register with it. The blob is
// runner credentials — it is written to the file and never logged.
func (p *scalesetProbe) mintJITConfigToFile(ctx context.Context, conn *adminConnection, scaleSetID, index int, path string) error {
	var out jitRunnerConfig
	status, err := p.svcCall(ctx, conn, http.MethodPost,
		fmt.Sprintf("/_apis/runtime/runnerscalesets/%d/generatejitconfig", scaleSetID),
		map[string]string{
			"name":       fmt.Sprintf("%s-runner-%d", p.cfg.ScaleSetName, index),
			"workFolder": "_work",
		}, &out)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(out.EncodedJITConfig), 0o600); err != nil {
		return fmt.Errorf("write JIT config: %w", err)
	}
	p.log.Info("INVESTIGATION-E2: JIT config minted to file",
		"status", status, "runnerId", out.Runner.ID, "runnerName", out.Runner.Name, "path", path)
	return nil
}

// holdAndObserve keeps the scale set alive for HoldSeconds, polling the queue
// (capacity 2) and logging every message — the JobStarted/JobCompleted
// lifecycle generated by externally run runners.
func (p *scalesetProbe) holdAndObserve(ctx context.Context, sess *runnerScaleSetSession) {
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
		status, msg, err := p.pollOnce(deadline, sess, 2, cursor)
		if err != nil {
			if deadline.Err() != nil {
				p.log.Info("INVESTIGATION-E2: hold window over")
				return
			}
			p.log.Warn("INVESTIGATION-E2: hold poll error", "error", err)
			return
		}
		if msg == nil {
			p.log.Debug("INVESTIGATION-E2: hold poll empty", "status", status)
			continue
		}
		cursor = msg.MessageID
		p.logQueueMessage("hold observed message", 2, msg)
	}
}

// registrationToken exchanges the installation token for a short-lived runner
// registration token at org scope (or repo scope when GITHUB_ORG_URL has an
// owner/repo path).
func (p *scalesetProbe) registrationToken(ctx context.Context, installToken string) (string, error) {
	u, err := url.Parse(p.cfg.ConfigURL)
	if err != nil {
		return "", fmt.Errorf("parse GITHUB_ORG_URL: %w", err)
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	var path string
	switch len(segs) {
	case 1:
		path = "/orgs/" + segs[0] + "/actions/runners/registration-token"
	case 2:
		path = "/repos/" + segs[0] + "/" + segs[1] + "/actions/runners/registration-token"
	default:
		return "", fmt.Errorf("GITHUB_ORG_URL must be an org or owner/repo URL, got path %q", u.Path)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+installToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, githubapp.SanitizeBody(body, 256))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode registration token: %w", err)
	}
	return out.Token, nil
}

// adminConnection performs the RemoteAuth hop that discovers the Actions
// Service tenant URL and mints the admin JWT.
func (p *scalesetProbe) adminConnection(ctx context.Context, regToken string) (*adminConnection, error) {
	payload, err := json.Marshal(map[string]string{
		"url":          p.cfg.ConfigURL,
		"runner_event": "register",
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.apiBase+"/actions/runner-registration", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	// The one place the protocol uses the nonstandard RemoteAuth scheme.
	req.Header.Set("Authorization", "RemoteAuth "+regToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	p.log.Info("INVESTIGATION-E: runner-registration response", "status", resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("POST /actions/runner-registration: status %d: %s",
			resp.StatusCode, githubapp.SanitizeBody(body, 256))
	}
	var conn adminConnection
	if err := json.Unmarshal(body, &conn); err != nil {
		return nil, fmt.Errorf("decode admin connection: %w", err)
	}
	if conn.URL == "" || conn.Token == "" {
		return nil, fmt.Errorf("admin connection missing url or token (urlSet=%t tokenLen=%d)",
			conn.URL != "", len(conn.Token))
	}
	return &conn, nil
}

// svcCall issues one Actions Service call ({conn.URL}{path}?api-version=6.0-preview)
// with the admin JWT, decoding the JSON response into out when non-nil.
// It returns the HTTP status so callers can log shape findings on non-2xx too.
func (p *scalesetProbe) svcCall(ctx context.Context, conn *adminConnection, method, path string, in, out any) (int, error) {
	u := strings.TrimSuffix(conn.URL, "/") + path
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	u += sep + "api-version=6.0-preview"

	var bodyReader io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		bodyReader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+conn.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("%s %s: status %d: %s",
			method, path, resp.StatusCode, githubapp.SanitizeBody(body, 512))
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: decode response: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// resolveRunnerGroup looks up the configured runner group id, falling back to
// 1 (GitHub's default-group id) if the endpoint rejects the call — the
// fallback keeps the probe productive while still logging the endpoint's real
// behaviour.
func (p *scalesetProbe) resolveRunnerGroup(ctx context.Context, conn *adminConnection) int {
	var out struct {
		Count int `json:"count"`
		Value []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"value"`
	}
	status, err := p.svcCall(ctx, conn, http.MethodGet,
		"/_apis/runtime/runnergroups/?groupName="+url.QueryEscape(p.cfg.GroupName), nil, &out)
	if err != nil {
		p.log.Warn("INVESTIGATION-E: runnergroups lookup failed; falling back to group id 1",
			"status", status, "error", err)
		return 1
	}
	p.log.Info("INVESTIGATION-E: runnergroups lookup", "status", status, "count", out.Count)
	if out.Count > 0 {
		p.log.Info("INVESTIGATION-E: resolved runner group",
			"id", out.Value[0].ID, "name", out.Value[0].Name)
		return out.Value[0].ID
	}
	return 1
}

func (p *scalesetProbe) createScaleSet(ctx context.Context, conn *adminConnection, groupID int) (*runnerScaleSet, error) {
	in := map[string]any{
		"name":          p.cfg.ScaleSetName,
		"runnerGroupId": groupID,
		"labels": []map[string]string{
			{"name": p.cfg.ScaleSetName, "type": "System"},
		},
		"runnerSetting": map[string]any{"ephemeral": true},
	}
	var ss runnerScaleSet
	status, err := p.svcCall(ctx, conn, http.MethodPost, "/_apis/runtime/runnerscalesets", in, &ss)
	if err != nil {
		return nil, err
	}
	p.log.Info("INVESTIGATION-E: scale set created",
		"status", status, "id", ss.ID, "name", ss.Name, "runnerGroupId", ss.RunnerGroupID)
	return &ss, nil
}

func (p *scalesetProbe) createSession(ctx context.Context, conn *adminConnection, scaleSetID int) (*runnerScaleSetSession, error) {
	var sess runnerScaleSetSession
	status, err := p.svcCall(ctx, conn, http.MethodPost,
		fmt.Sprintf("/_apis/runtime/runnerscalesets/%d/sessions", scaleSetID),
		map[string]string{"ownerName": "gag-probe"}, &sess)
	if err != nil {
		return nil, err
	}
	queueHost := sess.MessageQueueURL
	queuePath := ""
	queueParams := ""
	if qu, qerr := url.Parse(sess.MessageQueueURL); qerr == nil {
		queueHost = qu.Host
		queuePath = qu.Path
		// Query VALUES may carry signatures; log the parameter names only.
		var keys []string
		for k := range qu.Query() {
			keys = append(keys, k)
		}
		queueParams = strings.Join(keys, ",")
	}
	// Log fields selectively: the raw body carries messageQueueAccessToken.
	p.log.Info("INVESTIGATION-E: session created",
		"status", status,
		"sessionId", sess.SessionID,
		"ownerName", sess.OwnerName,
		"queueHost", queueHost,
		"queuePath", queuePath,
		"queueParamNames", queueParams,
		"queueTokenLen", len(sess.MessageQueueAccessToken),
		"statistics", string(sess.Statistics))
	return &sess, nil
}

// pollQueueOnce issues a single message-queue long-poll and logs the status,
// latency, and every X-*/RateLimit/Retry-After header — the U2 (202 semantics)
// and U4 (rate limits) evidence.
func (p *scalesetProbe) pollQueueOnce(ctx context.Context, sess *runnerScaleSetSession) error {
	u := sess.MessageQueueURL
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	u += sep + "lastMessageId=0"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+sess.MessageQueueAccessToken)
	req.Header.Set("X-ScaleSetMaxCapacity", "1")

	start := time.Now()
	resp, err := p.pollClient.Do(req)
	if err != nil {
		return fmt.Errorf("queue long-poll: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	headers := map[string]string{}
	for k, v := range resp.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-") || strings.Contains(lk, "ratelimit") || lk == "retry-after" {
			headers[k] = strings.Join(v, ",")
		}
	}
	p.log.Info("INVESTIGATION-E: queue long-poll returned",
		"status", resp.StatusCode,
		"elapsed", time.Since(start).Round(time.Second).String(),
		"bodyLen", len(body),
		"headers", fmt.Sprintf("%v", headers))
	if resp.StatusCode == http.StatusOK && len(body) > 0 {
		var msg runnerScaleSetMessage
		if jsonErr := json.Unmarshal(body, &msg); jsonErr == nil {
			p.log.Info("INVESTIGATION-E: queue message",
				"messageId", msg.MessageID, "messageType", msg.MessageType,
				"statistics", string(msg.Statistics),
				"body", githubapp.SanitizeBody([]byte(msg.Body), 512))
		}
	}
	return nil
}

// probeAcquireJobs observes the acquirejobs response shape for an empty batch
// and for an id that was never offered (U2 partial-batch semantics). Neither
// call can affect a real job: no job has been dispatched to this throwaway
// scale set.
func (p *scalesetProbe) probeAcquireJobs(ctx context.Context, conn *adminConnection, scaleSetID int, sess *runnerScaleSetSession) {
	svcBase := strings.TrimSuffix(conn.URL, "/")
	ssPath := fmt.Sprintf("/_apis/runtime/runnerscalesets/%d", scaleSetID)
	emptyBatch, _ := json.Marshal([]int64{})
	unknownBatch, _ := json.Marshal([]int64{9999999999})

	for _, tc := range []struct {
		label  string
		method string
		url    string
		token  string
		body   []byte
	}{
		// The official actions/scaleset client's construction: POST to the
		// Actions Service base, QUEUE token, bare []int64 body.
		{"empty batch, queue token", http.MethodPost,
			svcBase + ssPath + "/acquirejobs?api-version=6.0-preview", sess.MessageQueueAccessToken, emptyBatch},
		{"unknown id, queue token", http.MethodPost,
			svcBase + ssPath + "/acquirejobs?api-version=6.0-preview", sess.MessageQueueAccessToken, unknownBatch},
		// Diagnostics for a route-level 404 on the official construction:
		// same call with the admin JWT, and ARC's GET acquirablejobs.
		{"unknown id, admin token", http.MethodPost,
			svcBase + ssPath + "/acquirejobs?api-version=6.0-preview", conn.Token, unknownBatch},
		{"acquirablejobs, admin token", http.MethodGet,
			svcBase + ssPath + "/acquirablejobs?api-version=6.0-preview", conn.Token, nil},
		// The observed queue URL is {broker}/scalesets/message — a route
		// family outside /_apis/runtime. Probe the acquire verb there too.
		{"unknown id, queue-base route", http.MethodPost,
			queueBase(sess.MessageQueueURL) + "/acquirejobs", sess.MessageQueueAccessToken, unknownBatch},
		{"unknown id, queue-base route + api-version", http.MethodPost,
			queueBase(sess.MessageQueueURL) + "/acquirejobs?api-version=6.0-preview", sess.MessageQueueAccessToken, unknownBatch},
	} {
		var bodyReader io.Reader
		if tc.body != nil {
			bodyReader = bytes.NewReader(tc.body)
		}
		req, err := http.NewRequestWithContext(ctx, tc.method, tc.url, bodyReader)
		if err != nil {
			p.log.Warn("INVESTIGATION-E: acquirejobs build request failed", "case", tc.label, "error", err)
			continue
		}
		req.Header.Set("Authorization", "Bearer "+tc.token)
		if tc.body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := p.hc.Do(req)
		if err != nil {
			p.log.Warn("INVESTIGATION-E: acquirejobs request failed", "case", tc.label, "error", err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		p.log.Info("INVESTIGATION-E: acquirejobs shape probe",
			"case", tc.label,
			"status", resp.StatusCode,
			"body", githubapp.SanitizeBody(body, 256))
	}
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
func (p *scalesetProbe) probeJITConfig(ctx context.Context, conn *adminConnection, scaleSetID int) error {
	var out jitRunnerConfig
	status, err := p.svcCall(ctx, conn, http.MethodPost,
		fmt.Sprintf("/_apis/runtime/runnerscalesets/%d/generatejitconfig", scaleSetID),
		map[string]string{"name": p.cfg.ScaleSetName + "-runner", "workFolder": "_work"}, &out)
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
		"status", status,
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
func (p *scalesetProbe) jobTest(ctx context.Context, conn *adminConnection, scaleSetID int, sess *runnerScaleSetSession) {
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
			p.log.Warn("INVESTIGATION-E: job test timeout — no JobAvailable within 3 minutes")
			return
		}
		u := sess.MessageQueueURL
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + fmt.Sprintf("lastMessageId=%d", lastMessageID)
		req, err := http.NewRequestWithContext(deadline, http.MethodGet, u, nil)
		if err != nil {
			p.log.Error("INVESTIGATION-E: job test build request failed", "error", err)
			return
		}
		req.Header.Set("Authorization", "Bearer "+sess.MessageQueueAccessToken)
		req.Header.Set("X-ScaleSetMaxCapacity", "1")
		resp, err := p.pollClient.Do(req)
		if err != nil {
			if deadline.Err() != nil {
				continue
			}
			p.log.Error("INVESTIGATION-E: job test poll failed", "error", err)
			return
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || len(body) == 0 {
			p.log.Debug("INVESTIGATION-E: job test poll empty", "status", resp.StatusCode)
			continue
		}
		var msg runnerScaleSetMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			p.log.Warn("INVESTIGATION-E: job test message decode failed", "error", err)
			continue
		}
		lastMessageID = msg.MessageID
		p.log.Info("INVESTIGATION-E: job test message",
			"messageId", msg.MessageID, "messageType", msg.MessageType,
			"statistics", string(msg.Statistics),
			"body", githubapp.SanitizeBody([]byte(msg.Body), 1024))

		// Collect runnerRequestIds from any JobAvailable entries and acquire.
		// The message may carry an acquireJobUrl per entry — on backends where
		// the static /_apis/runtime acquire route 404s (observed live on the
		// broker-host tenant), the delivered URL is the authoritative one.
		var entries []struct {
			MessageType     string `json:"messageType"`
			RunnerRequestID int64  `json:"runnerRequestId"`
			AcquireJobURL   string `json:"acquireJobUrl"`
		}
		if err := json.Unmarshal([]byte(msg.Body), &entries); err != nil {
			continue
		}
		var ids []int64
		acquireURL := ""
		assigned := false
		for _, e := range entries {
			switch e.MessageType {
			case "JobAvailable":
				ids = append(ids, e.RunnerRequestID)
				if e.AcquireJobURL != "" {
					acquireURL = e.AcquireJobURL
					p.log.Info("INVESTIGATION-E: JobAvailable carries acquireJobUrl",
						"acquireJobUrl", e.AcquireJobURL)
				}
			case "JobAssigned":
				assigned = true
				p.log.Info("INVESTIGATION-E: JobAssigned observed — batch acquire confirmed",
					"runnerRequestId", e.RunnerRequestID)
			}
		}
		if assigned {
			return
		}
		if len(ids) == 0 {
			continue
		}
		payload, _ := json.Marshal(ids)
		acqURL := acquireURL
		if acqURL == "" {
			acqURL = strings.TrimSuffix(conn.URL, "/") +
				fmt.Sprintf("/_apis/runtime/runnerscalesets/%d/acquirejobs?api-version=6.0-preview", scaleSetID)
		}
		acqReq, err := http.NewRequestWithContext(deadline, http.MethodPost, acqURL, bytes.NewReader(payload))
		if err != nil {
			p.log.Error("INVESTIGATION-E: job test acquire build failed", "error", err)
			return
		}
		acqReq.Header.Set("Authorization", "Bearer "+sess.MessageQueueAccessToken)
		acqReq.Header.Set("Content-Type", "application/json")
		acqResp, err := p.hc.Do(acqReq)
		if err != nil {
			p.log.Error("INVESTIGATION-E: job test acquire failed", "error", err)
			return
		}
		acqBody, _ := io.ReadAll(acqResp.Body)
		_ = acqResp.Body.Close()
		p.log.Info("INVESTIGATION-E: job test acquirejobs",
			"requested", fmt.Sprintf("%v", ids),
			"status", acqResp.StatusCode,
			"body", githubapp.SanitizeBody(acqBody, 512))
	}
}
