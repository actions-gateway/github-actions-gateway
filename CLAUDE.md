# github-actions-gateway

A four-tier system for running GitHub Actions self-hosted runners in a shared Kubernetes cluster with zero idle compute.
The Gateway Manager Controller (GMC) provisions isolated per-tenant gateway instances from a single `ActionsGateway` CR.
Each instance is an Actions Gateway Controller (AGC) that multiplexes thousands of virtual runner sessions as goroutines — provisioning ephemeral worker pods only when a job is acquired and releasing them immediately on completion.
Per-tenant egress proxy pools give each tenant isolated egress IPs for GitHub traffic.
See `DESIGN.md` and `docs/design/` for full design context.

## Model selection

Use the `model-advisor` skill to assess the right model and thinking level at session start and whenever the task type shifts significantly.

## Development philosophy

Build the right thing AND build it well.
Before writing any code, state the goal in one sentence and the approach in two or three.
If the goal is unclear, ask one focused question rather than guessing. **Verify any technical claim before putting it to the user — a recommended approach is itself a claim** — they decide on it and cannot check it (Q561 offered an option describing `predicate-quantifier: every` with inverted semantics; the approved approach survived only by luck.
Q1085 recommended a CSS fix whose facts were all true but which the unmeasured layout could not accommodate at any viewport width — approved, built, then reworked).

Make the smallest change that achieves the goal.
If you notice problems outside the current task's scope, flag them rather than fixing them:
- New near-term or long-term work → add to the Queue in `docs/STATUS.md` in priority order.
- Long-horizon non-commitments → `docs/design/appendix-g-future-enhancements.md`.

The full fix/flag/defer/decline policy, the classification taxonomy, and what we do and don't measure are in `docs/development/technical-debt.md`.

Capture knowledge durably, don't leave it in chat.
When the user states a standing preference or decision, persist it in the repo (CLAUDE.md, the relevant `docs/` file, or memory) rather than applying it once and moving on.
When follow-up work surfaces mid-task, record it on the Queue — including the *why* of any decision it depends on — instead of only mentioning it in the response.
The Queue row caps at 250 chars: when the *why* doesn't fit, write the doc and link it.
Never drop context to make a row fit.

Before introducing a new pattern or abstraction, check whether the codebase already solves the problem, or already answers the question wrongly.
A wrong existing answer outranks a missing one, so correcting it is part of the change, not a follow-up: #1313 was scoped as "nothing reports the semver floor", and `release-delta.sh` was already reporting it from a `feat` count that read `minor` for a window of pure tooling.

**Never install host binary dependencies yourself, and never work around a missing one** (no hardcoded absolute tool paths, no inline `PATH` mutation).
If a task needs a CLI tool that `scripts/ci/check-tools.sh` doesn't list, stop and surface it to the user; once approved, add it to that script's registry (and to `CONTRIBUTING.md` prerequisites if it's `required`-tier) so every contributor knows to install it and `make doctor` validates it.
Exception: a Go build-/codegen-time tool may be added to the vendored `tools/` module (`tools/tools.go`, built by `make tools`) as-needed when that's genuinely its home — but not as a substitute for a real host dependency.

## Workflow

1. **Before making changes** — review `DESIGN.md` and any relevant docs in `docs/` to confirm the plan matches the design intent.
   If picking the next task: run `gh pr list` first and skip any Queue item already covered by an open PR; verify 🚫 blockers **and the defect the row asserts** are still real (grep for the blocker's deliverables, and for the claimed defect — a previous session may have closed either without flipping the row).
   The open PR is the in-flight signal — there is no "started" marker.
   - **Work on a `claude/`-prefixed branch, never on `main`.** In a worktree session, do all work via the worktree path — never edit files through the parent repo's path.
   - **Check the worktree is fresh:** `git fetch origin main && git log --oneline HEAD..origin/main | head` — any output means rebase onto `origin/main` before other work.
     Stale worktrees cause spurious conflicts, redundant reimplementation of merged work, and outdated reads of the Queue.
   - **One worktree per session, releases included.** Sessions share this clone: on 2026-08-14 two were committing in one working tree, `HEAD` sat on a foreign commit when a branch was cut from it, and a `git switch` failed on another session's live `index.lock`.
     Freshness does not cover it — `HEAD` moves between your own commands, and the index is shared.
     Releasing from a worktree is fine, measured on branch-guard 1.5.0: creating a tag is ungated (only `tag -d` asks), and the tag-*push* ask fires under `strict` for any non-branch ref wherever you are, primary checkout included.
     The one real constraint is git's, that a worktree cannot check out `main` while the primary holds it — so tag the ref (`git tag -a vX.Y.Z origin/main`) rather than switching to it.
     Never delete another session's `index.lock`; it is usually a live pre-commit gate.
   - **Measure before asserting a root cause.** A symptom match to a prior issue is a hypothesis, not a diagnosis — and so are the mechanism and the cost a Queue row asserts, however confidently they read (Q550's defect was real but its stated cause was already fixed, so coding to the row would have repaired a working path; Q658's stated `gridPos` cascade was avoidable and its approach was not the row's).
     Take a direct measurement from the failing system before acting on either — and **filing** a row makes the same claim, so state the measurement or say the mechanism is unverified (Q584 shipped a wrong one and cost a correcting PR; maintaining-backlog.md § A row's asserted defect is a claim).
     Likewise treat ✅ investigation findings in plan docs as unverified until confirmed end-to-end — actually exec the thing rather than trusting source inspection (PR #59).
     But check for a committed capture (`testdata/`) before booking a Tier C/dogfood run — Q495's answer was already in the repo — and treat teaching a fake a new field to make a test pass as a finding about the real interface, not a chore (that is how Q495 shipped).
     Detail: testing.md § Diagnosing failures.
   - **"I can't do that" is a claim too.** Before telling the user a task needs something you lack — credentials, a tool, access, a live cluster — grep for it: this repo documents the GitHub App key's keychain retrieval ([github-app-credentials.md](docs/development/github-app-credentials.md)) and every credential-gated probe scenario (testing.md § Credential-gated probe scenarios).
     Q583 offered the user an option premised on not having App credentials that were a documented `security find-generic-password` away, and the correction reshaped the session.
   - **The status you report is a claim too.** "Green", "stuck", "converging" need a measurement, not an inference: an exit code read through a pipe is the pipe's (`make check | tail` reports `tail`'s), a backgrounded `; echo "EXIT=$?"` chain reports the `echo`'s 0 to the task notification (end it `rc=$?; echo "EXIT=$rc"; exit $rc`), a tool that exits 0 silently may have checked nothing, a state seen once is not a steady state, a count grouped by symptom is not a measurement of cause (exercise the system that would produce the effect; don't correlate records), a count or superlative you cite is re-derived as you write it, never recalled from an earlier scan, a gate that fails on your branch is not yours until it fails on the base too (Q741 cost two wrong hypotheses), and a check sequenced before a state-changing command with `;` does not gate it. **An explanation you offer the user is a claim too, and this repo has usually written the answer down already** — grep the Queue, the shipping release's plan doc, and the runbook before explaining *why* something behaved as it did (1.5 cycle: four unchecked explanations, three caught by the user asking; one called Q630's designed behaviour a defect, one contradicted an open row).
     Detail: testing.md § [The status you report is a claim too](docs/development/testing.md#the-status-you-report-is-a-claim-too).
2. **For complex tasks** — write an explicit plan to `docs/plan/` and follow it.
   Keep it updated as the session progresses so completed scope is verifiable at the end; revise it when new information changes the approach.
   Status/Findings sections record only what has actually happened — never draft conclusion-shaped content (root causes, "fix shipped", metrics) ahead of the evidence, even as a placeholder.
3. **After making changes** — review the diff to confirm it matches the design, is well tested, and achieves the intent.
   Update docs proactively per the change-type → docs mapping in `docs/development/doc-update-matrix.md` — do not wait to be asked. **A design-doc-only update is not sufficient: if a change alters what an operator does, configures, or observes (defaults, fields, failure modes, annotations, metrics, admission rejections), the operator-facing `docs/operations/` docs must be updated too.** Then update `docs/STATUS.md`: remove the completed Queue row — **except a `flake` row, which moves to Deferred § Flake watch instead of being deleted** (`lint-backlog.sh` rule 8); update the Progress table if a plan-level status changed. **Deleting a row reaches outside `STATUS.md`** — dead `#QNNN` anchors, a roadmap bullet whose orphaned continuation line breaks the *previous* bullet's word cap, a plan doc to archive (which re-bases its own outbound links) and its `docs/plan/README.md` row.
   Read `docs/development/maintaining-backlog.md#closing-a-row-what-else-moves` and do all four in the same change; don't defer to an audit.
   - **Run `make docs-gates` the moment prose is written or a doc moves, and `make shellcheck` the moment a `scripts/*.sh` is edited — not after the code gate.** Each is every gate that change can fail, in seconds (`shellcheck` 37 s over all 210 scripts); `make check` runs the same ones ten minutes later at its very end, which is where this lands otherwise (Q699, the session that closed it, Q715, Q844, Q870).
     Read their lines in the output rather than only the exit code. `docs-gates` does not run `shellcheck`, so a shell edit needs its own call ([why it bites](docs/development/testing.md#the-make-check-pre-review-gate)).
4. **Commit when done** — automatically, without asking for permission (see Commits below).
5. **Open a PR when the task is finished** — after committing and pushing, open it with `gh pr create`, automatically and without asking.
   But first, the self-check: **"Is this ready for review?"** — yes to all of:
   - `make check` is green (plus any heavier tier the change warranted — integration/e2e, `make test-race`, `make vulncheck`, `make trivy-scan`).
   - The diff matches the design intent, is well tested, and has no stray debug code, TODOs, or unrelated changes.
   - Every doc per step 3 is updated (design **and** operator-facing), and `docs/STATUS.md` reflects the completed work.
   - The PR is scoped to one concern; unrelated work goes in its own PR, **including a fix for a broken `main`**, however completely it blocks this PR's gate.
     Search for a standalone fix first (`gh pr list`), wait and rebase if one is open, own it in a **draft** PR if none is ([CONTRIBUTING.md](CONTRIBUTING.md#when-main-is-broken)).
   - The description explains *what* changed and *why*, references Queue items by bare ID (`Q44`, never `#44`), and notes how it was tested.
   - **Concurrent work re-checked just now**, not at session start, in both halves: `git fetch origin main && git log --oneline HEAD..origin/main` for what *merged* under you (rebase when it touches your own gate), and `gh pr list` for an open PR whose files or gates overlap yours (duplicated or mutually-invalidating work). **Fetch first**: `git diff HEAD...origin/main` compares against a local ref, so without it a moved base reads as clean.
     The jointly-red case is machine-checked at merge time by the merge queue, so there is no pre-merge freshness check: enqueue and let the queue arbitrate ([CONTRIBUTING.md](CONTRIBUTING.md#re-check-concurrent-work-before-opening)).

   If any answer is no, finish the work first — don't open a PR to "get feedback" on something you know is incomplete.
   If the task is too ambiguous to judge review-readiness, say so and ask.

   **Once CI attaches, confirm the path-gated heavy gates actually RAN — green is not enough.** A PR opened docs-only then given code can show all-green/`CLEAN` while integration/security never tested it; never treat such a PR as ready or merge it.
   Put code in the PR's first push to avoid it; if a gate is missing, `gh pr close <n> && gh pr reopen <n>` to force it. **Exception: both e2e lanes are merge-group-only (Q675), so their heavy jobs never run on a PR and only the `e2e-gate`/`e2e-calico-gate` contexts report.** An absent e2e run on a PR is expected, not a symptom: don't chase it, and don't read the green gate as the suite having run.
   Get an earlier verdict from `make e2e` or a `workflow_dispatch`.
   Verify/fix: [`docs/development/testing.md`](docs/development/testing.md#path-gated-workflows-verify-the-heavy-gates-actually-ran).

6. **After a PR merges, or a substantial task finishes — offer a retrospective** (the `session-retro` skill), then act on whatever the user picks.
   Skip it for trivial work; a clean session's honest retro is two lines.
   The point is landing lessons somewhere durable while the context is still live: this repo's `no-push-to-open-PR` rule and its negative-assertion testing principle both came out of one.

## Code standards

### Go

- Follow Go best practices for code style, naming, comments, and package organization.
- Public types, functions, and packages must have godoc comments.
- **Comments are terse, mechanism-focused, and present tense.** Long-form narrative rationale belongs in `docs/` or the PR description, not in code comments.
  Never narrate the diff ("previously…", "no longer…", commit SHAs) or argue with a hypothetical reviewer in a comment; state a shared rationale once at the declaration and cross-reference it elsewhere.
  A comment asserting how something **upstream** behaves cites the measured version or the gate pinning it ([documentation-standards.md](docs/development/documentation-standards.md#an-upstream-behavior-claim-cites-a-measurement)) — Q270's unmeasured one was the safety argument for dropping a job, and Q551 refuted it.
  CRD field godoc (it becomes the API description) stays complete.
- Tests must be meaningful — verify behavior, not just that the code runs.
- All go modules in the repo must use the same Go version.
- When a function starts something asynchronous, return a `<-chan struct{}` done channel so the caller controls whether and how to wait (block, select with timeout, ignore).
  Do not hide the channel inside a closure or call site.

### Bash

Any new or edited shell script must follow `docs/development/bash-style.md` — `set -euo pipefail`, `local` in functions, `[[ ]]`/`(( ))`, quoted expansions, cleanup `trap`s, `awk` over `sed` for variable substitutions, subshell-wrapped pipelines when capturing exit codes via `wait`.
Scripts under `scripts/` are shellcheck-gated by `make check`. **A new script goes in a `scripts/<group>/` directory named for the gate that runs it — never at the top level** (`scripts/README.md` maps them; a `*-test.sh` sits beside its subject, a cross-gate helper in `fetch/` or `lib/`).

## Hooks: minimizing approval prompts

Six `PreToolUse` hooks guard tool calls; each denial message explains the specific fix.
The habits that avoid most prompts:

- **workspace-guard** prompts when a Bash file read/write command (`grep`/`cat`/`cp`/`rm`/…) resolves a path outside the worktree.
  Prefer the Read/Grep/Glob tools — their literal paths can't hit shell-parse edge cases; they are still guarded (since 1.5.0), so a genuinely outside read (plugin caches, sibling repos) asks either way — that prompt is the boundary working, approve it rather than working around it.
  Keep file args inside the worktree *and* literal — an unexpandable `$VAR`/`$(…)` prompts even when it resolves in-root, so use Glob for a newest-file lookup instead of `f=$(ls -t …)`, and a fixed `tmp/<name>/` scratch dir instead of `mktemp -d tmp/…` (the hook does resolve leading `~`, whitelisted read-only substitutions, and a var assigned a literal earlier in the same command); temp files go in the gitignored `tmp/` at the repo root because they persist and stay citable in a PR, never host-wide `/tmp`, which is denied (this session's own scratchpad is *allowed* read-write since guard Q21/Q29, just ephemeral); don't `cd` outside the worktree; read dependency source from the committed `vendor/`/`tools/vendor/` trees, never `~/go/pkg/mod` (`docs/development/go-workspaces.md`).
  Detail: the `workspace-guard:reduce-workspace-guard-prompts` skill.
- **branch-guard** prompts for git/edit operations on `main`/`master` and for destructive git (`reset --hard`, `clean -f`, `branch -D`, `restore <path>`, `config --global`) — by design.
  Work on a `claude/*` branch; push with `git push -u origin HEAD`; prefer `git pull --ff-only`.
  Chains auto-approve only when every segment is a git/gh command (or a read-only pager/`echo` after one); a non-git segment, a file-writing redirect, or a `$(…)` beyond the recognized read-only substitutions (`$(git rev-parse --show-toplevel)`, `$(pwd)`, …) drops the chain to a prompt.
- **foreground-guard** prompts when a Bash call parks the main thread — watch/follow modes (`gh pr checks --watch`, `gh run watch`, `tail -f`, `kubectl logs -f`), `sleep`-poll loops, `cmd; sleep N; cmd` sandwiches, bare `sleep` ≥ 10 s — and when a slow command registered in `.claude/foreground-guard.json` (`make test-race`/`test-integration`/`e2e*`) runs with an inadequate Bash timeout.
  Fix: one non-blocking snapshot now, or re-run with `run_in_background: true`; for slow tiers, set the timeout the ask names. pr-sentinel already watches PRs — never poll `gh` yourself.
  Detail: the `foreground-guard:reduce-foreground-guard-prompts` skill.
- **go-throttle** prefixes `go build`/`go test` with the local throttle prefix: a bare form is rewritten and auto-allowed; a compound, redirected, or wrapped `-race` form is rewritten and *asked* (no longer blocked); a `-race` it cannot throttle is *denied* with the reason: two go invocations and one prefix to place, or a throttle probe that could not run (Q785; retry, or use the `make` target).
  It counts a `go` in command position or behind an allowlisted wrapper (`timeout … go test -race`, Q696), so a commit message or heredoc body quoting one is left alone.
  Detail: testing.md § Resource auto-throttle.
- **piped-gate** *denies* when a command whose exit status is the answer (`make`, `go build/test`, a `scripts/` gate, `git pull`/`push`/`rebase`/`merge`/`commit`) is piped into a filter, when a command reads `$PIPESTATUS` (bash-only; empty in zsh), or when a **backgrounded** call ends in something that drops the gate's status (an `echo`, a `||` fallback, a trailing `&`). Fix: `cmd > tmp/out.log 2>&1; echo "EXIT=$?"`, then grep the file — plus `rc=$?; echo "EXIT=$rc"; exit $rc` when backgrounded.
  It also denies at two repo-state moments: a `git push` whose moved base overlaps this branch's own files (Q665; rebase and re-run the gate, or a queue kickback finds it), and a `gh pr create` whose files an open PR already changes (Q668; read that PR).
  Both discount the merge-driver-owned files, but the push check only while `git merge-tree` says the merge resolves, so a `docs/STATUS.md`-only branch can still be told it is dirty (Q790).
  Break-glass for a case the rule reads wrong: re-run prefixed `PIPED_GATE_OVERRIDE=<reason>`, and file a Queue row if the rule itself is the defect.
  Both probes fail silent, so offline costs a missed catch and never a block.
  Gate list and settings: `.claude/piped-gate-guard.json`.
- **Read background-task and watcher output with the Read tool, not Bash `cat`/`head`.** Only a Read puts the text in the transcript, and hooks that key on it go silent otherwise: pr-sentinel dampens a repeated event by comparing the last report's head SHA, so a Bash read leaves it unable to tell a repeat from a new failure and the Stop hook asks for a relaunch that re-fires the same event (three redundant cycles in Q844; upstream `karlkfi/claude-pr-sentinel` #9 and #14).
- **no-subagent-workers** fires on `Agent`/`Task` spawns and *asks* (soft) when a spawn looks like a parallel-dispatch worker — workers must be task chips, never sub-agents (`docs/development/parallel-dispatch.md`).
  Read-only agent types (`Explore`, `Plan`) pass untouched.

**Adding a pattern to a hook registry?** These hooks search their patterns against the *whole raw command string*, so a pattern naming a command or path also matches text that merely mentions it — a `git show`/`grep` of the file, a commit message quoting the command.
Anchor it to command position (the two Go hooks are the worked example: they parse instead of scanning, so a name inside a quoted string or a heredoc body is a word and never a command, Q624 and Q708), and assert both directions (`scripts/agent/foreground-guard-patterns-test.sh`, `scripts/agent/claude-go-throttle-hook-test.sh`): a pattern that stops matching fails as silently as one that matches everything.

Each Bash session starts at the worktree root.
Isolate every directory change in a subshell (`(cd cmd/agc && go test ./...)`) so the parent cwd can't drift.

## Testing

[`docs/development/testing.md`](docs/development/testing.md) is the canonical reference: per-module run commands (`go test ./...` from the repo root does **not** work — Go workspace), test-tier selection, the integration/e2e tiers, and the heavier gates.

- **A CLI tool isn't on `PATH`?
  Run `scripts/ci/check-tools.sh` (or `make doctor`) and surface its guidance** — never hardcode an absolute tool path or mutate `PATH` inline for one command.
  Detail: `CONTRIBUTING.md`.
- **Run `make check` before concluding work or requesting review.** The one-command fast gate — everything CI's `unit-test.yml` enforces except its `-race` step (reproduce that with `make test-race` when a change touches the concurrency core).
  Detail: testing.md § The `make check` pre-review gate.
- **Throttle direct `go` invocations.** An unthrottled `go build`/`go test` — `-race` above all — can saturate a dev Mac and crash the GUI (it has happened — Q92).
  Prefer `make` targets (they auto-throttle); the go-throttle hook auto-prefixes bare *and* compound/redirected `-race` commands (confirm the ask when it prompts).
  You add the prefix yourself when a `-race` form holds two `go` invocations in one command (it denies with the reason; split them so each is auto-handled, or prefix manually with `$(scripts/agent/local-throttle.sh prefix)`), or when `go` sits behind a wrapper the allowlist does not name (`wrappers` in `devtools/agent/gothrottle`).
  Detail: testing.md § Resource auto-throttle.
- **Pick the tier that can observe the bug class** (testing.md § Picking the right test tier). envtest suites already exist at `internal/controller/integration/` in both `cmd/agc` and `cmd/gmc` — add to them rather than concluding none exists.
  Iterating against a kind cluster: `docs/development/kind-iteration.md`.
- **Pin the target explicitly on every ad-hoc `kubectl`/`gcloud`, and never read a before/after change as proof your own action caused it** — parallel sessions share the ambient context *and* mutate the cluster; prod-guard denies unpinned mutating commands.
  Detail: kind-iteration.md § Target the cluster explicitly, § A state change you observe is not necessarily one you caused.
- **Verify the resolved target before any mutating command** — the GKE dogfood cluster is hard-classified prod; prod-guard denies (`PROD_GUARD_OVERRIDE=<reason>` for an intentional one; legitimate non-prod targets → `.claude/prod-guard.json`).
  Detail: kind-iteration.md § Verify the resolved target.
- Before concluding a test failure is a code bug, check whether the problem is in the test expectations, test setup, or the code itself.
- **A bulk rename/rewrite is verified by reconciliation, not by an empty leftover grep** — an empty result cannot tell "no sites remain" from "my query never matched them" (Q571 silently skipped 62 files that way).
  Assert a known-affected site *did* change, and reconcile before/after occurrence counts — taking the baseline with a query that spans every shape the change touches, or both counts share one blind spot and agree anyway.
  Detail: testing.md § [A bulk mechanical change proves itself by reconciliation](docs/development/testing.md#a-bulk-mechanical-change-proves-itself-by-reconciliation-not-by-an-empty-leftover-query).
- **A threshold on a shared counter counts events, not actors** — one participant looping clears `>= n-1` alone, so it cannot prove N distinct ones did anything (Q601: a 2-session pool met a 5-session dedup threshold).
  Give each actor its own counter, assert on distinct non-zero ones, pin the population so they're concurrent, and require red when the population is too small.
  Detail: testing.md § [An aggregate counter cannot count distinct participants](docs/development/testing.md#an-aggregate-counter-cannot-count-distinct-participants).
- **Settle a "this code caused that" test by deleting the mechanism** — comment out the code it claims to exercise, require red on the assertion that names the behaviour, restore.
  A first-try green proves nothing until then (testing.md § [Verify a causation claim by deleting the mechanism](docs/development/testing.md#verify-a-causation-claim-by-deleting-the-mechanism)).
- **A test's environment assumptions get probed, not inferred** — uid, tooling on `PATH`, whether mode bits bite. `runs-on` is an expression here, so nine jobs can route to the self-hosted dogfood runner; `os.Geteuid()`/`id -u` answer a different question than the one asserted.
  Detail: testing.md § [A test's environment assumptions must be probed](docs/development/testing.md#a-tests-environment-assumptions-must-be-probed-not-inferred).
- **Flake fixes go first.** If a CI test passes on rerun without a code change, file a Queue item and move it to the top of the Queue before continuing other work — flake cost compounds across every future PR (see `docs/development/maintaining-backlog.md#flake-fixes-go-first`).
- **Never foreground-poll CI, logs, or files** — foreground-guard prompts on watch/poll forms; take one non-blocking snapshot or use a background task (testing.md § [Never foreground-poll CI, logs, or files](docs/development/testing.md#never-foreground-poll-ci-logs-or-files)).
- **Slow tiers get an explicit timeout or a background run** — foreground-guard asks when a registered slow command (`.claude/foreground-guard.json`) runs with an inadequate timeout (testing.md § [Slow tiers need an explicit timeout or a background run](docs/development/testing.md#slow-tiers-need-an-explicit-timeout-or-a-background-run)).
  Background it via `scripts/agent/record-launch.sh <cmd> > tmp/<name>.log 2>&1` so it keeps a stop handle a compaction can't drop; `--list` reads them back (testing.md § [The launch record](docs/development/testing.md#the-launch-record)).

## Security principles

**Secure by default, not opt-in.** Defaults must never trade away a security property for convenience or modernity.
If a new option regresses any security property — even partially, even with mitigations — the more secure option stays the default.
The less secure option may be offered as an explicit opt-in with a flag or config, but must be clearly documented as a trade-off.
Do not introduce security regressions as defaults without raising them explicitly and getting sign-off.

Examples of regressions that must not silently become defaults:
- Switching to a key type that loses a layer of encryption (e.g.
  Ed25519 agents can't decrypt RSA-OAEP session keys)
- Removing a validation, admission check, or network policy
- Relaxing a pod security profile

**Keep secrets out of environment variables.** Prefer writing a secret to a file and reading it from there, deleting the file as soon as it is no longer needed (e.g. `mktemp` + `--from-file`), over passing it through an env var.
Env vars leak into process listings, logs, and child processes.

When in doubt, ask before shipping.

## Documentation conventions

Spell out acronyms on first use: write the full term first, then the acronym in parentheses — e.g. "Actions Runner Controller (ARC)".
Subsequent uses may use the acronym alone.

Human-facing docs must never link to `CLAUDE.md` (or its `AGENTS.md` symlink).
This file is the entrypoint for Claude/agents only; humans start at `README.md` and navigate the `docs/` tree.
The dependency direction is one-way: `CLAUDE.md` may link out to `docs/`, but nothing under `docs/`, `README.md`, or `CONTRIBUTING.md` may link back to it.
Canonical reference content humans need (commands, checklists, rules) lives in the `docs/` tree or `CONTRIBUTING.md`; `CLAUDE.md` keeps its own self-contained copy when it needs one.

**Editing `CLAUDE.md` — protect the context budget.** This file is loaded in full into every session, so every line costs context.
Keep it lean: add only load-bearing, must-act-on rules, and put the explanation/how-to in the relevant `docs/` page with a one-line pointer here rather than growing a self-contained copy past a few sentences.
When in doubt, write the detail in `docs/` and link it; prefer tightening an existing line over adding a new one.

## Commits

- Commit after each task is complete and validated — without asking; committing is automatic in this repo.
  Small, focused commits; Conventional Commits standard; never commit broken code or failing tests.
- **`docs/STATUS.md` changes always get their own isolated commit**, separate from code and plan-doc changes — it is high-contention across concurrent branches, and isolation keeps rebase conflicts trivial to resolve.
- **Commit explicit file paths, never a directory** (`git add cmd` and friends), and prefer `git commit -- <paths>` over `git add` plus a bare `git commit`: the pathspec form makes the commit's contents independent of whatever is already staged.
  Reading `git status` first is not enough on its own — `git mv` stages its rename immediately, and in `--short` output that is `R` in the *staged* column, which is easy to read as pending (Q844 shipped a rename into a `docs/STATUS.md`-only commit that way, caught by `status-isolation-check`).
  Test and build targets also regenerate tracked files as prerequisites (`make test-integration` → `config/crd`), so a directory add silently ships someone else's codegen drift; #847 broke CI that way.
- Amending an unpushed commit is fine — fix up the message or staged changes without asking.
  Once pushed (but before a PR exists), prefer a follow-up commit; only amend + force-push (always `--force-with-lease`, never on `main`/`master`) when the user asks for it.
- **Merges go through the merge queue, which only the web UI can enqueue into** — `gh pr merge` routes a queue enqueue through `enablePullRequestAutoMerge`, and this repo sets `allow_auto_merge: false`, so every form of it fails `Auto merge is not allowed for this repository` (measured 2026-08-14 on #1525, gh 2.96.0).
  The queue validates the candidate merge result and a failing entry is kicked back to its PR with the failure attached (the signal pr-sentinel reacts to).
  A push to a queued PR is **rejected** (`GH006`, measured 2026-08-12); wait for the queue to land or evict it rather than dequeuing, which would revoke a human's merge decision.
  Prefer everything in the first push and weigh the CI a push restarts (docs-only gates are seconds; the e2e matrix is a quarter-hour).
  A stale base no longer blocks merging — only a textual conflict (`mergeStateStatus: DIRTY`) does; rebase when the conflict arrives or when `git diff HEAD...origin/main --stat` shows something that affects your gate, since a queue kickback costs a full check cycle a local re-run would have caught.
  For separable work on an open PR, branch off `origin/main` and open a new one — unless it *blocks* the open PR, which must then be rebased onto it ([CONTRIBUTING.md](CONTRIBUTING.md#pushing-to-a-pr-that-is-already-open) covers both, and the `--onto` rebase after a base squash-merges). **Exception — self-healing your own open PR always pushes:** a CI fix or a `git rebase origin/main` conflict heal that a pr-sentinel wake asked for is repairing *this* PR, not adding scope, so push it once the queue releases it (`--force-with-lease` after a rebase; branch-guard's default `strict` policy auto-approves a force-push of the worktree's own branch) and relaunch the watcher. **A heal that follows a queue eviction may also re-enqueue**, but only when `scripts/agent/pr-requeue-eligible.sh --assess <pr>` (before the rebase) and `--confirm <pr>` (after CI) both pass — a prior *human* enqueue, no current queue entry, and conflicts confined to the merge-driver-owned files.
  That restores the maintainer's own decision; a **first** enqueue is never yours to make (Q692).
  Both verdicts are advisory until `gh` can enqueue at all: report the `ELIGIBLE` line and hand the re-enqueue back, rather than reading the failed `gh pr merge` as a defect to work around.
  Verify what landed by content (`git show origin/main:<path>`), never by SHA — a squash orphans your SHAs, and a rebase rewrites them.
- After pushing, check for a PR (`gh pr view`); if none exists and the task is finished, open one per Workflow step 5.
- No AI/Claude attribution anywhere in commits or PRs — no `Co-Authored-By`, no "Generated with" lines.
- If a change doesn't belong in the current PR, open a separate PR for it — parallel PRs beat bundling unrelated concerns.
- Act only on your own branch and PR.
  Never re-run, edit, or push to a PR or branch owned by another session; when CI fails on another session's PR, reproduce the failure locally instead.

## Agent reference docs

When working on specific tasks, read the relevant doc before starting:

| Task | Reference |
|---|---|
| Running tests or `make check`, picking a test tier/scope, editing CI workflows, the heavier gates (`test-race`, `vulncheck`, `trivy-scan`, coverage) | `docs/development/testing.md` |
| Standing up / iterating against a kind cluster | `docs/development/kind-iteration.md` (design context in `docs/design/07-test-plan.md` §7.3) |
| Go workspace / vendoring / worktrees | `docs/development/go-workspaces.md` |
| Writing or editing any shell script | `docs/development/bash-style.md` |
| Updating docs after a change — CRD fields, new behaviour, admission/validation rules, operator-visible changes, security, module dependencies | `docs/development/doc-update-matrix.md` |
| Writing, editing, or restructuring any doc — style for scannability, copy-paste-safe code blocks, conventions, maintenance | `docs/development/documentation-standards.md` |
| Modifying CRD types (`api/`, `cmd/agc/api/`, `cmd/gmc/api/`) — adding/reshaping a field, enum, condition, or default | `docs/development/api-review.md` (design rules + is-it-breaking; read **before** writing the field), then `docs/development/code-generation.md` |
| Adding a label/annotation an operator sets, a hand-set CRD field, **writing or changing a ValidatingAdmissionPolicy** (a `paramKind` must be a CRD, never a core type — Q444/Q492), or **writing/changing any binary's SIGTERM/shutdown path** | `docs/development/kubernetes-conventions.md` |
| Building binaries | `docs/development/building.md` |
| Deciding whether to fix, flag, defer, or decline tech debt | `docs/development/technical-debt.md` |
| Picking the next task, tracking progress, adding new items | `docs/STATUS.md` — run `gh pr list` first and skip any Queue item already covered by an open PR |
| **Spawning, creating, or making any worker/agent session(s)** — including a single one — or dispatching/parallelizing work across sessions, or clearing a batch of backlog items (dispatcher + one session/PR per task). Read this **before** spawning: workers must be full Claude Code sessions (task chips), **never** Agent/Task sub-agents, and carry the Auto-fix + background conflict-watch self-healing contract. | `docs/development/parallel-dispatch.md` |
| Editing `docs/STATUS.md` (any change to the Queue, Deferred, Progress table, or header) | `docs/development/maintaining-backlog.md` (format = the `backlog` skill; lint: `scripts/docs/lint-backlog.sh`) — allocate new IDs with `make queue-id TITLE="…"`, which searches for near-duplicates before it claims — read the candidates it prints (there is no `Next ID` line; the globally-installed skill still says there is, and this repo overrides it); done rows are **deleted**; Notes and Deferred triggers are **present tense, no status history**, **hard 250-char cap**, **>200 chars must link a doc** (a link counts its full source length). If fitting the cap means dropping a decision or a finding, write the doc first — durable rationale → `docs/design/`, in-flight context → `docs/plan/` — whatever the item's `Sz`. |
| Security-relevant changes | `docs/design/05-security.md` + the operator half per `docs/development/doc-update-matrix.md` |
| Cutting a release, or editing the image publish/sign/SBOM pipeline (`publish.yml`) | `docs/operations/release.md` |
| Editing the docs/marketing website — MkDocs config, brand assets, or the progressive-enhancement JS | `docs/development/website.md` |
