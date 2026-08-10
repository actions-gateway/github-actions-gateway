# Installation

> **Audience:** Platform engineer

This is the operator reference for installing the Gateway Manager Controller (GMC) with the shipped **`actions-gateway` Helm chart**.
For day-2 operations after install, see [upgrade.md](upgrade.md).
For the full end-to-end walkthrough that continues past the GMC install into the GitHub App Secret and the first `ActionsGateway` CR, see [Getting Started](../getting-started.md). **Replacing Actions Runner Controller (ARC)?** After the GMC is installed, the [Migrating from ARC guide](migration-from-arc.md) maps ARC scale sets onto tenant gateways and walks one runner group across.

The Helm chart installs the GMC and its cluster prerequisites **only** — CRDs, RBAC, the validating webhook, the `namespace-psa-guard`, `gmc-tenant-resource-guard`, and `priorityclass-allowlist-guard` admission policies, and NetworkPolicies.
Per-tenant Actions Gateway Controller (AGC) instances and egress proxy pools are **not** chart resources; the GMC provisions them at runtime from each tenant's `ActionsGateway` CR.
The chart is the **sole** install path — there is no kustomize overlay; the plain-YAML files under `cmd/gmc/config/` are the controller-gen codegen + test substrate, not an install vehicle.
For the full chart reference (every value, the templates it renders, offline validation), see the [chart README](../../charts/actions-gateway/README.md).

---

## Prerequisites

- **Kubernetes >= 1.30** — the GMC's `namespace-psa-guard` and `gmc-tenant-resource-guard` policies need the GA `ValidatingAdmissionPolicy` API.
- **Node architecture: `linux/amd64` or `linux/arm64`.** Published images are multi-arch — one pinned digest (the OCI index digest) serves both, so mixed amd64/arm64 (e.g.
  Graviton) node pools need no per-arch configuration.
  Other architectures are not published.
- **A CNI that enforces `NetworkPolicy`** (Calico, Cilium) for the egress/ingress isolation controls to take effect. `kindnet` does not enforce egress, so the tenant-isolation guarantees do not hold under it. **GKE Dataplane V2** (Cilium) is supported and tested; if the cluster also runs **NodeLocal DNSCache**, use a GAG build that includes the Q229 fix (its DNS egress rule allows the `node-local-dns` redirect backend) — older builds drop DNS under Dataplane V2 and the tenant AGC crash-loops on its first GitHub token fetch (see [Troubleshooting → DNS Times Out Under the Egress NetworkPolicy](troubleshooting.md#dns-times-out-under-the-egress-networkpolicy-gke-dataplane-v2--nodelocal-dnscache)).
- **Webhook serving cert** — choose one:
  - **cert-manager** (the default, `certManager.enabled=true`).
    Install [cert-manager](https://cert-manager.io) first; it issues and rotates the webhook serving cert.
  - **Self-signed** (`certManager.enabled=false`).
    The chart generates a self-signed serving cert and wires the webhook `caBundle` itself — no cert-manager dependency. **Trade-off:** the cert rotates on a `helm upgrade` that cannot reuse the existing `webhook-server-cert` Secret; see the cert behavior notes in [upgrade.md](upgrade.md#gmc-install-and-upgrade-via-helm-recommended).
- **A GitHub App** with a private key and installation ID.
  The chart does *not* consume the App credential — it is referenced per tenant by the `ActionsGateway` CR you create after install (see [Getting Started](../getting-started.md)).
  You only need the App registered and installed before onboarding a tenant, not before installing the chart.
- **Image digests** for the GMC, AGC, proxy, and worker-wrapper images (see [Pin images by digest](#pin-images-by-digest) below).

---

## Preflight the cluster (required first step)

Before installing, **validate that the target cluster can actually uphold the tenant-isolation guarantees**.
The most dangerous failure mode is silent: installing onto a CNI that does not enforce `NetworkPolicy` (e.g. `kindnet`) leaves every NetworkPolicy the chart ships **inert**, so tenants are *not* confined — and nothing fails or warns at install time.
Run the preflight from a [source checkout](#install) first:

```sh
make validate-cluster
```

It checks four prerequisites and prints a clear `PASS`/`WARN`/`FAIL` line with a remediation hint for each:

| Check | Severity | Meaning |
|---|---|---|
| **CNI NetworkPolicy enforcement** | **FAIL** (blocking) | Detects the cluster CNI. An enforcing CNI (Calico, Cilium, Antrea, Weave Net, kube-router, Canal) passes; `kindnet` **fails loudly** — it does not enforce egress, so tenant isolation would be silently void. An unrecognised CNI warns (cannot confirm enforcement). |
| **Kubernetes >= 1.30** | **FAIL** (blocking) | The GMC's `namespace-psa-guard` / `gmc-tenant-resource-guard` policies need the GA `ValidatingAdmissionPolicy` API. |
| **cert-manager present** | WARN | Required only for the default cert path (`certManager.enabled=true`). An install with `--set certManager.enabled=false` uses the chart's self-signed fallback and does not need it. |
| **metrics-server present** | WARN | The resource metrics the GMC/AGC HorizontalPodAutoscalers consume, and the source for the AGC's [worker usage sampling](worker-rightsizing.md). Install succeeds without it; autoscaling and usage sampling stay degraded until it is present. Checked with a [bounded retry](#preflighting-a-freshly-created-cluster) — on a brand-new cluster the addon is often still starting. |

`make validate-cluster` exits non-zero on any blocking **FAIL** (or if the cluster is unreachable).
Warnings do not block the install; set `VALIDATE_STRICT=1` to treat them as failures too.
The check is detection-based — it schedules no workloads and needs no extra permissions, so it is safe to run against a fresh cluster.
Resolve every **FAIL** before proceeding; for CNI enforcement specifically, install on a cluster whose CNI enforces `NetworkPolicy` (see [Prerequisites](#prerequisites)).

### Preflighting a freshly created cluster

A cluster that was created minutes ago is still converging, so a one-shot check can report a prerequisite absent that is merely slow to start.
The metrics-server check therefore retries, bounded in wall clock:

- If the `metrics.k8s.io` APIService is registered but not yet `Available`, the preflight prints a `[WAIT]` line and retries for up to **150 s** (a managed addon typically needs ~2 min from cluster creation).
  It reports `PASS` as soon as the API serves, so `VALIDATE_STRICT=1` does not fail a from-zero bootstrap on an addon that is still coming up.
- If no `metrics.k8s.io` APIService is registered at all, metrics-server is not installed: the preflight warns after a short **15 s** grace rather than paying the full budget.

Both budgets are env-overridable — `VALIDATE_METRICS_TIMEOUT`, `VALIDATE_METRICS_GRACE`, and the `VALIDATE_METRICS_INTERVAL` poll interval, all in whole seconds.
Set `VALIDATE_METRICS_TIMEOUT=0 VALIDATE_METRICS_GRACE=0` to restore the previous single-shot behaviour (useful in a scripted preflight against a long-running cluster, where a converging addon is not expected).

---

## Install

> **Current release — `v1.4.0`.** The chart is published, cosign-signed, and installable straight from the GHCR OCI registry.
> Pin `--version 1.4.0` (the chart version is the release tag without the leading `v`).
> Newer patch releases publish as `1.4.z`; pin the version you have verified.

Install the published, signed chart straight from the registry — no source checkout needed:

```sh
helm install gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version 1.4.0 \
  --namespace gmc-system --create-namespace \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy> \
  --set wrapper.image.digest=sha256:<wrapper>
```

Copy the four image digests from the [release notes](https://github.com/actions-gateway/github-actions-gateway/releases/tag/v1.4.0) (the chart ships **no** baked-in digests — empty digests are the fail-closed secure default, so an unconfigured render is rejected).
Verify the chart and image signatures before installing — see [release.md § Verify the publish](release.md#3-verify-the-publish) and [security-operations.md § Image provenance](security-operations.md#image-provenance-signature--sbom-verification).

> **Installing from a source checkout** (dev/CI, or to install an unreleased chart) still works — substitute the local `charts/actions-gateway` path for the `oci://…` ref in any command on this page:
>
> ```sh
> helm install gag charts/actions-gateway \
>   --namespace gmc-system --create-namespace \
>   --set gmc.image.digest=sha256:<gmc> \
>   --set agc.image.digest=sha256:<agc> \
>   --set proxy.image.digest=sha256:<proxy> \
>   --set wrapper.image.digest=sha256:<wrapper>
> ```

`gag` is the Helm release name and `gmc-system` is the install namespace; both are conventions you can change.
Keep `namePrefix` at its default `gmc` unless you are running two GMCs in one cluster — the operational docs and the `namespace-psa-guard` / `gmc-tenant-resource-guard` policies match resources by that prefix.

### Pin images by digest

Digest pinning is enforced for all four images.
This is the secure default: a digest is immutable, so neither the controller nor a tenant gateway can ever run from a tag that was silently re-pointed.

- **All four images — enforced at render time.** `helm install` / `helm upgrade` / `helm template` **fail** with `<image>.image must be pinned by digest: set <image>.image.digest=sha256:<64 hex digits> …` (naming `gmc`, `agc`, `proxy`, or `wrapper`) when any of the four digests is empty.
  Failing at render surfaces a missing digest up front, at the same clear error — rather than letting an unpinned AGC/proxy/wrapper image install successfully and only crash-loop the GMC later.
  See the [troubleshooting runbook](troubleshooting.md#helm-render-fails-gmcimage-must-be-pinned-by-digest) if you hit this.
- **AGC/proxy/wrapper images — re-checked at GMC startup (second layer).** Even past render, the GMC **rejects floating `AGC_IMAGE` / `PROXY_IMAGE` / `WRAPPER_IMAGE` tags and crash-loops** until those three images are pinned by digest.
  The worker-wrapper (Q235) is on by default — the chart always sets `WRAPPER_IMAGE`, so `wrapper.image.digest` is required like the rest.

Pin `gmc.image.digest`, `agc.image.digest`, `proxy.image.digest`, and `wrapper.image.digest` as shown above.

For **dev/test only**, you can bypass the pin:

```sh
helm install gag charts/actions-gateway \
  --namespace gmc-system --create-namespace \
  --set allowFloatingImageTags=true \
  --set gmc.image.tag=<tag> --set agc.image.tag=<tag> \
  --set proxy.image.tag=<tag> --set wrapper.image.tag=<tag>
```

Do **not** set `allowFloatingImageTags=true` in production.

### Air-gapped / private registry

On a cluster that cannot pull from GHCR, relocate the images and chart to a private registry, point the chart at it, and authenticate the pulls — see [air-gapped-install.md](air-gapped-install.md).
Digest pinning is preserved throughout (relocation is content-addressed).

### Without cert-manager

```sh
helm install gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version 1.4.0 \
  --namespace gmc-system --create-namespace \
  --set certManager.enabled=false \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy> \
  --set wrapper.image.digest=sha256:<wrapper>
```

The chart generates a self-signed webhook serving cert and wires the `caBundle` itself.
Review the rotation trade-off in [Prerequisites](#prerequisites) before choosing this path.

### GKE and other restricted-PriorityClass clusters

GKE Standard (and any cluster whose API server enables the restricted `PriorityClass` admission config) permits the `system-node-critical` / `system-cluster-critical` priority classes **only** in a namespace that carries a `ResourceQuota` whose `scopeSelector` matches them.
The GMC runs with `priorityClassName: system-cluster-critical` by default — a deliberate secure default that protects the control plane from eviction — so without such a quota GKE rejects the GMC ReplicaSet's pods with:

```
FailedCreate: insufficient quota to match these scopes:
  [{PriorityClass In [system-node-critical system-cluster-critical]}]
```

and the Deployment never becomes Ready.

**No action required: the chart handles this for you.** It ships a scoped, permit-only `ResourceQuota` (`<namePrefix>-critical-pods`, default `gmc-critical-pods`) in the install namespace by default (`systemCriticalPriorityQuota.enabled=true`), so a stock `helm install` brings the GMC to Ready on GKE out of the box without downgrading the `system-cluster-critical` default.
The quota only *permits* the classes — its pod ceiling is generous and scoped to the system-critical classes, so it counts nothing but the GMC's own pods and never caps scheduling — and it is inert on clusters that don't enforce the restriction (they already permit the classes).
It renders only while `priorityClassName` is a system-critical class.

Set `--set systemCriticalPriorityQuota.enabled=false` only if you manage this quota out-of-band (e.g. a cluster-wide policy already provisions it).
Do **not** work around the admission rejection by clearing `priorityClassName` — that drops the GMC's eviction protection.

### GitOps (Argo CD / Flux)

To install the chart declaratively from Git instead of running `helm install` by hand, see [gitops.md](gitops.md).
It gives ready-to-apply Argo CD `Application` and Flux `HelmRelease` examples (with the CRD-pruning gotcha handled) and shows how to source the GitHub App credential Secret securely — External Secrets Operator or Sealed Secrets — so the private key is never committed to Git.

### Optional: the v2 API CRDs

The main chart installs the `v1alpha1` (`actions-gateway.github.com`) CRDs — still fully served, but [deprecated](v1alpha1-deprecation.md).
The v2 (`actions-gateway.com`) API that new tenants onboard on ships its five CRDs in a **separate, opt-in chart**, `actions-gateway-crds-v2`, split out so the main chart's Helm release Secret stays under the 1 MiB limit.
Install it alongside the main chart unless you are running v1-only.

Each v2 CRD is served at **two versions**: **`v2beta1`** (the served **and** storage version — the graduated, ScaleSet-only shape) and **`v2alpha1`** (served as the `gag-migrate` on-ramp, deprecated, and [removed at `v2.0.0`](v1alpha1-deprecation.md)).
The apiserver converts between them through a **conversion webhook hosted by the GMC** (the `/convert` endpoint on the same `webhook-service` that fronts the validating webhooks).
Two consequences for an operator:

- **The v2 CRD chart depends on the main chart + cert-manager.** The conversion webhook's `clientConfig` points at the GMC `webhook-service`, and cert-manager's ca-injector supplies its CA.
  Install the v2 CRD chart into the **same namespace as the GMC** (`--namespace gmc-system`) so the webhook `clientConfig` resolves, or set `conversion.webhook.service.namespace` explicitly.
  With cert-manager disabled, set `conversion.certManager.enabled=false` and supply `conversion.caBundle` (the CA that signed the GMC webhook serving cert).
- **Create v2 objects only once the GMC is running.** The CRDs may be installed in any order, but a `RunnerSet`/`RunnerTemplate`/… create is routed through `/convert`, so it fails until the GMC (the conversion webhook server) is `Ready`.
  On a fresh install this resolves itself; on an existing cluster, install the CRDs and let the GMC roll out before applying v2 objects.

They are genuinely optional:

- **You do not need them for v1.** A plain `helm install` of the main chart (without `actions-gateway-crds-v2`) is a complete, supported v1-only install.
  The GMC **detects the v2 CRDs at startup**: when they are absent it logs a single info line — `actions-gateway.com/v2alpha1 CRDs not installed; v2 controllers disabled` — and does **not** start the v2 controllers or the v2 IP-range refresh passes.
  Each per-tenant AGC does the same for its v2 RunnerSet reconciler, logging `actions-gateway.com/v2alpha1 RunnerSet CRD not installed; v1-only mode, v2 RunnerSet reconciler disabled` and running only the v1 RunnerGroup reconciler. v1alpha1 tenants reconcile normally.
- **To use v2,** apply the CRDs server-side — do **not** `helm install` the chart.
  The two `RunnerTemplate` CRDs each embed a full `PodTemplateSpec`, so the rendered chart (~2.5 MB) exceeds the 1 MiB Helm release-Secret limit and `helm install` cannot store it.
  Applying the CRDs server-side is the **supported, deliberate install/upgrade path**, not a stopgap (see below for why, and the [design decision](../design/appendix-h-v2-api-decomposition.md#h132-installupgrade-lifecycle--apply-render-not-helm-install-q276) for the rejected alternatives).
  Two equivalent ways:

  **Default namespace (`gmc-system`), no helm — simplest for a manual install.** Every release attaches a pre-rendered, cosign-signed `actions-gateway-crds-v2.yaml` to its [GitHub Release](https://github.com/actions-gateway/github-actions-gateway/releases).
  Apply it straight from the release URL (pin `vX.Y.Z` to the GMC image's release):

  ```sh
  kubectl apply --server-side -f \
    https://github.com/actions-gateway/github-actions-gateway/releases/download/vX.Y.Z/actions-gateway-crds-v2.yaml
  ```

  Optionally verify the asset's keyless signature first (no key material — the same Fulcio/Rekor identity that signs the images and charts).
  Download the manifest and its `.cosign.bundle` from the same release, then:

  ```sh
  cosign verify-blob --bundle actions-gateway-crds-v2.yaml.cosign.bundle \
    --certificate-identity-regexp '^https://github.com/actions-gateway/github-actions-gateway/\.github/workflows/publish\.yml@refs/tags/v.*$' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    actions-gateway-crds-v2.yaml
  ```

  **Custom GMC namespace, or a GitOps render — use helm.** The release asset bakes in the default `gmc-system` namespace for the conversion webhook `clientConfig`; if the GMC runs elsewhere (or cert-manager is disabled), render for that namespace and apply server-side (this carries the same `spec.conversion` wiring, resolved to your namespace):

  ```sh
  helm template actions-gateway-crds-v2 \
    oci://ghcr.io/actions-gateway/charts/actions-gateway-crds-v2 \
    --namespace <gmc-namespace> \
    | kubectl apply --server-side -f -
  ```

  Override `conversion.webhook.service.namespace` / `conversion.certManager.*` with `--set` if the GMC runs elsewhere or cert-manager is disabled.

  **Why apply-render and not `helm install`.** Server-side apply is declarative and idempotent, so install and upgrade are the *same* command; it also clears kubectl's 256 KB client-side-apply annotation ceiling, which each ~1.16 MB `RunnerTemplate` CRD exceeds on its own.
  This is the mainstream practice for large CRDs (cert-manager, Crossplane, Istio, Gateway API all apply their CRDs out-of-band).
  The properties it forgoes — `helm rollback`, `helm uninstall` cleanup — are ones you should not use on CRDs anyway: rolling a schema backward can strand stored objects, and a cascading CRD delete removes every custom resource with it.

  **Upgrade.** Re-apply the newer release the same way you installed — either `kubectl apply --server-side -f …/releases/download/vX.Y.Z/actions-gateway-crds-v2.yaml` with the newer tag, or re-run the `helm template … | kubectl apply --server-side` render with the newer chart (`--version <x.y.z>` for a pinned OCI ref).
  Server-side apply merges the new schema in place — additive field/version changes need no other step.
  On a running cluster the conversion `caBundle` is supplied by cert-manager's ca-injector, so it resolves post-apply; a `helm template` alone cannot look it up.

  **Rollback.** Re-apply the render of the *previous* chart version the same way.
  Treat this as a deliberate migration, not a casual undo: if the newer version added a served or storage version, or a field that existing objects now populate, rolling back can reject or truncate stored resources.
  Prefer rolling *forward* with a corrected chart.

  **Uninstall.** There is no Helm release to `helm uninstall`; remove the CRDs explicitly and only when you intend to — deleting a CRD **cascades to every custom resource of that kind** (`kubectl delete -f <(helm template actions-gateway-crds-v2 … )`).
  The templates carry `helm.sh/resource-policy: keep`, so a GitOps tool that fronts this chart with a real Helm release will not prune the CRDs on its own uninstall.

  Detection happens once at GMC startup, so **after installing the CRDs into a running v1-only cluster, restart the GMC** (`kubectl rollout restart deploy -n gmc-system gmc-controller-manager`) to enable the v2 controllers.
  On startup the GMC logs `actions-gateway.com/v2alpha1 CRDs detected; enabling v2 controllers`.

See [getting-started.md § the v2 API](../getting-started.md#4-create-your-gateway-and-runner-set-v2-recommended) for what v2 adds, and [§1 Deploy the GMC](../getting-started.md#1-deploy-the-gmc) for the v2 CRD chart install and the Kubernetes version requirements.

---

## Key values an operator sets

The chart ships secure, HA defaults; most installs only set the four image digests.
The knobs an operator is most likely to override:

| Key | Default | When you change it |
|---|---|---|
| `gmc.image.digest` / `agc.image.digest` / `proxy.image.digest` / `wrapper.image.digest` | `""` | Always — pin all four by digest. The chart refuses to render while any of the four is empty (the GMC additionally re-checks `agc`/`proxy`/`wrapper` at startup). |
| `allowFloatingImageTags` | `false` | Dev/test only — opt out of digest pinning (render-time check on all four images and the GMC's startup-time AGC/proxy/wrapper check). |
| `certManager.enabled` | `true` | Set `false` to use the self-signed webhook cert instead of cert-manager. |
| `namePrefix` | `gmc` | Only when running a second GMC in the same cluster. |
| `replicaCount` | `2` | Lower to `1` only in dev; production wants HA + leader election. |
| `metrics.serviceMonitor.enabled` | `false` | Set `true` if you run Prometheus Operator and want a `ServiceMonitor`. |
| `metrics.tls.certManager.enabled` | `true` | Leave on for a cert-manager-issued metrics cert that the `ServiceMonitor` verifies. Set `false` (or `certManager.enabled=false`) to scrape the self-signed metrics cert with `insecureSkipVerify` — a documented MITM trade-off, see [observability.md](observability-metrics-access.md#verifying-the-metrics-scrape-tls-gmc-manager). |
| `networkPolicy.enabled` | `true` | Leave on; needs an enforcing CNI (see prerequisites). |
| `vpa.enabled` | `false` | Set `true` to let a Vertical Pod Autoscaler right-size the GMC instead of tuning `resources` by hand. Requires the Kubernetes vertical-pod-autoscaler installed (the `autoscaling.k8s.io` CRDs) — enabling it without them fails `helm install`, same as `metrics.serviceMonitor.enabled`. `vpa.updateMode` defaults to `Off` (recommendation only); it moves requests only, so `resources.limits` stays your hard ceiling. The per-tenant equivalent is `ActionsGateway.spec.agcAutoscaling` ([tenant-onboarding](tenant-onboarding.md#letting-an-autoscaler-size-the-agc-agcautoscaling)). |
| `systemCriticalPriorityQuota.enabled` | `true` | Leave on; ships the scoped `ResourceQuota` that lets the GMC's `system-cluster-critical` pods schedule under GKE's restricted PriorityClass admission (see [GKE and other restricted-PriorityClass clusters](#gke-and-other-restricted-priorityclass-clusters)). Set `false` only if you provision that quota out-of-band. |
| `admissionPolicy.enabled` | `true` | Leave it on. It ships the `priorityclass-allowlist-guard` `ValidatingAdmissionPolicy`, the backstop that gates direct `runnergroups` writes (which have no webhook) against the PriorityClass allowlist. The managed-control-plane caveat that used to sit here is [resolved](#the-policy-parameter-is-a-crd-not-a-configmap-q492). Set `false` to rely on the GMC webhook allowlist alone. |

A `values.schema.json` validates these at install/lint time (digest format, enum values, etc.).
The **full reference** — every value with its default and description — lives in the [chart README](../../charts/actions-gateway/README.md#values); this table is only the common subset, not a duplicate.

### The policy parameter is a CRD, not a ConfigMap (Q492)

The guard reads its allowlist from a cluster-scoped `PriorityClassAllowlist` CR (`<release>-priorityclass-allowlist`), which the chart renders from `allowedPriorityClasses` and the GMC also watches.
One object, two consumers, so the webhook and the policy cannot disagree.

That it is a **CRD** and not a `ConfigMap` is load-bearing, not incidental.
Releases that used a ConfigMap were exposed to a kube-apiserver defect (Q444): the apiserver keys one *shared* parameter informer per core-type `paramKind`, tears it down as soon as no binding names that kind, and never restarts it.
A `helm uninstall` deletes the guard's only binding and so tripped exactly that, leaving every `runnergroups`/`runnersets`/`runnertemplates` write denied cluster-wide with `no params found for policy binding` — recoverable only by restarting kube-apiserver, which EKS/GKE/AKS do not offer.
It could also fail *open*, silently enforcing a stale allowlist from a frozen cache.

For a CRD `paramKind` the apiserver allocates a fresh dynamic informer per context, so the same uninstall/reinstall cycle is survivable.
This is measured rather than inferred: `scripts/e2e/vap-param-informer-check.sh` runs the identical empty-binding-set transition against both kinds on one apiserver, and `scripts/e2e/chart-reinstall-check.sh` drives a real uninstall/reinstall in CI.

**One consequence, and it is not install-time.** This CRD ships in the chart-root `crds/` directory rather than `templates/crds/` like the others, because the same release also creates a `PriorityClassAllowlist` object and Helm resolves REST mappings for the whole manifest before applying any of it — a CR whose CRD is a template in the same release fails the install outright.
A **fresh `helm install` handles this for you**; `crds/` is applied first.

An **upgrade does not**.
Helm skips `crds/` entirely on upgrade, so applying the chart's CRDs is a standard pre-upgrade step for this chart — run unconditionally rather than conditionally, so the procedure does not depend on which version you are coming from:

```sh
helm show crds <chart> --version <version> | kubectl apply -f -
```

It is idempotent and version-correct, so it also covers any future schema change to this CRD with no release-note callout to remember.
The chart preflights the CRD's presence and fails with that command rather than letting Helm emit a bare `no matches for kind`.
Steps: [upgrade.md](upgrade.md#gmc-install-and-upgrade-via-helm-recommended).

`helm uninstall` leaves the CRD behind, which is the same end state as the `helm.sh/resource-policy: keep` the other CRDs carry.

Upstream, for the underlying apiserver bug: [kubernetes/kubernetes#130887](https://github.com/kubernetes/kubernetes/issues/130887).
Migrating an existing release off the ConfigMap: [upgrade.md](upgrade.md#priorityclass-allowlist-configmap-to-cr).


---

## Verify a healthy install

```sh
# 1. Both GMC replicas are Running and Ready (HA default replicaCount=2).
kubectl get deploy -n gmc-system gmc-controller-manager
# Expected: READY 2/2

kubectl get pods -n gmc-system -l app.kubernetes.io/name=actions-gateway
# Expected: 2 pods, all Running

# 2. A leader has been elected (the lease holder is the active replica).
kubectl get lease -n gmc-system
# Expected: a lease whose HOLDER is one of the GMC pods

# 3. Both CRDs are installed and Established.
kubectl get crd actionsgateways.actions-gateway.github.com \
                runnergroups.actions-gateway.github.com
kubectl wait --for=condition=Established \
  crd/actionsgateways.actions-gateway.github.com \
  crd/runnergroups.actions-gateway.github.com

# 4. The validating webhook and the admission policies are present.
# (Resource names carry the chart namePrefix, default "gmc-".)
kubectl get validatingwebhookconfiguration | grep actions-gateway
kubectl get validatingadmissionpolicy gmc-namespace-psa-guard gmc-tenant-resource-guard \
  gmc-priorityclass-allowlist-guard

# 5. No errors in the GMC manager logs.
kubectl logs -n gmc-system deploy/gmc-controller-manager --tail=30
# Look for: "Starting workers" / "successfully acquired lease"; no repeated
# "AGC_IMAGE/PROXY_IMAGE/WRAPPER_IMAGE must be pinned by digest".
```

A forgotten `agc`/`proxy`/`wrapper` digest now fails the **render** with a clear per-image message (see [Pin images by digest](#pin-images-by-digest)), so a digest slip is caught before install rather than as a GMC `CrashLoopBackOff`.
The GMC's startup pin check (message above) remains a second layer for non-chart deployments.
For other failure modes, see [troubleshooting.md](troubleshooting.md).

Once the GMC is healthy, continue with [Getting Started](../getting-started.md) to create the GitHub App Secret and the first `ActionsGateway` CR, or follow the [Tenant Onboarding Checklist](tenant-onboarding.md) to bring up a tenant.

---

## Uninstall

```sh
helm uninstall gag --namespace gmc-system
```

The two CRDs ship as templates carrying `helm.sh/resource-policy: keep`, so `helm uninstall` **preserves the CRDs and every tenant's `ActionsGateway` / `RunnerGroup` object** rather than cascade-deleting them — uninstalling the GMC does not tear down running tenant gateways.
To fully remove them, delete the tenant CRs first, then the CRDs explicitly:

```sh
# Only after confirming no tenant still needs their gateways:
kubectl delete actionsgateway --all --all-namespaces
kubectl delete crd actionsgateways.actions-gateway.github.com \
                   runnergroups.actions-gateway.github.com
```

The `priorityclass-allowlist-guard` `ValidatingAdmissionPolicy` is removed by `helm uninstall` along with its binding and its parameter object; the `PriorityClassAllowlist` **CRD** is left behind, as `crds/` contents always are.
Removing the binding used to be the trigger for a cluster-wide outage (Q444) when the parameter was a ConfigMap — a reinstall could not repair it.
It is safe now that the parameter is a CRD, which is [why it is one](#the-policy-parameter-is-a-crd-not-a-configmap-q492); the uninstall/reinstall cycle is covered by `scripts/e2e/chart-reinstall-check.sh` in CI.
Symptoms of the old failure, if you meet it on a pre-Q492 release: [troubleshooting.md](troubleshooting.md#every-runnergroup--runnerset-write-denied-no-params-found-for-policy-binding).

The install namespace (`gmc-system`) is left in place if you created it with `--create-namespace`; remove it with `kubectl delete namespace gmc-system` once empty.

---

## Next steps

- [Getting Started](../getting-started.md) — the full walkthrough from here: GitHub App Secret, `ActionsGateway` CR, first job.
- [Tenant Onboarding Checklist](tenant-onboarding.md) — onboard a tenant team.
- [upgrade.md](upgrade.md) — day-2 `helm upgrade` / `helm rollback`, CRD field changes, and the per-component upgrade procedures.

---

← [Back to Operations](README.md)
