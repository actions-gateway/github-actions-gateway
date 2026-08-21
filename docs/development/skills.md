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

**Working-method skills cost a contributor nothing**: `deslop`, `readability`, `semantic-remediation`, `verify-claims`, `rendered-page-review`, `tech-docs-layers`, `github-issue-filer`, `session-retro`.
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
It is the third of the four prose passes a release note goes through: [release.md § The prose passes](../operations/release.md#the-prose-passes).

### `github-issue-filer`

Files an issue to any GitHub repo through `gh`, matching the target's template and searching for duplicates first.
Used here when repo tooling misbehaves and the fix belongs upstream rather than in a local workaround.

### `readability`

Structures prose so a reader who did not do the work can find it, follow it, and act on it.
Second of the four prose passes over a release note, where the reader is an operator deciding what an upgrade costs them: [release.md § The prose passes](../operations/release.md#the-prose-passes).

### `rendered-page-review`

Reviews a page as a reader meets it: rendered, at real viewport widths, skimmed rather than read.
Everything under `docs/` publishes to [actions-gateway.com](https://actions-gateway.com/), so editing one of these files edits a live page.
[website.md](website.md#measure-the-render-the-source-cannot-answer-these-questions) holds the repo-specific probes it runs.

### `semantic-remediation`

Finds sentences that read fluently and fall apart on a literal read, then repairs them in a separate pass.
Last of the four prose passes over a release note, after the other three, because editing is what introduces the defects it looks for: [release.md § The prose passes](../operations/release.md#the-prose-passes).

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
It is also the first of the four prose passes over a release note, where the claim being checked is one the project is about to publish: [release.md § The prose passes](../operations/release.md#the-prose-passes).

## Vendored from the skills repo

Four files are copied out of `karlkfi/claude-skills` and run here as ordinary repo tooling.
[`scripts/ci/vendored-skills.tsv`](../../scripts/ci/vendored-skills.tsv) lists them with the upstream path and commit each came from.

| Here | Upstream | Taken from |
|---|---|---|
| [`scripts/docs/queue.py`](../../scripts/docs/queue.py) | `session-backlog/scripts/queue.py` | `b0330e0f`, 2026-08-16 |
| [`scripts/docs/rank-vectors.tsv`](../../scripts/docs/rank-vectors.tsv) | `session-backlog/scripts/rank-vectors.tsv` | `b0330e0f`, 2026-08-16 |
| [`scripts/agent/pr-requeue-eligible.py`](../../scripts/agent/pr-requeue-eligible.py) | `session-worker/scripts/pr-requeue-eligible.py` | `8f65c5b6`, 2026-08-16 |
| [`scripts/agent/pr-mergeability-watch.py`](../../scripts/agent/pr-mergeability-watch.py) | `session-orchestrator/scripts/pr-mergeability-watch.py` | `0d38df40`, 2026-08-15 |

[Q889](../plan/q889-backlog-item-store.md) phase 1 took all four byte-identical, so that an upstream fix would land here as a clean overwrite.
Nothing held them there, and nothing in the tree said they were vendored at all.

**Measured 2026-08-21, and it is not the drift the backlog row assumed.** Each file as of the vendoring commit hashes to an upstream commit exactly, so phase 1 did what it claimed.
Upstream has since taken 8 commits on `queue.py` and 1 to 2 on each of the others, and this repo took none until Q935 forked `queue.py` to fix its stale-citation pattern.
So the fork ran one way for five days, in the direction nobody here controls.
A clean overwrite is no longer available for `queue.py` in any case: it and its upstream now differ by 264 diff lines, which is a re-vendor rather than a patch.

`make vendored-skills-check` asserts the half a local read can reach.
Each file still hashes to the digest its row declares, so forking one moves the digest in the same diff and a reviewer sees it.
`scripts/ci/check-vendored-skills.sh --report` separates an unmodified vendor from a declared fork, and `--update` is how a fork is declared.

**Whether upstream has moved is still unasked, deliberately.** `karlkfi/claude-skills` is private, so a gate that fetched it would need a token this repo does not carry, and [a gate whose oracle is the network](testing.md#the-release-link-gate) fails when a third party sneezes.
Comparing against a local clone would key the gate to one workstation's layout.
So an upstream fix reaches this tree only when somebody looks, and the gate makes the looking cheap rather than automatic.

Nothing derives the set, because nothing marks a file as vendored, so a fifth one adopted without a row is invisible to the gate.

## Keeping this page honest

A skill can be renamed or retired upstream without anything here going red, because no gate reads the skill set.
Two names in this tree have already drifted that way: `backlog` became `session-backlog`, and `dispatch-worker` was retired in favour of `session-worker`.

So when a page here names a skill, name it exactly, and check this list.
When the list itself is wrong, the tell is a name that no longer appears in `karlkfi/claude-skills`.
