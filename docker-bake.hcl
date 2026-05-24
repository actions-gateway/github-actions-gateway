// docker-bake.hcl — build all four e2e images concurrently with one Buildx
// invocation. Replaces four sequential `docker buildx build` calls in the CI
// workflow with one bake step bounded by the slowest target's wall time.
//
// Invoke:
//   docker buildx bake                        # build all four (default group)
//   docker buildx bake gmc                    # build just one target
//   GHA_CACHE=true docker buildx bake         # opt into GitHub Actions cache
//
// All targets share the repo-root context and push to the local registry
// stood up by scripts/kind-with-registry.sh; see docs/plan/docker-image-speed.md
// for the full pipeline description.

variable "GIT_SHA" {
  default = ""
}

variable "IMAGE_REGISTRY" {
  default = "localhost:5000"
}

// GHA_CACHE controls GitHub Actions cache export/import. Empty by default so
// local invocations don't fail with "ActionsRuntimeToken required"; CI sets it
// to "true" after docker/setup-buildx-action has injected the cache env vars.
variable "GHA_CACHE" {
  default = ""
}

group "default" {
  targets = ["gmc", "agc", "proxy", "fakegithub"]
}

// _common holds the settings every target inherits. The output `type=registry`
// pushes the resulting image straight to IMAGE_REGISTRY; the local kind nodes
// pull from there on demand (see scripts/kind-with-registry.sh).
target "_common" {
  context = "."
  output  = ["type=registry"]
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
