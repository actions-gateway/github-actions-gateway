#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-agc-names.sh — the AGC Deployment-name gate
# (Q1098).
#
# The gate reads the tree it is pointed at, so each case builds a throwaway repo
# holding just the two inputs it consumes: the GMC builder the v2 suffix is read
# from, and one page under docs/operations/. That keeps the assertions about the
# rules rather than about whichever pages this repo happens to ship today.
#
# The first pair is the injected defect and its control — a v1-named Deployment
# with no version label must go red, and the same command under a `# v1 (legacy)`
# marker must go green. A gate asserted only against compliant documents has
# demonstrated nothing, because a checker whose pattern stopped matching passes
# those too.
#
# Rule 3 (Q1099) carries its own red case and two controls, because its remedy
# differs from rule 1's: the `app=` selector has a version-neutral answer in
# `app.kubernetes.io/name`, so that form must pass unmarked, while the bare label
# a NetworkPolicy selects on still takes the v1 marker.
#
# The remaining cases pin the parts that would otherwise pass by checking
# nothing: an unreadable suffix constant and an empty scope are refusals (exit 2)
# rather than clean runs, a misspelled v2 suffix is caught, the sibling suffixes
# the same builder mints are left alone, and an upstream version string
# (`v1.35.0`) does not pass for a version label.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
GATE="$REPO_ROOT/scripts/docs/check-agc-names.sh"

fails=0
workdirs=()
WORK=""

# shellcheck disable=SC2329 # invoked by `trap cleanup EXIT`; shellcheck 0.11
# misses that whenever the script ends in an explicit `exit`.
cleanup() {
    local d
    for d in "${workdirs[@]}"; do
        rm -rf "$d"
    done
}
trap cleanup EXIT

# new_repo — start a throwaway repo carrying the builder constant, and read the
# page body from stdin into docs/operations/page.md.
new_repo() {
    WORK="$(mktemp -d)"
    workdirs+=("$WORK")
    git -C "$WORK" init -q
    mkdir -p "$WORK/cmd/gmc/internal/controller" "$WORK/docs/operations"
    cat >"$WORK/cmd/gmc/internal/controller/actionsgateway_v2_builder.go" <<'GO'
package controller

const AGCResourceSuffix = "-agc"

const (
	agcWorkerSuffix        = "-worker"
	agcMetricsTLSSuffix    = "-agc-metrics-tls"
	agcMetricsClientSuffix = "-agc-metrics-client"
)
GO
    cat >"$WORK/docs/operations/page.md"
}

# run_gate NAME WANT — run the gate inside the throwaway repo and compare its
# exit status, which is what `make check` and the docs workflow consume.
run_gate() {
    local name="$1" want="$2" got=0
    (cd "$WORK" && "$GATE") >"$WORK/gate.out" 2>&1 || got=$?
    die_if_killed "$name" "$got" "$want"
    if [[ "$got" == "$want" ]]; then
        printf 'ok   %s\n' "$name"
        return
    fi
    printf 'FAIL %s: want exit %s, got %s\n' "$name" "$want" "$got"
    awk '{ print "    " $0 }' "$WORK/gate.out"
    fails=$((fails + 1))
}

# expect_out NAME PATTERN — assert the last run's output matched PATTERN.
expect_out() {
    local name="$1" pattern="$2"
    if grep -q -- "$pattern" "$WORK/gate.out"; then
        printf 'ok   %s\n' "$name"
        return
    fi
    printf 'FAIL %s: no match for %s in:\n' "$name" "$pattern"
    awk '{ print "    " $0 }' "$WORK/gate.out"
    fails=$((fails + 1))
}

# --- the injected defect, and its control ----------------------------------

new_repo <<'MD'
# Page

```sh
kubectl logs -n <namespace> deploy/actions-gateway-controller --tail=50
```
MD
run_gate "an unlabelled v1 Deployment reference fails" 1
expect_out "the finding offers the v2 form" '<gateway>-agc'

new_repo <<'MD'
# Page

```sh
# v2
kubectl logs -n <namespace> deploy/<gateway>-agc --tail=50
# v1 (legacy)
kubectl logs -n <namespace> deploy/actions-gateway-controller --tail=50
```
MD
run_gate "the same command under a v1 marker passes" 0

# Prose carries the label just as a comment does: a page that says it is the v1
# path does not have to repeat the marker on every command.
new_repo <<'MD'
# Page

This walkthrough stays on the v1 API.

```sh
kubectl rollout restart deploy/actions-gateway-controller -n <namespace>
```
MD
run_gate "a v1 label in the preceding prose passes" 0

# The window is short on purpose: a label the reader has scrolled past is not one
# they see beside the command.
new_repo <<'MD'
# Page

This walkthrough stays on the v1 API.

Some other paragraph.

Another paragraph after that.

```sh
kubectl rollout restart deploy/actions-gateway-controller -n <namespace>
```
MD
run_gate "a v1 label beyond the window fails" 1

# --- rule 2: the v2 suffix tracks the constant ------------------------------

new_repo <<'MD'
# Page

```sh
kubectl logs -n <namespace> deploy/<gateway>-agc-controller --tail=50
```
MD
run_gate "a v2 name that is not the builder's suffix fails" 1
expect_out "the finding names the real suffix" 'AGCResourceSuffix is'

new_repo <<'MD'
# Page

The per-gateway metrics bundle is `<gateway>-agc-metrics-{tls,client}`, and the
worker ServiceAccount is `<gateway>-worker`.
MD
run_gate "the builder's sibling suffixes are left alone" 0

# --- rule 3: the app= selector (Q1099) --------------------------------------

new_repo <<'MD'
# Page

```sh
kubectl get pod -n <namespace> -l app=actions-gateway-controller
```
MD
run_gate "an unlabelled v1 app= selector fails" 1
expect_out "the finding offers the version-neutral label" 'app.kubernetes.io/name=actions-gateway-controller'

# The control that keeps rule 3 from degenerating into "the v1 name appears": the
# recommended label spells the same name and is correct under both versions, so it
# must pass with no marker at all.
new_repo <<'MD'
# Page

```sh
kubectl get pod -n <namespace> -l app.kubernetes.io/name=actions-gateway-controller
```
MD
run_gate "the recommended label needs no version marker" 0

# Where a NetworkPolicy selector is the subject the bare label is the only correct
# one, so stating the version is the escape, exactly as for a Deployment name.
new_repo <<'MD'
# Page

The v1 AGC NetworkPolicy selects on the bare label:

```sh
kubectl run dbg --labels='app=actions-gateway-controller'
```
MD
run_gate "a v1-labelled app= selector passes" 0

# A version *string* is not a version label. `kindest/node:v1.35.0` satisfied a
# bare \bv1\b and told the reader nothing, which is why the pattern excludes it.
new_repo <<'MD'
# Page

Reproduced on a fresh kind cluster (`kindest/node:v1.35.0`).

```sh
kubectl get pod -n <namespace> -l app=actions-gateway-controller
```
MD
run_gate "an upstream version string does not count as a version label" 1

# --- refusals: a gate that checked nothing would otherwise read as clean -----

new_repo <<'MD'
# Page

Nothing to see.
MD
rm "$WORK/cmd/gmc/internal/controller/actionsgateway_v2_builder.go"
run_gate "an unreadable suffix constant is a refusal" 2

new_repo <<'MD'
# Page

Nothing to see.
MD
rm -r "$WORK/docs"
run_gate "an empty scope is a refusal" 2

if ((fails > 0)); then
    printf '\n%d test(s) failed\n' "$fails" >&2
    exit 1
fi
printf '\nall tests passed\n'
