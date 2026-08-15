# Agent reference: Maintaining the backlog

`docs/STATUS.md` is the single source of truth for project progress and priorities.
It is high-contention — almost every session edits it — so keeping churn low matters as much as keeping it accurate.

The format and process come from the globally-installed **backlog skill** (agents: invoke the `backlog` skill for the full playbook: grooming checklist, staleness signals, parallel dispatch, migration).
The repo vendors the skill's tooling so the rules hold for every contributor, with or without the skill:

- [`scripts/docs/lint-backlog.sh`](../../scripts/docs/lint-backlog.sh) — enforces every format rule below.
  It selects the file and maps the environment interface onto flags; the rules themselves are [`devtools/docs/backloglint`](../../devtools/docs/backloglint/), whose package comment is the canonical rule list.
  Rows are read from the GFM table AST rather than split on a literal `|`, and cell lengths count characters rather than bytes (Q613).
  Runs in `make check` (`make lint-backlog`), CI ([`status-lint.yml`](../../.github/workflows/status-lint.yml)), and the pre-commit hook.
  The hook's `--staged` mode also rejects a staged set that carries `docs/STATUS.md` alongside other files — the index half of [the isolation rule](#isolated-commits-and-what-actually-enforces-them).
- [`scripts/docs/check-status-isolation.sh`](../../scripts/docs/check-status-isolation.sh) — fails a branch whose commits mix the backlog with anything else.
  Backs `make status-isolation-check`; runs in `make check`, `make status-gates`, and [`status-lint.yml`](../../.github/workflows/status-lint.yml).
  [Why it exists next to the hook](#isolated-commits-and-what-actually-enforces-them).
- [`scripts/docs/alloc-queue-id.sh`](../../scripts/docs/alloc-queue-id.sh) — allocates a new Q-ID (`make queue-id TITLE="…"`) by claiming a ref on the remote, so concurrent sessions never take the same one.
  Rationale, the alternatives weighed, and what it does *not* fix: [queue-id-allocation.md](queue-id-allocation.md).
- [`scripts/docs/find-duplicate-rows.sh`](../../scripts/docs/find-duplicate-rows.sh) — the near-duplicate search that allocation runs before it claims an ID.
  Advisory: it never blocks a filing.
  [How it is calibrated](#search-before-you-file).
- [`scripts/docs/git-merge-status.sh`](../../scripts/docs/git-merge-status.sh) — a git merge driver that resolves Queue-table conflicts by row ID rather than by line position, and falls back to ordinary conflict markers for anything ambiguous.
  Its siblings [`git-merge-plan-index.sh`](../../scripts/docs/git-merge-plan-index.sh) and [`git-merge-roadmap.sh`](../../scripts/docs/git-merge-roadmap.sh) do the same for [`docs/plan/README.md`](../plan/README.md), keyed on the plan path, and for [`docs/roadmap.md`](../roadmap.md), keyed on each bullet's backlog annotation.
  One `make merge-driver` per clone installs all three; a no-op until then.
  [Details below](#the-merge-driver-resolve-queue-rows-by-id-not-by-line-position).
- [`scripts/docs/next-task.sh`](../../scripts/docs/next-task.sh) — prints a kickoff prompt (or `--title`) for the top ready 🔲 Queue row, for starting a fresh session on the next task.
- [`scripts/docs/backlog-metrics.sh`](../../scripts/docs/backlog-metrics.sh) — replays the file's git history into flow metrics (throughput, cycle time, prune ratio, aging WIP).
  Read-only.
  The replay reads each diff line's cells through the shared Markdown parse layer, so an escaped pipe in a cell cannot shift a row's fields (Q614).

## The shared process, in brief

- **Position is priority.** The Queue is read top-to-bottom; pick from the top.
  Decide a new item's priority *before* inserting it and place the row where it belongs — never append by default.
  Rank by severity/blast radius, then leverage (what it unblocks), ready over blocked; size only as a tiebreaker.
- **Two Queue states only: 🔲 ready and 🚫 blocked.** Done rows are **deleted** (git is the archive), "started" is signaled by the open PR (run `gh pr list` before picking; skip rows an open PR covers), and parked rows live in the Deferred table.
- **Verify 🚫 blockers before treating a row as blocked** — a prior session may have shipped the dependency without flipping the row; grep for its deliverables.
  Cross-item blockers are machine-readable: a 🚫 row's Notes start with `Blocked by [QN](#QN)`, and `make queue-unblock ID=QN` lists every dependent when the blocker lands.
- **Verify the defect a row asserts before implementing it.** A row's Notes are written at filing time from an observation that was never re-checked, so grep for the claimed defect the same way you grep for a blocker's deliverables — a prior session may have closed it without flipping the row, and Notes carry no observed-vs-suspected marker.
  Q506's row named a `noproxy` GHES gap that Q322 had already fixed; taking the row at its word would have "fixed" a non-bug and missed the real one next to it.
  An audit row inheriting an unverified premise is the sharpest case, but the rule is general.
- **Search before you file** — `make queue-id` does it for you, and reading its candidates is the part that is on you.
  Nothing else catches a semantic duplicate: two rows describing one problem in different words pass every lint, and both get worked.
  [Details and calibration below](#search-before-you-file).
- **Allocate IDs with `make queue-id TITLE="…"`**, which claims a `refs/queue-ids/QN` ref on the remote.
  There is no counter line in the file: a shared mutable counter handed concurrent sessions the same ID and conflicted on the same line by construction, forcing a renumber ([Q382](queue-id-allocation.md#why-the-counter-had-to-go)).
  IDs are sparse, stable, never reused or renumbered, and never get sub-IDs (`5a`) — a trackable child gets its own top-level ID.
  The `Q` prefix keeps `Q44` from auto-linking to PR/issue #44; use the bare ID in commits and PRs.
- **Notes are present tense, ≤ 250 characters (hard cap, counted as the cell is written — an em dash costs one, an escaped `\|` costs two); past 200 characters the row must link a doc** from its Item or Notes cell — a `#QN` sibling anchor doesn't count, since sibling rows are capped too.
  No merged-PR lists or "SHIPPED" narration — history lives in `git log` and the plan doc.
  The same caps apply to Deferred trigger cells.
  Write for a skimmer: cut detail and link a doc rather than compressing into fragments.
- **A literal pipe in any cell is written `\|`, code spans included.** GFM reads a raw `|` as a column separator wherever it appears, so it splits the row into more cells than the header declares and everything past the header's last column is dropped from the rendered table, on github.com and on the site both.
  Nothing about the source line looks wrong, which is why it needs a gate: `lint-backlog` rule 13 compares each row's own width against its table header.
  Measured 2026-08-14: Q866's Notes rendered as far as its opening backtick, losing the remaining two thirds, and the truncation also hid an over-cap cell from rule 4. **Budget for that second half: escaping the pipe is not a one-character fix.** Every rule downstream had been reading the stub before the pipe, so the cell they measure changes the moment it renders whole, and one of them can newly fail on a row nobody edited.
  Q866's Notes went from a measured 100 characters to 269 against the 250 cap, and had to be trimmed in the same edit.
- **A row never cites a count of the backlog.** "42 Queue rows", "60 parked" and friends go stale on the next filing — the file they measure is the one thing guaranteed to change under them, often the same day.
  State the *shape* instead ("the Queue is read top-down; the parked rows are only grepped on a trigger"), and put any dated figure in the linked plan doc, where a point-in-time measurement belongs.
  Q569's row was corrected twice in one session — 36 → 42 → 44 — before the count came out entirely.
- **Deferred rows carry a concrete revive trigger**, tagged by source: `**Demand:**` (an outside ask) · `**Event:**` (an observable outside-our-control condition) · `**Decision:**` (our own call — grep `**Decision:**` for what we could move on unilaterally).
  When the trigger fires, move the row back into the Queue at the position it then deserves.
  A non-commitment belongs in [appendix-g](../design/appendix-g-future-enhancements.md), not Deferred.
- **`docs/STATUS.md` edits are isolated commits** — never mixed with code or plan-doc changes, even when completing an item mid-feature ([enforced](#isolated-commits-and-what-actually-enforces-them)).
  Use `docs(status):` subjects, and name the removal reason with a fixed verb — `complete QN`, `prune QN`, `merge QN into QM`, `defer QN` — so metrics can tell throughput from garbage collection.
  Batch bulk additions (one audit's discoveries) into one commit; keep reshuffles separate from additions.
  When a rebase or merge conflicts on this file, resolve it via the [fast path](#resolving-a-statusmd-only-conflict-verify-cheap-push-now) below.
- **M/L items get a plan doc** under `docs/plan/`, linked from the Item cell.

## Isolated commits, and what actually enforces them

**The rule: a commit that touches `docs/STATUS.md` touches nothing else.** Not "no code" — nothing.
A plan doc, a roadmap bullet and the row deletion they belong to are three commits, in whatever order.
That literal shape is also the shape practice already has: across the last 80 merged PRs, all 74 commits touching the file were backlog-only, so encoding the sentence as written breaks no workflow anyone is using.
Several isolated backlog commits in one PR is ordinary and stays green — [#1239](https://github.com/actions-gateway/github-actions-gateway/pull/1239) landed three.

**`git commit --amend` after the row change amends the row change.** Order is free to the gates, but the backlog commit is usually the last one written, so it sits at `HEAD`; a later fix to the other half lands on it and builds exactly the mixed commit this rule forbids.
Nothing warns you, because the amend is a perfectly ordinary command that did what you asked.
Read `git show --stat HEAD` before amending anything, and write the row change once the rest is settled rather than mid-iteration. `check-status-isolation.sh` does catch the mix, but on the pushed branch, a CI cycle after the cheap moment to notice.

Two mechanisms enforce it, and neither subsumes the other:

| | pre-commit hook (`lint-backlog.sh --staged`) | `make status-isolation-check`, and status-lint.yml |
|---|---|---|
| Fires | at `git commit`, before the mistake exists | on `make check` and on the pushed PR |
| Reads | the **index** | the **commits** |
| Bypass | `--no-verify`; absent entirely until `make hooks` | none |

The hook is the better feedback loop and the weaker guarantee, and reading the index rather than the commit it produces is what makes it structurally blind to the case that motivated the second half: stage only `docs/STATUS.md`, `git commit --amend` onto a code commit, and the hook sees a clean index while git writes a commit carrying both.
That is measured, and pinned as a test case in [`check-status-isolation-test.sh`](../../scripts/docs/check-status-isolation-test.sh).

**The commit half scans a PR, never `main`.** Merges here are squash-merges, so a PR that kept its backlog edit in its own commit still lands on `main` as one commit touching `docs/STATUS.md` and everything else — mixed by construction.
The individual commits exist only while the PR is open, which makes that window the one place the property is both true and checkable; a scan of `main`'s history would fail on every merge and mean nothing.

**Scope, and a commit that predates the gate.** The range is `merge-base(base, HEAD)..HEAD`: the commits the branch adds, never one already on the base.
So every commit the gate can fail belongs to the branch being failed, and `git rebase -i` can always split it — there is no history it is asked to judge and cannot fix, which is why an older mixed commit gets no exemption.
When rewriting genuinely costs more than it buys, `BACKLOG_ALLOW_MIXED_COMMITS="<sha> ..."` admits named commits, the same deliberate-and-reviewable shape as `BACKLOG_ALLOW_RESURRECT` and friends.
Merge commits are skipped: their file list depends on which parent you diff against, so "what this commit touched" has no single answer for them.

### A gate label and its roadmap bullet are two commits, and the first one is red

`roadmapcheck` rule 7 requires a row labelled `X.Y-gate` **and** carrying `feature` or `security` to be named by a roadmap bullet, so adding the label and adding the bullet are one change in intent.
Isolation splits them regardless: the label lives in `docs/STATUS.md` and the bullet in `docs/roadmap.md`, so they cannot share a commit.

**A gate label answers two questions, and only one of them obliges a bullet.** Release scope is not all one kind: a capability or a security fix is what someone upgrades *for*, while the CI, test, docs and dogfood work that also blocks a tag is process.
Requiring a bullet for both put our own release harness on the page people read to evaluate the product, so the obligation follows `feature`/`security` rather than the gate label alone.
Gate a process row freely; it stays out of the roadmap and the release still waits for it.

Read on its own, the `docs(status):` commit therefore fails rule 7, which looks alarming enough to go hunting for a way around it.
There is none, and none is needed: `roadmapcheck` reads the **working tree**, not each commit in turn, so what `make check`, CI and the merge queue all judge is the pair together.
Order the two commits however you like and check the tip.
Only `status-isolation-check` reads commits individually, and it has no opinion about roadmap bullets.

## Closing a row: what else moves

Deleting a Queue row is the one backlog edit with reach outside `docs/STATUS.md`.
The row is an anchor, a plan doc's last reference, and often a roadmap bullet's reason to exist, so removing it breaks things in files the closing change never opened.
Every one of these is caught by a gate, and every one is cheaper to do up front than to diagnose from a gate failure pointing somewhere unexpected.

Work through all four:

1. **De-link the ID wherever the repo cites it.** `grep -rn "STATUS.md#QNNN" docs/` finds every anchor that is now dead (`make doc-links`).
   Rewrite them as a **bare `QNNN`**, the form the Archive rows in [`docs/plan/README.md`](../plan/README.md) already use.
   Keep the prose; only the link goes.
   In an active plan's Status cell the de-link is gated rather than tidy: `make plan-index-check` requires a live row to be linked and a closed one to be bare, so the anchor dying is what puts the cell in front of someone (Q800).
   Re-read the whole cell while you are there, since it is a rollup no individual row owns.
2. **Delete the roadmap bullet, continuation lines included.** A forward-looking bullet exists because the row does ([rule 7](#a-gate-label-and-its-roadmap-bullet-are-two-commits-and-the-first-one-is-red)), so it goes when the row does.
   Its indented follow-on lines are part of the same list item: leave one behind and Markdown attaches it to the **previous** bullet, whose word count then breaks the cap. `make roadmap-check` names the stray line and the bullet it landed on (rule 12), and reports the cap over the line span it actually counted, so a violation on a bullet you never touched no longer reads as a pre-existing failure.
3. **Archive the plan doc if this was its last `STATUS.md` reference**, per [the protocol below](#archiving-completed-plan-docs), whose step 4 is the one most often missed: dropping a level into `archive/` re-bases **the moved doc's own outbound links**, not just the links pointing at it.
4. **Update the plan's `docs/plan/README.md` row** in the same change, moving it to the Archive section.

The cluster is wider than the docs tree: Q790 was the same shape in the merge tooling, where piped-gate's `docs/STATUS.md` overlap exemption discounted the path unconditionally and so stayed silent on exactly the row *deletion* the driver refuses to resolve: a row deleted on one side and edited on the other.
When something new mishandles a closing row, it belongs with these rather than as a fresh curiosity.

### Repurposing an ID is a closure with every step skipped

A measurement that refutes a row's asserted defect usually hands you a different one, and the cheapest edit is to rewrite the row in place: same anchor, same ID, new title, new Notes.
That one edit is a closure and a filing wearing a single row, and it skips both halves.
The four steps above never run, because nothing looks deleted, and `make queue-id` never runs, because no ID is being taken, so [the duplicate search](#search-before-you-file) never happens either.

**An ID is bound to the observations the row was filed on, not to its title.** Retitling is routine and stays inside the row: [a symptom title earns its mechanism](#a-rows-asserted-defect-is-a-claim-not-a-finding) once the mechanism is measured, which is how Q553, Q703 and Q827 all reached their final titles.
The line is the evidence, not the wording.
When the measurement leaves the row's own observations describing a *different* defect, that row is finished: retire it with the refutation recorded, and let the new defect take a new ID.

#1441 is the instance.
Q809 was filed on three `e2e-calico` failures read as NetworkPolicy enforcement negatives; diagnosis attributed all three to one scale-set drain-recovery spec, which is [Q549](../STATUS.md#Q549)'s mode B. The diagnosis was right and the retitle in place cost three things anyway:

- **Every citation of Q809 re-pointed silently.** A repurpose leaves the anchor alive, so `make doc-links` and rule 5 see nothing: they catch a dead anchor, never a live one that has changed meaning.
  Seven files cited Q809 in its original sense at the moment it changed meaning, and three of them still asserted the refuted failures two days later, in a workflow comment, a spec comment and `testing.md`.
  A closure would have run `grep -rn Q809` over all seven as [step 1](#closing-a-row-what-else-moves).
- **The refuted hypothesis left the file.** `calico` appears nowhere in `docs/STATUS.md` afterwards, so nothing recorded that the enforcement negatives had been suspected and cleared, and the next session to see a calico failure starts cold.
  [The ledger](flake-watch-retired.md) is where that belongs.
- **The cross-link was care, not mechanism.** #1441 named Q549 by hand.
  Passing the new title to `make queue-id` returns Q549 at 0.43 on the same target, which is the prompt [nothing else supplies](#two-rows-on-one-defect-cross-link-them-and-say-which-owns-the-measurement).

**Rule 8 pushes toward the repurpose, which is worth knowing before it does.** It keys on the ID being present *anywhere* in the file, so a `flake` row whose defect is swapped out reads as correctly preserved.
Measured on a two-commit probe against the shipped linter: the repurpose passes, and deleting the same row fails with rule 8 naming `BACKLOG_ALLOW_FLAKE_DELETE`, so the honest closure is the one that hits a red gate and the shortcut is the one that goes green.
For a flake row whose defect was **refuted rather than fixed**, that override is the correct move: retire the row to [the ledger](flake-watch-retired.md) with no fix PR, which is a third route out of Flake watch alongside [soaked and obsolete](#retiring-a-flake-watch-row).

**Nothing gates this, and a title check is the wrong gate to reach for.** Scoring every Queue row's before/after title across the backlog's whole history with the same matcher `make queue-id` uses (1,108 commits touching `docs/STATUS.md`, 80 title changes) leaves 27 whose new title does not match its old one.
One is this repurpose.
Nine are pre-allocator duplicate-ID renumbers across four collisions, the last on 2026-07-05, a class [rule 12](queue-id-allocation.md#reserving-not-reporting) now prevents at filing time.
The remaining 17 are ordinary retitles of a live row.
A gate keyed on title distance would therefore be wrong about two thirds of the time, and wrong specifically on the practice the section above asks for.
The discriminator is whether the row's evidence still describes its defect, and no linter can read that.

## Search before you file

The rule used to be "grep the Queue and Deferred tables first", and it failed three times: [Q442](https://github.com/actions-gateway/github-actions-gateway/pull/847) and [Q456](https://github.com/actions-gateway/github-actions-gateway/pull/893) both duplicated Q440, and [Q635](https://github.com/actions-gateway/github-actions-gateway/pull/1186) duplicated Q619.
Every one satisfied the lint — a semantic duplicate is a well-formed row — and every one was filed mid-task, as a side effect of other work, exactly when the doc carrying the rule was not in context.
A rule that fails at the same seam three times wants a mechanism.

`make queue-id` is the mechanism, because it is the one chokepoint every filed row passes through, and it takes the title:

```bash
make queue-id TITLE='`doc-links` never reads a new doc until it is staged'
```

Single-quote it: these titles are full of backticks, which double quotes would hand to the shell as command substitution.
A title carrying an apostrophe as well is easier to pass straight to `scripts/docs/alloc-queue-id.sh`, which takes it as a plain argument.

It searches first and claims second, so recognising a duplicate costs no ID.
Candidates print to stderr, so `ID=$(make queue-id TITLE="…")` still works, and **nothing is blocked**: the filer routinely knows something the matcher cannot, such as that two rows sharing a file are genuinely separate defects.
Say which, in the new row's Notes.

**The title is mandatory, and there is no untitled batch form.** An optional argument is a gate nobody passes through, and `-n 3` was one: it claimed IDs without naming a single row.
Several rows at once means several titles: `scripts/docs/alloc-queue-id.sh` takes one argument per ID and searches each on its own, which is what a retro filing four rows actually wants.
Nothing automated calls the target, so making the title mandatory changed only the lines that document it.

**Every path through it claims, and a row whose ID holds no claim fails the lint.** `PEEK=1` used to report the next free ID without taking it, which two concurrent sessions read identically: the counter this mechanism replaced, behind a flag.
It is gone, and rule 12 catches the other way to obtain an unreserved ID (reading the file's highest and adding one) at the commit that files the row rather than at the rebase that collides.
What that cost in practice, and the one case rule 12 still cannot see: [queue-id-allocation.md § Reserving, not reporting](queue-id-allocation.md#reserving-not-reporting).

`TARGET=<link>` is optional and worth passing when the Item cell's link is already decided.

### Escalating a class observation is filing, so search first

`make queue-id` is a chokepoint for *filed* rows only.
Raising "these N failures look like one shared cause" in a message to the maintainer, or to a dispatcher, reaches the same reader with the same weight and passes through no matcher at all.
A message to a human is a filing with the safety rail removed.

It fired twice on 2026-08-12, in both directions.
A worker raised the "X-test fails under `make check`, passes standalone" family as an unfiled class; a dispatcher relayed it, citing Q596's 2418 runs and Q703's 240 as evidence that no class row existed.
[Q738](../STATUS.md#Q738) already said "measured across this family" and already named those two rows, so the maintainer was asked to decide something on the premise that it was unfiled.
The observation was real and the escalation still cost a wrong premise, which is the same shape as the three duplicate filings above: right finding, no matcher in the path.

Before escalating, run the search you would run before filing, and say what it returned.
The cheap version is to pass the title you would have used to `make queue-id`, which prints its candidates to stderr and claims an ID you can throw away.
Naming the near-miss rows is what lets the reader tell a genuinely new class from the half that is already tracked.

**A clean `make queue-id` is not an all-clear, because the matcher is lexical and a duplicate is semantic.** It keys on shared content words and the Item link, [calibrated below](#what-it-keys-on-and-why) against three pairs that had both.
Two rows describing one page's defect in different vocabulary share neither.
Q835 was filed on 2026-08-12 after `make queue-id` returned no candidates at all; reading the Queue by hand found Q832, filed the same day, measuring a third cause of the same undercount on the same page.
The search narrows the set worth reading, and reading is still what recognises the duplicate.

### What it keys on, and why

From the three pairs, not from a guess about what similarity means here:

| Pair | Shared content words | Item link |
|---|---|---|
| Q456 *"The GMC CRD manifests are stale and no gate notices"* / Q440 *"GMC CRD manifest drifts from the AGC types it embeds"* | 3 | same |
| Q635 *"`doc-links` never reads a new doc's own links until it is staged…"* / Q619 *"Three gates scan tracked files only, so a new file misses its own `make check`"* | 4 | same |
| Q511 *"Two live-GitHub runs collide invisibly…"* / Q500 *"Two concurrent live-GitHub runs collide on the fixture repo"* | 5 | different |

Neither signal alone covers that: two pairs agreed on the link and barely on the words, one agreed on the words and not the link.
So a row is a candidate on **either** route — ≥3 shared content words at ≥0.40 containment, or an exact link match at the lower bar of ≥2 words and ≥0.25.
The shared-word floor is what a ratio alone cannot supply, because containment divides by the *shorter* title: a five-word row scores 0.40 on two incidental words, which is exactly how the novel-row control gets rejected.

Deferred and Flake watch are searched too, because a row duplicating a parked item is the same mistake and those are the tables nobody greps.
Notes cells are deliberately not matched: folding a 250-character Notes cell into a row's token set can only raise every score, inflating the ranking without adding a cut.

### Whether it is noisy enough to ignore

An advisory that fires constantly is worse than none, so the thresholds are measured rather than asserted. `scripts/docs/find-duplicate-rows.sh --audit` runs every shipped row back through the same scoring path the search uses, and prints what flags:

```bash
scripts/docs/find-duplicate-rows.sh --audit
```

**Roughly one row in five surfaces a candidate when filed, and every pair it flags is topically adjacent rather than a nonsense match.** The snapshot behind that, on 2026-08-04 (72 Queue + 31 Deferred + 15 Flake-watch rows): 11 flagged pairs out of 6,903.
The rate held across every backlog state this was measured in — the Queue turns over faster than any fixed count survives, which is why the figure is a dated instance and `--audit` is the live answer.
Two of the eleven look like real duplicates nobody caught: Q663 and Q612 are both `check-doc-links` defects, and Q660 and Q588 are both the doc-update-matrix sending a row into a `scripts/README.md` table that does not exist.

Loosening either ratio by 0.05 roughly doubles the count.
Re-run the audit before changing a threshold.

## The merge driver: resolve Queue rows by ID, not by line position

Most `docs/STATUS.md` conflicts are an artifact of the file's shape rather than a real disagreement.
A plain three-way merge decides by line position, and the process puts every edit in the same place: pick from the top, insert at the priority the item deserves, flakes first.
One untouched row of separation merges cleanly; **adjacent** rows do not, so a four-worker dispatch batch that takes rows 1–4 conflicts by construction ([the measurements](queue-id-allocation.md#what-this-fixes-and-what-it-does-not)).

[`scripts/docs/git-merge-status.sh`](../../scripts/docs/git-merge-status.sh) is a git merge driver that decides the Queue table by **row ID** instead: a row deleted on either side is deleted, a row added on either side is present, a row changed on one side takes that change, and row order is rebuilt from whichever side reordered.
It runs on local merges, rebases, cherry-picks and stash applications — everywhere the pain is.

**One-time setup, per clone:**

```bash
make merge-driver
```

`.gitattributes` already routes the file to `merge=backlog`, but git deliberately refuses to let a tracked file define a driver's *command* — that would be remote code execution on clone — so the config half is per-clone and opt-in. **Nothing requires you to install it:** until you do, the attribute names an undefined driver and git silently uses its built-in three-way merge, which is exactly the pre-driver behaviour.
[`scripts/dev/setup.sh`](../../scripts/dev/setup.sh) installs it for you, as it does the git hooks.

**What it refuses to resolve.** Every uncertainty ends the same way — the plain three-way merge re-runs and its conflict markers stand, with a one-line reason on stderr:

| Situation | Outcome |
|---|---|
| A row changed on both sides | conflict markers |
| A row deleted on one side, edited on the other | conflict markers |
| One ID filed on both sides with different content | conflict markers |
| Rows reordered on both sides | conflict markers |
| A row whose anchor is missing or disagrees with its visible ID | conflict markers |
| A conflict outside the Queue rows (Progress table, Deferred table, prose) | conflict markers |

A conflict marker costs a minute; a wrongly resolved row loses backlog state.
Two consequences worth internalising: the driver **cannot resurrect a row the other side deleted** (a deletion either wins outright or produces markers, never a re-add), and it claims **no** knowledge of the Progress or Deferred tables — those merge as plain text, exactly as before.

**The refusal is per row, but the fallback is per file.** Every case in the table ends by re-running `git merge-file` over the whole file, so the hunk it marks spans the refused row *and* every row added beside it on either side: one refused row produced a five-row hunk in [the measurement below](#a-hand-resolved-conflict-drops-rows-the-markers-never-named).
Picking a side of that hunk is what loses rows neither side disagreed about.

**It does not help GitHub**, which cannot see a clone's config: the server-side squash-merge, the mergeability read behind `mergeStateStatus`, and the merge queue's candidate build all take the plain three-way merge.
That is why a batch's row deletions are [spaced at assignment](parallel-dispatch.md#the-dispatcher-owns-assignment-not-coordination-files) rather than left to the driver.
And a driver-resolved merge is still a merge you own: read the resulting row set, then run the three gates below. `make lint-backlog` remains the independent backstop — rules 8, 9 and 10 all still apply to whatever the driver produced.

### The same treatment for `docs/plan/README.md`

The plan index has `STATUS.md`'s contention and the same cause.
Every plan doc that lands adds one long row, every archival moves one to the top of the Archive table, and the topical sections concentrate both on the same few neighbours.
Over the 22 changes to the file that merged between 2026-08-01 and 2026-08-03, **18 of the 231 pairs touch a row in common; the other 213 disagree only about line position** — and adjacency makes a plain three-way merge conflict on those anyway.

[`scripts/docs/git-merge-plan-index.sh`](../../scripts/docs/git-merge-plan-index.sh) decides them by the **plan path in column 1**, sharing the Queue driver's row rules and its refusal discipline.
That key is not a new convention: [`check-plan-index.sh`](../../scripts/docs/check-plan-index.sh) already reads the same cell, so the driver and the gate cannot disagree about what a row is.

Two things differ from the `STATUS.md` driver:

- **It merges every table in the file, not one named section.** Archiving a plan is a delete in a topical table and an add in the Archive table, and a section-scoped merge would read that as an unexplained deletion.
- **It checks the whole file afterwards for a plan listed twice**, comparing basenames so `archive/x.md` and `x.md` count as one plan.
  Per-table merges cannot see that pair, which one branch archiving a plan while another relocates it produces.

Everything else is the Queue driver's behaviour: a row changed on both sides, a row deleted on one side and edited on the other, one plan filed twice with different text, rows reordered on both sides, a row whose first cell is not a link, and a side that added or removed a whole table all fall back to the plain three-way merge and its conflict markers.

### And for `docs/roadmap.md`

The public roadmap is bound to the backlog one bullet at a time: each forward-looking bullet carries a `<!-- q:QN -->` annotation naming the rows behind it, and shipping the work deletes the bullet.
That makes every gate PR the same edit in the same two sections.
Measured on `docs/roadmap.md` at 61cf54e7b: two branches each deleting their own bullet from the near-term list conflict under a plain three-way merge, while the same two deletions ten bullets apart merge clean.
[Q715](https://github.com/actions-gateway/github-actions-gateway/pull/1392)'s PR met that shape three times in one session, each time as a merge-queue eviction followed by a hand-resolved rebase.

**What the driver buys is the rebase, not the eviction.** A merge driver is per-clone `git config`, and GitHub builds the merge queue's candidate itself, so the server-side conflict recurs exactly as before ([merge-queue.md](../plan/merge-queue.md) measured the same thing for `docs/STATUS.md`, which has had a driver since Q611).
What changes is the heal: the rebase that follows an eviction resolves silently instead of by hand.
Fewer evictions is a different problem, and the lever is spacing before serializing: the same two deletions ten bullets apart merged clean, so a batch whose items sit apart on the page never meets the conflict ([the same rule for Queue rows](parallel-dispatch.md#the-dispatcher-owns-assignment-not-coordination-files)).

[`scripts/docs/git-merge-roadmap.sh`](../../scripts/docs/git-merge-roadmap.sh) decides the bullets by that **annotation**, normalized to a comma-joined ID list.
The key is not a new convention either: `devtools/docs/roadmapcheck` already parses the same comment, so the driver and the gate cannot disagree about what a bullet is.

Two things differ from the other two drivers:

- **A bullet spans several lines**, which the shared record rules do not model, so each one is encoded onto a single line and decoded after the merge.
- **The blank lines between bullets are held beside the records, not inside them.** Fold the trailing blank into a bullet and deleting a list's last bullet reads as an *edit* of its neighbour, which then collides with the other side deleting that neighbour: the exact merge the driver exists to resolve.

A run of bullets is only merged this way when every bullet in it is annotated, so an ordinary bulleted paragraph elsewhere on the page keeps git's own merge.
Everything else is the Queue driver's behaviour, plus a whole-page check that no binding ended up on two bullets.

`make merge-driver` installs all three drivers.
None is required: until you run it the `.gitattributes` lines name undefined drivers and git uses its built-in three-way merge, which is exactly the pre-driver behaviour.

## Resolving a `STATUS.md`-only conflict: verify cheap, push now

Because every session edits `docs/STATUS.md`, rebase and merge conflicts on it are routine — fewer with the merge driver installed, never zero.
When the conflict is **confined to `docs/STATUS.md`**, re-running the full `make check` before pushing is not just unnecessary — it is what causes the *next* conflict.

The full gate takes ~6 minutes.
Every one of those minutes is a window in which a sibling session merges its own `STATUS.md` edit and puts your branch behind again, so you resolve, wait ~6 minutes, and lose the race a second time.
It is a feedback loop, not bad luck: [PR #724](https://github.com/actions-gateway/github-actions-gateway/pull/724) went around it four times.
Shrinking the verify step from ~6 minutes to a few seconds is what breaks the loop.

**The fast path** — only when `git status` shows `docs/STATUS.md` as the sole conflicted path:

1. Resolve the conflict in `docs/STATUS.md`.
2. Check for leftover markers before staging: `git diff --check`.
   An `Edit`-based resolution can silently leave one behind, and `git diff --check` catches it in the working tree — before it becomes a commit the gate has to reject.
3. If you resolved the hunk by hand rather than letting the driver decide it, [reconcile the row set](#a-hand-resolved-conflict-drops-rows-the-markers-never-named).
   A clean marker scan says the resolution is well-formed, not that it kept every row.
4. Run only the gates that can actually observe a `STATUS.md` change:

   ```bash
   make status-gates
   ```

5. Commit and push **immediately**.
   Do not wait on `make check`.

`make check` adds nothing here: no gate it runs beyond these reads `docs/STATUS.md`, and CI runs the full gate on the pushed branch regardless. `status-gates` is the complete set a `STATUS.md`-only diff can fail, and every member is also in `make check`, so this is a strict subset and never a second opinion.

**Run the target, don't transcribe its contents.** This list used to be spelled out here as three `make` targets, and it was wrong: `roadmap-check` and `plan-index-check` both read Queue membership and both can fail on a `STATUS.md`-only diff.
A grooming pass that parked a row followed the three-target list, went green locally, and opened a PR red on `roadmap-check`.
The set now lives in the `STATUS_GATES` variable in the [`Makefile`](../../Makefile), whose comment names each member and what it catches, so there is one copy to keep true.
Transcribing is not the only way it drifts: `em-dash-check` and `page-density-check` both scan the file and were both missing from the variable while its comment called the list complete (Q749), so `make gate-lists-check` now derives membership from the pathspec each gate hands git and fails when a fast gate that scans `docs/STATUS.md` is left out.

**When it does not apply:** if the conflict touches *any* other file — code, a plan doc, another page under `docs/` — this is a normal conflict.
Resolve it and run the full `make check` (plus whatever heavier tier the change warrants) before pushing.
The fast path is licensed by the narrowness of the diff, not by the presence of `STATUS.md` in it.

**It is also not a general shortcut for authoring.** The minutes it saves buy speed in a race you are already losing — a sibling session merging its own edit while you verify.
Authoring a groom, filing a row, or completing an item is not that race, and a Queue edit routinely [cascades](#what-parking-a-row-obliges-elsewhere) into a plan doc, `plan/README.md`, or a roadmap bullet, which puts the diff outside this section's narrowness anyway.
Author the change first: if it really did touch only `docs/STATUS.md`, the gate set above still covers it; otherwise run `make check`.

### A moved row defeats conflict detection

Git raises a delete/modify conflict only when both sides touch the *same* lines.
Reordering a row moves it, so a branch that **relocates** a row while `main` **deletes** it produces no conflict at all: git applies the delete at the old position and the re-add at the new one, and a completed row silently comes back. **A clean rebase is not evidence of a correct one.**

This is the second and more dangerous of the two ways a done row comes back — the squash-merge case at least leaves a conflict to notice.
Both occurred on 2026-07-25: the squash case in [#766](https://github.com/actions-gateway/github-actions-gateway/pull/766)/[#768](https://github.com/actions-gateway/github-actions-gateway/pull/768), and the reorder case while rebasing a release-planning branch across [#805](https://github.com/actions-gateway/github-actions-gateway/pull/805), which had shipped the very row that branch was relabelling.
A third near-miss on 2026-07-26 — a row inserted directly above one `main` had just deleted — is what finally bought the automated check below.

**`make lint-backlog` checks this for you** (rule 10).
An ID present in your `docs/STATUS.md` but absent from the baseline's is *new* when the baseline's history never carried its anchor, and a *resurrection* when it did — the distinction a manual eyeball can't make cheaply.
The rule fires only once your branch already contains the commit that did the deleting, so a branch that is merely behind `main` isn't flagged for a deletion a rebase will apply anyway.

**That baseline is the merge base with `origin/main`, not its tip.** Every git-backed rule here asks what *your branch* changed, which is a question about the branch point.
Against the tip, a row `main` deleted while your branch was behind read as one you had added, and rule 12 then demanded you allocate an ID for a row another session had already finished (Q684).
Under `--staged` the baseline is the pre-commit tree instead, because that mode asks what the *commit* changes.

The [merge driver](#the-merge-driver-resolve-queue-rows-by-id-not-by-line-position) closes the reorder-over-delete path at the source — it decides by ID, so a relocated row cannot outvote a deletion — but only for people who installed it, and only for local merges.
The lint rule stays the load-bearing check.

Deliberately re-opening a closed item? `BACKLOG_ALLOW_RESURRECT="Q1 Q2" make lint-backlog`.

**To inspect by hand**, list the IDs your branch has that the branch point did not.
Read the branch point, not `origin/main` — against the tip this prints every row `main` has deleted since you branched:

```bash
base="$(git merge-base HEAD origin/main)"
comm -23 <(grep -o 'id="Q[0-9]*"' docs/STATUS.md | sort -u) <(git show "${base}:docs/STATUS.md" | grep -o 'id="Q[0-9]*"' | sort -u)
```

Every ID it prints should be one *you* filed.
Anything else is a row `main` deleted and your rebase brought back — check whether its work shipped before you push.

### A hand-resolved conflict drops rows the markers never named

Rule 10 catches a row that comes *back*.
The mirror case is a row that quietly goes *away*, and no rule catches that one, because deleting a row is how a row closes: a lint pass cannot tell a completed item from a casualty.

Measured on [#1471](https://github.com/actions-gateway/github-actions-gateway/pull/1471): the driver refused a `Q738` row both sides had edited, which is correct behaviour for a keyed merge, and the hand resolution that followed **deleted `Q823` and `Q822`**, rows belonging to `main` that the PR never touched, while **dropping `Q836` and `Q837`**, the two rows the PR existed to add. `git rebase` reported success, no marker remained, and `make status-gates` passed on the result. **An absent marker proves a resolution is well-formed, never that it is correct.**

Reproduced in a throwaway repo with the driver installed (git 2.55.0), one refused row sitting between two rows added on each side:

| Resolving the hunk by | What it cost | What reported it |
|---|---|---|
| keeping the branch's side | the two rows `main` had added | rule 8, and only because both carried `flake` |
| keeping `main`'s side | the two rows the branch had added | nothing: the rebase dropped the branch's now-empty commit and printed `Successfully rebased` |

`git diff --check` exits 0 in both.
The second case is the worse one, because the branch ends up changing nothing at all while its commit subject and PR description still claim two rows.
Do not lean on rule 8 for the first: it covers one label out of the whole vocabulary, and #1471 recorded `status-gates` passing on the damaged tree even though both lost rows carried it.
That tree was repaired before it was ever committed, so which of the two accounts of rule 8 applies there is no longer measurable.

**So reconcile the row set, not the markers.** Two comparisons name every casualty, and neither needs a count you memorised before the rebase started:

```bash
# rows your branch had and no longer has
comm -23 <(git show ORIG_HEAD:docs/STATUS.md | grep -o 'id="Q[0-9]*"' | sort -u) <(grep -o 'id="Q[0-9]*"' docs/STATUS.md | sort -u)
# rows main has and your branch does not
comm -13 <(grep -o 'id="Q[0-9]*"' docs/STATUS.md | sort -u) <(git show origin/main:docs/STATUS.md | grep -o 'id="Q[0-9]*"' | sort -u)
```

Every ID the first prints should be one `main` closed; every ID the second prints should be one *you* closed.
Anything else is collateral from the resolution.
Run them before `git rebase --continue`, and promptly either way: the next rebase or merge rewrites `ORIG_HEAD`.

The diffstat answers the same question and is what actually caught #1471 (`1 insertion, 3 deletions` on a branch that adds two rows), but it only fires if you knew the expected counts going in, and it names no IDs.
Either way the principle is [the one bulk mechanical changes already use](testing.md#a-bulk-mechanical-change-proves-itself-by-reconciliation-not-by-an-empty-leftover-query): a scan that finds nothing cannot distinguish "nothing was lost" from "this instrument cannot see the loss".

## When the context doesn't fit, write the doc — whatever the item's size

The trigger for writing a doc is **information loss, not item size**: `Sz` estimates effort, not how much context the work rests on.
If fitting the caps means dropping a decision the work depends on, an investigation finding a future session would re-derive, or a blocker's rationale — write (or extend) the doc and link it.
Compressing prose is fine; dropping a clause because it doesn't fit is the signal.
The content picks the home:

| Kind of context | Home | Why |
|---|---|---|
| **Durable rationale** — decisions, security governance, why a default is what it is | `docs/design/` | Survives plan archival; still there in two years. |
| **In-flight work context** — findings, phases, what's left | `docs/plan/<qNNN>-<slug>.md` | Archived on close (below). |

When a plan closes, **promote its load-bearing conclusions into `docs/design/`** rather than letting them archive out of reach — Queue rows and code cite the durable layer, never a plan path.

## A row's asserted defect is a claim, not a finding

A Queue row is read months later as established fact.
Write it so a future session can tell what was measured from what was inferred: state the measurement that establishes the defect, or say plainly that the mechanism is unverified.

An unmeasured mechanism costs more than the row saves.
Q584 was filed asserting that `check-path-filters.sh`'s awk YAML parsing could "mis-read as full coverage, failing green".
That was wrong — the gate iterates a hardcoded filter registry against `go.work`, so a parse failure *removes* patterns and fails closed — and the row reached `main` before anyone tried to reproduce it, costing a second PR to correct.
The real defect had the opposite sign: a valid reformat made the gate emit twelve errors naming patterns that were already present.

**A count of instances in a row is a claim too, and usually the least-verified part of it.** The mechanism gets checked because it reads like an assertion; "two live differences" reads like an observation and invites fixing exactly two.
Q851 named two label values whose tier the ledger got wrong; deriving the set from source found seven, plus seven stale `Help` strings the row had not mentioned at all.
Where the row names a small N, prefer deriving the population over repairing the named N: the derivation is what catches the rest, and it keeps catching them after the row is closed.

Rows that name an unknown are honest and useful — several in the Queue say "unmeasured live — confirm X before building".
That phrasing is the pattern: it tells the next session where to start, instead of sending it to repair something that already works.
Only a stated *mechanism* needs this; a symptom ("this test flaked on run N, passed on rerun") is already an observation.

**A CI observation cites the run or job, not the PR number.** A PR is not a stable pointer to a measurement: its head moves, and the run that failed can end up on a head nobody can reach while the merged head reads green.
Q803's Notes said "Measured 2026-08-12 on #1403, a docs-only diff: 78.7% against 79.5%".
Reading #1403's coverage job returned `./cmd/proxy 79.3% ok (floor 79.5%)`, so the cited artifact contradicted the row rather than supporting it.
The real evidence was on superseded head `8c67e281`, and recovering it took a scan of every failed `unit-test` run in the window to reach job `93978702603`.
The row was right and still cost that scan, which is the point: cite the job id, or the run id and head SHA, so `gh run view --job <id> --log` re-reads the evidence instead of the next session re-deriving it.

**The title carries a claim too, and it is the half that gets read.** A picker reads the Item cell, names the branch, commit and PR after it, and may never re-read the Notes.
Q656 was filed with a sound measurement in its Notes (two rows took Q644; the loser renumbered across a commit, a PR body and a plan doc) under a title asserting a mechanism nobody had checked: "`make queue-id` reports a free ID but reserves nothing".
The reservation existed and worked, an atomic ref claim with 240 live IDs, so the title named a defect the code did not have while the Notes named one it did.
When the cause is unverified, title the row with the symptom: "two sessions took the same Q-ID" survives being wrong about why, and still sends the next session to the right place.

**The cost a row asserts is a claim too, and it shapes the approach before the work starts.** A row that names what a fix will *require* hands the next session a plan, and a plan is harder to question than a diagnosis, because it arrives sounding like scope rather than analysis.
Q658 said adding a fifth tile to a full dashboard row "shifts every `gridPos` below y=52 and needs a `render.sh` re-shoot".
That is true of the obvious approach, and taking it would have moved four rows and fourteen panels.
The dashboard had already solved the problem one row below: five tiles fit 24 columns as 4×w5 plus one w4, so the shipped change touched only `y=46` and nothing under it moved.
State the constraint that is load-bearing ("the condition row is full at 4×w6") and leave the consequence to the session that can measure it; where a cost estimate is what sets the row's `Sz`, say which half of it was measured.

### A completion note is a claim too, and it is the one nobody re-checks

The rules above guard what a row asserts at filing time, when the next session is still expected to verify it.
A **closure** note is read the other way round: it says the question is settled, so nobody opens the row again to check.
That makes a wrong one strictly worse than an open row, because an open row invites the work and a falsely-closed row forecloses it.

`plan/README.md` recorded Q60 as "verified + folded into [appendix-d](../design/appendix-d-alternatives-considered.md)".
The Q60-closing commit added 34 lines about Kueue and Exostellar and contained **zero per-claim verification of the competitor it named**.
The eleven unverified cells that row was supposed to check shipped into the published comparison table and stayed there, precisely because the index said the checking was done.
Two of them went false at datable upstream releases before anyone re-measured ([competitive-analysis-2026-08](../plan/competitive-analysis-2026-08.md#why-the-marketing-drifted-and-the-fix)).

**Write the completion note from what the change did, not from what the row asked for.** The two diverge silently, because the row's title is sitting right there and it is easier to restate than to summarise a diff.
Where the closing change did part of the work, say which part and leave the rest open: "folded the Kueue and Exostellar comparison into appendix-d; the ARC per-claim verification is still open" is one clause longer and would have kept the row alive.

### A row proposing a gate names what must stay green

A row whose deliverable is a new assertion is a spec, and the hard half of an assertion is its false-positive boundary rather than the defect it catches.
Name both: the shape that must go red, and a shape already in the tree that must stay green.

**A rule phrased in prose can be tightened as it is restated; a named control cannot.** Q659 asked for an assertion against a "mid-pattern `**`" in a filter glob, which is right, because picomatch expands a pattern-initial `**` normally.
By the time it was implemented the rule had been restated as "`**` is only meaningful as a whole path segment", which is the same rule with the exemption dropped.
A gate built to it would have failed `make check` on arrival, since `plan-hygiene.yml`'s `plan` filter is `'**.go'`.
The measured table the row linked did carry the exemption, so nothing was lost permanently, but re-deriving the boundary cost a survey of every filter in the tree.
One control in the Notes, "`'**.go'` must stay green", would have survived the restatement intact.

**A warning's signal needs a firing rate, not just a condition.** An assertion that fires on the wrong shape is a false positive and reads as a bug.
A warning that fires on the right shape every single time reads as correct and is still worthless, because a prompt that always appears is a prompt that is always accepted.
Q665 asked for a warning when `git diff HEAD...origin/main` is non-empty at `git push`, which is exactly right about what it detects: the base really has moved.
Measured 2026-08-05, `main` takes 47 merges a day against a local gate that runs for minutes, so the condition holds at nearly every push.
The shipped check fires instead on the overlap between what the base gained and what the branch changes, the part the row's own Notes called the waste.
State roughly how often the condition holds in the tree as it stands, or say the rate is unmeasured; selectivity is what decides whether a warning is worth building, and it costs one `git log` before filing against a rebuild after.

This is the same reason a gate's own tests assert both directions ([testing.md](testing.md#a-bulk-mechanical-change-proves-itself-by-reconciliation-not-by-an-empty-leftover-query)): a gate that cannot go red is unfalsifiable, and a gate that cannot stay green is unshippable.

### Two rows on one defect: cross-link them, and say which owns the measurement

One defect routinely lands on two Queue rows, and the two ways that happens have opposite causes.
Either a reproduce campaign measures the production defect underneath the flake it was chasing, and both rows get filed minutes apart by someone holding all of it at once, so neither records that the other exists.
Or the second row is filed days later by someone who does not know there is a pair, and nothing connects them because nobody yet knows there is anything to connect.
Only the first is a habit at filing time.
The second is caught by whoever re-reads a sibling row and recognises the pair, so the obligation attaches there rather than to the filer.

**Each row names the other, and one of them owns the measurement.** The owner is the row whose link resolves to the measurement itself: the counts, the date, the correlation.
Where both rows link the same doc, the owner is the one pointing at the anchor that holds the run rather than at the doc's root, and its Notes say so outright, because nobody works it out by comparing two URLs.
The other row cites the owner by ID instead of restating the figure, since a bare number with no provenance reads as a second, independent measurement.

Q685 and Q689 were one window seen from two sides, the same-campaign shape.
Q685's campaign correlated 60 stops taken at maximum pressure: the 4 taken before the delete all replayed, the 56 taken after it did not.
That single correlation established both the test's bad wait and the production window the test was landing in.
Q689 was filed carrying "4/60 graceful stops" with nothing naming where the 60 came from, Q685's flake-watch row named no successor, and the correlation itself sat in a parenthetical inside Q685's [testing.md paragraph](testing.md#synchronize-on-the-signal-you-assert-on).
Q685 gained the pointer only in the commit that closed Q689, so it was missing for the whole window in which Q689 was open work someone might pick up; the Q689 retro records a dispatcher having to ask which row owned the number.

Q549 and Q809 are the other shape, and the reason this cannot live at filing time alone.
Q549 was filed 2026-07-31 carrying an undiagnosed second failure mode, Q809 on 2026-08-11 under a different diagnosis, and neither row named the other, because at filing nobody knew there was a pair to name.
A session re-reading a merged sibling eleven days later is what recognised one, which is both the moment the cross-link is owed and the moment nothing prompts for it.

**Nothing gates this, and the obvious proxy does not transfer.** The linter can see a `(#QN)` link that resolves to no row (rule 5); it cannot see a relationship nobody stated.
The nearest rule that does exist is [`check-plan-index`](../../scripts/docs/check-plan-index.sh)'s linked-iff-live check on `plan/README.md` Status cells (Q800), and extending it to Queue Notes cells would still miss this omission while breaking rows that are correct: seven of them on 2026-08-12, one more than the same scan returned that morning, all incidental mentions of the "distinct from Q695's alert gap" shape, and each would then spend link characters against the 250-character cap.

## Flake fixes go first

When a CI flake is observed (test passes on rerun, no code change in between), file it as a Queue item **and move it to the top of the Queue** before continuing other work.
Then pick it up next.
Flake cost compounds: a 1-hour fix saves cumulative CI wait + diagnosis + context-switch overhead across every future PR that hits it.
This overrides default ordering even over critical security items — those are typically M/L-sized and themselves benefit from flake-free CI.
Annotate the row's Notes with "**Top of queue per flakes-first rule**" linking this section.

Exceptions: a flake rooted in an outside service that hasn't recurred (file, don't bump); a flake whose fix is blocked on infrastructure that doesn't exist yet (file, mark 🚫, don't bump).

**Sweep the idiom, not just the instance.** When the cause is an idiom rather than a one-off, the same idiom is usually elsewhere in the file or package, and nothing else will go looking.
Sweep it in the fixing PR and state what the sweep found, "nothing" included: a stated empty sweep is evidence, while an unstated one is indistinguishable from not having looked.
Q602 taught one scale-set listener test to wait on a listener-produced signal and left a comment explaining why; four days later [Q685](../STATUS.md#Q685) was the same defect in a sibling test in the same file, and the sweep it finally prompted found a third case.
That one never flaked: its positive assertion held whether or not the listener ever observed the completion, so waiting for CI would never have surfaced it.

**A campaign that pins a flake often measures a production defect too, and a flake filed later often turns out to be one already on the Queue.** Either way the pair gets cross-linked the moment it is recognised, with [one row owning the measurement](#two-rows-on-one-defect-cross-link-them-and-say-which-owns-the-measurement).

**Once the mitigation ships, move the row to [Flake watch](../STATUS.md#flake-watch)** — a Deferred subsection whose revive mechanic differs from the rest of the table: the trigger is always `**Event:** recurs on main after the fix`, and on recurrence the row returns to the **top** of the Queue, escalated (the first mitigation didn't hold).
Keeping the row (rather than closing it) preserves the memory that a fix was already attempted, so a second occurrence reads as a recurrence, not a fresh find.
The lifecycle:

- **Observed, unfixed** → Queue top (flakes-first); pick next.
- **Mitigation shipped, not recurred** → Deferred § Flake watch.
- **Recurs** → back to Queue top, escalated.
- **Soaked or obsolete** → retire to the ledger (below).

A sighting on a **PR branch** does not meet the trigger, so the row stays in Flake watch — but it is still evidence the mitigation is incomplete: record it (on the row, or in the doc the row links) and count the soak from that date rather than from the fix.
Record *which mode* failed, too, where the row's fix addressed a specific one: [Q549](../STATUS.md#Q549)'s second sighting was a mode its fix never covered, and a row naming only the fixed mode would have sent the next session to re-diagnose the wrong thing.

This is the one place the general "done rows are deleted" rule does **not** apply, which makes it easy to miss when a flake fix otherwise looks like a routine change. `scripts/docs/lint-backlog.sh` enforces it (rule 8): a `flake`-labelled Queue row that disappears entirely — measured against the [merge base](#a-moved-row-defeats-conflict-detection) with `origin/main`, or the pre-commit state under `--staged` — fails the lint, naming the row and pointing here.
Retiring a row per the ledger rules below is the deliberate exception: `BACKLOG_ALLOW_FLAKE_DELETE="Q123"` lets specific IDs through.

### Retiring a flake-watch row

Flake watch must not grow without bound — a row whose recurrence-memory has decayed to ~zero still costs a live-table scan every grooming pass and a slice of context budget, for no signal.
During a grooming pass (never automatically), retire a row when **either** holds:

- **Soaked** — the covering spec has passed its **blast-radius run threshold** on `main` since the fix merged (table below), with no recurrence (any recurrence bounces the row back to the Queue, so "since the fix" passes are necessarily consecutive); **or**
- **Obsolete** — the flaky test or the mitigated code path no longer exists or was materially rewritten, so the old memory can no longer map to today's code (auto-retire regardless of age).

The two are an **or**, not an **and**: a stable test that simply keeps passing graduates via *soaked*; a deleted/rewritten test graduates immediately via *obsolete*.
Requiring both would mean a row never retires while its test sits quietly passing — exactly the row that has served its purpose.

The soak threshold scales with **blast radius** — how much a *false* retirement costs, keyed on one question: if this spec silently started failing *for real* after we retire it, what do we lose?

| Blast radius of a recurrence | Threshold |
|---|---|
| **Infra / CI flake** — a recurrence just costs a rerun (network / timing / disk / registry; e.g. kindnet, calico) | **≥25** passing `main` runs |
| **Correctness-guarding test** — the spec asserts a product behavior, so a false retirement makes a future *real* red read as "known flake, rerun" | **≥50** passing `main` runs |
| **Could mask a data-loss or security regression** | **Do not soak-retire** — root-cause it, or keep watching indefinitely |

The counts are the ~3/N 95% upper bound on the residual per-run failure rate (25 → ~12%, 50 → ~6%); the higher bar buys a trustworthy regression signal after retirement, not just more confidence the flake is gone.
Tune per flake — these are floors, not ceilings.

Soak is counted in **runs, not calendar days**: what proves a flake dead is the spec being *exercised* green, and calendar time is only a proxy for that — one that breaks whenever merge velocity shifts or the spec is path-gated and runs rarely.
Counting runs measures the thing directly and never needs re-tuning to velocity; the recurrences seen here (Q300, Q291) surfaced within a few dozen runs, inside even the lower threshold.
Count green runs of the covering workflow since the fix's merge date:

```bash
gh run list --workflow <name>.yml --branch main --status success \
  --created '>=YYYY-MM-DD' --json databaseId --jq 'length'
```

A run that failed on an *unrelated* flake is excluded, so this undercounts slightly — conservative, which is what we want.
One thing run-count can't see: a flake suspected to be **time-correlated** (nightly-load or API-rate-limit windows) should also sit through a few day/night cycles before graduating — judgment, not a fixed number.

On retirement, **move the row to [flake-watch-retired.md](flake-watch-retired.md)** (a cold, greppable ledger) rather than deleting it.
That preserves the "a fix was already attempted here" memory at zero live-table cost: if the flake ever returns post-retirement it re-enters as a fresh find, and the ledger is one `grep` away to reconnect the history.
Deleting outright throws that memory away and makes the next occurrence look novel.

## The Progress table

`docs/STATUS.md` keeps a plan-level **Progress** table above the Queue — one row per plan doc.
Update it only when a plan's overall status changes (⚠️ → ✅, a new plan lands, a plan retires); most STATUS.md commits touch only the Queue.
If completing a Queue row closes the last open item under a Progress row, update both in the same commit.

When you remove a Queue row for a **shipped user-facing capability**, also check whether it graduates a bullet on the website [roadmap.md](../roadmap.md) — an "In progress / near-term" item moving to "Available now (1.0)" — and state its true maturity (GA vs. alpha) so the roadmap doesn't overclaim. `make roadmap-check` (in `make check`) catches the drift for you: each forward-looking roadmap bullet carries an invisible `<!-- q:QN -->` annotation, so deleting the row here fails the gate until the bullet moves.
Deleting the row and the annotation without moving the bullet defeats it, which is the one case still on you.

### `⚠️` means an open *Queue* row remains — deferred residuals don't count

A plan is `⚠️` only while it has at least one open row **in the Queue**.
Intentionally-deferred residuals live in Deferred (or, for non-commitments, in [appendix-g](../design/appendix-g-future-enhancements.md)) and do **not** keep a plan `⚠️`: a plan whose only remainders are Deferred rows is `✅`.
This keeps the table honest — `⚠️` reads as "active work remains," not "a box was once left unchecked."

`scripts/docs/lint-backlog.sh` enforces the transition at the moment it becomes owed: deleting the **last** Queue row that links `plan/NAME.md` fails the gate while that plan's Progress row is still `⚠️`, naming the plan and the flip.
It fires only on that deletion, never on a steady-state scan — plenty of open rows merely *cite* a completed plan as evidence, and treating those as active work would make the rule cry wolf.
For the rare case where the vanished row was such a citation and real work genuinely remains elsewhere, `BACKLOG_ALLOW_PROGRESS_STALE="plan/NAME.md"` admits it.

When you flip a plan to `✅`, add (or update) a **Status** banner at the top of its plan doc naming the Deferred IDs carrying its residuals (e.g.
"Status: Complete — residuals deferred as [Q11](../STATUS.md#Q11)").
The plan doc is **not** archived in this case — its `✅` Progress row still references it.

### What parking a row obliges elsewhere

Deleting a row has a documented cascade — the Progress flip, plan archival, the roadmap graduation. **Moving a row to Deferred has the same one**, because parking changes Queue membership exactly as deleting does, and everything downstream reads Queue membership rather than row existence:

| What to check | Why parking triggers it |
|---|---|
| The plan's **Progress** row | A plan whose only remainders are now Deferred is `✅`, not `⚠️` — deferred residuals don't count (above). |
| The plan doc's **Status** banner | That `✅` flip owes one, naming the Deferred IDs and their triggers. |
| The plan's row in [`plan/README.md`](../plan/README.md) | Its status text usually describes the residual as open work. |
| The [roadmap](../roadmap.md) bullet annotated `<!-- q:QN -->` | An "In progress / near-term" bullet must name at least one row still **in the Queue**; an all-Deferred bullet was parked and belongs under "Exploring / longer-term". `make roadmap-check` fails until it moves. |
| Prose cross-references on the same page | Nothing checks these. Moving a roadmap bullet between sections leaves any "the near-term work below" phrasing pointing at the wrong place, and `make doc-links` reads links, not sentences. |

Only the first two were written down before.
A 2026-07-30 groom that deferred [Q273](../STATUS.md#Q273) found the other three the expensive way — two by opening a red PR, the third by re-reading the page.

## A label earns its place by discriminating

A label that lands on most rows costs a column and answers nothing. `infra` reached 69 of 160 table rows before it was retired — it had become "engineering work that isn't docs, tests, or security", covering controller bugs, API graduation, GPU support, and CI gates alike.
The rows it marked already carried `bug`/`feature`/`security`; the label added no cut.

Three narrower labels replaced it, each answering a question someone actually asks:

| Label | Scope | The question it answers |
|---|---|---|
| `ci` | The build/test gates themselves — `.github/workflows/**`, `make check` and its scripts, lint and coverage plumbing | *What's wrong with the gates?* |
| `dogfood` | The GKE dogfood cluster and its bootstrap/teardown scripts under `scripts/dogfood/` | *What bites me on the next cluster recreate?* |
| `debt` | Refactors, dedup, and dead-surface removal with no behavior change | *What can I clean up without a design decision?* |

Deliberately **not** added: an `e2e` label. `tests` already covers those rows and the item title names the suite, so `e2e` would double-label rather than split.
A product change to the AGC or GMC takes no area label at all — `bug`/`feature`/`security` carries it, and the linked path says where it lives.

Apply the same bar to any new label: if you can't name the question it answers, or it would land on more than a third of rows, it belongs in the item title instead.

`lint-backlog.sh` rule 11 holds the vocabulary closed: every label on a Progress, Queue, or Deferred row must appear on the `**Labels:**` line, so adding one is a deliberate edit to that line rather than a typo that sticks.
This is what a retirement needs — Q592 was filed wearing `infra` from a branch cut before the split and merged without a conflict, because the two edits touched different rows.

## Don't pre-assign release versions to backlog items

Do **not** tag Queue rows with speculative future release versions (`1.1`, `2.0`).
Introduce a release label only once that release is *concretely scoped* — a plan doc defining its Definition of Done exists — at which point the label answers a real yes/no question ("does this block that tag?").
Post-release estimates are guesses that move (churn without signal), position already encodes priority, and an undefined version anchors nothing.
The right pattern is the one `1.0-gate` followed: scope the release in a plan doc first, then add the label.

**The corollary: once a release *is* scoped, only what blocks the tag gets a label.** An item that is planned for the release but does not gate it belongs in that release's scope ledger (below), never in a second, softer label class.
A soft `1.4-plan` label would be a guess nothing enforces — it goes stale silently, needs relabeling on every slip, and blurs the `-gate` label's one crisp meaning, *the tag waits for this*.
The ledger can carry "planned" honestly because its lifecycle matches the release's: written when the release is scoped, archived when the tag is cut.

## Cutting a release: the scope ledger

Deciding a release is worth cutting is a [release.md](../operations/release.md#when-to-cut) question, answered from `scripts/release/release-delta.sh`.
Everything from that decision to the tag is a backlog question, and this is its shape.

**A release plan doc opens with a scope ledger** — one row per planned item, so "planned vs delivered" is readable at a glance instead of reconstructed from a prose status banner:

| Q-ID | Item | Gates? | Status |
|---|---|---|---|
| Q550 | Scale-set runner registration leak | `1.3-gate` | ✅ shipped |
| Q551 | Job skipped permanently after 4 attempts | `1.3-gate` | 🔲 open |
| Q406 | Capacity gate `AutoscalerVerdict` mode | rides | ⤴ punted (see *Explicitly out of scope*) |

Link each ID to its `STATUS.md` anchor while the row is open; a shipped or punted row's link goes stale when the row is deleted, so drop it as the row's status flips — the Q-ID itself stays, and it is what git history is searchable by.

**Delivered is a tick, not a narrative.** A row flips to ✅ in the same change that deletes its Queue row — the plan-docs-stay-current discipline already owns that edit, so the ledger costs nothing extra to keep true.

**The cut condition is one grep:** no `-gate` row for this release remains in the Queue (`grep '1.3-gate' docs/STATUS.md`), plus the release-candidate dogfood validation, which is deliberately not a Queue row because it can only run against a published RC.

**That validation still gets a ledger row** — Q-ID `—`, since it has none:

| — | RC validated on dogfood | gates | 🔲 rc.3: gate aborted at leg 1, no verdict |

Not a Queue row, for the reason above; but left in prose it becomes the one cut condition with no state anywhere.
1.3 published three RCs and none produced a verdict — rc.1 aborted on routing, rc.2 returned two defects instead of a result, rc.3 aborted on a broken wait — and each was diagnosed alone, because nothing displayed the run of misses.
A ledger row makes "no RC has ever passed this" answerable at a glance.

It is the one row that does not flip to ✅ and vanish: rewrite it as each RC reports, and it is the last thing to go green before the tag.

**Punt vs delay — the two ways scope changes:**

- **Punt** (the item leaves the release): remove its `-gate` label and move it to the plan doc's *Explicitly out of scope* table with the reason.
  It **keeps its Queue position** — punting from a release is a statement about the tag, not a demotion; an item can be too important to rush and still be the next thing worked on.
- **Delay** (the release waits): change nothing.
  The label stays on, the ledger row stays open, and the tag waits.
  Leaving the label on *is* the decision to delay — there is no separate marker for it.

Both are reversible and both are cheap, which is the point: the expensive failure is a release whose scope quietly drifts because nothing recorded what it was.

**A `-gate` label is also a public promise.** [`docs/roadmap.md`](../roadmap.md) is where an adopter reads it, so adding or removing one is an edit to both files and a punt that skips the second leaves the promise standing. `make roadmap-check` reconciles them in both directions: every `X.Y-gate` row must be named by a roadmap bullet's `<!-- q:QN -->` annotation, and a bullet that writes a version into its prose must name a row carrying that gate.
Only the second reads the prose, and only it goes quiet once the version is a derived chip rather than a sentence; the coverage half reads the annotation and the label alone, so it holds whatever the bullet looks like.
Naming a version without claiming to gate it stays free, which is how Q273's bullet names `v2.0.0` while carrying no label.

**A near-term roadmap bullet means "committed to a named release".** An item with no `X.Y-gate` label behind it belongs in Deferred with a revive trigger, which puts its bullet under *Exploring / longer-term*.
The [release ladder](../plan/release-ladder.md) is where that rule and the current 1.5 → 1.6 → 2.0 shape are argued; what enforces it is rule 3, since a bullet naming only Deferred rows fails the gate.
The failure mode it exists to catch is the comfortable one: leaving an item in the Queue because parking it feels like giving up.
That is how the section came to advertise nine things as *actively being built* while they waited on demand, a prerequisite, or hardware nobody has.

## Archiving completed plan docs

When a plan's work fully lands and `docs/STATUS.md` no longer references it (no Progress row, no Queue/Deferred row), move the doc under `docs/plan/archive/` rather than deleting it.
The rationale is usually more valuable than the diff, but a fully-closed plan in the top level of `docs/plan/` is noise for the next session scanning for active work.

**Archive on close, not on audit.** Do this in the same body of work that removes the plan's last `STATUS.md` reference — the moment you delete its final Queue row, or flip its Progress row to `✅` with nothing left open.
Two gates (both in `make check`) enforce it so the omission can't ship silently:

- **`make plan-index-check`** fails when an active, non-ⓘ plan listed in `docs/plan/README.md` is no longer referenced by `STATUS.md` — i.e. a plan that should have been archived.
  To clear it: archive the plan (below), or, if it's ongoing spec/strategy/research, mark its README row `ⓘ`.
- **`make doc-links`** fails on any broken link the move introduces.

The same change should also keep the plan's `docs/plan/README.md` **status text** current: when you delete a Queue row that completes a plan, update that plan's README row in the same edit.
For a `release-X.Y.md` row the text is gated rather than remembered: once that release is published, `make plan-index-check` rejects an open marker (❌, 🔲, 🚧) on it, because the tag settles the question the cell was still arguing (Q812).

**Keep archival a docs-only operation.** Archival must never touch code — a code edit re-triggers the heavy path-gated CI (e2e / integration / trivy) on what should be a `docs/**`-only move.
The way to guarantee that: **code never references a plan by path.** A Go comment must not contain `docs/plan/<file>.md`; cite the durable layer instead — a `docs/design/` or `docs/operations/` doc, or a stable `Q-ID` / appendix `§`-ref (those survive archival untouched).
If a plan's conclusion is load-bearing enough that code wants to cite it, promote that conclusion to a durable doc when the plan closes.
Prose mentions of a plan's *content* ("Milestone 1 §8") are fine — only file *paths* rot.

`make no-plan-refs-check` (in `make check`) enforces this, with two rules because the languages differ in what they legitimately do with a plan file:

- **Go** has no legitimate use for one, so any `docs/plan/` or `../plan/` path is rejected anywhere in a `.go` file — comment or string literal alike.
- **Shell scripts and `.github/workflows/`** read plan files as data: a workflow `paths:` filter names one, and a script may rewrite one.
  Those are values, not citations, and a value whose target moves breaks loudly instead of rotting into stale prose.
  So only **comment** text is scanned, and only a plan **file** path — the thing archival actually moves.
  A bare `docs/plan/` directory reference and the index `docs/plan/README.md` survive archival untouched and are never flagged.

A comment that must name a plan file — because that file is what the script operates on — opts out inline with a `no-plan-refs: <reason>` marker on the same line.
It silences exactly that line and shows up in the diff, so the exception stays reviewable; a whole-file allowlist would silence the next rot too.

**Protocol:**

1. **Confirm STATUS.md doesn't reference the doc.** `grep -n "<docname>" docs/STATUS.md` should be empty.
2. **Confirm the work actually landed.** Read the plan's Status banner if it has one; otherwise grep the codebase for the named tests, types, or behaviors the plan promised.
   A plan with open work is **not** archive-ready — leave it in place and make sure the open work has a Queue row.
3. `git mv docs/plan/<docname>.md docs/plan/archive/<docname>.md` — preserves history.
4. **Update any in-repo links** to the new path: `docs/plan/README.md` (move the row to the **Archive** section), other plan docs (`grep -rn "<docname>.md" docs/plan/`), the `docs/development|design|operations` trees, and **the moved doc's own outbound links** — dropping a level into `archive/` breaks every relative link in the doc itself (`make doc-links` catches all of these).
5. **Bundle archival in one commit** when several plans close in the same session — easier to review and revert as a unit.
6. **Do not edit STATUS.md in the same commit** as the archive move; STATUS.md edits are always isolated.

A plan that is partially complete stays in `docs/plan/`.
Archive is for "everything in this doc has shipped," not "most of it has."
