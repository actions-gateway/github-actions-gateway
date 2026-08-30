#!/usr/bin/env bash
#
# check-queue-claims.sh — entry point for backlog rule 12 (Q889, wired by Q1042).
#
# The checker is the vendored queue.py, whose `claims` asserts that every id
# this branch *adds* to the store holds a `refs/queue-ids/QN` on the remote.
# That is the half `alloc-queue-id.sh` cannot enforce on its own: an id read off
# the store and incremented by hand allocates nothing, and surfaces at the
# rebase that collides rather than at the commit that files the row.
#
# Its sibling lint-queue.sh carries the rules that are a pure function of the
# store, and check-queue-rules.sh the three that are functions of the branch but
# need no network.
#
# This wrapper exists because every gate in this repo is a scripts/ file: the
# Makefile recipe, the workflow step and gate-list.sh's own derivation all key
# on that, and a recipe reading `python3 <file>` satisfies none of them.
#
# `claims` skips rather than fails when the remote cannot be read, so an offline
# clone still runs the gate; `--strict` turns every such skip into a failure and
# belongs in CI, which always has a network. Pass it there, not here — this
# entry point is also the local `make queue-claims-check`.
#
# Usage: check-queue-claims.sh [--strict] [--allow QNNN]
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(git rev-parse --show-toplevel)"

exec python3 "$SCRIPT_DIR/queue.py" --store "$REPO_ROOT/docs/queue" claims "$@"
