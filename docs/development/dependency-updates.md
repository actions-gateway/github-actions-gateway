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
| kind node image | `KIND_NODE_IMAGE` in `e2e-reusable.yml` | **manual** (changes the tested Kubernetes version — a deliberate choice) |
| Calico, polaris, shellcheck, buildkit | version env vars in CI workflows | **manual** today; updatecli fan-out planned (see below) |

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

## The kind manifest (the reference pattern)

[`updatecli.d/kind.yaml`](../../updatecli.d/kind.yaml) is the first and template
manifest:

1. **Source `kindVersion`** — the latest `kubernetes-sigs/kind` release (semver
   filter excludes prereleases).
2. **Source `kindChecksum`** — a shell source that `dependson` `kindVersion` and
   runs [`scripts/updatecli/kind-linux-amd64-sha256.sh`](../../scripts/updatecli/kind-linux-amd64-sha256.sh)
   with the resolved tag, fetching the upstream-published
   `kind-linux-amd64.sha256sum` (the same file a human would copy by hand).
3. **Targets** — regex-replace `KIND_VERSION` and `KIND_BINARY_SHA256` in
   `e2e-reusable.yml`, then open one PR with both.

`KIND_NODE_IMAGE` is **intentionally not** auto-bumped: the node image a kind
release recommends lives in its release notes, not a clean datasource, and
bumping it changes the tested Kubernetes version (and the Calico compatibility
window). Review and bump it by hand in the updatecli PR when the Kubernetes
version should move.

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
  documents). For a kind bump, e2e must run before merge — a maintainer
  re-triggers checks by closing and reopening the PR. A stored App token would
  remove this step; it is deliberately not used yet (one more secret to manage),
  matching the go-sync rationale.
- **Triage cadence.** updatecli is scheduled just after Dependabot so all
  dependency PRs land together and are reviewed in one weekly pass.
- **Manifest rot.** A manifest is bespoke: if an upstream renames a release asset
  or changes its checksum-file layout, the run fails or no-ops. Watch the
  scheduled run's status; a silent green with no PR for a long-stale pin is the
  signal to check the manifest.

## Planned fan-out

The kind manifest is the proof of the pattern. The remaining manual env-var pins
are tracked for migration to their own `updatecli.d/*.yaml` manifests — see the
Queue in [docs/STATUS.md](../STATUS.md):

- `CALICO_VERSION` (version only; also de-duplicates the hand-kept copy in the
  root `Makefile` — one manifest can target both files)
- `BUILDKIT_IMAGE` (floating tag → could pin + track)
- `POLARIS_VERSION` + checksum, `SHELLCHECK_VERSION` + checksum (version+SHA, the
  same shape as kind)
- `KIND_NODE_IMAGE` (only if a reliable "recommended node image per kind release"
  source is found; otherwise stays manual)
