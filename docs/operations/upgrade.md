# Upgrade and Rollback Procedures

Audience: platform engineer. For upgrade strategy intent, see [§2.6 of the architecture doc](../design/02-architecture.md#26-upgrade-strategy).

The three independently versioned components — GMC, AGC, and worker image — each upgrade on their own cadence with separate procedures below.

---

## Pre-Upgrade Validation Checklist

Before upgrading any component, confirm the system is healthy:

```sh
# 1. No active incidents or RateLimited conditions
kubectl get actionsgateway --all-namespaces

# 2. All AGC pods healthy
kubectl get pods --all-namespaces -l app=actions-gateway-agc

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

## GMC Upgrade

The GMC runs `replicas: 2` with leader election. Only one replica reconciles at any time; leadership transfers seamlessly during a rolling update. In-flight reconciliations are idempotent — the new leader re-derives state and converges without producing duplicate resources.

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
kubectl set image deploy/actions-gateway-agc \
  agc=<registry>/agc:<new-tag> \
  -n <namespace>
```

**Step 3: Watch the rollout**

```sh
kubectl rollout status deploy/actions-gateway-agc -n <namespace>
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
kubectl rollout undo deploy/actions-gateway-agc -n <namespace>
kubectl rollout status deploy/actions-gateway-agc -n <namespace>
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
