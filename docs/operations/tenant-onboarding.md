# Tenant Onboarding Checklist

Audience: platform engineer onboarding a new tenant team.

This checklist walks from pre-conditions through first successful job. For the full setup reference, see [Getting Started](../getting-started.md). For day-2 operations after onboarding, see the [Runbook](runbook.md).

---

## Pre-Conditions

Before beginning, confirm all of the following:

- [ ] **Namespace exists.** The tenant's Kubernetes namespace has been created: `kubectl get namespace <tenant-namespace>`.
- [ ] **GMC is running.** The Gateway Manager Controller (GMC) is deployed and healthy: `kubectl get deploy -n gmc-system gmc-controller-manager`.
- [ ] **CRDs are installed.** `kubectl get crd actionsgateway.actions.gateway && kubectl get crd runnergroups.actions.gateway`.
- [ ] **GitHub App is registered.** The GitHub App is registered in the target GitHub organization with at least `Actions: Read` and `Administration: Read` permissions. The platform team has the `appId`, `installationId`, and private key `.pem` file.
- [ ] **GitHub App is installed.** The App is installed on the organization (or specific repos): Settings → Developer settings → GitHub Apps → `<app>` → Install App.
- [ ] **Quota is approved.** The tenant's resource requirements have been reviewed and a `namespaceQuota` has been agreed: CPU, memory, and pod count.
- [ ] **PriorityClass objects exist** (GPU tenants only). Any `priorityClassName` values referenced in `priorityTiers` are pre-created at the cluster level: `kubectl get priorityclass`.
- [ ] **Cluster service CIDR is known.** Needed if the tenant's `noProxyCIDRs` must be customized: `kubectl cluster-info dump | grep -m1 service-cluster-ip-range`.
- [ ] **Security profile decided.** Default `baseline` is correct for normal CI workloads (builds, tests). Confirm with the tenant whether they need `restricted` (compliance / high-isolation) or `privileged` (docker-in-docker, kernel-module workflows). Tenants with both needs deploy two `ActionsGateway` CRs in two namespaces. See [§5.3 — Security Profiles](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in).

---

## Step 1: Create the GitHub App Secret

Create this in the tenant's namespace. Use a stable, versioned name (e.g. `github-app-v1`) to enable clean credential rotation later.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: github-app-v1
  namespace: <tenant-namespace>
type: Opaque
stringData:
  appId: "<GitHub App ID>"
  installationId: "<Installation ID>"
  privateKey: |
    -----BEGIN RSA PRIVATE KEY-----
    <contents of the .pem file>
    -----END RSA PRIVATE KEY-----
```

```sh
kubectl apply -f secret.yaml
```

**Verify:**
```sh
kubectl get secret github-app-v1 -n <tenant-namespace>
kubectl get secret github-app-v1 -n <tenant-namespace> \
  -o jsonpath='{.data.privateKey}' | base64 -d | head -1
# Expected: -----BEGIN RSA PRIVATE KEY-----
```

---

## Step 2: Create the ActionsGateway Resource

Apply the `ActionsGateway` CR in the tenant's namespace. Adjust `namespaceQuota`, `proxy`, and `runnerGroups` for the tenant's workload.

```yaml
apiVersion: actions.gateway/v1alpha1
kind: ActionsGateway
metadata:
  name: <tenant>-gateway
  namespace: <tenant-namespace>
spec:
  gitHubAppRef:
    name: github-app-v1
  # Default: blocks privileged containers, host namespaces, hostPath, dangerous caps.
  # Set to "restricted" for stricter isolation, or "privileged" only if the workload
  # genuinely needs an unrestricted PodSpec (DinD, Buildah without sandbox, kernel modules).
  securityProfile: baseline
  proxy:
    minReplicas: 2
    maxReplicas: 10
  namespaceQuota:
    requests.cpu: "20"
    requests.memory: "40Gi"
    pods: "50"
  runnerGroups:
    - name: default
      runnerLabels: ["self-hosted", "linux"]
      maxListeners: 10
      maxWorkers: 20
      podTemplate:
        spec:
          containers:
            - name: runner
              resources:
                requests:
                  cpu: "1"
                  memory: "2Gi"
```

```sh
kubectl apply -f actionsgateway.yaml
```

---

## Step 3: Validate Provisioning

The GMC provisions all tenant resources within ~30 seconds of CR creation.

```sh
# Check the ActionsGateway conditions
kubectl get actionsgateway -n <tenant-namespace> <name> \
  -o jsonpath='{.status.conditions}' | jq .

# Expected conditions:
#   Ready=True
#   AGCAvailable=True
#   ProxyAvailable=True
```

```sh
# Confirm the AGC Deployment is running
kubectl get deploy -n <tenant-namespace> actions-gateway-agc
# Expected: READY 1/1

# Confirm the proxy pool is running
kubectl get deploy,hpa -n <tenant-namespace>
# Expected: proxy Deployment READY >= minReplicas, HPA TARGETS shows a percentage (not <unknown>)

# Confirm RunnerGroup CRs were created
kubectl get runnergroup -n <tenant-namespace>

# Confirm RBAC was created
kubectl get serviceaccount,role,rolebinding -n <tenant-namespace> | grep actions-gateway

# Confirm NetworkPolicies and ResourceQuota were applied
kubectl get networkpolicy,resourcequota -n <tenant-namespace>
# Expected NetworkPolicies (3):
#   actions-gateway-workload — restricts AGC and worker pods to proxy + DNS
#   actions-gateway-agc      — adds Kubernetes API server egress for the AGC only
#   actions-gateway-proxy    — restricts proxy pods to GitHub CIDRs + DNS

# Confirm the Pod Security Admission label matches the chosen securityProfile
kubectl get namespace <tenant-namespace> \
  -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}{"\n"}'
# Expected: baseline (default), or restricted / privileged if explicitly chosen
```

**If `TARGETS: <unknown>` on the HPA:** `resources.requests.cpu` is not set on proxy pods. Add it to `spec.proxy.resources.requests.cpu` in the `ActionsGateway` spec. See [Troubleshooting — Proxy Pool Not Scaling](troubleshooting.md#proxy-pool-not-scaling).

---

## Step 4: Validate Listener Sessions

The AGC should begin polling GitHub within seconds of starting.

```sh
# Check AGC logs for session registration
kubectl logs -n <tenant-namespace> deploy/actions-gateway-agc --tail=30
# Look for: "session registered" or "starting listener goroutine"

# Check the active sessions metric
# Metric: actions_gateway_active_sessions{namespace="<tenant-namespace>"}
# Expected: 1 per RunnerGroup (e.g. 1 if one RunnerGroup is defined)
```

If sessions are not appearing:
- Check for token errors: `kubectl logs ... | grep "token refresh"`.
- Check proxy connectivity: see [Troubleshooting — AGC CrashLoopBackOff](troubleshooting.md#agc-crashloopbackoff-or-not-acquiring-jobs).

---

## Step 5: Run a Test Job

Have the tenant run a workflow in their repository targeting the registered labels.

Example workflow:
```yaml
name: Runner connectivity test
on: workflow_dispatch
jobs:
  test:
    runs-on: [self-hosted, linux]
    steps:
      - run: echo "Runner is healthy. Host $(hostname)"
```

Trigger from the GitHub Actions UI or:
```sh
gh workflow run "Runner connectivity test" --repo <org>/<repo>
```

Watch for the job to be acquired and a worker pod to appear:
```sh
# Watch for worker pod creation
kubectl get pods -n <tenant-namespace> -w

# Check jobs acquired metric
# Metric: actions_gateway_jobs_acquired_total{namespace="<tenant-namespace>"}
# Expected: increments by 1

# Check pod creation latency
# Metric: actions_gateway_pod_creation_latency_seconds
# Expected: well under the 15s p95 SLO
```

---

## Success Criteria

Onboarding is complete when:

- [ ] `ActionsGateway` has `Ready=True` condition.
- [ ] HPA `TARGETS` shows a CPU percentage (not `<unknown>`).
- [ ] `actions_gateway_active_sessions` is ≥ 1 per RunnerGroup.
- [ ] At least one test job has completed successfully in the GitHub Actions UI.
- [ ] Worker pod was created and deleted after job completion.
- [ ] No errors in AGC logs during the test job.

---

## Common First-Day Mistakes

| Symptom | Cause | Fix |
|---------|-------|-----|
| `ActionsGateway` condition `AGCAvailable=False`, logs show `RSA key parse error` | Private key has trailing whitespace or incorrect PEM format | Recreate the Secret; ensure the key starts with `-----BEGIN RSA PRIVATE KEY-----` and has no extra blank lines or spaces |
| `HPA TARGETS: <unknown>` | `proxy.resources.requests.cpu` not set | Add `requests.cpu: "10m"` under `spec.proxy.resources.requests` |
| Worker pods stuck `Pending` | `ResourceQuota` exhausted or no schedulable nodes | Check `kubectl describe resourcequota -n <namespace>` and node capacity |
| `RunnerGroup` condition `VersionTooOld` | Worker image contains a runner version below GitHub's minimum | Update `workerImage` in the RunnerGroup spec |
| Test job stays queued in GitHub for >2 minutes | `active_sessions = 0` — listener goroutines are not running | Check AGC logs for credential or proxy errors |
| HPA present but proxy doesn't scale up | `maxReplicas` too low or HPA metric is `<unknown>` | Check both the HPA spec and that `requests.cpu` is set |
| Jobs acquired but pods not appearing | `priorityClassName` referenced in `priorityTiers` does not exist | `kubectl get priorityclass <name>` — create it if missing |

---

## Handing Off to the Tenant

Once onboarding is complete, share with the tenant team:

- The namespace name and the `ActionsGateway` CR name they own.
- The runner labels to use in their workflow `runs-on` fields.
- A link to [Getting Started](../getting-started.md) for self-service changes (RunnerGroup config, quota requests, credential rotation).
- A link to [Observability](observability.md) for the metrics they can watch.
- The on-call contact for platform-level issues (AGC crashes, GMC failures).

Tenants can manage their own RunnerGroup configuration, credential rotation, and `maxListeners` tuning without platform team involvement after this handoff.
