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
# it. See release.md § Checks that stay human for the ones that were measured and
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
#   - A drafting banner outlives its release's tag: a `> **Draft` line, or one
#     saying "before publishing", in a note whose filename is a tag here.
#   - A backticked commit hash resolves to no commit: a release note or a plan
#     doc names one that does not exist, as `v2.0.0-rc.1`'s was written
#     `00b4f4e` for the real `0c0048d5a` (Q1356). The scan covers the plan docs
#     as well as the notes when run without arguments, because that error was
#     in a plan doc.
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
WORK_CITES="$(mktemp)"
trap 'rm -f "$WORK_CITES"' EXIT INT TERM

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

# Backticked hashes are looked up in every doc the notes gate covers by default:
# the notes themselves and the plan docs, where candidate-validation tables name
# the commits a tag points at. Explicit arguments narrow the scan to those files.
cite_docs=("${notes[@]}")
if (($# == 0)); then
	for f in "${REPO_ROOT}"/docs/plan/*.md "${REPO_ROOT}"/docs/plan/archive/*.md; do
		[[ -e "$f" ]] && cite_docs+=("$f")
	done
fi

findings=0
# Reconciliation for the commit-count rule: without tags every note takes the
# skip path, and a run that checked no count at all must not read as a clean
# one. Reported on the ok line rather than failed, because a tagless fork is a
# legitimate tree (Q1002).
#
# A third bucket, because two of those are not the only ways a count goes
# unchecked: a note can name a range and assert no figure at all, and that read
# as a clean pass with nothing on the ok line to say so. Deleting the count from
# a note was therefore indistinguishable from correcting it (Q1102).
counts_checked=0
counts_skipped=0
counts_approx=0
counts_unranged=0
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

	to_tag="$(basename "$note" .md)"

	# A drafting banner, once the note's release is tagged. While it is being
	# written a note may carry one, and v2.0.0's did for eleven days with this
	# gate running on every PR, so the rule binds only from the tag, which is
	# the moment the note is about to become the Release body. v2.0.0's still
	# said "Delete this note before publishing" after its tag, and only a prose
	# pass caught it.
	if git -C "$REPO_ROOT" rev-parse -q --verify "refs/tags/$to_tag" >/dev/null; then
		draft="$(awk '
			/^[ \t]*(```|~~~)/ { infence = !infence; next }
			infence { next }
			/^>/ && ($0 ~ /^> *\*\*Draft[ ,.:]/ || tolower($0) ~ /before publishing/) { print FNR ": " substr($0, 1, 100) }
		' "$note")"
		if [[ -n "$draft" ]]; then
			printf '%s: %s is tagged, and the note still carries a drafting banner\n' "$name" "$to_tag" >&2
			printf '  %s\n' "$draft" >&2
			findings=$((findings + 1))
		fi
	fi

	# The "Everything since" commit count, against the range the note names.
	#
	# The number is the opening line of the most-read paragraph in the notes and
	# nothing derived it, so it went stale silently: during 1.6 it was written as
	# 158 at the rc.1 cut, was still 158 after rc.2 (actual 165), and reached 166
	# only because the promote pass happened to re-derive it. A stale count reads
	# exactly like a fresh one, which is what makes this worth a gate (Q1002).
	#
	# The note declares its own range — the heading names the previous tag and the
	# filename names this one — so nothing here guesses at tag ordering. That also
	# means a note is checked against the range it claims, not the range it should
	# have claimed; getting the heading wrong is a different defect.
	#
	# A figure may be deliberately approximate — v1.3.0 opens `Over 450 commits`
	# against a 463-commit window, where the exact number carries nothing the
	# reader wants. That form is recognised and reported rather than checked, so
	# the one thing left to fail on is a note that names a range and asserts no
	# figure at all.
	#
	# Both figures are read from inside the section, never from the whole file.
	# md-reflow puts every sentence at column 0, so an unscoped match lets any
	# sentence anywhere decide the verdict: a note whose count was deleted passed
	# as `approximate` on the strength of `Nearly 20 commits, mostly documentation,
	# land after the freeze.` sitting further down, which is the same deletion this
	# rule exists to fail.
	from_tag="$(awk '/^## Everything since v/ { print $4; exit }' "$note")"
	claimed="$(awk '
		/^## Everything since v/ { inrange = 1; next }
		/^## / { inrange = 0 }
		inrange && /^[0-9]+ commits[,. ]/ { print $1; exit }
	' "$note")"
	approx="$(awk '
		/^## Everything since v/ { inrange = 1; next }
		/^## / { inrange = 0 }
		inrange && /^(Over|About|Around|Roughly|Nearly) [0-9]+ commits[,. ]/ { print $1 " " $2; exit }
	' "$note")"

	if [[ -n "$from_tag" && -z "$claimed" && -z "$approx" ]]; then
		printf '%s: names a range since %s and asserts no commit count\n' "$name" "$from_tag" >&2
		printf '  the section opens with one; a deleted count reads exactly like a correct one\n' >&2
		printf '  scripts/release/semver-floor.sh %s %s prints the figure\n' "$from_tag" "$to_tag" >&2
		findings=$((findings + 1))
	elif [[ -n "$from_tag" && -z "$claimed" ]]; then
		counts_approx=$((counts_approx + 1))
		printf '%s: commit figure is approximate by construction (%s) — reported, not checked\n' \
			"$name" "$approx" >&2
	elif [[ -z "$from_tag" ]]; then
		# No range heading, so there is no range to count over. Right for a
		# fragment passed by hand, and counted anyway: without this the buckets
		# silently stop summing to the note count, which is the shape of hole
		# the rules above exist to close.
		counts_unranged=$((counts_unranged + 1))
	fi

	if [[ -n "$from_tag" && -n "$claimed" ]]; then
		# Both tags must exist to count between them. A shallow CI checkout has
		# neither, and reporting a mismatch from a tagless tree would be noise
		# about the checkout rather than about the note.
		if git -C "$REPO_ROOT" rev-parse -q --verify "refs/tags/$from_tag" >/dev/null &&
			git -C "$REPO_ROOT" rev-parse -q --verify "refs/tags/$to_tag" >/dev/null; then
			actual="$(git -C "$REPO_ROOT" rev-list --no-merges --count "$from_tag..$to_tag")"
			counts_checked=$((counts_checked + 1))
			if [[ "$claimed" != "$actual" ]]; then
				printf '%s: says %s commits since %s, but %s..%s holds %s (merges excluded)\n' \
					"$name" "$claimed" "$from_tag" "$from_tag" "$to_tag" "$actual" >&2
				printf '  scripts/release/semver-floor.sh %s %s prints the figure\n' "$from_tag" "$to_tag" >&2
				findings=$((findings + 1))
			fi
		else
			counts_skipped=$((counts_skipped + 1))
			printf '%s: SKIP commit count — %s or %s is not a tag here (shallow checkout?)\n' \
				"$name" "$from_tag" "$to_tag" >&2
		fi
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

# Cited commit hashes. A backticked token of 7 to 40 hex characters is a
# candidate; one needs both a digit and a letter, which spares an all-digit
# number (a port, a byte count) and an all-letter word, at the price of missing a
# fabricated hash that happens to be all one kind. A `sha256:` digest has a
# prefix and is never a bare token. Fenced blocks are skipped, since a command
# shown there is an example. The length is tested with length() rather than an
# interval, which mawk on the CI runner does not take.
#
# A hash passes when it resolves to a commit in the fetched history, and the job
# fetches every branch and tag, so a commit a squash merge orphaned still passes
# while its objects are here and an invented one never does. A shallow checkout
# cannot answer, so it reports a skip rather than a pass.
#
# Tokens the docs already carried that are not commits here (an abbreviated image
# digest, another project's commit, an orphaned one) are listed per doc in
# cited-commits-baseline.txt and exempt only there.
BASELINE="$(dirname "${BASH_SOURCE[0]}")/cited-commits-baseline.txt"
cites_checked=0
cites_skipped=0
cites_baselined=0
if [[ "$(git -C "$REPO_ROOT" rev-parse --is-shallow-repository)" == true ]]; then
	cites_skipped=1
	printf 'check-release-notes: SKIP cited commits — this checkout is shallow, so no hash can be resolved\n' >&2
else
	for doc in "${cite_docs[@]}"; do
		dname="${doc#"${REPO_ROOT}"/}"
		cites="$(awk '
			/^[ \t]*(```|~~~)/ { infence = !infence; next }
			infence { next }
			{
				n = split($0, a, "`")
				for (i = 2; i <= n; i += 2)
					if (length(a[i]) >= 7 && length(a[i]) <= 40 && a[i] ~ /^[0-9a-f]+$/ && a[i] ~ /[0-9]/ && a[i] ~ /[a-f]/)
						print FNR " " a[i]
			}
		' "$doc")"
		[[ -n "$cites" ]] || continue
		# One lookup per document. `<hash>^{commit}` also refuses a hash that names
		# a blob or a tree, which a bare existence check would accept.
		verdicts="$(awk '{ print $2 "^{commit}" }' <<<"$cites" | git -C "$REPO_ROOT" cat-file --batch-check)"
		paste -d' ' <(printf '%s\n' "$cites") <(printf '%s\n' "$verdicts") >"$WORK_CITES"
		while read -r line sha rest; do
			cites_checked=$((cites_checked + 1))
			# `<name> missing` is the invented case. `ambiguous` means the
			# abbreviation names several objects, which only a real hash can.
			if [[ "$rest" == *" missing" ]]; then
				if grep -qxF -- "${dname} ${sha}" "$BASELINE"; then
					cites_baselined=$((cites_baselined + 1))
					continue
				fi
				printf '%s:%s: `%s` is not a commit in this repository\n' "$dname" "$line" "$sha" >&2
				findings=$((findings + 1))
			fi
		done <"$WORK_CITES"
	done
fi

if ((findings > 0)); then
	printf '\ncheck-release-notes: %d finding(s)\n' "$findings" >&2
	exit 1
fi
printf 'check-release-notes: ok (%d note(s); %d commit count(s) checked, %d skipped for want of tags, %d approximate, %d with no range heading; %d cited commit hash(es) checked in %d doc(s), %d on the baseline%s)\n' \
	"${#notes[@]}" "$counts_checked" "$counts_skipped" "$counts_approx" "$counts_unranged" \
	"$cites_checked" "${#cite_docs[@]}" "$cites_baselined" "$( ((cites_skipped)) && printf ', skipped: shallow checkout')"
