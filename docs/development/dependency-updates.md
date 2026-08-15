# Keeping pinned dependencies current

Everything this repo builds on is pinned for reproducibility — Go modules, Docker base images, GitHub Actions, and a handful of tool versions.
Pinning trades automatic freshness for determinism, so each pinned surface needs a channel that bumps it on a schedule.
This doc is the map of those channels: what is automated, by what, and where the manual edges are.

## What updates each surface

| Surface | Where it's pinned | Update channel |
|---|---|---|
| Go module deps (10 modules) | `*/go.mod`, vendored in `vendor/` + `tools/vendor/` | **Dependabot** (`gomod`, weekly, grouped) → auto-repaired by [`dependabot-go-sync.yml`](../../.github/workflows/dependabot-go-sync.yml), and auto-rebased when stale by [`dependabot-rebase-stale.yml`](../../.github/workflows/dependabot-rebase-stale.yml) |
| GitHub Actions (`uses:` SHAs) | `.github/workflows/*.yml` | **Dependabot** (`github-actions`, weekly, grouped) |
| Docker base images (`FROM` digests) | `Dockerfile` (all image stages), `scripts/dogfood/*/Dockerfile` | **Dependabot** (`docker`, weekly, grouped) |
| kind version + binary checksum (2 files) | `KIND_VERSION` / `KIND_BINARY_SHA256` in [`e2e-reusable.yml`](../../.github/workflows/e2e-reusable.yml) **and** [`autoscaler-drift.yml`](../../.github/workflows/autoscaler-drift.yml) | **updatecli** ([`updatecli.d/kind.yaml`](../../updatecli.d/kind.yaml), weekly — rewrites both, so they can't drift. This PR doubles as the [live-autoscaler drift gate's](testing.md#its-cadence-the-version-bump-not-a-clock) trigger: the autoscaler harness pins no node image, so a kind bump that moves the default node image's Kubernetes minor is the moment `CA_VERSION` must move too) |
| Calico version (2 files) | `CALICO_VERSION` in `e2e-reusable.yml` **and** the root `Makefile` | **updatecli** ([`updatecli.d/calico.yaml`](../../updatecli.d/calico.yaml), weekly — rewrites both, so they can't drift) |
| shellcheck version + checksum | `SHELLCHECK_VERSION` / `SHELLCHECK_SHA256` in [`unit-test.yml`](../../.github/workflows/unit-test.yml) | **updatecli** ([`updatecli.d/shellcheck.yaml`](../../updatecli.d/shellcheck.yaml), weekly) |
| polaris version + checksum | `POLARIS_VERSION` / `POLARIS_SHA256` in [`security-scan.yml`](../../.github/workflows/security-scan.yml) | **updatecli** ([`updatecli.d/polaris.yaml`](../../updatecli.d/polaris.yaml), weekly) |
| syft version + checksum (2 files) | `SYFT_VERSION` / `SYFT_SHA256` in [`publish.yml`](../../.github/workflows/publish.yml) **and** [`security-scan.yml`](../../.github/workflows/security-scan.yml) | **updatecli** ([`updatecli.d/syft.yaml`](../../updatecli.d/syft.yaml), weekly, rewriting both so the release-time and PR-time SBOM generators can't drift apart) |
| buildkit builder image digest (3 files) | `BUILDKIT_IMAGE` in `e2e-reusable.yml`, `security-scan.yml` **and** `publish.yml` | **updatecli** ([`updatecli.d/buildkit.yaml`](../../updatecli.d/buildkit.yaml), weekly — rewrites all three, so they can't drift) |
| envtest Kubernetes version (3 files) | `ENVTEST_K8S_VERSION` in [`integration-test.yml`](../../.github/workflows/integration-test.yml) **and** `cmd/gmc/Makefile` + `cmd/agc/Makefile` | **updatecli** ([`updatecli.d/envtest.yaml`](../../updatecli.d/envtest.yaml), weekly — rewrites all three, so they can't drift; resolved from controller-tools' `envtest-releases.yaml`, **no auto-merge** since it moves the tested Kubernetes version — keep it on the same minor as `KIND_NODE_IMAGE`. The review-gated PR doubles as a **latest-Kubernetes compatibility canary**: it runs the integration tier against the newest envtest release, so a green PR confirms the project still works on the latest version) |
| kind node image | `KIND_NODE_IMAGE` in `e2e-reusable.yml` | **manual** (changes the tested Kubernetes version — a deliberate choice; keep the envtest version above on the same Kubernetes minor) |
| cluster-autoscaler **patch** (drift harness) | `CA_VERSION` in [`scripts/e2e/autoscaler-cluster.sh`](../../scripts/e2e/autoscaler-cluster.sh) | **updatecli** ([`updatecli.d/cluster-autoscaler.yaml`](../../updatecli.d/cluster-autoscaler.yaml), weekly — moves the patch *within* the pinned minor only, resolved from the registry's own tag list. **No auto-merge**: the PR edits `scripts/e2e/autoscaler-cluster.sh`, which is the [live-autoscaler drift gate's](testing.md#its-cadence-the-version-bump-not-a-clock) path filter, so it exists to run that gate against the new release) |
| cluster-autoscaler **minor** + kwok (drift harness) | `CA_VERSION`'s minor / `KWOK_VERSION` in [`scripts/e2e/autoscaler-cluster.sh`](../../scripts/e2e/autoscaler-cluster.sh) | **manual, prompted by the kind bump** — cluster-autoscaler is released per Kubernetes minor and the harness runs kind's *default* node image, so `CA_VERSION`'s minor is whatever kind ships. A kind bump that moves that minor fails the drift gate on skew; bump `CA_VERSION` to the matching CA minor in the same PR. See [testing.md](testing.md#its-cadence-the-version-bump-not-a-clock) |
| Karpenter (drift harness) | `KARPENTER_VERSION` in [`scripts/e2e/karpenter-cluster.sh`](../../scripts/e2e/karpenter-cluster.sh) | **updatecli** ([`updatecli.d/karpenter.yaml`](../../updatecli.d/karpenter.yaml), weekly — the **latest** upstream release, minor or patch: Karpenter is not released per Kubernetes minor, so there is no minor for the kind bump to own. **No auto-merge**: the PR edits `scripts/e2e/karpenter-cluster.sh`, which is the [drift gate's](testing.md#its-cadence-the-version-bump-not-a-clock) path filter, so it exists to run that gate against the new release) |

Dependabot config: [`.github/dependabot.yml`](../../.github/dependabot.yml).
The supply-chain gates that catch drift on any of these (`vendor-check`, `tidy-check`, `license-notices`, trivy, govulncheck) run in CI and via `make check`.

## Why updatecli, and not just Dependabot

Dependabot only updates dependencies it can recognise inside a package manifest (a `go.mod`, a `Dockerfile` `FROM`, a workflow `uses:`).
Several versions we pin are plain **shell env vars** in CI workflows — `KIND_VERSION`, `CALICO_VERSION`, `POLARIS_VERSION`, `SHELLCHECK_VERSION` — and Dependabot is blind to them.
Worse, some pair a version with a **companion checksum** (`KIND_BINARY_SHA256`, and the polaris/shellcheck SHAs): even a regex-based bumper would update the version and leave the checksum stale.

[updatecli](https://www.updatecli.io/) closes that gap.
Its declarative manifests resolve an upstream version *and* fetch/compute the matching checksum, then open a PR updating both together — the version+checksum coupling no manifest-aware bot handles.

## The manifests

Each pin gets one `updatecli.d/*.yaml` manifest; the workflow applies them all in one run, opening a separate PR per manifest.
They share a shape:

1. **A version source** — a `githubrelease` source with a `semver` filter that resolves the latest non-prerelease tag of the upstream repo.
2. **(version+checksum pins only) a checksum source** — a `shell` source that `dependson` the version source and, with the resolved tag templated in, produces the matching SHA-256.
   Two strategies, picked per upstream:
   - **Fetch a published checksum** — [`scripts/updatecli/kind-linux-amd64-sha256.sh`](../../scripts/updatecli/kind-linux-amd64-sha256.sh) reads kind's per-binary `kind-linux-amd64.sha256sum` (the value a human would copy by hand).
     Cheap; preferred when the upstream publishes one.
   - **Hash the bytes** — [`scripts/updatecli/sha256-of-url.sh`](../../scripts/updatecli/sha256-of-url.sh) downloads the asset and hashes it, for upstreams (e.g. shellcheck) that publish no checksum file.
3. **File targets** — regex-replace each pin in place and open one PR.

Three exceptions to the `githubrelease` source.
[`buildkit.yaml`](../../updatecli.d/buildkit.yaml) pins a Docker image *digest*, not a release tarball, so its source is a `dockerdigest` (resolving the current digest of the floating `buildx-stable-1` tag); it still ends in file targets — rewriting the `@sha256:…` suffix across all three workflows that boot a buildx builder.
[`envtest.yaml`](../../updatecli.d/envtest.yaml) has no GitHub release to track (envtest binaries are an index, not tagged releases), so its source is a `shell` script ([`scripts/updatecli/latest-envtest-version.sh`](../../scripts/updatecli/latest-envtest-version.sh)) that reads controller-tools' `envtest-releases.yaml` and prints the latest stable `1.<minor>.x` — guaranteeing the chosen minor has published binaries — then rewrites `ENVTEST_K8S_VERSION` across the workflow and both controller Makefiles.

[`cluster-autoscaler.yaml`](../../updatecli.d/cluster-autoscaler.yaml) is the third, and the only manifest that resolves a version **relative to the pin it is replacing**.
It reads the current `CA_VERSION` out of [`scripts/e2e/autoscaler-cluster.sh`](../../scripts/e2e/autoscaler-cluster.sh) with a `file` source, then hands it to [`scripts/updatecli/latest-cluster-autoscaler-patch.sh`](../../scripts/updatecli/latest-cluster-autoscaler-patch.sh), which returns the newest patch published **inside that same Kubernetes minor** — never a newer minor, never older than the pin.
The minor belongs to the kind bump (cluster-autoscaler ships one series per Kubernetes minor, and the harness runs kind's default node image), so letting this manifest cross one would manufacture exactly the version skew the drift gate exists to report.
Both invariants are asserted by `scripts/updatecli/latest-cluster-autoscaler-patch-test.sh` under `make scripts-test`.
Its source is the registry's own OCI tag list rather than GitHub releases: the harness pulls `registry.k8s.io/autoscaling/cluster-autoscaler`, a git tag exists before its image is published, and the release tags (`cluster-autoscaler-1.36.1`) are not semver anyway.

| Manifest | Pins | Checksum strategy |
|---|---|---|
| [`kind.yaml`](../../updatecli.d/kind.yaml) | `KIND_VERSION` + `KIND_BINARY_SHA256` in two files | published `.sha256sum` |
| [`calico.yaml`](../../updatecli.d/calico.yaml) | `CALICO_VERSION` in two files | none (version-only) |
| [`shellcheck.yaml`](../../updatecli.d/shellcheck.yaml) | `SHELLCHECK_VERSION` + `SHELLCHECK_SHA256` | hash the tarball |
| [`polaris.yaml`](../../updatecli.d/polaris.yaml) | `POLARIS_VERSION` + `POLARIS_SHA256` | published `checksums.txt` line |
| [`syft.yaml`](../../updatecli.d/syft.yaml) | `SYFT_VERSION` + `SYFT_SHA256` in two files | published `syft_<version>_checksums.txt` line |
| [`buildkit.yaml`](../../updatecli.d/buildkit.yaml) | `BUILDKIT_IMAGE` digest in three files | none (`dockerdigest` resolves the digest directly) |
| [`envtest.yaml`](../../updatecli.d/envtest.yaml) | `ENVTEST_K8S_VERSION` in three files | none (version-only; `shell` source from the envtest-releases index) |
| [`cluster-autoscaler.yaml`](../../updatecli.d/cluster-autoscaler.yaml) | `CA_VERSION` patch in `scripts/e2e/autoscaler-cluster.sh` | none (version-only; `file` source reads the current pin, `shell` source resolves the newest patch in its minor from the registry tag list) |
| [`karpenter.yaml`](../../updatecli.d/karpenter.yaml) | `KARPENTER_VERSION` in `scripts/e2e/karpenter-cluster.sh` | none (version-only; `githubrelease` — the harness builds from the git tag, so the repo's releases are the datasource) |

**Gate tools open PRs that may go red.** shellcheck and polaris are lint/scan gates: a new release can add findings.
The bump PR running CI is exactly the point — a human adopts the new version (fixing or justifying the new findings) or holds it, instead of the pin silently rotting.
`cluster-autoscaler.yaml` and `karpenter.yaml` are the strongest form of this: the bump is not a refresh with a test attached, it *is* the experiment.
Their PRs exist so the [live-autoscaler drift gate](testing.md#its-cadence-the-version-bump-not-a-clock) installs the new release and checks whether upstream reworded the event vocabulary the capacity gate matches on.

### Running it locally

`updatecli diff` is a read-only dry run — it prints the changes and opens no PR:

```bash
# Download the binary once (e.g. into the gitignored tmp/), then from the repo root:
export UPDATECLI_OWNER="$(gh repo view --json owner -q .owner.login)"
export UPDATECLI_REPO="$(gh repo view --json name -q .name)"
export UPDATECLI_ACTOR='github-actions[bot]'
export UPDATECLI_GITHUB_TOKEN="$(gh auth token)"
updatecli diff --config updatecli.d/
```

The scheduled [`updatecli.yml`](../../.github/workflows/updatecli.yml) workflow runs `apply` weekly (Mondays, after the Dependabot wave); `workflow_dispatch` with `dry_run: true` runs `diff`.

## Operating notes

- **Repo setting prerequisite.** updatecli opens PRs with the default `GITHUB_TOKEN`, so *Settings → Actions → General → "Allow GitHub Actions to create and approve pull requests"* must be enabled.
- **CI does not auto-run on the bump PR.** GitHub never triggers workflows from a `GITHUB_TOKEN`-authored PR (the same constraint [`dependabot-go-sync.yml`](../../.github/workflows/dependabot-go-sync.yml) documents).
  The relevant gate must run before merge — e2e for a kind or Calico bump, the lint job for shellcheck — so a maintainer re-triggers checks by closing and reopening the PR.
  **On a kind, cluster-autoscaler, or Karpenter bump this step is the whole point, not a formality:** those PRs are the only triggers the [live-autoscaler drift gate](testing.md#its-cadence-the-version-bump-not-a-clock) has, so merging one without re-running checks is the one path that lets an upstream vocabulary reword through unobserved.
  A stored App token would remove this step; it is deliberately not used yet (one more secret to manage), matching the go-sync rationale.
- **Triage cadence.** updatecli is scheduled just after Dependabot so all dependency PRs land together and are reviewed in one weekly pass.
- **A stale Go bump PR rebases itself, but never merge one by hand.** The vendor sync commit makes Dependabot disown the branch, so a Go bump PR left unmerged while `main` moves goes conflicting and cannot self-rebase.
  [`dependabot-rebase-stale.yml`](../../.github/workflows/dependabot-rebase-stale.yml) replays its bumps onto current `main` instead (Q427).
  Resolving that conflict by hand can silently downgrade a module; see [go-workspaces.md](go-workspaces.md#a-synced-branch-stops-auto-rebasing-and-is-rebased-for-you).
- **Manifest rot.** A manifest is bespoke: if an upstream renames a release asset or changes its checksum-file layout, the run fails or no-ops.
  Watch the scheduled run's status; a silent green with no PR for a long-stale pin is the signal to check the manifest.

## Deliberately manual pins

These are pinned but **not** on the updatecli cadence, for a concrete reason rather than because automation is missing:

- **`KIND_NODE_IMAGE`** — the node image a kind release recommends lives in its release notes, not a clean datasource, and bumping it changes the tested Kubernetes version (and the Calico compatibility window).
  Review and bump it by hand in the kind updatecli PR when the Kubernetes version should move.
- **`CA_VERSION`'s Kubernetes minor, and `KWOK_VERSION`** (both copies — its twin in [`scripts/e2e/karpenter-cluster.sh`](../../scripts/e2e/karpenter-cluster.sh) is kept the same), in [`scripts/e2e/autoscaler-cluster.sh`](../../scripts/e2e/autoscaler-cluster.sh) — the cluster-autoscaler and kwok releases behind the [live-autoscaler drift gate](testing.md#the-live-autoscaler-drift-gate). cluster-autoscaler ships one release series per Kubernetes minor and the harness runs kind's *default* node image, so the minor is not this pin's to choose: it follows `KIND_NODE_IMAGE`'s and moves by hand, in the kind bump PR, with `make test-autoscaler` run in the same change.
  Only the **patch** inside that minor is automated ([`cluster-autoscaler.yaml`](../../updatecli.d/cluster-autoscaler.yaml)) — the version move is what triggers the gate, so patch releases that no one pinned used to go untested until the next minor came round (Q483).
