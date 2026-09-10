#!/usr/bin/env bash
#
# merge-driver-common.sh — the shell every merge driver in this repo shares.
#
# It is sourced, never executed, and it carries two halves while the port to Go
# is under way (Q1046, Q1047):
#
#   merge_driver_exec  the ported drivers (scriptindex, planindex). Handles
#                      --help and --install, then builds and execs
#                      devtools/git/mergedriver, which is where the merge
#                      happens. A caller sets DRIVER_SUBCOMMAND.
#   merge_driver_init  the drivers still implemented in shell (roadmap, Q1046;
#                      gatelists, Q1047), with the argument handling, conflict
#                      labels and fallback they merge with. Goes when they do.
#
# The entry point stays shell either way, because `git config
# merge.<name>.driver` stores a path git runs directly: it has to work in a
# clone where nothing has been built yet. --install sits ahead of the build for
# the same reason — installing is metadata and must not need a toolchain.
#
# Callers set, before merge_driver_exec:
#   DRIVER_SUBCOMMAND  the mergedriver subcommand (merge_driver_exec only)
#   DEFAULT_PATH       the file this driver merges (merge_driver_init only)
#   DRIVER_NAME        the `merge.<name>` git config key
#   DRIVER_LOG         the prefix on this driver's stderr lines
#   DRIVER_PATH        the driver's repo-relative path, written into that config
#   DRIVER_DESC        the config's human-readable `.name`
#   DRIVER_SELF        ${BASH_SOURCE[0]} of the caller, for --help
set -euo pipefail
shopt -s inherit_errexit

# merge_driver_note MSG — one line of driver commentary on stderr, so a
# resolution (or a refusal) is never silent.
merge_driver_note() {
	printf '%s: %s\n' "$DRIVER_LOG" "$1" >&2
}

# _merge_driver_build BIN ERRFILE — build the driver binary, replacing BIN
# rather than rewriting it, and keeping the build's output in ERRFILE so a
# failure names a cause instead of vanishing (Q822).
#
# BIN is one path shared by every invocation, and invocations overlap: each
# merge rebuilds, and `make scripts-test` runs the scriptindex and planindex
# suites concurrently inside a 119-wide fan-out. A cache hit writes nothing, so
# the only contended moment is a cache miss, where go places the binary.
#
# How it places decides whether that is safe. Same filesystem it renames, which
# is atomic. Cross filesystem it unlinks and streams a copy, so a concurrent
# exec of BIN fails either way: ETXTBSY for the length of the stream, when BIN
# is an incomplete file held open for writing, and ENOENT in the gap between
# the unlink and the create. Measured 2026-09-10 on go1.26.8/linux-amd64 in a
# container, replicating that shape against a concurrent execve: 4,106 of 4,109
# execs failed, almost all ETXTBSY, and reproduced independently at 99.8% with
# a larger ENOENT share: the split moves with timing, the total does not.
#
# Building to a sibling of BIN and renaming takes the atomic path whichever
# route go took into the temp, and every build emits the same bytes so the
# winner does not matter.
#
# devtools/ is outside the Go workspace, hence GOWORK=off; see
# docs/development/go-workspaces.md.
_merge_driver_build() {
	local bin="$1" errfile="$2" root tmp
	root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
	mkdir -p "$(dirname "$bin")"
	tmp="$bin.build.$$"
	if ! (cd "$root/devtools" && GOWORK=off go build -o "$tmp" ./git/mergedriver) >"$errfile" 2>&1; then
		rm -f "$tmp"
		return 1
	fi
	if ! mv -f "$tmp" "$bin" >>"$errfile" 2>&1; then
		rm -f "$tmp"
		return 1
	fi
}

# _merge_driver_unbuilt_fallback BASE OURS THEIRS MARKER PATH ERRFILE — the one
# path that cannot go through the Go driver, because the Go driver is what
# failed to build. Redo the merge the way git would have and keep whatever it
# produces, exactly as an uncertain merge does. Exit status stays under 128 so
# git records a conflict rather than reading the driver as crashed.
#
# ERRFILE is the build's own output, relayed line by line under this driver's
# prefix. Without it the reason is `the driver could not be built` and nothing
# else, which is a symptom no one can act on: three CI sightings of Q822 were
# spent establishing that a build had failed, and none could say why. The race
# above is not that failure — it kills an exec, not a build — so this relay is
# what identifies the cause when Q822 next fires.
_merge_driver_unbuilt_fallback() {
	local base="$1" ours="$2" theirs="$3" marker="$4" path="$5" errfile="$6" rc=0 line
	if [[ -s "$errfile" ]]; then
		merge_driver_note 'the build failed with:'
		while IFS= read -r line; do
			merge_driver_note "  $line"
		done <"$errfile"
	fi
	git merge-file --marker-size="$marker" "$ours" "$base" "$theirs" >/dev/null 2>&1 || rc=$?
	if (( rc == 0 )); then
		merge_driver_note "the driver could not be built; the plain three-way merge resolved it cleanly"
		exit 0
	fi
	merge_driver_note "the driver could not be built; left ordinary conflict markers in $path"
	exit 1
}

# merge_driver_install — point this clone's git config at the driver. Repo-local
# by construction (never --global), and the script path stays relative so it
# resolves in the main checkout and in every linked worktree, which share one
# config file. The same reason core.hooksPath is relative in `make hooks`.
#
# Deliberately ahead of the build: installing is metadata, so it must work in a
# clone with no Go toolchain and before anything has been compiled.
merge_driver_install() {
	git config "merge.$DRIVER_NAME.name" "$DRIVER_DESC"
	git config "merge.$DRIVER_NAME.driver" "$DRIVER_PATH %O %A %B %L %P %S %X %Y"
	printf 'merge driver installed: merge.%s -> %s\n' "$DRIVER_NAME" "$DRIVER_PATH"
	[[ -n "${DRIVER_INSTALL_NOTE:-}" ]] && printf '%s\n' "$DRIVER_INSTALL_NOTE"
	return 0
}

# merge_driver_exec "$@" — handle --help/--install, then hand the merge to the
# binary.
merge_driver_exec() {
	if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
		# The caller's header comment block is the documentation; print it
		# without the `#`.
		awk 'NR == 1 { next } /^#/ { sub(/^#[ ]?/, ""); print; next } { exit }' "$DRIVER_SELF"
		exit 0
	fi

	if [[ "${1:-}" == "--install" ]]; then
		merge_driver_install
		exit 0
	fi

	local root bin builderr
	root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
	bin="$root/.build/mergedriver"

	builderr="$(mktemp "${TMPDIR:-/tmp}/${DRIVER_NAME}-build.XXXXXX")"
	trap 'rm -f "$builderr"' EXIT

	if ! _merge_driver_build "$bin" "$builderr"; then
		local marker="${4:-7}"
		[[ "$marker" =~ ^[0-9]+$ ]] && (( marker >= 7 )) || marker=7
		_merge_driver_unbuilt_fallback "${1:-}" "${2:-}" "${3:-}" "$marker" "${5:-the merged file}" "$builderr"
	fi

	# exec replaces this process, so the EXIT trap never runs past here.
	rm -f "$builderr"
	exec "$bin" "$DRIVER_SUBCOMMAND" "$@"
}

# merge_driver_first_line FILE DEFAULT — FILE's first line, or DEFAULT when it
# has none. Used to turn a helper's stderr into a fallback reason without ever
# producing an empty one.
merge_driver_first_line() {
	local line=""
	[[ -s "$1" ]] && IFS= read -r line <"$1"
	printf '%s' "${line:-$2}"
}

# merge_driver_fallback REASON — the only exit path for an uncertain merge: redo
# the merge the way git would have without this driver, and keep whatever it
# produces. Clean means nothing was actually contested, so it exits 0; otherwise
# the file carries ordinary conflict markers and the driver reports a conflict.
#
# Exit status stays under 128 either way: git reads >128 as "the driver
# crashed", which fails the whole merge instead of recording a conflict.
merge_driver_fallback() {
	local reason="$1" rc=0
	git merge-file --marker-size="$MARKER_SIZE" \
		-L "$LABEL_OURS" -L "$LABEL_BASE" -L "$LABEL_THEIRS" \
		"$OURS_FILE" "$BASE_FILE" "$THEIRS_FILE" >/dev/null 2>&1 || rc=$?
	if (( rc == 0 )); then
		merge_driver_note "$reason; the plain three-way merge resolved it cleanly"
		exit 0
	fi
	merge_driver_note "$reason; left ordinary conflict markers in $TARGET_PATH"
	exit 1
}

# _merge_driver_label VALUE FALLBACK — git passes %S/%X/%Y only from 2.44 on; an
# older git leaves the placeholder unexpanded, so a value that still looks like
# one is not a label.
_merge_driver_label() {
	local value="$1" fallback="$2"
	if [[ -z "$value" || "$value" == %* ]]; then
		printf '%s' "$fallback"
	else
		printf '%s' "$value"
	fi
}

# merge_driver_init "$@" — handle --install/--help, validate the placeholders,
# and set up everything merge_driver_fallback needs. Exits directly for the
# non-merge invocations.
merge_driver_init() {
	if [[ "${1:-}" == "--install" ]]; then
		merge_driver_install
		exit 0
	fi

	if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
		# The caller's header comment block is the documentation; print it
		# without the `#`.
		awk 'NR == 1 { next } /^#/ { sub(/^#[ ]?/, ""); print; next } { exit }' "$DRIVER_SELF"
		exit 0
	fi

	if (( $# < 3 )); then
		merge_driver_note 'expected the %O %A %B placeholders; run --install to configure git, or --help'
		exit 2
	fi

	BASE_FILE="$1"
	OURS_FILE="$2"
	THEIRS_FILE="$3"

	MARKER_SIZE="${4:-7}"
	if [[ ! "$MARKER_SIZE" =~ ^[0-9]+$ ]] || (( MARKER_SIZE < 7 )); then
		MARKER_SIZE=7
	fi

	TARGET_PATH="${5:-$DEFAULT_PATH}"

	LABEL_BASE="$(_merge_driver_label "${6:-}" "$TARGET_PATH (base)")"
	LABEL_OURS="$(_merge_driver_label "${7:-}" "$TARGET_PATH (ours)")"
	LABEL_THEIRS="$(_merge_driver_label "${8:-}" "$TARGET_PATH (theirs)")"

	WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/${DRIVER_NAME}-merge.XXXXXX")"
	trap 'rm -rf "$WORKDIR"' EXIT

	# Any unexpected internal failure is just another uncertainty.
	trap 'merge_driver_fallback "the driver failed unexpectedly"' ERR
}
