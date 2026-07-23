package scaleset_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
)

// recordingObserver captures every ResponseInfo the client reports.
type recordingObserver struct {
	mu   sync.Mutex
	seen []scaleset.ResponseInfo
}

func (o *recordingObserver) ObserveResponse(info scaleset.ResponseInfo) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, info)
}

func (o *recordingObserver) snapshot() []scaleset.ResponseInfo {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]scaleset.ResponseInfo(nil), o.seen...)
}

// ops returns the Op of each observed response, in order.
func (o *recordingObserver) ops() []string {
	var out []string
	for _, i := range o.snapshot() {
		out = append(out, i.Op)
	}
	return out
}

// firstOp returns the first observed response with the given Op.
func (o *recordingObserver) firstOp(t *testing.T, op string) scaleset.ResponseInfo {
	t.Helper()
	for _, i := range o.snapshot() {
		if i.Op == op {
			return i
		}
	}
	t.Fatalf("no %s response observed; saw %v", op, o.ops())
	return scaleset.ResponseInfo{}
}

// newObservedClient builds a client wired to the stub and to obs.
func newObservedClient(t *testing.T, srv *scalesettest.Server, obs scaleset.ResponseObserver) *scaleset.Client {
	t.Helper()
	c, err := scaleset.New(scaleset.Config{
		TokenProvider: fakeProvider{},
		ConfigURL:     "https://github.com/test-org",
		APIBase:       srv.URL,
		HTTPClient:    srv.HTTPClient(),
		PollClient:    srv.HTTPClient(),
		Observer:      obs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestObserver_ReportsEveryHopIncludingTheOnesTheTypedAPIHides is the contract the
// probe depends on: an observer must see the two bootstrap hops, the service calls,
// and — critically — a queue poll that GetMessage reports as (nil, nil) because it
// answered 202. Without that, a caller driving the client cannot report what the wire
// actually did (Q362).
func TestObserver_ReportsEveryHopIncludingTheOnesTheTypedAPIHides(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	obs := &recordingObserver{}
	c := newObservedClient(t, srv, obs)

	ctx := context.Background()
	_, sess := setupScaleSet(t, ctx, c)

	msg, err := c.GetMessage(ctx, sess, 1, 0)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg != nil {
		t.Fatalf("expected an empty poll, got message %d", msg.MessageID)
	}

	for _, want := range []string{"RegistrationToken", "RunnerRegistration", "ServiceCall", "GetMessage"} {
		if !containsOp(obs.ops(), want) {
			t.Errorf("observer never saw %s; saw %v", want, obs.ops())
		}
	}

	poll := obs.firstOp(t, "GetMessage")
	if poll.Status != http.StatusAccepted {
		t.Errorf("poll status = %d, want 202 — the status GetMessage collapses to (nil, nil)", poll.Status)
	}
	if poll.Method != http.MethodGet {
		t.Errorf("poll method = %q, want GET", poll.Method)
	}
	if poll.Host == "" {
		t.Error("poll Host is empty; the observer must report which backend answered")
	}
	if poll.Elapsed <= 0 {
		t.Error("poll Elapsed must be positive — it is the long-poll hold evidence")
	}
	if poll.Header == nil {
		t.Error("poll Header must be reported — it carries the rate-limit evidence")
	}
}

// TestObserver_PathNeverCarriesTheSignedQuery pins the privacy property of
// ResponseInfo: the message-queue URL carries a signed query string, so an observer
// is handed Host and Path and never a query.
func TestObserver_PathNeverCarriesTheSignedQuery(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	obs := &recordingObserver{}
	c := newObservedClient(t, srv, obs)

	ctx := context.Background()
	_, sess := setupScaleSet(t, ctx, c)
	if _, err := c.GetMessage(ctx, sess, 1, 0); err != nil {
		t.Fatalf("GetMessage: %v", err)
	}

	for _, info := range obs.snapshot() {
		if strings.ContainsAny(info.Path, "?&") {
			t.Errorf("%s Path %q carries a query string", info.Op, info.Path)
		}
		// lastMessageId is appended as a query parameter; it must not leak in
		// via Path either.
		if strings.Contains(info.Path, "lastMessageId") {
			t.Errorf("%s Path %q leaked a query parameter", info.Op, info.Path)
		}
	}
}

// TestObserver_NilIsSafe is the production configuration: no observer wired.
func TestObserver_NilIsSafe(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	c := newObservedClient(t, srv, nil)
	if _, _, err := c.ResolveRunnerGroup(context.Background(), "Default"); err != nil {
		t.Fatalf("ResolveRunnerGroup with no observer: %v", err)
	}
}

// TestRawServiceCall_AppliesClientAuthAndReturnsRawStatus covers the escape hatch the
// probe uses to compare un-modelled routes against the client's own construction: the
// call must carry the admin JWT and api-version the modelled calls carry, and must
// report a non-2xx as a status rather than as an error.
func TestRawServiceCall_AppliesClientAuthAndReturnsRawStatus(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	obs := &recordingObserver{}
	c := newObservedClient(t, srv, obs)

	ctx := context.Background()
	ss, _ := setupScaleSet(t, ctx, c)

	// A route the client does model, reached raw: it must succeed, proving the
	// auth and api-version framing match the modelled calls.
	status, body, err := c.RawServiceCall(ctx, http.MethodGet,
		"/_apis/runtime/runnerscalesets/"+strconv.Itoa(ss.ID), nil)
	if err != nil {
		t.Fatalf("RawServiceCall: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the stub rejects a request without the admin JWT)", status)
	}
	var got scaleset.RunnerScaleSet
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode raw body: %v", err)
	}
	if got.ID != ss.ID {
		t.Errorf("raw body id = %d, want %d", got.ID, ss.ID)
	}

	// A route the client does not model: the status is the finding, not an error.
	status, _, err = c.RawServiceCall(ctx, http.MethodGet,
		"/_apis/runtime/runnerscalesets/"+strconv.Itoa(ss.ID)+"/acquirablejobs", nil)
	if err != nil {
		t.Fatalf("RawServiceCall on an unmodelled route returned an error: %v", err)
	}
	if status >= 200 && status <= 299 {
		t.Logf("stub answered the unmodelled route with %d", status)
	}

	// A request body is sent as JSON.
	payload, _ := json.Marshal([]int64{9999999999})
	if _, _, err := c.RawServiceCall(ctx, http.MethodPost,
		"/_apis/runtime/runnerscalesets/"+strconv.Itoa(ss.ID)+"/acquirejobs", payload); err != nil {
		t.Fatalf("RawServiceCall with a body: %v", err)
	}

	if !containsOp(obs.ops(), "ServiceCall") {
		t.Errorf("raw calls must be observable too; saw %v", obs.ops())
	}
}

// TestRawServiceCall_PropagatesBootstrapFailure asserts the escape hatch still runs
// the auth bootstrap, so a caller cannot accidentally issue an unauthenticated call.
func TestRawServiceCall_PropagatesBootstrapFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	defer srv.Close()

	c, err := scaleset.New(scaleset.Config{
		TokenProvider: fakeProvider{},
		ConfigURL:     "https://github.com/test-org",
		APIBase:       srv.URL,
		HTTPClient:    srv.Client(),
		PollClient:    srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := c.RawServiceCall(context.Background(), http.MethodGet, "/_apis/runtime/x", nil); err == nil {
		t.Fatal("want the bootstrap failure to surface, got nil")
	}
}

func containsOp(ops []string, want string) bool {
	for _, o := range ops {
		if o == want {
			return true
		}
	}
	return false
}
