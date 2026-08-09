#!/usr/bin/env bash
#
# alloc-queue-id.sh — allocate backlog Q-IDs by claiming a ref on the remote.
#
# Replaces the `**Next ID:** QN` counter that used to live in docs/STATUS.md.
# That counter was a single mutable line, so two sessions filing a row
# concurrently always took the same ID and always conflicted on the same line
# — by construction, not by luck. Resolving it meant renumbering: the row, its
# `<a id="QN"></a>` anchor, every `(#QN)` cross-reference, the plan doc, the
# PR body, the commit subject. Q382 recorded three such renumberings across a
# single PR's rebases.
#
# A ref name is a compare-and-swap register: creating one that already exists
# fails server-side, atomically, with no lock to acquire or release. Claiming
# `refs/queue-ids/QN` therefore hands concurrent sessions distinct IDs with no
# shared mutable state, and nothing to clean up when a session dies mid-task.
#
# Two properties worth preserving if this is ever rewritten:
#
#   1. Every claim points at the repository's ROOT COMMIT, not at the caller's
#      branch tip. A ref is a GC root: claims pointing at `claude/*` tips would
#      pin every squash-orphaned branch history in the repo, forever. Pointing
#      them all at one already-permanent object retains nothing new — the refs
#      cost ~64 bytes each in packed-refs and nothing else.
#   2. The claim must fail on an EXISTING ref name. `git push <sha>:<ref>`
#      cannot be used for this: when the ref already exists and points at the
#      same sha, push reports "Everything up-to-date" and exits 0, so the
#      caller concludes it won a race it actually lost. The create-ref API
#      returns HTTP 422 "Reference already exists", which is unambiguous.
#
# IDs are sparse, not dense. A session that claims an ID and then never files
# a row leaves a hole; that is expected, not a leak. Refs are never deleted:
# the namespace is the record of which IDs have been used, and deleting one
# would let a retired ID be reissued. Measured 2026-08-03: 10 of 240 claims
# never became a row, so the space is consumed about 4% faster than rows are
# filed.
#
# There is NO no-reserve report. `--peek` printed the next free ID and claimed
# nothing, which is the `**Next ID:** QN` counter this script replaced, reborn
# as a flag: two sessions reading it concurrently read the same answer, and the
# one that merged second paid the renumber. Q656 measured that collision — a row
# carrying Q644 was committed 43 minutes before any Q644 claim existed. Knowing
# the next ID without taking it has no use that survives the session, so the
# only way to learn an ID is to hold it.
#
# Every ID takes a title, because this is the one place every row passes through
# and the near-duplicate search needs text to match on (Q639). An optional
# argument would be a gate nobody passes through, so titles are mandatory and
# `-n <count>` is gone: the count is however many titles you give, and each gets
# its own search. The search runs BEFORE the claim, so noticing a duplicate
# costs no ID; it prints to stderr and never blocks, so `ID=$(...)` still works.
#
# Usage:
#   alloc-queue-id.sh "<title>"              # claim and print one ID
#   alloc-queue-id.sh "<title>" "<title>"    # one ID per title, one per line
#   alloc-queue-id.sh --target <link> "<title>"   # link the Item cell will carry
#
# Rationale and the alternatives considered: docs/development/queue-id-allocation.md

set -euo pipefail
shopt -s inherit_errexit

readonly REF_NS='refs/queue-ids'
# Bounds the advance-on-collision walk. Exceeding it means either a very large
# concurrent burst or a bug; either way, failing beats spinning.
readonly MAX_ATTEMPTS=25

die() {
	printf 'alloc-queue-id: %s\n' "$*" >&2
	exit 1
}

usage() {
	awk '/^# Usage:/,/^$/ { sub(/^#[[:space:]]?/, ""); print }' "$0"
}

# Repository slug (owner/name) for the API call.
repo_slug() {
	local slug
	slug=$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null) ||
		die "could not resolve the GitHub repo. Is 'gh' authenticated? Try: gh auth status"
	printf '%s\n' "$slug"
}

# The object every claim points at. See property (1) in the header.
sentinel_sha() {
	local sha
	sha=$(git rev-list --max-parents=0 HEAD | tail -1)
	[[ -n "$sha" ]] || die 'could not resolve the root commit to anchor claims to'
	printf '%s\n' "$sha"
}

# Highest ID already claimed in the ref namespace. 0 when the namespace is empty.
highest_claimed() {
	git ls-remote origin "$REF_NS/*" 2>/dev/null |
		awk -F/ '$NF ~ /^Q[0-9]+$/ { n = substr($NF, 2) + 0; if (n > max) max = n } END { print max + 0 }'
}

# Highest ID appearing in the backlog file. This is a transition floor only:
# done rows are deleted, so the file's max can regress below an ID that was
# already used. The ref namespace is the durable high-water mark.
highest_in_file() {
	local file=$1
	[[ -f "$file" ]] || {
		printf '0\n'
		return
	}
	# `|| true`: grep exits 1 on no match, which pipefail would turn into a
	# function failure for a legitimately ID-free file.
	{ grep -o 'id="Q[0-9]*"' "$file" 2>/dev/null || true; } |
		awk '{ gsub(/[^0-9]/, ""); if ($0 + 0 > max) max = $0 + 0 } END { print max + 0 }'
}

# Claim one ID. Returns 0 on success, 1 when the name is already taken, and
# dies on any other failure — a network or auth error must not be misread as a
# collision, or a transient outage would silently burn 25 IDs.
claim() {
	local id=$1 slug=$2 sha=$3 out rc=0
	out=$(gh api -X POST "repos/$slug/git/refs" \
		-f ref="$REF_NS/$id" -f sha="$sha" 2>&1) && return 0
	rc=$?
	[[ "$out" == *'Reference already exists'* ]] && return 1
	die "claiming $REF_NS/$id failed (exit $rc): $out"
}

# Advisory near-duplicate search for one title. Never fails the allocation: a
# broken matcher must not stop a row being filed.
search_duplicates() {
	local title=$1 file=$2 target=$3 script
	script="$(dirname "$0")/find-duplicate-rows.sh"
	[[ -x "$script" ]] || return 0
	"$script" --file "$file" ${target:+--target "$target"} "$title" >&2 || true
}

main() {
	local file target='' titles=()
	file="$(git rev-parse --show-toplevel)/docs/STATUS.md"

	while (($# > 0)); do
		case "$1" in
		--target)
			target=${2:-}
			shift 2
			;;
		-h | --help)
			usage
			exit 0
			;;
		-*) die "unknown argument: $1" ;;
		*)
			# An empty argument is how `make queue-id` spells "no TITLE given"
			# — the Makefile passes the variable through unconditionally so
			# the shell never re-parses a title's quotes or backticks.
			[[ -n "$1" ]] && titles+=("$1")
			shift
			;;
		esac
	done

	((${#titles[@]} > 0)) ||
		die 'wants the title of each row you are about to file — one argument per ID, so each gets a near-duplicate search'
	[[ -z "$target" ]] || ((${#titles[@]} <= 1)) || die '--target describes one row, so it takes at most one title'

	local title
	for title in ${titles[@]+"${titles[@]}"}; do
		search_duplicates "$title" "$file" "$target"
	done

	local count=${#titles[@]} floor claimed_max file_max
	claimed_max=$(highest_claimed)
	file_max=$(highest_in_file "$file")
	floor=$((claimed_max > file_max ? claimed_max : file_max))

	local slug sha candidate=$((floor + 1)) issued=0 attempts=0
	slug=$(repo_slug)
	sha=$(sentinel_sha)

	while ((issued < count)); do
		if claim "Q$candidate" "$slug" "$sha"; then
			printf 'Q%d\n' "$candidate"
			issued=$((issued + 1))
			attempts=0
		else
			attempts=$((attempts + 1))
			((attempts < MAX_ATTEMPTS)) ||
				die "gave up after $MAX_ATTEMPTS collisions at Q$candidate; inspect: git ls-remote origin '$REF_NS/*'"
		fi
		candidate=$((candidate + 1))
	done
}

main "$@"
