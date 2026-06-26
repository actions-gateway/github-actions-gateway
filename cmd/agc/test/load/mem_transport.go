//go:build load

package load

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
)

// memBaseURL is the synthetic broker/auth base the per-session memory probe
// hands its agents. It never resolves to a real host: every request is answered
// in-process by memTransport, so no socket is ever opened.
const memBaseURL = "http://agc-mem.invalid/"

// memTransport is an in-process http.RoundTripper that answers the broker v2 and
// runner-OAuth wire protocol with canned, fixed responses and — crucially — no
// server, no socket, and no per-session server-side state.
//
// It exists to isolate the AGC's own per-session memory (Q181). The full load
// harness (Q13) runs a real httptest.Server broker stub whose per-connection
// goroutines, read/write buffers, and per-session maps all live in the same
// process, so any whole-process memory sample folds the stub's allocations into
// the per-session figure (the ~127 KiB/session upper bound). memTransport strips
// all of that away: the only things left resident per session are the AGC
// structures themselves — the listener goroutine, its broker.Client, its pooled
// agent (and key), and the Multiplexer's bookkeeping.
//
// GET …/message parks the caller on its request context until teardown cancels
// it — the resting state of an idle long-poll. No job is ever delivered, so each
// started listener holds exactly one goroutine blocked in GetMessage, which is the
// steady-state shape the per-session figure is meant to capture.
type memTransport struct {
	sessionCounter atomic.Int64
}

// RoundTrip answers the three calls a listener makes on its way to the resting
// long-poll — OAuth token exchange, CreateSession, GetMessage — plus the
// best-effort DeleteSession on exit. Anything else returns 200 with an empty
// body; the probe never drives jobs, so AcquireJob/RenewJob are never reached.
func (t *memTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Drain and close the request body so the client's write side is released and
	// nothing is left referencing it.
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}

	path := req.URL.Path
	switch {
	case strings.HasSuffix(path, "message") && req.Method == http.MethodGet:
		// Long-poll: park until the listener's context is cancelled at teardown.
		// This is the state every measured session rests in.
		<-req.Context().Done()
		return nil, req.Context().Err()

	case strings.HasSuffix(path, "token"):
		return jsonResponse(req, http.StatusOK,
			`{"access_token":"mem-bearer","token_type":"Bearer"}`), nil

	case strings.HasSuffix(path, "session") && req.Method == http.MethodPost:
		id := t.sessionCounter.Add(1)
		return jsonResponse(req, http.StatusOK,
			fmt.Sprintf(`{"sessionId":"mem-session-%d"}`, id)), nil

	default:
		// DeleteSession and any stray call: 200, empty body.
		return jsonResponse(req, http.StatusOK, ""), nil
	}
}

// jsonResponse builds a fixed-length in-memory *http.Response. ContentLength is
// set so the broker client's json.Decoder consumes the whole body and never
// blocks waiting for more.
func jsonResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// memRegistrar is an in-memory agentpool.Registrar that points every agent at
// memBaseURL (answered entirely by memTransport) and generates no key pair, so
// agentpool falls back to its configured key type. It mirrors loadRegistrar but
// needs no broker stub. clientId carries the agent ID ("client-<id>") for parity
// with the real registrar's assertion issuer, though the probe never inspects it.
type memRegistrar struct {
	nextID atomic.Int64
}

func (r *memRegistrar) Register(_ context.Context, _ string, _ agentpool.RegisterParams) (*agentpool.AgentCredentials, error) {
	id := r.nextID.Add(1)
	return &agentpool.AgentCredentials{
		AgentID:          id,
		ClientID:         fmt.Sprintf("client-%d", id),
		AuthorizationURL: memBaseURL + "token",
		BrokerURL:        memBaseURL,
	}, nil
}

func (r *memRegistrar) Deregister(_ context.Context, _ string, _ int64) error { return nil }

func (r *memRegistrar) ResolveAgentID(_ context.Context, _, _ string) (int64, error) {
	return 0, nil
}
