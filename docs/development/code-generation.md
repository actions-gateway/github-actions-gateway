# Agent reference: Code generation

## When to regenerate

Whenever you modify CRD types (`api/` for the shared v2 kinds, `cmd/agc/api/` or `cmd/gmc/api/` for the v1 kinds), run the corresponding targets. Also run `make manifests` whenever you add or remove RBAC verbs/resources in a controller.

The v2 (`actions-gateway.com`) `v2alpha1` kinds live in the neutral `api/` module shared by both controllers — the Actions Gateway Controller (AGC) and the Gateway Manager Controller (GMC) (Q164); the v1 (`actions-gateway.github.com`) kinds stay split across `cmd/agc/api/v1alpha1` and `cmd/gmc/api/v1alpha1`. `make -C <module> generate` is per-module, so editing a v2 type means regenerating the `api/` module, not the controller modules. The root `make generate` runs all three (`api`, `gmc`, `agc`) in order.

## API module (the shared v2 kinds)

```bash
make -C api generate   # regenerates zz_generated.deepcopy.go + the five v2 CRD YAMLs under api/config/crd
```

The `api/` module owns only API artifacts: DeepCopy methods and the five v2 CRD manifests (`ActionsGateway`, `EgressProxy`, `RunnerSet`, `RunnerTemplate`, `ClusterRunnerTemplate`). It emits **no** RBAC or webhook manifests — those markers live on the controllers/webhooks in `cmd/gmc` and `cmd/agc` and are generated there.

## AGC (two steps required)

```bash
make -C cmd/agc generate   # regenerates zz_generated.deepcopy.go
make -C cmd/agc manifests  # regenerates CRD YAML and RBAC manifests
```

## GMC (two steps required)

```bash
make -C cmd/gmc generate   # regenerates zz_generated.deepcopy.go
make -C cmd/gmc manifests  # regenerates CRD YAML and RBAC manifests
```

Both steps are required. Skipping `manifests` leaves the CRD YAML out of sync with the Go types — the apiserver will silently prune unknown fields, and tests that set those fields will see the zero value instead.

## Sync the Helm chart CRDs (after any CRD change)

The Helm charts ship the CRDs under `templates/crds/`, but the **authoritative** schema is the controller-gen output under `cmd/*/config/crd` (the v1alpha1 CRDs) and `api/config/crd` (the v2alpha1 CRDs). The chart copies are *generated* from those sources — do not hand-edit them. The split:

- **`charts/actions-gateway/templates/crds/`** — the two **v1alpha1** (`actions-gateway.github.com`) CRDs: `ActionsGateway`, `RunnerGroup` (sourced from `cmd/*/config/crd`).
- **`charts/actions-gateway-crds-v2/templates/crds/`** — the five **v2alpha1** (`actions-gateway.com`) CRDs: `ActionsGateway`, `EgressProxy`, `RunnerSet`, `RunnerTemplate`, `ClusterRunnerTemplate` (sourced from `api/config/crd`). They live in a **separate, opt-in chart** because the `RunnerTemplate`/`ClusterRunnerTemplate` CRDs each embed a full `PodTemplateSpec` (~600 KB) and adding them to the main chart pushed its Helm release Secret past the hard **1 MiB** limit (Helm stores the rendered manifest *plus* a copy of the chart source, gzipped, in one Secret). A separate release keeps each chart within budget and makes v2 opt-in ([Q149](../STATUS.md)).

`scripts/sync-chart-crds.sh` writes both charts in one pass. After regenerating manifests, re-sync:

```bash
make chart-crds   # scripts/sync-chart-crds.sh — regenerates the chart CRD templates from the sources
```

`make chart-crds-check` (run by `make check`, `make manifest-validate`, and CI's `manifest-validate.yml`) fails if a chart copy drifted from its source, or if the **GMC-bundled** RunnerGroup CRD (`cmd/gmc/config/crd/bases/…runnergroups.yaml`, controller-gen's copy of the *imported* type) has drifted from the AGC-authoritative copy — a k8s.io/api skew that would otherwise silently prune fields on deploy ([Q73](../STATUS.md)). If that check fails, align the k8s.io/api versions ([Q68](../STATUS.md)) and re-run `make -C cmd/gmc manifests`.

## Sync the Helm chart RBAC (after any RBAC marker change)

The chart's GMC `manager-role` ClusterRole templates the metadata/binding, but its **rules** are the controller-gen output of the GMC controllers' `+kubebuilder:rbac` markers (`cmd/gmc/config/rbac/role.yaml`). The chart embeds them via `.Files.Get` from a *generated* fragment — do not hand-edit it. After regenerating manifests, re-sync the chart:

```bash
make chart-rbac   # scripts/sync-chart-rbac.sh — regenerates charts/actions-gateway/files/manager-role-rules.yaml
```

`make chart-rbac-check` (run by `make check`, `make manifest-validate`, and CI's `manifest-validate.yml`) fails if the fragment drifted from `cmd/gmc/config/rbac/role.yaml` — so a permission added via a marker but not propagated to the chart, which would leave the deployed GMC missing the grant, fails CI ([Q142](../STATUS.md)).

## Sync the Helm chart webhook (after any webhook-marker change)

The chart ships the `ValidatingWebhookConfiguration` at `charts/actions-gateway/templates/webhook.yaml`, but the authoritative webhook **body** (rules, `failurePolicy`, `sideEffects`, `admissionReviewVersions`, the service path) is the controller-gen output of the `+kubebuilder:webhook` marker (`cmd/gmc/config/webhook/manifests.yaml` — the same file the GMC integration suite loads into envtest). The chart template is *generated* from that source, re-injecting the chart's Helm wiring (name prefix, labels, the cert-manager CA-inject annotation, the templated namespace, and the non-cert-manager `caBundle`) — do not hand-edit it. After regenerating manifests, re-sync the chart:

```bash
make chart-webhook   # scripts/sync-chart-webhook.sh — regenerates charts/actions-gateway/templates/webhook.yaml
```

`make chart-webhook-check` (run by `make check`, `make manifest-validate`, and CI's `manifest-validate.yml`) fails if the chart template drifted from the controller-gen source — so a marker change (a new intercepted resource/operation, a path or `failurePolicy` change) that is regenerated into `config/` but not propagated to the chart fails CI ([Q143](../STATUS.md)).

## agc-tenant-role rules (NOT controller-gen)

The `agc-tenant-role` ClusterRole — the permission set every AGC ServiceAccount runs as — is **not** generated from a `+kubebuilder:rbac` marker. It deliberately withholds permissions the AGC's own marker role (`cmd/agc/config/rbac/role.yaml`, ClusterRole `agc-role`) grants (e.g. `runnergroups` create/delete, `secrets` patch) for least privilege, so generating it from the markers would be a privilege escalation. Its single source is the hand-maintained fragment `charts/actions-gateway/files/agc-tenant-role-rules.yaml`: the chart embeds it via `.Files.Get` in `templates/agc-tenant-role.yaml`, and the GMC integration suite (`installAGCTenantClusterRole`) reads the same file — so the shipped role and the RBAC-scope test can never drift. Edit the fragment, not either consumer ([Q143](../STATUS.md)).

## RBAC marker placement

`+kubebuilder:rbac` is a **package-level** marker (controller-gen v0.21+). It must appear before the `package` declaration, not in a type's doc comment. Placing it on a struct silently produces no output — controller-gen won't warn, it will just generate nothing.

```go
// Correct — before the package declaration:
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

package controller
```

```go
// Wrong — on a type, silently ignored:

// MyReconciler reconciles things.
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
type MyReconciler struct { ... }
```

The markers live at the top of `cmd/gmc/internal/controller/actionsgateway_controller.go`. Non-standard verbs (`bind`, `escalate`) are supported in `verbs=` and appear in the generated role.
