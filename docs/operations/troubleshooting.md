# Troubleshooting Guide

> **Audience:** SRE, Platform engineer

Each section below covers a specific failure mode: symptoms, likely cause, diagnostic commands, and resolution steps.

---

## Table of Contents

- [How to Validate a Fresh Deployment](#how-to-validate-a-fresh-deployment)
- [Helm Render Fails: gmc.image Must Be Pinned by Digest](#helm-render-fails-gmcimage-must-be-pinned-by-digest)
- [GMC Pods Rejected: insufficient quota to match these scopes (PriorityClass)](#gmc-pods-rejected-insufficient-quota-to-match-these-scopes-priorityclass)
- [GMC Not Provisioning Tenant Resources](#gmc-not-provisioning-tenant-resources)
- [ActionsGateway Reports RunnerGroupsDegraded](#actionsgateway-reports-runnergroupsdegraded)
- [RunnerGroup Reports WorkersUnschedulable](#runnergroup-reports-workersunschedulable)
- [Worker / Proxy / AGC Pods Rejected by a Cluster Policy Engine](#worker--proxy--agc-pods-rejected-by-a-cluster-policy-engine)
- [ActionsGateway Reports EgressRulesStale](#actionsgateway-reports-egressrulesstale)
- [Tenant Namespace Missing the Managed-Tenant Marker Label](#tenant-namespace-missing-the-managed-tenant-marker-label)
- [ActionsGateway Stuck Deleting (Teardown Blocked on a Failing Delete)](#actionsgateway-stuck-deleting-teardown-blocked-on-a-failing-delete)
- [AGC CrashLoopBackOff or Not Acquiring Jobs](#agc-crashloopbackoff-or-not-acquiring-jobs)
- [RunnerGroup ActiveSessions Exceeds maxListeners](#runnergroup-activesessions-exceeds-maxlisteners)
- [RunnerGroup Stops Serving Jobs With Stale Ready=True](#runnergroup-stops-serving-jobs-with-stale-readytrue)
- [Orphaned RunnerGroup After Removing It From the Spec](#orphaned-runnergroup-after-removing-it-from-the-spec)
- [Proxy NetworkPolicy Has an Empty GitHub Allowlist](#proxy-networkpolicy-has-an-empty-github-allowlist)
- [Worker Pods Stuck Pending](#worker-pods-stuck-pending)
- [Worker Pod Reaped While Pending (WorkerPodStuckPending)](#worker-pod-reaped-while-pending-workerpodstuckpending)
- [Worker Pods Stuck Running After the Job Finished (Mesh Sidecar)](#worker-pods-stuck-running-after-the-job-finished-mesh-sidecar)
- [Job-Lifecycle Events on a RunnerGroup / RunnerSet](#job-lifecycle-events-on-a-runnergroup--runnerset)
- [Proxy Pool Not Scaling](#proxy-pool-not-scaling)
- [Proxy Tunnel Closed Mid-Stream — Idle or Lifetime Cap](#proxy-tunnel-closed-mid-stream--idle-or-lifetime-cap)
- [Metrics scrape returns a TLS / connection error](#metrics-scrape-returns-a-tls--connection-error)
- [RateLimited Condition on ActionsGateway](#ratelimited-condition-on-actionsgateway)
- [GitHub App Secret Misconfiguration](#github-app-secret-misconfiguration)
- [Token Refresh Errors Spiking](#token-refresh-errors-spiking)
- [RenewJob Failures Rising](#renewjob-failures-rising)
- [Sessions Stuck in 401/EOF GetMessage Loops (Tenant Throughput Decays to Zero)](#sessions-stuck-in-401eof-getmessage-loops-tenant-throughput-decays-to-zero)
- [Network Connectivity Failures](#network-connectivity-failures)
- [AGC Cannot Reach the Kubernetes API Server (NetworkPolicy + post-DNAT port mismatch)](#agc-cannot-reach-the-kubernetes-api-server-networkpolicy--post-dnat-port-mismatch)
- [Worker Pod Runner.Worker Fails TLS Handshake With UntrustedRoot](#worker-pod-runnerworker-fails-tls-handshake-with-untrustedroot)
- [Evicted Worker Pods Exhausting Retry Budget](#evicted-worker-pods-exhausting-retry-budget)
- [Jobs Failing Due to Namespace ResourceQuota Exhaustion](#jobs-failing-due-to-namespace-resourcequota-exhaustion)
- [Jobs Not Being Acquired Despite Queued Work (Capacity Gate Saturated)](#jobs-not-being-acquired-despite-queued-work-capacity-gate-saturated)
- [Worker Pod Fails to Start After Secure-by-Default SecurityContext](#worker-pod-fails-to-start-after-secure-by-default-securitycontext)
- [securityProfile Downgrade Rejected by Admission Webhook](#securityprofile-downgrade-rejected-by-admission-webhook)
- [Second ActionsGateway in a Namespace Rejected (Singleton Guard)](#second-actionsgateway-in-a-namespace-rejected-singleton-guard)
- [`proxy.noProxyCIDRs` Rejected: Entry Would Bypass the Proxy for GitHub](#proxynoproxycidrs-rejected-entry-would-bypass-the-proxy-for-github)
- [Privileged Worker Container Rejected by Admission](#privileged-worker-container-rejected-by-admission)
- [`RunnerTemplate` Rejected: Reserved Pod Field (`v2alpha1`)](#runnertemplate-rejected-reserved-pod-field-v2alpha1)
- [`RunnerSet` Stuck `Ready=False` With a `NotFound` Reason (`v2alpha1`)](#runnerset-stuck-readyfalse-with-a-notfound-reason-v2alpha1)
- [v2 `ActionsGateway` Stuck `Ready=False` (`CredentialUnavailable` / `ProxyNotFound`)](#v2-actionsgateway-stuck-readyfalse-credentialunavailable--proxynotfound)
- [Multiple v2 gateways in one namespace: naming, scoping, prerequisites](#multiple-v2-gateways-in-one-namespace-naming-scoping-prerequisites)
- [v2 Objects Not Reconciling After Installing the CRD Chart](#v2-objects-not-reconciling-after-installing-the-crd-chart)
- [Privileged securityProfile Rejected: Namespace Not Eligible](#privileged-securityprofile-rejected-namespace-not-eligible)
- [Tracing Sampler Rejected by Admission](#tracing-sampler-rejected-by-admission)
- [ActionsGateway Rejected: Missing or Malformed `gitHubURL`](#actionsgateway-rejected-missing-or-malformed-githuburl)
- [Worker-Pod Lifecycle Duration Rejected by Admission](#worker-pod-lifecycle-duration-rejected-by-admission)
- [Worker Pod Crashes With configuredSettings ArgumentNullException](#worker-pod-crashes-with-configuredsettings-argumentnullexception)
- [kubectl apply ActionsGateway Times Out On Webhook During GMC Rollout](#kubectl-apply-actionsgateway-times-out-on-webhook-during-gmc-rollout)
- [Worker HTTPS_PROXY Returns connection refused During Proxy Rollout](#worker-https_proxy-returns-connection-refused-during-proxy-rollout)
- [Prometheus Not Scraping Proxy or AGC Metrics](#prometheus-not-scraping-proxy-or-agc-metrics)
- [Proxy Replica Stuck Pending After Enabling HA Defaults](#proxy-replica-stuck-pending-after-enabling-ha-defaults)

## How to Validate a Fresh Deployment

Run these checks immediately after deploying a new tenant gateway or upgrading existing components.

```sh
# 1. Check ActionsGateway status
kubectl get actionsgateway -n <namespace> -o yaml | grep -A 20 status:

# 2. Confirm the AGC pod is running
kubectl get deploy -n <namespace> actions-gateway-controller
kubectl logs -n <namespace> deploy/actions-gateway-controller --tail=50

# 3. Confirm the proxy pool is healthy
kubectl get deploy -n <namespace> actions-gateway-proxy
kubectl get hpa -n <namespace>

# 4. Confirm RunnerGroup resources exist
kubectl get runnergroup -n <namespace>

# 5. Check for condition errors
kubectl get actionsgateway -n <namespace> -o jsonpath='{.status.conditions}' | jq .
```

Expected state after a healthy deployment:

- `ActionsGateway` condition `Ready=True`.
- `ActionsGateway` condition `AGCAvailable=True`.
- `ActionsGateway` condition `ProxyAvailable=True`.
- AGC Deployment: `READY 1/1`.
- Proxy Deployment: `READY` count >= `minReplicas`.
- HPA: `TARGETS` showing a CPU percentage (not `<unknown>`).
- Each RunnerGroup has at least one listener session (`actions_gateway_active_sessions > 0`).

---

## Helm Render Fails: `gmc.image` Must Be Pinned by Digest

**Symptoms.** `helm install`, `helm upgrade`, `helm lint`, or `helm template` of the `actions-gateway` chart fails immediately with:

```
Error: execution error at (actions-gateway/templates/deployment.yaml:...):
gmc.image must be pinned by digest: set gmc.image.digest=sha256:<64 hex digits>
(see docs/operations/install.md, "Pin images by digest").
DEV/TEST ONLY: set allowFloatingImageTags=true to allow a floating tag.
```

**Cause.** `gmc.image.digest` is empty in the release values. The chart enforces digest pinning of the GMC's own controller image at render time (secure by default): nothing at runtime validates the image the GMC itself runs from, so an empty digest must never silently fall back to a mutable `:latest` tag. Common ways to get here:

- A fresh install without `--set gmc.image.digest=sha256:<gmc>`.
- A `helm upgrade` with a values file (or `--reset-values`) that omits the digest. (`--reuse-values` carries the previously pinned digest forward.)
- Offline rendering (`helm template` / `helm lint`) without supplying a digest.

**Resolution.**

- **Production:** pin the digest published for the release you are installing (see [release.md](release.md) for where digests are recorded):

  ```sh
  helm upgrade --install gag charts/actions-gateway \
    --namespace gmc-system \
    --set gmc.image.digest=sha256:<gmc> \
    --set agc.image.digest=sha256:<agc> \
    --set proxy.image.digest=sha256:<proxy>
  ```

- **Dev/test only:** `--set allowFloatingImageTags=true` allows a floating tag for the GMC image *and* disables the GMC's startup digest check on the AGC/proxy images. Never use it in production.
- **Offline rendering:** any well-formed digest satisfies the check, e.g. `--set-string gmc.image.digest=sha256:1111111111111111111111111111111111111111111111111111111111111111`.

Note the contrast with the AGC/proxy images: those are validated by the GMC **at startup** (a floating tag there crash-loops the GMC — see [install.md § Pin images by digest](install.md#pin-images-by-digest)), while the GMC's own image is validated by the chart **at render time**.

---

## GMC Pods Rejected: `insufficient quota to match these scopes` (PriorityClass)

**Symptoms.** After `helm install`, the GMC Deployment never reaches Ready. There are no GMC pods, and the ReplicaSet emits a `FailedCreate` event:

```
kubectl describe replicaset -n gmc-system -l app.kubernetes.io/name=gmc
# Events:
#   Warning  FailedCreate  ...  Error creating: pods "gmc-controller-manager-..." is forbidden:
#   insufficient quota to match these scopes:
#   [{PriorityClass In [system-node-critical system-cluster-critical]}]
```

**Cause.** The cluster's API server enables the restricted `PriorityClass` admission config (GKE Standard does this by default), which permits `system-node-critical` / `system-cluster-critical` pods **only** in a namespace carrying a `ResourceQuota` whose `scopeSelector` matches those classes. The GMC runs with `priorityClassName: system-cluster-critical` by default — a deliberate secure default that protects the control plane from eviction — so without a permitting quota in the install namespace, the apiserver rejects every GMC pod.

**Resolution.** The chart ships the permitting quota by default, so this should not occur on a current chart. If you hit it:

- **Confirm the quota is enabled.** The chart renders `<namePrefix>-critical-pods` (default `gmc-critical-pods`) when `systemCriticalPriorityQuota.enabled=true` (the default) and `priorityClassName` is a system-critical class:

  ```sh
  kubectl get resourcequota -n gmc-system gmc-critical-pods
  ```

  If it is missing, you likely installed with `--set systemCriticalPriorityQuota.enabled=false`. Re-run the install/upgrade without that override (it defaults to `true`). See [install.md § GKE and other restricted-PriorityClass clusters](install.md#gke-and-other-restricted-priorityclass-clusters).
- **Do not** work around the rejection by clearing `priorityClassName` — that removes the GMC's eviction protection (a security regression). Keep `system-cluster-critical` and let the quota permit it.
- **If you manage the quota out-of-band** (e.g. a cluster-wide policy), ensure it exists in the install namespace and its `scopeSelector` matches the system-critical classes before installing.

---

## GMC Not Provisioning Tenant Resources

**Symptoms.** An `ActionsGateway` CR was applied but nothing has been created in the tenant namespace: no AGC Deployment, no proxy Deployment, no RunnerGroup resources.

**Likely causes.**
- GMC pod is not running or not the leader.
- GMC lacks permission to write to the tenant namespace (RBAC misconfiguration during initial GMC install).
- The `ActionsGateway` CR failed admission validation (check for validation errors in `kubectl apply` output or `Events`).

**Diagnostics.**

```sh
# Check whether the GMC is running and has a leader
kubectl get lease -n gmc-system
kubectl get pods -n gmc-system

# Check GMC logs for reconcile errors
kubectl logs -n gmc-system deploy/gmc-controller-manager --tail=100 | grep -i error

# Check events on the ActionsGateway CR
kubectl describe actionsgateway -n <namespace> <name>

# Check the conditions — Degraded names the failing provisioning step
kubectl get actionsgateway -n <namespace> <name> -o jsonpath='{.status.conditions}' | jq .
```

**Reading the `Degraded` condition.** When a reconcile fails partway through
provisioning, the GMC sets `Degraded=True` (reason `ProvisioningFailed`) on the
`ActionsGateway` and names the failing step in the message — e.g. `provisioning
failed at step "proxy Deployment + Service": ...`. The reconcile returns
immediately on that error, so the other conditions (`ProxyAvailable`,
`AGCAvailable`) may be stale; `Degraded` is the authoritative signal of which step
is stuck. It clears (`Degraded=False`, reason `ReconcileSucceeded`) automatically
once a reconcile completes all steps. Read it directly:

```sh
kubectl get actionsgateway -n <namespace> <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="Degraded")]}{.status} {.reason}: {.message}{"\n"}{end}'
```

**Resolution.**
- If the GMC pod is not running, restore it from its Deployment.
- If RBAC is missing, re-run `helm upgrade --install` of the chart (RBAC ships with it).
- If the admission webhook is rejecting the CR, fix the CR spec and re-apply.
- If `Degraded=True`, fix the underlying problem named by the failing step (e.g. a
  conflicting hand-created resource, missing permission, or exhausted quota) — also
  cross-check the `controller_runtime_reconcile_errors_total` metric and the full
  error in the GMC logs. The GMC's reconciler retries with backoff and clears
  `Degraded` on the next successful reconcile.

---

## ActionsGateway Reports RunnerGroupsDegraded

**Symptoms.** `kubectl get actionsgateway` shows a `RunnerGroupsDegraded=True`
condition, or the `actions_gateway_runnergroups_degraded` gauge is `1`. The gateway
infrastructure itself (proxy, AGC) may still be `Ready=True` — this condition rolls
**child RunnerGroup** health up to the gateway so you don't have to inspect each
group individually.

**Cause.** One or more of the gateway's owned `RunnerGroup`s reports an *impairing*
condition — `CredentialUnavailable` (the AGC can't obtain an installation token),
`Degraded` (an unhealthy/unauthorized listener session), `RunnerVersionTooOld`
(GitHub rejects the configured runner version), or `WorkersUnschedulable` (worker
pods can't be scheduled). Advisory capacity/throughput conditions
(`WorkerQuotaPressure`/`WorkerQuotaExceeded`, `RateLimited`) are deliberately
**not** rolled up here — they have their own signals. `RunnerGroupsDegraded` does
**not** gate `Ready`: the gateway can keep serving healthy groups while one is
impaired.

**Diagnostics.**

```sh
# Read the rollup — its message names the impaired groups and their tripped conditions.
kubectl get actionsgateway -n <namespace> <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="RunnerGroupsDegraded")]}{.status} {.reason}: {.message}{"\n"}{end}'

# Drill into a named RunnerGroup's own conditions.
kubectl get runnergroup -n <namespace> <runner-group> -o jsonpath='{.status.conditions}' | jq .
```

**Resolution.** Resolve the underlying per-group condition, then the rollup clears
automatically on the next reconcile (the GMC watches the owned RunnerGroups):
- `CredentialUnavailable` → see [GitHub App Secret Misconfiguration](#github-app-secret-misconfiguration).
- `Degraded` / `RunnerVersionTooOld` → see [AGC CrashLoopBackOff or Not Acquiring Jobs](#agc-crashloopbackoff-or-not-acquiring-jobs).
- `WorkersUnschedulable` → see [RunnerGroup Reports WorkersUnschedulable](#runnergroup-reports-workersunschedulable).

---

## RunnerGroup Reports WorkersUnschedulable

**Symptoms.** `kubectl get runnergroup` shows a `WorkersUnschedulable=True`
condition, or the `actions_gateway_workers_unschedulable` gauge is `1`. Jobs are
acquired but never start; worker pods sit `Pending`. Each pod the reaper eventually
gives up on also emits a `WorkersUnschedulable` Warning event and a
`WorkerPodStuckPending` event on the RunnerGroup.

**Cause.** The Kubernetes scheduler cannot place the group's worker pods on any
node — `PodScheduled=False` with reason `Unschedulable`. Typical reasons: no node
has enough allocatable CPU/memory for the pod's requests, the pod's `nodeSelector`
/ affinity matches no node, or every candidate node carries a taint the pod does
not tolerate. The condition trips once a pod has been Pending+Unschedulable for
longer than the scheduling grace (half the group's `pendingPodDeadline`), giving an
early warning before the reaper deletes the pod at the full deadline.

This is **not** a quota problem: a `ResourceQuota` rejection blocks pod *admission*
so the pod is never created — that path is the separate `WorkerQuotaExceeded`
condition. The two never both fire for the same cause.

**Diagnostics.**

```sh
# Read the condition — its message names the stuck pods and the scheduler verdict.
kubectl get runnergroup -n <namespace> <runner-group> \
  -o jsonpath='{range .status.conditions[?(@.type=="WorkersUnschedulable")]}{.status} {.reason}: {.message}{"\n"}{end}'

# Inspect a stuck worker pod's scheduler events for the exact reason.
kubectl describe pod -n <namespace> <worker-pod>   # look for "FailedScheduling"
```

**Resolution.** Match the scheduler verdict to the fix:
- *Insufficient cpu/memory* → add nodes / scale the cluster autoscaler, or lower the
  worker pod's resource requests in the group's `podTemplate`.
- *node(s) didn't match nodeSelector / affinity* → correct the `podTemplate`'s
  `nodeSelector`/affinity, or label the intended nodes.
- *node(s) had untolerated taint* → add the matching toleration to the `podTemplate`,
  or remove the taint from the target nodes.

The condition clears automatically on the next reconcile once a worker pod
schedules successfully.

---

## Worker / Proxy / AGC Pods Rejected by a Cluster Policy Engine

**Symptoms.** Pods never appear at all (not even `Pending`): a `Deployment`
stays at zero ready replicas, or no worker pod is created for an acquired job.
The owning controller's events or logs show an admission denial naming
[Kyverno](https://kyverno.io) or [OPA Gatekeeper](https://open-policy-agent.github.io/gatekeeper/),
for example:

```
admission webhook "validate.kyverno.svc-fail" denied the request:
... validation error: ... rule require-drop-all failed
```

**Cause.** A cluster-wide admission policy rejects the GAG pod for violating a
rule it does not satisfy. The usual culprits: a policy requiring `drop: [ALL]`
capabilities or `allowPrivilegeEscalation: false` on *all* pods (the default
`baseline` worker profile sets neither — baseline CI relies on in-job `sudo`); a
`readOnlyRootFilesystem` requirement (no worker profile sets it — the runner
needs a writable root filesystem); a registry allowlist that omits GAG's
registries; or a "require resource limits" rule (AGC v1alpha1 pods carry none).

This is distinct from [`WorkersUnschedulable`](#runnergroup-reports-workersunschedulable)
(scheduler can't place a *created* pod) and from `WorkerQuotaExceeded`
(`ResourceQuota` blocks admission): here the policy engine blocks pod creation
*before* either applies.

**Diagnostics.**

```sh
# Worker path: the owning RunnerGroup surfaces the create error in its conditions.
kubectl get runnergroup -n <namespace> <runner-group> -o yaml | less   # status.conditions / events
# Proxy / AGC path: the GMC logs the failed apply.
kubectl logs -n <gmc-install-namespace> deploy/<gmc-manager> | grep -i "denied\|forbidden\|policy"
# Confirm which policy fired.
kubectl get cpol,polr -A           # Kyverno ClusterPolicies + PolicyReports
kubectl get constraints            # Gatekeeper
```

**Resolution.** Reconcile the cluster policy with GAG's real pod posture — see
the [admission-policies compatibility matrix](admission-policies.md), which
states per policy class whether GAG complies or what to allow. In short:
- Add GAG's registries to your allowlist (`ghcr.io/actions-gateway/*` for the
  control plane, `ghcr.io/actions/actions-runner` for the default worker).
- For `drop-ALL` / no-privilege-escalation requirements: have tenants set
  `securityProfile: restricted` (which satisfies them), or apply the scoped
  [exception samples](examples/policies/) so `baseline` workers pass.
- For `readOnlyRootFilesystem`: exempt worker pods (no profile can satisfy it).
- For "require limits" on AGC v1alpha1: migrate the tenant to a v2alpha1
  `ActionsGateway`, or exempt AGC pods.

---

## ActionsGateway Reports EgressRulesStale

**Symptoms.** `kubectl get actionsgateway` shows an `EgressRulesStale=True`
condition, or the `actions_gateway_egress_rules_stale` gauge is `1`. Optionally,
jobs intermittently fail to reach newly-rotated GitHub endpoints.

**Cause.** The GMC refreshes each managed proxy `NetworkPolicy`'s egress allowlist
from `api.github.com/meta` on a ~24h cycle. If that refresh loop stalls (GitHub
meta API unreachable, persistent fetch errors), the allowlist freezes. GitHub
periodically rotates its published IP ranges, so a frozen allowlist eventually
drops egress to the new ranges silently. The condition trips when the last
successful refresh is older than the staleness window (just over two refresh
cycles), so a single missed/slow refresh does not false-trip it. It is advisory and
does **not** gate `Ready` — existing egress keeps working until GitHub rotates.
It is only evaluated for gateways whose proxy `NetworkPolicy` is gateway-managed
(`spec.proxy.managedNetworkPolicy` unset or `true`).

**Diagnostics.**

```sh
# Read the condition — its message reports how long ago the last refresh succeeded.
kubectl get actionsgateway -n <namespace> <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="EgressRulesStale")]}{.status} {.reason}: {.message}{"\n"}{end}'

# Inspect the GMC log for the refresh loop's errors.
kubectl logs -n <gmc-namespace> deploy/<gmc> | grep -i "ip range"

# Confirm the GMC can reach the GitHub meta API from its pod.
kubectl exec -n <gmc-namespace> deploy/<gmc> -- wget -qO- https://api.github.com/meta >/dev/null && echo ok
```

**Resolution.** Restore the GMC's reachability to `api.github.com` (egress policy,
DNS, proxy). The `actions_gateway_ip_range_updates_total` counter resumes
incrementing on the next successful refresh, and the condition clears automatically
within the re-check cadence (a fraction of the staleness window). If GitHub's meta
API is down, no action is needed beyond waiting — the allowlist is still valid until
GitHub rotates.

---

## Tenant Namespace Missing the Managed-Tenant Marker Label

**Symptoms.** An `ActionsGateway` never becomes `Ready`. `kubectl describe` shows a
`Warning` event with reason `NamespaceMarkerMissing`, and the GMC log reports a
`Forbidden` error stamping Pod Security Admission labels, citing the
`namespace-psa-guard` admission policy. This is common immediately after upgrading a
cluster whose tenant namespaces predate the policy (see
[Upgrade — Migration Notes](upgrade.md#migration-notes)).

**Cause.** The GMC's cluster-wide `namespaces:patch` grant is gated by the
`namespace-psa-guard` ValidatingAdmissionPolicy, which denies the GMC any namespace
that is not labelled `actions-gateway.github.com/tenant: "true"`. The label confines
the grant to managed tenants so a compromised GMC cannot relabel `kube-system` PSA
(see [Security §5.1/§5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in)).
The GMC never sets this label itself — a trusted administrator must apply it. The
same marker also gates the `gmc-tenant-resource-guard` policy, which confines every
tenant-resource write (Deployments, Secrets, RoleBindings, …) to marked namespaces;
provisioning fails at the PSA-stamping step first, so `NamespaceMarkerMissing` is the
signal you will see, but applying the label clears both gates.

**Diagnostics.**

```sh
# Confirm the warning event
kubectl describe actionsgateway -n <namespace> <name> | grep -A2 NamespaceMarkerMissing

# Check whether the marker label is present
kubectl get namespace <namespace> \
  -o jsonpath='{.metadata.labels.actions-gateway\.github\.com/tenant}'   # want: true

# Confirm both policies and their bindings are installed
kubectl get validatingadmissionpolicy gmc-namespace-psa-guard gmc-tenant-resource-guard
kubectl get validatingadmissionpolicybinding gmc-namespace-psa-guard-binding gmc-tenant-resource-guard-binding
```

**Resolution.** Apply the marker label as an administrator, then the GMC reconciler
retries automatically:

```sh
kubectl label namespace <namespace> actions-gateway.github.com/tenant=true
```

If the GMC's ServiceAccount is installed under a non-default namespace or name, also
confirm the policy's `matchConditions` username
(`system:serviceaccount:gmc-system:gmc-controller-manager`) matches your install.

---

## ActionsGateway Stuck Deleting (Teardown Blocked on a Failing Delete)

**Symptoms.** You deleted an `ActionsGateway`, but the CR does not disappear:
`kubectl get actionsgateway -n <namespace>` still lists it with a non-empty
`metadata.deletionTimestamp`, and `kubectl describe` shows a repeating `Warning`
event with reason `TeardownIncomplete`. Some tenant resources (e.g. the AGC
Deployment, RoleBinding, or a ServiceAccount) are still present in the namespace.

**Cause.** Teardown is **fail-closed by design** (Q125): the GMC keeps the
`actions-gateway.github.com/gmc-cleanup` finalizer on the CR and requeues until it
can confirm *every* owned resource is deleted (or already gone). If a delete keeps
failing — most often an API-server error, or a `Forbidden` from an admission policy
or revoked RBAC — the finalizer is retained on purpose so a live, credentialed AGC
Deployment is never orphaned by a half-finished teardown. A NotFound is treated as
success, so an already-deleted resource never blocks convergence.

**Diagnostics.**

```sh
# Confirm the CR is mid-deletion and which resources remain
kubectl get actionsgateway -n <namespace> <name> -o jsonpath='{.metadata.deletionTimestamp}{"\n"}{.metadata.finalizers}{"\n"}'
kubectl describe actionsgateway -n <namespace> <name> | grep -A3 TeardownIncomplete

# The event message names the namespace and the underlying error; also check the GMC log
kubectl logs -n gmc-system deploy/gmc-controller-manager --tail=50 | grep -i "delete resource during teardown"
```

**Resolution.** Fix the underlying delete failure — restore API-server health, or
re-grant the GMC the delete verb / re-apply the `gmc-tenant-resource-guard` marker if
the namespace lost its `actions-gateway.github.com/tenant=true` label (the policy gates
DELETE too, so an unmarked namespace blocks teardown). The reconciler retries on its
own backoff and removes the finalizer automatically once every delete is confirmed.
**Do not** manually strip the finalizer to force the CR away — that re-introduces the
orphaned-AGC failure mode the fail-closed behaviour exists to prevent; clear the real
delete error instead.

---

## AGC CrashLoopBackOff or Not Acquiring Jobs

**Symptoms.** The AGC pod is restarting repeatedly, or it is running but `actions_gateway_active_sessions` is zero and `actions_gateway_jobs_acquired_total` is not incrementing even when jobs are queued.

**Likely causes.**
- GitHub App Secret is missing, malformed, or contains an invalid private key.
- GitHub App `installationId` or `appId` is wrong.
- The proxy pool is not reachable from the AGC (network policy or proxy pod not ready).
- The AGC binary was built with an incompatible runner version (GitHub returns 400 on session creation).

**Diagnostics.**

```sh
# Check pod status and restarts
kubectl get pod -n <namespace> -l app=actions-gateway-controller

# Check logs for startup errors
kubectl logs -n <namespace> deploy/actions-gateway-controller

# Check that the referenced Secret exists and has the right keys
kubectl get secret -n <namespace> <gitHubAppRef.name>
kubectl get secret -n <namespace> <gitHubAppRef.name> -o jsonpath='{.data}' | jq 'keys'
# Expected keys: appId, installationId, privateKey

# Test proxy reachability — the AGC image is distroless (no shell, no curl),
# so spawn an ephemeral curl pod in the same namespace and use the same proxy URL.
kubectl run nettest-$$ -n <namespace> --rm -it --restart=Never \
  --image=curlimages/curl:latest \
  --overrides='{"spec":{"automountServiceAccountToken":false,"containers":[{"name":"c","image":"curlimages/curl:latest","command":["sh","-c","curl -x https://actions-gateway-proxy:8080 -sI https://api.github.com"]}]}}'

# Check RunnerGroup conditions
kubectl get runnergroup -n <namespace> -o yaml | grep -A 10 conditions

# Check RunnerGroup events — the AGC emits Warning events for the common failures.
kubectl describe runnergroup -n <namespace> <name>
# Look for:
#   TokenUnavailable          — GitHub App installation token could not be fetched (Secret/appId/installationId).
#   AgentPoolError            — agent Secret provisioning (EnsureAgents) failed.
#   ListenerStartFailed       — listener goroutines could not be (re)started.
#   AgentDeregistrationFailed — agent Secret cleanup on scale-down/delete failed.
#   RunnerVersionTooOld       — session creation rejected: the runner version is too old for GitHub (Q170).
#   SessionUnauthorized       — session creation rejected as unauthorized: agent credentials invalid/revoked (Q170).
#   JobAcquisitionFailed      — a delivered job could not be acquired from GitHub; it stays queued for redelivery (Q170).
#   NoActiveSessions / ListenerActive — Ready condition transitions.
```

**Resolution.**
- If the Secret is missing or has wrong keys, recreate it. See [Getting Started — GitHub App Secret](../getting-started.md#3-create-a-github-app-credential-secret).
- If the private key format is wrong, ensure it is a PEM-encoded key starting with `-----BEGIN RSA PRIVATE KEY-----` (PKCS#1) or `-----BEGIN PRIVATE KEY-----` (PKCS#8, RSA or Ed25519). The Secret `stringData.privateKey` must include the full key including header and footer lines.
- If the runner version is outdated, update `workerImage` in the RunnerGroup spec (or the AGC's `--worker-image` flag). Watch for `RunnerGroup` conditions with reason `VersionTooOld`.
- If `appId` or `installationId` are wrong, update the Secret.

---

## RunnerGroup ActiveSessions Exceeds maxListeners

**Symptoms.** `kubectl get runnergroup -n <namespace> -o jsonpath='{.items[*].status.activeSessions}'` reports a value greater than the group's `spec.maxListeners`, typically climbing by one after each broker or network outage. GitHub shows more concurrent runner sessions for the group than the configured ceiling.

**What happened.** On AGC versions without the Q100 fix, a recoverable crash of the permanent baseline listener left the active count at zero for the duration of the restart backoff; a reconcile firing inside that window started a second permanent baseline on top of the pending restart. Permanent listeners are restarted after every recoverable exit and are exempt from the `maxListeners` ceiling, so each repeat of the race ratchets the session count up by one, indefinitely. Fixed versions make the multiplexer start idempotent, so the race cannot stack baselines.

**Resolution.**
- Upgrade the AGC image to a version with the Q100 fix.
- To clear excess listeners immediately on an affected version, restart the AGC Deployment (`kubectl rollout restart deploy/actions-gateway-controller -n <namespace>`). Listener sessions are in-memory; the restarted AGC re-creates exactly one baseline per RunnerGroup.

---

## RunnerGroup Stops Serving Jobs With Stale Ready=True

**Symptoms.** A RunnerGroup stops servicing queued jobs even though the AGC pod is healthy, while `status.activeSessions` and the `Ready=True` condition still report the group as operational. `kubectl get runnergroup -n <namespace> -o jsonpath='{.status.activeSessions}'` shows a stale nonzero value that does not match the (zero) sessions GitHub sees for the group.

**What happened.** The permanent baseline listener exited *non-retriably* — e.g. GitHub returned `401 Unauthorized` on session creation for a credential it considers dead. The multiplexer does not auto-restart a non-retriable exit (that restart is reserved for recoverable crashes), so the in-memory listener count drops to zero. On AGC versions without the Q137 fix the RunnerGroup was only re-reconciled on a watch event (a RunnerGroup edit or a worker-pod lifecycle event) or the 10-hour informer resync, so with no such event the dead baseline — and the status written just before it died — could persist for hours.

**Resolution.**
- Upgrade the AGC image to a version with the Q137 fix. Fixed versions requeue the RunnerGroup on a bounded interval while the listener count is below the desired ceiling, so the reconciler re-runs its zero-listener recovery and revives the baseline within seconds; `status.activeSessions` and `Ready` then track reality again.
- To recover immediately on an affected version, trigger a reconcile by editing the RunnerGroup (e.g. a no-op annotation change) or restart the AGC Deployment (`kubectl rollout restart deploy/actions-gateway-controller -n <namespace>`); the restarted AGC re-creates one baseline per RunnerGroup from scratch.
- If the baseline keeps exiting non-retriably after revival, the underlying credential or runner-version problem is real — check `kubectl describe runnergroup` for `Degraded` / `Unauthorized` / `VersionTooOld` conditions and resolve per the [AGC CrashLoopBackOff or Not Acquiring Jobs](#agc-crashloopbackoff-or-not-acquiring-jobs) section.

---

## Listener Stalls for Minutes After a Black-Holed Broker Connection

**Symptoms.** One of a RunnerGroup's sessions stops picking up jobs for minutes at a stretch even though the AGC pod is healthy, the broker is reachable, and other sessions in the same group keep working. The stall typically follows a network event that silently drops an established connection — a firewall/NAT idle-timeout that discards packets without sending a RST, an egress-proxy failover, or a broker-side hang — so the long-poll's TCP connection is *black-holed*: accepted but never answered. `actions_gateway_message_poll_errors_total{reason="timeout"}` increments when an affected listener recovers.

**What happened.** The broker `GetMessage` long-poll holds the connection open for ~50s waiting for a job. On AGC versions without the Q108 fix the broker client had no response-header deadline, so a black-holed connection blocked the listener goroutine inside a single `GetMessage` call until the operating system's TCP timeout expired — minutes — during which that listener served no jobs. Fixed versions give the broker client a `ResponseHeaderTimeout` sized just above the 50s hold: a healthy long-poll is never cut short, but a black-holed connection is torn down a few seconds past the hold, classified as a benign "no message, retry", and the listener immediately opens a fresh long-poll.

**Resolution.**
- Upgrade the AGC image to a version with the Q108 fix. No configuration is required; the bound is built in.
- A steady stream of `actions_gateway_message_poll_errors_total{reason="timeout"}` after upgrade indicates the network is repeatedly black-holing broker connections (rather than wedging a listener). Investigate the egress path — proxy/NAT idle timeouts shorter than the 50s long-poll hold are the usual cause; raise the idle timeout above ~60s so healthy long-polls are not severed mid-hold.

---

## Reconcile or Token Mint Hangs on a Slow GitHub Endpoint

**Symptoms.** An AGC or GMC operation that calls a GitHub REST endpoint — installation-token mint, runner registration (`generate-jitconfig`), rerun-failed-jobs, or the GMC's `api.github.com/meta` IP-range fetch — appears to stall, and on a fixed version the logs now show a prompt `context deadline exceeded` / `Client.Timeout exceeded` error instead. These are short request/response calls, distinct from the broker long-poll above.

**What happened.** Before the Q138 fix these clients fell back to `http.DefaultClient`, which has no timeout: a peer that accepted the TCP connection but was slow — or never — to send response headers wedged the calling goroutine (a reconcile or a token fetch) until the multi-minute OS TCP timeout. Fixed versions build these clients with a bounded default (an overall request timeout plus a transport response-header timeout), so a slow GitHub endpoint fails fast and retriably rather than stalling the work. The broker long-poll is the one deliberate exception — it is bounded by the response-header deadline above, not an overall timeout.

**Resolution.**
- Upgrade to a version with the Q138 fix. No configuration is required; the bound is built in.
- Repeated timeout errors point at the egress path to `api.github.com` / `*.githubusercontent.com` (proxy, NAT, or DNS latency), not the gateway — investigate connectivity to those hosts.

---

## Orphaned RunnerGroup After Removing It From the Spec

**Symptoms.** A runner group was removed from (or reordered within) `spec.runnerGroups` on an `ActionsGateway`, but a `RunnerGroup` for it still exists and keeps running listeners and worker pods. `kubectl get runnergroup -n <namespace>` lists more groups than the CR now declares:

```sh
# Owner-labelled RunnerGroups for a gateway vs. what the spec now declares
kubectl get runnergroup -n <namespace> -l actions-gateway/owner-name=<gateway-name>
kubectl get actionsgateway <gateway-name> -n <namespace> -o jsonpath='{range .spec.runnerGroups[*]}{.runnerLabels[0]}{"\n"}{end}'
```

**What happened.** On GMC versions without the Q101 fix, reconciliation only created/patched the groups currently in the spec and never deleted the ones removed — and because groups were keyed by list index, a remove or reorder could orphan a `RunnerGroup` CR that kept serving jobs until the entire `ActionsGateway` was deleted.

**Resolution.**
- Upgrade the GMC to a version with the Q101 fix. Fixed versions reconcile `spec.runnerGroups` to the desired set: after applying the declared groups, the GMC prunes any owner-labelled `RunnerGroup` no longer in the spec, and keys pruning on owner labels (not list index) so a reorder never orphans a group. A subsequent reconcile (edit the CR, or wait for the next resync) cleans up any pre-existing orphans automatically.
- To remove a stranded group immediately on an affected version, delete its `RunnerGroup` directly: `kubectl delete runnergroup <name> -n <namespace>`. The AGC's RunnerGroup cleanup stops its listeners and cascades to its worker pods. Confirm you are deleting an orphan (its `runnerLabels` are not in the current `ActionsGateway` spec), not a live group.

---

## Proxy NetworkPolicy Has an Empty GitHub Allowlist

**Symptoms.** On a freshly provisioned tenant, all proxy egress to GitHub is silently dropped: `curl` through the proxy times out (no `502`), the AGC cannot acquire jobs, and token refresh fails. The proxy `NetworkPolicy` exists but its `ipBlock` egress peers are empty.

**Likely cause.** The IP Range Reconciler's initial `api.github.com/meta` fetch failed or stalled at GMC startup. The cached ranges seed each proxy `NetworkPolicy`'s `ipBlock` allowlist; until the first fetch lands, the allowlist is empty. The reconciler retries the initial fetch on a capped exponential backoff (1s→30s), so a transient outage normally self-heals within seconds — but a sustained inability to reach `api.github.com` from the GMC pod (egress firewall, DNS, or a long GitHub outage) leaves the allowlist empty until connectivity returns.

**Diagnostics.**

```sh
# Inspect the proxy NetworkPolicy's GitHub ipBlock egress peers — empty means the cache never populated.
kubectl get networkpolicy -n <namespace> actions-gateway-proxy \
  -o jsonpath='{.spec.egress[*].to[*].ipBlock.cidr}'

# Look for retry warnings in the GMC log.
kubectl logs -n gmc-system deploy/gmc-controller-manager \
  | grep -i "GitHub IP-range"
```

**Resolution.**
- Confirm the GMC pod itself can reach `api.github.com` (corporate egress firewall, DNS, or proxy in front of the cluster). The reconciler retries automatically; once connectivity is restored the next successful fetch patches every existing proxy `NetworkPolicy`.
- If the tenant manages its own egress policy (Cilium/Calico FQDN rules), set `spec.proxy.managedNetworkPolicy: false` so the reconciler leaves the policy alone.

---

## Worker Pods Stuck Pending

**Symptoms.** Jobs are acquired (`actions_gateway_jobs_acquired_total` increments) but worker pods remain in `Pending` state for more than 60 seconds. `pod_creation_latency_seconds` p95 exceeds the 15s SLO target.

**Likely causes.**
- Namespace `ResourceQuota` is exhausted — no pod slot, CPU request, or memory request available.
- No node has enough capacity for the pod's requested resources (GPU nodes may be at capacity).
- `PriorityClass` referenced in `priorityTiers` does not exist.
- Image pull is slow due to a large image on a cold node (expected; see SLO targets in [Appendix A](../design/appendix-a-capacity-slos.md)).

**Diagnostics.**

```sh
# Check quota usage
kubectl describe resourcequota -n <namespace>

# Describe a stuck pod to see the scheduling event
kubectl describe pod -n <namespace> <worker-pod-name>
# Look for: "Insufficient cpu", "Insufficient memory", "Insufficient nvidia.com/gpu",
#           "no nodes available to schedule pods", "didn't match PodDisruptionBudget"

# Check whether the PriorityClass exists
kubectl get priorityclass <priorityClassName>

# Check node capacity
kubectl describe nodes | grep -A 5 "Allocated resources"
```

**Resolution.**
- If quota is exhausted: raise the platform-owned `ResourceQuota` on the namespace (`kubectl edit resourcequota -n <namespace> <quota-name>`) or reduce `maxWorkers` / last-tier threshold.
- If no GPU nodes are available: check node autoscaler status or provision additional nodes.
- If a `PriorityClass` is missing: create it (cluster-admin action) or remove the tier reference.
- If image pull is slow (first job on a cold node): this is expected. If it exceeds the p99 SLO (60s), consider pre-pulling the image via a DaemonSet or enabling image streaming.

**Deadline.** A pod that stays Pending is not held forever: after `pendingPodDeadline` (default 10m, per-RunnerGroup) the AGC deletes it to free the concurrency-ceiling slot it was holding — see the next runbook section. Diagnose a stuck pod (`kubectl describe pod`) *before* the deadline reaps it, or raise `pendingPodDeadline` temporarily while debugging.

---

## Worker Pod Reaped While Pending (WorkerPodStuckPending)

**Symptoms.** A `Warning` Event with reason `WorkerPodStuckPending` appears on the RunnerGroup (`kubectl describe runnergroup -n <namespace>`), `actions_gateway_worker_pods_reaped_total{reason="pending_deadline"}` increments, and the job the pod was created for is cancelled by GitHub (it never started, so its lock lapsed). The worker pod itself is gone.

**What happened.** The pod stayed `Pending` longer than the RunnerGroup's `pendingPodDeadline` (default 10m), so the AGC deleted it. A permanently Pending pod would otherwise hold one of the group's concurrency-ceiling slots forever — the ceiling counts Pending pods. The deadline is a capacity-protection mechanism, not a retry mechanism: the job is **not** re-queued automatically.

**Likely causes.**
- `workerImage` (or the `podTemplate` container image) does not exist or is not pullable from the cluster — `ErrImagePull` / `ImagePullBackOff`.
- `podTemplate` scheduling constraints (nodeSelector, tolerations, GPU resources) that no node satisfies.
- Node autoscaler provisioning slower than the deadline (common for GPU node pools).

**Diagnostics.**

```sh
# The reap event names the deleted pod and the deadline that fired
kubectl get events -n <namespace> --field-selector reason=WorkerPodStuckPending

# Rate of reaps per group
# PromQL: rate(actions_gateway_worker_pods_reaped_total{reason="pending_deadline"}[1h])

# Reproduce the pull/scheduling failure before the next reap:
# trigger a job, then describe the new Pending pod within the deadline window
kubectl get pods -n <namespace> -l actions-gateway/runner-group=<group> -w
kubectl describe pod -n <namespace> <worker-pod-name>
```

**Resolution.**
- Fix the unpullable image or unsatisfiable scheduling constraint — that is the root cause; the reap is the messenger.
- If scheduling is legitimately slow (autoscaled GPU nodes), raise `spec.pendingPodDeadline` on the RunnerGroup (or the matching `runnerGroups[]` entry of the `ActionsGateway` CR) above the worst-case node-provisioning time, e.g. `pendingPodDeadline: "30m"`.
- Re-run the cancelled workflow from the GitHub UI once the cause is fixed.

---

## Worker Pods Stuck Running After the Job Finished (Mesh Sidecar)

**Symptoms.** Worker pods sit `Running` with a not-ready container count (`READY 1/2`) long after their job completed; `completedPodTTL` never deletes them; over time the RunnerGroup wedges at `maxWorkers` and new jobs stop being picked up even though no job is actually executing. `kubectl get pod -o jsonpath='{.spec.containers[*].name}'` shows a second container such as `istio-proxy` or `linkerd-proxy`.

**What happened.** A service-mesh sidecar was injected into the worker pod. GAG worker pods run to completion: the slot is freed and the pod reaped only when the pod reaches a *terminal* phase (`Succeeded`/`Failed`), which requires every container to exit. A classic mesh sidecar never exits on its own, so the pod stays `Running` forever and falls through both reaper paths (`completedPodTTL` covers terminal pods; `pendingPodDeadline` covers `Pending` pods — neither covers a stuck `Running` pod).

**Resolution.** Opt the GAG tenant namespace out of the mesh, or — if mesh membership is mandatory — switch to native sidecars (Kubernetes 1.28+) or a sidecar-less/ambient data plane. The full per-mesh configuration (Istio sidecar + ambient, Linkerd, Cilium, generic) is in [Running GAG Alongside a Service Mesh](service-mesh-coexistence.md). Note that mesh opt-out/exclusion annotations set on the RunnerGroup `podTemplate` are **not** honored — GAG strips arbitrary worker-pod-template metadata; configure the mesh at the namespace level instead.

---

## Job-Lifecycle Events on a RunnerGroup / RunnerSet

**What this is.** Beyond `WorkerPodStuckPending` (above), the AGC records `Warning`
Kubernetes Events on the owning `RunnerGroup` (`v1alpha1`) or `RunnerSet`
(`v2alpha1`) when a job-lifecycle transition fails terminally (Q170). They are the
event-based companion to the always-present metrics and status conditions — surfacing
the same incident in `kubectl describe`, `kubectl get events`, and any event watcher,
without a Prometheus query. Each `Reason` mirrors the corresponding metric name so you
can correlate the two.

Events are recorded **on a transition / terminal outcome**, not on every reconcile
or every requeue, so they do not spam. (The cluster's event recorder additionally
aggregates repeats of the same reason+message into one event with a count.) An event
is recorded on the owner's next reconcile, so it can trail the underlying metric by a
few seconds; the metric is the real-time signal.

| Reason | Type | Meaning | Where to look next |
|---|---|---|---|
| `JobAcquisitionFailed` | Warning | A delivered job could not be acquired from GitHub (`acquirejob` failed); the job stays queued at GitHub for redelivery. | [AGC CrashLoopBackOff or Not Acquiring Jobs](#agc-crashloopbackoff-or-not-acquiring-jobs) |
| `RunnerVersionTooOld` | Warning | Session creation was rejected permanently because the runner version is too old for GitHub. Also sets the `RunnerVersionTooOld` condition. | [AGC CrashLoopBackOff or Not Acquiring Jobs](#agc-crashloopbackoff-or-not-acquiring-jobs) |
| `SessionUnauthorized` | Warning | Session creation was rejected as unauthorized — the agent credentials are invalid or revoked. Also sets the `Degraded` condition. | [GitHub App Secret Misconfiguration](#github-app-secret-misconfiguration) |
| `QuotaRetriesExhausted` | Warning | Worker pod creation was abandoned after exhausting the namespace `ResourceQuota` retry budget (`maxQuotaRetries`). | [Jobs Failing Due to Namespace ResourceQuota Exhaustion](#jobs-failing-due-to-namespace-resourcequota-exhaustion) |
| `EvictionRetriesExhausted` | Warning | An evicted worker pod's auto-retry budget (`maxEvictionRetries`) is exhausted; a manual re-run is required. | [Evicted Worker Pods Exhausting Retry Budget](#evicted-worker-pods-exhausting-retry-budget) |

**Diagnostics.**

```sh
# All AGC-emitted Warning events on one owner, newest last.
kubectl describe runnergroup -n <namespace> <name>          # v1alpha1
kubectl describe runnerset   -n <namespace> <name>          # v2alpha1

# Filter the namespace event stream by a specific reason.
kubectl get events -n <namespace> --field-selector reason=EvictionRetriesExhausted
```

---

## Proxy Pool Not Scaling

**Symptoms.** The HPA for the proxy pool shows `TARGETS: <unknown>/60%` and the replica count does not increase under load.

**Likely cause.** `resources.requests.cpu` is unset or zero for proxy pods. The Kubernetes Horizontal Pod Autoscaler (HPA) computes CPU utilization as `(current_cpu_usage / requested_cpu)`. If `requests.cpu` is zero, the denominator is undefined and the HPA emits `<unknown>` for the target metric and stops scaling entirely.

**Diagnostics.**

```sh
# Check HPA status
kubectl describe hpa -n <namespace> actions-gateway-proxy

# Check proxy pod resource requests
kubectl get pod -n <namespace> -l app=actions-gateway-proxy -o jsonpath='{.items[0].spec.containers[0].resources}'

# Check metrics-server is running
kubectl get pods -n kube-system -l k8s-app=metrics-server
```

**Resolution.**

Ensure `spec.proxy.resources.requests.cpu` is set to a non-zero value in the `ActionsGateway` spec. The default is `10m`. If you explicitly set `resources` without including `requests.cpu`, the whole `resources` block is replaced and defaults are lost — set all four sub-fields explicitly:

```yaml
proxy:
  resources:
    requests:
      cpu: "10m"
      memory: "32Mi"
    limits:
      cpu: "100m"
      memory: "64Mi"
```

After updating the spec, patch the proxy Deployment or trigger a rollout; the HPA will start computing utilization on the next metrics scrape cycle (~30s).

**Second likely cause: the namespace `ResourceQuota` won't admit the replicas the HPA wants.** The HPA computes utilization correctly but the proxy Deployment cannot create more pods because the platform-owned namespace `ResourceQuota` is the hard cap. Under load the pool wedges below its target and the Deployment/ReplicaSet logs `FailedCreate ... exceeded quota` events instead of scaling out.

The GMC surfaces this as two non-blocking conditions on the `ActionsGateway` (neither gates `Ready` — the pool keeps serving at its current scale), each also exported as a gauge for alerting:

| Condition / metric | Meaning | Action |
|---|---|---|
| `ProxyQuotaPressure` (warning) — `actions_gateway_proxy_quota_pressure` | The pool can't grow to `maxReplicas` within the quota's remaining headroom (`hard − used`). Load-dependent. | Raise the quota or lower `maxReplicas` before the next spike. |
| `ProxyQuotaExceeded` (error) — `actions_gateway_proxy_quota_exceeded` | Replica creates are being **rejected now** (Deployment `ReplicaFailure` with `exceeded quota`). | Raise the quota now; the pool is degraded below the HPA's target. |

```sh
# Read both conditions (Exceeded supersedes Pressure when firing).
kubectl get actionsgateway -n <namespace> <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="ProxyQuotaPressure")]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
kubectl describe actionsgateway -n <namespace> <name>
```

Resolve by **either** raising the platform-owned quota (`kubectl edit resourcequota -n <namespace> <quota-name>`) to admit the configured `maxReplicas`, **or** lowering `spec.proxy.maxReplicas` to fit. Editing the quota's `.spec.hard` re-triggers reconciliation immediately; the conditions clear on the next reconcile.

---

## Proxy Tunnel Closed Mid-Stream — Idle or Lifetime Cap

**Symptoms.** A worker job logs a connection reset, `EOF`, or `broken pipe` from the GitHub SDK / `curl` / `git`, with no proxy `502` response. The proxy pod itself is healthy and serving other tunnels.

**Likely cause.** The proxy enforces two per-tunnel deadlines on the CONNECT relay (M-18, 2026-05-31):

- **Idle timeout** — 5 minutes of no data in either direction. A long-poll against the GitHub API or a stalled SDK call hits this first.
- **Hard lifetime cap** — 6 hours absolute, regardless of activity. A continuous artifact stream or Twirp log relay that exceeds this is torn down even with traffic flowing.

These are not bugs. They bound goroutine and file-descriptor exhaustion from slow or stuck clients. The healthy case (an actively-used GitHub API call) completes in seconds and does not trip either cap.

**Diagnostics.**

The proxy serves `/metrics` over mutual TLS on `:8443` (not `:8081`, which now
carries only the plaintext `/healthz` + `/readyz` probes). Scraping requires the
per-tenant scraper client certificate the GMC publishes — see
[Metrics scrape returns a TLS / connection error](#metrics-scrape-returns-a-tls--connection-error)
for how to fetch the bundle. With the bundle written to `scraper.crt` /
`scraper.key` / `metrics-ca.crt`:

```sh
ns=<namespace>
# Distribution of tunnel lifetimes; a heavy tail near 21600s (6h) or
# a spike at 300s (5m idle) indicates clients hitting the caps.
curl -s --cert scraper.crt --key scraper.key --cacert metrics-ca.crt \
  "https://actions-gateway-proxy.$ns.svc:8443/metrics" | \
  grep actions_gateway_proxy_tunnel_duration_seconds_bucket

# Active vs. total tunnels — healthy ratio is "active << total".
curl -s --cert scraper.crt --key scraper.key --cacert metrics-ca.crt \
  "https://actions-gateway-proxy.$ns.svc:8443/metrics" | \
  grep -E 'actions_gateway_proxy_connections_(active|total)'
```

**Resolution.**

For idle hits: examine the workflow step that stalls. A workflow `sleep`-ing inside a long-running `curl --connect-timeout 0` or a misconfigured webhook receiver are typical causes. The fix is usually in the workflow, not the proxy.

For lifetime-cap hits: split very long-running uploads or streams across multiple HTTP requests. The 6h cap is a safety net for stuck connections; a legitimately-long single stream should be rare.

To change the defaults during an incident, patch the proxy Deployment with environment overrides — note that there is no env-var knob today; defaults are baked into the Server struct and require a code change to adjust. File a Queue item if a tenant repeatedly hits either cap on a legitimate workload.

---

## Metrics scrape returns a TLS / connection error

**Symptoms.** Prometheus (or a manual `curl`) of a per-tenant proxy or AGC
`/metrics` endpoint fails with one of:

- `remote error: tls: certificate required` / `bad certificate` — no client cert, or one signed by the wrong CA.
- `connection refused` on `:8081/metrics` — the metrics endpoint moved to `:8443` (mTLS); `:8081` now serves only `/healthz` + `/readyz`.
- `context deadline exceeded` / no route — the scraper namespace is not labelled `metrics: enabled`, so the NetworkPolicy drops the connection before the handshake.

**Cause.** The proxy and AGC serve `/metrics` over **mutual TLS** on `:8443`
(Q69). A scraper must (1) connect from a namespace labelled `metrics: enabled`
and (2) present a client certificate signed by the per-tenant metrics CA the GMC
issues. Both halves are required.

**Resolution.**

1. Label the monitoring namespace so the NetworkPolicy admits it:
   ```sh
   kubectl label namespace <prometheus-namespace> metrics=enabled
   ```
2. Fetch the scraper client bundle the GMC publishes in each tenant namespace and
   point the scrape at `:8443` with `scheme: https`:
   ```sh
   ns=<tenant-namespace>
   kubectl get secret actions-gateway-metrics-client -n "$ns" -o jsonpath='{.data.tls\.crt}' | base64 -d > scraper.crt
   kubectl get secret actions-gateway-metrics-client -n "$ns" -o jsonpath='{.data.tls\.key}' | base64 -d > scraper.key
   kubectl get secret actions-gateway-metrics-client -n "$ns" -o jsonpath='{.data.ca\.crt}'  | base64 -d > metrics-ca.crt
   curl -s --cert scraper.crt --key scraper.key --cacert metrics-ca.crt \
     "https://actions-gateway-proxy.$ns.svc:8443/metrics" | head
   ```
   Delete the extracted key file when finished. For a `ServiceMonitor`, mount the
   bundle and reference it from `tlsConfig` (`cert`/`keySecret`/`ca`).
3. If the cert is rejected after a CA rotation, the GMC re-issues the whole
   bundle ~30 days before expiry but pods read certs at startup — restart the
   proxy/AGC pods (and re-fetch the client bundle) after a rotation.

---

## RateLimited Condition on ActionsGateway

**Symptoms.** `kubectl get actionsgateway` shows a `RateLimited=True` condition. `actions_gateway_active_sessions` is at or near the per-installation budget.

**Likely cause.** The GitHub App installation's API budget (15,000 `GET /message` requests/hour) is exhausted. This occurs when the sum of `maxListeners` across all RunnerGroups simultaneously bursts to their ceiling for a sustained period.

**SLO threshold.** A `RateLimited` condition lasting more than 1 minute during non-peak hours indicates the installation is over budget. Durations exceeding 10 minutes during business hours should page on-call.

**Diagnostics.**

```sh
# Check the condition
kubectl get actionsgateway -n <namespace> <name> -o jsonpath='{.status.conditions}' | jq .

# Check active sessions vs. budget
# Budget: ~208 sessions (15000/hr ÷ 72 polls/session/hr)
# Metric: actions_gateway_active_sessions{namespace="<namespace>"}

# Check per-RunnerGroup maxListeners sum
kubectl get runnergroup -n <namespace> -o jsonpath='{.items[*].spec.maxListeners}'
```

**Resolution.**
- If a burst is temporary and below 10 minutes: no action required, the condition will clear as the burst subsides.
- If `maxListeners` values are set higher than needed, reduce them.
- If the tenant's RunnerGroup count × `maxListeners` sustainably exceeds the installation budget, shard to a second `ActionsGateway` CR with a new GitHub App installation. See [Appendix E §E.6](../design/appendix-e-capacity-planning.md#e6-when-to-shard-across-installations).

---

## GitHub App Secret Misconfiguration

**Symptoms.** AGC logs show errors like `failed to create installation token`, `private key: RSA key parse error`, or `401 Unauthorized`. The `ActionsGateway` condition `AGCAvailable=False` with reason `CredentialError`. When the AGC cannot obtain an installation token while reconciling a RunnerGroup, that group also reports `CredentialUnavailable=True` (reason `TokenUnavailable`) in its status — surfacing the failure in `kubectl get runnergroup`/`describe`, not only as a `TokenUnavailable` Event. The condition clears (`CredentialUnavailable=False`, reason `CredentialAvailable`) on the next reconcile once a token is obtained. Read it with:

```sh
kubectl get runnergroup -n <namespace> <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="CredentialUnavailable")]}{.status} {.reason}: {.message}{"\n"}{end}'
```

**Common misconfigurations.**

| Error message | Likely cause |
| --- | --- |
| `private key: RSA key parse error` / `no PEM block found` | PEM key is corrupted — extra whitespace, missing or extra newlines, CRLF line endings, hand-paste damage, or an unsupported block type (e.g. `OPENSSH`/`EC`, which fail with `unsupported PEM block type`). Both PKCS#1 (`-----BEGIN RSA PRIVATE KEY-----`) and PKCS#8 (`-----BEGIN PRIVATE KEY-----`) are accepted, so PKCS#8 is **not** a wrong format. |
| `401 Unauthorized` on token exchange | `appId` or `installationId` is wrong. |
| `404 Not Found` on token exchange | The GitHub App is not installed in the target organization or the `installationId` does not match. |
| `422 Unprocessable Entity` | The App lacks the `Actions: Read` and `Administration: Read` permissions. |

**Diagnostics.**

```sh
# Check Secret keys exist and are non-empty
kubectl get secret -n <namespace> <name> -o jsonpath='{.data.appId}' | base64 -d
kubectl get secret -n <namespace> <name> -o jsonpath='{.data.installationId}' | base64 -d
kubectl get secret -n <namespace> <name> -o jsonpath='{.data.privateKey}' | base64 -d | head -1
# Expected first line: -----BEGIN RSA PRIVATE KEY----- (PKCS#1)
#                  or: -----BEGIN PRIVATE KEY----- (PKCS#8, RSA or Ed25519)

# Verify the App ID and installation ID match the GitHub App
# GitHub UI: Settings → Developer settings → GitHub Apps → <app> → General (App ID)
# GitHub UI: Settings → Developer settings → GitHub Apps → <app> → Install App (Installation ID in URL)
```

**Resolution.** Re-create the Secret with correct values. To trigger a rolling update on the AGC Deployment after fixing the Secret, change `gitHubAppRef.name` in the `ActionsGateway` spec to reference the new Secret name (the GMC will roll the AGC Deployment automatically) or manually restart the Deployment:

```sh
kubectl rollout restart deploy/actions-gateway-controller -n <namespace>
```

See [Getting Started — Rotating GitHub App Credentials](../getting-started.md#rotating-github-app-credentials) for the full rotation procedure.

---

## Token Refresh Errors Spiking

**Symptoms.** `actions_gateway_token_refresh_errors_total` is increasing. GitHub App installation tokens expire after one hour; if refresh fails, new sessions cannot be established once the token expires.

**Likely causes.**
- GitHub API is temporarily unavailable or returning 5xx errors.
- The GitHub App private key was rotated but the Secret was not updated.
- Network path from AGC to GitHub API is down (proxy pool issue).

**Diagnostics.**

```sh
# Check the error rate
# Metric: rate(actions_gateway_token_refresh_errors_total[5m])

# Check AGC logs for the error detail
kubectl logs -n <namespace> deploy/actions-gateway-controller | grep "token refresh"

# Test connectivity to GitHub via the tenant proxy (AGC is distroless — use an
# ephemeral curl pod in the same namespace; it picks up the same NetworkPolicy egress).
kubectl run nettest-$$ -n <namespace> --rm -it --restart=Never \
  --image=curlimages/curl:latest \
  --overrides='{"spec":{"automountServiceAccountToken":false,"containers":[{"name":"c","image":"curlimages/curl:latest","command":["sh","-c","curl -x https://actions-gateway-proxy:8080 -sI https://api.github.com/app"]}]}}'
```

**Resolution.**
- If GitHub is temporarily unavailable: the AGC's exponential back-off retry (5s → 60s cap) will recover automatically. Monitor until the error rate returns to zero.
- If the private key was rotated: update the Secret. See [Getting Started — Rotating GitHub App Credentials](../getting-started.md#rotating-github-app-credentials).
- If the proxy is unreachable: see [Proxy Pool Not Scaling](#proxy-pool-not-scaling) and the network connectivity section below.

**SLO.** Token refresh errors should stay below 1 per hour per tenant. Above this rate, begin investigating immediately. In-flight sessions will fail at the next reconnection once the token expires (~1 hour).

---

## RenewJob Failures Rising

**Symptoms.** `actions_gateway_renew_job_errors_total` is increasing. Jobs may start being cancelled by GitHub before completion.

**Likely causes.**
- Network connectivity issues between the AGC and GitHub (via proxy).
- GitHub API is temporarily unavailable.
- The runner job lock window expired before the renewer could refresh (AGC was slow or restarting).

**Diagnostics.**

```sh
# Check recent error rate
# Metric: rate(actions_gateway_renew_job_errors_total[5m])

# Check AGC logs for renewal errors and job IDs
kubectl logs -n <namespace> deploy/actions-gateway-controller | grep "renewjob"

# Confirm the proxy pool is healthy
kubectl get pods -n <namespace> -l app=actions-gateway-proxy
```

**Resolution.**
- Transient GitHub API errors: the renewer retries; monitor until the rate returns to zero.
- Proxy pool unhealthy: fix the proxy pool (see [Proxy Pool Not Scaling](#proxy-pool-not-scaling)).
- If the AGC restarted mid-job: jobs whose lock expired will have been cancelled by GitHub. These require manual re-run. Check `actions_gateway_eviction_retries_exhausted_total` for any jobs that were also evicted.

Each `renewjob` error is a warning, not an immediate job failure — GitHub grants ~10 minutes per renewal window. A single transient error on a long-running job is rarely fatal.

---

## Sessions Stuck in 401/EOF GetMessage Loops (Tenant Throughput Decays to Zero)

**Symptoms.** On gateway versions without the Q114 self-heal (≤ the M4 validation build):
- AGC logs fill with repeating `GetMessage error ... decode response: EOF` and later `broker: unauthorized (HTTP 401)` lines for the same session, backing off forever.
- The repo/org runner list (`gh api .../actions/runners`) loses one runner after each completed job, and the registrations never come back.
- `RunnerGroup` `status.activeSessions` decays over time; after roughly `maxListeners` completed jobs, queued workflow jobs wait forever even though the AGC pod is healthy.

**Cause.** GitHub deletes a JIT-registered runner record once it acquires a job (single-use runners). Pre-fix AGC versions keep polling the dead session with the dead agent's credentials instead of re-registering, so every completed job permanently burns one listener slot ([M4 §12, bug 2](../plan/milestone-4.md#12-live-multi-tenant-validation-evidence-2026-06-1112)).

**Diagnostics.**

```sh
# Repeating EOF/401 poll errors
kubectl logs -n <namespace> deploy/actions-gateway-controller | grep -E "decode response: EOF|unauthorized"

# Listener slots remaining
kubectl get runnergroup -n <namespace> -o jsonpath='{.items[*].status.activeSessions}'

# On fixed versions, recycles should be happening instead:
# Metric: rate(actions_gateway_agent_recycles_total[15m])  — roughly tracks job completions
# Metric: actions_gateway_agent_recycle_errors_total       — should stay flat
```

**Resolution.**
- **Upgrade** to a gateway version with the Q114 self-heal. Fixed versions re-register each agent after every job (`actions_gateway_agent_recycles_total{trigger="post_job"}`) and heal stale sessions discovered after a restart (`trigger="stale_session"` / `"startup"`); no per-job operator action is needed.
- **Interim manual recovery on pre-fix versions:** delete the RunnerGroup's agent Secrets and restart the AGC so it registers a fresh pool:

  ```sh
  kubectl delete secret -n <namespace> -l actions-gateway/runner-group=<group>
  kubectl rollout restart deploy/actions-gateway-controller -n <namespace>
  ```

  Expect `409 Already exists` registration errors for any agent that never ran a job — its record survives server-side under an ID the AGC no longer knows. Delete the survivor from GitHub first: find its ID with `gh api '.../actions/runners?name=<group>-<index>'`, then `gh api -X DELETE .../actions/runners/<id>`. Fixed versions resolve this 409 automatically.

**On fixed versions,** a sustained rise of `actions_gateway_agent_recycle_errors_total` means the AGC cannot re-register agents (registration API unreachable, installation token failures, or revoked GitHub App runner-administration permission) — listener capacity shrinks until recycles succeed. Check AGC logs for `recycle` errors and verify the App's runner permissions.

---

## Network Connectivity Failures

**Symptoms.** The AGC cannot reach GitHub through the proxy. Logs show `connection refused`, `dial tcp: i/o timeout`, or `proxy: no response from proxy`.

**Likely causes.**
- The proxy pod is not running or not ready.
- `HTTP_PROXY`/`HTTPS_PROXY` environment variables are incorrect (wrong Service name or port).
- `actions-gateway-workload` NetworkPolicy is blocking the AGC-to-proxy egress path (e.g. proxy ClusterIP changed after a recreate and the rule wasn't reconciled).
- `actions-gateway-proxy` NetworkPolicy is blocking the proxy's egress to GitHub (IP ranges stale or `managedNetworkPolicy: false` with no replacement rule).
- `actions-gateway-controller` NetworkPolicy is missing — AGC can't reach the K8s API server, so token refresh and webhook health checks fail before any GitHub traffic.

**Diagnostics.**

```sh
# Check proxy pod status
kubectl get pods -n <namespace> -l app=actions-gateway-proxy

# Verify the proxy Service exists and has endpoints
kubectl get svc -n <namespace> actions-gateway-proxy
kubectl get endpoints -n <namespace> actions-gateway-proxy

# Check the AGC container's HTTPS_PROXY env var (distroless — inspect spec, not the running process)
kubectl get pod -n <namespace> -l app=actions-gateway-controller \
  -o jsonpath='{range .items[0].spec.containers[?(@.name=="agc")].env[?(@.name=="HTTPS_PROXY")]}{.name}={.value}{"\n"}{end}'

# Test proxy connectivity using an ephemeral curl pod in the same namespace
kubectl run nettest-$$ -n <namespace> --rm -it --restart=Never \
  --image=curlimages/curl:latest \
  --overrides='{"spec":{"automountServiceAccountToken":false,"containers":[{"name":"c","image":"curlimages/curl:latest","command":["sh","-c","curl -v -x https://actions-gateway-proxy:8080 https://api.github.com 2>&1 | head -20"]}]}}'

# Check NetworkPolicy rules — there are three: workload, agc, proxy
kubectl get networkpolicy -n <namespace>
# Expected: actions-gateway-workload, actions-gateway-controller, actions-gateway-proxy
kubectl describe networkpolicy -n <namespace>

# Check the IP range refresh metric
# Metric: actions_gateway_ip_range_updates_total{namespace="<namespace>"}
```

**Resolution.**
- If the proxy pod is down: check its logs and restart if necessary.
- If the `NetworkPolicy` egress rules are stale: trigger a manual refresh by temporarily setting `spec.proxy.managedNetworkPolicy: false` and back to `true`, or wait for the 24-hour automatic refresh cycle. Check the GitHub meta API for current IP ranges: `curl https://api.github.com/meta | jq .actions`.
- If the `NO_PROXY` list is missing the cluster service CIDR: update `spec.proxy.noProxyCIDRs` to include your cluster's service CIDR (see the `noProxyCIDRs` field documentation in [§3.1](../design/03-api-contracts.md#31-kubernetes-crd-schemas)).

---

## AGC Cannot Reach the Kubernetes API Server (NetworkPolicy + post-DNAT port mismatch)

**Symptoms.** AGC logs show `dial tcp 10.96.0.1:443: i/o timeout` (or similar) when calling the K8s API server. The `actions-gateway-controller` NetworkPolicy *appears* to allow port 443, yet the connection is silently dropped. Most often surfaces in kind, but possible on any cluster where the `kubernetes` Service backends listen on a port other than 443.

**Cause.** NetworkPolicy enforcement evaluates packets *after* kube-proxy's DNAT. When a pod connects to `kubernetes.default.svc` (ClusterIP `10.96.0.1:443`), kube-proxy DNATs the destination to the apiserver's actual Endpoints address — in kind, that's `<node-ip>:6443`. The policy controller sees the post-DNAT destination port (6443), and an NP rule that allows only port 443 doesn't match. This is the port-axis equivalent of the `ipBlock: <ClusterIP>/32` trap that bit the proxy NP in PR #59.

**Diagnostics.**

```sh
# 1. Confirm the apiserver Endpoints port. If it's 6443, the AGC NP must allow 6443.
kubectl get endpointslice -n default -l kubernetes.io/service-name=kubernetes \
  -o jsonpath='{.items[0].ports[0].port}{"\n"}'

# 2. Confirm the AGC NetworkPolicy actually allows both 443 and 6443.
kubectl get networkpolicy -n <namespace> actions-gateway-controller -o yaml \
  | yq '.spec.egress[].ports[].port' | sort -u

# 3. If the cluster uses kindnet / kube-network-policies, check the verdict log
#    on the node hosting the AGC pod. Look for lines like:
#      "Pod is not allowed to connect to port" pod="<ns>/<agc-pod>" port=6443
kubectl get pod -n <namespace> -l app=actions-gateway-controller \
  -o jsonpath='{.items[0].spec.nodeName}{"\n"}'
kubectl logs -n kube-system -l app=kindnet --tail=200 --field-selector spec.nodeName=<node-name>
```

**Resolution.** Ensure `buildAGCNetworkPolicy` allows both port 443 (production Service shape) *and* port 6443 (kind / Endpoints-on-6443 clusters). The shipped policy does this. If you see this on a custom build or a hand-edited NP, add the 6443 rule. The diagnosis writeup at [`docs/development/networkpolicy-port-matching.md`](../development/networkpolicy-port-matching.md) has a minimal repro and the reasoning behind allowing both ports.

If you see the same symptom for an *ingress*-type rule or for a different Service whose backend port differs from the Service port, the same fix applies: list both ports, or omit the port restriction on that rule.

---

## Worker Pod Runner.Worker Fails TLS Handshake With UntrustedRoot

**Symptoms.** Worker pod logs (look at the `runner` container) contain repeated lines like:

```
System.Security.Authentication.AuthenticationException: The remote certificate is invalid because of errors in the certificate chain: UntrustedRoot
```

emitted from `JobExtension` connectivity checks, `ResultServer` init, `JobServerQueue` log uploads, the `GitHubActionsService` log-blob fetch, or `RunServer.CompleteJobAsync`. The runner retries for ~3 minutes, then exits 1. The AGC then logs `worker pod completed phase=Failed`, `renewjob` starts returning `401 Not authorized for this job`, and the GitHub workflow concludes `cancelled` even though the actual job steps may have run.

**Cause.** Runner.Worker's .NET HttpClient is validating the egress proxy's TLS cert and the worker pod's trust store does not include the cert-manager-issued self-signed CA that signed it. This is the worker-side mirror of the AGC's proxy-CA pinning ([§5.2](../design/05-security.md) "Cross-Tenant Proxy CA Trust"): the AGC mounts the CA explicitly so its outbound HTTPS works; worker pods must do the same.

The AGC's pod provisioner is supposed to project the per-tenant `actions-gateway-proxy-tls` Secret into every worker pod at `/etc/actions-gateway/proxy-ca/tls.crt` and set `PROXY_CA_CERT_PATH` so the worker entrypoint wrapper builds a combined `SSL_CERT_FILE` bundle before exec'ing `Runner.Worker`. UntrustedRoot means one of those steps did not happen.

**Diagnostics.**

```sh
# 1. Inspect a failed worker pod's spec — the Secret volume must exist.
kubectl get pod -n <namespace> <worker-pod-name> -o yaml \
  | yq '.spec.volumes[] | select(.name=="proxy-ca")'
# Expected: a Secret volume with secretName: actions-gateway-proxy-tls and Items: [{key: tls.crt, path: tls.crt}]
# If empty: the AGC was deployed without PROXY_TLS_SECRET_NAME.

# 2. Confirm the AGC has the PROXY_TLS_SECRET_NAME env wired.
kubectl get pod -n <namespace> -l app=actions-gateway-controller \
  -o jsonpath='{range .items[0].spec.containers[?(@.name=="agc")].env[?(@.name=="PROXY_TLS_SECRET_NAME")]}{.name}={.value}{"\n"}{end}'
# Expected: PROXY_TLS_SECRET_NAME=actions-gateway-proxy-tls
# Empty means the GMC needs to roll the AGC Deployment (likely an upgrade across the 5h boundary).

# 3. Confirm the worker container's PROXY_CA_CERT_PATH env.
kubectl get pod -n <namespace> <worker-pod-name> -o yaml \
  | yq '.spec.containers[] | select(.name=="runner") | .env[] | select(.name=="PROXY_CA_CERT_PATH")'

# 4. Confirm the proxy TLS Secret exists and contains tls.crt.
kubectl get secret -n <namespace> actions-gateway-proxy-tls \
  -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl x509 -noout -subject -issuer -dates
```

**Resolution.**
- If the worker pod has no `proxy-ca` volume: ensure the AGC was started with `PROXY_TLS_SECRET_NAME=actions-gateway-proxy-tls` (the GMC injects this automatically — if it's missing, the GMC needs to roll the AGC Deployment, e.g. by bumping `ag.Spec` or restarting the GMC).
- If the volume is present but the wrapper logs nothing about `proxy CA trust installed`: check that `PROXY_CA_CERT_PATH` is set on the runner container and the mounted file is non-empty. An empty/missing file is tolerated as a no-op, which silently leaves the runner with no proxy trust — the wrapper log line `no proxy CA cert mounted; skipping trust-store install` distinguishes this case from a wrapper that ran the install successfully.
- If the proxy TLS Secret is missing or the cert has expired: the GMC's cert-manager integration ([§2.1](../design/02-architecture.md#21-tier-1--gateway-manager-controller-gmc) "Proxy Deployer") owns rotation; check the GMC's logs for issuer errors. As a fallback, deleting the Secret triggers reissuance.
- If the issue persists after the volume and env are correct: confirm the proxy pod is presenting the cert signed by the CA in the Secret — `kubectl exec` into a curl pod in the same namespace and run `openssl s_client -connect actions-gateway-proxy:8080 -showcerts </dev/null` to inspect what the proxy actually serves.

---

## Evicted Worker Pods Exhausting Retry Budget

**Symptoms.** `actions_gateway_eviction_retries_exhausted_total` is incrementing. Jobs are being cancelled after eviction despite automatic retries. `kubectl describe` on the owning `RunnerGroup`/`RunnerSet` shows a `Warning` event with reason `EvictionRetriesExhausted` (Q170) naming the affected run — the event-based companion to the metric.

**Likely causes.**
- Worker pod keeps being evicted on every attempt (persistent node pressure, OOM loop, or scheduling conflict that prevents the pod from completing).
- `maxEvictionRetries` is set too low for a workload that occasionally experiences preemption.

**Diagnostics.**

```sh
# Check eviction retry metrics
# actions_gateway_eviction_retries_total{namespace, runner_group}
# actions_gateway_eviction_retries_exhausted_total{namespace, runner_group}

# Check recent evicted pods
kubectl get pods -n <namespace> --field-selector=status.phase=Failed | grep Evicted

# Describe an evicted pod for the eviction reason
kubectl describe pod -n <namespace> <evicted-pod-name>
# Look for: "The node was low on resource: memory" or "Preempted by another pod"

# Check node events around the eviction time
kubectl get events -n <namespace> --sort-by='.lastTimestamp' | grep -i evict

# See the budget-exhaustion event on the owner (RunnerGroup or RunnerSet)
kubectl describe runnergroup -n <namespace> <name> | grep -A1 EvictionRetriesExhausted
```

**Resolution.**
- If evictions are due to node memory pressure: increase the worker pod's memory requests to discourage the kubelet from evicting it, or investigate the workload's actual memory usage.
- If evictions are from preemption by higher-priority pods: reduce the priority of competing workloads or adjust `priorityTiers` to give this RunnerGroup a higher floor.
- If the retry budget is too low for a workload that occasionally gets preempted: increase `maxEvictionRetries` on the RunnerGroup spec (default 2, max 10).
- If the workload is consistently failing (OOM crash, not preemption): the auto-retry is not appropriate. Set `maxEvictionRetries: 0` and investigate the underlying workload issue.

---

## Jobs Failing Due to Namespace ResourceQuota Exhaustion

**Symptoms.** `actions_gateway_quota_retries_exhausted_total` is incrementing. Pod creation fails with a `Forbidden` error containing "exceeded quota" in the AGC logs. Jobs are being abandoned before a pod is ever scheduled. The `RunnerGroup` reports `WorkerQuotaExceeded=True` (and `actions_gateway_worker_quota_exceeded` reads `1`). `kubectl describe` on the owning `RunnerGroup`/`RunnerSet` shows a `Warning` event with reason `QuotaRetriesExhausted` (Q170) each time a job's pod creation is abandoned.

The AGC surfaces two non-blocking conditions on each `RunnerGroup` for the namespace-quota axis (Q82), each exported as a gauge so you can alert without kube-state-metrics. They are distinct from Q59's configured-ceiling backpressure (`actions_gateway_jobs_admission_rejected_total`), which is normal load-shedding to a sibling, not a quota problem:

| Condition / metric | Meaning | Severity |
|---|---|---|
| `WorkerQuotaPressure` — `actions_gateway_worker_quota_pressure` | Workers can't scale to the configured ceiling (`maxWorkers` / max `priorityTiers` threshold) within the quota's remaining headroom. | warning (don't page) |
| `WorkerQuotaExceeded` — `actions_gateway_worker_quota_exceeded` | The quota can't admit even one more worker pod — the next acquired job's pod will be rejected. | error (page) |

```sh
kubectl get runnergroup -n <namespace> <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="WorkerQuotaExceeded")]}{.status} {.reason}: {.message}{"\n"}{end}'
```

**Likely causes.**
- The namespace ResourceQuota `pods` or `requests.cpu`/`requests.memory` limit is too low for the burst of concurrent jobs arriving.
- A long-running job is holding quota that a new job needs; quota will clear once it completes.
- The quota retry budget (`maxQuotaRetries`, default 5) is exhausting before quota clears.

**Diagnostics.**

```sh
# Check quota retry metrics
# actions_gateway_quota_retries_total{namespace, runner_group}
# actions_gateway_quota_retries_exhausted_total{namespace, runner_group}

# Inspect current quota usage
kubectl describe resourcequota -n <namespace>

# Check AGC logs for quota errors
kubectl logs -n <agc-namespace> deploy/actions-gateway-controller | grep "exceeded quota"

# See the abandonment event on the owner (RunnerGroup or RunnerSet)
kubectl describe runnergroup -n <namespace> <name> | grep -A1 QuotaRetriesExhausted
```

**Resolution.**
- If quota is consistently full: increase the namespace `ResourceQuota` limits or reduce `maxWorkers` / `priorityTiers` thresholds so the AGC holds fewer concurrent pods.
- If quota clears quickly but the retry window is too short: increase `maxQuotaRetries` or `quotaRetryDelay` on the RunnerGroup spec (defaults: 5 retries / 30s delay).
- If quota retry is causing unwanted job-lock hold time: set `maxQuotaRetries: 0` to fail immediately on quota exhaustion — the job lock is dropped and GitHub redelivers the job.

---

## Jobs Not Being Acquired Despite Queued Work (Capacity Gate Saturated)

**Symptoms.** Workflow jobs sit queued in GitHub while `actions_gateway_jobs_admission_rejected_total{namespace, runner_group}` climbs and `actions_gateway_jobs_acquired_total` plateaus for the same group. `kubectl get pods` shows the group already running its full complement of worker pods. The AGC is healthy — this is throttling, not a fault.

**Cause.** This is the pre-acquisition admission gate working as designed (Q59). When a RunnerGroup is already at its worker ceiling (`maxWorkers`, or the maximum `priorityTiers` threshold), the AGC **deliberately skips `acquirejob`** for newly delivered jobs and leaves them queued at GitHub, so they are redelivered to a session with capacity rather than claimed-then-dropped (which would get the run cancelled). A rising rejection counter therefore means *demand exceeds the configured ceiling*, not that anything is broken. The gate's reservation count is in-memory and resets on AGC restart, so a brief post-restart burst of acquisitions is normal.

**Diagnostics.**

```sh
# Compare admission rejections against successful acquisitions for the group.
# A sustained gap with rejections rising = the ceiling is the bottleneck.
#   actions_gateway_jobs_admission_rejected_total{namespace, runner_group}
#   actions_gateway_jobs_acquired_total{namespace, runner_group}

# Confirm the group is at its ceiling.
kubectl get pods -n <namespace> -l actions-gateway/runner-group=<group> \
  --field-selector status.phase=Running

# Read the configured ceiling.
kubectl get runnergroup <group> -n <namespace> \
  -o jsonpath='{.spec.maxWorkers}{"\n"}{.spec.priorityTiers}{"\n"}'
```

**Resolution.**
- If the ceiling is intentionally protective (e.g. it sits below the namespace `ResourceQuota` to leave headroom): no action — jobs drain as in-flight work completes, and GitHub redelivers within its delivery window.
- If the group should run more concurrent jobs: raise `maxWorkers` (or the top `priorityTiers` threshold) on the RunnerGroup spec, and ensure the namespace `ResourceQuota` has matching headroom — otherwise the [ResourceQuota path](#jobs-failing-due-to-namespace-resourcequota-exhaustion) becomes the new bottleneck.
- If rejections appear with worker pods **below** the ceiling, suspect leaked reservations from pods that never reached a terminal phase — check for [stuck-Pending pods](#worker-pod-reaped-while-pending-workerpodstuckpending); the gate's slot is released when the job completes or its pod is reaped. An AGC restart clears any stale in-memory reservation.

---

## Worker Pod Fails to Start After Secure-by-Default SecurityContext

**Symptoms.** A worker pod that previously ran now stays in `CreateContainerConfigError` or `Pending`, or is rejected at admission. `kubectl describe pod` shows one of:
- `Error: container has runAsNonRoot and image has non-numeric user (<name>), cannot verify user is non-root` — the AGC stamped `runAsNonRoot: true` (every profile except `privileged`) and the image declares its user **by name**, which kubelet cannot verify against a numeric UID. The **default** `ghcr.io/actions/actions-runner` image (`USER runner`) is handled automatically — the AGC gap-fills `runAsUser: 1001` so kubelet can verify it (Q115). You hit this only with a **custom/third-party** runner image whose named user is **not** UID 1001, so the auto-stamped 1001 doesn't match what its `USER` resolves to, or whose image has no numeric UID at all.
- `Error: container has runAsNonRoot and image will run as root` — same stamp, but the worker image's default user is `root` (UID 0).
- A PodSecurity admission denial naming `allowPrivilegeEscalation != false` or `unrestricted capabilities` — the namespace is on `securityProfile: restricted` and a tenant container needs `sudo` or extra capabilities.

**Likely causes.**
- A **custom** worker image declares a **named** (non-numeric) user other than the default runner's UID 1001. The AGC's secure-by-default gap-fill stamps `runAsUser: 1001` to match the upstream `actions-runner` image; an image whose user is a different UID still needs its own numeric UID declared. (The default `actions-runner` image needs no action.)
- The worker image runs as root by default (common for custom or third-party runner images). The AGC's secure-by-default `runAsNonRoot: true` then blocks it.
- A job under `restricted` calls `sudo` or installs packages requiring capabilities the PSA-restricted floor drops.

**Diagnostics.**

```sh
# See the exact rejection / config error
kubectl describe pod -n <tenant-namespace> <pod> | sed -n '/Events:/,$p'

# Confirm the namespace's enforced PSA profile
kubectl get ns <tenant-namespace> -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}'; echo

# Confirm the SECURITY_PROFILE the AGC is running with
kubectl get deploy actions-gateway-controller -n <tenant-namespace> -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="SECURITY_PROFILE")].value}'; echo
```

**Resolution.**
- Default `actions-runner` image: **no action needed** — the AGC gap-fills `runAsUser: 1001` automatically (Q115).
- Custom named-user image whose user is **not** UID 1001: declare its actual numeric UID in the RunnerGroup `podTemplate` so kubelet can verify non-root (an explicit `runAsUser` overrides the gap-filled 1001):

  ```yaml
  podTemplate:
    spec:
      securityContext:
        runAsUser: <image-uid>
        runAsGroup: <image-gid>
  ```

  Note: a `podTemplate` edit takes effect on the next acquired job — the AGC
  re-reads the RunnerGroup at pod-build time, so no AGC restart is needed
  (Q117). Pods already running keep the template they were built with.
- Root-based image that must run as root: the defaults are gap-fill only — set an explicit `securityContext.runAsNonRoot: false` (and `runAsUser`/`runAsGroup` as needed) on the runner container in the RunnerGroup `podTemplate`. No profile escalation is required for `baseline`.
- Job genuinely needs `sudo`/capabilities: move that workload to a `baseline` `ActionsGateway` (the default), which does not stamp the privilege-escalation/capability floor. Reserve `restricted` for workloads that can run without them.
- Workload needs a real privileged container (DinD, kernel modules): set `securityProfile: privileged` on the `ActionsGateway` and pair it with a sandbox runtime — see [§5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in).

---

## securityProfile Downgrade Rejected by Admission Webhook

**Symptoms.** A `kubectl apply` / `kubectl edit` / GitOps sync that changes an existing `ActionsGateway`'s `spec.securityProfile` to a *less restrictive* level is rejected by the GMC validating webhook with:

```
admission webhook "vactionsgateway-v1alpha1.kb.io" denied the request:
securityProfile downgrade from "restricted" to "baseline" is not permitted
without the "actions-gateway.github.com/allow-profile-downgrade" annotation
set to "true"; downgrading relaxes Pod Security Admission isolation and must
be deliberate
```

The profiles rank `privileged` (least restrictive) < `baseline` < `restricted` (most restrictive); any move *down* that ranking is a downgrade — including `baseline → privileged`.

**Likely causes.**
- A deliberate relaxation — e.g. rolling back a `baseline → restricted` hardening attempt that broke the tenant's pods at PSA admission.
- **Unintentional drift:** re-applying an older manifest, or a Helm/Kustomize render that **omits** `securityProfile` (it then re-defaults to `baseline`) while the live object is on `restricted`. An empty/absent value is compared as `baseline`, so an omitted field reads as a downgrade — this is the guard working as intended, catching a silent weakening.

**Diagnostics.**

```sh
# Current (live) profile vs what your manifest sets
kubectl get actionsgateway -n <tenant-namespace> <name> -o jsonpath='{.spec.securityProfile}'; echo
```

**Resolution.**
- **If the downgrade is intended:** add the opt-in annotation, then change the profile (one apply works if both are in the manifest):
  ```sh
  kubectl annotate actionsgateway -n <tenant-namespace> <name> \
    actions-gateway.github.com/allow-profile-downgrade=true --overwrite
  ```
  Remove the annotation afterward if you want future accidental downgrades to keep being blocked. PSA enforce is namespace-scoped, so the new (looser) profile applies to *future* worker pods once the GMC re-stamps the namespace label; pods already running are not re-evaluated.
- **If the downgrade is accidental (drift):** do **not** add the annotation — fix the manifest to match the live profile (set `securityProfile: restricted`, or stop omitting it) so GitOps stops trying to weaken the namespace.

> Note: this guard catches *silent* downgrades; it is not an absolute boundary. Anyone with edit access to the CR can set the annotation, and an operator with direct namespace `patch` rights can change the PSA labels regardless. See [§5.3 — No silent profile downgrades](../design/05-security.md#no-silent-profile-downgrades).

---

## Second ActionsGateway in a Namespace Rejected (Singleton Guard)

**Symptoms.** Creating an `ActionsGateway` in a namespace that already has one is rejected by the GMC validating webhook with:

```
admission webhook "vactionsgateway-v1alpha1.kb.io" denied the request:
an ActionsGateway ("first-ag") already exists in namespace "team-a"; only one
ActionsGateway per namespace is supported — a second CR contends over fixed-name
per-tenant resources and would flap the namespace's Pod Security Admission labels
```

**Likely causes.**
- Two manifests (or two GitOps apps) target the same tenant namespace.
- A renamed CR applied before the old one was deleted (the guard reads live state, so the old CR still counts until its delete completes).

**Why it is enforced.** Every per-tenant resource the GMC provisions — the AGC Deployment, the proxy Deployment, Services, NetworkPolicies, RoleBindings — has a fixed, namespace-scoped name. Two CRs fight over those objects, and because each CR's `securityProfile` drives the namespace's Pod Security Admission labels, two CRs with different profiles make the GMC flap those labels (intermittently admitting privileged pods). Deleting either CR then tears down the survivor's infrastructure.

**Resolution.** Use one `ActionsGateway` per namespace. To run a second logical gateway, give it its own namespace (the guard is per-namespace, so a different namespace's first CR is admitted). To rename or replace an existing CR, delete the old one and wait for teardown to complete before creating the replacement.

> **`v2alpha1` lifts this restriction.** The singleton guard is a `v1alpha1`-only constraint rooted in fixed per-tenant resource names. The `v2alpha1` (`actions-gateway.com`) API supports **multiple `ActionsGateway`s per namespace**: every derived resource is named per gateway (`<gateway>-agc`, `<gateway>-worker`, …) and each gateway's AGC reconciles only its own `RunnerSet`s, so they never contend. `securityProfile` also moved off the gateway onto the namespace, so co-located gateways share one Pod Security posture instead of flapping it. See ["Multiple v2 gateways in one namespace"](#multiple-v2-gateways-in-one-namespace-naming-scoping-prerequisites) below.

---

## `proxy.noProxyCIDRs` Rejected: Entry Would Bypass the Proxy for GitHub

**Symptoms.** A `kubectl apply` is rejected by the GMC validating webhook with:

```
admission webhook "vactionsgateway-v1alpha1.kb.io" denied the request:
proxy.noProxyCIDRs[0]: "github.com" would route GitHub traffic (github.com)
around the per-tenant egress proxy, defeating egress-IP attribution; remove it
— GitHub must always traverse the proxy. noProxyCIDRs may exclude internal
destinations (CIDRs or domain suffixes), never GitHub
```

**Likely cause.** A `spec.proxy.noProxyCIDRs` entry NO_PROXY-matches a GitHub host: `github.com`, `.github.com`, `api.github.com`, `githubusercontent.com`, `ghcr.io`, your configured `gitHubURL` host (including a GitHub Enterprise Server host), or an over-broad suffix like `.com` that covers them.

**Why it is enforced.** `noProxyCIDRs` is threaded into the AGC/worker `NO_PROXY` env var, where a hostname entry is a domain-suffix match. If it matches a GitHub host, that tenant's GitHub traffic skips the per-tenant egress proxy — defeating the egress-IP attribution that isolates tenants.

**Resolution.** Remove the GitHub-matching entry — GitHub must always traverse the proxy. `noProxyCIDRs` is for *internal* destinations only and accepts CIDRs (`10.0.0.0/8`), bare IPs, and non-GitHub domain suffixes (`svc.cluster.local`, `internal.example.com`). Note the guard cannot detect a **CIDR/IP range** that happens to cover GitHub's (rotating) published ranges — never add those either; that residual is the operator's responsibility.

---

## Privileged Worker Container Rejected by Admission

**Symptoms.** An `ActionsGateway` whose `runnerGroups[].podTemplate` requests a privileged container or init container is rejected by the GMC validating webhook with:

```
admission webhook "vactionsgateway-v1alpha1.kb.io" denied the request:
runnerGroups[0]: privileged containers are not permitted in worker pods
(container "runner")
```

**Likely cause.** The CR requests `securityContext.privileged: true` while `spec.securityProfile` is `baseline` (the default) or `restricted`. Privileged worker containers are permitted **only** under the explicit `securityProfile: privileged` opt-in, which also stamps the namespace's Pod Security Admission level to `privileged` so the pod is actually admittable.

**Resolution.**
- If the privileged worker is intended (e.g. the Kata/DinD pattern), set `spec.securityProfile: privileged` on the same `ActionsGateway`. This requires the namespace to be **eligible** for privileged — a platform admin must label it `actions-gateway.github.com/privileged-profile=allowed` (see [Privileged securityProfile Rejected](#privileged-securityprofile-rejected-namespace-not-eligible) below). Privileged is a deliberate, audited relaxation — pair it with a sandboxed `runtimeClassName` (Kata, gVisor) per [§5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in).
- If the privileged flag is accidental, remove `securityContext.privileged: true` from the pod template; the secure-by-default profiles reject it on purpose.

> Note: this webhook check only covers the GMC-managed `ActionsGateway` path. A directly-applied `RunnerGroup` CR bypasses the webhook entirely — Pod Security Admission (stamped per the namespace's profile) is the real enforcement backstop for both paths.

---

## `RunnerTemplate` Rejected: Reserved Pod Field (`v2alpha1`)

> Applies to the `v2alpha1` (`actions-gateway.com`) API, currently early-adopter only. The `v1alpha1` path uses the `ActionsGateway`/`RunnerGroup` checks above.

**Symptoms.** Creating or updating a `RunnerTemplate` (or `ClusterRunnerTemplate`) is rejected by the GMC validating webhook with one of:

```
admission webhook "vrunnertemplate-v2alpha1.kb.io" denied the request:
podTemplate.spec.containers["runner"]: env "HTTP_PROXY" is reserved: the AGC
injects the egress-proxy variables (HTTP_PROXY/HTTPS_PROXY/NO_PROXY/PROXY_CA_CERT_PATH)
into worker containers; setting it in a template is overridden and not permitted
```

```
admission webhook "vrunnertemplate-v2alpha1.kb.io" denied the request:
podTemplate.spec.containers["runner"]: privileged containers are not permitted
in a namespaced RunnerTemplate; use a platform-owned ClusterRunnerTemplate for
privileged (DinD/sysbox) worker shapes
```

**Likely cause.** A worker pod's identity and egress wiring are controller-enforced invariants. In `v1alpha1` the AGC silently overwrote these fields when it built the pod; `v2alpha1` makes them an author-time rejection so a template fails closed instead of being rewritten behind your back.

- **Reserved proxy env vars** (`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`/`PROXY_CA_CERT_PATH`, matched case-insensitively) are injected by the AGC and may not be set in a template — on either kind.
- **Privileged containers** are rejected on the namespaced `RunnerTemplate` (a tenant must not self-author a privileged worker shape) but **allowed** on the cluster-scoped `ClusterRunnerTemplate`, which only a platform administrator can create.

The scalar reserved pod-level fields (`serviceAccountName`, `host{PID,Network,IPC}`, `automountServiceAccountToken`) are rejected by the CRD's own validation rules with a similar "is reserved" message.

**Resolution.**
- Remove the reserved proxy env vars from every container and init container; the AGC sets them itself.
- For a **privileged** worker shape (Kata/DinD/sysbox), have a platform administrator publish it as a `ClusterRunnerTemplate` and reference it from the `RunnerSet`'s `templateRef` with `kind: ClusterRunnerTemplate`. Privileged pods still require the namespace's Pod Security Admission level to admit them (stamped from the effective `securityProfile`), which remains the runtime backstop.

---

## `RunnerSet` Stuck `Ready=False` With a `NotFound` Reason (`v2alpha1`)

> Applies to the `v2alpha1` (`actions-gateway.com`) API, currently early-adopter only.

**Symptoms.** A `RunnerSet` never starts acquiring jobs and `kubectl describe runnerset <name>` shows `Ready=False` with one of:

```
Reason: GatewayNotFound    Message: ActionsGateway "gw" not found in namespace "team-a"
Reason: TemplateNotFound   Message: RunnerTemplate "dind-large" not found in namespace "team-a"
Reason: ProxyNotFound      Message: EgressProxy "shared" not found in namespace "team-a"
```

**Likely cause — this is by design, not an error.** v2 resolves a `RunnerSet`'s references (`gatewayRef`, `templateRef`, `proxyRef`/the gateway's `defaultProxyRef`) **at runtime**, not at admission, so applying a directory in any order converges (GitOps-friendly). Until every reference resolves the set sits `Ready=False` with the specific `*NotFound` reason and **provisions no worker pods** — fail-closed, so no traffic is ever permitted in the gap. The AGC watches the referents and flips the set to `Ready` the moment the missing object syncs; **no re-apply of the `RunnerSet` is needed**.

A `ProxyNotFound` here means a `proxyRef`/`defaultProxyRef` **names an `EgressProxy` that does not exist** — a named-but-missing reference fails closed (it does not silently fall back to direct egress). Apply the named `EgressProxy`, or remove the reference if you want direct egress. **Unset everywhere is not an error:** a `RunnerSet` with no `proxyRef` under a gateway with no `defaultProxyRef` resolves to **direct egress** (`Ready=True`, `status.proxyMode: Direct`, advisory `EgressUnattributed` condition), not `ProxyNotFound` — see ["RunnerSet reports `EgressUnattributed`"](#runnerset-or-gateway-reports-egressunattributed-direct-egress-v2alpha1).

A `ClusterRunnerTemplate` ref (`templateRef.kind: ClusterRunnerTemplate`) resolves the same way: `TemplateNotFound` means the named cluster-scoped template does not exist yet. The AGC reads it through a per-gateway `ClusterRoleBinding` to the shipped `agc-clusterrunnertemplate-reader` ClusterRole that the GMC creates with the gateway — so if every namespaced reference resolves but a `ClusterRunnerTemplate` ref stays `TemplateNotFound`, confirm a platform administrator has created that `ClusterRunnerTemplate` (it is cluster-scoped and platform-authored; tenants cannot create it).

**Resolution.** Apply the missing object (`ActionsGateway`, `RunnerTemplate`/`ClusterRunnerTemplate`, or `EgressProxy`) named in the message; the set self-heals on the next watch event. Confirm the referent's name and namespace match the `*Ref` exactly (references resolve in the `RunnerSet`'s own namespace).

---

## v2 `ActionsGateway` Stuck `Ready=False` (`CredentialUnavailable` / `ProxyNotFound`)

> Applies to the `v2alpha1` (`actions-gateway.com`) API, currently early-adopter only. The v1 `ActionsGateway` provisioning checks are above (["GMC Not Provisioning Tenant Resources"](#gmc-not-provisioning-tenant-resources)).

**Symptoms.** No AGC Deployment appears in the tenant namespace and `kubectl describe actionsgateway <name>` shows `Ready=False` with either:

```
CredentialUnavailable=True  Reason: SecretNotFound
  Message: GitHub App Secret "github-app" not found in namespace "team-a"
```
or
```
Ready=False  Reason: ProxyNotFound
  Message: EgressProxy "shared" (defaultProxyRef) not found in namespace "team-a"
```

**Likely cause.** The v2 gateway provisions the AGC control plane only after its preconditions resolve, and **fails closed** otherwise (no AGC Deployment is created):

- **`CredentialUnavailable`** — the Secret named by `spec.credentials.githubApp.name` does not exist in the gateway's namespace. The AGC mounts the GitHub App credential as files, so without it there is nothing to provision.
- **`ProxyNotFound`** — `spec.defaultProxyRef` **names an `EgressProxy` that does not exist**. The AGC's control-plane egress is routed through that proxy, so a dangling reference fails closed. Note this fires only for a *named but missing* proxy: an **unset** `defaultProxyRef` is **not** an error — it means **direct egress** (the gateway reaches Ready with `status.proxyMode: Direct` and an advisory `EgressUnattributed` condition; see below). Apply the named `EgressProxy`, or clear `defaultProxyRef` to use direct egress.

Unlike a `RunnerSet`'s reference resolution, these are the *gateway's own* preconditions; once the Secret or `EgressProxy` appears the gateway reconciles and the AGC Deployment is created (the gateway watches both). Note that the proxy **pool** is reconciled separately by the `EgressProxy` reconciler — the gateway only references it; and the namespace Pod Security Admission labels are stamped by the namespace PSA reconciler from the `actions-gateway.com/security-profile` label, which the gateway *reads* (to thread `SECURITY_PROFILE` to the AGC) but never stamps.

---

## `RunnerSet` or gateway reports `EgressUnattributed` (direct egress) (`v2alpha1`)

> Applies to the `v2alpha1` (`actions-gateway.com`) API, currently early-adopter only.

**Symptoms.** `kubectl get actionsgateway,runnerset -n <ns>` shows `Egress: Direct`, and `kubectl describe` shows an `EgressUnattributed=True` condition (`Reason: DirectEgress`). The object is otherwise `Ready`.

**This is not an error — it is informational.** It means the gateway has no `spec.defaultProxyRef` and/or the `RunnerSet` has no `spec.proxyRef`, so egress goes **directly** to GitHub instead of through an `EgressProxy` (appendix-h §H.10). Direct egress is a supported mode and never makes the object `NotReady`; the condition exists only so an operator can see at a glance that the workload has **no per-tenant egress IP identity** — the trade you make by not attaching a proxy.

**What is still guaranteed.** Egress is still **restricted**: the GMC's default-deny egress NetworkPolicy permits only **DNS (cluster DNS) + the GitHub CIDR allowlist** for workers (plus the kube API server for the AGC), and the IP-range refresh keeps that allowlist current. A direct-egress worker cannot reach an arbitrary internet host. What you lose is only the stable per-tenant *source IP* (needed for GitHub IP-allowlisting / EMU, incident attribution, and avoiding shared-NAT throttling).

**If you wanted attribution.** Create an `EgressProxy` in the namespace and set `spec.defaultProxyRef` on the gateway (or `spec.proxyRef` on the specific `RunnerSet`). The object flips to `proxyMode: Proxied` and the condition clears (`EgressUnattributed=False`). See [tenant-onboarding — Proxy-less onboarding](tenant-onboarding.md#proxy-less-onboarding-direct-egress).

**If GitHub egress fails in direct mode.** Confirm (1) the cluster CNI actually enforces egress NetworkPolicy (kindnet does not — see [tenant-onboarding Pre-Conditions](tenant-onboarding.md#pre-conditions)), and (2) the GMC's GitHub IP-range refresh has run — the direct-egress AGC + workload NetworkPolicies carry the GitHub CIDR allowlist only after the first fetch; `kubectl get networkpolicy <gateway>-workload -o yaml` should show `ipBlock` egress peers on port 443.

**Resolution.** Create the GitHub App Secret (see ["GitHub App Secret Misconfiguration"](#github-app-secret-misconfiguration) for the required keys) and/or the `EgressProxy` named by `defaultProxyRef`, in the gateway's namespace. The gateway self-heals on the next watch event.

---

## Multiple v2 gateways in one namespace: naming, scoping, prerequisites

> Applies to the `v2alpha1` (`actions-gateway.com`) API, currently early-adopter only.

The `v2alpha1` API supports **multiple `ActionsGateway`s per namespace** (unlike `v1alpha1` — see ["Second ActionsGateway in a Namespace Rejected"](#second-actionsgateway-in-a-namespace-rejected-singleton-guard)). A few operator-visible facts and failure modes:

- **Per-gateway resource names.** Every resource a gateway derives is prefixed with the gateway name: `<gateway>-agc` (AGC Deployment / ServiceAccount / RoleBinding / Service / AGC NetworkPolicy), `<gateway>-worker` (worker ServiceAccount), `<gateway>-workload` (workload NetworkPolicy), `<gateway>-agc-metrics-tls` / `-agc-metrics-client` (metrics Secrets). To list one gateway's resources: `kubectl get all,networkpolicy,secret -n <ns> -l actions-gateway.com/gateway=<gateway>`.
- **52-character name cap.** An `ActionsGateway`, `RunnerSet`, `RunnerTemplate`, `ClusterRunnerTemplate`, or `EgressProxy` whose `metadata.name` exceeds **52 characters** is rejected at admission (`metadata.name must be at most 52 characters`). The cap reserves room for the derived suffixes above so a label value / Service name stays within RFC 1123's 63-character ceiling. Use a shorter name.
- **Per-gateway scoping needs Kubernetes ≥ 1.31.** Each AGC reconciles only the `RunnerSet`s whose `spec.gatewayRef.name` targets it, using a server-side CRD field selector (KEP-4358). That selector is **alpha-off in Kubernetes 1.30**: on a 1.30 cluster an AGC's `RunnerSet` informer fails to sync (`field label not supported`) and the AGC pod will not become ready. If a v2 AGC `CrashLoopBackOff`s with a `RunnerSet` list/watch error, check the cluster is ≥ 1.31. (Single-gateway-per-namespace v2 still requires ≥ 1.31 once more than one gateway exists; for one gateway the scoping is a no-op but the selector is still applied.)
- **Per-gateway garbage collection.** Deleting one gateway removes only its own children (owner-referenced, per-gateway-named); a neighbor's gateway, AGC, and `RunnerSet`s are untouched. The one cluster-scoped child — the `agc-clusterrunnertemplate-reader.<ns>.<gateway>` `ClusterRoleBinding` — cannot carry a namespaced owner reference, so the gateway controller deletes it explicitly during teardown. If a gateway's `ClusterRoleBinding` lingers after the gateway is gone (e.g. the GMC was down during the delete), it is harmless (it binds a now-absent ServiceAccount) and safe to `kubectl delete clusterrolebinding agc-clusterrunnertemplate-reader.<ns>.<gateway>`.

**Prerequisite.** The five v2 CRDs ship in the separate `actions-gateway-crds-v2` chart, not the main chart. Install it before creating any v2 object: `helm upgrade --install actions-gateway-crds-v2 oci://ghcr.io/actions-gateway/charts/actions-gateway-crds-v2`. The shipped `agc-clusterrunnertemplate-reader` ClusterRole is in the main chart.

---

## v2 Objects Not Reconciling After Installing the CRD Chart

> Applies to the `v2alpha1` (`actions-gateway.com`) API, currently early-adopter only.

**Symptom.** You created a v2 `ActionsGateway` or `EgressProxy` and nothing happens — no AGC Deployment, no proxy pool, no status conditions. The GMC log shows it came up v1-only:

```text
actions-gateway.com/v2alpha1 CRDs not installed; v2 controllers disabled (install the actions-gateway-crds-v2 chart and restart the GMC to enable them)
```

**Cause.** The GMC checks for the `actions-gateway.com/v2alpha1` CRDs **once at startup**. If they were absent then, it disables the v2 controllers (and the v2 IP-range refresh passes) deliberately — this is the secure, quiet default that keeps a v1-only install from spinning a "no matches for kind" retry loop. Installing the CRD chart into an already-running cluster does **not** retroactively enable them.

**Remediation.**

```sh
# 1. Confirm the v2 CRDs are now installed and Established.
kubectl get crd actionsgateways.actions-gateway.com egressproxies.actions-gateway.com

# 2. Restart the GMC so it re-detects them at startup.
kubectl rollout restart deploy -n gmc-system gmc-controller-manager

# 3. Confirm the GMC now reports v2 enabled.
kubectl logs -n gmc-system deploy/gmc-controller-manager | grep v2alpha1
# Expected: actions-gateway.com/v2alpha1 CRDs detected; enabling v2 controllers
```

After the restart the v2 controllers pick up any v2 objects already in the cluster. v1alpha1 tenants are unaffected throughout — they reconcile whether or not the v2 CRDs are installed.

> **Note on older GMC builds.** A GMC predating this startup gate (before Q228) started the v2 controllers unconditionally, so on a v1-only install it logged `no matches for kind "ActionsGateway"/"EgressProxy" in version "actions-gateway.com/v2alpha1"` every ~10s and the IP-range reconciler logged `failed to list EgressProxies`. The fix is the same — install the CRD chart — or upgrade to a build with the gate, which logs a single info line instead.

---

## Privileged securityProfile Rejected: Namespace Not Eligible

**Symptoms.** An `ActionsGateway` requesting `spec.securityProfile: privileged` is rejected by the GMC validating webhook at create or update with:

```
admission webhook "vactionsgateway-v1alpha1.kb.io" denied the request:
securityProfile: privileged is not eligible in namespace "team-builds": it
requires the namespace label actions-gateway.github.com/privileged-profile=allowed,
which only a platform administrator may apply — privileged eligibility is a
platform decision and is deliberately not tenant-settable
```

A variant names a read failure (`cannot verify privileged eligibility for namespace …`) when the webhook cannot read the namespace; the gate is fail-closed, so that too rejects.

**Likely cause.** Whether a namespace may run privileged workers is a **platform** decision, not a tenant one. Because the tenant owns the `ActionsGateway` CR, they could otherwise self-select the cluster's least-restrictive Pod Security Admission posture simply by creating a CR. The webhook therefore gates `securityProfile: privileged` behind a label the platform applies to the *namespace* (which the tenant does not own): `actions-gateway.github.com/privileged-profile=allowed`. Absent the label — or with any other value — privileged is ineligible and rejected. (The value is the enum keyword `allowed`, not `true`, to avoid the YAML boolean-coercion footgun.)

**Resolution (platform administrator).** If this tenant is approved to run privileged workers, label the namespace:

```bash
kubectl label namespace <tenant-namespace> actions-gateway.github.com/privileged-profile=allowed
```

Apply it with a trusted (administrator) identity — the GMC must never set it itself, and tenants cannot. Verify:

```bash
kubectl get namespace <tenant-namespace> \
  -o jsonpath='{.metadata.labels.actions-gateway\.github\.com/privileged-profile}'
# Expected: allowed
```

Re-apply the `ActionsGateway`; the create/update is now admitted. To **revoke** eligibility, remove the label (`kubectl label namespace <ns> actions-gateway.github.com/privileged-profile-`); existing CRs already at `privileged` keep running, but any future create or profile change to `privileged` is rejected again.

> On an **update** that raises an existing CR from a stricter profile to `privileged`, the webhook also requires the `actions-gateway.github.com/allow-profile-downgrade: "true"` annotation (anything → `privileged` is a rank downgrade — see [securityProfile Downgrade Rejected](#securityprofile-downgrade-rejected-by-admission-webhook)). Both gates are independent: the namespace label is the platform's eligibility decision, the annotation is the tenant's deliberate relaxation.

If the privileged profile was **not** intended, set `spec.securityProfile` to `baseline` (the default) or `restricted`; neither consults the label.

---

## Tracing Sampler Rejected by Admission

**Symptoms.** A `kubectl apply` / `kubectl edit` / GitOps sync that sets `spec.tracing.sampler` on an `ActionsGateway` is rejected at admission with a CRD validation error like:

```
ActionsGateway.actions-gateway.github.com "<name>" is invalid:
spec.tracing.sampler: Unsupported value: "ratio": supported values:
"always_on", "always_off", "traceidratio", "parentbased_always_on",
"parentbased_always_off", "parentbased_traceidratio"
```

**Likely cause.** `spec.tracing.sampler` is a fixed enum mapping to the OpenTelemetry SDK's built-in samplers (it is forwarded verbatim as `OTEL_TRACES_SAMPLER`). A value outside that set — a typo, or one of the SDK's externally-configured samplers (`jaeger_remote`, `xray`) that this field intentionally does not expose — is rejected by the CRD schema before the object is stored.

**Resolution.**
- Pick a supported value. For probabilistic sampling use `parentbased_traceidratio` with `spec.tracing.samplerArg: "0.1"` (10%); for all/no traces use `parentbased_always_on` / `always_off`.
- Leave `sampler` unset to use the SDK default (`parentbased_always_on`).
- To *disable* tracing entirely, remove `spec.tracing.endpoint` (an empty endpoint emits no `OTEL_*` env and the AGC keeps its no-op tracer) — the sampler value is irrelevant when no endpoint is set.

See [observability — enabling tracing on GMC-managed AGCs](observability.md#enabling-tracing-on-gmc-managed-agcs) for the full field list.

---

## ActionsGateway Rejected: Missing or Malformed `gitHubURL`

**Symptoms.** A `kubectl apply` / `kubectl edit` / GitOps sync of an `ActionsGateway` is rejected at admission with either a CRD-schema error:

```
ActionsGateway.actions-gateway.github.com "<name>" is invalid:
spec.gitHubURL: Required value
```

```
ActionsGateway.actions-gateway.github.com "<name>" is invalid:
spec.gitHubURL in body should match '^https://'
```

or a validating-webhook error:

```
admission webhook "vactionsgateway-v1alpha1.kb.io" denied the request:
gitHubURL must include an organization, enterprise, or owner/repo path segment (got "https://github.com")
```

**Likely cause.** `spec.gitHubURL` is a **required** field — the GitHub org, enterprise, or repository URL the gateway's runners register against. There is no default: a gateway with no URL has nothing to register against. The CRD enforces a non-empty `https://` value; the GMC validating webhook additionally requires a parseable URL with the https scheme, a host, and at least one path segment (the org/enterprise/owner). Common misses: the field omitted entirely, an `http://` (non-TLS) URL, or a bare host (`https://github.com`) with no org.

**Resolution.**
- Set `spec.gitHubURL` to an org URL (`https://github.com/my-org`), a single-repo URL (`https://github.com/my-org/my-repo`), or your GitHub Enterprise Server URL (`https://ghes.example.com/my-org`).
- It must use `https://` and name the org/enterprise/owner — and must match where the App in `spec.gitHubAppRef` is installed.
- Setting it through the Helm chart's sample CR? Use the `sampleGateway.gitHubURL` value. See [tenant-onboarding — Step 2](tenant-onboarding.md#step-2-create-the-actionsgateway-resource).

---

## Worker-Pod Lifecycle Duration Rejected by Admission

**Symptoms.** Applying a `RunnerGroup` (or an `ActionsGateway` whose `runnerGroups[]` entry sets the field) is rejected at admission with one of:

```
The RunnerGroup "..." is invalid: spec: Invalid value: ...:
completedPodTTL must not be negative
```

```
The RunnerGroup "..." is invalid: spec: Invalid value: ...:
pendingPodDeadline must be at least 1s
```

**Likely cause.** CRD CEL validation on the two worker-pod lifecycle knobs: `completedPodTTL` accepts any non-negative duration (`"0s"` means delete worker pods immediately on completion), while `pendingPodDeadline` must be at least `1s` — a zero deadline would reap every worker pod the instant it was admitted, and there is deliberately no way to disable the deadline (an unbounded Pending pod permanently leaks a concurrency-ceiling slot).

**Resolution.**
- Use a non-negative Go duration string for `completedPodTTL` (`"0s"`, `"5m"`, `"1h"`).
- Use a duration of `1s` or more for `pendingPodDeadline`; to effectively park the deadline while debugging a scheduling issue, set it large (e.g. `"24h"`) rather than zero.
- Omit either field to get the defaults (`5m` retention, `10m` deadline).

---

## Worker Pod Crashes With `configuredSettings` ArgumentNullException

**Symptoms.** Worker pod reaches `Running`, the entrypoint wrapper logs `payload loaded` and starts Runner.Worker, but Runner.Worker exits non-zero almost immediately with a stack trace containing `System.ArgumentNullException: Value cannot be null. (Parameter 'configuredSettings')` originating from `Runner.Common.ConfigurationStore.GetSettings()`. The job is never reported back to GitHub.

**Likely causes.**
- The agent Secret was created before Q5a shipped and is missing the `encodedJITConfig` key; the AGC reconciled forward but the per-job Secret has no `jitconfig` key for the wrapper to materialize.
- A custom registrar (non-GitHub) returns an `AgentCredentials` value without `EncodedJITConfig` populated.
- The runner home directory inside the worker container is not `/home/runner` (custom image), but `RUNNER_HOME_DIR` was not overridden in the pod template — the wrapper writes the files to the wrong location and Runner.Worker reads from `$HOME`.

**Diagnostics.**

```sh
# 1. Inspect the agent Secret. encodedJITConfig must be present and non-empty.
kubectl get secret -n <agc-namespace> -l actions-gateway/runner-group=<group>,actions-gateway/agent-index -o jsonpath='{.items[*].data.encodedJITConfig}' | base64 -d | head -c 32; echo

# 2. Inspect the per-job worker Secret while a job is in flight. The jitconfig
#    key must be present.
kubectl get secret -n <tenant-namespace> -l actions-gateway/runner-group=<group> -o name | grep '^secret/job-' | head -1 | xargs -I{} kubectl get {} -n <tenant-namespace> -o jsonpath='{.data.jitconfig}' | base64 -d | head -c 32; echo

# 3. Confirm the wrapper materialized the files. From a debug sidecar or by
#    exec'ing into a running worker pod, list /home/runner:
kubectl exec -n <tenant-namespace> <pod> -c runner -- ls -la /home/runner/.runner /home/runner/.credentials /home/runner/.credentials_rsaparams
```

**Resolution.**
- If the agent Secret is missing `encodedJITConfig`: scale the agent pool to zero (`maxListeners: 0` on the RunnerGroup), wait for Secrets to be deleted, then scale back up. New agents will be registered via `generate-jitconfig` and carry the blob. An agent whose session is in flight is not torn down mid-job — its Secret is deleted on a later reconcile once the session completes (the controller logs `skipping scale-down delete of in-use agent`), so wait for active jobs to drain before expecting the count to reach zero.
- If the worker image puts `$HOME` elsewhere: set `RUNNER_HOME_DIR` on the runner container env via the RunnerGroup `podTemplate`.
- If a custom registrar is in use: ensure it populates `AgentCredentials.EncodedJITConfig` with the raw blob from GitHub's response (the wrapper only knows how to decode that exact format).

---

## `kubectl apply ActionsGateway` Times Out On Webhook During GMC Rollout

**Symptoms.** Right after a GMC rolling update (image bump, env-var change, leader transition), the next `kubectl apply` of an `ActionsGateway` CR fails with:

```
Internal error occurred: failed calling webhook
"vactionsgateway-v1alpha1.kb.io": failed to call webhook:
Post "https://gmc-webhook-service.gmc-system.svc:443/...?timeout=10s":
context deadline exceeded
```

The webhook recovers seconds later; the same `kubectl apply` succeeds on retry. Common pattern in CI / e2e suites that change GMC env vars then immediately apply a CR.

**Likely causes.**
- Running a GMC image built before the readyz-gates-webhook fix landed (commit `0eaa30e`). The default Kubebuilder scaffold registers `mgr.AddReadyzCheck("readyz", healthz.Ping)`, which returns OK as soon as the manager process is up — *before* the webhook listener on port 9443 is bound. The new pod is briefly added to the `gmc-webhook-service` endpoints in a not-yet-serving state.
- A custom probe override that replaces `/readyz` with a cheap liveness check.

**Diagnostics.**

```sh
# 1. Probe the GMC's /readyz directly. With the fix, output should include
#    "[+]readyz ok" AND "[+]webhook ok". Without the fix, only "[+]readyz ok".
kubectl run dbg-readyz --image=alpine --rm -i --restart=Never --quiet --command -- \
  sh -c "apk add --no-cache curl >/dev/null 2>&1; \
         curl -s http://$(kubectl get pod -n gmc-system -l control-plane=controller-manager -o jsonpath='{.items[0].status.podIP}'):8081/readyz?verbose"

# 2. Confirm the deployment is rolling. If yes, wait for it to settle before
#    retrying apply.
kubectl rollout status deployment/gmc-controller-manager -n gmc-system --timeout=2m
```

**Resolution.**
- Upgrade the GMC image to one built from commit `0eaa30e` or later — the readyz check now waits for the webhook server's `StartedChecker()`.
- Until the upgrade is in place, retry the failing `kubectl apply` after 5–10 seconds.

---

## Worker `HTTPS_PROXY` Returns `connection refused` During Proxy Rollout

**Symptoms.** Worker pods (or `kubectl exec` debug curls from a workload-labeled pod) intermittently fail with `connect: connection refused` against the per-tenant proxy `:8080` immediately after a proxy `Deployment` rollout, scale-up, or HPA event. The proxy pods report `READY 1/1` and `/healthz` returns 200.

**Likely causes.**
- Running a proxy image built before the proxy `/readyz` gate landed. The pre-fix proxy bound the health server on `:8081` in parallel with the CONNECT server on `:8080`. The kubelet observed `/healthz` returning 200 and added the pod IP to the proxy `Service` EndpointSlice before the CONNECT serve goroutine had bound the kernel socket. Worker pods racing the rollout connected to the new pod IP via `Service` DNS and got `ECONNREFUSED`.
- A custom probe override that points the GMC-managed proxy `Deployment`'s readinessProbe at `/healthz` instead of `/readyz` (e.g. an out-of-band `kubectl edit deploy`). The GMC reconciler overwrites the probe back to `/readyz` on the next reconcile, but until then the regression is live.

**Diagnostics.**

```sh
# 1. Confirm the proxy Deployment's readinessProbe path. Should be /readyz.
kubectl get deploy -n <tenant-namespace> actions-gateway-proxy \
  -o jsonpath='{.spec.template.spec.containers[0].readinessProbe.httpGet.path}{"\n"}'

# 2. Probe /readyz directly from a workload-labeled debug pod (the proxy
#    NetworkPolicy denies ingress from unlabeled pods).
kubectl run dbg-readyz --rm -i --restart=Never --quiet \
  --labels='actions-gateway/component=workload' \
  --image=alpine --command -- \
  sh -c "apk add --no-cache curl >/dev/null 2>&1; \
         curl -sv http://actions-gateway-proxy.<tenant-namespace>.svc:8081/readyz"

# 3. From the same debug pod, confirm the CONNECT port accepts TCP. A 200 on
#    /readyz paired with a refused TCP dial would be a Q42 regression.
kubectl run dbg-connect --rm -i --restart=Never --quiet \
  --labels='actions-gateway/component=workload' \
  --image=alpine --command -- \
  sh -c "apk add --no-cache busybox-extras >/dev/null 2>&1; \
         nc -zv actions-gateway-proxy.<tenant-namespace>.svc 8080"
```

**Resolution.**
- Upgrade the proxy image to one built with the `/readyz` gate. The handler returns 503 until both listeners are bound (`cmd/proxy/proxy.go` — `handleReadyz`).
- If a custom override changed the readinessProbe path back to `/healthz`, remove it. GMC re-applies the canonical `Deployment` on its next reconcile, so the regression window closes within a few seconds.

`/healthz` remains the liveness probe (always 200 if the process is up). `/readyz` is the readiness gate — kubelet keeps the pod out of the Service EndpointSlice until both `:8080` and `:8081` are bound.

---

## Prometheus Not Scraping Proxy or AGC Metrics

**Symptom.** The proxy and AGC `/metrics` endpoints (both on `:8081`) return no
data in Prometheus, or scrape targets show as `down` with a connection
timeout/refused — despite the pods being healthy.

**Cause.** Each tenant namespace runs under a default-deny ingress posture. The
GMC's per-tenant NetworkPolicies admit `:8081` ingress *only* from namespaces
labelled `metrics: enabled` (the same convention the GMC's own
`allow-metrics-traffic` NetworkPolicy uses). If the namespace your Prometheus
runs in is not labelled, its scrapes are dropped. Kubelet liveness/readiness
probes are unaffected — they originate from the node, which every supported CNI
exempts from NetworkPolicy enforcement.

```bash
# 1. Confirm the monitoring namespace carries the scrape label.
kubectl get ns <prometheus-namespace> -o jsonpath='{.metadata.labels.metrics}{"\n"}'
# Expected: enabled

# 2. Inspect the per-tenant NP ingress rules — each should list an 8081 rule
#    whose `from` is a namespaceSelector on metrics=enabled.
kubectl get networkpolicy -n <tenant-namespace> \
  actions-gateway-proxy actions-gateway-controller \
  -o jsonpath='{range .items[*]}{.metadata.name}: {.spec.ingress}{"\n"}{end}'

# 3. Reproduce a scrape from a pod in the monitoring namespace (allowed) and an
#    unlabelled namespace (denied) to confirm the policy, not the listener.
kubectl run dbg-scrape --rm -i --restart=Never --quiet \
  -n <prometheus-namespace> --image=curlimages/curl --command -- \
  curl -sS --max-time 5 http://actions-gateway-proxy.<tenant-namespace>.svc:8081/metrics | head
```

**Resolution.**
- Label the namespace your Prometheus runs in: `kubectl label ns <prometheus-namespace> metrics=enabled`.
- The proxy and AGC `/metrics` endpoints are unauthenticated plain HTTP; the
  NetworkPolicy namespace selector is the only access control. Keep the
  `metrics: enabled` label off namespaces that should not see per-tenant
  traffic-volume metrics.

**Same label gates the GMC manager metrics.** Since the §E manifest-defaults
work, the GMC install ships the manager NetworkPolicy enabled by default, which
flips the controller-manager pod to default-deny ingress and admits `:8443`
`/metrics` only from `metrics: enabled` namespaces. If the GMC manager scrape
target is `down`, apply the same label to the Prometheus namespace:
`kubectl label ns <prometheus-namespace> metrics=enabled`. The validating-webhook
port (`9443`) is re-allowed from any source by design (the apiserver caller is
not a labeled pod), so admission is unaffected by this label.

---

## Proxy Replica Stuck Pending After Enabling HA Defaults

**Symptom.** One of the two proxy replicas is `Pending`; `kubectl describe pod`
shows `didn't match pod anti-affinity rules` / `node(s) didn't match pod
anti-affinity`. The proxy Deployment never reaches full availability.

**Cause.** The proxy pool uses **required** pod anti-affinity on
`kubernetes.io/hostname` so replicas land on distinct nodes (a single node
failure must never drop the whole tenant's egress pool, and co-located replicas
defeat the PodDisruptionBudget). With the default `proxy.minReplicas: 2`, the
scheduler needs **at least two schedulable nodes**. On a single-node dev/kind
cluster (e.g. `test/kind-config-1worker.yaml`, where the control-plane is
tainted and only one worker is schedulable) the second replica can never place.

**Resolution.**
- Production: ensure the cluster has at least `proxy.minReplicas` schedulable
  nodes for the proxy pods (the default kind config ships two workers).
- Single-node dev clusters: set `spec.proxy.minReplicas: 1` on the
  `ActionsGateway` to run a single proxy replica.

---

← [Back to Operations](.)
