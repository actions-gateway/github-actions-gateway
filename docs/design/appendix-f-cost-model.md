# Appendix F — Cost Model

← [Appendix E](appendix-e-capacity-planning.md) | [Back to index](README.md)

---

Audience: budget owners, platform team leads. This appendix provides a framework for estimating and allocating compute costs under this system, and compares it to a representative Actions Runner Controller (ARC) deployment.

Want a number for your own workload? Jump to the **[interactive savings calculator](#f5-savings-calculator-this-system-vs-arc)** — it computes estimated monthly savings vs ARC from your jobs/day, average job duration, and the idle-runner floor ARC would hold.

---

## F.0. Rate sources and assumptions

Every dollar figure below uses **real public list prices**, cited here so you can audit and re-derive them. Rates are AWS EC2 Linux **on-demand** in `us-east-1` and GitHub Actions per-minute billing, captured **2026-06**. They change over time and vary by region, commitment (Savings Plans / Reserved / Spot), and provider — treat them as a worked baseline, not a quote, and substitute your own contracted rates.

| Rate | Value | Source |
|------|-------|--------|
| `p4d.24xlarge` — 8× NVIDIA A100 40 GB, 96 vCPU | **$32.77/hr** (≈ **$4.10/hr per GPU**) | [AWS EC2 On-Demand pricing](https://aws.amazon.com/ec2/pricing/on-demand/) |
| `g5.xlarge` — 1× NVIDIA A10G, 4 vCPU | **$1.006/hr** | [AWS EC2 On-Demand pricing](https://aws.amazon.com/ec2/pricing/on-demand/) |
| `g4dn.xlarge` — 1× NVIDIA T4, 4 vCPU | **$0.526/hr** | [AWS EC2 On-Demand pricing](https://aws.amazon.com/ec2/pricing/on-demand/) |
| `m6i.4xlarge` — 16 vCPU, 64 GiB (CPU node) | **$0.768/hr** | [AWS EC2 On-Demand pricing](https://aws.amazon.com/ec2/pricing/on-demand/) |
| GitHub-hosted Linux 2-vCPU runner | **$0.008/min** (= $0.48/hr) | [GitHub Actions billing](https://docs.github.com/en/billing/managing-billing-for-github-actions/about-billing-for-github-actions) |
| GitHub-hosted Linux GPU runner (4 vCPU, 1× T4) | **$0.07/min** (= $4.20/hr) | [GitHub Actions billing](https://docs.github.com/en/billing/managing-billing-for-github-actions/about-billing-for-github-actions) |

**Assumptions baked into every figure below:**

- **Zero idle compute is the core lever.** This system creates a worker pod only when a job is acquired and deletes it the instant the job completes (see [§02 Architecture](02-architecture.md)). It holds **no** always-on runner replicas. ARC's scale-set mode, by contrast, commonly runs `minRunners: N > 0` to mask cold-start latency — those N runners are billed 24/7 whether or not a job is running.
- **Active job cost is identical in both systems.** A job that runs for *D* minutes on an instance costing *R* $/hr costs `D/60 × R` in *either* system — both run exactly one pod for the job's duration. The cost difference is therefore **entirely the idle floor** ARC holds and this system does not. We do **not** claim a per-job execution saving; claiming one would be dishonest.
- **A month is 730 hours** (365 × 24 ÷ 12 ≈ 30.4 days).
- Figures ignore second-order differences (image-pull time, control-plane pods) that are small relative to GPU-node cost and roughly equal between the two systems.

---

## F.1. Per-Job Cost Breakdown

Each workflow job consumes resources from three components for distinct time windows:

| Component | When it runs | Cost driver |
|-----------|-------------|-------------|
| AGC goroutine | From session open through job completion | ~60 KiB RSS per goroutine; negligible at any reasonable rate |
| Proxy pod | Always running (HPA-managed, ≥ `minReplicas`) | CPU + memory of the minimum replica pool, plus burst capacity |
| Worker pod | Job duration only | The full cost of the pod's requested CPU, memory, and GPU |

### Worker Pod (Dominant Cost)

For most jobs, the worker pod dominates total cost — especially GPU jobs.

```
worker_cost_per_job = (job_duration_seconds / 3600) × hourly_node_rate × resource_fraction
```

Where `resource_fraction` is the fraction of a node the pod's resource requests consume.

**Example — GPU job:**
- Job duration: 30 minutes (0.5 hr)
- Node type: `p4d.24xlarge`, 8× A100, $32.77/hr
- Pod requests: 1 GPU (⅛ of the node)
- Worker cost: `0.5 × $32.77 × (1/8) = $2.05 per job`

**Example — CPU job:**
- Job duration: 10 minutes (0.167 hr)
- Node type: `m6i.4xlarge`, 16 vCPU, $0.768/hr
- Pod requests: 2 CPU (⅛ of the node)
- Worker cost: `0.167 × $0.768 × (1/8) = $0.016 per job`

### Proxy Pod (Fixed Overhead, Amortized)

The proxy pool runs continuously at `minReplicas`. At low job volumes this is a meaningful cost; at high job volumes it amortizes to near-zero per job.

```
proxy_overhead_per_job = (proxy_monthly_cost) / (jobs_per_month)
```

**Example:** 2 proxy pods at 10m CPU / 32Mi memory on a shared `m6i.4xlarge` pool ≈ 1/800 of the node ≈ $0.001/hr ≈ **$0.70/month**. At 10,000 jobs/month: well under $0.0001 per job — negligible.

### AGC (Negligible)

The AGC is a single pod running on a CPU-only node with modest resources (500m CPU / 2Gi memory request). On an `m6i.4xlarge` that is ~1/32 of the node ≈ $0.024/hr ≈ **$17.5/month**, amortized across all jobs for that tenant. At 1,000 jobs/month: $0.018 per job. At 10,000 jobs/month: $0.0018 per job.

---

## F.2. Cost Comparison: This System vs. ARC

ARC's idle GPU footprint depends heavily on configuration. Compare against the configuration your team actually runs:

| Scenario (10 GPU runner sets, 0 jobs running) | GPU pods alive | Other always-on overhead |
|----------|----------------|--------------------------|
| **ARC scale sets, `minRunners: 0`** | 0 | 10 listener pods (~256 MiB / 1 cluster IP each) on CPU nodes |
| **ARC scale sets, `minRunners: 1`** (common, to mask cold-start latency) | 10 | 10 listener pods on CPU nodes |
| **Legacy ARC `RunnerDeployment`** (per-pod listener) | ≥1 per scale set depending on HRA config | Each runner pod runs its own `Runner.Listener` (~256 MiB) |
| **This system** | 0 | 1 AGC pod (goroutine listener per group, ~60 KiB each) + 1 proxy pool |

**Idle-state cost examples.**

For 10 GPU runner sets, each requiring a dedicated `p4d.24xlarge` (8× A100) node at $32.77/hr:

*Against ARC scale sets with `minRunners: 0`:*
- ARC GPU idle cost: `$0/hr` — GPU pods scale to zero between jobs, same as this system.
- ARC always-on overhead: 10 listener pods × ~256 MiB on CPU nodes — small but real.
- This system always-on overhead: 1 AGC pod with goroutine listeners + 1 proxy pool — typically lower than the listener-pod aggregate at 10+ groups.
- **GPU cost difference at idle: zero.** The advantage is listener footprint and the fact that this system does not need `minRunners > 0` tuning to avoid cold-start latency.

*Against ARC scale sets with `minRunners: 1`:*
- ARC idle cost: `10 nodes × $32.77/hr = $327.70/hr` — 10 GPU pods held to avoid cold-start latency, even when no jobs are running.
- This system idle cost: AGC + proxy pool ≈ `$0.025/hr` — no GPU nodes held; the goroutine listener never goes cold, so no `minRunners > 0` workaround is needed.
- Over a 16-hour off-peak window: ARC idles away `16 × $327.70 ≈ $5,243` per day in GPU charges that this system does not incur.

*Against legacy ARC `RunnerDeployment`*: idle costs are similar to or worse than the `minRunners: 1` scale-set case, depending on HRA configuration. Migrating from legacy ARC produces the largest absolute idle-GPU savings.

**At 100% utilization** (jobs running 24/7 at full concurrency), all configurations hold the same number of GPU pods and the cost difference approaches zero. This system's GPU-cost advantage is proportional to how often the GPU fleet is idle *and* how the comparison ARC deployment is configured.

---

## F.3. Tenant Cost Allocation

Because each tenant has a dedicated namespace with a scoped `ResourceQuota`, per-tenant costs can be attributed precisely.

### Worker Pod Attribution

Label worker pods with the tenant namespace and RunnerGroup at creation time. Query your cloud provider's node usage metrics or use Kubernetes cost attribution tooling (e.g. OpenCost, Kubecost) scoped to the tenant namespace:

```sh
# Approximate: sum of worker pod CPU × memory × duration in namespace
kubectl top pods -n <tenant-namespace> --containers
```

For more precise attribution, use the `actions_gateway_job_duration_seconds` histogram to calculate per-tenant, per-RunnerGroup GPU-time consumed:

```
tenant_gpu_hours = sum(
  rate(actions_gateway_job_duration_seconds_sum[30d])
) by (namespace, runner_group)
/ 3600
```

Multiply by the GPU fraction from the RunnerGroup's pod template to convert to GPU-hours.

### Proxy and AGC Attribution

These are per-tenant fixed costs. Attribute them entirely to the tenant namespace since each tenant has a dedicated proxy pool and AGC. They are small relative to worker pod costs for active tenants.

### Showback vs. Chargeback

- **Showback** (recommended initially): publish per-tenant metrics (GPU-hours, job counts, pod-creation latency) to a shared Grafana dashboard so teams can see their own costs. No billing integration required.
- **Chargeback**: multiply GPU-hours by the cloud rate per GPU-hour, then apply any discount or overhead factor, and invoice the business unit. This requires integrating `job_duration_seconds` with the RunnerGroup's resource request profile and cloud billing data.

---

## F.4. Cost Optimization Levers

### Proxy Autoscaling Aggressiveness

The proxy pool's `minReplicas` is the primary fixed overhead. Set it as low as your availability requirements permit:

- `minReplicas: 1` — single point of failure during updates, but minimizes idle cost.
- `minReplicas: 2` — survives one proxy pod restart without dropped connections (recommended default).
- Higher values — warranted only for tenants with very high sustained job concurrency (hundreds of simultaneous connections).

The `HorizontalPodAutoscaler` handles burst capacity automatically; `minReplicas` only affects the floor.

### Worker Resource Right-Sizing

Oversized resource requests are the most common source of wasted spend. If a job's workflow steps use 2 CPU but the pod requests 4 CPU, the other 2 CPU are reserved but idle.

Remediation:
1. Monitor `container_cpu_usage_seconds_total` for worker pods to measure actual usage.
2. Reduce `requests.cpu` in the RunnerGroup's `podTemplate` to match the observed p95 usage.
3. Apply the same analysis to memory: compare `container_memory_working_set_bytes` to `requests.memory`.

For GPU jobs, ensure the GPU count in `resources.limits.nvidia.com/gpu` matches the job's actual GPU parallelism — requesting 2 GPUs for a single-GPU workload doubles the job cost.

### Job Duration Reduction

Reducing job duration directly reduces worker pod cost. Common levers:
- Cache build artifacts and dependencies across jobs (GitHub Actions cache, persistent volumes).
- Use smaller, targeted runner images with pre-installed tooling.
- Parallelize test steps using a matrix strategy.

The `actions_gateway_job_duration_seconds` histogram per RunnerGroup helps identify which runner shapes have the highest average duration and therefore the highest cost-reduction potential.

### Priority Tier Tuning

Priority tiers are a utilization lever, not only a fairness control. Without the floor guarantee, keeping GPU runners schedulable under a shared quota means reserving idle headroom so a GPU pod always has room — paid-for capacity that sits empty. The floor lets you instead pack the quota with cheap work and admit more runner demand than the floors reserve (safe oversubscription, arbitrated by the Kubernetes scheduler), raising utilization and throughput of the same cluster and lowering cost per job. The headroom you would otherwise hold idle is the saving.

The trade-off is preemption cost. For tenants using `priorityTiers`, a floor tier mapped to a *preempting* `PriorityClass` guarantees GPU floor pods will schedule even under cluster contention — at the cost of potentially evicting lower-priority workloads. Evicted jobs are cheap to re-run; the GPU floor guarantee is typically worth it. Keep the floor tier's `threshold` small (the minimum number of GPU pods that must schedule immediately). Whether a tier preempts is decided by the platform, not the tenant: the `priorityClassName` must be on the GMC `--allowed-priority-classes` allowlist, and the platform sets each `PriorityClass`'s `preemptionPolicy` (defaulting to `Never` so a tenant cannot evict other tenants' pods — see [security-operations.md § Priority classes](../operations/security-operations.md#priority-classes-the-allowed-priority-classes-allowlist)).

---

## F.5. Savings calculator (this system vs ARC)

The saving over ARC is the **idle-runner floor you stop paying for**. Active job time costs the same in both systems — one pod per job, for the job's duration — so it cancels out; what ARC adds on top is `minRunners` held 24/7 (see [§F.2](#f2-cost-comparison-this-system-vs-arc) and the [zero-idle-compute assumption](#f0-rate-sources-and-assumptions)). Enter your own numbers to estimate the monthly saving.

<div class="gag-calc" data-jobs="200" data-duration="12" data-idle="10" data-rate="4.10" data-rate-label="A100 GPU (p4d.24xlarge ⅛)"></div>

**Worked example** — the interactive calculator above defaults to these inputs; on GitHub or without JavaScript, this hand-worked case is the static fallback:

| Input | Value |
|-------|-------|
| Jobs per day | 200 |
| Average job duration | 12 min |
| Idle runners ARC holds (`minRunners` × runner sets) | 10 |
| Cost per runner-hour | $4.10 — one A100 GPU on a `p4d.24xlarge` (⅛ of $32.77/hr) |

Math, over a 730-hour month:

- **Active job-hours/month** = `200 × (12 ÷ 60) × 30.4 ≈ 1,217 hr` → **both** systems pay `1,217 × $4.10 ≈ $4,990/mo`.
- **ARC idle floor** = `10 × $4.10 × 730 ≈ $29,930/mo` — held 24/7 whether or not jobs run.
- **This system pays the active cost only.** Monthly saving = the eliminated idle floor ≈ **$29,930/mo (~$359k/yr)** — about **86%** of ARC's ≈ `$34,920/mo` total.

The saving shrinks toward zero as your fleet approaches 100% utilization (less idle to eliminate) and grows as idle dominates — the same dynamic spelled out in [§F.2](#f2-cost-comparison-this-system-vs-arc). The figure is an estimate from list prices in [§F.0](#f0-rate-sources-and-assumptions); your contracted rates and real utilization will differ.

---

← [Appendix E](appendix-e-capacity-planning.md) | [Back to index](README.md)
