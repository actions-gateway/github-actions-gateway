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
# Collapsed height is reported, never failed. The Releases *index* collapses a
# long body behind "read more", a <details> fold counts as its summary line while
# collapsed, and the fix for truncation is another fold rather than a cut — but
# GitHub publishes no threshold, so this prints the measurement the fold decision
# needs (runbook: pick the next fold by measuring, not by eye) and warns above the
# only watermark evidence supports: v1.3.0, the release recorded as having hit the
# limit before it was folded back under.
set -euo pipefail
shopt -s inherit_errexit

# Above the largest release known to render un-truncated: v1.3.0 measures 12,019
# collapsed after it was folded back under the limit. So 12,019 is proven fine and
# the real ceiling is somewhere above it, unknown. A watermark, not a threshold.
WARN_COLLAPSED_BYTES=13000

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

	# Collapsed height, and the sections that dominate it.
	read -r collapsed folds < <(awk '
		/<details/ { infold = 1 }
		{ if (!infold) bytes += length($0) + 1 }
		/<summary>/ { if (infold) { bytes += length($0) + 1; folds++ } }
		/<\/details>/ { infold = 0 }
		END { print bytes + 0, folds + 0 }
	' "$note")
	printf '%s: %s bytes collapsed, %s fold(s)\n' "$name" "$collapsed" "$folds"
	if ((collapsed > WARN_COLLAPSED_BYTES)); then
		printf '  warning: above the %d-byte watermark (v1.3.0 renders un-truncated at 12,019).\n' \
			"$WARN_COLLAPSED_BYTES" >&2
		printf '  The fix is another <details> fold, not a cut. Unfolded sections, largest first:\n' >&2
		# Ranked so the choice is made on bytes rather than on which section feels
		# long — the runbook's point is that those are rarely the same section.
		awk '
			/<details/ { infold = 1 }
			/<\/details>/ { infold = 0; next }
			/^## / { sect = substr($0, 4); next }
			!infold && sect != "" { size[sect] += length($0) + 1 }
			END { for (s in size) printf "%8d  %s\n", size[s], s }
		' "$note" | sort -rn | head -5 | sed 's/^/    /' >&2
	fi
done

if ((findings > 0)); then
	printf '\ncheck-release-notes: %d finding(s)\n' "$findings" >&2
	exit 1
fi
echo "check-release-notes: ok (${#notes[@]} note(s))"
