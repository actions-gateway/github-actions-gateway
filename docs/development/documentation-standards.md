# Documentation standards

The canonical home for **how we write and maintain docs** — the goals, the style,
the conventions, and the upkeep. It complements two neighbours rather than
repeating them:

- [doc-update-matrix.md](doc-update-matrix.md) — *which* docs to update for each kind of change.
- [maintaining-backlog.md](maintaining-backlog.md) — rules specific to [STATUS.md](../STATUS.md).

The bar is **correct and findable first, usable as well** — not polish in place of
substance. A beautifully scannable doc that is wrong, missing, or the wrong type for
the reader's task still fails. The goal hierarchy below makes that ordering explicit;
the rest of the page is how we hit each level.

## The docset in one paragraph

`docs/` is plain GitHub-native Markdown — no MkDocs front matter, no transclusion, no
versioned-docs tree (a [deliberate choice](../plan/docs-six-layer-audit.md): renders on
GitHub, git is the single source of truth). The taxonomy is the per-directory
`README.md` index. There are two audiences: `docs/design/` (how the system works, for
contributors) and `docs/operations/` (what an operator does and sees). A change that
alters operator-visible behaviour must update the operations docs too — design-only is
the classic miss.

The docs also serve **two kinds of reader**: humans, and the AI agents that build on
this repo. They want compatible things — agents reward deterministic, greppable,
single-canonical-home content; humans reward narrative and onboarding — but optimise
for one blindly and you degrade the other. Most rules here serve both; where they
diverge, the doc says so.

## Goals: what good looks like

A doc-quality goal is wasted if the one above it fails — you can't scan your way out of
a wrong or missing answer. In rough order of leverage:

1. **Correct & current.** The doc matches the code, today. A stale doc is *worse* than
   none: it misleads and erodes trust in the whole set. Highest leverage, hardest to
   sustain — see [Maintenance](#maintenance).
2. **Findable.** The reader lands on the right page fast: entry points, `README.md`
   indexes, cross-links, the [glossary](../design/08-glossary.md). A perfect doc nobody
   finds is zero docs.
3. **Complete enough.** The questions readers actually have are answered; no silent
   gaps. Coverage, not exhaustiveness.
4. **Fit-for-purpose.** Right *type* (tutorial / how-to / reference / explanation) and
   right *altitude* (operator vs contributor). A reference dump when someone needs a
   five-step how-to fails regardless of formatting.
5. **Usable.** Scannable, copy-paste-safe, free of filler — see [Write for
   scanning](#write-for-scanning) and [Commands and code blocks](#commands-and-code-blocks).
   Necessary, cheap to get wrong, but not a substitute for 1–4.
6. **Trustworthy in tone.** Specific and evidence-backed, not promotional, and honest
   about limitations, failure modes, and "not yet implemented" — see [Write with
   substance](#write-with-substance-dont-read-as-ai-slop). Trust is what makes a reader
   rely on 1–5 instead of discounting the project on sight.

## Write for scanning

A reader skims headings, then the first line of each block. Optimise for that.

- **Answer first (inverted pyramid).** Put the conclusion in the heading, the first
  sentence of the paragraph, and the first item of the list. Don't make the reader
  reach the end to get unstuck.
- **Self-describing headings.** A heading states the task or answer ("Verify the heavy
  gates ran"), not a bare topic ("Gates"). Someone reading only the headings should
  understand the page.
- **One idea per paragraph.** Keep paragraphs to ~4–5 lines. Split when a second idea
  starts.
- **Lists for any enumeration.** Steps, options, conditions, and requirements become
  ordered/unordered lists — never a comma-strung sentence. This *cuts* words.
- **Tables for comparisons and mappings.** Anything shaped "for X do Y" or "A vs B" is
  a table, not parallel paragraphs.
- **Front-load list items.** Start each bullet with the distinguishing word, bolded
  (`**Worktree paths** — …`), not boilerplate ("When you are in a worktree, …").
- **Bold sparingly.** Bold the one keyword a scanner hunts for. If everything is bold,
  nothing is.

## Cut before you polish

Length is the most common defect in this tree and the hardest to see from inside
a draft. Run these four passes before touching wording. Every example below is
from one review of the marketing pages, where 24 rounds of feedback collapsed
into these four causes.

**Ask whether the block belongs on this page at all.** Three questions: does
*this* page's reader need it, does it belong on another page, and what audience
wants it and where would they look for it. `why-gag.md` carried a 424-word
section on the v2 API decomposition, 22% of the page's height, whose four
comparison points were already rows in the table directly above it, and whose
subject was a GAG v1-to-v2 contrast rather than the ARC comparison the page
exists for. Deleting it lost nothing: `features.md` and
[appendix H](../design/appendix-h-v2-api-decomposition.md) already served the
reader who wanted it.

**Never restate in prose what a diagram or table already carries.** Prose beside
a visual is the visual admitting it did not work. That same page opened a
section with two paragraphs, 139 words, naming three capabilities, their
combined effect and four outcomes, directly above a flow diagram whose nodes
said exactly that. A one-line lead-in replaced both.

**State the invariant, not an instance.** "Listener footprint for 10 runner
sets" invites the question the number cannot answer, because nothing justifies
ten. The claim is a ratio: one always-on pod per set against one shared pod
whatever the count. Saying it that way is shorter *and* stronger. A number earns
its place when it is a measurement (15 to 26 s) or a declared scenario parameter
(the [cost model](../design/appendix-f-cost-model.md)'s ten-set fleet); it is
noise when it is an illustration.

**Scarcity is what makes an emphasis device work, and that is not only true of
bold.** Six consecutive admonitions ran 94 lines on one page, and because all
six were styled identically the most important of them, the honest
where-ARC-is-ahead list, looked exactly like the four routing notes above it.
Budget roughly one callout per screen; promote anything that needs more to a
heading of its own.

## Commands and code blocks

A reader copies a block and runs it. Make that safe.

- **Copyable blocks run as-is.** No `$`/`#` prompt prefixes on copyable lines, no
  command-output interleaved in the same block, no leading line numbers.
- **One runnable command per intent.** Give the whole working line once; don't make the
  reader assemble it from three scattered snippets.
- **Obvious, consistent placeholders.** Use one convention for substitutables
  (`<UPPER_SNAKE>` or `${VAR}`) so the reader knows exactly what to replace. Never let a
  real value masquerade as a placeholder, or vice versa.
- **No ellipses in copyable code.** If a sample is incomplete, mark the omission with a
  language-valid comment (`# … rest of config`) and don't present it as copy-to-run — an
  incomplete block that looks runnable is a trap.
- **Introduce, then show.** A one-line lead-in ending in a colon, then the block. Put
  explanation *before* the block, not as a wall of text after it.
- **A command that fetches a versioned artifact pins the version.** A URL on a moving
  ref — `raw.githubusercontent.com/<org>/<repo>/main/...`, a `:latest` image, an
  unpinned chart — silently hands the reader the *wrong* artifact whenever they are not
  on tip, which is most readers most of the time. Prefer a tool that derives the version
  from what they are installing (`helm show crds <chart> --version <v>`, `helm pull`,
  `gh release download <tag>`) over a hand-written URL; when only a URL will do, pin it
  to a tag. Worst case is silent: applying a CRD from `main` against an older release
  can prune a field the apiserver then drops without an error.
- Shell snippets follow the repo [bash conventions](bash-style.md).

## Write with substance (don't read as AI slop)

Readers discount a project on sight when its docs pattern-match to generated filler.
They bounce before they ever try the code. The tells that cost the most are
substance-level: padding, puffery, and claims without evidence. Punctuation density is
a real tell too, and the em-dash is the one this docset overuses. Earn trust by being
specific and honest.

- **Specifics over adjectives.** Replace "robust, scalable, high-performance" with the
  number, the command, or the named limit. "Multiplexes thousands of sessions as
  goroutines" beats "powerful and efficient." A real command, metric, or link to the
  code/test is the strongest anti-slop signal there is.
- **Plain verbs.** Prefer "is / has / does" over marketing verbs ("serves as", "boasts",
  "leverages", "empowers", "offers a seamless experience"). Cut the puffery vocabulary —
  *delve, showcase, foster, robust, seamless, comprehensive, cutting-edge, powerful*.
- **No padding, no treadmill.** Every paragraph adds new information. Delete
  section-summary restatement, "challenges and future prospects" speculation, and the
  optimistic wrap-up that says nothing. If a 500-word section carries 100 words of fact,
  cut it to 100.
- **Drop the rhetorical tics.** No "not just X, but Y"; no forced rule-of-three lists; no
  "it's important to note"; no manufactured significance ("marks a pivotal shift").
- **Honest, not promotional.** State limitations, known gaps, and "not yet implemented"
  plainly (goal 6). A doc that admits what doesn't work yet reads as written by someone
  who actually ran it.
- **Ration the em-dash.** One or two per page is punctuation. Several per paragraph is
  a signature, and it is the surface tell readers spot fastest. Reach for a comma, a
  period, a colon, or parentheses first, and keep the dash only where the aside really
  does interrupt the sentence and no other mark carries it. Two dashes in one sentence
  almost always means the sentence wants to be two sentences. Above roughly 3 per 1,000
  words, rewrite. `make em-dash-check` enforces it: see
  [Enforcing the em-dash rule](#enforcing-the-em-dash-rule).
- **Formatting in moderation.** Bold the keyword, not the sentence; sentence-case
  headings, not Title Case; no emoji as bullets or status markers. Mechanical,
  every-line formatting is itself a tell. (The one sanctioned Title-Case heading is the
  `## Table of Contents` index — a fixed, doc-wide convention, not prose.)

Fix both halves. Cutting punctuation density while leaving the padding and puffery
untouched produces well-punctuated filler, and no amount of substance survives prose
that reads as machine-generated on sight. Neither half is optional, and neither
substitutes for the other.

## Enforcing the em-dash rule

`make em-dash-check` ([`scripts/docs/check-em-dash.sh`](../../scripts/docs/check-em-dash.sh))
counts the dashes and fails the build. It runs inside `make check` and as its own job in
the `doc-links` CI workflow.

**What it does not count.** A raw `grep -o '—' | wc -l` was the rule's only instrument
before, and it counts four shapes where the dash is legitimate, which is most of why the
rule was unmeasurable. The counter reads the parsed document instead
([`devtools/docs/emdash`](../../devtools/docs/emdash/), over the goldmark layer Q612
built), and skips:

| Skipped | Why | In the tree |
|---|---|---|
| Fenced and indented code blocks, inline code spans | The dash is part of a command or an identifier | 289 |
| Heading text | The title separator, `2.1. Tier 1 — Gateway Manager Controller`, is this docset's section-naming convention | 590 |
| Link text | Every one is a title citation, `[Appendix A — Capacity Targets & SLOs](…)`, reproducing a heading the linking file does not own | 127 |
| Raw HTML, block and inline | Markup and comments, which no reader sees | 3 |

Words are skipped with them, so a long code block buys a page no headroom. Table cells,
blockquotes and list items are prose and are counted.

**The baseline.** With those exclusions the tree measured 9,993 em-dashes in 584,490
prose words on 2026-08-04, 17.1 per 1,000, with 231 of 249 files above the rule. A gate set at 3
would land red on everything and be switched off, so
[`scripts/docs/em-dash-baseline.txt`](../../scripts/docs/em-dash-baseline.txt) freezes
each of those files at its current count as a ceiling. A listed file may not gain
em-dashes; a file with no entry, including every new doc, is held to the rule itself.

[Q650](../STATUS.md#Q650) is the cleanup. As it lands, `make em-dash-baseline` re-records
the ceilings and the diff is the measure of what it cleared; an entry disappears once its
file reaches the rule, and an empty baseline means the ratchet is done.
`make em-dash-report` prints every file's density, worst first, which is the worklist.

## An upstream-behavior claim cites a measurement

A sentence about how something outside this repo behaves is a claim nobody here can
check, and it goes stale with no commit, no red gate, and no other signal. Write the
version it was measured against and when, or name the gate that keeps re-measuring it.
The Queue-row form of the same rule is [maintaining-backlog.md § A row's asserted defect
is a claim](maintaining-backlog.md#a-rows-asserted-defect-is-a-claim-not-a-finding);
this section is its other two homes, prose docs and code comments.

Four such claims shipped here from source inspection. All four were wrong:

| Claim, as shipped | What refuted it |
|---|---|
| "Karpenter sets `eventTime`", in the operator runbook, with the same premise in two places in the capacity plan | Q479's live run: at Karpenter v1.14.0 the events come from the **legacy** recorder, `lastTimestamp` set and `eventTime` null, the same generation as cluster autoscaler ([§9i](../plan/capacity-aware-intake.md#9i-the-karpenter-arm-of-the-drift-gate-and-what-it-measured-q479)). |
| "The skipped job is re-assigned or timed out server-side", a [`listener.go`](../../cmd/agc/internal/scalesetlistener/listener.go) comment (Q270) that was also the safety argument for dropping the job | Q551: the scale-set queue does **not** re-assign it. Skipped jobs sat queued at GitHub forever with no condition, Event, or metric naming them. The comment now reads "the queue does not re-assign it". |
| Q584's own filing: `check-path-filters.sh`'s awk YAML parsing could "mis-read as full coverage, failing green" | The gate iterates a hardcoded filter registry against `go.work`, so a parse failure removes patterns and fails **closed**. The row reached `main` first and cost a second PR to correct. |
| "Never `/tmp` **or the session scratchpad**", the `CLAUDE.md` workspace-guard bullet (#869) | Probed on workspace-guard 1.8.0: writing under this session's own scratchpad and reading it back both succeed silently, because `is_session_tmp_path` returns before the read/write split. The rule generalised a single prompt in a friction log that reached back before the guard widened the exemption to native `Edit`/`Write`. Eleven days of sessions routed temp files away from a directory they were allowed to use (#1319). |

Q594 is the fifth, and the one caught in time: it asserted that `plan-hygiene.yml`'s
`**.go` filter matched no Go file. Running the pinned action measured the opposite, so
no fix was needed, and what landed instead is a compliant version of the finding
([testing.md § Where a globstar works in a filter
glob](testing.md#where-a-globstar-works-in-a-filter-glob)).

### What counts as upstream

Anything whose behavior can change with no commit in this repo: a third-party library,
a Kubernetes API behavior, a GitHub API or Actions Service response, a CI action's
matching semantics, a Helm chart's rendered defaults, an autoscaler's event vocabulary,
**or a Claude Code hook, plugin, or harness decision** (what workspace-guard denies,
which paths are exempt, what a slash command does). The test is whether the sentence
could go false while `make check` stays green and nobody here touched anything.

**`CLAUDE.md` is in scope, and is the worst place for a stale one.** Its process rules
read as house convention rather than as claims, so they attract no citation and get no
review, while every session loads them and acts on them. A rule that says a tool denies
something is an upstream-behavior claim wearing a procedural hat: cite the version you
measured against, or say which command you ran. The scratchpad row above cost eleven
days precisely because "never the session scratchpad" looked like a preference.

Two edges keep the rule from swallowing every sentence in the docset:

- **Our own behavior is not upstream.** A wrong claim about this repo's code goes red
  in a test. That is the gate; a citation adds nothing to it. The exemption is exactly
  as strong as that red gate, which is why a restated default falls outside it (next
  section).
- **Specified is not observed.** Where upstream publishes normative text (an RFC
  grammar, a documented status code, a CRD field's own godoc), link that text and stop:
  the spec is its own citation. The rule bites on behavior learned by reading upstream
  source or by inference, which is most of what this system depends on, and is where
  all four claims above came from.

### A restated default is a claim with no gate

A flag or field default restated away from its wiring is the in-repo case the
"our own behavior" exemption does not cover: no test reads comments, so a wrong
one stays green forever. The measured case is Q676 (2026-08-04): `broker/types.go`,
`listener/job.go`, and the Q645 plan doc all said the AGC's completejob release
"stays gated off by default" while `cmd/agc/config.go` shipped
`AGC_FANOUT_COMPLETION` on unless explicitly `"false"`. A production false-green
defect read as contained for as long as the comments were trusted; the divergence
surfaced only because remedy work happened to read the wiring.

State a default's value only at the site that wires it. Everywhere else, say the
switch exists and cite that site (or the operator doc that documents it), rather
than copying the value into a sentence that can drift. When restating is genuinely
clearer, treat the sentence like an upstream claim: name the wiring site in the
same clause, so the reader can check it and the next editor knows what it depends
on.

### In prose

State the claim, then the version and the date, or the run they came from. Both of
these are in the docset today:

> Today both autoscalers set `lastTimestamp` and leave `eventTime` empty (measured:
> cluster autoscaler v1.36.1, Karpenter v1.14.0).

From [troubleshooting.md](../operations/troubleshooting.md#runnerset-reports-workercapacitydeclined-the-gateway-stopped-claiming-jobs).
The second names the input as well as the version, because the answer depended on it:

> Measured against the pinned `dorny/paths-filter@7b450ff` (v4.0.2) on a branch whose
> diff was two nested Go files.

### In code comments

Comments here are terse and mechanism-focused, so the citation is a parenthetical
rather than a paragraph: the version and what was observed, inside the clause that
makes the claim.

```go
// Neither project records through that generation today — measured live for both
// (CA v1.36.1, Karpenter v1.14.0: LastTimestamp set, EventTime null) — but nothing
// pins them to the legacy one, and the window is what keeps a future migration from
// reintroducing the accident.
```

From [`autoscaler_verdict.go`](../../cmd/agc/internal/controller/autoscaler_verdict.go).
When the measurement needs more than a clause, it goes in `docs/` and the comment links
there. Growing a provenance paragraph in the source is not the compliant form.

**A live gate beats a date.** Where a gate re-measures the claim on every run, name the
gate instead: it is fresher than any date, and it turns red when upstream moves.
`make test-autoscaler` and `make test-karpenter` drive real autoscalers and assert the
matcher's verdict on the events that come back, which is why the reason constants they
pin carry no per-claim date.

### When you cannot measure it

Say so in the same clause: "unverified, source inspection only". One clause tells the
next session where to start, instead of handing it something it will trust for months.
Every claim in the table above shipped as a flat assertion instead.

## Conventions

| Convention | Rule |
|---|---|
| **Acronyms** | Spell out on first use, then the acronym in parentheses — "Actions Gateway Controller (AGC)". Subsequent uses may use the acronym alone. Applies to **project-invented shorthand** (`GAG`, `GMC`, `AGC`, `CR`, `CRD`), which a reader cannot look up. It does **not** apply to vocabulary the stated audience already has: on a page whose audience line says "Platform engineer", `GKE`, `RBAC`, `CNI`, `PSA`, `KEDA`, and `VPA` are read, not decoded, and expanding a run of them ("GKE, EKS, AKS, RKE2") costs more than it explains. When an expansion would be clumsier than the plain term, use the plain term (`ActionsGateway` custom resource) instead of the acronym. |
| **Terminology** | One term per concept across all docs. Canonical definitions live in the [glossary](../design/08-glossary.md); link there rather than redefining. |
| **Diagrams** | Prefer ASCII box-art over Mermaid unless auto-layout is a quantifiable win. ASCII renders everywhere and diffs cleanly. |
| **Canonical home + link** | State a fact once, in its natural home, and link to it. Don't restate — copies drift. (Same rule for reuse, since GitHub has no transclusion.) |
| **A link is a claim** | Linking asserts the target covers the thing you linked *from*. Open it and confirm — nothing else will. `mkdocs build` and `check-doc-links.sh` check that a target **resolves**, never that it is on-topic, so a confidently wrong link stays permanently green. Riskiest when adding links in bulk: a roadmap bullet linked "single-replica by design" to [Appendix E](../design/appendix-e-capacity-planning.md), which never mentions replicas — [§2 Architecture](../design/02-architecture.md) is where that design and its job-level HA argument live. Same principle as [cite a measurement](#an-upstream-behavior-claim-cites-a-measurement), applied to cross-references. |
| **Name a cross-reference, don't count it** | Point at a section by name or link — "the negative-assertion rule above", never "the two rules above". A counted reference goes silently wrong the moment a section lands between the counter and the counted, and nothing catches it: `check-doc-links.sh` and `mkdocs build` resolve anchors, never prose. Inserting one testing.md section left the next one's "the two rules above" naming three. |
| **Anchor a cross-file section reference** | Cite another file's section with an anchor, never as prose beside a bare file link. Write `[testing.md § A negative assertion must be able to fail for only one reason](testing.md#a-negative-assertion-must-be-able-to-fail-for-only-one-reason)`, not `[testing.md](testing.md) § A negative assertion...`. Both render alike and only the first is checked, for the reason the row above gives, so the prose form is verbatim the day it is written and silently wrong after any rename. Measured 2026-08-05: 11 such references in the tree against 1,051 anchored links; [Q686](../STATUS.md#Q686) gates it. |
| **No links to `CLAUDE.md`** | `CLAUDE.md`/`AGENTS.md` is the agent entrypoint only. Human docs never link to it; the dependency direction is one-way (`CLAUDE.md` → `docs/`). Content humans need lives in `docs/` or `CONTRIBUTING.md`. |
| **Table of contents** | Long docs (~400+ lines) carry a `## Table of Contents` after the intro listing h2s (plus h3 for operator docs). Anchors follow GitHub slug rules (duplicate headings get `-1`/`-2`); verify against the rendered page. |
| **Cut filler** | Delete "in order to", "it should be noted that", "please note", and hedging preambles. A pure win for brevity and scannability. |
| **Versioning** | Docs describe `main`: mark unshipped behavior `(planned)`, never as if it ships. API versions (`v1alpha1`→v2…) are documented together on `main` with reference + migration guides — one running gateway serves them at once. Separately, the published *site* is a versioned *tree* (a built copy per release): it defaults to the latest stable release, with `main` behind an opt-in `dev` version — see [website.md § Versioned deploy](website.md#versioned-deploy-mike). |

## Maintenance

Goal 1 — *correct & current* — is the hardest to sustain because docs rot silently as
the code moves. Keep them current:

- **Update docs in the same change.** After a behaviour change, update every doc it
  touches before opening the PR — the change-type → docs map is the
  [doc-update-matrix](doc-update-matrix.md). Design-doc updates alone are not enough when
  a change alters what an operator does, configures, or observes.
- **Keep each `README.md` index complete.** A new doc gets a row in its directory's
  `README.md` index in the same change (a goal-2 *findability* failure otherwise).
- **Archive finished plans.** When a plan's last STATUS reference is removed, update its
  [`docs/plan/README.md`](../plan/README.md) row and archive the plan in the same change
  — `make plan-index-check` enforces this. See
  [maintaining-backlog.md](maintaining-backlog.md#archiving-completed-plan-docs).
- **STATUS.md gets its own commit.** It is high-contention; isolating its changes keeps
  rebases trivial. Queue Notes have a hard 250-char cap (lint-enforced). Details:
  [maintaining-backlog.md](maintaining-backlog.md).
- **Links and anchors are checked, twice.** `make doc-links` (in `make check`) fails on
  broken cross-file links and heading anchors as **github.com** resolves them;
  `make docs-build` re-checks them as the **published site** does. The two sluggers
  disagree, so a link needs both verdicts — write headings and links that survive each
  per [website.md § The two link gates](website.md#the-two-link-gates). External URLs are
  out of scope for both — eyeball those.

## Measuring doc quality

"Best docs" implies a way to *know* — quality you can observe, not just assert. What
exists today, and what's proposed:

| Signal | Goal | Status |
|---|---|---|
| `make plan-index-check` — every plan doc is indexed/archived | 2 | Wired (`make check`). |
| STATUS.md lint — Queue shape, 250-char Note cap | 1 | Wired (`make check`). |
| Per-change doc updates via the [doc-update-matrix](doc-update-matrix.md) | 1 | Convention, enforced in review (PR self-check). |
| `make doc-links` — broken cross-file links + heading anchors, GitHub slugs (Q52) | 1, 2 | Wired (`make check`). The automated guard against link rot. |
| `make docs-build` — the same links as the published site resolves them (Q560) | 1, 2 | Wired (`pages.yml` PR gate). Catches the site-only 404s `doc-links` cannot see. |
| `make em-dash-check`: em-dash density, code and titles excluded (Q654) | 1 | Wired (`make check`). Ratchets against a per-file baseline while [Q650](../STATUS.md#Q650) clears the debt; see [Enforcing the em-dash rule](#enforcing-the-em-dash-rule). |
| Periodic docs-vs-code drift audit | 1 | **Proposed** — a recurring backstop for what the per-change rule misses. |
| Reader questions (issues, support threads) logged as coverage gaps | 3 | **Proposed** — turns real confusion into Queue items. |

Treat the proposed rows as the roadmap for making quality observable; the highest-value
next step is the periodic drift audit, since it catches what the per-change rule misses.

## Authoring checklist

Before opening a docs PR, check against the goals — not just formatting:

- [ ] **Correct & current (1):** matches the code today; every doc the change touches is
      updated per the [doc-update-matrix](doc-update-matrix.md).
- [ ] **Findable (2):** linked from its directory `README.md` index; cross-links and
      anchors verified against the rendered page; no orphan.
- [ ] **Complete (3):** answers the question a reader arrives with; no silent gap or
      undocumented failure mode.
- [ ] **Fit-for-purpose (4):** right type and altitude for its audience
      (operator vs contributor).
- [ ] **Usable (5):** answer-first; enumerations are lists and comparisons are tables;
      code/command blocks are copy-paste-runnable with consistent placeholders; no walls
      of text or filler.
- [ ] **Trustworthy (6):** specific and evidence-backed, not promotional (doesn't read
      as AI slop); honest about limitations and "not yet implemented"; acronyms expanded
      on first use; terms match the glossary; no links to `CLAUDE.md`; every
      [upstream-behavior claim cites a
      measurement](#an-upstream-behavior-claim-cites-a-measurement) or the gate pinning it.
