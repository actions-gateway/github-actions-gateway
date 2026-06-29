# GitOps install (Argo CD / Flux)

> **Audience:** Platform engineer running a GitOps-managed cluster

This page shows how to install the Gateway Manager Controller (GMC) **declaratively**
from a Git repository using either [Argo CD](https://argo-cd.readthedocs.io) or
[Flux](https://fluxcd.io), instead of running `helm install` by hand. It builds on
the imperative [install.md](install.md) reference — the prerequisites, the required
image-digest pinning, and the healthy-install verification all apply unchanged here.

Two things move into Git:

1. **The chart install** — the published, cosign-signed `actions-gateway` OCI Helm
   chart, rendered by your GitOps controller with the four image digests pinned.
2. **The GitHub App credential Secret** — sourced the GitOps way so the raw private
   key is **never** committed to Git. Use [External Secrets Operator](#sourcing-the-github-app-secret-external-secrets-operator)
   (pulls from your secret manager) or [Sealed Secrets](#sourcing-the-github-app-secret-sealed-secrets)
   (commits an encrypted blob only).

- [The CRD pruning gotcha](#the-crd-pruning-gotcha-read-first)
- [Argo CD](#argo-cd)
- [Flux](#flux)
- [Sourcing the GitHub App Secret: External Secrets Operator](#sourcing-the-github-app-secret-external-secrets-operator)
- [Sourcing the GitHub App Secret: Sealed Secrets](#sourcing-the-github-app-secret-sealed-secrets)

---

## The CRD pruning gotcha (read first)

The chart ships the two CustomResourceDefinitions (CRDs) —
`actionsgateways.actions-gateway.github.com` and
`runnergroups.actions-gateway.github.com` — as templates carrying the
`helm.sh/resource-policy: keep` annotation. That annotation tells Helm (and both
GitOps controllers below) **not** to delete the CRDs when the release is removed,
so uninstalling the GMC never cascade-deletes a tenant's `ActionsGateway` /
`RunnerGroup` objects. (The same applies to the opt-in `actions-gateway-crds-v2`
chart if you install it.)

A GitOps controller that *prunes* resources no longer present in its source can
defeat this if you are not careful. Two safe outcomes you must preserve:

- **Deleting the Application / HelmRelease must not delete the CRDs** (and with
  them, every tenant gateway).
- **The CRDs are large**, so server-side apply is the right strategy — a
  client-side apply can blow the 256 KiB last-applied annotation limit.

Both Argo CD and Flux **honour `helm.sh/resource-policy: keep`** out of the box, so
the chart's CRDs are protected without extra configuration. The examples below add
the controller-native guardrails anyway (`Prune=false`, `ServerSideApply=true`,
`upgrade.crds: CreateReplace`) so the protection is explicit and survives a future
chart refactor.

---

## Argo CD

The chart is an OCI artifact, so the Argo CD `Application` points its Helm source
at the GHCR registry path. Copy the four image digests from the
[release notes](https://github.com/actions-gateway/github-actions-gateway/releases/tag/v1.0.0)
— the chart ships **no** baked-in digests (an unconfigured render is rejected,
fail-closed).

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: actions-gateway
  namespace: argocd
  annotations:
    # Belt-and-braces: never prune this Application's CRDs even if a future
    # chart drops the helm.sh/resource-policy:keep annotation. Argo CD already
    # honours that annotation, so this is defence in depth, not a requirement.
    argocd.argoproj.io/sync-options: ServerSideApply=true
spec:
  project: default
  source:
    repoURL: ghcr.io/actions-gateway/charts   # OCI registry path, NOT the chart
    chart: actions-gateway
    targetRevision: 1.0.0                      # chart version = release tag minus the leading "v"
    helm:
      parameters:
        - name: gmc.image.digest
          value: sha256:<gmc>
        - name: agc.image.digest
          value: sha256:<agc>
        - name: proxy.image.digest
          value: sha256:<proxy>
        - name: wrapper.image.digest
          value: sha256:<wrapper>
  destination:
    server: https://kubernetes.default.svc
    namespace: gmc-system
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
      - ServerSideApply=true   # the CRDs are large; client-side apply can exceed the annotation limit
```

Notes:

- **`repoURL` is the registry path, `chart` is the chart name** — for an OCI Helm
  source Argo CD splits them. On Argo CD older than v2.7 you must first register
  the repository with `enableOCI: true`; v2.7+ resolves `oci://`-style sources
  automatically.
- **Keep `automated.prune: true`.** Pruning is what makes Git the source of truth.
  The CRDs survive it because the chart already stamps them with
  `helm.sh/resource-policy: keep`, which Argo CD maps to `Prune=false` — so no
  Application-level override is needed. If you ever want to pin `Prune=false`
  regardless of the chart, set `argocd.argoproj.io/sync-options: Prune=false` on
  the CRD objects (e.g. via the chart's `commonAnnotations`), **not** on the whole
  Application — Application-wide `Prune=false` would strand every orphaned resource,
  not just the CRDs.
- Verify the chart and image cosign signatures before first sync — see
  [release.md § Verify the publish](release.md#3-verify-the-publish).

For the GitHub App credential Secret, do **not** put it in this Application. Source
it with [External Secrets Operator](#sourcing-the-github-app-secret-external-secrets-operator)
or [Sealed Secrets](#sourcing-the-github-app-secret-sealed-secrets) in the tenant
namespace.

---

## Flux

Flux installs the OCI chart with a `HelmRelease` backed by either an
`OCIRepository` (recommended for OCI charts) or a `HelmRepository` of `type: oci`.
Both forms are shown.

### OCIRepository + HelmRelease (recommended)

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: actions-gateway
  namespace: gmc-system
spec:
  interval: 30m
  url: oci://ghcr.io/actions-gateway/charts/actions-gateway
  ref:
    tag: "1.0.0"   # chart version = release tag minus the leading "v"
  # Verify the chart's keyless cosign signature before pulling (Flux >= 2.0):
  verify:
    provider: cosign
    matchOIDCIdentity:
      - issuer: https://token.actions.githubusercontent.com
        subject: https://github.com/actions-gateway/github-actions-gateway/.github/workflows/publish.yml@refs/tags/v.*
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: actions-gateway
  namespace: gmc-system
spec:
  interval: 30m
  chartRef:
    kind: OCIRepository
    name: actions-gateway
  install:
    createNamespace: true
    crds: Create          # install CRDs on first apply
  upgrade:
    crds: CreateReplace   # apply CRD schema changes on upgrade; never deletes CRDs
  # Flux/Helm never delete CRDs on uninstall, and the chart's
  # helm.sh/resource-policy:keep annotation keeps every tenant CR intact too.
  values:
    gmc:
      image:
        digest: sha256:<gmc>
    agc:
      image:
        digest: sha256:<agc>
    proxy:
      image:
        digest: sha256:<proxy>
    wrapper:
      image:
        digest: sha256:<wrapper>
```

### HelmRepository (type: oci) alternative

If you prefer the classic `HelmRepository` source, swap the `OCIRepository` for:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata:
  name: actions-gateway
  namespace: gmc-system
spec:
  type: oci
  interval: 30m
  url: oci://ghcr.io/actions-gateway/charts
```

…and reference it from the `HelmRelease` with a `spec.chart` block instead of
`chartRef`:

```yaml
  chart:
    spec:
      chart: actions-gateway
      version: "1.0.0"
      sourceRef:
        kind: HelmRepository
        name: actions-gateway
```

Notes:

- **`upgrade.crds: CreateReplace`** is the safe setting: it applies CRD schema
  changes on `helm upgrade` but **never deletes** a CRD (Helm has no delete-CRD
  path). Do **not** use `Skip` if a release ever changes the CRD schema, or the
  new fields will silently not apply. Deleting the `HelmRelease` leaves the CRDs
  and tenant CRs in place — Helm does not garbage-collect CRDs, and the
  `helm.sh/resource-policy: keep` annotation reinforces it.
- For air-gapped relocation of the chart and images, see
  [air-gapped-install.md](air-gapped-install.md); the `OCIRepository` `url` and the
  `*.image.digest` values point at your private registry instead of GHCR.

For the GitHub App credential Secret, use one of the two patterns below.

---

## Sourcing the GitHub App Secret: External Secrets Operator

[External Secrets Operator (ESO)](https://external-secrets.io) projects a Secret
from your external secret manager (HashiCorp Vault, AWS Secrets Manager, GCP Secret
Manager, Azure Key Vault, …) into the cluster. The raw private key lives in the
secret manager; only the *reference* is committed to Git.

The GMC consumes a per-tenant `Secret` with three keys — `appId`, `installationId`,
and `privateKey` — referenced by `spec.gitHubAppRef` on the `ActionsGateway` CR (see
[Getting Started §3](../getting-started.md#3-create-a-github-app-credential-secret)).
The `ExternalSecret` below reconstructs exactly those keys. Replace the
`ClusterSecretStore` name and `remoteRef` keys with your provider's; this example
uses a Vault-backed store.

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: my-github-app
  namespace: team-a            # the TENANT namespace, not gmc-system
spec:
  refreshInterval: 1h
  secretStoreRef:
    kind: ClusterSecretStore
    name: vault-backend        # your provisioned store
  target:
    name: my-github-app        # the Secret name spec.gitHubAppRef points at
    creationPolicy: Owner
    template:
      type: Opaque
  data:
    - secretKey: appId
      remoteRef:
        key: secret/data/team-a/github-app
        property: appId
    - secretKey: installationId
      remoteRef:
        key: secret/data/team-a/github-app
        property: installationId
    - secretKey: privateKey
      remoteRef:
        key: secret/data/team-a/github-app
        property: privateKey   # the full PEM, stored only in Vault
```

ESO recreates the Secret if it is deleted and re-projects it when the upstream key
rotates. After rotating the key in your secret manager, follow the same Secret-swap
flow as [Getting Started — Rotating GitHub App Credentials](../getting-started.md#rotating-github-app-credentials)
(point the CR at the new Secret name) so the rollout is controlled rather than
in-place.

---

## Sourcing the GitHub App Secret: Sealed Secrets

[Sealed Secrets](https://github.com/bitnami-labs/sealed-secrets) lets you commit an
**encrypted** `SealedSecret` to Git that only the in-cluster controller can decrypt.
The plaintext private key never enters the repository.

Seal the Secret from a local PEM file — the `.pem` stays on your workstation and is
**never** committed:

```sh
# app.private-key.pem is the key you downloaded from the GitHub App settings.
# Build the plaintext Secret only as an in-memory pipe, then seal it.
kubectl create secret generic my-github-app \
  --namespace team-a \
  --type Opaque \
  --from-literal=appId=123456 \
  --from-literal=installationId=78901234 \
  --from-file=privateKey=app.private-key.pem \
  --dry-run=client -o yaml \
| kubeseal --format yaml > my-github-app.sealedsecret.yaml
```

The resulting `my-github-app.sealedsecret.yaml` is safe to commit and reconcile via
Argo CD or Flux:

```yaml
apiVersion: bitnami.com/v1alpha1
kind: SealedSecret
metadata:
  name: my-github-app
  namespace: team-a
spec:
  encryptedData:
    appId: AgB...            # ciphertext — only the cluster controller can decrypt
    installationId: AgC...
    privateKey: AgD...
  template:
    metadata:
      name: my-github-app
      namespace: team-a
    type: Opaque
```

The controller decrypts it into a normal `Secret` named `my-github-app` with the
`appId` / `installationId` / `privateKey` keys the GMC expects. By default a
`SealedSecret` is sealed to its exact name **and** namespace, so it can only be
unsealed in `team-a` — keep the namespace consistent with the `ActionsGateway` CR.
After deleting the local `.pem`, the only copy of the private key outside GitHub is
the one the controller holds in-cluster.

---

## Next steps

- [install.md](install.md) — the imperative install reference these examples build
  on (prerequisites, digest pinning, healthy-install verification).
- [air-gapped-install.md](air-gapped-install.md) — relocate the chart and images to
  a private registry; the `OCIRepository` / `repoURL` and digests point there.
- [Getting Started](../getting-started.md) — create the GitHub App Secret and the
  first `ActionsGateway` CR; the credential-rotation flow.
- [backup-restore.md](backup-restore.md) — why GitOps *is* the primary backup
  posture for the gateway's desired state.

---

← [Back to Operations](.)
