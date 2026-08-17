# Markdown gates on a real parser

**Status:** filed 2026-08-02; all three phases shipped by 2026-08-04.
Q612 ✅ → Q613 ✅ → Q614 ✅.
Q654 shipped alongside them: an em-dash density gate on the same parse layer, filed after the phases were scoped.
Only the Progress row still references this plan, so archiving it is the milestone's call rather than any one phase's.

Phase 1 shipped: `devtools/docs/markdown` (the shared parse layer over goldmark) and `devtools/docs/doclinks` (the checker), with `check-doc-links.sh` kept as the entry point that selects the files.
What the build measured, and what it changed about the plan below, is in [Phase 1 result](#phase-1-result-q612).

Phase 2 shipped: `devtools/docs/backloglint`, plus `Tables()` on the parse layer.
[Phase 2 result](#phase-2-result-q613).

Four gates read Markdown with hand-rolled `awk`: the link/anchor checker, the backlog linter, the roadmap coherence check, and the backlog metrics replay.
Each re-implements some slice of Markdown with regular expressions, and regular expressions cannot count brackets.
Every defect below is that one root cause.

The failure mode is the expensive one: these gates report **green by not seeing things**.
That is the same class as the path-filter gap (Q400/Q429) where a filter missing a module made its gate pass by skipping.

## The evidence

### `check-doc-links.sh` is blind to a link shape the README uses today

[`scripts/docs/check-doc-links.sh`](../../scripts/docs/check-doc-links.sh) is 235 lines, ~190 of them a single `awk` program implementing a Markdown parser *and* the github-slugger algorithm.
Of the four gates in this plan it is the **only one with no `-test.sh` companion**: `lint-backlog`, `check-roadmap`, and `backlog-metrics` all have one.
The most parser-dense gate is the untested one.

Repointing the README's license badge at a nonexistent file left the gate green; the identical target as a plain link failed it:

```
[![License: Apache 2.0](…badge.svg)](THIS-FILE-DOES-NOT-EXIST.md)
  → check-doc-links: ok (242 files, 5134 links checked)   exit=0

[plain](THIS-FILE-DOES-NOT-EXIST.md)
  → README.md:219: dead link …  FAILED                    exit=1
```

The collection regex `\[[^]]*\]\([^)]*\)` matches the inner **image** first, so the outer destination is never collected.
[`README.md`](../../README.md) line 7 points at `LICENSE` through exactly this shape, a relative path the gate cannot see.

Measured against the verbatim collection block:

| Input | Collected | Effect |
|---|---|---|
| `[![badge](img)](target)` | the *image* target | dead outer link invisible; **live, 3 in README** |
| `[see [inner]](target)` | *nothing* | link silently skipped entirely |
| `[wiki](docs/a(x).md)` | `docs/a(x` | truncated → false positive |
| setext heading (`===`) | never registered as an anchor | false positive; latent, none in tracked docs |

Blast radius is the highest of the four: it runs in `make check`, in `STATUS_GATES`, and in its own [`doc-links.yml`](../../.github/workflows/doc-links.yml) workflow.

### `lint-backlog.sh` splits table rows positionally

[`scripts/docs/lint-backlog.sh`](../../scripts/docs/lint-backlog.sh) reads the Queue with `awk -F'|'` and fixed field indices.
One escaped pipe in any cell shifts every field, measured:

```
| … | Item with a \| pipe | `lbl` | 🔲 | S | short note |   → NF=9  $5=[ `lbl` ]  $7=[ S ]
| … | Item plain          | `lbl` | 🔲 | S | short note |   → NF=8  $5=[ 🔲 ]    $7=[ short note ]
```

`St` then reads the label cell and `Notes` reads the size, so the row's rules evaluate the wrong cells and pass.
This fails *silently wrong*, not loudly, which is why it is worth closing before it happens rather than after.
Filed as latent: "zero occurrences in `STATUS.md` today", which the build disproved: Q625 carried one, and the gate was reading 13 characters of a 243-character cell ([Phase 2 result](#phase-2-result-q613)).

Second defect, latent: `awk`'s `length()` counts **bytes** on this host (BWK awk 20200816) and **runes** under `gawk` in a UTF-8 locale.
`STATUS.md` carries 52 em dashes, 51 🔲, 26 ✅.
Measuring every cell exactly as the script extracts it, the longest is [Q555](../queue/Q555.md) at **249 bytes / 249 characters** — one byte of margin against the 250 cap, and Q640 (measured before it shipped, so no row to link) tied it on bytes at 249/245.
Nothing diverges today, but rows are routinely authored to fill the budget, and one at 251/249 would pass in one environment and fail in the other.

### Runtime is not the argument

`check-doc-links` 0.67 s, `lint-backlog` 0.49 s, `check-roadmap` 0.27 s.
All sub-second.
This plan buys **correctness and testability**, not speed.
Nothing here should be justified on performance.

## The decision: adopt goldmark

Measured, not assumed.
A probe against [`github.com/yuin/goldmark`](https://github.com/yuin/goldmark) with the GFM extension resolved every defect above:

| Case | `awk` today | goldmark |
|---|---|---|
| `[![badge](img)](target-a.md)` | image target only | `LINK target-a.md` **and** `IMAGE img.png` |
| `[wiki](target-b(x).md)` | truncated | `target-b(x).md` |
| `[see [inner]](target-c.md)` | dropped | `target-c.md`, text `see [inner]` |
| setext heading | no anchor | parsed, `setext-heading-style` |
| table row with `\|` in a cell | 9 fields, cells shift | **6 cells, correct** |

**The slugger question is settled too.** goldmark's `WithAutoHeadingID` agreed with the current `awk` slugger on **all 13** probed headings drawn from this repo's real docs — including duplicate de-duplication (`duplicate-1`), emoji stripping, code spans, punctuation runs (`c--go-100--done`), and hyphen runs.
Thirteen cases is not a proof of equivalence, so the differential validation below is a required step rather than a nicety — but the hand-written slugger is no longer load-bearing, which is what shrinks this work.

### Cost, verified

- **Zero external dependencies.** `go.mod` at v1.7.8, v1.7.16, and v1.8.5 has no `require` block at all, the smallest possible supply-chain delta for a parser.
- **265 KB** module zip including tests; `go mod vendor` strips tests, so the `devtools/vendor` delta (364 KB today) is smaller than that.
- **Wiring already exists.** [`vendor-sync.sh`](../../scripts/go/vendor-sync.sh), [`vendor-check.sh`](../../scripts/go/vendor-check.sh), and the `devtools/vendor/**` path filter in [`unit-test.yml`](../../.github/workflows/unit-test.yml) all already cover the module.
  No new gate plumbing.
- **Line numbers need an offset→line index.** goldmark nodes carry byte offsets, not line numbers, and the gates report `file:line:`.
  Roughly three lines of Go; called out because it is friction the `awk` does not have.

### `THIRD-PARTY-NOTICES` owes nothing — settled

`THIRD-PARTY-NOTICES` is generated from the **root** `vendor/modules.txt` only ([`gen-third-party-notices.sh`](../../scripts/release/gen-third-party-notices.sh)), so `devtools/` dependencies are not covered, and should not be.
Attribution is triggered by *distributing* a binary, and these gate binaries are never shipped or signed, which is the same reason `tools/vendor/` is already excluded.
Vendoring goldmark is therefore not a notices change.

Recorded durably in [building.md](../development/building.md#what-it-covers--and-why-build-time-tooling-does-not) (with the source-tree and SBOM distinctions) and cross-referenced from [go-workspaces.md](../development/go-workspaces.md#wiring-a-new-first-party-module), so the rule outlives this plan's archival.
No decision left for Q612.

## Scope

In scope, one phase each:

1. **Q612 — `check-doc-links.sh`.** ✅ Lands `devtools/docs/markdown` (the shared parse layer) plus goldmark, and closes the proven defects.
   Result: [Phase 1 result](#phase-1-result-q612).
2. **Q613 ✅ — `lint-backlog.sh`.** Table rows via the GFM AST; the character cap via `utf8.RuneCountInString`.
3. **Q614 — `check-roadmap.sh` + `backlog-metrics.sh`.** ✅ The remaining two consumers, onto the same layer.
   Result: [Phase 3 result](#phase-3-result-q614).

Each keeps its `scripts/` entry point, per [`scripts/README.md`](../../scripts/README.md): the gate map stays in one place.

### Out of scope, deliberately

[`git-merge-status.sh`](../../scripts/docs/git-merge-status.sh) and [`merge-keyed-records.awk`](../../scripts/lib/merge-keyed-records.awk) stay as they are, as does the [`git-merge-plan-index.sh`](../../scripts/docs/git-merge-plan-index.sh) sibling.
A merge driver must reconstruct the file **line for line**, including the conflict-marker fallback; an AST discards exactly the byte-level fidelity it depends on.
Rewriting it onto goldmark would be actively wrong, not merely unnecessary.

Also out: `check-codegen-drift.sh`, the `chart-*-check.sh` family, `validate-egress-ip.sh`, `dogfood/setup.sh`, and the `e2e/` scripts.
They are long, but they orchestrate external CLIs (`kubectl`, `helm`, `docker`, `controller-gen`).
Shell is the right language; Go would be `exec.Command` soup.
**Length is not the signal; parsing is.**

## Validation

A rewrite that merely passes on the current tree proves nothing: the `awk` gate also passes, and that is the bug.
Per [testing.md](../development/testing.md#a-bulk-mechanical-change-proves-itself-by-reconciliation-not-by-an-empty-leftover-query), each phase reconciles rather than greps:

1. **Differential run.** Go gate and `awk` gate over the same tree; every difference explained as a fixed defect, not waved through.
   The corpus is real: 242 files, 5134 links/anchors.
2. **Red-first on each proven defect.** The four rows in the first table become test cases that fail against the old behaviour and pass against the new, including the badge-wrapped link, which is the one with live instances.
3. **Slug equivalence.** Every heading in every tracked `.md` slugged both ways and reconciled.
   This is what upgrades "13 probed cases agreed" into a real assertion.
4. **Delete the mechanism.** For each new gate, remove the check and require red on the assertion that names it ([testing.md](../development/testing.md#verify-a-causation-claim-by-deleting-the-mechanism)).

## Phase 1 result (Q612)

The differential run is the evidence, over the whole tree: 249 files, **5521** links collected by the `awk` and **5558** by the parser.
Every difference resolved to a fix, and **nothing** the `awk` collected was lost:

| Difference | Count | What it is |
|---|---|---|
| Badge-wrapped outer targets | 3 | The filed defect. `README.md` lines 7–9; `LICENSE` is the relative one, and it resolves — the shape was unchecked, not broken. |
| Link text spanning a line break | 25 | The `awk` matched per line, so a link whose text wrapped was collected by neither half. The largest class, and unfiled. |
| Reference-link *uses* | 9 | `[text][label]` resolved to its destination. The `awk` collected only the definition. |
| Heading inside a blockquote | 1 anchor | `^#{1,6}` never matched it, so links to it read as dead. |
| Inline HTML in a heading | 1 anchor | Slugged from the rendered text, as GitHub does, instead of from the tag source. |
| `<a id="QN">` inside a code span | −3 anchors | Prose *about* an anchor published one. A fixed false positive, not a loss. |

**Two dialect gaps the survey missed.** MkDocs is not CommonMark, and both gaps would have been silent coverage losses:

- **Admonitions.** A `!!! note` body is four-space-indented, so a stock parser reads it as an indented code block: 15 links across three pages, all checked before.
- **`md_in_html`.** A `<p markdown="span">…</p>` element is raw HTML to CommonMark: 4 more links.

Both are now parsed by `MkDocsDialect`, a goldmark extension in the parse layer, which is where they belong: the other three gates read the same docs.

**The hand-written slugger stayed**, ported to Go and unit-tested, rather than being replaced by goldmark's `WithAutoHeadingID`.
The gate's contract is GitHub's anchors, and goldmark keeps Unicode letters GitHub drops.
Cost of keeping it: ~30 lines.
Slug reconciliation over the corpus is above: 3333 headings, the only two differences being fixes.

**Cost, corrected.** `devtools/vendor` grew 364 KB → 956 KB, not the "smaller than 265 KB" the module-zip figure predicted: `go mod vendor` keeps the GFM extension and the renderer packages.
Runtime 0.67 s → 0.96 s, both sub-second, which is still not the argument.
The checker is built to `.build/` and exec'd rather than `go run`, because its exit status is the verdict and `go run` prints an `exit status 1` line of its own on top of the findings.

**The blind spot was measured, not predicted** ([Q622's method](../development/testing.md#verify-a-causation-claim-by-deleting-the-mechanism)): repointing README line 7's badge link at a nonexistent target left the `awk` gate green (exit 0, no finding) and failed the new one at `README.md:7`.
The control — the same dead target as a plain link — failed both, so the difference is the shape and not the target.

## Phase 2 result (Q613)

The rules moved to `devtools/docs/backloglint`; `Tables()` on the parse layer is the only shared-package addition.
The script kept the file selection and the environment interface (`NOTES_MAX_CHARS`, `BACKLOG_ALLOW_*`), mapping them onto flags: 518 lines to 92.

**The reconciliation is the evidence.** `lint-backlog-test.sh` grew from 53 to 67 cases, covering all 12 rules, and `LINT_BACKLOG_BIN` points the whole suite at another implementation so both ran over the same corpus.
The `awk` passed 62 and failed 5; the rewrite passed 67.
Every one of the five disagreements is one of this plan's two filed defects, and each is paired with a control that agreed:

| Case | `awk` | Go | Why they differ |
|---|---|---|---|
| Over-cap Notes holding `\|` | pass | **fail** | The cap measured the stub before the escape |
| Control: same length, no escape | fail | fail | n/a |
| `\|` in the Item cell, `St` 🔲 | **fail** | pass | `St` was read from the Labels cell |
| Control: same Item, no escape | pass | pass | n/a |
| `\|` as the last two characters | pass | **fail** | The trailing `\` was the whole cell |
| 250 em dashes (750 bytes) | **fail** | pass | Cap counted bytes |
| Control: 251 em dashes | fail | fail | Over on either scale |
| 200 em dashes, no doc link | **fail** | pass | Link threshold counted bytes |

Over a 48 KB copy of the real `STATUS.md` carrying one instance each of rules 1, 2, 3, 5, 7 and 11, both produce the **same six findings, worded identically** — the new ones additionally carry `:LINE:`.
Rules 8, 9 and 10 are asserted through throwaway repos in the suite, and rule 8's positive case (a `flake` row actually deleted) was **untested before this change**: only the branch-behind-`main` negative was.

Rule 12 (a new row's ID holds a `refs/queue-ids` claim) landed on `main` while this was in flight and was ported the same way.
Its six cases — written against the `awk`, not against this — pass unchanged, which is the check that matters: a rule dropped in a port disarms a gate that still reports green.

**The escaped-pipe defect had a live instance.** Q625's Notes carry `` `make check \| tail` ``, so the `awk` read a 13-character cell where the row holds 243.
Pushing that cell to 264 characters left the `awk` green; the rewrite fails it at `docs/STATUS.md:77`.
The control — the same overflow on Q631, which has no escaped pipe — failed both, so the difference is the shape and not the row.

**Which `awk` counts what, measured.** `length()` on `"é—🔲"` answers 9 under this host's BWK awk 20200816 and under mawk 1.3.4, and 3 under GNU awk 5.2.1 in a UTF-8 locale — bytes and runes respectively (`LC_ALL=C` puts gawk back on bytes).
CI runs the byte one: `ubuntu-latest` inherits Ubuntu 24.04's `/usr/bin/awk` → mawk 1.3.4, and the runner image's apt manifest adds no awk of its own.
So the gate counted bytes in both places it actually ran, and the divergence was one `brew install gawk` away from arriving on a laptop.

**No row was reclassified.** All 109 Queue and Deferred cells re-measured: the longest is [Q555](../queue/Q555.md) at 249 bytes / 249 characters, confirming the filed figure, and the largest byte-vs-rune gap on any row is 9 (Q525, 215 B / 206 chars).
Nothing is over cap on either scale, and runes ≤ bytes always, so moving to characters can only relax, never break, an existing row.

**One workflow gap closed.** `status-lint.yml` gated on `scripts/docs/lint-backlog.sh` alone, which no longer holds the rules; it now also triggers on `devtools/**`, pins the toolchain with `setup-go`, and takes 5 minutes instead of 2 for the build.
Runtime 0.49 s → 0.69 s, still sub-second, still not the argument.

## Phase 3 result (Q614)

Both gates keep their `scripts/` entry point and gain a Go checker, `devtools/docs/roadmapcheck` and `devtools/docs/backlogmetrics`, over the same parse layer.
No new module, no new `make` target, no new path filter: the `devtools/**` filters Q612 wired already cover them.

**The parse layer grew three general things**, not one-caller helpers: `Tables()` gains a `Text` reading of each cell beside Q613's source reading, `TopLevelListItems()` reports a bullet's text, its lead bold run, its HTML comments and whether it links, and `SectionRange()` gives the lines a heading's section spans.
Plus `ParseRow()`, for the case Q612 did not have: **a table row with no table around it.** The metrics replay reads `git log -p` output, where every row is a lone `+`/`-` diff line.
GFM only recognizes a table whose delimiter is at least as wide as its header, and pads a narrower header out, so acceptance is monotone in the width and the narrowest accepted width *is* the row's cell count.
`ParseRow` binary-searches it, which leaves every escaping rule to goldmark instead of restating it.

**`check-roadmap` reconciled** to the same verdict and the same counts (18 bullets, 57 features), with byte-identical output.
Every bullet's annotation IDs and link flag match.
Word counts all fell, by 5 to 24 on the roadmap and by exactly 1 on `features.md`:

| Difference | What it is |
|---|---|
| −1 on every bullet | The `- ` marker, which `split()` counted as a word. |
| −1 per wrapped line | An indented continuation line yields an empty leading field. |
| −19 on `roadmap.md:43` | The `awk` ran a bullet's span to the next bullet or heading, so the paragraph *between* two bullets was charged to the one above it. |

Nothing crossed either cap under either counter, so nothing changed hands.
The caps now mean what they say: 60 real words, not ~54 plus markup.

**`backlog-metrics` reconciled over the whole history**, 2898 diff rows and 631 items.
Identical filed dates and sizes; one event differs and 156 titles do:

| Difference | Count | What it is |
|---|---|---|
| Q129 `pruned` 06-18 → `completed` 06-16 | 1 | The Q509 defect, one shape further out. The commit that moved Q129 to Progress buried `<a id="Q129"></a>Q129` mid-Notes, and the `awk`'s line-wide regex read that as re-adding the row, so the removal went unrecorded and a later commit booked it as a prune. Cell-scoped, a Progress row has no cell that reads as a bare ID. |
| Backticks, `*emphasis*`, a literal `[` | 156 | Titles are rendered text now, so `` `windowStartTime` `` reads as `windowStartTime` and Q485's `sizingRecommendation[]` survives; the `awk` stripped every `[` to undo link markup and ate that one. Widens the 60-char aging-report window. |

**One rule had to loosen, not tighten.** The ID cell is *searched for*, not pinned to index 0: history holds rows a botched edit prefixed with a stray delimiter (`|---|---|---|---|| <a id="Q166">…`).
Pinning index 0 dropped that add, which booked a removal for a row that never left.
The reconciliation caught it; review would not have.

**Red-first on the new shapes**, each failing against the `awk` and passing against the parser: a `<!-- q:QN -->` inside a code fence (old gate green on a bullet with no real annotation), prose after a bullet counted against it, an escaped pipe truncating a row's title, and a fenced example row counted as parked in Deferred.
The line-break case is a positive **control**: it passes both, and pins that excluding the following paragraph did not also exclude the bullet's own continuation lines.

**A boundary the port does not cross, now pinned:** the replay reads diff lines, which carry no document around them, so a row inside a fence still registers as an arrival there even though the same fence is respected when the file itself is parsed for Deferred IDs.
Asserted so a change to it is a deliberate one, since every metric moves with it.

`backlog-metrics.sh` started as the [`session-backlog`](../development/skills.md#session-backlog) skill's script.
It has now diverged; `scripts/README.md` says so rather than implying a sync obligation.

## Sequencing

Q612 lands the shared package, so Q613 and Q614 are blocked on it — recorded as `🚫` in the Queue with the `Blocked by` prefix `make queue-unblock` reads.
The ordering is also the value ordering: Q612 fixes defects with live instances, Q613 and Q614 close latent ones.
