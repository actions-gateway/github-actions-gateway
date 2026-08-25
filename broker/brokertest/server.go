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
	"github.com/actions-gateway/github-actions-gateway/broker/brokerstub"
)

// Server is a test HTTP server that implements the broker v2 protocol endpoints.
type Server struct {
	URL    string
	server *httptest.Server

	// sessions is the shared session registry (minting, bearer-DELETE
	// resolution, owner-scoped listing, and the "#POST − #DELETE" active count).
	sessions *brokerstub.Sessions

	mu                 sync.Mutex
	tokenCounter       atomic.Int64
	deleted            map[string]bool                         // sessionID → DELETE resolved (guarded by mu; backs WaitForSessionDelete)
	deletedSessions    map[string]chan struct{}                // sessionID → closed on DELETE
	firstPollNotify    map[string]chan struct{}                // sessionID → closed on first GET /message
	jobQueues          map[string]chan broker.TaskAgentMessage // sessionID → messages
	failSessionOwner   string                                  // when non-empty, 401 POST /session for owners with this prefix
	acquireJobResponse any                                     // custom AcquireJob response; nil uses default
	acquireCount       atomic.Int64
	ackCount           atomic.Int64
	renewJobCount      atomic.Int64
	completeJobCount   atomic.Int64
	lastCompleteJob    atomic.Value // broker.CompleteJobRequest of the most recent completejob
	msgCounter         atomic.Int64
	getMessageCount    atomic.Int64

	// Fan-out job-accounting model (Q260). Off by default so existing tests are
	// unaffected; EnableFanoutAccounting turns it on. It models the one property
	// the default stub omits and the one that wedged production: GitHub fans a
	// single logical job (one planID) out to N sibling sessions as N deliveries
	// with DISTINCT RunnerRequestIDs, and the job only concludes when its
	// per-delivery accounting is reconciled — completing a single sibling's own
	// delivery does NOT conclude the job, and any acquired-but-unresolved sibling
	// delivery cancels the whole job at the ~15-minute unstarted-job timeout. See
	// the accounting methods below.
	acctMu    sync.Mutex
	fanout    bool
	jobs      map[string]*fanoutJob // planID → logical job
	reqToPlan map[string]string     // delivery RunnerRequestID → planID
}

// fanoutDelivery is one per-delivery assignment of a logical fan-out job — one
// RunnerRequestID GitHub minted for the job's planID (Q260).
type fanoutDelivery struct {
	reqID    string
	handed   bool              // returned to a poller via GET /message
	acquired bool              // POST /acquirejob seen for this delivery
	result   broker.TaskResult // "" until POST /completejob resolves it
}

// fanoutJob is the accounting state of one logical job (planID) across all the
// sibling deliveries GitHub fanned it out to (Q260).
type fanoutJob struct {
	planID     string
	deliveries []*fanoutDelivery
	state      string // "queued" | "in_progress" | "completed" | "failed" | "cancelled"
}

// New creates and starts a new broker Stub. Call Close when done.
func New() *Server {
	s := &Server{
		sessions:        brokerstub.NewSessions(),
		deleted:         make(map[string]bool),
		deletedSessions: make(map[string]chan struct{}),
		firstPollNotify: make(map[string]chan struct{}),
		jobQueues:       make(map[string]chan broker.TaskAgentMessage),
		jobs:            make(map[string]*fanoutJob),
		reqToPlan:       make(map[string]string),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", s.handleToken)
	mux.HandleFunc("/session", s.handleSession)
	mux.HandleFunc("/message", s.handleMessage)
	mux.HandleFunc("/acquirejob", s.handleAcquireJob)
	mux.HandleFunc("/renewjob", s.handleRenewJob)
	mux.HandleFunc("/completejob", s.handleCompleteJob)
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
	return s.sessions.ActiveIDs("")
}

// ActiveSessionsForOwner returns the IDs of currently-active sessions owned by the
// runner name of the given stem. A listener owns its session as its own registered
// runner name, so a session matches when its ownerName is that stem, a "-", and a
// decimal index. The index segment is matched exactly rather than by prefix, so a
// stem that extends this one ("<name>-set") keeps its own bucket. Scoping by owner
// lets a test assert on only its own CR's sessions, immune to sessions other tests
// left active on this shared stub — the global RegisteredSessions/ActiveSessionCount
// counters accumulate across the whole package and cause cross-test flakes when used
// for exact-count assertions.
//
// The stem is the registered name's, not the CR's, and the two differ by kind: pass
// "<set>" for a RunnerGroup and "rs-<set>" for a RunnerSet, matching what Q466
// kind-scoped and Q677 carried onto the wire. A same-named group and set are now
// separable, which they were not before Q677.
func (s *Server) ActiveSessionsForOwner(name string) []string {
	prefix := name + "-"
	ids := s.sessions.ActiveIDs(prefix)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		sess, ok := s.sessions.Get(id)
		if !ok {
			continue
		}
		if isAgentIndex(strings.TrimPrefix(sess.Owner, prefix)) {
			out = append(out, id)
		}
	}
	return out
}

// isAgentIndex reports whether s is the agentIndex segment of an ownerName:
// non-empty and all decimal digits, as fmt.Sprintf("%s-%d", …) emits it.
func isAgentIndex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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
	// Fast path: DELETE already resolved before this call. The delete signal is
	// tracked under mu (alongside the notify channel) so the check-and-register
	// is atomic with handleSession's close — the session identity itself lives
	// in the shared registry under its own lock.
	if s.deleted[sessionID] {
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

// AcquireJobCalls returns the number of /acquirejob calls the stub has fully
// served — like CompleteJobCalls, the counter is published only after the call's
// fan-out accounting is committed, so waiting on it and then reading the
// accounting is race-free.
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

// GetMessageCalls returns the number of GET /message polls served. The stub
// answers 202 at once rather than holding the poll, so a caller with no pacing
// of its own shows up here as a request storm — the rate a poll-loop test
// measures against.
func (s *Server) GetMessageCalls() int {
	return int(s.getMessageCount.Load())
}

// RenewJobCalls returns the number of times /renewjob was called.
func (s *Server) RenewJobCalls() int {
	return int(s.renewJobCount.Load())
}

// CompleteJobCalls returns the number of /completejob calls the stub has FULLY
// SERVED — the counter is published only after the call's effects are committed,
// so waiting on it and then reading LastCompleteJob or the fan-out accounting is
// race-free. The AGC issues completejob for a deduplicated duplicate delivery it
// abandons (Q260 follow-up), so a test can assert the loser released its dangling
// assignment.
//
// It counts calls, not resolved deliveries: a call whose body never arrives (the
// client's context was cancelled mid-request) is served and counted, yet resolves
// nothing. Assert on DeliveryResults when what you mean is "these deliveries are
// resolved" (Q490).
func (s *Server) CompleteJobCalls() int {
	return int(s.completeJobCount.Load())
}

// LastCompleteJob returns the request body of the most recent /completejob call,
// and false if none has been received. AuthToken is never populated (the client
// sends it as a header, not in the body).
func (s *Server) LastCompleteJob() (broker.CompleteJobRequest, bool) {
	v := s.lastCompleteJob.Load()
	if v == nil {
		return broker.CompleteJobRequest{}, false
	}
	return v.(broker.CompleteJobRequest), true
}

// EnableFanoutAccounting turns on the per-delivery fan-out job-accounting model
// (Q260). Off by default. Call once before enqueuing a fan-out job. When on, the
// server tracks a logical job per planID with one assignment per delivery and only
// concludes the job when the accounting is reconciled — modeling GitHub's real
// fan-out completion semantics that the default stub omits (the gap that let the
// Q260 dedup pass envtest yet wedge production).
func (s *Server) EnableFanoutAccounting() {
	s.acctMu.Lock()
	s.fanout = true
	s.acctMu.Unlock()
}

// EnqueueFanoutJob registers one logical job (planID) that GitHub fans out to n
// sibling sessions as n deliveries with DISTINCT RunnerRequestIDs. The deliveries
// are handed to pollers on GET /message (one per poll, to whichever sessions poll),
// so a burst of n concurrent pollers each receives one delivery of the same job —
// exactly the shape the planID dedup must collapse. Returns the n RunnerRequestIDs.
// Requires EnableFanoutAccounting. Safe to call once per planID.
func (s *Server) EnqueueFanoutJob(planID string, n int) []string {
	s.acctMu.Lock()
	defer s.acctMu.Unlock()
	job := &fanoutJob{planID: planID, state: "queued"}
	reqIDs := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		reqID := fmt.Sprintf("%s-d%d", planID, i)
		job.deliveries = append(job.deliveries, &fanoutDelivery{reqID: reqID})
		s.reqToPlan[reqID] = planID
		reqIDs = append(reqIDs, reqID)
	}
	s.jobs[planID] = job
	return reqIDs
}

// DeliveryResults returns, for the fan-out job identified by planID, each delivery's
// resolved completejob result keyed by its RunnerRequestID — only deliveries a
// completejob has resolved appear. Empty when the job is unknown or none resolved.
// It lets a test assert the winner completed each deduped sibling delivery keyed on
// its OWN RunnerRequestID with the expected result (Q260 Option A). Read-only.
func (s *Server) DeliveryResults(planID string) map[string]broker.TaskResult {
	s.acctMu.Lock()
	defer s.acctMu.Unlock()
	out := make(map[string]broker.TaskResult)
	if job, ok := s.jobs[planID]; ok {
		for _, d := range job.deliveries {
			if d.result != "" {
				out[d.reqID] = d.result
			}
		}
	}
	return out
}

// JobState returns the accounting state of the logical fan-out job: "queued",
// "in_progress", "completed", "failed", or "cancelled" (or "" if unknown). See
// EnableFanoutAccounting.
func (s *Server) JobState(planID string) string {
	s.acctMu.Lock()
	defer s.acctMu.Unlock()
	if job, ok := s.jobs[planID]; ok {
		return job.state
	}
	return ""
}

// ExpireUnstartedDeliveries fires GitHub's ~15-minute unstarted-job timeout
// deterministically (no real timer): if any delivery of the job was acquired but
// never resolved with a terminal result, the whole job is CANCELLED — GitHub is
// still waiting on that phantom assignment even though a sibling already ran the
// job. A no-op once the job has concluded. This is the mechanism that turns the
// Q260 dedup's silently-abandoned sibling deliveries into a cancelled job.
func (s *Server) ExpireUnstartedDeliveries(planID string) {
	s.acctMu.Lock()
	defer s.acctMu.Unlock()
	if job, ok := s.jobs[planID]; ok {
		s.reconcileLocked(job, true /* expire */)
	}
}

// reconcileLocked recomputes a job's state from its deliveries. Caller holds acctMu.
//
// The model encodes the invariant observed live in re-route #4: a fan-out job
// concludes only when EVERY acquired delivery is resolved with a consistent, real
// (non-skipped) terminal result. Completing a single sibling's own delivery is not
// enough — the other acquired-but-unresolved siblings keep the job open, and at the
// unstarted timeout (expire=true) they cancel it. A delivery resolved as "skipped"
// (the #513 dead-end) acks the assignment but contributes no real conclusion, so a
// skipped-contaminated job never goes green — matching the live flag-ON result
// (indefinite in_progress rather than completed).
func (s *Server) reconcileLocked(job *fanoutJob, expire bool) {
	switch job.state {
	case "completed", "failed", "cancelled":
		return // terminal
	}
	danglingAcquired := false
	var results []broker.TaskResult
	for _, d := range job.deliveries {
		if !d.acquired {
			continue
		}
		if d.result == "" {
			danglingAcquired = true
		} else {
			results = append(results, d.result)
		}
	}
	if danglingAcquired {
		if expire {
			job.state = "cancelled"
		} else {
			job.state = "in_progress"
		}
		return
	}
	if len(results) == 0 {
		return // nothing acquired yet (or nothing resolved) — leave queued/in_progress
	}
	// Every acquired delivery is resolved. Decide the conclusion from the results.
	allSkipped, hasFailed, hasSucceeded := true, false, false
	for _, r := range results {
		if r != broker.TaskResultSkipped {
			allSkipped = false
		}
		switch r {
		case broker.TaskResultFailed:
			hasFailed = true
		case broker.TaskResultSucceeded:
			hasSucceeded = true
		}
	}
	switch {
	case allSkipped:
		job.state = "in_progress" // skipped-only never concludes (models #513)
	case hasFailed:
		job.state = "failed"
	case hasSucceeded:
		job.state = "completed"
	default:
		job.state = "in_progress"
	}
}

// fanoutMessage pops the next un-handed delivery of any pending fan-out job and
// returns it as a broker message, or (nil,false) if none is pending. Caller must
// NOT hold acctMu. The RunServiceURL points back at this stub so /acquirejob and
// /completejob for the delivery return here.
func (s *Server) fanoutMessage() (broker.TaskAgentMessage, bool) {
	s.acctMu.Lock()
	var picked *fanoutDelivery
	for _, job := range s.jobs {
		for _, d := range job.deliveries {
			if !d.handed {
				picked = d
				break
			}
		}
		if picked != nil {
			break
		}
	}
	if picked != nil {
		picked.handed = true
	}
	s.acctMu.Unlock()
	if picked == nil {
		return broker.TaskAgentMessage{}, false
	}
	body, _ := json.Marshal(broker.RunnerJobRequestBody{
		RunnerRequestID: picked.reqID,
		RunServiceURL:   strings.TrimRight(s.URL, "/"),
	})
	return broker.TaskAgentMessage{
		MessageID:   s.msgCounter.Add(1),
		MessageType: "RunnerJobRequest",
		Body:        string(body),
	}, true
}

// ActiveSessionCount returns the number of goroutines that have registered a session
// but not yet called DELETE /session. It is computed as (#POST /session − #DELETE /session)
// so each listener goroutine contributes +1 on start and −1 on exit, regardless of v2 mode.
func (s *Server) ActiveSessionCount() int {
	return s.sessions.ActiveCount()
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
// ("<CR name>-<agentIndex>"), so passing "<CR name>-" scopes it to one CR's pool.
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
	brokerstub.WriteToken(w, token)
}

// handleSession serves POST /session (create) and DELETE /session (delete).
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		bearer := brokerstub.Bearer(r)

		// Parse ownerName ("<CR name>-<agentIndex>") so tests can scope session
		// assertions to one CR via ActiveSessionsForOwner. Best-effort: a missing
		// or unparsable body simply leaves the owner empty.
		var reqBody struct {
			OwnerName string `json:"ownerName"`
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)

		// Simulate a broker that rejects this owner's session creation (e.g. a
		// consumed single-use credential). createSession maps 401 to a
		// NonRetriableError, so the listener's permanent baseline exits and is not
		// auto-restarted — the precondition for the controller's baseline-revival
		// path (Q137). Scoped by owner prefix so one test's failure injection does
		// not affect other RunnerGroups sharing this stub.
		s.mu.Lock()
		fail := s.failSessionOwner
		s.mu.Unlock()
		if fail != "" && strings.HasPrefix(reqBody.OwnerName, fail) {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		sessionID := s.sessions.Create(reqBody.OwnerName, 0, "", bearer)
		brokerstub.WriteJSON(w, http.StatusOK, map[string]string{"sessionId": sessionID})

	case http.MethodDelete:
		// Identify the session by the sessionId query param (v1) or the Bearer
		// token (v2, per-goroutine unique) and mark it deleted.
		sessionID, _ := s.sessions.ResolveDelete(r.URL.Query().Get("sessionId"), brokerstub.Bearer(r))

		// Each goroutine calls DELETE exactly once on exit; count it toward the
		// per-goroutine "#POST − #DELETE" total regardless of v2 vs v1 mode.
		// Counted after the registry flip so a waiter on ActiveSessionCount sees
		// the session already inactive, and before the delete-notify close so a
		// WaitForSessionDelete waiter sees both.
		s.sessions.CountDelete()

		if sessionID != "" {
			s.mu.Lock()
			s.deleted[sessionID] = true
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
// notifyFirstPollLocked closes the WaitForFirstPoll channel for sessionID on its
// first GET /message, or records the poll as already seen when no waiter has
// registered yet. Caller holds s.mu.
func (s *Server) notifyFirstPollLocked(sessionID string) {
	if pollCh, known := s.firstPollNotify[sessionID]; known {
		select {
		case <-pollCh: // already closed — nothing to do
		default:
			close(pollCh)
		}
		return
	}
	closedCh := make(chan struct{})
	close(closedCh)
	s.firstPollNotify[sessionID] = closedCh
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessionID := r.URL.Query().Get("sessionId")
	s.getMessageCount.Add(1)

	// Fan-out accounting (Q260): hand the next pending fan-out delivery to whichever
	// session polls, one per poll. Takes precedence over the per-session queue so a
	// burst of concurrent pollers each receives one delivery of the same logical job.
	s.acctMu.Lock()
	fanoutOn := s.fanout
	s.acctMu.Unlock()
	if fanoutOn {
		s.mu.Lock()
		s.notifyFirstPollLocked(sessionID)
		s.mu.Unlock()
		if msg, ok := s.fanoutMessage(); ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(msg)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	s.mu.Lock()
	ch, ok := s.jobQueues[sessionID]
	s.notifyFirstPollLocked(sessionID)
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

// handleCompleteJob serves POST /completejob — records the call and its request
// body, and replies 200. The AGC issues this only to release a deduplicated
// duplicate delivery's assignment (Q260 follow-up).
func (s *Server) handleCompleteJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req broker.CompleteJobRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.lastCompleteJob.Store(req)

	// Fan-out accounting (Q260): resolve this delivery with its reported result and
	// recompute the logical job's state. A single sibling's completion does not
	// conclude the job while other acquired deliveries remain unresolved.
	s.acctMu.Lock()
	if s.fanout {
		if planID, ok := s.reqToPlan[req.JobID]; ok {
			if job, ok := s.jobs[planID]; ok {
				for _, d := range job.deliveries {
					if d.reqID == req.JobID {
						d.result = req.Result
						break
					}
				}
				s.reconcileLocked(job, false)
			}
		}
	}
	s.acctMu.Unlock()

	// Publish the call LAST, after every piece of state this handler records is
	// committed, so the counter is a valid happens-before gate: a test that waits on
	// CompleteJobCalls() and then reads LastCompleteJob or the fan-out accounting is
	// guaranteed to see this call's effects. Incrementing first let a waiter observe
	// the count and then read state the handler had not written yet — the Q490 flake,
	// where the winner's Nth sibling completion was counted but not yet resolved when
	// ExpireUnstartedDeliveries ran, cancelling a job every delivery had completed.
	s.completeJobCount.Add(1)

	w.WriteHeader(http.StatusOK)
}

// handleAcquireJob serves POST /acquirejob — returns a synthetic AcquireJob response.
func (s *Server) handleAcquireJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	// Fan-out accounting (Q260): map this delivery (jobMessageId == RunnerRequestID)
	// to its logical job, mark the delivery acquired, and return the SHARED planID —
	// so every sibling that acquires its own distinct delivery resolves to one planID
	// (the collision the dedup must collapse). The response embeds runnerRequestId so
	// a worker-simulating JobHandler can complete this exact delivery.
	s.acctMu.Lock()
	fanoutOn := s.fanout
	s.acctMu.Unlock()
	if fanoutOn {
		var req broker.JobAcquisitionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.acctMu.Lock()
		planID := s.reqToPlan[req.JobMessageID]
		if job, ok := s.jobs[planID]; ok {
			for _, d := range job.deliveries {
				if d.reqID == req.JobMessageID {
					d.acquired = true
					break
				}
			}
			s.reconcileLocked(job, false)
		}
		s.acctMu.Unlock()
		// Publish the counter only after the delivery's acquired flag and the job
		// state are committed (the handleCompleteJob / Q490 rule), so a waiter on
		// AcquireJobCalls is a valid happens-before gate for the accounting.
		s.acquireCount.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan":            map[string]string{"planId": planID},
			"runnerRequestId": req.JobMessageID,
			"resources": map[string]any{
				"endpoints": []map[string]any{{
					"name":          "SystemVssConnection",
					"authorization": map[string]any{"parameters": map[string]string{"AccessToken": "job-token-" + req.JobMessageID}},
				}},
			},
		})
		return
	}

	s.mu.Lock()
	custom := s.acquireJobResponse
	s.mu.Unlock()

	n := s.acquireCount.Add(1)
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
