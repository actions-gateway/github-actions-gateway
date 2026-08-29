# Q549: the drained scale-set worker's re-run never fires, in four modes

`E2E_AGC_ScaleSetDrainedWorkerClaimAndRerunLandUnderChartRBAC` ([worker_scaleset_recovery_test.go](../../cmd/gmc/test/e2e/worker_scaleset_recovery_test.go)) deletes a running scale-set worker and waits 90 s for the AGC to re-run its disrupted run under the chart role.
Four distinct failure modes now sit behind that one timeout, and only one of them is the defect the spec exists to catch.
This file exists so the next occurrence is classified before anything is changed.

**Status:** the 2026-08-26 and 2026-08-28 sightings are attributed, as **two** further shapes rather than one, and both are mitigated.
Mode A is diagnosed and mitigated (PR #1120).
**Mode B is diagnosed as of 2026-08-12 and fixed under Q809**; see [Mode B, attributed](#mode-b-attributed-2026-08-12-the-claim-was-made-and-lost) below.
Mode C (the recovery scan never observed the disruption) and mode D (the spec failing on its own sampler) are attributed below: see [What the 2026-08-26 and 2026-08-28 sightings show](#what-the-2026-08-26-and-2026-08-28-sightings-show).
Reset the [soak clock](../development/maintaining-backlog.md#retiring-a-flake-watch-row) to **2026-08-28**: count green runs from the mode-C/D mitigation.

## What the 2026-08-26 and 2026-08-28 sightings show

Three failures on `main` or in the merge queue in three days, after the 228-run soak this row had been parked on:

| When | Lane | Run | Failed at | Reaches |
|---|---|---|---|---|
| 2026-08-26 05:25 UTC | `e2e-calico` merge_group | [32933225396](https://github.com/actions-gateway/github-actions-gateway/actions/runs/32933225396) | `cmd/gmc/test/e2e/worker_scaleset_recovery_test.go:299` | the `Fail()` branch |
| 2026-08-26 05:41 UTC | `e2e-test` merge_group | [32934571793](https://github.com/actions-gateway/github-actions-gateway/actions/runs/32934571793) | `cmd/gmc/test/e2e/worker_scaleset_recovery_test.go:264` | the sampler assertion, before the claim window |
| 2026-08-28 15:35 UTC | `e2e-test` push to `main` | [33185281929](https://github.com/actions-gateway/github-actions-gateway/actions/runs/33185281929) | `cmd/gmc/test/e2e/worker_scaleset_recovery_test.go:299` | the `Fail()` branch |

**The two line-299 failures clear both discriminators**, which is what makes them a new mode rather than a recurrence of either.
Reaching `Fail()` requires `evictionRecoveryEvidenceLost()` to be false (so mode B's unwinnable half did not apply and the disruption record was still there to claim) and then `agcPodIdentity() == pinnedAGC` (so mode A's replaced control plane did not apply).
The 08-28 run confirms the second directly: one `ssrec-agc` pod, 114 s old, zero restarts, at the moment of the dump.
Neither run recorded a `Q549 re-staging` or a `Q809 re-staging` entry.

So the spec was saying exactly what its own message says it would: a chart role short a verb the recovery path needs, or a regressed deletion-mark discriminator.
**Both are refuted**, by the same AGC logs the escalation pointed at.

## Mode C, attributed (2026-08-28): the scan never saw the disruption

The AGC captured its own logs on every one of the three runs, over the whole window, at `debug` (the tenant sets `logLevel: debug`).
Counting the JSON log lines that name the probe pod separates the runs cleanly:

| Run | AGC JSON lines | Naming `ssrec-drain-probe-1` |
|---|---|---|
| [32933225396](https://github.com/actions-gateway/github-actions-gateway/actions/runs/32933225396) (line 299) | 230 | **0** |
| [33185281929](https://github.com/actions-gateway/github-actions-gateway/actions/runs/33185281929) (line 299) | 259 | **0** |
| [32934571793](https://github.com/actions-gateway/github-actions-gateway/actions/runs/32934571793) (line 264) | 275 | **2** |

The third run is the control that makes the two zeros mean something: same spec, same day, and its two lines are `worker pod disrupted; scheduling auto-retry` at 05:41:28 and `disruption auto-retry triggered … "rerunCalls":1` at 05:41:33.
The instrument can show both answers.

A zero rules out every claim verdict at once: `Forbidden` (the role), `Conflict`, `NotFound`, and identity-unknown all log with the pod's name.
The claim patch was never attempted, so **the chart role was never exercised** and the deletion-mark discriminator was never consulted.

**On the calico run the scan had no opportunity, and the reconcile timestamps say so.** `RecoverEvictedScaleSetWorkers` is step 2 of `Reconcile`, ahead of the listener bootstrap that produces this tenant's `Reconciler error` on every pass, so every logged error is a reconcile that ran the scan, and the scan reads the informer cache at the reconcile's start.

| Run | Delete requested | Pod gone | Reconcile errors around the window |
|---|---|---|---|
| 32933225396 | 05:24:24.95 | 05:24:28.37 | 05:24:21 ×3, **then 05:24:29 ×4** |
| 33185281929 | 15:33:34.62 | 15:33:41.33 | 15:33:34, **then 15:33:39 ×2, 15:33:40** |
| 32934571793 (recovered) | 05:41:27.58 | before 05:41:28.51 | 05:41:27 ×2, 05:41:28 ×2, 05:41:29 |

On the calico run the pod's entire 3.4-second existence-after-delete falls inside an 8-second gap with no reconcile at all.
The recovered run is the opposite: reconciles every second across the window.

**33185281929 is not that shape, and the difference is worth keeping.** Three reconciles completed inside its interval, so the gap does not explain its zero.
What is measured there is the zero verdict itself, which is what the mitigation keys on; the mechanism behind it is not.
Two candidates, neither verified: a `Reconciler error` stamps a reconcile's *completion* rather than its cache read, so the one logging at 15:33:39 may have listed before the terminal phase published; and the spec's 1-second poll bounds "gone" loosely, so the object may have been removed well before 15:33:41.33.
Do not read the calico run's mechanism onto this one.

**Why the gap opens is not measured.** Nothing logs a reconcile's duration.
The reconciler runs at controller-runtime's default `MaxConcurrentReconciles: 1` and never reconciles one key concurrently, and the listener bootstrap does a DNS lookup and an HTTP POST inside `Reconcile`, so a slow bootstrap would delay the next scan.
That is a hypothesis read off timestamps, not a reading.
The gap is [Q1029](../queue/Q1029.md).

### What mode C changes

An unrecovered pod looked identical whether a scan judged it and declined or no scan ever saw it: both are silence.
The scan now says which.
`externallyDeletedTerminalWorker` names the preconditions the two deletion-driven arms share (externally deleted, terminal, unclaimed), and a pod matching it that neither arm accepted is logged at `Debug` as `did not qualify as a recoverable disruption`, with the two times the ordering check turns on.
`Debug` because declining is usually right: a cleanup delete of an already-failed pod is exactly what the ordering exists to reject.

The spec gains the matching discriminator, `agcReachedNoDisruptionVerdict`, checked after the mode-A pin (a replaced AGC has no logs to read, so it must be excluded first).
No verdict means the attempt proves nothing about the role, so it re-stages under the same `maxAttempts` budget as modes A and B rather than failing.
Reaching `Fail()` now means the AGC judged the pod, which is what puts the two causes its message names back in scope.

**Residual:** a regression in the scan's own List selector also presents as silence, and would re-stage rather than fail.
The envtest pair covers the selector.

## Mode D, attributed (2026-08-28): the spec failed on its own diagnostics

The 08-26 line-264 failure is `Expect(seq).NotTo(BeEmpty(), "the sampler saw nothing; the pod was never observed")`, and that run is the one where **recovery worked**.

Its timeline, from the spec's own step stamps and the AGC log:

| Time (UTC) | Event |
|---|---|
| 05:41:22.685 | the spec stages the worker |
| 05:41:27.576 | the sampler starts, and the delete is issued, on the same stamp |
| 05:41:28 | the AGC claims the disruption: `worker pod disrupted; scheduling auto-retry`, `cause: deletion` |
| 05:41:28.512 | the pod is already gone; the spec moves to the sampler assertion and fails |
| 05:41:33 | the AGC's re-run lands: `disruption auto-retry triggered`, `rerunCalls: 1` |

The sampler shells out to `kubectl get` per sample and skips every empty or failed read, so a teardown that completes inside one invocation leaves the sequence empty.
That is a fact about the sampler racing a fast teardown, not about the AGC, and the spec's own comment above it already said the sequence is diagnostics rather than evidence.
The assertion is removed; the report entry stays, and says so when nothing was observed.

## The four modes

The first three are all "this attempt never exercised the chart role", and the spec re-stages on each rather than failing.
They are checked in this order, because each later one depends on reading something an earlier one has invalidated.

| Mode | Discriminator in the spec | State |
|---|---|---|
| **B**: the pod was gone before the claim could land | `evictionRecoveryEvidenceLost()`: the AGC logged `disruption was lost before it could be claimed` for this pod. Records a `Q809 re-staging` entry | **Diagnosed 2026-08-12**: the disruption was detected and the *claim* failed. Fixed under Q809; the spec re-stages on the unwinnable half |
| **A**: the AGC control plane was replaced inside the claim window | `agcPodIdentity() != pinnedAGC` at the wait's expiry. Records a `Q549 re-staging` entry | Diagnosed on run [30658951388](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30658951388), mitigated by the UID pin + re-stage (PR #1120). Worked case in [testing.md § Pin the process when the signal comes out of its memory](../development/testing.md#pin-the-process-when-the-signal-comes-out-of-its-memory) |
| **C**: the recovery scan never observed the disruption | `agcReachedNoDisruptionVerdict()`: no AGC line names the pod alongside `disrupt`. Records a `Q549 re-staging (no verdict)` entry. Must be checked **after** A, since a replaced AGC has no logs to read | **Diagnosed 2026-08-28**, [below](#mode-c-attributed-2026-08-28-the-scan-never-saw-the-disruption). The product gap underneath is [Q1029](../queue/Q1029.md) |
| **D**: the spec failed on its own sampler | none: the assertion is removed | **Diagnosed 2026-08-28**, [below](#mode-d-attributed-2026-08-28-the-spec-failed-on-its-own-diagnostics). The run it reddened had already been recovered |

What is left when all three re-stage arms decline is the defect the spec exists to catch: the AGC judged this pod, under the pinned control plane, and no re-run fired.
Its failure text names two causes, a chart role missing a verb or a regressed deletion-mark discriminator, and each now has its own AGC log line, so the next `Fail()` is attributable from the log alone.

## Mode B: what run 30724186342 shows

Run [30724186342](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30724186342) attempt 1, job `e2e / e2e`, branch `claude/q547-537615` (PR #1140, an unrelated change — v2 gateway-teardown worker reap), 2026-08-02 00:01–00:03 UTC.
Attempt 2 passed with no code change.

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

- **The AGC was not replaced.** The pin held, there is no `Q549 re-staging` entry, and the AGC pod (`ssrec-agc-b8d7d6678-vkmmb`) has no previous container (`previous terminated container "agc" … not found`) — it never restarted either.
- **The claim annotation was never observed on the pod.** The field sampler recorded `Running/2026-08-02T00:01:53Z/ -> Failed/2026-08-02T00:01:53Z/`, with the `actions-gateway.com/eviction-handled-at` field empty in both states.
  In the passing attempt 2 the same sampler recorded `… -> Failed/2026-08-02T00:17:58Z/2026-08-02T00:17:29Z` — claim present.
  That contrast is the sharpest signal the two logs carry.
  The sampler is diagnostics, not evidence: it bounds what was *observed* in ~2.2 s, never what happened.
- **No `EvictionRerunFailed` event** in the namespace events dump.
  `rerunUntilAccepted` records that on every terminal failure, so no re-run reached a terminal refusal — the same discriminator that ruled out "attempted and refused" for mode A.
- **The AGC logged nothing after 00:01:24Z**, through the dump taken at 00:02:57Z — at debug verbosity, and with no truncation (93 lines against `kubectl logs --tail=2000`).
  The re-run wait ran entirely inside that silence.

Not evidence, and worth stating so it isn't mistaken for some later: attempt 2's log contains no `ScaleSetListenerStartFailed` at all, but it also contains no diagnostics dump — that only runs on failure.
The two attempts are **not comparable** on any signal that comes from the dump.

## Mode B again: what run 30864634442 shows

Run [30864634442](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30864634442) attempt 1, job **`e2e-calico / e2e`**, branch `claude/happy-lederberg-c8a92c` (PR #1217, an unrelated change — the Q648 egress-probe scoring), 2026-08-04 00:14–00:17 UTC.
Attempt 2 passed with no code change.

**This is the first mode-B sighting on the Calico leg**; 2026-08-01 was `e2e / e2e` (kindnet).
The mode does not belong to one CNI.

Mode B on both discriminators: the pin is unchanged (the spec's failure text takes the "and the AGC that observed the disruption is still running" branch) and the log contains zero `Q549 re-staging` entries.

| Time (UTC) | Event |
|---|---|
| 00:14:50–00:15:16 | the AGC's RunnerSet reconcile errors repeatedly on `scalesetlistener: ensure scale set "e2e-ssrec": scaleset: registration token: … lookup ghes.invalid … no such host` (14 `Reconciler error` entries) |
| 00:14:55.2 | the AGC control plane settles and is pinned |
| 00:14:58.2 | the spec stages the running worker |
| 00:15:08.2 | the sampler starts; the worker is deleted gracefully (`deletionTimestamp` 00:15:38Z — delete time plus the 30 s grace) |
| 00:15:16 | the AGC's last log line of any kind |
| 00:15:20.2 | the 90 s re-run wait starts |
| 00:16:52.6 | the wait expires; the pin is unchanged, so the spec fails |

Measured:

- **The claim annotation was never observed**, the same contrast the 2026-08-01 run showed: the sampler recorded `Running/2026-08-04T00:15:38Z/ -> Failed/2026-08-04T00:15:38Z/`, with `actions-gateway.com/eviction-handled-at` empty in both states.
- **The observation window was ~11.3 s** (00:15:08.98 → 00:15:20.24), against ~2.2 s on 2026-08-01 — about 5× wider, and the claim still never appeared.
  This weakens "the pod vanished before the claim could land" as the whole story, without ruling it out.
- **The AGC logged nothing after 00:15:16Z**, through the failure at 00:16:52.6.
  As on 2026-08-01, the entire re-run wait ran inside that silence.
- **The RunnerSet reconcile was erroring across the disruption window**, not converged — [capture item 4](#what-to-capture-on-the-next-occurrence) below.

**The listener error is by design, and is not a signal.** The open question below guessed it "may well be routine in this e2e"; it is.
`scaleSetRecoveryManifest` sets `githubURL: https://ghes.invalid/ssrecorg` ([worker_scaleset_recovery_test.go](../../cmd/gmc/test/e2e/worker_scaleset_recovery_test.go)), deliberately choosing an unresolvable host so the bootstrap fails on NXDOMAIN — that failure is the precondition the spec needs.
So a reconcile erroring on the scale-set listener is the normal state of this tenant and cannot distinguish a failing run from a passing one.
What remains open is whether the recovery scan is *reachable* while that is happening.

## Mode B, attributed (2026-08-12): the claim was made, and lost

Three `e2e-calico` failures in one three-hour window, all mode B, all this spec, all on attempt 1:

| Run | Trigger | Claim error in the AGC log |
|---|---|---|
| [31555321326](https://github.com/actions-gateway/github-actions-gateway/actions/runs/31555321326) | push `main`, 01:57 | `pods "ssrec-drain-probe-1" not found` |
| [31556806760](https://github.com/actions-gateway/github-actions-gateway/actions/runs/31556806760) | merge queue, pr-1405, 02:25 | `Operation cannot be fulfilled on pods "…": the object has been modified` |
| [31564438316](https://github.com/actions-gateway/github-actions-gateway/actions/runs/31564438316) | merge queue, pr-1415, 04:48 | `pods "ssrec-drain-probe-1" not found` |

Each run reported `Summarizing 1 Failure`: this spec and nothing else.

**These same three runs had already been filed as a calico NetworkPolicy flake, and that reading is refuted here.** Q809 was opened on 2026-08-11 asserting that the five calico-gated enforcement negatives intermittently see traffic the policy should drop; none of them failed in any of the three runs, and the row's remaining half (the enforcer dump reading `app=kindnet` only, so the Calico lane captured nothing) was real and shipped in #1417.
The refuted row is in [the retired-flake ledger](../development/flake-watch-retired.md) rather than the backlog, because #1441 reused its ID for the defect below instead of closing it ([why that is now a rule](../development/maintaining-backlog.md#repurposing-an-id-is-a-closure-with-every-step-skipped)).
Each carried exactly one AGC line about the probe pod, at `Debug`:

```
"msg":"scale-set worker disruption already claimed elsewhere; skipping","cause":"deletion", …
```

**That message was wrong in all three cases, and it is why the mode stayed undiagnosed for eleven days.** The [capture list](#what-to-capture-on-the-next-occurrence) below asked for `could not claim scale-set worker disruption`, the `Warn` line for an unrecognised error.
The `Debug` line above is what a `Conflict` or a `NotFound` actually produced, and it asserts a claimant that never existed.

What the three runs establish:

- **Detection worked.** `disruptionAwaitingRecovery` returned `cause: deletion` every time, so the Q502 discriminator was not regressed.
- **The role was not short a verb.** A refused claim is a `Forbidden`, and none of the three was.
- **Nobody else had claimed the pod.** The spec's own sampler recorded `actions-gateway.com/eviction-handled-at` empty throughout, the same contrast the 2026-08-01 and 2026-08-04 runs showed.
- **The claim raced the pod's own teardown.** Two runs found the object already gone; the delete was issued at ≈02:07:40 and the pod was unreadable by 02:07:42, well inside its 30 s grace period, because the probe container traps `TERM` and exits at once.
  The recovery scan lists from the informer cache and patches through the live client, so that gap is exactly where the claim falls.
- **The third was a conflict from a writer that was not a claimant.** The kubelet publishing the terminal phase is guaranteed to be racing here, because that transition is the edge that triggers the reconcile.
  The optimistic lock cannot tell it from a rival replica.

This also settles what the 2026-08-04 window could not: an 11.3 s observation window with no claim "weakened the pod vanished before the claim could land" only because both flavours were being read as one.
The conflict flavour needs no vanished pod.

**Lane asymmetry, measured over the same window** (`merge_group` and `push` runs only; the `pull_request` entries are gate no-ops, Q675): `e2e-test.yml` 0 failures in 87, `e2e-calico.yml` 3 in 80.
The spec carries no CNI gate and runs on both, and the 2026-08-01 sighting was on kindnet, so the mechanism is not calico's.
Calico's heavier per-node load is a plausible amplifier of the race and is **not** measured.

### The fix

**AGC** ([eviction_scaleset.go](../../cmd/agc/internal/provisioner/eviction_scaleset.go)): the claim settles its own verdict instead of handing back a raw error:

1. A `Conflict` is retried against a re-read pod, bounded, and only while the fresh object shows the claim still unstamped.
   A fresh object that *does* carry it is the genuine rival the optimistic lock exists to lose to, and is still skipped at `Debug`.
2. A `NotFound` is reported as a lost recovery: `Warn`, an `EvictionRecoveryEvidenceLost` Event, and `actions_gateway_eviction_recovery_evidence_lost_total`.

At-most-once is deliberately **not** relaxed.
Recovering from the in-memory pod copy after the object is gone would let two AGC replicas each spend a slot of one run's retry budget for a single disruption, which is the regression the claim exists to prevent.
A drain whose evidence outruns the claim stays unrecovered, but stops being silent.
Design boundary: [04-operational-flows.md § Detecting a disruption is not the same as claiming it](../design/04-operational-flows.md#detecting-a-disruption-is-not-the-same-as-claiming-it).

**Spec**: a lost claim means the attempt never exercised the chart role, exactly like mode A's replaced control plane, so it re-stages under the same `maxAttempts` budget rather than failing.
It reads the verdict from the AGC log, because the pod is gone either way and an unclaimed pod looks identical whether the claim was refused or never got to be made.

Deliberately not done: a finalizer on the probe pod to widen the window.
The real teardown window is what distinguishes this spec from its envtest twin, which simulates it with a finalizer; adding one here would delete the spec's reason to exist.

## Open questions

1. ~~**Is the recovery scan reachable while the RunnerSet reconcile is failing on the scale-set listener?**~~ **Settled 2026-08-28: yes.** `RecoverEvictedScaleSetWorkers` is step 2 of `Reconcile`, ahead of the protocol routing that reaches `reconcileScaleSetListener`, so every reconcile that logs the listener error ran the scan first.
   What is *not* guaranteed is that a reconcile begins inside the pod's teardown window: [mode C](#mode-c-attributed-2026-08-28-the-scan-never-saw-the-disruption).
   The listener error itself is settled too: it is deliberate (`ghes.invalid`, see the 2026-08-04 section), so it is normal and discriminates nothing.
2. **Is the AGC's silence after the disruption the reconcile loop going quiet, or something broader?** Unknown from the 2026-08-01 dump alone.
   The 2026-08-12 runs were not silent, so this may have been an artifact of that one run's log capture rather than a property of the mode.

## What to capture on the next occurrence

- Whether the failure is mode A or mode B — read the pin and the presence of a `Q549 re-staging` entry first, before anything else.
- Whether it is the mode B **above**: grep the AGC log for `disruption was lost before it could be claimed` and for `already claimed elsewhere`, and note that a `Q809 re-staging` entry means the spec classified it and kept going.
- **Count the AGC log lines that name the probe pod, first.** Zero is mode C and the spec re-stages on it; a `did not qualify as a recoverable disruption` line is the discriminator regression; a `could not claim` line is the role.
  A failure with none of those and no zero is a genuinely new shape.
- The AGC log **for the full wait window**, not just up to the failure dump.
- The claim annotation's fate: the sampler sequence, plus which claim line the AGC emitted.
  All three of `Forbidden` (the role), `Conflict`, and `NotFound` now say so distinctly.
- The RunnerSet reconcile's state across the window — erroring, or converged.
