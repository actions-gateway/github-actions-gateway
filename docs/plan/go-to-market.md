# Go-to-market — adoption plan (OSS, non-commercial)

**What and why.** This is the adoption plan for GitHub Actions Gateway (GAG).
The single goal is **adoption** — real operators running it in real clusters — not revenue.
GAG is Apache-2.0 and is kept deliberately vendor-neutral with **no commercial roadmap** (no SaaS, no paid tier, no consulting commitment).
That is a deliberate posture, not an oversight: it keeps the project clean to **donate to an employer or a foundation** with nothing to "fight over profits" about, because there is no profit model attached.
Monetization is explicitly out of scope here; if it is ever revisited it is a separate decision with its own sign-off.

This plan is internal strategy and stays GitHub-only (it lives under `docs/plan/`, which `mkdocs.yml` excludes from the published site).
The *outward-facing* positioning it drives lives in [`index.md`](../index.md), [`alternatives.md`](../alternatives.md), and [`why-gag.md`](../why-gag.md); the evidence under all three is [`competitive-analysis-2026-08.md`](competitive-analysis-2026-08.md); the public site itself is tracked in [`website.md`](website.md) (Q129).

## Status at a glance

| Workstream | State |
|---|---|
| ICP + messaging priority defined | ✅ this doc |
| Demand evidence gathered (real ARC issues) | ✅ this doc, §3 |
| Competitive claims verified against a named version | ✅ [competitive-analysis-2026-08](competitive-analysis-2026-08.md), measured 2026-08-06 vs ARC 0.14.2 |
| Public site launched | ⏳ gated, see [website.md](website.md) (Q129) |
| ARC → GAG migration guide | ✅ [migration-from-arc](../operations/migration-from-arc.md) (Q199) |
| Comparison page tuned for search (§5.2) | ✅ [alternatives.md](../alternatives.md) |
| README problem-first rewrite | ❌ open |
| Seed channels (HN / forums / ARC issues) | ❌ not started, gated on site + 1.0 install path |
| Content pieces (blogs, comparison SEO) | ❌ not started |

---

## 1. Who actually has this problem (ICP)

The pains GAG solves are real and documented (see §3), but the audience is **specific** — narrower than the loudest self-hosted-runner complaints online.
Targeting precisely is the whole game.

**Primary ICP — the bullseye.** Platform / developer-experience teams who:

- run **self-hosted** GitHub Actions on a **shared, multi-tenant** Kubernetes cluster, and
- **must** self-host — driven by a hard constraint, not preference: compliance / data residency, an **IP allow-list** requirement (GitHub EMU or a firewalled internal service), or **on-prem / reserved GPU** capacity — and
- serve **multiple internal tenant teams** out of one cluster and are tired of being the ticket queue for every runner change.

These are the people for whom "just use a cheaper SaaS runner" is a non-answer.
Their realistic field today is ARC, or ARC plus a hand-built tenancy layer (quotas, NetworkPolicies, per-team egress) that is nobody's product and appears in no comparison.
That DIY composition, not ARC alone, is what GAG usually replaces.

**Secondary ICP.** GPU/ML platform teams paying for idle accelerators between jobs; regulated orgs combining EMU with per-team egress allow-lists.

**Explicitly NOT our audience.** Teams whose problem is "CI is slow / expensive" and who are happy running on a vendor's infrastructure.
That is the managed-SaaS lane (§2).
GAG does not compete there and should not pretend to.
Chasing that audience dilutes the message and invites a comparison GAG will lose (it is not a hosted speed play).
**Be honest about scope in all messaging** — it builds credibility with the people who *are* the ICP.

## 2. Competitive landscape

> Measured 2026-08-06 against ARC 0.14.2 and its `master` branch.
> The 83-entry sweep, the deep dives, and the criteria are in [competitive-analysis-2026-08](competitive-analysis-2026-08.md); the outward-facing router built from it is [alternatives.md](../alternatives.md).

There are **three** lanes, not two.
The earlier two-lane version of this section placed RunsOn in the managed lane, which was wrong and mattered: the axis that sorts this market is first **whose compute the job runs on**, and only then **what runs the control plane**.

| Lane | Who | Your compute? | Kubernetes? |
|---|---|---|---|
| Managed runner services | Blacksmith, Namespace, Depot, WarpBuild, Ubicloud, Tenki | no, the vendor's | n/a |
| Self-hosted without Kubernetes | [RunsOn](https://runs-on.com), [terraform-aws-github-runner](https://github.com/github-aws-runners/terraform-aws-github-runner), [Actuated](https://actuated.com) | yes: AWS, or your hardware with a vendor control plane (Actuated) | no |
| Self-hosted Kubernetes control plane | ARC, GAG, [ForgeMT](https://github.com/cisco-open/forge) | yes, any conformant cluster | yes |

**GAG competes in the third lane only, and ARC is not alone there.** ForgeMT (Cisco, 211 stars on 2026-08-12) attacks the same multi-tenant problem on AWS, drawing its tenant boundary in IAM and OIDC role scope, GitHub App scope, and runner labels, with an EC2 lane when a pod is not a strong enough boundary (its docs, read 2026-08-12).
Call ARC the **closest** competitor, never the only one; the research contradicts the stronger claim and the shipped alternatives page names the rest.

**Location is the filter that removes most of the field**, and no published comparison in this space applies it: "self-hosted" almost always means "self-hosted on AWS".
On-premises, air-gapped, or on reserved hardware, only ARC and GAG remain, by the same constraint that made the ICP self-host.
This is also why the AWS-native answer is not a universal one: every enforcement point it uses is an AWS API, its bare-metal equivalent is a hardware purchase, and a VM per job cannot draw from a pool that is already bought.

Implication: market GAG **as the multi-tenant answer in the self-hosted Kubernetes lane**, never as "another fast-CI option".
Naming the other two lanes honestly is not a concession, it is the router that makes the third lane's claims believable.
The [GitHub Actions self-hosted pricing backlash](https://github.com/orgs/community/discussions/182089) (the now-postponed control-plane fee) is a tailwind for self-hosting in general, but it pushes price-sensitive teams toward the SaaS lane, so do **not** over-index the messaging on it.

## 3. Demand evidence (the receipts)

Every load-bearing claim maps to currently-open, engaged ARC issues.
Use these in content, in issue comments, and in the comparison page: they turn assertions into proof and they are exactly what operators (and AI assistants) search for.

!!! warning "An open issue is a perishable receipt"

    Re-check state and date before citing.
    Two claims in the public comparison table went false at datable ARC releases with nobody noticing: 0.13.1 changed quota-blocked pod retry, and 0.14.0 added multi-label scale sets.
    The standing fix is the pre-flight step in [release.md](../operations/release.md#1-pre-flight); the failure mode is diagnosed in [competitive-analysis-2026-08](competitive-analysis-2026-08.md#why-the-marketing-drifted-and-the-fix).

| Claim | Evidence |
|---|---|
| Jobs stick in `Queued`, manual rerun the only fix | [ARC #4423](https://github.com/actions/actions-runner-controller/issues/4423), [#4203](https://github.com/actions/actions-runner-controller/issues/4203), [#4121](https://github.com/actions/actions-runner-controller/issues/4121) |
| OOM / eviction → zombie runner blocks new jobs | [ARC #4155](https://github.com/actions/actions-runner-controller/issues/4155), [#4307](https://github.com/actions/actions-runner-controller/issues/4307) |
| Spot/preemption → silent failure, no auto-retry (open ask) | [actions/runner #2530](https://github.com/actions/runner/discussions/2530), [community #160565](https://github.com/orgs/community/discussions/160565) |
| Multi-tenant ARC is operationally painful | [ARC #1832](https://github.com/actions/actions-runner-controller/discussions/1832), [#3176](https://github.com/actions/actions-runner-controller/discussions/3176), [community #161772](https://github.com/orgs/community/discussions/161772) |
| Per-team egress IP / allow-list demand is real and unmet | [community #26442](https://github.com/orgs/community/discussions/26442); third-party workarounds (QuotaGuard, Border0, Depot egress filtering) exist because GitHub has no first-class answer |

**Weakest demand signal:** priority-tiered GPU scheduling and listener-memory overhead are GAG's most *differentiated* features but have the least *public* complaint volume.
Treat them as supporting proof, not the headline.
Corroborated independently: of the 25 most-reacted open ARC issues, **zero** concern quota safety, capacity gating, or intake backpressure.
That cuts both ways.
The demand is quiet, and so is the pressure on ARC to close the gap.

## 4. Messaging priority

**Two tiers, deliberately.** The public site states only what ships today, dated and measurable.
This plan carries the argument the site is not yet entitled to make.
Keeping them apart is what lets the strategy aim at future dominance without the site over-claiming.

### Tier 1: what the site claims today

Lead with validated demand, in this order:

1. **Auto-recovery of evicted / quota-blocked jobs.** Strongest and best-evidenced.
   "Jobs recover themselves; no manual rerun."
   This is the wedge.
2. **Safe per-tenant quotas → real tenant self-service.** The platform-team pain.
   "Enforce a quota per team without stranding their jobs."
3. **Per-tenant isolated egress IPs.** The compliance/EMU unlock.
   "Allow-list just your runners, not the whole cluster."
4. **Supporting:** priority tiers (no starved critical jobs), zero idle GPU, lower listener memory.
   Real, but they ladder up to cost, so keep them as backup rather than the lead.

Frame every benefit against ARC, the **closest** competitor the ICP is weighing, and carry a measurement date on every competitor-side claim.
The absence of that date is exactly how 11 unverified cells shipped as red X's in the comparison table.

### Tier 2: the argument the strategy plays for

Internal until the deliverables land.
One thesis in three legs, and it outranks any feature list because answering it requires a tenancy model rather than a field.

**Leg 1: shared CI has stayed unsafe because CI is the hard case on both axes at once.** It needs root (image builds, Docker-in-Docker, `services:`, package installs), and it needs network isolation, which is hard on Kubernetes: policy enforcement is CNI-dependent, FQDN egress is CNI-specific, and CDN-fronted registries cannot be pinned by CIDR at all.
The honest statement is not "ARC lacks feature X".
It is "nobody has pushed this far enough yet, and here is how far we have got".

**Leg 2: GAG is making it safe.** Kata micro-VM workers give a job root without a shared kernel, already the default in GAG's own end-to-end suite.
Per-tenant egress plus default-deny NetworkPolicy, reconciled rather than hand-built, close the network half, including the path Kata does not close (a micro-VM does not change the pod's network identity, so cloud metadata still answers from inside the guest).
Still open and load-bearing: [Q408](../queue/Q408.md), [Q539](../queue/Q539.md), [Q540](../queue/Q540.md), [Q716](../queue/Q716.md), under the [secure multi-tenant OSS CI](secure-multi-tenant-oss-ci.md) umbrella.

**Leg 3: GAG keeps it operable.** Quota-aware intake and automatic re-run are consequences of leg 2, not standalone features, and should be presented that way.
Bin-packing tenants onto shared expensive nodes *requires* enforceable per-tenant quotas, and enforcing a quota is what strands jobs unless intake respects it before the claim.
Sharing capacity *requires* eviction and preemption to be safe, and they are safe only if a disrupted job re-runs itself.
Without both, secured multi-tenancy is an operations nightmare and the team retreats to a cluster per tenant, which is where §2's router started.

**Prioritization consequence:** Q408 and Q540 are not roadmap polish.
They complete the leg the entire argument rests on, so they outrank the proxy-polish set (Q564 to Q567) and most of the feature backlog.

### The persona frame, and how to say it without attacking

Multi-tenancy adds roles, not features.
ARC models the platform owner and the tenant as the same person, which is coherent for a single-owner cluster and is why it has no primitive separating them.
Every GAG differentiator is an artifact of a boundary between roles: a platform-owned quota the tenant cannot raise, a `ClusterRunnerTemplate` for privileged shapes the tenant cannot author, **two** dashboards because there are two operating personas.
Roles and their limits are documented in [personas.md](../operations/personas.md).

Do **not** ship "consumer grade versus enterprise grade".
It reads as an attack and is unfair to a product that is genuinely good at its job.
The shippable form:

> ARC is built for a cluster with one owner, and it is a reasonable choice there.
> GAG is built for a cluster with a platform team, tenant teams, and untrusted contributors: three roles with different powers, different blast radii, and different things they are allowed to see.

That is checkable, fair, and much harder to answer than any feature row.

### Which differentiators will still be differentiators

Messaging built on a contested differentiator has a shelf life.
Ratings and reasoning in [competitive-analysis-2026-08](competitive-analysis-2026-08.md#which-differentiators-are-durable).
The messaging consequence:

| Safe to build a campaign on | Supporting proof only |
|---|---|
| Automatic re-run after disruption | Quota-aware pre-claim intake |
| Tenant self-service via namespace CRs | Priority tiers with reserved floors |
| Secure defaults, reconciled not documented | Measured worker right-sizing |
| Sandboxed workers as a paved road | Goroutine-multiplexed listeners |
| Per-tenant provisioned egress | |

**The pre-claim seat is the trap.** ARC's listener already holds a Kubernetes clientset and `actions/scaleset` already exposes `SetMaxRunners`, so the seat is a permissions change away for anyone who wants it.
Claim the **signal** (live quota headroom computed and put in the capacity header), never the seat.
The one place it is structurally defensible is gang scheduling ([Q718](../queue/Q718.md)): a multi-node job needs N co-scheduled pods in one topology domain, and the protocol advertises capacity as a single integer, so no `SetMaxRunners` workaround exists for a placement predicate.

## 5. Channels — where to reach the ICP (ranked by fit)

**High-intent / overt:**

1. **The ARC issues and discussions in §3.** People land there from search *while in pain*.
   A genuine, non-spammy "we hit this; here's how GAG handles eviction recovery / multi-tenant isolation" is the single highest-intent placement that exists.
   One honest comment per relevant thread; never astroturf.
2. **The comparison pages, tuned for search.** ✅ Shipped as [alternatives.md](../alternatives.md) (the router: which lane are you in) and [why-gag.md](../why-gag.md) (capability by capability against ARC).
   Target terms: `actions-runner-controller multi-tenant`, `ARC jobs stuck queued`, `ARC GPU quota`, `self-hosted runner egress IP per team`.
   The "N best ARC alternatives" roundup format (BetterStack, Tenki, WarpBuild) drives this whole category and **ignores the self-hosted multi-tenant angle**, which stays an open gap.
   Remaining work is search tuning and inbound links, not the pages.
3. **`awesome-actions` / `awesome-runners` lists and the ARC docs' alternatives discussions** — low effort, durable, and frequently crawled by AI assistants.

**Credibility-building / subtle:**

4. **A deep technical blog post** in the lineage of the recognized ARC-at-scale authorities (some-natalie.dev, Ken Muse, Marcin Cuber on Medium).
   Cross-post to **DEV Community** (ranks well; cited by LLMs).
5. **Hacker News (Show HN), r/devops, r/kubernetes.** Right human audience for a platform-engineering OSS tool.
   (Note: Reddit blocks several crawlers, so it helps humans more than AI discoverability.)
6. **CNCF / Kubernetes ecosystem** — CNCF Slack, a KubeCon-adjacent lightning talk, or a Kubernetes-operator showcase.
   ARC and the operator pattern live here; the ICP is in the room.

## 6. AI discoverability (GEO)

Increasingly the ICP asks an LLM "why are my ARC jobs stuck?" or "multi-tenant self-hosted runners on Kubernetes?" before they search.
LLMs answer from: GitHub issues/discussions, DEV Community, Medium, vendor comparison blogs, and a few authority personal blogs — **GAG is in none of them today.** To enter the answer set:

- **Get into the comparison corpus** (§5.2/§5.3).
  Being listed in — or authoring — a linked "ARC alternatives" piece is how the project becomes citeable.
- **Title content with the literal problem phrasing**, not feature names: "recovering stuck GitHub Actions jobs after pod eviction," "multi-tenant self-hosted runners with isolated egress."
  Retrieval matches the question.
- **Make the repo itself retrievable.** `README.md` and `why-gag.md` are crawled; lead them with the problem ("ARC leaves jobs stuck when pods are evicted; can't isolate tenant egress") so a model connects the repo to the question.

## 7. Content plan (concrete artifacts)

| Artifact | Status / note |
|---|---|
| [`alternatives.md`](../alternatives.md) router page | ✅ Shipped 2026-08-06. Four cases where something else wins, the location filter, the persona boundaries, and where GAG loses with attribution. |
| [`why-gag.md`](../why-gag.md) "vs ARC" page | ✅ Exists. **Was not accurate**: 11 ARC-side cells were unverified and two had gone false at ARC 0.13.1 and 0.14.0. Corrected 2026-08-06 and now carries a dated measurement stamp. Re-measure per the [release pre-flight](../operations/release.md#1-pre-flight), not opportunistically. |
| **ARC → GAG migration guide** | ✅ [migration-from-arc](../operations/migration-from-arc.md) (Q199): concept mapping, egress differences, gotchas, and a worked one-runner-group path. Q726 (multi-label `runs-on`) closed 2026-08-11, so the guide no longer carries a workflow-edit caveat. |
| README problem-first rewrite | Open. Claims were corrected 2026-08-06; the structure was not. Lead with the ARC pain, not the architecture, and note GitHub renders no CSS so it needs tables and bold lead-ins rather than the site's components. |
| Blog: "Recovering stuck Actions jobs after pod eviction" | New. Maps to the strongest demand signal (§3). |
| Blog: "Multi-tenant self-hosted runners with isolated egress" | New. Maps to ICP + EMU allow-list. |
| Show HN post + r/devops post | New. Sequenced after site + 1.0 install path are solid. |

## 8. Launch sequence (phased)

- **Phase 0 — readiness (prerequisite).** Public site live ([website.md](website.md)/Q129); a copy-pasteable install path that works for an outside operator; README problem-first; ARC→GAG migration guide drafted.
  Do not seed channels before this — first impressions from cold traffic are one-shot.
  **GitHub Discussions stays off** in this phase: an empty forum on a pre-adoption, solo-maintained project with manually-driven (non-staffed) support reads as a ghost town, and slow replies look worse there than on Issues.
  It also buys little over Issues until there's enough volume to want Q&A/idea threads separated from bug tracking.
  So the README, site footer, and roadmap community links point to **Issues** for now (already enabled, free, slow-response-tolerant for a small project).
- **Phase 1 — seed.** Show HN + r/devops + r/kubernetes; begin honest, one-per-thread participation in the §3 ARC issues.
  Goal: first handful of **external** deployers and their issues/questions (the real adoption signal).
  **Enable GitHub Discussions here** and seed 2–3 starter threads (intro, roadmap feedback), then repoint the community links from Issues back to Discussions — its value only exceeds the ghost-town cost once traffic is actively flowing and someone is watching it.
- **Phase 2 — amplify.** Publish the two blog posts; land in awesome-lists and at least one "ARC alternatives" roundup; tune the comparison page for the §5.2 search terms; pursue a CNCF/KubeCon lightning talk.
- **Phase 3 — sustain.** Keep the comparison and receipts current as ARC evolves; keep answering in ARC issues.
  Re-measurement is a **release gate**, not a background task: the pre-flight step in [release.md](../operations/release.md#1-pre-flight) exists because the opportunistic version of this failed for two ARC releases running.

## 9. Adoption metrics (lightweight)

Track adoption, not vanity.
The signal that matters most is **external operators filing issues/questions** — it means someone is actually running it.

- GitHub stars / forks (weak, directional).
- **External issues & discussions opened by non-contributors** (strong — real usage).
- Helm chart / container image pulls (`ghcr.io`).
- Search/referral traffic to `why-gag.md` for ARC-comparison terms.
- Mentions in third-party "ARC alternatives" content and in AI assistant answers.

## 10. Governance / donation posture

This shapes everything above and is the reason monetization stays out:

- **Apache-2.0, vendor-neutral, no commercial roadmap.** Nothing in the project, branding, or docs implies a paid product.
  (Branding already scrubbed of franchise terms — see the logomark history.)
- **No CLA that assigns copyright to any company.** Prefer **DCO sign-off** so contributions stay community-owned and the project is cleanly donatable to an employer or a foundation later.
- **Keep the door closed on revenue for now.** If donations/consulting are ever considered, that is a separate, explicit decision — recording it here so the non-commercial stance is intentional and visible, not accidental.

## 11. Open follow-ups (feed the Queue when scheduled)

- ~~**ARC → GAG migration guide** (§7)~~ ✅ shipped as [migration-from-arc](../operations/migration-from-arc.md) (Q199).
- ~~**Competitive verification** (Q60)~~ ✅ done properly on 2026-08-06 and recorded in [competitive-analysis-2026-08](competitive-analysis-2026-08.md).
  Q60's own closure was a **false record**: the plan index called it "verified + folded into appendix-d", and the closing commit added 34 lines about Kueue and Exostellar with zero ARC per-claim verification.
  A row asserting verification that did not happen is worse than an open row.
- **README problem-first rewrite** (§6, §7).
  Cheap, improves both humans and GEO.
- **Public site launch** ([website.md](website.md)/Q129).
  Phase 0 prerequisite for any seeding.
- **Under-claims not yet fixed**: five capabilities reach `features.md` and no other surface, including the ten-PR durability programme whose motivating incident was five worker pods burning 82 spot node-hours.
  That is a maturity claim backed by artifacts, in exactly the register where §2 concedes maturity.
  Listed in [competitive-analysis-2026-08](competitive-analysis-2026-08.md#under-claims-not-yet-fixed).
