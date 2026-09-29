#!/usr/bin/env bash
#
# go-work-tidy.sh - Tidy every Go module in the repo sequentially.
#
# Description:
#   Runs 'go mod tidy' on each go.work member in go.work order, then on every
#   module go.work does NOT list (devtools/, tools/, and whichever comes next)
#   with GOWORK=off, discovered via nonworkspace_modules() rather than named
#   here.
#
#   Deriving the whole list from go.work is what disarmed tidy-check in Q667:
#   the gate diffs '**/go.mod' across the repo, so it covered devtools/ and
#   tools/ already — but nothing in the flow ever rewrote them, and a real
#   defect committed in both passed the gate. A non-workspace module resolves
#   nothing through the workspace, so it tidies against its own module graph.
#
# Usage:
#   ./go-work-tidy.sh           # Completely silent on success
#   ./go-work-tidy.sh -d        # Show debug/progress logs
#   ./go-work-tidy.sh --debug   # Show debug/progress logs

# Strict Mode Setup
set -euo pipefail
shopt -s inherit_errexit

# Initialize Debug State
DEBUG=false

# Parse Command Line Arguments
for arg in "$@"; do
    case $arg in
        -d|--debug)
            DEBUG=true
            shift
            ;;
        *)
            # Ignore unknown arguments silently or add handling if needed
            ;;
    esac
done

# Helper Logging Functions
log_info() {
    if [[ "$DEBUG" == true ]]; then
        echo "[INFO] $1"
    fi
}

log_warn() {
    echo "[WARN] $1" >&2
}

log_error() {
    echo "[ERROR] $1" >&2
}

# Pre-flight Checks & Guard Clauses
if [[ ! -f "go.work" ]]; then
    log_error "go.work file not found. Please run this script from your workspace root."
    exit 1
fi

for cmd in git go jq; do
    if ! command -v "$cmd" &> /dev/null; then
        log_error "Required system command '$cmd' is missing from your PATH."
        exit 1
    fi
done

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

# 1. Tidy the go.work Members
#
# In go.work order; nothing sorts them by dependency.
log_info "Tidying workspace modules:"

while IFS= read -r mod; do
    [[ -z "$mod" ]] && continue
    if [[ -d "$mod" ]]; then
        log_info "  -> $mod"
        # Mute stdout of go mod tidy, but allow stderr to pass through if it errors
        (cd "$mod" && go mod tidy > /dev/null)
    else
        log_warn "Directory '$mod' listed in go.work does not exist. Skipping."
    fi
done < <(workspace_modules)

# 2. Tidy the Modules go.work Does Not List
#
# GOWORK=off so each resolves against its own module graph instead of the
# workspace build list it is not a member of. They carry no replace edges into
# the workspace, so ordering among them does not matter.
log_info "Tidying modules outside go.work:"

while IFS= read -r nonworkspace_mod; do
    [[ -z "$nonworkspace_mod" ]] && continue
    if [[ -d "$nonworkspace_mod" ]]; then
        log_info "  -> $nonworkspace_mod (GOWORK=off)"
        (cd "$nonworkspace_mod" && GOWORK=off go mod tidy > /dev/null)
    else
        log_warn "Module directory '$nonworkspace_mod' does not exist. Skipping."
    fi
done < <(nonworkspace_modules)

log_info "Repo tidy complete!"
