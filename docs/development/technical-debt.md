# Technical debt: policy and strategy

How this project classifies technical debt, decides what to do about each piece,
and keeps it from accumulating. This is the **policy** (the rules) and the
**strategy** (the lifecycle that enforces them). The mechanics live in adjacent
docs and are linked rather than repeated:

- [maintaining-backlog.md](maintaining-backlog.md) — how to record and prioritize a debt item in [docs/STATUS.md](../STATUS.md).
- [backpressure.md](backpressure.md) — the automated feedback loops that stop debt at authoring time.
- [release-1.0.md](../plan/release-1.0.md) — the quality gates that block the 1.0 release (bucket F).
- [appendix-g-future-enhancements.md](../design/appendix-g-future-enhancements.md) — long-horizon non-commitments.

## What we mean by technical debt

Technical debt is work we **knowingly defer** that trades short-term progress for
a long-term carrying cost — a shortcut taken on purpose, or erosion we have
noticed but not yet paid down. It is distinct from a plain bug: a bug is
incorrect behavior we want fixed; debt is a *known* gap between what exists and
what we would build with unlimited time. A debt item may *cause* bugs (the
[Q76](../STATUS.md) agent-pool claim race is debt that can corrupt sessions), but
the defining trait is that we are choosing — explicitly — when to pay it.

The policy below exists so that choice is always **explicit and recorded**, never
an accident of an unreviewed shortcut.

## Classification

We classify each item so its severity and owner are obvious. The taxonomy is the
common one (architectural, code, infrastructure, test, documentation, security),
mapped to where it shows up here:

| Kind | What it looks like in this repo |
|---|---|
| **Architectural** | Coupling or boundary erosion across the four tiers (GMC, AGC, proxy, worker); e.g. two logging libraries emitting incompatible JSON. |
| **Code** | Duplicated logic, tangled functions, dead annotations, residual `interface{}`. Caught increasingly by lint (see [backpressure.md](backpressure.md)). |
| **Infrastructure** | Gaps in continuous integration (CI), manifests, or observability — unvalidated YAML, missing scrape wiring, single-replica defaults. |
| **Test** | Coverage gaps, missing tiers, behaviors only a higher tier can prove (see [testing.md](testing.md)). |
| **Documentation** | Drift between code and docs — unregistered metrics, stale runbooks. |
| **Security** | Deferred hardening, missing validation, an unscanned dependency. Subject to the [secure-by-default](#secure-by-default-is-not-negotiable) rule below. |

There is no **design / user-experience (UX)** category: this is a backend
operator system with no user interface. Its analogue — operator and Custom
Resource Definition (CRD) ergonomics — is covered by the `docs/operations/` tree
and CRD validation, not tracked as UX debt.

## Policy

### Make the smallest change, then choose: fix, flag, defer, or decline

When you touch the code and notice debt outside the current task's scope, do
**not** silently expand the change to fix it. Decide deliberately:

1. **Fix now** — only if it is *in scope*, *quick*, and *low-risk*. A
   behavior-preserving cleanup that the current change naturally touches (e.g.
   extracting a duplicated security context while editing the builder it lives
   in) qualifies. Verify it changes no behavior.
2. **Flag to the Queue** — near- or long-term work that someone should do but not
   now. Add a row to the Queue in [docs/STATUS.md](../STATUS.md) **at the
   priority it deserves**, with the *why* of any decision it depends on. Follow
   [maintaining-backlog.md](maintaining-backlog.md).
3. **Defer** — a real commitment with no near-term intent, waiting on an explicit
   trigger (a tool, a cluster, a dependency that does not exist yet). It goes in
   the **Deferred** section of [docs/STATUS.md](../STATUS.md), out of the
   priority ordering, and returns to the Queue when its trigger fires.
4. **Decline** — a long-horizon idea we are explicitly *not* committing to. It
   belongs in [appendix-g-future-enhancements.md](../design/appendix-g-future-enhancements.md),
   not the backlog.

The bias is toward (2): **flag rather than fix out of scope.** A bundled
"while I was here" fix inflates a diff, hides the real change from review, and
couples unrelated risk.

### Capture knowledge durably, not in chat

A debt item that exists only in a conversation is lost. The moment you decide an
item is worth doing later, it must land in the repo — a Queue row, a plan doc, or
this policy. The same applies to the *reason* behind a decision: record the *why*
on the item, because the next person (or session) acting on it starts cold.

### Secure-by-default is not negotiable

Security debt has one extra rule: **a security regression may never become a
default to buy convenience.** If an option would weaken any security property —
removing a validation, relaxing a pod profile, switching to a weaker key type —
the secure choice stays the default. The weaker option may exist only as a
documented, explicit opt-in. Such a trade-off is raised and signed off *before*
shipping, never deferred silently as ordinary debt.

## Strategy: the debt lifecycle

Policy decides what to do with one item. Strategy is the loop that keeps the
whole codebase from accumulating debt faster than we pay it down.

1. **Prevent.** The cheapest debt is the kind that never lands. Layered
   [backpressure](backpressure.md) — the pre-commit hook, `make check`, and CI —
   rejects whole classes of debt at authoring time. The guiding habit is
   **correct it twice, then automate it**: a mistake worth catching once is worth
   a gate (this is why `scripts/lint-status.sh` and the [bucket-F gates](#quality-gates-as-debt-brakes) exist).
2. **Detect.** What prevention misses, a periodic **review pass** finds: read the
   code for the taxonomy above, scan for stale markers, and check whether new
   work re-introduced a class a gate was supposed to hold. Flaky CI is itself a
   detection signal (see below).
3. **Triage and track.** Classify the item, decide its disposition with the
   [policy](#policy) above, and record it. Prioritize **on entry** — position in
   the Queue *is* the priority, so a new row is placed where it belongs, not
   appended to the bottom by default.
4. **Pay down.** Work the Queue from the top. Two ordering rules override raw
   position: **flake fixes go first** (a flake's cost compounds across every
   future pull request), and a `1.0-gate`-labeled item blocks the release
   regardless of where it sits. Blocked items sort below their blocker with a
   machine-readable `Blocked by [QN]` note.
5. **Keep it paid.** Once a class of debt is paid down, a gate keeps it from
   returning. Paying down duplication is worth little if the next session
   re-introduces it; the `dupl` gate is what makes the cleanup durable.

### Flake fixes go first

If a test passes on rerun with no code change, that flake is debt with a
compounding cost — every future pull request can hit it, burning CI time and
attention. File it and move it to the **top** of the Queue before continuing
other work. The full rule is in
[maintaining-backlog.md](maintaining-backlog.md#flake-fixes-go-first).

## What we measure — and deliberately don't

Most published technical-debt metrics assume a team, an issue tracker with
timestamps, and a production deployment cadence. This project is a pre-1.0
codebase with a Markdown backlog and no shipped deployments, so those metrics
would be ceremony without signal. We track the ones that are cheap, automatable,
and catch real regressions — and we are explicit about the rest.

| Metric | Decision |
|---|---|
| **Test coverage** | Track — measured in CI, gated by a no-regression ratchet ([Q77](../STATUS.md)). |
| **Code duplication** | Track — `dupl` linter ([Q78](../STATUS.md)). |
| **Data-race freedom** | Track — `-race` on unit tests, the core concern for a goroutine-multiplexing engine ([Q79](../STATUS.md)). |
| **Static security findings** | Track — `gosec` ([Q80](../STATUS.md)); unchecked errors via `errcheck` ([Q81](../STATUS.md)). |
| **Reachable CVEs** | Track — `govulncheck` + `trivy`, already gating ([backpressure.md](backpressure.md)). |
| **Open-item count / age** | Track lightly — the labeled Queue in [docs/STATUS.md](../STATUS.md) is the register; formal aging is overkill at this scale. |
| **Cyclomatic complexity** | Skip for now — most long functions are legitimate wiring; high noise-to-signal. |
| Technical-debt ratio, defect ratio, DORA velocity (lead time, change-failure rate), debt index | **Skip** — each needs an issue tracker, a remediation-cost estimator, or a delivery cadence this project does not have. Revisit if the project grows a team and a release pipeline. |

The principle: **a metric earns a place only when it changes a decision.** A
number nobody acts on is itself a small piece of debt.

## Quality gates as debt brakes

A quality gate turns "do not let this class of debt accumulate" into machine
enforcement. The release-1.0 plan groups them as
[bucket F — engineering quality gates](../plan/release-1.0.md): coverage,
duplication, `-race`, `gosec`, `errcheck`, and install-artifact validation, on
top of the formatting, lint, `govulncheck`, and `trivy` gates that already run
([backpressure.md](backpressure.md)). Each gate is the durable form of a
detect-and-pay-down cycle: once paid, it does not regress.

Where a gate is threshold-shaped (coverage, `dupl`), it gates by **not getting
worse** — a ratchet or tuned threshold — rather than an arbitrary absolute bar,
so it raises quality without manufacturing low-value work.

## Where it all lives

| Concern | Doc |
|---|---|
| Deciding fix / flag / defer / decline | this doc |
| Recording and prioritizing an item | [maintaining-backlog.md](maintaining-backlog.md) → [docs/STATUS.md](../STATUS.md) |
| The automated prevention loops | [backpressure.md](backpressure.md) |
| Release-blocking gates | [release-1.0.md](../plan/release-1.0.md) (bucket F) |
| Long-horizon non-commitments | [appendix-g-future-enhancements.md](../design/appendix-g-future-enhancements.md) |
| Choosing the right test tier | [testing.md](testing.md), [07-test-plan.md](../design/07-test-plan.md) |
