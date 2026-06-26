# Implementation Plans — Code and Design Fixes

Three issues identified during the documentation audit require code or design changes, not just documentation. This doc covers each one.

## Status at a glance

Last refreshed 2026-06-01.

| # | Fix | Files affected | Status |
|---|---|---|---|
| 1 | Expose `maxEvictionRetries` / `evictionRetryDelay` on `RunnerGroup` CRD | `cmd/agc/api/v1alpha1/runnergroup_types.go`, `cmd/agc/internal/provisioner/provisioner.go` | ✅ Done — fields added to spec; `HandlerFor` reads per-RG overrides |
| 2 | Per-key merge for `proxy.resources` (fix HPA silent failure) | `cmd/gmc/internal/controller/builder.go:273-278`, `cmd/gmc/internal/webhook/v1alpha1/actionsgateway_webhook.go:138-151` | ✅ Done — per-key merge in builder; webhook emits warning when `requests` set without `cpu` |
| 3 | Credential rotation: pod-template annotation, Secret watch, `CredentialUnavailable` condition | `cmd/gmc/internal/controller/builder.go`, `cmd/gmc/internal/controller/actionsgateway_controller.go`, `docs/getting-started.md` | ✅ Done — all three sub-changes shipped; 3 integration tests + 1 unit test added |

All three fixes shipped.

---

## 1. Expose eviction retry settings in the RunnerGroup CRD

### Problem

`MaxEvictionRetries` and `EvictionRetryDelay` are hardcoded in `NewProvisioner` (`provisioner.go:89–90`). Every RunnerGroup in every tenant uses the same retry budget (2 retries, 5s delay) regardless of workload characteristics. A GPU job that legitimately OOMs every time and a CPU job that hit a transient node eviction should not share a retry budget or delay.

### Design

Two new optional fields on `RunnerGroupSpec` (already added to `docs/design/03-api-contracts.md §3.1`):

```go
MaxEvictionRetries *int32          `json:"maxEvictionRetries,omitempty"`  // default 2, min 0, max 10
EvictionRetryDelay *metav1.Duration `json:"evictionRetryDelay,omitempty"`  // default "5s", min "1s"
```

Operators setting `maxEvictionRetries: 0` disable auto-retry entirely (useful for GPU workloads where a failed job warrants manual inspection before rerunning).

### Implementation

**`cmd/agc/api/v1alpha1/runnergroup_types.go`**

Add the two fields to `RunnerGroupSpec` with kubebuilder markers matching the design doc. Add a CEL validation rule that rejects `evictionRetryDelay` below 1s:

```go
// +kubebuilder:validation:XValidation:rule="!has(self.evictionRetryDelay) || self.evictionRetryDelay.seconds >= 1",message="evictionRetryDelay must be at least 1s"
```

**`cmd/agc/internal/provisioner/provisioner.go`**

`HandlerFor` already receives a `*v1alpha1.RunnerGroup`. Read the spec fields and override the provisioner-level defaults when the fields are set:

```go
func (p *Provisioner) HandlerFor(rg *v1alpha1.RunnerGroup) listener.JobHandlerFunc {
    maxRetries := p.MaxEvictionRetries
    if rg.Spec.MaxEvictionRetries != nil {
        maxRetries = int(*rg.Spec.MaxEvictionRetries)
    }
    retryDelay := p.EvictionRetryDelay
    if rg.Spec.EvictionRetryDelay != nil {
        retryDelay = rg.Spec.EvictionRetryDelay.Duration
    }
    return func(ctx context.Context, runServiceURL, planID string, payload []byte) error {
        return p.provision(ctx, rg, planID, payload, maxRetries, retryDelay)
    }
}
```

Pass `maxRetries` and `retryDelay` into `provision` (or a per-call config struct) rather than reading them from `p` — this avoids a data race if two goroutines serve different RunnerGroups with different settings.

**`cmd/agc/internal/provisioner/provisioner_test.go`**

Add table-driven cases:
- `maxEvictionRetries: 0` — eviction is logged, metric incremented, no rerun call made
- `maxEvictionRetries: 1` — one rerun attempt, then exhausted
- `evictionRetryDelay: "50ms"` (test override) — verify the delay is respected before the rerun call

**CRD regeneration**

Run `make generate manifests` after the type change. The generated CRD YAML in `config/crd/bases/` will include the new fields and CEL rule.

### Acceptance criteria

- `kubectl apply` of a RunnerGroup with `maxEvictionRetries: 0` is accepted; with `maxEvictionRetries: 11` is rejected.
- A RunnerGroup with `maxEvictionRetries: 0` produces no rerun API call on eviction; `actions_gateway_eviction_retries_exhausted_total` increments immediately.
- A RunnerGroup omitting both new fields behaves identically to the pre-change code (defaults preserved).

---

## 2. Fix proxy resource override dropping CPU request (HPA silent failure) — ✅ Done

Per-key merge implemented in `builder.go:273-278`; webhook warning for `requests`-set-without-`cpu` in `actionsgateway_webhook.go:138-151`. Builder unit tests at `builder_test.go:140-185` and webhook tests at `actionsgateway_webhook_test.go:182-223` cover the four cases (default, partial override, limits-only, full override) and the warning. The original problem and design are kept below for historical reference.

### Problem

In `builder.go:173–174`, if an operator specifies any value in `proxy.resources`, the user-supplied `ResourceRequirements` entirely replaces the built-in defaults:

```go
if ag.Spec.Proxy.Resources.Requests != nil || ag.Spec.Proxy.Resources.Limits != nil {
    res = ag.Spec.Proxy.Resources
}
```

If an operator sets only `resources.limits` (or sets `requests` without a `cpu` entry), the proxy Deployment ends up with no `requests.cpu`. The HPA's CPU utilization metric becomes `<unknown>` and autoscaling stops silently. There is no error, no event, no condition — the proxy pool just sticks at `minReplicas` indefinitely under load.

### Design

Merge user-supplied values over the defaults instead of replacing them. The merge should be field-level (requests and limits independently) so an operator who sets only `limits.memory` still gets the default `requests.cpu`.

### Implementation

**`cmd/gmc/internal/controller/builder.go`**

Replace the current override block with a per-key merge:

```go
res := corev1.ResourceRequirements{
    Requests: corev1.ResourceList{
        corev1.ResourceCPU:    resource.MustParse("10m"),
        corev1.ResourceMemory: resource.MustParse("32Mi"),
    },
    Limits: corev1.ResourceList{
        corev1.ResourceCPU:    resource.MustParse("100m"),
        corev1.ResourceMemory: resource.MustParse("64Mi"),
    },
}
for k, v := range ag.Spec.Proxy.Resources.Requests {
    res.Requests[k] = v
}
for k, v := range ag.Spec.Proxy.Resources.Limits {
    res.Limits[k] = v
}
```

This means:
- Operator sets nothing → full defaults apply.
- Operator sets only `limits.memory: 128Mi` → CPU defaults preserved, memory limit overridden.
- Operator sets only `requests.cpu: 50m` → CPU request overridden, all other defaults preserved.
- Operator sets both fully → their values win for all specified keys.

**`cmd/gmc/internal/webhook/v1alpha1/actionsgateway_webhook.go`**

Add a validation warning (not a rejection) in `ValidateCreate` and `ValidateUpdate` if `proxy.resources.requests` is explicitly set but contains no `cpu` key. This catches the case where an operator provides a `requests` map without `cpu`, which would suppress the default and break HPA. A `Warning` (not an `error`) allows the apply to succeed while surfacing the issue in `kubectl apply` output.

```go
if ag.Spec.Proxy.Resources.Requests != nil {
    if _, hasCPU := ag.Spec.Proxy.Resources.Requests[corev1.ResourceCPU]; !hasCPU {
        warnings = append(warnings, "proxy.resources.requests does not include cpu; "+
            "HPA requires a cpu request to compute utilization — autoscaling will not function")
    }
}
```

**`cmd/gmc/internal/controller/builder_test.go` (or `hpa_update_test.go`)**

Add cases:
- User sets only `resources.limits.memory` → deployment has default `requests.cpu: 10m`
- User sets only `resources.requests` without cpu → deployment has default `requests.cpu: 10m`, webhook emits warning
- User sets full `resources` → user values win for each key set; defaults apply for unset keys

### Acceptance criteria

- A proxy Deployment created from an `ActionsGateway` that specifies only `proxy.resources.limits.memory: 128Mi` has `requests.cpu: 10m` in its container spec.
- HPA reports a numeric CPU utilization (not `<unknown>`) for a proxy Deployment built from a partial `resources` override.
- `kubectl apply` of an `ActionsGateway` with `proxy.resources.requests` that omits `cpu` prints a warning and succeeds (does not reject).

---

## 3. Make credential rotation reliable and observable

### Problem

The current `buildAGCDeployment` embeds the Secret name in `secretKeyRef` env vars. When an operator creates a new Secret and updates `gitHubAppRef.Name` in the CR, the GMC reconciler produces a new Deployment spec where the `secretKeyRef.Name` fields change. Kubernetes detects the pod template change and performs a rolling restart — so the mechanics work. However:

1. **No pod-template annotation records the rotation.** After the pod restarts, `kubectl rollout history` shows a revision but no cause. An operator debugging a broken rotation has no record of which Secret name was in effect at each revision.
2. **Silent failure if Secret content is updated in place.** If an operator updates the Secret content without changing the name (wrong procedure, but common), the Deployment spec does not change, so the pod is never restarted. The AGC continues to use the old cached env vars. There is no event or condition to indicate the mismatch. This is especially dangerous during a key-compromise scenario where speed matters.
3. **No watch on the referenced Secret.** The GMC reconciler is only triggered by changes to `ActionsGateway` objects. If the referenced Secret is deleted or corrupted, the AGC pod continues running until it restarts (at which point it fails to start). The operator has no proactive signal.

### Design

Three targeted changes:

**A. Add a pod-template annotation with the Secret name**

In `buildAGCDeployment`, add an annotation to the pod template metadata:

```go
ObjectMeta: metav1.ObjectMeta{
    Labels: map[string]string{"app": agcAppName, labelManagedBy: labelManagerValue},
    Annotations: map[string]string{
        "actions-gateway/github-app-secret": ag.Spec.GitHubAppRef.Name,
    },
},
```

When `gitHubAppRef.Name` changes, the pod template annotation changes, which guarantees Kubernetes generates a new ReplicaSet and records the rotation cause in rollout history. This change alone makes rotations fully observable with `kubectl rollout history deployment/actions-gateway-agc`.

**B. Watch the referenced Secret for deletion**

Add a watch on Secrets to the GMC controller setup, filtered to Secrets that are referenced by an `ActionsGateway`. When the referenced Secret is deleted or enters an error state, the GMC should set a `CredentialUnavailable` condition on the `ActionsGateway` CR and emit a Warning event. This gives the operator a signal without requiring log scraping.

In `actionsgateway_controller.go`, add to `SetupWithManager`:

```go
Watches(
    &corev1.Secret{},
    handler.EnqueueRequestsFromMapFunc(r.secretToActionsGateway),
)
```

Where `secretToActionsGateway` maps a Secret event to the `ActionsGateway` objects that reference it by name. The reconciler then checks that the referenced Secret exists; if not, it sets the `CredentialUnavailable` condition instead of proceeding.

**C. Document the rotation procedure**

Update `docs/getting-started.md` with a "Rotating GitHub App credentials" section that explains:
- Create a new Secret with a new name containing the updated key.
- Update `spec.gitHubAppRef.name` in the `ActionsGateway` CR to the new name.
- Wait for the AGC Deployment rollout to complete (`kubectl rollout status`).
- Verify the AGC is healthy, then delete the old Secret.
- Why updating the Secret content in place does NOT trigger a pod restart.

### Implementation

**`cmd/gmc/internal/controller/builder.go`**

Add the `actions-gateway/github-app-secret` annotation to the AGC pod template (change A).

**`cmd/gmc/internal/controller/actionsgateway_controller.go`**

- Add `secretToActionsGateway` map function (change B).
- Register the Secret watch in `SetupWithManager`.
- In `reconcileResources` or `updateStatus`, check the referenced Secret exists; surface a `CredentialUnavailable` condition if it does not.

**`cmd/gmc/internal/controller/integration/`**

Add tests:
- Rotation test: create `ActionsGateway`, rotate Secret by changing `gitHubAppRef.Name`, assert the AGC Deployment gets a new ReplicaSet and the pod template annotation reflects the new name.
- Deletion test: delete the referenced Secret, assert `CredentialUnavailable` condition is set on the CR within the reconcile window.
- In-place update test (negative): update Secret content without changing name, assert the Deployment pod template is unchanged (no spurious rollout).

**`docs/getting-started.md`**

Add the credential rotation procedure (change C).

### Acceptance criteria

- After changing `gitHubAppRef.Name` in the CR, `kubectl rollout history deployment/actions-gateway-agc` shows a new revision with an annotation indicating the new Secret name.
- Deleting the referenced Secret causes a `CredentialUnavailable` condition on the `ActionsGateway` within one reconcile cycle.
- Updating the Secret content in place (same name) does not trigger a Deployment rollout.
- The rotation procedure in `docs/getting-started.md` produces a working AGC when followed end-to-end.
