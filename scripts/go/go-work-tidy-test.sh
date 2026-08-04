#!/usr/bin/env bash
#
# Unit tests for scripts/go/go-work-tidy.sh (Q667). One property: the script
# tidies every Go module the repo tracks, not just the go.work members.
#
# That property is the whole of tidy-check's reach. The gate re-runs this script
# and then diffs '**/go.mod' across the repo, so the diff already covered
# devtools/ and tools/ — but nothing rewrote them, and a tidy defect committed
# in both passed the gate clean. A module the script never visits is a module
# the gate cannot fail on, silently.
#
# `go mod tidy` needs a warm module cache (a network on a cold one) and rewrites
# tracked files, so the assertions run the script against a PATH shim that
# records each tidy's working directory and forwards every other `go`
# subcommand to the real toolchain. Runs under `make check` (via `make
# scripts-test`) and the CI shellcheck job.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

fails=0
WORK="$REPO_ROOT/tmp/go-work-tidy-test.$$"
trap 'rm -rf "$WORK"' EXIT INT TERM
mkdir -p "$WORK/bin"

REAL_GO="$(command -v go)"
TIDY_LOG="$WORK/tidied.txt"
: >"$TIDY_LOG"

cat >"$WORK/bin/go" <<EOF
#!/usr/bin/env bash
set -euo pipefail
if [[ "\${1:-}" == mod && "\${2:-}" == tidy ]]; then
	printf '%s\n' "\$PWD" >>"$TIDY_LOG"
	exit 0
fi
exec "$REAL_GO" "\$@"
EOF
chmod +x "$WORK/bin/go"

PATH="$WORK/bin:$PATH" scripts/go/go-work-tidy.sh

# Repo-relative, newline-delimited, for substring membership tests.
tidied=$'\n'
tidied_count=0
while IFS= read -r dir; do
	[[ -n "$dir" ]] || continue
	tidied+="${dir#"$REPO_ROOT"/}"$'\n'
	tidied_count=$((tidied_count + 1))
done <"$TIDY_LOG"

# expect_tidied DIR — DIR received a `go mod tidy`.
expect_tidied() {
	local dir="$1"
	if [[ "$tidied" == *$'\n'"$dir"$'\n'* ]]; then
		printf 'ok   tidied   %s\n' "$dir"
	else
		printf 'FAIL untidied %s (go-work-tidy.sh never visited it)\n' "$dir" >&2
		fails=$((fails + 1))
	fi
}

# expect_member NAME HAYSTACK NEEDLE WANT — WANT is 'yes' or 'no'.
expect_member() {
	local name="$1" haystack="$2" needle="$3" want="$4" got=no
	[[ $'\n'"$haystack"$'\n' == *$'\n'"$needle"$'\n'* ]] && got=yes
	if [[ "$got" == "$want" ]]; then
		printf 'ok   member   %-34s %s\n' "$name" "$got"
	else
		printf 'FAIL member   %-34s want=%s got=%s\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

# Asserted per module discovered from the checkout rather than by naming
# devtools and tools, so a module added later is covered without editing this
# file — the shape Q670 settled on for go-lint's scoping.
module_count=0
while IFS= read -r dir; do
	[[ -n "$dir" ]] || continue
	expect_tidied "${dir#./}"
	module_count=$((module_count + 1))
done < <(workspace_modules; nonworkspace_modules)

# Control: the workspace modules are still tidied exactly once each, and no
# extra directory picked one up. A count check is what separates "covers every
# module" from "tidies the tree twice over".
if (( tidied_count == module_count )); then
	printf 'ok   count    %d modules tidied, %d expected\n' "$tidied_count" "$module_count"
else
	printf 'FAIL count    %d modules tidied, %d expected\n' "$tidied_count" "$module_count" >&2
	fails=$((fails + 1))
fi

# Control: the two views of the non-workspace set differ by `tools/` alone. The
# tidy flow needs the whole-repo view — `go mod tidy` normalises a module's
# files whoever wrote its imports — while the lint/test/vulncheck gates reason
# about first-party code only.
nonworkspace="$(nonworkspace_modules)"
firstparty="$(firstparty_nonworkspace_modules)"
expect_member 'tools is a non-workspace module' "$nonworkspace" tools yes
expect_member 'tools is not first-party' "$firstparty" tools no
expect_member 'devtools is first-party' "$firstparty" devtools yes

if (( fails > 0 )); then
	echo "go-work-tidy-test: $fails failure(s)" >&2
	exit 1
fi
echo "go-work-tidy-test: all assertions passed"
