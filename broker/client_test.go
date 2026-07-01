package broker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/actions-gateway/github-actions-gateway/broker"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// newTestClient returns a Client pointed at the given httptest server.
// PoolID is set to 1 so tests exercise the correct VSTS API path prefix:
//
//	/_apis/distributedtask/pools/1/sessions   (CreateSession, DeleteSession)
//	/_apis/distributedtask/pools/1/messages   (GetMessage)
func newTestClient(srv *httptest.Server) *broker.Client {
	return &broker.Client{
		BrokerURL:  srv.URL,
		PoolID:     1,
		HTTPClient: srv.Client(),
		Token:      "test-token",
	}
}

// ── CreateSession ────────────────────────────────────────────────────────────

func TestCreateSession_HappyPath(t *testing.T) {
	t.Parallel()
	// Declare srv before the closure so the handler can reference it.
	// `:=` scope begins after the statement, so a forward reference inside
	// the RHS would be undefined at compile time.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/_apis/distributedtask/pools/1/sessions", r.URL.Path)
		assert.Equal(t, "5.1-preview.1", r.URL.Query().Get("api-version"))
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

		// Verify body has nested agent object (not flat fields).
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "test-agent", body["ownerName"])
		agent, ok := body["agent"].(map[string]any)
		require.True(t, ok, "agent field must be a nested object")
		assert.Equal(t, float64(42), agent["id"], "agent.id must match agentID parameter")
		assert.Equal(t, "test-agent", agent["name"])
		assert.Equal(t, "2.327.1", agent["version"])

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"sessionId": "sess-abc",
			"brokerURL": srv.URL,
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	sess, err := c.CreateSession(context.Background(), 42, "test-agent", "2.327.1")
	require.NoError(t, err)
	assert.Equal(t, "sess-abc", sess.SessionID)
	assert.Equal(t, srv.URL, sess.BrokerURL)
}

func TestCreateSession_VersionTooOld(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"runner version too old, minimum required version is 2.300.0"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.CreateSession(context.Background(), 1, "test-agent", "1.0.0")
	require.Error(t, err)
	var vtoErr *broker.VersionTooOldError
	assert.ErrorAs(t, err, &vtoErr, "expected VersionTooOldError")
}

func TestCreateSession_FallsBackToBrokerURL(t *testing.T) {
	t.Parallel()
	// When the response body omits brokerURL, fall back to Client.BrokerURL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionId": "sess-xyz"})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	sess, err := c.CreateSession(context.Background(), 42, "test-agent", "2.327.1")
	require.NoError(t, err)
	assert.Equal(t, srv.URL, sess.BrokerURL)
}

// ── GetMessage ───────────────────────────────────────────────────────────────

func TestGetMessage_NoJob(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	var gotPath, gotSessionID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotSessionID = r.URL.Query().Get("sessionId")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, _ = c.GetMessage(context.Background(), "my-session-id")
	assert.Equal(t, "/_apis/distributedtask/pools/1/messages", gotPath)
	assert.Equal(t, "my-session-id", gotSessionID)
}

// ── AcquireJob ───────────────────────────────────────────────────────────────

func TestAcquireJob_UsesPlanIDFromHeader(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// Fixed, non-secret token values for the Q247 RenewJob/AcquireJob auth tests.
// Referenced as identifiers at every use site so gosec's hardcoded-credential
// heuristic (G101) fires only on these declarations, suppressed here — rather than
// at each map/struct literal that carries the value.
const (
	jobAuthToken   = "job-scoped-token" //nolint:gosec // G101: fixed test value, not a real credential
	otherAuthToken = "other-endpoint"   //nolint:gosec // G101: fixed test value, not a real credential
)

func TestAcquireJob_ExtractsJobAuthToken(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// The run service returns the job-scoped token in the SystemVssConnection
		// endpoint's AccessToken authorization parameter (Q247).
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan": map[string]string{"planId": "plan-1"},
			"resources": map[string]any{
				"endpoints": []map[string]any{
					{
						"name": "SomeOtherEndpoint",
						"authorization": map[string]any{
							"parameters": map[string]string{"AccessToken": otherAuthToken},
						},
					},
					{
						"name": "SystemVssConnection",
						"url":  "https://run.example/job",
						"authorization": map[string]any{
							"scheme":     "OAuth",
							"parameters": map[string]string{"AccessToken": jobAuthToken},
						},
					},
				},
			},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	result, _, err := c.AcquireJob(context.Background(), srv.URL, broker.JobAcquisitionRequest{})
	require.NoError(t, err)
	assert.Equal(t, jobAuthToken, result.JobAuthToken(),
		"must extract the AccessToken from the SystemVssConnection endpoint")
}

func TestAcquireJobResponse_JobAuthToken(t *testing.T) {
	t.Parallel()
	t.Run("case-insensitive endpoint name and param key", func(t *testing.T) {
		t.Parallel()
		var r broker.AcquireJobResponse
		require.NoError(t, json.Unmarshal([]byte(`{
			"resources": {"endpoints": [
				{"name": "systemvssconnection", "authorization": {"parameters": {"accesstoken": "tok"}}}
			]}
		}`), &r))
		assert.Equal(t, "tok", r.JobAuthToken())
	})
	t.Run("no SystemVssConnection endpoint returns empty", func(t *testing.T) {
		t.Parallel()
		var r broker.AcquireJobResponse
		require.NoError(t, json.Unmarshal([]byte(`{
			"plan": {"planId": "p"},
			"resources": {"endpoints": [
				{"name": "Other", "authorization": {"parameters": {"AccessToken": "tok"}}}
			]}
		}`), &r))
		assert.Empty(t, r.JobAuthToken())
	})
	t.Run("no resources returns empty", func(t *testing.T) {
		t.Parallel()
		var r broker.AcquireJobResponse
		require.NoError(t, json.Unmarshal([]byte(`{"plan": {"planId": "p"}}`), &r))
		assert.Empty(t, r.JobAuthToken())
	})
}

func TestAcquireJob_UsesRunServiceURL(t *testing.T) {
	t.Parallel()
	// runServiceSrv is a separate server from the broker — AcquireJob must
	// target the runServiceURL parameter, not Client.BrokerURL.
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
	t.Parallel()
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
	t.Parallel()
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

func TestRenewJob_UsesJobAuthToken(t *testing.T) {
	t.Parallel()
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(broker.RenewJobResponse{LockedUntil: time.Now().Add(10 * time.Minute)})
	}))
	defer srv.Close()

	c := newTestClient(srv) // Client.Token == "test-token"
	_, err := c.RenewJob(context.Background(), srv.URL, broker.RenewJobRequest{
		PlanID:    "p",
		JobID:     "j",
		AuthToken: jobAuthToken,
	})
	require.NoError(t, err)
	// The job-scoped token authorizes per-job renewal; the broker session token is
	// rejected 401 "Not authorized for this job" (Q247).
	assert.Equal(t, "Bearer "+jobAuthToken, gotAuth)
}

func TestRenewJob_FallsBackToClientTokenWithoutJobToken(t *testing.T) {
	t.Parallel()
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(broker.RenewJobResponse{LockedUntil: time.Now().Add(10 * time.Minute)})
	}))
	defer srv.Close()

	c := newTestClient(srv) // Client.Token == "test-token"
	_, err := c.RenewJob(context.Background(), srv.URL, broker.RenewJobRequest{PlanID: "p", JobID: "j"})
	require.NoError(t, err)
	assert.Equal(t, "Bearer test-token", gotAuth)
}

func TestRenewJob_AuthTokenNotInBody(t *testing.T) {
	t.Parallel()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(broker.RenewJobResponse{LockedUntil: time.Now().Add(10 * time.Minute)})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.RenewJob(context.Background(), srv.URL, broker.RenewJobRequest{
		PlanID:    "p",
		JobID:     "j",
		AuthToken: jobAuthToken,
	})
	require.NoError(t, err)
	// AuthToken is an auth header, never a body field — it must not leak into the
	// serialized request body.
	assert.NotContains(t, body, "AuthToken")
	assert.NotContains(t, body, "authToken")
}

func TestRenewJob_NonOKResponse(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	assert.Equal(t, "/_apis/distributedtask/pools/1/sessions/sess-del", gotPath)
}

// TestGetMessage_StalledResponseBounded proves that a broker which accepts the
// connection but never sends response headers cannot block GetMessage until the
// OS TCP timeout: the client's ResponseHeaderTimeout fires, GetMessage returns
// promptly, and the failure is a retryable client-side timeout (net.Error with
// Timeout() == true) rather than a session-level error (Q108). A short
// ResponseHeaderTimeout stands in for the production value
// (broker.LongPollResponseHeaderTimeout) so the test does not wait 55s.
func TestGetMessage_StalledResponseBounded(t *testing.T) {
	t.Parallel()
	// The handler simulates a black-holed broker: it never writes a response,
	// returning only when the client aborts (the ResponseHeaderTimeout fires and
	// closes the connection, cancelling r.Context()) or a safety cap elapses.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	defer srv.Close()

	const headerTimeout = 150 * time.Millisecond
	transport := srv.Client().Transport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = headerTimeout
	hc := &http.Client{Transport: transport}
	defer transport.CloseIdleConnections()

	c := &broker.Client{BrokerURL: srv.URL, PoolID: 1, HTTPClient: hc, Token: "test-token"}

	start := time.Now()
	msg, err := c.GetMessage(context.Background(), "sess-1")
	elapsed := time.Since(start)

	require.Error(t, err, "a stalled long-poll must return an error, not hang")
	assert.Nil(t, msg)
	assert.Less(t, elapsed, 5*time.Second,
		"GetMessage must be bounded by ResponseHeaderTimeout, not the OS TCP timeout (got %s)", elapsed)

	var netErr net.Error
	require.ErrorAs(t, err, &netErr, "stalled long-poll must surface as a net.Error")
	assert.True(t, netErr.Timeout(),
		"a black-holed long-poll must be classified as a retryable timeout")
}

// TestNewHTTPClient_BoundsLongPoll verifies the production broker client carries
// a ResponseHeaderTimeout sized just above the broker's 50s long-poll hold, so a
// healthy long-poll is never cut short while a black-holed connection is bounded
// (Q108).
func TestNewHTTPClient_BoundsLongPoll(t *testing.T) {
	t.Parallel()
	hc := broker.NewHTTPClient()
	transport, ok := hc.Transport.(*http.Transport)
	require.True(t, ok, "NewHTTPClient must use an *http.Transport")
	assert.Equal(t, broker.LongPollResponseHeaderTimeout, transport.ResponseHeaderTimeout)
	assert.Greater(t, broker.LongPollResponseHeaderTimeout, 50*time.Second,
		"ResponseHeaderTimeout must exceed the broker's 50s long-poll hold")
}

// ── Rate-limit / backoff ──────────────────────────────────────────────────────

func TestGetMessage_Retry429_HonorsRetryAfter(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
		tickCh <- time.Now() // trigger exactly one renewal
		<-renewed            // wait for the HTTP round-trip to complete
	}

	// Cancel and drain the error channel — this waits for the goroutine to exit,
	// which goleak.VerifyTestMain will confirm leaves no leaked goroutines.
	cancel()
	for range errCh {
	}
}

// ── RenewJobLoop error propagation ───────────────────────────────────────────

func TestRenewJobLoop_ErrorPropagated(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("job expired"))
	}))
	defer srv.Close()

	tickCh := make(chan time.Time, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newTestClient(srv)
	errCh := c.RenewJobLoop(ctx, srv.URL, broker.RenewJobRequest{PlanID: "p", JobID: "j"}, tickCh)

	tickCh <- time.Now()

	select {
	case err := <-errCh:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "500")
	case <-time.After(2 * time.Second):
		t.Fatal("expected error from RenewJobLoop; timed out")
	}
}

// ── v2 flow ───────────────────────────────────────────────────────────────────

// newV2TestClient returns a Client with UseV2Flow enabled, pointed at srv.
func newV2TestClient(srv *httptest.Server) *broker.Client {
	return &broker.Client{
		BrokerURL:     srv.URL,
		UseV2Flow:     true,
		RunnerVersion: "2.327.1",
		RunnerOS:      "linux",
		RunnerArch:    "x64",
		HTTPClient:    srv.Client(),
		Token:         "test-token",
	}
}

func TestCreateSession_V2Flow_URL(t *testing.T) {
	t.Parallel()
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionId": "sess-v2"})
	}))
	defer srv.Close()

	c := newV2TestClient(srv)
	sess, err := c.CreateSession(context.Background(), 1, "test-agent", "2.327.1")
	require.NoError(t, err)
	assert.Equal(t, "sess-v2", sess.SessionID)
	assert.Equal(t, "/session", gotPath, "v2 must use /session, not a VSTS pool path")
	assert.NotContains(t, gotQuery, "api-version", "v2 must not send api-version")
}

func TestGetMessage_V2Flow_URL(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/message", r.URL.Path)
		q := r.URL.Query()
		assert.Equal(t, "sess-v2", q.Get("sessionId"))
		assert.Equal(t, "online", q.Get("status"))
		assert.Equal(t, "2.327.1", q.Get("runnerVersion"))
		assert.Equal(t, "linux", q.Get("os"))
		assert.Equal(t, "x64", q.Get("architecture"))
		assert.Equal(t, "false", q.Get("disableUpdate"))
		assert.NotContains(t, r.URL.RawQuery, "api-version")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := newV2TestClient(srv)
	msg, err := c.GetMessage(context.Background(), "sess-v2")
	require.NoError(t, err)
	assert.Nil(t, msg)
}

func TestGetMessage_V2Flow_AdversarialRunnerOSEscaped(t *testing.T) {
	t.Parallel()
	// Verify that a RunnerOS containing query-injection characters is properly
	// percent-encoded and does not smuggle additional query parameters.
	var gotRawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := &broker.Client{
		BrokerURL:  srv.URL,
		UseV2Flow:  true,
		RunnerOS:   "linux&admin=true",
		HTTPClient: srv.Client(),
		Token:      "test-token",
	}
	_, err := c.GetMessage(context.Background(), "sess-1")
	require.NoError(t, err)

	q, err := url.ParseQuery(gotRawQuery)
	require.NoError(t, err)
	// Adversarial value must be encoded as a single "os" parameter value.
	assert.Equal(t, "linux&admin=true", q.Get("os"),
		"RunnerOS must be a single encoded 'os' value, not split into separate params")
	// The adversarial string must not inject a separate "admin" parameter.
	assert.Empty(t, q.Get("admin"),
		"adversarial RunnerOS must not smuggle an 'admin' query parameter")
}

func TestGetMessage_V2Flow_NoOptionalParams(t *testing.T) {
	t.Parallel()
	// When RunnerVersion/RunnerOS/RunnerArch are empty, their query params
	// must be absent (not present as empty strings).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		assert.Empty(t, q.Get("runnerVersion"), "runnerVersion must be absent when not configured")
		assert.Empty(t, q.Get("os"), "os must be absent when not configured")
		assert.Empty(t, q.Get("architecture"), "architecture must be absent when not configured")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := &broker.Client{
		BrokerURL:  srv.URL,
		UseV2Flow:  true,
		HTTPClient: srv.Client(),
		Token:      "test-token",
	}
	_, err := c.GetMessage(context.Background(), "sess-1")
	require.NoError(t, err)
}

func TestDeleteSession_V2Flow_URL(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newV2TestClient(srv)
	// The sessionID argument must be ignored in v2 mode.
	err := c.DeleteSession(context.Background(), "ignored-session-id")
	require.NoError(t, err)
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, "/session", gotPath)
}

// ── Misc error status codes ───────────────────────────────────────────────────

func TestCreateSession_UnexpectedStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("service unavailable"))
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.CreateSession(context.Background(), 1, "test-agent", "2.327.1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503")
}

func TestCreateSession_Unauthorized(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprintf("HTTP%d", status), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			c := newTestClient(srv)
			_, err := c.CreateSession(context.Background(), 1, "test-agent", "2.327.1")
			require.Error(t, err)
			var unauth *broker.UnauthorizedError
			require.ErrorAs(t, err, &unauth, "expected UnauthorizedError for HTTP %d", status)
			assert.Equal(t, status, unauth.StatusCode)
		})
	}
}

func TestAcquireJob_NonOKStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("job already acquired"))
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, _, err := c.AcquireJob(context.Background(), srv.URL, broker.JobAcquisitionRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "409")
	assert.Contains(t, err.Error(), "job already acquired")
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
