# Retired flakes

Cold storage for flake-watch rows that have graduated out of the live [flake watch](../queue/README.md) table.
A row lands here when its recurrence-memory has decayed to ~zero — see the retirement bar in [maintaining-backlog.md](maintaining-backlog.md#retiring-a-flake-watch-row) (**soaked** — the spec's blast-radius run threshold of green `main` runs since the fix, **or** the test/code path is **obsolete**).

This ledger exists so retirement is not deletion: it keeps the "a fix was already attempted here" memory `grep`-able at zero live-table cost.
If a retired flake ever recurs, it re-enters the Queue as a fresh find (flakes-first); this row is the pointer back to the original diagnosis, so re-add it to the escalation history rather than starting cold.

Run counts below are green `main` runs of the covering workflow since the fix merged, measured with the `gh run list` recipe in [maintaining-backlog.md](maintaining-backlog.md#retiring-a-flake-watch-row).
Each is anchored on a date at or after the fix, so every count is a conservative undercount.

A **refuted** row lands here too, with no fix PR and no soak: the flake it named was never observed, and the record of having looked is worth the same `grep`.
A refuted row that was [repurposed rather than retired](maintaining-backlog.md#repurposing-an-id-is-a-closure-with-every-step-skipped) has no ID left to carry, since another defect holds it.

Newest retirement first.

| ID | Item | Fix PR | Retired | Why retired |
|---|---|---|---|---|
| none | e2e-calico NetworkPolicy enforcement negatives intermittently see traffic that should be blocked | none | 2026-08-14 | Refuted: the three `e2e-calico` failures of 2026-08-12 it was filed on were all `E2E_AGC_ScaleSetDrainedWorkerClaimAndRerunLandUnderChartRBAC`, one spec per run ([the measurement](../plan/q549-scaleset-rerun-flake.md#mode-b-attributed-2026-08-12-the-claim-was-made-and-lost)); the five enforcement negatives never failed. Filed as Q809 on 2026-08-11, then [repurposed in place](maintaining-backlog.md#repurposing-an-id-is-a-closure-with-every-step-skipped) by #1441, so that ID now names the scale-set defect. Its enforcer-dump half was real and shipped in #1417. |
| Q350 | scalesetlistener metrics assertions race the provisioner stub | #948 | 2026-08-01 | Soaked: ≥136 green `unit-test` runs since 2026-07-28 (bar ≥50, correctness-guarding). |
| Q460 | `trivy` shards flake on the Docker Hub buildkit pull | #906 | 2026-08-01 | Soaked: 107 green `security-scan` runs since 2026-07-27 (bar ≥25, infra). |
| Q461 | `gag-migrate --apply` aborts the fan-out on an unreachable webhook | #904 | 2026-08-01 | Soaked: 94 green `e2e-test` runs since 2026-07-27 (bar ≥25, infra). |
| Q455 | Stack through `reconcileDelete` under host contention | #890 | 2026-08-01 | Soaked: 136 green `unit-test` runs since 2026-07-27 (bar ≥25, infra — log noise, not an assertion). |
| Q391 | e2e: GMC webhook unreachable in a `BeforeAll` apply | #889 | 2026-08-01 | Soaked: 94 green `e2e-test` runs since 2026-07-27 (bar ≥25, infra). |
| Q451 | `TestV2_RunnerSet_SizingRecommendationAndDrift` 409 on the reconcile poke | #876 | 2026-08-01 | Soaked: 107 green `integration-test` runs since 2026-07-27 (bar ≥50, correctness-guarding). |
| Q445 | `TestRunScaleSet_ForwardsSIGTERMToRunSh` kills the test binary | #872 | 2026-08-01 | Soaked: ≥136 green `unit-test` runs since 2026-07-27 (bar ≥50, correctness-guarding). |
| Q431 | PodTemplateEdit integration test races the reconciler | #838 | 2026-08-01 | Soaked: ≥107 green `integration-test` runs since 2026-07-27 (bar ≥50, correctness-guarding). |
| Q436 | A failed `DeleteSession` strands the job queued on that session | #837 | 2026-08-01 | Soaked: ≥136 green `unit-test` runs since 2026-07-27 (bar ≥50, correctness-guarding). |
| Q433 | CI pinned-binary downloads never retry a 403 | #834 | 2026-08-01 | Soaked: ≥136 green `unit-test` runs since 2026-07-27 (bar ≥25, infra). |
| Q378 | `TestReconcile_ReaperDefaults` baseline-recheck race | #738 | 2026-08-01 | Soaked: 241 green `unit-test` runs since 2026-07-21 (bar ≥50, correctness-guarding). |
| Q299 | manager-metrics curl pod flake (kindnet) | #608 | 2026-08-01 | Soaked: 229 green `e2e-test` runs since 2026-07-13 (bar ≥25, infra). |
| Q292 | e2e hosted-runner disk exhaustion during bring-up | #597 | 2026-08-01 | Soaked: 234 green `e2e-test` runs since 2026-07-12 (bar ≥25, infra). |
| Q291 | e2e-calico egress-to-GitHub reachability flake | #593 | 2026-08-01 | Soaked: 144 green `e2e-calico` runs since 2026-07-11 (bar ≥25, infra). Recurred twice pre-fix, never since. |
| Q256 | e2e-calico infra bring-up (registry + Calico node) | #590 | 2026-08-01 | Soaked: 144 green `e2e-calico` runs since 2026-07-11 (bar ≥25, infra). |
| Q285 | `TestListener_AssignedCountReconciliation` | #580 | 2026-08-01 | Soaked: ≥300 green `unit-test` runs since 2026-07-08 (bar ≥50, correctness-guarding). |
| Q221 | metrics-NP AllowsLabeledNamespace (calico) | #412 | 2026-08-01 | Soaked: 182 green `e2e-calico` runs since 2026-06-27 (bar ≥50, correctness-guarding). |
| Q179 | two kindnet v1 e2e timing races | #370 | 2026-08-01 | Soaked: 369 green `e2e-test` runs since 2026-06-23 (bar ≥50, correctness-guarding). |
