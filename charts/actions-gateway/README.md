# actions-gateway Helm chart

Installs the **Gateway Manager Controller (GMC)** — the operator that provisions isolated per-tenant gateways from `ActionsGateway` custom resources.
This chart deploys the GMC and its cluster prerequisites only.
Per-tenant Actions Gateway Controller (AGC) instances and egress proxy pools are **provisioned by the GMC at runtime** from each `ActionsGateway` CR; they are not chart resources.

> This chart is the **sole** install path (Q142) — there is no kustomize overlay.
> The plain-YAML files under `cmd/gmc/config/` and `cmd/agc/config/` are the controller-gen codegen + envtest substrate (they back `make manifests` and envtest) and the single-source inputs to this chart's CRD/RBAC generators; they are not an install vehicle.
> Helm was chosen over a Kustomize overlay for versioned releases and a real day-2 `helm upgrade`/`rollback` lifecycle (decision D-M5-1).

## What it installs

- The two tenant CRDs — `ActionsGateway` and `RunnerGroup` — under `templates/crds/` with `helm.sh/resource-policy: keep` so `helm upgrade` carries CRD field changes (Helm never upgrades the chart-root `crds/` dir) and `helm uninstall` preserves tenant objects.
- The `PriorityClassAllowlist` CRD, uniquely, under the chart-root **`crds/`** dir: the chart also renders a `PriorityClassAllowlist` CR (the `priorityclass-allowlist-guard` policy's param).
  Helm resolves REST mappings for the entire manifest before applying any of it, so a CR whose CRD is a template in the same release fails the install with `no matches for kind`.
  Only `crds/` is installed ahead of that resolution.

  The cost lands on **upgrades**: Helm skips `crds/` there entirely.
  So applying the chart's CRDs is a standard pre-upgrade step for this chart, run every time rather than conditionally:

  ```sh
  helm show crds <chart> --version <version> | kubectl apply -f -
  ```

  It is idempotent, and reads the CRDs from the exact chart version you are upgrading to — so it covers any future schema change without anyone having to remember a release note.
  The chart preflights the CRD's presence and fails with that command if it is skipped.
  Fresh installs are unaffected; Helm applies `crds/` for you.

  **All CRD files here are generated** from the authoritative controller-gen sources (`cmd/*/config/crd`, `api/config/crd`) by `make chart-crds` — do not hand-edit them; a CI drift gate (`make chart-crds-check`) fails if they fall out of sync.
  See [code-generation.md](../../docs/development/code-generation.md).
- The GMC `Deployment` (HA: 2 replicas + leader election + PDB), `ServiceAccount`, cluster/namespaced RBAC, and the `agc-tenant-role` ClusterRole.
  The manager ClusterRole's **rules are generated** from the controller-gen output of the GMC's `+kubebuilder:rbac` markers (`cmd/gmc/config/rbac/role.yaml`) into `files/manager-role-rules.yaml` by `make chart-rbac`; a CI drift gate (`make chart-rbac-check`) fails if they fall out of sync.
  Do not hand-edit them.
- The validating webhook (`ValidatingWebhookConfiguration` + Service) and its serving cert (cert-manager or self-signed — see below).
- Three ValidatingAdmissionPolicies: `namespace-psa-guard` (the namespace PSA-label patch) and `tenant-resource-guard` (create/update/delete of Deployments, Secrets, RoleBindings, NetworkPolicies, etc.) confine the GMC's cluster-wide write grants to marked tenant namespaces; `priorityclass-allowlist-guard` backstops the PriorityClass allowlist on direct `runnergroups` writes that bypass the GMC webhooks.
  Its parameter is the cluster-scoped `PriorityClassAllowlist` CR rendered from `allowedPriorityClasses`, which the GMC also watches — a CRD rather than a ConfigMap because a core-type `paramKind` is destroyed by a kube-apiserver defect on `helm uninstall` (Q444/Q492).
- NetworkPolicies (default-deny ingress + metrics/webhook allows) and the metrics Service / optional ServiceMonitor.

## Prerequisites

- Kubernetes **>= 1.30** (GA `ValidatingAdmissionPolicy`).
- A CNI that enforces `NetworkPolicy` (Calico/Cilium) for the egress/ingress controls to take effect. `kindnet` does not enforce egress.
- **cert-manager** *if* `certManager.enabled=true` (the default).
  Not required when you set `certManager.enabled=false`.
- **Image digests** for the GMC, AGC, proxy, and worker-wrapper images (see below).

## Install

```sh
helm install gag charts/actions-gateway \
  --namespace gmc-system --create-namespace \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy> \
  --set wrapper.image.digest=sha256:<wrapper>
```

Digest pinning is enforced for all four images at **render time** — this is the secure default.
The chart **fails to render** with `<image>.image must be pinned by digest …` (naming `gmc`, `agc`, `proxy`, or `wrapper`) when any of the four digests is empty, so no image can silently fall back to a mutable `:latest` tag.
Failing at render catches all four up front rather than letting an unpinned `AGC_IMAGE`/`PROXY_IMAGE`/`WRAPPER_IMAGE` surface later as a GMC crash-loop (the GMC does *also* re-check those three at startup as a second layer).
The worker-wrapper (Q235) is on by default — the chart always sets `WRAPPER_IMAGE`, so `wrapper.image.digest` is required like the rest.

Pin `gmc.image.digest`, `agc.image.digest`, `proxy.image.digest`, and `wrapper.image.digest` before installing, or pass `--set allowFloatingImageTags=true` — the one explicit opt-out covering both layers — for **dev/test only**.

### Without cert-manager

```sh
helm install gag charts/actions-gateway \
  --namespace gmc-system --create-namespace \
  --set certManager.enabled=false \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy> \
  --set wrapper.image.digest=sha256:<wrapper>
```

The chart generates a self-signed webhook serving cert and wires the webhook `caBundle` itself. **Trade-off:** the cert rotates on a `helm upgrade` that cannot reuse the existing Secret — see [upgrade](../../docs/operations/upgrade.md).

## Upgrade

```sh
helm upgrade gag charts/actions-gateway --namespace gmc-system --reset-then-reuse-values
```

CRDs ship as templates, so field changes are applied on upgrade — except the `PriorityClassAllowlist` CRD, which ships in `crds/` and needs `helm show crds <chart> | kubectl apply -f -` first (see *What it installs*).
Setting the removed `priorityClassAllowlist.configMapName` now fails the render with migration instructions.
The `namespace-psa-guard` and `tenant-resource-guard` bindings deny by default; if you are upgrading a cluster whose tenant namespaces are not yet labeled `actions-gateway.github.com/tenant=true`, label them first (or temporarily set both bindings to `Audit`) — see [upgrade](../../docs/operations/upgrade.md).

## Values

| Key | Default | Description |
|---|---|---|
| `namePrefix` | `gmc` | Prefix for all GMC resource names; also the SA identity the PSA-guard policy matches. Keep as `gmc` unless running two GMCs. |
| `replicaCount` | `2` | GMC controller-manager replicas (HA). |
| `imagePullSecrets` | `[]` | Image-pull Secret references (`[{name: …}]`) for the GMC pod, to pull from a private/mirrored registry. The AGC/proxy/worker images are pulled by runtime-provisioned pods — attach their pull Secret to the tenant ServiceAccount instead (see [air-gapped-install.md](../../docs/operations/air-gapped-install.md)). |
| `gmc.image.repository` | `ghcr.io/actions-gateway/gmc` | GMC image repo. Override (with `agc`/`proxy`) to relocate to a private mirror; keep the digest. |
| `gmc.image.tag` | `""` | GMC tag (used only when digest is empty **and** `allowFloatingImageTags=true`). |
| `gmc.image.digest` | `""` | GMC image digest (`sha256:…`). **Required** — rendering fails when empty unless `allowFloatingImageTags=true`. |
| `gmc.imagePullPolicy` | `IfNotPresent` | GMC image pull policy. |
| `agc.image.{repository,tag,digest}` | `ghcr.io/actions-gateway/agc`, `""`, `""` | Image the GMC **injects** into provisioned AGCs. **Required** — rendering fails when the digest is empty (unless `allowFloatingImageTags=true`). |
| `proxy.image.{repository,tag,digest}` | `ghcr.io/actions-gateway/proxy`, `""`, `""` | Image the GMC **injects** into provisioned proxy pools. **Required** — rendering fails when the digest is empty (unless `allowFloatingImageTags=true`). |
| `wrapper.image.{repository,tag,digest}` | `ghcr.io/actions-gateway/wrapper`, `""`, `""` | Worker-wrapper image (Q235) the GMC forwards to every AGC, which injects it into each worker pod. On by default — the chart always sets `WRAPPER_IMAGE`. **Required** — rendering fails when the digest is empty (unless `allowFloatingImageTags=true`). |
| `allowFloatingImageTags` | `false` | Dev/test opt-out of digest pinning: lets the chart render all four images (`gmc`/`agc`/`proxy`/`wrapper`) from floating tags and disables the GMC's AGC/proxy/wrapper startup pin check. **Do not enable in production.** |
| `leaderElection.enabled` | `true` | Pass `--leader-elect`. Keep on when `replicaCount > 1`. |
| `metrics.enabled` | `true` | Expose the HTTPS `:8443` metrics endpoint + Service. |
| `metrics.serviceMonitor.enabled` | `false` | Emit a Prometheus-Operator ServiceMonitor (needs its CRD). |
| `metrics.tls.certManager.enabled` | `true` | Issue a cert-manager metrics serving cert that the ServiceMonitor verifies (secure default). `false`/`certManager.enabled=false` falls back to the self-signed cert scraped with `insecureSkipVerify` (MITM trade-off). |
| `networkPolicy.enabled` | `true` | Ship the GMC ingress NetworkPolicies (needs an enforcing CNI). |
| `podDisruptionBudget.enabled` | `true` | Ship the `minAvailable: 1` PDB. |
| `admissionPolicy.enabled` | `true` | Ship the `namespace-psa-guard`, `tenant-resource-guard`, and `priorityclass-allowlist-guard` VAPs + bindings (needs k8s ≥ 1.30). |
| `certManager.enabled` | `true` | Issue the webhook cert via cert-manager; `false` uses the self-signed fallback. |
| `certManager.selfSignedCertDurationDays` | `3650` | Validity of the self-signed cert when cert-manager is disabled. |
| `resources` | cpu 10m–500m / mem 64–128Mi | GMC container resources. |
| `vpa.enabled` | `false` | Emit a `VerticalPodAutoscaler` that right-sizes the GMC (needs the `autoscaling.k8s.io` CRDs, i.e. the Kubernetes vertical-pod-autoscaler installed). |
| `vpa.updateMode` | `"Off"` | `Off` = recommendation only; `Initial` applies at pod creation; `Recreate` lets the autoscaler evict a GMC pod to resize it. |
| `vpa.minAllowed` / `vpa.maxAllowed` | `{}` / `{}` | Autoscaler floor/ceiling for the GMC's **requests** (it is pinned to `RequestsOnly`, so `resources.limits` is never moved). `maxAllowed` defaults to `resources.limits`. |
| `priorityClassName` | `system-cluster-critical` | GMC PriorityClass (`""` to disable). |
| `systemCriticalPriorityQuota.enabled` | `true` | Ship a scoped `ResourceQuota` that **permits** the `system-*-critical` `priorityClassName` under GKE's restricted PriorityClass admission (without it GKE rejects the GMC pods with `insufficient quota to match these scopes`). Permit-only (generous ceiling, scoped to the system-critical classes), inert elsewhere, and rendered only while `priorityClassName` is a system-critical class. Set `false` to manage it out-of-band. |
| `systemCriticalPriorityQuota.maxPods` | `100` | Pod ceiling for that scoped quota; generous so it only satisfies admission, never caps scheduling. |
| `nodeSelector` / `tolerations` / `affinity` | `{}` / `[]` / `{}` | GMC pod scheduling. |
| `topologySpreadConstraints.enabled` | `true` | Spread the GMC replicas across nodes (soft, `ScheduleAnyway`) so one node failure can't evict both. Set `false` to drop it. |
| `topologySpreadConstraints.{maxSkew,topologyKey,whenUnsatisfiable}` | `1` / `kubernetes.io/hostname` / `ScheduleAnyway` | Spread tuning; raise to `topology.kubernetes.io/zone` on multi-zone clusters. |
| `sampleGateway.create` | `false` | Render an example `ActionsGateway` (dev only). |
| `sampleGateway.securityProfile` | `baseline` | Profile for the sample CR (`baseline`/`restricted`/`privileged`). |
| `sampleGateway.gitHubAppSecretName` | `github-app-v1` | GitHub App Secret name referenced by the sample CR. |
| `sampleGateway.gitHubURL` | `https://github.com/my-org` | GitHub org/enterprise/repo URL the sample CR's runners register against. |

A `values.schema.json` validates these at install/lint time (image digest format, security-profile enum, pull-policy enum, etc.).

## Offline validation

Rendering requires all four image digests (`gmc`/`agc`/`proxy`/`wrapper`, see above); any well-formed digest works for offline validation:

```sh
DIGEST=sha256:1111111111111111111111111111111111111111111111111111111111111111
PINS="--set-string gmc.image.digest=$DIGEST --set-string agc.image.digest=$DIGEST"
PINS="$PINS --set-string proxy.image.digest=$DIGEST --set-string wrapper.image.digest=$DIGEST"
helm lint charts/actions-gateway $PINS
helm template gag charts/actions-gateway --namespace gmc-system $PINS | \
  kubeconform -strict -summary -kubernetes-version 1.30.0 \
    -skip CustomResourceDefinition,ActionsGateway,RunnerGroup,Certificate,Issuer,ServiceMonitor
```

The `-skip` list covers the CRDs and the CRs whose schemas (cert-manager, Prometheus Operator) are not in kubeconform's default store.
