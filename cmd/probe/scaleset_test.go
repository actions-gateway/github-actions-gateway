package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// staticTokenProvider satisfies tokenProvider with a fixed token.
type staticTokenProvider struct{ token string }

func (s staticTokenProvider) Token(context.Context) (string, error) { return s.token, nil }

// scalesetFake is an httptest handler faking the full scale-set endpoint
// chain: REST registration-token, the RemoteAuth runner-registration hop, and
// the Actions Service (runnergroups, runnerscalesets CRUD, sessions, the
// message queue, acquirejobs, generatejitconfig). It records the calls it
// serves so tests can assert the orchestration order and cleanup.
type scalesetFake struct {
	t *testing.T

	mu    sync.Mutex
	calls []string

	// URL is set after the httptest server starts (self-referential: the
	// admin connection and queue URL point back at this server).
	URL string

	// queueStatus controls the long-poll response when the script is
	// exhausted (default 202).
	queueStatus int
	// groupsStatus, when non-zero, is returned by the runnergroups lookup
	// instead of the happy-path body (exercises the fallback-to-1 branch).
	groupsStatus int
	// sessionsStatus, when non-zero, is returned by session create
	// (exercises the create-session error path).
	sessionsStatus int
	// wantGroupID is the runnerGroupId create-scaleset must receive
	// (default 7, the id the happy-path lookup returns).
	wantGroupID float64
	// queueScript is consumed one entry per poll; a nil entry means an empty
	// (202) response. When exhausted, polls return queueStatus.
	queueScript []*runnerScaleSetMessage
	queuePolls  int
}

func (f *scalesetFake) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *scalesetFake) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *scalesetFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/actions/runners/registration-token"):
		f.record("registration-token " + path)
		if got := r.Header.Get("Authorization"); got != "Bearer install-token" {
			f.t.Errorf("registration-token auth = %q, want install token", got)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"reg-token","expires_at":"2026-01-01T00:00:00Z"}`))

	case path == "/actions/runner-registration":
		f.record("runner-registration")
		if got := r.Header.Get("Authorization"); got != "RemoteAuth reg-token" {
			f.t.Errorf("runner-registration auth = %q, want RemoteAuth reg-token", got)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"runner_event":"register"`) {
			f.t.Errorf("runner-registration body = %s, want runner_event register", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"url": f.URL, "token": "admin-jwt"})

	case strings.HasPrefix(path, "/_apis/runtime/runnergroups/"):
		f.record("runnergroups")
		f.requireAdmin(r)
		if f.groupsStatus != 0 {
			w.WriteHeader(f.groupsStatus)
			return
		}
		_, _ = w.Write([]byte(`{"count":1,"value":[{"id":7,"name":"Default"}]}`))

	case path == "/_apis/runtime/runnerscalesets" && r.Method == http.MethodGet:
		f.record("get-scaleset name=" + r.URL.Query().Get("name"))
		f.requireAdmin(r)
		_, _ = w.Write([]byte(`{"count":1,"value":[{"id":42,"name":"gag-probe-scaleset","runnerGroupId":7}]}`))

	case path == "/_apis/runtime/runnerscalesets" && r.Method == http.MethodPost:
		f.record("create-scaleset")
		f.requireAdmin(r)
		body, _ := io.ReadAll(r.Body)
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		want := f.wantGroupID
		if want == 0 {
			want = 7
		}
		if in["runnerGroupId"] != want {
			f.t.Errorf("create scale set runnerGroupId = %v, want %v", in["runnerGroupId"], want)
		}
		_, _ = w.Write([]byte(`{"id":42,"name":"gag-probe-scaleset","runnerGroupId":7}`))

	case path == "/_apis/runtime/runnerscalesets/42" && r.Method == http.MethodDelete:
		f.record("delete-scaleset")
		f.requireAdmin(r)
		w.WriteHeader(http.StatusNoContent)

	case path == "/_apis/runtime/runnerscalesets/42/sessions" && r.Method == http.MethodPost:
		f.record("create-session")
		f.requireAdmin(r)
		if f.sessionsStatus != 0 {
			w.WriteHeader(f.sessionsStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessionId":               "11111111-2222-3333-4444-555555555555",
			"ownerName":               "gag-probe",
			"messageQueueUrl":         f.URL + "/queue/42/messages?dummy=1",
			"messageQueueAccessToken": "queue-token",
			"statistics":              map[string]int{"totalAssignedJobs": 0},
		})

	case strings.HasPrefix(path, "/_apis/runtime/runnerscalesets/42/sessions/") && r.Method == http.MethodDelete:
		f.record("delete-session")
		f.requireAdmin(r)
		w.WriteHeader(http.StatusNoContent)

	case strings.HasPrefix(path, "/_apis/runtime/runnerscalesets/42/sessions/") && r.Method == http.MethodPatch:
		f.record("refresh-session")
		f.requireAdmin(r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessionId":               "11111111-2222-3333-4444-555555555555",
			"messageQueueUrl":         f.URL + "/queue/42/messages?dummy=1",
			"messageQueueAccessToken": "queue-token",
		})

	case path == "/queue/42/messages":
		f.record("queue-poll cap=" + r.Header.Get("X-ScaleSetMaxCapacity") +
			" last=" + r.URL.Query().Get("lastMessageId"))
		if got := r.Header.Get("Authorization"); got != "Bearer queue-token" {
			f.t.Errorf("queue poll auth = %q, want queue token", got)
		}
		var msg *runnerScaleSetMessage
		f.mu.Lock()
		if f.queuePolls < len(f.queueScript) {
			msg = f.queueScript[f.queuePolls]
		}
		f.queuePolls++
		f.mu.Unlock()
		if msg == nil {
			status := f.queueStatus
			if status == 0 {
				status = http.StatusAccepted
			}
			w.Header().Set("X-RateLimit-Remaining", "4999")
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(msg)

	case path == "/queue/42/acquirejobs":
		body, _ := io.ReadAll(r.Body)
		f.record("acquirejobs(queue-base) " + strings.TrimSpace(string(body)))
		_ = json.NewEncoder(w).Encode(map[string]any{"count": 0, "value": []int64{}})

	case path == "/_apis/runtime/runnerscalesets/42/acquirablejobs":
		f.record("acquirablejobs")
		_, _ = w.Write([]byte(`{"count":0,"value":[]}`))

	case path == "/_apis/runtime/runnerscalesets/42/acquirejobs":
		body, _ := io.ReadAll(r.Body)
		tokenKind := "other"
		switch r.Header.Get("Authorization") {
		case "Bearer queue-token":
			tokenKind = "queue"
		case "Bearer admin-jwt":
			tokenKind = "admin"
		}
		f.record("acquirejobs(" + tokenKind + ") " + strings.TrimSpace(string(body)))
		var ids []int64
		_ = json.Unmarshal(body, &ids)
		// Echo back only ids below the bogus threshold, mimicking the
		// partial-batch subset response.
		var won []int64
		for _, id := range ids {
			if id < 9999999999 {
				won = append(won, id)
			}
		}
		if won == nil {
			won = []int64{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"count": len(won), "value": won})

	case path == "/_apis/runtime/runnerscalesets/42/generatejitconfig":
		f.record("generatejitconfig")
		f.requireAdmin(r)
		blob := base64.StdEncoding.EncodeToString([]byte(`{".runner":{},".credentials":{}}`))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runner":           map[string]any{"id": 77, "name": "gag-probe-scaleset-runner"},
			"encodedJITConfig": blob,
		})

	default:
		f.t.Errorf("unexpected request: %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *scalesetFake) requireAdmin(r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer admin-jwt" {
		f.t.Errorf("%s auth = %q, want admin JWT", r.URL.Path, got)
	}
	if got := r.URL.Query().Get("api-version"); got != "6.0-preview" {
		f.t.Errorf("%s api-version = %q, want 6.0-preview", r.URL.Path, got)
	}
}

func newScalesetProbeForTest(t *testing.T, fake *scalesetFake, cfg scalesetConfig) (*scalesetProbe, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	fake.URL = srv.URL
	if cfg.ConfigURL == "" {
		cfg.ConfigURL = "https://github.com/test-org"
	}
	if cfg.ScaleSetName == "" {
		cfg.ScaleSetName = "gag-probe-scaleset"
	}
	if cfg.GroupName == "" {
		cfg.GroupName = "Default"
	}
	return &scalesetProbe{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:        cfg,
		provider:   staticTokenProvider{token: "install-token"},
		apiBase:    srv.URL,
		hc:         srv.Client(),
		pollClient: srv.Client(),
	}, srv
}

func TestScalesetProbe_FullFlowAndCleanup(t *testing.T) {
	// The step-7 basic poll gets a 200 message so the decode branch of
	// pollQueueOnce is exercised alongside the empty-202 default.
	statsBody, _ := json.Marshal([]map[string]any{})
	fake := &scalesetFake{
		t: t,
		queueScript: []*runnerScaleSetMessage{
			{MessageID: 1, MessageType: "RunnerScaleSetJobMessages", Body: string(statsBody)},
		},
	}
	p, _ := newScalesetProbeForTest(t, fake, scalesetConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := strings.Join(fake.recorded(), "\n")
	for _, want := range []string{
		"registration-token /orgs/test-org/actions/runners/registration-token",
		"runner-registration",
		"runnergroups",
		"create-scaleset",
		"create-session",
		"queue-poll cap=1 last=0",
		"acquirejobs(queue) []",
		"acquirejobs(queue) [9999999999]",
		"acquirejobs(admin) [9999999999]",
		"acquirablejobs",
		"generatejitconfig",
		"delete-session",
		"delete-scaleset",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("call sequence missing %q; got:\n%s", want, got)
		}
	}
	// Cleanup ordering: session deleted before the scale set.
	if si, ci := strings.Index(got, "delete-session"), strings.Index(got, "delete-scaleset"); si > ci {
		t.Errorf("session must be deleted before the scale set; got:\n%s", got)
	}
}

func TestScalesetProbe_RepoScopedRegistrationToken(t *testing.T) {
	fake := &scalesetFake{t: t}
	p, _ := newScalesetProbeForTest(t, fake, scalesetConfig{ConfigURL: "https://github.com/test-org/test-repo"})

	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := strings.Join(fake.recorded(), "\n")
	if !strings.Contains(got, "registration-token /repos/test-org/test-repo/actions/runners/registration-token") {
		t.Errorf("expected repo-scoped registration-token path; got:\n%s", got)
	}
}

func TestScalesetProbe_JobTestAcquiresAndSeesAssigned(t *testing.T) {
	availableBody, _ := json.Marshal([]map[string]any{
		{"messageType": "JobAvailable", "runnerRequestId": 1234},
	})
	assignedBody, _ := json.Marshal([]map[string]any{
		{"messageType": "JobAssigned", "runnerRequestId": 1234},
	})
	fake := &scalesetFake{
		t: t,
		queueScript: []*runnerScaleSetMessage{
			nil, // step-7 basic poll sees an empty queue
			{MessageID: 1, MessageType: "RunnerScaleSetJobMessages", Body: string(availableBody)},
			{MessageID: 2, MessageType: "RunnerScaleSetJobMessages", Body: string(assignedBody)},
		},
	}
	p, _ := newScalesetProbeForTest(t, fake, scalesetConfig{JobTest: true})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := strings.Join(fake.recorded(), "\n")
	if !strings.Contains(got, "acquirejobs(queue) [1234]") {
		t.Errorf("job test did not acquire the offered id; got:\n%s", got)
	}
	if !strings.Contains(got, "queue-poll cap=1 last=1") {
		t.Errorf("job test did not advance lastMessageId after the first message; got:\n%s", got)
	}
}

func TestScalesetProbe_RegistrationTokenErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	defer srv.Close()

	p := &scalesetProbe{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:        scalesetConfig{ConfigURL: "https://github.com/test-org", ScaleSetName: "x"},
		provider:   staticTokenProvider{token: "install-token"},
		apiBase:    srv.URL,
		hc:         srv.Client(),
		pollClient: srv.Client(),
	}
	err := p.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "registration token") {
		t.Fatalf("want registration token error, got %v", err)
	}
}

func TestScalesetProbe_BadConfigURLPath(t *testing.T) {
	p := &scalesetProbe{
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:      scalesetConfig{ConfigURL: "https://github.com/a/b/c"},
		provider: staticTokenProvider{token: "install-token"},
		hc:       http.DefaultClient,
	}
	_, err := p.registrationToken(context.Background(), "install-token")
	if err == nil || !strings.Contains(err.Error(), "org or owner/repo") {
		t.Fatalf("want org/repo path error, got %v", err)
	}
}

func TestRunScalesetProbe_EntryPoint(t *testing.T) {
	fake := &scalesetFake{t: t}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	fake.URL = srv.URL

	cfg := scalesetConfig{
		ConfigURL:    "https://github.com/test-org",
		ScaleSetName: "gag-probe-scaleset",
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := runScalesetProbe(ctx, logger, cfg, staticTokenProvider{token: "install-token"}, srv.URL); err != nil {
		t.Fatalf("runScalesetProbe: %v", err)
	}
	got := strings.Join(fake.recorded(), "\n")
	if !strings.Contains(got, "delete-scaleset") {
		t.Errorf("entry point did not run to cleanup; got:\n%s", got)
	}
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

	p := &scalesetProbe{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:        scalesetConfig{ConfigURL: "https://github.com/test-org", ScaleSetName: "x"},
		provider:   staticTokenProvider{token: "install-token"},
		apiBase:    srv.URL,
		hc:         srv.Client(),
		pollClient: srv.Client(),
	}
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

	p := &scalesetProbe{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:        scalesetConfig{ConfigURL: "https://github.com/test-org", ScaleSetName: "x"},
		provider:   staticTokenProvider{token: "install-token"},
		apiBase:    srv.URL,
		hc:         srv.Client(),
		pollClient: srv.Client(),
	}
	err := p.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "missing url or token") {
		t.Fatalf("want missing url/token error, got %v", err)
	}
}

func TestScalesetProbe_RunnerGroupFallbackToDefault(t *testing.T) {
	fake := &scalesetFake{t: t, groupsStatus: http.StatusInternalServerError, wantGroupID: 1}
	p, _ := newScalesetProbeForTest(t, fake, scalesetConfig{})

	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := strings.Join(fake.recorded(), "\n")
	if !strings.Contains(got, "create-scaleset") {
		t.Errorf("scale set not created after group fallback; got:\n%s", got)
	}
}

func TestScalesetProbe_SessionCreateFailureStillDeletesScaleSet(t *testing.T) {
	fake := &scalesetFake{t: t, sessionsStatus: http.StatusForbidden}
	p, _ := newScalesetProbeForTest(t, fake, scalesetConfig{})

	err := p.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "create session") {
		t.Fatalf("want create session error, got %v", err)
	}
	got := strings.Join(fake.recorded(), "\n")
	if !strings.Contains(got, "delete-scaleset") {
		t.Errorf("scale set must be deleted on the error path; got:\n%s", got)
	}
	if strings.Contains(got, "delete-session") {
		t.Errorf("no session existed; delete-session must not fire; got:\n%s", got)
	}
}

func TestScalesetProbe_JobTestTimesOutOnEmptyQueue(t *testing.T) {
	fake := &scalesetFake{t: t} // queue always 202
	p, _ := newScalesetProbeForTest(t, fake, scalesetConfig{JobTest: true})
	p.jobTestTimeout = 300 * time.Millisecond

	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := strings.Join(fake.recorded(), "\n")
	if !strings.Contains(got, "delete-scaleset") {
		t.Errorf("timeout path must still clean up; got:\n%s", got)
	}
	// The empty-queue polls must never have acquired anything beyond the two
	// shape probes.
	if strings.Contains(got, "acquirejobs(queue) [1234]") {
		t.Errorf("nothing should be acquired on an empty queue; got:\n%s", got)
	}
}

func TestScalesetProbe_JobTestSkipsNonAvailableEntries(t *testing.T) {
	startedBody, _ := json.Marshal([]map[string]any{
		{"messageType": "JobStarted", "runnerRequestId": 55},
	})
	assignedBody, _ := json.Marshal([]map[string]any{
		{"messageType": "JobAssigned", "runnerRequestId": 55},
	})
	fake := &scalesetFake{
		t: t,
		queueScript: []*runnerScaleSetMessage{
			nil, // step-7 basic poll
			{MessageID: 1, MessageType: "RunnerScaleSetJobMessages", Body: string(startedBody)},
			{MessageID: 2, MessageType: "RunnerScaleSetJobMessages", Body: "not-json"},
			{MessageID: 3, MessageType: "RunnerScaleSetJobMessages", Body: string(assignedBody)},
		},
	}
	p, _ := newScalesetProbeForTest(t, fake, scalesetConfig{JobTest: true})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := strings.Join(fake.recorded(), "\n")
	// JobStarted carries no acquirable id and the bad body is skipped; only
	// the two shape probes may acquire.
	if strings.Contains(got, "acquirejobs(queue) [55]") {
		t.Errorf("JobStarted entry must not be acquired; got:\n%s", got)
	}
	if !strings.Contains(got, "queue-poll cap=1 last=1") ||
		!strings.Contains(got, "queue-poll cap=1 last=2") {
		t.Errorf("lastMessageId must advance across skipped messages; got:\n%s", got)
	}
}

func TestScalesetProbe_CapacityTestSequence(t *testing.T) {
	assigned1, _ := json.Marshal([]map[string]any{
		{"messageType": "JobAssigned", "runnerRequestId": 0, "jobId": "aaa"},
	})
	assigned2, _ := json.Marshal([]map[string]any{
		{"messageType": "JobAssigned", "runnerRequestId": 0, "jobId": "bbb"},
	})
	fake := &scalesetFake{
		t: t,
		queueScript: []*runnerScaleSetMessage{
			nil, // capacity-0 poll: jobs held
			{MessageID: 1, MessageType: "RunnerScaleSetJobMessages", Body: string(assigned1)},
			{MessageID: 2, MessageType: "RunnerScaleSetJobMessages", Body: string(assigned2)},
			{MessageID: 1, MessageType: "RunnerScaleSetJobMessages", Body: string(assigned1)}, // replay
		},
	}
	dir := t.TempDir()
	jit1 := dir + "/jit-1.b64"
	jit2 := dir + "/jit-2.b64"
	p, _ := newScalesetProbeForTest(t, fake, scalesetConfig{
		CapacityTest:   true,
		JITConfigFiles: []string{jit1, jit2},
		HoldSeconds:    1,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := strings.Join(fake.recorded(), "\n")
	for _, want := range []string{
		"queue-poll cap=0 last=0", // phase 1: capacity 0
		"queue-poll cap=1 last=0", // phase 2: capacity 1 (cursor unchanged by 202)
		"queue-poll cap=2 last=1", // phase 3: capacity 2, cursor advanced
		"refresh-session",         // phase 4: PATCH
		"queue-poll cap=2 last=0", // phase 5: replay poll on fresh session
		"generatejitconfig",       // JIT mints
		"delete-scaleset",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("call sequence missing %q; got:\n%s", want, got)
		}
	}
	// Two session creates (initial + replay recreate) and two deletes (replay
	// test + deferred cleanup of the recreated session).
	if n := strings.Count(got, "create-session"); n != 2 {
		t.Errorf("create-session count = %d, want 2; got:\n%s", n, got)
	}
	if n := strings.Count(got, "delete-session"); n != 2 {
		t.Errorf("delete-session count = %d, want 2; got:\n%s", n, got)
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
	fake := &scalesetFake{t: t}
	p, _ := newScalesetProbeForTest(t, fake, scalesetConfig{Cleanup: true})

	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := strings.Join(fake.recorded(), "\n")
	if !strings.Contains(got, "get-scaleset name=gag-probe-scaleset") ||
		!strings.Contains(got, "delete-scaleset") {
		t.Errorf("cleanup mode must look up by name and delete; got:\n%s", got)
	}
	if strings.Contains(got, "create-scaleset") || strings.Contains(got, "create-session") {
		t.Errorf("cleanup mode must not create anything; got:\n%s", got)
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
