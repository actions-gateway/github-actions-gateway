# Q988: read the worker runner version from the registry

> **Status: done 2026-09-07.** Shipped in one PR; the two residuals are [Q1065](../queue/Q1065.md) and [Q1066](../queue/Q1066.md).

## Goal

Make `RunnerVersionTooOld` verdict-bearing for a custom or digest-only worker image by reading the runner version out of the image in the registry, before any pod runs it (Q988; the row closes with this plan).

## What the measurements changed

The row offered two variants and left one fact unverified.
Measured 2026-09-07 against `ghcr.io/actions/actions-runner:2.335.1` (linux/amd64, config blob `sha256:9660a299…`):

- **The config label is the base image's, not the runner's.** `org.opencontainers.image.version` reads `24.04`, inherited from `ubuntu:24.04`; the other labels are `licenses=MIT` and `source=https://github.com/actions/runner`.
  The `history` carries no version either: the runner lands by `COPY --chown=runner:docker /actions-runner .` from a build stage, so no `RUN` line names the tarball.
  The "one GET" variant would misreport the OS version as the runner version, so it is dropped rather than offered as a fallback.
- **Only the layer content holds the version**, in `home/runner/bin/Runner.Listener.deps.json`, the same file the wrapper reads (Q792).
  Scanning layers last-to-first (overlay order), the two layers above the runner had to be streamed whole (88 MB + 23 MB compressed) and the runner layer gave the file up after 3 MB as its 31st member, so one inspection reads about 114 MB of the 544 MB image.
  That is once per digest per AGC process.
- **Reach is the AGC's egress policy's, not the feature's.** By default `buildAGCNetworkPolicy` admits 443 with no `to:` restriction — the breadth [05-security.md](../design/05-security.md#github-app-key-exfiltration-via-agc-apiserver-egress) records as the deliberate default — so any registry is reachable; an install that scopes it with `apiServerCIDRs` (Q145) closes the registry too, and `githubEgressFQDNs` lists no registry host, so an FQDN allowlist would not reopen even `ghcr.io` (whose address, `172.182.252.136` on 2026-09-07, sits in the `actions` and `packages` ranges of `api.github.com/meta`).
  An unreachable registry therefore has to be an ordinary outcome, not an incident: the tag verdict stands and the message says why.
  Whether a scoped policy should admit a registry is [Q1065](../queue/Q1065.md).
- **Credentials are the pod template's.** The AGC may `get` Secrets in its namespace and nothing else that could carry a pull credential: the tenant Role grants no read on ServiceAccounts, so the worker SA's `imagePullSecrets` are out of reach, and a node-identity registry (GKE/GAR, ECR) has no Secret at all.
  `podTemplate.spec.imagePullSecrets` is what the AGC can use, and what kubelet would use first.

## Design

- **`cmd/agc/internal/runnerimage`**, a stdlib-only OCI distribution client: reference parsing, the `WWW-Authenticate` Bearer/Basic dance with anonymous fallback, manifest and index fetch with `linux/amd64` preferred, and a last-to-first layer scan for `*/bin/Runner.Listener.deps.json` that honours whiteouts, stops at the first hit, and is bounded in bytes and time.
  No `go-containerregistry`: the surface needed is four requests, and the vendored tree would be far larger than the feature.
- **An async resolver** in the same package: digest-keyed cache, tag-to-digest resolution on a TTL, one in-flight inspection per key, exponential backoff on failure, and a wake callback the reconcilers route to their existing `wakeCh`.
  A reconcile never blocks on the registry.
- **Verdict merge in `runnercore`.** The tag reading is the immediate answer, exactly as today.
  A completed registry reading overrides it, and the message names the digest it was read at and any disagreement with the tag.
  A pending or failed reading leaves the tag verdict standing with the reason appended, so a reference with a runner-version tag never regresses to `Unknown` because a registry was unreachable.
- **Reasons unchanged.** `WorkerImageBelowMinimum`, `WorkerImageCurrent` and `WorkerImageVersionUnknown` keep their tiers; the message carries the source.
  No CRD field is added: the version is in the condition message, and `observedRunnerVersion` keeps its self-report role.

## Scope

| Piece | Status |
|---|---|
| `runnerimage` client, inspection, credentials | ✅ stdlib only; tests against an in-process TLS registry |
| Async resolver with cache, backoff, wake | ✅ one inspection in flight, digest cached for the process, tag re-read hourly, backoff 1 min to 1 h |
| `runnercore` verdict merge | ✅ nil lookup is byte-for-byte the Q715 verdict; `TestNilLookupIsTagVerdict` pins it |
| Both reconcilers pass the reading; `main.go` wiring | ✅ transport cloned after the trust pool, secrets through the uncached reader |
| Unit tests against an in-process registry; envtest proof the async result reaches status | ✅ three inversions red in the unit tier; the envtest holds the layer and requires the verdict inside a window only the wake explains |
| Operator docs, design appendix, API godoc | ✅ troubleshooting, tenant-onboarding, upgrade, features, 03, 05, appendix-h, `api.md` |
| Follow-up rows | ✅ [Q1065](../queue/Q1065.md), [Q1066](../queue/Q1066.md) |

## What the tests proved, and how

Each mechanism was deleted and its test required to go red before the tree was restored from the index: scanning the layers first-to-last reddened the topmost-layer and whiteout tests, disabling the registry override reddened every case of `TestRegistryReadingOverridesTag`, and dropping the completion wake reddened `TestResolverPendingThenDoneWakes`.
The envtest needed a second pass: with the fake registry answering at once, deleting the reconciler's wake left `TestV2_RegistryRead_DigestOnlyImageGetsVerdict` green in 0.8 s, because the status write's own watch event reconciled the set again before the inspection could lose the race.
The registry now holds the layer until the set has gone quiet on `Unknown`, and the verdict is required within three seconds of release, a window the ten-hour resync cannot explain.

## Out of scope

- Opening `ghcr.io` in the FQDN allowlist or the proxy's `PROXY_ALLOWED_HOST_SUFFIXES`: that widens worker egress too, and is a posture decision for its own row.
- Node-identity registry credentials, which the AGC cannot obtain.
- A `status.attestedRunnerVersion` field.
