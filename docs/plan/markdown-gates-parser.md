# Markdown gates on a real parser

**Status:** open, filed 2026-08-02. Three phases:
[Q612](../STATUS.md#Q612) ✅ → [Q613](../STATUS.md#Q613) → [Q614](../STATUS.md#Q614).

Phase 1 shipped: `devtools/docs/markdown` (the shared parse layer over goldmark)
and `devtools/docs/doclinks` (the checker), with `check-doc-links.sh` kept as the
entry point that selects the files. What the build measured, and what it changed
about the plan below, is in [Phase 1 result](#phase-1-result-q612).

Four gates read Markdown with hand-rolled `awk`: the link/anchor checker, the backlog
linter, the roadmap coherence check, and the backlog metrics replay. Each re-implements
some slice of Markdown with regular expressions, and regular expressions cannot count
brackets. Every defect below is that one root cause.

The failure mode is the expensive one: these gates report **green by not seeing
things**. That is the same class as the path-filter gap (Q400/Q429) where a filter
missing a module made its gate pass by skipping.

## The evidence

### `check-doc-links.sh` is blind to a link shape the README uses today

[`scripts/docs/check-doc-links.sh`](../../scripts/docs/check-doc-links.sh) is 235 lines,
~190 of them a single `awk` program implementing a Markdown parser *and* the
github-slugger algorithm. Of the four gates in this plan it is the **only one with no
`-test.sh` companion** — `lint-backlog`, `check-roadmap`, and `backlog-metrics` all
have one. The most parser-dense gate is the untested one.

Repointing the README's license badge at a nonexistent file left the gate green; the
identical target as a plain link failed it:

```
[![License: Apache 2.0](…badge.svg)](THIS-FILE-DOES-NOT-EXIST.md)
  → check-doc-links: ok (242 files, 5134 links checked)   exit=0

[plain](THIS-FILE-DOES-NOT-EXIST.md)
  → README.md:219: dead link …  FAILED                    exit=1
```

The collection regex `\[[^]]*\]\([^)]*\)` matches the inner **image** first, so the
outer destination is never collected. [`README.md`](../../README.md) line 7 points at
`LICENSE` through exactly this shape — a relative path the gate cannot see.

Measured against the verbatim collection block:

| Input | Collected | Effect |
|---|---|---|
| `[![badge](img)](target)` | the *image* target | dead outer link invisible — **live, 3 in README** |
| `[see [inner]](target)` | *nothing* | link silently skipped entirely |
| `[wiki](docs/a(x).md)` | `docs/a(x` | truncated → false positive |
| setext heading (`===`) | never registered as an anchor | false positive — latent, none in tracked docs |

Blast radius is the highest of the four: it runs in `make check`, in `STATUS_GATES`,
and in its own [`doc-links.yml`](../../.github/workflows/doc-links.yml) workflow.

### `lint-backlog.sh` splits table rows positionally

[`scripts/docs/lint-backlog.sh`](../../scripts/docs/lint-backlog.sh) reads the Queue
with `awk -F'|'` and fixed field indices. One escaped pipe in any cell shifts every
field — measured:

```
| … | Item with a \| pipe | `lbl` | 🔲 | S | short note |   → NF=9  $5=[ `lbl` ]  $7=[ S ]
| … | Item plain          | `lbl` | 🔲 | S | short note |   → NF=8  $5=[ 🔲 ]    $7=[ short note ]
```

`St` then reads the label cell and `Notes` reads the size, so the row's rules evaluate
the wrong cells and pass. Zero occurrences in `STATUS.md` today — this fails *silently
wrong*, not loudly, which is why it is worth closing before it happens rather than
after.

Second defect, latent: `awk`'s `length()` counts **bytes** on this host (BWK awk
20200816) and **runes** under `gawk` in a UTF-8 locale. `STATUS.md` carries 52 em
dashes, 51 🔲, 26 ✅. Measuring every cell exactly as the script extracts it, the
longest is [Q555](../STATUS.md#Q555) at **249 bytes / 249 characters** — one byte of
margin against the 250 cap, and Q640 (measured before it shipped, so no row to link)
tied it on bytes at 249/245.
Nothing diverges today, but rows are routinely authored to fill the budget, and one at
251/249 would pass in one environment and fail in the other.

### Runtime is not the argument

`check-doc-links` 0.67 s, `lint-backlog` 0.49 s, `check-roadmap` 0.27 s. All
sub-second. This plan buys **correctness and testability**, not speed. Nothing here
should be justified on performance.

## The decision: adopt goldmark

Measured, not assumed. A probe against
[`github.com/yuin/goldmark`](https://github.com/yuin/goldmark) with the GFM extension
resolved every defect above:

| Case | `awk` today | goldmark |
|---|---|---|
| `[![badge](img)](target-a.md)` | image target only | `LINK target-a.md` **and** `IMAGE img.png` |
| `[wiki](target-b(x).md)` | truncated | `target-b(x).md` |
| `[see [inner]](target-c.md)` | dropped | `target-c.md`, text `see [inner]` |
| setext heading | no anchor | parsed, `setext-heading-style` |
| table row with `\|` in a cell | 9 fields, cells shift | **6 cells, correct** |

**The slugger question is settled too.** goldmark's `WithAutoHeadingID` agreed with the
current `awk` slugger on **all 13** probed headings drawn from this repo's real docs —
including duplicate de-duplication (`duplicate-1`), emoji stripping, code spans,
punctuation runs (`c--go-100--done`), and hyphen runs. Thirteen cases is not a proof of
equivalence, so the differential validation below is a required step rather than a
nicety — but the hand-written slugger is no longer load-bearing, which is what shrinks
this work.

### Cost, verified

- **Zero external dependencies.** `go.mod` at v1.7.8, v1.7.16, and v1.8.5 has no
  `require` block at all — the smallest possible supply-chain delta for a parser.
- **265 KB** module zip including tests; `go mod vendor` strips tests, so the
  `devtools/vendor` delta (364 KB today) is smaller than that.
- **Wiring already exists.** [`vendor-sync.sh`](../../scripts/go/vendor-sync.sh),
  [`vendor-check.sh`](../../scripts/go/vendor-check.sh), and the `devtools/vendor/**`
  path filter in [`unit-test.yml`](../../.github/workflows/unit-test.yml) all already
  cover the module. No new gate plumbing.
- **Line numbers need an offset→line index.** goldmark nodes carry byte offsets, not
  line numbers, and the gates report `file:line:`. Roughly three lines of Go; called
  out because it is friction the `awk` does not have.

### `THIRD-PARTY-NOTICES` owes nothing — settled

`THIRD-PARTY-NOTICES` is generated from the **root** `vendor/modules.txt` only
([`gen-third-party-notices.sh`](../../scripts/release/gen-third-party-notices.sh)), so
`devtools/` dependencies are not covered — and should not be. Attribution is triggered
by *distributing* a binary, and these gate binaries are never shipped or signed, which
is the same reason `tools/vendor/` is already excluded. Vendoring goldmark is therefore
not a notices change.

Recorded durably in
[building.md](../development/building.md#what-it-covers--and-why-build-time-tooling-does-not)
(with the source-tree and SBOM distinctions) and cross-referenced from
[go-workspaces.md](../development/go-workspaces.md#wiring-a-new-first-party-module), so
the rule outlives this plan's archival. No decision left for Q612.

## Scope

In scope, one phase each:

1. **[Q612](../STATUS.md#Q612) — `check-doc-links.sh`.** Lands `devtools/docs/markdown`
   (the shared parse layer) plus goldmark, and closes the proven defects.
2. **[Q613](../STATUS.md#Q613) — `lint-backlog.sh`.** Table rows via the GFM AST; the
   character cap via `utf8.RuneCountInString`.
3. **[Q614](../STATUS.md#Q614) — `check-roadmap.sh` + `backlog-metrics.sh`.** The
   remaining two consumers, onto the same layer.

Each keeps its `scripts/` entry point, per
[`scripts/README.md`](../../scripts/README.md) — the gate map stays in one place.

### Out of scope, deliberately

[`git-merge-status.sh`](../../scripts/docs/git-merge-status.sh) and
[`merge-status-rows.awk`](../../scripts/lib/merge-status-rows.awk) stay as they are. A
merge driver must reconstruct the file **line for line**, including the conflict-marker
fallback; an AST discards exactly the byte-level fidelity it depends on. Rewriting it
onto goldmark would be actively wrong, not merely unnecessary.

Also out: `check-codegen-drift.sh`, the `chart-*-check.sh` family, `validate-egress-ip.sh`,
`dogfood/setup.sh`, and the `e2e/` scripts. They are long, but they orchestrate external
CLIs (`kubectl`, `helm`, `docker`, `controller-gen`). Shell is the right language;
Go would be `exec.Command` soup. **Length is not the signal — parsing is.**

## Validation

A rewrite that merely passes on the current tree proves nothing: the `awk` gate also
passes, and that is the bug. Per
[testing.md](../development/testing.md#a-bulk-mechanical-change-proves-itself-by-reconciliation-not-by-an-empty-leftover-query),
each phase reconciles rather than greps:

1. **Differential run.** Go gate and `awk` gate over the same tree; every difference
   explained as a fixed defect, not waved through. The corpus is real: 242 files, 5134
   links/anchors.
2. **Red-first on each proven defect.** The four rows in the first table become test
   cases that fail against the old behaviour and pass against the new — including the
   badge-wrapped link, which is the one with live instances.
3. **Slug equivalence.** Every heading in every tracked `.md` slugged both ways and
   reconciled. This is what upgrades "13 probed cases agreed" into a real assertion.
4. **Delete the mechanism.** For each new gate, remove the check and require red on the
   assertion that names it
   ([testing.md](../development/testing.md#verify-a-causation-claim-by-deleting-the-mechanism)).

## Phase 1 result (Q612)

The differential run is the evidence, over the whole tree: 249 files, **5521**
links collected by the `awk` and **5558** by the parser. Every difference
resolved to a fix, and **nothing** the `awk` collected was lost:

| Difference | Count | What it is |
|---|---|---|
| Badge-wrapped outer targets | 3 | The filed defect. `README.md` lines 7–9; `LICENSE` is the relative one, and it resolves — the shape was unchecked, not broken. |
| Link text spanning a line break | 25 | The `awk` matched per line, so a link whose text wrapped was collected by neither half. The largest class, and unfiled. |
| Reference-link *uses* | 9 | `[text][label]` resolved to its destination. The `awk` collected only the definition. |
| Heading inside a blockquote | 1 anchor | `^#{1,6}` never matched it, so links to it read as dead. |
| Inline HTML in a heading | 1 anchor | Slugged from the rendered text, as GitHub does, instead of from the tag source. |
| `<a id="QN">` inside a code span | −3 anchors | Prose *about* an anchor published one. A fixed false positive, not a loss. |

**Two dialect gaps the survey missed.** MkDocs is not CommonMark, and both gaps
would have been silent coverage losses:

- **Admonitions.** A `!!! note` body is four-space-indented, so a stock parser
  reads it as an indented code block — 15 links across three pages, all
  checked before.
- **`md_in_html`.** A `<p markdown="span">…</p>` element is raw HTML to
  CommonMark — 4 more links.

Both are now parsed by `MkDocsDialect`, a goldmark extension in the parse layer,
which is where they belong: the other three gates read the same docs.

**The hand-written slugger stayed**, ported to Go and unit-tested, rather than
being replaced by goldmark's `WithAutoHeadingID`. The gate's contract is
GitHub's anchors, and goldmark keeps Unicode letters GitHub drops. Cost of
keeping it: ~30 lines. Slug reconciliation over the corpus is above — 3333
headings, the only two differences being fixes.

**Cost, corrected.** `devtools/vendor` grew 364 KB → 956 KB, not the "smaller
than 265 KB" the module-zip figure predicted: `go mod vendor` keeps the GFM
extension and the renderer packages. Runtime 0.67 s → 0.96 s, both sub-second,
which is still not the argument. The checker is built to `.build/` and exec'd
rather than `go run`, because its exit status is the verdict and `go run` prints
an `exit status 1` line of its own on top of the findings.

**The blind spot was measured, not predicted** ([Q622's
method](../development/testing.md#verify-a-causation-claim-by-deleting-the-mechanism)):
repointing README line 7's badge link at a nonexistent target left the `awk`
gate green (exit 0, no finding) and failed the new one at `README.md:7`. The
control — the same dead target as a plain link — failed both, so the difference
is the shape and not the target.

## Sequencing

Q612 lands the shared package, so Q613 and Q614 are blocked on it — recorded as `🚫` in
the Queue with the `Blocked by` prefix `make queue-unblock` reads. The ordering is also
the value ordering: Q612 fixes defects with live instances, Q613 and Q614 close latent
ones.
