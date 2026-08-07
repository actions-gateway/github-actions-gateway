# Competitive analysis, August 2026

> **Status: research complete 2026-08-06, corrections shipped, several findings
> still unactioned.** This is the evidence behind the marketing changes made on
> that date. The decisions live in [release-1.4](release-1.4.md),
> [release-1.5](release-1.5.md), and
> [Appendix D.9–D.14](../design/appendix-d-alternatives-considered.md); this
> records what was measured, what it means, and what has not been done yet.

## Method, so it can be re-checked

Everything below was measured on **2026-08-06** against **ARC 0.14.2** (released
2026-05-22) and its `master` branch, by reading controller source, chart values,
release notes, and issue state rather than documentation. GitHub popularity came
from the API, not from prose.

Two measurements in the original research failed verification and were dropped
rather than used: a claimed assertion in the broker stub about rate-limit
headers, and a GitLab `retry_limits` example. **Treat any file-and-line citation
that cannot be reproduced as a lead, not a fact.**

## Which differentiators are durable

The most strategically useful output, and the one most likely to be wrong in six
months. Rated by what a competitor would have to change, not by how good the
feature is.

| Differentiator | Rating | Why |
|---|---|---|
| Automatic re-run after disruption | **Durable** | The re-run API is outside the runner-scale-set protocol, so ARC would need a second client and `actions: write` scope, which is a security decision rather than a feature. Disruption attribution took GAG several live measurement rounds; ARC is still fighting *stuck* runners (#4155, #4307, #4588 all open). A retry budget also needs a CRD surface `AutoscalingRunnerSet` does not have |
| Tenant self-service via namespace CRs | **Durable** | Requires a tenancy model, not a field |
| Secure defaults (PSA, default-deny NetworkPolicy) | **Durable** | Same reason |
| Per-tenant provisioned egress | **Durable as packaging** | The capability is assemblable; provisioning and reconciling it per tenant is the product |
| Sandboxed workers as a paved road | **Durable** | Not the `runtimeClassName` field, which both set. The validated reference architecture is the asset |
| Quota-aware pre-claim intake | **Contested, drifting temporary** | See below. Do not call this structural |
| Priority tiers with reserved floors | **Contested** | Per-index pod-spec variation in `EphemeralRunnerSet` is moderate work |
| Measured worker right-sizing | **Contested** | The stock-VPA argument in [D.7](../design/appendix-d-alternatives-considered.md#d7-worker-right-sizing-why-built-in-not-bolted-on) is sound, but an Alternative-4 webhook tool could exist |
| Goroutine-multiplexed listeners | **Contested**, low weight | Replicable; ARC's open #4169 asks for it |

### The pre-claim seat is not structural, and the docs must stop implying it is

This corrected an earlier conclusion in this same research, and it is the single
most important thing on this page.

1. **ARC's listener already holds a Kubernetes clientset.** `scaler.New()` builds
   one from `rest.InClusterConfig()`. It has the client and lacks the
   permissions: `rulesForListenerRole()` grants three rules, all `patch`, zero
   read verbs. Adding `get`/`list`/`watch` on `resourcequotas` and `pods` is a
   small change, not a redesign.
2. **`actions/scaleset` already exposes `Listener.SetMaxRunners(count)`**,
   documented as safe to call during `Run`. Nothing in ARC calls it.
3. **Open PR `actions/scaleset#113`** removes `acquireAvailableJobs` from the
   library and hands acquisition to the consumer, making selective acquisition a
   first-class extension point in GitHub's own client.

**What is not commoditising is the signal**: computing live quota headroom and
putting *that* number in the capacity header. Claim the measurement, never the
seat.

It also will not close soon. Of the 25 most-reacted open ARC issues, **zero**
concern quota safety, capacity gating, or intake backpressure.

**The exception, and the one place the seat is structurally defensible:**
gang scheduling. A multi-node job needs N co-scheduled pods in one topology
domain, and the scale-set protocol advertises capacity as a single integer. A
gang requirement is a placement predicate, not a count, so the `SetMaxRunners`
workaround does not exist for it. That is why [Q718](../STATUS.md#Q718) matters
beyond GPUs.

## What buyers actually evaluate on

A 31-criterion framework came out of the research. The subset worth keeping is
the part **no existing comparison in this space covers**, because that is both
the opening and the checklist for future positioning work:

- admission-time capacity arbitration: is a job claimed only if the cluster can
  place it, or claimed then retried;
- disruption recovery semantics: what happens to an in-flight job on eviction,
  preemption, drain, or spot reclaim;
- cross-tenant fairness: reserved floors, and whether one tenant's burst starves
  another;
- control-plane idle footprint per tenant and per runner shape;
- per-tenant egress identity, not just inbound reachability;
- credential blast radius and whether tenants share one GitHub API rate-limit
  budget;
- per-tenant observability partitioning and cost attribution;
- runner agent version currency against GitHub's enforced minimum;
- who manages the GitHub-side runner-group binding;
- cache locality and its egress bill;
- migration cost across the product's own API versions;
- support-scope *exclusions*, not just whether support exists;
- debug ergonomics after a failed job.

Criteria rated **decisive** where GAG is weak: install base and production
evidence, commercial support entitlement, and the container-job path. Those
decide real evaluations and no capability offsets them.

## Under-claims not yet fixed

Nine capabilities reach `features.md` and no other marketing surface. Five were
addressed on 2026-08-06 (workload identity, per-tenant egress validation,
Q683 fast-cancel, the `PriorityClassAllowlist` CR, and the paved-road framing).
These remain:

1. **The scale-set message-queue correctness body**
   (`design/02-architecture.md`): write-ahead conclusion persistence, deletes
   withheld until every named job concludes, drain-before-flush measured over 60
   stops at maximum pressure. Nothing sells it.
2. **The ten-PR durability programme** (Q435, Q438, Q583, Q606, Q436, Q550,
   Q547, Q497/Q459/Q502, Q603). The motivating incident was five worker pods
   running 16 hours on 82 spot node-hours. This is a *maturity* claim backed by
   artifacts, in exactly the register where the comparison page concedes
   maturity.
3. **Worker-quota footprint arithmetic** matching Kubernetes' own evaluator,
   pinned by envtest.
4. **The two disjoint PriorityClass allowlists** and their four-point
   disjointness enforcement.
5. **The GitHub protocol dependency register**, which is risk transparency a
   skeptical architect would value.

## Ideas deliberately rejected

Recorded so they are not re-proposed. All were generated as moat candidates and
killed on review:

- **Cross-tenant capacity arbitration** (a `SharedCapacityPool` CRD): a moat
  nobody asked for, L-sized, no demand evidence.
- **Scheduled or priced intake windows**: off-peak and spot routing is a cost
  play for an audience [go-to-market](go-to-market.md) §1 explicitly disclaims.
- **GitHub API budget instrumentation and throttling**: speculative
  instrumentation of a limit nobody has hit.
- **Job provenance classification before the worker exists**: its own evidence
  deflated the premise, since the backend auto-assigns on dotcom.
- **A signed per-job isolation record**: category-creating in theory, zero
  demand.
- **A time-boxed debug hold**: regresses a security property that
  `CLAUDE.md` makes non-negotiable.
- **Brokering `container:`/`services:` step pods through the AGC**: invents a
  bespoke protocol to reach parity with a capability ARC already ships. Kept as
  [Q727](../STATUS.md#Q727) framed as a *decision* rather than that design.

## Why the marketing drifted, and the fix

The comparison table's errors were not authoring mistakes. `#308` (2026-06-19)
converted a hedged prose comparison into a green-check/red-X **verdict table**,
and a verdict table requires a definite cell in every row. The working notes it
was built from had marked most competitor-side facts `VERIFY`. The format had
nowhere to put "we believe this but have not checked it", so **11 unverified
ARC-side facts shipped as red X's**, none of them citing an ARC version or a
measurement date.

Two went false at datable releases without anyone noticing: 0.13.1 changed
quota-blocked pod retry, and 0.14.0 added multi-label scale sets.

The structural fix is the dated measurement stamp now under the table, and the
recurring pre-flight step in
[release.md](../operations/release.md#1-pre-flight). Patching cells alone would
have reproduced the failure.

**A related process defect:** `plan/README.md` recorded Q60 as "verified +
folded into appendix-d". The Q60-closing commit added 34 lines about Kueue and
Exostellar and contained zero ARC per-claim verification. A row asserting
verification that did not happen is worse than an open row.

## Verification lessons

Two confident findings in this research were wrong, and both had the same shape:
a conclusion drawn from one surface without establishing what the author meant
or what the full surface said.

- **"No 48-hour figure exists"** came from fetching a single GitHub docs page.
  The figure was correctly sourced from the GHES usage-limits page when written;
  GitHub had since rewritten it.
- **"`cluster IP` is fabricated"** came from reading the term as
  `Service.spec.clusterIP`. The docs meant a pod IP from the cluster address
  space, which is true. The fix was terminology, not substance.

Both would have been caught by reading the claim's own surrounding prose first.
Every negative assertion here carries a positive control for that reason.

## Raw research artifacts

The full evidence lives under `tmp/compete/` in the worktree this was run from.
It is **gitignored and workstation-local**, so it survives a session but not a
fresh clone, and nobody else has it. This page is the distillation; those files
are the working notes behind it.

| File | Holds |
|---|---|
| `verified-findings.md` | the master record, sections A to M: corrections with evidence, provenance, under-claims, the moat cull, the durability correction |
| `sweep.json` | 83-entry landscape and the full 31-criterion evaluation framework |
| `dives.json` | deep dives on ARC, ForgeMT, the DIY composition and the AWS lane, plus the adversarial refutation of every published claim |
| `defensive.json` | claim provenance, shipped-capability delta, under-claims, self-misdescription |
| `moat.json` | 32 candidate backlog rows, 12 kept and 20 killed with reasoning |
| `distillation.json` | the full doc-tree distillation, six partitions |
| `arc_*.go`, `prow_reconciler.go`, `*_tree.txt` | the third-party source actually read, so a claim can be re-checked without re-fetching |

If those files are gone and a claim here needs re-verifying, the method section
above has the dates and versions to reproduce it against.

## Still open

- ~~`go-to-market.md` §2 and the two-tier positioning~~ ✅ reconciled 2026-08-06.
  §2 is now three lanes with the location filter, §4 carries both tiers (site
  claims versus the thesis the strategy plays for), and §11 records that Q60's
  closure was a false record.
- ~~`README.md` scannability pass~~ ✅ done 2026-08-06. It now leads with the
  measured numbers and an "Is this for you?" router, and both its problem and
  solution sections follow the validated messaging order rather than opening on
  priority tiers. One finding fell out of it:
  [Q728](../STATUS.md#Q728), since `check-release-pins.sh` reads any bare
  `X.Y.Z` in a pin-bearing doc as a GAG release pin, so the README cannot name
  the ARC version its comparison was measured against.
- ~~Caching and GPU umbrella goals~~ ✅ written 2026-08-07 as
  [caching-and-worker-storage](caching-and-worker-storage.md) and
  [gpu-and-accelerated-ci](gpu-and-accelerated-ci.md). Each surfaced a collision
  the individual rows do not state: closing untrusted-PR egress removes the
  `actions/cache` path that works today, and GPU plus Kata do not compose on
  cloud accelerator families, which lack nested virtualization. Both corrected a
  published claim in passing.

**Nothing from the 2026-08-06 research is unactioned.** Findings that became
work are on the Queue; findings that became positioning are in
[go-to-market](go-to-market.md) §2 and §4.
