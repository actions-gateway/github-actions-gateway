#!/usr/bin/env bash
#
# go-work-tidy.sh - Tidy all Go modules in a workspace sequentially.
#
# Description:
#   Parses the local 'go.work' file to extract active Go modules, resolves 
#   their internal dependency graph, and executes 'go mod tidy' on each 
#   module in strict dependency order.
#
# Usage:
#   ./go-work-tidy.sh           # Completely silent on success
#   ./go-work-tidy.sh -d        # Show debug/progress logs
#   ./go-work-tidy.sh --debug   # Show debug/progress logs

# Strict Mode Setup
set -euo pipefail

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

for cmd in awk grep go realpath sed; do
    if ! command -v "$cmd" &> /dev/null; then
        log_error "Required system command '$cmd' is missing from your PATH."
        exit 1
    fi
done

log_info "Extracting active modules from go.work..."

# 1. Parse go.work for Active Modules (Ignoring Comments and Vendor)
mapfile -t raw_modules < <(awk '
    { sub(/\/\/.*$/, "") } 
    /^[ \t]*$/ { next }    
    
    /^use \(/ { in_use=1; next }
    /^\)/     { in_use=0 }
    in_use    { gsub(/[ \t\r]/, ""); if ($0 != "") print $0 }
    /^use [^\(]/ { print $2 }
' go.work | grep -v "/vendor/")

if [[ ${#raw_modules[@]} -eq 0 ]]; then
    log_info "No valid active modules found in go.work."
    exit 0
fi

log_info "Calculating module dependency order..."

# 2. Determine Structural Ordering Using 'go list'
mapfile -t ordered_modules < <(go list -f '{{if not .Main}}{{.Dir}}{{end}}' -m all 2>/dev/null || true)

# 3. Intersect Data Arrays Safely
final_list=()

for mod_dir in "${ordered_modules[@]}"; do
    [[ -z "$mod_dir" ]] && continue
    rel_mod_dir=$(realpath --relative-to="." "$mod_dir")
    
    for raw_mod in "${raw_modules[@]}"; do
        clean_raw="${raw_mod#./}"
        if [[ "$clean_raw" == "$rel_mod_dir" ]]; then
            final_list+=("$rel_mod_dir")
            break
        fi
    done
done

for raw_mod in "${raw_modules[@]}"; do
    clean_raw="${raw_mod#./}"
    match_found=false
    for final_mod in "${final_list[@]}"; do
        if [[ "$final_mod" == "$clean_raw" ]]; then
            match_found=true
            break
        fi
    done
    if [[ "$match_found" == false ]]; then
        final_list+=("$clean_raw")
    fi
done

# 4. Execute Go Mod Tidy Sequentially
log_info "Tidying workspace modules in dependency order:"

for mod in "${final_list[@]}"; do
    if [[ -d "$mod" ]]; then
        log_info "  -> $mod"
        # Mute stdout of go mod tidy, but allow stderr to pass through if it errors
        (cd "$mod" && go mod tidy > /dev/null)
    else
        log_warn "Directory '$mod' listed in go.work does not exist. Skipping."
    fi
done

log_info "Workspace tidy complete!"
