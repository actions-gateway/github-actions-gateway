package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// jitRegister calls generate-jitconfig and returns the runner ID, the decoded
// .runner agentId (sanity), and the raw response status.
func jitRegister(t *testing.T, baseURL, name string) (int64, int) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name, "runner_group_id": 1})
	resp, err := http.Post(
		baseURL+"/api/v3/repos/testorg/testrepo/actions/runners/generate-jitconfig",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("generate-jitconfig: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return 0, resp.StatusCode
	}
	var result struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode jitconfig response: %v", err)
	}

	// Sanity: the blob decodes and .runner carries the same agentId.
	blob, err := base64.StdEncoding.DecodeString(result.EncodedJITConfig)
	if err != nil {
		t.Fatalf("decode blob: %v", err)
	}
	var files map[string]string
	if err := json.Unmarshal(blob, &files); err != nil {
		t.Fatalf("unmarshal blob: %v", err)
	}
	for _, f := range []string{".runner", ".credentials", ".credentials_rsaparams"} {
		if files[f] == "" {
			t.Fatalf("blob missing %s", f)
		}
	}
	runnerFile, _ := base64.StdEncoding.DecodeString(files[".runner"])
	var runnerCfg struct {
		AgentID     int64  `json:"agentId"`
		ServerURLV2 string `json:"serverUrlV2"`
	}
	if err := json.Unmarshal(runnerFile, &runnerCfg); err != nil {
		t.Fatalf("parse .runner: %v", err)
	}
	if runnerCfg.AgentID != result.Runner.ID {
		t.Fatalf(".runner agentId %d != runner.id %d", runnerCfg.AgentID, result.Runner.ID)
	}
	if runnerCfg.ServerURLV2 == "" {
		t.Fatal(".runner serverUrlV2 empty")
	}
	return result.Runner.ID, resp.StatusCode
}

func createSession(t *testing.T, baseURL string, agentID int64, bearer string) (string, int) {
	t.Helper()
	return createSessionWithOwner(t, baseURL, agentID, fmt.Sprintf("agent-%d", agentID), bearer)
}

// createSessionWithOwner is createSession with an explicit ownerName, so a test
// can exercise the single-use simulation's owner scoping (Q135).
func createSessionWithOwner(t *testing.T, baseURL string, agentID int64, owner, bearer string) (string, int) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"ownerName": owner,
		"agent":     map[string]any{"id": agentID, "name": fmt.Sprintf("agent-%d", agentID)},
	})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/session", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode session response: %v", err)
	}
	return out.SessionID, resp.StatusCode
}

func getMessage(t *testing.T, baseURL, sessionID string) (status int, body []byte) {
	t.Helper()
	resp, err := http.Get(baseURL + "/message?sessionId=" + sessionID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TestSingleUseLifecycle exercises the full Q114 reproduction loop over HTTP:
// register → session → job delivery → acquire (consumes the runner) →
// EOF-then-401 on the dead session → 401 on a new session for the dead agent →
// 409 re-registering the surviving name of an *unconsumed* runner →
// deregister-then-register succeeding.
func TestSingleUseLifecycle(t *testing.T) {
	s := newServer()
	s.singleUse.Store(true)
	main := httptest.NewServer(s.mainMux())
	defer main.Close()
	control := httptest.NewServer(s.controlMux())
	defer control.Close()

	// Register a JIT runner and open its session.
	agentID, status := jitRegister(t, main.URL, "rg-0")
	if status != http.StatusCreated {
		t.Fatalf("register: status %d", status)
	}
	sessionID, status := createSession(t, main.URL, agentID, "bearer-a")
	if status != http.StatusOK {
		t.Fatalf("create session: status %d", status)
	}

	// Enqueue a job (control API injects runner_request_id) and receive it.
	resp, err := http.Post(control.URL+"/control/enqueue?sessionId="+sessionID,
		"application/json", strings.NewReader(`{"run_service_url":"`+main.URL+`"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("enqueue: %v status %v", err, resp)
	}
	_ = resp.Body.Close()

	status, msgBody := getMessage(t, main.URL, sessionID)
	if status != http.StatusOK {
		t.Fatalf("message poll: status %d", status)
	}
	var msg struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(msgBody, &msg); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	var jobBody struct {
		RunnerRequestID string `json:"runner_request_id"`
	}
	if err := json.Unmarshal([]byte(msg.Body), &jobBody); err != nil || jobBody.RunnerRequestID == "" {
		t.Fatalf("job body missing injected runner_request_id: %v %q", err, msg.Body)
	}

	// Acquire the job — this consumes the runner record.
	acqBody, _ := json.Marshal(map[string]string{"jobMessageId": jobBody.RunnerRequestID})
	resp, err = http.Post(main.URL+"/acquirejob", "application/json", bytes.NewReader(acqBody))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("acquirejob: %v status %v", err, resp)
	}
	_ = resp.Body.Close()

	// The dead session serves the live-observed signature: one empty 200, then 401s.
	status, body := getMessage(t, main.URL, sessionID)
	if status != http.StatusOK || len(body) != 0 {
		t.Fatalf("first dead poll: want empty 200, got %d %q", status, body)
	}
	if status, _ = getMessage(t, main.URL, sessionID); status != http.StatusUnauthorized {
		t.Fatalf("second dead poll: want 401, got %d", status)
	}

	// A new session for the consumed agent is rejected.
	if _, status = createSession(t, main.URL, agentID, "bearer-b"); status != http.StatusUnauthorized {
		t.Fatalf("session for consumed agent: want 401, got %d", status)
	}

	// The consumed runner's record is gone, so its name is free again.
	if _, status = jitRegister(t, main.URL, "rg-0"); status != http.StatusCreated {
		t.Fatalf("re-register consumed name: want 201, got %d", status)
	}

	// A *surviving* (never-consumed) record's name conflicts with 409 until the
	// record is deleted — the manual-recovery failure observed in M4 §12.
	survivorID, status := jitRegister(t, main.URL, "rg-1")
	if status != http.StatusCreated {
		t.Fatalf("register survivor: status %d", status)
	}
	if _, status = jitRegister(t, main.URL, "rg-1"); status != http.StatusConflict {
		t.Fatalf("colliding register: want 409, got %d", status)
	}

	// ResolveAgentID's list endpoint finds the survivor by name.
	resp, err = http.Get(main.URL + "/api/v3/repos/testorg/testrepo/actions/runners?name=rg-1")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("list runners: %v status %v", err, resp)
	}
	var list struct {
		Runners []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"runners"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	_ = resp.Body.Close()
	if len(list.Runners) != 1 || list.Runners[0].ID != survivorID {
		t.Fatalf("list by name: want survivor %d, got %+v", survivorID, list.Runners)
	}

	// Deregister-then-register clears the conflict.
	req, _ := http.NewRequest(http.MethodDelete,
		fmt.Sprintf("%s/api/v3/repos/testorg/testrepo/actions/runners/%d", main.URL, survivorID), nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("deregister survivor: %v status %v", err, resp)
	}
	_ = resp.Body.Close()
	if _, status = jitRegister(t, main.URL, "rg-1"); status != http.StatusCreated {
		t.Fatalf("register after deregister: want 201, got %d", status)
	}
}

// TestSingleUseDisabledKeepsSessionsAlive verifies the default mode is
// unchanged pre-Q114 behavior: acquisition does not kill the session.
func TestSingleUseDisabledKeepsSessionsAlive(t *testing.T) {
	s := newServer()
	main := httptest.NewServer(s.mainMux())
	defer main.Close()
	control := httptest.NewServer(s.controlMux())
	defer control.Close()

	sessionID, status := createSession(t, main.URL, 42, "bearer-x")
	if status != http.StatusOK {
		t.Fatalf("create session: status %d", status)
	}
	resp, err := http.Post(control.URL+"/control/enqueue?sessionId="+sessionID,
		"application/json", strings.NewReader(`{}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("enqueue: %v status %v", err, resp)
	}
	_ = resp.Body.Close()

	status, msgBody := getMessage(t, main.URL, sessionID)
	if status != http.StatusOK {
		t.Fatalf("message poll: status %d", status)
	}
	var msg struct {
		Body string `json:"body"`
	}
	_ = json.Unmarshal(msgBody, &msg)
	var jobBody struct {
		RunnerRequestID string `json:"runner_request_id"`
	}
	_ = json.Unmarshal([]byte(msg.Body), &jobBody)

	acqBody, _ := json.Marshal(map[string]string{"jobMessageId": jobBody.RunnerRequestID})
	resp, err = http.Post(main.URL+"/acquirejob", "application/json", bytes.NewReader(acqBody))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("acquirejob: %v status %v", err, resp)
	}
	_ = resp.Body.Close()

	// Session stays alive: next poll is a normal 202.
	if status, _ = getMessage(t, main.URL, sessionID); status != http.StatusAccepted {
		t.Fatalf("post-acquire poll with single-use off: want 202, got %d", status)
	}
}

// TestSingleUseRejectionIsOwnerScoped is the Q135 regression: the single-use
// simulation's session-creation 401 must honour the configured owner scope.
// fakegithub is shared across parallel e2e specs, and agent IDs are not unique
// across tenants — each AGC's StubRegistrar counts from the same base — so an
// out-of-scope tenant's freshly recycled agent can collide by ID with an
// in-scope tenant's consumed agent in the global consumedAgents map. Before the
// fix that collision 401'd a healthy session creation, killing the
// non-single-use tenant's permanent baseline and timing out
// E2E_AGC_MultipleJobsQueued.
func TestSingleUseRejectionIsOwnerScoped(t *testing.T) {
	s := newServer()
	s.singleUse.Store(true)
	s.singleUseOwnerPrefix = "scoped-"
	main := httptest.NewServer(s.mainMux())
	defer main.Close()

	// Agent id 1001 was consumed by an in-scope tenant's job acquisition.
	s.mu.Lock()
	s.consumedAgents[1001] = true
	s.mu.Unlock()

	// An out-of-scope tenant whose recycled agent collides on id 1001 must still
	// be able to open a session — its owner is outside the single-use scope.
	if _, status := createSessionWithOwner(t, main.URL, 1001, "other-0", "bearer-other"); status != http.StatusOK {
		t.Fatalf("out-of-scope session for colliding agent id: want 200, got %d", status)
	}

	// An in-scope tenant reusing a consumed agent id is still rejected, exactly
	// as real GitHub rejects a dead single-use credential.
	if _, status := createSessionWithOwner(t, main.URL, 1001, "scoped-0", "bearer-scoped"); status != http.StatusUnauthorized {
		t.Fatalf("in-scope session for consumed agent id: want 401, got %d", status)
	}
}

// enqueueJob posts a job onto a session via the control API and returns the
// HTTP status.
func enqueueJob(t *testing.T, controlURL, sessionID, jobID string) int {
	t.Helper()
	resp, err := http.Post(controlURL+"/control/enqueue?sessionId="+sessionID,
		"application/json", strings.NewReader(`{"jobId":"`+jobID+`"}`))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// deleteSessionByBearer issues the v2 DELETE /session (bearer-keyed, no
// sessionId param), the shape the broker client uses when a listener recycles.
func deleteSessionByBearer(t *testing.T, baseURL, bearer string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, baseURL+"/session", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete session: %v", err)
	}
	_ = resp.Body.Close()
}

// TestOpportunisticRedelivery verifies that a job whose session is recycled
// away before it polls is redelivered to another live session of the same
// owner, rather than being stranded — fakegithub modelling GitHub's pool-wide
// delivery so the Q114 post-job recycle does not lose jobs that race the
// recycle window.
func TestOpportunisticRedelivery(t *testing.T) {
	s := newServer() // single-use off; redelivery is independent of it
	main := httptest.NewServer(s.mainMux())
	defer main.Close()
	control := httptest.NewServer(s.controlMux())
	defer control.Close()

	// Two sessions for the same owner (same agent id → same ownerName).
	sessA, st := createSession(t, main.URL, 7, "bearer-a")
	if st != http.StatusOK {
		t.Fatalf("create session A: %d", st)
	}
	sessB, st := createSession(t, main.URL, 7, "bearer-b")
	if st != http.StatusOK {
		t.Fatalf("create session B: %d", st)
	}

	t.Run("queued job survives its session being recycled", func(t *testing.T) {
		if st := enqueueJob(t, control.URL, sessA, "job-A"); st != http.StatusOK {
			t.Fatalf("enqueue onto A: %d", st)
		}
		// A recycles before ever polling: the job must not be lost.
		deleteSessionByBearer(t, main.URL, "bearer-a")
		if status, body := getMessage(t, main.URL, sessB); status != http.StatusOK || len(body) == 0 {
			t.Fatalf("redelivery to B: want 200+body, got %d %q", status, body)
		}
	})

	t.Run("job enqueued onto an already-dead session redelivers", func(t *testing.T) {
		// sessA is already dead from the previous subtest.
		if st := enqueueJob(t, control.URL, sessA, "job-A2"); st != http.StatusOK {
			t.Fatalf("enqueue onto dead A: %d", st)
		}
		if status, body := getMessage(t, main.URL, sessB); status != http.StatusOK || len(body) == 0 {
			t.Fatalf("redelivery of dead-session enqueue to B: want 200+body, got %d %q", status, body)
		}
	})

	t.Run("a job for a live session is delivered directly, not pooled", func(t *testing.T) {
		if st := enqueueJob(t, control.URL, sessB, "job-B"); st != http.StatusOK {
			t.Fatalf("enqueue onto B: %d", st)
		}
		if status, _ := getMessage(t, main.URL, sessB); status != http.StatusOK {
			t.Fatalf("direct delivery to B: want 200, got %d", status)
		}
	})
}

// TestSessionVersionCapture verifies fakegithub records the agent.version
// (runnerVersion) from POST /session and exposes it via the control API, so a
// spec can assert the AGC sent a non-empty, correct version (the Q71/Q118
// runner-version contract). A real GitHub validates this field at session
// creation.
func TestSessionVersionCapture(t *testing.T) {
	s := newServer()
	main := httptest.NewServer(s.mainMux())
	defer main.Close()
	control := httptest.NewServer(s.controlMux())
	defer control.Close()

	const wantVersion = "2.335.1"
	body, _ := json.Marshal(map[string]any{
		"ownerName": "agent-9",
		"agent":     map[string]any{"id": 9, "name": "agent-9", "version": wantVersion},
	})
	req, _ := http.NewRequest(http.MethodPost, main.URL+"/session", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer bearer-v")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatalf("decode session response: %v", err)
	}

	vresp, err := http.Get(control.URL + "/control/session-versions")
	if err != nil {
		t.Fatalf("get session-versions: %v", err)
	}
	defer func() { _ = vresp.Body.Close() }()
	var versions map[string]string
	if err := json.NewDecoder(vresp.Body).Decode(&versions); err != nil {
		t.Fatalf("decode session-versions: %v", err)
	}

	got := versions[sess.SessionID]
	if got == "" {
		t.Fatalf("agent.version was not captured for %s (empty); versions=%v", sess.SessionID, versions)
	}
	if got != wantVersion {
		t.Fatalf("agent.version = %q, want %q", got, wantVersion)
	}
}
