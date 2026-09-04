#!/usr/bin/env bash
#
# Unit tests for scripts/updatecli/latest-cluster-autoscaler-patch.sh (Q483).
#
# This script decides, unattended and weekly, which cluster-autoscaler the
# live-autoscaler drift gate installs. Two of its properties are load-bearing and
# neither is visible in review:
#
#   * It must never cross the pinned minor. cluster-autoscaler ships per
#     Kubernetes minor, and the harness runs kind's default node image, so a
#     bump into the next minor installs an autoscaler built against a Kubernetes
#     the cluster is not running — the exact skew the gate is meant to surface,
#     manufactured by the bumper itself.
#   * It must never go backwards. A downgrade would quietly retreat the harness
#     onto a cluster-autoscaler whose vocabulary was already vetted, which reads
#     as green while testing less than before.
#
# `curl` is stubbed on PATH to serve a fixture tag list, so the whole resolution
# path — fetch, parse, filter, order — runs here with no network. jq is real
# (a required tool; see scripts/ci/check-tools.sh).
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "$REPO_ROOT"
SCRIPT="$REPO_ROOT/scripts/updatecli/latest-cluster-autoscaler-patch.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/latest-cluster-autoscaler-patch-test.$$"
mkdir -p "$FIXTURE_DIR/bin"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s: %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

# --- stub ------------------------------------------------------------------

# curl: ignores its arguments and prints whatever tag list the case installed at
# $STUB_DIR/tags.json, so a case's fixture is the registry as far as the script
# under test can tell. Exits non-zero when $CURL_FAILS is set, to stand in for an
# unreachable registry.
cat > "$FIXTURE_DIR/bin/curl" << 'STUB'
#!/usr/bin/env bash
set -euo pipefail
if [[ -n "${CURL_FAILS:-}" ]]; then
	echo "stub curl: simulated transport failure" >&2
	exit 22
fi
cat "$STUB_DIR/tags.json"
STUB
chmod +x "$FIXTURE_DIR/bin/curl"

export STUB_DIR="$FIXTURE_DIR"
# The stub has to shadow the real curl for the script under test, which is what
# resolves it — so the override goes on its PATH, not into the script.
export PATH="$FIXTURE_DIR/bin:$PATH"

# --- fixtures --------------------------------------------------------------

# The registry serves an OCI tag list: a top-level "tags" array alongside the
# GCR-flavoured "manifest" map. The map is reproduced here because the script
# must read the array and not blunder into per-manifest tag fields.
#
# Patch 10 sits above patch 9 in 1.36 on purpose: version ordering, not lexical.
# v1.36.3-beta.0 is a pre-release the bumper must not adopt. 1.37 exists so
# "stays in the minor" is a real assertion and not a vacuous one.
install_tags() {
	cat > "$FIXTURE_DIR/tags.json"
}

standard_tags() {
	install_tags << 'JSON'
{
  "name": "autoscaling/cluster-autoscaler",
  "child": [],
  "manifest": {
    "sha256:aaaa": { "tag": ["v1.99.9"] }
  },
  "tags": [
    "v1.35.0",
    "v1.35.2",
    "v1.36.0",
    "v1.36.2",
    "v1.36.3-beta.0",
    "v1.36.9",
    "v1.36.10",
    "v1.37.0"
  ]
}
JSON
}

# --- assertions ------------------------------------------------------------

# expect_resolves NAME PIN WANT — assert the script prints exactly WANT.
expect_resolves() {
	local name="$1" pin="$2" want="$3" got status=0
	got="$("$SCRIPT" "$pin")" || status=$?
	die_if_killed "$name" "$status"
	if ((status != 0)); then
		fail "$name" "exited $status, wanted $want"
	elif [[ "$got" == "$want" ]]; then
		pass "$name"
	else
		fail "$name" "want=[$want] got=[$got]"
	fi
}

# expect_fails NAME PIN — assert the script exits non-zero rather than guessing.
expect_fails() {
	local name="$1" pin="${2-}" status=0
	if [[ $# -ge 2 ]]; then
		"$SCRIPT" "$pin" > /dev/null 2>&1 || status=$?
	else
		"$SCRIPT" > /dev/null 2>&1 || status=$?
	fi
	die_if_killed "$name" "$status"
	if ((status != 0)); then
		pass "$name"
	else
		fail "$name" "exited 0, wanted a non-zero exit"
	fi
}

standard_tags

# The bump this exists to make: a newer patch in the pinned minor is adopted.
expect_resolves 'adopts a newer patch in the pinned minor' v1.36.0 v1.36.10

# Version ordering, not lexical: "v1.36.9" sorts above "v1.36.10" as a string.
expect_resolves 'orders patches numerically (10 > 9)' v1.36.9 v1.36.10

# The invariant. A newer minor is published and must be ignored — moving there is
# the kind bump PR's decision, never this script's.
expect_resolves 'never crosses into a newer minor' v1.35.0 v1.35.2

# Pre-releases are not releases. v1.36.3-beta.0 must not outrank v1.36.2 for a
# pin that has not yet reached the higher patches.
install_tags << 'JSON'
{ "name": "autoscaling/cluster-autoscaler", "tags": ["v1.36.2", "v1.36.3-beta.0"] }
JSON
expect_resolves 'ignores pre-release tags' v1.36.2 v1.36.2

# Already current: emitting the pin verbatim is what makes updatecli see no diff
# and open no PR.
standard_tags
expect_resolves 'emits the pin unchanged when already newest' v1.36.10 v1.36.10

# Never backwards, even when the registry says the pin no longer exists.
install_tags << 'JSON'
{ "name": "autoscaling/cluster-autoscaler", "tags": ["v1.36.0", "v1.36.1"] }
JSON
expect_resolves 'never downgrades below the current pin' v1.36.5 v1.36.5

# A minor with nothing published is not a reason to reach for another minor.
standard_tags
expect_fails 'fails when the pinned minor has no published tag' v1.42.0

# Garbage in must not silently resolve to something plausible.
expect_fails 'rejects a pin without the v prefix' 1.36.0
expect_fails 'rejects a pin missing its patch component' v1.36
expect_fails 'rejects a missing pin argument'

# An unreachable registry must fail loudly, not resolve to the pin: a silent
# "no change" would let the pin rot for as long as the outage lasts, with the
# weekly run reporting success.
CURL_FAILS=1 expect_fails 'fails when the registry is unreachable' v1.36.0

# updatecli writes the source value into the target verbatim, so a trailing
# newline would land inside `CA_VERSION=${CA_VERSION:-...}`.
standard_tags
raw="$("$SCRIPT" v1.36.0; printf 'END')"
if [[ "$raw" == "v1.36.10END" ]]; then
	pass 'prints no trailing newline'
else
	fail 'prints no trailing newline' "want=[v1.36.10END] got=[$raw]"
fi

if ((fails > 0)); then
	printf '\n%d assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nall latest-cluster-autoscaler-patch assertions passed\n'
