//go:build load

package load

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
)

// scalesetMemBase is the synthetic GitHub API base the scale-set memory probe
// hands its clients. It never resolves to a real host: every request is answered
// in-process by scalesetMemTransport, so no socket is ever opened. The admin
// connection this transport mints points back at the same base, so the
// _apis/runtime calls land here too.
const scalesetMemBase = "http://agc-scaleset-mem.invalid"

// scalesetMemScaleSetID is the id every by-name lookup resolves to. One id for
// every set is fine: the probe never acquires a job, so nothing keys on it.
const scalesetMemScaleSetID = 7

// scalesetMemTransport answers the runner-scale-set protocol's bootstrap and its
// resting long poll with canned responses and no server, no socket, and no
// per-session server-side state.
//
// It is the scale-set counterpart of memTransport, and exists for the same reason
// (Q722, following Q181). scalesettest serves the real protocol model over an
// httptest.Server, which is the right venue for behaviour — but every parked long
// poll there holds a server goroutine and its read/write buffers in this same
// process, so a whole-process sample folds them into the per-session figure the
// probe is trying to isolate. Stripping the server away leaves only the AGC
// structures: the Listener, its scaleset.Client and transports, and its live
// session state.
//
// The message-queue GET parks the caller on its request context until teardown
// cancels it, which is the steady state of a scale set with nothing to deliver:
// one goroutine blocked in GetMessage per live Listener. No job is ever delivered,
// so no provisioning path is entered.
type scalesetMemTransport struct {
	sessionCounter atomic.Int64
	// parkedPolls is how many message polls are blocked right now. It is the
	// harness's own denominator check: the figure divides by the set count, so a
	// listener that never reached its long poll — or left it to retry a transient
	// error — would be measured as resting when it is not, and nothing in a
	// goroutine count distinguishes the two.
	parkedPolls atomic.Int64
}

// parked reports how many message polls are blocked in their long poll right now.
func (t *scalesetMemTransport) parked() int64 { return t.parkedPolls.Load() }

// sessions reports how many sessions have been opened.
func (t *scalesetMemTransport) sessions() int64 { return t.sessionCounter.Load() }

// RoundTrip answers the calls a Listener makes on its way to the resting long
// poll, in the order it makes them: the REST registration-token hop, the RemoteAuth
// runner-registration hop that discovers the tenant URL and mints the admin JWT,
// the by-name scale-set lookup that resolves an existing set (so no create is
// attempted), the session open, and then the parked message poll. Anything else
// answers 200 with an empty body.
func (t *scalesetMemTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Drain and close the request body so the client's write side is released and
	// nothing is left referencing it.
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}

	path := req.URL.Path
	switch {
	case strings.Contains(path, "/message") && req.Method == http.MethodGet:
		// The resting state every measured session sits in.
		t.parkedPolls.Add(1)
		<-req.Context().Done()
		t.parkedPolls.Add(-1)
		return nil, req.Context().Err()

	case strings.HasSuffix(path, "/actions/runners/registration-token"):
		return jsonResponse(req, http.StatusCreated, `{"token":"scaleset-mem-registration"}`), nil

	case strings.HasSuffix(path, "/actions/runner-registration"):
		return jsonResponse(req, http.StatusOK,
			fmt.Sprintf(`{"url":%q,"token":"scaleset-mem-admin"}`, scalesetMemBase)), nil

	case strings.HasSuffix(path, "/sessions") && req.Method == http.MethodPost:
		// One session per Listener. messageQueueUrl points back here, so the poll
		// below is what the client blocks on.
		id := t.sessionCounter.Add(1)
		return jsonResponse(req, http.StatusOK, fmt.Sprintf(
			`{"sessionId":"scaleset-mem-session-%d","ownerName":"mem","messageQueueUrl":%q,`+
				`"messageQueueAccessToken":"scaleset-mem-queue",`+
				`"statistics":{"totalAvailableJobs":0,"totalAssignedJobs":0}}`,
			id, scalesetMemBase+"/_apis/runtime/message")), nil

	case strings.HasPrefix(path, "/_apis/runtime/runnerscalesets") && req.Method == http.MethodGet:
		// Resolve an existing set, so ensureScaleSet adopts rather than creates.
		return jsonResponse(req, http.StatusOK, fmt.Sprintf(
			`{"count":1,"value":[{"id":%d,"name":%q,"runnerGroupId":1,`+
				`"labels":[{"name":%q,"type":"System"}]}]}`,
			scalesetMemScaleSetID, scalesetMemSetName(req), scalesetMemSetName(req))), nil

	default:
		// DeleteSession, the runner-group lookup, and any stray call.
		return jsonResponse(req, http.StatusOK, `{}`), nil
	}
}

// scalesetMemSetName echoes the requested scale-set name back in the lookup
// response, so the Listener's own label reconciliation sees the set it asked for
// and takes no corrective path. Falls back to a fixed name for an unfiltered list.
func scalesetMemSetName(req *http.Request) string {
	if n := req.URL.Query().Get("name"); n != "" {
		return n
	}
	return "scaleset-mem"
}
