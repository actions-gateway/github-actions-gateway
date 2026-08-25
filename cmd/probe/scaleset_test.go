package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
)

// installToken is the App installation token the probe's token provider hands the
// scaleset client, and the one the stub's registration-token hop requires.
const installToken = "install-token"

// staticTokenProvider satisfies tokenProvider with a fixed token.
type staticTokenProvider struct{ token string }

func (s staticTokenProvider) Token(context.Context) (string, error) { return s.token, nil }

// newScalesetStub starts the shared scale-set protocol stub (scaleset/scalesettest)
// configured for the probe: the installation token pinned, so a bootstrap hop
// presenting the wrong credential fails the run rather than passing unnoticed, and a
// poll window short enough that the probe's empty-queue polls do not stall a test
// while still keeping its polling loops off a hot spin.
func newScalesetStub(t *testing.T) *scalesettest.Server {
	t.Helper()
	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	srv.SetInstallationToken(installToken)
	srv.SetPollTimeout(50 * time.Millisecond)
	return srv
}

// newScalesetProbeForTest builds a probe against the stub with the scenario's
// defaults filled in and its log output discarded.
func newScalesetProbeForTest(t *testing.T, srv *scalesettest.Server, cfg scalesetConfig) *scalesetProbe {
	t.Helper()
	return newScalesetProbeLogging(t, srv, cfg, io.Discard)
}

// newScalesetProbeLogging is newScalesetProbeForTest with the probe's log output
// captured, for the tests that assert on what the probe REPORTS — the probe's
// findings are log lines, so that is where its evidence has to be checked.
func newScalesetProbeLogging(t *testing.T, srv *scalesettest.Server, cfg scalesetConfig, w io.Writer) *scalesetProbe {
	t.Helper()
	if cfg.ConfigURL == "" {
		cfg.ConfigURL = "https://github.com/test-org"
	}
	if cfg.ScaleSetName == "" {
		cfg.ScaleSetName = "gag-probe-scaleset"
	}
	if cfg.GroupName == "" {
		cfg.GroupName = "Default"
	}
	return newScalesetProbeOn(t, cfg, srv.URL, srv.HTTPClient(), w)
}

// newScalesetProbeOn builds a probe whose scaleset client targets base for both the
// REST bootstrap and the Actions Service, over hc, logging to w. It takes cfg
// verbatim — the bespoke error-path tests below drive it against a hand-written
// handler rather than the protocol stub.
func newScalesetProbeOn(t *testing.T, cfg scalesetConfig, base string, hc *http.Client, w io.Writer) *scalesetProbe {
	t.Helper()
	p, err := newScalesetProbe(
		slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})),
		cfg,
		staticTokenProvider{token: installToken},
		base,
		hc,
		hc,
	)
	if err != nil {
		t.Fatalf("newScalesetProbe: %v", err)
	}
	return p
}

// requireCalls fails the test for every want the stub's call log does not contain.
func requireCalls(t *testing.T, srv *scalesettest.Server, want ...string) {
	t.Helper()
	calls := srv.Calls()
	for _, w := range want {
		if !slices.Contains(calls, w) {
			t.Errorf("call log missing %q; got:\n%s", w, strings.Join(calls, "\n"))
		}
	}
}

// callIndex returns the position of the first call equal to want, or -1.
func callIndex(calls []string, want string) int {
	return slices.Index(calls, want)
}

// countCalls returns how many recorded calls start with prefix.
func countCalls(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// formatIDs renders a request-id batch the way the stub's call log does, so a test
// can build the exact recorded call it expects.
func formatIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func TestScalesetProbe_FullFlowAndCleanup(t *testing.T) {
	srv := newScalesetStub(t)
	// A job queued against the scale set's label before it registers: the step-7
	// poll advertises capacity 1, so the backend assigns it and the poll returns a
	// real message — exercising the decode branch of pollQueueOnce alongside the
	// empty-202 default the other tests take.
	srv.PrequeueJobs(1)
	p := newScalesetProbeForTest(t, srv, scalesetConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	requireCalls(t, srv,
		"registration-token /orgs/test-org/actions/runners/registration-token",
		"runner-registration",
		"runnergroups name=Default",
		"create-scaleset name=gag-probe-scaleset group=7",
		"create-session id=1",
		"poll id=1 cap=1 last=0",
		// The acquire matrix: the client's own construction on an empty batch and
		// on an id never offered, the same route under the admin JWT, ARC's
		// read-only listing, and the queue-host route family.
		"acquirejobs id=1 auth=queue ids=[]",
		"acquirejobs id=1 auth=queue ids=[9999999999]",
		"acquirejobs id=1 auth=admin ids=[9999999999]",
		"acquirablejobs id=1",
		"acquirejobs-queuehost id=1 auth=queue ids=[9999999999]",
		"generatejitconfig id=1",
		"delete-session id=1",
		"delete-scaleset id=1",
	)
	// Cleanup ordering: session deleted before the scale set.
	calls := srv.Calls()
	if si, ci := callIndex(calls, "delete-session id=1"), callIndex(calls, "delete-scaleset id=1"); si > ci {
		t.Errorf("session must be deleted before the scale set; got:\n%s", strings.Join(calls, "\n"))
	}
}

func TestScalesetProbe_RepoScopedRegistrationToken(t *testing.T) {
	srv := newScalesetStub(t)
	p := newScalesetProbeForTest(t, srv, scalesetConfig{ConfigURL: "https://github.com/test-org/test-repo"})

	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	requireCalls(t, srv, "registration-token /repos/test-org/test-repo/actions/runners/registration-token")
}

func TestScalesetProbe_JobTestAcquiresAndSeesAssigned(t *testing.T) {
	srv := newScalesetStub(t)
	// The GHES path: a queued job is offered as JobAvailable and must be claimed
	// with acquirejobs before a JobAssigned follows.
	srv.EnableGHESAcquireFlow()
	ids := srv.PrequeueJobs(1)
	p := newScalesetProbeForTest(t, srv, scalesetConfig{JobTest: true})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	requireCalls(t, srv,
		// The offered id was claimed through the client's own construction …
		"acquirejobs id=1 auth=queue ids="+formatIDs(ids),
		// … and the cursor advanced past the JobAvailable before the JobAssigned
		// that followed it could be read.
		"poll id=1 cap=1 last=1",
	)
}

func TestScalesetProbe_RegistrationTokenErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	defer srv.Close()

	p := newScalesetProbeOn(t, scalesetConfig{
		ConfigURL: "https://github.com/test-org", ScaleSetName: "x",
	}, srv.URL, srv.Client(), io.Discard)
	err := p.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "registration token") {
		t.Fatalf("want registration token error, got %v", err)
	}
}

// TestScalesetProbe_BadConfigURLPath asserts the probe surfaces the scaleset
// client's own config-URL validation rather than re-deriving the REST path — the
// probe no longer owns a copy of that mapping (Q362).
func TestScalesetProbe_BadConfigURLPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request should reach the server: the config URL is invalid")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newScalesetProbeOn(t, scalesetConfig{ConfigURL: "https://github.com/a/b/c"},
		srv.URL, srv.Client(), io.Discard)
	err := p.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "org or owner/repo") {
		t.Fatalf("want org/repo path error, got %v", err)
	}
}

func TestRunScalesetProbe_EntryPoint(t *testing.T) {
	srv := newScalesetStub(t)
	cfg := scalesetConfig{
		ConfigURL:    "https://github.com/test-org",
		ScaleSetName: "gag-probe-scaleset",
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := runScalesetProbe(ctx, logger, cfg, staticTokenProvider{token: installToken}, srv.URL); err != nil {
		t.Fatalf("runScalesetProbe: %v", err)
	}
	requireCalls(t, srv, "delete-scaleset id=1")
}

func TestScalesetProbe_AdminConnectionRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/registration-token") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"reg-token"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	p := newScalesetProbeOn(t, scalesetConfig{
		ConfigURL: "https://github.com/test-org", ScaleSetName: "x",
	}, srv.URL, srv.Client(), io.Discard)
	err := p.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "runner-registration") {
		t.Fatalf("want runner-registration error, got %v", err)
	}
}

func TestScalesetProbe_AdminConnectionMissingFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/registration-token") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"reg-token"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`)) // 200 but no url/token
	}))
	defer srv.Close()

	p := newScalesetProbeOn(t, scalesetConfig{
		ConfigURL: "https://github.com/test-org", ScaleSetName: "x",
	}, srv.URL, srv.Client(), io.Discard)
	err := p.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "missing url or token") {
		t.Fatalf("want missing url/token error, got %v", err)
	}
}

func TestScalesetProbe_RunnerGroupFallbackToDefault(t *testing.T) {
	srv := newScalesetStub(t)
	srv.FailRunnerGroups(true)
	p := newScalesetProbeForTest(t, srv, scalesetConfig{})

	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The lookup failed, so the scale set must have been created against GitHub's
	// default group id — not against the id the lookup would have returned (7).
	requireCalls(t, srv, "create-scaleset name=gag-probe-scaleset group=1")
}

func TestScalesetProbe_SessionCreateFailureStillDeletesScaleSet(t *testing.T) {
	srv := newScalesetStub(t)
	srv.FailSessionCreate(true)
	p := newScalesetProbeForTest(t, srv, scalesetConfig{})

	err := p.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "create session") {
		t.Fatalf("want create session error, got %v", err)
	}
	requireCalls(t, srv, "delete-scaleset id=1")
	if n := countCalls(srv.Calls(), "delete-session"); n != 0 {
		t.Errorf("no session existed; delete-session must not fire; got:\n%s",
			strings.Join(srv.Calls(), "\n"))
	}
}

func TestScalesetProbe_JobTestTimesOutOnEmptyQueue(t *testing.T) {
	srv := newScalesetStub(t) // no jobs queued: every poll answers 202
	p := newScalesetProbeForTest(t, srv, scalesetConfig{JobTest: true})
	p.jobTestTimeout = 300 * time.Millisecond

	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	requireCalls(t, srv, "delete-scaleset id=1")
	// The empty-queue polls must never have acquired anything beyond the two shape
	// probes, which claim an empty batch and an id that was never offered.
	for _, c := range srv.Calls() {
		if !strings.HasPrefix(c, "acquirejobs") {
			continue
		}
		if !strings.HasSuffix(c, "ids=[]") && !strings.HasSuffix(c, "ids=[9999999999]") {
			t.Errorf("nothing should be acquired on an empty queue; got %q", c)
		}
	}
}

func TestScalesetProbe_JobTestSkipsNonAvailableEntries(t *testing.T) {
	srv := newScalesetStub(t)
	// Two messages the job test has nothing to do with, ahead of the assignment it
	// is waiting for: a lifecycle message carrying no acquirable id, and a body it
	// cannot decode at all. Both must be skipped without wedging the cursor.
	srv.SeedMessage([]scaleset.JobMessage{{
		MessageType:     scaleset.MessageTypeJobStarted,
		RunnerRequestID: 55,
		JobID:           "job-55",
	}})
	srv.SeedRawMessage("not-json")
	srv.PrequeueJobs(1)
	p := newScalesetProbeForTest(t, srv, scalesetConfig{JobTest: true})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The cursor advanced past both skipped messages to reach the JobAssigned.
	requireCalls(t, srv, "poll id=1 cap=1 last=1", "poll id=1 cap=1 last=2")
	// The JobStarted entry carries no acquirable id, so it must never be claimed.
	for _, c := range srv.Calls() {
		if strings.HasPrefix(c, "acquirejobs") && strings.Contains(c, "ids=[55]") {
			t.Errorf("JobStarted entry must not be acquired; got %q", c)
		}
	}
}

// TestScalesetProbe_JobTestReportsRunIdentity pins the Q417 verification the probe
// exists to carry out on a live run: a JobAssigned must be reported with an explicit
// verdict on whether it carried the workflow-run identity scale-set eviction recovery
// reads. The raw body is logged too, but a truncated redacted blob is not a finding —
// the point of the verdict line is that a live run states the answer.
func TestScalesetProbe_JobTestReportsRunIdentity(t *testing.T) {
	srv := newScalesetStub(t)
	srv.PrequeueJobs(1)

	var out strings.Builder
	p := newScalesetProbeLogging(t, srv, scalesetConfig{JobTest: true}, &out)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "run identity present on JobAssigned") {
		t.Errorf("probe must report the identity verdict for an assignment that carried one; got:\n%s", got)
	}
	// The identity reported must be the one the backend actually delivered, not a
	// placeholder — that is the whole point of the observation.
	for _, want := range []string{
		"owner=" + scalesettest.DefaultJobOwner,
		"repo=" + scalesettest.DefaultJobRepository,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("verdict line must name the delivered identity (%s); got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "GAP — JobAssigned carries no complete run identity") {
		t.Errorf("a complete identity must not be reported as a gap; got:\n%s", got)
	}
}

// TestScalesetProbe_JobTestReportsMissingRunIdentity is the other half, and the one
// that matters on a backend that does not send the fields: the absence must be a loud
// GAP naming the consequence, not a silent omission that reads like a clean run.
func TestScalesetProbe_JobTestReportsMissingRunIdentity(t *testing.T) {
	srv := newScalesetStub(t)
	// A raw body bypasses the stub's model, which always fills identity in — the only
	// way to present the probe with the shape this verification is looking for.
	srv.SeedRawMessage(`[{"messageType":"JobAssigned","jobId":"bare-job"}]`)

	var out strings.Builder
	p := newScalesetProbeLogging(t, srv, scalesetConfig{JobTest: true}, &out)
	p.jobTestTimeout = 2 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "GAP — JobAssigned carries no complete run identity") {
		t.Errorf("a missing identity must be reported as a GAP; got:\n%s", got)
	}
	if strings.Contains(got, "run identity present on JobAssigned") {
		t.Errorf("an incomplete identity must not be reported as present; got:\n%s", got)
	}
}

func TestScalesetProbe_CapacityTestSequence(t *testing.T) {
	srv := newScalesetStub(t)
	// Two jobs queued against the label: at capacity 0 neither may be assigned, and
	// each widening of the gate releases exactly one more.
	srv.PrequeueJobs(2)

	dir := t.TempDir()
	jit1 := dir + "/jit-1.b64"
	jit2 := dir + "/jit-2.b64"
	p := newScalesetProbeForTest(t, srv, scalesetConfig{
		CapacityTest:   true,
		JITConfigFiles: []string{jit1, jit2},
		HoldSeconds:    1,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	requireCalls(t, srv,
		"poll id=1 cap=0 last=0", // phase 1: capacity 0 — nothing assignable
		"poll id=1 cap=1 last=0", // phase 2: capacity 1 (cursor unchanged by the 202)
		"poll id=1 cap=2 last=1", // phase 3: capacity 2, cursor advanced
		"refresh-session id=1",   // phase 4: PATCH
		"poll id=1 cap=2 last=0", // phase 5: replay poll on the fresh session
		"generatejitconfig id=1", // JIT mints
		"delete-scaleset id=1",
	)
	calls := srv.Calls()
	// Two session creates (initial + replay recreate) and two deletes (replay test
	// + deferred cleanup of the recreated session).
	if n := countCalls(calls, "create-session"); n != 2 {
		t.Errorf("create-session count = %d, want 2; got:\n%s", n, strings.Join(calls, "\n"))
	}
	if n := countCalls(calls, "delete-session"); n != 2 {
		t.Errorf("delete-session count = %d, want 2; got:\n%s", n, strings.Join(calls, "\n"))
	}
	// The JIT blobs must land in the files, 0600.
	for _, f := range []string{jit1, jit2} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if len(b) == 0 {
			t.Errorf("%s is empty", f)
		}
		info, _ := os.Stat(f)
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s perms = %v, want 0600", f, info.Mode().Perm())
		}
	}
}

func TestScalesetProbe_CleanupMode(t *testing.T) {
	srv := newScalesetStub(t)
	// A scale set an interrupted run leaked — the state cleanup mode exists to find.
	srv.AddScaleSet("gag-probe-scaleset", 7)
	p := newScalesetProbeForTest(t, srv, scalesetConfig{Cleanup: true})

	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The unfiltered list, not a by-name lookup: cleanup reaches the named set through
	// the listing so that the sets it is NOT deleting are reported too (Q344).
	requireCalls(t, srv, "get-scaleset name=", "delete-scaleset id=1")
	calls := srv.Calls()
	if countCalls(calls, "create-scaleset") != 0 || countCalls(calls, "create-session") != 0 {
		t.Errorf("cleanup mode must not create anything; got:\n%s", strings.Join(calls, "\n"))
	}
}

// TestScalesetProbe_CleanupReportsSetsItDoesNotDelete is the orphan-visibility half.
// An orphan's name is precisely what the operator does not have, so a cleanup that
// only ever spoke about the name it was given could never surface one.
func TestScalesetProbe_CleanupReportsSetsItDoesNotDelete(t *testing.T) {
	srv := newScalesetStub(t)
	srv.AddScaleSet("gag-probe-scaleset", 7)
	stranded := srv.AddScaleSet("gag-forgotten-by-everyone", 7)

	var log strings.Builder
	p := newScalesetProbeLogging(t, srv, scalesetConfig{Cleanup: true}, &log)
	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(log.String(), "gag-forgotten-by-everyone") {
		t.Errorf("a scale set nobody can name was never reported, so cleanup cannot "+
			"surface an orphan\n--- log ---\n%s", log.String())
	}
	if got := countCalls(srv.Calls(), "delete-scaleset"); got != 1 {
		t.Errorf("delete count = %d; want only the named set deleted", got)
	}
	if slices.Contains(srv.Calls(), fmt.Sprintf("delete-scaleset id=%d", stranded)) {
		t.Error("cleanup deleted a set it was not asked to; the sweep must be opt-in")
	}
}

func TestScalesetProbe_CleanupPrunePrefix(t *testing.T) {
	srv := newScalesetStub(t)
	doomed := srv.AddScaleSet("gag-e2e-run-1", 7)
	alsoDoomed := srv.AddScaleSet("gag-e2e-run-2", 7)
	keep := srv.AddScaleSet("prod-linux", 7)

	p := newScalesetProbeForTest(t, srv, scalesetConfig{
		Cleanup: true, ScaleSetName: "no-such-set", PrunePrefix: "gag-e2e-",
	})
	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	calls := srv.Calls()
	requireCalls(t, srv,
		fmt.Sprintf("delete-scaleset id=%d", doomed),
		fmt.Sprintf("delete-scaleset id=%d", alsoDoomed))
	if slices.Contains(calls, fmt.Sprintf("delete-scaleset id=%d", keep)) {
		t.Errorf("prefix sweep deleted a non-matching scale set; got:\n%s",
			strings.Join(calls, "\n"))
	}
	if got := countCalls(calls, "delete-scaleset"); got != 2 {
		t.Errorf("delete count = %d; want exactly the two prefix matches", got)
	}
}

// TestScalesetProbe_CleanupDryRun matters because the sweep is destructive against real
// GitHub and a prefix is easy to get wrong. Dry run must still list, so it is usable as
// the look-before-you-delete step rather than a mode that reports nothing.
func TestScalesetProbe_CleanupDryRun(t *testing.T) {
	srv := newScalesetStub(t)
	srv.AddScaleSet("gag-e2e-run-1", 7)

	var log strings.Builder
	p := newScalesetProbeLogging(t, srv, scalesetConfig{
		Cleanup: true, ScaleSetName: "no-such-set", PrunePrefix: "gag-e2e-", DryRun: true,
	}, &log)
	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := countCalls(srv.Calls(), "delete-scaleset"); got != 0 {
		t.Errorf("dry run issued %d delete(s); want none", got)
	}
	if !strings.Contains(log.String(), "WOULD DELETE") {
		t.Errorf("dry run did not name what it would delete\n--- log ---\n%s", log.String())
	}
	if !strings.Contains(log.String(), "gag-e2e-run-1") {
		t.Errorf("dry run did not list the scale set\n--- log ---\n%s", log.String())
	}
}

func TestParseScalesetConfig_CapacityAndHold(t *testing.T) {
	env := map[string]string{
		"GITHUB_APP_ID":                  "123",
		"GITHUB_APP_INSTALLATION_ID":     "456",
		"GITHUB_APP_PRIVATE_KEY":         testRSAPEM(t),
		"GITHUB_ORG_URL":                 "https://github.com/test-org",
		"PROBE_SCALESET_CAPACITY_TEST":   "true",
		"PROBE_SCALESET_JITCONFIG_FILES": "a.b64, b.b64",
		"PROBE_SCALESET_HOLD_SECONDS":    "42",
	}
	cfg, err := parseScalesetConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("parseScalesetConfig: %v", err)
	}
	if !cfg.CapacityTest || cfg.HoldSeconds != 42 {
		t.Errorf("capacity/hold not parsed: %+v", cfg)
	}
	if len(cfg.JITConfigFiles) != 2 || cfg.JITConfigFiles[1] != "b.b64" {
		t.Errorf("JITConfigFiles = %v, want [a.b64 b.b64]", cfg.JITConfigFiles)
	}

	env["PROBE_SCALESET_HOLD_SECONDS"] = "notanumber"
	if _, err := parseScalesetConfig(func(k string) string { return env[k] }); err == nil {
		t.Error("want error for bad PROBE_SCALESET_HOLD_SECONDS")
	}
}

func TestParseScalesetConfig_Valid(t *testing.T) {
	env := map[string]string{
		"GITHUB_APP_ID":              "123",
		"GITHUB_APP_INSTALLATION_ID": "456",
		"GITHUB_APP_PRIVATE_KEY":     testRSAPEM(t),
		"GITHUB_ORG_URL":             "https://github.com/test-org",
	}
	cfg, err := parseScalesetConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("parseScalesetConfig: %v", err)
	}
	if cfg.AppID != 123 || cfg.InstallationID != 456 {
		t.Errorf("ids = %d/%d, want 123/456", cfg.AppID, cfg.InstallationID)
	}
	if cfg.ScaleSetName != "gag-probe-scaleset" {
		t.Errorf("default scale set name = %q", cfg.ScaleSetName)
	}
	if cfg.JobTest {
		t.Error("JobTest should default false")
	}
}

func TestParseScalesetConfig_Overrides(t *testing.T) {
	env := map[string]string{
		"GITHUB_APP_ID":              "123",
		"GITHUB_APP_INSTALLATION_ID": "456",
		"GITHUB_APP_PRIVATE_KEY":     testRSAPEM(t),
		"GITHUB_ORG_URL":             "https://github.com/test-org",
		"PROBE_SCALESET_NAME":        "custom-name",
		"PROBE_SCALESET_JOB_TEST":    "true",
	}
	cfg, err := parseScalesetConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("parseScalesetConfig: %v", err)
	}
	if cfg.ScaleSetName != "custom-name" || !cfg.JobTest {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

func TestParseScalesetConfig_Errors(t *testing.T) {
	base := map[string]string{
		"GITHUB_APP_ID":              "123",
		"GITHUB_APP_INSTALLATION_ID": "456",
		"GITHUB_APP_PRIVATE_KEY":     testRSAPEM(t),
		"GITHUB_ORG_URL":             "https://github.com/test-org",
	}
	for _, tc := range []struct {
		name    string
		mutate  func(map[string]string)
		wantErr string
	}{
		{"missing app id", func(m map[string]string) { delete(m, "GITHUB_APP_ID") }, "GITHUB_APP_ID"},
		{"bad app id", func(m map[string]string) { m["GITHUB_APP_ID"] = "abc" }, "parse GITHUB_APP_ID"},
		{"missing installation", func(m map[string]string) { delete(m, "GITHUB_APP_INSTALLATION_ID") }, "GITHUB_APP_INSTALLATION_ID"},
		{"bad installation", func(m map[string]string) { m["GITHUB_APP_INSTALLATION_ID"] = "x" }, "parse GITHUB_APP_INSTALLATION_ID"},
		{"missing key", func(m map[string]string) { delete(m, "GITHUB_APP_PRIVATE_KEY") }, "GITHUB_APP_PRIVATE_KEY"},
		{"missing org url", func(m map[string]string) { delete(m, "GITHUB_ORG_URL") }, "GITHUB_ORG_URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			tc.mutate(env)
			_, err := parseScalesetConfig(func(k string) string { return env[k] })
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestScalesetProbe_ReportsRawWireTheClientHides is the Q362 acceptance check: the
// probe drives the shipping scaleset client, and must still report the wire the
// client's typed API discards. A 202 poll is the sharpest case — GetMessage returns
// (nil, nil), so without the observer hook the probe would have nothing to say about
// the long-poll's status, latency, or rate-limit headers.
func TestScalesetProbe_ReportsRawWireTheClientHides(t *testing.T) {
	srv := newScalesetStub(t) // no jobs queued: every poll answers 202
	srv.SetRateLimitRemaining(4999)

	var logs bytes.Buffer
	p := newScalesetProbeLogging(t, srv, scalesetConfig{}, &logs)

	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := logs.String()
	for _, want := range []string{
		"op=GetMessage",              // the poll was observed at all
		"status=202",                 // …with the status GetMessage collapses
		"X-Ratelimit-Remaining:4999", // …and the U4 rate-limit evidence
		"op=RegistrationToken",       // both bootstrap hops are visible
		"op=RunnerRegistration",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("probe never reported %q; log was:\n%s", want, got)
		}
	}
	// The queue URL's query carries a signature — it must never reach the log.
	if strings.Contains(got, "path=/queue/1/message?") {
		t.Errorf("wire log leaked the queue query string:\n%s", got)
	}
}

// TestScalesetProbe_JobTestReportsAcquireRouteDivergence pins the probe's most
// valuable independent assertion: when the route the scaleset client always targets
// fails but the acquireJobUrl delivered on the message succeeds, that is a
// library-vs-wire divergence and the probe must say so — trying the client's own
// construction FIRST is what makes the finding meaningful (Q362).
func TestScalesetProbe_JobTestReportsAcquireRouteDivergence(t *testing.T) {
	srv := newScalesetStub(t)
	srv.EnableGHESAcquireFlow()
	// The static _apis/runtime acquire route 404s, as the broker-host tenant was
	// observed to do live, while the delivered acquireJobUrl still claims.
	srv.FailStaticAcquireRoute(true)
	ids := srv.PrequeueJobs(1)

	var logs bytes.Buffer
	p := newScalesetProbeLogging(t, srv, scalesetConfig{JobTest: true}, &logs)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(logs.String(), "DIVERGENCE") {
		t.Errorf("probe did not report the acquire-route divergence; log was:\n%s", logs.String())
	}
	// The client's construction must have been attempted before the fallback.
	calls := srv.Calls()
	ci := callIndex(calls, "acquirejobs id=1 auth=queue ids="+formatIDs(ids))
	fi := callIndex(calls, "acquirejobs-queuehost id=1 auth=queue ids="+formatIDs(ids))
	if ci < 0 || fi < 0 || ci > fi {
		t.Errorf("client construction must be tried before the delivered URL; calls:\n%s",
			strings.Join(calls, "\n"))
	}
}

// TestScalesetProbe_NoDivergenceReportedWhenTheClientRouteWorks is the negative half:
// on a backend that honours the client's route, the fallback must not fire and no
// divergence must be claimed.
func TestScalesetProbe_NoDivergenceReportedWhenTheClientRouteWorks(t *testing.T) {
	srv := newScalesetStub(t)
	srv.EnableGHESAcquireFlow()
	ids := srv.PrequeueJobs(1)

	var logs bytes.Buffer
	p := newScalesetProbeLogging(t, srv, scalesetConfig{JobTest: true}, &logs)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(logs.String(), "DIVERGENCE") {
		t.Errorf("no divergence exists; probe must not claim one:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "acquirejobs (client construction)") {
		t.Errorf("probe must report the client-construction acquire result:\n%s", logs.String())
	}
	// The offered job was claimed on the client's route, so the delivered-URL
	// fallback must never have been reached.
	if n := countCalls(srv.Calls(), "acquirejobs-queuehost id=1 auth=queue ids="+formatIDs(ids)); n != 0 {
		t.Errorf("the delivered-URL fallback must not fire; calls:\n%s",
			strings.Join(srv.Calls(), "\n"))
	}
}

func TestDiagnosticHeaders_SelectsTheInvestigationHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-GitHub-Request-Id", "abc")
	h.Set("X-RateLimit-Remaining", "4999")
	h.Set("Retry-After", "30")
	h.Set("Content-Type", "application/json")
	h.Set("Date", "Thu, 23 Jul 2026 00:00:00 GMT")

	got := diagnosticHeaders(h)
	for _, want := range []string{"X-Github-Request-Id", "X-Ratelimit-Remaining", "Retry-After"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	for _, unwanted := range []string{"Content-Type", "Date"} {
		if _, ok := got[unwanted]; ok {
			t.Errorf("%s must not be reported: %v", unwanted, got)
		}
	}
}

func TestQueueBase(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"https://broker.example/scalesets/message?sig=x", "https://broker.example/scalesets"},
		{"https://broker.example/message", "https://broker.example/message"},
		{"://not a url", "://not a url"},
	} {
		if got := queueBase(tc.in); got != tc.want {
			t.Errorf("queueBase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStatsString_NilIsRendered(t *testing.T) {
	if got := statsString(nil); got != "<none>" {
		t.Errorf("statsString(nil) = %q, want <none>", got)
	}
	got := statsString(&scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 3})
	if !strings.Contains(got, "TotalAssignedJobs:3") {
		t.Errorf("statsString did not render the snapshot: %q", got)
	}
}
