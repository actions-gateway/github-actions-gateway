# Q710: overlong paragraphs in the design and operations docs

Split the paragraphs that carry more than one idea, without losing a qualifier.
[`documentation-standards.md`](../development/documentation-standards.md#write-for-scanning) asks for one idea per paragraph at roughly 4–5 lines, which is 60–70 words.
The docset has paragraphs six and ten times that, and the worst of them are in the chapters a new reader is sent to first.

Precision outranks the paragraph rule.
Where a split would cost a condition, a bound, or a named exception, the paragraph stays long and the phase says so.

## Status

| Phase | Scope | Status |
|---|---|---|
| 1 | Top-level prose paragraphs in the numbered design chapters (`01`–`08`) | ✅ Done, three paragraphs deliberately left long |
| 2 | Long list items in `02-architecture.md` (24) and `04-operational-flows.md` (5) | ❌ Open |
| 3 | `appendix-h-v2-api-decomposition.md` (16 prose) and the other design appendices | ❌ Open |
| 4 | `docs/operations/`, starting with `troubleshooting.md` (5 prose, 3 list, 2 quote) | ❌ Open |
| 5 | Oversized `>` admonition blocks, judged on their own terms | ❌ Open |

## The measurement, and how to repeat it

Taken 2026-08-06 against a real Markdown parser, not a line heuristic: **154 paragraphs of 140 words or more across 23 files** under `docs/`, excluding `docs/plan/` and `docs/development/`.

A blank-line heuristic gets this wrong, and quietly.
A list whose items carry no blank line between them reads as one enormous paragraph: an early Python pass reported a 1,155-word paragraph at `operations/tenant-onboarding.md:28`, which is an indented continuation line glued to the eleven checklist items after it.
It also reported 329 hits to the parser's 154.
Anything built on "blank line to blank line" will name paragraphs that do not exist and miss the ones inside list items.

Rebuild the probe over the repo's own parse layer, `devtools/docs/markdown` (goldmark + the MkDocs dialect), and walk it:

- Count `ast.KindParagraph` and `ast.KindTextBlock` nodes.
  That is the unit the rule is about, and it is the unit a list item nests inside.
- Classify each hit by walking its ancestors: `list`, `quote`, `table`, `admonition`, else `prose`.
  The four non-prose kinds are judged on their own terms; a bullet is not a paragraph and forcing it apart is usually wrong.
- Count a code span as one token, and a link as its text only, so a dense reference block is not scored as prose.
- Build from `devtools/` with `GOWORK=off`, since it sits outside the Go workspace ([go-workspaces.md](../development/go-workspaces.md)).

The probe is deliberately not shipped.
Every `devtools/` program here is fronted by a `scripts/` wrapper with a README row and a gate, and Q710 is prose work, not gate work.
Whether paragraph length is worth enforcing mechanically is a separate question, and one this plan does not answer.

### Distribution at the 140-word threshold, before Phase 1

| File | prose | list | quote |
|---|---|---|---|
| `design/02-architecture.md` | 2 | 25 | 0 |
| `design/appendix-h-v2-api-decomposition.md` | 16 | 10 | 0 |
| `design/05-security.md` | 13 | 1 | 7 |
| `design/07-test-plan.md` | 2 | 13 | 0 |
| `design/04-operational-flows.md` | 9 | 5 | 0 |
| `operations/troubleshooting.md` | 5 | 3 | 2 |
| 17 other files | 16 | 12 | 12 |

The Queue row's own per-file counts came from a different scan and do not match these.
The row reads `05-security.md` at 16 and `02-architecture.md` at 6; the parser puts the security chapter at 21 across all kinds and the architecture chapter at 27, almost all of them list items.
Both scans agree on which files are worst.
Use the parser's numbers.

## How a paragraph gets split

The change moves paragraph boundaries.
It does not move meaning.

- **Reach for a list before a split.** "Three disruptions reach this flow, and each is detected differently" wants three bullets, not three paragraphs.
  The same applies to a run-in `(a)`/`(b)`/`(c)` enumeration and to a "two properties" contrast.
- **Split at the bolded run-in heading.** A paragraph already carrying `**Explicit blast radius if compromised:**` mid-body has marked its own seam.
- **Keep a measurement with its claim.** A number, a date, and the run that produced it belong in one paragraph.
  Splitting a result away from what it measured is how a figure ends up quoted without its conditions.
- **Repair a referent the split breaks.** A paragraph opening "Both halves of that…" needs naming once the antecedent is two paragraphs back.
- **Never open a new paragraph with a bare pronoun.** A split turns a harmless mid-paragraph "It is enabled…" into the first word a scanner reads.
  Name the subject.
- **A bold run-in lead is a paragraph-scoped label, so splitting its block silently shrinks what it covers.** This docset uses the run-in where other docs use `####`, and `07-test-plan.md` uses nothing else.
  After a split, the lead governs one paragraph and the rest read as unlabelled.
  Give each new paragraph its own run-in, promote the lead to a real heading, or leave the block whole — but check, because no count shows this.
- **Match the file's own heading depth before promoting anything.** `07-test-plan.md` has no `###` at all, so a `####` under its `## 7.3` both skips a level and invents a convention the page does not use.
- **Do not orphan a bare cross-reference.** A lone "See [runbook] for X" left as its own paragraph is the word count being served rather than the reader.

## How the split is verified

Not by re-reading the diff, and not by the gates. `doc-links` resolves links and `em-dash-check` counts punctuation; neither reads prose, so a dropped qualifier or a mistyped bound passes every check in the repo.

Reconcile the token multiset instead.
Snapshot each file at `HEAD` before editing, then compare `collections.Counter` over whitespace-split tokens before and after.
Every token the diff drops has to be justified one by one: a `;` that became a `.`, an `(a)` that became a `*`, an article dropped by a deliberate rewrite.
A content word in that list is a lost qualifier.

Phase 1 reconciled to +19 tokens across five files, every one accounted for, and to +64 more once the `####` subheadings landed, which the same check showed to be exactly the heading texts and nothing else. `01-executive-summary.md` finished on an *identical* multiset, which is what a pure paragraph break looks like and is the target for every file.

**Reconciliation does not prove the split is correct.** It catches a *deletion*.
It is blind to words the edit *adds*, and that is where the one real defect in Phase 1 came from.
A split moved the sentence "This lets a platform team express 'GPU runners always get at least 5 slots…'" two paragraphs away from `priorityTiers`, and the reopened sentence read "Together these let a platform team express…".
"These" then pointed at the two platform-owned settings named in the intervening paragraph, which are not what expresses 5/20/30.
Nothing was dropped, so the multiset was clean and the gates were green.

So the check has two halves, and the second one is manual: reconcile the tokens, then re-read every paragraph the split *reopened* and ask what its first pronoun now points at.

**A non-zero token delta on a prose split is itself a warning.** Every added word is connective text invented to hold a seam together, and inventing connective text is how a claim the source never made gets in.
Where the delta will not go to zero, that is the signal the paragraph did not want splitting.

## Phase 1: what changed

Three top-level prose paragraphs of 140 words or more remain in `01`–`08`, each left long on purpose.
The corpus went from 154 such paragraphs to 127.

Five files: `01-executive-summary.md`, `02-architecture.md`, `04-operational-flows.md`, `05-security.md`, `07-test-plan.md`.

Three run-in enumerations became lists, because that is what they were:

- the three disruptions reaching the eviction flow ([`04-operational-flows.md`](../design/04-operational-flows.md#worker-pod-eviction-and-auto-retry))
- the GMC's blast radius and its four Secret-read compensating controls ([`05-security.md`](../design/05-security.md#gmc-privilege-escalation-blast-radius-and-compensating-controls))
- the three OR'd DNS peers ([`05-security.md`](../design/05-security.md#dns-exfiltration-side-channel-containment))

One wording fix rode along, because the split exposed it.
The sentence after the three-disruption enumeration read "on detecting either", left over from a two-disruption version of the list.
It now reads "on detecting any of these".

### The three paragraphs left long

Two more joined the `priorityTiers` paragraph once the bold-run-in problem surfaced, both in [`04-operational-flows.md`](../design/04-operational-flows.md): the Q421 drain-recovery block (278 words) and the Q628/Q676 abandoned-completion block (261 words).
Each opens with a bold run-in lead governing the whole block, and each sits under a `###`/`####` where a further heading level would be `#####`.
Splitting them left three and three paragraphs reading as unlabelled, so the splits were reverted.
Same trade as below: precision and structure over the count.

The `priorityTiers` paragraph in [`01-executive-summary.md`](../design/01-executive-summary.md) stays at 152 words, unchanged from before this plan existed.

It was split, and the split broke it.
The paragraph runs field → tier behaviour → concurrency cap → who owns the guardrails → what a platform team can therefore express, and the closing sentence's "This" reaches back across all of it to `priorityTiers`.
Moving the guardrail sentence into its own paragraph put a second candidate antecedent between the pronoun and its referent, and the reopened sentence then read as though the allowlist and the preemption policy were what express "5 slots, burst 20, cap 30".
They are not.

Two later attempts to keep the split and repair the referent both required inventing connective text that made claims the source does not ("the two settings that keep this safe").
At 152 words, twelve over the line, the rule was not worth that.
The paragraph is one idea — it just has a long chain of consequence, which is what the rule's exception is for.

That is the shape to expect in Phase 3, where `appendix-h` reasons about API decomposition across versions.
Everything else in these five chapters had a seam that cost nothing to cut.

Two sections in `05-security.md` came out of the split as walls of short paragraphs rather than one long one, so they gained `####` subheadings: four under [Supply-chain compromise of the worker image](../design/05-security.md#supply-chain-compromise-of-the-worker-image) (9 paragraphs) and three under [Cross-tenant pod preemption via PriorityClass](../design/05-security.md#cross-tenant-pod-preemption-via-priorityclass) (12).
The first paragraph of each stays unheaded as the section lead: a `###` followed immediately by a `####` appears nowhere else in `docs/design/` or `docs/operations/`, and this change does not introduce the first one.

### What the first pass got wrong

Recorded because the corrections are the useful part, and because a reviewer reading only the final diff cannot see them:

- **The `priorityTiers` referent**, above.
  The one defect that changed meaning, and the only one where the answer was to revert rather than repair.
  The first repair kept the split and rewrote around the seam; that is what introduced "the two settings that keep this safe", a purpose the source never states.
  Repairing damage from a split with more prose is how the second defect gets written.
- **A broken contrast pair.** `:8443` (mTLS) and `:8081` (plaintext probes) in `02-architecture.md` explain each other, and the first split put the `TokenReview` rationale between them.
  Regrouped by port instead, with the kubelet-exemption sentence moved to sit directly after the NetworkPolicy rule it is an exception to.
- **A pronoun opener.** "It is enabled…" began a paragraph; now "Tracing is enabled…".
- **An orphaned cross-reference.** A 23-word `See [security-operations.md § …]` had been split off purely to bring the paragraph above it under 140 words.
- **Bold run-in leads left governing only their first paragraph**, at eight sites across the five files.
  Three were bad enough to act on: `**The fake.**` in `07-test-plan.md` went from labelling one paragraph to labelling one of seven, and two blocks in `04-operational-flows.md` lost three each.
  The fake's continuations gained run-in leads of their own, the file's existing idiom; the other two were re-merged.
- **A count that measured the wrong thing.** The first sweep for this reported 22 affected sites by finding every bold lead with an unbolded paragraph after it, never checking whether *this change* produced the tail.
  Comparing tail length before against after gives eight.
  Two `####` promotions had already been made on the strength of the wrong number and were reverted.

## What Phase 2 inherits

`02-architecture.md` is the next target and it is a different shape: 25 of its 27 hits are list items, the "component: description" bullets under §2.1 and §2.2, each 150–250 words.
A bullet that long usually wants a nested sub-list or a short bullet followed by a paragraph, not a split down the middle.
Judge each one against what the bullet is enumerating.

## Related, and deliberately not absorbed

- [Q650](../STATUS.md) covers em-dash density in the same files.
  Different concern, different phase.
  A split that removes a dash because the sentence ended does not count as progress on it.
- PR #1322 rewrote lead paragraphs in several of these files for answer-first structure.
  Phase 1 is rebased onto it.
