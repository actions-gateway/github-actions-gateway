# Documentation standards

The canonical home for **how we write and maintain docs** — the goals, the style,
the conventions, and the upkeep. It complements two neighbours rather than
repeating them:

- [doc-update-matrix.md](doc-update-matrix.md) — *which* docs to update for each kind of change.
- [maintaining-backlog.md](maintaining-backlog.md) — rules specific to [STATUS.md](../STATUS.md).

The bar is **correct and findable first, usable as well** — not polish in place of
substance. A beautifully scannable doc that is wrong, missing, or the wrong type for
the reader's task still fails. The goal hierarchy below makes that ordering explicit;
the rest of the page is how we hit each level.

## The docset in one paragraph

`docs/` is plain GitHub-native Markdown — no MkDocs front matter, no transclusion, no
versioned-docs tree (a [deliberate choice](../plan/docs-six-layer-audit.md): renders on
GitHub, git is the single source of truth). The taxonomy is the per-directory
`README.md` index. There are two audiences: `docs/design/` (how the system works, for
contributors) and `docs/operations/` (what an operator does and sees). A change that
alters operator-visible behaviour must update the operations docs too — design-only is
the classic miss.

The docs also serve **two kinds of reader**: humans, and the AI agents that build on
this repo. They want compatible things — agents reward deterministic, greppable,
single-canonical-home content; humans reward narrative and onboarding — but optimise
for one blindly and you degrade the other. Most rules here serve both; where they
diverge, the doc says so.

## Goals: what good looks like

A doc-quality goal is wasted if the one above it fails — you can't scan your way out of
a wrong or missing answer. In rough order of leverage:

1. **Correct & current.** The doc matches the code, today. A stale doc is *worse* than
   none: it misleads and erodes trust in the whole set. Highest leverage, hardest to
   sustain — see [Maintenance](#maintenance).
2. **Findable.** The reader lands on the right page fast: entry points, `README.md`
   indexes, cross-links, the [glossary](../design/08-glossary.md). A perfect doc nobody
   finds is zero docs.
3. **Complete enough.** The questions readers actually have are answered; no silent
   gaps. Coverage, not exhaustiveness.
4. **Fit-for-purpose.** Right *type* (tutorial / how-to / reference / explanation) and
   right *altitude* (operator vs contributor). A reference dump when someone needs a
   five-step how-to fails regardless of formatting.
5. **Usable.** Scannable, copy-paste-safe, free of filler — see [Write for
   scanning](#write-for-scanning) and [Commands and code blocks](#commands-and-code-blocks).
   Necessary, cheap to get wrong, but not a substitute for 1–4.
6. **Trustworthy in tone.** Honest about limitations, failure modes, and "not yet
   implemented." Trust is what makes a reader rely on 1–5.

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

Goal 1 — *correct & current* — is the hardest to sustain because docs rot silently as
the code moves. Keep them current:

- **Update docs in the same change.** After a behaviour change, update every doc it
  touches before opening the PR — the change-type → docs map is the
  [doc-update-matrix](doc-update-matrix.md). Design-doc updates alone are not enough when
  a change alters what an operator does, configures, or observes.
- **Keep each `README.md` index complete.** A new doc gets a row in its directory's
  `README.md` index in the same change (a goal-2 *findability* failure otherwise).
- **Archive finished plans.** When a plan's last STATUS reference is removed, update its
  [`docs/plan/README.md`](../plan/README.md) row and archive the plan in the same change
  — `make plan-index-check` enforces this. See
  [maintaining-backlog.md](maintaining-backlog.md#archiving-completed-plan-docs).
- **STATUS.md gets its own commit.** It is high-contention; isolating its changes keeps
  rebases trivial. Queue Notes have a hard 250-char cap (lint-enforced). Details:
  [maintaining-backlog.md](maintaining-backlog.md).
- **Verify links render.** In-page anchors and cross-file links are not yet checked by
  CI; confirm them against the rendered page before merging.

## Measuring doc quality

"Best docs" implies a way to *know* — quality you can observe, not just assert. What
exists today, and what's proposed:

| Signal | Goal | Status |
|---|---|---|
| `make plan-index-check` — every plan doc is indexed/archived | 2 | Wired (`make check`). |
| STATUS.md lint — Queue shape, 250-char Note cap | 1 | Wired (`make check`). |
| Per-change doc updates via the [doc-update-matrix](doc-update-matrix.md) | 1 | Convention, enforced in review (PR self-check). |
| Link/anchor checker in CI | 1, 2 | **Proposed** — flagged open by the [six-layer audit](../plan/docs-six-layer-audit.md). The only automated guard against link rot. |
| Periodic docs-vs-code drift audit | 1 | **Proposed** — a recurring backstop for what the per-change rule misses. |
| Reader questions (issues, support threads) logged as coverage gaps | 3 | **Proposed** — turns real confusion into Queue items. |

Treat the proposed rows as the roadmap for making quality observable; the cheapest and
highest-value is the link checker.

## Authoring checklist

Before opening a docs PR, check against the goals — not just formatting:

- [ ] **Correct & current (1):** matches the code today; every doc the change touches is
      updated per the [doc-update-matrix](doc-update-matrix.md).
- [ ] **Findable (2):** linked from its directory `README.md` index; cross-links and
      anchors verified against the rendered page; no orphan.
- [ ] **Complete (3):** answers the question a reader arrives with; no silent gap or
      undocumented failure mode.
- [ ] **Fit-for-purpose (4):** right type and altitude for its audience
      (operator vs contributor).
- [ ] **Usable (5):** answer-first; enumerations are lists and comparisons are tables;
      code/command blocks are copy-paste-runnable with consistent placeholders; no walls
      of text or filler.
- [ ] **Trustworthy (6):** honest about limitations and "not yet implemented"; acronyms
      expanded on first use; terms match the glossary; no links to `CLAUDE.md`.
