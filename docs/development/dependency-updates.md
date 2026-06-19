# Keeping pinned dependencies current

Everything this repo builds on is pinned for reproducibility — Go modules,
Docker base images, GitHub Actions, and a handful of tool versions. Pinning
trades automatic freshness for determinism, so each pinned surface needs a
channel that bumps it on a schedule. This doc is the map of those channels: what
is automated, by what, and where the manual edges are.

## What updates each surface

| Surface | Where it's pinned | Update channel |
|---|---|---|
| Go module deps (9 modules) | `*/go.mod`, vendored in `vendor/` + `tools/vendor/` | **Dependabot** (`gomod`, weekly, grouped) → auto-repaired by [`dependabot-go-sync.yml`](../../.github/workflows/dependabot-go-sync.yml) |
| GitHub Actions (`uses:` SHAs) | `.github/workflows/*.yml` | **Dependabot** (`github-actions`, weekly, grouped) |
| Docker base images (`FROM` digests) | `cmd/*/Dockerfile`, `test/fakegithub/Dockerfile` | **Dependabot** (`docker`, weekly, grouped) |
| kind version + binary checksum | `KIND_VERSION` / `KIND_BINARY_SHA256` in [`e2e-reusable.yml`](../../.github/workflows/e2e-reusable.yml) | **updatecli** ([`updatecli.d/kind.yaml`](../../updatecli.d/kind.yaml), weekly) |
| Calico version (2 files) | `CALICO_VERSION` in `e2e-reusable.yml` **and** the root `Makefile` | **updatecli** ([`updatecli.d/calico.yaml`](../../updatecli.d/calico.yaml), weekly — rewrites both, so they can't drift) |
| shellcheck version + checksum | `SHELLCHECK_VERSION` / `SHELLCHECK_SHA256` in [`unit-test.yml`](../../.github/workflows/unit-test.yml) | **updatecli** ([`updatecli.d/shellcheck.yaml`](../../updatecli.d/shellcheck.yaml), weekly) |
| kind node image | `KIND_NODE_IMAGE` in `e2e-reusable.yml` | **manual** (changes the tested Kubernetes version — a deliberate choice) |
| polaris version + checksum | `POLARIS_VERSION` / `POLARIS_SHA256` in [`security-scan.yml`](../../.github/workflows/security-scan.yml) | **manual** (needs a deliberate v10 migration — see [below](#deliberately-manual-pins)) |
| buildkit builder image | `BUILDKIT_IMAGE` in `e2e-reusable.yml` | **manual** (intentionally a floating tag — see [below](#deliberately-manual-pins)) |

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

| Manifest | Pins | Checksum strategy |
|---|---|---|
| [`kind.yaml`](../../updatecli.d/kind.yaml) | `KIND_VERSION` + `KIND_BINARY_SHA256` | published `.sha256sum` |
| [`calico.yaml`](../../updatecli.d/calico.yaml) | `CALICO_VERSION` in two files | none (version-only) |
| [`shellcheck.yaml`](../../updatecli.d/shellcheck.yaml) | `SHELLCHECK_VERSION` + `SHELLCHECK_SHA256` | hash the tarball |

**Gate tools open PRs that may go red.** shellcheck (and, when migrated, polaris)
are lint/scan gates: a new release can add findings. The bump PR running CI is
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

These are pinned but **not** on the updatecli cadence, each for a concrete reason
rather than because automation is missing (tracked under **Q151** in
[docs/STATUS.md](../STATUS.md)):

- **`POLARIS_VERSION` + `POLARIS_SHA256`** — between the pinned `9.6.0` and the
  current major, upstream changed *both* the tag scheme (`9.6.0` → `v10.x`) and
  the asset name (`polaris_linux_amd64.tar.gz` → `polaris_<version>_linux_amd64.tar.gz`),
  so a blind bump would write a broken download URL. It is also a security gate
  whose verdict shifts across a major. Adopting v10 means migrating the
  [`security-scan.yml`](../../.github/workflows/security-scan.yml) install step to
  the new naming and vetting the new findings — a deliberate change, then a
  manifest like the others.
- **`BUILDKIT_IMAGE`** — `moby/buildkit:buildx-stable-1` is an *intentionally
  floating* stable tag, not a stale pin. "Tracking" it means converting to a
  version/digest pin — a reproducibility/posture change to decide on its own
  merits, not a freshness fix.
- **`KIND_NODE_IMAGE`** — the node image a kind release recommends lives in its
  release notes, not a clean datasource, and bumping it changes the tested
  Kubernetes version (and the Calico compatibility window). Review and bump it by
  hand in the kind updatecli PR when the Kubernetes version should move.
