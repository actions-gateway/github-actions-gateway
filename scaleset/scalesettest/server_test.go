package scalesettest_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

type provider struct{}

func (provider) Token(context.Context) (string, error) { return "install-token", nil }

func newClient(t *testing.T, srv *scalesettest.Server) *scaleset.Client {
	t.Helper()
	c, err := scaleset.New(scaleset.Config{
		TokenProvider: provider{},
		ConfigURL:     "https://github.com/test-org",
		APIBase:       srv.URL,
		HTTPClient:    srv.HTTPClient(),
		PollClient:    srv.HTTPClient(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestServer_RecordsCallOrder confirms the stub records its call log so client
// tests can assert orchestration order.
func TestServer_RecordsCallOrder(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := newClient(t, srv)

	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ss, err := c.CreateRunnerScaleSet(ctx, scaleset.RunnerScaleSet{Name: "s", RunnerGroupID: 7})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := c.CreateSession(ctx, ss.ID, "owner"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	calls := srv.Calls()
	// The bootstrap always precedes the Actions Service calls.
	idx := func(want string) int {
		for i, c := range calls {
			if c == want {
				return i
			}
		}
		return -1
	}
	const created = "create-scaleset name=s group=7"
	if idx("runner-registration") < 0 || idx(created) < 0 {
		t.Fatalf("expected bootstrap + create in call log: %v", calls)
	}
	if idx("runner-registration") > idx(created) {
		t.Errorf("bootstrap must precede create-scaleset: %v", calls)
	}
}

// TestServer_DropSessionReplaysUnackedMessage confirms DropSession clears the session
// server-side (so the next poll 404s) while the queue log persists, so a re-created
// session replays the unacked message — the recovery path the scale-set listener uses
// on a session drop. AssignedJobCount reports the server-authoritative assigned count.
func TestServer_DropSessionReplaysUnackedMessage(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := newClient(t, srv)

	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ss, err := c.CreateRunnerScaleSet(ctx, scaleset.RunnerScaleSet{Name: "s", RunnerGroupID: 7})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess, err := c.CreateSession(ctx, ss.ID, "owner")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	srv.EnqueueJob(ss.ID)

	// Assign the job (capacity 1) but do NOT ack it.
	msg, err := c.GetMessage(ctx, sess, 1, 0)
	if err != nil || msg == nil {
		t.Fatalf("GetMessage = %v, %v", msg, err)
	}
	if got := srv.AssignedJobCount(ss.ID); got != 1 {
		t.Fatalf("AssignedJobCount = %d, want 1", got)
	}

	// Drop the session server-side; the next poll on the stale session must 404.
	srv.DropSession(ss.ID)
	if _, err := c.GetMessage(ctx, sess, 1, msg.MessageID); err == nil {
		t.Fatalf("poll on a dropped session must error")
	}

	// A re-created session replays the unacked message from the queue head.
	fresh, err := c.CreateSession(ctx, ss.ID, "owner")
	if err != nil {
		t.Fatalf("re-CreateSession: %v", err)
	}
	replay, err := c.GetMessage(ctx, fresh, 1, 0)
	if err != nil || replay == nil {
		t.Fatalf("unacked message must replay to a fresh session, got %v, %v", replay, err)
	}
	if replay.MessageID != msg.MessageID {
		t.Errorf("replayed messageId = %d, want %d", replay.MessageID, msg.MessageID)
	}
}

// TestServer_AckStopsReplay confirms deleting (acking) a message prevents its
// replay to a re-created session, complementing the client-side replay test.
func TestServer_AckStopsReplay(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := newClient(t, srv)

	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ss, err := c.CreateRunnerScaleSet(ctx, scaleset.RunnerScaleSet{Name: "s", RunnerGroupID: 7})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess, err := c.CreateSession(ctx, ss.ID, "owner")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	srv.EnqueueJob(ss.ID)

	msg, err := c.GetMessage(ctx, sess, 1, 0)
	if err != nil || msg == nil {
		t.Fatalf("GetMessage = %v, %v", msg, err)
	}
	// Ack it, then re-create the session and confirm no replay.
	if err := c.DeleteMessage(ctx, sess, msg.MessageID); err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}
	if err := c.DeleteSession(ctx, ss.ID, sess.SessionID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	fresh, err := c.CreateSession(ctx, ss.ID, "owner")
	if err != nil {
		t.Fatalf("re-CreateSession: %v", err)
	}
	// The replay poll finds an empty queue, which is the point — skip the long-poll
	// window rather than waiting it out for a 202 (Q287).
	srv.SetPollTimeout(0)
	replay, err := c.GetMessage(ctx, fresh, 1, 0)
	if err != nil {
		t.Fatalf("replay GetMessage: %v", err)
	}
	if replay != nil {
		t.Errorf("acked message must not replay, got messageId %d", replay.MessageID)
	}
}

// TestServer_PollHoldsEmptyQueueUntilDeadline is the Q287 contract: a poll with nothing
// to deliver is held for the poll window before answering 202, rather than returning
// instantly and letting a looping client spin at thousands of requests per second.
func TestServer_PollHoldsEmptyQueueUntilDeadline(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	const window = 200 * time.Millisecond
	srv.SetPollTimeout(window)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := newClient(t, srv)
	_, sess := setupSession(t, ctx, c)

	start := time.Now()
	msg, err := c.GetMessage(ctx, sess, 1, 0)
	elapsed := time.Since(start)
	if err != nil || msg != nil {
		t.Fatalf("empty-queue GetMessage = %v, %v; want nil, nil (202)", msg, err)
	}
	// Allow for timer coarseness on a loaded box, but the poll must have actually blocked.
	if elapsed < window/2 {
		t.Errorf("empty poll returned after %v, want it held ~%v — the stub is not long-polling", elapsed, window)
	}
}

// TestServer_PollWakesOnEnqueue proves the long poll does not cost latency: a poll parked
// on an empty queue returns as soon as a job is enqueued, well inside the poll window.
// This is what keeps the suite fast while the window itself stays long enough to stop a
// listener hot-looping.
func TestServer_PollWakesOnEnqueue(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	srv.SetPollTimeout(30 * time.Second) // far longer than the test may take

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := newClient(t, srv)
	ss, sess := setupSession(t, ctx, c)

	type result struct {
		msg     *scaleset.RunnerScaleSetMessage
		err     error
		elapsed time.Duration
	}
	res := make(chan result, 1)
	start := time.Now()
	go func() {
		msg, err := c.GetMessage(ctx, sess, 1, 0)
		res <- result{msg, err, time.Since(start)}
	}()

	// Let the poll park, then land a job on the queue.
	time.Sleep(50 * time.Millisecond)
	srv.EnqueueJob(ss.ID)

	select {
	case r := <-res:
		if r.err != nil || r.msg == nil {
			t.Fatalf("parked GetMessage = %v, %v; want the enqueued job", r.msg, r.err)
		}
		if r.elapsed > 5*time.Second {
			t.Errorf("parked poll took %v to see the enqueued job; want a prompt wake", r.elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a poll parked on an empty queue never woke for the enqueued job")
	}
}

// TestServer_CloseReleasesParkedPoll confirms Close does not hang behind a long poll:
// httptest.Server.Close waits for outstanding requests, so the stub must release its
// parked polls first.
func TestServer_CloseReleasesParkedPoll(t *testing.T) {
	srv := scalesettest.New()
	srv.SetPollTimeout(time.Hour) // only Close can end this poll

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := newClient(t, srv)
	_, sess := setupSession(t, ctx, c)

	polling := make(chan struct{})
	go func() {
		defer close(polling)
		_, _ = c.GetMessage(ctx, sess, 1, 0)
	}()
	time.Sleep(50 * time.Millisecond) // let the poll park

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		srv.Close()
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung behind a parked long poll")
	}
	<-polling
}

// setupSession registers a scale set on the stub and opens its message-queue session.
func setupSession(t *testing.T, ctx context.Context, c *scaleset.Client) (scaleset.RunnerScaleSet, *scaleset.RunnerScaleSetSession) {
	t.Helper()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ss, err := c.CreateRunnerScaleSet(ctx, scaleset.RunnerScaleSet{Name: "s", RunnerGroupID: 7})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess, err := c.CreateSession(ctx, ss.ID, "owner")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return *ss, sess
}
