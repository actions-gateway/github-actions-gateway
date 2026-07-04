package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/actions-gateway/github-actions-gateway/broker/brokertest"
)

// testRSAPEM generates a small RSA key and returns it PEM-encoded (PKCS1),
// matching the format GitHub App private keys are distributed in.
func testRSAPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}
	return string(pem.EncodeToMemory(block))
}

// getenvFromMap returns a getenv-shaped func backed by a plain map, matching
// the signature parseProbeConfig expects (normally os.Getenv).
func getenvFromMap(m map[string]string) func(string) string {
	return func(k string) string {
		return m[k]
	}
}

func TestParseProbeConfig_ValidConfig(t *testing.T) {
	t.Parallel()
	pemStr := testRSAPEM(t)

	env := map[string]string{
		"GITHUB_APP_ID":              "123",
		"GITHUB_APP_INSTALLATION_ID": "456",
		"GITHUB_APP_PRIVATE_KEY":     pemStr,
		"GITHUB_BROKER_URL":          "https://pipelines.example.com/base",
		"GITHUB_RUNNER_VERSION":      "2.327.1",
		"GITHUB_AGENT_NAME":          "my-agent",
		"GITHUB_AGENT_ID":            "789",
		"GITHUB_RUNNER_OS":           "linux",
		"GITHUB_RUNNER_ARCH":         "x64",
	}

	cfg, err := parseProbeConfig(getenvFromMap(env))
	if err != nil {
		t.Fatalf("parseProbeConfig: unexpected error: %v", err)
	}

	if cfg.AppID != 123 {
		t.Errorf("AppID = %d, want 123", cfg.AppID)
	}
	if cfg.InstallationID != 456 {
		t.Errorf("InstallationID = %d, want 456", cfg.InstallationID)
	}
	if string(cfg.PrivateKeyPEM) != pemStr {
		t.Errorf("PrivateKeyPEM = %q, want %q", cfg.PrivateKeyPEM, pemStr)
	}
	// No GITHUB_BROKER_URL_V2 set, so BrokerURL should remain GITHUB_BROKER_URL.
	if cfg.BrokerURL != "https://pipelines.example.com/base" {
		t.Errorf("BrokerURL = %q, want unchanged base URL", cfg.BrokerURL)
	}
	if cfg.RunnerVersion != "2.327.1" {
		t.Errorf("RunnerVersion = %q, want 2.327.1", cfg.RunnerVersion)
	}
	if cfg.AgentName != "my-agent" {
		t.Errorf("AgentName = %q, want my-agent", cfg.AgentName)
	}
	if cfg.AgentID != 789 {
		t.Errorf("AgentID = %d, want 789", cfg.AgentID)
	}
	if cfg.RunnerOS != "linux" {
		t.Errorf("RunnerOS = %q, want linux", cfg.RunnerOS)
	}
	if cfg.RunnerArch != "x64" {
		t.Errorf("RunnerArch = %q, want x64", cfg.RunnerArch)
	}
	if !cfg.UseV2Flow {
		t.Errorf("UseV2Flow = false, want true (GITHUB_USE_VSTS_FLOW unset)")
	}
	if cfg.PoolID != 1 {
		t.Errorf("PoolID = %d, want default 1", cfg.PoolID)
	}
}

func TestParseProbeConfig_BrokerURLV2Override(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"GITHUB_APP_ID":              "1",
		"GITHUB_APP_INSTALLATION_ID": "2",
		"GITHUB_APP_PRIVATE_KEY":     testRSAPEM(t),
		"GITHUB_BROKER_URL":          "https://v1.example.com/",
		"GITHUB_RUNNER_VERSION":      "2.327.1",
		"GITHUB_AGENT_NAME":          "agent",
		"GITHUB_AGENT_ID":            "3",
		"GITHUB_BROKER_URL_V2":       "https://v2.example.com/",
	}

	cfg, err := parseProbeConfig(getenvFromMap(env))
	if err != nil {
		t.Fatalf("parseProbeConfig: unexpected error: %v", err)
	}
	if !cfg.UseV2Flow {
		t.Fatalf("UseV2Flow = false, want true")
	}
	if cfg.BrokerURL != "https://v2.example.com/" {
		t.Errorf("BrokerURL = %q, want v2 override applied", cfg.BrokerURL)
	}
}

func TestParseProbeConfig_BrokerURLV2IgnoredWhenVSTSFlow(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"GITHUB_APP_ID":              "1",
		"GITHUB_APP_INSTALLATION_ID": "2",
		"GITHUB_APP_PRIVATE_KEY":     testRSAPEM(t),
		"GITHUB_BROKER_URL":          "https://v1.example.com/",
		"GITHUB_RUNNER_VERSION":      "2.327.1",
		"GITHUB_AGENT_NAME":          "agent",
		"GITHUB_AGENT_ID":            "3",
		"GITHUB_BROKER_URL_V2":       "https://v2.example.com/",
		"GITHUB_USE_VSTS_FLOW":       "true",
	}

	cfg, err := parseProbeConfig(getenvFromMap(env))
	if err != nil {
		t.Fatalf("parseProbeConfig: unexpected error: %v", err)
	}
	if cfg.UseV2Flow {
		t.Fatalf("UseV2Flow = true, want false when GITHUB_USE_VSTS_FLOW=true")
	}
	// The v2 override only applies when UseV2Flow is true; legacy flow keeps
	// the v1 broker URL even if GITHUB_BROKER_URL_V2 happens to be set.
	if cfg.BrokerURL != "https://v1.example.com/" {
		t.Errorf("BrokerURL = %q, want v1 URL preserved under VSTS flow", cfg.BrokerURL)
	}
}

func TestParseProbeConfig_PoolIDDefault(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"GITHUB_APP_ID":              "1",
		"GITHUB_APP_INSTALLATION_ID": "2",
		"GITHUB_APP_PRIVATE_KEY":     testRSAPEM(t),
		"GITHUB_BROKER_URL":          "https://example.com/",
		"GITHUB_RUNNER_VERSION":      "2.327.1",
		"GITHUB_AGENT_NAME":          "agent",
		"GITHUB_AGENT_ID":            "3",
	}
	cfg, err := parseProbeConfig(getenvFromMap(env))
	if err != nil {
		t.Fatalf("parseProbeConfig: unexpected error: %v", err)
	}
	if cfg.PoolID != 1 {
		t.Errorf("PoolID = %d, want default 1 when GITHUB_POOL_ID unset", cfg.PoolID)
	}
}

func TestParseProbeConfig_PoolIDExplicit(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"GITHUB_APP_ID":              "1",
		"GITHUB_APP_INSTALLATION_ID": "2",
		"GITHUB_APP_PRIVATE_KEY":     testRSAPEM(t),
		"GITHUB_BROKER_URL":          "https://example.com/",
		"GITHUB_RUNNER_VERSION":      "2.327.1",
		"GITHUB_AGENT_NAME":          "agent",
		"GITHUB_AGENT_ID":            "3",
		"GITHUB_POOL_ID":             "42",
	}
	cfg, err := parseProbeConfig(getenvFromMap(env))
	if err != nil {
		t.Fatalf("parseProbeConfig: unexpected error: %v", err)
	}
	if cfg.PoolID != 42 {
		t.Errorf("PoolID = %d, want 42", cfg.PoolID)
	}
}

func TestParseProbeConfig_Errors(t *testing.T) {
	t.Parallel()

	validPEM := testRSAPEM(t)
	baseEnv := func() map[string]string {
		return map[string]string{
			"GITHUB_APP_ID":              "1",
			"GITHUB_APP_INSTALLATION_ID": "2",
			"GITHUB_APP_PRIVATE_KEY":     validPEM,
			"GITHUB_BROKER_URL":          "https://example.com/",
			"GITHUB_RUNNER_VERSION":      "2.327.1",
			"GITHUB_AGENT_NAME":          "agent",
			"GITHUB_AGENT_ID":            "3",
		}
	}

	tests := []struct {
		name    string
		mutate  func(env map[string]string)
		wantErr string
	}{
		{
			name:    "missing GITHUB_APP_ID",
			mutate:  func(env map[string]string) { delete(env, "GITHUB_APP_ID") },
			wantErr: "GITHUB_APP_ID is not set",
		},
		{
			name:    "unparseable GITHUB_APP_ID",
			mutate:  func(env map[string]string) { env["GITHUB_APP_ID"] = "not-a-number" },
			wantErr: "parse GITHUB_APP_ID",
		},
		{
			name:    "missing GITHUB_APP_INSTALLATION_ID",
			mutate:  func(env map[string]string) { delete(env, "GITHUB_APP_INSTALLATION_ID") },
			wantErr: "GITHUB_APP_INSTALLATION_ID is not set",
		},
		{
			name:    "unparseable GITHUB_APP_INSTALLATION_ID",
			mutate:  func(env map[string]string) { env["GITHUB_APP_INSTALLATION_ID"] = "abc" },
			wantErr: "parse GITHUB_APP_INSTALLATION_ID",
		},
		{
			name:    "missing GITHUB_APP_PRIVATE_KEY",
			mutate:  func(env map[string]string) { delete(env, "GITHUB_APP_PRIVATE_KEY") },
			wantErr: "GITHUB_APP_PRIVATE_KEY is not set",
		},
		{
			name: "bad PEM path",
			mutate: func(env map[string]string) {
				env["GITHUB_APP_PRIVATE_KEY"] = filepath.Join(t.TempDir(), "does-not-exist.pem")
			},
			wantErr: "load GITHUB_APP_PRIVATE_KEY",
		},
		{
			name:    "missing GITHUB_BROKER_URL",
			mutate:  func(env map[string]string) { delete(env, "GITHUB_BROKER_URL") },
			wantErr: "GITHUB_BROKER_URL is not set",
		},
		{
			name:    "missing GITHUB_RUNNER_VERSION",
			mutate:  func(env map[string]string) { delete(env, "GITHUB_RUNNER_VERSION") },
			wantErr: "GITHUB_RUNNER_VERSION is not set",
		},
		{
			name:    "missing GITHUB_AGENT_NAME",
			mutate:  func(env map[string]string) { delete(env, "GITHUB_AGENT_NAME") },
			wantErr: "GITHUB_AGENT_NAME is not set",
		},
		{
			name:    "missing GITHUB_AGENT_ID",
			mutate:  func(env map[string]string) { delete(env, "GITHUB_AGENT_ID") },
			wantErr: "GITHUB_AGENT_ID is not set",
		},
		{
			name:    "unparseable GITHUB_AGENT_ID",
			mutate:  func(env map[string]string) { env["GITHUB_AGENT_ID"] = "xyz" },
			wantErr: "parse GITHUB_AGENT_ID",
		},
		{
			name: "unparseable GITHUB_POOL_ID",
			mutate: func(env map[string]string) {
				env["GITHUB_POOL_ID"] = "not-an-int"
			},
			wantErr: "parse GITHUB_POOL_ID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := baseEnv()
			tt.mutate(env)

			_, err := parseProbeConfig(getenvFromMap(env))
			if err == nil {
				t.Fatalf("parseProbeConfig: expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("parseProbeConfig: error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestBackoffDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		consecutiveErrors int
		min, max          time.Duration
	}{
		{"first error", 1, 15 * time.Second, 30 * time.Second},
		{"at low-tier boundary", 5, 15 * time.Second, 30 * time.Second},
		{"just past boundary", 6, 30 * time.Second, 60 * time.Second},
		{"many errors capped", 100, 30 * time.Second, 60 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for i := 0; i < 20; i++ {
				got := backoffDelay(tt.consecutiveErrors)
				if got < tt.min || got > tt.max {
					t.Fatalf("backoffDelay(%d) = %v, want in [%v, %v]", tt.consecutiveErrors, got, tt.min, tt.max)
				}
			}
		})
	}
}

func TestBackoffDelay_GrowsWithErrors(t *testing.T) {
	t.Parallel()
	// The low tier's range ([15s,30s]) never overlaps the high tier's floor
	// (30s), so any low-tier sample is strictly less than the high tier's
	// minimum — this asserts the delay grows once the error count crosses
	// the 5-error threshold, not just that both live in disjoint ranges.
	low := backoffDelay(1)
	high := backoffDelay(6)
	if low >= 30*time.Second {
		t.Fatalf("backoffDelay(1) = %v, want < 30s", low)
	}
	if high < 30*time.Second {
		t.Fatalf("backoffDelay(6) = %v, want >= 30s", high)
	}
	if !(low < high) {
		t.Errorf("backoffDelay(1) = %v should be less than backoffDelay(6) = %v", low, high)
	}
}

func TestJitter_WithinBounds(t *testing.T) {
	t.Parallel()
	lo, hi := 10*time.Second, 20*time.Second
	for i := 0; i < 50; i++ {
		got := jitter(lo, hi)
		if got < lo || got > hi {
			t.Fatalf("jitter(%v, %v) = %v, want in range", lo, hi, got)
		}
	}
}

func TestJitter_ZeroRange(t *testing.T) {
	t.Parallel()
	// lo == hi should always return exactly lo (a zero-width range).
	got := jitter(5*time.Second, 5*time.Second)
	if got != 5*time.Second {
		t.Errorf("jitter(5s, 5s) = %v, want 5s", got)
	}
}

func TestLoadPEM_InlineLiteral(t *testing.T) {
	t.Parallel()
	pemStr := testRSAPEM(t)
	got, err := loadPEM(pemStr)
	if err != nil {
		t.Fatalf("loadPEM: unexpected error: %v", err)
	}
	if string(got) != pemStr {
		t.Errorf("loadPEM inline = %q, want %q", got, pemStr)
	}
}

func TestLoadPEM_FromFile(t *testing.T) {
	t.Parallel()
	pemStr := testRSAPEM(t)
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, []byte(pemStr), 0o600); err != nil {
		t.Fatalf("write temp PEM file: %v", err)
	}

	got, err := loadPEM(path)
	if err != nil {
		t.Fatalf("loadPEM: unexpected error: %v", err)
	}
	if string(got) != pemStr {
		t.Errorf("loadPEM from file = %q, want %q", got, pemStr)
	}
}

func TestLoadPEM_MissingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "does-not-exist.pem")
	_, err := loadPEM(path)
	if err == nil {
		t.Fatal("loadPEM: expected error for missing file, got nil")
	}
}

func TestLoadPEM_EmptyValueTreatedAsPath(t *testing.T) {
	t.Parallel()
	// An empty value is shorter than the PEM header, so it falls through to
	// the file-path branch and os.ReadFile("") must fail.
	_, err := loadPEM("")
	if err == nil {
		t.Fatal("loadPEM(\"\"): expected error, got nil")
	}
}

// discardLogger returns a slog.Logger that writes nowhere, keeping test
// output clean while still exercising the real logging call sites.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestProbeAcknowledge_ReturnsCompactHTTPStatus(t *testing.T) {
	t.Parallel()
	s := brokertest.New()
	defer s.Close()

	bc := &broker.Client{
		BrokerURL:  s.URL,
		PoolID:     1,
		Token:      "test-token",
		HTTPClient: s.HTTPClient(),
	}

	// brokertest serves the VSTS delete-message ("acknowledge") route under
	// {poolBase}/messages/{id} with a 200 OK. What's under test is that
	// probeAcknowledge issues the DELETE with the right auth header and reports
	// the response status back as a compact "HTTP-<code>" string rather than
	// erroring out, since the caller only logs this as an informational finding
	// (Investigation A).
	status := probeAcknowledge(context.Background(), discardLogger(), bc, 42, "session-abc")
	if status != "HTTP-200" {
		t.Errorf("probeAcknowledge = %q, want HTTP-200 (stub acknowledges the delete)", status)
	}
	if got := s.AcknowledgeCalls(); got != 1 {
		t.Errorf("AcknowledgeCalls = %d, want 1", got)
	}
}

func TestProbeAcknowledge_RequestError(t *testing.T) {
	t.Parallel()
	bc := &broker.Client{
		// Port 0 is never a live listener, so client.Do fails to connect
		// before any response is received.
		BrokerURL: "http://127.0.0.1:0/",
		PoolID:    1,
		Token:     "test-token",
	}

	status := probeAcknowledge(context.Background(), discardLogger(), bc, 1, "session-x")
	if !strings.HasPrefix(status, "request-error:") {
		t.Errorf("probeAcknowledge = %q, want request-error prefix", status)
	}
}

// TestProbeAcknowledge_BuildRequestError drives the earliest error branch:
// http.NewRequestWithContext rejects a URL containing a raw control
// character, which a BrokerURL carrying one (e.g. from a misconfigured
// environment variable) would produce via PoolBase()'s string concatenation.
func TestProbeAcknowledge_BuildRequestError(t *testing.T) {
	t.Parallel()
	bc := &broker.Client{
		BrokerURL: "http://example.com/\x7f",
		PoolID:    1,
		Token:     "test-token",
	}

	status := probeAcknowledge(context.Background(), discardLogger(), bc, 1, "session-x")
	if !strings.HasPrefix(status, "build-request-error:") {
		t.Errorf("probeAcknowledge = %q, want build-request-error prefix", status)
	}
}

func TestInvestigateSessionReuse_SecondJobDelivered(t *testing.T) {
	t.Parallel()
	s := brokertest.New()
	defer s.Close()

	bc := &broker.Client{
		BrokerURL: s.URL,
		PoolID:    1,
		Token:     "test-token",
		// brokertest's /session route implements the v2 flow (POST
		// {serverUrl}session, no pool path); the v1 flow POSTs to
		// {poolBase}/sessions instead, which the stub does not serve.
		UseV2Flow:  true,
		HTTPClient: s.HTTPClient(),
	}

	sess, err := bc.CreateSession(context.Background(), 1, "agent-1", "2.327.1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	bc.BrokerURL = sess.BrokerURL

	// Enqueue a second job before the investigation polls, so it is found
	// promptly rather than waiting out the 3-minute investigation timeout.
	s.EnqueueJob(sess.SessionID, broker.RunnerJobRequestBody{RunServiceURL: s.URL})

	done := make(chan struct{})
	go func() {
		defer close(done)
		investigateSessionReuse(context.Background(), discardLogger(), bc, sess.SessionID)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("investigateSessionReuse did not return after the second job was enqueued")
	}
}

func TestInvestigateJobDelivery_SecondSessionReceivesJob(t *testing.T) {
	t.Parallel()
	s := brokertest.New()
	defer s.Close()

	bc := &broker.Client{
		BrokerURL:  s.URL,
		PoolID:     1,
		Token:      "test-token",
		UseV2Flow:  true,
		HTTPClient: s.HTTPClient(),
	}

	// investigateJobDelivery creates its own second session internally; since
	// brokertest hands out session IDs sequentially starting at "session-1",
	// the session it creates will be "session-1" (first session ever created
	// on this fresh stub). Enqueue the job for that ID ahead of time — the
	// call blocks inside CreateSession momentarily, but EnqueueJob only needs
	// the channel to exist by the time GetMessage polls, and EnqueueJob lazily
	// creates the channel if needed.
	go func() {
		// Give CreateSession a moment to register the session before the
		// first GetMessage poll consumes the queue; EnqueueJob is safe to
		// call before or after since it creates the channel on demand.
		time.Sleep(50 * time.Millisecond)
		s.EnqueueJob("session-1", broker.RunnerJobRequestBody{RunServiceURL: s.URL})
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		investigateJobDelivery(context.Background(), discardLogger(), bc, 1, "agent-1", "2.327.1")
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("investigateJobDelivery did not return after the second session's job was enqueued")
	}

	if got := s.AcquireJobCalls(); got != 0 {
		t.Errorf("AcquireJobCalls = %d, want 0 (investigateJobDelivery only polls, never acquires)", got)
	}
}

// TestInvestigateSessionReuse_TimeoutNoJobArrives drives the inconclusive
// timeout branch: the investigation's internal 3-minute deadline is derived
// from the caller's context via context.WithTimeout(ctx, 3*time.Minute), so
// handing in an already-cancelled parent context makes deadline.Err() non-nil
// on the very first loop check — the same code path a real 3-minute timeout
// with no second job would take, without the test actually waiting 3 minutes.
func TestInvestigateSessionReuse_TimeoutNoJobArrives(t *testing.T) {
	t.Parallel()
	s := brokertest.New()
	defer s.Close()

	bc := &broker.Client{
		BrokerURL:  s.URL,
		PoolID:     1,
		Token:      "test-token",
		UseV2Flow:  true,
		HTTPClient: s.HTTPClient(),
	}

	sess, err := bc.CreateSession(context.Background(), 1, "agent-1", "2.327.1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	bc.BrokerURL = sess.BrokerURL

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled: the derived 3-minute deadline is already "expired"

	done := make(chan struct{})
	go func() {
		defer close(done)
		investigateSessionReuse(ctx, discardLogger(), bc, sess.SessionID)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("investigateSessionReuse did not return promptly on a pre-cancelled context")
	}
}

// TestInvestigateSessionReuse_ProtocolErrorInvalidatesSession drives the
// protocol-error branch: GetMessage fails with a non-context error (404
// session-not-found) while the deadline itself has not expired, the signal
// that the broker invalidated the session after AcquireJob.
func TestInvestigateSessionReuse_ProtocolErrorInvalidatesSession(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"session not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	bc := &broker.Client{
		BrokerURL:  srv.URL,
		PoolID:     1,
		Token:      "test-token",
		UseV2Flow:  true,
		HTTPClient: srv.Client(),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		investigateSessionReuse(context.Background(), discardLogger(), bc, "session-expired")
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("investigateSessionReuse did not return after a protocol-level GetMessage error")
	}
}

// TestInvestigateJobDelivery_CreateSessionFails drives the early-return branch
// taken when registering the second session itself fails, before any polling
// starts.
func TestInvestigateJobDelivery_CreateSessionFails(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	bc := &broker.Client{
		BrokerURL:  srv.URL,
		PoolID:     1,
		Token:      "test-token",
		UseV2Flow:  true,
		HTTPClient: srv.Client(),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		investigateJobDelivery(context.Background(), discardLogger(), bc, 1, "agent-1", "2.327.1")
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("investigateJobDelivery did not return after CreateSession failed")
	}
}

// TestInvestigateJobDelivery_Timeout drives the inconclusive timeout branch —
// the second session is created successfully but no job ever arrives before
// the deadline. As with the session-reuse timeout test, a pre-cancelled
// parent context makes the derived 3-minute deadline expire immediately.
func TestInvestigateJobDelivery_Timeout(t *testing.T) {
	t.Parallel()
	s := brokertest.New()
	defer s.Close()

	bc := &broker.Client{
		BrokerURL:  s.URL,
		PoolID:     1,
		Token:      "test-token",
		UseV2Flow:  true,
		HTTPClient: s.HTTPClient(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		investigateJobDelivery(ctx, discardLogger(), bc, 1, "agent-1", "2.327.1")
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("investigateJobDelivery did not return promptly on a pre-cancelled context")
	}

	if got := s.AcquireJobCalls(); got != 0 {
		t.Errorf("AcquireJobCalls = %d, want 0 (investigateJobDelivery only polls, never acquires)", got)
	}
}

// TestInvestigateSessionReuse_NoJobThenDeadlineExpires drives two branches
// in one run: the "202 no-job, keep polling" branch (got == nil) followed by
// the "deadline reached while GetMessage was in flight" branch, distinguished
// from a protocol error by deadline.Err() being non-nil after the call
// returns. A context whose own deadline is a few tens of milliseconds out
// means investigateSessionReuse's derived 3-minute timeout is dominated by
// the parent's short deadline, so the loop takes a few real 202 polls before
// GetMessage itself starts returning context.DeadlineExceeded.
func TestInvestigateSessionReuse_NoJobThenDeadlineExpires(t *testing.T) {
	t.Parallel()
	s := brokertest.New()
	defer s.Close()

	bc := &broker.Client{
		BrokerURL:  s.URL,
		PoolID:     1,
		Token:      "test-token",
		UseV2Flow:  true,
		HTTPClient: s.HTTPClient(),
	}

	sess, err := bc.CreateSession(context.Background(), 1, "agent-1", "2.327.1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	bc.BrokerURL = sess.BrokerURL
	// No job is ever enqueued for this session, so every poll returns 202
	// (got == nil) until the short parent deadline trips GetMessage's request.

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		investigateSessionReuse(ctx, discardLogger(), bc, sess.SessionID)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("investigateSessionReuse did not return after its parent deadline expired")
	}
}

// TestInvestigateSessionReuse_IgnoresNonJobMessage drives the "ignoring
// non-job message" branch: the second poll returns a message whose
// MessageType is not RunnerJobRequest, so the loop must log and continue
// rather than treat it as job delivery. The message is enqueued directly on
// the stub's job channel (bypassing EnqueueJob, which always stamps
// RunnerJobRequest) so the type is under the test's control.
func TestInvestigateSessionReuse_IgnoresNonJobMessage(t *testing.T) {
	t.Parallel()
	var pollCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sessionId":"session-x"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/message":
			n := pollCount.Add(1)
			if n == 1 {
				// First poll: a non-job message. The loop must not treat this
				// as delivery and must continue polling.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"messageId":1,"messageType":"JobCancelMessage","body":"{}"}`))
				return
			}
			// Second poll onward: RunnerJobRequest, so the loop can return.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"messageId":2,"messageType":"RunnerJobRequest","body":"{}"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	bc := &broker.Client{
		BrokerURL:  srv.URL,
		PoolID:     1,
		Token:      "test-token",
		UseV2Flow:  true,
		HTTPClient: srv.Client(),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		investigateSessionReuse(context.Background(), discardLogger(), bc, "session-x")
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("investigateSessionReuse did not return after the second (job) poll")
	}
	if got := pollCount.Load(); got < 2 {
		t.Fatalf("pollCount = %d, want >= 2 (must have polled past the non-job message)", got)
	}
}

// TestInvestigateJobDelivery_GetMessageError drives the GetMessage-error
// branch on the second session: CreateSession succeeds (registered against
// the real stub) but the subsequent poll hits a broker that 404s every
// GetMessage, the signal the session was rejected mid-poll.
func TestInvestigateJobDelivery_GetMessageError(t *testing.T) {
	t.Parallel()
	var created atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			created.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sessionId":"session-x"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/message":
			http.Error(w, `{"message":"session not found"}`, http.StatusNotFound)
		case r.Method == http.MethodDelete && r.URL.Path == "/session":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	bc := &broker.Client{
		BrokerURL:  srv.URL,
		PoolID:     1,
		Token:      "test-token",
		UseV2Flow:  true,
		HTTPClient: srv.Client(),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		investigateJobDelivery(context.Background(), discardLogger(), bc, 1, "agent-1", "2.327.1")
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("investigateJobDelivery did not return after a GetMessage protocol error")
	}

	if !created.Load() {
		t.Fatal("CreateSession was never called against the stub")
	}
}

// TestInvestigateJobDelivery_RealTimeoutNoJobArrives exercises a genuine
// (short) parent-context timeout after CreateSession has already succeeded —
// unlike TestInvestigateJobDelivery_Timeout, whose pre-cancelled parent
// context makes CreateSession itself fail before the poll loop is ever
// entered. The stub answers every /message poll with an immediate 202 (no
// job), so the loop polls rapidly until the short real deadline expires,
// confirming the investigation still exits promptly (via either the
// top-of-loop deadline check or a deadline-exceeded GetMessage error) rather
// than hanging past its parent's timeout.
func TestInvestigateJobDelivery_RealTimeoutNoJobArrives(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sessionId":"session-x"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/message":
			w.WriteHeader(http.StatusAccepted) // 202, no job — returns immediately.
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	bc := &broker.Client{
		BrokerURL:  srv.URL,
		PoolID:     1,
		Token:      "test-token",
		UseV2Flow:  true,
		HTTPClient: srv.Client(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		investigateJobDelivery(ctx, discardLogger(), bc, 1, "agent-1", "2.327.1")
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("investigateJobDelivery did not return after its parent deadline expired")
	}
}

// TestInvestigateJobDelivery_DeleteSessionFails drives the deferred
// DeleteSession-error branch: the second session registers and immediately
// receives its job (so the function returns promptly via the
// OPPORTUNISTIC-DELIVERY path), but the stub's DELETE /session route fails,
// which the deferred cleanup must log rather than panic on.
func TestInvestigateJobDelivery_DeleteSessionFails(t *testing.T) {
	t.Parallel()
	var deleteAttempted atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sessionId":"session-x"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/message":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"messageId":1,"messageType":"RunnerJobRequest","body":"{}"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/session":
			deleteAttempted.Store(true)
			http.Error(w, `{"message":"internal error"}`, http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	bc := &broker.Client{
		BrokerURL:  srv.URL,
		PoolID:     1,
		Token:      "test-token",
		UseV2Flow:  true,
		HTTPClient: srv.Client(),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		investigateJobDelivery(context.Background(), discardLogger(), bc, 1, "agent-1", "2.327.1")
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("investigateJobDelivery did not return after job delivery + failed cleanup")
	}
	if !deleteAttempted.Load() {
		t.Fatal("DeleteSession was never attempted against the stub")
	}
}

// fakeTokenProvider is a minimal tokenProvider stub for exercising runProbe
// without reaching real GitHub.
type fakeTokenProvider struct {
	token string
	err   error
}

func (f *fakeTokenProvider) Token(context.Context) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.token, nil
}

// baseRunProbeConfig returns a probeConfig sufficient to drive runProbe
// against a brokertest stub in v2 mode.
func baseRunProbeConfig(brokerURL string) probeConfig {
	return probeConfig{
		BrokerURL:     brokerURL,
		RunnerVersion: "2.327.1",
		AgentName:     "agent-1",
		AgentID:       1,
		RunnerOS:      "linux",
		RunnerArch:    "x64",
		UseV2Flow:     true,
		PoolID:        1,
	}
}

// TestRunProbe_HappyPath drives the full session lifecycle end-to-end against
// a brokertest stub: CreateSession, long-poll GetMessage, plaintext-body
// AcquireJob, then a clean shutdown via context cancellation that tears the
// session back down via the deferred DeleteSession.
func TestRunProbe_HappyPath(t *testing.T) {
	t.Parallel()
	s := brokertest.New()
	defer s.Close()

	cfg := baseRunProbeConfig(s.URL)
	provider := &fakeTokenProvider{token: "test-token"}
	getenv := getenvFromMap(map[string]string{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runProbe(ctx, discardLogger(), cfg, provider, getenv)
	}()

	// Wait for the session to be created and the poll loop to start, then
	// discover the session ID brokertest assigned and enqueue a plaintext job.
	var sessionID string
	deadline := time.After(5 * time.Second)
	for sessionID == "" {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for runProbe to register a session")
		case <-time.After(10 * time.Millisecond):
			sessions := s.RegisteredSessions()
			if len(sessions) > 0 {
				sessionID = sessions[0]
			}
		}
	}
	if !s.WaitForFirstPoll(sessionID, 5*time.Second) {
		t.Fatal("timed out waiting for the first GetMessage poll")
	}

	// No GITHUB_RUNNER_CREDENTIALS_FILE/RSA_PARAMS_FILE and no
	// GITHUB_SESSION_KEY are set, so runProbe takes the plaintext-body path
	// (json.Unmarshal of msg.Body directly) rather than AES decryption.
	s.EnqueueJob(sessionID, broker.RunnerJobRequestBody{
		RunnerRequestID: "job-1",
		BillingOwnerID:  "owner-1",
	})

	// Wait until AcquireJob has returned *client-side* before cancelling, then
	// cancel so runProbe proceeds past its blocking <-ctx.Done() and returns.
	//
	// We must not trigger on AcquireJobCalls(): the stub increments that counter
	// at the start of its handler, before the response is written, so observing
	// it == 1 can race a still-in-flight AcquireJob POST on the shared ctx —
	// cancelling then aborts the request and runProbe fails (Q258). The probe
	// calls the delete-message ("acknowledge") endpoint immediately *after*
	// AcquireJob returns, so AcknowledgeCalls() reaching 1 guarantees the
	// round-trip completed and the context is safe to cancel.
	acquireDeadline := time.After(5 * time.Second)
	for s.AcknowledgeCalls() == 0 {
		select {
		case <-acquireDeadline:
			t.Fatal("timed out waiting for AcquireJob to complete (acknowledge call)")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runProbe: unexpected error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runProbe did not return after context cancellation")
	}

	if got := s.AcquireJobCalls(); got != 1 {
		t.Errorf("AcquireJobCalls = %d, want 1", got)
	}
	if !s.WaitForSessionDelete(sessionID, 5*time.Second) {
		t.Errorf("session %q was not deleted on shutdown", sessionID)
	}
}

// TestRunProbe_TokenProviderError drives the earliest error branch: the
// installation token provider fails before any broker call is made.
func TestRunProbe_TokenProviderError(t *testing.T) {
	t.Parallel()
	s := brokertest.New()
	defer s.Close()

	cfg := baseRunProbeConfig(s.URL)
	provider := &fakeTokenProvider{err: fmt.Errorf("boom")}
	getenv := getenvFromMap(map[string]string{})

	err := runProbe(context.Background(), discardLogger(), cfg, provider, getenv)
	if err == nil {
		t.Fatal("runProbe: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "get installation token") {
		t.Errorf("runProbe error = %q, want substring %q", err.Error(), "get installation token")
	}
	if got := s.AcquireJobCalls(); got != 0 {
		t.Errorf("AcquireJobCalls = %d, want 0 (should fail before reaching the broker)", got)
	}
	if got := len(s.RegisteredSessions()); got != 0 {
		t.Errorf("RegisteredSessions = %d, want 0 (should fail before CreateSession)", got)
	}
}

// TestRunProbe_CreateSessionError drives the CreateSession failure branch:
// the token provider succeeds, but the broker rejects session creation.
func TestRunProbe_CreateSessionError(t *testing.T) {
	t.Parallel()
	s := brokertest.New()
	defer s.Close()
	// CreateSession sends ownerName=agentName ("agent-1" from
	// baseRunProbeConfig), so this prefix matches and 401s the call.
	s.FailCreateSessionForOwner("agent-")

	cfg := baseRunProbeConfig(s.URL)
	provider := &fakeTokenProvider{token: "test-token"}
	getenv := getenvFromMap(map[string]string{})

	err := runProbe(context.Background(), discardLogger(), cfg, provider, getenv)
	if err == nil {
		t.Fatal("runProbe: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "CreateSession") {
		t.Errorf("runProbe error = %q, want substring %q", err.Error(), "CreateSession")
	}
}

// TestInvestigateJobDelivery_IgnoresNonJobMessage drives the "non-job message
// on second session" branch: the first poll returns a non-RunnerJobRequest
// message, so the loop must log and continue rather than treat it as
// delivery.
func TestInvestigateJobDelivery_IgnoresNonJobMessage(t *testing.T) {
	t.Parallel()
	var pollCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sessionId":"session-x"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/message":
			n := pollCount.Add(1)
			if n == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"messageId":1,"messageType":"JobCancelMessage","body":"{}"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"messageId":2,"messageType":"RunnerJobRequest","body":"{}"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/session":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	bc := &broker.Client{
		BrokerURL:  srv.URL,
		PoolID:     1,
		Token:      "test-token",
		UseV2Flow:  true,
		HTTPClient: srv.Client(),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		investigateJobDelivery(context.Background(), discardLogger(), bc, 1, "agent-1", "2.327.1")
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("investigateJobDelivery did not return after the second (job) poll")
	}
	if got := pollCount.Load(); got < 2 {
		t.Fatalf("pollCount = %d, want >= 2 (must have polled past the non-job message)", got)
	}
}
