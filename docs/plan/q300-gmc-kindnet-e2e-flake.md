# Q300 — systemic kindnet `e2e / e2e` leg flakiness (cross-spec, control-plane starvation suspected)

**Status:** open — investigation in progress.

## Symptom

The GMC kindnet e2e leg (job `e2e / e2e` in
[`.github/workflows/e2e-test.yml`](../../.github/workflows/e2e-test.yml), suite
at [`cmd/gmc/test/e2e/`](../../cmd/gmc/test/e2e/)) fails on **different,
unrelated specs from run to run** — unlike Q291 (calico-only, one fixed spec
set) or Q299 (one metrics spec). 3 of the last 5 `e2e-test.yml` runs on `main`
are red, including the most recent merged commit at the time of filing
(2026-07-13).

Distinct failure modes observed across two PR #608 runs (whose own Q299 metrics
spec passed every time):

| Run | Spec / site | Failure |
|---|---|---|
| 29217433954 | [v2_multigateway_test.go:208](../../cmd/gmc/test/e2e/v2_multigateway_test.go) | curl through the v2 EgressProxy never reached GitHub — `curl: (28) Connection timed out after 30002 ms`, `HTTP_CODE=000` |
| 29217433954 | [resources.go:122/:214](../../cmd/gmc/test/utils/resources.go) BeforeAll (tenant-resilience, tenant-pod-clean) | `kubectl apply` of an ActionsGateway → `failed calling webhook "vactionsgateway-v1alpha1.kb.io": Post "https://webhook-service.gmc-system.svc:443/validate-...": context deadline exceeded` |
| 29218365545 | [job_lifecycle_test.go:191](../../cmd/gmc/test/e2e/job_lifecycle_test.go) | `Eventually` 360 s timeout: "expected >= 1 new worker pods, have 0" — a job was never acquired/delivered, so no worker pod was ever provisioned |

Corroborating cluster-health signals in the same logs: HPA `shared-proxy`
"no metrics returned from resource metrics API" (metrics-server not serving),
and a worker pod in `phase: Failed`.

## Hypothesis

The GMC controller-manager (and/or the per-tenant AGC) becomes
**resource-starved or unresponsive partway through the run**: the validating
webhook times out, job delivery stalls (fakegithub long-poll windows are
~35–50 s, so a starved AGC misses whole poll cycles), and egress proxying to
GitHub times out. The immediately preceding `main` commit was an ENOSPC
firefight ("free hosted-runner disk up front"), so hosted-runner resource
pressure is an active, adjacent problem.

Candidate root causes, in investigation order:

1. **Runner disk/CPU/memory pressure** — manager pod or kind node under
   pressure, OOMKills, evictions during the run (check node/pod events in the
   failure dumps).
2. **GMC webhook server capacity** — single replica; readiness vs. actual
   serving; webhook `timeoutSeconds` too tight when the manager is
   CPU-throttled.
3. **metrics-server / kube-proxy / CNI readiness racing the specs** (the HPA
   "no metrics returned" error).
4. **Spec-side gating** — specs may need readiness gates or bounded
   per-attempt timeouts (the Q299 pattern) so one hung connect doesn't burn the
   whole retry budget.

## Investigation plan

1. Pull the failure dumps/artifacts from the red runs (29217433954,
   29218365545, and the red `main` runs) — node conditions, pod events,
   OOMKills/evictions, kubelet pressure, manager pod restarts, CPU throttling
   if captured.
2. Check the GMC manager deployment: replicas, requests/limits, webhook
   `timeoutSeconds`, whether admission has a CFS quota that can throttle it.
3. Reproduce locally against a kindnet kind cluster
   ([kind-iteration.md](../development/kind-iteration.md)) — actually run the
   affected specs; if it only manifests under CI load, constrain local
   resources to simulate.
4. Decide where the fix belongs: e2e harness (requests/limits, readiness
   gates, bounded per-attempt timeouts) vs. workflow (runner sizing, disk),
   and whether one fix covers all three failure modes or they split.

## Findings

*(none verified yet — this section is filled in as the investigation lands;
per CLAUDE.md, findings are unverified until confirmed end-to-end)*

## Recurrence guard

Same rule as Q291: this reproduces only under CI load, so **one green run
proves nothing**. Keep the row open until the kindnet leg soaks clean on
`main` across several consecutive runs. If starvation is confirmed and a fix
still doesn't hold, the next lever is runner sizing (Q286's dedicated pool),
not more retry-budget widening.

## Not in scope

- Q291 (calico-only Felix ipBlock programming window) — separate mechanism,
  separate lane.
- Q299 (manager-metrics curl pod, PR #608) — already fixed; do not touch.
- Fan-out delivery starvation (Q264 residual) — GitHub-side, unrelated to CI
  host resources.
