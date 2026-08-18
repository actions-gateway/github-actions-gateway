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
# Every run ends with per-label wall time, slowest first. A fan-out's total is
# its slowest member, so that block is the only place a gate's cost is legible
# (Q819).
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

# Children report their own wall time here: a subshell cannot assign to the
# parent, and the parent cannot time them itself because `wait` collects in spawn
# order while commands finish in any order.
timings="$(mktemp -d)"

# shellcheck disable=SC2329 # invoked by `trap cleanup EXIT`; shellcheck 0.11 misses
# that whenever the script ends in an explicit `exit`.
cleanup() {
    local pid
    for pid in "${pids[@]+"${pids[@]}"}"; do
        kill "$pid" 2>/dev/null || true
    done
    rm -rf "$timings"
}
trap cleanup EXIT INT TERM

for spec in "$@"; do
    label="${spec%%:*}"
    cmd="${spec#*:}"
    idx="${#pids[@]}"
    # Wrap in a subshell so $! is the subshell's PID and wait correctly reflects
    # the pipeline's exit code (via inherited pipefail) rather than awk's.
    # awk -v passes the label as a literal string, avoiding sed delimiter and
    # metacharacter issues. fflush() ensures lines appear in real time.
    #
    # The status is captured off the pipeline and the subshell exits on it, so
    # nothing between the two can change the verdict — the timing write is
    # advisory, and a full disk or an unwritable TMPDIR costs a duration rather
    # than a gate's red or green. `date +%s` rather than SECONDS: resetting
    # SECONDS per subshell reads to shellcheck as a lost assignment (SC2030), and
    # the suppression would be wider than the line it covers.
    (
        started="$(date +%s)"
        rc=0
        bash -c "$cmd" 2>&1 | awk -v label="[$label]" '{ print label, $0; fflush() }' || rc=$?
        printf '%s\t%s\n' "$(( $(date +%s) - started ))" "$label" > "$timings/$idx" || true
        exit "$rc"
    ) &
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

# Wall time before the failure summary, so a red run still ends on its failures.
# A command killed before it could report leaves no file and prints "?".
{
    for i in "${!labels[@]}"; do
        if [[ -s "$timings/$i" ]]; then
            cat "$timings/$i"
        else
            printf '%s\t%s\n' -1 "${labels[$i]}"
        fi
    done
} | sort -rn | awk -F'\t' '
    BEGIN { print "[run-parallel] wall time, slowest first:" }
    { printf "[run-parallel] %8s  %s\n", ($1 < 0 ? "?" : $1 "s"), $2 }
' || true
printf '[run-parallel] elapsed %ss across %d command(s)\n' "$SECONDS" "${#labels[@]}"

if (( ${#failed[@]} > 0 )); then
    printf '[run-parallel] FAILED: %s\n' "${failed[@]}" >&2
fi
if (( ${#killed[@]} > 0 )); then
    printf '[run-parallel] KILLED: %s\n' "${killed[@]}" >&2
    printf '[run-parallel] KILLED means a signal ended the command before it reached a verdict. This runner never kills a child, so the signal came from elsewhere: SIGTERM (143) under host contention is not a gate failure, while signal 9 (137) is the OOM killer and is.\n' >&2
fi

# A verdict outranks a kill: exit 1 whenever anything reached a bad one.
# kill_rc stays 0 when nothing was killed, so a clean run exits 0 here.
(( ${#failed[@]} > 0 )) && exit 1
exit "$kill_rc"
