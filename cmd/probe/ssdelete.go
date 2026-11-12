// Investigation J (Q1344): what does deleting a scale set at GitHub do to the jobs
// queued on it and the runner records minted under it?
//
// Set PROBE_SSDELETE_TEST=true to run this scenario instead of the classic broker
// probe. A renamed or deleted RunnerSet leaves its scale set registered, and the next
// tenant admitted onto that name adopts it by exact name (Q1344). Deleting the scale
// set when its listener stops would release the name at GitHub as well as in the
// cluster, but whether that is safe turns on three things nothing here has measured:
//
//  1. Does the Actions Service accept the delete while a job is queued or
//     assigned on the scale set, and while a runner record minted under it exists?
//  2. What happens to that job: concluded (and how), left queued, or requeued?
//  3. Does the runner record go with the scale set, or stay behind?
//
// and a fourth, which decides whether a delete closes the adoption at all:
//
//  4. Does a scale set re-created under the same name receive the deleted one's
//     still-queued job? A job targets a label, not a scale-set id, so if it does,
//     deleting releases nothing a successor cannot take.
//
// One arm per run, each with its own throwaway scale set and its own fixture job:
//
//	PROBE_SSDELETE_ARM=queued   — the scale set has no session, so the job waits on
//	                              it unassigned: the state a renamed set's stopped
//	                              listener leaves.
//	PROBE_SSDELETE_ARM=assigned — a session takes the assignment and mints a JIT
//	                              runner record for it, then the session is deleted:
//	                              a listener stopped between provisioning and the
//	                              runner coming online.
//
// Then, for both:
//
//	T0  DELETE the scale set                                   [measurement 1]
//	    poll the job and the runner record over the window     [measurements 2, 3]
//	    if the job is still not concluded, re-create a scale
//	    set under the same name and poll for the job           [measurement 4]
//	cleanup: cancel the fixture run, deregister the runner record, delete every
//	         scale set this run created
//
// Dispatch the fixture (q1344-ssdelete-probe.yml) BEFORE starting the probe, so the
// job is queued against the label when the scale set appears.
//
// Required environment variables:
//
//	GITHUB_APP_ID              - GitHub App numeric ID
//	GITHUB_APP_PRIVATE_KEY     - Path to PEM file, or PEM literal
//	GITHUB_APP_INSTALLATION_ID - Installation ID for the target repo
//	GITHUB_ORG_URL             - Repository URL (https://github.com/{owner}/{repo});
//	                             repo-level, because the org Default runner group
//	                             refuses public repositories (see testing.md)
//
// Optional:
//
//	PROBE_SSDELETE_ARM        - queued (default) or assigned.
//	PROBE_SSDELETE_NAME       - Scale set name (default gag-q1344-ssdelete); the
//	                            fixture's runs-on must match it.
//	PROBE_SSDELETE_GROUP_NAME - Runner group (default Default).
//	PROBE_SSDELETE_WORKFLOW   - Fixture workflow file (default q1344-ssdelete-probe.yml).
//	PROBE_SSDELETE_TIMEOUT    - Wait for the fixture job, and for measurement 4's
//	                            delivery (default 5m).
//	PROBE_SSDELETE_WINDOW     - Post-delete observation window (default 20m).
//	PROBE_SSDELETE_INTERVAL   - Read cadence inside the window (default 30s).
//	PROBE_SSDELETE_RECREATE   - "false" skips measurement 4 (default on).
//
// The App needs administration: write (scale set and runner registration) and
// actions: write (the cleanup run-cancel). The Q1344 plan doc carries the design and
// what would make a result invalid.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/actions-gateway/github-actions-gateway/githubapp"
	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
)

// The two arms.
const (
	ssDeleteArmQueued   = "queued"
	ssDeleteArmAssigned = "assigned"
)

// Session owner names. The re-created scale set's listener is a different owner, as a
// second tenant's AGC would be.
const (
	ssDeleteOwner          = "gag-q1344-first"
	ssDeleteSuccessorOwner = "gag-q1344-successor"
)

// Verdicts, one family per measurement.
const (
	verdictSSDeleteAccepted = "DELETE-ACCEPTED"
	verdictSSDeleteRefused  = "DELETE-REFUSED"
	verdictSSDeleteNoEffect = "DELETE-NOT-EFFECTIVE"
	verdictJobConcluded     = "JOB-CONCLUDED"
	verdictJobNotConcluded  = "JOB-NOT-CONCLUDED"
	verdictRunnerRemoved    = "RUNNER-REMOVED"
	verdictRunnerLeft       = "RUNNER-LEFT"
	verdictJobInherited     = "JOB-INHERITED"
	verdictJobNotInherited  = "JOB-NOT-INHERITED"
)

// ssDeleteConfig holds the parsed environment for the scale-set delete scenario.
type ssDeleteConfig struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPEM  []byte
	RepoURL        string
	Owner          string
	Repo           string

	Arm          string
	ScaleSetName string
	GroupName    string
	WorkflowFile string
	Timeout      time.Duration
	Window       time.Duration
	Interval     time.Duration
	Recreate     bool
}

// parseSSDeleteConfig reads and validates the scenario environment from the injected
// getenv function (normally os.Getenv).
func parseSSDeleteConfig(getenv func(string) string) (ssDeleteConfig, error) {
	var cfg ssDeleteConfig

	appIDStr, err := mustEnv(getenv, "GITHUB_APP_ID")
	if err != nil {
		return ssDeleteConfig{}, err
	}
	if _, err := fmt.Sscan(appIDStr, &cfg.AppID); err != nil {
		return ssDeleteConfig{}, fmt.Errorf("parse GITHUB_APP_ID: %w", err)
	}
	installIDStr, err := mustEnv(getenv, "GITHUB_APP_INSTALLATION_ID")
	if err != nil {
		return ssDeleteConfig{}, err
	}
	if _, err := fmt.Sscan(installIDStr, &cfg.InstallationID); err != nil {
		return ssDeleteConfig{}, fmt.Errorf("parse GITHUB_APP_INSTALLATION_ID: %w", err)
	}
	pemValue, err := mustEnv(getenv, "GITHUB_APP_PRIVATE_KEY")
	if err != nil {
		return ssDeleteConfig{}, err
	}
	if cfg.PrivateKeyPEM, err = loadPEM(pemValue); err != nil {
		return ssDeleteConfig{}, fmt.Errorf("load GITHUB_APP_PRIVATE_KEY: %w", err)
	}
	if cfg.RepoURL, err = mustEnv(getenv, "GITHUB_ORG_URL"); err != nil {
		return ssDeleteConfig{}, err
	}
	if cfg.Owner, cfg.Repo, err = parseOwnerRepo(cfg.RepoURL); err != nil {
		return ssDeleteConfig{}, err
	}

	cfg.Arm = getenv("PROBE_SSDELETE_ARM")
	if cfg.Arm == "" {
		cfg.Arm = ssDeleteArmQueued
	}
	if cfg.Arm != ssDeleteArmQueued && cfg.Arm != ssDeleteArmAssigned {
		return ssDeleteConfig{}, fmt.Errorf("PROBE_SSDELETE_ARM %q: want %q or %q",
			cfg.Arm, ssDeleteArmQueued, ssDeleteArmAssigned)
	}
	cfg.ScaleSetName = getenv("PROBE_SSDELETE_NAME")
	if cfg.ScaleSetName == "" {
		cfg.ScaleSetName = "gag-q1344-ssdelete"
	}
	cfg.GroupName = getenv("PROBE_SSDELETE_GROUP_NAME")
	if cfg.GroupName == "" {
		cfg.GroupName = "Default"
	}
	cfg.WorkflowFile = getenv("PROBE_SSDELETE_WORKFLOW")
	if cfg.WorkflowFile == "" {
		cfg.WorkflowFile = "q1344-ssdelete-probe.yml"
	}
	if cfg.Timeout, err = parseDurationEnv(getenv, "PROBE_SSDELETE_TIMEOUT", 5*time.Minute); err != nil {
		return ssDeleteConfig{}, err
	}
	if cfg.Window, err = parseDurationEnv(getenv, "PROBE_SSDELETE_WINDOW", 20*time.Minute); err != nil {
		return ssDeleteConfig{}, err
	}
	if cfg.Interval, err = parseDurationEnv(getenv, "PROBE_SSDELETE_INTERVAL", 30*time.Second); err != nil {
		return ssDeleteConfig{}, err
	}
	cfg.Recreate = getenv("PROBE_SSDELETE_RECREATE") != "false"
	return cfg, nil
}

// ssDeleteProbe carries the scenario's dependencies. It drives the shipping
// scaleset.Client, so the delete it measures is the call an AGC would make.
type ssDeleteProbe struct {
	log *slog.Logger
	cfg ssDeleteConfig

	client  *scaleset.Client
	hc      *http.Client
	tokens  githubapp.TokenProvider
	apiBase string

	// created is every scale set this run registered and has not yet seen deleted,
	// which is what cleanup removes.
	created map[int]bool
	// afterDelete runs once T0's delete has been accepted. Tests use it to stage what
	// the backend would do next; a live run leaves it nil.
	afterDelete func()
}

// ssDeleteJob is the fixture job under measurement, by its REST identities.
type ssDeleteJob struct {
	RunID int64
	JobID int64
	// RunnerName is the JIT runner record the assigned arm minted, or "" on the
	// queued arm.
	RunnerName string
}

// newSSDeleteProbe builds the scenario around a scaleset.Client wired to the probe's
// wire logger. Arguments as newScalesetProbe.
func newSSDeleteProbe(logger *slog.Logger, cfg ssDeleteConfig, provider githubapp.TokenProvider,
	apiBase string, hc, pollClient *http.Client) (*ssDeleteProbe, error) {
	client, err := scaleset.New(scaleset.Config{
		TokenProvider: provider,
		ConfigURL:     cfg.RepoURL,
		APIBase:       apiBase,
		HTTPClient:    hc,
		PollClient:    pollClient,
		Observer:      wireLog{log: logger, tag: "J"},
	})
	if err != nil {
		return nil, err
	}
	if hc == nil {
		hc = httpx.NewClient()
	}
	return &ssDeleteProbe{
		log: logger, cfg: cfg, client: client, hc: hc, tokens: provider,
		apiBase: strings.TrimSuffix(apiBase, "/"), created: map[int]bool{},
	}, nil
}

// runSSDeleteProbe is the Investigation J entry point wired from run().
func runSSDeleteProbe(ctx context.Context, logger *slog.Logger, cfg ssDeleteConfig,
	provider githubapp.TokenProvider, apiBase string) error {
	p, err := newSSDeleteProbe(logger, cfg, provider, apiBase, nil, nil)
	if err != nil {
		return err
	}
	return p.run(ctx)
}

func (p *ssDeleteProbe) run(ctx context.Context) error {
	if err := p.client.Connect(ctx); err != nil {
		return err
	}
	p.log.Info("INVESTIGATION-J: admin connection established", "arm", p.cfg.Arm)

	ss, err := p.createScaleSet(ctx)
	if err != nil {
		return err
	}
	job := &ssDeleteJob{}
	defer p.cleanup(context.WithoutCancel(ctx), job)

	if err := p.stage(ctx, ss.ID, job); err != nil {
		return err
	}
	beforeStatus, beforeConclusion, err := p.fetchJobStatus(ctx, job.JobID)
	if err != nil {
		return fmt.Errorf("read job %d before the delete: %w", job.JobID, err)
	}
	p.log.Info("INVESTIGATION-J: STAGED — deleting the scale set now (T0)",
		"scaleSetId", ss.ID, "runId", job.RunID, "jobId", job.JobID,
		"jobStatus", beforeStatus, "jobConclusion", beforeConclusion, "runner", job.RunnerName)

	if !p.measureDelete(ctx, ss.ID) {
		return nil
	}
	if p.afterDelete != nil {
		p.afterDelete()
	}

	concluded, err := p.observe(ctx, job)
	if err != nil {
		return err
	}
	if concluded || !p.cfg.Recreate {
		return nil
	}
	return p.measureInheritance(ctx, job)
}

// ── Stage ────────────────────────────────────────────────────────────────────

// stage puts the fixture job into the arm's state on scale set id and records its
// identities in job.
func (p *ssDeleteProbe) stage(ctx context.Context, scaleSetID int, job *ssDeleteJob) error {
	p.log.Info("INVESTIGATION-J: waiting for the fixture job — dispatch "+p.cfg.WorkflowFile+
		" now if it is not already queued", "label", p.cfg.ScaleSetName, "waiting", p.cfg.Timeout.String())
	if p.cfg.Arm == ssDeleteArmQueued {
		runID, jobID, err := p.awaitQueuedJob(ctx)
		if err != nil {
			return err
		}
		job.RunID, job.JobID = runID, jobID
		return nil
	}

	sess, err := p.client.CreateSession(ctx, scaleSetID, ssDeleteOwner)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	assigned, err := p.awaitAssignment(ctx, scaleSetID, sess, 0)
	if err != nil {
		p.deleteSession(context.WithoutCancel(ctx), scaleSetID, sess.SessionID)
		return err
	}
	job.RunID = assigned.WorkflowRunID
	if job.JobID, err = p.resolveJobID(ctx, job.RunID); err != nil {
		p.deleteSession(context.WithoutCancel(ctx), scaleSetID, sess.SessionID)
		return err
	}

	// The record generatejitconfig pre-registers is what a reaped worker leaves
	// behind (Q550), so it is the record measurement 3 watches.
	name := p.cfg.ScaleSetName + "-" + shortID(assigned.JobID)
	if _, err := p.client.GenerateJITConfig(ctx, scaleSetID, name, ""); err != nil {
		p.deleteSession(context.WithoutCancel(ctx), scaleSetID, sess.SessionID)
		return fmt.Errorf("mint JIT runner %q: %w", name, err)
	}
	job.RunnerName = name
	present, err := p.runnerPresent(ctx, name)
	switch {
	case err != nil:
		p.log.Warn("INVESTIGATION-J: could not read the runner record back; measurement 3 has "+
			"no baseline", "runner", name, "error", err)
	case !present:
		p.log.Warn("INVESTIGATION-J: the minted runner record is not listed before the delete, "+
			"so measurement 3 cannot be read: a later absence is not the delete's doing", "runner", name)
	default:
		p.log.Info("INVESTIGATION-J: runner record registered", "runner", name)
	}

	// A renamed set's listener stops and deletes its session before anything else
	// happens to the scale set, so T0 finds no session.
	if err := p.client.DeleteSession(ctx, scaleSetID, sess.SessionID); err != nil {
		return fmt.Errorf("delete session before T0 (a live session would make the delete "+
			"measure something no stopped listener leaves): %w", err)
	}
	return nil
}

// awaitQueuedJob finds the newest queued run of the fixture workflow and waits until
// its job reads queued.
func (p *ssDeleteProbe) awaitQueuedJob(ctx context.Context) (int64, int64, error) {
	deadline, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	for {
		runID, jobID, status, err := p.newestQueuedJob(deadline)
		switch {
		case err == nil && status == "queued":
			p.log.Info("INVESTIGATION-J: fixture job queued on the scale set's label",
				"runId", runID, "jobId", jobID)
			return runID, jobID, nil
		case err != nil:
			p.log.Info("INVESTIGATION-J: no queued fixture job yet", "detail", err.Error())
		default:
			p.log.Info("INVESTIGATION-J: fixture job not queued yet", "jobId", jobID, "status", status)
		}
		if !sleepCtx(deadline, p.cfg.Interval) {
			return 0, 0, fmt.Errorf("no queued job of %s within %s — was it dispatched?",
				p.cfg.WorkflowFile, p.cfg.Timeout)
		}
	}
}

// awaitAssignment polls sess until a job is assigned, acquiring offered jobs on the
// GHES flow. A non-zero wantRun ignores assignments for any other run. It returns nil,
// nil when the timeout passes with nothing assigned.
func (p *ssDeleteProbe) awaitAssignment(ctx context.Context, scaleSetID int,
	sess *scaleset.RunnerScaleSetSession, wantRun int64) (*scaleset.JobMessage, error) {
	deadline, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	cursor := int64(0)
	for {
		msg, err := p.client.GetMessage(deadline, sess, 1, cursor)
		if err != nil {
			if deadline.Err() != nil {
				if wantRun != 0 {
					return nil, nil
				}
				return nil, fmt.Errorf("no job assigned within %s — was %s dispatched?",
					p.cfg.Timeout, p.cfg.WorkflowFile)
			}
			return nil, fmt.Errorf("queue poll: %w", err)
		}
		if msg == nil {
			continue
		}
		cursor = msg.MessageID
		jobs, jErr := msg.Jobs()
		if jErr != nil {
			p.log.Warn("INVESTIGATION-J: message decode failed", "messageId", msg.MessageID, "error", jErr)
		}
		p.log.Info("INVESTIGATION-J: observed message", "messageId", msg.MessageID,
			"entries", len(jobs), "body", githubapp.SanitizeBody([]byte(msg.Body), 512))
		for _, a := range scaleset.AssignedJobs(jobs) {
			if wantRun == 0 || a.WorkflowRunID == wantRun {
				return &a, nil
			}
			p.log.Info("INVESTIGATION-J: assignment for another run ignored",
				"runId", a.WorkflowRunID, "want", wantRun)
		}
		if ids := scaleset.AvailableJobIDs(jobs); len(ids) > 0 {
			won, aErr := p.client.AcquireJobs(deadline, scaleSetID, sess, ids)
			if aErr != nil {
				p.log.Warn("INVESTIGATION-J: acquirejobs failed", "requested", ids, "error", aErr)
				continue
			}
			p.log.Info("INVESTIGATION-J: acquired offered jobs", "requested", ids, "won", won)
		}
	}
}

// ── Measurement 1: the delete ────────────────────────────────────────────────

// measureDelete deletes the scale set and reads it back by name. It returns true only
// when the delete took effect, because the later measurements are about a deleted
// scale set.
func (p *ssDeleteProbe) measureDelete(ctx context.Context, scaleSetID int) bool {
	if err := p.client.DeleteRunnerScaleSet(ctx, scaleSetID); err != nil {
		p.log.Error("INVESTIGATION-J: VERDICT 1 "+verdictSSDeleteRefused+" — the service will not "+
			"delete a scale set in this state, so an AGC deleting on stop would have to drain first "+
			"or retry; measurements 2-4 have no subject", "scaleSetId", scaleSetID, "arm", p.cfg.Arm, "error", err)
		return false
	}
	still, err := p.client.GetRunnerScaleSetByName(ctx, p.cfg.ScaleSetName)
	if err != nil {
		p.log.Error("INVESTIGATION-J: VERDICT 1 unreadable — the delete answered success but the "+
			"read-back failed", "scaleSetId", scaleSetID, "error", err)
		return false
	}
	if still != nil && still.ID == scaleSetID {
		p.log.Error("INVESTIGATION-J: VERDICT 1 "+verdictSSDeleteNoEffect+" — the delete answered "+
			"success and the scale set is still registered", "scaleSetId", scaleSetID)
		return false
	}
	delete(p.created, scaleSetID)
	p.log.Info("INVESTIGATION-J: VERDICT 1 "+verdictSSDeleteAccepted+" — the scale set is gone by name",
		"scaleSetId", scaleSetID, "arm", p.cfg.Arm)
	return true
}

// ── Measurements 2 and 3: the job and the runner record ──────────────────────

// observe reads the job, and the runner record on the assigned arm, every interval
// for the window. It stops early once everything it watches has settled, and reports
// whether the job concluded.
func (p *ssDeleteProbe) observe(ctx context.Context, job *ssDeleteJob) (bool, error) {
	deadline, cancel := context.WithTimeout(ctx, p.cfg.Window)
	defer cancel()
	t0 := time.Now()

	var status, conclusion string
	var history []string
	runnerGone := false
	for {
		s, c, err := p.fetchJobStatus(deadline, job.JobID)
		if err != nil && deadline.Err() == nil {
			p.log.Warn("INVESTIGATION-J: job read failed; retrying", "jobId", job.JobID, "error", err)
		} else if err == nil {
			status, conclusion = s, c
			if obs := s + "/" + c; len(history) == 0 || history[len(history)-1] != obs {
				history = append(history, obs)
				p.log.Info("INVESTIGATION-J: job state", "jobId", job.JobID, "status", s,
					"conclusion", c, "sinceT0", time.Since(t0).Round(time.Second).String())
			}
		}
		if job.RunnerName != "" && !runnerGone {
			present, rErr := p.runnerPresent(deadline, job.RunnerName)
			if rErr == nil && !present {
				runnerGone = true
				p.log.Info("INVESTIGATION-J: runner record no longer listed", "runner", job.RunnerName,
					"sinceT0", time.Since(t0).Round(time.Second).String())
			}
		}
		settled := status == "completed" && (job.RunnerName == "" || runnerGone)
		if settled || !sleepCtx(deadline, p.cfg.Interval) {
			break
		}
	}

	concluded := status == "completed"
	p.log.Info("INVESTIGATION-J: VERDICT 2 "+jobVerdict(status, conclusion),
		"jobId", job.JobID, "status", status, "conclusion", conclusion,
		"history", strings.Join(history, " → "), "window", p.cfg.Window.String())
	if job.RunnerName != "" {
		p.log.Info("INVESTIGATION-J: VERDICT 3 "+runnerVerdict(runnerGone),
			"runner", job.RunnerName, "window", p.cfg.Window.String())
	}
	return concluded, nil
}

// jobVerdict names measurement 2's outcome from the job's last observed state.
func jobVerdict(status, conclusion string) string {
	if status == "completed" {
		return verdictJobConcluded + " (" + conclusion + ")"
	}
	if status == "" {
		return verdictJobNotConcluded + " (never read)"
	}
	return verdictJobNotConcluded + " (" + status + ")"
}

// runnerVerdict names measurement 3's outcome.
func runnerVerdict(gone bool) string {
	if gone {
		return verdictRunnerRemoved
	}
	return verdictRunnerLeft
}

// ── Measurement 4: a successor under the same name ───────────────────────────

// measureInheritance re-creates a scale set under the deleted one's name, as the next
// tenant's AGC would, and reports whether the still-queued fixture job is delivered
// to it.
func (p *ssDeleteProbe) measureInheritance(ctx context.Context, job *ssDeleteJob) error {
	ss, err := p.createScaleSet(ctx)
	if err != nil {
		return fmt.Errorf("re-create scale set %q: %w", p.cfg.ScaleSetName, err)
	}
	sess, err := p.client.CreateSession(ctx, ss.ID, ssDeleteSuccessorOwner)
	if err != nil {
		return fmt.Errorf("create successor session: %w", err)
	}
	defer p.deleteSession(context.WithoutCancel(ctx), ss.ID, sess.SessionID)

	got, err := p.awaitAssignment(ctx, ss.ID, sess, job.RunID)
	if err != nil {
		return err
	}
	if got != nil {
		p.log.Warn("INVESTIGATION-J: VERDICT 4 "+verdictJobInherited+" — a scale set re-created "+
			"under the deleted one's name received its queued job, so a delete releases nothing a "+
			"same-name successor cannot take: the jobs have to drain first",
			"scaleSetId", ss.ID, "runId", job.RunID, "jobId", got.JobID)
		return nil
	}
	p.log.Info("INVESTIGATION-J: VERDICT 4 "+verdictJobNotInherited+" — nothing for the deleted "+
		"scale set's run reached the successor within the timeout",
		"scaleSetId", ss.ID, "runId", job.RunID, "timeout", p.cfg.Timeout.String())
	return nil
}

// ── Cleanup ──────────────────────────────────────────────────────────────────

// cleanup leaves the repo as the run found it: no queued fixture run, no runner
// record, no scale set. Each step is best-effort and reported, because a leftover
// here is what the next run would adopt.
func (p *ssDeleteProbe) cleanup(ctx context.Context, job *ssDeleteJob) {
	if job.RunID != 0 {
		if err := cancelWorkflowRun(ctx, cancelRunDeps{
			log: p.log, hc: p.hc, tokens: p.tokens, apiBase: p.apiBase, tag: "INVESTIGATION-J",
		}, p.cfg.Owner, p.cfg.Repo, job.RunID); err != nil {
			p.log.Warn("INVESTIGATION-J: cleanup cancel failed; the fixture run may still be queued",
				"runId", job.RunID, "error", err)
		}
	}
	if job.RunnerName != "" {
		if _, err := p.client.DeregisterRunnerByName(ctx, job.RunnerName); err != nil {
			p.log.Warn("INVESTIGATION-J: cleanup deregister failed; delete the runner record by hand",
				"runner", job.RunnerName, "error", err)
		}
	}
	for id := range p.created {
		if err := p.client.DeleteRunnerScaleSet(ctx, id); err != nil {
			p.log.Error("INVESTIGATION-J: cleanup delete failed — the scale set is still registered; "+
				"prune it with the probe's cleanup mode", "scaleSetId", id, "error", err)
			continue
		}
		p.log.Info("INVESTIGATION-J: scale set deleted", "scaleSetId", id)
	}
}

// ── Shared helpers ───────────────────────────────────────────────────────────

// createScaleSet registers a fresh scale set under the configured name. An existing
// one is refused rather than reused: it would carry a previous run's queue and
// runner records, which is the very state under measurement.
func (p *ssDeleteProbe) createScaleSet(ctx context.Context) (*scaleset.RunnerScaleSet, error) {
	existing, err := p.client.GetRunnerScaleSetByName(ctx, p.cfg.ScaleSetName)
	if err != nil {
		return nil, fmt.Errorf("lookup scale set %q: %w", p.cfg.ScaleSetName, err)
	}
	if existing != nil {
		return nil, fmt.Errorf("a scale set named %q is already registered (id %d); prune it with "+
			"the probe's cleanup mode and re-run, since a reused one carries another run's state",
			p.cfg.ScaleSetName, existing.ID)
	}
	groupID := 1
	if id, ok, gErr := p.client.ResolveRunnerGroup(ctx, p.cfg.GroupName); gErr != nil {
		p.log.Warn("INVESTIGATION-J: runnergroups lookup failed; falling back to group id 1", "error", gErr)
	} else if ok {
		groupID = id
	}
	ss, err := p.client.CreateRunnerScaleSet(ctx, scaleset.RunnerScaleSet{
		Name:          p.cfg.ScaleSetName,
		RunnerGroupID: groupID,
		Labels:        []scaleset.Label{{Name: p.cfg.ScaleSetName, Type: "System"}},
		RunnerSetting: &scaleset.RunnerSetting{Ephemeral: true},
	})
	if err != nil {
		return nil, fmt.Errorf("create scale set: %w", err)
	}
	p.created[ss.ID] = true
	p.log.Info("INVESTIGATION-J: scale set created", "id", ss.ID, "name", ss.Name,
		"runnerGroupId", ss.RunnerGroupID)
	return ss, nil
}

// newestQueuedJob returns the newest queued run of the fixture workflow, its first
// job, and that job's status.
func (p *ssDeleteProbe) newestQueuedJob(ctx context.Context) (int64, int64, string, error) {
	var runs struct {
		WorkflowRuns []struct {
			ID int64 `json:"id"`
		} `json:"workflow_runs"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%s/runs?status=queued&per_page=1",
		p.apiBase, p.cfg.Owner, p.cfg.Repo, p.cfg.WorkflowFile)
	if err := restGetJSON(ctx, p.hc, p.tokens, u, &runs); err != nil {
		return 0, 0, "", err
	}
	if len(runs.WorkflowRuns) == 0 {
		return 0, 0, "", fmt.Errorf("no queued run of %s", p.cfg.WorkflowFile)
	}
	runID := runs.WorkflowRuns[0].ID
	jobID, err := p.resolveJobID(ctx, runID)
	if err != nil {
		return 0, 0, "", err
	}
	status, _, err := p.fetchJobStatus(ctx, jobID)
	return runID, jobID, status, err
}

// resolveJobID returns the REST id of runID's single job.
func (p *ssDeleteProbe) resolveJobID(ctx context.Context, runID int64) (int64, error) {
	var jobs struct {
		Jobs []struct {
			ID int64 `json:"id"`
		} `json:"jobs"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/jobs", p.apiBase, p.cfg.Owner, p.cfg.Repo, runID)
	if err := restGetJSON(ctx, p.hc, p.tokens, u, &jobs); err != nil {
		return 0, err
	}
	if len(jobs.Jobs) != 1 {
		return 0, fmt.Errorf("run %d has %d jobs; the fixture defines one", runID, len(jobs.Jobs))
	}
	return jobs.Jobs[0].ID, nil
}

// fetchJobStatus reads a job's status and conclusion over REST.
func (p *ssDeleteProbe) fetchJobStatus(ctx context.Context, jobID int64) (string, string, error) {
	var job struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/actions/jobs/%d", p.apiBase, p.cfg.Owner, p.cfg.Repo, jobID)
	if err := restGetJSON(ctx, p.hc, p.tokens, u, &job); err != nil {
		return "", "", err
	}
	return job.Status, job.Conclusion, nil
}

// runnerPresent reports whether a runner record named exactly name is listed.
func (p *ssDeleteProbe) runnerPresent(ctx context.Context, name string) (bool, error) {
	runners, err := p.client.ListRunnersWithPrefix(ctx, name)
	if err != nil {
		return false, err
	}
	for _, r := range runners {
		if r.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// deleteSession tears a session down, best-effort.
func (p *ssDeleteProbe) deleteSession(ctx context.Context, scaleSetID int, sessionID string) {
	dCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := p.client.DeleteSession(dCtx, scaleSetID, sessionID); err != nil {
		p.log.Warn("INVESTIGATION-J: delete session failed", "sessionId", sessionID, "error", err)
	}
}

// shortID is the first eight characters of a job UUID, enough to keep a runner name
// distinct per run and well inside GitHub's runner-name limit.
func shortID(jobID string) string {
	id := strings.ReplaceAll(jobID, "-", "")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// sleepCtx waits d or until ctx ends, reporting whether the full wait elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
