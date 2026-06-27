# Air-gapped / private-registry install (Q187)

> Status: ✅ Done — chart image-pull-secret support + air-gapped install guide
> shipped. Q187 removed from the Queue.

## Goal

Make GAG installable on egress-restricted / air-gapped clusters that cannot pull
from GHCR, by letting an operator relocate every image to a private mirror and
authenticate the pulls — without weakening the secure-by-default posture
(digest pinning stays mandatory).

## Background — what must be relocated

GAG references **four** first-/third-party images, only one of which the chart
runs directly:

| Image | Default ref | Pulled by | Override point |
|---|---|---|---|
| GMC | `ghcr.io/actions-gateway/gmc` | the chart's GMC Deployment | `gmc.image.repository` + `gmc.image.digest` |
| AGC | `ghcr.io/actions-gateway/agc` | per-tenant AGC Deployments (GMC-provisioned at runtime) | `agc.image.repository` + `agc.image.digest` (GMC injects `AGC_IMAGE`) |
| Proxy | `ghcr.io/actions-gateway/proxy` | per-tenant egress-proxy pools (GMC-provisioned) | `proxy.image.repository` + `proxy.image.digest` (GMC injects `PROXY_IMAGE`) |
| Worker | `ghcr.io/actions/actions-runner` (digest-pinned default; first-party `ghcr.io/actions-gateway/worker` is the alternative) | worker pods (AGC-provisioned) | `RunnerGroup.spec.workerImage` / AGC `WORKER_IMAGE` env — **not** a chart value |

Per-image `repository` overrides already exist in `values.yaml` (and
`values.schema.json` requires `repository`); the registry is therefore *already*
relocatable per image. The gaps Q187 closes:

1. No way to attach an image-pull Secret for a private registry that requires
   authentication.
2. No documented procedure for mirroring the images (with digests) + the OCI
   Helm chart, wiring the overrides, and verifying.

## Why chart + docs (no Go change)

AGC, proxy, and worker pods are **provisioned at runtime**, not by the chart, so
the chart cannot stamp `imagePullSecrets` onto them. Kubernetes' canonical
air-gapped mechanism covers them with **zero code**: an `imagePullSecrets`
reference on the *ServiceAccount* is auto-injected into every pod that uses it.
The relevant SAs all live in the tenant namespace:

- AGC pods → SA `<gateway>` (the per-gateway AGC SA the GMC creates).
- Worker pods → SA `<gateway>-worker` (the worker SA the GMC creates).
- Proxy pods → the namespace `default` SA (the proxy pod spec sets no SA).

The GMC reconciles the AGC/worker SAs with `controllerutil.CreateOrPatch`, whose
mutate closure only sets `Labels` + the owner reference
(`applyServiceAccount` in `actionsgateway_v2_controller.go`) — it never touches
`imagePullSecrets`, so an operator-added pull-secret reference **survives
reconciliation**. The `default` SA is never touched by the GMC at all.

So the chart only needs to wire `imagePullSecrets` for its own workload (the GMC
Deployment); the runtime workloads are covered by the documented SA-attach
pattern.

## Scope

### Chart
- Add top-level `imagePullSecrets: []` value (list of `{name: <secret>}`),
  applied to the GMC Deployment pod spec. Empty default (no behavior change).
- Add it to `values.schema.json` and the chart README values table.
- Keep digest pinning intact — `imagePullSecrets` is orthogonal to the
  digest-required `gmcImage`/`image` helpers; no change there.
- Per-image registry overrides: already present; clarify the `values.yaml`
  comment that `repository` is the private-mirror relocation knob.

### Docs
- New `docs/operations/air-gapped-install.md` (Platform-engineer audience):
  1. Mirror/relocate the four images **preserving digests** (`cosign copy` /
     `crane copy` / `skopeo`).
  2. Relocate the OCI Helm chart(s) (`helm pull` / `oras cp`).
  3. Create the `dockerconfigjson` pull Secret **securely** — from a file
     (`kubectl create secret docker-registry … --from-file`), never registry
     creds committed to `values.yaml`. Created in `gmc-system` + each tenant ns.
  4. `helm install` with per-image `repository`/`digest` overrides +
     `imagePullSecrets`.
  5. SA-attach the pull Secret for AGC / worker / proxy in each tenant ns.
  6. Set the worker image override to the mirrored ref.
  7. Verify.
- Cross-link from `install.md` and `docs/operations/README.md`.

## Acceptance
- Chart supports private-registry overrides + pull secrets for all images;
  digest pinning preserved. `helm lint`/`template` green; `make check` green.
- Air-gapped doc with the secure pull-secret pattern.
