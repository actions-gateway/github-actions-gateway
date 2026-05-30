# Agent reference: Maintaining the backlog

`docs/STATUS.md` is the single source of truth for project progress and priorities. It is high-contention — almost every session edits it — so keeping churn low matters as much as keeping it accurate. This doc captures the rules that keep merge conflicts trivial and the file readable.

## The non-negotiables

1. **Isolate `docs/STATUS.md` edits in their own commit**, separate from code and plan-doc changes. Rebase conflicts on STATUS.md should always be resolvable by `git checkout --theirs` or `--ours` on a single file. This is also stated in `CLAUDE.md` and is the highest-leverage rule in this doc.
2. **Run `gh pr list` before picking a task.** A Queue row already covered by an open PR should be skipped, not re-started. The open PR — not a `▶ Started` marker — is the real "in-flight" signal.
3. **Verify 🚫 blockers are still real before treating an item as blocked.** A previous session may have silently completed the dependency without flipping the row. Grep for the deliverables (test names, env vars, code paths) before skipping.

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

### Queue `Notes` column: ≤2 sentences

`Notes` answers two questions only:

- **What is this item, in one sentence?** (often just a pointer: "→ M3/M4 kind end-to-end" for blocked items.)
- **What unblocks it or what's the next concrete step?**

Anything longer — dry-run write-ups, root-cause analyses, design rationale — belongs in the linked plan doc, not the row. Plan docs aren't high-contention; STATUS.md is.

If a row's Notes is growing past two sentences, that's the signal to move the content to `docs/plan/<plan>.md` and replace the row Notes with a link.

### Don't use `▶ Started` markers for solo work

The `Maintaining this file` section in STATUS.md historically said to mark M/L items `▶ Started`. In practice:

- The open PR (visible to `gh pr list`) is the started signal, and the `gh pr list` check before picking already prevents double-starting.
- Marking ▶ Started adds one wasted isolated commit per task (one to mark started, one to delete the row on completion).
- The marker rots if a session is abandoned, requiring cleanup churn later.

Only set `▶ Started` if you have a specific reason to broadcast in-progress state beyond the open PR (e.g. an exploratory task with no PR yet, an item you've reserved but won't start for several days). Default to not setting it.

### Stable IDs; do not reuse

Each Queue row has a numeric ID. Once assigned, it stays — even if the row is deleted. New rows take the next unused integer. This makes cross-references in plan docs, commit messages, and PR descriptions stable.

**Do not introduce sub-IDs (`5a`, `5b`, `5h`)** to track derivative work under a parent item. The 5a–5j sequence in early 2026 caused multiple "rewrite/renumber" churn commits. If a child task is discrete enough to track, give it its own top-level ID.

### Batch audit-discovery items in one commit

When a single review pass surfaces many new items (security audit → #20–29, k8s audit → #30–36, Go audit → #37–41), add them all in one commit. One commit moving a contiguous block of rows is far easier to rebase than N commits each inserting one row.

The same applies to bulk completions: if a session verifies that a stale Queue entry was actually finished weeks ago, fold the deletion into the same commit as the verification work rather than splitting.

## When to update the Progress table vs. the Queue

- **Progress table** (plan-level rows): updated only when a plan's overall status changes — ⚠️ becomes ✅, a brand-new ⚠️ row appears, or a plan doc is added/retired. Edits here are rare.
- **Queue**: updated whenever a specific item is started, completed, blocked, unblocked, or newly identified. Most STATUS.md commits touch only the Queue.

If you completed work that closes the last ⚠️ open item under a Progress row, update both in the same commit.

## Archiving completed plan docs

When a plan's work fully lands and `docs/STATUS.md` no longer references it (no Progress row, no Queue row), move the doc under `docs/plan/archive/` rather than deleting it. The rationale is usually more valuable than the diff, but a fully-closed plan in the top level of `docs/plan/` is noise for the next session scanning for active work.

**Protocol:**

1. **Confirm STATUS.md doesn't reference the doc.** `grep -n "<docname>" docs/STATUS.md` should be empty.
2. **Confirm the work actually landed.** Read the plan's Status banner if it has one; otherwise grep the codebase for the named tests, types, or behaviors the plan promised. A plan with one of three fixes still ❌ Open is **not** archive-ready — leave it in place and (if not already there) add the open work to STATUS.md as a Queue row.
3. `git mv docs/plan/<docname>.md docs/plan/archive/<docname>.md` — preserves history.
4. **Update any in-repo links** to the new path. Likely candidates:
   - `docs/plan/README.md` — move the doc's row from its current section into the **Archive** section and update the status text.
   - Other plan docs cross-referencing it (`grep -rn "<docname>.md" docs/plan/`).
   - `docs/development/`, `docs/design/`, `docs/operations/` if the plan is cited there.
   - Code comments (rare, but worth checking with `grep -rn "<docname>" --include="*.go"`).
5. **Bundle archival in one commit.** If multiple plans are being archived in the same session (e.g. after a sweep), move them together — easier to review and to revert as a unit if a reference was missed.
6. **Do not edit STATUS.md in the same commit** as the archive move. STATUS.md edits are always isolated (see §1 of the non-negotiables).

A plan that is partially complete should stay in `docs/plan/`. Archive is for "everything in this doc has shipped," not "most of it has."

## Anti-patterns to watch for

- **Narrating recent session work in the conventions header.** That's what commit messages are for.
- **Carrying root-cause writeups in Queue Notes.** That's what plan docs are for.
- **Splitting bulk discovery into many one-row commits.** That maximizes rebase pain.
- **Renumbering existing IDs to "tidy up".** IDs are pointers; renumbering invalidates every external reference.
- **Editing STATUS.md alongside a code change.** Conflicts on the code commit cascade into the STATUS.md edit. Always a separate commit.
