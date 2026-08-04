# Doc-update matrix

Which docs to update for each kind of change. Use this after making changes, before committing — update docs proactively rather than waiting to be asked.

The `docs/` tree has two audiences: `docs/design/` explains how the system works (for contributors); `docs/operations/` explains what an operator does and sees (onboarding, runbooks, upgrades). **Updating the design docs is not sufficient** — if a change alters what an operator does, configures, or observes, the operator-facing docs must be updated too.

| Change | Docs to update |
|---|---|
| New or changed CRD fields / API surface | **`make api-reference`** — a `v2beta1` doc comment or validation marker is published verbatim at [reference/api.md](../reference/api.md), so write it for an operator reading the site, not only for a contributor reading the type ([code-generation.md](code-generation.md#the-generated-api-reference); `make check` fails on a stale page). Then [design/03-api-contracts.md](../design/03-api-contracts.md) — add the field with its comment block; [design/02-architecture.md](../design/02-architecture.md) — update prose, and the metrics table if new metrics were added. Apply the [API design rules](api-review.md#design-rules) *before* writing the field, and check the change against [Is this change breaking?](api-review.md#is-this-change-breaking) if it touches shipped surface. **If the chart renders an instance of the CRD** (today: `PriorityClassAllowlist`), the chart template must [omit the new field when empty](api-review.md#if-the-chart-renders-an-instance-of-the-crd-omit-the-new-field-when-empty) — Helm never reapplies `crds/` on upgrade, so an unconditional render breaks `helm upgrade` midway. |
| New behaviour, retry logic, or operational mode | [design/02-architecture.md](../design/02-architecture.md) (architecture prose); [design/04-operational-flows.md](../design/04-operational-flows.md) (flow diagrams/prose); [design/07-test-plan.md](../design/07-test-plan.md) (integration test criteria); [operations/troubleshooting.md](../operations/troubleshooting.md) (a runbook section for any new failure mode an operator might observe). |
| New or changed admission/validation rule (CRD CEL, OpenAPI bounds, validating webhook) an operator could trip | [operations/troubleshooting.md](../operations/troubleshooting.md) — a runbook for the rejection: the exact admission error as the symptom, the cause, the remediation — **and** the usage doc for the action that now gets rejected: [operations/tenant-onboarding.md](../operations/tenant-onboarding.md) for create-time/day-2 config, [operations/upgrade.md](../operations/upgrade.md) for upgrade-time edits. A design-doc-only update is the classic miss: the operator who hits the rejection never reads `docs/design/`. |
| Operator-visible behaviour, default, or workflow change (a changed default, a new required/optional field an operator sets, a new failure mode, an annotation/label they must apply, an observable metric/condition) | The relevant `docs/operations/` usage docs: [tenant-onboarding.md](../operations/tenant-onboarding.md), [runbook.md](../operations/runbook.md), [upgrade.md](../operations/upgrade.md), [observability.md](../operations/observability.md), [security-operations.md](../operations/security-operations.md). |
| New, renamed, or re-labelled **metric family** | The docs above, **and the shipped monitoring artifacts under [`deploy/monitoring/`](../../deploy/monitoring/)** — these are appliable/importable files, not prose, so updating only the docs ships a half-fix: [`prometheusrule.yaml`](../../deploy/monitoring/prometheusrule.yaml) (the alert rules `observability-alerting.md` reproduces), [`grafana-dashboard-tenant.json`](../../deploy/monitoring/grafana-dashboard-tenant.json) and [`grafana-dashboard-platform.json`](../../deploy/monitoring/grafana-dashboard-platform.json) (the panels `observability-dashboards.md` tabulates), and [`preview/exporter.py`](../../deploy/monitoring/preview/exporter.py) so the preview harness populates the new series. Re-render the screenshots per [`preview/README.md`](../../deploy/monitoring/preview/README.md) when a dashboard changes. Q319 updated every doc and the `PrometheusRule` but missed the dashboard JSON, which cost a follow-up PR. |
| Security-relevant change | [design/05-security.md](../design/05-security.md), plus the operator-facing docs above when the control is something an operator configures or can trip. |
| New or changed **user-facing capability** (a marketable feature, a new tenant/operator capability, a competitive differentiator) | The website positioning pages: [features.md](../features.md), [why-gag.md](../why-gag.md), [index.md](../index.md), **and [roadmap.md](../roadmap.md)** — keep the capability index, the value proposition, the competitive placement vs ARC, and the roadmap buckets current with what actually ships. A shipped capability **leaves the roadmap entirely and gains a `features.md` line**: one bullet, under 45 words, ending in a link to the doc that explains it — a capability with no doc to link is a docs gap to file, not a longer bullet. State maturity honestly with the `gag-maturity-badge` pill (`alpha`/`beta`) for a not-yet-GA item, distinct from the `gag-v2-badge` API-surface pill. A **roadmap** bullet says what is missing, what would change, and the gate it waits on — under 60 words, with the approach itself in the linked plan doc or Appendix G section. **This is gated:** every bullet under "In progress / near-term" and "Exploring / longer-term" carries an invisible `<!-- q:QN -->` annotation naming the backlog rows behind it, and `make roadmap-check` fails when a named row no longer exists (it shipped), has moved between the Queue and Deferred, or when a bullet on either page loses its link or runs long (see [`scripts/docs/check-roadmap.sh`](../../scripts/docs/check-roadmap.sh)). Pair with the operator usage docs above. Internal-only changes (refactors, types/codegen, tooling) need no website update. |
| New, removed, or re-pointed inter-module dependency (an added/removed `replace` edge between workspace modules, or a new/deleted module in `go.work`) | [go-workspaces.md](go-workspaces.md) — update the module table's **Internal deps** column and the **Dependency direction** graph. The tidy script derives the order at runtime so it won't drift, but the human-readable table will. |
| New or changed pinned tool/image version (a CI workflow version env var, an `updatecli.d/*.yaml` manifest, a `.github/dependabot.yml` ecosystem) | [dependency-updates.md](dependency-updates.md) — update the "What updates each surface" table and, if it's a new manual pin, the planned-fan-out list. |
| New or moved shell script under `scripts/` | Put it in the `scripts/<group>/` directory named for the gate that runs it — never at the top level — and add a row to that group's table in [`scripts/README.md`](../../scripts/README.md). A `*-test.sh` goes beside its subject; a helper more than one gate calls goes in `fetch/` or `lib/`, and **every** consuming filter lists that group. Only if the script introduces a gate a filter must newly see does `.github/workflows/` change: the groups are prefix globs, so a script added to an existing group is already covered. Rationale: [testing.md § `scripts/` is grouped by blast radius](testing.md#scripts-is-grouped-by-blast-radius). |
| Anything general | `README.md`, `CONTRIBUTING.md`, and any other `docs/` file that describes the changed behaviour. Also `.github/workflows/` if the change affects how tests are run, what modules exist, or what build inputs CI depends on (e.g. `go-version-file`, test commands, module paths). |

## Name the acquisition tier when a capability is not universal

Applies across every row above. **Before writing that the gateway does X, check
which acquisition tier actually does X** — and if it is not both, say so in the
same sentence. Prose that reads as a property of the system is the failure mode;
it is not repaired by being true of the tier the author had in mind.

This has bitten three times (Q419 eviction recovery, Q439 the pre-claim quota
gate, Q446 poll-error metrics), and once in a change whose whole purpose was to
polish the same paragraphs. The claims are not wrong so much as unqualified, so
review does not catch them — only checking the code does.

How to check: the tier seams are the `ScaleSet` early return in
`runnerset_controller.go` (everything after it is classic-only by construction),
`provision()` versus `ProvisionScaleSetWorker`, and the `listener/` versus
`scalesetlistener/` packages.

Two places it costs the most:

- **Metrics tables.** A counter emitted from one tier reads a flat zero on the
  other, and zero looks like "nothing happened" rather than "nothing is
  watching". Say which tier emits it, and name the signal that substitutes.
- **Positioning copy** (`README.md`, `why-gag.md`, `features.md`, `roadmap.md`). A capability
  claimed for the system but implemented only on the deprecated tier is
  scheduled for deletion, not shipped — it belongs in the parity table in
  [v2-ga.md](../plan/v2-ga.md#capability-parity-is-a-precondition-of-the-removal),
  which gates the `v2.0.0` removal on closing exactly this gap.

This convention retires with the classic tier at `v2.0.0`; until then the parity
table is the tracking mechanism, and anything found classic-only joins it.
