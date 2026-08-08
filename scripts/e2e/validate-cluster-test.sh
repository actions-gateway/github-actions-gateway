#!/usr/bin/env bash
#
# Unit tests for the decision helpers in scripts/e2e/validate-cluster.sh: CNI
# classification and Kubernetes-version parsing/comparison (Q184), plus the
# bounded metrics-server retry (Q397). These are the logic that determines
# pass/warn/fail, so they are asserted here without a live cluster — the
# metrics-server probes are faked, and so is the clock they retry against (Q471),
# so the suite neither sleeps nor depends on machine load. Runs under `make check`
# (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# Shrink the metrics-server retry budgets before sourcing (they are read from the
# environment at source time, so this also asserts that override path): the
# production defaults are minutes, and these assertions must run in seconds.
export VALIDATE_METRICS_TIMEOUT=2 VALIDATE_METRICS_GRACE=1 VALIDATE_METRICS_INTERVAL=1
# Source the script under test for its functions; the BASH_SOURCE guard there
# keeps main() from running on source.
# shellcheck source=scripts/e2e/validate-cluster.sh
source "$REPO_ROOT/scripts/e2e/validate-cluster.sh"

fails=0

# expect_cni DAEMONSET_NAMES EXPECT — feed newline-separated DaemonSet names to
# classify_cni and assert the printed verdict word matches EXPECT (pass/fail/warn).
expect_cni() {
	local names="$1" expect="$2" got verdict
	got="$(classify_cni <<<"$names")"
	verdict="${got%% *}"
	if [[ "$verdict" == "$expect" ]]; then
		printf 'ok   cni  %-5s %s\n' "$expect" "${got#* }"
	else
		printf 'FAIL cni  want=%s got=%s  for: %s\n' "$expect" "$got" "$(tr '\n' ',' <<<"$names")" >&2
		fails=$((fails + 1))
	fi
}

# expect_version VERSION EXPECT — parse VERSION and assert it does/does-not meet
# the 1.30 floor; EXPECT is meet|below|unparseable.
expect_version() {
	local version="$1" expect="$2" parsed maj min got
	if ! parsed="$(parse_k8s_minor "$version")"; then
		got=unparseable
	else
		read -r maj min <<<"$parsed"
		if k8s_meets_min "$maj" "$min" 1 30; then got=meet; else got=below; fi
	fi
	if [[ "$got" == "$expect" ]]; then
		printf 'ok   ver  %-11s %s\n' "$expect" "$version"
	else
		printf 'FAIL ver  want=%s got=%s  %s\n' "$expect" "$got" "$version" >&2
		fails=$((fails + 1))
	fi
}

# CNI: enforcing CNIs pass. kindnet (the dangerous silent-failure case) fails.
expect_cni "calico-node"$'\n'"calico-kube-controllers" pass
expect_cni "cilium"$'\n'"cilium-operator" pass
# GKE Dataplane V2 runs Cilium as `anetd` (with size-suffixed variants).
expect_cni "anetd"$'\n'"anetd-l"$'\n'"kube-proxy" pass
expect_cni "antrea-agent" pass
expect_cni "weave-net" pass
expect_cni "kube-router" pass
expect_cni "canal" pass
expect_cni "kindnet" fail
# Mixed: an enforcing CNI present alongside kindnet still passes (enforcing wins).
expect_cni "kindnet"$'\n'"calico-node" pass
# Case-insensitive matching.
expect_cni "Calico-Node" pass
expect_cni "KindNet" fail
# Unrelated DaemonSets only → cannot determine → warn.
expect_cni "kube-proxy"$'\n'"node-exporter" warn
expect_cni "" warn
# A name that merely contains "kind" but isn't kindnet must not match.
expect_cni "my-kind-of-agent" warn
# Plain `netd` (non-Dataplane-V2 GKE) must NOT match `anetd` — such clusters
# enforce NetworkPolicy via Calico, detected separately; netd alone → warn.
expect_cni "netd"$'\n'"kube-proxy" warn

# Version: 1.30+ meets, below 1.30 fails, junk is unparseable.
expect_version "v1.30.0" meet
expect_version "v1.30.2" meet
expect_version "v1.31.0+rke2r1" meet
expect_version "v1.29.4-gke.1043000" below
expect_version "v1.29.15" below
expect_version "v2.0.0" meet
expect_version "1.30.0" meet
expect_version "v0.99.0" below
expect_version "notaversion" unparseable
expect_version "v1" unparseable

# --- bounded metrics-server retry (Q397) --------------------------------------

# Deterministic clock (Q471). The assertions below bound how long the retry paths
# are allowed to take, and measuring that against the real clock made them flake:
# `make check` runs this suite alongside the Go tests, and a loaded machine
# stretches a 1 s sleep far enough to overshoot the bound — the same run passed
# standalone. So time is faked instead. `now_seconds` shadows the seam in
# validate-cluster.sh, and the `sleep` function shadows the real command,
# advancing the counter by exactly the interval it was asked to wait. Elapsed time
# is then the script's own accounting of the budget it spent: independent of load,
# exact rather than approximate, and the suite no longer sleeps at all.
fake_clock=0

now_seconds() {
	printf '%s\n' "$fake_clock"
}

sleep() {
	fake_clock=$((fake_clock + $1))
}

# Fake probes replacing the two kubectl calls check_metrics_server makes, so the
# retry behaviour is asserted without a cluster. Redefining them after the source
# above shadows the real ones; the counters record how hard the check actually
# tried. fake_available_after is how many probes fail before one succeeds
# (a large value = metrics-server never converges).
fake_available_after=0
fake_available_calls=0
fake_registered=yes

metrics_api_available() {
	fake_available_calls=$((fake_available_calls + 1))
	((fake_available_calls > fake_available_after))
}

metrics_api_registered() {
	[[ "$fake_registered" == yes ]]
}

# expect_retry DESC SUCCEED_AFTER TIMEOUT EXPECT_RC EXPECT_CALLS — assert
# retry_until's contract directly: it always probes once, and stops on the budget.
expect_retry() {
	local desc="$1" succeed_after="$2" timeout="$3" expect_rc="$4" expect_calls="$5" rc=0
	fake_available_after="$succeed_after"
	fake_available_calls=0
	retry_until "$timeout" "$VALIDATE_METRICS_INTERVAL" metrics_api_available || rc=$?
	if ((rc == expect_rc)) && ((fake_available_calls == expect_calls)); then
		printf 'ok   retry     %s (rc=%s, %s probe(s))\n' "$desc" "$rc" "$fake_available_calls"
	else
		printf 'FAIL retry     %s want rc=%s calls=%s got rc=%s calls=%s\n' \
			"$desc" "$expect_rc" "$expect_calls" "$rc" "$fake_available_calls" >&2
		fails=$((fails + 1))
	fi
}

# A zero budget still probes exactly once — it degrades to the one-shot check
# this replaced, never to no check at all.
expect_retry "timeout=0 probes once and gives up" 99 0 1 1
expect_retry "timeout=0 succeeds without sleeping" 0 0 0 1
# Within the budget: keeps probing until the probe succeeds.
expect_retry "succeeds on the second probe" 1 2 0 2

# expect_metrics DESC AVAILABLE_AFTER REGISTERED EXPECT MAX_SECONDS MIN_PROBES —
# run check_metrics_server against the fake probes and assert the verdict it
# tallies (pass|warn — a WARN is what VALIDATE_STRICT turns into a failure), that
# it spent no more than MAX_SECONDS of the fake clock (its own budget, so this
# asserts retry_until never sleeps past the deadline), and that it probed at least
# MIN_PROBES times (so "warn" cannot pass by never retrying at all).
expect_metrics() {
	local desc="$1" available_after="$2" registered="$3" expect="$4" max_seconds="$5" min_probes="$6"
	local start elapsed got
	fake_available_after="$available_after"
	fake_available_calls=0
	fake_registered="$registered"
	n_warn=0
	n_fail=0
	start="$fake_clock"
	check_metrics_server >/dev/null
	elapsed=$((fake_clock - start))
	if ((n_warn > 0)); then got=warn; else got=pass; fi
	if [[ "$got" == "$expect" ]] && ((elapsed <= max_seconds)) && ((fake_available_calls >= min_probes)); then
		printf 'ok   metrics   %-4s %s (%ss, %s probe(s))\n' "$expect" "$desc" "$elapsed" "$fake_available_calls"
	else
		printf 'FAIL metrics   want=%s got=%s elapsed=%ss (max %ss) probes=%s (min %s)  %s\n' \
			"$expect" "$got" "$elapsed" "$max_seconds" "$fake_available_calls" "$min_probes" "$desc" >&2
		fails=$((fails + 1))
	fi
}

# The bounds below are the script's own budgets, exported at the top of this file:
# VALIDATE_METRICS_GRACE=1 for registration, VALIDATE_METRICS_TIMEOUT=2 for
# convergence. Against the fake clock they hold exactly, so each case asserts that
# the path it exercises stayed inside the budget that governs it.

# Already Available: passes on the first probe, no waiting at all.
expect_metrics "Available on the first probe" 0 yes pass 0 1
# The Q397 case: the addon is registered but still converging, and goes Available
# on a retry. Must PASS — VALIDATE_STRICT must not fail a from-zero bootstrap on
# an addon that is merely still coming up.
expect_metrics "becomes Available on retry" 2 yes pass 2 3
# Registered but never Available inside the budget: still WARNs (and so still
# fails under VALIDATE_STRICT), bounded by the timeout rather than hanging.
expect_metrics "never Available within budget" 99 yes warn 2 3
# Nothing registered metrics.k8s.io at all: genuinely absent, so WARN after the
# short registration grace instead of paying the full convergence budget.
expect_metrics "absent — no APIService registered" 99 no warn 1 1

if ((fails > 0)); then
	echo "validate-cluster-test: ${fails} assertion(s) failed" >&2
	exit 1
fi
echo "validate-cluster-test: all assertions passed"
