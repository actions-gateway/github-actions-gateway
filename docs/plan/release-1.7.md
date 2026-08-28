# Release 1.7 Milestone Definition

> **Status: scope opened 2026-08-26**, the day `v1.6.0` tagged, which is the trigger the [ladder](release-ladder.md) recorded for writing this doc.
> 1.7 is the untrusted-PR CI release: [Q408](../queue/Q408.md) Phases 2 to 5 build the in-cluster registry pull-through mirror, wire the job-side clients to it, and delete the e2e tenant's open-egress NetworkPolicy.
> Phase 1 validated 2026-08-24, so the non-registry residual is measured gone rather than argued away; what is left is registry egress.
> Q408 is the only `1.7-gate` row, and it is an `L` with a phased plan doc already written ([q408-untrusted-pr-egress.md](q408-untrusted-pr-egress.md)).
> The bump is unforced so far, and measured rather than assumed: `semver-floor.sh v1.6.0` reads six commits and reports **FLOOR: NONE**.
> Four of them carry a `feat` or `fix` subject and ship in no image and no chart, which is the gap between counting subjects and reading what a release contains.

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
| 2. Mirror manifests | ❌ | `deploy/registry-mirror/`, one instance per upstream, applied from the e2e setup path |
| 3. Wiring | ❌ | A green Kata e2e run **with open egress still present** and mirror hit counts > 0, so wiring is proven before enforcement changes |
| 4. Enforcement | 🔨 built 2026-08-28, validation pending | The deletion plus the negative probes; this is where the posture becomes real |
| 5. Docs and close-out | ❌ | The caveat flips to a how-to; G.14 marked shipped; the roadmap entry leaves "exploring" |

**Phases 2, 3 and 4 need live dogfood sessions**, which are prod-guarded and operator-driven.
That is the schedule risk in this release and it is not reducible by planning: a phase whose validation is a booked cluster run cannot be compressed the way a code change can.

**Phase 3 must run with the image caches cold**, so the `quay.io` and `registry.k8s.io` prepulls are exercised rather than skipped.
A warm run would report hit counts that prove nothing about the paths a fresh tenant takes.

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

### Q986 is in scope, and it is what Definition of Done #5 actually costs

[Q986](../queue/Q986.md) is the gap under [secure-multi-tenant-oss-ci.md](secure-multi-tenant-oss-ci.md) Definition of Done #5, a per-tenant and per-job record of which host each job reached.
Q564 shipped the egress audit record in 1.6 and attributes per pool, so the tenant half holds only on an unshared pool and the job half holds nowhere.

**It is admitted to 1.7 but does not gate the tag.** The reasoning is that an untrusted-PR claim without attribution is a claim about controls with no evidence behind them, which argues for the gate; and against it, that Q986's own row says neither half closes on a source identifier alone: tenant needs IP to namespace, job needs IP to pod to the AGC's mapping, and Q564 already declined the client IP as a per-worker movement log nothing has weighed.
A gate on a row whose first step is a declined design is a gate on a decision, not on a build.

So Q986 is ranked into the release and reviewed at the candidate: if it has landed, Definition of Done #5 is claimed; if it has not, the docs say which half of the record exists, and the claim stays narrowed to an unshared pool.
It takes no `1.7-gate` label, which keeps the label meaning "blocks the tag" rather than "belongs to the release".

### The default-versus-opt-in conflict is real and is this release's to resolve

[secure-multi-tenant-oss-ci.md](secure-multi-tenant-oss-ci.md) Definition of Done #1 requires the isolated posture to be the default rather than an opt-in.
[runner-template-library.md § Nothing ships as a cluster default](../operations/runner-template-library.md#nothing-ships-as-a-cluster-default) declines exactly that, because a shipped default template would silently hand a privileged pod shape to sets that never asked for one.

Both are defensible and they cannot both stand as written, so 1.7 rewrites one of them.
The resolution is not pre-empted here, but the shape the evidence points at is worth stating: the library's objection is to shipping a *template* as a default, and the goal doc's requirement is that the *posture* be the default, which is a NetworkPolicy and a mirror rather than a pod shape.
If that distinction holds, both survive with the goal doc's criterion re-worded to name the posture; if it does not, the criterion is the one that changes, because the library's rationale is a security argument and the criterion is an aspiration.

**This is a docs decision with a gate attached, not a build**, and it belongs in Phase 5 rather than ahead of it, because Phase 4 is what establishes whether the posture is deployable without a template change at all.

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
5. **Q986 reviewed at the candidate**, per the section above: landed and claimed, or not landed and the docs narrowed to what the record actually attributes.
6. **The API surface review**, from `scripts/release/api-surface-since.sh` over `v1.6.0..<rc commit>`.
   This release expects it to be empty, which makes a non-empty result a finding rather than a formality.
7. **Release mechanics**: a candidate tagged, artifacts verified, and the dogfood validation in [release.md](../operations/release.md) passing on the candidate that becomes the tag.

## Critical path

Phase 2 → Phase 3 → Phase 4 → Phase 5, strictly, because each phase's validation is the next one's precondition: manifests must serve before wiring can be proven to ride them, and wiring must be proven before enforcement can be distinguished from breakage.
Phases 2, 3 and 4 book dogfood sessions.
Nothing else in the release is on that path: Q986 runs beside it, and the docs decision resolves inside Phase 5.
