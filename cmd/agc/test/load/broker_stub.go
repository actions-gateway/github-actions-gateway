//go:build load

package load

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/broker/brokerstub"
)

// brokerStub is an in-process implementation of the GitHub Actions broker v2
// wire protocol, purpose-built for the load harness. It shares the session and
// credential mechanics (ID minting, bearer-DELETE resolution, JSON framing) with
// every other in-repo broker double via broker/brokerstub, and layers on the two
// behaviours the load model depends on:
//
//   - It auto-delivers a fresh job to every live session that polls (after an
//     optional think-time), so the harness drives continuous load without an
//     external enqueue loop. Each delivered job carries a unique
//     runner_request_id linked back to its session.
//   - It models the single-use JIT runner lifecycle (Q114): a successful
//     AcquireJob consumes the delivering session's agent — the session dies
//     (next GET /message returns 200-empty, then 401) and the agent's
//     credentials are rejected — forcing the listener goroutine to re-register
//     before it can poll again. This is the per-job re-registration cost the
//     harness exists to measure.
//
// It also holds an idle poll open for up to longPollHold (mirroring the real
// broker, Q148), though in the default saturated mode a job is always ready so
// the hold is never reached.
type brokerStub struct {
	server *httptest.Server
	// URL is the base URL with a trailing slash, matching what the agentpool
	// registrar hands to the broker client (broker.Client expects to join paths
	// onto it).
	URL string

	// longPollHold bounds how long GET /message parks an idle session before
	// returning 202. Reached only when no job is ready (think-time > 0).
	longPollHold time.Duration
	// thinkTime delays job delivery on each poll, modelling a gap between jobs.
	// Zero (the default) saturates every session: a job is always ready.
	thinkTime time.Duration

	// Counters (atomic, lock-free reads for the sampler).
	acquireCount atomic.Int64
	msgCounter   atomic.Int64

	// sessions is the shared session registry (minting, bearer-DELETE
	// resolution). The single-use lifecycle state below is load-specific and
	// keyed off the IDs it mints.
	sessions *brokerstub.Sessions

	mu              sync.Mutex
	sessionReadyAt  map[string]time.Time // sessionID → when think-time started
	consumedAgents  map[int64]bool       // agent IDs whose single-use record is spent
	requestSessions map[string]string    // runner_request_id → delivering sessionID
	deadPolls       map[string]int       // dead sessionID → polls since death
}

// longPollTick bounds how often a held GET /message rechecks for a ready job.
const longPollTick = 25 * time.Millisecond

func newBrokerStub(longPollHold, thinkTime time.Duration) *brokerStub {
	s := &brokerStub{
		longPollHold:    longPollHold,
		thinkTime:       thinkTime,
		sessions:        brokerstub.NewSessions(),
		sessionReadyAt:  make(map[string]time.Time),
		consumedAgents:  make(map[int64]bool),
		requestSessions: make(map[string]string),
		deadPolls:       make(map[string]int),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", s.handleToken)
	mux.HandleFunc("/session", s.handleSession)
	mux.HandleFunc("/message", s.handleMessage)
	mux.HandleFunc("/acquirejob", s.handleAcquireJob)
	mux.HandleFunc("/renewjob", s.handleRenewJob)
	s.server = httptest.NewServer(mux)
	s.URL = s.server.URL + "/"
	return s
}

// Close shuts down the stub server.
func (s *brokerStub) Close() { s.server.Close() }

// AcquireCount returns the total number of jobs acquired so far.
func (s *brokerStub) AcquireCount() int64 { return s.acquireCount.Load() }

func (s *brokerStub) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Reject the token exchange for a consumed single-use credential, mirroring
	// GitHub deleting the runner record (Q114). The issuer (clientId) carries the
	// agent ID as a suffix ("client-<agentID>") so we can map it back without a
	// clientId→agentID table.
	if id := assertionAgentID(r); id > 0 {
		s.mu.Lock()
		consumed := s.consumedAgents[id]
		s.mu.Unlock()
		if consumed {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
	}
	brokerstub.WriteToken(w, fmt.Sprintf("bearer-%d", s.msgCounter.Add(1)))
}

func (s *brokerStub) handleSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		bearer := brokerstub.Bearer(r)
		var reqBody struct {
			Agent struct {
				ID int64 `json:"id"`
			} `json:"agent"`
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)

		s.mu.Lock()
		consumed := s.consumedAgents[reqBody.Agent.ID]
		s.mu.Unlock()
		if consumed {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		id := s.sessions.Create("", reqBody.Agent.ID, "", bearer)
		s.mu.Lock()
		s.sessionReadyAt[id] = time.Now()
		s.mu.Unlock()

		brokerstub.WriteJSON(w, http.StatusOK, map[string]string{"sessionId": id})

	case http.MethodDelete:
		s.sessions.ResolveDelete(r.URL.Query().Get("sessionId"), brokerstub.Bearer(r))
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleMessage long-polls, then auto-delivers a fresh job to a live session.
// A consumed session returns the GitHub deleted-JIT signature: 200-empty on the
// first poll, 401 thereafter.
func (s *brokerStub) handleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("sessionId")
	deadline := time.Now().Add(s.longPollHold)
	for {
		s.mu.Lock()
		if polls, dead := s.deadPolls[id]; dead {
			s.deadPolls[id] = polls + 1
			s.mu.Unlock()
			if polls == 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK) // empty body → decode EOF
				return
			}
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		ready := s.sessionReadyAt[id]
		s.mu.Unlock()

		if !s.sessions.IsActive(id) {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		if time.Since(ready) >= s.thinkTime {
			s.deliverJob(w, id)
			return
		}
		wait := longPollTick
		if remaining := time.Until(deadline); remaining < wait {
			if remaining <= 0 {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			wait = remaining
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(wait):
		}
	}
}

// deliverJob writes a RunnerJobRequest carrying a unique runner_request_id
// (linked to the session for single-use consumption) and a run_service_url that
// points AcquireJob/RenewJob back at this stub.
func (s *brokerStub) deliverJob(w http.ResponseWriter, sessionID string) {
	reqID := fmt.Sprintf("req-%d", s.msgCounter.Add(1))
	s.mu.Lock()
	s.requestSessions[reqID] = sessionID
	s.mu.Unlock()

	body, _ := json.Marshal(map[string]any{
		"runner_request_id": reqID,
		"run_service_url":   strings.TrimRight(s.URL, "/"),
		"billing_owner_id":  "load",
	})
	msg := map[string]any{
		"messageId":   s.msgCounter.Add(1),
		"messageType": "RunnerJobRequest",
		"body":        string(body),
	}
	brokerstub.WriteJSON(w, http.StatusOK, msg)
}

// handleAcquireJob consumes the delivering session's single-use agent (Q114)
// and returns a synthetic plan.
func (s *brokerStub) handleAcquireJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var reqBody struct {
		JobMessageID string `json:"jobMessageId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&reqBody)

	n := s.acquireCount.Add(1)
	if reqBody.JobMessageID != "" {
		s.mu.Lock()
		sid, ok := s.requestSessions[reqBody.JobMessageID]
		if ok {
			delete(s.requestSessions, reqBody.JobMessageID)
		}
		s.mu.Unlock()
		if ok {
			// Consume the delivering session's single-use agent. deadPolls is set
			// under mu before the registry marks the session inactive, so a racing
			// GET /message sees the dead signature (checked first) rather than a
			// bare 202.
			sess, _ := s.sessions.Get(sid)
			s.mu.Lock()
			if sess.AgentID > 0 {
				s.consumedAgents[sess.AgentID] = true
			}
			s.deadPolls[sid] = 0
			s.mu.Unlock()
			s.sessions.SetInactive(sid)
		}
	}

	brokerstub.WriteJSON(w, http.StatusOK, map[string]any{
		"plan": map[string]string{"planId": fmt.Sprintf("plan-%d", n)},
	})
}

func (s *brokerStub) handleRenewJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	brokerstub.WriteJSON(w, http.StatusOK, map[string]string{
		"lockedUntil": time.Now().Add(10 * time.Minute).Format(time.RFC3339),
	})
}

// assertionAgentID extracts the agent ID from the client_assertion JWT issuer
// ("client-<agentID>"). Returns 0 when the request carries no parsable
// assertion or the issuer is not in that form.
func assertionAgentID(r *http.Request) int64 {
	var id int64
	if _, err := fmt.Sscanf(brokerstub.AssertionIssuer(r), "client-%d", &id); err != nil {
		return 0
	}
	return id
}

// loadRegistrar is an in-memory agentpool.Registrar. Every Register mints a
// globally-unique agent ID and a clientId of the form "client-<agentID>" (so
// the broker stub can recover the agent ID from the OAuth assertion issuer).
// Sharing one registrar across all tenants keeps agent IDs globally unique,
// sidestepping the cross-tenant ID-collision class fakegithub had to guard
// against (Q135). It generates no key pair, so agentpool falls back to its
// configured key type (the harness uses Ed25519 for fast generation).
type loadRegistrar struct {
	brokerURL string
	authURL   string
	nextID    atomic.Int64
}

func newLoadRegistrar(stub *brokerStub) *loadRegistrar {
	return &loadRegistrar{
		brokerURL: stub.URL,
		authURL:   stub.URL + "token",
	}
}

func (r *loadRegistrar) Register(_ context.Context, _ string, _ agentpool.RegisterParams) (*agentpool.AgentCredentials, error) {
	id := r.nextID.Add(1)
	return &agentpool.AgentCredentials{
		AgentID:          id,
		ClientID:         fmt.Sprintf("client-%d", id),
		AuthorizationURL: r.authURL,
		BrokerURL:        r.brokerURL,
	}, nil
}

func (r *loadRegistrar) Deregister(_ context.Context, _ string, _ int64) error { return nil }

func (r *loadRegistrar) ResolveAgentID(_ context.Context, _, _ string) (int64, error) {
	return 0, nil
}
