# Kata Containers on GKE — Secure CI Reference Architecture

GAG's own e2e CI suite uses `kind create cluster` inside a runner pod (Docker-in-Docker). That means the runner needs a Docker daemon. The obvious solution — privileged DinD — is unacceptable for an OSS project: external contributors open PRs and CI runs their code, making the runner a direct target for "pwn request" attacks. GAG must also dogfood its own isolation model. This plan scopes a spike to validate Kata Containers on GKE as the right path, then build a reusable reference architecture that users with the same requirement (regulated environments, multi-tenant shops, public OSS projects) can follow.

**Status at a glance**

| Phase | Status |
|---|---|
| Spike — prove Kata + GKE nested-virt + kind works end-to-end | ❌ Not started |
| CI integration — replace privileged DinD in e2e-test.yml | ❌ Blocked on spike |
| Reference architecture doc — `docs/operations/kata-ci.md` | ❌ Blocked on spike |

---

## Motivation

Three independent reasons converge on this work:

### 1. OSS "pwn request" threat

GAG is a public OSS project. Any contributor can open a PR, and GitHub Actions CI runs
their code on GAG infrastructure. Privileged DinD in a runner pod means:

- The pod can write to `/proc` and `/sys` on the host node.
- Node service account tokens are reachable via the GKE metadata server from inside the
  pod — a direct path to cluster-scoped credentials.
- A compromised runner can pivot to other tenant namespaces if network policy is not
  perfectly airtight.

This is the "pwn request" attack class, actively exploited against OSS projects. GitHub's
mitigations (approval gates for first-time contributors, `pull_request` vs
`pull_request_target` scoping) are process controls, not isolation. They reduce but do not
eliminate the risk.

### 2. GAG must dogfood its own security model

GAG's core value proposition is secure multi-tenant runner isolation. Running GAG's own CI
in privileged DinD would mean:

- The project claims secure isolation but does not use it for its own workloads.
- The privileged-DinD path is implicitly endorsed as acceptable for users who need kind
  inside a runner.

Both undermine the product. GAG's CI runners should use the same isolation model GAG
provides to tenants — or a stricter one.

### 3. Reference architecture for users

Many GAG users have the same requirement: run kind (or Docker builds) inside a self-hosted
runner without `privileged: true`. This includes:

- Regulated environments (FedRAMP, SOC 2, PCI) where privileged containers are
  prohibited or require compensating controls.
- Multi-tenant clusters where operator policy blocks privileged pods cluster-wide.
- Other OSS projects that want to run their own e2e CI through GAG.

A validated, documented reference architecture turns a one-off internal fix into a
reusable deliverable. It also differentiates GAG from ARC: ARC users typically accept
privileged DinD; GAG provides a secure path.

---

## Why not the other options?

**Sysbox** — `nestybox/sysbox#920` (opened March 2025) documents that kind inside Sysbox
breaks for K8s v1.25+ node images. The only kind-specific fix in the Sysbox changelog was
v0.5.0 (March 2022, fixing #415). v0.6.1 (April 2023) added K8s 1.24–1.26 support in the
`sysbox-deploy-k8s` installer but contains no changelog entry for kind with 1.25+ node
images, and issue #920 post-dates it. Claims that a Sysbox v0.7.0 released in June 2026
resolves this were adversarially checked and refuted (no such release found). Docker
acquired Nestybox in May 2022 and development has slowed sharply. Contributing a fix would
take 4–8 weeks of low-level systems work with uncertain upstream acceptance and indefinite
fork-maintenance cost.

**kindbox** — Nestybox's own Sysbox-aware kind replacement is a bash script wrapper
explicitly documented as "a reference example, not a replacement for kind." Last commit:
2021-10-12. No `kind load docker-image` equivalent. Calico CNI (which GAG's e2e uses)
requires Sysbox-EE (enterprise edition), which was archived in May 2022 at
`docker-archive/nestybox.sysbox-ee` after the Docker acquisition and has received no
releases since.

**Rootless Docker + rootless kind** — Requires cgroup v2 on the host node and four
iptables kernel modules pre-loaded: `ip_tables`, `iptable_nat`, `ip6_tables`,
`ip6table_nat`. Doable on GKE COS nodes but requires a privileged DaemonSet to
`modprobe` the modules — the runner pod stays unprivileged but the setup requires node
surgery. Lower isolation gain than Kata (shared kernel vs. micro-VM).

**Kata Containers** — Runs each pod inside a lightweight VM via an OCI-compatible
`RuntimeClass`. The pod itself requires no `privileged: true`; isolation is enforced at
the hypervisor layer. Inside the Kata VM, Docker and kind run natively with no DinD
tricks. This is the highest-security option available on GKE.

> **Common confusion:** GKE's nested-virtualization documentation mentions
> `securityContext.privileged: true` in some contexts. That requirement applies to pods
> that interact *directly* with the nested hypervisor (e.g. launching their own VMs). A
> pod that uses `runtimeClassName: kata` does not do this — the Kata shim handles VM
> lifecycle outside the pod. The runner pod runs without any privileged context.

---

## Technical approach

GKE nodes are themselves VMs (on GCE). To run VMs inside them (as Kata requires), GKE
must be configured with nested virtualization on the node pool. This is a node-level config
— the runner pod does not need `privileged: true`.

The setup has three layers:

```
GKE node (GCE VM, nested-virt enabled)
  └── kubelet hands pod to kata-containerd shim
       └── Kata micro-VM (QEMU or Cloud Hypervisor)
            └── runner container
                 └── dockerd (not DinD — no special flags)
                      └── kind cluster (kind node containers)
                           └── GAG e2e tests
```

Key properties:
- Runner pod: `runtimeClassName: kata`, no `privileged`, no `allowPrivilegeEscalation`
- Docker daemon inside the runner: standard `dockerd`, no `--insecure` or `--privileged`
- kind node containers: standard `kindest/node` images, no Sysbox patches needed
- Node-level: nested virtualization enabled in the GKE node pool config

**Why kind-in-runner rather than a shared test cluster.** An alternative would run the
e2e suite against a pre-provisioned GKE cluster rather than spinning up kind inside each
runner pod. This eliminates the Docker-in-runner requirement but breaks parallel PR
testing: CRDs, webhooks, and ClusterRoles are cluster-scoped, so concurrent runs collide
unless each gets a fully isolated API server (e.g. via vcluster). kind-in-runner avoids
this entirely — each CI run gets its own cluster, and any number of PRs can run
simultaneously without coordination. For a project developed with multiple concurrent
sessions this parallelism property is load-bearing.

---

## Spike acceptance criteria

The spike is a go/no-go gate. Timebox: one week. Accept if all of the following hold:

1. A GKE Standard node pool with nested virtualization enabled and Kata Containers
   installed can be provisioned with documented steps.
2. A runner pod with `runtimeClassName: kata` (and no privileged context) starts
   successfully and a `dockerd` daemon runs inside it.
3. `kind create cluster` (using the same `kindest/node` image version as GAG's e2e suite)
   completes inside the runner pod.
4. `kind load docker-image` loads an image into the cluster.
5. The GAG e2e suite (`make e2e`) passes inside the runner pod on that cluster.
6. Wall-clock time for `kind create cluster` inside Kata is ≤ 3× the current baseline
   (currently ~2 min → ceiling ~6 min).

If any criterion fails and there is no known fix within the timebox, the spike outcome is
documented and the recommendation reverts to privileged DinD on a dedicated locked-down
node pool (with the security rationale documented so the trade-off is explicit).

---

## Reference architecture deliverable

The spike validates the approach on GKE, but the reference architecture
(`docs/operations/kata-ci.md`) is provider-agnostic. It covers three tiers:

**Tier 1 — cloud-hosted (GKE, AKS, EKS).** Nested-virtualization node pool + Kata
RuntimeClass. Variant-specific guidance per provider: machine family requirements
(n2/n2d/c2/c2d on GKE), Standard vs. Autopilot trade-offs (Autopilot blocks nested
virt), Kata DaemonSet installer vs. managed add-on. Best fit for teams already
cloud-native.

**Tier 2 — bare metal and on-prem.** Kata on real hardware requires no nested
virtualization — QEMU or Cloud Hypervisor runs directly. No machine-family constraints,
lower overhead, and the correct path for GPU workloads: PCIe passthrough of NVIDIA or AMD
GPUs into the Kata micro-VM works from bare metal. GKE's GPU machine families (a2, a3,
g2) do not support nested virtualization, so GPU + Kata on cloud requires bare-metal or
dedicated instances. This tier is the reference architecture for teams running GPU CI on
owned hardware or cost-sensitive on-prem environments.

**Tier 3 — pragmatic fallback (any provider).** Privileged DinD on a dedicated,
locked-down node pool. Documents compensating controls explicitly: workload-identity
scope-down, metadata-server block, network policy, node taint isolation. For teams where
Kata is not feasible but full privilege is also unacceptable.

Each tier covers: pod security context, RuntimeClass or equivalent, node requirements,
`ActionsGateway` CR configuration to target the right pool, observed startup overhead,
and CI timeout guidance.

---

## CI integration (post-spike)

Once the spike validates the approach, `e2e-test.yml` changes:

1. Add a step to provision/verify the Kata node pool (or assert it exists via a
   pre-provisioned permanent node pool).
2. Replace the `kind create cluster` step with one that targets the Kata node pool.
3. Remove any `privileged: true` from the runner pod spec.
4. Update `docs/development/testing.md` to document the new runner requirements.

The Kata node pool can be a permanent fixture in the CI GKE project (provisioned once via
Terraform / `gcloud` config, not per-run) to avoid the ~5–10 min node pool create/delete
overhead on every CI run.
