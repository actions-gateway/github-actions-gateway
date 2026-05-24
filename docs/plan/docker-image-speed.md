# Docker Image Speed Improvements

This document analyses where time is spent building and loading the five
Docker images in this repo (`gmc`, `agc`, `proxy`, `worker`, `fakegithub`) and
describes concrete improvements in order of estimated impact. §1–§9 target
the **build** phase; §12–§13 target the **load-into-kind** phase used by the
e2e suite. Each section covers motivation, implementation steps, files
affected, and estimated savings.

Items that were obsoleted by other decisions (§3 and §6 by workspace
vendoring; §10 and §11 by the in-cluster registry in §13) have been removed
from this document — see commit history for the original plans.

## Status

| # | Change | Status |
|---|---|---|
| 1  | Root `.dockerignore` | ✅ Done |
| 2  | Compile-cache mount | ✅ Done (lite — `/root/.cache/go-build` only; module-cache half mooted by vendoring) |
| 4  | Drop `go work sync` no-op | ✅ Done |
| 5  | Parallel CI builds (bake) | ✅ Done |
| 7  | Alpine builder base | ⬜ TODO |
| 8  | Pin bases by digest | ⬜ TODO |
| 9  | Path-based image skip in CI | ⬜ TODO (depends on §5, now satisfied) |
| 12 | Single-node CI cluster | ⬜ TODO (smaller benefit now §13 is in) |
| 13 | In-cluster registry | ✅ Done |

Plus, as an extra-plan change: **Go workspace vendoring** (`go work vendor`
produces a single `vendor/` at the repo root, committed to git). Every
Dockerfile now does `COPY . .` + `go build` with `-mod=vendor` auto-selected,
no module-cache plumbing required. Vendor/ adds ~76 MB to the repo;
`.gitattributes` marks it `linguist-vendored` so it doesn't dominate GitHub UI
summaries.

---

## Background — where time goes today

A cold-cache run of `make e2e-images` builds four images sequentially:

| Phase | Typical duration |
|---|---|
| Send build context (`COPY . .`) ×4 | ~10–25 s |
| Pull `golang:1.26` base (~800 MB) on cold CI runner | ~30–60 s |
| `go mod download` (implicit on first `go build`) ×4 | ~60–120 s |
| `go build` (gmc, agc, fakegithub, proxy) | ~120–180 s |
| **Total (cold)** | **~4–6 min** |
| **Total (warm, current GHA cache hit)** | **~1–2 min** |

CI already enables `cache-from: type=gha` per image in
[.github/workflows/e2e-test.yml](.github/workflows/e2e-test.yml), so warm runs
do reuse layer blobs. The catch: BuildKit invalidates the `go build` layer the
moment any file inside `COPY . .` changes — which is every PR. With no
`--mount=type=cache` for `/go/pkg/mod` or `/root/.cache/go-build`, every
non-cached `go build` recompiles the world.

Local builds (`make e2e-images`) have no layer cache at all and rebuild from
scratch every time.

The improvements below target both paths: shrinking the build context so fewer
edits invalidate layers, and adding BuildKit cache mounts so compilation
artefacts survive cache misses.

---

## 1. Root `.dockerignore`

**Estimated savings: 5–15 s per build × 4 builds; eliminates many spurious
cache invalidations**

### Problem

Docker reads `.dockerignore` from the **context root**, not from the directory
next to the Dockerfile. The only `.dockerignore` in the repo is at
[cmd/gmc/.dockerignore](cmd/gmc/.dockerignore), but every Dockerfile except
proxy builds with `context: .`. That means none of those builds get a
dockerignore applied. (BuildKit also supports `<dockerfile>.dockerignore` next
to the Dockerfile, but the file would have to be named e.g.
`cmd/gmc/Dockerfile.dockerignore` — the current `.dockerignore` filename is not
matched.)

Result: the full repo ships into every build, including:

- `tools/` — 29 MB (vendored `controller-gen`, `kubebuilder`, `setup-envtest`)
- `.git/` — varies, often 50–200 MB
- `docs/` — 672 KB
- `testdata/`, `scripts/`, `broker/`, `internal/` — irrelevant to most images
- All `*_test.go` files — irrelevant to builds; ~hundreds of KB
- `.claude/`, `.build/`, IDE files — never needed

Beyond the wire transfer cost, every change to any of those files invalidates
the `COPY . .` layer and forces a full `go build` rebuild.

### Approach

Add a single root `.dockerignore` that excludes everything by default and
re-includes only Go source, module files, and the workspace files needed for
multi-module builds. Model after the existing
[cmd/gmc/.dockerignore](cmd/gmc/.dockerignore).

### Implementation steps

1. **Create `/.dockerignore`**:

   ```dockerignore
   # Ignore everything by default and re-include only what builds need.
   **

   # Re-include Go source (but not tests).
   !**/*.go
   **/*_test.go

   # Re-include Go module / workspace files.
   !go.mod
   !go.sum
   !go.work
   !go.work.sum
   !**/go.mod
   !**/go.sum

   # Re-include build-time inputs that aren't .go (e.g. embedded assets).
   # Add specific entries here if/when introduced; do not blanket-include
   # directories.
   ```

2. **Delete [cmd/gmc/.dockerignore](cmd/gmc/.dockerignore)** — superseded by
   the root file, and ignored anyway when context is `.`.

3. **Add `cmd/proxy/.dockerignore`** mirroring the root file. The proxy build
   uses `context: cmd/proxy`, so the root `.dockerignore` does not apply to it.
   In particular, exclude `proxy_test.go` so test edits do not invalidate the
   build layer:

   ```dockerignore
   **
   !*.go
   *_test.go
   !go.mod
   !go.sum
   ```

### Files

- `.dockerignore` (new)
- `cmd/proxy/.dockerignore` (new)
- `cmd/gmc/.dockerignore` (delete)

---

## 2. Compile-cache mount on `/root/.cache/go-build` ✓

**Estimated savings: 30–60 s on rebuilds that invalidate the build layer**

### Problem

The `go build` step lives in its own layer; that layer's cache key depends on
every file in `COPY . .`. Any source edit invalidates the layer and forces
Go to recompile every package from scratch — including unchanged dependencies
— because the build cache lived inside the discarded layer.

### Approach

Wrap the `go build` step with a BuildKit cache mount targeting
`/root/.cache/go-build`. The mount is BuildKit-managed scratch space that is
NOT part of the layer output, so it survives across layer-cache misses.
Go's content-addressed build cache then reuses compiled `.a` files for any
package whose inputs haven't changed.

The module-cache half (`/go/pkg/mod`) from the original plan was dropped
because workspace vendoring (committed separately) eliminated the module
cache entirely — `go build` reads from `vendor/` without touching the
module cache at all.

### Implementation steps (shipped)

1. Pinned `# syntax=docker/dockerfile:1.7` at the top of each Dockerfile.
2. Added `--mount=type=cache,target=/root/.cache/go-build` to the `go build`
   step in [cmd/gmc/Dockerfile](cmd/gmc/Dockerfile),
   [cmd/agc/Dockerfile](cmd/agc/Dockerfile),
   [cmd/proxy/Dockerfile](cmd/proxy/Dockerfile),
   and [test/fakegithub/Dockerfile](test/fakegithub/Dockerfile).

### Notes

- The cache mount uses BuildKit-managed storage, separate from `cache-to=gha`.
  In CI it persists *within* a workflow run (so parallel bake targets share
  the cache), but does not survive across runs unless BuildKit state itself
  is cached.
- Inside the bake parallel run, the four targets share the mount with
  `sharing=shared` (the default), so the first build to compile each package
  populates a `.a` that the other three reuse.

### Files

- All four buildable Dockerfiles (`cmd/worker/Dockerfile` already uses
  alpine and isn't part of the e2e bake set, so it's untouched).

---

## 4. Drop the `go work sync` no-op ✓

**Estimated savings: ~1 s × 3 builds; layer-cache hygiene**

### Problem

[cmd/gmc/Dockerfile:11](cmd/gmc/Dockerfile),
[cmd/agc/Dockerfile:10](cmd/agc/Dockerfile), and
[test/fakegithub/Dockerfile:11](test/fakegithub/Dockerfile) all ran
`RUN go work sync 2>/dev/null || true` immediately before `COPY . .`. The next
layer overwrites the working directory, so any side-effect of `go work sync`
was thrown away. It was a no-op step that produced its own layer and added
~1 s.

### Approach

Removed the line. `go build` resolves the workspace correctly without an
explicit sync.

### Files

- `cmd/gmc/Dockerfile`
- `cmd/agc/Dockerfile`
- `test/fakegithub/Dockerfile`

---

## 5. Build CI images in parallel ✓

**Estimated savings: ~2–4 min of wall time on cold-cache CI runs (observed:
~2 min 15 s on the standard e2e job, ~2 min 40 s on multi-node)**

### Problem

The four CI images used to build sequentially inside one job via four
`docker/build-push-action@v6` steps. They have independent GHA cache scopes
(`scope=gmc`, `scope=agc`, etc.) and no inter-image dependencies, so they
could build concurrently and cut the build phase to roughly the slowest
single image.

### Approach (shipped)

`docker buildx bake` with one HCL file describing all four targets. Bake
runs them in parallel using a single BuildKit instance, so the compile-cache
mount from §2 is shared across siblings.

The implementation added a `GHA_CACHE` variable that toggles the
`type=gha` cache-from/cache-to flags so local invocations don't fail with
"ActionsRuntimeToken required" — see [docker-bake.hcl](docker-bake.hcl).

### Files

- [docker-bake.hcl](docker-bake.hcl) (new)
- [Makefile](Makefile) — `e2e-images` and `docker-build-*` targets call bake
- [.github/workflows/e2e-test.yml](.github/workflows/e2e-test.yml) and
  [.github/workflows/e2e-multi-node.yml](.github/workflows/e2e-multi-node.yml)
  — four build-push-action steps collapsed into one bake step

---

## 7. Switch builder base to `golang:1.26-alpine`

**Estimated savings: 15–30 s on cold-cache CI runs (smaller base pull)**

### Problem

gmc, agc, proxy, and fakegithub all use `golang:1.26` (~800 MB). The worker
uses `golang:1.24-alpine` (~250 MB) and builds fine. The Debian-based base is
~500 MB heavier, and that pull happens on every cold CI runner.

### Approach

Switch to `golang:1.26-alpine`. The binaries are already built `CGO_ENABLED=0`
and shipped into `gcr.io/distroless/static:nonroot`, so the builder libc
doesn't matter.

### Implementation steps

1. Change `FROM golang:1.26 AS builder` to `FROM golang:1.26-alpine AS builder`
   in each Dockerfile.

2. **Verify the build still works** — alpine ships with musl and may lack
   tools like `git` that some Go modules need at install time. With vendoring
   in place there's no `go mod download` reaching out for tools, so this is
   probably a no-op, but worth verifying.

### Files

- `cmd/gmc/Dockerfile`
- `cmd/agc/Dockerfile`
- `cmd/proxy/Dockerfile`
- `test/fakegithub/Dockerfile`

---

## 8. Pin base images by digest

**Estimated savings: reproducibility / cache stability, not raw speed**

### Problem

`golang:1.26`, `gcr.io/distroless/static:nonroot`, and
`ghcr.io/actions/runner:2.327.1` (worker base) are referenced by mutable tags.
A registry-side tag move silently busts the layer cache for every downstream
build. The worker Dockerfile flags this already
([cmd/worker/Dockerfile:20-23](cmd/worker/Dockerfile)).

### Approach

Pin each base image to its `@sha256:...` digest. `docker buildx imagetools
inspect <image>` prints the digest; CI dependabot or renovate can keep them
up to date.

### Implementation steps

1. Resolve current digests:

   ```sh
   docker buildx imagetools inspect golang:1.26-alpine
   docker buildx imagetools inspect gcr.io/distroless/static:nonroot
   docker buildx imagetools inspect ghcr.io/actions/runner:2.327.1
   ```

2. Update each Dockerfile's `FROM` lines to `image@sha256:...` form (keep the
   tag as a comment for human readability).

### Files

- All five Dockerfiles

---

## 9. Skip image rebuilds when nothing relevant changed

**Estimated savings: 60–120 s when CI re-runs against an unchanged tree**

### Problem

CI rebuilds all four images on every PR push, even when the change is
docs-only or touches a different module's tests. With a root `.dockerignore`
(§1) the build context excludes most non-build files, but Buildx still spends
time computing the layer hash and probing the cache.

### Approach

Use `dorny/paths-filter` (or equivalent) to detect which Go modules changed
and only invoke the corresponding bake targets. This is a CI-only optimisation
that requires bake (§5) to be in place first.

### Implementation steps

1. **Add a `changes` job** to
   [.github/workflows/e2e-test.yml](.github/workflows/e2e-test.yml) that runs
   `dorny/paths-filter` with filters mapping paths → image targets:

   ```yaml
   - uses: dorny/paths-filter@v3
     id: filter
     with:
       filters: |
         gmc:
           - 'cmd/gmc/**'
           - 'go.work*'
           - 'broker/**'
           - 'githubapp/**'
         agc:
           - 'cmd/agc/**'
           - 'go.work*'
           - 'broker/**'
           - 'githubapp/**'
         proxy:
           - 'cmd/proxy/**'
         fakegithub:
           - 'test/fakegithub/**'
   ```

2. **Conditionally invoke bake** with only the changed targets, falling back
   to all targets when the filter detects a workspace-wide change.

3. **Always run the e2e tests** — only the image build phase is skippable;
   tests must still execute against the cached images.

### Risks

- Cache misses if a previous run for the same branch didn't produce an image
  (e.g. first run). Mitigate by always building if `cache-from` reports no hit.
- False negatives if the path filter doesn't list a dependency. Keep the filter
  permissive (workspace files always trigger all builds).

### Files

- `.github/workflows/e2e-test.yml` — add `changes` job and condition bake step

---

## Background — why `kind load docker-image` was slow

(Historical context for §12 and §13. The kind-load path itself has been
replaced by the in-cluster registry in §13; this section explains why.)

`kind load docker-image IMG --name X` did:

1. **`docker save IMG`** — re-serialise the entire image from the local
   daemon's storage into a tarball. No layer-level dedup across images; the
   distroless base was exported separately for every image.
2. **For each node in the cluster, sequentially**: pipe that tarball into
   `docker exec <node-container> ctr --namespace=k8s.io images import -`.
   Containerd inside the node container re-computed digests and wrote the
   layers into its own content store.

Multiplying by the CI cluster topology (2 nodes: 1 control-plane + 1 worker)
and image sizes (~77+61+17+11 = ~166 MB total): **4 × `docker save`** plus
**8 × `ctr import`**, all serial in one make target.

Three structural CI penalties on top of that:

1. **Slow ephemeral disk.** Every byte traversed the runner's disk multiple
   times: daemon storage → save tarball → pipe → node container filesystem →
   containerd content store.
2. **Single-threaded pipeline.** `docker save | docker exec … ctr import` is
   one pipe; gzip in the save tarball is single-threaded; `ctr import` runs
   digest verification on one CPU per call.
3. **Control-plane was loaded for nothing.** The control-plane node is tainted
   `NoSchedule`, but `kind load docker-image` still pushed every image into
   it, doubling the work in the 2-node config.

§13 (in-cluster registry) replaced all of this. §12 (single-node CI cluster)
remains relevant as a smaller follow-up for cluster-create overhead.

---

## 12. Single-node CI cluster

**Estimated savings: ~30–60 s on cluster create**

### Problem

[test/kind-config-ci.yaml](test/kind-config-ci.yaml) provisions 1 control-plane
+ 1 worker. The control-plane already runs every system addon (cert-manager,
metrics-server, GMC) and the worker only exists to run tenant workloads. A
single-node cluster (control-plane only, with the `NoSchedule` taint removed)
runs everything on one kubelet, eliminating cross-node scheduling and one
node-startup wait during `kind create`.

§13 already removed the per-node image-load penalty that was the bigger
motivator for this change; what remains is just the cluster-create overhead.

### Approach

Switch [test/kind-config-ci.yaml](test/kind-config-ci.yaml) to a single
control-plane node and remove the `NoSchedule` taint via the kind config's
`kubeadmConfigPatches`.

### Implementation steps

1. **Replace [test/kind-config-ci.yaml](test/kind-config-ci.yaml)**:

   ```yaml
   kind: Cluster
   apiVersion: kind.x-k8s.io/v1alpha4
   nodes:
     - role: control-plane
       kubeadmConfigPatches:
         - |
           kind: InitConfiguration
           nodeRegistration:
             taints: []
   ```

   Setting `taints: []` clears the default `node-role.kubernetes.io/control-plane:NoSchedule`
   taint so tenant pods schedule on the control-plane.

2. **Skip-verify the worker-required tests** — the only e2e test that requires
   a distinct worker node is `E2E_GMC_ProxyPodScheduledOnWorker`, already
   tagged `local-only` and excluded from CI. Confirm no other test asserts on
   node count.

3. **Local-dev compatibility**: keep [test/kind-config.yaml](test/kind-config.yaml)
   (3-node) as the local default; only CI switches to the single-node config.

### Risks

- Single-node clusters are denser than 2-node; if CI runner memory becomes
  the bottleneck this could surface as OOMKills. Monitor the first few runs.
- Some Kubernetes default behaviour differs when the control-plane is also a
  worker (e.g. `NoSchedule` removal also affects DaemonSet scheduling, which
  is desirable here but worth verifying).

### Files

- `test/kind-config-ci.yaml`

---

## 13. Replace `kind load docker-image` with an in-cluster registry ✓

**Estimated savings: ~60–120 s on the CI image-load step; replaces the
entire `make e2e-load-images` step**

### Problem

`kind load docker-image` was a serial bytes-over-pipe operation (see Background
above): no layer dedup across images, no reuse of unchanged layers across runs,
full image serialisation on every push. Every CI run re-paid the full ~166 MB
transfer cost.

### Approach (shipped)

Adopted the "kind-with-registry" pattern documented at
<https://kind.sigs.k8s.io/docs/user/local-registry/>: a `registry:2` container
runs alongside the kind cluster on the kind docker network; each node's
containerd is configured to mirror `localhost:5000` → `kind-registry:5000`;
buildx pushes directly to the registry; pods pull on demand.

[scripts/kind-with-registry.sh](scripts/kind-with-registry.sh) handles the
whole setup idempotently. `make e2e-cluster` invokes it; the legacy `kind
load` flow was removed entirely (replaced rather than gated). Image tags now
include `localhost:5000/<name>:e2e-<sha>` so kubelet's `IfNotPresent` cache
can't serve a stale image across cluster reuse.

### Files

- [scripts/kind-with-registry.sh](scripts/kind-with-registry.sh) (new)
- [Makefile](Makefile) — `e2e-cluster` invokes the script;
  `e2e-load-images` removed; image tags include the registry prefix
- [.github/workflows/e2e-test.yml](.github/workflows/e2e-test.yml) and
  [.github/workflows/e2e-multi-node.yml](.github/workflows/e2e-multi-node.yml)
  — drop the `kind load` step; setup-buildx-action uses `network=host` so
  buildx can push to the host registry
- [cmd/gmc/test/e2e/e2e_suite_test.go](cmd/gmc/test/e2e/e2e_suite_test.go) —
  dropped the redundant `LoadImageToKindClusterWithName` loop; flipped the
  fakegithub manifest's `imagePullPolicy: Never` → `IfNotPresent`

---

## Recommended implementation order

| # | Status | Notes |
|---|---|---|
| 1 — root `.dockerignore` | ✅ Done | Highest value-per-minute |
| 4 — drop `go work sync` | ✅ Done | Trivial cleanup |
| 13 — in-cluster registry | ✅ Done | Replaced the `kind load` pipeline entirely |
| Workspace vendoring | ✅ Done | Subsumed §3, §6, and §2's module-cache half |
| 2 — compile-cache mount | ✅ Done | Lite version (build cache only) |
| 5 — parallel buildx bake | ✅ Done | Observed ~2:15 saved on the standard e2e job |
| 7 — alpine builder base | ⬜ TODO | 15 min effort; 15–30 s on cold CI |
| 8 — pin bases by digest | ⬜ TODO | Reproducibility / cache stability |
| 9 — path-based image skip | ⬜ TODO | Now that §5 is in, this is the biggest remaining CI win (60–120 s on path-targeted PRs) |
| 12 — single-node CI cluster | ⬜ TODO | Reduced value after §13 (~30–60 s on `kind create` only) |

The bulk of the planned savings have shipped. What remains: §9 is the biggest
remaining CI improvement (path-based image skip on top of bake); §7 and §8
are 15–30 minute follow-ups; §12 is optional now that image loading isn't
on the critical path.
