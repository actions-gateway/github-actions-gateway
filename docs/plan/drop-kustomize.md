# Plan: Helm as the sole cluster-install path (Q142)

← [STATUS](../STATUS.md) · absorbs [Q73](../STATUS.md)

## Goal

Make the **Helm chart** the single thing that installs the gateway to a cluster,
and eliminate the duplicate, hand-maintained manifests that currently exist in
both `cmd/*/config/` (kustomize) and `charts/actions-gateway/`. Today there are
two parallel deploy systems and the chart's CRDs/RBAC are hand-copied from the
generated `config/` output — a silent-drift liability ([Q73](../STATUS.md)).

## What stays, what goes

`config/` is **not** purely an install vehicle — it is the code-generation and
test substrate, and that role is irreplaceable:

- **`controller-gen` output target.** `make manifests` writes the CRD/RBAC
  manifests as plain YAML into `config/crd`, `config/rbac`, `config/webhook`
  (`cmd/gmc/Makefile`, `cmd/agc/Makefile`). controller-gen emits plain YAML; the
  chart is downstream of it.
- **envtest CRD source.** The integration suites load CRDs straight from disk
  (`CRDDirectoryPaths: [".../config/crd/bases", ".../agc/config/crd"]`). envtest
  needs plain YAML; it never invokes kustomize.

So we keep the generated plain-YAML manifests under `config/`, but retire
**kustomize the overlay tool** as a *deploy* path and stop hand-maintaining the
chart copies.

## Slices (each its own PR)

### A. Single-source CRDs + drift gate — *this PR* (absorbs Q73)

The chart's `templates/crds/*.yaml` are hand-copies of the controller-gen
sources plus a `helm.sh/resource-policy: keep` annotation. The GMC's bundled
`config/crd/bases/...runnergroups.yaml` is controller-gen output of the
*imported* RunnerGroup type, which can drift from the AGC-authoritative copy
under k8s.io/api skew — the exact Q73 hazard.

- **Authoritative sources** (owned by each module's controller-gen):
  - `cmd/gmc/config/crd/bases/...actionsgateways.yaml` (GMC owns `ActionsGateway`)
  - `cmd/agc/config/crd/...runnergroups.yaml` (AGC owns `RunnerGroup`)
- **`scripts/sync-chart-crds.sh`** generates the two chart CRD templates from
  those sources, injecting the per-CRD `helm.sh/resource-policy: keep` annotation
  block. `make chart-crds` writes them.
- **`make chart-crds-check`** (vendor-check pattern) re-runs the sync and
  `git diff --exit-code`s the chart copies, **and** asserts the GMC-bundled
  RunnerGroup is byte-identical to the AGC-authoritative copy. Wired into
  `make check` and `manifest-validate.yml`. A future field add that isn't
  propagated, or an api-skew drift, fails CI.

This removes the chart-side hand-copy as a source of truth (it becomes
generated) and gates the GMC-bundled copy against AGC.

### B. e2e installs via Helm — *later PR*

Switch the kind e2e suite's `setupGMC()` from `make deploy` (kustomize) to
`helm install` of the chart, so the chart install path is exercised on every
e2e run. Needs a CI e2e cycle to validate; cannot complete in a local-only
session.

### C. Delete kustomize overlays — *later PR, after B*

Once e2e installs via Helm, remove the kustomize *deploy* path (`config/default`
overlays, the `make deploy`/`make install` kustomize wiring) and replace the dev
loop with a `helm install ... allowFloatingImageTags=true` shortcut. Keep the
controller-gen plain-YAML output under `config/` for codegen + envtest. The one
real seam to design through is RBAC: controller-gen emits the role *rules* under
a fixed `roleName`, while the chart templates the *metadata/names* — so RBAC
becomes a generate-rules + Helm-template-wrapper step, not a copy.

## Out of scope

- Generating the chart's **RBAC** from `config/rbac` (slice C seam) — deferred
  with slice C.
- Removing the GMC's bundled RunnerGroup copy entirely (vs. gating it) — that
  depends on reworking the GMC kustomization/envtest paths in slice C.
