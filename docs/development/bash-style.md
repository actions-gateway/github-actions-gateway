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

### SC2329 on a `trap`-invoked cleanup function

shellcheck 0.11 reports `SC2329 (info): This function is never invoked` for a function reached only through `trap`, but **only when the script's final statement is an unconditional `exit`**. The same script ending in an ordinary command is clean, and a conditional `exit` inside an `if` does not trigger it either — so most scripts here never see it, and the ones that do look identical to the ones that do not.

It is a false positive: the function *is* invoked, by the `EXIT` trap. Keep the `exit` if the script needs a meaningful status, and disable the check where the function is defined:

```bash
# shellcheck disable=SC2329 # invoked by `trap cleanup EXIT`; shellcheck 0.11 misses
# that whenever the script ends in an explicit `exit`.
cleanup() {
	rm -rf "${work_dir}"
}
trap cleanup EXIT
```

Do not delete the trailing `exit` or restructure the cleanup to silence it — the exit status is the useful thing and the warning is the wrong one. Live example: [`scripts/vap-param-informer-check.sh`](../../scripts/vap-param-informer-check.sh).
