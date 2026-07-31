# Runner template library (Q554)

Ship a small curated library of golden `ClusterRunnerTemplate` instances so a
new operator starts from a validated template instead of hand-authoring one,
and later forks to a custom template via the normal `templateRef` path. Filed
2026-07-31 from a design discussion on Kata onboarding; this doc records the
decisions so the implementing session doesn't re-litigate them.

## Status

Not started. Tracked as [Q554](../STATUS.md#Q554).

## Motivation

- [getting-started.md](../getting-started.md) makes every operator hand-author
  a `RunnerTemplate` before anything runs — the single biggest onboarding
  cliff.
- The Kata DinD shape's hard-won details (raw block volume for
  `/var/lib/docker`, `/dev/kmsg`, cgroup/`/proc/sys` remounts, the validated
  18-capability set) live only in the 493-line
  [kata-dind-workloads.md](../operations/kata-dind-workloads.md); operators
  must transcribe them by hand.
- The charts ship zero template instances today, yet two live-validated ones
  already exist as dogfood e2e fixtures.

## Decisions

### No new CRD

`ClusterRunnerTemplate` already exists for exactly this — its godoc names
"golden privileged templates (DinD, sysbox)" as its purpose
(`api/v2beta1/runnertemplate_types.go`). A new PodTemplate-bearing CRD would
re-pay the ~1.21 MB OpenAPI cost that already forced the v2 CRDs into their
own opt-in chart (`api/v2beta1/scheduling_types.go` quantifies it), and the
Q172 default ladder (`templateRef` → gateway default → cluster-default
annotation) already provides the start-simple-then-customize migration path.
Library entries are CRs, not CRDs — free at the apiserver.

### Library admission rule: only what CI exercises

A shipped golden template is an implicit validation claim. The gate for
membership: **GAG's own e2e runs it, or it is trivially inert.** This is why
gVisor and sysbox are excluded from v1 (see Later, below) — shipping them
would claim a validation the repo explicitly does not have
([appendix-b-worker-isolation.md](../design/appendix-b-worker-isolation.md)).

### v1 contents

| Template | Source | Covers |
|---|---|---|
| `plain` | new; trivial (baseline PSA, no Docker) | Unit tests / plain CI jobs. The only entry that composes with the `applySecurityDefaults` gap-fill. |
| `kata-dind` | promote `deploy/dogfood-e2e/overlays/kata/resources.yaml` | Image builds and kind-based e2e with sandbox isolation. |
| `privileged-dind` | promote `deploy/dogfood-e2e/overlays/dind/resources.yaml` | The DinD fallback for clusters without nested virt. |

### Name by mechanism, map by workload

Workload names ("unit tests", "kind e2e") are rows in a mapping table, not
template names — kind e2e *is* the DinD shape, delivered as `kata-dind` or
`privileged-dind` depending on cluster capability. The library README carries
the workload → template decision table, following the pattern
[in-runner-image-builds.md](../operations/in-runner-image-builds.md)
established.

### Invert the dogfood relationship

The library (e.g. `deploy/templates/`) becomes the kustomize **base**; the
dogfood e2e overlays patch e2e-specifics (worker image tag, sizing, GKE
Workload Identity annotations) on top. The shipped artifact is then
byte-for-byte the thing CI validates, minus declared patches — the library
cannot silently drift from its validation. The main genericizing decision is
what the shipped `workerImage` reference is; the GKE-specific bits get marked
as such (the reference architecture claims provider-agnostic).

### Packaging and security posture

- Every entry is **opt-in** (kustomize base and/or values-gated chart
  instances, each off by default). None ships carrying the
  `is-default-template` annotation — choosing a cluster default stays an
  operator decision.
- The Kata/DinD entries require namespace PSA `privileged`, which turns
  `applySecurityDefaults` into a no-op — the template carries the whole
  security burden. Opt-in artifacts improve security (operators get the
  vetted capability set instead of hand-rolling worse); a shipped *default*
  would silently disable the gap-fill and is out.
- `privileged-dind` is framed everywhere as the fallback ("almost never the
  approach you actually need"), never the starting point.

### Fold in: stale enforcement claim in 05-security.md

[05-security.md](../design/05-security.md) § "Pairing `privileged` with a
sandbox runtime" still says the platform "cannot enforce" the pairing from
the GMC. That predates the Q172 ladder — a cluster-default
`ClusterRunnerTemplate` *is* platform enforcement now. Fix alongside the
library docs.

## Later — demand-gated, not v1

- `gvisor` — only after [Q15](../STATUS.md#Q15) fires *and* the template gets
  e2e coverage.
- `sysbox`, rootless-BuildKit — same admission rule; BuildKit-rootless is the
  decision table's recommended build path but has no template-level CI
  coverage today.
- GPU — [Q216](../STATUS.md#Q216) already scopes RuntimeClass conventions.

## Acceptance criteria

1. `deploy/templates/` (or equivalent) ships `plain`, `kata-dind`,
   `privileged-dind` with a README carrying the workload → template mapping
   and the fallback framing.
2. Dogfood e2e overlays consume the library base via kustomize patches; the
   e2e stays green.
3. `getting-started.md` and `tenant-onboarding.md` point at the library as
   the starting path; `kata-dind-workloads.md` references `kata-dind` instead
   of only inlining the full spec.
4. The 05-security.md enforcement claim is corrected.
5. No template ships as a cluster default; each is individually opt-in.

## Non-goals

- Installing runtime handlers or `RuntimeClass` objects — the controllers
  deliberately never do this ([kata-dind-workloads.md](../operations/kata-dind-workloads.md)
  § "What GAG does and does not manage"); node pools, kata-deploy, and
  RuntimeClass registration remain cluster-admin prerequisites.
- An isolation-profile enum modeled on `sizing.profile` — a profile can
  inject one field, but the Kata DinD shape is a whole pod spec
  (template-shaped, not transform-shaped).
- Workload-named template aliases.
