# The merge drivers on Go

**Status:** filed 2026-08-30.
Phase 1 in progress.

Four git merge drivers resolve this repo's contended registry files, and all four are shell over `awk`: 1,603 lines across six files, of which [`merge-keyed-records.awk`](../../scripts/lib/merge-keyed-records.awk) is the 320-line three-way set merge that three of them share.
This plan moves the stack to Go in `devtools/`, keeping every `scripts/` entry point.

## Why, and why the existing answer does not cover it

[markdown-gates-parser.md](markdown-gates-parser.md) moved four `awk` Markdown *gates* onto goldmark and excluded the drivers, on this reasoning:

> A merge driver must reconstruct the file line for line, including the conflict-marker fallback; an AST discards exactly the byte-level fidelity it depends on.

That is correct, and it rules out **goldmark**, not **Go**.
Go over `bufio`/`strings` reconstructs line for line exactly as `awk` does.
The exclusion has been read since as a ruling against Go, which is why that doc's line is corrected as part of Phase 1.

The case for moving is testability, not correctness-today.
No defect is measured in any of the four drivers, and this plan asserts none.

- **The algorithm is the untested part.** `merge-keyed-records.awk` decides a three-way set merge *and* reconstructs row order by inferring which side reordered, comparing each side's shared-row sequence against the base's.
  Nothing exercises `seq_equal`, the skeleton walk, or the splice in isolation; all four suites drive a whole driver end to end.
- **Its failure mode is silent state loss.** The file carries a completeness backstop and an emitted-vs-surviving count check because a dropped row is worse than a conflict marker.
  Those are the assertions of an author who could not unit-test the thing.
- **The set merge exists twice.** [`git-merge-gate-lists.sh`](../../scripts/ci/git-merge-gate-lists.sh) has its own `merge_entries`, a second three-way set merge with different tie-breaking, because a Makefile list is not a Markdown record.
  One typed core serves both.

Runtime is not the argument, and nothing here should be justified on it.
Measured 2026-08-30: the `awk` merges a 200-row three-way input in 7.2 ms; a `devtools/` binary builds in 0.203 s warm and 0.810 s cold, then runs.
A driver invocation is human-paced, so the build cost is irrelevant either way.

## What stays exactly as it is

- **The four `scripts/` entry points**, at their current paths.
  `git config merge.<name>.driver` stores a repo-relative path, so every clone that has run `make merge-driver` keeps working with no reinstall.
  Each becomes a thin script that builds the binary into `.build/` and execs it, the pattern [`check-roadmap.sh`](../../scripts/docs/check-roadmap.sh) already uses.
- **The fallback contract.** Every uncertainty still re-runs `git merge-file` and keeps its conflict markers, and the exit status still stays under 128 so git records a conflict rather than a crashed driver.
  A failed `go build` is one more uncertainty and takes the same path.
- **The four test suites**, 1,949 lines, unchanged.
  They drive the entry point with git's placeholders and never reach inside, so they are the differential oracle for the port: a Go driver that changes any observable behaviour fails them.

## Scope, one phase each

1. **Phase 1**: the shared runtime (`devtools/git/mergedriver`: argument handling, `--install`, labels, fallback), the keyed-record set merge, and the two table drivers (`scriptindex`, `planindex`).
2. **Phase 2**: the roadmap driver, including the bullet encode/decode and the spacing three-way rule.
   Deletes `merge-keyed-records.awk`.
3. **Phase 3**: the gate-lists driver, folding its second set merge onto the shared core.

One binary with a subcommand per driver, so the four entry points share one build and one artifact.

## Validation

Per [testing.md](../development/testing.md#a-bulk-mechanical-change-proves-itself-by-reconciliation-not-by-an-empty-leftover-query), each phase reconciles rather than greps.

- The suite for each ported driver passes **unchanged**, with no edit to the assertions.
- Each ported driver is additionally checked against the shell one it replaces, on the same inputs, before the shell is deleted.
- New Go tests cover what no suite could reach: the order-reconstruction cases, the uncertainty matrix per key, and the round-trip.
- Deleting the mechanism must make the new tests red, per testing.md § Verify a causation claim by deleting the mechanism.
