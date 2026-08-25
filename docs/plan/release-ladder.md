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
| **1.6** | The ARC-parity ports: Q719's RWX storage validation, shipped 2026-08-24 ([worker-shared-storage.md](../operations/worker-shared-storage.md)), then [Q727](../queue/Q727.md) the non-privileged `container:` path | release-1.6.md, written when 1.5 tags |
| **2.0** | v2 GA graduation and the three coupled removals: `v1alpha1`, `v2alpha1`, and classic acquisition | [v2-ga.md](v2-ga.md) |

## Why 1.6 exists rather than folding into 1.5

**Revised 2026-08-09.** This section originally rested on two capability drops that go permanent at `v2.0.0`, and one of them closed while the page was being written: [Q766](arc-parity.md) ported the abandoned-run recovery to the ScaleSet tier inside 1.4, so it is shipped rather than scheduled.
The surviving one was Q726, where `v1alpha1` put no ceiling on `runnerLabels` while `v2beta1` enforced exactly one and the godoc's workaround was to stay on a `v2alpha1` Classic set, which `v2.0.0` removes.
Closed 2026-08-11: `v2beta1` registers every label, so the escape hatch is no longer load-bearing.

So 1.6 is not the pre-2.0 repair slot any more.
What justifies it is Q719 and [Q727](../queue/Q727.md), which are the last two [ARC parity](arc-parity.md) gaps and cannot compress into 1.5: Q727 is `L` and strictly depends on Q719's `ReadWriteMany` validation, and 1.5 already carries three `M` items plus a marketing body of work.
Q719 closed on 2026-08-24, which settles the dependency and leaves 1.6 resting on Q727 alone.

The soak argument is unchanged and still favours a separate minor.
`v2-ga.md` Phase 1 requires no incompatible `v2beta1` shape change across at least two minors of real use; 1.4 and 1.5 are those two, so 1.6 lands the ports without restarting the clock.

Q719 has landed, so the rung now has contents.
If Q727 slips on demand, what 1.6 carries is one `M` item, which is a thin minor rather than an empty one, and that is the call to make when 1.5 tags.
That is the honest reading of a ladder whose middle rung exists to carry two specific items.

## What is punted past `v2.0.0`

Not scheduled, and that is a decision rather than an oversight.
Each waits on a real signal, and each carries a revive trigger on the backlog rather than a release:

| Waiting on | Items |
|---|---|
| Recorded demand from an operator | The proxy hardening cluster: [Q565](../queue/Q565.md) rate limiting, [Q566](../queue/Q566.md) in-cluster TLS, [Q567](../queue/Q567.md) per-group pools |
| An unbuilt prerequisite | [Q555](../queue/Q555.md) flaky-job retry, which needs a real job outcome |
| Hardware nobody has yet | [Q765](../queue/Q765.md) GHES validation on a real appliance |

Q564 was the seventh, revived on the same 2026-08-13 evidence and since shipped, so it has left this accounting and six of the original seven remain in it, which is the set the sentence below counts against.
Its demand was [Q725](../queue/Q725.md), which had sat in the Queue the whole time.

**One of the original six are back.** [Q408](../queue/Q408.md) waited on an operator ask plus a measurement, and its trigger fired by 2026-08-13: the maintainer is the operator asking for untrusted-PR CI.
That is the trigger list working rather than a rule being bent, and it is what narrowed the rule above.

The proxy cluster is the clearest case and the one most likely to be re-litigated.
[release-1.4.md](release-1.4.md) shelved all four together with the reasoning that they are a coherent release theme, and recorded in the same breath that **none had demand recorded against it**.
That still holds of the three left, and Q564 leaving on a recorded ask, then shipping, is the reasoning working rather than failing.
A theme with no demand is a theme, not a release.

## The rule this establishes

**The roadmap's near-term section means "not waiting on an outside signal".** An item that waits on demand, on an unbuilt prerequisite, or on hardware belongs in Deferred with a revive trigger, and surfaces on the roadmap under *Exploring / longer-term*.

This is enforceable rather than aspirational: `roadmapcheck` already binds each roadmap bullet to its backlog rows and requires a near-term bullet to name at least one Queue row.
Parking an item moves the row to Deferred, which moves the bullet, which the gate checks.

**Narrowed 2026-08-18.** This rule originally read *"committed to a named release"*, which is a stronger claim than the gate makes and than the ladder can keep.
The two coincide only while every ungated item is also parked, and a revive trigger firing breaks that: [Q408](../queue/Q408.md) and Q564 came back to the Queue on 2026-08-13 with their triggers fired, so rule 4 moved both bullets into near-term with no release to name (Q564 has since shipped, taking its bullet off the roadmap).
Restoring the stronger reading needs either an invented gate label, which publishes a commitment nobody made, or a re-parked row whose trigger has fired, which is the dishonesty this page exists to remove, pointed the other way.
The release commitment is the narrower claim the `X.Y-gate` label carries on its own, so near-term holds both gated and ungated work (Q843).

The failure mode to watch is the comfortable one: leaving an item in the Queue because parking it feels like giving up.
Deferred is not a graveyard here, it is a trigger list, and every row in it names the event that revives it.

## What this does not decide

**Applied 2026-08-09.** The seven punted items moved to Deferred with the triggers this page names (five of them still are), and Q719 and [Q727](../queue/Q727.md) took the `1.6-gate` label, which is what publishes the commitment where an adopter reads it.

That does not make 1.6 a decided release.
The labels encode the target, and the reading above still governs: if both items slip on demand, the labels come off rather than an empty tag being cut.
`release-1.6.md` gets written when 1.5 tags, on the same evidence the other release plans use.

The one hard constraint the ladder encoded — Q726 landing before `v2.0.0`, since 2.0 removes the `v2alpha1` escape hatch its godoc pointed at — is satisfied: it landed in 1.5.
Its `1.5-gate` label is what enforces that, a release earlier than strictly required.
