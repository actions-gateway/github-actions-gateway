# Per-item queue store with a computed priority order

**Status:** filed 2026-08-14.
Phase 1 is being built in the same PR as this plan, so the counts and importer constraints below are corrected from its measurements rather than from the plan's first estimates.

The Queue stores priority as line adjacency in one Markdown table, and the process aims every edit at the same end of that table.
Picking from the top concentrates deletions there; priority-on-entry and flakes-first concentrate insertions there.
The hottest region of the file is therefore the contended one by construction, and a four-worker dispatch batch conflicts every time.

This plan moves each backlog item into its own file under `docs/queue/`, stores priority as a per-item rank key rather than a position, and renders the ordered view instead of storing it.
Two files that are added, edited, or deleted independently cannot conflict under any merge algorithm, on any server, with no driver installed.

## The measurement

Against the live 102-row Queue, using the repo's own measured adjacency rule (a conflict needs one untouched row of separation, [queue-id-allocation.md § What this fixes](../development/queue-id-allocation.md#what-this-fixes-and-what-it-does-not)):

| Concurrent workers | Today, taking the top *k* | If storage order were decorrelated from priority |
|---|---|---|
| 2 | conflict, always | 1.9% |
| 3 | conflict, always | 5.8% |
| 4 | conflict, always | 11.3% |
| 6 | conflict, always | 26.5% |

`docs/STATUS.md` took 214 commits in the week of 2026-W31 and 157 in W32, so the contended file is also the busiest doc in the repo.

## Why the existing machinery cannot close this

[`git-merge-status.sh`](../../scripts/docs/git-merge-status.sh) already resolves Queue rows by ID rather than by line position, and it is correct.
It is also per-clone `git config`, so it never runs on GitHub: the mergeability read behind `mergeStateStatus` and the merge queue's candidate build both take the plain three-way merge ([parallel-dispatch.md](../development/parallel-dispatch.md)).

That places all 1,073 lines of merge-driver code (`git-merge-status.sh`, `git-merge-plan-index.sh`, `git-merge-roadmap.sh`, and the shared `merge-keyed-records.awk`) *after* the cost has already been paid.
By the time the driver runs, GitHub has marked the PR `DIRTY`, which forces a rebase, a force-push, and a full CI re-run.
The driver saves the resolution, which the same doc measures as "two lines, resolve obviously". **The retest is the expensive half, and no merge driver can reach it.**

That is why the retros landed on "never hand one batch two adjacent Queue rows": spacing is the only lever that acts before GitHub looks.
It is also, per that section, an assignment rule and not a guarantee, since any session filing a row above an assigned one re-creates the adjacency and nothing reserves the gap.

## Design

### One file per item

`docs/queue/Q869.md`, YAML frontmatter plus prose body:

```yaml
---
id: Q869
rank: "hzzk"
labels: [ci, debt]
status: ready          # ready | blocked
size: L
blocked_by: []
target: plan/q869-per-item-queue-store.md   # optional
---
```

The file is both the storage record and a published page, which is what makes the inbound-link story work (below) and what retires the 250-character Notes cap.
That cap exists only because a row is a table cell; `CLAUDE.md` currently instructs writing a separate doc whenever the *why* does not fit, and the item page is that doc.

Deferred items live in the same store with `status: deferred` and a `trigger:` field, rather than in a second table.

Three shapes in the live tables that the importer has to respect, measured 2026-08-14 rather than assumed:

- **`target` is optional.** All 102 Queue Item cells are exactly one Markdown link, but 2 of the 67 Deferred cells are bare text.
  Requiring a target would either fabricate a link or drop those two, so the title stands alone when no link is written.
- **Row anchors are not unique to the backlog tables.** The Progress table carries one too (Q248), so the importer is scoped to the `## Queue` and `## Deferred` sections rather than to every `<a id=` in the file.
- **Cells are read from the GFM table AST, never split on `|`.** Queue rows split into 6, 7 or 8 pipe-delimited fields depending on what a cell contains, which is the Q613 finding that put `backloglint` on `devtools/docs/markdown`. `queuestore` parses the same way, and a first pass at these very numbers went wrong by splitting on the pipe.

### Priority as a rank key, not a position

`rank` is a base-36 fractional index.
To place an item between two others, the tooling computes a key strictly between their keys and writes it **in that item's file only**.
Nothing else moves.

Two sessions independently choosing the same rank is not a git conflict.
It is a tie, broken deterministically by ID, so concurrent inserts commute and merge order stops mattering.
That property, not the encoding, is the point.

Base-36 strings rather than floats or gapped integers: inserting repeatedly at the head is the exact pathological case here, since flakes-first sends every new flake to the top, and that is where float precision and integer gaps both run out.

**Plain midpointing does not run out, but it does degrade at the head, and phase 1 measured it rather than assuming otherwise.** An earlier draft of this section claimed string midpointing simply handles the case; it does not.
Minimal-length midpointing below the smallest key prepends a digit roughly every five insertions, so a `rank_test.go` case driving 500 consecutive head insertions grew a key to 100 characters.
Unbounded, but linear in exactly the operation the process performs most.

Phase 1 therefore owes the standard order-prefixed key format, where a leading magnitude character lets the integer part grow downward and keeps head and tail insertion at constant key length, instead of the bare fraction the first implementation used.
The failing test stays as the bound.

Nobody types a rank.
Intent is expressed as `make queue-rank ID=Q869 AFTER=Q863 BEFORE=Q854`, and a helper computes the key.

**Fallback if rank churn proves annoying in practice:** a coarse `tier` field (`now`/`next`/`later`) with a rule-based sort within tier, since flakes-first and blocked-items-skip are already policies rather than hand placements.
Deciding that needs a cycle of real use, so it is not phase scope.

### Rendering the order, on three surfaces

Order is computed, never stored.
It surfaces as:

1. **The site.** A fifth MkDocs hook, `hooks/queue_render.py`, reads the store at build time and substitutes the ordered table into a `<!-- queue:render -->` placeholder in `docs/STATUS.md`.
   This follows the established four-hook pattern and adds no plugin dependency.
   Publication scope stays as it is: the backlog is a `dev`-only page (Q558), `backlog_link.py` already derives the banner link from the built file set, and `exclude_docs` gains `/queue/` on non-dev builds.
2. **The terminal.** `make queue` renders the same ordered table to stdout, and [`next-task.sh`](../../scripts/docs/next-task.sh) reads the store directly.
3. **github.com.** `docs/STATUS.md` keeps its prose, Conventions, and Progress table, plus the placeholder.
   The raw GitHub view shows an unordered `docs/queue/` directory rather than the ordered list. **This is the accepted trade** and the reason the site render is in phase scope rather than deferred.

A committed rendered table, regenerated post-merge on `main` by a serialized job, would restore the ordered github.com view at the cost of a bot commit against a protected branch.
Recorded as a possible follow-up, deliberately out of scope here.

### Inbound links

Roughly 100 `(#QNNN)` and `(../STATUS.md#QNNN)` links across `docs/` resolve today because each row carries an `<a id="QNNN">` anchor.
A site-generated table has that anchor on the site but not on github.com, where [`check-doc-links.sh`](../../scripts/docs/check-doc-links.sh) resolves links the way github.com does.

Per-item pages fix this rather than paper over it: `Q408` becomes `queue/Q408.md`, a real address that resolves on both surfaces and survives the row being reordered.
The rewrite is mechanical and both link gates already cover it.

**Outbound links re-base too, and that is a second rewrite this plan originally missed.** A row's own links are written relative to `docs/`, and an item file sits one level down in `docs/queue/`, so every one of them gains a `../`.
Measured 2026-08-15 across the live item rows: 206 link destinations, of which 135 are relative to `docs/`, 52 already escape it with `../`, and 19 are `#QNNN` anchors pointing at sibling rows.

That fixes the storage form.
Links are held in each item file relative to **that file**, so the page works unrendered on github.com and on the site, and the renderer strips one `../` when it emits the table.
The transformation is deterministic in both directions, including the sibling case, where a `#QNNN` anchor becomes `QNNN.md` and a destination matching `^Q\d+\.md$` becomes an anchor again.
The round-trip test is what holds both halves honest, and `check-doc-links` resolves the result the way github.com does.

## Phases

**Phase 1: format and generator, no cutover.** Add `devtools/docs/queuestore` on the existing `devtools/docs/markdown` parse layer: read item files, compute the order, render the table.
Prove it by round-tripping the *live* table through the store and diffing the re-render against the original.
Ship with a `-test.sh` companion. `docs/STATUS.md` stays authoritative throughout.

**Phase 2: cutover.** Gated on 1.5.0 shipping, for the reason under [Risks](#risks).
Generate the 169 item files, replace both tables with the placeholder, add `queue_render.py`, rewrite the `#QNNN` links, and repoint the consumers: `backloglint`, `backlogmetrics`, `roadmapcheck`, `next-task.sh`, `queue-unblock.sh`, `find-duplicate-rows.sh`, `alloc-queue-id.sh`, `check-path-filters.sh`, and the three workflows.

**Phase 3: retire the contention machinery.** Candidates, each verified against its remaining callers before deletion: `git-merge-status.sh` (156), `check-status-isolation.sh` and its test (126), the isolated `docs(status):` commit rule in `CLAUDE.md` and `CONTRIBUTING.md`, and lint rule 4 (the Notes cap).
Rules 8, 9, 10 and 12 stay, re-expressed over files. `pipedgate`'s driver-owned-files list and [`pr-requeue-eligible.sh`](../../scripts/agent/pr-requeue-eligible.sh) both need their file sets updated in the same change.

**Phase 4: the same disease, twice more.** Out of scope here; filed separately if phases 1 to 3 land clean.
[`docs/plan/README.md`](README.md) (168 rows) and [`docs/roadmap.md`](../roadmap.md) (21 annotated bullets) are also hand-maintained lists whose entries are each owned by exactly one item.
Deriving both from per-item frontmatter retires `git-merge-plan-index.sh`, `git-merge-roadmap.sh`, and `merge-keyed-records.awk`, which is why phase 3 cannot delete the shared awk on its own.

## Verifying the migration

A bulk rewrite proves itself by reconciliation, not by an empty leftover grep ([testing.md](../development/testing.md#a-bulk-mechanical-change-proves-itself-by-reconciliation-not-by-an-empty-leftover-query)).

- **Round-trip equality.** Re-render the table from the generated store and diff it against the pre-migration table.
  Byte equality modulo the anchor form is the strongest available check, and it is the phase 1 exit criterion.
- **Count reconciliation.** 102 Queue plus 67 Deferred items in, exactly 169 files out, taken with a query that spans both tables.
  Those two counts are line-anchored on each row's `<a id=` prefix, which is the one shape a pipe inside a cell cannot perturb.
- **A known-affected site changed.** `Q408` carries inbound `(#Q408)` links from more than one doc; assert every one rewrote, rather than asserting no `#QNNN` remains.
- **Red when it should be red.** Drop one item file and require the gate to fail, so a generator that silently skips files cannot pass.

## Risks

- **Phase 2 lands after 1.5.0 ships**, which is itself after the currently open PRs land.
  That is a sequencing decision, not a technical blocker: a cutover that rewrites every row and ~100 inbound links during a release window would put the backlog and the release ledger in the same blast radius, and the 1.5-gate rows are the ones being read most while the tag is cut.
  It also disposes of the narrower risk, since every open PR deleting a Queue row will have drained by then and none of their rows need migrating by hand.
- **Phase 1 is not gated on that.** It adds `devtools/docs/queuestore` and its round-trip test and changes no existing behaviour, so it can land whenever it is green.
  Keeping it independent is the reason the phase split puts the generator before the cutover rather than shipping them together.
- **`mkdocs build --strict` is a gate.** The link rewrite and the new hook both have to keep the two link gates and the strict build green.
- **The ordered view leaves github.com.** Accepted, mitigated by the site render and `make queue`, revisitable via the post-merge-render follow-up.
- **Phase 3 deletes safety machinery.** Rule 10 (a deleted row may not reappear) exists because a botched merge resolution silently reopens finished work.
  It stays, re-expressed as "a deleted item file may not reappear"; only the rules that exist purely for *table* contention are candidates for deletion.
