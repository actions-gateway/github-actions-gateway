# Right-sizing worker resources from measured usage

> **Audience:** Tenant operator, Platform engineer

Every worker pod's CPU/memory `requests`/`limits` start as a guess in the tenant's `RunnerTemplate`.
This guide turns the gateway's built-in usage metrics into measured values, so the guess is revisited with data instead of `kubectl top` scraping.
Actions Runner Controller (ARC) has no equivalent feedback loop.

**Scope:** v2 `RunnerSet` worker pods.
The Actions Gateway Controller (AGC) samples usage per RunnerSet × container.

## How the sampling works

The AGC polls the `metrics.k8s.io` API (metrics-server) every 15 seconds (`WORKER_USAGE_SAMPLE_INTERVAL`; `0`/`off` disables) and keeps the running CPU/memory peak per worker pod × container.
When the pod finishes, its peaks are folded into per-`RunnerSet` Prometheus series — one worker pod runs exactly one job, so a per-pod peak is a per-job peak.
See the [metrics reference](observability-metrics.md#worker-usage--right-sizing-metrics-q359) for the full series list.

Two caveats to hold in mind when reading the numbers:

- **Short jobs are under-sampled.** metrics-server resolves at ~15s; a job shorter than one interval finishes with no sample and is counted in `actions_gateway_worker_usage_jobs_unsampled_total` instead.
  Check the unsampled:sampled ratio before trusting the distribution — short jobs are rarely the sizing constraint, but a mostly-unsampled RunnerSet has no signal.
- **Peaks are point-in-time reads.** A sub-second CPU burst between reads is smoothed by metrics-server's window.
  Treat CPU figures as sustained-peak, not instantaneous-max; memory (a level, not a rate) reads accurately.

## Prerequisites

- **metrics-server** (the `metrics.k8s.io` aggregated API) — present by default on GKE/EKS/AKS; on kind/bare clusters install it explicitly.
  Without it the AGC runs normally but emits `actions_gateway_worker_usage_poll_errors_total` instead of usage data (see [Troubleshooting](#troubleshooting)).
- The AGC tenant role ships the required `pods.metrics.k8s.io` read grant since this feature landed; no manual RBAC step.
- A Prometheus scraping the AGC (see [metrics access](observability-metrics-access.md)).

## Step 0 — read the built-in recommendation first

Since Q359 Phase 2 the gateway derives the recommendation for you and publishes it on the `RunnerSet` itself — check there before reaching for PromQL:

```bash
kubectl get runnerset <name> -n <ns> -o jsonpath='{.status.sizingRecommendation}' | jq
```

Each entry carries, per container: recommended `requests` (p95 of per-job peaks), a recommended memory `limit` (observed max × 1.4 headroom; no CPU limit is ever recommended), the raw `observedPeak`/`observedP95`, and — the confidence signal — `sampleCount` plus `windowStartTime`.
Treat a recommendation with a low `sampleCount` as a hint, not a target; it appears from 5 sampled jobs and survives AGC restarts (the status field is also the store the sampler re-seeds from).

The gateway also judges your current ask: the advisory **`SizingDrift` condition** turns `True` (with the offending containers named in the message) when, after at least 20 sampled jobs, a template request is ≥2× the recommendation (waste) or a memory limit sits below the highest observed per-job peak (OOM risk):

```bash
kubectl get runnerset <name> -n <ns> -o jsonpath='{.status.conditions[?(@.type=="SizingDrift")]}' | jq
```

| `status` / `reason` | Meaning |
|---|---|
| `True` / `SizingDriftDetected` | At least one template container has drifted materially. The message names each one, the resource, and the direction — `request … is >=2x the recommended …` (waste) or `memory limit … is below the observed per-job peak …` (OOM risk) — with both measured figures. |
| `False` / `SizingWithinRange` | Every container the AGC could judge is inside the thresholds. **Not the same as "right-sized":** drift flags only waste and OOM risk, so an *under*-asking CPU request reads as within range — CPU is compressible, and the job bursts into idle node capacity rather than failing. Read `status.sizingRecommendation` yourself if you are sizing for latency. |
| `False` / `InsufficientSamples` | Fewer than 20 sampled jobs for every template container, so drift is not judged yet; the message points at each entry's `sampleCount`. Also what you get when the containers that *do* have enough samples are not in the template (an injected sidecar) — there is measured data but no ask to compare it against. |
| `False` / `SizingProfileActive` | A [sizing profile](#sizing-profiles-opt-in-auto-apply) is deriving resources at pod build, so the template's static ask is not what worker pods run with and judging it would mislead. |

**No `SizingDrift` condition at all** means there was nothing to judge: no `status.sizingRecommendation` (metrics-server absent, sampling off, or the sampler still warming up) or no resolved `RunnerTemplate`.
That silence is deliberate — otherwise every set in a cluster without metrics-server would carry `InsufficientSamples` forever.
Read a missing condition as "not measured", not as "no drift"; if you expected data, start at [Troubleshooting](#troubleshooting).

The condition never gates `Ready`, and by default nothing is auto-applied: apply the values to the `RunnerTemplate` yourself (Step 2's validation still applies), or opt into a [sizing profile](#sizing-profiles-opt-in-auto-apply) to have the gateway apply them at pod-build time.
Use the PromQL below when you want the full distribution behind the recommendation or a different window/percentile.

## Step 1 — read the distribution

Let jobs run until `actions_gateway_worker_usage_jobs_sampled_total` for the RunnerSet is meaningful (a few dozen jobs of the workload mix you are sizing for).
Then, per container (the runner container is named `runner`):

```promql
# p95 of per-job CPU peaks (cores), by RunnerSet and container, over 7 days
histogram_quantile(0.95, sum by (le, namespace, runner_set, container) (
  rate(actions_gateway_worker_usage_job_cpu_peak_cores_bucket[7d])))

# p95 of per-job memory peaks (bytes)
histogram_quantile(0.95, sum by (le, namespace, runner_set, container) (
  rate(actions_gateway_worker_usage_job_memory_peak_bytes_bucket[7d])))

# absolute max peak seen since the AGC last restarted
actions_gateway_worker_usage_cpu_peak_cores
actions_gateway_worker_usage_memory_peak_bytes

# how much of the job population the histograms actually saw
sum by (namespace, runner_set) (rate(actions_gateway_worker_usage_jobs_sampled_total[7d]))
  /
sum by (namespace, runner_set) (rate(actions_gateway_worker_usage_jobs_unsampled_total[7d]))
```

The histogram quantiles are bucket-interpolated — read them as the bucket range, not an exact figure, and cross-check against the max-peak gauges.

## Step 2 — derive `requests`/`limits`

Apply the resource-model rules (proven in the [dogfood right-sizing exercise](../plan/archive/dogfood-runner-rightsizing.md#resource-model-principles) that this feature automates the measurement half of):

| Field | Set to | Why |
|---|---|---|
| memory `request` | ≈ p95 of per-job memory peaks | Memory is non-compressible; the request is what the scheduler packs by. |
| memory `limit` | ≈ max peak × 1.3–1.4 | OOM headroom. Widen the factor if any job OOMs; exceeding it kills the job. |
| CPU `request` | ≈ p90–p95 of per-job CPU peaks | Drives packing; jobs above it borrow idle node CPU. |
| CPU `limit` | **omit** | CPU is compressible — a limit only throttles bursty build/test steps for no packing benefit. Keep the memory limit for noisy-neighbor safety. |

Update the tenant's `RunnerTemplate.spec.podTemplate` container resources with the derived values, then validate: watch for `OOMKilled` events in the tenant namespace over the next few days, and confirm the derived requests still fit the node shape and namespace `ResourceQuota` (see [capacity planning](../design/appendix-e-capacity-planning.md)).

**Sizing the trade deliberately:** packing tighter (requests ≈ p95) maximizes workers per node — the right call on expensive nodes (GPUs, large VMs) where worker count per node is the cost driver.
Sizing up (requests ≈ max) buys burst headroom so jobs finish faster.
The [sizing-profiles plan](../plan/runner-sizing-profiles.md) tracks automating this choice; today it is a per-`RunnerTemplate` decision.

## Sizing profiles (opt-in auto-apply)

Instead of copying the recommendation into the `RunnerTemplate` by hand, a `RunnerSet` can opt into a **sizing profile** (Q359 Phase 3): the AGC derives the worker containers' CPU/memory at pod-build time, per acquired job, so a spec edit or newly confident history takes effect on the next job with no restart.
The template stays authoritative unless you opt in.

```yaml
spec:
  sizing:
    profile: Binpack          # Static (default) | Binpack | Throughput | NodeShare
    # minRequests / maxRequests clamp every derived value (optional):
    minRequests: { cpu: 250m, memory: 512Mi }
    maxRequests: { cpu: "8", memory: 16Gi }
```

| Profile | Derivation | Use when |
|---|---|---|
| `Static` (default) | Exactly what the template says — today's behavior. | You apply measured values by hand. |
| `Binpack` | `requests` = `limits`: CPU from the p95 of per-job peaks, memory from the recommended limit (observed max + headroom) → **Guaranteed QoS**. The implied CPU limit deliberately trades burst for predictable packing. | Expensive nodes (GPUs, large VMs) where workers-per-node is the cost driver. |
| `Throughput` | `requests` from the p95 of per-job peaks; **no CPU limit** (jobs burst into idle node capacity); memory limit = observed peak × `limitHeadroomPercent` (default 150), clamped by `maxRequests.memory`. | Job latency matters more than packing density. |
| `NodeShare` | Runner-container `requests` = a declared per-node envelope ÷ `workersPerNode` — no usage history needed. Declare the envelope yourself (`sizing.nodeShare.allocatable` + `workersPerNode`); the AGC is namespace-scoped and never reads Node objects. Limits keep the template's values (a template limit below the derived request is lifted to it). | GPU bin-packing: `allocatable ÷ GPUs per node` keeps the GPU count, not an inflated CPU ask, the binding constraint. |

Safety rails, in all profiles:

- **Extended resources (GPUs) are never modified** — only the cpu/memory keys are ever derived; the shape's job-selected identity passes through byte-identical.
  This is also why `nodeShare.allocatable` must declare `cpu`, `memory`, or both: an envelope naming only extended resources divides nothing, so the apiserver rejects it at admission rather than letting the profile report `Active` over untouched template values ([runbook](troubleshooting.md#runnerset-rejected-nodeshareallocatable-declares-neither-cpu-nor-memory)).
  Declaring just one of the two is fine — the other keeps the template's ask.
- **History-based profiles fall back to `Static` until confident** — `Binpack` and `Throughput` apply only once *every* template container has a recommendation with ≥20 sampled jobs (whole-pod, so QoS stays predictable).
  `status.sizingProfileState` reports which side you're on: `Active` (derived values applied) or `AwaitingSamples` (template values, history accumulating).
- **Clamps** — `minRequests`/`maxRequests` bound every derived request, and `Throughput`'s derived memory limit, so a skewed history (one pathological job) cannot push pods beyond an operator-set envelope.
  The limit needs the bound most: it follows a peak the jobs control, so without `maxRequests.memory` a job held near its limit raises the next one by the headroom factor each generation (Q1327).
  Size the ceiling against the namespace `ResourceQuota` and any `LimitRange`: derived values are still subject to both at admission, and the existing `WorkerQuota*` conditions and quota retries surface a conflict at runtime.
  An admission mutation that cancels `Throughput` rejects nothing, so it gets its own condition — [`SizingProfileOverridden`](#when-something-re-injects-the-cpu-limit-throughput-removes).

  > **Set `maxRequests` before enabling `Binpack` *or* `Throughput` on a shape whose measured peak approaches node allocatable.** Both derive `requests` from the observed history, and a derived request above a node's allocatable CPU is simply unschedulable: every worker pod sits `Pending` and no job runs.
  > This is not hypothetical — on the project's own dogfood tenant a measured CPU peak of ~3750m derived a **request of 3800m** against an `e2-standard-4`'s ~3.4 vCPU allocatable, so either profile without `maxRequests: {cpu: "3"}` would have wedged the pool.
  > **`Throughput` is not exempt because it drops the CPU *limit*:** scheduling is decided by the *request*, so the clamp matters exactly as much — and it is the derived request, not the peak, you size against allocatable, since the request is the p95 rounded up and can land above the peak.
  > The failure is silent until pods stop scheduling, so clamp first, then enable.
  > `WorkersUnschedulable` is the condition that fires if you get it wrong.
- **Drift reporting steps aside** — while a profile is `Active`, the `SizingDrift` condition reports `False/SizingProfileActive` (pods no longer run the template ask, so judging it would mislead).

### When a job runs out of memory

A job killed at its memory limit is normally the job's to fix: a re-run gets the same limit and is killed the same way.
Under `Binpack` and `Throughput` the AGC chose that limit, so on a scale-set `RunnerSet` it re-runs a job killed for memory with a higher one (Q1183).
It does not check that the job reached the limit; [which limit ran out](#which-limit-ran-out) is yours to judge.
The history alone cannot be relied on to raise it, because a job killed at its limit is sampled at or below that limit.

**What it does.** When a worker the profile built has its runner container killed `OOMKilled`, the job's next attempt gets the killed limit times the profile's headroom (1.4 under `Binpack`, `limitHeadroomPercent / 100` under `Throughput`), rounded up to 64Mi and capped at `maxRequests.memory`, or at twice the killed limit where you set none.
Under `Binpack` the memory request rises with it, and nothing checks that a node can hold the raised request.
One that no node can hold leaves the re-run's worker `Pending` with `WorkersUnschedulable` set, until the AGC cancels the run; a later placement of any worker on the set then re-runs it into the same wait, on the same budget (measured on dogfood, 2026-11-24).
So under `Binpack` set `maxRequests.memory` no higher than the memory an empty worker node has free after its DaemonSets, less what the pod's other containers request: the cap applies to each container, not to the pod.
The job is then re-run automatically on the same per-run budget as every other recovery (`maxEvictionRetries`), and a Normal `MemoryLimitRaisedForRetry` Event on the `RunnerSet` names the worker, its node, the job, the run, the memory request and both limits.
The re-run waits until GitHub concludes the run, because GitHub refuses one while any other job of the run is still going, so a long matrix is re-run only after its last job finishes.
It waits up to 24 hours, then gives up with an `EvictionRerunFailed` Warning Event and `actions_gateway_eviction_rerun_failures_total{cause="oom",reason="run_never_concluded"}`; re-run it by hand then.
An AGC restart does not lose the wait (Q1244).

**A job killed in its first seconds** can die before GitHub's message saying which runner took it is handled.
The AGC then waits for that message, up to 15 minutes, before it raises any limit, and keeps the `Failed` pod until it has acted (Q1389).
Without one it raises nothing and re-runs nothing, and reports a Warning `EvictionRecoveryIdentityUnknown` Event and `actions_gateway_eviction_recovery_identity_unknown_total{cause="oom"}`; [find the job and re-run it by hand](troubleshooting.md#a-runner-killed-for-memory-whose-job-is-unknown).

Only the killed job is re-run, found by the name of the runner that died, and a sibling that failed on its own stays failed (Q1319).
Five things follow:

- **Jobs that need it run again**, even ones that passed: GitHub re-runs a job's `needs:` dependents with it.
- **The run stays red** while a sibling's failure is not re-run, so read the job's own result.
  GitHub can conclude it `cancelled` rather than `failure` where a matrix's cancelled legs are left too.
- **Several killed jobs of one run** are re-run together, on one slot, where every failed job of the run was killed for memory.
  Where another job failed on its own, they are re-run one after another, each once the attempt before it concludes and each on its own slot, until `maxEvictionRetries` is spent and the rest get `EvictionRetriesExhausted`.
- **A job that reports on the others** (`if: always()`, red when one it needs is) costs the rest of the run a second attempt and slot, because GitHub's job list does not say what needs what.
  The first killed job is re-run with it; where it is red again and nothing else failed, the other killed jobs and the cancelled ones are then re-run together (Q1401).
- **A cancelled job is re-run with the killed one where no other job failed**, such as the unfinished legs that GitHub's default `fail-fast: true` cancels with a killed leg.
  GitHub's job list spells a job past its `timeout-minutes` the same way, and one a concurrency group cancelled, so those are re-run too.
  A run GitHub itself concludes `cancelled`, as it can one holding a failure and a timeout, is not re-run at all (`run_cancelled`), unless the attempt only carries cancelled jobs from an earlier one.
  Behind a job that reports on the matrix the cancelled legs are re-run in a second attempt, which needs `maxEvictionRetries` of at least 2, the default; at 1 they get `EvictionRetriesExhausted`.
  It needs one more for each other killed job re-run before the matrix's leg, and two matrices that each lost a leg and have a reporting job each keep their cancelled legs.
  Where another job failed on its own they are not re-run: re-run the run's failed jobs by hand, or set `fail-fast: false` on a matrix whose legs outgrow their memory.

Every failed job is re-run, as a disruption's recovery does, where the AGC cannot tell which jobs were killed: GitHub's job list cannot be read, the worker's pod names no runner and its job name is not unique, a disruption recovery is re-running the same attempt, or 16 or more workers of one attempt were killed.

A run whose budget is already spent gets `EvictionRetriesExhausted` instead, with no `MemoryLimitRaisedForRetry`; the raised limit still applies if you re-run it by hand.
The raised limit stays with that job of that run for up to 7 days, so a manual re-run gets it too; no other job is affected, and the next run of the workflow starts from the history again.

**When it does not.** Each of these is withheld with a Normal `EvictionRerunWithheld` Event and counted in `actions_gateway_eviction_rerun_withheld_total{cause="oom"}`, with no budget slot spent unless the row says so:

| `reason` | Why | What to do |
|---|---|---|
| `own_limit` | The set's profile is `Static` or `NodeShare`, the profile was still `AwaitingSamples` when the pod was built, or the killed limit is already at `maxRequests.memory` | If the runner [reached its limit](#which-limit-ran-out), raise the limit or `maxRequests.memory`; then, in either case, re-run by hand |
| `same_class_again` | The job was killed again after a retry raised its limit: one raise per job | If the runner [reached its limit](#which-limit-ran-out), look at what grew or raise the limit; then, in either case, re-run by hand, which gets the raised limit for up to 7 days |
| `job_not_found` | No failed job of the run ran on the killed worker's runner: the runner took no job, or its job ended before the kill. Decided after the run concludes, so this one has spent its slot | Nothing, unless a job was lost; find it in the run's other attempts and re-run it by hand |
| `breaker_open` | More than 10 runs were re-run for memory within 10 minutes, so retries are paused for 30, with one Warning `AutoRetryPaused` Event | Look for a shared cause first: a dependency that grew every job, or nodes packed past their memory |

A sidecar killed for memory, such as `dind`, is not retried: the runner survives it and reports the job's own result.
Neither is a runner with no memory limit, since there is none to raise; it gets no Event from the retry, and `PlatformFaultOnFailedJob` reports its job.

**What it hides.** A change that doubles a job's memory gets the memory on its one retry instead of failing.
Watch `actions_gateway_eviction_retries_total{cause="oom"}` and the `MemoryLimitRaisedForRetry` Events; `actions_gateway_eviction_rerun_outcomes_total{cause="oom"}` says how often the raised retry passed, and the `ActionsGatewayRetryCauseMisclassified` alert fires when more than half of them fail over 7 days (Q1245).
`actions_gateway_auto_retry_paused{cause="oom"}` reads 1 while the breaker has retries paused.
The re-run's worker pods carry `actions-gateway.com/rerun-cause: oom`, so [cost attribution](cost-attribution.md#the-labels-and-namespaces-to-query) can count what the retries cost.

#### Which limit ran out

`OOMKilled` says the runner was killed for memory, and a runner is expected to read that way too when its node ran out, or the pod reached a limit of its own (Q1400).
The AGC cannot tell them apart, so its Events name no cause and give you the node, the memory request and the limit.

- **The runner's own limit is likely** where the template sets no pod-level `spec.resources`, and either the request equals the limit, as `Binpack` sets them, or the pod's `status.qosClass` is `Guaranteed`.
  Such a runner is late in line when its node runs out, which makes this likely and not proven.
- **The pod's limit is possible** where the template sets a memory limit in its pod-level `spec.resources`: the kernel then chooses inside the pod, so the runner's request says nothing.
  Raise that limit: the runner shares it with the pod's other containers.
- **The node is likely** where it reports a `SystemOOM` Event at about the time of the kill, or where other pods on it were evicted or killed for memory then.
  No `SystemOOM` Event proves nothing: the kubelet raises none when the pods together reach the limit set for all of them, and Events expire, after an hour by default.

```sh
# Node Events are in the default namespace. <node> is the one the Event names.
kubectl get events -n default \
  --field-selector involvedObject.kind=Node,involvedObject.name=<node>,reason=SystemOOM
```

Where the node ran out, raise the runner's memory request toward its limit (`minRequests.memory` under a profile) or pack the node less, and re-run the job by hand: it may pass on another node.
A full node kills a runner sooner the less memory it requests.

### Getting Guaranteed QoS out of `NodeShare`

`Binpack` is the profile that sets `requests == limits` for you.
`NodeShare` does not — it derives the runner container's *requests* and leaves limits to the template — so a GPU pool that wants both an even node split **and** Guaranteed QoS has to arrange the second part itself.
It can:

Set the runner container's template CPU **and** memory limits at or below the share you expect (`allocatable ÷ workersPerNode`).
Each is then raised to the derived request by the same rule that stops a too-low template limit being rejected at admission, and the container comes out with `requests == limits`.

```yaml
# runner container in the RunnerTemplate; envelope 15 CPU / 60Gi ÷ 4 workers
resources:
  limits: { cpu: "1", memory: 2Gi }   # both below the 3750m / 15Gi share
```

Two caveats, both easy to trip over:

- **A limit *above* the share is left alone**, and the pod is Burstable.
  This is the ordinary case — a template CPU limit of `4` against a `3750m` share stays `4`.
  The values above are deliberately far under the envelope so the outcome does not depend on getting the arithmetic exactly right.
- **Pod QoS is Guaranteed only if every container qualifies.** `NodeShare` touches the runner container only; a `dind` sidecar keeps its template ask, so give it explicit equal requests and limits or the pod lands Burstable however the runner is sized.

This works, but it is a side effect of the limit-lift guard rather than a knob that names the intent.
If you want it, say so on the issue tracker — the clean form is an explicit field under `sizing.nodeShare`, an additive change we held back from 1.3 for want of a concrete asker ([appendix-h §H.7](../design/appendix-h-v2-api-decomposition.md#h7-reference-integrity--runtime-conditions-not-admission)).

### When something re-injects the CPU limit `Throughput` removes

`Throughput` bursts by **removing** the runner container's CPU limit — that is its mechanism, not a side effect.
Anything that puts the limit back at admission cancels the profile, and cancels it *silently*: the pod is **not rejected**, it is admitted with a CPU limit, `sizingProfileState` still reports `Active`, and every other signal looks correct.
Jobs simply stop bursting.

The usual cause is a `LimitRange` entry of type `Container` carrying a `default` for `limits.cpu` — or a `max` with no `default`, which Kubernetes then uses as the default.
It applies to any container that does not declare one, and the container `Throughput` just built declares none.
A mutating admission webhook or a policy engine's mutate rule does the same thing just as quietly.

So the gateway reports the **effect** rather than any one cause.
It knows which worker pods it built without a CPU limit (they carry `actions-gateway.com/sizing-profile: Throughput`), and it can see what the apiserver admitted.
When those disagree, the `RunnerSet` raises the advisory **`SizingProfileOverridden`** condition:

```bash
kubectl get runnerset <name> -n <ns> -o jsonpath='{.status.conditions[?(@.type=="SizingProfileOverridden")]}' | jq
```

| `status` / `reason` | Meaning |
|---|---|
| `True` / `CPULimitInjected` | A pod the profile built with no CPU limit is running with one. The message names the pod, the container, and the limit. Jobs are capped; the profile has no effect. |
| `False` / `NoCPULimitInjected` | Every profile-built pod reached the kubelet as built. Jobs burst as intended. |
| `False` / `AwaitingWorkerPods` | The profile has built no pod yet, so there is nothing to observe — *not* a clean bill of health. |

The condition is advisory: it never gates `Ready`, and jobs keep running.
It appears only under `Throughput` and is removed under any other profile.
Because it reads the admitted pods rather than a policy object, it catches whatever did the injecting — and it needs no extra cluster access to do so.

To check a namespace *before* enabling the profile, read the `LimitRange` directly:

```bash
kubectl get limitrange -n <ns> -o jsonpath='{range .items[*].spec.limits[*]}{.type}{"\t"}{.default}{"\t"}{.max}{"\n"}{end}'
```

If a `Container` row carries a `cpu` `default` or `max`, `Throughput` will not burst in that namespace.
Three ways out, in order of preference:

1. **Drop the `cpu` default from the `LimitRange`** — the namespace stops dictating a limit and the profile works as designed.
   Keep `maxRequests` set, since that is the clamp actually bounding a skewed derivation.
2. **Use `Binpack`** — it always sets its own limit, so the `LimitRange` default never applies.
   You trade burst for predictable packing, which is the choice between the two profiles anyway.
3. **Stay on `Static`** and apply `status.sizingRecommendation` by hand, keeping whatever limit the `LimitRange` requires.

This is the one profile whose contract an admission mutation can quietly void — which is why it is the one that gets a condition.
It is also why the check is `Throughput`-only: every other profile *sets* a CPU limit, leaving a defaulting policy nothing to inject.
The memory side is unaffected for the same reason: `Throughput` sets a memory limit explicitly, so a `LimitRange` default never reaches it.

The signal arrives with the first worker pod the profile builds, not when the policy is written — it reports what was admitted, so it needs something to have been admitted.
Use the `kubectl get limitrange` check above if you want an answer before the first job runs.

## Troubleshooting

**`actions_gateway_worker_usage_poll_errors_total` rising steadily** — the AGC cannot list `PodMetrics`.
Either metrics-server is not installed (the [install pre-flight](install.md#preflight-the-cluster-required-first-step) warns about this) or the AGC's RoleBinding predates the `pods.metrics.k8s.io` grant (re-render from the current chart: the `agc-tenant-role` ClusterRole must contain a `metrics.k8s.io` rule).
The AGC log line `list PodMetrics (is metrics-server installed?)` carries the underlying error, throttled to roughly one line per ten minutes.

**All jobs land in `…_jobs_unsampled_total`** — the workload's jobs finish faster than the sampling interval.
There is no per-job signal to size from at 15s resolution; size such a RunnerSet by its node-shape share instead (see the `NodeShare` idea in the [sizing-profiles plan](../plan/runner-sizing-profiles.md)).

**`Throughput` is `Active` but jobs still run at the old CPU ceiling** — read `SizingProfileOverridden` on the `RunnerSet`.
`True/CPULimitInjected` means a pod the profile built without a CPU limit was admitted with one; the message names the pod and the limit, and [the three ways out](#when-something-re-injects-the-cpu-limit-throughput-removes) start with the namespace `LimitRange`.
If that comes back clean, the injector is a mutating webhook or policy engine — `kubectl get mutatingwebhookconfiguration` and your policy engine's rules are the next stop.

**Gauges reset after an AGC rollout** — the `…_usage_cpu_peak_cores` / `…_usage_memory_peak_bytes` gauges are peaks *since AGC start* by design.
Use `max_over_time(...[30d])` in Prometheus to bridge restarts; the histograms and counters are cumulative series that Prometheus rate/window queries already handle across restarts.
