# Technical debt: policy and strategy

How this project classifies technical debt, decides what to do about each piece,
and keeps it from accumulating. This is the **policy** (the rules) and the
**strategy** (the lifecycle that enforces them). The mechanics live in adjacent
docs and are linked rather than repeated:

- [maintaining-backlog.md](maintaining-backlog.md) — how to record and prioritize a debt item in [docs/STATUS.md](../STATUS.md).
- [backpressure.md](backpressure.md): the automated feedback loops that stop debt at authoring time.
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

**Measure before you flag, not only before you fix.** A flag is a claim: it goes
into a Queue row or a PR description, and the next reader treats it as a finding
someone verified. "Measure before asserting a root cause" applies to the aside as
much as to the change in front of you — a plausible-by-analogy flag ("X probably
has the same shape as Y") is a hypothesis, and filing one costs someone else the
measurement you skipped. If measuring it now is out of scope, flag *the question*
rather than the answer: "does X have Y's failure mode? unmeasured" is honest and
still actionable, where a confident wrong flag is neither.

Worked example: Q492 flagged "`helm rollback` past a chart version that added a
values key fails the way `--reuse-values` does" in a PR description, reasoning
from shape alone. Measuring it disproved the claim — `helm rollback` replays the
target revision's stored manifest instead of re-rendering, so the failure cannot
arise — while turning up a *different*, real hazard the analogy had hidden
([Q492's rollback caveat](../operations/upgrade.md#rolling-back-past-this-change-re-arms-the-outage-it-fixes)).
The wrong flag had already merged into the record by then.

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

### Distinguish a fixable defect from an external structural ceiling

Not every repeatedly-failing symptom is debt you can pay down. Some failures are a
**structural ceiling in a system we do not control** — a server-side behaviour, a
protocol constraint, a third-party rate limit — where each fix on our side is motion
without progress. Misclassifying one as ordinary debt funds an open-ended loop of
fixes that cannot converge.

The signal to stop and re-triage: after the obvious on-our-side causes are fixed and
the symptom **still reproduces on a clean setup with those causes provably quiet**,
do not fund another fix round yet. Ask whether the residual is on our side of the
boundary at all. The cheap way to force the question is to build the minimal case
that isolates the external actor — a probe, a clean namespace — and read what *it*
does, rather than iterating on our code.

Worked example — the fan-out saga (Q260 → Q224 → Q264). Eight live re-routes fixed
AGC-side seam after seam (completion accounting, planID dedup, recycle churn,
slot-stranding, capacity) before a clean-namespace run with every seam provably quiet
*still* wedged: the residual was GitHub's server-side fan-out dispatch, structurally
unfixable on our side — the [Q224 lever spike](../plan/q224-fanout-dispatch-lever-spike.md)
confirmed no AGC-side lever exists. The lesson is not "fix faster"; it is to run the
*is-this-a-structural-ceiling?* check **earlier**, so the boundary is found before the
Nth fix round, not after. When the answer is "external," the disposition is usually
**defer + a watch trigger** (the external condition changing — tracked with the
[protocol dependency register](../design/github-protocol-dependencies.md) when the
ceiling is a GitHub protocol) or an architecture change that removes the dependency —
not another patch. This complements the standing rule that source-reading is unverified
until exercised end-to-end.

### A shell gate becomes a Go devtool on parsing density, not length

A long shell script is not debt. A shell script standing in for a parser is. The two
look alike from the outside, so the decision needs a test that reads the script rather
than its line count.

Ask what the script does with **text it did not produce**:

- **It parses a structured format into fields and reasons over them** (Markdown, a
  GitHub Flavored Markdown table, YAML, JSON), so it has to track nesting, escaping,
  and matched delimiters. Regular expressions cannot count brackets. Rewrite it in Go
  against a real parser.
- **It sequences external command-line interfaces** (`kubectl`, `helm`, `gcloud`,
  `docker`) and branches on their exit codes. Length here comes from the number of
  steps, not from the difficulty of any one of them. Shell is the right language, and
  Go would be `exec.Command` soup.

Two corroborating signals when the first read is ambiguous:

- **How does it fail?** A parser-dense gate fails *silently wrong*: a link never
  collected, a table cell read from the wrong index, and the gate stays green by not
  seeing. An orchestrator fails loudly, because the CLI it called exits non-zero.
- **What would a test look like?** A parse routine takes a string and returns fields,
  which is a table test. An orchestrator needs a live cluster, and moving it to Go buys
  no testability it did not already have.

Parsing structured text is necessary but not sufficient. A script that must reproduce
its input **byte for byte** is worse off with an abstract syntax tree, which discards
exactly the fidelity it depends on. [`git-merge-status.sh`](../../scripts/docs/git-merge-status.sh)
and [`merge-table-rows.awk`](../../scripts/lib/merge-table-rows.awk) parse Markdown
tables and stay in `awk` for that reason: a merge driver reconstructs the file line for
line, conflict-marker fallback included.

#### Worked examples

Line counts measured 2026-08-03 with `wc -l`. The [Q612 survey](../plan/markdown-gates-parser.md)
and the Queue row that asked for this section cited 235, 601, and 790; all three scripts
have drifted since.

| Script | Lines | Reads | Verdict |
|---|---|---|---|
| [`scripts/docs/check-doc-links.sh`](../../scripts/docs/check-doc-links.sh) | 252, now 76 | 178 of them were one `awk` program implementing a Markdown parser plus the github-slugger algorithm | **Rewritten** (Q612) — the checker is [`devtools/docs/doclinks`](../../devtools/docs/doclinks/) over a shared goldmark parse layer; the script keeps the file selection |
| [`scripts/docs/lint-backlog.sh`](../../scripts/docs/lint-backlog.sh) | 518, now 92 | Queue rows split on a literal `\|` field separator at fixed indices; one escaped pipe in a cell shifted every field and the row's rules then evaluated the wrong ones | **Rewritten** (Q613): the rules are [`devtools/docs/backloglint`](../../devtools/docs/backloglint/) over the same parse layer; the script keeps the file selection and the environment interface |
| [`scripts/dev/validate-egress-ip.sh`](../../scripts/dev/validate-egress-ip.sh) | 603 | Zero `awk`/`jq`/`sed` invocations. Field extraction is delegated to `kubectl -o jsonpath`; the body is `kubectl`, `helm`, `gcloud`, `curl`, and `docker` calls | **Stays shell** |
| [`scripts/dogfood/setup.sh`](../../scripts/dogfood/setup.sh) | 791 | One line invoking `awk`/`jq`/`sed`. The longest script under `scripts/`, and the least parsing of the four | **Stays shell** |
| [`scripts/agent/claude-piped-gate-hook.sh`](../../scripts/agent/claude-piped-gate-hook.sh) | 393, now 74 | 175 of its 257 code lines were a hand-rolled shell-grammar scanner: quote state, heredoc bodies, subshell nesting, matched delimiters | **Rewritten** (Q625): the decision is [`devtools/agent/pipedgate`](../../devtools/agent/pipedgate/) over `mvdan.cc/sh`; the script keeps the build seam and the registry path |

Sorted by length, the verdicts run backwards: the three shortest are the rewrites and the
two longest stay shell. That is why length is not the signal.

#### What the rewrite costs

Moving a gate to Go buys a parser that handles nesting and escaping correctly, rune
counting that does not vary with the host `awk`, and a test suite over functions with
inputs and outputs. It costs a module in the Go workspace, a vendored dependency to
sync and check ([`vendor-sync.sh`](../../scripts/go/vendor-sync.sh),
[`vendor-check.sh`](../../scripts/go/vendor-check.sh)), a compile before the gate runs,
and a continuous integration (CI) path filter that has to include the new tree.

Most of that is a one-time cost per module, not per gate: `devtools/` already exists
with its vendor tree and CI wiring, so the second and third gates onto the same parse
layer are cheap where the first was not. The gate keeps its `scripts/` entry point
either way, so the [script map](../../scripts/README.md) stays in one place.

None of this argues for rewriting shell in general. Almost every script under
`scripts/` sits on the orchestration side of the line and should stay there; the
criterion exists to find the few that do not.

## Strategy: the debt lifecycle

Policy decides what to do with one item. Strategy is the loop that keeps the
whole codebase from accumulating debt faster than we pay it down.

1. **Prevent.** The cheapest debt is the kind that never lands. Layered
   [backpressure](backpressure.md) — the pre-commit hook, `make check`, and CI —
   rejects whole classes of debt at authoring time. The guiding habit is
   **correct it twice, then automate it**: a mistake worth catching once is worth
   a gate (this is why `scripts/docs/lint-backlog.sh` and the [bucket-F gates](#quality-gates-as-debt-brakes) exist).
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
| **Function length** | Track — `funlen` as a ratcheted ceiling ([Q371](../STATUS.md)): the threshold starts just above the worst surviving function and lowers as long functions are decomposed, the same "gates by not getting worse" pattern the coverage ratchet uses. Cyclomatic complexity proper (`gocyclo`) stays skipped — length is the cheaper proxy, and Q367 showed the god `main`/`run` functions were the real target. |
| **Suppression hygiene** | Track — `nolintlint` (`allow-unused: false`, `require-specific: true`) ([Q371](../STATUS.md)): an inert or blanket `//nolint` directive fails the build, so a suppression cannot outlive the finding it documents (the class the dead `nolint:gocyclo` on the old `main()` was). |
| Technical-debt ratio, defect ratio, DORA velocity (lead time, change-failure rate), debt index | **Skip**: each needs an issue tracker, a remediation-cost estimator, or a delivery cadence this project does not have. Revisit if the project grows a team and a release pipeline. |

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

Where a gate is threshold-shaped (coverage, `dupl`, `funlen`), it gates by **not
getting worse** — a ratchet or tuned threshold — rather than an arbitrary
absolute bar, so it raises quality without manufacturing low-value work.

## Where it all lives

| Concern | Doc |
|---|---|
| Deciding fix / flag / defer / decline | this doc |
| Deciding whether a shell gate should become a Go devtool | this doc ([the criterion](#a-shell-gate-becomes-a-go-devtool-on-parsing-density-not-length)), pointed to from [bash-style.md](bash-style.md) |
| Recording and prioritizing an item | [maintaining-backlog.md](maintaining-backlog.md) → [docs/STATUS.md](../STATUS.md) |
| The automated prevention loops | [backpressure.md](backpressure.md) |
| Release-blocking gates | [release-1.0.md](../plan/release-1.0.md) (bucket F) |
| Long-horizon non-commitments | [appendix-g-future-enhancements.md](../design/appendix-g-future-enhancements.md) |
| Choosing the right test tier | [testing.md](testing.md), [07-test-plan.md](../design/07-test-plan.md) |
