#!/usr/bin/env bash
# check-release-notes.sh — the release-body rules a machine can settle.
#
#   scripts/release/check-release-notes.sh [note.md ...]   # default: docs/releases/v*.md
#
# Exit 0 when every note passes, 1 on a finding, 2 on a usage error.
#
# Scope is deliberately narrow. Most of what the runbook asks of a release note is
# judgement — whether a caveat is a landmine, whether a claim still holds — and a
# gate that guessed at those would fail good notes and train its reader to ignore
# it. See release.md § Checks that stay human for the two that were measured and
# rejected on exactly that ground.
#
# What is left is genuinely mechanical, and each rule below is a defect the
# runbook records as having shipped:
#
#   - A `# ` heading duplicates the Releases page's own <h1>, which is the tag.
#   - An in-page anchor is dead: release-body headings carry no id, so `](#…)`
#     renders as a link that goes nowhere.
#   - Images are tagged `vX.Y.Z` and charts `X.Y.Z`, so a copy-pasteable helm
#     command carrying the `v` fails for the reader who trusts it.
#
# Index truncation is reported, never failed, and what it reports is deliberately
# NOT collapsed height.
#
# Measured 2026-08-15 against the live Releases index: every stable release this
# project has cut is truncated behind "Read more" — v1.3.0, v1.4.0 and v1.5.0
# alike — and the cut lands around 9k of source, before which the body is served
# and after which it is genuinely absent from the page, not merely hidden.
#
# So folding is not the lever the runbook assumed. v1.5.0 carries six folds and
# every one of them begins past the cut, which is why adding a seventh would
# change nothing: the reader is already looking at the first 9k. Only content
# ABOVE the cut is visible, so what this reports is how much of the body sits
# there and whether any fold is positioned early enough to matter.
#
# Truncation is normal and not a defect. It is worth measuring because it decides
# what a reader sees before clicking, which is where the danger banner and the
# upgrade steps need to be.
set -euo pipefail
shopt -s inherit_errexit

# Where the Releases index stopped serving the body, measured across the three
# stable notes: ~8,955 bytes for v1.5.0 and ~9,387 for v1.4.0. A source-byte proxy
# for whatever rendered budget GitHub actually applies, so it is reported as
# approximate and never enforced.
INDEX_CUT_BYTES=9000

usage() {
	cat >&2 <<-EOF
		usage: $(basename "$0") [note.md ...]

		  Defaults to docs/releases/v*.md. Fails on the release-body rules that
		  are mechanical; reports collapsed height for the fold decision.
	EOF
	exit 2
}

[[ "${1:-}" == -h || "${1:-}" == --help ]] && usage

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

notes=()
if (($# > 0)); then
	notes=("$@")
else
	for f in "${REPO_ROOT}"/docs/releases/v*.md; do
		[[ -e "$f" ]] && notes+=("$f")
	done
fi
((${#notes[@]} > 0)) || {
	echo "check-release-notes: no release notes found" >&2
	exit 2
}

findings=0
for note in "${notes[@]}"; do
	name="${note#"${REPO_ROOT}"/}"
	[[ -f "$note" ]] || {
		echo "check-release-notes: not a file: ${note}" >&2
		exit 2
	}

	# A `# ` heading. Fenced blocks can legitimately contain one (a shell comment,
	# a markdown sample), so the scan skips them.
	dup="$(awk '
		/^[ \t]*(```|~~~)/ { infence = !infence; next }
		infence { next }
		/^# / { print FNR ": " $0 }
	' "$note")"
	if [[ -n "$dup" ]]; then
		# shellcheck disable=SC2016  # backticks are literal here, not substitution
		printf '%s: a `# ` heading duplicates the Releases page <h1> (the tag)\n' "$name" >&2
		printf '  %s\n' "$dup" >&2
		findings=$((findings + 1))
	fi

	# In-page anchors. A link to another document's anchor is fine; only a
	# bare `](#…)` is dead.
	anchors="$(grep -noE '\]\(#[^)]*\)' "$note" || true)"
	if [[ -n "$anchors" ]]; then
		printf '%s: in-page anchors are dead in a release body (headings carry no id)\n' "$name" >&2
		printf '  %s\n' "$anchors" >&2
		findings=$((findings + 1))
	fi

	# A helm command carrying an image-style chart version.
	badver="$(grep -noE -- '--version[ =]v[0-9]+\.[0-9]+\.[0-9]+' "$note" || true)"
	if [[ -n "$badver" ]]; then
		printf '%s: charts are versioned X.Y.Z, not vX.Y.Z — this helm command fails\n' "$name" >&2
		printf '  %s\n' "$badver" >&2
		findings=$((findings + 1))
	fi

	# What the index shows before "Read more": the sections that fall entirely
	# above the cut, and the first fold's position relative to it.
	total="$(wc -c <"$note" | tr -d ' ')"
	first_fold="$(awk '/<details/ { print off; exit } { off += length($0) + 1 }' "$note")"
	printf '%s: %s bytes' "$name" "$total"
	if [[ -n "$first_fold" ]]; then
		printf ', first fold at %s' "$first_fold"
	fi
	printf ' (index serves ~%s)\n' "$INDEX_CUT_BYTES"

	if ((total > INDEX_CUT_BYTES)); then
		printf '  note: the index truncates this behind "Read more", as it does every\n'
		printf '  stable release here. Sections a reader sees before clicking:\n'
		awk -v cut="$INDEX_CUT_BYTES" '
			/^## / { sect = substr($0, 4) }
			{ off += length($0) + 1; if (sect != "" && off <= cut) shown[sect] = 1; else if (sect != "") cut_off[sect] = 1 }
			END {
				for (s in shown) printf "    visible  %s\n", s
				for (s in cut_off) if (!(s in shown)) printf "    cut      %s\n", s
			}
		' "$note"
		if [[ -n "$first_fold" ]] && ((first_fold > INDEX_CUT_BYTES)); then
			printf '  Every fold begins past the cut, so another fold would not change what\n'
			printf '  a reader sees. Move what matters above it instead.\n'
		fi
	fi
done

if ((findings > 0)); then
	printf '\ncheck-release-notes: %d finding(s)\n' "$findings" >&2
	exit 1
fi
echo "check-release-notes: ok (${#notes[@]} note(s))"
