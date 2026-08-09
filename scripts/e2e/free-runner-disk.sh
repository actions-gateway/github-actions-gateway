#!/usr/bin/env bash
#
# Drop the preinstalled toolchains the e2e job never touches (~15-20 GB) from a
# GitHub-hosted runner.
#
# The e2e job runs the hosted image close to its free-disk ceiling: the kind node
# image, a 3-node cluster with Calico/cert-manager/metrics-server images
# preloaded onto every node, six bake-built images, and the restored cache tars
# all land on the same root filesystem — and the background bake peaks exactly
# while images are being kind-loaded. One cold-ish buildx cache was enough to tip
# a main run into ENOSPC mid `kind load` with 59 MB free (Q292).
#
# Nothing before the image build needs the headroom, so e2e-reusable.yml starts
# this in the background at the top of the job and blocks on the sentinel just
# before the bake, overlapping the deletions with toolchain setup and cache
# restores instead of paying 17-61 s of serial critical path for them. Run
# directly (no DONE_SENTINEL) it is an ordinary synchronous cleanup, which is
# also how the workflow's fallback path invokes it.
#
# Env:
#   DONE_SENTINEL  Path to touch once every deletion has finished. The waiter
#                  polls for it; leave unset for a synchronous run.
set -euo pipefail
shopt -s inherit_errexit

# Only these paths, and only ones this job provably never reads. `rm -rf` is
# silent on an absent path, so a runner image that drops one of them is a no-op
# rather than a failure.
readonly PURGE_PATHS=(
	/usr/local/lib/android
	/usr/share/dotnet
	/opt/hostedtoolcache/CodeQL
	/usr/share/swift
	/opt/ghc
	/usr/local/.ghcup
	/usr/local/share/powershell
)

main() {
	local path
	echo "==> disk before cleanup"
	df -h /

	# Deletions run concurrently — the Android SDK alone is tens of thousands of
	# files and dominates a serial rm.
	for path in "${PURGE_PATHS[@]}"; do
		sudo rm -rf "${path}" &
	done
	wait

	echo "==> disk after cleanup"
	df -h /

	# Written last, and only on the success path: `set -e` aborts before this if
	# any deletion failed, so the waiter times out and falls back rather than
	# proceeding on a half-cleaned runner.
	if [[ -n "${DONE_SENTINEL:-}" ]]; then
		touch "${DONE_SENTINEL}"
		echo "==> wrote completion sentinel ${DONE_SENTINEL}"
	fi
}

main "$@"
