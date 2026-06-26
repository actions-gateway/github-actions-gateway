// Package compat is the broker-compatibility suite (Q191). It exercises the
// GitHub Actions broker wire protocol and data contracts end to end against an
// in-process broker model, asserting conformance to every documented contract
// in docs/design/03-api-contracts.md (§3.2 credential crypto, §3.3 endpoints,
// §3.4 payload shapes, §3.5 rate-limit handling) plus the live-protocol
// findings confirmed in the Milestone 1 plan §8.
//
// The suite is deliberately credential-free: it drives broker.Client against
// the shared broker stub (broker/brokertest) and small purpose-built httptest
// stubs for the error-status contracts, so it runs in `make check`/CI with no
// secrets and no network. The live-against-real-GitHub probe — the
// credential-gated cmd/probe binary — remains the companion that confirms the
// model matches production; this suite is the repeatable, always-green asset
// that turns a silent wire-protocol break into a visible test failure before
// the v2beta1 API shape is frozen.
//
// This is a test/diagnostic package: it is not imported by the probe binary
// (package main) and never ships in a compiled artifact. Checks() is the
// catalogue; RunAll executes it; Report renders the published markdown report
// (docs/development/broker-compatibility.md), kept in sync by a golden test.
package compat

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // G505: SHA-1 is mandated by the .NET RSA.Decrypt OAEP default the broker uses; mirroring it is the compatibility test.
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/actions-gateway/github-actions-gateway/broker/brokertest"
)

// runnerVersion is the version string the suite registers sessions with. Any
// value above GitHub's enforced minimum works against the in-process model.
const runnerVersion = "2.335.1"

// Check is one broker-compatibility assertion. ID is a stable identifier used
// in the report and as the subtest name; Contract names the design-doc section
// the check pins; Asserts is a one-line human summary of what passing proves.
type Check struct {
	ID       string
	Title    string
	Contract string
	Asserts  string
	Run      func(ctx context.Context) error
}

// Result pairs a Check with the outcome of running it.
type Result struct {
	Check Check
	Err   error
}

// Pass reports whether the check succeeded.
func (r Result) Pass() bool { return r.Err == nil }

// Checks returns the ordered broker-compatibility catalogue. The order is
// stable so the generated report is deterministic.
func Checks() []Check {
	return []Check{
		{
			ID:       "C01",
			Title:    "Create session",
			Contract: "§3.3 POST /session",
			Asserts:  "CreateSession registers a virtual runner and returns a non-empty sessionId.",
			Run:      checkCreateSession,
		},
		{
			ID:       "C02",
			Title:    "Runner version too old",
			Contract: "§3.3 POST /session (400)",
			Asserts:  "A 400 version-too-old response surfaces as *broker.VersionTooOldError (non-retriable).",
			Run:      checkVersionTooOld,
		},
		{
			ID:       "C03",
			Title:    "Session unauthorized",
			Contract: "§3.3 POST /session (401/403)",
			Asserts:  "401 and 403 on CreateSession surface as *broker.UnauthorizedError (refresh-token signal).",
			Run:      checkSessionUnauthorized,
		},
		{
			ID:       "C04",
			Title:    "One session per agent",
			Contract: "§3.3 POST /session (409)",
			Asserts:  "A 409 Conflict (agent already has a session) surfaces as an error the listener can act on.",
			Run:      checkSessionConflict,
		},
		{
			ID:       "C05",
			Title:    "Long-poll empty",
			Contract: "§3.3 GET /message (202)",
			Asserts:  "GetMessage returns (nil, nil) on a 202 no-job long-poll response.",
			Run:      checkMessageEmpty,
		},
		{
			ID:       "C06",
			Title:    "Job delivery",
			Contract: "§3.3 GET /message; §3.4 RunnerJobRequest",
			Asserts:  "A queued job arrives as a RunnerJobRequest carrying run_service_url and runner_request_id.",
			Run:      checkJobDelivery,
		},
		{
			ID:       "C07",
			Title:    "Session expired",
			Contract: "§3.3 GET /message (404/410)",
			Asserts:  "404 and 410 on GetMessage surface as *broker.SessionExpiredError (recreate-session signal).",
			Run:      checkSessionExpired,
		},
		{
			ID:       "C08",
			Title:    "Rate limited",
			Contract: "§3.5 GET /message (429)",
			Asserts:  "429 surfaces as *broker.RateLimitError, honoring Retry-After when present and signalling backoff when absent.",
			Run:      checkRateLimited,
		},
		{
			ID:       "C09",
			Title:    "Two-URL model",
			Contract: "§3.3 run_service_url vs broker_url",
			Asserts:  "acquirejob/renewjob target the per-job run_service_url; reusing the static broker_url 404s (the cached-URL pitfall).",
			Run:      checkTwoURLModel,
		},
		{
			ID:       "C10",
			Title:    "Plan-ID header precedence",
			Contract: "§3.3/§3.4 AcquireJob x-plan-id",
			Asserts:  "The x-plan-id response header takes precedence over .plan.planId; the body value is used when the header is absent.",
			Run:      checkPlanIDPrecedence,
		},
		{
			ID:       "C11",
			Title:    "Renew job lock",
			Contract: "§3.3/§3.4 POST /renewjob",
			Asserts:  "RenewJob extends the lock and returns a future lockedUntil.",
			Run:      checkRenewJob,
		},
		{
			ID:       "C12",
			Title:    "Session reuse after acquire",
			Contract: "§3.3 session reuse (M1 Investigation C)",
			Asserts:  "GetMessage on the same sessionId immediately after AcquireJob stays valid (202, no error) — no delete→create cycle.",
			Run:      checkSessionReuse,
		},
		{
			ID:       "C13",
			Title:    "Acknowledge not required",
			Contract: "§3.3 acknowledge (M1 Investigation A)",
			Asserts:  "The full create→poll→acquire→renew→delete lifecycle completes with zero acknowledge calls.",
			Run:      checkAcknowledgeOptional,
		},
		{
			ID:       "C14",
			Title:    "Session-key RSA-OAEP unwrap",
			Contract: "§3.2 encryptionKey (RSA-OAEP SHA-1)",
			Asserts:  "A session key wrapped with RSA-OAEP(SHA-1) — the .NET runner default — round-trips through DecryptSessionKey.",
			Run:      checkSessionKeyCrypto,
		},
		{
			ID:       "C15",
			Title:    "Message-body AES-256-CBC unwrap",
			Contract: "§3.4 encrypted message body",
			Asserts:  "A body encrypted as base64(IV‖AES-256-CBC(PKCS#7)) round-trips through DecryptMessageBody into a RunnerJobRequestBody.",
			Run:      checkMessageBodyCrypto,
		},
	}
}

// RunAll executes every check in order and returns the results.
func RunAll(ctx context.Context) []Result {
	checks := Checks()
	results := make([]Result, 0, len(checks))
	for _, c := range checks {
		results = append(results, Result{Check: c, Err: c.Run(ctx)})
	}
	return results
}

// ── Checks ───────────────────────────────────────────────────────────────────

func checkCreateSession(ctx context.Context) error {
	srv := brokertest.New()
	defer srv.Close()
	c := stubClient(srv)
	res, err := c.CreateSession(ctx, 1, "agent-1", runnerVersion)
	if err != nil {
		return fmt.Errorf("CreateSession: %w", err)
	}
	if res.SessionID == "" {
		return errors.New("CreateSession returned an empty sessionId")
	}
	return nil
}

func checkVersionTooOld(ctx context.Context) error {
	srv := statusServer(http.StatusBadRequest, `{"message":"runner version 2.0.0 is below the minimum supported version"}`, nil)
	defer srv.Close()
	c := errorClient(srv)
	_, err := c.CreateSession(ctx, 1, "agent-1", "2.0.0")
	var tooOld *broker.VersionTooOldError
	if !errors.As(err, &tooOld) {
		return fmt.Errorf("want *broker.VersionTooOldError, got %v", err)
	}
	return nil
}

func checkSessionUnauthorized(ctx context.Context) error {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := statusServer(status, `{"message":"unauthorized"}`, nil)
		c := errorClient(srv)
		_, err := c.CreateSession(ctx, 1, "agent-1", runnerVersion)
		srv.Close()
		var unauth *broker.UnauthorizedError
		if !errors.As(err, &unauth) {
			return fmt.Errorf("status %d: want *broker.UnauthorizedError, got %v", status, err)
		}
		if unauth.StatusCode != status {
			return fmt.Errorf("status %d: UnauthorizedError carried %d", status, unauth.StatusCode)
		}
	}
	return nil
}

func checkSessionConflict(ctx context.Context) error {
	srv := statusServer(http.StatusConflict, `{"message":"agent already has an active session"}`, nil)
	defer srv.Close()
	c := errorClient(srv)
	_, err := c.CreateSession(ctx, 1, "agent-1", runnerVersion)
	if err == nil {
		return errors.New("CreateSession on 409 returned nil error")
	}
	// 409 is a server-side uniqueness constraint, not one of the client's typed
	// signals; the contract is only that the listener sees a non-nil error that
	// names the status so it can assign a distinct agent.
	if !strings.Contains(err.Error(), "409") {
		return fmt.Errorf("409 error did not name the status: %v", err)
	}
	return nil
}

func checkMessageEmpty(ctx context.Context) error {
	srv := brokertest.New()
	defer srv.Close()
	c := stubClient(srv)
	res, err := c.CreateSession(ctx, 1, "agent-1", runnerVersion)
	if err != nil {
		return fmt.Errorf("CreateSession: %w", err)
	}
	msg, err := c.GetMessage(ctx, res.SessionID)
	if err != nil {
		return fmt.Errorf("GetMessage: %w", err)
	}
	if msg != nil {
		return fmt.Errorf("want nil message on 202, got %+v", msg)
	}
	return nil
}

func checkJobDelivery(ctx context.Context) error {
	srv := brokertest.New()
	defer srv.Close()
	c := stubClient(srv)
	res, err := c.CreateSession(ctx, 1, "agent-1", runnerVersion)
	if err != nil {
		return fmt.Errorf("CreateSession: %w", err)
	}
	srv.EnqueueJob(res.SessionID, broker.RunnerJobRequestBody{
		RunnerRequestID: "req-123",
		BillingOwnerID:  "owner-1",
	})
	msg, err := c.GetMessage(ctx, res.SessionID)
	if err != nil {
		return fmt.Errorf("GetMessage: %w", err)
	}
	if msg == nil {
		return errors.New("GetMessage returned no message for a queued job")
	}
	if msg.MessageType != "RunnerJobRequest" {
		return fmt.Errorf("messageType = %q, want RunnerJobRequest", msg.MessageType)
	}
	var body broker.RunnerJobRequestBody
	if err := json.Unmarshal([]byte(msg.Body), &body); err != nil {
		return fmt.Errorf("decode message body: %w", err)
	}
	if body.RunnerRequestID != "req-123" {
		return fmt.Errorf("runner_request_id = %q, want req-123", body.RunnerRequestID)
	}
	if body.RunServiceURL == "" {
		return errors.New("message body carried no run_service_url")
	}
	return nil
}

func checkSessionExpired(ctx context.Context) error {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		srv := statusServer(status, `{"message":"session not found"}`, nil)
		c := errorClient(srv)
		_, err := c.GetMessage(ctx, "session-x")
		srv.Close()
		var expired *broker.SessionExpiredError
		if !errors.As(err, &expired) {
			return fmt.Errorf("status %d: want *broker.SessionExpiredError, got %v", status, err)
		}
		if expired.StatusCode != status {
			return fmt.Errorf("status %d: SessionExpiredError carried %d", status, expired.StatusCode)
		}
	}
	return nil
}

func checkRateLimited(ctx context.Context) error {
	// With a Retry-After header the client must honor it.
	withHeader := statusServer(http.StatusTooManyRequests, "", map[string]string{"Retry-After": "30"})
	c := errorClient(withHeader)
	_, err := c.GetMessage(ctx, "session-x")
	withHeader.Close()
	var rl *broker.RateLimitError
	if !errors.As(err, &rl) {
		return fmt.Errorf("want *broker.RateLimitError, got %v", err)
	}
	if rl.RetryAfter != 30*time.Second {
		return fmt.Errorf("RetryAfter = %s, want 30s", rl.RetryAfter)
	}
	// Without the header the client must signal "apply backoff" via RetryAfter < 0.
	noHeader := statusServer(http.StatusTooManyRequests, "", nil)
	defer noHeader.Close()
	c = errorClient(noHeader)
	_, err = c.GetMessage(ctx, "session-x")
	if !errors.As(err, &rl) {
		return fmt.Errorf("no-header: want *broker.RateLimitError, got %v", err)
	}
	if rl.RetryAfter >= 0 {
		return fmt.Errorf("no-header: RetryAfter = %s, want negative (backoff)", rl.RetryAfter)
	}
	return nil
}

func checkTwoURLModel(ctx context.Context) error {
	// The run service is a distinct host from the broker. acquirejob must go to
	// it; pointing at the broker host (the classic "cached run_service_url"
	// mistake) must fail.
	var runAcquires atomic.Int64
	runSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/acquirejob" {
			http.NotFound(w, r)
			return
		}
		runAcquires.Add(1)
		_ = json.NewEncoder(w).Encode(broker.AcquireJobResponse{})
	}))
	defer runSrv.Close()
	runBase := strings.TrimRight(runSrv.URL, "/")

	// The broker host serves only sessions/messages — it has no acquirejob route.
	brokerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer brokerSrv.Close()
	brokerBase := strings.TrimRight(brokerSrv.URL, "/")

	c := &broker.Client{Token: "tok", HTTPClient: runSrv.Client()}
	if _, _, err := c.AcquireJob(ctx, runBase, broker.JobAcquisitionRequest{JobMessageID: "req-1", RunnerOS: "Linux"}); err != nil {
		return fmt.Errorf("AcquireJob on run_service_url: %w", err)
	}
	if got := runAcquires.Load(); got != 1 {
		return fmt.Errorf("run service saw %d acquirejob calls, want 1", got)
	}
	// Reusing the broker host for acquirejob — the cached-URL pitfall — must 404.
	if _, _, err := c.AcquireJob(ctx, brokerBase, broker.JobAcquisitionRequest{JobMessageID: "req-1", RunnerOS: "Linux"}); err == nil {
		return errors.New("AcquireJob against broker_url unexpectedly succeeded (two-URL model violated)")
	}
	return nil
}

func checkPlanIDPrecedence(ctx context.Context) error {
	// Header present: x-plan-id wins over the body value.
	hdrSrv := acquireJobServer("body-plan", "header-plan")
	defer hdrSrv.Close()
	c := &broker.Client{Token: "tok", HTTPClient: hdrSrv.Client()}
	resp, _, err := c.AcquireJob(ctx, strings.TrimRight(hdrSrv.URL, "/"), broker.JobAcquisitionRequest{JobMessageID: "req-1"})
	if err != nil {
		return fmt.Errorf("AcquireJob (header): %w", err)
	}
	if resp.Plan.PlanID != "header-plan" {
		return fmt.Errorf("planId = %q, want header-plan (header precedence)", resp.Plan.PlanID)
	}
	// Header absent: fall back to .plan.planId in the body.
	bodySrv := acquireJobServer("body-plan", "")
	defer bodySrv.Close()
	c = &broker.Client{Token: "tok", HTTPClient: bodySrv.Client()}
	resp, _, err = c.AcquireJob(ctx, strings.TrimRight(bodySrv.URL, "/"), broker.JobAcquisitionRequest{JobMessageID: "req-1"})
	if err != nil {
		return fmt.Errorf("AcquireJob (body): %w", err)
	}
	if resp.Plan.PlanID != "body-plan" {
		return fmt.Errorf("planId = %q, want body-plan (body fallback)", resp.Plan.PlanID)
	}
	return nil
}

func checkRenewJob(ctx context.Context) error {
	srv := brokertest.New()
	defer srv.Close()
	c := stubClient(srv)
	before := time.Now()
	resp, err := c.RenewJob(ctx, strings.TrimRight(srv.URL, "/"), broker.RenewJobRequest{PlanID: "plan-1", JobID: "req-1"})
	if err != nil {
		return fmt.Errorf("RenewJob: %w", err)
	}
	if !resp.LockedUntil.After(before) {
		return fmt.Errorf("lockedUntil %s is not after the renew time %s", resp.LockedUntil, before)
	}
	return nil
}

func checkSessionReuse(ctx context.Context) error {
	srv := brokertest.New()
	defer srv.Close()
	c := stubClient(srv)
	res, err := c.CreateSession(ctx, 1, "agent-1", runnerVersion)
	if err != nil {
		return fmt.Errorf("CreateSession: %w", err)
	}
	srv.EnqueueJob(res.SessionID, broker.RunnerJobRequestBody{RunnerRequestID: "req-1"})
	msg, err := c.GetMessage(ctx, res.SessionID)
	if err != nil || msg == nil {
		return fmt.Errorf("GetMessage (first job): msg=%v err=%w", msg, err)
	}
	var body broker.RunnerJobRequestBody
	if err := json.Unmarshal([]byte(msg.Body), &body); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	if _, _, err := c.AcquireJob(ctx, body.RunServiceURL, broker.JobAcquisitionRequest{JobMessageID: body.RunnerRequestID, RunnerOS: "Linux"}); err != nil {
		return fmt.Errorf("AcquireJob: %w", err)
	}
	// Re-poll the same session: it must remain valid (202, no error).
	reuse, err := c.GetMessage(ctx, res.SessionID)
	if err != nil {
		return fmt.Errorf("GetMessage after AcquireJob returned an error (session invalidated): %w", err)
	}
	if reuse != nil {
		return fmt.Errorf("expected 202 no-job on reuse poll, got %+v", reuse)
	}
	return nil
}

func checkAcknowledgeOptional(ctx context.Context) error {
	srv := brokertest.New()
	defer srv.Close()
	c := stubClient(srv)
	res, err := c.CreateSession(ctx, 1, "agent-1", runnerVersion)
	if err != nil {
		return fmt.Errorf("CreateSession: %w", err)
	}
	srv.EnqueueJob(res.SessionID, broker.RunnerJobRequestBody{RunnerRequestID: "req-1"})
	msg, err := c.GetMessage(ctx, res.SessionID)
	if err != nil || msg == nil {
		return fmt.Errorf("GetMessage: msg=%v err=%w", msg, err)
	}
	var body broker.RunnerJobRequestBody
	if err := json.Unmarshal([]byte(msg.Body), &body); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	// Claim, renew, and tear down WITHOUT any acknowledge call. If acknowledge
	// were a delivery gate this lifecycle would not complete cleanly.
	if _, _, err := c.AcquireJob(ctx, body.RunServiceURL, broker.JobAcquisitionRequest{JobMessageID: body.RunnerRequestID, RunnerOS: "Linux"}); err != nil {
		return fmt.Errorf("AcquireJob: %w", err)
	}
	if _, err := c.RenewJob(ctx, body.RunServiceURL, broker.RenewJobRequest{PlanID: "plan-1", JobID: body.RunnerRequestID}); err != nil {
		return fmt.Errorf("RenewJob: %w", err)
	}
	if err := c.DeleteSession(ctx, res.SessionID); err != nil {
		return fmt.Errorf("DeleteSession: %w", err)
	}
	for _, id := range srv.RegisteredSessions() {
		if id == res.SessionID {
			return errors.New("session still registered after DeleteSession")
		}
	}
	if got := srv.AcquireJobCalls(); got != 1 {
		return fmt.Errorf("acquirejob calls = %d, want exactly 1 (the atomic claim)", got)
	}
	return nil
}

func checkSessionKeyCrypto(_ context.Context) error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generate RSA key: %w", err)
	}
	aesKey := make([]byte, 32)
	if _, err := rand.Read(aesKey); err != nil {
		return fmt.Errorf("random AES key: %w", err)
	}
	//nolint:gosec // G505: the broker wraps session keys with RSA-OAEP(SHA-1); this asserts that exact scheme decrypts.
	wrapped, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, &key.PublicKey, aesKey, nil)
	if err != nil {
		return fmt.Errorf("wrap session key: %w", err)
	}
	got, err := broker.DecryptSessionKey(wrapped, key)
	if err != nil {
		return fmt.Errorf("DecryptSessionKey: %w", err)
	}
	if !bytes.Equal(got, aesKey) {
		return errors.New("unwrapped session key did not match the original")
	}
	return nil
}

func checkMessageBodyCrypto(_ context.Context) error {
	aesKey := make([]byte, 32)
	if _, err := rand.Read(aesKey); err != nil {
		return fmt.Errorf("random AES key: %w", err)
	}
	want := broker.RunnerJobRequestBody{
		RunnerRequestID: "req-1",
		RunServiceURL:   "https://run.example/abc",
		BillingOwnerID:  "owner-1",
	}
	plaintext, err := json.Marshal(want)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}
	wire, err := encryptCBC(aesKey, plaintext)
	if err != nil {
		return fmt.Errorf("encrypt body: %w", err)
	}
	decrypted, err := broker.DecryptMessageBody(wire, aesKey)
	if err != nil {
		return fmt.Errorf("DecryptMessageBody: %w", err)
	}
	var got broker.RunnerJobRequestBody
	if err := json.Unmarshal(decrypted, &got); err != nil {
		return fmt.Errorf("unmarshal decrypted body: %w", err)
	}
	if got != want {
		return fmt.Errorf("round-tripped body = %+v, want %+v", got, want)
	}
	return nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// stubClient builds a v2-flow broker.Client wired to the brokertest stub.
func stubClient(srv *brokertest.Server) *broker.Client {
	return &broker.Client{
		BrokerURL:     srv.URL,
		UseV2Flow:     true,
		RunnerVersion: runnerVersion,
		Token:         "tok",
		HTTPClient:    srv.HTTPClient(),
	}
}

// errorClient builds a v2-flow broker.Client pointed at a single-status httptest
// stub, using that server's own bounded client.
func errorClient(srv *httptest.Server) *broker.Client {
	return &broker.Client{
		BrokerURL:     srv.URL,
		UseV2Flow:     true,
		RunnerVersion: runnerVersion,
		Token:         "tok",
		HTTPClient:    srv.Client(),
	}
}

// statusServer returns an httptest server that answers every request with the
// given status, body, and headers. Used to exercise the broker client's
// error-status contracts without standing up the full stub.
func statusServer(status int, body string, headers map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
}

// acquireJobServer returns an httptest server whose /acquirejob route returns
// the given body planId and, when headerPlanID is non-empty, the x-plan-id
// response header.
func acquireJobServer(bodyPlanID, headerPlanID string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/acquirejob" {
			http.NotFound(w, r)
			return
		}
		if headerPlanID != "" {
			w.Header().Set("x-plan-id", headerPlanID)
		}
		resp := broker.AcquireJobResponse{}
		resp.Plan.PlanID = bodyPlanID
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// encryptCBC mirrors how the broker wraps a message body: base64(IV ‖
// AES-256-CBC(PKCS#7-padded plaintext)). It is the producer side of
// broker.DecryptMessageBody, so a successful round-trip proves wire-format
// compatibility.
func encryptCBC(key, plaintext []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	padded := pkcs7Pad(plaintext, aes.BlockSize)
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	ciphertext := make([]byte, len(padded))
	//nolint:gosec // G407: the IV is freshly random per call (rand.Read above), not a hardcoded value.
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)
	return base64.StdEncoding.EncodeToString(append(iv, ciphertext...)), nil
}

// pkcs7Pad appends PKCS#7 padding to a multiple of blockSize.
func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	return append(data, bytes.Repeat([]byte{byte(pad)}, pad)...)
}

// ── Report ───────────────────────────────────────────────────────────────────

// Report renders the published broker-compatibility report from a set of
// results. The output is deterministic (no timestamps) so it can be committed
// and kept in sync by a golden test; freshness is established by CI, which runs
// the suite on every change.
func Report(results []Result) string {
	pass := 0
	for _, r := range results {
		if r.Pass() {
			pass++
		}
	}

	var b strings.Builder
	b.WriteString("<!-- GENERATED by cmd/probe/compat; regenerate with `make compat-report`. Do not edit by hand. -->\n")
	b.WriteString("# Broker compatibility report\n\n")
	b.WriteString("This report is the published result of the broker-compatibility suite (Q191).\n")
	b.WriteString("The suite exercises the GitHub Actions broker wire protocol and data contracts\n")
	b.WriteString("end to end and asserts conformance to every documented contract in\n")
	b.WriteString("[docs/design/03-api-contracts.md](../design/03-api-contracts.md). It exists to\n")
	b.WriteString("confirm full broker compatibility before the v2beta1 API shape is frozen, so a\n")
	b.WriteString("silent wire-protocol break shows up as a red test rather than a field that has\n")
	b.WriteString("to be added after the beta cut.\n\n")

	b.WriteString("## Result\n\n")
	fmt.Fprintf(&b, "**%d/%d contracts verified.**\n\n", pass, len(results))
	if pass == len(results) {
		b.WriteString("All documented broker contracts pass against the in-process broker model.\n\n")
	} else {
		b.WriteString("⚠️ One or more contracts are failing — see the table below.\n\n")
	}

	b.WriteString("## How it runs\n\n")
	b.WriteString("The suite is credential-free: it drives the `broker.Client` against the shared\n")
	b.WriteString("broker stub (`broker/brokertest`) and small purpose-built `httptest` stubs for\n")
	b.WriteString("the error-status contracts. It needs no secrets and no network, so it runs in\n")
	b.WriteString("`make check` and CI on every change:\n\n")
	b.WriteString("```sh\n")
	b.WriteString("(cd cmd/probe && go test ./compat/...)   # or: make check\n")
	b.WriteString("```\n\n")
	b.WriteString("Regenerate this report after adding or changing a check:\n\n")
	b.WriteString("```sh\n")
	b.WriteString("make compat-report\n")
	b.WriteString("```\n\n")

	b.WriteString("## Coverage\n\n")
	b.WriteString("| ID | Contract | Compatibility check | Result |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	// Render in catalogue order.
	sorted := append([]Result(nil), results...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Check.ID < sorted[j].Check.ID })
	for _, r := range sorted {
		status := "✅ PASS"
		if !r.Pass() {
			status = "❌ FAIL"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
			r.Check.ID, escapePipes(r.Check.Contract), escapePipes(r.Check.Asserts), status)
	}
	b.WriteString("\n")

	b.WriteString("## Scope and the live probe\n\n")
	b.WriteString("This suite verifies conformance to the **documented** broker contract against an\n")
	b.WriteString("in-process model of the broker. It does not, by itself, talk to GitHub. The\n")
	b.WriteString("behaviours the model encodes were confirmed against real GitHub during\n")
	b.WriteString("Milestone 1 — see the live-probe findings in\n")
	b.WriteString("the Milestone 1 plan §8 (acknowledge not required,\n")
	b.WriteString("session reuse after acquire, egress-IP variance). The companion\n")
	b.WriteString("credential-gated probe binary (`cmd/probe`) is the live check that the model\n")
	b.WriteString("still matches production; this suite is the repeatable, always-green guard that\n")
	b.WriteString("the client honours that contract.\n\n")
	b.WriteString("Not covered here (requires live GitHub credentials, run via the probe binary):\n")
	b.WriteString("the GitHub App JWT→installation-token exchange, the runner OAuth token\n")
	b.WriteString("assertion, and GitHub's server-side minimum-runner-version threshold. These are\n")
	b.WriteString("exercised by the credential-gated probe, not by this in-process suite.\n")
	return b.String()
}

// escapePipes escapes the markdown table delimiter so a contract string
// containing "|" cannot break the table.
func escapePipes(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}
