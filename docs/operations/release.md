# Release and Publishing

> **Audience:** Maintainer

How a maintainer cuts a release: tag → publish (build, push, sign, attest, and
package + push + sign the chart) → verify → record digests. This is the
**maintainer** runbook for
*producing* a release. Operators *consuming* a release pin the published digests
at install time — see [tenant-onboarding.md](tenant-onboarding.md) and the
[chart README](../../charts/actions-gateway/README.md).

## What a release produces

> Operators *installing* a release pin the published digests at install time —
> see [install.md § Pin images by digest](install.md#pin-images-by-digest).

A release is a `vX.Y.Z` git tag plus its outputs:

- The four first-party images — `gmc`, `agc`, `proxy`, `worker` — pushed to GHCR
  (`ghcr.io/actions-gateway/<name>`), each tagged `vX.Y.Z` and by long commit SHA.
  Each is **multi-arch** (`linux/amd64` + `linux/arm64`): the pushed artifact is
  an OCI image **index**, and the digest recorded everywhere (run summary,
  release notes, chart pins) is the index digest — the kubelet resolves the
  per-arch manifest from it at pull time, so one pinned digest schedules on both
  amd64 and arm64 (e.g. Graviton) nodes.
- A keyless **cosign signature** on every image (sigstore/Fulcio via GitHub
  Actions OIDC — no signing key, no stored secret), signed **recursively** — the
  index *and* each per-arch manifest — and an **SPDX-JSON SBOM per architecture**
  attached as a keyless cosign attestation to that architecture's manifest.
- The **Helm chart**, packaged and pushed as an OCI artifact to
  `oci://ghcr.io/actions-gateway/charts/actions-gateway`, with its `version` and
  `appVersion` set to the release tag and a keyless **cosign signature** from the
  same Fulcio/Rekor flow as the images. Operators install it straight from the
  registry (`helm install … oci://…`) with the published image **digests** pinned
  — no `git clone` of the chart. OCI (over a `gh-pages` chart repo) is chosen so
  the chart reuses the images' registry, login, and keyless-signing path; Artifact
  Hub (see [`Chart.yaml`](../../charts/actions-gateway/Chart.yaml) annotations)
  indexes the OCI ref for discoverability.

Both the image and chart work are automated by the
[`publish.yml`](../../.github/workflows/publish.yml) workflow, which triggers on
the `v*` tag push (the `chart-publish` job runs after every image leg succeeds).
The maintainer's job is to cut the tag and verify the result.

> **Signing and chart publish are exercised for the first time on the first real
> `v*` tag.** Pull-request CI builds each image and generates its SBOM, but it
> does **not** push, sign, attest, or publish the chart (those need a registry
> push and the publish workflow's OIDC identity). The verification step below is
> therefore not optional on the first release — it is the only thing that proves
> the signing and chart-publish paths work.

## One-time setup (first release only)

1. **GHCR package visibility.** The first publish *creates* the
   `ghcr.io/actions-gateway/{gmc,agc,proxy,worker}` image packages **and the
   `ghcr.io/actions-gateway/charts/actions-gateway` chart package**. They inherit
   the repository's visibility and may start **private**. For third parties to run
   `cosign verify` / `helm pull` (and for an air-gapped operator to pull), set each
   package to **public** in the org's GHCR package settings, or keep them private
   and distribute pull credentials. Verification by *this project's* CI and by
   anyone with pull access works either way.
2. **Workflow permissions** are already declared in `publish.yml`
   (`packages: write` to push, `id-token: write` for keyless cosign). No repo
   secret is required — that is the point of keyless signing.

## Release sequence

### 1. Pre-flight

- `main` is green: unit/integration/e2e and `security-scan.yml` all passing on
  the commit you are about to tag. Run `make check` locally as a final gate.
- Choose the version `vX.Y.Z` (semver). The tag **must** match `v*` or
  `publish.yml` will refuse to publish.

### 2. Tag and push

```bash
git switch main && git pull --ff-only
git tag -a vX.Y.Z -m "Release vX.Y.Z"
git push origin vX.Y.Z
```

Pushing the tag starts `publish.yml`. Watch it:

```bash
gh run watch "$(gh run list --workflow=publish.yml --branch=vX.Y.Z -L1 --json databaseId -q '.[0].databaseId')"
```

> A `workflow_dispatch` run with a `tag` input publishes the same way without a
> git tag — use it to dry-run the pipeline against a throwaway `vX.Y.Z-rc1` tag.

### 3. Verify the publish

Confirm every image **and the chart** was signed by *this* workflow before
announcing the release. The one-command check uses the pinned cosign
(`make` downloads `COSIGN_VERSION` — the same version `publish.yml` signs with —
into `.build/`):

```bash
make verify-release VERSION=vX.Y.Z
```

This verifies the four image signatures plus the chart (whose tag is `X.Y.Z`,
without the leading `v`) against the publish workflow's keyless identity. It
needs no credentials once the GHCR packages are public. The equivalent explicit
commands (and SBOM attestation retrieval) live in
[security-operations.md § Image provenance](security-operations.md#image-provenance-signature--sbom-verification);
each is a `cosign verify --certificate-identity-regexp '…/publish\.yml@refs/tags/v.*$' --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' <ref>`.

A `cosign verify` failure is a **stop-ship**: do not announce the release until it
passes. Spot-check one SBOM attestation too so the attestation path is exercised —
SBOM attestations are bound to the **per-arch manifest digests**, not the index,
so resolve one first (the full command set is in
[security-operations.md § Retrieve and inspect the SBOM](security-operations.md#retrieve-and-inspect-the-sbom)):

```bash
digest="$(docker buildx imagetools inspect ghcr.io/actions-gateway/gmc:vX.Y.Z --raw \
  | jq -r '.manifests[] | select(.platform.os == "linux" and .platform.architecture == "amd64") | .digest')"
cosign verify-attestation --type spdxjson \
  --certificate-identity-regexp '^https://github.com/actions-gateway/github-actions-gateway/\.github/workflows/publish\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  "ghcr.io/actions-gateway/gmc@${digest}" >/dev/null && echo OK
```

Also spot-check that the index actually carries both platforms
(`docker buildx imagetools inspect ghcr.io/actions-gateway/gmc:vX.Y.Z` should
list `linux/amd64` and `linux/arm64` manifests).

### 4. Record the published digests

`publish.yml` writes each image's immutable `ghcr.io/.../<name>@sha256:…` ref to
the **run summary** (the "Record published digest" step). These are the
**multi-arch index digests** — the single ref that serves both amd64 and arm64
nodes. Copy those four refs — operators pin the workload to the digest, not the
mutable `vX.Y.Z` tag. You can also resolve a digest directly:

```bash
docker buildx imagetools inspect ghcr.io/actions-gateway/gmc:vX.Y.Z \
  --format '{{json .Manifest.Digest}}'
```

### 5. Cut the GitHub Release

Create the GitHub Release for the tag. In the notes, include the four
`name@sha256:…` digests from step 4 and the `cosign verify` command from step 3,
so a consumer can verify provenance and pin digests without reading this runbook.

### 6. Chart version & metadata

The `chart-publish` job sets the published chart's `version` and `appVersion` to
the release tag (with the leading `v` stripped, since chart SemVer forbids it), so
there is **no manual Chart.yaml version bump** to remember — the in-repo
`version`/`appVersion` are dev placeholders the pipeline overrides at package time.
Two things still need a maintainer's eye, in a normal PR (not on the tag):

- **Prerelease annotation.** [`Chart.yaml`](../../charts/actions-gateway/Chart.yaml)
  carries `artifacthub.io/prerelease: "true"`. Keep it `true` while cutting `0.x`
  / `-rc` tags; set it to `false` for the first stable `v1.0.0+` release so Artifact
  Hub stops flagging the listing as a prerelease.
- **Empty `values.yaml` digests.** **Do not** commit real `sha256:…` digests into
  [`values.yaml`](../../charts/actions-gateway/values.yaml). The empty `digest`
  fields are the *secure default*: an unconfigured install fails closed (the GMC
  rejects floating AGC/proxy tags at startup) until the operator pins a real
  digest at install time. Baking a digest into the shipped chart would defeat
  that fail-closed posture and immediately go stale. The published digests belong
  in the **release notes** (step 5), which is where the operator copies them from.

### 7. Hand off to operators

Operators install/upgrade straight from the **published OCI chart** with the
digests pinned via `--set`, exactly as
[install.md § Pin images by digest](install.md#pin-images-by-digest) and
[upgrade.md](upgrade.md) document (`X.Y.Z` is the release tag without the `v`):

```bash
helm install gag oci://ghcr.io/actions-gateway/charts/actions-gateway --version X.Y.Z \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy>
```

## Rollback

A release is just a tag and a set of immutable, digest-addressed images — nothing
is destructive. To roll an *installed* release back, re-pin the previous digests
and `helm rollback`/`helm upgrade`; the procedure and post-rollback validation are
in [upgrade.md](upgrade.md). A bad tag can be superseded by a higher patch
release; do not retag an existing `vX.Y.Z` (it would break the digest↔tag binding
consumers rely on).

## The worker image and `DefaultWorkerImage`

`publish.yml` builds and signs `ghcr.io/actions-gateway/worker` (the wrapper that
feeds the job payload into `Runner.Worker`). Note that the AGC's
`DefaultWorkerImage`
([provisioner.go](../../cmd/agc/internal/provisioner/provisioner.go)) currently
defaults to the **upstream** `ghcr.io/actions/actions-runner` image, not this
signed first-party worker — so a default install does not run the signed worker
unless a tenant sets `RunnerGroup.Spec.WorkerImage` to it. Signing it is still
correct supply-chain hygiene; whether the default should point at the signed
first-party worker is a separate decision tracked on the backlog.

## PR CI vs publish — what runs where

| Stage | Build image | Generate SBOM | Push to GHCR | Sign + attest |
|---|---|---|---|---|
| Pull request (`security-scan.yml`) | ✅ | ✅ (artifact) | — | — |
| Release tag (`publish.yml`) | ✅ | ✅ (attached) | ✅ | ✅ keyless |

PR CI proves the image builds and the SBOM generates so those paths can't silently
break; signing and attestation are first exercised on a real `v*` tag, which is
why step 3 verification matters on every release.
