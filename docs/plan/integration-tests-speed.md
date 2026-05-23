# Integration test speed improvements

## Background

Integration tests run against an in-process envtest API server (etcd + kube-apiserver, no kubelet). Reconciliations complete in single-digit milliseconds. Several tests use polling intervals and fixed sleeps calibrated for a real cluster, leaving significant dead time on the table.

The AGC integration tests already use aggressive polling (1ms for in-memory checks, 50ms for Kubernetes state). The GMC tests and a handful of AGC provisioner tests have not been brought in line.

---

## 1. GMC integration tests: 500ms polling → 25ms

**Files:** `cmd/gmc/internal/controller/integration/` (all test files)

Every `Eventually` call in the GMC suite uses a 500ms polling interval. `TestGMC_TenantProvisioning_AllResourcesCreated` alone has 11 serial `Eventually` calls — each condition resolves in <10ms against envtest, but the test then waits up to 490ms for the next poll. At 11 calls that is up to 5.5 seconds of dead time in one test.

Changing all GMC `Eventually` polling from `500*time.Millisecond` to `25*time.Millisecond` is safe: envtest never needs 500ms to process a write, and 25ms gives plenty of headroom on a loaded CI machine. This matches the AGC side's existing convention.

Estimated saving: **10–15 seconds** across the GMC integration suite.

---

## 2. `time.Sleep(2 * time.Second)` → deterministic wait

**File:** `cmd/agc/internal/controller/integration/pod_provisioning_test.go:276–278`

`TestAGC_PodProvisioning_MaxWorkersCeiling` uses an unconditional 2-second sleep to give the provisioner time to attempt — and abort — pod creation when the `maxWorkers` ceiling is saturated.

Replace with an active poll: wait until the job Secret that the provisioner creates has been deleted (indicating the provisioner completed its cycle and backed off), then assert that no `runner-*` pod exists. This is already deterministic because the provisioner always cleans up the Secret whether or not it creates a pod.

```go
// Instead of time.Sleep(2 * time.Second):
require.Eventually(t, func() bool {
    var secrets corev1.SecretList
    _ = k8sClient.List(ctx, &secrets,
        client.InNamespace(nsName),
        client.MatchingLabels{"actions-gateway/runner-group": "ceiling-rg"},
    )
    for _, s := range secrets.Items {
        if strings.HasPrefix(s.Name, "job-") {
            return false
        }
    }
    return true
}, 10*time.Second, 25*time.Millisecond, "provisioner should clean up the job Secret after ceiling check")
```

Estimated saving: **2 seconds** unconditionally.

---

## 3. AGC failure recovery: 200ms → 1ms for session registration

**File:** `cmd/agc/internal/controller/integration/failure_recovery_test.go:62`, `:122`

Two tests in the failure recovery suite poll `brokerStub.RegisteredSessions()` at 200ms. This is a pure in-memory map lookup on the broker stub — no network, no Kubernetes API. All other AGC tests use 1ms for the same check. Change both to `1*time.Millisecond`.

Estimated saving: small per test, but eliminates the inconsistency.

---

## 4. Provisioner poll interval: 200ms → 50ms

**File:** `cmd/agc/internal/controller/integration/suite_integration_test.go:165`

`startAGCReconcilerOpts` defaults the `Provisioner.PollInterval` to `200ms` for all provisioner-based integration tests (pod provisioning, secret lifecycle, failure recovery — six tests total). This is the loop that watches pod phase transitions and fires cleanup/rerun. Every test that advances a pod phase must wait up to one full poll cycle.

Reducing to `50ms` cuts the per-test wait without making tests flaky: envtest's status updates are synchronous and the provisioner will see them on the very next poll.

Estimated saving: **1–3 seconds** across the six provisioner tests.

---

## Implementation order

| # | Change | Est. saving | Risk |
|---|--------|-------------|------|
| 1 | GMC poll 500ms → 25ms | 10–15 s | Low |
| 2 | Remove 2 s sleep | 2 s | Low |
| 3 | Failure recovery poll 200ms → 1ms | < 1 s | Low |
| 4 | Provisioner poll 200ms → 50ms | 1–3 s | Low |

All changes are mechanical. None affect what the tests verify, only how quickly they detect the outcome.
