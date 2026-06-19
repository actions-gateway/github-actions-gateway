// docker-bake.hcl — build all five e2e images concurrently with one Buildx
// invocation. Replaces five sequential `docker buildx build` calls in the CI
// workflow with one bake step bounded by the slowest target's wall time.
//
// Invoke:
//   docker buildx bake                        # build all five (default group)
//   docker buildx bake gmc                    # build just one target
//   GHA_CACHE=true docker buildx bake         # opt into GitHub Actions cache
//
// All targets share the repo-root context and push to the local registry
// stood up by scripts/kind-with-registry.sh; see docs/plan/docker-image-speed.md
// for the full pipeline description.

variable "GIT_SHA" {
  default = ""
}

// Use the literal IPv4 loopback, not "localhost". The registry container is
// published IPv4-only (-p 127.0.0.1:5000:5000 in scripts/start-registry.sh), so
// a pusher that resolves "localhost" to the IPv6 [::1] first hits a closed port
// and fails intermittently ("connect: connection refused"). 127.0.0.1 is
// unambiguous. This string is also the image-name prefix the kind nodes'
// containerd mirror is keyed on, so it must stay in sync with the certs.d host
// dir in scripts/kind-with-registry.sh and the *_IMG refs that pods consume.
variable "IMAGE_REGISTRY" {
  default = "127.0.0.1:5000"
}

// VERSION stamps org.opencontainers.image.version. Defaults empty so the
// _common args fall back to GIT_SHA; set it to a release tag (e.g. v1.0.0)
// when cutting a versioned build.
variable "VERSION" {
  default = ""
}

// GHA_CACHE controls GitHub Actions cache export/import. Empty by default so
// local invocations don't fail with "ActionsRuntimeToken required"; CI sets it
// to "true" after docker/setup-buildx-action has injected the cache env vars.
variable "GHA_CACHE" {
  default = ""
}

group "default" {
  targets = ["gmc", "agc", "proxy", "fakegithub", "worker"]
}

// _common holds the settings every target inherits. The output `type=registry`
// pushes the resulting image straight to IMAGE_REGISTRY; the local kind nodes
// pull from there on demand (see scripts/kind-with-registry.sh).
target "_common" {
  context = "."
  output  = ["type=registry"]
  // Provenance for the org.opencontainers.image.* labels each Dockerfile sets.
  // REVISION is the build's git SHA; VERSION falls back to it when no release
  // tag is supplied.
  args = {
    REVISION = GIT_SHA
    VERSION  = VERSION != "" ? VERSION : GIT_SHA
  }
}

target "gmc" {
  inherits   = ["_common"]
  dockerfile = "cmd/gmc/Dockerfile"
  tags       = ["${IMAGE_REGISTRY}/gmc:e2e-${GIT_SHA}"]
  cache-from = GHA_CACHE != "" ? ["type=gha,scope=gmc"] : []
  cache-to   = GHA_CACHE != "" ? ["type=gha,mode=max,scope=gmc"] : []
}

target "agc" {
  inherits   = ["_common"]
  dockerfile = "cmd/agc/Dockerfile"
  tags       = ["${IMAGE_REGISTRY}/agc:e2e-${GIT_SHA}"]
  cache-from = GHA_CACHE != "" ? ["type=gha,scope=agc"] : []
  cache-to   = GHA_CACHE != "" ? ["type=gha,mode=max,scope=agc"] : []
}

target "proxy" {
  inherits   = ["_common"]
  dockerfile = "cmd/proxy/Dockerfile"
  tags       = ["${IMAGE_REGISTRY}/proxy:e2e-${GIT_SHA}"]
  cache-from = GHA_CACHE != "" ? ["type=gha,scope=proxy"] : []
  cache-to   = GHA_CACHE != "" ? ["type=gha,mode=max,scope=proxy"] : []
}

target "fakegithub" {
  inherits   = ["_common"]
  dockerfile = "test/fakegithub/Dockerfile"
  tags       = ["${IMAGE_REGISTRY}/fakegithub:e2e-${GIT_SHA}"]
  cache-from = GHA_CACHE != "" ? ["type=gha,scope=fakegithub"] : []
  cache-to   = GHA_CACHE != "" ? ["type=gha,mode=max,scope=fakegithub"] : []
}

target "worker" {
  inherits   = ["_common"]
  dockerfile = "cmd/worker/Dockerfile"
  tags       = ["${IMAGE_REGISTRY}/worker:e2e-${GIT_SHA}"]
  cache-from = GHA_CACHE != "" ? ["type=gha,scope=worker"] : []
  cache-to   = GHA_CACHE != "" ? ["type=gha,mode=max,scope=worker"] : []
}
