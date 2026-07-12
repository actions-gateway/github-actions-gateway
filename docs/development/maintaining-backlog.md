# Agent reference: Maintaining the backlog

`docs/STATUS.md` is the single source of truth for project progress and priorities. It is high-contention — almost every session edits it — so keeping churn low matters as much as keeping it accurate.

The format and process come from the globally-installed **backlog skill** (agents: invoke the `backlog` skill for the full playbook — grooming checklist, staleness signals, parallel dispatch, migration). The repo vendors the skill's tooling so the rules hold for every contributor, with or without the skill:

- [`scripts/lint-backlog.sh`](../../scripts/lint-backlog.sh) — enforces every format rule below; its header comment is the canonical rule list. Runs in `make check` (`make lint-backlog`), CI ([`status-lint.yml`](../../.github/workflows/status-lint.yml) and `unit-test.yml`), and the pre-commit hook. The hook's `--staged` mode also rejects any commit that stages `docs/STATUS.md` alongside other files.
- [`scripts/next-task.sh`](../../scripts/next-task.sh) — prints a kickoff prompt (or `--title`) for the top ready 🔲 Queue row, for starting a fresh session on the next task.
- [`scripts/backlog-metrics.sh`](../../scripts/backlog-metrics.sh) — replays the file's git history into flow metrics (throughput, cycle time, prune ratio, aging WIP). Read-only.

## The shared process, in brief

- **Position is priority.** The Queue is read top-to-bottom; pick from the top. Decide a new item's priority *before* inserting it and place the row where it belongs — never append by default. Rank by severity/blast radius, then leverage (what it unblocks), ready over blocked; size only as a tiebreaker.
- **Two Queue states only: 🔲 ready and 🚫 blocked.** Done rows are **deleted** (git is the archive), "started" is signaled by the open PR (run `gh pr list` before picking; skip rows an open PR covers), and parked rows live in the Deferred table.
- **Verify 🚫 blockers before treating a row as blocked** — a prior session may have shipped the dependency without flipping the row; grep for its deliverables. Cross-item blockers are machine-readable: a 🚫 row's Notes start with `Blocked by [QN](#QN)`, and `make queue-unblock ID=QN` lists every dependent when the blocker lands.
- **The `**Next ID:**` counter is the allocator.** Take it for a new row, bump it in the same edit. IDs are stable, never reused or renumbered, and never get sub-IDs (`5a`) — a trackable child gets its own top-level ID. The `Q` prefix keeps `Q44` from auto-linking to PR/issue #44; use the bare ID in commits and PRs.
- **Notes are present tense, ≤ 250 chars (hard cap); past 200 chars the row must link a doc** from its Item or Notes cell — a `#QN` sibling anchor doesn't count, since sibling rows are capped too. No merged-PR lists or "SHIPPED" narration — history lives in `git log` and the plan doc. The same caps apply to Deferred trigger cells. Write for a skimmer: cut detail and link a doc rather than compressing into fragments.
- **Deferred rows carry a concrete revive trigger**, tagged by source: `**Demand:**` (an outside ask) · `**Event:**` (an observable outside-our-control condition) · `**Decision:**` (our own call — grep `**Decision:**` for what we could move on unilaterally). When the trigger fires, move the row back into the Queue at the position it then deserves. A non-commitment belongs in [appendix-g](../design/appendix-g-future-enhancements.md), not Deferred.
- **`docs/STATUS.md` edits are isolated commits** — never mixed with code or plan-doc changes, even when completing an item mid-feature (the pre-commit hook enforces this). Use `docs(status):` subjects, and name the removal reason with a fixed verb — `complete QN`, `prune QN`, `merge QN into QM`, `defer QN` — so metrics can tell throughput from garbage collection. Batch bulk additions (one audit's discoveries) into one commit; keep reshuffles separate from additions.
- **M/L items get a plan doc** under `docs/plan/`, linked from the Item cell.

## When the context doesn't fit, write the doc — whatever the item's size

The trigger for writing a doc is **information loss, not item size**: `Sz` estimates effort, not how much context the work rests on. If fitting the caps means dropping a decision the work depends on, an investigation finding a future session would re-derive, or a blocker's rationale — write (or extend) the doc and link it. Compressing prose is fine; dropping a clause because it doesn't fit is the signal. The content picks the home:

| Kind of context | Home | Why |
|---|---|---|
| **Durable rationale** — decisions, security governance, why a default is what it is | `docs/design/` | Survives plan archival; still there in two years. |
| **In-flight work context** — findings, phases, what's left | `docs/plan/<qNNN>-<slug>.md` | Archived on close (below). |

When a plan closes, **promote its load-bearing conclusions into `docs/design/`** rather than letting them archive out of reach — Queue rows and code cite the durable layer, never a plan path.

## Flake fixes go first

When a CI flake is observed (test passes on rerun, no code change in between), file it as a Queue item **and move it to the top of the Queue** before continuing other work. Then pick it up next. Flake cost compounds: a 1-hour fix saves cumulative CI wait + diagnosis + context-switch overhead across every future PR that hits it. This overrides default ordering even over critical security items — those are typically M/L-sized and themselves benefit from flake-free CI. Annotate the row's Notes with "**Top of queue per flakes-first rule**" linking this section.

Exceptions: a flake rooted in an outside service that hasn't recurred (file, don't bump); a flake whose fix is blocked on infrastructure that doesn't exist yet (file, mark 🚫, don't bump).

**Once the mitigation ships, move the row to [Flake watch](../STATUS.md#flake-watch)** — a Deferred subsection whose revive mechanic differs from the rest of the table: the trigger is always `**Event:** recurs on main after the fix`, and on recurrence the row returns to the **top** of the Queue, escalated (the first mitigation didn't hold). Keeping the row (rather than closing it) preserves the memory that a fix was already attempted, so a second occurrence reads as a recurrence, not a fresh find. The lifecycle:

- **Observed, unfixed** → Queue top (flakes-first); pick next.
- **Mitigation shipped, not recurred** → Deferred § Flake watch.
- **Recurs** → back to Queue top, escalated.

## The Progress table

`docs/STATUS.md` keeps a plan-level **Progress** table above the Queue — one row per plan doc. Update it only when a plan's overall status changes (⚠️ → ✅, a new plan lands, a plan retires); most STATUS.md commits touch only the Queue. If completing a Queue row closes the last open item under a Progress row, update both in the same commit.

When you remove a Queue row for a **shipped user-facing capability**, also check whether it graduates a bullet on the website [roadmap.md](../roadmap.md) — an "In progress / near-term" item moving to "Available now (1.0)" — and state its true maturity (GA vs. alpha) so the roadmap doesn't overclaim. The roadmap is hand-maintained; nothing else catches the drift.

### `⚠️` means an open *Queue* row remains — deferred residuals don't count

A plan is `⚠️` only while it has at least one open row **in the Queue**. Intentionally-deferred residuals live in Deferred (or, for non-commitments, in [appendix-g](../design/appendix-g-future-enhancements.md)) and do **not** keep a plan `⚠️`: a plan whose only remainders are Deferred rows is `✅`. This keeps the table honest — `⚠️` reads as "active work remains," not "a box was once left unchecked."

When you flip a plan to `✅`, add (or update) a **Status** banner at the top of its plan doc naming the Deferred IDs carrying its residuals (e.g. "Status: Complete — residuals deferred as [Q11](../STATUS.md#Q11)"). The plan doc is **not** archived in this case — its `✅` Progress row still references it.

## Don't pre-assign release versions to backlog items

Do **not** tag Queue rows with speculative future release versions (`1.1`, `2.0`). Introduce a release label only once that release is *concretely scoped* — a plan doc defining its Definition of Done exists — at which point the label answers a real yes/no question ("does this block that tag?"). Post-release estimates are guesses that move (churn without signal), position already encodes priority, and an undefined version anchors nothing. The right pattern is the one `1.0-gate` followed: scope the release in a plan doc first, then add the label.

## Archiving completed plan docs

When a plan's work fully lands and `docs/STATUS.md` no longer references it (no Progress row, no Queue/Deferred row), move the doc under `docs/plan/archive/` rather than deleting it. The rationale is usually more valuable than the diff, but a fully-closed plan in the top level of `docs/plan/` is noise for the next session scanning for active work.

**Archive on close, not on audit.** Do this in the same body of work that removes the plan's last `STATUS.md` reference — the moment you delete its final Queue row, or flip its Progress row to `✅` with nothing left open. Two gates (both in `make check`) enforce it so the omission can't ship silently:

- **`make plan-index-check`** fails when an active, non-ⓘ plan listed in `docs/plan/README.md` is no longer referenced by `STATUS.md` — i.e. a plan that should have been archived. To clear it: archive the plan (below), or, if it's ongoing spec/strategy/research, mark its README row `ⓘ`.
- **`make doc-links`** fails on any broken link the move introduces.

The same change should also keep the plan's `docs/plan/README.md` **status text** current: when you delete a Queue row that completes a plan, update that plan's README row in the same edit.

**Keep archival a docs-only operation.** Archival must never touch code — a code edit re-triggers the heavy path-gated CI (e2e / integration / trivy) on what should be a `docs/**`-only move. The way to guarantee that: **code never references a plan by path.** A Go comment must not contain `docs/plan/<file>.md`; cite the durable layer instead — a `docs/design/` or `docs/operations/` doc, or a stable `Q-ID` / appendix `§`-ref (those survive archival untouched). If a plan's conclusion is load-bearing enough that code wants to cite it, promote that conclusion to a durable doc when the plan closes. `make no-plan-refs-check` (in `make check`) fails on any `docs/plan/` path in a `.go` file. Prose mentions of a plan's *content* ("Milestone 1 §8") are fine — only file *paths* rot.

**Protocol:**

1. **Confirm STATUS.md doesn't reference the doc.** `grep -n "<docname>" docs/STATUS.md` should be empty.
2. **Confirm the work actually landed.** Read the plan's Status banner if it has one; otherwise grep the codebase for the named tests, types, or behaviors the plan promised. A plan with open work is **not** archive-ready — leave it in place and make sure the open work has a Queue row.
3. `git mv docs/plan/<docname>.md docs/plan/archive/<docname>.md` — preserves history.
4. **Update any in-repo links** to the new path: `docs/plan/README.md` (move the row to the **Archive** section), other plan docs (`grep -rn "<docname>.md" docs/plan/`), the `docs/development|design|operations` trees, and **the moved doc's own outbound links** — dropping a level into `archive/` breaks every relative link in the doc itself (`make doc-links` catches all of these).
5. **Bundle archival in one commit** when several plans close in the same session — easier to review and revert as a unit.
6. **Do not edit STATUS.md in the same commit** as the archive move — STATUS.md edits are always isolated.

A plan that is partially complete stays in `docs/plan/`. Archive is for "everything in this doc has shipped," not "most of it has."
