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
	if idx("runner-registration") < 0 || idx("create-scaleset") < 0 {
		t.Fatalf("expected bootstrap + create in call log: %v", calls)
	}
	if idx("runner-registration") > idx("create-scaleset") {
		t.Errorf("bootstrap must precede create-scaleset: %v", calls)
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
	replay, err := c.GetMessage(ctx, fresh, 1, 0)
	if err != nil {
		t.Fatalf("replay GetMessage: %v", err)
	}
	if replay != nil {
		t.Errorf("acked message must not replay, got messageId %d", replay.MessageID)
	}
}
