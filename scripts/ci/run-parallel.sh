#!/usr/bin/env bash
# run-parallel.sh — run commands in parallel with labeled, real-time output.
#
# Usage:
#   scripts/ci/run-parallel.sh "label1:cmd1 [args]" "label2:cmd2 [args]" ...
#
# Each argument is a "label:command" pair. Output lines are prefixed with
# [label] so concurrent output remains attributable.
#
# The summary separates two outcomes, because they call for different responses:
#
#   FAILED — the command ran to a verdict and the verdict was bad. Exit 1.
#   KILLED — a signal ended the command (128+n) before it reached a verdict.
#            Exit is that command's own status, so a caller can tell the two
#            apart without parsing the summary.
#
# A FAILED entry is a defect to go and read. A KILLED one usually is not: this
# runner never kills a child (every pid is waited, siblings are never
# cancelled), so 128+n means the signal came from elsewhere, and under host
# contention that is SIGTERM. Both are still reported and both still exit
# non-zero — a killed command did not do its work, and 137 is the OOM killer's.
#
# Example:
#   scripts/ci/run-parallel.sh \
#     "cert-manager:make apply-cert-manager" \
#     "bake:docker buildx bake --file docker-bake.hcl"

set -euo pipefail
shopt -s inherit_errexit

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
killed=()
kill_rc=0
for i in "${!pids[@]}"; do
    rc=0
    wait "${pids[$i]}" || rc=$?
    (( rc == 0 )) && continue
    # A bare label cannot separate an assertion failing (small rc) from a
    # command the kernel killed (128+n; 137 is the OOM killer's) or one that was
    # never found (127), so the status goes in the summary (Q703).
    #
    # 128 itself is not a signal death — git spends it on any fatal error — so
    # the split is rc > 128, not rc >= 128 (Q837).
    if (( rc > 128 )); then
        killed+=("${labels[$i]} (signal $(( rc - 128 )), exit $rc)")
        (( kill_rc == 0 )) && kill_rc=$rc
    else
        failed+=("${labels[$i]} (exit $rc)")
    fi
done

if (( ${#failed[@]} > 0 )); then
    printf '[run-parallel] FAILED: %s\n' "${failed[@]}" >&2
fi
if (( ${#killed[@]} > 0 )); then
    printf '[run-parallel] KILLED: %s\n' "${killed[@]}" >&2
    printf '[run-parallel] KILLED means a signal ended the command before it reached a verdict. This runner never kills a child, so the signal came from elsewhere: SIGTERM (143) under host contention is not a gate failure, while signal 9 (137) is the OOM killer and is.\n' >&2
fi

# A verdict outranks a kill: exit 1 whenever anything reached a bad one. No
# trailing `exit 0` — shellcheck 0.11.0 reads an unconditional one as proof the
# EXIT trap is unreachable and reports cleanup() as never invoked (SC2329).
if (( ${#failed[@]} > 0 )); then
    exit 1
elif (( ${#killed[@]} > 0 )); then
    exit "$kill_rc"
fi
