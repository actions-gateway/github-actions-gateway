# Documentation standards

The canonical home for **how we write and maintain docs** — style, conventions,
and upkeep. It complements two neighbours rather than repeating them:

- [doc-update-matrix.md](doc-update-matrix.md) — *which* docs to update for each kind of change.
- [maintaining-backlog.md](maintaining-backlog.md) — rules specific to [STATUS.md](../STATUS.md).

This page is *what good looks like* for any prose doc under `docs/`, `README.md`, or
`CONTRIBUTING.md`. The bar is usability without verbosity: a reader should find the
answer by scanning, and copy a command without editing it.

## The docset in one paragraph

`docs/` is plain GitHub-native Markdown — no MkDocs front matter, no transclusion, no
versioned-docs tree (a [deliberate choice](../plan/docs-six-layer-audit.md): renders on
GitHub, git is the single source of truth). The taxonomy is the per-directory
`README.md` index. There are two audiences: `docs/design/` (how the system works, for
contributors) and `docs/operations/` (what an operator does and sees). A change that
alters operator-visible behaviour must update the operations docs too — design-only is
the classic miss.

## Write for scanning

A reader skims headings, then the first line of each block. Optimise for that.

- **Answer first (inverted pyramid).** Put the conclusion in the heading, the first
  sentence of the paragraph, and the first item of the list. Don't make the reader
  reach the end to get unstuck.
- **Self-describing headings.** A heading states the task or answer ("Verify the heavy
  gates ran"), not a bare topic ("Gates"). Someone reading only the headings should
  understand the page.
- **One idea per paragraph.** Keep paragraphs to ~4–5 lines. Split when a second idea
  starts.
- **Lists for any enumeration.** Steps, options, conditions, and requirements become
  ordered/unordered lists — never a comma-strung sentence. This *cuts* words.
- **Tables for comparisons and mappings.** Anything shaped "for X do Y" or "A vs B" is
  a table, not parallel paragraphs.
- **Front-load list items.** Start each bullet with the distinguishing word, bolded
  (`**Worktree paths** — …`), not boilerplate ("When you are in a worktree, …").
- **Bold sparingly.** Bold the one keyword a scanner hunts for. If everything is bold,
  nothing is.

## Commands and code blocks

A reader copies a block and runs it. Make that safe.

- **Copyable blocks run as-is.** No `$`/`#` prompt prefixes on copyable lines, no
  command-output interleaved in the same block, no leading line numbers.
- **One runnable command per intent.** Give the whole working line once; don't make the
  reader assemble it from three scattered snippets.
- **Obvious, consistent placeholders.** Use one convention for substitutables
  (`<UPPER_SNAKE>` or `${VAR}`) so the reader knows exactly what to replace. Never let a
  real value masquerade as a placeholder, or vice versa.
- **No ellipses in copyable code.** If a sample is incomplete, mark the omission with a
  language-valid comment (`# … rest of config`) and don't present it as copy-to-run — an
  incomplete block that looks runnable is a trap.
- **Introduce, then show.** A one-line lead-in ending in a colon, then the block. Put
  explanation *before* the block, not as a wall of text after it.
- Shell snippets follow the repo [bash conventions](bash-style.md).

## Conventions

| Convention | Rule |
|---|---|
| **Acronyms** | Spell out on first use, then the acronym in parentheses — "Actions Gateway Controller (AGC)". Subsequent uses may use the acronym alone. |
| **Terminology** | One term per concept across all docs. Canonical definitions live in the [glossary](../design/08-glossary.md); link there rather than redefining. |
| **Diagrams** | Prefer ASCII box-art over Mermaid unless auto-layout is a quantifiable win. ASCII renders everywhere and diffs cleanly. |
| **Canonical home + link** | State a fact once, in its natural home, and link to it. Don't restate — copies drift. (Same rule for reuse, since GitHub has no transclusion.) |
| **No links to `CLAUDE.md`** | `CLAUDE.md`/`AGENTS.md` is the agent entrypoint only. Human docs never link to it; the dependency direction is one-way (`CLAUDE.md` → `docs/`). Content humans need lives in `docs/` or `CONTRIBUTING.md`. |
| **Table of contents** | Long docs (~400+ lines) carry a `## Table of Contents` after the intro listing h2s (plus h3 for operator docs). Anchors follow GitHub slug rules (duplicate headings get `-1`/`-2`); verify against the rendered page. |
| **Cut filler** | Delete "in order to", "it should be noted that", "please note", and hedging preambles. A pure win for brevity and scannability. |

## Maintenance

Docs rot when they drift from the code or pile up after a task. Keep them current:

- **Update docs in the same change.** After a behaviour change, update every doc it
  touches before opening the PR — the change-type → docs map is the
  [doc-update-matrix](doc-update-matrix.md). Design-doc updates alone are not enough when
  a change alters what an operator does, configures, or observes.
- **Keep each `README.md` index complete.** A new doc gets a row in its directory's
  `README.md` index in the same change.
- **Archive finished plans.** When a plan's last STATUS reference is removed, update its
  [`docs/plan/README.md`](../plan/README.md) row and archive the plan in the same change
  — `make plan-index-check` enforces this. See
  [maintaining-backlog.md](maintaining-backlog.md#archiving-completed-plan-docs).
- **STATUS.md gets its own commit.** It is high-contention; isolating its changes keeps
  rebases trivial. Queue Notes have a hard 250-char cap (lint-enforced). Details:
  [maintaining-backlog.md](maintaining-backlog.md).
- **Verify links render.** In-page anchors and cross-file links are not checked by CI;
  confirm them against the rendered page before merging.

## Authoring checklist

Before opening a docs PR:

- [ ] Answer-first: headings and opening sentences carry the conclusion.
- [ ] Enumerations are lists; comparisons/mappings are tables; no walls of text.
- [ ] Code/command blocks are copy-paste-runnable with consistent placeholders.
- [ ] Acronyms expanded on first use; terms match the glossary.
- [ ] Stated once and linked — no duplicated content; no links to `CLAUDE.md`.
- [ ] Every doc the change touches is updated (per the doc-update-matrix), and the
      directory `README.md` index includes any new doc.
- [ ] Links and anchors verified against the rendered page.
