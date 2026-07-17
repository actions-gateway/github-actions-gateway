// Package scalesettest provides a controllable HTTP stub for GitHub's
// runner-scale-set protocol, for testing the scaleset client. It encodes the
// semantics the live probes settled (Q264 plan §2a/§2b): auto-assign under an
// advertised capacity, the GHES-style JobAvailable→acquire flow, cursor-based
// at-least-once message replay (including replay to a re-created session),
// message-queue token expiry, claim-once acquisition, and the queue's long poll —
// an empty poll is held until a message lands or the poll window elapses, so a client
// looping on it does not spin (see DefaultPollTimeout).
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
	"strings"
	"sync"
	"time"

	"github.com/actions-gateway/github-actions-gateway/scaleset"
)

// DefaultPollTimeout is how long the stub holds an empty poll open before answering
// 202 ("nothing to deliver") — the long-poll window. The real message queue blocks
// ~50s; the stub uses a far shorter window so a test that *wants* to observe a 202
// (a capacity-gated poll, say) does not stall, while an idle listener still issues
// ~1 request per second instead of hot-looping (Q287).
//
// The window is only a ceiling: a poll parked on an empty queue wakes the instant the
// queue changes (a job enqueued, acquired, or completed, or the session dropped), so
// message delivery stays immediate and tests stay fast. Override with SetPollTimeout.
const DefaultPollTimeout = time.Second

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

	// conflictJITNames holds exact runner names generatejitconfig rejects with 409, and
	// conflictJITPrefixes holds name prefixes it always rejects — the levers for the
	// Q270 runner-name-conflict path (a stale registered runner name).
	conflictJITNames    map[string]bool
	conflictJITPrefixes []string

	// runnerIDs maps a registered runner name to its REST id, and busyRunners marks
	// names whose REST DELETE answers 422 "still running a job" — the levers for the
	// Q334 deregister-on-conflict path. FailJITConfigName registers a stale runner here
	// so the client can resolve and delete it; a successful delete clears the matching
	// conflictJITNames entry so the re-registration under the base name then succeeds.
	runnerIDs    map[string]int64
	busyRunners  map[string]bool
	nextRunnerID int64

	// rateLimitPolls makes every message poll answer 429 (no Retry-After) while set —
	// the lever for the sustained-rate-limit condition path (Q325).
	rateLimitPolls bool
	// failSessionRefresh makes session refresh (PATCH) answer 401 while set, and
	// failSessionCreate does the same for session create (POST) — modelling revoked
	// credentials at each call site, the levers for the Degraded/Unauthorized
	// condition paths (Q325).
	failSessionRefresh bool
	failSessionCreate  bool

	adminToken    string
	adminTokenTTL time.Duration

	// pollTimeout bounds how long an empty poll is held open; zero answers 202 at once.
	pollTimeout time.Duration
	// wake is closed (and replaced) whenever the queue state changes, broadcasting to
	// every parked poll so it re-evaluates. Guarded by mu.
	wake chan struct{}
	// closed is closed by Close, releasing any parked poll so httptest's Close — which
	// waits for outstanding requests — cannot hang behind a long-poll.
	closed    chan struct{}
	closeOnce sync.Once

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
	deleteRunnerCalls       int
}

// New creates and starts a stub in the default dotcom auto-assign mode.
func New() *Server {
	s := &Server{
		adminTokenTTL:    time.Hour,
		pollTimeout:      DefaultPollTimeout,
		wake:             make(chan struct{}),
		closed:           make(chan struct{}),
		scaleSets:        make(map[int]*scaleSet),
		nextReqID:        1000,
		conflictJITNames: make(map[string]bool),
		runnerIDs:        make(map[string]int64),
		busyRunners:      make(map[string]bool),
		nextRunnerID:     5000,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orgs/{org}/actions/runners/registration-token", s.handleRegistrationToken)
	mux.HandleFunc("POST /repos/{owner}/{repo}/actions/runners/registration-token", s.handleRegistrationToken)
	mux.HandleFunc("GET /orgs/{org}/actions/runners", s.handleListRunners)
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runners", s.handleListRunners)
	mux.HandleFunc("DELETE /orgs/{org}/actions/runners/{rid}", s.handleDeleteRunner)
	mux.HandleFunc("DELETE /repos/{owner}/{repo}/actions/runners/{rid}", s.handleDeleteRunner)
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

// Close shuts down the stub server. It first releases any parked long-poll, because
// httptest.Server.Close waits for outstanding requests to finish and would otherwise
// block for a whole poll window. Safe to call more than once.
func (s *Server) Close() {
	s.closeOnce.Do(func() { close(s.closed) })
	s.server.Close()
}

// SetPollTimeout overrides how long an empty poll is held open before the stub answers
// 202 (DefaultPollTimeout). Zero restores the non-blocking behavior — a poll with
// nothing to deliver returns 202 immediately — which a test asserting that response
// directly wants, but which makes a polling client hot-loop (Q287). Takes effect on the
// next poll.
func (s *Server) SetPollTimeout(d time.Duration) {
	s.mu.Lock()
	s.pollTimeout = d
	s.mu.Unlock()
}

// notifyLocked broadcasts a queue-state change to every parked poll. Caller holds s.mu.
func (s *Server) notifyLocked() {
	close(s.wake)
	s.wake = make(chan struct{})
}

// EnableGHESAcquireFlow switches the stub to the GHES path: queued jobs are offered
// as JobAvailable and the client must claim them with acquirejobs (auto-assign is
// the default). Call before enqueuing jobs.
func (s *Server) EnableGHESAcquireFlow() {
	s.mu.Lock()
	s.ghesAcquireFlow = true
	s.mu.Unlock()
}

// FailJITConfigName makes generatejitconfig reject the exact runner name with 409
// Conflict, modelling a stale registered runner name. It also registers a REST runner
// record under that name (a resolvable id) so the listener's Q334 recovery can delete
// it: a successful DELETE clears this conflict, so the re-registration under the same
// base name then succeeds. Failing only the base name (and not its suffixed retries)
// models a conflict the listener clears — by deleting the stale record and reusing the
// base name (Q334), or by retrying under a fresh suffixed name (Q270) if it cannot.
func (s *Server) FailJITConfigName(name string) {
	s.mu.Lock()
	s.conflictJITNames[name] = true
	if _, ok := s.runnerIDs[name]; !ok {
		s.nextRunnerID++
		s.runnerIDs[name] = s.nextRunnerID
	}
	s.mu.Unlock()
}

// SetRunnerBusy makes the REST DELETE for the runner registered under name answer 422
// "is still running a job" (surfaced as *scaleset.RunnerBusyError) while set, modelling
// a live runner that must not be deleted — the lever for the Q334 busy-record path.
func (s *Server) SetRunnerBusy(name string) {
	s.mu.Lock()
	s.busyRunners[name] = true
	s.mu.Unlock()
}

// FailJITConfigNamePrefix makes generatejitconfig always reject any runner name with
// the given prefix with 409 Conflict, modelling a *persistent* conflict even fresh-name
// retries cannot clear (the base name and every numeric-suffixed retry share the
// prefix). Scoped to one job's names, it proves a permanently stuck assignment does not
// wedge the queue cursor behind it, so the other jobs still provision (Q270).
func (s *Server) FailJITConfigNamePrefix(prefix string) {
	s.mu.Lock()
	s.conflictJITPrefixes = append(s.conflictJITPrefixes, prefix)
	s.mu.Unlock()
}

// jitConfigConflicts reports whether generatejitconfig should 409 for name. Caller
// holds s.mu.
func (s *Server) jitConfigConflicts(name string) bool {
	if s.conflictJITNames[name] {
		return true
	}
	for _, p := range s.conflictJITPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// SetRateLimitPolls makes every message poll answer 429 Too Many Requests (no
// Retry-After header — the common case, §2a-5) while on, modelling a sustained rate
// limit — the lever for the RateLimited condition path (Q325). Parked polls wake and
// 429 immediately.
func (s *Server) SetRateLimitPolls(on bool) {
	s.mu.Lock()
	s.rateLimitPolls = on
	s.notifyLocked()
	s.mu.Unlock()
}

// FailSessionRefresh makes session refresh (PATCH sessions/{sid}) answer 401
// Unauthorized while on, modelling credentials revoked after the session opened —
// combined with ExpireQueueToken it drives the poll-401 → refresh-401 path that must
// surface Degraded/Unauthorized (Q325).
func (s *Server) FailSessionRefresh(on bool) {
	s.mu.Lock()
	s.failSessionRefresh = on
	s.mu.Unlock()
}

// FailSessionCreate makes session create (POST sessions) answer 401 Unauthorized
// while on, modelling revoked credentials at session creation — the lever for the
// Start-path and session-re-create Degraded/Unauthorized paths (Q325).
func (s *Server) FailSessionCreate(on bool) {
	s.mu.Lock()
	s.failSessionCreate = on
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
	// A queued job produces no message until a poll re-evaluates it against the
	// advertised capacity, so wake the parked polls to do exactly that.
	s.notifyLocked()
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
			s.notifyLocked()
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
		// A parked poll authorized under the old token must wake and 401 now.
		s.notifyLocked()
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
		// A parked poll on the now-dead session must wake and 404 now, so the client
		// re-creates the session promptly rather than after the poll window.
		s.notifyLocked()
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

// DeleteRunnerCalls returns how many REST delete-runner calls the stub served — the
// Q334 deregister-on-conflict recovery drives one per reclaimed stale record.
func (s *Server) DeleteRunnerCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteRunnerCalls
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
	if s.failSessionCreate {
		http.Error(w, `{"message":"credentials revoked"}`, http.StatusUnauthorized)
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
	if s.failSessionRefresh {
		http.Error(w, `{"message":"credentials revoked"}`, http.StatusUnauthorized)
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
		s.notifyLocked() // release any poll still parked on the deleted session
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
	if s.jitConfigConflicts(in.Name) {
		// A stale runner already holds this name — the runner-name 409 the client maps to
		// *RunnerNameConflictError (Q270), distinct from a session-create conflict.
		http.Error(w, `{"message":"runner name already exists"}`, http.StatusConflict)
		return
	}
	blob := base64.StdEncoding.EncodeToString([]byte(`{".runner":{},".credentials":{}}`))
	out := scaleset.JITRunnerConfig{EncodedJITConfig: blob}
	out.Runner.ID = 77
	out.Runner.Name = in.Name
	_ = json.NewEncoder(w).Encode(out)
}

// handleListRunners serves the REST list-runners name filter used by
// Client.DeregisterRunnerByName to resolve a stale record's id (Q334). It returns the
// registered runner for an exact name match, or an empty list otherwise.
func (s *Server) handleListRunners(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.URL.Query().Get("name")
	s.record("list-runners name=%s", name)
	type restRunner struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	var runners []restRunner
	if id, ok := s.runnerIDs[name]; ok {
		runners = append(runners, restRunner{ID: id, Name: name})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(runners), "runners": runners})
}

// handleDeleteRunner serves the REST DELETE used by Client.DeregisterRunnerByName. A
// runner marked busy answers 422 (a live runner that must not be deleted); otherwise the
// record is removed and any matching generatejitconfig conflict is cleared, so a
// re-registration under the same base name then succeeds (Q334).
func (s *Server) handleDeleteRunner(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rid, _ := strconv.ParseInt(r.PathValue("rid"), 10, 64)
	s.record("delete-runner id=%d", rid)
	s.deleteRunnerCalls++
	name := ""
	for n, id := range s.runnerIDs {
		if id == rid {
			name = n
			break
		}
	}
	if name == "" {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		return
	}
	if s.busyRunners[name] {
		http.Error(w, `{"message":"This runner is still running a job and cannot be deleted"}`, http.StatusUnprocessableEntity)
		return
	}
	delete(s.runnerIDs, name)
	delete(s.conflictJITNames, name)
	w.WriteHeader(http.StatusNoContent)
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
	} else {
		s.notifyLocked() // the JobAssigned messages just appended are deliverable now
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"count": len(won), "value": won})
}

// handlePoll serves the message queue's long poll. It re-evaluates assignment against
// the capacity this request advertised, and if that yields nothing deliverable it parks
// the request — waking on any queue-state change (notifyLocked) to re-evaluate, and
// answering 202 only once the poll window elapses. Returning 202 immediately, as the
// stub used to, makes a polling client re-poll with no pause: ~5,000 requests/second per
// listener, which burns CI CPU and amplifies timing flakes (Q287). The real queue blocks
// ~50s; SetPollTimeout(0) restores the non-blocking behavior for a test that asserts the
// 202 directly.
//
// The advertised capacity is fixed for the life of one request, exactly as on the real
// backend — a client whose free-slot count grows mid-poll only advertises the new value
// on its next poll, so the window also bounds how stale that value can get.
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	capacity, _ := strconv.Atoi(r.Header.Get("X-ScaleSetMaxCapacity"))
	last, _ := strconv.ParseInt(r.URL.Query().Get("lastMessageId"), 10, 64)

	s.mu.Lock()
	// Record once per request, not once per wake, so Calls() counts HTTP polls.
	s.record("poll id=%d cap=%d last=%d", id, capacity, last)
	s.mu.Unlock()

	// The poll window starts on the first evaluation that finds nothing to deliver, and
	// spans every subsequent wake — so a stream of no-op wakes cannot extend it.
	var timer *time.Timer
	var deadline <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		s.mu.Lock()
		if s.rateLimitPolls {
			s.mu.Unlock()
			http.Error(w, `{"message":"rate limited"}`, http.StatusTooManyRequests)
			return
		}
		ss := s.scaleSets[id]
		if ss == nil || ss.session == nil {
			s.mu.Unlock()
			http.Error(w, `{"message":"session not found"}`, http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+ss.session.queueToken {
			s.mu.Unlock()
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

		if msg := ss.nextMessageAfter(last); msg != nil {
			out := s.encodeMessage(ss, msg)
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(out)
			return
		}

		// Nothing to deliver. Snapshot the wake channel before releasing the lock so a
		// change landing in the gap still wakes us.
		wake := s.wake
		timeout := s.pollTimeout
		s.mu.Unlock()

		if timeout <= 0 {
			w.WriteHeader(http.StatusAccepted) // long-polling disabled
			return
		}
		if deadline == nil { // start the window on the first empty evaluation
			timer = time.NewTimer(timeout)
			deadline = timer.C
		}

		select {
		case <-wake: // queue state changed — re-evaluate
		case <-deadline:
			w.WriteHeader(http.StatusAccepted) // 202 — window elapsed, nothing to deliver
			return
		case <-s.closed:
			w.WriteHeader(http.StatusAccepted) // stub shutting down; release the client
			return
		case <-r.Context().Done():
			return // client hung up (its ctx was cancelled)
		}
	}
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
