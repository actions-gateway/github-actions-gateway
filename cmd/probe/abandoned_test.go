package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // SHA-1 required by the broker's .NET RSA-OAEP session-key wrapping
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/actions-gateway/github-actions-gateway/broker/brokertest"
)

func testAbandonedEnv(t *testing.T, overrides map[string]string) func(string) string {
	t.Helper()
	env := map[string]string{
		"GITHUB_APP_ID":              "12345",
		"GITHUB_APP_PRIVATE_KEY":     testRSAPEM(t),
		"GITHUB_APP_INSTALLATION_ID": "67890",
		"GITHUB_ORG_URL":             "https://github.com/my-org/my-repo",
	}
	for k, v := range overrides {
		env[k] = v
	}
	return func(key string) string { return env[key] }
}

func TestParseAbandonedConfig_Defaults(t *testing.T) {
	t.Parallel()
	cfg, err := parseAbandonedConfig(testAbandonedEnv(t, nil))
	if err != nil {
		t.Fatalf("parseAbandonedConfig: %v", err)
	}
	if cfg.Owner != "my-org" || cfg.Repo != "my-repo" {
		t.Errorf("owner/repo = %q/%q, want my-org/my-repo", cfg.Owner, cfg.Repo)
	}
	if cfg.Label != "gag-q645-abandoned" {
		t.Errorf("Label = %q, want gag-q645-abandoned", cfg.Label)
	}
	if cfg.WorkflowFile != "q645-abandoned-probe.yml" {
		t.Errorf("WorkflowFile = %q, want q645-abandoned-probe.yml", cfg.WorkflowFile)
	}
	if cfg.RunnerVersion != "2.335.1" {
		t.Errorf("RunnerVersion = %q, want 2.335.1", cfg.RunnerVersion)
	}
	if cfg.Result != broker.TaskResultAbandoned {
		t.Errorf("Result = %q, want abandoned", cfg.Result)
	}
	if cfg.RerunCheck {
		t.Error("RerunCheck = true, want false by default")
	}
	if cfg.Timeout != 5*time.Minute || cfg.Window != 20*time.Minute {
		t.Errorf("Timeout/Window = %v/%v, want 5m/20m", cfg.Timeout, cfg.Window)
	}
}

func TestParseAbandonedConfig_RejectsOrgURL(t *testing.T) {
	t.Parallel()
	_, err := parseAbandonedConfig(testAbandonedEnv(t, map[string]string{
		"GITHUB_ORG_URL": "https://github.com/my-org",
	}))
	if err == nil || !strings.Contains(err.Error(), "repository URL") {
		t.Fatalf("expected repository-URL error for an org URL, got %v", err)
	}
}

func TestParseAbandonedConfig_Overrides(t *testing.T) {
	t.Parallel()
	cfg, err := parseAbandonedConfig(testAbandonedEnv(t, map[string]string{
		"PROBE_ABANDONED_LABEL":          "my-label",
		"PROBE_ABANDONED_RESULT":         "failed",
		"PROBE_ABANDONED_RERUN_CHECK":    "true",
		"PROBE_ABANDONED_WORKFLOW":       "other.yml",
		"PROBE_ABANDONED_RUNNER_VERSION": "2.999.0",
		"PROBE_ABANDONED_TIMEOUT":        "30s",
		"PROBE_ABANDONED_WINDOW":         "1m",
	}))
	if err != nil {
		t.Fatalf("parseAbandonedConfig: %v", err)
	}
	if cfg.Label != "my-label" || cfg.WorkflowFile != "other.yml" || cfg.RunnerVersion != "2.999.0" {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if cfg.Result != broker.TaskResultFailed || !cfg.RerunCheck {
		t.Errorf("Result/RerunCheck = %q/%v, want failed/true", cfg.Result, cfg.RerunCheck)
	}
	if cfg.Timeout != 30*time.Second || cfg.Window != time.Minute {
		t.Errorf("Timeout/Window = %v/%v, want 30s/1m", cfg.Timeout, cfg.Window)
	}
}

// makeJITBlob builds an encoded_jit_config blob the way GitHub does: an outer
// base64 JSON object whose values are the base64-encoded runner config files.
func makeJITBlob(t *testing.T, key *rsa.PrivateKey, serverURLV2, authURL, clientID string) string {
	t.Helper()
	b64 := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal jit file: %v", err)
		}
		return base64.StdEncoding.EncodeToString(b)
	}
	enc := base64.StdEncoding.EncodeToString
	files := map[string]string{
		".runner": b64(map[string]string{
			"serverUrl":   "https://legacy.example",
			"serverUrlV2": serverURLV2,
		}),
		".credentials": b64(map[string]any{
			"scheme": "OAuth",
			"data":   map[string]string{"clientId": clientID, "authorizationUrl": authURL},
		}),
		".credentials_rsaparams": b64(map[string]string{
			"modulus":  enc(key.PublicKey.N.Bytes()),
			"exponent": enc([]byte{1, 0, 1}),
			"d":        enc(key.D.Bytes()),
			"p":        enc(key.Primes[0].Bytes()),
			"q":        enc(key.Primes[1].Bytes()),
		}),
	}
	outer, err := json.Marshal(files)
	if err != nil {
		t.Fatalf("marshal jit blob: %v", err)
	}
	return base64.StdEncoding.EncodeToString(outer)
}

func TestParseJITBlob_RoundTrip(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	blob := makeJITBlob(t, key, "https://broker.example/", "https://auth.example/token", "client-1")

	r, err := parseJITBlob(42, "runner-a", blob)
	if err != nil {
		t.Fatalf("parseJITBlob: %v", err)
	}
	if r.ID != 42 || r.Name != "runner-a" {
		t.Errorf("identity = %d/%q, want 42/runner-a", r.ID, r.Name)
	}
	if r.BrokerURL != "https://broker.example/" {
		t.Errorf("BrokerURL = %q, want the serverUrlV2 value", r.BrokerURL)
	}
	if r.ClientID != "client-1" || r.AuthorizationURL != "https://auth.example/token" {
		t.Errorf("credentials = %q/%q", r.ClientID, r.AuthorizationURL)
	}
	if r.Key.D.Cmp(key.D) != 0 {
		t.Error("reconstructed RSA key does not match the one the blob was built from")
	}
}

func TestParseJITBlob_FallsBackToServerURL(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	blob := makeJITBlob(t, key, "", "https://auth.example/token", "client-1")
	r, err := parseJITBlob(1, "r", blob)
	if err != nil {
		t.Fatalf("parseJITBlob: %v", err)
	}
	if r.BrokerURL != "https://legacy.example" {
		t.Errorf("BrokerURL = %q, want the serverUrl fallback", r.BrokerURL)
	}
}

// abandonedRESTStub serves the GitHub REST surface Investigation H touches:
// JIT registration, runner listing/deletion, the fixture run lookup, the job
// status poll, the run cancel, and the runner OAuth token exchange.
type abandonedRESTStub struct {
	srv *httptest.Server

	// jobStatus is consulted on every GET /actions/jobs/{id}; swap it to drive
	// the job-level CONCLUDED path.
	jobStatus atomic.Value // func() (status, conclusion string)
	// runStatus is consulted on every GET /actions/runs/{id}; swap it to drive
	// the run-level CONCLUDED path — the one the 2026-08-04 live run took.
	runStatus atomic.Value // func() (status, conclusion string)
	// runners backs GET /actions/runners; seed it to exercise the stale-runner
	// report and the 409 name-conflict recovery.
	runners atomic.Value // []restRunner
	// conflictOnce makes the next generate-jitconfig answer 409, as GitHub does
	// when a record with the requested name survives an interrupted run.
	conflictOnce atomic.Bool

	// rerunStatus is the status code POST …/rerun-failed-jobs answers with
	// (default 201).
	rerunStatus atomic.Int32

	registrations atomic.Int64
	deregistered  atomic.Int64
	cancels       atomic.Int64
	reruns        atomic.Int64
}

func newAbandonedRESTStub(t *testing.T, key *rsa.PrivateKey, brokerURL string) *abandonedRESTStub {
	t.Helper()
	s := &abandonedRESTStub{}
	s.jobStatus.Store(func() (string, string) { return "queued", "" })
	s.runStatus.Store(func() (string, string) { return "queued", "" })
	s.runners.Store([]restRunner{})
	s.rerunStatus.Store(int32(http.StatusCreated))

	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "broker-token", "token_type": "Bearer"})
	})
	mux.HandleFunc("POST /repos/my-org/my-repo/actions/runners/generate-jitconfig", func(w http.ResponseWriter, _ *http.Request) {
		if s.conflictOnce.CompareAndSwap(true, false) {
			w.WriteHeader(http.StatusConflict)
			return
		}
		id := 100 + s.registrations.Add(1)
		blob := makeJITBlob(t, key, brokerURL, s.srv.URL+"/oauth/token", fmt.Sprintf("client-%d", id))
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runner":             map[string]any{"id": id},
			"encoded_jit_config": blob,
		})
	})
	mux.HandleFunc("GET /repos/my-org/my-repo/actions/runners", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"runners": s.runners.Load()})
	})
	mux.HandleFunc("DELETE /repos/my-org/my-repo/actions/runners/{id}", func(w http.ResponseWriter, _ *http.Request) {
		s.deregistered.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /repos/my-org/my-repo/actions/workflows/q645-abandoned-probe.yml/runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workflow_runs": []map[string]any{
				{"id": 7, "status": "queued", "created_at": "2026-08-03T00:00:00Z"},
			},
		})
	})
	mux.HandleFunc("GET /repos/my-org/my-repo/actions/runs/7/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jobs": []map[string]any{{"id": 99, "status": "queued"}},
		})
	})
	mux.HandleFunc("GET /repos/my-org/my-repo/actions/jobs/99", func(w http.ResponseWriter, _ *http.Request) {
		status, conclusion := s.jobStatus.Load().(func() (string, string))()
		_ = json.NewEncoder(w).Encode(map[string]string{"status": status, "conclusion": conclusion})
	})
	mux.HandleFunc("GET /repos/my-org/my-repo/actions/runs/7", func(w http.ResponseWriter, _ *http.Request) {
		status, conclusion := s.runStatus.Load().(func() (string, string))()
		_ = json.NewEncoder(w).Encode(map[string]string{"status": status, "conclusion": conclusion})
	})
	mux.HandleFunc("POST /repos/my-org/my-repo/actions/runs/7/cancel", func(w http.ResponseWriter, _ *http.Request) {
		s.cancels.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /repos/my-org/my-repo/actions/runs/7/rerun-failed-jobs", func(w http.ResponseWriter, _ *http.Request) {
		s.reruns.Add(1)
		w.WriteHeader(int(s.rerunStatus.Load()))
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// startAbandonedRun wires the full scenario against brokertest + the REST stub
// and runs it on a goroutine, returning the stubs and a channel carrying the
// verdict.
func startAbandonedRun(t *testing.T, window time.Duration, env map[string]string, configure func(*abandonedRESTStub)) (*brokertest.Server, *abandonedRESTStub, chan string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	bs := brokertest.New()
	t.Cleanup(bs.Close)
	rest := newAbandonedRESTStub(t, key, bs.URL)
	if configure != nil {
		configure(rest)
	}

	overrides := map[string]string{
		"PROBE_ABANDONED_TIMEOUT": "10s",
		"PROBE_ABANDONED_WINDOW":  window.String(),
	}
	for k, v := range env {
		overrides[k] = v
	}
	cfg, err := parseAbandonedConfig(testAbandonedEnv(t, overrides))
	if err != nil {
		t.Fatalf("parseAbandonedConfig: %v", err)
	}
	p := newAbandonedProbe(discardLogger(), cfg, staticTokenProvider{token: "install-token"},
		rest.srv.URL, bs.HTTPClient(), bs.HTTPClient())
	p.restPollInterval = 100 * time.Millisecond
	p.rerunWait = 3 * time.Second

	// The fixture delivery for A. Session IDs are minted sequentially by the
	// stub, and the probe opens A's session first.
	bs.EnqueueJob("session-1", broker.RunnerJobRequestBody{
		RunnerRequestID: "req-a",
		BillingOwnerID:  "billing-1",
	})

	verdictCh := make(chan string, 1)
	go func() {
		verdict, err := p.run(context.Background())
		if err != nil {
			t.Errorf("run: %v", err)
		}
		verdictCh <- verdict
	}()
	return bs, rest, verdictCh
}

// waitForCompleteJob blocks until the stub has served the abandoned completejob.
func waitForCompleteJob(t *testing.T, bs *brokertest.Server) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for bs.CompleteJobCalls() == 0 {
		select {
		case <-deadline:
			t.Fatal("completejob never reached the broker stub")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestAbandonedProbe_Redispatched(t *testing.T) {
	t.Parallel()
	bs, rest, verdictCh := startAbandonedRun(t, 30*time.Second, nil, nil)

	waitForCompleteJob(t, bs)
	// The re-dispatch: a fresh delivery reaches B's session after T0.
	bs.EnqueueJob("session-2", broker.RunnerJobRequestBody{RunnerRequestID: "req-b"})

	select {
	case verdict := <-verdictCh:
		if verdict != verdictRedispatched {
			t.Fatalf("verdict = %q, want %q", verdict, verdictRedispatched)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("probe did not reach a verdict")
	}

	// The measurement's own claim: the completion the stub saw is the Q628 call.
	req, ok := bs.LastCompleteJob()
	if !ok {
		t.Fatal("no completejob recorded")
	}
	if req.Result != broker.TaskResultAbandoned {
		t.Errorf("completejob result = %q, want abandoned", req.Result)
	}
	if req.JobID != "req-a" {
		t.Errorf("completejob jobId = %q, want the acquired delivery's request id req-a", req.JobID)
	}
	if got := rest.cancels.Load(); got != 1 {
		t.Errorf("cleanup cancels = %d, want 1", got)
	}
	if got := rest.deregistered.Load(); got != 2 {
		t.Errorf("deregistered runners = %d, want 2 (A at recycle, B at cleanup)", got)
	}
}

// TestAbandonedProbe_ConcludedRunLevel drives the outcome the 2026-08-04 live
// run measured: the run concludes success while the job record never does.
func TestAbandonedProbe_ConcludedRunLevel(t *testing.T) {
	t.Parallel()
	bs, rest, verdictCh := startAbandonedRun(t, 30*time.Second, nil, nil)

	waitForCompleteJob(t, bs)
	// No redelivery; the run goes terminal while the job stays in_progress.
	rest.jobStatus.Store(func() (string, string) { return "in_progress", "" })
	rest.runStatus.Store(func() (string, string) { return "completed", "success" })

	select {
	case verdict := <-verdictCh:
		if verdict != verdictConcluded+"-run-success" {
			t.Fatalf("verdict = %q, want %s-run-success", verdict, verdictConcluded)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("probe did not reach a verdict")
	}
	if got := rest.cancels.Load(); got != 1 {
		t.Errorf("cleanup cancels = %d, want 1 (409-tolerant cancel still issued)", got)
	}
}

// TestAbandonedProbe_SiblingRedeliveryIsNotRedispatch: a fan-out sibling the
// observer saw before T0 keeps redelivering unacked after T0 (measured
// 2026-08-04); its request id must not produce a REDISPATCHED verdict.
func TestAbandonedProbe_SiblingRedeliveryIsNotRedispatch(t *testing.T) {
	t.Parallel()
	bs, _, verdictCh := startAbandonedRun(t, 3*time.Second, nil, nil)

	// The pre-T0 sibling delivery on B's session.
	bs.EnqueueJob("session-2", broker.RunnerJobRequestBody{RunnerRequestID: "req-sibling"})
	waitForCompleteJob(t, bs)
	// The same sibling redelivered after T0: fan-out noise, not a re-dispatch.
	bs.EnqueueJob("session-2", broker.RunnerJobRequestBody{RunnerRequestID: "req-sibling"})

	select {
	case verdict := <-verdictCh:
		if verdict != verdictNoSignal {
			t.Fatalf("verdict = %q, want %q (sibling redelivery filtered)", verdict, verdictNoSignal)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("probe did not reach a verdict")
	}
}

// TestAbandonedProbe_RerunCheck drives the Q676 remedy measurement end to end:
// completejob(failed), the run concludes failure, rerun-failed-jobs is accepted,
// and the re-queued job reaches the surviving listener.
func TestAbandonedProbe_RerunCheck(t *testing.T) {
	t.Parallel()
	bs, rest, verdictCh := startAbandonedRun(t, 30*time.Second, map[string]string{
		"PROBE_ABANDONED_RESULT":      "failed",
		"PROBE_ABANDONED_RERUN_CHECK": "true",
	}, nil)

	waitForCompleteJob(t, bs)
	rest.jobStatus.Store(func() (string, string) { return "in_progress", "" })
	rest.runStatus.Store(func() (string, string) { return "completed", "failure" })

	// Once the probe posts the rerun, re-queue the job to B's session.
	deadline := time.After(10 * time.Second)
	for rest.reruns.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("rerun-failed-jobs never reached the REST stub")
		case <-time.After(20 * time.Millisecond):
		}
	}
	bs.EnqueueJob("session-2", broker.RunnerJobRequestBody{RunnerRequestID: "req-a2"})

	select {
	case verdict := <-verdictCh:
		if verdict != verdictConcluded+"-run-failure" {
			t.Fatalf("verdict = %q, want %s-run-failure", verdict, verdictConcluded)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("probe did not reach a verdict")
	}
	req, ok := bs.LastCompleteJob()
	if !ok {
		t.Fatal("no completejob recorded")
	}
	if req.Result != broker.TaskResultFailed {
		t.Errorf("completejob result = %q, want failed", req.Result)
	}
	if got := rest.reruns.Load(); got != 1 {
		t.Errorf("rerun-failed-jobs calls = %d, want 1", got)
	}
}

// TestAbandonedProbe_RerunRefused: a non-2xx rerun-failed-jobs answer is a
// recorded outcome, not a failure — the probe still finishes with the window
// verdict and cleans up.
func TestAbandonedProbe_RerunRefused(t *testing.T) {
	t.Parallel()
	bs, rest, verdictCh := startAbandonedRun(t, 30*time.Second, map[string]string{
		"PROBE_ABANDONED_RESULT":      "failed",
		"PROBE_ABANDONED_RERUN_CHECK": "true",
	}, func(s *abandonedRESTStub) {
		s.rerunStatus.Store(int32(http.StatusForbidden))
	})

	waitForCompleteJob(t, bs)
	rest.runStatus.Store(func() (string, string) { return "completed", "failure" })

	select {
	case verdict := <-verdictCh:
		if verdict != verdictConcluded+"-run-failure" {
			t.Fatalf("verdict = %q, want %s-run-failure", verdict, verdictConcluded)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("probe did not reach a verdict")
	}
	if got := rest.reruns.Load(); got != 1 {
		t.Errorf("rerun-failed-jobs calls = %d, want 1", got)
	}
	if got := rest.cancels.Load(); got != 1 {
		t.Errorf("cleanup cancels = %d, want 1", got)
	}
}

func TestAbandonedProbe_ConcludedJobLevel(t *testing.T) {
	t.Parallel()
	bs, rest, verdictCh := startAbandonedRun(t, 30*time.Second, nil, nil)

	waitForCompleteJob(t, bs)
	// No redelivery; instead the REST job goes terminal.
	rest.jobStatus.Store(func() (string, string) { return "completed", "cancelled" })

	select {
	case verdict := <-verdictCh:
		if verdict != verdictConcluded+"-job-cancelled" {
			t.Fatalf("verdict = %q, want %s-job-cancelled", verdict, verdictConcluded)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("probe did not reach a verdict")
	}
}

func TestAbandonedProbe_NoSignal(t *testing.T) {
	t.Parallel()
	bs, _, verdictCh := startAbandonedRun(t, 2*time.Second, nil, nil)

	waitForCompleteJob(t, bs)
	// Nothing happens on either channel; the window closes.
	select {
	case verdict := <-verdictCh:
		if verdict != verdictNoSignal {
			t.Fatalf("verdict = %q, want %q", verdict, verdictNoSignal)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("probe did not reach a verdict")
	}
}

// TestAbandonedProbe_RecoversRunnerNameConflict drives the 409 path: a runner
// record surviving an interrupted run answers the first generate-jitconfig with
// a conflict, and the probe deletes the survivor and retries. The seeded stale
// record also exercises the startup stale-runner report.
func TestAbandonedProbe_RecoversRunnerNameConflict(t *testing.T) {
	t.Parallel()
	bs, rest, verdictCh := startAbandonedRun(t, 30*time.Second, nil, func(s *abandonedRESTStub) {
		s.conflictOnce.Store(true)
		s.runners.Store([]restRunner{{
			ID: 55, Name: "gag-q645-abandoned-a", Status: "offline",
			Labels: []struct {
				Name string `json:"name"`
			}{{Name: "gag-q645-abandoned"}},
		}})
	})

	waitForCompleteJob(t, bs)
	bs.EnqueueJob("session-2", broker.RunnerJobRequestBody{RunnerRequestID: "req-b"})

	select {
	case verdict := <-verdictCh:
		if verdict != verdictRedispatched {
			t.Fatalf("verdict = %q, want %q after conflict recovery", verdict, verdictRedispatched)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("probe did not reach a verdict")
	}
	// Three deletes: the stale survivor, A at the recycle, B at cleanup.
	if got := rest.deregistered.Load(); got != 3 {
		t.Errorf("deregistered = %d, want 3 (stale survivor + A + B)", got)
	}
}

// encryptMessageBody is the inverse of broker.DecryptMessageBody:
// base64(IV || AES-256-CBC(PKCS#7-padded plaintext)).
func encryptMessageBody(t *testing.T, plaintext, key []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes cipher: %v", err)
	}
	padLen := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append(append([]byte{}, plaintext...), bytes.Repeat([]byte{byte(padLen)}, padLen)...)
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		t.Fatalf("iv: %v", err)
	}
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	return base64.StdEncoding.EncodeToString(append(iv, ct...))
}

func TestDecodeJobRequest_EncryptedBody(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("key: %v", err)
	}
	plain, _ := json.Marshal(broker.RunnerJobRequestBody{
		RunnerRequestID: "req-enc",
		RunServiceURL:   "https://run.example",
	})
	p := newAbandonedProbe(discardLogger(), abandonedConfig{}, staticTokenProvider{}, "", http.DefaultClient, http.DefaultClient)
	sess := &brokerSession{aesKey: key}

	body, err := p.decodeJobRequest(sess, &broker.TaskAgentMessage{
		Body: encryptMessageBody(t, plain, key),
	})
	if err != nil {
		t.Fatalf("decodeJobRequest: %v", err)
	}
	if body.RunnerRequestID != "req-enc" || body.RunServiceURL != "https://run.example" {
		t.Errorf("decoded = %+v, want the encrypted payload's fields", body)
	}

	// A wrong key must surface as a decrypt error, not silently parse garbage.
	wrongKey := make([]byte, 32)
	sess.aesKey = wrongKey
	if _, err := p.decodeJobRequest(sess, &broker.TaskAgentMessage{
		Body: encryptMessageBody(t, plain, key),
	}); err == nil {
		t.Fatal("decodeJobRequest with the wrong key: expected error, got nil")
	}
}

// TestRunAbandonedProbe_NoDelivery covers the wired entry point and the
// delivery-timeout branch: with no fixture job queued, the probe reports what
// was missing rather than hanging.
func TestRunAbandonedProbe_NoDelivery(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	bs := brokertest.New()
	t.Cleanup(bs.Close)
	rest := newAbandonedRESTStub(t, key, bs.URL)

	cfg, err := parseAbandonedConfig(testAbandonedEnv(t, map[string]string{
		"PROBE_ABANDONED_TIMEOUT": "1s",
		"PROBE_ABANDONED_WINDOW":  "2s",
	}))
	if err != nil {
		t.Fatalf("parseAbandonedConfig: %v", err)
	}
	err = runAbandonedProbe(context.Background(), discardLogger(), cfg,
		staticTokenProvider{token: "install-token"}, rest.srv.URL)
	if err == nil || !strings.Contains(err.Error(), "no RunnerJobRequest") {
		t.Fatalf("expected the delivery-timeout error, got %v", err)
	}
}

func TestParseAbandonedConfig_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		overrides map[string]string
		clear     string
	}{
		{name: "missing app id", clear: "GITHUB_APP_ID"},
		{name: "missing key", clear: "GITHUB_APP_PRIVATE_KEY"},
		{name: "missing installation id", clear: "GITHUB_APP_INSTALLATION_ID"},
		{name: "missing org url", clear: "GITHUB_ORG_URL"},
		{name: "bad app id", overrides: map[string]string{"GITHUB_APP_ID": "not-a-number"}},
		{name: "bad installation id", overrides: map[string]string{"GITHUB_APP_INSTALLATION_ID": "nope"}},
		{name: "bad timeout", overrides: map[string]string{"PROBE_ABANDONED_TIMEOUT": "soon"}},
		{name: "bad window", overrides: map[string]string{"PROBE_ABANDONED_WINDOW": "later"}},
		{name: "bad result", overrides: map[string]string{"PROBE_ABANDONED_RESULT": "exploded"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			overrides := map[string]string{}
			for k, v := range tc.overrides {
				overrides[k] = v
			}
			if tc.clear != "" {
				overrides[tc.clear] = ""
			}
			if _, err := parseAbandonedConfig(testAbandonedEnv(t, overrides)); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestParseJITBlob_Errors(t *testing.T) {
	t.Parallel()
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	outer := func(files map[string]string) string {
		b, err := json.Marshal(files)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.StdEncoding.EncodeToString(b)
	}
	cases := []struct {
		name string
		blob string
	}{
		{name: "not base64", blob: "%%%"},
		{name: "outer not json", blob: b64("not json")},
		{name: "runner file not json", blob: outer(map[string]string{
			".runner": b64("nope"), ".credentials": b64("{}"), ".credentials_rsaparams": b64("{}"),
		})},
		{name: "credentials not json", blob: outer(map[string]string{
			".runner": b64("{}"), ".credentials": b64("nope"), ".credentials_rsaparams": b64("{}"),
		})},
		{name: "rsaparams not json", blob: outer(map[string]string{
			".runner": b64("{}"), ".credentials": b64("{}"), ".credentials_rsaparams": b64("nope"),
		})},
		{name: "rsaparams empty", blob: outer(map[string]string{
			".runner": b64("{}"), ".credentials": b64("{}"), ".credentials_rsaparams": b64("{}"),
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseJITBlob(1, "r", tc.blob); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestHostOf(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"https://broker.example/path": "broker.example",
		"https://broker.example":      "broker.example",
		"not a url":                   "not a url",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOpenSession_KeyBranches drives openSession's three session-key arms
// against a hand-rolled broker /session stub: RSA-OAEP-encrypted (the live
// GitHub shape), unencrypted, and the two upstream failure modes.
func TestOpenSession_KeyBranches(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	aesKey := make([]byte, 32)
	if _, err := rand.Read(aesKey); err != nil {
		t.Fatalf("aes key: %v", err)
	}
	encKey, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, &key.PublicKey, aesKey, nil) //nolint:gosec // matches DecryptSessionKey's .NET-mandated OAEP-SHA1
	if err != nil {
		t.Fatalf("oaep encrypt: %v", err)
	}

	sessionResp := atomic.Value{} // map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
	})
	mux.HandleFunc("POST /session", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(sessionResp.Load())
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	p := newAbandonedProbe(discardLogger(), abandonedConfig{RunnerVersion: "2.335.1"},
		staticTokenProvider{}, srv.URL, http.DefaultClient, http.DefaultClient)
	runner := &jitRunner{
		ID: 1, Name: "r", ClientID: "c",
		AuthorizationURL: srv.URL + "/oauth/token",
		BrokerURL:        srv.URL,
		Key:              key,
	}

	sessionResp.Store(map[string]any{
		"sessionId":     "s-enc",
		"encryptionKey": map[string]any{"encrypted": true, "value": encKey},
	})
	sess, err := p.openSession(context.Background(), runner)
	if err != nil {
		t.Fatalf("openSession (encrypted): %v", err)
	}
	if !bytes.Equal(sess.aesKey, aesKey) {
		t.Error("encrypted session key did not decrypt to the original AES key")
	}

	sessionResp.Store(map[string]any{
		"sessionId":     "s-plain",
		"encryptionKey": map[string]any{"encrypted": false, "value": aesKey},
	})
	sess, err = p.openSession(context.Background(), runner)
	if err != nil {
		t.Fatalf("openSession (unencrypted): %v", err)
	}
	if !bytes.Equal(sess.aesKey, aesKey) {
		t.Error("unencrypted session key was not taken as-is")
	}

	// A key the runner's RSA key cannot decrypt degrades to plaintext parsing
	// rather than failing the session.
	sessionResp.Store(map[string]any{
		"sessionId":     "s-bad",
		"encryptionKey": map[string]any{"encrypted": true, "value": []byte("garbage-not-oaep")},
	})
	sess, err = p.openSession(context.Background(), runner)
	if err != nil {
		t.Fatalf("openSession (undecryptable): %v", err)
	}
	if sess.aesKey != nil {
		t.Error("undecryptable session key should leave aesKey nil")
	}

	// Broker OAuth rejection is fatal.
	badRunner := *runner
	badRunner.AuthorizationURL = srv.URL + "/nope"
	if _, err := p.openSession(context.Background(), &badRunner); err == nil {
		t.Fatal("openSession with a rejected OAuth exchange: expected error")
	}

	// CreateSession rejection is fatal.
	badRunner = *runner
	badRunner.BrokerURL = srv.URL + "/nope"
	if _, err := p.openSession(context.Background(), &badRunner); err == nil {
		t.Fatal("openSession with a failed CreateSession: expected error")
	}
}

// TestDeregisterRunner_Statuses drives the tolerated and unexpected DELETE
// answers, and the once-only guard.
func TestDeregisterRunner_Statuses(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	status := atomic.Int64{}
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /repos/o/r/actions/runners/{id}", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(int(status.Load()))
	})
	mux.HandleFunc("POST /repos/o/r/actions/runs/9/cancel", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	p := newAbandonedProbe(discardLogger(), abandonedConfig{Owner: "o", Repo: "r"},
		staticTokenProvider{token: "tok"}, srv.URL, http.DefaultClient, http.DefaultClient)

	status.Store(http.StatusNotFound)
	r := &jitRunner{ID: 5, Name: "gone"}
	p.deregisterRunner(context.Background(), r)
	if got := calls.Load(); got != 1 {
		t.Fatalf("DELETE calls = %d, want 1", got)
	}
	// The guard: a second call must not re-issue the DELETE.
	p.deregisterRunner(context.Background(), r)
	if got := calls.Load(); got != 1 {
		t.Fatalf("DELETE calls after repeat = %d, want still 1", got)
	}

	status.Store(http.StatusUnprocessableEntity)
	p.deregisterRunner(context.Background(), &jitRunner{ID: 6, Name: "busy"})
	if got := calls.Load(); got != 2 {
		t.Fatalf("DELETE calls = %d, want 2", got)
	}

	// cancelFixtureRun's failure branch is log-only.
	p.cancelFixtureRun(context.Background(), 9)
}

// TestResolveFixtureRun_Empty covers the two discovery failures a live run can
// hit: no queued run of the fixture workflow, and a run with no jobs yet.
func TestResolveFixtureRun_Empty(t *testing.T) {
	t.Parallel()
	runsEmpty := atomic.Bool{}
	runsEmpty.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/actions/workflows/wf.yml/runs", func(w http.ResponseWriter, _ *http.Request) {
		if runsEmpty.Load() {
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []any{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workflow_runs": []map[string]any{{"id": 3, "status": "queued", "created_at": "2026-08-03T00:00:00Z"}},
		})
	})
	mux.HandleFunc("GET /repos/o/r/actions/runs/3/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []any{}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	p := newAbandonedProbe(discardLogger(), abandonedConfig{Owner: "o", Repo: "r", WorkflowFile: "wf.yml"},
		staticTokenProvider{token: "tok"}, srv.URL, http.DefaultClient, http.DefaultClient)

	if _, _, err := p.resolveFixtureRun(context.Background()); err == nil || !strings.Contains(err.Error(), "no queued run") {
		t.Fatalf("expected no-queued-run error, got %v", err)
	}
	runsEmpty.Store(false)
	if _, _, err := p.resolveFixtureRun(context.Background()); err == nil || !strings.Contains(err.Error(), "no jobs") {
		t.Fatalf("expected no-jobs error, got %v", err)
	}
	// getJSON's non-200 branch, via a route the stub does not serve.
	if _, _, err := p.fetchJobStatus(context.Background(), 12345); err == nil {
		t.Fatal("expected error for an unserved job route")
	}
}
