# Backpressure: the developer/agent feedback loops

**Backpressure** is any machine-readable signal that pushes back on bad changes
*before* a human (or coding agent) spends attention reviewing them. A typed
compiler error, a failing test, a lint finding, a rejected commit — each lets the
author see the failure and fix it without a reviewer having to find and explain
it. The term and the framing here are from
[*Stop Babysitting Your Coding Agent*](https://generativeprogrammer.com/p/stop-babysitting-your-coding-agent);
this doc records which mechanisms this repo has, how they are layered by speed,
and a graded assessment of the result with the evidence enumerated.

The guiding principle: **layer feedback by speed** — the cheapest checks fire in
the tightest loop (at commit time), slower checks run on demand before review,
and the full suite runs in continuous integration (CI). A second principle:
**the local verdict must match the CI verdict**, so a green local check is
trustworthy and you don't burn a CI round-trip to learn what a local command
could have told you.

## The three tiers

| Tier | What runs | When | Typical cost | Source of truth |
|---|---|---|---|---|
| **Pre-commit hook** | `gofmt` on staged `.go` files; `docs/STATUS.md` format lint when that file is staged | every `git commit` | ~0.5s | [`.githooks/pre-commit`](../../.githooks/pre-commit) |
| **`make check`** | `gofmt` + `golangci-lint` + `STATUS.md` lint + unit tests, all modules | manual, before requesting review | minutes cold / seconds warm | [`Makefile`](../../Makefile) `check` target |
| **CI** | the above **plus** integration (envtest), end-to-end (e2e, `kind`), `govulncheck`, and `trivy` image scans | on every pull request and push to `main` | full | [`.github/workflows/`](../../.github/workflows/) |

Installation: `make hooks` (or [`scripts/setup.sh`](../../scripts/setup.sh), which
runs it for you) points `core.hooksPath` at the in-repo `.githooks/` directory.
The path is relative, so it resolves correctly in the main checkout and every
linked worktree. Bypass the hook for a single commit with `git commit --no-verify`.

## Mechanisms by category

| Category | Mechanism | Where enforced |
|---|---|---|
| Type checking | Go compiler (`go build`, and `go vet` via `golangci-lint`) | `make build`; `make check`; CI |
| Unit tests | per-module `go test` (Go workspace — no root-level `./...`) | `make check` → `make test`; [`unit-test.yml`](../../.github/workflows/unit-test.yml) |
| Integration tests | envtest, `integration` build tag, `cmd/agc` + `cmd/gmc` `internal/controller/integration/` | `make test-integration`; [`integration-test.yml`](../../.github/workflows/integration-test.yml) |
| End-to-end tests | `kind` cluster, `e2e` build tag, Tier A/B/C (see [07-test-plan.md](../design/07-test-plan.md) §7.3) | `make e2e`; [`e2e-test.yml`](../../.github/workflows/e2e-test.yml) |
| Linting / formatting | `gofmt -s`, `golangci-lint` (govet, staticcheck, ineffassign, unused) | `.githooks/pre-commit` (gofmt); `make check`; `unit-test.yml` |
| Structural rules | `scripts/lint-status.sh` — `docs/STATUS.md` format: single-line `Last touched:`, no duplicate Queue IDs, Notes ≤250 chars | `.githooks/pre-commit`; `make check`; [`status-lint.yml`](../../.github/workflows/status-lint.yml) |
| Vulnerability scan | `govulncheck` (symbol-reachable CVEs, per module) | `make vulncheck`; [`security-scan.yml`](../../.github/workflows/security-scan.yml) |
| Image scan | `trivy` (OS + library CVEs, per image) | `make trivy-scan`; `security-scan.yml` |

Real-CNI / kube-proxy / kubelet / TLS-over-tunnel behaviours are only observable
at the e2e tier — see [testing.md](testing.md) and [07-test-plan.md](../design/07-test-plan.md)
§7.3 for why unit and envtest tiers cannot prove them.

## Compress success, expand failure

Test output is non-verbose by default. `go test` without `-v` prints one
`ok <pkg>` line per passing package and the **full** output of any package that
fails — so passing runs stay quiet and failures keep all their detail. Add
`V=1` (`make check V=1` or `make test V=1`) to stream output live when debugging
a slow or hanging test: without `-v`, `go test` buffers each package's output
until the package completes, so a hung test shows nothing — not even its
`t.Log` lines — until it finishes or hits `-timeout`; with `-v` the output
streams as it is produced. See [testing.md](testing.md#the-make-check-pre-review-gate).

## Assessment

Graded against the article's rubric on 2026-06-06. Overall: **A**.

Each dimension below lists the grade and the concrete evidence behind it.

### Sensor coverage — A
Type checking (Go compiler + `go vet`), three test tiers (unit/integration/e2e),
formatting and linting (`gofmt -s`, `golangci-lint` with govet/staticcheck/
ineffassign/unused per [`.golangci.yml`](../../.golangci.yml)), supply-chain
scanning (`govulncheck`, `trivy`), and custom structural eval scripts
(`scripts/lint-status.sh`, `scripts/queue-unblock.sh`). Browser/screenshot
sensors are correctly absent — there is no user interface.

### Layer by speed — A
A clean three-rung ladder, fastest first: pre-commit hook (~0.5s) →
`make check` (lint + unit tests) → CI (adds integration, e2e, `govulncheck`,
`trivy`). Slow tiers are path-gated (`dorny/paths-filter`) and use
`cancel-in-progress` to reclaim superseded runs. The e2e suite carries
`--poll-progress-after 30s` so a long-running spec reports progress without full
verbosity. CLAUDE.md tells contributors which tier proves which bug class.

### Actionable + trustworthy feedback — A
Local verdict equals CI verdict by deliberate construction:
- `make trivy-scan` is "mirrored exactly by the CI `trivy` job so local and CI verdicts match" ([`Makefile`](../../Makefile)).
- `make vulncheck` "matches the CI `govulncheck` gate."
- `make check` runs the same gofmt + `golangci-lint` + `STATUS.md` lint + unit tests as [`unit-test.yml`](../../.github/workflows/unit-test.yml), so a green `make check` means a green unit-test workflow.

This parity is what makes the fast local signal worth trusting.

### Document expectations — A−
CLAUDE.md carries a doc-update matrix, a test-tier selection guide, and an
agent-reference table mapping task → doc; [testing.md](testing.md) is the
canonical run-command source; `make check` is the single "run before review"
command. Held just short of A because expectations are spread across prose plus
the one command rather than a single consolidated checklist.

### Self-reinforcing (correct twice → automate) — A
The article's core principle is visibly practiced. [`.golangci.yml`](../../.golangci.yml)
states its scope is to "catch regressions of the bugs and idiom violations
tracked in Queue items 38–41." `scripts/lint-status.sh` exists solely because
the `docs/STATUS.md` format (e.g. the 250-char Notes cap) kept getting violated.
The `claude-workspace-guard` PreToolUse hook is real-time backpressure on file
operations, and `claude-branch-guard` is the same on git operations — prompting
before commits, pushes, or destructive commands on a protected branch. Each is a
repeated correction turned into an automated gate.

### Compress success, expand failure — A−
The agent-loop path is non-verbose by default (native compress-success /
expand-failure from `go test`), with a `V=1` opt-in for live streaming when
debugging a hang. Minor deduction: `integration-test.yml` hard-codes `-v`, so
its passing runs are noisy — a deliberate CI-only choice (envtest failures are
opaque and those logs are read only when a run fails, exactly when verbosity
helps).

### Fast inner-loop enforcement — A−
Backpressure now fires at commit latency, not only CI latency: a tracked,
auto-installable, sub-second pre-commit hook plus the `make check` aggregate
gate. The only deduction is that git cannot set `core.hooksPath` automatically
on clone, so installation is a one-time `make hooks` / `scripts/setup.sh` step
rather than truly automatic.

## Remaining nits

- **Hook install is one-time, not automatic on clone** — a git limitation; `make hooks` / `scripts/setup.sh` is the workaround.
- **`integration-test.yml` hard-codes `-v`** — noisy on green, but CI-only and useful for opaque envtest failures (deliberate).
- **The pre-commit hook covers `gofmt` + `STATUS.md` only, not `golangci-lint`** — deliberate, to stay sub-second; `golangci-lint` runs in `make check` and CI.
