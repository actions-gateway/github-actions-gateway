# Worker wrapper injection (Q235)

> **Status:** ▶ Started. Tracks [Q235](../STATUS.md#Q235).

## Problem

GAG worker pods need GAG's `cmd/worker` **wrapper** as the container entrypoint:
it reads the job payload from `PAYLOAD_SECRET_PATH`, materializes the runner
config from the `jitconfig`, and spawns `Runner.Worker` over anonymous pipes
(see [cmd/worker/main.go](../../cmd/worker/main.go)). Today the wrapper is baked
into a first-party image (`ghcr.io/actions-gateway/worker` = upstream
`actions-runner` + the wrapper as `ENTRYPOINT`), and `names.DefaultWorkerImage`
is the **bare upstream** `actions-runner`, which has no wrapper. Consequences:

- A **default** install (no per-tenant `workerImage`) provisions worker pods that
  silently no-op every job — the pod exits `Completed` with empty logs and the
  AGC's `RenewJob` 401s. Tests never caught it: the e2e suite always passes the
  wrapper image explicitly. Found live in [Q224](../STATUS.md).
- An **ARC migrator's** custom/slim image (`FROM actions-runner` + tooling) is not
  drop-in: it has `Runner.Worker` but the stock entrypoint, so it must be rebuilt
  with the wrapper layered on.

## Goal

Inject the wrapper into the worker pod at runtime so the **runner container is the
upstream (or any tenant) image, unmodified**. A default install runs jobs against
upstream `actions-runner`; any `actions/runner`-derived image works without a
rebuild (ARC bring-your-image parity). GAG hosts only a ~2 MB wrapper artifact;
the 518 MB runner is the upstream image (GitHub-hosted, widely mirrored).

## Design

### Worker pod shape

The provisioner ([provisioner.go](../../cmd/agc/internal/provisioner/provisioner.go))
builds every worker pod with:

- **runner container** = `resolveWorkerImage(spec)` (default upstream, or per-tenant
  `workerImage`), with:
  - `command: ["/<wrapperdir>/wrapper"]` — overrides the image entrypoint.
  - env `RUNNER_HOME_DIR` (default `/home/runner`) and `PATH=/home/runner/bin:$PATH`
    so the wrapper's `exec.LookPath("Runner.Worker")` resolves in the upstream image.
  - the existing job-payload Secret mount + proxy-CA mount (unchanged).
- **wrapper delivery** (auto-selected, see below):
  - **OCI image volume** (K8s ≥ 1.33): an `image:` `VolumeSource` mounting the
    wrapper image read-only at `/<wrapperdir>`. No init container, no copy — lowest
    per-job latency.
  - **initContainer fallback** (< 1.33): a tiny `emptyDir` + an init container from
    the wrapper image that `cp`s `/wrapper` into it.

`applySecurityDefaults` already hardens `spec.InitContainers` and gap-fills
pod-level `runAsNonRoot` + `runAsUser: 1001` + seccomp, so the scratch wrapper init
runs non-root as 1001 and the shared `emptyDir` is 1001↔1001. The image volume is
read-only and PSA-compatible by KEP-4639 design (verify under `restricted` in e2e).

### Delivery selection

The AGC gains a discovery client (from the controller-runtime `rest.Config`) and
queries `ServerVersion()` once at startup. Selection precedence:

1. `WRAPPER_DELIVERY` env (`auto` | `imagevolume` | `init`), default `auto`.
2. `auto` → image volume when server minor ≥ 33 (the `ImageVolume` feature gate is
   beta/default-on there), else init container.

Image volumes being unavailable on a ≥1.33 cluster with the gate explicitly
disabled is handled by the `WRAPPER_DELIVERY=init` override (documented). A
runtime probe-and-fallback is out of scope for v1.

### Wrapper artifact

- New build target: `cmd/worker` packaged as a **`FROM scratch`** image
  `ghcr.io/actions-gateway/wrapper` (~2 MB, just the static binary). Added to
  `publish.yml` (build + cosign sign + SBOM + provenance), digest-pinned the same
  way as the other images.
- `names.DefaultWorkerImage` **stays** the upstream `actions-runner` digest — the
  wrapper is now injected, so the default runs jobs.
- The existing 518 MB `ghcr.io/actions-gateway/worker` image is **kept for now** as
  an optional batteries-included image; retiring it is a follow-up (Phase 2).

### Chart / GMC wiring

- New `wrapper:` image block in [values.yaml](../../charts/actions-gateway/values.yaml)
  (`repository`/`tag`/`digest`), digest-pinned like `agc`/`proxy`.
- [deployment.yaml](../../charts/actions-gateway/templates/deployment.yaml) sets
  `WRAPPER_IMAGE` on the GMC via the `actions-gateway.image` helper.
- The GMC propagates `WRAPPER_IMAGE` (+ optional `WRAPPER_DELIVERY`) into each AGC
  Deployment it provisions; the AGC reads them and the provisioner uses
  `WRAPPER_IMAGE` as the init/volume source.

## Backward compatibility

- Existing `workerImage` pins — **including the old full `worker` image** — keep
  working: the command override runs the injected wrapper, which finds
  `Runner.Worker` in any `actions/runner`-derived image.
- DinD is unchanged: the wrapper replaces the entrypoint, so DinD stays a
  **sidecar** + `securityProfile: privileged` (not the `actions-runner-dind`
  bundled `dockerd`). Already documented in `migration-from-arc.md`.

## Test plan

- **Unit** ([provisioner](../../cmd/agc/internal/provisioner)): image-volume path,
  init-container path, command override, `RUNNER_HOME_DIR`/`PATH` env, init-container
  hardening under `restricted`, and `WRAPPER_DELIVERY` selection (incl. version gate).
- **E2e** (kind): a case that sets `WORKER_IMG` to the **bare upstream**
  `actions-runner` (not the wrapper image) and asserts a job runs end-to-end —
  the regression the unit tests can't cover. Run on both the ≥1.33 (image-volume)
  and an init-fallback path.
- **Live**: dogfood re-validate with `DefaultWorkerImage` = upstream + injection
  (the [Q224](../STATUS.md) path), worker image **unset**.

## Rollout

1. **This plan:** wrapper image + injection + default-on + tests + docs.
2. **Phase 2 (separate item):** retire the 518 MB `worker` image; flip the
   `migration-from-arc.md` story to "point `workerImage` at your existing ARC image,
   no rebuild."

## Docs to update

`tenant-onboarding.md` (default now runs on upstream; injection explained),
`migration-from-arc.md` (drop-in once shipped), `release.md` (new `wrapper` image +
the `DefaultWorkerImage` note), `values.yaml` comments.
