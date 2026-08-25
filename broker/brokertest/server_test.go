package brokertest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/broker"
)

// postSession POSTs POST /session with the given owner and bearer token and
// returns the session ID (empty on non-200) and the HTTP status.
func postSession(t *testing.T, baseURL, owner, bearer string) (string, int) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"ownerName": owner,
		"agent":     map[string]any{"id": 1, "name": owner},
	})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"session", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post session: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	return out.SessionID, resp.StatusCode
}

// TestServer_SessionLifecycle drives the broker stub through a full session →
// job → acquire flow, exercising the POST/DELETE /session, /message, and
// /acquirejob handlers plus the session-introspection helpers.
func TestServer_SessionLifecycle(t *testing.T) {
	s := New()
	defer s.Close()

	sessionID, status := postSession(t, s.URL, "lifecycle-0", "bearer-a")
	if status != http.StatusOK || sessionID == "" {
		t.Fatalf("create session: status %d id %q", status, sessionID)
	}

	if got := s.RegisteredSessions(); len(got) != 1 || got[0] != sessionID {
		t.Fatalf("RegisteredSessions = %v, want [%s]", got, sessionID)
	}
	if got := s.ActiveSessionsForOwner("lifecycle"); len(got) != 1 {
		t.Fatalf("ActiveSessionsForOwner(lifecycle) = %v, want one session", got)
	}
	if got := s.ActiveSessionsForOwner("other"); len(got) != 0 {
		t.Fatalf("ActiveSessionsForOwner(other) = %v, want none", got)
	}
	if n := s.ActiveSessionCount(); n != 1 {
		t.Fatalf("ActiveSessionCount = %d, want 1", n)
	}

	// Enqueue a job and receive it via GET /message.
	s.EnqueueJob(sessionID, broker.RunnerJobRequestBody{RunServiceURL: s.URL})
	resp, err := http.Get(s.URL + "message?sessionId=" + sessionID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("message poll: status %d", resp.StatusCode)
	}
	if !s.WaitForFirstPoll(sessionID, time.Second) {
		t.Fatal("WaitForFirstPoll did not observe the poll")
	}
	var msg struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(bodyBytes, &msg); err != nil {
		t.Fatalf("decode message: %v", err)
	}

	// Acquire the job.
	acqBody, _ := json.Marshal(map[string]string{"jobMessageId": "req-1"})
	resp, err = http.Post(s.URL+"acquirejob", "application/json", bytes.NewReader(acqBody))
	if err != nil {
		t.Fatalf("acquirejob: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("acquirejob: status %d", resp.StatusCode)
	}
	if s.AcquireJobCalls() == 0 {
		t.Fatal("AcquireJobCalls should be > 0")
	}

	// DELETE /session via the v2 bearer-keyed path the broker client uses.
	req, _ := http.NewRequest(http.MethodDelete, s.URL+"session", nil)
	req.Header.Set("Authorization", "Bearer bearer-a")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete session: %v", err)
	}
	_ = resp.Body.Close()
	if !s.WaitForSessionDelete(sessionID, time.Second) {
		t.Fatal("session should be marked deleted")
	}
	if got := s.RegisteredSessions(); len(got) != 0 {
		t.Fatalf("RegisteredSessions after delete = %v, want none", got)
	}
}

// TestServer_ActiveSessionsForOwnerScoping pins what the owner filter separates.
// Name: a stem that extends another's owns its own bucket, which is what lets two
// CRs in one namespace be asserted on independently. Kind: separable since Q677 put
// the registered runner name on the wire, so a RunnerSet owns "rs-<set>-<index>"
// while its same-named RunnerGroup owns "<set>-<index>".
//
// The kind half is this file's regression cover for Q677 and was written to fail
// before it: until the listener sent the registered name, both kinds emitted a
// byte-identical owner and shared one bucket with nothing to read them apart.
func TestServer_ActiveSessionsForOwnerScoping(t *testing.T) {
	s := New()
	defer s.Close()

	base, _ := postSession(t, s.URL, "tenant-0", "bearer-base")
	ext, _ := postSession(t, s.URL, "tenant-set-0", "bearer-ext")

	if got := s.ActiveSessionsForOwner("tenant"); len(got) != 1 || got[0] != base {
		t.Fatalf("ActiveSessionsForOwner(tenant) = %v, want [%s]: a name-extending CR must keep its own bucket", got, base)
	}
	if got := s.ActiveSessionsForOwner("tenant-set"); len(got) != 1 || got[0] != ext {
		t.Fatalf("ActiveSessionsForOwner(tenant-set) = %v, want [%s]", got, ext)
	}

	// A RunnerSet named "tenant" at index 0 now owns "rs-tenant-0", so it lands in
	// its own bucket and leaves the same-named RunnerGroup's alone.
	rs, _ := postSession(t, s.URL, "rs-tenant-0", "bearer-rs")
	if got := s.ActiveSessionsForOwner("rs-tenant"); len(got) != 1 || got[0] != rs {
		t.Fatalf("ActiveSessionsForOwner(rs-tenant) = %v, want [%s]: the RunnerSet owns its own bucket (Q677)", got, rs)
	}
	got := s.ActiveSessionsForOwner("tenant")
	if len(got) != 1 || !slices.Contains(got, base) {
		t.Fatalf("ActiveSessionsForOwner(tenant) = %v, want only %s: a RunnerSet must no longer share the RunnerGroup's bucket (Q677)", got, base)
	}
}

// TestServer_FailCreateSessionForOwner covers the owner-scoped CreateSession
// failure injection used to drive the controller's baseline-revival path (Q137):
// POST /session must 401 only for in-scope owners and recover when cleared.
func TestServer_FailCreateSessionForOwner(t *testing.T) {
	s := New()
	defer s.Close()

	s.FailCreateSessionForOwner("blocked-")

	// In-scope owner is rejected.
	if _, status := postSession(t, s.URL, "blocked-0", "bearer-b"); status != http.StatusUnauthorized {
		t.Fatalf("in-scope owner: want 401, got %d", status)
	}
	// Out-of-scope owner is unaffected.
	if _, status := postSession(t, s.URL, "allowed-0", "bearer-c"); status != http.StatusOK {
		t.Fatalf("out-of-scope owner: want 200, got %d", status)
	}

	// Clearing the override restores the in-scope owner.
	s.FailCreateSessionForOwner("")
	if _, status := postSession(t, s.URL, "blocked-0", "bearer-d"); status != http.StatusOK {
		t.Fatalf("after clear: want 200, got %d", status)
	}
}

// TestServer_HandleToken exercises the OAuth token endpoint and its
// unique-token-per-call contract.
func TestServer_HandleToken(t *testing.T) {
	s := New()
	defer s.Close()

	first := postToken(t, s.URL)
	second := postToken(t, s.URL)
	if first == "" || second == "" || first == second {
		t.Fatalf("expected two distinct tokens, got %q and %q", first, second)
	}
}

func postToken(t *testing.T, baseURL string) string {
	t.Helper()
	resp, err := http.Post(baseURL+"token", "application/x-www-form-urlencoded",
		strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		t.Fatalf("post token: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token: status %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	return out.AccessToken
}
