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
| Q471 | `validate-cluster-test` flakes under parallel `make check` load | #963 | 2026-08-18 | Soaked: 411 green `unit-test` runs since 2026-07-29 (bar ≥25, infra). |
| Q490 | A fan-out completion spec cancels a job every delivery completed | #965 | 2026-08-18 | Soaked: 411 green `unit-test` runs since 2026-07-29 (bar ≥50, correctness-guarding). |
| Q498 | Provisioner eviction/rerun tests flake under parallel load | #1010 | 2026-08-18 | Soaked: 381 green `unit-test` runs since 2026-07-31 (bar ≥50, correctness-guarding). |
| Q516 | Stale sibling-worktree lint cache fails `make check` cross-session | #1014 | 2026-08-18 | Soaked: 381 green `unit-test` runs since 2026-07-31 (bar ≥25, infra). |
| Q548 | Released-upgrade check dies on tar EPIPE reading the chart's values.yaml | #1044 | 2026-08-18 | Soaked: 381 green `unit-test` runs since 2026-07-31 (bar ≥25, infra). |
| Q559 | `TestV2_RunnerSet_CapacityGate_FixedClusterSkipsAcquire` acquires anyway | #1116 | 2026-08-18 | Soaked: 223 green `integration-test` runs since 2026-08-01 (bar ≥50, correctness-guarding). |
| Q570 | Migration e2e times out when the v1 proxy pool takes both workers | #1112 | 2026-08-18 | Soaked: 215 green `e2e-test` runs since 2026-08-01 (bar ≥50, correctness-guarding). |
| Q604 | `stallJob` installs its runner-name conflict after the job is pollable | #1143 | 2026-08-18 | Soaked: 342 green `unit-test` runs since 2026-08-01 (bar ≥50, correctness-guarding). |
| Q596 | `check-v2-api-sync-test`'s `tree-in-sync` fails under the fan-out | #1146 | 2026-08-18 | Soaked: 286 green `unit-test` runs since 2026-08-02 (bar ≥25, infra). |
| Q600 | `TestMultiplexer_DuplicateJobDeliveryProvisionsOnce` reads too early | #1142 | 2026-08-18 | Soaked: 286 green `unit-test` runs since 2026-08-02 (bar ≥50, correctness-guarding). |
| Q602 | `TestListener_AbandonedJobDoesNotSurviveARestart` stops before delete | #1143 | 2026-08-18 | Soaked: 286 green `unit-test` runs since 2026-08-02 (bar ≥50, correctness-guarding). |
| Q642 | `progress-watch-test`'s `interval 0` case fails under the fan-out | #1222 | 2026-08-18 | Soaked: 227 green `unit-test` runs since 2026-08-04 (bar ≥25, infra). |
| Q648 | The runner→GitHub egress probe scores an HTTP 403 as reachable | #1217 | 2026-08-18 | Soaked: 148 green `e2e-test` runs since 2026-08-04 (bar ≥50, correctness-guarding). |
| Q664 | `E2E_AGC_StuckPendingPodReaped` times out, the workers never reaped | #1244 | 2026-08-18 | Soaked: 148 green `e2e-test` runs since 2026-08-04 (bar ≥50, correctness-guarding). |
| Q685 | `TestListener_RestartDoesNotReprovisionAConcludedJob` saw a re-provision | #1296 | 2026-08-18 | Soaked: 180 green `unit-test` runs since 2026-08-06 (bar ≥50, correctness-guarding). |
| Q690 | `release-sentinel-test`'s live-run case fails under the fan-out | #1310 | 2026-08-18 | Soaked: 180 green `unit-test` runs since 2026-08-06 (bar ≥25, infra). |
| Q703 | `claude-go-throttle-hook-test` reads an empty decision under the fan-out | #1384 | 2026-08-18 | Soaked: 122 green `unit-test` runs since 2026-08-11 (bar ≥25, infra). |
| Q741 | GMC envtest suite exceeds its 5m timeout | #1387 | 2026-08-18 | Soaked: 88 green `integration-test` runs since 2026-08-11 (bar ≥25, infra). |
| Q752 | `record-launch-test`'s 10s launch bound fails under the fan-out | #1389 | 2026-08-18 | Soaked: 122 green `unit-test` runs since 2026-08-11 (bar ≥25, infra). |
| Q761 | `TestAbandonedProbe_Redispatched` bounds a verdict on 20s of wall clock | #1389 | 2026-08-18 | Soaked: 122 green `unit-test` runs since 2026-08-11 (bar ≥50, correctness-guarding). |
| Q789 | `alloc-queue-id-test`'s `gh` stub reads a ref-lock collision as broken | #1389 | 2026-08-18 | Soaked: 122 green `unit-test` runs since 2026-08-11 (bar ≥25, infra). |
| Q806 | A transient syft download fails the SBOM step in scan and publish | #1428 | 2026-08-18 | Soaked: 73 green `security-scan` runs since 2026-08-12 (bar ≥25, infra). |
| Q809 | A drained worker's recovery is detected, then lost at the claim | #1441 | 2026-08-18 | Soaked: 75 green `e2e-test` runs since 2026-08-12 (bar ≥50, correctness-guarding). |
| Q803 | `cmd/proxy` coverage is nondeterministic and its floor has no headroom | #1433 | 2026-08-18 | Soaked: 81 green `unit-test` runs since 2026-08-13 (bar ≥50, correctness-guarding). |
| Q829 | A pinned download outlives `download-verified.sh`'s retry budget | #1482 | 2026-08-18 | Soaked: 81 green `unit-test` runs since 2026-08-13 (bar ≥25, infra). |
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
