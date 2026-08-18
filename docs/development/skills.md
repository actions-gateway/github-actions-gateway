# Agent skills

Several rules in this repo are carried by **skills**: packaged instruction sets an agent loads on demand rather than reading out of a doc.
The ones this repo leans on are installed on the workstation, not committed here, and their source repo is private.
So a page that needs to name one has nowhere to link, and a bare URL would 404 for most readers.

This page is that link target.
It records what each skill is for and where this repo invokes it, so another page can point a reader at an explainer with `skills.md#<name>`.

**It deliberately does not restate what a skill says.** A skill is written to be portable across repos; the repo-specific half already lives in the page that invokes it, and duplicating the general half here would leave two copies to correct.
Every entry below is a pointer plus the local usage, and nothing more.

## What a contributor without the skills actually loses

Two classes, and they answer differently.

**Working-method skills cost a contributor nothing**: `deslop`, `verify-claims`, `rendered-page-review`, `tech-docs-layers`, `github-issue-filer`, `session-retro`.
They change how an *agent* works, never what a contributor must do, and the page that invokes each one carries this repo's half in full.
If one of those pages ever reads as though the skill is required to follow it, that page has a defect, so file it.

**The three process skills are different: they own the process, and this tree keeps only its deltas.** [`session-backlog`](#session-backlog), [`session-orchestrator`](#session-orchestrator) and [`session-worker`](#session-worker) carry the portable backlog format and the dispatch contract, and [maintaining-backlog.md](maintaining-backlog.md) and [parallel-dispatch.md](parallel-dispatch.md) deliberately do not restate them.
A contributor reading those two pages gets what is true *here*: the caps, the allocator, the gate, the merge queue, the tooling, the measurements.
What they do not get is the process those deltas modify.

What holds the rules for that reader is that the **tooling is in-tree and gate-enforced, where the prose is not**: `lint-backlog.sh`, the ID allocator, the merge drivers, `check-status-isolation.sh` and the dispatch hooks all run in `make check` and the pre-commit hook whether or not any skill is installed.
Those five were written here rather than copied in, and the skill has no counterpart for most of them, so "vendored" would read the dependency backwards.
A contributor cannot violate a rule that matters without a gate saying so.
That is the trade this repo took, and the reader it accepts losing is the drive-by contributor who would groom the backlog or run a dispatch by hand, which nobody outside the maintainer does.
Restoring the prose is what would have to change if that stops being true.

## Three sources, and only one of them is linkable

The distinction matters because getting it wrong produces a link that resolves today and dies silently later.

| Source | Where it lives | Linkable from `docs/`? |
|---|---|---|
| **Globally-installed** | `~/.claude/skills/`, from the private `karlkfi/claude-skills` | **No.** Outside every repo, and private. Name it and link this page. |
| **Plugin** | `~/.claude/plugins/**/skills/`, from each plugin's own repo | **No.** Same reason; these are namespaced `plugin:skill`. |
| **Repo-local** | `.claude/skills/` in this repo | **Yes.** In-tree, so a relative link resolves and `doc-links` checks it. |

The failure this table exists to prevent has already happened once: a doc cited a repo-local skill by relative path, an unrelated change retired that skill, and `main` went red on `doc-links`, which is the good outcome, because the gate can see a relative path.
It cannot see a dead external URL, so a private-repo link would have rotted in silence instead.

## The skills this repo uses

From `karlkfi/claude-skills`.
Listed alphabetically; each heading is a stable anchor.

### `deslop`

Removes AI tells from prose, and supplies the writing system that avoids them in the first place.
Used here for anything a reader outside the work will read.
[release.md](../operations/release.md) calls for it over a draft release note, the most-read prose the project ships.

### `github-issue-filer`

Files an issue to any GitHub repo through `gh`, matching the target's template and searching for duplicates first.
Used here when repo tooling misbehaves and the fix belongs upstream rather than in a local workaround.

### `rendered-page-review`

Reviews a page as a reader meets it: rendered, at real viewport widths, skimmed rather than read.
Everything under `docs/` publishes to [actions-gateway.com](https://actions-gateway.com/), so editing one of these files edits a live page.
[website.md](website.md#measure-the-render-the-source-cannot-answer-these-questions) holds the repo-specific probes it runs.

### `session-backlog`

The format and grooming process for the backlog in [docs/queue/](../queue/README.md).
[maintaining-backlog.md](maintaining-backlog.md) is authoritative wherever the two overlap, and this repo's own tooling (`lint-backlog.sh` over the `backloglint` rules, the ID allocator, the merge drivers) enforces the format with or without the skill installed.

### `session-orchestrator`

The dispatcher half of a parallel run: deciding whether a batch is worth parallelizing, spawning workers as full sessions, and carrying every PR to ready without merging any.
See [parallel-dispatch.md](parallel-dispatch.md).

### `session-retro`

Runs a retrospective over a finished session and turns the findings into tracked follow-through: a PR or a backlog row, never a note in the transcript.
Offered after a PR merges or a substantial task ends.

### `session-worker`

The worker half of a parallel run: one backlog item, one branch, one PR, plus the self-healing watcher loop and the rule that a worker never merges.
[parallel-dispatch.md](parallel-dispatch.md#the-worker-contract-self-healing) is where this repo's version of the contract lives.

### `tech-docs-layers`

The six-layer model for repo-resident documentation, applied when adding, restructuring, or updating a page under `docs/`.

### `verify-claims`

Checks that the evidence under a statement could have shown the opposite, before the statement decides anything.
Fires before reporting a gate result, diagnosing a failure, or writing a test or gate.
[testing.md § Diagnosing failures](testing.md#diagnosing-failures-measure-before-asserting-a-root-cause) keeps this repo's measured cases (which run, which PR, what the numbers were) for the rules the skill states in general.

## Keeping this page honest

A skill can be renamed or retired upstream without anything here going red, because no gate reads the skill set.
Two names in this tree have already drifted that way: `backlog` became `session-backlog`, and `dispatch-worker` was retired in favour of `session-worker`.

So when a page here names a skill, name it exactly, and check this list.
When the list itself is wrong, the tell is a name that no longer appears in `karlkfi/claude-skills`.
