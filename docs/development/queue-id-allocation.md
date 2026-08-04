# Agent reference: Allocating backlog Q-IDs

Backlog IDs are allocated by claiming a git ref on the remote, not by a counter line in [`docs/STATUS.md`](../STATUS.md).

```bash
make queue-id TITLE='GMC CRD manifest drifts from the AGC types it embeds'
```

That searches the backlog for near-duplicates of the title, prints any candidates to stderr, then claims and prints one ID (`Q423`). `make queue-id PEEK=1` shows what the next one would be without claiming it; `TARGET=<link>` sharpens the search when the Item cell's link is already decided. The script is [`scripts/docs/alloc-queue-id.sh`](../../scripts/docs/alloc-queue-id.sh), which takes one title argument per ID — several titles claim several IDs, each searched on its own.

Claim an ID when you file the row, use it, and move on. There is nothing to release and nothing to clean up.

**The title is mandatory, and there is no untitled batch form.** This target is the one chokepoint every filed row passes through, so it is where the near-duplicate search belongs — and an optional argument would be a gate nobody passes through. `-n <count>` used to claim IDs without naming a row; it is gone, because a door beside the gate is the same as no gate. Why the search keys on what it does, and what it costs in false positives: [maintaining-backlog.md § Search before you file](maintaining-backlog.md#search-before-you-file).

## Why a ref

Creating a ref that already exists fails server-side, atomically. That makes a ref name a compare-and-swap register, so two sessions asking for an ID at the same instant get different ones without any lock, lease, or coordination.

The mechanism is a single API call:

```bash
gh api -X POST "repos/$REPO/git/refs" -f ref=refs/queue-ids/Q423 -f sha="$SENTINEL"
```

`201` means you won. `422 Reference already exists` means someone beat you, so advance and retry.

## Why the counter had to go

`**Next ID:** QN` was one mutable line. Two sessions filing a row concurrently always read the same value, always took the same ID, and always conflicted on the same line. Not occasionally: by construction.

The conflict itself was cheap. The resolution was not, because it meant renumbering: the row, its `<a id="QN"></a>` anchor, every `(#QN)` cross-reference in sibling rows, the plan doc, the PR body, and the commit subject. Q382 recorded three such renumberings across a single PR's rebases.

## What this fixes, and what it does not

Measured on a 10-row table, resolving two branches against a common base:

| Concurrent edit | Result |
|---|---|
| Delete two rows 5 apart | clean |
| Delete two rows 2 apart | clean |
| Delete two **adjacent** rows | **conflict** |
| Both insert at the top | **conflict** |
| Insert at top vs delete row 8 | clean |
| Insert at top vs delete row 1 | **conflict** |
| Both delete the **same** row | clean |

Adjacency is the whole story: one untouched row of separation is enough to merge cleanly.

**Row conflicts are unchanged by this.** They are also concentrated where the process puts them, because picking from the top means deletions cluster at the top, and priority-on-entry plus flakes-first means insertions cluster there too. A four-worker dispatch batch takes rows 1 through 4 and every pair is adjacent.

What the ref allocator removes is the *expensive* class (duplicate IDs and the renumbering cascade) and the one conflict that was guaranteed rather than incidental. Row conflicts remain, are two lines, and resolve obviously. Their real danger is a botched resolution — a done row silently restored — not the conflict itself; `lint-backlog` rule 10 now catches that ([why](maintaining-backlog.md#a-moved-row-defeats-conflict-detection)).

Every row in the table above is a *line-position* verdict. The [merge driver](maintaining-backlog.md#the-merge-driver-resolve-queue-rows-by-id-not-by-line-position) re-decides the same cases by row ID, which makes all of them clean, and leaves conflict markers only where the two sides genuinely disagree about one row. It is opt-in per clone (`make merge-driver`) and applies to local merges and rebases only, so the numbers above are still what an uninstalled clone — and GitHub's squash-merge — will do.

## Alternatives considered

**Move the backlog to GitHub issues.** Rejected. Issues would give free IDs, no backlog conflicts, and a native in-flight signal, but they cannot express *position is priority*: a single total ordering, readable in one file read and changeable in one reviewable diff. Projects v2 has a position field, but it lives outside the repo and cannot be diffed. Issues would also cost the lintable write path that keeps Notes under the cap and pushes rationale into a doc, and the atomicity of deleting the row in the same diff as the work. Revisit if outside contributors need to see and claim work; that is the one thing issues clearly win.

**A custom merge driver for `STATUS.md`.** Shipped — [`scripts/docs/git-merge-status.sh`](../../scripts/docs/git-merge-status.sh), documented in [maintaining-backlog.md](maintaining-backlog.md#the-merge-driver-resolve-queue-rows-by-id-not-by-line-position). It resolves the Queue table by ID set-semantics during merge and rebase, which is where the pain is, and keeps the whole existing tooling stack. It needs a one-time `git config` per clone (`make merge-driver`; git will not let `.gitattributes` configure a driver, since that would be remote code execution on clone) and degrades to ordinary conflict markers both when unconfigured and whenever the resolution is not certain. It does not help GitHub's server-side squash-merge.

**One file per item** (`docs/queue/Q423.md`). The permanent answer to row conflicts, since adds and removes become file creates and deletes, which cannot conflict. Held in reserve: it rewrites `lint-backlog.sh`, this process, the shared backlog skill, and costs the single-file read of the prioritized queue.

**Dispatcher-owned row removal.** Rejected. It only works when a dispatcher session exists, and items are also spawned by ID directly or as "start the next one", which have no serialization point.

## Operational notes

- **Claims point at the repository's root commit**, never at a branch tip. A ref is a GC root, so claims anchored to `claude/*` tips would pin every squash-orphaned branch history forever. Anchored to one already-permanent object, they retain nothing new.
- **No garbage collection.** Each ref is ~64 bytes in `packed-refs`. The remote already carries ~800 `refs/pull/*` refs that GitHub never prunes, so the namespace is noise against what is already there. Deleting a claim would also let a retired ID be reissued, which the "IDs are never reused" rule forbids.
- **Clones never fetch them.** The default refspec is `+refs/heads/*:refs/remotes/origin/*`, and GitHub serves git protocol v2, which filters refs server-side. There is no fetch or clone cost.
- **IDs are sparse.** A session that claims an ID and never files a row leaves a hole. That is expected, not a leak.
- **Never claim with `git push <sha>:<ref>`.** When the ref already exists and points at the same object, push reports `Everything up-to-date` and exits 0, so the caller concludes it won a race it lost. Every claim shares one sentinel object, so this failure mode is the default, not an edge case.

To see what has been allocated:

```bash
git ls-remote origin 'refs/queue-ids/*'
```
