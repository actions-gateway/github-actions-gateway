# Node shutdown budgets across platforms

`terminationGracePeriodSeconds` is a **request, not a guarantee**.
The kubelet grants a terminating pod `min(terminationGracePeriodSeconds, whatever the node shutdown path allows)`, and that second number varies by platform, by node type, and by *why* the pod is going away.
On some platforms it is 15 seconds.

This page collects the defaults so you can size shutdown budgets against what your clusters actually grant, rather than against what the pod spec asks for.
It is reference material — for what the gateway's own components do on SIGTERM, see [Proxy Tunnel Cut During a Rollout](troubleshooting.md#proxy-tunnel-cut-during-a-rollout).

!!! warning "The short version"

    A pod's grace period is honoured in full during a **voluntary** drain (`kubectl drain`, autoscaler scale-down, node pool upgrade) and truncated hard during an **involuntary** one (spot reclamation, node shutdown).
    The gateway's proxy asks for 60s; on a GKE Spot node it will get 15.

---

## Why the same pod gets different budgets

Three different code paths terminate pods, and they honour the grace period differently:

| Path | Who drives it | Grace period |
|---|---|---|
| **Eviction / drain** — `kubectl drain`, autoscaler scale-down, node pool upgrade | API server eviction, then kubelet | Pod's own `terminationGracePeriodSeconds`, subject to the drainer's own timeout |
| **Graceful node shutdown** — the OS is shutting down and the kubelet noticed | kubelet Node Shutdown Manager, via a systemd inhibitor lock | **Capped by `shutdownGracePeriod`**, which is usually far smaller than the pod's ask |
| **Non-graceful shutdown** — the kubelet never noticed | nobody | None. Processes die with the node |

The middle row is the one that surprises people, and it is the row spot instances land in.

---

## Graceful node shutdown defaults

`shutdownGracePeriod` is the **total** node budget; `shutdownGracePeriodCriticalPods` is carved out of it for pods with `system-cluster-critical` / `system-node-critical` priority.
Ordinary workloads get the difference.

| Platform | Enabled by default? | Total | Ordinary pods get |
|---|---|---|---|
| **Upstream kubelet** | Feature gate on since 1.21, but **functionally off** — both values default to `0` | 0 | 0 (no delay at all) |
| **GKE** | **Yes** | 30s | **15s** (15s reserved for critical) |
| **EKS** (AL2 / AL2023) | No — inherits the upstream `0` default; set it in kubelet config | 0 | 0 |
| **EKS** (Bottlerocket) | **Not supported** — see the gotcha below | — | — |
| **AKS** | No kubelet-level default; Azure delivers a `Preempt` Scheduled Event instead, which nothing drains on unless you deploy a handler | — | — |
| **RKE2** | No — upstream default; set via `/var/lib/rancher/rke2/agent/etc/kubelet.conf.d/` | 0 | 0 |
| **Kubespray** | No — upstream default; set via kubelet config vars | 0 | 0 |
| **OpenShift** | No — upstream default; set via a `KubeletConfig` CR, which renders a MachineConfig | 0 | 0 |

Two consequences worth internalising:

- **On most self-managed distributions the feature is off**, so an OS-level shutdown kills pods outright with no SIGTERM at all.
  That is *worse* than a short budget, and it is the default on RKE2, Kubespray, OpenShift and stock EKS.
- **GKE is the outlier that enables it**, and its 15s for ordinary pods is the tightest real budget in the table.

!!! danger "Bottlerocket cannot do graceful node shutdown"

    The kubelet implements graceful node shutdown with **systemd inhibitor locks**, which live in `systemd-logind`.
    Bottlerocket does not ship `systemd-logind`, so the mechanism is unavailable: on node shutdown, pods are not gracefully terminated regardless of `shutdownGracePeriod` or `terminationGracePeriodSeconds`.
    If you run Bottlerocket, an interruption handler that cordons and drains *before* the shutdown begins is the only graceful path.

### Raising it on GKE

GKE can extend the total to **120s** (`shutdownGracePeriodSeconds` / `shutdownGracePeriodCriticalPodsSeconds` in a node-pool kubelet config).
120s is a hard ceiling — **no pod on the node can get more, whatever its `terminationGracePeriodSeconds` says**.
At the time of writing this is in Preview and needs GKE 1.35.0-gke.1171000+ on Standard clusters.

---

## Preemption notice windows

The node shutdown budget can never exceed the notice the cloud gives:

| Provider | Notice before reclamation |
|---|---|
| **GCE** Spot / preemptible | 30s |
| **Azure** Spot | 30s minimum (`Preempt` Scheduled Event, polled from IMDS at `169.254.169.254`) |
| **AWS** EC2 Spot | 2 minutes |

AWS's two minutes is roomy enough that a 60s pod budget fits.
GCE's and Azure's 30s are not — and on GKE the pod's own share of that 30s is 15s.

---

## Voluntary drain timeouts

These paths *do* honour `terminationGracePeriodSeconds`, bounded by the drainer's own patience:

| Drainer | Timeout | Notes |
|---|---|---|
| `kubectl drain` | **None** by default (`--timeout=0` waits forever) | |
| **cluster-autoscaler** scale-down | **600s** (`--max-graceful-termination-sec`) | Node is deleted anyway after this |
| **Karpenter** | Honours pod `terminationGracePeriodSeconds`; `spec.template.spec.terminationGracePeriod` on the NodePool sets an upper bound (unset ⇒ wait) | Default disruption budget is 10% of nodes |
| **EKS** managed node group update | **15 minutes** to drain, then `PodEvictionFailure` unless forced | |
| **GKE** node pool upgrade / delete | Honours the max pod `terminationGracePeriodSeconds` on the node, capped at **1 hour**; PDBs respected up to 1 hour | |

A 60s pod budget is comfortably inside every row here.
The problem is never the voluntary path.

---

## What this means for the gateway

The egress proxy asks for `terminationGracePeriodSeconds: 60` and runs its whole shutdown sequence — endpoint-removal linger, then in-flight CONNECT tunnel drain — inside a single 45s budget (`PROXY_SHUTDOWN_DRAIN_TIMEOUT`).

| Where the proxy runs | Budget it actually gets | Verdict |
|---|---|---|
| On-demand nodes, any voluntary drain | Full 60s | Fine |
| AWS EC2 Spot | ~120s window, 60s honoured if a handler drains in time | Usually fine |
| Azure Spot | 30s notice, and nothing drains without a handler | Degraded |
| **GKE Spot** | **15s** | **Drain is cut short every time** |

Nothing breaks in the degraded rows — the sequence still runs in the right order, it is simply cut short, and tunnels still open get force-closed with a logged warning rather than silently severed.
But the drain cannot do its job in 15 seconds.

### Recommendation: keep proxy pools off spot capacity

This is a recommendation, not an enforced constraint — it is your call, against your own tolerance for disruption.

The egress proxy is **shared infrastructure for every worker in the tenant**, and its IP is the tenant's [egress identity](../design/02-architecture.md).
Losing a proxy replica is categorically different from losing a worker:

- A reclaimed **worker** costs one CI job, which re-runs.
- A reclaimed **proxy** cuts live egress for every job currently routed through it, and on a tight budget the drain cannot get out of the way cleanly.

Pin the pool to on-demand capacity with `spec.scheduling` (nodeSelector, tolerations, affinity — see [tenant onboarding](tenant-onboarding.md)).
Workers are the right place to spend spot savings; the proxy is not.
This is exactly the split the dogfood cluster uses: a spot `workers` pool tainted `dedicated=workers:NoSchedule` so control-plane and proxy pods cannot land on it.

If you accept the risk and run proxies on spot anyway, **disable** the endpoint-removal linger so the whole truncated window goes to draining tunnels:

```sh
# Spend the entire (short) window on the tunnel drain.
PROXY_SHUTDOWN_LINGER=-1s
```

Disable rather than shorten.
On this path the linger provably cannot do its job (see [the measurement below](#measured-on-graceful-node-shutdown-nothing-removes-the-endpoint-q388)): the endpoint stays in rotation until the pod is dead, so arrivals never stop and the linger runs to its ceiling no matter how the ceiling is set.
Any time given to it is time taken from the drain.

Set it on the `EgressProxy` / `ActionsGateway` spec rather than with `kubectl set env` — the GMC owns the Deployment and reconciles direct edits away.

---

## Why a delay is needed at all, and what Kubernetes does not give you

Endpoint removal is **concurrent with SIGTERM, not ordered before it**.
Marking the pod terminating, removing it from EndpointSlices, and each kube-proxy applying that removal are independent control loops.
A pod can therefore receive SIGTERM while traffic is still being routed to it.

**There is no signal for "endpoint removal has propagated."** No API, no notification, no readback aggregated across the N kube-proxies applying it.
This is the root reason the ecosystem standardised on a `preStop` sleep: it is not elegance, it is the absence of anything better.

### Upstream state of the art

This is a known, accepted, unsolved gap — not an oversight, and not something to re-derive.
Three upstream issues surround it; check them before assuming anything here has changed:

| Issue | What it covers | State |
|---|---|---|
| [kubernetes#106476](https://github.com/kubernetes/kubernetes/issues/106476) | The race itself: SIGTERM is delivered before load balancers and ingress controllers have processed the endpoint removal. The recurring ask is a **"termination gate"** — readiness gates, but for deletion | Open since 2021, `triage/accepted`, `lifecycle/frozen` |
| [kubernetes#116965](https://github.com/kubernetes/kubernetes/issues/116965) | Under **graceful node shutdown** the pod reportedly never enters `Terminating` and is not removed from EndpointSlices until it reaches `Completed`/`Failed` | Open since 2023, `triage/accepted`, `priority/important-longterm` |
| [kubernetes#124648](https://github.com/kubernetes/kubernetes/issues/124648) | *Related, narrower scope than often cited:* "Readiness probe stops too early at **eviction**" — the probe worker is halted when eviction sets the pod phase to `Failed` before containers stop. The reporter states the probe works during termination on ordinary delete. It does **not** cover the delete-path behaviour measured below (probe runs and fails, `Ready` never flips) | Open since 2024, `triage/accepted`, `priority/backlog` |

No KEP exists for the termination gate.
The blocking design question, per SIG Network maintainers on 106476, is *"who do we wait for?"* — a pod may sit behind several ingresses, gateways and service LBs, some unreachable from inside the cluster, and deletion cannot be un-started once begun.
Synchronising state back from every kube-proxy on every endpoint change is explicitly considered infeasible at scale.

### Measured: on graceful node shutdown, nothing removes the endpoint (Q388)

Both termination paths — ordinary delete, and the 116965 graceful-node-shutdown case — were measured on kind, Kubernetes **1.35.0**, with the kubelet configured GKE-style (`shutdownGracePeriod: 30s`, `shutdownGracePeriodCriticalPods: 15s`) and a two-replica Service whose pods fail readiness the instant they get SIGTERM.

**Ordinary `kubectl delete`.** The readiness probe (1s period, failure threshold 1) began failing at SIGTERM and kept failing.
The pod's `Ready` condition stayed `True` for the entire 48-second termination and never flipped.
The endpoint *was* withdrawn immediately, but by the `deletionTimestamp`, not by readiness. **The probe runs and fails, but its result never reaches the `Ready` condition — endpoint removal is driven by `deletionTimestamp` regardless.**

Note this is *adjacent to but distinct from* 124648, which is scoped to the eviction path and whose reporter states the probe works during termination on ordinary delete.
On this evidence the delete-path behaviour (probe results never reflected into `Ready` during termination) appears to be unreported upstream — Q401 tracks filing it.

**116965, graceful node shutdown.** With the node powering off:

| Time | Node | Pods | Endpoints |
|---|---|---|---|
| t+0 to t+14 | `NotReady` | `Running`, **no deletionTimestamp** | **both still `ready=true`** |
| t+16 | `NotReady` | `Failed` | gone |

The pods never entered `Terminating` and never received a `deletionTimestamp`.
The endpoints stayed `Ready` for the full ~15s ordinary-pod budget and were withdrawn only once the pods reached a terminal phase, which is to say *after they were already dead*.

**Together these mean the endpoint stays in rotation for the whole graceful-node-shutdown window.** New connections keep arriving, so the proxy's linger never observes quiescence and burns its entire ceiling.
On a 15s GKE Spot budget that leaves roughly 5s for the tunnel drain.

!!! warning "On spot capacity, disable the linger"

    Set `PROXY_SHUTDOWN_LINGER=-1s` so the whole truncated window goes to draining tunnels.
    The linger cannot do its job on that path, and this is a measured result rather than an inference.

The upstream design intent is exactly what the proxy does.
A SIG Node maintainer on 116965: *"the Kubernetes-portable behavior is to use the readiness endpoint to control being in rotation during graceful shutdown."* Failing `/readyz` is correct and stays.
It simply does not work yet: as measured above (Q388), the probe's failures never reach the `Ready` condition on the delete path — and on eviction, 124648 halts the probe worker outright.

!!! note "Reproducing this yourself: kind needs dbus first"

    Stock kind **cannot** run graceful node shutdown.
    The kubelet logs `Failed to start node shutdown manager: dial unix /var/run/dbus/system_bus_socket: no such file or directory`, because the node image ships no dbus at all. `apt-get install dbus`, start `dbus` and `systemd-logind`, restart the kubelet, and confirm with `systemd-inhibit --list` that the kubelet holds a `shutdown` lock before trusting any result.
    This is the same missing-`systemd-logind` mechanism that disables the feature on Bottlerocket.

### What the newer traffic-engineering features do and don't fix

`ProxyTerminatingEndpoints` (KEP-1669, stable since **1.28**; EndpointSlice `terminating` conditions GA in **1.26**) is frequently cited as having solved this.
It has not, for the multi-replica case.
The rule is a **fallback**, not a preference — kube-proxy picks the first non-empty set of:

1. ready endpoints that are **not** terminating,
2. ready endpoints that **are** terminating.

So a terminating pod receives traffic only when it is the *last* one standing.
With a proxy pool of two or more healthy replicas, set (1) is never empty and the feature does nothing for the pod being rolled.
It rescues the single-replica / `internalTrafficPolicy: Local` case, which is real and worth having — just not this one.

### The `preStop` sleep, if you use one

- Use the **native `sleep` handler** (KEP-3960: beta and on by default in **1.30**, GA'd after a reverted attempt in 1.32). `exec: ["sleep", …]` fails at runtime on distroless images — no shell, no `sleep` binary — and the pod proceeds straight to SIGTERM, silently reintroducing the race.
  Zero-valued sleeps became usable in **1.33** (KEP-4818), which makes the field templatable.
- Community guidance clusters at **5–15s**.
  Upstream examples use 15s.
- Remember it is **deducted from** `terminationGracePeriodSeconds`, not added to it — and on a truncated budget it is deducted from a number much smaller than the manifest says.

### Why the gateway measures instead

The proxy holds its CONNECT listener open after SIGTERM and watches its own **new-connection arrivals**, exiting once none has arrived for a short quiescence interval.
Arrivals stopping is the property a `preStop` sleep is approximating, and the server can observe it directly — a local signal where no cluster-wide one exists.
Because the wait is spent *inside* the drain budget rather than ahead of it, worst case is `max(linger, drain)` rather than their sum, which is what keeps a truncated window spending its seconds on draining rather than idling.
See [kubernetes-conventions](../development/kubernetes-conventions.md#graceful-shutdown-sigterm) for the pattern and the reasoning.

---

## Checklist for sizing a budget

- [ ] Which of the three termination paths will actually hit this pod most often?
- [ ] Does this platform even have graceful node shutdown enabled?
  (On RKE2, Kubespray, OpenShift and stock EKS: no, unless you configured it.)
- [ ] Is the workload on spot/preemptible capacity, and is that appropriate for what it does?
- [ ] Does the drain still do something useful when truncated to 15s?
- [ ] Is there a `preStop` sleep eating into an already-truncated budget?

---

## Sources

Vendor and upstream documentation, retrieved 2026-07-22.
Defaults change — re-check before relying on a number for capacity planning.

- [Node Shutdowns — Kubernetes](https://kubernetes.io/docs/concepts/cluster-administration/node-shutdown/)
- [Spot VMs — GKE](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/spot-vms)
- [Node upgrade strategies — GKE](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/node-pool-upgrade-strategies)
- [Understand each phase of node updates — Amazon EKS](https://docs.aws.amazon.com/eks/latest/userguide/managed-node-update-behavior.html)
- [Does EKS Bottlerocket support graceful node shutdown? — bottlerocket-os/bottlerocket#3291](https://github.com/bottlerocket-os/bottlerocket/discussions/3291)
- [Build workloads with Azure Spot Virtual Machines — Microsoft Learn](https://learn.microsoft.com/en-us/azure/architecture/guide/spot/spot-eviction)
- [Graceful node shutdown — rancher/rke2#4395](https://github.com/rancher/rke2/discussions/4395)
- [cluster-autoscaler FAQ](https://github.com/kubernetes/autoscaler/blob/master/cluster-autoscaler/FAQ.md)
- [Disruption — Karpenter](https://karpenter.sh/docs/concepts/disruption/)
- [KEP-1669: Proxy terminating endpoints](https://github.com/kubernetes/enhancements/blob/master/keps/sig-network/1669-proxy-terminating-endpoints/README.md)
- [KEP-3960: Sleep action for preStop hook](https://github.com/kubernetes/enhancements/blob/master/keps/sig-node/3960-pod-lifecycle-sleep-action/README.md)
- [Kubernetes v1.33: Updates to container lifecycle](https://kubernetes.io/blog/2025/05/14/kubernetes-v1-33-updates-to-container-lifecycle/)
- [Kubernetes v1.26: Advancements in traffic engineering](https://kubernetes.io/blog/2022/12/30/advancements-in-kubernetes-traffic-engineering/)
