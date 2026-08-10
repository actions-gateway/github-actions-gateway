# Q365 — Move the shared foundation out of v1-named files

**Status:** ✅ complete (2026-07-25) **Backlog:** Q365 · audit finding F6 in [structural-debt-audit-2026-07.md](../structural-debt-audit-2026-07.md)

## Goal

Relocate the version-neutral code that today lives inside v1-named files into version-neutral homes, so the later v1 removal ([Q273](../../STATUS.md#Q273)) and the classic-machinery removal ([Q264](../../STATUS.md#Q264)) become file deletions rather than extract-then-delete refactors.

Strictly behaviour-preserving: same emitted objects, same metric names and labels, same admission behaviour.
This is a move, not a redesign.

## What "version-neutral" means here, measured

The audit is dated 2026-07-20; Q360, Q364 and Q366 have reshaped these files since.
The classification below was re-derived from the current tree with an AST pass over `cmd/gmc/internal/controller` (declaration site → referencing files, comments excluded), then transitively closed over the helpers each shared entry point calls.

### GMC — `cmd/gmc/internal/controller`

The v1 deletion unit is `builder.go`, `actionsgateway_controller.go`, `cert.go`, `metrics_cert.go` and the v1 tests (these are the only non-test files that import `gmc/api/v1alpha1`, apart from `ipranges.go` and `metrics.go`, which are genuinely dual-version).

Version-neutral (moved out), grouped by the file it moves to:

| New file | Contents |
|---|---|
| `shared_util.go` | `ptr`, `fieldRef`, `toStringMapIface`, `formatResourceAttributes` |
| `shared_labels.go` | managed-by / component / app-name label constants, `copyRecommendedLabels` |
| `shared_workload.go` | container name, the three listener ports, the proxy shutdown budget, the two SecurityContext helpers, log-level and NO_PROXY defaults |
| `shared_networkpolicy.go` | DNS + metrics-scrape peer constants, `dnsEgressRule`, `githubCIDREgressRule`, `metricsScrapeIngressRule`, `agcAPIServerEgressRule`, `buildAGCNetworkPolicyFrom` |
| `shared_agc_deployment.go` | credential/vault mount constants, `agcTenantRoleName`, `agcWorkloadNames`, `buildAGCDeploymentFrom` |
| `shared_pki.go` | proxy-TLS / proxy-CA / metrics-TLS mount constants, renew windows, `metricsCertBundle`, `signLeaf`, `randSerial`, `encodeCertPEM`, `parseCertPEM` |
| `shared_quota.go` | `quotaCheck`, `proxyQuotaChecks`, `footprintFromResources`, `mulQuantity`, `findDeploymentCondition`, `proxyQuotaConditions`, `evalProxyPoolQuota`, `quotaHeadroomViolations`, `resourceListEqual`, `quotaHardChangedPredicate` |
| `shared_reconcile.go` | `provisioningError`, `errRoleRefImmutable`, `DefaultEgressStaleThreshold`, `egressStale`, `TenantNamespaceMarkerLabel`, `serviceMonitorGVK` |

Left in the v1 files because they are genuinely v1-specific — every one takes a `gmcv1alpha1.ActionsGateway`, or names a v1 singleton resource that v2 derives per-object:

- `builder.go`: the fixed v1 resource names (`agcSAName`, `workerSAName`, `proxyServiceName`, `np*Name`, the two ServiceMonitor names), `finalizerName`, `securityProfileOrDefault` + `defaultSecurityProfile` (v2 reads the namespace label instead), `componentLabels`/`managedLabels`/`proxyLabels`/`workerLabels`, `metricsServiceLabels`, `metricsServiceDNSName`, `proxyResources`, `tracingEnv`, and every `build*` that renders a v1 object.
- `actionsgateway_controller.go`: the reconciler and its methods, `proxyFootprint`, `proxyMaxReplicas`, `runnerGroupHealth`, `runnerGroupImpairingConditionsChanged`, `psaFieldManager`, `labelSafe`.
- `cert.go`: `proxyTLSSecretName`, `generateProxyCert`.
- `metrics_cert.go`: `metricsTLSSecretName`, `metricsClientSecretName`, `generateMetricsCerts`, `metricsServerSANs`.

`ipranges.go` also carried a v1-only pass.
Its v1 gateway loop and `patchNetworkPolicy` move to `ipranges_v1.go`, leaving `ipranges.go` free of `gmc/api/v1alpha1`.

`metrics.go` is left alone: its collectors interleave v1 and v2 series inside a single `Collect`, so separating them is a redesign, not a move.
Filed as Q403 and shipped separately — the v1 passes now live in `metrics_v1.go`.

### AGC — `cmd/agc/internal/listener`

`internal/listener` is the classic long-poll acquisition tier, deleted by Q264.
Four of its exported types have no classic-protocol content and are consumed by packages that outlive it (`provisioner`, `token`, `agentpool`, `usage`, the RunnerSet reconciler's ScaleSet path, `main.go`).
They move to a new leaf package `cmd/agc/internal/runnercore`:

- `Metrics` / `NewMetrics` / the `Inc*` recorder adapters — most of the metric set is emitted by the provisioner, the reapers, the agent pool and the token manager, none of which is classic.
- `EventRecorder`, `ConditionUpdater` — the non-blocking sinks; the ScaleSet listener's status sink implements both.
- `AdmitFunc` — the worker-capacity gate.
  Its signature and semantics carry no protocol detail; the gate itself lives in `provisioner/admission.go` and is wired by both the v1 RunnerGroup and the v2 RunnerSet reconcilers.

Contrary to the audit's list, **`JobHandlerFunc` stays in `listener`**: its parameters are the classic `AcquireJob` response (`runServiceURL`, `planID`, payload) and it returns a `broker.TaskResult` from the classic `broker` module.
The ScaleSet tier uses `scalesetlistener.ProvisionFunc` instead.
Moving it would relocate classic surface into a package meant to outlive classic.

## Proving behaviour unchanged

- Every move is a cut-and-paste of the declaration with its doc comment; no signature, constant value, metric name, metric label, or control flow changes.
- Both existing suites are the check: the GMC package's v1 and v2 tests (including the rendered-object assertions in `builder_test.go`, `actionsgateway_v2_test.go`, `egressproxy_builder_test.go` and the owner-reference contract pinned by `apply_helpers_ownerref_test.go`) and the AGC listener/provisioner/controller suites.
  No test expectations are edited.
- `make check` for the fast gate.

## Out of scope

- No ownerRef or finalizer-GC policy change (Q394 owns that); `apply_helpers_ownerref_test.go` must keep passing unmodified.
- No renames of moved symbols, even where the name now reads oddly (`proxyQuotaChecks` is shared with the EgressProxy); renaming would churn the diff and hide the moves.
