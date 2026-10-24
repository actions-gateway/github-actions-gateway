#!/usr/bin/env bash
#
# load-pins.sh — export scripts/ci/pins.env to the current GitHub Actions job.
#
# Every key becomes a job env var (via $GITHUB_ENV) for `run:` steps, and a step
# output (via $GITHUB_OUTPUT) for expressions. Run it as a step with `id: pins`
# and reference `steps.pins.outputs.<KEY>` in `with:`, cache keys and step
# names: actionlint rejects a reference to a step id the job does not define, so
# a job that forgot to load the pins fails the gate instead of expanding
# `env.<KEY>` to an empty string, which an action such as cosign-installer
# reads as "use your default".
#
# The file reaches $GITHUB_ENV, which can set anything a later step inherits
# (PATH, LD_PRELOAD, BASH_ENV), so each line is held to a key ending in
# _VERSION, _SHA256 or _IMAGE and a value with no whitespace or shell syntax.
# A malformed line, a duplicate key or an empty file fails the job.
#
# Usage: load-pins.sh [path/to/pins.env]
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"

main() {
	local file="${1:-$REPO_ROOT/scripts/ci/pins.env}"
	local env_out="${GITHUB_ENV:?load-pins.sh runs inside a GitHub Actions job (GITHUB_ENV is unset)}"
	local step_out="${GITHUB_OUTPUT:?load-pins.sh runs inside a GitHub Actions job (GITHUB_OUTPUT is unset)}"

	local line key n=0 bad=0
	local -A seen=()
	local -a pins=()
	while IFS= read -r line || [[ -n "$line" ]]; do
		n=$((n + 1))
		[[ -z "$line" || "$line" == \#* ]] && continue
		if [[ ! "$line" =~ ^([A-Z][A-Z0-9_]*_(VERSION|SHA256|IMAGE))=([A-Za-z0-9._:/@+-]+)$ ]]; then
			printf '%s:%d: not a KEY=value pin with a key ending in _VERSION, _SHA256 or _IMAGE: %s\n' \
				"$file" "$n" "$line" >&2
			bad=1
			continue
		fi
		key="${BASH_REMATCH[1]}"
		if [[ -n "${seen[$key]:-}" ]]; then
			printf '%s:%d: %s is already pinned on line %s\n' "$file" "$n" "$key" "${seen[$key]}" >&2
			bad=1
			continue
		fi
		seen[$key]="$n"
		pins+=("$line")
	done < "$file"

	if ((bad)); then
		exit 1
	fi
	if ((${#pins[@]} == 0)); then
		printf '%s pins nothing, so every job reading it would run unpinned\n' "$file" >&2
		exit 1
	fi

	printf '%s\n' "${pins[@]}" >> "$env_out"
	printf '%s\n' "${pins[@]}" >> "$step_out"
	printf 'load-pins: %d pin(s) from %s\n' "${#pins[@]}" "${file#"$REPO_ROOT"/}"
}

main "$@"
