package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
)

// What these tests can and cannot establish. The stub implements the queue
// semantics we BELIEVE the backend has — a scale-set-scoped log, cursor replay to a
// fresh session, a delete that prunes — so a green suite here means the scenario
// drives the protocol correctly and reads its own evidence correctly. It cannot mean
// the backend behaves that way; that is what the live run is for, and it is the only
// thing Investigation G exists to answer.
//
// What they do protect is the part a live run cannot re-check cheaply: that a
// verdict is reported from the evidence rather than from an assumption, and that the
// dependent measurements refuse to run when their premise failed.

// replayEnv builds a getenv function over a map with the credentials filled in, so a
// test states only what it varies.
func replayEnv(overrides map[string]string) func(string) string {
	env := map[string]string{
		"GITHUB_APP_ID":              "42",
		"GITHUB_APP_INSTALLATION_ID": "99",
		// A PEM literal rather than a path, exercising loadPEM's inline branch.
		"GITHUB_APP_PRIVATE_KEY": "-----BEGIN TEST PEM-----\nnot-a-key\n-----END TEST PEM-----",
		"GITHUB_ORG_URL":         "https://github.com/test-org",
	}
	for k, v := range overrides {
		env[k] = v
	}
	return func(k string) string { return env[k] }
}

// newReplayProbeForTest builds a replay probe against the stub at test-scale timings.
// Its log is captured because the verdicts ARE log lines: that is where the evidence
// has to be checked.
func newReplayProbeForTest(t *testing.T, srv *scalesettest.Server, cfg replayConfig, w io.Writer) *replayProbe {
	t.Helper()
	if cfg.ConfigURL == "" {
		cfg.ConfigURL = "https://github.com/test-org"
	}
	if cfg.ScaleSetName == "" {
		cfg.ScaleSetName = "gag-q583-replay"
	}
	if cfg.GroupName == "" {
		cfg.GroupName = "Default"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Window == 0 {
		cfg.Window = 500 * time.Millisecond
	}
	if cfg.Capacity == 0 {
		cfg.Capacity = 1
	}
	p, err := newReplayProbe(
		slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})),
		cfg,
		staticTokenProvider{token: installToken},
		srv.URL,
		srv.HTTPClient(),
		srv.HTTPClient(),
	)
	if err != nil {
		t.Fatalf("newReplayProbe: %v", err)
	}
	if err := p.client.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return p
}

// stageAgainstStub runs the probe through gen 1 against a stub holding one queued
// job, returning the staged state and the log.
func stageAgainstStub(t *testing.T, srv *scalesettest.Server) (*replayProbe, *staged, string) {
	t.Helper()
	srv.PrequeueJobs(1)
	var log strings.Builder
	p := newReplayProbeForTest(t, srv, replayConfig{}, &log)
	ss, err := p.ensureScaleSet(context.Background())
	if err != nil {
		t.Fatalf("ensureScaleSet: %v\n%s", err, log.String())
	}
	st, err := p.stage(context.Background(), ss.ID)
	if err != nil {
		t.Fatalf("stage: %v\n%s", err, log.String())
	}
	return p, st, log.String()
}

func TestParseReplayConfig_Defaults(t *testing.T) {
	cfg, err := parseReplayConfig(replayEnv(nil))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.ScaleSetName != "gag-q583-replay" {
		t.Errorf("scale set name = %q", cfg.ScaleSetName)
	}
	if cfg.Timeout != 5*time.Minute || cfg.Window != 90*time.Second {
		t.Errorf("timings = %s/%s", cfg.Timeout, cfg.Window)
	}
	if cfg.Capacity != 1 {
		t.Errorf("capacity = %d, want 1", cfg.Capacity)
	}
	if cfg.Keep {
		t.Error("Keep defaults true, want false — a run would strand a scale set")
	}
}

func TestParseReplayConfig_Overrides(t *testing.T) {
	cfg, err := parseReplayConfig(replayEnv(map[string]string{
		"PROBE_REPLAY_NAME":     "custom",
		"PROBE_REPLAY_TIMEOUT":  "2m",
		"PROBE_REPLAY_WINDOW":   "30s",
		"PROBE_REPLAY_CAPACITY": "3",
		"PROBE_REPLAY_KEEP":     "true",
	}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.ScaleSetName != "custom" || cfg.Timeout != 2*time.Minute ||
		cfg.Window != 30*time.Second || cfg.Capacity != 3 || !cfg.Keep {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

// TestParseReplayConfig_NamesTheMissingVariable asserts a missing credential is
// reported as itself rather than surfacing later as an auth failure against GitHub.
func TestParseReplayConfig_NamesTheMissingVariable(t *testing.T) {
	for _, missing := range []string{
		"GITHUB_APP_ID", "GITHUB_APP_INSTALLATION_ID", "GITHUB_APP_PRIVATE_KEY", "GITHUB_ORG_URL",
	} {
		_, err := parseReplayConfig(replayEnv(map[string]string{missing: ""}))
		if err == nil {
			t.Errorf("%s missing was accepted", missing)
			continue
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("error for missing %s does not name it: %v", missing, err)
		}
	}
}

func TestParseReplayConfig_RejectsNonNumericIDs(t *testing.T) {
	for _, v := range []string{"GITHUB_APP_ID", "GITHUB_APP_INSTALLATION_ID"} {
		if _, err := parseReplayConfig(replayEnv(map[string]string{v: "not-a-number"})); err == nil {
			t.Errorf("%s accepted a non-numeric value", v)
		}
	}
}

func TestParseReplayConfig_RejectsBadDuration(t *testing.T) {
	if _, err := parseReplayConfig(replayEnv(map[string]string{"PROBE_REPLAY_WINDOW": "ninety"})); err == nil {
		t.Fatal("bad duration accepted, want rejected")
	}
}

// TestReplay_StageLeavesBothMessagesUndeleted asserts the state the measurement
// reads from. Both messages must still be in the queue and the cursor must be past
// them: that combination is what makes gen 2 a test of cursor-acked replay rather
// than of ordinary unacked redelivery, which Investigation F already answered.
func TestReplay_StageLeavesBothMessagesUndeleted(t *testing.T) {
	srv := newScalesetStub(t)
	_, st, log := stageAgainstStub(t, srv)

	if st.JobID == "" {
		t.Fatalf("no job staged\n%s", log)
	}
	if st.AssignedMessageID == 0 || st.CompletedMessageID == 0 {
		t.Fatalf("staged ids incomplete: %+v\n%s", st, log)
	}
	if st.Cursor < st.CompletedMessageID {
		t.Errorf("cursor %d is not past the completion %d — gen 1 did not ack what a listener would",
			st.Cursor, st.CompletedMessageID)
	}
	// The whole point: cursor-acked, NOT deleted. A delete here would make gen 2
	// measure the deletion instead of the replay.
	for _, call := range srv.Calls() {
		if strings.HasPrefix(call, "delete-message") {
			t.Errorf("gen 1 issued a delete (%q); it must ack by cursor only", call)
		}
	}
	if srv.HasActiveSession(st.ScaleSetID) {
		t.Error("gen 1 left a session behind, so a later generation is a resumption not an arrival")
	}
}

// TestReplay_ReportsAllThreeVerdicts drives the full scenario against a stub whose
// queue behaves as we believe GitHub's does, and asserts each verdict is reported
// from what the poll actually returned.
func TestReplay_ReportsAllThreeVerdicts(t *testing.T) {
	srv := newScalesetStub(t)
	srv.PrequeueJobs(1)
	var log strings.Builder
	p := newReplayProbeForTest(t, srv, replayConfig{}, &log)
	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	out := log.String()

	for _, want := range []string{
		"VERDICT 1 " + verdictReplayed,
		"VERDICT 2 " + verdictDeleteOK,
		"VERDICT 3 " + verdictPruned,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in probe log:\n%s", want, out)
		}
	}
	// Each measuring generation must open its session under its own owner name, or
	// the backend sees a resumption and the measurement is of something else.
	for _, owner := range []string{replayOwnerGen1, replayOwnerGen2, replayOwnerGen3} {
		if !strings.Contains(out, "owner="+owner) {
			t.Errorf("no session was created under generation owner %q:\n%s", owner, out)
		}
	}
}

// TestReplay_NotReplayedWhenTheQueueIsEmpty drives the negative branch: with nothing
// in the log, measurement 1 must report NOT-REPLAYED rather than defaulting to the
// answer the scenario expects.
func TestReplay_NotReplayedWhenTheQueueIsEmpty(t *testing.T) {
	srv := newScalesetStub(t)
	var log strings.Builder
	p := newReplayProbeForTest(t, srv, replayConfig{}, &log)
	ss, err := p.ensureScaleSet(context.Background())
	if err != nil {
		t.Fatalf("ensureScaleSet: %v", err)
	}
	// A staged message id that was never enqueued: the queue cannot replay it.
	st := &staged{ScaleSetID: ss.ID, JobID: "never-staged", AssignedMessageID: 999999}

	seen, err := p.measureReplay(context.Background(), st)
	if err != nil {
		t.Fatalf("measureReplay: %v\n%s", err, log.String())
	}
	if len(seen) != 0 {
		t.Errorf("replayed %v from an empty queue", seen)
	}
	if !strings.Contains(log.String(), "VERDICT 1 "+verdictNotReplayed) {
		t.Errorf("expected a %s verdict:\n%s", verdictNotReplayed, log.String())
	}
}

// TestReplay_DeleteFailureSkipsThePruneMeasurement is the guard that keeps the
// scenario honest: measurement 3 reads "did it stop replaying", which is only
// evidence about DeleteMessage if the delete succeeded. A failed delete must abort
// the run rather than produce a PRUNED verdict off an unrelated cause.
func TestReplay_DeleteFailureSkipsThePruneMeasurement(t *testing.T) {
	srv := newScalesetStub(t)
	srv.PrequeueJobs(1)
	var log strings.Builder
	p := newReplayProbeForTest(t, srv, replayConfig{}, &log)
	srv.FailDeleteMessage(http.StatusNotFound)

	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	out := log.String()

	if !strings.Contains(out, "VERDICT 2 "+verdictDeleteFail) {
		t.Errorf("expected a %s verdict:\n%s", verdictDeleteFail, out)
	}
	if strings.Contains(out, "VERDICT 3") {
		t.Errorf("measurement 3 ran after a failed delete; its result would be unattributable:\n%s", out)
	}
}

// TestReplay_RunDeletesTheScaleSet asserts the run leaves nothing registered — the
// queue log lives on the scale set, so a leaked one poisons the next run's verdicts
// with messages it did not stage.
func TestReplay_RunDeletesTheScaleSet(t *testing.T) {
	srv := newScalesetStub(t)
	srv.PrequeueJobs(1)
	var log strings.Builder
	p := newReplayProbeForTest(t, srv, replayConfig{}, &log)
	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	if _, ok := srv.ScaleSetIDByName("gag-q583-replay"); ok {
		t.Errorf("scale set still registered after the run:\n%s", log.String())
	}
}

// TestReplay_EntryPointRunsTheWholeScenario covers runReplayProbe, the path a live
// invocation actually takes — config in hand, no test-built probe.
func TestReplay_EntryPointRunsTheWholeScenario(t *testing.T) {
	srv := newScalesetStub(t)
	srv.PrequeueJobs(1)
	var log strings.Builder
	logger := slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := replayConfig{
		ConfigURL:    "https://github.com/test-org",
		ScaleSetName: "gag-q583-replay",
		GroupName:    "Default",
		Timeout:      10 * time.Second,
		Window:       500 * time.Millisecond,
		Capacity:     1,
	}
	err := runReplayProbe(context.Background(), logger, cfg,
		staticTokenProvider{token: installToken}, srv.URL)
	if err != nil {
		t.Fatalf("runReplayProbe: %v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "VERDICT 1 "+verdictReplayed) {
		t.Errorf("entry point produced no verdict 1:\n%s", log.String())
	}
}

// TestReplay_ReusedScaleSetIsFlagged covers the hazard a leaked scale set creates: its
// queue log carries messages this run did not stage, so the replay verdicts would be
// read off someone else's evidence. The run continues — reuse is how an interrupted
// run recovers — but it must say so.
func TestReplay_ReusedScaleSetIsFlagged(t *testing.T) {
	srv := newScalesetStub(t)
	srv.AddScaleSet("gag-q583-replay", 1)
	var log strings.Builder
	p := newReplayProbeForTest(t, srv, replayConfig{}, &log)

	if _, err := p.ensureScaleSet(context.Background()); err != nil {
		t.Fatalf("ensureScaleSet: %v", err)
	}
	if !strings.Contains(log.String(), "reusing an existing scale set") {
		t.Errorf("reuse of a pre-existing scale set was not flagged:\n%s", log.String())
	}
}

// TestReplay_NoDispatchedJobFailsTheRun asserts the scenario stops with a diagnosis
// rather than proceeding to measure an empty queue, which would report NOT-REPLAYED
// and read as evidence about GitHub instead of about a missing fixture dispatch.
func TestReplay_NoDispatchedJobFailsTheRun(t *testing.T) {
	srv := newScalesetStub(t)
	var log strings.Builder
	p := newReplayProbeForTest(t, srv, replayConfig{Timeout: 150 * time.Millisecond}, &log)

	err := p.run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded with no job dispatched:\n%s", log.String())
	}
	if !strings.Contains(err.Error(), "was a workflow dispatched") {
		t.Errorf("error does not point at the missing dispatch: %v", err)
	}
	if strings.Contains(log.String(), "VERDICT") {
		t.Errorf("a verdict was reported from a run that never staged anything:\n%s", log.String())
	}
}

// TestReplay_StagesOverTheGHESAcquireFlow covers the other acquisition path: on GHES a
// job arrives as an offer to claim, not as an assignment, and a scenario that only
// handled the dotcom auto-assign shape would hang there.
func TestReplay_StagesOverTheGHESAcquireFlow(t *testing.T) {
	srv := newScalesetStub(t)
	srv.EnableGHESAcquireFlow()
	_, st, log := stageAgainstStub(t, srv)

	if st.AssignedMessageID == 0 || st.CompletedMessageID == 0 {
		t.Fatalf("GHES flow did not stage both messages: %+v\n%s", st, log)
	}
}

// TestContainsID_ZeroIsNeverPresent guards the seam between staging and verdict: a
// zero id means the scenario never staged that message, and treating it as found
// would turn a staging failure into a REPLAYED verdict.
func TestContainsID_ZeroIsNeverPresent(t *testing.T) {
	if containsID([]int64{0, 1, 2}, 0) {
		t.Error("a zero id was reported as present")
	}
	if !containsID([]int64{1, 2}, 2) {
		t.Error("a present id was reported missing")
	}
	if containsID(nil, 5) {
		t.Error("an id was found in an empty replay")
	}
}

// TestReplay_AcceptedDeleteThatDoesNotPruneStillReplays is the verdict that would
// rule delete-acking out as the Q583 fix, and it is the reason measurement 3 exists
// at all: a backend can take the DELETE, answer 204, and leave the message in the
// log. Measurement 2 cannot see that — only a third session polling from 0 can.
func TestReplay_AcceptedDeleteThatDoesNotPruneStillReplays(t *testing.T) {
	srv := newScalesetStub(t)
	srv.PrequeueJobs(1)
	var log strings.Builder
	p := newReplayProbeForTest(t, srv, replayConfig{}, &log)
	srv.AcceptDeleteWithoutPruning(true)

	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	out := log.String()

	// The delete was accepted on the wire, so measurement 2 is satisfied...
	if !strings.Contains(out, "VERDICT 2 "+verdictDeleteOK) {
		t.Errorf("expected %s from an accepted delete:\n%s", verdictDeleteOK, out)
	}
	// ...and measurement 3 is what catches that it did nothing.
	if !strings.Contains(out, "VERDICT 3 "+verdictStillThere) {
		t.Errorf("expected %s — an accepted-but-ignored delete must not read as PRUNED:\n%s",
			verdictStillThere, out)
	}
}

// TestDeleteObserver_ReadsTheWireNotTheError covers the seam verdict 2 rests on:
// Client.DeleteMessage reports a 404 as success, so the status has to survive the
// trip from the response to the verdict — and each take must be attributable to one
// delete rather than lingering into the next.
func TestDeleteObserver_ReadsTheWireNotTheError(t *testing.T) {
	o := &deleteObserver{}
	if got := o.take(); got != 0 {
		t.Errorf("take with nothing observed = %d, want 0", got)
	}
	o.ObserveResponse(scaleset.ResponseInfo{Op: "GetMessage", Status: 200})
	if got := o.take(); got != 0 {
		t.Errorf("a non-delete response was recorded as a delete status (%d)", got)
	}
	o.ObserveResponse(scaleset.ResponseInfo{Op: "DeleteMessage", Status: 404})
	if got := o.take(); got != 404 {
		t.Errorf("delete status = %d, want 404", got)
	}
	if got := o.take(); got != 0 {
		t.Errorf("status %d lingered into the next take; it would be read as a second delete", got)
	}
}

// TestReplay_SessionCreateFailureAbortsBeforeAnyVerdict asserts revoked credentials
// stop the run rather than producing verdicts about a queue that was never read.
func TestReplay_SessionCreateFailureAbortsBeforeAnyVerdict(t *testing.T) {
	srv := newScalesetStub(t)
	srv.PrequeueJobs(1)
	var log strings.Builder
	p := newReplayProbeForTest(t, srv, replayConfig{}, &log)
	srv.FailSessionCreate(true)

	err := p.run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded with session create failing:\n%s", log.String())
	}
	if !strings.Contains(err.Error(), "gen-1 session") {
		t.Errorf("error does not name the failing step: %v", err)
	}
	if strings.Contains(log.String(), "VERDICT") {
		t.Errorf("a verdict was reported from a run with no session:\n%s", log.String())
	}
}

// TestReplay_PollErrorSurfaces asserts a rejected poll fails the run instead of being
// read as an empty queue — which would report NOT-REPLAYED off a throttled poll.
func TestReplay_PollErrorSurfaces(t *testing.T) {
	srv := newScalesetStub(t)
	srv.PrequeueJobs(1)
	var log strings.Builder
	p := newReplayProbeForTest(t, srv, replayConfig{}, &log)
	srv.SetRateLimitPolls(true)

	err := p.run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded with every poll rate-limited:\n%s", log.String())
	}
	if !strings.Contains(err.Error(), "queue poll") {
		t.Errorf("error does not name the failing poll: %v", err)
	}
	if strings.Contains(log.String(), "VERDICT") {
		t.Errorf("a verdict was reported from a run whose polls all failed:\n%s", log.String())
	}
}

// TestReplay_UndecodableMessageDoesNotStopTheScenario asserts a message the client
// cannot parse is stepped over rather than fataled on: the verdicts are about which
// message IDS come back, so a body the scenario cannot read is still evidence.
func TestReplay_UndecodableMessageDoesNotStopTheScenario(t *testing.T) {
	srv := newScalesetStub(t)
	srv.SeedRawMessage("this is not JSON")
	srv.PrequeueJobs(1)
	var log strings.Builder
	p := newReplayProbeForTest(t, srv, replayConfig{}, &log)

	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	out := log.String()
	if !strings.Contains(out, "message decode failed") {
		t.Errorf("the undecodable message was never reported:\n%s", out)
	}
	if !strings.Contains(out, "VERDICT 1 ") {
		t.Errorf("the scenario stopped at the undecodable message:\n%s", out)
	}
}

// TestReplay_DeleteNeedsASessionAndSaysSoWhenItCannotGetOne asserts a session failure
// during measurement 2 stops the run rather than being reported as a wire-shape
// result: nothing was sent, so there is nothing to conclude about the endpoint.
func TestReplay_DeleteNeedsASessionAndSaysSoWhenItCannotGetOne(t *testing.T) {
	srv := newScalesetStub(t)
	p, st, _ := stageAgainstStub(t, srv)
	var log strings.Builder
	p.log = slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv.FailSessionCreate(true)

	if p.measureDelete(context.Background(), st, []int64{st.AssignedMessageID}) {
		t.Errorf("measureDelete reported success with no session:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "could not open a session to delete from") {
		t.Errorf("the session failure was not distinguished from a wire verdict:\n%s", log.String())
	}
}

// TestReplay_KeepLeavesTheScaleSet covers the opt-out, which exists so a surprising
// live result can be inspected by hand before its evidence is destroyed.
func TestReplay_KeepLeavesTheScaleSet(t *testing.T) {
	srv := newScalesetStub(t)
	srv.PrequeueJobs(1)
	var log strings.Builder
	p := newReplayProbeForTest(t, srv, replayConfig{Keep: true}, &log)
	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	if _, ok := srv.ScaleSetIDByName("gag-q583-replay"); !ok {
		t.Errorf("PROBE_REPLAY_KEEP did not keep the scale set:\n%s", log.String())
	}
}
