# Q550: reaping a worker pod leaks its scale-set runner registration

Close the runner-registration leak in the scale-set acquisition tier: every `generatejitconfig` pre-registers a runner record at GitHub, and nothing in the provision→reap lifecycle removes it.
Because the runner name is derived from the job ID, a job's own leftovers collide with its retries — the failure mode that wedged the `v1.3.0-rc.2` validation window with 22 stale `gag-ci-e2e` records.

Closed 2026-07-31 (Q550, `bug` `1.3-gate`).
Sibling row Q551 covered what the listener does *after* the collisions become unrecoverable — it kept the skip that stops one stuck assignment wedging the batch, but now holds the job and re-offers it on a backoff, surfacing the stall as `JobProvisionStalled`.
The two shipped separately the same day: this fix removes most of what *causes* the collisions, Q551 makes what remains recoverable and visible.

> **Verified at unit and integration; not yet re-verified live.** The defect was found in the `v1.3.0-rc.2` dogfood window, and no tier below a live-GitHub run can confirm that the REST listing behaves as the fake models it.
> The release gate's own dogfood validation is where that confirmation lands — it is part of the [Release 1.3](../release-1.3.md) Definition of Done rather than a Queue row.
> The `Warn` line added to the unresolvable-reclaim branch is what will name the live mechanism if it is still the one suspected here.

## Status

| Phase | Scope | Status |
|---|---|---|
| 0 | Confirm the leak mechanism against the stub and name what is measured vs. inferred | ✅ Done — see [Findings](#findings) |
| 1 | Carry the minted runner name onto the worker pod | ✅ Done |
| 2 | Deregister on reap | ✅ Done |
| 3 | Sweep unclaimed records at listener start | ✅ Done |
| 4 | Tests, docs, operator surface | ✅ Done — unit + integration; no new metric (see [Out of scope](#out-of-scope)) |

## Findings

**The stub could not express the bug.** `handleGenerateJIT` never recorded the name it minted, so no test at any tier could observe a leaked registration — the leak was invisible to the whole suite by construction.
Teaching the fake to register on mint (and to `409` a re-mint of a live name, as the real endpoint does) is a finding about the real interface, not test scaffolding: it is what made the two cases below distinguishable.

**Q334's reclaim already covers the self-collision — when the record resolves.** With the faithful stub, a job that fails to provision three times and retries ends with exactly one record: each attempt's `409` triggers the by-name delete and re-registers under the same base name.
`TestListener_RetryReclaimsItsOwnLeftoverRegistration` pins that, and it is why the original framing of "retries 409 against their own leftovers" is not by itself the accumulation mechanism.

**The accumulation needs the reclaim to resolve nothing.** When a record holds the name at `generatejitconfig` but the REST name filter does not return it, `DeregisterRunnerByName` reports `(false, nil)` — which the code ignored silently, with no log at any level — and every retry escalates to a suffixed name that no later attempt ever revisits.
Each of those registers a record nothing will collect.
`TestListener_UnresolvableLeftoverAccumulatesSuffixedRecords` pins it.
This is hypothesis (2) below, now demonstrated as *a* sufficient mechanism against the fake; whether it is what the live API did on 2026-07-31 is still unconfirmed, which is why the `(false, nil)` branch now logs at `Warn` — an operator hitting it will see it named.

The fix closes the leak under either mechanism, so nothing here blocked implementation.

## The defect

### What is established

These come from reading the code and from the incident, not from a live probe:

- `Client.GenerateJITConfig` pre-registers a runner record server-side ([client.go:512](../../../scaleset/client.go)).
  The listener mints one per assigned job before provisioning.
- The runner name is `{scaleSetName}-{jobID}`, deterministic per job (`Listener.runnerName`), with `-1`/`-2`/`-3` suffixes on the conflict-retry path.
- Nothing in the reap path deregisters anything.
  `reapWorkerPodsByLabel` ([runner_shared.go:232](../../../cmd/agc/internal/controller/runner_shared.go)) patches a deletion reason and deletes the pod.
  It has no GitHub client and no runner name to act on.
- **The minted runner name never leaves the listener.** `scalesetlistener.Job` carries `RunnerName`, but `ensureScaleSetListener`'s `Provision` closure ([runnerset_scaleset.go:165](../../../cmd/agc/internal/controller/runnerset_scaleset.go)) copies `JobID`, `JITConfig`, and the run identity into `provisioner.ScaleSetJob` and drops `RunnerName` on the floor.
  `ScaleSetJob` has no such field.
  So no downstream component *could* deregister the right record today.
- A record is removed only by the ephemeral runner deregistering itself after it completes a job, or by Q334's opportunistic reclaim on a 409 (`generateJITConfig`'s base-name branch).

Every path that kills a worker before its runner runs a job therefore leaks: pending-deadline reap (unschedulable — quota, stockout), orphaned-running reap, lifetime-cap reap, and terminal-pod reap of a pod that failed before the runner connected (image pull, crash).

### What is inferred, and must be checked in Phase 0

Q334's reclaim already deletes a colliding *base* name, so a job retrying against its own base-name leftover should self-heal.
Two things it does not cover, either of which produces the observed accumulation:

1. **The suffixed names are never reclaimed.** The fresh-name loop in `generateJITConfig` tries `-1`, `-2`, `-3` and calls `DeregisterRunnerByName` on none of them.
   A job that once took the fallback path leaks up to three records that no later attempt can clear, and the next cycle collides on all four names and hits Q551's permanent skip.
2. **`DeregisterRunnerByName` may not resolve the record.** It finds the id via the REST `list-runners?name=` filter; a `(false, nil)` return — no match — is silently ignored (the `else if deleted` branch simply is not taken, with no log at any level), and the code falls into the suffixed-name loop.
   If scale-set JIT records are not visible under the REST prefix the client derives, *no* reclaim ever works and every retry leaks a fresh name.

(1) is certain from the code.
(2) is a hypothesis about the live API; Phase 0 records which one the 22 records are consistent with.
**The fix below closes the leak under either**, so implementation does not block on the answer — but a `(false, nil)` from a name we believe is registered is worth a log line, and Phase 0 adds one either way.

## Design: the worker pod is the registry

The AGC keeps no process state about a scale-set job — that is the tier's design (fire-and-forget provisioning, §2.4), and it is why Q417 put the run identity and Q420 put the reap deadline on the pod rather than in memory.
The runner name belongs there for the same reason: the component that must deregister it (the reaper) runs long after the listener goroutine that minted it, and possibly in a different AGC process.

That single decision gives both halves of the fix:

- **Deregister on reap** — the reaper reads the name off the pod it is about to delete and removes the record.
  Closes the leak at its source.
- **Sweep at start** — a record whose name is not claimed by a live worker pod is by definition unclaimed, so the listener can delete it safely.
  Clears whatever accumulated before the fix (and anything a crashed AGC misses afterwards), which is what lets a wedged scale set self-heal.

The pod cross-check is what makes the sweep safe.
A worker that is still `Pending` has a legitimately *offline* record — its runner has not connected yet — so "offline" alone is not a sweepable signal, and the REST runner object carries no timestamp to age records by.
Excluding every name stamped on a non-terminal worker pod is the available sound guard.

## Phases

### Phase 0 — confirm the mechanism

- Add a `Debug` log to the `(false, nil)` branch of `generateJITConfig`'s reclaim so an unresolvable record is distinguishable from a resolvable one in AGC logs.
- Extend `scalesetstub` to model the leak: a `generatejitconfig` that registers the name (it already tracks `runnerIDs`), and a knob for "registered but not resolvable by the name filter" so hypothesis (2) can be exercised as a test case rather than a guess.
- Write a failing unit test that drives the observed cycle: provision fails repeatedly for one job, and assert the count of records left registered at the stub.
  This is the regression test the fix has to flip.

### Phase 1 — carry the runner name to the pod

- Add `RunnerName` to `provisioner.ScaleSetJob` and stamp it as `actions-gateway.com/runner-name` in `ProvisionScaleSetWorker`, alongside the existing `jobMeta` annotations.
- Wire `job.RunnerName` through the `Provision` closure in `ensureScaleSetListener` — the one-line drop that makes the rest possible.
- An unstamped pod (pre-upgrade, or a classic-tier pod) must degrade to today's behaviour: no deregister attempt, no error.
  The reap itself never depends on it.

### Phase 2 — deregister on reap

- Give `reapWorkerPodsByLabel` a deregister hook.
  Its signature already carries three nil-able Event closures and is at twelve parameters; bundle those plus the new hook into a small `reapHooks` struct in the same change rather than adding a thirteenth positional argument.
- The hook is wired only by the RunnerSet reconciler and only for pods labelled `AcquisitionProtocolScaleSet`; the classic tier's agent-pool JIT agents have their own re-registration lifecycle (Q114) and are out of scope.
- Deregister **before** deleting the pod, so a delete that fails still leaves the record clean.
  Best-effort throughout: an error or a `RunnerBusyError` logs and proceeds with the reap — the reap policy has already condemned the pod, and a busy record is one the AGC must not remove.
- All four reap reasons deregister.
  A record the ephemeral runner already removed costs one `(false, nil)` lookup, which is the common case for a healthy `completed_ttl` reap.

Cost: one REST list plus at most one delete per reaped pod.
Reaps are not a hot path, but this is new per-pod GitHub API traffic and should be named as such in the operator docs.

### Phase 3 — sweep unclaimed records at start

- New client method to list runners under a name prefix (the existing helper only resolves one exact name), returning id, name, status, and `busy`, with pagination.
- New `Listener` config hook returning the runner names currently claimed by live worker pods; the reconciler wires it to a pod list filtered on `LabelRunnerSet` + the runner-name annotation.
- On `Start`, after `ensureScaleSet`: list records prefixed `{scaleSetName}-`, drop any that are claimed, busy, or online, delete the rest.
  Failures are logged and non-fatal — a sweep that cannot run must not stop a listener from starting.
- The sweep is bounded and runs once per listener start, not per poll.

### Phase 4 — tests, docs, operator surface

Tests:

- Unit: the Phase 0 regression test flips; sweep selection (claimed / busy / online / unclaimed); deregister-on-reap per reap reason; busy and error paths leave the reap unaffected; an unstamped pod is a no-op.
- Integration (`cmd/agc/internal/controller/integration/`): a reaped scale-set worker's record is gone at the stub — the envtest tier is the one that can observe reconciler wiring, which is where the name is dropped today.
- e2e: assert the stub holds no leftover records after a scale-set run completes.

Docs (per the change-type mapping in [doc-update-matrix.md](../../development/doc-update-matrix.md)) — this change adds a pod annotation, changes a failure mode, and adds GitHub API traffic, so the operator half is required, not optional:

- `docs/operations/` — the new annotation, the deregister-on-reap behaviour, the start-up sweep and what it will and will not delete, and the added API calls.
- `docs/design/` — the "pod is the registry" rationale, extending the Q417/Q420 precedent.
- A metric for swept and deregistered records, if it fits the existing scale-set counter vocabulary without inventing a new one.

## Out of scope

- **Q551's permanent skip.** This fix removes most of what *causes* the collisions, but a job that still exhausts its attempts must not be dropped silently.
  Separate row, separate PR.
- **The classic tier.** Its JIT agents re-register per job (Q114) and do not use `generatejitconfig`.
- **Delete-acking the message queue.** Unrelated open P4 question (`advanceCursor`'s note), even though it also concerns cleanup on replay.
- **A dedicated metric for swept/deregistered records.** Considered in Phase 4 and dropped: the deregistration rides an existing reap, which `actions_gateway_worker_pods_reaped_total` already counts, and the sweep runs once per listener start with its result on one log line.
  A counter would add a series to every AGC to report a number that should be zero, and no alert would key off it.
  If a standing rate of sweeps ever turns out to be the signal that a tenant is leaking, that is the point to add one.
