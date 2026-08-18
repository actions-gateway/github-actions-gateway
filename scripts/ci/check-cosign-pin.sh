#!/usr/bin/env bash
#
# check-cosign-pin.sh — hold the Makefile's COSIGN_VERSION to the
# `cosign-release` publish.yml signs with (Q903).
#
# Two hand-kept pins name the same binary from opposite ends of the release:
# publish.yml installs cosign through sigstore/cosign-installer to SIGN the
# images and charts, and the Makefile downloads it to VERIFY them
# (`make verify-release`, and the dogfood CRD smoke). Only a comment beside each
# one asked them to stay in step, so a bump to either side left `make
# verify-release` running a verifier the publish job never signed with — a
# silent divergence, since cosign verifies across versions until it doesn't.
#
# Also asserts every cosign-installer step carries a pin at all: the action
# floats to latest with no `cosign-release` input, so a deleted pin is the same
# drift arriving by omission, and comparing only the pins that remain would pass
# it.
#
# Scoped to cosign on purpose. CALICO_VERSION is the repo's other Makefile pin
# duplicated into a workflow, and updatecli.d/calico.yaml rewrites both copies
# together; a general cross-file pin gate is a larger change and would want its
# own backlog item.
#
# Usage:
#   check-cosign-pin.sh [path/to/Makefile] [path/to/publish.yml]
#
# Exits 1 on drift, and 2 when a pin the gate compares is absent — the gate
# would otherwise pass by comparing nothing.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

MAKEFILE="${1:-Makefile}"
WORKFLOW="${2:-.github/workflows/publish.yml}"

for f in "$MAKEFILE" "$WORKFLOW"; do
	if [[ ! -f "$f" ]]; then
		printf 'cosign-pin: %s does not exist, so this gate would compare nothing\n' "$f" >&2
		exit 2
	fi
done

# The same derivation download-cosign-test.sh uses, so the two cannot disagree
# about which version the Makefile pins.
mapfile -t makefile_pins < <(awk '/^COSIGN_VERSION[[:space:]]*\?=/ {print $3}' "$MAKEFILE")
if ((${#makefile_pins[@]} != 1)); then
	printf 'cosign-pin: expected exactly one COSIGN_VERSION ?= line in %s, found %d\n' \
		"$MAKEFILE" "${#makefile_pins[@]}" >&2
	exit 2
fi
want="${makefile_pins[0]}"

# Every step installing cosign, and every version pin, counted independently:
# an installer step with no pin floats to latest and must fail the gate rather
# than drop out of the comparison.
installs="$(grep -cE '^[[:space:]]*uses:[[:space:]]*sigstore/cosign-installer@' "$WORKFLOW" || true)"
mapfile -t pins < <(awk '/^[[:space:]]*cosign-release:[[:space:]]*/ {print $2}' "$WORKFLOW")

if ((installs == 0)); then
	printf 'cosign-pin: %s installs cosign nowhere, so this gate would compare nothing\n' "$WORKFLOW" >&2
	exit 2
fi
if ((${#pins[@]} != installs)); then
	printf 'cosign-pin: %s has %d sigstore/cosign-installer step(s) but %d cosign-release pin(s)\n' \
		"$WORKFLOW" "$installs" "${#pins[@]}" >&2
	printf 'an unpinned installer floats to the latest cosign, which is the drift this gate exists to stop\n' >&2
	exit 1
fi

drift=0
for pin in "${pins[@]}"; do
	if [[ "$pin" != "$want" ]]; then
		printf 'cosign-pin: %s pins cosign-release %s, but %s pins COSIGN_VERSION %s\n' \
			"$WORKFLOW" "$pin" "$MAKEFILE" "$want" >&2
		drift=1
	fi
done
if ((drift)); then
	printf 'the signer and the local verifier must be the same cosign; bump both (and the digests in scripts/release/download-cosign.sh)\n' >&2
	exit 1
fi

printf 'cosign-pin: ok (%s, %d cosign-release pin(s) in %s)\n' "$want" "${#pins[@]}" "$WORKFLOW"
