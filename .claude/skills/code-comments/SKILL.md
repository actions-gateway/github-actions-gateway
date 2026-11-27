---
name: code-comments
description: Write, edit, and review code comments so each one is short and says only what a later reader of the file needs. Use any time a comment is added or changed in source — a `//` or `#` comment, a godoc or docstring, a script or file header, a workflow or Makefile comment, a TODO — and before committing a diff that contains one. Also when a review says code comments are "excessively long", "too long", or "too many comments", asks whether something needs "to be in the code comment or better for the PR", says a comment's content should be in docs, or asks to "rewrite the comment" and keep it short. Covers the keep-or-move test, where history, rationale, measurements, and backlog IDs go instead (commit message, PR description, docs/), length limits, and rules for Go, Bash, Python, workflow YAML, Helm values, and Makefiles. Neighbours — code-restraint owns code shape and the general comment rules, and defers to this skill on this repo's venues, limits, and backlog IDs; documentation-standards.md owns prose docs.
---

# Code comments

[`docs/development/code-comments.md`](../../../docs/development/code-comments.md) holds the reasons, the sources, and the measurements.
This is the rule set, complete enough to apply without opening it.

## The test

The reader is someone changing this file later with the code in front of them and nothing else: no PR, no backlog, no memory of this session.
**Keep a sentence only if that reader needs it to read the code correctly or change it safely.** Run the test per sentence, not per comment.

A sentence passes when it gives one of:

- why the code is this way, where the code cannot say it: the constraint met, the failure avoided
- precision the code cannot hold: units, inclusive or exclusive, what `nil` or empty means, lock held, call order
- a trap: code that looks wrong or simplifiable and is not, and what breaks
- the contract, in a doc comment: what a caller relies on and must provide
- an upstream-behavior claim, with the measured version in a parenthetical

A sentence that restates the line under it fails.
Delete it, or rename the thing.

## Move, don't delete

A sentence that fails the test usually still has a home.
Put it there in the same change, so cutting the comment loses nothing.

| In the draft comment | Goes to |
|---|---|
| "previously", "no longer", "now", "used to", what changed | commit message |
| why this approach beat another; "deliberately"; "the obvious fix was rejected" | PR description, or `docs/design/` if it outlives the PR |
| the incident, flake, or review behind the code, and what it cost | commit message and PR description |
| measurements, run counts, dates | PR description, or `docs/` if a later decision rests on them |
| `Q123` or `#123` for finished work | commit message and PR description |
| a defense against a reviewer reverting it | the PR thread |
| how the subsystem fits together | `docs/design/`, with a one-line `See docs/design/…` in the comment |
| what an operator sees, sets, or does | `docs/operations/` |

**No backlog ID or PR number in a comment**, with one exception: `TODO(Q123):` for open work.
A finished row is deleted from `docs/queue/`, so its ID points at nothing; `git blame` already leads to the commit, the PR, and the item.

## Length

- **Implementation comment: one or two lines.** At four, re-run the test on each sentence.
  Past that the content is a doc: write the doc and link it.
- **Doc comment: as long as the contract**, one sentence for most functions.
  How the function works inside is not contract.
- **File or script header: purpose and usage**, ten lines at most.
- **Once.** Shared rationale sits at the declaration; other sites name it.

Published documentation that lives in source is exempt from the length limits only: godoc on CRD fields, and comments in a chart's `values.yaml`.

## By language

- **Go.** Doc comments are sentences opening with the name.
  Skip what the signature says; state what a caller cannot guess (concurrency, zero value, ownership).
  `//nolint` gives its reason on the same line.
  No comment above a test that repeats the test's name.
- **Bash.** Header is purpose plus usage.
  A function comment only where arguments, globals, or outputs are not obvious.
  `# shellcheck disable=` has its justification on the line above; an intentional unquoted expansion says so.
- **Python.** Docstrings on public functions and modules; inline comments rarely.
- **Workflow YAML.** Why a step, permission, or condition exists, when its name does not say.
  Keep the `# vX.Y.Z` trailing comment on a commit-pinned action, on the same line.
- **Makefile.** `target: ## help text` stays one line.
  A block above a target explains a non-obvious prerequisite or ordering only.

## Editing existing code

**Do not match the comment density of the file being edited.** Most files here run several times denser than this page allows, with dead backlog IDs throughout, and matching them reproduces it.

- New and rewritten comments follow this page regardless of their neighbours.
- When changing code, trim the comment that describes it to this page in the same commit, moving what fails the test per the table.
- Leave untouched comments alone; a sweep is its own PR.

## Before committing

Read every comment line the diff adds and check, in order:

1. Each sentence passes the test, read as the later reader.
2. No backlog ID, PR number, date, or change-narration word outside a `TODO(Q…)`.
   `make comment-provenance-check` fails the branch on the first three and on "used to"; other narration and the two checks below are yours to catch.
3. No implementation comment over three lines without a reason this page names.
4. Everything cut is in the commit message, the PR description, or a doc, not gone.
