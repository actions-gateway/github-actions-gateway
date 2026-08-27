#!/usr/bin/env bash
#
# check-endpoint-parity-test.sh — assertions for check-endpoint-parity.sh.
#
# The Go checker's own cases live in devtools/e2e/endpointparity/main_test.go and
# cover what each finding means. What only this suite can cover is the entry
# point: that the gate defaults to the shipped paths, that it reports the real
# tree as clean, that it fails on a venue missing an endpoint rather than passing
# by checking nothing, and that it refuses rather than reporting parity when the
# fake stops marking its unserved answers.
#
# That last case is the one worth the fixtures. Every negative the gate takes
# rests on the marker arriving, so a fake that stopped sending it would report
# full parity with nothing checked — the exact failure this gate exists to end.
#
# Both directions are asserted throughout. The shipped-tree case is the one that
# would go quiet if the gate ever stopped checking anything, so it is paired with
# the failure cases rather than standing alone.

set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="$SCRIPT_DIR/check-endpoint-parity.sh"

failures=0

ok() { printf 'ok   %s\n' "$1"; }
fail() {
    printf 'FAIL %s\n' "$1"
    failures=$((failures + 1))
}

# expect_rc NAME EXPECTED_RC -- command...
#
# The command's output is captured rather than discarded, and printed when the
# status is not the wanted one. Discarding it left the shipped-tree case
# reporting a bare "expected rc=0, got rc=2" — the one case whose whole
# diagnosis is the checker's stderr, and the reason a red run here yielded
# nothing to act on (Q912).
expect_rc() {
    local name="$1" want="$2" rc=0 out
    shift 3
    out="$("$@" 2>&1)" || rc=$?
    if ((rc == want)); then
        ok "$name (rc=$rc)"
    else
        fail "$name: expected rc=$want, got rc=$rc"
        printf '%s\n' "$out" | awk '{ print "     | " $0 }' >&2
    fi
}

# expect_output NAME NEEDLE -- command...
expect_output() {
    local name="$1" needle="$2" out
    shift 3
    out="$("$@" 2>&1 || true)"
    if [[ "$out" == *"$needle"* ]]; then
        ok "$name"
    else
        fail "$name: output did not contain '$needle'; got: $out"
    fi
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# The shipped tree is clean, with no arguments.
expect_rc "shipped tree passes with no arguments" 0 -- "$GATE"

# Argument handling. A lone argument is ambiguous — a fake with no source roots
# would check nothing and pass.
expect_rc "one argument is refused" 2 -- "$GATE" test/fakegithub
expect_rc "a missing source root is refused" 2 -- "$GATE" test/fakegithub no/such/dir

# Fixtures. A caller with one request site, and a fake that either serves that
# path or does not, so the gate is exercised in both directions against a venue
# small enough to state in full.
caller="$TMP/caller"
mkdir -p "$caller"
cat >"$caller/call.go" <<'EOF'
package caller

import (
	"context"
	"fmt"
	"net/http"
)

func Call(ctx context.Context, base, id string) {
	u := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%s", base, "o", "r", id)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	_ = req
}
EOF

# write_fake DIR SERVED_PREFIX MARKER — a fake serving one prefix. MARKER=off
# drops the unserved header, which is the instrument the gate's negatives rest on.
write_fake() {
    local dir="$1" prefix="$2" marker="$3"
    mkdir -p "$dir"
    printf 'module minifake\n\ngo 1.26.6\n' >"$dir/go.mod"
    cat >"$dir/main.go" <<EOF
package main

import (
	"net/http"
	"os"
	"strings"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "$prefix") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if "$marker" != "off" {
			w.Header().Set("X-Fakegithub-Unserved", "1")
		}
		http.NotFound(w, r)
	})
	go func() { _ = http.ListenAndServe(os.Getenv("CONTROL_ADDR"), mux) }()
	_ = http.ListenAndServe(os.Getenv("ADDR"), mux)
}
EOF
}

write_fake "$TMP/serving" "/repos/" on
write_fake "$TMP/missing" "/nothing/" on
write_fake "$TMP/unmarked" "/nothing/" off

expect_rc "a fake serving the endpoint passes" 0 -- "$GATE" "$TMP/serving" "$caller"
expect_rc "a fake missing the endpoint fails" 1 -- "$GATE" "$TMP/missing" "$caller"
expect_output "the failure names the endpoint and the call site" \
    "serves no GET /repos/x/x/actions/runs/x" -- "$GATE" "$TMP/missing" "$caller"
expect_output "the failure names the caller" "call.go:" -- "$GATE" "$TMP/missing" "$caller"

# A fake that dies during startup must be reported, and reported promptly. This
# is the path where the checker waited on the child's exit status in two places
# and deadlocked, which fails a gate by hanging it: worse than a wrong verdict,
# because a parallel runner just stops. `timeout` is the assertion.
dead="$TMP/dead"
mkdir -p "$dead"
printf 'module minifake\n\ngo 1.26.6\n' >"$dead/go.mod"
cat >"$dead/main.go" <<'EOF'
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "listen tcp: bind: address already in use")
	os.Exit(1)
}
EOF
expect_rc "a fake that exits during startup is reported, not waited on" 2 \
    -- timeout 60 "$GATE" "$dead" "$caller"
expect_output "the report quotes what the fake said" "address already in use" \
    -- timeout 60 "$GATE" "$dead" "$caller"

# The instrument check. Without the marker every path reads as served, so the
# gate must refuse rather than report the parity it cannot measure.
expect_rc "an unmarked fake is refused, not passed" 2 -- "$GATE" "$TMP/unmarked" "$caller"
expect_output "the refusal names the missing marker" "X-Fakegithub-Unserved" \
    -- "$GATE" "$TMP/unmarked" "$caller"

if ((failures > 0)); then
    printf '\n%d assertion(s) failed\n' "$failures" >&2
    exit 1
fi
printf '\nall assertions passed\n'
