# Agent reference: Maintaining the backlog

`docs/STATUS.md` is the single source of truth for project progress and priorities. It is high-contention — almost every session edits it — so keeping churn low matters as much as keeping it accurate.

The format and process come from the globally-installed **backlog skill** (agents: invoke the `backlog` skill for the full playbook — grooming checklist, staleness signals, parallel dispatch, migration). The repo vendors the skill's tooling so the rules hold for every contributor, with or without the skill:

- [`scripts/docs/lint-backlog.sh`](../../scripts/docs/lint-backlog.sh) — enforces every format rule below. It selects the file and maps the environment interface onto flags; the rules themselves are [`devtools/docs/backloglint`](../../devtools/docs/backloglint/), whose package comment is the canonical rule list. Rows are read from the GFM table AST rather than split on a literal `|`, and cell lengths count characters rather than bytes (Q613). Runs in `make check` (`make lint-backlog`), CI ([`status-lint.yml`](../../.github/workflows/status-lint.yml) and `unit-test.yml`), and the pre-commit hook. The hook's `--staged` mode also rejects a staged set that carries `docs/STATUS.md` alongside other files — the index half of [the isolation rule](#isolated-commits-and-what-actually-enforces-them).
- [`scripts/docs/check-status-isolation.sh`](../../scripts/docs/check-status-isolation.sh) — fails a branch whose commits mix the backlog with anything else. Backs `make status-isolation-check`; runs in `make check`, `make status-gates`, and [`status-lint.yml`](../../.github/workflows/status-lint.yml). [Why it exists next to the hook](#isolated-commits-and-what-actually-enforces-them).
- [`scripts/docs/alloc-queue-id.sh`](../../scripts/docs/alloc-queue-id.sh) — allocates a new Q-ID (`make queue-id TITLE="…"`) by claiming a ref on the remote, so concurrent sessions never take the same one. Rationale, the alternatives weighed, and what it does *not* fix: [queue-id-allocation.md](queue-id-allocation.md).
- [`scripts/docs/find-duplicate-rows.sh`](../../scripts/docs/find-duplicate-rows.sh) — the near-duplicate search that allocation runs before it claims an ID. Advisory: it never blocks a filing. [How it is calibrated](#search-before-you-file).
- [`scripts/docs/git-merge-status.sh`](../../scripts/docs/git-merge-status.sh) — a git merge driver that resolves Queue-table conflicts by row ID rather than by line position, and falls back to ordinary conflict markers for anything ambiguous. One-time `make merge-driver` per clone; a no-op until then. [Details below](#the-merge-driver-resolve-queue-rows-by-id-not-by-line-position).
- [`scripts/docs/next-task.sh`](../../scripts/docs/next-task.sh) — prints a kickoff prompt (or `--title`) for the top ready 🔲 Queue row, for starting a fresh session on the next task.
- [`scripts/docs/backlog-metrics.sh`](../../scripts/docs/backlog-metrics.sh) — replays the file's git history into flow metrics (throughput, cycle time, prune ratio, aging WIP). Read-only. The replay reads each diff line's cells through the shared Markdown parse layer, so an escaped pipe in a cell cannot shift a row's fields (Q614).

## The shared process, in brief

- **Position is priority.** The Queue is read top-to-bottom; pick from the top. Decide a new item's priority *before* inserting it and place the row where it belongs — never append by default. Rank by severity/blast radius, then leverage (what it unblocks), ready over blocked; size only as a tiebreaker.
- **Two Queue states only: 🔲 ready and 🚫 blocked.** Done rows are **deleted** (git is the archive), "started" is signaled by the open PR (run `gh pr list` before picking; skip rows an open PR covers), and parked rows live in the Deferred table.
- **Verify 🚫 blockers before treating a row as blocked** — a prior session may have shipped the dependency without flipping the row; grep for its deliverables. Cross-item blockers are machine-readable: a 🚫 row's Notes start with `Blocked by [QN](#QN)`, and `make queue-unblock ID=QN` lists every dependent when the blocker lands.
- **Verify the defect a row asserts before implementing it.** A row's Notes are written at filing time from an observation that was never re-checked, so grep for the claimed defect the same way you grep for a blocker's deliverables — a prior session may have closed it without flipping the row, and Notes carry no observed-vs-suspected marker. Q506's row named a `noproxy` GHES gap that Q322 had already fixed; taking the row at its word would have "fixed" a non-bug and missed the real one next to it. An audit row inheriting an unverified premise is the sharpest case, but the rule is general.
- **Search before you file** — `make queue-id` does it for you, and reading its candidates is the part that is on you. Nothing else catches a semantic duplicate: two rows describing one problem in different words pass every lint, and both get worked. [Details and calibration below](#search-before-you-file).
- **Allocate IDs with `make queue-id TITLE="…"`**, which claims a `refs/queue-ids/QN` ref on the remote. There is no counter line in the file: a shared mutable counter handed concurrent sessions the same ID and conflicted on the same line by construction, forcing a renumber ([Q382](queue-id-allocation.md#why-the-counter-had-to-go)). IDs are sparse, stable, never reused or renumbered, and never get sub-IDs (`5a`) — a trackable child gets its own top-level ID. The `Q` prefix keeps `Q44` from auto-linking to PR/issue #44; use the bare ID in commits and PRs.
- **Notes are present tense, ≤ 250 characters (hard cap, counted as the cell is written — an em dash costs one, an escaped `\|` costs two); past 200 characters the row must link a doc** from its Item or Notes cell — a `#QN` sibling anchor doesn't count, since sibling rows are capped too. No merged-PR lists or "SHIPPED" narration — history lives in `git log` and the plan doc. The same caps apply to Deferred trigger cells. Write for a skimmer: cut detail and link a doc rather than compressing into fragments.
- **A row never cites a count of the backlog.** "42 Queue rows", "60 parked" and friends go stale on the next filing — the file they measure is the one thing guaranteed to change under them, often the same day. State the *shape* instead ("the Queue is read top-down; the parked rows are only grepped on a trigger"), and put any dated figure in the linked plan doc, where a point-in-time measurement belongs. Q569's row was corrected twice in one session — 36 → 42 → 44 — before the count came out entirely.
- **Deferred rows carry a concrete revive trigger**, tagged by source: `**Demand:**` (an outside ask) · `**Event:**` (an observable outside-our-control condition) · `**Decision:**` (our own call — grep `**Decision:**` for what we could move on unilaterally). When the trigger fires, move the row back into the Queue at the position it then deserves. A non-commitment belongs in [appendix-g](../design/appendix-g-future-enhancements.md), not Deferred.
- **`docs/STATUS.md` edits are isolated commits** — never mixed with code or plan-doc changes, even when completing an item mid-feature ([enforced](#isolated-commits-and-what-actually-enforces-them)). Use `docs(status):` subjects, and name the removal reason with a fixed verb — `complete QN`, `prune QN`, `merge QN into QM`, `defer QN` — so metrics can tell throughput from garbage collection. Batch bulk additions (one audit's discoveries) into one commit; keep reshuffles separate from additions. When a rebase or merge conflicts on this file, resolve it via the [fast path](#resolving-a-statusmd-only-conflict-verify-cheap-push-now) below.
- **M/L items get a plan doc** under `docs/plan/`, linked from the Item cell.

## Isolated commits, and what actually enforces them

**The rule: a commit that touches `docs/STATUS.md` touches nothing else.** Not "no code" — nothing. A plan doc, a roadmap bullet and the row deletion they belong to are three commits, in whatever order. That literal shape is also the shape practice already has: across the last 80 merged PRs, all 74 commits touching the file were backlog-only, so encoding the sentence as written breaks no workflow anyone is using. Several isolated backlog commits in one PR is ordinary and stays green — [#1239](https://github.com/actions-gateway/github-actions-gateway/pull/1239) landed three.

Two mechanisms enforce it, and neither subsumes the other:

| | pre-commit hook (`lint-backlog.sh --staged`) | `make status-isolation-check`, and status-lint.yml |
|---|---|---|
| Fires | at `git commit`, before the mistake exists | on `make check` and on the pushed PR |
| Reads | the **index** | the **commits** |
| Bypass | `--no-verify`; absent entirely until `make hooks` | none |

The hook is the better feedback loop and the weaker guarantee, and reading the index rather than the commit it produces is what makes it structurally blind to the case that motivated the second half: stage only `docs/STATUS.md`, `git commit --amend` onto a code commit, and the hook sees a clean index while git writes a commit carrying both. That is measured, and pinned as a test case in [`check-status-isolation-test.sh`](../../scripts/docs/check-status-isolation-test.sh).

**The commit half scans a PR, never `main`.** Merges here are squash-merges, so a PR that kept its backlog edit in its own commit still lands on `main` as one commit touching `docs/STATUS.md` and everything else — mixed by construction. The individual commits exist only while the PR is open, which makes that window the one place the property is both true and checkable; a scan of `main`'s history would fail on every merge and mean nothing.

**Scope, and a commit that predates the gate.** The range is `merge-base(base, HEAD)..HEAD`: the commits the branch adds, never one already on the base. So every commit the gate can fail belongs to the branch being failed, and `git rebase -i` can always split it — there is no history it is asked to judge and cannot fix, which is why an older mixed commit gets no exemption. When rewriting genuinely costs more than it buys, `BACKLOG_ALLOW_MIXED_COMMITS="<sha> ..."` admits named commits, the same deliberate-and-reviewable shape as `BACKLOG_ALLOW_RESURRECT` and friends. Merge commits are skipped: their file list depends on which parent you diff against, so "what this commit touched" has no single answer for them.

## Search before you file

The rule used to be "grep the Queue and Deferred tables first", and it failed three times: [Q442](https://github.com/actions-gateway/github-actions-gateway/pull/847) and [Q456](https://github.com/actions-gateway/github-actions-gateway/pull/893) both duplicated Q440, and [Q635](https://github.com/actions-gateway/github-actions-gateway/pull/1186) duplicated Q619. Every one satisfied the lint — a semantic duplicate is a well-formed row — and every one was filed mid-task, as a side effect of other work, exactly when the doc carrying the rule was not in context. A rule that fails at the same seam three times wants a mechanism.

`make queue-id` is the mechanism, because it is the one chokepoint every filed row passes through, and it takes the title:

```bash
make queue-id TITLE='`doc-links` never reads a new doc until it is staged'
```

Single-quote it: these titles are full of backticks, which double quotes would hand to the shell as command substitution. A title carrying an apostrophe as well is easier to pass straight to `scripts/docs/alloc-queue-id.sh`, which takes it as a plain argument.

It searches first and claims second, so recognising a duplicate costs no ID. Candidates print to stderr, so `ID=$(make queue-id TITLE="…")` still works, and **nothing is blocked**: the filer routinely knows something the matcher cannot, such as that two rows sharing a file are genuinely separate defects. Say which, in the new row's Notes.

**The title is mandatory, and there is no untitled batch form.** An optional argument is a gate nobody passes through, and `-n 3` was one: it claimed IDs without naming a single row. Several rows at once means several titles: `scripts/docs/alloc-queue-id.sh` takes one argument per ID and searches each on its own, which is what a retro filing four rows actually wants. Nothing automated calls the target, so making the title mandatory changed only the lines that document it.

**Every path through it claims, and a row whose ID holds no claim fails the lint.** `PEEK=1` used to report the next free ID without taking it, which two concurrent sessions read identically — the counter this mechanism replaced, behind a flag. It is gone, and rule 12 catches the other way to obtain an unreserved ID (reading the file's highest and adding one) at the commit that files the row rather than at the rebase that collides. What that cost in practice, and the one case rule 12 still cannot see: [queue-id-allocation.md § Reserving, not reporting](queue-id-allocation.md#reserving-not-reporting).

`TARGET=<link>` is optional and worth passing when the Item cell's link is already decided.

### What it keys on, and why

From the three pairs, not from a guess about what similarity means here:

| Pair | Shared content words | Item link |
|---|---|---|
| Q456 *"The GMC CRD manifests are stale and no gate notices"* / Q440 *"GMC CRD manifest drifts from the AGC types it embeds"* | 3 | same |
| Q635 *"`doc-links` never reads a new doc's own links until it is staged…"* / Q619 *"Three gates scan tracked files only, so a new file misses its own `make check`"* | 4 | same |
| Q511 *"Two live-GitHub runs collide invisibly…"* / Q500 *"Two concurrent live-GitHub runs collide on the fixture repo"* | 5 | different |

Neither signal alone covers that: two pairs agreed on the link and barely on the words, one agreed on the words and not the link. So a row is a candidate on **either** route — ≥3 shared content words at ≥0.40 containment, or an exact link match at the lower bar of ≥2 words and ≥0.25. The shared-word floor is what a ratio alone cannot supply, because containment divides by the *shorter* title: a five-word row scores 0.40 on two incidental words, which is exactly how the novel-row control gets rejected.

Deferred and Flake watch are searched too, because a row duplicating a parked item is the same mistake and those are the tables nobody greps. Notes cells are deliberately not matched: folding a 250-character Notes cell into a row's token set can only raise every score, inflating the ranking without adding a cut.

### Whether it is noisy enough to ignore

An advisory that fires constantly is worse than none, so the thresholds are measured rather than asserted. `scripts/docs/find-duplicate-rows.sh --audit` runs every shipped row back through the same scoring path the search uses, and prints what flags:

```bash
scripts/docs/find-duplicate-rows.sh --audit
```

**Roughly one row in five surfaces a candidate when filed, and every pair it flags is topically adjacent rather than a nonsense match.** The snapshot behind that, on 2026-08-04 (72 Queue + 31 Deferred + 15 Flake-watch rows): 11 flagged pairs out of 6,903. The rate held across every backlog state this was measured in — the Queue turns over faster than any fixed count survives, which is why the figure is a dated instance and `--audit` is the live answer. Two of the eleven look like real duplicates nobody caught: Q663 and Q612 are both `check-doc-links` defects, and Q660 and Q588 are both the doc-update-matrix sending a row into a `scripts/README.md` table that does not exist.

Loosening either ratio by 0.05 roughly doubles the count. Re-run the audit before changing a threshold.

## The merge driver: resolve Queue rows by ID, not by line position

Most `docs/STATUS.md` conflicts are an artifact of the file's shape rather than a real disagreement. A plain three-way merge decides by line position, and the process puts every edit in the same place: pick from the top, insert at the priority the item deserves, flakes first. One untouched row of separation merges cleanly; **adjacent** rows do not, so a four-worker dispatch batch that takes rows 1–4 conflicts by construction ([the measurements](queue-id-allocation.md#what-this-fixes-and-what-it-does-not)).

[`scripts/docs/git-merge-status.sh`](../../scripts/docs/git-merge-status.sh) is a git merge driver that decides the Queue table by **row ID** instead: a row deleted on either side is deleted, a row added on either side is present, a row changed on one side takes that change, and row order is rebuilt from whichever side reordered. It runs on local merges, rebases, cherry-picks and stash applications — everywhere the pain is.

**One-time setup, per clone:**

```bash
make merge-driver
```

`.gitattributes` already routes the file to `merge=backlog`, but git deliberately refuses to let a tracked file define a driver's *command* — that would be remote code execution on clone — so the config half is per-clone and opt-in. **Nothing requires you to install it:** until you do, the attribute names an undefined driver and git silently uses its built-in three-way merge, which is exactly the pre-driver behaviour. [`scripts/dev/setup.sh`](../../scripts/dev/setup.sh) installs it for you, as it does the git hooks.

**What it refuses to resolve.** Every uncertainty ends the same way — the plain three-way merge re-runs and its conflict markers stand, with a one-line reason on stderr:

| Situation | Outcome |
|---|---|
| A row changed on both sides | conflict markers |
| A row deleted on one side, edited on the other | conflict markers |
| One ID filed on both sides with different content | conflict markers |
| Rows reordered on both sides | conflict markers |
| A row whose anchor is missing or disagrees with its visible ID | conflict markers |
| A conflict outside the Queue rows (Progress table, Deferred table, prose) | conflict markers |

A conflict marker costs a minute; a wrongly resolved row loses backlog state. Two consequences worth internalising: the driver **cannot resurrect a row the other side deleted** (a deletion either wins outright or produces markers, never a re-add), and it claims **no** knowledge of the Progress or Deferred tables — those merge as plain text, exactly as before.

**It does not help GitHub's server-side squash-merge**, which cannot see a clone's config. And a driver-resolved merge is still a merge you own: read the resulting row set, then run the three gates below. `make lint-backlog` remains the independent backstop — rules 8, 9 and 10 all still apply to whatever the driver produced.

## Resolving a `STATUS.md`-only conflict: verify cheap, push now

Because every session edits `docs/STATUS.md`, rebase and merge conflicts on it are routine — fewer with the merge driver installed, never zero. When the conflict is **confined to `docs/STATUS.md`**, re-running the full `make check` before pushing is not just unnecessary — it is what causes the *next* conflict.

The full gate takes ~6 minutes. Every one of those minutes is a window in which a sibling session merges its own `STATUS.md` edit and puts your branch behind again, so you resolve, wait ~6 minutes, and lose the race a second time. It is a feedback loop, not bad luck: [PR #724](https://github.com/actions-gateway/github-actions-gateway/pull/724) went around it four times. Shrinking the verify step from ~6 minutes to a few seconds is what breaks the loop.

**The fast path** — only when `git status` shows `docs/STATUS.md` as the sole conflicted path:

1. Resolve the conflict in `docs/STATUS.md`.
2. Check for leftover markers before staging: `git diff --check`. An `Edit`-based resolution can silently leave one behind, and `git diff --check` catches it in the working tree — before it becomes a commit the gate has to reject.
3. Run only the gates that can actually observe a `STATUS.md` change:

   ```bash
   make status-gates
   ```

4. Commit and push **immediately**. Do not wait on `make check`.

`make check` adds nothing here: its remaining targets (unit tests, coverage, `golangci-lint`, `shellcheck`, chart drift, Go version) read no Markdown, and CI runs the full gate on the pushed branch regardless. `status-gates` is the complete set a `STATUS.md`-only diff can fail — `lint-backlog` for the format rules, `roadmap-check` for a row that changed table or vanished while a [roadmap](../roadmap.md) bullet still names it, `plan-index-check` for a plan whose last citing row went away, `conflict-markers-check` for a marker that survived the resolution, and `doc-links` for a link or anchor broken while rows moved. Every one is also in `make check`, so this is a strict subset and never a second opinion.

**Run the target, don't transcribe its contents.** This list used to be spelled out here as three `make` targets, and it was wrong: `roadmap-check` and `plan-index-check` both read Queue membership and both can fail on a `STATUS.md`-only diff. A grooming pass that parked a row followed the three-target list, went green locally, and opened a PR red on `roadmap-check`. The set now lives in the `STATUS_GATES` variable in the [`Makefile`](../../Makefile), so there is one copy to keep true.

**When it does not apply:** if the conflict touches *any* other file — code, a plan doc, another page under `docs/` — this is a normal conflict. Resolve it and run the full `make check` (plus whatever heavier tier the change warrants) before pushing. The fast path is licensed by the narrowness of the diff, not by the presence of `STATUS.md` in it.

**It is also not a general shortcut for authoring.** The minutes it saves buy speed in a race you are already losing — a sibling session merging its own edit while you verify. Authoring a groom, filing a row, or completing an item is not that race, and a Queue edit routinely [cascades](#what-parking-a-row-obliges-elsewhere) into a plan doc, `plan/README.md`, or a roadmap bullet, which puts the diff outside this section's narrowness anyway. Author the change first: if it really did touch only `docs/STATUS.md`, the gate set above still covers it; otherwise run `make check`.

### A moved row defeats conflict detection

Git raises a delete/modify conflict only when both sides touch the *same* lines. Reordering a row moves it, so a branch that **relocates** a row while `main` **deletes** it produces no conflict at all: git applies the delete at the old position and the re-add at the new one, and a completed row silently comes back. **A clean rebase is not evidence of a correct one.**

This is the second and more dangerous of the two ways a done row comes back — the squash-merge case at least leaves a conflict to notice. Both occurred on 2026-07-25: the squash case in [#766](https://github.com/actions-gateway/github-actions-gateway/pull/766)/[#768](https://github.com/actions-gateway/github-actions-gateway/pull/768), and the reorder case while rebasing a release-planning branch across [#805](https://github.com/actions-gateway/github-actions-gateway/pull/805), which had shipped the very row that branch was relabelling. A third near-miss on 2026-07-26 — a row inserted directly above one `main` had just deleted — is what finally bought the automated check below.

**`make lint-backlog` checks this for you** (rule 10). An ID present in your `docs/STATUS.md` but absent from `origin/main`'s is *new* when the baseline's history never carried its anchor, and a *resurrection* when it did — the distinction a manual eyeball can't make cheaply. The rule fires only once your branch already contains the commit that did the deleting, so a branch that is merely behind `main` isn't flagged for a deletion a rebase will apply anyway.

The [merge driver](#the-merge-driver-resolve-queue-rows-by-id-not-by-line-position) closes the reorder-over-delete path at the source — it decides by ID, so a relocated row cannot outvote a deletion — but only for people who installed it, and only for local merges. The lint rule stays the load-bearing check.

Deliberately re-opening a closed item? `BACKLOG_ALLOW_RESURRECT="Q1 Q2" make lint-backlog`.

**To inspect by hand**, list the IDs your branch has that `origin/main` does not:

```bash
comm -23 <(grep -o 'id="Q[0-9]*"' docs/STATUS.md | sort -u) <(git show origin/main:docs/STATUS.md | grep -o 'id="Q[0-9]*"' | sort -u)
```

Every ID it prints should be one *you* filed. Anything else is a row `main` deleted and your rebase brought back — check whether its work shipped before you push.

## When the context doesn't fit, write the doc — whatever the item's size

The trigger for writing a doc is **information loss, not item size**: `Sz` estimates effort, not how much context the work rests on. If fitting the caps means dropping a decision the work depends on, an investigation finding a future session would re-derive, or a blocker's rationale — write (or extend) the doc and link it. Compressing prose is fine; dropping a clause because it doesn't fit is the signal. The content picks the home:

| Kind of context | Home | Why |
|---|---|---|
| **Durable rationale** — decisions, security governance, why a default is what it is | `docs/design/` | Survives plan archival; still there in two years. |
| **In-flight work context** — findings, phases, what's left | `docs/plan/<qNNN>-<slug>.md` | Archived on close (below). |

When a plan closes, **promote its load-bearing conclusions into `docs/design/`** rather than letting them archive out of reach — Queue rows and code cite the durable layer, never a plan path.

## A row's asserted defect is a claim, not a finding

A Queue row is read months later as established fact. Write it so a future session can tell what was measured from what was inferred: state the measurement that establishes the defect, or say plainly that the mechanism is unverified.

An unmeasured mechanism costs more than the row saves. Q584 was filed asserting that `check-path-filters.sh`'s awk YAML parsing could "mis-read as full coverage, failing green". That was wrong — the gate iterates a hardcoded filter registry against `go.work`, so a parse failure *removes* patterns and fails closed — and the row reached `main` before anyone tried to reproduce it, costing a second PR to correct. The real defect had the opposite sign: a valid reformat made the gate emit twelve errors naming patterns that were already present.

Rows that name an unknown are honest and useful — several in the Queue say "unmeasured live — confirm X before building". That phrasing is the pattern: it tells the next session where to start, instead of sending it to repair something that already works. Only a stated *mechanism* needs this; a symptom ("this test flaked on run N, passed on rerun") is already an observation.

**The title carries a claim too, and it is the half that gets read.** A picker reads the Item cell, names the branch, commit and PR after it, and may never re-read the Notes. Q656 was filed with a sound measurement in its Notes (two rows took Q644; the loser renumbered across a commit, a PR body and a plan doc) under a title asserting a mechanism nobody had checked: "`make queue-id` reports a free ID but reserves nothing". The reservation existed and worked, an atomic ref claim with 240 live IDs, so the title named a defect the code did not have while the Notes named one it did. When the cause is unverified, title the row with the symptom: "two sessions took the same Q-ID" survives being wrong about why, and still sends the next session to the right place.

## Flake fixes go first

When a CI flake is observed (test passes on rerun, no code change in between), file it as a Queue item **and move it to the top of the Queue** before continuing other work. Then pick it up next. Flake cost compounds: a 1-hour fix saves cumulative CI wait + diagnosis + context-switch overhead across every future PR that hits it. This overrides default ordering even over critical security items — those are typically M/L-sized and themselves benefit from flake-free CI. Annotate the row's Notes with "**Top of queue per flakes-first rule**" linking this section.

Exceptions: a flake rooted in an outside service that hasn't recurred (file, don't bump); a flake whose fix is blocked on infrastructure that doesn't exist yet (file, mark 🚫, don't bump).

**Once the mitigation ships, move the row to [Flake watch](../STATUS.md#flake-watch)** — a Deferred subsection whose revive mechanic differs from the rest of the table: the trigger is always `**Event:** recurs on main after the fix`, and on recurrence the row returns to the **top** of the Queue, escalated (the first mitigation didn't hold). Keeping the row (rather than closing it) preserves the memory that a fix was already attempted, so a second occurrence reads as a recurrence, not a fresh find. The lifecycle:

- **Observed, unfixed** → Queue top (flakes-first); pick next.
- **Mitigation shipped, not recurred** → Deferred § Flake watch.
- **Recurs** → back to Queue top, escalated.
- **Soaked or obsolete** → retire to the ledger (below).

A sighting on a **PR branch** does not meet the trigger, so the row stays in
Flake watch — but it is still evidence the mitigation is incomplete: record it
(on the row, or in the doc the row links) and count the soak from that date
rather than from the fix. Record *which mode* failed, too, where the row's fix
addressed a specific one: [Q549](../STATUS.md#Q549)'s second sighting was a
mode its fix never covered, and a row naming only the fixed mode would have
sent the next session to re-diagnose the wrong thing.

This is the one place the general "done rows are deleted" rule does **not** apply, which makes it easy to miss when a flake fix otherwise looks like a routine change. `scripts/docs/lint-backlog.sh` enforces it (rule 8): a `flake`-labelled Queue row that disappears entirely — measured against `origin/main`, or the pre-commit state under `--staged` — fails the lint, naming the row and pointing here. Retiring a row per the ledger rules below is the deliberate exception: `BACKLOG_ALLOW_FLAKE_DELETE="Q123"` lets specific IDs through.

### Retiring a flake-watch row

Flake watch must not grow without bound — a row whose recurrence-memory has decayed to ~zero still costs a live-table scan every grooming pass and a slice of context budget, for no signal. During a grooming pass (never automatically), retire a row when **either** holds:

- **Soaked** — the covering spec has passed its **blast-radius run threshold** on `main` since the fix merged (table below), with no recurrence (any recurrence bounces the row back to the Queue, so "since the fix" passes are necessarily consecutive); **or**
- **Obsolete** — the flaky test or the mitigated code path no longer exists or was materially rewritten, so the old memory can no longer map to today's code (auto-retire regardless of age).

The two are an **or**, not an **and**: a stable test that simply keeps passing graduates via *soaked*; a deleted/rewritten test graduates immediately via *obsolete*. Requiring both would mean a row never retires while its test sits quietly passing — exactly the row that has served its purpose.

The soak threshold scales with **blast radius** — how much a *false* retirement costs, keyed on one question: if this spec silently started failing *for real* after we retire it, what do we lose?

| Blast radius of a recurrence | Threshold |
|---|---|
| **Infra / CI flake** — a recurrence just costs a rerun (network / timing / disk / registry; e.g. kindnet, calico) | **≥25** passing `main` runs |
| **Correctness-guarding test** — the spec asserts a product behavior, so a false retirement makes a future *real* red read as "known flake, rerun" | **≥50** passing `main` runs |
| **Could mask a data-loss or security regression** | **Do not soak-retire** — root-cause it, or keep watching indefinitely |

The counts are the ~3/N 95% upper bound on the residual per-run failure rate (25 → ~12%, 50 → ~6%); the higher bar buys a trustworthy regression signal after retirement, not just more confidence the flake is gone. Tune per flake — these are floors, not ceilings.

Soak is counted in **runs, not calendar days**: what proves a flake dead is the spec being *exercised* green, and calendar time is only a proxy for that — one that breaks whenever merge velocity shifts or the spec is path-gated and runs rarely. Counting runs measures the thing directly and never needs re-tuning to velocity; the recurrences seen here (Q300, Q291) surfaced within a few dozen runs, inside even the lower threshold. Count green runs of the covering workflow since the fix's merge date:

```bash
gh run list --workflow <name>.yml --branch main --status success \
  --created '>=YYYY-MM-DD' --json databaseId --jq 'length'
```

A run that failed on an *unrelated* flake is excluded, so this undercounts slightly — conservative, which is what we want. One thing run-count can't see: a flake suspected to be **time-correlated** (nightly-load or API-rate-limit windows) should also sit through a few day/night cycles before graduating — judgment, not a fixed number.

On retirement, **move the row to [flake-watch-retired.md](flake-watch-retired.md)** (a cold, greppable ledger) rather than deleting it. That preserves the "a fix was already attempted here" memory at zero live-table cost: if the flake ever returns post-retirement it re-enters as a fresh find, and the ledger is one `grep` away to reconnect the history. Deleting outright throws that memory away and makes the next occurrence look novel.

## The Progress table

`docs/STATUS.md` keeps a plan-level **Progress** table above the Queue — one row per plan doc. Update it only when a plan's overall status changes (⚠️ → ✅, a new plan lands, a plan retires); most STATUS.md commits touch only the Queue. If completing a Queue row closes the last open item under a Progress row, update both in the same commit.

When you remove a Queue row for a **shipped user-facing capability**, also check whether it graduates a bullet on the website [roadmap.md](../roadmap.md) — an "In progress / near-term" item moving to "Available now (1.0)" — and state its true maturity (GA vs. alpha) so the roadmap doesn't overclaim. `make roadmap-check` (in `make check`) catches the drift for you: each forward-looking roadmap bullet carries an invisible `<!-- q:QN -->` annotation, so deleting the row here fails the gate until the bullet moves. Deleting the row and the annotation without moving the bullet defeats it, which is the one case still on you.

### `⚠️` means an open *Queue* row remains — deferred residuals don't count

A plan is `⚠️` only while it has at least one open row **in the Queue**. Intentionally-deferred residuals live in Deferred (or, for non-commitments, in [appendix-g](../design/appendix-g-future-enhancements.md)) and do **not** keep a plan `⚠️`: a plan whose only remainders are Deferred rows is `✅`. This keeps the table honest — `⚠️` reads as "active work remains," not "a box was once left unchecked."

`scripts/docs/lint-backlog.sh` enforces the transition at the moment it becomes owed: deleting the **last** Queue row that links `plan/NAME.md` fails the gate while that plan's Progress row is still `⚠️`, naming the plan and the flip. It fires only on that deletion, never on a steady-state scan — plenty of open rows merely *cite* a completed plan as evidence, and treating those as active work would make the rule cry wolf. For the rare case where the vanished row was such a citation and real work genuinely remains elsewhere, `BACKLOG_ALLOW_PROGRESS_STALE="plan/NAME.md"` admits it.

When you flip a plan to `✅`, add (or update) a **Status** banner at the top of its plan doc naming the Deferred IDs carrying its residuals (e.g. "Status: Complete — residuals deferred as [Q11](../STATUS.md#Q11)"). The plan doc is **not** archived in this case — its `✅` Progress row still references it.

### What parking a row obliges elsewhere

Deleting a row has a documented cascade — the Progress flip, plan archival, the roadmap graduation. **Moving a row to Deferred has the same one**, because parking changes Queue membership exactly as deleting does, and everything downstream reads Queue membership rather than row existence:

| What to check | Why parking triggers it |
|---|---|
| The plan's **Progress** row | A plan whose only remainders are now Deferred is `✅`, not `⚠️` — deferred residuals don't count (above). |
| The plan doc's **Status** banner | That `✅` flip owes one, naming the Deferred IDs and their triggers. |
| The plan's row in [`plan/README.md`](../plan/README.md) | Its status text usually describes the residual as open work. |
| The [roadmap](../roadmap.md) bullet annotated `<!-- q:QN -->` | An "In progress / near-term" bullet must name at least one row still **in the Queue**; an all-Deferred bullet was parked and belongs under "Exploring / longer-term". `make roadmap-check` fails until it moves. |
| Prose cross-references on the same page | Nothing checks these. Moving a roadmap bullet between sections leaves any "the near-term work below" phrasing pointing at the wrong place, and `make doc-links` reads links, not sentences. |

Only the first two were written down before. A 2026-07-30 groom that deferred [Q273](../STATUS.md#Q273) found the other three the expensive way — two by opening a red PR, the third by re-reading the page.

## A label earns its place by discriminating

A label that lands on most rows costs a column and answers nothing. `infra` reached 69 of 160 table rows before it was retired — it had become "engineering work that isn't docs, tests, or security", covering controller bugs, API graduation, GPU support, and CI gates alike. The rows it marked already carried `bug`/`feature`/`security`; the label added no cut.

Three narrower labels replaced it, each answering a question someone actually asks:

| Label | Scope | The question it answers |
|---|---|---|
| `ci` | The build/test gates themselves — `.github/workflows/**`, `make check` and its scripts, lint and coverage plumbing | *What's wrong with the gates?* |
| `dogfood` | The GKE dogfood cluster and its bootstrap/teardown scripts under `scripts/dogfood/` | *What bites me on the next cluster recreate?* |
| `debt` | Refactors, dedup, and dead-surface removal with no behavior change | *What can I clean up without a design decision?* |

Deliberately **not** added: an `e2e` label. `tests` already covers those rows and the item title names the suite, so `e2e` would double-label rather than split. A product change to the AGC or GMC takes no area label at all — `bug`/`feature`/`security` carries it, and the linked path says where it lives.

Apply the same bar to any new label: if you can't name the question it answers, or it would land on more than a third of rows, it belongs in the item title instead.

`lint-backlog.sh` rule 11 holds the vocabulary closed: every label on a Progress, Queue, or Deferred row must appear on the `**Labels:**` line, so adding one is a deliberate edit to that line rather than a typo that sticks. This is what a retirement needs — Q592 was filed wearing `infra` from a branch cut before the split and merged without a conflict, because the two edits touched different rows.

## Don't pre-assign release versions to backlog items

Do **not** tag Queue rows with speculative future release versions (`1.1`, `2.0`). Introduce a release label only once that release is *concretely scoped* — a plan doc defining its Definition of Done exists — at which point the label answers a real yes/no question ("does this block that tag?"). Post-release estimates are guesses that move (churn without signal), position already encodes priority, and an undefined version anchors nothing. The right pattern is the one `1.0-gate` followed: scope the release in a plan doc first, then add the label.

**The corollary: once a release *is* scoped, only what blocks the tag gets a label.** An item that is planned for the release but does not gate it belongs in that release's scope ledger (below), never in a second, softer label class. A soft `1.4-plan` label would be a guess nothing enforces — it goes stale silently, needs relabeling on every slip, and blurs the `-gate` label's one crisp meaning, *the tag waits for this*. The ledger can carry "planned" honestly because its lifecycle matches the release's: written when the release is scoped, archived when the tag is cut.

## Cutting a release: the scope ledger

Deciding a release is worth cutting is a [release.md](../operations/release.md#when-to-cut) question, answered from `scripts/release/release-delta.sh`. Everything from that decision to the tag is a backlog question, and this is its shape.

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

Not a Queue row, for the reason above; but left in prose it becomes the one cut condition with no state anywhere. 1.3 published three RCs and none produced a verdict — rc.1 aborted on routing, rc.2 returned two defects instead of a result, rc.3 aborted on a broken wait — and each was diagnosed alone, because nothing displayed the run of misses. A ledger row makes "no RC has ever passed this" answerable at a glance.

It is the one row that does not flip to ✅ and vanish: rewrite it as each RC reports, and it is the last thing to go green before the tag.

**Punt vs delay — the two ways scope changes:**

- **Punt** (the item leaves the release): remove its `-gate` label and move it to the plan doc's *Explicitly out of scope* table with the reason. It **keeps its Queue position** — punting from a release is a statement about the tag, not a demotion; an item can be too important to rush and still be the next thing worked on.
- **Delay** (the release waits): change nothing. The label stays on, the ledger row stays open, and the tag waits. Leaving the label on *is* the decision to delay — there is no separate marker for it.

Both are reversible and both are cheap, which is the point: the expensive failure is a release whose scope quietly drifts because nothing recorded what it was.

## Archiving completed plan docs

When a plan's work fully lands and `docs/STATUS.md` no longer references it (no Progress row, no Queue/Deferred row), move the doc under `docs/plan/archive/` rather than deleting it. The rationale is usually more valuable than the diff, but a fully-closed plan in the top level of `docs/plan/` is noise for the next session scanning for active work.

**Archive on close, not on audit.** Do this in the same body of work that removes the plan's last `STATUS.md` reference — the moment you delete its final Queue row, or flip its Progress row to `✅` with nothing left open. Two gates (both in `make check`) enforce it so the omission can't ship silently:

- **`make plan-index-check`** fails when an active, non-ⓘ plan listed in `docs/plan/README.md` is no longer referenced by `STATUS.md` — i.e. a plan that should have been archived. To clear it: archive the plan (below), or, if it's ongoing spec/strategy/research, mark its README row `ⓘ`.
- **`make doc-links`** fails on any broken link the move introduces.

The same change should also keep the plan's `docs/plan/README.md` **status text** current: when you delete a Queue row that completes a plan, update that plan's README row in the same edit.

**Keep archival a docs-only operation.** Archival must never touch code — a code edit re-triggers the heavy path-gated CI (e2e / integration / trivy) on what should be a `docs/**`-only move. The way to guarantee that: **code never references a plan by path.** A Go comment must not contain `docs/plan/<file>.md`; cite the durable layer instead — a `docs/design/` or `docs/operations/` doc, or a stable `Q-ID` / appendix `§`-ref (those survive archival untouched). If a plan's conclusion is load-bearing enough that code wants to cite it, promote that conclusion to a durable doc when the plan closes. Prose mentions of a plan's *content* ("Milestone 1 §8") are fine — only file *paths* rot.

`make no-plan-refs-check` (in `make check`) enforces this, with two rules because the languages differ in what they legitimately do with a plan file:

- **Go** has no legitimate use for one, so any `docs/plan/` or `../plan/` path is rejected anywhere in a `.go` file — comment or string literal alike.
- **Shell scripts and `.github/workflows/`** read plan files as data: a workflow `paths:` filter names one, and a script may rewrite one. Those are values, not citations, and a value whose target moves breaks loudly instead of rotting into stale prose. So only **comment** text is scanned, and only a plan **file** path — the thing archival actually moves. A bare `docs/plan/` directory reference and the index `docs/plan/README.md` survive archival untouched and are never flagged.

A comment that must name a plan file — because that file is what the script operates on — opts out inline with a `no-plan-refs: <reason>` marker on the same line. It silences exactly that line and shows up in the diff, so the exception stays reviewable; a whole-file allowlist would silence the next rot too.

**Protocol:**

1. **Confirm STATUS.md doesn't reference the doc.** `grep -n "<docname>" docs/STATUS.md` should be empty.
2. **Confirm the work actually landed.** Read the plan's Status banner if it has one; otherwise grep the codebase for the named tests, types, or behaviors the plan promised. A plan with open work is **not** archive-ready — leave it in place and make sure the open work has a Queue row.
3. `git mv docs/plan/<docname>.md docs/plan/archive/<docname>.md` — preserves history.
4. **Update any in-repo links** to the new path: `docs/plan/README.md` (move the row to the **Archive** section), other plan docs (`grep -rn "<docname>.md" docs/plan/`), the `docs/development|design|operations` trees, and **the moved doc's own outbound links** — dropping a level into `archive/` breaks every relative link in the doc itself (`make doc-links` catches all of these).
5. **Bundle archival in one commit** when several plans close in the same session — easier to review and revert as a unit.
6. **Do not edit STATUS.md in the same commit** as the archive move — STATUS.md edits are always isolated.

A plan that is partially complete stays in `docs/plan/`. Archive is for "everything in this doc has shipped," not "most of it has."
