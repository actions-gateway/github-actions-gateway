# Condition and Event Reasons in the Acquisition-Tier Ledger (Q850)

> **Status (2026-08-14): done.** 71 reasons walked and given a tier: 45 condition reasons (34 Both, 5 classic-only, 6 scale-set-only) and 26 Event reasons (12 Both, 6 classic-only, 8 scale-set-only), with `make reason-tiers-check` holding both tables to the source.
> What the walk found is in [§6](#6-what-the-walk-found).

Q776 made the AGC's *metric* surface derive itself: every `actions_gateway_*` series carries a tier in the [acquisition-tier ledger](../../operations/observability-metrics.md#acquisition-tier-reach), and `make metric-tiers-check` fails a series added without one.
Condition reasons and Event reasons are the other two signals an operator sees, they are enumerable the same way, and neither is gated.
A capability that reaches only the classic tier is therefore still invisible if it surfaces as a condition or an Event rather than as a counter.
That is the failure mode Q683, Q691, Q713 and Q844 each demonstrated after parity had been declared.

**The goal:** a completeness claim about tier reach that covers all three signal surfaces, not one of them.
That is the precondition [release-1.5.md](../release-1.5.md#scope-reopened-2026-08-14-what-a-question-cost) records for saying *parity* in the marketing surfaces rather than *parity on the metric surface*.

---

## 1. Scope

**In.** Condition reasons the AGC writes into `.status.conditions[].reason`, and Event reasons the AGC records on a `RunnerGroup`/`RunnerSet`.
Both get a ledger row naming the tier, and a gate that fails a missing row, a bad tier value, and a single-tier row the source refutes.

**Out.** The GMC and proxy binaries: neither acquires jobs, so neither has a tier.
That is the exclusion the metric ledger already states.
Label values were Q851, a separate row, since shipped as [Label-value reach](../../operations/observability-metrics.md#label-value-reach); prose surfaces are [Q848](../../STATUS.md#Q848).

## 2. Decisions

**A second tool, not a rename.** The metric gate ships as `make metric-tiers-check`, and [`docs/releases/v1.5.0.md`](../../releases/v1.5.0.md) records it under that name.
A shipped release note is a historical record, so renaming `metrictiers` would either falsify it or leave the two out of step.
The new checker is `devtools/docs/reasontiers` behind `make reason-tiers-check`.
It reads its tables through the shared goldmark layer in `devtools/docs/markdown` rather than sharing `metrictiers`' hand-rolled scan, which leaves that gate untouched; the hand-rolled reader is itself a deviation from what every other Markdown gate uses, filed as its own row rather than fixed here.

**A sibling ledger section, not more rows in the metric table.** `metrictiers` anchors on the `## Acquisition-tier reach` heading and reads every table row to the next `##`, rejecting anything that is not an `actions_gateway_*` name.
Condition and Event rows go in their own `##` section immediately after it, which leaves the shipped gate's parse untouched and keeps both sections' anchors stable for the docs that already link them.

**The inventory is derived from references, not from declarations.** The reason constants live in `api/apiconditions`, shared with the GMC, so the declared set is not the AGC's set.
What the AGC references is, and a reason it only *reads* (`prev.Reason == ReasonVersionTooOld`) is one it sets somewhere too, or the comparison is dead.
Over-approximating adds a row; it never drops one, which is the safe direction for a completeness gate.

## 3. How the reason arguments resolve

An Event's reason reaches the recorder as a literal at some call sites and through a variable at others, and the recorder wrappers themselves pass it through as a parameter.
A scan that keys on the call name alone therefore counts plumbing as emission and misses the computed cases.
The [1.5 pre-flight](../release-1.5.md#scope-reopened-2026-08-14-what-a-question-cost) already hit that trap once, where keying on `recordEvent(` missed the two additions that record through `RecordEvent(`.

The scanner classifies each recorder call's reason argument into one of five forms, and fails anything it cannot place:

| Form | Treatment |
|---|---|
| String literal | An Event reason. Needs a ledger row. |
| Identifier that is a parameter of the enclosing function | A forwarder (`recordEvent`, `RecordEvent`, `Event`), not an emission site. Skipped. |
| Identifier assigned only from condition-reason constants in the enclosing function | Those condition reasons are re-emitted as Events. Covered by the condition ledger. |
| Selector ending in `.Reason` / `.reason` | A condition or queued-record pass-through. Covered by the condition ledger. |
| Anything else | A finding. The ledger cannot account for a reason nobody can name. |

Which argument holds the reason is read off the callee's own declaration: any function or interface method here taking both an `eventtype` and a `reason` is a recorder, and its parameter list gives the index.
That is not a refinement but a correction, and [§6](#6-what-the-walk-found) records what the tabulated version got wrong.
Only `Eventf`, declared in `client-go`, is stated by hand.
A call carrying `corev1.EventTypeWarning`/`EventTypeNormal` that matches no signature is a finding, so a new recorder cannot be added silently.

## 4. Tier classification

Same seam the metric gate uses, and the same one-sidedness.
A site under `internal/listener/` is classic-only; a site under `internal/scalesetlistener/` or in a `*_scaleset.go` file is scale-set-only; a shared file says nothing.
The check **refutes** a wrong single-tier row and never confirms a right one, because a reason emitted through an adapter interface writes no site in a tier-exclusive file.
The ledger row carries the positive claim, and the inventory check is what makes the row unavoidable.

## 5. Where the tier came from

The file heuristic answers only the tier-exclusive packages, which covers 21 of the 71 rows.
The rest sit in files both protocols execute, so the tier came from where the call sits relative to the protocol routing at `runnerset_controller.go:461`:

- Everything **before** it reaches both tiers: reference resolution, egress mode, the sidecar and worker-image readings, the sizing verdict and profile override, the reaper, and `applyWorkerCapacityConditions`, which the scale-set arm calls too (`runnerset_scaleset.go:188`).
- Everything **after** it is the classic arm alone: the installation-token fetch, the agent pool, and the multiplexer.
  That is why `TokenUnavailable`, `AgentProvisioningFailed` and `ListenerStartFailed` are classic-only.
  The scale-set arm hands the token manager to its listener rather than fetching at reconcile, and reports a start failure as `NoActiveSessions` with `ScaleSetListenerStartFailed` carrying the cause.
- A `RunnerGroup` only ever acquires classically, so a reason no v2 path writes is classic-only by construction.
  `CredentialAvailable` is the only row that rests on this alone.

## 6. What the walk found

**Two Events an operator can meet in `kubectl describe` had no runbook entry.** `AgentDeregistrationFailed` fires when a set being deleted cannot deregister its agent Secrets, holding the finalizer; `OrphanedWorkerRecovered` fires when a scale-set worker was already gone at AGC startup, so the disruption's cause was lost with the pod (Q844).
Both are now in the [job-lifecycle Events table](../../operations/troubleshooting.md#job-lifecycle-events-on-a-runnergroup--runnerset), and the gate's reference check is what keeps the next one from shipping the same way.
It is the condition/Event analogue of the Q809 metric that reached the ledger and nothing else.

**The first scanner was wrong in the way this repo keeps being wrong about extraction.** Keying the reason's argument index on the function name read the scale-set listener's action string `"ProvisionWorker"` as a reason, and missed `AssignmentAbandoned`, `JobProvisionStalled`, `WorkerCeilingReached` and `SessionUnauthorized`, because two methods here are called `recordEvent` and two are called `Event`, with the reason at a different index in each.
It was caught by checking the output against reasons the runbook already documented, not by the scan reporting anything.
The fix reads the index off the callee's own declaration, and `TestReasonIndexComesFromTheCalleesDeclaration` pins both directions.
This is the third instance of the same class in the 1.5 cycle: a CRD property scan that matched a wrapped godoc line, and an Event scan keyed on `recordEvent(` that missed the `RecordEvent(` sites ([release-1.5.md](../release-1.5.md#scope-reopened-2026-08-14-what-a-question-cost)).

**No condition reason was found reaching one tier by accident.** Every single-tier row is single-tier by design, and the two prose claims most at risk, `VersionTooOld` and `RunnerVersionTooOld`, were already correct: the condition *type* reaches both tiers, and only the classic tier's GitHub-rejection reason does not.
That is a narrower result than the metric walk's, which found two classic-only series on no list.
The metric surface had drifted because series are added one at a time; the reason vocabulary is declared in one shared file that `check-v2-api-sync.sh` already holds to both API versions, which is the likeliest explanation and is not a measurement.
