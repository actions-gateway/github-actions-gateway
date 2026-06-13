// Command fakegithub is a deployable HTTP stub that implements the GitHub App
// token exchange endpoint, the Actions runner registration API, and the
// Actions broker v2 protocol. It is used in Tier B e2e tests so the AGC can
// start and process jobs without real GitHub credentials.
//
// Endpoints served:
//
//	POST /app/installations/{id}/access_tokens  — GitHub App token exchange
//	POST /token                                  — broker OAuth2 client credentials
//	POST /session                                — broker create session
//	DELETE /session                              — broker delete session
//	GET  /message                                — broker poll for message
//	POST /acquirejob                             — broker acquire job
//	POST /api/v3/{orgs/{org}|repos/{o}/{r}}/actions/runners/generate-jitconfig
//	GET  /api/v3/.../actions/runners?name=<n>    — list runners (name filter)
//	DELETE /api/v3/.../actions/runners/{id}      — deregister runner
//
// Jobs are injected via the HTTP control API (only reachable from within the
// pod; bind address is configurable via CONTROL_ADDR, default :9090):
//
//	POST /control/enqueue?sessionId=<id>  — body: RunnerJobRequestBody JSON
//	GET  /control/sessions                — active session IDs
//	POST /control/singleuse?enabled=true  — toggle single-use JIT simulation
//
// # Single-use JIT runner simulation (Q114)
//
// With single-use mode on (SINGLE_USE_RUNNERS=true or the control toggle),
// fakegithub mimics GitHub deleting a JIT runner record once it acquires a
// job: the session that delivered the acquired message dies — its next
// GET /message returns 200 with an empty body (the "decode response: EOF"
// signature) and 401 from then on — the runner record disappears (a
// name-colliding re-register without an intervening DELETE returns 409), and
// new sessions or token exchanges for the consumed agent's credentials return
// 401. Default off, opt in via SINGLE_USE_RUNNERS or /control/singleuse.
//
// # Opportunistic job redelivery
//
// A job whose target session is recycled away before it is acquired is not
// lost: it is carried to the owner's pending pool and delivered to the next
// live session of that owner, mirroring GitHub's pool-wide delivery (M1
// Investigation C/D). This keeps the post-job re-registration of single-use
// agents (Q114) from stranding jobs that race a session's recycle window.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"strconv"
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
	if os.Getenv("SINGLE_USE_RUNNERS") == "true" {
		s.singleUse.Store(true)
	}

	go func() {
		log.Printf("control API listening on %s", controlAddr)
		if err := http.ListenAndServe(controlAddr, s.controlMux()); err != nil { //nolint:gosec // G114: throwaway test fixture, not a production server
			log.Fatalf("control server: %v", err)
		}
	}()

	log.Printf("fakegithub listening on %s (single-use runners: %v)", addr, s.singleUse.Load())
	if err := http.ListenAndServe(addr, s.mainMux()); err != nil { //nolint:gosec // G114: throwaway test fixture, not a production server
		log.Fatalf("main server: %v", err)
	}
}

// runnerRecord is a live registered runner (JIT or implicit).
type runnerRecord struct {
	ID       int64
	Name     string
	ClientID string
}

type server struct {
	mu             sync.Mutex
	tokenCounter   atomic.Int64
	msgCounter     atomic.Int64
	sessionCounter int
	sessions       map[string]bool
	jobQueues      map[string][]message // sessionID → jobs enqueued directly to it
	// ownerPending holds jobs awaiting opportunistic delivery to any live
	// session of an owner — GitHub redelivers a job whose session went away
	// before acquiring it to any other polling session (M1 Investigation C/D).
	// A job is moved here when its session is deleted/consumed with the job
	// still queued, or enqueued here directly when its target session is
	// already dead. handleMessage drains a session's own queue first, then the
	// owner pool. Without it, a job stranded on a recycled session's queue
	// would be lost — fakegithub's per-session queue would otherwise be a
	// fidelity gap relative to GitHub's pool-wide delivery (Q114).
	ownerPending    map[string][]message // owner ("<group>-…" prefix-keyed) → jobs
	bearerSessions  map[string]string    // bearer → sessionID
	acquireResponse any                  // nil = default
	acquireCount    atomic.Int64

	// single-use JIT runner simulation (Q114)
	singleUse atomic.Bool
	// singleUseOwnerPrefix scopes the simulation to sessions whose ownerName
	// has this prefix ("" = all sessions). Lets one e2e spec opt its own
	// RunnerGroup in without affecting specs running in parallel against this
	// shared instance. Guarded by mu.
	singleUseOwnerPrefix string
	runnerCounter        int64
	runners              map[int64]*runnerRecord // live records by ID
	runnerNames          map[string]int64        // live record name → ID
	clientRunners        map[string]int64        // clientId → runner ID
	consumedAgents       map[int64]bool          // runner IDs whose record was consumed
	sessionAgents        map[string]int64        // sessionID → agent ID
	sessionOwners        map[string]string       // sessionID → ownerName
	deadPolls            map[string]int          // dead sessionID → GET /message count since death
	requestSessions      map[string]string       // runnerRequestId → delivering sessionID
}

type message struct {
	MessageID   int64  `json:"messageId"`
	MessageType string `json:"messageType"`
	Body        string `json:"body"`
}

func newServer() *server {
	return &server{
		sessions:        make(map[string]bool),
		jobQueues:       make(map[string][]message),
		ownerPending:    make(map[string][]message),
		bearerSessions:  make(map[string]string),
		runners:         make(map[int64]*runnerRecord),
		runnerNames:     make(map[string]int64),
		clientRunners:   make(map[string]int64),
		consumedAgents:  make(map[int64]bool),
		sessionAgents:   make(map[string]int64),
		sessionOwners:   make(map[string]string),
		deadPolls:       make(map[string]int),
		requestSessions: make(map[string]string),
	}
}

func (s *server) mainMux() http.Handler {
	mux := http.NewServeMux()
	// GitHub App token exchange — path includes installation ID
	mux.HandleFunc("/app/installations/", s.handleInstallationToken)
	// Runner registration API (GHES-style /api/v3 prefix, matching what
	// GithubRegistrar derives for a non-github.com host)
	mux.HandleFunc("/api/v3/", s.handleRunnerAPI)
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
	mux.HandleFunc("/control/singleuse", s.handleSetSingleUse)
	return mux
}

// externalBase derives the base URL clients should use to reach this server,
// from the Host header of the request being handled. fakegithub serves plain
// HTTP only.
func externalBase(r *http.Request) string {
	return "http://" + r.Host
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

// handleRunnerAPI routes the GHES-style runner registration endpoints:
//
//	POST   /api/v3/{prefix}/actions/runners/generate-jitconfig
//	GET    /api/v3/{prefix}/actions/runners[?name=<n>]
//	DELETE /api/v3/{prefix}/actions/runners/{id}
//
// where {prefix} is orgs/{org} or repos/{owner}/{repo}. The prefix itself is
// not validated — any org/repo works.
func (s *server) handleRunnerAPI(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	idx := strings.Index(path, "/actions/runners")
	if idx < 0 {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(path[idx:], "/actions/runners")

	switch {
	case rest == "/generate-jitconfig" && r.Method == http.MethodPost:
		s.handleGenerateJITConfig(w, r)
	case rest == "" && r.Method == http.MethodGet:
		s.handleListRunners(w, r)
	case strings.HasPrefix(rest, "/") && r.Method == http.MethodDelete:
		s.handleDeleteRunner(w, r, strings.TrimPrefix(rest, "/"))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleGenerateJITConfig registers a JIT runner: mints an ID and an RSA key
// pair, and returns the encoded JIT config blob in the format the AGC's
// GithubRegistrar parses. A name held by a live record returns 409.
func (s *server) handleGenerateJITConfig(w http.ResponseWriter, r *http.Request) {
	var reqBody struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil || reqBody.Name == "" {
		http.Error(w, `{"message":"name required"}`, http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	if _, exists := s.runnerNames[reqBody.Name]; exists {
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"Already exists"}`))
		return
	}
	s.runnerCounter++
	id := s.runnerCounter
	clientID := fmt.Sprintf("jit-client-%d", id)
	rec := &runnerRecord{ID: id, Name: reqBody.Name, ClientID: clientID}
	s.runners[id] = rec
	s.runnerNames[reqBody.Name] = id
	s.clientRunners[clientID] = id
	s.mu.Unlock()

	blob, err := buildJITConfigBlob(id, clientID, externalBase(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"runner":             map[string]any{"id": id, "name": reqBody.Name},
		"encoded_jit_config": blob,
	})
}

// handleListRunners serves the list endpoint with the optional name filter
// used by GithubRegistrar.ResolveAgentID.
func (s *server) handleListRunners(w http.ResponseWriter, r *http.Request) {
	nameFilter := r.URL.Query().Get("name")
	type runnerJSON struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	var out []runnerJSON
	s.mu.Lock()
	for _, rec := range s.runners {
		if nameFilter == "" || rec.Name == nameFilter {
			out = append(out, runnerJSON{ID: rec.ID, Name: rec.Name})
		}
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"total_count": len(out),
		"runners":     out,
	})
}

// handleDeleteRunner deregisters a runner record by ID.
func (s *server) handleDeleteRunner(w http.ResponseWriter, _ *http.Request, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"message":"bad runner id"}`, http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	rec, ok := s.runners[id]
	if ok {
		delete(s.runners, id)
		delete(s.runnerNames, rec.Name)
		delete(s.clientRunners, rec.ClientID)
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// buildJITConfigBlob assembles the base64 JIT config blob: a JSON object
// mapping runner config file names to their base64-encoded contents, in the
// format parsed by the AGC's parseJITCredentials.
func buildJITConfigBlob(agentID int64, clientID, baseURL string) (string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", fmt.Errorf("generate runner key: %v", err)
	}
	key.Precompute()

	runnerFile, _ := json.Marshal(map[string]any{
		"agentId":     agentID,
		"serverUrl":   baseURL,
		"serverUrlV2": baseURL,
		"useV2Flow":   true,
	})
	credsFile, _ := json.Marshal(map[string]any{
		"scheme": "OAuth",
		"data": map[string]string{
			"clientId":         clientID,
			"authorizationUrl": baseURL + "/token",
		},
	})
	b64 := base64.StdEncoding.EncodeToString
	rsaFile, _ := json.Marshal(map[string]string{
		"modulus":  b64(key.N.Bytes()),
		"exponent": b64(big.NewInt(int64(key.E)).Bytes()),
		"d":        b64(key.D.Bytes()),
		"p":        b64(key.Primes[0].Bytes()),
		"q":        b64(key.Primes[1].Bytes()),
		"dp":       b64(key.Precomputed.Dp.Bytes()),
		"dq":       b64(key.Precomputed.Dq.Bytes()),
		"inverseQ": b64(key.Precomputed.Qinv.Bytes()),
	})

	blob, _ := json.Marshal(map[string]string{
		".runner":                b64(runnerFile),
		".credentials":           b64(credsFile),
		".credentials_rsaparams": b64(rsaFile),
	})
	return b64(blob), nil
}

// handleToken serves POST /token — OAuth2 client credentials. In single-use
// mode, a client assertion issued by a consumed agent's credentials is
// rejected with 401 (the runner record behind it no longer exists). Unknown
// client IDs — e.g. the AGC's StubRegistrar's shared "stub-client-id" — are
// always accepted.
func (s *server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.singleUse.Load() {
		if clientID := assertionIssuer(r); clientID != "" {
			s.mu.Lock()
			// clientRunners entries survive record consumption (see
			// consumeSessionLocked) precisely so this lookup can reject the
			// dead credential.
			id, known := s.clientRunners[clientID]
			consumed := known && s.consumedAgents[id]
			s.mu.Unlock()
			if consumed {
				http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
		}
	}
	token := fmt.Sprintf("bearer-%d", s.tokenCounter.Add(1))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"access_token": token,
		"token_type":   "Bearer",
	})
}

// assertionIssuer extracts the iss claim from the client_assertion JWT in an
// OAuth token request, without verifying the signature. Returns "" when the
// request carries no parsable assertion.
func assertionIssuer(r *http.Request) string {
	if err := r.ParseForm(); err != nil {
		return ""
	}
	assertion := r.PostFormValue("client_assertion")
	parts := strings.Split(assertion, ".")
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

// handleSession serves POST /session and DELETE /session.
func (s *server) handleSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

		var reqBody struct {
			OwnerName string `json:"ownerName"`
			Agent     struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"agent"`
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)

		s.mu.Lock()
		if s.singleUse.Load() {
			if s.consumedAgents[reqBody.Agent.ID] {
				// The agent's single-use runner record was consumed; like real
				// GitHub, a new session under its credentials is rejected.
				s.mu.Unlock()
				http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if _, known := s.runners[reqBody.Agent.ID]; !known && reqBody.Agent.ID > 0 {
				// Implicitly register agents minted by the AGC's StubRegistrar so
				// single-use mode works without routing registration through us.
				name := reqBody.Agent.Name
				if name == "" {
					name = reqBody.OwnerName
				}
				s.runners[reqBody.Agent.ID] = &runnerRecord{ID: reqBody.Agent.ID, Name: name}
				if name != "" {
					s.runnerNames[name] = reqBody.Agent.ID
				}
			}
		}
		s.sessionCounter++
		id := fmt.Sprintf("session-%d", s.sessionCounter)
		s.sessions[id] = true
		s.sessionAgents[id] = reqBody.Agent.ID
		s.sessionOwners[id] = reqBody.OwnerName
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
			// A listener recycling its agent deletes the old session; carry any
			// jobs still queued on it to the owner pool for redelivery.
			s.requeueLocked(id)
			s.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleMessage serves GET /message — returns 202 (no job) or 200+body (job).
// A session whose agent was consumed mimics the live-observed GitHub
// signature: 200 with an empty body on the first poll after death, 401 from
// then on (M4 §12).
func (s *server) handleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("sessionId")
	s.mu.Lock()
	if polls, dead := s.deadPolls[id]; dead {
		s.deadPolls[id] = polls + 1
		s.mu.Unlock()
		if polls == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK) // empty body → "decode response: EOF"
			return
		}
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	// Deliver from the session's own queue first, then fall back to the owner's
	// pending pool (a job whose original session was recycled away). Returning
	// the message under the lock keeps the dequeue atomic.
	var msg *message
	if q := s.jobQueues[id]; len(q) > 0 {
		m := q[0]
		s.jobQueues[id] = q[1:]
		msg = &m
	} else if owner := s.sessionOwners[id]; owner != "" {
		if p := s.ownerPending[owner]; len(p) > 0 {
			m := p[0]
			s.ownerPending[owner] = p[1:]
			msg = &m
		}
	}
	s.mu.Unlock()
	if msg != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(*msg)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleAcquireJob serves POST /acquirejob. In single-use mode a successful
// acquisition consumes the delivering session's agent: the runner record is
// deleted and the session dies.
func (s *server) handleAcquireJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var reqBody struct {
		JobMessageID string `json:"jobMessageId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&reqBody)

	n := s.acquireCount.Add(1)

	if s.singleUse.Load() && reqBody.JobMessageID != "" {
		s.mu.Lock()
		if sid, ok := s.requestSessions[reqBody.JobMessageID]; ok {
			delete(s.requestSessions, reqBody.JobMessageID)
			if s.singleUseOwnerPrefix == "" || strings.HasPrefix(s.sessionOwners[sid], s.singleUseOwnerPrefix) {
				s.consumeSessionLocked(sid)
			}
		}
		s.mu.Unlock()
	}

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

// consumeSessionLocked marks a session's agent as consumed and the session as
// dead. Caller must hold s.mu.
func (s *server) consumeSessionLocked(sessionID string) {
	agentID := s.sessionAgents[sessionID]
	if agentID > 0 {
		s.consumedAgents[agentID] = true
		if rec, ok := s.runners[agentID]; ok {
			delete(s.runners, agentID)
			delete(s.runnerNames, rec.Name)
			// clientRunners entry is kept so /token can 401 the dead credential.
		}
	}
	s.sessions[sessionID] = false
	s.deadPolls[sessionID] = 0
	s.requeueLocked(sessionID)
}

// requeueLocked moves any jobs still queued on a now-dead session to its
// owner's pending pool so a live session can pick them up. Caller must hold
// s.mu. The acquired job that triggered consumption is already dequeued, so
// this only carries genuinely undelivered jobs.
func (s *server) requeueLocked(sessionID string) {
	q := s.jobQueues[sessionID]
	if len(q) == 0 {
		return
	}
	owner := s.sessionOwners[sessionID]
	s.ownerPending[owner] = append(s.ownerPending[owner], q...)
	delete(s.jobQueues, sessionID)
}

// handleEnqueue is the control API: POST /control/enqueue?sessionId=<id>
// Body is a RunnerJobRequestBody JSON that gets wrapped as a broker message.
// A missing runner_request_id is injected (single-use mode links the
// subsequent AcquireJob back to this session through it).
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

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	msgID := s.msgCounter.Add(1)
	if body == nil {
		body = map[string]any{}
	}
	reqID, _ := body["runner_request_id"].(string)
	if reqID == "" {
		reqID = fmt.Sprintf("req-%d", msgID)
		body["runner_request_id"] = reqID
	}
	bodyBytes, _ := json.Marshal(body)

	msg := message{
		MessageID:   msgID,
		MessageType: "RunnerJobRequest",
		Body:        string(bodyBytes),
	}

	s.mu.Lock()
	s.requestSessions[reqID] = id
	owner := s.sessionOwners[id]
	if s.sessions[id] {
		// Target session is live: queue it there so a specific session can be
		// addressed (the single-use spec relies on this to consume one session).
		s.jobQueues[id] = append(s.jobQueues[id], msg)
	} else {
		// Target session is already gone (recycled between the test's session
		// query and this enqueue): hand the job to the owner pool so the next
		// live session picks it up, mirroring GitHub's pool-wide redelivery.
		s.ownerPending[owner] = append(s.ownerPending[owner], msg)
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// handleListSessions is the control API: GET /control/sessions[?owner=<prefix>]
// The optional owner prefix filters to sessions whose ownerName starts with
// it, so a test can observe only its own RunnerGroup's sessions on this
// shared instance.
func (s *server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ownerPrefix := r.URL.Query().Get("owner")
	s.mu.Lock()
	var active []string
	for id, ok := range s.sessions {
		if ok && (ownerPrefix == "" || strings.HasPrefix(s.sessionOwners[id], ownerPrefix)) {
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

// handleSetSingleUse is the control API:
//
//	POST /control/singleuse?enabled=true|false[&owner=<prefix>]
//
// Toggles the single-use JIT runner simulation at runtime. The optional owner
// prefix scopes consumption to sessions whose ownerName starts with it, so a
// test can opt in only its own RunnerGroup's sessions on this shared instance.
func (s *server) handleSetSingleUse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	enabled, err := strconv.ParseBool(r.URL.Query().Get("enabled"))
	if err != nil {
		http.Error(w, "enabled=true|false required", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.singleUseOwnerPrefix = r.URL.Query().Get("owner")
	s.mu.Unlock()
	s.singleUse.Store(enabled)
	w.WriteHeader(http.StatusOK)
}
