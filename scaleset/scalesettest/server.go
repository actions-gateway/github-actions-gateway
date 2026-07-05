// Package scalesettest provides a controllable HTTP stub for GitHub's
// runner-scale-set protocol, for testing the scaleset client. It encodes the
// semantics the live probes settled (Q264 plan §2a/§2b): auto-assign under an
// advertised capacity, the GHES-style JobAvailable→acquire flow, cursor-based
// at-least-once message replay (including replay to a re-created session),
// message-queue token expiry, and claim-once acquisition.
//
// A single httptest server serves the whole chain — the REST registration-token
// call, the RemoteAuth runner-registration hop, and the Actions Service
// (runnergroups, runnerscalesets CRUD, sessions, the message queue, acquirejobs,
// generatejitconfig) — with self-referential URLs, mirroring the real backend's
// broker-host tenant.
package scalesettest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"

	"github.com/actions-gateway/github-actions-gateway/scaleset"
)

// jobState is the lifecycle of a queued job inside the stub.
type jobState int

const (
	jobQueued    jobState = iota // enqueued, not yet offered or assigned
	jobAvailable                 // offered as JobAvailable (GHES acquire flow)
	jobAssigned                  // assigned to the scale set (auto-assign or post-acquire)
	jobRunning                   // a runner reported JobStarted
	jobCompleted                 // terminal
)

// job is one unit of work queued on a scale set.
type job struct {
	reqID  int64
	jobID  string
	state  jobState
	result string
}

// qmessage is one message in a scale set's queue log. The log is scale-set-scoped
// (not session-scoped), so it survives a session delete/recreate — the property
// that gives cursor replay to a fresh session (§2b-3).
type qmessage struct {
	id      int64
	entries []scaleset.JobMessage
	deleted bool
}

// session is a scale set's single active message-queue session.
type session struct {
	id         string
	ownerName  string
	queueToken string
}

// scaleSet is one registered scale set and its queue state.
type scaleSet struct {
	id       int
	name     string
	groupID  int
	session  *session
	jobs     []*job
	messages []*qmessage
}

// Server is the scale-set protocol stub. Construct it with New; call Close when done.
type Server struct {
	// URL is the Actions Service tenant base AND the REST API base (self-referential):
	// pass it as both scaleset.Config.APIBase and the base the admin connection
	// returns.
	URL    string
	server *httptest.Server

	mu sync.Mutex

	// ghesAcquireFlow selects the GHES JobAvailable→acquire path over the default
	// dotcom auto-assign path.
	ghesAcquireFlow bool

	adminToken    string
	adminTokenTTL time.Duration

	scaleSets  map[int]*scaleSet
	nextSSID   int
	nextSessID int
	nextMsgID  int64
	nextReqID  int64
	nextJobSeq int

	calls []string

	runnerRegistrationCalls int
	acquireJobsCalls        int
	refreshSessionCalls     int
	generateJITCalls        int
}

// New creates and starts a stub in the default dotcom auto-assign mode.
func New() *Server {
	s := &Server{
		adminTokenTTL: time.Hour,
		scaleSets:     make(map[int]*scaleSet),
		nextReqID:     1000,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orgs/{org}/actions/runners/registration-token", s.handleRegistrationToken)
	mux.HandleFunc("POST /repos/{owner}/{repo}/actions/runners/registration-token", s.handleRegistrationToken)
	mux.HandleFunc("POST /actions/runner-registration", s.handleRunnerRegistration)
	mux.HandleFunc("GET /_apis/runtime/runnergroups/", s.handleRunnerGroups)
	mux.HandleFunc("POST /_apis/runtime/runnerscalesets", s.handleCreateScaleSet)
	mux.HandleFunc("GET /_apis/runtime/runnerscalesets", s.handleGetScaleSetByName)
	mux.HandleFunc("GET /_apis/runtime/runnerscalesets/{id}", s.handleGetScaleSet)
	mux.HandleFunc("PATCH /_apis/runtime/runnerscalesets/{id}", s.handlePatchScaleSet)
	mux.HandleFunc("DELETE /_apis/runtime/runnerscalesets/{id}", s.handleDeleteScaleSet)
	mux.HandleFunc("POST /_apis/runtime/runnerscalesets/{id}/sessions", s.handleCreateSession)
	mux.HandleFunc("PATCH /_apis/runtime/runnerscalesets/{id}/sessions/{sid}", s.handleRefreshSession)
	mux.HandleFunc("DELETE /_apis/runtime/runnerscalesets/{id}/sessions/{sid}", s.handleDeleteSession)
	mux.HandleFunc("POST /_apis/runtime/runnerscalesets/{id}/generatejitconfig", s.handleGenerateJIT)
	mux.HandleFunc("POST /_apis/runtime/runnerscalesets/{id}/acquirejobs", s.handleAcquireJobs)
	mux.HandleFunc("GET /queue/{id}/message", s.handlePoll)
	mux.HandleFunc("DELETE /queue/{id}/message/{msgid}", s.handleDeleteMessage)
	s.server = httptest.NewServer(mux)
	s.URL = s.server.URL
	s.adminToken = s.mintAdminJWT(s.adminTokenTTL)
	return s
}

// HTTPClient returns an *http.Client suitable for the stub (a real local TCP
// listener, so the default client's unbounded read timeout is harmless — the test
// bounds the call). Pass it as both scaleset.Config.HTTPClient and PollClient.
func (s *Server) HTTPClient() *http.Client {
	return s.server.Client()
}

// Close shuts down the stub server.
func (s *Server) Close() { s.server.Close() }

// EnableGHESAcquireFlow switches the stub to the GHES path: queued jobs are offered
// as JobAvailable and the client must claim them with acquirejobs (auto-assign is
// the default). Call before enqueuing jobs.
func (s *Server) EnableGHESAcquireFlow() {
	s.mu.Lock()
	s.ghesAcquireFlow = true
	s.mu.Unlock()
}

// SetAdminTokenTTL controls the TTL of the admin JWT minted by the
// runner-registration hop. Set it below the client's refresh lead (default 60s) to
// force the client to re-mint the admin connection on its next admin call — the
// lever for the admin-JWT lifecycle test (§2b-7). Takes effect on the next hop.
func (s *Server) SetAdminTokenTTL(ttl time.Duration) {
	s.mu.Lock()
	s.adminTokenTTL = ttl
	s.mu.Unlock()
}

// EnqueueJob queues one job on the scale set and returns its RunnerRequestID and
// job UUID. In auto-assign mode a poll advertising enough capacity assigns it; in
// GHES mode it is first offered as JobAvailable and assigned on acquire.
func (s *Server) EnqueueJob(scaleSetID int) (reqID int64, jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.scaleSets[scaleSetID]
	if ss == nil {
		panic(fmt.Sprintf("scalesettest: EnqueueJob on unknown scale set %d", scaleSetID))
	}
	s.nextReqID++
	s.nextJobSeq++
	j := &job{reqID: s.nextReqID, jobID: fmt.Sprintf("job-%d", s.nextJobSeq), state: jobQueued}
	ss.jobs = append(ss.jobs, j)
	return j.reqID, j.jobID
}

// CompleteAssignedJob marks an assigned job completed and appends a JobCompleted
// message to the queue, so a polling client observes the terminal result — the
// signal the classic protocol never delivered (§2b-6). Returns false if no
// non-terminal job with that id exists.
func (s *Server) CompleteAssignedJob(scaleSetID int, jobID, result string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.scaleSets[scaleSetID]
	if ss == nil {
		return false
	}
	for _, j := range ss.jobs {
		if j.jobID == jobID && j.state != jobCompleted {
			j.state = jobCompleted
			j.result = result
			ss.appendMessage(s.newMsgID(), []scaleset.JobMessage{{
				MessageType: scaleset.MessageTypeJobCompleted,
				JobID:       j.jobID,
				Result:      result,
			}})
			return true
		}
	}
	return false
}

// ExpireQueueToken rotates the active session's queue token without revealing the
// new value, so the client's cached token 401s on its next poll/acquire — the lever
// for the queue-token refresh test. RefreshSession (PATCH) hands the client the new
// token (§2b-2).
func (s *Server) ExpireQueueToken(scaleSetID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss := s.scaleSets[scaleSetID]; ss != nil && ss.session != nil {
		s.nextSessID++
		ss.session.queueToken = fmt.Sprintf("queue-token-rotated-%d", s.nextSessID)
	}
}

// DropSession clears the scale set's active session server-side, so the client's next
// poll 404s (NotFoundError) and it must re-create the session — which replays the
// scale-set-scoped queue log from the cursor head (§2b-3). The queue log itself
// persists (it is not session-scoped), which is what makes replay possible.
func (s *Server) DropSession(scaleSetID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss := s.scaleSets[scaleSetID]; ss != nil {
		ss.session = nil
	}
}

// AssignedJobCount returns how many of the scale set's jobs are currently
// assigned-but-not-completed (assigned or running) — the server-authoritative
// totalAssignedJobs. A test asserts it drains to zero to prove no delivery dangles
// after every job completes (the scale-set analog of brokertest's JobState check).
func (s *Server) AssignedJobCount(scaleSetID int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.scaleSets[scaleSetID]
	if ss == nil {
		return 0
	}
	return ss.stats().TotalAssignedJobs
}

// Calls returns the recorded call log in order.
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// RunnerRegistrationCalls returns how many RemoteAuth runner-registration hops the
// stub served — one per admin-connection mint, so >1 proves an admin-JWT refresh.
func (s *Server) RunnerRegistrationCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runnerRegistrationCalls
}

// AcquireJobsCalls returns how many acquirejobs calls the stub served — zero in
// auto-assign mode.
func (s *Server) AcquireJobsCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acquireJobsCalls
}

// RefreshSessionCalls returns how many session-refresh (PATCH) calls the stub served.
func (s *Server) RefreshSessionCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshSessionCalls
}

// GenerateJITCalls returns how many generatejitconfig calls the stub served.
func (s *Server) GenerateJITCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generateJITCalls
}

// ScaleSetIDByName returns the id of the scale set registered under name and whether
// one exists. It lets a caller that did not create the scale set itself — e.g. a
// controller test where the reconciler created it internally — resolve the id it needs
// for EnqueueJob/AssignedJobCount.
func (s *Server) ScaleSetIDByName(name string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ss := range s.scaleSets {
		if ss.name == name {
			return ss.id, true
		}
	}
	return 0, false
}

// HasActiveSession reports whether the scale set currently has a live message-queue
// session. A test asserts it drops to false after the listener stops, proving the
// session was deleted on shutdown (no leaked session).
func (s *Server) HasActiveSession(scaleSetID int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.scaleSets[scaleSetID]
	return ss != nil && ss.session != nil
}

// ── internals ───────────────────────────────────────────────────────────────

func (s *Server) record(format string, args ...any) {
	s.calls = append(s.calls, fmt.Sprintf(format, args...))
}

func (s *Server) newMsgID() int64 {
	s.nextMsgID++
	return s.nextMsgID
}

// mintAdminJWT builds a syntactically valid JWT with an exp claim ttl from now. The
// client reads exp (without verifying the signature) to schedule its pre-expiry
// refresh, so a dummy signature segment suffices.
func (s *Server) mintAdminJWT(ttl time.Duration) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	header := enc(map[string]string{"alg": "none", "typ": "JWT"})
	payload := enc(map[string]int64{"exp": time.Now().Add(ttl).Unix()})
	return header + "." + payload + ".sig"
}

// requireAdmin validates the admin JWT on an _apis/runtime call, writing 401 and
// returning false on mismatch.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+s.adminToken {
		http.Error(w, `{"message":"invalid admin token"}`, http.StatusUnauthorized)
		return false
	}
	if r.URL.Query().Get("api-version") != "6.0-preview" {
		http.Error(w, `{"message":"missing api-version"}`, http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) handleRegistrationToken(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.record("registration-token %s", r.URL.Path)
	s.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"token":      "reg-token",
		"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	})
}

func (s *Server) handleRunnerRegistration(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("runner-registration")
	s.runnerRegistrationCalls++
	if got := r.Header.Get("Authorization"); got != "RemoteAuth reg-token" {
		http.Error(w, `{"message":"bad RemoteAuth"}`, http.StatusUnauthorized)
		return
	}
	// Mint a fresh admin JWT with the current TTL — a short TTL forces the client
	// to re-mint on its next admin call.
	s.adminToken = s.mintAdminJWT(s.adminTokenTTL)
	_ = json.NewEncoder(w).Encode(scaleset.AdminConnection{URL: s.URL, Token: s.adminToken})
}

func (s *Server) handleRunnerGroups(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.URL.Query().Get("groupName")
	s.record("runnergroups name=%s", name)
	if !s.requireAdmin(w, r) {
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"count": 1,
		"value": []scaleset.RunnerGroup{{ID: 7, Name: name}},
	})
}

func (s *Server) handleCreateScaleSet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("create-scaleset")
	if !s.requireAdmin(w, r) {
		return
	}
	var in scaleset.RunnerScaleSet
	_ = json.NewDecoder(r.Body).Decode(&in)
	s.nextSSID++
	ss := &scaleSet{id: s.nextSSID, name: in.Name, groupID: in.RunnerGroupID}
	s.scaleSets[ss.id] = ss
	_ = json.NewEncoder(w).Encode(scaleset.RunnerScaleSet{
		ID: ss.id, Name: ss.name, RunnerGroupID: ss.groupID, Labels: in.Labels, RunnerSetting: in.RunnerSetting,
	})
}

func (s *Server) handleGetScaleSetByName(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.URL.Query().Get("name")
	s.record("get-scaleset name=%s", name)
	if !s.requireAdmin(w, r) {
		return
	}
	var match []scaleset.RunnerScaleSet
	for _, ss := range s.scaleSets {
		if ss.name == name {
			match = append(match, scaleset.RunnerScaleSet{ID: ss.id, Name: ss.name, RunnerGroupID: ss.groupID})
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"count": len(match), "value": match})
}

func (s *Server) handleGetScaleSet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.record("get-scaleset id=%d", id)
	if !s.requireAdmin(w, r) {
		return
	}
	ss := s.scaleSets[id]
	if ss == nil {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(scaleset.RunnerScaleSet{ID: ss.id, Name: ss.name, RunnerGroupID: ss.groupID})
}

func (s *Server) handlePatchScaleSet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.record("patch-scaleset id=%d", id)
	if !s.requireAdmin(w, r) {
		return
	}
	ss := s.scaleSets[id]
	if ss == nil {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		return
	}
	var patch scaleset.RunnerScaleSet
	_ = json.NewDecoder(r.Body).Decode(&patch)
	if patch.Name != "" {
		ss.name = patch.Name
	}
	if patch.RunnerGroupID != 0 {
		ss.groupID = patch.RunnerGroupID
	}
	_ = json.NewEncoder(w).Encode(scaleset.RunnerScaleSet{ID: ss.id, Name: ss.name, RunnerGroupID: ss.groupID})
}

func (s *Server) handleDeleteScaleSet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.record("delete-scaleset id=%d", id)
	if !s.requireAdmin(w, r) {
		return
	}
	delete(s.scaleSets, id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.record("create-session id=%d", id)
	if !s.requireAdmin(w, r) {
		return
	}
	ss := s.scaleSets[id]
	if ss == nil {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		return
	}
	if ss.session != nil {
		// One active session per scale set.
		http.Error(w, `{"message":"session already exists"}`, http.StatusConflict)
		return
	}
	var body struct {
		OwnerName string `json:"ownerName"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.nextSessID++
	ss.session = &session{
		id:         fmt.Sprintf("session-%d", s.nextSessID),
		ownerName:  body.OwnerName,
		queueToken: fmt.Sprintf("queue-token-%d", s.nextSessID),
	}
	_ = json.NewEncoder(w).Encode(scaleset.RunnerScaleSetSession{
		SessionID:               ss.session.id,
		OwnerName:               ss.session.ownerName,
		MessageQueueURL:         fmt.Sprintf("%s/queue/%d/message", s.URL, id),
		MessageQueueAccessToken: ss.session.queueToken,
		Statistics:              ss.stats(),
	})
}

func (s *Server) handleRefreshSession(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.record("refresh-session id=%d", id)
	s.refreshSessionCalls++
	if !s.requireAdmin(w, r) {
		return
	}
	ss := s.scaleSets[id]
	if ss == nil || ss.session == nil {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		return
	}
	// Issue a fresh queue token (rotating any expired one back to a known value).
	s.nextSessID++
	ss.session.queueToken = fmt.Sprintf("queue-token-%d", s.nextSessID)
	_ = json.NewEncoder(w).Encode(scaleset.RunnerScaleSetSession{
		SessionID:               ss.session.id,
		MessageQueueURL:         fmt.Sprintf("%s/queue/%d/message", s.URL, id),
		MessageQueueAccessToken: ss.session.queueToken,
		Statistics:              ss.stats(),
	})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.record("delete-session id=%d", id)
	if !s.requireAdmin(w, r) {
		return
	}
	if ss := s.scaleSets[id]; ss != nil {
		ss.session = nil // the queue log persists; a re-created session replays it
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGenerateJIT(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.record("generatejitconfig id=%d", id)
	s.generateJITCalls++
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	blob := base64.StdEncoding.EncodeToString([]byte(`{".runner":{},".credentials":{}}`))
	out := scaleset.JITRunnerConfig{EncodedJITConfig: blob}
	out.Runner.ID = 77
	out.Runner.Name = in.Name
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleAcquireJobs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.acquireJobsCalls++
	ss := s.scaleSets[id]
	if ss == nil || ss.session == nil {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		return
	}
	// acquirejobs is authorized by the queue token, not the admin JWT (§2.5).
	if r.Header.Get("Authorization") != "Bearer "+ss.session.queueToken {
		http.Error(w, `{"message":"invalid queue token"}`, http.StatusUnauthorized)
		return
	}
	var ids []int64
	_ = json.NewDecoder(r.Body).Decode(&ids)
	s.record("acquirejobs id=%d ids=%v", id, ids)
	var won []int64
	for _, reqID := range ids {
		for _, j := range ss.jobs {
			// Claim-once: only an offered-but-unclaimed job can be won; a second
			// claim of the same id finds it already assigned and is refused.
			if j.reqID == reqID && j.state == jobAvailable {
				j.state = jobAssigned
				ss.appendMessage(s.newMsgID(), []scaleset.JobMessage{{
					MessageType:     scaleset.MessageTypeJobAssigned,
					RunnerRequestID: j.reqID,
					JobID:           j.jobID,
				}})
				won = append(won, reqID)
			}
		}
	}
	if won == nil {
		won = []int64{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"count": len(won), "value": won})
}

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := strconv.Atoi(r.PathValue("id"))
	capacity, _ := strconv.Atoi(r.Header.Get("X-ScaleSetMaxCapacity"))
	last, _ := strconv.ParseInt(r.URL.Query().Get("lastMessageId"), 10, 64)
	s.record("poll id=%d cap=%d last=%d", id, capacity, last)

	ss := s.scaleSets[id]
	if ss == nil || ss.session == nil {
		http.Error(w, `{"message":"session not found"}`, http.StatusNotFound)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+ss.session.queueToken {
		http.Error(w, `{"message":"invalid queue token"}`, http.StatusUnauthorized)
		return
	}

	// Re-evaluate assignment on every poll against the advertised capacity — the
	// dynamic gate the live probe proved (§2b-1). GHES instead offers each queued
	// job as JobAvailable for the client to claim.
	if s.ghesAcquireFlow {
		ss.offerAvailable(s.newMsgID)
	} else {
		ss.assignUnderCapacity(capacity, s.newMsgID)
	}

	msg := ss.nextMessageAfter(last)
	if msg == nil {
		w.WriteHeader(http.StatusAccepted) // 202 — nothing to deliver, poll again
		return
	}
	_ = json.NewEncoder(w).Encode(s.encodeMessage(ss, msg))
}

func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := strconv.Atoi(r.PathValue("id"))
	msgID, _ := strconv.ParseInt(r.PathValue("msgid"), 10, 64)
	s.record("delete-message id=%d msg=%d", id, msgID)
	ss := s.scaleSets[id]
	if ss == nil || ss.session == nil {
		http.Error(w, `{"message":"session not found"}`, http.StatusNotFound)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+ss.session.queueToken {
		http.Error(w, `{"message":"invalid queue token"}`, http.StatusUnauthorized)
		return
	}
	for _, m := range ss.messages {
		if m.id == msgID {
			m.deleted = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	http.Error(w, `{"message":"message not found"}`, http.StatusNotFound)
}

// encodeMessage renders a queue message with the scale set's current statistics
// snapshot attached (the server-authoritative counts, read off every envelope).
func (s *Server) encodeMessage(ss *scaleSet, m *qmessage) scaleset.RunnerScaleSetMessage {
	body, _ := json.Marshal(m.entries)
	return scaleset.RunnerScaleSetMessage{
		MessageID:   m.id,
		MessageType: "RunnerScaleSetJobMessages",
		Body:        string(body),
		Statistics:  ss.stats(),
	}
}

// appendMessage adds a message to the scale set's queue log.
func (ss *scaleSet) appendMessage(id int64, entries []scaleset.JobMessage) {
	ss.messages = append(ss.messages, &qmessage{id: id, entries: entries})
}

// nextMessageAfter returns the earliest non-deleted message with id > last, or nil.
// This is the cursor: a re-created session polling from 0 replays any unacked
// (undeleted) message with its original id (§2b-3).
func (ss *scaleSet) nextMessageAfter(last int64) *qmessage {
	for _, m := range ss.messages {
		if m.id > last && !m.deleted {
			return m
		}
	}
	return nil
}

// assignUnderCapacity assigns queued jobs until the number of assigned-but-unfinished
// jobs reaches capacity, emitting one JobAssigned entry per newly assigned job. On
// the auto-assign backend RunnerRequestID is 0 and the JobID is authoritative (§2a-3).
func (ss *scaleSet) assignUnderCapacity(capacity int, nextID func() int64) {
	assigned := 0
	for _, j := range ss.jobs {
		if j.state == jobAssigned || j.state == jobRunning {
			assigned++
		}
	}
	for _, j := range ss.jobs {
		if assigned >= capacity {
			return
		}
		if j.state == jobQueued {
			j.state = jobAssigned
			ss.appendMessage(nextID(), []scaleset.JobMessage{{
				MessageType: scaleset.MessageTypeJobAssigned,
				JobID:       j.jobID,
			}})
			assigned++
		}
	}
}

// offerAvailable offers each still-queued job as a JobAvailable message (GHES flow).
func (ss *scaleSet) offerAvailable(nextID func() int64) {
	for _, j := range ss.jobs {
		if j.state == jobQueued {
			j.state = jobAvailable
			ss.appendMessage(nextID(), []scaleset.JobMessage{{
				MessageType:     scaleset.MessageTypeJobAvailable,
				RunnerRequestID: j.reqID,
			}})
		}
	}
}

// stats computes the server-authoritative statistics snapshot. TotalAssignedJobs is
// the assigned-but-not-completed count — the provision target the ARC clamp model
// keys off (§2.2).
func (ss *scaleSet) stats() *scaleset.RunnerScaleSetStatistic {
	var available, assigned, running int
	for _, j := range ss.jobs {
		switch j.state {
		case jobQueued, jobAvailable:
			available++
		case jobAssigned:
			assigned++
		case jobRunning:
			assigned++
			running++
		case jobCompleted:
		}
	}
	return &scaleset.RunnerScaleSetStatistic{
		TotalAvailableJobs: available,
		TotalAssignedJobs:  assigned,
		TotalRunningJobs:   running,
	}
}
