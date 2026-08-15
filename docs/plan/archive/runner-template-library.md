# Runner template library (Q554)

Ship a small curated library of golden `ClusterRunnerTemplate` instances so a new operator starts from a validated template instead of hand-authoring one, and later forks to a custom template via the normal `templateRef` path.
Filed 2026-07-31 from a design discussion on Kata onboarding; this doc records the decisions so the implementing session doesn't re-litigate them.

## Status

**Done, 2026-08-08.** Shipped as `deploy/templates/{plain,kata-dind,privileged-dind}` with [`deploy/templates/README.md`](../../../deploy/templates/README.md) as the map and [runner-template-library.md](../../operations/runner-template-library.md) as the operator entry point.
Every acceptance criterion below is met.
Two things the implementing session learned that the plan did not anticipate are recorded under [What implementation changed](#what-implementation-changed).

## Motivation

- [getting-started.md](../../getting-started.md) makes every operator hand-author a `RunnerTemplate` before anything runs: the single biggest onboarding cliff.
- The Kata DinD shape's hard-won details (raw block volume for `/var/lib/docker`, `/dev/kmsg`, cgroup/`/proc/sys` remounts, the validated 18-capability set) live only in the 493-line [kata-dind-workloads.md](../../operations/kata-dind-workloads.md); operators must transcribe them by hand.
- The charts ship zero template instances today, yet two live-validated ones already exist as dogfood e2e fixtures.

## Decisions

### No new CRD

`ClusterRunnerTemplate` already exists for exactly this.
Its godoc names "golden privileged templates (DinD, sysbox)" as its purpose (`api/v2beta1/runnertemplate_types.go`).
A new PodTemplate-bearing CRD would re-pay the ~1.21 MB OpenAPI cost that already forced the v2 CRDs into their own opt-in chart (`api/v2beta1/scheduling_types.go` quantifies it), and the Q172 default ladder (`templateRef` → gateway default → cluster-default annotation) already provides the start-simple-then-customize migration path.
Library entries are CRs, not CRDs, and free at the apiserver.

### Library admission rule: only what CI exercises

A shipped golden template is an implicit validation claim.
The gate for membership: **GAG's own e2e runs it, or it is trivially inert.** This is why gVisor and sysbox are excluded from v1 (see Later, below): shipping them would claim a validation the repo explicitly does not have ([appendix-b-worker-isolation.md](../../design/appendix-b-worker-isolation.md)).

### v1 contents

| Template | Source | Covers |
|---|---|---|
| `plain` | new; trivial (baseline PSA, no Docker) | Unit tests / plain CI jobs. The only entry that composes with the `applySecurityDefaults` gap-fill. |
| `kata-dind` | promote `deploy/dogfood-e2e/overlays/kata/resources.yaml` | Image builds and kind-based e2e with sandbox isolation. |
| `privileged-dind` | promote `deploy/dogfood-e2e/overlays/dind/resources.yaml` | The DinD fallback for clusters without nested virt. |

### Name by mechanism, map by workload

Workload names ("unit tests", "kind e2e") are rows in a mapping table, not template names. kind e2e *is* the DinD shape, delivered as `kata-dind` or `privileged-dind` depending on cluster capability.
The library README carries the workload → template decision table, following the pattern [in-runner-image-builds.md](../../operations/in-runner-image-builds.md) established.

### Invert the dogfood relationship

The library (e.g.
`deploy/templates/`) becomes the kustomize **base**; the dogfood e2e overlays patch e2e-specifics (worker image tag, sizing, GKE Workload Identity annotations) on top.
The shipped artifact is then byte-for-byte the thing CI validates, minus declared patches, so the library cannot silently drift from its validation.
The main genericizing decision is what the shipped `workerImage` reference is; the GKE-specific bits get marked as such (the reference architecture claims provider-agnostic).

### Packaging and security posture

- Every entry is **opt-in** (kustomize base and/or values-gated chart instances, each off by default).
  None ships carrying the `is-default-template` annotation, because choosing a cluster default stays an operator decision.
- The Kata/DinD entries require namespace PSA `privileged`, which turns `applySecurityDefaults` into a no-op, so the template carries the whole security burden.
  Opt-in artifacts improve security (operators get the vetted capability set instead of hand-rolling worse); a shipped *default* would silently disable the gap-fill and is out.
- `privileged-dind` is framed everywhere as the fallback ("almost never the approach you actually need"), never the starting point.

### Fold in: stale enforcement claim in 05-security.md

[05-security.md](../../design/05-security.md) § "Pairing `privileged` with a sandbox runtime" still says the platform "cannot enforce" the pairing from the GMC.
That predates the Q172 ladder, and a cluster-default `ClusterRunnerTemplate` *is* platform enforcement now.
Fix alongside the library docs.

## What implementation changed

Two findings the plan did not anticipate, both load-bearing.

### A strategic-merge patch against a CRD destroys lists, silently

kustomize resolves strategic-merge semantics from the OpenAPI schema.
It has none for a custom resource, so it falls back to an RFC 7386 JSON merge patch, where a list is **replaced wholesale** rather than merged by key.
Measured on kustomize v5.8.1 (kubectl 1.36): an overlay patch naming only `initContainers[0].resources` produced an init container with *just* that field, having dropped its `image`, `restartPolicy` and capability set, and rendered at exit 0.

This is directly in the path of "the overlay patches the base", which is the whole inversion.
Every overlay patch that reaches into a list is therefore JSON 6902 (`target:` + op list), verified lossless by the same probe.
The rule is stated in the library README, the operator doc, both entry kustomizations and both overlays, and `make template-library-check` fails a strategic merge against a `ClusterRunnerTemplate`.

The parser for that rule needed one non-obvious property: an overlay's RunnerSet patch carries `templateRef.kind: ClusterRunnerTemplate` as a nested *value*, and a naive string search reads it as the patch's own kind.
The first draft did exactly that and flagged the correct overlays.
The false positive is pinned in `check-template-library-test.sh`.

### The shipped DinD entries cannot carry a working default image

The runner container needs a Docker CLI on `PATH`; the sidecar supplies only the daemon.
Neither `ghcr.io/actions/actions-runner` nor GAG's own `worker` image ships one (`scripts/dogfood/e2e-runner/Dockerfile` exists precisely to add it).
Omitting the image would let the AGC gap-fill the stock runner, producing a pod that starts, takes a job, and fails on `docker: not found` mid-run.

Both DinD entries therefore ship `spec.workerImage: example.invalid/build-capable-runner:replace-me`.
`.invalid` is RFC 2606 reserved, so an unreplaced value fails at image pull with the fix in the string.
It also makes the e2e overlays' `workerImage` patch mandatory rather than incidental, which is a second attachment between the shipped artifact and the exercised one.

## Later: demand-gated, not v1

- `gvisor`: only after [Q15](../../STATUS.md#Q15) fires *and* the template gets e2e coverage.
- `sysbox`, rootless-BuildKit: same admission rule; BuildKit-rootless is the decision table's recommended build path but has no template-level CI coverage today.
- GPU: [Q216](../../STATUS.md#Q216) already scopes RuntimeClass conventions.

## Acceptance criteria

1. ✅ `deploy/templates/` ships `plain`, `kata-dind`, `privileged-dind` with a README carrying the workload → template mapping and the fallback framing.
2. ✅ Dogfood e2e overlays consume the library base via kustomize patches.
   The render is reconciled against the pre-change one: both overlays produce byte-identical output apart from the object rename, the runner image moving to `spec.workerImage`, and the `templateRef` following the rename.
3. ✅ `getting-started.md` and `tenant-onboarding.md` point at the library as the starting path; `kata-dind-workloads.md` leads with `kubectl apply -k deploy/templates/kata-dind` and keeps its inline spec as annotation.
4. ✅ The 05-security.md enforcement claim is corrected.
5. ✅ No template ships as a cluster default; each is individually opt-in, and `check-template-library.sh` fails on an entry that so much as mentions the annotation.

Beyond the criteria: the admission rule is now mechanical rather than aspirational.
`make template-library-check` reconciles the shipped set against the exercised set in both directions, and `TestTemplateLibrary_Admitted` applies every entry to a real apiserver on each integration run, which is what upgrades `plain` from "trivially inert, trust us" to a template CI actually admits.

## Non-goals

- Installing runtime handlers or `RuntimeClass` objects.
  The controllers deliberately never do this ([kata-dind-workloads.md](../../operations/kata-dind-workloads.md) § "What GAG does and does not manage"); node pools, kata-deploy, and RuntimeClass registration remain cluster-admin prerequisites.
- An isolation-profile enum modeled on `sizing.profile`.
  A profile can inject one field, but the Kata DinD shape is a whole pod spec (template-shaped, not transform-shaped).
- Workload-named template aliases.
