# Upgrade and Rollback Procedures

> **Audience:** Platform engineer

For upgrade strategy intent, see [§2.6 of the architecture doc](../design/02-architecture.md#26-upgrade-strategy). For **initial installation** of the GMC, see [install.md](install.md) — this document covers day-2 upgrade and rollback.

The three independently versioned components — GMC, AGC, and worker image — each upgrade on their own cadence with separate procedures below.

---

## Table of Contents

- [Pre-Upgrade Validation Checklist](#pre-upgrade-validation-checklist)
- [Migration Notes](#migration-notes)
  - [BREAKING: spec.namespaceQuota removed — the ResourceQuota is now platform-owned](#breaking-specnamespacequota-removed--the-resourcequota-is-now-platform-owned)
  - [Tenant namespaces now require the actions-gateway.github.com/tenant marker label](#tenant-namespaces-now-require-the-actions-gatewaygithubcomtenant-marker-label)
  - [Worker pods are now cleaned up automatically (one-time sweep recommended)](#worker-pods-are-now-cleaned-up-automatically-one-time-sweep-recommended)
  - [AGC Deployment renamed from actions-gateway-agc to actions-gateway-controller](#agc-deployment-renamed-from-actions-gateway-agc-to-actions-gateway-controller)
  - [GMC manager NetworkPolicy is now enabled by default](#gmc-manager-networkpolicy-is-now-enabled-by-default)
- [GMC Upgrade](#gmc-upgrade)
  - [GMC install and upgrade via Helm (recommended)](#gmc-install-and-upgrade-via-helm-recommended)
  - [Step 1: Upgrade the CRDs](#step-1-upgrade-the-crds)
  - [Step 2: Roll the GMC Deployment](#step-2-roll-the-gmc-deployment)
  - [Step 3: Post-Upgrade Validation](#step-3-post-upgrade-validation)
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
# Metric: rate(actions_gateway_reconcile_errors_total[5m]) == 0
```

Also check the release notes for the new version before upgrading, particularly:
- CRD schema changes (new required fields, removed fields, validation tightening).
- Behavior changes that require configuration updates before the new binary takes effect.

---

## Migration Notes

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

### Tenant namespaces now require the `actions-gateway.github.com/tenant` marker label

The GMC's cluster-wide `namespaces:patch` grant is now gated by the
`namespace-psa-guard` ValidatingAdmissionPolicy (shipped in
`cmd/gmc/config/admission-policy/namespace-psa-guard.yaml`, applied by `make deploy`).
It denies the GMC any namespace patch unless the namespace already carries
`actions-gateway.github.com/tenant: "true"`. **Existing tenant namespaces created
before this change do not have the label**, so after upgrade the GMC cannot stamp
their Pod Security Admission labels and each affected `ActionsGateway` will emit a
`NamespaceMarkerMissing` warning event.

Before (or immediately after) upgrading, label every existing tenant namespace:

```sh
# Label all namespaces that currently hold an ActionsGateway CR.
kubectl get actionsgateway -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' \
  | sort -u \
  | xargs -I{} kubectl label namespace {} actions-gateway.github.com/tenant=true --overwrite
```

For a phased rollout where you cannot label every namespace up front, temporarily set
the binding's `validationActions` to `[Audit]` (instead of `[Deny]`) so denials are
logged but not enforced, label the namespaces, then restore `[Deny]`.

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

The default install (`config/default`) now ships the GMC manager NetworkPolicy
enabled. This flips the controller-manager pod to default-deny ingress and
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
on your CNI; see [observability.md](observability.md). The cert-manager metrics
certificate and the Prometheus `ServiceMonitor` remain **opt-in** behind their
kustomize comment blocks.

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

The shipped install artifact is the **`actions-gateway` Helm chart** under [`charts/actions-gateway/`](../../charts/actions-gateway/README.md). It is the supported 1.0 install/upgrade vehicle; the `make install` / `make deploy` flow in the steps below is the dev/CI path that drives the kustomize bases directly.

```sh
# First install
helm install gag charts/actions-gateway \
  --namespace gmc-system --create-namespace \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy>

# Upgrade in place (carries CRD field changes — see below)
helm upgrade gag charts/actions-gateway --namespace gmc-system --reuse-values \
  --set gmc.image.digest=sha256:<new-gmc>

# Roll back to the previous release
helm rollback gag --namespace gmc-system
```

Four upgrade-time behaviors are specific to this chart:

- **`gmc.image.digest` is required at render time.** Both `helm install` and `helm upgrade` fail with `gmc.image must be pinned by digest …` when the release values carry no digest — e.g. a values file that omits it, or `--reset-values`. `--reuse-values` (as in the example above) carries the previously pinned digest forward; pass `--set gmc.image.digest=sha256:<new-gmc>` to move to the new release's image. See the [troubleshooting runbook](troubleshooting.md#helm-render-fails-gmcimage-must-be-pinned-by-digest). Dev/test only: `allowFloatingImageTags=true` opts out.
- **CRDs upgrade with the release.** The `ActionsGateway` and `RunnerGroup` CRDs ship as templates under `templates/crds/` with `helm.sh/resource-policy: keep`, **not** the chart-root `crds/` directory — Helm never upgrades resources in `crds/`. So a `helm upgrade` applies additive CRD field changes automatically, and `helm uninstall` preserves the CRDs (and every tenant's `ActionsGateway`/`RunnerGroup` object) rather than cascade-deleting them. You do not run a separate CRD apply step. The `RunnerGroup` CRD is sourced from the AGC authoritative copy.
- **The webhook cert path depends on `certManager.enabled`.** With the default `certManager.enabled=true`, cert-manager issues and rotates the serving cert; nothing to do on upgrade. With `certManager.enabled=false`, the chart generates a self-signed serving cert and wires the webhook `caBundle` itself. On an in-place `helm upgrade` the chart **reuses the existing `webhook-server-cert` Secret** (it looks the Secret up), so the cert does not rotate; it only regenerates if that Secret is missing (a fresh install, or after you delete it to force rotation). A `helm template` (no cluster) cannot look the Secret up and therefore renders a fresh cert each time — expected for offline rendering only.
- **The `namespace-psa-guard` binding denies by default.** If you are upgrading a cluster whose existing tenant namespaces are not yet labeled `actions-gateway.github.com/tenant=true`, label them **before** the upgrade (see the migration note above), or the GMC's namespace patches will be denied. To stage the rollout you can temporarily set the binding to `Audit` by editing `validationActions` on the `ValidatingAdmissionPolicyBinding`, then flip it back to `Deny` once the labels are in place.

The remaining steps describe the manual (kustomize/`make`) path used in dev/CI.

### Step 1: Upgrade the CRDs

If the release includes CRD changes, apply them before rolling the operator:

```sh
make install
# or: kubectl apply -f config/crd/bases/
```

CRD changes are additive (new optional fields) by default. If a release includes breaking CRD changes, refer to the release notes for a migration procedure.

### Step 2: Roll the GMC Deployment

```sh
make deploy IMG=<registry>/gmc:<new-tag>
# or: kubectl set image deploy/gmc-controller-manager \
#       manager=<registry>/gmc:<new-tag> \
#       -n gmc-system
```

Watch the rollout:

```sh
kubectl rollout status deploy/gmc-controller-manager -n gmc-system
```

The rolling update replaces one replica at a time. Leadership transfers before the old leader is deleted. The total rollout time is typically < 30 seconds.

### Step 3: Post-Upgrade Validation

```sh
# Confirm both replicas are on the new image
kubectl get pods -n gmc-system -o wide

# Confirm the GMC has re-elected a leader
kubectl get lease -n gmc-system

# Confirm no new reconcile errors appeared
# Metric: actions_gateway_reconcile_errors_total

# Spot-check one ActionsGateway CR
kubectl describe actionsgateway -n <namespace> <name>
```

### Rollback

```sh
kubectl rollout undo deploy/gmc-controller-manager -n gmc-system
kubectl rollout status deploy/gmc-controller-manager -n gmc-system
```

If the rollback targets a different CRD schema version, re-apply the previous CRDs before rolling back the operator binary.

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

The AGC's SIGTERM handler calls `DELETE /sessions` for all open sessions before exiting, causing GitHub to immediately re-queue unacquired jobs rather than waiting for session TTL. To rely on this:

- Ensure `terminationGracePeriodSeconds` on the AGC Deployment is ≥ 30 seconds (the default).
- Do not use `kubectl delete pod` directly — it sends SIGKILL without a grace period. Use `kubectl rollout restart` or `kubectl set image` instead.

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
# Metric: rate(actions_gateway_renewjob_errors_total{namespace="<namespace>"}[5m])
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
# Metrics: token_refresh_errors_total, renewjob_errors_total
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
# Metrics: token_refresh_errors_total, renewjob_errors_total, reconcile_errors_total

# Pod creation latency within SLO
# Metric: histogram_quantile(0.95, rate(actions_gateway_pod_creation_latency_seconds_bucket[5m]))
```

If a regression is detected within the first 15 minutes after an upgrade, roll back immediately rather than investigating in production. Investigate using a non-production environment.

---

## Zero-Downtime Configuration

The GMC and worker image upgrades are non-disruptive. The AGC upgrade is the only component with a brief drain window. To minimize its impact:

- **Time upgrades outside peak hours** to reduce the number of in-flight jobs at risk.
- **Rely on SIGTERM drain** — `kubectl rollout restart` (not `delete pod`) gives the AGC time to call `DELETE /sessions` before the pod exits, reducing the redelivery window from session TTL (minutes) to pod startup time (seconds).
- **Use a generous `terminationGracePeriodSeconds`** (≥ 30s). The AGC's SIGTERM handler is fast (a few hundred milliseconds for most tenants), but give it headroom for high-listener-count namespaces.
- **Accept the blackout as a known cost** rather than attempting zero-downtime tricks. GitHub's 2-minute redelivery window means most jobs survive an AGC restart transparently; the risk window is only jobs whose `renewjob` lock happens to expire during the restart (unlikely in practice for a < 5-second restart).

---

← [Back to Operations](.)
