# Agent reference: Allocating backlog Q-IDs

Backlog IDs are allocated by claiming a git ref on the remote, not by a counter line in [`docs/STATUS.md`](../STATUS.md).

```bash
make queue-id TITLE='GMC CRD manifest drifts from the AGC types it embeds'
```

That searches the backlog for near-duplicates of the title, prints any candidates to stderr, then claims and prints one ID (`Q423`). `TARGET=<link>` sharpens the search when the Item cell's link is already decided. The script is [`scripts/docs/alloc-queue-id.sh`](../../scripts/docs/alloc-queue-id.sh), which takes one title argument per ID — several titles claim several IDs, each searched on its own.

Claim an ID when you file the row, use it, and move on. There is nothing to release and nothing to clean up.

**Every path through the target claims, and an ID you did not claim will not lint.** Those are the two halves of the same rule: the only way to learn an ID is to hold it, and a row carrying an ID nobody holds fails [`lint-backlog`](maintaining-backlog.md) rule 12 at the commit that files it. Both are below, under [Reserving, not reporting](#reserving-not-reporting).

**The title is mandatory, and there is no untitled batch form.** This target is the one chokepoint every filed row passes through, so it is where the near-duplicate search belongs — and an optional argument would be a gate nobody passes through. `-n <count>` used to claim IDs without naming a row; it is gone, because a door beside the gate is the same as no gate. Why the search keys on what it does, and what it costs in false positives: [maintaining-backlog.md § Search before you file](maintaining-backlog.md#search-before-you-file).

## Why a ref

Creating a ref that already exists fails server-side, atomically. That makes a ref name a compare-and-swap register, so two sessions asking for an ID at the same instant get different ones without any lock, lease, or coordination.

The mechanism is a single API call:

```bash
gh api -X POST "repos/$REPO/git/refs" -f ref=refs/queue-ids/Q423 -f sha="$SENTINEL"
```

`201` means you won. `422 Reference already exists` means someone beat you, so advance and retry.

## Reserving, not reporting

A ref claim is only a reservation for the sessions that make one. Q656 measured what the rest costs.

On 2026-08-03 a row carrying Q644 was committed at 09:30:59. Another session allocated at 10:14:06, was handed Q643 and Q644, and therefore saw a floor of 642. **No Q644 claim existed 43 minutes after a row was already using it.** Two sessions running the allocator cannot both be handed Q644; the create-ref call fails for the second, atomically. The collision proves one of them never made the call. The one that did held the ID legitimately, merged second, and paid the renumber across a commit message, a PR body and a plan doc.

There were two ways to hold an ID without reserving it, and both are closed:

- **`--peek` / `PEEK=1`** printed the next free ID and claimed nothing. That is the `**Next ID:** QN` counter this mechanism replaced, behind a flag: two sessions reading it concurrently read the same answer. **Removed.** Knowing the next ID without taking it has no use that survives the session, and IDs are free: if you want to know, claim it.
- **Reading the file's highest ID and adding one.** No tool can prevent that, so it fails loudly instead. `lint-backlog` rule 12 requires every Q-ID a branch *adds* to hold a `refs/queue-ids/QN` claim, and it runs in the pre-commit hook, `make check`, and CI — so an unreserved ID is caught at the commit that files it rather than at the rebase that collides. The message names the fix, and `BACKLOG_ALLOW_UNCLAIMED_ID="Q1 Q2"` is there for an ID claimed from another clone.

What rule 12 costs and what it still misses:

- It checks only IDs that are new against the git baseline, so a branch that files no row makes no network call at all.
- IDs below the namespace's lowest claim (Q421) predate the allocator and hold no ref, so they are skipped.
- When `git ls-remote` cannot reach the remote it skips rather than fails, so an offline clone still lints. CI re-runs it with a network.
- **It cannot catch a hand-picked ID that another session has already claimed but not yet filed.** That is the narrow residual: the ref exists, so the row looks reserved. Closing it needs the claim to record *who* holds it, which no one has needed yet.

The concurrency the claim exists for is asserted in [`scripts/docs/alloc-queue-id-test.sh`](../../scripts/docs/alloc-queue-id-test.sh): eight allocators released at the same instant, against a bare fixture origin and a `gh` stub, each writing its own result. They take eight distinct IDs. The last case deletes the mechanism, running the identical fleet against a stub whose create-ref reports success and reserves nothing. All eight then take the same ID, which is what makes the first assertion mean anything.

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

**A custom merge driver for `STATUS.md`.** Shipped — [`scripts/docs/git-merge-status.sh`](../../scripts/docs/git-merge-status.sh), documented in [maintaining-backlog.md](maintaining-backlog.md#the-merge-driver-resolve-queue-rows-by-id-not-by-line-position). It resolves the Queue table by ID set-semantics during merge and rebase, which is where the pain is, and keeps the whole existing tooling stack. It needs a one-time `git config` per clone (`make merge-driver`; git will not let `.gitattributes` configure a driver, since that would be remote code execution on clone) and degrades to ordinary conflict markers both when unconfigured and whenever the resolution is not certain. It does not help GitHub's server-side squash-merge. The same `make merge-driver` installs a [sibling for `docs/plan/README.md`](maintaining-backlog.md#the-same-treatment-for-docsplanreadmemd), which has the same contention keyed on the plan path (Q611).

**One file per item** (`docs/queue/Q423.md`). The permanent answer to row conflicts, since adds and removes become file creates and deletes, which cannot conflict. Held in reserve: it rewrites `lint-backlog.sh`, this process, the shared backlog skill, and costs the single-file read of the prioritized queue.

**Dispatcher-owned row removal.** Rejected. It only works when a dispatcher session exists, and items are also spawned by ID directly or as "start the next one", which have no serialization point.

## Operational notes

- **Claims point at the repository's root commit**, never at a branch tip. A ref is a GC root, so claims anchored to `claude/*` tips would pin every squash-orphaned branch history forever. Anchored to one already-permanent object, they retain nothing new.
- **No garbage collection.** Each ref is ~64 bytes in `packed-refs`. The remote already carries ~800 `refs/pull/*` refs that GitHub never prunes, so the namespace is noise against what is already there. Deleting a claim would also let a retired ID be reissued, which the "IDs are never reused" rule forbids.
- **Clones never fetch them.** The default refspec is `+refs/heads/*:refs/remotes/origin/*`, and GitHub serves git protocol v2, which filters refs server-side. There is no fetch or clone cost.
- **IDs are sparse, and a claimed-but-unused ID is never reclaimed.** A session that claims an ID and dies, or files nothing, strands it — the ref stays, the number is never reissued, and the gap is permanent. That is the intended behaviour, not a leak to fix: reclaiming would mean deciding that a claim is stale, and the session holding it is exactly the one that cannot be asked. Measured 2026-08-03: 10 of 240 claims (Q432, Q442, Q470, Q473, Q487, Q494, Q496, Q504, Q608, Q655) never became a row, so the space is consumed about 4% faster than rows are filed. Against a 64-byte ref and an unbounded integer, that buys nothing worth the risk of reissuing a retired ID. Removing `--peek` raises the rate slightly, since a session that would have peeked now holds one.
- **Never claim with `git push <sha>:<ref>`.** When the ref already exists and points at the same object, push reports `Everything up-to-date` and exits 0, so the caller concludes it won a race it lost. Every claim shares one sentinel object, so this failure mode is the default, not an edge case.

To see what has been allocated:

```bash
git ls-remote origin 'refs/queue-ids/*'
```
