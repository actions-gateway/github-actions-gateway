#!/usr/bin/env bash
#
# Unit tests for scripts/docs/alloc-queue-id.sh — the reservation half of
# `make queue-id`. (The near-duplicate search it runs first has its own suite,
# find-duplicate-rows-test.sh.)
#
# The claim is a compare-and-swap on a ref name, and its whole value is what it
# does under concurrency — which is exactly what reading it cannot tell you. So
# the suite runs a fleet of allocators at once and asks whether they came away
# with different IDs.
#
# Two traps this repo has already paid for shape the fleet:
#
#   - An aggregate counter cannot count distinct participants (Q601). Each
#     worker writes its OWN id/rc/stderr files, and the assertion is on the set
#     of per-worker IDs, not on a total.
#   - A first-try green proves nothing until the mechanism is deleted. The last
#     case reruns the identical fleet against a `gh` stub whose create-ref
#     reports success and reserves nothing — the Q656 defect, verbatim — and
#     requires the fleet to collide. Without that case a suite asserting
#     "distinct IDs" passes just as happily on eight serialised runs.
#
# Nothing here touches the real remote: `origin` is a bare repo under a temp
# dir, and the `gh` stub — which shadows any real gh on PATH — writes refs only
# into that bare repo, via the same must-not-exist test-and-set the GitHub
# create-ref API performs server-side.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
ALLOC="$REPO_ROOT/scripts/docs/alloc-queue-id.sh"

# The fleet size, and the highest ID the fixture backlog carries. Eight workers
# from a floor of 900 must come away with Q901..Q908.
readonly FLEET=8
readonly FLOOR=900

fails=0
WORK="$(mktemp -d)"

# A label cell carries backticks, and SC2016 reads a literal backtick in a
# single-quoted string as legacy command substitution. Carrying one in a
# variable keeps the fixture row faithful without the false positive.
bt="$(printf '\140')"

git_id=(-c user.email=t@t -c user.name=t)

# shellcheck disable=SC2329 # invoked by `trap cleanup EXIT`; shellcheck 0.11
# misses that whenever the script ends in an explicit `exit`.
cleanup() {
	rm -rf "$WORK"
}
trap cleanup EXIT

ok() {
	printf 'ok   %s\n' "$1"
}

bad() {
	printf 'FAIL %s: %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

# stub_gh PATH_TO_STUB — write a `gh` that answers the two calls the allocator
# makes. Its create-ref honours FIXTURE_CLAIM_MODE:
#
#   cas  — `git update-ref <ref> <sha> ""`, whose empty old-value means "must
#          not exist". That is the server-side atomic test-and-set, run against
#          the fixture origin.
#   noop — report success, reserve nothing. The mechanism deleted.
stub_gh() {
	cat >"$1" <<-'STUB'
		#!/usr/bin/env bash
		set -euo pipefail

		case "${1:-}" in
		repo) printf 'fixture/backlog\n'; exit 0 ;;
		api) ;;
		*) printf 'gh stub: unexpected call: %s\n' "$*" >&2; exit 64 ;;
		esac

		ref='' sha=''
		while (($# > 0)); do
			case "$1" in
			-f)
				case "${2:-}" in
				ref=*) ref="${2#ref=}" ;;
				sha=*) sha="${2#sha=}" ;;
				esac
				shift 2
				;;
			*) shift ;;
			esac
		done
		[[ -n "$ref" && -n "$sha" ]] || { printf 'gh stub: no ref/sha in call\n' >&2; exit 64; }

		if [[ "${FIXTURE_CLAIM_MODE:-cas}" == noop ]]; then
			exit 0
		fi

		if err="$(git --git-dir="$FIXTURE_ORIGIN" update-ref "$ref" "$sha" "" 2>&1)"; then
			exit 0
		fi
		# Only a name that is already taken is a collision. Anything else is a
		# broken fixture, and reporting it as 422 would hand the allocator 25
		# phantom collisions instead of an error — the same misreading claim()
		# guards against for a network or auth failure.
		if [[ "$err" == *'already exists'* ]]; then
			# gh renders a 422 body on stderr; claim() keys on this text.
			printf 'gh: Reference already exists (HTTP 422)\n' >&2
			exit 1
		fi
		printf 'gh stub: update-ref failed, and not on an existing name: %s\n' "$err" >&2
		exit 64
	STUB
	chmod +x "$1"
}

# fixture DIR — build an isolated origin/worktree pair and a `gh` stub under
# DIR. The backlog it writes carries one row, Q900, so the floor is known.
fixture() {
	local dir="$1"
	mkdir -p "$dir/bin" "$dir/out" "$dir/repo/docs"
	git init -q --bare "$dir/origin.git"
	git init -q "$dir/repo"
	{
		printf '# Project Status\n\n## Queue\n\n'
		printf '| ID | Item | Labels | St | Sz | Notes |\n|---|---|---|---|---|---|\n'
		printf '| <a id="Q%d"></a>Q%d | [a row that already exists](x.md) | %s | 🔲 | S | notes |\n' \
			"$FLOOR" "$FLOOR" "${bt}debt${bt}"
	} >"$dir/repo/docs/STATUS.md"
	git -C "$dir/repo" add docs/STATUS.md
	git -C "$dir/repo" "${git_id[@]}" commit -qm 'fixture backlog'
	git -C "$dir/repo" remote add origin "$dir/origin.git"
	# Claims anchor to the root commit, so the origin has to carry that object.
	git -C "$dir/repo" push -q origin HEAD:refs/heads/main
	stub_gh "$dir/bin/gh"
}

# fleet DIR — run FLEET allocators against DIR's fixture, all released at the
# same instant, each writing its own id/rc/err files under DIR/out.
fleet() {
	local dir="$1" i ready=0 waited=0
	export FIXTURE_ORIGIN="$dir/origin.git"
	export PATH="$dir/bin:$PATH"

	for ((i = 1; i <= FLEET; i++)); do
		(
			cd "$dir/repo" || exit 1
			: >"$dir/out/ready.$i"
			# Released together below, so the claims genuinely overlap. A fleet
			# that trickles in serialises, and serialised runs never collide —
			# they would pass with no reservation at all.
			while [[ ! -e "$dir/out/go" ]]; do sleep 0.01; done
			local rc=0
			"$ALLOC" "worker $i on an unrelated subject line" \
				>"$dir/out/id.$i" 2>"$dir/out/err.$i" || rc=$?
			printf '%s\n' "$rc" >"$dir/out/rc.$i"
		) &
	done

	while ((ready < FLEET)); do
		ready="$(find "$dir/out" -name 'ready.*' -type f | wc -l)"
		((waited++ < 1000)) || {
			bad 'fleet startup' "only $ready of $FLEET workers came up within 10s"
			break
		}
		sleep 0.01
	done

	: >"$dir/out/go"
	wait
}

# ids DIR — every worker's allocated ID, one per line, in worker order.
ids() {
	local i
	for ((i = 1; i <= FLEET; i++)); do
		printf '%s\n' "$(cat "$1/out/id.$i" 2>/dev/null || true)"
	done
}

# --- Concurrent claims hand out distinct IDs -------------------------------

fixture "$WORK/cas"
fleet "$WORK/cas"

rcs="$(cat "$WORK"/cas/out/rc.* 2>/dev/null | sort -u | tr '\n' ' ')"
if [[ "$rcs" == "0 " ]]; then
	ok 'every concurrent allocator exits 0'
else
	bad 'every concurrent allocator exits 0' \
		"exit codes were: $rcs — stderr: $(cat "$WORK"/cas/out/err.* 2>/dev/null | head -5)"
fi

got="$(ids "$WORK/cas" | sort | tr '\n' ' ')"
want=''
for ((n = 1; n <= FLEET; n++)); do
	want+="Q$((FLOOR + n)) "
done
if [[ "$got" == "$want" ]]; then
	ok "$FLEET concurrent allocators take $FLEET distinct IDs"
else
	bad "$FLEET concurrent allocators take $FLEET distinct IDs" "want [$want], got [$got]"
fi

distinct="$(ids "$WORK/cas" | sort -u | grep -c '^Q[0-9]' || true)"
if ((distinct == FLEET)); then
	ok "each of the $FLEET workers observed its own ID"
else
	bad "each of the $FLEET workers observed its own ID" \
		"$distinct distinct non-empty IDs across $FLEET workers"
fi

claimed="$(git --git-dir="$WORK/cas/origin.git" for-each-ref --format='%(refname)' 'refs/queue-ids/*' | wc -l)"
if ((claimed == FLEET)); then
	ok 'every ID handed out left a claim on the remote'
else
	bad 'every ID handed out left a claim on the remote' \
		"$claimed refs under refs/queue-ids/ for $FLEET IDs"
fi

# --- Delete the mechanism: the same fleet must now collide ------------------
#
# The claim reports success and reserves nothing, which is Q656's defect
# exactly. Every worker then reads the same floor — the namespace stays empty,
# so timing cannot save it — and prints the same ID.

fixture "$WORK/noop"
FIXTURE_CLAIM_MODE=noop
export FIXTURE_CLAIM_MODE
fleet "$WORK/noop"
unset FIXTURE_CLAIM_MODE

distinct="$(ids "$WORK/noop" | sort -u | grep -c '^Q[0-9]' || true)"
if ((distinct < FLEET)); then
	ok "without the claim, the same fleet collides ($distinct distinct IDs for $FLEET workers)"
else
	bad 'without the claim, the same fleet collides' \
		"$FLEET workers still took $FLEET distinct IDs, so the assertion above does not test the claim"
fi

# --- The no-reserve report is gone (Q656) ----------------------------------

if out="$("$ALLOC" --peek 2>&1)"; then
	bad '--peek is rejected' "expected a non-zero exit, got 0 and: $out"
elif [[ "$out" == *'unknown argument: --peek'* ]]; then
	ok '--peek is rejected: an ID you can read without holding is the old counter'
else
	bad '--peek is rejected' "want 'unknown argument: --peek', got: $out"
fi

if ((fails > 0)); then
	printf '\nalloc-queue-id-test: %d assertion(s) failed\n' "$fails" >&2
	exit 1
fi

printf '\nalloc-queue-id-test: all assertions passed\n'
exit 0
