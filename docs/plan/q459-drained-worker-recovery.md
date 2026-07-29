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

Reproduced on four further runs (2026-07-28): conclusion `failure` every time, `201`
every time, second attempt reaching a runner every time. The *shape* is stable; the
latency varies more than one observation suggested — 15s, 16s, 17s, 26s across five
runs. Quote it as "well under a minute", not as a point estimate: the spread is
GitHub's own conclusion latency, which this experiment does not control for and which
a published figure should not imply it does.

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

## The measurement found something it was not looking for (Q495)

Worker pods provisioned for **real** GitHub jobs on the classic tier carry **no**
`actions-gateway.com/run-id` annotation. Observed 2026-07-28 while scoping the spec's
worker lookup to its own run; the jsonpath was verified against a control pod carrying
a known annotation, so the empty value is the pod's, not the query's.

That matters well beyond this plan.
[`repoInfo()`](../../cmd/agc/internal/provisioner/payload.go) — which supplies the
`runID` that eviction recovery re-runs — and `jobMetaFrom()` — which supplies the
annotation — read the *same two sources*: `Variables["system.github.run_id"]`, falling
back to `ap.RunID`. An absent annotation therefore means both were empty, so `runID` is
`"0"`, and [`handleEviction`](../../cmd/agc/internal/provisioner/eviction.go) opens with
`if runID == "0" || runID == ""` → log and return.

**Inference, not yet a direct observation:** classic-tier eviction recovery cannot name
the run to re-run against real GitHub, so it cannot fire. Every test that exercises it
uses a fakegithub payload carrying the identity explicitly — Q421's own Tier B drain
spec had to *inject* it, recording that "the default fakegithub response carries no run
identity, and handleEviction returns early without one". The fake was adjusted to make
the test pass; the real payload was never checked. Confirming it needs one run that
evicts a real job and looks for the skip. [Q495](../STATUS.md#Q495) carries that.

It also bears on this plan's decision: closing the drained-worker gap by calling
`rerun-failed-jobs` buys nothing on classic if the run ID is unavailable in the first
place. Q495 is therefore a prerequisite for the "close" branch, not a side finding.

## Question 2 is written but not yet measured

`E2E_GitHub_CancelledRunLeavesNoDeletionMark` (in
[`github_e2e_test.go`](../../cmd/gmc/test/e2e/github_e2e_test.go)) cancels a real run
from GitHub and asserts the worker publishes a terminal phase carrying no
`deletionTimestamp` — the other half of the discriminator. **It is checked in as
`PIt` — pending — because it has never produced a result**, and shipping it active
would redden any credentialed Tier C run on a known-incomplete spec.

Two things stand between it and a result, both understood:

- **Worker contention.** The spec before it triggers a re-run whose worker holds the
  tenant for the fixture's full ten-minute sleep, so "which worker is mine" is
  ambiguous. Cancelling that run does not promptly free the worker either — measured,
  a five-minute wait for the pod to clear times out.
- **No exact disambiguator.** The run-id annotation that would resolve it outright is
  the one Q495 found missing. Fixing Q495 makes the worker lookup exact and dissolves
  the contention, which is why it is the natural prerequisite rather than more
  scaffolding here.

Earlier attempts were blocked before even reaching it, by the
PriorityClass VAP param-resolution failure that
[q444-vap-param-resolution.md](q444-vap-param-resolution.md) investigates and
[Q492](../STATUS.md#Q492) now carries.

The e2e cluster's kube-apiserver entered that broken state between the run that
produced the result above and the next one, and every run since fails in `BeforeAll`
with the RunnerGroup create denied:

```
ValidatingAdmissionPolicy 'gmc-priorityclass-allowlist-guard' … denied request:
failed to configure binding: no params found for policy binding with `Deny` parameterNotFoundAction
```

with `gmc-priorityclass-allowlist` present in `gmc-system` and the binding pointing
exactly at it — the shape
[q444-vap-param-resolution.md](q444-vap-param-resolution.md) § Established by
measurement records as findings 1 and 2. Nothing here adds to that investigation: it
has since established the trigger — deleting the `ValidatingAdmissionPolicyBinding`
empties the paramKind's binding set and the apiserver never restarts the shared
informer — which is consistent with what happened here, since the suite's
`helm uninstall` teardown deletes exactly that object.

Recovery was confirmed on 2026-07-28: `crictl stop` on the kube-apiserver container
cleared it, verified by the container ID changing and its `ATTEMPT` going 0 → 1. A
`kubectl delete pod` of the static-pod *mirror* does **not** restart it and must not
be mistaken for a restart — that plan records a conclusion drawn from exactly that
non-restart and later withdrawn.

**What that costs Q459:** the decision cannot be taken yet. The hazard is measured and
the discriminator's first half is measured; the second half needs one run of a spec
that is already written, on a cluster whose apiserver has been restarted.

## Status

**In progress.** Question 1 is answered and recorded above. Question 2 is specified,
implemented as a spec, and unrun. No close-or-accept decision has been taken, and no
production code has been changed.

Next step, in order:

1. **[Q495](../STATUS.md#Q495) first.** It is both the more serious defect and the
   unblocker here: restoring the run identity makes the worker lookup exact, which
   removes the contention that keeps question 2 pending.
2. Un-pend `E2E_GitHub_CancelledRunLeavesNoDeletionMark` and run it.
3. If a cancelled run's worker publishes a terminal phase with no `deletionTimestamp`,
   take the decision table's first row — close, gated on the deletion mark — and note
   that the residual ambiguity is an operator's bare `kubectl delete pod`, which is
   indistinguishable from a drain and arguably should re-run anyway.
4. If it does not, drop to the second row: close behind a default-off opt-in.

Operational note for whoever runs Tier C next, learned the expensive way: the suite
teardown's `helm uninstall` deletes the `ValidatingAdmissionPolicyBinding`, which is
exactly the trigger [Q492](../STATUS.md#Q492) documents — so each run poisons the next
one's apiserver. Restart the kube-apiserver (`crictl stop`, verified by `ATTEMPT`
incrementing) *before* a run, not after a failure. Running with
`E2E_SKIP_TEARDOWN=true` avoids the uninstall, but then a subsequent `helm upgrade`
conflicts on server-side-apply field ownership (`kubectl-patch`/`kubectl-set` claim
fields helm owns); deleting just the `gmc-controller-manager` Deployment beforehand
clears that without touching the binding.
