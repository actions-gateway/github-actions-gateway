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

### B. e2e installs via Helm — *DONE*

`cmd/gmc/Makefile`'s `deploy`/`undeploy` now wrap `helm upgrade --install` /
`helm uninstall` of `charts/actions-gateway` (the same chart we publish), and the
kind e2e suite's `setupGMC()`/`teardownGMC()` drive them — so the chart install
path is exercised on every e2e run. The chart sets `allowFloatingImageTags=true`
and the gmc/agc/proxy image values from `GMC_IMG`/`AGC_IMG`/`PROXY_IMG`; the
e2e-only `--allow-agc-extra-env` + `AGC_EXTRA_*` env injection stays as a
post-install `kubectl patch`. An `azure/setup-helm` step was added to
`e2e-test.yml`. The green e2e CI run on the PR is the proof.

### C. Delete kustomize overlays — *DONE*

Removed the kustomize *deploy* path: `config/default`, `config/manager`,
`config/certmanager`, `config/network-policy`, `config/prometheus`,
`config/samples`, the `config/agc-tenant-role` copy, and the deploy-only
`config/rbac` scaffolding (leader-election, metrics, editor/viewer/admin roles,
service account, role binding, kustomizations), plus the `make deploy`/`install`
kustomize wiring. The chart owns every one of those resources.

**What stays** under `cmd/*/config/` is the codegen + envtest substrate, NOT an
install vehicle: the controller-gen CRD/RBAC/webhook outputs (also the
single-source inputs to the chart generators, and what `rbac_test.go` + envtest
load) and the two `admission-policy` ValidatingAdmissionPolicies the GMC
integration suite applies in envtest. `make manifests` reproduces them.

**RBAC seam — single-sourced now** (user decision, this PR). `scripts/sync-chart-rbac.sh`
generates `charts/actions-gateway/files/manager-role-rules.yaml` from
`cmd/gmc/config/rbac/role.yaml`; the chart's `manager-role` template embeds it
via `.Files.Get`, and `make chart-rbac-check` gates drift (wired into `make check`
and `make manifest-validate`) — mirroring slice A's CRD gate. `manifest-validate`
no longer renders any kustomize overlay; it schema-validates the retained
controller-gen manifests + VAPs as standalone files plus the chart renders.

## Out of scope (follow-ups)

- ~~Single-sourcing the chart's **webhook** config and the **agc-tenant-role** /
  metrics / leader-election roles.~~ **DONE in [Q143](q143-single-source-webhook-roles.md).**
  The webhook now generates from `cmd/gmc/config/webhook/manifests.yaml`
  (`make chart-webhook` + `chart-webhook-check`); the agc-tenant-role rules are
  single-sourced in a shared `files/` fragment read by both the chart and the GMC
  integration suite. The metrics / leader-election roles needed no work: slice C
  removed their `config/` copies, so the chart is already their sole copy and no
  controller-gen generator exists for that standard scaffolding. (The original
  note here assumed retained `config/` copies for all four; only the webhook
  still had one.)
- Removing the GMC's bundled RunnerGroup copy entirely (vs. gating it).
