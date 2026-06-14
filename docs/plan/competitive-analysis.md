# Competitive analysis notes — GAG vs ARC (working notes for Q60)

**Status: unverified working notes.** Distilled from a product discussion on
2026-06-13 while reworking the website benefits. These are the claims we *think*
are true; [Q60](../STATUS.md) is the place to verify each against ARC's current
behavior/docs and fold confirmed conclusions into
[appendix-d](../design/appendix-d-alternatives-considered.md). The website states
only the high-confidence subset (flagged below); everything marked **VERIFY**
stays off the site until checked.

## The overarching theme: cost

The single most important framing is **lower cost**, reached three ways:

- **Fewer always-on resources** — combined goroutine listeners instead of a pod
  per scale set (memory).
- **No idle pods** — scale to zero between jobs, which matters most for
  expensive GPU and e2e runners.
- **Guaranteed throughput instead of blocked critical jobs** — minimum
  per-type thresholds keep critical work moving rather than stalling on a full
  cluster.

Most individual benefits below ladder up to this. Lead the website with it.

## Automatic eviction retry — likely the strongest differentiator

- ARC plays poorly with Kubernetes namespace `ResourceQuota`: when a job can't
  schedule because quota is exhausted, it fails and needs a **manual** rerun.
- That makes `ResourceQuota` risky to use with ARC, which in turn makes it risky
  to let tenants self-manage their own max-runner counts.
- GAG fast-cancels the GitHub job lock and reruns automatically. Solving this one
  problem is what unlocks **platform-level quota management** → safe, true
  **tenant self-service** without compromising cluster-level allocations.
- **VERIFY:** ARC's exact behavior on a quota-exhausted scheduling failure (no
  retry? listener requeue? ephemeral-runner failure mode). Also clarify GAG's two
  distinct paths and that *both* are covered: (a) the provisioner's in-place
  **quota-rejection retry** (`maxQuotaRetries`, holds the lock) and (b) the Job
  Lock Renewer's **eviction retry** (rerun-failed-jobs). Cross-ref [Q59](../STATUS.md)
  (pre-acquire capacity gate).
- **Website: high confidence** (our design) — state the quota→self-service story.

## Priority-tiered scheduling

- Reserve "at least N" of each runner type so none gets completely blocked on a
  full cluster. You can burn down many heterogeneous queues at once (short/long,
  big/small) so a PR's full test battery finishes, instead of big expensive tests
  starving behind a flood of small queued ones.
- Critical because **most repos run a battery of different tests on each PR.**
- **VERIFY:** ARC has no per-quota "floor" primitive (confirm). Whether the
  effect is approximable in ARC via separate scale sets + `PriorityClass`es, and
  how Kueue compares (borrowing/quota/priority) — cross-ref the Kueue angle in
  [Q60](../STATUS.md)/[Q59](../STATUS.md).
- **Website: high confidence** — state the "at least N, no blocked critical
  jobs, PRs still finish" framing.

## Scale to zero (esp. GPU / e2e)

- Most valuable for **expensive GPU jobs and e2e tests** — return the node to the
  scheduler the moment a job finishes.
- **ARC can also scale to zero** (`minRunners: 0`) — but not everyone knows that.
  GAG's edge is that it's the default and you don't pin idle runners to mask
  cold-start latency (which silently reintroduces idle GPUs).
- So this is a **shared capability framed better**, not a unique moat. Be honest.
- **VERIFY:** real-world cold-start latency ARC vs GAG; how often teams actually
  pin `minRunners > 0` on GPU pools.
- **Website: high confidence, stated honestly** (acknowledge ARC can too).

## Lower listener overhead

- ~60 KiB goroutine per runner group in one shared pod vs ~256 MiB .NET listener
  pod per scale set (~600 KiB vs ~2.5 GiB across ten groups). Matters with
  today's high memory cost — but **far less important than avoiding idle GPUs.**
- Subtle extra angle: combined listeners mean you don't schedule a **new listener
  pod per scale set**, which could help latency — especially if the cluster is
  full and a fresh ARC listener would go `Pending`.
- **Counterpoint (investigate):** if the cluster is too full for a listener pod,
  it's probably too full for a runner pod too, so the latency win may be
  marginal. **VERIFY** whether ARC puts listener scheduling on the critical path
  under contention in a way that actually adds job latency.
- **Website: memory numbers only** (high confidence). Keep the latency hypothesis
  off the site until investigated.

## Per-tenant egress IP pool

- Enables **allow-listing with GitHub EMU**: only your runners can reach the EMU
  servers, without allow-listing the entire cluster or standing up a NAT gateway.
  Put proxies on a small node pool with known IPs and allow-list just those.
- Especially useful when the allow-list has a **max IP count** and the nodes'
  public IPs aren't contiguous (so a simple CIDR won't work).
- **Noisy-neighbor isolation:** if one tenant's runners get flagged as spam or
  throttled, tenants with their own egress IPs (own egress node pool / separate
  public IPs) are unaffected.
- Ambitious extension: per-tenant proxies on specific node pools → **separate NAT
  gateways per tenant** when using private node IPs.
- **VERIFY:** EMU allow-list mechanics and any max-IP limit; confirm ARC has no
  native egress-isolation story. Document the node-pool / NAT patterns.
- **Website: high confidence for the EMU allow-list + noisy-neighbor framing;**
  keep the NAT-per-tenant pattern as a docs detail, not a landing claim.

## Self-service onboarding

- **Mostly a consequence of the quota unlock above**, not a standalone win — ARC
  already has namespace-scoped autoscaling runner sets.
- Mild convenience: manage a *group* of runner sets with a single CR.
- **Caveat / VERIFY:** many complex `podTemplate`s in one CR could hit the etcd
  object size cap (~1.5 MiB). May need sharding guidance (we already advise
  sharding across CRs beyond ~250 sessions — see appendix-a). Call this out as a
  known scaling limit.
- **Website: fold into the quota/self-service tile**, don't feature separately.

## Per-tenant utilization metrics

- Prometheus metrics scoped per tenant and runner group.
- **Uncertain moat.** ARC should be able to do this too — **VERIFY** whether ARC
  already exposes per-scale-set/per-tenant metrics, or could with a flag. Decide:
  real moat, just easier in GAG, or temporary until ARC adds parity.
- **Website: state modestly** ("scoped per tenant out of the box"); do not claim
  ARC can't.

## Cross-cutting open questions (for Q60)

- etcd object-size cap for large multi-group CRs (sharding guidance).
- Listener-scheduling-latency-under-contention hypothesis (may be marginal).
- ARC per-tenant metrics parity.
- Kueue as an alternative for the priority/quota story (cross-ref [Q59](../STATUS.md)).
