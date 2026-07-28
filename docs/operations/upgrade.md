# Upgrade and Rollback Procedures

> **Audience:** Platform engineer

For upgrade strategy intent, see [§2.6 of the architecture doc](../design/02-architecture.md#26-upgrade-strategy). For **initial installation** of the GMC, see [install.md](install.md) — this document covers day-2 upgrade and rollback.

The three independently versioned components — GMC, AGC, and worker image — each upgrade on their own cadence with separate procedures below.

---

## Table of Contents

- [Pre-Upgrade Validation Checklist](#pre-upgrade-validation-checklist)
- [Migration Notes](#migration-notes)
  - [BREAKING (pre-GA): capacityGate.mode values replaced by On + a gateway-level cluster fact](#breaking-pre-ga-capacitygatemode-values-replaced-by-on--a-gateway-level-cluster-fact)
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
  - [GMC install and upgrade via Helm (recommended)](#gmc-install-and-upgrade-via-helm-recommended)
  - [Post-upgrade validation](#post-upgrade-validation)
  - [Rollback](#rollback)
- [AGC Upgrade](#agc-upgrade)
  - [Per-Tenant Upgrade Procedure](#per-tenant-upgrade-procedure)
  - [Rollback](#rollback-1)
- [Proxy Upgrade](#proxy-upgrade)
  - [Step 1: Pre-Upgrade Checks](#step-1-pre-upgrade-checks)
  - [Step 2: Update the Proxy Image](#step-2-update-the-proxy-image)
  - [Step 3: Watch the Rollout](#step-3-watch-the-rollout)
  - [Step 4: Post-Upgrade Validation](#step-4-post-upgrade-validation)
  - [Rollback](#rollback-2)
- [Worker Image Upgrade](#worker-image-upgrade)
  - [Upgrade Procedure](#upgrade-procedure)
  - [Canary Testing a New Worker Image](#canary-testing-a-new-worker-image)
  - [Minimum Version Requirement](#minimum-version-requirement)
  - [Rollback](#rollback-3)
- [Post-Upgrade Validation](#post-upgrade-validation)
- [Zero-Downtime Configuration](#zero-downtime-configuration)

## Pre-Upgrade Validation Checklist

Before upgrading any component, confirm the system is healthy:

```sh
# 1. No active incidents or RateLimited conditions
kubectl get actionsgateway --all-namespaces

# 2. All AGC pods healthy
kubectl get pods --all-namespaces -l app=actions-gateway-controller

# 3. All proxy pools healthy
kubectl get pods --all-namespaces -l app=actions-gateway-proxy

# 4. No CrashLoopBackOff pods
kubectl get pods --all-namespaces | grep -v Running | grep -v Completed | grep -v Terminating

# 5. No recent reconcile errors
# Metric: rate(controller_runtime_reconcile_errors_total[5m]) == 0
```

Also check the release notes for the new version before upgrading, particularly:
- CRD schema changes (new required fields, removed fields, validation tightening).
- Behavior changes that require configuration updates before the new binary takes effect.

---

## Migration Notes

### BREAKING (pre-GA): `capacityGate.mode` values replaced by `On` + a gateway-level cluster fact

**Who is affected:** only a `RunnerSet` that set
`spec.capacityGate.mode: SchedulerVerdict` or `AutoscalerVerdict`. Those values existed
for a single release-day window and the field defaults to `Off`, so most installs have
nothing to do.

The capacity gate's mode enum was carrying two independent things: whether a set should
refuse work it cannot run (a tenant's choice), and which signal may be trusted (a
consequence of whether the cluster can grow — a property of the cluster, identical for
every set in it). Asking each tenant to assert the second meant asking them to speak for
infrastructure they may not own, and it made the one harmful misconfiguration —
gating on the scheduler's verdict where an autoscaler was about to add a node —
reachable by the party least equipped to avoid it.

The two axes are now separate objects, and that combination is unrepresentable:

| Was, on the `RunnerSet` | Now, on the `RunnerSet` | Plus, on its `ActionsGateway` |
|---|---|---|
| `mode: SchedulerVerdict` | `mode: On` | `clusterCapacity.nodeAutoscaling: Absent` |
| `mode: AutoscalerVerdict` | `mode: On` | `clusterCapacity.nodeAutoscaling: Present` (the default — nothing to set) |
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
  -p '{"spec":{"capacityGate":{"mode":"On"}}}'
```

**Order matters on a fixed-size cluster.** Apply the gateway patch first. Between the
CRD upgrade and the gateway patch, a set that was gating on the scheduler's verdict
falls back to the autoscaler-declination signal, which on a cluster with no autoscaler
simply never gates — you get today's un-gated behavior, never over-gating.

**If you upgrade the CRDs before the AGC** (they ship as separate charts), a set still
carrying an old value reports `WorkerCapacityDeclined=False` with
`reason: GateModeUnsupported` and is not gated — the fail-open direction. See
[troubleshooting](troubleshooting.md#the-mode-is-reported-as-unsupported).

### Non-breaking: an over-long derived `RunnerGroup` name is bounded (and renamed)

The GMC derives a `RunnerGroup`'s name from its gateway and first runner label
(`<gateway>-<label>`), and the AGC stamps that name on every worker pod and agent Secret
as the `actions-gateway/runner-group` label value. Object names may be 253 characters but
**label values stop at 63**, so a derived name past 63 produced a `RunnerGroup` that
reconciled perfectly and then rejected every worker pod create. A 15-character gateway
name with a 40-character runner label was enough to reach it. The derivation is now
bounded to 63 characters, with a hash replacing whatever is cut (Q473).

`v1alpha1` only. `v2alpha1`/`v2beta1` cap CR names at 52 characters for exactly this
reason, so v2 tenants are unaffected.

**Almost every install sees no change at all**: a derived name that already fit is
returned byte for byte as before, so existing `RunnerGroup`s keep their names. Only a
gateway whose derived name exceeded 63 characters is affected — on the first reconcile
after the upgrade the GMC creates the correctly-named `RunnerGroup` and prunes the old
one. That tenant could not place a worker pod before the rename, so no working runner is
disturbed; it starts working.

To check whether any of your gateways is affected before upgrading:

```sh
kubectl get runnergroups -A -o json \
  | jq -r '.items[] | select(.metadata.name | length > 63) | "\(.metadata.namespace)/\(.metadata.name)"'
```

Anything of yours keyed on such a name — dashboards, alerts, scripts — should move to the
gateway's own name or a label selector.

### Non-breaking: a `RunnerSet`'s agent Secrets and runner names gain an `rs-` prefix

A v2 `RunnerSet`'s pre-registered agents now derive their identity from the kind as well
as the name, so a `RunnerSet` and a same-named v1 `RunnerGroup` can run side by side
through a migration without fighting over one pool (Q466):

| | Before | After |
|---|---|---|
| Agent Secret | `agentpool-<set>-<index>` | `agentpool-rs-<set>-<index>` |
| Secret label | `actions-gateway/runner-group=<set>` | `actions-gateway.com/runner-set=<set>` |
| GitHub runner name | `<set>-<index>` | `rs-<set>-<index>` |

The v1 `RunnerGroup` derivation is **unchanged** — a v1-only install sees nothing.

**The upgrade needs no action, and orphans nothing.** On its first reconcile after the
upgrade, each `RunnerSet` moves its existing agent Secrets to the new names, carrying
each agent's GitHub registration along, then deletes the old copies. Nothing
re-registers, so no runner record is stranded and no runner goes offline for the move.
The old runner *name* survives at GitHub until that agent is next recycled (which
happens after its next job), at which point the old record is deregistered and the
`rs-`-prefixed one replaces it — expect the names to change over gradually, not at once.

The one thing to check is **anything of yours that matches those names**: dashboards,
alerts, scripts, or GitHub runner-name filters keyed on `agentpool-<set>-` or on a
`RunnerSet`'s runner names. Prefer the label selector, which is stable:

```sh
kubectl -n <tenant> get secret -l actions-gateway.com/runner-set=<set>
```

Agent Secrets also now carry an `ownerReference` to their `RunnerGroup` or `RunnerSet`,
back-filled onto existing ones during the same reconcile, so deleting the owner reclaims
them even if its finalizer never runs.

### Non-breaking: `v2alpha1` is deprecated and the apiserver now warns

All five `actions-gateway.com` kinds carry `deprecated: true` on their `v2alpha1`
version, so `kubectl` prints a warning on any `v2alpha1` read or write:

```text
Warning: actions-gateway.com/v2alpha1 RunnerSet is deprecated; use actions-gateway.com/v2beta1. v2alpha1 is served until v2.0.0, which removes it.
```

**Nothing breaks, and the upgrade needs no action.** Deprecation marks intent and
removes nothing: `v2alpha1` stays fully served, the conversion webhook still
round-trips it against the `v2beta1` storage version, and existing objects keep
reconciling untouched. The warning is advisory client-side output; it does not fail an
`apply`, and controllers or CI that ignore warnings are unaffected.

**What to do about it.** Onboard new tenants on `v2beta1` (see
[tenant onboarding](tenant-onboarding.md)), and move existing `v2alpha1` objects to
`v2beta1` at your convenience by re-applying the same object with the `apiVersion`
changed, since the two versions convert both ways. `v2alpha1` remains the
[`gag-migrate`](migration-v1-to-v2.md) on-ramp for tenants coming off `v1alpha1`, so a
freshly migrated tenant emits these warnings until it is moved onto `v2beta1`. Note
that `v2beta1` is ScaleSet-only: a `RunnerSet` still on `acquisitionProtocol: Classic`
must adopt the runner-scale-set protocol as part of that move (see
["`RunnerSet` Rejected: `acquisitionProtocol`"](troubleshooting.md#runnerset-rejected-acquisitionprotocol-v2alpha1-early-adopter)).
`v2alpha1` is one of three things `v2.0.0` removes; the standing notice for all three,
with a pre-upgrade checklist, is the
[deprecation and removal notice](v1alpha1-deprecation.md).

### Non-breaking: `v2alpha1` CRDs ship in a separate, opt-in chart

The v2 (`actions-gateway.com`) API is introduced as a decomposed set of five CRDs —
`actionsgateways`, `egressproxies`, `runnersets`, `runnertemplates`, and the
cluster-scoped `clusterrunnertemplates`. **The main `actions-gateway` chart upgrade
is unchanged: it does not install these.** They ship in a separate, opt-in chart,
`actions-gateway-crds-v2`, because the `RunnerTemplate`/`ClusterRunnerTemplate` CRDs
each embed a full pod template (~1.1 MB apiece, served at `v2beta1` + `v2alpha1`) and
would otherwise push the main chart's Helm release Secret past its hard 1 MiB limit.

**No action is required for existing tenants.** Install the v2 CRDs only when you want
the v2 API available. They are **applied server-side, not `helm install`ed** — the chart
is over the 1 MiB release-Secret limit — and the same command upgrades them (re-apply
with the newer tag to carry CRD field changes). For the default `gmc-system` namespace,
apply the signed manifest attached to the release:

```bash
kubectl apply --server-side -f \
  https://github.com/actions-gateway/github-actions-gateway/releases/download/vX.Y.Z/actions-gateway-crds-v2.yaml
```

For a custom GMC namespace, render the chart instead. Either way, see
[install.md § the v2 API CRDs](install.md#optional-the-v2-api-crds) for the full
lifecycle (upgrade/rollback/uninstall), signature verification, and namespace/cert-manager
overrides:

```bash
helm template actions-gateway-crds-v2 oci://ghcr.io/actions-gateway/charts/actions-gateway-crds-v2 \
  --namespace <gmc-namespace> \
  | kubectl apply --server-side -f -
```

The v2 controllers now reconcile these kinds, so a v2 object set provisions a working
tenant. Both groups are served side by side, so you can stay on the `v1alpha1`
(`actions-gateway.github.com`) API until the **`v2.0.0`** release removes it, or
migrate a tenant to v2 now with the one-shot fan-out tool: see
[migration-v1-to-v2.md](migration-v1-to-v2.md) and the
[deprecation and removal notice](v1alpha1-deprecation.md), which lists everything
`v2.0.0` removes (`v1alpha1`, `v2alpha1`, and the Classic acquisition protocol) and
the pre-upgrade checklist for it. Note: v2's `ActionsGateway`
reuses the `ag` short name, so once both groups are installed `kubectl get ag` is
ambiguous — qualify it as `kubectl get actionsgateways.actions-gateway.github.com`
(or `.com`) to disambiguate.

### BREAKING: `spec.namespaceQuota` removed — the ResourceQuota is now platform-owned

**This is a breaking CRD change, made pre-1.0 while the API can still break.** The
`spec.namespaceQuota` field has been removed from the `ActionsGateway` CRD. The
namespace `ResourceQuota` (and any `LimitRange`) is now **platform-owned**: the
platform admin creates and manages it on the tenant namespace, and the gateway
operates within it but never creates or mutates it. The GMC's `resourcequotas`
write RBAC has been dropped (least privilege — Q122/Q130). The rationale: a
tenant-set quota is no real cap (the tenant could raise it in their own CR) and it
fought GitOps and tenant-operator stacks (Capsule, HNC, vCluster, kiosk) that
already own namespaces and quotas.

**What you must do before (or as part of) the upgrade:**

1. **Provision a platform-managed `ResourceQuota` in each tenant namespace** *before*
   the new GMC takes over — the gateway no longer creates one, so a namespace with
   no quota becomes uncapped. For each tenant that relied on `spec.namespaceQuota`,
   read the current values and create a standalone `ResourceQuota`:

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

2. **Adopt or replace the orphaned GAG-created quota.** A `ResourceQuota` the old
   GMC created carries an `ownerReference` to the `ActionsGateway` CR, so it would be
   garbage-collected if the CR were ever deleted. Either adopt it by stripping that
   ownerReference (so it survives independently), or delete it and recreate a
   platform-managed one as in step 1:

   ```sh
   # Adopt: drop the ownerReference so the quota is no longer GC-tied to the CR
   kubectl patch resourcequota actions-gateway -n <tenant-namespace> \
     --type=json -p='[{"op":"remove","path":"/metadata/ownerReferences"}]'
   ```

3. **Drop `namespaceQuota` from your `ActionsGateway` manifests / GitOps.** On upgrade
   the CRD's structural-schema pruning silently drops the now-unknown field from
   stored and re-applied CRs — applying a manifest that still sets `namespaceQuota`
   is **not rejected**, the field is just ignored. Remove it from source so intent
   stays clear.

### BREAKING: `priorityTiers` PriorityClasses now require a platform allowlist; per-tier `preemptionPolicy` removed

**Two breaking CRD/admission changes, made pre-1.0 (Q132).** Both concern
`spec.runnerGroups[].priorityTiers`:

1. **The GMC validating webhook now rejects any `priorityClassName` not on the
   platform allowlist.** The allowlist is the new GMC `--allowed-priority-classes`
   flag (comma-separated class names) and is **empty by default**, so after upgrade
   *every* `ActionsGateway` that sets `priorityTiers` will be rejected on its next
   apply until you configure the flag. The rationale: a tenant-chosen, cluster-scoped
   `PriorityClass` with the default `PreemptLowerPriority` policy could evict other
   tenants' running worker pods — a cross-tenant isolation break.

2. **The per-tier `preemptionPolicy` field has been removed** from the
   `PriorityTier` schema. It was never wired to worker pods (a no-op) and was a
   tenant-controlled preemption lever; preemption is now governed solely by the
   platform-created `PriorityClass` object. Structural-schema pruning silently drops
   the now-unknown field from stored/re-applied CRs (no apply rejection); remove it
   from source so intent stays clear.

Migration steps:

1. **Before rolling the GMC**, decide which `PriorityClass` names tenants may use,
   ensure those classes exist (create them with `preemptionPolicy: Never` unless a
   tier is genuinely meant to preempt cross-tenant — see
   [security-operations.md § Priority classes](security-operations.md#priority-classes-the-allowed-priority-classes-allowlist)),
   and set `--allowed-priority-classes` on the GMC Deployment (or the chart values).
2. **Audit existing CRs** for the classes they reference:
   `kubectl get actionsgateway -A -o jsonpath='{range .items[*]}{range .spec.runnerGroups[*]}{range .spec.priorityTiers[*]}{.priorityClassName}{"\n"}{end}{end}{end}' | sort -u`.
   Every name in that list must be on the allowlist or the next apply/reconcile of
   that CR is rejected.
3. **Drop `preemptionPolicy` from your `priorityTiers` manifests / GitOps.**

### Tenant namespaces now require the `actions-gateway.github.com/tenant` marker label

The GMC's cluster-wide write grants are now gated by two ValidatingAdmissionPolicies
(both shipped by the Helm chart, gated on `admissionPolicy.enabled`):
`namespace-psa-guard` gates `namespaces:patch`, and `gmc-tenant-resource-guard`
gates create/update/delete of all tenant provisioning resources (Deployments,
Services, ServiceAccounts, RoleBindings, Roles, NetworkPolicies, HPAs, PDBs,
RunnerGroups, and Secret create/update). Both deny the GMC unless the target
namespace already carries `actions-gateway.github.com/tenant: "true"`. **Existing
tenant namespaces created before this change do not have the label**, so after
upgrade the GMC cannot stamp their Pod Security Admission labels *or provision any
resources in them*, and each affected `ActionsGateway` will emit a
`NamespaceMarkerMissing` warning event.

Before (or immediately after) upgrading, label every existing tenant namespace:

```sh
# Label all namespaces that currently hold an ActionsGateway CR.
kubectl get actionsgateway -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' \
  | sort -u \
  | xargs -I{} kubectl label namespace {} actions-gateway.github.com/tenant=true --overwrite
```

For a phased rollout where you cannot label every namespace up front, temporarily set
**both** bindings' `validationActions` to `[Audit]` (instead of `[Deny]`) so denials are
logged but not enforced, label the namespaces, then restore `[Deny]` on each.

### Worker quota accounting now counts native sidecars, `RuntimeClass` overhead, and storage

The worker footprint behind `WorkerQuotaPressure`/`WorkerQuotaExceeded`, the
pre-claim quota gate, and the capacity a scale-set advertises to GitHub previously
summed CPU and memory over `podTemplate.spec.containers` only. It now matches what
Kubernetes charges, on both the compute and the storage keys:

- **Compute.** Native sidecars (`restartPolicy: Always` init containers) are summed
  in, and `RuntimeClass` pod overhead is added. Plain init containers still
  contribute only a `max()` floor, as before.
- **Storage.** `requests.ephemeral-storage` / `limits.ephemeral-storage` are summed
  like CPU and memory, and every **generic ephemeral volume**
  (`podTemplate.spec.volumes[].ephemeral`) is charged as the per-worker PVC it
  creates — against `persistentvolumeclaims`, `requests.storage`, and the
  `<class>.storageclass.storage.k8s.io/…` keys when the claim names a
  `storageClassName`.

**No behaviour change for a tenant with none of these.** A shape declaring no
ephemeral storage and no ephemeral volumes is charged nothing on the storage keys,
so a namespace already at its `persistentvolumeclaims` ceiling for unrelated reasons
does not newly hold such a set back. For a Docker-in-Docker (DinD) or Kata tenant the
reported footprint goes **up**:

| Shape | Per-pod increase |
|---|---|
| Privileged DinD | `1` CPU / `3Gi` requests; `4Gi` memory limit |
| Kata | `2` CPU / `6Gi` requests; `4` CPU / `8Gi` limits; plus `250m` / `160Mi` overhead; plus `1` PVC and `100Gi` of `requests.storage` |

So an affected tenant whose quota was sized against the old numbers can come up with
`WorkerQuotaPressure=True` (or `WorkerQuotaExceeded=True`) after upgrade with no
change to the quota or the workload. **Nothing got worse** — a compute shortfall was
already quota-rejecting those pods at creation and retrying, and a storage shortfall
was already stranding them `Pending` on a PVC that could not be created. The
condition now reports it up front instead of surfacing it as burnt lock time.
Re-derive the affected quotas with
[sizing the platform-owned `ResourceQuota`](resourcequota-sizing.md).

Find the tenants this can affect before upgrading — any runner template with a native
sidecar, a `runtimeClassName`, or a generic ephemeral volume:

```sh
kubectl get runnertemplate,clusterrunnertemplate -A -o json | jq -r '.items[] | select(any((.spec.podTemplate.spec.initContainers // [])[]; .restartPolicy == "Always") or (.spec.podTemplate.spec.runtimeClassName // "") != "" or any((.spec.podTemplate.spec.volumes // [])[]; has("ephemeral"))) | "\(.kind) \(.metadata.namespace // "-")/\(.metadata.name)"'
```

Reading `RuntimeClass` overhead needs a cluster-scoped `runtimeclasses` read, added
to the per-gateway `agc-clusterrunnertemplate-reader` `ClusterRole` in the same
release — so **upgrade the Helm chart, not just the AGC image**. The read is
fail-open: without the grant the overhead term is simply omitted and the conditions
behave as they did before. Verify after upgrading:

```sh
kubectl auth can-i get runtimeclasses --as=system:serviceaccount:<NAMESPACE>:<AGC_SERVICEACCOUNT>
```

### Worker pods are now cleaned up automatically (one-time sweep recommended)

AGC versions with Q95 delete completed worker pods after `completedPodTTL`
(default 5m) and stuck-Pending worker pods after `pendingPodDeadline` (default
10m), and stamp every new worker pod and job Secret with an OwnerReference to
its RunnerGroup. Two operator-visible consequences:

- **Behaviour change:** completed worker pods no longer linger indefinitely.
  If your debugging workflow relied on inspecting old pods, raise
  `completedPodTTL` on the affected `runnerGroups[]` entries (see
  [tenant-onboarding](tenant-onboarding.md#step-2-create-the-actionsgateway-resource)).
- **One-time sweep:** pods created by *pre-upgrade* AGC versions whose
  RunnerGroup still exists are reaped automatically after upgrade, but pods
  whose RunnerGroup was already deleted have no OwnerReference and no
  reconciler to reap them. Clean those up once per tenant namespace:

```sh
# Terminal worker pods left behind by pre-Q95 AGCs (label is stamped on all worker pods)
kubectl delete pods -n <tenant-namespace> \
  -l app.kubernetes.io/name=actions-runner \
  --field-selector 'status.phase!=Running,status.phase!=Pending'
```

### AGC Deployment renamed from `actions-gateway-agc` to `actions-gateway-controller`

Deployments and resources created by the GMC are now named `actions-gateway-controller`
instead of `actions-gateway-agc`. After upgrading the GMC:

1. The GMC creates a new `actions-gateway-controller` Deployment in each tenant namespace.
2. The old `actions-gateway-agc` Deployment is left **orphaned** (still running but no longer
   managed). Remove it manually per tenant:

   ```sh
   kubectl delete deploy actions-gateway-agc -n <namespace>
   ```

3. Pods labelled `app=actions-gateway-agc` become `app=actions-gateway-controller`. Update
   any Prometheus alerts, Grafana dashboards, or PodMonitor selectors that reference the old
   label before upgrading.

### GMC manager NetworkPolicy is now enabled by default

The default install ships the GMC manager NetworkPolicy enabled
(`networkPolicy.enabled=true`). This flips the controller-manager pod to default-deny ingress and
admits its `:8443` `/metrics` endpoint **only** from namespaces labelled
`metrics: enabled`. **If your Prometheus runs in an unlabelled namespace, GMC
manager scrapes will start failing after upgrade.** Label it before (or right
after) upgrading:

```sh
kubectl label namespace <prometheus-namespace> metrics=enabled --overwrite
```

The validating-webhook port (`9443`) is re-allowed from any source, so CR
admission is unaffected. This change also adds a `PodDisruptionBudget`
(`minAvailable: 1`) and `priorityClassName: system-cluster-critical` to the
manager — no operator action required. Runtime NetworkPolicy enforcement depends
on your CNI; see [observability.md](observability.md). The Prometheus
`ServiceMonitor` remains **opt-in** behind the `metrics.serviceMonitor.enabled`
chart value.

---

## GMC Upgrade

The GMC runs `replicas: 2` with leader election. Only one replica reconciles at any time; leadership transfers seamlessly during a rolling update. In-flight reconciliations are idempotent — the new leader re-derives state and converges without producing duplicate resources.

The active replica releases its leader lease on graceful shutdown (`--leader-elect-release-on-cancel`, on by default), so during a rollout the standby takes over within one retry period (~2s) instead of waiting out the full lease (~15s). This is why the Deployment's short `terminationGracePeriodSeconds: 10` introduces no reconcile gap. If you run on a slow or heavily loaded API server and see spurious leader-lease losses (the GMC restarting with "failed to renew lease"), widen the timing with the tunables below rather than disabling leader election:

| Flag | Default | Purpose |
|---|---|---|
| `--leader-elect-lease-duration` | `15s` | How long a candidate waits before force-acquiring a stale lease. |
| `--leader-elect-renew-deadline` | `10s` | How long the leader keeps retrying a renewal before stepping down. |
| `--leader-elect-retry-period` | `2s` | Interval between election attempts (and the failover floor with release-on-cancel). |
| `--leader-elect-release-on-cancel` | `true` | Release the lease on SIGTERM for fast failover. Leave on. |

The invariant `lease-duration > renew-deadline > retry-period × 1.2` is validated at startup; a misordered set makes the GMC exit immediately with a message naming the offending flags.

### GMC install and upgrade via Helm (recommended)

The shipped install artifact is the **`actions-gateway` Helm chart**, published and cosign-signed to the GHCR OCI registry (`oci://ghcr.io/actions-gateway/charts/actions-gateway`); the [`charts/actions-gateway/`](../../charts/actions-gateway/README.md) source path is the dev/CI copy of the same chart. The chart is the **sole** install/upgrade vehicle — there is no kustomize path. For dev/CI iteration `make deploy` wraps `helm install` of the local chart with floating image tags.

> **Current release — `v1.2.0`** (chart version = release tag without the leading `v`). Pin `--version 1.2.0`; copy the image digests from the [release notes](https://github.com/actions-gateway/github-actions-gateway/releases/tag/v1.2.0) and verify the chart/image signatures before installing (see [release.md § Verify the publish](release.md#3-verify-the-publish)).

```sh
# First install (from the published, signed OCI chart)
helm install gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version 1.2.0 \
  --namespace gmc-system --create-namespace \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy> \
  --set wrapper.image.digest=sha256:<wrapper>

# Upgrade in place to a newer published chart version (carries CRD field changes — see below)
helm upgrade gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version <new-chart-version> --namespace gmc-system --reuse-values \
  --set gmc.image.digest=sha256:<new-gmc>

# Roll back to the previous release
helm rollback gag --namespace gmc-system
```

Five upgrade-time behaviors are specific to this chart:

- **All four image digests are required at render time.** Both `helm install` and `helm upgrade` fail with `<image>.image must be pinned by digest …` (naming `gmc`/`agc`/`proxy`/`wrapper`) when the release values carry no digest for one of the four — e.g. a values file that omits it, or `--reset-values`. `--reuse-values` (as in the example above) carries the previously pinned digests forward; pass `--set <image>.image.digest=sha256:<new>` for each image you are moving to the new release's build. See the [troubleshooting runbook](troubleshooting.md#helm-render-fails-gmcimage-must-be-pinned-by-digest). Dev/test only: `allowFloatingImageTags=true` opts out.
- **CRDs upgrade with the release.** The `ActionsGateway` and `RunnerGroup` CRDs ship as templates under `templates/crds/` with `helm.sh/resource-policy: keep`, **not** the chart-root `crds/` directory — Helm never upgrades resources in `crds/`. So a `helm upgrade` applies additive CRD field changes automatically, and `helm uninstall` preserves the CRDs (and every tenant's `ActionsGateway`/`RunnerGroup` object) rather than cascade-deleting them. You do not run a separate CRD apply step. The `RunnerGroup` CRD is sourced from the AGC authoritative copy.
- **The webhook cert path depends on `certManager.enabled`.** With the default `certManager.enabled=true`, cert-manager issues and rotates the serving cert; nothing to do on upgrade. With `certManager.enabled=false`, the chart generates a self-signed serving cert and wires the webhook `caBundle` itself. On an in-place `helm upgrade` the chart **reuses the existing `webhook-server-cert` Secret** (it looks the Secret up), so the cert does not rotate; it only regenerates if that Secret is missing (a fresh install, or after you delete it to force rotation). A `helm template` (no cluster) cannot look the Secret up and therefore renders a fresh cert each time — expected for offline rendering only.
- **A reinstall can leave the `priorityclass-allowlist-guard` policy unable to resolve its parameters (Q444, open).** Every `runnergroups`/`runnersets`/`runnertemplates` write is then denied cluster-wide with `no params found for policy binding`, even though the parameter ConfigMap is present at the referenced name and namespace. The only known recovery is a kube-apiserver restart, which is not available on a managed control plane. The trigger is not yet established and there is no chart-side mitigation; if you cannot tolerate the risk on EKS/GKE/AKS, run with `admissionPolicy.enabled=false`. Symptoms, mitigation and recovery: [troubleshooting.md](troubleshooting.md#every-runnergroup--runnerset-write-denied-no-params-found-for-policy-binding).
- **The `namespace-psa-guard` and `gmc-tenant-resource-guard` bindings deny by default.** If you are upgrading a cluster whose existing tenant namespaces are not yet labeled `actions-gateway.github.com/tenant=true`, label them **before** the upgrade (see the migration note above), or the GMC's namespace patches *and all tenant-resource writes* will be denied. To stage the rollout you can temporarily set both bindings to `Audit` by editing `validationActions` on each `ValidatingAdmissionPolicyBinding`, then flip them back to `Deny` once the labels are in place.

`helm upgrade` rolls the GMC Deployment (and carries additive CRD field changes —
no separate CRD apply step). Watch the rollout:

```sh
kubectl rollout status deploy/gmc-controller-manager -n gmc-system
```

The rolling update replaces one replica at a time. Leadership transfers before the old leader is deleted. The total rollout time is typically < 30 seconds.

### Post-upgrade validation

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

### Rollback

Roll back to the previously deployed release with `helm rollback` (see the Helm
section above):

```sh
helm rollback gag --namespace gmc-system
kubectl rollout status deploy/gmc-controller-manager -n gmc-system
```

`helm rollback` restores the prior release's values and manifests. CRDs carry
`helm.sh/resource-policy: keep`, so they are not rolled back automatically; if the
rollback targets a different CRD schema version, consult the release notes for any
CRD migration.

---

## AGC Upgrade

The AGC runs `replicas: 1`. **Every AGC upgrade incurs a brief drain window** — the period between the old pod terminating and the new pod acquiring sessions. During this window:

- **In-flight long polls** are dropped. GitHub redelivers these jobs within ~2 minutes (GitHub's redelivery window).
- **Per-job RenewJob loops** are abandoned. Any job whose lock window (~10 minutes per renewal) expires before the new AGC starts will be cancelled by GitHub. These require manual re-run.
- **Queued but unacquired jobs** are redelivered after the session TTL expires (typically < 2 minutes).

**Scheduling guidance.** Schedule AGC upgrades during low-traffic periods (off-peak hours, weekends) when in-flight job count is minimal. If zero-downtime is required, accept that GitHub redelivery provides effective recovery for most jobs.

### Per-Tenant Upgrade Procedure

Upgrade each tenant's AGC one at a time. If tenants are independent, you may parallelize across namespaces.

**Step 1: Drain the AGC before upgrading (optional, reduces blackout window)**

The AGC's SIGTERM handler calls `DELETE /sessions` for all open sessions before exiting, causing GitHub to immediately re-queue unacquired jobs rather than waiting for session TTL. The shutdown **blocks until every listener goroutine has issued its session DELETE** — the process does not exit while sessions are still open. To rely on this:

- Ensure `terminationGracePeriodSeconds` on the AGC Deployment is ≥ 30 seconds (the GMC stamps the AGC Deployment with 60s by default). The session drain is bounded at 10 seconds and runs concurrently across RunnerGroups, so it does not scale with listener count.
- Do not use `kubectl delete pod` directly — it sends SIGKILL without a grace period. Use `kubectl rollout restart` or `kubectl set image` instead.

If a session cannot be deleted (the broker is unreachable during the drain), the AGC retries within its budget and then logs a Warn naming the session before exiting:

```
DeleteSession failed; the broker session is leaked until it expires server-side sessionId=... attempts=3 error=...
```

That session's runner stays online on GitHub until the server-side session TTL expires. It is harmless but wastes a runner slot for the TTL; a burst of these lines during a rollout points at broker or egress-path reachability, not at the AGC.

**Step 2: Update the AGC image**

The GMC manages the AGC Deployment. To update the AGC image, update the GMC's configuration (Helm values or Kustomize overlay) with the new AGC image tag and re-deploy the GMC, which will then roll each tenant's AGC Deployment. Alternatively, patch per-namespace:

```sh
kubectl set image deploy/actions-gateway-controller \
  agc=<registry>/agc:<new-tag> \
  -n <namespace>
```

**Step 3: Watch the rollout**

```sh
kubectl rollout status deploy/actions-gateway-controller -n <namespace>
```

**Step 4: Confirm session recovery**

```sh
# sessions should return to >= 1 per RunnerGroup within a few seconds of pod startup
# Metric: actions_gateway_active_sessions{namespace="<namespace>"}

# No new renewjob errors
# Metric: rate(actions_gateway_renew_job_errors_total{namespace="<namespace>"}[5m])
```

**Step 5: Check for cancelled jobs**

After the rollout, verify that jobs active during the restart have either completed or been redelivered. Check the GitHub Actions UI for any unexpectedly cancelled runs.

### Rollback

```sh
kubectl rollout undo deploy/actions-gateway-controller -n <namespace>
kubectl rollout status deploy/actions-gateway-controller -n <namespace>
```

Then confirm sessions are re-established and job acquisition resumes.

---

## Proxy Upgrade

The proxy pool is HPA-managed and stateless. Rolling updates are non-disruptive as long as the `PodDisruptionBudget` (`minAvailable: 1`) is respected during the rollout.

Each terminating pod runs a graceful shutdown before exiting: it briefly holds its CONNECT listener open so endpoint removal can propagate (ending early once new connections stop arriving), then drains in-flight tunnels. Both stages share one 45s budget inside a 60s `terminationGracePeriodSeconds`, so a pod with quiet traffic typically terminates in a few seconds and a busy one takes longer. Pods that appear to hang for tens of seconds during a rollout are draining, not stuck — see [Proxy Tunnel Cut During a Rollout](troubleshooting.md#proxy-tunnel-cut-during-a-rollout) for the budget breakdown, and the spot/preemptible note beneath it if your nodes reclaim on a short notice.

### Step 1: Pre-Upgrade Checks

```sh
# Confirm the PodDisruptionBudget is in place
kubectl get pdb -n <namespace> actions-gateway-proxy

# Confirm current replica count
kubectl get deploy -n <namespace> actions-gateway-proxy

# Confirm the HPA is healthy (TARGETS should show a percentage, not <unknown>)
kubectl get hpa -n <namespace>
```

### Step 2: Update the Proxy Image

The GMC manages the proxy Deployment. Update the proxy image via the GMC's Helm values or Kustomize overlay, then re-deploy the GMC (which will reconcile the updated image into all tenant proxy Deployments). To patch a single namespace:

```sh
kubectl set image deploy/actions-gateway-proxy \
  proxy=<registry>/proxy:<new-tag> \
  -n <namespace>
```

### Step 3: Watch the Rollout

The rolling update replaces one proxy pod at a time. Kubernetes honours the `PodDisruptionBudget` and only terminates a pod once its replacement is `Ready`.

```sh
kubectl rollout status deploy/actions-gateway-proxy -n <namespace>
```

In-flight `CONNECT` tunnels through the old proxy pod will be interrupted when that pod is terminated. The AGC and worker pods will reconnect through the remaining proxy pods automatically. For high-concurrency tenants, schedule the upgrade during a low-traffic window to minimise connection resets.

### Step 4: Post-Upgrade Validation

```sh
# All proxy pods on the new image
kubectl get pods -n <namespace> -l app=actions-gateway-proxy -o wide

# HPA still computing utilization (not <unknown>)
kubectl get hpa -n <namespace>

# No spike in token or renewjob errors after the rollout
# Metrics: actions_gateway_token_refresh_errors_total, actions_gateway_renew_job_errors_total
```

### Rollback

```sh
kubectl rollout undo deploy/actions-gateway-proxy -n <namespace>
kubectl rollout status deploy/actions-gateway-proxy -n <namespace>
```

---

## Worker Image Upgrade

Worker image upgrades are non-disruptive: the new image takes effect on future jobs; running pods complete on the old image.

### Upgrade Procedure

Update `spec.runnerGroups[N].workerImage` in the `ActionsGateway` CR:

```sh
kubectl edit actionsgateway -n <namespace> <name>
# Update spec.runnerGroups[N].workerImage to the new image digest
```

The GMC propagates the change to the `RunnerGroup` CR. The AGC starts using the new image on the next job acquisition. No restart required.

**Production recommendation:** pin to a digest, not a tag:

```yaml
workerImage: ghcr.io/my-org/actions-runner-worker@sha256:abc123...
```

This ensures the exact same image is used for all jobs until explicitly changed, and enables unambiguous rollback.

### Canary Testing a New Worker Image

To test a new image on a subset of jobs before rolling it out broadly:

1. Add a second `RunnerGroup` with the new image and a distinct label (e.g. `canary`).
2. Update a subset of workflows to use `runs-on: [self-hosted, canary]`.
3. Monitor job success rates. If healthy, update the main `RunnerGroup` and remove the canary group.

### Minimum Version Requirement

GitHub enforces a minimum runner version at session creation time. If the worker image contains a runner below this threshold, the session goroutine will receive a `400 Bad Request` and surface a `VersionTooOld` condition on the `RunnerGroup`. Monitor `actions_gateway_active_sessions` and RunnerGroup conditions for this symptom after deploying an older image.

### Rollback

Set `workerImage` back to the previous digest:

```sh
kubectl patch actionsgateway -n <namespace> <name> \
  --type=json \
  -p='[{"op":"replace","path":"/spec/runnerGroups/0/workerImage","value":"<previous-digest>"}]'
```

---

## Post-Upgrade Validation

After any component upgrade:

```sh
# All ActionsGateway CRs healthy
kubectl get actionsgateway --all-namespaces

# Active sessions restored
# Metric: actions_gateway_active_sessions per namespace

# No spike in errors
# Metrics: actions_gateway_token_refresh_errors_total, actions_gateway_renew_job_errors_total, controller_runtime_reconcile_errors_total

# Pod creation latency within SLO
# Metric: histogram_quantile(0.95, rate(actions_gateway_pod_creation_latency_seconds_bucket[5m]))
```

If a regression is detected within the first 15 minutes after an upgrade, roll back immediately rather than investigating in production. Investigate using a non-production environment.

---

## Zero-Downtime Configuration

The GMC and worker image upgrades are non-disruptive. The AGC upgrade is the only component with a brief drain window. To minimize its impact:

- **Time upgrades outside peak hours** to reduce the number of in-flight jobs at risk.
- **Rely on SIGTERM drain** — `kubectl rollout restart` (not `delete pod`) gives the AGC time to call `DELETE /sessions` before the pod exits, reducing the redelivery window from session TTL (minutes) to pod startup time (seconds).
- **Use a generous `terminationGracePeriodSeconds`** (≥ 30s). The AGC's SIGTERM handler is fast (a few hundred milliseconds for most tenants) and its session drain is bounded at 10s even when the broker is unreachable, but give it headroom.
- **Accept the blackout as a known cost** rather than attempting zero-downtime tricks. GitHub's 2-minute redelivery window means most jobs survive an AGC restart transparently; the risk window is only jobs whose `renewjob` lock happens to expire during the restart (unlikely in practice for a < 5-second restart).

---

← [Back to Operations](.)
