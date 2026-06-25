# Go-to-market — adoption plan (OSS, non-commercial)

**What and why.** This is the adoption plan for GitHub Actions Gateway (GAG). The
single goal is **adoption** — real operators running it in real clusters — not
revenue. GAG is Apache-2.0 and is kept deliberately vendor-neutral with **no
commercial roadmap** (no SaaS, no paid tier, no consulting commitment). That is a
deliberate posture, not an oversight: it keeps the project clean to **donate to an
employer or a foundation** with nothing to "fight over profits" about, because
there is no profit model attached. Monetization is explicitly out of scope here;
if it is ever revisited it is a separate decision with its own sign-off.

This plan is internal strategy and stays GitHub-only (it lives under
`docs/plan/`, which `mkdocs.yml` excludes from the published site). The
*outward-facing* positioning it drives lives in [`why-gag.md`](../why-gag.md) and
[`index.md`](../index.md); the per-claim verification work feeds
[`competitive-analysis.md`](competitive-analysis.md) (Q60); the public site
itself is tracked in [`website.md`](website.md) (Q129).

## Status at a glance

| Workstream | State |
|---|---|
| ICP + messaging priority defined | ✅ this doc |
| Demand evidence gathered (real ARC issues) | ✅ this doc + [competitive-analysis](competitive-analysis.md) |
| Public site launched | ⏳ gated — see [website.md](website.md) (Q129) |
| ARC → GAG migration guide | ✅ [migration-from-arc](../operations/migration-from-arc.md) (Q199) |
| README problem-first rewrite | ❌ open |
| Seed channels (HN / forums / ARC issues) | ❌ not started — gated on site + 1.0 install path |
| Content pieces (blogs, comparison SEO) | ❌ not started |

---

## 1. Who actually has this problem (ICP)

The pains GAG solves are real and documented (see §3), but the audience is
**specific** — narrower than the loudest self-hosted-runner complaints online.
Targeting precisely is the whole game.

**Primary ICP — the bullseye.** Platform / developer-experience teams who:

- run **self-hosted** GitHub Actions on a **shared, multi-tenant** Kubernetes
  cluster, and
- **must** self-host — driven by a hard constraint, not preference:
  compliance / data residency, an **IP allow-list** requirement (GitHub EMU or a
  firewalled internal service), or **on-prem / reserved GPU** capacity — and
- serve **multiple internal tenant teams** out of one cluster and are tired of
  being the ticket queue for every runner change.

These are the people for whom "just use a cheaper SaaS runner" is a non-answer,
and for whom ARC is the only real option today — and a painful one.

**Secondary ICP.** GPU/ML platform teams paying for idle accelerators between
jobs; regulated orgs combining EMU with per-team egress allow-lists.

**Explicitly NOT our audience.** Teams whose problem is "CI is slow / expensive"
and who are happy running on a vendor's infrastructure. That is the managed-SaaS
lane (§2). GAG does not compete there and should not pretend to. Chasing that
audience dilutes the message and invites a comparison GAG will lose (it is not a
hosted speed play). **Be honest about scope in all messaging** — it builds
credibility with the people who *are* the ICP.

## 2. Competitive landscape

There are two distinct markets; conflating them is the most common positioning
mistake.

- **The managed-SaaS lane (speed + cost):** WarpBuild, Blacksmith, Namespace,
  Depot, Ubicloud, Tenki, RunsOn. They compete on faster builds at lower cost;
  with the exception of RunsOn (BYO-AWS), **your code and secrets run on their
  infrastructure.** GAG is out of scope here — different axis entirely.
- **The self-hosted control-plane lane (governance + isolation):** ARC (free,
  official, Kubernetes-native) and GAG. **ARC is GAG's only direct competitor.**
  It is widely deployed and genuinely painful for the multi-tenant ICP — which is
  the opening.

Implication: market GAG **as the better ARC for multi-tenant self-hosting**, never
as "another fast-CI option." The recent
[GitHub Actions self-hosted pricing backlash](https://github.com/orgs/community/discussions/182089)
(the now-postponed control-plane fee) is a tailwind for self-hosting in general,
but it pushes price-sensitive teams toward the SaaS lane — do **not** over-index
the messaging on it.

## 3. Demand evidence (the receipts)

Every load-bearing claim maps to currently-open, engaged ARC issues. Use these in
content, in issue comments, and in the comparison page — they turn assertions into
proof and they are exactly what operators (and AI assistants) search for.

| Claim | Evidence |
|---|---|
| Jobs stick in `Queued`, manual rerun the only fix | [ARC #4423](https://github.com/actions/actions-runner-controller/issues/4423), [#4203](https://github.com/actions/actions-runner-controller/issues/4203), [#4121](https://github.com/actions/actions-runner-controller/issues/4121) |
| OOM / eviction → zombie runner blocks new jobs | [ARC #4155](https://github.com/actions/actions-runner-controller/issues/4155), [#4307](https://github.com/actions/actions-runner-controller/issues/4307) |
| Spot/preemption → silent failure, no auto-retry (open ask) | [actions/runner #2530](https://github.com/actions/runner/discussions/2530), [community #160565](https://github.com/orgs/community/discussions/160565) |
| Multi-tenant ARC is operationally painful | [ARC #1832](https://github.com/actions/actions-runner-controller/discussions/1832), [#3176](https://github.com/actions/actions-runner-controller/discussions/3176), [community #161772](https://github.com/orgs/community/discussions/161772) |
| Per-team egress IP / allow-list demand is real and unmet | [community #26442](https://github.com/orgs/community/discussions/26442); third-party workarounds (QuotaGuard, Border0, Depot egress filtering) exist because GitHub has no first-class answer |

**Weakest demand signal:** priority-tiered GPU scheduling and listener-memory
overhead are GAG's most *differentiated* features but have the least *public*
complaint volume. Treat them as supporting proof, not the headline. (Cross-ref the
open VERIFY items in [competitive-analysis](competitive-analysis.md).)

## 4. Messaging priority

Lead with validated demand, in this order:

1. **Auto-recovery of evicted / quota-blocked jobs** — strongest and best-evidenced.
   "Jobs recover themselves; no manual rerun." This is the wedge.
2. **Safe per-tenant quotas → real tenant self-service** — the platform-team pain.
   "Enforce a quota per team without stranding their jobs."
3. **Per-tenant isolated egress IPs** — the compliance/EMU unlock. "Allow-list
   just your runners, not the whole cluster."
4. **Supporting:** priority tiers (no starved critical jobs), zero idle GPU,
   lower listener memory. Real, but they ladder up to cost — keep them as backup,
   not the lead.

Frame every benefit against ARC, the only competitor the ICP is weighing.

## 5. Channels — where to reach the ICP (ranked by fit)

**High-intent / overt:**

1. **The ARC issues and discussions in §3.** People land there from search *while
   in pain*. A genuine, non-spammy "we hit this; here's how GAG handles eviction
   recovery / multi-tenant isolation" is the single highest-intent placement that
   exists. One honest comment per relevant thread; never astroturf.
2. **A "GAG vs ARC for multi-tenant" comparison page tuned for search** —
   ranking for `actions-runner-controller multi-tenant`, `ARC jobs stuck queued`,
   `ARC GPU quota`, `self-hosted runner egress IP per team`. The "N best ARC
   alternatives" roundup format (BetterStack, Tenki, WarpBuild) drives this whole
   category and **ignores the self-hosted multi-tenant angle** — an open gap.
3. **`awesome-actions` / `awesome-runners` lists and the ARC docs' alternatives
   discussions** — low effort, durable, and frequently crawled by AI assistants.

**Credibility-building / subtle:**

4. **A deep technical blog post** in the lineage of the recognized ARC-at-scale
   authorities (some-natalie.dev, Ken Muse, Marcin Cuber on Medium). Cross-post to
   **DEV Community** (ranks well; cited by LLMs).
5. **Hacker News (Show HN), r/devops, r/kubernetes.** Right human audience for a
   platform-engineering OSS tool. (Note: Reddit blocks several crawlers, so it
   helps humans more than AI discoverability.)
6. **CNCF / Kubernetes ecosystem** — CNCF Slack, a KubeCon-adjacent lightning
   talk, or a Kubernetes-operator showcase. ARC and the operator pattern live
   here; the ICP is in the room.

## 6. AI discoverability (GEO)

Increasingly the ICP asks an LLM "why are my ARC jobs stuck?" or "multi-tenant
self-hosted runners on Kubernetes?" before they search. LLMs answer from: GitHub
issues/discussions, DEV Community, Medium, vendor comparison blogs, and a few
authority personal blogs — **GAG is in none of them today.** To enter the answer
set:

- **Get into the comparison corpus** (§5.2/§5.3). Being listed in — or authoring —
  a linked "ARC alternatives" piece is how the project becomes citeable.
- **Title content with the literal problem phrasing**, not feature names:
  "recovering stuck GitHub Actions jobs after pod eviction," "multi-tenant
  self-hosted runners with isolated egress." Retrieval matches the question.
- **Make the repo itself retrievable.** `README.md` and `why-gag.md` are crawled;
  lead them with the problem ("ARC leaves jobs stuck when pods are evicted; can't
  isolate tenant egress") so a model connects the repo to the question.

## 7. Content plan (concrete artifacts)

| Artifact | Status / note |
|---|---|
| `why-gag.md` "vs ARC" page | Exists and accurate; keep claims and receipts current as Q60 verifies. |
| **ARC → GAG migration guide** | ✅ [migration-from-arc](../operations/migration-from-arc.md) (Q199) — concept mapping, egress differences, gotchas, and a worked one-runner-group path. |
| README problem-first rewrite | Open. Lead with the ARC pain, not the architecture. |
| Blog: "Recovering stuck Actions jobs after pod eviction" | New. Maps to the strongest demand signal (§3). |
| Blog: "Multi-tenant self-hosted runners with isolated egress" | New. Maps to ICP + EMU allow-list. |
| Show HN post + r/devops post | New. Sequenced after site + 1.0 install path are solid. |

## 8. Launch sequence (phased)

- **Phase 0 — readiness (prerequisite).** Public site live ([website.md](website.md)/Q129);
  a copy-pasteable install path that works for an outside operator; README
  problem-first; ARC→GAG migration guide drafted. Do not seed channels before this
  — first impressions from cold traffic are one-shot. **GitHub Discussions stays
  off** in this phase: an empty forum on a pre-adoption, solo-maintained project
  with manually-driven (non-staffed) support reads as a ghost town, and slow
  replies look worse there than on Issues. It also buys little over Issues until
  there's enough volume to want Q&A/idea threads separated from bug tracking. So
  the README, site footer, and roadmap community links point to **Issues** for
  now (already enabled, free, slow-response-tolerant for a small project).
- **Phase 1 — seed.** Show HN + r/devops + r/kubernetes; begin honest, one-per-thread
  participation in the §3 ARC issues. Goal: first handful of **external** deployers
  and their issues/questions (the real adoption signal). **Enable GitHub
  Discussions here** and seed 2–3 starter threads (intro, roadmap feedback), then
  repoint the community links from Issues back to Discussions — its value only
  exceeds the ghost-town cost once traffic is actively flowing and someone is
  watching it.
- **Phase 2 — amplify.** Publish the two blog posts; land in awesome-lists and at
  least one "ARC alternatives" roundup; tune the comparison page for the §5.2
  search terms; pursue a CNCF/KubeCon lightning talk.
- **Phase 3 — sustain.** Keep the comparison and receipts current as ARC evolves;
  keep answering in ARC issues; fold new validated claims in from Q60.

## 9. Adoption metrics (lightweight)

Track adoption, not vanity. The signal that matters most is **external operators
filing issues/questions** — it means someone is actually running it.

- GitHub stars / forks (weak, directional).
- **External issues & discussions opened by non-contributors** (strong — real usage).
- Helm chart / container image pulls (`ghcr.io`).
- Search/referral traffic to `why-gag.md` for ARC-comparison terms.
- Mentions in third-party "ARC alternatives" content and in AI assistant answers.

## 10. Governance / donation posture

This shapes everything above and is the reason monetization stays out:

- **Apache-2.0, vendor-neutral, no commercial roadmap.** Nothing in the project,
  branding, or docs implies a paid product. (Branding already scrubbed of
  franchise terms — see the logomark history.)
- **No CLA that assigns copyright to any company.** Prefer **DCO sign-off** so
  contributions stay community-owned and the project is cleanly donatable to an
  employer or a foundation later.
- **Keep the door closed on revenue for now.** If donations/consulting are ever
  considered, that is a separate, explicit decision — recording it here so the
  non-commercial stance is intentional and visible, not accidental.

## 11. Open follow-ups (feed the Queue when scheduled)

- ~~**ARC → GAG migration guide** (§7)~~ — ✅ shipped as [migration-from-arc](../operations/migration-from-arc.md) (Q199).
- **README problem-first rewrite** (§6) — cheap, improves both humans and GEO.
- **Q60 competitive verification** — several claims still marked VERIFY in
  [competitive-analysis](competitive-analysis.md); confirmed ones harden the
  comparison page and content.
- **Public site launch** ([website.md](website.md)/Q129) — Phase 0 prerequisite for any seeding.
