#!/usr/bin/env bash
#
# Assertions for the out-of-module test read gate (Q895).
#
# Both directions matter and both fail silently. A detector that stops matching
# lets the original bug back in — a unit test asserting against a repo file its
# module cannot see, replaying a cached pass after that file changes. One that
# matches too much fails every test carrying a `..` string, including path
# literals that are fixture data rather than file reads.
#
# The cases run against throwaway fixture repos rather than this tree, because
# the gate enumerates its input with `git ls-files`: a fixture is the only way
# to assert the red direction without breaking the real repo. One case does run
# against this tree, to catch an allowlist that has drifted into a blanket
# exemption. Runs under `make check` (via `make scripts-test`) and CI shellcheck.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "$REPO_ROOT"

GATE="$REPO_ROOT/scripts/go/check-test-cache-inputs.sh"
WORK="$REPO_ROOT/tmp/check-test-cache-inputs-test.$$"
trap 'rm -rf "$WORK"' EXIT INT TERM
mkdir -p "$WORK"

fails=0

# fixture NAME — a one-module repo with an outside file the module cannot see.
# Prints its path; the caller writes pkg/x_test.go into it.
fixture() {
	local dir="$WORK/$1"
	mkdir -p "$dir/mod/pkg"
	printf 'outside\n' >"$dir/outside.txt"
	printf 'module example.com/m\n\ngo 1.26\n' >"$dir/mod/go.mod"
	(
		cd "$dir"
		git init -q .
		git config user.email t@example.com
		git config user.name t
	)
	printf '%s' "$dir"
}

# assert_gate NAME WANT_RC DESCRIPTION — run the gate in a fixture, check status.
assert_gate() {
	local dir="$1" want="$2" desc="$3" rc=0 out
	(cd "$dir" && git add -A)
	out="$(cd "$dir" && "$GATE" 2>&1)" || rc=$?
	die_if_killed "$desc" "$rc" "$want"
	if [[ "$rc" != "$want" ]]; then
		echo "FAIL: $desc — want exit $want, got $rc" >&2
		printf '%s\n' "$out" >&2
		fails=$((fails + 1))
	fi
}

# --- red: a plain "../" literal escaping the module root ---------------------
d="$(fixture escapes)"
cat >"$d/mod/pkg/x_test.go" <<'GO'
package pkg

import "os"

func read() { _, _ = os.ReadFile("../../outside.txt") }
GO
assert_gate "$d" 1 "a ../ read escaping the module root is caught"

# --- red: the filepath.Join spelling, whose ".." is a separate argument -------
d="$(fixture join)"
cat >"$d/mod/pkg/x_test.go" <<'GO'
package pkg

import (
	"os"
	"path/filepath"
)

func read() { _, _ = os.ReadFile(filepath.Join("..", "..", "outside.txt")) }
GO
assert_gate "$d" 1 "the filepath.Join spelling is caught"

# --- green: the same file reached through an in-module testdata/ symlink ------
d="$(fixture symlinked)"
mkdir -p "$d/mod/pkg/testdata"
ln -s ../../../outside.txt "$d/mod/pkg/testdata/outside.txt"
cat >"$d/mod/pkg/x_test.go" <<'GO'
package pkg

import "os"

func read() { _, _ = os.ReadFile("testdata/outside.txt") }
GO
assert_gate "$d" 0 "reading through an in-module symlink passes"

# --- green: a path that stays inside the module root -------------------------
d="$(fixture inside)"
mkdir -p "$d/mod/sibling"
printf 'x\n' >"$d/mod/sibling/f.txt"
cat >"$d/mod/pkg/x_test.go" <<'GO'
package pkg

import "os"

func read() { _, _ = os.ReadFile("../sibling/f.txt") }
GO
assert_gate "$d" 0 "a ../ read staying inside the module root passes"

# --- red: the integration tier is in scope too (Q902) ------------------------
# `go test` caches this tier, and a bare `scripts/go/go-test-integration.sh` run
# deliberately replays from that cache, so an escaping read replays stale there
# exactly as it does in the unit tier.
d="$(fixture integration)"
cat >"$d/mod/pkg/x_test.go" <<'GO'
//go:build integration

package pkg

import "os"

func read() { _, _ = os.ReadFile("../../outside.txt") }
GO
assert_gate "$d" 1 "an //go:build integration file is checked"

# --- green: the e2e tier consults no test-result cache (Q902) ----------------
# `ginkgo run` compiles with `go test -c` and execs the binary itself, so there
# is no cached result for an escaping read to replay.
d="$(fixture e2e)"
cat >"$d/mod/pkg/x_test.go" <<'GO'
//go:build e2e

package pkg

import "os"

func read() { _, _ = os.ReadFile("../../outside.txt") }
GO
assert_gate "$d" 0 "an //go:build e2e file is skipped"

# --- red: a root derived at runtime, which carries no ".." to sweep for -------
# os.Getwd()/runtime.Caller(0) walked up to a marker file escapes the module
# root with nothing lexical to match, and the reads it drives are dropped from
# the cache key exactly as a literal's are (Q936).
d="$(fixture getwd)"
cat >"$d/mod/pkg/x_test.go" <<'GO'
package pkg

import (
	"os"
	"path/filepath"
)

func read() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
}
GO
assert_gate "$d" 1 "a root derived from os.Getwd is caught"

d="$(fixture caller)"
cat >"$d/mod/pkg/x_test.go" <<'GO'
package pkg

import (
	"path/filepath"
	"runtime"
)

func root() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}
GO
assert_gate "$d" 1 "a root derived from runtime.Caller is caught"

# --- green: runtime.Caller with no path construction --------------------------
# Reporting a caller's line number is the ordinary test-helper idiom and reads
# no file, so the detector requires the path-building half as well.
d="$(fixture callerline)"
cat >"$d/mod/pkg/x_test.go" <<'GO'
package pkg

import (
	"fmt"
	"runtime"
)

func fail(msg string) string {
	_, file, line, _ := runtime.Caller(1)
	return fmt.Sprintf("%s:%d: %s", file, line, msg)
}
GO
assert_gate "$d" 0 "runtime.Caller without path construction passes"

# --- green: the e2e tier is skipped for derivations too -----------------------
d="$(fixture e2ederiv)"
cat >"$d/mod/pkg/x_test.go" <<'GO'
//go:build e2e

package pkg

import (
	"path/filepath"
	"runtime"
)

func root() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}
GO
assert_gate "$d" 0 "an //go:build e2e derivation is skipped"

# --- this tree stays green, and its allowlist stays specific ------------------
rc=0
"$GATE" >/dev/null 2>&1 || rc=$?
die_if_killed "the gate passes against this repo" "$rc"
if [[ "$rc" != 0 ]]; then
	echo "FAIL: the gate does not pass against this repo (exit $rc)" >&2
	fails=$((fails + 1))
fi

# An allowlist that grew a wildcard would pass every case above while exempting
# the tree, so the entries are counted rather than trusted.
allowed="$("$GATE" --list | grep -c .)"
if [[ "$allowed" -gt 4 ]]; then
	echo "FAIL: $allowed allowlisted escaping reads — each needs a reason, not a bulk exemption" >&2
	fails=$((fails + 1))
fi

# The same guard for the derivation detector. Its exemptions cost real time
# (-count=1 busts a whole package) or real coverage (DERIV_ALLOW asserts the
# path stays in-module), so a list that grew would pass every case above while
# exempting the tree.
derivations="$("$GATE" --list-derivations | grep -c .)"
if [[ "$derivations" -gt 3 ]]; then
	echo "FAIL: $derivations runtime root derivations — each needs a judgement, not a bulk exemption" >&2
	fails=$((fails + 1))
fi

# go-test.sh reads this list to build its forced -count=1 pass. An empty one
# means that pass covers nothing, which is how the Q936 defect looked.
uncached="$("$GATE" --uncached-packages | grep -c .)"
if [[ "$uncached" -lt 1 || "$uncached" -gt 3 ]]; then
	echo "FAIL: $uncached uncached package(s) — expected 1..3; go-test.sh forces -count=1 over exactly these" >&2
	fails=$((fails + 1))
fi

if ((fails > 0)); then
	echo "$fails assertion(s) failed" >&2
	exit 1
fi
echo "ok: check-test-cache-inputs asserts both directions"
