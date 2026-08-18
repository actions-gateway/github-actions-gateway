# Docs page boundaries: what each adopter-facing page is for

Draw the boundary between `index.md`, `why-gag.md`, `roadmap.md`, and `STATUS.md` on **reader job** rather than on time ("shipped" vs "coming") or audience tier ("public" vs "internal").
Both of the old boundaries cut sideways across the content, which is why the roadmap ended up holding a feature list.

## Status

| Phase | Scope | Status |
|---|---|---|
| 1 | Extract `docs/features.md`; shrink `roadmap.md` to forward-looking only; lint the link discipline | ✅ Done |
| 2 | Split `STATUS.md`'s Deferred + Flake watch onto their own page | ✅ Superseded by Q889 |

## The problem, measured

`docs/roadmap.md` served two reader jobs at once.
Its "Available now" section was 16 bullets / ~1,270 words — 53% of the page — and duplicated a capability list that already existed twice: as `index.md`'s nine pillar cards and as `why-gag.md`'s 17-row comparison table.

Three measurements taken 2026-08-01, before the change:

- **9 of 16 "Available now" bullets contained zero outbound links.** The four longest unlinked bullets averaged 98 words.
  The docs those bullets needed all existed — `why-gag.md`'s table links every one of them — so the cause was not a documentation gap.
  A bullet with nowhere to point keeps explaining itself inline until it is a paragraph.
- **The staleness had nowhere else to go.** `scripts/docs/check-roadmap.sh` already ties both forward-looking sections to `STATUS.md` through `<!-- q:QN -->` annotations, and all 15 markers resolved correctly.
  Its header records why "Available now" is exempt: shipped capability has no Queue row left to point at.
  So the one section the gate could not reach was the one that rotted — the 2026-07-25 audit in that header found six of seven near-term items had already shipped, some frozen into published releases.
- **`STATUS.md` stacks three registers in one file:** the Queue (a priority list read top-down), Deferred, and Flake watch (a parked register grepped when a trigger fires).
  Only the Queue is what a session reads to pick work.
  On 2026-08-01 that was 44 Queue rows above 38 parked ones — a count that moved three times during this change alone, which is why the Queue row states the shape rather than the number.

## The reader-job model

| Job | Reader's question | Page |
|---|---|---|
| J1 Evaluate | "Should I use this over ARC?" | `why-gag.md` |
| J2 Verify | "Can it do X?" | `features.md` |
| J3 Gap-check | "X is missing — is it coming, or do I work around it?" | `roadmap.md` |
| J4 Pick work | "What should I build next?" | `STATUS.md` Queue |
| J5 Bookkeep | "Why is this parked, and what unparks it?" | Deferred + Flake watch |

`index.md` sits above all five as the hook: it names the outcome and routes to the right page, and its pillar cards now link into `features.md` rather than standing as a fourth partial capability list.

## Why `features.md` is a new page, not a section of `why-gag.md`

`why-gag.md`'s table is ARC-relative, and it has to stay that way to make its argument — every row is a claim about a difference.
GitHub Enterprise Server support, air-gapped install, GitOps install, backup/restore, and workload-identity credentials have no ARC row at all: they are capabilities, not differentiators.
Folding them in would blunt the comparison, and the page is already ~3,000 words.

## The format is the fix

A one-time rewrite would rot the same way.
The rule that prevents regrowth is structural: **every capability line in `features.md` carries a link.** One line, one capability, one link to the doc that explains it.
A capability with no doc to link is a documentation gap to file, not a longer bullet.

`scripts/docs/check-roadmap.sh` enforces it (rule 5), alongside the four rules that already gate the roadmap's `<!-- q: -->` annotations.
That makes the failure mode that produced the wall of text mechanically impossible rather than a matter of review attention.

The roadmap needed the same treatment, and rule 6 applies it there under a looser 60-word cap — a roadmap bullet also has to name the gate it waits on, which the feature index never does.
Its five worst bullets ran 74–123 words by explaining the whole approach inline while the plan doc holding that detail went unlinked; all fifteen now fit, and every one links out.

What made that possible is recent: `plan/` is excluded from every release's publication scope, so until [Q561](../development/website.md#publication-scope) shipped `source_links.py`'s per-build absolutization, a roadmap link to a plan doc resolved on `dev` and 404'd on every numbered version.
Appendix G was always safe (`design/` publishes everywhere); plan docs became safe on 2026-08-01.

This is the repo's own [canonical-home-and-link rule](../development/documentation-standards.md#conventions) applied to a page that had drifted from it: state a fact once, in its natural home, and link to it.

## What the roadmap keeps

Only J3, plus the release-mechanics context an adopter needs to read it:

- a one-paragraph pointer to `features.md` for what ships today, carrying the version-selector explanation (a tagged release versus `dev`);
- **Next up** — Queue-backed, gated by `check-roadmap.sh` rules 1–3;
- **Exploring** — Deferred-backed, gated by rules 1, 2, and 4;
- **How priorities are set** — and the pointer to the backlog.

Every remaining bullet carries a `<!-- q: -->` annotation, so the gate now covers the whole page instead of 47% of it.

## Phase 2: splitting the parked register (Q569)

Deferred and Flake watch move to their own page, leaving `STATUS.md` as the Queue.
The [retired-flake ledger](../development/flake-watch-retired.md) set the precedent on 2026-08-01, moving 18 soaked rows to a cold, greppable page at no live-table cost; this applies the same move to the rest of the parked register.

Deliberately a separate change — it is lint and anchor work, not prose:

- `check-roadmap.sh` reads both `## Queue` and `## Deferred` out of one file to classify a bullet; it needs the second path.
- `scripts/docs/lint-backlog.sh` rules span both tables (rule 8 moves a `flake` row from Queue to Flake watch).
- `#QNNN` deep links from other docs resolve against `STATUS.md` today and would need retargeting.
- Rows move *between* the files when a trigger fires, so the two must stay co-edited under the same isolated-commit discipline.

## Out of scope

- `why-gag.md`'s content.
  It was the only one of the four pages already linking properly, and its job is unchanged.
- `index.md`'s structure.
  Its cards gain links into `features.md`; the hero, audience segments, and architecture flow stay as they are.
