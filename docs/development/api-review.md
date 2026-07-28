# Agent reference: Pre-release API review

Every release publishes API surface. Once a field, enum value, or default ships
in a tagged release, changing it costs a conversion shim and a deprecation
window; before it ships, changing it costs a rename. This review is the step
that spends the cheap window on purpose instead of letting the tag close it.

Run it in [release pre-flight](../operations/release.md#1-pre-flight), before
tagging any release — including a prerelease that will become a stable line.

## Why this exists

Q476 renamed `capacityGate.mode: On` to `Observe` days before 1.3.0 would have
published it. Nothing surfaced that question: it came up in an unrelated
conversation, and the value was one commit from being frozen for the life of
`v2beta1`. The review turns that from luck into a step.

The checklist below is not generic API advice — every item is a mistake this
project actually made and caught, or nearly did not.

## What counts as API

| In scope | Why |
|---|---|
| CRD spec/status fields, their types, defaults, and validation | The wire contract |
| Enum values | Removing one is breaking; adding one is not |
| Condition types and reasons (`api/apiconditions`) | Operators alert on them |
| Label and annotation keys the operator sets or reads | Selectors depend on them |
| Printer columns, short names, categories | `kubectl` muscle memory |

Out of scope here: metric names (see
[observability-metrics.md](../operations/observability-metrics.md)) and Go
symbols that do not appear on the wire — a bare `string` field promoted to a
named type serialises identically, so it is a Go-API break for `api` module
consumers only and does not need to beat a tag.

## Step 1 — enumerate what is new

```bash
scripts/api-surface-since.sh
```

It diffs the API packages and CRD manifests between the last tag and `HEAD` and
prints what changed. Everything it lists is surface this release publishes for
the first time; everything it does not list has already shipped and is governed
by the normal compatibility rules instead.

Pass an explicit ref to review a different span, e.g. `scripts/api-surface-since.sh v1.1.0`.

## Step 2 — ask these of each addition

**Does this enum answer exactly one question?**
An enum carrying two axes makes their cross-product unrepresentable and asks one
party to assert something they may not know. The tells are mechanical: values
that each activate a *different* sibling field, and CEL rules shaped like
`field X is only meaningful when mode == Y`. Q470 split `capacityGate.mode` for
exactly this reason; [Q481](https://github.com/actions-gateway/github-actions-gateway/blob/main/docs/STATUS.md)
asks the same of `sizing.profile`.

**Is the value named for what it does, or merely that it is on?**
`On` is fine as the second of two values and stops carrying information the
moment a third arrives. Name the *method*, so the axis stays legible as it
grows — that is why the gate's modes are `Off`/`Observe` with `Probe`/`Provision`
reserved, not `On` plus two strangers (Q476).

**Does the name collide with an established Kubernetes convention?**
`observe`/`audit`/`dry-run` mean *report-only, do not enforce* in Pod Security
Admission and Gatekeeper. `Observe` gates, so the docs say so explicitly in
four places. Either avoid the collision or neutralise it where an operator will
meet it — never leave it to inference.

**Whose fact is this?**
A tenant should not be asked to assert a property of infrastructure they do not
own. When a field's correct value is identical for every object in the cluster
and known to whoever runs the nodes, it belongs on the platform-owned object —
`clusterCapacity.nodeAutoscaling` on `ActionsGateway`, not on each `RunnerSet`
(Q470).

**Does the default fail safe?**
Not "is the default common" — *which way does a wrong answer fail?*
`nodeAutoscaling` defaults to `Present` because that direction can only
under-gate (today's behaviour), while `Absent` can starve a tenant.

**Will this shape still read correctly when the reserved values arrive?**
Check the planned-but-unbuilt work the field already anticipates. If a
deferred item extends the same axis, the shape has to accommodate it now — the
enum values that are *not yet accepted* are still a design constraint.

**Is it wire-breaking or only Go-breaking?**
Only wire-breaking changes must beat the tag. This is the question that keeps
the review's urgent pile small.

## Step 3 — record the outcome

Write the verdict into the release's plan doc under `docs/plan/` (its Definition
of Done section), naming what was reviewed and what was decided. **"Ship as-is,
deliberately" is a valid outcome** and the most common one — the point is that
the choice is made rather than defaulted into.

Anything deferred gets a Queue row with the release's gate label
(`1.3-gate` and friends) so it cannot be frozen by the tag while nobody is
looking. Allocate the ID with `make queue-id`; format per
[maintaining-backlog.md](maintaining-backlog.md).

## Scope discipline

This is a review of *newly published* surface, not a standing audit of the whole
API. Older fields have had real soak and operator contact that this release's
additions have not. Re-litigating them here makes the gate expensive, and an
expensive gate gets skipped.
