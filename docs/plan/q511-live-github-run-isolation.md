# Isolating concurrent live-GitHub runs

Two live-GitHub e2e runs executed simultaneously on 2026-07-29 and collided on shared GitHub-side state.
Neither run failed — they interfered *invisibly*, and roughly 2.5 hours went into diagnosing symptoms that had no in-cluster cause.
A third failure mode surfaced the same day: a run killed with `kill -9` skips its `AfterAll`, stranding a registered runner that then poisons the next run.

This plan carried Q511, and absorbed the collision reported separately as Q500 — that row described the same incident and proposed the alternative fix rather than complementary work.
The forbid half shipped 2026-07-31; the isolation half is parked as [Q530](../STATUS.md#Q530).

## What was observed

| Symptom | Mechanism |
|---|---|
| Two runs register the same runner name | Both dispatch `drain-probe.yml` in `actions-gateway/gateway-test` and register `real-ag-e2e-6d8749c-0`. The name is derived from run-invariant inputs, so it is identical across concurrent runs. |
| Neither run reports a conflict | `dispatchAndResolveRun`'s snapshot keeps each spec bound to its own workflow run, so *run identity* is defended. The **runner name** is not, and nothing asserts the peer's absence. |
| A killed run breaks the next one | `kill -9` skips `AfterAll`, so the runner is never deregistered. The stranded registration is still present when the next run starts. |

### Why the collision is silent

Runner names are unique per registration scope, and the fixture repo is one scope (the live-GitHub tenant sets `GITHUB_ORG_URL` to the repo, so `agentpool` registers repo-scoped).
A second run registering a name the first already holds therefore takes `registerAgent`'s conflict path in [`pool.go`](../../cmd/agc/internal/agentpool/pool.go): resolve the conflicting record, **delete it**, register again.
Both runs end up holding a listener whose registration the other has removed, and each acquires jobs the other dispatched.

Nothing on either side errors, because that path is the intended recovery for an AGC restart — where deleting the incumbent record *is* correct.
Concurrency turns a correct repair into mutual sabotage, and the only observable is a job that never gets a worker.

The singleton rule ("only one live-GitHub run at a time") was documented in [testing.md](../development/testing.md) but had no enforcement, which is why two runs were in flight without either operator noticing.

The measurement taken during the colliding run was separately confirmed sound — the worker pod's job began at 13:21:00Z, matching its own run's 13:20:55Z job start, and its namespace did not exist when the other run started.
So this plan addresses a latent hazard, not a known-corrupted result.

## The decision: forbid concurrency

The two filed rows proposed opposite designs — fail fast when a peer run is live, or scope the names so concurrent runs cannot collide. **Forbid wins**, decided 2026-07-31.

What settled it is that isolating the *names* does not isolate the *routing*.
GitHub routes a job by the runner's `runs-on` label, and the fixture workflows in `actions-gateway/gateway-test` pin that label literally.
Per-run names would leave both runs advertising the same label in the same repo, so either run's job could still land on either cluster's gateway — the collision would survive the fix that was supposed to remove it.
Real isolation therefore needs the fixture workflows parameterized (`runs-on: ${{ inputs.label }}`), which is a change to a **different repository**, unverifiable from this one, and it still leaves `dispatchAndResolveRun`'s "the run that was not there before" racy under concurrency.

Forbidding costs the ability to take two independent live measurements at once.
That is a real cost given how many Queue rows want live runs, so isolation is deferred rather than declined — [Q530](../STATUS.md#Q530) carries it, and this plan stays open for that half.

## Scope

1. ✅ Decide forbid-vs-isolate and record the rationale here.
2. ✅ Implement the chosen design — a `BeforeAll` preflight.
3. ✅ Add a cleanup target that deregisters stranded runners, for the `kill -9` path that skips `AfterAll`.
   This is needed regardless of the decision above.
4. 🔲 Isolate the runs, if the cost of the singleton rule proves too high ([Q530](../STATUS.md#Q530)).

## What shipped

`preflightFixtureRepoIdle` runs in the live-GitHub suite's `BeforeAll`, ahead of the cluster-wide GMC env swap — the suite's first mutation of anything shared.
It fails the suite when the fixture repo carries either a runner this suite owns or a workflow run that has not completed.

It cannot tell a live peer from wreckage a killed run left behind, and does not try: it prints what it found and names both remedies.
The `kill -9` remedy is `make e2e-github-cleanup`, which deregisters those runners and cancels the runs holding them.
The two filters have to agree — a cleanup narrower than the preflight would report success and leave the next run still blocked — so the runner-name rule is stated in both places with a cross-reference, and `scripts/e2e/e2e-github-cleanup-test.sh` pins it from the shell side against the actual name observed on 2026-07-29.

## Acceptance criteria

- ✅ Starting a second live-GitHub run while one is in flight produces a clear, immediate outcome — a fast failure or a clean isolated run — never silent interference.
- 🔲 A run terminated with `kill -9` leaves no registration that affects the next run, after the cleanup target is invoked.
  Shipped, not yet demonstrated — see below.
- ✅ The singleton rule in `testing.md` is either enforced in code or replaced by documentation of the isolation guarantee that supersedes it.

### What is verified, and what is not

Verified live on 2026-07-31, against `actions-gateway/gateway-test`: the listing half of both filters. `gh api repos/…/actions/runners` returns what the preflight parses, over the operator's existing `repo` token scope — no App credential and no new host dependency — and `make e2e-github-cleanup ARGS='--dry-run'` reported the repo idle, which it was.

Not demonstrated: the destructive half.
The fixture repo held zero runners and no in-flight run when this was written, so nothing exercised a real DELETE, a real cancel, or the 422 wait that follows one.
The runner filter is asserted at the unit level against the name observed on 2026-07-29 (`scripts/e2e/e2e-github-cleanup-test.sh`), and the preflight's failure path has not been observed against real GitHub state at all.
The next `kill -9` is the opportunity — take it rather than assuming the second acceptance criterion above.
