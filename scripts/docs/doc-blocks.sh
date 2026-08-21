#!/usr/bin/env bash
#
# doc-blocks.sh — read the `gag:verify` annotations on a doc's fenced blocks,
# and hand them to whatever executes them (Q958).
#
# docs/getting-started.md is the install procedure an operator follows verbatim,
# and until now nothing ran a line of it: `make check` held it to link and prose
# rules, the e2e suite built its tenant fixtures in Go, and the doc's own text
# had no executor at all. A CRD field renamed under it would leave the page
# rendering perfectly and failing on contact.
#
# Executing every fence is the wrong shape — of the 14 blocks on that page, two
# need bytes that exist only after a release is cut and two need a kubelet — so
# coverage is opt-in, one HTML comment directly above the fence:
#
#   <!-- gag:verify id=tenant-ns mode=apply teardown=namespace -->
#
# The comment renders as nothing in MkDocs and on github.com, and mdreflow
# leaves it byte-for-byte (measured against a fixture carrying both this shape
# and a `{ .attr }` info string; only prose reflows). An UNANNOTATED block is
# inert: this gate never fails on one, because demanding an annotation on an
# illustrative fence would be noise. What it does instead is hold each file to a
# declared floor of EXECUTED blocks — a declared skip counts zero — so a block
# cannot quietly drop out of coverage the way a hand-kept list drifts, and a
# demotion to mode=skip cannot slip past by keeping the total steady.
#
# Keys:
#   id=       unique slug within the file; names the block in every failure
#   mode=     apply | run | dry-run | render | skip
#   needs=    comma-separated ids that must be applied first, all declared earlier
#   teardown= what the executor removes afterwards (namespace | object | none)
#   reason=   required on mode=skip; why this block is not executed
#
# Modes, and who runs them:
#   apply     yaml: server-side apply, assert accepted, read back } the GMC envtest
#   run       sh:   execute against the apiserver, assert exit 0  } integration
#   dry-run   yaml: server-side dry-run, assert accepted          } suite
#   render    sh:   `helm template`, assert it renders              `--check-render`
#   skip      declared uncovered, with a reason                     nobody
#
# `run` executes the doc's own command lines. envtest ships kubectl in its binary
# assets (pkg/envtest/binaries.go), so the suite puts KUBEBUILDER_ASSETS on PATH
# and a kubeconfig for its own apiserver in the environment — the block runs as
# written rather than as a Go translation of what it means.
#
# `--emit` is the machine interface both halves read, so the local and CI
# verdicts come from one parse rather than two. `--check` is the offline gate
# (`make getting-started-check`): annotation integrity plus the floor, no host
# tool beyond git, which is what lets it sit in CHECK_FAST_GATES. The helm half
# is `--check-render` behind its own target, because `make check` requires no
# chart tooling and this gate is not where that changes.
#
# Usage:
#   doc-blocks.sh --emit <file|->           # id<TAB>mode<TAB>lang<TAB>line<TAB>needs<TAB>teardown
#   doc-blocks.sh --body <file|-> <id>      # the block's literal text, to stdout
#   doc-blocks.sh --check [--floor N] [file ...]  # offline gate; no file = the registry
#   doc-blocks.sh --check-render [file ...]       # `helm template` the render-mode blocks
#
# --floor is for the test suite, like gate-list.sh's --makefile/--doc: a file
# named on the command line carries no registry floor, so without it the floor
# path is unreachable from a fixture and only the shipped page would exercise it.
#
# Exits non-zero on any finding. Findings print as `file:line: message`.

set -euo pipefail
shopt -s inherit_errexit

# REPO_ROOT before the source, and the source keyed off it rather than off this
# file's own directory: the GMC integration suite execs this script through a
# committed testdata/ symlink (so the read lands inside its module root and go's
# test cache keys on it, Q895/Q902), and a $SCRIPT_DIR-relative source resolves
# to the symlink's directory, where scripts/lib/ does not exist.
REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

# Files under coverage, as `path<TAB>floor`. The floor is the number of
# annotated blocks the file must carry, and it is a ratchet in one direction:
# raising it is the point, lowering it is the edit that needs saying out loud in
# a review. Calibrated against the page as annotated, not guessed.
#
# demo.md is deliberately absent: it has no fenced blocks at all. Its executable
# content is docs/development/local-kind-demo.md, which is a from-source
# procedure needing a kind cluster and real GitHub credentials — out of this
# venue's reach, and tracked as its own item rather than declared here.
COVERED_FILES=(
	"docs/getting-started.md	10"
)

VALID_MODES='apply run dry-run render skip'
VALID_TEARDOWNS='namespace object none'

MODE=""
TARGET=""
BLOCK_ID=""
FLOOR_OVERRIDE=""

while (($# > 0)); do
	case "$1" in
	--emit | --body | --check | --check-render)
		MODE="${1#--}"
		;;
	--floor)
		FLOOR_OVERRIDE="$2"
		shift
		;;
	--)
		shift
		break
		;;
	-)
		# A bare `-` is the stdin document, not a flag.
		break
		;;
	-*)
		printf 'doc-blocks.sh: unknown argument: %s\n' "$1" >&2
		exit 2
		;;
	*)
		break
		;;
	esac
	shift
done

if [[ -z "$MODE" ]]; then
	printf 'doc-blocks.sh: one of --emit, --body, --check, --check-render is required\n' >&2
	exit 2
fi

cd "$REPO_ROOT"

# `-` means the document arrives on stdin — how the GMC integration suite hands
# over the exact bytes it read through its testdata/ symlink, so one parser
# serves the make target and the executor. Spooled here, in the main shell:
# records() and malformed() each run in a command substitution, and a temp file
# created inside one would be removed by its own EXIT trap before the next call.
SPOOL=""
case "$MODE" in
emit)
	TARGET="${1:-}"
	[[ -n "$TARGET" ]] || {
		printf 'doc-blocks.sh --emit: a file argument is required\n' >&2
		exit 2
	}
	;;
body)
	TARGET="${1:-}"
	BLOCK_ID="${2:-}"
	[[ -n "$TARGET" && -n "$BLOCK_ID" ]] || {
		printf 'doc-blocks.sh --body: a file and a block id are required\n' >&2
		exit 2
	}
	;;
esac

# parse FILE — emit one record per annotated block as
# `id<TAB>mode<TAB>lang<TAB>line<TAB>needs<TAB>teardown<TAB>reason`, plus one
# `!<TAB>line<TAB>message` record per malformed annotation. Both go to stdout;
# the caller decides which it cares about, so the grammar lives in exactly one
# awk program rather than one per consumer.
#
# `line` is the fence's own line number, which is what a failure message needs:
# a reader jumps to the block, not to its comment.
parse() {
	local src="$1"
	awk '
		function fail(msg) { printf "!\t%d\t%s\n", FNR, msg; pending = 0 }

		# A fence opens; close-fence handling is the `infence` branch below.
		/^[[:space:]]*(```|~~~)/ {
			if (infence) { infence = 0; next }
			infence = 1
			lang = $0
			sub(/^[[:space:]]*(```|~~~)/, "", lang)
			sub(/[[:space:]].*$/, "", lang)
			if (lang == "") lang = "none"
			if (pending) {
				printf "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", \
					p_id, p_mode, lang, FNR, p_needs, p_teardown, p_reason
				pending = 0
			}
			next
		}
		infence { next }

		# An annotation already read, and something other than a blank line or a
		# fence follows it: the comment is not attached to any block. A second
		# annotation lands here too, which is why the annotation rule below has no
		# case of its own for one — this rule has already cleared pending.
		pending && !/^[[:space:]]*$/ {
			fail("gag:verify annotation for id=" p_id " is not followed by a fenced block")
			# Fall through: this line may itself be another annotation.
		}

		/<!--[[:space:]]*gag:verify/ {
			body = $0
			sub(/^.*<!--[[:space:]]*gag:verify[[:space:]]*/, "", body)
			sub(/[[:space:]]*-->.*$/, "", body)
			p_id = ""; p_mode = ""; p_needs = ""; p_teardown = ""; p_reason = ""
			n = split(body, kv, /[[:space:]]+/)
			bad = 0
			for (i = 1; i <= n; i++) {
				if (kv[i] == "") continue
				eq = index(kv[i], "=")
				if (eq == 0) { fail("gag:verify: \"" kv[i] "\" is not a key=value pair"); bad = 1; break }
				k = substr(kv[i], 1, eq - 1)
				v = substr(kv[i], eq + 1)
				if (k == "id") p_id = v
				else if (k == "mode") p_mode = v
				else if (k == "needs") p_needs = v
				else if (k == "teardown") p_teardown = v
				else if (k == "reason") p_reason = v
				else { fail("gag:verify: unknown key \"" k "\""); bad = 1; break }
			}
			if (bad) next
			if (p_id == "") { fail("gag:verify: id= is required"); next }
			if (p_mode == "") { fail("gag:verify id=" p_id ": mode= is required"); next }
			if (p_needs == "") p_needs = "-"
			if (p_teardown == "") p_teardown = "-"
			if (p_reason == "") p_reason = "-"
			pending = 1
			next
		}

		END {
			if (pending) fail("gag:verify annotation for id=" p_id " is not followed by a fenced block")
			if (infence) printf "!\t%d\t%s\n", FNR, "unterminated fenced block"
		}
	' "$src"
}

# records FILE — the well-formed records only.
records() { parse "$1" | grep -v '^!	' || true; }

# malformed FILE — the parse errors only.
malformed() { parse "$1" | grep '^!	' || true; }

# body FILE ID — the block's literal text, resolved from the same parse --emit
# reads, so the two cannot disagree about which block an id names.
body() {
	local src="$1" fence_line
	fence_line="$(records "$src" | awk -F'\t' -v want="$2" '$1 == want { print $4; found = 1 } END { exit !found }')" ||
		die "doc-blocks.sh --body: no block with id=$2 in $1"
	awk -v start="$fence_line" 'FNR > start { if ($0 ~ /^[[:space:]]*(```|~~~)[[:space:]]*$/) exit; print }' "$src"
}

case "$MODE" in
emit)
	if [[ "$TARGET" == "-" ]]; then
		SPOOL="$(mktemp)"
		trap 'rm -f "$SPOOL"' EXIT
		cat >"$SPOOL"
		TARGET="$SPOOL"
	fi
	[[ -f "$TARGET" ]] || die "doc-blocks.sh --emit: no such file: $TARGET"
	bad="$(malformed "$TARGET")"
	if [[ -n "$bad" ]]; then
		printf '%s\n' "$bad" | while IFS=$'\t' read -r _ line msg; do
			printf '%s:%s: %s\n' "$TARGET" "$line" "$msg" >&2
		done
		exit 1
	fi
	records "$TARGET"
	exit 0
	;;
body)
	if [[ "$TARGET" == "-" ]]; then
		SPOOL="$(mktemp)"
		trap 'rm -f "$SPOOL"' EXIT
		cat >"$SPOOL"
		TARGET="$SPOOL"
	fi
	[[ -f "$TARGET" ]] || die "doc-blocks.sh --body: no such file: $TARGET"
	# The fence line for this id, then the block's contents up to the closing
	# fence. Resolved from the same parse the executor reads, so --body and
	# --emit cannot disagree about which block an id names.
	body "$TARGET" "$BLOCK_ID"
	exit 0
	;;
esac

# --- the two gate modes -------------------------------------------------------

files=()
floors=()
if (($# > 0)); then
	for f in "$@"; do
		files+=("$f")
		floors+=("${FLOOR_OVERRIDE:-0}")
	done
else
	for entry in "${COVERED_FILES[@]}"; do
		IFS=$'\t' read -r path floor <<<"$entry"
		files+=("$path")
		floors+=("$floor")
	done
fi

fail=0

for idx in "${!files[@]}"; do
	file="${files[$idx]}"
	floor="${floors[$idx]}"

	if [[ ! -f "$file" ]]; then
		printf '%s: covered by doc-blocks.sh but not present. Update COVERED_FILES if the page moved.\n' "$file" >&2
		fail=1
		continue
	fi

	bad="$(malformed "$file")"
	if [[ -n "$bad" ]]; then
		while IFS=$'\t' read -r _ line msg; do
			printf '%s:%s: %s\n' "$file" "$line" "$msg" >&2
		done <<<"$bad"
		fail=1
	fi

	recs="$(records "$file")"

	if [[ "$MODE" == check-render ]]; then
		require_cmd helm "https://helm.sh/docs/intro/install/"
		while IFS=$'\t' read -r id mode _lang line _needs _teardown _reason; do
			[[ -n "$id" ]] || continue
			[[ "$mode" == render ]] || continue
			# A render block names a chart directory in the checkout. Rendering
			# it is the whole assertion: a chart path the doc names that no
			# longer exists, or a values flag the chart stopped accepting, fails
			# here rather than in an operator's terminal.
			chart="$(body "$file" "$id" |
				awk '{ for (i = 1; i <= NF; i++) if ($i ~ /^charts\//) { print $i; exit } }')"
			if [[ -z "$chart" ]]; then
				printf '%s:%s: block "%s" is mode=render but names no charts/ path to render\n' "$file" "$line" "$id" >&2
				fail=1
				continue
			fi
			if ! helm template gag "$chart" --set allowFloatingImageTags=true >/dev/null 2>&1; then
				printf '%s:%s: block "%s" names chart %s, which no longer renders under helm template\n' \
					"$file" "$line" "$id" "$chart" >&2
				fail=1
			fi
		done <<<"$recs"
		continue
	fi

	# --check: integrity, then the floor.
	declare -A seen_ids=()
	count=0
	while IFS=$'\t' read -r id mode lang line needs teardown reason; do
		[[ -n "$id" ]] || continue
		# The floor counts EXECUTED blocks. Counting declared skips too would let
		# a block flip from apply to skip without moving the number, which is the
		# drift the floor exists to catch.
		[[ "$mode" == skip ]] || count=$((count + 1))

		if [[ ! "$id" =~ ^[a-z0-9][a-z0-9-]*$ ]]; then
			printf '%s:%s: block id "%s" is not a lowercase slug ([a-z0-9][a-z0-9-]*)\n' "$file" "$line" "$id" >&2
			fail=1
		fi
		if [[ -n "${seen_ids[$id]:-}" ]]; then
			printf '%s:%s: block id "%s" is already used at line %s. Ids name blocks in failures, so they must be unique per file.\n' \
				"$file" "$line" "$id" "${seen_ids[$id]}" >&2
			fail=1
		fi
		seen_ids[$id]="$line"

		if [[ " $VALID_MODES " != *" $mode "* ]]; then
			printf '%s:%s: block "%s" has mode=%s; expected one of: %s\n' "$file" "$line" "$id" "$mode" "$VALID_MODES" >&2
			fail=1
		fi

		case "$mode" in
		apply | dry-run)
			if [[ "$lang" != yaml ]]; then
				printf '%s:%s: block "%s" is mode=%s but its fence language is "%s"; apply and dry-run take yaml. A command block is mode=run.\n' \
					"$file" "$line" "$id" "$mode" "$lang" >&2
				fail=1
			fi
			;;
		run | render)
			if [[ "$lang" != sh ]]; then
				printf '%s:%s: block "%s" is mode=%s but its fence language is "%s"; %s takes sh. A manifest block is mode=apply.\n' \
					"$file" "$line" "$id" "$mode" "$lang" "$mode" >&2
				fail=1
			fi
			;;
		skip)
			if [[ "$reason" == "-" ]]; then
				printf '%s:%s: block "%s" is mode=skip with no reason=. A declared gap states why; an undeclared one is indistinguishable from an oversight.\n' \
					"$file" "$line" "$id" >&2
				fail=1
			fi
			;;
		esac

		if [[ "$teardown" != "-" && " $VALID_TEARDOWNS " != *" $teardown "* ]]; then
			printf '%s:%s: block "%s" has teardown=%s; expected one of: %s\n' \
				"$file" "$line" "$id" "$teardown" "$VALID_TEARDOWNS" >&2
			fail=1
		fi

		if [[ "$needs" != "-" ]]; then
			IFS=',' read -ra need_ids <<<"$needs"
			for need in "${need_ids[@]}"; do
				if [[ -z "${seen_ids[$need]:-}" ]]; then
					printf '%s:%s: block "%s" needs "%s", which is not declared earlier in this file. A prerequisite must be applied before its dependent.\n' \
						"$file" "$line" "$id" "$need" >&2
					fail=1
				fi
			done
		fi
	done <<<"$recs"

	if ((count < floor)); then
		printf '%s: %d executed blocks, floor is %d. A block lost its gag:verify annotation or flipped to mode=skip, or the floor in doc-blocks.sh needs lowering deliberately.\n' \
			"$file" "$count" "$floor" >&2
		fail=1
	fi

	unset seen_ids
done

if ((fail)); then
	exit 1
fi

printf 'doc-blocks: %d file(s) checked\n' "${#files[@]}"
