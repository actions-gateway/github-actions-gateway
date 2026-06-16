# actions-gateway Helm chart

Installs the **Gateway Manager Controller (GMC)** — the operator that
provisions isolated per-tenant gateways from `ActionsGateway` custom resources.
This chart deploys the GMC and its cluster prerequisites only. Per-tenant
Actions Gateway Controller (AGC) instances and egress proxy pools are
**provisioned by the GMC at runtime** from each `ActionsGateway` CR; they are
not chart resources.

> This chart is the **sole** install path (Q142) — there is no kustomize
> overlay. The plain-YAML files under `cmd/gmc/config/` and `cmd/agc/config/`
> are the controller-gen codegen + envtest substrate (they back `make manifests`
> and envtest) and the single-source inputs to this chart's CRD/RBAC generators;
> they are not an install vehicle. Helm was chosen over a Kustomize overlay for
> versioned releases and a real day-2 `helm upgrade`/`rollback` lifecycle
> (decision D-M5-1).

## What it installs

- The two CRDs — `ActionsGateway` and `RunnerGroup` — under `templates/crds/`
  with `helm.sh/resource-policy: keep` so `helm upgrade` carries CRD field
  changes (Helm never upgrades the chart-root `crds/` dir) and `helm uninstall`
  preserves tenant objects. **These files are generated** from the authoritative
  controller-gen sources (`cmd/*/config/crd`) by `make chart-crds` — do not
  hand-edit them; a CI drift gate (`make chart-crds-check`) fails if they fall
  out of sync. See [code-generation.md](../../docs/development/code-generation.md).
- The GMC `Deployment` (HA: 2 replicas + leader election + PDB), `ServiceAccount`,
  cluster/namespaced RBAC, and the `agc-tenant-role` ClusterRole. The manager
  ClusterRole's **rules are generated** from the controller-gen output of the
  GMC's `+kubebuilder:rbac` markers (`cmd/gmc/config/rbac/role.yaml`) into
  `files/manager-role-rules.yaml` by `make chart-rbac`; a CI drift gate
  (`make chart-rbac-check`) fails if they fall out of sync. Do not hand-edit them.
- The validating webhook (`ValidatingWebhookConfiguration` + Service) and its
  serving cert (cert-manager or self-signed — see below).
- Two ValidatingAdmissionPolicies that confine the GMC's cluster-wide write
  grants to marked tenant namespaces: `namespace-psa-guard` (the namespace
  PSA-label patch) and `tenant-resource-guard` (create/update/delete of
  Deployments, Secrets, RoleBindings, NetworkPolicies, etc.).
- NetworkPolicies (default-deny ingress + metrics/webhook allows) and the
  metrics Service / optional ServiceMonitor.

## Prerequisites

- Kubernetes **>= 1.30** (GA `ValidatingAdmissionPolicy`).
- A CNI that enforces `NetworkPolicy` (Calico/Cilium) for the egress/ingress
  controls to take effect. `kindnet` does not enforce egress.
- **cert-manager** *if* `certManager.enabled=true` (the default). Not required
  when you set `certManager.enabled=false`.
- **Image digests** for the GMC, AGC, and proxy images (see below).

## Install

```sh
helm install gag charts/actions-gateway \
  --namespace gmc-system --create-namespace \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy>
```

Digest pinning is enforced for all three images — this is the secure default:

- **GMC (render time):** the chart **fails to render** with
  `gmc.image must be pinned by digest …` when `gmc.image.digest` is empty, so
  the controller image can never silently fall back to a mutable `:latest` tag.
- **AGC/proxy (startup time):** the GMC **rejects floating
  `AGC_IMAGE`/`PROXY_IMAGE` tags** and crash-loops until they are pinned.

Pin `gmc.image.digest`, `agc.image.digest`, and `proxy.image.digest` before
installing, or pass `--set allowFloatingImageTags=true` — the one explicit
opt-out covering both layers — for **dev/test only**.

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
`namespace-psa-guard` and `tenant-resource-guard` bindings deny by default; if
you are upgrading a cluster whose tenant namespaces are not yet labeled
`actions-gateway.github.com/tenant=true`, label them first (or temporarily set
both bindings to `Audit`) — see [upgrade](../../docs/operations/upgrade.md).

## Values

| Key | Default | Description |
|---|---|---|
| `namePrefix` | `gmc` | Prefix for all GMC resource names; also the SA identity the PSA-guard policy matches. Keep as `gmc` unless running two GMCs. |
| `replicaCount` | `2` | GMC controller-manager replicas (HA). |
| `gmc.image.repository` | `ghcr.io/actions-gateway/gmc` | GMC image repo. |
| `gmc.image.tag` | `""` | GMC tag (used only when digest is empty **and** `allowFloatingImageTags=true`). |
| `gmc.image.digest` | `""` | GMC image digest (`sha256:…`). **Required** — rendering fails when empty unless `allowFloatingImageTags=true`. |
| `gmc.imagePullPolicy` | `IfNotPresent` | GMC image pull policy. |
| `agc.image.{repository,tag,digest}` | `ghcr.io/actions-gateway/agc`, `""`, `""` | Image the GMC **injects** into provisioned AGCs. Digest required by default. |
| `proxy.image.{repository,tag,digest}` | `ghcr.io/actions-gateway/proxy`, `""`, `""` | Image the GMC **injects** into provisioned proxy pools. Digest required by default. |
| `allowFloatingImageTags` | `false` | Dev/test opt-out of digest pinning: lets the chart render `gmc.image` from a floating tag and disables the GMC's AGC/proxy pin check. **Do not enable in production.** |
| `leaderElection.enabled` | `true` | Pass `--leader-elect`. Keep on when `replicaCount > 1`. |
| `metrics.enabled` | `true` | Expose the HTTPS `:8443` metrics endpoint + Service. |
| `metrics.serviceMonitor.enabled` | `false` | Emit a Prometheus-Operator ServiceMonitor (needs its CRD). |
| `metrics.tls.certManager.enabled` | `true` | Issue a cert-manager metrics serving cert that the ServiceMonitor verifies (secure default). `false`/`certManager.enabled=false` falls back to the self-signed cert scraped with `insecureSkipVerify` (MITM trade-off). |
| `networkPolicy.enabled` | `true` | Ship the GMC ingress NetworkPolicies (needs an enforcing CNI). |
| `podDisruptionBudget.enabled` | `true` | Ship the `minAvailable: 1` PDB. |
| `admissionPolicy.enabled` | `true` | Ship the `namespace-psa-guard` and `tenant-resource-guard` VAPs + bindings (needs k8s ≥ 1.30). |
| `certManager.enabled` | `true` | Issue the webhook cert via cert-manager; `false` uses the self-signed fallback. |
| `certManager.selfSignedCertDurationDays` | `3650` | Validity of the self-signed cert when cert-manager is disabled. |
| `resources` | cpu 10m–500m / mem 64–128Mi | GMC container resources. |
| `priorityClassName` | `system-cluster-critical` | GMC PriorityClass (`""` to disable). |
| `nodeSelector` / `tolerations` / `affinity` | `{}` / `[]` / `{}` | GMC pod scheduling. |
| `topologySpreadConstraints.enabled` | `true` | Spread the GMC replicas across nodes (soft, `ScheduleAnyway`) so one node failure can't evict both. Set `false` to drop it. |
| `topologySpreadConstraints.{maxSkew,topologyKey,whenUnsatisfiable}` | `1` / `kubernetes.io/hostname` / `ScheduleAnyway` | Spread tuning; raise to `topology.kubernetes.io/zone` on multi-zone clusters. |
| `sampleGateway.create` | `false` | Render an example `ActionsGateway` (dev only). |
| `sampleGateway.securityProfile` | `baseline` | Profile for the sample CR (`baseline`/`restricted`/`privileged`). |
| `sampleGateway.gitHubAppSecretName` | `github-app-v1` | GitHub App Secret name referenced by the sample CR. |
| `sampleGateway.gitHubURL` | `https://github.com/my-org` | GitHub org/enterprise/repo URL the sample CR's runners register against. |

A `values.schema.json` validates these at install/lint time (image digest
format, security-profile enum, pull-policy enum, etc.).

## Offline validation

Rendering requires `gmc.image.digest` (see above); any well-formed digest works
for offline validation:

```sh
DIGEST=sha256:1111111111111111111111111111111111111111111111111111111111111111
helm lint charts/actions-gateway --set-string gmc.image.digest="$DIGEST"
helm template gag charts/actions-gateway --namespace gmc-system \
  --set-string gmc.image.digest="$DIGEST" | \
  kubeconform -strict -summary -kubernetes-version 1.30.0 \
    -skip CustomResourceDefinition,ActionsGateway,RunnerGroup,Certificate,Issuer,ServiceMonitor
```

The `-skip` list covers the CRDs and the CRs whose schemas (cert-manager,
Prometheus Operator) are not in kubeconform's default store.
