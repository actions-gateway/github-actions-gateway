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

	"github.com/karlkfi/github-actions-gateway/broker"
)

// Server is a test HTTP server that implements the broker v2 protocol endpoints.
type Server struct {
	URL    string
	server *httptest.Server

	mu                  sync.Mutex
	tokenCounter        atomic.Int64
	sessionCounter      int
	sessions            map[string]bool                       // sessionID → active
	deletedSessions     map[string]chan struct{}               // sessionID → closed on DELETE
	jobQueues           map[string]chan broker.TaskAgentMessage // sessionID → messages
	bearerSessions      map[string]string                     // bearerToken → sessionID
	acquireJobResponse  interface{}                           // custom AcquireJob response; nil uses default
	acquireCount        atomic.Int64
	msgCounter          atomic.Int64
	activeSessionsCount atomic.Int32 // +1 per POST /session, -1 per DELETE /session call
}

// New creates and starts a new broker Stub. Call Close when done.
func New() *Server {
	s := &Server{
		sessions:        make(map[string]bool),
		deletedSessions: make(map[string]chan struct{}),
		jobQueues:       make(map[string]chan broker.TaskAgentMessage),
		bearerSessions:  make(map[string]string),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", s.handleToken)
	mux.HandleFunc("/session", s.handleSession)
	mux.HandleFunc("/message", s.handleMessage)
	mux.HandleFunc("/acquirejob", s.handleAcquireJob)
	s.server = httptest.NewServer(mux)
	s.URL = s.server.URL + "/"
	return s
}

// HTTPClient returns an *http.Client suitable for use with the stub server.
// Since the stub uses a real TCP listener via httptest, the default client works.
func (s *Server) HTTPClient() *http.Client {
	return http.DefaultClient
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
func (s *Server) WaitForSessionDelete(sessionID string, timeout time.Duration) bool {
	s.mu.Lock()
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

// AcquireJobCalls returns the number of times /acquirejob was called.
func (s *Server) AcquireJobCalls() int {
	return int(s.acquireCount.Load())
}

// ActiveSessionCount returns the number of goroutines that have registered a session
// but not yet called DELETE /session. It is computed as (#POST /session − #DELETE /session)
// so each listener goroutine contributes +1 on start and −1 on exit, regardless of v2 mode.
func (s *Server) ActiveSessionCount() int {
	return int(s.activeSessionsCount.Load())
}

// SetAcquireJobResponse configures the JSON body returned by the next /acquirejob call.
// Pass nil to reset to the default response. The value is serialised with json.Marshal.
func (s *Server) SetAcquireJobResponse(v interface{}) {
	s.mu.Lock()
	s.acquireJobResponse = v
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

		s.mu.Lock()
		s.sessionCounter++
		sessionID := fmt.Sprintf("session-%d", s.sessionCounter)
		s.sessions[sessionID] = true
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

// handleAcquireJob serves POST /acquirejob — returns a synthetic AcquireJob response.
func (s *Server) handleAcquireJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.acquireCount.Add(1)
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
		}{PlanID: "test-plan-123"},
	})
}
