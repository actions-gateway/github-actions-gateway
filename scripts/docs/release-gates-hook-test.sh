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
# guards the other direction — a STATUS.md format change that makes every lookup
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

with open(sys.argv[2], encoding="utf-8") as handle:
    status = handle.read()
with open(sys.argv[3], encoding="utf-8") as handle:
    page = handle.read()
sys.stdout.write(hook.render(page, hook.gates(status)))
PY

# status ROW... — write a STATUS.md whose Queue holds the given `ID:labels`
# entries, labels comma-separated and rendered backticked the way the real table
# does. The backticks are what separate a label from a mention of one.
status() {
	local entry labels file="$WORKDIR/STATUS.md"
	{
		printf '# Project Status\n\n## Queue\n\n'
		printf '| ID | Item | Labels | St | Sz | Notes |\n|---|---|---|---|---|---|\n'
		for entry in "$@"; do
			labels="${entry#*:}"
			# shellcheck disable=SC2016 # backticks are Markdown here, not substitution
			printf '| <a id="%s"></a>%s | Thing | `%s` | 🔲 | S | note |\n' \
				"${entry%%:*}" "${entry%%:*}" "${labels//,/\` \`}"
		done
	} >"$file"
	printf '%s\n' "$file"
}

# expect NAME WANT_SUBSTRING_OR_'' PAGE_BODY -- STATUS_ROW...
#
# Renders PAGE_BODY against a STATUS.md holding STATUS_ROW..., then asserts the
# output does (or, with an empty WANT, does not) carry a release chip.
expect() {
	local name="$1" want="$2" body="$3" statusfile got
	shift 3
	[[ "${1:-}" == "--" ]] && shift
	statusfile="$(status "$@")"
	printf '%s\n' "$body" >"$WORKDIR/page.md"
	got="$(python3 "$WORKDIR/drive.py" "$HOOK" "$statusfile" "$WORKDIR/page.md")"
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
# Both write STATUS.md by hand, because the shapes under test are ones the
# fixture helper cannot produce.

drift() {
	local name="$1" want="$2" statusbody="$3" page="$4" got
	printf '%s\n' "$statusbody" >"$WORKDIR/STATUS.md"
	printf '%s\n' "$page" >"$WORKDIR/page.md"
	got="$(python3 "$WORKDIR/drive.py" "$HOOK" "$WORKDIR/STATUS.md" "$WORKDIR/page.md")"
	if [[ "$want" == "none" && "$got" != *gag-release-chip* ]] ||
		[[ "$want" != "none" && "$got" == *"$want"* ]]; then
		printf 'ok   %s\n' "$name"
	else
		printf 'FAIL %s: want=%q got=%q\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

# The Labels column is located from the header, so a column inserted ahead of it
# moves the read rather than silently pointing it at the wrong cell.
# shellcheck disable=SC2016 # backticks below are Markdown, not substitution
drift 'the Labels column is found by header, not position' '>1.5<' \
	'## Queue

| ID | Owner | Item | Labels | St |
|---|---|---|---|---|
| <a id="Q1"></a>Q1 | nobody | Thing | `1.5-gate` | 🔲 |' \
	'- **Thing** <!-- q:Q1 -->'

# A gate label quoted in a Notes cell is a mention, not a commitment.
# shellcheck disable=SC2016 # ditto
drift 'a gate named in Notes is not a label' none \
	'## Queue

| ID | Item | Labels | Notes |
|---|---|---|---|
| <a id="Q1"></a>Q1 | Thing | `feature` | dropped its `1.5-gate` label |' \
	'- **Thing** <!-- q:Q1 -->'

# --- the real tree ------------------------------------------------------------
# A lookup that stops matching renders a chipless page, which is indistinguishable
# from "nothing is gated" by eye. Assert the real backlog still resolves gates.
resolved="$(python3 "$HOOK" "$REPO_ROOT/docs" | wc -l | tr -d ' ')"
if [[ "$resolved" -gt 0 ]]; then
	printf 'ok   docs/STATUS.md still resolves %s gated row(s)\n' "$resolved"
else
	printf 'FAIL docs/STATUS.md resolved no gated rows — the table format changed?\n' >&2
	fails=$((fails + 1))
fi

if ((fails > 0)); then
	printf '\nrelease-gates-hook-test: %d failure(s)\n' "$fails" >&2
	exit 1
fi
printf '\nrelease-gates-hook-test: ok\n'
