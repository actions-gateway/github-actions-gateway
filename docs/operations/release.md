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
- The **signed v2 CRD manifest** (Q276), a pre-rendered
  `actions-gateway-crds-v2.yaml` (rendered for the default `gmc-system` namespace)
  attached to the tag's **GitHub Release** with a keyless **cosign blob signature**
  (`sign-blob` → a Sigstore bundle, `actions-gateway-crds-v2.yaml.cosign.bundle`).
  This is the helm-free manual install path — the v2 CRD chart is too large to
  `helm install`, so operators
  `kubectl apply --server-side -f …/releases/download/<tag>/actions-gateway-crds-v2.yaml`.
  The `chart-publish` job renders, signs, and uploads it (widening the job to
  `contents: write`).
- The **`gag-migrate` CLI binaries** (Q306), cross-compiled by `chart-publish` for
  the operator platform matrix (`linux/amd64`, `linux/arm64`, `darwin/amd64`,
  `darwin/arm64`, `windows/amd64`) via `scripts/build-migrate-binaries.sh` and
  attached to the **GitHub Release** as `gag-migrate-<tag>-<os>-<arch>` assets. A
  single `SHA256SUMS` manifest is keyless **cosign `sign-blob`**-signed
  (`SHA256SUMS.cosign.bundle`) — the same no-secret Fulcio/Rekor path as the v2 CRD
  manifest — so one signature covers the whole set (verify the manifest signature,
  then `sha256sum -c`). This is the one-shot v1→v2 migration tool, previously
  source-build-only.
- The **GitHub Release** itself (Q293), composed by `chart-publish`: the five image
  index digests, the `make verify-release` command, a generated changelog, and a
  tag-derived `--prerelease` flag. It is created only if the tag has no Release yet,
  so a maintainer's pre-tag curated notes are never clobbered.

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
- **Reconcile [`docs/roadmap.md`](../roadmap.md) against
  [`docs/STATUS.md`](https://github.com/actions-gateway/github-actions-gateway/blob/main/docs/STATUS.md)
  before you tag.** The same freeze that applies to the announce bar applies
  here: a stable tag deploys that tag's docs wholesale, so a stale roadmap is
  published permanently under that version. Nothing lints this. Walk the three
  sections against the backlog: anything in *In progress / near-term* with no
  Queue row has either shipped (move it to *Available now*) or been parked (move
  it to *Exploring*), and a Deferred row describing a capability an adopter would
  ask about belongs in *Exploring*. A 2026-07-25 audit found six of seven
  near-term items already shipped.
- **Optional: refresh the docs-site announce bar's highlight.** The banner in
  [`overrides/main.html`](../../overrides/main.html) is the "vX.Y.Z is here" strip
  at the top of every page on the site. Its **version needs no action**: it is
  derived from the git tags at build time, so a stable tag names itself
  automatically ([website.md § The announce bar](../development/website.md#the-announce-bar)).
  What you may want to update is the one-line highlight after it, and the
  `highlight_for` version that guards it. Leave both alone and the banner reads
  *"vX.Y.Z is here. Read the release notes."*, which is correct but plainer;
  update them together and it leads with this release's headline. Land the change
  before you tag either way.

  This used to be a manual version bump, and every stable tag to date missed it:
  `v1.0.0` shipped saying *"Alpha, pre-1.0"*, and `v1.1.0` and `v1.2.0` both said
  *"v1.0.0 is here"*. Preview what the tag will render, standing in the version
  you are about to cut (without it the local build resolves the *current* newest
  tag, which is still the previous release):

  ```bash
  GAG_DOCS_RELEASE=vX.Y.Z make docs-build && awk '/md-banner/,/<\/aside>/' site/index.html
  ```

  **`publish.yml` enforces the result** via its `announce-bar` job, which every
  publishing job depends on: it builds the site at the tag and fails the release
  if the *rendered* banner does not name it, before any image is pushed.
  Prereleases are exempt (they publish no docs), as is a backport tag cut after a
  newer minor, since the banner advertises the newest release by design.

  (A `docs_ref` seed of an already-cut release pins `overrides/` to the current
  checkout, so re-seeding refreshes the highlight on past tags too. See
  [website.md § Seeding](../development/website.md#seeding-already-released-versions).)

#### Validate the release candidate on dogfood

Before promoting a release-candidate line to a **stable** `vX.Y.Z` tag, validate
the *latest RC* functionally on the dogfood cluster. `main`-green covers
unit/integration/kind-e2e, but publishing an image the pipeline signed is not the
same as proving it runs jobs — this gate exercises real GAG-provisions-runners-on-GKE
behaviour the CI tiers can't observe. Run it before every GA (`vX.Y.Z`) cut; skip it
only for an RC-to-RC or a patch tag that changes nothing an operator runs.

The dogfood scripts pin GAG to any published ref via `GAG_IMAGE_TAG`, which resolves
both as an image tag (`ghcr.io/actions-gateway/{gmc,agc,proxy,wrapper}:<ref>`) and as
a git ref (for the matching CRDs) — an RC tag satisfies both by construction.

**One command runs the whole gate.** From a checkout of any post-Q74 ref with
`PROJECT`/`CLUSTER`/`ZONE`/`REPO` exported (App IDs auto-resolved; the one-time
`scripts/dogfood/e2e-setup.sh` must have run once):

```bash
PROJECT=… CLUSTER=… ZONE=… REPO=… scripts/dogfood/validate-release.sh vX.Y.Z-rc.N
```

`validate-release.sh` bakes in all the env and ordering below — deploy → route CI →
on-demand e2e → `gh run rerun` → CRD smoke → teardown — is idempotent, and
self-cleans back to 0 nodes on exit (success or failure). On failure it first
dumps a cluster snapshot (nodes, pods, unhealthy-pod detail, events) to the
gate's output, because the teardown's scale-to-0 evicts every pod and destroys
the evidence — read the `Failure diagnostics` section of a failed run's log
(e.g. the `FailedScheduling` events) instead of re-running the gate to watch
it fail again. The manual steps that
follow document what it does (and the recovery path if a leg needs re-running by
hand). From a detached checkout of the RC tag (`git switch --detach vX.Y.Z-rc.N`):

**If you just merged something, the gate waits before it spends anything.** `gh run
rerun` refuses a run that is still in flight, and the latest `e2e-test.yml` run is
usually the push-run of the merge you just made. The gate resolves and settles that
run up front — before the node scale-up, the deploy, and the e2e AGC — so a collision
costs a wait, not a cluster cycle. It polls for up to `E2E_WAIT_TIMEOUT` seconds
(default 1800), then fails with the run id. To skip the wait, pin an already-completed
run with `E2E_RUN_ID=<id>`; `E2E_WAIT_TIMEOUT=0` fails immediately instead of waiting.

The gate also checks every local tool it needs up front — including the pinned
`cosign` the final CRD-smoke leg verifies with (`make cosign` downloads it to
`.build/cosign`; `COSIGN=<path>` overrides) — so a missing binary fails the run
before it spends anything, not 25 minutes in.

1. **Deploy the RC to dogfood.** `setup.sh` needs `APP_ID`, `INSTALLATION_ID`, and
   `ASSUME_YES=1` exported alongside `GAG_IMAGE_TAG` — it reads the GitHub App private
   key from the macOS keychain (not from an env var), so run it on a macOS host that
   has that keychain entry. The cluster sits at **0 nodes at rest**, so `setup.sh`'s
   GMC-rollout wait has nothing to schedule on and **times out** — that is expected;
   `scripts/dogfood/start.sh` then scales the system pool to one node and completes the
   rollout. So: `GAG_IMAGE_TAG=vX.Y.Z-rc.N APP_ID=… INSTALLATION_ID=… ASSUME_YES=1 scripts/dogfood/setup.sh`
   (a timed-out rollout wait here is fine), then `scripts/dogfood/start.sh`. Run the
   one-time `scripts/dogfood/e2e-setup.sh` first if the e2e node pool / GitHub App
   Secret aren't set up yet. The cluster, context pinning, and prod-guard cautions are
   in [gke-dogfood.md](../plan/gke-dogfood.md).
2. **Run the e2e job matrix on GAG runners.** This is two moves, not one.
   `scripts/dogfood/e2e-start.sh` only **wires routing**: it spins up the on-demand
   e2e tenant's AGC and sets the `GAG_E2E_RUNNER` repo variable so `e2e-reusable.yml`
   targets the GAG scale set — it does **not** start a run. Trigger the matrix by
   **re-running an existing e2e workflow run** (`gh run rerun <run-id>` for
   `e2e-test.yml` / `e2e-calico.yml`); the rerun's jobs pick up the new `runs-on` and
   land on the RC's GAG-provisioned runners. Pick a **completed** run — `gh run rerun`
   rejects one that is still in flight (`This workflow is already running`), so check
   `gh run view <run-id> --json status` first when re-running this leg by hand. **Node contention:** the on-demand e2e AGC
   (~500m CPU) does not fit on the single `e2-standard-2` system node beside the
   always-on CI AGCs (the CI AGC goes `Pending`/`Insufficient cpu`), so temporarily add
   a system node (e.g. scale `default-pool` to 2) for the duration of the e2e leg and
   scale it back after. Require the matrix **green** — this is GAG running its own CI
   end-to-end on the RC images.
3. **Smoke the signed v2 CRD asset.** Download the RC release's
   `actions-gateway-crds-v2.yaml` + `.cosign.bundle`, `cosign verify-blob` against the
   publish identity (step 3 below), `kubectl apply --server-side` it, and assert the
   five v2 CRDs register — the helm-free install path operators actually use.
4. **Tear down.** `scripts/dogfood/e2e-stop.sh`, then `scripts/dogfood/stop.sh`
   (dogfood scales to 0 at rest).

A red matrix or a failed CRD smoke is a **stop-ship for the GA tag**: fix forward and
cut a new RC — never promote a known-bad RC to a stable tag.

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

> **The docs site publishes this release too (Q238).** A **stable** `vX.Y.Z` tag
> also triggers [`pages.yml`](../../.github/workflows/pages.yml), which deploys the
> release's docs as a new version on [actions-gateway.com](https://actions-gateway.com)
> and moves the `stable` alias + the site's default root redirect to it — so the
> public docs default to this release, with the unreleased `main` docs kept behind
> an opt-in `dev` version. Prerelease tags (`0.x`, `-rc`/`-alpha`/`-beta`) do
> **not** deploy the site. The versioned-docs model, and the one-time `mike`
> seeding of releases cut before it landed, are documented in
> [website.md § Versioned deploy](../development/website.md#versioned-deploy-mike).

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

`make verify-release` covers the OCI artifacts (images + both charts) but not the
GitHub Release's **signed v2 CRD manifest** asset (Q276), which is a *blob* signature —
verify it against the same identity with `verify-blob`. Download the manifest and its
bundle from the release, then:

```bash
cosign verify-blob --bundle actions-gateway-crds-v2.yaml.cosign.bundle \
  --certificate-identity-regexp '^https://github.com/actions-gateway/github-actions-gateway/\.github/workflows/publish\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  actions-gateway-crds-v2.yaml >/dev/null && echo OK
```

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
the **run summary** (the "Record published digest" step) **and into the GitHub
Release notes** (step 5). These are the **multi-arch index digests** — the single
ref that serves both amd64 and arm64 nodes. Operators pin the workload to the
digest (`gmc`, `agc`, `proxy`, `worker`, `wrapper`), not the mutable `vX.Y.Z` tag.
You can also resolve a digest directly:

```bash
docker buildx imagetools inspect ghcr.io/actions-gateway/gmc:vX.Y.Z \
  --format '{{json .Manifest.Digest}}'
```

### 5. Cut the GitHub Release

**`publish.yml` creates the GitHub Release itself** (Q293) — no manual step. The
`chart-publish` job's "Compose and create the GitHub Release" step writes the body
with the five `name@sha256:…` **index digests**, the `make verify-release
VERSION=vX.Y.Z` command, and a generated changelog (previous-tag compare link),
and sets `--prerelease` from the tag (0.x or a `-rc`/`-alpha`/`-beta` suffix ⇒
prerelease; a stable `≥1.0.0` tag ⇒ latest). So the default flow for this step is:
**nothing — verify the auto-created Release looks right.**

The step only creates a Release when the tag has **none yet**, so it never
clobbers curated notes. If you want richer notes (highlights, upgrade caveats),
**create the Release before pushing the tag** — e.g. `gh release create vX.Y.Z
--draft --notes-file …` — and the pipeline will leave your body untouched while
still attaching the signed v2 CRD manifest asset.

### 6. Chart version & metadata

The `chart-publish` job sets the published chart's `version` and `appVersion` to
the release tag (with the leading `v` stripped, since chart SemVer forbids it), so
there is **no manual Chart.yaml version bump** to remember — the in-repo
`version`/`appVersion` are dev placeholders the pipeline overrides at package time.
The **prerelease annotation is likewise derived from the tag** now, so nothing here
needs a hand-flip. The remaining items below are one-time setup or guardrails, not
per-release steps:

- **Prerelease annotation — derived, not hand-flipped (Q293).**
  [`Chart.yaml`](../../charts/actions-gateway/Chart.yaml) carries
  `artifacthub.io/prerelease`, but its committed value is a **dev placeholder**:
  `publish.yml` overrides it with `yq` before `helm package`, setting `"true"` for
  a `0.x` or `-rc`/`-alpha`/`-beta` tag and `"false"` for a stable `≥1.0.0` tag
  (same test that sets the Release's `--prerelease` flag). There is **no flip PR**
  to land before or after a cut. The v2 CRD chart is stamped the same way.
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
  --set proxy.image.digest=sha256:<proxy> \
  --set wrapper.image.digest=sha256:<wrapper>
```

## Patch releases and backports

A patch release (`vX.Y.Z+1`) is **bugfix-only** by SemVer. That has a release-
engineering consequence: **do not tag a patch from `main` once `main` has merged
features headed for the next minor** — doing so ships those unreleased features in
the patch's images *and* chart, and advertises them in the patch's docs (the site
builds each version from its tag). Tag a patch from the released line instead:

- **Ephemeral branch off the tag (branchless-friendly).** For a one-off patch:
  `git switch --detach vX.Y.Z`, `git switch -c release-X.Y`, cherry-pick the fix,
  tag `vX.Y.(Z+1)`, push the tag. Delete the branch afterward if you don't maintain
  the line.
- **Long-lived release branch.** If you support multiple minor lines at once, keep
  a `release-X.Y` branch at the minor's tag and land backported fixes on it.

You only need a branch/backport **when `main` isn't itself the intended patch** —
i.e. when it has diverged past the release with content you don't want in the
patch. If `main` is clean and ready to ship, that's the next **minor** (`vX.(Y+1).0`),
not a patch.

The **docs site follows automatically**: `mike` builds each version from its own
tag, so a patch tagged off the release line publishes docs with only that line's
content — no unreleased features. And the site's `stable` alias + default root
redirect move **only to the highest released version**, so a backport patch
released *after* a newer minor updates its own version's docs without demoting the
site (see
[website.md § Versioned deploy](../development/website.md#versioned-deploy-mike)).
No docs-specific branch is ever required beyond what the release itself needs.

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

Only `wrapper` is digest-pinned in the chart (`wrapper.image.digest`, like
`agc`/`proxy`), so a release must publish the `wrapper` image and pin its digest
for the default install to run jobs. The `worker` image has no chart `image`
block — it is the optional batteries-included image a tenant opts into via its
per-RunnerGroup `workerImage`, not a chart-provisioned one — so nothing in the
chart pins it.

## PR CI vs publish — what runs where

| Stage | Build image | Generate SBOM | Push to GHCR | Sign + SBOM-attest | Provenance attest |
|---|---|---|---|---|---|
| Pull request (`security-scan.yml`) | ✅ | ✅ (artifact) | — | — | — |
| Release tag (`publish.yml`) | ✅ | ✅ (attached) | ✅ | ✅ keyless | ✅ keyless (SLSA L2) |

PR CI proves the image builds and the SBOM generates so those paths can't silently
break; signing, SBOM attestation, and build-provenance attestation are all first
exercised on a real `v*` tag, which is why step 3 verification matters on every
release.

`publish.yml` also runs one **pre-publish gate**, `announce-bar`, that every
publishing job depends on. It builds the docs site at the tag and asserts the
rendered banner names it (see [Pre-flight](#1-pre-flight)), so a docs-site banner
advertising the wrong version stops the release before an image, chart, or GitHub
Release exists, rather than after.

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
