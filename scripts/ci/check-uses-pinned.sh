#!/usr/bin/env bash
#
# check-uses-pinned.sh — fail when a GitHub Actions `uses:` reference is pinned
# to anything a third party can move (Q644).
#
# actionlint does not close this. Measured against v1.7.12, the version tools/
# pins: `actions/checkout@v7.0.1`, `@v4` and `@main` all exit 0, and it resolves
# the action's `with:` inputs against the tag's metadata while doing so — so it
# reads the ref and only ever asserts that one is present and well formed. A tag
# is mutable by whoever owns the action, and an action runs inside a job holding
# the repository token, so a tag-pinned step is arbitrary third-party code that
# can change under us between one run and the next. Dependabot bumps pins; it
# cannot stop one being written as a tag. Every reference in the tree is a SHA
# today, which is exactly why this needs a gate rather than a convention.
#
# The rules, the reference shapes that are exempt (local `./…` actions and
# reusable workflows, `docker://` images, which need a digest instead), and why a
# trailing `# v7.0.1` comment is required are documented in the package comment
# of devtools/ci/usespin, which does the checking over a real YAML parser. A
# regex cannot do this job here: `uses:` appears inside a comment in
# unit-test.yml and could appear inside any `run:` block, and neither is a
# mapping key.
#
# Scope is every workflow and composite action in the tree, not just the ones
# GitHub runs. That is wider than actionlint, which lints only the 24 files under
# the root .github/workflows/ and never sees cmd/gmc/.github/workflows/ — three
# tracked scaffolding workflows whose `uses:` refs no gate had ever read. A gate
# narrower than the claim it makes is how this repo has been bitten before
# (Q400/Q429/Q571), so the selection below is derived from the tree rather than
# hand-listed, and the checker fails when it finds no references at all.
#
# Usage:
#   check-uses-pinned.sh [file...]     # defaults to every workflow/action file
#
# Backs `make uses-pinned-check` (part of `make check`) and the `uses-pinned` job
# in .github/workflows/unit-test.yml. Assertions: scripts/ci/check-uses-pinned-test.sh.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"
DEVTOOLS_DIR="$SCRIPT_DIR/../../devtools"

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$repo_root"

files=("$@")
if ((${#files[@]} == 0)); then
	# Untracked-but-not-ignored files are included on purpose: a brand-new
	# workflow must be checked by its own first `make check`, not first by the
	# commit that tracks it (the Q432 / Q619 false-green class). Vendored
	# action.yml files belong to third-party modules and are not ours to pin.
	mapfile -t files < <(
		git_candidates \
			':(glob)**/.github/workflows/*.yml' \
			':(glob)**/.github/workflows/*.yaml' \
			':(glob)**/action.yml' \
			':(glob)**/action.yaml' \
			':(exclude)*vendor/*' | select_present_files
	)
fi

if ((${#files[@]} == 0)); then
	die "no workflow or action files found under $repo_root. Either this repo has none,
       or the pathspec above stopped matching — an empty file set would otherwise
       report green having checked nothing."
fi

# Built and exec'd rather than `go run`: the checker's exit status IS the gate's
# verdict, and `go run` prints its own "exit status 1" line on top of the
# findings. devtools/ is outside the Go workspace, hence GOWORK=off — see
# docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/usespin"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./ci/usespin)

step "checking \`uses:\` pins across ${#files[@]} workflow/action file(s)"
"$bin" "${files[@]}"
