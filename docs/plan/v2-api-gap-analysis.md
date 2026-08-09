# v2 ↔ v1 gap analysis (API + controller behavior)

**Type:** ⓘ Review / evidence record. **Date:** 2026-07-14.
**Status (2026-08-09):** every gap recorded below has since shipped, and the last
v2 API milestone (M4 cross-namespace sharing, Q166) landed 2026-08-08. The audit's
own scope is therefore closed. One capability drop survives it:

- **Multi-label runner sets** ([Q726](../STATUS.md#Q726)). `v1alpha1` sets
  `MinItems=1` with no ceiling; `v2beta1` CEL-enforces `size(self) == 1` because
  the single label doubles as the scale-set name. Deliberate, and the field's
  godoc offers a migration path (stay on a `v2alpha1` Classic RunnerSet), but
  that path expires with `v2alpha1` at `v2.0.0`
  ([Q264](../STATUS.md#Q264)), which is what makes it permanent.

A second one opened and closed inside the same release. Q683 and Q691 shipped in
August wired only into the classic `provision()` path, and `v2beta1` is
ScaleSet-only, so for a few days `v2` tenants had neither; Q766 ported both and
1.4 ships them on both tiers. It is worth recording because it is the shape
[04-operational-flows.md](../design/04-operational-flows.md) calls a silent
capability deletion, the one Q417 and Q443 were ported to avoid, and it reappeared
without anyone deciding to reopen it.

The doc is kept as the evidence record and, above all, as the list of
[intentional differences](#intentional-differences--verified-no-action) so future
audits do not re-litigate them.
**Scope:** the v1 API (`actions-gateway.github.com/v1alpha1`: GMC `ActionsGateway` + AGC `RunnerGroup`) versus the v2 API (`actions-gateway.com` — `v2alpha1` served/reconciled, `v2beta1` storage/hub: `ActionsGateway`, `EgressProxy`, `RunnerSet`, `RunnerTemplate`/`ClusterRunnerTemplate`), covering both the API surface and what each controller/webhook actually does. Goal: find v1 behavior not yet ported to v2, now that v2 is the recommended front door (Q273) and v1 removal is on the deprecation clock (Q264/Q273).

**Method:** field-by-field API comparison plus four parallel end-to-end code reads — GMC gateway reconcilers, proxy provisioning (v1 inline vs v2 `EgressProxy`), the full admission surface (webhooks + VAPs + CEL), and the AGC `RunnerGroup` vs `RunnerSet` controllers. Every finding below was verified against the code at the cited locations; the security-relevant ones were re-confirmed by hand.

**Outcome:** 8 genuine gaps found. The most severe — the missing `noProxyCIDRs` GitHub-bypass admission validation — was fixed same-day in [#641](https://github.com/actions-gateway/github-actions-gateway/pull/641), and its GHES-host residual (was Q322) is closed by the referrer-aware follow-up (both webhook sides thread the referring gateways' `gitHubURL` hosts); the other 7 are filed as [Q323–Q329](../STATUS.md#queue). Q319/Q321 were already on the Queue and are confirmed. Everything else is at parity, a v2 superset, or an intentional, documented design difference.

## Summary of gaps

| Queue | Gap | Severity |
|---|---|---|
| [#641](https://github.com/actions-gateway/github-actions-gateway/pull/641) (fixed) + Q322 residual (fixed) | `noProxyCIDRs` GitHub-bypass admission validation missing in v2 (doc claimed it existed) | **Security regression** |
| Q323 (fixed) | v2 admission drops three v1 guards: `gitHubURL` structural check, reserved-namespace rejection, PriorityClass VAP backstop | Security (defense-in-depth) |
| Q324 (fixed) | v2 proxy metrics-mTLS stack + per-tenant ServiceMonitors never landed (M2→M3a deferral fell through) | Observability |
| Q325 (fixed) | ScaleSet acquisition path (the default) surfaces no failure conditions/events | Operability (bug class) |
| Q326 (fixed) | No ResourceQuota watch on the v2 `EgressProxy`/`RunnerSet` reconcilers; Ready FQDN-mode proxy never requeues | Staleness bug |
| Q327 (fixed) | Per-proxy `logLevel` knob dropped from the v2 API | API parity |
| Q328 (fixed) | v2 gateway teardown not fail-closed (Q125 parity); `MaxConcurrentReconciles=1` not set | Robustness |
| Q329 (fixed) | Stale doc claims + chart-only v2 RBAC (no kubebuilder markers) | Docs / hygiene |
| Q321 (fixed) | v2 ActionsGateway condition gauges (`runnersets_degraded` twin + `agc_available`/`egress_unattributed`) | Observability |

<a id="admission"></a>
## Admission surface

v2beta1 has no validators of its own — the conversion webhook + `matchPolicy=Equivalent` route v2beta1 writes through the v2alpha1 validators, so every verdict here covers both v2 versions.

**Gap — `noProxyCIDRs` GitHub-bypass rejection (FIXED in [#641](https://github.com/actions-gateway/github-actions-gateway/pull/641); GHES residual FIXED by the Q322 follow-up).** v1's webhook rejects any `spec.proxy.noProxyCIDRs` entry that would route GitHub traffic around the per-tenant proxy — the public GitHub host set plus the tenant's own `gitHubURL` host, with Go httpproxy suffix semantics (`cmd/gmc/internal/webhook/v1alpha1/actionsgateway_webhook.go:463-522`). As found, the v2 `EgressProxy` webhook validated FQDN backend, destination allowlists, and scheduling priorityClass only, with no CEL rule either — yet the field's godoc asserted "rejected by the GMC admission path": a v2 tenant could set `noProxyCIDRs: ["github.com"]` and silently defeat per-tenant egress-IP attribution. #641 (merged the same day, from this audit's task chip) extracted the v1 guard into a shared `noproxy` package and wired it into the EgressProxy validator, covering the public GitHub host set. The residual was exactly the porting caveat noted here: `EgressProxy` carries no `gitHubURL`, so an entry matching a referring gateway's GHES host still passed admission until the referrer's host was threaded through. The Q322 follow-up closed it from **both** webhook sides (`gmc/internal/webhook/v2alpha1/noproxy_referrers.go`): the `EgressProxy` write resolves its referrers' `gitHubURL` hosts (gateways via `defaultProxyRef`, `RunnerSet`s via `proxyRef` through their gateway), and the `ActionsGateway`/`RunnerSet` write validates its GitHub host against the referenced proxy's `noProxyCIDRs` — so the bypass pair is rejected regardless of creation order.

**Gap — three dropped v1 guards (Q323, FIXED).**
- *`gitHubURL` structural validation:* v1 validates https/host/org-path-segment in the webhook (`v1alpha1/actionsgateway_webhook.go`); as found, v2 kept only `Pattern=^https://` + the (new, good) immutability CEL — `https://x` with no org segment was admitted. Fixed by extracting the v1 check into the shared `gmc/internal/webhook/validation` package (so it survives the planned v1 sunset) and running it in the v2 ActionsGateway webhook on create and update.
- *Reserved-namespace create rejection:* v1 rejects an ActionsGateway created in `kube-system`/`kube-public`/`gmc-system`/POD_NAMESPACE; as found, no v2 kind had an equivalent check. Fixed via the same shared package, wired into the two v2 kinds that make the GMC provision workloads into their namespace: `ActionsGateway` (AGC control plane) and `EgressProxy` (proxy Deployment/NetworkPolicies). `RunnerSet`/`RunnerTemplate` deliberately carry no guard — a RunnerSet is fail-closed (`GatewayNotFound`) without a same-namespace gateway and a template is inert, so blocking gateway+proxy blocks the chain.
- *PriorityClass VAP backstop:* `priorityclass-allowlist-guard.yaml` matched only v1 `runnergroups`; v2 `runnersets`/`runnertemplates` relied on the `failurePolicy=fail` webhooks alone, so stored-object re-validation and webhook-outage defense-in-depth were v1-only. Fixed by adding an `actions-gateway.com` `v2alpha1`+`v2beta1` `runnersets`/`runnertemplates` rule to both policy copies (the shared has()-guarded CEL variables are total across all three shapes); `ClusterRunnerTemplate` stays exempt as platform-authored, matching its webhook exemptions. Covered live by the extended VAP envtest.

**Parity (relocated or equal):** secret-ref namespace confinement (v2 `LocalSecretReference` makes it structurally impossible); securityProfile downgrade gate + privileged eligibility (v1 webhook → `namespace-security-profile-guard` VAP, dual-domain, no invariant weakened); worker PriorityClass allowlist on both routes (priorityTiers + podTemplate.priorityClassName) including the Q188 ConfigMap-watched augmentation, shared across v1/v2 webhooks; privileged-container rejection (RunnerTemplate webhook; ClusterRunnerTemplate deliberately exempt as platform-authored); runnerLabels/threshold/duration CEL identical across v1/v2alpha1/v2beta1; tenant-resource-guard + namespace-psa-guard dual-read both domains and match `runnersets`.

**Intentional drops:** the v1 singleton-per-namespace rule (superseded by multi-gateway; replaced by ScaleSet runner-label uniqueness per gateway, `runnerset_webhook.go:133-176`).

**v2-only additions:** infra PriorityClass allowlist on `spec.scheduling` (Q284, EgressProxy + v2 gateway), destination FQDN/CIDR platform allowlists, FQDN-mode-requires-backend fail-closed check, reserved proxy-env rejection in templates.

<a id="gmc-gateway"></a>
## GMC gateway reconciler (v1 `ActionsGateway` vs v2)

The v2 reconciler (`cmd/gmc/internal/controller/actionsgateway_v2_controller.go`, operating on v2alpha1) provisions the same AGC control-plane child set through the shared `buildAGCDeploymentFrom`/`buildAGCNetworkPolicyFrom` assembly: SA/worker-SA/RoleBinding/metrics-certs/AGC Service/both NetworkPolicies/Deployment all at parity, plus v2-only additions (GATEWAY_NAME scoping, WorkloadIdentity credential env, `agcResources` default+overlay — v1 stamps no resources — `spec.scheduling`, direct-egress GitHub rule, Vault egress peer). Conditions: Ready/AGCAvailable/Degraded/CredentialUnavailable at parity; proxy-pool conditions moved to `EgressProxy` (Q320); `RunnerGroupsDegraded` → `RunnerSetsDegraded` (Q304); v2 adds `EgressUnattributed`/`proxyMode`. Events are a strict v2 superset (the two PSA events moved to `NamespacePSAReconciler`).

**Gap — teardown + serialization (Q328, CLOSED).** As found: v1's delete path is fail-closed — explicit deletion of every child, errors collected, finalizer retained + `TeardownIncomplete` event + requeue until confirmed (Q125; `actionsgateway_controller.go:382-475`) — while v2 deleted only the cluster-scoped ClusterRoleBinding then removed the finalizer immediately, trusting owner-ref cascade GC, and left `MaxConcurrentReconciles` at the default where v1 sets 1 deliberately. Both halves are now ported: the v2 `reconcileDelete` deletes every child explicitly *and verifies each is gone* (a delete error or a child lingering under a foreign finalizer retains the finalizer + emits `TeardownIncomplete`; metrics Secrets stay owner-ref-GC'd since the GMC holds no delete verb on Secrets), and the v2 builder sets `MaxConcurrentReconciles: 1`. Design note: [appendix-h §H.8](../design/appendix-h-v2-api-decomposition.md#h8-ownership-gc-and-deletion); operator doc: [troubleshooting](../operations/troubleshooting.md#actionsgateway-stuck-deleting-teardown-blocked-on-a-failing-delete).

**Gap — ServiceMonitors (part of Q324, FIXED).** As found, v1 optionally provisions per-tenant proxy + AGC ServiceMonitors (`applyOrPruneServiceMonitors`, `EnableTenantServiceMonitors` flag, graceful missing-CRD handling) while the v2 reconciler created the AGC metrics Service but no ServiceMonitor and had no flag equivalent. Fixed for the proxy: the v2 `EgressProxy` reconciler now provisions a `<ep>-proxy-metrics` ServiceMonitor gated by the same `--enable-tenant-service-monitors` flag (`EnableServiceMonitor`), with the same missing-CRD downgrade (Warning event, never blocks provisioning) and owner-ref GC. The AGC-side v2 ServiceMonitor remains out of scope here (Q324 is the proxy metrics parity item).

**Intentional:** `spec.runnerGroups` bootstrapping dropped (RunnerSets are independent, gatewayRef-referencing CRs); PSA stamping relocated to `NamespacePSAReconciler` keyed on the namespace `security-profile` label (both directions covered; downgrade protection in the VAP).

<a id="proxy"></a>
## Proxy provisioning (v1 inline `spec.proxy` vs v2 `EgressProxy`)

Parity confirmed on: Deployment/HPA/PDB with byte-identical resource defaults, self-signed cert generation + rotation window, GitHub-CIDR egress NetworkPolicy incl. the Q229 NodeLocal-DNS third peer, fail-closed empty-IP-cache behavior, 24h IP-range refresh + `EgressRulesStale` (`IPRangeReconciler` patches v2 EgressProxy NPs when `V2Enabled`), NO_PROXY auto-append via shared `buildNoProxy`, Q283 HPA-revert-safe apply, quota condition math, Q320 v2-aware GMC gauges, and the built-in cross-node podAntiAffinity (v2 makes it an overridable default per Q282). v2-only supersets: FQDN egress modes, destination allowlists + CONNECT-allowlist env, scheduling passthrough, richer Events, multiple proxies per namespace. Cross-namespace sharing was deferred at audit time (M4/Q166) with `spec.sharing` inert; it **shipped 2026-08-08**, enforcing provider-side consent with ConfigMap CA distribution and dual-side NetworkPolicy ([§H.9](../design/appendix-h-v2-api-decomposition.md#h9-cross-namespace-proxy-sharing)).

**Gap — metrics-mTLS stack (Q324, FIXED).** As found, v1 wired the proxy metrics listener end to end (`PROXY_METRICS_*` env + TLS volume + `metrics` container/Service port + metrics-scrape NP ingress rule + ServiceMonitor) while the v2 builder omitted all of it, documented as "lands in M3a" — but M3a and M3b shipped without it, so a v2 proxy pool's `/metrics` fell back to plaintext on the health port and was unscrapable. Fixed: the v2 `EgressProxy` now mounts a per-`EgressProxy` metrics-mTLS bundle (`<ep>-metrics-tls`), publishes the scraper client bundle (`<ep>-metrics-client`), exposes the `metrics` container/Service port, threads all three `PROXY_METRICS_*` env (the mTLS gate — this removes the plaintext fallback), and admits the `:8443` scrape from `metrics: enabled` namespaces. Each `EgressProxy` owns its own metrics CA — no cross-tenant reuse, no security regression vs classic. Design: [appendix-h §H.8](../design/appendix-h-v2-api-decomposition.md#h8-ownership-gc-and-deletion); operator doc: [observability-metrics-access.md](../operations/observability-metrics-access.md#v2-egressproxy-proxy-metrics).

**Gap — `logLevel` (Q327, FIXED).** v1 threads `spec.logLevel` to the proxy container as `LOG_LEVEL` (`builder.go:693-699`); as found, the `EgressProxy` API had no logLevel field and the v2 container got no LOG_LEVEL env — no per-proxy debug knob on v2. Dropped by API design without a documented decision. Fixed by adding `spec.logLevel` to the `EgressProxy` CRD (both v2 versions, identity-converted) with the v1 enum/default (`info`|`debug`, default `info`) and threading it into the v2 deployment builder; the migration tool now also stamps the v1 gateway-level `logLevel` onto the emitted `EgressProxy` so a migrated tenant keeps its proxy verbosity.

**Gap — quota-watch staleness (Q326, shared with the AGC below; FIXED).** v1 watches ResourceQuota with a `.spec.hard`-changed predicate so quota conditions refresh promptly. As found, the `EgressProxyReconciler` had no ResourceQuota watch, and a Ready FQDN-mode proxy got `ctrl.Result{}` with a zero recheck interval — its `ProxyQuota*` conditions could stay stale until an unrelated child event. The Q326 fix mirrors the v1 watch (shared `quotaHardChangedPredicate` + `quotaToEgressProxies` fan-out) and extends the Ready-state recheck requeue to the managed FQDN modes (same `threshold/8` cadence), which also gives the unwatched CNI-native FQDN policy a periodic drift re-check.

<a id="agc"></a>
## AGC controller (v1 `RunnerGroup` vs v2 `RunnerSet`)

The runtime machinery is shared through owner-agnostic seams (provisioner `Target`, `assembleListenerConfig`, `reapWorkerPodsByLabel`, `buildPod`), so worker pod build (including the build-time reserved-field overrides — still enforced even though v2 also rejects at admission), every lifecycle knob (eviction/quota retries, completedPodTTL, pendingPodDeadline, scaleUp token bucket), priority-tier selection, status counts, and reconciler-emitted events are at parity, with v2 adding the whole reference-resolution layer (fail-closed `GatewayNotFound`/`TemplateNotFound`/`AmbiguousDefault`/`ProxyNotFound`, `templateSource`/`proxyMode`, server-side gatewayRef watch scoping) and Q303/Q308/Q249 conditions.

**Gap — ScaleSet path surfaces no failures (Q325; FIXED).** `Degraded`, `RateLimited`, and `RunnerVersionTooOld` conditions (and the `RunnerVersionTooOld`/`SessionUnauthorized` events) are produced inside the *classic* listener goroutine (`listener/goroutine.go:434,679-688`) and reached the RunnerSet only on the classic path; as found, `scalesetlistener/listener.go` never called `SetCondition` — so on the **default** acquisition protocol these failure classes were invisible: the set stayed Ready while e.g. GitHub rate-limited the queue. Adjacent to (not covered by) Q311's metrics/alerting scope and Q309's vocabulary cleanup. The fix wires owner-bound `Conditions`/`Events` sinks into the scale-set listener (mirroring its `MetricsRecorder` seam) reusing the reconciler's existing condition/event drain channels: sustained-429 polling (ten-minute window, classic parity) surfaces `RateLimited=True/SustainedRateLimit`, and an unauthorized session create/refresh surfaces `Degraded=True/Unauthorized` plus a `SessionUnauthorized` Warning event once per episode. Beyond classic parity the listener publishes the healthy baseline on start and clears an abnormal condition on recovery (new `PollingHealthy`/`SessionAuthorized` False-reasons in both v2 packages). `RunnerVersionTooOld` is documented as classic-only — the scale-set protocol carries no runner version at session creation, so the class cannot occur.

**Gap — classic listener never cleared recovered failure conditions (Q332; FIXED).** The reverse of the Q325 gap: the *classic* listener goroutine only ever set the abnormal (True) states — a recovered `RateLimited` sat stale until the process restarted, and a `Degraded` surfaced by a prior failed instance was never cleared. The Q332 fix ports the ScaleSet listener's clear-on-recovery + healthy-baseline pattern to the classic path (`listener/goroutine.go`): the healthy baseline (`Degraded=False/SessionAuthorized`, `RateLimited=False/PollingHealthy`) is published on session start, and the first successful poll after a sustained-429 episode clears `RateLimited=False/PollingHealthy`. The `PollingHealthy`/`SessionAuthorized` False-reasons now exist in v1alpha1 too (value-parity with the v2 packages pinned by `conditions_parity_test.go`).

**Gap — ResourceQuota watch (Q326; FIXED).** v1 `RunnerGroupReconciler` watches ResourceQuota (`runnergroup_controller.go:169-173`); as found, the `RunnerSetReconciler` did not, so the Q303 quota conditions lagged an admin's quota edit until an unrelated event. The Q326 fix adds the same watch (`quotaToRunnerSets` + the shared `quotaHardChangedPredicate`), proven in envtest.

**Gap — condition gauges (pre-existing Q319/Q321).** The `worker_quota_pressure`/`worker_quota_exceeded`/`workers_unschedulable` collectors List only `RunnerGroupList` and register only in the v1 reconciler (`runnergroup_controller.go:160-161`); no `RunnerSetsDegraded` gauge exists either. Already tracked; the analysis confirms both. **Both have since FIXED:** Q321 landed the gateway-level gauges, and Q319 exported the v2 RunnerSet worker-capacity conditions as gauges, with Q643 adding the `WorkerCapacityDeclined` reason label.

**Minor (Q329):** ~~v2alpha1 lacks `ConditionRateLimited`/`ConditionRunnerVersionTooOld` constants — the classic path writes raw v1 strings onto RunnerSet status~~ — resolved with Q309, which declared the classic-listener vocabulary in both v2 packages (value-parity + mirror-sync tests) and documented it; ~~v2 AGC RBAC lives only in chart files (`charts/actions-gateway/files/agc-*-rules.yaml`) with no `+kubebuilder:rbac` markers, a drift risk relative to v1's generated role~~ — closed with Q329 (see the Minor section below).

<a id="observability"></a>
## Observability roll-up

Cross-cutting view of the gaps above plus what was on the Queue at audit time. **All of them have since closed:** v2 proxy metrics-mTLS and the per-`EgressProxy` ServiceMonitor (Q324); the gateway-level condition gauges (Q321) and the RunnerSet worker-capacity gauges (Q319, extended by Q643's reason label); the ScaleSet tier's missing failure conditions and events (Q325); its alerts and dashboards (Q311); and the dead v2 condition vocabulary (Q309).

The audit's closing verdict, that an operator running pure-v2 had status conditions and scale-set counters but almost no alertable Prometheus surface for them, no longer holds: [observability-alerting.md](../operations/observability-alerting.md) ships scale-set alert rules, and both shipped dashboards carry the tier.

<a id="minor"></a>
## Minor / stale-doc findings (Q329)

- ~~`cmd/agc/api/v1alpha1/runnergroup_types.go:59-60` claims "the controller sets a Degraded condition if [tiers] are not [ascending]"~~ — resolved with Q329: the doc (and the `07-test-plan.md` CEL-rejection claim) now state that strictly-ascending tier order is a caller contract, not enforced by admission or a status condition (only the CEL last-threshold==maxWorkers rule exists).
- ~~`api/v2beta1/egressproxy_types.go:93-100` noProxyCIDRs doc/code contradiction~~ — resolved by [#641](https://github.com/actions-gateway/github-actions-gateway/pull/641), which implemented the check and re-scoped the doc to name the GHES residual (Q322).
- ~~v2 status godocs list stale "Known types" sets~~ — resolved with Q329: `RunnerSetStatus` now lists all nine condition types set on it and `ActionsGatewayStatus` adds `EgressUnattributed`/`RunnerSetsDegraded`, in both v2alpha1 and v2beta1.
- ~~v2 AGC RBAC lives only in the chart files with no `+kubebuilder:rbac` markers~~ — resolved with Q329: markers on `cmd/agc/internal/controller/doc.go` now mirror the chart's `agc-tenant-role`/`agc-clusterrunnertemplate-reader` v2 grants so the generated `agc-role` no longer drifts (the chart's deliberate v1 withholds — runnergroup create/delete, secret patch — are unchanged).

## Intentional differences — verified, no action

For the record, so future audits don't re-litigate them: proxy decomposed to `EgressProxy` (with `ProxyAvailable`/`ProxyReadyReplicas`/quota/staleness signals moving there — Q320); `spec.runnerGroups` bootstrap dropped for independent RunnerSets; `securityProfile` relocated to the namespace label + `NamespacePSAReconciler` + VAP; singleton-per-namespace dropped for multi-gateway; `SecretReference.namespace` deleted (`LocalSecretReference`); `maxListeners` v2beta1 drop (ScaleSet-only, Q264) while v2alpha1 keeps it transitional (default 10); proxy env omitted from the AGC in Direct mode (§H.10); ClusterRunnerTemplate privileged-allowed (platform-authored); RunnerSet credential loss surfaced as `Ready=False/TokenUnavailable` instead of a per-set `CredentialUnavailable`; v1 `status.activeSessions` on the gateway was never set by the GMC in v1 either, so its absence from v2 loses nothing; legacy-name cleanup paths dropped (v2 is greenfield).
