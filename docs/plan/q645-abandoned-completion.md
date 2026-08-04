# Q645 — What the run service does with an `abandoned` completion

**Status:** probe built; live run pending.

Queue item: [Q645](../STATUS.md#Q645). Origin:
[release-1.3.md § The rc.5 re-run](release-1.3.md#the-rc5-re-run-2026-08-02).

## The question

The Q628 fix releases an acquired-but-never-run job assignment (a worker pod
reaped while `Pending`, so no runner binary ever registered) by calling
`POST {run_service_url}/completejob` with `result=abandoned`
(`cmd/agc/internal/listener/job.go`, behind `AGC_FANOUT_COMPLETION`). That stops
the assignment dangling until GitHub's ~15-minute unstarted-job timeout cancels
the whole run. What it does **not** establish is what GitHub does next:

1. **Re-dispatch.** The job returns to the queue and is offered to the next
   available runner. The AGC needs no further mechanism: when capacity returns,
   a new worker picks the job up.
2. **Conclude.** The job (and with it the run) is driven terminal. Then Q628
   trades a silent 16-minute hang for a fast, visible failure. Better, but a
   re-run arm (the `rerun-failed-jobs` mechanism the disruption-recovery arms
   already have) would be needed for the job to ever execute.

Which one happens decides whether a re-run arm is also needed, which is the
decision the Q645 row exists to inform. A secondary unknown rides along for
free: `broker/types.go` flags the completejob **wire format** (`result` as a
lowercase-camelCase string) as never live-confirmed, the reason
`AGC_FANOUT_COMPLETION` defaults off. The same live call that answers the
primary question confirms or refutes the serialization.

## Why a probe, not a dogfood turn-up

The Q645 row proposes a turn-up: reap a Pending worker on GKE, watch the run.
That measures the same wire exchange, but through five extra moving layers
(cluster, autoscaler, AGC deploy, worker scheduling, reap timing), each of
which has independently eaten a validation attempt in the 1.3 ledger. The
question is about **GitHub's behaviour, not ours**, which is exactly the class
[testing.md § The credential-gated probe scenarios](../development/testing.md#the-credential-gated-probe-scenarios)
routes to `cmd/probe`: no double can answer it, and no cluster is needed to ask
it. The probe drives the same shipping `broker.Client` calls the listener
makes, so the exchange under measurement is byte-identical to the AGC's.

## Investigation H — the measurement

`PROBE_ABANDONED_TEST=true` selects it. One run, two JIT runners, both
registered repo-level against this repo (the org `Default` group refuses public
repos, same as Investigations E/F/G):

1. Register JIT runners **A** and **B** with the probe label
   (`gag-q645-abandoned`) via `generate-jitconfig`, and open a broker v2
   session for each. This is the registration, OAuth-exchange, and session
   flow the AGC's agent pool and listener run
   (`agentpool.GithubRegistrar` / `listener.createSession`), reproduced in the
   probe over the shared `githubapp` and `broker` packages.
2. Dispatch the fixture workflow
   ([`q645-abandoned-probe.yml`](../../.github/workflows/q645-abandoned-probe.yml)):
   one job, `runs-on: gag-q645-abandoned`, nothing ever runs it.
3. Both sessions receive the fan-out `RunnerJobRequest`. **A acquires** its
   delivery (`acquirejob` on the delivered `run_service_url`); B only polls and
   logs. The probe resolves the run REST-side (newest queued run of the fixture
   workflow) before acquiring, so the REST watch has a subject.
4. **A completes its assignment with `result=abandoned`**: the Q628 call,
   nothing else in between. No renew loop is needed, since the initial lock
   outlasts the seconds between acquire and complete. T0 is this call's
   response. A's session is then deleted and its consumed runner record
   deregistered, mirroring the listener's post-job recycle, so B is the only
   listener during the window.
5. Observe for a bounded window (default 20 min, spanning the ~15-minute
   unstarted-job horizon), on two channels at once:
   - **B's session.** A `RunnerJobRequest` arriving after T0 is a
     re-dispatch, observed on the same channel a real AGC listener would see
     it on. Its `runner_request_id` (same as A's, or fresh) is recorded:
     that is the fan-out identity a recovering AGC would have to dedup against.
   - **REST.** The fixture job's `status`/`conclusion` polled every 15 s;
     every transition logged with its timestamp.

Verdicts, each logged with the evidence behind it:

| Verdict | Meaning |
|---|---|
| `WIRE-ACCEPTED` / `WIRE-REJECTED` | completejob's response status: the serialization confirmation `broker/types.go` is waiting on. Rejected ends the run, since the primary question cannot be asked with a call the service refused. |
| `REDISPATCHED` | B received a job delivery after T0. The job survives an abandoned completion; no re-run arm is needed. |
| `CONCLUDED-<conclusion>` | The REST job went terminal without redelivery. An abandoned completion kills the job; a re-run arm is needed for it to ever execute. |
| `NO-SIGNAL` | Neither within the window. The job is dangling exactly as if nothing had been reported, itself a finding: completejob(abandoned) released nothing. |

`REDISPATCHED` and `CONCLUDED-*` are not mutually exclusive in principle
(GitHub could re-queue, fail to place, then conclude at the unstarted-job
timeout); the log carries both channels' full timelines so a compound outcome
reads as what it is rather than being flattened into one word.

### What would make a result invalid

- **B's session missing or late.** B must be polling before T0, or a prompt
  re-dispatch lands on no listener and `NO-SIGNAL` lies. The probe opens both
  sessions before the acquire and fails the run if either drops.
- **Another consumer on the label.** A leftover runner from an earlier run
  acquiring the redelivery would turn `REDISPATCHED` into `NO-SIGNAL` or
  `CONCLUDED`. The label is probe-specific and both runner records are
  deregistered on exit; the probe logs every runner on the label at startup.
- **The wrong run watched.** REST discovery takes the newest queued run of the
  fixture workflow. A stale queued run from an aborted earlier probe would be
  older, but the probe logs the run id and `created_at` it locked onto so a
  misattribution is visible in the record.
- **A cancel racing the window.** Cleanup cancels the fixture run only after
  the window closes (409-tolerant, per Investigation F's helper); a cancel
  before T0+window would manufacture `CONCLUDED-cancelled`.

## What the answer feeds

- **`REDISPATCHED`** → Q628 is complete as shipped; the Q645 row is deleted
  and the `release-1.3.md` "unmeasured" sentence gets the answer. The
  `AGC_FANOUT_COMPLETION` default-off caveat in `broker/types.go` can cite a
  live confirmation instead of a gap (flipping the default is its own
  decision, filed separately if taken).
- **`CONCLUDED-*`** → a new Queue row for the re-run arm: on an abandoned
  completion the listener must also call `rerun-failed-jobs` (or the run-level
  re-run), the mechanism `externallyDeletedBeforeTerminal` deliberately does
  not arm today because "a job that never ran has no failed job to re-run",
  which this verdict would refute at the run level.
- **`WIRE-REJECTED`** → the `TaskResult` serialization is wrong; fixing it
  precedes everything above, and the gated-off default was the right call.

## Findings

*(recorded after the live run)*
