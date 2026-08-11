// Package scalesetstub is the single implementation of GitHub's
// runner-scale-set protocol shared by the in-repo scale-set test doubles. It
// encodes the semantics the live probes settled (Q264 plan §2a/§2b):
// auto-assign under an advertised capacity, the GHES-style JobAvailable→acquire
// flow, cursor-based at-least-once message replay (including replay to a
// re-created session), message-queue token expiry, claim-once acquisition, and
// the queue's long poll — an empty poll is held until a message lands or the
// poll window elapses, so a client looping on it does not spin (see
// DefaultPollTimeout).
//
// One handler serves the whole chain — the REST registration-token call, the
// RemoteAuth runner-registration hop, and the Actions Service (runnergroups,
// runnerscalesets CRUD, sessions, the message queue, acquirejobs,
// generatejitconfig) — with self-referential URLs, mirroring the real backend's
// broker-host tenant.
//
// The package carries no transport of its own, so both doubles can build on it:
// scaleset/scalesettest wraps it in an httptest.Server for the unit and envtest
// suites, and test/fakegithub mounts it into the deployed fake-GitHub image.
// That second consumer is why the httptest server lives in the wrapper rather
// than here — fakegithub is package main, and no package main in the workspace
// may link net/http/httptest (cmd/probe/compat). It is the same split
// broker/brokerstub makes for the classic protocol.
package scalesetstub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
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
	jobCompleted                 // terminal, with a JobCompleted delivered
	jobDropped                   // terminal, with nothing delivered (see DropAssignedJob)
)

// terminal reports whether a job has ended, however it ended. The two terminal states
// differ only in what the client was told, which is exactly the distinction a test of
// the client's dangling-assignment handling needs.
func (s jobState) terminal() bool { return s == jobCompleted || s == jobDropped }

// job is one unit of work queued on a scale set.
type job struct {
	reqID  int64
	jobID  string
	state  jobState
	result string
	// Run identity, delivered on every job message by the protocol's JobMessageBase.
	// The fake populates it because the real backend does: a client that reads
	// ownerName/repositoryName/workflowRunId off an assignment (scale-set eviction
	// recovery does — Q417) would otherwise be exercised against a fake that answers
	// with fields the wire actually carries left empty, and the gap would only surface
	// live. A test that needs the no-identity degrade path seeds a raw body instead
	// (SeedRawMessage).
	owner string
	repo  string
	runID int64
}

// The identity the fake stamps on every enqueued job. Exported so a test can assert
// against the same values the fake will deliver rather than restating them.
const (
	DefaultJobOwner      = "acme"
	DefaultJobRepository = "widgets"
)

// qmessage is one message in a scale set's queue log. The log is scale-set-scoped
// (not session-scoped), so it survives a session delete/recreate — the property
// that gives cursor replay to a fresh session (§2b-3).
type qmessage struct {
	id      int64
	entries []scaleset.JobMessage
	// rawBody, when non-nil, is delivered verbatim as the message body instead of
	// the marshalled entries — see SeedRawMessage.
	rawBody *string
	deleted bool
}

// seedMessage is a message template SeedMessage/SeedRawMessage attach to every
// newly created scale set's queue log, ahead of anything the model generates.
type seedMessage struct {
	entries []scaleset.JobMessage
	rawBody *string
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
	labels   []scaleset.Label
	session  *session
	jobs     []*job
	messages []*qmessage
}

// Stub is the scale-set protocol model. Construct it with New; call Close when
// done to release any parked long poll.
type Stub struct {
	// baseURL resolves the externally reachable base the protocol's
	// self-referential URLs are built from — the admin connection's tenant URL, a
	// session's messageQueueUrl, and a JobAvailable's acquireJobUrl. It is per
	// request because a deployed stub is reached under whatever host the caller
	// dialled, while an httptest-wrapped one always answers with its fixed base.
	baseURL func(*http.Request) string
	handler http.Handler

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
	// onlineRunners marks names the listing reports as status "online" (a runner has
	// connected); every other registered name lists as "offline", which is what a
	// generatejitconfig record looks like until its worker starts. hiddenRunners marks
	// names the exact-name filter omits while the unfiltered listing still returns them
	// — the shape a record that DeregisterRunnerByName cannot resolve would have (Q550).
	runnerIDs     map[string]int64
	busyRunners   map[string]bool
	onlineRunners map[string]bool
	hiddenRunners map[string]bool
	nextRunnerID  int64

	// rateLimitPolls makes every message poll answer 429 (no Retry-After) while set —
	// the lever for the sustained-rate-limit condition path (Q325).
	rateLimitPolls bool
	// rateLimitRemaining, when positive, is echoed as X-RateLimit-Remaining on every
	// message-poll response — see SetRateLimitRemaining.
	rateLimitRemaining int
	// failRunnerGroups makes the runnergroups lookup answer 500 while set — see
	// FailRunnerGroups.
	failRunnerGroups bool
	// dropExtraScaleSetLabels makes a create keep only the scale set's name label
	// while set, without erroring — see DropExtraScaleSetLabels.
	dropExtraScaleSetLabels bool
	// failStaticAcquire makes the _apis/runtime acquirejobs route answer 404 while
	// set, leaving the queue-host route working — see FailStaticAcquireRoute.
	failStaticAcquire bool
	// installToken, when non-empty, is the installation token the REST
	// registration-token hop requires — see SetInstallationToken.
	installToken string
	// failSessionRefresh makes session refresh (PATCH) answer 401 while set, and
	// failSessionCreate does the same for session create (POST) — modelling revoked
	// credentials at each call site, the levers for the Degraded/Unauthorized
	// condition paths (Q325).
	failSessionRefresh bool
	failSessionCreate  bool
	// failDeleteMessageStatus, when non-zero, is the status the message-DELETE ack
	// answers instead of acting. The status matters rather than just the failure: the
	// client reports a 404/410 as a benign ack, so 404 models a backend that does not
	// serve the endpoint (invisible to a caller reading only the error) while a 5xx
	// models one that is momentarily unable to (Q583).
	failDeleteMessageStatus int
	// deleteWithoutPruning makes the ack answer 204 while leaving the message in the
	// log, modelling a backend that accepts the call and does not act on it — which a
	// caller reading only the status cannot tell from a real prune (Q583).
	deleteWithoutPruning bool

	adminToken    string
	adminTokenTTL time.Duration

	// pollTimeout bounds how long an empty poll is held open; zero answers 202 at once.
	pollTimeout time.Duration
	// wake is closed (and replaced) whenever the queue state changes, broadcasting to
	// every parked poll so it re-evaluates. Guarded by mu.
	wake chan struct{}
	// closed is closed by Close, releasing any parked poll so a wrapping server's
	// shutdown — which waits for outstanding requests — cannot hang behind a
	// long-poll.
	closed    chan struct{}
	closeOnce sync.Once

	// pendingJobs and pendingMessages are the pre-registration queue state every
	// newly created scale set inherits — see PrequeueJobs, SeedMessage.
	pendingJobs     []*job
	pendingMessages []seedMessage

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

// New creates a stub in the default dotcom auto-assign mode. baseURL resolves the
// externally reachable base for the protocol's self-referential URLs; a nil
// baseURL derives it from the request's Host over plain HTTP.
func New(baseURL func(*http.Request) string) *Stub {
	if baseURL == nil {
		baseURL = func(r *http.Request) string { return "http://" + r.Host }
	}
	s := &Stub{
		baseURL:          baseURL,
		adminTokenTTL:    time.Hour,
		pollTimeout:      DefaultPollTimeout,
		wake:             make(chan struct{}),
		closed:           make(chan struct{}),
		scaleSets:        make(map[int]*scaleSet),
		nextReqID:        1000,
		conflictJITNames: make(map[string]bool),
		runnerIDs:        make(map[string]int64),
		busyRunners:      make(map[string]bool),
		onlineRunners:    make(map[string]bool),
		hiddenRunners:    make(map[string]bool),
		nextRunnerID:     5000,
	}
	s.handler = s.buildMux()
	s.adminToken = s.mintAdminJWT(s.adminTokenTTL)
	return s
}

// Handler returns the stub's routes. Every pattern is an absolute path, so a
// consumer mounting a subset behind a prefix (fakegithub serves the two REST hops
// under /api/v3) must strip that prefix first.
func (s *Stub) Handler() http.Handler {
	return s.handler
}

func (s *Stub) buildMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orgs/{org}/actions/runners/registration-token", s.handleRegistrationToken)
	mux.HandleFunc("POST /repos/{owner}/{repo}/actions/runners/registration-token", s.handleRegistrationToken)
	mux.HandleFunc("GET /orgs/{org}/actions/runners", s.handleListRunners)
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runners", s.handleListRunners)
	mux.HandleFunc("DELETE /orgs/{org}/actions/runners/{rid}", s.handleDeleteRunner)
	mux.HandleFunc("DELETE /repos/{owner}/{repo}/actions/runners/{rid}", s.handleDeleteRunner)
	mux.HandleFunc("POST /repos/{owner}/{repo}/actions/runs/{runid}/cancel", s.handleCancelRun)
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
	mux.HandleFunc("GET /_apis/runtime/runnerscalesets/{id}/acquirablejobs", s.handleAcquirableJobs)
	mux.HandleFunc("GET /queue/{id}/message", s.handlePoll)
	mux.HandleFunc("DELETE /queue/{id}/message/{msgid}", s.handleDeleteMessage)
	mux.HandleFunc("POST /queue/{id}/acquirejobs", s.handleQueueHostAcquireJobs)
	return mux
}

// Close releases every parked long poll. A wrapping server must call it before its
// own shutdown, which waits for outstanding requests and would otherwise block for
// a whole poll window. Safe to call more than once.
func (s *Stub) Close() {
	s.closeOnce.Do(func() { close(s.closed) })
}

// SetPollTimeout overrides the empty-poll window (see DefaultPollTimeout). Zero
// makes an empty poll answer 202 immediately, for a test asserting that response
// directly. Takes effect on the next poll.
func (s *Stub) SetPollTimeout(d time.Duration) {
	s.mu.Lock()
	s.pollTimeout = d
	s.mu.Unlock()
}

// notifyLocked broadcasts a queue-state change to every parked poll. Caller holds s.mu.
func (s *Stub) notifyLocked() {
	close(s.wake)
	s.wake = make(chan struct{})
}

// EnableGHESAcquireFlow switches the stub to the GHES path: queued jobs are offered
// as JobAvailable and the client must claim them with acquirejobs (auto-assign is
// the default). Call before enqueuing jobs.
func (s *Stub) EnableGHESAcquireFlow() {
	s.SetGHESAcquireFlow(true)
}

// SetGHESAcquireFlow selects the acquire flow in either direction, for a consumer
// that drives both against one long-lived stub. Call before enqueuing jobs.
func (s *Stub) SetGHESAcquireFlow(on bool) {
	s.mu.Lock()
	s.ghesAcquireFlow = on
	s.mu.Unlock()
}

// FailJITConfigName makes generatejitconfig reject the exact runner name with 409
// Conflict, modelling a stale registered runner name. It also registers a REST runner
// record under that name (a resolvable id) so the listener's Q334 recovery can delete
// it: a successful DELETE clears this conflict, so the re-registration under the same
// base name then succeeds. Failing only the base name (and not its suffixed retries)
// models a conflict the listener clears — by deleting the stale record and reusing the
// base name (Q334), or by retrying under a fresh suffixed name (Q270) if it cannot.
func (s *Stub) FailJITConfigName(name string) {
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
func (s *Stub) SetRunnerBusy(name string) {
	s.mu.Lock()
	s.busyRunners[name] = true
	s.mu.Unlock()
}

// SetRunnerOnline makes the listing report name with status "online", modelling a
// runner that has connected. Every registered name is "offline" until then — the state
// a generatejitconfig record sits in while its worker pod is still Pending, which is
// why the start-up sweep cannot treat "offline" alone as sweepable (Q550).
func (s *Stub) SetRunnerOnline(name string) {
	s.mu.Lock()
	s.onlineRunners[name] = true
	s.mu.Unlock()
}

// HideRunnerFromNameFilter keeps name registered (and returned by the unfiltered
// listing) while making the exact-name filter report no match, so
// DeregisterRunnerByName resolves nothing and reports (false, nil). It models the
// suspected live behaviour behind Q550's accumulation: a record the per-name reclaim
// can never clear, leaving the sweep as the only path that collects it.
func (s *Stub) HideRunnerFromNameFilter(name string) {
	s.mu.Lock()
	s.hiddenRunners[name] = true
	s.mu.Unlock()
}

// RegisteredRunners returns the names currently registered at the stub, sorted. It is
// the assertion surface for the registration leak: a scale set that has finished its
// jobs and reaped its workers must leave none behind (Q550).
func (s *Stub) RegisteredRunners() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.runnerIDs))
	for n := range s.runnerIDs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// FailJITConfigNamePrefix makes generatejitconfig always reject any runner name with
// the given prefix with 409 Conflict, modelling a *persistent* conflict even fresh-name
// retries cannot clear (the base name and every numeric-suffixed retry share the
// prefix). Scoped to one job's names, it proves a permanently stuck assignment does not
// wedge the queue cursor behind it, so the other jobs still provision (Q270).
func (s *Stub) FailJITConfigNamePrefix(prefix string) {
	s.mu.Lock()
	s.conflictJITPrefixes = append(s.conflictJITPrefixes, prefix)
	s.mu.Unlock()
}

// ClearJITConfigConflicts drops every configured runner-name conflict, exact and
// prefixed, modelling the stale registrations finally clearing. It is the lever for the
// re-offer path: a job deferred because its name would not register must provision once
// the conflict goes away, rather than staying queued at GitHub forever (Q551).
func (s *Stub) ClearJITConfigConflicts() {
	s.mu.Lock()
	s.conflictJITNames = map[string]bool{}
	s.conflictJITPrefixes = nil
	s.mu.Unlock()
}

// jitConfigConflicts reports whether generatejitconfig should 409 for name. Caller
// holds s.mu.
func (s *Stub) jitConfigConflicts(name string) bool {
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
func (s *Stub) SetRateLimitPolls(on bool) {
	s.mu.Lock()
	s.rateLimitPolls = on
	s.notifyLocked()
	s.mu.Unlock()
}

// FailSessionRefresh makes session refresh (PATCH sessions/{sid}) answer 401
// Unauthorized while on, modelling credentials revoked after the session opened —
// combined with ExpireQueueToken it drives the poll-401 → refresh-401 path that must
// surface Degraded/Unauthorized (Q325).
func (s *Stub) FailSessionRefresh(on bool) {
	s.mu.Lock()
	s.failSessionRefresh = on
	s.mu.Unlock()
}

// FailSessionCreate makes session create (POST sessions) answer 401 Unauthorized
// while on, modelling revoked credentials at session creation — the lever for the
// Start-path and session-re-create Degraded/Unauthorized paths (Q325).
func (s *Stub) FailSessionCreate(on bool) {
	s.mu.Lock()
	s.failSessionCreate = on
	s.mu.Unlock()
}

// FailDeleteMessage makes the message-DELETE ack answer status instead of acting;
// zero restores it. Pass 404 for a backend that does not serve the endpoint — which
// Client.DeleteMessage reports as a completed ack that deleted nothing, visible only
// in its first result (Q609) — or a 5xx for one that is momentarily unable to, which
// surfaces as an error the caller must retry (Q583).
func (s *Stub) FailDeleteMessage(status int) {
	s.mu.Lock()
	s.failDeleteMessageStatus = status
	s.mu.Unlock()
}

// AcceptDeleteWithoutPruning makes the message-DELETE ack answer 204 while leaving
// the message in the queue log. It models the backend a status check alone cannot
// distinguish from a working delete, and is the lever for the one verdict that would
// rule delete-acking out as the Q583 fix (Q583).
func (s *Stub) AcceptDeleteWithoutPruning(on bool) {
	s.mu.Lock()
	s.deleteWithoutPruning = on
	s.mu.Unlock()
}

// SetRateLimitRemaining makes every message-poll response carry
// X-RateLimit-Remaining: n (n > 0; zero, the default, omits the header). GitHub's
// queue reports the caller's remaining budget on every poll — including the 202 the
// typed API collapses to (nil, nil) — so this is the lever for a test asserting that
// a caller surfaces the rate-limit evidence the return value discards (U4).
func (s *Stub) SetRateLimitRemaining(n int) {
	s.mu.Lock()
	s.rateLimitRemaining = n
	s.mu.Unlock()
}

// FailRunnerGroups makes the runnergroups lookup answer 500 while on, modelling a
// backend that will not resolve a group by name — the lever for a caller's
// fall-back-to-the-default-group path.
func (s *Stub) FailRunnerGroups(on bool) {
	s.mu.Lock()
	s.failRunnerGroups = on
	s.mu.Unlock()
}

// FailStaticAcquireRoute makes the static _apis/runtime acquirejobs route answer 404
// while on, leaving the queue-host route (the acquireJobUrl delivered on a
// JobAvailable) working. It models the broker-host tenant observed live in Q264
// §2a-3, and is the lever for proving a caller notices that divergence rather than
// silently failing to claim. The refused attempt is still recorded, so a test can
// assert the static route was tried first.
func (s *Stub) FailStaticAcquireRoute(on bool) {
	s.mu.Lock()
	s.failStaticAcquire = on
	s.mu.Unlock()
}

// SetInstallationToken makes the REST registration-token hop require
// "Authorization: Bearer <tok>", answering 401 otherwise (an empty token, the
// default, accepts anything). It pins the first link of the token matrix (§2.5): the
// App installation token, and only it, authorizes that call.
func (s *Stub) SetInstallationToken(tok string) {
	s.mu.Lock()
	s.installToken = tok
	s.mu.Unlock()
}

// PrequeueJobs queues n jobs against the scale set's label *before* any scale set
// exists, and returns their RunnerRequestIDs. Every scale set created afterwards
// starts with a copy of them — the real routing behaviour for a workflow dispatched
// with runs-on: <scale set name> ahead of the scale set registering. It is the way
// to give jobs to a caller that creates its own scale set mid-run and so cannot be
// handed an id to EnqueueJob against.
func (s *Stub) PrequeueJobs(n int) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, n)
	for range n {
		j := s.newQueuedJobLocked()
		s.pendingJobs = append(s.pendingJobs, j)
		ids = append(ids, j.reqID)
	}
	return ids
}

// newQueuedJobLocked mints one queued job with a fresh request id, job id, and run
// identity. It is the SINGLE construction site for a job, shared by EnqueueJob (a job
// queued against a live scale set) and PrequeueJobs (a job queued before the scale set
// exists). Caller holds s.mu.
//
// newQueuedJobLocked is the single construction site for queued jobs, so
// EnqueueJob and PrequeueJobs cannot drift on which identity fields they set —
// the real backend sends them on every assignment.
func (s *Stub) newQueuedJobLocked() *job {
	s.nextReqID++
	s.nextJobSeq++
	return &job{
		reqID: s.nextReqID,
		jobID: fmt.Sprintf("job-%d", s.nextJobSeq),
		state: jobQueued,
		owner: DefaultJobOwner,
		repo:  DefaultJobRepository,
		// One run per job, derived from the sequence so it is deterministic and
		// distinct — a shared run id would hide any per-run bookkeeping bug.
		runID: int64(900000 + s.nextJobSeq),
	}
}

// SeedMessage appends one message to every newly created scale set's queue log,
// ahead of the messages the model generates itself. It exists for the shapes the
// model cannot reach on its own — a lifecycle message (JobStarted / JobCompleted)
// with no preceding assignment on this scale set — so a test can prove a client
// tolerates and skips a message it has nothing to do with, and that its cursor
// still advances past it. Prefer EnqueueJob/CompleteAssignedJob whenever the model
// can produce the message for real.
func (s *Stub) SeedMessage(entries []scaleset.JobMessage) {
	s.mu.Lock()
	s.pendingMessages = append(s.pendingMessages, seedMessage{entries: entries})
	s.mu.Unlock()
}

// SeedRawMessage is SeedMessage with a verbatim body, for the one shape no typed
// entry list can express: a body the client cannot decode. A client must skip it and
// keep polling rather than wedge its cursor behind it.
func (s *Stub) SeedRawMessage(body string) {
	s.mu.Lock()
	s.pendingMessages = append(s.pendingMessages, seedMessage{rawBody: &body})
	s.mu.Unlock()
}

// AddScaleSet registers a scale set out of band and returns its id, modelling one a
// previous run leaked — the state a cleanup path exists to find by name and delete.
func (s *Stub) AddScaleSet(name string, groupID int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newScaleSetLocked(name, groupID).id
}

// DropExtraScaleSetLabels models a GitHub Enterprise Server appliance below 3.21
// without DistributedTask.AllowRunnerScaleSetCustomLabels: a create keeps only the
// label matching the scale set's name and discards the rest, answering 200 as though
// it had taken them all. It is the silent half of the multi-label failure mode, and
// the only way a client learns of it is by reading the labels back (Q726).
func (s *Stub) DropExtraScaleSetLabels(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropExtraScaleSetLabels = on
}

// SetScaleSetLabels records the System labels a scale set carries, for a set put there
// by AddScaleSet rather than by a create. Together they model the state an AGC restart
// meets: a scale set registered by an earlier generation, carrying whatever labels were
// declared then rather than the ones declared now.
func (s *Stub) SetScaleSetLabels(id int, names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.scaleSets[id]
	if ss == nil {
		return
	}
	ss.labels = nil
	for _, n := range names {
		ss.labels = append(ss.labels, scaleset.Label{Name: n, Type: "System"})
	}
}

// SetAdminTokenTTL controls the TTL of the admin JWT minted by the
// runner-registration hop. Set it below the client's refresh lead (default 60s) to
// force the client to re-mint the admin connection on its next admin call — the
// lever for the admin-JWT lifecycle test (§2b-7). Takes effect on the next hop.
func (s *Stub) SetAdminTokenTTL(ttl time.Duration) {
	s.mu.Lock()
	s.adminTokenTTL = ttl
	s.mu.Unlock()
}

// Job is the identity the stub delivers on a job's assignment messages, which is
// where a client reading ownerName/repositoryName/workflowRunId off an assignment
// gets them (scale-set eviction recovery does — Q417).
type Job struct {
	RunnerRequestID int64  `json:"runnerRequestId"`
	JobID           string `json:"jobId"`
	OwnerName       string `json:"ownerName"`
	RepositoryName  string `json:"repositoryName"`
	WorkflowRunID   int64  `json:"workflowRunId"`
}

// EnqueueJob queues one job on the scale set and returns its RunnerRequestID and
// job UUID. In auto-assign mode a poll advertising enough capacity assigns it; in
// GHES mode it is first offered as JobAvailable and assigned on acquire.
func (s *Stub) EnqueueJob(scaleSetID int) (reqID int64, jobID string) {
	j := s.Enqueue(scaleSetID)
	return j.RunnerRequestID, j.JobID
}

// Enqueue is EnqueueJob returning the whole identity the assignment will carry, for
// a caller that asserts against what the job delivers rather than only tracking it.
func (s *Stub) Enqueue(scaleSetID int) Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enqueueLocked(scaleSetID, "")
}

// EnqueueStalledJob queues a job whose runner names already conflict, so it cannot
// provision from the moment it is pollable. namePrefix is joined to the job id the
// stub mints here, which is why a caller cannot install the conflict itself:
// FailJITConfigNamePrefix after EnqueueJob leaves a window in which a poll assigns
// and provisions the job before the conflict applies, and it never defers.
func (s *Stub) EnqueueStalledJob(scaleSetID int, namePrefix string) (reqID int64, jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.enqueueLocked(scaleSetID, namePrefix)
	return j.RunnerRequestID, j.JobID
}

// enqueueLocked queues a job, registering the runner-name conflict that stalls it in
// the same critical section when conflictPrefix is non-empty. Caller holds s.mu.
func (s *Stub) enqueueLocked(scaleSetID int, conflictPrefix string) Job {
	ss := s.scaleSets[scaleSetID]
	if ss == nil {
		panic(fmt.Sprintf("scalesetstub: Enqueue on unknown scale set %d", scaleSetID))
	}
	j := s.newQueuedJobLocked()
	ss.jobs = append(ss.jobs, j)
	if conflictPrefix != "" {
		s.conflictJITPrefixes = append(s.conflictJITPrefixes, conflictPrefix+j.jobID)
	}
	// A queued job produces no message until a poll re-evaluates it against the
	// advertised capacity, so wake the parked polls to do exactly that.
	s.notifyLocked()
	return Job{
		RunnerRequestID: j.reqID,
		JobID:           j.jobID,
		OwnerName:       j.owner,
		RepositoryName:  j.repo,
		WorkflowRunID:   j.runID,
	}
}

// CompleteAssignedJob marks an assigned job completed and appends a JobCompleted
// message to the queue, so a polling client observes the terminal result — the
// signal the classic protocol never delivered (§2b-6). Returns false if no
// non-terminal job with that id exists.
func (s *Stub) CompleteAssignedJob(scaleSetID int, jobID, result string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.scaleSets[scaleSetID]
	if ss == nil {
		return false
	}
	for _, j := range ss.jobs {
		if j.jobID == jobID && !j.state.terminal() {
			s.completeJobLocked(ss, j, result)
			s.notifyLocked()
			return true
		}
	}
	return false
}

// DropAssignedJob ends an assignment WITHOUT delivering a JobCompleted — the way the
// backend loses a job a client is still holding. The statistics stop counting it and
// nothing else is said, so a client that waits for a completion waits forever (Q553).
//
// This is not a hypothetical shape the fake invented: a scale set whose statistics
// report zero assigned jobs while the AGC still held fifteen is what the v1.3.0-rc.3
// dogfood gate recorded. Returns false if no non-terminal job with that id exists.
func (s *Stub) DropAssignedJob(scaleSetID int, jobID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.scaleSets[scaleSetID]
	if ss == nil {
		return false
	}
	for _, j := range ss.jobs {
		if j.jobID == jobID && !j.state.terminal() {
			j.state = jobDropped
			// No message, but the parked poll must still wake: the statistics it
			// serves have changed.
			s.notifyLocked()
			return true
		}
	}
	return false
}

// completeJobLocked drives one job terminal and appends its JobCompleted to the
// owning scale set's queue log. Both ways a job can end here — the direct test
// hook and the REST run-cancel route — go through it, so a client cannot observe
// a different message depending on which one the test used.
func (s *Stub) completeJobLocked(ss *scaleSet, j *job, result string) {
	j.state = jobCompleted
	j.result = result
	ss.appendMessage(s.newMsgID(), []scaleset.JobMessage{{
		MessageType: scaleset.MessageTypeJobCompleted,
		JobID:       j.jobID,
		Result:      result,
	}})
}

// ExpireQueueToken rotates the active session's queue token without revealing the
// new value, so the client's cached token 401s on its next poll/acquire — the lever
// for the queue-token refresh test. RefreshSession (PATCH) hands the client the new
// token (§2b-2).
func (s *Stub) ExpireQueueToken(scaleSetID int) {
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
func (s *Stub) DropSession(scaleSetID int) {
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
func (s *Stub) AssignedJobCount(scaleSetID int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.scaleSets[scaleSetID]
	if ss == nil {
		return 0
	}
	return ss.stats().TotalAssignedJobs
}

// Calls returns the recorded call log in order.
func (s *Stub) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// RunnerRegistrationCalls returns how many RemoteAuth runner-registration hops the
// stub served — one per admin-connection mint, so >1 proves an admin-JWT refresh.
func (s *Stub) RunnerRegistrationCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runnerRegistrationCalls
}

// AcquireJobsCalls returns how many acquirejobs calls the stub served — zero in
// auto-assign mode.
func (s *Stub) AcquireJobsCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acquireJobsCalls
}

// RefreshSessionCalls returns how many session-refresh (PATCH) calls the stub served.
func (s *Stub) RefreshSessionCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshSessionCalls
}

// GenerateJITCalls returns how many generatejitconfig calls the stub served.
func (s *Stub) GenerateJITCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generateJITCalls
}

// DeleteRunnerCalls returns how many REST delete-runner calls the stub served — the
// Q334 deregister-on-conflict recovery drives one per reclaimed stale record.
func (s *Stub) DeleteRunnerCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteRunnerCalls
}

// ScaleSetIDByName returns the id of the scale set registered under name and whether
// one exists. It lets a caller that did not create the scale set itself — e.g. a
// controller test where the reconciler created it internally — resolve the id it needs
// for EnqueueJob/AssignedJobCount.
func (s *Stub) ScaleSetIDByName(name string) (int, bool) {
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
func (s *Stub) HasActiveSession(scaleSetID int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.scaleSets[scaleSetID]
	return ss != nil && ss.session != nil
}

// ── internals ───────────────────────────────────────────────────────────────

func (s *Stub) record(format string, args ...any) {
	s.calls = append(s.calls, fmt.Sprintf(format, args...))
}

// newScaleSetLocked registers a scale set and gives it the pre-registration queue
// state (PrequeueJobs, SeedMessage) every new scale set inherits. Caller holds s.mu.
func (s *Stub) newScaleSetLocked(name string, groupID int) *scaleSet {
	s.nextSSID++
	ss := &scaleSet{id: s.nextSSID, name: name, groupID: groupID}
	for _, j := range s.pendingJobs {
		clone := *j
		ss.jobs = append(ss.jobs, &clone)
	}
	for _, m := range s.pendingMessages {
		ss.messages = append(ss.messages, &qmessage{
			id:      s.newMsgID(),
			entries: m.entries,
			rawBody: m.rawBody,
		})
	}
	s.scaleSets[ss.id] = ss
	return ss
}

// credentialKind names which token an inbound request presented, so a call log can
// show the token half of the acquire matrix (§2.5) — including an attempt the route
// then refuses. Caller holds s.mu.
func (s *Stub) credentialKind(r *http.Request, ss *scaleSet) string {
	auth := r.Header.Get("Authorization")
	switch {
	case ss.session != nil && auth == "Bearer "+ss.session.queueToken:
		return "queue"
	case auth == "Bearer "+s.adminToken:
		return "admin"
	default:
		return "other"
	}
}

func (s *Stub) newMsgID() int64 {
	s.nextMsgID++
	return s.nextMsgID
}

// mintAdminJWT builds a syntactically valid JWT with an exp claim ttl from now. The
// client reads exp (without verifying the signature) to schedule its pre-expiry
// refresh, so a dummy signature segment suffices.
func (s *Stub) mintAdminJWT(ttl time.Duration) string {
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
func (s *Stub) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
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

func (s *Stub) handleRegistrationToken(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.record("registration-token %s", r.URL.Path)
	want := s.installToken
	s.mu.Unlock()
	if want != "" && r.Header.Get("Authorization") != "Bearer "+want {
		http.Error(w, `{"message":"bad installation token"}`, http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"token":      "reg-token",
		"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	})
}

// handleCancelRun models the REST run-cancel route
// (POST /repos/{owner}/{repo}/actions/runs/{id}/cancel): every non-terminal job
// belonging to that run goes terminal with result "cancelled", queueing its
// JobCompleted on the owning scale set.
//
// It exists because cancelling a run is the only way to drive a job terminal
// without a live runner, which is what the Q468 retention probe does to produce
// the JobCompleted whose retention it measures. Modelling the causal chain — REST
// call in, queue message out — means a test covers that wiring instead of
// stubbing around it.
//
// A run with nothing left to cancel answers 409, matching the real API's response
// for an already-terminal run.
//
// The result string is "canceled", one L — the spelling observed on a live
// JobCompleted (Q468, 2026-07-28), not Go's or English's preference.
func (s *Stub) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runID, _ := strconv.ParseInt(r.PathValue("runid"), 10, 64)
	s.record("cancel-run id=%d", runID)
	if s.installToken != "" && r.Header.Get("Authorization") != "Bearer "+s.installToken {
		http.Error(w, `{"message":"bad installation token"}`, http.StatusUnauthorized)
		return
	}
	cancelled := 0
	for _, ss := range s.scaleSets {
		for _, j := range ss.jobs {
			if j.runID == runID && !j.state.terminal() {
				s.completeJobLocked(ss, j, "canceled")
				cancelled++
			}
		}
	}
	if cancelled == 0 {
		http.Error(w, `{"message":"run is already terminal"}`, http.StatusConflict)
		return
	}
	s.notifyLocked()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Stub) handleRunnerRegistration(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("runner-registration")
	s.runnerRegistrationCalls++
	if got := r.Header.Get("Authorization"); got != "RemoteAuth reg-token" {
		http.Error(w, `{"message":"bad RemoteAuth"}`, http.StatusUnauthorized)
		return
	}
	// The hop is a *registration*: the backend keys off runner_event, so a caller
	// that omits it gets no admin connection.
	var reg struct {
		RunnerEvent string `json:"runner_event"`
	}
	_ = json.NewDecoder(r.Body).Decode(&reg)
	if reg.RunnerEvent != "register" {
		http.Error(w, `{"message":"runner_event must be register"}`, http.StatusBadRequest)
		return
	}
	// Mint a fresh admin JWT with the current TTL — a short TTL forces the client
	// to re-mint on its next admin call.
	s.adminToken = s.mintAdminJWT(s.adminTokenTTL)
	_ = json.NewEncoder(w).Encode(scaleset.AdminConnection{URL: s.baseURL(r), Token: s.adminToken})
}

func (s *Stub) handleRunnerGroups(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.URL.Query().Get("groupName")
	s.record("runnergroups name=%s", name)
	if !s.requireAdmin(w, r) {
		return
	}
	if s.failRunnerGroups {
		http.Error(w, `{"message":"runner groups unavailable"}`, http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"count": 1,
		"value": []scaleset.RunnerGroup{{ID: 7, Name: name}},
	})
}

func (s *Stub) handleCreateScaleSet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var in scaleset.RunnerScaleSet
	_ = json.NewDecoder(r.Body).Decode(&in)
	s.record("create-scaleset name=%s group=%d", in.Name, in.RunnerGroupID)
	if !s.requireAdmin(w, r) {
		return
	}
	ss := s.newScaleSetLocked(in.Name, in.RunnerGroupID)
	ss.labels = in.Labels
	if s.dropExtraScaleSetLabels {
		ss.labels = nil
		for _, lbl := range in.Labels {
			if lbl.Name == in.Name {
				ss.labels = append(ss.labels, lbl)
			}
		}
	}
	_ = json.NewEncoder(w).Encode(scaleset.RunnerScaleSet{
		ID: ss.id, Name: ss.name, RunnerGroupID: ss.groupID, Labels: ss.labels, RunnerSetting: in.RunnerSetting,
	})
}

func (s *Stub) handleGetScaleSetByName(w http.ResponseWriter, r *http.Request) {
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
			match = append(match, scaleset.RunnerScaleSet{ID: ss.id, Name: ss.name, RunnerGroupID: ss.groupID, Labels: ss.labels})
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"count": len(match), "value": match})
}

func (s *Stub) handleGetScaleSet(w http.ResponseWriter, r *http.Request) {
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

func (s *Stub) handlePatchScaleSet(w http.ResponseWriter, r *http.Request) {
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

func (s *Stub) handleDeleteScaleSet(w http.ResponseWriter, r *http.Request) {
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

func (s *Stub) handleCreateSession(w http.ResponseWriter, r *http.Request) {
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
		MessageQueueURL:         fmt.Sprintf("%s/queue/%d/message", s.baseURL(r), id),
		MessageQueueAccessToken: ss.session.queueToken,
		Statistics:              ss.stats(),
	})
}

func (s *Stub) handleRefreshSession(w http.ResponseWriter, r *http.Request) {
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
		MessageQueueURL:         fmt.Sprintf("%s/queue/%d/message", s.baseURL(r), id),
		MessageQueueAccessToken: ss.session.queueToken,
		Statistics:              ss.stats(),
	})
}

func (s *Stub) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
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

func (s *Stub) handleGenerateJIT(w http.ResponseWriter, r *http.Request) {
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
	// A record already under this name — whether a configured stale one or one an
	// earlier mint left behind — is the runner-name 409 the client maps to
	// *RunnerNameConflictError (Q270), distinct from a session-create conflict. The
	// second case is Q550's cycle: a job whose provision failed collides with its own
	// leftover on the retry, because the name derives from the job ID.
	if s.jitConfigConflicts(in.Name) || s.runnerIDs[in.Name] != 0 {
		http.Error(w, `{"message":"runner name already exists"}`, http.StatusConflict)
		return
	}
	// Minting pre-registers the runner server-side, which is what makes the record
	// outlive a worker that never runs its job (Q550). The record is offline until a
	// runner connects under it, and holds the name against a re-mint.
	if _, ok := s.runnerIDs[in.Name]; !ok {
		s.nextRunnerID++
		s.runnerIDs[in.Name] = s.nextRunnerID
	}
	blob := base64.StdEncoding.EncodeToString([]byte(`{".runner":{},".credentials":{}}`))
	out := scaleset.JITRunnerConfig{EncodedJITConfig: blob}
	out.Runner.ID = int(s.runnerIDs[in.Name])
	out.Runner.Name = in.Name
	_ = json.NewEncoder(w).Encode(out)
}

// handleListRunners serves the REST list-runners endpoint in both the shapes the
// client uses: the exact-name filter DeregisterRunnerByName resolves a stale record
// with (Q334), and the unfiltered paginated listing the start-up sweep walks to find
// records no live worker claims (Q550). A name hidden by HideRunnerFromNameFilter is
// omitted from the filtered form only, so the two can disagree the way the live API is
// suspected to.
func (s *Stub) handleListRunners(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := r.URL.Query()
	name := q.Get("name")
	s.record("list-runners name=%s page=%s", name, q.Get("page"))

	type restRunner struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Busy   bool   `json:"busy"`
	}
	entry := func(n string) restRunner {
		status := "offline"
		if s.onlineRunners[n] {
			status = "online"
		}
		return restRunner{ID: s.runnerIDs[n], Name: n, Status: status, Busy: s.busyRunners[n]}
	}

	if name != "" {
		var runners []restRunner
		if _, ok := s.runnerIDs[name]; ok && !s.hiddenRunners[name] {
			runners = append(runners, entry(name))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(runners), "runners": runners})
		return
	}

	names := make([]string, 0, len(s.runnerIDs))
	for n := range s.runnerIDs {
		names = append(names, n)
	}
	sort.Strings(names) // stable paging across requests

	perPage := atoiOr(q.Get("per_page"), 100)
	page := atoiOr(q.Get("page"), 1)
	runners := []restRunner{}
	if start := (page - 1) * perPage; start < len(names) {
		end := min(start+perPage, len(names))
		for _, n := range names[start:end] {
			runners = append(runners, entry(n))
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(names), "runners": runners})
}

// atoiOr parses a positive integer query parameter, falling back to def.
func atoiOr(v string, def int) int {
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// handleDeleteRunner serves the REST DELETE used by Client.DeregisterRunnerByName. A
// runner marked busy answers 422 (a live runner that must not be deleted); otherwise the
// record is removed and any matching generatejitconfig conflict is cleared, so a
// re-registration under the same base name then succeeds (Q334).
func (s *Stub) handleDeleteRunner(w http.ResponseWriter, r *http.Request) {
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
	delete(s.onlineRunners, name)
	delete(s.hiddenRunners, name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Stub) handleAcquireJobs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.acquireJobsCalls++
	ss := s.scaleSets[id]
	if ss == nil || ss.session == nil {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		return
	}
	var ids []int64
	_ = json.NewDecoder(r.Body).Decode(&ids)
	// Record before authorizing, so a refused attempt (an admin JWT on a
	// queue-token route, or the 404 FailStaticAcquireRoute models) is still visible
	// as "this route was tried, with this token, for these ids".
	s.record("acquirejobs id=%d auth=%s ids=%v", id, s.credentialKind(r, ss), ids)
	if s.failStaticAcquire {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		return
	}
	// acquirejobs is authorized by the queue token, not the admin JWT (§2.5).
	if r.Header.Get("Authorization") != "Bearer "+ss.session.queueToken {
		http.Error(w, `{"message":"invalid queue token"}`, http.StatusUnauthorized)
		return
	}
	_ = json.NewEncoder(w).Encode(s.claimLocked(ss, ids))
}

// handleQueueHostAcquireJobs serves the acquire verb on the message-queue host — the
// route family a JobAvailable's acquireJobUrl points at, outside /_apis/runtime. The
// broker-host tenant honours it even when the static route 404s (Q264 §2a-3), so
// modelling both is what lets a caller measure one against the other. It claims
// identically and is authorized by the same queue token.
func (s *Stub) handleQueueHostAcquireJobs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.acquireJobsCalls++
	ss := s.scaleSets[id]
	if ss == nil || ss.session == nil {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		return
	}
	var ids []int64
	_ = json.NewDecoder(r.Body).Decode(&ids)
	s.record("acquirejobs-queuehost id=%d auth=%s ids=%v", id, s.credentialKind(r, ss), ids)
	if r.Header.Get("Authorization") != "Bearer "+ss.session.queueToken {
		http.Error(w, `{"message":"invalid queue token"}`, http.StatusUnauthorized)
		return
	}
	_ = json.NewEncoder(w).Encode(s.claimLocked(ss, ids))
}

// handleAcquirableJobs serves ARC's read-only listing of the jobs currently offered
// but unclaimed. It claims nothing — a caller comparing acquire constructions can
// issue it safely — and is authorized by the admin JWT, unlike the acquire verb.
func (s *Stub) handleAcquirableJobs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.record("acquirablejobs id=%d", id)
	if !s.requireAdmin(w, r) {
		return
	}
	ss := s.scaleSets[id]
	if ss == nil {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		return
	}
	type acquirable struct {
		RunnerRequestID int64  `json:"runnerRequestId"`
		AcquireJobURL   string `json:"acquireJobUrl"`
	}
	value := []acquirable{}
	for _, j := range ss.jobs {
		if j.state == jobAvailable {
			value = append(value, acquirable{RunnerRequestID: j.reqID, AcquireJobURL: s.acquireURL(r, ss.id)})
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"count": len(value), "value": value})
}

// claimLocked claims each offered-but-unclaimed id for the scale set and returns the
// {count, value} acquire response. Claim-once: a second claim of the same id finds it
// already assigned and is refused. Caller holds s.mu.
func (s *Stub) claimLocked(ss *scaleSet, ids []int64) map[string]any {
	var won []int64
	for _, reqID := range ids {
		for _, j := range ss.jobs {
			if j.reqID == reqID && j.state == jobAvailable {
				j.state = jobAssigned
				ss.appendMessage(s.newMsgID(), []scaleset.JobMessage{{
					MessageType:     scaleset.MessageTypeJobAssigned,
					RunnerRequestID: j.reqID,
					JobID:           j.jobID,
					OwnerName:       j.owner,
					RepositoryName:  j.repo,
					WorkflowRunID:   j.runID,
					JobDisplayName:  j.jobID,
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
	return map[string]any{"count": len(won), "value": won}
}

// acquireURL is the queue-host acquire endpoint a JobAvailable advertises for a
// scale set — the acquireJobUrl the real backend delivers on the offer. It is
// resolved against the polling request's base, so an offer always names a host the
// poller can actually reach.
func (s *Stub) acquireURL(r *http.Request, scaleSetID int) string {
	return fmt.Sprintf("%s/queue/%d/acquirejobs", s.baseURL(r), scaleSetID)
}

// handlePoll serves the message queue's long poll. It re-evaluates assignment against
// the capacity this request advertised, and if that yields nothing deliverable it parks
// the request — waking on any queue-state change (notifyLocked) to re-evaluate, and
// answering 202 only once the poll window elapses (see DefaultPollTimeout).
//
// The advertised capacity is fixed for the life of one request, exactly as on the real
// backend — a client whose free-slot count grows mid-poll only advertises the new value
// on its next poll, so the window also bounds how stale that value can get.
func (s *Stub) handlePoll(w http.ResponseWriter, r *http.Request) {
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
		// The queue reports the caller's remaining budget on every poll, whatever the
		// status — including the 202 a typed API collapses to "no message".
		if s.rateLimitRemaining > 0 {
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(s.rateLimitRemaining))
		}
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
			ss.offerAvailable(s.newMsgID, s.acquireURL(r, ss.id))
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

func (s *Stub) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := strconv.Atoi(r.PathValue("id"))
	msgID, _ := strconv.ParseInt(r.PathValue("msgid"), 10, 64)
	s.record("delete-message id=%d msg=%d", id, msgID)
	if s.failDeleteMessageStatus != 0 {
		http.Error(w, `{"message":"delete refused"}`, s.failDeleteMessageStatus)
		return
	}
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
			if !s.deleteWithoutPruning {
				m.deleted = true
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	http.Error(w, `{"message":"message not found"}`, http.StatusNotFound)
}

// encodeMessage renders a queue message with the scale set's current statistics
// snapshot attached (the server-authoritative counts, read off every envelope).
func (s *Stub) encodeMessage(ss *scaleSet, m *qmessage) scaleset.RunnerScaleSetMessage {
	var body string
	if m.rawBody != nil {
		body = *m.rawBody
	} else {
		b, _ := json.Marshal(m.entries)
		body = string(b)
	}
	return scaleset.RunnerScaleSetMessage{
		MessageID:   m.id,
		MessageType: "RunnerScaleSetJobMessages",
		Body:        body,
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
				MessageType:    scaleset.MessageTypeJobAssigned,
				JobID:          j.jobID,
				OwnerName:      j.owner,
				RepositoryName: j.repo,
				WorkflowRunID:  j.runID,
				JobDisplayName: j.jobID,
			}})
			assigned++
		}
	}
}

// offerAvailable offers each still-queued job as a JobAvailable message (GHES flow),
// carrying the queue-host acquireJobUrl the real backend delivers on the offer.
func (ss *scaleSet) offerAvailable(nextID func() int64, acquireURL string) {
	for _, j := range ss.jobs {
		if j.state == jobQueued {
			j.state = jobAvailable
			ss.appendMessage(nextID(), []scaleset.JobMessage{{
				MessageType:     scaleset.MessageTypeJobAvailable,
				RunnerRequestID: j.reqID,
				AcquireJobURL:   acquireURL,
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
		case jobCompleted, jobDropped:
		}
	}
	return &scaleset.RunnerScaleSetStatistic{
		TotalAvailableJobs: available,
		TotalAssignedJobs:  assigned,
		TotalRunningJobs:   running,
	}
}
