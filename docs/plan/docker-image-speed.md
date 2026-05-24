# Docker Image Speed Improvements

This document analyses where time is spent building and loading the five
Docker images in this repo (`gmc`, `agc`, `proxy`, `worker`, `fakegithub`) and
describes concrete improvements in order of estimated impact. The first nine
items target the **build** phase; §10–§13 target the **load-into-kind** phase
used by the e2e suite. Each section covers motivation, implementation steps,
files affected, and estimated savings.

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

## 2. BuildKit cache mounts for Go's module + build cache

**Estimated savings: 30–90 s on cache-miss rebuilds (the common case in CI)**

### Problem

None of the five Dockerfiles use `--mount=type=cache` for `/go/pkg/mod` or
`/root/.cache/go-build`. The Go module cache and build cache live inside the
layer that produced them; the layer is reused only if its inputs are identical.
Any source change invalidates the `go build` layer and forces Go to re-download
modules and recompile every package.

GHA layer caching (already configured) helps only when the layer hash matches
exactly. With a cache mount, the Go caches persist across cache-miss rebuilds,
so an unchanged dependency or unchanged package is reused even when the layer
itself is recomputed.

### Approach

Use BuildKit's `RUN --mount=type=cache,target=...` for both Go cache locations.
Cache mounts are part of the standard Dockerfile frontend; no syntax-version
pragma is needed for the version of BuildKit shipped with current Docker, but
adding a `# syntax=docker/dockerfile:1.7` pin keeps behaviour stable.

### Implementation steps

1. **Add `# syntax=docker/dockerfile:1.7` as the first line of every
   Dockerfile** ([cmd/gmc/Dockerfile](cmd/gmc/Dockerfile),
   [cmd/agc/Dockerfile](cmd/agc/Dockerfile),
   [cmd/proxy/Dockerfile](cmd/proxy/Dockerfile),
   [cmd/worker/Dockerfile](cmd/worker/Dockerfile),
   [test/fakegithub/Dockerfile](test/fakegithub/Dockerfile)).

2. **Wrap each `go build` invocation in cache mounts**. Example for gmc
   ([cmd/gmc/Dockerfile:14](cmd/gmc/Dockerfile)):

   ```dockerfile
   RUN --mount=type=cache,target=/root/.cache/go-build \
       --mount=type=cache,target=/go/pkg/mod \
       CGO_ENABLED=0 go build -C cmd/gmc -o /bin/manager ./cmd/main.go
   ```

   Apply the same pattern to:
   - [cmd/agc/Dockerfile:13](cmd/agc/Dockerfile)
   - [cmd/proxy/Dockerfile:4](cmd/proxy/Dockerfile)
   - [cmd/worker/Dockerfile:13](cmd/worker/Dockerfile)
   - [test/fakegithub/Dockerfile:14](test/fakegithub/Dockerfile)

3. **Also wrap the `go mod download` step** introduced in §3 below in the same
   cache mounts.

### Notes

- The cache mounts use BuildKit-managed storage, separate from `cache-to=gha`.
  In CI, BuildKit creates them fresh per workflow run unless the runner image
  preserves them — they primarily speed up *local* rebuilds and within-build
  reuse across multiple `RUN` steps.
- To make the Go caches survive across CI runs as well, see §6 (single shared
  builder stage) — once dependencies live in their own layer, the GHA layer
  cache will preserve them.

### Files

- All five Dockerfiles

---

## 3. Split `go mod download` into its own layer

**Estimated savings: 60–120 s on PRs that don't change `go.{mod,sum}` (most of
them)**

### Problem

Four of the five Dockerfiles jump from copying `go.mod`/`go.sum` files
straight to `COPY . .` and `go build`. The implicit `go mod download` happens
inside the `go build` step, in the same layer that holds the full source tree.
That layer's hash depends on every file in `COPY . .`, so any source edit
re-runs `go mod download` even though dependencies haven't changed.

The worker Dockerfile already does this correctly
([cmd/worker/Dockerfile:12](cmd/worker/Dockerfile)).

### Approach

After copying `go.mod`/`go.sum`/`go.work*` (and *before* `COPY . .`), add an
explicit `RUN go mod download` step. The layer cache key for that step depends
only on the module manifests, so it is reused on every PR that doesn't touch
dependencies.

### Implementation steps

1. **`cmd/gmc/Dockerfile`** — between line 11 and line 13, insert:

   ```dockerfile
   RUN --mount=type=cache,target=/root/.cache/go-build \
       --mount=type=cache,target=/go/pkg/mod \
       cd cmd/gmc && go mod download
   ```

2. **`cmd/agc/Dockerfile`** — same pattern, `cd cmd/agc && go mod download`.

3. **`test/fakegithub/Dockerfile`** — fakegithub is built from the root module;
   add `go mod download` at the root.

4. **`cmd/proxy/Dockerfile`** — currently the proxy uses
   `context: cmd/proxy` and goes straight to `COPY . . && go build`. Restructure
   to copy `go.mod`/`go.sum` first, run `go mod download`, then `COPY . .`:

   ```dockerfile
   # syntax=docker/dockerfile:1.7
   FROM golang:1.26 AS builder
   WORKDIR /src
   COPY go.mod go.sum ./
   RUN --mount=type=cache,target=/root/.cache/go-build \
       --mount=type=cache,target=/go/pkg/mod \
       go mod download
   COPY . .
   RUN --mount=type=cache,target=/root/.cache/go-build \
       --mount=type=cache,target=/go/pkg/mod \
       CGO_ENABLED=0 go build -o /bin/proxy .

   FROM gcr.io/distroless/static:nonroot
   COPY --from=builder /bin/proxy /proxy
   ENTRYPOINT ["/proxy"]
   ```

5. **`cmd/worker/Dockerfile`** — already correct; just add the cache mount
   wrapping from §2.

### Files

- `cmd/gmc/Dockerfile`
- `cmd/agc/Dockerfile`
- `cmd/proxy/Dockerfile`
- `test/fakegithub/Dockerfile`

---

## 4. Drop the `go work sync` no-op

**Estimated savings: ~1 s × 3 builds; layer-cache hygiene**

### Problem

[cmd/gmc/Dockerfile:11](cmd/gmc/Dockerfile),
[cmd/agc/Dockerfile:10](cmd/agc/Dockerfile), and
[test/fakegithub/Dockerfile:11](test/fakegithub/Dockerfile) all run
`RUN go work sync 2>/dev/null || true` immediately before `COPY . .`. The next
layer overwrites the working directory, so any side-effect of `go work sync`
is thrown away. It's a noop step that produces its own layer and adds time.

### Approach

Remove the line. With cache mounts (§2) and a proper `go mod download` layer
(§3), `go build` resolves the workspace correctly without an explicit sync.

### Files

- `cmd/gmc/Dockerfile`
- `cmd/agc/Dockerfile`
- `test/fakegithub/Dockerfile`

---

## 5. Build CI images in parallel

**Estimated savings: 60–180 s of wall time on cold-cache CI runs**

### Problem

[.github/workflows/e2e-test.yml:32-70](.github/workflows/e2e-test.yml) builds
the four CI images sequentially inside one job. They have independent GHA
cache scopes (`scope=gmc`, `scope=agc`, etc.) and no inter-image dependencies,
so they could build concurrently and cut the build phase to roughly the
slowest single image.

### Approach

Two viable patterns:

1. **Matrix job** — split image builds into a matrix-based `build-images` job
   that produces per-image tarballs as artefacts, with the `e2e` job downloading
   and loading them into kind. Simple to reason about; adds artefact upload
   round-trip.

2. **`docker buildx bake` with `--load`** — one Buildx invocation builds all
   targets in parallel using shared BuildKit. Cleaner inside a single job, but
   requires a `docker-bake.hcl` file and `cache-from`/`cache-to` configuration
   per target.

Recommended: **option 2 (bake)**, because it keeps everything in one job and
shares the BuildKit instance (so cache mounts in §2 are reused across siblings
that share base images).

### Implementation steps

1. **Add `docker-bake.hcl` at the repo root**:

   ```hcl
   variable "GIT_SHA" { default = "" }

   group "default" {
     targets = ["gmc", "agc", "proxy", "fakegithub"]
   }

   target "gmc" {
     context    = "."
     dockerfile = "cmd/gmc/Dockerfile"
     tags       = ["gmc:e2e-${GIT_SHA}"]
     cache-from = ["type=gha,scope=gmc"]
     cache-to   = ["type=gha,mode=max,scope=gmc"]
     output     = ["type=docker"]
   }

   target "agc" {
     context    = "."
     dockerfile = "cmd/agc/Dockerfile"
     tags       = ["agc:e2e"]
     cache-from = ["type=gha,scope=agc"]
     cache-to   = ["type=gha,mode=max,scope=agc"]
     output     = ["type=docker"]
   }

   target "proxy" {
     context    = "cmd/proxy"
     dockerfile = "Dockerfile"
     tags       = ["proxy:e2e"]
     cache-from = ["type=gha,scope=proxy"]
     cache-to   = ["type=gha,mode=max,scope=proxy"]
     output     = ["type=docker"]
   }

   target "fakegithub" {
     context    = "."
     dockerfile = "test/fakegithub/Dockerfile"
     tags       = ["fakegithub:e2e"]
     cache-from = ["type=gha,scope=fakegithub"]
     cache-to   = ["type=gha,mode=max,scope=fakegithub"]
     output     = ["type=docker"]
   }
   ```

2. **Add a `docker-bake` Makefile target**:

   ```makefile
   .PHONY: docker-bake
   docker-bake: ## Build all four e2e images in parallel via docker buildx bake
   	GIT_SHA=$(GIT_SHA) docker buildx bake --file docker-bake.hcl
   ```

3. **Replace the four per-image build steps** in
   [.github/workflows/e2e-test.yml](.github/workflows/e2e-test.yml) with a
   single bake step:

   ```yaml
   - name: Build all e2e images in parallel
     run: docker buildx bake --file docker-bake.hcl
     env:
       GIT_SHA: ${{ github.sha }}
   ```

4. **Keep `make e2e-images`** as the local-dev path (it stays sequential,
   which is fine without GHA cache plumbing). Add a comment pointing at the
   bake target for parallel local builds.

### Files

- `docker-bake.hcl` (new)
- `Makefile` — add `docker-bake` target
- `.github/workflows/e2e-test.yml` — replace four build steps with one bake step

---

## 6. Single shared builder stage across gmc/agc/fakegithub

**Estimated savings: ~30–60 s on cold-cache CI builds; cleaner cache scoping**

### Problem

`gmc`, `agc`, and `fakegithub` all build from the same Go workspace and share
the same dependency set (controller-runtime, k8s.io/api, etc.). Each Dockerfile
runs its own `go mod download` and `go build`, downloading the same modules
three times.

### Approach

Define one shared `builder-base` stage in a top-level Dockerfile (or bake
target) that performs `COPY` of workspace + `go mod download` once. The three
final images each derive from `builder-base`, run their own `go build`, and
copy the resulting binary into a distroless image.

This dovetails with `docker buildx bake` (§5): the shared base becomes a bake
target referenced by `contexts = { base = "target:builder-base" }`.

### Implementation steps

1. **Add a `Dockerfile.base` at the repo root**:

   ```dockerfile
   # syntax=docker/dockerfile:1.7
   FROM golang:1.26 AS builder-base
   WORKDIR /src
   COPY go.work go.work.sum go.mod go.sum ./
   COPY cmd/agc/go.mod cmd/agc/go.sum cmd/agc/
   COPY cmd/gmc/go.mod cmd/gmc/go.sum cmd/gmc/
   COPY cmd/proxy/go.mod cmd/proxy/go.sum cmd/proxy/
   COPY cmd/worker/go.mod cmd/worker/
   COPY cmd/probe/go.mod cmd/probe/
   RUN --mount=type=cache,target=/root/.cache/go-build \
       --mount=type=cache,target=/go/pkg/mod \
       go mod download
   COPY . .
   ```

2. **Rewrite gmc/agc/fakegithub Dockerfiles** to start
   `FROM builder-base AS builder` and skip the COPY/go-mod-download dance.
   Example for `cmd/gmc/Dockerfile`:

   ```dockerfile
   # syntax=docker/dockerfile:1.7
   FROM builder-base AS builder
   RUN --mount=type=cache,target=/root/.cache/go-build \
       --mount=type=cache,target=/go/pkg/mod \
       CGO_ENABLED=0 go build -C cmd/gmc -o /bin/manager ./cmd/main.go

   FROM gcr.io/distroless/static:nonroot
   COPY --from=builder /bin/manager /manager
   ENTRYPOINT ["/manager"]
   ```

3. **Wire the shared base into `docker-bake.hcl`** via `contexts`:

   ```hcl
   target "builder-base" {
     context    = "."
     dockerfile = "Dockerfile.base"
     output     = ["type=cacheonly"]
     cache-from = ["type=gha,scope=builder-base"]
     cache-to   = ["type=gha,mode=max,scope=builder-base"]
   }

   target "gmc" {
     contexts = { builder-base = "target:builder-base" }
     # ...
   }
   ```

4. **Decide on proxy and worker**: proxy is a standalone module; keep it
   separate. Worker uses a different base image (`golang:1.24-alpine`) and
   only needs a subset of the workspace; leave it as-is or migrate it to
   `builder-base` in a follow-up.

### Files

- `Dockerfile.base` (new)
- `cmd/gmc/Dockerfile`, `cmd/agc/Dockerfile`, `test/fakegithub/Dockerfile` —
  rewrite to use shared base
- `docker-bake.hcl` — add `builder-base` target

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
   (or to `FROM builder-base` if §6 lands first).

2. **Verify the build still works** — alpine ships with musl and may lack
   tools like `git` that some Go modules need at install time. If
   `go mod download` fails with missing tools, prepend
   `RUN apk add --no-cache git ca-certificates`.

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

## Background — why `kind load docker-image` is slow

Items §10–§13 all target the `make e2e-load-images` step
([Makefile:94-97](Makefile)). Understanding the underlying mechanism makes the
trade-offs of each fix clearer.

For each `kind load docker-image IMG --name X`, kind does:

1. **`docker save IMG`** — re-serialises the entire image from the local
   daemon's storage into a tarball. There is no layer-level dedup across
   images; the distroless base is exported separately for every image.
2. **For each node in the cluster, sequentially**: pipe that tarball into
   `docker exec <node-container> ctr --namespace=k8s.io images import -`.
   Containerd inside the node container re-computes digests and writes the
   layers into its own content store.

Multiplying by the CI cluster topology (2 nodes: 1 control-plane + 1 worker)
and the current image sizes from `docker images`:

| image | size |
|---|---|
| gmc | ~77 MB |
| agc | ~61 MB |
| proxy | ~17 MB |
| fakegithub | ~11 MB |
| **total** | **~166 MB** |

…produces **4 × `docker save`** (~166 MB serialised) plus **8 × `ctr import`**,
all serial in one make target.

Three structural CI penalties on top of that:

1. **Slow ephemeral disk.** Every byte traverses the runner's disk multiple
   times: daemon storage → save tarball → pipe → node container filesystem →
   containerd content store. Standard GitHub-hosted runners sustain ~150–250
   MB/s, and throughput drops further when reads and writes contend.
2. **Single-threaded pipeline.** `docker save | docker exec … ctr import` is
   one pipe; gzip in the save tarball is single-threaded; `ctr import` runs
   digest verification on one CPU per call.
3. **Control-plane is loaded for nothing.** The control-plane node is tainted
   `NoSchedule` by default — workloads will never run there — but
   `kind load docker-image` still pushes every image into it, doubling the
   work in the 2-node config.

§10–§13 attack these in order: cut wasted per-node work (§10, §12), parallelise
the remaining work (§11), and ultimately replace the save/pipe path with a
proper registry (§13).

---

## 10. Skip the control-plane node when loading images

**Estimated savings: ~40–80 s on the CI image-load step (≈50%)**

### Problem

`kind load docker-image IMG --name X` pushes every image to every node in the
cluster. The control-plane node is tainted `NoSchedule` by default in the kind
config used by CI ([test/kind-config-ci.yaml](test/kind-config-ci.yaml)), so
no workload ever runs there — but it still receives a full copy of every
image. In a 2-node cluster that doubles the load work for zero benefit.

### Approach

Pass `--nodes` to restrict loading to the worker node. kind exposes node
names as `<cluster-name>-control-plane` and `<cluster-name>-worker` (or
`-worker2`, `-worker3` for multi-worker clusters), so the worker name is
deterministic.

### Implementation steps

1. **Update [Makefile:94-97](Makefile)** to target the worker node explicitly:

   ```makefile
   .PHONY: e2e-load-images
   e2e-load-images: e2e-images
   	kind load docker-image $(GMC_IMG)        --name $(KIND_CLUSTER) --nodes $(KIND_CLUSTER)-worker
   	kind load docker-image $(AGC_IMG)        --name $(KIND_CLUSTER) --nodes $(KIND_CLUSTER)-worker
   	kind load docker-image $(PROXY_IMG)      --name $(KIND_CLUSTER) --nodes $(KIND_CLUSTER)-worker
   	kind load docker-image $(FAKEGITHUB_IMG) --name $(KIND_CLUSTER) --nodes $(KIND_CLUSTER)-worker
   ```

2. **Multi-node suite**: [.github/workflows/e2e-multi-node.yml](.github/workflows/e2e-multi-node.yml)
   uses a 3-worker cluster, so the `--nodes` flag must list every worker:
   `--nodes $(KIND_CLUSTER)-worker,$(KIND_CLUSTER)-worker2,$(KIND_CLUSTER)-worker3`.
   Either parameterise via a `KIND_WORKER_NODES` make variable that the
   workflow overrides, or duplicate the recipe in the multi-node target.

3. **Sanity check**: confirm that no test pod sets a toleration that would let
   it schedule on the control-plane. A quick grep for `node-role.kubernetes.io/control-plane`
   tolerations in [cmd/gmc/test/e2e](cmd/gmc/test/e2e) catches this.

### Files

- `Makefile` — `e2e-load-images` target
- `.github/workflows/e2e-multi-node.yml` — pass worker list for 3-worker config

---

## 11. Load all four images in parallel

**Estimated savings: ~30–60 s on the CI image-load step**

### Problem

[Makefile:94-97](Makefile) runs the four `kind load` invocations serially. They
contend on the same disk and the same node container, so parallelism is not
linear — but the `docker save` phase of one image overlaps with the `ctr import`
phase of another, and `docker save` on a different image can fan across CPUs.

### Approach

Background each `kind load` and `wait`. This composes with §10 (worker-only)
so the saved bytes-per-node halves first and parallelism amortises the rest.

### Implementation steps

1. **Update the `e2e-load-images` target** to background each load:

   ```makefile
   .PHONY: e2e-load-images
   e2e-load-images: e2e-images
   	@set -e; pids=""; \
   	kind load docker-image $(GMC_IMG)        --name $(KIND_CLUSTER) --nodes $(KIND_CLUSTER)-worker & pids="$$pids $$!"; \
   	kind load docker-image $(AGC_IMG)        --name $(KIND_CLUSTER) --nodes $(KIND_CLUSTER)-worker & pids="$$pids $$!"; \
   	kind load docker-image $(PROXY_IMG)      --name $(KIND_CLUSTER) --nodes $(KIND_CLUSTER)-worker & pids="$$pids $$!"; \
   	kind load docker-image $(FAKEGITHUB_IMG) --name $(KIND_CLUSTER) --nodes $(KIND_CLUSTER)-worker & pids="$$pids $$!"; \
   	for p in $$pids; do wait $$p; done
   ```

   The explicit `pids`/`wait` loop ensures the recipe fails if any single load
   fails (a bare `wait` exits 0 even if a background job failed).

### Risks

- All four loads compete for the same kind node container's filesystem; the
  speedup is sub-linear (~30–50%) rather than the theoretical 4×.
- Memory pressure on small runners: `docker save` buffers can briefly hold
  tens of MB each. Not an issue on the 7 GB standard GitHub runner.

### Files

- `Makefile` — `e2e-load-images` target

---

## 12. Single-node CI cluster

**Estimated savings: ~30–60 s on cluster create + further halves the load step
when combined with §10**

### Problem

[test/kind-config-ci.yaml](test/kind-config-ci.yaml) provisions 1 control-plane
+ 1 worker. The control-plane already runs every system addon (cert-manager,
metrics-server, GMC) and the worker only exists to run tenant workloads. A
single-node cluster (control-plane only, with the `NoSchedule` taint removed)
runs everything on one kubelet, eliminating cross-node scheduling, image
load doubling, and one node-startup wait during `kind create`.

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

2. **Update §10's `--nodes` flag** to use the control-plane name:
   `--nodes $(KIND_CLUSTER)-control-plane`. Either parameterise via a
   `KIND_LOAD_NODES` make variable, or detect the topology at runtime.

3. **Skip-verify the worker-required tests** — the only e2e test that requires
   a distinct worker node is `E2E_GMC_ProxyPodScheduledOnWorker`, already
   tagged `local-only` and excluded from CI. Confirm no other test asserts on
   node count.

4. **Local-dev compatibility**: keep [test/kind-config.yaml](test/kind-config.yaml)
   (3-node) as the local default; only CI switches to the single-node config.

### Risks

- Single-node clusters are denser than 2-node; if CI runner memory becomes
  the bottleneck this could surface as OOMKills. Monitor the first few runs.
- Some Kubernetes default behaviour differs when the control-plane is also a
  worker (e.g. `NoSchedule` removal also affects DaemonSet scheduling, which
  is desirable here but worth verifying).

### Files

- `test/kind-config-ci.yaml`
- `Makefile` — adjust `--nodes` value to `-control-plane` (interacts with §10)

---

## 13. Replace `kind load docker-image` with an in-cluster registry

**Estimated savings: ~60–120 s on the CI image-load step in the steady state;
larger as image count grows**

### Problem

`kind load docker-image` is fundamentally a serial bytes-over-pipe operation
(see Background above). It cannot dedup layers across images, cannot reuse
unchanged layers across runs, and serialises the whole image even when only
the binary layer changed. Every CI run re-pays the full ~166 MB transfer cost.

A proper container registry running alongside the kind cluster fixes all of
these structurally:

- **Layer dedup**: the distroless base is stored once, referenced by digest
  from every image.
- **Incremental transfer**: only changed layers move from buildx → registry.
- **Parallel pulls**: kind nodes pull layers concurrently from the registry,
  not serially via `docker save` pipes.
- **Cacheable**: the registry container's storage volume can persist across
  workflow steps (and, with some effort, across runs via GHA cache).

### Approach

Adopt the "kind-with-registry" pattern documented at
<https://kind.sigs.k8s.io/docs/user/local-registry/>:

1. Run a `registry:2` container on the same Docker network as the kind cluster.
2. Configure kind's containerd to treat `localhost:5000` as a mirror.
3. Build images with `docker buildx build --push` directly to that registry.
4. Cluster pods reference `localhost:5000/<image>:<tag>` (or any hostname
   alias) and containerd pulls on demand.

This pairs well with §5 (`docker buildx bake`): a single `bake --push` step
replaces the build + load phases entirely.

### Implementation steps

1. **Add `scripts/kind-with-registry.sh`** based on the upstream recipe:

   ```sh
   #!/usr/bin/env bash
   set -euo pipefail

   reg_name='kind-registry'
   reg_port='5000'

   # Start a local registry if not already running.
   if [ "$(docker inspect -f '{{.State.Running}}' "${reg_name}" 2>/dev/null || true)" != 'true' ]; then
     docker run -d --restart=always -p "127.0.0.1:${reg_port}:5000" \
       --network bridge --name "${reg_name}" registry:2
   fi

   # Create the cluster with containerd configured to use the registry as a mirror.
   cat <<EOF | kind create cluster --name "${KIND_CLUSTER}" --config=-
   kind: Cluster
   apiVersion: kind.x-k8s.io/v1alpha4
   containerdConfigPatches:
     - |-
       [plugins."io.containerd.grpc.v1.cri".registry]
         config_path = "/etc/containerd/certs.d"
   nodes:
     - role: control-plane
       kubeadmConfigPatches:
         - |
           kind: InitConfiguration
           nodeRegistration:
             taints: []
   EOF

   # Wire the registry into every node's containerd.
   REGISTRY_DIR="/etc/containerd/certs.d/localhost:${reg_port}"
   for node in $(kind get nodes --name "${KIND_CLUSTER}"); do
     docker exec "${node}" mkdir -p "${REGISTRY_DIR}"
     cat <<EOF | docker exec -i "${node}" cp /dev/stdin "${REGISTRY_DIR}/hosts.toml"
   [host."http://${reg_name}:5000"]
   EOF
   done

   # Connect the registry to the kind network so node containers can reach it.
   if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' "${reg_name}")" = 'null' ]; then
     docker network connect kind "${reg_name}"
   fi

   # Publish the registry endpoint so tools running in-cluster can discover it.
   cat <<EOF | kubectl apply -f -
   apiVersion: v1
   kind: ConfigMap
   metadata:
     name: local-registry-hosting
     namespace: kube-public
   data:
     localRegistryHosting.v1: |
       host: "localhost:${reg_port}"
       help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
   EOF
   ```

2. **Update [Makefile](Makefile)** so `e2e-cluster` calls the script when a
   `USE_LOCAL_REGISTRY=1` toggle is set, and so image tags use
   `localhost:5000/<name>` for the registry path. Keep the legacy
   `kind load` flow as the default until the registry path is validated.

3. **Update [docker-bake.hcl](docker-bake.hcl)** (introduced in §5) so each
   target writes to `localhost:5000/<image>:<tag>` and has
   `output = ["type=registry"]`. The image references inside e2e tests stay
   the same once the test helpers honour a `LOCAL_REGISTRY` prefix env var.

4. **Drop `e2e-load-images` entirely** when the registry path is enabled — the
   cluster pulls on demand. This also removes one Makefile target's CI step,
   shrinking workflow time.

5. **CI cache**: the registry's storage volume can be persisted across
   workflow steps via `actions/cache@v4` keyed on `go.sum` + Dockerfile
   hashes. This further shrinks cache-hit transfers, but it is optional and
   can be deferred.

### Risks

- More moving parts: a separate container that has to start before the cluster
  and stay reachable from the kind network.
- Test helpers and Kubernetes manifests must reference the registry prefix.
  This is a one-time refactor but touches every place an image is named.
- Local-dev experience changes: developers running `make e2e-up` need the
  registry container too (the script handles it idempotently).

### Files

- `scripts/kind-with-registry.sh` (new)
- `Makefile` — gate `e2e-cluster` on `USE_LOCAL_REGISTRY`; remove
  `e2e-load-images` when enabled; rewrite image tags to include the registry
  prefix
- `docker-bake.hcl` — change `output` to `type=registry`
- `cmd/gmc/test/utils/*.go` — honour a `LOCAL_REGISTRY` prefix when resolving
  image names
- Helm/manifest templates under `cmd/gmc/config/` — same

---

## Recommended implementation order

| # | Change | Effort | Savings | Notes |
|---|---|---|---|---|
| 1 | Root `.dockerignore` | 15 min | 5–15 s/build; major cache stability | Highest value-per-minute |
| 4 | Drop `go work sync` no-op | 5 min | ~1 s/build × 3 | Trivial cleanup |
| 2 | BuildKit cache mounts | 30 min | 30–90 s on local rebuilds | Local-dev win primarily |
| 3 | Split `go mod download` layer | 30 min | 60–120 s on PRs without dep changes | Compounds with §1 |
| 10 | Skip control-plane when loading | 10 min | ~40–80 s on CI load step | Trivial, biggest load-step win |
| 11 | Parallel `kind load` | 15 min | ~30–60 s on CI load step | Composes with §10 |
| 12 | Single-node CI cluster | 30 min | ~30–60 s create + halves load again | Verify no worker-required tests |
| 7 | Switch to `golang:1.26-alpine` | 15 min | 15–30 s on cold CI | Verify build still works |
| 5 | Parallel CI builds (bake) | 2 hours | 60–180 s on cold CI | Largest CI build-phase win |
| 6 | Shared `builder-base` stage | 3 hours | 30–60 s; cache scoping | Requires §5 |
| 13 | In-cluster registry | 4–6 hours | 60–120 s on CI load step | Replaces §10–§11; pairs with §5 |
| 9 | Path-based image skip in CI | 2 hours | 60–120 s when unchanged | Requires §5 |
| 8 | Pin bases by digest | 30 min | reproducibility | Renovate/dependabot follow-up |

The first four items are local-only Dockerfile edits that deliver most of the
per-build savings. §10–§12 are small CI-side wins that collectively cut the
image-load step roughly 3–4× with very little risk. §5–§6 are the larger CI
restructuring that unlocks the rest of the build-phase wall-time wins. §13 is
the proper long-term answer to image loading — adopt it once the
quick-and-cheap §10–§12 wins are in and image load is still a hot path.
