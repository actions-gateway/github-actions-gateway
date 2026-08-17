# Competitive analysis, August 2026

> **Status: research complete 2026-08-06, corrections shipped, several findings still unactioned.** This is the evidence behind the marketing changes made on that date.
> The decisions live in [release-1.4](release-1.4.md), [release-1.5](release-1.5.md), and [Appendix D.9–D.14](../design/appendix-d-alternatives-considered.md); this records what was measured, what it means, and what has not been done yet.

## Method, so it can be re-checked

Everything below was measured on **2026-08-06** against **ARC 0.14.2** (released 2026-05-22) and its `master` branch, by reading controller source, chart values, release notes, and issue state rather than documentation.
GitHub popularity came from the API, not from prose.

Two measurements in the original research failed verification and were dropped rather than used: a claimed assertion in the broker stub about rate-limit headers, and a GitLab `retry_limits` example.
**Treat any file-and-line citation that cannot be reproduced as a lead, not a fact.**

## Which differentiators are durable

The most strategically useful output, and the one most likely to be wrong in six months.
Rated by what a competitor would have to change, not by how good the feature is.

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

This corrected an earlier conclusion in this same research, and it is the single most important thing on this page.

1. **ARC's listener already holds a Kubernetes clientset.** `scaler.New()` builds one from `rest.InClusterConfig()`.
   It has the client and lacks the permissions: `rulesForListenerRole()` grants two rules, both `patch`, zero read verbs.
   Adding `get`/`list`/`watch` on `resourcequotas` and `pods` is a small change, not a redesign.
   (This said three rules until 2026-08-12, when re-reading it at `9bb16ae` found two.
   Whether the function changed or the original count was wrong is not established, and does not move the load-bearing half, which is the zero read verbs.)
2. **`actions/scaleset` already exposes `Listener.SetMaxRunners(count)`**, documented as safe to call during `Run`.
   Nothing in ARC calls it.
3. **Open PR `actions/scaleset#113`** removes `acquireAvailableJobs` from the library and hands acquisition to the consumer, making selective acquisition a first-class extension point in GitHub's own client.

**What is not commoditising is the signal**: computing live quota headroom and putting *that* number in the capacity header.
Claim the measurement, never the seat.

It also will not close soon.
Of the 25 most-reacted open ARC issues, **zero** concern quota safety, capacity gating, or intake backpressure.

**The exception, and the one place the seat is structurally defensible:** gang scheduling.
A multi-node job needs N co-scheduled pods in one topology domain, and the scale-set protocol advertises capacity as a single integer.
A gang requirement is a placement predicate, not a count, so the `SetMaxRunners` workaround does not exist for it.
That is why [Q718](../queue/Q718.md) matters beyond GPUs.

## What buyers actually evaluate on

A 31-criterion framework came out of the research.
The subset worth keeping is the part **no existing comparison in this space covers**, because that is both the opening and the checklist for future positioning work:

- admission-time capacity arbitration: is a job claimed only if the cluster can place it, or claimed then retried;
- disruption recovery semantics: what happens to an in-flight job on eviction, preemption, drain, or spot reclaim;
- cross-tenant fairness: reserved floors, and whether one tenant's burst starves another;
- control-plane idle footprint per tenant and per runner shape;
- per-tenant egress identity, not just inbound reachability;
- credential blast radius and whether tenants share one GitHub API rate-limit budget;
- per-tenant observability partitioning and cost attribution;
- runner agent version currency against GitHub's enforced minimum;
- who manages the GitHub-side runner-group binding;
- cache locality and its egress bill;
- migration cost across the product's own API versions;
- support-scope *exclusions*, not just whether support exists;
- debug ergonomics after a failed job.

Criteria rated **decisive** where GAG is weak: install base and production evidence, commercial support entitlement, and the container-job path.
Those decide real evaluations and no capability offsets them.

## Under-claims not yet fixed

**The "nine" below never reconciled, and the list is the trustworthy half.** The headline said nine reach `features.md` and no other surface, then named five as addressed and listed five as remaining, which is ten.
[go-to-market](go-to-market.md) §"Under-claims not yet fixed" already carried five rather than nine.
Re-derived on 2026-08-12 against each page's own vocabulary rather than against the words this list uses, which is what the first pass got wrong: **three of the five reached no marketing surface at all, `features.md` included**, so "reach `features.md` and no other" was wrong in that direction too; and item 4 was already on `README.md`, `docs/index.md` and `features.md` before the pass started.
Q821 shipped items 1, 2 and 5 to `features.md` and `README.md`.
Item 3 is the only one still outstanding, and item 4 never was.

Five were addressed on 2026-08-06 (workload identity, per-tenant egress validation, Q683 fast-cancel, the `PriorityClassAllowlist` CR, and the paved-road framing).
The list as it stood:

1. **The scale-set message-queue correctness body** (`design/02-architecture.md`): write-ahead conclusion persistence, deletes withheld until every named job concludes, drain-before-flush measured over 60 stops at maximum pressure.
   Nothing sells it.
2. **The ten-PR durability programme** (Q435, Q438, Q583, Q606, Q436, Q550, Q547, Q497/Q459/Q502, Q603).
   The motivating incident was five worker pods running 16 hours on 82 spot node-hours.
   This is a *maturity* claim backed by artifacts, in exactly the register where the comparison page concedes maturity.
3. **Worker-quota footprint arithmetic** matching Kubernetes' own evaluator, pinned by envtest.
4. **The two disjoint PriorityClass allowlists** and their four-point disjointness enforcement.
5. **The GitHub protocol dependency register**, which is risk transparency a skeptical architect would value.

## Ideas deliberately rejected

Recorded so they are not re-proposed.
All were generated as moat candidates and killed on review:

- **Cross-tenant capacity arbitration** (a `SharedCapacityPool` CRD): a moat nobody asked for, L-sized, no demand evidence.
- **Scheduled or priced intake windows**: off-peak and spot routing is a cost play for an audience [go-to-market](go-to-market.md) §1 explicitly disclaims.
- **GitHub API budget instrumentation and throttling**: speculative instrumentation of a limit nobody has hit.
- **Job provenance classification before the worker exists**: its own evidence deflated the premise, since the backend auto-assigns on dotcom.
- **A signed per-job isolation record**: category-creating in theory, zero demand.
- **A time-boxed debug hold**: regresses a security property that `CLAUDE.md` makes non-negotiable.
- **Brokering `container:`/`services:` step pods through the AGC**: invents a bespoke protocol to reach parity with a capability ARC already ships.
  Kept as [Q727](../queue/Q727.md) framed as a *decision* rather than that design.

## Why the marketing drifted, and the fix

The comparison table's errors were not authoring mistakes.
`#308` (2026-06-19) converted a hedged prose comparison into a green-check/red-X **verdict table**, and a verdict table requires a definite cell in every row.
The working notes it was built from had marked most competitor-side facts `VERIFY`.
The format had nowhere to put "we believe this but have not checked it", so **11 unverified ARC-side facts shipped as red X's**, none of them citing an ARC version or a measurement date.

Two went false at datable releases without anyone noticing: 0.13.1 changed quota-blocked pod retry, and 0.14.0 added multi-label scale sets.

The first fix was a dated measurement stamp under the table, plus the recurring pre-flight step in [release.md](../operations/release.md#1-pre-flight).
That was not enough: one note for the whole column still renders a checked claim and an assumed one identically, so the format could go back to asserting things nobody measured with nothing to notice.
The structural fix is the per-cell stamp and its gate (Q801, 2026-08-12), documented in [documentation-standards.md](../development/documentation-standards.md#a-competitor-side-verdict-carries-its-own-stamp).
Patching cells alone would have reproduced the failure either way.

**A related process defect:** `plan/README.md` recorded Q60 as "verified + folded into appendix-d".
The Q60-closing commit added 34 lines about Kueue and Exostellar and contained zero ARC per-claim verification.
A row asserting verification that did not happen is worse than an open row: an open row invites the work, a falsely-closed one forecloses it.
The rule this produced is [maintaining-backlog.md § A completion note is a claim too](../development/maintaining-backlog.md#a-completion-note-is-a-claim-too-and-it-is-the-one-nobody-re-checks).

## Verification lessons

Two confident findings in this research were wrong, and both had the same shape: a conclusion drawn from one surface without establishing what the author meant or what the full surface said.

- **"No 48-hour figure exists"** came from fetching a single GitHub docs page.
  The figure was correctly sourced from the GHES usage-limits page when written; GitHub had since rewritten it.
- **"`cluster IP` is fabricated"** came from reading the term as `Service.spec.clusterIP`.
  The docs meant a pod IP from the cluster address space, which is true.
  The fix was terminology, not substance.

Both would have been caught by reading the claim's own surrounding prose first.
Every negative assertion here carries a positive control for that reason.

## Per-cell evidence for the ARC column, 2026-08-12

What each competitor-side cell of the [comparison table](../why-gag.md#gag-vs-arc-scale-set-mode) was read against, so a re-check starts from a file rather than from the claim.
Rows were read at ARC `gha-runner-scale-set` **0.14.2**, commit [`9bb16ae`](https://github.com/actions/actions-runner-controller/tree/9bb16ae49d0ce585d8e682aa7e2668a6e832d5d8), in two passes: the original column on **2026-08-12**, and the eight rows Q875 added on **2026-08-15**, marked in the table below.
The second pass re-ran two of the first pass's findings as a positive control before measuring anything new — `rulesForListenerRole`'s two `patch`-only rules and the `30 * time.Second` / `10 * time.Minute` quota cycle both reproduced exactly, which is what makes the rest of that pass's negatives worth reading.
0.14.2 was still the newest published chart on 2026-08-15, so both passes name the same version because nothing had shipped between them, not because the second pass reused the first's reading.
Blob links are pinned to that commit rather than to `master`, so the quoted lines stay at the cited coordinates (the [#1422](https://github.com/actions-gateway/github-actions-gateway/pull/1422) precedent).

The 2026-08-06 research above is the reasoning; this is the file-and-line layer under it, which is what [Q60's false closure](#why-the-marketing-drifted-and-the-fix) turned out not to have.

| Cell | Read at `9bb16ae` |
|---|---|
| Scale-set acquisition, ephemeral pods | `go.mod` names [`github.com/actions/scaleset v0.4.0`](https://github.com/actions/actions-runner-controller/blob/9bb16ae49d0ce585d8e682aa7e2668a6e832d5d8/go.mod), the same client library GAG acquires with; `EphemeralRunner` is the per-job object |
| Custom pod template and image, reusable templates | [`charts/gha-runner-scale-set/values.yaml`](https://github.com/actions/actions-runner-controller/blob/9bb16ae49d0ce585d8e682aa7e2668a6e832d5d8/charts/gha-runner-scale-set/values.yaml) takes one `template:` PodSpec per release, inline. No shared template object exists |
| Scale to zero, guaranteed floor | Same file: `maxRunners` and `minRunners` are per scale set, and `minRunners` is a warm-pool floor rather than a reservation another set cannot spend |
| Safe under a `ResourceQuota` | [`ephemeralrunner_controller.go`](https://github.com/actions/actions-runner-controller/blob/9bb16ae49d0ce585d8e682aa7e2668a6e832d5d8/controllers/actions.github.com/ephemeralrunner_controller.go): on `exceeded quota:` it requeues after `30 * time.Second`, and once the runner passes `CreationTimestamp + 10 * time.Minute` it deletes and re-creates it, spending a fresh single-use registration each cycle |
| Stop claiming when the cluster can't place | [`scaler.go`](https://github.com/actions/actions-runner-controller/blob/9bb16ae49d0ce585d8e682aa7e2668a6e832d5d8/cmd/ghalistener/scaler/scaler.go): `HandleDesiredRunnerCount` patches `EphemeralRunnerSet.spec.replicas` and consults no cluster state. `rulesForListenerRole` in [`resourcebuilder.go`](https://github.com/actions/actions-runner-controller/blob/9bb16ae49d0ce585d8e682aa7e2668a6e832d5d8/controllers/actions.github.com/resourcebuilder.go) grants two rules, both `patch`, so it could not read a quota if it wanted to |
| Auto-re-run | ARC's own client, [`github/actions/actions.go`](https://github.com/actions/actions-runner-controller/blob/9bb16ae49d0ce585d8e682aa7e2668a6e832d5d8/github/actions/actions.go), exposes no `/actions/runs` path, and no controller or client file calls a re-run endpoint. **The negative is a keyword-and-surface reading, not an exhaustive one**, so a mechanism under another name would not have been found |
| Anti-stampede throttling | [`charts/gha-runner-scale-set-controller/values.yaml`](https://github.com/actions/actions-runner-controller/blob/9bb16ae49d0ce585d8e682aa7e2668a6e832d5d8/charts/gha-runner-scale-set-controller/values.yaml): `flags.rateLimiter`, `k8sClientRateLimiterQPS`/`Burst`, `runnerMaxConcurrentReconciles`. All controller-wide, all metering reconciles and API calls |
| Per-tenant egress IPs | Scale-set `values.yaml` `proxy:` takes an `http`/`https`/`noProxy` URL plus a credential secret, for a proxy the operator already runs. Nothing provisions or reconciles a pool |
| App private key in the cluster | `newScaleSetListenerConfig` in `resourcebuilder.go` writes `config.AppConfig` into the listener's `config.json` Secret; `appconfig.FromSecret` reads `github_app_private_key`. With `vaultConfig` set, only the vault coordinates are written and the listener resolves the key itself |
| Listener footprint | Scale-set `values.yaml` `listenerTemplate` is "the PodSpec for each listener Pod"; [`autoscalinglistener_controller.go`](https://github.com/actions/actions-runner-controller/blob/9bb16ae49d0ce585d8e682aa7e2668a6e832d5d8/controllers/actions.github.com/autoscalinglistener_controller.go) mirrors its secrets "in the Controller namespace" |
| Per-tenant utilization metrics | Both charts ship the metrics blocks commented out, disabling metrics unless uncommented. [`cmd/ghalistener/metrics/metrics.go`](https://github.com/actions/actions-runner-controller/blob/9bb16ae49d0ce585d8e682aa7e2668a6e832d5d8/cmd/ghalistener/metrics/metrics.go) carries a `namespace` label and no quota series |
| Right-sizing | `AutoscalingRunnerSetStatus` in [`autoscalingrunnerset_types.go`](https://github.com/actions/actions-runner-controller/blob/9bb16ae49d0ce585d8e682aa7e2668a6e832d5d8/apis/actions.github.com/v1alpha1/autoscalingrunnerset_types.go) is five fields: a phase and four runner counts. No recommendation surface, and no `recommend`/`rightsize`/`vpa` path anywhere in the tree |
| Cross-tenant fleet health | One sample dashboard, `docs/gha-runner-scale-set-controller/samples/grafana-dashboard/`, and zero alert-rule files in the tree |
| Runner group binding *(2026-08-15)* | `runnerGroup` ships commented out in [`charts/gha-runner-scale-set/values.yaml`](https://github.com/actions/actions-runner-controller/blob/9bb16ae49d0ce585d8e682aa7e2668a6e832d5d8/charts/gha-runner-scale-set/values.yaml) and is templated into `AutoscalingRunnerSet.spec.runnerGroup`. In [`autoscalingrunnerset_controller.go`](https://github.com/actions/actions-runner-controller/blob/9bb16ae49d0ce585d8e682aa7e2668a6e832d5d8/controllers/actions.github.com/autoscalingrunnerset_controller.go), `GetRunnerGroupByName` failing **returns the error** rather than falling through to the `runnerGroupID := 1` default. This is parity, and the first pass had not looked |
| Multi-label sets *(2026-08-15)* | Same file: `RunnerScaleSetLabels` are added as `System` labels alongside the set name, deduplicated against it. Parity, and the row 0.14.0 made false before anyone noticed |
| Worker lifetime bound *(2026-08-15)* | [`resourcebuilder.go`](https://github.com/actions/actions-runner-controller/blob/9bb16ae49d0ce585d8e682aa7e2668a6e832d5d8/controllers/actions.github.com/resourcebuilder.go): `pod.Spec.ActiveDeadlineSeconds = tmpl.Spec.ActiveDeadlineSeconds` is the only occurrence in `apis/`, `controllers/` or `charts/` — a pass-through of what an operator wrote, never a derived or defaulted bound |
| Pod Security Admission *(2026-08-15)* | No `pod-security.kubernetes.io/*` label anywhere in `charts/gha-runner-scale-set` or `charts/gha-runner-scale-set-controller`. `securityContext` appears in those values only inside commented-out example blocks |
| Worker NetworkPolicy *(2026-08-15)* | No `kind: NetworkPolicy` in either scale-set chart. The scale-set chart's ten templates are the `AutoscalingRunnerSet`, the GitHub secret, the kube-mode role/binding/SA, the manager role/binding, and a no-permission SA. **This is an enumeration of the template set, not a keyword search**, so it is a stronger negative than the auto-re-run one above |
| Scale-set name collision *(2026-08-15)* | `autoscalingrunnerset_controller.go` looks a set up with `GetRunnerScaleSet(ctx, runnerGroupID, name)` and, when one exists, **reuses it** — the log line reads `Created/Reused a runner scale set`. No ownership, namespace or creator check gates the reuse, so two `AutoscalingRunnerSet`s in different namespaces naming one set in one group both bind to it |
| Control-plane availability *(2026-08-15)* | [`charts/gha-runner-scale-set-controller/values.yaml`](https://github.com/actions/actions-runner-controller/blob/9bb16ae49d0ce585d8e682aa7e2668a6e832d5d8/charts/gha-runner-scale-set-controller/values.yaml): `replicaCount: 1`, above a comment that leader election is enabled only when `replicaCount>1`. The chart's fourteen templates include no `PodDisruptionBudget` |
| Runner agent version currency *(2026-08-15)* | Scale sets are created with `RunnerSetting{DisableUpdate: true}`, so a pinned image is never self-updated. The `Outdated` phase in the same file tracks `EphemeralRunnerSet` spec drift, not the agent version, and no semver comparison against a GitHub-published minimum exists in `controllers/`, `cmd/` or `github/` |

Three things these passes did not settle, kept as claims rather than promoted to verdicts elsewhere on the page:

- the auto-re-run negative, above;
- the runner-agent-version negative from 2026-08-15, which is a keyword reading of `controllers/`, `cmd/` and `github/` in the same shape, so a check under another name would not have been found.
  The chart-level half of that row (`DisableUpdate: true`) is a positive reading and is solid;
- the three "[where ARC is ahead](../why-gag.md#where-arc-is-ahead)" bullets, which still carry the 2026-08-06 blanket date and no version.
  They concede rather than assert a gap, so they are not gated, but they age the same way.

## Raw research artifacts

The full evidence lives under `tmp/compete/` in the worktree this was run from.
It is **gitignored and workstation-local**, so it survives a session but not a fresh clone, and nobody else has it.
This page is the distillation; those files are the working notes behind it.

| File | Holds |
|---|---|
| `verified-findings.md` | the master record, sections A to M: corrections with evidence, provenance, under-claims, the moat cull, the durability correction |
| `sweep.json` | 83-entry landscape and the full 31-criterion evaluation framework |
| `dives.json` | deep dives on ARC, ForgeMT, the DIY composition and the AWS lane, plus the adversarial refutation of every published claim |
| `defensive.json` | claim provenance, shipped-capability delta, under-claims, self-misdescription |
| `moat.json` | 32 candidate backlog rows, 12 kept and 20 killed with reasoning |
| `distillation.json` | the full doc-tree distillation, six partitions |
| `arc_*.go`, `prow_reconciler.go`, `*_tree.txt` | the third-party source actually read, so a claim can be re-checked without re-fetching |

If those files are gone and a claim here needs re-verifying, the method section above has the dates and versions to reproduce it against.

## Still open

- ~~`go-to-market.md` §2 and the two-tier positioning~~ ✅ reconciled 2026-08-06. §2 is now three lanes with the location filter, §4 carries both tiers (site claims versus the thesis the strategy plays for), and §11 records that Q60's closure was a false record.
- ~~`README.md` scannability pass~~ ✅ done 2026-08-06.
  It now leads with the measured numbers and an "Is this for you?" router, and both its problem and solution sections follow the validated messaging order rather than opening on priority tiers.
  One finding fell out of it: [Q728](../queue/Q728.md), since `check-release-pins.sh` reads any bare `X.Y.Z` in a pin-bearing doc as a GAG release pin, so the README cannot name the ARC version its comparison was measured against.
- ~~Caching and GPU umbrella goals~~ ✅ written 2026-08-07 as [caching-and-worker-storage](caching-and-worker-storage.md) and [gpu-and-accelerated-ci](gpu-and-accelerated-ci.md).
  Each surfaced a collision the individual rows do not state: closing untrusted-PR egress removes the `actions/cache` path that works today, and GPU plus Kata do not compose on managed cloud (not for want of nested virtualization, which A2/A3/G2 have, but because the Kata passthrough path needs BIOS and host-driver control a node pool does not expose).
  Both corrected a published claim in passing.

**Nothing from the 2026-08-06 research is unactioned.** Findings that became work are on the Queue; findings that became positioning are in [go-to-market](go-to-market.md) §2 and §4.
