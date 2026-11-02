# Retry helper for the scripts/ tree. Source, don't execute:
#
#   REPO_ROOT="$(git rev-parse --show-toplevel)"
#   # shellcheck source=scripts/lib/retry.sh
#   source "$REPO_ROOT/scripts/lib/retry.sh"
#
# Callers need `set -euo pipefail` active, like every script here.
# shellcheck shell=bash

# retry_backoff ATTEMPTS DELAY MAX_DELAY LABEL -- COMMAND [ARG...] — run COMMAND
# up to ATTEMPTS times, sleeping between attempts, and return 0 on the first
# success or the last attempt's status when the budget runs out.
#
# The schedule is exponential, capped and jittered: the delay starts at DELAY,
# doubles after every sleep, and is clamped to MAX_DELAY, so DELAY=5 and
# MAX_DELAY=60 give 5, 10, 20, 40, 60 seconds before jitter. Each sleep adds up
# to half its delay again at random, so callers that failed in the same second
# do not retry in the same second and land every round as one burst on the
# service that is denying them.
#
# Nothing sleeps after the final attempt. LABEL names the command in the stderr
# line printed before each sleep. Any nonzero status is retried; a caller whose
# command has permanent failures (a digest mismatch) keeps them out of COMMAND.
retry_backoff() {
	local attempts="$1" backoff="$2" max_delay="$3" label="$4"
	[[ "${5:-}" == -- ]] || {
		echo "retry_backoff: expected '--' before the command" >&2
		return 2
	}
	shift 5
	# Zero attempts ran nothing, which is a failure and not a success.
	local attempt rc=1 sleep_for
	for ((attempt = 1; attempt <= attempts; attempt++)); do
		rc=0
		"$@" || rc=$?
		if ((rc == 0)); then
			return 0
		fi
		if ((attempt < attempts)); then
			sleep_for="$backoff"
			if ((sleep_for > 0)); then
				sleep_for=$((sleep_for + RANDOM % (sleep_for / 2 + 1)))
			fi
			echo "${label} failed with exit ${rc} (attempt ${attempt}/${attempts}); retrying in ${sleep_for}s" >&2
			sleep "$sleep_for"
			backoff=$((backoff * 2))
			if ((backoff > max_delay)); then
				backoff="$max_delay"
			fi
		fi
	done
	return "$rc"
}
