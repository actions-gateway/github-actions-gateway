# Kubernetes Best-Practices Audit

Findings from a project-wide Kubernetes best-practices audit performed 2026-05-30. Items are grouped by theme; `docs/STATUS.md` Queue rows reference the anchors below.

**Severity legend:** 🔴 high · 🟡 medium · 🟢 low.

**Verified-good (not flagged here):** GMC/AGC/proxy manager pods all set `runAsNonRoot` + RO rootfs + drop ALL + seccomp `RuntimeDefault`; NetworkPolicies are well-split (workload / proxy / AGC / k8s-API with documented kube-proxy DNAT notes); Secret cache `DisableFor` discipline + `WatchesMetadata` for credentials; finalizers + owner references for GC; HTTP/2 disabled on metrics/webhook (Rapid Reset CVE); admission webhook rejects privileged containers and cross-namespace SecretRef.

---

## A. Controller correctness 🔴

High-priority controller-runtime patterns whose absence causes silent staleness, lost events, or excessive API load.

| # | Sev | Finding | Location | Fix |
|---|---|---|---|---|
| A1 | 🔴 | No `EventRecorder` used anywhere — `kubectl describe ag/rg` shows no events for credential/quota/eviction failures. | both controllers | Inject `mgr.GetEventRecorderFor(...)`; emit `Warning`/`Normal` from each `setCondition` site. |
| A2 | 🔴 | GMC swallows `IsConflict` on `Status().Update`; in-memory `ag` is stale across the `Update` → `Status().Update` sequence in the deletion path. | [actionsgateway_controller.go:82](../../cmd/gmc/internal/controller/actionsgateway_controller.go), L249–L253 | Return `ctrl.Result{Requeue: true}` on conflict instead of nil; re-`Get` before the second mutation. |
| A3 | 🔴 | RunnerGroup controller has no `Owns()` / watch on worker Pods or agent Secrets — evicted pods leave `ActiveSessions` stale until the next Generation bump. | [runnergroup_controller.go:99](../../cmd/agc/internal/controller/runnergroup_controller.go) | Add `.Watches(&corev1.Pod{}, podToRunnerGroup)` filtered by the `actions-gateway/runner-group` label. |
| A4 | 🔴 | Provisioner polls pod state every 5 s via `r.Get` instead of watching — ~200 gets/s at 1,000 sessions; 5 s detection latency on a 10 s job. | [provisioner.go:597](../../cmd/agc/internal/provisioner/provisioner.go) | Replace `time.NewTicker(5s)` with a cache-backed `Watch` (single shared Pod informer filtered by `managedBy`). |
| A5 | 🟡 | GMC never explicitly sets `MaxConcurrentReconciles`; comments assert `=1` invariant but the default is implicit. | [actionsgateway_controller.go:377](../../cmd/gmc/internal/controller/actionsgateway_controller.go) | Set `.WithOptions(controller.Options{MaxConcurrentReconciles: 1})`; TODO documenting what gates raising it. |
| A6 | 🟡 | Eleven `apply*` helpers do read-modify-write without `IsConflict` handling. | [actionsgateway_controller.go:394](../../cmd/gmc/internal/controller/actionsgateway_controller.go) | Migrate to `controllerutil.CreateOrPatch`. |
| A7 | 🟡 | Credential-missing path returns `RequeueAfter: 30s` — less than default backoff after the second failure; inconsistent. | [actionsgateway_controller.go:350](../../cmd/gmc/internal/controller/actionsgateway_controller.go) | Drop the explicit RequeueAfter or align it with the rate-limited default. |
| A8 | 🟡 | RunnerGroup `mergeCondition` overwrites `LastTransitionTime` every reconcile, not only on Status transition. | [runnergroup_controller.go:336](../../cmd/agc/internal/controller/runnergroup_controller.go) | Switch to `meta.SetStatusCondition` (already used by GMC at L293). |
| A9 | 🟡 | Finalizer-add path races with deletion — can leak provisioner pool maps across reconciler restart. | [runnergroup_controller.go:130](../../cmd/agc/internal/controller/runnergroup_controller.go) | Check `DeletionTimestamp.IsZero()` before any `Update`; ensure pool cleanup runs on `NotFound`. |

## B. RBAC & cluster-wide privilege 🔴

Privilege-escalation primitives on a compromised GMC pod.

| # | Sev | Finding | Location | Fix |
|---|---|---|---|---|
| B1 | 🔴 | Manager ClusterRole grants `rbac.authorization.k8s.io: escalate` — lets a compromised GMC create Roles at any privilege level. | [role.yaml:131](../../cmd/gmc/config/rbac/role.yaml) | Drop `escalate`; keep `bind`. GMC's existing grants are already a superset of the AGC Role it creates. |
| B2 | 🔴 | `patch`/`update` on `namespaces` cluster-wide so the GMC can stamp PSA labels — also lets a compromised pod relabel `kube-system` PSA to `privileged`. | [role.yaml:8](../../cmd/gmc/config/rbac/role.yaml), [actionsgateway_controller.go:584](../../cmd/gmc/internal/controller/actionsgateway_controller.go) | Move PSA stamping behind a per-CR admission webhook on Namespace, OR add a ValidatingAdmissionPolicy gating which namespace names the GMC SA may patch. |
| B3 | 🔴 | `applyNamespacePSA` silently overwrites admin edits — no resourceVersion precondition, no event when admin intent loses. | [actionsgateway_controller.go:584](../../cmd/gmc/internal/controller/actionsgateway_controller.go) | Use server-side apply with a distinct field manager; emit an Event on conflict reconciliation. |
| B4 | 🟡 | AGC `secrets:list` returns full Secret bodies (including GitHub App credential) on every list. | [builder.go:99](../../cmd/gmc/internal/controller/builder.go) | Restrict to labeled `PartialObjectMetadata` watch + per-name `Get`, or `resourceNames` matching the agent-pool naming pattern. |

## C. Worker / proxy pod security defaults 🔴

The operator-created pods (worker pods from AGC, proxy pods from GMC) are the highest-blast-radius surface. PSA `baseline` (the project default) does **not** enforce `runAsNonRoot` or seccomp.

| # | Sev | Finding | Location | Fix |
|---|---|---|---|---|
| C1 | 🔴 | Worker pods get no default `SecurityContext` stamped — `runAsNonRoot`, RO rootfs, capability drop, seccomp absent unless tenant `PodTemplate` provides them. The webhook only rejects `privileged: true`. | [provisioner.go:464](../../cmd/agc/internal/provisioner/provisioner.go) | Default a restrictive Pod- and container-level SecurityContext into `buildPod` before merging tenant overrides; let `SecurityProfile=privileged` opt out. |
| C2 | 🔴 | Worker pods have no default resource requests/limits when the tenant template omits them → Best-Effort, evictable, burns the eviction-retry budget fast. | [provisioner.go:464](../../cmd/agc/internal/provisioner/provisioner.go) | Default `500m/1Gi` requests + limits when the tenant template omits them. |
| C3 | 🟡 | No `seccompProfile: RuntimeDefault` on worker pods — blocks any future PSA `restricted` upgrade. | [provisioner.go:464](../../cmd/agc/internal/provisioner/provisioner.go) | Stamp `RuntimeDefault` as default; allow explicit override. |
| C4 | 🟡 | Worker labels missing `app.kubernetes.io/{name,instance,component,part-of}` recommended labels. | [provisioner.go:572](../../cmd/agc/internal/provisioner/provisioner.go) | Add the recommended-labels set so k9s/Prometheus relabel rules work out of the box. |

## D. CRD design polish 🟡

Mostly cosmetic + future-proofing; no security implications except D5.

| # | Sev | Finding | Location | Fix |
|---|---|---|---|---|
| D1 | 🟡 | No printer column for `observedGeneration` / Reason on RunnerGroup — operators can't see generation skew. | [runnergroup_types.go:117](../../cmd/agc/api/v1alpha1/runnergroup_types.go) | Add `+kubebuilder:printcolumn` for `ObservedGen` and `Reason`. |
| D2 | 🟡 | `RunnerGroupStatus.Conditions` missing `+listType=map` / `+listMapKey=type` markers — server-side apply treats as atomic list, causing churn. | [runnergroup_types.go:100](../../cmd/agc/api/v1alpha1/runnergroup_types.go) | Mirror the markers already on `ActionsGatewayStatus.Conditions` (L78–L80). |
| D3 | 🟡 | `ProxyReadyReplicas`, `ActiveSessions`, `ObservedGeneration` lack `+optional` / `omitempty` — always serialized as `0` on new objects. | [actionsgateway_types.go:81](../../cmd/gmc/api/v1alpha1/actionsgateway_types.go) | Mark `+optional`; add `,omitempty` JSON tags. |
| D4 | 🟡 | No `categories=actions-gateway` on either CRD — `kubectl get all -l ...` and group selectors don't pick up the CRs. | both CRDs | Add the category. |
| D5 | 🟡 | No immutability / CEL `XValidation` on `gitHubAppRef.name` or `securityProfile` — silent security downgrades (e.g. `restricted` → `baseline`) possible. | [actionsgateway_types.go:56](../../cmd/gmc/api/v1alpha1/actionsgateway_types.go), L72 | Add CEL rule pinning the field, or a webhook that rejects/audits downgrade. |
| D6 | 🟡 | `RunnerLabels` has no `MinItems=1` or per-item pattern — empty list silently matches every workflow. | [runnergroup_types.go:49](../../cmd/agc/api/v1alpha1/runnergroup_types.go) | `+kubebuilder:validation:MinItems=1` + items `Pattern`. |
| D7 | 🟢 | No conversion-webhook scaffolding for `v1alpha1` — fine today, will matter at v1beta1 graduation. | both CRDs | Stub `Hub`/`Convertible` interfaces. |

## E. Manifest defaults & HA 🟡

The shipped kustomize bases are not HA-by-default and disable several secure-by-default integrations behind comments.

| # | Sev | Finding | Location | Fix |
|---|---|---|---|---|
| E1 | 🟡 | Manager Deployment `replicas: 1`, no PDB, no PriorityClass — single node drain halts all GMC reconciliation. Leader election is already wired, so 2 replicas is safe. | [manager.yaml:24](../../cmd/gmc/config/manager/manager.yaml) | Bump to `replicas: 2`; ship a PDB `minAvailable: 1`; `priorityClassName: system-cluster-critical`. |
| E2 | 🟡 | No `startupProbe` on GMC manager; cold cluster cache-sync can race the liveness probe. | [manager.yaml:87](../../cmd/gmc/config/manager/manager.yaml) | Add `startupProbe` with `failureThreshold: 30 periodSeconds: 5`. |
| E3 | 🟡 | Metrics endpoint uses controller-runtime self-signed cert; cert-manager `[METRICS-WITH-CERTS]` block commented out. Prometheus scrape requires `insecure_skip_verify`. | [kustomization.yaml:25](../../cmd/gmc/config/default/kustomization.yaml) | Enable the cert-manager block; ship the Certificate. |
| E4 | 🟡 | `../prometheus` (ServiceMonitor) commented out by default — no out-of-box Prom scraping. | [kustomization.yaml:26](../../cmd/gmc/config/default/kustomization.yaml) | Enable by default. |
| E5 | 🟡 | `../network-policy` commented out by default — violates the project's secure-by-default principle. | [kustomization.yaml:34](../../cmd/gmc/config/default/kustomization.yaml) | Enable by default. |
| E6 | 🟡 | Proxy anti-affinity is `PreferredDuringScheduling` — 2 replicas can co-locate on one node, defeating the PDB. | [builder.go:303](../../cmd/gmc/internal/controller/builder.go) | Switch to `RequiredDuringScheduling`. Document the dev-cluster trade-off. |
| E7 | 🟡 | Proxy + AGC Deployments have no `terminationGracePeriodSeconds` — CONNECT tunnels truncated on rollout; AGC listener-renewals lose lock on SIGKILL. | [builder.go:292](../../cmd/gmc/internal/controller/builder.go), L472 | Set `terminationGracePeriodSeconds: 60`; ensure signal handlers drain. |
| E8 | 🟡 | HPA `MaxReplicas` allows 100 per tenant with no per-cluster guard. | [actionsgateway_types.go:33](../../cmd/gmc/api/v1alpha1/actionsgateway_types.go) | Lower hard CRD max (e.g. 30), or admission webhook correlating with namespace quota. |
| E9 | 🟡 | Proxy CPU limit 100m throttles before HPA's 60% util signal trips. | [builder.go:263](../../cmd/gmc/internal/controller/builder.go) | Raise limit (e.g. 500m), or remove CPU limit while keeping memory limit. |

## F. Observability & operational 🟡

| # | Sev | Finding | Location | Fix |
|---|---|---|---|---|
| F1 | 🟡 | Two logger libraries: `slog` (application logs) + `zap` (manager logs) → incompatible JSON schemas in aggregators. | [main.go:84](../../cmd/gmc/cmd/main.go), [actionsgateway_controller.go:21](../../cmd/gmc/internal/controller/actionsgateway_controller.go) | Bridge `slog` ↔ `logr`; pick one shape and stick to it. |
| F2 | 🟡 | No OpenTelemetry tracing — Reconcile → provision → waitForPodCompletion spans would be operationally invaluable. | codebase-wide | Add OTel tracer init in `cmd/agc/main.go`; span the provisioner path. |
| F3 | 🟡 | GMC `terminationGracePeriodSeconds: 10` shorter than default 15 s leader lease → ~5 s dead reconcile per rollout. | [manager.yaml:111](../../cmd/gmc/config/manager/manager.yaml) | Bump to 30 s, or set `LeaderElectionReleaseOnCancel: true` (line 183 main.go) and keep 10 s. |
| F4 | 🟡 | Leader-election lease/retry/renew flags not exposed for tuning. | [main.go:61](../../cmd/gmc/cmd/main.go) | Expose `--leader-elect-lease-duration` etc. |
| F5 | 🟡 | AGC hard-codes `zap.UseDevMode(true)` — pretty colored logs in production. | [main.go:47](../../cmd/agc/main.go) | Read `--zap-devel` flag or `LOG_DEV` env. |
| F6 | 🟡 | AGC has no `HealthProbeBindAddress` wired and the AGC Deployment carries no liveness/readiness probes — wedged AGC is invisible. | [main.go](../../cmd/agc/main.go), [builder.go:518](../../cmd/gmc/internal/controller/builder.go) | Wire `HealthProbeBindAddress: ":8081"`, register `healthz.Ping`; stamp probes on the AGC Deployment. |

## G. Supply chain 🟡

| # | Sev | Finding | Location | Fix |
|---|---|---|---|---|
| G1 | ✅ | ~~`ghcr.io/actions/actions-runner:2.327.1` not pinned to a digest.~~ Pinned 2026-06-01 to `@sha256:551dc313…`; bump procedure documented in Dockerfile header. | `cmd/worker/Dockerfile` | — |
| G2 | 🟡 | No `org.opencontainers.image.*` labels on any Dockerfile — SBOM scanners miss provenance. | all four Dockerfiles | Add `source`/`revision`/`version`/`licenses` labels. |
| G3 | 🟢 | `go build` missing `-trimpath -ldflags=-buildid=` for reproducibility (SLSA-L3 friendly). | all four Dockerfiles | Add the flags. |

## H. Test coverage 🟡

These fold into existing test plans rather than warranting new plans.

| # | Sev | Finding | Where it fits |
|---|---|---|---|
| H1 | 🟡 | No envtest suite for the RunnerGroup controller — finalizer-ordering bug A9 would slip. | [archive/milestone-2-tests.md](archive/milestone-2-tests.md) (add gap row). |
| H2 | 🟡 | No e2e covering a real GitHub `rerun-failed-jobs` POST on eviction. | [milestone-3-tests.md](milestone-3-tests.md) (item H2 in that plan is the rerun-API 5xx case; add a Tier-C live happy-path companion). |
