# Migrating from Actions Runner Controller (ARC) to GitHub Actions Gateway (GAG)

> **Audience:** Platform engineer running [Actions Runner Controller (ARC)](https://github.com/actions/actions-runner-controller) scale-set mode on a shared, multi-tenant Kubernetes cluster and evaluating GitHub Actions Gateway (GAG) as a replacement.

This guide maps ARC scale-set concepts onto GAG, calls out the behavioral differences you will actually hit, and walks one runner group from ARC to GAG end to end.
It assumes you already run ARC's `gha-runner-scale-set` chart; if you are new to GAG, read [Why GAG](../why-gag.md) and [Getting Started](../getting-started.md) first, then come back here.

The good news up front: GAG was designed to make this migration cheap.
The worker pod template is the **same Kubernetes type** ARC uses, so your pod spec transfers with no schema translation; job routing is the **same single-name model** ARC scale sets use, so your `runs-on` lines need no edit; and ARC and GAG can run **side by side** on the same cluster, so you migrate one scale set at a time and roll back by pointing `runs-on` back at the old name.

**Scope.** This covers ARC's **scale-set mode** (the `AutoscalingRunnerSet` / `gha-runner-scale-set` chart — GitHub's current and recommended mode), and targets GAG's **v2 API** (`actions-gateway.com/v2beta1`), which is where new tenants onboard.
Legacy ARC (`RunnerDeployment` + `HorizontalRunnerAutoscaler`) maps similarly, including its multi-label routing; see [Job routing](#job-routing-a-11-map).

> **Do not migrate via GAG's v1 API.** The older single-CR `actions-gateway.github.com/v1alpha1` shape is **[deprecated](v1alpha1-deprecation.md)** and, more to the point here, it is *Classic-protocol only* — it would move you **off** the single-acquirer model your ARC scale sets already use.
> Migrate straight to v2.

---

## The mental model

In ARC scale-set mode, **each scale set is its own controller surface**: one `AutoscalingRunnerSet` CR, one long-running listener pod, its own `maxRunners` cap, configured by its own Helm release.
Ten runner types means ten scale sets, ten listener pods, ten Helm releases.

In GAG, a tenant declares one `ActionsGateway` (identity and GitHub binding) and one `RunnerSet` per runner type.
A single per-gateway controller — the Actions Gateway Controller (AGC) — multiplexes every set's listener as a goroutine (~12 KiB) in one shared pod, instead of one always-on listener pod per scale set.
The platform installs the Gateway Manager Controller (GMC) **once**; from there a tenant's whole gateway (controller, egress proxy, RBAC, NetworkPolicies) is provisioned from those CRs.

So the shape of the migration is: **N ARC scale sets in a namespace → 1 `ActionsGateway` + N `RunnerSet`s in that namespace**, sharing a `RunnerTemplate` wherever the pod shapes are the same.

```
ARC (scale-set mode)                    GAG (v2 API)
─────────────────────                   ─────────────────────────────
namespace: team-a                       namespace: team-a
  AutoscalingRunnerSet "cpu"              ActionsGateway "team-a-gateway"
    └─ listener pod (always-on)           EgressProxy   "team-a-egress"
  AutoscalingRunnerSet "gpu"              RunnerTemplate "default" (shared shape)
    └─ listener pod (always-on)           RunnerSet "cpu"  (goroutine listener)
  AutoscalingRunnerSet "arm"              RunnerSet "gpu"  (goroutine listener)
    └─ listener pod (always-on)           RunnerSet "arm"  (goroutine listener)
  (shared cluster egress)                 └─ one AGC pod multiplexes all three
                                          └─ per-tenant egress proxy pool
```

Unlike GAG's deprecated v1 shape, v2 puts **no one-gateway-per-namespace limit** on you: if your ARC namespace spans two GitHub orgs, that is two `ActionsGateway`s side by side, closer to how your scale sets are laid out today.

---

## Concept mapping: ARC → GAG

| ARC scale-set concept | GAG equivalent | Notes |
|---|---|---|
| `AutoscalingRunnerSet` (one per runner type) | one `RunnerSet` CR | 1:1. Each keeps its own name, labels, and caps. |
| `gha-runner-scale-set` Helm release per scale set | plain CRs applied with `kubectl` | You stop managing a Helm release per runner type. The GMC is the only Helm install ([Install](install.md)). |
| `gha-runner-scale-set-controller` (cluster controller) | Gateway Manager Controller (GMC) | Installed once by the platform; provisions every tenant's AGC. |
| The `AutoscalingListener` pod (one per scale set) | a goroutine inside the shared per-gateway AGC pod | one always-on pod per scale set → ~12 KiB/goroutine in one shared pod; no per-listener cluster IP. |
| `githubConfigUrl` | `ActionsGateway.spec.githubURL` | Same org / enterprise / repo URL form. **Immutable** in v2 — no accidental rebinding of a live gateway. |
| `githubConfigSecret` (PAT **or** GitHub App) | `ActionsGateway.spec.credentials` | **GAG is GitHub-App-only — no Personal Access Token (PAT) path.** See [GitHub App setup](#1-create-the-github-app-secret). v2 also offers `type: WorkloadIdentity` to keep the signing key out of the cluster entirely. |
| `runnerScaleSetName` / the name you put in `runs-on` | `RunnerSet.spec.runnerLabels[0]` | **Same model.** The first label is the scale set's name. See [Job routing](#job-routing-a-11-map). |
| `scaleSetLabels` (ARC 0.14.0+, the extra `runs-on` array targets) | the rest of `RunnerSet.spec.runnerLabels` | **Same model.** One list here instead of a name plus a list. |
| `runnerGroup` (the GitHub runner-group name) | `RunnerSet.spec.runnerGroup`, or `ActionsGateway.spec.defaultRunnerGroup` for every set under one gateway | **Same field, plus a per-gateway default.** GAG fails the set closed (`RunnerGroupNotFound`) if the group does not exist, where ARC's chart leaves it to GitHub. See [Runner groups](tenant-onboarding.md#bind-a-runner-set-to-a-github-runner-group). |
| `spec.template` (pod template, a `PodTemplateSpec`) | `RunnerTemplate.spec.podTemplate` (the same `PodTemplateSpec`) | `resources`, `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`, `runtimeClassName`, `securityContext`, `volumes`, init/sidecar containers transfer **one-to-one**. ARC inlines this per scale set; a `RunnerTemplate` is referenced by many `RunnerSet`s, so identical shapes are written once. |
| runner container image (ARC default `ghcr.io/actions/actions-runner`) | `RunnerTemplate.spec.workerImage` (same image, or unset for the default) | **Drop-in.** See [Your runner image works unchanged](#your-runner-image-works-unchanged). |
| `maxRunners` (per scale set) | `RunnerSet.spec.maxWorkers` + the **namespace `ResourceQuota`** | The real cap is the platform-owned quota, shared across sets; `maxWorkers` is the per-set ceiling. See [Quotas](#quotas-and-scheduling). |
| `minRunners` (warm pool to mask cold start) | n/a — **not needed** | The goroutine listener never goes cold, so there is no cold start to mask; GAG always scales workers to zero between jobs. Dropping `minRunners > 0` is what removes your idle GPU/compute pods. |
| `containerMode: kubernetes` / `dind` | a template choice + the namespace security profile | GAG always runs one worker pod per job. Docker-in-Docker needs a platform-owned `ClusterRunnerTemplate` plus a privileged-eligible namespace; see [Security profiles](#security-profiles). |
| Per-scale-set scaling math (`minRunners`/`maxRunners`) | `RunnerSet.spec.maxWorkers` (+ optional `sizing`) | One knob instead of two. Concurrency is advertised to GitHub as the scale set's capacity. |
| Pod Security / NetworkPolicy / RBAC hand-built around ARC | reconciled from the CRs by the GMC | GAG ships these as secure-by-default; you do not assemble them per tenant ([Why GAG → Secure by default](../why-gag.md#secure-by-default)). |
| (no ARC equivalent) | `RunnerSet.spec.priorityTiers` | A guaranteed floor of preempting slots for a runner type under a shared quota — the thing ARC's per-scale-set `maxRunners` cannot express. |
| (no ARC equivalent) | `EgressProxy` / dedicated egress IPs | See [Egress isolation](#egress-isolation-the-big-difference). |

### Your runner image works unchanged

Point `workerImage` at your **existing ARC runner image** — no rebuild — or leave it unset to use the upstream `ghcr.io/actions/actions-runner` default.
GAG's AGC **pre-acquires** each job from the GitHub broker and hands it to the pod (rather than letting the pod's runner register and poll), so the worker needs GAG's small **wrapper** to feed the job payload into `Runner.Worker`.
Rather than make you bake that wrapper into your image, the AGC **injects** it into every worker pod at runtime — a read-only OCI image volume on Kubernetes ≥ 1.33, an initContainer below that — and runs it in place of the image entrypoint.
The only requirement is that the image be `actions/runner`-derived (it must contain `Runner.Worker`) — the same agent requirement ARC has, so **any image that works on ARC qualifies**, with its tools and userland intact.

> **Docker-in-Docker:** because the injected wrapper runs in place of the image entrypoint, an `actions-runner-dind` image's bundled `dockerd` startup is **skipped**.
> Run DinD as a **sidecar** container in a privileged `ClusterRunnerTemplate` instead (see [Security profiles](#security-profiles)) — GAG's DinD model regardless of the worker image.

---

## Job routing: a 1:1 map

This is the section that used to carry a warning.
Against GAG's v2 API it does not, and that is worth stating plainly because it is the difference between a migration that touches your workflows and one that does not.

**ARC scale-set mode routes by the scale-set name, plus any extra labels.** A workflow targets a scale set with `runs-on: <runnerScaleSetName>`, and since ARC 0.14.0 also with an array matched against the scale set's `scaleSetLabels`.

**GAG v2 routes the same way, from one list.** `spec.runnerLabels[0]` is the scale set's name; every entry after it is an additional match target.
So both ARC shapes carry across unchanged:

```yaml
# ARC:  runs-on: gpu-large        →   GAG: runs-on: gpu-large        (no edit)
# ARC:  runs-on: [linux, gpu]     →   GAG: runs-on: [linux, gpu]     (no edit)
spec:
  runnerLabels: ["gpu-large", "linux", "gpu"]   # ARC's scale-set name, then its scaleSetLabels
```

Rules worth knowing:

- **The first label is the identity.** It names the scale set at GitHub, so reordering the list renames the scale set and orphans the old one.
  Append rather than prepend.
- **The first label is unique per GitHub org, enterprise, or repo.** Two `RunnerSet`s whose gateways name the same `githubURL` may not share `runnerLabels[0]`; they would register the same scale-set name and collide.
  This spans namespaces, because the scale-set name is unique at GitHub rather than in the cluster.
  Keeping your ARC names gives you uniqueness for free, since ARC already required distinct install names.
  Later labels **may** overlap, and `linux` across every set is the normal shape; which set an ambiguous `runs-on` reaches is GitHub's decision.
- **Labels are registered once, at creation.** Adding one to a live set does not register it; the set reports [`RunnerLabelsIncomplete`](troubleshooting.md#jobs-targeting-one-of-a-runner-sets-labels-never-start-runnerlabelsincomplete) instead of failing silently.
  Declare the full list up front.
- **No `self-hosted`.** Under the scale-set model `self-hosted` is not part of the match, exactly as in ARC scale-set mode.
  Do not add it.
- **Labels are ≤ 256 chars** and may not contain whitespace or commas.

> **On GitHub Enterprise Server below 3.21**, more than one label per scale set needs the `DistributedTask.AllowRunnerScaleSetCustomLabels` feature flag, which a site admin enables on the appliance (it is on by default from 3.21).
> Without it the appliance keeps only the name label and drops the rest silently.
> GAG surfaces that as `RunnerLabelsIncomplete`, but the labels still will not match until the flag is on.

> **Coming from legacy ARC (`RunnerDeployment` + multi-label `runs-on`)?** Your `runs-on` arrays carry across too: declare the same labels on one `RunnerSet`, with a distinctive one first to name its scale set.

---

## Egress isolation: the big difference

ARC runners share the cluster's egress path: GitHub (and any other) traffic leaves via whatever node / NAT the pod lands on, so you cannot attribute a source IP to a tenant or allow-list one team's runners without allow-listing the whole cluster.

GAG gives **each tenant its own egress proxy pool** (Tier 3), declared as an `EgressProxy` and pointed at by the gateway's `defaultProxyRef`.
All GitHub-bound traffic from the AGC and worker pods routes through that pool, so every tenant gets a dedicated, stable set of egress IPs never shared with another tenant.
That is what makes GitHub Enterprise Managed Users (EMU) IP allow-listing, per-tenant incident attribution, and avoiding shared-NAT throttling possible.
The pool is HPA-managed between `spec.minReplicas` and `maxReplicas`.

Differences and quirks an ARC operator should know:

- **The proxy is optional, but it is the reason to be here.** A gateway with no `defaultProxyRef` and sets with no `proxyRef` egress **directly**: still default-deny-restricted to DNS + the GitHub CIDR allowlist, but with no per-tenant IP *attribution*.
  The trade is surfaced as `status.proxyMode: Direct` plus an advisory `EgressUnattributed` condition.
  If per-tenant egress IPs are what brought you off ARC, keep the proxy.
  See [Proxy-less onboarding](tenant-onboarding.md#proxy-less-onboarding-direct-egress).
- **Default-deny egress is the default, and it needs a policy-aware CNI.** GAG provisions NetworkPolicies that restrict worker pods to **DNS + the tenant proxy only** (no direct GitHub, no Kubernetes API), and the proxy pods to the **GitHub IP allow-list + DNS**.
  These rules are inert unless your Container Network Interface (CNI) plugin enforces egress NetworkPolicy — Calico or Cilium do; kind's default kindnet does **not**.
  If you ran ARC on a cluster whose CNI does not enforce egress, **isolation is silently void** until you switch CNI.
  Validate with the probes in [network-architecture § How to Validate Network Isolation](../design/network-architecture.md#how-to-validate-network-isolation).
- **The GitHub IP allow-list is maintained for you.** The GMC refreshes the GitHub CIDR set the proxy permits, so the egress allow-list tracks GitHub's published ranges without operator action.
  `egressPolicyMode: FQDN` allow-lists GitHub by hostname instead, where your CNI supports it.
- **Internal destinations need `noProxyCIDRs`.** Anything your jobs reach *inside* the cluster or your network (artifact stores, internal registries) must be excluded from the proxy via `EgressProxy.spec.noProxyCIDRs` (CIDRs, bare IPs, or `NO_PROXY` domain suffixes).
  Cluster-internal defaults are appended automatically on every distribution — `svc.cluster.local`, `kubernetes.default.svc`, `localhost`, `127.0.0.1`, and the cluster's API server ClusterIP — so neither the API server nor the service CIDR belongs in this field.
  **Admission rejects any entry that would route GitHub traffic around the proxy** (a host matching `githubURL` or `github.com` / `githubusercontent.com` / `ghcr.io`) — that would break egress-IP attribution.
- **The AGC↔proxy hop is HTTPS with a pinned cert.** The GMC issues a per-tenant self-signed cert and pins it into the AGC trust store, so the proxy hop is not eavesdroppable by other tenants on the cluster.

---

## Gotchas and behavioral differences

| Area | ARC scale-set behavior | GAG behavior | What to do |
|---|---|---|---|
| **Evicted / quota-blocked job** | Runner marked `Failed`; job sits in GitHub's queue until a **manual rerun** | The job is cancelled when its lock lapses (~10 min at worst) and **re-queued automatically**; it runs as soon as capacity frees | Nothing — this is the headline upgrade. You can stop the manual-rerun runbook. |
| **Job routing** | `runs-on: <scale-set-name>` (single name) | same single-name model | [Carry the name across as the label](#job-routing-a-11-map); no workflow edit. |
| **Auth** | PAT or GitHub App | **GitHub App only** | Create a GitHub App if you were on a PAT. |
| **Listener** | one always-on pod per scale set, 24/7 | one shared goroutine pod per gateway | No action; expect far fewer pods/IPs at rest. |
| **Warm pool** | `minRunners > 0` to mask cold start | not needed; always scales to zero | Drop `minRunners`; this removes idle GPU/compute. |
| **Per-runner-type cap** | `maxRunners` per scale set | `maxWorkers` per set **+ shared namespace `ResourceQuota`** | The quota is the real cap; size it for all sets ([Quotas](#quotas-and-scheduling)). |
| **Critical-runner floor** | none (each scale set caps only itself) | `priorityTiers` per set | Optionally reserve preempting slots for expensive runner types. |
| **DinD / privileged** | `containerMode: dind` | a privileged `ClusterRunnerTemplate` + privileged-eligible namespace | Platform-granted, not tenant-settable; see [Security profiles](#security-profiles). |
| **Pod template reuse** | `template` copied into every scale set | one `RunnerTemplate` referenced by many sets | Deduplicate identical shapes; keep separate templates where they genuinely differ. |
| **Gateways per namespace** | many scale sets per namespace | many `RunnerSet`s, and many gateways, per namespace | No restructuring needed. (GAG's deprecated v1 API had a one-gateway limit; v2 does not.) |
| **Quota / RBAC / NetworkPolicy** | hand-assembled per tenant | reconciled from the CRs | Remove your bespoke policy manifests after cutover. |
| **Worker-pod debugging** | runner pod lingers per HRA config | finished pod kept for `completedPodTTL` (default `5m`), then deleted | Raise `completedPodTTL` if you need a longer `kubectl logs` window. |
| **Maximum worker lifetime** | none — a wedged runner pod runs until deleted by hand | `maxWorkerLifetime` (default `12h`) as the pod's `activeDeadlineSeconds`, enforced by the kubelet | **Check this before cutover if any job runs longer than 12 hours.** ARC has no equivalent cap, so a long job that worked there is killed here unless you raise the field. GitHub's own default `timeout-minutes` is 360 (6h), so this affects only jobs that explicitly opted past twice that. |
| **Cluster version** | ARC supports older clusters | **Kubernetes ≥ 1.31** for v2 | The AGC selects its `RunnerSet`s with a server-side CRD field selector that is alpha-off on 1.30. Confirm before you start. |

### Quotas and scheduling

In ARC, each scale set's `maxRunners` is its own independent cap.
In GAG, the real ceiling is the **platform-owned namespace `ResourceQuota`**, shared across every runner set, and `maxWorkers` is a per-set ceiling within it.
This is deliberate — a tenant-authored cap is no cap (the tenant could raise it), and it is what makes per-tenant quotas *safe*: a quota-blocked job recovers instead of dying.
Size the quota for the proxy pool at `maxReplicas` **plus** worker pods up to each set's `maxWorkers`; the [onboarding quota step](tenant-onboarding.md#step-1b-set-the-platform-owned-resourcequota) has the formula.
When a set should never be starved by cheaper runners, use `priorityTiers` to reserve a floor of preempting slots — the primitive ARC's per-scale-set model has no equivalent for.

### Security profiles

ARC's `containerMode: dind` becomes two platform-owned grants in GAG v2, neither tenant-settable:

1. **A privileged namespace.** The Pod Security level is a **namespace label** in v2 (`actions-gateway.com/security-profile`), shared by every gateway in the namespace.
   `privileged` additionally requires the namespace to carry `actions-gateway.com/privileged-profile: allowed`, applied by an administrator, or admission rejects it.
2. **A privileged template.** A namespaced `RunnerTemplate` may **not** declare privileged containers — a tenant must not self-author a privileged worker shape.
   Privileged DinD/sysbox/Kata shapes are published by a platform admin as a cluster-scoped `ClusterRunnerTemplate`, which a `RunnerSet` references with `templateRef: { kind: ClusterRunnerTemplate, name: … }`.

The default `baseline` profile is correct for ordinary build/test workloads; use `restricted` for high-isolation tenants.
You can *harden* a profile in place freely but *relaxing* it is an explicit annotated opt-in.
Full rules: [tenant-onboarding Pre-Conditions](tenant-onboarding.md#pre-conditions) and [Security § 5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in).

---

## Worked migration: one scale set, end to end

This migrates a single ARC scale set to a GAG runner set with **zero downtime** — ARC keeps serving until you cut workflows over, and rollback is one line.

Worked example: an ARC scale set named **`gpu-large`** in namespace **`team-a`**, authenticated with a GitHub App, running GPU workflows.

### 0. Confirm prerequisites

The GMC is installed **with the v2 CRD chart** ([Install](install.md#optional-the-v2-api-crds)), the cluster is **Kubernetes ≥ 1.31**, and its CNI enforces egress NetworkPolicy ([Egress isolation](#egress-isolation-the-big-difference)).
Confirm the namespace is marked a managed tenant, carries its security-profile label, and has a platform-owned `ResourceQuota` — see the [tenant-onboarding Pre-Conditions](tenant-onboarding.md#pre-conditions).
**Leave the ARC scale set running** throughout.

### 1. Create the GitHub App Secret

If your ARC scale set used a **GitHub App** already, you can reuse the same App — create a GAG-shaped Secret (`appId`, `installationId`, `privateKey`) in the tenant namespace per [tenant-onboarding Step 1](tenant-onboarding.md#step-1-create-the-github-app-secret).
If your scale set used a **PAT**, register a GitHub App now (`Actions: Read` + `Administration: Read`, installed on the same org/repos) — GAG has no PAT path.
Never copy a private key through an environment variable or shell history.

### 2. Translate the scale set into the v2 object set

Lift your ARC `spec.template` straight into the `RunnerTemplate` (same type), and carry the scale-set name across as the runner set's single label so existing `runs-on` keeps working:

```yaml
apiVersion: actions-gateway.com/v2beta1
kind: EgressProxy
metadata:
  name: team-a-egress
  namespace: team-a
spec:
  minReplicas: 2
  maxReplicas: 10
---
apiVersion: actions-gateway.com/v2beta1
kind: RunnerTemplate           # your ARC spec.template, written once and shared
metadata:
  name: gpu
  namespace: team-a
spec:
  podTemplate:                 # ← paste your ARC spec.template here, unchanged
    spec:
      containers:
        - name: runner
          resources:
            requests:
              cpu: "4"
              memory: "16Gi"
            limits:
              nvidia.com/gpu: "1"
---
apiVersion: actions-gateway.com/v2beta1
kind: ActionsGateway
metadata:
  name: team-a-gateway
  namespace: team-a
spec:
  credentials:
    type: GitHubApp
    githubApp:
      name: github-app-v2      # name-only Secret ref in this namespace
  githubURL: https://github.com/team-a-org   # was ARC's githubConfigUrl (immutable)
  defaultProxyRef:
    name: team-a-egress        # every RunnerSet below inherits this
  # No securityProfile field: the Pod Security level is a namespace label in v2.
---
apiVersion: actions-gateway.com/v2beta1
kind: RunnerSet
metadata:
  name: gpu-large              # v2 names the set explicitly
  namespace: team-a
spec:
  gatewayRef:  { name: team-a-gateway }
  templateRef: { name: gpu }
  runnerLabels: ["gpu-large"]  # the ARC scale-set name: runs-on: gpu-large still routes
  maxWorkers: 20               # was ARC's maxRunners (real cap is the namespace quota)
  # No minRunners: GAG scales to zero with no cold-start penalty.
```

Add one `RunnerSet` per ARC scale set you are migrating in this namespace — they all point at the same `ActionsGateway`, and share a `RunnerTemplate` wherever the pod shapes match.
Where they differ, add a template per shape.

### 3. Apply and validate provisioning

```sh
kubectl apply -f gateway.yaml
```

Wait for `Ready=True` on the gateway and each runner set, and confirm the AGC, proxy pool, RBAC, and NetworkPolicies came up — follow [tenant-onboarding Step 3](tenant-onboarding.md#step-3-validate-provisioning) and [Step 4](tenant-onboarding.md#step-4-validate-listener-sessions).
At this point **both** the ARC scale set and the GAG runner set are registered with GitHub; GitHub routes a matching job to whichever acquires it first.

### 4. Cut workflows over and verify

Run a test job targeting the label and confirm a GAG worker pod is created, egresses through the proxy, and is deleted on completion ([tenant-onboarding Step 5](tenant-onboarding.md#step-5-run-a-test-job)).
Because you carried the name across as the label, existing `runs-on: gpu-large` workflows already land on GAG.
Watch a few real jobs through the GAG path before removing ARC.

### 5. Decommission the ARC scale set

Once you trust the GAG path, scale the ARC scale set to zero / uninstall its Helm release:

```sh
helm uninstall gpu-large -n team-a   # removes the AutoscalingRunnerSet + its listener pod
```

Repeat steps 2–5 for each remaining scale set, then remove any bespoke NetworkPolicy / RBAC / quota manifests you hand-built around ARC — GAG reconciles those from the CRs now.

### Rollback

Nothing in this flow removes ARC until step 5, so rollback during migration is trivial: if the GAG path misbehaves, **keep the ARC scale set running** and delete the GAG `RunnerSet` (or point `runs-on` back at the ARC scale-set name if you gave the GAG set a different label).
Because both register independently with GitHub, there is no split-brain to untangle.

---

## Where to go next

- [Tenant onboarding checklist](tenant-onboarding.md) — the full pre-conditions → first-job reference this guide builds on.
- [Why GAG over ARC](../why-gag.md) — the capability-by-capability comparison.
- [Getting Started](../getting-started.md) — the end-to-end first install.
- [Network architecture](../design/network-architecture.md) — egress proxy and NetworkPolicy detail, with isolation-validation probes.
- [Troubleshooting](troubleshooting.md) — common first-day failures.
- [Deprecation and removal notice](v1alpha1-deprecation.md), covering what `v2.0.0` removes (`v1alpha1`, `v2alpha1`, Classic acquisition).
  Relevant only if you are *already* on GAG's v1 API; new migrations should not start there.
