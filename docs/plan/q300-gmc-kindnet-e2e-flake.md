# Q300 — systemic kindnet `e2e / e2e` leg flakiness (cross-spec, control-plane starvation suspected)

**Status:** watching — the PR #612 kindnetd-unthrottle fix **did** close the
CFS-throttling mechanism (throttle counters are zero in every post-#612 failure
dump), and the 2026-07-18 "recurrence" that briefly escalated this row was a
**misattribution** (it was the Q349 flake on a commit that predated the Q349
fix — see the triage below). One genuinely unexplained dataplane event remains
(2026-07-15, `CrossTenantNetworkBlocked` fail-open), so the row sits in
[Flake watch](../STATUS.md#flake-watch) with the failure dump upgraded to
attribute the next occurrence from CI artifacts alone.

## Post-#612 soak triage (corrected 2026-07-19)

#612 merged 2026-07-13. Of ~26 `main` kindnet runs through 2026-07-19, three
were red (down from 3 of 5 pre-fix). All three dumps show kindnetd
`nr_throttled 0` on every node — the CPU-limit starvation mechanism did not
recur. Per-run attribution:

| Run | Spec(s) | Attribution |
|---|---|---|
| [29346467482](https://github.com/actions-gateway/github-actions-gateway/actions/runs/29346467482) (07-14) | `E2E_GMC_TenantProvisioning_ProxyConnectWorks` + `E2E_V2_ProxyConnectWorks` | Both the v1 proxy and the v2 EgressProxy returned `CONNECT tunnel failed, response 502` on every attempt — clients reached the proxies fine (NP dataplane healthy); the proxies could not dial **real GitHub**. A shared upstream cause on the hosted runner's external egress (network/DNS blip), not cluster-internal. Not Q300's mechanism; watch for recurrence before filing separately. |
| [29432174271](https://github.com/actions-gateway/github-actions-gateway/actions/runs/29432174271) (07-15) | `E2E_GMC_CrossTenantNetworkBlocked` | **The one genuine open signal.** The gate probe observed enforcement live (3 consecutive blocked attempts), then the fresh one-shot curl pod's connection was **accepted** (`HTTP_CODE=400` from the nsB proxy) — enforcement flapped open *after* being confirmed, with zero kindnetd throttling. See "Remaining hypotheses" below. |
| [29634174711](https://github.com/actions-gateway/github-actions-gateway/actions/runs/29634174711) (07-18) | `E2E_AGC_SkippedJobIsRedeliveredAfterCapacityFrees` | **Not Q300 — this was Q349, pre-fix.** The run's commit `0160e26` (a Dependabot merge) predates the Q349 fix (PR #692, merged later that day): deliveries stayed 0 through the 90 s window, the exact Q349 signature #692 removes by gating the spec on the HPA being scaling-active. This run drove the erroneous "#612 held <5 days" escalation. All subsequent `main` runs (6+ with #692 in) are green. |

## Remaining hypotheses (07-15 fail-open event)

The CrossTenant sequence — blocked, blocked, blocked, then a new pod's
connection accepted — is not *programming lag* (enforcement was already live).
Candidates, none yet discriminable from the 07-15 dump:

1. **nfqueue overflow → bypass.** kube-network-policies verdicts packets in
   userspace via nfqueue; the nftables queue rule carries the `bypass` flag, so
   when the queue is full the packet is **accepted** unverdicted. A burst
   (suite at `--procs 6`) could overflow the queue for exactly the window the
   one-shot pod connected in. `/proc/net/netfilter/nfnetlink_queue`
   `queue_dropped`/`user_dropped` counters now captured in the dump would prove
   or rule this out.
2. **Memory pressure in kindnetd.** #612 removed the CPU limit but kept
   `memory: 50Mi`. A GC-thrashing or reclaim-stalled enforcer delays verdicts
   (with bypass → accepts). `memory.events` (`high`/`max` counters) now
   captured.
3. **Per-node / traffic-path asymmetry.** The one-shot pod is a fresh pod with
   a fresh IP, potentially on a different node than the gate probe; verdict
   hooks differ for node-local vs cross-node traffic. Full-window kindnet logs
   (now captured via `--since`) plus the existing all-namespace `-o wide` pod
   listing cover this.

The 07-15 dump could not discriminate these because the kindnet log capture
(`--tail=100`) started ~2 minutes **after** the failure window ended, and no
nfqueue/memory counters were collected. The failure dump in
[`e2e-reusable.yml`](../../.github/workflows/e2e-reusable.yml) now captures all
three signals.

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

### Evidence from the red runs (verified in logs)

- **No OOMKills, no evictions, no disk pressure** in any failing run (disk at
  34% at dump time; all `NodeHasNoDiskPressure`). Memory/disk are not the
  starved resource; the GMC manager pods were Running/Ready, 0 restarts, the
  whole time — the "Killing"/"FailedMount webhook-server-cert" events are
  normal bring-up churn (helm install + two patches = 3 back-to-back rollouts
  before cert-manager issues the cert).
- Run 29216254327 (`CrossTenantNetworkBlocked`): the probe pod connected
  cross-tenant on **every** attempt for >5 min (re-run 29218283916: >15 min) —
  a freshly created NetworkPolicy was never programmed into the dataplane.
- Run 29218283916: the AGC's listener goroutines died on
  `Post http://fakegithub...:8080/token: context deadline exceeded` for ~2 min
  (7 listeners), and at the end of the run `kubectl apply` of ActionsGateways
  failed with webhook `context deadline exceeded` three times in a row — with
  both GMC pods Running/Ready.
- Attempt 1 of 29218365545 (`MultipleJobsQueued`): same signature — 8 listener
  goroutines died on token-refresh `context deadline exceeded`, one webhook
  deadline; the job was enqueued onto a live session but no worker pod appeared
  in 6 min. Attempt 2 passed with no code change (flake confirmed).
- So in the same window: traffic that **should be blocked flowed** (CrossTenant)
  while traffic that **should be allowed timed out** (token refresh, webhook,
  EgressProxy→GitHub).

### Root cause (mechanism)

On the kindnet lane, NetworkPolicy is enforced by **`kube-network-policies`
running inside the kindnetd DaemonSet** (verified in kindnetd logs:
`Starting controller name="kube-network-policies"`), and kind ships kindnetd
with **`limits: cpu=100m, memory=50Mi`** (verified on a live
`make e2e-cluster` cluster, kind v0.31.0). Three consequences:

1. `kube-network-policies` is nfqueue-based: the first packet(s) of every new
   connection involving a policied pod are verdicted in **userspace, inside
   that 100m-capped container**. The GMC chart ships NPs selecting the manager
   (`allow-webhook-traffic`, `allow-metrics-traffic`) and every tenant
   namespace carries per-tenant NPs — so apiserver→webhook, AGC→fakegithub,
   proxy→GitHub, and metrics scrapes ALL go through the throttled verdict path.
2. When the agent falls behind, new-connection verdicts stall → **allowed
   traffic times out** (webhook 10 s deadline, token refresh, curl connects);
   when its policy programming lags, **new NetworkPolicies are not enforced**
   (CrossTenant) — the two contradictory-looking symptom classes are the same
   starved component.
3. kindnetd is also an NRI plugin handling `RunPodSandbox` — a throttled
   kindnetd slows **pod creation** cluster-wide (worker pods appearing late).

Local measurement: kindnetd is CFS-throttled ~38% of periods **at cluster
bring-up idle** (nr_throttled 33/86, 5.2 s throttled); the e2e suite at
`--procs 6` multiplies connection churn while the 4-vCPU CI host multiplies
contention. The 100m limit is absolute, so throttling reproduces locally on a
larger host as well.

### Fix

Remove the kindnetd **CPU limit** at cluster bring-up
([`scripts/kind-with-registry.sh`](../../scripts/kind-with-registry.sh),
kindnet lane only) — the 100m CPU *request* keeps scheduling unchanged and the
memory limit stays (no OOM was ever observed). Idempotent strategic-merge
patch; re-running bring-up against an existing cluster is a no-op.

Plus diagnosability: the e2e workflow's failure dump now includes per-node
kindnetd `cpu.stat` (CFS throttle counters) and kindnet logs, so a recurrence
is attributable from CI artifacts alone.

Deliberately **not** done (single-concern scope):

- `--procs 6 → 4`: would reduce contention but costs wall-clock and papers
  over the real bottleneck; revisit only if the fix doesn't hold.
- GMC manager limits (500m/128Mi): never implicated — pods stayed
  Running/Ready with 0 restarts through every failure window.
- Webhook `timeoutSeconds` 10→30: mitigation, not fix; the 10 s deadline only
  fired because verdicts stalled.

### Local validation

Full suite (`--procs 6`, 3-node kindnet cluster, 8-vCPU Docker VM), kindnetd
CFS throttle deltas sampled every 10 s across the whole run:

| Config | Result | kindnetd throttling (worst node) |
|---|---|---|
| baseline (100m limit) | 51/51 passed | **15.2 % of periods throttled; 16.5 s throttled vs 3.6 s used (4.6× more time throttled than running)** |
| CPU limit removed (fresh cluster, fix applied by bring-up) | 51/51 passed | **0 throttled periods, 0 ms throttled** (~5–6 s CPU used per node — the enforcer wanted that CPU all along; with the limit it waited 4.6× longer than it ran) |

A local pass is expected either way (8 local vCPUs vs CI's 4 — CI adds the
host contention that tips budgets over); the throttle counters are the
mechanism measurement.

Side-finding from a back-to-back local re-run on the same cluster (an invalid
comparison run, discarded): the suite's AfterSuite `make undeploy` removes the
GMC/AGC while tenant namespaces from failed/late specs are still Terminating,
stranding `actions-gateway.com/agentpool-cleanup` finalizers with no
controller left to clear them — the namespaces never finish deleting and the
next run's BeforeAll `create namespace` collides. CI is unaffected (fresh
cluster per run); filed as its own Queue item for the local iteration loop.

**Resolved (Q301):** the AfterSuite now drains tenant namespaces before
`make undeploy`. `drainTenantNamespaces` (in
[`cmd/gmc/test/e2e/e2e_suite_test.go`](../../cmd/gmc/test/e2e/e2e_suite_test.go))
deletes every namespace carrying the `actions-gateway.github.com/tenant` marker
and waits (bounded, best-effort) for them to finish terminating while the
GMC/AGC controllers are still up, so the finalizers clear before the controllers
are torn down. A back-to-back local re-run's BeforeAll no longer collides on a
pre-existing Terminating namespace.

## Recurrence guard

Same rule as Q291: this reproduces only under CI load, so **one green run
proves nothing**. The row sits in Flake watch; on the next red kindnet run on
`main`, attribute from the upgraded failure dump **before** touching code:

- Check the failing spec against the triage table above first — `ProxyConnectWorks`
  502s to real GitHub are the external-egress class, not the dataplane class.
- For a dataplane-class failure, read the dump's nfqueue counters, kindnetd
  `memory.events`, and the full-window kindnet logs against the three
  hypotheses above; fix whichever is proven.
- If the dump implicates none of them, the next lever is runner sizing (Q286's
  dedicated pool), not more retry-budget widening.

## Not in scope

- Q291 (calico-only Felix ipBlock programming window) — separate mechanism,
  separate lane.
- Q299 (manager-metrics curl pod, PR #608) — already fixed; do not touch.
- Fan-out delivery starvation (Q264 residual) — GitHub-side, unrelated to CI
  host resources.
