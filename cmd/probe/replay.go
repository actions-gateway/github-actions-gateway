// Investigation G (Q583): does a cursor-acked message replay to a fresh session,
// and does DeleteMessage stop it?
//
// Set PROBE_REPLAY_TEST=true to run this scenario instead of the classic broker
// probe. It settles the two questions the Q583 fix rests on, which the existing
// evidence leaves open in opposite directions: the 2026-07-05 dogfood reconnect
// observed the replay but recorded no message ids and had no controlled
// before/after, and Investigation F acknowledged its JobAssigned in FULL (cursor
// and DeleteMessage), so its result cannot separate "gone because deleted" from
// "gone because cursor-acked".
//
// The shipping listener acks by cursor only. So the state this scenario stages is
// the one a live AGC leaves behind — every message acked by cursor, none deleted —
// and the question is what a restarted AGC then sees.
//
// One run, three session generations, each under a different owner name so the
// backend sees a new listener arriving rather than the same one resuming:
//
//	gen 1 → assign a real job, cursor-ack it WITHOUT deleting, cancel the run,
//	        cursor-ack its JobCompleted without deleting, delete the session.
//	gen 2 → poll from cursor 0. Does the assignment come back?      [measurement 1]
//	        Then DeleteMessage everything it replayed.              [measurement 2]
//	gen 3 → poll from cursor 0. Is it gone now?                     [measurement 3]
//
// Measurement 1 is Q583's premise under control. Measurements 2 and 3 are the
// DeleteMessage wire shape Q264 left as a P2-surfaced P4 unknown — and together
// they are the proof that delete-acking fixes the defect, taken before the fix is
// written rather than after.
//
// The three verdicts are independent and all three are reported. A DELETE-FAILED
// says nothing about whether the replay happened, and a PRUNED after a
// DELETE-FAILED means something other than the delete pruned the queue.
//
// Required environment variables:
//
//	GITHUB_APP_ID              - GitHub App numeric ID
//	GITHUB_APP_PRIVATE_KEY     - Path to PEM file, or PEM literal
//	GITHUB_APP_INSTALLATION_ID - Installation ID for the target org
//	GITHUB_ORG_URL             - Org or repo URL (e.g. https://github.com/my-org)
//
// The App needs actions: write — the scenario cancels the workflow run to drive
// the job terminal without a live runner, the same permission Investigation F
// needs for the same reason.
//
// Optional:
//
//	PROBE_REPLAY_NAME       - Scale set name (default gag-q583-replay).
//	PROBE_REPLAY_GROUP_NAME - Runner group (default Default).
//	PROBE_REPLAY_TIMEOUT    - How long to wait for a dispatched job (default 5m).
//	PROBE_REPLAY_WINDOW     - How long each replay poll runs before reporting
//	                          nothing came back (default 90s).
//	PROBE_REPLAY_CAPACITY   - Advertised poll capacity (default 1).
//	PROBE_REPLAY_KEEP       - "true" to leave the scale set registered on exit,
//	                          for a follow-up hand inspection. The queue log
//	                          survives with it.
//
// The Q583 plan doc carries the experiment's design and what would make a result
// invalid; developer-facing entry points are indexed from the credential-gated
// probe scenarios section of docs/development/testing.md.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/actions-gateway/github-actions-gateway/githubapp"
	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
)

// Session owner names, one per generation. They differ deliberately: gen 2 and gen 3
// must look to the backend like new listeners arriving, which is what a restarted AGC
// is, not like gen 1 resuming.
const (
	replayOwnerGen1 = "gag-q583-gen1"
	replayOwnerGen2 = "gag-q583-gen2"
	replayOwnerGen3 = "gag-q583-gen3"
)

// Verdicts, one per measurement. Each is logged with the evidence behind it.
const (
	verdictReplayed    = "REPLAYED"
	verdictNotReplayed = "NOT-REPLAYED"
	verdictDeleteOK    = "DELETE-OK"
	verdictDeleteFail  = "DELETE-FAILED"
	verdictPruned      = "PRUNED"
	verdictStillThere  = "STILL-REPLAYS"
)

// replayConfig holds the parsed environment for the replay scenario.
type replayConfig struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPEM  []byte
	ConfigURL      string

	ScaleSetName string
	GroupName    string

	// Timeout bounds the wait for a dispatched job to be assigned.
	Timeout time.Duration
	// Window bounds each from-cursor-0 poll. A poll that returns nothing within it
	// means "not redelivered in the budget a restarting AGC actually has", not
	// "provably deleted" — the same reading Investigation F's check window takes.
	Window time.Duration
	// Capacity is the advertised X-ScaleSetMaxCapacity. It stays at 1 so the
	// measurement sees what a restarted AGC with a free worker slot sees.
	Capacity int
	// Keep leaves the scale set registered on exit for hand inspection.
	Keep bool
}

// parseReplayConfig reads and validates the replay scenario environment from the
// injected getenv function (normally os.Getenv).
func parseReplayConfig(getenv func(string) string) (replayConfig, error) {
	var cfg replayConfig

	appIDStr, err := mustEnv(getenv, "GITHUB_APP_ID")
	if err != nil {
		return replayConfig{}, err
	}
	if _, err := fmt.Sscan(appIDStr, &cfg.AppID); err != nil {
		return replayConfig{}, fmt.Errorf("parse GITHUB_APP_ID: %w", err)
	}
	installIDStr, err := mustEnv(getenv, "GITHUB_APP_INSTALLATION_ID")
	if err != nil {
		return replayConfig{}, err
	}
	if _, err := fmt.Sscan(installIDStr, &cfg.InstallationID); err != nil {
		return replayConfig{}, fmt.Errorf("parse GITHUB_APP_INSTALLATION_ID: %w", err)
	}
	pemValue, err := mustEnv(getenv, "GITHUB_APP_PRIVATE_KEY")
	if err != nil {
		return replayConfig{}, err
	}
	cfg.PrivateKeyPEM, err = loadPEM(pemValue)
	if err != nil {
		return replayConfig{}, fmt.Errorf("load GITHUB_APP_PRIVATE_KEY: %w", err)
	}
	cfg.ConfigURL, err = mustEnv(getenv, "GITHUB_ORG_URL")
	if err != nil {
		return replayConfig{}, err
	}

	cfg.ScaleSetName = getenv("PROBE_REPLAY_NAME")
	if cfg.ScaleSetName == "" {
		cfg.ScaleSetName = "gag-q583-replay"
	}
	cfg.GroupName = getenv("PROBE_REPLAY_GROUP_NAME")
	if cfg.GroupName == "" {
		cfg.GroupName = "Default"
	}
	if cfg.Timeout, err = parseDurationEnv(getenv, "PROBE_REPLAY_TIMEOUT", 5*time.Minute); err != nil {
		return replayConfig{}, err
	}
	if cfg.Window, err = parseDurationEnv(getenv, "PROBE_REPLAY_WINDOW", 90*time.Second); err != nil {
		return replayConfig{}, err
	}
	cfg.Capacity = 1
	if v := getenv("PROBE_REPLAY_CAPACITY"); v != "" {
		if _, err := fmt.Sscan(v, &cfg.Capacity); err != nil {
			return replayConfig{}, fmt.Errorf("parse PROBE_REPLAY_CAPACITY: %w", err)
		}
	}
	cfg.Keep = getenv("PROBE_REPLAY_KEEP") == "true"
	return cfg, nil
}

// replayProbe carries the dependencies of the replay scenario. Like the other
// scenarios it drives the shipping scaleset.Client, so a live run is evidence about
// the code GAG ships rather than about a probe-local dialect of the protocol — which
// matters more here than anywhere else, because measurement 2 is specifically about
// whether Client.DeleteMessage's construction is the one the backend accepts.
type replayProbe struct {
	log *slog.Logger
	cfg replayConfig

	client *scaleset.Client
	// deletes carries the raw status of each DeleteMessage response, which is the
	// evidence measurement 2's verdict is recorded against — see deleteObserver.
	deletes *deleteObserver
	// hc serves the one call outside the scale-set protocol: the REST run-cancel
	// that drives the job terminal without a live runner.
	hc      *http.Client
	tokens  githubapp.TokenProvider
	apiBase string
}

// deleteObserver records the raw status of every DeleteMessage response on its way
// past the wire logger, so a verdict names the status the backend actually answered.
//
// The verdict itself turns on Client.DeleteMessage's first result, which separates a
// delete the wire performed from the 404/410 an unserved endpoint answers (Q609). The
// status is what that verdict is reported against — the same rule Investigation E
// states for its own reporting: the finding is what the wire did, not what the client
// made of it.
type deleteObserver struct {
	inner scaleset.ResponseObserver

	mu sync.Mutex
	// last is the status of the most recent DeleteMessage response, or 0 when none
	// has arrived since it was taken. Deletes are issued one at a time from a single
	// goroutine, so last is unambiguous without keying by message.
	last int
}

// ObserveResponse implements scaleset.ResponseObserver.
func (o *deleteObserver) ObserveResponse(info scaleset.ResponseInfo) {
	if o.inner != nil {
		o.inner.ObserveResponse(info)
	}
	if info.Op != "DeleteMessage" {
		return
	}
	o.mu.Lock()
	o.last = info.Status
	o.mu.Unlock()
}

// take returns the status of the last observed DeleteMessage and clears it, so a
// call that produced no response at all is distinguishable (0) from one that did.
func (o *deleteObserver) take() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	status := o.last
	o.last = 0
	return status
}

// staged is what gen 1 leaves behind for the later generations to look for.
type staged struct {
	ScaleSetID int
	JobID      string
	// AssignedMessageID and CompletedMessageID are cursor-acked and NOT deleted —
	// the state a live AGC leaves. Both are what gen 2 polls from 0 to find.
	AssignedMessageID  int64
	CompletedMessageID int64
	// Cursor is where gen 1 left off, recorded only so the log can show that gen 2
	// polling from 0 is reading behind it.
	Cursor int64
}

// newReplayProbe builds the scenario around a scaleset.Client wired to the probe's
// wire logger. Arguments as newScalesetProbe.
func newReplayProbe(logger *slog.Logger, cfg replayConfig, provider githubapp.TokenProvider,
	apiBase string, hc, pollClient *http.Client) (*replayProbe, error) {
	deletes := &deleteObserver{inner: wireLog{log: logger, tag: "G"}}
	client, err := scaleset.New(scaleset.Config{
		TokenProvider: provider,
		ConfigURL:     cfg.ConfigURL,
		APIBase:       apiBase,
		HTTPClient:    hc,
		PollClient:    pollClient,
		Observer:      deletes,
	})
	if err != nil {
		return nil, err
	}
	if hc == nil {
		hc = httpx.NewClient()
	}
	return &replayProbe{
		log: logger, cfg: cfg, client: client, deletes: deletes,
		hc: hc, tokens: provider, apiBase: apiBase,
	}, nil
}

// runReplayProbe is the Investigation G entry point wired from run().
func runReplayProbe(ctx context.Context, logger *slog.Logger, cfg replayConfig,
	provider githubapp.TokenProvider, apiBase string) error {
	p, err := newReplayProbe(logger, cfg, provider, apiBase, nil, nil)
	if err != nil {
		return err
	}
	return p.run(ctx)
}

func (p *replayProbe) run(ctx context.Context) error {
	if err := p.client.Connect(ctx); err != nil {
		return err
	}
	p.log.Info("INVESTIGATION-G: admin connection established")

	ss, err := p.ensureScaleSet(ctx)
	if err != nil {
		return err
	}
	if !p.cfg.Keep {
		// The scale set carries the queue log, so deleting it is what makes the run
		// leave nothing behind. On a WithoutCancel context: a Ctrl-C mid-run should
		// still clean up rather than strand a registered scale set.
		defer p.deleteScaleSet(context.WithoutCancel(ctx), ss.ID)
	}

	st, err := p.stage(ctx, ss.ID)
	if err != nil {
		return err
	}

	replayed, err := p.measureReplay(ctx, st)
	if err != nil {
		return err
	}
	if len(replayed) == 0 {
		p.log.Warn("INVESTIGATION-G: nothing replayed, so measurements 2 and 3 have no subject; " +
			"read the plan doc before concluding the defect is absent — this result contradicts " +
			"the 2026-07-05 dogfood observation, and the contradiction is the finding")
		return nil
	}
	if !p.measureDelete(ctx, st, replayed) {
		p.log.Warn("INVESTIGATION-G: skipping measurement 3 — a PRUNED verdict after a failed " +
			"delete would say something else pruned the queue, not that the delete worked")
		return nil
	}
	return p.measurePrune(ctx, st)
}

// ── Stage (gen 1) ────────────────────────────────────────────────────────────

// stage puts the queue into the state a live AGC leaves behind: a job assigned and
// concluded, both its messages acked by cursor, neither deleted, and no session.
func (p *replayProbe) stage(ctx context.Context, scaleSetID int) (*staged, error) {
	sess, err := p.client.CreateSession(ctx, scaleSetID, replayOwnerGen1)
	if err != nil {
		return nil, fmt.Errorf("create gen-1 session: %w", err)
	}
	p.log.Info("INVESTIGATION-G: gen-1 session created",
		"sessionId", sess.SessionID, "owner", replayOwnerGen1)

	st := &staged{ScaleSetID: scaleSetID}
	assigned, owner, repo, runID, err := p.awaitAssignment(ctx, scaleSetID, sess, st)
	if err != nil {
		p.deleteSession(context.WithoutCancel(ctx), scaleSetID, sess.SessionID)
		return nil, err
	}
	st.JobID = assigned.JobID

	if err := cancelWorkflowRun(ctx, cancelRunDeps{
		log: p.log, hc: p.hc, tokens: p.tokens, apiBase: p.apiBase, tag: "INVESTIGATION-G",
	}, owner, repo, runID); err != nil {
		p.deleteSession(context.WithoutCancel(ctx), scaleSetID, sess.SessionID)
		return nil, fmt.Errorf("cancel run %d: %w", runID, err)
	}
	p.log.Info("INVESTIGATION-G: workflow run cancelled",
		"owner", owner, "repo", repo, "runId", runID, "jobId", st.JobID)

	if err := p.awaitCompletion(ctx, sess, st); err != nil {
		p.deleteSession(context.WithoutCancel(ctx), scaleSetID, sess.SessionID)
		return nil, err
	}

	if err := p.client.DeleteSession(ctx, scaleSetID, sess.SessionID); err != nil {
		return nil, fmt.Errorf("delete gen-1 session (the queue still has a listener, so a "+
			"later poll would not be measuring a restart): %w", err)
	}
	p.log.Info("INVESTIGATION-G: STAGED — job concluded, both messages cursor-acked and NOT "+
		"deleted, no session exists",
		"jobId", st.JobID, "assignedMessageId", st.AssignedMessageID,
		"completedMessageId", st.CompletedMessageID, "cursor", st.Cursor)
	return st, nil
}

// awaitAssignment polls until a job is assigned, acquiring it first on the GHES offer
// flow. Every message is acked by CURSOR ONLY — no delete — because that is what the
// shipping listener does and what the whole scenario is about.
func (p *replayProbe) awaitAssignment(ctx context.Context, scaleSetID int,
	sess *scaleset.RunnerScaleSetSession, st *staged) (*scaleset.JobMessage, string, string, int64, error) {
	p.log.Info("INVESTIGATION-G: dispatch a workflow with runs-on: "+p.cfg.ScaleSetName+" NOW",
		"waiting", p.cfg.Timeout.String())
	deadline, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	for {
		msg, err := p.pollOnce(deadline, sess, st.Cursor)
		if err != nil {
			return nil, "", "", 0, err
		}
		if msg == nil {
			if deadline.Err() != nil {
				return nil, "", "", 0, fmt.Errorf(
					"no job assigned within %s — was a workflow dispatched to %q?",
					p.cfg.Timeout, p.cfg.ScaleSetName)
			}
			continue
		}
		jobs, jErr := msg.Jobs()
		if jErr != nil {
			p.log.Warn("INVESTIGATION-G: message decode failed", "messageId", msg.MessageID, "error", jErr)
		}
		p.log.Info("INVESTIGATION-G: gen-1 observed message",
			"messageId", msg.MessageID, "entries", len(jobs),
			"body", githubapp.SanitizeBody([]byte(msg.Body), 512))

		assigned := scaleset.AssignedJobs(jobs)
		st.Cursor = msg.MessageID

		if len(assigned) > 0 {
			a := assigned[0]
			owner, repo, _, ok := a.RunIdentity()
			if !ok {
				return nil, "", "", 0, fmt.Errorf(
					"JobAssigned for job %s carries no complete run identity (owner=%q repo=%q runId=%d); "+
						"the scenario cannot name a run to cancel",
					a.JobID, a.OwnerName, a.RepositoryName, a.WorkflowRunID)
			}
			st.AssignedMessageID = msg.MessageID
			p.log.Info("INVESTIGATION-G: job assigned; cursor advanced past it, NOT deleted",
				"jobId", a.JobID, "messageId", msg.MessageID,
				"owner", owner, "repo", repo, "runId", a.WorkflowRunID)
			return &a, owner, repo, a.WorkflowRunID, nil
		}
		if ids := scaleset.AvailableJobIDs(jobs); len(ids) > 0 {
			won, aErr := p.client.AcquireJobs(deadline, scaleSetID, sess, ids)
			if aErr != nil {
				p.log.Warn("INVESTIGATION-G: acquirejobs failed", "requested", ids, "error", aErr)
				continue
			}
			p.log.Info("INVESTIGATION-G: acquired offered jobs", "requested", ids, "won", won)
		}
	}
}

// awaitCompletion polls until the staged job's JobCompleted arrives and advances the
// cursor past it — again without deleting.
func (p *replayProbe) awaitCompletion(ctx context.Context,
	sess *scaleset.RunnerScaleSetSession, st *staged) error {
	deadline, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	for {
		msg, err := p.pollOnce(deadline, sess, st.Cursor)
		if err != nil {
			return err
		}
		if msg == nil {
			if deadline.Err() != nil {
				return fmt.Errorf("no JobCompleted for job %s within %s of cancelling its run",
					st.JobID, p.cfg.Timeout)
			}
			continue
		}
		jobs, jErr := msg.Jobs()
		if jErr != nil {
			p.log.Warn("INVESTIGATION-G: message decode failed", "messageId", msg.MessageID, "error", jErr)
		}
		p.log.Info("INVESTIGATION-G: gen-1 observed message",
			"messageId", msg.MessageID, "entries", len(jobs),
			"body", githubapp.SanitizeBody([]byte(msg.Body), 512))
		st.Cursor = msg.MessageID

		if completed, ok := findCompleted(jobs, st.JobID); ok {
			st.CompletedMessageID = msg.MessageID
			p.log.Info("INVESTIGATION-G: job completed; cursor advanced past it, NOT deleted",
				"jobId", completed.JobID, "messageId", msg.MessageID, "result", completed.Result)
			return nil
		}
	}
}

// ── Measurement 1 (gen 2) ────────────────────────────────────────────────────

// measureReplay opens a fresh session under a new owner name and polls from cursor 0
// — what a restarted AGC does — reporting which of the staged messages come back.
func (p *replayProbe) measureReplay(ctx context.Context, st *staged) ([]int64, error) {
	sess, err := p.client.CreateSession(ctx, st.ScaleSetID, replayOwnerGen2)
	if err != nil {
		return nil, fmt.Errorf("create gen-2 session: %w", err)
	}
	defer p.deleteSession(context.WithoutCancel(ctx), st.ScaleSetID, sess.SessionID)
	p.log.Info("INVESTIGATION-G: gen-2 session created — polling from cursor 0, as a restarted AGC does",
		"sessionId", sess.SessionID, "owner", replayOwnerGen2, "gen1Cursor", st.Cursor)

	seen, err := p.drainFromZero(ctx, sess, "gen-2")
	if err != nil {
		return nil, err
	}
	sawAssigned := containsID(seen, st.AssignedMessageID)
	switch {
	case sawAssigned:
		p.log.Info("INVESTIGATION-G: VERDICT 1 "+verdictReplayed+" — the JobAssigned gen 1 acked by "+
			"cursor came back to a session that never saw it. A restarted AGC, whose provisioned/"+
			"completed/abandoned guards are all empty, provisions a worker for this concluded job",
			"assignedMessageId", st.AssignedMessageID, "jobId", st.JobID, "replayed", seen)
	default:
		p.log.Warn("INVESTIGATION-G: VERDICT 1 "+verdictNotReplayed+" — the cursor-acked JobAssigned "+
			"did not come back within the window; see the plan doc, this contradicts the 2026-07-05 "+
			"dogfood observation and the contradiction is the finding",
			"assignedMessageId", st.AssignedMessageID, "window", p.cfg.Window.String(), "replayed", seen)
	}
	return seen, nil
}

// ── Measurement 2 (delete) ───────────────────────────────────────────────────

// measureDelete issues Client.DeleteMessage for each replayed message and reports
// whether the wire accepted the construction — the P4 unknown Q264 left open. It
// returns true only when every delete succeeded, because a partial result cannot
// support measurement 3.
func (p *replayProbe) measureDelete(ctx context.Context, st *staged, ids []int64) bool {
	sess, err := p.client.CreateSession(ctx, st.ScaleSetID, replayOwnerGen2)
	if err != nil {
		p.log.Error("INVESTIGATION-G: VERDICT 2 "+verdictDeleteFail+" — could not open a session to "+
			"delete from", "error", err)
		return false
	}
	defer p.deleteSession(context.WithoutCancel(ctx), st.ScaleSetID, sess.SessionID)

	ok := true
	for _, id := range ids {
		deleted, err := p.client.DeleteMessage(ctx, sess, id)
		status := p.deletes.take()
		switch {
		case err != nil:
			p.log.Error("INVESTIGATION-G: VERDICT 2 "+verdictDeleteFail+" — DeleteMessage rejected; "+
				"the source-derived wire shape is wrong or the endpoint is not there. Delete-acking "+
				"cannot be the Q583 fix; fall back to persisting the cursor",
				"messageId", id, "status", status, "error", err)
			ok = false
		case status == 0:
			p.log.Error("INVESTIGATION-G: VERDICT 2 "+verdictDeleteFail+" — no DeleteMessage response "+
				"was observed at all, so there is no wire evidence to read a verdict from",
				"messageId", id)
			ok = false
		case !deleted:
			// A 404/410, which completes an ack for a listener but here says the wire
			// removed nothing — the branch that separates "the endpoint took the delete"
			// from "the endpoint is not there".
			p.log.Error("INVESTIGATION-G: VERDICT 2 "+verdictDeleteFail+" — the wire reported the "+
				"message already gone rather than deleting it. The endpoint is not served at the "+
				"shape the client constructs; delete-acking cannot be the Q583 fix",
				"messageId", id, "status", status)
			ok = false
		default:
			p.log.Info("INVESTIGATION-G: DeleteMessage accepted", "messageId", id, "status", status)
		}
	}
	if ok {
		p.log.Info("INVESTIGATION-G: VERDICT 2 "+verdictDeleteOK+" — the DeleteMessage wire shape is "+
			"confirmed live, closing the P2-surfaced P4 unknown from Q264", "messageIds", ids)
	}
	return ok
}

// ── Measurement 3 (gen 3) ────────────────────────────────────────────────────

// measurePrune opens a third session and polls from cursor 0 again. This is the one
// that says whether delete-acking actually fixes Q583: a message that no longer
// replays to a brand-new session is a worker a restarted AGC no longer provisions.
func (p *replayProbe) measurePrune(ctx context.Context, st *staged) error {
	sess, err := p.client.CreateSession(ctx, st.ScaleSetID, replayOwnerGen3)
	if err != nil {
		return fmt.Errorf("create gen-3 session: %w", err)
	}
	defer p.deleteSession(context.WithoutCancel(ctx), st.ScaleSetID, sess.SessionID)
	p.log.Info("INVESTIGATION-G: gen-3 session created — polling from cursor 0 after the deletes",
		"sessionId", sess.SessionID, "owner", replayOwnerGen3)

	seen, err := p.drainFromZero(ctx, sess, "gen-3")
	if err != nil {
		return err
	}
	if containsID(seen, st.AssignedMessageID) {
		p.log.Warn("INVESTIGATION-G: VERDICT 3 "+verdictStillThere+" — the JobAssigned replayed again "+
			"after a successful delete, so DeleteMessage does not prune the queue log and delete-"+
			"acking would not fix Q583",
			"assignedMessageId", st.AssignedMessageID, "replayed", seen)
		return nil
	}
	p.log.Info("INVESTIGATION-G: VERDICT 3 "+verdictPruned+" — the deleted messages did not replay to "+
		"a third session. Delete-acking prunes the queue, so a restarted AGC reads only genuinely "+
		"unfinished work: this is the Q583 fix, measured before it is built",
		"assignedMessageId", st.AssignedMessageID, "replayed", seen)
	return nil
}

// ── Shared helpers ───────────────────────────────────────────────────────────

// drainFromZero polls from cursor 0 for the configured window, stepping the cursor
// over each message it receives so the whole retained log is read rather than the
// head alone. It returns every message id delivered, which is the evidence both
// replay verdicts are read from.
//
// It acknowledges nothing: no delete, and the cursor it steps is its own local
// variable, so the queue is in the same state it found.
func (p *replayProbe) drainFromZero(ctx context.Context,
	sess *scaleset.RunnerScaleSetSession, gen string) ([]int64, error) {
	deadline, cancel := context.WithTimeout(ctx, p.cfg.Window)
	defer cancel()

	var seen []int64
	cursor := int64(0)
	for {
		msg, err := p.pollOnce(deadline, sess, cursor)
		if err != nil {
			return seen, err
		}
		if msg == nil {
			// Window over, or the queue has nothing past the cursor. Either way the
			// log has been read as far as it goes.
			return seen, nil
		}
		jobs, jErr := msg.Jobs()
		if jErr != nil {
			p.log.Warn("INVESTIGATION-G: message decode failed", "messageId", msg.MessageID, "error", jErr)
		}
		p.log.Info("INVESTIGATION-G: "+gen+" replayed message",
			"messageId", msg.MessageID, "entries", len(jobs),
			"body", githubapp.SanitizeBody([]byte(msg.Body), 512))
		seen = append(seen, msg.MessageID)
		cursor = msg.MessageID
	}
}

// containsID reports whether id is in ids. A zero id is never present: it means the
// scenario never staged that message, and treating it as found would turn a staging
// failure into a verdict.
func containsID(ids []int64, id int64) bool {
	if id == 0 {
		return false
	}
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

// ensureScaleSet looks the scale set up by name and creates it when it is not there,
// so a run after an interrupted one reuses what is already registered.
func (p *replayProbe) ensureScaleSet(ctx context.Context) (*scaleset.RunnerScaleSet, error) {
	existing, err := p.client.GetRunnerScaleSetByName(ctx, p.cfg.ScaleSetName)
	if err != nil {
		return nil, fmt.Errorf("lookup scale set %q: %w", p.cfg.ScaleSetName, err)
	}
	if existing != nil {
		p.log.Warn("INVESTIGATION-G: reusing an existing scale set — its queue log carries whatever "+
			"a previous run left, so the replay verdicts may see messages this run did not stage",
			"id", existing.ID, "name", existing.Name)
		return existing, nil
	}
	groupID := 1
	if id, ok, gErr := p.client.ResolveRunnerGroup(ctx, p.cfg.GroupName); gErr != nil {
		p.log.Warn("INVESTIGATION-G: runnergroups lookup failed; falling back to group id 1", "error", gErr)
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
	p.log.Info("INVESTIGATION-G: scale set created",
		"id", ss.ID, "name", ss.Name, "runnerGroupId", ss.RunnerGroupID)
	return ss, nil
}

// pollOnce issues one long-poll, translating the deadline-expired case into
// (nil, nil) so callers distinguish "window over" from a real failure.
func (p *replayProbe) pollOnce(ctx context.Context, sess *scaleset.RunnerScaleSetSession,
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

// deleteSession tears a session down on a best-effort basis. A session left behind
// makes the next generation look like a resumption rather than an arrival, which is
// the one thing this scenario cannot tolerate — so the failure is reported loudly.
func (p *replayProbe) deleteSession(ctx context.Context, scaleSetID int, sessionID string) {
	dCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := p.client.DeleteSession(dCtx, scaleSetID, sessionID); err != nil {
		p.log.Error("INVESTIGATION-G: delete session failed — the next generation is not a fresh "+
			"listener arriving, so treat its verdict as invalid",
			"sessionId", sessionID, "error", err)
		return
	}
	p.log.Info("INVESTIGATION-G: session deleted", "sessionId", sessionID)
}

// deleteScaleSet removes the scale set, taking its queue log with it.
func (p *replayProbe) deleteScaleSet(ctx context.Context, scaleSetID int) {
	dCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := p.client.DeleteRunnerScaleSet(dCtx, scaleSetID); err != nil {
		p.log.Error("INVESTIGATION-G: delete scale set failed — it is still registered and its "+
			"queue log with it; delete it by hand or a later run will reuse both",
			"id", scaleSetID, "error", err)
		return
	}
	p.log.Info("INVESTIGATION-G: scale set deleted (its queue log with it)", "id", scaleSetID)
}
