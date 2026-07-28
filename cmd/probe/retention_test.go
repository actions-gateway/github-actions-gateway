package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
)

// retentionEnv builds a getenv function over a map, with the credentials every
// phase needs already filled in, so a test only states what it is varying.
func retentionEnv(overrides map[string]string) func(string) string {
	env := map[string]string{
		"GITHUB_APP_ID":              "42",
		"GITHUB_APP_INSTALLATION_ID": "99",
		// A PEM literal rather than a path, exercising loadPEM's inline branch.
		// Deliberately not a key-shaped header: config parsing never decodes it,
		// and a real one here would be an unnecessary secret-scanner tripwire.
		"GITHUB_APP_PRIVATE_KEY": "-----BEGIN TEST PEM-----\nnot-a-key\n-----END TEST PEM-----",
		"GITHUB_ORG_URL":         "https://github.com/test-org",
	}
	for k, v := range overrides {
		env[k] = v
	}
	return func(k string) string { return env[k] }
}

// newRetentionProbeForTest builds a retention probe against the stub with test-scale
// timings. Its log output is captured so the tests can assert on what the probe
// REPORTS: a verdict is a log line, so that is where the evidence has to be checked.
func newRetentionProbeForTest(t *testing.T, srv *scalesettest.Server, cfg retentionConfig, w io.Writer) *retentionProbe {
	t.Helper()
	if cfg.ConfigURL == "" {
		cfg.ConfigURL = "https://github.com/test-org"
	}
	if cfg.ScaleSetName == "" {
		cfg.ScaleSetName = "gag-q468-retention"
	}
	if cfg.GroupName == "" {
		cfg.GroupName = "Default"
	}
	if cfg.ArmTimeout == 0 {
		cfg.ArmTimeout = 10 * time.Second
	}
	if cfg.CheckWindow == 0 {
		cfg.CheckWindow = 500 * time.Millisecond
	}
	if cfg.Capacity == 0 {
		cfg.Capacity = 1
	}
	p, err := newRetentionProbe(
		slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})),
		cfg,
		staticTokenProvider{token: installToken},
		srv.URL,
		srv.HTTPClient(),
		srv.HTTPClient(),
	)
	if err != nil {
		t.Fatalf("newRetentionProbe: %v", err)
	}
	if err := p.client.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return p
}

// readState decodes the experiment the probe persisted.
func readState(t *testing.T, path string) retentionState {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state retentionState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	return state
}

// armAgainstStub runs a full arm phase against a stub holding one queued job,
// returning the probe's config (so a check can reuse the state path) and its log.
func armAgainstStub(t *testing.T, srv *scalesettest.Server, cfg retentionConfig) (retentionConfig, string) {
	t.Helper()
	if cfg.StatePath == "" {
		cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	}
	srv.PrequeueJobs(1)
	var log strings.Builder
	p := newRetentionProbeForTest(t, srv, cfg, &log)
	if err := p.arm(context.Background()); err != nil {
		t.Fatalf("arm: %v\n%s", err, log.String())
	}
	return p.cfg, log.String()
}

func TestParseRetentionConfig_Defaults(t *testing.T) {
	cfg, err := parseRetentionConfig(retentionEnv(map[string]string{"PROBE_RETENTION_TEST": "arm"}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Phase != retentionPhaseArm {
		t.Errorf("phase = %q, want %q", cfg.Phase, retentionPhaseArm)
	}
	if cfg.ScaleSetName != "gag-q468-retention" {
		t.Errorf("scale set name = %q", cfg.ScaleSetName)
	}
	if cfg.StatePath != filepath.Join("tmp", "q468-retention-state.json") {
		t.Errorf("state path = %q", cfg.StatePath)
	}
	if cfg.Capacity != 1 {
		t.Errorf("capacity = %d, want 1", cfg.Capacity)
	}
	if cfg.ArmTimeout != 5*time.Minute || cfg.CheckWindow != 90*time.Second {
		t.Errorf("timings = %s/%s", cfg.ArmTimeout, cfg.CheckWindow)
	}
	if cfg.KeepArmed {
		t.Error("KeepArmed defaults true, want false")
	}
}

func TestParseRetentionConfig_RejectsUnknownPhase(t *testing.T) {
	for _, phase := range []string{"", "measure", "ARM"} {
		if _, err := parseRetentionConfig(retentionEnv(map[string]string{"PROBE_RETENTION_TEST": phase})); err == nil {
			t.Errorf("phase %q accepted, want rejected", phase)
		}
	}
}

func TestParseRetentionConfig_Overrides(t *testing.T) {
	cfg, err := parseRetentionConfig(retentionEnv(map[string]string{
		"PROBE_RETENTION_TEST":         "check",
		"PROBE_RETENTION_STATE":        "/tmp/x.json",
		"PROBE_RETENTION_NAME":         "custom",
		"PROBE_RETENTION_CHECK_WINDOW": "30s",
		"PROBE_RETENTION_ARM_TIMEOUT":  "2m",
		"PROBE_RETENTION_CAPACITY":     "3",
		"PROBE_RETENTION_KEEP_ARMED":   "true",
	}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Phase != retentionPhaseCheck || cfg.StatePath != "/tmp/x.json" || cfg.ScaleSetName != "custom" {
		t.Errorf("unexpected config: %+v", cfg)
	}
	if cfg.CheckWindow != 30*time.Second || cfg.ArmTimeout != 2*time.Minute || cfg.Capacity != 3 || !cfg.KeepArmed {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestParseRetentionConfig_RejectsBadDuration(t *testing.T) {
	_, err := parseRetentionConfig(retentionEnv(map[string]string{
		"PROBE_RETENTION_TEST":         "check",
		"PROBE_RETENTION_CHECK_WINDOW": "ninety",
	}))
	if err == nil {
		t.Fatal("bad duration accepted, want rejected")
	}
}

// TestRetention_ArmStagesTheExperiment asserts the state the gap starts from: a
// job assigned and cancelled, its JobCompleted observed, and — the property the
// whole measurement rests on — that message left UNACKNOWLEDGED behind a cursor
// pointing at the message before it.
func TestRetention_ArmStagesTheExperiment(t *testing.T) {
	srv := newScalesetStub(t)
	cfg, log := armAgainstStub(t, srv, retentionConfig{})

	state := readState(t, cfg.StatePath)
	if state.JobID == "" || state.RunID == 0 || state.Owner == "" || state.Repo == "" {
		t.Fatalf("run identity not captured: %+v", state)
	}
	if state.CompletedMessageID == 0 {
		t.Fatal("no JobCompleted was observed, so a later LOST verdict would be meaningless")
	}
	if state.CompletedMessageID <= state.Cursor {
		t.Errorf("cursor %d is not before the armed message %d — a check would poll past it",
			state.Cursor, state.CompletedMessageID)
	}
	if state.ArmedAt.IsZero() {
		t.Error("armedAt not stamped; the gap has no start")
	}
	if state.Concluded {
		t.Error("a freshly armed experiment is already concluded")
	}
	// The gap only starts once the session is gone; a live session would keep the
	// scale set listening and measure nothing.
	if srv.HasActiveSession(state.ScaleSetID) {
		t.Error("arm left a session behind")
	}
	if !strings.Contains(log, "left UNACKNOWLEDGED") {
		t.Errorf("arm did not report leaving the message unacknowledged:\n%s", log)
	}
	// The JobCompleted has to come from the run actually being cancelled over
	// REST — that call is how the arm drives a job terminal with no runner, and a
	// message arriving by any other route would not be the one under test.
	if !slices.Contains(srv.Calls(), "cancel-run id="+strconv.FormatInt(state.RunID, 10)) {
		t.Errorf("arm did not cancel run %d over REST; calls: %v", state.RunID, srv.Calls())
	}
}

// TestRetention_CheckReportsRetained is the RETAINED path end to end: arm, then
// check against the stub, whose queue log is scale-set-scoped and never expires.
//
// That last clause is the point and the limit — the stub CANNOT produce a LOST
// from a real arm, so this proves the harness reads a surviving message correctly,
// not that GitHub retains one.
func TestRetention_CheckReportsRetained(t *testing.T) {
	srv := newScalesetStub(t)
	cfg, _ := armAgainstStub(t, srv, retentionConfig{})

	var log strings.Builder
	p := newRetentionProbeForTest(t, srv, cfg, &log)
	if err := p.check(context.Background()); err != nil {
		t.Fatalf("check: %v\n%s", err, log.String())
	}

	state := readState(t, cfg.StatePath)
	if len(state.Checks) != 1 {
		t.Fatalf("checks = %d, want 1", len(state.Checks))
	}
	if got := state.Checks[0].Verdict; got != verdictRetained {
		t.Errorf("verdict = %q, want %q\n%s", got, verdictRetained, log.String())
	}
	if state.Checks[0].MessageID != state.CompletedMessageID {
		t.Errorf("redelivered message id = %d, want the armed %d",
			state.Checks[0].MessageID, state.CompletedMessageID)
	}
	if !state.Concluded {
		t.Error("a RETAINED check without KeepArmed should conclude the experiment")
	}
	if !strings.Contains(log.String(), "VERDICT RETAINED") {
		t.Errorf("no verdict reported:\n%s", log.String())
	}
	// A session left behind would silently shorten any later gap, so the check
	// must not leak one.
	if srv.HasActiveSession(state.ScaleSetID) {
		t.Error("check left a session behind")
	}
}

// TestRetention_CheckKeepArmedLeavesTheExperimentOpen covers the ladder mode: a
// RETAINED verdict records the rung and leaves the message in place for a
// longer-gap check.
func TestRetention_CheckKeepArmedLeavesTheExperimentOpen(t *testing.T) {
	srv := newScalesetStub(t)
	cfg, _ := armAgainstStub(t, srv, retentionConfig{KeepArmed: true})

	for rung := 1; rung <= 2; rung++ {
		var log strings.Builder
		p := newRetentionProbeForTest(t, srv, cfg, &log)
		if err := p.check(context.Background()); err != nil {
			t.Fatalf("check %d: %v\n%s", rung, err, log.String())
		}
		state := readState(t, cfg.StatePath)
		if len(state.Checks) != rung {
			t.Fatalf("after check %d: checks = %d", rung, len(state.Checks))
		}
		if state.Checks[rung-1].Verdict != verdictRetained {
			t.Fatalf("check %d verdict = %q", rung, state.Checks[rung-1].Verdict)
		}
		if state.Concluded {
			t.Fatalf("check %d concluded the experiment despite KeepArmed", rung)
		}
	}
}

// TestRetention_CheckReportsLost drives the verdict the stub cannot produce from a
// real arm: the armed job's JobCompleted is not on the queue, so the check window
// elapses with nothing to deliver.
func TestRetention_CheckReportsLost(t *testing.T) {
	srv := newScalesetStub(t)
	scaleSetID := srv.AddScaleSet("gag-q468-retention", 1)

	statePath := filepath.Join(t.TempDir(), "state.json")
	writeStateFile(t, statePath, retentionState{
		ScaleSetID:         scaleSetID,
		ScaleSetName:       "gag-q468-retention",
		JobID:              "job-never-queued",
		Owner:              scalesettest.DefaultJobOwner,
		Repo:               scalesettest.DefaultJobRepository,
		RunID:              900001,
		Cursor:             0,
		CompletedMessageID: 7,
		ArmedAt:            time.Now().Add(-4 * time.Hour).UTC(),
	})

	var log strings.Builder
	p := newRetentionProbeForTest(t, srv, retentionConfig{StatePath: statePath}, &log)
	if err := p.check(context.Background()); err != nil {
		t.Fatalf("check: %v\n%s", err, log.String())
	}

	state := readState(t, statePath)
	if len(state.Checks) != 1 || state.Checks[0].Verdict != verdictLost {
		t.Fatalf("checks = %+v, want one LOST\n%s", state.Checks, log.String())
	}
	// The gap is what the verdict is about; a check that does not report it is
	// not a measurement.
	if state.Checks[0].GapSeconds < 3*60*60 {
		t.Errorf("gap = %.0fs, want ~4h", state.Checks[0].GapSeconds)
	}
	if !strings.Contains(log.String(), "VERDICT LOST") {
		t.Errorf("no verdict reported:\n%s", log.String())
	}
}

// TestRetention_CheckWithoutStateFails: a check with no armed experiment is a
// user error, and saying so beats reporting LOST against a gap of zero.
func TestRetention_CheckWithoutStateFails(t *testing.T) {
	srv := newScalesetStub(t)
	p := newRetentionProbeForTest(t, srv,
		retentionConfig{StatePath: filepath.Join(t.TempDir(), "absent.json")}, io.Discard)
	err := p.check(context.Background())
	if err == nil {
		t.Fatal("check with no state succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "arm") {
		t.Errorf("error does not point at the arm phase: %v", err)
	}
}

// TestRetention_CheckFailsWhenScaleSetIsGone: without the scale set there is no
// queue log, so a LOST verdict would be an artefact of the cleanup rather than a
// measurement of retention.
func TestRetention_CheckFailsWhenScaleSetIsGone(t *testing.T) {
	srv := newScalesetStub(t)
	cfg, _ := armAgainstStub(t, srv, retentionConfig{})
	state := readState(t, cfg.StatePath)

	cleanup := newRetentionProbeForTest(t, srv, cfg, io.Discard)
	if err := cleanup.cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, ok := srv.ScaleSetIDByName(state.ScaleSetName); ok {
		t.Fatal("cleanup did not delete the scale set")
	}

	p := newRetentionProbeForTest(t, srv, cfg, io.Discard)
	err := p.check(context.Background())
	if err == nil {
		t.Fatal("check against a deleted scale set succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "queue log") {
		t.Errorf("error does not explain why the gap measured nothing: %v", err)
	}
}

// TestRetention_ArmWithoutADispatchedJobFails: the arm phase waits for a job that
// an operator has to dispatch, and a timeout has to name that rather than arming an
// experiment with no job in it.
func TestRetention_ArmWithoutADispatchedJobFails(t *testing.T) {
	srv := newScalesetStub(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	p := newRetentionProbeForTest(t, srv, retentionConfig{
		StatePath:  statePath,
		ArmTimeout: 300 * time.Millisecond,
	}, io.Discard)

	err := p.arm(context.Background())
	if err == nil {
		t.Fatal("arm with no dispatched job succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "dispatched") {
		t.Errorf("error does not name the missing dispatch: %v", err)
	}
	if _, statErr := os.Stat(statePath); statErr == nil {
		t.Error("a failed arm wrote a state file a later check would trust")
	}
	// A failed arm must not leave the scale set listening: a live session would
	// silently invalidate whatever gap the next arm measures.
	if id, ok := srv.ScaleSetIDByName("gag-q468-retention"); ok && srv.HasActiveSession(id) {
		t.Error("a failed arm left a session behind")
	}
}

// TestRetention_CleanupByNameWithoutState covers recovery from an interrupted arm:
// the scale set is registered but no state file names it.
func TestRetention_CleanupByNameWithoutState(t *testing.T) {
	srv := newScalesetStub(t)
	srv.AddScaleSet("gag-q468-retention", 1)

	p := newRetentionProbeForTest(t, srv,
		retentionConfig{StatePath: filepath.Join(t.TempDir(), "absent.json")}, io.Discard)
	if err := p.cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, ok := srv.ScaleSetIDByName("gag-q468-retention"); ok {
		t.Fatal("cleanup did not delete the scale set")
	}
}

// TestRetention_CleanupIsIdempotent: nothing registered is not an error, so a
// second cleanup (or one after a manual delete) is safe.
func TestRetention_CleanupIsIdempotent(t *testing.T) {
	srv := newScalesetStub(t)
	p := newRetentionProbeForTest(t, srv,
		retentionConfig{StatePath: filepath.Join(t.TempDir(), "absent.json")}, io.Discard)
	if err := p.cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup with nothing registered: %v", err)
	}
}

// writeStateFile persists a hand-built experiment, for the cases the stub cannot
// reach by arming for real.
func writeStateFile(t *testing.T, path string, state retentionState) {
	t.Helper()
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

// TestRetention_RunDispatchesByPhase covers the phase dispatcher: the selector is
// the whole user interface for this scenario, so a phase that silently ran the
// wrong thing would be the worst failure mode it has.
func TestRetention_RunDispatchesByPhase(t *testing.T) {
	srv := newScalesetStub(t)
	srv.AddScaleSet("gag-q468-retention", 1)

	cleanupProbe := newRetentionProbeForTest(t, srv, retentionConfig{
		Phase:     retentionPhaseCleanup,
		StatePath: filepath.Join(t.TempDir(), "absent.json"),
	}, io.Discard)
	if err := cleanupProbe.run(context.Background()); err != nil {
		t.Fatalf("run(cleanup): %v", err)
	}
	if _, ok := srv.ScaleSetIDByName("gag-q468-retention"); ok {
		t.Error("run(cleanup) did not reach the cleanup phase")
	}

	// A phase that got past parsing but is not one of the three must fail loudly
	// rather than fall through to a default.
	bogus := newRetentionProbeForTest(t, srv, retentionConfig{
		Phase:     "measure",
		StatePath: filepath.Join(t.TempDir(), "absent.json"),
	}, io.Discard)
	if err := bogus.run(context.Background()); err == nil {
		t.Error("run with an unknown phase succeeded, want an error")
	}
}

// TestRetention_ArmAcquiresOnTheGHESOfferFlow: on the GHES path a job is offered
// as JobAvailable and only becomes an assignment once claimed, so the arm phase
// has to acquire before it has anything to cancel.
func TestRetention_ArmAcquiresOnTheGHESOfferFlow(t *testing.T) {
	srv := newScalesetStub(t)
	srv.EnableGHESAcquireFlow()
	cfg, log := armAgainstStub(t, srv, retentionConfig{})

	if srv.AcquireJobsCalls() == 0 {
		t.Errorf("arm never claimed the offered job:\n%s", log)
	}
	state := readState(t, cfg.StatePath)
	if state.JobID == "" || state.CompletedMessageID == 0 {
		t.Fatalf("offer flow did not reach an armed experiment: %+v", state)
	}
}

// TestRetention_ArmStepsOverAnUnrelatedMessage: a lifecycle message the probe has
// nothing to do with must be acknowledged and stepped over, not treated as the
// assignment and not left to wedge the cursor.
func TestRetention_ArmStepsOverAnUnrelatedMessage(t *testing.T) {
	srv := newScalesetStub(t)
	srv.SeedMessage([]scaleset.JobMessage{{
		MessageType: scaleset.MessageTypeJobStarted,
		JobID:       "someone-elses-job",
	}})
	cfg, _ := armAgainstStub(t, srv, retentionConfig{})

	state := readState(t, cfg.StatePath)
	if state.JobID == "someone-elses-job" {
		t.Fatal("arm armed the experiment on an unrelated job")
	}
	if state.CompletedMessageID == 0 {
		t.Fatal("arm did not get past the unrelated message")
	}
}

// TestRetention_ArmRefusesAnAssignmentWithoutRunIdentity: the cancel step is
// addressed by (owner, repo, run_id), so an assignment missing any of them has to
// fail the arm rather than cancel a run the probe cannot name.
func TestRetention_ArmRefusesAnAssignmentWithoutRunIdentity(t *testing.T) {
	srv := newScalesetStub(t)
	srv.SeedMessage([]scaleset.JobMessage{{
		MessageType: scaleset.MessageTypeJobAssigned,
		JobID:       "job-without-identity",
	}})
	statePath := filepath.Join(t.TempDir(), "state.json")
	p := newRetentionProbeForTest(t, srv, retentionConfig{StatePath: statePath}, io.Discard)

	err := p.arm(context.Background())
	if err == nil {
		t.Fatal("arm accepted an assignment with no run identity")
	}
	if !strings.Contains(err.Error(), "run identity") {
		t.Errorf("error does not name the missing identity: %v", err)
	}
	if _, statErr := os.Stat(statePath); statErr == nil {
		t.Error("a refused arm wrote a state file")
	}
}

// TestRetention_CheckIgnoresAnotherJobsCompletion: the check matches on the armed
// job's id, so an unrelated completion sitting on the queue must not be read as
// the armed message surviving.
func TestRetention_CheckIgnoresAnotherJobsCompletion(t *testing.T) {
	srv := newScalesetStub(t)
	srv.SeedMessage([]scaleset.JobMessage{{
		MessageType: scaleset.MessageTypeJobCompleted,
		JobID:       "a-different-job",
		Result:      "succeeded",
	}})
	// The seed attaches to scale sets registered after it, so register the
	// experiment's scale set now and point the state file at it.
	scaleSetID := srv.AddScaleSet("gag-q468-retention-seeded", 1)

	statePath := filepath.Join(t.TempDir(), "state.json")
	writeStateFile(t, statePath, retentionState{
		ScaleSetID:         scaleSetID,
		ScaleSetName:       "gag-q468-retention-seeded",
		JobID:              "the-armed-job",
		Owner:              scalesettest.DefaultJobOwner,
		Repo:               scalesettest.DefaultJobRepository,
		RunID:              900001,
		CompletedMessageID: 99,
		ArmedAt:            time.Now().Add(-time.Hour).UTC(),
	})

	var log strings.Builder
	p := newRetentionProbeForTest(t, srv, retentionConfig{StatePath: statePath}, &log)
	if err := p.check(context.Background()); err != nil {
		t.Fatalf("check: %v\n%s", err, log.String())
	}
	state := readState(t, statePath)
	if state.Checks[0].Verdict != verdictLost {
		t.Errorf("verdict = %q, want %q — another job's completion is not evidence",
			state.Checks[0].Verdict, verdictLost)
	}
}

// TestRetention_CancelRunTreatsAlreadyTerminalAsSuccess: the cancel exists to make
// a job terminal, so a run that is already terminal has reached the goal.
func TestRetention_CancelRunTreatsAlreadyTerminalAsSuccess(t *testing.T) {
	srv := newScalesetStub(t)
	p := newRetentionProbeForTest(t, srv, retentionConfig{}, io.Discard)

	// No job belongs to this run, so the stub answers 409 as GitHub does.
	if err := p.cancelRun(context.Background(), "acme", "widgets", 424242); err != nil {
		t.Errorf("cancelRun on an already-terminal run = %v, want nil", err)
	}
}

// TestRetention_LoadStateRejectsIncompleteState: a half-written state file would
// otherwise produce a verdict against a zero gap.
func TestRetention_LoadStateRejectsIncompleteState(t *testing.T) {
	srv := newScalesetStub(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	writeStateFile(t, statePath, retentionState{ScaleSetID: 7, ScaleSetName: "x"})

	p := newRetentionProbeForTest(t, srv, retentionConfig{StatePath: statePath}, io.Discard)
	_, err := p.loadState()
	if err == nil {
		t.Fatal("incomplete state accepted, want an error")
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("error does not name the problem: %v", err)
	}
}

func TestParseRetentionConfig_RequiresCredentials(t *testing.T) {
	for _, missing := range []string{"GITHUB_APP_ID", "GITHUB_APP_INSTALLATION_ID", "GITHUB_APP_PRIVATE_KEY", "GITHUB_ORG_URL"} {
		env := retentionEnv(map[string]string{"PROBE_RETENTION_TEST": "arm", missing: ""})
		if _, err := parseRetentionConfig(env); err == nil {
			t.Errorf("missing %s accepted, want rejected", missing)
		}
	}
}

// TestRetention_ReArmOverAnOpenExperiment: re-arming is the normal recovery from
// a run that went wrong, and it must say out loud that it is discarding an
// experiment whose gap was still being measured.
func TestRetention_ReArmOverAnOpenExperiment(t *testing.T) {
	srv := newScalesetStub(t)
	cfg, _ := armAgainstStub(t, srv, retentionConfig{})
	first := readState(t, cfg.StatePath)

	// The scale set is durable, so a re-arm reuses it — the new job goes onto the
	// existing one rather than into the pre-registration queue.
	srv.EnqueueJob(first.ScaleSetID)
	var log strings.Builder
	p := newRetentionProbeForTest(t, srv, cfg, &log)
	if err := p.arm(context.Background()); err != nil {
		t.Fatalf("re-arm: %v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "overwriting a state file") {
		t.Errorf("re-arm did not warn about discarding the open experiment:\n%s", log.String())
	}
	second := readState(t, cfg.StatePath)
	if second.JobID == first.JobID {
		t.Error("re-arm reused the previous job rather than arming a fresh one")
	}
}

// TestRetention_ArmSurvivesARunnerGroupLookupFailure: the group lookup is a
// convenience, not a prerequisite — a backend that will not answer it must not
// cost the run, since the default group is the fallback anyway.
func TestRetention_ArmSurvivesARunnerGroupLookupFailure(t *testing.T) {
	srv := newScalesetStub(t)
	srv.FailRunnerGroups(true)
	cfg, log := armAgainstStub(t, srv, retentionConfig{})

	if !strings.Contains(log, "falling back to group id 1") {
		t.Errorf("no fallback reported:\n%s", log)
	}
	if state := readState(t, cfg.StatePath); state.CompletedMessageID == 0 {
		t.Fatalf("arm did not complete despite the fallback: %+v", state)
	}
}

// TestRetention_CheckFailsRatherThanReportLostOnInfraFailure: a session it could
// not create is a broken check, not a message that was dropped. Reporting LOST
// here would manufacture evidence for the very claim the experiment exists to
// test.
func TestRetention_CheckFailsRatherThanReportLostOnInfraFailure(t *testing.T) {
	srv := newScalesetStub(t)
	cfg, _ := armAgainstStub(t, srv, retentionConfig{})
	srv.FailSessionCreate(true)

	p := newRetentionProbeForTest(t, srv, cfg, io.Discard)
	if err := p.check(context.Background()); err == nil {
		t.Fatal("check with no session succeeded, want an error")
	}
	if state := readState(t, cfg.StatePath); len(state.Checks) != 0 {
		t.Errorf("a failed check recorded a verdict: %+v", state.Checks)
	}
}

// TestRetention_RunRetentionProbeWiresTheScenario covers the entry point main.go
// actually calls, so the wiring between the parsed config and the phase cannot
// rot untested.
func TestRetention_RunRetentionProbeWiresTheScenario(t *testing.T) {
	srv := newScalesetStub(t)
	srv.AddScaleSet("gag-q468-retention", 1)

	err := runRetentionProbe(context.Background(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		retentionConfig{
			Phase:        retentionPhaseCleanup,
			ConfigURL:    "https://github.com/test-org",
			ScaleSetName: "gag-q468-retention",
			GroupName:    "Default",
			StatePath:    filepath.Join(t.TempDir(), "absent.json"),
			Capacity:     1,
		},
		staticTokenProvider{token: installToken},
		srv.URL)
	if err != nil {
		t.Fatalf("runRetentionProbe: %v", err)
	}
	if _, ok := srv.ScaleSetIDByName("gag-q468-retention"); ok {
		t.Error("the scenario did not reach the cleanup phase")
	}
}

// TestRetention_ArmReportsAnUnwritableStatePath: the state file is written after
// the session is deleted, so a path that cannot be written has already burned the
// experiment — the arm has to fail loudly rather than exit zero having lost it.
func TestRetention_ArmReportsAnUnwritableStatePath(t *testing.T) {
	srv := newScalesetStub(t)
	dir := t.TempDir()
	blocker := filepath.Join(dir, "notadir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	srv.PrequeueJobs(1)
	p := newRetentionProbeForTest(t, srv, retentionConfig{
		StatePath: filepath.Join(blocker, "state.json"),
	}, io.Discard)
	err := p.arm(context.Background())
	if err == nil {
		t.Fatal("arm with an unwritable state path succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "state") {
		t.Errorf("error does not name the state file: %v", err)
	}
}
