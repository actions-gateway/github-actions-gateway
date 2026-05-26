#!/usr/bin/env bash
# run-parallel.sh — run commands in parallel with labeled, real-time output.
#
# Usage:
#   scripts/run-parallel.sh "label1:cmd1 [args]" "label2:cmd2 [args]" ...
#
# Each argument is a "label:command" pair. Output lines are prefixed with
# [label] so concurrent output remains attributable. Exits non-zero and
# reports which commands failed if any do.
#
# Example:
#   scripts/run-parallel.sh \
#     "cert-manager:make apply-cert-manager" \
#     "bake:docker buildx bake --file docker-bake.hcl"

set -euo pipefail

if (( $# == 0 )); then
    printf 'usage: %s "label1:cmd1" "label2:cmd2" ...\n' "${0##*/}" >&2
    exit 1
fi

pids=()
labels=()

cleanup() {
    local pid
    for pid in "${pids[@]+"${pids[@]}"}"; do
        kill "$pid" 2>/dev/null || true
    done
}
trap cleanup EXIT INT TERM

for spec in "$@"; do
    label="${spec%%:*}"
    cmd="${spec#*:}"
    # Wrap in a subshell so $! is the subshell's PID and wait correctly reflects
    # the pipeline's exit code (via inherited pipefail) rather than awk's.
    # awk -v passes the label as a literal string, avoiding sed delimiter and
    # metacharacter issues. fflush() ensures lines appear in real time.
    ( bash -c "$cmd" 2>&1 | awk -v label="[$label]" '{ print label, $0; fflush() }' ) &
    pids+=($!)
    labels+=("$label")
done

failed=()
for i in "${!pids[@]}"; do
    if ! wait "${pids[$i]}"; then
        failed+=("${labels[$i]}")
    fi
done

if (( ${#failed[@]} > 0 )); then
    printf '[run-parallel] FAILED: %s\n' "${failed[@]}" >&2
    exit 1
fi
