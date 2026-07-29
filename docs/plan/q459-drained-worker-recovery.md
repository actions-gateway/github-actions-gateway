# Q459: Close or Accept the Drained-Worker Recovery Gap

Q421 measured that a graceful worker-pod removal — a `kubectl drain`, or a bare
`kubectl delete pod` — reaches no eviction recovery on either acquisition tier, so
a *graceful* disruption recovers strictly worse than an *ungraceful* one. The
result and the reasoning are in
[eviction-oversubscription-validation.md § Result](eviction-oversubscription-validation.md#result-measured-2026-07-27).

This plan carries the decision Q421 deliberately did not make: close the gap, or
accept it.

## Why the decision was deferred rather than taken

Extending both tiers to treat a graceful deletion as recoverable is a small code
change. What made it unsafe to write in Q421 was that its premise was unmeasured.
Two things had to be known first:

1. **Does the relayed report leave the run re-runnable at all?** The AGC recovers a
   run by `POST /repos/{owner}/{repo}/actions/runs/{run_id}/rerun-failed-jobs`
   ([`eviction.go`](../../cmd/agc/internal/provisioner/eviction.go)). If the runner's
   own SIGTERM-relayed report puts the run in a state that endpoint declines, there
   is no gap to close on this path — only a different endpoint, or nothing.
2. **Can the AGC tell this disruption from a deliberate one?** A run cancelled by a
   human is the case an automatic re-run must never fight. Q421 recorded the concern
   as "deliberate cancels share the path"; whether they actually do is a claim about
   the pod's observed shape, and it was never checked.

Both are Tier C questions: they need a real runner executing a real job, reported to
real GitHub. Neither the envtest pair nor the Tier B drain spec can ask them —
Tier B's drained worker is deliberately held `Pending`, so there is no live container
to signal and no report to follow.

## The measurement

**Venue.** Tier C on the local kind cluster, the same footing as
`E2E_GitHub_RealDispatch`: the GMC's fakegithub overrides are swapped for the real
org URL, the tenant carries the live `actions-gateway-test` App credential, and the
workflow is dispatched against `actions-gateway/gateway-test`.

**Why a `kubectl delete pod` rather than a `kubectl drain`.** The drain-versus-delete
distinction is already settled: Q421 measured at Tier B that an admitted eviction *is*
a graceful delete and publishes no `Failed`/`Evicted` shape. What is open here is
everything downstream of the delete — the relay, the report, and what GitHub does with
it — and a bare delete reaches that identically while removing the cordon, the
node-scoping, and the `--force` from a measurement none of them are about. The spec
asserts the pod's `deletionTimestamp`/grace period so the deletion it performs is on
the record as the graceful kind.

**Steps.**

1. Dispatch a workflow whose job runs long enough to be interrupted mid-step, and wait
   until GitHub reports that job `in_progress` — not merely that a worker pod exists.
   A pod that has not yet reached the runner's job loop has nothing to report, which
   would make the measurement about startup rather than about the relay.
2. Delete the worker pod with the default grace period.
3. Record, in order:
   - **The relay.** The wrapper's `forwarding termination signal` line, and whether
     the child outlived the grace budget.
   - **The pod.** Its phase/reason across the deletion window, and its container exit
     code — this is what decides whether the AGC's waiter sees a terminal phase or a
     vanished object, and therefore whether a discriminator is even available.
   - **GitHub.** Time from deletion to the job leaving `in_progress`, and the
     `conclusion` it lands on.
   - **Re-runnability.** Whether `rerun-failed-jobs` is accepted for that run, and
     whether the attempt it creates actually runs to completion on the gateway.

Step 3's last item is the load-bearing one. It is the exact call the AGC would make
if the gap were closed, so its answer is the answer.

## The decision this produces

| Measured outcome | Decision |
|---|---|
| The report leaves the run re-runnable, **and** a deliberate cancel is distinguishable at the pod | **Close.** Extend both tiers' detection to the graceful-deletion shape, gated on the discriminator. |
| The report leaves the run re-runnable, but a deliberate cancel is **not** distinguishable | **Close behind an opt-in**, defaulting off — an automatic re-run that fights a human cancel is worse than the gap. |
| `rerun-failed-jobs` declines the run | **Accept.** No re-run is available on this path; the operator-facing docs already say so and the design records why. |

Whichever branch is taken, the operator-facing consequence is already documented in
[troubleshooting.md](../operations/troubleshooting.md#draining-a-node-does-not-auto-re-run-the-jobs-it-interrupts)
and must be brought into line with the outcome.

## Result: the graceful-deletion path, measured 2026-07-28

Tier C on kind, against `actions-gateway/gateway-test`
([run 30410156445](https://github.com/actions-gateway/gateway-test/actions/runs/30410156445)),
by `E2E_GitHub_GracefullyDeletedWorkerReportsAndIsRerunnable`. A real runner was
executing a real job — GitHub reported it `in_progress` before anything was touched —
and the worker pod was deleted with the default grace period.

| Observation | Value |
|---|---|
| `deletionGracePeriodSeconds` / `deletionTimestamp` | `30` / set — the deletion was the graceful kind |
| Worker pod phase/reason across the window | `Running/` → `Failed/` |
| Wrapper relay | `forwarding termination signal to child`, `grace: 25s`; runner logs `Runner will be shutdown for UserCancelled` |
| Job conclusion on GitHub | **`failure`** |
| Deletion → conclusion | **15s** |
| `POST .../rerun-failed-jobs` | **`201 Created`** |
| Second attempt | created, and reached a gateway runner |

**Question 1 is answered, and the answer is yes.** The relay gets the runner's own
report out well inside the grace period, GitHub concludes the job promptly rather than
waiting out the lock, and the run is fully re-runnable by the exact call
`handleEviction` already makes. The premise Q417 shipped on holds. Closing the gap is
mechanically available.

**But the measurement also found the hazard that decides how.** Q421 predicted, from
its Tier B run, that a disrupted worker is *deleted without ever publishing a terminal
phase* — and at Tier B that was true, because the pod was deliberately held `Pending`.
A **running** worker behaves differently: the kubelet's terminal-phase update wins the
race against the object's removal, so the pod lands in `PodFailed` with an **empty
reason** and the informer's `onPodEvent` resolves the waiter before `onPodDelete` ever
fires.

`PodFailed` with an empty reason is also exactly what a worker whose job *genuinely
failed* lands in. So the shape alone cannot carry recovery: keying re-runs off
`PodFailed`-with-empty-reason would re-run every legitimately failing job in the
cluster, which is far worse than the gap being closed. Whatever closes this must key
off something else.

**The candidate discriminator is `metadata.deletionTimestamp`** — set on a worker taken
away by a drain or a delete at the moment its terminal phase publishes, absent on one
whose job ended by itself. The first half is measured above. The second half is
question 2, and it is what
`E2E_GitHub_CancelledRunLeavesNoDeletionMark` measures.

Worth noting for the eventual implementation: the AGC already *has* this information
and discards it. `InformerPodWaiter.onPodDelete` resolves waiters with
`phase: PodSucceeded` and an empty reason
([`podwaiter.go`](../../cmd/agc/internal/provisioner/podwaiter.go)), identical to a
genuine success, matching the older poll loop's "deleted externally → treat as
completion". Closing the gap needs no new watch, annotation or pod bookkeeping — it
needs the waiter to stop flattening a distinction it can already see.

## Findings so far

**`rerun-failed-jobs` has a one-month retention window** — measured 2026-07-28, while
attempting to answer the re-runnability question cheaply against an already-cancelled
historical run rather than a freshly interrupted one:

```
POST /repos/actions-gateway/gateway-test/actions/runs/27386814795/rerun-failed-jobs
HTTP/2.0 403 Forbidden
{"message":"Unable to retry this workflow run because it was created over a month ago"}
```

Two things follow. The shortcut does not work — every cancelled run in the fixture
repo predates the window, and the 403 is about the run's *age*, not about its
`cancelled` conclusion, so it says nothing about the question Q459 asks. And the
constraint is real for the design: whatever the AGC recovers, it recovers only inside
that window. That is comfortably wide for eviction recovery, which fires seconds after
the disruption, but it does bound any future backstop that retries from a persisted
record after a long AGC outage — the 12h `maxWorkerLifetime` cap sits inside it, a
restart-time sweep of much older runs would not.

## Question 2 is written but not yet measured

`E2E_GitHub_CancelledRunLeavesNoDeletionMark` (in
[`github_e2e_test.go`](../../cmd/gmc/test/e2e/github_e2e_test.go)) cancels a real run
from GitHub and asserts the worker publishes a terminal phase carrying no
`deletionTimestamp` — the other half of the discriminator. It has **not produced a
result**: three attempts to run it were blocked before reaching it, by
[Q444](../STATUS.md#Q444).

The e2e cluster's kube-apiserver entered the Q444 broken state between the run that
produced the result above and the next one, and every run since fails in `BeforeAll`
with the RunnerGroup create denied:

```
ValidatingAdmissionPolicy 'gmc-priorityclass-allowlist-guard' … denied request:
failed to configure binding: no params found for policy binding with `Deny` parameterNotFoundAction
```

with `gmc-priorityclass-allowlist` present in `gmc-system` and the binding pointing
exactly at it — the shape
[q444-vap-param-resolution.md](q444-vap-param-resolution.md) § Established by
measurement records as findings 1 and 2. Recovery is a genuine kube-apiserver restart
(`crictl stop` on the container; a `kubectl delete pod` of the static-pod mirror does
**not** restart it). Nothing here adds to Q444 — the observation matches what that
plan already establishes, including that the uninstall/reinstall cycle is its first
casualty rather than its trigger.

**What that costs Q459:** the decision cannot be taken yet. The hazard is measured and
the discriminator's first half is measured; the second half needs one run of a spec
that is already written, on a cluster whose apiserver has been restarted.

## Status

**In progress.** Question 1 is answered and recorded above. Question 2 is specified,
implemented as a spec, and unrun. No close-or-accept decision has been taken, and no
production code has been changed.

Next step, in order:

1. Restart the e2e cluster's kube-apiserver and run
   `E2E_GitHub_CancelledRunLeavesNoDeletionMark`.
2. If a cancelled run's worker publishes a terminal phase with no `deletionTimestamp`,
   take the decision table's first row — close, gated on the deletion mark — and note
   that the residual ambiguity is an operator's bare `kubectl delete pod`, which is
   indistinguishable from a drain and arguably should re-run anyway.
3. If it does not, drop to the second row: close behind a default-off opt-in.
