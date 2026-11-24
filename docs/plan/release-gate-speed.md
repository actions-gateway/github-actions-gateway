# Release gate speed

> **Status:** 🚧 Investigation done 2026-11-18; the first five options (Q1363, Q1362, Q1365, Q1364, Q1366) are implemented, the other two are not.
> Two rows are open, ranked in the order below: [Q1367](../queue/Q1367.md) and [Q1368](../queue/Q1368.md).

The release-candidate gate (`.github/workflows/validate-candidate.yml` running `scripts/dogfood/validate-release.sh`) took 115.5 minutes for `v2.0.1-rc.1` and 133.1 minutes for `v2.0.0-rc.3`, counted from the start of its gate step to its verdict.
About a fifth of that, 21.7 and 25.3 minutes, is the gate waiting on a worker pod that finished long before.
That wait is a GMC teardown defect, not a cost of the proof, and removing it is the first change worth making.
Six smaller changes follow, ranked by saving, and none of them raises GKE node-hours or GitHub Actions minutes.

The goal is a shorter gate with no rise in either cost.
The gate's proof is out of scope for cuts: every proposal below keeps each leg running the same suite on the same template and back end.

## How it was measured

Two gate runs were read, nothing was run, and the dogfood cluster was not touched.

- `v2.0.1-rc.1`: gate run 44236535751, publish run 44236526788, and e2e runs 44238725228 (Kata), 44243477731 (DinD) and 44247655794 (Dragonfly).
- `v2.0.0-rc.3`: gate run 44043297649, publish run 44043285692, and e2e runs 44047403171, 44054950768 and 44061304044.

Stage times come from the gate's step log (`gh run view --log`) and from each run's jobs API (`created_at`, `started_at`, `completed_at`).
The causes come from the dogfood project's Cloud Logging, read on 2026-11-18: Kubernetes events for the `dogfood-e2e` ActionsGateway, its worker pods and their nodes, and the Admin Activity audit log for the pods.
Those are reads of the GCP project's log store, not of the cluster, and they took no lease.

## Where the time goes

Each "matrix" is one job, `e2e / e2e`, on one GAG runner: the whole e2e suite runs inside a single worker pod.
So the three legs are three jobs in series on one tenant, separated by tenant swaps.

| Stage | `v2.0.1-rc.1` | `v2.0.0-rc.3` |
|---|---|---|
| Environment approval (the job's `created_at` to `started_at`) | 2m30s | 24m18s |
| Waiting for `publish.yml` | 11m13s | 1s |
| Checkout and tool setup | 53s | 54s |
| Preflight and deploy, to the first dispatch | 4m16s | 3m51s |
| Kata: dispatch to a runner picking the job up | 3m59s | 9m56s |
| Kata: job | 24m22s | 27m52s |
| Kata: watch end and mirror census | 31s | 37s |
| Sizing, capacity, CRD smoke, soak, storage | 2m37s | 2m36s |
| **Swap to DinD** (of which the gateway delete hold) | **9m45s (8m39s)** | **10m11s (8m59s)** |
| DinD: dispatch to runner | 3m47s | 3m48s |
| DinD: job | 17m23s | 19m31s |
| **Swap to Dragonfly** (of which the hold) | **14m47s (13m02s)** | **18m03s (16m19s)** |
| Dragonfly: dispatch to runner | 5m16s | 5m03s |
| Dragonfly: job | 24m01s | 26m22s |
| Dragonfly: census and readings | 4m25s | 5m03s |
| **Gate step, start to verdict** | **115.5 min** | **133.1 min** |
| Teardown after the verdict | hung, then reclaimed (Q1360, fixed in #2357) | 5m03s |

The three e2e jobs are 65.8 and 73.8 minutes, 57% and 55% of the gate step.
That is the work the gate exists to do.
The two swap holds are 21.7 and 25.3 minutes, 19% in both runs, and they prove nothing.
Waiting for a runner is 13.0 and 18.8 minutes, of which the cold-node components are measured below.

Tag push to the gate step starting was 14.6 minutes for rc.1 and 25.3 for rc.3.
The environment approval is a human click and runs concurrently with `publish.yml`, so the gate waits for whichever finishes last.

## The pacing stage

The gate is one serial lane: one e2e tenant namespace, one RunnerSet, one e2e node pool capped at two nodes, used by three legs in turn.
Whatever sits idle on that lane delays the verdict minute for minute.
The suite's own execution is the lane doing its job.
The largest idle block on it is the gateway delete inside each tenant swap, which is 19% of the gate in both samples.

### Why the swap waits

All four holds have the same cause, read from the ActionsGateway's `TeardownIncomplete` events:

```text
still present: NetworkPolicy dogfood-e2e-workload (held for worker pods runner-ci-e2e-67d39d7-0ebebbcb-…-c41e273)
```

The held pod is the previous leg's own worker, the one whose job had already finished.
In the first swap of rc.1:

1. 05:06:47Z: the Kata job's runner finished.
   The AGC patched the pod at 05:06:49Z, and no container event follows.
2. 05:10:15Z: `e2e-stop.sh` deleted the ActionsGateway.
   Its drain checks passed, because the RunnerSet counts no active or pending job.
3. The GMC's `reconcileDelete` (`cmd/gmc/internal/controller/actionsgateway_v2_controller.go`) passed its Q547 drain gate for the same reason and deleted the AGC Deployment, RoleBinding and ServiceAccount in that pass.
   The design ([04-operational-flows.md](../design/04-operational-flows.md)) relies on the AGC deleting every worker pod when it sees the gateway's deletion timestamp.
   With zero jobs counted there is nothing to wait for, so the AGC is gone before it acts.
   The audit log shows no delete of the pod by the AGC.
4. The AGC's reaper would have deleted the finished pod at `completedPodTTL`, 5 minutes by default (`DefaultCompletedPodTTL`), at about 05:11:49Z.
   The swap came 3.5 minutes after the job, so the reaper was deleted first.
5. The Q1328 hold keeps the workload NetworkPolicy, and the finalizer with it, while any bound worker pod exists.
   `boundWorkerPods` lists pod metadata only, so a finished pod counts the same as a running one.
6. 05:17:04Z: the cluster autoscaler marked the now-idle e2e node for scale-down, 10m15s after the job ended.
   The node was deleted at 05:17:59Z, `pod-garbage-collector` deleted the orphaned pod at 05:18:52Z, and the delete returned at 05:18:54Z.

rc.3's final teardown shows the converse: it ran 5.4 minutes after the last job, past the TTL, and its delete returned in 2 seconds.

The pod's phase at the hold was not read directly: the audit-log query for the pod returned no kubelet status writes, and the cluster itself was off limits.
That no container event follows the job's end, and that pod GC rather than a kubelet deleted the pod, both point to a finished pod.

Outside the gate this is an operator-facing defect.
Deleting a gateway within `completedPodTTL` of its last job holds the finalizer until something else deletes the finished pod.
On a node that never scales down, pod GC leaves terminated pods alone until the cluster passes its terminated-pod threshold, so the hold could last indefinitely.
That case is inferred from pod GC's rules and was not measured.

## Ranked opportunities

Savings are per gate.
"Measured" means the number is a duration read from the two runs; "estimate" means it was derived from those durations and has not been observed.
Cost effects are on the dogfood cluster's node-hours and the gate's Actions minutes.
The gate runs on `ubuntu-latest` in a public repository, so its minutes are not billed, but every saved minute is counted here anyway.

| Rank | Row | Change | Saving | Cost effect |
|---|---|---|---|---|
| 1 | Q1363 (shipped) | Put the Dragonfly leg on a fresh e2e node explicitly | None: it keeps rank 2 from weakening a reading | Neutral (estimate): that leg cold-starts today too |
| 2 | Q1362 (shipped) | Release the gateway delete once the finished worker pod is reaped, not when its node goes | 20–24 min (estimate, from measured holds of 21.7 and 25.3 min) | Falls (estimate): 2 system nodes × 20–24 min ≈ 0.7–0.8 node-h, the same Actions minutes, and the e2e node's 10-min idle before scale-down |
| 3 | Q1365 (shipped) | Deploy once the image jobs succeed; require the whole publish run only before the CRD smoke | Up to 5.8 min (measured: `chart-publish` took 5m47s and 5m30s), and only when the approval beats the images | Falls by the same Actions minutes; node-hours unchanged |
| 4 | Q1364 (shipped) | Bring the e2e node up during deploy instead of on the Kata dispatch | About 3 min (estimate, from rc.1's measured 3m59s dispatch-to-runner) | Falls slightly (estimate): the e2e node starts ~1–2 min early, while 2 system nodes stop ~3 min early |
| 5 | Q1366 (shipped) | Take the Dragonfly readings concurrently | About 2.5 min (estimate: 3m42s of probes measured in series, the slowest 74s) | Falls by the same minutes and system node time |
| 6 | [Q1367](../queue/Q1367.md) | Schedule Kata workers only once a node's `kata-qemu` runtime is ready | Up to 37–74s per cold node (measured failure window), so about 1–2 min | Falls slightly |
| 7 | [Q1368](../queue/Q1368.md) | Scope running the Kata and DinD legs at the same time on dogfood | About 20 min (estimate: the DinD leg's length once rank 2 lands) | Unpriced: peak e2e nodes rise from 1 to 2 inside the quota the gate already reserves, node-hours are expected to fall |

Ranks 1 to 6 together bring a gate like rc.1's from 115.5 minutes to roughly 85 (estimate), plus up to 5.8 minutes off tag-to-gate.
Rank 7 is a scoping row because its proof risks and quota are unmeasured.

### 1. A fresh node for the Dragonfly leg (Q1363)

Shipped as `empty_node_pool` in `lib/pool.sh`, called at the top of `dragonfly_leg` after `e2e-stop.sh`: it resizes the e2e pool to zero and waits until the cluster lists no node.
A pool that cannot be emptied is a warning, not a gate failure, because the reading is evidence rather than a gate.
GKE accepts `resize` on an autoscaled pool but advises against combining the two, so the size interaction is not exercised; there is no live cluster in this change.

Not a saving, and it must land before rank 2.
The Dragonfly leg installs Dragonfly's node client so the kubelet pulls worker images through it, and its `node-p2p` reading records whether containerd did.
Today that leg always starts on a new node, because the swap hold outlasts the autoscaler's scale-down.
Once rank 2 lands, the DinD node is still up when the Dragonfly worker is scheduled, the 722 MB runner image is already cached there, and `node-p2p` would report "no request by rule", the reading's `finding` shape, for a reason that has nothing to do with Dragonfly.
So the gate should resize the e2e pool to zero before the Dragonfly dispatch.
That reproduces today's cold start on purpose, and costs a pool resize (estimated at about a minute) on top of the cold start the leg already pays.

The risk to the proof is the point of the row: without it, rank 2 silently changes what a reading means.

### 2. The swap hold (Q1362)

The fix belongs in the GMC, because the hold is wrong for every operator, not only the gate.
Two shapes fit the design, and which is smaller depends on the held pod's phase, which needs reading on kind first:

- **Hold the AGC, not only the policy.** Extend the Q547 drain gate from "no active or pending job" to "every bound worker pod carries a deletion timestamp", so the AGC gets to issue its `gateway_deleted` deletes before the GMC removes it.
  This follows the design as written, and works whatever phase the pod is in.
- **Skip terminal pods in the policy hold.** A pod in `Succeeded` or `Failed` runs no container, so the workload policy no longer protects anything.
  This changes Q1328's rule, and leaves the pod for the next AGC or pod GC to delete.

The saving is the measured hold less whatever the reap takes, which should be seconds for a finished pod.
That residual is an estimate.
A second effect is unmeasured: the DinD leg's worker would land on the Kata leg's still-warm node, skipping most of its measured 3m47s wait.
That bonus is not counted in the saving above.

Risk to the proof is low.
The NetworkPolicy still outlives every pod that can run code, and the gate's own drain checks in `e2e-stop.sh` are unchanged.
The risk is to the product: this is teardown code, which Q125, Q547 and Q1328 each hardened, so the change needs envtest cases for a finished pod, a running pod, and the no-AGC deadline.

### 3. Deploy on the images, not the whole publish run (Q1365)

`wait-for-publish.sh` waits for `publish.yml` to conclude.
The deploy needs only the seven `publish (…)` image jobs, which finished 5m47s (rc.1) and 5m30s (rc.3) before the run did.
The rest of the run is `chart-publish`, which creates the GitHub Release and attaches the signed v2 CRD manifest and the `gag-migrate` binaries.
The gate reads those only in the CRD-smoke and storage legs, about 35 minutes after deploy.

So the gate can start deploying once the image jobs succeed, and check that the whole run succeeded before the CRD smoke.
The saving applies only when the environment approval comes before the images, as in rc.1; in rc.3 the approval took 24 minutes and the wait was 1 second.

Shipped as `wait-for-publish.sh --images`, which returns once every `publish (…)` job has succeeded.
The legs that read what `chart-publish` produces wait for the whole run through `await_publish_run` in `validate-release.sh`: `crd_smoke`, and `storage_leg`, which also pulls the OCI chart (`helm pull oci://…/charts/actions-gateway`) in addition to the release assets the row named.
The saving was not re-measured after the change; the 5m47s and 5m30s above are the `chart-publish` durations it is sized against.

The risk is spending: a red `chart-publish` would now be found after the Kata leg rather than before deploy, costing about 35 minutes of node time on a candidate that cannot ship.
How often `chart-publish` fails was not counted.

### 4. Bring the e2e node up during deploy (Q1364)

The Kata leg's worker triggers the e2e pool's scale-up from zero, so its start pays the whole cold path.
For rc.1: 1m42s from scale-up to node Ready, 33s until the CSI driver and scheduler placed the pod, a 37s `kata-qemu` sandbox failure, and 36s of image pulls.
Deploy and `e2e-start.sh` take about 4 minutes before the dispatch, longer than that path, so scaling the e2e pool to one node as deploy starts would hide it.

rc.3's Kata wait was 9m56s, but its worker pod's first event came 5 minutes after the job was created.
What happened in those 5 minutes was not read; the cold path after them took the same shape as rc.1's.

Shipped as `prewarm_e2e_node` in `validate-release.sh`: an asynchronous resize of the e2e pool to one node, issued after `reserve_cpu_budget` and before `start.sh`, and only when the Kata leg is selected.
That placement is after `setup.sh`, so the head start is `start.sh` plus `e2e-start.sh` rather than the whole deploy, and the roughly 3 minute saving above is an upper bound that the next gate's Kata dispatch-to-runner time will measure.
The gate log carries `e2e-pool-nodes:` (the pool's node count at dispatch, to show whether the autoscaler removed the early node before the job arrived) and `e2e-prewarm: warning:` (a refused resize).
A refused resize is a warning, not a failure; GKE accepts `resize` on an autoscaled pool but advises against combining the two, so the size interaction and the overlap with the system-pool resizes in `start.sh` are not exercised; there is no live cluster in this change.

The risk is to the CPU budget.
`reserve_cpu_budget` (Q631) reserves the e2e pool's ceiling after setup, and a node raised before it draws on the same project quota.
The change has to keep that reservation ahead of the scale-up, or reserve first.
The node must not be the one the Dragonfly leg uses, which rank 1 already guarantees.

### 5. Concurrent Dragonfly readings (Q1366)

After the Dragonfly job, the gate takes its readings one probe at a time.
In rc.1: the mirror battery probe pod took 68s, `worker to seed proxy` 74s, the `CONNECT` probe 5s, `node P2P` 1s, and `worker to node client` 74s.
Both 74-second probes are waiting out a timeout that is the expected answer.
They read different pods and different targets, so running them together bounds the step by its slowest probe.

Shipped in `dragonfly_readings`: the worker probe, the node-client probe and the node-client log read run beside a seed lane, and each reading's output and record are held apart and emitted in the order they had before.
Three of the readings are not independent.
`p2p-path` counts lines in the seed peer's log, which the mirror battery's fetches add to and the `CONNECT` probe's requests would, so the battery, that read and the `CONNECT` probe stay in series as the seed lane.
On rc.1's times the seed lane is 74s, the same as each worker probe, so the saving holds; it was not re-measured.
One interaction remains: a worker that does reach the seed proxy, itself a finding, can add a line to the log `p2p-path` counts.

The battery fetches its own cluster credentials, and gcloud truncates and rewrites the kubeconfig in place (read from its source), so it is given one of its own.

The risk is to the readings, not the verdict, since none of them gates: the probe pods have distinct names and each keeps its own control.
Three of them share the e2e tenant at once, which with its AGC and two workers is the six pods its ResourceQuota allows.

### 6. Schedule Kata workers onto ready nodes (Q1367)

Every cold e2e node failed its first sandbox with `no runtime for "kata-qemu" is configured`: 37s in rc.1's Kata leg, 74s in rc.3's, and 52s in rc.1's Dragonfly leg (from Q1360).
The pod is scheduled before the node's Kata runtime is installed, and the kubelet's retry backoff decides when it tries again.
Gating placement on a node label or taint that the runtime installer clears would remove the backoff part.
How much of each window is the install itself, which no change removes, was not measured.
This helps every Kata job on a cold node, not only the gate.

### 7. Kata and DinD at the same time (Q1368)

After ranks 1 to 6 the suite's execution is the constraint, and running two legs at once is the only change left that removes a whole leg from the wall clock.
It stays on the one dogfood cluster: the legs are serial so the gate runs on a single cluster inside GKE's free tier, and both legs already share that cluster's `e2e` pool.
Kata and DinD are two overlays of one namespace, ActionsGateway and RunnerSet, and their namespace security profiles differ (baseline and privileged), so each needs its own tenant with its own runner label.
The e2e pool already allows two nodes (the autoscaler's `max: 2`), and e2e node-hours should stay about the same while system-pool hours fall.
Quota does not block it: the gate already reserves the e2e pool's full ceiling, 2 × 8 vCPU, out of `CPUS_ALL_REGIONS` before capping `workers` (Q631).
`e2e-test.yml`'s concurrency group keys on workflow and ref, so two dispatches on `main` would run one after the other; the group needs the `runner` input in its key.
Dragonfly stays serial: its mirror back end and node client are cluster-wide.

What is not known decides whether it is worth doing.
Whether the Kata leg's mirror census stays complete with a second tenant's clients on the same mirrors is unknown, and so is whether the sizing and capacity readings, taken from the Kata tenant, are disturbed by a concurrent DinD tenant.
Q1161, deferred, is the cross-project form of the same idea and expects to cost more.

## Considered and not worth pursuing

- **Running a subset of the suite on the DinD or Dragonfly leg.** The DinD leg exists so the shipped privileged-dind template runs a real job before release (Q1159), and the Dragonfly leg is the one full e2e run the node client gets (`docs/operations/release.md`).
  The suite is the workload in both, so cutting it weakens the proof the leg was added for.
- **Pre-pulling the 722 MB runner image.** It would save at most 22–70s per cold node, and on the Dragonfly leg it would defeat the `node-p2p` reading, which needs that pull to go through the node client.
- **Keeping the registry mirrors scaled up across swaps.** About 30–40s per swap (estimate), and each leg's mirror census relies on the mirrors' access logs starting empty.
- **Scaling the system pool during the publish wait.** The pool's 83s scale-up would overlap the wait, but two nodes would sit idle for up to 11 minutes, which raises node-hours.
- **Removing the environment approval.** It took 2.5 and 24 minutes, but the required reviewer is the human check on the gate's cloud credentials, and nothing else stands in for it.
- **Polling.** No step on the critical path sleeps for long: the e2e watch prints a 30s heartbeat, and the worker-drain settle costs about 15s per swap.
- **Teardown.** rc.3's took 5 minutes, after the verdict; it costs node time but does not delay the verdict, and its rc.1 hang is fixed (Q1360).

## Next measurement

The first gate run after rank 2 lands should be read the same way: the two swaps from `Taking the … tenant down` to the next `dispatched run is`, and the DinD leg's dispatch-to-runner time.
If the swaps are under two minutes, the remaining constraint is the e2e jobs themselves, which is where rank 7 starts.
