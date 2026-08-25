// Command fakegithub is a deployable HTTP stub that implements the GitHub App
// token exchange endpoint, the Actions runner registration API, and BOTH job
// acquisition protocols — the Actions broker v2 protocol (classic) and the
// runner-scale-set protocol. It is used in fake-GitHub e2e tests so the AGC can
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
//	GET  /api/v3/repos/{o}/{r}/actions/runs/{id}  — run status/conclusion (Q811)
//	POST /api/v3/repos/{o}/{r}/actions/runs/{id}/rerun-failed-jobs — eviction auto-retry
//	POST /api/v3/repos/{o}/{r}/actions/runs/{id}/force-cancel — abandoned-run fast ending (Q683)
//	POST /api/v3/.../actions/runners/registration-token — scale-set bootstrap, hop 1
//	POST /api/v3/actions/runner-registration     — scale-set bootstrap, hop 2 (RemoteAuth)
//	     /_apis/runtime/...                      — scale-set Actions Service
//	     /queue/{id}/...                         — scale-set message queue
//
// Jobs are injected via the HTTP control API (only reachable from within the
// pod; bind address is configurable via CONTROL_ADDR, default :9090):
//
//	POST /control/enqueue?sessionId=<id>  — body: RunnerJobRequestBody JSON
//	GET  /control/sessions                — active session IDs
//	POST /control/singleuse?enabled=true  — toggle single-use JIT simulation
//	GET  /control/reruns                  — eviction auto-retry calls observed
//	POST /control/runstate?run=<id>&concluded=false — refuse re-runs for the run
//	POST /control/scaleset/enqueue?name=<set>      — queue a scale-set job
//	POST /control/scaleset/acquireflow?ghes=<bool> — pick the acquire flow
//	GET  /control/scaleset/state?name=<set>        — session/assignment/call state
//
// # The scale-set protocol (Q528)
//
// The scale-set half is scaleset/scalesetstub — the same protocol model the
// scaleset client's own unit tests run against, so the two doubles cannot drift.
// Everything scale-set-specific lives there; this command only mounts it, gives it
// the request-derived base its self-referential URLs need, and exposes the control
// verbs above.
//
// The two tiers share the process and nothing else. In particular they keep
// separate runner registries: the classic tier registers through
// /api/v3/.../generate-jitconfig, the scale-set tier through generatejitconfig on
// the Actions Service. They never have to agree, because the scale-set tier reaches
// the REST list/delete routes only on a runner-name 409, which this venue does not
// produce.
//
// # Eviction auto-retry observability (Q421)
//
// The AGC's automatic recovery from a worker-pod eviction is a POST to GitHub's
// rerun-failed-jobs endpoint, and it is the only externally visible signal that
// recovery fired. fakegithub answers that call like GitHub does (201, empty body)
// and records it, so a fake-GitHub spec can assert recovery both ways: that it fires
// for a kubelet eviction, and — the Q421 measurement — that it does NOT fire for a
// node drain, whose Eviction API call deletes the pod rather than failing it.
// Without this the absence of a rerun is indistinguishable from a 404 nobody read.
//
// # Run-conclusion gating on re-runs (Q517)
//
// Real GitHub refuses rerun-failed-jobs with `403 This workflow is already
// running` until it has concluded the original run — after an ungraceful kill
// that takes until the job lock's TTL lapses, ~10 minutes (measured 9m36s,
// Q503) — and the AGC's recovery keys its retry loop on exactly that refusal.
// A run marked not-yet-concluded via /control/runstate gets the same answer
// here, with the measured message the AGC discriminates on; concluding it
// (concluded=true) flips the answer back to 201. Refused calls are recorded
// separately from accepted ones on /control/reruns, so a spec can assert the
// refusal window and the post-conclusion acceptance without the refusals
// inflating its accepted count. Runs never marked are concluded — the default
// instantly-concluding model — so only a spec that opts its own run in sees
// refusals, and parallel specs are unaffected.
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
//
// That carry rides the session's DELETE, which is best-effort: an AGC whose
// DeleteSession times out logs the session as leaked and moves on, leaving a
// session that is registered but polled by nobody. A job addressed to one
// session therefore also ages out on its own — after defaultSessionQueueGrace
// undelivered it moves to the owner pool, from where any live session of that
// owner picks it up (Q436). A session that is actually polling drains its queue
// within one longPollTick, so a healthy targeted delivery is never diverted.
// Together the two paths hold the invariant the real broker has by
// construction: an enqueued job is always reachable by *some* session.
//
// # Lease / acquire-vs-redeliver fidelity (Q154)
//
// Opt-in via /control/redelivery?enabled=true. When on for an in-scope owner,
// GET /message no longer drops a delivered job: it *leases* it, holding it out
// of circulation until one of two things happens, which models the GitHub
// broker contract the Q59 admission gate rests on:
//
//   - AcquireJob claims the job → it is consumed and never delivered again,
//     even though the runner may then abandon it at the worker pod-capacity
//     ceiling. This is GitHub keeping an *acquired* job (the runner owns it; an
//     unrenewed lock is cancelled, not handed to a sibling) — the assumption
//     Q59's pre-acquire gate is designed around.
//   - the lease expires unclaimed → the job returns to the owner pool and is
//     redelivered. This is GitHub returning a *delivered-but-not-acquired* job
//     (the gate skipped it for lack of capacity) to the queue, so a skipped job
//     is not lost.
//
// Control endpoints (only meaningful with redelivery enabled):
//
//	POST /control/redelivery?enabled=true[&owner=<prefix>][&leaseMs=<n>]
//	GET  /control/jobstats?requestId=<runner_request_id>  — {deliveries,leased,acquired}
//
// Off by default; the immediate-dequeue model the other specs rely on is
// unchanged. Owner-scoped like the single-use simulation so it does not disturb
// specs running in parallel against the shared instance.
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

	"github.com/actions-gateway/github-actions-gateway/broker/brokerstub"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesetstub"
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
	// MESSAGE_LONGPOLL_HOLD enables the broker long-poll on GET /message (e.g.
	// "30s"). Empty or unparseable leaves it at zero (immediate 202). See the
	// server.longPollHold field doc (Q148).
	if v := os.Getenv("MESSAGE_LONGPOLL_HOLD"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			s.longPollHold = d
		} else {
			log.Printf("ignoring invalid MESSAGE_LONGPOLL_HOLD %q: %v", v, err)
		}
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
	mu           sync.Mutex
	tokenCounter atomic.Int64
	msgCounter   atomic.Int64
	// sessions is the shared session registry: it mints "session-<n>" IDs,
	// tracks each session's owner/agent/version, and resolves a DELETE by its
	// sessionId query param or bearer token. The single-use and redelivery
	// state below is fakegithub-specific and keyed off the IDs it mints.
	sessions *brokerstub.Sessions
	// scaleSet is the runner-scale-set protocol, served alongside the classic
	// broker one from the same listener. It carries its own state and lock; the
	// two tiers share nothing but the process (Q528).
	scaleSet  *scalesetstub.Stub
	jobQueues map[string][]message // sessionID → jobs enqueued directly to it
	// ownerPending holds jobs awaiting opportunistic delivery to any live
	// session of an owner — GitHub redelivers a job whose session went away
	// before acquiring it to any other polling session (M1 Investigation C/D).
	// A job is moved here when its session is deleted/consumed with the job
	// still queued, or enqueued here directly when its target session is
	// already dead. handleMessage drains a session's own queue first, then the
	// owner pool. Without it, a job stranded on a recycled session's queue
	// would be lost — fakegithub's per-session queue would otherwise be a
	// fidelity gap relative to GitHub's pool-wide delivery (Q114). A job whose
	// target session simply stops polling — without ever being deleted — ages
	// into the pool on sessionQueueGrace instead (Q436).
	ownerPending    map[string][]message // owner ("<group>-…" prefix-keyed) → jobs
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
	deadPolls            map[string]int          // dead sessionID → GET /message count since death
	requestSessions      map[string]string       // runnerRequestId → delivering sessionID
	// jobTokens maps a job's runner_request_id to the job-scoped token issued in
	// its acquirejob response (the SystemVssConnection AccessToken). RenewJob must
	// present it: the real run service rejects the broker session token for per-job
	// renewal with 401 "Not authorized for this job" (Q247). Guarded by mu.
	jobTokens map[string]string

	// Lease / acquire-vs-redeliver fidelity (Q154). Opt-in, owner-scoped. When
	// enabled, a delivered job is leased rather than dropped: AcquireJob consumes
	// it permanently (an acquired job is never redelivered), while an unclaimed
	// lease expiry returns it for redelivery (a skipped job is not lost). See the
	// package doc. The four maps/fields are guarded by mu; redelivery is an atomic
	// so the hot GET /message and AcquireJob paths can check it without the lock.
	redelivery            atomic.Bool
	redeliveryOwnerPrefix string                // "" = all owners
	redeliveryLease       time.Duration         // unclaimed-lease window
	leased                map[string]*leasedJob // runnerRequestId → in-flight delivery
	acquiredReqs          map[string]bool       // runnerRequestId → claimed (terminal)
	deliveryCount         map[string]int        // runnerRequestId → times delivered

	// longPollHold is how long GET /message holds an idle connection open before
	// returning 202, mirroring the real GitHub broker's long-poll window. Zero
	// (the default) returns 202 immediately, which keeps the unit tests fast.
	// The e2e deployment sets it to a realistic value: without the hold the AGC's
	// empty-poll loop spins at network speed and a replacement listener reaches
	// its 50-empty-poll idle-shutdown threshold within milliseconds, collapsing a
	// RunnerGroup's pool back to one listener while the busy listener's worker pod
	// runs — which stranded the next job and flaked E2E_AGC_SingleUseSelfHeal
	// (Q148). The real broker holds ~50s, so in production those 50 empty polls
	// span ~40min and never fire mid-job. Set once at startup; read without mu.
	longPollHold time.Duration

	// sessionQueueGrace is how long a job may sit undelivered on a specific
	// session's queue before it ages into the owner's pending pool (Q436). Set
	// once at startup; read without mu. See sweepStaleQueuesLocked.
	sessionQueueGrace time.Duration

	// forceCancelPaths records the request path of every force-cancel call — the
	// provisioner's fast honest ending for a worker removed before it ran (Q683).
	// Always accepted 202, the answer live GitHub gave the standalone call.
	forceCancelPaths []string
	// rerunPaths records the request path of every accepted (201) rerun-failed-jobs
	// call the AGC has made, in order — the eviction auto-retry signal (Q421).
	// Guarded by mu.
	rerunPaths []string
	// refusedRerunPaths records the rerun-failed-jobs calls refused with the 403
	// still-running answer, separately from rerunPaths so the accepted count keeps
	// meaning "the run was re-run" while a spec asserts the refusal window (Q517).
	// Guarded by mu.
	refusedRerunPaths []string
	// nonConcludedRuns holds run_ids whose original run has not concluded:
	// rerun-failed-jobs for one is refused like real GitHub refuses it (Q517).
	// Marked per run via /control/runstate. Guarded by mu.
	nonConcludedRuns map[string]bool
	// cancelledRuns holds run_ids the run read reports as concluded `cancelled`,
	// the state that stands a graceful-deletion recovery down instead of re-running
	// it (Q811). Marked per run via /control/runstate. Real GitHub still ACCEPTS
	// rerun-failed-jobs for such a run (measured 2026-08-05), so this deliberately
	// does not refuse the POST: what must not happen is the AGC making the call at
	// all, and a fake that refused it would hide a regression rather than fail on
	// it. Guarded by mu.
	cancelledRuns map[string]bool
}

// longPollTick is how often a held GET /message rechecks for a freshly enqueued
// job, bounding job-delivery latency under the long-poll to one tick. Cheap at
// the handful of concurrent idle pollers a test cluster holds.
const longPollTick = 50 * time.Millisecond

// defaultSessionQueueGrace bounds how long a job enqueued onto one specific
// session may stay reachable only through that session (Q436).
//
// A session that is polling drains its own queue within one longPollTick, so a
// job only ages if its target session has stopped polling — it is running a
// job, or it was recycled but its DELETE never landed. GitHub has no such
// state: work sits in the pool until some session polls for it, so the queue a
// spec addresses must not be able to hold a job hostage. The window is well
// above a poll cycle (a healthy targeted delivery is never stolen) and well
// below any spec's Eventually budget.
const defaultSessionQueueGrace = 30 * time.Second

// defaultRedeliveryLease is the unclaimed-lease window used when redelivery mode
// is enabled without an explicit leaseMs. Short so a skipped job is redelivered
// promptly in a test, but comfortably longer than the gap between a delivery and
// the AcquireJob the admission gate issues when it admits.
const defaultRedeliveryLease = 2 * time.Second

// leasedJob is a job delivered under the Q154 redelivery model but not yet
// acquired: it is held out of circulation until AcquireJob claims it (terminal)
// or its lease expires and it is redelivered.
type leasedJob struct {
	owner       string
	msg         message
	deliveredAt time.Time
}

type message struct {
	MessageID   int64  `json:"messageId"`
	MessageType string `json:"messageType"`
	Body        string `json:"body"`
	// reqID is the job's runner_request_id, captured at enqueue so the Q154 lease
	// model can correlate a delivered message with the later AcquireJob (which
	// references the job by jobMessageId == runner_request_id). Unexported, so it
	// is never serialised to the broker client.
	reqID string
	// queuedAt is when the job was placed on a specific session's queue, so an
	// undelivered job can age into the owner pool (Q436). Zero once the job is
	// in the owner pool — pooled jobs are already deliverable to any session.
	// Unexported, so it is never serialised to the broker client.
	queuedAt time.Time
}

func newServer() *server {
	return &server{
		scaleSet:          scalesetstub.New(externalBase),
		sessions:          brokerstub.NewSessions(),
		jobQueues:         make(map[string][]message),
		ownerPending:      make(map[string][]message),
		runners:           make(map[int64]*runnerRecord),
		runnerNames:       make(map[string]int64),
		clientRunners:     make(map[string]int64),
		consumedAgents:    make(map[int64]bool),
		deadPolls:         make(map[string]int),
		requestSessions:   make(map[string]string),
		jobTokens:         make(map[string]string),
		leased:            make(map[string]*leasedJob),
		acquiredReqs:      make(map[string]bool),
		deliveryCount:     make(map[string]int),
		nonConcludedRuns:  make(map[string]bool),
		cancelledRuns:     make(map[string]bool),
		sessionQueueGrace: defaultSessionQueueGrace,
	}
}

func (s *server) mainMux() http.Handler {
	mux := http.NewServeMux()
	// GitHub App token exchange — path includes installation ID
	mux.HandleFunc("/app/installations/", s.handleInstallationToken)
	// Runner registration API (GHES-style /api/v3 prefix, matching what
	// GithubRegistrar derives for a non-github.com host)
	mux.HandleFunc("/api/v3/", s.handleRunnerAPI)
	// REST endpoints the AGC addresses relative to GITHUB_API_BASE_URL itself
	// rather than through the GHES /api/v3 prefix — the eviction auto-retry is
	// the only one today (Q421). Both shapes route to the same handler because
	// which one the AGC uses depends on how its API base is configured.
	mux.HandleFunc("/repos/", s.handleReposAPI)
	// Broker endpoints
	mux.HandleFunc("/token", s.handleToken)
	mux.HandleFunc("/session", s.handleSession)
	mux.HandleFunc("/message", s.handleMessage)
	mux.HandleFunc("/acquirejob", s.handleAcquireJob)
	mux.HandleFunc("/renewjob", s.handleRenewJob)
	// Runner-scale-set protocol (Q528). The Actions Service and message-queue
	// routes are scale-set-only, so they mount straight onto the stub; the two
	// REST bootstrap hops live under the GHES /api/v3 prefix the scaleset client
	// derives for a non-github.com host, and reach the stub through
	// handleRunnerAPI below.
	mux.Handle("/_apis/", s.scaleSet.Handler())
	mux.Handle("/queue/", s.scaleSet.Handler())
	// Everything else is unserved. Registered explicitly rather than left to the
	// mux's own NotFound so the answer carries the marker below.
	mux.HandleFunc("/", notServed)
	return mux
}

// unservedHeader marks a response the fake produced because nothing serves the
// path, as opposed to a handler's own 404 for a resource that does not exist.
// The two are the same status, and the endpoint-parity gate has to tell them
// apart: deleting an unregistered runner 404s from a route that exists, while
// Q811's run read 404'd from a route that did not, and only the second is a
// venue that has drifted from the code (devtools/e2e/endpointparity).
const unservedHeader = "X-Fakegithub-Unserved"

// notServed answers a path no handler claims, marked so a caller can tell.
func notServed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(unservedHeader, "1")
	http.NotFound(w, r)
}

// scaleSetAPIV3 serves the two REST hops of the scale-set bootstrap — the
// registration-token call and the RemoteAuth runner-registration hop — off the
// /api/v3 prefix. The stub's patterns are absolute paths, so the prefix is
// stripped before it sees them.
func (s *server) scaleSetAPIV3() http.Handler {
	return http.StripPrefix("/api/v3", s.scaleSet.Handler())
}

func (s *server) controlMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/control/enqueue", s.handleEnqueue)
	mux.HandleFunc("/control/sessions", s.handleListSessions)
	mux.HandleFunc("/control/session-versions", s.handleSessionVersions)
	mux.HandleFunc("/control/acquirejob", s.handleSetAcquireJob)
	mux.HandleFunc("/control/singleuse", s.handleSetSingleUse)
	mux.HandleFunc("/control/redelivery", s.handleSetRedelivery)
	mux.HandleFunc("/control/jobstats", s.handleJobStats)
	mux.HandleFunc("/control/reruns", s.handleReruns)
	mux.HandleFunc("/control/runstate", s.handleSetRunState)
	mux.HandleFunc("/control/scaleset/enqueue", s.handleScaleSetEnqueue)
	mux.HandleFunc("/control/scaleset/acquireflow", s.handleSetScaleSetAcquireFlow)
	mux.HandleFunc("/control/scaleset/state", s.handleScaleSetState)
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
//
// The scale-set tier's two REST bootstrap hops share this prefix and are handed
// off to the scale-set stub. They address distinct paths, so the two tiers'
// runner registries never have to agree: the scale-set tier mints its runners
// through generatejitconfig on the Actions Service, and reaches the REST
// list/delete routes below only on a runner-name 409 this venue does not produce.
func (s *server) handleRunnerAPI(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasSuffix(path, "/rerun-failed-jobs") && r.Method == http.MethodPost {
		s.handleRerunFailedJobs(w, r)
		return
	}
	if strings.HasSuffix(path, "/force-cancel") && r.Method == http.MethodPost {
		s.handleForceCancel(w, r)
		return
	}
	// The run read (Q811) is served on both prefixes for the same reason the two
	// calls above are: the AGC addresses GITHUB_API_BASE_URL, which the e2e venue
	// points at /api/v3, while the unprefixed form stays reachable for a caller
	// configured without it.
	if id, ok := runIDFromRunPath(path); ok && r.Method == http.MethodGet {
		s.handleRunStatus(w, id)
		return
	}
	if path == "/api/v3/actions/runner-registration" ||
		strings.HasSuffix(path, "/actions/runners/registration-token") {
		s.scaleSetAPIV3().ServeHTTP(w, r)
		return
	}
	idx := strings.Index(path, "/actions/runners")
	if idx < 0 {
		notServed(w, r)
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
		if clientID := brokerstub.AssertionIssuer(r); clientID != "" {
			s.mu.Lock()
			// clientRunners entries survive record consumption (see
			// consumeSession) precisely so this lookup can reject the
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
	brokerstub.WriteToken(w, fmt.Sprintf("bearer-%d", s.tokenCounter.Add(1)))
}

// handleSession serves POST /session and DELETE /session.
func (s *server) handleSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		bearer := brokerstub.Bearer(r)

		var reqBody struct {
			OwnerName string `json:"ownerName"`
			Agent     struct {
				ID      int64  `json:"id"`
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"agent"`
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)

		s.mu.Lock()
		// Honour the single-use simulation only for in-scope owners. Agent IDs are
		// not globally unique across tenants — each AGC's StubRegistrar counts from
		// the same base — so an out-of-scope tenant's freshly recycled agent can
		// collide by ID with an in-scope tenant's consumed agent in the global
		// consumedAgents map. Without the owner guard that collision 401s a healthy
		// session creation, killing the non-single-use tenant's baseline (Q135). An
		// out-of-scope owner therefore behaves exactly as non-single-use mode.
		if s.singleUse.Load() && s.inSingleUseScopeLocked(reqBody.OwnerName) {
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
		s.mu.Unlock()

		// Record agent.version (the runnerVersion the AGC pins) alongside the
		// owner and agent ID in the shared registry. GitHub validates the version
		// at session creation; capturing it lets specs assert it is non-empty and
		// correct (Q71/Q118 runner-version contract).
		id := s.sessions.Create(reqBody.OwnerName, reqBody.Agent.ID, reqBody.Agent.Version, bearer)
		brokerstub.WriteJSON(w, http.StatusOK, map[string]string{"sessionId": id})

	case http.MethodDelete:
		id, ok := s.sessions.ResolveDelete(r.URL.Query().Get("sessionId"), brokerstub.Bearer(r))
		if ok {
			// A listener recycling its agent deletes the old session; carry any
			// jobs still queued on it to the owner pool for redelivery.
			sess, _ := s.sessions.Get(id)
			s.mu.Lock()
			s.requeueLocked(id, sess.Owner)
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
//
// Like the real broker it long-polls: a live session with no queued job holds
// the connection open for up to s.longPollHold (returning the moment a job is
// enqueued) before answering 202. The consumed-session signature is never
// held — the AGC detects a dead single-use session by the prompt
// 200-empty-then-401 sequence. See the longPollHold field doc for why the hold
// matters (Q148).
func (s *server) handleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("sessionId")
	// The session's owner is immutable once created, so resolve it once from the
	// shared registry (outside the poll loop and its lock) rather than per-tick.
	sess, _ := s.sessions.Get(id)
	owner := sess.Owner
	deadline := time.Now().Add(s.longPollHold)
	for {
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
		// Under the Q154 redelivery model, first return any of this owner's leases
		// that expired unclaimed to the deliverable pool, so a skipped job becomes
		// redeliverable on this very poll.
		inRedeliver := s.redelivery.Load() && s.inRedeliveryScopeLocked(owner)
		if inRedeliver {
			s.expireLeasesLocked(owner)
		}
		// Likewise pool any of this owner's jobs left undelivered on a session
		// that stopped polling (Q436), so this poll can carry them.
		s.sweepStaleQueuesLocked(owner, time.Now())
		// Deliver from the session's own queue first, then fall back to the owner's
		// pending pool (a job whose original session was recycled away, or one
		// redelivered after an expired lease). Returning the message under the lock
		// keeps the dequeue atomic.
		var msg *message
		if q := s.jobQueues[id]; len(q) > 0 {
			m := q[0]
			m.queuedAt = time.Time{} // delivered: no longer queued on a session
			s.jobQueues[id] = q[1:]
			msg = &m
		} else if owner != "" {
			if p := s.ownerPending[owner]; len(p) > 0 {
				m := p[0]
				s.ownerPending[owner] = p[1:]
				msg = &m
			}
		}
		// Lease the delivered job: it is now invisible to further polls until it is
		// either acquired (consumed) or its lease expires (redelivered).
		if msg != nil && inRedeliver {
			s.leaseDeliveredLocked(owner, *msg)
		}
		s.mu.Unlock()
		if msg != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(*msg)
			return
		}
		// No job queued. Hold the connection until one arrives or the hold expires,
		// rechecking every tick. Honour client disconnect so a recycling listener
		// (or suite teardown) is never blocked by an in-flight long-poll.
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

	if s.singleUse.Load() && reqBody.JobMessageID != "" {
		s.mu.Lock()
		sid, ok := s.requestSessions[reqBody.JobMessageID]
		if ok {
			delete(s.requestSessions, reqBody.JobMessageID)
		}
		s.mu.Unlock()
		if ok {
			sess, _ := s.sessions.Get(sid)
			if s.inSingleUseScopeLocked(sess.Owner) {
				s.consumeSession(sid)
			}
		}
	}

	// Q154: an acquired job is terminal — drop its lease and record the claim so
	// it is never redelivered. This is the GitHub behaviour the admission gate
	// assumes: once acquired, the runner owns the job (an unrenewed lock is
	// cancelled, not handed back), so a ceiling-held acquired job does not return
	// to the queue as a duplicate.
	if s.redelivery.Load() && reqBody.JobMessageID != "" {
		s.mu.Lock()
		s.recordAcquireLocked(reqBody.JobMessageID)
		s.mu.Unlock()
	}

	// Bumped only after the single-use consumption and the acquire claim are
	// committed, so the counter never signals an acquisition whose state is still
	// in flight (the Q490 rule; today it only mints the plan/token IDs below).
	n := s.acquireCount.Add(1)

	w.Header().Set("Content-Type", "application/json")
	s.mu.Lock()
	custom := s.acquireResponse
	s.mu.Unlock()
	if custom != nil {
		_ = json.NewEncoder(w).Encode(custom)
		return
	}
	// Issue a job-scoped token in the SystemVssConnection endpoint — the shape the
	// real run service returns and the runner uses to renew the job lock. RenewJob
	// must present it, not the broker session token (Q247). Record it keyed by the
	// job's runner_request_id (== jobMessageId) so handleRenewJob can enforce it.
	jobToken := fmt.Sprintf("jobtoken-%d", n)
	if reqBody.JobMessageID != "" {
		jobToken = "jobtoken-" + reqBody.JobMessageID
		s.mu.Lock()
		s.jobTokens[reqBody.JobMessageID] = jobToken
		s.mu.Unlock()
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"plan": map[string]string{
			"planId": fmt.Sprintf("plan-%d", n),
		},
		"resources": map[string]any{
			"endpoints": []map[string]any{{
				"name": "SystemVssConnection",
				"url":  externalBase(r),
				"authorization": map[string]any{
					"scheme":     "OAuth",
					"parameters": map[string]string{"AccessToken": jobToken},
				},
			}},
		},
	})
}

// handleRenewJob serves POST /renewjob. It enforces the Q247 contract: a job's
// lock is renewed with the job-scoped token issued in its acquirejob response (the
// SystemVssConnection AccessToken), not the broker session token. When a token was
// recorded for the job, a mismatching Authorization header is rejected 401 "Not
// authorized for this job" — the exact signature the real run service returns.
// Jobs with no recorded token (a custom acquire response without an endpoint) are
// renewed leniently so specs that override the response are unaffected.
func (s *server) handleRenewJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var reqBody struct {
		JobID string `json:"jobId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&reqBody)

	s.mu.Lock()
	want, recorded := s.jobTokens[reqBody.JobID]
	s.mu.Unlock()

	if recorded && r.Header.Get("Authorization") != "Bearer "+want {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"source":       "actions-run-service",
			"statusCode":   401,
			"errorMessage": "Not authorized for this job",
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"lockedUntil": time.Now().Add(10 * time.Minute).Format(time.RFC3339),
	})
}

// inSingleUseScopeLocked reports whether ownerName falls within the configured
// single-use simulation scope. The single-use JIT lifecycle is opt-in per
// RunnerGroup via the owner prefix so specs running in parallel against this
// shared instance are unaffected (an empty prefix scopes to all owners).
//
// Both the consumption side (handleAcquireJob) and the rejection side
// (handleSession) must honour the same scope: agent IDs are not globally unique
// across tenants, so an out-of-scope tenant's recycled agent can collide by ID
// with an in-scope tenant's consumed agent in the global consumedAgents map.
// Scoping the rejection by owner keeps one tenant's single-use simulation from
// spuriously rejecting another tenant's session creation (Q135). Caller must
// hold s.mu.
func (s *server) inSingleUseScopeLocked(ownerName string) bool {
	return s.singleUseOwnerPrefix == "" || strings.HasPrefix(ownerName, s.singleUseOwnerPrefix)
}

// consumeSession marks a session's agent as consumed and the session as dead.
// It does its own locking: the single-use bookkeeping (consumed agents, runner
// records, dead-poll counter, job requeue) is set under s.mu, then the shared
// registry is told to mark the session inactive. The dead-poll counter is set
// before the registry flips the session so a racing GET /message sees the dead
// signature (checked first) rather than a bare 202.
func (s *server) consumeSession(sessionID string) {
	sess, _ := s.sessions.Get(sessionID)
	s.mu.Lock()
	if sess.AgentID > 0 {
		s.consumedAgents[sess.AgentID] = true
		if rec, ok := s.runners[sess.AgentID]; ok {
			delete(s.runners, sess.AgentID)
			delete(s.runnerNames, rec.Name)
			// clientRunners entry is kept so /token can 401 the dead credential.
		}
	}
	s.deadPolls[sessionID] = 0
	s.requeueLocked(sessionID, sess.Owner)
	s.mu.Unlock()
	s.sessions.SetInactive(sessionID)
}

// requeueLocked moves any jobs still queued on a now-dead session to its
// owner's pending pool so a live session can pick them up. Caller must hold
// s.mu and pass the session's owner. The acquired job that triggered
// consumption is already dequeued, so this only carries genuinely undelivered
// jobs.
func (s *server) requeueLocked(sessionID, owner string) {
	q := s.jobQueues[sessionID]
	if len(q) == 0 {
		return
	}
	s.ownerPending[owner] = append(s.ownerPending[owner], pooled(q)...)
	delete(s.jobQueues, sessionID)
}

// pooled clears the per-session queuedAt stamp on jobs moving into an owner
// pool: a pooled job is deliverable to any of the owner's sessions, so it has
// no queue to age out of.
func pooled(msgs []message) []message {
	out := make([]message, len(msgs))
	for i, m := range msgs {
		m.queuedAt = time.Time{}
		out[i] = m
	}
	return out
}

// sweepStaleQueuesLocked moves owner's jobs that have sat undelivered on one
// session's queue for longer than sessionQueueGrace into the owner's pending
// pool, where any live session of that owner can pick them up. Caller must hold
// s.mu.
//
// This is what keeps a job reachable when its target session stops polling
// without being deleted (Q436): a listener recycling a single-use agent deletes
// the old session first, and requeueLocked hands the jobs over — but that
// DELETE is best-effort. When it fails (three timed-out attempts against a
// loaded broker), the AGC logs the session as leaked and moves on, and before
// this sweep the job queued on it was unreachable for the rest of the run: the
// session was still Active so nothing requeued it, and nothing was polling it.
// The real broker cannot reach that state — a job is dispatched pool-wide and
// redelivered until some session acquires it — so the strand was purely an
// artifact of the session-targeted /control/enqueue.
//
// Scoped to one owner so a shared fakegithub never moves another tenant's work,
// and because an owner's own sessions are the only ones that can deliver it:
// ownerName is the listener's registered runner name, kind-scoped as
// "<name>-<agentIndex>" or "rs-<name>-<agentIndex>" (Q677) and stable across
// recycles either way, so the session that replaced the stranded one sweeps its
// predecessor's queue on its next poll.
func (s *server) sweepStaleQueuesLocked(owner string, now time.Time) {
	if owner == "" || s.sessionQueueGrace <= 0 {
		return
	}
	for sid, q := range s.jobQueues {
		if sess, ok := s.sessions.Get(sid); !ok || sess.Owner != owner {
			continue
		}
		kept := q[:0:0]
		for _, m := range q {
			if !m.queuedAt.IsZero() && now.Sub(m.queuedAt) >= s.sessionQueueGrace {
				m.queuedAt = time.Time{}
				s.ownerPending[owner] = append(s.ownerPending[owner], m)
				continue
			}
			kept = append(kept, m)
		}
		if len(kept) == 0 {
			delete(s.jobQueues, sid)
		} else {
			s.jobQueues[sid] = kept
		}
	}
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
		reqID:       reqID,
	}

	s.mu.Lock()
	s.requestSessions[reqID] = id
	// Reading session liveness (shared registry) under s.mu keeps the decision
	// atomic with the enqueue: whichever way a concurrent DELETE races, a job
	// placed on jobQueues[id] here is always caught by requeueLocked (which runs
	// under s.mu on delete), so it is never stranded on a dead session's queue.
	sess, _ := s.sessions.Get(id)
	if sess.Active {
		// Target session is live: queue it there so a specific session can be
		// addressed (the single-use spec relies on this to consume one session).
		// Stamped so the job ages into the owner pool if that session turns out
		// to be registered-but-not-polling (Q436) — "Active" only means the
		// broker still holds the session, not that anyone is polling it.
		msg.queuedAt = time.Now()
		s.jobQueues[id] = append(s.jobQueues[id], msg)
	} else {
		// Target session is already gone (recycled between the test's session
		// query and this enqueue): hand the job to the owner pool so the next
		// live session picks it up, mirroring GitHub's pool-wide redelivery.
		s.ownerPending[sess.Owner] = append(s.ownerPending[sess.Owner], msg)
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
	active := s.sessions.ActiveIDs(r.URL.Query().Get("owner"))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(active)
}

// handleSessionVersions is the control API: GET /control/session-versions[?owner=<prefix>]
// It returns a JSON object mapping each active session ID to the agent.version
// (runnerVersion) the AGC sent on CreateSession, so a spec can assert the
// version is non-empty and matches the pinned runner version (Q71/Q118). The
// optional owner prefix scopes the result to one RunnerGroup on a shared
// instance, mirroring handleListSessions.
func (s *server) handleSessionVersions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	versions := s.sessions.Versions(r.URL.Query().Get("owner"))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(versions)
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

// handleSetRedelivery is the control API:
//
//	POST /control/redelivery?enabled=true|false[&owner=<prefix>][&leaseMs=<n>]
//
// Toggles the Q154 lease / acquire-vs-redeliver model. The owner prefix scopes
// it (so parallel specs are unaffected) and leaseMs sets the unclaimed-lease
// window (default 2s). See the package doc.
func (s *server) handleSetRedelivery(w http.ResponseWriter, r *http.Request) {
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
	s.redeliveryOwnerPrefix = r.URL.Query().Get("owner")
	s.redeliveryLease = defaultRedeliveryLease
	if ms := r.URL.Query().Get("leaseMs"); ms != "" {
		if n, perr := strconv.Atoi(ms); perr == nil && n > 0 {
			s.redeliveryLease = time.Duration(n) * time.Millisecond
		}
	}
	s.mu.Unlock()
	s.redelivery.Store(enabled)
	w.WriteHeader(http.StatusOK)
}

// scaleSetIDByName resolves the scale set the AGC registered under name, writing
// the 400/404 and returning false when it cannot. The AGC names a scale set after
// the RunnerSet's single runs-on label, so a spec addresses it by the label it
// declared rather than by an id only the AGC ever saw.
func (s *server) scaleSetIDByName(w http.ResponseWriter, r *http.Request) (int, bool) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return 0, false
	}
	id, ok := s.scaleSet.ScaleSetIDByName(name)
	if !ok {
		http.Error(w, fmt.Sprintf("no scale set named %q is registered", name), http.StatusNotFound)
		return 0, false
	}
	return id, true
}

// handleScaleSetEnqueue is the control API:
//
//	POST /control/scaleset/enqueue?name=<scale set>
//
// Queues one job on the scale set and returns the identity its assignment will
// carry — {runnerRequestId, jobId, ownerName, repositoryName, workflowRunId} — so a
// spec asserts the worker pod against what the server actually delivered rather
// than against values it restated.
func (s *server) handleScaleSetEnqueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, ok := s.scaleSetIDByName(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.scaleSet.Enqueue(id))
}

// handleSetScaleSetAcquireFlow is the control API:
//
//	POST /control/scaleset/acquireflow?ghes=true|false
//
// Selects the GHES JobAvailable→acquire flow or the dotcom auto-assign one
// (the default). Unlike the classic single-use and redelivery toggles this is
// process-wide rather than owner-scoped — the flow is a property of the backend,
// not of one tenant — so a spec that flips it must not run in parallel with
// another scale-set spec.
func (s *server) handleSetScaleSetAcquireFlow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ghes, err := strconv.ParseBool(r.URL.Query().Get("ghes"))
	if err != nil {
		http.Error(w, "ghes=true|false required", http.StatusBadRequest)
		return
	}
	s.scaleSet.SetGHESAcquireFlow(ghes)
	w.WriteHeader(http.StatusOK)
}

// handleScaleSetState is the control API:
//
//	GET /control/scaleset/state?name=<scale set>
//
// Reports what the server saw: whether a message-queue session is live, how many
// jobs are assigned-but-not-completed, the acquirejobs/generatejitconfig call
// counts, and the call log. A spec asserts acquisition against this rather than
// inferring it from AGC logs — an absent session and an absent claim are otherwise
// indistinguishable from a job that was never enqueued.
func (s *server) handleScaleSetState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, ok := s.scaleSetIDByName(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"scaleSetId":       id,
		"activeSession":    s.scaleSet.HasActiveSession(id),
		"assignedJobs":     s.scaleSet.AssignedJobCount(id),
		"acquireJobsCalls": s.scaleSet.AcquireJobsCalls(),
		"generateJITCalls": s.scaleSet.GenerateJITCalls(),
		"calls":            s.scaleSet.Calls(),
	})
}

// handleJobStats is the control API: GET /control/jobstats?requestId=<id>
// Returns the Q154 lease state for a job, keyed by its runner_request_id:
// {"deliveries": <count>, "leased": <bool>, "acquired": <bool>}. A test asserts
// `acquired` with `deliveries` staying flat (acquired job not redelivered) or
// `deliveries` climbing while `acquired` is false (skipped job redelivered).
func (s *server) handleJobStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req := r.URL.Query().Get("requestId")
	if req == "" {
		http.Error(w, "requestId required", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	_, leased := s.leased[req]
	stats := map[string]any{
		"deliveries": s.deliveryCount[req],
		"leased":     leased,
		"acquired":   s.acquiredReqs[req],
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stats)
}

// handleReposAPI routes the /repos/{owner}/{repo}/... REST endpoints the AGC
// addresses directly off GITHUB_API_BASE_URL. Only the run read, rerun-failed-jobs
// and force-cancel are served; anything else 404s, as an unimplemented endpoint should.
func (s *server) handleReposAPI(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/rerun-failed-jobs") && r.Method == http.MethodPost {
		s.handleRerunFailedJobs(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/force-cancel") && r.Method == http.MethodPost {
		s.handleForceCancel(w, r)
		return
	}
	if id, ok := runIDFromRunPath(r.URL.Path); ok && r.Method == http.MethodGet {
		s.handleRunStatus(w, id)
		return
	}
	notServed(w, r)
}

// runIDFromRunPath reports the run_id of a bare .../actions/runs/{run_id} path —
// the run itself, not one of its sub-resources, which carry a further segment.
func runIDFromRunPath(path string) (string, bool) {
	const marker = "/actions/runs/"
	i := strings.Index(path, marker)
	if i < 0 {
		return "", false
	}
	id := path[i+len(marker):]
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// handleRunStatus serves the run read the deletion arm's cancel check makes before
// it asks for a re-run (Q811):
//
//	GET /api/v3/repos/{owner}/{repo}/actions/runs/{run_id}
//
// It answers off the same /control/runstate the re-run refusal keys on, so the two
// calls cannot disagree about one run: a run marked not-yet-concluded reads
// in_progress with a null conclusion, exactly as it refuses the re-run with 403; any
// other run reads completed/failure, the conclusion a disrupted run reaches at live
// GitHub (measured, Q459), which is what lets the recovery proceed to its re-run.
//
// A cancelled conclusion — the state that stands the recovery down — is reachable by
// marking the run cancelled through /control/runstate, so a spec asserting the
// stand-down and one asserting the re-run drive the same endpoint.
func (s *server) handleRunStatus(w http.ResponseWriter, runID string) {
	s.mu.Lock()
	status, conclusion := "completed", "failure"
	switch {
	case s.cancelledRuns[runID]:
		conclusion = "cancelled"
	case s.nonConcludedRuns[runID]:
		status, conclusion = "in_progress", ""
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	body := map[string]any{"id": runID, "status": status, "conclusion": conclusion}
	if conclusion == "" {
		body["conclusion"] = nil
	}
	_ = json.NewEncoder(w).Encode(body)
}

// handleRerunFailedJobs serves the eviction auto-retry call:
//
//	POST /api/v3/repos/{owner}/{repo}/actions/runs/{run_id}/rerun-failed-jobs
//
// A run marked non-concluded via /control/runstate is refused 403 "This workflow
// is already running" — real GitHub's answer until it concludes the original run,
// and the refusal the AGC's retry loop discriminates by message (Q503/Q517).
// Otherwise GitHub answers 201 with an empty object, and so does this. Accepted
// and refused paths are recorded separately so /control/reruns can report which
// runs were re-run vs still being refused (Q421/Q517).
func (s *server) handleRerunFailedJobs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	refused := s.nonConcludedRuns[rerunRunID(r.URL.Path)]
	if refused {
		s.refusedRerunPaths = append(s.refusedRerunPaths, r.URL.Path)
	} else {
		s.rerunPaths = append(s.rerunPaths, r.URL.Path)
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if refused {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message":           "This workflow is already running",
			"documentation_url": "https://docs.github.com/rest/actions/workflow-runs#re-run-failed-jobs-from-a-workflow-run",
			"status":            "403",
		})
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{})
}

// handleForceCancel serves the abandoned-run fast ending (Q683):
//
//	POST /api/v3/repos/{owner}/{repo}/actions/runs/{run_id}/force-cancel
//
// Always 202 Accepted — the answer live GitHub gave the standalone call in the
// told-nothing state (measured 2026-08-05, the Q645 plan doc). Calls are recorded
// for /control/reruns so a spec can assert the ending was requested.
func (s *server) handleForceCancel(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.forceCancelPaths = append(s.forceCancelPaths, r.URL.Path)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{})
}

// rerunRunID extracts the run_id segment from a rerun-failed-jobs path
// (.../actions/runs/{run_id}/rerun-failed-jobs).
func rerunRunID(path string) string {
	p := strings.TrimSuffix(path, "/rerun-failed-jobs")
	return p[strings.LastIndex(p, "/")+1:]
}

// handleSetRunState is the control API:
//
//	POST /control/runstate?run=<run_id>&concluded=true|false[&conclusion=cancelled]
//
// concluded=false marks the run as not yet concluded, so rerun-failed-jobs for it
// is refused the way real GitHub refuses it until it concludes the original run —
// after an ungraceful kill that takes ~10 minutes (measured 9m36s, Q503).
// concluded=true restores acceptance. Runs never marked are concluded, so only a
// spec that opts its own run in sees refusals (Q517).
//
// conclusion=cancelled additionally makes the run read `cancelled` (Q811), the state
// the graceful-deletion arm stands down on rather than re-queueing a job a human
// stopped. It is only meaningful with concluded=true, since a run still in progress
// has no conclusion; any other conclusion value restores the default `failure`.
func (s *server) handleSetRunState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runID := r.URL.Query().Get("run")
	if runID == "" {
		http.Error(w, "run required", http.StatusBadRequest)
		return
	}
	concluded, err := strconv.ParseBool(r.URL.Query().Get("concluded"))
	if err != nil {
		http.Error(w, "concluded=true|false required", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if concluded {
		delete(s.nonConcludedRuns, runID)
	} else {
		s.nonConcludedRuns[runID] = true
	}
	if r.URL.Query().Get("conclusion") == "cancelled" {
		s.cancelledRuns[runID] = true
	} else {
		delete(s.cancelledRuns, runID)
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// handleReruns is the control API: GET /control/reruns[?run=<substring>]
// Returns {"count": <n>, "paths": [...], "refusedCount": <n>, "refusedPaths": [...]}:
// accepted (201) rerun-failed-jobs calls in count/paths — the Q421 "the run was
// re-run" signal — and the 403 still-running refusals separately, so a spec can
// assert the refusal window without inflating its accepted count (Q517). Both are
// optionally filtered to paths containing `run`. The filter is what makes the
// endpoint usable from a spec running beside others on the shared instance: an
// unfiltered count is process-wide, so a spec asserting "no rerun fired for MY run"
// scopes to its own owner/repo (Q421).
func (s *server) handleReruns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	filter := r.URL.Query().Get("run")
	match := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, p := range in {
			if filter == "" || strings.Contains(p, filter) {
				out = append(out, p)
			}
		}
		return out
	}
	s.mu.Lock()
	accepted := match(s.rerunPaths)
	refused := match(s.refusedRerunPaths)
	forceCancels := match(s.forceCancelPaths)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"count": len(accepted), "paths": accepted,
		"refusedCount": len(refused), "refusedPaths": refused,
		"forceCancelCount": len(forceCancels), "forceCancelPaths": forceCancels,
	})
}

// inRedeliveryScopeLocked reports whether ownerName is within the configured
// redelivery scope (empty prefix = all owners). Caller must hold s.mu.
func (s *server) inRedeliveryScopeLocked(ownerName string) bool {
	return s.redeliveryOwnerPrefix == "" || strings.HasPrefix(ownerName, s.redeliveryOwnerPrefix)
}

// leaseDeliveredLocked records a just-delivered job as leased to owner and counts
// the delivery. An already-acquired job is never re-leased (defensive: an
// acquired job is removed from the deliverable queues). Caller must hold s.mu.
func (s *server) leaseDeliveredLocked(owner string, msg message) {
	if s.acquiredReqs[msg.reqID] {
		return
	}
	s.deliveryCount[msg.reqID]++
	s.leased[msg.reqID] = &leasedJob{owner: owner, msg: msg, deliveredAt: time.Now()}
}

// expireLeasesLocked returns owner's leases that have aged past the lease window
// without being acquired to the owner pending pool, where the next poll
// redelivers them. Already-acquired leases are simply dropped. Caller must hold
// s.mu.
func (s *server) expireLeasesLocked(owner string) {
	lease := s.redeliveryLease
	if lease <= 0 {
		lease = defaultRedeliveryLease
	}
	now := time.Now()
	for req, lj := range s.leased {
		if lj.owner != owner {
			continue
		}
		if s.acquiredReqs[req] {
			delete(s.leased, req)
			continue
		}
		if now.Sub(lj.deliveredAt) >= lease {
			s.ownerPending[owner] = append(s.ownerPending[owner], lj.msg)
			delete(s.leased, req)
		}
	}
}

// recordAcquireLocked marks an in-scope job as acquired (terminal) and drops any
// outstanding lease, so it is never redelivered. The owner is resolved from the
// lease if present, else from the delivering session. Caller must hold s.mu.
func (s *server) recordAcquireLocked(req string) {
	owner := ""
	if lj, ok := s.leased[req]; ok {
		owner = lj.owner
	} else if sid, ok := s.requestSessions[req]; ok {
		sess, _ := s.sessions.Get(sid)
		owner = sess.Owner
	}
	if !s.inRedeliveryScopeLocked(owner) {
		return
	}
	delete(s.leased, req)
	s.acquiredReqs[req] = true
}
