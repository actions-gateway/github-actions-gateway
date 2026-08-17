# Q889: Migrate the backlog to the per-item store

**Status:** Phase 1 in progress.
Both agent scripts adopted (Q694, Q814 and Q828 closed); `queue.py` and the rules checker remain.
Phases 2 to 6 not started.

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

Each phase is separately verifiable.
They land as one PR unless the maintainer splits it.

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

**4.
Anchors and docs.** Rewrite 218 `STATUS.md#QNNN` references across 50 docs to the item pages.
This is a bulk mechanical change, so it proves itself by reconciliation, never by an empty leftover grep: assert a known-affected site changed, and reconcile before/after counts.
Then rewrite `maintaining-backlog.md`, `parallel-dispatch.md`, `CLAUDE.md` and the doc-update matrix.

**5.
The website.** Render `/dev/queue/` into the MkDocs theme as a build step, gitignore the generated path, and gate that a committed index never reappears.
Do not add a second Pages deploy: the repo has one site on a custom domain, and a second job publishing its own artifact replaces the first.

**6.
Delete `docs/STATUS.md`, then groom.** Remove or rewrite items the work obsoletes, including anything asserting the table's mechanics.

## Risks

- **Review size.** Roughly 176 new files, 218 rewritten references and 53 consumer changes in one PR.
  This is the accepted cost of decision 7's atomic switch; if it proves unreviewable the fallback is phases 1 and 2 as their own PR behind a drift check.
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
