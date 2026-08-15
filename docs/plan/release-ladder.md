# Release ladder to `v2.0.0`

> **Status: shape decided 2026-08-09.** How many releases stand between here and the v2 General Availability (GA) cut, what each carries, and what is deliberately not scheduled at all.
> Per-release detail lives in the individual plans; this page is the map between them.

## Why this exists

The backlog answers "what is next" and the individual release plans answer "what is in this one".
Neither answered **"how many more releases before `v2.0.0`, and what is punted past it"**, so the roadmap's near-term section accumulated work with no release behind it: on 2026-08-09 it listed twelve items, nine of which carried no release gate, under a heading that reads *"Work that is scoped and actively being built"*.

That is a page telling adopters nine things are in progress when they are waiting on demand, a prerequisite, or hardware nobody has.

## The ladder

| Release | Carries | Gate |
|---|---|---|
| **1.4** | Shipped scope: cross-namespace proxy sharing, the runner template library, v2 capacity gauges, the v1alpha1 apiserver warning, and the abandoned-run recovery | [release-1.4.md](release-1.4.md) |
| **1.5** | Q712 runner-group binding, Q713 default-tier latency series, and Q726 multi-label runner sets, all shipped, plus the marketing reconciliation | [release-1.5.md](release-1.5.md) |
| **1.6** | The ARC-parity ports: [Q719](../STATUS.md#Q719) RWX storage validation, then [Q727](../STATUS.md#Q727) the non-privileged `container:` path | release-1.6.md, written when 1.5 tags |
| **2.0** | v2 GA graduation and the three coupled removals: `v1alpha1`, `v2alpha1`, and classic acquisition | [v2-ga.md](v2-ga.md) |

## Why 1.6 exists rather than folding into 1.5

**Revised 2026-08-09.** This section originally rested on two capability drops that go permanent at `v2.0.0`, and one of them closed while the page was being written: [Q766](arc-parity.md) ported the abandoned-run recovery to the ScaleSet tier inside 1.4, so it is shipped rather than scheduled.
The surviving one was Q726, where `v1alpha1` put no ceiling on `runnerLabels` while `v2beta1` enforced exactly one and the godoc's workaround was to stay on a `v2alpha1` Classic set, which `v2.0.0` removes.
Closed 2026-08-11: `v2beta1` registers every label, so the escape hatch is no longer load-bearing.

So 1.6 is not the pre-2.0 repair slot any more.
What justifies it is [Q719](../STATUS.md#Q719) and [Q727](../STATUS.md#Q727), which are the last two [ARC parity](arc-parity.md) gaps and cannot compress into 1.5: Q727 is `L` and strictly depends on Q719's `ReadWriteMany` validation, and 1.5 already carries three `M` items plus a marketing body of work.

The soak argument is unchanged and still favours a separate minor.
`v2-ga.md` Phase 1 requires no incompatible `v2beta1` shape change across at least two minors of real use; 1.4 and 1.5 are those two, so 1.6 lands the ports without restarting the clock.

If Q719 and Q727 both slip on demand, 1.6 has no contents and should not be cut.
That is the honest reading of a ladder whose middle rung exists to carry two specific items.

## What is punted past `v2.0.0`

Not scheduled, and that is a decision rather than an oversight.
Each waits on a real signal, and each carries a revive trigger on the backlog rather than a release:

| Waiting on | Items |
|---|---|
| Recorded demand from an operator | The proxy hardening cluster: [Q564](../STATUS.md#Q564) audit logging, [Q565](../STATUS.md#Q565) rate limiting, [Q566](../STATUS.md#Q566) in-cluster TLS, [Q567](../STATUS.md#Q567) per-group pools |
| An operator ask plus measurement | [Q408](../STATUS.md#Q408) untrusted-PR egress isolation |
| An unbuilt prerequisite | [Q555](../STATUS.md#Q555) flaky-job retry, which needs a real job outcome |
| Hardware nobody has yet | [Q765](../STATUS.md#Q765) GHES validation on a real appliance |

The proxy cluster is the clearest case and the one most likely to be re-litigated.
[release-1.4.md](release-1.4.md) shelved all four together with the reasoning that they are a coherent release theme, and recorded in the same breath that **none has demand recorded against it**.
A theme with no demand is a theme, not a release.

## The rule this establishes

**The roadmap's near-term section means "committed to a named release".** An item with no release belongs in Deferred with a revive trigger, and surfaces on the roadmap under *Exploring / longer-term*.

This is enforceable rather than aspirational: `roadmapcheck` already binds each roadmap bullet to its backlog rows and requires a near-term bullet to name at least one Queue row.
Parking an item moves the row to Deferred, which moves the bullet, which the gate checks.

The failure mode to watch is the comfortable one: leaving an item in the Queue because parking it feels like giving up.
Deferred is not a graveyard here, it is a trigger list, and every row in it names the event that revives it.

## What this does not decide

**Applied 2026-08-09.** The seven punted items above moved to Deferred with the triggers this page names, and [Q719](../STATUS.md#Q719) and [Q727](../STATUS.md#Q727) now carry `1.6-gate`, which is what publishes the commitment where an adopter reads it.

That does not make 1.6 a decided release.
The labels encode the target, and the reading above still governs: if both items slip on demand, the labels come off rather than an empty tag being cut.
`release-1.6.md` gets written when 1.5 tags, on the same evidence the other release plans use.

The one hard constraint the ladder encoded — Q726 landing before `v2.0.0`, since 2.0 removes the `v2alpha1` escape hatch its godoc pointed at — is satisfied: it landed in 1.5.
Its `1.5-gate` label is what enforces that, a release earlier than strictly required.
