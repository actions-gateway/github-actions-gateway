# actions-gateway Helm chart

Installs the **Gateway Manager Controller (GMC)** — the operator that
provisions isolated per-tenant gateways from `ActionsGateway` custom resources.
This chart deploys the GMC and its cluster prerequisites only. Per-tenant
Actions Gateway Controller (AGC) instances and egress proxy pools are
**provisioned by the GMC at runtime** from each `ActionsGateway` CR; they are
not chart resources.

> The `cmd/gmc/config/` and `cmd/agc/config/` kustomize bases remain the
> dev/CI source of truth (they back `make manifests`, envtest, and e2e). This
> chart is the shipped install artifact, generated to match them. Helm was
> chosen over a Kustomize overlay for versioned releases and a real day-2
> `helm upgrade`/`rollback` lifecycle (decision D-M5-1).

## What it installs

- The two CRDs — `ActionsGateway` and `RunnerGroup` — under `templates/crds/`
  with `helm.sh/resource-policy: keep` so `helm upgrade` carries CRD field
  changes (Helm never upgrades the chart-root `crds/` dir) and `helm uninstall`
  preserves tenant objects.
- The GMC `Deployment` (HA: 2 replicas + leader election + PDB), `ServiceAccount`,
  cluster/namespaced RBAC, and the `agc-tenant-role` ClusterRole.
- The validating webhook (`ValidatingWebhookConfiguration` + Service) and its
  serving cert (cert-manager or self-signed — see below).
- The `namespace-psa-guard` ValidatingAdmissionPolicy (constrains the GMC's
  namespace-patch grant to PSA labels on marked tenants).
- NetworkPolicies (default-deny ingress + metrics/webhook allows) and the
  metrics Service / optional ServiceMonitor.

## Prerequisites

- Kubernetes **>= 1.30** (GA `ValidatingAdmissionPolicy`).
- A CNI that enforces `NetworkPolicy` (Calico/Cilium) for the egress/ingress
  controls to take effect. `kindnet` does not enforce egress.
- **cert-manager** *if* `certManager.enabled=true` (the default). Not required
  when you set `certManager.enabled=false`.
- **Image digests** for the AGC and proxy images (see below).

## Install

```sh
helm install gag charts/actions-gateway \
  --namespace gmc-system --create-namespace \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy>
```

The GMC **rejects floating `AGC_IMAGE`/`PROXY_IMAGE` tags** and crash-loops
until they are pinned by digest — this is the secure default. Pin
`agc.image.digest` and `proxy.image.digest` (and `gmc.image.digest`) before
installing, or pass `--set allowFloatingImageTags=true` for **dev/test only**.

### Without cert-manager

```sh
helm install gag charts/actions-gateway \
  --namespace gmc-system --create-namespace \
  --set certManager.enabled=false \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy>
```

The chart generates a self-signed webhook serving cert and wires the webhook
`caBundle` itself. **Trade-off:** the cert rotates on a `helm upgrade` that
cannot reuse the existing Secret — see [upgrade](../../docs/operations/upgrade.md).

## Upgrade

```sh
helm upgrade gag charts/actions-gateway --namespace gmc-system --reuse-values
```

CRDs ship as templates, so field changes are applied on upgrade. The
`namespace-psa-guard` binding denies by default; if you are upgrading a cluster
whose tenant namespaces are not yet labeled
`actions-gateway.github.com/tenant=true`, label them first (or temporarily set
the binding to `Audit`) — see [upgrade](../../docs/operations/upgrade.md).

## Values

| Key | Default | Description |
|---|---|---|
| `namePrefix` | `gmc` | Prefix for all GMC resource names; also the SA identity the PSA-guard policy matches. Keep as `gmc` unless running two GMCs. |
| `replicaCount` | `2` | GMC controller-manager replicas (HA). |
| `gmc.image.repository` | `ghcr.io/actions-gateway/gmc` | GMC image repo. |
| `gmc.image.tag` | `""` | GMC tag (used only when digest empty). |
| `gmc.image.digest` | `""` | GMC image digest (`sha256:…`); takes precedence over tag. |
| `gmc.imagePullPolicy` | `IfNotPresent` | GMC image pull policy. |
| `agc.image.{repository,tag,digest}` | `ghcr.io/actions-gateway/agc`, `""`, `""` | Image the GMC **injects** into provisioned AGCs. Digest required by default. |
| `proxy.image.{repository,tag,digest}` | `ghcr.io/actions-gateway/proxy`, `""`, `""` | Image the GMC **injects** into provisioned proxy pools. Digest required by default. |
| `allowFloatingImageTags` | `false` | Dev/test opt-out of AGC/proxy digest pinning. **Do not enable in production.** |
| `leaderElection.enabled` | `true` | Pass `--leader-elect`. Keep on when `replicaCount > 1`. |
| `metrics.enabled` | `true` | Expose the HTTPS `:8443` metrics endpoint + Service. |
| `metrics.serviceMonitor.enabled` | `false` | Emit a Prometheus-Operator ServiceMonitor (needs its CRD). |
| `networkPolicy.enabled` | `true` | Ship the GMC ingress NetworkPolicies (needs an enforcing CNI). |
| `podDisruptionBudget.enabled` | `true` | Ship the `minAvailable: 1` PDB. |
| `admissionPolicy.enabled` | `true` | Ship the `namespace-psa-guard` VAP + binding (needs k8s ≥ 1.30). |
| `certManager.enabled` | `true` | Issue the webhook cert via cert-manager; `false` uses the self-signed fallback. |
| `certManager.selfSignedCertDurationDays` | `3650` | Validity of the self-signed cert when cert-manager is disabled. |
| `resources` | cpu 10m–500m / mem 64–128Mi | GMC container resources. |
| `priorityClassName` | `system-cluster-critical` | GMC PriorityClass (`""` to disable). |
| `nodeSelector` / `tolerations` / `affinity` | `{}` / `[]` / `{}` | GMC pod scheduling. |
| `sampleGateway.create` | `false` | Render an example `ActionsGateway` (dev only). |
| `sampleGateway.securityProfile` | `baseline` | Profile for the sample CR (`baseline`/`restricted`/`privileged`). |
| `sampleGateway.gitHubAppSecretName` | `github-app-v1` | GitHub App Secret name referenced by the sample CR. |

A `values.schema.json` validates these at install/lint time (image digest
format, security-profile enum, pull-policy enum, etc.).

## Offline validation

```sh
helm lint charts/actions-gateway
helm template gag charts/actions-gateway --namespace gmc-system | \
  kubeconform -strict -summary -kubernetes-version 1.30.0 \
    -skip CustomResourceDefinition,ActionsGateway,RunnerGroup,Certificate,Issuer,ServiceMonitor
```

The `-skip` list covers the CRDs and the CRs whose schemas (cert-manager,
Prometheus Operator) are not in kubeconform's default store.
