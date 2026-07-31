package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesetstub"
)

// These tests drive a REAL scaleset.Client — the one the AGC's listener uses —
// against fakegithub's own mux, over HTTP. That is the part the scalesetstub unit
// tests cannot reach: the stub is proven against its own handler, but fakegithub's
// value depends on its ROUTING, and the two REST bootstrap hops are the fragile
// half. They arrive under the /api/v3 prefix, alongside the classic tier's
// same-prefix runner routes, and reach the stub only after the prefix is stripped —
// so a routing slip fails the bootstrap on a deployed cluster while every unit test
// stays green.

// stubTokenProvider is the App installation token the two-hop bootstrap starts from.
// fakegithub accepts any value.
type stubTokenProvider struct{}

func (stubTokenProvider) Token(context.Context) (string, error) { return "inst-token", nil }

// scaleSetHarness starts fakegithub's main and control listeners and returns a
// scaleset client wired the way the AGC wires one against the stub: the config URL
// keeps the org path, and the API base carries the GHES-shaped /api/v3 prefix
// (cmd/agc/internal/controller.scaleSetStubURLs).
func scaleSetHarness(t *testing.T) (*scaleset.Client, *httptest.Server, *httptest.Server) {
	t.Helper()
	s := newServer()
	main := httptest.NewServer(s.mainMux())
	t.Cleanup(main.Close)
	control := httptest.NewServer(s.controlMux())
	t.Cleanup(control.Close)
	// Release any parked long poll before the servers close; httptest.Close waits for
	// outstanding requests and would otherwise block for a whole poll window.
	t.Cleanup(s.scaleSet.Close)

	c, err := scaleset.New(scaleset.Config{
		TokenProvider: stubTokenProvider{},
		ConfigURL:     main.URL + "/e2e-org",
		APIBase:       main.URL + "/api/v3",
		HTTPClient:    main.Client(),
		PollClient:    main.Client(),
	})
	if err != nil {
		t.Fatalf("build scaleset client: %v", err)
	}
	return c, main, control
}

// controlEnqueue queues a scale-set job through the control API and returns the
// identity its assignment will carry.
func controlEnqueue(t *testing.T, controlURL, name string) scalesetstub.Job {
	t.Helper()
	resp, err := http.Post(controlURL+"/control/scaleset/enqueue?name="+name, "", nil)
	if err != nil {
		t.Fatalf("control enqueue: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("control enqueue: status %d", resp.StatusCode)
	}
	var job scalesetstub.Job
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		t.Fatalf("decode enqueued job: %v", err)
	}
	return job
}

// controlState reads the server-side view a spec asserts acquisition against.
func controlState(t *testing.T, controlURL, name string) map[string]any {
	t.Helper()
	resp, err := http.Get(controlURL + "/control/scaleset/state?name=" + name)
	if err != nil {
		t.Fatalf("control state: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("control state: status %d", resp.StatusCode)
	}
	var state map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	return state
}

// pollForMessage polls from the cursor until the queue delivers a message, then
// returns its decoded entries and the new cursor. An empty poll is a 202 the typed
// client collapses to (nil, nil). The cursor matters: polling from 0 forever
// re-reads the first undeleted message, so a caller expecting the NEXT message must
// carry it forward.
func pollForMessage(ctx context.Context, t *testing.T, c *scaleset.Client, sess *scaleset.RunnerScaleSetSession, capacity int, from int64) ([]scaleset.JobMessage, int64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := c.GetMessage(ctx, sess, capacity, from)
		if err != nil {
			t.Fatalf("GetMessage: %v", err)
		}
		if msg == nil {
			continue
		}
		var entries []scaleset.JobMessage
		if err := json.Unmarshal([]byte(msg.Body), &entries); err != nil {
			t.Fatalf("decode message body %q: %v", msg.Body, err)
		}
		return entries, msg.MessageID
	}
	t.Fatal("no message delivered before the deadline")
	return nil, 0
}

// TestScaleSetAcquisitionOverFakegithub walks the whole acquisition chain the AGC's
// scale-set listener walks — the two-hop bootstrap, scale-set registration, the
// message-queue session, a capacity-advertising poll, and the JIT config the worker
// pod is provisioned from — against fakegithub's mux.
func TestScaleSetAcquisitionOverFakegithub(t *testing.T) {
	ctx := t.Context()
	c, _, control := scaleSetHarness(t)

	// The bootstrap: registration-token, then the RemoteAuth runner-registration hop.
	// Both arrive under /api/v3, which the classic tier also owns.
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	groupID, ok, err := c.ResolveRunnerGroup(ctx, "default")
	if err != nil || !ok {
		t.Fatalf("ResolveRunnerGroup: id=%d ok=%v err=%v", groupID, ok, err)
	}
	set, err := c.CreateRunnerScaleSet(ctx, scaleset.RunnerScaleSet{
		Name: "e2e-acq", RunnerGroupID: groupID, Labels: []scaleset.Label{{Name: "e2e-acq"}},
	})
	if err != nil {
		t.Fatalf("CreateRunnerScaleSet: %v", err)
	}

	sess, err := c.CreateSession(ctx, set.ID, "ns/set")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// The session's messageQueueUrl is built from the request's Host, so a poll
	// against it only works if that derivation named a host this client can reach.
	if sess.MessageQueueURL == "" {
		t.Fatal("session carried no messageQueueUrl")
	}

	job := controlEnqueue(t, control.URL, "e2e-acq")

	entries, _ := pollForMessage(ctx, t, c, sess, 1, 0)
	if len(entries) != 1 {
		t.Fatalf("expected 1 message entry, got %d: %+v", len(entries), entries)
	}
	got := entries[0]
	if got.MessageType != scaleset.MessageTypeJobAssigned {
		t.Errorf("messageType = %q, want %q", got.MessageType, scaleset.MessageTypeJobAssigned)
	}
	// The assignment is the only place this tier learns the run identity, and the
	// worker pod's recovery annotations are stamped from it (Q417) — so assert it
	// against what the control API said would be delivered, not against a restatement.
	if got.JobID != job.JobID {
		t.Errorf("jobId = %q, want %q", got.JobID, job.JobID)
	}
	if got.OwnerName != job.OwnerName || got.RepositoryName != job.RepositoryName {
		t.Errorf("identity = %s/%s, want %s/%s",
			got.OwnerName, got.RepositoryName, job.OwnerName, job.RepositoryName)
	}
	if got.WorkflowRunID != job.WorkflowRunID {
		t.Errorf("workflowRunId = %d, want %d", got.WorkflowRunID, job.WorkflowRunID)
	}

	jit, err := c.GenerateJITConfig(ctx, set.ID, "e2e-acq-worker", "_work")
	if err != nil {
		t.Fatalf("GenerateJITConfig: %v", err)
	}
	if jit.EncodedJITConfig == "" {
		t.Error("JIT config blob was empty; the worker pod would have nothing to run")
	}

	state := controlState(t, control.URL, "e2e-acq")
	if state["activeSession"] != true {
		t.Errorf("activeSession = %v, want true", state["activeSession"])
	}
	if state["assignedJobs"] != float64(1) {
		t.Errorf("assignedJobs = %v, want 1", state["assignedJobs"])
	}

	if err := c.DeleteSession(ctx, set.ID, sess.SessionID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if state := controlState(t, control.URL, "e2e-acq"); state["activeSession"] != false {
		t.Errorf("activeSession after delete = %v, want false", state["activeSession"])
	}
}

// TestScaleSetGHESAcquireFlowOverFakegithub covers the rung auto-assign skips: with
// the GHES flow selected, a queued job is offered as JobAvailable and only becomes an
// assignment once the client claims it with acquirejobs.
func TestScaleSetGHESAcquireFlowOverFakegithub(t *testing.T) {
	ctx := t.Context()
	c, _, control := scaleSetHarness(t)

	resp, err := http.Post(control.URL+"/control/scaleset/acquireflow?ghes=true", "", nil)
	if err != nil {
		t.Fatalf("select GHES acquire flow: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("select GHES acquire flow: status %d", resp.StatusCode)
	}

	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	set, err := c.CreateRunnerScaleSet(ctx, scaleset.RunnerScaleSet{Name: "e2e-ghes", RunnerGroupID: 7})
	if err != nil {
		t.Fatalf("CreateRunnerScaleSet: %v", err)
	}
	sess, err := c.CreateSession(ctx, set.ID, "ns/set")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	job := controlEnqueue(t, control.URL, "e2e-ghes")

	offer, cursor := pollForMessage(ctx, t, c, sess, 1, 0)
	if len(offer) != 1 || offer[0].MessageType != scaleset.MessageTypeJobAvailable {
		t.Fatalf("expected one JobAvailable, got %+v", offer)
	}
	if offer[0].RunnerRequestID != job.RunnerRequestID {
		t.Errorf("offered runnerRequestId = %d, want %d", offer[0].RunnerRequestID, job.RunnerRequestID)
	}

	won, err := c.AcquireJobs(ctx, set.ID, sess, []int64{job.RunnerRequestID})
	if err != nil {
		t.Fatalf("AcquireJobs: %v", err)
	}
	if len(won) != 1 || won[0] != job.RunnerRequestID {
		t.Fatalf("claimed %v, want [%d]", won, job.RunnerRequestID)
	}

	assigned, _ := pollForMessage(ctx, t, c, sess, 1, cursor)
	if len(assigned) != 1 || assigned[0].MessageType != scaleset.MessageTypeJobAssigned {
		t.Fatalf("expected one JobAssigned after the claim, got %+v", assigned)
	}

	if state := controlState(t, control.URL, "e2e-ghes"); state["acquireJobsCalls"] != float64(1) {
		t.Errorf("acquireJobsCalls = %v, want 1", state["acquireJobsCalls"])
	}
}

// TestScaleSetControlAPIRejectsUnknownScaleSet pins the control API's own failure
// mode: a spec that mistypes the scale-set name, or asks before the AGC has
// registered one, must get a 404 rather than a silent success that reads as
// "enqueued but never delivered".
func TestScaleSetControlAPIRejectsUnknownScaleSet(t *testing.T) {
	_, _, control := scaleSetHarness(t)

	resp, err := http.Post(control.URL+"/control/scaleset/enqueue?name=nope", "", nil)
	if err != nil {
		t.Fatalf("control enqueue: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("enqueue against an unregistered scale set: status %d, want 404", resp.StatusCode)
	}

	resp, err = http.Post(control.URL+"/control/scaleset/enqueue", "", nil)
	if err != nil {
		t.Fatalf("control enqueue: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("enqueue with no name: status %d, want 400", resp.StatusCode)
	}
}
