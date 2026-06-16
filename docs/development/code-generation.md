# Agent reference: Code generation

## When to regenerate

Whenever you modify CRD types (`cmd/agc/api/` or `cmd/gmc/api/`), run the corresponding targets. Also run `make manifests` whenever you add or remove RBAC verbs/resources in a controller.

## AGC

```bash
make -C cmd/agc manifests  # regenerates CRD YAML and RBAC manifests
```

## GMC (two steps required)

```bash
make -C cmd/gmc generate   # regenerates zz_generated.deepcopy.go
make -C cmd/gmc manifests  # regenerates CRD YAML and RBAC manifests
```

Both steps are required. Skipping `manifests` leaves the CRD YAML out of sync with the Go types — the apiserver will silently prune unknown fields, and tests that set those fields will see the zero value instead.

## Sync the Helm chart CRDs (after any CRD change)

The Helm chart ships the two CRDs under `charts/actions-gateway/templates/crds/`, but the **authoritative** schema is the controller-gen output under `cmd/*/config/crd`. The chart copies are *generated* from those sources — do not hand-edit them. After regenerating manifests, re-sync the chart:

```bash
make chart-crds   # scripts/sync-chart-crds.sh — regenerates the chart CRD templates from the sources
```

`make chart-crds-check` (run by `make check`, `make manifest-validate`, and CI's `manifest-validate.yml`) fails if a chart copy drifted from its source, or if the **GMC-bundled** RunnerGroup CRD (`cmd/gmc/config/crd/bases/…runnergroups.yaml`, controller-gen's copy of the *imported* type) has drifted from the AGC-authoritative copy — a k8s.io/api skew that would otherwise silently prune fields on deploy ([Q73](../STATUS.md)). If that check fails, align the k8s.io/api versions ([Q68](../STATUS.md)) and re-run `make -C cmd/gmc manifests`.

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
