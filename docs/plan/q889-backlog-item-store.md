# Q889: Migrate the backlog to the per-item store

**Status:** Phases 1 to 4 merged (#1595, #1596, #1598, #1600); phase 5 is open.
The store is live at `docs/queue/` alongside the table, held to it by `queue-drift-check`, and every reference that can point at an item now does.
Phase 6, the atomic cutover, is the last.

Replace the single `docs/STATUS.md` Queue table with the `session-backlog` skill's per-item store under `docs/queue/`, adopt its Python tooling, and groom the backlog to remove what the move obsoletes.

Supersedes the attempt on [#1534](https://github.com/actions-gateway/github-actions-gateway/pull/1534) (draft, +4150/−1 across 186 files, stale).
That branch carries a Go `queuestore` implementation and its own row Q869, neither of which reached `main`; closing it discards both.

## Why the table has to go

Priority is a line position and the process aims every edit at the same end of the file, so the hot region and the contended region are the same region.
Measured upstream against a 102-row Queue: taking the top *k* rows conflicts every time for any *k* ≥ 2, against 1.9% for two concurrent workers once storage order is decorrelated from priority.
The four merge drivers here reduce the resolution cost and cannot reach the retest cost, because they are per-clone `git config` and never run on GitHub.

## Measured baseline, 2026-08-16

Taken on `main` at the time of writing; re-derive before acting on any of it.

| | |
|---|---|
| Backlog rows (Queue + Deferred + Flake watch) | 176 |
| Machine consumers referencing `docs/STATUS.md` | 53 |
| `STATUS.md#QNNN` anchor references | 218, across 50 docs |
| Progress rows | 31, every one linking a `plan/*.md`; 30 are ✅ |
| `docs/plan/README.md` rows | 157 (60 active + 97 archived), with richer Status cells than Progress |

The consumer count is what decides the migration's shape.
The skill's guidance branches on who reads the table: a table nothing reads but people cuts over in one commit, while a table with live consumers needs both representations committed through an overlap with a drift gate.
53 consumers puts this firmly in the second case **for an incremental rollout**.
But every consumer switches atomically in a single squash-merge, so a one-PR cutover needs verification rather than an overlap gate.
That is the trade this plan takes, and the risk it accepts is review size rather than a broken intermediate state on `main`.

## Decisions taken

Settled with the maintainer 2026-08-16.
Recorded here because the plan is unreadable without them.

1. **`docs/STATUS.md` is retired entirely**, not reduced.
2. **`/dev/STATUS/` becomes `/dev/queue/`**: the skill's rendered queue page, built into the MkDocs Material theme rather than shipped as the skill's standalone HTML.
3. **Progress is deleted as redundant, not migrated.** `docs/plan/README.md` already carries per-plan Status cells with strictly more information (`✅ Done` / `⚠️` / `💤 Parked` plus prose).
   What is unique to Progress is the `⚠️ = at least one open Queue row remains` semantic that `lint-backlog` rule 9 guards; that moves to `plan-index-check`.
4. **Flake watch rows become items.** Recommended `status: deferred` with a `flake` label, keeping the `**Event:** recurs on main after the fix` trigger convention.
   Deferred is the skill's parked-awaiting-a-trigger state, where blocked means waiting on a dependency and would put every flake row in the blocked set a groom reads each pass.
   Settled 2026-08-17: `deferred`, so phase 2 is unblocked.
   The trigger convention is what carries the flake lifecycle, and a `flake` row parked awaiting a recurrence is not waiting on a dependency; putting them in `blocked` would have added every one of them to the set a groom reads each pass, for rows whose whole point is that nothing is expected to happen.
5. **Rules 8, 9 and 11 are ported to a repo-local checker**, since `queue.py lint` has no equivalent.
   `queue.py claims` covers rule 12, and rule 10 is dropped on the measurement below.
6. **No `docs/milestones/` directory.** The milestone/design split already exists as the `milestone` label and as the index's topical sections; a third classification by directory would cost re-homing across 157 plans, re-base every moved file's outbound links, and fork `no-plan-refs-check`, `plan-index-check`, the plan-index merge driver, `.gitattributes` and the path filters, all of which key on `docs/plan/`.
7. **#1587 merges first**; this work branches from `main` after it lands.
8. **One PR per phase, except phase 6, which is atomic.** Settled 2026-08-17, superseding decision 7's single cutover on the evidence the split produced.
   Phases 1, 2 and 3 each surfaced defects a red gate pointed straight at — the MkDocs nav failure, `mdreflow` folding the store README, the untested block-list label form, the truncated blocker clause, a duplicate search silently swallowed by its own `|| true`, the Progress dependency, the merge-driver fixture — and each was attributable because one red signal had one cause.
   Phases 4 and 5 land separately because each is verifiable alone: `doc-links` proves the anchor rewrite, and the render touches only the site.
   Phase 6 cannot be split: deleting the table, retiring the gates that take it as their subject, and grooming have to be one commit, or `main` holds either a live ungated table or gates pointing at a file that is gone.
   The cost accepted is the interim double-write, paid three times so far (Q890, Q891, and #1596's rebase), which ends when phase 6 lands.

## What each rule becomes

The point of porting rather than accepting the loss is that each guards a loss the store does not otherwise prevent.
Which is why the fourth is dropped: measurement found it guards nothing the layout does not already refuse.

| Rule | Guards | New home |
|---|---|---|
| 8: a `flake` row may not vanish | a flake fix deleting the recurrence memory | store check: an item with `flake` may not be deleted, only moved to the retired ledger |
| 9: the last row of a plan flips its Progress | a plan reading as active after its work shipped | `plan-index-check`, which already reads Status cells and already gates linked-iff-live |
| 11: closed label vocabulary | a typo'd label sticking silently | store check over frontmatter `labels` against a declared set |

**Rule 10 was measured rather than ported, and the measurement says do not build it.** Measured 2026-08-17 in throwaway repositories, git 2.55.0, both arms run:

| Layout | The merge | Outcome |
|---|---|---|
| Single table, a row relocated far enough from the deletion to clear the diff context | **exit 0, clean** | the completed row is silently resurrected |
| Per-item store, a `rank` edit against a file deletion | **exit 1, `CONFLICT (modify/delete)`** | the file is left in the tree, so resurrecting it takes an explicit `git add` |

The rule exists because a *relocated* row and a *deleted* row are indistinguishable to a line-position merge.
One file per item makes them a modify and a delete of one path, which git refuses rather than resolves, so the silent default the rule was built for is gone and what remains is a careless resolution of a loud conflict.
That residual belongs to the reconciliation habit [already documented for hand-resolved conflicts](../development/maintaining-backlog.md#a-hand-resolved-conflict-drops-rows-the-markers-never-named), not to a new gate.

The first control run got this wrong in the informative direction: a four-row fixture put the relocation inside the deletion's diff context, so the table arm conflicted too and the comparison read as "no difference".
Reproducing the documented silent case needed twenty rows and a relocation from position 18 to the top.
A probe that cannot reproduce a defect the repo has already met twice is measuring itself.

## Phases

Each phase is separately verifiable, and each lands as its own PR except phase 6, which is atomic (decision 8).

**1.
Tooling, changing nothing.** Vendor `queue.py`; adopt `pr-requeue-eligible.py` and `pr-mergeability-watch.py`; write the repo-local checker for rules 8, 9 and 11.
Wire the gates.
Retire nothing.
**Not** the claim allocator: this repo's `alloc-queue-id.sh` is upstream of the skill's, which cites this repo's 460+ live claims as its own proof point, and `backloglint` rule 12 already enforces what `queue.py claims` does.
The store does not exist yet, so `queue.py lint` reports `0 item(s) OK`.
Read the count, not the exit code: an empty run is a clean bill of health for a store it never read.

**2.
Build the store.** ✅ `queue.py migrate docs/STATUS.md` into `docs/queue/`, with flake-watch and Deferred handling per decision 4.
Verified by round-trip against the pre-migration table, ID sets and order both.
The titles were the work; the migration was one command.

**3.
Switch the 53 consumers.** Retire, repoint or rewrite each: `lint-backlog.sh`/`backloglint`, `check-status-isolation.sh`, `find-duplicate-rows.sh`, `git-merge-status.sh`, `next-task.sh`, `backlog-metrics.sh`/`backlogmetrics`, `queue-unblock.sh`, `alloc-queue-id.sh`, plus `roadmapcheck` and `check-plan-index.sh` which read Queue membership, plus `STATUS_GATES`/`gate-lists-check`, the pre-commit hook, `.gitattributes`, `status-lint.yml`, the path filters, and piped-gate's `docs/STATUS.md` overlap exemption.
Re-counted at the start of the work: **61 files outside `docs/` reference `STATUS.md`**, 40 under `scripts/`, 10 under `devtools/` and 11 of wiring.
The 53 above counted tools rather than files; whichever unit, re-derive it rather than quoting it.

**4.
Anchors and docs.** ✅ 158 `STATUS.md#QNNN` references across 47 files now point at item pages, on top of the 52 the plan index took in phase 3, for 211 store links tree-wide.
Reconciled by count and by `doc-links` rather than by an empty leftover grep, which is what surfaced the classes below.
Thirteen references stay: seven name `Q248` and one names `#progress`, both Progress rather than items, so phase 6 owns them; five are prose placeholders that must never be substituted.
The prose half came to one wrong paragraph in `maintaining-backlog.md` and two lines in `CLAUDE.md`; `parallel-dispatch.md` needed nothing, and `doc-update-matrix.md` referenced the table nowhere at all.

**5.
The website.** ✅ `/dev/queue/` is the store's README with the ordered backlog appended by `hooks/queue_page.py`, which inserts `queue.py render`'s own table.
**The gitignore entry and the committed-index gate this phase planned are both unnecessary**, because appending to a page MkDocs already serves generates no file: there is no artifact to ignore and none to gate.
No second Pages deploy, as planned.
Two dependencies that fail quietly are recorded in [website.md](../development/website.md#the-backlog-renders-at-build-time): the hook must precede `source_links.py`, and its guard counts item rows rather than bytes.

**6.
Delete `docs/STATUS.md`, then groom.** Remove or rewrite items the work obsoletes, including anything asserting the table's mechanics.

## Risks

- **Review size.** Roughly 176 new files, 218 rewritten references and 53 consumer changes in one PR.
  Decision 7 accepted that; the fallback it named — phases as their own PRs behind a drift check — is what actually happened, and decision 8 makes it the rule.
  `queue-drift-check` is that check, so the fallback's condition was met rather than assumed.
- **Rank assignment must preserve priority exactly.** The table's order is the priority; a migration that scrambles it loses information no gate can recover.
  Reconcile the rendered order against the pre-migration table, not just the item count.
- **`Q248` carries an anchor inside the Progress table**, so it is an item ID on a plan row.
  Migration left it there, correctly: it is the one ID the store does not hold.
  It settles in phase 3 with the rest of Progress, when that table moves to `docs/plan/README.md`.
- **The freshness check inverts.** `git log -1 -- docs/STATUS.md` keeps answering after the file is deleted, reporting the date of the commit that removed it, which reads as fresh.
  Every freshness read must move to `docs/queue`.
- **Two linters, one silent wrong-tool error.** `queue.py lint` pointed at a directory holding a table finds no `Q*.md` and reports `0 item(s) OK` at exit 0.

## Phase 2, landed 2026-08-17

The store is `docs/queue/`, 173 items and a README carrying the vocabulary rule 11 reads.
`queue.py lint` reports 173 OK, and `check-queue-rules.sh` reports 173 items with 0 at the merge base.

What the reconciliation established, which is the check worth running because the table's order *is* the priority and a count cannot see it scrambled:

- **173 items written, and the ID sets reconcile in both directions** against the 174 anchors in the table.
- **The one difference is `Q248`**, which the migration correctly left behind: it is an item ID on a *Progress* row rather than a Queue row, which is the edge case this plan already flagged as needing a decision instead of a mechanical move.
- **Order is preserved end to end.** The Queue's 106 rows are the store's first 106 in order, and the remaining 67 match Deferred plus Flake watch in order.

Four probes were written before one measured the right thing, which is worth recording because the first three all looked plausible.
Comparing `render --all` against the Queue section interleaves deferred items by rank; matching `\bQ\d+\b` over the rendered table catches IDs quoted inside Notes text, so `Q811` cited in `Q871`'s note read as a reordering.
Only reading the first column answers the question asked.
The fourth read the wrong first column and returned nothing, which a set comparison scores as agreement: the run was saved by a known-answer control asserting the probe could distinguish a deliberately scrambled order from the real one, which fired on an empty list before the comparison could pass vacuously.

**The re-run on 2026-08-17 found the real phase-2 cost, and it is not the migration.** The flake-watch handling needed no work at all: all 29 rows arrive as `status: deferred` carrying `flake`, which is decision 4 satisfied by `migrate` itself.
What did need work is the title cap.

**62 of the 173 items had titles over the store's 72-character cap**, the longest at 130.
`queue.py lint` fails on every one, so the store cannot land until they are rewritten.
This is not a migration defect: the single table capped the *Notes* cell at 250 characters and never capped the Item cell at all, so adopting the store imposes a constraint this backlog has never been held to.
The cap is deliberate upstream, because a title renders whole in every index row, in `next`'s kickoff prompt, and in any session named after the item, so it is the one field with nowhere to overflow.

**Settled 2026-08-17: rewrite all 62 by hand**, which is what landed.
The cap stays, because the reasons for it are this repo's reasons too, and the alternatives both cost more than they save: raising it means either patching the vendored `queue.py`, forking the file phase 1 spent its whole length un-forking, or duplicating the check locally; and an allowlist of 62 IDs is how a cap stops meaning anything.

The count moved from 61 to 62 between the measurement and the work, and the difference is not a miscount: the first reading was taken after `Q490` had already been fixed by hand in the store that was then discarded, so it reported what remained rather than what the table held.
A number carried across a discarded artifact is a number about the artifact.

Rewriting them is judgement rather than truncation, and the two obvious mechanical answers are both wrong: cutting at 72 characters severs titles mid-identifier, and moving the tail into the body leaves a title that no longer says what the item is.
`Q490` shows the shape, going from a spec name plus its symptom to "A fan-out completion spec cancels a job every delivery completed" at 64 characters, with the spec name moved into the body where it costs nothing.
Where a test identifier was the item's only handle it stayed: `Q549` keeps `E2E_AGC_ScaleSetDrainedWorkerClaimAndRerunLandUnderChartRBAC` at exactly the cap, and `Q809`, which is that spec's mode B, reads as prose and leans on the plan doc both rows link.
Twenty-one of the 62 are flake-watch rows, where the identifier also appears in the trigger, so dropping it from the title loses nothing.

The round-trip is what surfaced all of this, which is the outcome the skill predicts for running one against a live table.
Two smaller things came with it: `v2.0.0` is not a label, though a regex over the `**Labels:**` line reads it as one because it is backticked link text inside `2.0-gate`'s parenthetical, and the derived vocabulary is 18 rather than 19.
Of those 18, thirteen are actually in use across 338 assignments; the store's README declares those plus `2.0-gate` and retires the shipped 1.0 through 1.5 gates, since no open item can carry one.

**The store's own shape was the untested case.** `migrate` writes labels as a YAML block list, and every fixture in `check-queue-rules-test.sh` filed the inline form, so rule 11's coverage never touched the shape it will run against for the rest of the repo's life.
The checker parses both, so nothing was broken; what was missing was any assertion holding it to that.
Found by a probe of my own that read the store with an inline-only parser and reported every label unused, a result only visible because the reconciliation printed both directions rather than the empty one.
A one-sided read would have said "no undeclared labels" and been believed.

**The store meets this repo's own prose gates, and one of them nearly disabled rule 11.** Migrated bodies arrive as single-line paragraphs, which `md-reflow-check` rejects across 152 files.
Reflowing them is safe: `read_item` joins every non-empty body line with a space, so the reconstructed note is identical either way, which was verified by rendering the whole store before and after and diffing rather than by reading the function.
What was not safe was the README: `mdreflow` folded its four `**Status:**`/`**Size:**`/`**Labels:**`/`**New IDs:**` lines into one paragraph, and rule 11 anchors the vocabulary to a line *starting* `**Labels:**`.
`STATUS.md` survives the same treatment because its equivalent block carries trailing two-space hard breaks; the new page now does too, and says why.
The checker exiting 2 is the only reason this was seen at all.
Had an unreadable vocabulary been treated as nothing to check, rule 11 would have passed green while enforcing nothing, for as long as the store exists.

**173 new pages is a website change, and `make check` cannot see one.** Both site scopes build with `mkdocs build --strict`, and `validation.nav.omitted_files` fails on a published page in no nav section, so the store reddened `build` and `release-links` on a branch whose local gate was green.
`/queue/` now sits where `/STATUS.md` and `/plan/` sit: excluded from the stable versions, published on `dev` and declared in `not_in_nav`.
Measured on the local oracle rather than the exit code, since the same green would follow from excluding it everywhere: stable builds 0 queue pages, `dev` builds 176, and `/dev/queue/` serves the store's README until phase 5 replaces it with a rendered index.

**The interim window needed a gate, so phase 2 also ships `queue-drift-check`.** With the table still authoritative and the store already committed, an edit to either side alone is silent, and the silence favours the wrong reading: a groomed table leaves a store that still looks current.
The check re-runs `migrate` into a throwaway store and compares the *loaded* items, so the thing being compared is `queue.py`'s own reading of the table rather than a second parser free to drift from it.
Rank values are excluded and only the order they produce is compared, because a re-rank inside the store is the one operation the store exists to allow and a stricter check would fire on it.
It retires itself: once `docs/STATUS.md` is deleted in phase 6 it passes and says the two can no longer disagree, so nothing has to remember to remove it.

## Phase 3, in progress

**Phase 2's gate is what lets this go incrementally.** `queue-drift-check` holds the table and the store to the same items, fields and order, so for as long as it is green the two are interchangeable and a consumer reading either returns the same answer.
That removes the reason phase 3 had to be atomic: consumers switch a group at a time, and no window exists where the backlog says two different things.

**Repointing a consumer is not a path swap, because the link format changed too.** `rebase_link` rewrites a table's `#QNNN` anchor to `QNNN.md`, so any consumer that parses links, or takes a span up to the next period, reads something different at the new path.
Measured on `queue-unblock.sh`: its blocker clause ran `Blocked by` up to the next period, which in the store stops inside the first link's `.md` — the first blocker still matches and every later one in a list disappears, with no error.
A two-blocker line finds `Q1` and misses `Q2`.
That is the shape to look for in the remaining consumers, and it is invisible to a test that only exercises one blocker, which is why the rewrite came with the script's first suite.

**The table's own gates stay until the table dies.** `lint-backlog.sh`, `check-status-isolation.sh` and `git-merge-status.sh` take `docs/STATUS.md` as their subject rather than as a source, and the table is live through phase 5 and still obliged to match the store.
Retiring them in phase 3 would leave it ungated while the drift gate keeps depending on it.
They belong in phase 6, with the deletion.

**Phase 3's movable set is much smaller than 61, and the rest is not deferral but dependency.** Of the 61 files, 43 hold a live reference and 18 mention the table only in prose.
Sorting the 43 by what actually blocks them:

- **Movable now, and done**: `next-task.sh`, `queue-unblock.sh`, `find-duplicate-rows.sh`, `alloc-queue-id.sh` (both its duplicate search and its ID floor), and `roadmapcheck`.
  Each reads the backlog to answer a question, so the drift gate makes the switch verifiable by output.
- **Blocked on phase 4**, because they assert a *link format* rather than a path: `check-plan-index.sh` and its suite, `git-merge-plan-index-test.sh`, the two hook suites.
  Invariant 3 requires a plan's Status cell to link `../STATUS.md#QNNN`, which is exactly the anchor phase 4 rewrites; moving it earlier would mean a transitional rule accepting both forms that someone then has to remember to tighten.
- **Must outlive the table, so phase 6**: `lint-backlog.sh`, `check-status-isolation.sh`, `git-merge-status.sh`, `backloglint`, the pre-commit hook, and the `status-lint.yml` steps that run them.
  These take `docs/STATUS.md` as their subject, and it is live and drift-gated through phase 5.
- **Incidental**: path-filter lists, `gate-list.sh`, the release tooling and the `semverfloor` tests name the file without reading a backlog from it.

**`.gitattributes` and piped-gate need no change, and that is a decision rather than an omission.** The merge driver and the overlap discount both exist because one table is contended by construction.
A store has no equivalent: two sessions touching different items touch different files, and two touching the *same* item have a real conflict that must not be discounted or auto-resolved by row ID.
So `docs/queue/**` deliberately gets neither, and the `docs/STATUS.md` entries retire with the table.
Written down because the helpful-looking move is to add the store to both lists, which would rebuild the problem the store exists to remove.

**Phase 4's anchors are four classes, not one, and only the largest is mechanical.** Re-measured 2026-08-17, 233 references to `STATUS.md#…` across the tree where the baseline recorded 218:

| Count | Class | Treatment |
|---|---|---|
| 217 | `#Q<digits>` item anchors, 50 files | substitution, but see the depth below |
| 5 | `#QNNN` prose placeholders, 3 files | must **not** be rewritten: they document the format |
| 11 | section anchors `#deferred`, `#queue`, `#flake-watch`, `#progress` | each needs a destination decided, not substituted |

The baseline counted the first class only, so the work was understated by the sixteen references that need judgement.
Both of the others are silent under a naive substitution: a pattern written `#Q[0-9]*` matches the literal `#QNNN` with zero digits and rewrites it to `queue/Q.md`, and a pattern written `#Q[0-9]+` skips every section anchor and leaves it pointing at a file phase 6 deletes.
That is how the classes were found at all: `grep -o 'STATUS\.md#Q[0-9]*'` counted 53 in `docs/plan/README.md` where a `\d+` pattern counted 52, and the one-reference gap was `#deferred`.

**`check-plan-index.sh` reads both sources now, and that is the shape rather than a halfway house.** Its invariant 3 had to move with the 52 anchors in `docs/plan/README.md`, since the cells it reads are the ones that changed.
Its invariant 1 could not: a plan counts as referenced when a backlog row *or* a Progress row names it, and decision 3 deletes Progress rather than migrating it, so 21 active plans whose only reference is a Progress row read as unreferenced the moment invariant 1 looks at items alone.
Measured, not assumed — every one of a five-plan sample appears in the Progress table.
So invariant 1 keeps reading the table and moves when Progress does, in the phase that decides where Progress lands.
The gate's summary line is identical across the switch.

**Depth is the second trap.** 192 of the item anchors sit one directory under `docs/` and take `../queue/`, but 25 sit two deep (`docs/plan/archive/`) and take `../../queue/`.
A single substitution applied tree-wide gets those 25 wrong, and `check-doc-links` is what would catch it — so the rewrite is per-file with the prefix derived from the file's own depth, and the gate is the reconciliation rather than a leftover grep.

**`backlog-metrics.sh` is a phase-6 decision, not a phase-3 one.** `queue.py metrics` replays the store's git history, and the store has 3 commits against `docs/STATUS.md`'s 1155.
Switching it now would report flow metrics over nothing while looking like a working tool.
Deleting the table ends that series whatever happens, so what phase 6 has to decide is whether the old series is frozen into a doc, bridged, or simply allowed to restart.

## Working rules for the remaining phases

Both came out of phase 1 and both cost something before they were written down.

**Check what the local version guarded before adopting an external port.** Every adoption so far has been better than the copy it replaced in every measurable way and has still dropped something the local one covered.
The mergeability watch's upstream suite never exercised `BLOCKED`, where ours did in three cases; the re-enqueue gate stopped recording a verdict for a probe that could not run, where ours wrote `UNMEASURABLE`.
Neither is visible from reading the port, only from diffing its guarantees against the local one's, so the check is: enumerate what the thing being replaced asserted, then find each assertion in the replacement or account for its absence.
`queue.py` and the rules checker are both still to adopt.

**A reading of an upstream file ages, so re-read it before acting.** The skills audit that opened this work was correct when taken and obsolete thirty-five minutes later, when an upstream merge retired a section the audit had deliberately kept.
Nothing signalled it, because a reading is not a measurement that can be re-run against a system; it is a snapshot of someone else's repository.
So state when a read was taken whenever reporting one, and re-read before acting on it rather than trusting the earlier pass.
Five phases of this plan rest on reading files this repo does not own.

## Verification

- Round-trip equality: the store renders to the pre-migration table.
- Count reconciliation: 176 items in, 176 out, and the rendered order matches.
- Every one of the 218 anchors resolves (`make doc-links`).
- The rules-8/9/11 checker is shown **failing** on each violation before it is trusted to pass, one proof per rule rather than one for the checker.
- `make check` green over the final tree.
