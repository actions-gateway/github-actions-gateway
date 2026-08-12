# Shared node-occupancy probes for the dogfood lifecycle scripts. Source,
# don't execute:
#
#   REPO_ROOT="$(git rev-parse --show-toplevel)"
#   # shellcheck source=scripts/dogfood/lib/nodes.sh
#   source "$REPO_ROOT/scripts/dogfood/lib/nodes.sh"
#
# Why this exists (Q779): "is the cluster back at rest?" is a billing question,
# and the obvious probe answers it wrong at exactly the wrong moment.
# `gcloud container clusters describe --format='value(currentNodeCount)'` prints
# empty both for a cluster at 0 nodes and for a projection that resolved to
# nothing, so at-rest and answered-nothing are one reading; a teardown was
# nearly reported at rest on it through a DNS outage on 2026-08-09. The full
# mechanism is in release.md § Confirming the cluster is actually at rest.
#
# So these count instances instead. Every Compute Engine instance carries a
# name, so an empty list means no instances rather than a key that went missing,
# and the list is deliberately unfiltered: a `--filter` that stops matching (a
# renamed GKE label, a pool made by hand) returns empty exactly like an idle
# project. The read's own exit status is checked explicitly, and a failure reads
# "unknown", never 0, for the same reason lib/workers.sh does.
#
# Callers must have `set -euo pipefail` active and PROJECT set.
# shellcheck shell=bash

# list_dogfood_instances — print one `name zone machineType status` line per
# Compute Engine instance in PROJECT. Returns 1 when the project cannot be read,
# so a caller can tell an empty project from an unreadable one.
#
# stderr is deliberately not swallowed: why the read failed (auth, DNS, a
# project that does not exist) is the operator's next move, and the count these
# feed says only that it did.
list_dogfood_instances() {
	gcloud compute instances list --project="${PROJECT}" \
		--format='value(name,zone.basename(),machineType.basename(),status)' || return 1
}

# count_dogfood_instances — print how many Compute Engine instances are up in
# PROJECT, or "unknown" when it cannot be read. Never 0 for a failed read.
count_dogfood_instances() {
	local out
	out="$(list_dogfood_instances)" || {
		echo "unknown"
		return
	}
	# awk, not `grep -c`, because grep exits 1 on no match and would abort the
	# caller under `set -e`.
	printf '%s\n' "${out}" | awk 'NF {c++} END {print c+0}'
}

# report_dogfood_at_rest — print whether PROJECT is at rest, and return which of
# the three answers it is:
#
#   0  at rest — no instances, nothing billing
#   1  not at rest — instances are up, and the report names them
#   2  unknown — the project could not be read, which is NOT a synonym for 0
#
# Three outcomes rather than two is the whole point: an unreadable probe that
# returns "at rest" is what leaves a cluster billing overnight.
report_dogfood_at_rest() {
	local out count
	if ! out="$(list_dogfood_instances)"; then
		echo "UNKNOWN: ${PROJECT} could not be read. This is not the same as at rest —"
		echo "  re-run once the read above succeeds."
		return 2
	fi
	count="$(printf '%s\n' "${out}" | awk 'NF {c++} END {print c+0}')"
	if ((count == 0)); then
		echo "AT REST: no Compute Engine instances in ${PROJECT} — nothing is billing."
		return 0
	fi
	echo "NOT AT REST: ${count} Compute Engine instance(s) up in ${PROJECT}:"
	printf '%s\n' "${out}" | awk 'NF {print "  " $0}'
	return 1
}
