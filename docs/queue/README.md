# Backlog

One file per item.
Priority is the `rank` key, not the position of a line in a table, so two sessions taking the top two items touch two different files and never conflict.

Pick the top ready item, or read the whole backlog in priority order:

```bash
python3 scripts/docs/queue.py next
```

`next` skips an item an open pull request already names, and says which PR on stderr.
A PR that merely cites the id has not claimed it, so read it and pass `--allow QNNN` to take the item anyway.
With no network, `--no-pr-check` hands out the top item and says nothing checked it.


```bash
python3 scripts/docs/queue.py render
```

`--group release` sections the output by gate label, in version order, with everything that gates no release under **Unscheduled**.
That is the answer to *what is left before the next minor*, and to how much of the store is not release work at all.

```bash
python3 scripts/docs/queue.py render --group release
```

`--capacity W` slices the ungated remainder into consecutive releases of at most `W` session-equivalents, in rank order.
Gate-labelled rows are commitments, so they ride above the line and do not consume it.

```bash
python3 scripts/docs/queue.py render --group release --capacity 80
```

Nothing is assigned to a release by hand: a slice is a window over the rank order, so re-ranking a row re-plans every release below it and there is no second priority axis to drift out of step with `rank`.
Sizes are weighted because they are not comparable (XS 0.5, S 1, M 2.5, L 6; an item with no size costs 1), so one `L` cannot displace six `S` on a row count.

**Where 80 comes from.** Measured 2026-09-18 over the rows deleted from the store: 240 session-equivalents in the last 28 days and 146 in the last 14, against a 9-day median gap across the nine minors from `v1.0.0` to `v1.8.0`.
That is 77 and 94 sessions per release window respectively, and about a tenth of those deletions are retirements rather than delivered work.
Pick the number deliberately rather than treating it as a constant: it is a decision about how long a release should take, and the store's throughput is the evidence for it, not the answer.

## Conventions

**Status:** `ready` · `blocked` · `deferred`  
**Size:** S = one session · M = 2-3 sessions · L = multi-session, needs a phased plan doc in [`docs/plan/`](../plan/README.md)  
**Labels:** `milestone` `security` `tests` `speed` `docs` `ci` `dogfood` `debt` `feature` `bug` `flake` `retro` `open-question` (the row ends in a choice, and names the route that settles it) `1.9-gate` (blocks the Release 1.9 tag, the [Rule 4b overlap release](../plan/release-ladder.md)) `2.0-gate` (blocks the [v2 GA](../plan/v2-ga.md) tag)  
**New IDs:** `make queue-id TITLE="…"`: it searches for near-duplicates, then claims ([why there is no counter](../development/queue-id-allocation.md))  
**Ranks:** `make queue-rank ARGS="--after <rank> --before <rank>"` (also `--head` / `--tail`): it warns when the key it mints is one the store already holds ([why a tie is legal](../development/maintaining-backlog.md#mint-the-rank-dont-write-one))

The trailing hard breaks above are load-bearing: `check-queue-rules.sh` anchors the vocabulary to a line starting `**Labels:**`, and without them `mdreflow` folds all four into one paragraph.
It exits unmeasurable rather than passing when that happens, which is how this was caught.

The label list is closed: `check-queue-rules.sh` fails an item carrying a label this page does not declare.
Adding a category means adding it here first.
Gate labels for shipped releases are retired rather than kept, because no open item can carry one.

A `deferred` item carries no priority position and is not picked from the top.
Each waits on an explicit trigger, tagged by source: **Demand:** an outside operator or user ask · **Event:** an observable outside-our-control condition · **Decision:** our own call, where we are the blocker.

## Rules the gates enforce

`queue.py lint` checks the store's shape: unique ranks, a closed status set, a title within 72 characters, and a note that opens with what a blocked item waits on.

`check-queue-rules.sh` checks what a per-item store makes silent, and each of these guards a loss no other gate can see:

- **A `flake` item may not simply vanish.** Retiring one means recording it in [the flake-watch ledger](../development/flake-watch-retired.md), so a flake closed without a fix leaves a trace.
- **The last item targeting a plan flips that plan's row** in [`docs/plan/README.md`](../plan/README.md), so a plan cannot read open once nothing points at it.
- **Every label is declared here.**
- **A link a row carries resolves for MkDocs.** This page publishes at `/dev/queue/`, so a link that leaves `docs/` and either points back into it or leaves the repository aborts `mkdocs --strict`, a class no local gate builds the site to catch.
  Write it relative to the store: `../development/website.md`, never `../../docs/development/website.md`.
  Notes count as well as `target:`, and a link quoted inside a fenced block or a code span does not.

Maintained per [`maintaining-backlog.md`](../development/maintaining-backlog.md): completed items are deleted and git is the archive, the open PR is the in-flight signal, and new items enter at the rank they deserve.
