#!/usr/bin/env bash
#
# q822-fs-probe.sh — reads how `go build -o` reaches its destination on this
# runner. Dispatch-only, driven by .github/workflows/q822-fs-probe.yml; it is
# an instrument, not a gate, and never fails on what it measures.
#
# WHY. Before #1898 every merge driver built into one shared .build/mergedriver
# and exec'd it, so a second driver rebuilding under a concurrent exec could
# strike the destination mid-exec. That is a hazard only on go's copy path: go
# links into its temp work dir and then calls moveOrCopyFile, which is
# os.Rename when work dir and destination share a filesystem, and an unlink
# plus a stream copy when they do not. The copy is what opens the window —
# ETXTBSY while the new image is open for writing, ENOENT between the unlink
# and the create. Reproduced independently at 99.8% on a forced cross-device
# layout; whether any runner here takes that path is what this reads.
#
# The fix does not depend on the answer: it builds to a private temp path in
# the destination directory and renames within that directory, which is atomic
# on any filesystem. This settles whether the pre-fix code was ever hazardous
# in CI or only on a layout no runner has.
#
# WHAT IT READS. go branches on os.Rename failing with EXDEV, so the probe
# takes that condition directly rather than inferring it from go's internals:
# it attempts a real link/rename from go's work dir into .build/ and reports
# which way it went. The device ids are printed beside it as the explanation.
#
# Deliberately NOT read by inode: go's copy branch unlinks the destination
# before creating it, so a placed file is a fresh inode on both paths and an
# inode comparison cannot tell them apart. GNU coreutils and Linux only, which
# is what both runner types are.

set -euo pipefail
shopt -s inherit_errexit

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly ROOT
readonly BUILD_DIR="$ROOT/.build"
readonly BIN="$BUILD_DIR/q822probe"
readonly SRC_DIR="$BUILD_DIR/q822probe-src"

# Prints device id and filesystem type for a directory, or a reason it could
# not. Never fails the run: a missing path is a reading, not an error.
describe_dir() {
	local label="$1" dir="$2"
	if [[ -z "$dir" ]]; then
		printf '%-14s (unset)\n' "$label"
		return 0
	fi
	if [[ ! -d "$dir" ]]; then
		printf '%-14s %s (does not exist)\n' "$label" "$dir"
		return 0
	fi
	printf '%-14s %s  dev=%s  fstype=%s\n' "$label" "$dir" \
		"$(stat -c %d "$dir")" "$(stat -f -c %T "$dir")"
}

# Where go links its output before placing it. GOTMPDIR wins; otherwise go
# falls back to the standard temp dir, which is TMPDIR when set and /tmp when
# it is not.
effective_work_dir() {
	local gotmp
	gotmp="$(go env GOTMPDIR)"
	if [[ -n "$gotmp" ]]; then
		printf '%s\n' "$gotmp"
		return 0
	fi
	printf '%s\n' "${TMPDIR:-/tmp}"
}

cmd_devices() {
	mkdir -p "$BUILD_DIR"
	echo "== go build placement inputs =="
	go version
	describe_dir 'GOTMPDIR' "$(go env GOTMPDIR)"
	describe_dir 'TMPDIR' "${TMPDIR:-}"
	describe_dir 'RUNNER_TEMP' "${RUNNER_TEMP:-}"
	describe_dir 'work dir' "$(effective_work_dir)"
	describe_dir '.build' "$BUILD_DIR"
}

# Attempts the syscall go's placement branches on, from go's own work dir into
# .build/. `ln` and rename(2) fail with the same EXDEV across a filesystem
# boundary; `ln` is used because it needs nothing but coreutils, and python3's
# os.rename is taken as well where the interpreter exists, because it is the
# exact call go makes.
cmd_placement() {
	mkdir -p "$BUILD_DIR"
	local work src dst
	work="$(effective_work_dir)"
	src="$(mktemp "$work/q822probe.XXXXXX")"
	dst="$BUILD_DIR/q822probe.placement"
	rm -f "$dst"

	echo "== placement =="
	printf 'work dir dev: %s\n.build dev:   %s\n' \
		"$(stat -c %d "$work")" "$(stat -c %d "$BUILD_DIR")"

	local verdict='rename'
	if ln "$src" "$dst" 2>"$BUILD_DIR/q822probe.ln.err"; then
		echo 'link across the boundary: OK — same filesystem'
	else
		verdict='copy'
		printf 'link across the boundary: failed — %s\n' "$(cat "$BUILD_DIR/q822probe.ln.err")"
	fi
	rm -f "$dst"

	if command -v python3 >/dev/null 2>&1; then
		python3 - "$src" "$dst" <<-'PY'
			import os, sys
			src, dst = sys.argv[1], sys.argv[2]
			try:
			    os.rename(src, dst)
			except OSError as e:
			    print(f"rename(2) across the boundary: failed — errno {e.errno} {e.strerror}")
			else:
			    print("rename(2) across the boundary: OK — same filesystem")
			    os.rename(dst, src)
		PY
	else
		echo 'rename(2) across the boundary: not read — no python3 on this runner'
	fi
	rm -f "$src" "$dst" "$BUILD_DIR/q822probe.ln.err"

	if [[ "$verdict" == rename ]]; then
		echo 'RESULT: rename — go places atomically here.'
		echo 'The pre-#1898 shared-path build was safe on this runner.'
	else
		echo 'RESULT: copy — go unlinks and streams here.'
		echo 'The pre-#1898 shared-path build was hazardous on this runner.'
	fi
}

# Builds a throwaway program to $BIN and confirms a real `go build -o` places a
# file where the readings above say it will. The nonce lives in the *source*,
# so a second call cannot be served from the build cache and the bytes must
# differ. A fixture rather than the merge driver itself: this measures go's
# placement of an -o path, which does not depend on what was compiled, and a
# probe must not rebuild a binary the repo's own drivers use.
build_with_nonce() {
	local nonce="$1"
	mkdir -p "$SRC_DIR"
	cat >"$SRC_DIR/go.mod" <<-EOF
		module q822probe

		go $(go env GOVERSION | sed 's/^go//')
	EOF
	cat >"$SRC_DIR/main.go" <<-EOF
		package main

		func main() { println("$nonce") }
	EOF
	(cd "$SRC_DIR" && GOWORK=off go build -o "$BIN" .)
}

cmd_build() {
	mkdir -p "$BUILD_DIR"
	rm -rf "$BIN" "$SRC_DIR"
	build_with_nonce probe-a
	local before after
	before="$(sha256sum "$BIN" | cut -d' ' -f1)"
	build_with_nonce probe-b
	after="$(sha256sum "$BIN" | cut -d' ' -f1)"

	echo "== go build -o control =="
	printf 'before: %s\nafter:  %s\n' "$before" "$after"
	if [[ "$before" == "$after" ]]; then
		echo 'NO READING: the second build placed nothing, so the placement'
		echo 'reading above describes a path go did not take here.'
	else
		echo 'OK: a second go build -o to the same path replaced the file.'
	fi
	rm -rf "$BIN" "$SRC_DIR"
}

main() {
	case "${1:-}" in
	devices) cmd_devices ;;
	placement) cmd_placement ;;
	build) cmd_build ;;
	*)
		echo "usage: ${BASH_SOURCE[0]##*/} devices|placement|build" >&2
		exit 2
		;;
	esac
}

main "$@"
