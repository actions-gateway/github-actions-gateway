#!/usr/bin/env bash
#
# Unit tests for hooks/release_gates.py — the MkDocs hook that renders each
# roadmap bullet's release commitment as a chip derived from the backlog's
# `X.Y-gate` labels (Q770).
#
# The whole point of deriving the chip is that the page cannot promise a release
# the backlog does not, so the cases that matter are the ones where a chip must
# NOT appear: an ungated row, a shipped row, a label mentioned in prose. A
# renderer that emitted a chip for those would be exactly as wrong as the
# hand-typed sentence it replaced, and just as quiet about it. The last case
# guards the other direction — a store format change that makes every lookup
# miss would otherwise render a chipless page that looks intentional.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
#
# python3 is an extended-tier prerequisite (scripts/ci/check-tools.sh), not a
# required one, so this skips rather than fails when it is absent. CI runners
# always have it.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
readonly REPO_ROOT
readonly HOOK="$REPO_ROOT/hooks/release_gates.py"

if ! command -v python3 >/dev/null 2>&1; then
	printf 'skip release-gates-hook-test: python3 not found (extended tier, scripts/ci/check-tools.sh)\n'
	exit 0
fi

WORKDIR="$(mktemp -d)"
readonly WORKDIR
trap 'rm -rf "$WORKDIR"' EXIT

fails=0

# The hook exposes gates() and render() so a case can drive the transform
# without an MkDocs Page; on_page_markdown is the thin file-reading wrapper over
# exactly this pair.
cat >"$WORKDIR/drive.py" <<'PY'
import importlib.util
import sys

spec = importlib.util.spec_from_file_location("release_gates", sys.argv[1])
hook = importlib.util.module_from_spec(spec)
spec.loader.exec_module(hook)

with open(sys.argv[3], encoding="utf-8") as handle:
    page = handle.read()
sys.stdout.write(hook.render(page, hook.gates(sys.argv[2])))
PY

# store ENTRY... — write a store holding the given `ID:labels` entries, labels
# comma-separated and written as the YAML block list `queue.py migrate` emits.
store() {
	local entry id label dir="$WORKDIR/queue"
	rm -rf "$dir"
	mkdir -p "$dir"
	for entry in "$@"; do
		id="${entry%%:*}"
		{
			printf -- '---\nid: %s\nrank: m\nlabels:\n' "$id"
			# shellcheck disable=SC2086 # the comma split is the point
			for label in ${entry#*:}; do printf -- '    - %s\n' "$label"; done
			printf -- 'status: ready\nsize: S\n---\n\n# Thing\n\nnote\n'
		} >"$dir/$id.md"
	done
	printf '%s\n' "$dir"
}

# expect NAME WANT_SUBSTRING_OR_'' PAGE_BODY -- ITEM...
#
# Renders PAGE_BODY against a store holding ITEM..., then asserts the output
# does (or, with an empty WANT, does not) carry a release chip.
expect() {
	local name="$1" want="$2" body="$3" storedir got entries=()
	shift 3
	[[ "${1:-}" == "--" ]] && shift
	for e in "$@"; do entries+=("${e%%:*}:${e#*:}"); done
	storedir="$(store "${entries[@]//,/ }")"
	printf '%s\n' "$body" >"$WORKDIR/page.md"
	got="$(python3 "$WORKDIR/drive.py" "$HOOK" "$storedir" "$WORKDIR/page.md")"
	if [[ -z "$want" ]]; then
		if [[ "$got" != *gag-release-chip* ]]; then
			printf 'ok   %s\n' "$name"
		else
			printf 'FAIL %s: wanted no chip, got=%q\n' "$name" "$got" >&2
			fails=$((fails + 1))
		fi
		return
	fi
	if [[ "$got" == *"$want"* ]]; then
		printf 'ok   %s\n' "$name"
	else
		printf 'FAIL %s: want=%q got=%q\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

expect 'a gated row renders its release' \
	'<span class="gag-release-chip" title="Blocks the 1.5 release">1.5</span> <!-- q:Q1 -->' \
	'- **Thing** <!-- q:Q1 -->' -- 'Q1:feature,1.5-gate'

expect 'an ungated row renders no chip' '' \
	'- **Thing** <!-- q:Q1 -->' -- 'Q1:feature'

# A Q-ID with no row shipped, which is roadmapcheck rule 2's finding. Inventing
# a chip here would hide it behind a plausible-looking pill.
expect 'a shipped row renders no chip' '' \
	'- **Thing** <!-- q:Q9 -->' -- 'Q1:feature,1.5-gate'

expect 'two gates render lowest first' \
	'>1.5</span><span class="gag-release-chip" title="Blocks the 2.0 release">2.0</span>' \
	'- **Thing** <!-- q:Q1,Q2 -->' -- 'Q1:2.0-gate' 'Q2:1.5-gate'

# 1.10 sorts below 1.9 as a string; the chip order is numeric.
expect '1.10 outranks 1.9' \
	'>1.9</span><span class="gag-release-chip" title="Blocks the 1.10 release">1.10</span>' \
	'- **Thing** <!-- q:Q1,Q2 -->' -- 'Q1:1.10-gate' 'Q2:1.9-gate'

expect 'the same gate on two rows renders once' \
	'>1.5</span> <!-- q:Q1,Q2 -->' \
	'- **Thing** <!-- q:Q1,Q2 -->' -- 'Q1:1.5-gate' 'Q2:1.5-gate'

expect 'the chip lands after the bold label, not mid-sentence' \
	'- **Thing** <span class="gag-release-chip" title="Blocks the 1.5 release">1.5</span> <!-- q:Q1 --> and then prose' \
	'- **Thing** <!-- q:Q1 --> and then prose' -- 'Q1:1.5-gate'

# An annotation inside a fence is prose about the format — the reading
# roadmapcheck takes, so the gate and the renderer agree on what counts.
expect 'a fenced annotation is left alone' '' \
	'```
- **Thing** <!-- q:Q1 -->
```' -- 'Q1:1.5-gate'

expect 'a fence that closed still renders' \
	'gag-release-chip' \
	'```
example
```
- **Thing** <!-- q:Q1 -->' -- 'Q1:1.5-gate'

# --- format-drift guards ------------------------------------------------------
# Both write an item by hand, because the shapes under test are ones the fixture
# helper cannot produce.

drift() {
	local name="$1" want="$2" item="$3" page="$4" got dir="$WORKDIR/drift"
	rm -rf "$dir"
	mkdir -p "$dir"
	printf '%s\n' "$item" >"$dir/Q1.md"
	printf '%s\n' "$page" >"$WORKDIR/page.md"
	got="$(python3 "$WORKDIR/drive.py" "$HOOK" "$dir" "$WORKDIR/page.md")"
	if [[ "$want" == "none" && "$got" != *gag-release-chip* ]] ||
		[[ "$want" != "none" && "$got" == *"$want"* ]]; then
		printf 'ok   %s\n' "$name"
	else
		printf 'FAIL %s: want=%q got=%q\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

# `migrate` writes a block list; a hand-filed item may use the inline form. Both
# are the store's, so the hook reads both.
drift 'an inline label list is read' '>1.5<' \
	'---
id: Q1
rank: m
labels: [feature, 1.5-gate]
status: ready
size: S
---

# Thing' \
	'- **Thing** <!-- q:Q1 -->'

# A gate named in the body is a mention, not a commitment. This is the job the
# backticks did when labels lived in a table cell, and the frontmatter boundary
# does it now — so the body below carries the label in every shape it could.
# shellcheck disable=SC2016 # the backticked label below is Markdown, not substitution
drift 'a gate named in the body is not a label' none \
	'---
id: Q1
rank: m
labels:
    - feature
status: ready
size: S
---

# Thing

Dropped its 1.5-gate label, and `1.5-gate` with it.' \
	'- **Thing** <!-- q:Q1 -->'

# Frontmatter only. A labels key further down the body is body text, and reading
# it would reopen exactly the mention-versus-commitment hole above.
# shellcheck disable=SC2016 # the backticked label below is Markdown, not substitution
drift 'a labels block outside the frontmatter is not read' none \
	'---
id: Q1
rank: m
status: ready
size: S
---

# Thing

labels:
    - 1.5-gate' \
	'- **Thing** <!-- q:Q1 -->'

# --- the real tree ------------------------------------------------------------
# A lookup that stops matching renders a chipless page, which is indistinguishable
# from "nothing is gated" by eye. Assert the real backlog still resolves gates.
resolved="$(python3 "$HOOK" "$REPO_ROOT/docs" | wc -l | tr -d ' ')"
if [[ "$resolved" -gt 0 ]]; then
	printf 'ok   docs/queue/ still resolves %s gated item(s)\n' "$resolved"
else
	printf 'FAIL docs/queue/ resolved no gated items — the store format changed?\n' >&2
	fails=$((fails + 1))
fi

if ((fails > 0)); then
	printf '\nrelease-gates-hook-test: %d failure(s)\n' "$fails" >&2
	exit 1
fi
printf '\nrelease-gates-hook-test: ok\n'
