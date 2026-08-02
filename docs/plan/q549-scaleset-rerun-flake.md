# Q549 — the drained scale-set worker's re-run never fires: two modes

`E2E_AGC_ScaleSetDrainedWorkerClaimAndRerunLandUnderChartRBAC`
([worker_scaleset_recovery_test.go](../../cmd/gmc/test/e2e/worker_scaleset_recovery_test.go))
deletes a running scale-set worker and waits 90 s for the AGC to re-run its
disrupted run under the chart role. Two distinct failure modes now sit behind
that one timeout. This file exists so the next occurrence is classified before
anything is changed.

**Status:** watching. Mode A is diagnosed and mitigated (PR #1120). Mode B was
seen once, on 2026-08-01, and is **undiagnosed** — no mechanism is asserted
below. [Q549](../STATUS.md#Q549) stays in
[Flake watch](../STATUS.md#flake-watch) rather than escalating: the documented
revive trigger is recurrence on `main`, and the mode-B sighting was on a PR
branch. It does reset the [soak clock](../development/maintaining-backlog.md#retiring-a-flake-watch-row) —
count green runs from 2026-08-01, not from the fix.

## The two modes

| Mode | Discriminator in the spec | State |
|---|---|---|
| **A** — the AGC control plane was replaced inside the claim window | `agcPodIdentity() != pinnedAGC` at the wait's expiry: the spec records a `Q549 re-staging` report entry and retries the whole staging | Diagnosed on run [30658951388](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30658951388), mitigated by the UID pin + re-stage (PR #1120). Worked case in [testing.md § Pin the process when the signal comes out of its memory](../development/testing.md#pin-the-process-when-the-signal-comes-out-of-its-memory) |
| **B** — the window was undisturbed and the re-run still never fired | the pin is **unchanged**, so the spec takes its `Fail()` branch, with zero `Q549 re-staging` entries | Seen once (below). Cause unverified |

The spec's own failure text names two candidate causes for the `Fail()` branch —
a chart role missing a verb, or a regressed deletion-mark discriminator. Neither
is established for the 2026-08-01 sighting; both remain open alongside anything
else.

## Mode B: what run 30724186342 shows

Run [30724186342](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30724186342)
attempt 1, job `e2e / e2e`, branch `claude/q547-537615` (PR #1140, an unrelated
change — v2 gateway-teardown worker reap), 2026-08-02 00:01–00:03 UTC. Attempt 2
passed with no code change.

Timeline of attempt 1, from the job log:

| Time (UTC) | Event |
|---|---|
| 00:01:16–00:01:24 | the AGC's RunnerSet reconcile loops on `Reconciler error` / `ScaleSetListenerStartFailed` (`scalesetlistener: ensure scale set "e2e-ssrec": scaleset: registration token: POST /orgs/ssrecorg/actions/runners/registration-token …`) |
| 00:01:20.4 | the spec stages the running worker |
| 00:01:23.1 | the worker is deleted gracefully |
| 00:01:23.7–00:01:25.3 | the pod terminates and disappears — the sampler's whole observation window is **~2.2 s** |
| 00:01:25.3 | the 90 s re-run wait starts |
| 00:02:55 | the wait expires; the pin is unchanged, so the spec fails |

Measured, in the order the log gives it:

- **The AGC was not replaced.** The pin held, there is no `Q549 re-staging`
  entry, and the AGC pod (`ssrec-agc-b8d7d6678-vkmmb`) has no previous
  container (`previous terminated container "agc" … not found`) — it never
  restarted either.
- **The claim annotation was never observed on the pod.** The field sampler
  recorded `Running/2026-08-02T00:01:53Z/ -> Failed/2026-08-02T00:01:53Z/`, with
  the `actions-gateway.com/eviction-handled-at` field empty in both states. In
  the passing attempt 2 the same sampler recorded
  `… -> Failed/2026-08-02T00:17:58Z/2026-08-02T00:17:29Z` — claim present. That
  contrast is the sharpest signal the two logs carry. The sampler is
  diagnostics, not evidence: it bounds what was *observed* in ~2.2 s, never what
  happened.
- **No `EvictionRerunFailed` event** in the namespace events dump.
  `rerunUntilAccepted` records that on every terminal failure, so no re-run
  reached a terminal refusal — the same discriminator that ruled out
  "attempted and refused" for mode A.
- **The AGC logged nothing after 00:01:24Z**, through the dump taken at
  00:02:57Z — at debug verbosity, and with no truncation (93 lines against
  `kubectl logs --tail=2000`). The re-run wait ran entirely inside that silence.

Not evidence, and worth stating so it isn't mistaken for some later:
attempt 2's log contains no `ScaleSetListenerStartFailed` at all, but it also
contains no diagnostics dump — that only runs on failure. The two attempts are
**not comparable** on any signal that comes from the dump.

## Open questions

None of these is answered; each names what would answer it.

1. **Did the claim ever land?** The pod is gone by 00:01:25, so it cannot be
   read back after the fact — the sampler's ~2.2 s is the only window there
   ever was. The passing run stamped the claim inside a window of the same
   width, so "too slow to claim before the pod vanished" and "never scanned"
   both fit what was captured, and nothing here separates them.
2. **Is the recovery scan reachable while the RunnerSet reconcile is failing on
   the scale-set listener?** Unchecked — neither the code path nor a repro has
   been looked at. The listener error may well be routine in this e2e; a green
   run's log cannot tell us, per the note above.
3. **Is the AGC's silence after 00:01:24Z the reconcile loop going quiet, or
   something broader?** Unknown from the dump alone.

## What to capture on the next occurrence

- Whether the failure is mode A or mode B — read the pin and the presence of a
  `Q549 re-staging` entry first, before anything else.
- The AGC log **for the full wait window**, not just up to the failure dump:
  whether the recovery scan runs at all during those 90 s is the fork the
  triage above cannot pass.
- The claim annotation's fate: the sampler sequence, plus whether the AGC
  emitted any claim attempt (`could not claim scale-set worker disruption` is
  the refusal line).
- The RunnerSet reconcile's state across the window — erroring, or converged.
