# Q156 — Surface provisioning + credential failures as conditions

**Goal:** Make provisioning and credential failures visible in CRD `.status.conditions`
(not just stale conditions / the event stream), and promote condition type + reason
strings to exported API consts so Q157 can extend them consistently.

**Approach:**
1. Promote condition **type** and **reason** strings to exported consts in new
   `conditions.go` files in `cmd/gmc/api/v1alpha1` and `cmd/agc/api/v1alpha1`.
   Reconcilers/metrics/listener reference the consts.
2. **ActionsGateway `Degraded` (GMC):** on a provisioning error `Reconcile` returns
   before `updateStatus`, leaving conditions stale. Capture the failing step
   (typed `provisioningError` set via a deferred wrap in `reconcileResources`) and
   write `Degraded=True` (+ `Ready=False`) *before* the early return. Clear it
   (`Degraded=False`) on the success path in `updateStatus`.
3. **RunnerGroup `CredentialUnavailable` (AGC):** the installation-token fetch
   failure path emits only an Event. Add `CredentialUnavailable=True` written to
   status before the early return (keep the Event). Clear it on the success path.

## Status
- [x] Investigate reconcilers, condition helpers, status types, const locations
- [x] GMC api consts (`conditions.go`)
- [x] AGC api consts (`conditions.go`)
- [x] GMC: provisioningError + Degraded on error path + clear on success + consts
- [x] GMC metrics.go consts
- [x] AGC: CredentialUnavailable on token-failure path + clear on success + consts
- [x] AGC worker_quota.go + listener consts
- [x] Regenerate CRDs/manifests (comment-only field doc changes)
- [x] Tests: integration (envtest) + unit
- [x] Docs: operator-facing conditions reference
- [x] Remove Q156 from STATUS.md (isolated commit)

## Notes
- Q82 quota-condition **reason** literals (`QuotaUnknown`, `InsufficientQuotaHeadroom`,
  …) are left inline — computed branch states owned by that feature; only their
  **types** are promoted. Q157 adds `WorkersUnschedulable`/`EgressRulesStale`
  alongside these consts.
