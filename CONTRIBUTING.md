# Contributing

## Prerequisites

- Go 1.26+
- **bash 4.4 or newer, ahead of `/bin/bash` on your `PATH`** ([why, and how to get one](#the-bash-floor))
- Docker (for e2e tests and image builds)
- [kind](https://kind.sigs.k8s.io/) (for the local e2e cluster)
- `make`
- [`gh`](https://cli.github.com/), authenticated (`gh auth status`) — opens PRs, and allocates backlog IDs via [`make queue-id TITLE="…"`](docs/development/queue-id-allocation.md)

Verify your toolchain at any time with `scripts/ci/check-tools.sh` (or `make doctor`).
It checks the tools the project needs — grouped into `required` (the fast `make check` loop), `e2e`, and `extended` (heavier gates, dogfood) tiers — and for anything missing prints a per-OS install command or, when a tool is installed but not on your `PATH`, the exact directory to add.
A tool below a version floor the registry declares is reported the same way, with the version it found.
It exits nonzero if a required tool is missing or too old, so it also works as a CI/setup preflight.

### The bash floor

Every script under `scripts/` opens with `shopt -s inherit_errexit`, without which `set -e` does not reach inside a command substitution and a failed builder yields a truncated value and exit 0 ([bash-style.md](docs/development/bash-style.md#set--e-stops-at-a-command-substitution)).
That shopt arrived in **bash 4.4**, and 175 of the 185 scripts under `scripts/` declare it today.

Apple still ships bash 3.2 at `/bin/bash` and has no plan to update it, so on stock macOS `/usr/bin/env bash` finds a shell below the floor and every one of those scripts exits before doing anything, saying only:

```
shopt: inherit_errexit: invalid shell option name
```

Three `PreToolUse` hooks under `scripts/agent/` are the exception, and they fail the other way: they swallow the shopt's own failure so a hook can never block a tool call, which means on bash 3.2 they keep running with the protection silently switched off.

```bash
brew install bash
```

Homebrew installs to `/opt/homebrew/bin` (Apple silicon) or `/usr/local/bin` (Intel), which must come before `/bin` on your `PATH`.
Current Linux distributions are all well past 4.4, so this is a macOS concern in practice. `make doctor` reports the version it found and, on a bash below the floor, names it instead of failing on the shopt.

That registry is also the project's **approved list of host CLI dependencies**.
If new work needs a tool that isn't listed, raise it before relying on it — once agreed, add a row to [`scripts/ci/check-tools.sh`](scripts/ci/check-tools.sh) (and to the prerequisites above when it's a hard requirement) so every contributor and `make doctor` stay in sync.
Go build- and codegen-time tools are handled differently: pin them in the vendored [`tools/`](tools/README.md) module rather than adding a host dependency.

**Optional — AI-assisted development (Claude Code):** Two skills from [`karlkfi/claude-skills`](https://github.com/karlkfi/claude-skills) are recommended:

- [`model-advisor`](https://github.com/karlkfi/claude-skills/tree/main/model-advisor) — model and thinking-level recommendations at session start and on task shifts.
- [`tech-docs-layers`](https://github.com/karlkfi/claude-skills/tree/main/tech-docs-layers) — applies the six-layer model of technical documentation when writing, editing, or restructuring docs.

```bash
# clone once, then symlink into your user-level skills directory
git clone git@github.com:karlkfi/claude-skills.git ~/workspace/claude-skills
ln -s ~/workspace/claude-skills/model-advisor    ~/.claude/skills/model-advisor
ln -s ~/workspace/claude-skills/tech-docs-layers ~/.claude/skills/tech-docs-layers
```

Three guard plugins are also recommended — `PreToolUse` hooks that keep AI-assisted work on the rails this repo expects (worktree-scoped edits, `claude/*` feature branches, no accidental destructive commands against the shared dogfood cluster).
Install all three from within Claude Code:

```
/plugin marketplace add karlkfi/claude-workspace-guard
/plugin install workspace-guard@workspace-guard

/plugin marketplace add karlkfi/claude-branch-guard
/plugin install branch-guard@branch-guard

/plugin marketplace add karlkfi/claude-prod-guard
/plugin install prod-guard@prod-guard
```

- [`workspace-guard`](https://github.com/karlkfi/claude-workspace-guard) — path-aware bash permissions: prompts when a guarded file command targets a path outside the project root.
- [`branch-guard`](https://github.com/karlkfi/claude-branch-guard) — prompts before commits, pushes, or destructive git commands on a protected branch (`main`/`master`).
- [`prod-guard`](https://github.com/karlkfi/claude-prod-guard) — denies destructive commands (`kubectl`/`helm`/`gcloud`/`terraform`) aimed at production-classified targets.
  This repo ships [`.claude/prod-guard.json`](.claude/prod-guard.json) marking the shared GKE dogfood cluster (`gag-dogfood`) as production, so ad-hoc `kubectl delete`/`helm uninstall`/`gcloud clusters delete` against it are blocked unless prefixed with `PROD_GUARD_OVERRIDE=<reason>`.

Restart Claude Code after installing so the hooks register (`python3` and `git` must be on your `PATH`).

**These plugins are built by this project's maintainer, and this repo is their primary dogfood.** That makes the guards part of what is being developed here, not just tooling around it.
So a prompt that fires wrongly, a pattern that misses, or a denial with an unhelpful message is a **finding**, and it gets filed against the plugin's own repo rather than worked around in a session.
Working around one fixes nothing for the next session, the next repo, or anyone else running the plugin.
The same goes for bugs found in Claude Code itself, and in upstream projects this exercises; several have been reported from work on this repo.

The point of the guards is trust, and trust is what buys speed: they are why AI-assisted work here can move quickly without risking a leaked credential, a destroyed cluster, or a surprise cloud bill.
A change that buys throughput by weakening one of those is a bad trade even when the throughput is real.

Build the vendored tool binaries and install the git hooks before doing anything else:

```bash
make tools         # builds controller-gen, setup-envtest, ginkgo, kubebuilder into .build/
make hooks         # installs the tracked pre-commit hook (core.hooksPath -> .githooks)
make merge-driver  # installs the Markdown merge drivers (optional, recommended)
```

`scripts/dev/setup.sh` runs both of the last two for you.
The pre-commit hook is a sub-second gate, each part firing only when the file type it covers is staged: gofmt on staged Go files, the `docs/STATUS.md` format lint, and the em-dash ceiling when any Markdown is staged.
Bypass a single commit with `git commit --no-verify`.

**Using linked worktrees?
Check that `core.hooksPath` is still relative.** `make hooks` sets it to `.githooks`, which each worktree resolves against its own checkout — that is the point.
But a worktree can arrive with it already pinned to an absolute path: worktree-creation tooling writes `.git/worktrees/<name>/config.worktree`, and that file has been seen carrying `hooksPath = /abs/path/to/main/.githooks` next to `core.longpaths`, both stamped in the same second as the worktree itself.
Nobody opts into it, so check rather than assume:

```bash
git config --show-origin --get-all core.hooksPath
```

An absolute pin makes every worktree run the **main** checkout's hook against **its own** tree.
Those agree right up until a branch moves or renames something the hook invokes — then that PR blocks commits in every worktree until it merges, and the error names a path that looks correct for the worktree with nothing to say the hook came from elsewhere.
Q571 hit exactly this when `lint-backlog.sh` moved into `scripts/docs/`; `--no-verify` is the escape, which is when you least want to be skipping the gate.

Repoint a worktree's own copy with `git config --worktree core.hooksPath .githooks`.
Note the repo-level value in `.git/config` is shared by every worktree, so `make hooks` there fixes all of them at once — but a `config.worktree` entry outranks it and has to be cleared per worktree.

`make merge-driver` is a per-clone `git config` that makes `docs/STATUS.md` conflicts resolve by backlog row ID, `docs/plan/README.md` conflicts by plan path, and `docs/roadmap.md` conflicts by each bullet's `<!-- q:QN -->` backlog annotation, instead of by line position — all three files are high-contention and their conflicts are usually an artifact of two rows being adjacent, not a real disagreement.
Git will not let a tracked file define a merge driver's command, so this half cannot be committed.
It is genuinely optional: without it, git uses its built-in three-way merge, and with it anything ambiguous still gets ordinary conflict markers.
Details: [`docs/development/maintaining-backlog.md`](docs/development/maintaining-backlog.md#the-merge-driver-resolve-queue-rows-by-id-not-by-line-position). **The config stores the driver script's path**, so a clone that installed it before the script moved to `scripts/docs/` has a dead path — re-run `make merge-driver`.

## Design first

Before starting non-trivial work, read `DESIGN.md` and any relevant section under `docs/design/` to confirm your plan matches the design intent.
The four-tier architecture has load-bearing constraints — particularly around egress isolation, zero-idle compute, and multi-tenant security boundaries — that are easy to accidentally violate with a well-intentioned shortcut.

## Building

```bash
make build       # all binaries → .build/agc, .build/gmc, .build/probe, .build/proxy
make build-agc   # single binary
```

See [`docs/development/building.md`](docs/development/building.md) for the full target list and output layout.

## Testing

The repo uses a `go.work` workspace. `go test ./...` from the root does **not** work — use per-module commands:

```bash
(cd broker     && go test ./...)
(cd githubapp  && go test ./...)
(cd cmd/agc   && go test ./...)
(cd cmd/gmc   && go test ./...)
(cd cmd/probe && go test ./...)
(cd cmd/proxy && go test ./...)
(cd cmd/worker && go test ./...)
```

Integration tests require `KUBEBUILDER_ASSETS`.
See [`docs/development/testing.md`](docs/development/testing.md) for setup.

## The pre-review gate

Before requesting review or opening a PR, run the one-command gate:

```bash
make check
```

To see what it covers, ask the target rather than a list in a doc — `make list-gates` prints every gate `make check` runs, in order, with what each one checks.

`make check` runs exactly what `.github/workflows/unit-test.yml` runs, so a green `make check` means a green unit-test workflow — run it locally to avoid burning CI.
The slower security gates (`make vulncheck`, `make trivy-scan`) and the integration/e2e tiers are kept separate so this loop stays fast; run them when your change warrants it.

**Before merging, confirm CI actually tested the code.** Most heavy gates (integration, security scans, manifest-validate) are path-gated; a PR that was **opened as docs-only and later had code pushed** can leave those workflows *skipped* while still showing all-green and mergeable — shipping untested code to `main`.
Avoid it by putting code in the PR's first push, and verify with `gh pr checks <n>` / `gh run list` that the relevant gates ran (close+reopen the PR to force them if they were skipped).
Both e2e lanes are the exception: they run at merge-queue time only, so an absent e2e run on a PR is expected rather than a skipped gate.
Run `make e2e` locally when you want that verdict before the queue gives it.
See [`docs/development/testing.md`](docs/development/testing.md#path-gated-workflows-verify-the-heavy-gates-actually-ran).

## Linting

`make lint` runs `gofmt -s` and `golangci-lint` across every workspace module. `golangci-lint` runs `govet` internally (enabled in [`.golangci.yml`](.golangci.yml)), so it is not invoked separately. `golangci-lint` is vendored in `tools/` and built into `.build/golangci-lint`.
CI runs the same gates in `.github/workflows/unit-test.yml`.

For the full e2e suite against a local kind cluster:

```bash
make e2e-up     # create cluster, build+push images, run cluster-only + fake-GitHub suites
make e2e-clean  # tear down the cluster when done
```

## Changing dependencies

When you change any module's `go.mod`:

1. Run `scripts/go/go-work-tidy.sh` to tidy all modules in dependency order.
2. Run `go work sync` to sync the workspace build list.
3. Run `go work vendor` at the repo root to update the shared `vendor/`.
4. Commit the `go.mod`, `go.sum`, and `vendor/` changes together in the same commit.

Do not run `go mod tidy` or `go mod vendor` inside an individual module — that conflicts with the workspace vendor.
See [`docs/development/go-workspaces.md`](docs/development/go-workspaces.md) for the full vendoring discipline and worktree layout.

## Modifying CRD types

After editing types under `cmd/agc/api/` or `cmd/gmc/api/`, regenerate manifests and deepcopy code.
There is a silent failure mode with RBAC markers that's worth knowing about before you hit it.
See [`docs/development/code-generation.md`](docs/development/code-generation.md).

## Code standards

- Public types, functions, and packages must have godoc comments.
- Tests must verify behavior, not just that the code runs.
- Async functions return a `<-chan struct{}` done channel — callers decide whether to block, select with timeout, or ignore.
- All modules in the repo must use the same Go version.
- Shell scripts follow the repo bash conventions — see [`docs/development/bash-style.md`](docs/development/bash-style.md).

## Documentation

- Style, conventions, and maintenance for all docs live in [`docs/development/documentation-standards.md`](docs/development/documentation-standards.md) — read it before writing or restructuring a doc.
  The essentials:
- After a behaviour change, update every doc the change touches — the change-type → docs mapping is in [`docs/development/doc-update-matrix.md`](docs/development/doc-update-matrix.md).
  Design-doc updates alone are not enough when a change alters what an operator does, configures, or observes.
- Humans start at [`README.md`](README.md) and navigate the [`docs/`](docs/README.md) tree.
  Do **not** link to `CLAUDE.md`/`AGENTS.md` from any human-facing doc — that file is the entrypoint for AI agents only.
  Reference content humans need lives in `docs/` or this file.
- Spell out acronyms on first use: full term, then the acronym in parentheses — e.g. "Actions Gateway Controller (AGC)".
- Long docs (roughly 400+ lines) carry a `## Table of Contents` section after the intro, listing h2 headings (plus h3 for operator-facing docs).
  Anchors follow GitHub's slug rules — duplicate headings get `-1`/`-2` suffixes — so verify links against the rendered page.

## Commits

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat(agc): add retry budget metric for exhausted jobs
fix(gmc): correct RBAC verb for lease escalation
docs: add vendoring discipline to CONTRIBUTING
```

Keep commits small and focused.
Never commit broken code or failing tests.
Amending unpushed commits is fine; once pushed, prefer a follow-up commit unless a rebase is explicitly needed.

### Skip the reflow commit in `git blame`

Every prose line was rewritten once, mechanically, when the docs moved to one sentence per line.
That commit sits between each line and the author who wrote it.
[`.git-blame-ignore-revs`](.git-blame-ignore-revs) lists it; point git at the file once per clone:

```bash
git config blame.ignoreRevsFile .git-blame-ignore-revs
```

Without it, `git blame README.md` credits 93 lines to the reflow.
With it, none.
The file holds full 40-character commit hashes because that is all `git blame` parses — a short hash or a tag makes it exit with `fatal: invalid object name`, and a hash naming no commit is accepted and silently matches nothing.

### Pushing to a PR that is already open

Prefer putting everything in the first push — but "never push again" is too strong, and the reason matters.
Every merge goes through the **merge queue** (`gh pr merge --squash` enqueues; the queue validates the candidate merge and lands it), so the old race (a direct squash-merge overtaking a just-pushed commit and stranding it) cannot happen: while a PR is queued, a push to its branch is **rejected** (`GH006 ... Branches that are queued for merging cannot be updated`, measured 2026-08-12), so the candidate the queue is validating cannot change under it.

The cost of a push to a PR that is *not* queued is therefore CI and queue position, not a stranded commit.
On a docs-only PR the gates are seconds, so an amend is cheap; on a Go change a push restarts `integration-test` and the security scans, roughly ten minutes, and sends the PR to the back of the queue, where it then pays the e2e lanes for the first time.
Weigh that before pushing a nicety onto a PR that is otherwise done.

**The practical guard is a check immediately before the push, not a rule of thumb:**

```sh
gh pr view <n> --json state,mergeStateStatus --jq '{state,mergeStateStatus}'
```

`OPEN` is safe to push, with one exception: a `QUEUED` PR **rejects** the push.
Wait for the queue to land or evict it, then push; never dequeue to make room for your own push, because the enqueue was a human's decision and dropping it silently revokes their merge authority.
This bites hardest when a maintainer enqueues while you are mid-rebase, so the rejection arrives on a heal you were asked to perform.
A state you read ten minutes ago is not the state you are pushing into.

What the rejected push *carries* matters as much as the rejection.
If it holds a correction to something the merging head asserts, the queue lands the uncorrected claim and the fix needs its own follow-up.
Measured 2026-08-12 on #1436: a one-line `scripts/README.md` narrowing was rejected while the PR merged, so `main` took the overstated row and the correction became #1459.
So read your unpushed commits before waiting the queue out, and say in the handback what the merged head is missing; the wait is also a decision about what ships.

`MERGED` is the state worth naming separately, because the push *appears to work*.
The merge deletes the branch, so pushing to it **recreates** it — git says `* [new branch]`, you get a branch with no PR and a commit that will never reach `main`, and nothing errors.
Two tells: the `new branch` line on a branch you have pushed to before, and `git status` reporting `your upstream is gone`.
The recovery is to move the commit rather than re-push: confirm the PR's own work landed by content, `git checkout -b <new> origin/main`, `git cherry-pick <sha>`, and open a fresh PR.
The stray branch then needs deleting (`git push origin --delete <branch>`), which is easy to leave behind.

**A stale base no longer forces a rebase — a conflicting one does.** The queue merges the candidate result of your branch against current `main`, so being commits behind is fine; only a textual conflict (`mergeStateStatus: DIRTY`) blocks the queue, and pr-sentinel wakes the session for that.
Rebasing onto a moved `main` is still worth it when what moved can affect your gate — `git diff HEAD...origin/main --stat` says what did — because a queue kickback costs a full check cycle where a local re-run would have caught it first.
The local-gate window varies by an order of magnitude across the machines this repo has been measured on (a cold `make check` is ~21 min on a 4-core Intel i7 and 102 s on an 18-core M5 Max — [measurements](docs/plan/archive/local-gate-throughput.md)), so the longer your gate, the more of `main`'s merge traffic lands inside it.

**A hook asks about the overlap at the push itself** (Q665), because the rule above is read before the gate starts and the push happens after it.
It fires only when what `main` gained overlaps the files this branch changes. `main` takes ~47 merges a day, so "the base moved" alone is true at nearly every push and would be accepted without reading.
It reads the local `origin/main` ref and never fetches, so it under-reports until you do, and it stays silent when the probe fails.
The merge-driver-owned files are discounted, but only while the merge really resolves them, which the hook settles by asking `git merge-tree` rather than by assuming (Q790).
The driver refuses on a row deleted on one side and edited on the other, the shape every flake-row move takes, and a branch whose only changed file is `docs/STATUS.md` can be `DIRTY` on that alone.

Whatever happens, verify what actually landed **by content, not SHA** — see below.

### Re-check concurrent work before opening

The check at the start of a task has a shelf life of minutes; run it again immediately before `gh pr create`.
Two halves, because concurrent work reaches you two ways: as an open PR, and as something that already merged.

```sh
git fetch origin main && git log --oneline HEAD..origin/main
gh pr list --json number,title --jq '.[] | "#\(.number)\t\(.title)"'
```

**Fetch before you compare.** `origin/main` is a local remote-tracking ref, so `git diff HEAD...origin/main` and `git log HEAD..origin/main` are only as fresh as your last fetch.
On a stale ref they report a clean base while `main` has moved, which is indistinguishable from being up to date.
Nothing in a normal session refreshes that ref on its own.

**Work that merged under you can change your own gate.** A moved base is usually just a rebase at merge time, but not when what merged is the machinery your branch is gated by.
During #1342, #1334 merged mid-session carrying a change to `scripts/ci/run-parallel.sh` and to `SCRIPTS_TESTS`, the fan-out backing `make scripts-test`, which that branch was adding a suite to.
It surfaced only incidentally, after both commits were written, and cost a rebase, a recommit, and a fourth full `make check`.
Fetch before the final gate run, not after it: a rebase discovered afterwards voids the verdict you just paid for.

**An open PR can overlap yours** — read its diff and its body, not its title. #1093 was opened mid-session on a topic that overlapped the Q577 change, and carried evidence disproving the remedy that change was about to ship in `stop.sh`'s error text.
The title said nothing about that.
Revise before opening; if the other PR's evidence invalidates yours, put that on the Queue row instead of shipping around it.

**A hook asks when it sees the overlap** (Q668): at `gh pr create` it compares this branch's files against every open PR's and names the ones that collide, discounting the merge-driver-owned files that every branch edits.
It costs one `gh pr list` call and stays silent when that fails (offline, or a rate-limited token), so it can miss an overlap but never blocks the create.

**The jointly-red case is machine-checked at merge time.** Two individually green PRs used to merge into a red `main` because a PR gate only ever sees its own base — #1062 raised MkDocs' link validation to strict-build warnings (Q560) while #1063 added a link that trips it (Q558); each passed without the other and the merged tree built dirty.
The **merge queue** closes this for the workflows it actually runs: every merge validates the candidate result (your branch plus whatever is ahead of it in the queue, on current `main`) before it lands, and a failing entry is kicked back to its PR with the failure attached, the signal pr-sentinel already reacts to.
There is no manual union-gate or pre-merge freshness check to run; enqueue and let the queue arbitrate the race.

**It closes it only for a workflow that declares `merge_group`,** which is 9 of the 25.
[`doc-links.yml`](.github/workflows/doc-links.yml) does not, so its five gates (`em-dash`, `doc-links`, `gate-lists`, `release-links`, `release-pins`) are validated against each PR's own base and never against the candidate merge result.
Measured 2026-08-08: #1340 and #1342 each added em-dashes to `docs/development/testing.md`, each was green alone, and the pair landed it at 595 against a ceiling of 594, turning `main` red on a gate the queue had no opportunity to run.
Q743 carries the fix.
Until it lands, the jointly-red case is still live for those five, and a ratcheted gate (em-dash density, the coverage floor) is where it bites: two branches can each sit at the ceiling.

### When new work blocks an open PR

Work requested *after* a PR is open normally branches off `main` and gets its own PR.
The exception is a fix the open PR **cannot go green without** — a blocking bug it surfaced, say.
Branching that off `main` leaves the first PR red with no path forward, so branch it off `main`, then rebase the blocked PR **onto the fix branch** and say so in the PR body.
Reviewers merge the fix first.

The same stacking applies to work that *depends on* an open PR (needs a file only that branch has): base on its branch and open the stacked PR against it. **Re-check the base PR's state immediately before `gh pr create`** — it can merge (and its branch delete) between your fetch and the create, at which point the create fails with `Base ref must be a branch`; the fix is the `--onto` rebase below, then a plain PR against `main`.

Two mechanics make the follow-through safe once the base lands:

- **Rebase with `--onto` to drop the merged commits.** PRs here squash-merge, so the base's commits reach `main` under a new SHA and a plain `git rebase origin/main` tries to replay your local copies on top of themselves.
  Use `git rebase --onto origin/main <old-base-tip>` to replay only the commits that are genuinely yours.
- **Verify the base landed by content, never by SHA.** A squash orphans the original SHAs, so `git log` cannot tell you the work is in.
  Check the code: `git show origin/main:<path> | grep <the symbol you added>`.
- **Retargeting the PR does not re-point CI — the push does.** `gh pr edit --base main` changes where the PR merges, but the checks already queued still resolve their range against the base SHA recorded at the last push, which the squash has orphaned.
  The merge-base then falls back to a commit predating the base PR, and range-scanning gates report on `main`'s own history: `lint-status` will name other people's commits as mixing `docs/STATUS.md` with code.
  Nothing is wrong with your branch.
  Do the `--onto` rebase and push, and the next run computes a real merge-base.

### When `main` is broken

A red gate on your branch is not yours until it fails on the base too ([how to tell](docs/development/testing.md#the-status-you-report-is-a-claim-too)).
Once it does, the fix gets its own PR.
It never rides inside an unrelated one, however one-line it is and however completely it blocks your own gate.

**Search before writing it.** One `gh pr list` is the whole search, and an open PR fixing it means the work is claimed.
Wait for that PR to merge and rebase onto `main`; stack on its branch per [When new work blocks an open PR](#when-new-work-blocks-an-open-pr) only if you cannot wait.
Do not write a second fix: both land on the same line, and one of you takes a conflict for nothing.

**Own it if nobody has,** and claim it by opening the PR as a **draft** the moment the branch carries the fix, before you have finished verifying it.
The draft is what makes the search above work. `gh pr list --state open` includes drafts, so the next blocked session finds it, and the `gh pr create` overlap check (Q668) names it to that session without anyone having to look.
An issue would not: the check reads open PRs.
Take it out of draft when it is genuinely ready.

The obligation carries as much weight as the prohibition.
A rule that only forbids embedding the fix leaves nobody obliged to write it, so every blocked session declines in turn and `main` stays red, which is worse than the duplication it replaced.
Search-then-claim yields exactly one fixer, because the second session to look finds the first one's PR.

Measured 2026-08-08, when `main` went over this file's own em-dash ceiling: four sessions each wrote a one-line fix, two of them editing the same line with different wording.
Two landed, #1351 cutting one em-dash inside a PR about a pre-commit hook and #1353 cutting two more as a PR of its own, so an overrun that needed one fix got two.
The guidance at the time asked for exactly that, telling every blocked PR to carry the fix as though one session sees the breakage at a time.

### A conflict inside a section your change deletes

Deleting or replacing a section makes conflicts in it **semantic, not textual**.
Git offers a two-sided choice, but neither side is right: "theirs" restores the section you removed, and "ours" silently discards whatever the other branch put there.
Both look clean afterwards, and only the second one is invisible.

Read what the other side *added* before resolving, and give it a home in the new structure.
When #1072 replaced the roadmap's "Available now" section with [features.md](docs/features.md), a concurrent groom had added a shipped capability to exactly that section — taking "ours" would have dropped a documented feature from the site with a green build and no reviewer signal.
The same groom moved a bullet between two other sections, which had to be carried across rather than re-resolved.

The tell is that the conflict sits in a region your diff removes wholesale.
When you see that, resolve by rehoming, then diff your result against the other branch to confirm nothing of theirs vanished:

```sh
git diff origin/main -- <the file you restructured>
```

Queue items in `docs/STATUS.md` are identified by `Q`-prefixed IDs (e.g. `Q44`).
Use the bare ID in commit messages and PR bodies — its `Q` prefix is what keeps GitHub from auto-linking to PR/issue 44 (`#44` would be linked, `Q44` is not).

## Security

Defaults must never trade away a security property for convenience.
If a change regresses any security property — even partially — raise it explicitly before shipping.
See [docs/design/05-security.md](docs/design/05-security.md) for the threat model and examples of what counts as a regression.
