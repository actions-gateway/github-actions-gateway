# Q459: Close or Accept the Drained-Worker Recovery Gap

Q421 measured that a graceful worker-pod removal — a `kubectl drain`, or a bare `kubectl delete pod` — reaches no eviction recovery on either acquisition tier, so a *graceful* disruption recovers strictly worse than an *ungraceful* one.
The result and the reasoning are in [eviction-oversubscription-validation.md § Result](../eviction-oversubscription-validation.md#result-measured-2026-07-27).

This plan carries the decision Q421 deliberately did not make: close the gap, or accept it.

## Why the decision was deferred rather than taken

Extending both tiers to treat a graceful deletion as recoverable is a small code change.
What made it unsafe to write in Q421 was that its premise was unmeasured.
Two things had to be known first:

1. **Does the relayed report leave the run re-runnable at all?** The AGC recovers a run by `POST /repos/{owner}/{repo}/actions/runs/{run_id}/rerun-failed-jobs` ([`eviction.go`](../../../cmd/agc/internal/provisioner/eviction.go)).
   If the runner's own SIGTERM-relayed report puts the run in a state that endpoint declines, there is no gap to close on this path — only a different endpoint, or nothing.
2. **Can the AGC tell this disruption from a deliberate one?** A run cancelled by a human is the case an automatic re-run must never fight.
   Q421 recorded the concern as "deliberate cancels share the path"; whether they actually do is a claim about the pod's observed shape, and it was never checked.

Both are live-GitHub questions: they need a real runner executing a real job, reported to real GitHub.
Neither the envtest pair nor the fake-GitHub drain spec can ask them — the fake-GitHub tier's drained worker is deliberately held `Pending`, so there is no live container to signal and no report to follow.

## The measurement

**Venue.** live-GitHub on the local kind cluster, the same footing as `E2E_GitHub_RealDispatch`: the GMC's fakegithub overrides are swapped for the real org URL, the tenant carries the live `actions-gateway-test` App credential, and the workflow is dispatched against `actions-gateway/gateway-test`.

**Why a `kubectl delete pod` rather than a `kubectl drain`.** The drain-versus-delete distinction is already settled: Q421 measured at fake-GitHub that an admitted eviction *is* a graceful delete and publishes no `Failed`/`Evicted` shape.
What is open here is everything downstream of the delete — the relay, the report, and what GitHub does with it — and a bare delete reaches that identically while removing the cordon, the node-scoping, and the `--force` from a measurement none of them are about.
The spec asserts the pod's `deletionTimestamp`/grace period so the deletion it performs is on the record as the graceful kind.

**Steps.**

1. Dispatch a workflow whose job runs long enough to be interrupted mid-step, and wait until GitHub reports that job `in_progress` — not merely that a worker pod exists.
   A pod that has not yet reached the runner's job loop has nothing to report, which would make the measurement about startup rather than about the relay.
2. Delete the worker pod with the default grace period.
3. Record, in order:
   - **The relay.** The wrapper's `forwarding termination signal` line, and whether the child outlived the grace budget.
   - **The pod.** Its phase/reason across the deletion window, and its container exit code — this is what decides whether the AGC's waiter sees a terminal phase or a vanished object, and therefore whether a discriminator is even available.
   - **GitHub.** Time from deletion to the job leaving `in_progress`, and the `conclusion` it lands on.
   - **Re-runnability.** Whether `rerun-failed-jobs` is accepted for that run, and whether the attempt it creates actually runs to completion on the gateway.

Step 3's last item is the load-bearing one: it is the call the AGC would make if the gap were closed, so GitHub's answer to it decides whether there is anything to close.

**What it does not establish** — recorded here because the original wording of this line ("the exact call the AGC would make, so its answer is the answer") was later shown to overreach.
The harness issues this call with `gh api`, after waiting for GitHub to conclude the job.
The AGC issues it from `rerunFailedJobs`, against whatever host `Provisioner.GitHubAPIURL` names, `evictionRetryDelay` after the disruption.
Both of those differences turned out to hide defects the `201` here could not see — Q504 (the call ignored `GITHUB_API_BASE_URL`) and Q503 (it fired ~9.5 minutes too early and was refused `403`; both since fixed).
Read this measurement as *GitHub will re-run a run in this state*, and nothing more.

## The decision this produces

| Measured outcome | Decision |
|---|---|
| The report leaves the run re-runnable, **and** a deliberate cancel is distinguishable at the pod | **Close.** Extend both tiers' detection to the graceful-deletion shape, gated on the discriminator. |
| The report leaves the run re-runnable, but a deliberate cancel is **not** distinguishable | **Close behind an opt-in**, defaulting off — an automatic re-run that fights a human cancel is worse than the gap. |
| `rerun-failed-jobs` declines the run | **Accept.** No re-run is available on this path; the operator-facing docs already say so and the design records why. |

Whichever branch is taken, the operator-facing consequence is already documented in [troubleshooting.md](../../operations/troubleshooting.md#draining-a-worker-auto-re-runs-the-jobs-it-interrupts) (retitled when Q502 shipped the close) and must be brought into line with the outcome.

## Result: the graceful-deletion path, measured 2026-07-28

Live-GitHub tier on kind, against `actions-gateway/gateway-test` ([run 30410156445](https://github.com/actions-gateway/gateway-test/actions/runs/30410156445)), by `E2E_GitHub_GracefullyDeletedWorkerReportsAndIsRerunnable`.
A real runner was executing a real job — GitHub reported it `in_progress` before anything was touched — and the worker pod was deleted with the default grace period.

| Observation | Value |
|---|---|
| `deletionGracePeriodSeconds` / `deletionTimestamp` | `30` / set — the deletion was the graceful kind |
| Worker pod phase/reason across the window | `Running/` → `Failed/` |
| Wrapper relay | `forwarding termination signal to child`, `grace: 25s`; runner logs `Runner will be shutdown for UserCancelled` |
| Job conclusion on GitHub | **`failure`** |
| Deletion → conclusion | **15s** |
| `POST .../rerun-failed-jobs` | **`201 Created`** |
| Second attempt | created, and reached a gateway runner |

Reproduced on four further runs (2026-07-28): conclusion `failure` every time, `201` every time, second attempt reaching a runner every time.
The *shape* is stable; the latency varies more than one observation suggested — 15s, 16s, 17s, 26s across five runs.
Quote it as "well under a minute", not as a point estimate: the spread is GitHub's own conclusion latency, which this experiment does not control for and which a published figure should not imply it does.

**Question 1 is answered, and the answer is yes.** The relay gets the runner's own report out well inside the grace period, GitHub concludes the job promptly rather than waiting out the lock, and the run is re-runnable by the endpoint `handleEviction` calls.
The premise Q417 shipped on holds, and closing the gap is mechanically available — at the endpoint.
Whether the AGC's own call reaches it correctly and at the right moment is a separate question this measurement did not ask; Q504 and Q503 are the answers, and both were defects.

**But the measurement also found the hazard that decides how.** Q421 predicted, from its fake-GitHub run, that a disrupted worker is *deleted without ever publishing a terminal phase* — and at fake-GitHub that was true, because the pod was deliberately held `Pending`.
A **running** worker behaves differently: the kubelet's terminal-phase update wins the race against the object's removal, so the pod lands in `PodFailed` with an **empty reason** and the informer's `onPodEvent` resolves the waiter before `onPodDelete` ever fires.

`PodFailed` with an empty reason is also exactly what a worker whose job *genuinely failed* lands in.
So the shape alone cannot carry recovery: keying re-runs off `PodFailed`-with-empty-reason would re-run every legitimately failing job in the cluster, which is far worse than the gap being closed.
Whatever closes this must key off something else.

**The candidate discriminator is `metadata.deletionTimestamp`** — set on a worker taken away by a drain or a delete at the moment its terminal phase publishes, absent on one whose job ended by itself.
The first half is measured above.
The second half is question 2, and it is what `E2E_GitHub_CancelledRunLeavesNoDeletionMark` measures.

Worth noting for the eventual implementation: the AGC already *has* this information and discards it.
`InformerPodWaiter.onPodDelete` resolves waiters with `phase: PodSucceeded` and an empty reason ([`podwaiter.go`](../../../cmd/agc/internal/provisioner/podwaiter.go)), identical to a genuine success, matching the older poll loop's "deleted externally → treat as completion".
Closing the gap needs no new watch, annotation or pod bookkeeping — it needs the waiter to stop flattening a distinction it can already see.

## Findings so far

**`rerun-failed-jobs` has a one-month retention window** — measured 2026-07-28, while attempting to answer the re-runnability question cheaply against an already-cancelled historical run rather than a freshly interrupted one:

```
POST /repos/actions-gateway/gateway-test/actions/runs/27386814795/rerun-failed-jobs
HTTP/2.0 403 Forbidden
{"message":"Unable to retry this workflow run because it was created over a month ago"}
```

Two things follow.
The shortcut does not work — every cancelled run in the fixture repo predates the window, and the 403 is about the run's *age*, not about its `cancelled` conclusion, so it says nothing about the question Q459 asks.
And the constraint is real for the design: whatever the AGC recovers, it recovers only inside that window.
That is comfortably wide for eviction recovery, which fires seconds after the disruption, but it does bound any future backstop that retries from a persisted record after a long AGC outage — the 12h `maxWorkerLifetime` cap sits inside it, a restart-time sweep of much older runs would not.

## The measurement found something it was not looking for (Q495)

Worker pods provisioned for **real** GitHub jobs on the classic tier carry **no** `actions-gateway.com/run-id` annotation.
Observed 2026-07-28 while scoping the spec's worker lookup to its own run; the jsonpath was verified against a control pod carrying a known annotation, so the empty value is the pod's, not the query's.

That matters well beyond this plan.
[`repoInfo()`](../../../cmd/agc/internal/provisioner/payload.go) — which supplies the `runID` that eviction recovery re-runs — and `jobMetaFrom()` — which supplies the annotation — read the *same two sources*: `Variables["system.github.run_id"]`, falling back to `ap.RunID`.
An absent annotation therefore means both were empty, so `runID` is `"0"`, and [`handleEviction`](../../../cmd/agc/internal/provisioner/eviction.go) opens with `if runID == "0" || runID == ""` → log and return.

**Inference, not yet a direct observation:** classic-tier eviction recovery cannot name the run to re-run against real GitHub, so it cannot fire.
Every test that exercises it uses a fakegithub payload carrying the identity explicitly — Q421's own fake-GitHub drain spec had to *inject* it, recording that "the default fakegithub response carries no run identity, and handleEviction returns early without one".
The fake was adjusted to make the test pass; the real payload was never checked.
Confirming it looked like it needed one run that evicts a real job and looks for the skip; Q495 carried that, and the section below records what confirming it actually took.

It also bears on this plan's decision: closing the drained-worker gap by calling `rerun-failed-jobs` buys nothing on classic if the run ID is unavailable in the first place.
Q495 is therefore a prerequisite for the "close" branch, not a side finding.

### Confirmed and fixed, 2026-07-29 (Q495)

The inference held, and confirming it needed no live-GitHub run after all.
The repo already contained the answer: `testdata/job_payload.json` is a redacted capture of a **live** `acquirejob` response, taken by `cmd/probe` and committed as ground truth for Milestone 3's handoff — and nothing had ever read it back.
Parsed, it carries no top-level `run_id`, no `system.github.run_id`, and no `system.github.repository`.
Its run identity is in `contextData.github`, as `run_id` and `repository` entries of the serialised `github` context.
Milestone 3's own plan doc had said so all along: "`contextData.github.run_id` (a string)".

So both readers were looking somewhere the identity has never been, and the two symptoms are one cause: `repoInfo()` returned `("", "", 0)` and `jobMetaFrom()` returned an empty `jobMeta`.

The fix reads the `github` context, keeps the variables and top-level `run_id` as tolerated fallbacks, and routes both readers through one `runIdentity()` so they cannot diverge again.
`payload_groundtruth_test.go` asserts against the capture — against the unfixed parser it fails with exactly the observed symptom (owner `""`, repo `""`, run `0`), which is what makes it a guard rather than a restatement.
The synthetic payloads in the provisioner unit tests, the envtest eviction and drain specs, and Q421's fake-GitHub drain spec were all moved onto the real shape, since a fake carrying a field GitHub does not send is what kept this invisible.

Two consequences for this plan.
The "close" branch's prerequisite is met at the level it was blocked on — classic can name the run it would re-run.
And the exact-worker lookup question 2 wanted should now be available, since the annotation is stamped from the same resolved identity; that it lands on a real worker pod is the one thing here still owed a live-GitHub observation, and un-pending `E2E_GitHub_CancelledRunLeavesNoDeletionMark` is where it gets one.

## Result: the cancellation path, measured 2026-07-29

Live-GitHub tier on a dedicated kind cluster, against `actions-gateway/gateway-test` ([run 30455540731](https://github.com/actions-gateway/gateway-test/actions/runs/30455540731)), by `E2E_GitHub_CancelledRunLeavesNoDeletionMark` — now un-pended, and passing.
A real runner was executing a real job, and the run was cancelled from GitHub the way a human would cancel it.

| Observation | Value |
|---|---|
| Worker pod phase/reason/deletion sequence | `Running//deleting=` → **`Failed//deleting=`** |
| Cancel → GitHub concludes the job | **5m02s**, conclusion `cancelled` |
| Cancel → worker pod reaches a terminal phase | **10m02s** — the fixture's full 600s sleep |
| `deletionTimestamp` at terminal publish | **absent** |

Reproduced the same day on a second cluster, with the whole live-GitHub container running in order rather than this spec alone ([run 30459264313](https://github.com/actions-gateway/gateway-test/actions/runs/30459264313)): the identical `Running//deleting=` → `Failed//deleting=` sequence, GitHub concluding at **5m01s**, the pod at **10m04s**.
That run also re-measured the graceful-deletion half independently — `deletionGracePeriodSeconds` 30 with the mark set, `Running/` → `Failed/`, conclusion `failure` in **16s**, `rerun-failed-jobs` **201**, second attempt reaching a runner — so both halves of the discriminator come from two runs each, not one.

**Question 2 is answered, and the discriminator holds.** Put beside the graceful-deletion result above, the three cases separate exactly where the candidate said they would:

| Case | Phase | `status.reason` | `deletionTimestamp` at terminal publish | |
|---|---|---|---|---|
| Drained / deleted worker | `Failed` | *empty* | **set** | measured 2026-07-28 |
| Human-cancelled run | `Failed` | *empty* | absent | measured 2026-07-29 |
| Genuinely failed job | `Failed` | *empty* | absent | not separately measured — a worker nothing deleted has no deletion mark by construction |

Phase and reason are identical across all three, so neither can carry recovery.
The deletion mark separates the first from the other two, which is precisely what an automatic re-run needs: it fires on the disruption and never on a cancel or a real failure.

The third row is the one that did not need its own live-GitHub run: `deletionTimestamp` is set by the apiserver only when something issues a delete, so a worker whose job simply exited non-zero cannot carry one.
What *did* need measuring is the cancel row — the case where a human's intent reaches the system through GitHub rather than through Kubernetes, and where it was genuinely open whether some part of the gateway responds by deleting the pod.
Nothing does.

### The measurement also found that a cancel never reaches the worker (Q501)

The 10m02s figure is not incidental.
The job's own step — `sleep 600`, started at 13:21:00Z — ran to completion at 13:31:04Z, ten minutes after the run was cancelled and five minutes after GitHub had already concluded it.
The runner was never told.

That is consistent with the architecture rather than a fluke: the AGC owns the broker session, a cancellation arrives on that session, and nothing in [`listener/`](../../../cmd/agc/internal/listener/) relays it to the worker pod.
The 5m02s to conclusion is GitHub's own cancellation grace lapsing, not the runner acknowledging anything — the same figure, 5m01s, was measured on the first attempt at this spec.

Two consequences, both real:

- **A cancelled job keeps burning a worker** for the remainder of its steps, up to `maxWorkerLifetime`.
  Cancelling a runaway job does not reclaim its capacity.
- **[04-operational-flows.md](../../design/04-operational-flows.md) §4.2 step 7 overstates the Q385 relay.** It lists "cancelled run" among the terminations the wrapper's SIGTERM relay reaches.
  The relay does reach the engine whenever the *pod* is terminated — but on the cancel path nothing terminates the pod, so the relay never runs.
  [Q501](../../queue/Q501.md) carries the gap; the doc is corrected as part of this change.

### What the spec needed in order to run at all

It was pending because it could not tell its own worker from the one the graceful-deletion spec leaves behind, and the run-id annotation that would have disambiguated them is the one Q495 found missing.
Q495 turned out **not** to be the only way through: both specs now snapshot the Running worker pods immediately before dispatching, and take the worker that was not there before.
Identity, not count — the same trick `dispatchAndResolveRun` already uses for the run itself.
A lingering worker from an earlier spec is no longer ambiguous, so no waiting for the namespace to fall quiet is needed either.

Q495 was still a prerequisite for the **implementation**, for the separate reason recorded above — without a run ID, classic-tier `handleEviction` returns early and has nothing to re-run — and it has since been fixed.

### Operational notes for the next live-GitHub run

- **Do not use the shared `actions-gateway-e2e` cluster.** This spec swaps the GMC's GitHub env vars cluster-wide and holds them for ~12 minutes.
  Mid-run on 2026-07-29 another session reinstalled the chart on that cluster six times (helm revisions 2–7), and the `kubectl set env` this suite performs is itself what makes the next `helm upgrade` conflict on server-side-apply field ownership.
  Create a throwaway cluster: `make e2e-cluster KIND_CLUSTER=<name>`, and point the run at it with a private `KUBECONFIG` rather than the ambient context.
- **Read the spec summary, not the wall clock.** This run's four specs finished in 19m04s inside a `ginkgo` process that lived 94 minutes, because the host slept mid-run.
  Both false "it's wedged" diagnoses this session came from host-side clocks, and the rule that came out of it is now in [testing.md](../../development/testing.md#end-to-end-tests).
- **Two concurrent live-GitHub sessions collide on the fixture repo.** Both dispatch the same `drain-probe.yml` in `actions-gateway/gateway-test` and both register a runner named `real-ag-e2e-6d8749c-0`.
  Two such runs were in flight simultaneously on 2026-07-29.
  `dispatchAndResolveRun`'s snapshot keeps each spec on its own run, and this measurement's binding was confirmed directly (the pod's job began at 13:21:00Z, matching its run's 13:20:55Z job start, and the namespace did not exist when the other run started) — but the runner-name collision was not defended against.
  [q511-live-github-run-isolation.md](../q511-live-github-run-isolation.md) settled it: the suite's `BeforeAll` now refuses to start while the fixture repo is not idle.

Earlier attempts were blocked before even reaching it, by the PriorityClass VAP param-resolution failure that [q444-vap-param-resolution.md](q444-vap-param-resolution.md) investigates and Q492 has since fixed, by moving the guard's `paramKind` off a core type.

The e2e cluster's kube-apiserver entered that broken state between the run that produced the result above and the next one, and every run since fails in `BeforeAll` with the RunnerGroup create denied:

```
ValidatingAdmissionPolicy 'gmc-priorityclass-allowlist-guard' … denied request:
failed to configure binding: no params found for policy binding with `Deny` parameterNotFoundAction
```

with `gmc-priorityclass-allowlist` present in `gmc-system` and the binding pointing exactly at it — the shape [q444-vap-param-resolution.md](q444-vap-param-resolution.md) § Established by measurement records as findings 1 and 2.
Nothing here adds to that investigation: it has since established the trigger — deleting the `ValidatingAdmissionPolicyBinding` empties the paramKind's binding set and the apiserver never restarts the shared informer — which is consistent with what happened here, since the suite's `helm uninstall` teardown deletes exactly that object.

Recovery was confirmed on 2026-07-28: `crictl stop` on the kube-apiserver container cleared it, verified by the container ID changing and its `ATTEMPT` going 0 → 1.
A `kubectl delete pod` of the static-pod *mirror* does **not** restart it and must not be mistaken for a restart — that plan records a conclusion drawn from exactly that non-restart and later withdrawn.

That blockage is historical.
Q492 fixed the trigger, and the 2026-07-29 run above completed on a freshly created cluster without going near it.

## The decision: close, gated on the deletion mark

Both questions are answered, and they select the decision table's **first row**.

> **Close.** Extend both tiers' detection to the graceful-deletion shape, gated on `metadata.deletionTimestamp` being set at the moment the worker's terminal phase publishes.

The premise is measured on both halves: the relayed report leaves the run re-runnable (`rerun-failed-jobs` → `201`, second attempt reaches a runner), and a deliberate cancel is distinguishable at the pod (no deletion mark).
Neither is inferred from reading code.

**The residual ambiguity is an operator's bare `kubectl delete pod`.** It is indistinguishable from a drain, and it would re-run.
That is the right behaviour rather than a defect to design around: someone deleting a worker mid-job has interrupted a job they did not intend to fail, which is the same thing a drain does.

### What the implementation has to get right

Recorded here because each was established while measuring, not while coding, and none of it is visible from the eviction path alone:

1. **The waiter already sees the mark and throws it away.** `InformerPodWaiter.onPodEvent` holds the whole pod when it resolves, so `deletionTimestamp` is readable at exactly the instant the terminal phase publishes.
   What blocks it is the contract: `PodWaiter.WaitForCompletion` returns only `(phase, reason, error)` ([`podwaiter.go`](../../../cmd/agc/internal/provisioner/podwaiter.go)), so the interface has to widen for the signal to reach `provision()`.
   No new watch, annotation, or pod bookkeeping is needed.
2. **Exclude the AGC's own deletions, or the reaper becomes a re-run trigger.** [`reapWorkerPods`](../../../cmd/agc/internal/controller/runnergroup_reaper.go) deletes worker pods on three arms; `pending_deadline` applies to both tiers and `orphaned_running` to scale-set.
   Each sets a `deletionTimestamp` on a pod the AGC itself gave up on, which under a naive deletion-keyed rule would re-run it.
   `lifetime_exceeded` is already safe by a different route — the kubelet publishes `DeadlineExceeded` *before* the reaper deletes, so that terminal phase carries no deletion mark and the existing reason check still separates it.
3. **The classic half needs Q495's fix to be real on a live worker.** Without a run ID, `handleEviction` returns early, so closing the gap on classic would buy an interface change and no recovery.
   Q495 has since been fixed — the identity is read from the payload's `github` context — but that fix has not yet been seen on a real worker pod at live-GitHub, so confirm the annotation before relying on the recovery it enables.
   The scale-set tier reads its identity from the pod annotations and is unaffected.
4. **Do not fold in the cancel path.** [Q501](../../queue/Q501.md) is a separate defect with a separate fix.
   A cancelled run's worker is *not* deleted, so it never reaches this recovery path — but if Q501 is later fixed by having the AGC delete the worker on cancellation, that deletion becomes indistinguishable from a drain and must be excluded exactly as the reaper's are.

## Status

**Decided and implemented.** Both questions are answered and recorded above, the close-or-accept decision is taken (close, gated on the deletion mark), and Q502 shipped the implementation.
What landed, against the four constraints:

1. *Widen the waiter.* `PodOutcome` gained `ExternallyDeleted` — set when the terminal phase publishes with a `deletionTimestamp` that is not the AGC's own — and `provision()` recovers on `PodFailed` + that mark (`cause="deletion"`), through the same `handleEviction` and shared budget as eviction and preemption.
2. *Exclude the AGC's own deletions.* The reaper stamps `actions-gateway.com/deletion-reason` on every pod before deleting it — which needed a `pods:patch` grant the AGC role never had; adding it also un-broke the shipped scale-set claim and completed-at stamps, silently 403'd on real clusters until now.
   Both tiers additionally order the mark against the container's recorded `finishedAt`: that excludes an operator's cleanup delete of an already-failed pod, and a deleted worker that never ran — a real kubelet publishes a transient `Failed`-with-mark even for a drained still-`Pending` pod, which CI's fake-GitHub drain spec caught a mark-only rule recovering.
3. *Classic needs Q495.* Fixed previously, and **confirmed at live-GitHub on 2026-08-03** against the published `v1.3.0` images (Q544): a real worker running a real job carried both `run-id` and `repository`.
   The specs no longer accept their absence — worker lookup matches on the annotation and nothing else, so resolving at all proves `run-id`, and `repository` is asserted beside it because `handleEviction` needs owner/repo too.
   Detail: [eviction-oversubscription-validation.md](../eviction-oversubscription-validation.md#the-scale-set-half-how-it-was-measured).
4. *Do not fold in the cancel path.* Untouched — a cancelled run's worker is not deleted, so it never enters this path; Q501 remains its own item, and any future cancel-relay deletion must stamp the same annotation.

The scale-set arm shares preemption's non-restart-safety, with a shorter window: the Failed-with-mark pod is readable only between terminal publish and the kubelet's final removal.
Design boundary: 04-operational-flows.md §4.2; operator behaviour: troubleshooting.md "Draining a Worker Auto-Re-Runs the Jobs It Interrupts".
Pinned by `TestAGC_Drain_ClassicWorkerTerminalWithMark_Reruns` / `TestAGC_Drain_ScaleSetWorkerTerminalWithMark_Recovers` (recovered side) and the retained `DoesNotRerun`/`DoesNotRecover` pair (no-terminal-phase side), plus the podwaiter/scan/reaper unit tests.

**Q519's first run caught the deletion arm inert on real clusters — a timestamp-shape bug, fixed with the gate.** The spec's field sampler recorded the disrupted worker as `Running/2026-07-31T07:02:00Z/` → `Failed/2026-07-31T07:02:00Z/` against a delete issued at 07:01:32: the apiserver stamps `deletionTimestamp` as request time **plus the grace period**, so on a real kubelet the mark sits ~28s in the *future* of the exit a SIGTERM-honouring runner records seconds after the request.
`externallyDeletedBeforeTerminal` compared the raw mark and read every real drain as "deleted after terminal" — the cleanup shape — so no recovery fired; the shipped form could only ever recover a worker that ignored SIGTERM to its SIGKILL (exit at mark == grace expiry).
Both tiers shared the predicate, so classic drain recovery was equally inert.
Every prior gate missed it for venue reasons: envtest pods are unscheduled, so their deletion collapses grace to zero and the mark *equals* the request time (the `TerminalWithMark` pair passes against that artifact shape), and the unit fixture restated the same wrong ordering (`finishedAt = deletionTimestamp + 5s`).
The fix orders the deletion *request* — `deletionTimestamp` minus `deletionGracePeriodSeconds`, the two fields the apiserver stamps together — against the termination record, and makes the never-ran exclusion explicit (no termination record → nothing reportable to re-run) rather than an accident of the creation-time fallback.
The unit fixtures now model the real stamp shape.

**The RBAC gate this shipped without now exists (Q519).** The claim and completed-at patches ran 403-broken on real clusters because every local tier's client is admin — envtest does not enforce RBAC, and the fake-GitHub disruption specs were classic-tier.
`E2E_AGC_ScaleSetRecovery` (`cmd/gmc/test/e2e/worker_scaleset_recovery_test.go`) closes that class at the fake-GitHub kind tier: a ScaleSet-protocol RunnerSet whose AGC runs under the chart's shipped `agc-tenant-role`, a running scale-set-shaped worker deleted gracefully, and the assertion that exactly one rerun lands — which, by the reconciler's ordering, is also the assertion that the claim patch landed (recovery calls GitHub only after the claim succeeds).
Two scope limits, stated plainly: the spec stages the worker pod itself rather than provisioning it from an assignment, and the deliberately-failing listener bootstrap — the gateway's `githubURL` names fakegithub's plaintext port over https — is what keeps the reconcile loop fast enough to win the real teardown window.

Both limits were originally forced by the venue: the e2e fakegithub spoke only the classic protocol, so no scale-set session could open there at all.
That is no longer true — Q528 taught it the scale-set protocol, and [`E2E_AGC_ScaleSetAcquisition`](q528-scaleset-acquisition-e2e.md) drives the acquisition half through it end to end.
The limits above are now this spec's deliberate scope: a recovery scan running on a set whose listener is *not* up is the harder case, and the staged pod is a faithful subject for it because recovery selects by label and reads annotations, never caring who created the pod.

The remaining work was carried by these Queue rows:

1. ~~**Q495 first.**~~ Done — the run identity is read from the payload's `github` context, so the worker lookup can be made exact.
   **Still owed a live-GitHub confirmation**: the 2026-07-29 runs below predate that fix (their images were built from `719e67f1`), so what they observed — every worker matched by the snapshot fallback, none by the annotation — is the *unfixed* build's behaviour, and is evidence for the defect rather than for the fix.
   The next credentialed live-GitHub run should check that `actions-gateway.com/run-id` now appears on a real worker pod.
2. ~~Un-pend `E2E_GitHub_CancelledRunLeavesNoDeletionMark` and run it.~~ Done — it no longer needed the annotation to become runnable (see above), and it passes.
3. ~~Take the decision.~~ Done — the decision table's first row, recorded above.
4. ~~Q502 — implement the close, per the four constraints above.~~ Done — see Status.
5. **[Q501](../../queue/Q501.md)** — relay a run cancellation to the worker pod.
   Found by this measurement, independent of the gap.
   Split into a trigger and an actuator by [q501-cancel-relay.md](../q501-cancel-relay.md): the actuator shipped (a worker whose job the gateway abandons is now deleted, stamped `deletion-reason: job_abandoned` exactly as the constraint above requires), the trigger is still open.

Operational note for whoever runs live-GitHub next, learned the expensive way: the suite teardown's `helm uninstall` deletes the `ValidatingAdmissionPolicyBinding`, which was exactly the trigger — so each run poisoned the next one's apiserver.
**Q492 has since fixed this**: the guard's `paramKind` is now a CRD, for which the apiserver allocates a fresh dynamic informer per context, so emptying the binding set no longer breaks param resolution.
The workaround below is retained for anyone reproducing this investigation on a pre-Q492 build: restart the kube-apiserver (`crictl stop`, verified by `ATTEMPT` incrementing) *before* a run, not after a failure.
Running with `E2E_SKIP_TEARDOWN=true` avoids the uninstall, but then a subsequent `helm upgrade` conflicts on server-side-apply field ownership (`kubectl-patch`/`kubectl-set` claim fields helm owns); deleting just the `gmc-controller-manager` Deployment beforehand clears that without touching the binding.
