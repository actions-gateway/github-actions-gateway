// Investigation H (Q645/Q676): what does the run service do with a completion
// for an acquired-but-never-run assignment — re-dispatch the job, or conclude
// it, and with which conclusion?
//
// Set PROBE_ABANDONED_TEST=true to run this scenario instead of the classic
// broker probe. The Q628 fix releases an acquired-but-never-run job assignment
// (worker pod reaped while Pending, no runner binary ever registered) with
// POST {run_service_url}/completejob. The 2026-08-04 run measured
// result=abandoned: the run concluded SUCCESS one second later — a false green
// (the Q645 findings). PROBE_ABANDONED_RESULT re-runs the same instrument with
// a candidate remedy value (Q676), and PROBE_ABANDONED_RERUN_CHECK=true adds
// the half a conclusion alone cannot prove: whether the concluded run gives
// POST /actions/runs/{id}/rerun-failed-jobs a target.
//
// One run, two repo-level JIT runners on the probe label:
//
//	installation token
//	  → POST {api}/repos/{owner}/{repo}/actions/runners/generate-jitconfig  (A and B)
//	  → RFC 7523 OAuth exchange per runner (githubapp.FetchRunnerOAuthToken)
//	  → POST {serverUrlV2}session per runner                (broker v2, Q267 flow)
//	  → both sessions long-poll; the fixture job fans out to both
//	  → A: POST {run_service_url}/acquirejob
//	  → A: POST {run_service_url}/completejob result=<configured>   [T0]
//	  → A's session deleted, runner deregistered (the listener's post-job recycle)
//	  → window: B keeps polling (a post-T0 RunnerJobRequest is a re-dispatch)
//	            while the fixture job's REST status is polled for a conclusion
//	  → cleanup: cancel the fixture run, delete B's session, deregister both
//
// Every broker and run-service call is issued by the shipping broker package,
// so a live run is evidence about the exchange the AGC's listener actually
// performs (listener/job.go), not a probe-local dialect. The JIT registration
// and REST calls mirror agentpool.GithubRegistrar, which lives in cmd/agc's
// internal tree and is reproduced here.
//
// Required environment variables:
//
//	GITHUB_APP_ID              - GitHub App numeric ID
//	GITHUB_APP_PRIVATE_KEY     - Path to PEM file, or PEM literal
//	GITHUB_APP_INSTALLATION_ID - Installation ID for the target repo
//	GITHUB_ORG_URL             - Repository URL (https://github.com/{owner}/{repo});
//	                             must be repo-level — the org Default runner group
//	                             refuses public repositories (see testing.md)
//
// Optional:
//
//	PROBE_ABANDONED_LABEL          - Runner label (default gag-q645-abandoned).
//	PROBE_ABANDONED_RESULT         - completejob result value (default abandoned;
//	                                 any broker.TaskResult value), or "none" to
//	                                 send no completion at all and watch what the
//	                                 acquire lock's lapse does with the job.
//	PROBE_ABANDONED_RERUN_CHECK    - "true" adds the rerun-failed-jobs measurement
//	                                 after a CONCLUDED-run verdict (default off).
//	PROBE_ABANDONED_FORCECANCEL    - "true" issues a standalone REST force-cancel of
//	                                 the fixture run right after T0, with no prior
//	                                 plain cancel — the Q683 candidate remedy. The
//	                                 wire answer and the conclusion it drives are
//	                                 the measurement; canonical use is with
//	                                 result=none (default off).
//	PROBE_ABANDONED_WORKFLOW       - Fixture workflow file (default q645-abandoned-probe.yml).
//	PROBE_ABANDONED_RUNNER_VERSION - Advertised runner version (default 2.335.1,
//	                                 the version cmd/agc/names pins).
//	PROBE_ABANDONED_TIMEOUT        - Wait for the fixture delivery (default 5m).
//	PROBE_ABANDONED_WINDOW         - Post-completion observation window (default 20m,
//	                                 spanning GitHub's ~15m unstarted-job horizon).
//
// The App needs administration: write (runner registration) and actions: write
// (the cleanup run-cancel). The Q645 plan doc carries the experiment's design
// and what would make a result invalid.
package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/actions-gateway/github-actions-gateway/githubapp"
	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
)

// Verdicts. WIRE reports the completejob call itself; the remaining three are
// the mutually-logged outcomes of the observation window.
const (
	verdictWireAccepted = "WIRE-ACCEPTED"
	verdictWireRejected = "WIRE-REJECTED"
	verdictRedispatched = "REDISPATCHED"
	verdictConcluded    = "CONCLUDED"
	verdictNoSignal     = "NO-SIGNAL"
)

// abandonedConfig holds the parsed environment for the abandoned-completion
// scenario.
type abandonedConfig struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPEM  []byte
	RepoURL        string
	Owner          string
	Repo           string

	Label         string
	Result        broker.TaskResult
	SkipComplete  bool
	RerunCheck    bool
	ForceCancel   bool
	WorkflowFile  string
	RunnerVersion string
	Timeout       time.Duration
	Window        time.Duration
}

// parseAbandonedConfig reads and validates the scenario environment from the
// injected getenv function (normally os.Getenv).
func parseAbandonedConfig(getenv func(string) string) (abandonedConfig, error) {
	var cfg abandonedConfig

	appIDStr, err := mustEnv(getenv, "GITHUB_APP_ID")
	if err != nil {
		return abandonedConfig{}, err
	}
	if _, err := fmt.Sscan(appIDStr, &cfg.AppID); err != nil {
		return abandonedConfig{}, fmt.Errorf("parse GITHUB_APP_ID: %w", err)
	}
	installIDStr, err := mustEnv(getenv, "GITHUB_APP_INSTALLATION_ID")
	if err != nil {
		return abandonedConfig{}, err
	}
	if _, err := fmt.Sscan(installIDStr, &cfg.InstallationID); err != nil {
		return abandonedConfig{}, fmt.Errorf("parse GITHUB_APP_INSTALLATION_ID: %w", err)
	}
	pemValue, err := mustEnv(getenv, "GITHUB_APP_PRIVATE_KEY")
	if err != nil {
		return abandonedConfig{}, err
	}
	cfg.PrivateKeyPEM, err = loadPEM(pemValue)
	if err != nil {
		return abandonedConfig{}, fmt.Errorf("load GITHUB_APP_PRIVATE_KEY: %w", err)
	}
	cfg.RepoURL, err = mustEnv(getenv, "GITHUB_ORG_URL")
	if err != nil {
		return abandonedConfig{}, err
	}
	cfg.Owner, cfg.Repo, err = parseOwnerRepo(cfg.RepoURL)
	if err != nil {
		return abandonedConfig{}, err
	}

	cfg.Label = getenv("PROBE_ABANDONED_LABEL")
	if cfg.Label == "" {
		cfg.Label = "gag-q645-abandoned"
	}
	cfg.Result = broker.TaskResult(getenv("PROBE_ABANDONED_RESULT"))
	if cfg.Result == "" {
		cfg.Result = broker.TaskResultAbandoned
	}
	switch cfg.Result {
	case broker.TaskResultSucceeded, broker.TaskResultSucceededWithIssues, broker.TaskResultFailed,
		broker.TaskResultCanceled, broker.TaskResultSkipped, broker.TaskResultAbandoned:
	case "none":
		cfg.SkipComplete = true
	default:
		return abandonedConfig{}, fmt.Errorf("PROBE_ABANDONED_RESULT %q is not a broker.TaskResult value or \"none\"", cfg.Result)
	}
	cfg.RerunCheck = getenv("PROBE_ABANDONED_RERUN_CHECK") == "true"
	cfg.ForceCancel = getenv("PROBE_ABANDONED_FORCECANCEL") == "true"
	cfg.WorkflowFile = getenv("PROBE_ABANDONED_WORKFLOW")
	if cfg.WorkflowFile == "" {
		cfg.WorkflowFile = "q645-abandoned-probe.yml"
	}
	cfg.RunnerVersion = getenv("PROBE_ABANDONED_RUNNER_VERSION")
	if cfg.RunnerVersion == "" {
		cfg.RunnerVersion = "2.335.1"
	}
	if cfg.Timeout, err = parseDurationEnv(getenv, "PROBE_ABANDONED_TIMEOUT", 5*time.Minute); err != nil {
		return abandonedConfig{}, err
	}
	if cfg.Window, err = parseDurationEnv(getenv, "PROBE_ABANDONED_WINDOW", 20*time.Minute); err != nil {
		return abandonedConfig{}, err
	}
	return cfg, nil
}

// parseOwnerRepo extracts owner and repo from a repository URL. The scenario
// requires repo-level registration, so an org-only URL is rejected.
func parseOwnerRepo(githubURL string) (string, string, error) {
	trimmed := strings.TrimRight(githubURL, "/")
	parts := strings.Split(trimmed, "/")
	// parts: ["https:", "", "host", "owner", "repo"]
	if len(parts) < 5 || parts[3] == "" || parts[4] == "" {
		return "", "", fmt.Errorf("GITHUB_ORG_URL %q must be a repository URL (https://host/owner/repo) for repo-level JIT registration", githubURL)
	}
	return parts[3], parts[4], nil
}

// abandonedProbe carries the scenario dependencies. The HTTP clients and API
// base are injectable so the whole flow runs in unit tests against httptest
// stubs, mirroring the other scenarios.
type abandonedProbe struct {
	log *slog.Logger
	cfg abandonedConfig

	tokens  githubapp.TokenProvider
	hc      *http.Client // REST API + generate-jitconfig + OAuth exchange
	pollHC  *http.Client // broker long-poll + run service (header timeout above the 50s hold)
	apiBase string

	// restPollInterval paces the REST status watch and the observer check during
	// the window (default 15s; tests shorten it).
	restPollInterval time.Duration
	// rerunWait bounds the post-rerun watch for a redelivery (default 2m; tests
	// shorten it).
	rerunWait time.Duration
	// jobTailWait bounds the post-conclusion job-record watch (default 3m; tests
	// shorten it). See watchJobTail.
	jobTailWait time.Duration
}

// jitRunner is one repo-level JIT registration: the runner record's identity
// plus the broker credentials parsed out of its encoded_jit_config blob (the
// same decomposition agentpool.parseJITCredentials performs).
type jitRunner struct {
	ID               int64
	Name             string
	ClientID         string
	AuthorizationURL string
	BrokerURL        string
	Key              *rsa.PrivateKey
	deregistered     bool
}

// brokerSession is one live broker v2 session: the shipping client bound to the
// session's broker URL, plus the AES message key when the server returned one.
type brokerSession struct {
	bc        *broker.Client
	sessionID string
	aesKey    []byte
	closed    bool
}

// newAbandonedProbe builds the scenario. hc and pollHC may be nil to take the
// bounded defaults (httpx.NewClient / broker.NewHTTPClient). Both are wrapped
// in a wire logger so the record shows what GitHub answered, not only what the
// client made of it.
func newAbandonedProbe(logger *slog.Logger, cfg abandonedConfig, provider githubapp.TokenProvider,
	apiBase string, hc, pollHC *http.Client) *abandonedProbe {
	if hc == nil {
		hc = httpx.NewClient()
	}
	if pollHC == nil {
		pollHC = broker.NewHTTPClient()
	}
	return &abandonedProbe{
		log:              logger,
		cfg:              cfg,
		tokens:           provider,
		hc:               wireLoggedClient(hc, logger),
		pollHC:           wireLoggedClient(pollHC, logger),
		apiBase:          strings.TrimSuffix(apiBase, "/"),
		restPollInterval: 15 * time.Second,
		rerunWait:        2 * time.Minute,
		jobTailWait:      3 * time.Minute,
	}
}

// runAbandonedProbe is the Investigation H entry point wired from run().
func runAbandonedProbe(ctx context.Context, logger *slog.Logger, cfg abandonedConfig,
	provider githubapp.TokenProvider, apiBase string) error {
	p := newAbandonedProbe(logger, cfg, provider, apiBase, nil, nil)
	_, err := p.run(ctx)
	return err
}

// run executes the scenario and returns the window verdict (empty when the run
// failed before a verdict could be read).
func (p *abandonedProbe) run(ctx context.Context) (string, error) {
	p.reportStaleRunners(ctx)

	runnerA, err := p.registerRunner(ctx, p.cfg.Label+"-a")
	if err != nil {
		return "", err
	}
	defer p.deregisterRunner(context.WithoutCancel(ctx), runnerA)
	runnerB, err := p.registerRunner(ctx, p.cfg.Label+"-b")
	if err != nil {
		return "", err
	}
	defer p.deregisterRunner(context.WithoutCancel(ctx), runnerB)

	sessA, err := p.openSession(ctx, runnerA)
	if err != nil {
		return "", err
	}
	defer p.closeSession(context.WithoutCancel(ctx), sessA)
	sessB, err := p.openSession(ctx, runnerB)
	if err != nil {
		return "", err
	}
	defer p.closeSession(context.WithoutCancel(ctx), sessB)

	// B starts observing before the acquire, so a re-dispatch prompt enough to
	// land during the acquire/complete exchange still has a listener (see the
	// plan doc's invalid-result conditions).
	obsCtx, stopObs := context.WithCancel(ctx)
	obs := p.startObserver(obsCtx, sessB)
	defer func() {
		stopObs()
		<-obs.done
	}()

	p.log.Info("INVESTIGATION-H: dispatch the fixture workflow NOW if not already queued",
		"workflow", p.cfg.WorkflowFile, "runsOn", p.cfg.Label, "waiting", p.cfg.Timeout.String())
	jobReq, msgID, err := p.awaitDelivery(ctx, sessA)
	if err != nil {
		return "", err
	}
	p.log.Info("INVESTIGATION-H: RunnerJobRequest delivered to A",
		"messageId", msgID, "runnerRequestId", jobReq.RunnerRequestID,
		"runServiceHost", hostOf(jobReq.RunServiceURL))

	runID, jobID, err := p.resolveFixtureRun(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve fixture run (the REST watch has no subject without it): %w", err)
	}
	defer p.cancelFixtureRun(context.WithoutCancel(ctx), runID)

	acq, _, err := sessA.bc.AcquireJob(ctx, jobReq.RunServiceURL, broker.JobAcquisitionRequest{
		JobMessageID:   jobReq.RunnerRequestID,
		RunnerOS:       "Linux",
		BillingOwnerID: jobReq.BillingOwnerID,
	})
	if err != nil {
		return "", fmt.Errorf("AcquireJob: %w", err)
	}
	jobToken := acq.JobAuthToken()
	p.log.Info("INVESTIGATION-H: job acquired by A",
		"planId", acq.Plan.PlanID, "hasJobToken", jobToken != "")

	var t0 time.Time
	if p.cfg.SkipComplete {
		// The told-nothing arm: the winner walks away after the acquire, exactly
		// what the listener would do with no release call at all. T0 is the
		// walk-away; the window then measures whether the acquire lock's lapse
		// (~10 min, Q247) recycles and redelivers the job.
		t0 = time.Now()
		p.log.Info("INVESTIGATION-H: no completion sent (result=none); watching what the acquire " +
			"lock's lapse does with the job")
	} else {
		err = sessA.bc.CompleteJob(ctx, jobReq.RunServiceURL, broker.CompleteJobRequest{
			PlanID:    acq.Plan.PlanID,
			JobID:     jobReq.RunnerRequestID,
			Result:    p.cfg.Result,
			AuthToken: jobToken,
		})
		if err != nil {
			p.log.Error("INVESTIGATION-H: VERDICT "+verdictWireRejected+" — the run service refused "+
				"completejob result="+string(p.cfg.Result)+"; the primary question cannot be asked "+
				"with a refused call",
				"error", err)
			return verdictWireRejected, nil
		}
		t0 = time.Now()
		p.log.Info("INVESTIGATION-H: VERDICT "+verdictWireAccepted+" — completejob result="+
			string(p.cfg.Result)+" accepted (2xx); the wire serialization is live-confirmed",
			"planId", acq.Plan.PlanID, "jobId", jobReq.RunnerRequestID)
	}

	// The Q683 candidate remedy, in the order the listener's release path would
	// run it: the remedy call first, then the recycle below.
	if p.cfg.ForceCancel {
		p.forceCancelRun(ctx, runID, t0)
	}

	// The listener's post-job recycle: the consumed delivery's session goes away
	// and its single-use runner record with it, leaving B the only listener. The
	// DELETE's answer here is the Q418-mechanism datum: the acquire's in_progress
	// job pins the record, so a 422 is expected until the run concludes, and a
	// refused attempt is retried by the deferred cleanup after the window.
	p.closeSession(ctx, sessA)
	p.deregisterRunner(ctx, runnerA)

	verdict := p.observe(ctx, obs, jobReq.RunnerRequestID, runID, jobID, t0)
	if strings.HasPrefix(verdict, verdictConcluded+"-run-") {
		p.watchJobTail(ctx, jobID, t0)
		if p.cfg.RerunCheck {
			p.rerunCheck(ctx, obs, runID, jobID)
		}
	}
	return verdict, nil
}

// forceCancelRun issues the Q683 candidate remedy: a standalone REST
// force-cancel of the fixture run, with no prior plain cancel — the call shape
// the listener's release path would use, since the prior force-cancel
// measurements (2026-08-04) all followed a 202-accepted plain cancel and a
// plain cancel alone was measured sluggish against an orphaned acquire. The
// wire answer is logged here; the observation window times the conclusion.
func (p *abandonedProbe) forceCancelRun(ctx context.Context, runID int64, t0 time.Time) {
	u := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/force-cancel", p.apiBase, p.cfg.Owner, p.cfg.Repo, runID)
	code, body, err := p.postJSON(ctx, u)
	switch {
	case err != nil:
		p.log.Warn("INVESTIGATION-H: FORCECANCEL-ERROR — the force-cancel request failed",
			"runId", runID, "error", err)
	case code < 200 || code > 299:
		p.log.Warn("INVESTIGATION-H: FORCECANCEL-REFUSED — force-cancel answered non-2xx with no "+
			"prior plain cancel; the standalone call shape is not usable as a remedy",
			"runId", runID, "status", code, "body", githubapp.SanitizeBody(body, 256))
	default:
		p.log.Info("INVESTIGATION-H: FORCECANCEL-ACCEPTED — standalone force-cancel accepted; the "+
			"window now times the conclusion it drives",
			"runId", runID, "status", code, "afterT0", time.Since(t0).Round(time.Millisecond).String())
	}
}

// watchJobTail keeps polling the job record after a CONCLUDED-run verdict until
// it concludes or the bounded tail closes. Whether a conclusion reaches the JOB
// record is its own datum, not implied by the run's: every accepted completejob
// value concluded the run while orphaning the job in_progress indefinitely (the
// Q645/Q676 findings), and only the told-nothing 15-minute cancel concluded
// both.
func (p *abandonedProbe) watchJobTail(ctx context.Context, jobID int64, t0 time.Time) {
	deadline := time.Now().Add(p.jobTailWait)
	lastStatus := ""
	for {
		status, conclusion, err := p.fetchJobStatus(ctx, jobID)
		switch {
		case err != nil:
			p.log.Warn("INVESTIGATION-H: job tail: REST job status fetch failed", "jobId", jobID, "error", err)
		case status == "completed":
			p.log.Info("INVESTIGATION-H: JOB-CONCLUDED — the job record went terminal too; no "+
				"orphaned in_progress record",
				"jobId", jobID, "conclusion", conclusion,
				"afterT0", time.Since(t0).Round(time.Second).String())
			return
		default:
			lastStatus = status
		}
		if time.Now().After(deadline) {
			p.log.Warn("INVESTIGATION-H: JOB-ORPHANED — the run concluded but the job record was "+
				"still not terminal when the tail closed; the in_progress orphan persists",
				"jobId", jobID, "status", lastStatus, "tail", p.jobTailWait.String())
			return
		}
		select {
		case <-ctx.Done():
			p.log.Warn("INVESTIGATION-H: job tail interrupted")
			return
		case <-time.After(p.restPollInterval):
		}
	}
}

// ── Observation ──────────────────────────────────────────────────────────────

// observedDelivery is one RunnerJobRequest B's session received, timestamped so
// the verdict can separate the pre-acquire fan-out sibling from a post-T0
// re-dispatch.
type observedDelivery struct {
	At        time.Time
	MessageID int64
	RequestID string
}

// sessionObserver drains B's session on a goroutine, recording every delivery.
type sessionObserver struct {
	done chan struct{}

	mu         sync.Mutex
	deliveries []observedDelivery
}

func (o *sessionObserver) record(d observedDelivery) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.deliveries = append(o.deliveries, d)
}

// after returns the deliveries received strictly after t.
func (o *sessionObserver) after(t time.Time) []observedDelivery {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []observedDelivery
	for _, d := range o.deliveries {
		if d.At.After(t) {
			out = append(out, d)
		}
	}
	return out
}

// requestIDsBefore returns the request ids of deliveries received at or before
// t. A queued job can fan out a sibling delivery to the observer's session
// before the acquire (measured 2026-08-04: distinct RunnerRequestID, fresh
// broker MessageID on every unacked redelivery, ~1/s), and that sibling keeps
// redelivering after T0 — so a post-T0 delivery counts as a re-dispatch only
// when its request id was never seen before T0.
func (o *sessionObserver) requestIDsBefore(t time.Time) map[string]bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	ids := map[string]bool{}
	for _, d := range o.deliveries {
		if !d.At.After(t) && d.RequestID != "" {
			ids[d.RequestID] = true
		}
	}
	return ids
}

// startObserver long-polls sess until ctx is cancelled, recording every
// RunnerJobRequest. Poll errors are logged and retried after a short backoff:
// the observer dying silently would turn a re-dispatch into NO-SIGNAL.
func (p *abandonedProbe) startObserver(ctx context.Context, sess *brokerSession) *sessionObserver {
	obs := &sessionObserver{done: make(chan struct{})}
	go func() {
		defer close(obs.done)
		for {
			if ctx.Err() != nil {
				return
			}
			msg, err := sess.bc.GetMessage(ctx, sess.sessionID)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				p.log.Warn("INVESTIGATION-H: observer poll failed; retrying", "error", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
				continue
			}
			if msg == nil {
				continue
			}
			if msg.MessageType != "RunnerJobRequest" {
				p.log.Info("INVESTIGATION-H: observer received non-job message",
					"messageId", msg.MessageID, "messageType", msg.MessageType)
				continue
			}
			body, err := p.decodeJobRequest(sess, msg)
			d := observedDelivery{At: time.Now(), MessageID: msg.MessageID}
			if err != nil {
				p.log.Warn("INVESTIGATION-H: observer could not decode a RunnerJobRequest body; "+
					"recording the delivery without its request id", "messageId", msg.MessageID, "error", err)
			} else {
				d.RequestID = body.RunnerRequestID
			}
			obs.record(d)
			p.log.Info("INVESTIGATION-H: observer received RunnerJobRequest",
				"messageId", msg.MessageID, "runnerRequestId", d.RequestID)
		}
	}()
	return obs
}

// observe watches both channels until one is decisive or the window closes.
func (p *abandonedProbe) observe(ctx context.Context, obs *sessionObserver,
	acquiredRequestID string, runID, jobID int64, t0 time.Time) string {
	deadline := t0.Add(p.cfg.Window)
	p.log.Info("INVESTIGATION-H: observation window open",
		"window", p.cfg.Window.String(), "runId", runID, "jobId", jobID)

	siblingIDs := obs.requestIDsBefore(t0)
	siblingLogged := map[string]bool{}
	lastStatus, lastConclusion := "", ""
	lastRunStatus, lastRunConclusion := "", ""
	nextREST := time.Time{} // first REST check happens immediately
	for {
		var fresh []observedDelivery
		for _, d := range obs.after(t0) {
			if siblingIDs[d.RequestID] {
				if !siblingLogged[d.RequestID] {
					siblingLogged[d.RequestID] = true
					p.log.Info("INVESTIGATION-H: pre-T0 sibling delivery still redelivering (unacked "+
						"fan-out, not a re-dispatch); further redeliveries of it are not logged",
						"runnerRequestId", d.RequestID, "messageId", d.MessageID)
				}
				continue
			}
			fresh = append(fresh, d)
		}
		if len(fresh) > 0 {
			for _, d := range fresh {
				same := d.RequestID != "" && d.RequestID == acquiredRequestID
				p.log.Info("INVESTIGATION-H: post-T0 delivery",
					"messageId", d.MessageID, "runnerRequestId", d.RequestID,
					"sameRequestIdAsAcquired", same, "afterT0", d.At.Sub(t0).Round(time.Second).String())
			}
			p.log.Info("INVESTIGATION-H: VERDICT " + verdictRedispatched + " — a RunnerJobRequest " +
				"reached a live listener after the " + string(p.cfg.Result) + " completion. The job " +
				"survives it; the AGC needs no re-run arm, only capacity")
			return verdictRedispatched
		}

		now := time.Now()
		if now.After(deadline) {
			p.log.Warn("INVESTIGATION-H: VERDICT "+verdictNoSignal+" — no redelivery and no "+
				"conclusion within the window; the job is dangling as if nothing had been reported, "+
				"so completejob("+string(p.cfg.Result)+") released nothing",
				"window", p.cfg.Window.String(), "lastStatus", lastStatus, "lastConclusion", lastConclusion)
			return verdictNoSignal
		}
		if now.After(nextREST) {
			// Both levels, because the live run split them: the 2026-08-04 run
			// concluded the RUN success one second after the abandoned completion
			// while the JOB record stayed in_progress with a null conclusion past
			// the whole window (see the Q645 plan doc findings). A watch on either
			// level alone reads NO-SIGNAL or misses the orphaned job.
			rStatus, rConclusion, rErr := p.fetchRunStatus(ctx, runID)
			switch {
			case rErr != nil:
				p.log.Warn("INVESTIGATION-H: REST run status fetch failed", "runId", runID, "error", rErr)
			case rStatus != lastRunStatus || rConclusion != lastRunConclusion:
				p.log.Info("INVESTIGATION-H: REST run transition",
					"runId", runID, "status", rStatus, "conclusion", rConclusion,
					"afterT0", now.Sub(t0).Round(time.Second).String())
				lastRunStatus, lastRunConclusion = rStatus, rConclusion
			}
			status, conclusion, err := p.fetchJobStatus(ctx, jobID)
			switch {
			case err != nil:
				p.log.Warn("INVESTIGATION-H: REST job status fetch failed", "jobId", jobID, "error", err)
			case status != lastStatus || conclusion != lastConclusion:
				p.log.Info("INVESTIGATION-H: REST job transition",
					"jobId", jobID, "status", status, "conclusion", conclusion,
					"afterT0", now.Sub(t0).Round(time.Second).String())
				lastStatus, lastConclusion = status, conclusion
			}
			if rStatus == "completed" {
				p.log.Info("INVESTIGATION-H: VERDICT "+verdictConcluded+"-run-"+rConclusion+" — the run "+
					"went terminal with no redelivery: a "+string(p.cfg.Result)+" completion ends the run "+
					"rather than re-queueing the job, so a job that never executed reports this conclusion",
					"runId", runID, "runConclusion", rConclusion,
					"jobStatus", status, "jobConclusion", conclusion)
				return verdictConcluded + "-run-" + rConclusion
			}
			if status == "completed" {
				p.log.Info("INVESTIGATION-H: VERDICT "+verdictConcluded+"-job-"+conclusion+" — the job "+
					"went terminal with no redelivery: a "+string(p.cfg.Result)+" completion kills the "+
					"job, so a re-run arm is needed for it to ever execute",
					"jobId", jobID, "conclusion", conclusion)
				return verdictConcluded + "-job-" + conclusion
			}
			nextREST = now.Add(p.restPollInterval)
		}

		wait := p.restPollInterval / 5
		if wait < 200*time.Millisecond {
			wait = 200 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			p.log.Warn("INVESTIGATION-H: interrupted mid-window; no verdict")
			return ""
		case <-time.After(wait):
		}
	}
}

// ── Rerun check ──────────────────────────────────────────────────────────────

// rerunCheck measures whether the concluded run gives rerun-failed-jobs a
// target — the half of a red-conclusion remedy (Q676) the conclusion alone
// cannot prove, and one the orphaned in_progress job record from the
// 2026-08-04 run gives real grounds to doubt. Best-effort: every outcome is
// logged, none is fatal, and the deferred cleanup cancel drives whatever this
// re-queues terminal. B's session observer is still polling, so a re-queued
// job reaching a live listener is observed on the same channel a real AGC
// would see it on.
func (p *abandonedProbe) rerunCheck(ctx context.Context, obs *sessionObserver, runID, jobID int64) {
	status, conclusion, err := p.fetchJobStatus(ctx, jobID)
	if err != nil {
		p.log.Warn("INVESTIGATION-H: rerun check: job status fetch failed", "jobId", jobID, "error", err)
	} else {
		p.log.Info("INVESTIGATION-H: rerun check: job record before rerun-failed-jobs",
			"jobId", jobID, "status", status, "conclusion", conclusion)
	}

	t1 := time.Now()
	u := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/rerun-failed-jobs", p.apiBase, p.cfg.Owner, p.cfg.Repo, runID)
	code, body, err := p.postJSON(ctx, u)
	if err != nil {
		p.log.Warn("INVESTIGATION-H: rerun check: rerun-failed-jobs request failed", "runId", runID, "error", err)
		return
	}
	if code < 200 || code > 299 {
		p.log.Info("INVESTIGATION-H: RERUN-REFUSED — rerun-failed-jobs refused the concluded run, "+
			"so this conclusion does not arm a re-run",
			"runId", runID, "status", code, "body", githubapp.SanitizeBody(body, 256))
		return
	}
	p.log.Info("INVESTIGATION-H: rerun-failed-jobs accepted; watching for the re-queued job",
		"runId", runID, "status", code, "window", p.rerunWait.String())

	deadline := t1.Add(p.rerunWait)
	siblingIDs := obs.requestIDsBefore(t1)
	requeued := false
	for {
		var fresh []observedDelivery
		for _, d := range obs.after(t1) {
			if !siblingIDs[d.RequestID] {
				fresh = append(fresh, d)
			}
		}
		if len(fresh) > 0 {
			d := fresh[0]
			p.log.Info("INVESTIGATION-H: RERUN-REDELIVERED — the re-run's job reached the live "+
				"listener; a red conclusion plus rerun-failed-jobs closes the loop end to end",
				"messageId", d.MessageID, "runnerRequestId", d.RequestID,
				"afterRerun", d.At.Sub(t1).Round(time.Second).String())
			return
		}
		if time.Now().After(deadline) {
			if requeued {
				p.log.Warn("INVESTIGATION-H: RERUN-REQUEUED-NO-DELIVERY — the run left completed "+
					"but no delivery reached the live listener within the wait",
					"runId", runID, "wait", p.rerunWait.String())
			} else {
				p.log.Warn("INVESTIGATION-H: RERUN-NO-EFFECT — rerun-failed-jobs answered 2xx but "+
					"the run never left completed within the wait",
					"runId", runID, "wait", p.rerunWait.String())
			}
			return
		}
		rStatus, rConclusion, rErr := p.fetchRunStatus(ctx, runID)
		if rErr != nil {
			p.log.Warn("INVESTIGATION-H: rerun check: REST run status fetch failed", "runId", runID, "error", rErr)
		} else if rStatus != "completed" && !requeued {
			requeued = true
			p.log.Info("INVESTIGATION-H: rerun check: the run left completed",
				"runId", runID, "status", rStatus, "conclusion", rConclusion,
				"afterRerun", time.Since(t1).Round(time.Second).String())
		}
		select {
		case <-ctx.Done():
			p.log.Warn("INVESTIGATION-H: rerun check interrupted")
			return
		case <-time.After(p.restPollInterval):
		}
	}
}

// ── Broker session plumbing ──────────────────────────────────────────────────

// openSession exchanges the runner's JIT credentials for a broker OAuth token
// and opens a v2 session, deriving the AES message key the same way
// listener.createSession does.
func (p *abandonedProbe) openSession(ctx context.Context, r *jitRunner) (*brokerSession, error) {
	token, err := githubapp.FetchRunnerOAuthToken(ctx, &githubapp.RunnerCredentials{
		ClientID:         r.ClientID,
		AuthorizationURL: r.AuthorizationURL,
	}, r.Key, p.hc)
	if err != nil {
		return nil, fmt.Errorf("broker OAuth exchange for %s: %w", r.Name, err)
	}
	bc := &broker.Client{
		BrokerURL:     r.BrokerURL,
		Token:         token,
		UseV2Flow:     true,
		RunnerVersion: p.cfg.RunnerVersion,
		RunnerOS:      "linux",
		RunnerArch:    "x64",
		HTTPClient:    p.pollHC,
	}
	sess, err := bc.CreateSession(ctx, r.ID, r.Name, p.cfg.RunnerVersion)
	if err != nil {
		return nil, fmt.Errorf("CreateSession for %s: %w", r.Name, err)
	}
	bc.BrokerURL = sess.BrokerURL
	out := &brokerSession{bc: bc, sessionID: sess.SessionID}
	if len(sess.EncryptionKey) > 0 {
		if sess.EncryptionKeyEncrypted {
			aesKey, decErr := broker.DecryptSessionKey(sess.EncryptionKey, r.Key)
			if decErr != nil {
				p.log.Warn("INVESTIGATION-H: session key decrypt failed; message bodies will be "+
					"parsed as plaintext", "runner", r.Name, "error", decErr)
			} else {
				out.aesKey = aesKey
			}
		} else {
			out.aesKey = sess.EncryptionKey
		}
	}
	p.log.Info("INVESTIGATION-H: session created",
		"runner", r.Name, "sessionId", sess.SessionID, "hasAESKey", out.aesKey != nil)
	return out, nil
}

// closeSession deletes the session once; safe to call again from the deferred
// cleanup after the explicit post-completion recycle.
func (p *abandonedProbe) closeSession(ctx context.Context, sess *brokerSession) {
	if sess.closed {
		return
	}
	sess.closed = true
	dCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := sess.bc.DeleteSession(dCtx, sess.sessionID); err != nil {
		p.log.Warn("INVESTIGATION-H: DeleteSession failed; the broker session leaks until it "+
			"expires server-side", "sessionId", sess.sessionID, "error", err)
		return
	}
	p.log.Info("INVESTIGATION-H: session deleted", "sessionId", sess.sessionID)
}

// awaitDelivery polls A's session for the fixture RunnerJobRequest.
func (p *abandonedProbe) awaitDelivery(ctx context.Context, sess *brokerSession) (*broker.RunnerJobRequestBody, int64, error) {
	deadline, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	for {
		msg, err := sess.bc.GetMessage(deadline, sess.sessionID)
		if err != nil {
			if deadline.Err() != nil {
				return nil, 0, fmt.Errorf("no RunnerJobRequest within %s — was the fixture workflow dispatched?", p.cfg.Timeout)
			}
			return nil, 0, fmt.Errorf("poll for delivery: %w", err)
		}
		if msg == nil {
			if deadline.Err() != nil {
				return nil, 0, fmt.Errorf("no RunnerJobRequest within %s — was the fixture workflow dispatched?", p.cfg.Timeout)
			}
			continue
		}
		if msg.MessageType != "RunnerJobRequest" {
			p.log.Info("INVESTIGATION-H: A received non-job message",
				"messageId", msg.MessageID, "messageType", msg.MessageType)
			continue
		}
		body, err := p.decodeJobRequest(sess, msg)
		if err != nil {
			return nil, 0, fmt.Errorf("decode RunnerJobRequest %d: %w", msg.MessageID, err)
		}
		if body.RunServiceURL == "" {
			return nil, 0, fmt.Errorf("RunnerJobRequest %d carries no run_service_url", msg.MessageID)
		}
		return body, msg.MessageID, nil
	}
}

// decodeJobRequest decrypts (when the session has an AES key) and parses one
// RunnerJobRequest body.
func (p *abandonedProbe) decodeJobRequest(sess *brokerSession, msg *broker.TaskAgentMessage) (*broker.RunnerJobRequestBody, error) {
	raw := []byte(msg.Body)
	if sess.aesKey != nil {
		plain, err := broker.DecryptMessageBody(msg.Body, sess.aesKey)
		if err != nil {
			return nil, fmt.Errorf("decrypt body: %w", err)
		}
		raw = plain
	}
	var body broker.RunnerJobRequestBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("unmarshal body: %w", err)
	}
	return &body, nil
}

// ── JIT registration (mirrors agentpool.GithubRegistrar, repo-level) ─────────

// registerRunner registers a repo-level JIT runner with the probe label. A 409
// name conflict (a record left by an interrupted run) is resolved by deleting
// the survivor and retrying once.
func (p *abandonedProbe) registerRunner(ctx context.Context, name string) (*jitRunner, error) {
	r, status, err := p.tryRegisterRunner(ctx, name)
	if status == http.StatusConflict {
		p.log.Warn("INVESTIGATION-H: runner name already registered (an interrupted run's leftover); "+
			"deleting it and retrying", "name", name)
		if id, lookErr := p.lookupRunnerID(ctx, name); lookErr == nil && id != 0 {
			p.deregisterRunner(ctx, &jitRunner{ID: id, Name: name})
		}
		r, _, err = p.tryRegisterRunner(ctx, name)
	}
	if err != nil {
		return nil, err
	}
	p.log.Info("INVESTIGATION-H: JIT runner registered",
		"name", r.Name, "id", r.ID, "brokerHost", hostOf(r.BrokerURL))
	return r, nil
}

func (p *abandonedProbe) tryRegisterRunner(ctx context.Context, name string) (*jitRunner, int, error) {
	token, err := p.tokens.Token(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("installation token: %w", err)
	}
	reqBody, err := json.Marshal(map[string]any{
		"name":            name,
		"runner_group_id": 1,
		"labels":          []string{p.cfg.Label},
		"work_folder":     "_work",
	})
	if err != nil {
		return nil, 0, err
	}
	u := fmt.Sprintf("%s/repos/%s/%s/actions/runners/generate-jitconfig", p.apiBase, p.cfg.Owner, p.cfg.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(reqBody))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("generate-jitconfig: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("generate-jitconfig for %s: status %d: %s",
			name, resp.StatusCode, githubapp.SanitizeBody(respBody, 512))
	}
	var result struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("decode generate-jitconfig response: %w", err)
	}
	r, err := parseJITBlob(result.Runner.ID, name, result.EncodedJITConfig)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return r, resp.StatusCode, nil
}

// deregisterRunner deletes the runner record; the guard is set only once the
// record is actually gone (deleted, or 404 for a consumed single-use JIT record
// GitHub auto-removed), so a refused attempt — above all the 422 "currently
// running a job" an orphaned in_progress acquire pins the record with — is
// retried by the deferred cleanup after the window. That retry's answer is the
// post-conclusion deletability datum (Q683/Q418), and it stops an interrupted
// run leaking the record.
func (p *abandonedProbe) deregisterRunner(ctx context.Context, r *jitRunner) {
	if r.deregistered {
		return
	}
	token, err := p.tokens.Token(ctx)
	if err != nil {
		p.log.Warn("INVESTIGATION-H: installation token for deregister failed", "runner", r.Name, "error", err)
		return
	}
	u := fmt.Sprintf("%s/repos/%s/%s/actions/runners/%d", p.apiBase, p.cfg.Owner, p.cfg.Repo, r.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := p.hc.Do(req)
	if err != nil {
		p.log.Warn("INVESTIGATION-H: deregister runner failed", "runner", r.Name, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		r.deregistered = true
		p.log.Info("INVESTIGATION-H: runner deregistered", "runner", r.Name, "id", r.ID)
	case http.StatusNotFound:
		r.deregistered = true
		p.log.Info("INVESTIGATION-H: runner already gone (single-use record consumed)", "runner", r.Name, "id", r.ID)
	default:
		p.log.Warn("INVESTIGATION-H: deregister runner refused; the record stays and the deferred "+
			"cleanup retries after the window",
			"runner", r.Name, "status", resp.StatusCode, "body", githubapp.SanitizeBody(body, 256))
	}
}

// lookupRunnerID resolves a runner id by exact name, for the 409 recovery path.
func (p *abandonedProbe) lookupRunnerID(ctx context.Context, name string) (int64, error) {
	runners, err := p.listRunners(ctx)
	if err != nil {
		return 0, err
	}
	for _, r := range runners {
		if r.Name == name {
			return r.ID, nil
		}
	}
	return 0, nil
}

type restRunner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (p *abandonedProbe) listRunners(ctx context.Context) ([]restRunner, error) {
	var out struct {
		Runners []restRunner `json:"runners"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/actions/runners?per_page=100", p.apiBase, p.cfg.Owner, p.cfg.Repo)
	if err := p.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	return out.Runners, nil
}

// reportStaleRunners logs every pre-existing runner on the probe label: another
// consumer on the label is one of the plan doc's invalid-result conditions, so
// the record must show whether one existed.
func (p *abandonedProbe) reportStaleRunners(ctx context.Context) {
	runners, err := p.listRunners(ctx)
	if err != nil {
		p.log.Warn("INVESTIGATION-H: could not list existing runners", "error", err)
		return
	}
	for _, r := range runners {
		for _, l := range r.Labels {
			if l.Name == p.cfg.Label {
				p.log.Warn("INVESTIGATION-H: a runner already carries the probe label — if it is "+
					"live it can consume the redelivery and invalidate the verdict",
					"name", r.Name, "id", r.ID, "status", r.Status)
			}
		}
	}
}

// ── REST watch ───────────────────────────────────────────────────────────────

// resolveFixtureRun finds the newest queued run of the fixture workflow and its
// single job, which the window's REST watch polls.
func (p *abandonedProbe) resolveFixtureRun(ctx context.Context) (int64, int64, error) {
	var runs struct {
		WorkflowRuns []struct {
			ID        int64  `json:"id"`
			Status    string `json:"status"`
			CreatedAt string `json:"created_at"`
		} `json:"workflow_runs"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%s/runs?status=queued&per_page=5",
		p.apiBase, p.cfg.Owner, p.cfg.Repo, p.cfg.WorkflowFile)
	if err := p.getJSON(ctx, u, &runs); err != nil {
		return 0, 0, err
	}
	if len(runs.WorkflowRuns) == 0 {
		return 0, 0, fmt.Errorf("no queued run of %s found", p.cfg.WorkflowFile)
	}
	// The list endpoint returns newest first; take the head and record it so a
	// stale-run misattribution is visible in the record.
	run := runs.WorkflowRuns[0]
	p.log.Info("INVESTIGATION-H: watching fixture run",
		"runId", run.ID, "createdAt", run.CreatedAt, "status", run.Status,
		"queuedRunsOnWorkflow", len(runs.WorkflowRuns))

	var jobs struct {
		Jobs []struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"jobs"`
	}
	if err := p.getJSON(ctx, fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/jobs", p.apiBase, p.cfg.Owner, p.cfg.Repo, run.ID), &jobs); err != nil {
		return 0, 0, err
	}
	if len(jobs.Jobs) == 0 {
		return 0, 0, fmt.Errorf("run %d has no jobs", run.ID)
	}
	p.log.Info("INVESTIGATION-H: watching fixture job", "jobId", jobs.Jobs[0].ID, "status", jobs.Jobs[0].Status)
	return run.ID, jobs.Jobs[0].ID, nil
}

func (p *abandonedProbe) fetchRunStatus(ctx context.Context, runID int64) (string, string, error) {
	var run struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d", p.apiBase, p.cfg.Owner, p.cfg.Repo, runID)
	if err := p.getJSON(ctx, u, &run); err != nil {
		return "", "", err
	}
	return run.Status, run.Conclusion, nil
}

func (p *abandonedProbe) fetchJobStatus(ctx context.Context, jobID int64) (string, string, error) {
	var job struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/actions/jobs/%d", p.apiBase, p.cfg.Owner, p.cfg.Repo, jobID)
	if err := p.getJSON(ctx, u, &job); err != nil {
		return "", "", err
	}
	return job.Status, job.Conclusion, nil
}

// cancelFixtureRun drives the fixture run terminal at cleanup so the queue is
// left empty. Deferred until after the window: a cancel inside it would
// manufacture CONCLUDED-cancelled.
func (p *abandonedProbe) cancelFixtureRun(ctx context.Context, runID int64) {
	err := cancelWorkflowRun(ctx, cancelRunDeps{
		log: p.log, hc: p.hc, tokens: p.tokens, apiBase: p.apiBase, tag: "INVESTIGATION-H",
	}, p.cfg.Owner, p.cfg.Repo, runID)
	if err != nil {
		p.log.Warn("INVESTIGATION-H: cleanup cancel failed; the fixture run may still be queued",
			"runId", runID, "error", err)
		return
	}
	p.log.Info("INVESTIGATION-H: fixture run cancelled", "runId", runID)
}

// postJSON issues one authorized bodyless REST POST and returns the response
// status and body.
func (p *abandonedProbe) postJSON(ctx context.Context, url string) (int, []byte, error) {
	token, err := p.tokens.Token(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("installation token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, nil
}

// getJSON issues one authorized REST GET and decodes the response.
func (p *abandonedProbe) getJSON(ctx context.Context, url string, out any) error {
	token, err := p.tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("installation token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, githubapp.SanitizeBody(body, 256))
	}
	return json.Unmarshal(body, out)
}

// ── JIT blob decomposition ───────────────────────────────────────────────────

// parseJITBlob decodes the encoded_jit_config blob into the runner's broker
// credentials — the same decomposition agentpool.parseJITCredentials performs
// (that helper lives in cmd/agc's internal tree).
func parseJITBlob(agentID int64, name, encodedBlob string) (*jitRunner, error) {
	decoded, err := base64.StdEncoding.DecodeString(encodedBlob)
	if err != nil {
		return nil, fmt.Errorf("decode jit config blob: %w", err)
	}
	var files map[string]string
	if err := json.Unmarshal(decoded, &files); err != nil {
		return nil, fmt.Errorf("unmarshal jit config blob: %w", err)
	}
	decodeFile := func(key string) ([]byte, error) {
		b, err := base64.StdEncoding.DecodeString(files[key])
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		return b, nil
	}

	runnerFile, err := decodeFile(".runner")
	if err != nil {
		return nil, err
	}
	var runnerCfg struct {
		ServerURL   string `json:"serverUrl"`
		ServerURLV2 string `json:"serverUrlV2"`
	}
	if err := json.Unmarshal(runnerFile, &runnerCfg); err != nil {
		return nil, fmt.Errorf("parse .runner config: %w", err)
	}
	brokerURL := runnerCfg.ServerURLV2
	if brokerURL == "" {
		brokerURL = runnerCfg.ServerURL
	}

	credFile, err := decodeFile(".credentials")
	if err != nil {
		return nil, err
	}
	var credCfg struct {
		Data struct {
			ClientID         string `json:"clientId"`
			AuthorizationURL string `json:"authorizationUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(credFile, &credCfg); err != nil {
		return nil, fmt.Errorf("parse .credentials config: %w", err)
	}

	rsaFile, err := decodeFile(".credentials_rsaparams")
	if err != nil {
		return nil, err
	}
	key, err := parseJITRSAParams(rsaFile)
	if err != nil {
		return nil, fmt.Errorf("parse RSA params: %w", err)
	}

	return &jitRunner{
		ID:               agentID,
		Name:             name,
		ClientID:         credCfg.Data.ClientID,
		AuthorizationURL: credCfg.Data.AuthorizationURL,
		BrokerURL:        brokerURL,
		Key:              key,
	}, nil
}

// parseJITRSAParams reconstructs the RSA private key from the JIT blob's
// .credentials_rsaparams JSON (lowercase keys, unlike the PascalCase config.sh
// files githubapp.ParseRunnerRSAKey reads).
func parseJITRSAParams(data []byte) (*rsa.PrivateKey, error) {
	var p struct {
		Modulus  string `json:"modulus"`
		Exponent string `json:"exponent"`
		D        string `json:"d"`
		P        string `json:"p"`
		Q        string `json:"q"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("unmarshal RSA JSON: %w", err)
	}
	decodeParam := func(name, b64 string) (*big.Int, error) {
		b, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			b, err = base64.RawURLEncoding.DecodeString(b64)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		}
		return new(big.Int).SetBytes(b), nil
	}
	n, err := decodeParam("modulus", p.Modulus)
	if err != nil {
		return nil, err
	}
	e, err := decodeParam("exponent", p.Exponent)
	if err != nil {
		return nil, err
	}
	d, err := decodeParam("d", p.D)
	if err != nil {
		return nil, err
	}
	pp, err := decodeParam("p", p.P)
	if err != nil {
		return nil, err
	}
	q, err := decodeParam("q", p.Q)
	if err != nil {
		return nil, err
	}
	key := &rsa.PrivateKey{
		PublicKey: rsa.PublicKey{N: n, E: int(e.Int64())},
		D:         d,
		Primes:    []*big.Int{pp, q},
	}
	key.Precompute()
	if err := key.Validate(); err != nil {
		return nil, fmt.Errorf("invalid RSA key: %w", err)
	}
	return key, nil
}

// ── Wire logging ─────────────────────────────────────────────────────────────

// wireTransport logs every response status on its way past, so the record
// shows what GitHub answered (the completejob 2xx above all) rather than only
// what the client made of it. Paths only — no query strings, no headers.
type wireTransport struct {
	rt  http.RoundTripper
	log *slog.Logger
}

func (w wireTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := w.rt.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	w.log.Debug("INVESTIGATION-H: wire",
		"method", req.Method, "host", req.URL.Host, "path", req.URL.Path,
		"status", resp.StatusCode, "elapsed", time.Since(start).Round(time.Millisecond).String())
	return resp, err
}

// wireLoggedClient returns a copy of c whose transport logs response statuses.
func wireLoggedClient(c *http.Client, log *slog.Logger) *http.Client {
	rt := c.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	c2 := *c
	c2.Transport = wireTransport{rt: rt, log: log}
	return &c2
}

// hostOf returns the host of a URL for logging, or the raw string when it does
// not parse as one.
func hostOf(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	return raw
}
