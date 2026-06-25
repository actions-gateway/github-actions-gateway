# Migrating from Actions Runner Controller (ARC) to GitHub Actions Gateway (GAG)

> **Audience:** Platform engineer running [Actions Runner Controller (ARC)](https://github.com/actions/actions-runner-controller) scale-set mode on a shared, multi-tenant Kubernetes cluster and evaluating GitHub Actions Gateway (GAG) as a replacement.

This guide maps ARC scale-set concepts onto GAG, calls out the behavioral
differences you will actually hit, and walks one runner group from ARC to GAG
end to end. It assumes you already run ARC's `gha-runner-scale-set` chart; if you
are new to GAG, read [Why GAG](../why-gag.md) and
[Getting Started](../getting-started.md) first, then come back here.

The good news up front: GAG was designed to make this migration cheap. The worker
pod template is the **same Kubernetes type** ARC uses, so your pod spec transfers
with no schema translation, and ARC and GAG can run **side by side** on the same
cluster — you migrate one runner group at a time and roll back by pointing
`runs-on` back at the old labels.

**Scope.** This covers ARC's **scale-set mode** (the `AutoscalingRunnerSet` /
`gha-runner-scale-set` chart — GitHub's current and recommended mode). Legacy ARC
(`RunnerDeployment` + `HorizontalRunnerAutoscaler`) maps similarly but its
per-pod listener and label model differ; the [job-routing](#job-routing-the-one-that-bites)
and [concept-mapping](#concept-mapping-arc--gag) sections note where legacy ARC
diverges.

---

## The mental model

In ARC scale-set mode, **each scale set is its own controller surface**: one
`AutoscalingRunnerSet` CR, one long-running `.NET` listener pod, its own
`maxRunners` cap, configured by its own Helm release. Ten runner types means ten
scale sets, ten listener pods, ten Helm releases.

In GAG, **one tenant declares one `ActionsGateway`** and lists its runner types as
`runnerGroups[]` entries inside it. A single per-tenant controller — the Actions
Gateway Controller (AGC) — multiplexes every group's listener as a goroutine
(~60 KiB) in one shared pod, instead of one ~256 MiB listener pod per scale set.
The platform installs the Gateway Manager Controller (GMC) **once**; from there a
tenant's whole gateway (controller, egress proxy pool, RBAC, NetworkPolicies) is
provisioned from that single CR.

So the shape of the migration is: **N ARC scale sets in a namespace → 1
`ActionsGateway` with N `runnerGroups` in that namespace.**

```
ARC (scale-set mode)                    GAG (v1 API)
─────────────────────                   ─────────────────────────────
namespace: team-a                       namespace: team-a
  AutoscalingRunnerSet "cpu"              ActionsGateway "team-a-gateway"
    └─ listener pod (~256 MiB)              runnerGroups:
  AutoscalingRunnerSet "gpu"                 - name: cpu   (goroutine listener)
    └─ listener pod (~256 MiB)               - name: gpu   (goroutine listener)
  AutoscalingRunnerSet "arm"                 - name: arm   (goroutine listener)
    └─ listener pod (~256 MiB)             └─ one AGC pod multiplexes all three
  (shared cluster egress)                  └─ per-tenant egress proxy pool
```

> **One gateway per namespace (v1).** The v1 API allows exactly one
> `ActionsGateway` per namespace — all your runner groups for a tenant live under
> it. If you genuinely need multiple independent gateways in one namespace, that
> is a v2 capability; see [the v2 note](#v2-differences-worth-knowing) and the
> [v1 → v2 migration guide](migration-v1-to-v2.md).

---

## Concept mapping: ARC → GAG

| ARC scale-set concept | GAG equivalent | Notes |
|---|---|---|
| `AutoscalingRunnerSet` (one per runner type) | one `runnerGroups[]` entry in the `ActionsGateway` CR | N scale sets collapse into one CR with N entries. |
| `gha-runner-scale-set` Helm release per scale set | one `ActionsGateway` CR for the whole tenant | You stop managing a Helm release per runner type. The GMC is the only Helm install ([Install](install.md)). |
| `gha-runner-scale-set-controller` (cluster controller) | Gateway Manager Controller (GMC) | Installed once by the platform; provisions every tenant's AGC. |
| The `.NET` `AutoscalingListener` pod (one per scale set) | a goroutine inside the shared per-tenant AGC pod | ~256 MiB/pod → ~60 KiB/goroutine; no per-listener cluster IP. |
| `githubConfigUrl` | `spec.gitHubURL` | Same org / enterprise / repo URL form. |
| `githubConfigSecret` (PAT **or** GitHub App) | `spec.gitHubAppRef.name` → a namespace `Secret` (`appId`, `installationId`, `privateKey`) | **GAG is GitHub-App-only — no Personal Access Token (PAT) path.** If your scale sets authenticate with a PAT, you create a GitHub App first; see [GitHub App setup](#1-create-the-github-app-secret). |
| `runnerScaleSetName` / the install name you put in `runs-on` | `runnerGroups[].runnerLabels` (a label **set**) | The routing model differs — see [Job routing](#job-routing-the-one-that-bites). |
| `runnerGroup` (the GitHub runner-group name) | n/a | GAG registers runners by **label**, not by a GitHub runner group. There is no `runnerGroup` field. |
| `spec.template` (pod template, a `PodTemplateSpec`) | `runnerGroups[].podTemplate` (the same `PodTemplateSpec`) | `resources`, `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`, `runtimeClassName`, `securityContext`, `volumes`, init/sidecar containers transfer **one-to-one**. Renamed `template` → `podTemplate` only to keep the type unambiguous. |
| runner container image (chart default `ghcr.io/actions/actions-runner`) | `runnerGroups[].workerImage` (same default) | Identical default image, so a stock scale set needs no image change. |
| `maxRunners` (per scale set) | `runnerGroups[].maxWorkers` (per group) + the **namespace `ResourceQuota`** | The real cap is the platform-owned quota, shared across groups; `maxWorkers` is the per-group ceiling. See [Quotas](#quotas-and-scheduling). |
| `minRunners` (warm pool to mask cold start) | n/a — **not needed** | The goroutine listener never goes cold, so there is no cold-start to mask; GAG always scales workers to zero between jobs. Dropping `minRunners > 0` is what removes your idle GPU/compute pods. |
| `containerMode: kubernetes` / `dind` | a `podTemplate` choice + `securityProfile` | GAG always runs one worker pod per job. Docker-in-Docker needs `securityProfile: privileged` (a platform-granted opt-in); see [Security profiles](#security-profiles). |
| Per-scale-set scaling math (`minRunners`/`maxRunners`) | `runnerGroups[].maxListeners` + `maxWorkers` | `maxListeners` caps concurrent job acquisition; `maxWorkers` caps concurrent worker pods. |
| Pod Security / NetworkPolicy / RBAC hand-built around ARC | reconciled from the one CR by the GMC | GAG ships these as secure-by-default; you do not assemble them per tenant ([Why GAG → Secure by default](../why-gag.md#secure-by-default)). |
| (no ARC equivalent) | `runnerGroups[].priorityTiers` | A guaranteed floor of preempting slots for a runner type under a shared quota — the thing ARC's per-scale-set `maxRunners` cannot express. |
| (no ARC equivalent) | per-tenant egress proxy pool / dedicated egress IPs | See [Egress isolation](#egress-isolation-the-big-difference). |

---

## Job routing: the one that bites

This is the single behavioral difference most likely to surprise you, so it gets
its own section.

**ARC scale-set mode routes by the scale-set name.** A workflow targets a scale
set with `runs-on: <runnerScaleSetName>` — a single name that equals the install
name. Scale-set mode deliberately dropped the arbitrary multi-label matching that
legacy ARC `RunnerDeployment` had; the name *is* the selector.

**GAG routes by a label set.** Each `runnerGroups[]` entry declares
`runnerLabels: [...]`, and a job is routed to a group when the job's `runs-on`
labels match that group's label set — the same multi-label model GitHub uses for
any self-hosted runner. So in GAG you write:

```yaml
# workflow
jobs:
  build:
    runs-on: [self-hosted, linux, gpu]   # matched against a runnerGroup's runnerLabels
```

What this means for migration:

- **Coming from scale-set mode (single name):** pick a `runnerLabels` set that
  includes a label your workflows can target. The simplest zero-churn move is to
  add the **old scale-set name as one of the labels** so existing `runs-on: <name>`
  keeps working, e.g. a scale set named `gpu-large` becomes
  `runnerLabels: ["self-hosted", "gpu-large"]`. Then optionally migrate workflows
  to richer label sets at your own pace.
- **`runnerLabels` must be non-empty.** An empty set would match *every* workflow
  run, so admission rejects it (`MinItems=1`). Each label is ≤ 256 chars and may
  not contain whitespace or commas (comma is the `runs-on` list separator).
- **Distinct groups need distinct label sets.** Two runner groups whose labels
  both match a job is an ambiguous route — give each group a label that uniquely
  identifies it (the old scale-set name works well for this).
- **`self-hosted` is conventional, not magic.** GitHub adds `self-hosted`
  implicitly to self-hosted runners; include it in `runnerLabels` so
  `runs-on: self-hosted` style workflows still match.

> **Coming from legacy ARC (`RunnerDeployment` + labels)?** Your label model is
> already close to GAG's — carry the same label set into `runnerLabels` and most
> workflows route unchanged.

---

## Egress isolation: the big difference

ARC runners share the cluster's egress path: GitHub (and any other) traffic
leaves via whatever node / NAT the pod lands on, so you cannot attribute a source
IP to a tenant or allow-list one team's runners without allow-listing the whole
cluster.

GAG gives **each tenant its own egress proxy pool** (Tier 3). All GitHub-bound
traffic from the AGC and worker pods routes through that pool, so every tenant
gets a dedicated, stable set of egress IPs never shared with another tenant. That
is what makes GitHub Enterprise Managed Users (EMU) IP allow-listing, per-tenant
incident attribution, and avoiding shared-NAT throttling possible. The pool is
HPA-managed between `spec.proxy.minReplicas` and `maxReplicas`.

Differences and quirks an ARC operator should know:

- **Default-deny egress is the default, and it needs a policy-aware CNI.** GAG
  provisions NetworkPolicies that restrict worker pods to **DNS + the tenant
  proxy only** (no direct GitHub, no Kubernetes API), and the proxy pods to the
  **GitHub IP allow-list + DNS**. These rules are inert unless your Container
  Network Interface (CNI) plugin enforces egress NetworkPolicy — Calico or Cilium
  do; kind's default kindnet does **not**. If you ran ARC on a cluster whose CNI
  does not enforce egress, **isolation is silently void** until you switch CNI.
  Validate with the probes in
  [network-architecture § How to Validate Network Isolation](../design/network-architecture.md#how-to-validate-network-isolation).
- **The GitHub IP allow-list is maintained for you.** The GMC refreshes the
  GitHub CIDR set the proxy permits, so the egress allow-list tracks GitHub's
  published ranges without operator action.
- **Internal destinations need `noProxyCIDRs`.** Anything your jobs reach
  *inside* the cluster or your network (artifact stores, internal registries)
  must be excluded from the proxy via `spec.proxy.noProxyCIDRs` (CIDRs, bare IPs,
  or `NO_PROXY` domain suffixes). Cluster-internal defaults are appended
  automatically. **Admission rejects any entry that would route GitHub traffic
  around the proxy** (a host matching `gitHubURL` or `github.com` /
  `githubusercontent.com` / `ghcr.io`) — that would break egress-IP attribution.
- **The AGC↔proxy hop is HTTPS with a pinned cert.** The GMC issues a per-tenant
  self-signed cert and pins it into the AGC trust store, so the proxy hop is not
  eavesdroppable by other tenants on the cluster.
- **v2 makes the proxy optional.** In the v2 API you may run **direct egress**
  (no proxy) — egress is still default-deny-restricted to DNS + GitHub, but you
  lose per-tenant IP *attribution*. The trade is surfaced as
  `status.proxyMode: Direct` + an advisory `EgressUnattributed` condition. Keep
  the proxy if you came to GAG *for* the per-tenant egress IPs. See the
  [v2 onboarding note](tenant-onboarding.md#proxy-less-onboarding-direct-egress).

---

## Gotchas and behavioral differences

| Area | ARC scale-set behavior | GAG behavior | What to do |
|---|---|---|---|
| **Evicted / quota-blocked job** | Runner marked `Failed`; job sits in GitHub's queue until a **manual rerun** | The job lock is fast-cancelled and the job **re-queued automatically**; it runs as soon as capacity frees | Nothing — this is the headline upgrade. You can stop the manual-rerun runbook. |
| **Job routing** | `runs-on: <scale-set-name>` (single name) | `runs-on:` matched against a label **set** | [Add the old name as a label](#job-routing-the-one-that-bites). |
| **Auth** | PAT or GitHub App | **GitHub App only** | Create a GitHub App if you were on a PAT. |
| **Listener** | one ~256 MiB pod per scale set, 24/7 | one shared goroutine pod per tenant | No action; expect far fewer pods/IPs at rest. |
| **Warm pool** | `minRunners > 0` to mask cold start | not needed; always scales to zero | Drop `minRunners`; this removes idle GPU/compute. |
| **Per-runner-type cap** | `maxRunners` per scale set | `maxWorkers` per group **+ shared namespace `ResourceQuota`** | The quota is the real cap; size it for all groups ([Quotas](#quotas-and-scheduling)). |
| **Critical-runner floor** | none (each scale set caps only itself) | `priorityTiers` per group | Optionally reserve preempting slots for expensive runner types. |
| **DinD / privileged** | `containerMode: dind` | `securityProfile: privileged` (platform-granted, per namespace) | Requires a namespace eligibility label; see [Security profiles](#security-profiles). |
| **One gateway per namespace** | many scale sets per namespace | one `ActionsGateway` per namespace (v1) | Put all runner types in one CR; use v2 or a second namespace if you truly need separate gateways. |
| **Quota / RBAC / NetworkPolicy** | hand-assembled per tenant | reconciled from the CR | Remove your bespoke policy manifests after cutover. |
| **Worker-pod debugging** | runner pod lingers per HRA config | finished pod kept for `completedPodTTL` (default `5m`), then deleted | Raise `completedPodTTL` if you need a longer `kubectl logs` window. |

### Quotas and scheduling

In ARC, each scale set's `maxRunners` is its own independent cap. In GAG, the real
ceiling is the **platform-owned namespace `ResourceQuota`**, shared across every
runner group, and `maxWorkers` is a per-group ceiling within it. This is
deliberate — a tenant-authored cap is no cap (the tenant could raise it), and it
is what makes per-tenant quotas *safe*: a quota-blocked job recovers instead of
dying. Size the quota for the proxy pool at `proxy.maxReplicas` **plus** worker
pods up to each group's `maxWorkers`; the
[onboarding quota step](tenant-onboarding.md#step-1b-set-the-platform-owned-resourcequota)
has the formula. When a group should never be starved by cheaper runners, use
`priorityTiers` to reserve a floor of preempting slots — the primitive ARC's
per-scale-set model has no equivalent for.

### Security profiles

ARC's `containerMode: dind` becomes GAG's `securityProfile: privileged`, which is a
**platform decision, not tenant-settable**: the namespace must carry the
`actions-gateway.github.com/privileged-profile=allowed` eligibility label
(applied by an administrator) or admission rejects the CR. The default
`baseline` profile is correct for ordinary build/test workloads; use `restricted`
for high-isolation tenants. You can *harden* a profile in place freely but
*relaxing* it is an explicit annotated opt-in. Full rules:
[tenant-onboarding Pre-Conditions](tenant-onboarding.md#pre-conditions) and
[Security § 5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in).

---

## Worked migration: one runner group, end to end

This migrates a single ARC scale set to a GAG runner group with **zero downtime**
— ARC keeps serving until you cut workflows over, and rollback is one line.

Worked example: an ARC scale set named **`gpu-large`** in namespace **`team-a`**,
authenticated with a GitHub App, running GPU workflows.

### 0. Confirm prerequisites

GAG's GMC is installed ([Install](install.md)) and your cluster's CNI enforces
egress NetworkPolicy ([Egress isolation](#egress-isolation-the-big-difference)).
Confirm the namespace is marked a managed tenant and has a platform-owned
`ResourceQuota` — see the
[tenant-onboarding Pre-Conditions](tenant-onboarding.md#pre-conditions). **Leave
the ARC scale set running** throughout.

### 1. Create the GitHub App Secret

If your ARC scale set used a **GitHub App** already, you can reuse the same App —
create a GAG-shaped Secret (`appId`, `installationId`, `privateKey`) in the tenant
namespace per
[tenant-onboarding Step 1](tenant-onboarding.md#step-1-create-the-github-app-secret).
If your scale set used a **PAT**, register a GitHub App now (`Actions: Read` +
`Administration: Read`, installed on the same org/repos) — GAG has no PAT path.
Never copy a private key through an environment variable or shell history.

### 2. Translate the scale set into a `runnerGroups` entry

Lift your ARC `spec.template` straight into `podTemplate` (same type), and turn the
scale-set name into a label so existing `runs-on` keeps working:

```yaml
apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: team-a-gateway
  namespace: team-a
spec:
  gitHubAppRef:
    name: github-app-v1
  gitHubURL: https://github.com/team-a-org   # was ARC's githubConfigUrl
  securityProfile: baseline                  # privileged only if you used containerMode: dind
  proxy:
    minReplicas: 2
    maxReplicas: 10
  runnerGroups:
    - name: gpu-large
      # Old scale-set name kept as a label so `runs-on: gpu-large` still routes:
      runnerLabels: ["self-hosted", "gpu-large"]
      maxListeners: 10
      maxWorkers: 20              # was ARC's maxRunners (real cap is the namespace quota)
      # No minRunners: GAG scales to zero with no cold-start penalty.
      podTemplate:                # ← paste your ARC spec.template here, unchanged
        spec:
          containers:
            - name: runner
              resources:
                requests:
                  cpu: "4"
                  memory: "16Gi"
                limits:
                  nvidia.com/gpu: "1"
```

Add one `runnerGroups[]` entry per ARC scale set you are migrating in this
namespace — they all live under this one `ActionsGateway`.

### 3. Apply and validate provisioning

```sh
kubectl apply -f actionsgateway.yaml
```

Wait for `Ready=True` and confirm the AGC, proxy pool, RBAC, NetworkPolicies, and
the `RunnerGroup` CR(s) came up — follow
[tenant-onboarding Step 3](tenant-onboarding.md#step-3-validate-provisioning) and
[Step 4](tenant-onboarding.md#step-4-validate-listener-sessions). At this point
**both** the ARC scale set and the GAG runner group are registered with GitHub and
listening; GitHub will route a matching job to whichever acquires it first.

### 4. Cut workflows over and verify

Run a test job targeting the labels and confirm a GAG worker pod is created,
egresses through the proxy, and is deleted on completion
([tenant-onboarding Step 5](tenant-onboarding.md#step-5-run-a-test-job)). Because
you kept the old name as a label, existing `runs-on: gpu-large` workflows already
land on GAG. Watch a few real jobs through the GAG path before removing ARC.

### 5. Decommission the ARC scale set

Once you trust the GAG path, scale the ARC scale set to zero / uninstall its Helm
release:

```sh
helm uninstall gpu-large -n team-a   # removes the AutoscalingRunnerSet + its listener pod
```

Repeat steps 2–5 for each remaining scale set, then remove any bespoke
NetworkPolicy / RBAC / quota manifests you hand-built around ARC — GAG reconciles
those from the CR now.

### Rollback

Nothing in this flow removes ARC until step 5, so rollback during migration is
trivial: if the GAG path misbehaves, **keep the ARC scale set running** and point
`runs-on` back at the scale-set name (or just delete the GAG runner group entry
and re-apply). Because both register independently with GitHub, there is no
split-brain to untangle.

---

## v2 differences worth knowing

GAG's v1 API (`actions-gateway.github.com/v1alpha1`, the GA default) is the right
target for a first migration and everything above uses it. The opt-in v2 API
(`actions-gateway.com`) adds capabilities an ARC operator may want later:

- **Multiple gateways per namespace** — lifts the v1 one-gateway-per-namespace
  rule, closer to ARC's many-scale-sets-per-namespace model.
- **Reusable `RunnerTemplate` / `ClusterRunnerTemplate`** — share one pod template
  across runner sets instead of inlining it per group (ARC inlines `template` per
  scale set).
- **Optional egress proxy (direct egress)** — see
  [Egress isolation](#egress-isolation-the-big-difference).
- **Workload-identity credentials** — keep the GitHub App signing key out of the
  cluster entirely (external signer).

These are not required to replace ARC. If you adopt v2 later, see the
[v1 → v2 migration guide](migration-v1-to-v2.md).

---

## Where to go next

- [Tenant onboarding checklist](tenant-onboarding.md) — the full pre-conditions →
  first-job reference this guide builds on.
- [Why GAG over ARC](../why-gag.md) — the capability-by-capability comparison.
- [Getting Started](../getting-started.md) — the end-to-end first install.
- [Network architecture](../design/network-architecture.md) — egress proxy and
  NetworkPolicy detail, with isolation-validation probes.
- [Troubleshooting](troubleshooting.md) — common first-day failures.
