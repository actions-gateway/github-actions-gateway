# Q12 — Production Helm chart (`charts/actions-gateway/`)

← [Milestone 5 §1](milestone-5.md#1-packaging-helm-chart) | [Release 1.0 §C](release-1.0.md#c-packaging--supply-chain--gating--recommended) | [STATUS](../STATUS.md)

**Goal.** Ship a Helm chart that an operator can `helm install` to deploy
the Gateway Manager Controller (GMC) — its CRDs, RBAC, validating webhook,
and admission policy — with every default at the secure posture already
encoded in the `cmd/gmc/config/` kustomize bases. Helm was decided over
Kustomize ([D-M5-1](milestone-5.md#11-install-vehicle--decided-helm-chart)).

**Scope of this PR (slice 1 — the whole GMC core install).** A
lint-clean, offline-validated chart that renders the complete GMC control
plane. Not a second distribution path — the `cmd/*/config/` kustomize
bases stay the dev/CI source of truth (they back `make manifests` and the
envtest/e2e tiers).

## What the chart installs

The GMC is the only thing installed. **AGC instances and proxy pools are
provisioned by the GMC at runtime** from each `ActionsGateway` CR — they
are *not* chart resources. Verified against
[builder.go](../../cmd/gmc/internal/controller/builder.go)
(`buildAGCDeployment`/`buildProxyDeployment`) and
[main.go](../../cmd/gmc/cmd/main.go) (`AGCImage`/`ProxyImage` injected into
the reconciler), and the `docs/operations/tenant-onboarding.md` flow.

Resources, sourced 1:1 from the kustomize bases:

| Template | Source base |
|---|---|
| `crds/actionsgateway-crd.yaml` | `cmd/gmc/config/crd/bases/…_actionsgateways.yaml` |
| `crds/runnergroup-crd.yaml` | **`cmd/agc/config/crd/…_runnergroups.yaml`** (AGC authoritative copy — see drift note) |
| `serviceaccount.yaml` | `rbac/service_account.yaml` |
| `rbac.yaml` | `rbac/role.yaml` + `role_binding.yaml` + `metrics_auth_*` + `metrics_reader_role.yaml` + `leader_election_*` |
| `agc-tenant-role.yaml` | `agc-tenant-role/agc_tenant_role.yaml` (unprefixed name kept stable) |
| `deployment.yaml` | `manager/manager.yaml` + metrics/webhook patches |
| `pdb.yaml` | `manager/pdb.yaml` |
| `metrics-service.yaml` | `default/metrics_service.yaml` |
| `webhook-service.yaml` | `webhook/service.yaml` |
| `webhook.yaml` | `webhook/manifests.yaml` (ValidatingWebhookConfiguration) |
| `certmanager.yaml` | `certmanager/issuer.yaml` + `certificate-webhook.yaml` (gated) |
| `networkpolicy.yaml` | `network-policy/allow-metrics-traffic.yaml` + `allow-webhook-traffic.yaml` |
| `namespace-psa-guard.yaml` | `admission-policy/namespace-psa-guard.yaml` (VAP + binding) |
| `servicemonitor.yaml` | `prometheus/monitor.yaml` (gated, opt-in) |
| `sample-gateway.yaml` | `samples/…` (gated, off by default) |

## Two Helm-specific gotchas (both handled)

1. **cert-manager is optional.** `certManager.enabled=true` (default,
   secure) emits the `Issuer`+`Certificate` and the
   `cert-manager.io/inject-ca-from` annotation on the webhook. When
   `false`, the chart generates a self-signed serving cert at render time
   with Helm's `genCA`/`genSignedCert` and writes both the
   `webhook-server-cert` Secret and the webhook `caBundle` directly — so
   the webhook installs with no cert-manager present. The fallback is
   pure-template (no Job/hook), which renders offline and is simpler than a
   post-install hook; the trade-off is that the cert rotates on a
   `helm upgrade` that does not reuse the existing Secret — noted in
   [upgrade.md](../operations/upgrade.md).
2. **Helm never upgrades `crds/`.** Both CRDs ship under
   `templates/crds/` with `helm.sh/resource-policy: keep` (not the chart
   root `crds/` dir), so day-2 `helm upgrade` carries CRD field changes
   while `helm uninstall` still preserves them. Recorded in
   [upgrade.md](../operations/upgrade.md).

## Secure-by-default values

Every templated default matches the kustomize posture; no security
property is traded for convenience:

- Hardened pod/container `securityContext` (runAsNonRoot, RO root FS,
  drop ALL caps, seccomp RuntimeDefault), resource limits, PDB,
  `system-cluster-critical` PriorityClass, startup/liveness/readiness
  probes — all carried verbatim, not re-defaulted.
- NetworkPolicies **on by default** (selecting the manager pod flips it to
  default-deny ingress); metrics restricted to `metrics: enabled`
  namespaces.
- `namespace-psa-guard` VAP binding ships `validationActions: [Deny]`.
- Leader election on, 2 replicas, HA defaults.
- **Image digest pinning is the secure default.** The GMC rejects floating
  `AGC_IMAGE`/`PROXY_IMAGE` tags unless `--allow-floating-image-tags` is
  set ([main.go](../../cmd/gmc/cmd/main.go) `validateImageDigest`). The
  chart does **not** pass that flag by default and leaves digests empty, so
  an unconfigured install fails closed at GMC startup until the operator
  pins `agc.image.digest`/`proxy.image.digest`. `values.yaml` documents
  this prominently. `allowFloatingImageTags=true` is the documented dev-only
  opt-out.

## Operator-facing values (`values.yaml` + README table)

`gmc.image.*`, `agc.image.*`, `proxy.image.*` (repository/tag/digest),
`leaderElection.enabled`, `metrics.enabled`, `metrics.serviceMonitor.enabled`,
`securityProfile` (sample default), `certManager.enabled`,
`allowFloatingImageTags`, `sampleGateway.create`, `networkPolicy.enabled`,
`podDisruptionBudget.enabled`, `replicaCount`, `resources`. Each has a
documented default and a `values.schema.json` entry where it constrains a
shape an operator can get wrong.

## RunnerGroup CRD drift (Q73 — note only, not fixed here)

The GMC's `crd/bases/…_runnergroups.yaml` (8041 lines) is **stale** vs the
AGC authoritative `cmd/agc/config/crd/…_runnergroups.yaml` (8738 lines).
The chart sources the **AGC** copy per the task. Reconciling the two bases
(so `make manifests` keeps them in sync) is **[Q73](../STATUS.md)** and is
out of scope here — this PR only consumes the authoritative copy.

## Offline validation (no cluster)

1. `helm lint charts/actions-gateway`
2. `helm template charts/actions-gateway` — renders cleanly, both cert
   modes (`--set certManager.enabled=false`), sample gateway on/off.
3. Pipe rendered output through `kubeconform` (skip the two CRDs + the
   cert-manager / monitoring.coreos.com CRs whose schemas are not in the
   default store; `-skip` them with a note).

## Out of scope (documented future slices)

- CI drift check that re-renders the chart and diffs against the kustomize
  bases (M5 §7 risk row) — folds into [Q66](../STATUS.md) install-artifact
  validation.
- `polaris` posture scan against the rendered output — **[Q14](../STATUS.md)**.
- Publishing the chart as an OCI artifact with digest-pinned images and
  cosign signatures — **[Q28](../STATUS.md)**.
- Reconciling the RunnerGroup CRD drift at the kustomize-base layer —
  **[Q73](../STATUS.md)**.
</content>
</invoke>
