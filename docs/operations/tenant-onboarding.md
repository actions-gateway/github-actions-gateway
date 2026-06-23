# Tenant Onboarding Checklist

> **Audience:** Platform engineer

This checklist walks from pre-conditions through first successful job. For the full setup reference, see [Getting Started](../getting-started.md). For day-2 operations after onboarding, see the [Runbook](runbook.md).

---

## Pre-Conditions

Before beginning, confirm all of the following:

- [ ] **Namespace exists and is marked as a managed tenant.** The tenant's Kubernetes namespace has been created and carries the marker label `actions-gateway.github.com/tenant=true`:
  ```sh
  kubectl create namespace <tenant-namespace>   # if it does not exist yet
  kubectl label namespace <tenant-namespace> actions-gateway.github.com/tenant=true
  ```
  This label is what authorizes the GMC to operate in the namespace at all. Two admission policies key on it: `namespace-psa-guard` denies the GMC any namespace patch (the PSA-stamping step) it has *not* been marked for, and `gmc-tenant-resource-guard` denies the GMC any create/update/delete of tenant resources (Deployments, Secrets, RoleBindings, NetworkPolicies, …) outside marked namespaces. So an unlabelled namespace leaves the `ActionsGateway` stuck with a `NamespaceMarkerMissing` warning event and no provisioned resources. Apply the label with a trusted (administrator) identity — the GMC must never set it itself. Verify: `kubectl get namespace <tenant-namespace> -o jsonpath='{.metadata.labels.actions-gateway\.github\.com/tenant}'` → `true`.
- [ ] **Cluster CNI enforces egress NetworkPolicy.** The tenant isolation model (workers restricted to DNS + the per-tenant proxy; no direct GitHub or Kubernetes API egress) is implemented as NetworkPolicy egress rules, which are inert unless the cluster's Container Network Interface (CNI) plugin enforces them. Production clusters must run an egress-enforcing CNI such as Calico or Cilium — kind's default kindnet, for example, accepts NetworkPolicy objects without enforcing egress. Verify with your CNI's documentation, or run the negative probes in [network-architecture.md § How to Validate Network Isolation](../design/network-architecture.md#how-to-validate-network-isolation) after onboarding: the "blocked" probes must actually time out.
- [ ] **GMC is running.** The Gateway Manager Controller (GMC) is deployed and healthy: `kubectl get deploy -n gmc-system gmc-controller-manager`. Install it with the [`actions-gateway` Helm chart](../../charts/actions-gateway/README.md) (`helm install gag charts/actions-gateway -n gmc-system --create-namespace …`).
- [ ] **CRDs are installed.** `kubectl get crd actionsgateway.actions.gateway && kubectl get crd runnergroups.actions.gateway`.
- [ ] **GitHub App is registered.** The GitHub App is registered in the target GitHub organization with at least `Actions: Read` and `Administration: Read` permissions. The platform team has the `appId`, `installationId`, and private key `.pem` file.
- [ ] **GitHub App is installed.** The App is installed on the organization (or specific repos): Settings → Developer settings → GitHub Apps → `<app>` → Install App.
- [ ] **GitHub URL is known.** The org/enterprise/repo URL the runners register against — `https://github.com/<org>`, `https://github.com/<org>/<repo>`, or a GitHub Enterprise Server URL `https://ghes.example.com/<org>`. It goes in `spec.gitHubURL` (Step 2) and must match where the App is installed. It is a required field — there is no default.
- [ ] **Quota is provisioned (platform-owned).** The tenant's resource requirements have been reviewed and the platform has created a `ResourceQuota` (and any `LimitRange`) on the tenant namespace — CPU, memory, and pod count. This is the real, tenant-uncontrollable cap; the gateway operates within it but never creates or mutates it. See [Step 1b](#step-1b-set-the-platform-owned-resourcequota). (If you provision namespaces and quotas via a GitOps or tenant-operator stack — Capsule, HNC, vCluster, kiosk — the quota comes from there instead.)
- [ ] **PriorityClass objects exist and are allowlisted** (priority-tiered tenants only). Any `priorityClassName` a tenant references in `priorityTiers` must (1) be pre-created at the cluster level by the platform (`kubectl get priorityclass`) **and** (2) appear on the GMC `--allowed-priority-classes` flag. The GMC validating webhook rejects any `priorityClassName` not on the allowlist (an empty allowlist rejects *all* of them) — this stops a tenant naming a high-priority, preempting class and evicting other tenants' worker pods. Create allowlisted classes with `preemptionPolicy: Never` unless cross-tenant preemption is genuinely intended for that tier; see [security-operations.md § Priority classes](security-operations.md#priority-classes-the-allowed-priority-classes-allowlist).
- [ ] **Cluster service CIDR is known.** Needed if the tenant's `noProxyCIDRs` must be customized: `kubectl cluster-info dump | grep -m1 service-cluster-ip-range`.
- [ ] **Security profile decided.** Default `baseline` is correct for normal CI workloads (builds, tests). Confirm with the tenant whether they need `restricted` (compliance / high-isolation) or `privileged` (docker-in-docker, kernel-module workflows). Tenants with both needs deploy two `ActionsGateway` CRs in two namespaces. You can *harden* a profile later in place (`baseline → restricted`) freely, but *relaxing* it (a downgrade) is rejected by admission unless you set the `actions-gateway.github.com/allow-profile-downgrade: "true"` annotation — see [troubleshooting: securityProfile downgrade rejected](troubleshooting.md#securityprofile-downgrade-rejected-by-admission-webhook). See [§5.3 — Security Profiles](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in).

  > **v2 API (`actions-gateway.com`):** the security profile is **no longer a field on the `ActionsGateway` CR** — it is a property of the **namespace**, because Pod Security Admission is namespace-scoped (appendix-h §H.16 #7). Instead of `spec.securityProfile`, label the namespace:
  >
  > ```bash
  > kubectl label namespace <tenant-namespace> actions-gateway.com/security-profile=restricted   # baseline | restricted | privileged; absent ⇒ baseline
  > ```
  >
  > The GMC stamps the `pod-security.kubernetes.io/*` labels from it. The downgrade and privileged-eligibility rules are identical to v1 but enforced by the `gmc-namespace-security-profile-guard` ValidatingAdmissionPolicy on the **namespace** (downgrade needs `actions-gateway.com/allow-profile-downgrade: allowed` as a namespace annotation; `privileged` needs `actions-gateway.com/privileged-profile: allowed` as a namespace label). Co-located v2 gateways share the one namespace profile; tenants needing different postures use different namespaces.
- [ ] **Privileged eligibility granted (only if the tenant needs `privileged`).** `securityProfile: privileged` is a **platform** decision, not tenant-settable: the GMC validating webhook rejects it (at create *and* update) unless the namespace carries the eligibility label, applied by a trusted administrator:

  ```bash
  kubectl label namespace <tenant-namespace> actions-gateway.github.com/privileged-profile=allowed
  ```

  The granting value is the enum keyword `allowed`, not `true` — a boolean-looking label value is a YAML footgun. This is a separate gate from the `actions-gateway.github.com/tenant` marker (which authorizes GMC management at all): without `privileged-profile`, the tenant can still run `baseline`/`restricted` but not `privileged`. The gate is fail-closed — absent the label (or any value other than `allowed`), privileged is refused — so a tenant cannot self-grant the cluster's least-restrictive PSA posture by creating a CR. Apply this only for tenants approved for docker-in-docker / kernel-module workloads, ideally paired with a sandbox `runtimeClassName`. Verify: `kubectl get namespace <tenant-namespace> -o jsonpath='{.metadata.labels.actions-gateway\.github\.com/privileged-profile}'` → `allowed`. To revoke, remove the label (`…/privileged-profile-`). See [troubleshooting: privileged securityProfile rejected](troubleshooting.md#privileged-securityprofile-rejected-namespace-not-eligible) and [§5.3](../design/05-security.md#privileged-eligibility-is-a-platform-decision).

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

## Step 1b: Set the Platform-Owned ResourceQuota

The namespace `ResourceQuota` (and any `LimitRange`) is **platform-owned** — it is
not a field on the `ActionsGateway` CR. The platform admin creates and manages it
on the tenant namespace, and the gateway operates within it but never creates or
mutates it. This is deliberate: a tenant-authored quota would be no real cap (the
tenant could raise it in their own CR), and owning quotas would force broad,
cluster-wide write RBAC on the GMC. Apply it with a trusted (administrator)
identity:

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

```sh
kubectl apply -f resourcequota.yaml
```

If you already provision namespaces and quotas through a GitOps pipeline or a
tenant operator (Capsule, HNC, vCluster, kiosk), set the quota there instead — the
gateway will not fight it.

**Size the quota for both pools at full scale.** The quota must leave room for the proxy pool at `spec.proxy.maxReplicas` *and* worker pods up to `maxWorkers` (each × its per-pod requests/limits, plus pod count). When the remaining headroom can't cover scaling to those ceilings, the gateway flags it without blocking provisioning: the GMC raises `ProxyQuotaPressure`/`ProxyQuotaExceeded` on the `ActionsGateway` and the AGC raises `WorkerQuotaPressure`/`WorkerQuotaExceeded` on each `RunnerGroup` (each also exported as a gauge for alerting). See [troubleshooting: Proxy Pool Not Scaling](troubleshooting.md#proxy-pool-not-scaling) and [Jobs Failing Due to Namespace ResourceQuota Exhaustion](troubleshooting.md#jobs-failing-due-to-namespace-resourcequota-exhaustion).

---

## Step 2: Create the ActionsGateway Resource

Apply the `ActionsGateway` CR in the tenant's namespace. Adjust `proxy` and `runnerGroups` for the tenant's workload. The namespace quota is set separately on the namespace (Step 1b), not on this CR.

**One `ActionsGateway` per namespace.** The admission webhook rejects a second `ActionsGateway` in a namespace that already has one — every per-tenant resource has a fixed, namespace-scoped name, so two CRs would contend over them and flap the namespace's PSA labels. To run a second logical gateway (e.g. a `privileged` profile alongside a `baseline` one), give it its own namespace. See [troubleshooting: second ActionsGateway rejected](troubleshooting.md#second-actionsgateway-in-a-namespace-rejected-singleton-guard).

```yaml
apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: <tenant>-gateway
  namespace: <tenant-namespace>
spec:
  gitHubAppRef:
    name: github-app-v1
  # GitHub org/enterprise/repo URL the runners register against (required). Use an
  # org URL (https://github.com/my-org) for org-wide runners, a repo URL
  # (https://github.com/my-org/my-repo) to scope to one repo, or your GitHub
  # Enterprise Server URL (https://ghes.example.com/my-org). The App referenced by
  # gitHubAppRef must be installed on this same org/enterprise.
  gitHubURL: https://github.com/my-org
  # Default: blocks privileged containers, host namespaces, hostPath, dangerous caps.
  # Set to "restricted" for stricter isolation, or "privileged" only if the workload
  # genuinely needs an unrestricted PodSpec (DinD, Buildah without sandbox, kernel modules).
  # "privileged" requires the namespace to be eligible: a platform admin must label it
  # actions-gateway.github.com/privileged-profile=allowed (see Pre-Conditions), else the webhook
  # rejects the CR at create/update. It is deliberately not tenant-settable.
  # A privileged worker container (securityContext.privileged: true in a podTemplate)
  # is ONLY admitted under securityProfile: privileged — under baseline/restricted the
  # webhook rejects it. See troubleshooting: privileged worker container rejected.
  securityProfile: baseline
  # Log verbosity for this tenant's AGC and egress proxy: info (default) or debug.
  # Leave at info; flip to debug only for a bug repro (see "Per-tenant log level"
  # below). Changing it is a rolling restart of the AGC and proxy, not a hot reload.
  logLevel: info
  proxy:
    minReplicas: 2
    maxReplicas: 10
    # Optional: noProxyCIDRs excludes internal destinations from the egress proxy.
    # Entries may be CIDRs (10.0.0.0/8), bare IPs, or NO_PROXY domain suffixes
    # (svc.cluster.local, internal.example.com). Admission rejects any entry that
    # would route this tenant's GitHub traffic around the proxy — a hostname
    # matching the gitHubURL host or the public GitHub domains (github.com,
    # githubusercontent.com, ghcr.io) — since that breaks egress-IP attribution.
    # Never list GitHub here. Cluster-internal defaults are appended automatically.
    # noProxyCIDRs: ["10.0.0.0/8"]
  # The namespace ResourceQuota is platform-owned and set on the namespace in
  # Step 1b — it is not a field on this CR.
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

**Optional — worker-pod lifecycle.** Each `runnerGroups[]` entry accepts two cleanup knobs. `completedPodTTL` (default `5m`) is how long a finished worker pod (Succeeded/Failed) is kept before the AGC deletes it — the retention window is your chance to `kubectl logs`/`describe` a failed pod; `"0s"` deletes pods immediately on completion. `pendingPodDeadline` (default `10m`, minimum `1s`) is how long a worker pod may sit Pending (unpullable image, unschedulable constraints) before the AGC deletes it and frees the concurrency slot it was holding — raise it above your worst-case node-autoscaling time for GPU pools, e.g.:

```yaml
  runnerGroups:
    - runnerLabels: ["self-hosted", "gpu"]
      completedPodTTL: "30m"      # longer debugging window for failed jobs
      pendingPodDeadline: "30m"   # GPU node provisioning can exceed the 10m default
```

A reaped Pending pod emits a `WorkerPodStuckPending` Warning Event on the RunnerGroup and cancels the job (it never started); see [troubleshooting: worker pod reaped while Pending](troubleshooting.md#worker-pod-reaped-while-pending-workerpodstuckpending).

**Changing `runnerGroups` later.** Editing `spec.runnerGroups` on an existing `ActionsGateway` reconciles to the desired set: added entries create new `RunnerGroup` CRs, and **removing an entry deletes its `RunnerGroup`** (which stops its listeners and cascades to its worker pods). Reordering entries is safe — the GMC keys pruning on owner labels, not list position, so a reorder never deletes or recreates a group. Removing an entry is the way to retire a runner group; `maxListeners` has a minimum of `1`, so there is no in-place scale-to-zero.

**Optional — custom worker image.** The default `ghcr.io/actions/actions-runner` image works out of the box: on every profile except `privileged` the AGC stamps `runAsNonRoot: true` and gap-fills `runAsUser: 1001` (the runner image's UID) so kubelet can verify non-root. If you point `workerImage` at a **custom** image whose user is **not** UID 1001 — a different named user, or one that runs as root — set `securityContext.runAsUser` (or `runAsNonRoot: false` for a root-based image) on the runner container in the `podTemplate`; otherwise kubelet rejects the pod with `CreateContainerConfigError`. See [troubleshooting: worker pod fails to start after secure-by-default SecurityContext](troubleshooting.md#worker-pod-fails-to-start-after-secure-by-default-securitycontext).

**Optional — distributed tracing.** To send the AGC's OpenTelemetry traces to a collector, add a `spec.tracing` block. Setting `endpoint` is what turns tracing on; leave the block out to keep it off (the default). `sampler` is a fixed enum — an unrecognized value is rejected by admission (see [troubleshooting: tracing sampler rejected](troubleshooting.md#tracing-sampler-rejected-by-admission)).

```yaml
spec:
  tracing:
    endpoint: https://otel-collector.observability:4317
    sampler: parentbased_traceidratio   # optional
    samplerArg: "0.1"                    # optional — sample 10% of traces
    resourceAttributes:                  # optional
      deployment.environment: prod
    # insecure: true   # only for a plaintext in-cluster collector; TLS is the default
```

There is no field for OTLP auth headers: collector authentication is a network-layer concern (in-cluster collector, mutual TLS, or a service mesh), not a CR secret. See [observability — enabling tracing on GMC-managed AGCs](observability.md#enabling-tracing-on-gmc-managed-agcs).

<a id="per-tenant-log-level"></a>
**Optional — per-tenant log level.** `spec.logLevel` sets the log verbosity of this tenant's AGC and egress proxy: `info` (the default) or `debug`. The GMC threads it to both workloads as the `LOG_LEVEL` environment variable, so you can crank one gateway to `debug` for a bug repro without redeploying the GMC or touching any other tenant:

```sh
kubectl patch actionsgateway -n <tenant-namespace> <name> \
  --type merge -p '{"spec":{"logLevel":"debug"}}'
# ...reproduce the issue, read the debug logs, then revert:
kubectl patch actionsgateway -n <tenant-namespace> <name> \
  --type merge -p '{"spec":{"logLevel":"info"}}'
```

- **The default is `info`, never `debug`.** A CR that omits the field — or sets it back to `info` — runs at `info`. At thousands of concurrent sessions the per-session/per-job `debug` lines dominate log volume, so `debug` is a deliberate, temporary opt-in, not a steady state.
- **Changing it is a rolling restart, not a hot reload.** The new level takes effect once the AGC and proxy pods roll (the value is part of their pod templates). Expect the AGC's listener pool to drain and re-establish; in-flight jobs finish on the old pod within its termination grace period.
- `debug` surfaces the AGC's per-session → per-job → per-pod lifecycle lines (the listener/multiplexer/provisioner traces, each carrying `namespace`/`group`/`sessionId`/`podName` correlation fields) and the proxy's per-CONNECT detail. The grep anchors are in [observability — debug diagnostics](observability.md#debug-diagnostics-for-otherwise-silent-paths).
- Only `info` and `debug` are accepted; admission rejects any other value.

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
#   ProxyQuotaPressure=False  (True warns the proxy can't scale to maxReplicas within the ResourceQuota)
#   ProxyQuotaExceeded=False  (True means proxy replica creates are being rejected by the ResourceQuota)
```

```sh
# Confirm the AGC Deployment is running
kubectl get deploy -n <tenant-namespace> actions-gateway-controller
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
#   actions-gateway-controller      — adds Kubernetes API server egress for the AGC only
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
kubectl logs -n <tenant-namespace> deploy/actions-gateway-controller --tail=30
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
| Proxy stuck below `maxReplicas`; `FailedCreate ... exceeded quota` events | `proxy.maxReplicas` exceeds the namespace `ResourceQuota` | Check the `ProxyQuotaPressure` condition (`kubectl describe actionsgateway …`); raise the quota or lower `maxReplicas` |
| Jobs acquired but pods not appearing | `priorityClassName` referenced in `priorityTiers` does not exist | `kubectl get priorityclass <name>` — create it if missing |
| `ActionsGateway` apply rejected: `priorityClassName … is not in the platform allowlist` | The named `PriorityClass` is not on the GMC `--allowed-priority-classes` flag (the allowlist is empty by default) | Have the platform admin create the `PriorityClass` and add its name to `--allowed-priority-classes`; see [security-operations.md § Priority classes](security-operations.md#priority-classes-the-allowed-priority-classes-allowlist) |

---

## v2 API (alpha): multiple gateways per namespace

> **Audience:** Platform engineer adopting the **`v2alpha1`** (`actions-gateway.com`) API. This is an **alpha, early-adopter** API served *beside* `v1alpha1` — everything above (the `v1alpha1`, `actions-gateway.github.com` flow) stays fully supported. Install the opt-in `actions-gateway-crds-v2` chart first; see [Getting Started — Optional: the v2alpha1 API](../getting-started.md#optional-the-v2alpha1-api-alpha).

The biggest onboarding change in v2 is that a single namespace may hold **multiple `ActionsGateway`s**, lifting the v1 one-gateway-per-namespace rule ([Step 2](#step-2-create-the-actionsgateway-resource)). What that changes when onboarding a v2 tenant:

- **Per-gateway resource naming.** Every resource a gateway derives is prefixed with the gateway name — `<gateway>-agc` (AGC Deployment / ServiceAccount / RoleBinding / Service), `<gateway>-worker` (worker ServiceAccount), `<gateway>-workload` (workload NetworkPolicy), and so on — so two gateways in one namespace never contend over a fixed name. List one gateway's resources with `kubectl get all,networkpolicy,secret -n <tenant-namespace> -l actions-gateway.com/gateway=<gateway>`.
- **52-character name cap.** Any v2 CR (`ActionsGateway`, `RunnerSet`, `RunnerTemplate`, `ClusterRunnerTemplate`, `EgressProxy`) whose `metadata.name` exceeds **52 characters** is rejected at admission. The cap reserves room for the derived `<name>-<suffix>` so a label value / Service name stays under RFC 1123's 63-character ceiling (appendix-h §H.6). Pick short gateway names.
- **Kubernetes ≥ 1.31 required.** Each AGC reconciles only the `RunnerSet`s whose `spec.gatewayRef.name` targets it, via a server-side CRD field selector (KEP-4358) that is alpha-off on 1.30. On a 1.30 cluster a v2 AGC's `RunnerSet` informer fails to sync (`field label not supported`) and the pod never becomes ready. Confirm the cluster is ≥ 1.31 before onboarding any v2 gateway.
- **Co-located gateways share one namespace security profile.** In v2 the Pod Security level is a property of the **namespace**, not the gateway (see the v2 callout in [Pre-Conditions](#pre-conditions)) — so all gateways in a namespace run under the same `actions-gateway.com/security-profile` label. Tenants needing different postures (e.g. `baseline` vs `privileged`) still use separate namespaces, exactly as in v1.

For the full reference — the naming table, per-gateway garbage-collection behavior, the CRD chart prerequisite, and the failure modes — see [Troubleshooting — Multiple v2 gateways in one namespace](troubleshooting.md#multiple-v2-gateways-in-one-namespace-naming-scoping-prerequisites) and [Appendix H — v2 API decomposition](../design/appendix-h-v2-api-decomposition.md).

---

## Handing Off to the Tenant

Once onboarding is complete, share with the tenant team:

- The namespace name and the `ActionsGateway` CR name they own.
- The runner labels to use in their workflow `runs-on` fields.
- A link to [Getting Started](../getting-started.md) for self-service changes (RunnerGroup config, quota requests, credential rotation).
- A link to [Observability](observability.md) for the metrics they can watch.
- The on-call contact for platform-level issues (AGC crashes, GMC failures).

Tenants can manage their own RunnerGroup configuration, credential rotation, and `maxListeners` tuning without platform team involvement after this handoff.
