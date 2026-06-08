// Command fakegithub is a deployable HTTP stub that implements the GitHub App
// token exchange endpoint and the Actions broker v2 protocol. It is used in
// Tier B e2e tests so the AGC can start and process jobs without real GitHub
// credentials.
//
// Endpoints served:
//
//	POST /app/installations/{id}/access_tokens  — GitHub App token exchange
//	POST /token                                  — broker OAuth2 client credentials
//	POST /session                                — broker create session
//	DELETE /session                              — broker delete session
//	GET  /message                                — broker poll for message
//	POST /acquirejob                             — broker acquire job
//
// Jobs are injected via the HTTP control API (only reachable from within the
// pod; bind address is configurable via CONTROL_ADDR, default :9090):
//
//	POST /control/enqueue?sessionId=<id>  — body: RunnerJobRequestBody JSON
//	GET  /control/sessions                — active session IDs
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	controlAddr := os.Getenv("CONTROL_ADDR")
	if controlAddr == "" {
		controlAddr = ":9090"
	}

	s := newServer()

	go func() {
		log.Printf("control API listening on %s", controlAddr)
		if err := http.ListenAndServe(controlAddr, s.controlMux()); err != nil {
			log.Fatalf("control server: %v", err)
		}
	}()

	log.Printf("fakegithub listening on %s", addr)
	if err := http.ListenAndServe(addr, s.mainMux()); err != nil {
		log.Fatalf("main server: %v", err)
	}
}

type server struct {
	mu              sync.Mutex
	tokenCounter    atomic.Int64
	msgCounter      atomic.Int64
	sessionCounter  int
	sessions        map[string]bool
	jobQueues       map[string]chan message
	bearerSessions  map[string]string // bearer → sessionID
	acquireResponse any               // nil = default
	acquireCount    atomic.Int64
}

type message struct {
	MessageID   int64  `json:"messageId"`
	MessageType string `json:"messageType"`
	Body        string `json:"body"`
}

func newServer() *server {
	return &server{
		sessions:       make(map[string]bool),
		jobQueues:      make(map[string]chan message),
		bearerSessions: make(map[string]string),
	}
}

func (s *server) mainMux() http.Handler {
	mux := http.NewServeMux()
	// GitHub App token exchange — path includes installation ID
	mux.HandleFunc("/app/installations/", s.handleInstallationToken)
	// Broker endpoints
	mux.HandleFunc("/token", s.handleToken)
	mux.HandleFunc("/session", s.handleSession)
	mux.HandleFunc("/message", s.handleMessage)
	mux.HandleFunc("/acquirejob", s.handleAcquireJob)
	return mux
}

func (s *server) controlMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/control/enqueue", s.handleEnqueue)
	mux.HandleFunc("/control/sessions", s.handleListSessions)
	mux.HandleFunc("/control/acquirejob", s.handleSetAcquireJob)
	return mux
}

// handleInstallationToken serves POST /app/installations/{id}/access_tokens.
// It accepts any JWT and returns a synthetic installation access token.
func (s *server) handleInstallationToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := fmt.Sprintf("inst-token-%d", s.tokenCounter.Add(1))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":      token,
		"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	})
}

// handleToken serves POST /token — OAuth2 client credentials.
func (s *server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := fmt.Sprintf("bearer-%d", s.tokenCounter.Add(1))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"access_token": token,
		"token_type":   "Bearer",
	})
}

// handleSession serves POST /session and DELETE /session.
func (s *server) handleSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		s.sessionCounter++
		id := fmt.Sprintf("session-%d", s.sessionCounter)
		s.sessions[id] = true
		if bearer != "" {
			s.bearerSessions[bearer] = id
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionId": id})

	case http.MethodDelete:
		id := r.URL.Query().Get("sessionId")
		if id == "" {
			bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			s.mu.Lock()
			if sid, ok := s.bearerSessions[bearer]; ok {
				id = sid
				delete(s.bearerSessions, bearer)
			}
			s.mu.Unlock()
		}
		if id != "" {
			s.mu.Lock()
			s.sessions[id] = false
			s.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleMessage serves GET /message — returns 202 (no job) or 200+body (job).
func (s *server) handleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("sessionId")
	s.mu.Lock()
	ch := s.jobQueues[id]
	s.mu.Unlock()
	if ch != nil {
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

// handleAcquireJob serves POST /acquirejob.
func (s *server) handleAcquireJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	n := s.acquireCount.Add(1)
	w.Header().Set("Content-Type", "application/json")
	s.mu.Lock()
	custom := s.acquireResponse
	s.mu.Unlock()
	if custom != nil {
		_ = json.NewEncoder(w).Encode(custom)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"plan": map[string]string{
			"planId": fmt.Sprintf("plan-%d", n),
		},
	})
}

// handleEnqueue is the control API: POST /control/enqueue?sessionId=<id>
// Body is a RunnerJobRequestBody JSON that gets wrapped as a broker message.
func (s *server) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("sessionId")
	if id == "" {
		http.Error(w, "sessionId required", http.StatusBadRequest)
		return
	}

	var body any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	bodyBytes, _ := json.Marshal(body)

	msg := message{
		MessageID:   s.msgCounter.Add(1),
		MessageType: "RunnerJobRequest",
		Body:        string(bodyBytes),
	}

	s.mu.Lock()
	ch, ok := s.jobQueues[id]
	if !ok {
		ch = make(chan message, 16)
		s.jobQueues[id] = ch
	}
	s.mu.Unlock()

	select {
	case ch <- msg:
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "queue full", http.StatusTooManyRequests)
	}
}

// handleListSessions is the control API: GET /control/sessions
func (s *server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	var active []string
	for id, ok := range s.sessions {
		if ok {
			active = append(active, id)
		}
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(active)
}

// handleSetAcquireJob is the control API: POST /control/acquirejob
// Sets a custom response body for the next /acquirejob call. Empty body resets to default.
func (s *server) handleSetAcquireJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.ContentLength == 0 {
		s.acquireResponse = nil
		w.WriteHeader(http.StatusOK)
		return
	}
	var v any
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.acquireResponse = v
	w.WriteHeader(http.StatusOK)
}
