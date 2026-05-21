# Milestone 4 Test Improvements

This document identifies gaps in the Milestone 4 unit tests and proposes concrete additions, ordered by priority.

---

## 1. Bug: `buildNoProxy` replaces instead of merges

**File:** `cmd/gmc/internal/controller/builder.go`

The design plan (§3.5) states:

> `buildNoProxy` merges the user-provided `spec.proxy.noProxyCIDRs` (or the default list if empty) with the cluster-internal exclusions: `kubernetes.default.svc.cluster.local`, `localhost`, `127.0.0.1`, and the service CIDR.

The implementation does not merge — when the user provides `noProxyCIDRs`, it returns only those values, dropping the mandatory cluster-internal exclusions:

```go
func buildNoProxy(userCIDRs []string) string {
    if len(userCIDRs) > 0 {
        return strings.Join(userCIDRs, ",")  // drops defaultNoProxy entirely
    }
    return defaultNoProxy
}
```

**Impact:** If a user sets `spec.proxy.noProxyCIDRs`, the AGC's Kubernetes API calls may be routed through the CONNECT proxy instead of going directly to the API server. This would cause the AGC to malfunction at runtime.

**Fix:** Always append the mandatory defaults, prepending user-provided CIDRs if any:

```go
func buildNoProxy(userCIDRs []string) string {
    if len(userCIDRs) > 0 {
        return strings.Join(userCIDRs, ",") + "," + defaultNoProxy
    }
    return defaultNoProxy
}
```

**Tests to add in `builder_test.go`:**

| Test | Assertion |
|---|---|
| `TestBuildNoProxy_DefaultWhenEmpty` | No user CIDRs → result equals `defaultNoProxy` |
| `TestBuildNoProxy_UserCIDRsPrependedToDefaults` | User provides `["192.168.0.0/16"]` → result contains both the user CIDR and all four default entries |
| `TestBuildNoProxy_AlwaysContainsKubeAPIServer` | Any non-empty user input → result contains `kubernetes.default.svc.cluster.local` |
| `TestBuildAGCDeployment_NoProxyContainsDefaults` | `buildAGCDeployment` with non-empty `NoProxyCIDRs` → `NO_PROXY` env still contains cluster-internal entries |

---

## 2. `buildNetworkPolicy` — AGC/worker egress and DNS rules untested

**File:** `cmd/gmc/internal/controller/builder.go`

`TestBuildNetworkPolicy_ProxyEgress` passes a non-empty `proxyClusterIP` but only asserts the port-443 GitHub CIDR rule. Two other rules in the policy are not verified:

- **AGC/worker egress:** the `proxyClusterIP/32` peer on port 8080 (routes AGC and worker pod traffic to the proxy)
- **DNS egress:** port 53 UDP and TCP (always present, regardless of `managedNetworkPolicy`)

**Tests to add in `builder_test.go`:**

| Test | Assertion |
|---|---|
| `TestBuildNetworkPolicy_AGCWorkerEgressToProxy` | Pass `proxyClusterIP="10.96.0.1"` → egress rule exists with peer `10.96.0.1/32` on port 8080 |
| `TestBuildNetworkPolicy_DNSEgressAlwaysPresent` | Both `managedNetworkPolicy=true` and `managedNetworkPolicy=false` → egress rules include port 53 UDP and TCP |
| `TestBuildNetworkPolicy_NoProxyEgressWhenClusterIPEmpty` | Pass empty `proxyClusterIP` → no AGC/worker egress rule exists |

---

## 3. `HTTPGitHubIPRangeFetcher` — untested production code path

**File:** `cmd/gmc/internal/controller/ipranges.go`

`HTTPGitHubIPRangeFetcher.FetchIPRanges` is the only production implementation of `GitHubIPRangeFetcher` and is entirely untested. All existing IP range tests use `stubFetcher`.

**Tests to add in `ipranges_test.go`** (using `httptest.NewServer`):

| Test | Assertion |
|---|---|
| `TestHTTPFetcher_ParsesCIDRs` | Server returns `{"actions":["140.82.112.0/20","192.30.252.0/22"]}` → returns two parsed `net.IPNet` values |
| `TestHTTPFetcher_Non200Response` | Server returns 500 → error containing the status code |
| `TestHTTPFetcher_InvalidJSON` | Server returns malformed body → error |
| `TestHTTPFetcher_MalformedCIDRSkipped` | Response contains one valid and one malformed CIDR → returns only the valid one (no error) |
| `TestHTTPFetcher_EmptyActions` | Response contains `{"actions":[]}` → returns empty slice, no error |

---

## 4. Missing builder coverage — `buildProxyServiceAddr`

**File:** `cmd/gmc/internal/controller/builder.go`

`buildProxyServiceAddr` produces the `HTTP_PROXY`/`HTTPS_PROXY` value that the AGC uses for all outbound traffic. It is not directly tested; `TestBuildAGCDeployment_ProxyEnv` only checks that the env is non-empty.

**Tests to add in `builder_test.go`:**

| Test | Assertion |
|---|---|
| `TestBuildProxyServiceAddr_Format` | `buildProxyServiceAddr` for namespace `"team-a"` returns `"http://actions-gateway-proxy.team-a.svc.cluster.local:8080"` |
| `TestBuildAGCDeployment_NoProxyNotEmpty` | (existing `TestBuildAGCDeployment_ProxyEnv` already checks `NO_PROXY` is non-empty — update it to also assert the format after the `buildNoProxy` fix) |

---

## 5. Missing builder coverage — untested resource constructors

Several builder functions have no direct tests. Most are simple but some have non-trivial field choices.

**Tests to add in `builder_test.go`:**

| Test | What to verify |
|---|---|
| `TestBuildAGCRoleBinding_WiresCorrectSA` | `RoleRef.Kind="Role"`, `RoleRef.Name=agcSAName`, subject SA name and namespace match the AG |
| `TestBuildProxyService_PortAndSelector` | Port 8080 TCP, `Type=ClusterIP`, selector `app=actions-gateway-proxy` |
| `TestBuildRunnerGroup_SetsSpecAndLabels` | Spec is passed through unchanged; `managedLabels` present; name and namespace from AG |
| `TestBuildResourceQuota_PassesThrough` | `Spec.Hard` in the returned quota equals `spec.namespaceQuota` from the AG |
| `TestBuildPDB_MinAvailableAndSelector` | `MinAvailable=1`, selector matches proxy app label |
| `TestBuildHPA_DefaultCPUTarget` | No `TargetCPUUtilizationPercentage` in spec → HPA metric target is 60 |
| `TestBuildHPA_CustomCPUTarget` | `TargetCPUUtilizationPercentage=80` → HPA metric target is 80 |
| `TestBuildProxyDeployment_InitialReplicas` | No `MinReplicas` → `Spec.Replicas=2`; with `MinReplicas=1` → `Spec.Replicas=1` |
| `TestBuildProxyDeployment_AntiAffinity` | `PodAntiAffinity` present, weight=100, topology key `kubernetes.io/hostname` |
| `TestBuildProxyDeployment_Probes` | Liveness and readiness probes both target `/healthz` on port 8081 |
| `TestManagedLabels_ContainsOwnerRef` | Labels map contains `actions-gateway/owner-name` and `actions-gateway/owner-ns` matching the AG |

---

## 6. `Server.ListenAndServe` context cancellation

**File:** `cmd/proxy/proxy.go`

`ListenAndServe` is the entry point for the proxy in production but is not tested. The existing tests bypass it by calling `handleConnect` directly via `httptest.NewServer`. The context cancellation path (the `select { case <-ctx.Done() }` branch) is completely untested.

**Tests to add in `proxy_test.go`:**

| Test | Assertion |
|---|---|
| `TestServer_ListenAndServeShutdown` | Start `ListenAndServe` in a goroutine, cancel the context, verify it returns `nil` (not an error) and does not leak goroutines (goleak) |
| `TestServer_ListenAndServeBothServersReachable` | After `ListenAndServe` starts, a CONNECT request to `Addr` succeeds and `GET /healthz` to `HealthAddr` returns 200 |

---

## 7. `IPRangeReconciler.Start` — ticker loop untested

**File:** `cmd/gmc/internal/controller/ipranges.go`

`Start` is the `manager.Runnable` entry point and contains: an immediate first call, a ticker loop, and context-cancellation exit. None of this is tested — existing tests call `reconcileAll` directly.

**Tests to add in `ipranges_test.go`:**

| Test | Assertion |
|---|---|
| `TestIPRangeReconciler_Start_RunsImmediately` | Set a short `Interval` and a stub that counts calls; after `Start` returns the first reconcile has already run before the first tick |
| `TestIPRangeReconciler_Start_CancelExitsCleanly` | `Start` with a cancelled context returns `nil` without blocking (timeout the test at 1s) |

---

## 8. Webhook — `ValidateDelete` explicit coverage

**File:** `cmd/gmc/internal/webhook/v1alpha1/actionsgateway_webhook.go`

`ValidateDelete` is a no-op but is not explicitly covered. The plan's test table mentions it should return no error.

**Test to add in `actionsgateway_webhook_test.go`:**

| Test | Assertion |
|---|---|
| `TestWebhook_DeleteNoOp` | `ValidateDelete` on any AG returns no error |

---

## Summary by priority

| Priority | Area | Tests to add |
|---|---|---|
| **High — bug** | `buildNoProxy` merge-vs-replace | 4 |
| **High** | `buildNetworkPolicy` DNS + AGC/worker egress | 3 |
| **High** | `HTTPGitHubIPRangeFetcher` | 5 |
| **Medium** | `buildProxyServiceAddr` format | 2 |
| **Medium** | Untested resource builders | 11 |
| **Medium** | `ListenAndServe` lifecycle | 2 |
| **Medium** | `IPRangeReconciler.Start` loop | 2 |
| **Low** | `ValidateDelete` explicit | 1 |
| **Total** | | **30** |

The `buildNoProxy` bug (§1) should be fixed before the other tests are written, since several builder tests assert `NO_PROXY` values that will change once the fix lands.
