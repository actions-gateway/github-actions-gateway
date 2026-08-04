// Package brokerstub is the single implementation of the GitHub Actions broker
// wire protocol's session and credential mechanics shared by every in-repo
// broker test double.
//
// The doubles — broker/brokertest (the in-process integration stub),
// test/fakegithub (the deployed fake-GitHub e2e image), and cmd/agc/test/load
// (the load harness) — diverge in what a job delivery and an AcquireJob mean:
// fan-out accounting, single-use JIT consumption, saturated auto-delivery. The
// parts that are not scenario-specific — minting "session-<n>" IDs, resolving a
// DELETE /session by its sessionId query param or its bearer token, listing the
// live sessions, and the connection-reuse-safe JSON framing GitHub's clients
// require — live here, implemented once, so a wire-protocol change is made in
// one place rather than three.
//
// The package is deliberately dependency-free (standard library only): the
// fakegithub image links it into a distroless binary that is Trivy-scanned, so
// pulling in the broker client (and its transitive githubapp/JWT/Prometheus
// dependencies) would enlarge the scanned surface for no benefit. The scenario
// policies — job queues, fan-out state, lease models, single-use lifecycles —
// stay in each double, layered on top of this shared core.
package brokerstub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// Session is one virtual runner session registered via POST /session. The
// scenario-specific job and credential state each double tracks (queues,
// consumed-agent flags, dead-poll counters) is keyed off Session.ID and lives
// in the double, not here.
type Session struct {
	// ID is the minted "session-<n>" identifier.
	ID string
	// Owner is the ownerName the listener sent ("<CR name>-<agentIndex>", for a
	// RunnerGroup or a RunnerSet alike), or "" when the double does not model
	// owners. Used for prefix-scoped listing so a test asserts on only its own
	// CR's sessions on a shared instance.
	Owner string
	// AgentID is the runner agent ID the session was created for (the
	// single-use doubles map this back to a consumable runner record); 0 when
	// unset.
	AgentID int64
	// Version is the agent.version (runnerVersion) the AGC pinned at session
	// creation, captured so a spec can assert it is non-empty and correct.
	Version string
	// Active is false once DELETE /session has resolved the session (or a
	// single-use consumption killed it). Inactive sessions are retained so a
	// test can observe that a specific ID was deleted rather than never seen.
	Active bool
}

// Sessions is the shared session registry: it mints session IDs, tracks each
// session's identity, and resolves a DELETE by sessionId query param or bearer
// token. It carries its own mutex and is safe for concurrent use; callers must
// not hold any other lock while calling its methods, so the registry lock never
// nests under a double's own lock.
type Sessions struct {
	mu       sync.Mutex
	counter  int
	byID     map[string]*Session
	byBearer map[string]string // bearer token → sessionID

	// activeDelta is +1 per Create and -1 per CountDelete, giving the
	// "#POST − #DELETE /session" running total the integration stub asserts on.
	// It is intentionally independent of the live-session map (it accumulates
	// across a whole test package), so it is an atomic rather than a derived
	// count.
	activeDelta atomic.Int32
}

// NewSessions returns an empty session registry.
func NewSessions() *Sessions {
	return &Sessions{
		byID:     make(map[string]*Session),
		byBearer: make(map[string]string),
	}
}

// Create mints the next "session-<n>" ID, records the session as active, maps a
// non-empty bearer token to it, and returns the new ID. It increments the
// active-delta counter (see ActiveCount).
func (s *Sessions) Create(owner string, agentID int64, version, bearer string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counter++
	id := fmt.Sprintf("session-%d", s.counter)
	s.byID[id] = &Session{ID: id, Owner: owner, AgentID: agentID, Version: version, Active: true}
	if bearer != "" {
		s.byBearer[bearer] = id
	}
	s.activeDelta.Add(1)
	return id
}

// Get returns a copy of the session with the given ID and whether it exists
// (including inactive sessions retained after deletion).
func (s *Sessions) Get(id string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return Session{}, false
	}
	return *sess, true
}

// IsActive reports whether the session exists and has not been deleted or
// consumed.
func (s *Sessions) IsActive(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	return ok && sess.Active
}

// ResolveDelete resolves the session a DELETE /session refers to — by its
// sessionId query param (the v1 path) or, failing that, by the bearer token in
// its Authorization header (the v2 path) — marks it inactive, drops its bearer
// mapping, and returns the resolved ID. ok is false when neither path
// identifies a known session. It does not touch the active-delta counter; call
// CountDelete once per DELETE request for that.
func (s *Sessions) ResolveDelete(sessionIDQuery, bearer string) (id string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id = sessionIDQuery
	if id == "" {
		if sid, found := s.byBearer[bearer]; found {
			id = sid
			delete(s.byBearer, bearer)
		}
	}
	if id == "" {
		return "", false
	}
	sess, found := s.byID[id]
	if !found {
		return id, false
	}
	sess.Active = false
	return id, true
}

// SetInactive marks a session dead without going through the DELETE resolution
// (used when a single-use AcquireJob consumes the delivering session). It is a
// no-op for an unknown ID and leaves the active-delta counter untouched.
func (s *Sessions) SetInactive(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.byID[id]; ok {
		sess.Active = false
	}
}

// CountDelete decrements the active-delta counter. Call it once per DELETE
// /session request, unconditionally, so ActiveCount reflects "#POST − #DELETE"
// regardless of whether the session resolved.
func (s *Sessions) CountDelete() { s.activeDelta.Add(-1) }

// ActiveCount returns the running "#POST − #DELETE /session" total (see the
// activeDelta field). Each listener goroutine contributes +1 on start and −1 on
// exit, so this counts goroutines holding a session, independent of protocol
// version.
func (s *Sessions) ActiveCount() int { return int(s.activeDelta.Load()) }

// ActiveIDs returns the IDs of currently-active sessions whose owner has the
// given prefix. An empty prefix returns every active session. Deleted or
// consumed sessions are excluded.
func (s *Sessions) ActiveIDs(ownerPrefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.byID))
	for id, sess := range s.byID {
		if sess.Active && strings.HasPrefix(sess.Owner, ownerPrefix) {
			out = append(out, id)
		}
	}
	return out
}

// Versions returns a map of active session ID → captured agent.version for
// sessions whose owner has the given prefix (empty prefix = all). It backs the
// runner-version assertion (the AGC pins agent.version at CreateSession).
func (s *Sessions) Versions(ownerPrefix string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string)
	for id, sess := range s.byID {
		if sess.Active && strings.HasPrefix(sess.Owner, ownerPrefix) {
			out[id] = sess.Version
		}
	}
	return out
}

// Bearer returns the Authorization header's bearer token, or "" when absent.
func Bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// WriteJSON marshals v and writes it as a fixed-length response with no trailing
// newline. Both properties are load-bearing for HTTP connection reuse against
// the broker clients: they decode responses with a json.Decoder, which stops at
// the end of the JSON value — a trailing '\n' (as json.Encoder.Encode emits) or
// a chunked/unknown-length body leaves the connection un-drained, so net/http
// will not return it to the idle pool and every delivery leaks a connection and
// its read/write goroutines. With an exact Content-Length and no trailing byte
// the decode consumes the whole body and the connection is reused.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// AssertionIssuer extracts the iss claim from the client_assertion JWT in an
// OAuth client-credentials token request, without verifying the signature. The
// doubles issue predictable client IDs as the iss claim so /token can map a
// request back to the runner record behind it (to reject a consumed single-use
// credential). Returns "" when the request carries no parsable assertion.
func AssertionIssuer(r *http.Request) string {
	if err := r.ParseForm(); err != nil {
		return ""
	}
	parts := strings.Split(r.PostFormValue("client_assertion"), ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Iss
}

// WriteToken writes the OAuth client-credentials token response with the given
// access token as a Bearer token.
func WriteToken(w http.ResponseWriter, accessToken string) {
	WriteJSON(w, http.StatusOK, map[string]string{
		"access_token": accessToken,
		"token_type":   "Bearer",
	})
}
