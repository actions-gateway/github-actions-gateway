# Q710: overlong paragraphs in the design and operations docs

Split the paragraphs that carry more than one idea, without losing a qualifier.
[`documentation-standards.md`](../development/documentation-standards.md#write-for-scanning)
asks for one idea per paragraph at roughly 4–5 lines, which is 60–70 words. The
docset has paragraphs six and ten times that, and the worst of them are in the
chapters a new reader is sent to first.

Precision outranks the paragraph rule. Where a split would cost a condition, a
bound, or a named exception, the paragraph stays long and the phase says so.

## Status

| Phase | Scope | Status |
|---|---|---|
| 1 | Top-level prose paragraphs in the numbered design chapters (`01`–`08`) | ✅ Done |
| 2 | Long list items in `02-architecture.md` (24) and `04-operational-flows.md` (5) | ❌ Open |
| 3 | `appendix-h-v2-api-decomposition.md` (16 prose) and the other design appendices | ❌ Open |
| 4 | `docs/operations/`, starting with `troubleshooting.md` (5 prose, 3 list, 2 quote) | ❌ Open |
| 5 | Oversized `>` admonition blocks, judged on their own terms | ❌ Open |

## The measurement, and how to repeat it

Taken 2026-08-06 against a real Markdown parser, not a line heuristic: **154
paragraphs of 140 words or more across 23 files** under `docs/`, excluding
`docs/plan/` and `docs/development/`.

A blank-line heuristic gets this wrong, and quietly. A list whose items carry no
blank line between them reads as one enormous paragraph: an early Python pass
reported a 1,155-word paragraph at `operations/tenant-onboarding.md:28`, which is
an indented continuation line glued to the eleven checklist items after it. It
also reported 329 hits to the parser's 154. Anything built on "blank line to
blank line" will name paragraphs that do not exist and miss the ones inside list
items.

Rebuild the probe over the repo's own parse layer, `devtools/docs/markdown`
(goldmark + the MkDocs dialect), and walk it:

- Count `ast.KindParagraph` and `ast.KindTextBlock` nodes. That is the unit the
  rule is about, and it is the unit a list item nests inside.
- Classify each hit by walking its ancestors: `list`, `quote`, `table`,
  `admonition`, else `prose`. The four non-prose kinds are judged on their own
  terms; a bullet is not a paragraph and forcing it apart is usually wrong.
- Count a code span as one token, and a link as its text only, so a dense
  reference block is not scored as prose.
- Build from `devtools/` with `GOWORK=off`, since it sits outside the Go
  workspace ([go-workspaces.md](../development/go-workspaces.md)).

The probe is deliberately not shipped. Every `devtools/` program here is fronted
by a `scripts/` wrapper with a README row and a gate, and Q710 is prose work, not
gate work. Whether paragraph length is worth enforcing mechanically is a separate
question, and one this plan does not answer.

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

The Queue row's own per-file counts came from a different scan and do not match
these. The row reads `05-security.md` at 16 and `02-architecture.md` at 6; the
parser puts the security chapter at 21 across all kinds and the architecture
chapter at 27, almost all of them list items. Both scans agree on which files are
worst. Use the parser's numbers.

## How a paragraph gets split

The change moves paragraph boundaries. It does not move meaning.

- **Reach for a list before a split.** "Three disruptions reach this flow, and
  each is detected differently" wants three bullets, not three paragraphs. The
  same applies to a run-in `(a)`/`(b)`/`(c)` enumeration and to a
  "two properties" contrast.
- **Split at the bolded run-in heading.** A paragraph already carrying
  `**Explicit blast radius if compromised:**` mid-body has marked its own seam.
- **Keep a measurement with its claim.** A number, a date, and the run that
  produced it belong in one paragraph. Splitting a result away from what it
  measured is how a figure ends up quoted without its conditions.
- **Repair a referent the split breaks.** A paragraph opening "Both halves of
  that…" needs naming once the antecedent is two paragraphs back.

## How the split is verified

Not by re-reading the diff, and not by the gates. `doc-links` resolves links and
`em-dash-check` counts punctuation; neither reads prose, so a dropped qualifier
or a mistyped bound passes every check in the repo.

Reconcile the token multiset instead. Snapshot each file at `HEAD` before
editing, then compare `collections.Counter` over whitespace-split tokens before
and after. Every token the diff drops has to be justified one by one: a `;` that
became a `.`, an `(a)` that became a `*`, an article dropped by a deliberate
rewrite. A content word in that list is a lost qualifier.

Phase 1 reconciled to +28 tokens across five files, every one accounted for.

## Phase 1: what changed

Zero top-level prose paragraphs of 140 words or more remain in `01`–`08`. The
corpus went from 154 such paragraphs to 124.

Five files: `01-executive-summary.md`, `02-architecture.md`,
`04-operational-flows.md`, `05-security.md`, `07-test-plan.md`.

Three run-in enumerations became lists, because that is what they were:

- the three disruptions reaching the eviction flow
  ([`04-operational-flows.md`](../design/04-operational-flows.md#worker-pod-eviction-and-auto-retry))
- the GMC's blast radius and its four Secret-read compensating controls
  ([`05-security.md`](../design/05-security.md#gmc-privilege-escalation-blast-radius-and-compensating-controls))
- the three OR'd DNS peers
  ([`05-security.md`](../design/05-security.md#dns-exfiltration-side-channel-containment))

One wording fix rode along, because the split exposed it. The sentence after the
three-disruption enumeration read "on detecting either", left over from a
two-disruption version of the list. It now reads "on detecting any of these".

Nothing was left long on precision grounds in Phase 1. Every paragraph in these
five chapters had a seam that cost nothing to cut. Expect that to stop holding in
Phase 3, where `appendix-h` reasons about API decomposition across versions.

## What Phase 2 inherits

`02-architecture.md` is the next target and it is a different shape: 25 of its 27
hits are list items, the "component: description" bullets under §2.1 and §2.2,
each 150–250 words. A bullet that long usually wants a nested sub-list or a short
bullet followed by a paragraph, not a split down the middle. Judge each one
against what the bullet is enumerating.

## Related, and deliberately not absorbed

- [Q650](../STATUS.md) covers em-dash density in the same files. Different
  concern, different phase. A split that removes a dash because the sentence
  ended does not count as progress on it.
- PR #1322 rewrote lead paragraphs in several of these files for answer-first
  structure. Phase 1 is rebased onto it.
