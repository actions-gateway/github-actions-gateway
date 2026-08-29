# Release 1.7 Milestone Definition

> **Status: the gating row is closed as of 2026-08-28.** Q408 built the in-cluster registry pull-through mirror, wired the job-side clients to it, deleted the e2e tenant's open-egress NetworkPolicy, and published the recipe as the supported posture; all five phases are done and the row is gone ([q408-untrusted-pr-egress.md](q408-untrusted-pr-egress.md)).
> It was the only `1.7-gate` row, so nothing now blocks the tag.
> What remains is release mechanics plus the one review below that runs at the candidate rather than before it: the API surface diff.
> The bump is measured rather than assumed, and it has moved since this scope opened: `semver-floor.sh v1.6.0` read six commits and **FLOOR: NONE** on 2026-08-26, and reads 45 commits and **FLOOR: MINOR** on 2026-08-28, set by five AGC changes that touch the released surface.
> Fifteen more carry a `feat` or `fix` subject and ship in no image and no chart, which is the gap between counting subjects and reading what a release contains, and Q408's own commits are all in that group: the deliverable is manifests, wiring and docs.

## Why this is a release rather than a row that lands whenever

[release-1.6.md § Why untrusted-PR CI on Kata is 1.7](release-1.6.md#why-untrusted-pr-ci-on-kata-is-17) recorded the split at the moment it was made, and it holds: Q727 was a migration blocker for a team leaving ARC, Q408 is a threat-model deliverable for a team running untrusted code.
Neither advances the other, because Kata already makes Docker-in-Docker unprivileged.

What makes the axis a release is Phase 5, not Phase 4.
The enforcement swap is a NetworkPolicy change in one overlay; the release is the sentence it retires.
"Kata-isolated runners are only suitable for trusted CI" appears in [kata-dind-workloads.md](../operations/kata-dind-workloads.md) as a caveat, and a tag is what lets an adopter cite the version it stopped being true in.

## The gating row: Q408, Phases 2 to 5

Phases and their validation are in [q408-untrusted-pr-egress.md § 4](q408-untrusted-pr-egress.md#4-phases) and are not restated here.
What this scope adds is the release's read of them.

| Phase | State | What the release needs from it |
|---|---|---|
| 0. Measured egress inventory | ✅ 2026-08-03 | The upstream set, corrected to five by Phase 1's grading |
| 1. Shrink the non-registry residual to GitHub | ✅ implemented 2026-08-05, validated 2026-08-24 | Nothing further; it is the evidence the residual is gone |
| 2. Mirror manifests | ✅ authored 2026-08-27, validated 2026-08-28 | `deploy/registry-mirror/`, one instance per upstream, applied from the e2e setup path |
| 3. Wiring | ✅ built and validated 2026-08-28 | A green Kata e2e run **with open egress still present** and mirror hit counts > 0, so wiring is proven before enforcement changes |
| 4. Enforcement | ✅ built and validated 2026-08-28 | The deletion plus the negative probes; this is where the posture becomes real |
| 5. Docs and close-out | ✅ 2026-08-28 | The caveat became a how-to; G.14 marked shipped; the roadmap bullet deleted and the capability moved to Features |

**Phases 2, 3 and 4 each needed a live dogfood session**, prod-guarded and operator-driven, and all three ran (2026-08-27 and 2026-08-28).
That was the schedule risk in this release and it was not reducible by planning: a phase whose validation is a booked cluster run cannot be compressed the way a code change can.
Phase 5 needed no cluster, so the release never waited on one after 2026-08-28.

**Phase 3 had to run with the image caches cold**, so the `quay.io` and `registry.k8s.io` prepulls were exercised rather than skipped.
A warm run would have reported hit counts that prove nothing about the paths a fresh tenant takes.
Nothing had to be arranged for it: Phase 1 removed every `actions/cache` step from the self-hosted lane, so that lane has been cold since, which is why Phase 4's run reported the same five instances serving too.

## The three questions 1.6 left for this scope

[release-1.6.md](release-1.6.md#why-untrusted-pr-ci-on-kata-is-17) named these rather than deciding them, on the ground that they were not that release's to settle.
Two are settled here; the third is a real conflict and is scoped as work.

### Q215's trigger has not fired, and the row now says which reading it takes

**Settled 2026-08-26.** [Q215](../queue/Q215.md)'s revive trigger is an in-cluster cache ask **or** Q408 Phase 1 landing and removing the working one, and 1.6 could not tell whether "the working one" meant GAG's own self-hosted lane or a tenant's.

It means a tenant's, and the row's own next sentence is what decides it: "`actions/cache` reaches its store today, so the gap is local."
That is a claim about what GAG offers a tenant, not about this repo's CI, and it is true: the tenant egress proxy's implicit GitHub allowlist carries `*.blob.core.windows.net`, the Azure-blob store the Actions cache runs on (`githubEgressFQDNs`, `cmd/gmc/internal/controller/egressproxy_fqdn.go`).
A tenant routed through the proxy reaches the cache today.

What Phase 1 removed was this repo's own self-hosted e2e lane, by gating every `actions/cache` step and `GHA_CACHE` to `runner.environment == 'github-hosted'` in [e2e-reusable.yml](../../.github/workflows/e2e-reusable.yml).
That is GAG dogfooding its own posture, not a tenant losing a capability, so the trigger's second clause is unfired and Q215 stays deferred.

The row has been rewritten to say so, because an ambiguous trigger is a standing query nobody can re-run, which is the failure mode [release-ladder.md](release-ladder.md#the-rule-this-establishes) names.

**One asymmetry is worth recording rather than leaving to be rediscovered.** The direct-egress form admits GitHub CIDRs, and the Azure blob store is not in them, so a tenant on that form has no working `actions/cache` now and never did.
The trigger reads on the proxy form because that is where the capability exists to lose.

### Q986 landed, and Definition of Done #5 is claimed

Q986 was the gap under [secure-multi-tenant-oss-ci.md](secure-multi-tenant-oss-ci.md) Definition of Done #5, a per-tenant and per-job record of which host each job reached.
Q564 shipped the egress audit record in 1.6 and attributes per pool, so the tenant half held only on an unshared pool and the job half held nowhere.

**It was admitted to 1.7 without gating the tag**, on the reasoning that its first step was a declined design rather than a build: Q564 had declined the client IP as a per-worker movement log nothing had weighed, and the row said neither half closed on a source identifier alone.

It shipped anyway, and the row was right that the identifier alone closes nothing — for a reason the row did not name.
A worker pod is deleted when its job ends, so a source address resolves to nothing after the fact and the binding has to be recorded live.
The answer is two opted-in records rather than one wider one: `EgressProxy.spec.auditLogging: ConnectionsWithSource` puts the client address on the egress record, and `ActionsGateway.spec.auditLogging: WorkerAddresses` has the AGC say which job holds each address while its pod lives.
Both default `Off`, because neither is a movement log alone and the join of the two is.
Scope and the rejected alternatives are in [q986-egress-attribution.md](q986-egress-attribution.md).

### The default-versus-opt-in conflict is real and is this release's to resolve

[secure-multi-tenant-oss-ci.md](secure-multi-tenant-oss-ci.md) Definition of Done #1 requires the isolated posture to be the default rather than an opt-in.
[runner-template-library.md § Nothing ships as a cluster default](../operations/runner-template-library.md#nothing-ships-as-a-cluster-default) declines exactly that, because a shipped default template would silently hand a privileged pod shape to sets that never asked for one.

**Resolved 2026-08-28, in the direction the evidence pointed at.** The distinction holds, and Phase 4 is what established it: enforcement shipped as the **deletion** of an additive allow-all policy, with no template change of any kind, so the network posture is what an operator gets by not acting while the pod shape stays a deliberate `templateRef`.
Definition of Done #1 is re-worded to say that, and [runner-template-library.md](../operations/runner-template-library.md#nothing-ships-as-a-cluster-default) stands unedited: its objection was to shipping a *template* as a default and nothing here asks for one.
The criterion now also records the decline, so a later reading cannot revive the demand without re-arguing the security case against it.

## Explicitly out of scope

- **[Q539](../queue/Q539.md) and [Q540](../queue/Q540.md).** Both are blocked behind Q408 by design: the mirror contract is validated on the simple implementation before Dragonfly and the composed stack are graded against it ([§6](q408-untrusted-pr-egress.md#6-follow-on-validations-q539-q540)).
  They are the follow-on validations of this release's deliverable, so they cannot also be in it.
- **[Q215](../queue/Q215.md) as a build.** Deferred with an unfired trigger, per the section above.
- **Controller or API changes.** Q408's scope statement is explicit that the GMC, the AGC and the CRDs are untouched: the deliverable is manifests, wiring, docs and live validation, published as the supported reference recipe.
  A 1.7 that grows an API field has lost the plot of what the release is.
- **The remaining proxy-hardening cluster.** [Q565](../queue/Q565.md), [Q566](../queue/Q566.md) and [Q567](../queue/Q567.md) keep their demand triggers, unchanged since [release-1.4.md](release-1.4.md#deferred-out-of-14-and-why) shelved them.

## Definition of done

1. **Q408 closed**, which by its own [§7](q408-untrusted-pr-egress.md#7-success-criteria) means the dogfood Kata e2e variant runs green with `e2e-open-egress` deleted, the negative probes confirm enforcement, and the Phase 5 docs make the recipe the supported posture.
2. **The trusted-CI caveat is gone** from [kata-dind-workloads.md](../operations/kata-dind-workloads.md), replaced by the reference architecture, with [security-operations.md](../operations/security-operations.md) linking the concrete manifests it has recommended in the abstract since before they existed.
3. **The default-versus-opt-in conflict is resolved** in whichever direction Phase 4's evidence supports, and the losing text is rewritten rather than left standing.
4. **G.14 and the roadmap reflect it**: [Appendix G.14](../design/appendix-g-future-enhancements.md#g14-kata-e2e-untrusted-pr-posture--tight-egress--in-cluster-pull-through-mirror) marked shipped, the [roadmap](../roadmap.md) entry moved out of "exploring".
5. **Definition of Done #5 claimed**, per the section above: Q986 landed, so the release claims a per-tenant and per-job record of which host each job reached rather than narrowing to an unshared pool.
6. **The API surface review**, from `scripts/release/api-surface-since.sh` over `v1.6.0..<rc commit>`.
   Q986 adds two additive fields to the v2 surface, so this release expects exactly those and nothing else: a `ConnectionsWithSource` value on `EgressProxy.spec.auditLogging`, and a new `ActionsGateway.spec.auditLogging` defaulting `Off`.
   Anything beyond them is a finding rather than a formality.
7. **Release mechanics**: a candidate tagged, artifacts verified, and the dogfood validation in [release.md](../operations/release.md) passing on the candidate that becomes the tag.

## Critical path

Phase 2 → Phase 3 → Phase 4 → Phase 5, strictly, because each phase's validation is the next one's precondition: manifests must serve before wiring can be proven to ride them, and wiring must be proven before enforcement can be distinguished from breakage.
Phases 2, 3 and 4 book dogfood sessions.
Nothing else in the release is on that path: Q986 ran beside it and has landed, and the docs decision resolves inside Phase 5.

## Pre-flight verdicts

Each verdict below names the commit it was measured at, because a verdict covers that commit and nothing later ([release.md](../operations/release.md#1-pre-flight)).
Re-run any whose window has moved before the stable tag.

| Check | Measured at | Verdict |
|---|---|---|
| Gating rows | `c54b712a9` | **PASS.** No `1.7-gate` row remains, and no `X.Y-gate` label survives anywhere in the store. The pattern was confirmed against a label that does exist before the empty result was trusted. |
| `main` green | `c54b712a9` | **PASS.** All nine required gates ran and passed on the SHA, none path-skipped, so no `check-artifact-unchanged.sh` proof is owed. |
| Semver floor | `c54b712a9` | **MINOR**, over 56 commits, set by eight touching the released surface. `v1.7.0` is forced by merged work rather than chosen. |
| API surface | `c54b712a9` | **PASS, ship as-is.** Exactly what [Definition of done #6](#definition-of-done) predicted and nothing else: one added wire field `auditLogging`, the `Off;Connections;ConnectionsWithSource` enum on `EgressProxy`, a new `Off;WorkerAddresses` enum on `ActionsGateway`, defaulting `Off`. No new condition types, Event reasons, labels or annotations. |

Both `auditLogging` fields default `Off` and are additive, so the shape is chosen rather than frozen by default: neither is a movement log alone, and only the join of the two is ([q986-egress-attribution.md](q986-egress-attribution.md)).

**Deferred to the stable tag, deliberately.** The marketing reconciliation, the operator-caveat pass, the roadmap and `features.md` reconciliation, and the three prose passes (`readability`, `deslop`, `semantic-remediation`) all bind when the text publishes, and a prerelease deploys no docs and generates rather than curates its Release body.

## Candidate validation

### `v1.7.0-rc.1`: FAILED, 2026-08-29

Tagged at `c54b712a9`, published and artifact-verified; the dogfood gate failed its capacity leg.

**Publish verified by content, not by green jobs.** 9 of 9 assets with `draft: false`, all eight signatures OK (six images, both charts), and the provenance digest reported `publish.yml@refs/tags/v1.7.0-rc.1` with `sourceRepositoryDigest` equal to the tag target.
That digest is the check that discriminates: signatures prove who built an artifact, and only the digest proves what was built.
A negative control against a wrong signer workflow exited 1, so the check could have failed.

| Leg | Result |
|---|---|
| e2e (Kata, mirrors) | **PASS**, 75/75 specs, 62 ok, 0 failed, 13 skipped |
| capacity (quota rung) | **FAIL**, bound correctly at zero headroom, did not release |
| Everything after | Not reached; the gate tore down |

**What failed.** The rung bound as designed (`withheldCapacity[quota]=2`, `advertisedCapacity=0`), and 300s after the ResourceQuota was restored to 6 pods it was still withholding 2.
The gate's own discriminator is whether the headroom came back rather than the rung.
It did, at `hard.pods=6` against `used.pods=1`, so this is not a namespace that had grown.

**Settled as a product defect (Q1035).** The two candidates were "the listener stopped advertising" (a gate defect, since the leg supplies no demand) and "the listener kept advertising and nothing published it" (a product defect).
Reading the controller answers it without the cluster: the poll loop recomputes the advertisement every long-poll and records it, and only a reconcile publishes it, but on an idle set every input to the scale-set branch's `RequeueAfter` is 0 (no worker pods to reap, no pending workers, and no projected proxy share to re-check).
The advertisement was therefore current and unpublished, and the only refresh available was controller-runtime's 10h default resync.
The fix wakes the reconciler from the poll loop when the advertisement changes, over the channel Q333 already built for listener-pushed conditions.
That closes the gate leg too: the leg was waiting on a refresh the controller had no path to make.

**Read the leg's newness as part of the finding.** Q637 added this leg on 2026-08-29, hours before the cut, so `rc.1` is the first candidate any quota-rung check has run against and it failed on its first outing.
That is the leg working: its first run found a real product defect.

**The host slept through the run.** It changed nothing about the verdict, but it changed how the run read.
`pmset` records maintenance sleeps from 08:25 to 08:56 with two 45-second dark wakes, so the gate's 300s poll budget, which counts `sleep` rather than wall clock, took 35 minutes of wall clock to spend.
A watcher reading elapsed time against a process-time budget saw a wedged gate for half an hour and recommended killing it.
The gate was polling normally and tore itself down cleanly once the host woke.
Confirmed at rest afterwards by asking the cluster rather than by reading the gate's own teardown line.
