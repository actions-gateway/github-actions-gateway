package listener_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karlkfi/github-actions-gateway/agc/internal/agentpool"
	"github.com/karlkfi/github-actions-gateway/agc/internal/listener"
	"github.com/karlkfi/github-actions-gateway/broker"
	"github.com/karlkfi/github-actions-gateway/githubapp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

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
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return &agentpool.Agent{
		Index:         0,
		AgentID:       42,
		RunnerVersion: "2.327.1",
		PrivateKey:    key,
		Creds: &githubapp.RunnerCredentials{
			ClientID:         "stub-client",
			AuthorizationURL: oauthSrvURL + "/token",
		},
	}
}

// brokerMux routes broker API calls to per-endpoint handlers.
type brokerMux struct {
	mu        sync.Mutex
	onCreate  func(http.ResponseWriter, *http.Request)
	onMessage func(http.ResponseWriter, *http.Request)
	onDelete  func(http.ResponseWriter, *http.Request)
}

func (m *brokerMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	cr, gm, del := m.onCreate, m.onMessage, m.onDelete
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

func defaultSession(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"sessionId": "sess-test"})
}

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

// makeCfg builds a listener.Config backed by the given stub servers.
func makeCfg(t *testing.T, oauthSrv, brokerSrv *httptest.Server) listener.Config {
	t.Helper()
	agent := makeAgent(t, oauthSrv.URL)
	bc := &broker.BrokerClient{
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
		IsLastListener:   func() bool { return false },
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

// closeHTTP closes an httptest server and drains idle client connections so that
// goleak does not report net/http transport goroutines as false positives.
func closeHTTP(srv *httptest.Server) {
	srv.Close()
	if tr, ok := srv.Client().Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	time.Sleep(50 * time.Millisecond)
}

func TestListener_CreateSessionVersionTooOld(t *testing.T) {
	oauthSrv := oauthStub()
	mux := &brokerMux{}
	mux.SetCreate(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "runner version too old, minimum required", http.StatusBadRequest)
	})
	brokerSrv := httptest.NewServer(mux)

	conds := &condRecorder{}
	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.Conditions = conds

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := listener.Run(ctx, cfg)
	assert.Error(t, err)
	assert.True(t, conds.Has("RunnerVersionTooOld"), "expected RunnerVersionTooOld condition")

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
	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.Conditions = conds

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := listener.Run(ctx, cfg)
	assert.Error(t, err)
	assert.True(t, conds.Has("Degraded"), "expected Degraded condition on 401")

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
	cfg.IsLastListener = func() bool { return true }

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_ = listener.Run(ctx, cfg)
	assert.GreaterOrEqual(t, int(polls.Load()), 1)

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
	cfg.IsLastListener = func() bool { return false }

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
	var polls atomic.Int32
	mux := &brokerMux{}
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		polls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	brokerSrv := httptest.NewServer(mux)

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IdleThreshold = 5
	cfg.IsLastListener = func() bool { return true }

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_ = listener.Run(ctx, cfg)
	assert.GreaterOrEqual(t, int(polls.Load()), 5, "should poll past threshold when last listener")

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
	cfg.IsLastListener = func() bool { return true }

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

func TestListener_AcquireJobThenReuse(t *testing.T) {
	oauthSrv := oauthStub()
	var delivered atomic.Bool
	var pollsAfter atomic.Int32
	mux := &brokerMux{}
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		if !delivered.Swap(true) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(broker.TaskAgentMessage{
				MessageID:   1,
				MessageType: "RunnerJobRequest",
				Body:        `{}`,
			})
			return
		}
		pollsAfter.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	brokerSrv := httptest.NewServer(mux)

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IsLastListener = func() bool { return true }
	cfg.JobHandler = func(_ context.Context, _, _ string, _ []byte) error { return nil }

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

func TestListener_SpawnReplacementOnAcquire(t *testing.T) {
	oauthSrv := oauthStub()
	var spawnCalls atomic.Int32
	var delivered atomic.Bool
	mux := &brokerMux{}
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		if !delivered.Swap(true) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(broker.TaskAgentMessage{
				MessageID:   1,
				MessageType: "RunnerJobRequest",
				Body:        `{}`,
			})
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	brokerSrv := httptest.NewServer(mux)

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IsLastListener = func() bool { return true }
	cfg.SpawnReplacement = func(_ context.Context) { spawnCalls.Add(1) }
	cfg.JobHandler = func(_ context.Context, _, _ string, _ []byte) error { return nil }

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
