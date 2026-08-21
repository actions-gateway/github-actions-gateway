#!/usr/bin/env bash
#
# check-vendored-skills.sh — hold each file vendored from karlkfi/claude-skills
# to the digest that declares it (Q890).
#
# Q889 phase 1 vendored four files byte-identical so an upstream fix would land
# as a clean overwrite, and nothing held them there. Measured 2026-08-21: all
# four matched an upstream commit exactly at vendor time, and none had been
# edited here since — the drift was entirely upstream's, 8 commits on queue.py
# and 1 to 2 on the rest. So the fork was one-way, and unseen either way,
# because no gate and no record named these files as vendored at all.
#
# This gate asserts the half it can: a vendored file hashes to the
# `local_sha256` its row in vendored-skills.tsv declares. Forking one is then a
# reviewable act — the digest moves in the same diff — rather than a hunk that
# reads like any other edit.
#
# It cannot ask whether upstream has moved, and that is a property of the
# question rather than of this script. karlkfi/claude-skills is private, so a
# fetching gate needs a token this repo does not carry; and a gate whose oracle
# is the network fails when a third party sneezes, which is why `make check`
# excludes the two dependency gates that can reach out. Comparing against a
# local clone would key the gate to one machine's layout. The upstream half is
# therefore still unasked, deliberately and in the open.
#
# Nothing derives the vendored set, because nothing in the tree marks a file as
# vendored, so a fifth one adopted without a row is invisible here.
#
# Usage:
#   check-vendored-skills.sh [--manifest PATH]   verify every declared digest
#   check-vendored-skills.sh --report            what each file is, forked or not
#   check-vendored-skills.sh --update            re-stamp local_sha256 from disk
#
# --update is how a deliberate fork is declared. It is not a bypass: the new
# digest lands in the diff, next to the edit that moved it.
#
# Exits 1 on a finding, and 2 on a usage error, an unreadable manifest, or a
# manifest holding no rows — a manifest this gate could not parse must not read
# as every file intact.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

MANIFEST="scripts/ci/vendored-skills.tsv"
MODE="verify"

usage() {
	cat >&2 <<-EOF
		usage: $(basename "$0") [--manifest PATH] [--report | --update]

		  --manifest PATH  default: scripts/ci/vendored-skills.tsv
		  --report         print each vendored file's origin and fork state
		  --update         re-stamp local_sha256 from the files on disk
	EOF
	exit 2
}

while (($# > 0)); do
	case "$1" in
	--manifest)
		[[ $# -ge 2 ]] || usage
		MANIFEST="$2"
		shift 2
		;;
	--report)
		MODE="report"
		shift
		;;
	--update)
		MODE="update"
		shift
		;;
	-h | --help) usage ;;
	*)
		echo "check-vendored-skills: unknown argument: $1" >&2
		usage
		;;
	esac
done

[[ -f "$MANIFEST" ]] || {
	echo "check-vendored-skills: no manifest at $MANIFEST" >&2
	exit 2
}

# Portable SHA256: coreutils sha256sum on Linux/CI, shasum -a 256 on macOS.
sha256_of() {
	local f="$1"
	if command -v sha256sum > /dev/null 2>&1; then
		sha256sum "$f" | awk '{print $1}'
	else
		shasum -a 256 "$f" | awk '{print $1}'
	fi
}

# Data rows only, and each must carry all five fields. A short row is a parse
# failure rather than a row to skip: silently dropping one would take a file
# out of the gate while the count still looked plausible.
rows=()
lineno=0
while IFS= read -r line; do
	lineno=$((lineno + 1))
	[[ -n "$line" ]] || continue
	[[ "$line" != \#* ]] || continue
	if [[ "$(awk -F'\t' '{print NF}' <<< "$line")" != 5 ]]; then
		echo "check-vendored-skills: $MANIFEST:$lineno: want 5 tab-separated fields" >&2
		exit 2
	fi
	rows+=("$line")
done < "$MANIFEST"

if ((${#rows[@]} == 0)); then
	echo "check-vendored-skills: $MANIFEST declares no vendored files" >&2
	exit 2
fi

if [[ "$MODE" == "update" ]]; then
	changed=0
	for row in "${rows[@]}"; do
		IFS=$'\t' read -r local_path _ _ _ local_sha <<< "$row"
		[[ -f "$local_path" ]] || {
			echo "check-vendored-skills: $local_path is declared but missing" >&2
			exit 1
		}
		got="$(sha256_of "$local_path")"
		[[ "$got" != "$local_sha" ]] || continue
		awk -F'\t' -v OFS='\t' -v p="$local_path" -v s="$got" \
			'$1 == p { $5 = s } { print }' "$MANIFEST" > "$MANIFEST.tmp"
		mv "$MANIFEST.tmp" "$MANIFEST"
		echo "check-vendored-skills: re-stamped $local_path -> $got"
		changed=$((changed + 1))
	done
	echo "check-vendored-skills: $changed row(s) re-stamped"
	exit 0
fi

if [[ "$MODE" == "report" ]]; then
	for row in "${rows[@]}"; do
		IFS=$'\t' read -r local_path upstream_path vendored_at vendored_sha local_sha <<< "$row"
		if [[ "$vendored_sha" == "$local_sha" ]]; then
			state="vendored"
		else
			state="forked"
		fi
		printf '%-40s %-8s %s @ %s\n' "$local_path" "$state" "$upstream_path" "${vendored_at:0:12}"
	done
	echo "check-vendored-skills: ${#rows[@]} file(s) from karlkfi/claude-skills"
	exit 0
fi

findings=0
for row in "${rows[@]}"; do
	IFS=$'\t' read -r local_path _ _ _ local_sha <<< "$row"
	if [[ ! -f "$local_path" ]]; then
		echo "check-vendored-skills: $local_path is declared in $MANIFEST but missing" >&2
		findings=$((findings + 1))
		continue
	fi
	got="$(sha256_of "$local_path")"
	[[ "$got" != "$local_sha" ]] || continue
	echo "check-vendored-skills: $local_path has changed since it was declared" >&2
	echo "    declared $local_sha" >&2
	echo "    on disk  $got" >&2
	findings=$((findings + 1))
done

if ((findings > 0)); then
	echo "check-vendored-skills: $findings finding(s). A deliberate fork is declared with" >&2
	echo "    scripts/ci/check-vendored-skills.sh --update" >&2
	echo "which moves the digest in the same diff as the edit." >&2
	exit 1
fi

echo "check-vendored-skills: ok (${#rows[@]} file(s) match their declared digest)"
