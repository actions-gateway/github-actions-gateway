# Release 1.10 Milestone Definition

> **Status: scoped 2026-10-09, no candidate cut.** The rung was decided by the maintainer on 2026-10-09, when scoping 2.0 found that the storage advance cannot share a CustomResourceDefinition apply with the removals.
> Five rows gate the tag and three ride.
> Nothing here is a commitment to a date.

## Why this release exists

`v2.0.0` removes `v2beta1`, `v2alpha1`, `v1alpha1` and classic acquisition, and every object an operator has stored must be rewritten as `v2` first.
The apiserver will not remove a version from a CRD's `spec.versions` while `status.storedVersions` still lists it, and every 1.x cluster lists `v2beta1` there, so the rewrite has to finish in a state where `v2` is storage and every version is still served.
This release is that state: it marks `v2` the storage version and ships the sweep that rewrites stored objects and prunes `storedVersions` to `["v2"]`, and `v2.0.0` is then removals only, in one apply.

The alternative was a two-phase `v2.0.0` upgrade, apply, sweep, apply, inside one release.
It saved a release and made the one breaking upgrade a procedure an operator can stop halfway through.
Full reasoning: [release-ladder.md](release-ladder.md#why-110-exists-the-storage-advance-cannot-share-an-apply-with-the-removals), and the contract's source in [v2-ga.md](v2-ga.md#the-storage-advance-needs-a-state-between-two-applies).

Rule #4b is not what forces it: `v1.9.0` already served `v2` beside `v2beta1`, which is what allows the storage version to advance here at all.
Rollback from this release reaches `v1.9.0`, which serves `v2` and so reads what this release stored.

## What this release must NOT do

**It must not stop serving any version.** `v1alpha1`, `v2alpha1` and `v2beta1` stay served, so every client and every stored object an operator has keeps working.
Dropping one is [Q1167](../queue/Q1167.md)'s and [Q273](../queue/Q273.md)'s, in `v2.0.0`, after this tag.

**It must not remove classic acquisition.** That is [Q264](../queue/Q264.md), also in `v2.0.0`.

## Scope ledger

Five gating rows and the candidate validation, then the three items that ride.

| Q-ID | Item | Gates? | Status |
|---|---|---|---|
| [Q1086](../queue/Q1086.md) | Mark `v2` the storage version, rewrite every stored object with a read-write sweep, and prune `storedVersions` to `["v2"]` | `1.10-gate` | 🔲 ready |
| [Q1152](../queue/Q1152.md) | Scale-set recovery ties a job to the runner that took it, not the worker created for it | `1.10-gate` | 🔲 ready |
| [Q1153](../queue/Q1153.md) | Reclaim the idle scale-set worker a cancelled job leaves behind | `1.10-gate` | 🔲 ready |
| [Q1154](../queue/Q1154.md) | Reclaim the JIT-config Secret of a worker reaped before its job completes | `1.10-gate` | 🔲 ready |
| [Q1069](../queue/Q1069.md) | Per-`RunnerSet` egress-audit attribution condition and gauge | `1.10-gate` | 🔲 ready |
| — | RC validated on dogfood | gates | 🔲 no candidate cut |
| [Q1070](../queue/Q1070.md) | Security-dashboard panel keyed on Q1069's per-set gauge | rides | 🔲 ready, with an open question on what the panel plots |
| [Q1066](../queue/Q1066.md) | Registry read presents the worker ServiceAccount's pull secrets, then node-identity credentials for Artifact Registry and ECR | rides | 🔲 ready, staged |
| [Q540](../queue/Q540.md) | Validate Kata with Dragonfly at the node: the P2P mesh stays unreachable from worker pods | rides | 🔲 ready |

**The split is the maintainer's, chosen 2026-10-09; the reasons below are this plan's, offered with it.** The theme is in the maintainer's own words: the storage advance plus scale-set correctness.
Q1086 is what the release is for.
Q1152, Q1153 and Q1154 are defects on the scale-set tier, the one `v2.0.0` keeps as the only tier: recovery that re-runs the wrong run, a worker that holds a node for up to 12 hours, and a credential-bearing Secret that outlives its worker.
Q1152 and Q1154 were found reviewing Q1151's fix, and all three follow from the same fact, that GitHub hands a scale-set job to whichever runner asks first.
Q1069 gates as the one feature: the gateway-level attribution flag reads `0` where a set's traffic leaves through a pool that logs no source address, the wrong direction for an audit signal.

**The riders are planned and worked, and the tag does not wait for them.** Each can come back short for a reason a gate should not absorb.
Q540 is a measurement whose answer may be a fix of unknown size: [Q539](../plan/q539-dragonfly-mirror-backend.md#8-what-this-plan-does-not-cover) measured Dragonfly's node proxy as an open forward proxy.
Q1066's second stage adds a cloud dependency and an identity grant per provider, and its first stage ships alone if the second slips.
Q1070 depends on Q1069 and carries an open question of the maintainer's.

`scripts/release/semver-floor.sh v1.9.0` read `FLOOR: NONE` on 2026-10-09 over two commits, so nothing merged yet rides beyond the three above.
The ledger takes a row for each further item that merges onto the released surface before the tag.

## What the candidate must show

The dogfood gate validates every candidate; this one also has to show the storage advance completed on a cluster that has run 1.x.
Q1086's deliverable is that evidence, and the candidate is where it is read: every one of the five kinds' CRDs reports `storedVersions: ["v2"]` after the upgrade, on the dogfood cluster, whose objects are stored as `v2beta1` today.
