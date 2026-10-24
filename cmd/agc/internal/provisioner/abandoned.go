package provisioner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

// abandonedForceCancelTimeout bounds the single force-cancel POST. The call is on
// the provision goroutine's return path — deliberately: the cancelled conclusion is
// what unpins the acquired job's runner record (deleting it answers 422 "currently
// running a job" until the run concludes), so the recycle that follows this return
// must not race it.
const abandonedForceCancelTimeout = 30 * time.Second

// forceCancelAbandonedRun drives the run of an acquired-but-never-run job terminal.
// Nothing will ever report such a job — its worker was removed before the runner
// binary registered, and every accepted completejob value concludes the run as a
// false green while failed is refused (Q645/Q676) — so told nothing, GitHub cancels
// the run and job at its ~15-minute unstarted-job timeout. A standalone REST
// force-cancel (no prior plain cancel; a plain cancel alone is sluggish against an
// orphaned acquire) reaches the same cancelled conclusion in about a second, and the
// cancelled run accepts rerun-failed-jobs where the false green refused it (measured
// live 2026-08-05, Q683; the Q645 plan doc carries the runs).
//
// Best-effort: on an unknown identity or a refused call the unstarted-job timeout
// remains the honest backstop, so every outcome is logged and counted, none returned.
//
// A cancelled run is also registered for automatic re-run once the owner can place a
// worker pod again (Q691) — not immediately, because the job was abandoned for want of
// capacity and re-queueing it into the same starved pool is how a shortage compounds.
//
// tier labels the metric; both acquisition tiers run the identical call (Q766). On the
// scale-set tier the run's attempt is read and recorded in the owner's failure ledger
// as GAG's own cancel before cancelling, so the cancelled conclusion it produces is not
// taken for a user's (Q1182), and the re-run it registers is judged against that
// attempt. Neither step blocks the cancel: without them the run is cancelled as before,
// and a later deletion-arm check reads the cancel as a user's, the safe direction. d
// names the abandoned worker to the ledger, and is ignored on the classic tier.
func (p *Provisioner) forceCancelAbandonedRun(ctx context.Context, target Target, owner, repo, runID, tier string, log *slog.Logger, d rerunDisruption) {
	outcome := "cancelled"
	if owner == "" || repo == "" || runID == "" || runID == "0" {
		log.Warn("run identity unknown; cannot force-cancel the abandoned job's run; GitHub cancels it at its ~15-minute unstarted-job timeout")
		outcome = "identity_unknown"
	} else {
		if tier == evictionTierScaleSet {
			// Its own deadline, so a slow ledger write cannot spend the cancel's.
			rctx, rcancel := context.WithTimeout(ctx, abandonedForceCancelTimeout)
			d.failedAttempt = p.recordAbandonedAttempt(rctx, target, owner, repo, runID, log)
			rcancel()
		}
		cctx, cancel := context.WithTimeout(ctx, abandonedForceCancelTimeout)
		err := p.forceCancelRun(cctx, owner, repo, runID)
		cancel()
		if err != nil {
			outcome = "error"
			log.Warn("force-cancel of the abandoned job's run failed; GitHub cancels it at its ~15-minute unstarted-job timeout",
				"runID", runID, "error", err)
		} else {
			log.Info("force-cancelled the abandoned job's run: the worker was removed before it ran and nothing will ever report the job",
				"runID", runID)
			// Only this outcome is re-runnable: the cancelled conclusion is the state
			// rerun-failed-jobs was measured to accept, and it is the one outcome in
			// which we know the run concluded at all (Q691, abandoned_rerun.go).
			p.registerAbandonedRerun(target, owner, repo, runID, tier, d)
		}
	}
	if p.Metrics != nil {
		key := target.Key()
		p.Metrics.AbandonedRunForceCancels.WithLabelValues(key.Namespace, key.Name, tier, outcome).Inc()
	}
}

// recordAbandonedAttempt reads the run's attempt and records it in target's failure
// ledger as the one GAG is about to force-cancel, returning it, or 0 where either step
// failed.
func (p *Provisioner) recordAbandonedAttempt(ctx context.Context, target Target, owner, repo, runID string, log *slog.Logger) int {
	run, err := p.runConclusion(ctx, owner, repo, runID)
	if err != nil || run.attempt == 0 {
		log.Warn("could not read the abandoned job's run attempt; force-cancelling without recording it, so the cancel may read as a user's",
			"runID", runID, "error", err)
		return 0
	}
	if err := p.recordSelfCancel(ctx, target, runID, run.attempt); err != nil {
		log.Warn("could not record GAG's own force-cancel in the failure ledger; force-cancelling anyway, so the cancel may read as a user's",
			"runID", runID, "runAttempt", run.attempt, "error", err)
	}
	return run.attempt
}

// forceCancelRun calls POST /repos/{owner}/{repo}/actions/runs/{run_id}/force-cancel.
// GitHub answers 202 Accepted. The guards mirror rerunFailedJobs.
func (p *Provisioner) forceCancelRun(ctx context.Context, owner, repo, runID string) error {
	if !repoSegmentRE.MatchString(owner) || !repoSegmentRE.MatchString(repo) {
		return fmt.Errorf("invalid owner/repo characters: %q/%q", owner, repo)
	}
	if p.TokenFunc == nil {
		return fmt.Errorf("TokenFunc is not configured")
	}
	token, err := p.TokenFunc(ctx)
	if err != nil {
		return fmt.Errorf("get installation token: %w", err)
	}
	// No silent default: refusing to guess an endpoint keeps the installation
	// token from being posted to a host that never issued it (see GitHubAPIURL, Q504).
	if p.GitHubAPIURL == "" {
		return fmt.Errorf("GitHubAPIURL is not configured; refusing to guess an endpoint for the force-cancel call")
	}
	apiURL := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%s/force-cancel",
		p.GitHubAPIURL,
		neturl.PathEscape(owner),
		neturl.PathEscape(repo),
		neturl.PathEscape(runID))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, nil)
	if err != nil {
		return fmt.Errorf("build force-cancel request: %w", err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	hc := p.HTTPClient
	if hc == nil {
		hc = defaultProvisionerClient()
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("force-cancel API call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("force-cancel API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
