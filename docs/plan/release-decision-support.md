# Release decision support: delta report + scope ledger (Q556)

Make the three release-timing questions answerable from data the repo already
records, instead of by narrative and judgment: *has enough been delivered to
justify a release?* — *what was planned vs what's delivered?* — *punt the
item or delay the tag?* Filed 2026-07-31 from a release-process design
discussion; this doc records the decisions so the implementing session
doesn't re-litigate them.

## Status

Not started. Tracked as [Q556](../STATUS.md#Q556).

## The gap

[operations/release.md](../operations/release.md) covers *how* to cut a
release (mechanics, verification, the RC dogfood gate) but says nothing about
*when*. The gate-label machinery answers "can we cut the scoped release?"
crisply, but two questions have no canonical answer today:

- **"Has enough accumulated to justify a tag?"** — the delete-on-done Queue
  erases delivered work from STATUS.md by design, so nothing shows the
  unreleased delta.
- **"What was planned vs delivered?"** — release plan docs narrate this in a
  prose status banner ([release-1.3.md](release-1.3.md)), faithful but not
  measurable at a glance.

Both are *derivable* because two disciplines are already enforced:
Conventional Commits, and verbed `docs(status): … complete QN` subjects.
Baseline measurement (2026-07-31): since `v1.2.0`, main had accumulated 332
commits — 31 `feat`, 65 `fix`, 10 `perf` — computed with two `git log`
one-liners. The data exists; only the view and the "enough" definition are
missing.

## The model: two complementary views

**Delta-out — "should a release exist?"** A report off the last non-RC tag.
This view is what *triggers* scoping a release plan doc.

**Scope-in — "is the scoped release done?"** The release plan doc + scope
ledger + `-gate` labels. Takes over from the moment the release doc is
written until the tag.

## Deliverables

### 1. `scripts/release-delta.sh`

Prints, for the range `<last non-RC tag>..origin/main` (tag overridable by
argument):

- commit counts by Conventional Commit type, breaking changes called out;
- completed Q-IDs, from `docs(status)` subjects' removal verbs;
- `api/` diffstat — the semver signal (breaking → major, feat → minor,
  fix-only → patch);
- `docs/operations/` pages touched — the operator-visible surface.

Bash-style compliant, shellcheck-gated, with a test per the `scripts-test`
convention. Read-only; no state, no recording step.

### 2. release.md "When to cut" section

Standing triggers, with the delta report as input:

- a security fix users can't get any other way → cut promptly;
- a headline capability lands → scope a minor around it (the 1.3 pattern);
- accumulated user-visible fixes with no feature → patch release;
- internal-only churn → wait.

Counterweight that makes "enough" a real bar: every tag costs the RC dogfood
validation, so releases are not free.

### 3. Scope-ledger convention in maintaining-backlog.md

A "Cutting a release" subsection: a release plan doc opens with a **scope
ledger** table — one row per planned item: Q-ID, item, gate-or-rides,
status (open / shipped / punted with a why-link). Delivered = ticked rows
(updated when the Queue row is deleted, riding the existing
plan-docs-stay-current discipline); cut condition = zero open `-gate` rows
(one grep) + the RC validation; **punt** = remove the label and move the item
to "Explicitly out of scope" (it keeps its Queue position — punting from a
release doesn't demote priority); **delay** = leave the label on.

Also extend the existing "Don't pre-assign release versions" rule with its
corollary: even once a release *is* scoped, non-gating "planned" items live
in the ledger, never as labels.

## Non-goals

- **No soft `X.Y-plan` labels.** A soft assignment is a guess nothing
  enforces: it goes stale silently, needs manual relabeling each slip, and
  blurs the `-gate` label's crisp semantics ("the tag waits for this"). The
  ledger carries "planned" because the release doc's lifecycle matches the
  release's — written at scoping, archived at tag.
- No new Queue states or STATUS.md format changes.
- No automation of the cut decision itself — the report informs judgment,
  the triggers frame it.

## Acceptance criteria

1. `scripts/release-delta.sh` ships with a test and produces the four report
   sections against the live repo.
2. release.md gains "When to cut" with the triggers and the RC-cost
   counterweight, referencing the script.
3. maintaining-backlog.md gains the scope-ledger convention and the
   no-soft-labels corollary.
4. The next release plan doc written (1.4 or later) opens with a scope
   ledger.
