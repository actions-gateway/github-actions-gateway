#!/usr/bin/env bash
#
# Unit tests for the pure decision helpers in scripts/validate-cluster.sh
# (Q184): CNI classification and Kubernetes-version parsing/comparison. These
# are the logic that determines pass/warn/fail, so they are asserted here
# without a live cluster. Runs under `make check` (via `make scripts-test`) and
# the CI shellcheck job.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# Source the script under test for its functions; the BASH_SOURCE guard there
# keeps main() from running on source.
# shellcheck source=scripts/validate-cluster.sh
source "$REPO_ROOT/scripts/validate-cluster.sh"

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

if ((fails > 0)); then
	echo "validate-cluster-test: ${fails} assertion(s) failed" >&2
	exit 1
fi
echo "validate-cluster-test: all assertions passed"
