package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesetstub"
)

// What these tests can and cannot establish. The stub deletes a scale set and keeps
// its runner records, and the REST job state below is whatever a test sets, so a
// green suite says the scenario stages each arm, reads its evidence, and reports the
// verdict that evidence supports. What github.com actually does is the live run's
// question, and the only one Investigation J exists to answer.

const ssDeleteTestJobID = 7001

// ssDeleteFixture is the stub plus the REST surface it lacks: the fixture workflow's
// runs, its one job, and a delete the test can make the service refuse.
type ssDeleteFixture struct {
	stub *scalesetstub.Stub
	url  string
	hc   *http.Client

	mu         sync.Mutex
	runID      int64
	status     string
	conclusion string

	refuseDelete atomic.Bool
}

func newSSDeleteFixture(t *testing.T) *ssDeleteFixture {
	t.Helper()
	f := &ssDeleteFixture{status: "queued"}
	f.stub = scalesetstub.New(func(*http.Request) string { return f.url })
	f.stub.SetInstallationToken(installToken)
	f.stub.SetPollTimeout(50 * time.Millisecond)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/workflows/{wf}/runs", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.runID == 0 {
			_, _ = fmt.Fprint(w, `{"workflow_runs":[]}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"workflow_runs":[{"id":%d}]}`, f.runID)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runs/{runid}/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"jobs":[{"id":%d}]}`, ssDeleteTestJobID)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/jobs/{jobid}", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"status":%q,"conclusion":%q}`, f.status, f.conclusion)
	})
	mux.HandleFunc("DELETE /_apis/runtime/runnerscalesets/{id}", func(w http.ResponseWriter, r *http.Request) {
		if f.refuseDelete.CompareAndSwap(true, false) {
			http.Error(w, `{"message":"scale set has jobs"}`, http.StatusConflict)
			return
		}
		f.stub.Handler().ServeHTTP(w, r)
	})
	mux.Handle("/", f.stub.Handler())

	srv := httptest.NewUnstartedServer(mux)
	f.url = "http://" + srv.Listener.Addr().String()
	srv.Start()
	t.Cleanup(func() {
		f.stub.Close()
		srv.Close()
	})
	f.hc = srv.Client()
	return f
}

func (f *ssDeleteFixture) setJob(status, conclusion string) {
	f.mu.Lock()
	f.status, f.conclusion = status, conclusion
	f.mu.Unlock()
}

func (f *ssDeleteFixture) setRun(id int64) {
	f.mu.Lock()
	f.runID = id
	f.mu.Unlock()
}

func newSSDeleteProbeForTest(t *testing.T, f *ssDeleteFixture, arm string, log *bytes.Buffer) *ssDeleteProbe {
	t.Helper()
	cfg := ssDeleteConfig{
		RepoURL:      "https://github.com/test-org/test-repo",
		Owner:        "test-org",
		Repo:         "test-repo",
		Arm:          arm,
		ScaleSetName: "gag-q1344-ssdelete",
		GroupName:    "Default",
		WorkflowFile: "q1344-ssdelete-probe.yml",
		Timeout:      time.Second,
		Window:       200 * time.Millisecond,
		Interval:     20 * time.Millisecond,
		Recreate:     true,
	}
	p, err := newSSDeleteProbe(slog.New(slog.NewTextHandler(log, nil)), cfg,
		staticTokenProvider{token: installToken}, f.url, f.hc, f.hc)
	if err != nil {
		t.Fatalf("newSSDeleteProbe: %v", err)
	}
	return p
}

func requireLog(t *testing.T, log, want string) {
	t.Helper()
	if !strings.Contains(log, want) {
		t.Fatalf("log lacks %q:\n%s", want, log)
	}
}

func refuteLog(t *testing.T, log, unwanted string) {
	t.Helper()
	if strings.Contains(log, unwanted) {
		t.Fatalf("log carries %q:\n%s", unwanted, log)
	}
}

// requireNoScaleSet asserts cleanup left nothing registered under the name.
func requireNoScaleSet(t *testing.T, f *ssDeleteFixture, name string) {
	t.Helper()
	if id, ok := f.stub.ScaleSetIDByName(name); ok {
		t.Fatalf("scale set %q (id %d) still registered after the run", name, id)
	}
}

func TestParseSSDeleteConfig(t *testing.T) {
	env := func(over map[string]string) func(string) string {
		m := map[string]string{
			"GITHUB_APP_ID":              "42",
			"GITHUB_APP_INSTALLATION_ID": "99",
			"GITHUB_APP_PRIVATE_KEY":     "-----BEGIN TEST PEM-----\nnot-a-key\n-----END TEST PEM-----",
			"GITHUB_ORG_URL":             "https://github.com/test-org/test-repo",
		}
		for k, v := range over {
			m[k] = v
		}
		return func(k string) string { return m[k] }
	}

	cfg, err := parseSSDeleteConfig(env(nil))
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if cfg.Arm != ssDeleteArmQueued || cfg.ScaleSetName != "gag-q1344-ssdelete" ||
		cfg.WorkflowFile != "q1344-ssdelete-probe.yml" || !cfg.Recreate ||
		cfg.Window != 20*time.Minute || cfg.Owner != "test-org" || cfg.Repo != "test-repo" {
		t.Fatalf("defaults = %+v", cfg)
	}
	if cfg, err = parseSSDeleteConfig(env(map[string]string{"PROBE_SSDELETE_RECREATE": "false"})); err != nil || cfg.Recreate {
		t.Fatalf("PROBE_SSDELETE_RECREATE=false: recreate=%v err=%v", cfg.Recreate, err)
	}
	if _, err := parseSSDeleteConfig(env(map[string]string{"PROBE_SSDELETE_ARM": "drained"})); err == nil {
		t.Fatal("an unknown arm parsed")
	}
	if _, err := parseSSDeleteConfig(env(map[string]string{"GITHUB_ORG_URL": "https://github.com/test-org"})); err == nil {
		t.Fatal("an org URL parsed; the scenario needs a repository")
	}
}

// The queued arm with a job the delete leaves queued, and a successor that receives
// nothing: every measurement reports from its own reading, and cleanup leaves no
// scale set behind, the re-created one included.
func TestSSDelete_QueuedArm_JobLeftQueued_NotInherited(t *testing.T) {
	f := newSSDeleteFixture(t)
	f.setRun(555)
	var log bytes.Buffer
	p := newSSDeleteProbeForTest(t, f, ssDeleteArmQueued, &log)

	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	out := log.String()
	requireLog(t, out, "VERDICT 1 "+verdictSSDeleteAccepted)
	requireLog(t, out, "VERDICT 2 "+verdictJobNotConcluded+" (queued)")
	requireLog(t, out, "VERDICT 4 "+verdictJobNotInherited)
	refuteLog(t, out, "VERDICT 3")
	requireNoScaleSet(t, f, "gag-q1344-ssdelete")
	if !hasCall(f.stub.Calls(), "cancel-run") {
		t.Fatalf("cleanup never cancelled the fixture run: %v", f.stub.Calls())
	}
}

// A successor that receives the deleted scale set's run reports JOB-INHERITED. The
// delivery is matched on the run, so the verdict turns on WHICH job arrived.
func TestSSDelete_QueuedArm_SuccessorInheritsJob(t *testing.T) {
	f := newSSDeleteFixture(t)
	f.setRun(555)
	var log bytes.Buffer
	p := newSSDeleteProbeForTest(t, f, ssDeleteArmQueued, &log)
	p.afterDelete = func() {
		f.stub.SeedMessage([]scaleset.JobMessage{{
			MessageType: scaleset.MessageTypeJobAssigned, JobID: "job-other",
			OwnerName: "test-org", RepositoryName: "test-repo", WorkflowRunID: 556,
		}, {
			MessageType: scaleset.MessageTypeJobAssigned, JobID: "job-555",
			OwnerName: "test-org", RepositoryName: "test-repo", WorkflowRunID: 555,
		}})
	}

	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	out := log.String()
	requireLog(t, out, "assignment for another run ignored")
	requireLog(t, out, "VERDICT 4 "+verdictJobInherited)
	requireLog(t, out, "jobId=job-555")
	requireNoScaleSet(t, f, "gag-q1344-ssdelete")
}

// The assigned arm: the job is assigned through a real session, a JIT runner record
// is minted for it, and the session is gone before T0. A record the stub keeps reads
// RUNNER-LEFT, a job that concludes skips measurement 4, and cleanup deregisters the
// record.
func TestSSDelete_AssignedArm_RunnerLeft_JobConcluded(t *testing.T) {
	f := newSSDeleteFixture(t)
	var log bytes.Buffer
	p := newSSDeleteProbeForTest(t, f, ssDeleteArmAssigned, &log)
	p.afterDelete = func() { f.setJob("completed", "cancelled") }

	go func() {
		for i := 0; i < 200; i++ {
			if id, ok := f.stub.ScaleSetIDByName("gag-q1344-ssdelete"); ok {
				f.stub.Enqueue(id)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	out := log.String()
	requireLog(t, out, "runner record registered")
	requireLog(t, out, "VERDICT 1 "+verdictSSDeleteAccepted)
	requireLog(t, out, "VERDICT 2 "+verdictJobConcluded+" (cancelled)")
	requireLog(t, out, "VERDICT 3 "+verdictRunnerLeft)
	refuteLog(t, out, "VERDICT 4")
	if got := f.stub.RegisteredRunners(); len(got) != 0 {
		t.Fatalf("cleanup left runner records: %v", got)
	}
	requireNoScaleSet(t, f, "gag-q1344-ssdelete")
}

// A refused delete stops the run at measurement 1, since every later measurement is
// about a deleted scale set, and cleanup still removes it.
func TestSSDelete_DeleteRefused_StopsAndCleansUp(t *testing.T) {
	f := newSSDeleteFixture(t)
	f.setRun(555)
	f.refuseDelete.Store(true)
	var log bytes.Buffer
	p := newSSDeleteProbeForTest(t, f, ssDeleteArmQueued, &log)

	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	out := log.String()
	requireLog(t, out, "VERDICT 1 "+verdictSSDeleteRefused)
	refuteLog(t, out, "VERDICT 2")
	requireNoScaleSet(t, f, "gag-q1344-ssdelete")
}

// A scale set already registered under the name is refused rather than reused.
func TestSSDelete_RefusesExistingScaleSet(t *testing.T) {
	f := newSSDeleteFixture(t)
	f.stub.AddScaleSet("gag-q1344-ssdelete", 1)
	var log bytes.Buffer
	p := newSSDeleteProbeForTest(t, f, ssDeleteArmQueued, &log)

	err := p.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("run = %v; want a refusal naming the registered scale set", err)
	}
}

func TestSSDelete_Verdicts(t *testing.T) {
	for _, tc := range []struct{ status, conclusion, want string }{
		{"completed", "failure", verdictJobConcluded + " (failure)"},
		{"queued", "", verdictJobNotConcluded + " (queued)"},
		{"in_progress", "", verdictJobNotConcluded + " (in_progress)"},
		{"", "", verdictJobNotConcluded + " (never read)"},
	} {
		if got := jobVerdict(tc.status, tc.conclusion); got != tc.want {
			t.Errorf("jobVerdict(%q, %q) = %q; want %q", tc.status, tc.conclusion, got, tc.want)
		}
	}
	if runnerVerdict(true) != verdictRunnerRemoved || runnerVerdict(false) != verdictRunnerLeft {
		t.Error("runnerVerdict swapped")
	}
}

func hasCall(calls []string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}
