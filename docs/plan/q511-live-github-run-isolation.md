# Isolating concurrent live-GitHub runs

Two live-GitHub e2e runs executed simultaneously on 2026-07-29 and collided on
shared GitHub-side state. Neither run failed — they interfered *invisibly*, and
roughly 2.5 hours went into diagnosing symptoms that had no in-cluster cause.
A third failure mode surfaced the same day: a run killed with `kill -9` skips
its `AfterAll`, stranding a registered runner that then poisons the next run.

This plan carries [Q511](../STATUS.md#Q511). It absorbs the collision reported
separately as Q500 — that row described the same incident and is merged here,
because its proposed fix and Q511's are alternatives to one another rather than
complementary work.

## What was observed

| Symptom | Mechanism |
|---|---|
| Two runs register the same runner name | Both dispatch `drain-probe.yml` in `actions-gateway/gateway-test` and register `real-ag-e2e-6d8749c-0`. The name is derived from run-invariant inputs, so it is identical across concurrent runs. |
| Neither run reports a conflict | `dispatchAndResolveRun`'s snapshot keeps each spec bound to its own workflow run, so *run identity* is defended. The **runner name** is not, and nothing asserts the peer's absence. |
| A killed run breaks the next one | `kill -9` skips `AfterAll`, so the runner is never deregistered. The stranded registration is still present when the next run starts. |

The singleton rule ("only one live-GitHub run at a time") is documented in
[testing.md](../development/testing.md) but has no enforcement, which is why two
runs were in flight without either operator noticing.

The measurement taken during the colliding run was separately confirmed sound —
the worker pod's job began at 13:21:00Z, matching its own run's 13:20:55Z job
start, and its namespace did not exist when the other run started. So this plan
addresses a latent hazard, not a known-corrupted result.

## The open decision: forbid concurrency, or isolate it

The two filed rows proposed opposite designs. Pick one before writing code —
implementing both wastes the work of whichever loses.

- **Forbid it (preflight).** Fail fast at suite start when a peer run is live.
  Cheap, enforces the rule already documented, and needs no change to name
  derivation. Costs the ability to run two independent live measurements at
  once — which matters given how many Queue rows need live runs.
- **Isolate it (scope the names).** Derive the runner group / runner name per
  run so concurrent runs cannot collide. Unlocks parallel live measurement,
  but is a larger change and leaves the shared fixture repo and workflow file
  as remaining shared state to audit.

A preflight is worth having under *either* choice — as an enforcement gate in
the first, and as a diagnostic in the second. Sequence the decision first.

## Scope

1. Decide forbid-vs-isolate and record the rationale here.
2. Implement the chosen design.
3. Add a cleanup target that deregisters stranded runners, for the `kill -9`
   path that skips `AfterAll`. This is needed regardless of the decision above.

## Acceptance criteria

- Starting a second live-GitHub run while one is in flight produces a clear,
  immediate outcome — a fast failure or a clean isolated run — never silent
  interference.
- A run terminated with `kill -9` leaves no registration that affects the next
  run, after the cleanup target is invoked.
- The singleton rule in `testing.md` is either enforced in code or replaced by
  documentation of the isolation guarantee that supersedes it.
