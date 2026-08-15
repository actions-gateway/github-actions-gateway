# Backlog item store

One file per backlog item, named for its ID.
This is the storage half of [Q869](../plan/q869-per-item-queue-store.md); the [Queue and Deferred tables in `docs/STATUS.md`](../STATUS.md#queue) are still the authoritative copy.

**Both representations are committed right now, and a gate keeps them identical.** Edit the table as you always have, then regenerate:

```bash
make queue-import
```

If you skip that, `make check` fails with the store and the tables disagreeing.
That is the gate doing its job, not a broken tree — regenerate and commit.

## Why the files exist before anything reads them

Priority is stored as a position in the table, and the process aims every edit at the same end of it: pick from the top, file at the priority the item deserves, flakes first.
Two branches taking adjacent rows conflict by construction, and the [ID-keyed merge driver](../../scripts/docs/git-merge-status.sh) cannot help where it counts, because it is per-clone `git config` and never runs on GitHub.
By the time it fires, the pull request is already `DIRTY` and a rebase has forced a full CI re-run.

An item that owns its own file cannot collide with another item at all, whatever the merge algorithm and with no driver installed.
Ordering moves into each file as a `rank` key, so placing an item writes one file and moves nothing else.

## What is in a file

```yaml
---
id: Q869
rank: a0
labels: [speed, ci, debt]
status: ready          # ready | blocked | deferred
size: L
target: ../plan/q869-per-item-queue-store.md   # optional
---

# The item's title

The Notes cell, as prose.
```

`rank` is an order key, computed by tooling and never typed by hand.
It is compared as a plain string, and two items that end up with the same key are ordered by ID, so two sessions that never saw each other can file at the same priority without either one losing.

Links are written relative to **this directory**, one level below the table, so an item page resolves unrendered on github.com and on the site.
The renderer converts them back when it emits a table row.

## What has not happened yet

The tables are still written and read by hand, `docs/STATUS.md` remains the file to edit, and nothing links to an item page.
The cutover that deletes the tables, renders the ordered view from this store, and retires the merge driver lands after 1.5.0 ships — the phases and their sequencing are in [the plan](../plan/q869-per-item-queue-store.md).

Until then, treat this directory as generated output.
