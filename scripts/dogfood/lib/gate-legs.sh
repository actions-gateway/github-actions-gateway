# The release gate's legs, shared by the gate that runs them and the publish
# check that requires them. Source, don't execute:
#
#   REPO_ROOT="$(git rev-parse --show-toplevel)"
#   # shellcheck source=scripts/dogfood/lib/gate-legs.sh
#   source "$REPO_ROOT/scripts/dogfood/lib/gate-legs.sh"
#
# One list in one place, so a leg added to the gate is a leg every stable tag
# then needs evidence for: scripts/dogfood/validate-release.sh runs these in this
# order, and scripts/release/check-validated-candidate.sh refuses a stable tag
# until a CI gate run has passed each one for the same candidate commit.
# shellcheck shell=bash

# `kata` carries sizing and capacity, because both read the Kata tenant its
# matrix leaves up. Deploy and teardown are not legs: every run needs both.
# shellcheck disable=SC2034 # read by the scripts that source this file
GATE_LEGS_ALL=(kata crd-smoke soak dind dragonfly)
