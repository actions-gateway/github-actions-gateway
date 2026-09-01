# Agent reference: Maintaining the backlog

[`docs/queue/`](../queue/README.md) is the single source of truth for project progress and priorities: one file per item, with priority carried by a `rank` key rather than by a line position.
Almost every session edits the backlog, so keeping churn low matters as much as keeping it accurate — and one file per item is most of how that is achieved, since two sessions touching different items now touch different files.

The format and process come from the globally-installed **[`session-backlog`](skills.md#session-backlog) skill**, which agents invoke for the full playbook: the format, adding, picking, deferring, completing, the grooming checklist, staleness signals, and migration.
**This page does not restate any of that.** It states what is true *here* — the character caps, the ID allocator, the release scope ledger, the flake-watch lifecycle, and every measurement taken in this repo.
**Where the two overlap, this page wins**; read a difference as this repo being more specific, not as the skill being wrong.

A contributor reading this page alone therefore gets the deltas rather than the process.
What holds the rules for them is the tooling below, which is in-tree and runs in `make check` and the pre-commit hook whether or not the skill is installed:

- [`scripts/docs/queue.py`](../../scripts/docs/queue.py) — the store's reader, checker and order tool, vendored byte-identical from the skill and never edited here.
  `queue.py lint` is a pure function of the directory: frontmatter, rank shape, filename/id agreement, the 72-character title cap, unresolvable targets.
- [`scripts/docs/check-queue-rules.py`](../../scripts/docs/check-queue-rules.py) — the three rules `queue.py lint` cannot express, because each is a function of what the *branch changed* rather than of what the store holds: a `flake` item may not simply vanish, deleting a plan's last item obliges its index row, and the label vocabulary is closed.
  Backs `make queue-rules-check`; runs in `make check`, `make queue-gates`, and CI ([`status-lint.yml`](../../.github/workflows/status-lint.yml)).
- [`scripts/docs/alloc-queue-id.sh`](../../scripts/docs/alloc-queue-id.sh) — allocates a new Q-ID (`make queue-id TITLE="…"`) by claiming a ref on the remote, so concurrent sessions never take the same one.
  Rationale, the alternatives weighed, and what it does *not* fix: [queue-id-allocation.md](queue-id-allocation.md).
- [`scripts/docs/find-duplicate-rows.sh`](../../scripts/docs/find-duplicate-rows.sh) — the near-duplicate search that allocation runs before it claims an ID.
  Advisory: it never blocks a filing.
  [How it is calibrated](#search-before-you-file).
- [`git-merge-plan-index.sh`](../../scripts/docs/git-merge-plan-index.sh) and [`git-merge-roadmap.sh`](../../scripts/docs/git-merge-roadmap.sh) — merge drivers for [`docs/plan/README.md`](../plan/README.md), keyed on the plan path, and [`docs/roadmap.md`](../roadmap.md), keyed on each bullet's backlog annotation.
  One `make merge-driver` per clone installs them; a no-op until then.
  **The backlog itself has no driver, deliberately**: one file per item is what the driver was compensating for, and two sessions editing the *same* item have a real conflict that must not be resolved by ID behind anyone's back.
  [Details below](#the-merge-drivers-resolve-registry-rows-by-key-not-by-line-position).
- [`scripts/docs/next-task.sh`](../../scripts/docs/next-task.sh) — prints a kickoff prompt (or `--title`) for the top ready item, for starting a fresh session on the next task.
  A thin forward to `queue.py next` since Q889; it reads `docs/queue/` and takes no file path.
  **The pick is checked against open pull requests before it is printed** (Q990): `next` lists them and skips an item one already names, so the instruction to check no longer sits in three places with nothing performing it.
  A hit is a candidate rather than a verdict, because an id is cited by neighbouring rows and by the retro that filed it, so the skip names the PR and `--allow QNNN` takes the item anyway once you have read it.
  When `gh` cannot answer it fails rather than handing out an unverified pick; `--no-pr-check` is the deliberate offline escape, and it says on stderr that nothing was checked.
  `lint` reaches no network, so the gates that run it are unaffected.
- [`scripts/docs/backlog-metrics.sh`](../../scripts/docs/backlog-metrics.sh) — replays the backlog's git history into flow metrics (throughput, cycle time, prune ratio, aging WIP).
  Read-only, and continuous across the storage move: it reads the retired table by path and the store after it, suppresses the two bulk commits at the seam, and marks where the storage changed so a span across it stays one span (Q889).

## Where the format differs from the skill

- **There is no `**Next ID:**` counter line.** IDs are claimed instead, with `make queue-id TITLE="…"`, which takes a `refs/queue-ids/QN` ref on the remote.
  A shared mutable counter handed concurrent sessions the same ID and conflicted on the same line by construction, forcing a renumber ([Q382](queue-id-allocation.md#why-the-counter-had-to-go)).
  Allocation also runs [the duplicate search](#search-before-you-file), which is the part of "search before you file" a mechanism can carry; reading the candidates it prints is the part that is on you.
- **The title is capped and the body is not**, because their homes differ.
  **A title is at most 72 characters**, enforced as `TITLE_MAX`, because it renders whole in every index row and in `queue.py next`'s kickoff prompt, which is the one field with nowhere to overflow.
  A row's body has no cap at all, so an item can hold its full context and a finding never has to be trimmed to fit.
  What the rendered table does instead is *truncate*: `summarize()` shows the first sentence, or a clean cut at 140 characters, because a table rendering every note whole is squeezed by its own longest row.
  So the constraint on a note is that its **first sentence carries the point**, not that the note is short, and a row too long to skim wants a linked doc for the same reason a long function wants a name.
  This is the one rule the store inverted when rows became files, and the docs said otherwise for long enough to cost real evidence: five sessions trimmed rows to a 250-character cap nothing enforces, losing detail each time.
  The table capped notes and never capped titles at all, so adopting the store imposed a constraint this backlog had never been held to: 62 of 173 titles were over, the longest at 130, and all 62 were rewritten by hand rather than truncated.
- **An item never cites a count of the backlog.** "42 Queue rows", "60 parked" and friends go stale on the next filing — the file they measure is the one thing guaranteed to change under them, often the same day.
  State the *shape* instead ("the Queue is read top-down; the parked rows are only grepped on a trigger"), and put any dated figure in the linked plan doc, where a point-in-time measurement belongs.
  Q569's row was corrected twice in one session — 36 → 42 → 44 — before the count came out entirely.
- **A non-commitment belongs in [appendix-g](../design/appendix-g-future-enhancements.md)**, never parked in Deferred.
  Deferred triggers are tagged by source here: `**Demand:**` (an outside ask) · `**Event:**` (an observable outside-our-control condition) · `**Decision:**` (our own call — grep `**Decision:**` for what we could move on unilaterally).
- **`make queue-unblock ID=QN` lists every dependent** when a blocker lands, which is what makes the skill's `Blocked by [QN](#QN)` convention worth writing exactly.
- **A row's asserted defect gets re-checked before it is implemented, not only when it is groomed.** Q506's row named a `noproxy` GHES gap that Q322 had already fixed; taking the row at its word would have "fixed" a non-bug and missed the real one next to it.
  An audit row inheriting an unverified premise is the sharpest case, but the rule is general — [what a row can be wrong about, and what each mistake cost](#a-rows-asserted-defect-is-a-claim-not-a-finding).
- **The isolated-commit rule is gone with the table it protected.** A backlog edit no longer has to be its own commit, because the cost it was avoiding was a rebase conflict on one contended file, and one file per item removes it at the source ([what it was, and what replaced it](#isolated-commits-and-what-replaced-them)).

## Isolated commits, and what replaced them

**The rule was: a commit touching `docs/STATUS.md` touches nothing else.** Not "no code" — nothing.
Two gates enforced it, a pre-commit hook reading the index and `check-status-isolation.sh` reading the branch's commits, because the hook was structurally blind to an `--amend` onto a code commit (Q652).

**It is retired, and the reason it existed is what retired it.** Isolation bought exactly one thing: a rebase conflict on the backlog stayed confined to one file and resolved trivially.
That cost came from every session editing the same file, which is the property the item store removes — two sessions filing different items now write different paths, and git has nothing to reconcile.
A rule whose whole benefit was containing a conflict that can no longer happen is overhead, and a real one: it forced a plan doc, a roadmap bullet and the item they belong to into three commits.

**What is still true is the amend hazard, in its general form.** `git commit --amend` rewrites `HEAD`, never the commit owning the path you meant to fix, so amending to correct an earlier commit folds the fix into the newest one instead.
Nothing warns you, because the amend is an ordinary command that did what you asked.
Read `git show --stat HEAD` before amending, and use `git commit --fixup=<sha>` with `GIT_SEQUENCE_EDITOR=true git rebase --autosquash origin/main` when the target is not `HEAD`.

**Backlog edits still get their own commit when the change also touches code**, not as a gate but as a courtesy to review: the item is the *why*, the code is the *what*, and a reviewer reading one should not have to separate them by hand.

### A gate label and its roadmap bullet are two commits, and the first one is red

`roadmapcheck` rule 7 requires a row labelled `X.Y-gate` **and** carrying `feature` or `security` to be named by a roadmap bullet, so adding the label and adding the bullet are one change in intent.
They land in different files regardless: the label lives on the item in `docs/queue/` and the bullet in `docs/roadmap.md`.

**A gate label answers two questions, and only one of them obliges a bullet.** Release scope is not all one kind: a capability or a security fix is what someone upgrades *for*, while the CI, test, docs and dogfood work that also blocks a tag is process.
Requiring a bullet for both put our own release harness on the page people read to evaluate the product, so the obligation follows `feature`/`security` rather than the gate label alone.
Gate a process row freely; it stays out of the roadmap and the release still waits for it.

Read on its own, the `docs(status):` commit therefore fails rule 7, which looks alarming enough to go hunting for a way around it.
There is none, and none is needed: `roadmapcheck` reads the **working tree**, not each commit in turn, so what `make check`, CI and the merge queue all judge is the pair together.
Order the two commits however you like and check the tip.
Only `status-isolation-check` reads commits individually, and it has no opinion about roadmap bullets.

## Closing a row: what else moves

Closing an item is the one backlog edit with reach outside `docs/queue/`.
The row is an anchor, a plan doc's last reference, and often a roadmap bullet's reason to exist, so removing it breaks things in files the closing change never opened.
Every one of these is caught by a gate, and every one is cheaper to do up front than to diagnose from a gate failure pointing somewhere unexpected.

Work through all four:

1. **De-link the ID wherever the repo cites it.** `grep -rn "queue/QNNN.md" docs/` finds every link that is now dead (`make doc-links`).
   Rewrite them as a **bare `QNNN`**, the form the Archive rows in [`docs/plan/README.md`](../plan/README.md) already use.
   Keep the prose; only the link goes.
   In an active plan's Status cell the de-link is gated rather than tidy: `make plan-index-check` requires a live row to be linked and a closed one to be bare, so the anchor dying is what puts the cell in front of someone (Q800).
   Re-read the whole cell while you are there, since it is a rollup no individual row owns.
2. **Delete the roadmap bullet, continuation lines included.** A forward-looking bullet exists because the row does ([rule 7](#a-gate-label-and-its-roadmap-bullet-are-two-commits-and-the-first-one-is-red)), so it goes when the row does.
   Its indented follow-on lines are part of the same list item: leave one behind and Markdown attaches it to the **previous** bullet, whose word count then breaks the cap.
   `make roadmap-check` names the stray line and the bullet it landed on (rule 12), and reports the cap over the line span it actually counted, so a violation on a bullet you never touched no longer reads as a pre-existing failure.
3. **Archive the plan doc if this was its last backlog reference**, per [the protocol below](#archiving-completed-plan-docs), whose step 4 is the one most often missed: dropping a level into `archive/` re-bases **the moved doc's own outbound links**, not just the links pointing at it.
4. **Update the plan's `docs/plan/README.md` row** in the same change, moving it to the Archive section.

The cluster is wider than the docs tree: Q790 was the same shape in the merge tooling, where the since-retired piped-gate hook's backlog overlap exemption discounted the path unconditionally and so stayed silent on exactly the row *deletion* the driver refuses to resolve: a row deleted on one side and edited on the other.
When something new mishandles a closing row, it belongs with these rather than as a fresh curiosity.

### Repurposing an ID is a closure with every step skipped

A measurement that refutes a row's asserted defect usually hands you a different one, and the cheapest edit is to rewrite the row in place: same anchor, same ID, new title, new Notes.
That one edit is a closure and a filing wearing a single row, and it skips both halves.
The four steps above never run, because nothing looks deleted, and `make queue-id` never runs, because no ID is being taken, so [the duplicate search](#search-before-you-file) never happens either.

**An ID is bound to the observations the row was filed on, not to its title.** Retitling is routine and stays inside the row: [a symptom title earns its mechanism](#a-rows-asserted-defect-is-a-claim-not-a-finding) once the mechanism is measured, which is how Q553, Q703 and Q827 all reached their final titles.
The line is the evidence, not the wording.
When the measurement leaves the row's own observations describing a *different* defect, that row is finished: retire it with the refutation recorded, and let the new defect take a new ID.

#1441 is the instance.
Q809 was filed on three `e2e-calico` failures read as NetworkPolicy enforcement negatives; diagnosis attributed all three to one scale-set drain-recovery spec, which is [Q549](../queue/Q549.md)'s mode B. The diagnosis was right and the retitle in place cost three things anyway:

- **Every citation of Q809 re-pointed silently.** A repurpose leaves the anchor alive, so `make doc-links` and rule 5 see nothing: they catch a dead anchor, never a live one that has changed meaning.
  Seven files cited Q809 in its original sense at the moment it changed meaning, and three of them still asserted the refuted failures two days later, in a workflow comment, a spec comment and `testing.md`.
  A closure would have run `grep -rn Q809` over all seven as [step 1](#closing-a-row-what-else-moves).
- **The refuted hypothesis left the backlog.** `calico` appears nowhere in it afterwards, so nothing recorded that the enforcement negatives had been suspected and cleared, and the next session to see a calico failure starts cold.
  [The ledger](flake-watch-retired.md) is where that belongs.
- **The cross-link was care, not mechanism.** #1441 named Q549 by hand.
  Passing the new title to `make queue-id` returns Q549 at 0.43 on the same target, which is the prompt [nothing else supplies](#two-rows-on-one-defect-cross-link-them-and-say-which-owns-the-measurement).

**Rule 8 pushes toward the repurpose, which is worth knowing before it does.** It keys on the item file still being there, so a `flake` item whose defect is swapped out reads as correctly preserved.
Measured 2026-08-30 on a two-arm probe against the shipped checker: the repurpose passes, and deleting the same item fails with rule 8 naming `QUEUE_ALLOW_FLAKE_DELETE`, so the honest closure is the one that hits a red gate and the shortcut is the one that goes green.
For a flake item whose defect was **refuted rather than fixed**, retire it to [the ledger](flake-watch-retired.md) with no fix PR, which is a third route out of flake watch alongside [soaked and obsolete](#retiring-a-flake-watch-row).

**Nothing gates this, and a title check is the wrong gate to reach for.** Scoring every item's before/after title across the backlog's whole history with the same matcher `make queue-id` uses (1,108 commits touching `docs/STATUS.md`, 80 title changes) leaves 27 whose new title does not match its old one.
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
Candidates print to stderr, so `ID=$(make queue-id TITLE="…")` still works, and **the allocator blocks nothing**: the filer routinely knows something the matcher cannot, such as that two rows sharing a file are genuinely separate defects.
Say which, in the new row's Notes.

**Saying which is rule 13, and it is a gate.** `check-queue-rules.sh` re-scores every item a branch adds against the store as it stood at the merge base, and fails when the matcher flags a candidate the new row names nowhere.
Naming any one of them clears it, because what the rule asks is that the warning was read rather than that every candidate is a duplicate: [Q830](../queue/Q830.md) was a false positive and answering it was still the right move.
`QUEUE_ALLOW_UNCITED_DUPLICATE="Q123"` is the deliberate pass.

**What it cannot reach**: scoring against the merge base means two rows filed on concurrent branches never see each other, since neither branch's base holds the other.
Q922 and Q924 were exactly that, filed three hours apart on 2026-08-18, so rule 13 could not have caught that pair; only the Q987 filing a week later, which is the one that made three.
Rows filed in one commit are also unscored against each other, and on the matcher's own verdict some of those are duplicates rather than one editorial act; that is a deliberate trade for not reddening every retro, not a claim they are all distinct.

It exists because the advisory left no trace.
[Q987](https://github.com/actions-gateway/github-actions-gateway/pull/1812) was filed while the matcher scored Q922 at 0.50, named it zero times, and became the third row describing one defect; two sessions then picked different rows and collided at review.
The matcher is quiet enough to gate on: over the store at `2eec06d51`, 25 flagged pairs out of 17,955 across 190 items, measured 2026-08-31 with `find-duplicate-rows.sh --audit`.
The reading names a commit because it moves with the store: the same figure was 26 of 18,336 one merge earlier, and closing two items changed it.

**The title is mandatory, and there is no untitled batch form.** An optional argument is a gate nobody passes through, and `-n 3` was one: it claimed IDs without naming a single row.
Several rows at once means several titles: `scripts/docs/alloc-queue-id.sh` takes one argument per ID and searches each on its own, which is what a retro filing four rows actually wants.
Nothing automated calls the target, so making the title mandatory changed only the lines that document it.

**Every path through it claims, and a row whose ID holds no claim fails the lint.** `PEEK=1` used to report the next free ID without taking it, which two concurrent sessions read identically: the counter this mechanism replaced, behind a flag.
It is gone, and rule 12 catches the other way to obtain an unreserved ID (reading the file's highest and adding one) at the commit that files the row rather than at the rebase that collides.
What that cost in practice, and the one case rule 12 still cannot see: [queue-id-allocation.md § Reserving, not reporting](queue-id-allocation.md#reserving-not-reporting).

`TARGET=<link>` is optional and worth passing when the Item cell's link is already decided; it is a second matcher signal, and rule 13 reads it from the filed row's `target:` either way.

### Escalating a class observation is filing, so search first

`make queue-id` is a chokepoint for *filed* rows only.
Raising "these N failures look like one shared cause" in a message to the maintainer, or to a dispatcher, reaches the same reader with the same weight and passes through no matcher at all.
A message to a human is a filing with the safety rail removed.

It fired twice on 2026-08-12, in both directions.
A worker raised the "X-test fails under `make check`, passes standalone" family as an unfiled class; a dispatcher relayed it, citing Q596's 2418 runs and Q703's 240 as evidence that no class row existed.
[Q738](../queue/Q738.md) already said "measured across this family" and already named those two rows, so the maintainer was asked to decide something on the premise that it was unfiled.
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

An advisory that fires constantly is worse than none, so the thresholds are measured rather than asserted.
`scripts/docs/find-duplicate-rows.sh --audit` runs every shipped row back through the same scoring path the search uses, and prints what flags:

```bash
scripts/docs/find-duplicate-rows.sh --audit
```

**Roughly one row in five surfaces a candidate when filed, and every pair it flags is topically adjacent rather than a nonsense match.** The snapshot behind that, on 2026-08-04 (72 Queue + 31 Deferred + 15 Flake-watch rows): 11 flagged pairs out of 6,903.
The rate held across every backlog state this was measured in — the Queue turns over faster than any fixed count survives, which is why the figure is a dated instance and `--audit` is the live answer.
Two of the eleven look like real duplicates nobody caught: Q663 and Q612 are both `check-doc-links` defects, and Q660 and Q588 are both the doc-update-matrix sending a row into a `scripts/README.md` table that does not exist.

Loosening either ratio by 0.05 roughly doubles the count.
Re-run the audit before changing a threshold.

## The merge drivers: resolve registry rows by key, not by line position

A registry file — one row per plan, per script, per gate — puts every branch's edit in the same place, and a plain three-way merge decides by line position.
One untouched row of separation merges cleanly; **adjacent** rows do not, so concurrent work collides by construction ([the measurements](queue-id-allocation.md#what-this-fixes-and-what-it-does-not)).
Four files here are routed to a driver that decides by **row key** instead: a row deleted on either side is deleted, a row added on either side is present, a row changed on one side takes that change, and row order is rebuilt from whichever side reordered.
They run on local merges, rebases, cherry-picks and stash applications — everywhere the pain is.

**The backlog is deliberately not one of them.** It was, until Q889: `docs/STATUS.md` was one table every session edited, and `git-merge-status.sh` resolved its rows by ID.
One file per item retires both the contention and the driver.
Two sessions filing different items write different paths and git has nothing to reconcile, and two editing the *same* item have a genuine disagreement that must surface as a conflict rather than be resolved by ID behind anyone's back.
Adding `docs/queue/**` to `.gitattributes` is the helpful-looking move that would rebuild the problem the store exists to remove.

**One-time setup, per clone:**

```bash
make merge-driver
```

`.gitattributes` already routes each file to its driver, but git deliberately refuses to let a tracked file define a driver's *command* — that would be remote code execution on clone — so the config half is per-clone and opt-in.
**Nothing requires you to install it:** until you do, the attribute names an undefined driver and git silently uses its built-in three-way merge, which is exactly the pre-driver behaviour.
[`scripts/dev/setup.sh`](../../scripts/dev/setup.sh) installs it for you, as it does the git hooks.

**What a driver refuses to resolve.** Every uncertainty ends the same way — the plain three-way merge re-runs and its conflict markers stand, with a one-line reason on stderr: a row changed on both sides, a row deleted on one side and edited on the other, one key filed on both sides with different content, rows reordered on both sides, a row whose key is missing or malformed, and any conflict outside the keyed rows.
A conflict marker costs a minute; a wrongly resolved row loses state.

**The refusal is per row, but the fallback is per file.** Every refusal ends by re-running `git merge-file` over the whole file, so the hunk it marks spans the refused row *and* every row added beside it on either side: one refused row produced a five-row hunk in [the measurement below](#a-hand-resolved-conflict-drops-rows-the-markers-never-named).
Picking a side of that hunk is what loses rows neither side disagreed about.

**They do not help GitHub**, which cannot see a clone's config: the server-side squash-merge, the mergeability read behind `mergeStateStatus`, and the merge queue's candidate build all take the plain three-way merge.
A driver-resolved merge is also still a merge you own: read the resulting row set, then run the gates.

### The same treatment for `docs/plan/README.md`

The plan index has the same contention and the same cause.
Every plan doc that lands adds one long row, every archival moves one to the top of the Archive table, and the topical sections concentrate both on the same few neighbours.
Over the 22 changes to the file that merged between 2026-08-01 and 2026-08-03, **18 of the 231 pairs touch a row in common; the other 213 disagree only about line position** — and adjacency makes a plain three-way merge conflict on those anyway.

[`scripts/docs/git-merge-plan-index.sh`](../../scripts/docs/git-merge-plan-index.sh) decides them by the **plan path in column 1**, sharing the Queue driver's row rules and its refusal discipline.
That key is not a new convention: [`check-plan-index.sh`](../../scripts/docs/check-plan-index.sh) already reads the same cell, so the driver and the gate cannot disagree about what a row is.

Two things are specific to it:

- **It merges every table in the file, not one named section.** Archiving a plan is a delete in a topical table and an add in the Archive table, and a section-scoped merge would read that as an unexplained deletion.
- **It checks the whole file afterwards for a plan listed twice**, comparing basenames so `archive/x.md` and `x.md` count as one plan.
  Per-table merges cannot see that pair, which one branch archiving a plan while another relocates it produces.

Everything else is the shared behaviour: a row changed on both sides, a row deleted on one side and edited on the other, one plan filed twice with different text, rows reordered on both sides, a row whose first cell is not a link, and a side that added or removed a whole table all fall back to the plain three-way merge and its conflict markers.

### And for `docs/roadmap.md`

The public roadmap is bound to the backlog one bullet at a time: each forward-looking bullet carries a `<!-- q:QN -->` annotation naming the rows behind it, and shipping the work deletes the bullet.
That makes every gate PR the same edit in the same two sections.
Measured on `docs/roadmap.md` at 61cf54e7b: two branches each deleting their own bullet from the near-term list conflict under a plain three-way merge, while the same two deletions ten bullets apart merge clean.
[Q715](https://github.com/actions-gateway/github-actions-gateway/pull/1392)'s PR met that shape three times in one session, each time as a merge-queue eviction followed by a hand-resolved rebase.

**What the driver buys is the rebase, not the eviction.** A merge driver is per-clone `git config`, and GitHub builds the merge queue's candidate itself, so the server-side conflict recurs exactly as before ([merge-queue.md](../plan/merge-queue.md) measured the same thing for the backlog table, which had a driver from Q611 until Q889 retired both).
What changes is the heal: the rebase that follows an eviction resolves silently instead of by hand.
Fewer evictions is a different problem, and the lever is spacing before serializing: the same two deletions ten bullets apart merged clean, so a batch whose bullets sit apart on the page never meets the conflict.

[`scripts/docs/git-merge-roadmap.sh`](../../scripts/docs/git-merge-roadmap.sh) decides the bullets by that **annotation**, normalized to a comma-joined ID list.
The key is not a new convention either: `devtools/docs/roadmapcheck` already parses the same comment, so the driver and the gate cannot disagree about what a bullet is.

Two things are specific to it:

- **A bullet spans several lines**, which the shared record rules do not model, so each one is encoded onto a single line and decoded after the merge.
- **The blank lines between bullets are held beside the records, not inside them.** Fold the trailing blank into a bullet and deleting a list's last bullet reads as an *edit* of its neighbour, which then collides with the other side deleting that neighbour: the exact merge the driver exists to resolve.

A run of bullets is only merged this way when every bullet in it is annotated, so an ordinary bulleted paragraph elsewhere on the page keeps git's own merge.
Everything else is the shared behaviour, plus a whole-page check that no binding ended up on two bullets.

`make merge-driver` installs all four drivers ([`scripts/README.md`](../../scripts/README.md#per-clone-setup) lists them).
None is required: until you run it the `.gitattributes` lines name undefined drivers and git uses its built-in three-way merge, which is exactly the pre-driver behaviour.

## Verifying a backlog-only change: run the subset, push now

A backlog-only change cannot fail most of `make check`, and the full gate takes ~6 minutes.
Run the subset instead:

```bash
make queue-gates
```

That is the complete set a `docs/queue/`-only diff can fail, and every member is also in `make check`, so it is a strict subset and never a second opinion.
CI runs the full gate on the pushed branch regardless.

**Run the target, don't transcribe its contents.** This list was once spelled out here as three `make` targets, and it was wrong: `roadmap-check` and `plan-index-check` both read backlog membership and both can fail on a backlog-only diff.
A grooming pass that parked a row followed the three-target list, went green locally, and opened a PR red on `roadmap-check`.
The set now lives in the `QUEUE_GATES` variable in [`mk/gate-lists.mk`](../../mk/gate-lists.mk), whose comment names each member and what it catches, so there is one copy to keep true.
Transcribing is not the only way it drifts: `em-dash-check` and `page-density-check` both scan the backlog and were both missing from the variable while its comment called the list complete (Q749), so `make gate-lists-check` derives membership from the pathspec each gate hands git and fails when a fast gate that selects an item file is left out.
It fired again at the cutover, on `page-density-check` against all 178 store pages.

**When it does not apply:** if the change touches *any* other file — code, a plan doc, another page under `docs/` — run the full `make check` (plus whatever heavier tier the change warrants).
Closing an item routinely [cascades](#what-parking-a-row-obliges-elsewhere) into a plan doc, `plan/README.md`, or a roadmap bullet, which puts the diff outside this section's narrowness.
Author the change first, then pick the gate by what the diff actually touches.

### Two gate behaviours a failure message does not explain

Both hit two sessions independently on 2026-08-27, and neither is inferable from what the gate prints.

**`queue-lint` keys on the bare `name.go:NNN` text wherever it appears, link label included.** A row citing a source line is asked to re-point or drop it, and the obvious remedy does *not* clear the note: turning the citation into a proper Markdown link leaves the pattern matching inside the label.
Only moving the number out of the pattern works: write "at line 457 of [`pod_provisioning_test.go`](../../cmd/agc/internal/controller/integration/pod_provisioning_test.go)" rather than linking the `file:line` string itself.

**`mdreflow` silently collapses a header-less table onto one line, and the gate then passes.** `md-reflow-check` is sentence-per-line, so a Markdown table written without its `|---|` separator row is not a table to the parser; it is prose, and the formatter joins its rows.
Running the formatter to satisfy the gate therefore turns two rows into one unreadable line **and exits 0**, so nothing downstream reports it.
Confirm the file after formatting rather than reading the gate's status, per [the status-is-a-claim rule](testing.md#the-status-you-report-is-a-claim-too): a file change is verified by reading the file, never by the exit code of the call that wrote it.

### A moved row defeated conflict detection, and one file per item ends it

Under the single table, priority was a line position, so a branch that **relocated** a row while `main` **deleted** it produced no conflict at all: git applied the delete at the old position and the re-add at the new one, and a completed row silently came back.
A clean rebase was not evidence of a correct one.
It happened twice on 2026-07-25 and near-missed a third time the next day, which is what bought `lint-backlog` rule 10.

**Rule 10 was measured at the cutover rather than ported, and the measurement said not to build it.** In throwaway repositories on git 2.55.0, both arms run:

| Layout | The merge | Outcome |
|---|---|---|
| Single table, a row relocated far enough from the deletion to clear the diff context | **exit 0, clean** | the completed row is silently resurrected |
| Per-item store, a `rank` edit against a file deletion | **exit 1, `CONFLICT (modify/delete)`** | the file is left in the tree, so resurrecting it takes an explicit `git add` |

The rule existed because a *relocated* row and a *deleted* row are indistinguishable to a line-position merge.
One file per item makes them a modify and a delete of one path, which git refuses rather than resolves, so the silent default the rule was built for is gone and what remains is a careless resolution of a loud conflict.
That residual belongs to the reconciliation habit below, not to a gate.

The first control run got this wrong in the informative direction: a four-row fixture put the relocation inside the deletion's diff context, so the table arm conflicted too and the comparison read as "no difference".
Reproducing the documented silent case needed twenty rows and a relocation from position 18 to the top.
**A probe that cannot reproduce a defect the repo has already met twice is measuring itself.**

### A hand-resolved conflict drops rows the markers never named

A conflict git refuses is loud; the resolution that follows is not.
The case no rule catches is an item that quietly goes *away*, because deleting one is how an item closes: no lint pass can tell a completed item from a casualty.
This applies to the keyed registry files that still have drivers, and to any hand-resolved backlog conflict.

Measured on [#1471](https://github.com/actions-gateway/github-actions-gateway/pull/1471), against the backlog table when it still had a driver: the driver refused a `Q738` row both sides had edited, which is correct behaviour for a keyed merge, and the hand resolution that followed **deleted `Q823` and `Q822`**, rows belonging to `main` that the PR never touched, while **dropping `Q836` and `Q837`**, the two rows the PR existed to add.
`git rebase` reported success, no marker remained, and the backlog gate subset passed on the result.
**An absent marker proves a resolution is well-formed, never that it is correct.**

Reproduced in a throwaway repo with the driver installed (git 2.55.0), one refused row sitting between two rows added on each side:

| Resolving the hunk by | What it cost | What reported it |
|---|---|---|
| keeping the branch's side | the two rows `main` had added | rule 8, and only because both carried `flake` |
| keeping `main`'s side | the two rows the branch had added | nothing: the rebase dropped the branch's now-empty commit and printed `Successfully rebased` |

`git diff --check` exits 0 in both.
The second case is the worse one, because the branch ends up changing nothing at all while its commit subject and PR description still claim two rows.
Do not lean on rule 8 for the first: it covers one label out of the whole vocabulary, and #1471 recorded the gate subset passing on the damaged tree even though both lost rows carried it.
That tree was repaired before it was ever committed, so which of the two accounts of rule 8 applies there is no longer measurable.

**So reconcile the row set, not the markers.** [`reconcile-queue-rows.sh`](../../scripts/docs/reconcile-queue-rows.sh) does it, and needs no count you memorised before the rebase started:

```bash
make queue-reconcile
```

Run it while the conflict is still open — before `git rebase --continue` or `git commit` — or immediately after.
It names every item that left your row set and every item the other side has that you do not, and says which of them the two sides account for: an item is accounted for when the side that no longer has it is the side that deleted it.
Anything else is collateral from the resolution, which it prints with the ref that still holds the file, and exits 1 for.

**Which comparison is right depends on where the resolution currently lives, which is why this is a script rather than two commands to type.** Measured on git 2.55.0:

| State | The row set before | Your row set now | The other side |
|---|---|---|---|
| Rebase in progress | `.git/rebase-*/orig-head` | the **index** | `.git/rebase-merge/onto` |
| Merge in progress | `HEAD` | the **index** | `MERGE_HEAD` |
| Neither | `ORIG_HEAD` | `HEAD` | `origin/main` |

Mid-operation the resolution is in the index, and `HEAD` is the replay so far — it excludes the very commit being resolved.
Reading `HEAD` there reports every row the branch adds as a casualty: on a fixture whose branch files two items and whose `main` closes one, it names three suspects where the index names the one that is real.
That is the reading Q840 shipped, at exactly the moment it told you to run it, so the false positives arrive mixed in with the true one they bury.
Only the third row needs `ORIG_HEAD`, which the next rebase or merge overwrites — pass `--base` to name the pre-resolution tip by hand once it is gone.

A read it cannot take exits 2 rather than guessing: an item still unmerged under the store, a branch that never merged the other side at all, or a ref that does not resolve.
`reconcile-queue-rows-test.sh` pairs every case, and drives real `git rebase` and `git merge` conflicts rather than staging a tree by hand, since a hand-staged fixture agrees with an implementation reading either `HEAD` or the index and cannot see the distinction under test.

**No CI gate can run any of this**, which is why it is a local command and not a check: every ref it reads is gone by push time.
Nor is widening rule 8 past `flake` a substitute, since deleting an item is how an item closes: the rule would then refuse every closure, which gates a different thing entirely.

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
Q602 taught one scale-set listener test to wait on a listener-produced signal and left a comment explaining why; four days later Q685 was the same defect in a sibling test in the same file, and the sweep it finally prompted found a third case.
That one never flaked: its positive assertion held whether or not the listener ever observed the completion, so waiting for CI would never have surfaced it.

**A campaign that pins a flake often measures a production defect too, and a flake filed later often turns out to be one already on the Queue.** Either way the pair gets cross-linked the moment it is recognised, with [one row owning the measurement](#two-rows-on-one-defect-cross-link-them-and-say-which-owns-the-measurement).

**Once the mitigation ships, move the row to [flake watch](../queue/README.md)** — a Deferred subsection whose revive mechanic differs from the rest of the table: the trigger names the recurrence that would show the fix didn't hold, observed on `main`, and on recurrence the row returns to the **top** of the Queue, escalated.
Write it against the flake's own signature (the failing test, the job, the symptom) rather than to a fixed string; *recurs on `main` after the fix* is the fallback for a flake with no narrower one.
Measured 2026-08-30 over the ten parked rows: seven name a test or a symptom, two use that fallback ([Q549](../queue/Q549.md), [Q912](../queue/Q912.md)), and one delegates to a sibling whose class-wide trigger covers it ([Q797](../queue/Q797.md) → [Q1007](../queue/Q1007.md)).
Escalation is the mechanic rather than something a row has to restate, though writing it into the trigger is harmless; two of the ten do.
Keeping the row (rather than closing it) preserves the memory that a fix was already attempted, so a second occurrence reads as a recurrence, not a fresh find.
The lifecycle:

- **Observed, unfixed** → Queue top (flakes-first); pick next.
- **Mitigation shipped, not recurred** → Deferred § Flake watch.
- **Recurs** → back to Queue top, escalated.
- **Soaked or obsolete** → retire to the ledger (below).

A sighting on a **PR branch** does not meet the trigger, so the row stays in Flake watch — but it is still evidence the mitigation is incomplete: record it (on the row, or in the doc the row links) and count the soak from that date rather than from the fix.
Record *which mode* failed, too, where the row's fix addressed a specific one: [Q549](../queue/Q549.md)'s second sighting was a mode its fix never covered, and a row naming only the fixed mode would have sent the next session to re-diagnose the wrong thing.

This is the one place the general "done rows are deleted" rule does **not** apply, which makes it easy to miss when a flake fix otherwise looks like a routine change.
[`check-queue-rules.py`](../../scripts/docs/check-queue-rules.py) enforces it (rule 8), behind `make queue-rules-check` in `make check` and `make queue-gates`: a `flake`-labelled item that disappears entirely — measured against the [merge base](#a-moved-row-defeated-conflict-detection-and-one-file-per-item-ends-it) with `origin/main`, never its tip — fails the gate, naming the item and pointing here.
It keys on the label alone, so an item already parked in flake watch is protected exactly as a `status: ready` one is.
Retiring per the ledger rules below needs no exception: the ledger entry clears rule 8 by itself, and `QUEUE_ALLOW_FLAKE_DELETE="Q123"` is for the other case, a deliberate drop that reaches no ledger at all.

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

## A plan enumerates readers, not references

A plan's scope section lists what a change has to touch, and the natural way to build that list is to grep for the thing's name.
That counts *references*.
What a change breaks is whatever *reads* the thing, and the two sets differ by every reader that never names it.
No better query closes the gap, because the gap is the absence of the string being searched for.

[Q889](../plan/q889-backlog-item-store.md) moved the backlog from one table to the per-item store in six phases, and understated its scope in all five that preceded the cutover, every time in the same direction.
Its phase 6 section records three readers a reference count could not have reached, one of each kind:

| The reader | Q889's case | Why no query reaches it |
|---|---|---|
| A reference in a structured field | `Q878`'s frontmatter `target:` named a suite the phase deleted | It is not link syntax, so no markdown link checker resolves it. `queue.py lint` reads that field and found it on arrival, having been vendored since phase 1 and wired into no gate until the cutover |
| A consumer whose failure is silent | [`hooks/release_gates.py`](../../hooks/release_gates.py) returns an empty gate map when its source is unreadable, so the page renders with no release chips at exit 0; deleting the table would have dropped every one of them on a green build | Grep does find it, since it names the path. Nothing tells you the build that stayed green ran it rather than watched it give up. Phase 5's render guard was the same shape, which makes it a class rather than an instance |
| A glob that crosses into the new tree | `page-density-check`'s `docs/*.md` pathspec selects every item page | Nothing names the store. A git pathspec glob crosses directory separators, so what points at the tree is a separator the query does not stop at. Measured 2026-08-18: 151 of the 408 files it selects are items |

**Enumerate from the reader's side.** Ask each thing that runs what it reads, instead of asking the artifact who names it.
One gate here already works that way: `gate-lists-check` resolves every gate's own pathspec against the real repo and fails when a gate selecting an item is missing from `QUEUE_GATES`, which is how the crossing glob surfaced the moment that list was repointed at the store.
Resolve any glob against the tree the change *leaves*, not the one it starts from, since a directory that does not exist yet matches nothing.

**Write the count as a floor.** "61 files name it, plus whatever reads it without naming it" keeps the number as the start of the enumeration; "61 files" reads as the end of one.
Sort each reader you find by whether it takes the artifact as its **subject** or as a **source**: a gate whose whole reason to exist is a property of that one file retires with it, and a gate answering a question that outlives the storage gets repointed instead.
Sorting on "does it mention the path" answers neither question, and asking the other one is what carried a staleness guard and a seconds-long verification set through Q889's cutover intact.

**A phase that overran its scope is evidence about the method, not about that phase.** Q889 read each miss as local and re-counted, which is why its fifth phase repeated the fourth's mistake and the cutover then found three more.
Re-enumerate the next phase from the reader's side rather than grepping again more carefully.

## Plan-level status lives in the plan index

The backlog table carried a **Progress** table above the Queue, one row per plan doc, and the store has no counterpart: [`docs/plan/README.md`](../plan/README.md) already carried per-plan Status cells with strictly more information, so Q889 deleted Progress rather than migrating it.
Update a plan's Status cell when its overall status changes; most backlog edits touch no plan at all.

When you close an item for a **shipped user-facing capability**, also check whether it graduates a bullet on the website [roadmap.md](../roadmap.md) — an "In progress / near-term" item moving to "Available now" — and state its true maturity (GA vs. alpha) so the roadmap doesn't overclaim.
`make roadmap-check` (in `make check`) catches the drift for you: each forward-looking roadmap bullet carries an invisible `<!-- q:QN -->` annotation, so deleting the item here fails the gate until the bullet moves.
Deleting the item and the annotation without moving the bullet defeats it, which is the one case still on you.

### An open marker means an open *item* remains — deferred residuals don't count

A plan's Status cell carries an open marker (⚠️ ❌ 🚧 🔲) only while at least one item is genuinely open under it.
Intentionally-deferred residuals live in the store with `status: deferred` (or, for non-commitments, in [appendix-g](../design/appendix-g-future-enhancements.md)) and do **not** keep a plan open: a plan whose only remainders are deferred is done.
This keeps the index honest — an open marker reads as "active work remains," not "a box was once left unchecked."

Two gates hold it, from opposite directions.
`check-queue-rules.py` rule 9 enforces the transition at the moment it becomes owed: deleting the **last** item targeting `plan/NAME.md` fails while that plan's index row still reads as open, naming the plan and the flip.
It fires only on that deletion, never on a steady-state scan — plenty of open items merely *cite* a completed plan as evidence, and treating those as active work would make the rule cry wolf.
`check-plan-index.sh` invariant 1 asks the mirror question on every run: a row claiming open work must be backed by a live item, either one targeting the plan or one the cell itself links.
That is what catches a plan whose phases all shipped while its marker never moved, which rule 9 cannot see because no deletion is involved — it found two the day it was written, both reading "all phases shipped" under a ⚠️.

When you flip a plan to done, add (or update) a **Status** banner at the top of its plan doc naming the deferred IDs carrying its residuals (e.g.
"Status: Complete — residuals deferred as [Q11](../queue/Q11.md)").
The plan doc is **not** archived in this case — the deferred items still reference it.

### What parking a row obliges elsewhere

Closing an item has a documented cascade — the plan's status flip, plan archival, the roadmap graduation.
**Parking one has the same cascade**, because deferring changes open membership exactly as closing does, and everything downstream reads whether work is open rather than whether the item exists:

| What to check | Why parking triggers it |
|---|---|
| The plan's row in [`plan/README.md`](../plan/README.md) | A plan whose only remainders are now deferred is done, not open — deferred residuals don't count (above), and invariant 1 fails while the marker says otherwise. |
| The plan doc's **Status** banner | That flip owes one, naming the deferred IDs and their triggers. |
| The [roadmap](../roadmap.md) bullet annotated `<!-- q:QN -->` | An "In progress / near-term" bullet must name at least one item still open; an all-deferred bullet was parked and belongs under "Exploring / longer-term". `make roadmap-check` fails until it moves. |
| Prose cross-references on the same page | Nothing checks these. Moving a roadmap bullet between sections leaves any "the near-term work below" phrasing pointing at the wrong place, and `make doc-links` reads links, not sentences. |

Only the first two were written down before.
A 2026-07-30 groom that deferred [Q273](../queue/Q273.md) found the other three the expensive way — two by opening a red PR, the third by re-reading the page.

## A label earns its place by discriminating

A label that lands on most rows costs a column and answers nothing.
`infra` reached 69 of 160 table rows before it was retired — it had become "engineering work that isn't docs, tests, or security", covering controller bugs, API graduation, GPU support, and CI gates alike.
The rows it marked already carried `bug`/`feature`/`security`; the label added no cut.

Three narrower labels replaced it, each answering a question someone actually asks:

| Label | Scope | The question it answers |
|---|---|---|
| `ci` | The build/test gates themselves — `.github/workflows/**`, `make check` and its scripts, lint and coverage plumbing | *What's wrong with the gates?* |
| `dogfood` | The GKE dogfood cluster and its bootstrap/teardown scripts under `scripts/dogfood/` | *What bites me on the next cluster recreate?* |
| `debt` | Refactors, dedup, and dead-surface removal with no behavior change | *What can I clean up without a design decision?* |

Deliberately **not** added: an `e2e` label.
`tests` already covers those rows and the item title names the suite, so `e2e` would double-label rather than split.
A product change to the AGC or GMC takes no area label at all — `bug`/`feature`/`security` carries it, and the linked path says where it lives.

Apply the same bar to any new label: if you can't name the question it answers, or it would land on more than a third of rows, it belongs in the item title instead.

[`check-queue-rules.py`](../../scripts/docs/check-queue-rules.py) rule 11 holds the vocabulary closed: every label an item wears must appear on the `**Labels:**` line in [`docs/queue/README.md`](../queue/README.md), so adding one is a deliberate edit to that line rather than a typo that sticks.
This is what a retirement needs — Q592 was filed wearing `infra` from a branch cut before the split and merged without a conflict, because the two edits touched different rows.

## Don't pre-assign release versions to backlog items

Do **not** tag Queue rows with speculative future release versions (`1.1`, `2.0`).
Introduce a release label only once that release is *concretely scoped* — a plan doc defining its Definition of Done exists — at which point the label answers a real yes/no question ("does this block that tag?").
Post-release estimates are guesses that move (churn without signal), position already encodes priority, and an undefined version anchors nothing.
The right pattern is the one `1.0-gate` followed: scope the release in a plan doc first, then add the label.

**What that release may contain is a SemVer question, not a priority one.** A patch is bugfix-only, so labelling a `feature` row for one is wrong however badly the release wants it, and the way out is a backport branch rather than a relabel: [release.md § Patch releases and backports](../operations/release.md#patch-releases-and-backports) is the rule and the mechanism both.
Check it before adding a label, because the label is a public promise as much as an internal one (below).

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

Link each ID to its item page while it is open; a shipped or punted item's link goes stale when the file is deleted, so drop it as the row's status flips — the Q-ID itself stays, and it is what git history is searchable by.

**Delivered is a tick, not a narrative.** A row flips to ✅ in the same change that deletes its Queue row — the plan-docs-stay-current discipline already owns that edit, so the ledger costs nothing extra to keep true.

**The cut condition is one grep:** no `-gate` item for this release remains open (`grep -l '1.3-gate' docs/queue/Q*.md`), plus the release-candidate dogfood validation, which is deliberately not a Queue row because it can only run against a published RC.

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

**A `-gate` label is also a public promise.** [`docs/roadmap.md`](../roadmap.md) is where an adopter reads it, so adding or removing one is an edit to both files and a punt that skips the second leaves the promise standing.
`make roadmap-check` reconciles them in both directions: every `X.Y-gate` row must be named by a roadmap bullet's `<!-- q:QN -->` annotation, and a bullet that writes a version into its prose must name a row carrying that gate.
Only the second reads the prose, and only it goes quiet once the version is a derived chip rather than a sentence; the coverage half reads the annotation and the label alone, so it holds whatever the bullet looks like.
Naming a version without claiming to gate it stays free, which is how Q273's bullet names `v2.0.0` while carrying no label.

**A near-term roadmap bullet means "not waiting on an outside signal".** An item that waits on demand, on an unbuilt prerequisite, or on hardware belongs in Deferred with a revive trigger, which puts its bullet under *Exploring / longer-term*.
The [release ladder](../plan/release-ladder.md) is where that rule and the current 1.5 → 1.6 → 2.0 shape are argued; what enforces it is rule 3, since a bullet naming only Deferred rows fails the gate.
The failure mode it exists to catch is the comfortable one: leaving an item in the Queue because parking it feels like giving up.
That is how the section came to advertise nine things as *actively being built* while they waited on demand, a prerequisite, or hardware nobody has.

**A release commitment is the narrower claim, and the `X.Y-gate` label alone carries it.** The rule above read *"committed to a named release"* until 2026-08-18, which held only while every ungated item was also parked.
Reviving Q408 and Q564 broke that: both revive triggers had fired, so rule 4 moved their bullets into near-term, and neither had a release to name.
Both ways of restoring the stronger wording are worse than dropping it: invent a gate label, which publishes a promise nobody decided, or re-park a row whose trigger has fired, which misstates why it is parked (Q843).
So near-term holds some items with a pill and some without, and the pill is the only place a release is claimed.

## Archiving completed plan docs

When a plan's work fully lands and no backlog item references it, move the doc under `docs/plan/archive/` rather than deleting it.
The rationale is usually more valuable than the diff, but a fully-closed plan in the top level of `docs/plan/` is noise for the next session scanning for active work.

**Archive on close, not on audit.** Do this in the same body of work that removes the plan's last backlog reference — the moment you delete its final Queue row, or flip its Progress row to `✅` with nothing left open.
Two gates (both in `make check`) enforce it so the omission can't ship silently:

- **`make plan-index-check`** fails when an active, non-ⓘ plan listed in `docs/plan/README.md` claims open work backed by no live item — i.e. a plan that should have been archived.
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

1. **Confirm no item references the doc.** `grep -rn "<docname>" docs/queue/` should be empty.
2. **Confirm the work actually landed.** Read the plan's Status banner if it has one; otherwise grep the codebase for the named tests, types, or behaviors the plan promised.
   A plan with open work is **not** archive-ready — leave it in place and make sure the open work has a Queue row.
3. `git mv docs/plan/<docname>.md docs/plan/archive/<docname>.md` — preserves history.
4. **Update any in-repo links** to the new path: `docs/plan/README.md` (move the row to the **Archive** section), other plan docs (`grep -rn "<docname>.md" docs/plan/`), the `docs/development|design|operations` trees, and **the moved doc's own outbound links** — dropping a level into `archive/` breaks every relative link in the doc itself (`make doc-links` catches all of these).
5. **Bundle archival in one commit** when several plans close in the same session — easier to review and revert as a unit.
6. **Keep the archive move in its own commit**, so the rename reads as a rename rather than as part of a content change.

A plan that is partially complete stays in `docs/plan/`.
Archive is for "everything in this doc has shipped," not "most of it has."
