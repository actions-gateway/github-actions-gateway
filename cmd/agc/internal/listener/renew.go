package listener

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/actions-gateway/github-actions-gateway/broker"
)

// StartRenewLoop starts a per-job renewal goroutine that ticks on the given interval.
// It returns a stop function that cancels the loop and a done channel that closes
// once the loop goroutine has fully exited. Callers must call stop when the job
// completes to avoid goroutine leaks; they may then wait on done if they need to
// guarantee the goroutine has stopped before releasing shared resources.
//
// jobToken is the job-scoped bearer token from the acquirejob response
// (AcquireJobResponse.JobAuthToken). Each RenewJob call presents it instead of the
// broker session token: the run service rejects the session token for per-job lock
// renewal with 401 "Not authorized for this job" even though it accepted the same
// token to claim the job, so without jobToken every renewal fails and the lock
// lapses at its ~10-minute TTL (Q247). An empty jobToken falls back to the client's
// session token (test/stub use, or a run service that authorizes renewal with it).
//
// renewCallTimeout bounds each individual RenewJob call. It MUST be smaller than
// renewInterval and smaller than GitHub's lock TTL. The renewal call runs inline
// in the loop, so an unbounded call that black-holes (egress-proxy path saturated
// under heavy worker load — the Q247 residual) would wedge the goroutine and
// starve every subsequent tick until it returned, letting the job's lock lapse at
// the initial ~10-minute TTL even when the jobId is correct. A bounded call aborts
// (counted as a non-fatal RenewJob error), and the loop proceeds to the next tick,
// so a single slow renewal costs one renewal, not all of them. A zero value leaves
// the call unbounded (test/stub use only).
//
// cancelJob tears the worker down when the job's lock is definitively lost: a
// definitive job-gone response (broker.JobNotFoundError, 404/410) or a sustained
// run of renewFailureThreshold consecutive renewal failures. Without it the loop
// would keep logging every failure as non-fatal and the worker would run on to
// completion as an orphan pod while GitHub recycles the job and redelivers it to a
// sibling session (a duplicate acquire) — the Q247 residual. A single/transient
// failure stays non-fatal and is retried (GitHub grants ~10 min per renewal
// window). cancelJob may be nil (test/stub use); teardown is then a no-op.
func StartRenewLoop(
	ctx context.Context,
	cancelJob context.CancelFunc,
	client *broker.Client,
	runServiceURL, planID, jobID, jobToken string,
	metrics *runnercore.Metrics,
	namespace string,
	clk Clock,
	log *slog.Logger,
	renewInterval, renewCallTimeout time.Duration,
) (stop func(), done <-chan struct{}) {
	stopCtx, cancel := context.WithCancel(ctx)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		var consecutiveFailures int
		for {
			select {
			case <-stopCtx.Done():
				return
			case <-clk.After(renewInterval):
				if runServiceURL == "" {
					continue // M2 stub: no real run service URL
				}
				callCtx, cancelCall := stopCtx, context.CancelFunc(func() {})
				if renewCallTimeout > 0 {
					callCtx, cancelCall = context.WithTimeout(stopCtx, renewCallTimeout)
				}
				_, err := client.RenewJob(callCtx, runServiceURL, broker.RenewJobRequest{
					PlanID:    planID,
					JobID:     jobID,
					AuthToken: jobToken,
				})
				cancelCall()
				if err == nil {
					consecutiveFailures = 0
					continue
				}
				// A renewal aborted only because the loop itself is shutting down
				// (stop() cancelled stopCtx mid-call) is not a lock failure — don't
				// count it toward teardown; just exit.
				if stopCtx.Err() != nil {
					return
				}
				consecutiveFailures++
				if metrics != nil {
					metrics.RenewJobErrorsTotal.WithLabelValues(namespace).Inc()
				}
				if reason := renewTeardownReason(err, consecutiveFailures); reason != "" {
					if metrics != nil {
						metrics.RenewJobTeardownsTotal.WithLabelValues(namespace, reason).Inc()
					}
					if log != nil {
						log.Error("RenewJob: job lock definitively lost; cancelling worker to avoid an orphan pod and a sibling duplicate-acquire (Q254)",
							"reason", reason, "consecutiveFailures", consecutiveFailures, "error", err)
					}
					if cancelJob != nil {
						cancelJob()
					}
					return
				}
				if log != nil {
					log.Warn("RenewJob error (non-fatal)", "error", err, "consecutiveFailures", consecutiveFailures)
				}
			}
		}
	}()
	return cancel, doneCh
}

// renewFailureThreshold is the number of consecutive RenewJob failures that trips
// a worker teardown. With the default 60s renew interval and GitHub's ~10-minute
// lock TTL, 5 consecutive failures (~5 min of a sustained outage) is well past any
// single transient blip, yet still tears the worker down before the lock lapses at
// ~10 min — so the orphan pod is gone before GitHub can recycle the job and
// redeliver it to a sibling session (the duplicate-acquire window) (Q254).
const renewFailureThreshold = 5

// renewTeardownReason returns a non-empty metric reason when a RenewJob error
// means the job's lock is unrecoverably lost and the worker must be cancelled: a
// definitive job-gone response (broker.JobNotFoundError, 404/410), or a sustained
// run of consecutive failures reaching renewFailureThreshold. It returns "" for a
// transient failure that should stay non-fatal and be retried.
func renewTeardownReason(err error, consecutiveFailures int) string {
	var notFound *broker.JobNotFoundError
	if errors.As(err, &notFound) {
		return "job_not_found"
	}
	if consecutiveFailures >= renewFailureThreshold {
		return "consecutive_failures"
	}
	return ""
}
