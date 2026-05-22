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

// Stub is a test HTTP server that implements the broker v2 protocol endpoints.
type Stub struct {
	URL    string
	server *httptest.Server

	mu              sync.Mutex
	sessionCounter  int
	sessions        map[string]bool          // sessionID → active
	deletedSessions map[string]chan struct{}  // sessionID → closed on DELETE
	jobQueues       map[string]chan broker.TaskAgentMessage // sessionID → messages
	acquireCount    atomic.Int64
	msgCounter      atomic.Int64
}

// New creates and starts a new broker Stub. Call Close when done.
func New() *Stub {
	s := &Stub{
		sessions:        make(map[string]bool),
		deletedSessions: make(map[string]chan struct{}),
		jobQueues:       make(map[string]chan broker.TaskAgentMessage),
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
func (s *Stub) HTTPClient() *http.Client {
	return http.DefaultClient
}

// RegisteredSessions returns the IDs of all sessions that have been created.
func (s *Stub) RegisteredSessions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		out = append(out, id)
	}
	return out
}

// EnqueueJob places a job message onto the given session's queue.
// The RunServiceURL in the payload is overridden to point back to the stub
// so that /acquirejob calls come back here.
func (s *Stub) EnqueueJob(sessionID string, payload broker.RunnerJobRequestBody) {
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
func (s *Stub) WaitForSessionDelete(sessionID string, timeout time.Duration) bool {
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
func (s *Stub) AcquireJobCalls() int {
	return int(s.acquireCount.Load())
}

// Close shuts down the stub server.
func (s *Stub) Close() {
	s.server.Close()
}

// handleToken serves POST /token — OAuth2 client credentials response.
func (s *Stub) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"access_token": "test-token",
		"token_type":   "Bearer",
	})
}

// handleSession serves POST /session (create) and DELETE /session (delete).
func (s *Stub) handleSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.mu.Lock()
		s.sessionCounter++
		sessionID := fmt.Sprintf("session-%d", s.sessionCounter)
		s.sessions[sessionID] = true
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"sessionId": sessionID,
		})

	case http.MethodDelete:
		// In v2 flow the server identifies the session from the bearer token;
		// the stub marks any active session as deleted. We read the sessionId
		// from the URL query param if present (non-v2 path), otherwise close
		// all active sessions' delete channels.
		sessionID := r.URL.Query().Get("sessionId")
		if sessionID == "" {
			// v2 flow: close delete channels for all sessions (simple approach
			// since our tests use one session per registrar per test).
			s.mu.Lock()
			for id, active := range s.sessions {
				if active {
					s.sessions[id] = false
					if ch, ok := s.deletedSessions[id]; ok {
						select {
						case <-ch:
						default:
							close(ch)
						}
					}
				}
			}
			s.mu.Unlock()
		} else {
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
func (s *Stub) handleMessage(w http.ResponseWriter, r *http.Request) {
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
func (s *Stub) handleAcquireJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.acquireCount.Add(1)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(broker.AcquireJobResponse{
		Plan: struct {
			PlanID string `json:"planId"`
		}{PlanID: "test-plan-123"},
	})
}
