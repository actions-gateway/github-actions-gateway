# Installation

> **Audience:** Platform engineer

This is the operator reference for installing the
Gateway Manager Controller (GMC) with the shipped **`actions-gateway` Helm
chart**. For day-2 operations after install, see [upgrade.md](upgrade.md). For
the full end-to-end walkthrough that continues past the GMC install into the
GitHub App Secret and the first `ActionsGateway` CR, see
[Getting Started](../getting-started.md). **Replacing Actions Runner Controller (ARC)?**
After the GMC is installed, the [Migrating from ARC guide](migration-from-arc.md)
maps ARC scale sets onto tenant gateways and walks one runner group across.

The Helm chart installs the GMC and its cluster prerequisites **only** — CRDs,
RBAC, the validating webhook, the `namespace-psa-guard` and
`gmc-tenant-resource-guard` admission policies, and NetworkPolicies. Per-tenant Actions Gateway Controller (AGC) instances and
egress proxy pools are **not** chart resources; the GMC provisions them at
runtime from each tenant's `ActionsGateway` CR. The chart is the **sole** install
path — there is no kustomize overlay; the plain-YAML files under `cmd/gmc/config/`
are the controller-gen codegen + test substrate, not an install vehicle. For the
full chart reference (every value, the templates it renders, offline validation),
see the [chart README](../../charts/actions-gateway/README.md).

---

## Prerequisites

- **Kubernetes >= 1.30** — the GMC's `namespace-psa-guard` and
  `gmc-tenant-resource-guard` policies need the GA `ValidatingAdmissionPolicy` API.
- **Node architecture: `linux/amd64` or `linux/arm64`.** Published images are
  multi-arch — one pinned digest (the OCI index digest) serves both, so mixed
  amd64/arm64 (e.g. Graviton) node pools need no per-arch configuration. Other
  architectures are not published.
- **A CNI that enforces `NetworkPolicy`** (Calico, Cilium) for the egress/ingress
  isolation controls to take effect. `kindnet` does not enforce egress, so the
  tenant-isolation guarantees do not hold under it. **GKE Dataplane V2** (Cilium)
  is supported and tested; if the cluster also runs **NodeLocal DNSCache**, use a
  GAG build that includes the Q229 fix (its DNS egress rule allows the
  `node-local-dns` redirect backend) — older builds drop DNS under Dataplane V2 and
  the tenant AGC crash-loops on its first GitHub token fetch (see
  [Troubleshooting → DNS Times Out Under the Egress NetworkPolicy](troubleshooting.md#dns-times-out-under-the-egress-networkpolicy-gke-dataplane-v2--nodelocal-dnscache)).
- **Webhook serving cert** — choose one:
  - **cert-manager** (the default, `certManager.enabled=true`). Install
    [cert-manager](https://cert-manager.io) first; it issues and rotates the
    webhook serving cert.
  - **Self-signed** (`certManager.enabled=false`). The chart generates a
    self-signed serving cert and wires the webhook `caBundle` itself — no
    cert-manager dependency. **Trade-off:** the cert rotates on a `helm upgrade`
    that cannot reuse the existing `webhook-server-cert` Secret; see the cert
    behavior notes in [upgrade.md](upgrade.md#gmc-install-and-upgrade-via-helm-recommended).
- **A GitHub App** with a private key and installation ID. The chart does *not*
  consume the App credential — it is referenced per tenant by the
  `ActionsGateway` CR you create after install (see
  [Getting Started](../getting-started.md)). You only need the App registered
  and installed before onboarding a tenant, not before installing the chart.
- **Image digests** for the GMC, AGC, and proxy images (see
  [Pin images by digest](#pin-images-by-digest) below).

---

## Preflight the cluster (required first step)

Before installing, **validate that the target cluster can actually uphold the
tenant-isolation guarantees**. The most dangerous failure mode is silent:
installing onto a CNI that does not enforce `NetworkPolicy` (e.g. `kindnet`)
leaves every NetworkPolicy the chart ships **inert**, so tenants are *not*
confined — and nothing fails or warns at install time. Run the preflight from a
[source checkout](#install) first:

```sh
make validate-cluster
```

It checks four prerequisites and prints a clear `PASS`/`WARN`/`FAIL` line with a
remediation hint for each:

| Check | Severity | Meaning |
|---|---|---|
| **CNI NetworkPolicy enforcement** | **FAIL** (blocking) | Detects the cluster CNI. An enforcing CNI (Calico, Cilium, Antrea, Weave Net, kube-router, Canal) passes; `kindnet` **fails loudly** — it does not enforce egress, so tenant isolation would be silently void. An unrecognised CNI warns (cannot confirm enforcement). |
| **Kubernetes >= 1.30** | **FAIL** (blocking) | The GMC's `namespace-psa-guard` / `gmc-tenant-resource-guard` policies need the GA `ValidatingAdmissionPolicy` API. |
| **cert-manager present** | WARN | Required only for the default cert path (`certManager.enabled=true`). An install with `--set certManager.enabled=false` uses the chart's self-signed fallback and does not need it. |
| **metrics-server present** | WARN | The resource metrics the GMC/AGC HorizontalPodAutoscalers consume. Install succeeds without it; autoscaling stays degraded until it is present. |

`make validate-cluster` exits non-zero on any blocking **FAIL** (or if the
cluster is unreachable). Warnings do not block the install; set
`VALIDATE_STRICT=1` to treat them as failures too. The check is detection-based —
it schedules no workloads and needs no extra permissions, so it is safe to run
against a fresh cluster. Resolve every **FAIL** before proceeding; for CNI
enforcement specifically, install on a cluster whose CNI enforces
`NetworkPolicy` (see [Prerequisites](#prerequisites)).

---

## Install

> **General availability — `v1.0.0`.** The chart is published, cosign-signed,
> and installable straight from the GHCR OCI registry. Pin `--version 1.0.0`
> (the chart version is the release tag without the leading `v`). Newer patch
> releases publish as `1.0.z`; pin the version you have verified.

Install the published, signed chart straight from the registry — no source
checkout needed:

```sh
helm install gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version 1.0.0 \
  --namespace gmc-system --create-namespace \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy>
```

Copy the three image digests from the
[release notes](https://github.com/actions-gateway/github-actions-gateway/releases/tag/v1.0.0)
(the chart ships **no** baked-in digests — empty digests are the fail-closed
secure default, so an unconfigured render is rejected). Verify the chart and
image signatures before installing — see
[release.md § Verify the publish](release.md#3-verify-the-publish) and
[security-operations.md § Image provenance](security-operations.md#image-provenance-signature--sbom-verification).

> **Installing from a source checkout** (dev/CI, or to install an unreleased
> chart) still works — substitute the local `charts/actions-gateway` path for the
> `oci://…` ref in any command on this page:
>
> ```sh
> helm install gag charts/actions-gateway \
>   --namespace gmc-system --create-namespace \
>   --set gmc.image.digest=sha256:<gmc> \
>   --set agc.image.digest=sha256:<agc> \
>   --set proxy.image.digest=sha256:<proxy>
> ```

`gag` is the Helm release name and `gmc-system` is the install namespace; both
are conventions you can change. Keep `namePrefix` at its default `gmc` unless
you are running two GMCs in one cluster — the operational docs and the
`namespace-psa-guard` / `gmc-tenant-resource-guard` policies match resources by
that prefix.

### Pin images by digest

Digest pinning is enforced for all three images, at two layers. This is the
secure default: a digest is immutable, so neither the controller nor a tenant
gateway can ever run from a tag that was silently re-pointed.

- **GMC image — enforced at render time.** `helm install` / `helm upgrade` /
  `helm template` **fail** with
  `gmc.image must be pinned by digest: set gmc.image.digest=sha256:<64 hex digits> …`
  when `gmc.image.digest` is empty. See the
  [troubleshooting runbook](troubleshooting.md#helm-render-fails-gmcimage-must-be-pinned-by-digest)
  if you hit this.
- **AGC/proxy images — enforced at GMC startup.** The GMC **rejects floating
  `AGC_IMAGE` / `PROXY_IMAGE` tags and crash-loops** until the AGC and proxy
  images are pinned by digest.

Pin `gmc.image.digest`, `agc.image.digest`, and `proxy.image.digest` as shown
above.

For **dev/test only**, you can bypass the pin:

```sh
helm install gag charts/actions-gateway \
  --namespace gmc-system --create-namespace \
  --set allowFloatingImageTags=true \
  --set gmc.image.tag=<tag> --set agc.image.tag=<tag> --set proxy.image.tag=<tag>
```

Do **not** set `allowFloatingImageTags=true` in production.

### Air-gapped / private registry

On a cluster that cannot pull from GHCR, relocate the images and chart to a
private registry, point the chart at it, and authenticate the pulls — see
[air-gapped-install.md](air-gapped-install.md). Digest pinning is preserved
throughout (relocation is content-addressed).

### Without cert-manager

```sh
helm install gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version 1.0.0 \
  --namespace gmc-system --create-namespace \
  --set certManager.enabled=false \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy>
```

The chart generates a self-signed webhook serving cert and wires the `caBundle`
itself. Review the rotation trade-off in
[Prerequisites](#prerequisites) before choosing this path.

### GKE and other restricted-PriorityClass clusters

GKE Standard (and any cluster whose API server enables the restricted
`PriorityClass` admission config) permits the `system-node-critical` /
`system-cluster-critical` priority classes **only** in a namespace that carries a
`ResourceQuota` whose `scopeSelector` matches them. The GMC runs with
`priorityClassName: system-cluster-critical` by default — a deliberate secure
default that protects the control plane from eviction — so without such a quota
GKE rejects the GMC ReplicaSet's pods with:

```
FailedCreate: insufficient quota to match these scopes:
  [{PriorityClass In [system-node-critical system-cluster-critical]}]
```

and the Deployment never becomes Ready.

**No action required: the chart handles this for you.** It ships a scoped,
permit-only `ResourceQuota` (`<namePrefix>-critical-pods`, default
`gmc-critical-pods`) in the install namespace by default
(`systemCriticalPriorityQuota.enabled=true`), so a stock `helm install` brings
the GMC to Ready on GKE out of the box without downgrading the
`system-cluster-critical` default. The quota only *permits* the classes — its pod
ceiling is generous and scoped to the system-critical classes, so it counts
nothing but the GMC's own pods and never caps scheduling — and it is inert on
clusters that don't enforce the restriction (they already permit the classes). It
renders only while `priorityClassName` is a system-critical class.

Set `--set systemCriticalPriorityQuota.enabled=false` only if you manage this
quota out-of-band (e.g. a cluster-wide policy already provisions it). Do **not**
work around the admission rejection by clearing `priorityClassName` — that drops
the GMC's eviction protection.

### GitOps (Argo CD / Flux)

To install the chart declaratively from Git instead of running `helm install` by
hand, see [gitops.md](gitops.md). It gives ready-to-apply Argo CD `Application` and
Flux `HelmRelease` examples (with the CRD-pruning gotcha handled) and shows how to
source the GitHub App credential Secret securely — External Secrets Operator or
Sealed Secrets — so the private key is never committed to Git.

### Optional: the v2alpha1 (alpha) API CRDs

The main chart installs the `v1alpha1` (`actions-gateway.github.com`) CRDs — the
fully supported, standard path. The **alpha** `v2alpha1` (`actions-gateway.com`)
API ships its five CRDs in a **separate, opt-in chart**,
`actions-gateway-crds-v2`, split out so the main chart's Helm release Secret
stays under the 1 MiB limit. They are genuinely optional:

- **You do not need them for v1.** A plain `helm install` of the main chart
  (without `actions-gateway-crds-v2`) is a complete, supported v1-only install.
  The GMC **detects the v2 CRDs at startup**: when they are absent it logs a
  single info line — `actions-gateway.com/v2alpha1 CRDs not installed; v2
  controllers disabled` — and does **not** start the v2 controllers or the v2
  IP-range refresh passes. v1alpha1 tenants reconcile normally.
- **To use v2,** install the CRD chart alongside the main chart (any order):

  ```sh
  helm install actions-gateway-crds-v2 \
    oci://ghcr.io/actions-gateway/charts/actions-gateway-crds-v2
  ```

  Detection happens once at GMC startup, so **after installing the CRDs into a
  running v1-only cluster, restart the GMC** (`kubectl rollout restart deploy -n
  gmc-system gmc-controller-manager`) to enable the v2 controllers. On startup
  the GMC logs `actions-gateway.com/v2alpha1 CRDs detected; enabling v2
  controllers`.

See [getting-started.md § the v2alpha1 API](../getting-started.md#optional-the-v2alpha1-api-alpha)
for what v2 adds and the Kubernetes version requirements.

---

## Key values an operator sets

The chart ships secure, HA defaults; most installs only set the three image
digests. The knobs an operator is most likely to override:

| Key | Default | When you change it |
|---|---|---|
| `gmc.image.digest` / `agc.image.digest` / `proxy.image.digest` | `""` | Always — pin all three by digest. The chart refuses to render while `gmc.image.digest` is empty. |
| `allowFloatingImageTags` | `false` | Dev/test only — opt out of digest pinning (render-time GMC check and startup-time AGC/proxy check). |
| `certManager.enabled` | `true` | Set `false` to use the self-signed webhook cert instead of cert-manager. |
| `namePrefix` | `gmc` | Only when running a second GMC in the same cluster. |
| `replicaCount` | `2` | Lower to `1` only in dev; production wants HA + leader election. |
| `metrics.serviceMonitor.enabled` | `false` | Set `true` if you run Prometheus Operator and want a `ServiceMonitor`. |
| `metrics.tls.certManager.enabled` | `true` | Leave on for a cert-manager-issued metrics cert that the `ServiceMonitor` verifies. Set `false` (or `certManager.enabled=false`) to scrape the self-signed metrics cert with `insecureSkipVerify` — a documented MITM trade-off, see [observability.md](observability.md#verifying-the-metrics-scrape-tls-gmc-manager). |
| `networkPolicy.enabled` | `true` | Leave on; needs an enforcing CNI (see prerequisites). |
| `systemCriticalPriorityQuota.enabled` | `true` | Leave on; ships the scoped `ResourceQuota` that lets the GMC's `system-cluster-critical` pods schedule under GKE's restricted PriorityClass admission (see [GKE and other restricted-PriorityClass clusters](#gke-and-other-restricted-priorityclass-clusters)). Set `false` only if you provision that quota out-of-band. |

A `values.schema.json` validates these at install/lint time (digest format,
enum values, etc.). The **full reference** — every value with its default and
description — lives in the
[chart README](../../charts/actions-gateway/README.md#values); this table is
only the common subset, not a duplicate.

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

# 4. The validating webhook and both admission policies are present.
# (Resource names carry the chart namePrefix, default "gmc-".)
kubectl get validatingwebhookconfiguration | grep actions-gateway
kubectl get validatingadmissionpolicy gmc-namespace-psa-guard gmc-tenant-resource-guard

# 5. No errors in the GMC manager logs.
kubectl logs -n gmc-system deploy/gmc-controller-manager --tail=30
# Look for: "Starting workers" / "successfully acquired lease"; no repeated
# "AGC_IMAGE must be pinned by digest" (that means a floating-tag crash-loop).
```

If the GMC pods are in `CrashLoopBackOff` with an image-pinning error, you
installed with a floating AGC/proxy tag and without `allowFloatingImageTags` —
re-run the install with the three digests pinned (see
[Pin images by digest](#pin-images-by-digest)). For other failure modes, see
[troubleshooting.md](troubleshooting.md).

Once the GMC is healthy, continue with [Getting Started](../getting-started.md)
to create the GitHub App Secret and the first `ActionsGateway` CR, or follow the
[Tenant Onboarding Checklist](tenant-onboarding.md) to bring up a tenant.

---

## Uninstall

```sh
helm uninstall gag --namespace gmc-system
```

The two CRDs ship as templates carrying `helm.sh/resource-policy: keep`, so
`helm uninstall` **preserves the CRDs and every tenant's `ActionsGateway` /
`RunnerGroup` object** rather than cascade-deleting them — uninstalling the GMC
does not tear down running tenant gateways. To fully remove them, delete the
tenant CRs first, then the CRDs explicitly:

```sh
# Only after confirming no tenant still needs their gateways:
kubectl delete actionsgateway --all --all-namespaces
kubectl delete crd actionsgateways.actions-gateway.github.com \
                   runnergroups.actions-gateway.github.com
```

The install namespace (`gmc-system`) is left in place if you created it with
`--create-namespace`; remove it with `kubectl delete namespace gmc-system` once
empty.

---

## Next steps

- [Getting Started](../getting-started.md) — the full walkthrough from here:
  GitHub App Secret, `ActionsGateway` CR, first job.
- [Tenant Onboarding Checklist](tenant-onboarding.md) — onboard a tenant team.
- [upgrade.md](upgrade.md) — day-2 `helm upgrade` / `helm rollback`, CRD field
  changes, and the per-component upgrade procedures.

---

← [Back to Operations](.)
