# Agent reference: Allocating backlog Q-IDs

Backlog IDs are allocated by claiming a git ref on the remote, not by a counter line in [the backlog](../queue/README.md).

```bash
make queue-id TITLE='GMC CRD manifest drifts from the AGC types it embeds'
```

That searches the backlog for near-duplicates of the title, prints any candidates to stderr, then claims and prints one ID (`Q423`).
`TARGET=<link>` sharpens the search when the Item cell's link is already decided.
The script is [`scripts/docs/alloc-queue-id.sh`](../../scripts/docs/alloc-queue-id.sh), which takes one title argument per ID — several titles claim several IDs, each searched on its own.

Claim an ID when you file the row, use it, and move on.
There is nothing to release and nothing to clean up.

**Every path through the target claims, and an ID you did not claim is one `queue.py claims` rejects.** Those are the two halves of the same rule: the only way to learn an ID is to hold it, and an item carrying an ID nobody holds fails that check, which is rule 12 of the retired table-era linter, moved into `queue.py` by [Q889](../plan/q889-backlog-item-store.md).
`make queue-claims-check` runs it over this store, and the `status-lint` workflow runs it with `--strict` (Q1042).
Both are below, under [Reserving, not reporting](#reserving-not-reporting).

**The title is mandatory, and there is no untitled batch form.** This target is the one chokepoint every filed row passes through, so it is where the near-duplicate search belongs — and an optional argument would be a gate nobody passes through.
`-n <count>` used to claim IDs without naming a row; it is gone, because a door beside the gate is the same as no gate.
Why the search keys on what it does, and what it costs in false positives: [maintaining-backlog.md § Search before you file](maintaining-backlog.md#search-before-you-file).

## Why a ref

Creating a ref that already exists fails server-side, atomically.
That makes a ref name a compare-and-swap register, so two sessions asking for an ID at the same instant get different ones without any lock, lease, or coordination.

The mechanism is a single API call:

```bash
gh api -X POST "repos/$REPO/git/refs" -f ref=refs/queue-ids/Q423 -f sha="$SENTINEL"
```

`201` means you won.
`422 Reference already exists` means someone beat you, so advance and retry.

## Reserving, not reporting

A ref claim is only a reservation for the sessions that make one.
Q656 measured what the rest costs.

On 2026-08-03 a row carrying Q644 was committed at 09:30:59.
Another session allocated at 10:14:06, was handed Q643 and Q644, and therefore saw a floor of 642.
**No Q644 claim existed 43 minutes after a row was already using it.** Two sessions running the allocator cannot both be handed Q644; the create-ref call fails for the second, atomically.
The collision proves one of them never made the call.
The one that did held the ID legitimately, merged second, and paid the renumber across a commit message, a PR body and a plan doc.

There were two ways to hold an ID without reserving it, and both are closed:

- **`--peek` / `PEEK=1`** printed the next free ID and claimed nothing.
  That is the `**Next ID:** QN` counter this mechanism replaced, behind a flag: two sessions reading it concurrently read the same answer.
  **Removed.** Knowing the next ID without taking it has no use that survives the session, and IDs are free: if you want to know, claim it.
- **Reading the file's highest ID and adding one.** No tool can prevent that, so it fails loudly instead.
  `queue.py claims` requires every Q-ID a branch *adds* to hold a `refs/queue-ids/QN` claim, measured against the merge base rather than `origin/main`'s tip, and skips rather than fails when the remote cannot be read unless `--strict` is passed.
  The message names the fix, and `QUEUE_CLAIMS_ALLOW="Q1 Q2"` (or `--allow`) is there for an ID claimed from another clone.
  `scripts/docs/check-queue-claims.sh` points it at this store, backing `make queue-claims-check` in `make check` and `make queue-gates`, and the `status-lint` workflow runs the same script with `--strict` (Q1042).
  It reads the working tree rather than `HEAD`, so a row that has been written and not yet committed is already its subject, which is when a hand-picked ID is cheapest to fix.

What the check costs and what it still misses:

- It checks only IDs that are new against the git baseline, so a branch that files no row makes no network call at all.
  That baseline is the merge base with `origin/main`, not its tip: against the tip a row `main` deleted while your branch was behind read as one you had filed, and the rule demanded an ID for finished work (Q684).
- IDs below the namespace's lowest claim (Q421) predate the allocator and hold no ref, so they are skipped.
- When `git ls-remote` cannot reach the remote it skips rather than fails, so an offline clone still lints.
  `--strict` turns that skip into a failure, and CI is the only caller that passes it: a network is guaranteed there, so a skip in the one place the check is relied on would be a silent non-run.
- **It cannot catch a hand-picked ID that another session has already claimed but not yet filed.** That is the narrow residual: the ref exists, so the row looks reserved.
  Closing it needs the claim to record *who* holds it, which no one has needed yet.

The concurrency the claim exists for is asserted in [`scripts/docs/alloc-queue-id-test.sh`](../../scripts/docs/alloc-queue-id-test.sh): eight allocators released at the same instant, against a bare fixture origin and a `gh` stub, each writing its own result.
They take eight distinct IDs.
The last case deletes the mechanism, running the identical fleet against a stub whose create-ref reports success and reserves nothing.
All eight then take the same ID, which is what makes the first assertion mean anything.

## Why the counter had to go

`**Next ID:** QN` was one mutable line.
Two sessions filing a row concurrently always read the same value, always took the same ID, and always conflicted on the same line.
Not occasionally: by construction.

The conflict itself was cheap.
The resolution was not, because it meant renumbering: the row, its `<a id="QN"></a>` anchor, every `(#QN)` cross-reference in sibling rows, the plan doc, the PR body, and the commit subject.
Q382 recorded three such renumberings across a single PR's rebases.

## What this fixes, and what it does not

Measured on the single-table backlog this store replaced, 10 rows, resolving two branches against a common base:

| Concurrent edit | Result |
|---|---|
| Delete two rows 5 apart | clean |
| Delete two rows 2 apart | clean |
| Delete two **adjacent** rows | **conflict** |
| Both insert at the top | **conflict** |
| Insert at top vs delete row 8 | clean |
| Insert at top vs delete row 1 | **conflict** |
| Both delete the **same** row | clean |

Adjacency was the whole story: one untouched row of separation was enough to merge cleanly.
The finding outlived its subject, which is why [the registry drivers](maintaining-backlog.md#the-merge-drivers-resolve-registry-rows-by-key-not-by-line-position) cite it: any file carrying one row per thing collides the same way.

**Row conflicts were unchanged by this.** They were also concentrated where the process put them, because picking from the top means deletions cluster at the top, and priority-on-entry plus flakes-first means insertions cluster there too.
A four-worker dispatch batch took rows 1 through 4 and every pair was adjacent.

What the ref allocator removes is the *expensive* class (duplicate IDs and the renumbering cascade) and the one conflict that was guaranteed rather than incidental.
Row conflicts remained, were two lines, and resolved obviously.
Their real danger was a botched resolution — a done row silently restored — not the conflict itself; one file per item ends it, because a relocated item and a deleted one become a modify and a delete of one path, which git refuses rather than resolves ([why](maintaining-backlog.md#a-moved-row-defeated-conflict-detection-and-one-file-per-item-ends-it)).

Every verdict in that table is a *line-position* one, which is an artifact of storing the backlog as lines in one file.
A merge driver re-decided the same cases by row ID and made all of them clean, but it was opt-in per clone, never reached GitHub's server-side squash-merge, and retired with the table it served ([Q889](../plan/q889-backlog-item-store.md)).
`.gitattributes` routes no backlog path today, and adding one would rebuild the contention the store exists to remove.
One file per item deletes the line positions rather than resolving them, so none of these cases arises in the backlog now.
They arise in the registry files, which is where the drivers went.

## Alternatives considered

**Move the backlog to GitHub issues.** Rejected.
Issues would give free IDs, no backlog conflicts, and a native in-flight signal, but they cannot express a total ordering that lives in the repo and changes in one reviewable diff: a line position under the table, the `rank` key under the store.
Projects v2 has a position field, but it lives outside the repo and cannot be diffed.
Issues would also cost the atomicity of deleting the item in the same diff as the work, and the lintable write path that `queue.py lint` reads off the tree: frontmatter shape, the 72-character title cap, unresolvable targets.
Revisit if outside contributors need to see and claim work; that is the one thing issues clearly win.

**A custom merge driver for the backlog table.** Shipped, then retired with the table it served ([Q889](../plan/q889-backlog-item-store.md)).
It resolved the table by ID set-semantics during merge and rebase, which was where the pain was, and it kept the whole tooling stack of the day.
It needed a one-time `git config` per clone (`make merge-driver`; git will not let `.gitattributes` configure a driver, since that would be remote code execution on clone) and degraded to ordinary conflict markers both when unconfigured and whenever the resolution was not certain.
It never helped GitHub's server-side squash-merge.
The mechanism outlived the backlog: the same `make merge-driver` installs [the four drivers the registry files still use](maintaining-backlog.md#the-merge-drivers-resolve-registry-rows-by-key-not-by-line-position), keyed on a plan path (Q611), a bullet's backlog annotation (Q799), a script path, and a gate-list entry.

**One file per item** (`docs/queue/Q423.md`).
Shipped ([Q889](../plan/q889-backlog-item-store.md)), and the layout this doc now describes.
It was the permanent answer to row conflicts, since adds and removes became file creates and deletes, which cannot conflict.
It cost what this entry predicted: `lint-backlog.sh` retired in favour of `lint-queue.sh` and `check-queue-rules.sh`, this process and the shared `session-backlog` skill followed, and the single-file read of the prioritized queue became `queue.py render`.

**Dispatcher-owned row removal.** Rejected, and moot since Q889.
It only worked when a dispatcher session existed, and items are also spawned by ID directly or as "start the next one", which have no serialization point.
Closing an item now deletes its own file, so there is no shared line for two removals to contend for.

## Operational notes

- **Claims point at the repository's root commit**, never at a branch tip.
  A ref is a GC root, so claims anchored to `claude/*` tips would pin every squash-orphaned branch history forever.
  Anchored to one already-permanent object, they retain nothing new.
- **No garbage collection.** Each ref is ~64 bytes in `packed-refs`.
  The remote already carries ~800 `refs/pull/*` refs that GitHub never prunes, so the namespace is noise against what is already there.
  Deleting a claim would also let a retired ID be reissued, which the "IDs are never reused" rule forbids.
- **Clones never fetch them.** The default refspec is `+refs/heads/*:refs/remotes/origin/*`, and GitHub serves git protocol v2, which filters refs server-side.
  There is no fetch or clone cost.
- **IDs are sparse, and a claimed-but-unused ID is never reclaimed.** A session that claims an ID and dies, or files nothing, strands it — the ref stays, the number is never reissued, and the gap is permanent.
  That is the intended behaviour, not a leak to fix: reclaiming would mean deciding that a claim is stale, and the session holding it is exactly the one that cannot be asked.
  Measured 2026-08-03: 10 of 240 claims (Q432, Q442, Q470, Q473, Q487, Q494, Q496, Q504, Q608, Q655) never became a row, so the space is consumed about 4% faster than rows are filed.
  Against a 64-byte ref and an unbounded integer, that buys nothing worth the risk of reissuing a retired ID.
  Removing `--peek` raises the rate slightly, since a session that would have peeked now holds one.
- **Never claim with `git push <sha>:<ref>`.** When the ref already exists and points at the same object, push reports `Everything up-to-date` and exits 0, so the caller concludes it won a race it lost.
  Every claim shares one sentinel object, so this failure mode is the default, not an edge case.

To see what has been allocated:

```bash
git ls-remote origin 'refs/queue-ids/*'
```
