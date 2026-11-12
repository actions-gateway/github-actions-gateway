# Upgrade and Rollback Procedures

> **Audience:** Platform engineer

For upgrade strategy intent, see [§2.6 of the architecture doc](../design/02-architecture.md#26-upgrade-strategy).
For **initial installation** of the GMC, see [install.md](install.md) — this document covers day-2 upgrade and rollback.

The three independently versioned components — GMC, AGC, and worker image — each upgrade on their own cadence with separate procedures below.

---

## Table of Contents

- [Pre-Upgrade Validation Checklist](#pre-upgrade-validation-checklist)
  - [Before upgrading to v2.0.0: the cluster runs Kubernetes 1.35 or later](#before-upgrading-to-v200-the-cluster-runs-kubernetes-135-or-later)
  - [Before upgrading to v2.0.0: no EgressProxy still names a deprecated FQDN alias](#before-upgrading-to-v200-no-egressproxy-still-names-a-deprecated-fqdn-alias)
  - [Before upgrading to v2.0.0: every actions-gateway.com CRD stores only v2](#before-upgrading-to-v200-every-actions-gatewaycom-crd-stores-only-v2)
  - [Before upgrading to v2.0.0: no v1alpha1 object is left](#before-upgrading-to-v200-no-v1alpha1-object-is-left)
  - [Before upgrading to v2.0.0: every tenant namespace carries the actions-gateway.com labels](#before-upgrading-to-v200-every-tenant-namespace-carries-the-actions-gatewaycom-labels)
- [Migration Notes](#migration-notes)
  - [Action required before v2.0.0: v2 is the storage version, and stored objects need rewriting](#action-required-before-v200-v2-is-the-storage-version-and-stored-objects-need-rewriting)
  - [Action required before v2.0.0: a Classic RunnerSet now runs on the scale-set protocol](#action-required-before-v200-a-classic-runnerset-now-runs-on-the-scale-set-protocol)
  - [Action required at v2.0.0: remove the CRDs' conversion webhook before applying them](#action-required-at-v200-remove-the-crds-conversion-webhook-before-applying-them)
  - [Action required at v2.0.0: apply the CRDs with the upgrade, because the AGC's watch depends on them](#action-required-at-v200-apply-the-crds-with-the-upgrade-because-the-agcs-watch-depends-on-them)
  - [Action required after v2.0.0: delete what an owner deleted during the upgrade left behind](#action-required-after-v200-delete-what-an-owner-deleted-during-the-upgrade-left-behind)
  - [Action required if you alert on or chart the classic acquisition series: they are no longer exported](#action-required-if-you-alert-on-or-chart-the-classic-acquisition-series-they-are-no-longer-exported)
  - [Action required if a RunnerSet runs fork pull request jobs: they are now refused](#action-required-if-a-runnerset-runs-fork-pull-request-jobs-they-are-now-refused)
  - [Action required if controllers and workers share nodes: the controllers now keep off worker nodes](#action-required-if-controllers-and-workers-share-nodes-the-controllers-now-keep-off-worker-nodes)
  - [Action required if a RunnerSet's priorityTiers do not ascend: the list is now rejected](#action-required-if-a-runnersets-prioritytiers-do-not-ascend-the-list-is-now-rejected)
  - [Action required if a query reads tier on a disruption series: the label is gone](#action-required-if-a-query-reads-tier-on-a-disruption-series-the-label-is-gone)
  - [Action required if spec.tracing.endpoint does not parse as a URL: the value is now rejected](#action-required-if-spectracingendpoint-does-not-parse-as-a-url-the-value-is-now-rejected)
  - [Action required if spec.tracing sets insecure: true with an https:// endpoint: the pair is now rejected](#action-required-if-spectracing-sets-insecure-true-with-an-https-endpoint-the-pair-is-now-rejected)
  - [Action required if a noProxyCIDRs entry exempts GitHub through a comma, wildcard, port or address: the entry is now rejected](#action-required-if-a-noproxycidrs-entry-exempts-github-through-a-comma-wildcard-port-or-address-the-entry-is-now-rejected)
  - [Action required if a shared EgressProxy exempts a consumer's GitHub host: the pair is now rejected](#action-required-if-a-shared-egressproxy-exempts-a-consumers-github-host-the-pair-is-now-rejected)
  - [Action required if a networkPolicy peer names a broad or link-local cidr: the value is now rejected](#action-required-if-a-networkpolicy-peer-names-a-broad-or-link-local-cidr-the-value-is-now-rejected)
  - [Action required if a gateway names a GitHub Enterprise Server host: an FQDN-mode pool reaches it only when the platform allowlists it](#action-required-if-a-gateway-names-a-github-enterprise-server-host-an-fqdn-mode-pool-reaches-it-only-when-the-platform-allowlists-it)
  - [Action required if a githubURL spells public GitHub www.github.com or api.github.com: a new gateway is rejected, and a stored one shares scale-set names with github.com](#action-required-if-a-githuburl-spells-public-github-wwwgithubcom-or-apigithubcom-a-new-gateway-is-rejected-and-a-stored-one-shares-scale-set-names-with-githubcom)
  - [Action required if a namespaced RunnerTemplate goes past Pod Security baseline: the shape is now rejected](#action-required-if-a-namespaced-runnertemplate-goes-past-pod-security-baseline-the-shape-is-now-rejected)
  - [Action required if a namespaced RunnerTemplate declares an nfs, iscsi or similar volume: the type is now rejected](#action-required-if-a-namespaced-runnertemplate-declares-an-nfs-iscsi-or-similar-volume-the-type-is-now-rejected)
  - [Action required if a template projects a ServiceAccount token: the source is now rejected, and removed from worker pods](#action-required-if-a-template-projects-a-serviceaccount-token-the-source-is-now-rejected-and-removed-from-worker-pods)
  - [Action required if a namespaced `RunnerTemplate` sets `nodeName` or `schedulerName`: both are now rejected, and cleared from worker pods](#action-required-if-a-namespaced-runnertemplate-sets-nodename-or-schedulername-both-are-now-rejected-and-cleared-from-worker-pods)
  - [Action required if you overlay a library template: deploy/templates/ is written at v2](#action-required-if-you-overlay-a-library-template-deploytemplates-is-written-at-v2)
  - [Action required: the GMC reads Secrets only in marked tenant namespaces](#action-required-the-gmc-reads-secrets-only-in-marked-tenant-namespaces)
  - [Non-breaking: the GMC's ConfigMap, CNI policy and ServiceMonitor writes are confined to tenant namespaces](#non-breaking-the-gmcs-configmap-cni-policy-and-servicemonitor-writes-are-confined-to-tenant-namespaces)
  - [Action required if a Throughput RunnerSet sets maxRequests.memory: its memory limit now stops there](#action-required-if-a-throughput-runnerset-sets-maxrequestsmemory-its-memory-limit-now-stops-there)
  - [Non-breaking: one repository's out-of-memory kills no longer fill the scale-set failure ledger](#non-breaking-one-repositorys-out-of-memory-kills-no-longer-fill-the-scale-set-failure-ledger)
  - [Action required if a tenant namespace selects privileged without the eligibility label: the GMC can no longer stamp it](#action-required-if-a-tenant-namespace-selects-privileged-without-the-eligibility-label-the-gmc-can-no-longer-stamp-it)
  - [Non-breaking: deleting a gateway keeps its workers' NetworkPolicy until they are gone](#non-breaking-deleting-a-gateway-keeps-its-workers-networkpolicy-until-they-are-gone)
  - [Action required if you copied the webhook-rejection alert or imported the security dashboard: both now see the -v2 webhooks](#action-required-if-you-copied-the-webhook-rejection-alert-or-imported-the-security-dashboard-both-now-see-the--v2-webhooks)
  - [Non-breaking: two `RunnerSet`s may share a name across namespaces](#non-breaking-two-runnersets-may-share-a-name-across-namespaces)
  - [Non-breaking: a worker deleted while still pulling its image is force-cancelled, then re-run](#non-breaking-a-worker-deleted-while-still-pulling-its-image-is-force-cancelled-then-re-run)
  - [Non-breaking: `EgressRulesStale` now fires when no GitHub ranges have loaded](#non-breaking-egressrulesstale-now-fires-when-no-github-ranges-have-loaded)
  - [Non-breaking: a direct-egress v2 gateway reports `EgressRulesStale` and pages on it](#non-breaking-a-direct-egress-v2-gateway-reports-egressrulesstale-and-pages-on-it)
  - [Non-breaking: the GMC keeps GitHub egress across a restart](#non-breaking-the-gmc-keeps-github-egress-across-a-restart)
  - [Non-breaking: a reaped scale-set worker whose job ran elsewhere recovers the job left without a runner](#non-breaking-a-reaped-scale-set-worker-whose-job-ran-elsewhere-recovers-the-job-left-without-a-runner)
  - [Non-breaking: a worker whose node shuts down before it starts is force-cancelled, then re-run](#non-breaking-a-worker-whose-node-shuts-down-before-it-starts-is-force-cancelled-then-re-run)
  - [Non-breaking: a worker on a dead node you taint out-of-service is re-run automatically](#non-breaking-a-worker-on-a-dead-node-you-taint-out-of-service-is-re-run-automatically)
  - [A worker evicted for its own storage limit is no longer re-run](#a-worker-evicted-for-its-own-storage-limit-is-no-longer-re-run)
  - [Non-breaking: the scale-set re-run budget survives an AGC restart, and one run attempt is re-run once](#non-breaking-the-scale-set-re-run-budget-survives-an-agc-restart-and-one-run-attempt-is-re-run-once)
  - [Non-breaking: the worker-image registry read presents the worker ServiceAccount's pull secrets](#non-breaking-the-worker-image-registry-read-presents-the-worker-serviceaccounts-pull-secrets)
  - [Non-breaking: a scale-set worker that never started a job no longer cancels or re-runs its namesake's run](#non-breaking-a-scale-set-worker-that-never-started-a-job-no-longer-cancels-or-re-runs-its-namesakes-run)
  - [Non-breaking: a scale-set worker's run identity follows the job its runner started](#non-breaking-a-scale-set-workers-run-identity-follows-the-job-its-runner-started)
  - [Non-breaking: an idle scale-set worker left by a job no runner started is reclaimed](#non-breaking-an-idle-scale-set-worker-left-by-a-job-no-runner-started-is-reclaimed)
  - [Non-breaking: a scale-set worker's JIT-config Secret is now owned by its pod](#non-breaking-a-scale-set-workers-jit-config-secret-is-now-owned-by-its-pod)
  - [Non-breaking: v2 is served beside v2beta1, and an unpinned read now returns v2](#non-breaking-v2-is-served-beside-v2beta1-and-an-unpinned-read-now-returns-v2)
  - [Non-breaking: the actions-gateway.com validating webhooks now validate v2, and their names end in -v2](#non-breaking-the-actions-gatewaycom-validating-webhooks-now-validate-v2-and-their-names-end-in--v2)
  - [A new CiliumFQDN / CalicoFQDN EgressProxy is now rejected at admission](#a-new-ciliumfqdn--calicofqdn-egressproxy-is-now-rejected-at-admission)
  - [Non-breaking: a drained worker's recovery claim moves off the pod into a second ConfigMap](#non-breaking-a-drained-workers-recovery-claim-moves-off-the-pod-into-a-second-configmap)
  - [Non-breaking: a `spec.scaleUp` token is now charged per worker pod, not per delivered job](#non-breaking-a-specscaleup-token-is-now-charged-per-worker-pod-not-per-delivered-job)
  - [Non-breaking: `spec.scaleUp` now withholds intake instead of delaying jobs it has already claimed](#non-breaking-specscaleup-now-withholds-intake-instead-of-delaying-jobs-it-has-already-claimed)
  - [Non-breaking: job_duration_seconds now measures worker pod lifetime (the classic-tier span shrinks)](#non-breaking-job_duration_seconds-now-measures-worker-pod-lifetime-the-classic-tier-span-shrinks)
  - [Non-breaking: deleting a cancelled run's worker no longer re-queues the job](#non-breaking-deleting-a-cancelled-runs-worker-no-longer-re-queues-the-job)
  - [Non-breaking: an abandoned run is force-cancelled and re-run automatically, on both acquisition tiers](#non-breaking-an-abandoned-run-is-force-cancelled-and-re-run-automatically-on-both-acquisition-tiers)
  - [Non-breaking: v1alpha1 is deprecated and the apiserver now warns](#non-breaking-v1alpha1-is-deprecated-and-the-apiserver-now-warns)
  - [Non-breaking: an EgressProxy pool's pods drop the app: actions-gateway-proxy label (its pool is recreated once)](#non-breaking-an-egressproxy-pools-pods-drop-the-app-actions-gateway-proxy-label-its-pool-is-recreated-once)
  - [Non-breaking: GitHub Enterprise Server gateways now reach their own appliance (they never did)](#non-breaking-github-enterprise-server-gateways-now-reach-their-own-appliance-they-never-did)
  - [Non-breaking: a GHES appliance behind a private CA can now be trusted (`spec.githubCABundleRef`)](#non-breaking-a-ghes-appliance-behind-a-private-ca-can-now-be-trusted-specgithubcabundleref)
  - [Agent-identity collisions are now rejected at admission (RunnerSet name, and derived RunnerGroup name)](#agent-identity-collisions-are-now-rejected-at-admission-runnerset-name-and-derived-runnergroup-name)
  - [Scale-set name uniqueness is now enforced across the whole GitHub scope, not per namespace](#scale-set-name-uniqueness-is-now-enforced-across-the-whole-github-scope-not-per-namespace)
  - [Non-breaking: a worker lost while the AGC was down is now re-run automatically (cause="vanished")](#non-breaking-a-worker-lost-while-the-agc-was-down-is-now-re-run-automatically-causevanished)
  - [Non-breaking: drained and hand-deleted workers are now re-run automatically (cause="deletion")](#non-breaking-drained-and-hand-deleted-workers-are-now-re-run-automatically-causedeletion)
  - [Non-breaking: an evicted job's auto-re-run now lands (GitHub refused it before)](#non-breaking-an-evicted-jobs-auto-re-run-now-lands-github-refused-it-before)
  - [Non-breaking: eviction auto-retry now honours GITHUB_API_BASE_URL (it never did on GHES)](#non-breaking-eviction-auto-retry-now-honours-github_api_base_url-it-never-did-on-ghes)
  - [Non-breaking: preempted workers are now re-run automatically; eviction counters gain a cause label](#non-breaking-preempted-workers-are-now-re-run-automatically-eviction-counters-gain-a-cause-label)
  - [Non-breaking: classic-tier eviction auto-retry now fires (it never did against real GitHub)](#non-breaking-classic-tier-eviction-auto-retry-now-fires-it-never-did-against-real-github)
  - [BREAKING (pre-GA): capacityGate.mode values replaced by Observe + a gateway-level cluster fact](#breaking-pre-ga-capacitygatemode-values-replaced-by-observe--a-gateway-level-cluster-fact)
  - [Non-breaking: an over-long derived RunnerGroup name is bounded (and renamed)](#non-breaking-an-over-long-derived-runnergroup-name-is-bounded-and-renamed)
  - [Non-breaking: a RunnerSet's agent Secrets and runner names gain an rs- prefix](#non-breaking-a-runnersets-agent-secrets-and-runner-names-gain-an-rs--prefix)
  - [Non-breaking: v2alpha1 is deprecated and the apiserver now warns](#non-breaking-v2alpha1-is-deprecated-and-the-apiserver-now-warns)
  - [Non-breaking: v2alpha1 CRDs ship in a separate, opt-in chart](#non-breaking-v2alpha1-crds-ship-in-a-separate-opt-in-chart)
  - [BREAKING: spec.namespaceQuota removed — the ResourceQuota is now platform-owned](#breaking-specnamespacequota-removed--the-resourcequota-is-now-platform-owned)
  - [BREAKING: priorityTiers PriorityClasses now require a platform allowlist; per-tier preemptionPolicy removed](#breaking-prioritytiers-priorityclasses-now-require-a-platform-allowlist-per-tier-preemptionpolicy-removed)
  - [Tenant namespaces now require the actions-gateway.github.com/tenant marker label](#tenant-namespaces-now-require-the-actions-gatewaygithubcomtenant-marker-label)
  - [Worker quota accounting now counts native sidecars, RuntimeClass overhead, and storage](#worker-quota-accounting-now-counts-native-sidecars-runtimeclass-overhead-and-storage)
  - [Worker pods are now cleaned up automatically (one-time sweep recommended)](#worker-pods-are-now-cleaned-up-automatically-one-time-sweep-recommended)
  - [AGC Deployment renamed from actions-gateway-agc to actions-gateway-controller](#agc-deployment-renamed-from-actions-gateway-agc-to-actions-gateway-controller)
  - [GMC manager NetworkPolicy is now enabled by default](#gmc-manager-networkpolicy-is-now-enabled-by-default)
- [GMC Upgrade](#gmc-upgrade)
  - [A GMC restart costs tenants nothing; a GMC upgrade rolls every one of them](#a-gmc-restart-costs-tenants-nothing-a-gmc-upgrade-rolls-every-one-of-them)
  - [GMC install and upgrade via Helm (recommended)](#gmc-install-and-upgrade-via-helm-recommended)
  - [PriorityClass allowlist: ConfigMap to CR](#priorityclass-allowlist-configmap-to-cr)
  - [GMC post-upgrade validation](#gmc-post-upgrade-validation)
  - [GMC rollback](#gmc-rollback)
- [AGC Upgrade](#agc-upgrade)
  - [Per-Tenant Upgrade Procedure](#per-tenant-upgrade-procedure)
  - [AGC rollback](#agc-rollback)
- [Proxy Upgrade](#proxy-upgrade)
  - [Step 1: Pre-Upgrade Checks](#step-1-pre-upgrade-checks)
  - [Step 2: Update the Proxy Image](#step-2-update-the-proxy-image)
  - [Step 3: Watch the Rollout](#step-3-watch-the-rollout)
  - [Step 4: Post-Upgrade Validation](#step-4-post-upgrade-validation)
  - [Proxy rollback](#proxy-rollback)
- [Worker Image Upgrade](#worker-image-upgrade)
  - [Upgrade Procedure](#upgrade-procedure)
  - [Canary Testing a New Worker Image](#canary-testing-a-new-worker-image)
  - [Minimum Version Requirement](#minimum-version-requirement)
  - [Worker image rollback](#worker-image-rollback)
- [Post-Upgrade Validation](#post-upgrade-validation)
- [Zero-Downtime Configuration](#zero-downtime-configuration)

## Pre-Upgrade Validation Checklist

Before upgrading any component, confirm the system is healthy:

```sh
# 1. No active incidents or RateLimited conditions
kubectl get actionsgateway --all-namespaces

# 2. All AGC pods healthy
kubectl get pods --all-namespaces -l app.kubernetes.io/name=actions-gateway-controller

# 3. All proxy pools healthy — the recommended label covers v1 inline pools and v2
#    EgressProxy pools alike; `app=actions-gateway-proxy` finds only v1's.
kubectl get pods --all-namespaces -l app.kubernetes.io/name=actions-gateway-proxy

# 4. No CrashLoopBackOff pods
kubectl get pods --all-namespaces | grep -v Running | grep -v Completed | grep -v Terminating

# 5. No recent reconcile errors
# Metric: rate(controller_runtime_reconcile_errors_total[5m]) == 0
```

### Before upgrading to `v2.0.0`: the cluster runs Kubernetes 1.35 or later

`v2.0.0` raises both charts' floor from 1.30 to 1.35, the version CI tests.
**Upgrade the cluster first**, before any other step in this guide:

```sh
kubectl version
```

The `v1.10.0` chart's floor is 1.30, so Helm accepts it while the cluster moves, and the `v1.10.0` to `v2.0.0` walk below was measured on Kubernetes 1.36.

**If the cluster will pass through 1.33 or 1.34, or is on one now, set this first, or `v1.10.0` workers fail to start:**

```sh
kubectl -n gmc-system set env deployment/gmc-controller-manager WRAPPER_DELIVERY=init
```

A `v1.10.0` AGC delivers the worker wrapper through an image volume on any server from 1.33, but upstream leaves the `ImageVolume` feature gate off until 1.35.
With the gate off, the apiserver drops the volume with no error, and the runner's command then points at a file that is not there.
The variable has the GMC tell each AGC to use an initContainer instead, and the AGCs restart to pick it up.
The chart has no value for it and `helm upgrade` leaves it set, so remove it once the cluster is at 1.35 or later:

```sh
kubectl -n gmc-system set env deployment/gmc-controller-manager WRAPPER_DELIVERY-
```

Measured on kind with Kubernetes 1.34.11 and the `v1.10.0` charts: the AGC logged `imageVolume: true`, a Pod with its worker's volume, mount and command stored neither the volume nor the mount and failed with exit 128, and the initContainer form reached the wrapper.
The variable reached the AGC Deployment and rolled it, and a `helm upgrade` kept it.
Measured on kind with Kubernetes 1.35.5, the gate was on and the same Pod kept its volume and reached the wrapper.
No job ran through a worker the AGC created, and neither 1.33 nor a managed provider's gate setting was measured.

Below the floor the upgrade stops part-way, which is why this comes first.
Measured on kind with Kubernetes 1.34.11 and Helm 4.2.3:

| Step | Result |
|---|---|
| `make validate-cluster` | Fails, naming the server version and the floor |
| `helm upgrade` | Fails with `UPGRADE FAILED: chart requires kubeVersion`, and the release stays at its current revision |
| `helm install` | Fails the same way |
| `helm template` of the CRD chart, applied with `kubectl` | **Succeeds** |

The last row is the trap.
`helm template` checks the floor against the version Helm was built for rather than your cluster, and `kubectl apply` of the release asset has no floor to check, so the CRDs go on and `helm upgrade` then refuses.
That leaves a `v1.10.0` control plane on `v2.0.0` CRDs in [the gap the upgrade order keeps short](#action-required-at-v200-apply-the-crds-with-the-upgrade-because-the-agcs-watch-depends-on-them), for as long as the cluster upgrade takes.

### Before upgrading to `v2.0.0`: no `EgressProxy` still names a deprecated FQDN alias

`v2.0.0` removes the deprecated `CiliumFQDN` and `CalicoFQDN` values of `EgressProxy.spec.egressPolicyMode`, along with every API version that defines them ([why](v1alpha1-deprecation.md#the-ciliumfqdn--calicofqdn-aliases-ride-the-v200-clock)).
`v2` cannot represent one, so `v2.0.0` stores an unmigrated pool as plain `FQDN`, enforced by whatever `--fqdn-policy-backend` names rather than the backend its alias pinned.

Run this against the version that can still represent the aliases, and expect **no output**:

```sh
kubectl get egressproxies.v2beta1.actions-gateway.com -A -o jsonpath='{range .items[?(@.spec.egressPolicyMode=="CiliumFQDN")]}{.metadata.namespace}/{.metadata.name}{"\t"}{.spec.egressPolicyMode}{"\n"}{end}{range .items[?(@.spec.egressPolicyMode=="CalicoFQDN")]}{.metadata.namespace}/{.metadata.name}{"\t"}{.spec.egressPolicyMode}{"\n"}{end}'
```

Each line it prints is a pool to migrate before the upgrade: set `egressPolicyMode: FQDN` on the `EgressProxy` and have the platform operator set the matching GMC `--fqdn-policy-backend` (`cilium` or `calico`).
The enforced policy is identical either way, so this is a re-label rather than a change in what is enforced.

Two details the command depends on:

- **Pin the version.** `kubectl get egressproxies` resolves to the preferred served version, which becomes `v2` in 1.9, and `v2` shows a stored alias as `FQDN`.
  The unpinned form therefore prints nothing on a cluster that still has pools to migrate.
- **Two `range` blocks, not one filter.** `kubectl`'s JSONPath has no `||` in a filter expression, so a single combined filter fails to parse rather than matching nothing.
  Measured 2026-09-24 on the `kubectl` 1.36 client, which rejects the combined form with `unrecognized character in action: U+007C '|'`.

### Before upgrading to `v2.0.0`: every `actions-gateway.com` CRD stores only `v2`

`v2.0.0` stops serving `v2beta1`, `v2alpha1` and the other versions `v2` replaces, and the apiserver refuses to remove a version from a CustomResourceDefinition (CRD) whose `status.storedVersions` still lists it.
Run this and expect `[v2]` on every line:

```sh
kubectl get crd actionsgateways.actions-gateway.com egressproxies.actions-gateway.com \
  runnersets.actions-gateway.com runnertemplates.actions-gateway.com \
  clusterrunnertemplates.actions-gateway.com priorityclassallowlists.actions-gateway.com \
  -o custom-columns='NAME:.metadata.name,STORED:.status.storedVersions'
```

Any other value means the cluster still holds objects written at an older version: run [the storage sweep](#action-required-before-v200-v2-is-the-storage-version-and-stored-objects-need-rewriting) first.

### Before upgrading to `v2.0.0`: no `v1alpha1` object is left

`v2.0.0` has no `actions-gateway.github.com` controller, so migrate every tenant with [`gag-migrate`](migration-v1-to-v2.md) and [decommission v1](migration-v1-to-v2.md#step-4--decommission-v1-when-ready) while your 1.x release still runs.
The `v2.0.0` chart no longer ships the two `v1alpha1` CRDs, and Helm keeps them (`helm.sh/resource-policy: keep`), so a leftover object stays stored with nothing reconciling it.
A `RunnerGroup`'s finalizer then never clears, and its agent Secrets and GitHub registrations are never cleaned up.
Run this and expect `No resources found`:

```sh
kubectl get actionsgateways.actions-gateway.github.com,runnergroups.actions-gateway.github.com -A
```

After upgrading, delete the two CRDs the chart left behind:

```sh
kubectl delete crd actionsgateways.actions-gateway.github.com runnergroups.actions-gateway.github.com
```

### Before upgrading to `v2.0.0`: every tenant namespace carries the `actions-gateway.com` labels

`v2.0.0` stops honouring the `actions-gateway.github.com` namespace keys that 1.x accepted beside their replacements:

| 1.x also accepted | `v2.0.0` honours only |
|---|---|
| label `actions-gateway.github.com/tenant: "true"` | label `actions-gateway.com/tenant: managed` |
| label `actions-gateway.github.com/privileged-profile: allowed` | label `actions-gateway.com/privileged-profile: allowed` |
| annotation `actions-gateway.github.com/allow-profile-downgrade: "true"` | annotation `actions-gateway.com/allow-profile-downgrade: allowed` |

A namespace carrying only the old tenant marker is no longer a tenant namespace once the `v2.0.0` GMC starts.
The GMC stops watching Secrets there, so a gateway in it cannot read its credential, and the `gmc-tenant-resource-guard` policy denies every write the GMC makes there, deletes included, so its teardown cannot finish either.
The GMC's Secret *access* ends with it: the GMC deletes the `gmc-tenant-secret-reader` RoleBinding 1.x created there, a write the guard admits outside a tenant namespace.
A namespace whose privileged grant is only on the old key loses it, and the `namespace-security-profile-guard` policy rejects the next change that selects `privileged` there.
A tenant migrated with [`gag-migrate`](migration-v1-to-v2.md) already carries both spellings and needs nothing.

Run these and expect `No resources found` from each:

```sh
kubectl get namespaces -l 'actions-gateway.github.com/tenant=true,actions-gateway.com/tenant!=managed'
kubectl get namespaces -l 'actions-gateway.github.com/privileged-profile=allowed,actions-gateway.com/privileged-profile!=allowed'
```

A `!=` selector also matches a namespace with no such label, so each line lists exactly the namespaces holding the old key without its replacement.
Label each one it prints, as an administrator:

```sh
kubectl label namespace <namespace> actions-gateway.com/tenant=managed
kubectl label namespace <namespace> actions-gateway.com/privileged-profile=allowed
```

The downgrade annotation is read only on the update that lowers a profile, so nothing breaks at upgrade; replace the old spelling in any manifest or GitOps source that sets it.

Also check the release notes for the new version before upgrading, particularly:
- CRD schema changes (new required fields, removed fields, validation tightening).
- Behavior changes that require configuration updates before the new binary takes effect.

---

## Migration Notes

Each note is written as of its release, so one older than `v2.0.0` can name a version or a kind that has since been removed.

### Action required before `v2.0.0`: `v2` is the storage version, and stored objects need rewriting

**Who is affected:** every cluster with the v2 CRDs installed.

**What changed.** Every `actions-gateway.com` kind now stores `v2`, the six in both charts alike.
The apiserver writes the new version only on the next write, so an object stored at `v2beta1` stays that way until something rewrites it, and each CRD's `status.storedVersions` keeps listing `v2beta1` beside `v2`.
`v2.0.0`'s CRDs remove `v2beta1`, and the apiserver rejects that apply while `storedVersions` lists it.
This release ships the rewrite as `gag-migrate storage-version`, in the same signed `gag-migrate` binary as the [v1 → v2 migration](migration-v1-to-v2.md).

**What you will see.** Nothing changes for any client: every version stays served, and the conversion webhook converts between them as before.
After the CRD apply, `storedVersions` reads `["v2beta1","v2"]`, and can list `v2alpha1` too on a cluster that has run since before `v2beta1` existed.
An `EgressProxy` that still names `CiliumFQDN` or `CalicoFQDN` is stored from its next write as `egressPolicyMode: FQDN` with the alias in the annotation `conversion.actions-gateway.com/egress-policy-mode`, and a `v2beta1` read still returns the alias, so its pool keeps the backend the alias pinned.

**What to do.** Any time after applying this release's CRDs, and before upgrading to `v2.0.0`:

1. Run the [pre-upgrade alias check](#before-upgrading-to-v200-no-egressproxy-still-names-a-deprecated-fqdn-alias) and migrate every pool it prints.
   `v2.0.0` cannot represent an alias, so the sweep refuses to start while one exists, names each pool, and writes nothing.
2. Preview the sweep, which writes nothing and prints each CRD's `storedVersions` and how many objects it would rewrite:

   ```sh
   gag-migrate storage-version --context <your-cluster-context>
   ```

3. Run it, and confirm the prompt that echoes the target context:

   ```sh
   gag-migrate storage-version --context <your-cluster-context> --apply
   ```

   It writes every object of each kind back unchanged, which makes the apiserver store it at `v2`, then sets `storedVersions` to `["v2"]`.
   A CRD already reading `["v2"]` is skipped, so a second run is safe.
4. Confirm with the [storedVersions check](#before-upgrading-to-v200-every-actions-gatewaycom-crd-stores-only-v2).

The sweep needs `list` and `update` on every `actions-gateway.com` kind cluster-wide and `update` on `customresourcedefinitions/status`: run it as a cluster administrator, as you apply the CRDs.
Every write passes through the GMC's validating webhooks, so run it with the GMC up.
If the apiserver refuses an object, for example because a validating webhook rejects it, the sweep finishes the other objects, names each refused one, leaves that object's CRD listing the old version, and exits non-zero.
Fix the named objects and run it again.

**Rolling back** to the 1.9 release is safe, because 1.9 serves `v2` and so reads what this release stored.
Rolling back further is not: a release that does not serve `v2` cannot read an object stored at it.

### Action required before `v2.0.0`: a `Classic` RunnerSet now runs on the scale-set protocol

**Who is affected:** a `RunnerSet` written at `v2alpha1` with `spec.acquisitionProtocol: Classic`.
`v2beta1` and `v2` never served the field, so a set written at either already runs on the scale-set protocol.

**What changed.** The AGC no longer has the classic acquisition tier (Q264).
A set that asked for `Classic` registers a scale set named after its first `runnerLabels` entry and takes jobs through it, like every other set, and its `maxListeners` is ignored.
The GMC's admission check that no two sets in one GitHub scope share a first label now applies to it too.

It also leaves the set's classic agents behind.
On 1.10, deleting a `Classic` set deregistered its agents from GitHub and deleted their Secrets; from `v2.0.0` nothing does, so the `agentpool-rs-<set>-<n>` Secrets stay in the namespace and the `rs-<set>-<n>` runners stay registered, offline, at GitHub.
How long GitHub keeps an offline runner of this kind has not been measured here.

**Before you upgrade,** list the sets that still carry the choice, which the conversion keeps in an annotation:

```bash
kubectl get runnersets.actions-gateway.com -A -o json \
  | jq -r '.items[] | select(.metadata.annotations["conversion.actions-gateway.com/acquisition-protocol"] == "Classic") | "\(.metadata.namespace)/\(.metadata.name)"'
```

Replace each one while still on 1.10: create a second `RunnerSet` with the same `runnerLabels` and `acquisitionProtocol: ScaleSet` (or none, at `v2beta1` or `v2`), then delete the `Classic` set once its running jobs finish.
On 1.10 that delete deregisters the set's agents and deletes their Secrets (`runnerset_controller.go` `reconcileDelete`, at the `v1.10.0` tag), which is the cleanup nothing does after the upgrade.

A `Classic` set you keep through the upgrade needs two checks.
First, its first label must not be the first label of another set whose gateway names the same GitHub org, enterprise or repository: two such sets would register one scale set and both drive it, and admission does not re-check sets that already exist, so rename one set's first label before upgrading.
A pair that slips through shows after the upgrade as the `ScaleSetNameCollision` condition on the gateway, and fires [`ActionsGatewayScaleSetNameCollision`](runbook.md#actionsgatewayscalesetnamecollision).
Second, after the upgrade its classic agents need removing by hand.
List their Secrets:

```bash
kubectl get secrets -n <namespace> -o name | grep '^secret/agentpool-rs-<set>-'
```

Delete each one it prints, and remove the offline `rs-<set>-<n>` runners in the org's or repository's runner settings.

**Rolling back** to 1.10 returns the set to classic acquisition.

### Action required at `v2.0.0`: remove the CRDs' conversion webhook before applying them

**Who is affected:** every cluster on the 1.x CRD chart whose conversion `caBundle` was set by cert-manager, which is the default, or by a `kubectl patch`.

**What changed.** The `v2.0.0` CRDs serve `v2` alone and carry no `spec.conversion`.
Server-side apply removes only the fields its own field manager owns, and the `caBundle` belongs to whichever manager set it, so the apply leaves it behind, and the apiserver refuses a `caBundle` on a CRD with no webhook strategy.

**What you see.** `kubectl apply --server-side` refuses all five CRDs, with or without `--force-conflicts`, and the refused CRDs stay as they were:

```text
CustomResourceDefinition.apiextensions.k8s.io "runnersets.actions-gateway.com" is invalid: [spec.conversion.strategy: Required value, spec.conversion.webhookClientConfig: Forbidden: should not be set when strategy is not set to Webhook]
```

**What to do.** Run [the `storedVersions` check](#before-upgrading-to-v200-every-actions-gatewaycom-crd-stores-only-v2) first, and stop unless every line reads `[v2]`.
The patch below changes the cluster and the apply can still refuse, so anything that would refuse it has to be settled while the webhook is still there.
Pull the chart at this point too, [as the next section says](#action-required-at-v200-apply-the-crds-with-the-upgrade-because-the-agcs-watch-depends-on-them), so a wrong chart version also fails with nothing changed.

Then download the release, so a wrong tag fails with nothing changed, remove the webhook from each CRD, and apply straight away, in the same shell:

```sh
curl -fsSLo actions-gateway-crds-v2.yaml \
  https://github.com/actions-gateway/github-actions-gateway/releases/download/vX.Y.Z/actions-gateway-crds-v2.yaml
for crd in actionsgateways clusterrunnertemplates egressproxies runnersets runnertemplates; do
  kubectl patch crd "${crd}.actions-gateway.com" --type=merge \
    -p '{"spec":{"conversion":{"strategy":"None","webhook":null}}}'
done
kubectl apply --server-side -f actions-gateway-crds-v2.yaml
```

Between the patch and the apply `v2alpha1` and `v2beta1` are still served with nothing converting them, so a read at either returns the stored `v2` object relabelled, without converting it.
Measured on kind with Kubernetes 1.36.1 and cert-manager v1.20.2: the CA injector did not restore the webhook during a 60-second pause, the apply then succeeded, and the CRDs served `v2` alone.

**If the apply is refused after the patch.** A failure that is not a refusal, such as a timeout, only needs the apply run again.
After a refusal the cluster is half upgraded, and an object still stored at an old version reads unconverted, so do not run the storage sweep in this state.
Measured the same way with `status.storedVersions` reading `["v2beta1","v2"]` on `runnersets`: the apply exited 1, the other four CRDs served `v2` alone, and `runnersets` served all three versions with strategy `None`.

Go back by applying the CRDs of the 1.x release you came from.
`--force-conflicts` is required, because the plain form is refused with a conflict on `.spec.conversion.strategy`:

```sh
kubectl apply --server-side --force-conflicts -f \
  https://github.com/actions-gateway/github-actions-gateway/releases/download/v1.X.Y/actions-gateway-crds-v2.yaml
```

Measured the same way, going back from that state to the `v1.10.0` CRDs: all five returned to strategy `Webhook` serving all three versions, the CA injector restored each `caBundle` within about 15 seconds, and once `storedVersions` read `[v2]` this section's steps then succeeded.
A running `v1.10.0` GMC then converts on reads, as [GMC rollback](#gmc-rollback) records.
Fix what refused the apply, then start this section again.

### Action required at `v2.0.0`: apply the CRDs with the upgrade, because the AGC's watch depends on them

**Who is affected:** every gateway.

**What changed.** Each Actions Gateway Controller (AGC) watches only its own gateway's `RunnerSet`s, through a server-side field selector on `spec.gatewayRef.name`, and the apiserver evaluates that selector at the API version the request names.
A `v1.10.0` AGC lists at `v2alpha1`, and the `v1.10.0` CRD declares the field there alone; a `v2.0.0` AGC lists at `v2`, and `v2` is the only version the `v2.0.0` CRD serves.
So neither AGC can list its `RunnerSet`s against the other release's CRDs.

**What you see.** A `v2.0.0` AGC on the `v1.10.0` CRDs fails its `RunnerSet` list with this error, opens no session, and exits about two minutes later with `timed out waiting for cache to be synced`:

```text
failed to list *v2.RunnerSet: field label not supported: spec.gatewayRef.name
```

**That pod reads `Ready` while it holds no session, and so does the `RunnerSet`.** The rolling update therefore removes the old AGC, and neither a finished rollout nor `kubectl get runnerset` says anything here: the set stays `Ready` `True` with reason `ListenerActive`, as last written, because no AGC can update it.
Read `actions_gateway_scaleset_session_open` instead, and treat no series the same as `0`.
Only a listener sets that gauge, so a pod that never syncs its cache should export no series, which also leaves `ActionsGatewayScaleSetNoSession` (`== 0`) quiet; that part is read from the code, not measured.

A `v1.10.0` AGC that restarts on the `v2.0.0` CRDs crash-loops at startup, because the version it looks for is gone:

```text
create manager: failed to determine if *v2alpha1.RunnerSet is namespaced: failed to get restmapping: unable to retrieve the complete list of server APIs: actions-gateway.com/v2alpha1: no matches for actions-gateway.com/v2alpha1, Resource=
```

The GMC's startup schema check does not catch either, because it probes fields rather than selectable fields.

**What to do.** Confirm [the cluster is at the floor](#before-upgrading-to-v200-the-cluster-runs-kubernetes-135-or-later), because `helm upgrade` refuses below it and the CRDs would already be on.
Pull the chart before [the webhook removal](#action-required-at-v200-remove-the-crds-conversion-webhook-before-applying-them), so a wrong version fails with nothing changed.
Then apply the five CRDs in `actions-gateway-crds-v2.yaml`, upgrade the chart, and apply the chart-root CRD, with no gap between them:

```sh
# Before the webhook removal
helm pull oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version <chart-version> --untar --untardir gag-chart-<chart-version>

# Straight after the webhook removal, whose block ends with this same apply
kubectl apply --server-side -f actions-gateway-crds-v2.yaml
helm upgrade gag gag-chart-<chart-version>/actions-gateway \
  --namespace gmc-system --reset-then-reuse-values \
  --set gmc.image.digest=sha256:<new-gmc> \
  --set agc.image.digest=sha256:<new-agc> \
  --set proxy.image.digest=sha256:<new-proxy> \
  --set wrapper.image.digest=sha256:<new-wrapper>
helm show crds gag-chart-<chart-version>/actions-gateway | kubectl apply -f -
```

Set all four digests to the new release's, because `--reset-then-reuse-values` otherwise replays the `v1.10.0` ones.

**The CRDs go first because an AGC already running survives that gap, and nothing survives the other one.** A running AGC keeps its session and takes jobs on the new CRDs, while upgrading the chart first replaces it at once with one that can do neither:

| In the gap | CRDs first | Chart first |
|---|---|---|
| The AGC | `v1.10.0` keeps running: session open, no restarts for the five minutes held | `v2.0.0` replaces it within a minute, with no session |
| A job queued in the gap | Taken within 30 seconds | Waits until the CRDs are applied |
| An AGC pod that restarts | Crash-loops with no session until the chart is upgraded | Already restarting, about every two minutes |
| Creating or editing a `RunnerSet` | Refused by the `v1.10.0` webhook, `the server could not find the requested resource` | Not measured |

So keep the gap short, and do not restart an AGC or a GMC pod inside it.
A `v1.10.0` GMC pod that starts during the gap crash-loops on these CRDs, and a running pod is not safe either: pods left alone stayed Ready for the six minutes measured, but when the leader was deleted the standby was crash-looping within about 2.5 minutes.

Whichever way the gap went, closing it recovered the gateway: the new AGC had a session within a minute, took any job that had waited, and left every existing worker pod in place.
Measured on kind with Kubernetes 1.36.1 against `test/fakegithub`, from the `v1.10.0` images to a `v2.0.0` candidate built from `main`, with workers held `Pending`; a job running through the gap and github.com itself were not measured.

A rollback crosses the same gap in reverse, so it [puts the CRDs back first](#gmc-rollback).

**The chart-root CRD goes last at this release only.** Helm builds the current release's manifest before it applies anything, that manifest names the `PriorityClassAllowlist` at `v2beta1`, and the `v2.0.0` CRD serves `v2` alone.
Applied first, as other upgrades do it, it stops the upgrade with the revision unchanged:

```text
Error: UPGRADE FAILED: unable to build kubernetes objects from current release manifest: resource mapping not found for name: "gmc-priorityclass-allowlist" namespace: "" from "": no matches for kind "PriorityClassAllowlist" in version "actions-gateway.com/v2beta1"
ensure CRDs are installed first
```

A `v1.10.0` GMC pod that starts in that state logs `failed to determine if *v2beta1.PriorityClassAllowlist is namespaced` and crash-loops.
To recover, go forward: apply the chart-root CRD of the 1.x chart you are on, run the `helm upgrade`, then apply the `v2.0.0` chart-root CRD.
Applying the 1.x CRD alone does not bring the pods back, because the five CRDs are still on `v2`.

Measured on kind with Kubernetes 1.36.1, cert-manager v1.20.2 and a local chart directory, from `v1.10.0`, with no tenant gateway: in this order every step exited 0, all six CRDs served `v2` alone, the guard denied an off-allowlist write, and a further `helm upgrade` exited 0.

### Action required after `v2.0.0`: delete what an owner deleted during the upgrade left behind

**Who is affected:** a cluster where an owner went away between the CRD apply and its `v2.0.0` controller reconciling it.
That means a `RunnerSet` whose finalizer cleared before a `v2.0.0` Actions Gateway Controller (AGC) reconciled it, an `EgressProxy`, which has no finalizer and so goes at once, or an `ActionsGateway` already being deleted when the `v2.0.0` Gateway Manager Controller (GMC) starts.

**What changed.** A `v1.10.0` AGC writes an owner reference naming `actions-gateway.com/v2alpha1` on each set's three long-lived ConfigMaps, the scale-set guard, the recovery claims and the failure ledger, and on its worker pods.
A `v1.10.0` GMC writes the same reference on the children of each `ActionsGateway` and `EgressProxy`.
The garbage collector cannot map a version the cluster no longer serves, so it leaves the object in place when the owner goes away.

Measured on kind with `kube-controller-manager` 1.36.1: such a ConfigMap was still present 12 minutes after its owner was deleted, across a controller-manager restart, while one naming `v2` was collected within 30 seconds.
An `EgressProxy`'s Deployment, Service, HorizontalPodAutoscaler, PodDisruptionBudget, NetworkPolicy and Secret behaved the same way: all six naming `v2alpha1` were still present 10 minutes after their owner was deleted, with the Deployment's pod still running, while the same six naming `v2` were gone within 45 seconds.
So a proxy Deployment and its TLS Secret can outlive their `EgressProxy`.

A `v2.0.0` AGC rewrites the three ConfigMaps' references to `v2` the first time it reconciles each set, including a set already being deleted, so nothing is needed for any set it sees.
It does not rewrite worker pods, which end with their job.
A `v2.0.0` GMC rewrites a child's reference each time it applies the child, so nothing is needed for a live gateway or proxy (read from the code, not measured).
For a gateway being deleted it deletes the children by name, except the two metrics Secrets, which it leaves to the garbage collector (also read from the code).

**What to do.** After the upgrade, once the GMC and every AGC have started, list what still names the old version:

```sh
kubectl get configmaps,pods,secrets,services,serviceaccounts,deployments,horizontalpodautoscalers,poddisruptionbudgets,networkpolicies,rolebindings -A --no-headers \
  -o custom-columns='KIND:.kind,NAMESPACE:.metadata.namespace,NAME:.metadata.name,OWNER_KIND:.metadata.ownerReferences[*].kind,OWNER:.metadata.ownerReferences[*].name,VERSION:.metadata.ownerReferences[*].apiVersion' \
  | grep 'actions-gateway.com/v2alpha1'
```

The columns carry metadata only, so no Secret data reaches the pipe.
Where the GMC manages ServiceMonitors, VerticalPodAutoscalers, `CiliumNetworkPolicy` or Calico `NetworkPolicy` objects, add those kinds to the list; they are left out because the command fails on a cluster without their CRDs.

Delete an object from this list whose owner, named in its last columns, is gone.
Deleting a Deployment takes its ReplicaSet and pods with it (measured the same way).
Rewriting its reference did not help within three minutes: measured the same way, a ConfigMap repointed to `v2` after its owner was gone was still present then.
A ConfigMap left in place also matters if you recreate a set of the same name, because the AGC finds these ConfigMaps by name (read from the code, not measured).
An object whose owner still exists needs nothing, except the agent Secrets of a set [kept from `Classic`](#action-required-before-v200-a-classic-runnerset-now-runs-on-the-scale-set-protocol), which nothing cleans up.

### Action required if you alert on or chart the classic acquisition series: they are no longer exported

**Who is affected:** anyone whose alerts, recording rules or dashboards read a series only the classic tier wrote, including copies of the reference [`PrometheusRule`](../../deploy/monitoring/prometheusrule.yaml) and Grafana dashboards taken before `v2.0.0`.

**What changed.** With the classic tier gone (Q264), the AGC no longer exports `actions_gateway_active_sessions`, `jobs_acquired_total`, `job_acquisition_errors_total`, `jobs_admission_rejected_total`, `jobs_duplicate_delivery_total`, `abandoned_delivery_completions_total`, `fanout_loser_recycle_deferred_total`, `renew_job_errors_total`, `renew_job_teardowns_total`, `agent_recycles_total`, `agent_recycle_errors_total`, `broker_token_propagation_retries_total` or `broker_session_leaks_total`.
A rule reading one of them never fires again, and a panel reading one shows `No data`, without any error to say why.

The `RunnerGroup` removal (Q273) took four more the same way, each with a twin to read instead: `actions_gateway_worker_quota_exceeded`, `worker_quota_pressure` and `workers_unschedulable` have `actions_gateway_runnerset_worker_quota_exceeded`, `runnerset_worker_quota_pressure` and `runnerset_workers_unschedulable`, and `actions_gateway_runnergroups_degraded` has `actions_gateway_runnersets_degraded`.

The reference rules drop the alerts and the recording rule that read them:

| Removed | Use instead |
| --- | --- |
| `ActionsGatewayNoActiveSessions` | [`ActionsGatewayScaleSetNoSession`](runbook.md#actionsgatewayscalesetnosession), on `actions_gateway_scaleset_session_open == 0` for 10 minutes |
| `ActionsGatewayCapacityGateRejectingJobs` | [`ActionsGatewayScaleSetCapacityWithheld`](runbook.md#actionsgatewayscalesetcapacitywithheld) |
| `ActionsGatewayRenewJobErrors`, `ActionsGatewayAgentRecycleErrors`, `ActionsGatewayFanoutFallbackTimeout`, `ActionsGatewayAbandonedDeliveryErrors` | Nothing: the AGC no longer renews jobs, re-registers agents or receives one job more than once, so there is nothing for them to measure |
| `actions_gateway:job_acquisition_success_rate:rate5m` | `actions_gateway:scaleset_provision_success_rate:rate5m` |

The reference dashboards drop the panels that read the removed series, and chart the listener session gauge where they charted active sessions.
The AGC's `Provisioner.provision` span went with the classic path too, and the scale-set path is traced as `Provisioner.ProvisionScaleSetWorker` instead, so a trace query or dashboard that filters on the old span name needs the new one ([Distributed Tracing](observability-logging.md#distributed-tracing-agc)).
A NetworkPolicy you wrote to let the AGC reach a collector keeps working, and `spec.tracing.networkPolicy` can now replace it ([Reaching the collector](observability-logging.md#reaching-the-collector)).

**Before you upgrade,** search your own rules and dashboards for the names above, and re-apply the reference `PrometheusRule` and re-import the dashboards if you run copies of them.

### Action required if a `RunnerSet` runs fork pull request jobs: they are now refused

**Who is affected:** operators with a `RunnerSet` that runs jobs from pull requests opened from a fork, which on a public repository is usually every outside contribution.

**What changed.** Every `RunnerSet` now refuses a fork pull request's job unless it sets `spec.forkPullRequests: Accept`.
A fork could already name any set's label in `runs-on`, because a `pull_request` run executes the fork's edited workflow, and nothing at GitHub refuses a job for coming from a fork.
A refused job gets no runner, its run is force-cancelled, and the set records a `ForkJobRefused` Event.
`pull_request_target` workflows and every non-pull-request event run as before.
What counts as a fork's job and what the contributor sees: [security operations](security-operations.md#fork-pull-requests-choose-their-own-runner-label).

**Before you upgrade,** decide for each set that fork pull requests reach whether it should run them.
For a set built for untrusted code, such as one on the Kata template, keep fork CI running by setting the field:

```bash
kubectl patch runnerset <name> -n <tenant-namespace> --type merge \
  -p '{"spec":{"forkPullRequests":"Accept"}}'
```

The field is new at `v2.0.0`, so patch it right after the upgraded CRDs are applied and before the new AGC takes jobs.
Patched against an older CRD, the field is pruned rather than rejected, so the patch can succeed and store nothing.
Read it back after the patch:

```bash
kubectl get runnerset <name> -n <tenant-namespace> -o jsonpath='{.spec.forkPullRequests}'
```

`Accept` is the answer that keeps fork CI; an empty answer means the set refuses fork jobs.
To show contributors why a job was refused, also grant the GitHub App `Checks: Read and write` ([onboarding](tenant-onboarding.md#0a-create-the-app-web-ui)).
Without it the refusal still happens, and the run reads only as cancelled.

### Action required if controllers and workers share nodes: the controllers now keep off worker nodes

**Who is affected:** clusters where the Gateway Manager Controller (GMC), an Actions Gateway Controller (AGC) or an egress proxy can run on a node that also runs worker pods.
That is any cluster without a separate worker pool, and every cluster with one schedulable node.

**What changed.** The GMC pod, and every AGC and egress proxy pod the GMC builds, now carry a required pod anti-affinity against worker pods in every namespace.
A worker that escapes its container can read the credentials of every pod on its node, so a controller beside it gives those up ([why](security-operations.md#keep-gags-control-plane-off-worker-nodes)).
The scheduler enforces the rule from both sides: a controller pod is not placed on a node running a worker, and a new worker is not placed on a node running a controller.
The chart value `controllerWorkerSeparation` turns it on and off, and defaults to `required`.

The upgrade rolls the GMC and, as the new GMC reconciles, every AGC and egress proxy Deployment.
Each rolls one new pod at a time and keeps the old pod serving until the new one is ready.
A new controller pod that finds no node free of workers stays `Pending`, and its rollout stops there.
Its `FailedScheduling` event names the reason `node(s) didn't match pod anti-affinity rules`.

Once the controllers are placed, a new worker that finds only controller nodes stays `Pending` with `node(s) didn't satisfy existing pods anti-affinity rules`.
On a cluster with one schedulable node, one side is always stranded.
A `helm upgrade --wait` on such a cluster times out on the GMC rollout.

**Before you upgrade,** pick one of these:

- **Run workers on their own pool**, tainted, with workers pinned to it ([procedure](security-operations.md#keep-gags-control-plane-off-worker-nodes)).
  The controllers then already sit apart from the workers, and the new rule changes nothing.
- **Keep today's placement** by setting the opt-out, which leaves every pod as it was before this release:

  ```bash
  helm upgrade <release> <chart> --namespace gmc-system --reuse-values \
    --set controllerWorkerSeparation=none
  ```

  This gives up the separation, so a worker escape can reach the controllers on its node.
  Use it on a single-node cluster, or until a separate worker pool exists.

`ActionsGateway.spec.scheduling` and `EgressProxy.spec.scheduling` cannot remove the term: it is appended after them.
The chart's `affinity` value cannot remove it either, for the same reason.

### Action required if a `RunnerSet`'s `priorityTiers` do not ascend: the list is now rejected

**Who is affected:** operators with a `RunnerSet` whose `spec.priorityTiers` thresholds are not strictly ascending, such as a tier at `threshold: 10` followed by one at `threshold: 5`, or two tiers at the same threshold.

**What changed.** The AGC gives each new worker the first tier whose threshold the active-pod count is still below, so a tier at or below an earlier one never received a pod.
With thresholds 10 then 5, every worker got the first tier's class, and the set's real ceiling was 10 although `maxWorkers`, which has to equal the last threshold, said 5.
The schema now refuses such a list.
The upgrade itself changes nothing: a set already stored with one keeps running, and an edit that leaves `priorityTiers` alone is still accepted.
A change to the list that leaves it out of order is rejected, and so is a delete and re-create, which includes a restore from backup.

**Before you upgrade,** list the sets whose thresholds do not ascend:

```bash
kubectl get runnersets.actions-gateway.com -A -o json \
  | jq -r '.items[] | select((.spec.priorityTiers // []) as $t | any(range(1; $t | length); $t[.].threshold <= $t[. - 1].threshold)) | "\(.metadata.namespace)/\(.metadata.name)"'
```

Fix each one by the classes its workers should get rather than by sorting alone, because sorting starts assigning a class the set has never assigned.
To keep today's behaviour, delete every tier whose threshold is not above all the thresholds before it, then set `maxWorkers` to the new last threshold, or unset it.

### Action required if a query reads `tier` on a disruption series: the label is gone

**Who is affected:** anyone whose own alerts, recording rules or dashboards filter or group on `tier` on a disruption series, including copies of the tenant Grafana dashboard taken before `v2.0.0`.
The reference [`PrometheusRule`](../../deploy/monitoring/prometheusrule.yaml) never read it.

**What changed.** [Six series](observability-metrics.md#breaking-observability-changes-q1291) no longer carry `tier`, whose values were `classic` and `scaleset`, because the classic tier is gone.
A query filtering on either value, such as `{tier="scaleset"}`, returns nothing and raises no error, so a rule reading it never fires again and a panel reading it shows `No data`.
`by (tier)` keeps working, with one group and an empty `tier` value.
Series recorded before the upgrade keep the label, so an `increase()` across the upgrade spans two series.

**Before you upgrade,** remove `tier` from those queries and legends, and re-import the tenant dashboard if you run a copy.

**Rolling back** to a `1.x` AGC brings the label back on those six series, so a query with the filter removed keeps working.
It also stops `actions_gateway_eviction_rerun_outcomes_total`, which `1.x` does not export.

### Action required if `spec.tracing.endpoint` does not parse as a URL: the value is now rejected

**Who is affected:** operators whose `spec.tracing.endpoint` is a bare IP with a port, such as `10.0.0.5:4317`, or another value in [Tracing Endpoint Rejected by Admission](troubleshooting.md#tracing-endpoint-rejected-by-admission).

**What changed.** The exporter ignores such a value and sends spans to `127.0.0.1:4317` with no error, so no spans arrive unless a collector listens on the AGC pod's own loopback, and the schema now refuses the value.
The upgrade itself changes nothing: a gateway already stored with one keeps running, and an edit that leaves the field alone is still accepted.
A delete and re-create is rejected, which includes a restore from backup.

**Before you upgrade,** correct the value, as in `https://10.0.0.5:4317`.

### Action required if `spec.tracing` sets `insecure: true` with an `https://` endpoint: the pair is now rejected

**Who is affected:** operators with an `ActionsGateway` whose `spec.tracing` has `insecure: true` and an `endpoint` starting `https://`, in any letter case and after any leading whitespace.

**What changed.** The exporter applies `insecure: true` after the endpoint's scheme, so the pair has been exporting spans in plaintext although the endpoint says TLS (measured against the exporter `v2.0.0` ships).
The schema now refuses the pair.
The upgrade itself changes nothing: a gateway already stored with it keeps running, and an edit that leaves `endpoint` and `insecure` alone is still accepted.
Changing the endpoint to another `https://` one while `insecure` stays true is rejected, and so is a delete and re-create, which includes a restore from backup.

**Before you upgrade,** list the gateways that have the pair:

```bash
kubectl get actionsgateways.actions-gateway.com -A -o json \
  | jq -r '.items[] | select(.spec.tracing.insecure == true and ((.spec.tracing.endpoint // "") | test("^\\s*https://"; "i"))) | "\(.metadata.namespace)/\(.metadata.name)"'
```

Fix each one by what its collector serves.
To keep exporting in plaintext, as these gateways do today, change the scheme to `http://`.
To export over TLS, remove `insecure`; spans then stop arriving at a collector that does not serve TLS, and the AGC logs the export error.

### Action required if a `noProxyCIDRs` entry exempts GitHub through a comma, wildcard, port or address: the entry is now rejected

**Who is affected:** operators with an `EgressProxy` whose `spec.noProxyCIDRs` covers a GitHub host in one of the forms below.
A GitHub host is a public one, such as `github.com` or `ghcr.io`, or the `githubURL` host of a gateway that uses the proxy.

| Form | Example |
|---|---|
| Two values in one entry, joined by a comma, a space or a tab | `internal.example.com,github.com`, `internal.example.com github.com` |
| A wildcard | `*`, `*.github.com` |
| A host with a port | `github.com:443` |
| A host with a trailing dot | `github.com.` |
| A lone dot, where a gateway's `githubURL` host ends in a dot | `.` for `https://ghes.corp.example./my-org` |
| A hostname spelled with non-ASCII letters | `ｇｉｔｈｕｂ.com` |
| An address or CIDR that contains a `githubURL` host written as an IP | `10.0.0.0/8` for `https://10.1.2.3/my-org` |

**What changed.** The GMC joins the entries with commas into the AGC's and the workers' `NO_PROXY`.
Two clients read that variable: the AGC's HTTP client, and libcurl, which `git` and `curl` in a worker pod use.
Each form above exempts the GitHub host in at least one of them.
The AGC's client was measured against the Go toolchain `v2.0.0` is built with; libcurl was measured in one release of `curl` on a workstation, not in a worker image.
The admission check compared each entry as one bare hostname and admitted them.
It now refuses an entry that either client reads as an exemption, and refuses a port-qualified entry whatever the port.
Whether the exempted traffic left the pod or was dropped by the namespace's NetworkPolicy was not measured.

The upgrade itself changes nothing: a proxy already stored with such an entry keeps running, and an edit that leaves `noProxyCIDRs` alone is still accepted.
An edit that changes the list is checked against every entry in it, so it is rejected until the entry is gone.
A delete and re-create is rejected, which includes a restore from backup.

**Before you upgrade,** list the entries written with a comma, a space, a tab, a `*`, a port, a trailing dot or a non-ASCII character:

```bash
kubectl get egressproxies.actions-gateway.com -A -o json \
  | jq -r '.items[] | "\(.metadata.namespace)/\(.metadata.name)" as $p | (.spec.noProxyCIDRs // [])[] | select(test("[,* \\t]|\\.$|[^\\x00-\\x7f]") or (test(":[0-9]*$") and (test("^[0-9a-fA-F:]+(/[0-9]+)?$") | not))) | "\($p)\t\(.)"'
```

The command lists an entry by its form, whatever it names, so `*.internal.example.com` appears and is still accepted.
It does not list a plain address or CIDR; check those by hand only where a gateway's `githubURL` names its host by IP.
Nor does it list a plain hostname, which is newly refused only where a gateway's `githubURL` spells that same host with a trailing dot.
Remove each entry that covers a GitHub host.
GitHub traffic for the gateways on that proxy then goes through it.

### Action required if a shared `EgressProxy` exempts a consumer's GitHub host: the pair is now rejected

**Who is affected:** operators with an `EgressProxy` that lists namespaces in `spec.sharing.allowedNamespaces` and has a `noProxyCIDRs` entry covering the `githubURL` host of a gateway in one of them.
A public GitHub host was already refused in any proxy, so in practice the host is a GitHub Enterprise Server one.

**What changed.** The check compared a proxy with the gateways and `RunnerSet`s in its own namespace only, and a gateway or `RunnerSet` with the proxy of that name in its own namespace only.
A shared proxy and a referrer in another namespace were therefore never compared, from either side.
Both sides now follow `defaultProxyRef.namespace` and `proxyRef.namespace`.
A reference the proxy does not grant is still not compared, because it connects nothing; adding that namespace to `allowedNamespaces` is the write that is then checked.

The upgrade itself changes nothing, and a stored pair keeps running.
An edit to the proxy that leaves `noProxyCIDRs` and `allowedNamespaces` alone is accepted, and so is an edit to the gateway or `RunnerSet` that leaves its proxy reference alone.
Changing the proxy's `noProxyCIDRs`, or pointing a reference at the proxy, is rejected while the entry covers the host.
So is a delete and re-create of either object, which includes a restore from backup.

**Before you upgrade,** list the references that cross a namespace:

```bash
kubectl get actionsgateways.actions-gateway.com,runnersets.actions-gateway.com -A -o json \
  | jq -r '.items[] | (.spec.defaultProxyRef // .spec.proxyRef) as $r | select($r != null and ($r.namespace // .metadata.namespace) != .metadata.namespace) | "\(.kind) \(.metadata.namespace)/\(.metadata.name) (\(.spec.githubURL // "gateway \(.spec.gatewayRef.name)")) -> EgressProxy \($r.namespace)/\($r.name)"'
```

Each line names a gateway with its `githubURL`, or a `RunnerSet` with its gateway, and the proxy it references.
Read that proxy's entries, with the proxy's namespace and name from the line:

```bash
kubectl get egressproxies.actions-gateway.com -n team-shared shared-proxy -o jsonpath='{.spec.noProxyCIDRs}'
```

Remove an entry that covers the gateway's `githubURL` host, in any of the forms in [the section above](#action-required-if-a-noproxycidrs-entry-exempts-github-through-a-comma-wildcard-port-or-address-the-entry-is-now-rejected).

### Action required if a `networkPolicy` peer names a broad or link-local `cidr`: the value is now rejected

**Who is affected:** operators with an `ActionsGateway` whose `spec.tracing.networkPolicy.cidr` or `spec.credentials.workloadIdentity.signer.vault.networkPolicy.cidr` is one of these:

- broader than `/16` for IPv4 or `/48` for IPv6, as `0.0.0.0/0` is
- link-local: inside `169.254.0.0/16` or `fe80::/10`
- a range covering one of four cloud metadata and platform addresses: `100.100.100.200`, `168.63.129.16`, `fd00:ec2::254` or `fd20:ce::254`
- not a CIDR: an octet above 255, an octet or prefix length written with a leading zero, or an IPv4-mapped IPv6 address

**What changed.** Whoever writes the gateway writes this peer, and the GMC copies its `cidr` into the AGC's NetworkPolicy beside the API-server rule.
So `0.0.0.0/0` opened the peer's port from the AGC to every address, the node's metadata server included, whatever `apiServerCIDRs` scoped.
The schema now refuses the values above.
The upgrade itself changes nothing: a gateway already stored with one keeps running and keeps the egress rule it has, and an edit that leaves the `cidr` alone is still accepted.
That includes an edit to the peer's `port`, so a stored `0.0.0.0/0` can still be moved to another port until it is narrowed.
Changing the `cidr` to another refused value is rejected, and so is a delete and re-create, which includes a restore from backup.

**Before you upgrade,** list every gateway that sets a peer `cidr`, with the value, and check each against the list above:

```bash
kubectl get actionsgateways.actions-gateway.com -A -o json \
  | jq -r '.items[] | . as $g
      | (.spec.tracing.networkPolicy.cidr, .spec.credentials.workloadIdentity.signer.vault.networkPolicy.cidr)
      | select(. != null and . != "")
      | "\($g.metadata.namespace)/\($g.metadata.name)\t\(.)"'
```

The command does not filter, because an address can be spelled too many ways for a text match to be trusted: every line is a peer to read.

Narrow each one to the peer's own address or subnet, as in `10.0.5.7/32`.
The stored rule stays as broad as it is until you do.
If the peer's addresses span more than one `/16`, remove `networkPolicy` and write a NetworkPolicy of your own that selects the AGC pod.
NetworkPolicies add up, so yours opens the path and the GMC's is unchanged.

### Action required if a gateway names a GitHub Enterprise Server host: an FQDN-mode pool reaches it only when the platform allowlists it

**Who is affected:** platform admins with an `ActionsGateway` whose `githubURL` names a host other than public GitHub, where the `EgressProxy` it uses is in an FQDN egress mode.
A pool with `managedNetworkPolicy: false` is affected too when it sets `destinationFQDNs` or `destinationCIDRs`, either of which turns on the proxy's CONNECT allowlist.
The appliance's host name no longer admits a CONNECT there, so such a pool reaches the appliance only when its `destinationCIDRs` contain the appliance's address.
With `destinationCIDRs` set it reports no gap, because the GMC cannot tell whether those ranges contain the appliance, and the `GitHubEgressIncomplete` message names the host the CONNECT allowlist lost.
A CIDR-mode pool whose policy the GMC manages is not affected: it reaches the appliance through the ranges in `destinationCIDRs`, as before.

**What changed.** The tenant writes `githubURL`, and the GMC used to add its host to the pool's FQDN policy and to the proxy's CONNECT allowlist without consulting the platform egress allowlist.
The same host in `spec.destinationFQDNs` was refused at admission, so `githubURL` was a way around that allowlist: a tenant could open TCP/443 from the proxy pool to any hostname.
The GMC now adds the host only when `--allowed-egress-fqdns` or the watched allowlist ConfigMap covers it.
Public GitHub needs no entry, whichever of `github.com`, `www.github.com` and `api.github.com` the gateway spells and in any letter case.

**This takes effect when the GMC is upgraded, with no object edited.** The new GMC's first reconcile of the pool rewrites its policy without a host the allowlist does not cover, and that gateway's AGC and workers can then no longer reach the appliance.
The pool reports `GitHubEgressIncomplete=True` with reason `GitHubHostNotAllowlisted`, and the message names the host.

**Before you upgrade,** list the hosts your gateways name other than public GitHub:

```bash
kubectl get actionsgateways.actions-gateway.com -A -o json \
  | jq -r '[.items[].spec.githubURL | capture("^https://(?:[^/@]*@)?(?<host>\\[[^\\]]*\\]|[^/:?#]+)").host | ascii_downcase | sub("\\.$"; "")] | unique[] | select(. != "github.com" and . != "www.github.com" and . != "api.github.com")'
```

Add each host to the chart's `allowedEgressFQDNs` in the same `helm upgrade` that installs `v2.0.0`, so the new GMC starts with it:

```yaml
allowedEgressFQDNs:
  - ghes.corp.example
```

An entry covers its subdomains, so `corp.example` also covers `ghes.corp.example`.
Prefer the chart value to the [watched ConfigMap](security-operations.md#self-service-additions-via-a-watched-configmap) for an appliance host.
A ConfigMap that is deleted or fails validation is ignored whole, and every pool that reached its appliance through a ConfigMap entry then loses it until the ConfigMap is fixed.
A host the command prints that is not a DNS hostname, such as the wildcard `*.corp.example` or a bracketed IPv6 literal, is never added, whatever the allowlist holds.
The proxy admits a CONNECT to that appliance only at an address in the pool's `destinationCIDRs`.

**After the upgrade,** confirm no pool reports the gap:

```bash
kubectl get egressproxy -A -o jsonpath='{range .items[?(@.status.conditions[?(@.type=="GitHubEgressIncomplete")].status=="True")]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'
```

For a pool it lists, see [A GHES Tenant's Traffic Never Reaches the Appliance](troubleshooting.md#a-ghes-tenants-traffic-never-reaches-the-appliance).

**Rolling back** to a `1.x` GMC puts every referrer's host back in the policy, allowlisted or not.

### Action required if a `githubURL` spells public GitHub `www.github.com` or `api.github.com`: a new gateway is rejected, and a stored one shares scale-set names with `github.com`

**Who is affected:** operators with an `ActionsGateway` whose `githubURL` host is `www.github.com` or `api.github.com`, stored in the cluster or written in a manifest or backup that will create one.
A stored gateway is affected a second way when another gateway names the same organization, enterprise or repository under another of the three hosts, and a `RunnerSet` under each has the same first `runnerLabel`.

**What changed.** The AGC sends all three hosts to the public GitHub API, but the [scale-set name guard](troubleshooting.md#runnerset-rejected-duplicate-first-runnerlabel) compared them as three different places, so it admitted two `RunnerSet`s claiming one scale-set name in one organization.
The guard now reads all three as `github.com`, and admission rejects a new gateway that uses either alias.
Whether GitHub registers runners under the `www.` and `api.` spellings was not measured.
If it does, such a pair drives one scale set and each tenant acquires the other's jobs; if it does not, the sets under that gateway never register.

The upgrade itself refuses nothing.
A gateway already stored under either alias keeps running and stays editable, and so does every set under it.

Creating a gateway under either alias is [rejected](troubleshooting.md#actionsgateway-rejected-githuburl-spells-public-github-by-an-alias), in any letter case and with or without a trailing dot.
A delete and re-create is a create, so this covers a restore from backup and a GitOps apply that re-creates a deleted gateway.
`githubURL` is immutable, so a stored gateway leaves the alias only by that route: delete it, then create it with the host written `github.com`.
Until you do, keep the alias in its manifest, because an apply that changes the host of a stored gateway fails the immutability rule.
In a backup, change the host to `github.com` before you restore from it.

A stored gateway also shares scale-set names with the other two hosts from now on.
An edit that keeps a set's first `runnerLabel` and its `gatewayRef` is still accepted, and a new claim on a shared name is rejected.
That covers creating a set, and moving a set onto the name by changing its first label or its `gatewayRef`.

The name sharing also applies to a host written with a trailing dot, such as `github.com.`, which names the same place as the host without it.
A new gateway on `github.com.` is still accepted.

**Before you upgrade,** list the gateways that use either spelling or a trailing dot:

```bash
kubectl get actionsgateways.actions-gateway.com -A -o json \
  | jq -r '.items[] | select(.spec.githubURL | test("^https://([^/@]*@)?((www|api)\\.github\\.com\\.?|[^/:@]+\\.)(:[0-9]+)?/"; "i")) | "\(.metadata.namespace)/\(.metadata.name)\t\(.spec.githubURL)"'
```

No output means no stored gateway is affected.
Search your manifests and backups for the two hosts as well, since the command reads only what is stored.
For each gateway listed, check whether another gateway names the same path on another of the three hosts, or on the same host without the dot, and whether a set under each has the same first label.
`githubURL` is immutable, so fix a pair by giving one side's `RunnerSet` a distinct first `runnerLabel`, and change the `runs-on` of the workflows that name it in the same change.

**After the upgrade,** both gateways of a pair that is still stored report `ScaleSetNameCollision=True`, which pages through `ActionsGatewayScaleSetNameCollision` ([finding the other holder](troubleshooting.md#actionsgateway-reports-scalesetnamecollision)).

### Action required if a namespaced `RunnerTemplate` goes past Pod Security baseline: the shape is now rejected

**Who is affected:** tenants in a namespace on the `privileged` security profile who author their own `RunnerTemplate` with a field the Pod Security Standards `baseline` level forbids: a capability outside the baseline set, a `hostPath` volume, a `hostPort`, an `Unconfined` seccomp profile, an `Unconfined` AppArmor profile (the field or the `unconfined` annotation), an unmasked `procMount`, a sysctl outside the safe set, an SELinux type outside the allowed set, a Windows `hostProcess` container, or a `host` on a probe or lifecycle hook.
On the `baseline` and `restricted` profiles Pod Security Admission already refuses the worker pod such a template builds, so there the template never produced a worker.

**What changed.** The admission webhook refused only `securityContext.privileged: true` on a namespaced `RunnerTemplate`.
In a `privileged`-profile namespace nothing else constrained it, so a tenant could author a host-reaching worker without a privileged container, for example one mounting the node's root filesystem through `hostPath`.
The webhook now evaluates the whole `podTemplate` against the `baseline` level, with the same checks Pod Security Admission runs, on every profile.
It evaluates the level as Kubernetes 1.37 defines it, and that version is fixed in the release: if a later Kubernetes tightens `baseline`, this rule does not follow until a GAG release's notes say so.
A `ClusterRunnerTemplate` is exempt: it is how the platform publishes a shape past `baseline`, such as the library's [`kata-dind`](runner-template-library.md).

The upgrade itself changes nothing: a template already stored keeps building workers, and an edit that leaves `podTemplate` alone is still accepted.
Any change to `podTemplate` is rejected until the template meets `baseline`, and so is a delete and re-create, which includes a restore from backup.

**Before you upgrade,** list the namespaced templates that set a field `baseline` restricts:

```bash
kubectl get runnertemplates.actions-gateway.com -A -o json | jq -r '
  ["AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "MKNOD", "NET_BIND_SERVICE", "SETFCAP", "SETGID", "SETPCAP", "SETUID", "SYS_CHROOT"] as $caps
  | .items[]
  | .spec.podTemplate as $t
  | ([$t.spec.securityContext] + [($t.spec.containers // []) + ($t.spec.initContainers // []) | .[].securityContext] | map(select(. != null))) as $sc
  | select(
      any($sc[]; ((.capabilities.add // []) - $caps != [])
        or .seccompProfile.type == "Unconfined"
        or .appArmorProfile.type == "Unconfined"
        or (.procMount // "Default") != "Default"
        or .seLinuxOptions != null
        or .windowsOptions.hostProcess == true
        or (.sysctls // []) != [])
      or any(($t.spec.containers // []) + ($t.spec.initContainers // []) | .[].ports[]?; (.hostPort // 0) != 0)
      or any($t.metadata.annotations // {} | to_entries[]; (.key | startswith("container.apparmor.security.beta.kubernetes.io/")) and .value == "unconfined"))
  | "\(.metadata.namespace)/\(.metadata.name)"'
```

The list is wider than the rule in two places and narrower in two.
It names every template that sets `sysctls` or `seLinuxOptions`, although `baseline` allows a short list of each.
It does not read a probe or lifecycle hook's `host` field, which `baseline` also forbids, and it leaves `hostPath` to the volume query in the next section.

Have a platform administrator publish each shape that has to stay as a `ClusterRunnerTemplate`, and point the `RunnerSet`'s `templateRef` at it with `kind: ClusterRunnerTemplate`.
Otherwise remove the field.

**If your own admission policy already decides tenant pod shape,** you can turn this rule and the volume-type rule in the next section off instead.
A `ValidatingAdmissionPolicy` or a policy engine can refuse more than the GMC webhook does, but it cannot admit a template the webhook refuses, so with the rules on, a policy that permits tenants one specific shape past `baseline`, such as a `hostPath` under one directory, has no effect.
Set the chart value `allowUncheckedRunnerTemplatePodShape: true`, which passes `--allow-unchecked-runner-template-pod-shape` to the GMC.
The webhook then checks neither rule on a namespaced `RunnerTemplate`, and what a tenant may author is whatever your policy admits.
With no such policy, two things come back.
A tenant in a `privileged`-profile namespace can again author a worker pod that reaches the node.
And a tenant in any namespace below the `restricted` profile, the default `baseline` one included, can again declare an `nfs` or `iscsi` volume that the node mounts from a server the tenant names, so an install with no `privileged`-profile namespace gives something up too.
Turning the value off again does not re-check a template admitted while it was on: it is a stored template like any other, checked the next time its `podTemplate` changes.
The privileged-container, PriorityClass and ServiceAccount-token rules stay on either way.

### Action required if a namespaced `RunnerTemplate` declares an `nfs`, `iscsi` or similar volume: the type is now rejected

**Who is affected:** tenants whose `RunnerTemplate` declares a volume of any type other than `configMap`, `csi`, `downwardAPI`, `emptyDir`, `ephemeral`, `image`, `persistentVolumeClaim`, `projected` or `secret`.
Those nine are the types the Pod Security Standards `restricted` level allows.
On the `restricted` profile Pod Security Admission already refuses the worker pod, so the change reaches the `baseline` and `privileged` profiles.

**What changed.** Pod Security Admission at `baseline` forbids `hostPath` and admits every other volume type, `nfs` and `iscsi` included.
The node mounts such a volume, not the pod, so its traffic to a server the tenant names passes neither the worker `NetworkPolicy` nor the egress proxy.
The admission webhook now refuses those types on a namespaced `RunnerTemplate`.
A `ClusterRunnerTemplate` is exempt.

The upgrade itself changes nothing: a template already stored keeps building workers, and an edit that leaves `podTemplate` alone is still accepted.
Any change to `podTemplate` is rejected while the volume is there, and so is a delete and re-create, which includes a restore from backup.

**Before you upgrade,** list the namespaced templates with such a volume:

```bash
kubectl get runnertemplates.actions-gateway.com -A -o json \
  | jq -r '.items[] | select(any(.spec.podTemplate.spec.volumes[]?; [keys[] | select(IN("configMap", "csi", "downwardAPI", "emptyDir", "ephemeral", "image", "persistentVolumeClaim", "projected", "secret"))] == [])) | "\(.metadata.namespace)/\(.metadata.name)"'
```

Move each one to a `persistentVolumeClaim` volume bound to a `PersistentVolume` or `StorageClass` the platform provides ([shared worker storage](worker-shared-storage.md) is the worked example), or have a platform administrator publish the shape as a `ClusterRunnerTemplate`.
A platform whose own admission policy decides volume types can turn this rule off together with the `baseline` one ([how, and what it gives up](#action-required-if-a-namespaced-runnertemplate-goes-past-pod-security-baseline-the-shape-is-now-rejected)).

### Action required if a template projects a ServiceAccount token: the source is now rejected, and removed from worker pods

**Who is affected:** anyone with a `RunnerTemplate` or `ClusterRunnerTemplate` whose `podTemplate` declares a `projected` volume with a `serviceAccountToken` source, and any job that reads the token that volume mounts.

**What changed.** The AGC forces `automountServiceAccountToken: false` on every worker pod so that a job holds no Kubernetes API credential.
That field stops only the automatic mount: the apiserver stores a `serviceAccountToken` source a pod declares for itself, and the kubelet mints the token for it.
`v2.0.0` closes the route in two places.
The admission webhook refuses the source on both template kinds, and the AGC removes it from every worker pod it builds.

The same rule and the same removal cover a `podCertificate` source whose `signerName` begins with `kubernetes.io`.
Kubernetes reserves those signers, and its 1.37 API documents one, `kubernetes.io/kube-apiserver-client-pod`, as issuing client certificates the apiserver accepts and as not yet implemented.
A `podCertificate` from any other signer is left alone.

Neither reaches a token that a mutating admission webhook adds when the pod is created, as IAM Roles for Service Accounts (IRSA), EKS Pod Identity and Azure Workload Identity do.
The template does not declare that token, and the webhook adds it after the AGC has built the pod.

**This one takes effect at the upgrade, for stored templates too.** A stored template is still accepted while `podTemplate` is unchanged, but workers built from it after the AGC upgrades no longer get the token.
The volume stays and mounts without the token file, so the pod starts and a step that reads the file fails.

**Before you upgrade,** list the templates of either kind that have the source:

```bash
kubectl get runnertemplates.actions-gateway.com,clusterrunnertemplates.actions-gateway.com -A -o json \
  | jq -r '.items[] | select(any(.spec.podTemplate.spec.volumes[]?.projected.sources[]?; has("serviceAccountToken") or ((.podCertificate.signerName // "") | startswith("kubernetes.io")))) | "\(.kind) \(.metadata.namespace // "-")/\(.metadata.name)"'
```

Remove the source from each, and give a job that needs a cloud or cluster credential one through its workflow, for example GitHub's OpenID Connect token exchanged with the provider.

### Action required if a namespaced `RunnerTemplate` sets `nodeName` or `schedulerName`: both are now rejected, and cleared from worker pods

**Who is affected:** anyone with a namespaced `RunnerTemplate` whose `podTemplate.spec` sets `nodeName`, or a `schedulerName` other than `default-scheduler`.
A `ClusterRunnerTemplate` is not affected.

**What changed.** The GMC, AGC and egress proxy pods carry a required anti-affinity against worker pods, which the default scheduler enforces ([why](security-operations.md#keep-gags-control-plane-off-worker-nodes)).
A worker with `nodeName` set skips the scheduler, and one naming another scheduler is placed by something that need not read the term, so either could put a tenant's worker beside a controller.
The admission webhook now refuses both on a namespaced `RunnerTemplate`, and the AGC clears both from every worker it builds from one.
The `allowUncheckedRunnerTemplatePodShape` opt-out does not turn this rule off.

**This one takes effect at the upgrade, for stored templates too.** A stored template is still accepted while `podTemplate` is unchanged, but its workers are placed by the default scheduler once the AGC upgrades.

**Before you upgrade,** list the namespaced templates that set either field:

```bash
kubectl get runnertemplates.actions-gateway.com -A -o json \
  | jq -r '.items[] | select((.spec.podTemplate.spec.nodeName // "") != "" or ((.spec.podTemplate.spec.schedulerName // "default-scheduler") != "default-scheduler")) | "\(.metadata.namespace)/\(.metadata.name)"'
```

Move each one's placement to a `nodeSelector` or a required node affinity, or have a platform administrator publish the shape as a `ClusterRunnerTemplate`.

### Action required if you overlay a library template: `deploy/templates/` is written at `v2`

**Who is affected:** operators who apply a [runner template library](runner-template-library.md) entry through their own kustomize overlay with a JSON 6902 patch.

**What changed.** The three `ClusterRunnerTemplate`s under `deploy/templates/` are now written at `actions-gateway.com/v2`, the version new tenants onboard on, instead of `v2beta1`.
The objects they create are identical: the two versions share every `ClusterRunnerTemplate` field.

**What you will see.** A patch whose `target` still says `version: v2beta1` matches nothing, and kustomize skips it without an error.
`kubectl kustomize` exits 0 and renders the library template without your changes, so your workers run its default image and land on whatever nodes it allows.

**What to do.** Change `version: v2beta1` to `version: v2` in each patch `target` that names a library template, then render the overlay and confirm your patched fields are in the output.
A strategic-merge patch that names the template by `apiVersion` needs the same change.

### Action required: the GMC reads Secrets only in marked tenant namespaces

**Who is affected:** a cluster with an `ActionsGateway` or `EgressProxy` in a namespace carrying neither tenant marker, which is possible only while the `gmc-tenant-resource-guard` binding runs in `Audit` mode or `admissionPolicy.enabled=false`.
An install whose guard runs in the default `Deny` mode already has every tenant namespace marked, and needs nothing.

**What changed.** The GMC's ClusterRole no longer grants `get`, `list` or `watch` on Secrets cluster-wide (Q1188).
The chart ships a `gmc-tenant-secret-reader` ClusterRole holding those three verbs, and the GMC binds it to its own ServiceAccount with a RoleBinding of the same name in every namespace labelled `actions-gateway.github.com/tenant=true` or `actions-gateway.com/tenant=managed`.
A compromised GMC can therefore no longer read Secrets in `kube-system` or any other unmarked namespace.

**What you will see.** A RoleBinding named `gmc-tenant-secret-reader` appears in each tenant namespace once the upgraded GMC starts.
A gateway in an unmarked namespace stops reconciling, and the GMC logs a `secrets is forbidden` error for it on every retry.

- **RBAC:** the manager ClusterRole drops `get`/`list`/`watch` on `secrets`, keeps `create`/`update`, and gains `bind` on `clusterroles` restricted to the name `gmc-tenant-secret-reader`.
- **Deployment:** the GMC reads its own ServiceAccount name from a new `POD_SERVICE_ACCOUNT` downward-API variable, and refuses to start without it.
  The chart sets it; a GMC deployed from your own manifests needs the same `env` entry, `fieldRef: {fieldPath: spec.serviceAccountName}`.

**What to do.** Before upgrading, list the namespaces holding gateways or proxies that carry neither marker, and label each one:

```sh
kubectl get actionsgateways.actions-gateway.github.com -A \
  -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' | sort -u
```

On a cluster with the v2 CRDs, run it again for `actionsgateways.actions-gateway.com` and `egressproxies.actions-gateway.com`.
Label each namespace that `kubectl get namespace <namespace> --show-labels` shows without a marker:

```sh
kubectl label namespace <namespace> actions-gateway.com/tenant=managed
```

`v2.0.0` honours only this marker ([why](#before-upgrading-to-v200-every-tenant-namespace-carries-the-actions-gatewaycom-labels)), so apply it rather than the 1.x `actions-gateway.github.com/tenant=true`.

**Removing a namespace from the tenants** now has a second step: delete its `gmc-tenant-secret-reader` RoleBinding after removing the marker.
The guard denies the GMC that delete once the namespace is no longer a tenant, so the binding, and the GMC's read access, stay until you remove it.
From `v2.0.0` the GMC deletes the binding itself when the marker goes, and this step is no longer needed.

### Non-breaking: the GMC's ConfigMap, CNI policy and ServiceMonitor writes are confined to tenant namespaces

**Who is affected:** nobody whose `gmc-tenant-resource-guard` binding runs in the default `Deny` mode with every gateway and proxy in a marked tenant namespace.
Read on if a namespace that is not a tenant is listed in an `EgressProxy`'s `spec.sharing.allowedNamespaces`, or if you deploy the GMC from your own copy of the policy manifests.

**What changed.** The guard now matches ConfigMaps, the CNI-native policy kinds (`CiliumNetworkPolicy`, Calico `NetworkPolicy`, GKE `FQDNNetworkPolicy`), and ServiceMonitors, so the GMC may create, update or delete them only in namespaces labelled `actions-gateway.com/tenant=managed` (Q1321).
Before, the GMC's ClusterRole let it write those kinds in any namespace and no policy stopped it, which a compromised GMC could have used to rewrite a ConfigMap in `kube-system`.
Two ConfigMap writes are admitted outside a tenant namespace: any in the GMC's own install namespace, where it keeps its GitHub IP-range store, and the delete of a ConfigMap the GMC projected into a consumer namespace (`proxy-share-*` or `proxy-attribution-*`), so either is still withdrawn from a namespace that has lost its marker.

**What you will see.** Nothing, on an install where every namespace holding a gateway is a marked tenant.
Where an unmarked namespace is granted a share and holds a gateway or `RunnerSet` naming the proxy, the GMC no longer writes the `proxy-share-*` ConfigMap there, and the **provider** `EgressProxy` reports it: `Degraded=True` and `Ready=False` with reason `ProvisioningFailed`, a Warning event, and a message carrying the guard's denial, which names the namespace.
The proxy itself keeps serving, because the share step is the last one, and shares into marked namespaces and revocations proceed in the same reconcile.
The GMC retries, so the condition and the denial in the API server audit log repeat until the namespace is labelled or the grant removed.
That gateway could not run before either, since the guard already denied its Deployment.

**What to do.** Label the consumer namespace as a tenant ([tenant onboarding](tenant-onboarding.md)), or remove it from `allowedNamespaces`.
If you apply `cmd/gmc/config/admission-policy/tenant-resource-guard.yaml` yourself and the GMC does not run in `gmc-system`, change the namespace in the `isInstallNamespaceConfigMap` expression as well as the ServiceAccount identity in `matchConditions`; the chart renders both from the release namespace.

### Action required if a `Throughput` `RunnerSet` sets `maxRequests.memory`: its memory limit now stops there

**Who is affected:** a `RunnerSet` with `spec.sizing.profile: Throughput` and a `maxRequests.memory` below its observed memory peak × `limitHeadroomPercent`.

**What changed.** `maxRequests` already bounded the derived requests; the `Throughput` memory limit, derived from a peak the jobs themselves control, now stops at `maxRequests.memory` too, and never below the request (Q1327).
Without the bound a job sampled near its limit raised the next limit by the headroom factor, generation after generation.

**What you will see.** Worker pods built after the upgrade carry the lower limit, and a job that needed the difference is killed for memory where it was not before; on a scale-set set its out-of-memory retry is capped at `maxRequests.memory` as it already was.

**What to do.** Compare `status.sizingRecommendation[].observedPeak.memory` × `limitHeadroomPercent` / 100 with `maxRequests.memory`, and raise the ceiling where the gap is headroom you meant to grant ([worker rightsizing](worker-rightsizing.md)).
A set with no `maxRequests.memory` is unchanged.

### Non-breaking: one repository's out-of-memory kills no longer fill the scale-set failure ledger

**What changed.** Under `Binpack` and `Throughput`, each job a `RunnerSet` lost to an out-of-memory kill wrote a memory floor, a parked re-run and a merged disruption into `scaleset-failures-<set>`, none of which the 512 KiB cap evicted, so 564 killed jobs from one repository's matrix runs filled it and every other repository's automatic re-run was withheld as `eviction_rerun_failures_total{reason="budget_unrecorded"}` (Q1327).
Now one parked re-run is kept per run attempt, since its release re-runs every failed job of the attempt; a re-run lists at most 16 merged disruptions and jobs; and memory floors are evicted oldest first, after failed-job records, before a budget write is refused.

**What you will see.** Nothing, unless floors are evicted: a job whose floor is gone provisions at its resolved limit again, and a second kill of it reads as a first one, still inside the run's `maxEvictionRetries`.

### Action required if a tenant namespace selects `privileged` without the eligibility label: the GMC can no longer stamp it

**Who is affected:** a tenant namespace labelled `actions-gateway.com/security-profile=privileged` without `actions-gateway.com/privileged-profile=allowed`.
`namespace-security-profile-guard` refuses that pair when the profile label is set, so it exists only where that guard ran in `Audit` or was disabled.

```sh
kubectl get namespaces -l 'actions-gateway.com/tenant=managed,actions-gateway.com/security-profile=privileged,actions-gateway.com/privileged-profile!=allowed'
```

**What changed.** `namespace-psa-guard` now admits a GMC Pod Security stamp only at the values the namespace's own profile label selects, at version `latest`, with no mode label dropped, and `privileged` only where the namespace is eligible (Q1328).
Before, it checked which labels the GMC changed but not their values, so the GMC's namespace grant could set `enforce=privileged` on any tenant namespace.

**What you will see.** Nothing on a namespace whose labels already agree.
On an affected one the GMC's stamp is denied, the Pod Security labels stay as they are, and the namespace gets a repeating `NamespaceMarkerMissing` Warning event carrying the guard's denial.

**What to do.** Either add `actions-gateway.com/privileged-profile=allowed`, if privileged workers there are a platform decision you have made, or set the profile label to `baseline` or `restricted` with the `actions-gateway.com/allow-profile-downgrade=allowed` annotation.

### Non-breaking: deleting a gateway keeps its workers' NetworkPolicy until they are gone

**Who is affected:** anyone who deletes an `ActionsGateway` while its `RunnerSet`s still have worker pods.

**What changed.** Teardown used to delete the `<gateway>-workload` NetworkPolicy once the bound sets reported no jobs, which happens as soon as the AGC issues its pod deletes, while each pod still runs through its `terminationGracePeriodSeconds`; past the 90-second drain deadline the pods were not reaped at all.
It now deletes every other child and keeps that policy, and the gateway's finalizer, until no pod of a bound set, or of a RunnerSet already deleted, remains (Q1328); deleting the namespace skips the wait.
To see those pods, the `gmc-tenant-secret-reader` ClusterRole gains `list` on Pods, still granted one tenant namespace at a time.
`gmc-tenant-resource-guard` also now admits a GMC RoleBinding write only as its own reader binding or an `agc-tenant-role` binding to ServiceAccounts of that namespace, the two shapes the GMC writes.

**What you will see.** A deletion that waits for terminating workers shows a `TeardownIncomplete` event naming the policy as held for them, and finishes when they exit.
After a `WorkerDrainTimeout` it waits until you delete the pods or their `maxWorkerLifetime` deadline fires ([troubleshooting](troubleshooting.md#actionsgateway-deletion-hangs-on-waitingforworkerdrain)).

### Action required if you copied the webhook-rejection alert or imported the security dashboard: both now see the `-v2` webhooks

**Who is affected:** anyone who copied the `ActionsGatewayWebhookRejections` alert from [security-operations.md](security-operations.md#prometheus-abuse-alerts), or imported the security dashboard, whose `name=~` selector still names a `-v2alpha1` webhook: any copy taken from release 1.8, 1.9 or 1.10.

**What changed.** Both select the GMC's webhooks by name, and through 1.10 they still named the `-v2alpha1` webhooks that 1.9 renamed to `-v2`, so a copy taken from 1.8 went blind on the upgrade to 1.9.
From 1.9 on, neither could see a denial by the five `actions-gateway.com` webhooks: the alert could fire only for `vactionsgateway-v1alpha1.kb.io`, and the dashboard's **total denied** series counted that webhook alone.
Both now name the webhooks the chart ships (Q1267): five from `v2.0.0`, whose chart no longer ships the `v1alpha1` one.

**What to do.** Replace the `name=~` selector in your copy of the alert with the one in [security-operations.md](security-operations.md#prometheus-abuse-alerts), and re-import `deploy/monitoring/grafana-dashboard-security.json`.

### Non-breaking: two `RunnerSet`s may share a name across namespaces

**Who is affected:** nobody has to change anything.
A platform that picked distinct `RunnerSet` names only to get past the `agent-identity stem … is already claimed` rejection may stop.

**What changed.** Admission no longer rejects a `RunnerSet` because another namespace holds its name under the same GitHub org, enterprise, or repo.
That rule protected the runner names the classic agent pool derived from the CR name, and that pool no longer exists.
A set's name now reaches only objects in its own namespace and the scale-set session owner, which is `<namespace>/<name>`.

**What still has to be unique.** A set's first `runnerLabel` names its scale set at GitHub, so [admission still refuses a duplicate](troubleshooting.md#runnerset-rejected-duplicate-first-runnerlabel) across the whole scope.

**How far this is tested.** The end-to-end suite runs such a pair against its fake GitHub server: each job lands in its own namespace, and deleting one set leaves the other serving.
No run has tested the pair against github.com or GitHub Enterprise Server.

**Rolling back** to a `1.x` GMC restores the rejection, which lands on the next write to either object, so rename one of a pair first.

### Non-breaking: a worker deleted while still pulling its image is force-cancelled, then re-run

**Who is affected:** anyone whose workers can be reaped by `pendingPodDeadline` or drained while scheduled but still `Pending`, typically pulling a large or unpullable image.
Nothing to do.

A real kubelet deleting such a worker publishes a transient `Failed` and records its never-started container as terminated with no start, exit or container ID (measured on a kind cluster).
The AGC read that record as a container that had run, so on the classic tier the job was reported failed with no force-cancel and its run waited out GitHub's ~15-minute unstarted-job timeout, and on the `ScaleSet` tier an external delete of such a worker was not recovered at all.
Both now take the never-started handling: the run is force-cancelled at once and re-run once the owner places a worker again, counted under `abandoned_run_force_cancels_total` (Q1231).
A worker that never left `Pending` because it was never scheduled was already handled this way.
These workers also stop adding samples to `actions_gateway_job_duration_seconds`, which already skipped a worker that never started a container, so where image pulls are slow or broken the series' count drops and its percentiles shift.

**Rolling back** returns these workers to the unstarted-job timeout.

### Non-breaking: `EgressRulesStale` now fires when no GitHub ranges have loaded

**Who is affected:** anyone alerting on `EgressRulesStale` or `actions_gateway_egress_rules_stale`.
Nothing to do, unless you alert on reason `RefreshPending`: that reason no longer covers this state, so match `RefreshNeverSucceeded` as well.

A CIDR-mode `EgressProxy` or v1 `ActionsGateway` used to read `EgressRulesStale=False` / `RefreshPending` while the GMC had no GitHub IP ranges at all, which is exactly when its policy denies all GitHub egress.
It now reads `True` with the new reason `RefreshNeverSucceeded`, so the existing `ActionsGatewayEgressRulesStale` alert fires on it after 15 minutes (Q1228).
`RefreshPending` remains only for an unmanaged or FQDN-mode policy.
While the state lasts, the GMC re-checks it every minute, so it clears within a minute of the first fetch.

**Rolling back** returns the state to `False` / `RefreshPending`.

### Non-breaking: a direct-egress v2 gateway reports `EgressRulesStale` and pages on it

**Who is affected:** anyone running a v2 `ActionsGateway` with no `defaultProxyRef`, and anyone applying `deploy/monitoring/prometheusrule.yaml`.
Nothing to do.

Such a gateway carries the GitHub allowlist on its own AGC and workload `NetworkPolicies`, and no condition reported whether that allowlist was being refreshed.
It now carries `EgressRulesStale`, with the reasons the `EgressProxy` uses, exported as the new gauge `actions_gateway_direct_egress_rules_stale` and paged on by the new `ActionsGatewayDirectEgressRulesStale` alert after 15 minutes (Q1249).
A proxied gateway reads `False` / `RefreshPending`, since its `EgressProxy` reports the allowlist.
The gauge has its own name, rather than a series on `actions_gateway_egress_rules_stale`, because a gateway and an `EgressProxy` may share a namespace and a name.

**Rolling back** removes the gauge and stops updating the condition, which keeps its last value until the gateway is next written by a GMC that sets it.

### Non-breaking: the GMC keeps GitHub egress across a restart

**Who is affected:** every install whose proxy pools or direct-egress gateways use the default `CIDR` egress mode.
Nothing to do.

A restarted GMC used to give those pools no GitHub egress until its first `api.github.com/meta` fetch succeeded, which a rate-limited fetch could hold off for up to an hour.
The GMC now writes each successful fetch to a ConfigMap named `actions-gateway-gmc-ip-ranges` in its own namespace and seeds itself from it at startup (Q1227).
A copy as old as the `EgressRulesStale` window (49 hours on the default refresh) is not used, and a seeded copy stops being used at that age even if no fetch has succeeded.
The ConfigMap is created on the first successful fetch after the upgrade and is owned by the GMC's Deployment, so `helm uninstall` deletes it with the Deployment.
An install that does not set the chart's `DEPLOYMENT_NAME` environment variable on the GMC writes it with no owner, and it is left behind on uninstall.
Anything that recreates the Deployment rather than updating it (`helm upgrade --force-replace`, renaming the release, or `kubectl delete deployment`) deletes the ConfigMap too, so the next GMC start waits for its first fetch.

**Rolling back** returns a restart to waiting for the first fetch; the older GMC ignores the ConfigMap.

### Non-breaking: a reaped scale-set worker whose job ran elsewhere recovers the job left without a runner

**Who is affected:** `ScaleSet` sets whose workers schedule unevenly, so a worker is reaped `Pending`, or evicted, preempted or drained before its container ran, after another runner of the set took the job it was created for.
Nothing to do.

Since Q1169 such a reap cancels nothing, because the worker's own job is running.
The set is still one runner short, and the job that lost its runner used to wait out GitHub's unstarted-job timeout and need a manual re-run.
The AGC now names that job from the listener's in-flight record, force-cancels its run, and re-runs it once capacity returns, as for any abandoned worker (Q1195).
The evicted, preempted and drained shapes take the same path when no idle worker of the set could take a job instead (Q1235).
It names none while a `Running` worker's job start may not be recorded yet, and walks again once the start lands, for up to ten minutes after the loss (Q1235).

**Rolling back** returns that job to waiting out the timeout.

### Non-breaking: a worker whose node shuts down before it starts is force-cancelled, then re-run

**Who is affected:** clusters on Spot or preemptible capacity, or anywhere a node can go away while a worker is still `Pending`, pulling its image.
Nothing to do.

The kubelet's graceful shutdown fails a still-`Pending` worker the same way it fails a running one, and such a worker used to be re-run under `cause="node_shutdown"` at once.
Its job never ran, and GitHub refuses a re-run of a run it has not concluded, so that recovery waited on GitHub's own ending of a run with an unstarted job: about 15 minutes, as long as the re-run window, which risks ending as `eviction_rerun_failures_total{reason="run_never_concluded"}` with a spent retry slot.
That outcome is read from source and has not been observed on a cluster.
It now takes the never-started handling on both tiers: the run is force-cancelled at once and re-run once the owner places a worker again, counted under `abandoned_run_force_cancels_total` rather than `eviction_retries_total{cause="node_shutdown"}` (Q1198).
A classic-tier worker that never started on a dead node you taint out-of-service used to stay failed, and is now recovered the same way.

**Rolling back** returns these workers to the immediate re-run, and the classic out-of-service case to staying failed.

### Non-breaking: a worker on a dead node you taint out-of-service is re-run automatically

**Who is affected:** clusters where nodes go `NotReady` without being deleted (network failure, kernel panic, power loss, hardware failure) and an admin applies Kubernetes' `node.kubernetes.io/out-of-service` taint to confirm the node is dead.
Nothing to do.

A job whose worker ran on such a node used to stay failed: PodGC failed and removed the pod, and no recovery arm recognised the shape it left.
It is now re-run under `cause="node_shutdown"`, the same counter a Spot reclamation uses (Q1207).
A worker the AGC had already deleted itself (a reaper delete, or a reclaimed `job_abandoned` worker) is no longer re-run under `node_shutdown` when its node then dies, matching the `deletion` cause.
A `NotReady` node with no taint is unchanged: its worker stays `Running` until you taint the node or delete its Node object, because a node that is only partitioned may still be running the job.
The command is in [Which Disruptions Auto-Re-Run a Job](troubleshooting.md#which-disruptions-auto-re-run-a-job-and-which-never-do).

**Rolling back** returns the job on such a node to staying failed.

### A worker evicted for its own storage limit is no longer re-run

**Who is affected:** a tenant whose jobs outgrow the worker's own `ephemeral-storage` limit, one container's limit, or an `emptyDir` `sizeLimit`.

**What changed.** The kubelet marks a pod that exceeds its own storage limit `Failed` with `reason: Evicted`, the same phase and reason it writes for node pressure, and eviction recovery used to re-run both.
A re-run gets the same limit and fails the same way, so such a job spent up to `maxEvictionRetries` re-runs failing.
The AGC now tells the two apart by the kubelet's message and its `DisruptionTarget` condition, and withholds the re-run for the limit case on both acquisition tiers (Q1177).
A node-pressure eviction is re-run as before, and so is any `Evicted` shape the AGC does not recognise.

**What you will see.** `actions_gateway_eviction_rerun_withheld_total{cause="eviction", reason="own_limit"}` increments, and a **Normal** `EvictionRerunWithheld` Event quotes the kubelet's message.
No retry slot is spent, so `eviction_retries_total{cause="eviction"}` stops counting these, and the job stays failed at GitHub.

**What to do.** Nothing at upgrade time.
When the Event appears, raise the limit in the runner template or reduce what the job writes, then re-run the job by hand.
Behaviour is covered in [Which Disruptions Auto-Re-Run a Job](troubleshooting.md#which-disruptions-auto-re-run-a-job-and-which-never-do).

### Non-breaking: the scale-set re-run budget survives an AGC restart, and one run attempt is re-run once

**Who is affected:** every `ScaleSet`-protocol `RunnerSet` with `maxEvictionRetries` above 0.
Nothing to do; one new object appears per set that has a worker disrupted.

`maxEvictionRetries` counted GAG's re-runs of each run in AGC memory, so an AGC restart refilled every run's budget.
And two workers of one run disrupted in the same attempt each spent a slot and each re-ran the run: the second waited out `403 This workflow is already running` until the first re-run concluded, then re-ran it again.
The count now lives in a per-`RunnerSet` ConfigMap, and a second disruption of an attempt GAG has already re-run merges into that re-run (Q1182).
The field's meaning is unchanged: it counts GAG's own re-runs, never a human's.

Operational consequences:

- **A third ConfigMap per set.** `scaleset-failures-<set>` appears the first time one of that set's workers is disrupted, or, from the release after `v1.10.0`, the first time one of its jobs fails (Q555), beside `scaleset-guards-<set>` and `scaleset-recovery-claims-<set>`.
  It carries the same `actions-gateway.com/runner-set` label and is owner-ref'd to the set.
  Each run's entry expires 30 days after GAG last reserved a re-run for it, GitHub's re-run window, and the object is capped at 512 KiB, about 1,100 runs re-run inside 30 days at the default budget of 2 (measured by the AGC's unit test).
  Past the cap a new reservation is refused rather than an old budget dropped.
  From the release after `v1.10.0` the object also holds failed-job records, evicted oldest first past half the cap, so a `v1.10.0` AGC sharing it during a rollout or after a rollback, which keeps those records but cannot evict them, still has the other half for re-run budgets.
- **Fewer re-runs and fewer `eviction_retries_total` increments for multi-job runs.** A drain that takes three workers of one run now re-runs it once and counts once; the merged disruptions log `a re-run is already reserved for this run attempt`, and re-run the attempt themselves only if that re-run never lands.
- **Every scale-set recovery now reads the run.** The eviction, preemption and vanished arms did not call GitHub before the re-run; they now `GET` the run once to learn the attempt the job failed in, and again before each re-run call, standing down once the run is already past that attempt.
  A run that cannot be read, or that names no `run_attempt`, is not re-run, and counts as `eviction_rerun_failures_total{reason="conclusion_unknown"}` or `{reason="api_error"}`.
  The call needs only Actions read access, which the re-run already requires.
- **A new failure reason.** `eviction_rerun_failures_total{reason="budget_unrecorded"}` counts a recovery whose budget decision could not be written, which withholds the re-run.
  Expected to be zero; see the [runbook](troubleshooting.md#evicted-worker-pods-exhausting-retry-budget).
- **The budget is per set, not shared with the classic tier.** A run whose workers span a classic `RunnerGroup` and a scale-set `RunnerSet`, or two `RunnerSet`s, can be re-run up to `maxEvictionRetries` times on each.
  A classic `RunnerGroup` keeps its in-memory count.
- **A drained worker of a run whose other job was abandoned now re-runs.** GAG force-cancels an abandoned run, and the drain recovery used to read that cancel as a human's and stand down; the AGC now records the attempt it cancels and treats that cancel as its own.
- **RBAC:** unchanged.
  The AGC already holds `get`, `create` and `update` on `configmaps`.

**Rolling back** restores the in-memory count, so the next restart refills budgets and a multi-job disruption re-runs once per worker again.
A leftover `scaleset-failures-<set>` is inert on older versions, which never read it, and is collected with its `RunnerSet`.
Rolling forward again reads it as it was left, including budgets spent before the rollback that have not expired.

### Non-breaking: the worker-image registry read presents the worker ServiceAccount's pull secrets

**Who is affected:** a tenant whose worker image sits in a registry that requires a login, with the pull Secret on the worker ServiceAccount rather than in the pod template, as the [air-gapped install](air-gapped-install.md#5-wire-pull-secrets-for-the-runtime-workloads-agc--proxy--worker) wires it.

The registry read behind `RunnerVersionTooOld` used to present only the Secrets the pod template's `imagePullSecrets` names, so on that wiring it failed authentication on every reconcile while the pull itself worked.
When the template names none, the AGC now presents the worker ServiceAccount's `imagePullSecrets`, the same set ServiceAccount admission puts on the worker pod (Q1066), and the `WorkerImageVersionUnknown` authentication failure resolves to a verdict read from the image.

- **RBAC:** the `agc-tenant-role` ClusterRole gains `get` on `serviceaccounts`, no `list`, `watch` or write, and the chart ships it.
  An AGC running against a Role without the grant reads anonymously as before, and a refused read names the unread ServiceAccount in its message.
- A pull Secret named in both places can come out of the pod template, since the ServiceAccount's is now read too; leaving it in changes nothing.

No action is required at upgrade time.

### Non-breaking: a scale-set worker that never started a job no longer cancels or re-runs its namesake's run

A worker whose runner started no job carries the run identity of the job it was created for, and GitHub can give that job to another runner of the set.
Recovery used to force-cancel that run when the worker was reaped `Pending`, and re-run it when the worker was evicted, preempted or drained, though the job was running elsewhere (Q1169).

Recovery now lists the set's workers first.
A reaped or deleted `Pending` worker's run is left alone when another worker carries its job as `actions-gateway.com/started-job-id`.
An evicted, preempted or drained worker with no `started-job-id` is not re-run when another worker started its job.
Where its container never ran, it is also not re-run while a live worker whose runner has started nothing could still take that job; a pod marked as a disruption target is never counted as that worker.
The AGC logs each skip at `info`; no metric or Event changes.

No action is required at upgrade time.

### Non-breaking: a scale-set worker's run identity follows the job its runner started

GitHub gives a scale-set job to whichever of the set's runners asks first, so a worker can run a different job, from a different run, than the one it was created for.
Recovery used to re-run the run the worker was created for, which left the run that actually lost a job failed (Q1152).

A worker's `actions-gateway.com/run-id`, `/repository` and `/job-name` annotations are now rewritten when its runner starts a job, so `kubectl describe pod` shows the run the worker is serving, and an evicted, preempted or drained worker re-runs that run.
Recovery after an AGC restart finds a job's worker by `actions-gateway.com/started-job-id` rather than by pod name, and the `OrphanedWorkerRecovered` Event now names the job rather than a pod.
The recovery-claim ConfigMap's entries gain a `jobID` field; entries written by an older AGC lack it and are read as before.

No action is required at upgrade time.

### Non-breaking: an idle scale-set worker left by a job no runner started is reclaimed

When a scale-set job ends before any runner starts it, typically a run cancelled by a `cancel-in-progress` concurrency group, one of the set's workers has nothing left to run.
Earlier releases reclaimed only the worker created for that job, and when its runner had already taken another job, the idle worker that was left held a concurrency slot and a node until `maxWorkerLifetime` (12 hours by default).
The AGC now gives the oldest idle `Running` worker the five-minute reap deadline instead, marked `actions-gateway.com/surplus-for-job`, and moves that deadline to another idle worker if the picked one is given a job (Q1153).
A worker whose runner has started a job is never picked.

Expect more `WorkerPodOrphanedRunning` Events and `actions_gateway_worker_pods_reaped_total{reason="orphaned_running"}` on busy sets that cancel queued jobs: each is a worker that used to sit idle until the lifetime cap.
No action is required at upgrade time.

### Non-breaking: a scale-set worker's JIT-config Secret is now owned by its pod

Each scale-set worker mounts a `job-ss-*` Secret holding its runner's JIT config, a runner credential.
That Secret's `ownerReferences` now name the worker pod instead of the `RunnerSet`, so Kubernetes deletes it once the pod is gone, including a pod deleted by hand or reaped before its job's completion arrived (Q1154).
Earlier releases could leave such a Secret in the namespace until the `RunnerSet` was deleted.
Deleting the `RunnerSet` still removes both, since it owns the pods.

Secrets created by an earlier AGC keep the `RunnerSet` as owner and are reclaimed as before.
No action is required at upgrade time.

### Non-breaking: `v2` is served beside `v2beta1`, and an unpinned read now returns `v2`

**Who is affected:** anyone with the v2 CRDs installed.

**What changed.** Every `actions-gateway.com` kind is now also served at `v2`, the General Availability (GA) version: the five kinds in `actions-gateway-crds-v2`, and `PriorityClassAllowlist` in the main chart.
`v2beta1` stays the storage version, so no stored object is rewritten.
The GMC's conversion webhook converts the five chart kinds between `v2` and the other two versions; `PriorityClassAllowlist` has no webhook, and the apiserver converts it by rewriting `apiVersion` alone.
`v2` is identical to `v2beta1` except that `EgressProxy.spec.egressPolicyMode` accepts only `CIDR` and `FQDN`.

**What you will see.** The apiserver prefers a GA version, so `kubectl get <kind>` and `kubectl get <kind> -o yaml` without a version now return `apiVersion: actions-gateway.com/v2`.
An `EgressProxy` that still names `CiliumFQDN` or `CalicoFQDN` reads at `v2` as `egressPolicyMode: FQDN`, with the alias in the annotation `conversion.actions-gateway.com/egress-policy-mode`.
The stored object keeps the alias, and so does the pool: writing the `v2` view back with the annotation in place restores it.
Deleting the annotation, or setting another mode, at `v2` migrates the pool off the alias.
So does replacing the whole object with a `v2` manifest that lacks the annotation, as `kubectl replace` or a GitOps tool that replaces objects does, with no warning; migrate such a pool deliberately first.
Don't set the annotation yourself: admission rejects a write that would introduce an alias this way, as it does any other.
Find any pools still on an alias with the [pre-upgrade check](#before-upgrading-to-v200-no-egressproxy-still-names-a-deprecated-fqdn-alias), which is pinned to `v2beta1` because an unpinned read shows `FQDN`.

**What to do.** Re-apply both charts' CRDs as every upgrade already does: the `actions-gateway-crds-v2` render, and the main chart's `PriorityClassAllowlist` CRD, pulled first.
Nothing else is required, and manifests can stay on `v2beta1`.
Move them to `v2` before `v2.0.0`, which removes `v2beta1`; until then, a manifest on `v2beta1` is the one that still applies after a rollback.

**Rolling back** to a release that does not serve `v2` is safe for stored objects, because they are stored at `v2beta1`.
A manifest written at `apiVersion: actions-gateway.com/v2` fails to apply there.

### Non-breaking: the `actions-gateway.com` validating webhooks now validate `v2`, and their names end in `-v2`

**Who is affected:** anyone who matches GMC admission errors by webhook name, in alerts, log queries or runbooks.

**What changed.** The five GMC validating webhooks for `actions-gateway.com` kinds are renamed, and so are their serving paths:

| Kind | Old name | New name |
|---|---|---|
| `ActionsGateway` | `vactionsgateway-v2alpha1.kb.io` | `vactionsgateway-v2.kb.io` |
| `EgressProxy` | `vegressproxy-v2alpha1.kb.io` | `vegressproxy-v2.kb.io` |
| `RunnerSet` | `vrunnerset-v2alpha1.kb.io` | `vrunnerset-v2.kb.io` |
| `RunnerTemplate` | `vrunnertemplate-v2alpha1.kb.io` | `vrunnertemplate-v2.kb.io` |
| `ClusterRunnerTemplate` | `vclusterrunnertemplate-v2alpha1.kb.io` | `vclusterrunnertemplate-v2.kb.io` |

Each rule now names `v2`, so the apiserver converts a write at `v2alpha1`, `v2beta1` or `v2` to `v2` before the GMC validates it.
The checks and their messages are unchanged, and so is the version a write can use.
`v2.0.0` stops serving `v2alpha1` and `v2beta1`, and a rule naming only those would stop matching without an error; this release moves the rules first, so the `v2` validators run for a whole release before `v2.0.0` depends on them.

**What you will see.** An admission rejection names the new webhook, for example `admission webhook "vrunnerset-v2.kb.io" denied the request`.
Until every GMC pod runs the new release, a write to one of these kinds can fail with an admission webhook call error instead, because the new configuration can reach an old pod that does not serve the new path; retry it once the rollout completes.

**What to do.** Update anything that matches the old names.
Nothing else is required.

**Rolling back** restores the old names and paths, and the same error can appear in reverse while it runs: the old configuration can reach a new pod that serves only the `-v2` path.

### A new `CiliumFQDN` / `CalicoFQDN` `EgressProxy` is now rejected at admission

**Who is affected:** anyone who still creates an `EgressProxy` naming one of the two deprecated per-CNI `spec.egressPolicyMode` values, or switches an existing pool onto one.
A pool that already stores one is unaffected and keeps running.

**What changed.** The GMC admission webhook warned on these values; it now rejects a write that *introduces* one:

```text
spec.egressPolicyMode: CiliumFQDN is deprecated and may no longer be introduced; it is
removed at v2.0.0 along with every API version that defines it. Use egressPolicyMode: FQDN
and have the platform operator set GMC --fqdn-policy-backend=cilium, which enforces exactly
the same policy.
```

A create is always a new write.
An update is one only when it changes the value, so re-applying an unmigrated pool, editing the rest of its spec, and migrating it off the alias are all still admitted; switching one alias for the other is not.
The warning stays for the unchanged case.

**Why now rather than at `v2.0.0`.** `v2` does not define the aliases, and `v2.0.0` removes every version that does, so a pool still on one is rewritten as plain `FQDN` then and falls to whatever `--fqdn-policy-backend` names.
The reject is what stops the population growing while there is still time to drain it.
Find any that remain with the [pre-upgrade check](#before-upgrading-to-v200-no-egressproxy-still-names-a-deprecated-fqdn-alias).

**What to do.** Set `egressPolicyMode: FQDN` on the `EgressProxy` and have the platform operator set the matching GMC `--fqdn-policy-backend` (`cilium` or `calico`).
Each alias pins its namesake backend and `FQDN` plus the matching selector resolves to the same emitter, so the policy enforced on the wire does not change.

**Rolling back** restores the warning: a new alias write is accepted again.
Nothing about a stored object changes in either direction.

### Non-breaking: a drained worker's recovery claim moves off the pod into a second ConfigMap

**Who is affected:** every `ScaleSet`-protocol `RunnerSet`.
Nothing to do; one new object appears per set that has a worker disrupted.

A drained or preempted worker's pod used to carry the `actions-gateway.com/eviction-handled-at` annotation that made its recovery at-most-once, and the kubelet removes that pod seconds after the container exits.
A recovery whose write lost that race was reported and abandoned, and its run needed a manual re-run.
The claim now lives in a per-`RunnerSet` ConfigMap that outlives the pod, so those runs are recovered normally.

Operational consequences:

- **A second ConfigMap per set.** `scaleset-recovery-claims-<set>` appears the first time one of that set's workers is disrupted, beside the existing `scaleset-guards-<set>`.
  It carries the same `actions-gateway.com/runner-set` label, is owner-ref'd to the set so the garbage collector reaps it, and is bounded by a one-hour claim TTL and a 256-entry cap rather than by history.
  A tooling script that lists ConfigMaps by that label and takes the first now gets two objects in no fixed order.
- **`eviction_recovery_evidence_lost_total` narrows without changing meaning.** It still counts disruptions whose runs will not be re-run automatically, so an alert on it needs no edit, but the cause behind it is now a claim the AGC could not write rather than a pod that went away.
  Expect the rate to fall; a rate that does not points at the `configmaps` grant rather than at AGC responsiveness.
- **The pod annotation stays and still means what it said.** It is written after the claim, as the marker an operator reads and the filter that keeps the recovery scan off a pod it has already judged.
- **RBAC:** unchanged.
  The AGC already holds `get`, `create` and `update` on `configmaps` for the guard ConfigMap.

**Rolling back** restores the old behaviour: a drain whose pod goes before the claim lands needs a manual re-run again.
A leftover `scaleset-recovery-claims-<set>` is inert on older versions, which never read it, and is collected with its `RunnerSet`.

### Non-breaking: a `spec.scaleUp` token is now charged per worker pod, not per delivered job

**Who is affected:** every runner group or runner set that sets `spec.scaleUp` on the classic acquisition tier.
It is off by default, so a tenant that never configured a ramp sees nothing.
`ScaleSet` sets are unaffected: that tier states its capacity per long-poll and never charged per delivery.

**What changed.** The rate rung takes its token before the job is claimed, which is what lets it bind on a simultaneously-delivered burst (Q717).
GitHub fans a queued job out to several sibling sessions at once, and only one of them provisions: the rest recognise the duplicate after `acquirejob` and return without creating anything (Q260).
Each of those took a token and never gave it back, so a job delivered to N sessions spent N tokens for one worker pod.

| | Effective ramp under an N-way fan-out |
| --- | --- |
| Before | `maxPerSecond / N`, because every duplicate delivery charged a token for a pod it never created |
| Now | `maxPerSecond`: a delivery that returns before asking for a worker pod hands its token back |

A failed `acquirejob` refunds on the same rule.
The refund stops at pod creation and never fires on job completion, so the bucket stays a rate limit rather than becoming a second concurrency ceiling.

**What to expect at upgrade.** A set whose deliveries fan out will ramp at the rate it configured, which may be faster than what it has been doing.
Nothing else moves: `burst` still bounds the instantaneous batch, and a refund into a full bucket adds nothing.
`actions_gateway_worker_scaleup_throttled_total` and `actions_gateway_jobs_admission_rejected_total{reason="scaleup"}` should fall for an affected set, because fewer deliveries now find the bucket empty.

**Action required:** none.
If you raised `maxPerSecond` to compensate for a ramp that ran slower than configured, the value you first chose is now the one that applies, so re-check it against the egress path you were protecting.

### Non-breaking: `spec.scaleUp` now withholds intake instead of delaying jobs it has already claimed

**Who is affected:** every runner group or runner set that sets `spec.scaleUp`.
It is off by default, so a tenant that never configured a ramp sees nothing.

**What changed.** The scale-up token bucket became a rung of the pre-acquisition admission ladder (Q717).
It used to be waited on *after* the job was claimed, at pod-creation time, which meant a throttled job sat on a GitHub job lock for the length of its wait.
A lock that lapses cancels the job rather than redelivering it, so a large burst at a low `maxPerSecond` could destroy the jobs the ramp was smoothing.

| | Behaviour when the bucket is empty |
| --- | --- |
| Before | The job was claimed, then slept until a token freed, holding its GitHub job lock throughout |
| Now | The job is not claimed at all: it stays queued at GitHub for redelivery, and no lock is spent |

The ramp itself is unchanged: the same `maxPerSecond` and `burst` produce the same pod-creation rate.
A `ScaleSet`-protocol set expresses the rung as a smaller advertised capacity per long-poll, so GitHub never assigns the excess in the first place.

**What to expect at upgrade.** Time-to-pickup for a throttled job is now a GitHub redelivery interval rather than the remainder of the ramp, so individual jobs may be picked up later while none is at risk of being cancelled by a lapsed lock.
`actions_gateway_worker_scaleup_throttled_total` keeps its name and now counts withheld intake rather than delayed creations.
On the scale-set tier it stays a pod-creation measure and falls close to zero, because the advertisement withholds the work before it is assigned; a non-zero rate there now means the advertisement was stale rather than that the ramp is engaging.

**Action required:** none, but two new label values appear on existing series and any exhaustive `reason` selector should be widened: `actions_gateway_jobs_admission_rejected_total{reason="scaleup"}` and `actions_gateway_scaleset_capacity_withheld{reason="scaleup"}`.
If you lowered `maxPerSecond` to protect a shared egress path and then raised it again because claimed jobs were being held, the original value is now safe to restore.

### Non-breaking: `job_duration_seconds` now measures worker pod lifetime (the classic-tier span shrinks)

**Who is affected:** every tenant on the classic acquisition tier that has a dashboard, an alert, or a cost figure reading `actions_gateway_job_duration_seconds`.
Scale-set tenants are affected in the other direction: the series was empty there before and now reports.

**What changed.** The observation moved off the classic provisioning goroutine and onto the shared pod informer, which sees a scale-set worker pod and a classic one identically (Q713).
The span moved with it, and **nothing was renamed**, so both spans land in the same series:

| | Span |
| --- | --- |
| Before | Job acquisition to the controller observing the pod terminal, which included the job Secret staging, the active-pod count, any quota retry, and any `spec.scaleUp` throttle wait, none of which a node bills for |
| Now | The worker pod's own lifetime: `creationTimestamp` to its last container finishing, or to the deletion request for a worker removed mid-run |

A pod that never started a container is no longer observed at all: it ran no job and occupied no node time.

**What to expect at upgrade.** Classic-tier `job_duration_seconds` percentiles **step down**, by the pre-creation window the old span carried.
With `spec.scaleUp` unset (the default) and no quota pressure that window is seconds; with a configured `scaleUp` throttle or a namespace at its ResourceQuota it can be minutes.
Anything costed off the series steps down with it.
[Appendix F](../design/appendix-f-cost-model.md) multiplies it by an hourly node rate, so classic-tier attribution was reading **high** before this release, and the new span is the one a node actually bills for.

**Action required: re-baseline rather than compare across the step.** A percentile or `rate()` window that spans the upgrade mixes two spans and means nothing.
Re-baseline any threshold you set on classic job duration after the rollout settles, and treat pre-upgrade classic figures as a different measurement rather than a trend to extend.
The shipped dashboards now mark this: the tenant dashboard's Job-duration panel carries the span in its description, with an `actions_gateway_build_info` version panel beneath it, and the platform dashboard has a Build Versions row so a staggered fleet shows which tenants have crossed over ([dashboards](observability-dashboards.md)).

**Rolling back** returns the classic tier to the old, longer span and leaves the scale-set tier emitting nothing.

### Non-breaking: deleting a cancelled run's worker no longer re-queues the job

The remedy for a worker that keeps running after you cancel its run is to delete the pod, and that used to undo the cancel: a hand-deleted worker is the shape graceful-deletion recovery acts on, and `rerun-failed-jobs` is accepted for a `cancelled` conclusion.
That arm now reads the run's conclusion before it calls, on both acquisition tiers, and stands down when GitHub concluded the run `cancelled` (Q811).

A withheld re-run is visible rather than silent: `actions_gateway_eviction_rerun_withheld_total{reason="run_cancelled"}` is new here, and a **Normal** `EvictionRerunWithheld` Event names the run.
The disruption's retry slot is still spent, since it is reserved when the disruption is detected, minutes before GitHub has a conclusion to read, so `eviction_retries_total` keeps counting these; read the two together.
`actions_gateway_eviction_rerun_failures_total` also gains `reason="conclusion_unknown"`, for a recovery whose re-run window closed with the run's conclusion unreadable: a conclusion the AGC cannot read is not assumed to be uncancelled, so no re-run is fired and the run needs a manual one.

No action is required at upgrade time.
What changes for an operator is that deleting to reclaim capacity is now safe on a cancelled run, but only once GitHub shows it `completed`/`cancelled`, which takes up to GitHub's ~5-minute cancellation grace; delete before that and the job is re-queued as before.
Behaviour is covered in [Cancelling a Run Does Not Stop Its Worker Pod](troubleshooting.md#cancelling-a-run-does-not-stop-its-worker-pod).

### Non-breaking: an abandoned run is force-cancelled and re-run automatically, on both acquisition tiers

This release adds two recoveries for a worker removed before its container ever ran, the `pendingPodDeadline` reap being the common cause: the run is force-cancelled in about a second instead of waiting out GitHub's ~15-minute unstarted-job timeout (Q683), and it is then re-run automatically once the runner set places a worker pod again (Q691).
Both work on the classic **and** ScaleSet tiers (Q766), the latter being the default and `v2beta1`'s only option.

On the ScaleSet tier the run identity comes off the worker pod's `actions-gateway.com/run-id` annotation, read as the reaper deletes the pod.
So a `ScaleSet` set whose assignment messages carry no `workflowRunId` gets an `EvictionRecoveryIdentityUnknown` Warning Event and `actions_gateway_eviction_recovery_identity_unknown_total{cause="abandoned"}` instead of the recovery.
That is the same signal the Q417 eviction port already emits for the same missing field.

`actions_gateway_abandoned_run_force_cancels_total` and `actions_gateway_abandoned_run_rerun_waits_total` are both new here, and carry a `tier` label (`classic` / `scaleset`) from the start.
Add `tier` to a `by (...)` clause where you want the split; it follows the same convention [Q417 introduced](observability-metrics.md#breaking-observability-changes-q417).

No action is required at upgrade time.
What changes for an operator coming from 1.3 is the ending: a worker reaped while still `Pending` used to leave its run waiting out GitHub's unstarted-job timeout, and now concludes in about a second and is re-queued when capacity returns, on either tier.
Behaviour is covered in [Worker Pod Reaped While Pending](troubleshooting.md#worker-pod-reaped-while-pending-workerpodstuckpending).

### Non-breaking: `v1alpha1` is deprecated and the apiserver now warns

Both `actions-gateway.github.com` kinds carry `deprecated: true` on their `v1alpha1` version, so `kubectl` prints a warning on any `v1alpha1` read or write:

```text
Warning: actions-gateway.github.com/v1alpha1 ActionsGateway is deprecated; use actions-gateway.com/v2beta1 ActionsGateway. v1alpha1 is served until v2.0.0, which removes it; migrate with gag-migrate.
```

`RunnerGroup` carries the same warning naming `actions-gateway.com/v2beta1 RunnerSet` as its replacement.
The v1 monolith fans out into several v2 objects, so unlike the `v2alpha1` → `v2beta1` move below, the replacement is a different **kind** and not the same kind at a newer version — which is why the warning names [`gag-migrate`](migration-v1-to-v2.md) rather than a re-apply.

**Nothing breaks, and the upgrade needs no action.** Deprecation marks intent and removes nothing: `v1alpha1` stays fully served, existing objects keep reconciling untouched, and the removal release is the same `v2.0.0` the docs have named since the deprecation shipped.
The warning is advisory client-side output; it does not fail an `apply`, and controllers or CI that ignore warnings are unaffected.
The AGC and GMC receive the same warning through their own Kubernetes clients but log it once per unique message per process (deduplicated), so a tenant reconciling under churn does not flood its controller log.

**What to do about it.** Run [`gag-migrate`](migration-v1-to-v2.md) at your convenience — it is a one-shot fan-out of one v1 object into several v2 objects, and it preserves how jobs are acquired.
Onboard new tenants on `v2beta1` instead ([tenant onboarding](tenant-onboarding.md)).
`v1alpha1` is one of three things `v2.0.0` removes; the standing notice for all three, with a pre-upgrade checklist, is the [deprecation and removal notice](v1alpha1-deprecation.md).

### Non-breaking: an `EgressProxy` pool's pods drop the `app: actions-gateway-proxy` label (its pool is recreated once)

**Who is affected:** every namespace that already runs a v2 `EgressProxy`.
Tenants still wholly on v1 are unaffected — no v1 object, pod, or label changes, and no v1 proxy pool restarts.

**What was broken.** A v2 `EgressProxy` pool's pods stamped `app: actions-gateway-proxy` alongside their own `actions-gateway.com/egress-proxy: <proxy>` identity.
That bare `app` label is the *sole* key of v1's `PodDisruptionBudget` selector, v1's proxy `Deployment` selector, and v1's required hostname anti-affinity term — v1 has one fixed-name pool per namespace and never had to distinguish it from a second.
So for the whole coexistence window of a [v1→v2 migration](migration-v1-to-v2.md), each pool's pods were claimed by the other's:

- Each pool's pods fell under **both** `PodDisruptionBudget`s.
  A pod covered by two PDBs cannot be evicted at all, so a node drain of either pool's node never completed.
- **Neither pool autoscaled.** A `HorizontalPodAutoscaler` has no selector of its own — the controller reads its scale target's, refuses to act on pods a second HPA also controls, and reports `ScalingActive=False`/`AmbiguousSelector`.
  With overlapping pod sets, both HPAs wedged, and the v1 pool could not scale back down either.
- The two pools **repelled each other off every node**, so coexistence cost v1+v2 worker nodes rather than `max(v1, v2)`.

**What changed.** A v2 pool is now selected solely by `actions-gateway.com/egress-proxy`, which no v1 pod carries.
Its `Deployment`, `Service`, PDB, `NetworkPolicy`, and anti-affinity term all key on it alone, and the v2 workload `NetworkPolicy`'s proxy egress peer matches that label's *presence* rather than v1's `app` label — so v2 AGC and worker pods reach every `EgressProxy` pool in their namespace exactly as before, and no longer reach a coexisting v1 pool.

**What to expect on upgrade.** `Deployment.spec.selector` is immutable, so an `EgressProxy` pool provisioned before this release is **deleted and recreated once**, on the first reconcile after the GMC upgrade.
The replacement's pods wait out the old pods' termination grace period on the hostname anti-affinity before scheduling, so that pool's egress is briefly unavailable — treat it like the [proxy rollout](#proxy-upgrade) and upgrade during a quiet window if the tenant is mid-build.
A tenant migrating *after* the upgrade never pays this: `gag-migrate` creates the `EgressProxy` fresh, so its pool is born with the narrowed selector.

**What to do about it.** Nothing, unless you select v2 proxy pods by `app` yourself.
Any hand-authored `NetworkPolicy`, monitoring rule, `kubectl -l`, or dashboard that reaches a **v2** pool's pods via `app=actions-gateway-proxy` must move to `actions-gateway.com/egress-proxy=<proxy>` (or its presence, for any pool):

```bash
kubectl -n <namespace> get pods -l actions-gateway.com/egress-proxy
```

v1 pools keep `app=actions-gateway-proxy`, so recipes aimed at a v1 pool are unchanged.
The recommended `app.kubernetes.io/name=actions-gateway-proxy` label is still on both pools' pods and remains the version-agnostic way to find any proxy pod — as additive metadata only; never build a policy selector on it.

### Non-breaking: GitHub Enterprise Server gateways now reach their own appliance (they never did)

**Who is affected:** every tenant whose `spec.gitHubURL` names a GitHub Enterprise Server (GHES) host.
Tenants on `github.com` are unaffected in behaviour — but **every AGC pod restarts on this upgrade**, on both API versions, because the AGC `Deployment` gains an environment variable and the pod template changes.

**What was broken.** The AGC resolves the GitHub REST API base from `GITHUB_API_BASE_URL` and falls back to `https://api.github.com` when it is unset.
Nothing ever set it: both provisioning paths injected `GITHUB_ORG_URL` from `spec.gitHubURL` and stopped there, and no CRD field, Helm value, or other supported surface reached the variable.
A GHES gateway therefore POSTed its App JWT to `https://api.github.com/app/installations/<id>/access_tokens` — an App that host has never heard of.
Every token mint failed, on both acquisition tiers, before any job was acquired, and no configuration changed it.

**What changed.** The GMC derives `GITHUB_API_BASE_URL` from `spec.gitHubURL` and sets it on the AGC `Deployment`:

| `spec.gitHubURL` | injected `GITHUB_API_BASE_URL` |
|---|---|
| `https://github.com/my-org` | `https://api.github.com` (what the AGC already defaulted to) |
| `https://ghes.example.com/my-org` | `https://ghes.example.com/api/v3` |

The same derivation now answers for runner registration and the scale-set client, which each had their own copy.
One of those copies tested `github.com` as a substring of the whole URL, so a GHES org path literally named `github.com` — `https://ghes.corp/github.com` — was misread as public SaaS; that is fixed with the rest.

**Egress is a separate obligation, and only partly automatic.** Token exchange reaching the appliance does not by itself let traffic out:

- **FQDN egress modes** now carry every referrer's GHES host into the CNI policy and the proxy CONNECT allowlist automatically.
  Nothing to do.
  From `v2.0.0` the host must also be on the platform egress allowlist ([what to add](#action-required-if-a-gateway-names-a-github-enterprise-server-host-an-fqdn-mode-pool-reaches-it-only-when-the-platform-allowlists-it)).
- **CIDR mode (the default)** allows only the ranges `api.github.com/meta` publishes, which never contain a customer appliance.
  **You must supply the appliance's ranges in the `EgressProxy`'s `spec.destinationCIDRs`** — and, because that field is gated by the platform `--allowed-egress-cidrs` allowlist, a platform admin must allowlist them first.
  A pool in this state now reports `GitHubEgressIncomplete=True` with reason `ApplianceRangesRequired`, naming the unreachable host, instead of failing as an unexplained connect timeout.

**Post-upgrade check** for a GHES tenant:

```bash
kubectl get egressproxy -A -o jsonpath='{range .items[?(@.status.conditions[?(@.type=="GitHubEgressIncomplete")].status=="True")]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'
```

**Rolling back** restores the defect: GHES gateways return to minting against `api.github.com` and acquiring no jobs.
There is no configuration that works around it on an older image other than the testing-only `--allow-agc-extra-env` flag, which [security-operations.md](security-operations.md#github-api-base-url-must-be-https) tells you not to use in production.

### Non-breaking: a GHES appliance behind a private CA can now be trusted (`spec.githubCABundleRef`)

**Who is affected:** tenants whose GitHub Enterprise Server appliance is fronted by a private or internal certificate authority.
Nobody else: the new field is optional, and a gateway that does not set it produces a byte-identical AGC `Deployment`.
No pod restarts on this upgrade.

**What was broken.** The AGC and its worker pods trust the OS system roots plus the per-tenant egress proxy's own CA, and nothing — no CRD field, Helm value, or GMC flag — could extend that.
An appliance whose certificate chains to an internal CA therefore failed the TLS handshake on every call: token exchange and runner registration from the AGC, and `actions/checkout`, log upload, and artifact upload from the worker pod.
Routing through an `EgressProxy` did not help, because the proxy tunnels with `CONNECT` and the TLS session is end to end.
This was the last gap named by the audit behind the change above.

**What changed.** `ActionsGateway.spec.githubCABundleRef` (v2 only) names a `ConfigMap` in the gateway's namespace carrying a PEM bundle under `ca.crt`.
The GMC mounts it on the AGC `Deployment` and the AGC projects the same `ConfigMap` into every worker pod, so the control plane and the runners trust the same appliance.
The bundle is **additive** — the system roots stay trusted — so a gateway that also reaches public hosts is unaffected.

```bash
kubectl -n team-a create configmap ghes-ca --from-file=ca.crt=/path/to/corp-root-ca.pem
kubectl -n team-a patch actionsgateway my-gateway --type=merge \
  -p '{"spec":{"githubCABundleRef":{"name":"ghes-ca"}}}'
```

Setting the field re-renders the AGC `Deployment`, so **that tenant's AGC pod restarts**.
In-flight jobs are unaffected — worker pods are separate.

**A ref that does not resolve fails closed.** A `ConfigMap` that is missing, or whose `ca.crt` holds no PEM certificate, leaves the gateway `Ready=False` /`Degraded=True` with reason `CABundleNotFound` or `CABundleInvalid` and provisions no AGC — rather than a `Deployment` whose pod would sit at `ContainerCreating` with no explanation.
The gateway recovers on its own once the `ConfigMap` is applied.
Runbook: [troubleshooting.md § a GHES appliance's certificate is not trusted](troubleshooting.md#a-ghes-appliances-certificate-is-not-trusted).

**Egress remains a separate obligation.** Trusting the CA does not put the appliance's address space in the NetworkPolicy; see the CIDR-mode note above.

**Rolling back** restores the defect for these tenants.
`v1alpha1` has no equivalent field and never will — it is frozen and removed at `v2.0.0`.

### Agent-identity collisions are now rejected at admission (`RunnerSet` name, and derived `RunnerGroup` name)

This section describes the `1.x` releases; [`v2.0.0` removes the guard](#non-breaking-two-runnersets-may-share-a-name-across-namespaces).

**Who is affected:** a platform where two runner pools derive the same **agent-identity stem**, the string that `agentpool-<stem>-N` and the GitHub runner name `<stem>-N` are both built from.
Two shapes reach it, and everyone else is unaffected with no configuration change:

- a `RunnerGroup` whose name starts `rs-` sitting on the stem of a same-suffixed `RunnerSet` (the v1/v2 cross-derivation, since `rs-` is a prefix rather than an injection);
- two `RunnerSet`s of one **name**, or two `RunnerGroup`s of one name, in *different* namespaces whose gateways point at the same GitHub org, enterprise, or repo.

That second shape is the one most likely to be in a running cluster, and it was accepted before this release.
It was never safe: the runner name is unique per GitHub scope, so both pools register `<stem>-N` and each 409 is resolved by deleting the incumbent's record.
The loser reports `agent identity is owned by another pool` on every reconcile and never recovers.

**Nothing is re-validated at upgrade time**, because admission runs on create and update.
An existing colliding pair keeps running exactly as it did; the rejection lands whenever someone next re-applies either object, which may be long after the upgrade and on a tenant who changed nothing.
Unlike the scale-set guard above, there is **no** status condition or gauge reporting a pair carried in from an older release: the AGC's existing `AgentPoolError` Event is the only signal, and it fires only once a pool is actually blocked.

Find it in advance, before upgrading, in two commands.
Any stem appearing twice under one GitHub scope, or twice in one namespace, is a collision:

```bash
kubectl get runnersets.actions-gateway.com -A \
  -o jsonpath='{range .items[*]}{.metadata.namespace}{"\trs-"}{.metadata.name}{"\n"}{end}'
kubectl get runnergroups.actions-gateway.github.com -A \
  -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\n"}{end}'
```

**A v1 `ActionsGateway` write is rejected too**, for a `spec.runnerGroups` entry whose derived `RunnerGroup` name (`<gateway>-<first runnerLabel>-<hash>`) lands on a taken stem.
The entry has no name field, so the rejection names the spec index and the derived name.
**This can block a rollback**: re-applying a v1 gateway while the colliding v2 `RunnerSet` still exists is refused.
Delete that `RunnerSet` first: a rollback removes the v2 objects regardless, and the rejection names which one is in the way.

**Rolling back** removes both guards, so the colliding configuration is accepted again along with the pool takeover it allows.
Nothing is left behind to clean up: the guards are admission-only and write no status.

### Scale-set name uniqueness is now enforced across the whole GitHub scope, not per namespace

**Who is affected:** a platform running two or more `ScaleSet` runner sets, in *different* namespaces, whose gateways point at the same GitHub org, enterprise, or repo, and which share a **first** `runnerLabel`.
Everyone else is unaffected, and no configuration changes.

That configuration was accepted before this release and is rejected now.
It was never safe: the first `runnerLabel` is the scale set's name at GitHub, so two sets sharing it drive one scale set from two controllers and each tenant's AGC can acquire the other tenant's jobs.
The guard existed but was namespace-scoped, which is the one boundary that does not match how GitHub adopts a scale set by name.

**Nothing is re-validated at upgrade time**, because admission runs on create and update.
A colliding pair keeps running until someone re-applies either set, and the rejection lands then, which may be long after the upgrade and on a tenant who changed nothing.

**The GMC now tells you instead of leaving you to find out.** Each affected gateway reconciles into an advisory `ScaleSetNameCollision=True` condition with a `Warning` Event, and `actions_gateway_scale_set_name_collision` reads `1` for it, so a pair carried in from an older release surfaces on the first reconcile after the upgrade rather than at the next unrelated `kubectl apply`.
The condition does not gate `Ready` and does not stop the AGC: GAG cannot pick which tenant loses the name, and refusing to provision would take down the tenant that was there first as well.
Resolution steps: [`ActionsGateway` reports `ScaleSetNameCollision`](troubleshooting.md#actionsgateway-reports-scalesetnamecollision).

You can also find it in advance, before upgrading, for one command:

```bash
kubectl get runnersets -A -o jsonpath='{range .items[?(@.spec.acquisitionProtocol=="ScaleSet")]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.spec.runnerLabels[0]}{"\n"}{end}'
```

Any first label appearing twice under one GitHub scope is a collision: give one of the sets a distinct first label, which registers a new scale set.
Labels *after* the first may be shared freely.

The rejection names the GitHub scope but withholds the conflicting set, since it may belong to another tenant; a platform admin finds it in the GMC controller log or with the command above.
Two `Classic` sets are unaffected, registering no scale set at all.

**Rolling back** restores the namespace-scoped guard, so the colliding configuration is accepted again, along with the cross-tenant job acquisition it allows, and the condition and gauge that would have reported it go away with it.
A `ScaleSetNameCollision` condition left on a gateway is inert on the older version, which does not set or clear it; delete it by hand if a stale `True` would confuse a dashboard.

### Non-breaking: a worker lost while the AGC was down is now re-run automatically (`cause="vanished"`)

**Who is affected:** every tenant on the `ScaleSet` acquisition tier.
No configuration changes; the behaviour fires only on disruptions that previously needed a manual re-run.

Preemption and drain both *delete* the worker pod, and on this tier that pod is the only record of which run to re-run.
An AGC that was down for the pod's teardown (a rollout, an OOM kill, a node loss) therefore saw nothing and issued no re-run.
The AGC now persists the run identity of every job it builds a worker for, and on start re-runs any run whose worker pod is no longer there.

Operational consequences:

- **An AGC restart can now spend re-run budget.** Each recovered run consumes one slot of its shared `maxEvictionRetries` budget (default 2), under `cause="vanished"`.
- **New `cause` label value:** dashboards or alerts enumerating `cause` on `actions_gateway_eviction_retries_total` / `eviction_retries_exhausted_total` should add `vanished`.
  A climbing rate is a signal about the *controller's* availability rather than the workers': it means the AGC keeps missing worker teardowns.
- **New Event:** `OrphanedWorkerRecovered` (Warning) on the `RunnerSet`, naming the pod and the run.
- **The guard ConfigMap grows a field.** `scaleset-guards-<set>` gains an `inFlight` list alongside its concluded-job guards, bounded by the jobs currently running and swept at 24 hours.
  It is owner-ref'd to the `RunnerSet` as before, so it is still collected with it.
- **RBAC:** unchanged.
  The AGC already reads and writes that ConfigMap and already lists worker pods.

**Rolling back** restores the old behaviour: a worker lost while the AGC is down needs a manual re-run again.
A leftover `inFlight` list is inert on older versions, which ignore unknown fields when they load the guards.

### Non-breaking: drained and hand-deleted workers are now re-run automatically (`cause="deletion"`)

**Who is affected:** every tenant on either acquisition tier.
No configuration changes; the behaviour change fires only on disruptions that previously needed a manual re-run.

Before this version, a `kubectl drain` (or a bare `kubectl delete pod`) of a running worker interrupted its job with no automatic recovery.
Now the interrupted run is re-run automatically when the worker's terminal phase publishes while the pod is marked for deletion — the shape a drained running worker takes (measured, Q459).
The mark is absent on a human-cancelled run and on a genuine job failure, so neither can trigger it; the AGC's own reaper deletions are excluded by a new controller-set annotation.
Behaviour and boundary: the [drain runbook](troubleshooting.md#draining-a-worker-auto-re-runs-the-jobs-it-interrupts).

Operational consequences:

- **A node drain now spends re-run budget.** Each interrupted run consumes one slot of its shared `maxEvictionRetries` budget (default 2), under `cause="deletion"` on the eviction counters.
  Routine node maintenance across busy runner pools may warrant a higher budget.
- **A bare `kubectl delete pod` of a running worker re-runs its job.** If you delete a worker to *stop* its job, cancel the run at GitHub instead — a cancel is never re-run.
- **New annotation:** worker pods deleted by the AGC's reaper are stamped `actions-gateway.com/deletion-reason` just before deletion.
  Never set it by hand on a live worker; a hand-set stamp suppresses automatic recovery for that pod.
- **New `cause` label value:** dashboards or alerts that enumerate `cause="eviction"|"preemption"` on `actions_gateway_eviction_retries_total` / `eviction_retries_exhausted_total` should add `deletion`.
  No series is renamed.
- **RBAC:** the chart's `agc-tenant-role` gains `patch` on `pods` — metadata-only annotation stamps, never a spec or status write.
  Installs that mirror the role by hand must add the verb; without it the reaper cannot mark its own deletions and worker-pod cleanup stops (and the scale-set tier's recovery-claim and job-completed-at stamps, which always needed this verb, remain broken).
- **Timing:** the drain path's measured GitHub conclusion latency (15–26s) exceeds the default `evictionRetryDelay` (5s), so the first re-run call may be refused `403 This workflow is already running`; the Q503 retry loop (see the next note) absorbs that and lands the re-run within a few paced attempts.

**Rolling back** restores the old behaviour: drained workers' runs need a manual re-run again.
Leftover `deletion-reason` annotations are inert on older versions.

### Non-breaking: an evicted job's auto-re-run now lands (GitHub refused it before)

No action required, but two observable behaviours change (Q503).

Previously the AGC fired `rerun-failed-jobs` exactly once, `evictionRetryDelay` (default 5s) after seeing a worker evicted.
Against real GitHub that call always lost a race it could not win: an ungracefully killed runner reports nothing, GitHub concludes the run only when the job lock's TTL lapses (~10 minutes, measured 9m36-9m45s when the runner reports nothing), and until then the API refuses re-runs with `403 This workflow is already running`.
The retry budget was spent, `actions_gateway_eviction_retries_total` incremented, and the job was never re-run — every evicted job needed a manual re-run despite the metrics saying recovery had happened.

The AGC now treats that refusal as "not yet" and retries the re-run every 30 seconds inside a 15-minute window, on both acquisition tiers, still spending one budget slot per recovery.
What you will observe after upgrading:

- Evicted jobs actually re-run — expect the `disruption auto-retry triggered` log line and the second run attempt **~10 minutes** after the eviction, not seconds.
  Preempted jobs are unaffected in timing: their runner reports before dying, GitHub concludes in seconds, and the first or second call is accepted.
- A new counter, `actions_gateway_eviction_rerun_failures_total` (`reason="run_never_concluded"` | `"api_error"`), and a new `EvictionRerunFailed` Warning Event surface the recoveries whose re-run never landed — those still need a manual re-run.
  Expected to be zero; see [observability-metrics.md](observability-metrics.md) and the [runbook](troubleshooting.md#evicted-worker-pods-exhausting-retry-budget).

### Non-breaking: eviction auto-retry now honours `GITHUB_API_BASE_URL` (it never did on GHES)

**Who is affected:** any deployment that sets `GITHUB_API_BASE_URL`.
Deployments against `github.com` (the default) are unaffected: the endpoint they were reaching is the one they were meant to reach.
**No action is required, and nothing you configured was wrong.**

> **Correction.** This note originally said that meant "every GitHub Enterprise Server install".
> It did not: at the time, no GMC-provisioned AGC could set the variable at all, so this fix reached only hand-rolled deployments until [GHES gateways now reach their own appliance](#non-breaking-github-enterprise-server-gateways-now-reach-their-own-appliance-they-never-did) made the GMC derive it.
> On a GHES tenant the failure below was never observed, because token exchange failed first and no job was ever acquired.

Before this version the AGC resolved `GITHUB_API_BASE_URL` for the App **token exchange** but not for the `rerun-failed-jobs` call that eviction and preemption recovery make.
That call read a provisioner field nothing ever assigned, so it fell back to `https://api.github.com` unconditionally.
On GHES it therefore posted a valid installation token — minted against *your* endpoint — to a host that had never issued it, and the recovery failed with:

```text
disruption auto-retry failed; manual rerun may be required ... rerun API returned 401: Bad credentials
```

The 401 names `api.github.com`, a server the operator never configured, which is why this read as a credential problem rather than a routing one.
Recovery on GHES could not work, whatever `maxEvictionRetries` was set to.

After the upgrade:

- The rerun call addresses `GITHUB_API_BASE_URL`, the same endpoint the token was minted against, so recovery works on GHES.
- `actions_gateway_eviction_retries_total` starts moving on GHES clusters that evict or preempt workers.
  A rise here is the fix working, not a new fault.
- An **unset** base URL is now a startup error on the recovery path rather than a silent guess.
  It cannot occur in a GMC-provisioned AGC (the value is always resolved), but a hand-rolled deployment that removed the variable will now be told so instead of quietly addressing the wrong host.

The HTTPS rule is unchanged: a plaintext `GITHUB_API_BASE_URL` is still rejected unless the dev/test opt-in is present, and this call now inherits exactly that rule rather than having none of its own.

**Rolling back** re-arms the gap: recovery on GHES silently returns to calling `api.github.com` and failing with a 401.
There is no configuration that restores the behaviour on an older image.
### Non-breaking: preempted workers are now re-run automatically; eviction counters gain a `cause` label

**Who is affected:** every tenant on either acquisition tier.
The behaviour change only manifests for tenants using `priorityTiers` with a preempting class; the metric change affects anyone with dashboards or alerts on the eviction counters.

**What changed.** A worker displaced by kube-scheduler preemption — what a `priorityTiers` floor drives — is now recovered like a node-pressure eviction: lock renewal stops, and the displaced run is re-run via `rerun-failed-jobs`.
Previously it reached no recovery at all, because the scheduler *deletes* its victim rather than leaving it `Failed`/`Evicted` (measured, Q423), so the displaced job needed a manual re-run.
Detection now keys on the `DisruptionTarget` condition with reason `PreemptionByScheduler`, which only kube-scheduler writes.

The retry budget is unchanged and still shared: `maxEvictionRetries` remains a hard lifetime cap per workflow run across both acquisition tiers **and** both disruption causes.
A run that is alternately evicted and preempted spends one budget, not two — so a tenant running a preempting floor may see the budget exhaust sooner than before, and may want to raise `maxEvictionRetries` (default 2, max 10).

**What did NOT change at the time.** A `kubectl drain`, a `kubectl delete pod`, and the worker-pod reaper got no automatic re-run in this version.
That has since changed for the first two — see the `cause="deletion"` migration note above; the reaper's own deletions remain excluded.

**Metric change.** Three counters gained a `cause` label (`eviction` | `preemption`):

| Metric | Labels before | Labels after |
| --- | --- | --- |
| `actions_gateway_eviction_retries_total` | `namespace`, `runner_group`, `tier` | + `cause` |
| `actions_gateway_eviction_retries_exhausted_total` | `namespace`, `runner_group`, `tier` | + `cause` |
| `actions_gateway_eviction_recovery_identity_unknown_total` | `namespace`, `runner_group` | + `cause` |

No query breaks: aggregations such as `sum(...)`, `increase(...) > 0`, and `sum by (namespace, runner_group) (...)` are unaffected, which covers the shipped dashboards and alert rules.
Only exact-label-set matchers, and panels that rendered one series per metric and now render more, need attention.

**Action required — review any alert that pages on the eviction rate.** The meaning of `actions_gateway_eviction_retries_total` widened: before, a rising rate meant node trouble; now it also counts preemptions, which are the *expected* steady state under a preempting floor.
Scope such alerts to `{cause="eviction"}` unless they genuinely want both.
The shipped `ActionsGatewayEvictionRetryAbuse` rule in [security-operations.md](security-operations.md) was updated this way — copy the change if you forked it.

**Rolling back** returns preempted jobs to needing a manual re-run, and removes the `cause` label.
Nothing else is affected; no configuration restores the recovery on an older image.

### Non-breaking: classic-tier eviction auto-retry now fires (it never did against real GitHub)

**Who is affected:** any tenant on the classic acquisition tier — a `RunnerGroup`, or a `RunnerSet` with `spec.acquisitionProtocol: Classic`.
Scale-set tenants are unaffected; that tier takes its run identity from the assignment message and has always worked.
**No action is required, and nothing you configured was wrong.**

Before this version the AGC looked for a classic job's workflow run in the `system.github.run_id` and `system.github.repository` job variables.
A real `acquirejob` response carries neither: the run identity lives in the payload's serialised `github` context.
The run therefore resolved to `0`, and every eviction of a classic worker took `handleEviction`'s early return — logging `pod evicted but run_id unknown; skipping auto-retry` and calling nothing.
`maxEvictionRetries` was configurable but unreachable.
The same lookup fed the worker-pod annotations, so real classic worker pods also carried no `actions-gateway.com/run-id`, `/repository`, or `/workflow`.

After the upgrade, on the classic tier:

- An evicted worker's run is re-run automatically, within the run's `maxEvictionRetries` budget (default 2) — the behaviour the docs have always described.
- `actions_gateway_eviction_retries_total{tier="classic"}` starts moving.
  If you alert on it being flat, or on `EvictionRetriesExhausted` events, expect first signal from clusters that evict workers.
  A rise here is the fix working, not a new fault.
- New worker pods carry the run-identity annotations.
  Existing pods are not back-filled; they are replaced as jobs turn over.

The budget is shared per run across both tiers, so a run whose workers span tiers now consumes slots from the classic side too.

**Rolling back** re-arms the gap: a rolled-back AGC returns to reading the variables, and classic evictions stop being re-run again, silently.
There is no configuration that restores the behaviour on an older image.

### BREAKING (pre-GA): `capacityGate.mode` values replaced by `Observe` + a gateway-level cluster fact

**Who is affected:** only a `RunnerSet` that set `spec.capacityGate.mode` to `SchedulerVerdict`, `AutoscalerVerdict`, or `On`.
None of those values ever appeared in a tagged release — they existed only on `main`, and the field defaults to `Off` — so an install tracking releases has nothing to do here.

`Observe` is the name the single gating value settled on.
`On` said only *that* the gate was enabled; `Observe` says *how* it decides — from evidence an already-stuck worker pod produced, rather than by asking.
That distinction is invisible with one gating value and load-bearing once the reserved `Probe`/`Provision` values ([Q407](https://github.com/actions-gateway/github-actions-gateway/blob/main/docs/design/appendix-g-future-enhancements.md#g16-provisioningrequest-pre-acquire-capacity-probe-check-capacity)) join the same axis by soliciting an answer instead.
Renaming now cost nothing; renaming after the value shipped would have needed a conversion shim and a deprecation window.

`Observe` is **not** an audit or dry-run tier.
Every value except `Off` refuses jobs; they differ only in how the AGC learns the cluster cannot place the pod.

The capacity gate's mode enum was carrying two independent things: whether a set should refuse work it cannot run (a tenant's choice), and which signal may be trusted (a consequence of whether the cluster can grow — a property of the cluster, identical for every set in it).
Asking each tenant to assert the second meant asking them to speak for infrastructure they may not own, and it made the one harmful misconfiguration — gating on the scheduler's verdict where an autoscaler was about to add a node — reachable by the party least equipped to avoid it.

The two axes are now separate objects, and that combination is unrepresentable:

| Was, on the `RunnerSet` | Now, on the `RunnerSet` | Plus, on its `ActionsGateway` |
|---|---|---|
| `mode: SchedulerVerdict` | `mode: Observe` | `clusterCapacity.nodeAutoscaling: Absent` |
| `mode: AutoscalerVerdict` | `mode: Observe` | `clusterCapacity.nodeAutoscaling: Present` (the default — nothing to set) |
| `mode: On` | `mode: Observe` | whatever you already set — unchanged |
| `mode: Off` | unchanged | — |

```sh
# 1. If your cluster has NO node autoscaler, state that once per gateway.
#    Skip this entirely on a cluster that can grow — Present is the default.
kubectl patch actionsgateway -n <namespace> <gateway> --type=merge \
  -p '{"spec":{"clusterCapacity":{"nodeAutoscaling":"Absent"}}}'
```

```sh
# 2. Move each opted-in runner set to the single mode value.
kubectl patch runnerset -n <namespace> <runner-set> --type=merge \
  -p '{"spec":{"capacityGate":{"mode":"Observe"}}}'
```

**Order matters on a fixed-size cluster.** Apply the gateway patch first.
Between the CRD upgrade and the gateway patch, a set that was gating on the scheduler's verdict falls back to the autoscaler-declination signal, which on a cluster with no autoscaler simply never gates — you get today's un-gated behavior, never over-gating.

**If you upgrade the CRDs before the AGC** (they ship as separate charts), a set still carrying an old value reports `WorkerCapacityDeclined=False` with `reason: GateModeUnsupported` and is not gated — the fail-open direction.
See [troubleshooting](troubleshooting.md#the-mode-is-reported-as-unsupported).

### Non-breaking: an over-long derived `RunnerGroup` name is bounded (and renamed)

The GMC derives a `RunnerGroup`'s name from its gateway and first runner label (`<gateway>-<label>`), and the AGC stamps that name on every worker pod and agent Secret as the `actions-gateway/runner-group` label value.
Object names may be 253 characters but **label values stop at 63**, so a derived name past 63 produced a `RunnerGroup` that reconciled perfectly and then rejected every worker pod create.
A 15-character gateway name with a 40-character runner label was enough to reach it.
The derivation is now bounded to 63 characters, with a hash replacing whatever is cut (Q473).

`v1alpha1` only.
`v2alpha1`/`v2beta1` cap CR names at 52 characters for exactly this reason, so v2 tenants are unaffected.

**Almost every install sees no change at all**: a derived name that already fit is returned byte for byte as before, so existing `RunnerGroup`s keep their names.
Only a gateway whose derived name exceeded 63 characters is affected — on the first reconcile after the upgrade the GMC creates the correctly-named `RunnerGroup` and prunes the old one.
That tenant could not place a worker pod before the rename, so no working runner is disturbed; it starts working.

To check whether any of your gateways is affected before upgrading:

```sh
kubectl get runnergroups -A -o json \
  | jq -r '.items[] | select(.metadata.name | length > 63) | "\(.metadata.namespace)/\(.metadata.name)"'
```

Anything of yours keyed on such a name — dashboards, alerts, scripts — should move to the gateway's own name or a label selector.

### Non-breaking: a `RunnerSet`'s agent Secrets and runner names gain an `rs-` prefix

A v2 `RunnerSet`'s pre-registered agents now derive their identity from the kind as well as the name, so a `RunnerSet` and a same-named v1 `RunnerGroup` can run side by side through a migration without fighting over one pool (Q466):

| | Before | After |
|---|---|---|
| Agent Secret | `agentpool-<set>-<index>` | `agentpool-rs-<set>-<index>` |
| Secret label | `actions-gateway/runner-group=<set>` | `actions-gateway.com/runner-set=<set>` |
| GitHub runner name | `<set>-<index>` | `rs-<set>-<index>` |

The v1 `RunnerGroup` derivation is **unchanged** — a v1-only install sees nothing.

**The upgrade needs no action, and orphans nothing.** On its first reconcile after the upgrade, each `RunnerSet` moves its existing agent Secrets to the new names, carrying each agent's GitHub registration along, then deletes the old copies.
Nothing re-registers, so no runner record is stranded and no runner goes offline for the move.
The old runner *name* survives at GitHub until that agent is next recycled (which happens after its next job), at which point the old record is deregistered and the `rs-`-prefixed one replaces it — expect the names to change over gradually, not at once.

The one thing to check is **anything of yours that matches those names**: dashboards, alerts, scripts, or GitHub runner-name filters keyed on `agentpool-<set>-` or on a `RunnerSet`'s runner names.
Prefer the label selector, which is stable:

```sh
kubectl -n <tenant> get secret -l actions-gateway.com/runner-set=<set>
```

Agent Secrets also now carry an `ownerReference` to their `RunnerGroup` or `RunnerSet`, back-filled onto existing ones during the same reconcile, so deleting the owner reclaims them even if its finalizer never runs.

### Non-breaking: `v2alpha1` is deprecated and the apiserver now warns

All five `actions-gateway.com` kinds carry `deprecated: true` on their `v2alpha1` version, so `kubectl` prints a warning on any `v2alpha1` read or write:

```text
Warning: actions-gateway.com/v2alpha1 RunnerSet is deprecated; use actions-gateway.com/v2beta1. v2alpha1 is served until v2.0.0, which removes it.
```

**Nothing breaks, and the upgrade needs no action.** Deprecation marks intent and removes nothing: `v2alpha1` stays fully served, the conversion webhook still round-trips it against the `v2beta1` storage version, and existing objects keep reconciling untouched.
The warning is advisory client-side output; it does not fail an `apply`, and controllers or CI that ignore warnings are unaffected.
The AGC and GMC receive the same warning through their Kubernetes clients but log it once per unique message per process (deduplicated), so a controller reconciling `v2alpha1` objects under churn does not flood its own log.

**What to do about it.** Onboard new tenants on `v2beta1` (see [tenant onboarding](tenant-onboarding.md)), and move existing `v2alpha1` objects to `v2beta1` at your convenience by re-applying the same object with the `apiVersion` changed, since the two versions convert both ways.
`v2alpha1` remains the [`gag-migrate`](migration-v1-to-v2.md) on-ramp for tenants coming off `v1alpha1`, so a freshly migrated tenant emits these warnings until it is moved onto `v2beta1`.
Note that `v2beta1` is ScaleSet-only: a `RunnerSet` still on `acquisitionProtocol: Classic` must adopt the runner-scale-set protocol as part of that move (see [a Classic `RunnerSet` now runs on the scale-set protocol](#action-required-before-v200-a-classic-runnerset-now-runs-on-the-scale-set-protocol)).
`v2alpha1` is one of three things `v2.0.0` removes; the standing notice for all three, with a pre-upgrade checklist, is the [deprecation and removal notice](v1alpha1-deprecation.md).

### Non-breaking: `v2alpha1` CRDs ship in a separate, opt-in chart

The v2 (`actions-gateway.com`) API is introduced as a decomposed set of five CRDs — `actionsgateways`, `egressproxies`, `runnersets`, `runnertemplates`, and the cluster-scoped `clusterrunnertemplates`.
**The main `actions-gateway` chart upgrade is unchanged: it does not install these.** They ship in a separate, opt-in chart, `actions-gateway-crds-v2`, because the `RunnerTemplate`/`ClusterRunnerTemplate` CRDs each embed a full pod template (~1.1 MB apiece, served at `v2beta1` + `v2alpha1`) and would otherwise push the main chart's Helm release Secret past its hard 1 MiB limit.

**No action is required for existing tenants.** Install the v2 CRDs only when you want the v2 API available.
They are **applied server-side, not `helm install`ed** — the chart is over the 1 MiB release-Secret limit — and the same command upgrades them (re-apply with the newer tag to carry CRD field changes).
For the default `gmc-system` namespace, apply the signed manifest attached to the release:

```bash
kubectl apply --server-side -f \
  https://github.com/actions-gateway/github-actions-gateway/releases/download/vX.Y.Z/actions-gateway-crds-v2.yaml
```

For a custom GMC namespace, render the chart instead.
Either way, see [install.md § the v2 API CRDs](install.md#the-v2-api-crds) for the full lifecycle (upgrade/rollback/uninstall), signature verification, and namespace/cert-manager overrides:

```bash
helm template actions-gateway-crds-v2 oci://ghcr.io/actions-gateway/charts/actions-gateway-crds-v2 \
  --namespace <gmc-namespace> \
  | kubectl apply --server-side -f -
```

**Skipping the re-apply is the one v2 upgrade mistake that is not self-announcing, so the GMC refuses to start on it.** A structural schema prunes an undeclared field on write with no error, so a cluster left on the previous release's CRD accepts a manifest setting a newer field and stores nothing.
For [`spec.runnerGroup`](../design/05-security.md#unbounded-job-intake-via-the-installations-default-runner-group) that is a tenant boundary silently going away, not a feature going missing, so the GMC probes the installed schema at startup and exits with the field, the version, and this command rather than provisioning against a boundary that is not there.
Runbook: [troubleshooting.md](troubleshooting.md#gmc-exits-at-startup-an-installed-crd-schema-is-older-than-the-gmc).
From `v2.0.0` a skipped re-apply also [stops every AGC](#action-required-at-v200-apply-the-crds-with-the-upgrade-because-the-agcs-watch-depends-on-them), which the GMC's check does not catch.

The v2 controllers now reconcile these kinds, so a v2 object set provisions a working tenant.
Both groups are served side by side, so you can stay on the `v1alpha1` (`actions-gateway.github.com`) API until the **`v2.0.0`** release removes it, or migrate a tenant to v2 now with the one-shot fan-out tool: see [migration-v1-to-v2.md](migration-v1-to-v2.md) and the [deprecation and removal notice](v1alpha1-deprecation.md), which lists everything `v2.0.0` removes (`v1alpha1`, `v2alpha1`, and the Classic acquisition protocol) and the pre-upgrade checklist for it.
Note: v2's `ActionsGateway` reuses the `ag` short name, so once both groups are installed `kubectl get ag` is ambiguous — qualify it as `kubectl get actionsgateways.actions-gateway.github.com` (or `.com`) to disambiguate.

### BREAKING: `spec.namespaceQuota` removed — the ResourceQuota is now platform-owned

**This is a breaking CRD change, made pre-1.0 while the API can still break.** The `spec.namespaceQuota` field has been removed from the `ActionsGateway` CRD.
The namespace `ResourceQuota` (and any `LimitRange`) is now **platform-owned**: the platform admin creates and manages it on the tenant namespace, and the gateway operates within it but never creates or mutates it.
The GMC's `resourcequotas` write RBAC has been dropped (least privilege — Q122/Q130).
The rationale: a tenant-set quota is no real cap (the tenant could raise it in their own CR) and it fought GitOps and tenant-operator stacks (Capsule, HNC, vCluster, kiosk) that already own namespaces and quotas.

**What you must do before (or as part of) the upgrade:**

1. **Provision a platform-managed `ResourceQuota` in each tenant namespace** *before* the new GMC takes over — the gateway no longer creates one, so a namespace with no quota becomes uncapped.
   For each tenant that relied on `spec.namespaceQuota`, read the current values and create a standalone `ResourceQuota`:

   ```sh
   # Inspect the GAG-managed quota the old GMC created (named "actions-gateway")
   kubectl get resourcequota actions-gateway -n <tenant-namespace> -o yaml
   ```

   ```yaml
   apiVersion: v1
   kind: ResourceQuota
   metadata:
     name: <tenant>-quota
     namespace: <tenant-namespace>
   spec:
     hard:
       requests.cpu: "20"
       requests.memory: "40Gi"
       pods: "50"
   ```

2. **Adopt or replace the orphaned GAG-created quota.** A `ResourceQuota` the old GMC created carries an `ownerReference` to the `ActionsGateway` CR, so it would be garbage-collected if the CR were ever deleted.
   Either adopt it by stripping that ownerReference (so it survives independently), or delete it and recreate a platform-managed one as in step 1:

   ```sh
   # Adopt: drop the ownerReference so the quota is no longer GC-tied to the CR
   kubectl patch resourcequota actions-gateway -n <tenant-namespace> \
     --type=json -p='[{"op":"remove","path":"/metadata/ownerReferences"}]'
   ```

3. **Drop `namespaceQuota` from your `ActionsGateway` manifests / GitOps.** On upgrade the CRD's structural-schema pruning silently drops the now-unknown field from stored and re-applied CRs — applying a manifest that still sets `namespaceQuota` is **not rejected**, the field is just ignored.
   Remove it from source so intent stays clear.

### BREAKING: `priorityTiers` PriorityClasses now require a platform allowlist; per-tier `preemptionPolicy` removed

**Two breaking CRD/admission changes, made pre-1.0 (Q132).** Both concern `spec.runnerGroups[].priorityTiers`:

1. **The GMC validating webhook now rejects any `priorityClassName` not on the platform allowlist.** The allowlist is the new GMC `--allowed-priority-classes` flag (comma-separated class names) and is **empty by default**, so after upgrade *every* `ActionsGateway` that sets `priorityTiers` will be rejected on its next apply until you configure the flag.
   The rationale: a tenant-chosen, cluster-scoped `PriorityClass` with the default `PreemptLowerPriority` policy could evict other tenants' running worker pods — a cross-tenant isolation break.

2. **The per-tier `preemptionPolicy` field has been removed** from the `PriorityTier` schema.
   It was never wired to worker pods (a no-op) and was a tenant-controlled preemption lever; preemption is now governed solely by the platform-created `PriorityClass` object.
   Structural-schema pruning silently drops the now-unknown field from stored/re-applied CRs (no apply rejection); remove it from source so intent stays clear.

Migration steps:

1. **Before rolling the GMC**, decide which `PriorityClass` names tenants may use, ensure those classes exist (create them with `preemptionPolicy: Never` unless a tier is genuinely meant to preempt cross-tenant — see [security-operations.md § Priority classes](security-operations.md#priority-classes-the-allowed-priority-classes-allowlist)), and set `--allowed-priority-classes` on the GMC Deployment (or the chart values).
2. **Audit existing CRs** for the classes they reference: `kubectl get actionsgateway -A -o jsonpath='{range .items[*]}{range .spec.runnerGroups[*]}{range .spec.priorityTiers[*]}{.priorityClassName}{"\n"}{end}{end}{end}' | sort -u`.
   Every name in that list must be on the allowlist or the next apply/reconcile of that CR is rejected.
3. **Drop `preemptionPolicy` from your `priorityTiers` manifests / GitOps.**

### Tenant namespaces now require the `actions-gateway.github.com/tenant` marker label

The GMC's cluster-wide write grants are now gated by two ValidatingAdmissionPolicies (both shipped by the Helm chart, gated on `admissionPolicy.enabled`): `namespace-psa-guard` gates `namespaces:patch`, and `gmc-tenant-resource-guard` gates create/update/delete of all tenant provisioning resources (Deployments, Services, ServiceAccounts, RoleBindings, Roles, NetworkPolicies, HPAs, PDBs, RunnerGroups, and Secret create/update).
Both deny the GMC unless the target namespace already carries `actions-gateway.github.com/tenant: "true"`.
**Existing tenant namespaces created before this change do not have the label**, so after upgrade the GMC cannot stamp their Pod Security Admission labels *or provision any resources in them*, and each affected `ActionsGateway` will emit a `NamespaceMarkerMissing` warning event.

Before (or immediately after) upgrading, label every existing tenant namespace:

```sh
# Label all namespaces that currently hold an ActionsGateway CR.
kubectl get actionsgateway -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' \
  | sort -u \
  | xargs -I{} kubectl label namespace {} actions-gateway.github.com/tenant=true --overwrite
```

For a phased rollout where you cannot label every namespace up front, temporarily set **both** bindings' `validationActions` to `[Audit]` (instead of `[Deny]`) so denials are logged but not enforced, label the namespaces, then restore `[Deny]` on each.

From `v2.0.0` the marker is `actions-gateway.com/tenant=managed` instead ([why](#before-upgrading-to-v200-every-tenant-namespace-carries-the-actions-gatewaycom-labels)).

### Worker quota accounting now counts native sidecars, `RuntimeClass` overhead, and storage

The worker footprint behind `WorkerQuotaPressure`/`WorkerQuotaExceeded`, the pre-claim quota gate, and the capacity a scale-set advertises to GitHub previously summed CPU and memory over `podTemplate.spec.containers` only.
It now matches what Kubernetes charges, on both the compute and the storage keys:

- **Compute.** Native sidecars (`restartPolicy: Always` init containers) are summed in, and `RuntimeClass` pod overhead is added.
  Plain init containers still contribute only a `max()` floor, as before.
- **Storage.** `requests.ephemeral-storage` / `limits.ephemeral-storage` are summed like CPU and memory, and every **generic ephemeral volume** (`podTemplate.spec.volumes[].ephemeral`) is charged as the per-worker PVC it creates — against `persistentvolumeclaims`, `requests.storage`, and the `<class>.storageclass.storage.k8s.io/…` keys when the claim names a `storageClassName`.

**No behaviour change for a tenant with none of these.** A shape declaring no ephemeral storage and no ephemeral volumes is charged nothing on the storage keys, so a namespace already at its `persistentvolumeclaims` ceiling for unrelated reasons does not newly hold such a set back.
For a Docker-in-Docker (DinD) or Kata tenant the reported footprint goes **up**:

| Shape | Per-pod increase |
|---|---|
| Privileged DinD | `1` CPU / `3Gi` requests; `4Gi` memory limit |
| Kata | `2` CPU / `6Gi` requests; `4` CPU / `8Gi` limits; plus `250m` / `160Mi` overhead; plus `1` PVC and `100Gi` of `requests.storage` |

So an affected tenant whose quota was sized against the old numbers can come up with `WorkerQuotaPressure=True` (or `WorkerQuotaExceeded=True`) after upgrade with no change to the quota or the workload.
**Nothing got worse** — a compute shortfall was already quota-rejecting those pods at creation and retrying, and a storage shortfall was already stranding them `Pending` on a PVC that could not be created.
The condition now reports it up front instead of surfacing it as burnt lock time.
Re-derive the affected quotas with [sizing the platform-owned `ResourceQuota`](resourcequota-sizing.md).

Find the tenants this can affect before upgrading — any runner template with a native sidecar, a `runtimeClassName`, or a generic ephemeral volume:

```sh
kubectl get runnertemplate,clusterrunnertemplate -A -o json | jq -r '.items[] | select(any((.spec.podTemplate.spec.initContainers // [])[]; .restartPolicy == "Always") or (.spec.podTemplate.spec.runtimeClassName // "") != "" or any((.spec.podTemplate.spec.volumes // [])[]; has("ephemeral"))) | "\(.kind) \(.metadata.namespace // "-")/\(.metadata.name)"'
```

Reading `RuntimeClass` overhead needs a cluster-scoped `runtimeclasses` read, added to the per-gateway `agc-clusterrunnertemplate-reader` `ClusterRole` in the same release — so **upgrade the Helm chart, not just the AGC image**.
The read is fail-open: without the grant the overhead term is simply omitted and the conditions behave as they did before.
Verify after upgrading:

```sh
kubectl auth can-i get runtimeclasses --as=system:serviceaccount:<NAMESPACE>:<AGC_SERVICEACCOUNT>
```

### Worker pods are now cleaned up automatically (one-time sweep recommended)

AGC versions with Q95 delete completed worker pods after `completedPodTTL` (default 5m) and stuck-Pending worker pods after `pendingPodDeadline` (default 10m), and stamp every new worker pod and job Secret with an OwnerReference to its RunnerGroup.
Two operator-visible consequences:

- **Behaviour change:** completed worker pods no longer linger indefinitely.
  If your debugging workflow relied on inspecting old pods, raise `completedPodTTL` on the affected `runnerGroups[]` entries (see [tenant-onboarding](tenant-onboarding.md#step-2-create-the-gateway-and-its-runner-set)).
- **One-time sweep:** pods created by *pre-upgrade* AGC versions whose RunnerGroup still exists are reaped automatically after upgrade, but pods whose RunnerGroup was already deleted have no OwnerReference and no reconciler to reap them.
  Clean those up once per tenant namespace:

```sh
# Terminal worker pods left behind by pre-Q95 AGCs (label is stamped on all worker pods)
kubectl delete pods -n <tenant-namespace> \
  -l app.kubernetes.io/name=actions-runner \
  --field-selector 'status.phase!=Running,status.phase!=Pending'
```

### AGC Deployment renamed from `actions-gateway-agc` to `actions-gateway-controller`

This section is v1 history: it is the rename that gave v1 its fixed per-namespace name.
Under v2 the AGC is named per gateway as `<gateway>-agc`, so the v1 name below is the *old* name a second time over; see the v1 to v2 [migration guide](migration-v1-to-v2.md) for that move.

Deployments and resources created by the GMC are now named `actions-gateway-controller` instead of `actions-gateway-agc`.
After upgrading the GMC:

1. The GMC creates a new `actions-gateway-controller` Deployment in each tenant namespace.
2. The old `actions-gateway-agc` Deployment is left **orphaned** (still running but no longer managed).
   Remove it manually per tenant:

   ```sh
   kubectl delete deploy actions-gateway-agc -n <namespace>
   ```

3. Pods labelled `app=actions-gateway-agc` become `app=actions-gateway-controller`, which is v1's fixed label; on v2 the same label carries `<gateway>-agc` instead.
   Update any Prometheus alerts, Grafana dashboards, or PodMonitor selectors that reference the old label before upgrading, and prefer `app.kubernetes.io/name=actions-gateway-controller` where the selector has to survive the move to v2 as well.

### GMC manager NetworkPolicy is now enabled by default

The default install ships the GMC manager NetworkPolicy enabled (`networkPolicy.enabled=true`).
This flips the controller-manager pod to default-deny ingress and admits its `:8443` `/metrics` endpoint **only** from namespaces labelled `metrics: enabled`.
**If your Prometheus runs in an unlabelled namespace, GMC manager scrapes will start failing after upgrade.** Label it before (or right after) upgrading:

```sh
kubectl label namespace <prometheus-namespace> metrics=enabled --overwrite
```

The validating-webhook port (`9443`) is re-allowed from any source, so CR admission is unaffected.
This change also adds a `PodDisruptionBudget` (`minAvailable: 1`) and `priorityClassName: system-cluster-critical` to the manager — no operator action required.
Runtime NetworkPolicy enforcement depends on your CNI; see [observability.md](observability.md).
The Prometheus `ServiceMonitor` remains **opt-in** behind the `metrics.serviceMonitor.enabled` chart value.

---

## GMC Upgrade

The GMC runs `replicas: 2` with leader election.
Only one replica reconciles at any time.
The outgoing leader releases its lease on shutdown, so leadership transfers in seconds during a rolling update rather than at a lease timeout.
In-flight reconciliations are idempotent — the new leader re-derives state and converges without producing duplicate resources.

The active replica releases its leader lease on graceful shutdown (`--leader-elect-release-on-cancel`, on by default), so during a rollout the standby takes over within one retry period (~2s) instead of waiting out the full lease (~15s).
This is why the Deployment's short `terminationGracePeriodSeconds: 10` introduces no reconcile gap.
If you run on a slow or heavily loaded API server and see spurious leader-lease losses (the GMC restarting with "failed to renew lease"), widen the timing with the tunables below rather than disabling leader election:

| Flag | Default | Purpose |
|---|---|---|
| `--leader-elect-lease-duration` | `15s` | How long a candidate waits before force-acquiring a stale lease. |
| `--leader-elect-renew-deadline` | `10s` | How long the leader keeps retrying a renewal before stepping down. |
| `--leader-elect-retry-period` | `2s` | Interval between election attempts (and the failover floor with release-on-cancel). |
| `--leader-elect-release-on-cancel` | `true` | Release the lease on SIGTERM for fast failover. Leave on. |

The invariant `lease-duration > renew-deadline > retry-period × 1.2` is validated at startup; a misordered set makes the GMC exit immediately with a message naming the offending flags.

### A GMC restart costs tenants nothing; a GMC upgrade rolls every one of them

The GMC renders each tenant's AGC `Deployment` from its own configuration, so what a GMC replacement costs tenants turns entirely on whether that configuration changed.

**Restart, failover, eviction, node drain — no tenant impact.** A replacement leader re-derives every AGC pod template identically and writes nothing, so no new `pod-template-hash` and no new `ReplicaSet` appear and no AGC pod is replaced.
This holds for any restart of an unchanged GMC.

**Upgrade — every tenant's AGC rolls, all at once.** These GMC-side values are part of the AGC pod template, and a release normally changes at least the first:

| Value in the AGC pod template | Where the GMC gets it |
|---|---|
| AGC container image | `AGC_IMAGE` on the GMC pod (chart value `agc.image`) |
| `WRAPPER_IMAGE`, `WRAPPER_DELIVERY` | GMC pod environment |
| `AGC_EXTRA_*` (testing only) | GMC pod environment, behind `--allow-agc-extra-env` |

Every tenant whose template changes rolls, so a GMC upgrade fans out into one [AGC drain window](#agc-upgrade) per tenant, concurrently and unstaged — closed sessions and no job acquisition across the whole fleet rather than in one namespace.
Schedule a GMC upgrade in the low-traffic window you would give an AGC upgrade, and validate with the [AGC post-upgrade checks](#agc-upgrade) as well as the GMC ones.

**Do not hand-edit a GMC-managed AGC `Deployment`.** The reconciler watches the Deployments it owns and replaces the whole spec from its own render, so a `kubectl set image` — or any other direct edit — is reverted within seconds.
`kubectl rollout restart` is the one exception, and its history is a trap of its own: see [`kubectl rollout restart` of a Managed Deployment Reports Success but Nothing Restarts](troubleshooting.md#kubectl-rollout-restart-of-a-managed-deployment-reports-success-but-nothing-restarts).

### GMC install and upgrade via Helm (recommended)

The shipped install artifact is the **`actions-gateway` Helm chart**, published and cosign-signed to the GHCR OCI registry (`oci://ghcr.io/actions-gateway/charts/actions-gateway`); the [`charts/actions-gateway/`](../../charts/actions-gateway/README.md) source path is the dev/CI copy of the same chart.
The chart is the **sole** install/upgrade vehicle — there is no kustomize path.
For dev/CI iteration `make deploy` wraps `helm install` of the local chart with floating image tags.

> **Current release — `v1.10.0`** (chart version = release tag without the leading `v`).
> Pin `--version 1.10.0`; copy the image digests from the [release notes](https://github.com/actions-gateway/github-actions-gateway/releases/tag/v1.10.0) and verify the chart/image signatures before installing (see [release.md § Verify the publish](release.md#3-verify-the-publish)).

```sh
# First install (from the published, signed OCI chart)
helm install gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version 1.10.0 \
  --namespace gmc-system --create-namespace \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy> \
  --set wrapper.image.digest=sha256:<wrapper>

# Apply the chart-root CRDs first — EVERY upgrade, not just some, except 1.x to
# v2.0.0, where they go after the upgrade (see "apply the CRDs with the upgrade").
# Helm installs that directory on `helm install` only and skips it on upgrade, so
# this is the only way changes to those CRDs reach an existing release. Idempotent,
# and version-correct: it reads the CRDs out of the exact chart you are upgrading to.
# Pull first: `helm show crds oci://...` prints its pull banner on stdout
# (measured on Helm 4.2), and kubectl rejects the banner.
helm pull oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version <new-chart-version> --untar --untardir gag-chart-<new-chart-version>
helm show crds gag-chart-<new-chart-version>/actions-gateway | kubectl apply -f -

# Upgrade in place to a newer published chart version (carries CRD field changes — see below)
# --reset-then-reuse-values, NOT --reuse-values: see the values-reuse note below
helm upgrade gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version <new-chart-version> --namespace gmc-system --reset-then-reuse-values \
  --set gmc.image.digest=sha256:<new-gmc>

# Roll back to the previous release (from v2.0.0 to 1.x, see "GMC rollback")
helm rollback gag --namespace gmc-system
```

The following upgrade-time behaviors are specific to this chart:

- **Use `--reset-then-reuse-values`, not `--reuse-values`.** `--reuse-values` replays the *old* release's values over the *old* chart's defaults, so any values key introduced after your release was created is simply absent — and a template reading a field under it fails the render with an opaque Go error naming a file you did not touch:

  ```text
  Error: UPGRADE FAILED: actions-gateway/templates/vpa.yaml:1:14
    executing "actions-gateway/templates/vpa.yaml" at <.Values.vpa.enabled>:
      nil pointer evaluating interface {}.enabled
  ```

  `--reset-then-reuse-values` (Helm ≥ 3.14) starts from the *new* chart's defaults and layers your release's values on top, so new keys get their intended defaults and everything you set is preserved.
  It is the correct flag for every upgrade of this chart, not just ones that hit the error.

  The chart deliberately does **not** paper over this by making templates tolerate a missing block.
  A missing block would then render as "unset", and for a security-relevant key — `admissionPolicy`, `networkPolicy` — that silently disables a guard on upgrade.
  Failing the render loudly is the safe direction; reaching for the right flag is the fix.
  See the [troubleshooting runbook](troubleshooting.md#helm-upgrade-fails-with-nil-pointer-evaluating-interface-field).
- **All four image digests are required at render time.** Both `helm install` and `helm upgrade` fail with `<image>.image must be pinned by digest …` (naming `gmc`/`agc`/`proxy`/`wrapper`) when the release values carry no digest for one of the four — e.g. a values file that omits it, or `--reset-values`.
  `--reset-then-reuse-values` and `--reuse-values` both carry the previously pinned digests forward; pass `--set <image>.image.digest=sha256:<new>` for each image you are moving to the new release's build.
  See the [troubleshooting runbook](troubleshooting.md#helm-render-fails-gmcimage-must-be-pinned-by-digest).
  Dev/test only: `allowFloatingImageTags=true` opts out.
- **Helm never upgrades this chart's one CRD, so apply the chart's CRDs every time.** The `PriorityClassAllowlist` CRD cannot ship as a template: the same release *creates* a `PriorityClassAllowlist` object, and Helm resolves REST mappings for the whole manifest before applying any of it, so a CR whose CRD is a template in the same release fails outright.
  It therefore ships in the chart-root `crds/` directory — which Helm installs on `helm install` **only, never on upgrade**.
  The tenant CRDs are not in this chart: they ship in `actions-gateway-crds-v2`, applied with `kubectl apply --server-side`.

  Rather than make that a conditional "did your release predate X?" step, the upgrade command above applies the chart's CRDs unconditionally:

  ```bash
  helm pull oci://ghcr.io/actions-gateway/charts/actions-gateway \
    --version <new-chart-version> --untar --untardir gag-chart-<new-chart-version>
  helm show crds gag-chart-<new-chart-version>/actions-gateway | kubectl apply -f -
  ```

  It is idempotent (a no-op when nothing changed), needs no local checkout, and reads the CRDs from the exact chart version you are upgrading to — so it stays correct for any future schema change without a release-note callout to remember.
  The one exception is 1.x to `v2.0.0`, where [it goes after the upgrade](#action-required-at-v200-apply-the-crds-with-the-upgrade-because-the-agcs-watch-depends-on-them).
  The chart also preflights the CRD's presence and fails with this command if you skip it, so a missed step costs you a re-run, not a broken cluster.
- **The webhook cert path depends on `certManager.enabled`.** With the default `certManager.enabled=true`, cert-manager issues and rotates the serving cert; nothing to do on upgrade.
  With `certManager.enabled=false`, the chart generates a self-signed serving cert and wires the webhook `caBundle` itself.
  On an in-place `helm upgrade` the chart **reuses the existing `webhook-server-cert` Secret** (it looks the Secret up), so the cert does not rotate; it only regenerates if that Secret is missing (a fresh install, or after you delete it to force rotation).
  A `helm template` (no cluster) cannot look the Secret up and therefore renders a fresh cert each time — expected for offline rendering only.
- **`priorityClassAllowlist.configMapName` is removed (Q492) — a breaking values change.** The PriorityClass allowlist moved from a watched ConfigMap to the cluster-scoped `PriorityClassAllowlist` CR, which is now both the GMC's dynamic source and the guard policy's `paramKind`.
  A release that sets the old key fails the render with a migration message; every other release needs no values change, because the chart renders the new object from the `allowedPriorityClasses` it already rendered the ConfigMap from.
  Full steps: [PriorityClass allowlist: ConfigMap to CR](#priorityclass-allowlist-configmap-to-cr) below.
  (The CRD itself is covered by the apply step above, like any other chart-root CRD.)
- **`allowedInfraPriorityClasses` is new (Q298) and needs no upgrade action unless you set it.** The `PriorityClassAllowlist` CR gained a second list, so the infra PriorityClass allowlist can be grown without a GMC restart like the worker one.
  The chart renders the key only when the value is non-empty, so an upgrade that has not adopted it applies cleanly against the CRD your *current* release installed — no ordering requirement.
  If you do set it, apply the chart's CRDs first (the step above): a field the stored CRD does not declare fails server-side apply **midway** through the upgrade, so the chart preflights it and fails at render with the same command instead.
  This is a stale *schema*, not a missing kind, which the older CRD-presence preflight cannot see.
- **A hand-patched release blocks the next `helm upgrade`.** Helm 4 applies server-side, so a field owned by a different field manager is a hard conflict rather than a silent overwrite.
  If you have `kubectl patch`ed or `kubectl edit`ed a chart-owned object — the GMC Deployment's container `args` is the usual one — the next upgrade fails outright:

  ```text
  Error: UPGRADE FAILED: conflict occurred while applying object gmc-system/gmc-controller-manager
  apps/v1, Kind=Deployment: Apply failed with 1 conflict: conflict with "kubectl-patch" using
  apps/v1: .spec.template.spec.containers[name="manager"].args
  ```

  Re-run the upgrade with `--force-conflicts` to hand the field back to Helm — your manual edit is reverted, which is the point of the flag.
  The durable fix is to express the change in chart values so it survives every upgrade instead of being reapplied by hand.
  Do **not** reach for `--force-replace`: it replaces objects wholesale and is destructive.
- **The `namespace-psa-guard` and `gmc-tenant-resource-guard` bindings deny by default.** If you are upgrading a cluster whose existing tenant namespaces are not yet labeled `actions-gateway.com/tenant=managed`, label them **before** the upgrade (see the migration note above), or the GMC's namespace patches *and all tenant-resource writes* will be denied.
  To stage the rollout you can temporarily set both bindings to `Audit` by editing `validationActions` on each `ValidatingAdmissionPolicyBinding`, then flip them back to `Deny` once the labels are in place.

### PriorityClass allowlist: ConfigMap to CR

**Who this affects:** everyone, for step 1 — which is now simply part of every upgrade of this chart, not a one-off.
Step 2 additionally applies if you set `priorityClassAllowlist.configMapName`.

**What changed.** The watched allowlist is now a cluster-scoped `PriorityClassAllowlist` CR named `<release>-priorityclass-allowlist`, rendered by the chart from `allowedPriorityClasses`.
It is both the GMC's restart-free dynamic source (Q188) and the `priorityclass-allowlist-guard` policy's `paramKind`.

**Why it had to move.** A `paramKind` on a *core* type such as `ConfigMap` is permanently broken for the rest of a kube-apiserver process once the set of bindings naming it goes empty for one refresh tick — which `helm uninstall` does.
That was the Q444 cluster-wide outage: every `runnergroups`/`runnersets`/ `runnertemplates` write denied with `no params found for policy binding`, and no recovery short of a kube-apiserver restart.
A CRD `paramKind` gets a fresh dynamic informer per context and survives the same transition, so this removes the exposure rather than mitigating it.

**Why the CRD needs a manual step.** This is the one CRD the chart ships in the chart-root `crds/` directory rather than `templates/crds/`, because the same release also *creates* a `PriorityClassAllowlist` object — and Helm resolves REST mappings for the entire manifest before applying any of it, so a CR whose CRD is a template in the same release fails the install outright.
`crds/` is the only directory Helm installs early enough.
The cost is that Helm skips `crds/` entirely on upgrade, so an existing release never receives it.

#### 1. Apply the chart's CRDs

```bash
helm pull oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version <new-chart-version> --untar --untardir gag-chart-<new-chart-version>
helm show crds gag-chart-<new-chart-version>/actions-gateway | kubectl apply -f -
```

This is the standard pre-upgrade step for every release but `v2.0.0` ([above](#gmc-install-and-upgrade-via-helm-recommended)), not something special to this migration — running it unconditionally is what keeps the procedure the same whatever version you are coming from.
From a chart directory, `helm show crds charts/actions-gateway | kubectl apply -f -` does the same thing.

Skip it and the upgrade stops before changing anything, naming the command:

```text
Error: UPGRADE FAILED: execution error at (actions-gateway/templates/priorityclass-allowlist.yaml):
the PriorityClassAllowlist CRD is not installed in this cluster. Helm installs the
chart-root crds/ dir on a fresh install ONLY and skips it on every upgrade, so
applying it is a standard pre-upgrade step for this chart. ...
```

The check is skipped on a fresh `helm install`, where `crds/` is applied for you.

#### 2. Move any watched-ConfigMap names into `allowedPriorityClasses`

Only if your release sets `priorityClassAllowlist.configMapName`.
That key is removed, and the render fails while it is set:

```text
priorityClassAllowlist.configMapName ("gmc-priority-class-allowlist") is removed as
of Q492. ...
```

Read the names the ConfigMap currently carries — including any added self-service since install, which will **not** be in your values file.
Use *your* configured name, not the example (`helm get values <release> -n gmc-system` shows it under `priorityClassAllowlist.configMapName`):

```bash
kubectl get configmap -n gmc-system <your-configmap-name> -o jsonpath='{.data.allowedPriorityClasses}'
```

Put the **union** of those and your existing `allowedPriorityClasses` into `allowedPriorityClasses`, and unset the removed key:

```yaml
allowedPriorityClasses:
  - runner-standard      # was already in values
  - runner-bursty        # was ConfigMap-only
  - runner-batch         # was ConfigMap-only
priorityClassAllowlist:
  configMapName: ""
```

Getting this wrong fails **closed**, not open: a name you drop stops being admitted, denying writes that name it.
Nothing becomes permitted that was not permitted before.

#### 3. Upgrade, then verify

```bash
kubectl get priorityclassallowlist gmc-priorityclass-allowlist -o jsonpath='{.spec.allowedPriorityClasses}'
```

The GMC's grant becomes a `ClusterRole` with `get`/`list`/`watch` on `priorityclassallowlists` (the kind is cluster-scoped); it carries no write verb, so the GMC still cannot widen its own allowlist.
No manual RBAC step.

**What the upgrade cleans up depends on which case you were in.** Helm only removes objects it rendered:

| You had | Helm removes on upgrade | You remove by hand |
|---|---|---|
| `configMapName` **unset** (default) | the chart-rendered param ConfigMap `<release>-priorityclass-allowlist` | nothing |
| `configMapName` **set** | the namespaced `Role`/`RoleBinding` the chart rendered for that watch | your own ConfigMap — the chart never rendered it, so Helm will not touch it |

So if you set `configMapName`, delete your ConfigMap once you are satisfied the CR carries everything.
Nothing reads it any more:

```bash
kubectl delete configmap -n gmc-system <your-configmap-name>
```

**Your self-service workflow moves to the CR.** Adding a class without a GMC rollout used to mean editing that ConfigMap; it now means editing the CR, which the GMC watches the same way:

```bash
kubectl edit priorityclassallowlist <release>-priorityclass-allowlist
```

A chart upgrade reasserts `allowedPriorityClasses` over it, exactly as it would have reasserted a chart-rendered ConfigMap — so persist anything durable in values.
Full detail: [security-operations.md](security-operations.md#self-service-additions-via-the-priorityclassallowlist-cr-q188-q298).

#### Rolling back past this change re-arms the outage it fixes

**`helm rollback` to a revision from before this change is not safe on a running cluster.** It reports success, and then every `runnergroups`/`runnersets`/ `runnertemplates` write is denied cluster-wide with `no params found for policy binding` — the exact Q444 outage this change removes.

Measured on kind v1.36.1: install `v1.2.0`, upgrade to this chart, `helm rollback` to revision 1.
Rollback succeeds, the `paramKind` reverts to `ConfigMap`, and a class-free `RunnerGroup` write is denied on every attempt across a 20-second window — it does not clear.

The reason is the defect itself.
Upgrading to the CRD `paramKind` removes the last binding naming `v1/ConfigMap`, which is precisely the transition that kills that apiserver's shared ConfigMap param informer for the life of the process.
The rollback then recreates a ConfigMap-`paramKind` binding, and that informer is never coming back — so it can never resolve, exactly as a fresh install could not after the break.

**Roll forward, not back.** If you need to undo this release for an unrelated reason, do it in a way that does not recreate a ConfigMap-`paramKind` binding — upgrade to the older chart version with the guard disabled rather than rolling back to it:

```bash
helm upgrade gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version <older-chart-version> --namespace gmc-system \
  --reset-then-reuse-values --set admissionPolicy.enabled=false
```

Verified: writes are admitted again within seconds.
You lose the PriorityClass backstop until you roll forward again, so treat it as an incident mitigation — the GMC webhook still gates the tenant-facing CRs meanwhile.
The other recovery is a kube-apiserver restart, which is not available on EKS/GKE/AKS.

Rolling back a release that has *always* been on the CR `paramKind` is unaffected — this is specifically about crossing the ConfigMap→CR boundary downwards.

`helm upgrade` rolls the GMC Deployment (and carries additive CRD field changes — no separate CRD apply step).
Watch the rollout:

```sh
kubectl rollout status deploy/gmc-controller-manager -n gmc-system
```

The rolling update replaces one replica at a time.
Leadership transfers before the old leader is deleted.
The total rollout time is typically < 30 seconds.

### GMC post-upgrade validation

```sh
# Confirm both replicas are on the new image
kubectl get pods -n gmc-system -o wide

# Confirm the GMC has re-elected a leader
kubectl get lease -n gmc-system

# Confirm no new reconcile errors appeared
# Metric: controller_runtime_reconcile_errors_total

# Spot-check one ActionsGateway CR
kubectl describe actionsgateway -n <namespace> <name>
```

### GMC rollback

Roll back to the previously deployed release with `helm rollback` (see the Helm section above):

```sh
helm rollback gag --namespace gmc-system
kubectl rollout status deploy/gmc-controller-manager -n gmc-system
```

`helm rollback` restores the prior release's values and manifests.
CRDs carry `helm.sh/resource-policy: keep`, so they are not rolled back automatically; if the rollback targets a different CRD schema version, consult the release notes for any CRD migration.

**From `v2.0.0` back to 1.x, put the CRDs back first.** A `v1.10.0` GMC refuses to start without `v2alpha1` and `v2beta1`, and its release's manifest names the `PriorityClassAllowlist` at a version the `v2.0.0` chart-root CRD no longer serves.
So both sets of CRDs go back before the chart.
Read the revision from `helm history` at that moment, and pick the newest `superseded` one on a 1.x chart.
Do not leave it out: with no revision `helm rollback` goes to the previous one, which after any further upgrade is still `v2.0.0`, and it exits 0 with `rollout status` passing.
Helm also prunes old revisions, so one too old fails with `release has no 1 version`.

```sh
helm history gag --namespace gmc-system
helm pull oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version 1.X.Y --untar --untardir gag-chart-1.X.Y
helm show crds gag-chart-1.X.Y/actions-gateway | kubectl apply -f -
kubectl apply --server-side --force-conflicts -f \
  https://github.com/actions-gateway/github-actions-gateway/releases/download/v1.X.Y/actions-gateway-crds-v2.yaml
helm rollback gag <revision> --namespace gmc-system
kubectl rollout status deploy/gmc-controller-manager -n gmc-system
```

`--force-conflicts` is required after the [`v2.0.0` patch](#action-required-at-v200-remove-the-crds-conversion-webhook-before-applying-them), because that patch owns `.spec.conversion.strategy`.

**A `v2.0.0` AGC already running survives this gap too**, measured on kind against `test/fakegithub` with workers held `Pending`; a job running through the gap and github.com were not measured.
On the restored CRDs it logged `field label not supported` on each watch retry, kept its session, and took a job queued in the gap.
A `RunnerSet` edited in the gap was admitted and left unreconciled, with `Ready` still reporting the old generation, until the `v1.10.0` AGC took over; that AGC then reconciled it through the restored conversion webhook and had a session within 30 seconds.
An AGC pod restarted in the gap reads `Ready`, holds no session, and exits after about two minutes, as [the upgrade note](#action-required-at-v200-apply-the-crds-with-the-upgrade-because-the-agcs-watch-depends-on-them) describes.
Keep this gap short as well; the chart-first order was not measured for a rollback.
Between the applies and the rollback, a read at `v2alpha1` or `v2beta1` fails because a `v2.0.0` GMC has no conversion endpoint, and a write at `v2beta1` is denied with `failed to convert object version`:

```text
Error from server: conversion webhook for actions-gateway.com/v2, Kind=RunnerTemplate failed: the server could not find the requested resource
```

Measured on kind with Kubernetes 1.36.1 and cert-manager v1.20.2, rolling back to the `v1.10.0` chart with no restart: every command exited 0, two `v1.10.0` pods were Ready, reads at all three versions returned a `RunnerTemplate` stored at `v2`, and the guard denied an off-allowlist write.

**If `helm rollback` ran with the chart-root CRD still on `v2` alone,** it exits 1 in one of two forms, depending on what the client has cached about the API:

```text
Error: unable to build kubernetes objects from new release manifest: resource mapping not found for name: "gmc-priorityclass-allowlist" namespace: "" from "": no matches for kind "PriorityClassAllowlist" in version "actions-gateway.com/v2beta1"
ensure CRDs are installed first
```

With this form the Deployment is unchanged and the revision reads `pending-rollback`, and `helm upgrade` then fails with `another operation (install/upgrade/rollback) is in progress` until a rollback succeeds.
The other form is `server-side apply failed for object /gmc-priorityclass-allowlist`, with the revision `failed`, the Deployment changed, and a `v1.10.0` pod crash-looping beside the `v2.0.0` pods.
Either way, run the block above again.

**If `helm rollback` ran with the chart-root CRD still serving `v2beta1`,** which is the state between the `helm upgrade` and the chart-root apply, it reports `Rollback was a success!` and reads `deployed`, or exits 1 and reads `failed` with `--wait`.
The new pod crash-loops, and the `v2.0.0` pods keep running beside it.
Its log says the CRDs are older than the GMC, which is backwards here, since they are newer:

```text
verify installed CRD schemas: the CustomResourceDefinitions installed in this cluster are older than this GMC and no longer declare a field that bounds tenant access:
  - runnersets.actions-gateway.com at v2beta1 does not declare spec.runnerGroup
```

The plain `kubectl apply --server-side` the log recommends is refused with a conflict on `.spec.conversion.strategy` and changes nothing.
The forced apply above is the recovery, and no restart is needed: the pods went Ready on their own in up to about four minutes.

Going forward again by the [`v2.0.0` steps](#action-required-at-v200-apply-the-crds-with-the-upgrade-because-the-agcs-watch-depends-on-them) succeeded after the block above.
The failure shapes above were measured with the GMC and one `RunnerTemplate`; the clean path, with the AGC running through it, was also measured with a tenant gateway.
A field that only the `v2.0.0` schema declares is expected to be pruned from an object the next time it is written, as [install.md](install.md#the-v2-api-crds) warns; that part was not measured.

---

## AGC Upgrade

The AGC runs `replicas: 1`.
**Every AGC upgrade incurs a brief drain window** — the period between the old pod terminating and the new pod opening its sessions.
During this window:

- **No job is acquired.** Each `RunnerSet`'s message session is closed, so jobs wait in GitHub's queue for that scale set.
- **Unacknowledged assignments replay.** The new session receives them again, and the AGC skips any it already provisioned a worker for.
- **Running worker pods are unaffected.** They are separate from the AGC and keep running their jobs.

**Scheduling guidance.** Schedule AGC upgrades during low-traffic periods (off-peak hours, weekends), when the fewest jobs are waiting to start.

### Per-Tenant Upgrade Procedure

The image change itself is fleet-wide (Step 2), so the per-tenant work is the drain that precedes it and the validation that follows: run Step 1 for each tenant you want drained cleanly, then Steps 3–5 per namespace.

**Step 1: Let the AGC drain (optional, reduces blackout window)**

The AGC's SIGTERM handler deletes each `RunnerSet`'s message session before exiting.
A scale set allows one session at a time, so the next AGC cannot open its session until the previous one is gone.
To rely on this:

- Ensure `terminationGracePeriodSeconds` on the AGC Deployment is ≥ 30 seconds (the GMC stamps the AGC Deployment with 60s by default).
  Each session delete is bounded at 10 seconds.
- Do not use `kubectl delete pod` directly — it sends SIGKILL without a grace period.
  Use `kubectl rollout restart`, which the reconciler honours on a managed Deployment.

If a session cannot be deleted (GitHub is unreachable during the drain), the new AGC's session is refused until GitHub expires the old one: see [`ScaleSetListenerStartFailed`](troubleshooting.md#scaleset-runnerset-stuck-not-ready-scalesetlistenerstartfailed-naming-the-guard-configmap).

**Step 2: Update the AGC image**

The GMC manages the AGC Deployment, so the AGC image is a GMC-level setting, not a per-namespace one: update the GMC's Helm values with the new AGC image and upgrade the GMC, which then rolls **every** tenant's AGC Deployment at once — see [A GMC restart costs tenants nothing; a GMC upgrade rolls every one of them](#a-gmc-restart-costs-tenants-nothing-a-gmc-upgrade-rolls-every-one-of-them) before you schedule it.

There is no per-namespace alternative.
`kubectl set image` on the AGC Deployment appears to work and is reverted on the next reconcile, because the reconciler replaces the managed Deployment's whole spec from its own render.

**Step 3: Watch the rollout**

The AGC Deployment is named per gateway, as `<gateway>-agc`.

```sh
kubectl rollout status deploy/<gateway>-agc -n <namespace>
```

**Step 4: Confirm session recovery**

```sh
# every RunnerSet's listener session should be open again within a few seconds of pod startup
# Metric: actions_gateway_scaleset_session_open{namespace="<namespace>"} == 1 per runner_set

# No new message poll errors
# Metric: rate(actions_gateway_message_poll_errors_total{namespace="<namespace>"}[5m])
```

**Step 5: Check for cancelled jobs**

After the rollout, verify that jobs active during the restart have either completed or been redelivered.
Check the GitHub Actions UI for any unexpectedly cancelled runs.

### AGC rollback

Roll the AGC image back where the GMC reads it, not on the Deployment — `kubectl rollout undo` on a managed AGC Deployment is reverted on the next reconcile, same as `kubectl set image`:

```sh
# Re-run the GMC upgrade pinning the previous AGC digest, then watch the AGCs converge
helm upgrade gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version <chart-version> --namespace gmc-system --reset-then-reuse-values \
  --set agc.image.digest=sha256:<previous-agc>
kubectl rollout status deploy/<gateway>-agc -n <namespace>
```

Then confirm sessions are re-established and job acquisition resumes.

---

## Proxy Upgrade

The proxy pool is HPA-managed and stateless.
Rolling updates are non-disruptive as long as the `PodDisruptionBudget` (`minAvailable: 1`) is respected during the rollout.

Each terminating pod runs a graceful shutdown before exiting: it briefly holds its CONNECT listener open so endpoint removal can propagate (ending early once new connections stop arriving), then drains in-flight tunnels.
Both stages share one 45s budget inside a 60s `terminationGracePeriodSeconds`, so a pod with quiet traffic typically terminates in a few seconds and a busy one takes longer.
Pods that appear to hang for tens of seconds during a rollout are draining, not stuck — see [Proxy Tunnel Cut During a Rollout](troubleshooting.md#proxy-tunnel-cut-during-a-rollout) for the budget breakdown, and the spot/preemptible note beneath it if your nodes reclaim on a short notice.

### Step 1: Pre-Upgrade Checks

```sh
# Confirm the PodDisruptionBudget is in place
kubectl get pdb -n <namespace> <proxy>-proxy

# Confirm current replica count
kubectl get deploy -n <namespace> <proxy>-proxy

# Confirm the HPA is healthy (TARGETS should show a percentage, not <unknown>)
kubectl get hpa -n <namespace>
```

### Step 2: Update the Proxy Image

The GMC manages the proxy Deployment.
Update the proxy image via the GMC's Helm values or Kustomize overlay, then re-deploy the GMC, which reconciles the updated image into all tenant proxy Deployments.
There is no per-namespace alternative, for the reason [the AGC's Step 2](#per-tenant-upgrade-procedure) gives: an image set on the managed Deployment is reverted on the next reconcile.

### Step 3: Watch the Rollout

The rolling update replaces one proxy pod at a time.
Kubernetes honours the `PodDisruptionBudget` and only terminates a pod once its replacement is `Ready`.

```sh
kubectl rollout status deploy/<proxy>-proxy -n <namespace>
```

In-flight `CONNECT` tunnels through the old proxy pod will be interrupted when that pod is terminated.
The AGC and worker pods will reconnect through the remaining proxy pods automatically.
For high-concurrency tenants, schedule the upgrade during a low-traffic window to minimise connection resets.

### Step 4: Post-Upgrade Validation

```sh
# All proxy pods on the new image
kubectl get pods -n <namespace> -l actions-gateway.com/egress-proxy=<proxy> -o wide

# HPA still computing utilization (not <unknown>)
kubectl get hpa -n <namespace>

# No spike in token or message poll errors after the rollout
# Metrics: actions_gateway_token_refresh_errors_total, actions_gateway_message_poll_errors_total
```

### Proxy rollback

Roll the proxy image back where the GMC reads it, as [the AGC rollback](#agc-rollback) does, not on the Deployment:

```sh
helm upgrade gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version <chart-version> --namespace gmc-system --reset-then-reuse-values \
  --set proxy.image.digest=sha256:<previous-proxy>
kubectl rollout status deploy/<proxy>-proxy -n <namespace>
```

---

## Worker Image Upgrade

Worker image upgrades are non-disruptive: the new image takes effect on future jobs; running pods complete on the old image.

### Upgrade Procedure

Update `spec.workerImage` on the `RunnerTemplate` the `RunnerSet` references:

```sh
kubectl patch runnertemplate -n <namespace> <name> \
  --type=merge -p '{"spec":{"workerImage":"<new-image-digest>"}}'
```

The AGC starts using the new image on the next job acquisition.
No restart required.

**Production recommendation:** pin to a digest, not a tag:

```yaml
workerImage: ghcr.io/my-org/actions-runner-worker@sha256:abc123...
```

This ensures the exact same image is used for all jobs until explicitly changed, and enables unambiguous rollback.

### Canary Testing a New Worker Image

To test a new image on a subset of jobs before rolling it out broadly:

1. Add a second `RunnerTemplate` with the new image, and a `RunnerSet` that references it with a distinct label (e.g.
   `canary`).
2. Update a subset of workflows to use `runs-on: [self-hosted, canary]`.
3. Monitor job success rates.
   If healthy, update the main `RunnerTemplate` and delete the canary set and template.

### Minimum Version Requirement

GitHub enforces a minimum runner version to register, and separately requires each new runner release be installed within 30 days of publication to keep executing jobs.
[Troubleshooting § Worker Image Runner Version](troubleshooting.md#worker-image-runner-version) carries the current floor and where it comes from.

You do not have to wait for a failure to see this.
Every reconcile, the AGC reads the runner version off the effective `workerImage` reference, then out of the image itself in its registry, and reports `RunnerVersionTooOld` with reason `WorkerImageBelowMinimum` (`True`), `WorkerImageCurrent` (`False`), or `WorkerImageVersionUnknown` (`Unknown`, while the registry read is pending or failed and the reference names no version, or when the image is not `actions/runner`-derived).
Check it right after deploying a different worker image:

```sh
kubectl get runnerset <name> -n <namespace> \
  -o jsonpath='{.status.conditions[?(@.type=="RunnerVersionTooOld")]}'
```

See [troubleshooting § Worker Image Runner Version](troubleshooting.md#worker-image-runner-version) for reading the version a running worker actually shipped.

### Worker image rollback

Set `workerImage` back to the previous digest:

```sh
kubectl patch runnertemplate -n <namespace> <name> \
  --type=merge -p '{"spec":{"workerImage":"<previous-digest>"}}'
```

---

## Post-Upgrade Validation

After any component upgrade:

```sh
# All ActionsGateway CRs healthy
kubectl get actionsgateway --all-namespaces

# Listener sessions restored
# Metric: actions_gateway_scaleset_session_open per namespace and runner_set

# No spike in errors
# Metrics: actions_gateway_token_refresh_errors_total, actions_gateway_message_poll_errors_total, controller_runtime_reconcile_errors_total

# Pod creation latency within SLO
# Metric: histogram_quantile(0.95, rate(actions_gateway_pod_creation_latency_seconds_bucket[5m]))
```

If a regression is detected within the first 15 minutes after an upgrade, roll back immediately rather than investigating in production.
Investigate using a non-production environment.

---

## Zero-Downtime Configuration

Worker image upgrades are non-disruptive.
An AGC upgrade has a brief drain window, and a GMC upgrade [rolls every tenant's AGC](#a-gmc-restart-costs-tenants-nothing-a-gmc-upgrade-rolls-every-one-of-them), so it has the same one.
To minimize its impact:

- **Time upgrades outside peak hours** to reduce the number of jobs waiting to start.
- **Rely on SIGTERM drain** — `kubectl rollout restart` (not `delete pod`) gives the AGC time to delete each `RunnerSet`'s message session before the pod exits, so the next AGC can open its own without waiting for GitHub to expire the old one.
- **Use a generous `terminationGracePeriodSeconds`** (≥ 30s).
  Each session delete is bounded at 10s even when GitHub is unreachable, but give it headroom.
- **Accept the blackout as a known cost** rather than attempting zero-downtime tricks.
  Running workers keep running, jobs wait in GitHub's queue, and the new session receives any unacknowledged assignment again.

---

← [Back to Operations](README.md)
