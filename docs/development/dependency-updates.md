# Keeping pinned dependencies current

Everything this repo builds on is pinned for reproducibility — Go modules,
Docker base images, GitHub Actions, and a handful of tool versions. Pinning
trades automatic freshness for determinism, so each pinned surface needs a
channel that bumps it on a schedule. This doc is the map of those channels: what
is automated, by what, and where the manual edges are.

## What updates each surface

| Surface | Where it's pinned | Update channel |
|---|---|---|
| Go module deps (10 modules) | `*/go.mod`, vendored in `vendor/` + `tools/vendor/` | **Dependabot** (`gomod`, weekly, grouped) → auto-repaired by [`dependabot-go-sync.yml`](../../.github/workflows/dependabot-go-sync.yml). Note: `api/` (the shared v2 kinds module) is **not yet** listed in `dependabot.yml`, so it currently receives no automated `gomod` bumps. |
| GitHub Actions (`uses:` SHAs) | `.github/workflows/*.yml` | **Dependabot** (`github-actions`, weekly, grouped) |
| Docker base images (`FROM` digests) | `cmd/*/Dockerfile`, `test/fakegithub/Dockerfile` | **Dependabot** (`docker`, weekly, grouped) |
| kind version + binary checksum | `KIND_VERSION` / `KIND_BINARY_SHA256` in [`e2e-reusable.yml`](../../.github/workflows/e2e-reusable.yml) | **updatecli** ([`updatecli.d/kind.yaml`](../../updatecli.d/kind.yaml), weekly) |
| Calico version (2 files) | `CALICO_VERSION` in `e2e-reusable.yml` **and** the root `Makefile` | **updatecli** ([`updatecli.d/calico.yaml`](../../updatecli.d/calico.yaml), weekly — rewrites both, so they can't drift) |
| shellcheck version + checksum | `SHELLCHECK_VERSION` / `SHELLCHECK_SHA256` in [`unit-test.yml`](../../.github/workflows/unit-test.yml) | **updatecli** ([`updatecli.d/shellcheck.yaml`](../../updatecli.d/shellcheck.yaml), weekly) |
| polaris version + checksum | `POLARIS_VERSION` / `POLARIS_SHA256` in [`security-scan.yml`](../../.github/workflows/security-scan.yml) | **updatecli** ([`updatecli.d/polaris.yaml`](../../updatecli.d/polaris.yaml), weekly) |
| buildkit builder image digest (3 files) | `BUILDKIT_IMAGE` in `e2e-reusable.yml`, `security-scan.yml` **and** `publish.yml` | **updatecli** ([`updatecli.d/buildkit.yaml`](../../updatecli.d/buildkit.yaml), weekly — rewrites all three, so they can't drift) |
| envtest Kubernetes version (3 files) | `ENVTEST_K8S_VERSION` in [`integration-test.yml`](../../.github/workflows/integration-test.yml) **and** `cmd/gmc/Makefile` + `cmd/agc/Makefile` | **updatecli** ([`updatecli.d/envtest.yaml`](../../updatecli.d/envtest.yaml), weekly — rewrites all three, so they can't drift; resolved from controller-tools' `envtest-releases.yaml`, **no auto-merge** since it moves the tested Kubernetes version — keep it on the same minor as `KIND_NODE_IMAGE`. The review-gated PR doubles as a **latest-Kubernetes compatibility canary**: it runs the integration tier against the newest envtest release, so a green PR confirms the project still works on the latest version) |
| kind node image | `KIND_NODE_IMAGE` in `e2e-reusable.yml` | **manual** (changes the tested Kubernetes version — a deliberate choice; keep the envtest version above on the same Kubernetes minor) |

Dependabot config: [`.github/dependabot.yml`](../../.github/dependabot.yml). The
supply-chain gates that catch drift on any of these (`vendor-check`,
`tidy-check`, `license-notices`, trivy, govulncheck) run in CI and via
`make check`.

## Why updatecli, and not just Dependabot

Dependabot only updates dependencies it can recognise inside a package manifest
(a `go.mod`, a `Dockerfile` `FROM`, a workflow `uses:`). Several versions we pin
are plain **shell env vars** in CI workflows — `KIND_VERSION`, `CALICO_VERSION`,
`POLARIS_VERSION`, `SHELLCHECK_VERSION` — and Dependabot is blind to them. Worse,
some pair a version with a **companion checksum** (`KIND_BINARY_SHA256`, and the
polaris/shellcheck SHAs): even a regex-based bumper would update the version and
leave the checksum stale.

[updatecli](https://www.updatecli.io/) closes that gap. Its declarative
manifests resolve an upstream version *and* fetch/compute the matching checksum,
then open a PR updating both together — the version+checksum coupling no
manifest-aware bot handles.

## The manifests

Each pin gets one `updatecli.d/*.yaml` manifest; the workflow applies them all in
one run, opening a separate PR per manifest. They share a shape:

1. **A version source** — a `githubrelease` source with a `semver` filter that
   resolves the latest non-prerelease tag of the upstream repo.
2. **(version+checksum pins only) a checksum source** — a `shell` source that
   `dependson` the version source and, with the resolved tag templated in,
   produces the matching SHA-256. Two strategies, picked per upstream:
   - **Fetch a published checksum** — [`scripts/updatecli/kind-linux-amd64-sha256.sh`](../../scripts/updatecli/kind-linux-amd64-sha256.sh)
     reads kind's per-binary `kind-linux-amd64.sha256sum` (the value a human would
     copy by hand). Cheap; preferred when the upstream publishes one.
   - **Hash the bytes** — [`scripts/updatecli/sha256-of-url.sh`](../../scripts/updatecli/sha256-of-url.sh)
     downloads the asset and hashes it, for upstreams (e.g. shellcheck) that
     publish no checksum file.
3. **File targets** — regex-replace each pin in place and open one PR.

Two exceptions to the `githubrelease` source. [`buildkit.yaml`](../../updatecli.d/buildkit.yaml)
pins a Docker image *digest*, not a release tarball, so its source is a
`dockerdigest` (resolving the current digest of the floating `buildx-stable-1`
tag); it still ends in file targets — rewriting the `@sha256:…` suffix across all
three workflows that boot a buildx builder. [`envtest.yaml`](../../updatecli.d/envtest.yaml)
has no GitHub release to track (envtest binaries are an index, not tagged
releases), so its source is a `shell` script
([`scripts/updatecli/latest-envtest-version.sh`](../../scripts/updatecli/latest-envtest-version.sh))
that reads controller-tools' `envtest-releases.yaml` and prints the latest stable
`1.<minor>.x` — guaranteeing the chosen minor has published binaries — then
rewrites `ENVTEST_K8S_VERSION` across the workflow and both controller Makefiles.

| Manifest | Pins | Checksum strategy |
|---|---|---|
| [`kind.yaml`](../../updatecli.d/kind.yaml) | `KIND_VERSION` + `KIND_BINARY_SHA256` | published `.sha256sum` |
| [`calico.yaml`](../../updatecli.d/calico.yaml) | `CALICO_VERSION` in two files | none (version-only) |
| [`shellcheck.yaml`](../../updatecli.d/shellcheck.yaml) | `SHELLCHECK_VERSION` + `SHELLCHECK_SHA256` | hash the tarball |
| [`polaris.yaml`](../../updatecli.d/polaris.yaml) | `POLARIS_VERSION` + `POLARIS_SHA256` | published `checksums.txt` line |
| [`buildkit.yaml`](../../updatecli.d/buildkit.yaml) | `BUILDKIT_IMAGE` digest in three files | none (`dockerdigest` resolves the digest directly) |
| [`envtest.yaml`](../../updatecli.d/envtest.yaml) | `ENVTEST_K8S_VERSION` in three files | none (version-only; `shell` source from the envtest-releases index) |

**Gate tools open PRs that may go red.** shellcheck and polaris are lint/scan
gates: a new release can add findings. The bump PR running CI is
exactly the point — a human adopts the new version (fixing or justifying the new
findings) or holds it, instead of the pin silently rotting.

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

The scheduled [`updatecli.yml`](../../.github/workflows/updatecli.yml) workflow
runs `apply` weekly (Mondays, after the Dependabot wave); `workflow_dispatch`
with `dry_run: true` runs `diff`.

## Operating notes

- **Repo setting prerequisite.** updatecli opens PRs with the default
  `GITHUB_TOKEN`, so *Settings → Actions → General → "Allow GitHub Actions to
  create and approve pull requests"* must be enabled.
- **CI does not auto-run on the bump PR.** GitHub never triggers workflows from a
  `GITHUB_TOKEN`-authored PR (the same constraint [`dependabot-go-sync.yml`](../../.github/workflows/dependabot-go-sync.yml)
  documents). The relevant gate must run before merge — e2e for a kind or Calico
  bump, the lint job for shellcheck — so a maintainer re-triggers checks by
  closing and reopening the PR. A stored App token would remove this step; it is
  deliberately not used yet (one more secret to manage), matching the go-sync
  rationale.
- **Triage cadence.** updatecli is scheduled just after Dependabot so all
  dependency PRs land together and are reviewed in one weekly pass.
- **Manifest rot.** A manifest is bespoke: if an upstream renames a release asset
  or changes its checksum-file layout, the run fails or no-ops. Watch the
  scheduled run's status; a silent green with no PR for a long-stale pin is the
  signal to check the manifest.

## Deliberately manual pins

This is pinned but **not** on the updatecli cadence, for a concrete reason rather
than because automation is missing:

- **`KIND_NODE_IMAGE`** — the node image a kind release recommends lives in its
  release notes, not a clean datasource, and bumping it changes the tested
  Kubernetes version (and the Calico compatibility window). Review and bump it by
  hand in the kind updatecli PR when the Kubernetes version should move.
