#!/usr/bin/env bash
#
# check-cosign-pin.sh — hold the Makefile's COSIGN_VERSION to the cosign
# publish.yml signs with (Q903).
#
# Two pins name the same binary from opposite ends of the release: publish.yml
# installs cosign through sigstore/cosign-installer to SIGN the images and
# charts, and the Makefile downloads it to VERIFY them (`make verify-release`,
# and the dogfood CRD smoke). A bump to either side alone leaves `make
# verify-release` running a verifier the publish job never signed with — a
# silent divergence, since cosign verifies across versions until it doesn't.
#
# The signer's version lives in scripts/ci/pins.env (Q1166), and every
# cosign-installer step must read it as `${{ steps.pins.outputs.COSIGN_VERSION }}`.
# So the gate holds pins.env's COSIGN_VERSION to the Makefile's, and refuses an
# installer step whose `cosign-release` is anything else: a literal version would
# stop moving with the pin, and a missing input floats to latest. The `pins` step
# that reference names is checked by actionlint, which rejects a reference to a
# step id the job does not define.
#
# From cosign v3 it also holds the signer to the v2 format (Q1218): every
# `cosign sign`, `attest` and `sign-blob` in publish.yml must pass
# --new-bundle-format=false --use-signing-config=false, because v3's default
# image signatures are invisible to the cosign v2 verifiers operators run.
#
# Scoped to cosign on purpose. CALICO_VERSION is the repo's other Makefile pin
# duplicated into pins.env, and updatecli.d/calico.yaml rewrites both copies
# together; a general cross-file pin gate is a larger change and would want its
# own backlog item.
#
# Usage:
#   check-cosign-pin.sh [path/to/Makefile] [path/to/publish.yml] [path/to/pins.env]
#
# Exits 1 on drift, and 2 when a pin or signing call the gate compares is
# absent — the gate would otherwise pass by comparing nothing.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

MAKEFILE="${1:-Makefile}"
WORKFLOW="${2:-.github/workflows/publish.yml}"
PINS="${3:-scripts/ci/pins.env}"
# The literal an installer step must carry, unexpanded.
# shellcheck disable=SC2016
WANT_REF='${{ steps.pins.outputs.COSIGN_VERSION }}'

for f in "$MAKEFILE" "$WORKFLOW" "$PINS"; do
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

mapfile -t env_pins < <(awk -F= '$1 == "COSIGN_VERSION" {print $2}' "$PINS")
if ((${#env_pins[@]} != 1)); then
	printf 'cosign-pin: expected exactly one COSIGN_VERSION= line in %s, found %d\n' \
		"$PINS" "${#env_pins[@]}" >&2
	exit 2
fi
if [[ "${env_pins[0]}" != "$want" ]]; then
	printf 'cosign-pin: %s pins COSIGN_VERSION %s, but %s pins COSIGN_VERSION %s\n' \
		"$PINS" "${env_pins[0]}" "$MAKEFILE" "$want" >&2
	printf 'the signer and the local verifier must be the same cosign; bump both (and the digests in scripts/release/download-cosign.sh)\n' >&2
	exit 1
fi

# Every step installing cosign, and every version input, counted independently:
# an installer step with no input floats to latest and must fail the gate rather
# than drop out of the comparison.
installs="$(grep -cE '^[[:space:]]*uses:[[:space:]]*sigstore/cosign-installer@' "$WORKFLOW" || true)"
mapfile -t refs < <(awk '/^[[:space:]]*cosign-release:/ {
	sub(/^[[:space:]]*cosign-release:[[:space:]]*/, ""); sub(/[[:space:]]+$/, ""); print
}' "$WORKFLOW")

if ((installs == 0)); then
	printf 'cosign-pin: %s installs cosign nowhere, so this gate would compare nothing\n' "$WORKFLOW" >&2
	exit 2
fi
if ((${#refs[@]} != installs)); then
	printf 'cosign-pin: %s has %d sigstore/cosign-installer step(s) but %d cosign-release input(s)\n' \
		"$WORKFLOW" "$installs" "${#refs[@]}" >&2
	printf 'an unpinned installer floats to the latest cosign, which is the drift this gate exists to stop\n' >&2
	exit 1
fi

drift=0
for ref in "${refs[@]}"; do
	if [[ "$ref" != "$WANT_REF" ]]; then
		printf 'cosign-pin: %s sets cosign-release: %s, but it must read %s so the signer is the cosign %s pins\n' \
			"$WORKFLOW" "$ref" "$WANT_REF" "$PINS" >&2
		drift=1
	fi
done
if ((drift)); then
	exit 1
fi

# From cosign v3 the signer must also be told to write the v2 format (Q1218):
# v3's default image signatures are invisible to a cosign v2 verifier, and v2
# rejects the two flags as unknown, so they are required from v3 and only there.
# Continuation lines are joined first, so a flag on a wrapped line still counts.
signs=0
if [[ "$want" =~ ^v([0-9]+)\. ]] && ((BASH_REMATCH[1] >= 3)); then
	mapfile -t calls < <(awk '
		/^[[:space:]]*#/ { next }
		{ line = (held == "" ? $0 : held " " $0) }
		/\\[[:space:]]*$/ { sub(/\\[[:space:]]*$/, "", line); held = line; next }
		{ held = ""; if (line ~ /cosign (sign|sign-blob|attest)([[:space:]]|$)/) print line }
	' "$WORKFLOW")
	signs=${#calls[@]}
	if ((signs == 0)); then
		printf 'cosign-pin: %s installs cosign %s but no cosign sign/attest/sign-blob call was found, so the format check compared nothing\n' \
			"$WORKFLOW" "$want" >&2
		exit 2
	fi
	for call in "${calls[@]}"; do
		for flag in --new-bundle-format=false --use-signing-config=false; do
			if [[ "$call" != *"$flag"* ]]; then
				printf 'cosign-pin: %s signs with cosign %s without %s:\n  %s\n' \
					"$WORKFLOW" "$want" "$flag" "${call#"${call%%[![:space:]]*}"}" >&2
				drift=1
			fi
		done
	done
	if ((drift)); then
		printf 'cosign v3 writes a format cosign v2 verifiers cannot read unless both flags are passed\n' >&2
		exit 1
	fi
fi

printf 'cosign-pin: ok (%s in %s and %s; %d cosign-release input(s) and %d v2-format signing call(s) in %s)\n' \
	"$want" "$MAKEFILE" "$PINS" "${#refs[@]}" "$signs" "$WORKFLOW"
