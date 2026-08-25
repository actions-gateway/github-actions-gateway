// Investigation F (Q468): how long GitHub holds an unacknowledged JobCompleted
// when the scale set has no session at all.
//
// Set PROBE_RETENTION_TEST to arm, check, or cleanup to run one phase of this
// scenario instead of the classic broker probe. It settles the one question
// Q435's restart-orphan measurement left open: a Running worker whose job ended
// while the AGC was down is reclaimed if and only if GitHub redelivers that
// job's terminal JobCompleted to the restarted AGC's NEW session. The published
// contract covers only within-session redelivery of unacknowledged messages, so
// retention across a session gap is undocumented and can only be measured.
//
// The experiment spans hours and must run with no session in existence for the
// whole gap, so it cannot live in one process. It is three phases around a state
// file:
//
//	arm     → durable scale set, session, wait for a dispatched job, acquire it,
//	          ACK the assignment fully, cancel the run, poll until JobCompleted
//	          appears, leave it UNACKNOWLEDGED, delete the session, write state.
//	          The gap starts when the session delete returns.
//	check   → fresh session under a new owner name, poll from the recorded cursor
//	          for a bounded window, report RETAINED or LOST against the elapsed gap.
//	cleanup → delete any live session and the durable scale set.
//
// Why the JobCompleted is deliberately left unacknowledged: a LOST verdict is
// only evidence if the message provably existed and was provably unacknowledged
// when the gap began. The arm phase therefore neither advances the cursor past it
// nor issues the DeleteMessage half of the ack, and records the id of the message
// BEFORE it as the cursor a later check polls from.
//
// Required environment variables:
//
//	GITHUB_APP_ID              - GitHub App numeric ID
//	GITHUB_APP_PRIVATE_KEY     - Path to PEM file, or PEM literal
//	GITHUB_APP_INSTALLATION_ID - Installation ID for the target org
//	GITHUB_ORG_URL             - Org or repo URL (e.g. https://github.com/my-org)
//
// The App needs actions: write — the arm phase cancels the workflow run to drive
// the job terminal without a live runner, the same permission the Investigation
// C/D driver already needs for workflow_dispatch.
//
// Optional:
//
//	PROBE_RETENTION_STATE       - State file path (default tmp/q468-retention-state.json).
//	PROBE_RETENTION_NAME        - Scale set name (default gag-q468-retention).
//	PROBE_RETENTION_GROUP_NAME  - Runner group (default Default).
//	PROBE_RETENTION_ARM_TIMEOUT - How long arm waits for a dispatched job (default 5m).
//	PROBE_RETENTION_CHECK_WINDOW- How long check polls before calling it LOST (default 90s).
//	PROBE_RETENTION_CAPACITY    - Advertised poll capacity (default 1).
//	PROBE_RETENTION_KEEP_ARMED  - "true" to leave a RETAINED experiment open for a
//	                              longer-gap check instead of concluding it.
//
// The Q468 plan doc carries the experiment's design and, more importantly, what
// would make a result invalid; developer-facing entry points are indexed from
// the credential-gated probe scenarios section of docs/development/testing.md.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/actions-gateway/github-actions-gateway/githubapp"
	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
)

// Retention phase names, as accepted in PROBE_RETENTION_TEST.
const (
	retentionPhaseArm     = "arm"
	retentionPhaseCheck   = "check"
	retentionPhaseCleanup = "cleanup"
)

// Session owner names for each phase. They differ deliberately: a check must look
// to the backend like a different listener arriving after the gap — which is what a
// restarted AGC is — not like the arming listener resuming.
const (
	retentionArmOwner   = "gag-q468-arm"
	retentionCheckOwner = "gag-q468-check"
)

// retentionConfig holds the parsed environment for the retention scenario.
type retentionConfig struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPEM  []byte
	ConfigURL      string

	// Phase is arm, check, or cleanup.
	Phase string
	// StatePath is the JSON file carrying the experiment across the gap.
	StatePath string
	// ScaleSetName is durable, unlike Investigation E's throwaway: the scale set
	// has to outlive the arming process for the queue log to survive with it.
	ScaleSetName string
	GroupName    string

	// ArmTimeout bounds the arm phase's wait for a dispatched job.
	ArmTimeout time.Duration
	// CheckWindow bounds the check phase's poll before it reports LOST. A LOST
	// verdict means "not redelivered within this window", which is the budget a
	// restarting AGC actually has, not "provably deleted".
	CheckWindow time.Duration
	// Capacity is the advertised X-ScaleSetMaxCapacity. It stays at 1 rather than
	// dropping to 0 for the check because the measurement should see what a
	// restarted AGC sees, and capacity gates job assignment rather than delivery
	// of a terminal message.
	Capacity int
	// KeepArmed leaves a RETAINED experiment open so the same armed job can be
	// probed again at a longer gap. Read the resulting ladder with the caveat in
	// the plan doc: creating a session may itself reset retention.
	KeepArmed bool
}

// retentionCheck is one check phase's result, appended to the state file so a
// ladder of gaps is recorded rather than overwritten.
type retentionCheck struct {
	At         time.Time `json:"at"`
	GapSeconds float64   `json:"gapSeconds"`
	Gap        string    `json:"gap"`
	Verdict    string    `json:"verdict"`
	MessageID  int64     `json:"messageId,omitempty"`
	Result     string    `json:"result,omitempty"`
}

// retentionState is the experiment, persisted across the gap.
type retentionState struct {
	ScaleSetID   int    `json:"scaleSetId"`
	ScaleSetName string `json:"scaleSetName"`

	JobID           string `json:"jobId"`
	RunnerRequestID int64  `json:"runnerRequestId,omitempty"`
	Owner           string `json:"owner"`
	Repo            string `json:"repo"`
	RunID           int64  `json:"runId"`

	// Cursor is the id of the last message the arm phase acknowledged — the
	// lastMessageId a check polls from, so the JobCompleted under test is the very
	// next thing the queue can deliver.
	Cursor int64 `json:"cursor"`
	// CompletedMessageID is the JobCompleted the arm phase observed and left
	// unacknowledged. Its presence is what makes a later LOST verdict evidence.
	CompletedMessageID int64 `json:"completedMessageId"`

	// ArmedAt is when the arming session's delete returned — the instant the gap
	// starts, because that is when the scale set stopped having a session.
	ArmedAt time.Time `json:"armedAt"`

	Checks    []retentionCheck `json:"checks,omitempty"`
	Concluded bool             `json:"concluded,omitempty"`
}

// Verdicts a check phase reports.
const (
	verdictRetained = "RETAINED"
	verdictLost     = "LOST"
)

// parseRetentionConfig reads and validates the retention scenario environment from
// the injected getenv function (normally os.Getenv).
func parseRetentionConfig(getenv func(string) string) (retentionConfig, error) {
	var cfg retentionConfig

	cfg.Phase = getenv("PROBE_RETENTION_TEST")
	switch cfg.Phase {
	case retentionPhaseArm, retentionPhaseCheck, retentionPhaseCleanup:
	default:
		return retentionConfig{}, fmt.Errorf(
			"PROBE_RETENTION_TEST must be one of %q, %q, %q; got %q",
			retentionPhaseArm, retentionPhaseCheck, retentionPhaseCleanup, cfg.Phase)
	}

	appIDStr, err := mustEnv(getenv, "GITHUB_APP_ID")
	if err != nil {
		return retentionConfig{}, err
	}
	if _, err := fmt.Sscan(appIDStr, &cfg.AppID); err != nil {
		return retentionConfig{}, fmt.Errorf("parse GITHUB_APP_ID: %w", err)
	}
	installIDStr, err := mustEnv(getenv, "GITHUB_APP_INSTALLATION_ID")
	if err != nil {
		return retentionConfig{}, err
	}
	if _, err := fmt.Sscan(installIDStr, &cfg.InstallationID); err != nil {
		return retentionConfig{}, fmt.Errorf("parse GITHUB_APP_INSTALLATION_ID: %w", err)
	}
	pemValue, err := mustEnv(getenv, "GITHUB_APP_PRIVATE_KEY")
	if err != nil {
		return retentionConfig{}, err
	}
	cfg.PrivateKeyPEM, err = loadPEM(pemValue)
	if err != nil {
		return retentionConfig{}, fmt.Errorf("load GITHUB_APP_PRIVATE_KEY: %w", err)
	}
	cfg.ConfigURL, err = mustEnv(getenv, "GITHUB_ORG_URL")
	if err != nil {
		return retentionConfig{}, err
	}

	cfg.StatePath = getenv("PROBE_RETENTION_STATE")
	if cfg.StatePath == "" {
		cfg.StatePath = filepath.Join("tmp", "q468-retention-state.json")
	}
	cfg.ScaleSetName = getenv("PROBE_RETENTION_NAME")
	if cfg.ScaleSetName == "" {
		cfg.ScaleSetName = "gag-q468-retention"
	}
	cfg.GroupName = getenv("PROBE_RETENTION_GROUP_NAME")
	if cfg.GroupName == "" {
		cfg.GroupName = "Default"
	}
	if cfg.ArmTimeout, err = parseDurationEnv(getenv, "PROBE_RETENTION_ARM_TIMEOUT", 5*time.Minute); err != nil {
		return retentionConfig{}, err
	}
	if cfg.CheckWindow, err = parseDurationEnv(getenv, "PROBE_RETENTION_CHECK_WINDOW", 90*time.Second); err != nil {
		return retentionConfig{}, err
	}
	cfg.Capacity = 1
	if v := getenv("PROBE_RETENTION_CAPACITY"); v != "" {
		if _, err := fmt.Sscan(v, &cfg.Capacity); err != nil {
			return retentionConfig{}, fmt.Errorf("parse PROBE_RETENTION_CAPACITY: %w", err)
		}
	}
	cfg.KeepArmed = getenv("PROBE_RETENTION_KEEP_ARMED") == "true"
	return cfg, nil
}

// parseDurationEnv reads an optional Go duration from the environment.
func parseDurationEnv(getenv func(string) string, name string, def time.Duration) (time.Duration, error) {
	v := getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return d, nil
}

// retentionProbe carries the dependencies of the retention scenario. Like the
// scale-set scenario it drives the shipping scaleset.Client, so a live run is
// evidence about the code GAG ships rather than about the probe's own dialect.
type retentionProbe struct {
	log *slog.Logger
	cfg retentionConfig

	client *scaleset.Client
	// hc serves the one call outside the scale-set protocol: the REST run-cancel
	// that drives the job terminal without a live runner.
	hc      *http.Client
	tokens  githubapp.TokenProvider
	apiBase string
	// now is injectable so a test can drive the gap arithmetic without waiting.
	now func() time.Time
}

// newRetentionProbe builds the scenario around a scaleset.Client wired to the
// probe's wire logger. Arguments as newScalesetProbe.
func newRetentionProbe(logger *slog.Logger, cfg retentionConfig, provider githubapp.TokenProvider,
	apiBase string, hc, pollClient *http.Client) (*retentionProbe, error) {
	client, err := scaleset.New(scaleset.Config{
		TokenProvider: provider,
		ConfigURL:     cfg.ConfigURL,
		APIBase:       apiBase,
		HTTPClient:    hc,
		PollClient:    pollClient,
		Observer:      wireLog{log: logger, tag: "F"},
	})
	if err != nil {
		return nil, err
	}
	if hc == nil {
		hc = httpx.NewClient()
	}
	return &retentionProbe{
		log:     logger,
		cfg:     cfg,
		client:  client,
		hc:      hc,
		tokens:  provider,
		apiBase: apiBase,
		now:     time.Now,
	}, nil
}

// runRetentionProbe is the Investigation F entry point wired from run().
func runRetentionProbe(ctx context.Context, logger *slog.Logger, cfg retentionConfig,
	provider githubapp.TokenProvider, apiBase string) error {
	p, err := newRetentionProbe(logger, cfg, provider, apiBase, nil, nil)
	if err != nil {
		return err
	}
	return p.run(ctx)
}

func (p *retentionProbe) run(ctx context.Context) error {
	if err := p.client.Connect(ctx); err != nil {
		return err
	}
	p.log.Info("INVESTIGATION-F: admin connection established", "phase", p.cfg.Phase)

	switch p.cfg.Phase {
	case retentionPhaseArm:
		return p.arm(ctx)
	case retentionPhaseCheck:
		return p.check(ctx)
	case retentionPhaseCleanup:
		return p.cleanup(ctx)
	}
	return fmt.Errorf("unknown retention phase %q", p.cfg.Phase)
}

// ── Arm ──────────────────────────────────────────────────────────────────────

// arm stages the experiment and starts the gap. It ends with the scale set alive,
// no session, and exactly one unacknowledged message in the queue log: the
// JobCompleted under test.
func (p *retentionProbe) arm(ctx context.Context) error {
	if prior, err := p.loadState(); err == nil && !prior.Concluded {
		p.log.Warn("INVESTIGATION-F: overwriting a state file that still describes an open experiment",
			"path", p.cfg.StatePath, "armedAt", prior.ArmedAt.Format(time.RFC3339), "jobId", prior.JobID)
	}

	ss, err := p.ensureScaleSet(ctx)
	if err != nil {
		return err
	}

	sess, err := p.client.CreateSession(ctx, ss.ID, retentionArmOwner)
	if err != nil {
		return fmt.Errorf("create arming session: %w", err)
	}
	p.log.Info("INVESTIGATION-F: arming session created", "sessionId", sess.SessionID)

	state := &retentionState{ScaleSetID: ss.ID, ScaleSetName: ss.Name}

	// 1. Wait for a dispatched job and acquire it, acknowledging everything up to
	//    and including the assignment so the JobCompleted is the only thing left
	//    unacknowledged when the gap starts.
	assigned, err := p.awaitAssignment(ctx, ss.ID, sess, state)
	if err != nil {
		p.deleteSession(ctx, ss.ID, sess.SessionID)
		return err
	}

	// 2. Drive the job terminal. Nothing is running it, so cancel the run.
	if err := p.cancelRun(ctx, state.Owner, state.Repo, state.RunID); err != nil {
		p.deleteSession(ctx, ss.ID, sess.SessionID)
		return fmt.Errorf("cancel run %d: %w", state.RunID, err)
	}
	p.log.Info("INVESTIGATION-F: workflow run cancelled",
		"owner", state.Owner, "repo", state.Repo, "runId", state.RunID, "jobId", assigned.JobID)

	// 3. Observe the JobCompleted — and stop short of acknowledging it.
	if err := p.awaitCompletion(ctx, sess, state); err != nil {
		p.deleteSession(ctx, ss.ID, sess.SessionID)
		return err
	}

	// 4. Delete the session. The gap starts here, and only here: until this
	//    returns, the scale set still has a listener.
	if err := p.client.DeleteSession(ctx, ss.ID, sess.SessionID); err != nil {
		return fmt.Errorf("delete arming session (the gap never started): %w", err)
	}
	state.ArmedAt = p.now().UTC()

	if err := p.writeState(state); err != nil {
		return err
	}
	p.log.Info("INVESTIGATION-F: ARMED — no session exists; the gap starts now",
		"armedAt", state.ArmedAt.Format(time.RFC3339),
		"jobId", state.JobID,
		"completedMessageId", state.CompletedMessageID,
		"cursor", state.Cursor,
		"state", p.cfg.StatePath)
	p.log.Info("INVESTIGATION-F: run `PROBE_RETENTION_TEST=check` after the gap you want to measure")
	return nil
}

// ensureScaleSet looks the durable scale set up by name and creates it when it is
// not there, so an arm after a cleanup works and an arm after an interrupted run
// reuses what is already registered.
func (p *retentionProbe) ensureScaleSet(ctx context.Context) (*scaleset.RunnerScaleSet, error) {
	existing, err := p.client.GetRunnerScaleSetByName(ctx, p.cfg.ScaleSetName)
	if err != nil {
		return nil, fmt.Errorf("lookup scale set %q: %w", p.cfg.ScaleSetName, err)
	}
	if existing != nil {
		p.log.Info("INVESTIGATION-F: reusing scale set", "id", existing.ID, "name", existing.Name)
		return existing, nil
	}
	groupID := 1
	if id, ok, gErr := p.client.ResolveRunnerGroup(ctx, p.cfg.GroupName); gErr != nil {
		p.log.Warn("INVESTIGATION-F: runnergroups lookup failed; falling back to group id 1", "error", gErr)
	} else if ok {
		groupID = id
	}
	ss, err := p.client.CreateRunnerScaleSet(ctx, scaleset.RunnerScaleSet{
		Name:          p.cfg.ScaleSetName,
		RunnerGroupID: groupID,
		Labels:        []scaleset.Label{{Name: p.cfg.ScaleSetName, Type: "System"}},
		RunnerSetting: &scaleset.RunnerSetting{Ephemeral: true},
	})
	if err != nil {
		return nil, fmt.Errorf("create scale set: %w", err)
	}
	p.log.Info("INVESTIGATION-F: scale set created",
		"id", ss.ID, "name", ss.Name, "runnerGroupId", ss.RunnerGroupID)
	return ss, nil
}

// awaitAssignment polls until a job is assigned to the scale set, acquiring it
// first on the GHES offer flow. Every message it sees is acknowledged in full —
// cursor advanced AND deleted — because the experiment's validity rests on the
// JobCompleted being the only unacknowledged message when the gap starts.
//
// It fills the run identity into state, which the cancel step and a later check
// both need; an assignment carrying no complete identity fails the arm rather
// than proceeding to cancel a run we cannot name.
func (p *retentionProbe) awaitAssignment(ctx context.Context, scaleSetID int,
	sess *scaleset.RunnerScaleSetSession, state *retentionState) (*scaleset.JobMessage, error) {
	p.log.Info("INVESTIGATION-F: dispatch a workflow with runs-on: "+p.cfg.ScaleSetName+" NOW",
		"waiting", p.cfg.ArmTimeout.String())
	deadline, cancel := context.WithTimeout(ctx, p.cfg.ArmTimeout)
	defer cancel()

	for {
		msg, err := p.pollOnce(deadline, sess, state.Cursor)
		if err != nil {
			return nil, err
		}
		if msg == nil {
			if deadline.Err() != nil {
				return nil, fmt.Errorf("no job assigned within %s — was a workflow dispatched to %q?",
					p.cfg.ArmTimeout, p.cfg.ScaleSetName)
			}
			continue
		}
		jobs, err := msg.Jobs()
		if err != nil {
			p.log.Warn("INVESTIGATION-F: message decode failed", "messageId", msg.MessageID, "error", err)
		}
		p.log.Info("INVESTIGATION-F: arm observed message",
			"messageId", msg.MessageID, "entries", len(jobs),
			"body", githubapp.SanitizeBody([]byte(msg.Body), 512))

		assigned := scaleset.AssignedJobs(jobs)
		// Acknowledge this message in full before deciding, so the cursor lands
		// immediately after the assignment either way.
		p.ackMessage(deadline, sess, msg, state)

		if len(assigned) > 0 {
			a := assigned[0]
			owner, repo, _, ok := a.RunIdentity()
			if !ok {
				return nil, fmt.Errorf(
					"JobAssigned for job %s carries no complete run identity (owner=%q repo=%q runId=%d); "+
						"the arm phase cannot name a run to cancel",
					a.JobID, a.OwnerName, a.RepositoryName, a.WorkflowRunID)
			}
			state.JobID = a.JobID
			state.RunnerRequestID = a.RunnerRequestID
			state.Owner, state.Repo, state.RunID = owner, repo, a.WorkflowRunID
			p.log.Info("INVESTIGATION-F: job assigned",
				"jobId", a.JobID, "owner", owner, "repo", repo, "runId", a.WorkflowRunID)
			return &a, nil
		}
		if ids := scaleset.AvailableJobIDs(jobs); len(ids) > 0 {
			won, aErr := p.client.AcquireJobs(deadline, scaleSetID, sess, ids)
			if aErr != nil {
				p.log.Warn("INVESTIGATION-F: acquirejobs failed", "requested", ids, "error", aErr)
				continue
			}
			p.log.Info("INVESTIGATION-F: acquired offered jobs", "requested", ids, "won", won)
		}
	}
}

// awaitCompletion polls until the JobCompleted for the armed job arrives, and
// deliberately leaves it unacknowledged — that message is the subject of the
// experiment. Messages that arrive ahead of it are acknowledged in full so the
// recorded cursor sits immediately before it.
func (p *retentionProbe) awaitCompletion(ctx context.Context,
	sess *scaleset.RunnerScaleSetSession, state *retentionState) error {
	deadline, cancel := context.WithTimeout(ctx, p.cfg.ArmTimeout)
	defer cancel()

	for {
		msg, err := p.pollOnce(deadline, sess, state.Cursor)
		if err != nil {
			return err
		}
		if msg == nil {
			if deadline.Err() != nil {
				return fmt.Errorf("no JobCompleted for job %s within %s of cancelling run %d",
					state.JobID, p.cfg.ArmTimeout, state.RunID)
			}
			continue
		}
		jobs, err := msg.Jobs()
		if err != nil {
			p.log.Warn("INVESTIGATION-F: message decode failed", "messageId", msg.MessageID, "error", err)
		}
		if completed, ok := findCompleted(jobs, state.JobID); ok {
			state.CompletedMessageID = msg.MessageID
			p.log.Info("INVESTIGATION-F: JobCompleted observed and left UNACKNOWLEDGED",
				"messageId", msg.MessageID, "jobId", completed.JobID, "result", completed.Result,
				"cursor", state.Cursor)
			return nil
		}
		p.log.Info("INVESTIGATION-F: arm observed intervening message",
			"messageId", msg.MessageID, "entries", len(jobs),
			"body", githubapp.SanitizeBody([]byte(msg.Body), 512))
		p.ackMessage(deadline, sess, msg, state)
	}
}

// findCompleted returns the JobCompleted entry for jobID, if the batch carries one.
func findCompleted(jobs []scaleset.JobMessage, jobID string) (scaleset.JobMessage, bool) {
	for _, j := range jobs {
		if j.MessageType == scaleset.MessageTypeJobCompleted && j.JobID == jobID {
			return j, true
		}
	}
	return scaleset.JobMessage{}, false
}

// ackMessage performs the full acknowledgement — advance the cursor past the
// message AND delete it — so it cannot be replayed to a later session and confound
// the measurement. A delete failure is logged rather than fatal: the cursor half
// still moved, and the check reports what it actually receives.
func (p *retentionProbe) ackMessage(ctx context.Context, sess *scaleset.RunnerScaleSetSession,
	msg *scaleset.RunnerScaleSetMessage, state *retentionState) {
	state.Cursor = msg.MessageID
	deleted, err := p.client.DeleteMessage(ctx, sess, msg.MessageID)
	switch {
	case err != nil:
		p.log.Warn("INVESTIGATION-F: message delete failed; cursor advanced anyway",
			"messageId", msg.MessageID, "error", err)
	case !deleted:
		p.log.Warn("INVESTIGATION-F: the wire reported the message already gone, so nothing was "+
			"pruned; a later session may still replay it", "messageId", msg.MessageID)
	}
}

// cancelRun cancels a workflow run over the REST API with the installation token.
// This is the one call in the scenario outside the scale-set protocol, and the
// reason the App needs actions: write.
func (p *retentionProbe) cancelRun(ctx context.Context, owner, repo string, runID int64) error {
	return cancelWorkflowRun(ctx, cancelRunDeps{
		log: p.log, hc: p.hc, tokens: p.tokens, apiBase: p.apiBase, tag: "INVESTIGATION-F",
	}, owner, repo, runID)
}

// cancelRunDeps is what cancelWorkflowRun needs from a scenario: a token source, an
// HTTP client, the REST root, and the scenario's log tag.
type cancelRunDeps struct {
	log     *slog.Logger
	hc      *http.Client
	tokens  githubapp.TokenProvider
	apiBase string
	tag     string
}

// cancelWorkflowRun drives a workflow run terminal with no runner involved, which is
// how both Investigation F and Investigation G produce a JobCompleted for a job
// nothing ever ran.
//
// A 409 is success for this purpose: the run is already terminal, which is the state
// the cancel exists to reach.
func cancelWorkflowRun(ctx context.Context, d cancelRunDeps, owner, repo string, runID int64) error {
	token, err := d.tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("installation token: %w", err)
	}
	u := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/cancel",
		strings.TrimSuffix(d.apiBase, "/"), owner, repo, runID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(nil))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return nil
	case resp.StatusCode == http.StatusConflict:
		d.log.Info(d.tag+": run already terminal (409) — cancel is a no-op", "runId", runID)
		return nil
	default:
		return fmt.Errorf("status %d: %s", resp.StatusCode, githubapp.SanitizeBody(body, 256))
	}
}

// ── Check ────────────────────────────────────────────────────────────────────

// check opens a fresh session after the gap and reports whether the armed
// JobCompleted is still there.
func (p *retentionProbe) check(ctx context.Context) error {
	state, err := p.loadState()
	if err != nil {
		return err
	}
	if state.Concluded {
		p.log.Warn("INVESTIGATION-F: this experiment was already concluded by an earlier check; " +
			"its queue state has been observed before, so treat this result as exploratory")
	}
	gap := p.now().UTC().Sub(state.ArmedAt)
	p.log.Info("INVESTIGATION-F: checking after the gap",
		"gap", gap.Round(time.Second).String(),
		"armedAt", state.ArmedAt.Format(time.RFC3339),
		"jobId", state.JobID,
		"completedMessageId", state.CompletedMessageID)

	if _, err := p.client.GetRunnerScaleSet(ctx, state.ScaleSetID); err != nil {
		return fmt.Errorf("scale set %d is gone, so the queue log went with it and the gap "+
			"measured nothing: %w", state.ScaleSetID, err)
	}

	sess, err := p.client.CreateSession(ctx, state.ScaleSetID, retentionCheckOwner)
	if err != nil {
		return fmt.Errorf("create checking session: %w", err)
	}
	defer p.deleteSession(context.WithoutCancel(ctx), state.ScaleSetID, sess.SessionID)
	p.log.Info("INVESTIGATION-F: checking session created", "sessionId", sess.SessionID)

	result := retentionCheck{At: p.now().UTC(), GapSeconds: gap.Seconds(), Gap: gap.Round(time.Second).String()}
	completed, msgID, err := p.pollForCompletion(ctx, sess, state)
	switch {
	case err != nil:
		return err
	case completed != nil:
		result.Verdict = verdictRetained
		result.MessageID = msgID
		result.Result = completed.Result
		p.log.Info("INVESTIGATION-F: VERDICT RETAINED — GitHub redelivered the JobCompleted to a "+
			"session created after the gap; the Q435 replay path works at this gap",
			"gap", result.Gap, "messageId", msgID, "jobId", completed.JobID, "result", completed.Result)
	default:
		result.Verdict = verdictLost
		p.log.Warn("INVESTIGATION-F: VERDICT LOST — no JobCompleted for the armed job within the "+
			"check window; the Q435 replay path does not recover at this gap, leaving Q438's "+
			"maxWorkerLifetime as the only reclaim mechanism",
			"gap", result.Gap, "window", p.cfg.CheckWindow.String(), "jobId", state.JobID)
	}

	state.Checks = append(state.Checks, result)
	state.Concluded = !(result.Verdict == verdictRetained && p.cfg.KeepArmed)
	if err := p.writeState(state); err != nil {
		return err
	}
	if !state.Concluded {
		p.log.Info("INVESTIGATION-F: experiment left armed for a longer-gap check " +
			"(this check created a session; see the plan doc on why later rungs are exploratory)")
	}
	return nil
}

// pollForCompletion polls from the recorded cursor for the check window, returning
// the JobCompleted entry for the armed job if it is redelivered. It acknowledges
// nothing: leaving the message in place is what allows a longer-gap check.
func (p *retentionProbe) pollForCompletion(ctx context.Context, sess *scaleset.RunnerScaleSetSession,
	state *retentionState) (*scaleset.JobMessage, int64, error) {
	deadline, cancel := context.WithTimeout(ctx, p.cfg.CheckWindow)
	defer cancel()

	for {
		msg, err := p.pollOnce(deadline, sess, state.Cursor)
		if err != nil {
			return nil, 0, err
		}
		if msg == nil {
			if deadline.Err() != nil {
				return nil, 0, nil
			}
			continue
		}
		jobs, jErr := msg.Jobs()
		if jErr != nil {
			p.log.Warn("INVESTIGATION-F: message decode failed", "messageId", msg.MessageID, "error", jErr)
		}
		p.log.Info("INVESTIGATION-F: check observed message",
			"messageId", msg.MessageID, "entries", len(jobs),
			"body", githubapp.SanitizeBody([]byte(msg.Body), 512))
		if completed, ok := findCompleted(jobs, state.JobID); ok {
			return &completed, msg.MessageID, nil
		}
		// Something else is at the head of the queue. Step over it in the cursor
		// only — no delete, so nothing about the armed message changes — and keep
		// looking within the window.
		state.Cursor = msg.MessageID
	}
}

// ── Cleanup ──────────────────────────────────────────────────────────────────

// cleanup deletes the durable scale set, which takes its queue log with it. It
// works from the state file when there is one and falls back to a lookup by name,
// so an interrupted arm is still recoverable.
func (p *retentionProbe) cleanup(ctx context.Context) error {
	id := 0
	if state, err := p.loadState(); err == nil {
		id = state.ScaleSetID
	}
	if id == 0 {
		ss, err := p.client.GetRunnerScaleSetByName(ctx, p.cfg.ScaleSetName)
		if err != nil {
			return fmt.Errorf("lookup scale set %q: %w", p.cfg.ScaleSetName, err)
		}
		if ss == nil {
			p.log.Info("INVESTIGATION-F: cleanup — no scale set with that name", "name", p.cfg.ScaleSetName)
			return nil
		}
		id = ss.ID
	}
	if err := p.client.DeleteRunnerScaleSet(ctx, id); err != nil {
		return fmt.Errorf("delete scale set %d: %w", id, err)
	}
	p.log.Info("INVESTIGATION-F: cleanup — scale set deleted (its queue log with it)",
		"id", id, "state", p.cfg.StatePath)
	return nil
}

// ── Shared helpers ───────────────────────────────────────────────────────────

// pollOnce issues one long-poll, translating the deadline-expired case into
// (nil, nil) so callers distinguish "window over" from a real failure by checking
// the context themselves.
func (p *retentionProbe) pollOnce(ctx context.Context, sess *scaleset.RunnerScaleSetSession,
	cursor int64) (*scaleset.RunnerScaleSetMessage, error) {
	msg, err := p.client.GetMessage(ctx, sess, p.cfg.Capacity, cursor)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil
		}
		return nil, fmt.Errorf("queue poll: %w", err)
	}
	return msg, nil
}

// deleteSession tears a session down on a best-effort basis, logging failure. A
// session left behind would shorten the next gap without saying so, which is why
// the failure is reported rather than swallowed.
func (p *retentionProbe) deleteSession(ctx context.Context, scaleSetID int, sessionID string) {
	dCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := p.client.DeleteSession(dCtx, scaleSetID, sessionID); err != nil {
		p.log.Error("INVESTIGATION-F: delete session failed — a live session shortens any "+
			"subsequent gap; delete it before checking again",
			"sessionId", sessionID, "error", err)
		return
	}
	p.log.Info("INVESTIGATION-F: session deleted", "sessionId", sessionID)
}

// loadState reads the experiment from the state file.
func (p *retentionProbe) loadState() (*retentionState, error) {
	raw, err := os.ReadFile(p.cfg.StatePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no experiment state at %s — run PROBE_RETENTION_TEST=arm first",
				p.cfg.StatePath)
		}
		return nil, fmt.Errorf("read state %s: %w", p.cfg.StatePath, err)
	}
	var state retentionState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("decode state %s: %w", p.cfg.StatePath, err)
	}
	if state.ScaleSetID == 0 || state.JobID == "" || state.ArmedAt.IsZero() {
		return nil, fmt.Errorf("state %s is incomplete — re-arm the experiment", p.cfg.StatePath)
	}
	return &state, nil
}

// writeState persists the experiment. The file names a scale set and a workflow
// run, no credentials, so 0600 is caution rather than necessity.
func (p *retentionProbe) writeState(state *retentionState) error {
	if dir := filepath.Dir(p.cfg.StatePath); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create state directory %s: %w", dir, err)
		}
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(p.cfg.StatePath, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write state %s: %w", p.cfg.StatePath, err)
	}
	return nil
}
