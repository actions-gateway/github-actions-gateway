# Documentation standards

The canonical home for **how we write and maintain docs** — the goals, the style, the conventions, and the upkeep.
It complements two neighbours rather than repeating them:

- [doc-update-matrix.md](doc-update-matrix.md) — *which* docs to update for each kind of change.
- [maintaining-backlog.md](maintaining-backlog.md) — rules specific to [the backlog](../queue/README.md).

The bar is **correct and findable first, usable as well** — not polish in place of substance.
A beautifully scannable doc that is wrong, missing, or the wrong type for the reader's task still fails.
The goal hierarchy below makes that ordering explicit; the rest of the page is how we hit each level.

## The docset in one paragraph

`docs/` is plain GitHub-native Markdown — no MkDocs front matter, no transclusion, no versioned-docs tree (a [deliberate choice](../plan/docs-six-layer-audit.md): renders on GitHub, git is the single source of truth).
The taxonomy is the per-directory `README.md` index.
There are two audiences: `docs/design/` (how the system works, for contributors) and `docs/operations/` (what an operator does and sees).
A change that alters operator-visible behaviour must update the operations docs too — design-only is the classic miss.

The docs also serve **two kinds of reader**: humans, and the AI agents that build on this repo.
They want compatible things — agents reward deterministic, greppable, single-canonical-home content; humans reward narrative and onboarding — but optimise for one blindly and you degrade the other.
Most rules here serve both; where they diverge, the doc says so.

## Goals: what good looks like

A doc-quality goal is wasted if the one above it fails — you can't scan your way out of a wrong or missing answer.
In rough order of leverage:

1. **Correct & current.** The doc matches the code, today.
   A stale doc is *worse* than none: it misleads and erodes trust in the whole set.
   Highest leverage, hardest to sustain — see [Maintenance](#maintenance).
2. **Findable.** The reader lands on the right page fast: entry points, `README.md` indexes, cross-links, the [glossary](../design/08-glossary.md).
   A perfect doc nobody finds is zero docs.
3. **Complete enough.** The questions readers actually have are answered; no silent gaps.
   Coverage, not exhaustiveness.
4. **Fit-for-purpose.** Right *type* (tutorial / how-to / reference / explanation) and right *altitude* (operator vs contributor).
   A reference dump when someone needs a five-step how-to fails regardless of formatting.
5. **Usable.** Scannable, copy-paste-safe, free of filler — see [Write for scanning](#write-for-scanning) and [Commands and code blocks](#commands-and-code-blocks).
   Necessary, cheap to get wrong, but not a substitute for 1–4.
6. **Trustworthy in tone.** Specific and evidence-backed, not promotional, and honest about limitations, failure modes, and "not yet implemented" — see [Write with substance](#write-with-substance-dont-read-as-ai-slop).
   Trust is what makes a reader rely on 1–5 instead of discounting the project on sight.

## Write for scanning

A reader skims headings, then the first line of each block.
Optimise for that.

- **Answer first (inverted pyramid).** Put the conclusion in the heading, the first sentence of the paragraph, and the first item of the list.
  Don't make the reader reach the end to get unstuck.
- **Self-describing headings.** A heading states the task or answer ("Verify the heavy gates ran"), not a bare topic ("Gates").
  Someone reading only the headings should understand the page.
- **One idea per paragraph.** Keep paragraphs to ~4–5 lines.
  Split when a second idea starts.
- **Lists for any enumeration.** Steps, options, conditions, and requirements become ordered/unordered lists — never a comma-strung sentence.
  This *cuts* words.
- **Tables for comparisons and mappings.** Anything shaped "for X do Y" or "A vs B" is a table, not parallel paragraphs.
- **Front-load list items.** Start each bullet with the distinguishing word, bolded (`**Worktree paths** — …`), not boilerplate ("When you are in a worktree, …").
- **Bold sparingly.** Bold the one keyword a scanner hunts for.
  If everything is bold, nothing is.

## Cut before you polish

Length is the most common defect in this tree and the hardest to see from inside a draft.
Run these four passes before touching wording.
Every example below is from one review of the marketing pages, where 24 rounds of feedback collapsed into these four causes.

**Ask whether the block belongs on this page at all.** Three questions: does *this* page's reader need it, does it belong on another page, and what audience wants it and where would they look for it.
`why-gag.md` carried a 424-word section on the v2 API decomposition, 22% of the page's height, whose four comparison points were already rows in the table directly above it, and whose subject was a GAG v1-to-v2 contrast rather than the ARC comparison the page exists for.
Deleting it lost nothing: `features.md` and [appendix H](../design/appendix-h-v2-api-decomposition.md) already served the reader who wanted it.

**Never restate in prose what a diagram or table already carries.** Prose beside a visual is the visual admitting it did not work.
That same page opened a section with two paragraphs, 139 words, naming three capabilities, their combined effect and four outcomes, directly above a flow diagram whose nodes said exactly that.
A one-line lead-in replaced both.

**State the invariant, not an instance.** "Listener footprint for 10 runner sets" invites the question the number cannot answer, because nothing justifies ten.
The claim is a ratio: one always-on pod per set against one shared pod whatever the count.
Saying it that way is shorter *and* stronger.
A number earns its place when it is a measurement (15 to 26 s) or a declared scenario parameter (the [cost model](../design/appendix-f-cost-model.md)'s ten-set fleet); it is noise when it is an illustration.

**Scarcity is what makes an emphasis device work, and that is not only true of bold.** Six consecutive admonitions ran 94 lines on one page, and because all six were styled identically the most important of them, the honest where-ARC-is-ahead list, looked exactly like the four routing notes above it.
Budget roughly one callout per screen; promote anything that needs more to a heading of its own.

## Commands and code blocks

A reader copies a block and runs it.
Make that safe.

- **Copyable blocks run as-is.** No `$`/`#` prompt prefixes on copyable lines, no command-output interleaved in the same block, no leading line numbers.
- **One runnable command per intent.** Give the whole working line once; don't make the reader assemble it from three scattered snippets.
- **Obvious, consistent placeholders.** Use one convention for substitutables (`<UPPER_SNAKE>` or `${VAR}`) so the reader knows exactly what to replace.
  Never let a real value masquerade as a placeholder, or vice versa.
- **No ellipses in copyable code.** If a sample is incomplete, mark the omission with a language-valid comment (`# … rest of config`) and don't present it as copy-to-run — an incomplete block that looks runnable is a trap.
- **Introduce, then show.** A one-line lead-in ending in a colon, then the block.
  Put explanation *before* the block, not as a wall of text after it.
- **A command that fetches a versioned artifact pins the version.** A URL on a moving ref — `raw.githubusercontent.com/<org>/<repo>/main/...`, a `:latest` image, an unpinned chart — silently hands the reader the *wrong* artifact whenever they are not on tip, which is most readers most of the time.
  Prefer a tool that derives the version from what they are installing (`helm show crds <chart> --version <v>`, `helm pull`, `gh release download <tag>`) over a hand-written URL; when only a URL will do, pin it to a tag.
  Worst case is silent: applying a CRD from `main` against an older release can prune a field the apiserver then drops without an error.
- Shell snippets follow the repo [bash conventions](bash-style.md).

## Write with substance (don't read as AI slop)

Readers discount a project on sight when its docs pattern-match to generated filler.
They bounce before they ever try the code.
The tells that cost the most are substance-level: padding, puffery, and claims without evidence.
Punctuation density is a real tell too, and the em-dash is the one this docset overuses.
Earn trust by being specific and honest.

- **Specifics over adjectives.** Replace "robust, scalable, high-performance" with the number, the command, or the named limit.
  "Multiplexes thousands of sessions as goroutines" beats "powerful and efficient."
  A real command, metric, or link to the code/test is the strongest anti-slop signal there is.
- **Plain verbs.** Prefer "is / has / does" over marketing verbs ("serves as", "boasts", "leverages", "empowers", "offers a seamless experience").
  Cut the puffery vocabulary — *delve, showcase, foster, robust, seamless, comprehensive, cutting-edge, powerful*.
- **No padding, no treadmill.** Every paragraph adds new information.
  Delete section-summary restatement, "challenges and future prospects" speculation, and the optimistic wrap-up that says nothing.
  If a 500-word section carries 100 words of fact, cut it to 100.
- **Drop the rhetorical tics.** No "not just X, but Y"; no forced rule-of-three lists; no "it's important to note"; no manufactured significance ("marks a pivotal shift").
- **Honest, not promotional.** State limitations, known gaps, and "not yet implemented" plainly (goal 6).
  A doc that admits what doesn't work yet reads as written by someone who actually ran it.
- **Ration the em-dash.** One or two per page is punctuation.
  Several per paragraph is a signature, and it is the surface tell readers spot fastest.
  Reach for a comma, a period, a colon, or parentheses first, and keep the dash only where the aside really does interrupt the sentence and no other mark carries it.
  Two dashes in one sentence almost always means the sentence wants to be two sentences.
  Above roughly 3 per 1,000 words, rewrite.
  `make em-dash-check` enforces it: see [Enforcing the em-dash rule](#enforcing-the-em-dash-rule).
- **Formatting in moderation.** Bold the keyword, not the sentence; sentence-case headings, not Title Case; no emoji as bullets or status markers.
  Mechanical, every-line formatting is itself a tell.
  (The one sanctioned Title-Case heading is the `## Table of Contents` index — a fixed, doc-wide convention, not prose.)

Fix both halves.
Cutting punctuation density while leaving the padding and puffery untouched produces well-punctuated filler, and no amount of substance survives prose that reads as machine-generated on sight.
Neither half is optional, and neither substitutes for the other.

## Enforcing the em-dash rule

`make em-dash-check` ([`scripts/docs/check-em-dash.sh`](../../scripts/docs/check-em-dash.sh)) counts the dashes and fails the build.
It runs inside `make check` and as its own job in the `doc-links` CI workflow.

**A per-file ceiling is a per-PR check, so two PRs can merge jointly red.** Each sits within its own ceiling on its own base, so per-PR CI never sees the merge result: measured twice on 2026-08-08, the second reaching `main` ([Q742](../queue/Q742.md)).
Whether the merge queue runs this gate on the candidate at all is the first thing to establish, and the fix is to ratchet on the diff rather than the file total.

**What it does not count.** A raw `grep -o '—' | wc -l` was the rule's only instrument before, and it counts four shapes where the dash is legitimate, which is most of why the rule was unmeasurable.
The counter reads the parsed document instead ([`devtools/docs/emdash`](../../devtools/docs/emdash/), over the goldmark layer Q612 built), and skips:

| Skipped | Why | In the tree |
|---|---|---|
| Fenced and indented code blocks, inline code spans | The dash is part of a command or an identifier | 289 |
| Heading text | The title separator, `2.1. Tier 1 — Gateway Manager Controller`, is this docset's section-naming convention | 590 |
| Link text | Every one is a title citation, `[Appendix A — Capacity Targets & SLOs](…)`, reproducing a heading the linking file does not own | 127 |
| Raw HTML, block and inline | Markup and comments, which no reader sees | 3 |

Words are skipped with them, so a long code block buys a page no headroom.
Table cells, blockquotes and list items are prose and are counted.

**The baseline.** With those exclusions the tree measured 9,993 em-dashes in 584,490 prose words on 2026-08-04, 17.1 per 1,000, with 231 of 249 files above the rule.
A gate set at 3 would land red on everything and be switched off, so [`scripts/docs/em-dash-baseline.txt`](../../scripts/docs/em-dash-baseline.txt) freezes each of those files at its current count as a ceiling.
A listed file may not gain em-dashes; a file with no entry, including every new doc, is held to the rule itself.

[Q650](../queue/Q650.md) is the cleanup.
As it lands, `make em-dash-baseline` re-records the ceilings and the diff is the measure of what it cleared; an entry disappears once its file reaches the rule, and an empty baseline means the ratchet is done.
`make em-dash-report` prints every file's density, worst first, which is the worklist.

## An upstream-behavior claim cites a measurement

A sentence about how something outside this repo behaves is a claim nobody here can check, and it goes stale with no commit, no red gate, and no other signal.
Write the version it was measured against and when, or name the gate that keeps re-measuring it.
The Queue-row form of the same rule is [maintaining-backlog.md § A row's asserted defect is a claim](maintaining-backlog.md#a-rows-asserted-defect-is-a-claim-not-a-finding); this section is its other two homes, prose docs and code comments.

Four such claims shipped here from source inspection.
All four were wrong:

| Claim, as shipped | What refuted it |
|---|---|
| "Karpenter sets `eventTime`", in the operator runbook, with the same premise in two places in the capacity plan | Q479's live run: at Karpenter v1.14.0 the events come from the **legacy** recorder, `lastTimestamp` set and `eventTime` null, the same generation as cluster autoscaler ([§9i](../plan/capacity-aware-intake.md#9i-the-karpenter-arm-of-the-drift-gate-and-what-it-measured-q479)). |
| "The skipped job is re-assigned or timed out server-side", a [`listener.go`](../../cmd/agc/internal/scalesetlistener/listener.go) comment (Q270) that was also the safety argument for dropping the job | Q551: the scale-set queue does **not** re-assign it. Skipped jobs sat queued at GitHub forever with no condition, Event, or metric naming them. The comment now reads "the queue does not re-assign it". |
| Q584's own filing: `check-path-filters.sh`'s awk YAML parsing could "mis-read as full coverage, failing green" | The gate iterates a hardcoded filter registry against `go.work`, so a parse failure removes patterns and fails **closed**. The row reached `main` first and cost a second PR to correct. |
| "Never `/tmp` **or the session scratchpad**", the `CLAUDE.md` workspace-guard bullet (#869) | Probed on workspace-guard 1.8.0: writing under this session's own scratchpad and reading it back both succeed silently, because `is_session_tmp_path` returns before the read/write split. The rule generalised a single prompt in a friction log that reached back before the guard widened the exemption to native `Edit`/`Write`. Eleven days of sessions routed temp files away from a directory they were allowed to use (#1319). |

Q594 is the fifth, and the one caught in time: it asserted that `plan-hygiene.yml`'s `**.go` filter matched no Go file.
Running the pinned action measured the opposite, so no fix was needed, and what landed instead is a compliant version of the finding ([testing.md § Where a globstar works in a filter glob](testing.md#where-a-globstar-works-in-a-filter-glob)).

### What counts as upstream

Anything whose behavior can change with no commit in this repo: a third-party library, a Kubernetes API behavior, a GitHub API or Actions Service response, a CI action's matching semantics, a Helm chart's rendered defaults, an autoscaler's event vocabulary, **or a Claude Code hook, plugin, or harness decision** (what workspace-guard denies, which paths are exempt, what a slash command does).
The test is whether the sentence could go false while `make check` stays green and nobody here touched anything.

**`CLAUDE.md` is in scope, and is the worst place for a stale one.** Its process rules read as house convention rather than as claims, so they attract no citation and get no review, while every session loads them and acts on them.
A rule that says a tool denies something is an upstream-behavior claim wearing a procedural hat: cite the version you measured against, or say which command you ran.
The scratchpad row above cost eleven days precisely because "never the session scratchpad" looked like a preference.

Two edges keep the rule from swallowing every sentence in the docset:

- **Our own behavior is not upstream.** A wrong claim about this repo's code goes red in a test.
  That is the gate; a citation adds nothing to it.
  The exemption is exactly as strong as that red gate, which is why a restated default falls outside it (next section).
- **Specified is not observed.** Where upstream publishes normative text (an RFC grammar, a documented status code, a CRD field's own godoc), link that text and stop: the spec is its own citation.
  The rule bites on behavior learned by reading upstream source or by inference, which is most of what this system depends on, and is where all four claims above came from.

### A credit claim cites the issue, not the release notes

Saying an upstream fix was reported here is an attribution rather than a behavior claim, so a measurement does not settle it.
The record does: `gh api repos/<owner>/<repo>/issues/<n> --jq .user.login`, one call per issue.
Release notes group fixes by theme and not by who found them, which is what makes the wrong reading easy. mdreflow v0.1.7's notes credit [#37](https://github.com/jbeda/mdreflow/issues/37) to this repo and describe [#39](https://github.com/jbeda/mdreflow/issues/39) and [#41](https://github.com/jbeda/mdreflow/issues/41) as fuzz-found in the next breath; the second two are upstream's own, and reading the section as one narrative credits all three here.
Getting it wrong takes credit for someone else's work in a file that outlives the session, and no gate can see it.

### A restated default is a claim with no gate

A flag or field default restated away from its wiring is the in-repo case the "our own behavior" exemption does not cover: no test reads comments, so a wrong one stays green forever.
The measured case is Q676 (2026-08-04): `broker/types.go`, `listener/job.go`, and the Q645 plan doc all said the AGC's completejob release "stays gated off by default" while `cmd/agc/config.go` shipped `AGC_FANOUT_COMPLETION` on unless explicitly `"false"`.
A production false-green defect read as contained for as long as the comments were trusted; the divergence surfaced only because remedy work happened to read the wiring.

State a default's value only at the site that wires it.
Everywhere else, say the switch exists and cite that site (or the operator doc that documents it), rather than copying the value into a sentence that can drift.
When restating is genuinely clearer, treat the sentence like an upstream claim: name the wiring site in the same clause, so the reader can check it and the next editor knows what it depends on.

### A section cross-reference outlives its target without a symptom

Citing a section of a document outside this repo (`the session-worker skill §8`) is an upstream claim wearing a citation's hat, and it is the variant with no visible failure.
A dead link is at least conspicuous.
A dead section number is not: the sentence still parses, still reads as authoritative, and simply points at nothing.
`doc-links` cannot help, because it resolves anchors in files it can see, and a section number inside a globally-installed skill is not one of them.

Measured 2026-08-16 while retiring the local worker skill (#1573).
Three sentences in `parallel-dispatch.md` cited `dispatch-worker` skill §8 for how a worker addresses the dispatcher.
Retiring that skill in favour of `session-worker` read as a rename, so the obvious edit was to swap the name in all three and move on.
The ported skill did keep a §8 carrying the same content, so the digit survived, but nothing in the change would have said otherwise.

So assert the target before trusting the digit, and treat three outcomes as distinct: the section exists with the same content, so keep the number; the content moved, so name the section instead of numbering it; or the content is gone, which is a finding to send upstream rather than three sentences to quietly delete.
Prefer naming a section over numbering it whenever the target is a document this repo does not gate.

### In prose

State the claim, then the version and the date, or the run they came from.
Both of these are in the docset today:

> Today both autoscalers set `lastTimestamp` and leave `eventTime` empty (measured: cluster autoscaler v1.36.1, Karpenter v1.14.0).

From [troubleshooting.md](../operations/troubleshooting.md#runnerset-reports-workercapacitydeclined-the-gateway-stopped-claiming-jobs).
The second names the input as well as the version, because the answer depended on it:

> Measured against the pinned `dorny/paths-filter@7b450ff` (v4.0.2) on a branch whose diff was two nested Go files.

### In code comments

Comments here are terse and mechanism-focused, so the citation is a parenthetical rather than a paragraph: the version and what was observed, inside the clause that makes the claim.

```go
// Neither project records through that generation today — measured live for both
// (CA v1.36.1, Karpenter v1.14.0: LastTimestamp set, EventTime null) — but nothing
// pins them to the legacy one, and the window is what keeps a future migration from
// reintroducing the accident.
```

From [`autoscaler_verdict.go`](../../cmd/agc/internal/controller/autoscaler_verdict.go).
When the measurement needs more than a clause, it goes in `docs/` and the comment links there.
Growing a provenance paragraph in the source is not the compliant form.

**A live gate beats a date.** Where a gate re-measures the claim on every run, name the gate instead: it is fresher than any date, and it turns red when upstream moves.
`make test-autoscaler` and `make test-karpenter` drive real autoscalers and assert the matcher's verdict on the events that come back, which is why the reason constants they pin carry no per-claim date.

### A competitor-side verdict carries its own stamp

The marketing case, and the one where the prose form of the rule is not enough.
[why-gag.md](../why-gag.md) renders competitor claims as green checks and red X's, and a verdict table needs a definite cell in every row.
The working notes it was built from had marked most ARC-side facts unverified, and the format had nowhere to put "we believe this but have not checked it", so eleven unverified facts shipped as red X's ([#308](https://github.com/actions-gateway/github-actions-gateway/pull/308)).
Two then went false at datable upstream releases with nothing going red.

One blanket "measured on DATE against VERSION" note under the table does not fix it: under a blanket note a checked claim and an assumed one render identically, so staleness accumulates invisibly and re-verification is all-or-nothing.
The rule is therefore per cell, and mechanical enough to gate:

- a **verdict** carries exactly one `<span class="gag-asof">VERSION · DATE</span>`, naming the upstream version it was read at and the day it was read;
- **`.gag-unverified`** is the state for a claim we believe and have not checked, and it carries no stamp, because a stamp is what a verdict rests on;
- there is no third case.
  `make comparison-stamps-check` fails the page, in `make check`.

Only the competitor column is stamped.
A wrong claim about this repo's own behaviour goes red in a test, which is the "our own behaviour is not upstream" exemption above; requiring a citation there would add nothing to that gate and would make every GAG cell unfixable without one nobody can produce.

**Staleness is reported, never failed.** A gate that reds on a date nobody committed turns `main` red with no change to revert, so `check-comparison-stamps.sh --report` prints the stamps oldest-first and [release pre-flight](../operations/release.md#1-pre-flight) reads it.
That is what turns pre-flight from "re-verify everything" into "re-check what went stale".

**Pin the ref beside the version.** A tag names what the stamp claims; a commit is what makes it re-checkable, since a link to a branch drifts out from under the claim it was cited for (#1422).
The page carries the commit once, under the table, and the per-cell evidence lives in [the competitive analysis](../plan/competitive-analysis-2026-08.md#per-cell-evidence-for-the-arc-column-2026-08-12).

### When you cannot measure it

Say so in the same clause: "unverified, source inspection only".
One clause tells the next session where to start, instead of handing it something it will trust for months.
Every claim in the table above shipped as a flat assertion instead.
On a surface that renders verdicts rather than sentences, the same admission has a rendering of its own (previous section).

## A rule written from one instance names its second one

A process rule gets written when the incident that motivated it is freshest, from exactly one example, by someone who has just spent a session inside that example.
That is the worst moment to tell which parts of it are the rule and which are the instance, so the rule carries the example's accidents forward as though they were the mechanism.

**Check it against a second, unlike instance before it ships, or say in the rule that it is single-sourced.** The second instance is what separates the two: what the pair share is the rule, what only one has was scenery.
Shipping a single-sourced rule is fine; shipping one that reads like a general rule is not.

Two here.
Q702's first version, in [maintaining-backlog.md](maintaining-backlog.md), explained one defect landing on two Queue rows as two rows filed minutes apart out of one campaign, which was true of the pair it was written from.
A session applying it to a different pair found the opposite cause, a row filed eleven days later by someone who did not know there was a pair, and neither the rationale nor the ownership test fitted.
That second pair had been sitting in the Queue the whole time.
The `CLAUDE.md` scratchpad bullet in the table above is the same shape and the more expensive one: it generalised a single prompt in a friction log, and sessions routed temp files away from a directory they were allowed to use until someone probed the guard.

The cost is what makes the check worth its minutes.
Finding a second instance costs a grep, while a rule that shipped narrow reads as settled, gets followed, and is questioned only when somebody applies it somewhere it does not fit.

## Naming a trap and applying it are different acts

Writing down a failure mode feels like having guarded against it, and the feeling arrives immediately while the guarding does not.
So the riskiest claim in a session is often the one made just after articulating the trap it falls into, by the person who articulated it.

**When you flag a trap, the next claim you make in that same conversation is the one to re-derive rather than assert.** That is the check; the observation on its own is not one, because "notice when you feel covered" is not something anyone can run.

Two instances, an hour apart, on 2026-08-12.
A worker flagged that a merge driver writes its advisory to stderr naming the file it resolved, so a reader scanning combined output assembles a conflict list out of chatter.
They then read a filename off that same advisory and reported it as a measured conflict set.
The dispatcher wrote that trap into the playbook on the worker's evidence, then read a `MERGED` field as merged-to-`main` without checking `baseRefName` on the object it had already fetched, and reported a contradiction on `main` that did not exist.

Neither was carelessness, which is the point: both had the mechanism in working memory, and that is what made the next check feel redundant.
If the check does not fire on real cases, demote this to an observation rather than leaving it stated as a rule.

## Conventions

| Convention | Rule |
|---|---|
| **Acronyms** | Spell out on first use, then the acronym in parentheses — "Actions Gateway Controller (AGC)". Subsequent uses may use the acronym alone. Applies to **project-invented shorthand** (`GAG`, `GMC`, `AGC`, `CR`, `CRD`), which a reader cannot look up. It does **not** apply to vocabulary the stated audience already has: on a page whose audience line says "Platform engineer", `GKE`, `RBAC`, `CNI`, `PSA`, `KEDA`, and `VPA` are read, not decoded, and expanding a run of them ("GKE, EKS, AKS, RKE2") costs more than it explains. When an expansion would be clumsier than the plain term, use the plain term (`ActionsGateway` custom resource) instead of the acronym. |
| **Terminology** | One term per concept across all docs. Canonical definitions live in the [glossary](../design/08-glossary.md); link there rather than redefining. |
| **Diagrams** | Prefer ASCII box-art over Mermaid unless auto-layout is a quantifiable win. ASCII renders everywhere and diffs cleanly. |
| **Canonical home + link** | State a fact once, in its natural home, and link to it. Don't restate — copies drift. (Same rule for reuse, since GitHub has no transclusion.) |
| **A link is a claim** | Linking asserts the target covers the thing you linked *from*. Open it and confirm — nothing else will. `mkdocs build` and `check-doc-links.sh` check that a target **resolves**, never that it is on-topic, so a confidently wrong link stays permanently green. Riskiest when adding links in bulk: a roadmap bullet linked "single-replica by design" to [Appendix E](../design/appendix-e-capacity-planning.md), which never mentions replicas — [§2 Architecture](../design/02-architecture.md) is where that design and its job-level HA argument live. Same principle as [cite a measurement](#an-upstream-behavior-claim-cites-a-measurement), applied to cross-references. |
| **Name a cross-reference, don't count it** | Point at a section by name or link — "the negative-assertion rule above", never "the two rules above". A counted reference goes silently wrong the moment a section lands between the counter and the counted, and nothing catches it: `check-doc-links.sh` and `mkdocs build` resolve anchors, never prose. Inserting one testing.md section left the next one's "the two rules above" naming three. |
| **Anchor a cross-file section reference** | Cite another file's section with an anchor, never as prose beside a bare file link. Write `[testing.md § A negative assertion must be able to fail for only one reason](testing.md#a-negative-assertion-must-be-able-to-fail-for-only-one-reason)`, not `[testing.md](testing.md) § A negative assertion...`. Both render alike and only the first is checked, for the reason the row above gives, so the prose form is verbatim the day it is written and silently wrong after any rename. Measured 2026-08-05: 11 such references in the tree against 1,051 anchored links; [Q686](../queue/Q686.md) gates it. |
| **No links to `CLAUDE.md`** | `CLAUDE.md`/`AGENTS.md` is the agent entrypoint only. Human docs never link to it; the dependency direction is one-way (`CLAUDE.md` → `docs/`). Content humans need lives in `docs/` or `CONTRIBUTING.md`. |
| **Link a globally-installed skill to [skills.md](skills.md), never to its source** | A skill under `~/.claude/skills/` is outside every repo and its source is private (`karlkfi/claude-skills`, measured 2026-08-16), so a link there resolves for no reader, and `check-doc-links.sh` cannot see that an external URL is dead where it catches a relative one immediately. Point at that skill's entry in [skills.md](skills.md) instead (`skills.md#verify-claims`). How much the invoking page must then carry depends on the skill's class, which [skills.md § What a contributor without the skills actually loses](skills.md#what-a-contributor-without-the-skills-actually-loses) settles: a working-method skill's page stays sufficient on its own ([testing.md § Diagnosing failures](testing.md#diagnosing-failures-measure-before-asserting-a-root-cause) does this for `verify-claims`), while the three process skills own their process and the page keeps only this repo's deltas plus the gates enforcing them. A **repo-local** skill under `.claude/skills/` is in-tree and links normally ([`release`](../../.claude/skills/release/SKILL.md)). |
| **Table of contents** | Long docs (~400+ lines) carry a `## Table of Contents` after the intro listing h2s (plus h3 for operator docs), in document order. Anchors follow GitHub slug rules (duplicate headings get `-1`/`-2`); verify against the rendered page. **Gated on [upgrade.md](../operations/upgrade.md) only** (Q865): `make upgrade-toc-check` fails a heading that page's index never gained, an entry naming no heading, or entries out of document order. `make doc-links` does not cover this: it resolves the anchors a page writes, so a heading the index never mentions has no link for it to fail. Q911 is the rest of the tree. |
| **Cut filler** | Delete "in order to", "it should be noted that", "please note", and hedging preambles. A pure win for brevity and scannability. |
| **Versioning** | Docs describe `main`: mark unshipped behavior `(planned)`, never as if it ships. API versions (`v1alpha1`→v2…) are documented together on `main` with reference + migration guides — one running gateway serves them at once. Separately, the published *site* is a versioned *tree* (a built copy per release): it defaults to the latest stable release, with `main` behind an opt-in `dev` version — see [website.md § Versioned deploy](website.md#versioned-deploy-mike). |
| **Line breaks** | One sentence per source line. Never hand-wrap a paragraph to a column: run `make md-reflow` and let the formatter place the breaks. What it declines to convert is covered below. |

## Line breaks in prose: one sentence per line

Prose uses [semantic line breaks](https://sembr.org/), one sentence per source line.
A one-sentence edit is then a one-line diff, so prose review stops being a re-wrap hunt and concurrent branches stop colliding on paragraphs neither of them meaningfully changed.
That payoff is large here: 263 non-vendored Markdown files carry roughly 82,000 lines, and almost every session edits some of them.

Do not place the breaks by hand.
[mdreflow](https://github.com/jbeda/mdreflow) is vendored in `tools/` at a pinned version and does it:

```bash
make md-reflow
```

`make md-reflow-check` reports what is unformatted without writing, `make md-reflow-diff` prints the change, and `make md-reflow-explain` names every paragraph the formatter declines, where it is, and why.
The check is in `make check` and costs about a second for the whole tree.
`.mdreflow.yaml` at the repo root holds the configuration: `sentence` mode at `max-width: 0`, excluding the two generated docs, `docs/STATUS.md`, and the `AGENTS.md` symlink. mdreflow always excludes `vendor/`, so the 471 tracked vendored Markdown files are never walked.

### What stays hard-wrapped, and why it stays that way

Measured 2026-08-13 on mdreflow v0.1.7: 99.70% of interior line breaks in the docset's prose sit at a sentence boundary, 13,746 of 13,787, leaving 41.
The formatter declines 38 paragraphs across 8 files, spanning 90 source lines.
"In-scope" excludes the generated docs, which `.mdreflow.yaml` skips.
Re-derive both rather than quoting them: `make md-reflow-coverage` recomputes the percentage and `ARGS=-v` lists all 41, while `make md-reflow-explain` names each declined paragraph with a stable reason code.
Neither number is this page's to hold, and the classification below is reproducible without re-diagnosing anything.

That figure reads lower than the 99.81% this section carried before, and the tree did not get worse: the measurement got wider.
The 41 split two ways, and the split is the finding.
Twenty sit in paragraphs mdreflow declines and reports, which `--explain` accounts for exactly.
The other 21 sit in two MkDocs admonition bodies mdreflow never sees at all: an admonition body reflows while it holds a single block and silently leaves scope once it holds two, reported neither as a change nor as a decline.
A hand-derived count could not see the second group, because nothing named it.

What remains is a small set of guards, each a correctness property rather than a defect, and `--explain` names which one fired.
The one an author must not fight is the link guard: moving a break inside a link construct is where render changes come from, so mdreflow passes over any paragraph where a link's text or destination is left open at a line end:

```
Control plane (GMC rolls to
rc.6, [self-hosted] ready). Second sentence.       → reflows

[t](/a
b) here. Second one.                               → skipped
```

Do not hand-wrap around it.
A paragraph the formatter leaves alone is correctly formatted, and hand-joining to force one through is how a link ends up straddling a break in the first place.

### Coverage is not the check that matters

The coverage figure counts line breaks.
It cannot see whether a page still renders what it should, and that difference is not academic: a MkDocs callout flattened into a paragraph still scores as sentence-per-line, because it is.

Adopting this convention destroyed 7 of 17 callouts on this tree and every gate stayed green, because the output was still valid Markdown that built.
What caught it was rendering the site before and after and diffing every page.
**A change that moves line breaks across the docs is verified by a `mkdocs build` diff, not by `make check` and not by a coverage number.**

Two pages differ structurally under that diff today, and both are the reflow fixing latent bugs rather than causing them: a sentence wrapped so `10.` began a line had been rendering as a stray list item, and a heading missing from a page's nav now appears.
Wrapping mid-sentence is what puts a number, a `---`, or another block-starting token at line start where Markdown reinterprets it.
One sentence per line cannot.

### Measuring the residue

Counting lines that "do not end in sentence punctuation" over-reports badly, and every intermediate figure in this section's history came from that mistake.
It counts YAML front matter, `<details>` blocks, `[label]: target` definitions and immovable marker lines as prose the formatter failed to split, and it misses a sentence ending `.)` or `."`.
Corrected against one tree, that metric read 97.1% where the honest number was 99.34%.
Measure by excluding the constructs mdreflow never reflows, and treat a closing delimiter after terminal punctuation as a sentence end.

Seven mdreflow bugs surfaced while adopting this, all fixed upstream: [#14](https://github.com/jbeda/mdreflow/issues/14), [#15](https://github.com/jbeda/mdreflow/issues/15), [#16](https://github.com/jbeda/mdreflow/issues/16), [#33](https://github.com/jbeda/mdreflow/issues/33), [#37](https://github.com/jbeda/mdreflow/issues/37), and pull requests [#29](https://github.com/jbeda/mdreflow/pull/29) and [#31](https://github.com/jbeda/mdreflow/pull/31).
Between them they took this tree from roughly two thirds converted to its current 20 residue lines, and stopped callouts being destroyed.
[#33](https://github.com/jbeda/mdreflow/issues/33) was fixed structurally rather than by another narrowing: deriving no-break spans from a goldmark parse means linkify is modeled by the parser instead of mirrored by hand, which is what let a code span ending in a URL stop skipping its paragraph.
[#37](https://github.com/jbeda/mdreflow/issues/37) landed in v0.1.7, and the tree reconciles exactly: narrowing the definition zone freed one seven-line bullet in an archived plan doc, worth six residue lines, and one deep-nesting line arrived with prose written since the previous count.
Two more fixes ride along in that release, both found by upstream's own fuzzing rather than here: hard breaks are now confirmed against goldmark's AST ([#39](https://github.com/jbeda/mdreflow/issues/39)) and a footnote body under definition machinery stays frozen ([#41](https://github.com/jbeda/mdreflow/issues/41)).
Neither changes a line in this tree.

The 20 lines that still do not reflow fall into two classes, both settled, so a later session need not re-diagnose them.
Fourteen sit three or more containers deep, which mdreflow declines by design (`deep-nesting`, [#1](https://github.com/jbeda/mdreflow/issues/1)).
That one is written off rather than merely unfixed: raising the depth cap locally corrupted structure, moving content out of its list item into a sibling blockquote, so a human deciding where the breaks go is the right default at that depth.
The other six are `link-ref-def-shape`, and two of them are a paragraph in [website.md](website.md) that documents `[label]: target` syntax, where skipping is the correct answer and no exclusion is warranted, since excluding the file would stop reflowing its other 658 lines.
Do not patch around that zone here: the one-line fix breaks fuzz idempotency, which is why [#36](https://github.com/jbeda/mdreflow/pull/36) was withdrawn.

A third reason code appears in `--explain` and costs no residue: every bullet in [roadmap.md](../roadmap.md) carrying a `<!-- q:QNNN -->` marker holds a raw `<!` opener, so mdreflow declines it (`raw-html-decl-opener`) even though each is already one sentence per line.
The correspondence is one-to-one, so that count tracks the roadmap and is not a figure this page has to hold.
The gate therefore cannot catch a hand-wrap in those bullets.
It is not worth chasing, because `roadmap-check` already enforces their shape and word cap.

Two lessons worth keeping.
A narrowing this repo proposed was wrong in a way a 2.8M-execution fuzz run did not catch: it armed only at paren depth zero, so a link destination opened inside an outer prose paren escaped it and was corrupted.
A large execution count over a corpus that never contained the shape is weaker evidence than one deliberately constructed case.
And a differential test needs a known-answer case of its own: an oracle built on a bare goldmark reported a render change on every hard break, because it omits raw HTML where mdreflow's own instance sets `html.WithUnsafe()`.

## Maintenance

Goal 1 — *correct & current* — is the hardest to sustain because docs rot silently as the code moves.
Keep them current:

- **Update docs in the same change.** After a behaviour change, update every doc it touches before opening the PR — the change-type → docs map is the [doc-update-matrix](doc-update-matrix.md).
  Design-doc updates alone are not enough when a change alters what an operator does, configures, or observes.
- **Keep each `README.md` index complete.** A new doc gets a row in its directory's `README.md` index in the same change (a goal-2 *findability* failure otherwise).
- **Archive finished plans.** When a plan's last STATUS reference is removed, update its [`docs/plan/README.md`](../plan/README.md) row and archive the plan in the same change — `make plan-index-check` enforces this.
  See [maintaining-backlog.md](maintaining-backlog.md#archiving-completed-plan-docs).
- **A backlog change gets its own commit when the change also touches code.** The item is the *why* and the code the *what*; a reviewer should not have to separate them.
  Queue Notes have a hard 250-char cap (lint-enforced).
  Details: [maintaining-backlog.md](maintaining-backlog.md).
- **Links and anchors are checked, twice.** `make doc-links` (in `make check`) fails on broken cross-file links and heading anchors as **github.com** resolves them; `make docs-build` re-checks them as the **published site** does.
  The two sluggers disagree, so a link needs both verdicts — write headings and links that survive each per [website.md § The two link gates](website.md#the-two-link-gates).
  External URLs are out of scope for both — eyeball those.

## Measuring doc quality

"Best docs" implies a way to *know* — quality you can observe, not just assert.
What exists today, and what's proposed:

| Signal | Goal | Status |
|---|---|---|
| `make plan-index-check` — every plan doc is indexed/archived, and no `release-X.Y.md` row still reads as open for a published release (Q812) | 1, 2 | Wired (`make check`). |
| Backlog store lint — frontmatter, rank, 72-char title cap, unresolvable targets | 1 | Wired (`make check`). |
| Per-change doc updates via the [doc-update-matrix](doc-update-matrix.md) | 1 | Convention, enforced in review (PR self-check). |
| `make doc-links` — broken cross-file links + heading anchors, GitHub slugs (Q52) | 1, 2 | Wired (`make check`). The automated guard against link rot. |
| `make docs-build` — the same links as the published site resolves them (Q560) | 1, 2 | Wired (`pages.yml` PR gate). Catches the site-only 404s `doc-links` cannot see. |
| `make em-dash-check`: em-dash density, code and titles excluded (Q654) | 1 | Wired (`make check`). Ratchets against a per-file baseline while [Q650](../queue/Q650.md) clears the debt; see [Enforcing the em-dash rule](#enforcing-the-em-dash-rule). |
| Periodic docs-vs-code drift audit | 1 | **Proposed** — a recurring backstop for what the per-change rule misses. |
| Reader questions (issues, support threads) logged as coverage gaps | 3 | **Proposed** — turns real confusion into Queue items. |

Treat the proposed rows as the roadmap for making quality observable; the highest-value next step is the periodic drift audit, since it catches what the per-change rule misses.

## Authoring checklist

Before opening a docs PR, check against the goals — not just formatting:

- [ ] **Correct & current (1):** matches the code today; every doc the change touches is updated per the [doc-update-matrix](doc-update-matrix.md).
- [ ] **Findable (2):** linked from its directory `README.md` index; cross-links and anchors verified against the rendered page; no orphan.
- [ ] **Complete (3):** answers the question a reader arrives with; no silent gap or undocumented failure mode.
- [ ] **Fit-for-purpose (4):** right type and altitude for its audience (operator vs contributor).
- [ ] **Usable (5):** answer-first; enumerations are lists and comparisons are tables; code/command blocks are copy-paste-runnable with consistent placeholders; no walls of text or filler.
- [ ] **Trustworthy (6):** specific and evidence-backed, not promotional (doesn't read as AI slop); honest about limitations and "not yet implemented"; acronyms expanded on first use; terms match the glossary; no links to `CLAUDE.md`; every [upstream-behavior claim cites a measurement](#an-upstream-behavior-claim-cites-a-measurement) or the gate pinning it.
