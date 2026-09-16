# The merge drivers on Go

**Status:** ✅ Complete 2026-09-16.
All four drivers are Go in `devtools/git/`, every `scripts/` entry point is a thin build-and-exec, and both `awk` set merges are gone.
Phase 1 shipped 2026-08-31 in [#1819](https://github.com/actions-gateway/github-actions-gateway/pull/1819): the shared runtime, the keyed-record set merge and the two table drivers.
Phase 2 shipped 2026-09-16 in [#1954](https://github.com/actions-gateway/github-actions-gateway/pull/1954): the roadmap driver, its bullet encode/decode and the `<!-- q:QN -->` key reader, deleting `merge-keyed-records.awk`.
Phase 3 shipped 2026-09-16: the gate-lists driver's `merge_entries`, its Makefile lift and render, and the shell half of `merge-driver-common.sh`.

Four git merge drivers resolve this repo's contended registry files, and at filing all four were shell over `awk`: 1,603 lines across six files, of which `merge-keyed-records.awk` was the 320-line three-way set merge that three of them shared.
This plan moves the stack to Go in `devtools/`, keeping every `scripts/` entry point.

## Why, and why the existing answer does not cover it

[markdown-gates-parser.md](markdown-gates-parser.md) moved four `awk` Markdown *gates* onto goldmark and excluded the drivers, on this reasoning:

> A merge driver must reconstruct the file line for line, including the conflict-marker fallback; an AST discards exactly the byte-level fidelity it depends on.

That is correct, and it rules out **goldmark**, not **Go**.
Go over `bufio`/`strings` reconstructs line for line exactly as `awk` does.
The exclusion has been read since as a ruling against Go, so that doc's line carries the distinction.
It was added in Phase 2, which is also where the link it carried to the deleted `awk` had to go.

The case for moving is testability, not correctness-today.
No defect is measured in any of the four drivers, and this plan asserts none.

- **The algorithm is the untested part.** `merge-keyed-records.awk` decided a three-way set merge *and* reconstructed row order by inferring which side reordered, comparing each side's shared-row sequence against the base's.
  Nothing exercises `seq_equal`, the skeleton walk, or the splice in isolation; all four suites drive a whole driver end to end.
- **Its failure mode is silent state loss.** The file carries a completeness backstop and an emitted-vs-surviving count check because a dropped row is worse than a conflict marker.
  Those are the assertions of an author who could not unit-test the thing.
- **The set merge exists twice.** [`git-merge-gate-lists.sh`](../../../scripts/ci/git-merge-gate-lists.sh) has its own `merge_entries`, a second three-way set merge with different tie-breaking, because a Makefile list is not a Markdown record.
  One typed core serves both.

Runtime is not the argument, and nothing here should be justified on it.
Measured 2026-08-30: the `awk` merges a 200-row three-way input in 7.2 ms; a `devtools/` binary builds in 0.203 s warm and 0.810 s cold, then runs.
A driver invocation is human-paced, so the build cost is irrelevant either way.

## What stays exactly as it is

- **The four `scripts/` entry points**, at their current paths.
  `git config merge.<name>.driver` stores a repo-relative path, so every clone that has run `make merge-driver` keeps working with no reinstall.
  Each becomes a thin script that builds the binary into `.build/` and execs it, the pattern [`check-roadmap.sh`](../../../scripts/docs/check-roadmap.sh) already uses.
- **The fallback contract.** Every uncertainty still re-runs `git merge-file` and keeps its conflict markers, and the exit status still stays under 128 so git records a conflict rather than a crashed driver.
  A failed `go build` is one more uncertainty and takes the same path.
- **The four test suites**, 1,949 lines at filing, with no change to any assertion.
  They drive the entry point with git's placeholders and never reach inside, so they are the differential oracle for the port: a Go driver that changes any observable behaviour fails them.
  A suite that copies the driver into a throwaway repo does gain symlinks to `devtools/` and `.build/`, because the copied entry point builds the binary.
  That is scaffolding, not an assertion.

## Scope, one phase each

1. **Phase 1**: the shared runtime (`devtools/git/mergedriver`: argument handling, `--install`, labels, fallback), the keyed-record set merge, and the two table drivers (`scriptindex`, `planindex`).
2. **Phase 2**: the roadmap driver, including the bullet encode/decode and the spacing three-way rule.
   Deletes `merge-keyed-records.awk`.
   The encode/decode is `devtools/git/mdroadmap`, which is also where `MarkerKey` lives: the roadmap is bullet lists rather than tables, so it does not belong beside `mdregistry`'s table splitter.
3. **Phase 3**: the gate-lists driver, folding its second set merge onto the shared core.
   The lift, render and append are `devtools/git/mklists`; the second set merge became an `Order` on the shared core rather than a second implementation of it, since a Makefile list is a set make expands and the row-order reconstruction the Markdown registries need would refuse a merge over a reorder that means nothing.
   Also retires the shell merge half of `merge-driver-common.sh`, which had no callers left.

One binary with a subcommand per driver, so the four entry points share one build and one artifact.

## Validation

Per [testing.md](../../development/testing.md#a-bulk-mechanical-change-proves-itself-by-reconciliation-not-by-an-empty-leftover-query), each phase reconciles rather than greps.

- The suite for each ported driver passes **unchanged**, with no edit to the assertions.
- Each ported driver is additionally checked against the shell one it replaces, on the same inputs, before the shell is deleted.
- New Go tests cover what no suite could reach: the order-reconstruction cases, the uncertainty matrix per key, and the round-trip.
- Deleting the mechanism must make the new tests red, per testing.md § Verify a causation claim by deleting the mechanism.

### What that found, Phase 3

The inversions were not a formality in the last phase: two of them came back green and were the finding.

The gate-lists round-trip check reads the rebuilt block's entries back and compares the set.
Deleting the continuation backslash from the render left that check passing, because reading entries back finds them all whether or not the lines are joined — so a block that assigns only its first line round-trips perfectly, and `make` then expands the list to a fraction of itself.
The check now asserts the shape as well as the membership, and the two fail independently.
Measured by running the render bug with and without the check: with it the driver refuses and leaves markers, without it the driver reports the merge resolved.

The awk-versus-Go differential over 400 generated three-way Makefiles agrees on the merged bytes and the exit status in every case but one deliberate class.
An entry listed twice within one side is refused by the Go core and carried silently through by the awk.
`gate-lists-check` does not reject a duplicate either — measured 2026-09-16 by adding one and running the gate, which passed — so the refusal converts an invisible defect into a visible one, and cannot fire on the file as it stands, whose five lists are all distinct.
