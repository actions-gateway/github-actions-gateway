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
	"time"
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

// setRedelivery toggles the Q154 lease/redelivery model via the control API.
func setRedelivery(t *testing.T, controlURL, query string) {
	t.Helper()
	resp, err := http.Post(controlURL+"/control/redelivery?"+query, "", nil)
	if err != nil {
		t.Fatalf("set redelivery: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set redelivery: status %d", resp.StatusCode)
	}
}

// enqueueReq enqueues a job with an explicit runner_request_id so the test can
// address it by request id in /control/jobstats.
func enqueueReq(t *testing.T, controlURL, sessionID, reqID string) int {
	t.Helper()
	body := fmt.Sprintf(`{"runner_request_id":%q,"run_service_url":"http://x"}`, reqID)
	resp, err := http.Post(controlURL+"/control/enqueue?sessionId="+sessionID,
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("enqueue %s: %v", reqID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// acquire posts /acquirejob for a runner_request_id.
func acquire(t *testing.T, baseURL, reqID string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"jobMessageId": reqID})
	resp, err := http.Post(baseURL+"/acquirejob", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("acquire %s: %v", reqID, err)
	}
	_ = resp.Body.Close()
}

type jobStats struct {
	Deliveries int  `json:"deliveries"`
	Leased     bool `json:"leased"`
	Acquired   bool `json:"acquired"`
}

func getJobStats(t *testing.T, controlURL, reqID string) jobStats {
	t.Helper()
	resp, err := http.Get(controlURL + "/control/jobstats?requestId=" + reqID)
	if err != nil {
		t.Fatalf("jobstats %s: %v", reqID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var js jobStats
	if err := json.NewDecoder(resp.Body).Decode(&js); err != nil {
		t.Fatalf("decode jobstats: %v", err)
	}
	return js
}

// TestRedeliveryLeaseModel is the Q154 fidelity test: with the lease model on,
// an *acquired* job is consumed and never redelivered (the GitHub contract the
// Q59 admission gate assumes — a ceiling-held acquired job is cancelled, not
// handed back), while a *delivered-but-not-acquired* job is redelivered once its
// lease expires (so a job the gate skips for lack of capacity is not lost).
func TestRedeliveryLeaseModel(t *testing.T) {
	s := newServer() // single-use off; redelivery is independent of it
	main := httptest.NewServer(s.mainMux())
	defer main.Close()
	control := httptest.NewServer(s.controlMux())
	defer control.Close()

	setRedelivery(t, control.URL, "enabled=true&leaseMs=120")

	sess, st := createSession(t, main.URL, 11, "bearer-r")
	if st != http.StatusOK {
		t.Fatalf("create session: %d", st)
	}

	t.Run("acquired job is not redelivered", func(t *testing.T) {
		if st := enqueueReq(t, control.URL, sess, "req-acq"); st != http.StatusOK {
			t.Fatalf("enqueue: %d", st)
		}
		if status, body := getMessage(t, main.URL, sess); status != http.StatusOK || len(body) == 0 {
			t.Fatalf("delivery: want 200+body, got %d %q", status, body)
		}
		acquire(t, main.URL, "req-acq")

		js := getJobStats(t, control.URL, "req-acq")
		if !js.Acquired || js.Leased || js.Deliveries != 1 {
			t.Fatalf("post-acquire stats = %+v, want {Deliveries:1 Leased:false Acquired:true}", js)
		}
		// Poll well past several lease windows: an acquired job must never come
		// back, so deliveries stays at 1.
		for i := 0; i < 6; i++ {
			if status, _ := getMessage(t, main.URL, sess); status != http.StatusAccepted {
				t.Fatalf("poll %d after acquire: want 202 (no redelivery), got %d", i, status)
			}
			time.Sleep(40 * time.Millisecond)
		}
		if js := getJobStats(t, control.URL, "req-acq"); js.Deliveries != 1 {
			t.Fatalf("acquired job was redelivered: deliveries=%d, want 1", js.Deliveries)
		}
	})

	t.Run("skipped job is redelivered after its lease expires", func(t *testing.T) {
		if st := enqueueReq(t, control.URL, sess, "req-skip"); st != http.StatusOK {
			t.Fatalf("enqueue: %d", st)
		}
		// Deliver once, then *skip* it (no acquire), as the admission gate does
		// when it is full.
		if status, body := getMessage(t, main.URL, sess); status != http.StatusOK || len(body) == 0 {
			t.Fatalf("first delivery: want 200+body, got %d %q", status, body)
		}
		if js := getJobStats(t, control.URL, "req-skip"); js.Deliveries != 1 || !js.Leased || js.Acquired {
			t.Fatalf("after skip stats = %+v, want {Deliveries:1 Leased:true Acquired:false}", js)
		}
		// Within the lease window the job stays invisible.
		if status, _ := getMessage(t, main.URL, sess); status != http.StatusAccepted {
			t.Fatalf("poll within lease: want 202, got %d", status)
		}
		// After the lease expires, the next poll redelivers it.
		var redelivered bool
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(40 * time.Millisecond)
			if status, _ := getMessage(t, main.URL, sess); status == http.StatusOK {
				redelivered = true
				break
			}
		}
		if !redelivered {
			t.Fatal("skipped job was never redelivered after its lease expired")
		}
		if js := getJobStats(t, control.URL, "req-skip"); js.Deliveries < 2 || js.Acquired {
			t.Fatalf("redelivered stats = %+v, want Deliveries>=2 Acquired:false", js)
		}
	})

	t.Run("out-of-scope owner keeps the immediate-dequeue model", func(t *testing.T) {
		// Scope redelivery to a prefix this session's owner ("agent-11") lacks.
		setRedelivery(t, control.URL, "enabled=true&owner=scoped-&leaseMs=120")
		if st := enqueueReq(t, control.URL, sess, "req-oos"); st != http.StatusOK {
			t.Fatalf("enqueue: %d", st)
		}
		if status, _ := getMessage(t, main.URL, sess); status != http.StatusOK {
			t.Fatalf("delivery: want 200, got %d", status)
		}
		// Out of scope: no lease is tracked, so jobstats stays empty and a second
		// poll returns 202 (the job was dequeued immediately, not leased).
		if js := getJobStats(t, control.URL, "req-oos"); js.Deliveries != 0 || js.Leased {
			t.Fatalf("out-of-scope stats = %+v, want untracked", js)
		}
		if status, _ := getMessage(t, main.URL, sess); status != http.StatusAccepted {
			t.Fatalf("second poll out of scope: want 202, got %d", status)
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

// TestLongPollHoldsIdlePollAndDeliversPromptly verifies the broker long-poll
// (Q148): an empty GET /message holds the connection for ~longPollHold instead
// of returning 202 at network speed, while a job enqueued mid-hold is delivered
// promptly rather than after the full hold. The fast spin is what let a
// replacement listener idle-shut-down within milliseconds and collapse the pool.
func TestLongPollHoldsIdlePollAndDeliversPromptly(t *testing.T) {
	s := newServer()
	s.longPollHold = 400 * time.Millisecond
	main := httptest.NewServer(s.mainMux())
	defer main.Close()
	control := httptest.NewServer(s.controlMux())
	defer control.Close()

	sessID, st := createSession(t, main.URL, 5, "bearer-lp")
	if st != http.StatusOK {
		t.Fatalf("create session: %d", st)
	}

	// 1. An idle poll holds for ~longPollHold before answering 202, rather than
	// returning immediately (which is what flaked the self-heal pool).
	start := time.Now()
	status, _ := getMessage(t, main.URL, sessID)
	held := time.Since(start)
	if status != http.StatusAccepted {
		t.Fatalf("idle poll: status %d, want 202", status)
	}
	if held < s.longPollHold/2 {
		t.Fatalf("idle poll returned after %v; expected to hold ~%v", held, s.longPollHold)
	}

	// 2. A job enqueued while a poll is in flight is delivered promptly — well
	// before the full hold elapses — and the poll returns 200 with the job body.
	type result struct {
		status int
		body   []byte
		took   time.Duration
	}
	resCh := make(chan result, 1)
	go func() {
		t0 := time.Now()
		st, body := getMessage(t, main.URL, sessID)
		resCh <- result{st, body, time.Since(t0)}
	}()
	// Let the poll begin its hold, then enqueue.
	time.Sleep(longPollTick * 2)
	if st := enqueueJob(t, control.URL, sessID, "lp-job"); st != http.StatusOK {
		t.Fatalf("enqueue: %d", st)
	}

	select {
	case r := <-resCh:
		if r.status != http.StatusOK {
			t.Fatalf("job poll: status %d, want 200", r.status)
		}
		if !strings.Contains(string(r.body), "lp-job") {
			t.Fatalf("job poll body = %q, want it to contain the job id", r.body)
		}
		if r.took >= s.longPollHold {
			t.Fatalf("job delivered after %v; expected prompt delivery well under the %v hold", r.took, s.longPollHold)
		}
	case <-time.After(s.longPollHold + 2*time.Second):
		t.Fatal("job poll did not return after enqueue")
	}
}

// TestStrandedSessionQueueAgesIntoPool is the Q436 regression: a job enqueued
// onto a session that never polls again must still reach the owner's pool, even
// though that session is never deleted.
//
// The e2e failure this reproduces: the AGC finished a job, recycled its
// single-use agent, and its best-effort DeleteSession timed out three times.
// The session stayed registered (so nothing requeued its jobs) with nobody
// polling it (the listener had moved to a fresh session), and the job a spec
// had enqueued onto it moments earlier was unreachable for the rest of the run
// — a liveness failure no Eventually budget can wait out. Ageing an undelivered
// job into the owner pool restores the invariant the real broker has by
// construction: an enqueued job is always reachable by some session.
func TestStrandedSessionQueueAgesIntoPool(t *testing.T) {
	// Both sessions carry the same ownerName (createSession derives it from the
	// agent id), as an AGC's recycled sessions do — ownerName is
	// "<group>-<agentIndex>" and the index survives a recycle.
	newPair := func(t *testing.T, grace time.Duration) (s *server, mainURL, controlURL, sessA, sessB string) {
		t.Helper()
		s = newServer()
		s.sessionQueueGrace = grace
		main := httptest.NewServer(s.mainMux())
		t.Cleanup(main.Close)
		control := httptest.NewServer(s.controlMux())
		t.Cleanup(control.Close)

		var st int
		if sessA, st = createSession(t, main.URL, 9, "bearer-a"); st != http.StatusOK {
			t.Fatalf("create session A: %d", st)
		}
		if sessB, st = createSession(t, main.URL, 9, "bearer-b"); st != http.StatusOK {
			t.Fatalf("create session B: %d", st)
		}
		return s, main.URL, control.URL, sessA, sessB
	}

	t.Run("a job on a session nobody polls ages into the owner pool", func(t *testing.T) {
		// Grace of one nanosecond: any elapsed time is "stale", so the sweep is
		// asserted without a sleep. A never polls and is never deleted — exactly
		// the leaked-session state a failed DeleteSession leaves behind.
		_, mainURL, controlURL, sessA, sessB := newPair(t, time.Nanosecond)
		if st := enqueueJob(t, controlURL, sessA, "stranded-job"); st != http.StatusOK {
			t.Fatalf("enqueue onto A: %d", st)
		}

		status, body := getMessage(t, mainURL, sessB)
		if status != http.StatusOK || !strings.Contains(string(body), "stranded-job") {
			t.Fatalf("poll on B: got %d %q, want 200 carrying the stranded job", status, body)
		}

		// Delivered exactly once: neither B nor the still-registered A sees it again.
		if status, _ := getMessage(t, mainURL, sessB); status != http.StatusAccepted {
			t.Fatalf("second poll on B: got %d, want 202 (job already delivered)", status)
		}
		if status, _ := getMessage(t, mainURL, sessA); status != http.StatusAccepted {
			t.Fatalf("poll on A after the sweep: got %d, want 202 (job moved to the pool)", status)
		}
	})

	t.Run("a job on a polling session is not diverted", func(t *testing.T) {
		// With a grace no test can outrun, the targeted delivery the single-use
		// specs rely on is unaffected: B does not steal A's job, and A gets it.
		_, mainURL, controlURL, sessA, sessB := newPair(t, time.Hour)
		if st := enqueueJob(t, controlURL, sessA, "targeted-job"); st != http.StatusOK {
			t.Fatalf("enqueue onto A: %d", st)
		}

		if status, _ := getMessage(t, mainURL, sessB); status != http.StatusAccepted {
			t.Fatalf("poll on B: got %d, want 202 (A's job must not be diverted)", status)
		}
		status, body := getMessage(t, mainURL, sessA)
		if status != http.StatusOK || !strings.Contains(string(body), "targeted-job") {
			t.Fatalf("poll on A: got %d %q, want 200 carrying its own job", status, body)
		}
	})

	t.Run("the sweep is owner-scoped", func(t *testing.T) {
		// fakegithub is shared across parallel specs and tenants: one owner's poll
		// must never move another owner's work, however stale.
		s, mainURL, controlURL, sessA, _ := newPair(t, time.Nanosecond)
		other, st := createSessionWithOwner(t, mainURL, 11, "other-owner-0", "bearer-o")
		if st != http.StatusOK {
			t.Fatalf("create other-owner session: %d", st)
		}
		if st := enqueueJob(t, controlURL, sessA, "owner-a-job"); st != http.StatusOK {
			t.Fatalf("enqueue onto A: %d", st)
		}

		if status, _ := getMessage(t, mainURL, other); status != http.StatusAccepted {
			t.Fatalf("poll on the other owner's session: got %d, want 202", status)
		}
		s.mu.Lock()
		queued := len(s.jobQueues[sessA])
		s.mu.Unlock()
		if queued != 1 {
			t.Fatalf("A's queue holds %d jobs after a foreign poll, want 1 (untouched)", queued)
		}
	})
}

// TestRerunFailedJobsIsRecordedAndScoped covers the eviction auto-retry
// observability the Q421 drain experiment reads (see the package doc): the call
// is answered like GitHub answers it, recorded, and reportable per run — the last
// part being what lets one spec assert "no rerun for MY run" while another spec's
// rerun sits in the same list.
//
// Both routes are exercised because which one the AGC uses depends on its
// configured API base: a GHES-shaped base carries the /api/v3 prefix, while the
// e2e deployment points GITHUB_API_BASE_URL straight at this server and addresses
// /repos/... directly.
func TestRerunFailedJobsIsRecordedAndScoped(t *testing.T) {
	s := newServer()
	main := httptest.NewServer(s.mainMux())
	defer main.Close()
	control := httptest.NewServer(s.controlMux())
	defer control.Close()

	rerun := func(path string) int {
		t.Helper()
		resp, err := http.Post(main.URL+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}
	reruns := func(filter string) (int, []string) {
		t.Helper()
		query := ""
		if filter != "" {
			query = "?run=" + filter
		}
		resp, err := http.Get(control.URL + "/control/reruns" + query)
		if err != nil {
			t.Fatalf("GET /control/reruns: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out struct {
			Count int      `json:"count"`
			Paths []string `json:"paths"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode reruns: %v", err)
		}
		return out.Count, out.Paths
	}

	if n, _ := reruns(""); n != 0 {
		t.Fatalf("a fresh server reports %d reruns, want 0", n)
	}

	// The shape the e2e AGC sends (API base is this server's root).
	if code := rerun("/repos/o/r/actions/runs/4210/rerun-failed-jobs"); code != http.StatusCreated {
		t.Fatalf("rerun-failed-jobs returned %d, want 201 (GitHub's own status)", code)
	}
	// The GHES-prefixed shape.
	if code := rerun("/api/v3/repos/o/r/actions/runs/999/rerun-failed-jobs"); code != http.StatusCreated {
		t.Fatalf("/api/v3 rerun-failed-jobs returned %d, want 201", code)
	}

	if n, paths := reruns(""); n != 2 {
		t.Fatalf("server recorded %d reruns (%v), want both routes", n, paths)
	}
	n, paths := reruns("/runs/4210/")
	if n != 1 {
		t.Fatalf("run-scoped filter matched %d reruns (%v), want exactly the 4210 call", n, paths)
	}
	if !strings.Contains(paths[0], "/runs/4210/") {
		t.Fatalf("filtered path %q is not the run that was asked for", paths[0])
	}
	if n, _ := reruns("/runs/12345/"); n != 0 {
		t.Fatalf("a run nobody re-ran reports %d reruns, want 0 — the Q421 assertion depends on this", n)
	}

	// An unimplemented /repos endpoint must still 404 rather than be silently
	// counted as a rerun. The run itself is served since Q811, so this probes a
	// sub-resource that is not: the point is that an unserved path answers 404,
	// not that this particular one is unserved.
	resp, err := http.Get(main.URL + "/repos/o/r/actions/runs/4210/jobs")
	if err != nil {
		t.Fatalf("GET unimplemented repos endpoint: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unimplemented /repos endpoint returned %d, want 404", resp.StatusCode)
	}
}

// TestRerunFailedJobsRefusedUntilRunConcludes covers the run-conclusion gate
// (Q517): real GitHub refuses rerun-failed-jobs with 403 "This workflow is
// already running" until it concludes the original run — the refusal the AGC's
// Q503 retry loop keys on — so a spec must get both branches from the fake:
// refused while its run is marked non-concluded, accepted once concluded, with
// the two recorded separately so an accepted-count assertion is not inflated by
// the refusals.
func TestRerunFailedJobsRefusedUntilRunConcludes(t *testing.T) {
	s := newServer()
	main := httptest.NewServer(s.mainMux())
	defer main.Close()
	control := httptest.NewServer(s.controlMux())
	defer control.Close()

	const path = "/repos/o/r/actions/runs/5030/rerun-failed-jobs"

	setConcluded := func(concluded bool) {
		t.Helper()
		resp, err := http.Post(fmt.Sprintf("%s/control/runstate?run=5030&concluded=%t", control.URL, concluded), "", nil)
		if err != nil {
			t.Fatalf("POST /control/runstate: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("/control/runstate returned %d, want 200", resp.StatusCode)
		}
	}
	rerun := func(p string) (int, []byte) {
		t.Helper()
		resp, err := http.Post(main.URL+p, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("POST %s: %v", p, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}
	reruns := func() (accepted, refused int) {
		t.Helper()
		resp, err := http.Get(control.URL + "/control/reruns?run=/runs/5030/")
		if err != nil {
			t.Fatalf("GET /control/reruns: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out struct {
			Count        int `json:"count"`
			RefusedCount int `json:"refusedCount"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode reruns: %v", err)
		}
		return out.Count, out.RefusedCount
	}

	setConcluded(false)

	// The refusal window: repeated attempts, as the AGC's paced retry loop makes
	// them, are all refused with the measured shape.
	for i := 0; i < 2; i++ {
		status, body := rerun(path)
		if status != http.StatusForbidden {
			t.Fatalf("rerun on a non-concluded run returned %d, want 403", status)
		}
		var errBody struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		}
		if err := json.Unmarshal(body, &errBody); err != nil {
			t.Fatalf("refusal body is not JSON: %v (%s)", err, body)
		}
		// The exact message matters: the AGC discriminates the retryable refusal
		// from a terminal 403 (permissions) by "already running" in the body, so a
		// drifted message would make it give up instead of retry.
		if errBody.Message != "This workflow is already running" {
			t.Fatalf("refusal message %q does not match real GitHub's", errBody.Message)
		}
		if errBody.Status != "403" {
			t.Fatalf("refusal status field %q, want %q", errBody.Status, "403")
		}
	}
	if accepted, refused := reruns(); accepted != 0 || refused != 2 {
		t.Fatalf("after two refused attempts: accepted=%d refused=%d, want 0/2", accepted, refused)
	}

	// The gate is per run: an unmarked run is still accepted while 5030 refuses.
	if status, _ := rerun("/repos/o/r/actions/runs/9999/rerun-failed-jobs"); status != http.StatusCreated {
		t.Fatalf("rerun on an unmarked run returned %d, want 201", status)
	}

	// Conclusion flips the answer to 201 — the branch where the AGC's retry lands.
	setConcluded(true)
	if status, _ := rerun(path); status != http.StatusCreated {
		t.Fatalf("rerun on a concluded run returned %d, want 201", status)
	}
	if accepted, refused := reruns(); accepted != 1 || refused != 2 {
		t.Fatalf("after conclusion: accepted=%d refused=%d, want 1/2", accepted, refused)
	}
}

// TestRunStatusReadTracksRunState pins the run read the deletion arm's cancel check
// makes before it asks for a re-run (Q811). The endpoint did not exist when that
// check shipped, so the AGC's GET 404'd, the check read the run as unanswerable, and
// the drained-worker recovery never made its re-run call: the e2e spec that asserts
// the re-run failed for a reason no unit tier could see.
//
// It answers off the same /control/runstate the refusal keys on, so the read and the
// POST cannot disagree about one run.
func TestRunStatusReadTracksRunState(t *testing.T) {
	s := newServer()
	main := httptest.NewServer(s.mainMux())
	defer main.Close()
	control := httptest.NewServer(s.controlMux())
	defer control.Close()

	readRun := func(runID string) (int, string, any) {
		t.Helper()
		resp, err := http.Get(main.URL + "/repos/o/r/actions/runs/" + runID)
		if err != nil {
			t.Fatalf("read run %s: %v", runID, err)
		}
		defer func() { _ = resp.Body.Close() }()
		var body struct {
			Status     string `json:"status"`
			Conclusion any    `json:"conclusion"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Status, body.Conclusion
	}
	setState := func(runID, query string) {
		t.Helper()
		resp, err := http.Post(control.URL+"/control/runstate?run="+runID+"&"+query, "", nil)
		if err != nil {
			t.Fatalf("set run state: %v", err)
		}
		_ = resp.Body.Close()
	}

	// An unmarked run is the disrupted shape a recovery must be allowed to re-run:
	// completed/failure, which live GitHub reaches for a drained worker (Q459).
	if status, s, c := readRun("4242"); status != http.StatusOK || s != "completed" || c != "failure" {
		t.Fatalf("unmarked run read %d %q/%v, want 200 completed/failure", status, s, c)
	}

	// Marked not-yet-concluded, the read agrees with the 403 the POST gives (Q517).
	setState("4242", "concluded=false")
	if status, s, c := readRun("4242"); status != http.StatusOK || s != "in_progress" || c != nil {
		t.Fatalf("non-concluded run read %d %q/%v, want 200 in_progress/null", status, s, c)
	}

	// Cancelled is the state that stands the recovery down rather than re-running.
	setState("4242", "concluded=true&conclusion=cancelled")
	if status, s, c := readRun("4242"); status != http.StatusOK || s != "completed" || c != "cancelled" {
		t.Fatalf("cancelled run read %d %q/%v, want 200 completed/cancelled", status, s, c)
	}

	// The POST is deliberately still accepted for it: real GitHub accepts a re-run of
	// a cancelled run (measured 2026-08-05), so a spec asserting the AGC does not ask
	// is asserting about the AGC, not about a fake that refuses on its behalf.
	resp, err := http.Post(main.URL+"/repos/o/r/actions/runs/4242/rerun-failed-jobs", "", nil)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("rerun of a cancelled run returned %d, want 201", resp.StatusCode)
	}

	// A sub-resource path is not the run read: it must keep routing to its own handler.
	if status, _, _ := readRun("4242/rerun-failed-jobs"); status != http.StatusNotFound {
		t.Fatalf("GET on a sub-resource returned %d, want 404", status)
	}
}
