// docker-bake.hcl — build all six e2e images concurrently with one Buildx
// invocation. Every target is a named stage of the single root Dockerfile,
// selected with `target`; they share its `deps` stage, so the vendored
// dependency tree is compiled once per bake instead of once per image.
//
// Invoke:
//   docker buildx bake                        # build all six (default group)
//   docker buildx bake gmc                    # build just one target
//   GHA_CACHE=true docker buildx bake         # opt into GitHub Actions cache
//
// All targets share the repo-root context and push to the local registry
// stood up by scripts/e2e/kind-with-registry.sh; see
// docs/plan/e2e-ci-speed-round-2.md for the current pipeline description and
// docs/plan/docker-image-speed.md for the earlier round.

variable "GIT_SHA" {
  default = ""
}

// Use the literal IPv4 loopback, not "localhost". The registry container is
// published IPv4-only (-p 127.0.0.1:5000:5000 in scripts/e2e/start-registry.sh), so
// a pusher that resolves "localhost" to the IPv6 [::1] first hits a closed port
// and fails intermittently ("connect: connection refused"). 127.0.0.1 is
// unambiguous. This string is also the image-name prefix the kind nodes'
// containerd mirror is keyed on, so it must stay in sync with the certs.d host
// dir in scripts/e2e/kind-with-registry.sh and the *_IMG refs that pods consume.
variable "IMAGE_REGISTRY" {
  default = "127.0.0.1:5000"
}

// VERSION stamps org.opencontainers.image.version. Defaults empty so the
// _common args fall back to GIT_SHA; set it to a release tag (e.g. v1.0.0)
// when cutting a versioned build.
variable "VERSION" {
  default = ""
}

// GHA_CACHE controls GitHub Actions cache export/import. Empty by default. The
// type=gha backend needs ACTIONS_RUNTIME_TOKEN and ACTIONS_RESULTS_URL, which
// the runner injects into action processes only: docker/setup-buildx-action
// exports no environment of its own, so a `run:` caller must re-export them
// into GITHUB_ENV first — e2e-reusable.yml does that with
// crazy-max/ghaction-github-runtime immediately before the bake. Without them
// buildx drops both entries silently (measured on buildx v0.36.1 / buildkit
// v0.30.0: exit 0, no import or export vertex, no warning), so set this only
// where the runtime is exposed.
variable "GHA_CACHE" {
  default = ""
}

group "default" {
  targets = ["gmc", "agc", "proxy", "fakegithub", "worker", "wrapper"]
}

// _common holds the settings every target inherits. The output `type=registry`
// pushes the resulting image straight to IMAGE_REGISTRY; the local kind nodes
// pull from there on demand (see scripts/e2e/kind-with-registry.sh).
//
// ONE cache scope for all six targets, not one per image. They share the root
// Dockerfile's `deps` stage — a ~1.1 GB warm Go build cache — and a per-image
// scope would export six copies of it, burning the repo's 10 GB Actions-cache
// budget to store the same layer six times. A single scope stores it once and
// every target restores from it.
target "_common" {
  context    = "."
  dockerfile = "Dockerfile"
  output     = ["type=registry"]
  // Provenance for the org.opencontainers.image.* labels each final stage sets.
  // REVISION is the build's git SHA; VERSION falls back to it when no release
  // tag is supplied.
  args = {
    REVISION = GIT_SHA
    VERSION  = VERSION != "" ? VERSION : GIT_SHA
  }
  cache-from = GHA_CACHE != "" ? ["type=gha,scope=images"] : []
  cache-to   = GHA_CACHE != "" ? ["type=gha,mode=max,scope=images"] : []
}

target "gmc" {
  inherits = ["_common"]
  target   = "gmc"
  tags     = ["${IMAGE_REGISTRY}/gmc:e2e-${GIT_SHA}"]
}

target "agc" {
  inherits = ["_common"]
  target   = "agc"
  tags     = ["${IMAGE_REGISTRY}/agc:e2e-${GIT_SHA}"]
}

target "proxy" {
  inherits = ["_common"]
  target   = "proxy"
  tags     = ["${IMAGE_REGISTRY}/proxy:e2e-${GIT_SHA}"]
}

target "fakegithub" {
  inherits = ["_common"]
  target   = "fakegithub"
  tags     = ["${IMAGE_REGISTRY}/fakegithub:e2e-${GIT_SHA}"]
}

target "worker" {
  inherits = ["_common"]
  target   = "worker"
  tags     = ["${IMAGE_REGISTRY}/worker:e2e-${GIT_SHA}"]
}

// wrapper is the ~2 MB scratch image holding just the cmd/worker wrapper binary,
// injected into worker pods at runtime so the runner image can be the unmodified
// upstream actions-runner (Q235). It shares the `build-wrapper` stage with the
// worker target, so the two ship byte-identical binaries from one compile.
target "wrapper" {
  inherits = ["_common"]
  target   = "wrapper"
  tags     = ["${IMAGE_REGISTRY}/wrapper:e2e-${GIT_SHA}"]
}
