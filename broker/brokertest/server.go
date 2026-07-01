// Package brokertest provides a controllable HTTP stub for the GitHub Actions
// broker protocol used in integration tests.
package brokertest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions-gateway/github-actions-gateway/broker"
)

// Server is a test HTTP server that implements the broker v2 protocol endpoints.
type Server struct {
	URL    string
	server *httptest.Server

	mu                  sync.Mutex
	tokenCounter        atomic.Int64
	sessionCounter      int
	sessions            map[string]bool                         // sessionID → active
	sessionOwners       map[string]string                       // sessionID → ownerName ("<group>-<index>")
	deletedSessions     map[string]chan struct{}                // sessionID → closed on DELETE
	firstPollNotify     map[string]chan struct{}                // sessionID → closed on first GET /message
	jobQueues           map[string]chan broker.TaskAgentMessage // sessionID → messages
	bearerSessions      map[string]string                       // bearerToken → sessionID
	failSessionOwner    string                                  // when non-empty, 401 POST /session for owners with this prefix
	acquireJobResponse  any                                     // custom AcquireJob response; nil uses default
	acquireCount        atomic.Int64
	ackCount            atomic.Int64
	renewJobCount       atomic.Int64
	msgCounter          atomic.Int64
	activeSessionsCount atomic.Int32 // +1 per POST /session, -1 per DELETE /session call
}

// New creates and starts a new broker Stub. Call Close when done.
func New() *Server {
	s := &Server{
		sessions:        make(map[string]bool),
		sessionOwners:   make(map[string]string),
		deletedSessions: make(map[string]chan struct{}),
		firstPollNotify: make(map[string]chan struct{}),
		jobQueues:       make(map[string]chan broker.TaskAgentMessage),
		bearerSessions:  make(map[string]string),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", s.handleToken)
	mux.HandleFunc("/session", s.handleSession)
	mux.HandleFunc("/message", s.handleMessage)
	mux.HandleFunc("/acquirejob", s.handleAcquireJob)
	mux.HandleFunc("/renewjob", s.handleRenewJob)
	// The VSTS Task Agent delete-message ("acknowledge") endpoint lives under the
	// pool path: DELETE {poolBase}/messages/{id}. The probe calls it immediately
	// after AcquireJob returns client-side, so it is an observable "acquire
	// returned" signal (AcknowledgeCalls) that a test can wait on without racing
	// the still-in-flight AcquireJob request (Q258).
	mux.HandleFunc("/_apis/distributedtask/pools/", s.handleDeleteMessage)
	s.server = httptest.NewServer(mux)
	s.URL = s.server.URL + "/"
	return s
}

// HTTPClient returns an *http.Client suitable for use with the stub server.
// Since the stub uses a real TCP listener via httptest, the default client works
// and the unbounded read timeout is harmless — the test bounds the call (Q138).
func (s *Server) HTTPClient() *http.Client {
	return http.DefaultClient //nolint:forbidigo // Q138: bounded by the test's local httptest server.
}

// RegisteredSessions returns the IDs of sessions that are currently active
// (i.e. POST /session was called but DELETE /session has not been called yet).
// Deleted sessions from prior tests are not included.
func (s *Server) RegisteredSessions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.sessions))
	for id, active := range s.sessions {
		if active {
			out = append(out, id)
		}
	}
	return out
}

// ActiveSessionsForOwner returns the IDs of currently-active sessions whose
// ownerName belongs to the given RunnerGroup. CreateSession sends ownerName as
// "<group>-<agentIndex>", so a session is matched when its owner has the prefix
// "<group>-". Scoping by owner lets a test assert on only its own RunnerGroup's
// sessions, immune to sessions other tests left active on this shared stub — the
// global RegisteredSessions/ActiveSessionCount counters accumulate across the
// whole package and cause cross-test flakes when used for exact-count assertions.
func (s *Server) ActiveSessionsForOwner(group string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := group + "-"
	out := make([]string, 0, len(s.sessions))
	for id, active := range s.sessions {
		if active && strings.HasPrefix(s.sessionOwners[id], prefix) {
			out = append(out, id)
		}
	}
	return out
}

// EnqueueJob places a job message onto the given session's queue.
// The RunServiceURL in the payload is overridden to point back to the stub
// so that /acquirejob calls come back here.
func (s *Server) EnqueueJob(sessionID string, payload broker.RunnerJobRequestBody) {
	payload.RunServiceURL = strings.TrimRight(s.URL, "/")
	bodyBytes, _ := json.Marshal(payload)

	msg := broker.TaskAgentMessage{
		MessageID:   s.msgCounter.Add(1),
		MessageType: "RunnerJobRequest",
		Body:        string(bodyBytes),
	}

	s.mu.Lock()
	ch, ok := s.jobQueues[sessionID]
	if !ok {
		ch = make(chan broker.TaskAgentMessage, 16)
		s.jobQueues[sessionID] = ch
	}
	s.mu.Unlock()

	ch <- msg
}

// WaitForSessionDelete blocks until the given sessionID is deleted via DELETE /session
// or the timeout elapses. Returns true if the session was deleted in time.
// If the DELETE already arrived before this call, returns true immediately.
func (s *Server) WaitForSessionDelete(sessionID string, timeout time.Duration) bool {
	s.mu.Lock()
	// Fast path: DELETE already received before this call.
	if active, registered := s.sessions[sessionID]; registered && !active {
		s.mu.Unlock()
		return true
	}
	ch, ok := s.deletedSessions[sessionID]
	if !ok {
		ch = make(chan struct{})
		s.deletedSessions[sessionID] = ch
	}
	s.mu.Unlock()

	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	}
}

// WaitForFirstPoll blocks until the session with the given ID sends its first
// GET /message request, or until the timeout elapses. Returns true on success.
// Use this to confirm a listener goroutine has fully started (passed createSession
// and entered the poll loop) before simulating SIGTERM, so the goroutine is
// guaranteed to have registered its cleanup defer and will send DELETE /session.
func (s *Server) WaitForFirstPoll(sessionID string, timeout time.Duration) bool {
	s.mu.Lock()
	ch, ok := s.firstPollNotify[sessionID]
	if !ok {
		ch = make(chan struct{})
		s.firstPollNotify[sessionID] = ch
	}
	s.mu.Unlock()

	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	}
}

// AcquireJobCalls returns the number of times /acquirejob was called.
func (s *Server) AcquireJobCalls() int {
	return int(s.acquireCount.Load())
}

// AcknowledgeCalls returns the number of delete-message ("acknowledge") calls
// served — DELETE {poolBase}/messages/{id}. The probe issues this call only
// after AcquireJob has returned client-side, so observing it reach 1 guarantees
// the AcquireJob round-trip completed and its context is safe to cancel (Q258).
func (s *Server) AcknowledgeCalls() int {
	return int(s.ackCount.Load())
}

// RenewJobCalls returns the number of times /renewjob was called.
func (s *Server) RenewJobCalls() int {
	return int(s.renewJobCount.Load())
}

// ActiveSessionCount returns the number of goroutines that have registered a session
// but not yet called DELETE /session. It is computed as (#POST /session − #DELETE /session)
// so each listener goroutine contributes +1 on start and −1 on exit, regardless of v2 mode.
func (s *Server) ActiveSessionCount() int {
	return int(s.activeSessionsCount.Load())
}

// SetAcquireJobResponse configures the JSON body returned by the next /acquirejob call.
// Pass nil to reset to the default response. The value is serialised with json.Marshal.
func (s *Server) SetAcquireJobResponse(v any) {
	s.mu.Lock()
	s.acquireJobResponse = v
	s.mu.Unlock()
}

// FailCreateSessionForOwner makes POST /session return 401 Unauthorized for any
// session whose ownerName has the given prefix, simulating a broker that rejects
// a tenant's session creation. createSession maps the 401 to a NonRetriableError,
// so the listener's permanent baseline exits without being auto-restarted —
// letting a test drive the controller's baseline-revival path (Q137). An empty
// prefix clears the override. The prefix is matched against ownerName
// ("<group>-<agentIndex>"), so passing "<group>-" scopes it to one RunnerGroup.
func (s *Server) FailCreateSessionForOwner(prefix string) {
	s.mu.Lock()
	s.failSessionOwner = prefix
	s.mu.Unlock()
}

// Close shuts down the stub server.
func (s *Server) Close() {
	s.server.Close()
}

// handleToken serves POST /token — OAuth2 client credentials response.
// Each call returns a unique token so the v2 DELETE /session path can identify
// which session belongs to the calling goroutine via the Authorization header.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := fmt.Sprintf("token-%d", s.tokenCounter.Add(1))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"access_token": token,
		"token_type":   "Bearer",
	})
}

// handleSession serves POST /session (create) and DELETE /session (delete).
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

		// Parse ownerName ("<group>-<agentIndex>") so tests can scope session
		// assertions to one RunnerGroup via ActiveSessionsForOwner. Best-effort:
		// a missing or unparsable body simply leaves the owner empty.
		var reqBody struct {
			OwnerName string `json:"ownerName"`
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)

		s.mu.Lock()
		// Simulate a broker that rejects this owner's session creation (e.g. a
		// consumed single-use credential). createSession maps 401 to a
		// NonRetriableError, so the listener's permanent baseline exits and is not
		// auto-restarted — the precondition for the controller's baseline-revival
		// path (Q137). Scoped by owner prefix so one test's failure injection does
		// not affect other RunnerGroups sharing this stub.
		if p := s.failSessionOwner; p != "" && strings.HasPrefix(reqBody.OwnerName, p) {
			s.mu.Unlock()
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		s.sessionCounter++
		sessionID := fmt.Sprintf("session-%d", s.sessionCounter)
		s.sessions[sessionID] = true
		s.sessionOwners[sessionID] = reqBody.OwnerName
		if bearer != "" {
			s.bearerSessions[bearer] = sessionID
		}
		s.mu.Unlock()

		s.activeSessionsCount.Add(1)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"sessionId": sessionID,
		})

	case http.MethodDelete:
		// Each goroutine calls DELETE exactly once on exit; decrement the per-goroutine
		// counter regardless of v2 vs v1 mode.
		s.activeSessionsCount.Add(-1)

		// Identify the session: use the sessionId query param (v1) or look up the
		// Bearer token in the Authorization header (v2). The v2 path uses per-goroutine
		// unique tokens so only the calling goroutine's session is marked deleted.
		sessionID := r.URL.Query().Get("sessionId")
		if sessionID == "" {
			bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			s.mu.Lock()
			if sid, ok := s.bearerSessions[bearer]; ok {
				sessionID = sid
				delete(s.bearerSessions, bearer)
			}
			s.mu.Unlock()
		}

		if sessionID != "" {
			s.mu.Lock()
			s.sessions[sessionID] = false
			if ch, ok := s.deletedSessions[sessionID]; ok {
				select {
				case <-ch:
				default:
					close(ch)
				}
			}
			s.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleMessage serves GET /message — returns 202 (no job) or 200+JSON (job).
func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessionID := r.URL.Query().Get("sessionId")

	s.mu.Lock()
	ch, ok := s.jobQueues[sessionID]
	// Notify WaitForFirstPoll on the first GET /message for this session.
	if pollCh, known := s.firstPollNotify[sessionID]; known {
		select {
		case <-pollCh: // already closed — nothing to do
		default:
			close(pollCh)
		}
	} else {
		closedCh := make(chan struct{})
		close(closedCh)
		s.firstPollNotify[sessionID] = closedCh
	}
	s.mu.Unlock()

	if ok {
		select {
		case msg := <-ch:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(msg)
			return
		default:
		}
	}

	w.WriteHeader(http.StatusAccepted)
}

// handleDeleteMessage serves the VSTS Task Agent delete-message
// ("acknowledge") endpoint: DELETE {poolBase}/messages/{id}?sessionId=... It
// counts only DELETEs to a /messages/ path so unrelated pool traffic is ignored,
// and always replies 200 — the probe records the status but does not depend on
// it. See AcknowledgeCalls for why tests use this as a post-acquire signal.
func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/messages/") {
		s.ackCount.Add(1)
	}
	w.WriteHeader(http.StatusOK)
}

// handleRenewJob serves POST /renewjob — returns a synthetic RenewJob response.
func (s *Server) handleRenewJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.renewJobCount.Add(1)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"lockedUntil": time.Now().Add(10 * time.Minute).Format(time.RFC3339),
	})
}

// handleAcquireJob serves POST /acquirejob — returns a synthetic AcquireJob response.
func (s *Server) handleAcquireJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	n := s.acquireCount.Add(1)
	w.Header().Set("Content-Type", "application/json")

	s.mu.Lock()
	custom := s.acquireJobResponse
	s.mu.Unlock()

	if custom != nil {
		_ = json.NewEncoder(w).Encode(custom)
		return
	}
	_ = json.NewEncoder(w).Encode(broker.AcquireJobResponse{
		Plan: struct {
			PlanID string `json:"planId"`
		}{PlanID: fmt.Sprintf("test-plan-%d", n)},
	})
}
