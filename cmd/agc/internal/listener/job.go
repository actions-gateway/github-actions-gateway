package listener

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/actions-gateway/github-actions-gateway/broker"
	corev1 "k8s.io/api/core/v1"
)

// handleJob acquires a job, notifies the multiplexer, starts the renew loop,
// calls the job handler, and returns. acquired reports whether AcquireJob
// succeeded — the point at which GitHub considers the single-use JIT runner
// record spent (Q114); the caller recycles the agent afterwards. The session
// itself is NOT closed here. aesKey is the AES-256-CBC key derived from the
// session's encryptionKey; nil means no encryption and the body is parsed as
// plaintext JSON.
func handleJob(ctx context.Context, cfg Config, log *slog.Logger, aesKey []byte, msg *broker.TaskAgentMessage) (acquired bool, err error) {
	// Decrypt message body with the session key, then parse as RunnerJobRequestBody.
	bodyBytes := []byte(msg.Body)
	if aesKey != nil {
		decrypted, err := broker.DecryptMessageBody(msg.Body, aesKey)
		if err != nil {
			log.Warn("failed to decrypt message body; falling back to plaintext parse", "error", err)
		} else {
			bodyBytes = decrypted
		}
	}

	var jobBody broker.RunnerJobRequestBody
	if err := json.Unmarshal(bodyBytes, &jobBody); err != nil {
		log.Warn("could not parse job body; skipping AcquireJob", "error", err)
	}

	// Admission gate (Q59): reserve worker capacity BEFORE AcquireJob claims the
	// job from GitHub. If the gate is full, skip the acquire so the job stays
	// queued at GitHub and is redelivered to a sibling session with capacity —
	// rather than claiming a job whose worker pod we cannot place, which would be
	// cancelled when its unrenewed lock lapses (failure shape 1 in the Q59 plan).
	// admitRelease frees the reserved worker slot; nil when the gate is disabled.
	// The AdmitFunc's closure is idempotent, so the deferred release and any earlier
	// explicit release (the deduped-loser path below) together free it exactly once.
	var admitRelease func()
	if cfg.Admit != nil {
		release, ok, reason := cfg.Admit(ctx)
		if !ok {
			if cfg.Metrics != nil {
				cfg.Metrics.JobsAdmissionRejectedTotal.WithLabelValues(cfg.Namespace, cfg.Group, reason).Inc()
			}
			// Per-delivery line that can be high-volume under sustained capacity
			// pressure; Debug, with the metric as the operator-facing signal (Q87, Theme D).
			// reason distinguishes the configured ceiling from exhausted namespace quota.
			log.Debug("job admission rejected: no worker capacity; leaving job queued for redelivery",
				"reason", reason, "messageId", msg.MessageID)
			return false, nil
		}
		admitRelease = release
		// Hold the reservation until handleJob returns. On the acquire path that is
		// pod terminal (JobHandler has returned by then); on any earlier return it
		// fires immediately. Either way the gate's in-flight count tracks only live
		// jobs. Released exactly once via the AdmitFunc's idempotent closure.
		defer release()
	}

	var (
		payload       []byte
		planID        = "stub"
		runServiceURL = jobBody.RunServiceURL
		// jobToken is the job-scoped bearer token the run service returns in the
		// acquirejob response (the SystemVssConnection AccessToken). RenewJob must
		// present it: the run service rejects the broker session token for per-job
		// lock renewal with 401 "Not authorized for this job" (Q247).
		jobToken string
	)

	// Call AcquireJob if we have a runServiceURL. Bounded by the control-plane
	// timeout for the same reason as createSession: it is a short request/response
	// call (not the long-poll), so an unresponsive broker here must not wedge the
	// goroutine — that would block job pickup and the worker pod would never spawn
	// (Q134 class). A timeout surfaces as a recoverable AcquireJob error; the poll
	// loop logs it and continues, re-acquiring on the next delivery.
	if runServiceURL != "" {
		acqCtx, cancelAcq := context.WithTimeout(ctx, cfg.controlPlaneTimeout())
		resp, rawBytes, acqErr := cfg.Broker.AcquireJob(acqCtx, runServiceURL, broker.JobAcquisitionRequest{
			JobMessageID:   jobBody.RunnerRequestID,
			RunnerOS:       cfg.RunnerOS,
			BillingOwnerID: jobBody.BillingOwnerID,
		})
		cancelAcq()
		if acqErr != nil {
			if cfg.Metrics != nil {
				cfg.Metrics.JobAcquisitionErrors.WithLabelValues(cfg.Namespace, "acquirejob_failed").Inc()
			}
			recordEvent(cfg, corev1.EventTypeWarning, "JobAcquisitionFailed", "AcquireJob",
				fmt.Sprintf("failed to acquire a delivered job from GitHub: %v; the job stays queued at GitHub for redelivery to a sibling session", acqErr))
			log.Error("AcquireJob failed", "error", acqErr)
			return false, acqErr
		}
		acquired = true
		// The acquisition just consumed this agent's single-use JIT runner
		// record. Record it on the pool now, before the long job wait, so the
		// agent is parked (not re-issued) if this goroutine dies mid-job (Q114).
		if cfg.MarkAgentConsumed != nil {
			cfg.MarkAgentConsumed()
		}
		planID = resp.Plan.PlanID
		payload = rawBytes
		jobToken = resp.JobAuthToken()
		if jobToken == "" {
			// The run service authorizes per-job renewal with this token; without it
			// RenewJob falls back to the broker session token, which the run service
			// rejects with 401 "Not authorized for this job", so the lock lapses at
			// its ~10-minute TTL (Q247). Warn so a protocol drift that drops the token
			// is visible rather than silently re-orphaning long jobs.
			log.Warn("AcquireJob response carried no SystemVssConnection token; " +
				"RenewJob will fall back to the broker token and the run service may reject the renewal (Q247)")
		}
	} else {
		payload = []byte(msg.Body)
	}

	// Dedup gate (Q260): claim this job by its planID AFTER AcquireJob (planID is
	// only known post-acquire) but BEFORE provisioning. Under a concurrent burst
	// the broker fans one job out to several sibling sessions with DISTINCT
	// RunnerRequestIDs but the SAME planID, so without this every sibling would
	// race to create the per-job worker Secret "job-<planID>" and die with its
	// runner slot burned. A losing sibling skips provisioning and returns
	// acquired=true so its consumed single-use runner is recycled back online.
	// The claim is held for the whole job and released on return, so a later
	// GitHub redelivery is provisionable again. See Config.ClaimJob.
	//
	// jobResult is the winner's pod-phase-proxy result, fanned out to the deduped
	// sibling deliveries on completion (Q260 Option A). It defaults to succeeded
	// and is overwritten by the JobHandler's terminal result below; it is unused
	// on the loser path.
	jobResult := broker.TaskResultSucceeded
	if cfg.ClaimJob != nil && acquired && planID != "" {
		delivery := SiblingDelivery{
			RunnerRequestID: jobBody.RunnerRequestID,
			RunServiceURL:   runServiceURL,
			JobToken:        jobToken,
		}
		claim := cfg.ClaimJob(planID, delivery)
		if !claim.Won {
			if cfg.Metrics != nil {
				cfg.Metrics.JobsDuplicateDeliveryTotal.WithLabelValues(cfg.Namespace, cfg.Group).Inc()
			}
			// High-volume under a burst of duplicate deliveries; Debug, with the
			// metric as the operator-facing signal (Q87, Theme D). acquired stays
			// true so the caller recycles the consumed runner (slot reclaimed);
			// SpawnReplacement/renew/provision are all skipped for the loser.
			log.Debug("duplicate job delivery: planID already claimed by a sibling session; skipping provisioning and recycling the runner slot",
				"planID", planID, "runnerRequestID", jobBody.RunnerRequestID)
			// Q260 Option A (guarded): the loser already ran AcquireJob, so GitHub
			// holds a per-delivery assignment for it. If the winner is still running,
			// this delivery was registered on the claim and the winner completes it on
			// finish. If the job ALREADY concluded (a late redelivery within the
			// linger window, when the winner is gone), resolve this delivery here with
			// the winner's recorded result — keyed on this delivery's OWN jobID
			// (distinct from the winner's), so under the per-delivery lock model
			// (Q247) it resolves only this assignment. Gated by
			// Config.FanoutCompletion.
			if cfg.FanoutCompletion && claim.LateResult != "" && runServiceURL != "" {
				completeDelivery(ctx, cfg, log, planID, delivery, claim.LateResult)
				return acquired, nil
			}
			// Q266: the winner is still running. GitHub considers THIS deduped runner
			// assigned to the job (the loser's own AcquireJob claimed a per-delivery
			// assignment), so its recycle 422s — "runner is currently running a job and
			// cannot be deleted" — for the winner's ENTIRE runtime. That far outlasts
			// the bounded Q259 recycle backoff, so recycling now would give up and exit
			// the listener; under sustained fan-out burst enough losers strand+exit to
			// collapse the pool. Instead HOLD this slot until the winner concludes —
			// when it fans completjob out to this delivery (Option A), releasing the
			// assignment so the 422 finally clears — then let the caller recycle
			// normally. Only fan-out completion clears the loser's 422, so the defer
			// applies only when it is enabled; with it off, fall through to the eager
			// recycle of the documented (worse) opt-out path.
			if cfg.FanoutCompletion && claim.WinnerConcluded != nil {
				// The worker slot reserved above is for a pod this loser will never
				// provision. Free it before parking so a deduped loser never pins
				// worker capacity while it waits — that would starve the winner's own
				// pod under a tight maxWorkers ceiling (Q248). Idempotent with the
				// deferred release.
				if admitRelease != nil {
					admitRelease()
				}
				outcome := waitForWinnerConclusion(ctx, cfg, log, planID, claim.WinnerConcluded)
				if cfg.Metrics != nil {
					cfg.Metrics.FanoutLoserRecycleDeferredTotal.WithLabelValues(cfg.Namespace, cfg.Group, outcome).Inc()
				}
			}
			return acquired, nil
		}
		// Winner: when the job finishes, conclude the claim (always — this replaces
		// the pre-Option-A release, so the claim still lingers past completion for
		// the #512 redelivery dedup) and, when enabled, fan completjob out to every
		// deduped sibling delivery so none dangles at GitHub's unstarted-job timeout.
		defer func() {
			siblings := claim.Complete(jobResult)
			if cfg.FanoutCompletion && runServiceURL != "" && len(siblings) > 0 {
				<-completeSiblingDeliveries(ctx, cfg, log, planID, siblings, jobResult)
			}
		}()
	}

	// Notify multiplexer to spawn a replacement listener before blocking on job handler.
	if cfg.SpawnReplacement != nil {
		cfg.SpawnReplacement(ctx)
	}

	// Start renew loop for this job.
	renewInterval := cfg.RenewInterval
	if renewInterval == 0 {
		renewInterval = 60 * time.Second
	}
	// RenewJob's jobId is the job's RunnerRequestID — the same value AcquireJob
	// sends as jobMessageId — NOT the broker envelope's numeric MessageID. Sending
	// the MessageID renews a job the run service does not recognize, so the lock is
	// never actually renewed: on any job that outlives GitHub's lock TTL the job is
	// recycled and redelivered to a sibling session (a duplicate worker pod), while
	// this worker runs to completion and then orphans at CompleteJobAsync with
	// TaskOrchestrationJobNotFoundException (Q247). Short jobs finish before the TTL
	// lapses, which is why only long jobs (e.g. e2e) exposed it.
	jobID := jobBody.RunnerRequestID
	// Bound each RenewJob call with the same per-call deadline as AcquireJob, so a
	// black-holed renewal (egress path saturated under load) aborts instead of
	// wedging the loop and starving every later renewal until the lock lapses (the
	// Q247 residual — an exactly-~10-minute orphan even with the correct jobId).
	// jobToken authorizes the renewal: the run service rejects the broker session
	// token for per-job renewal (401 "Not authorized for this job") even though it
	// accepted the same token to claim the job — the third and final Q247 facet.
	// Derive a per-job context the renew loop can cancel. When the loop detects the
	// job's lock is definitively lost (a definitive job-gone response or a sustained
	// run of renewal failures), it calls cancelJob so the JobHandler's context is
	// cancelled and the worker tears down — rather than running on to completion as
	// an orphan pod while GitHub recycles the job and redelivers it to a sibling
	// session (a duplicate acquire). On the normal path cancelJob fires via defer
	// once the job completes (a no-op teardown of an already-finished worker) (Q254).
	//
	// WithCancelCause, not WithCancel: the JobHandler reclaims the worker pod only
	// when THIS job was abandoned, and the parent ctx's own cancellation (AGC
	// shutdown) reaches the same job context. Only the cause tells them apart (Q501).
	jobCtx, cancelJob := context.WithCancelCause(ctx)
	defer cancelJob(nil)
	stop, renewDone := StartRenewLoop(jobCtx, cancelJob, cfg.Broker, runServiceURL, planID, jobID, jobToken,
		cfg.Metrics, cfg.Namespace, cfg.Clock, log, renewInterval, cfg.controlPlaneTimeout())
	// Cancel the renew loop and wait for it to exit before returning, so the
	// goroutine never outlives the job it renews.
	defer func() { stop(); <-renewDone }()

	if cfg.Metrics != nil {
		cfg.Metrics.JobsAcquiredTotal.WithLabelValues(cfg.Namespace, cfg.Group).Inc()
	}

	if cfg.JobHandler != nil {
		result, jobErr := cfg.JobHandler(jobCtx, runServiceURL, planID, payload, cfg.Agent.EncodedJITConfig)
		// Record the pod-phase proxy for the winner's deferred sibling fan-out; keep
		// the succeeded default on an empty result (the pod never reached a terminal
		// phase — e.g. a provisioning error), matching "PodFailed→failed, else
		// succeeded" (Q260 Option A).
		if result != "" {
			jobResult = result
		}
		// Abandoned means the worker was removed before it ran, so the runner binary
		// never registered and nothing will ever report THIS delivery. Release it on
		// the same terms as a deduped sibling — it is the same dangling assignment —
		// rather than leave GitHub told nothing (Q628). Measured (Q645 Investigation
		// H): this call concludes the run as SUCCESS immediately, it does not re-queue
		// the job — a false green for a job that never ran; the remedy is Q676, and
		// AGC_FANOUT_COMPLETION stays off until it lands. ctx, not jobCtx: the renew
		// loop may have cancelled the job, which is exactly when the release matters.
		if result == broker.TaskResultAbandoned && cfg.FanoutCompletion && runServiceURL != "" {
			completeDelivery(ctx, cfg, log, planID, SiblingDelivery{
				RunnerRequestID: jobID,
				RunServiceURL:   runServiceURL,
				JobToken:        jobToken,
			}, result)
		}
		return acquired, jobErr
	}
	return acquired, nil
}

// completeSiblingDeliveries fans completjob out to every deduped sibling delivery
// of a fanned-out job concurrently, on a background goroutine, and returns a done
// channel closed once all completions have been attempted (Q260 Option A). It is
// async per CLAUDE.md's channel convention: the winner may block on the channel (as
// handleJob does, so its recycle happens after the assignments are resolved) or
// ignore it. Each call is bounded and best-effort — see completeSiblingDelivery.
func completeSiblingDeliveries(ctx context.Context, cfg Config, log *slog.Logger, planID string, siblings []SiblingDelivery, result broker.TaskResult) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, sib := range siblings {
			wg.Add(1)
			go func(sib SiblingDelivery) {
				defer wg.Done()
				completeDelivery(ctx, cfg, log, planID, sib, result)
			}(sib)
		}
		wg.Wait()
	}()
	return done
}

// completeDelivery resolves one acquired-but-unrun job assignment via completejob so
// GitHub does not leave it dangling until the ~15-minute unstarted-job timeout and
// cancel the whole job (Q260 Option A). Two deliveries reach it: a deduped sibling the
// winner is reconciling, and — when the winner's own worker was removed before it ran,
// so the runner binary never reported anything — the winner's own (Q628).
//
// sib.RunnerRequestID is that delivery's OWN jobID; under the per-delivery lock model
// (Q247) completing it resolves only that assignment. Best-effort: the call is bounded
// by the control-plane timeout and failures are logged and counted, never fatal — the
// runner still recycles its slot. Gated by Config.FanoutCompletion.
func completeDelivery(ctx context.Context, cfg Config, log *slog.Logger, planID string, sib SiblingDelivery, result broker.TaskResult) {
	cctx, cancel := context.WithTimeout(ctx, cfg.controlPlaneTimeout())
	defer cancel()
	err := cfg.Broker.CompleteJob(cctx, sib.RunServiceURL, broker.CompleteJobRequest{
		PlanID:    planID,
		JobID:     sib.RunnerRequestID,
		Result:    result,
		AuthToken: sib.JobToken,
	})

	outcome := "completed"
	var notFound *broker.JobNotFoundError
	switch {
	case err == nil:
		log.Debug("released an acquired-but-unrun job assignment via completejob so GitHub does not cancel the job at its unstarted-job timeout",
			"planID", planID, "jobID", sib.RunnerRequestID, "result", result)
	case errors.As(err, &notFound):
		// The assignment is already gone server-side — nothing left to resolve.
		log.Debug("job assignment already resolved server-side",
			"planID", planID, "jobID", sib.RunnerRequestID)
	default:
		outcome = "error"
		log.Warn("failed to release an acquired-but-unrun job assignment; GitHub may cancel the job at its unstarted-job timeout",
			"planID", planID, "jobID", sib.RunnerRequestID, "error", err)
	}
	if cfg.Metrics != nil {
		cfg.Metrics.AbandonedDeliveryCompletionsTotal.WithLabelValues(cfg.Namespace, cfg.Group, outcome).Inc()
	}
}

// defaultLoserRecycleDeferTimeout bounds a deduped fan-out loser's wait for its
// winner to conclude before it recycles anyway (Q266). It sits just past GitHub's
// ~15-minute unstarted-job timeout: if the winner never concludes (crash/hang),
// GitHub cancels the whole job at that timeout and releases the loser's assignment,
// so the loser's recycle 422 has cleared by the time this fallback fires — the wait
// is a safety valve, not the normal path (a winner concludes in seconds to minutes
// and always signals via its deferred Complete).
const defaultLoserRecycleDeferTimeout = 16 * time.Minute

// waitForWinnerConclusion blocks a deduped fan-out loser until its winner concludes
// — the point at which the winner fans completjob out to this loser's delivery
// (Option A), releasing GitHub's assignment on the loser's deduped runner so its
// recycle 422 clears (Q266). It returns the outcome for the metric: "winner_concluded"
// on the signal, "fallback_timeout" if the winner did not conclude within the bound
// (a winner crash/hang; GitHub's unstarted-job timeout has released the assignment by
// then), or "context_cancelled" on shutdown. The loser holds its listener slot and
// pool agent throughout — it is not counted as a poller (SetPolling(false) was set
// before the job) so it is never mistaken for available capacity.
func waitForWinnerConclusion(ctx context.Context, cfg Config, log *slog.Logger, planID string, winnerConcluded <-chan struct{}) string {
	timeout := cfg.LoserRecycleDeferTimeout
	if timeout <= 0 {
		timeout = defaultLoserRecycleDeferTimeout
	}
	select {
	case <-winnerConcluded:
		return "winner_concluded"
	case <-cfg.Clock.After(timeout):
		log.Warn("deferred loser recycle: winner did not conclude within the fallback bound; recycling anyway "+
			"(GitHub's unstarted-job timeout should have released this deduped runner's assignment by now)",
			"planID", planID, "timeout", timeout)
		return "fallback_timeout"
	case <-ctx.Done():
		return "context_cancelled"
	}
}
