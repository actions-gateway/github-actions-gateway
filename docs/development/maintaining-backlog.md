# Agent reference: Maintaining the backlog

`docs/STATUS.md` is the single source of truth for project progress and priorities. It is high-contention — almost every session edits it — so keeping churn low matters as much as keeping it accurate. This doc captures the rules that keep merge conflicts trivial and the file readable.

## The non-negotiables

1. **Isolate `docs/STATUS.md` edits in their own commit**, separate from code and plan-doc changes. Rebase conflicts on STATUS.md should always be resolvable by `git checkout --theirs` or `--ours` on a single file. This is also stated in `CLAUDE.md` and is the highest-leverage rule in this doc.
2. **Run `gh pr list` before picking a task.** A Queue row already covered by an open PR should be skipped, not re-started. The open PR — not a `▶ Started` marker — is the real "in-flight" signal.
3. **Verify 🚫 blockers are still real before treating an item as blocked.** A previous session may have silently completed the dependency without flipping the row. Grep for the deliverables (test names, env vars, code paths) before skipping.

## Flake fixes go first

When a CI flake is observed (test passes on rerun, no code change in between), file it as a Queue item **and move it to the top of the Queue** before continuing other work. Then pick it up next.

The reasoning: flake cost compounds. A 1-hour flake fix saves N hours of cumulative CI wait + diagnostic time + context-switch overhead across every future PR that hits it. Even a low-frequency flake (1-in-5 reruns) on a 10-minute job adds up to roughly half a wasted runner-hour per ten PRs, plus the human investigation time on each occurrence.

The convention overrides default Queue ordering even when other items are 🔴 critical security — security items are typically M/L-sized and will themselves benefit from flake-free CI during their multi-session work. Annotate the row's Notes with "**Top of queue per flakes-first rule**" with a link to this section so the priority is self-documenting.

Exceptions:
- A flake whose root cause is genuinely an outside-the-repo service (GitHub API outage, registry hiccup) and that has not recurred in many runs — file but don't bump.
- A flake whose fix is blocked on infrastructure not yet built (e.g. requires a CNI that the cluster doesn't have) — file, mark 🚫, and don't bump.

## Prioritize new items on entry

When you identify a new item, decide its priority **before** adding it — place it at the Queue position it deserves, not at the bottom by default. The Queue is read top-to-bottom ("Pick from the top"), so position *is* the priority signal. A row appended to the bottom is a row you've silently declared the lowest priority in the project; make that an explicit judgement, not a fallback you reach for to avoid deciding.

To place a new row, compare it against its prospective neighbours:

- **Severity / blast radius.** A correctness or security defect that can reach users outranks a cleanup or docs item. `bug`, `security`, and flake items generally sort high; `docs` and idiom-cleanup items generally sort low — but judge the specific item, not the label.
- **Leverage.** Work that unblocks other Queue items, or removes a recurring cost (see [flake fixes go first](#flake-fixes-go-first)), sorts above equal-severity work that doesn't.
- **Blocked vs. ready.** A new item blocked by an unlanded dependency goes *below* that dependency, marked 🚫 with `Blocked by [QN](#QN)` in its Notes (see [the cross-item blockers rule](#use-blocked-by-qnqn-for-cross-item-blockers)). Ready work sorts above blocked work of similar severity.
- **Size as a tiebreaker only.** Between two items of equal priority, a smaller (S) item that clears quickly may go first — but never let size override severity.

If you genuinely can't tell where it belongs, slot it next to the nearest comparable item rather than defaulting to the bottom, and note the reasoning in the commit message. Re-prioritizing later (moving existing rows) is a deliberate, separate STATUS.md commit — don't bundle a reshuffle with the addition of a new row.

## Deferred items live below the Queue, not in it

The Queue is for work with a priority position — things you'd pick from the top. An item that is intentionally parked — waiting on an explicit external trigger, with no near-term intent to act — does **not** belong in the priority ordering, where it would sit at the bottom collecting dust and diluting the signal that "bottom of Queue" means "lowest priority we still intend to do soon."

Such items move to the **Deferred** section below the Queue in `docs/STATUS.md`. A row belongs in Deferred when **all** of these hold:

- It has a concrete trigger condition that must fire first (a dependency that isn't on the Queue, a tool/cluster that doesn't exist yet, a usage threshold not yet hit).
- There is no near-term intent to do it — reviving it is conditional, not scheduled.
- It is still a real commitment (otherwise it's a non-commitment and belongs in [`appendix-g-future-enhancements.md`](../design/appendix-g-future-enhancements.md), not STATUS.md at all).

Mechanics:

- The Deferred table keeps each row's stable `Q`-prefixed ID and inline anchor, so cross-references (`[Q19](#Q19)`) keep resolving. IDs are not reused when an item moves sections.
- Its columns drop `St` (every row is implicitly 💤) and replace `Notes` with **`Trigger to revive`** — one phrase naming the condition that returns it to the Queue.
- **When a trigger fires, move the row back into the Queue at the position it then deserves** (see [Prioritize new items on entry](#prioritize-new-items-on-entry)) — don't just flip a status in place.
- This is the home for the three statuses that aren't actionable-now: a genuinely-parked item enters Deferred directly rather than being added to the Queue and immediately marked 💤. (A 🚫 *blocked-by-another-Queue-item* row stays in the Queue, below its blocker — see [the cross-item blockers rule](#use-blocked-by-qnqn-for-cross-item-blockers) — because it revives automatically the moment the blocker lands.)

## Format rules that exist to reduce churn

### `Last touched:` is one line, date only

The header line under "Conventions" is for the date of the most recent edit and nothing else:

```
Last touched: 2026-05-30
```

Do **not** append a session narrative, do **not** preserve prior entries with "Earlier: …", do **not** describe what changed. That information lives in:

- the commit message (for the most recent change),
- `git log docs/STATUS.md` (for the full history),
- the linked plan doc under `docs/plan/` (for design context).

A multi-paragraph `Last refreshed:` block is the single largest source of merge conflicts in this file. Every concurrent branch edits it, every session feels pressure to imitate the prior format and add their own entry. Resist.

### Queue `Notes` column: ≤2 sentences, hard cap 250 characters

`Notes` answers two questions only:

- **What is this item, in one sentence?** (often just a pointer: "→ M3/M4 kind end-to-end" for blocked items.)
- **What unblocks it or what's the next concrete step?**

**The 250-character cap is hard and lint-enforced** ([`scripts/lint-status.sh`](../../scripts/lint-status.sh) rule 3, run by the pre-commit hook and CI). Count before you commit — a markdown link counts its full `[text](url)` source length, so two short sentences with one link can blow the cap (this is the usual way it's exceeded). When you edit a Notes cell, check its length up front rather than waiting for the hook to reject the commit. The cap is configurable via the `NOTES_MAX_CHARS` env var but defaults to 250 everywhere.

Anything longer — dry-run write-ups, root-cause analyses, design rationale — belongs in the linked plan doc, not the row. Plan docs aren't high-contention; STATUS.md is.

If a row's Notes is growing past two sentences (or nears 250 chars), that's the signal to move the content to `docs/plan/<plan>.md` and replace the row Notes with a link.

### Don't use `▶ Started` markers for solo work

The `Maintaining this file` section in STATUS.md historically said to mark M/L items `▶ Started`. In practice:

- The open PR (visible to `gh pr list`) is the started signal, and the `gh pr list` check before picking already prevents double-starting.
- Marking ▶ Started adds one wasted isolated commit per task (one to mark started, one to delete the row on completion).
- The marker rots if a session is abandoned, requiring cleanup churn later.

Only set `▶ Started` if you have a specific reason to broadcast in-progress state beyond the open PR (e.g. an exploratory task with no PR yet, an item you've reserved but won't start for several days). Default to not setting it.

### Don't pre-assign release versions to backlog items

Do **not** tag Queue rows with speculative future release versions (`1.1`, `1.2`, `2.0`). Introduce a release label only once that release is *concretely scoped* — a plan doc defining its Definition of Done exists — at which point the label answers a real yes/no question ("does this block that tag?").

The reasoning:

- **It generates churn for no signal.** Post-release version estimates are guesses that move, and every re-estimate is another commit on this high-contention file. Same family as the banned sub-IDs and renumbering.
- **Position already encodes priority.** The Queue is read top-to-bottom; a version tag duplicates that ordering coarsely and can drift out of agreement with it.
- **Undefined versions anchor nothing.** A `1.1` tag with no `1.1` scope doc just asserts a roadmap that doesn't exist.

The "when" of an item is already partitioned without versions: a release label (e.g. `1.0-gate`, defined by [release-1.0.md](../plan/release-1.0.md)) marks what blocks that tag; an un-labelled Queue row is "after that release, priority = its position"; [Deferred](#deferred-items-live-below-the-queue-not-in-it) is "parked until a trigger"; [appendix-g](../design/appendix-g-future-enhancements.md) is "long-horizon non-commitment." The right pattern is the one `1.0-gate` followed: **scope the release in a plan doc first, then add the label** — not the reverse.

### Use `Blocked by [QN](#QN)` for cross-item blockers

When a 🚫 Queue row is blocked by another Queue item, start its Notes with `Blocked by [QN](#QN)` (or comma-separated for multiple). External dependencies that have no Queue ID — "needs a cluster with gVisor installed", a third-party sign-off — stay as plain prose.

The structured form pairs with `make queue-unblock ID=QN` (the `Q` prefix is optional — `ID=12` works too), which lists every row currently blocked on that ID. When the dependency lands, run it to enumerate dependents and clear them in a single isolated STATUS.md commit. Free-text "→ M5 packaging" notes are not machine-readable; `Blocked by [Q12](#Q12)` is.

### Stable IDs; do not reuse

Each Queue row has a `Q`-prefixed ID (e.g. `Q44`). Once assigned, it stays — even if the row is deleted. New rows take the next unused integer (continuing the same sequence). This makes cross-references in plan docs, commit messages, and PR descriptions stable.

The `Q` prefix exists so that references like `Q44` in a commit message or PR body are **not** auto-linked by GitHub to PR/issue 44 — `#NN` would be, `Q<N>` is not. Use the bare ID (`Q44`) in commits, PRs, and prose. Inside `docs/STATUS.md`, each row carries an inline anchor (`<a id="Q44"></a>Q44`), so cross-references between rows render as Markdown links: `[Q34](#Q34)`.

**Do not introduce sub-IDs (`5a`, `5b`, `5h`)** to track derivative work under a parent item. The 5a–5j sequence in early 2026 caused multiple "rewrite/renumber" churn commits. If a child task is discrete enough to track, give it its own top-level ID.

### Batch audit-discovery items in one commit

When a single review pass surfaces many new items (security audit → Q20–Q29, k8s audit → Q30–Q36, Go audit → Q37–Q41), add them all in one commit. One commit moving a contiguous block of rows is far easier to rebase than N commits each inserting one row.

The same applies to bulk completions: if a session verifies that a stale Queue entry was actually finished weeks ago, fold the deletion into the same commit as the verification work rather than splitting.

## When to update the Progress table vs. the Queue

- **Progress table** (plan-level rows): updated only when a plan's overall status changes — ⚠️ becomes ✅, a brand-new ⚠️ row appears, or a plan doc is added/retired. Edits here are rare.
- **Queue**: updated whenever a specific item is started, completed, blocked, unblocked, or newly identified. Most STATUS.md commits touch only the Queue.

If you completed work that closes the last ⚠️ open item under a Progress row, update both in the same commit.

### `⚠️` means an open *Queue* row remains — deferred residuals don't count

A plan is `⚠️` only while it has at least one open row **in the Queue**. Intentionally-deferred residuals live in the [Deferred](#deferred-items-live-below-the-queue-not-in-it) section (or, for non-commitments, in [appendix-g](../design/appendix-g-future-enhancements.md)), and they **do not keep a plan `⚠️`**. A plan whose only remainders are Deferred rows or accepted-by-design residuals is `✅`, not `⚠️`.

This keeps the Progress table honest: `⚠️` reads as "active work remains on this plan," not "a box was once left unchecked." Leaving a finished-but-for-deferrals plan at `⚠️` makes old, intentionally-parked work look like an open obligation.

When you flip a plan to `✅`, add (or update) a **Status** banner at the top of its plan doc that names the Deferred IDs carrying its residuals (e.g. "Status: Complete — residuals deferred as [Q11](../STATUS.md#Q11)"). That makes the deferral auditable from the plan itself, and explains the `✅` to anyone who notices the plan still lists open-sounding items in its body. The plan doc is **not** archived in this case — it stays in `docs/plan/` because its `✅` Progress row still references it (archival is only for plans no longer referenced anywhere; see [Archiving completed plan docs](#archiving-completed-plan-docs)).

## Archiving completed plan docs

When a plan's work fully lands and `docs/STATUS.md` no longer references it (no Progress row, no Queue/Deferred row), move the doc under `docs/plan/archive/` rather than deleting it. The rationale is usually more valuable than the diff, but a fully-closed plan in the top level of `docs/plan/` is noise for the next session scanning for active work.

**Archive on close, not on audit.** Do this in the same body of work that removes the plan's last `STATUS.md` reference — the moment you delete its final Queue row, or flip its Progress row to `✅` with nothing left open. Closed plans left in place pile up and make the index read as though finished projects still have work — the exact drift this rule exists to prevent. Two gates (both in `make check`) enforce it so the omission can't ship silently:
- **`make plan-index-check`** fails when an active, non-ⓘ plan listed in `docs/plan/README.md` is no longer referenced by `STATUS.md` — i.e. a plan that should have been archived. To clear it: archive the plan (below), or, if it's ongoing spec/strategy/research, mark its README row `ⓘ`.
- **`make doc-links`** fails on any broken link the move introduces.

The same change should also keep the plan's `docs/plan/README.md` **status text** current: when you delete a Queue row that completes a plan, update that plan's README row in the same edit (don't wait for someone to notice it citing a since-completed `QNN`).

**Keep archival a docs-only operation.** Archival must never touch code — a code edit re-triggers the heavy path-gated CI (e2e / integration / trivy) on what should be a `docs/**`-only move. The way to guarantee that: **code never references a plan by path.** A Go comment must not contain `docs/plan/<file>.md` (or `../plan/<file>.md`); cite the durable layer instead — a `docs/design/` or `docs/operations/` doc, or a stable `Q-ID` / appendix `§`-ref (those survive archival untouched, since IDs are never reused and design sections don't move on plan close). If a plan's conclusion is load-bearing enough that code wants to cite it, **promote that conclusion to a durable doc when the plan closes** (the [doc-update matrix](doc-update-matrix.md) already requires the design/operations update on the code change); the plan keeps the full derivation as history, and code points at the durable home. `make no-plan-refs-check` (in `make check`) fails on any `docs/plan/` path in a `.go` file, so the coupling can't re-accrete. Prose mentions of a plan's *content* ("Milestone 1 §8", "the worker-egress-proxy plan") are fine — only file *paths* rot.

**Protocol:**

1. **Confirm STATUS.md doesn't reference the doc.** `grep -n "<docname>" docs/STATUS.md` should be empty.
2. **Confirm the work actually landed.** Read the plan's Status banner if it has one; otherwise grep the codebase for the named tests, types, or behaviors the plan promised. A plan with one of three fixes still ❌ Open is **not** archive-ready — leave it in place and (if not already there) add the open work to STATUS.md as a Queue row.
3. `git mv docs/plan/<docname>.md docs/plan/archive/<docname>.md` — preserves history.
4. **Update any in-repo links** to the new path. Likely candidates:
   - `docs/plan/README.md` — move the doc's row from its current section into the **Archive** section and update the status text.
   - Other plan docs cross-referencing it (`grep -rn "<docname>.md" docs/plan/`).
   - `docs/development/`, `docs/design/`, `docs/operations/` if the plan is cited there.
   - Code comments (rare, but worth checking with `grep -rn "<docname>" --include="*.go"`). Prose mentions (e.g. "see foo Theme E") don't break; only `](…<docname>.md…)` *links* need rewriting.
   - **The moved doc's *own* outbound links** — dropping a level into `archive/` breaks every relative link in the doc itself: each `](../…)` needs one more `../`, and a bare same-dir link to a doc still in `plan/` becomes `](../name.md)`. Easy to miss; `make doc-links` catches it.
5. **Bundle archival in one commit.** If multiple plans are being archived in the same session (e.g. after a sweep), move them together — easier to review and to revert as a unit if a reference was missed.
6. **Do not edit STATUS.md in the same commit** as the archive move. STATUS.md edits are always isolated (see §1 of the non-negotiables).

A plan that is partially complete should stay in `docs/plan/`. Archive is for "everything in this doc has shipped," not "most of it has."

## Anti-patterns to watch for

- **Narrating recent session work in the conventions header.** That's what commit messages are for.
- **Carrying root-cause writeups in Queue Notes.** That's what plan docs are for.
- **Splitting bulk discovery into many one-row commits.** That maximizes rebase pain.
- **Renumbering existing IDs to "tidy up".** IDs are pointers; renumbering invalidates every external reference.
- **Pre-assigning future release versions to items.** A version tag with no scoped release behind it is churn without signal — [scope the release first, then label](#dont-pre-assign-release-versions-to-backlog-items).
- **Editing STATUS.md alongside a code change.** Conflicts on the code commit cascade into the STATUS.md edit. Always a separate commit.
