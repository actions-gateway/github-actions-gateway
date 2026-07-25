package brokerstub

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestSessions_CreateAndResolveByQuery(t *testing.T) {
	s := NewSessions()
	id := s.Create("grp-0", 7, "2.335.1", "bearer-a")
	if id != "session-1" {
		t.Fatalf("first minted ID = %q, want session-1", id)
	}
	sess, ok := s.Get(id)
	if !ok || sess.Owner != "grp-0" || sess.AgentID != 7 || sess.Version != "2.335.1" || !sess.Active {
		t.Fatalf("Get(%q) = %+v, ok=%v", id, sess, ok)
	}

	// Resolve DELETE by the v1 sessionId query param.
	got, ok := s.ResolveDelete(id, "")
	if !ok || got != id {
		t.Fatalf("ResolveDelete(query) = %q, %v; want %q, true", got, ok, id)
	}
	if s.IsActive(id) {
		t.Fatal("session should be inactive after ResolveDelete")
	}
}

func TestSessions_ResolveDeleteByBearer(t *testing.T) {
	s := NewSessions()
	id := s.Create("", 0, "", "bearer-xyz")

	// The v2 path presents no sessionId; the bearer token identifies the session.
	got, ok := s.ResolveDelete("", "bearer-xyz")
	if !ok || got != id {
		t.Fatalf("ResolveDelete(bearer) = %q, %v; want %q, true", got, ok, id)
	}
	// The bearer mapping is consumed, so a replay does not resolve again.
	if _, ok := s.ResolveDelete("", "bearer-xyz"); ok {
		t.Fatal("bearer should be single-use for DELETE resolution")
	}
}

func TestSessions_ActiveCountTracksDelta(t *testing.T) {
	s := NewSessions()
	s.Create("", 0, "", "b1")
	s.Create("", 0, "", "b2")
	if got := s.ActiveCount(); got != 2 {
		t.Fatalf("ActiveCount after 2 creates = %d, want 2", got)
	}
	// CountDelete is unconditional (mirrors one DELETE request), independent of
	// whether a session resolves.
	s.CountDelete()
	if got := s.ActiveCount(); got != 1 {
		t.Fatalf("ActiveCount after 1 delete = %d, want 1", got)
	}
}

func TestSessions_ActiveIDsAndVersionsScopeByOwner(t *testing.T) {
	s := NewSessions()
	a := s.Create("teamA-0", 1, "2.1.0", "ba")
	s.Create("teamB-0", 2, "2.2.0", "bb")

	if got := s.ActiveIDs("teamA-"); !slices.Equal(got, []string{a}) {
		t.Fatalf("ActiveIDs(teamA-) = %v, want [%s]", got, a)
	}
	if got := s.ActiveIDs(""); len(got) != 2 {
		t.Fatalf("ActiveIDs(all) = %v, want 2 sessions", got)
	}
	vers := s.Versions("teamA-")
	if len(vers) != 1 || vers[a] != "2.1.0" {
		t.Fatalf("Versions(teamA-) = %v, want {%s:2.1.0}", vers, a)
	}

	// A deleted session drops out of the active listings.
	s.ResolveDelete(a, "")
	if got := s.ActiveIDs("teamA-"); len(got) != 0 {
		t.Fatalf("ActiveIDs(teamA-) after delete = %v, want none", got)
	}
}

func TestSessions_SetInactive(t *testing.T) {
	s := NewSessions()
	id := s.Create("", 0, "", "b")
	s.SetInactive(id)
	if s.IsActive(id) {
		t.Fatal("SetInactive should mark the session dead")
	}
	// Still retained so a test can observe the ID was seen then killed.
	if _, ok := s.Get(id); !ok {
		t.Fatal("inactive session should be retained in the registry")
	}
	s.SetInactive("session-unknown") // no-op, must not panic
}

func TestWriteJSON_ExactLengthNoTrailingNewline(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusOK, map[string]string{"a": "b"})
	body := rec.Body.String()
	if strings.HasSuffix(body, "\n") {
		t.Fatalf("WriteJSON body has a trailing newline: %q", body)
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Fatal("WriteJSON must set an explicit Content-Length for connection reuse")
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(body), &out); err != nil || out["a"] != "b" {
		t.Fatalf("round-trip failed: %v (%q)", err, body)
	}
}

func TestAssertionIssuer(t *testing.T) {
	// A JWT payload (middle segment) carrying iss=client-42; header and
	// signature are irrelevant since the issuer is read without verification.
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"client-42"}`))
	assertion := "hdr." + payload + ".sig"
	form := url.Values{"client_assertion": {assertion}}
	r := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if got := AssertionIssuer(r); got != "client-42" {
		t.Fatalf("AssertionIssuer = %q, want client-42", got)
	}

	// A request without a parsable assertion yields "".
	empty := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(""))
	empty.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := AssertionIssuer(empty); got != "" {
		t.Fatalf("AssertionIssuer(no assertion) = %q, want empty", got)
	}
}

func TestWriteToken(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteToken(rec, "bearer-99")
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if out.AccessToken != "bearer-99" || out.TokenType != "Bearer" {
		t.Fatalf("WriteToken = %+v, want {bearer-99 Bearer}", out)
	}
}
