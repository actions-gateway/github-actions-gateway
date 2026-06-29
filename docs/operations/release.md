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

- The five first-party images — `gmc`, `agc`, `proxy`, `worker`, `wrapper` — pushed
  to GHCR (`ghcr.io/actions-gateway/<name>`), each tagged `vX.Y.Z` and by long commit
  SHA.
  Each is **multi-arch** (`linux/amd64` + `linux/arm64`): the pushed artifact is
  an OCI image **index**, and the digest recorded everywhere (run summary,
  release notes, chart pins) is the index digest — the kubelet resolves the
  per-arch manifest from it at pull time, so one pinned digest schedules on both
  amd64 and arm64 (e.g. Graviton) nodes.
- A keyless **cosign signature** on every image (sigstore/Fulcio via GitHub
  Actions OIDC — no signing key, no stored secret), signed **recursively** — the
  index *and* each per-arch manifest — and an **SPDX-JSON SBOM per architecture**
  attached as a keyless cosign attestation to that architecture's manifest.
- A signed **SLSA build-provenance attestation** on every image
  (`actions/attest-build-provenance`), attached to the index digest as an OCI
  referrer. It is generated through the *same* keyless path as the signatures —
  the publish workflow's GitHub OIDC identity → a short-lived Fulcio cert →
  Rekor — so the provenance is **authenticated** (it records the workflow, repo,
  commit, and trigger that produced the image and cannot be forged by a pusher).
  This reaches **SLSA Build L2**; buildx's own *unsigned* default provenance is
  disabled in favour of it. Consumers verify it with `gh attestation verify` or
  `cosign verify-attestation` (see step 3).
- The **Helm chart**, packaged and pushed as an OCI artifact to
  `oci://ghcr.io/actions-gateway/charts/actions-gateway`, with its `version` and
  `appVersion` set to the release tag and a keyless **cosign signature** from the
  same Fulcio/Rekor flow as the images. Operators install it straight from the
  registry (`helm install … oci://…`) with the published image **digests** pinned
  — no `git clone` of the chart. OCI (over a `gh-pages` chart repo) is chosen so
  the chart reuses the images' registry, login, and keyless-signing path; Artifact
  Hub (see [`Chart.yaml`](../../charts/actions-gateway/Chart.yaml) annotations)
  indexes the OCI ref for discoverability.
- The **opt-in v2 CRD chart**, packaged and pushed alongside the main chart to
  `oci://ghcr.io/actions-gateway/charts/actions-gateway-crds-v2`, with the same
  version derivation and keyless signature. It ships only the v2alpha1
  (`actions-gateway.com`) CRDs — separated from the main chart because the large
  pod-template CRDs would otherwise push the main chart's Helm release Secret past
  its 1 MiB limit (Q149). Operators install it only when adopting the v2 API. Both
  chart packages are produced by the same `chart-publish` job.

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
   `ghcr.io/actions-gateway/{gmc,agc,proxy,worker,wrapper}` image packages **and the
   `ghcr.io/actions-gateway/charts/actions-gateway` chart package**. They inherit
   the repository's visibility and may start **private**. For third parties to run
   `cosign verify` / `helm pull` (and for an air-gapped operator to pull), set each
   package to **public** in the org's GHCR package settings, or keep them private
   and distribute pull credentials. Verification by *this project's* CI and by
   anyone with pull access works either way.
2. **Workflow permissions** are already declared in `publish.yml`
   (`packages: write` to push, `id-token: write` for keyless cosign and
   provenance, `attestations: write` for the build-provenance attestation). No
   repo secret is required — that is the point of keyless signing.

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

This verifies the five image signatures (`gmc`, `agc`, `proxy`, `worker`,
`wrapper`) plus the chart (whose tag is `X.Y.Z`,
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

Finally, confirm the **build-provenance attestation** is present and was minted
by *this* workflow. The attestation binds to the **index** digest (unlike the
per-arch SBOMs), so a tag reference resolves correctly:

```bash
# Verifies the signed SLSA provenance against the publish workflow's identity.
gh attestation verify oci://ghcr.io/actions-gateway/gmc:vX.Y.Z \
  --repo actions-gateway/github-actions-gateway \
  --signer-workflow actions-gateway/github-actions-gateway/.github/workflows/publish.yml
```

The equivalent cosign command and the predicate-inspection one-liner are in
[security-operations.md § Verify build provenance](security-operations.md#verify-build-provenance).
A provenance verification failure is the same **stop-ship** signal as a
`cosign verify` failure.

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
  carries `artifacthub.io/prerelease`. **`publish.yml` does not derive this from
  the tag** — it packages the chart from `charts/actions-gateway/` as is, so the
  committed value is baked into the published chart at tag time and is immutable
  once tagged. Keep it `"true"` while cutting `0.x` / `-rc` tags; it was flipped
  to `"false"` for the `v1.0.0` GA cut (Q98) so Artifact Hub no longer flags the
  listing as a prerelease. This flip must land in a normal PR **before** the
  stable tag is pushed.
- **Artifact Hub listing.** Discoverability metadata (description, keywords,
  prerelease flag) ships in the chart's own annotations. Ownership verification
  uses [`artifacthub-repo.yml`](../../artifacthub-repo.yml) at the repo root —
  register the OCI repository in the Artifact Hub control panel, copy the
  assigned `repositoryID` into that file, and push it to the registry as the
  repository-metadata OCI artifact (the file's header documents the exact
  steps). This is a one-time control-panel action, not part of `publish.yml`.
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

## The worker images: `wrapper` and `worker`

`publish.yml` builds and signs **two** worker-related images, both holding the
same `cmd/worker` wrapper that feeds the job payload into `Runner.Worker`:

- **`ghcr.io/actions-gateway/wrapper`** — a ~2 MB `FROM scratch` image with just
  the wrapper binary. The GMC forwards it to every AGC (`WRAPPER_IMAGE`), whose
  provisioner injects it into each worker pod — as a read-only OCI image volume
  (K8s ≥ 1.33) or via an initContainer below that — so the runner container can be
  the **unmodified upstream `ghcr.io/actions/actions-runner`** (or any tenant
  `workerImage`). This is what makes `DefaultWorkerImage` (still the digest-pinned
  upstream `actions-runner`, runner version locked to `agent.version` — see
  [building.md](../development/building.md#runner-version-pin-lockstep)) actually
  run jobs (Q235, [plan](../plan/archive/q235-worker-wrapper-injection.md)).
- **`ghcr.io/actions-gateway/worker`** — the full upstream `actions-runner` + the
  wrapper as `ENTRYPOINT` (~520 MB). Kept as an optional batteries-included image;
  unnecessary once injection is enabled, since the runner image is the upstream one
  with the wrapper injected. Retiring it is tracked separately.

Both are digest-pinned in the chart (`wrapper.image.digest` like `agc`/`proxy`),
so a release must publish the `wrapper` image and pin its digest for the default
install to run jobs.

## PR CI vs publish — what runs where

| Stage | Build image | Generate SBOM | Push to GHCR | Sign + SBOM-attest | Provenance attest |
|---|---|---|---|---|---|
| Pull request (`security-scan.yml`) | ✅ | ✅ (artifact) | — | — | — |
| Release tag (`publish.yml`) | ✅ | ✅ (attached) | ✅ | ✅ keyless | ✅ keyless (SLSA L2) |

PR CI proves the image builds and the SBOM generates so those paths can't silently
break; signing, SBOM attestation, and build-provenance attestation are all first
exercised on a real `v*` tag, which is why step 3 verification matters on every
release.

## Supply-chain integrity of the pipeline itself

The publish job holds `packages: write` + `id-token: write` + `attestations:
write`: its ambient OIDC identity *is* the release trust root. A hijacked upstream action tag executing in
that job could push and keyless-sign malicious images as the legitimate publish
identity. Three controls keep the pipeline itself trustworthy.

### Actions are pinned to full commit SHAs

Every `uses:` across `.github/workflows/` is pinned to a full 40-char commit SHA
with a trailing `# vX.Y.Z` comment for readability — never a floating tag (`@v4`)
or branch. A tag is mutable: whoever controls the upstream repo can repoint it at
new code, which would then run inside the privileged publish job. A SHA is
immutable. The runtime tool downloads in the publish path are pinned the same way:
`cosign` via `sigstore/cosign-installer` with an explicit `cosign-release`
(kept in step with `COSIGN_VERSION` in the Makefile so a local `make
verify-release` uses the same version that signed), and `syft` via the
`syft-version` input on `anchore/sbom-action/download-syft` (the action is
SHA-pinned, but syft itself is a runtime download).

**Bumping a pinned action.** Dependabot's `github-actions` ecosystem
([`.github/dependabot.yml`](../../.github/dependabot.yml)) opens weekly PRs that
bump both the SHA *and* the `# vX.Y.Z` comment, so the pins don't rot — review and
merge those like any dependency PR. To pin or bump by hand, resolve the tag to its
commit SHA and keep the comment in sync:

```bash
gh api repos/<owner>/<action>/commits/<tag> --jq '.sha'
# -> uses: <owner>/<action>@<sha> # <tag>
```

`syft-version` is **not** Dependabot-managed (it's a tool download, not an action
ref) — bump it by hand in `publish.yml` when you bump the `anchore/sbom-action`
SHA. `actionlint` (CI `lint` job) keeps SHA-pinned `uses:` lint-clean.

### Signing identity is tags-only

Releases are cut by pushing a `v*` tag, so a legitimate keyless signature's Fulcio
certificate records `publish.yml` running from `refs/tags/vX.Y.Z`. Two layers
enforce that a signature can only ever be a tag signature:

- **publish.yml refuses to run from a non-tag ref.** Both publish jobs' "Resolve
  publish tag" step rejects any `GITHUB_REF` that isn't `refs/tags/…`, so a
  `workflow_dispatch` run from a branch can't even reach the sign step.
- **`make verify-release` only accepts a tags identity.** The
  `--certificate-identity-regexp` is anchored to `…/publish\.yml@refs/tags/v.*$`
  (sourced from `release_identity_regexp` in
  [`scripts/lib/common.sh`](../../scripts/lib/common.sh)), so a signature minted
  from `refs/heads/…` is rejected even if one were somehow produced. The
  `scripts/verify-release-test.sh` assertions (run by `make check` and CI) guard
  that the regexp stays tags-only.

Together these close the hole where repo-write could dispatch `publish.yml` from a
scratch branch, overwrite a released GHCR version tag, and still pass
verification.

### Build inputs and the signer binary are integrity-checked

The first two controls protect *who* runs the pipeline and *how* signatures are
trusted; this one protects *what goes into* the signed artifacts and *the tool
that verifies them*.

- **Vendored dependencies are gated against `go.sum`.** Images build with `go
  build -mod=vendor`, but `-mod=vendor` only checks `vendor/modules.txt`
  consistency — it never verifies that the vendored *source* matches the hashes
  in `go.sum`. A malicious or accidental edit under `vendor/` (or `tools/vendor/`)
  would otherwise compile straight into the signed release images. The
  `vendor-check` job (in `unit-test.yml`, single source of truth `make
  vendor-check` → `scripts/vendor-check.sh`) re-runs the workspace-vendor flow —
  which re-fetches every module verified against `go.sum` — and fails on any diff
  against the committed trees. A Dependabot `go.mod` bump legitimately fails this
  gate until a follow-up vendor sync lands; that is the intended signal (see
  [go-workspaces.md § Changing dependencies](../development/go-workspaces.md#changing-dependencies)).
- **The cosign verify binary is checksummed.** GitHub release assets are mutable
  for an existing tag, so a raw download of the release verifier can't be trusted
  on its own. The publish pipeline obtains cosign via the SHA-pinned
  `sigstore/cosign-installer` action (which performs its own signature
  verification); the *local* verify path (`make verify-release` →
  `scripts/download-cosign.sh`) pins the expected SHA256 per platform in-repo and
  refuses to install a binary whose bytes don't match. Bumping `COSIGN_VERSION`
  must add the new digests to that script (it fails closed on an unpinned
  version) — the same deliberate-pin discipline as `KIND_BINARY_SHA256` in
  `e2e-test.yml`.
