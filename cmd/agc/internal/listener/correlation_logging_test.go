package listener_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// lockedLogBuffer is a goroutine-safe io.Writer for capturing slog output, since
// the listener may log from more than one goroutine while a job is in flight.
type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "log line is not JSON: %s", line)
		recs = append(recs, rec)
	}
	return recs
}

// TestListener_HotPathLogsAreDebugWithCorrelation pins the Q87 logging contract:
// the per-session "listener goroutine started" and per-job "job message received"
// lines are emitted at DEBUG (Theme D), and every line carries the
// group/namespace/sessionId correlation fields woven onto the logger context
// (Theme F) — never at INFO.
func TestListener_HotPathLogsAreDebugWithCorrelation(t *testing.T) {
	defer goleak.VerifyNone(t)

	oauthSrv := oauthStub()
	agent := makeAgent(t, oauthSrv.URL)

	mux := &brokerMux{}
	mux.SetCreate(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"sessionId": "sess-q87"})
	})
	var delivered atomic.Bool
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		// Deliver exactly one job (no RunServiceURL, so AcquireJob is skipped),
		// then 202 forever. The JobHandler cancels the context to end the run.
		if delivered.CompareAndSwap(false, true) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(broker.TaskAgentMessage{
				MessageID:   7,
				MessageType: "RunnerJobRequest",
				Body:        `{}`,
			})
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	brokerSrv := httptest.NewServer(mux)

	buf := &lockedLogBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.Agent = agent
	cfg.Log = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg.JobHandler = func(_ context.Context, _, _ string, _ []byte, _ string) error {
		cancel()
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- listener.Run(ctx, cfg) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("listener did not exit after JobHandler cancelled the context")
	}

	recs := buf.records(t)

	assertHotPathLine := func(msg string) {
		var found map[string]any
		for _, rec := range recs {
			if rec["msg"] == msg {
				found = rec
				assert.Equal(t, "DEBUG", rec["level"], "%q must be demoted to DEBUG (Q87 Theme D)", msg)
			}
		}
		require.NotNil(t, found, "expected a log line %q", msg)
		// Correlation fields woven onto the logger context (Q87 Theme F).
		assert.Equal(t, "test-rg", found["group"], "%q must carry group", msg)
		assert.Equal(t, "default", found["namespace"], "%q must carry namespace", msg)
		assert.Equal(t, "sess-q87", found["sessionId"], "%q must carry sessionId", msg)
	}

	assertHotPathLine("listener goroutine started")
	assertHotPathLine("job message received")

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
}
