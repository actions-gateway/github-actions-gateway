package scaleset_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fakeProvider is a githubapp.TokenProvider returning a fixed installation token.
type fakeProvider struct{}

func (fakeProvider) Token(context.Context) (string, error) { return "install-token", nil }

// countingMetrics records IncPollError/IncTokenRefresh calls by label.
type countingMetrics struct {
	mu        sync.Mutex
	pollErr   map[string]int
	refreshes map[string]int
}

func newCountingMetrics() *countingMetrics {
	return &countingMetrics{pollErr: map[string]int{}, refreshes: map[string]int{}}
}

func (m *countingMetrics) IncPollError(reason string) {
	m.mu.Lock()
	m.pollErr[reason]++
	m.mu.Unlock()
}

func (m *countingMetrics) IncTokenRefresh(kind string) {
	m.mu.Lock()
	m.refreshes[kind]++
	m.mu.Unlock()
}

func (m *countingMetrics) refresh(kind string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refreshes[kind]
}

// newClient builds a scaleset.Client wired to the stub.
func newClient(t *testing.T, srv *scalesettest.Server, metrics scaleset.MetricsRecorder) *scaleset.Client {
	t.Helper()
	c, err := scaleset.New(scaleset.Config{
		TokenProvider: fakeProvider{},
		ConfigURL:     "https://github.com/test-org",
		APIBase:       srv.URL,
		HTTPClient:    srv.HTTPClient(),
		PollClient:    srv.HTTPClient(),
		Metrics:       metrics,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// setupScaleSet drives the bootstrap + scale-set + session creation shared by most
// tests, returning the created scale set and its session.
func setupScaleSet(t *testing.T, ctx context.Context, c *scaleset.Client) (*scaleset.RunnerScaleSet, *scaleset.RunnerScaleSetSession) {
	t.Helper()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	groupID, ok, err := c.ResolveRunnerGroup(ctx, "Default")
	if err != nil || !ok {
		t.Fatalf("ResolveRunnerGroup: %v ok=%v", err, ok)
	}
	ss, err := c.CreateRunnerScaleSet(ctx, scaleset.RunnerScaleSet{
		Name:          "gag-x",
		RunnerGroupID: groupID,
		Labels:        []scaleset.Label{{Name: "gag-x", Type: "System"}},
		RunnerSetting: &scaleset.RunnerSetting{Ephemeral: true},
	})
	if err != nil {
		t.Fatalf("CreateRunnerScaleSet: %v", err)
	}
	if ss.ID == 0 {
		t.Fatal("scale set id is zero")
	}
	sess, err := c.CreateSession(ctx, ss.ID, "gag-listener")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.MessageQueueAccessToken == "" {
		t.Fatal("session has no queue token")
	}
	return ss, sess
}

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestClient_AutoAssignCapacityGating exercises the headline dotcom flow: jobs
// auto-assign strictly under the advertised X-ScaleSetMaxCapacity, re-evaluated per
// poll, with no acquire call and TotalAssignedJobs authoritative (§2b-1).
func TestClient_AutoAssignCapacityGating(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	ctx := testContext(t)
	c := newClient(t, srv, nil)
	ss, sess := setupScaleSet(t, ctx, c)

	// This test asserts the capacity-0 poll's 202 directly, so disable the stub's
	// long-poll window — otherwise that one call would park for it (Q287). Every other
	// poll here has a message waiting and returns at once either way.
	srv.SetPollTimeout(0)

	srv.EnqueueJob(ss.ID)
	srv.EnqueueJob(ss.ID)

	// Capacity 0: jobs held server-side → 202, no message.
	if msg, err := c.GetMessage(ctx, sess, 0, 0); err != nil || msg != nil {
		t.Fatalf("capacity-0 GetMessage = %v, %v; want nil, nil", msg, err)
	}

	// Capacity 1: exactly one JobAssigned.
	msg, err := c.GetMessage(ctx, sess, 1, 0)
	if err != nil || msg == nil {
		t.Fatalf("capacity-1 GetMessage = %v, %v", msg, err)
	}
	jobs, _ := msg.Jobs()
	if got := scaleset.AssignedJobs(jobs); len(got) != 1 {
		t.Fatalf("capacity-1 assigned = %d, want 1", len(got))
	}
	if msg.Statistics == nil || msg.Statistics.TotalAssignedJobs != 1 {
		t.Fatalf("capacity-1 TotalAssignedJobs = %+v, want 1", msg.Statistics)
	}
	cursor := msg.MessageID
	if deleted, err := c.DeleteMessage(ctx, sess, msg.MessageID); err != nil || !deleted {
		t.Fatalf("DeleteMessage = %t, %v; want true, nil", deleted, err)
	}

	// Capacity 2: the second job now assigns; TotalAssignedJobs climbs to 2.
	msg2, err := c.GetMessage(ctx, sess, 2, cursor)
	if err != nil || msg2 == nil {
		t.Fatalf("capacity-2 GetMessage = %v, %v", msg2, err)
	}
	jobs2, _ := msg2.Jobs()
	if got := scaleset.AssignedJobs(jobs2); len(got) != 1 {
		t.Fatalf("capacity-2 delivered assigned = %d, want 1 new", len(got))
	}
	if msg2.Statistics.TotalAssignedJobs != 2 {
		t.Fatalf("capacity-2 TotalAssignedJobs = %d, want 2", msg2.Statistics.TotalAssignedJobs)
	}

	// No acquire call on the auto-assign backend.
	if n := srv.AcquireJobsCalls(); n != 0 {
		t.Errorf("AcquireJobsCalls = %d, want 0 on auto-assign", n)
	}

	if err := c.DeleteSession(ctx, ss.ID, sess.SessionID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if err := c.DeleteRunnerScaleSet(ctx, ss.ID); err != nil {
		t.Fatalf("DeleteRunnerScaleSet: %v", err)
	}
}

// TestClient_GHESAcquireFlowAndClaimOnce exercises the GHES path: JobAvailable →
// acquire the offered ids → JobAssigned, with a second claim of the same id refused
// (claim-once, §5a-U8 / §7-P2).
func TestClient_GHESAcquireFlowAndClaimOnce(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	srv.EnableGHESAcquireFlow()
	ctx := testContext(t)
	c := newClient(t, srv, nil)
	ss, sess := setupScaleSet(t, ctx, c)

	reqID, _ := srv.EnqueueJob(ss.ID)

	// The queued job is offered as JobAvailable.
	msg, err := c.GetMessage(ctx, sess, 1, 0)
	if err != nil || msg == nil {
		t.Fatalf("GetMessage = %v, %v", msg, err)
	}
	jobs, _ := msg.Jobs()
	ids := scaleset.AvailableJobIDs(jobs)
	if len(ids) != 1 || ids[0] != reqID {
		t.Fatalf("AvailableJobIDs = %v, want [%d]", ids, reqID)
	}

	// Claim the offered id.
	won, err := c.AcquireJobs(ctx, ss.ID, sess, ids)
	if err != nil {
		t.Fatalf("AcquireJobs: %v", err)
	}
	if len(won) != 1 || won[0] != reqID {
		t.Fatalf("won = %v, want [%d]", won, reqID)
	}

	// Claim-once: acquiring the same id again wins nothing.
	won2, err := c.AcquireJobs(ctx, ss.ID, sess, ids)
	if err != nil {
		t.Fatalf("second AcquireJobs: %v", err)
	}
	if len(won2) != 0 {
		t.Fatalf("second acquire won = %v, want empty (claim-once)", won2)
	}

	// A follow-up poll now delivers JobAssigned for the claimed job.
	msg2, err := c.GetMessage(ctx, sess, 1, msg.MessageID)
	if err != nil || msg2 == nil {
		t.Fatalf("post-acquire GetMessage = %v, %v", msg2, err)
	}
	jobs2, _ := msg2.Jobs()
	if got := scaleset.AssignedJobs(jobs2); len(got) != 1 {
		t.Fatalf("post-acquire assigned = %d, want 1", len(got))
	}
	if n := srv.AcquireJobsCalls(); n != 2 {
		t.Errorf("AcquireJobsCalls = %d, want 2", n)
	}
}

// TestClient_SessionRecreateReplaysUnackedMessage confirms recovery-by-recreate:
// an unacked message replays with the same messageId to a freshly created session
// (§2b-3).
func TestClient_SessionRecreateReplaysUnackedMessage(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	ctx := testContext(t)
	c := newClient(t, srv, nil)
	ss, sess := setupScaleSet(t, ctx, c)

	_, jobID := srv.EnqueueJob(ss.ID)

	msg, err := c.GetMessage(ctx, sess, 1, 0)
	if err != nil || msg == nil {
		t.Fatalf("first GetMessage = %v, %v", msg, err)
	}
	origID := msg.MessageID
	// Deliberately do NOT ack (no DeleteMessage).

	// Drop the session and re-create it — the queue log survives.
	if err := c.DeleteSession(ctx, ss.ID, sess.SessionID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	fresh, err := c.CreateSession(ctx, ss.ID, "gag-listener")
	if err != nil {
		t.Fatalf("re-CreateSession: %v", err)
	}

	// Polling the fresh session from cursor 0 replays the unacked message.
	replay, err := c.GetMessage(ctx, fresh, 1, 0)
	if err != nil || replay == nil {
		t.Fatalf("replay GetMessage = %v, %v", replay, err)
	}
	if replay.MessageID != origID {
		t.Fatalf("replay messageId = %d, want %d (same message)", replay.MessageID, origID)
	}
	jobs, _ := replay.Jobs()
	assigned := scaleset.AssignedJobs(jobs)
	if len(assigned) != 1 || assigned[0].JobID != jobID {
		t.Fatalf("replay jobs = %v, want the same assigned job %q", jobs, jobID)
	}
}

// TestClient_AdminJWTRefreshedBeforeExpiry proves the mandatory admin-JWT lifecycle
// (§2b-7): with a short-TTL token the client re-mints the admin connection on its
// next admin call rather than presenting an expiring one.
func TestClient_AdminJWTRefreshedBeforeExpiry(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	srv.SetAdminTokenTTL(30 * time.Second) // below the client's 60s refresh lead
	ctx := testContext(t)
	metrics := newCountingMetrics()
	c := newClient(t, srv, metrics)

	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if n := srv.RunnerRegistrationCalls(); n != 1 {
		t.Fatalf("after Connect RunnerRegistrationCalls = %d, want 1", n)
	}

	// The next admin call finds the token within the refresh lead and re-mints.
	if _, _, err := c.ResolveRunnerGroup(ctx, "Default"); err != nil {
		t.Fatalf("ResolveRunnerGroup: %v", err)
	}
	if n := srv.RunnerRegistrationCalls(); n < 2 {
		t.Fatalf("RunnerRegistrationCalls = %d, want >=2 (admin JWT refreshed)", n)
	}
	if metrics.refresh("admin") < 1 {
		t.Errorf("IncTokenRefresh(admin) = %d, want >=1", metrics.refresh("admin"))
	}
}

// TestClient_QueueTokenRefreshOn401 proves the queue-token lifecycle: an expired
// queue token 401s the poll (as *UnauthorizedError), and RefreshSession restores it
// (§2b-2).
func TestClient_QueueTokenRefreshOn401(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	ctx := testContext(t)
	metrics := newCountingMetrics()
	c := newClient(t, srv, metrics)
	ss, sess := setupScaleSet(t, ctx, c)

	srv.ExpireQueueToken(ss.ID)

	_, err := c.GetMessage(ctx, sess, 1, 0)
	var unauth *scaleset.UnauthorizedError
	if !errors.As(err, &unauth) {
		t.Fatalf("expired-token GetMessage err = %v, want *UnauthorizedError", err)
	}
	if metrics.pollErr["unauthorized"] < 1 {
		t.Errorf("IncPollError(unauthorized) = %d, want >=1", metrics.pollErr["unauthorized"])
	}

	if err := c.RefreshSession(ctx, ss.ID, sess); err != nil {
		t.Fatalf("RefreshSession: %v", err)
	}
	if metrics.refresh("queue") < 1 {
		t.Errorf("IncTokenRefresh(queue) = %d, want >=1", metrics.refresh("queue"))
	}

	// The refreshed token polls successfully.
	srv.EnqueueJob(ss.ID)
	msg, err := c.GetMessage(ctx, sess, 1, 0)
	if err != nil || msg == nil {
		t.Fatalf("post-refresh GetMessage = %v, %v", msg, err)
	}
}

// TestClient_JobCompletedDelivered confirms the terminal result reaches the
// listener — the signal the classic protocol never gave the AGC (§2b-6).
func TestClient_JobCompletedDelivered(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	ctx := testContext(t)
	c := newClient(t, srv, nil)
	ss, sess := setupScaleSet(t, ctx, c)

	_, jobID := srv.EnqueueJob(ss.ID)
	msg, err := c.GetMessage(ctx, sess, 1, 0)
	if err != nil || msg == nil {
		t.Fatalf("assign GetMessage = %v, %v", msg, err)
	}
	if !srv.CompleteAssignedJob(ss.ID, jobID, "succeeded") {
		t.Fatal("CompleteAssignedJob returned false")
	}

	done, err := c.GetMessage(ctx, sess, 1, msg.MessageID)
	if err != nil || done == nil {
		t.Fatalf("completed GetMessage = %v, %v", done, err)
	}
	jobs, _ := done.Jobs()
	if len(jobs) != 1 || jobs[0].MessageType != scaleset.MessageTypeJobCompleted || jobs[0].Result != "succeeded" {
		t.Fatalf("completed jobs = %+v, want one JobCompleted result=succeeded", jobs)
	}
}

// TestClient_DeleteMessageReportsWhatTheWireDid pins the distinction Q609 asked for:
// a 404/410 completes an ack but deletes nothing, and a backend that never serves the
// endpoint answers the same way — so a caller reading only the error cannot tell a
// pruned queue from an untouched one. Each case fixes a status the stub answers and
// asserts the pair the caller sees.
func TestClient_DeleteMessageReportsWhatTheWireDid(t *testing.T) {
	cases := []struct {
		name        string
		failStatus  int
		wantDeleted bool
		wantErr     bool
	}{
		{name: "served endpoint deletes", wantDeleted: true},
		{name: "already gone", failStatus: http.StatusNotFound},
		{name: "gone", failStatus: http.StatusGone},
		{name: "momentarily unable", failStatus: http.StatusInternalServerError, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := scalesettest.New()
			defer srv.Close()
			ctx := testContext(t)
			c := newClient(t, srv, nil)
			ss, sess := setupScaleSet(t, ctx, c)

			srv.EnqueueJob(ss.ID)
			msg, err := c.GetMessage(ctx, sess, 1, 0)
			if err != nil || msg == nil {
				t.Fatalf("GetMessage = %v, %v", msg, err)
			}
			srv.FailDeleteMessage(tc.failStatus)

			deleted, err := c.DeleteMessage(ctx, sess, msg.MessageID)
			if deleted != tc.wantDeleted {
				t.Errorf("deleted = %t, want %t", deleted, tc.wantDeleted)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

// TestClient_GetMessageEmptyQueue returns nil,nil on 202.
func TestClient_GetMessageEmptyQueue(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	ctx := testContext(t)
	c := newClient(t, srv, nil)
	ss, sess := setupScaleSet(t, ctx, c)
	_ = ss
	if msg, err := c.GetMessage(ctx, sess, 5, 0); err != nil || msg != nil {
		t.Fatalf("empty-queue GetMessage = %v, %v; want nil, nil", msg, err)
	}
}

// TestClient_ScaleSetCRUDAndJIT covers the CRUD surface and per-job JIT minting.
func TestClient_ScaleSetCRUDAndJIT(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	ctx := testContext(t)
	c := newClient(t, srv, nil)
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ss, err := c.CreateRunnerScaleSet(ctx, scaleset.RunnerScaleSet{Name: "gag-y", RunnerGroupID: 7})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := c.GetRunnerScaleSet(ctx, ss.ID)
	if err != nil || got.Name != "gag-y" {
		t.Fatalf("GetRunnerScaleSet = %+v, %v", got, err)
	}
	byName, err := c.GetRunnerScaleSetByName(ctx, "gag-y")
	if err != nil || byName == nil || byName.ID != ss.ID {
		t.Fatalf("GetRunnerScaleSetByName = %+v, %v", byName, err)
	}
	if miss, err := c.GetRunnerScaleSetByName(ctx, "nope"); err != nil || miss != nil {
		t.Fatalf("GetRunnerScaleSetByName(miss) = %+v, %v; want nil, nil", miss, err)
	}
	updated, err := c.UpdateRunnerScaleSet(ctx, ss.ID, scaleset.RunnerScaleSet{Name: "gag-y2"})
	if err != nil || updated.Name != "gag-y2" {
		t.Fatalf("UpdateRunnerScaleSet = %+v, %v", updated, err)
	}

	jit, err := c.GenerateJITConfig(ctx, ss.ID, "gag-y-runner", "")
	if err != nil {
		t.Fatalf("GenerateJITConfig: %v", err)
	}
	if jit.EncodedJITConfig == "" || jit.Runner.Name != "gag-y-runner" {
		t.Fatalf("JIT config = %+v, want blob + runner name", jit)
	}

	if err := c.DeleteRunnerScaleSet(ctx, ss.ID); err != nil {
		t.Fatalf("DeleteRunnerScaleSet: %v", err)
	}
	if _, err := c.GetRunnerScaleSet(ctx, ss.ID); err == nil {
		t.Fatal("GetRunnerScaleSet after delete should error")
	} else {
		var nf *scaleset.NotFoundError
		if !errors.As(err, &nf) {
			t.Errorf("post-delete err = %v, want *NotFoundError", err)
		}
	}
}

// TestClient_CreateSessionConflict maps the one-active-session invariant to
// *SessionConflictError.
func TestClient_CreateSessionConflict(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	ctx := testContext(t)
	c := newClient(t, srv, nil)
	ss, _ := setupScaleSet(t, ctx, c)

	_, err := c.CreateSession(ctx, ss.ID, "second-listener")
	var conflict *scaleset.SessionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("second CreateSession err = %v, want *SessionConflictError", err)
	}
}

// TestClient_GenerateJITConfigRunnerNameConflict maps a generatejitconfig 409 to
// *RunnerNameConflictError — distinct from a session-create 409's *SessionConflictError,
// so the mislabel is gone (Q270).
func TestClient_GenerateJITConfigRunnerNameConflict(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	ctx := testContext(t)
	c := newClient(t, srv, nil)
	ss, _ := setupScaleSet(t, ctx, c)

	srv.FailJITConfigName("taken-name")
	_, err := c.GenerateJITConfig(ctx, ss.ID, "taken-name", "")

	var nameConflict *scaleset.RunnerNameConflictError
	if !errors.As(err, &nameConflict) {
		t.Fatalf("GenerateJITConfig err = %v, want *RunnerNameConflictError", err)
	}
	// The same 409 must NOT surface as a session conflict — that was the mislabel.
	var sessConflict *scaleset.SessionConflictError
	if errors.As(err, &sessConflict) {
		t.Fatalf("GenerateJITConfig err = %v, must not be *SessionConflictError", err)
	}
}

// TestClient_DeregisterRunnerByName covers the Q334 REST deregister path: a stale record
// is resolved by name and deleted (clearing the generatejitconfig conflict), a missing
// name is a no-op, and a busy record surfaces *RunnerBusyError without being deleted.
func TestClient_DeregisterRunnerByName(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	ctx := testContext(t)
	c := newClient(t, srv, nil)
	ss, _ := setupScaleSet(t, ctx, c)

	// No record under this name — a no-op, not an error.
	if deleted, err := c.DeregisterRunnerByName(ctx, "absent"); err != nil || deleted {
		t.Fatalf("DeregisterRunnerByName(absent) = (%v, %v); want (false, nil)", deleted, err)
	}

	// A stale record that also blocks generatejitconfig: deleting it clears the conflict,
	// so a re-register under the same name then succeeds.
	srv.FailJITConfigName("stale")
	if _, err := c.GenerateJITConfig(ctx, ss.ID, "stale", ""); err == nil {
		t.Fatalf("GenerateJITConfig(stale) before delete: want RunnerNameConflictError, got nil")
	}
	deleted, err := c.DeregisterRunnerByName(ctx, "stale")
	if err != nil || !deleted {
		t.Fatalf("DeregisterRunnerByName(stale) = (%v, %v); want (true, nil)", deleted, err)
	}
	if srv.DeleteRunnerCalls() != 1 {
		t.Fatalf("DeleteRunnerCalls = %d; want 1", srv.DeleteRunnerCalls())
	}
	if _, err := c.GenerateJITConfig(ctx, ss.ID, "stale", ""); err != nil {
		t.Fatalf("GenerateJITConfig(stale) after delete: want success, got %v", err)
	}

	// A busy record cannot be deleted — surfaced as *RunnerBusyError, left in place.
	srv.FailJITConfigName("busy")
	srv.SetRunnerBusy("busy")
	deleted, err = c.DeregisterRunnerByName(ctx, "busy")
	var busyErr *scaleset.RunnerBusyError
	if deleted || !errors.As(err, &busyErr) {
		t.Fatalf("DeregisterRunnerByName(busy) = (%v, %v); want (false, *RunnerBusyError)", deleted, err)
	}
}

// TestClient_DeregisterRunnerByNameErrors covers the REST error branches: a malformed
// ConfigURL, a non-200 list response, and a non-2xx (non-422) delete response. Since
// DeregisterRunnerByName authorizes with the installation token directly, no admin
// bootstrap is needed — the client talks straight to a raw REST stub.
func TestClient_DeregisterRunnerByNameErrors(t *testing.T) {
	ctx := testContext(t)

	newRESTClient := func(configURL, apiBase string) *scaleset.Client {
		c, err := scaleset.New(scaleset.Config{
			TokenProvider: fakeProvider{},
			ConfigURL:     configURL,
			APIBase:       apiBase,
			HTTPClient:    http.DefaultClient,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return c
	}

	// Malformed ConfigURL: no org or owner/repo path to derive the runners prefix from.
	if _, err := newRESTClient("https://github.com/", "https://api.github.com").DeregisterRunnerByName(ctx, "x"); err == nil {
		t.Fatal("DeregisterRunnerByName with a path-less ConfigURL: want error, got nil")
	}

	// List returns non-200.
	listErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer listErr.Close()
	if _, err := newRESTClient("https://github.com/org", listErr.URL).DeregisterRunnerByName(ctx, "x"); err == nil {
		t.Fatal("DeregisterRunnerByName with a 500 list response: want error, got nil")
	}

	// List resolves an id, but the DELETE fails with a non-busy status.
	delErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"total_count":1,"runners":[{"id":42,"name":"x"}]}`))
			return
		}
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer delErr.Close()
	if _, err := newRESTClient("https://github.com/org", delErr.URL).DeregisterRunnerByName(ctx, "x"); err == nil {
		t.Fatal("DeregisterRunnerByName with a 500 delete response: want error, got nil")
	}
}

// TestClient_RegistrationTokenScope covers repo-scoped registration path derivation.
func TestClient_RegistrationTokenScope(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	ctx := testContext(t)
	c, err := scaleset.New(scaleset.Config{
		TokenProvider: fakeProvider{},
		ConfigURL:     "https://github.com/test-org/test-repo",
		APIBase:       srv.URL,
		HTTPClient:    srv.HTTPClient(),
		PollClient:    srv.HTTPClient(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	var sawRepoPath bool
	for _, call := range srv.Calls() {
		if call == "registration-token /repos/test-org/test-repo/actions/runners/registration-token" {
			sawRepoPath = true
		}
	}
	if !sawRepoPath {
		t.Errorf("repo-scoped registration path not observed; calls: %v", srv.Calls())
	}
}

// TestClient_ListRunnerScaleSets covers the list-all route (Q344): the only way to
// reach a scale set whose name nobody recorded, which is exactly the state an orphan
// is in.
//
// The stub answers the unfiltered GET the way github.com was measured to answer it on
// 2026-08-24 — the full {count, value} envelope rather than a rejection — so this
// asserts the client reads that envelope, not that the service produces it.
func TestClient_ListRunnerScaleSets(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	ctx := testContext(t)
	c := newClient(t, srv, nil)
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Empty must be an empty slice and no error. A caller pruning orphans acts on
	// this result, so "none registered" has to be distinguishable from a failure.
	empty, err := c.ListRunnerScaleSets(ctx)
	if err != nil {
		t.Fatalf("ListRunnerScaleSets(empty) = _, %v; want no error", err)
	}
	if len(empty) != 0 {
		t.Fatalf("ListRunnerScaleSets(empty) = %+v; want none", empty)
	}

	first, err := c.CreateRunnerScaleSet(ctx, scaleset.RunnerScaleSet{Name: "gag-a", RunnerGroupID: 7})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := c.CreateRunnerScaleSet(ctx, scaleset.RunnerScaleSet{
		Name:          "gag-b",
		RunnerGroupID: 9,
		Labels:        []scaleset.Label{{Name: "gag-b", Type: "System"}, {Name: "gpu", Type: "System"}},
	})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	all, err := c.ListRunnerScaleSets(ctx)
	if err != nil {
		t.Fatalf("ListRunnerScaleSets: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListRunnerScaleSets = %+v; want both scale sets", all)
	}
	byID := map[int]scaleset.RunnerScaleSet{}
	for _, ss := range all {
		byID[ss.ID] = ss
	}
	if got, ok := byID[first.ID]; !ok || got.Name != "gag-a" || got.RunnerGroupID != 7 {
		t.Errorf("first scale set = %+v, present=%v", got, ok)
	}
	// The labels matter: a caller deciding whether a scale set is referenced compares
	// its labels against the RunnerSets it knows about, so a list that drops them
	// cannot answer the question it exists for.
	got, ok := byID[second.ID]
	if !ok || got.Name != "gag-b" || got.RunnerGroupID != 9 {
		t.Fatalf("second scale set = %+v, present=%v", got, ok)
	}
	if len(got.Labels) != 2 || got.Labels[0].Name != "gag-b" || got.Labels[1].Name != "gpu" {
		t.Errorf("second scale set labels = %+v; want the declared pair in order", got.Labels)
	}

	// The name filter must survive the unfiltered route sharing its handler.
	one, err := c.GetRunnerScaleSetByName(ctx, "gag-b")
	if err != nil || one == nil || one.ID != second.ID {
		t.Fatalf("GetRunnerScaleSetByName after list = %+v, %v", one, err)
	}

	// Prune: delete leaves the other listed, which is the loop an orphan sweep runs.
	if err := c.DeleteRunnerScaleSet(ctx, first.ID); err != nil {
		t.Fatalf("DeleteRunnerScaleSet: %v", err)
	}
	rest, err := c.ListRunnerScaleSets(ctx)
	if err != nil {
		t.Fatalf("ListRunnerScaleSets after delete: %v", err)
	}
	if len(rest) != 1 || rest[0].ID != second.ID {
		t.Fatalf("ListRunnerScaleSets after delete = %+v; want only the survivor", rest)
	}
}
