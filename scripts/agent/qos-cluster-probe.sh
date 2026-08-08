#!/usr/bin/env bash
#
# qos-cluster-probe.sh — measure how much of this Mac a throttle prefix actually
# lets a build use.
#
# Context: scripts/agent/local-throttle.sh wraps heavy phases in a low-priority prefix
# and caps parallelism at (physical cores - 2). On Apple Silicon a `-c` QoS clamp
# turns out to confine work to a single CPU cluster at a pinned frequency, so the
# parallelism cap can be sizing against cores the build will never get — this
# probe is what established that, and it is why the macOS prefix demotes I/O and
# CPU separately (`nice -n 10 taskpolicy -d throttle`) instead of clamping QoS.
# Re-run it whenever the prefix is revisited: it measures the real ceiling per
# candidate.
#
# Method: saturate N spin threads under a candidate prefix, sample per-cluster
# HW active residency and frequency with powermetrics, tear the load down, and
# report effective compute (residency x cores x clock, in GHz-cores). Comparing
# candidates against the unthrottled row shows what each prefix costs.
#
# Usage:
#   scripts/agent/qos-cluster-probe.sh sweep                # compare all candidate prefixes
#   scripts/agent/qos-cluster-probe.sh one 'taskpolicy -b'  # measure one arbitrary prefix
#   scripts/agent/qos-cluster-probe.sh one '' 18 20         # unthrottled, 18 threads, 20 samples
#
# powermetrics requires root, so this prompts for sudo once.
set -euo pipefail
shopt -s inherit_errexit

# Raw powermetrics captures are scratch: they belong in the gitignored tmp/ at
# the repo root, never beside the script.
OUT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/tmp"
readonly OUT_DIR

readonly DEFAULT_SAMPLES=12
readonly SETTLE_SECONDS=3

# Candidate prefixes, in reporting order. The first is the unthrottled ceiling
# every other row is measured against; the second is today's production setting.
readonly CANDIDATES=(
	''                              # unthrottled ceiling
	'nice -n 10 taskpolicy -d throttle' # current local-throttle.sh prefix
	'taskpolicy -d throttle'        # ... without the CPU deprioritization
	'taskpolicy -c utility'         # the pre-Q441 clamp, for contrast
	'taskpolicy -c background'      # the lower clamp, for contrast
)

pids=()

# cleanup kills every spinner, including on Ctrl-C or an early exit. Armed by
# main(), not at load time, so sourcing this file for its parsers installs no
# traps over the caller's own.
cleanup() {
	local pid
	for pid in "${pids[@]:-}"; do
		[[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
	done
	wait 2>/dev/null || true
	pids=()
}

# spin_load starts $2 busy loops under the prefix in $1.
spin_load() {
	local prefix_str="$1" threads="$2"
	local i prefix=()
	[[ -n "$prefix_str" ]] && read -r -a prefix <<<"$prefix_str"
	for (( i = 0; i < threads; i++ )); do
		"${prefix[@]}" bash -c 'while :; do :; done' &
		pids+=("$!")
	done
}

# summarize parses a powermetrics capture into per-cluster residency and an
# effective-compute total. Emits one TSV line: cluster:pct,... <tab> ghz_cores
summarize() {
	# Note: macOS ships BWK awk, which has no gawk asorti(). Cluster report order
	# is tracked by first appearance instead, which is stable across samples.
	awk '
		function note(c) { if (!(c in known)) { known[c] = 1; order[++ncl] = c } }
		/-Cluster HW active frequency:/ {
			cur = $1; sub(/-Cluster$/, "", cur); note(cur)
			fsum[cur] += $5; fn[cur]++
			next
		}
		/-Cluster HW active residency:/ {
			cur = $1; sub(/-Cluster$/, "", cur); note(cur)
			pct = $5; sub(/%$/, "", pct)
			rsum[cur] += pct; rn[cur]++
			next
		}
		/^CPU [0-9]+ frequency:/ { if (cur != "") seen[cur "," $2] = 1 }
		END {
			for (k in seen) { split(k, p, ","); cores[p[1]]++ }
			total = 0; out = ""
			for (i = 1; i <= ncl; i++) {
				c = order[i]
				# note() registers a cluster from EITHER line kind, so a capture
				# truncated mid-sample (powermetrics interrupted) can leave one
				# counter at zero. Division by zero is fatal in awk, which would
				# abort the sweep and discard every candidate already measured;
				# report the missing half as 0 instead.
				r = (rn[c] > 0) ? rsum[c] / rn[c] : 0
				f = (fn[c] > 0) ? fsum[c] / fn[c] : 0
				total += (r / 100) * cores[c] * (f / 1000)
				out = out sprintf("%s=%.0f%% ", c, r)
			}
			printf "%s\t%.1f\n", out, total
		}
	' "$1"
}

# pct_of_max GHZ BASE — GHz-cores as a percentage of the unthrottled ceiling.
# A zero or absent base (the first row measured nothing) yields 0 rather than a
# divide-by-zero, so a broken baseline reads as an empty column instead of
# poisoning every comparison below it.
pct_of_max() {
	awk -v g="$1" -v b="$2" 'BEGIN { printf("%.0f", (b > 0) ? (100 * g / b) : 0) }'
}

# measure runs one candidate end to end and prints its summary line.
measure() {
	local pfx="$1" threads="$2" samples="$3" raw
	raw="${OUT_DIR}/qos-probe.$(printf '%s' "${pfx:-none}" | tr -c 'A-Za-z0-9' '-').txt"
	spin_load "$pfx" "$threads"
	sleep "$SETTLE_SECONDS"
	sudo powermetrics -s cpu_power -i 1000 -n "$samples" 2>/dev/null | tee "$raw" >/dev/null
	cleanup
	summarize "$raw"
}

# print_topology reports the machine's cluster layout for the record.
print_topology() {
	local lvl levels
	levels="$(sysctl -n hw.nperflevels)"
	printf '==> topology: %s physical / %s logical cores\n' \
		"$(sysctl -n hw.physicalcpu)" "$(sysctl -n hw.logicalcpu)"
	for (( lvl = 0; lvl < levels; lvl++ )); do
		printf '    perflevel%s: %-12s cores=%s\n' "$lvl" \
			"$(sysctl -n "hw.perflevel${lvl}.name")" \
			"$(sysctl -n "hw.perflevel${lvl}.physicalcpu")"
	done
}

# sweep measures every candidate and prints a comparison table.
sweep() {
	local threads samples line prefix pct ghz base=""
	threads="$(sysctl -n hw.logicalcpu)"
	samples="$DEFAULT_SAMPLES"

	print_topology
	printf '==> sweeping %s candidates, %s threads each, %s s per sample set\n\n' \
		"${#CANDIDATES[@]}" "$threads" "$samples"
	printf '%-36s %-34s %10s %8s\n' 'PREFIX' 'PER-CLUSTER ACTIVE RESIDENCY' 'GHz-CORES' '% OF MAX'
	printf '%-36s %-34s %10s %8s\n' \
		'------------------------------------' \
		'----------------------------------' '----------' '--------'

	for prefix in "${CANDIDATES[@]}"; do
		line="$(measure "$prefix" "$threads" "$samples")"
		pct="$(printf '%s' "$line" | cut -f1)"
		ghz="$(printf '%s' "$line" | cut -f2)"
		[[ -z "$base" ]] && base="$ghz"
		printf '%-36s %-34s %10s %7s%%\n' "${prefix:-<none>}" "$pct" "$ghz" \
			"$(pct_of_max "$ghz" "$base")"
	done

	printf '\nGHz-cores = sum over clusters of (active residency x cores x clock).\n'
	printf 'It is the compute a build of that shape can actually reach.\n'
}

main() {
	local mode="${1:-sweep}"

	trap cleanup EXIT INT TERM

	if [[ "$(uname -s)" != "Darwin" ]]; then
		printf 'qos-cluster-probe: macOS only\n' >&2
		return 2
	fi

	mkdir -p "$OUT_DIR"

	case "$mode" in
		sweep) sweep ;;
		one)
			print_topology
			measure "${2-}" "${3:-$(sysctl -n hw.logicalcpu)}" "${4:-$DEFAULT_SAMPLES}"
			;;
		*)
			printf 'usage: %s {sweep | one <prefix> [threads] [samples]}\n' "$0" >&2
			return 2
			;;
	esac
}

# Run main only when executed directly, so qos-cluster-probe-test.sh can source
# this file to exercise the pure parsers against a recorded powermetrics capture
# — the measurement path needs root and macOS, the parsers need neither.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
