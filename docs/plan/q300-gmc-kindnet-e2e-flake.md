# Q300 — systemic kindnet `e2e / e2e` leg flakiness (cross-spec, control-plane starvation suspected)

**Status:** watching — the PR #612 kindnetd-unthrottle fix **did** close the
CFS-throttling mechanism (throttle counters are zero in every post-#612 failure
dump), and the 2026-07-18 "recurrence" that briefly escalated this row was a
**misattribution** (it was the Q349 flake on a commit that predated the Q349
fix — see the triage below). One genuinely unexplained dataplane event remains
(2026-07-15, `CrossTenantNetworkBlocked` fail-open), so the row sits in
[Flake watch](../STATUS.md#flake-watch) with the failure dump upgraded to
attribute the next occurrence from CI artifacts alone.

A second `CrossTenantNetworkBlocked` occurrence (2026-08-03, on a PR branch)
points at the same spec from the opposite direction — enforcement flapping
rather than leaking open — and turned up a test-harness defect that hides the
probe's own diagnostic. See
[2026-08-03](#2026-08-03-a-second-crosstenant-occurrence-and-what-its-phase-implies).

The third occurrence (2026-08-08, Q747) **is** attributed, from the upgraded
dump plus a local reproduction, and it is a fourth mechanism rather than any of
the three hypotheses below: kindnetd crash-looped on the node hosting every pod
in the spec, and kindnet's `queue flags bypass` rules accept every packet while
nothing is bound to the nfqueue. See
[2026-08-08](#2026-08-08-q747-the-enforcer-was-not-starved-it-was-absent).

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
Candidates, ordered by likelihood after the upstream source review below:

1. **nfqueue overflow → bypass accept → conntrack latch.** kindnetd runs
   kube-network-policies with **`FailOpen: true` hardcoded**
   ([kindnetd `main.go`, kind v0.31.0](https://github.com/kubernetes-sigs/kind/blob/v0.31.0/images/kindnetd/cmd/kindnetd/main.go)),
   which sets `QueueFlagBypass` on the nftables queue rules with
   `MaxQueueLen: 1024`: past 1024 in-flight verdicts the kernel **accepts**
   packets unverdicted. Critically, the agent's chain has
   `ct state established,related accept` *before* the queue rule, so one
   bypassed SYN makes the whole connection conntrack-established and
   permanently accepted — which is why 07-15 saw a complete HTTP
   request/response, not a lone stray packet. The
   `/proc/net/netfilter/nfnetlink_queue` `queue_dropped`/`user_dropped`
   counters in the dump are **cumulative since boot**, so overflow evidence
   survives even a late dump. (A packet the agent fails to parse is also
   accepted under FailOpen — "Can not process packet, applying default
   policy".)
2. **Stale IP→pod mapping on pod-IP reuse.** In v0.8.0's `evaluateIngress` an
   *unknown* source IP fails **closed** (it can't match a selector peer), so an
   informer race on the brand-new probe pod cannot produce an allow. But kind
   reuses pod IPs aggressively and the suite churns short-lived probe pods; if
   `getPodAssignedToIP` still maps the reused IP to a **deleted pod from an
   allowed namespace**, the verdict is a genuine spurious allow. Discriminator:
   no queue drops in the dump + full-window kindnet logs showing the stale pod
   attribution.
3. **Memory pressure in kindnetd.** #612 removed the CPU limit but kept
   `memory: 50Mi`. A GC-thrashing or reclaim-stalled enforcer delays verdicts,
   and with FailOpen a stalled queue overflows into accepts (folds into #1).
   `memory.events` (`high`/`max` counters) now captured.

The 07-15 dump could not discriminate these because the kindnet log capture
(`--tail=100`) started ~2 minutes **after** the failure window ended, and no
nfqueue/memory counters were collected. The failure dump in
[`e2e-reusable.yml`](../../.github/workflows/e2e-reusable.yml) now captures all
three signals.

## Upstream prior art (researched 2026-07-19)

kind v0.31.0's kindnetd bundles **kube-network-policies v0.8.0** (2025-04);
upstream is at **v1.1.0** (2026-06). Relevant upstream issues:

- [#171 NetworkPolicy enforcement delayed for newly created pods](https://github.com/kubernetes-sigs/kube-network-policies/issues/171)
  — short-lived pods could escape enforcement entirely; fixed May 2025,
  **after** v0.8.0 cut, so our lane carries it (egress-side mechanism — the
  new pod's IP missing from the `@podips` nftables set means its packets are
  never queued).
- [#283 same-node traffic always allowed](https://github.com/kubernetes-sigs/kube-network-policies/issues/283)
  (egress side, fixed 2026-04) and field report
  [#158](https://github.com/kubernetes-sigs/kube-network-policies/issues/158)
  — same-node allow exceptions exist by design (kubelet probes; the
  `meta skuid 0 accept` rule); worth remembering when a same-node placement
  coincides with a spurious allow.
- [#366 Scalability Improvements](https://github.com/kubernetes-sigs/kube-network-policies/issues/366)
  (open) — upstream acknowledges the userspace-verdict architecture struggles
  under high pod churn, which also retroactively validates the #612
  unthrottling: the verdict path is inherently CPU-hungry.

**Extra lever if the flake recurs:** upgrade kind (a newer bundled
kube-network-policies reduces both queue pressure and known enforcement
gaps) — alongside the runner-sizing lever (Q286).

**Lever pulled (Q353, 2026-07-19):** CI now pins kind v0.32.0 with the
`kindest/node:v1.35.5` image, whose kindnetd (`v20260528-9350166c`, a 2026-05
kube-network-policies snapshot) carries the #171 short-lived-pod and #283
same-node fixes above. Verified against the image itself: the bundled kindnet
DaemonSet still ships `limits: cpu=100m` on the `kindnet-cni` container, so
the #612 unthrottle patch (`tune_kindnet_limits` in
[`kind-with-registry.sh`](../../scripts/e2e/kind-with-registry.sh)) still applies
unchanged and is still needed.

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
([`scripts/e2e/kind-with-registry.sh`](../../scripts/e2e/kind-with-registry.sh),
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

## 2026-08-03: a second `CrossTenant` occurrence, and what its *phase* implies

[Run 30833071096](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30833071096),
on PR #1197's branch rather than `main`. `E2E_GMC_CrossTenantNetworkBlocked`
failed at
[`isolation_test.go:150`](../../cmd/gmc/test/e2e/isolation_test.go) — the gate
probe pod was **still `Running`** when the 6-minute `Eventually` expired
(`Timed out after 360.001s`, `probe pod still in phase "Running"`); 61 passed, 1
failed, 11 skipped. It passed on a re-run of the same tree with no change to
GMC, to any NetworkPolicy, or to the spec.

**The phase narrows the cause more than the timeout does.** The probe loops 150
times over `curl --max-time 5 --connect-timeout 5` plus `sleep 2`, exiting 0 on
3 consecutive blocks and 1 when the loop is exhausted. That gives three
predictions, and only one of them ends with the pod still `Running` at 360 s:

| Dataplane behaviour | Per-iteration cost | Where the pod is at 360 s |
|---|---|---|
| Every attempt connects (never enforced) | ~2 s (the sleep) | `Failed` at ~300 s — the loop exhausts *inside* the window |
| Enforcement lands and holds | ~7 s | `Succeeded` at ~21 s |
| Blocks **intermittently**, never 3 in a row | 2–7 s, mean > 2.4 s | still `Running` — the observed state |

So this run is not "enforcement was slow to program". A never-enforced probe
finishes and reports `Failed` with its own diagnostic; to still be looping at
six minutes, some attempts must have paid the 5 s connect timeout — meaning the
dataplane *was* dropping — while never sustaining three in a row. That is
enforcement **flapping**, which is the 2026-07-15 fail-open signal seen from the
other side: there enforcement was confirmed and then leaked, here it never held
long enough to be confirmed. Both are consistent with hypothesis 1 (nfqueue
bypass accepting under load).

**The assumption this rests on**, stated so it can be falsified: that a blocked
attempt costs the full `--connect-timeout`, i.e. the packets are dropped rather
than rejected. kindnetd's kube-network-policies issues nfqueue *drop* verdicts,
so this holds — but a REJECT anywhere in the path would return fast and collapse
the whole argument. **No failure dump was read for this run** (it is a
PR-branch run, and the upgraded dump is wired for `main`), so the nfqueue
counters that would confirm hypothesis 1 directly were not consulted. This is
recorded as an occurrence plus an inference from the observed phase, not as
proven attribution.

### The probe's loop budget does not span the outer window

Separately, and repairable without settling the dataplane question: the comment
at `isolation_test.go:95` says the 150-iteration budget is "sized to span the
outer `Eventually` window below". That is true only while attempts *connect*
(~2 s each → ~300 s, just under the 6-minute window). Once attempts block, each
iteration costs ~7 s and 150 of them run ~17 minutes — so in exactly the case
the budget was widened for (Q179, slow NP programming), the probe is still
looping when the window closes and the spec fails on the outer timeout instead
of on the probe's own verdict. The failure then reports a bare phase rather than
the probe's log, which is why this run needed arithmetic to attribute at all.
The window and the loop budget should be derived from one another, against the
blocked-iteration cost rather than the connected one.

## 2026-08-08 (Q747): the enforcer was not starved, it was absent

[Run 31272058691](https://github.com/actions-gateway/github-actions-gateway/actions/runs/31272058691)
on `main` at `9480f29b`. Third `CrossTenantNetworkBlocked` occurrence, same
shape as 2026-07-15: the gate pod observed enforcement on attempts 1, 2 and 3
(never once allowed), then the asserting curl pod completed with
`HTTP_CODE=400`. It passed unchanged on `7a531dff`.

This one is attributable, and the answer is **none of the three hypotheses
above**. It is a fourth mechanism, and the failure dump already carried it.

### Evidence, all from the run's own artifact

| Signal | Value |
|---|---|
| `kindnet-jmxsm` (node `actions-gateway-e2e-worker2`) | `RESTARTS 3 (5m28s ago)`; dump ran ≈18:45:01, so the last restart was ≈18:39:33 |
| kube-system events | `BackOff restarting failed container kindnet-cni in pod kindnet-jmxsm` at T-5m27s (≈18:39:33); `Created` T-5m3s (≈18:39:57); `Started` T-5m2s (≈18:39:58) |
| `cross-tenant-probe` (the asserting curl) | Start Time **18:39:40**, node `actions-gateway-e2e-worker2` |
| `cross-tenant-gate` | Start Time 18:38:54, same node, so it ran *before* the crash |
| nsB `actions-gateway-proxy-d756c4f8f-h7mhm` | 18:38:47, same node. All three pods co-located |
| kindnetd's current-container log on worker2 | first line **18:39:59** |
| `nfnetlink_queue` (all three nodes) | `queue_dropped 0`, `user_dropped 0` |
| `cpu.stat` (all three nodes) | `nr_periods 0`, so the #612 unthrottle is in effect; this is not CPU starvation |
| `memory.events` | `max 3733` (worker2), `max 609` (worker), with `memory.current` 47.4 MiB and 49.0 MiB against the `50Mi` limit |

The asserting curl ran **inside a ~25 s window in which no kindnetd process was
running on that node at all**. kindnetd hardcodes `FailOpen: true`, which puts
`queue flags bypass` on its nftables rules: with nothing bound to nfqueue 101
the kernel skips the queue rule and the chain's `policy accept` takes every
packet. NetworkPolicy was not being enforced on worker2, for any namespace, for
that window.

So the spec's observation was *correct*: the connection really was not blocked.
The cause is the lane's enforcer being dead, not the GMC's policy. The
policy objects were present and unchanged, and the same source namespace was
demonstrably blocked from reaching the same destination pod 40 s earlier.

`HTTP_CODE=400` also identifies the responder rather than merely implying it:
the proxy serves its CONNECT listener with `ServeTLS`
([`cmd/proxy/proxy.go`](../../cmd/proxy/proxy.go)), and Go's TLS server answers
a plaintext request with `400 Bad Request: Client sent an HTTP request to an
HTTPS server`. A plaintext listener would have returned **405** (the
non-CONNECT branch of the same file). The connection reached nsB's proxy.

### Hypotheses 1 and 2 are ruled out for this occurrence

Hypothesis 1 (nfqueue overflow) needs a *running* agent whose queue overflows;
hypothesis 2 (stale IP→pod mapping) needs a running agent to emit a verdict at
all. No agent was running.

**Correction to hypothesis 1's supporting claim.** It says the
`queue_dropped`/`user_dropped` counters are "cumulative since boot, so overflow
evidence survives even a late dump". They are not: they belong to the nfqueue
*instance*, which is destroyed when the agent unbinds and recreated when it
rebinds. Measured locally: after killing kindnetd, the same queue reappeared
with a new `peer_portid` and `id_sequence` reset to 7. With 3-4 restarts on
these nodes, the zeros in this dump describe only the window since 18:39:58.

### Reproduced deterministically

kind v0.32.0, `kindest/node:v1.35.5`, kindnetd `v20260528-9350166c`, two
workers, with a mirror of `buildProxyNetworkPolicy`'s ingress half (default-deny
ingress to `app=actions-gateway-proxy`, admitting only same-namespace
`actions-gateway/component: workload` on the proxy port):

| Enforcer | Cross-tenant connections |
|---|---|
| kindnetd running | **0 allowed of 38** consecutive attempts (each paying the full 3 s connect timeout) |
| `pkill -KILL kindnetd` on the node hosting both pods | **664 allowed in ~1.0 s** (07:13:13.76 → 07:13:14.76), ending when the container restarted |
| after the restart | blocked again |

Window forced open → red; closed → green. Forty rounds of NetworkPolicy churn
in unrelated namespaces against a live enforcer produced **zero** leaks, which
is the negative control: policy-set churn alone does not open the window.

### Fix

1. **`tune_kindnet_limits` now raises the memory limit too**
   ([`kind-with-registry.sh`](../../scripts/e2e/kind-with-registry.sh)),
   default `256Mi` via `KINDNET_MEMORY_LIMIT`. #612 deliberately left the
   `50Mi` limit alone on the grounds that "no OOM was ever observed"; this run
   refutes that reading. Note the honest limit of this step: the crash-loop is
   *measured*, and memory pressure at the ceiling is the leading cause of it,
   but the terminating reason itself was never captured, which is why (2)
   exists.
2. **The dump now captures why the enforcer died**, not just that it did:
   `lastState.terminated.{reason,exitCode,finishedAt}` per enforcer pod, plus
   previous-container logs, in
   [`e2e-reusable.yml`](../../.github/workflows/e2e-reusable.yml) and in
   `DumpCNIEnforcerState`
   ([`cmd/gmc/test/utils/cni_enforcer.go`](../../cmd/gmc/test/utils/cni_enforcer.go)).
   The next occurrence names its own cause.
3. **The spec discriminates instead of guessing.**
   [`isolation_test.go`](../../cmd/gmc/test/e2e/isolation_test.go) fingerprints
   the enforcer pods (name/restartCount/startedAt) around its probe. An allow
   observed across a changed fingerprint is discarded and re-measured; an allow
   observed with the fingerprint *unchanged* fails immediately and says in the
   message that no restart explains it. That is the distinction the row could
   not make: lane artifact versus isolation regression.

Deliberately not done: merging the gate and asserting pods into one pod to
shrink the 40 s scheduling gap between them. The fingerprint check already
makes that gap harmless, and folding two pods into one conflates their exit
codes.

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
