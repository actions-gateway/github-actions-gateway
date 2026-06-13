# Bash style

Conventions for every shell script in this repo — `scripts/`, `.githooks/`, and (where practical) inline `run:` blocks in `.github/workflows/`. Every tracked script under `scripts/` is linted against these by `make shellcheck`, which is part of `make check` (see [testing.md § The shellcheck gate](testing.md#the-shellcheck-gate)).

## Rules

- Every script must start with `set -euo pipefail`.
- Use `local` for all variables inside functions.
- Use `[[ ]]` for conditionals and `(( ))` for arithmetic — never `[ ]`.
- Quote all variable expansions (`"$var"`, `"${arr[@]}"`) unless word-splitting is explicitly intended — annotate that intent with a comment.
- When background processes need cleanup, register a `trap cleanup EXIT INT TERM` function that kills tracked PIDs.
- Prefer `awk -v name="$value" '...'` over `sed` for substitutions involving variables — `sed` delimiter and metacharacter (`/`, `&`, `\`) issues are a common source of bugs.
- When capturing the exit code of a pipeline via `wait`, wrap it in a subshell (`( cmd | other ) &`) so `$!` is the subshell's PID and `wait` reflects the pipeline result under `pipefail`, not just the last process's exit code.

## Shared helpers and Makefile wiring

Before writing a new helper function, check [`scripts/lib/common.sh`](../../scripts/lib/common.sh) — `require_cmd`, `workspace_modules`, and the throttle setup already live there. The root `Makefile` keeps recipes as thin target→script wiring so the logic stays shellcheck-covered; see [`scripts/README.md`](../../scripts/README.md) for the script inventory and parameter conventions.

## Accepted shellcheck findings

A finding that is accepted rather than fixed carries a targeted `# shellcheck disable=SCxxxx` directive with a justifying comment immediately above the line (example: the dynamic-name `read`/`export` in [`scripts/probe-investigations-cd.sh`](../../scripts/probe-investigations-cd.sh)). Everything else is fixed to match the rules above.
