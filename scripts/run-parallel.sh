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

set -uo pipefail

pids=()
labels=()

for spec in "$@"; do
    label="${spec%%:*}"
    cmd="${spec#*:}"
    (
        set -o pipefail
        bash -c "$cmd" 2>&1 | sed "s/^/[$label] /"
    ) &
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
