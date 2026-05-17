package broker_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/karlkfi/github-actions-gateway/broker"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// newTestClient returns a BrokerClient pointed at the given httptest server.
func newTestClient(srv *httptest.Server) *broker.BrokerClient {
	return &broker.BrokerClient{
		BrokerURL:  srv.URL,
		HTTPClient: srv.Client(),
		Token:      "test-token",
	}
}

// ── CreateSession ────────────────────────────────────────────────────────────

func TestCreateSession_HappyPath(t *testing.T) {
	// Declare srv before the closure so the handler can reference it.
	// `:=` scope begins after the statement, so a forward reference inside
	// the RHS would be undefined at compile time.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/sessions", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"sessionId": "sess-abc",
			"brokerURL": srv.URL,
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	sessionID, brokerURL, err := c.CreateSession(context.Background(), "2.327.1")
	require.NoError(t, err)
	assert.Equal(t, "sess-abc", sessionID)
	assert.Equal(t, srv.URL, brokerURL)
}

func TestCreateSession_VersionTooOld(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"runner version too old, minimum required version is 2.300.0"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, _, err := c.CreateSession(context.Background(), "1.0.0")
	require.Error(t, err)
	var vtoErr *broker.VersionTooOldError
	assert.ErrorAs(t, err, &vtoErr, "expected VersionTooOldError")
}

func TestCreateSession_FallsBackToBrokerURL(t *testing.T) {
	// When the response body omits brokerURL, fall back to BrokerClient.BrokerURL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionId": "sess-xyz"})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, brokerURL, err := c.CreateSession(context.Background(), "2.327.1")
	require.NoError(t, err)
	assert.Equal(t, srv.URL, brokerURL)
}

// ── GetMessage ───────────────────────────────────────────────────────────────

func TestGetMessage_NoJob(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted) // 202 = no job queued
	}))
	defer srv.Close()

	c := newTestClient(srv)
	msg, err := c.GetMessage(context.Background(), "sess-1")
	require.NoError(t, err)
	assert.Nil(t, msg, "expected nil message on 202")
}

func TestGetMessage_JobAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(broker.TaskAgentMessage{
			MessageID:   42,
			MessageType: "RunnerJobRequest",
			Body:        "encrypted-body",
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	msg, err := c.GetMessage(context.Background(), "sess-1")
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, int64(42), msg.MessageID)
	assert.Equal(t, "RunnerJobRequest", msg.MessageType)
}

func TestGetMessage_UsesSessionID(t *testing.T) {
	var gotSessionID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSessionID = r.URL.Query().Get("sessionId")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, _ = c.GetMessage(context.Background(), "my-session-id")
	assert.Equal(t, "my-session-id", gotSessionID)
}

// ── AcquireJob ───────────────────────────────────────────────────────────────

func TestAcquireJob_UsesPlanIDFromHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/acquirejob", r.URL.Path)
		w.Header().Set("x-plan-id", "plan-from-header")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan": map[string]string{"planId": "plan-from-body"},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	result, _, err := c.AcquireJob(context.Background(), srv.URL, broker.JobAcquisitionRequest{
		JobMessageID:   "req-1",
		RunnerOS:       "Linux",
		BillingOwnerID: "owner-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "plan-from-header", result.Plan.PlanID, "header planId should take precedence")
}

func TestAcquireJob_FallsBackToPlanIDFromBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No x-plan-id header.
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan": map[string]string{"planId": "plan-from-body"},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	result, _, err := c.AcquireJob(context.Background(), srv.URL, broker.JobAcquisitionRequest{})
	require.NoError(t, err)
	assert.Equal(t, "plan-from-body", result.Plan.PlanID)
}

func TestAcquireJob_UsesRunServiceURL(t *testing.T) {
	// runServiceSrv is a separate server from the broker — AcquireJob must
	// target the runServiceURL parameter, not BrokerClient.BrokerURL.
	var acquireJobHit bool
	runServiceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acquireJobHit = true
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"plan": map[string]string{"planId": "p"}})
	}))
	defer runServiceSrv.Close()

	brokerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("AcquireJob must not call BrokerURL; it called %s", r.URL.Path)
	}))
	defer brokerSrv.Close()

	c := newTestClient(brokerSrv)
	_, _, err := c.AcquireJob(context.Background(), runServiceSrv.URL, broker.JobAcquisitionRequest{})
	require.NoError(t, err)
	assert.True(t, acquireJobHit, "acquirejob endpoint on runServiceURL must have been called")
}

func TestAcquireJob_ReturnsRawBody(t *testing.T) {
	want := `{"plan":{"planId":"p123"},"extra":"data"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(want))
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, raw, err := c.AcquireJob(context.Background(), srv.URL, broker.JobAcquisitionRequest{})
	require.NoError(t, err)
	assert.JSONEq(t, want, string(raw))
}

// ── RenewJob ─────────────────────────────────────────────────────────────────

func TestRenewJob_UsesRunServiceURL(t *testing.T) {
	var renewHit bool
	runServiceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		renewHit = true
		assert.Equal(t, "/renewjob", r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(broker.RenewJobResponse{LockedUntil: time.Now().Add(10 * time.Minute)})
	}))
	defer runServiceSrv.Close()

	brokerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("RenewJob must not call BrokerURL; it called %s", r.URL.Path)
	}))
	defer brokerSrv.Close()

	c := newTestClient(brokerSrv)
	_, err := c.RenewJob(context.Background(), runServiceSrv.URL, broker.RenewJobRequest{PlanID: "p", JobID: "j"})
	require.NoError(t, err)
	assert.True(t, renewHit)
}

func TestRenewJob_NonOKResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.RenewJob(context.Background(), srv.URL, broker.RenewJobRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestRenewJob_StopsOnCancel(t *testing.T) {
	// Start a renew loop in a goroutine and cancel the context; verify the
	// goroutine exits cleanly (goleak.VerifyTestMain handles the leak check).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(broker.RenewJobResponse{LockedUntil: time.Now().Add(10 * time.Minute)})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := newTestClient(srv)

	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := c.RenewJob(ctx, srv.URL, broker.RenewJobRequest{PlanID: "p", JobID: "j"}); err != nil {
					return
				}
			}
		}
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("renew goroutine did not exit after context cancellation")
	}
}

// ── DeleteSession ─────────────────────────────────────────────────────────────

func TestDeleteSession_IssuesDELETE(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	err := c.DeleteSession(context.Background(), "sess-del")
	require.NoError(t, err)
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, "/sessions/sess-del", gotPath)
}

// ── Rate-limit / backoff ──────────────────────────────────────────────────────

func TestGetMessage_Retry429_HonorsRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.GetMessage(context.Background(), "sess-1")
	require.Error(t, err)

	var rlErr *broker.RateLimitError
	require.ErrorAs(t, err, &rlErr)
	assert.Equal(t, 30*time.Second, rlErr.RetryAfter)
}

func TestGetMessage_Retry429_ExponentialFallback(t *testing.T) {
	// No Retry-After header — RetryAfter should be -1 indicating fallback.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.GetMessage(context.Background(), "sess-1")
	require.Error(t, err)

	var rlErr *broker.RateLimitError
	require.ErrorAs(t, err, &rlErr)
	assert.Equal(t, time.Duration(-1), rlErr.RetryAfter, "RetryAfter -1 signals exponential fallback")
}

func TestGetMessage_Retry429_CounterIncremented(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	counter := &stubPollMetrics{}
	c := newTestClient(srv)
	c.PollMetrics = counter

	for i := 0; i < 3; i++ {
		_, _ = c.GetMessage(context.Background(), "sess-1")
	}

	assert.Equal(t, int64(3), counter.rateLimited.Load(),
		"IncPollError(\"rate_limited\") must be called once per 429 response")
}

// ── RenewJobLoop ──────────────────────────────────────────────────────────────

// TestRenewJob_Interval verifies that RenewJobLoop calls RenewJob exactly once
// per tick with no drift. A manually-driven channel replaces time.Ticker so the
// test advances time without sleeping and gets a deterministic call count.
func TestRenewJob_Interval(t *testing.T) {
	// renewed is signalled by the server handler after each successful renewjob.
	renewed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(broker.RenewJobResponse{
			LockedUntil: time.Now().Add(10 * time.Minute),
		})
		renewed <- struct{}{}
	}))
	defer srv.Close()

	// Unbuffered channel: sending a tick blocks until the goroutine selects it,
	// giving us precise sequencing with no sleep-based synchronisation.
	tickCh := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newTestClient(srv)
	errCh := c.RenewJobLoop(ctx, srv.URL, broker.RenewJobRequest{PlanID: "p", JobID: "j"}, tickCh)

	const wantRenewals = 3
	for i := 0; i < wantRenewals; i++ {
		tickCh <- time.Now()  // trigger exactly one renewal
		<-renewed             // wait for the HTTP round-trip to complete
	}

	// Cancel and drain the error channel — this waits for the goroutine to exit,
	// which goleak.VerifyTestMain will confirm leaves no leaked goroutines.
	cancel()
	for range errCh {
	}
}

// ── Test helpers ──────────────────────────────────────────────────────────────

// stubPollMetrics is a test-only PollMetricsRecorder that counts calls by label.
type stubPollMetrics struct {
	rateLimited atomic.Int64
}

func (s *stubPollMetrics) IncPollError(reason string) {
	switch reason {
	case "rate_limited":
		s.rateLimited.Add(1)
	}
}
