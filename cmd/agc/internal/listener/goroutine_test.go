package listener_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // SHA-1 required by RSA-OAEP to match server side
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/actions-gateway/github-actions-gateway/githubapp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// testRSAKey is a shared 2048-bit RSA key for tests that need a valid key
// but do not care about key uniqueness. Generated once to avoid per-test overhead.
var testRSAKey = func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}()

// ── fakeClock ────────────────────────────────────────────────────────────────

// fakeClock is an injectable Clock for deterministic time tests.
// Call Stop() before goleak.VerifyNone so all After goroutines exit.
type fakeClock struct {
	mu       sync.Mutex
	now      time.Time
	done     chan struct{}
	stopOnce sync.Once
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{now: t, done: make(chan struct{})}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// Stop signals all pending After goroutines to exit.
func (c *fakeClock) Stop() {
	c.stopOnce.Do(func() { close(c.done) })
}

// After returns a channel that fires once the clock is advanced past the
// target time. The polling goroutine exits when Stop() is called.
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	target := c.Now().Add(d)
	go func() {
		for {
			select {
			case <-c.done:
				return
			case <-time.After(time.Millisecond):
				now := c.Now()
				if !now.Before(target) {
					select {
					case ch <- now:
					default:
					}
					return
				}
			}
		}
	}()
	return ch
}

// ── oauthStub ────────────────────────────────────────────────────────────────

// oauthStub is a minimal OAuth2 token endpoint stub.
func oauthStub() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token": "stub-runner-token",
			"token_type":   "Bearer",
		})
	}))
}

// makeAgent creates a test agent whose AuthorizationURL points to oauthSrvURL.
func makeAgent(t *testing.T, oauthSrvURL string) *agentpool.Agent {
	t.Helper()
	return &agentpool.Agent{
		Index:         0,
		AgentID:       42,
		RunnerVersion: "2.327.1",
		PrivateKey:    testRSAKey,
		Creds: &githubapp.RunnerCredentials{
			ClientID:         "stub-client",
			AuthorizationURL: oauthSrvURL + "/token",
		},
	}
}

// ── brokerMux ────────────────────────────────────────────────────────────────

// brokerMux routes broker API calls to per-endpoint handlers.
type brokerMux struct {
	mu        sync.Mutex
	onCreate  func(http.ResponseWriter, *http.Request)
	onMessage func(http.ResponseWriter, *http.Request)
	onDelete  func(http.ResponseWriter, *http.Request)
	onAcquire func(http.ResponseWriter, *http.Request)
	onRenew   func(http.ResponseWriter, *http.Request)
}

func (m *brokerMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	cr, gm, del, acq, ren := m.onCreate, m.onMessage, m.onDelete, m.onAcquire, m.onRenew
	m.mu.Unlock()

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/session":
		if cr != nil {
			cr(w, r)
		} else {
			defaultSession(w)
		}
	case r.Method == http.MethodDelete && r.URL.Path == "/session":
		if del != nil {
			del(w, r)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	case r.Method == http.MethodGet && r.URL.Path == "/message":
		if gm != nil {
			gm(w, r)
		} else {
			w.WriteHeader(http.StatusAccepted)
		}
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/acquirejob"):
		if acq != nil {
			acq(w, r)
		} else {
			defaultAcquireJob(w)
		}
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/renewjob"):
		if ren != nil {
			ren(w, r)
		} else {
			defaultRenewJob(w)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (m *brokerMux) SetGetMessage(fn func(http.ResponseWriter, *http.Request)) {
	m.mu.Lock()
	m.onMessage = fn
	m.mu.Unlock()
}

func (m *brokerMux) SetCreate(fn func(http.ResponseWriter, *http.Request)) {
	m.mu.Lock()
	m.onCreate = fn
	m.mu.Unlock()
}

func (m *brokerMux) SetAcquire(fn func(http.ResponseWriter, *http.Request)) {
	m.mu.Lock()
	m.onAcquire = fn
	m.mu.Unlock()
}

func (m *brokerMux) SetRenew(fn func(http.ResponseWriter, *http.Request)) {
	m.mu.Lock()
	m.onRenew = fn
	m.mu.Unlock()
}

func defaultSession(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"sessionId": "sess-test"})
}

func defaultAcquireJob(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-plan-id", "plan-stub")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"plan": map[string]string{"planId": "plan-stub"},
	})
}

func defaultRenewJob(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"lockedUntil": time.Now().Add(time.Minute).Format(time.RFC3339)})
}

// ── condRecorder ─────────────────────────────────────────────────────────────

// condRecorder records SetCondition calls and is safe for concurrent use.
type condRecorder struct {
	mu    sync.Mutex
	conds []metav1.Condition
}

func (r *condRecorder) SetCondition(_, _ string, c metav1.Condition) {
	r.mu.Lock()
	r.conds = append(r.conds, c)
	r.mu.Unlock()
}

func (r *condRecorder) Has(condType string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conds {
		if c.Type == condType {
			return true
		}
	}
	return false
}

// ── eventRecorder ────────────────────────────────────────────────────────────

// recordedEvent captures one listener.EventRecorder.Event call.
type recordedEvent struct {
	namespace, name, eventtype, reason, action, note string
}

// eventRecorder records Event calls and is safe for concurrent use.
type eventRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (r *eventRecorder) Event(namespace, name, eventtype, reason, action, note string) {
	r.mu.Lock()
	r.events = append(r.events, recordedEvent{namespace, name, eventtype, reason, action, note})
	r.mu.Unlock()
}

// find returns the first recorded event with the given reason, or false.
func (r *eventRecorder) find(reason string) (recordedEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.reason == reason {
			return e, true
		}
	}
	return recordedEvent{}, false
}

// ── helpers ──────────────────────────────────────────────────────────────────

// makeCfg builds a listener.Config backed by the given stub servers.
func makeCfg(t *testing.T, oauthSrv, brokerSrv *httptest.Server) listener.Config {
	t.Helper()
	agent := makeAgent(t, oauthSrv.URL)
	bc := &broker.Client{
		BrokerURL:  brokerSrv.URL,
		UseV2Flow:  true,
		HTTPClient: brokerSrv.Client(),
	}
	return listener.Config{
		Group:            "test-rg",
		Namespace:        "default",
		Agent:            agent,
		Broker:           bc,
		HTTPClient:       oauthSrv.Client(),
		Clock:            listener.RealClock,
		IsLastPoller:     func() bool { return false },
		SpawnReplacement: func(_ context.Context) {},
	}
}

// runAndWait starts listener.Run in a goroutine and returns a done channel.
func runAndWait(ctx context.Context, cfg listener.Config) <-chan error {
	ch := make(chan error, 1)
	go func() {
		ch <- listener.Run(ctx, cfg)
	}()
	return ch
}

// closeHTTP closes an httptest server and drains connections so that goleak
// does not report net/http transport goroutines as false positives.
func closeHTTP(srv *httptest.Server) {
	srv.CloseClientConnections() // close in-flight connections before shutdown
	srv.Close()
	if tr, ok := srv.Client().Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

// jobMsgWithURL returns a TaskAgentMessage whose Body contains a RunnerJobRequestBody
// with RunServiceURL set to brokerSrvURL (so AcquireJob hits the stub server).
func jobMsgWithURL(brokerSrvURL string) broker.TaskAgentMessage {
	body, _ := json.Marshal(broker.RunnerJobRequestBody{
		RunnerRequestID: "req-1",
		RunServiceURL:   brokerSrvURL,
	})
	return broker.TaskAgentMessage{
		MessageID:   1,
		MessageType: "RunnerJobRequest",
		Body:        string(body),
	}
}

// ── listener goroutine tests ─────────────────────────────────────────────────

func TestListener_CreateSessionVersionTooOld(t *testing.T) {
	oauthSrv := oauthStub()
	mux := &brokerMux{}
	mux.SetCreate(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "runner version too old, minimum required", http.StatusBadRequest)
	})
	brokerSrv := httptest.NewServer(mux)

	conds := &condRecorder{}
	events := &eventRecorder{}
	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.Conditions = conds
	cfg.Events = events

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := listener.Run(ctx, cfg)
	assert.Error(t, err)
	assert.True(t, conds.Has("RunnerVersionTooOld"), "expected RunnerVersionTooOld condition")

	// The non-retriable session failure also surfaces as a Warning Event on the
	// owner, complementing the condition (Q170).
	ev, ok := events.find("RunnerVersionTooOld")
	require.True(t, ok, "expected RunnerVersionTooOld Event")
	assert.Equal(t, corev1.EventTypeWarning, ev.eventtype)
	assert.Equal(t, "test-rg", ev.name)

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

func TestListener_CreateSessionUnauthorized(t *testing.T) {
	oauthSrv := oauthStub()
	mux := &brokerMux{}
	mux.SetCreate(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "401 unauthorized", http.StatusUnauthorized)
	})
	brokerSrv := httptest.NewServer(mux)

	conds := &condRecorder{}
	events := &eventRecorder{}
	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.Conditions = conds
	cfg.Events = events

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := listener.Run(ctx, cfg)
	assert.Error(t, err)
	assert.True(t, conds.Has("Degraded"), "expected Degraded condition on 401")

	// The unauthorized session failure also records a Warning Event (Q170).
	ev, ok := events.find("SessionUnauthorized")
	require.True(t, ok, "expected SessionUnauthorized Event")
	assert.Equal(t, corev1.EventTypeWarning, ev.eventtype)
	assert.Equal(t, "test-rg", ev.name)

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

// TestListener_CreateSessionStallDoesNotWedge proves that a broker which
// accepts the connection but never responds to CreateSession cannot wedge the
// goroutine: the per-call ControlPlaneTimeout fires, Run returns a *retriable*
// error well before the outer context deadline, and the Multiplexer is free to
// restart the baseline and retry. This is the Q134 regression guard — before
// the control-plane timeout the goroutine inherited only the long-lived manager
// context and blocked inside a single CreateSession indefinitely, so the
// RunnerGroup never registered a session within the e2e budget.
func TestListener_CreateSessionStallDoesNotWedge(t *testing.T) {
	oauthSrv := oauthStub()

	// The Create handler simulates an overloaded broker that accepts the
	// connection but is slow to respond: it blocks until the test releases it
	// (stop), with a hard-cap fallback so it can never wedge the run. Note we do
	// NOT rely on r.Context() cancellation here — server-side observation of a
	// client's context-deadline disconnect is not prompt or reliable, so the
	// test drives the handler's lifetime directly via stop.
	stop := make(chan struct{})
	var once sync.Once
	handlerReturned := make(chan struct{})
	mux := &brokerMux{}
	mux.SetCreate(func(_ http.ResponseWriter, _ *http.Request) {
		select {
		case <-stop:
		case <-time.After(30 * time.Second): // safety cap; close(stop) fires first
		}
		once.Do(func() { close(handlerReturned) })
	})
	brokerSrv := httptest.NewServer(mux)

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.ControlPlaneTimeout = 200 * time.Millisecond

	// Generous outer deadline: the assertion is that Run returns far sooner,
	// proving the per-call timeout fired rather than the outer context.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err := listener.Run(ctx, cfg)
	elapsed := time.Since(start)

	require.Error(t, err, "Run must surface the stalled CreateSession, not hang")
	assert.Less(t, elapsed, 3*time.Second,
		"Run should fail fast on a stalled CreateSession (got %s)", elapsed)
	// The failure must be retriable so the Multiplexer restarts the baseline; a
	// NonRetriableError would permanently park it.
	var nre *listener.NonRetriableError
	assert.False(t, errors.As(err, &nre),
		"a control-plane timeout must be retriable, got %v", err)

	close(stop)       // release the blocked handler
	<-handlerReturned // ...and wait for it to return before the leak check
	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

func TestListener_GetMessage202Loop(t *testing.T) {
	oauthSrv := oauthStub()
	var polls atomic.Int32
	mux := &brokerMux{}
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		polls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	brokerSrv := httptest.NewServer(mux)

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IdleThreshold = 50
	cfg.IsLastPoller = func() bool { return true }

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_ = listener.Run(ctx, cfg)
	assert.GreaterOrEqual(t, int(polls.Load()), 1)

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

// TestListener_GetMessageStallDoesNotWedge proves that a broker which accepts
// the GetMessage connection but never answers cannot wedge the goroutine: the
// broker client's ResponseHeaderTimeout fires, the poll loop classifies the
// timeout as a benign "no message, retry", and the listener keeps polling rather
// than blocking on a single call for the multi-minute OS TCP timeout (Q108). The
// goroutine must neither exit nor heal the session — only retry.
func TestListener_GetMessageStallDoesNotWedge(t *testing.T) {
	oauthSrv := oauthStub()

	var polls atomic.Int32
	secondPoll := make(chan struct{})
	var once sync.Once
	mux := &brokerMux{}
	mux.SetGetMessage(func(_ http.ResponseWriter, r *http.Request) {
		if polls.Add(1) >= 2 {
			once.Do(func() { close(secondPoll) })
		}
		// Never write a response: simulate a black-holed long-poll. Return when
		// the client aborts (ResponseHeaderTimeout fires) or a safety cap elapses.
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	})
	brokerSrv := httptest.NewServer(mux)

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IsLastPoller = func() bool { return true }
	// Short ResponseHeaderTimeout stands in for the production value so the test
	// observes several bounded polls quickly instead of waiting 55s each.
	transport := brokerSrv.Client().Transport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 150 * time.Millisecond
	cfg.Broker.HTTPClient = &http.Client{Transport: transport}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runAndWait(ctx, cfg)

	// A second bounded poll proves the loop retried rather than wedging on the
	// first stalled call.
	select {
	case <-secondPoll:
	case err := <-done:
		t.Fatalf("listener exited instead of retrying a stalled poll (polls=%d, err=%v)", polls.Load(), err)
	case <-time.After(5 * time.Second):
		t.Fatalf("listener did not retry a stalled poll within 5s (polls=%d)", polls.Load())
	}

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err, "context cancellation during a stalled poll should return nil")
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not return after context cancellation")
	}

	transport.CloseIdleConnections()
	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

func TestListener_IdleShutdownAt50(t *testing.T) {
	oauthSrv := oauthStub()
	mux := &brokerMux{} // default: always 202
	brokerSrv := httptest.NewServer(mux)

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IdleThreshold = 5
	cfg.IsLastPoller = func() bool { return false }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := listener.Run(ctx, cfg)
	assert.NoError(t, err, "idle shutdown should return nil")

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

func TestListener_IdleNotShutdownIfLast(t *testing.T) {
	oauthSrv := oauthStub()
	const idleThreshold = 5
	var polls atomic.Int32
	// Closed once a poll lands strictly past the idle threshold: reaching it
	// proves the last listener kept polling instead of idle-exiting at the
	// threshold. Synchronizing on the poll event — rather than counting polls
	// inside a fixed wall-clock window — keeps the test deterministic: a slow or
	// loaded runner only delays the signal, it no longer changes the verdict
	// (Q131; the old 300ms/≥5-polls window flaked when round-trips were slow).
	pastThreshold := make(chan struct{})
	var once sync.Once
	mux := &brokerMux{}
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		if polls.Add(1) > idleThreshold {
			once.Do(func() { close(pastThreshold) })
		}
		w.WriteHeader(http.StatusAccepted)
	})
	brokerSrv := httptest.NewServer(mux)

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IdleThreshold = idleThreshold
	cfg.IsLastPoller = func() bool { return true }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runAndWait(ctx, cfg)

	select {
	case <-pastThreshold:
		// Polled past the idle threshold without exiting — the suppression held.
	case err := <-done:
		t.Fatalf("listener exited before polling past idle threshold (polls=%d, err=%v); "+
			"idle shutdown was not suppressed for the last listener", polls.Load(), err)
	case <-time.After(5 * time.Second):
		t.Fatalf("listener did not poll past idle threshold within 5s (polls=%d)", polls.Load())
	}

	cancel()
	<-done

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

func TestListener_RateLimitBackoff(t *testing.T) {
	oauthSrv := oauthStub()
	var attempts atomic.Int32
	mux := &brokerMux{}
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		n := attempts.Add(1)
		if n <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
		} else {
			w.WriteHeader(http.StatusAccepted)
		}
	})
	brokerSrv := httptest.NewServer(mux)

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IsLastPoller = func() bool { return true }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := runAndWait(ctx, cfg)
	assert.Eventually(t, func() bool { return attempts.Load() >= 3 }, 4*time.Second, 10*time.Millisecond)
	cancel()
	<-done

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

func TestListener_RateLimitedConditionAfter10Min(t *testing.T) {
	oauthSrv := oauthStub()
	clk := newFakeClock(time.Now())
	mux := &brokerMux{}
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	brokerSrv := httptest.NewServer(mux)

	conds := &condRecorder{}
	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.Conditions = conds
	cfg.IsLastPoller = func() bool { return true }
	cfg.Clock = clk

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := runAndWait(ctx, cfg)

	// Advance the fake clock past 10 minutes; the listener will see it on next iteration.
	assert.Eventually(t, func() bool {
		clk.Advance(11 * time.Minute)
		return conds.Has("RateLimited")
	}, 8*time.Second, 50*time.Millisecond, "expected RateLimited condition after 10 min of 429s")

	cancel()
	<-done
	clk.Stop()
	time.Sleep(50 * time.Millisecond)

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

func TestListener_AcquireJobThenReuse(t *testing.T) {
	oauthSrv := oauthStub()
	var delivered atomic.Bool
	var pollsAfter atomic.Int32
	mux := &brokerMux{}
	brokerSrv := httptest.NewServer(mux)

	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		if !delivered.Swap(true) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(jobMsgWithURL(brokerSrv.URL))
			return
		}
		pollsAfter.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IsLastPoller = func() bool { return true }
	cfg.JobHandler = func(_ context.Context, _, _ string, _ []byte, _ string) error { return nil }

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := runAndWait(ctx, cfg)
	assert.Eventually(t, func() bool { return pollsAfter.Load() > 0 }, 2*time.Second, 10*time.Millisecond,
		"listener should re-enter GetMessage on same session after job")
	cancel()
	<-done

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

// TestListener_AcquireJobStallDoesNotWedge proves the control-plane timeout also
// guards the job-pickup path: a broker that accepts the connection but never
// responds to AcquireJob must not wedge the goroutine inside handleJob (which
// would block job pickup so the worker pod never spawns — the Q134 class at the
// AcquireJob call site). The bounded AcquireJob fails fast, handleJob returns a
// recoverable error, and the poll loop re-enters GetMessage.
func TestListener_AcquireJobStallDoesNotWedge(t *testing.T) {
	oauthSrv := oauthStub()
	var delivered atomic.Bool
	var pollsAfter atomic.Int32
	mux := &brokerMux{}
	brokerSrv := httptest.NewServer(mux)

	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		if !delivered.Swap(true) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(jobMsgWithURL(brokerSrv.URL))
			return
		}
		pollsAfter.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})

	// AcquireJob accepts the connection but is slow to respond. Without the
	// per-call timeout the listener would wedge in handleJob here and never poll
	// again; with it, AcquireJob fails fast and the poll loop continues.
	stop := make(chan struct{})
	var once sync.Once
	handlerReturned := make(chan struct{})
	mux.SetAcquire(func(_ http.ResponseWriter, _ *http.Request) {
		select {
		case <-stop:
		case <-time.After(30 * time.Second): // safety cap; close(stop) fires first
		}
		once.Do(func() { close(handlerReturned) })
	})

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IsLastPoller = func() bool { return true }
	cfg.ControlPlaneTimeout = 200 * time.Millisecond
	cfg.JobHandler = func(_ context.Context, _, _ string, _ []byte, _ string) error { return nil }

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := runAndWait(ctx, cfg)
	assert.Eventually(t, func() bool { return pollsAfter.Load() > 0 }, 3*time.Second, 10*time.Millisecond,
		"listener should re-poll after a stalled AcquireJob times out, not wedge in handleJob")
	cancel()
	<-done

	close(stop)
	<-handlerReturned
	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

func TestListener_SpawnReplacementOnAcquire(t *testing.T) {
	oauthSrv := oauthStub()
	var spawnCalls atomic.Int32
	var delivered atomic.Bool
	mux := &brokerMux{}
	brokerSrv := httptest.NewServer(mux)

	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		if !delivered.Swap(true) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(jobMsgWithURL(brokerSrv.URL))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IsLastPoller = func() bool { return true }
	cfg.SpawnReplacement = func(_ context.Context) { spawnCalls.Add(1) }
	cfg.JobHandler = func(_ context.Context, _, _ string, _ []byte, _ string) error { return nil }

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := runAndWait(ctx, cfg)
	assert.Eventually(t, func() bool { return spawnCalls.Load() >= 1 }, 2*time.Second, 10*time.Millisecond)
	cancel()
	<-done

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

func TestListener_SessionExpiredRecreates(t *testing.T) {
	oauthSrv := oauthStub()
	var createCalls atomic.Int32
	var pollCount atomic.Int32
	mux := &brokerMux{}

	mux.SetCreate(func(w http.ResponseWriter, _ *http.Request) {
		createCalls.Add(1)
		defaultSession(w)
	})
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		n := pollCount.Add(1)
		if n == 1 {
			// First poll: return session-expired error.
			http.Error(w, "session not found: 404", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	brokerSrv := httptest.NewServer(mux)

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IsLastPoller = func() bool { return true }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := runAndWait(ctx, cfg)
	// Wait for at least 2 CreateSession calls (initial + re-create after expiry).
	assert.Eventually(t, func() bool { return createCalls.Load() >= 2 }, 4*time.Second, 10*time.Millisecond,
		"expected session re-creation after expiry")
	cancel()
	<-done

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

// ── StartRenewLoop tests ─────────────────────────────────────────────────────

func TestRenewLoop_TicksAt60s(t *testing.T) {
	var renewCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/renewjob") {
			renewCalls.Add(1)
			defaultRenewJob(w)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))

	clk := newFakeClock(time.Now())
	bc := &broker.Client{BrokerURL: srv.URL, HTTPClient: srv.Client()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop, done := listener.StartRenewLoop(ctx, bc, srv.URL, "plan-1", "job-1", nil, "default", clk, nil, 60*time.Second)

	// Advance 5 s per check — 12 steps to clear the 60 s threshold, vs the
	// original 1 s × 60 steps. The advance must stay inside Eventually to avoid
	// a race where the goroutine hasn't registered clk.After yet.
	for i := 0; i < 3; i++ {
		assert.Eventually(t, func() bool {
			clk.Advance(5 * time.Second)
			return renewCalls.Load() >= int32(i+1)
		}, 2*time.Second, time.Millisecond, "expected RenewJob call %d", i+1)
	}

	stop()
	<-done
	clk.Stop()
	// Close server and drain connections before goleak.
	srv.Close()
	if tr, ok := srv.Client().Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	time.Sleep(50 * time.Millisecond)
	goleak.VerifyNone(t)
}

func TestRenewLoop_StopsOnStop(t *testing.T) {
	clk := newFakeClock(time.Now())
	bc := &broker.Client{BrokerURL: "http://127.0.0.1:0"} // unreachable

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop, done := listener.StartRenewLoop(ctx, bc, "", "plan-1", "job-1", nil, "default", clk, nil, 60*time.Second)
	stop() // should not hang

	// done must close once the loop goroutine exits after stop().
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("done channel did not close after stop()")
	}

	clk.Stop()
	goleak.VerifyNone(t)
}

func TestRenewLoop_NonOKContinues(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/renewjob") {
			n := calls.Add(1)
			if n <= 2 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			defaultRenewJob(w)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))

	clk := newFakeClock(time.Now())
	bc := &broker.Client{BrokerURL: srv.URL, HTTPClient: srv.Client()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop, done := listener.StartRenewLoop(ctx, bc, srv.URL, "plan-1", "job-1", nil, "default", clk, nil, 60*time.Second)

	// Advance 5 s per check — 12 steps to clear the 60 s threshold, vs the
	// original 1 s × 60 steps. The advance must stay inside Eventually to avoid
	// a race where the goroutine hasn't registered clk.After yet.
	for i := 0; i < 3; i++ {
		assert.Eventually(t, func() bool {
			clk.Advance(5 * time.Second)
			return calls.Load() >= int32(i+1)
		}, 2*time.Second, time.Millisecond, "expected RenewJob call %d", i+1)
	}
	assert.Equal(t, int32(3), calls.Load(), "loop must not exit on non-OK responses")

	stop()
	<-done
	clk.Stop()
	srv.Close()
	if tr, ok := srv.Client().Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	time.Sleep(50 * time.Millisecond)
	goleak.VerifyNone(t)
}

func TestRenewLoop_NoCallAfterStop(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/renewjob") {
			calls.Add(1)
			defaultRenewJob(w)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))

	clk := newFakeClock(time.Now())
	bc := &broker.Client{BrokerURL: srv.URL, HTTPClient: srv.Client()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop, done := listener.StartRenewLoop(ctx, bc, srv.URL, "plan-1", "job-1", nil, "default", clk, nil, 60*time.Second)

	// Stop before any tick fires.
	stop()
	<-done
	clk.Stop()

	// Give any in-flight requests time to complete, then verify no calls.
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(0), calls.Load(), "no RenewJob calls expected after stop")

	srv.Close()
	if tr, ok := srv.Client().Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	time.Sleep(50 * time.Millisecond)
	goleak.VerifyNone(t)
}

// TestListener_RenewJobUsesRunnerRequestID drives a real job through the full
// Run path and asserts that the per-job renewal targets the job's
// RunnerRequestID — the same value AcquireJob sends as jobMessageId — and NOT
// the broker envelope's numeric MessageID. Sending the MessageID renews a job
// the run service does not recognize, so the lock never renews: on a job that
// outlives GitHub's lock TTL the job is recycled and redelivered to a sibling
// (a duplicate worker pod) while this worker orphans at CompleteJobAsync with
// TaskOrchestrationJobNotFoundException (Q247). MessageID and RunnerRequestID
// are deliberately distinct here so a regression cannot pass by coincidence.
func TestListener_RenewJobUsesRunnerRequestID(t *testing.T) {
	const (
		wantJobID = "req-renew-abc123"
		msgID     = int64(987654321)
	)

	oauthSrv := oauthStub()

	renewJobID := make(chan string, 1)
	mux := &brokerMux{}
	brokerSrv := httptest.NewServer(mux)

	body, _ := json.Marshal(broker.RunnerJobRequestBody{
		RunnerRequestID: wantJobID,
		RunServiceURL:   brokerSrv.URL, // must resolve so AcquireJob + RenewJob hit the stub
	})

	jobDelivered := atomic.Bool{}
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		if jobDelivered.CompareAndSwap(false, true) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(broker.TaskAgentMessage{
				MessageID:   msgID,
				MessageType: "RunnerJobRequest",
				Body:        string(body),
			})
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.SetRenew(func(w http.ResponseWriter, r *http.Request) {
		var req broker.RenewJobRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		select {
		case renewJobID <- req.JobID:
		default:
		}
		defaultRenewJob(w)
	})

	clk := newFakeClock(time.Now())
	bc := &broker.Client{
		BrokerURL:     brokerSrv.URL,
		RunnerVersion: "2.327.1",
		UseV2Flow:     true,
		HTTPClient:    brokerSrv.Client(),
	}

	// JobHandler blocks until the renew tick has been observed, so the renew loop
	// is live while we advance the clock.
	release := make(chan struct{})
	cfg := listener.Config{
		Group:         "grp",
		Namespace:     "ns",
		Agent:         makeAgent(t, oauthSrv.URL),
		Broker:        bc,
		HTTPClient:    oauthSrv.Client(),
		Clock:         clk,
		RunnerOS:      "Linux",
		RenewInterval: 60 * time.Second,
		IsLastPoller:  func() bool { return true },
		JobHandler: func(_ context.Context, _, _ string, _ []byte, _ string) error {
			<-release
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- listener.Run(ctx, cfg) }()

	// Advance past the 60s renew interval to fire the renewal, then assert the
	// jobId it carried.
	var got string
	require.Eventually(t, func() bool {
		clk.Advance(10 * time.Second)
		select {
		case got = <-renewJobID:
			return true
		default:
			return false
		}
	}, 3*time.Second, time.Millisecond, "expected a RenewJob call")
	assert.Equal(t, wantJobID, got, "RenewJob must target the job's RunnerRequestID, not the broker MessageID")

	close(release)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("goroutine did not exit after context cancellation")
	}

	clk.Stop()
	time.Sleep(20 * time.Millisecond)
	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

// ── Gap 6: refreshBrokerToken failure ────────────────────────────────────────

func TestListener_OAuthTokenFetchError(t *testing.T) {
	// OAuth stub always returns 500 — refreshBrokerToken will fail.
	oauthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	var createCalled atomic.Bool
	mux := &brokerMux{}
	mux.SetCreate(func(w http.ResponseWriter, _ *http.Request) {
		createCalled.Store(true)
		defaultSession(w)
	})
	brokerSrv := httptest.NewServer(mux)

	cfg := makeCfg(t, oauthSrv, brokerSrv)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := listener.Run(ctx, cfg)
	assert.Error(t, err, "Run should return error when OAuth token fetch fails")
	assert.False(t, createCalled.Load(), "CreateSession must not be called when OAuth fails")

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

// ── Gap 7: AcquireJob failure increments metrics counter ─────────────────────

// newTestMetrics builds a Metrics with unregistered counters safe for per-test use.
func newTestMetrics() *listener.Metrics {
	return &listener.Metrics{
		ActiveSessions: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "t_active_sessions",
		}, []string{"namespace", "runner_group"}),
		JobsAcquiredTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_jobs_acquired_total",
		}, []string{"namespace", "runner_group"}),
		JobAcquisitionErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_job_acquisition_errors_total",
		}, []string{"namespace", "reason"}),
		JobsAdmissionRejectedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_jobs_admission_rejected_total",
		}, []string{"namespace", "runner_group"}),
		TokenRefreshesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_token_refreshes_total",
		}, []string{"namespace"}),
		TokenRefreshErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_token_refresh_errors_total",
		}, []string{"namespace"}),
		RenewJobErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_renewjob_errors_total",
		}, []string{"namespace"}),
		MessagePollErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_message_poll_errors_total",
		}, []string{"namespace", "reason"}),
	}
}

func TestListener_AcquireJobError(t *testing.T) {
	oauthSrv := oauthStub()
	var delivered atomic.Bool
	mux := &brokerMux{}
	brokerSrv := httptest.NewServer(mux)

	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		if !delivered.Swap(true) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(jobMsgWithURL(brokerSrv.URL))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.SetAcquire(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	m := newTestMetrics()
	events := &eventRecorder{}
	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.Metrics = m
	cfg.Events = events
	cfg.IsLastPoller = func() bool { return true }

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := runAndWait(ctx, cfg)

	// Wait for the AcquireJob error counter to be incremented.
	assert.Eventually(t, func() bool {
		return testutil.ToFloat64(m.JobAcquisitionErrors.WithLabelValues("default", "acquirejob_failed")) >= 1
	}, 2*time.Second, 10*time.Millisecond, "JobAcquisitionErrors counter should be incremented")

	// The failed acquisition also records a Warning Event on the owner (Q170).
	ev, ok := events.find("JobAcquisitionFailed")
	require.True(t, ok, "expected JobAcquisitionFailed Event")
	assert.Equal(t, corev1.EventTypeWarning, ev.eventtype)
	assert.Equal(t, "test-rg", ev.name)

	cancel()
	<-done

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

// ── Q59: pre-acquisition admission gate ──────────────────────────────────────

// TestListener_AdmissionRejected verifies that when the admission gate returns
// ok=false the listener skips AcquireJob entirely (leaving the job queued at
// GitHub) and increments the rejection counter.
func TestListener_AdmissionRejected(t *testing.T) {
	oauthSrv := oauthStub()
	var acquireCalled atomic.Bool
	mux := &brokerMux{}
	brokerSrv := httptest.NewServer(mux)

	var delivered atomic.Bool
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		if !delivered.Swap(true) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(jobMsgWithURL(brokerSrv.URL))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.SetAcquire(func(w http.ResponseWriter, _ *http.Request) {
		acquireCalled.Store(true)
		w.WriteHeader(http.StatusInternalServerError)
	})

	m := newTestMetrics()
	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.Metrics = m
	cfg.IsLastPoller = func() bool { return true }
	// Gate is full: every delivered job is rejected.
	cfg.Admit = func(_ context.Context) (func(), bool) { return nil, false }

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := runAndWait(ctx, cfg)

	assert.Eventually(t, func() bool {
		return testutil.ToFloat64(m.JobsAdmissionRejectedTotal.WithLabelValues("default", "test-rg")) >= 1
	}, 2*time.Second, 10*time.Millisecond, "admission rejection counter should be incremented")
	assert.False(t, acquireCalled.Load(), "AcquireJob must not be called when admission is rejected")

	cancel()
	<-done
	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

// TestListener_AdmissionReleasedOnCompletion verifies that an admitted job's
// reservation is released after the job handler completes, so the slot is
// returned to the gate.
func TestListener_AdmissionReleasedOnCompletion(t *testing.T) {
	oauthSrv := oauthStub()
	mux := &brokerMux{}
	brokerSrv := httptest.NewServer(mux)

	var delivered atomic.Bool
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		if !delivered.Swap(true) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(jobMsgWithURL(brokerSrv.URL))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.SetAcquire(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"plan": map[string]string{"planId": "p1"}})
	})

	var admitCalls, releaseCalls atomic.Int32
	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.Metrics = newTestMetrics()
	cfg.IsLastPoller = func() bool { return true }
	cfg.Admit = func(_ context.Context) (func(), bool) {
		admitCalls.Add(1)
		return func() { releaseCalls.Add(1) }, true
	}
	handlerDone := make(chan struct{}, 1)
	cfg.JobHandler = func(_ context.Context, _, _ string, _ []byte, _ string) error {
		handlerDone <- struct{}{}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := runAndWait(ctx, cfg)

	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("job handler was never invoked")
	}

	// The reservation must be released once the job completes.
	assert.Eventually(t, func() bool {
		return admitCalls.Load() == 1 && releaseCalls.Load() == 1
	}, 2*time.Second, 10*time.Millisecond, "admission reservation should be released after job completion")

	cancel()
	<-done
	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

// ── Gap 8: Generic poll-error backoff ────────────────────────────────────────

func TestListener_PollErrorBackoff(t *testing.T) {
	oauthSrv := oauthStub()
	var pollCount atomic.Int32
	mux := &brokerMux{}
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		pollCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable) // generic 503, not rate-limit
	})
	brokerSrv := httptest.NewServer(mux)

	clk := newFakeClock(time.Now())
	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.Clock = clk
	cfg.IsLastPoller = func() bool { return true }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := runAndWait(ctx, cfg)

	// Wait for the first poll to hit the 503.
	assert.Eventually(t, func() bool { return pollCount.Load() >= 1 }, 2*time.Second, 10*time.Millisecond)

	// Advance the fake clock to release the backoff timer, retrying the advance
	// inside the Eventually loop. The poll loop increments pollCount *before* it
	// parks on Clock.After(wait); a single pre-advance races that park — if it
	// lands first, the timer target is computed past the advanced clock and the
	// backoff timer never fires. Re-advancing each iteration releases the timer
	// once the goroutine has parked, however late that happens.
	assert.Eventually(t, func() bool {
		clk.Advance(30 * time.Second)
		return pollCount.Load() >= 2
	}, 2*time.Second, 10*time.Millisecond,
		"goroutine should retry after backoff, not exit")

	// Confirm goroutine is still alive (has not returned).
	select {
	case err := <-done:
		t.Fatalf("goroutine exited unexpectedly with: %v", err)
	default:
	}

	cancel()
	<-done
	clk.Stop()
	time.Sleep(50 * time.Millisecond)

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

func TestBackoffDelay_HighErrorCount(t *testing.T) {
	clk := listener.RealClock
	// With >5 consecutive errors the delay must be in [30s, 60s).
	for i := 0; i < 20; i++ {
		d := listener.BackoffDelay(6, clk)
		assert.GreaterOrEqual(t, d, 30*time.Second, "high-error backoff must be ≥30s")
		assert.Less(t, d, 60*time.Second, "high-error backoff must be <60s")
	}
	// With ≤5 consecutive errors the delay must be in [15s, 30s).
	for i := 0; i < 20; i++ {
		d := listener.BackoffDelay(3, clk)
		assert.GreaterOrEqual(t, d, 15*time.Second, "low-error backoff must be ≥15s")
		assert.Less(t, d, 30*time.Second, "low-error backoff must be <30s")
	}

}

// ── decryption helpers ────────────────────────────────────────────────────────

// agentRSAPublicKey returns the RSA public key from an agent whose PrivateKey
// was generated as *rsa.PrivateKey (used only in session-key decryption tests).
func agentRSAPublicKey(t *testing.T, a *agentpool.Agent) *rsa.PublicKey {
	t.Helper()
	k, ok := a.PrivateKey.(*rsa.PrivateKey)
	require.True(t, ok, "test agent key must be *rsa.PrivateKey for this test")
	return &k.PublicKey
}

// encryptSessionKey RSA-OAEP (SHA-1) encrypts rawKey with pub, matching the
// server-side encryption that broker.DecryptSessionKey reverses.
func encryptSessionKey(t *testing.T, pub *rsa.PublicKey, rawKey []byte) []byte {
	t.Helper()
	enc, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, pub, rawKey, nil) //nolint:gosec
	require.NoError(t, err)
	return enc
}

// encryptBody AES-256-CBC encrypts plaintext with key, producing
// base64(IV || PKCS7-padded-ciphertext) — the wire format that
// broker.DecryptMessageBody reverses.
func encryptBody(t *testing.T, key, plaintext []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	require.NoError(t, err)

	// PKCS7 pad to block boundary.
	bs := block.BlockSize()
	pad := bs - len(plaintext)%bs
	padded := append(plaintext, bytes.Repeat([]byte{byte(pad)}, pad)...)

	iv := make([]byte, bs)
	_, err = io.ReadFull(rand.Reader, iv)
	require.NoError(t, err)

	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)

	return base64.StdEncoding.EncodeToString(append(iv, ciphertext...))
}

// ── TestListener_DecryptsMessageBody ─────────────────────────────────────────

// TestListener_DecryptsMessageBody verifies the end-to-end decryption path:
// CreateSession returns an RSA-encrypted AES key; GetMessage returns an
// AES-CBC-encrypted body; AcquireJob receives the correct jobMessageId from
// the decrypted RunnerJobRequestBody.
func TestListener_DecryptsMessageBody(t *testing.T) {
	defer goleak.VerifyNone(t)

	oauthSrv := oauthStub()
	defer oauthSrv.Close()
	agent := makeAgent(t, oauthSrv.URL)

	// Generate a 32-byte session AES key and encrypt it with the agent's public key.
	aesKey := make([]byte, 32)
	_, err := io.ReadFull(rand.Reader, aesKey)
	require.NoError(t, err)
	encryptedKey := encryptSessionKey(t, agentRSAPublicKey(t, agent), aesKey)

	// Create the broker server first so we can embed its URL in the encrypted body.
	jobMsgIDReceived := make(chan string, 1)
	mux := &brokerMux{}
	brokerSrv := httptest.NewServer(mux)
	defer brokerSrv.Close()

	// Encrypt a RunnerJobRequestBody whose RunServiceURL points at the test server.
	const wantJobMsgID = "req-decrypt-123"
	bodyPlain, _ := json.Marshal(broker.RunnerJobRequestBody{
		RunnerRequestID: wantJobMsgID,
		RunServiceURL:   brokerSrv.URL, // must resolve so AcquireJob succeeds
		BillingOwnerID:  "owner-1",
	})
	encryptedBody := encryptBody(t, aesKey, bodyPlain)

	mux.SetCreate(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessionId": "sess-enc",
			"encryptionKey": map[string]any{
				"value":     base64.StdEncoding.EncodeToString(encryptedKey),
				"encrypted": true,
			},
		})
	})

	jobDelivered := atomic.Bool{}
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		if jobDelivered.CompareAndSwap(false, true) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(broker.TaskAgentMessage{
				MessageID:   1,
				MessageType: "RunnerJobRequest",
				Body:        encryptedBody,
			})
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})

	mux.SetAcquire(func(w http.ResponseWriter, r *http.Request) {
		var req broker.JobAcquisitionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		jobMsgIDReceived <- req.JobMessageID
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-plan-id", "plan-enc")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan": map[string]string{"planId": "plan-enc"},
		})
	})

	ctx, cancel := context.WithCancel(context.Background())
	clk := newFakeClock(time.Now())

	bc := &broker.Client{
		BrokerURL:     brokerSrv.URL,
		RunnerVersion: "2.327.1",
		UseV2Flow:     true,
		HTTPClient:    brokerSrv.Client(),
	}

	var handlerCalled atomic.Bool
	cfg := listener.Config{
		Group:        "grp",
		Namespace:    "ns",
		Agent:        agent,
		Broker:       bc,
		Clock:        clk,
		RunnerOS:     "Linux",
		IsLastPoller: func() bool { return true },
		JobHandler: func(_ context.Context, _, _ string, _ []byte, _ string) error {
			handlerCalled.Store(true)
			cancel()
			return nil
		},
	}

	done := make(chan error, 1)
	go func() { done <- listener.Run(ctx, cfg) }()

	// Verify AcquireJob received the jobMessageId from the decrypted body.
	select {
	case got := <-jobMsgIDReceived:
		assert.Equal(t, wantJobMsgID, got, "AcquireJob must receive jobMessageId from decrypted body")
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for AcquireJob call")
	}

	// Wait for goroutine exit (JobHandler calls cancel, which drains the goroutine).
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		t.Error("goroutine did not exit after context cancellation")
	}

	// JobHandler must have been called — safe to check after goroutine exited.
	assert.True(t, handlerCalled.Load(), "JobHandler must be called after successful AcquireJob")

	// Stop clock before goleak so fakeClock.After goroutines exit.
	clk.Stop()
	time.Sleep(20 * time.Millisecond)
	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

// ── TestListener_SessionKeyPassedToHandleJob ─────────────────────────────────

// TestListener_SessionKeyPassedToHandleJob verifies that after a session
// expires and is recreated with a new encryption key K2, the listener uses K2
// (not the old K1) to decrypt subsequent messages.
//
// Structure:
//  1. Session 1 created with key K1; GetMessage immediately returns 404 (session expired).
//  2. Session 2 created with key K2; GetMessage delivers a message encrypted with K2.
//  3. AcquireJob must be called with the correct jobMessageId, proving K2 was used.
//
// If the listener cached K1 and never updated it, DecryptMessageBody(body_K2, K1)
// would produce garbage, JSON unmarshal would fail, and AcquireJob would never fire.
func TestListener_SessionKeyPassedToHandleJob(t *testing.T) {
	defer goleak.VerifyNone(t)

	oauthSrv := oauthStub()
	defer oauthSrv.Close()
	agent := makeAgent(t, oauthSrv.URL)

	// Two distinct AES-256 keys, one per session.
	k1, k2 := make([]byte, 32), make([]byte, 32)
	_, err := io.ReadFull(rand.Reader, k1)
	require.NoError(t, err)
	_, err = io.ReadFull(rand.Reader, k2)
	require.NoError(t, err)
	k2[0] ^= 0xFF // guarantee k1 ≠ k2

	encK1 := encryptSessionKey(t, agentRSAPublicKey(t, agent), k1)
	encK2 := encryptSessionKey(t, agentRSAPublicKey(t, agent), k2)

	var createCalls atomic.Int32
	mux := &brokerMux{}

	// CreateSession: first call returns K1, all subsequent calls return K2.
	mux.SetCreate(func(w http.ResponseWriter, _ *http.Request) {
		n := createCalls.Add(1)
		key := encK1
		if n >= 2 {
			key = encK2
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessionId": "sess-renewed",
			"encryptionKey": map[string]any{
				"value":     base64.StdEncoding.EncodeToString(key),
				"encrypted": true,
			},
		})
	})

	jobMsgIDReceived := make(chan string, 1)
	brokerSrv := httptest.NewServer(mux)
	defer brokerSrv.Close()

	// Body encrypted with K2 (the key from session 2).
	const wantJobMsgID = "req-renewed-key-456"
	bodyPlain, _ := json.Marshal(broker.RunnerJobRequestBody{
		RunnerRequestID: wantJobMsgID,
		RunServiceURL:   brokerSrv.URL,
		BillingOwnerID:  "owner-renewed",
	})
	encryptedBody := encryptBody(t, k2, bodyPlain)

	var pollCount atomic.Int32
	var jobDelivered atomic.Bool
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		n := pollCount.Add(1)
		if n == 1 {
			// Expire session 1 immediately.
			http.Error(w, "session not found: 404", http.StatusNotFound)
			return
		}
		if jobDelivered.CompareAndSwap(false, true) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(broker.TaskAgentMessage{
				MessageID:   1,
				MessageType: "RunnerJobRequest",
				Body:        encryptedBody,
			})
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})

	mux.SetAcquire(func(w http.ResponseWriter, r *http.Request) {
		var req broker.JobAcquisitionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		jobMsgIDReceived <- req.JobMessageID
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-plan-id", "plan-renewed")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan": map[string]string{"planId": "plan-renewed"},
		})
	})

	ctx, cancel := context.WithCancel(context.Background())
	clk := newFakeClock(time.Now())

	bc := &broker.Client{
		BrokerURL:     brokerSrv.URL,
		RunnerVersion: "2.327.1",
		UseV2Flow:     true,
		HTTPClient:    brokerSrv.Client(),
	}

	var handlerCalled atomic.Bool
	cfg := listener.Config{
		Group:        "grp",
		Namespace:    "ns",
		Agent:        agent,
		Broker:       bc,
		Clock:        clk,
		RunnerOS:     "Linux",
		IsLastPoller: func() bool { return true },
		JobHandler: func(_ context.Context, _, _ string, _ []byte, _ string) error {
			handlerCalled.Store(true)
			cancel()
			return nil
		},
	}

	done := make(chan error, 1)
	go func() { done <- listener.Run(ctx, cfg) }()

	select {
	case got := <-jobMsgIDReceived:
		assert.Equal(t, wantJobMsgID, got,
			"AcquireJob must receive jobMessageId decrypted with the renewed session key K2")
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for AcquireJob call after session key renewal")
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		t.Error("goroutine did not exit after context cancellation")
	}

	assert.True(t, handlerCalled.Load(), "JobHandler must be called after decryption with renewed key")

	clk.Stop()
	time.Sleep(20 * time.Millisecond)
	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

// TestListener_DecryptFailureFallsBackToPlaintext verifies the decryption-failure
// fallback path (H3): when the session key cannot decrypt the body, the listener
// falls back to the raw body bytes. AcquireJob is NOT called (JSON parse of
// ciphertext fails), but the JobHandler IS called with the raw payload.
func TestListener_DecryptFailureFallsBackToPlaintext(t *testing.T) {
	defer goleak.VerifyNone(t)

	oauthSrv := oauthStub()
	defer oauthSrv.Close()
	agent := makeAgent(t, oauthSrv.URL)

	// Session key K; body encrypted with a different key K2.
	aesKey := make([]byte, 32)
	wrongKey := make([]byte, 32)
	_, err := io.ReadFull(rand.Reader, aesKey)
	require.NoError(t, err)
	_, err = io.ReadFull(rand.Reader, wrongKey)
	require.NoError(t, err)

	encryptedKey := encryptSessionKey(t, agentRSAPublicKey(t, agent), aesKey)

	mux := &brokerMux{}

	var acquireCalled atomic.Bool
	mux.SetAcquire(func(w http.ResponseWriter, _ *http.Request) {
		acquireCalled.Store(true)
		defaultAcquireJob(w)
	})

	brokerSrv := httptest.NewServer(mux)
	defer brokerSrv.Close()

	// Encrypt with wrong key — decryption with aesKey will produce bad PKCS#7 padding.
	bodyPlain, _ := json.Marshal(broker.RunnerJobRequestBody{
		RunnerRequestID: "req-wrongkey",
		RunServiceURL:   brokerSrv.URL,
	})
	wrongBody := encryptBody(t, wrongKey, bodyPlain)

	mux.SetCreate(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessionId": "sess-wrongkey",
			"encryptionKey": map[string]any{
				"value":     base64.StdEncoding.EncodeToString(encryptedKey),
				"encrypted": true,
			},
		})
	})

	var jobDelivered atomic.Bool
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		if jobDelivered.CompareAndSwap(false, true) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(broker.TaskAgentMessage{
				MessageID:   1,
				MessageType: "RunnerJobRequest",
				Body:        wrongBody,
			})
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})

	ctx, cancel := context.WithCancel(context.Background())
	clk := newFakeClock(time.Now())

	bc := &broker.Client{
		BrokerURL:     brokerSrv.URL,
		RunnerVersion: "2.327.1",
		UseV2Flow:     true,
		HTTPClient:    brokerSrv.Client(),
	}

	var handlerCalled atomic.Bool
	cfg := listener.Config{
		Group:        "grp",
		Namespace:    "ns",
		Agent:        agent,
		Broker:       bc,
		Clock:        clk,
		IsLastPoller: func() bool { return true },
		JobHandler: func(_ context.Context, _, _ string, _ []byte, _ string) error {
			handlerCalled.Store(true)
			cancel()
			return nil
		},
	}

	done := make(chan error, 1)
	go func() { done <- listener.Run(ctx, cfg) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for listener to exit")
	}

	// H3: AcquireJob must not be called — garbled JSON after decrypt failure → no RunServiceURL.
	assert.False(t, acquireCalled.Load(), "AcquireJob must not be called when decryption fails")
	// JobHandler is called via the fallback path with the raw (encrypted) body.
	assert.True(t, handlerCalled.Load(), "JobHandler must be called on the fallback plaintext path")

	clk.Stop()
	time.Sleep(20 * time.Millisecond)
	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
}

// TestListener_PlaintextSessionKey verifies branch (b) of createSession (M1):
// when the server returns encryptionKey.encrypted == false, the raw key bytes are
// used directly for AES-CBC decryption without any RSA step.
func TestListener_PlaintextSessionKey(t *testing.T) {
	defer goleak.VerifyNone(t)

	oauthSrv := oauthStub()
	defer oauthSrv.Close()
	agent := makeAgent(t, oauthSrv.URL)

	rawKey := make([]byte, 32)
	_, err := io.ReadFull(rand.Reader, rawKey)
	require.NoError(t, err)

	mux := &brokerMux{}
	brokerSrv := httptest.NewServer(mux)
	defer brokerSrv.Close()

	const wantJobMsgID = "req-plaintext-key-789"
	bodyPlain, _ := json.Marshal(broker.RunnerJobRequestBody{
		RunnerRequestID: wantJobMsgID,
		RunServiceURL:   brokerSrv.URL,
		BillingOwnerID:  "owner-plain",
	})
	encryptedBody := encryptBody(t, rawKey, bodyPlain)

	mux.SetCreate(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessionId": "sess-plain-key",
			"encryptionKey": map[string]any{
				// encrypted: false → raw bytes used directly (no RSA step).
				"value":     base64.StdEncoding.EncodeToString(rawKey),
				"encrypted": false,
			},
		})
	})

	var jobDelivered atomic.Bool
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		if jobDelivered.CompareAndSwap(false, true) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(broker.TaskAgentMessage{
				MessageID:   1,
				MessageType: "RunnerJobRequest",
				Body:        encryptedBody,
			})
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})

	jobMsgIDReceived := make(chan string, 1)
	mux.SetAcquire(func(w http.ResponseWriter, r *http.Request) {
		var req broker.JobAcquisitionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		jobMsgIDReceived <- req.JobMessageID
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-plan-id", "plan-plain")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan": map[string]string{"planId": "plan-plain"},
		})
	})

	ctx, cancel := context.WithCancel(context.Background())
	clk := newFakeClock(time.Now())

	bc := &broker.Client{
		BrokerURL:     brokerSrv.URL,
		RunnerVersion: "2.327.1",
		UseV2Flow:     true,
		HTTPClient:    brokerSrv.Client(),
	}

	cfg := listener.Config{
		Group:        "grp",
		Namespace:    "ns",
		Agent:        agent,
		Broker:       bc,
		Clock:        clk,
		IsLastPoller: func() bool { return true },
		JobHandler: func(_ context.Context, _, _ string, _ []byte, _ string) error {
			cancel()
			return nil
		},
	}

	done := make(chan error, 1)
	go func() { done <- listener.Run(ctx, cfg) }()

	// M1: AcquireJob must receive the correct jobMessageId proving the raw key worked.
	select {
	case got := <-jobMsgIDReceived:
		assert.Equal(t, wantJobMsgID, got, "AcquireJob must receive correct jobMessageId with plaintext session key")
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for AcquireJob call with plaintext session key")
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		t.Error("goroutine did not exit after context cancellation")
	}

	clk.Stop()
	time.Sleep(20 * time.Millisecond)
	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
}

// TestListener_NoSessionKey verifies branch (c) of createSession (M1): when the
// server returns no encryptionKey field, aesKey stays nil and messages are parsed
// as plaintext JSON directly.
func TestListener_NoSessionKey(t *testing.T) {
	defer goleak.VerifyNone(t)

	oauthSrv := oauthStub()
	defer oauthSrv.Close()
	agent := makeAgent(t, oauthSrv.URL)

	mux := &brokerMux{}
	brokerSrv := httptest.NewServer(mux)
	defer brokerSrv.Close()

	mux.SetCreate(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionId": "sess-nokey"})
	})

	const wantJobMsgID = "req-nokey-101"
	var jobDelivered atomic.Bool
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		if jobDelivered.CompareAndSwap(false, true) {
			body, _ := json.Marshal(broker.RunnerJobRequestBody{
				RunnerRequestID: wantJobMsgID,
				RunServiceURL:   brokerSrv.URL,
			})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(broker.TaskAgentMessage{
				MessageID:   1,
				MessageType: "RunnerJobRequest",
				Body:        string(body),
			})
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})

	jobMsgIDReceived := make(chan string, 1)
	mux.SetAcquire(func(w http.ResponseWriter, r *http.Request) {
		var req broker.JobAcquisitionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		jobMsgIDReceived <- req.JobMessageID
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-plan-id", "plan-nokey")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan": map[string]string{"planId": "plan-nokey"},
		})
	})

	ctx, cancel := context.WithCancel(context.Background())
	clk := newFakeClock(time.Now())

	bc := &broker.Client{
		BrokerURL:     brokerSrv.URL,
		RunnerVersion: "2.327.1",
		UseV2Flow:     true,
		HTTPClient:    brokerSrv.Client(),
	}

	cfg := listener.Config{
		Group:        "grp",
		Namespace:    "ns",
		Agent:        agent,
		Broker:       bc,
		Clock:        clk,
		IsLastPoller: func() bool { return true },
		JobHandler: func(_ context.Context, _, _ string, _ []byte, _ string) error {
			cancel()
			return nil
		},
	}

	done := make(chan error, 1)
	go func() { done <- listener.Run(ctx, cfg) }()

	// M1: no session key → plaintext body delivered directly to AcquireJob.
	select {
	case got := <-jobMsgIDReceived:
		assert.Equal(t, wantJobMsgID, got, "AcquireJob must receive correct jobMessageId with no session key")
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for AcquireJob call with no session key")
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		t.Error("goroutine did not exit after context cancellation")
	}

	clk.Stop()
	time.Sleep(20 * time.Millisecond)
	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
}
