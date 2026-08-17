# Backlog

One file per item. Priority is the `rank` key, not the position of a line in a table, so two sessions taking the top two items touch two different files and never conflict.

Pick the top ready item, or read the whole backlog in priority order:

```bash
python3 scripts/docs/queue.py next
```

```bash
python3 scripts/docs/queue.py render
```

## Conventions

**Status:** `ready` · `blocked` · `deferred`
**Size:** S = one session · M = 2-3 sessions · L = multi-session, needs a phased plan doc in [`docs/plan/`](../plan/README.md)
**Labels:** `milestone` `security` `tests` `speed` `docs` `ci` `dogfood` `debt` `feature` `bug` `flake` `retro` `1.6-gate` (blocks the Release 1.6 tag, [scoped on the ladder](../plan/release-ladder.md)) `2.0-gate` (blocks the [v2 GA](../plan/v2-ga.md) tag)
**New IDs:** `make queue-id TITLE="…"`: it searches for near-duplicates, then claims ([why there is no counter](../development/queue-id-allocation.md))

The label list is closed: `check-queue-rules.sh` fails an item carrying a label this page does not declare. Adding a category means adding it here first. Gate labels for shipped releases are retired rather than kept, because no open item can carry one.

A `deferred` item carries no priority position and is not picked from the top. Each waits on an explicit trigger, tagged by source: **Demand:** an outside operator or user ask · **Event:** an observable outside-our-control condition · **Decision:** our own call, where we are the blocker.

## Rules the gates enforce

`queue.py lint` checks the store's shape: unique ranks, a closed status set, a title within 72 characters, and a note that opens with what a blocked item waits on.

`check-queue-rules.sh` checks what a per-item store makes silent, and each of the three guards a loss no other gate can see:

- **A `flake` item may not simply vanish.** Retiring one means recording it in [the flake-watch ledger](../development/flake-watch-retired.md), so a flake closed without a fix leaves a trace.
- **The last item targeting a plan flips that plan's row** in [`docs/plan/README.md`](../plan/README.md), so a plan cannot read open once nothing points at it.
- **Every label is declared here.**

Maintained per [`maintaining-backlog.md`](../development/maintaining-backlog.md): completed items are deleted and git is the archive, the open PR is the in-flight signal, and new items enter at the rank they deserve.
