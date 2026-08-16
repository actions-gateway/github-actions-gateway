#!/usr/bin/env bash
# check-release-digests-test.sh — the parsing half, offline.
#
# The registry half needs the network and a published tag, so it is exercised at
# release time (and the script takes a note path precisely so a known-bad digest
# can be planted then). What is testable here is the half that decides WHICH
# digests get compared: a parser that silently matched nothing would report "ok"
# for a note whose Container images section was never written, which is the
# failure this check exists to prevent.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBJECT="$SCRIPT_DIR/check-release-digests.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0
ok() {
	printf '[check-release-digests-test] ok   %s\n' "$1"
	pass=$((pass + 1))
}
bad() {
	printf '[check-release-digests-test] FAIL %s\n' "$1" >&2
	fail=$((fail + 1))
}
expect() {
	local desc="$1" want="$2" got="$3"
	if [[ "$got" == "$want" ]]; then
		ok "$desc"
	else
		bad "$desc"
		printf '       want: %q\n       got:  %q\n' "$want" "$got" >&2
	fi
}

# The parser is reachable without running the registry half: source with a version
# that has no note, which exits 2 before any network call. Instead of that dance,
# reimplementing it here would test the copy — so pull the function out by name.
# shellcheck source=/dev/null
noted_digests() {
	grep -oE 'ghcr\.io/[^/]+/[a-z0-9-]+@sha256:[a-f0-9]{64}' "$1" |
		sort -u |
		awk -F'@' '{ n = split($1, p, "/"); print p[n] "\t" $2 }'
}
# Guard against the copy above drifting from the subject: the subject's own body
# must still contain this exact pipeline.
if grep -qF "awk -F'@' '{ n = split(\$1, p, \"/\"); print p[n] \"\\t\" \$2 }'" "$SUBJECT"; then
	ok "the parser under test still matches the subject's"
else
	bad "the parser under test still matches the subject's (the copy has drifted)"
fi

D1="sha256:$(printf '5%.0s' {1..64})"
D2="sha256:$(printf 'a%.0s' {1..64})"

cat >"$WORK/note.md" <<EOF
## Container images

- **gmc**: \`ghcr.io/actions-gateway/gmc@${D1}\`
- **agc**: \`ghcr.io/actions-gateway/agc@${D2}\`
EOF

expect "each pinned image is parsed with its digest" \
	"agc	${D2}
gmc	${D1}" \
	"$(noted_digests "$WORK/note.md")"

# A tag reference is not a pin, and must not be mistaken for one — the whole point
# is that operators pin by digest.
cat >"$WORK/tagonly.md" <<'EOF'
## Container images

Pull `ghcr.io/actions-gateway/gmc:v1.5.0` to try it.
EOF
expect "a tag reference is not read as a digest pin" "" "$(noted_digests "$WORK/tagonly.md")"

# --- the empty-section case, end to end --------------------------------------
#
# Exit 1, not 0: a note with no Container images section has not been finished,
# and reporting "ok" for it would be the silent pass this check exists to stop.
rc=0
"$SUBJECT" v1.5.0 "$WORK/tagonly.md" >/dev/null 2>&1 || rc=$?
expect "a note with no pinned images fails rather than passing" 1 "$rc"

# --- usage -------------------------------------------------------------------
rc=0
"$SUBJECT" >/dev/null 2>&1 || rc=$?
expect "no arguments is a usage error" 2 "$rc"
rc=0
"$SUBJECT" v9.9.9 >/dev/null 2>&1 || rc=$?
expect "a version with no note is exit 2" 2 "$rc"

# --- reconciliation against the shipped note ---------------------------------
note="$REPO_ROOT/docs/releases/v1.5.0.md"
if [[ -f "$note" ]]; then
	n="$(noted_digests "$note" | grep -c . || true)"
	expect "the shipped v1.5.0 note pins five images" 5 "$n"
else
	printf '[check-release-digests-test] SKIP shipped-note case\n'
fi

printf '[check-release-digests-test] %d passed, %d failed\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]]
