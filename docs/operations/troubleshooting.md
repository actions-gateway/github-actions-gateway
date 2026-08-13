# Troubleshooting Guide

> **Audience:** Platform engineer, Tenant operator

Each section below covers a specific failure mode: symptoms, likely cause, diagnostic commands, and resolution steps.

---

## Table of Contents

- [How to Validate a Fresh Deployment](#how-to-validate-a-fresh-deployment)
- [Helm Render Fails: gmc.image Must Be Pinned by Digest](#helm-render-fails-gmcimage-must-be-pinned-by-digest)
- [GMC Pods Rejected: insufficient quota to match these scopes (PriorityClass)](#gmc-pods-rejected-insufficient-quota-to-match-these-scopes-priorityclass)
- [Every RunnerGroup / RunnerSet Write Denied: no params found for policy binding](#every-runnergroup--runnerset-write-denied-no-params-found-for-policy-binding)
- [GMC Not Provisioning Tenant Resources](#gmc-not-provisioning-tenant-resources)
- [`kubectl rollout restart` of a Managed Deployment Reports Success but Nothing Restarts](#kubectl-rollout-restart-of-a-managed-deployment-reports-success-but-nothing-restarts)
- [ActionsGateway Reports RunnerGroupsDegraded](#actionsgateway-reports-runnergroupsdegraded)
- [Runners Never Appear Online — AGC `unknown authority` Through the Egress Proxy](#runners-never-appear-online--agc-unknown-authority-through-the-egress-proxy)
- [RunnerGroup Reports WorkersUnschedulable](#runnergroup-reports-workersunschedulable)
- [RunnerSet Reports WorkerCapacityDeclined (the Gateway Stopped Claiming Jobs)](#runnerset-reports-workercapacitydeclined-the-gateway-stopped-claiming-jobs)
- [Worker / Proxy / AGC Pods Rejected by a Cluster Policy Engine](#worker--proxy--agc-pods-rejected-by-a-cluster-policy-engine)
- [ActionsGateway Reports EgressRulesStale](#actionsgateway-reports-egressrulesstale)
- [A GHES Tenant's Traffic Never Reaches the Appliance](#a-ghes-tenants-traffic-never-reaches-the-appliance)
- [A GHES Appliance's Certificate Is Not Trusted](#a-ghes-appliances-certificate-is-not-trusted)
- [Tenant Namespace Missing the Managed-Tenant Marker Label](#tenant-namespace-missing-the-managed-tenant-marker-label)
- [ActionsGateway Stuck Deleting (Teardown Blocked on a Failing Delete)](#actionsgateway-stuck-deleting-teardown-blocked-on-a-failing-delete)
- [Tenant Namespace Stuck Terminating on agentpool-cleanup Finalizers](#tenant-namespace-stuck-terminating-on-agentpool-cleanup-finalizers)
- [Tenant Namespace Stuck Terminating After Narrowing the PriorityClass Allowlist](#tenant-namespace-stuck-terminating-after-narrowing-the-priorityclass-allowlist)
- [AGC CrashLoopBackOff or Not Acquiring Jobs](#agc-crashloopbackoff-or-not-acquiring-jobs)
- [ScaleSet RunnerSet Stuck Not Ready: `ScaleSetListenerStartFailed` Naming the Guard ConfigMap](#scaleset-runnerset-stuck-not-ready-scalesetlistenerstartfailed-naming-the-guard-configmap)
- [AGC Exits at Startup: GATEWAY_NAME Set but the v2 RunnerSet CRD Is Missing](#agc-exits-at-startup-gateway_name-set-but-the-v2-runnerset-crd-is-missing)
- [AGC Exits at Startup: Proxy CA Cert Present but Unreadable](#agc-exits-at-startup-proxy-ca-cert-present-but-unreadable)
- [RunnerGroup ActiveSessions Exceeds maxListeners](#runnergroup-activesessions-exceeds-maxlisteners)
- [RunnerGroup Stops Serving Jobs With Stale Ready=True](#runnergroup-stops-serving-jobs-with-stale-readytrue)
- [Orphaned RunnerGroup After Removing It From the Spec](#orphaned-runnergroup-after-removing-it-from-the-spec)
- [Proxy NetworkPolicy Has an Empty GitHub Allowlist](#proxy-networkpolicy-has-an-empty-github-allowlist)
- ["Runner Lost Communication" and No Worker Pod Was Ever Created](#runner-lost-communication-and-no-worker-pod-was-ever-created)
- [Worker Pods Stuck Pending](#worker-pods-stuck-pending)
- [Worker Pod Reaped While Pending (WorkerPodStuckPending)](#worker-pod-reaped-while-pending-workerpodstuckpending)
- [Worker Pod Reaped While Pending After Its Job Completed (WorkerPodCompletedPending)](#worker-pod-reaped-while-pending-after-its-job-completed-workerpodcompletedpending)
- [Worker Pod Reaped While Running (WorkerPodOrphanedRunning)](#worker-pod-reaped-while-running-workerpodorphanedrunning)
- [Workers Left Behind by an AGC That Was Down](#workers-left-behind-by-an-agc-that-was-down)
- [Worker Killed by the Lifetime Cap (WorkerPodLifetimeExceeded)](#worker-killed-by-the-lifetime-cap-workerpodlifetimeexceeded)
- [Worker Pods Reaped on Gateway Teardown (WorkerPodsReapedOnGatewayTeardown)](#worker-pods-reaped-on-gateway-teardown-workerpodsreapedongatewayteardown)
- [ActionsGateway Deletion Hangs on WaitingForWorkerDrain](#actionsgateway-deletion-hangs-on-waitingforworkerdrain)
- [Scale-Set Job Stranded by a Stale Runner Record (Runner-Name 409)](#scale-set-job-stranded-by-a-stale-runner-record-runner-name-409)
- [Worker Pods Stuck Running After the Job Finished (Mesh Sidecar)](#worker-pods-stuck-running-after-the-job-finished-mesh-sidecar)
- [RunnerSet Reports PossibleReapBlockingSidecar (Build/DinD Sidecar in the Template)](#runnerset-reports-possiblereapblockingsidecar-builddind-sidecar-in-the-template)
- [Worker Image Runner Version](#worker-image-runner-version)
- [Job-Lifecycle Events on a RunnerGroup / RunnerSet](#job-lifecycle-events-on-a-runnergroup--runnerset)
- [Proxy Pool Not Scaling](#proxy-pool-not-scaling)
- [Proxy Tunnel Closed Mid-Stream — Idle or Lifetime Cap](#proxy-tunnel-closed-mid-stream--idle-or-lifetime-cap)
- [Metrics scrape returns a TLS / connection error](#metrics-scrape-returns-a-tls--connection-error)
- [RateLimited Condition on ActionsGateway](#ratelimited-condition-on-actionsgateway)
- [GitHub App Secret Misconfiguration](#github-app-secret-misconfiguration)
- [Token Refresh Errors Spiking](#token-refresh-errors-spiking)
- [RenewJob Failures Rising](#renewjob-failures-rising)
- [Sessions Stuck in 401/EOF GetMessage Loops (Tenant Throughput Decays to Zero)](#sessions-stuck-in-401eof-getmessage-loops-tenant-throughput-decays-to-zero)
- [Concurrent Job Burst Serializes to ~1 Worker (Recycle Blocked on a Still-Running Runner)](#concurrent-job-burst-serializes-to-1-worker-recycle-blocked-on-a-still-running-runner)
- [Network Connectivity Failures](#network-connectivity-failures)
- [AGC Crash-Loops Dialling the API Server Through the Egress Proxy](#agc-crash-loops-dialling-the-api-server-through-the-egress-proxy)
- [AGC Cannot Reach the Kubernetes API Server (NetworkPolicy + post-DNAT port mismatch)](#agc-cannot-reach-the-kubernetes-api-server-networkpolicy--post-dnat-port-mismatch)
- [DNS Times Out Under the Egress NetworkPolicy (GKE Dataplane V2 / NodeLocal DNSCache)](#dns-times-out-under-the-egress-networkpolicy-gke-dataplane-v2--nodelocal-dnscache)
- [Worker Pod Runner.Worker Fails TLS Handshake With UntrustedRoot](#worker-pod-runnerworker-fails-tls-handshake-with-untrustedroot)
- [Which Disruptions Auto-Re-Run a Job (and Which Never Do)](#which-disruptions-auto-re-run-a-job-and-which-never-do)
- [Evicted Worker Pods Exhausting Retry Budget](#evicted-worker-pods-exhausting-retry-budget)
- [Draining a Worker Auto-Re-Runs the Jobs It Interrupts](#draining-a-worker-auto-re-runs-the-jobs-it-interrupts)
- [A Preempted Worker's Job Is Not Re-Run](#a-preempted-workers-job-is-not-re-run)
- [Cancelling a Run Does Not Stop Its Worker Pod](#cancelling-a-run-does-not-stop-its-worker-pod)
- [Evicted Scale-Set Jobs Are Not Re-Run Automatically](#evicted-scale-set-jobs-are-not-re-run-automatically)
- [Terminated Worker Pod Never Reports Its Job (Job Hangs on GitHub Until the Lock Lapses)](#terminated-worker-pod-never-reports-its-job-job-hangs-on-github-until-the-lock-lapses)
- [Jobs Failing Due to Namespace ResourceQuota Exhaustion](#jobs-failing-due-to-namespace-resourcequota-exhaustion)
- [Jobs Not Being Acquired Despite Queued Work (Capacity Gate Saturated)](#jobs-not-being-acquired-despite-queued-work-capacity-gate-saturated)
- [Worker Pod Fails to Start After Secure-by-Default SecurityContext](#worker-pod-fails-to-start-after-secure-by-default-securitycontext)
- [securityProfile Downgrade Rejected by Admission Webhook](#securityprofile-downgrade-rejected-by-admission-webhook)
- [Second ActionsGateway in a Namespace Rejected (Singleton Guard)](#second-actionsgateway-in-a-namespace-rejected-singleton-guard)
- [`proxy.noProxyCIDRs` Rejected: Entry Would Bypass the Proxy for GitHub](#proxynoproxycidrs-rejected-entry-would-bypass-the-proxy-for-github)
- [Privileged Worker Container Rejected by Admission](#privileged-worker-container-rejected-by-admission)
- [`RunnerTemplate` Rejected: Reserved Pod Field (`v2alpha1`)](#runnertemplate-rejected-reserved-pod-field-v2alpha1)
- [`RunnerSet` Rejected: `acquisitionProtocol` (`v2alpha1`, early-adopter)](#runnerset-rejected-acquisitionprotocol-v2alpha1-early-adopter)
- [`RunnerSet` Rejected: `nodeShare.allocatable` Declares Neither cpu Nor memory](#runnerset-rejected-nodeshareallocatable-declares-neither-cpu-nor-memory)
- [`RunnerSet` Stuck `Ready=False` With a `NotFound` Reason (`v2alpha1`)](#runnerset-stuck-readyfalse-with-a-notfound-reason-v2alpha1)
- [`RunnerSet` Stuck `Ready=False` With `RunnerGroupNotFound`](#runnerset-stuck-readyfalse-with-runnergroupnotfound)
- [v2 `ActionsGateway` Stuck `Ready=False` (`CredentialUnavailable` / `ProxyNotFound`)](#v2-actionsgateway-stuck-readyfalse-credentialunavailable--proxynotfound)
- [`AGCAutoscalingUnavailable` — the VPA CRDs are not installed](#agcautoscalingunavailable--the-vpa-crds-are-not-installed)
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
- [Proxy Pool Never Scales Out](#proxy-pool-never-scales-out)

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

**Symptoms.** `helm install`, `helm upgrade`, `helm lint`, or `helm template` of the `actions-gateway` chart fails immediately with (here for `gmc`; the message names whichever image is unpinned — `gmc`, `agc`, `proxy`, or `wrapper`):

```
Error: execution error at (actions-gateway/templates/deployment.yaml:...):
gmc.image must be pinned by digest: set gmc.image.digest=sha256:<64 hex digits>
(see docs/operations/install.md, "Pin images by digest").
DEV/TEST ONLY: set allowFloatingImageTags=true to allow a floating tag.
```

**Cause.** One of the four image digests (`gmc`, `agc`, `proxy`, or `wrapper`) is empty in the release values.
The chart enforces digest pinning of **all four** images at render time (secure by default): an empty digest must never silently fall back to a mutable `:latest` tag — for the GMC's own image nothing at runtime validates it, and for the AGC/proxy/wrapper images an unpinned tag would otherwise only surface later as a GMC crash-loop.
Common ways to get here:

- A fresh install without `--set <image>.image.digest=sha256:<…>` for one of the four (a forgotten `wrapper.image.digest` is the most common).
- A `helm upgrade` with a values file (or `--reset-values`) that omits a digest.
  (`--reset-then-reuse-values` carries the previously pinned digests forward.)
- Offline rendering (`helm template` / `helm lint`) without supplying all four digests.

**Resolution.**

- **Production:** pin the digest published for the release you are installing (see [release.md](release.md) for where digests are recorded):

  ```sh
  helm upgrade --install gag charts/actions-gateway \
    --namespace gmc-system \
    --set gmc.image.digest=sha256:<gmc> \
    --set agc.image.digest=sha256:<agc> \
    --set proxy.image.digest=sha256:<proxy> \
    --set wrapper.image.digest=sha256:<wrapper>
  ```

- **Dev/test only:** `--set allowFloatingImageTags=true` lets all four images render from a floating tag *and* disables the GMC's startup digest check on the AGC/proxy/wrapper images.
  Never use it in production.
- **Offline rendering:** any well-formed digest satisfies the check, e.g. `--set-string gmc.image.digest=sha256:1111111111111111111111111111111111111111111111111111111111111111` (repeat for `agc`/`proxy`/`wrapper`).

All four images are validated by the chart **at render time**.
The AGC/proxy/wrapper images are *additionally* re-checked by the GMC **at startup** (a floating tag there crash-loops the GMC — see [install.md § Pin images by digest](install.md#pin-images-by-digest)), a second layer that also covers non-chart deployments.

---

## GMC Pods Rejected: `insufficient quota to match these scopes` (PriorityClass)

**Symptoms.** After `helm install`, the GMC Deployment never reaches Ready.
There are no GMC pods, and the ReplicaSet emits a `FailedCreate` event:

```
kubectl describe replicaset -n gmc-system -l app.kubernetes.io/name=gmc
# Events:
#   Warning  FailedCreate  ...  Error creating: pods "gmc-controller-manager-..." is forbidden:
#   insufficient quota to match these scopes:
#   [{PriorityClass In [system-node-critical system-cluster-critical]}]
```

**Cause.** The cluster's API server enables the restricted `PriorityClass` admission config (GKE Standard does this by default), which permits `system-node-critical` / `system-cluster-critical` pods **only** in a namespace carrying a `ResourceQuota` whose `scopeSelector` matches those classes.
The GMC runs with `priorityClassName: system-cluster-critical` by default — a deliberate secure default that protects the control plane from eviction — so without a permitting quota in the install namespace, the apiserver rejects every GMC pod.

**Resolution.** The chart ships the permitting quota by default, so this should not occur on a current chart.
If you hit it:

- **Confirm the quota is enabled.** The chart renders `<namePrefix>-critical-pods` (default `gmc-critical-pods`) when `systemCriticalPriorityQuota.enabled=true` (the default) and `priorityClassName` is a system-critical class:

  ```sh
  kubectl get resourcequota -n gmc-system gmc-critical-pods
  ```

  If it is missing, you likely installed with `--set systemCriticalPriorityQuota.enabled=false`.
  Re-run the install/upgrade without that override (it defaults to `true`).
  See [install.md § GKE and other restricted-PriorityClass clusters](install.md#gke-and-other-restricted-priorityclass-clusters).
- **Do not** work around the rejection by clearing `priorityClassName` — that removes the GMC's eviction protection (a security regression).
  Keep `system-cluster-critical` and let the quota permit it.
- **If you manage the quota out-of-band** (e.g. a cluster-wide policy), ensure it exists in the install namespace and its `scopeSelector` matches the system-critical classes before installing.

---

## `helm upgrade` Fails With `nil pointer evaluating interface {}.FIELD`

**Symptoms.** An upgrade fails while rendering a template you did not touch, naming a values key you may never have set:

```text
Error: UPGRADE FAILED: actions-gateway/templates/vpa.yaml:1:14
  executing "actions-gateway/templates/vpa.yaml" at <.Values.vpa.enabled>:
    nil pointer evaluating interface {}.enabled
```

Nothing is changed in the cluster — the render fails before anything is applied.

**Cause.** The upgrade used `--reuse-values`.
That flag replays the *old* release's values over the *old* chart's defaults; it never consults the new chart's `values.yaml`.
Any key introduced after your release was created is therefore absent, and a template reading a field under it dereferences nil.
The named file is just the first template to touch such a key — it is not the problem.

**Fix.** Re-run with `--reset-then-reuse-values` (Helm ≥ 3.14), which starts from the new chart's defaults and layers your release's values on top:

```sh
helm upgrade gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version <new-chart-version> --namespace gmc-system --reset-then-reuse-values
```

Everything you set is preserved — verify with `helm get values gag -n gmc-system`.
Use this flag for every upgrade of this chart; see [upgrade.md](upgrade.md#gmc-install-and-upgrade-via-helm-recommended).

**Why the chart does not just tolerate the missing block.** It could render a missing key as unset, and the upgrade would succeed — but for a security-relevant block (`admissionPolicy`, `networkPolicy`) that silently *disables* a guard on upgrade, with no error anywhere.
A failed render is the safe direction, so this error is deliberate rather than a bug to route around.

---

## Every `RunnerGroup` / `RunnerSet` Write Denied: `no params found for policy binding`

**Symptoms.** Every write to a `runnergroups`, `runnersets`, or `runnertemplates` object is rejected — including ones that name no `priorityClassName` at all — while the parameter object plainly exists at the name the binding references:

```
kubectl apply -f runnergroup.yaml
# Error from server: ... ValidatingAdmissionPolicy 'gmc-priorityclass-allowlist-guard'
# with binding 'gmc-priorityclass-allowlist-guard-binding' denied request:
# failed to configure binding: no params found for policy binding with `Deny`
# parameterNotFoundAction

kubectl get priorityclassallowlist gmc-priorityclass-allowlist   # it is right there
```

The GMC surfaces it as provisioning failures on every gateway.

**First, rule out the benign case.** The same message appears for a second or two during any reinstall: `helm uninstall` removes the parameter object and the reinstall recreates it, and the guard correctly fails closed until the apiserver observes the new one.
That clears on its own.

**If it persists, check what your release uses as the parameter.**

```sh
kubectl get validatingadmissionpolicy gmc-priorityclass-allowlist-guard \
  -o jsonpath='{.spec.paramKind}{"\n"}'
```

- `{"apiVersion":"actions-gateway.com/v2beta1","kind":"PriorityClassAllowlist"}` — current.
  The apiserver defect below does not apply.
  A persistent denial means the object really is missing or the binding names the wrong one: compare `kubectl get validatingadmissionpolicybinding gmc-priorityclass-allowlist-guard-binding -o jsonpath='{.spec.paramRef.name}'` against `kubectl get priorityclassallowlist`, and recreate the object (or re-run the chart upgrade) to recover.
- `{"apiVersion":"v1","kind":"ConfigMap"}` — a release predating Q492, exposed to the defect below.

### Cause on pre-Q492 releases (Q444, kube-apiserver defect)

The apiserver caches one parameter informer per `paramKind`, shared by every policy naming it.
That informer is torn down as soon as no `ValidatingAdmissionPolicyBinding` in the cluster names the `paramKind` any more — and because a ConfigMap is a *core* type, the teardown stops a **shared** informer that the apiserver will never restart.
The state belongs to the process, so:

- Deleting the binding — which is what `helm uninstall` does — is the trigger.
  The guard has exactly one binding, so removing it empties the set.
  One second is enough.
- Afterwards the informer's cache is **frozen**, not emptied.
  ConfigMaps created later are invisible, which is why even a *fresh* install fails with the binding pointing at a ConfigMap that demonstrably exists.
- `helm upgrade` never removes the binding and is safe.

**The quieter failure mode.** If the frozen cache happens to still hold the old ConfigMap, there is no error at all — the policy keeps validating against an allowlist **that no longer exists in the cluster**.
Edits to the allowlist appear to apply and silently do nothing.
After any event that could have tripped this, confirm the guard sees your current allowlist rather than assuming it does: change the allowlist and check that a `RunnerGroup` naming a newly removed class is actually rejected.

Observed on Kubernetes 1.35.5 and 1.36.1.
Upstream: [kubernetes/kubernetes#130887](https://github.com/kubernetes/kubernetes/issues/130887).
Full mechanism and the reproducer: [`q444-vap-param-resolution.md`](../plan/archive/q444-vap-param-resolution.md).

**Resolution.**

- **Upgrade the release.** The parameter is now a cluster-scoped `PriorityClassAllowlist` CR, and the apiserver allocates a fresh dynamic informer per context for a CRD `paramKind` — so the transition that breaks a ConfigMap parameter is survivable.
  Migration steps: [upgrade.md](upgrade.md#priorityclass-allowlist-configmap-to-cr).
  Note the upgrade does **not** by itself un-break an apiserver that is already in the broken state for the ConfigMap kind; the breakage is per-kind, so pointing the policy at the new kind is what restores resolution.
- **Restore writes immediately**, if you cannot upgrade right now, by removing the binding — that is what evaluates the policy:

  ```sh
  helm upgrade gag charts/actions-gateway --namespace gmc-system --reset-then-reuse-values \
    --set admissionPolicy.enabled=false
  ```

  Denials stop at once.
  This also disables the PriorityClass backstop, so treat it as mitigation, not a fix — the GMC webhook allowlist still gates the tenant-facing CRs in the meantime.
- **Restarting kube-apiserver** also clears it, and was the only recovery before Q492.
  Straightforward on a self-managed control plane; on EKS/GKE/AKS you cannot restart it directly, and a control-plane version upgrade is usually the only lever that recycles the process.

  Restart the container itself (`crictl stop` on the apiserver container, then confirm its `createdAt` changed). `kubectl delete pod -n kube-system kube-apiserver-…` does **not** work — it recreates the static pod's mirror object while the container keeps running.

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

**Reading the `Degraded` condition.** When a reconcile fails partway through provisioning, the GMC sets `Degraded=True` (reason `ProvisioningFailed`) on the `ActionsGateway` and names the failing step in the message — e.g. `provisioning failed at step "proxy Deployment + Service": ...`.
The reconcile returns immediately on that error, so the other conditions (`ProxyAvailable`, `AGCAvailable`) may be stale; `Degraded` is the authoritative signal of which step is stuck.
It clears (`Degraded=False`, reason `ReconcileSucceeded`) automatically once a reconcile completes all steps.
Read it directly:

```sh
kubectl get actionsgateway -n <namespace> <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="Degraded")]}{.status} {.reason}: {.message}{"\n"}{end}'
```

**Resolution.**
- If the GMC pod is not running, restore it from its Deployment.
- If RBAC is missing, re-run `helm upgrade --install` of the chart (RBAC ships with it).
- If the admission webhook is rejecting the CR, fix the CR spec and re-apply.
- If `Degraded=True`, fix the underlying problem named by the failing step (e.g. a conflicting hand-created resource, missing permission, or exhausted quota) — also cross-check the `controller_runtime_reconcile_errors_total` metric and the full error in the GMC logs.
  The GMC's reconciler retries with backoff and clears `Degraded` on the next successful reconcile.

---

## `kubectl rollout restart` of a Managed Deployment Reports Success but Nothing Restarts

**Symptoms.** `kubectl rollout restart deploy/<agc-or-proxy> -n <namespace>` prints `deployment.apps/... restarted`, and a following `kubectl rollout status` prints `successfully rolled out` within a second.
But the pod is unchanged: same name, same `AGE`, no new ReplicaSet in `kubectl get rs -n <namespace>`, and whatever the restart was meant to clear (a stale in-memory listener, a cached credential) is still there.

**Which Deployments.** Every Deployment the GMC provisions inside a tenant namespace: the AGC control plane (`actions-gateway-controller` on v1, `<gateway>-agc` on v2) and the egress proxy pool.
The GMC's *own* Deployment (`gmc-controller-manager` in `gmc-system`) is not managed by a controller and restarts normally.

**What happened.** `kubectl rollout restart` works by patching a `kubectl.kubernetes.io/restartedAt` annotation onto the Deployment's **pod template** — that changed template is what makes the Deployment controller roll a new ReplicaSet.
On GMC versions without the Q552 fix, the reconciler rebuilt the whole pod template from the `ActionsGateway` / `EgressProxy` spec on every pass, so it reverted the annotation before the rollout began. `kubectl rollout status` then reported the *pre-existing* ReplicaSet — trivially complete — as a successful rollout.

**Resolution.**
- **Upgrade the GMC** to a release carrying the Q552 fix.
  Fixed versions carry an operator-injected `kubectl.kubernetes.io/restartedAt` through reconcile, so `kubectl rollout restart` performs a real rolling update.
  Nothing else on the pod template is tolerated — a hand-edited image, env var, or any other annotation is still reconciled back to the CR, which remains the source of truth.
- **Workaround on an older GMC:** delete the pod and let the Deployment recreate it.

  ```sh
  kubectl delete pod -n <namespace> -l app=actions-gateway-controller
  ```

  This is a hard restart, not a rolling one: the replacement pod is only scheduled after the old one terminates, so the tenant's control plane is down for the pod's startup time.
  The AGC's 60s termination grace period still applies, so in-flight session work drains as it would on a rollout.

**What a working restart still does not do.** Either path replaces the control-plane pod and nothing else.
It deletes no worker pod — see [Workers Left Behind by an AGC That Was Down](#workers-left-behind-by-an-agc-that-was-down) for what actually reclaims those — so a restart is not a remedy for workers that will not drain.
The reaper runs on the live AGC on deadlines measured from each pod, so a fresh one reaps no sooner than the one it replaced.

**Verify either path took effect** — check that the pod is actually new rather than trusting the rollout message:

```sh
kubectl get pods -n <namespace> -l app=actions-gateway-controller \
  -o custom-columns=NAME:.metadata.name,AGE:.metadata.creationTimestamp
```

---

## ActionsGateway Reports RunnerGroupsDegraded

**Symptoms.** `kubectl get actionsgateway` shows a `RunnerGroupsDegraded=True` condition, or the `actions_gateway_runnergroups_degraded` gauge is `1`.
The gateway infrastructure itself (proxy, AGC) may still be `Ready=True` — this condition rolls **child RunnerGroup** health up to the gateway so you don't have to inspect each group individually.

**Cause.** One or more of the gateway's owned `RunnerGroup`s reports an *impairing* condition — `CredentialUnavailable` (the AGC can't obtain an installation token), `Degraded` (an unhealthy/unauthorized listener session), `RunnerVersionTooOld` (the worker image ships a runner below GitHub's enforced minimum, or GitHub rejected the configured version outright; see [Worker Image Runner Version](#worker-image-runner-version)), or `WorkersUnschedulable` (worker pods can't be scheduled).
Advisory capacity/throughput conditions (`WorkerQuotaPressure`/`WorkerQuotaExceeded`, `RateLimited`) are deliberately **not** rolled up here — they have their own signals. `RunnerGroupsDegraded` does **not** gate `Ready`: the gateway can keep serving healthy groups while one is impaired.

**Diagnostics.**

```sh
# Read the rollup — its message names the impaired groups and their tripped conditions.
kubectl get actionsgateway -n <namespace> <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="RunnerGroupsDegraded")]}{.status} {.reason}: {.message}{"\n"}{end}'

# Drill into a named RunnerGroup's own conditions.
kubectl get runnergroup -n <namespace> <runner-group> -o jsonpath='{.status.conditions}' | jq .
```

**Resolution.** Resolve the underlying per-group condition, then the rollup clears automatically on the next reconcile (the GMC watches the owned RunnerGroups):
- `CredentialUnavailable` → see [GitHub App Secret Misconfiguration](#github-app-secret-misconfiguration).
- `Degraded` / `RunnerVersionTooOld` → see [AGC CrashLoopBackOff or Not Acquiring Jobs](#agc-crashloopbackoff-or-not-acquiring-jobs).
- `WorkersUnschedulable` → see [RunnerGroup Reports WorkersUnschedulable](#runnergroup-reports-workersunschedulable).

> **v2 `ActionsGateway`: `RunnerSetsDegraded`.** The v2 `ActionsGateway` reports the same rollup as `RunnerSetsDegraded` (Q304), rolling **child `RunnerSet`** health up to the gateway.
> A set counts as impaired on either of two axes: it is not `Ready` for a non-transient reason — a reference or provisioning failure, which a v2 `RunnerSet` folds into `Ready=False` (anything but the benign startup `NoActiveSessions`) — **or** any of its abnormal-is-True impairing conditions is `True`: `Degraded` (revoked or invalid credentials), `CredentialUnavailable`, `RunnerVersionTooOld`, or `WorkersUnschedulable`.
> The second axis matters because the shared listener pushes `Degraded`/`RunnerVersionTooOld` independently of `Ready` (Q330): a classic set whose sessions are all rejected as unauthorized converges to the benign `Ready=NoActiveSessions` while `Degraded=True` sits on its own condition.
> Advisory throughput signals (`RateLimited`, the `WorkerQuota` ladder) are deliberately excluded so the rollup does not flap on normal load.
> The message names the impaired sets and their tripped signals; a set targeting a *different* gateway is never counted.
> It is advisory (does **not** gate `Ready`) and clears automatically once the children recover (the GMC watches bound `RunnerSet`s).
> Read it with:
>
> ```sh
> kubectl get actionsgateway -n <namespace> <name> \
>   -o jsonpath='{range .status.conditions[?(@.type=="RunnerSetsDegraded")]}{.status} {.reason}: {.message}{"\n"}{end}'
> ```

---

## Runners Never Appear Online — AGC `unknown authority` Through the Egress Proxy

**Symptoms.** A freshly onboarded tenant reaches `Ready=True` with the proxy (`PROXYREADY`) and AGC pods running, but **no runner ever appears** in the repo/org runner list, the `RunnerGroup` shows `ActiveSessions` empty/0, the gateway reports `RunnerGroupsDegraded`, and the AGC log repeats:

```
EnsureAgents failed ... register agent: generate jit config:
Post "https://api.github.com/.../actions/runners/generate-jitconfig":
proxyconnect tcp: tls: failed to verify certificate:
x509: certificate signed by unknown authority
```

The installation-token fetch **succeeds** (`initial token acquired` appears in the log just before the failures), so this is specific to agent **registration**, not credentials — distinguishing it from [GitHub App Secret Misconfiguration](#github-app-secret-misconfiguration).

**Cause.** The AGC routes its own GitHub API traffic through the per-tenant egress proxy, whose serving certificate is the GMC's self-signed per-tenant CA.
In **`v1.1.0-rc.4` and earlier**, the AGC's runner-registration HTTP client was constructed before that proxy CA was added to the process trust store, so it trusted only the system roots and could not complete the TLS handshake to the proxy.
It affects any **proxied** AGC — every `v1alpha1` `ActionsGateway`, and a `v2` `RunnerSet` whenever an `EgressProxy` is attached.
Direct-egress (no proxy) tenants are unaffected (which is why it does not reproduce without a proxy).

**Resolution.** Upgrade the AGC image to a release **after `v1.1.0-rc.4`**, where the registration client is built lazily — after the proxy CA is trusted.
There is no clean configuration workaround on `rc.4` for a proxied tenant; switching the tenant to direct egress sidesteps the bug but gives up the proxy's egress isolation, so prefer the upgrade.

---

## RunnerGroup Reports WorkersUnschedulable

**Symptoms.** `kubectl get runnergroup` shows a `WorkersUnschedulable=True` condition, or the `actions_gateway_workers_unschedulable` gauge is `1` — on a v2 `RunnerSet`, `kubectl get runnerset` and the per-set twin `actions_gateway_runnerset_workers_unschedulable`.
Jobs are acquired but never start; worker pods sit `Pending`.
Each pod the reaper eventually gives up on also emits a `WorkersUnschedulable` Warning event and a `WorkerPodStuckPending` event on the RunnerGroup.

**Cause.** The Kubernetes scheduler cannot place the group's worker pods on any node — `PodScheduled=False` with reason `Unschedulable`.
Typical reasons: no node has enough allocatable CPU/memory for the pod's requests, the pod's `nodeSelector` / affinity matches no node, or every candidate node carries a taint the pod does not tolerate.
The condition trips once a pod has been Pending+Unschedulable for longer than the scheduling grace (half the group's `pendingPodDeadline`), giving an early warning before the reaper deletes the pod at the full deadline.

This is **not** a quota problem: a `ResourceQuota` rejection blocks pod *admission* so the pod is never created — that path is the separate `WorkerQuotaExceeded` condition.
The two never both fire for the same cause.

> **v2 `RunnerSet`.** The same `WorkersUnschedulable` condition is set on a v2 `RunnerSet` (Q303) with identical semantics — swap `runnergroup` for `runnerset` in the commands below.
> Its gauge is the per-set twin `actions_gateway_runnerset_workers_unschedulable` (Q319), keyed on `namespace`/`runner_set` rather than `namespace`/`runner_group`; the v1 `actions_gateway_workers_unschedulable` stays `RunnerGroup`-only, so `actions_gateway_workers_unschedulable == 1 or actions_gateway_runnerset_workers_unschedulable == 1` is the query that covers both while `v1alpha1` is still served.

**Diagnostics.**

```sh
# Read the condition — its message names the stuck pods and the scheduler verdict.
kubectl get runnergroup -n <namespace> <runner-group> \
  -o jsonpath='{range .status.conditions[?(@.type=="WorkersUnschedulable")]}{.status} {.reason}: {.message}{"\n"}{end}'

# Inspect a stuck worker pod's scheduler events for the exact reason.
kubectl describe pod -n <namespace> <worker-pod>   # look for "FailedScheduling"
```

**Resolution.** Match the scheduler verdict to the fix:
- *Insufficient cpu/memory* → add nodes / scale the cluster autoscaler, or lower the worker pod's resource requests in the group's `podTemplate`.
- *node(s) didn't match nodeSelector / affinity* → correct the `podTemplate`'s `nodeSelector`/affinity, or label the intended nodes.
- *node(s) had untolerated taint* → add the matching toleration to the `podTemplate`, or remove the taint from the target nodes.

The condition clears automatically on the next reconcile once a worker pod schedules successfully.

---

## RunnerSet Reports WorkerCapacityDeclined (the Gateway Stopped Claiming Jobs)

**Symptoms.** `kubectl get runnerset` shows a `WorkerCapacityDeclined=True` condition and a `WorkerCapacityDeclined` Warning event.
Jobs sit **queued at GitHub** and are never picked up — no worker pods are being created, and no jobs are being cancelled either.
On the classic tier `actions_gateway_jobs_admission_rejected_total{reason="capacity"}` climbs; on the default `ScaleSet` tier `actions_gateway_scaleset_advertised_capacity` has dropped and `actions_gateway_scaleset_capacity_withheld{reason="capacity"}` holds the remainder of the ceiling.
Those two count the *cost* of the refusal; the gate's own state is `actions_gateway_runnerset_worker_capacity_declined`, which reads `1` under the `reason` label below and is emitted only for a set that opted in — so `max by (namespace, runner_set, reason) (actions_gateway_runnerset_worker_capacity_declined == 1)` is the fleet-wide "which sets are gated, and on what evidence" query.

**Cause.** This is **deliberate, and only ever happens on a set that opted in.** The runner set has `spec.capacityGate.mode: Observe` (Q405, Q406, Q470), and the signal the gate reads is currently saying the cluster cannot place another worker pod of this set's shape.
The gateway is refusing to claim work it cannot run, because each such claim spends a single-use JIT runner record, holds a GitHub job lock until `pendingPodDeadline`, and ends in a **cancelled** workflow run rather than a redelivered one.

Which signal it read follows from the gateway's `spec.clusterCapacity.nodeAutoscaling`, not from anything on the set.
The condition's `reason` names it:

| `reason` | Gateway says | What was observed |
|---|---|---|
| `ScaleUpDeclined` | `nodeAutoscaling: Present` (default) | The cluster autoscaler itself recorded, on a stuck worker pod, that it will **not** add a node for it. The message carries the autoscaler's own per-node-group text. |
| `PodsUnschedulable` | `nodeAutoscaling: Absent` | The scheduler's own verdict — the same `PodScheduled=False`/`Unschedulable` fact behind [`WorkersUnschedulable`](#runnergroup-reports-workersunschedulable). |
| `AwaitingProbe` | either | The stuck pods that produced the verdict were reaped at `pendingPodDeadline`, and nothing has yet shown that capacity returned — the decline is **retained** (Q512). Intake is limited to **one probe job** per deadline window, not closed; the message carries the reaped verdict. Clears when a worker pod schedules. |
| `CapacityAvailable` (status `False`) | either | The gate is engaged and is **not** refusing intake. |
| `GateModeUnsupported` (status `False`) | — | This AGC does not implement the mode the set selected, so no rung is evaluated. See [below](#the-mode-is-reported-as-unsupported). |

A set with no `capacityGate` (the default) never carries this condition at all — its absence is not a failure to report, it means the set did not opt in.

> **The gate throttles, it does not seal.** When the reaper deletes the stuck pod at `pendingPodDeadline`, the condition does not clear — it latches as `AwaitingProbe` and admits exactly **one probe job**; if capacity is still missing, that job's pod trips the live verdict again, and if it schedules the gate clears completely.
> Expect roughly one claim per deadline window, not zero — a `RunnerSet` that never claims anything again has a different problem.
> On an idle gated set whose shape stays unplaceable, `AwaitingProbe` can persist `True` indefinitely; that is truthful, and harmless until jobs arrive.
>
> A latched set is invisible to the scheduler-side signals: the pod that produced the verdict is gone, so `WorkersUnschedulable` — and its gauge — is back to `0` while intake is still throttled. `worker_capacity_declined{reason="AwaitingProbe"}` is the series that stays `1`, which is why the gauge carries the reason.

**What alerts on this.** Two ticket-severity rules cover the gate, one per acquisition tier: [`ActionsGatewayCapacityGateRejectingJobs`](runbook.md#actionsgatewaycapacitygaterejectingjobs) on the classic tier and [`ActionsGatewayScaleSetCapacityWithheld`](runbook.md#actionsgatewayscalesetcapacitywithheld) on the default `ScaleSet` one.
Both watch the *cost* series above rather than `worker_capacity_declined` itself, and the scale-set rule additionally requires the set to have been assigned work in the last hour.
That is deliberate: the gauge alone cannot tell a gate that is costing throughput from the harmless latched state this section describes, so alerting on it directly would open a ticket against an idle set doing exactly what it should.

**Diagnostics.**

```sh
# Is the gate on, and is it currently closed?
kubectl get runnerset -n <namespace> <runner-set> \
  -o jsonpath='{.spec.capacityGate.mode}{"\n"}{range .status.conditions[?(@.type=="WorkerCapacityDeclined")]}{.status} {.reason}: {.message}{"\n"}{end}'
```

```sh
# Which signal is it reading? This is the gateway's answer, not the set's.
kubectl get actionsgateway -n <namespace> <gateway> \
  -o jsonpath='{.spec.clusterCapacity.nodeAutoscaling}{"\n"}'   # empty ⇒ Present
```

```sh
# The message carries the verdict; confirm it on a stuck pod. Under
# nodeAutoscaling: Absent look for "FailedScheduling"; under Present look for the
# autoscaler's own event — "NotTriggerScaleUp" (cluster autoscaler) or a
# "FailedScheduling" whose source is karpenter rather than default-scheduler.
kubectl describe pod -n <namespace> <worker-pod>
```

```sh
# nodeAutoscaling: Present only — the raw events the gate read, with their reporters.
# Both timestamp columns are printed because the two recorder generations populate
# different ones. Today both autoscalers set lastTimestamp and leave eventTime empty
# (measured: cluster autoscaler v1.36.1, Karpenter v1.14.0), but the gate reads
# whichever is set, so the listing shows both.
kubectl get events -n <namespace> \
  --field-selector involvedObject.name=<worker-pod> \
  -o custom-columns='TIME:.lastTimestamp,MICROTIME:.eventTime,SOURCE:.source.component,REASON:.reason,MESSAGE:.message'
```

> **A declination is not always the last word, even when it is the last event.** One autoscaler loop can record a scale-up and *then* a declination for the same pod, milliseconds apart — the first round found a plan, a second round still could not place the pod even with the node that is now on its way.
> So the gate reads a declination as superseding a scale-up only when it lands more than a second after it; anything closer is one loop's own output and leaves the gate open, because a node is in fact arriving.
> If the events above show a `NotTriggerScaleUp` (or Karpenter `FailedScheduling`) close behind a `TriggeredScaleUp`/`Nominated` while the condition reads `False`, that is the rule working, not a stale condition.

**Resolution.** The gate is reporting a real cluster condition, so fix the placement problem — the resolutions are the same ones listed under [`WorkersUnschedulable`](#runnergroup-reports-workersunschedulable) (allocatable capacity, `nodeSelector`/affinity, tolerations).
Under `nodeAutoscaling: Present` the autoscaler has already told you which one in the condition message: a node-group ceiling (`max node group size reached`, `max total nodes in cluster reached`), an untolerated taint, a cloud quota, or — for Karpenter — no instance type with enough resources, or requirements no pool is compatible with.
The condition clears on the next reconcile once a worker pod schedules, or once the autoscaler records that it *is* scaling up for one.

> **The taint case names a category, not a key.** Cluster-autoscaler aggregates its per-node-group reasons into counts, so the message reads `1 node(s) had untolerated taint(s)` — *which* taint stopped the scale-up is in the autoscaler's own logs, not in the condition.
> Read it there:
>
> ```sh
> kubectl logs -n kube-system deploy/cluster-autoscaler | grep untoleratedTaint
> ```

**If you need jobs flowing again immediately**, turn the gate off and accept today's claim-and-cancel behaviour while you fix the cluster:

```sh
kubectl patch runnerset -n <namespace> <runner-set> --type=merge \
  -p '{"spec":{"capacityGate":{"mode":"Off"}}}'
```

The next delivered job (classic) or the next long-poll (`ScaleSet`) picks that up — no AGC restart — and the condition is retracted on the next reconcile.

> **Check this first if the reason is `PodsUnschedulable` and you did not expect the gate to be closed.** That reason means the gateway reports `clusterCapacity.nodeAutoscaling: Absent` — an assertion that nothing will add a node.
> It is sound **only** on a fixed-size cluster.
> If this cluster *does* run a cluster autoscaler or Karpenter, the assertion is wrong: the unschedulable pod was the request for a node, and gating on it can hold every set under this gateway back exactly when scale-up would have rescued them.
> Fix it **on the gateway**, not on the set:
>
> ```sh
> kubectl patch actionsgateway -n <namespace> <gateway> --type=merge \
>   -p '{"spec":{"clusterCapacity":{"nodeAutoscaling":"Present"}}}'
> ```

### A Cluster-Wide Verdict Closes Every Set at Once

Under `nodeAutoscaling: Present`, some autoscaler declinations are **cluster-wide rather than per-pool** — cluster autoscaler's `max total nodes in cluster reached` is the common one.
Every runner set with a stuck pod sees it in the same window, so they all report `WorkerCapacityDeclined=True` together and every one of them stops claiming.

That is correct — no node is coming for any of them — but it looks alarming, and it is worth distinguishing from a per-set problem before you start debugging individual sets.
Read the condition messages: a cluster-wide cause is the same sentence on every set, while a drained GPU pool names only the sets pinned to it.
The remedy is a cluster one (raise the total node ceiling, or free nodes), not a per-set one.

### The Mode Is Reported as Unsupported

`WorkerCapacityDeclined=False` with `reason: GateModeUnsupported` means this AGC does not implement the mode the set selected, so **no capacity rung is evaluated** and intake is exactly as it would be with the gate `Off`.

Two causes.
Either a version skew — the CRDs ship as their own chart (`actions-gateway-crds-v2`) and can be upgraded ahead of the controllers, so a mode a newer CRD accepts can reach an AGC that predates it — or a set still carrying one of the retired `SchedulerVerdict`/`AutoscalerVerdict`/`On` values, which are now `mode: Observe` plus a gateway-level fact ([upgrade note](upgrade.md#breaking-pre-ga-capacitygatemode-values-replaced-by-observe--a-gateway-level-cluster-fact)).

Failing open is deliberate: an operator who asked to *solicit* capacity did not ask to be gated on an observed verdict, and quietly substituting one for the other is exactly the silent wrong-semantics this gate's design is ordered around.

**Resolution.** Upgrade the AGC to a version that implements the mode (for a GMC-provisioned gateway, the AGC image is rolled by the GMC), or set the mode to one this AGC accepts.
The accepted values are on the CRD:

```sh
kubectl get crd runnersets.actions-gateway.com \
  -o jsonpath='{.spec.versions[-1:].schema.openAPIV3Schema.properties.spec.properties.capacityGate.properties.mode.enum}{"\n"}'
```

---

## Worker / Proxy / AGC Pods Rejected by a Cluster Policy Engine

**Symptoms.** Pods never appear at all (not even `Pending`): a `Deployment` stays at zero ready replicas, or no worker pod is created for an acquired job.
The owning controller's events or logs show an admission denial naming [Kyverno](https://kyverno.io) or [OPA Gatekeeper](https://open-policy-agent.github.io/gatekeeper/), for example:

```
admission webhook "validate.kyverno.svc-fail" denied the request:
... validation error: ... rule require-drop-all failed
```

**Cause.** A cluster-wide admission policy rejects the GAG pod for violating a rule it does not satisfy.
The usual culprits: a policy requiring `drop: [ALL]` capabilities or `allowPrivilegeEscalation: false` on *all* pods (the default `baseline` worker profile sets neither — baseline CI relies on in-job `sudo`); a `readOnlyRootFilesystem` requirement (no worker profile sets it — the runner needs a writable root filesystem); a registry allowlist that omits GAG's registries; or a "require resource limits" rule (AGC v1alpha1 pods carry none).

This is distinct from [`WorkersUnschedulable`](#runnergroup-reports-workersunschedulable) (scheduler can't place a *created* pod) and from `WorkerQuotaExceeded` (`ResourceQuota` blocks admission): here the policy engine blocks pod creation *before* either applies.

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

**Resolution.** Reconcile the cluster policy with GAG's real pod posture — see the [admission-policies compatibility matrix](admission-policies.md), which states per policy class whether GAG complies or what to allow.
In short:
- Add GAG's registries to your allowlist (`ghcr.io/actions-gateway/*` for the control plane, `ghcr.io/actions/actions-runner` for the default worker).
- For `drop-ALL` / no-privilege-escalation requirements: have tenants set `securityProfile: restricted` (which satisfies them), or apply the scoped [exception samples](examples/policies/README.md) so `baseline` workers pass.
- For `readOnlyRootFilesystem`: exempt worker pods (no profile can satisfy it).
- For "require limits" on AGC v1alpha1: migrate the tenant to a v2alpha1 `ActionsGateway`, or exempt AGC pods.

---

## ActionsGateway Reports EgressRulesStale

**Symptoms.** `kubectl get actionsgateway` shows an `EgressRulesStale=True` condition, or the `actions_gateway_egress_rules_stale` gauge is `1`.
Optionally, jobs intermittently fail to reach newly-rotated GitHub endpoints.

**Cause.** The GMC refreshes each managed proxy `NetworkPolicy`'s egress allowlist from `api.github.com/meta` on a ~24h cycle.
If that refresh loop stalls (GitHub meta API unreachable, persistent fetch errors), the allowlist freezes.
GitHub periodically rotates its published IP ranges, so a frozen allowlist eventually drops egress to the new ranges silently.
The condition trips when the last successful refresh is older than the staleness window (just over two refresh cycles), so a single missed/slow refresh does not false-trip it.
It is advisory and does **not** gate `Ready` — existing egress keeps working until GitHub rotates.
It is only evaluated for gateways whose proxy `NetworkPolicy` is gateway-managed (`spec.proxy.managedNetworkPolicy` unset or `true`).

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

**Resolution.** Restore the GMC's reachability to `api.github.com` (egress policy, DNS, proxy).
The `actions_gateway_ip_range_updates_total` counter resumes incrementing on the next successful refresh, and the condition clears automatically within the re-check cadence (a fraction of the staleness window).
If GitHub's meta API is down, no action is needed beyond waiting — the allowlist is still valid until GitHub rotates.

---

## A GHES Tenant's Traffic Never Reaches the Appliance

**Symptoms.** A tenant whose `spec.gitHubURL` names a GitHub Enterprise Server (GHES) host acquires no jobs.
Depending on which of the two gaps you have hit:

- **Token exchange fails.** The AGC logs a `401` from `api.github.com` — a host you never configured — when minting the installation token.
  This is the pre-fix behaviour: nothing set `GITHUB_API_BASE_URL`, so the AGC defaulted to public GitHub.
  Fixed by upgrading; see [Upgrade — GHES gateways now reach their own appliance](upgrade.md#non-breaking-github-enterprise-server-gateways-now-reach-their-own-appliance-they-never-did).
- **Egress is denied.** The AGC reaches the right host name and the connection times out.
  The tenant's `EgressProxy` reports `GitHubEgressIncomplete=True` / reason `ApplianceRangesRequired`, with a message naming the host.

**Cause of the second.** The default `CIDR` egress mode programs the merged `api`/`actions`/`web` ranges from `api.github.com/meta` as the proxy pool's egress `ipBlock`.
A GHES appliance sits on your own address space and appears in none of them, so the `NetworkPolicy` denies the proxy's traffic to the one host it exists to reach.
The GMC cannot close this gap — the appliance's ranges are knowable only to you — so it names it on status instead.

**Diagnostics.**

```sh
# Which pools are missing their appliance's ranges, and which host is unreachable.
kubectl get egressproxy -n <namespace> <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="GitHubEgressIncomplete")]}{.status} {.reason}: {.message}{"\n"}{end}'

# Confirm the AGC is addressing the appliance and not api.github.com.
kubectl get deploy -n <namespace> <agc> \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="GITHUB_API_BASE_URL")].value}{"\n"}'
```

Fleet-wide, the GMC exports the same condition as a gauge (Q537), so one query finds every pool carrying the gap — and the `ActionsGatewayGitHubEgressIncomplete` alert fires on it:

```promql
actions_gateway_github_egress_incomplete == 1
```

**Resolution.** Either:

- **Supply the ranges.** Put the appliance's CIDRs in the `EgressProxy`'s `spec.destinationCIDRs`.
  The field is gated by the platform `--allowed-egress-cidrs` allowlist, so a platform admin must allowlist them first — a tenant cannot self-serve.
  Admission rejects an entry outside the allowlist with a message naming the allowed ranges.
- **Switch to an FQDN egress mode.** `spec.egressPolicyMode: FQDN` (with a `--fqdn-policy-backend` configured) allows by hostname, and every referring gateway's `gitHubURL` host is added to the policy and the proxy CONNECT allowlist automatically.

The condition is advisory and does not gate `Ready`: the pool is serving exactly the policy it was asked for.
It clears once `destinationCIDRs` is non-empty — the GMC takes your declaration at face value and does not verify the ranges actually cover the appliance, so if traffic still fails, check the ranges themselves.

**Confirming it cleared.** `GitHubEgressIncomplete=False` always carries the reason `GitHubEgressAllowed`; the message says *which* clean state the pool is in.
The GMC takes the first that applies, so read the table top-down — only the last one means you supplied ranges:

| Message | The GMC sees no gap because |
|---|---|
| `egress policy is operator-maintained` | `spec.managedNetworkPolicy: false` — the GMC programs no `NetworkPolicy` for this pool, so it makes no claim either way. **Not a reachability check:** your own policy still has to allow the appliance. |
| `FQDN mode carries every referrer's GitHub host` | `spec.egressPolicyMode` is not `CIDR`, so each referring gateway's `gitHubURL` host is added to the policy automatically. |
| `every referrer targets public GitHub, which the CIDR allowlist covers` | No referring gateway names a GHES host, so the ranges `api.github.com/meta` publishes are the whole story. |
| `spec.destinationCIDRs supplies ranges for <host>` | You supplied ranges while a GHES referrer is bound. Taken at face value — this does **not** confirm they cover the appliance. |

**A private-CA appliance needs one more thing.** Egress reachability and TLS trust are separate problems: allowing the ranges gets packets to the appliance, but the AGC still has to validate its certificate.
If yours is fronted by an internal CA, see [A GHES Appliance's Certificate Is Not Trusted](#a-ghes-appliances-certificate-is-not-trusted).

---

## A GHES Appliance's Certificate Is Not Trusted

**Symptoms.** A GHES tenant reaches its appliance — the connection is not timing out — but every call fails at the TLS handshake.
The AGC logs `x509: certificate signed by unknown authority` on token exchange or runner registration, and job steps that talk to the appliance (`actions/checkout`, log and artifact upload) fail the same way from the worker pod.

**Cause.** The AGC and its worker pods trust the OS system roots plus the per-tenant egress proxy's own CA.
An appliance fronted by a private or internal certificate authority chains to neither, so its certificate does not verify.
Routing through an `EgressProxy` does not help: the proxy tunnels with `CONNECT`, so the TLS session is end to end between the AGC and the appliance.

**Fix.** Put the CA bundle in a ConfigMap in the tenant namespace under the key `ca.crt`, and name it from the gateway.
This is tenant-self-serve — no platform allowlist is involved, unlike the egress ranges above.

```bash
kubectl -n team-a create configmap ghes-ca --from-file=ca.crt=/path/to/corp-root-ca.pem
```

```yaml
apiVersion: actions-gateway.com/v2beta1
kind: ActionsGateway
metadata:
  name: my-gateway
  namespace: team-a
spec:
  githubURL: https://ghes.example.com/my-org
  githubCABundleRef:
    name: ghes-ca
```

The bundle is **additive**: the system roots stay trusted, so a gateway that also reaches public hosts is unaffected.
The GMC mounts it on the AGC Deployment and the AGC projects the same ConfigMap into every worker pod, so the control plane and the runners trust the same appliance.

Setting or changing `githubCABundleRef` re-renders the AGC Deployment, which restarts that tenant's AGC pod.
In-flight jobs are unaffected — worker pods are separate.

**If the gateway degrades instead.** The reference is resolved at runtime, and an unresolvable one fails closed rather than provisioning an AGC whose pod would sit at `ContainerCreating`:

| `Degraded` reason | Meaning | Remedy |
|---|---|---|
| `CABundleNotFound` | No ConfigMap of that name in the gateway's namespace | Create it, or fix the name. The gateway recovers on its own within about a minute — no edit needed |
| `CABundleInvalid` | The ConfigMap exists but has no `ca.crt` key, or that key holds no PEM certificate | Re-create it with `--from-file=ca.crt=...` and a PEM-encoded (not DER) certificate |

```bash
kubectl -n team-a get actionsgateway my-gateway \
  -o jsonpath='{.status.conditions[?(@.type=="Degraded")].message}{"\n"}'
```

`v1alpha1` has no equivalent field — it is frozen and removed at `v2.0.0`.
A GHES tenant behind a private CA must be on the v2 API.

**This does not cover the appliance's egress ranges.** Trusting the CA does not put the appliance's address space in the NetworkPolicy; that is the separate problem [above](#a-ghes-tenants-traffic-never-reaches-the-appliance).

---

## Tenant Namespace Missing the Managed-Tenant Marker Label

**Symptoms.** An `ActionsGateway` never becomes `Ready`. `kubectl describe` shows a `Warning` event with reason `NamespaceMarkerMissing`, and the GMC log reports a `Forbidden` error stamping Pod Security Admission labels, citing the `namespace-psa-guard` admission policy.
This is common immediately after upgrading a cluster whose tenant namespaces predate the policy (see [Upgrade — Migration Notes](upgrade.md#migration-notes)).

**Cause.** The GMC's cluster-wide `namespaces:patch` grant is gated by the `namespace-psa-guard` ValidatingAdmissionPolicy, which denies the GMC any namespace that is not labelled `actions-gateway.github.com/tenant: "true"`.
The label confines the grant to managed tenants so a compromised GMC cannot relabel `kube-system` PSA (see [Security §5.1/§5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in)).
The GMC never sets this label itself — a trusted administrator must apply it.
The same marker also gates the `gmc-tenant-resource-guard` policy, which confines every tenant-resource write (Deployments, Secrets, RoleBindings, …) to marked namespaces; provisioning fails at the PSA-stamping step first, so `NamespaceMarkerMissing` is the signal you will see, but applying the label clears both gates.

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

**Resolution.** Apply the marker label as an administrator, then the GMC reconciler retries automatically:

```sh
kubectl label namespace <namespace> actions-gateway.github.com/tenant=true
```

If the GMC's ServiceAccount is installed under a non-default namespace or name, also confirm the policy's `matchConditions` username (`system:serviceaccount:gmc-system:gmc-controller-manager`) matches your install.

---

## ActionsGateway Stuck Deleting (Teardown Blocked on a Failing Delete)

**Symptoms.** You deleted an `ActionsGateway`, but the CR does not disappear: `kubectl get actionsgateway -n <namespace>` still lists it with a non-empty `metadata.deletionTimestamp`, and `kubectl describe` shows a repeating `Warning` event with reason `TeardownIncomplete`.
Some tenant resources (e.g. the AGC Deployment, RoleBinding, or a ServiceAccount) are still present in the namespace.

**Cause.** Teardown is **fail-closed by design** (Q125): the GMC keeps the cleanup finalizer on the CR and requeues until it can confirm *every* owned resource is deleted (or already gone).
If a delete keeps failing — most often an API-server error, or a `Forbidden` from an admission policy or revoked RBAC — the finalizer is retained on purpose so a live, credentialed AGC Deployment is never orphaned by a half-finished teardown.
A NotFound is treated as success, so an already-deleted resource never blocks convergence.

This applies to both API versions (Q328): the v1 (`actions-gateway.github.com`) gateway holds the `actions-gateway.github.com/gmc-cleanup` finalizer; the v2 (`actions-gateway.com`) gateway holds its own cleanup finalizer and additionally **verifies each child is actually gone** after its delete is accepted — a child held by another controller's finalizer (its `deletionTimestamp` is set but the object lingers) also keeps the teardown open, with the lingering child named in the `TeardownIncomplete` event message.
The v2 per-tenant metrics Secrets are the one exception: they are removed by owner-reference garbage collection (the GMC deliberately holds no delete permission on Secrets), so they never appear in the event.

**Diagnostics.**

```sh
# Confirm the CR is mid-deletion and which resources remain
kubectl get actionsgateway -n <namespace> <name> -o jsonpath='{.metadata.deletionTimestamp}{"\n"}{.metadata.finalizers}{"\n"}'
kubectl describe actionsgateway -n <namespace> <name> | grep -A3 TeardownIncomplete

# The event message names the namespace and the underlying error; also check the GMC log
kubectl logs -n gmc-system deploy/gmc-controller-manager --tail=50 | grep -i "delete resource during teardown"
```

**Resolution.** Fix the underlying delete failure — restore API-server health, or re-grant the GMC the delete verb / re-apply the `gmc-tenant-resource-guard` marker if the namespace lost its `actions-gateway.github.com/tenant=true` label (the policy gates DELETE too, so an unmarked namespace blocks teardown).
The reconciler retries on its own backoff and removes the finalizer automatically once every delete is confirmed. **Do not** manually strip the finalizer to force the CR away — that re-introduces the orphaned-AGC failure mode the fail-closed behaviour exists to prevent; clear the real delete error instead.

**If the finalizer was already stripped.** Every namespaced child the GMC applies carries a controller `OwnerReference` to its `ActionsGateway`, in both API versions, so once the CR leaves etcd the Kubernetes garbage collector reclaims the AGC and proxy `Deployment`s, both `ServiceAccount`s, the AGC `RoleBinding`, the egress `NetworkPolicy`s, the `Service`s, the `PodDisruptionBudget`, the `HorizontalPodAutoscaler`, the TLS `Secret`s, and the `RunnerGroup`s — asynchronously and unordered, so worker pods are not drained first.
Give it a few seconds, then confirm the namespace is clean and delete anything left by hand:

```sh
kubectl get all,sa,rolebinding,networkpolicy,pdb,hpa,secret,runnergroup -n <namespace> -l app.kubernetes.io/managed-by=actions-gateway-gmc
```

Two categories are **not** covered by that cascade and must be checked explicitly after a stripped finalizer: the `v2alpha1` per-gateway `ClusterRoleBinding` (cluster-scoped, so a namespaced gateway cannot own it) and any object left over from a pre-v0.X install (the per-tenant `Role`, the legacy `actions-gateway` `NetworkPolicy`), which only the finalizer's explicit deletes reach.

```sh
kubectl get clusterrolebinding -l app.kubernetes.io/managed-by=actions-gateway-gmc
```

```sh
kubectl get role,networkpolicy -n <namespace>
```

---

## Tenant Namespace Stuck Terminating on agentpool-cleanup Finalizers

**Symptoms.** `kubectl delete namespace <tenant>` never completes.
The namespace sits in `Terminating` with nothing left in it, and the remaining objects hold `actions-gateway.com/agentpool-cleanup`, `actions-gateway.github.com/agentpool-cleanup`, or `actions-gateway.github.com/gmc-cleanup`.

**Cause.** The teardown order was inverted.
The AGC Deployments live *inside* the tenant namespace, so deleting the namespace (or an `ActionsGateway`, which cascades its AGC) before the `RunnerGroup`s and `RunnerSet`s removes the very controllers whose finalizers have to clear.
Nothing will clear them afterwards — this is structural, not a slow reconcile you can wait out.

**Fix.** Delete in dependency order next time: the CRs first, then the gateways, then the namespace.
The ordered commands and the manual finalizer-drop recovery (including what that recovery skips — the GitHub-side runner deregistration) are in [migration-v1-to-v2.md § Teardown order is load-bearing](migration-v1-to-v2.md#teardown-order-is-load-bearing-never-delete-the-namespace-first).
The same order applies to any tenant teardown, migration or not.

---

## Tenant Namespace Stuck Terminating After Narrowing the PriorityClass Allowlist

**Symptoms.** Same surface as the entry above — the namespace sits in `Terminating` and the remaining `RunnerGroup`/`RunnerSet` objects hold their `agentpool-cleanup` finalizer — but the teardown order was correct and the AGC was still running when the CRs were deleted.
The AGC logs (and its retry errors) show the finalizer update itself being denied by the allowlist guard:

```text
ValidatingAdmissionPolicy 'gmc-priorityclass-allowlist-guard' with binding
'gmc-priorityclass-allowlist-guard-binding' denied request: priorityTiers
priorityClassName <class> is not in the platform PriorityClassAllowlist ...
```

**Confirm it.** The stuck object still names a class the allowlist no longer carries:

```sh
# The deleting objects, their finalizers, and the classes they still name
kubectl --context "$CTX" get runnersets.actions-gateway.com,runnergroups.actions-gateway.github.com \
  -n <TENANT_NS> -o json | jq -r '
  .items[] | select(.metadata.deletionTimestamp != null)
  | "\(.kind)/\(.metadata.name) finalizers=\(.metadata.finalizers) classes=\([(.spec.priorityTiers // [])[].priorityClassName, (.spec.podTemplate.spec.priorityClassName // empty)])"'

# What the guard currently allows
kubectl --context "$CTX" get priorityclassallowlist -o jsonpath='{.items[*].spec.allowedPriorityClasses}'
```

**Cause.** The allowlist was narrowed while stored objects still named the removed class, on a chart version **without the Q518 deletion-only exemption**.
The guard policy (and the GMC webhooks on the tenant-facing kinds) re-validate the **whole stored object on every update**, and removing a finalizer is an update — so the AGC's teardown write was denied on every retry.
This is not a slow reconcile: nothing clears it until admission admits the write.

Current versions exempt deletion-only updates (deletionTimestamp set, spec unchanged) from re-validation in both admission layers, so this wedge no longer occurs: teardown completes even when the deleting objects name a removed class.
If you hit these symptoms, the cluster is running a pre-Q518 policy/webhook — upgrade the chart, or recover in place as below.
See [Narrowing the allowlist: drain stored references first](security-operations.md#narrowing-the-allowlist-drain-stored-references-first).

**Recovery.**

1. **Re-add the removed class** to the `PriorityClassAllowlist` CR in place — effective on the next watch event, no GMC restart, and it unblocks both the guard and the webhooks (the webhook allowlist is the union of the flag and this CR):

   ```sh
   kubectl --context "$CTX" edit priorityclassallowlist gmc-priorityclass-allowlist
   ```

2. **Let the AGC finish.** Its retry backoff clears the finalizer and the namespace completes terminating on its own. **Do not** strip the finalizer by hand while the controller can still do it — that skips the GitHub-side runner deregistration the finalizer exists for.

3. **If the AGC is already gone** — the namespace deletion killed it before the CRs cleared — re-adding the class unblocks admission but no controller remains to issue the write.
   That is the structural case of the entry above: follow the manual finalizer-drop recovery in [migration-v1-to-v2.md § Teardown order is load-bearing](migration-v1-to-v2.md#teardown-order-is-load-bearing-never-delete-the-namespace-first), including its caveats about what the manual drop skips.

4. **Narrow again only after draining** — move every remaining stored reference off the class, then remove it from the allowlist, per the [drain-first order](security-operations.md#narrowing-the-allowlist-drain-stored-references-first).

---

## Self-Serviced PriorityClasses Stopped Being Accepted All At Once

**Symptoms.** Classes that were being admitted are suddenly rejected — on both the worker surfaces (`priorityTiers[]`, `podTemplate.spec.priorityClassName`) and the infra ones (`spec.scheduling.priorityClassName`) — right after someone edited the `PriorityClassAllowlist` CR.
The edit itself **succeeded**; only the classes named directly on the GMC flags still work.

**Confirm it.** The GMC logged the refusal, naming the shared classes:

```sh
kubectl --context "$CTX" logs -n <GMC_NS> deploy/<release>-controller-manager \
  | grep sharedClasses
```

```text
WARNING: PriorityClassAllowlist would make the worker and infra allowlists
intersect; ignoring it and using the static flag allowlists only
  sharedClasses=["runner-standard"]
```

**Cause.** The CR put a class on one list that the **other surface's flag** already pins — `allowedInfraPriorityClasses` naming something in `--allowed-priority-classes`, or the reverse.
The CRD's CEL rule catches an overlap between the object's own two lists at write time, but it cannot read a controller flag, so this shape is admitted by the apiserver and caught by the GMC instead.

The two allowlists must stay disjoint: a class on both is nameable from a worker pod, which is how a tenant lifts its workers to infra priority and preempts another tenant's proxy.
Rather than apply half the object, the GMC drops **both** dynamic sets back to the flags — which is why every self-serviced class disappears together, not just the offending one.

**Recovery.** Remove the shared class from one of the two lists:

```sh
kubectl --context "$CTX" edit priorityclassallowlist <release>-priorityclass-allowlist
```

Both dynamic sets are restored on the next watch event, no restart.
If the class is genuinely needed on both surfaces, it is not — that is the escalation the split exists to prevent.
Create a second `PriorityClass` and give each surface its own; an `infra-` name prefix keeps the two sets obviously distinct.
See [Disjointness is enforced on every edit](security-operations.md#disjointness-is-enforced-on-every-edit-not-only-at-startup).

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
- If the Secret is missing or has wrong keys, recreate it.
  See [Getting Started — GitHub App Secret](../getting-started.md#3-create-a-github-app-credential-secret).
- If the private key format is wrong, ensure it is a PEM-encoded key starting with `-----BEGIN RSA PRIVATE KEY-----` (PKCS#1) or `-----BEGIN PRIVATE KEY-----` (PKCS#8, RSA or Ed25519).
  The Secret `stringData.privateKey` must include the full key including header and footer lines.
- If the runner version is outdated, update `workerImage` in the RunnerGroup spec (or the AGC's `--worker-image` flag).
  Watch for `RunnerGroup` conditions with reason `VersionTooOld`.
- If `appId` or `installationId` are wrong, update the Secret.

---

## ScaleSet RunnerSet Stuck Not Ready: `ScaleSetListenerStartFailed` Naming the Guard ConfigMap

**Symptoms.** A `ScaleSet`-protocol `RunnerSet` reports `Ready=False` with reason `NoActiveSessions`, and `kubectl describe runnerset` shows a Warning Event with reason `ScaleSetListenerStartFailed` whose message contains `load concluded-job guards`, typically ending `holds unparseable state (delete the ConfigMap to reset it)`.

**What happened.** Before its listener polls, the AGC loads the set's `scaleset-guards-<runnerset>` ConfigMap (Q606): the record of jobs it concluded whose queue messages may not be deleted yet, written so a hard-killed AGC does not provision workers for jobs that are over.
An unreadable or unparseable ConfigMap fails the listener start deliberately: polling without the guards would silently reopen that replay window, so the failure is surfaced instead, and the reconciler keeps retrying.
The AGC writes only well-formed JSON here, so unparseable state means the ConfigMap was edited by hand or corrupted.

**Diagnostics.**

```sh
# The event carries the load error verbatim
kubectl describe runnerset -n <namespace> <name>
```

```sh
# Inspect the guard state (a JSON object with completed/abandoned job-id lists)
kubectl get configmap -n <namespace> -l actions-gateway.com/runner-set=<name> -o yaml
```

**Resolution.** Delete the ConfigMap; the next reconcile starts the listener with empty guards and re-creates it on the next conclusion:

```sh
kubectl delete configmap -n <namespace> scaleset-guards-<name>
```

The cost of the reset is bounded and one-time: if a hard kill had stranded undeleted messages, their assignments replay once and may each provision one short-lived worker for a job that is already over (the pre-Q606 restart behaviour).
A transient apiserver error in the same event message needs no action at all; the reconciler retries and the listener starts once the read succeeds.

**Not this event: a session still held by the outgoing AGC during a rollout.** One session per scale set is a protocol invariant, so while the previous AGC finishes its exit teardown the new one cannot open a session.
That is a wait, not a fault, and since Q689 it records no Event at all — the reconciler logs `scale-set session still held by a predecessor; retrying shortly` at Info and re-attempts every couple of seconds until the predecessor's session delete lands.
Nothing to do.
If that line repeats for longer than the outgoing pod's termination grace period, the previous AGC is not completing its teardown: look at *its* logs for a stuck message delete or session delete rather than at the new pod.

---

## AGC Exits at Startup: GATEWAY_NAME Set but the v2 RunnerSet CRD Is Missing

**Symptoms.** A per-gateway AGC (`<gateway>-agc`) never becomes Ready, and its logs end at:

```
GATEWAY_NAME=<gateway> is set but the actions-gateway.com/v2alpha1 RunnerSet CRD is not
installed: a gateway-scoped AGC serves RunnerSets only, so it would reconcile nothing
(install the actions-gateway-crds-v2 chart)
```

**Cause.** Each AGC serves exactly one API: the v1 singleton (no `GATEWAY_NAME`) reconciles `RunnerGroup`s, and a gateway-scoped AGC reconciles only its own gateway's `RunnerSet`s.
With the v2 CRDs absent there is nothing for the latter to serve, so it exits rather than run a pod that passes its probes and reconciles nothing.

Normally unreachable — the GMC stamps `GATEWAY_NAME` only when provisioning from a v2 `ActionsGateway`, which the v2 CRDs must exist to serve.
In practice it means the opt-in `actions-gateway-crds-v2` chart was uninstalled (or rolled back) while v2 gateways were still deployed.

**Resolution.** Reinstall the CRD chart, or delete the v2 `ActionsGateway`s whose AGCs are failing:

```bash
helm upgrade --install actions-gateway-crds-v2 <chart> -n <gmc-namespace>
kubectl get crd runnersets.actions-gateway.com
```

---

## AGC Exits at Startup: Proxy CA Cert Present but Unreadable

**Symptoms.** The AGC never becomes Ready, and its logs end at:

```
read proxy CA /etc/actions-gateway/proxy-ca/tls.crt: permission denied
```

or, when the mounted file holds no PEM certificate:

```
build trust pool from /etc/actions-gateway/proxy-ca/tls.crt,
/etc/actions-gateway/github-ca/ca.crt: CA PEM contained no valid certificates
```

The message names both mounted CA paths because one pool holds both (the proxy CA and, on GHES, the [appliance's CA](#a-ghes-appliances-certificate-is-not-trusted)); the unparseable one is whichever is mounted.

**Cause.** The per-tenant egress proxy's CA is mounted but the AGC cannot read or parse it.
Only an *absent* file means "no TLS egress proxy" — the direct-egress and local-dev case, logged as `proxy CA cert absent; leaving the default transport unchanged`.
A mounted-but-unreadable CA is a misconfiguration: continuing without it would strip proxy trust and resurface much later as an unrelated-looking [`unknown authority` failure](#runners-never-appear-online--agc-unknown-authority-through-the-egress-proxy) on the first proxied GitHub call, so the AGC exits at startup instead.

The GMC renders the `proxy-ca` volume mode `0444` and projects only `tls.crt`, so neither error is reachable from a GMC-managed Deployment on its own.
A read error means the pod spec was mutated after the fact — a hand-edited Deployment, or a mutating policy engine rewriting volume modes or `securityContext`.
A parse error means the Secret's `tls.crt` is not a PEM certificate, which only a hand-edited Secret produces.

**Diagnostics.**

```bash
kubectl get secret -n <namespace> actions-gateway-proxy-tls -o jsonpath='{.data.tls\.crt}' \
  | base64 -d | openssl x509 -noout -subject -dates
```

```bash
kubectl get deploy -n <namespace> actions-gateway-controller \
  -o jsonpath='{.spec.template.spec.volumes[?(@.name=="proxy-ca")]}'
```

**Resolution.** For the parse error, delete the `actions-gateway-proxy-tls` Secret: the GMC re-issues an unparseable proxy cert on its next reconcile, and the proxy pods must be restarted to serve the new one.
For the read error, restore the mount as the GMC renders it — revert the manual edit, or exempt the tenant namespace from the mutating policy — then let the GMC re-render by bumping `ag.Spec`.
Either way the AGC recovers on its next restart.
An AGC that should egress directly must have no `proxy-ca` volume at all, rather than an empty or unreadable one.

---

## RunnerGroup ActiveSessions Exceeds maxListeners

**Symptoms.** `kubectl get runnergroup -n <namespace> -o jsonpath='{.items[*].status.activeSessions}'` reports a value greater than the group's `spec.maxListeners`, typically climbing by one after each broker or network outage.
GitHub shows more concurrent runner sessions for the group than the configured ceiling.

**What happened.** On AGC versions without the Q100 fix, a recoverable crash of the permanent baseline listener left the active count at zero for the duration of the restart backoff; a reconcile firing inside that window started a second permanent baseline on top of the pending restart.
Permanent listeners are restarted after every recoverable exit and are exempt from the `maxListeners` ceiling, so each repeat of the race ratchets the session count up by one, indefinitely.
Fixed versions make the multiplexer start idempotent, so the race cannot stack baselines.

**Resolution.**
- Upgrade the AGC image to a version with the Q100 fix.
- To clear excess listeners immediately on an affected version, restart the AGC Deployment (`kubectl rollout restart deploy/actions-gateway-controller -n <namespace>`).
  Listener sessions are in-memory; the restarted AGC re-creates exactly one baseline per RunnerGroup.
  On a GMC older than the Q552 fix the restart is a silent no-op — see [`kubectl rollout restart` of a Managed Deployment Reports Success but Nothing Restarts](#kubectl-rollout-restart-of-a-managed-deployment-reports-success-but-nothing-restarts).

---

## RunnerGroup Stops Serving Jobs With Stale Ready=True

**Symptoms.** A RunnerGroup stops servicing queued jobs even though the AGC pod is healthy, while `status.activeSessions` and the `Ready=True` condition still report the group as operational. `kubectl get runnergroup -n <namespace> -o jsonpath='{.status.activeSessions}'` shows a stale nonzero value that does not match the (zero) sessions GitHub sees for the group.

**What happened.** The permanent baseline listener exited *non-retriably* — e.g. GitHub returned `401 Unauthorized` on session creation for a credential it considers dead.
The multiplexer does not auto-restart a non-retriable exit (that restart is reserved for recoverable crashes), so the in-memory listener count drops to zero.
On AGC versions without the Q137 fix the RunnerGroup was only re-reconciled on a watch event (a RunnerGroup edit or a worker-pod lifecycle event) or the 10-hour informer resync, so with no such event the dead baseline — and the status written just before it died — could persist for hours.

**Resolution.**
- Upgrade the AGC image to a version with the Q137 fix.
  Fixed versions requeue the RunnerGroup on a bounded interval while the listener count is below the desired ceiling, so the reconciler re-runs its zero-listener recovery and revives the baseline within seconds; `status.activeSessions` and `Ready` then track reality again.
- To recover immediately on an affected version, trigger a reconcile by editing the RunnerGroup (e.g. a no-op annotation change) or restart the AGC Deployment (`kubectl rollout restart deploy/actions-gateway-controller -n <namespace>`); the restarted AGC re-creates one baseline per RunnerGroup from scratch.
  On a GMC older than the Q552 fix the restart is a silent no-op — see [`kubectl rollout restart` of a Managed Deployment Reports Success but Nothing Restarts](#kubectl-rollout-restart-of-a-managed-deployment-reports-success-but-nothing-restarts).
- If the baseline keeps exiting non-retriably after revival, the underlying credential or runner-version problem is real — check `kubectl describe runnergroup` for `Degraded` / `Unauthorized` / `VersionTooOld` conditions and resolve per the [AGC CrashLoopBackOff or Not Acquiring Jobs](#agc-crashloopbackoff-or-not-acquiring-jobs) section.

---

## Listener Stalls for Minutes After a Black-Holed Broker Connection

**Symptoms.** One of a RunnerGroup's sessions stops picking up jobs for minutes at a stretch even though the AGC pod is healthy, the broker is reachable, and other sessions in the same group keep working.
The stall typically follows a network event that silently drops an established connection — a firewall/NAT idle-timeout that discards packets without sending a RST, an egress-proxy failover, or a broker-side hang — so the long-poll's TCP connection is *black-holed*: accepted but never answered. `actions_gateway_message_poll_errors_total{reason="timeout"}` increments when an affected listener recovers.

**What happened.** The broker `GetMessage` long-poll holds the connection open for ~50s waiting for a job.
On AGC versions without the Q108 fix the broker client had no response-header deadline, so a black-holed connection blocked the listener goroutine inside a single `GetMessage` call until the operating system's TCP timeout expired — minutes — during which that listener served no jobs.
Fixed versions give the broker client a `ResponseHeaderTimeout` sized just above the 50s hold: a healthy long-poll is never cut short, but a black-holed connection is torn down a few seconds past the hold, classified as a benign "no message, retry", and the listener immediately opens a fresh long-poll.

**Resolution.**
- Upgrade the AGC image to a version with the Q108 fix.
  No configuration is required; the bound is built in.
- A steady stream of `actions_gateway_message_poll_errors_total{reason="timeout"}` after upgrade indicates the network is repeatedly black-holing broker connections (rather than wedging a listener).
  Investigate the egress path — proxy/NAT idle timeouts shorter than the 50s long-poll hold are the usual cause; raise the idle timeout above ~60s so healthy long-polls are not severed mid-hold.

---

## Reconcile or Token Mint Hangs on a Slow GitHub Endpoint

**Symptoms.** An AGC or GMC operation that calls a GitHub REST endpoint — installation-token mint, runner registration (`generate-jitconfig`), rerun-failed-jobs, or the GMC's `api.github.com/meta` IP-range fetch — appears to stall, and on a fixed version the logs now show a prompt `context deadline exceeded` / `Client.Timeout exceeded` error instead.
These are short request/response calls, distinct from the broker long-poll above.

**What happened.** Before the Q138 fix these clients fell back to `http.DefaultClient`, which has no timeout: a peer that accepted the TCP connection but was slow — or never — to send response headers wedged the calling goroutine (a reconcile or a token fetch) until the multi-minute OS TCP timeout.
Fixed versions build these clients with a bounded default (an overall request timeout plus a transport response-header timeout), so a slow GitHub endpoint fails fast and retriably rather than stalling the work.
The broker long-poll is the one deliberate exception — it is bounded by the response-header deadline above, not an overall timeout.

**Resolution.**
- Upgrade to a version with the Q138 fix.
  No configuration is required; the bound is built in.
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
- Upgrade the GMC to a version with the Q101 fix.
  Fixed versions reconcile `spec.runnerGroups` to the desired set: after applying the declared groups, the GMC prunes any owner-labelled `RunnerGroup` no longer in the spec, and keys pruning on owner labels (not list index) so a reorder never orphans a group.
  A subsequent reconcile (edit the CR, or wait for the next resync) cleans up any pre-existing orphans automatically.
- To remove a stranded group immediately on an affected version, delete its `RunnerGroup` directly: `kubectl delete runnergroup <name> -n <namespace>`.
  The AGC's RunnerGroup cleanup stops its listeners and cascades to its worker pods.
  Confirm you are deleting an orphan (its `runnerLabels` are not in the current `ActionsGateway` spec), not a live group.

---

## Proxy NetworkPolicy Has an Empty GitHub Allowlist

**Symptoms.** On a freshly provisioned tenant, all proxy egress to GitHub is silently dropped: `curl` through the proxy times out (no `502`), the AGC cannot acquire jobs, and token refresh fails.
The proxy `NetworkPolicy` exists but its `ipBlock` egress peers are empty.

**Likely cause.** The IP Range Reconciler's initial `api.github.com/meta` fetch failed or stalled at GMC startup.
The cached ranges seed each proxy `NetworkPolicy`'s `ipBlock` allowlist; until the first fetch lands, the allowlist is empty.
The reconciler retries the initial fetch on a capped exponential backoff (1s→30s), so a transient outage normally self-heals within seconds — but a sustained inability to reach `api.github.com` from the GMC pod (egress firewall, DNS, or a long GitHub outage) leaves the allowlist empty until connectivity returns.

For **direct-egress** gateways (no `defaultProxyRef`) the same GitHub allowlist lives on the `<gateway>-workload` and `<gateway>-agc` policies instead of a proxy policy, so the empty-allowlist symptom applies to worker and AGC egress there.
A gateway that has already been programmed **keeps** its allowlist across a GMC restart: the per-gateway reconcile preserves an existing direct-egress policy's rules while the cache is still warming, rather than rebuilding it from the empty cache (which would have blanked the allowlist for the seconds until the first fetch lands — a window that widened under node CPU pressure and caused release-asset downloads to time out right after a restart).
Only a **first-ever** provision with a not-yet-populated cache shows the empty allowlist, and it self-heals on the first fetch.

**Diagnostics.**

```sh
# Proxied gateway: inspect the proxy NetworkPolicy's GitHub ipBlock egress peers — empty means the cache never populated.
kubectl get networkpolicy -n <namespace> actions-gateway-proxy \
  -o jsonpath='{.spec.egress[*].to[*].ipBlock.cidr}'

# Direct-egress gateway: check the workload (and AGC) policy instead.
kubectl get networkpolicy -n <namespace> <gateway>-workload \
  -o jsonpath='{.spec.egress[*].to[*].ipBlock.cidr}'

# Look for retry warnings in the GMC log.
kubectl logs -n gmc-system deploy/gmc-controller-manager \
  | grep -i "GitHub IP-range"
```

**Resolution.**
- Confirm the GMC pod itself can reach `api.github.com` (corporate egress firewall, DNS, or proxy in front of the cluster).
  The reconciler retries automatically; once connectivity is restored the next successful fetch patches every existing `NetworkPolicy`.
- If the tenant manages its own egress policy (Cilium/Calico FQDN rules), set `spec.proxy.managedNetworkPolicy: false` so the reconciler leaves the policy alone.

---

## "Runner Lost Communication" and No Worker Pod Was Ever Created

**Symptoms.** GitHub fails the job with *"The self-hosted runner: … lost communication with the server.
Verify the machine is running and has a healthy network connection."* On the cluster there is **no worker pod at all** for that job — not `Pending`, not `Failed`, absent — and `kubectl get events` in the tenant namespace shows nothing about it unless you look at the owner object.
The message points at networking, but nothing is wrong with the network.

**Likely cause.** The API server rejected the worker pod, so the AGC never got one.
The job was acquired at GitHub, no runner ever came online to run it, and the job's lock lapsed.
Any create rejection produces this shape: an invalid `metadata.name`, a policy-engine admission webhook, a `PodSecurity` label the pod violates, or a missing `PriorityClass`.

The two most confusing instances were names the controllers derived themselves.
Both are fixed; both are worth recognising if you are running an older release, and in both the workaround is to rename the gateway to a different length.

- **The worker pod's own name (Q467).** `runner-<owner>-<jobID>` truncated to the 63-character DNS-label limit could land the cut on one of the job UUID's hyphens, and a name ending in `-` is rejected.
  Deterministic per gateway-name length: for an affected length, *every* worker pod was rejected and *no* job ever ran.
- **The `RunnerGroup` name used as a label value (Q473, `v1alpha1` only).** The GMC derives `<gateway>-<runner-label>`, and the AGC stamps that on every worker pod as `actions-gateway/runner-group`.
  Object names may be 253 characters but **label values stop at 63**, so past that the `RunnerGroup` reconciles perfectly while every worker pod create fails.
  A 15-character gateway with a 40-character runner label was enough. `v2alpha1`/`v2beta1` are unaffected — v2 caps CR names at 52 characters precisely so derived children fit.

Both now derive through one bounded helper: the budget is split across the name's segments and each truncated tail carries a hash, so a derived name is always valid and still unique. **On upgrade, a gateway whose derived `RunnerGroup` name exceeded 63 characters gets a renamed `RunnerGroup`** (the old one is pruned) — that tenant could not place a worker pod before the rename, so nothing working is disturbed.

**Diagnostics.**

```sh
# The AGC records a WorkerPodCreateFailed Warning on the owner, carrying the API
# server's own message. This is the shortest path to the real cause.
kubectl describe runnergroup -n <namespace> <name>          # v1alpha1
kubectl describe runnerset   -n <namespace> <name>          # v2alpha1
```

```sh
kubectl get events -n <namespace> --field-selector reason=WorkerPodCreateFailed
```

```sh
# The same rejection in the AGC log, with the pod name it tried to create.
kubectl logs -n <namespace> deploy/actions-gateway-controller | grep -i "rejected worker pod"
```

**Resolution.**
- Read the API server's message in the event. `metadata.name: Invalid value` or `metadata.labels: Invalid value … must be no more than 63 bytes` means a derived name — upgrade to a release carrying the Q467/Q473 fixes, or shorten the gateway name (or its first runner label) in the meantime.
  An `admission webhook … denied the request` means a cluster policy engine — see [Worker / Proxy / AGC Pods Rejected by a Cluster Policy Engine](#worker--proxy--agc-pods-rejected-by-a-cluster-policy-engine).
- Confirm the pod really is absent rather than reaped: `kubectl get pods -n <namespace> -l actions-gateway/runner-group=<group>` (v1) or `-l actions-gateway.com/runner-set=<set>` (v2).
- Re-run the workflow once the rejection is resolved; nothing is retried automatically, because the job's GitHub-side lock has already lapsed.

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
- If image pull is slow (first job on a cold node): this is expected.
  If it exceeds the p99 SLO (60s), consider pre-pulling the image via a DaemonSet or enabling image streaming.

**Deadline.** A pod that stays Pending is not held forever: after `pendingPodDeadline` (default 10m, per-RunnerGroup) the AGC deletes it to free the concurrency-ceiling slot it was holding — see the next runbook section.
Diagnose a stuck pod (`kubectl describe pod`) *before* the deadline reaps it, or raise `pendingPodDeadline` temporarily while debugging.

---

## Worker Pod Reaped While Pending (WorkerPodStuckPending)

> **Both acquisition tiers** perform the one-second force-cancel and the automatic re-run below.
> They read the same run identity from different places: the classic path from the payload the acquiring goroutine still holds, the ScaleSet path from the `actions-gateway.com/run-id` annotation on the pod the reaper is deleting.
> The `tier` label on `actions_gateway_abandoned_run_force_cancels_total` and `actions_gateway_abandoned_run_rerun_waits_total` says which one acted.

**Symptoms.** A `Warning` Event with reason `WorkerPodStuckPending` appears on the RunnerGroup (`kubectl describe runnergroup -n <namespace>`), `actions_gateway_worker_pods_reaped_total{reason="pending_deadline"}` increments, and the job the pod was created for does not run: its workflow run and job conclude **`cancelled`** within seconds of the reap (`actions_gateway_abandoned_run_force_cancels_total{outcome="cancelled"}` increments alongside).
The worker pod itself is gone.
If the run instead lingers `in_progress` for ~15 minutes before the same `cancelled` ending, the force-cancel could not act — see the outcome labels below.

**What happened.** The pod stayed `Pending` longer than the owner's `pendingPodDeadline` (default 10m), so the AGC deleted it.
A permanently Pending pod would otherwise hold one of the group's concurrency-ceiling slots forever — the ceiling counts Pending pods.
The deadline is a capacity-protection mechanism; the job is re-queued separately, by the automatic re-run described below, and only once the group can place a worker pod again.

**The session reports nothing for the assignment and force-cancels the run (Q628 → Q676 → Q683).** On a `ScaleSet` set there is no session to report from, the assignment being fire-and-forget, so only the force-cancel half applies, issued by the reconciler that reaped the pod (Q766).
On the classic path: no runner ever registered, so nothing inside the pod can report the job, and the AGC sends no `completejob` either.
Measured live (the Q645/Q676 probe runs, 2026-08-04): completing the winner's own never-run delivery concludes the run as **`success`** one second later, a job that never executed reporting green, for `result=abandoned` and `canceled` alike, while `failed` is refused with a 401, and the green run cannot be retried with `rerun-failed-jobs` (403).
Instead the AGC logs `worker pod was removed before it ran; reporting the job as abandoned and force-cancelling its run` and issues a REST `force-cancel` of the run: measured live (2026-08-05), the standalone call is accepted and concludes the run *and* job as **`cancelled`** about one second later, and the cancelled run *does* accept `rerun-failed-jobs`.
When the call cannot act because the payload carried no run identity (`outcome="identity_unknown"` on `actions_gateway_abandoned_run_force_cancels_total`) or GitHub refused it (`outcome="error"`), GitHub's own ~15-minute unstarted-job timeout reaches the same `cancelled` ending.
On the `ScaleSet` tier the identity comes off the worker pod instead, so a missing one reports as `actions_gateway_eviction_recovery_identity_unknown_total{cause="abandoned"}` with an `EvictionRecoveryIdentityUnknown` Warning Event, and means the assignment message carried no `workflowRunId`. `AGC_FANOUT_COMPLETION` does not affect this path (it gates only the Q260 sibling fan-out).

**Automatic re-run of the cancelled run (Q691).** The cancelled conclusion is recoverable, and the AGC recovers it for you: the run is queued for an automatic `rerun-failed-jobs`, fired once the owning group **places a worker pod again**.
The wait is the point.
The job was abandoned because its worker could not be scheduled, so re-running at once would put it straight back into the pool that was starved, and a shortage would compound into a re-run storm.
"Capacity returned" is a worker pod of this owner binding to a node (`PodScheduled=True`) after the abandonment; a pod that was already bound before it does not count.

Two bounds apply, and both are visible:

- **The retry budget.** The re-run draws on the same per-run budget as every other disruption recovery, so it reports as `actions_gateway_eviction_retries_total{cause="abandoned"}`, and a run abandoned again after its re-run is capped at `spec.maxEvictionRetries` re-runs in total.
  On exhaustion you get `actions_gateway_eviction_retries_exhausted_total{cause="abandoned"}` and an `EvictionRetriesExhausted` Warning Event on the owner, and the run then needs a manual re-run.
  The budget is keyed by run ID alone, so a run that is abandoned *and* evicted cannot spend two budgets.
- **The wait window.** Capacity may never return, because `pending_deadline` also reaps a pod no amount of waiting will place (an unpullable image, a constraint no node satisfies).
  After 30 minutes the wait is given up and counted as `actions_gateway_abandoned_run_rerun_waits_total{outcome="expired"}`; the successful case is `{outcome="capacity_returned"}`.

Fixing the root cause is still yours to do: the re-run is a recovery, not a repair, and a re-run into an unchanged unpullable image will be abandoned again until the budget runs out.

**Timing.** `pendingPodDeadline` is a floor, not an instant.
The AGC arms a timer for it on the reconcile that last saw the pod, and a reconcile that ends in an error — an optimistic-lock conflict on the owner's status is the routine one — hands the wake-up to the work queue's retry instead.
That retry is capped at 30s, so on a contended cluster a reap can land up to about half a minute past the deadline, and the `WorkerPodStuckPending` event with it.

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
- Fix the cause first, then let the automatic re-run land: it fires on the next worker pod the group places, so a fix that restores scheduling also releases the recovery.
  Re-run from the GitHub UI when the budget is already exhausted (`eviction_retries_exhausted_total{cause="abandoned"}`) or the wait expired (`abandoned_run_rerun_waits_total{outcome="expired"}`).

---

## Worker Pod Reaped While Pending After Its Job Completed (WorkerPodCompletedPending)

**Symptoms.** A `Warning` Event with reason `WorkerPodCompletedPending` appears on the `RunnerSet` (`kubectl describe runnerset <name> -n <namespace>`) and `actions_gateway_worker_pods_reaped_total{reason="completed_pending"}` increments.
Before the reap, the pod is `Pending` with a `FailedMount` event naming a `job-ss-<jobID>` Secret that does not exist.

**What happened.** GitHub reported the job terminal while its worker pod was still `Pending`, so the AGC deleted the pod thirty seconds later.
That pod could never have run: on the ScaleSet tier the job's terminal `JobCompleted` reclaims the JIT-config Secret the pod mounts, and a pod that has not mounted yet cannot mount one that is gone.

This is **not** a scheduling problem, which is why it is a separate reason from [`WorkerPodStuckPending`](#worker-pod-reaped-while-pending-workerpodstuckpending) — the scheduler placed the pod fine.
Before Q575 these pods were left to the unrelated `pendingPodDeadline` (default 10m), holding a concurrency slot and a node the whole time and then reporting themselves as a scheduling stall.

**Likely causes.**
- **Workflow runs cancelled shortly after being assigned** — the common one, and benign.
  The job ends between the assignment and the pod starting.
- **An AGC restart replaying its message queue — on releases before Q583.** A re-created scale-set session polls from cursor 0, and until the listener started deleting the messages it had finished with, every job the scale set had ever run was still in that queue: a restart replayed the lot and provisioned a worker for each.
  That produced a batch of these correlated with the AGC pod's restart time.
  Since Q583 the listener issues the delete half of the ack once every job in a message has concluded, so a restart re-reads only work that is genuinely unfinished — and since Q603 it issues any outstanding ones as it shuts down, so a graceful stop does not strand them either.
  A burst still correlated with a restart on a current release is therefore **not** the historical replay; it is one of the three narrower causes in the resolution below.
- **Pod startup slower than the job's own lifetime** — slow image pulls or node scale-up against very short jobs.

**Diagnostics.**

```sh
# The reap event names the deleted pod and the grace that elapsed
kubectl get events -n <namespace> --field-selector reason=WorkerPodCompletedPending
```

```sh
# Which Pending workers already have a completion stamp (they are on the clock)
kubectl get pods -n <namespace> -l actions-gateway.com/runner-set=<set> \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,JOB-DONE:.metadata.annotations.actions-gateway\.com/job-completed-at'
```

```sh
# Correlate a burst with an AGC restart
kubectl get pods -n <namespace> -l app.kubernetes.io/name=actions-gateway-controller
```

**Resolution.** A low background rate needs no action — the reap returns the slot and the node, and the workflow run was already over.
Treat a *sustained* or *bursty* rate as the signal:

- A burst right after an AGC restart, on a release before Q583, is the queue replay; it is self-limiting.
  On a current release the queue is pruned as jobs conclude, so a restart burst instead means messages are **not** being deleted — check the AGC log for two lines. `delete acked message` names the message id and the error each *rejected* delete returned. `queue reported the acked message already gone` is the quieter one: the delete was accepted but removed nothing, which is what a backend that has stopped serving the delete endpoint answers.
  Either way the queue grows and every subsequent restart is worse; an isolated already-gone line is harmless, a steady stream of them means nothing is being pruned at all.
- **One or two after a restart, with neither of those lines in the log, is the hard-kill window (Q603) — on releases before Q606.** A job concludes in the AGC's memory a moment before its message is deleted at GitHub.
  A graceful stop flushes those deletes on the way out, but a pod killed outright (SIGKILL at the end of its grace period, an OOM kill, a lost node) cannot, so the message survived and the next process read it.
  Since Q606 the conclusion is persisted to the set's `scaleset-guards-*` ConfigMap *before* any delete is issued, so a restarted AGC recognises the replayed assignment and never builds the pod: the residue of a hard kill is at most a re-derived conclusion in the log, not a worker.
  Seeing one on a current release means the kill landed in the moment between concluding and the ConfigMap write, which is possible but rare; confirm the kill itself by the pod's last state (`kubectl get pod -n <namespace> <agc-pod> -o jsonpath='{.status.containerStatuses[0].lastState}'`) showing a non-zero exit or `OOMKilled` rather than a clean termination.
  A *recurring* burst means the AGC is being killed rather than stopped — an under-set memory limit, or a grace period shorter than its shutdown.
- **One or two after an ordinary rollout, with no kill involved, is the unread-completion window (Q689), on releases before it.** Here the AGC had not yet learned the job was over: GitHub concluded it and wrote the `JobCompleted` to the queue, and the pod was replaced before a poll delivered it.
  Nothing was stranded by a failed delete and nothing was lost to a kill: the assignment was legitimately still held, so it replayed.
  The tell is that the AGC terminated cleanly (`lastState` shows a normal termination, not `OOMKilled` or a non-zero exit) and neither delete-failure line is in the log.
  Since Q689 the listener reads the queue's outstanding conclusions on its way out, before it flushes its deletes, and logs `read conclusions the queue was still holding at shutdown` with the job count when it did so; a rollout that ends without that line and without an error from `read outstanding conclusions on shutdown` (debug level) simply had nothing outstanding.
  Nothing to configure, but note it costs a few seconds of the pod's grace period per rollout, so a grace period trimmed below the AGC's shutdown will cut the read short and reopen the window.
- A steady rate with no restarts means jobs are being cancelled faster than pods start.
  Look at pod startup time (`kubectl describe pod` → image pull duration) rather than at the AGC.

The thirty-second grace is a fixed constant, not a CRD field: the pod has not started, so there is no runner shutdown to wait out — the grace exists only to let a pod that was already mid-start reach `Running`, where the longer [`orphaned_running`](#worker-pod-reaped-while-running-workerpodorphanedrunning) grace takes over.

---

## Worker Pod Reaped While Running (WorkerPodOrphanedRunning)

**Symptoms.** A `Warning` Event with reason `WorkerPodOrphanedRunning` appears on the `RunnerSet` (`kubectl describe runnerset <name> -n <namespace>`) and `actions_gateway_worker_pods_reaped_total{reason="orphaned_running"}` increments.
Before the reap fires, the shape is a set that looks busy and is not: `status.activeJobs` sits at some non-zero number, worker pods are `Running`, but no job is executing — `kubectl logs` on the pod ends at `Listening for Jobs`, and GitHub shows nothing in progress for the set.

**What happened.** The pod was still `Running` five minutes after GitHub reported its job terminal, so the AGC deleted it.
Two causes produce that:

- **A ScaleSet worker that never received its job** (the common one).
  The ScaleSet tier provisions fire-and-forget: the worker registers and pulls its own job.
  If the assignment lapsed, was cancelled, or completed elsewhere before the runner got to it, the runner waits at `Listening for Jobs` forever.
  It holds a concurrency slot, a namespace-quota slot, and a node while doing nothing.
- **A container that outlived the runner** — an injected mesh sidecar, or a regular (non-native) build/DinD sidecar.
  See the two runbook sections below for fixing the root cause; on the ScaleSet tier this reap is now the backstop that stops those pods accumulating.

Classic-protocol worker pods are never affected: their provisioning goroutine owns the pod through to a terminal phase, so a Running classic pod always has a live job behind it and is never given this deadline.

**Diagnostics.**

```sh
# The reap event names the deleted pod and the grace that elapsed
kubectl get events -n <namespace> --field-selector reason=WorkerPodOrphanedRunning

# Which Running workers already have a completion stamp (they are on the clock)
kubectl get pods -n <namespace> -l actions-gateway.com/runner-set=<set> \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,JOB-DONE:.metadata.annotations.actions-gateway\.com/job-completed-at'

# Rate of orphan reaps per set
# PromQL: rate(actions_gateway_worker_pods_reaped_total{reason="orphaned_running"}[1h])
```

**Resolution.** The reap itself needs no action — it returns the slot and the node.
Treat a *sustained* rate as the signal:

- Recurring orphans with no sidecar in the template mean jobs are being assigned and then lost before the worker can pull them.
  Check for a provisioning stall upstream — namespace `ResourceQuota` denials (`WorkerQuotaExceeded`), scheduling pressure (`WorkersUnschedulable`), or a `maxWorkers` set above what the quota can hold, which puts the AGC into a provision/deny/retry loop while GitHub's 10-minute lock lapses under it.
- Orphans on a set whose pods show `READY 1/2` are the sidecar cases — fix those at the source (next two sections).

The five-minute grace is a fixed constant, not a CRD field: it measures runner shutdown (a runner that actually ran its job reports completion and exits within seconds), and the job is already over at GitHub, so the only thing the reap costs is the terminal pod's `completedPodTTL` inspection window.

---

## Workers Left Behind by an AGC That Was Down

**Symptoms.** Worker pods sit `Running` for hours with no job executing, their nodes never scale down, and — unlike the section above — **no** `WorkerPodOrphanedRunning` Event ever fires and `actions_gateway_worker_pods_reaped_total` does not move.
The AGC for that tenant is `Pending`, `CrashLoopBackOff`, or gone.

**What happened.** The reap above needs a *live* AGC.
If the AGC itself was down when a job ended — evicted, `Pending` with nowhere to schedule, `CrashLoopBackOff` — nothing stamped its workers, and those pods have no reap deadline at all.
They keep `Running`, hold their concurrency and quota slots, and because every worker carries `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` (deliberately — a worker mid-job has no replacement) the autoscaler will not reclaim their nodes either.
One dogfood incident stranded five workers and 82 spot node-hours this way.

Restarting the AGC clears most of this on its own.
Workers that already reached a terminal phase, workers stuck `Pending`, and workers stamped before the AGC died are all reclaimed within their normal deadlines with no action from you.
A still-`Running` worker whose job ended during the outage is reclaimed if GitHub redelivers that job's completion to the restarted AGC, which stamps the pod and reaps it five minutes later.
GitHub holds that completion for a long time: a live probe found one still redelivered to a brand-new session **13 hours** after the previous session went away (observed 2026-07-29 — GitHub does not publish a retention window, so treat this as "long enough in practice", not a guarantee).
Since no worker outlives the 12 h `maxWorkerLifetime` below anyway, redelivery covers essentially the whole span in which a stranded `Running` worker can still exist.

**The unconditional backstop is `maxWorkerLifetime` (default 12h).** Every worker pod is created with that value as its `activeDeadlineSeconds`, so the **kubelet** — not the AGC — kills a worker that outlives it.
That is what bounds this failure even when the AGC never comes back at all: the incident above ran 16 hours precisely because nothing was running to notice.
A pod killed this way goes `Failed`/`DeadlineExceeded`, is reaped under `reason="lifetime_exceeded"`, and emits a `WorkerPodLifetimeExceeded` Warning Event.
See [Worker Killed by the Lifetime Cap](#worker-killed-by-the-lifetime-cap-workerpodlifetimeexceeded) if that is what you are looking at.

So: **bring the AGC back first, then wait for one reap cycle before deleting anything by hand.** Hand-deletion is now the impatient path, not the only one — an untouched pod is reclaimed within `maxWorkerLifetime` regardless.

```sh
# 1. Is the AGC actually running? A Pending/CrashLoop AGC reaps nothing.
kubectl get pods -n <namespace> -l app.kubernetes.io/component=controller
```

```sh
# 2. After it is Ready, list Running workers that still have no completion stamp.
#    A JOB-DONE of <none> a few minutes after the AGC is healthy is the stuck shape.
kubectl get pods -n <namespace> -l actions-gateway.com/runner-set=<set> \
  --field-selector status.phase=Running \
  -o custom-columns='NAME:.metadata.name,NODE:.spec.nodeName,AGE:.metadata.creationTimestamp,JOB-DONE:.metadata.annotations.actions-gateway\.com/job-completed-at'
```

Before deleting any of those, confirm the job really is over at GitHub — check the run in the Actions UI, or `kubectl logs <pod> -c runner` and look for a worker sitting at `Listening for Jobs` with nothing after it. **A pod whose job is genuinely still executing looks identical in cluster state**, and deleting it strands that job with no replacement.

```sh
# 3. Only once confirmed abandoned:
kubectl delete pod <pod> -n <namespace>
```

Prevention is on the teardown side: never scale down the pool carrying the AGCs while jobs are in flight.
For the dogfood cluster, `scripts/dogfood/stop.sh` drains in-flight workers before resizing and fails the stop rather than stranding them.

---

## Worker Killed by the Lifetime Cap (WorkerPodLifetimeExceeded)

**Symptoms.** A job that used to finish now fails partway through, always at roughly the same elapsed time.
The worker pod is `Failed` with `reason: DeadlineExceeded`, a `Warning` Event with reason `WorkerPodLifetimeExceeded` appears on the `RunnerGroup`/`RunnerSet`, and `actions_gateway_worker_pods_reaped_total{reason="lifetime_exceeded"}` increments.

```sh
kubectl get pod <pod> -n <namespace> -o jsonpath='{.status.phase}{"\t"}{.status.reason}{"\n"}'
# Failed	DeadlineExceeded
```

**What happened.** Every worker pod is created with `spec.maxWorkerLifetime` (default **12h**) as its `activeDeadlineSeconds`, and the **kubelet** killed this one for exceeding it.
The cap exists because a worker whose job ended while the AGC was down carries no other deadline — and in the incident that motivated it the AGC was down for the whole 16 hours, so an AGC-side deadline would not have helped.
The kubelet is the one actor still running in that failure.

There are two possibilities, and they are worth telling apart before changing anything:

- **The job was genuinely stuck** — the cap did its job.
  Check whether GitHub shows the run as still executing at the time of the kill, or whether the runner had been sitting at `Listening for Jobs`.
- **The job was legitimately that long.** GitHub's own default `timeout-minutes` is 360 (6 h), so a job running past 12 h has explicitly declared a `timeout-minutes` more than twice that default.
  GAG cannot read that declaration — the job's timeout is not carried on the acquisition wire — so it cannot distinguish a 14-hour job from a wedged one, which is exactly why the cap is a blunt fixed value with an override.

**Resolution for a legitimately long job.** Raise the cap on that group or set:

```sh
kubectl patch runnerset <name> -n <namespace> --type=merge \
  -p '{"spec":{"maxWorkerLifetime":"24h"}}'
```

```sh
# v1: the same field on the RunnerGroup
kubectl patch runnergroup <name> -n <namespace> --type=merge \
  -p '{"spec":{"maxWorkerLifetime":"24h"}}'
```

Set it to `"0s"` to disable the cap entirely — that restores the pre-cap behaviour, including the orphan risk above, so prefer a raised value over an opt-out.
A negative value is rejected at admission.
An `activeDeadlineSeconds` set explicitly on the pod template takes precedence over this field and is never overwritten, which is the escape hatch for a per-template exception.

Note the ceiling you cannot raise past: GitHub terminates any job on a self-hosted runner at **5 days** regardless of this setting.

---

## Worker Pods Reaped on Gateway Teardown (WorkerPodsReapedOnGatewayTeardown)

> Applies to the `v2alpha1`/`v2beta1` (`actions-gateway.com`) API. v1 gateway teardown deletes its `RunnerGroup`s, so the pods cascade off their owner reference instead.

**Symptoms.** Someone deleted an `ActionsGateway`, and the jobs that were running on its tenant failed mid-run with "The runner has received a shutdown signal" or lost communication.
A `Warning` Event with reason `WorkerPodsReapedOnGatewayTeardown` appears on each affected `RunnerSet`, `actions_gateway_worker_pods_reaped_total{reason="gateway_deleted"}` increments by the number of pods, and the sets sit `Ready=False`/`GatewayTerminating` until the gateway is gone.

**What happened — this is deliberate.** Deleting a gateway tears down the AGC that serves it, and that AGC is the *only* thing that reaps its tenant's worker pods: the pods are owned by `RunnerSet`s, which survive gateway deletion by design so a tenant can re-apply the gateway and resume.
Before this behaviour existed the pods were simply left behind, and their node-disruption-safety annotations (`karpenter.sh/do-not-disrupt`, `cluster-autoscaler.kubernetes.io/safe-to-evict: false`) kept consolidation and scale-down away from them, pinning a billable node until the kubelet's `activeDeadlineSeconds` fired up to `maxWorkerLifetime` — 12 hours by default — later.

So the AGC now stops acquiring and deletes them itself while it still has the permissions to do so, and the GMC holds the gateway's teardown open until it has (see the next entry).
A job running at that moment is lost; there is no way to both delete the gateway and finish the job.

**Resolution.** Nothing to fix — the reap is the correct outcome of the delete.
To avoid losing jobs next time, **drain before deleting the gateway**: stop routing new work to the tenant, wait for `status.activeJobs` and `status.pendingJobs` to reach zero on every bound `RunnerSet`, then delete.

```sh
kubectl get runnerset -n <namespace> \
  -o custom-columns='NAME:.metadata.name,GATEWAY:.spec.gatewayRef.name,ACTIVE:.status.activeJobs,PENDING:.status.pendingJobs'
```

If you only meant to take the tenant offline temporarily, scaling the AGC down is not the tool either — deleting the gateway is what removes the reaper.
Prefer setting `maxWorkers: 0` on the sets, which stops new jobs while leaving the control plane in place.

---

## ActionsGateway Deletion Hangs on WaitingForWorkerDrain

> Applies to the `v2alpha1`/`v2beta1` (`actions-gateway.com`) API.

**Symptoms.** `kubectl delete actionsgateway` does not return.
The gateway sits `Terminating` on the `actions-gateway.com/gmc-cleanup` finalizer, and `kubectl describe` shows repeated `Normal` `WaitingForWorkerDrain` events naming the sets that still report workers.

**What happened.** Teardown deliberately deletes nothing until the AGC has reaped the tenant's worker pods (previous entry) — deleting its Deployment first would strand them.
The wait normally lasts a second or two: the counts fall as soon as the AGC *issues* its deletes, because a pod that already carries a deletion timestamp is finished by the kubelet with no controller involved.

**Likely cause of a long wait.** The AGC is not running, so nothing is reaping and nothing is updating the counts — it is crash-looping, scaled to zero, or never became healthy.

```sh
kubectl get deploy -n <namespace> -l app.kubernetes.io/component=agc
kubectl logs -n <namespace> deploy/<gateway>-agc --tail=50
```

**Resolution.** Teardown is bounded: after **90 seconds** it proceeds anyway and emits a `WorkerDrainTimeout` Warning naming what it is leaving behind.
If you see that event, those pods now have no reaper and are bounded only by their `maxWorkerLifetime` deadline — delete them by hand to release the node:

```sh
kubectl delete pod -n <namespace> -l actions-gateway.com/runner-set=<set-name>
```

To avoid the wait entirely, fix or restore the AGC before deleting the gateway, or drain the sets first so there is nothing to wait for.

---

## Scale-Set Job Stranded by a Stale Runner Record (Runner-Name 409)

**Symptoms.** On the scale-set path (`acquisitionProtocol: ScaleSet`), one job is never picked up while others in the same `RunnerSet` run fine.
The AGC logs repeat `scaleset: runner name conflict` for a single `jobID`, and — on versions before the fix — end in `scaleset: runner name conflict persists, skipping job`.
Restarting the AGC clears it.
In the repo/org runner list, offline records named `<scaleSet>-<jobID>` (and `<scaleSet>-<jobID>-1`, `-2`, `-3`) accumulate over time.

**What happened.** The scale-set listener pre-registers each worker's runner under a deterministic `<scaleSet>-<jobID>` name via `generatejitconfig`.
GitHub auto-removes an ephemeral runner's record only when that runner actually *completes a job*, so any worker killed before then — reaped while still `Pending` (see [above](#worker-pod-reaped-while-pending-workerpodstuckpending)), reaped past its lifetime cap, or failed before the runner connected — leaves its record behind, offline, still holding the name.
Every later re-provision of the same `jobID` derives the same name and `409`s.

**Fix.** Three mechanisms, in the order they engage:

1. **Deregistration on reap.** When the AGC reaps a scale-set worker pod it deletes that pod's runner record first (REST `DELETE .../actions/runners/{id}`), using the name stamped on the pod as `actions-gateway.com/runner-name`.
   This is what stops the records accumulating in the first place.
   A record still running a job (`422`) is kept, and a failed deregistration never blocks the reap.
2. **Reclaim on conflict.** On a base-name `409` the listener deletes the colliding record and re-registers under the same name, so a job that races its own leftover still provisions under its deterministic name.
3. **Sweep at listener start.** When a listener starts it lists the records named `<scaleSet>-*` and deletes those that are offline, not busy, and not claimed by any live worker pod.
   This collects what the first two cannot: records whose pod is already gone (an AGC that crashed between deregistering and deleting), and the suffixed `-1`/`-2`/`-3` names the conflict path mints and never revisits.

A record whose worker pod still exists is never swept, in any phase — a `Pending` worker's record is legitimately offline, and deleting it would strand exactly the job the sweep exists to protect.

> **Before `v1.3.0`, only mechanism 2 existed**, and it clears a record only if the REST name filter resolves it.
> Records it could not resolve, and every suffixed retry name, accumulated unbounded — 22 stale records under one scale set wedged the `v1.3.0-rc.2` validation window this way.
> If you see `scaleset: runner name is taken but no record resolves under it` in the AGC logs, mechanism 2 is failing and the sweep is what will collect the leftovers.
> On such a version, use the manual cleanup below.

**If the conflict does not clear.** A name no attempt can register — a live runner holding it, or a record the AGC's credentials cannot delete — leaves that one job unprovisionable.
It is not dropped: the listener holds it and re-offers it on a backoff (30s, doubling to 5 minutes) until it provisions, so the run still starts whenever the conflict clears.
The hold ends without running the job only if GitHub reports the job complete, or if the scale set stops counting the assignment at all — see [Scale-Set Assignments Abandoned](#scale-set-assignments-abandoned-assignmentabandoned).
While a job is held, the `RunnerSet` reports the advisory condition `JobProvisionStalled=True/RunnerNameConflict` naming the job ids, the `actions_gateway_scaleset_jobs_deferred{reason="name_conflict"}` gauge is `> 0`, and a `JobProvisionStalled` Warning Event is recorded once per episode.
Other jobs in the set are unaffected.

```sh
kubectl get runnerset <name> -n <namespace> \
  -o jsonpath='{.status.conditions[?(@.type=="JobProvisionStalled")].message}'
```

Act on it by freeing the name: find the record the message's `<scaleSet>-<jobID>` names in the repo/org runner list and delete it if it is offline (the cleanup below), or wait out the live runner still using it.

**Manual cleanup (older versions, or to clear an existing backlog).** Delete the offline records — they re-register on the next run:

```sh
# List self-hosted runner records (repo-scoped shown; use /orgs/<org>/... for org scope)
gh api /repos/<owner>/<repo>/actions/runners --paginate \
  | jq -r '.runners[] | select(.status=="offline") | "\(.id)\t\(.name)"'

# Delete each offline record by id (skip any that are online or busy)
gh api -X DELETE /repos/<owner>/<repo>/actions/runners/<id>
```

Only delete records that are `offline` and not `busy`; an `online` record is a live listener or a running worker.

---

## Scale-Set Jobs Waiting at the Worker Ceiling (WorkerCeilingReached)

**Symptom.** A ScaleSet `RunnerSet` reports `JobProvisionStalled=True/WorkerCeilingReached` and `actions_gateway_scaleset_jobs_deferred{reason="ceiling"}` is `> 0`.
Jobs are queued at GitHub with no worker running them, while the set's existing workers keep running normally.

**This is normal backpressure, not a fault.** The set is running as many workers as its spec allows — `spec.maxWorkers`, or the last `spec.priorityTiers` threshold.
GitHub keeps assigning jobs up to the capacity the listener advertises, and each one that arrives with no slot free is held and re-offered every 5 seconds until a running worker finishes.
Nothing is dropped, and the held jobs start in turn as capacity frees.

```sh
kubectl get runnerset <name> -n <namespace> \
  -o jsonpath='{.status.conditions[?(@.type=="JobProvisionStalled")].message}'
```

**The condition's three reasons.** `JobProvisionStalled` is advisory — it never gates `Ready` — and it appears only on the scale-set path (`acquisitionProtocol: ScaleSet`):

| `status` / `reason` | Meaning |
|---|---|
| `True` / `RunnerNameConflict` | At least one held job cannot register its runner name. An anomaly worth acting on — see [Scale-Set Job Stranded by a Stale Runner Record](#scale-set-job-stranded-by-a-stale-runner-record-runner-name-409). Outranks `WorkerCeilingReached` whenever both classes are held at once, because it is the one you can do something about; a full ceiling clears itself. |
| `True` / `WorkerCeilingReached` | Every held job is waiting only on worker capacity — this section. |
| `False` / `JobsProvisioning` | No assigned job is waiting on a runner name or on capacity. Published when the listener opens its session and again the moment the last held job provisions or completes, so a set that has never stalled reports it too. |

**When to act.** Only when the wait itself is the problem.
Either raise the ceiling (`spec.maxWorkers` or the top `priorityTiers` threshold) if the cluster has room, or treat it as the signal that the tenant needs more capacity.
Check first that the ceiling is the real constraint and not a downstream one — `WorkerQuotaExceeded` (namespace `ResourceQuota`) and `WorkersUnschedulable` (nothing can place the pod) are separate conditions with their own sections.

**What it should NOT look like.** A steady stream of `generate-jitconfig` and runner-deregister calls against GitHub while the set sits at its ceiling.
Before `v1.3.0`, a ceiling-blocked job was read as a transient failure: the queue message was redelivered immediately, so the listener retried it several times a second and minted (then deregistered) a runner registration on every pass — 704 deregister calls for a single job during one 14-minute window.
If you see that on an older version, the fix is to upgrade; the ceiling wait itself was never the bug.

---

## Scale-Set Assignments Abandoned (AssignmentAbandoned)

**Symptom.** A ScaleSet `RunnerSet` records an `AssignmentAbandoned` Warning Event, `actions_gateway_scaleset_jobs_abandoned_total` steps up, and `JobProvisionStalled` clears at the same moment.
The AGC logs `scaleset: giving up on assigned jobs the scale set no longer holds` naming the job ids.

**What happened.** The listener was holding one or more assignments it could not provision — a runner name that would not register, or a full worker ceiling — and the scale set's server-authoritative statistics then reported **no assigned jobs at all**, twice in a row.
A held job is by definition assigned-and-not-complete, so GitHub counts it; a count of zero means GitHub is no longer holding any of them.
Rather than keep re-offering assignments that no longer exist, the listener gives them up.

**Each abandoned job is a workflow run that will not run.** GitHub had already stopped waiting for it, so this reports the loss rather than causing it — but the run is not going to start on its own.
Re-run it from the Actions UI or `gh run rerun <run-id>`.

**When this is expected.** A burst around a mass cancellation, a deleted run, or a `stop.sh` drain — GitHub drops the assignments and, for a job it never started, does not always send a terminal `JobCompleted`.
Before `v1.3.0`, nothing ended those holds: the listener re-offered them forever and provisioned a worker for every re-offer that got through, which is what stopped a drain converging (`stop.sh` waits on in-flight worker pods, and the listener kept making them).
Fifteen such assignments wedged the `ci` tenant on the `v1.3.0-rc.3` gate and cleared only by hand.

**When to investigate.** A sustained rate with no cancellations to explain it.
That points at assignments being lost upstream rather than completed — check for a mismatch between `…_jobs_assigned_total` and `…_jobs_completed_total` over the same window, and check the run service for jobs being concluded without a queue message.

```sh
# Which jobs were given up on, and when
kubectl logs -n <namespace> deploy/<gateway>-agc \
  | grep 'giving up on assigned jobs'
```

**Convergence takes a couple of minutes.** The check reads the statistics at most once a minute and needs two consecutive zero readings, so expect a stalled set to clear roughly two to three minutes after GitHub drops the last assignment — well inside `stop.sh`'s default `DRAIN_TIMEOUT`.
The wait is deliberate: a count is server state a fresh assignment may briefly lead, and a single reading could give up on a job GitHub was still waiting to have run.

---

## Worker Pods Stuck Running After the Job Finished (Mesh Sidecar)

**Symptoms.** Worker pods sit `Running` with a not-ready container count (`READY 1/2`) long after their job completed; `completedPodTTL` never deletes them; over time the RunnerGroup wedges at `maxWorkers` and new jobs stop being picked up even though no job is actually executing. `kubectl get pod -o jsonpath='{.spec.containers[*].name}'` shows a second container such as `istio-proxy` or `linkerd-proxy`.

**What happened.** A service-mesh sidecar was injected into the worker pod.
GAG worker pods run to completion: the slot is freed and the pod reaped only when the pod reaches a *terminal* phase (`Succeeded`/`Failed`), which requires every container to exit.
A classic mesh sidecar never exits on its own, so the pod stays `Running` forever and falls through the two phase-based reaper paths (`completedPodTTL` covers terminal pods; `pendingPodDeadline` covers `Pending` pods — neither covers a stuck `Running` pod).

On the **ScaleSet** protocol there is a backstop: once GitHub reports the job terminal, a pod still `Running` five minutes later is reaped as [`WorkerPodOrphanedRunning`](#worker-pod-reaped-while-running-workerpodorphanedrunning), so the slot comes back instead of wedging the set.
It is a backstop, not a fix — the pod is still killed rather than completing, so resolve the sidecar as below.
On the **Classic** protocol there is no such backstop and the pods accumulate until deleted by hand.

**Resolution.** Opt the GAG tenant namespace out of the mesh, or — if mesh membership is mandatory — switch to native sidecars (Kubernetes 1.28+) or a sidecar-less/ambient data plane.
The full per-mesh configuration (Istio sidecar + ambient, Linkerd, Cilium, generic) is in [Running GAG Alongside a Service Mesh](service-mesh-coexistence.md).
Note that mesh opt-out/exclusion annotations set on the RunnerGroup `podTemplate` are **not** honored — GAG strips arbitrary worker-pod-template metadata; configure the mesh at the namespace level instead.

A mesh sidecar is *injected* at admission, so it is not in the `RunnerTemplate` and GAG cannot warn about it ahead of time — the runtime symptom above is the only signal.
A **build/DinD sidecar you declare in the template** is different: GAG detects it and warns proactively — see the next section.

---

## RunnerSet Reports PossibleReapBlockingSidecar (Build/DinD Sidecar in the Template)

**Symptoms.** A `RunnerSet` reports the advisory condition `PossibleReapBlockingSidecar=True` (`kubectl get runnerset <name> -n <ns> -o jsonpath='{.status.conditions}'`), the `actions_gateway_reap_blocking_sidecar_templates` gauge is `> 0`, and/or `kubectl apply` of the `RunnerTemplate`/`ClusterRunnerTemplate` printed a `Warning:` naming a sidecar container.
Left unfixed, the symptom is the same `READY 1/2` stranding as the mesh-sidecar case above: worker pods linger after the job and the set wedges at `maxWorkers`.

**What happened.** The resolved worker template carries a **regular** (non-native) sidecar container besides the `runner` container — e.g. a `docker:dind` daemon or a BuildKit sidecar declared under `spec.containers[]`.
A pod terminates only when every regular container exits, so a sidecar that runs for the life of the job keeps the pod from reaping (Q249).
The condition, gauge, and admission warning are **advisory only** — they never block template creation or gate the set's `Ready` — because the "runs forever" property can't be proven from a pod spec.

**Resolution.**

- **Preferred — convert to a native sidecar.** Move the sidecar to `spec.initContainers[]` with `restartPolicy: Always` (Kubernetes ≥ 1.29).
  The kubelet tears it down when the `runner` container exits, so the pod completes on its own.
  The template shape is in [In-runner image builds § Sidecar containers must be native sidecars](in-runner-image-builds.md#sidecar-containers-must-be-native-sidecars-q249).
- **If the sidecar genuinely exits on its own** when the job ends, acknowledge it in the template's `actions-gateway.com/self-exiting-sidecars` annotation (a comma-separated name-list).
  This silences the warning, the condition, and the gauge for the named containers only — a newly added, unacknowledged sidecar still warns.

```bash
# See which containers a set's condition is flagging
kubectl get runnerset <name> -n <namespace> \
  -o jsonpath='{.status.conditions[?(@.type=="PossibleReapBlockingSidecar")].message}'
# Which templates are flagged, fleet-wide
# PromQL: actions_gateway_reap_blocking_sidecar_templates > 0
```

---

## Worker Image Runner Version

**Symptoms.** A `RunnerGroup` or `RunnerSet` reports `RunnerVersionTooOld` with reason `WorkerImageBelowMinimum` (`True`) or `WorkerImageVersionUnknown` (`Unknown`), and a `WorkerImageBelowMinimum` Warning event names both versions.
Jobs may still be running normally: the condition is a prediction about GitHub's enforcement, not a report that something already broke.

**What happened.** GitHub refuses to register a self-hosted runner below an enforced minimum version, `2.329.0` as of the 2026-06-12 changelog, and separately requires each new runner release be installed within 30 days of publication for the runner to keep executing jobs.
Every reconcile, the AGC reads the runner version off the effective worker image reference (the set's `workerImage`, else the AGC's `WORKER_IMAGE`, else the digest-pinned built-in default) and compares it to that floor.
It asks GitHub nothing, which is why the signal exists on a `ScaleSet` set: the scale-set protocol carries no runner version at session creation, so the listener there never sees the rejection that produces the `VersionTooOld` reason on the classic tier.

Only the reference is read, so a tag that is not a runner version reports `Unknown` rather than a verdict. `Unknown` is deliberately not `False`: a custom image is exactly where a stale runner hides, and reporting "current" for an image nothing has checked would be worse than saying so.

**A `True` verdict makes the set impaired**, so it rolls up into the gateway's `RunnerGroupsDegraded`/`RunnerSetsDegraded` condition. `Unknown` and `False` do not.

**Resolution.**

- **`WorkerImageBelowMinimum`**: build or pull a `workerImage` on runner `2.329.0` or later and update the spec.
  Prefer both a tag and a digest (`myrepo/runner:2.335.1@sha256:…`): the digest is what pins the image, and the tag is what makes the version checkable.
- **`WorkerImageVersionUnknown`**: either re-tag with the runner version the image ships, or read the version out of a worker pod directly.
  The injected wrapper logs it once per pod, from the runner's own dependency manifest rather than from the tag:

```bash
kubectl logs -n <namespace> <worker-pod> -c runner | grep "runner version"
# runner version detected version=2.335.1
```

`runner version not detected` in that log means the image does not carry `bin/Runner.Listener.deps.json` where the runner layout puts it: it is not `actions/runner`-derived, or the runner lives somewhere `RUNNER_HOME_DIR` does not point at.

```bash
# The verdict and its message for one set
kubectl get runnerset <name> -n <namespace> \
  -o jsonpath='{.status.conditions[?(@.type=="RunnerVersionTooOld")]}'
```

---

## Job-Lifecycle Events on a RunnerGroup / RunnerSet

**What this is.** Beyond `WorkerPodStuckPending` (above), the AGC records `Warning` Kubernetes Events on the owning `RunnerGroup` (`v1alpha1`) or `RunnerSet` (`v2alpha1`) when a job-lifecycle transition fails terminally (Q170).
They are the event-based companion to the always-present metrics and status conditions — surfacing the same incident in `kubectl describe`, `kubectl get events`, and any event watcher, without a Prometheus query.
Each `Reason` mirrors the corresponding metric name so you can correlate the two.

Events are recorded **on a transition / terminal outcome**, not on every reconcile or every requeue, so they do not spam.
(The cluster's event recorder additionally aggregates repeats of the same reason+message into one event with a count.)
An event is recorded on the owner's next reconcile, so it can trail the underlying metric by a few seconds; the metric is the real-time signal.

| Reason | Type | Meaning | Where to look next |
|---|---|---|---|
| `JobAcquisitionFailed` | Warning | A delivered job could not be acquired from GitHub (`acquirejob` failed); the job stays queued at GitHub for redelivery. | [AGC CrashLoopBackOff or Not Acquiring Jobs](#agc-crashloopbackoff-or-not-acquiring-jobs) |
| `RunnerVersionTooOld` | Warning | Session creation was rejected permanently because the runner version is too old for GitHub. Also sets the `RunnerVersionTooOld` condition. | [AGC CrashLoopBackOff or Not Acquiring Jobs](#agc-crashloopbackoff-or-not-acquiring-jobs) |
| `WorkerImageBelowMinimum` | Warning | The effective `workerImage` ships a runner below GitHub's enforced minimum, read at reconcile on both tiers and emitted once per transition, before GitHub rejects anything. Also sets the `RunnerVersionTooOld` condition. | [Worker Image Runner Version](#worker-image-runner-version) |
| `SessionUnauthorized` | Warning | Session creation was rejected as unauthorized — the agent credentials are invalid or revoked. Also sets the `Degraded` condition. | [GitHub App Secret Misconfiguration](#github-app-secret-misconfiguration) |
| `QuotaRetriesExhausted` | Warning | Worker pod creation was abandoned after exhausting the namespace `ResourceQuota` retry budget (`maxQuotaRetries`). | [Jobs Failing Due to Namespace ResourceQuota Exhaustion](#jobs-failing-due-to-namespace-resourcequota-exhaustion) |
| `WorkerPodCreateFailed` | Warning | The API server refused to create a worker pod (invalid name, admission webhook, pod-security policy). The note carries the API server's own message. No pod exists, so GitHub reports only that the runner lost communication. | ["Runner Lost Communication" and No Worker Pod Was Ever Created](#runner-lost-communication-and-no-worker-pod-was-ever-created) |
| `EvictionRetriesExhausted` | Warning | An evicted worker pod's auto-retry budget (`maxEvictionRetries`) is exhausted; a manual re-run is required. Emitted on both acquisition tiers, which share one per-run budget. | [Evicted Worker Pods Exhausting Retry Budget](#evicted-worker-pods-exhausting-retry-budget) · [Evicted Scale-Set Jobs Are Not Re-Run Automatically](#evicted-scale-set-jobs-are-not-re-run-automatically) |
| `EvictionRerunFailed` | Warning | A disrupted run's automatic re-run was never accepted by GitHub — refused past the 15-minute re-run window, or a terminal API error (Q503). The budget slot is spent; the named run needs a manual re-run. | [Evicted Worker Pods Exhausting Retry Budget](#evicted-worker-pods-exhausting-retry-budget) |
| `JobProvisionStalled` | Warning | A scale-set job cannot register its runner name (`generate-jitconfig` 409 that no retry cleared), so no worker can be created for it. The job is held and re-offered on a backoff. Also sets the advisory `JobProvisionStalled` condition, whose message names the job ids. Once per episode. | [Scale-Set Job Stranded by a Stale Runner Record](#scale-set-job-stranded-by-a-stale-runner-record-runner-name-409) |
| `WorkerCeilingReached` | Normal | Scale-set jobs are waiting because the set is already running as many workers as its spec allows; they are re-offered until capacity frees. Expected backpressure, hence Normal. Also sets the advisory `JobProvisionStalled` condition, whose message names the job ids. Once per episode. | [Scale-Set Jobs Waiting at the Worker Ceiling](#scale-set-jobs-waiting-at-the-worker-ceiling-workerceilingreached) |
| `AssignmentAbandoned` | Warning | The listener gave up on assigned jobs it was holding, because the scale set reported no assigned jobs at all on two consecutive readings — GitHub is no longer holding them, and never reported them complete. Each is a workflow run that will not run. Clears `JobProvisionStalled` and steps `…_jobs_abandoned_total`. | [Scale-Set Assignments Abandoned](#scale-set-assignments-abandoned-assignmentabandoned) |

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

**Symptoms.** The HPA for the proxy pool shows `TARGETS: cpu: <unknown>/60%` and the replica count does not increase under load.

`<unknown>` is common to the first and third causes below, so it does not tell them apart.
The `ScalingActive` condition's **reason** does, and unlike the condition's message text it is stable API surface.
Match on it:

```sh
kubectl get hpa -n <namespace> \
  -o custom-columns='NAME:.metadata.name,REASON:.status.conditions[?(@.type=="ScalingActive")].reason'
```

| `ScalingActive` reason | Cause |
|---|---|
| `FailedGetResourceMetric` | `resources.requests.cpu` unset (first cause below). The same reason covers a metrics-server that is absent or not yet serving, so check that first. |
| `AmbiguousSelector` | Two pools share a selector (third cause below). |

A real percentage in `TARGETS` means metric computation is working; go to the `ResourceQuota` cause instead.

**Likely cause.** `resources.requests.cpu` is unset or zero for proxy pods.
The Kubernetes Horizontal Pod Autoscaler (HPA) computes CPU utilization as `(current_cpu_usage / requested_cpu)`.
With no request there is no denominator, so the HPA emits `<unknown>` for the target metric and stops scaling entirely. `ScalingActive` goes `False` with reason `FailedGetResourceMetric`:

```text
the HPA was unable to compute the replica count: failed to get cpu utilization: unable to get metrics for resource cpu: no metrics returned from resource metrics API
```

A metrics-server that is absent or not yet serving produces the same reason with a different tail (`the server could not find the requested resource (get pods.metrics.k8s.io)`), which is why the diagnostics below check for it.
(Measured on Kubernetes v1.36.1, 2026-08-03: the first message against a pool declaring no `requests.cpu` with metrics-server v0.8.1 serving, the second on the same cluster before metrics-server was installed.)

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

Ensure `spec.proxy.resources.requests.cpu` is set to a non-zero value in the `ActionsGateway` spec.
The default is `10m`.
If you explicitly set `resources` without including `requests.cpu`, the whole `resources` block is replaced and defaults are lost — set all four sub-fields explicitly:

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

**Second likely cause: the namespace `ResourceQuota` won't admit the replicas the HPA wants.** The HPA computes utilization correctly but the proxy Deployment cannot create more pods because the platform-owned namespace `ResourceQuota` is the hard cap.
Under load the pool wedges below its target and the Deployment/ReplicaSet logs `FailedCreate ... exceeded quota` events instead of scaling out.

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

Resolve by **either** raising the platform-owned quota (`kubectl edit resourcequota -n <namespace> <quota-name>`) to admit the configured `maxReplicas`, **or** lowering `spec.proxy.maxReplicas` to fit.
Editing the quota's `.spec.hard` re-triggers reconciliation immediately; the conditions clear on the next reconcile.

**Third likely cause (mid-migration, GMC before `v1.3.0`): two proxy pools share a selector.** In a namespace running both a v1 inline pool and a v2 `EgressProxy` pool, **neither** pool scales, and both HPAs go `ScalingActive=False` with reason `AmbiguousSelector`.
Each names **its own** scale target's selector and lists every HPA in the conflict.
Below, the `EgressProxy` is named `ep1`:

```sh
kubectl get hpa -n <namespace> \
  -o custom-columns='NAME:.metadata.name,REASON:.status.conditions[?(@.type=="ScalingActive")].reason,MESSAGE:.status.conditions[?(@.type=="ScalingActive")].message'
```

```text
NAME                    REASON              MESSAGE
actions-gateway-proxy   AmbiguousSelector   pods by selector app=actions-gateway-proxy are controlled by multiple HPAs: [<namespace>/actions-gateway-proxy <namespace>/ep1-proxy]
ep1-proxy               AmbiguousSelector   pods by selector actions-gateway.com/egress-proxy=ep1,app=actions-gateway-proxy are controlled by multiple HPAs: [<namespace>/ep1-proxy <namespace>/actions-gateway-proxy]
```

Key any alerting on the **reason**, never the message: the selector differs per HPA, and the bracketed HPA list is unordered, and consecutive emissions swap it.
The controller also records `AmbiguousSelector` and `FailedComputeMetricsReplicas` Warning Events carrying the same text, so `kubectl describe hpa` shows both.
(Measured: Kubernetes v1.35.5 and v1.36.1, metrics-server v0.8.1, 2026-08-03; two Deployments whose pods both carry `app=actions-gateway-proxy`, one HPA each.
Identical wording on both versions.)

An HPA has no selector of its own — it resolves its scale target's — and refuses to act on pods a second HPA also controls.
The v2 pool used to stamp `app: actions-gateway-proxy`, the label v1's `Deployment` selector keys on, so each HPA saw the other's pods.
The same overlap put each pool's pods under both `PodDisruptionBudget`s (making them unevictable, so node drains hung) and made the two pools repel each other off every node.

```sh
# Both pools' pods answer to the v1 label on an affected install; only v1's should.
kubectl get pods -n <namespace> -l app=actions-gateway-proxy \
  -o custom-columns='NAME:.metadata.name,PROXY:.metadata.labels.actions-gateway\.com/egress-proxy'
```

**Resolution: upgrade the GMC.** A v2 pool no longer carries the v1 label, so both HPAs recover on their own.
The upgrade recreates that `EgressProxy`'s pool once — see [the upgrade note](upgrade.md#non-breaking-an-egressproxy-pools-pods-drop-the-app-actions-gateway-proxy-label-its-pool-is-recreated-once).
There is no in-place workaround: `Deployment.spec.selector` is immutable, so the overlap cannot be edited away.

**v2 (`EgressProxy`) note — bring-your-own autoscaler.** If the pool's `EgressProxy` sets `spec.managedAutoscaling: false`, there is **no managed HPA by design** — `kubectl get hpa` finding nothing named `<name>-proxy` is not a fault.
Scaling is owned by whatever you attached to the `<name>-proxy` Deployment (KEDA, VPA, a custom HPA), so debug that scaler instead; `spec.maxReplicas` and `spec.targetCPUUtilizationPercentage` are inert.
The quota conditions still work, but `ProxyQuotaPressure` measures headroom to the Deployment's *current desired replicas* (what your scaler asked for) rather than `maxReplicas`, and both conditions surface on the `EgressProxy`.
An external scale-to-zero is preserved and reported `Ready` (`0/0 proxy pods ready`) — while at zero, the tenant has no egress path, so make sure your scaler's floor matches your availability expectations.

---

## Proxy Tunnel Closed Mid-Stream — Idle or Lifetime Cap

**Symptoms.** A worker job logs a connection reset, `EOF`, or `broken pipe` from the GitHub SDK / `curl` / `git`, with no proxy `502` response.
The proxy pod itself is healthy and serving other tunnels.

**Likely cause.** The proxy enforces two per-tunnel deadlines on the CONNECT relay (M-18, 2026-05-31):

- **Idle timeout** — 5 minutes of no data in either direction.
  A long-poll against the GitHub API or a stalled SDK call hits this first.
- **Hard lifetime cap** — 6 hours absolute, regardless of activity.
  A continuous artifact stream or Twirp log relay that exceeds this is torn down even with traffic flowing.

These are not bugs.
They bound goroutine and file-descriptor exhaustion from slow or stuck clients.
The healthy case (an actively-used GitHub API call) completes in seconds and does not trip either cap.

**Diagnostics.**

The proxy serves `/metrics` over mutual TLS on `:8443` (not `:8081`, which now carries only the plaintext `/healthz` + `/readyz` probes).
Scraping requires the per-tenant scraper client certificate the GMC publishes — see [Metrics scrape returns a TLS / connection error](#metrics-scrape-returns-a-tls--connection-error) for how to fetch the bundle.
With the bundle written to `scraper.crt` / `scraper.key` / `metrics-ca.crt`:

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

For idle hits: examine the workflow step that stalls.
A workflow `sleep`-ing inside a long-running `curl --connect-timeout 0` or a misconfigured webhook receiver are typical causes.
The fix is usually in the workflow, not the proxy.

For lifetime-cap hits: split very long-running uploads or streams across multiple HTTP requests.
The 6h cap is a safety net for stuck connections; a legitimately-long single stream should be rare.

To change the defaults during an incident, patch the proxy Deployment with environment overrides — note that there is no env-var knob today; defaults are baked into the Server struct and require a code change to adjust.
File a Queue item if a tenant repeatedly hits either cap on a legitimate workload.

If the resets cluster around a proxy rollout rather than being spread across the pool's uptime, the cause is the shutdown drain, not these caps — see [Proxy Tunnel Cut During a Rollout](#proxy-tunnel-cut-during-a-rollout).

---

## Proxy Tunnel Cut During a Rollout

**Symptoms.** Worker jobs fail with a connection reset or `EOF` mid-request, and the failures line up with a proxy `Deployment` rollout, node drain, eviction, or scale-down rather than being spread evenly over time.
The terminating proxy pod logs:

```
WARN drain deadline expired; cutting in-flight CONNECT tunnels tunnels=3 drainTimeout=45s
```

**Cause.** On SIGTERM the proxy fails `/readyz` so the endpoint controller stops steering traffic to it, then runs a two-stage shutdown inside a single budget:

1. **Linger** — the CONNECT listener is deliberately held *open* for a short period.
   Endpoint removal is a control loop concurrent with SIGTERM, not a predecessor, so a kube-proxy that has not yet applied the removal is still sending new connections here; refusing them is the failure this stage prevents.
   It ends as soon as new connections stop arriving (a short quiescence interval), capped by `PROXY_SHUTDOWN_LINGER` (default 10s).
   A typical termination pays a couple of seconds, not the full ceiling.
2. **Drain** — the listener closes and the proxy waits for in-flight tunnels.

Both stages share `PROXY_SHUTDOWN_DRAIN_TIMEOUT` (default 45s) — the linger is spent *inside* that budget, not added to it — which fits inside the pod's `terminationGracePeriodSeconds: 60` with headroom. **Tunnels still open when the deadline expires are force-closed** — the alternative is holding the pod until the kubelet SIGKILLs it, which cuts them anyway and with no log line to show for it.

A tunnel carrying a long artifact upload or a GitHub long-poll can legitimately outlive 45s, so a small number of these warnings during a rollout is expected.
A large `tunnels=` count on every terminating pod is the signal to act.

> Proxy images built before this drain landed have **no** SIGTERM handler at all: the process is terminated outright and *every* live tunnel is cut, with no warning logged.
> Absence of the log line above on a terminating pod means the image predates the fix — upgrade it.

**Diagnostics.**

```sh
ns=<namespace>

# Count cut tunnels across the pool's recent terminations. Use --previous to
# read the log of the container instance that was replaced.
for p in $(kubectl get pods -n "$ns" -l actions-gateway/component=proxy \
    -o jsonpath='{.items[*].metadata.name}'); do
  kubectl logs -n "$ns" "$p" --previous 2>/dev/null |
    grep -F 'cutting in-flight CONNECT tunnels'
done

# Confirm the pod's grace period still exceeds the drain budget.
kubectl get deploy -n "$ns" actions-gateway-proxy \
  -o jsonpath='{.spec.template.spec.terminationGracePeriodSeconds}{"\n"}'
```

**Resolution.**

- **Prefer draining less often.** Roll the proxy pool during a quiet CI window; the drain bounds the damage, it does not eliminate it.
- **Raise the budget** if a tenant's legitimate traffic needs longer.
  Set `PROXY_SHUTDOWN_DRAIN_TIMEOUT` on the proxy container **and** raise `terminationGracePeriodSeconds` to at least the new budget plus headroom — raising the drain alone changes nothing, because the kubelet SIGKILLs the pod at the grace period regardless of what the process is still waiting for.
- **Do not raise it past the grace period.** The drain would then never complete on its own and every rollout would end in SIGKILL, which is strictly worse: the tunnels are cut either way and the warning log never gets written.

### Proxy pods on spot / preemptible nodes

The kubelet grants a terminating pod `min(terminationGracePeriodSeconds, remaining node-shutdown window)`, so on a node with a short reclamation notice the 60s grace period is truncated to whatever the node actually has:

| Where the proxy runs | Budget it actually gets |
|---|---|
| On-demand nodes, any voluntary drain | Full 60s |
| AWS EC2 Spot (2-minute notice) | 60s honoured if a handler drains in time |
| Azure Spot (30s notice) | Degraded; nothing drains without a handler |
| **GKE Spot** | **15s** — GKE's 30s node budget is split 15s ordinary / 15s critical |

Cluster autoscaler drains (`--max-graceful-termination-sec`, default 600s), `kubectl drain` (no timeout), and node pool upgrades are not affected — the voluntary paths honour the full grace period.
Full per-platform defaults, and the reason a delay is needed at all, are in [Node shutdown budgets](node-shutdown-budgets.md).

Within a truncated window the shutdown sequence still runs in the right order — it simply gets cut short — so the linger, which is deliberately spent inside the drain budget rather than ahead of it, does not starve the drain the way a fixed `preStop` sleep would.
If you are running proxy pools on aggressively reclaimed nodes and want every available second spent draining tunnels rather than waiting for endpoint convergence, shorten or disable the linger:

```sh
# Disable the endpoint-removal linger (negative duration). Accepts the race in
# exchange for spending the whole window on the tunnel drain.
kubectl set env deploy/actions-gateway-proxy -n <namespace> \
  PROXY_SHUTDOWN_LINGER=-1s
```

Setting it to a short positive value (`2s`) rather than disabling it keeps most of the benefit at a fraction of the cost.
Note the GMC owns this Deployment and will reconcile the env away; set it on the `EgressProxy`/`ActionsGateway` spec for a durable change.

> **Prefer keeping proxy pools off spot capacity.** The egress proxy is shared infrastructure for every worker in the tenant, and its IP is the tenant's egress identity: a reclaimed *worker* costs one CI job, which re-runs, while a reclaimed *proxy* cuts live egress for every job routed through it — and on a 15s budget the drain cannot get out of the way cleanly. `spec.scheduling` exists to pin the pool; use it to select on-demand nodes, and spend the spot savings on workers instead.
> This is a recommendation, not an enforced constraint — see [Node shutdown budgets](node-shutdown-budgets.md#recommendation-keep-proxy-pools-off-spot-capacity) for the full trade-off.

---

## Metrics scrape returns a TLS / connection error

**Symptoms.** Prometheus (or a manual `curl`) of a per-tenant proxy or AGC `/metrics` endpoint fails with one of:

- `remote error: tls: certificate required` / `bad certificate` — no client cert, or one signed by the wrong CA.
- `connection refused` on `:8081/metrics` — the metrics endpoint moved to `:8443` (mTLS); `:8081` now serves only `/healthz` + `/readyz`.
- `context deadline exceeded` / no route — the scraper namespace is not labelled `metrics: enabled`, so the NetworkPolicy drops the connection before the handshake.

**Cause.** The proxy and AGC serve `/metrics` over **mutual TLS** on `:8443` (Q69).
A scraper must (1) connect from a namespace labelled `metrics: enabled` and (2) present a client certificate signed by the per-tenant metrics CA the GMC issues.
Both halves are required.

**Resolution.**

1. Label the monitoring namespace so the NetworkPolicy admits it:
   ```sh
   kubectl label namespace <prometheus-namespace> metrics=enabled
   ```
2. Fetch the scraper client bundle the GMC publishes in each tenant namespace and point the scrape at `:8443` with `scheme: https`:
   ```sh
   ns=<tenant-namespace>
   kubectl get secret actions-gateway-metrics-client -n "$ns" -o jsonpath='{.data.tls\.crt}' | base64 -d > scraper.crt
   kubectl get secret actions-gateway-metrics-client -n "$ns" -o jsonpath='{.data.tls\.key}' | base64 -d > scraper.key
   kubectl get secret actions-gateway-metrics-client -n "$ns" -o jsonpath='{.data.ca\.crt}'  | base64 -d > metrics-ca.crt
   curl -s --cert scraper.crt --key scraper.key --cacert metrics-ca.crt \
     "https://actions-gateway-proxy.$ns.svc:8443/metrics" | head
   ```
   Delete the extracted key file when finished.
   For a `ServiceMonitor`, mount the bundle and reference it from `tlsConfig` (`cert`/`keySecret`/`ca`).
3. If the cert is rejected after a CA rotation, the GMC re-issues the whole bundle ~30 days before expiry but pods read certs at startup — restart the proxy/AGC pods (and re-fetch the client bundle) after a rotation.

---

## Jobs Targeting One of a Runner Set's Labels Never Start (`RunnerLabelsIncomplete`)

**Symptoms.** A `RunnerSet` is `Ready=True` with an active listener, and jobs whose `runs-on` names its **first** label run normally, but jobs naming one of the later labels queue at GitHub forever and no worker pod is ever created.
The set carries:

```sh
kubectl get runnerset <name> -n <ns> \
  -o jsonpath='{range .status.conditions[?(@.type=="RunnerLabelsIncomplete")]}{.status} {.reason}: {.message}{"\n"}{end}'
```

```
True LabelsNotRegistered: scale set "linux" does not carry runnerLabel(s) [gpu]
(registered: [linux]); jobs whose runs-on names them stay queued at GitHub. ...
```

A `Warning` Event with reason `RunnerLabelsNotRegistered` is recorded on the set when the condition first goes `True`.

**Likely cause.** The scale set at GitHub carries fewer labels than the runner set declares.
Two ways in, and the condition message says which labels are affected either way:

- **A label was appended to a live runner set.** The AGC finds an existing scale set by its name, the set's first `runnerLabel`, and reuses it untouched, so labels added afterwards are never registered.
  Nothing errors; the new label simply matches nothing.
- **GitHub Enterprise Server below 3.21.** Multiple labels per scale set need `DistributedTask.AllowRunnerScaleSetCustomLabels`, which is off by default on 3.18–3.20 and on by default from 3.21.
  With it off the appliance keeps only the name label and **discards the rest without an error**, so the create looks entirely successful.

**Resolution.**

- **On GHES 3.18–3.20**, have a site admin enable the flag, then recreate the runner set:

    ```sh
    ghe-actions-console -s actions
    # In the LightRail prompt:
    Set-FeatureFlag -FeatureName DistributedTask.AllowRunnerScaleSetCustomLabels -State On
    ```

    Upstream documents the flag for 3.18 and later only, so on 3.17 and earlier treat multi-label as unavailable and declare one label per set until the appliance is upgraded.

- **Otherwise, give the set a new scale set.** Labels are registered when the scale set is created and are not reconciled afterwards, and the AGC finds the scale set by name.
  So neither editing `spec.runnerLabels` nor deleting and re-creating the `RunnerSet` under the same first label will add them: the second re-adopts the same scale set with the same old labels.

    Two ways out, and both cost the old scale set:

    - **Change `runnerLabels[0]` as well as adding the label.** A new name means a new scale set, registered with the full list.
      Workflows naming the old first label must be updated, and the orphaned scale set stays at GitHub until removed there.
    - **Delete the scale set at GitHub first**, then let the set re-register.
      Deleting a `RunnerSet` does not delete its scale set, so this is a deliberate step in the GitHub UI or API.

    Either way, drain the set first: jobs already assigned to the old scale set are not carried over.

**Note on the first label.** It names the scale set, which makes it the set's identity.
Reordering `runnerLabels` renames the scale set, leaving the old one orphaned at GitHub with any queued jobs still pointed at it.
Append rather than prepend.

**Why it does not gate `Ready`.** The set is still serving every job that targets the labels which *did* register, so it is a configuration mismatch rather than an outage.
It is advisory, and deliberately not rolled into the gateway's `RunnerSetsDegraded` summary.

---

## RateLimited Condition on ActionsGateway

**Symptoms.** `kubectl get actionsgateway` shows a `RateLimited=True` condition. `actions_gateway_active_sessions` is at or near the per-installation budget.

**Likely cause.** The GitHub App installation's API budget (15,000 `GET /message` requests/hour) is exhausted.
This occurs when the sum of `maxListeners` across all RunnerGroups simultaneously bursts to their ceiling for a sustained period.

**SLO threshold.** A `RateLimited` condition lasting more than 1 minute during non-peak hours indicates the installation is over budget.
Durations exceeding 10 minutes during business hours should page on-call.

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
- If a burst is temporary and below 10 minutes: no action required, the condition will clear as the burst subsides. `RateLimited=True` (reason `SustainedRateLimit`) is set only after `GET /message` has been answered `429` for over 10 minutes, and clears to `RateLimited=False` (reason `PollingHealthy`) on the first successful poll once the budget recovers — you do not need to restart the AGC to clear a stale condition.
- If `maxListeners` values are set higher than needed, reduce them.
- If the tenant's RunnerGroup count × `maxListeners` sustainably exceeds the installation budget, shard to a second `ActionsGateway` CR with a new GitHub App installation.
  See [Appendix E §E.6](../design/appendix-e-capacity-planning.md#e6-when-to-shard-across-installations).

---

## GitHub App Secret Misconfiguration

**Symptoms.** AGC logs show errors like `failed to create installation token`, `private key: RSA key parse error`, or `401 Unauthorized`.
The `ActionsGateway` condition `AGCAvailable=False` with reason `CredentialError`.
When the AGC cannot obtain an installation token while reconciling a RunnerGroup, that group also reports `CredentialUnavailable=True` (reason `TokenUnavailable`) in its status — surfacing the failure in `kubectl get runnergroup`/`describe`, not only as a `TokenUnavailable` Event.
The condition clears (`CredentialUnavailable=False`, reason `CredentialAvailable`) on the next reconcile once a token is obtained.
Read it with:

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

**Resolution.** Re-create the Secret with correct values.
To trigger a rolling update on the AGC Deployment after fixing the Secret, change `gitHubAppRef.name` in the `ActionsGateway` spec to reference the new Secret name (the GMC will roll the AGC Deployment automatically) or manually restart the Deployment:

```sh
kubectl rollout restart deploy/actions-gateway-controller -n <namespace>
```

On a GMC older than the Q552 fix that manual restart is a silent no-op — prefer the `gitHubAppRef.name` change, or see [`kubectl rollout restart` of a Managed Deployment Reports Success but Nothing Restarts](#kubectl-rollout-restart-of-a-managed-deployment-reports-success-but-nothing-restarts).

See [Getting Started — Rotating GitHub App Credentials](../getting-started.md#rotating-github-app-credentials) for the full rotation procedure.

---

## Token Refresh Errors Spiking

**Symptoms.** `actions_gateway_token_refresh_errors_total` is increasing.
GitHub App installation tokens expire after one hour; if refresh fails, new sessions cannot be established once the token expires.

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
- If GitHub is temporarily unavailable: the AGC's exponential back-off retry (5s → 60s cap) will recover automatically.
  Monitor until the error rate returns to zero.
- If the private key was rotated: update the Secret.
  See [Getting Started — Rotating GitHub App Credentials](../getting-started.md#rotating-github-app-credentials).
- If the proxy is unreachable: see [Proxy Pool Not Scaling](#proxy-pool-not-scaling) and the network connectivity section below.

**SLO.** Token refresh errors should stay below 1 per hour per tenant.
Above this rate, begin investigating immediately.
In-flight sessions will fail at the next reconnection once the token expires (~1 hour).

---

## RenewJob Failures Rising

**Symptoms.** `actions_gateway_renew_job_errors_total` is increasing.
Jobs may start being cancelled by GitHub before completion.
On current versions, a **definitively lost** lock also increments `actions_gateway_renew_job_teardowns_total` and the AGC self-cancels the worker (see the self-cancel note below).

**Likely causes.**
- Network connectivity issues between the AGC and GitHub (via proxy).
- GitHub API is temporarily unavailable.
- The runner job lock window expired before the renewer could refresh (AGC was slow or restarting).
- **AGC versions before the Q247 fix** renewed with the wrong job identifier (the broker envelope's `MessageID` instead of the job's `RunnerRequestID`), so *every* renewal returned an error and **no** lock was ever refreshed.
  The tell is a *sustained, non-transient* error rate that tracks the acquired-job rate — every job that outlives GitHub's lock window (roughly one renewal interval) is then recycled and redelivered to a sibling session, so you also see duplicate worker pods for one job and completed jobs that fail with `conclusion: failure`, no failed step, no logs, and a `TaskOrchestrationJobNotFoundException` at `CompleteJobAsync`.
  Long jobs expose it; sub-window jobs finish before the lock lapses.
- **AGC versions before the Q247 *residual* fix** ran each `RenewJob` call inline with no per-call timeout.
  Under heavy worker-node load (CPU/egress saturation) a single `/renewjob` call can black-hole — the connection is accepted but never answered — and, because the next renewal cannot start until the call returns, that one hung call starves *every* subsequent renewal.
  The tell is a long job failing at *exactly* GitHub's ~10-minute lock window (the initial lock TTL, never refreshed) with a **single** worker pod that keeps running past the cutoff — distinct from the wrong-jobId signature above, which produces *duplicate* pods.
  Fixed versions bound each renewal with the control-plane timeout, so a hung call aborts (one `renew_job_errors_total` increment) and the loop renews on schedule.
- **AGC versions before the Q247 *auth* fix** renewed with the broker session (OAuth) token instead of the job-scoped token GitHub issues in the `acquirejob` response (the `SystemVssConnection` endpoint's `AccessToken`).
  GitHub accepts the session token to *claim* a job but rejects it for *renewing* that job's lock, so *every* renewal returns **`401 {"source":"actions-run-service","errorMessage":"Not authorized for this job"}`** from the very first call.
  The tell is identical to the residual signature — a long job failing at *exactly* the ~10-minute lock window with a **single** worker pod — but the AGC log shows every `RenewJob` returning that specific 401 (not a timeout, not a wrong jobId).
  Fixed versions present the job-scoped token, so renewals return 200 and the lock is refreshed.

**Self-cancel on a definitively lost lock (current behavior, Q254).** On a lock the renewer can prove is *unrecoverably* lost, the AGC no longer lets the worker run on to completion as an orphan pod.
Two triggers:

- **Definitive job-gone.** The run service answers `/renewjob` with `404`/`410` (the job's lock no longer exists — GitHub recycled or reassigned it).
  A single such response is enough.
- **Sustained failure run.** Renewal fails for **5 consecutive** intervals (~5 min at the default 60s cadence) — a network partition or a persistently unreachable run service.
  This is well past any single transient blip, and it tears down before the ~10-minute lock TTL lapses.

On either trigger the AGC cancels the job's context, logs a distinct error line (`job lock definitively lost; cancelling worker …`), and increments `actions_gateway_renew_job_teardowns_total{reason="job_not_found"|"consecutive_failures"}`.
It then **deletes the worker pod**, gracefully, so the runner gets its termination grace to report and the slot and node are freed.
Tearing the orphan down *before* the lock lapses closes the residual window in which GitHub could recycle the job and redeliver it to a sibling session (a duplicate worker pod for one job).
A *single/transient* renewal failure still stays non-fatal and is retried.

The reclaim is counted on `actions_gateway_worker_pods_reaped_total{reason="job_abandoned"}`, so `renew_job_teardowns_total` and that series should track each other. **Gateway versions before Q501 cancelled the job context but never deleted the pod**, so a torn-down worker kept running its (already reassigned) job until the kubelet enforced `maxWorkerLifetime` — 12 hours by default.
The tell on an affected version is `renew_job_teardowns_total` rising with no matching `job_abandoned` reaps and worker pods that stay `Running` long after their teardown line; the fix is the upgrade.
An **AGC restart or rollout does not** delete live workers — only a per-job teardown does.

**Diagnostics.**

```sh
# Check recent error rate
# Metric: rate(actions_gateway_renew_job_errors_total[5m])

# Check AGC logs for renewal errors and job IDs
kubectl logs -n <namespace> deploy/actions-gateway-controller | grep "renewjob"

# Definitive-loss teardowns (worker self-cancelled), split by reason
# Metric: sum by (reason) (rate(actions_gateway_renew_job_teardowns_total[15m]))
kubectl logs -n <namespace> deploy/actions-gateway-controller | grep "job lock definitively lost"

# Confirm the proxy pool is healthy
kubectl get pods -n <namespace> -l app=actions-gateway-proxy
```

**Resolution.**
- **Sustained errors on every job (the Q247 signature above): upgrade** to a gateway version with the Q247 fix, which renews by `RunnerRequestID`.
  On affected versions no renewal succeeds, so there is no interim mitigation short of the upgrade — jobs longer than the lock window keep failing.
- **A long job failing at exactly the ~10-minute lock window with a single (not duplicate) worker pod (the Q247 residual signature above): upgrade** to a gateway version that bounds each renewal call with the control-plane timeout.
  Reducing worker-node CPU/egress pressure (which is what makes a renewal call black-hole) lowers the odds on affected versions but is not a reliable mitigation — the upgrade is the fix.
- **Every renewal returning `401 "Not authorized for this job"` from the first call (the Q247 auth signature above): upgrade** to a gateway version that renews with the job-scoped token from the `acquirejob` response.
  On affected versions no renewal is authorized, so there is no interim mitigation short of the upgrade — jobs longer than the lock window keep failing.
- Transient GitHub API errors: the renewer retries; monitor until the rate returns to zero.
- Proxy pool unhealthy: fix the proxy pool (see [Proxy Pool Not Scaling](#proxy-pool-not-scaling)).
- If the AGC restarted mid-job: jobs whose lock expired will have been cancelled by GitHub.
  These require manual re-run.
  Check `actions_gateway_eviction_retries_exhausted_total` for any jobs that were also evicted.
- **`actions_gateway_renew_job_teardowns_total` rising (a self-cancel, Q254 behavior above):** the worker was torn down and its pod reclaimed because the lock was definitively lost — this is the AGC *avoiding* an orphan pod, not a new fault.
  Investigate the *underlying* cause via the split reason: `reason="job_not_found"` means GitHub reassigned/recycled the job (often downstream of a lock that already lapsed for one of the Q247 reasons, or genuine cancellation); `reason="consecutive_failures"` means renewal was unreachable for ~5 min — treat it like a sustained error rate above (proxy/egress or GitHub reachability).
  The affected job is re-run by GitHub on the sibling session that picks it up.

Each `renewjob` error is a warning, not an immediate job failure — GitHub grants ~10 minutes per renewal window.
A single *transient* error on a long-running job is rarely fatal; a *sustained* error on every job is the Q247 signature above, not a transient blip.
When a lock is *definitively* lost, current versions self-cancel the worker and delete its pod (Q254/Q501 behavior above) rather than orphaning it.

---

## Sessions Stuck in 401/EOF GetMessage Loops (Tenant Throughput Decays to Zero)

**Symptoms.** On gateway versions without the Q114 self-heal (≤ the M4 validation build):
- AGC logs fill with repeating `GetMessage error ... decode response: EOF` and later `broker: unauthorized (HTTP 401)` lines for the same session, backing off forever.
- The repo/org runner list (`gh api .../actions/runners`) loses one runner after each completed job, and the registrations never come back.
- `RunnerGroup` `status.activeSessions` decays over time; after roughly `maxListeners` completed jobs, queued workflow jobs wait forever even though the AGC pod is healthy.

**Cause.** GitHub deletes a JIT-registered runner record once it acquires a job (single-use runners).
Pre-fix AGC versions keep polling the dead session with the dead agent's credentials instead of re-registering, so every completed job permanently burns one listener slot ([M4 §12, bug 2](../plan/milestone-4.md#12-live-multi-tenant-validation-evidence-2026-06-1112)).

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
- **Upgrade** to a gateway version with the Q114 self-heal.
  Fixed versions re-register each agent after every job (`actions_gateway_agent_recycles_total{trigger="post_job"}`) and heal stale sessions discovered after a restart (`trigger="stale_session"` / `"startup"`); no per-job operator action is needed.
- **Interim manual recovery on pre-fix versions:** delete the RunnerGroup's agent Secrets and restart the AGC so it registers a fresh pool:

  ```sh
  kubectl delete secret -n <namespace> -l actions-gateway/runner-group=<group>
  kubectl rollout restart deploy/actions-gateway-controller -n <namespace>
  ```

  On a GMC older than the Q552 fix that restart is a silent no-op — see [`kubectl rollout restart` of a Managed Deployment Reports Success but Nothing Restarts](#kubectl-rollout-restart-of-a-managed-deployment-reports-success-but-nothing-restarts).

  Expect `409 Already exists` registration errors for any agent that never ran a job — its record survives server-side under an ID the AGC no longer knows.
  Delete the survivor from GitHub first: find its ID with `gh api '.../actions/runners?name=<group>-<index>'`, then `gh api -X DELETE .../actions/runners/<id>`.
  Fixed versions resolve this 409 automatically.

**On fixed versions,** a sustained rise of `actions_gateway_agent_recycle_errors_total` means the AGC cannot re-register agents (registration API unreachable, installation token failures, or revoked GitHub App runner-administration permission) — listener capacity shrinks until recycles succeed.
Check AGC logs for `recycle` errors and verify the App's runner permissions.

---

## Concurrent Job Burst Serializes to ~1 Worker (Recycle Blocked on a Still-Running Runner)

**Symptoms.** Each job runs green *individually*, but bursting a whole CI matrix onto the gateway at once serializes to roughly one running worker even with ample node room (nodes well under capacity, zero `Pending` pods).
Queued jobs sit unstarted until GitHub cancels them at its ~15-minute unstarted-job timeout; an already-running job whose token is invalidated dies with `RenewJob 401 "Not authorized for this job"`.

**Cause.** After a single-use JIT runner completes a job, GitHub auto-removes the ephemeral runner record — but for a few to tens of seconds it still reports the runner as running and answers a delete with `422 "Runner … is currently running a job and cannot be deleted"`.
Because the AGC re-registers each agent under a **stable name**, the lingering record also makes the re-registration conflict (`409`).
Under a burst, many agents recycle at once and hit this window together.
On gateway versions before the Q259 fix the AGC treated the `422` as fatal: the post-job recycle failed, the listener goroutine exited, and a non-permanent replacement listener is not restarted — so every completed job permanently dropped a polling slot until only the permanent baseline remained.
GitHub then had one online runner to dispatch to, so it dispatched ~1 job at a time.

**Diagnostics.**

```sh
# Recycle errors climbing in lockstep with a burst (pre-fix: fatal 422s)
kubectl logs -n <namespace> deploy/actions-gateway-controller | grep -iE "currently running|recycle"

# Metric: actions_gateway_agent_recycle_errors_total — spikes during the burst on pre-fix versions
# Metric: actions_gateway_active_sessions             — collapses toward 1 as replacements exit
```

**Resolution.**
- **Upgrade** to a gateway version with the Q259 fix.
  Fixed versions treat the `422 "currently running"` as transient: `Pool.Recycle` retries the re-registration with a bounded, jittered backoff (waiting for GitHub to release the just-consumed runner) instead of failing, so the listener keeps its polling slot and concurrency is sustained.
  A recycle that finally succeeds after the wait increments `actions_gateway_agent_recycles_total{trigger="post_job"}` as usual.
- **On fixed versions,** `actions_gateway_agent_recycle_errors_total` still rises only if a runner *never* releases within the retry bound — a genuine fault (registration API unreachable, or the runner is truly wedged running server-side), not the normal post-job race.
  Investigate as in the section above.

**Related seam — the freshly recycled record's token exchange (Q267).** A recycle that *does* re-register successfully then exchanges the new record's client credential for a broker OAuth token.
For a brief window between `generate-jitconfig` creating the record and GitHub's OAuth service recognizing it, that exchange returns a transient `400 "Registration … was not found"`.
On versions before the Q267 fix this 400 was fatal on the recycle path — the listener exited and churned yet another record, and at a **wide** `maxListeners` under sustained burst the exits multiplied stale records and held the *online* pool near zero even though `agent_recycles_total` kept climbing.
The tell is AGC logs showing `broker token exchange rejected … "Registration … was not found"` recurring while `active_sessions` stays low.
Fixed versions ride it out with a bounded, jittered retry of the **same** fresh credential (no re-registration), counted by `actions_gateway_broker_token_propagation_retries_total`; a listener stays online through the propagation lag.
A *sustained* rate on that counter is expected during wide-pool bursts and is benign; investigate only if it climbs alongside `agent_recycle_errors_total` or `active_sessions` fails to recover after the burst drains.

> **If the burst still serializes to ~1 worker after the Q259 fix, see the next section (Q260) — a distinct duplicate-acquisition cause.**

---

## Concurrent Job Burst Serializes to ~1 Worker (Duplicate Job Acquisition)

**Symptoms.** As above, a whole CI matrix bursted onto the gateway serializes to roughly one running worker — but this variant persists *even on a gateway with the Q259 recycle fix*.
The distinguishing tell is in the AGC logs: several sessions of the same RunnerGroup (distinct `agentIndex` / `sessionId`) all fail provisioning the **same** job with `provisioner: create Secret job-<planid>: secrets "…" already exists`.
In GitHub's runner list the losing runners show `busy` but `offline` with **no** worker pod; their sessions then die.
Remaining jobs sit `in_progress` (assigned to the now-dead duplicate runners) until GitHub's ~15-minute unstarted-job timeout.

**Cause.** Under a burst, GitHub's broker fans one job out to several sibling long-poll sessions of one RunnerGroup — as separate `RunnerJobRequest` messages with **distinct** `RunnerRequestID`s but one shared **plan ID**.
On gateway versions before the Q260 fix, every recipient independently ran `acquirejob` — succeeding and marking its runner **busy** — and then entered the provisioner, where the per-job worker Secret name is derived from the job's plan ID.
The first session created the Secret; the rest collided (`already exists`), failed provisioning, and died *with their runner slot already consumed*.
Net effect: one worker runs the job while the other slots are burned, collapsing the pool to the baseline listener.
This is distinct from the Q259 post-job recycle churn (which may still be visible as a secondary `422 "currently running"` signal).

A second, **late-redelivery** variant collides on the worker **Pod** rather than the Secret: `provisioner: create Pod runner-<group>-<planid>: pods "…" already exists`.
Here the winning session already ran the job to completion, but its terminal worker pod lingers for `completedPodTTL` before the reaper GCs it.
A gateway version that freed the plan-ID claim on *completion* (rather than on pod GC) would let a late GitHub redelivery of that same plan ID pass the claim gate, re-provision, and collide on the winner's still-present Completed pod.

**Diagnostics.**

```sh
# Multiple sessions provisioning the SAME job (the duplicate-acquisition signature)
# — the Secret variant (burst) and the Pod variant (late redelivery)
kubectl logs -n <namespace> deploy/actions-gateway-controller | grep -iE "create (Secret|Pod).*already exists|duplicate job delivery"

# Metric: actions_gateway_jobs_duplicate_delivery_total — on fixed versions, this
#   rises (deliveries safely deduplicated) INSTEAD of runner slots being burned.
# Metric: actions_gateway_active_sessions — collapses toward 1 on pre-fix versions.
```

**Resolution.**
- **Upgrade** to a gateway version with the Q260 fix.
  Fixed versions dedup a job across the RunnerGroup's sibling sessions on its **plan ID** — the identity that collapses across the fan-out and names the worker Secret.
  Because the plan ID is only returned by `acquirejob`, a sibling still acquires, but then finds the plan ID already claimed by another session in the same AGC and **skips provisioning**, recycling its consumed runner back online (slot reclaimed) instead of colliding on the shared `job-<planid>` Secret.
  Each such skip increments `actions_gateway_jobs_duplicate_delivery_total`; a steady low rate of that counter during bursts is the fix working as intended.
  (The first Q260 fix keyed on `RunnerRequestID` before `acquirejob`, but siblings get distinct request ids, so it did not collapse the fan-out — upgrade past it.)
- **For the late-redelivery Pod variant,** the same fixed versions hold the plan-ID claim for `completedPodTTL` *past* job completion — until the winner's terminal worker pod has been reaped — so a late redelivery is deduped (counted on `actions_gateway_jobs_duplicate_delivery_total`) rather than colliding on the lingering pod.
  No operator action; a lower `completedPodTTL` shortens both the retained pod and the claim linger together.
- **No operator action** is needed on fixed versions — the dedup is automatic and per-RunnerGroup.
  If the burst still serializes with `jobs_duplicate_delivery_total` flat and `jobs_acquired_total` not climbing, the bottleneck is elsewhere (worker-node capacity, namespace `ResourceQuota`, or the Q259 recycle path) — work through those sections.

**Known limitation — redelivery accounting.** The dedup gate is *post-*`acquirejob` (the plan ID is only known then), so a deduplicated sibling has *already* claimed its own per-delivery job assignment at GitHub before it skips provisioning.
Left untouched, GitHub still expects a runner to start *that* assignment and cancels the whole job at its ~15-minute unstarted-job timeout — even after the winning sibling ran the job to completion.
The tell: a job whose pod logged `Job completed` with no renewal errors is nonetheless reported `cancelled` on GitHub.
The mitigation (`AGC_FANOUT_COMPLETION`, Q260 Option A) is **on by default**: when the winner's job finishes, it fans a `completejob` out to *every* deduped sibling delivery — keyed on each sibling's own delivery job ID, with the winner's pod-phase-proxy result (a Failed pod → `failed`, else `succeeded`) — and a late redelivery arriving during the linger window is resolved with the same result; all tracked by `actions_gateway_abandoned_delivery_completions_total`.
The dogfood re-route #5 experiment (2026-07-04) live-confirmed it: the run service's completion is **per-delivery, not plan-ID-scoped** — `completejob` on a sibling's own job ID resolves only that assignment (returns OK), while the winner's own delivery still carries the real workflow result reported by its runner binary, so a green sibling proxy cannot mask a red workflow.
Previously-wedged concurrent jobs concluded green, the recycle 422 cleared per job on winner completion, and no job cancelled at the ~15-minute timeout.
Opt out with `AGC_FANOUT_COMPLETION=false`.
See [`docs/plan/q260-fanout-completion-reconciliation.md`](../plan/archive/q260-fanout-completion-reconciliation.md).

**Related failure mode — deduped-loser slot stranding (Q266).** Because a deduped sibling already ran `acquirejob`, GitHub holds *its* runner as assigned to the job.
That runner's recycle therefore `422`s ("runner is currently running a job and cannot be deleted") for the **winner's entire runtime** — the loser's `422` only clears when the winner fans `completejob` out to that delivery on completion (above).
On gateway versions before Q266 the loser recycled *eagerly*, blew through the bounded recycle backoff (which is sized for the seconds-long Q259 post-job race, not a whole job runtime), gave up, and **exited the listener** — and a non-permanent replacement is never restarted, so under sustained fan-out burst enough losers stranded and exited to collapse the pool (observed 2/8 online at dogfood re-route #5).
Fixed versions **defer** each loser's recycle until its winner concludes, holding the slot (but freeing its worker-capacity reservation, since the loser provisions no pod) instead of recycling into a guaranteed `422`; the loser then recycles in place and resumes polling.
Each defer increments `actions_gateway_fanout_loser_recycle_deferred_total` with `outcome="winner_concluded"`.
A sustained `outcome="fallback_timeout"` rate means winners are not concluding within the ~15-minute bound (a stuck-winner class — investigate the winners' pods/renewals), not a loser problem.
Requires `AGC_FANOUT_COMPLETION` enabled (the default) — with it off, losers fall back to the eager-recycle path and the collapse can recur.

**The durable fix for all of the above is to stop using Classic.** Every failure mode in this section is a consequence of the classic broker's many-acquirers model: `acquirejob` marks the job `in_progress` at GitHub and stamps the runner name *before* the gateway decides whether it can provision a worker.
Any job it then declines to provision is orphaned (`in_progress`, a runner name, **zero steps**) until GitHub's ~10-minute lock-lapse or ~15-minute unstarted-job timeout ends it.
The mitigations above bound that behaviour; they do not remove it.
Measured on the project's own dogfood tenant (2026-07-25, a 6-job CI matrix bursted across 18 runs): **85 jobs acquired, 16 worker pods, 69 orphaned, 81%.**

The `ScaleSet` acquisition protocol (the default since `v1.1.0`, and the only protocol in `v2beta1`) is single-acquirer: one listener holds the scale set's message queue, and GitHub never assigns beyond the capacity the gateway advertises, so there is no fan-out to dedup and nothing to orphan.
Side-by-side on the same cluster and workload it ran 7/7 jobs green against Classic's 2/7.
If you are on Classic only because a runner set matches multiple labels, split it into one single-label `ScaleSet` runner set per `runs-on` target.
That is the supported migration, and Classic is deprecated.
Note `spec.acquisitionProtocol` is **immutable**: delete and recreate the runner set rather than patching it.

---

## Network Connectivity Failures

**Symptoms.** The AGC cannot reach GitHub through the proxy.
Logs show `connection refused`, `dial tcp: i/o timeout`, or `proxy: no response from proxy`.

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
- If the `NetworkPolicy` egress rules are stale: trigger a manual refresh by temporarily setting `spec.proxy.managedNetworkPolicy: false` and back to `true`, or wait for the 24-hour automatic refresh cycle.
  Check the GitHub meta API for current IP ranges: `curl https://api.github.com/meta | jq .actions`.
- If an internal destination is being sent through the proxy: add it to `spec.proxy.noProxyCIDRs` (v1) / the `EgressProxy`'s `spec.noProxyCIDRs` (v2).
  The cluster-internal exemptions — including the API server — are appended automatically, so a missing service CIDR is not the cause; see [AGC Crash-Loops Dialling the API Server Through the Egress Proxy](#agc-crash-loops-dialling-the-api-server-through-the-egress-proxy) and the `noProxyCIDRs` field documentation in [§3.1](../design/03-api-contracts.md#31-kubernetes-crd-schemas).

---

## AGC Crash-Loops Dialling the API Server Through the Egress Proxy

**Symptoms.** A **proxied** tenant's AGC never reaches Ready and `CrashLoopBackOff`s at startup.
The log shows a Kubernetes API call — not a GitHub call — failing at the proxy, addressed to an **IP**, not a hostname:

```
detect actions-gateway.com/v2alpha1 RunnerSet CRD: … Get "https://34.118.224.1:443/api":
proxyconnect tcp: tls: failed to verify certificate:
x509: certificate signed by unknown authority
```

Two things distinguish it from [Runners Never Appear Online — AGC `unknown authority` Through the Egress Proxy](#runners-never-appear-online--agc-unknown-authority-through-the-egress-proxy), which produces a near-identical TLS error: the destination is the API server's ClusterIP rather than `api.github.com`, and the AGC dies during startup instead of running with failing registrations.
Direct-egress tenants never hit it, because `NO_PROXY` is not consulted when no proxy is set.

**Cause.** The AGC's Kubernetes client dials the API server by the ClusterIP in `KUBERNETES_SERVICE_HOST`, never by DNS name, so the API server must be exempted from `NO_PROXY` **by address**.
Before Q465 the generated `NO_PROXY` exempted the fixed range `10.96.0.0/12` — the kind/kubeadm Service CIDR.
That range is wrong on every managed distribution (EKS `172.20.0.0/16`, AKS `10.0.0.0/16`, GKE provider-assigned), so on those clusters the AGC CONNECTed to the API server through the tenant's egress proxy and could not verify the proxy's self-signed CA.

**Applies to.** GMC releases before the Q465 fix, on any cluster whose API server ClusterIP falls outside `10.96.0.0/12` — that is, every managed Kubernetes offering.
Current releases derive the exemption from the cluster's own `KUBERNETES_SERVICE_HOST`, so no configuration is needed on any distribution.

**Diagnostics.**

```sh
# 1. This cluster's API server ClusterIP — the address the AGC will dial.
kubectl get svc kubernetes -n default -o jsonpath='{.spec.clusterIP}{"\n"}'

# 2. The NO_PROXY the GMC generated for the tenant's AGC. It must contain the
#    address from step 1 (distroless image — read the spec, not the process).
kubectl get deploy -n <namespace> -l app=actions-gateway-controller \
  -o jsonpath='{range .items[0].spec.template.spec.containers[0].env[?(@.name=="NO_PROXY")]}{.value}{"\n"}{end}'
```

**Resolution.**

- **Upgrade the GMC** to a release carrying the Q465 fix and let the tenant's AGC Deployment be re-reconciled (`kubectl rollout restart deploy -n <namespace> <agc>` if it does not roll on its own).
  The exemption then follows the cluster.
- **Workaround on an older GMC**: name the API server's ClusterIP in the tenant's `noProxyCIDRs` (v1 `spec.proxy.noProxyCIDRs`, v2 `EgressProxy.spec.noProxyCIDRs`) — the single address from step 1, or that cluster's Service CIDR if you prefer (`kubectl cluster-info dump | grep -m1 service-cluster-ip-range`).
  Prefer the single address: every entry bypasses the tenant's egress proxy, and a CIDR exempts hosts you did not name.
- If `NO_PROXY` shows the literal `$(KUBERNETES_SERVICE_HOST)` unexpanded inside the **running container**, the GMC was running outside the cluster when it composed the Deployment and the kubelet did not expand it.
  Run the GMC in-cluster (the supported deployment) and re-reconcile.

---

## AGC Cannot Reach the Kubernetes API Server (NetworkPolicy + post-DNAT port mismatch)

**Symptoms.** AGC logs show `dial tcp 10.96.0.1:443: i/o timeout` (or similar) when calling the K8s API server.
The `actions-gateway-controller` NetworkPolicy *appears* to allow port 443, yet the connection is silently dropped.
Most often surfaces in kind, but possible on any cluster where the `kubernetes` Service backends listen on a port other than 443.

**Cause.** NetworkPolicy enforcement evaluates packets *after* kube-proxy's DNAT.
When a pod connects to `kubernetes.default.svc` (ClusterIP `10.96.0.1:443`), kube-proxy DNATs the destination to the apiserver's actual Endpoints address — in kind, that's `<node-ip>:6443`.
The policy controller sees the post-DNAT destination port (6443), and an NP rule that allows only port 443 doesn't match.
This is the port-axis equivalent of the `ipBlock: <ClusterIP>/32` trap that bit the proxy NP in PR #59.

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

**Resolution.** Ensure `buildAGCNetworkPolicy` allows both port 443 (production Service shape) *and* port 6443 (kind / Endpoints-on-6443 clusters).
The shipped policy does this.
If you see this on a custom build or a hand-edited NP, add the 6443 rule.
The diagnosis writeup at [`docs/development/networkpolicy-port-matching.md`](../development/networkpolicy-port-matching.md) has a minimal repro and the reasoning behind allowing both ports.

If you see the same symptom for an *ingress*-type rule or for a different Service whose backend port differs from the Service port, the same fix applies: list both ports, or omit the port restriction on that rule.

---

## DNS Times Out Under the Egress NetworkPolicy (GKE Dataplane V2 / NodeLocal DNSCache)

**Symptoms.** A tenant's AGC pod crash-loops on startup, unable to resolve `api.github.com`:

```
token fetch: Post "https://api.github.com/app/installations/<id>/access_tokens":
  dial tcp: lookup api.github.com: i/o timeout
startup failed: initial token fetch: context deadline exceeded
```

From a pod in the tenant namespace (which the GMC egress `NetworkPolicy` governs) DNS times out, while the *same* lookup from a pod in a namespace with no GAG policy (e.g. `default`) succeeds — so cluster DNS is healthy and the egress policy is the cause.
Direct TCP to a GitHub IP on 443 from the tenant pod still works (the GitHub-CIDR egress rule is fine); **only DNS is broken**.
This was first hit on the first live GAG install on GKE (Q224) running on a GKE Standard cluster with **Dataplane V2** (Cilium) and **NodeLocal DNSCache** enabled.

**Cause (Q229).** On GKE Dataplane V2, NodeLocal DNSCache does not give pods a link-local resolver address — pods' `resolv.conf` still points at the `kube-dns` ClusterIP.
GKE installs a `RedirectService` (`networking.gke.io/v1alpha1`, `spec.redirect.type: nodelocaldns`) that drives a Cilium Local Redirect: traffic to the `kube-dns` ClusterIP is transparently redirected to the per-node `node-local-dns` **pod**.
Cilium enforces the egress `NetworkPolicy` against that redirect *backend's* identity — and the backend is `node-local-dns` (`k8s-app: node-local-dns`), **not** `kube-dns`.
The GMC's DNS egress rule selected only `k8s-app: kube-dns` pods plus the link-local block `169.254.0.0/16`; neither matches a `node-local-dns` pod (on Dataplane V2 it runs with `-setupinterface=false`, so it has a regular pod IP and no link-local address), so DNS is dropped.

> A supplemental `NetworkPolicy` allowing egress to the kube-dns ClusterIP CIDR does **not** help: Cilium matches `ipBlock`/CIDR egress only against the external (`world`) identity, never against in-cluster destinations such as a ClusterIP-backed pod.
> The selector path is the only one that works.

> **This contradicts Google's published guidance — trust the measurement.** Google's [NodeLocal DNSCache page](https://cloud.google.com/kubernetes-engine/docs/how-to/nodelocal-dns-cache) says extra NetworkPolicy rules are needed only when you are *not* using Cloud DNS or GKE Dataplane V2 — i.e. it names Dataplane V2 as the exempt case, because the cache pods "don't use `hostNetwork` mode" there.
> That exemption is about the link-local/hostNetwork rules the classic mode needs; it does not address selector matching after local redirection.
> Precisely *because* the DPv2 cache is not hostNetwork, it has a real pod identity, and Cilium enforces the egress policy against it — so the `node-local-dns` peer is required, as the live repro above shows.
> An operator reading Google's page would conclude the peer is unnecessary; on Autopilot (always Dataplane V2, NodeLocal DNSCache not disableable) that conclusion breaks all tenant DNS.

**Resolution.** Upgrade to a GAG build that includes the fix — the GMC-generated DNS egress rule now carries a third peer selecting `k8s-app: node-local-dns` in `kube-system`, alongside the `kube-dns` selector and the link-local block.
This is a strict, minimal widening (still cluster DNS only, port 53 only — it preserves the DNS-exfiltration containment of [§ Security](../design/05-security.md)) and is harmless on clusters without NodeLocal DNSCache, where the selector simply matches no pod. **Re-validated live (2026-07-07)** on a fresh GKE Standard / Dataplane V2 cluster with NodeLocal DNSCache: from a pod governed by the GMC egress `NetworkPolicy`, `nslookup github.com` resolves through the `node-local-dns` peer while non-DNS non-allowlisted egress stays blocked.
Confirm the shipped policy:

```bash
# The DNS (port-53) egress rule must list BOTH k8s-app: kube-dns and
# k8s-app: node-local-dns as peers.
kubectl get networkpolicy <gateway>-workload -n <tenant-ns> -o yaml | grep -A3 'k8s-app'
```

To verify resolution end-to-end from inside the tenant's policy, run an ephemeral pod carrying the workload label and resolve through the kube-dns ClusterIP:

```bash
kubectl run dnstest -n <tenant-ns> --image=busybox:1.36 \
  --labels='actions-gateway/component=workload' --restart=Never -- sleep 300
kubectl exec -n <tenant-ns> dnstest -- nslookup api.github.com
kubectl delete pod dnstest -n <tenant-ns>
```

If you are pinned to an older GAG build and cannot upgrade immediately, the same effect can be obtained by setting `spec.proxy.managedNetworkPolicy: false` and supplying your own egress policy that allows port-53 to `node-local-dns` — but prefer upgrading, since a hand-managed policy must then track GitHub IP-range rotation itself.

---

## Worker Pod Runner.Worker Fails TLS Handshake With UntrustedRoot

**Symptoms.** Worker pod logs (look at the `runner` container) contain repeated lines like:

```
System.Security.Authentication.AuthenticationException: The remote certificate is invalid because of errors in the certificate chain: UntrustedRoot
```

emitted from `JobExtension` connectivity checks, `ResultServer` init, `JobServerQueue` log uploads, the `GitHubActionsService` log-blob fetch, or `RunServer.CompleteJobAsync`.
The runner retries for ~3 minutes, then exits 1.
The AGC then logs `worker pod completed phase=Failed`, `renewjob` starts returning `401 Not authorized for this job`, and the GitHub workflow concludes `cancelled` even though the actual job steps may have run.

**Cause.** Runner.Worker's .NET HttpClient is validating the egress proxy's TLS cert and the worker pod's trust store does not include the cert-manager-issued self-signed CA that signed it.
This is the worker-side mirror of the AGC's proxy-CA pinning ([§5.2](../design/05-security.md) "Cross-Tenant Proxy CA Trust"): the AGC mounts the CA explicitly so its outbound HTTPS works; worker pods must do the same.

The AGC's pod provisioner is supposed to project the per-tenant `actions-gateway-proxy-tls` Secret into every worker pod at `/etc/actions-gateway/proxy-ca/tls.crt` and set `PROXY_CA_CERT_PATH` so the worker entrypoint wrapper builds a combined `SSL_CERT_FILE` bundle before exec'ing `Runner.Worker`.
UntrustedRoot means one of those steps did not happen.

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
- If the volume is present but the wrapper logs nothing about `proxy CA trust installed`: check that `PROXY_CA_CERT_PATH` is set on the runner container and the mounted file is non-empty.
  An empty/missing file is tolerated as a no-op, which silently leaves the runner with no proxy trust — the wrapper log line `no proxy CA cert mounted; skipping trust-store install` distinguishes this case from a wrapper that ran the install successfully.
- If the proxy TLS Secret is missing or the cert has expired: the GMC's cert-manager integration ([§2.1](../design/02-architecture.md#21-tier-1--gateway-manager-controller-gmc) "Proxy Deployer") owns rotation; check the GMC's logs for issuer errors.
  As a fallback, deleting the Secret triggers reissuance.
- If the issue persists after the volume and env are correct: confirm the proxy pod is presenting the cert signed by the CA in the Secret — `kubectl exec` into a curl pod in the same namespace and run `openssl s_client -connect actions-gateway-proxy:8080 -showcerts </dev/null` to inspect what the proxy actually serves.

---

## Which Disruptions Auto-Re-Run a Job (and Which Never Do)

<!-- A marketing-toned summary of this matrix lives on docs/why-gag.md ("What
     re-runs itself" box). When a row here changes, update that box too. -->

The consolidated boundary for the automatic re-run machinery the sections below troubleshoot individually.
Every firing case spends one slot of the run's shared `maxEvictionRetries` budget (default 2, max 10) and works on **both acquisition tiers**.
A fifth cause, `abandoned`, works on both tiers too but sits outside this table because it does not re-run directly: the job never ran, so its run is force-cancelled first and re-run only once capacity returns.
It is covered in [Worker Pod Reaped While Pending](#worker-pod-reaped-while-pending-workerpodstuckpending).

**Fires — the interrupted run is re-run via `rerun-failed-jobs`:**

| Disruption | <span class="gag-nowrap">`cause` label</span> | Detected by |
|---|---|---|
| Kubelet eviction (node pressure) | <code class="gag-nowrap">eviction</code> | pod `Failed` with `reason: Evicted` |
| Scheduler preemption (a preempting `priorityTiers` floor) | <code class="gag-nowrap">preemption</code> | `DisruptionTarget` condition, reason `PreemptionByScheduler` |
| Node drain of a running worker | <code class="gag-nowrap">deletion</code> | terminal phase published while the pod carries a `deletionTimestamp` |
| Bare `kubectl delete pod` of a running worker | <code class="gag-nowrap">deletion</code> | same mark as a drain — indistinguishable, by design |

**Never fires, by design:**

- **A genuine job failure.** The job ran and failed on its own merits; nothing was disrupted, and re-running it would mask real breakage.
- **A cancelled run.** A cancel is the intended stop — it is the supported way to end a job without triggering recovery (delete the worker pod instead and the job *is* re-run; see [Cancelling a Run Does Not Stop Its Worker Pod](#cancelling-a-run-does-not-stop-its-worker-pod)).
- **The AGC's own reaper deletions** — the lifetime cap (`maxWorkerLifetime`), the stuck-`Running` reap deadline, orphan cleanup.
  Each is stamped `actions-gateway.com/deletion-reason` before deletion and excluded: the gateway just judged that job stuck, so a re-run would loop it.
- **A worker whose container never ran** does not fire *this* recovery.
  A drain catching a still-`Pending` pod, or the `pendingPodDeadline` reap, leaves no reportable failure for `rerun-failed-jobs` to act on.
  It is not lost, though: the run is force-cancelled and then re-run once capacity returns, on **both tiers**, which is the out-of-table `abandoned` cause named above.
  See [Worker Pod Reaped While Pending](#worker-pod-reaped-while-pending-workerpodstuckpending).

**Fires but can come up short** — each has its own runbook below:

- The shared budget exhausts (`eviction_retries_exhausted_total`, the `EvictionRetriesExhausted` event): [Evicted Worker Pods Exhausting Retry Budget](#evicted-worker-pods-exhausting-retry-budget).
- The run identity is missing (`eviction_recovery_identity_unknown_total`): [Evicted Scale-Set Jobs Are Not Re-Run Automatically](#evicted-scale-set-jobs-are-not-re-run-automatically).
- A scale-set worker died before GitHub delivered its job — nothing to re-run: ["Runner Lost Communication" and No Worker Pod Was Ever Created](#runner-lost-communication-and-no-worker-pod-was-ever-created).
- GitHub refuses past the re-run window (`EvictionRerunFailed`): [Evicted Worker Pods Exhausting Retry Budget](#evicted-worker-pods-exhausting-retry-budget).

A quota-blocked job is deliberately absent from this table: it is **never claimed** in the first place, so it stays queued at GitHub with no re-run needed — see [Jobs Failing Due to Namespace ResourceQuota Exhaustion](#jobs-failing-due-to-namespace-resourcequota-exhaustion).

---

## Evicted Worker Pods Exhausting Retry Budget

> **Applies to both acquisition tiers.** The budget is keyed by workflow run alone, so `maxEvictionRetries` caps re-runs per run across classic and scale-set together, not once each — several evicted workers of one run share one budget whichever tier they came from.
> Split the counters by the `tier` label to see where the evictions are landing.
> If the counters are flat at **zero** on a scale-set set rather than exhausted, the recovery is not firing at all: see [Evicted Scale-Set Jobs Are Not Re-Run Automatically](#evicted-scale-set-jobs-are-not-re-run-automatically) below.
>
> **Flat at zero on the *classic* tier, with `pod evicted but run_id unknown; skipping auto-retry` in the AGC log?** That is a fixed defect, not a misconfiguration: the AGC read the run identity from a payload field GitHub does not send, so no classic eviction was ever re-run and worker pods carried no `actions-gateway.com/run-id` annotation (Q495).
> The only remedy is to upgrade the AGC — see [the migration note](upgrade.md#non-breaking-classic-tier-eviction-auto-retry-now-fires-it-never-did-against-real-github).
> On a fixed AGC, check the pod's annotation first: `kubectl get pod -n <namespace> <pod> -o jsonpath='{.metadata.annotations.actions-gateway\.com/run-id}'` — empty there means the payload genuinely carried no identity.
>
> **`eviction_retries_total` incrementing but the job still never re-runs?** An evicted run's re-run is deliberately slow to land: a kubelet eviction SIGKILLs the runner before it can report, so GitHub does not conclude the run until the job lock's TTL lapses — measured at 9m36–9m45s when the runner's report does not escape (Q396) — and until then it refuses `rerun-failed-jobs` with `403 This workflow is already running`.
> The AGC retries that refusal every 30 seconds inside a 15-minute re-run window (Q503), so expect `disruption auto-retry triggered` in the AGC log **~10 minutes** after the eviction, not seconds, with `rerunCalls` in the tens — that attribute counts the calls the recovery made.
> A single-digit count is not a fault: it means GitHub had already concluded the run before the recovery started calling — which is what happens when the disrupted runner got its own report out, as a drain or a preemption lets it (15–26s, Q459).
> One eviction has been seen concluding that fast too (2026-08-03, 17s), but the run could not confirm which worker it disrupted, so do not read a low count after an eviction as proof the runner reported.
> A recovery that gave up instead logs `disruption auto-retry failed`, increments `actions_gateway_eviction_rerun_failures_total`, and emits an `EvictionRerunFailed` Warning Event naming the run — that job needs a manual re-run. `reason="run_never_concluded"` means the original run outlived the 15-minute window (check whether GitHub still shows it in progress); `reason="api_error"` means the API failed outright — check the AGC log line's error for a permissions 403 or an endpoint problem.
> On an AGC older than the Q503 fix, the single un-retried re-run always lost this race and every evicted job needed a manual re-run.

**Symptoms.** `actions_gateway_eviction_retries_exhausted_total` is incrementing.
Jobs are being cancelled after eviction despite automatic retries. `kubectl describe` on the owning `RunnerGroup`/`RunnerSet` shows a `Warning` event with reason `EvictionRetriesExhausted` (Q170) naming the affected run — the event-based companion to the metric.

**Likely causes.**
- Worker pod keeps being evicted on every attempt (persistent node pressure, OOM loop, or scheduling conflict that prevents the pod from completing).
- The run is being **repeatedly preempted** by a higher `priorityTiers` floor.
  The budget is shared across both causes (see the `cause` label below), so a run alternately evicted and preempted exhausts it twice as fast as either alone.
- `maxEvictionRetries` is set too low for a workload that is disrupted more than twice per run.

**Diagnostics.**

```sh
# Check disruption retry metrics. The tier label splits the two acquisition paths
# (classic, scaleset) and the cause label splits the three disruptions (eviction,
# preemption, deletion); the retry budget itself is shared across every combination.
# actions_gateway_eviction_retries_total{namespace, runner_group, tier, cause}
# actions_gateway_eviction_retries_exhausted_total{namespace, runner_group, tier, cause}
# actions_gateway_eviction_rerun_failures_total{namespace, runner_group, tier, cause, reason}
# Start here: the cause that is climbing decides which resolution below applies.

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
- `cause="eviction"` climbing — node memory pressure: increase the worker pod's memory requests to discourage the kubelet from evicting it, or investigate the workload's actual memory usage.
- `cause="preemption"` climbing — a higher-priority tier is repeatedly displacing this group: reduce the priority of competing workloads, adjust `priorityTiers` to give this RunnerGroup a higher floor, or move the work to a tier that is not displaceable.
  A long job in an opportunistic tier is the usual culprit, since every displacement restarts it from the beginning.
- `cause="deletion"` climbing — something outside the gateway is deleting live workers: a node drain (upgrades, autoscaler consolidation), a descheduler, or hand-run `kubectl delete pod`.
  Find the deleter before raising the budget; see [Draining a Worker Auto-Re-Runs the Jobs It Interrupts](#draining-a-worker-auto-re-runs-the-jobs-it-interrupts).
- If the retry budget is simply too low for a workload that is legitimately disrupted more than twice per run: increase `maxEvictionRetries` on the RunnerGroup spec (default 2, max 10).
- If the workload is consistently failing (OOM crash, not a disruption): the auto-retry is not appropriate.
  Set `maxEvictionRetries: 0` and investigate the underlying workload issue.

---

## Draining a Worker Auto-Re-Runs the Jobs It Interrupts

> **Applies to both acquisition tiers.** Behaviour since Q502 — on earlier versions a drained worker's run needed a manual re-run. **Preemption has its own runbook** — [A Preempted Worker's Job Is Not Re-Run](#a-preempted-workers-job-is-not-re-run) below.

**Behaviour.** You cordon and drain a node that has worker pods on it, or delete a running worker pod by hand.
The eviction (a drain is a PDB-checked graceful **delete** of each pod) starts the pod's termination: the Q385 SIGTERM relay gives the runner the pod's grace period to abort its job and report, GitHub concludes the job `failure` within seconds (15–26s measured, Q459), and the AGC re-runs the interrupted run — `actions_gateway_eviction_retries_total{cause="deletion"}` increments and a `rerun-failed-jobs` call is made, spending one slot of the run's shared `maxEvictionRetries` budget.

**How it is detected, and the boundary.** A drained running worker lands in `Failed` with an **empty** `status.reason` — the same shape as a genuinely failed job and as a human-cancelled run — but it is the only one of the three that publishes that phase while carrying a `deletionTimestamp` (measured on both halves, Q459: a cancelled run's worker is never deleted, and a failed job's pod was never deleted either).
Recovery keys on that mark.
Consequences an operator should know:

- **A bare `kubectl delete pod` of a running worker re-runs its job too.** It is indistinguishable from a drain, and that is the intended reading: deleting a worker mid-job interrupts a job you did not mean to fail.
- **The AGC's own cleanup never triggers a re-run.** Every pod the reaper deletes is stamped `actions-gateway.com/deletion-reason: <reason>` first, and stamped deletions are excluded from recovery.
  Never set that annotation by hand on a live worker — it suppresses automatic recovery for that pod.
- **A worker whose container never ran is not re-run *by this path*** — e.g. a drain catching a still-`Pending` worker.
  Its job never ran to a reportable end, so there is no failed job for `rerun-failed-jobs` to act on; detection requires a recorded container exit that the deletion preceded, which such a pod does not have.
  It is recovered by the `abandoned` path instead, which force-cancels the run first: see [Worker Pod Reaped While Pending](#worker-pod-reaped-while-pending-workerpodstuckpending).
- **A cancelled run is never re-run.** Nothing in the gateway deletes a cancelled run's pod, so it carries no mark.
- **A small fraction of drains lose the window entirely** (scale-set tier only).
  The pod is the disruption's only record, and the kubelet removes it once the container's exit is published — so if the AGC's claim lands after that, nothing recovers the run and no later reconcile can.
  It is reported rather than silent: `actions_gateway_eviction_recovery_evidence_lost_total{cause="deletion"}` increments and an `EvictionRecoveryEvidenceLost` Warning Event is recorded on the `RunnerSet`.
  Those runs need a manual re-run.
  A sustained rate means the AGC is not reaching the window — check whether it is CPU-starved or its work queue is backlogged, not whether the policy or role is wrong.
- **The first re-run call may be refused.** GitHub's conclusion on this path takes 15–26s while `evictionRetryDelay` defaults to 5s, so the first `rerun-failed-jobs` can land while the run is still in progress and be answered `403 This workflow is already running`.
  The Q503 retry loop absorbs that — the re-run is retried on a 30s pace until accepted — so no action is needed; see [Evicted Worker Pods Exhausting Retry Budget](#evicted-worker-pods-exhausting-retry-budget) for the loop's own failure modes.

Two related things operators reasonably expect to change this, and which do not:

- The worker pod's `cluster-autoscaler.kubernetes.io/safe-to-evict: false`, `karpenter.sh/do-not-disrupt: true`, and descheduler prefer-no-eviction annotations are advisory to **those controllers only**. `kubectl drain` and kube-scheduler preemption both ignore all three, by design — they exist to stop an autoscaler or descheduler from moving a mid-job worker, not to stop an administrator or the scheduler.
- Worker pods carry no PodDisruptionBudget, so nothing rejects the eviction either.

**Diagnostics.**

```sh
# Which disruption was it? A kubelet eviction leaves the pod behind in Failed/Evicted;
# a preemption records Preempted on the victim; a drain shows up as the eviction API's
# events and, briefly, a Failed pod with a deletionTimestamp.
kubectl get events -n <namespace> --sort-by='.lastTimestamp' \
  | grep -E 'Evicted|Preempted|Killing|TaintManagerEviction'

# Whether recovery fired for the window in question. The cause label separates the
# three recovered disruptions (eviction, preemption, deletion):
# actions_gateway_eviction_retries_total{namespace, runner_group, tier, cause}
# Flat across a drain of RUNNING workers is unexpected — check the AGC log below.
# Flat across a drain that only caught Pending workers is correct.

# Confirm the runner did get its chance to report before the pod went away.
kubectl logs -n <namespace> <worker-pod> --previous \
  | grep -E 'forwarding termination signal|outlived the shutdown grace period'

# The AGC logs the decision explicitly, with the cause.
kubectl logs -n <namespace> deploy/<agc-deployment> \
  | grep -E 'worker pod disrupted; scheduling auto-retry|disruption auto-retry'

# Scale-set tier: whether a disruption was found and then lost because the pod went
# away before the claim landed. Names the pod, so it maps to the run that needs a
# manual re-run.
kubectl logs -n <namespace> deploy/<agc-deployment> \
  | grep 'disruption was lost before it could be claimed'
```

If a drain of running workers produced no re-run, check whether the pods carried the `actions-gateway.com/deletion-reason` stamp (then the AGC deleted them, not your drain), whether the run's retry budget was already spent (`eviction_retries_exhausted_total`), and — scale-set tier only — whether the AGC was down across the teardown window, lost the pod before it could claim it (`eviction_recovery_evidence_lost_total`), or the pods carried no run identity (see [A Preempted Worker's Job Is Not Re-Run](#a-preempted-workers-job-is-not-re-run), whose scale-set failure modes apply to drains identically).

**Resolution / how to drain safely.**
- **Prefer a quiet window anyway.** The re-run restarts each interrupted job from the beginning, so a drain mid-job still costs the work done so far — and each interrupted run spends re-run budget. `kubectl get pods -n <namespace> -l app.kubernetes.io/managed-by=actions-gateway-controller` on the target node shows what a drain would interrupt; an empty result is the free moment.
- **Give the runner room to report** before you drain, so the jobs at least conclude cleanly instead of hanging: raise `terminationGracePeriodSeconds` in the runner group's `podTemplate` and `WORKER_SHUTDOWN_GRACE` with it, per [Terminated Worker Pod Never Reports Its Job](#terminated-worker-pod-never-reports-its-job-job-hangs-on-github-until-the-lock-lapses).
  The same budget is what a preempted runner gets.
- **Put cheap-to-repeat work in the preemptible tiers.** A displaced job is re-run automatically, but it restarts from the beginning — so a long, expensive job is still the wrong tenant for an opportunistic tier.
  Create allowlisted classes `preemptionPolicy: Never` unless a tier is genuinely meant to displace others — which is the default guidance in [security-operations.md](security-operations.md) for a separate reason (cross-tenant preemption) and holds here too.
- Do **not** try to make the drain fail instead: a PodDisruptionBudget over worker pods would block the drain rather than protect the job, and would leave the node undrainable for as long as any job runs.
  It would not stop a preemption at all — the scheduler's victim selection does not consult PodDisruptionBudgets as a hard constraint.

  That last point is deliberate upstream behaviour, not an oversight, and it is worth knowing before you spend time on it.
  The Eviction API a drain uses is effectively *a delete that checks PDBs first*; kube-scheduler preemption skips that check because a PDB that could veto preemption would let any low-priority workload make itself un-preemptible simply by declaring one — which would turn a guaranteed tier's scheduling guarantee into a suggestion, and in a shared cluster would let one tenant starve another's guaranteed capacity.
  The same reasoning is why the worker's `safe-to-evict: false` and `do-not-disrupt` annotations do not deflect a preemption: they are advisory to autoscalers and deschedulers, and the scheduler is neither.
  See [§4.2](../design/04-operational-flows.md#why-preemption-deletes-rather-than-evicts-and-what-that-costs-us) for the full reasoning and what it costs the gateway.

---

## A Preempted Worker's Job Is Not Re-Run

> **Applies to both acquisition tiers**, but the failure modes differ by tier — the scale-set one has a time limit the classic one does not.

**Symptoms.** A higher-priority `priorityTiers` tier displaces an opportunistic worker, and the displaced run stays failed: `actions_gateway_eviction_retries_total{cause="preemption"}` does not move, and no `rerun-failed-jobs` call is made.
Expected behaviour is one automatic re-run per preempted run.

**Cause.** Work through these in order:

1. **The victim was not actually preempted.** A `kubectl drain`, a manual delete, or a descheduler eviction looks similar but is recovered under `cause="deletion"` instead (see the previous runbook) — and only if the worker reached a terminal phase before the object went away.
   Check which cause, if any, actually moved.
2. **The retry budget is spent.** `maxEvictionRetries` (default 2) is a hard lifetime cap per workflow run, shared across both tiers *and* both disruption causes: a run already re-run twice for node-pressure evictions has nothing left for a preemption. `actions_gateway_eviction_retries_exhausted_total{cause="preemption"}` confirms it, as does a `Warning` event with reason `EvictionRetriesExhausted`.
3. **The worker carried no workflow-run identity** (scale-set tier only). `actions_gateway_eviction_recovery_identity_unknown_total{cause="preemption"}` increments and a `Warning` event with reason `EvictionRecoveryIdentityUnknown` is recorded on the `RunnerSet`.
   See [Evicted Scale-Set Jobs Are Not Re-Run Automatically](#evicted-scale-set-jobs-are-not-re-run-automatically) — the cause and the fix are the same for both disruptions.
4. **The AGC was down for the victim's whole termination grace period** (scale-set tier only).
   This one has no workaround, and is a property of the signal rather than a bug: the scheduler *deletes* its victim, so unlike an evicted pod — which sits in `Failed` until the reaper takes it — a preempted pod is readable only until its grace period expires (30s by default).
   An AGC restarting across that window never sees the marker, and the displaced run needs a manual re-run.
   The classic tier is unaffected: its provisioning goroutine is already watching the pod, and if that goroutine is gone the session is gone with it.
5. **The AGC saw the victim but lost it before claiming it** (scale-set tier only).
   The same window as (4), missed by a margin rather than entirely: the recovery scan reads pods from the informer cache and claims them through the live API, so a pod removed in between yields a claim that finds nothing. `actions_gateway_eviction_recovery_evidence_lost_total{cause="preemption"}` increments and an `EvictionRecoveryEvidenceLost` Warning Event is recorded.
   A manual re-run is required, and a sustained rate points at AGC responsiveness — CPU starvation or a backlogged work queue — rather than at the role or the policy.

**Diagnostics.**

```sh
# Did recovery fire, and under which cause?
# actions_gateway_eviction_retries_total{namespace, runner_group, tier, cause="preemption"}

# The AGC logs the decision explicitly.
kubectl logs -n <namespace> deploy/<agc-deployment> \
  | grep -E 'worker pod disrupted; scheduling auto-retry|disruption auto-retry|budget exhausted'

# Confirm the AGC was up across the preemption — the scale-set tier's one hard limit.
kubectl get pods -n <namespace> -l app.kubernetes.io/name=actions-gateway-controller \
  -o custom-columns=NAME:.metadata.name,RESTARTS:.status.containerStatuses[0].restartCount,START:.status.startTime
```

**Resolution.**
- **Raise `maxEvictionRetries`** on the `RunnerGroup`/`RunnerSet` spec (default 2, max 10) if a workload is legitimately displaced more than twice per run.
- **Fix the missing run identity** per the scale-set runbook linked above; without it no disruption of any cause can be recovered on that tier.
- **Re-run the affected run manually** for anything lost to case 4, and keep AGC restarts (upgrades, node maintenance on the control-plane namespace) out of windows where a floor tier is actively preempting.

---

## Cancelling a Run Does Not Stop Its Worker Pod

> **Applies to the Classic acquisition tier.** On **ScaleSet** (the default) a cancel does not stop the worker promptly either, but it *is* reclaimed — see [How the two tiers differ](#how-the-two-tiers-differ) below.

**Symptoms.** You cancel a workflow run in the GitHub UI (or with `gh run cancel`).
GitHub shows the job `cancelled` a few minutes later, but the worker pod stays `Running` and keeps consuming its CPU/memory request — and its concurrency-ceiling slot — until the job's own steps finish or `maxWorkerLifetime` kills it.
Cancelling a runaway job does not free capacity.

**Cause — this is current behaviour, not a misconfiguration.** The gateway multiplexes runner sessions inside the AGC, so a cancellation is delivered to the *AGC's* broker session, not to the runner in the worker pod.
Nothing forwards it, and nothing deletes the pod, so the runner never learns the job was cancelled and executes its remaining steps to the end.
GitHub concludes the job on its own ~5-minute cancellation grace rather than on anything the runner reports.

Measured against real GitHub on 2026-07-29: a job whose step was `sleep 600` was cancelled 2 seconds after it started, GitHub concluded it `cancelled` 5m02s later, and the worker pod ran the full 600s before reaching a terminal phase.

This is also why the SIGTERM relay does not help here.
The relay fires when the *pod* is terminated — an eviction, a drain, a delete — and on this path nothing terminates the pod.

### How the two tiers differ

The Classic tier has no channel from GitHub to the AGC once the job is acquired, so nothing downstream of the cancel reaches the gateway at all and the pod is bounded only by `maxWorkerLifetime` (12 hours by default).

The **ScaleSet** tier keeps a message queue open for the whole job, and a cancelled run puts a terminal `JobCompleted` on it.
That completion stamps `actions-gateway.com/job-completed-at` on the worker, and the reaper deletes a pod still `Running` five minutes later ([WorkerPodOrphanedRunning](#worker-pod-reaped-while-running-workerpodorphanedrunning)).
So the worst case there is GitHub's cancellation grace plus the reap grace — roughly ten minutes — rather than the job's whole remaining runtime.
Expect `actions_gateway_worker_pods_reaped_total{reason="orphaned_running"}` to move when this happens.

Two caveats on that figure.
The `JobCompleted`-on-cancel was measured against real GitHub for a job with **no runner attached** (the queue message appeared ~0.2 s after the cancel); the stamp-and-reap half is proven against a real apiserver.
The composed chain has not been observed end to end for a job with a live worker, so treat ten minutes as the shape of the bound rather than a measured latency.

**Diagnostics.**

```sh
# Worker pods still Running for a run you already cancelled. Compare against the
# run's state in GitHub: gh run view <run-id> --json status,conclusion
kubectl get pods -n <namespace> \
  -l app.kubernetes.io/managed-by=actions-gateway-controller \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,AGE:.metadata.creationTimestamp'

# What the runner thinks it is doing. A worker still executing steps after its run
# was cancelled prints ordinary job output, with no cancellation notice.
kubectl logs -n <namespace> <worker-pod> --tail=50
```

**Resolution.**
- **Deleting the worker pod reclaims the capacity, and re-queues the job you cancelled.** `kubectl delete pod -n <namespace> <worker-pod>` terminates it gracefully: the wrapper relays SIGTERM and the runner stops.
  Only reach for it when you cancelled to free the slot rather than to stop the work.

  > **The delete undoes the cancel.** A hand-deleted worker is the shape graceful-deletion recovery acts on (an external delete ordered before the container's exit), so the AGC asks GitHub to re-run the run's failed jobs.
  > GitHub honours that for a `cancelled` conclusion: the `rerun-failed-jobs` call is accepted and the job is re-queued, where a `success` conclusion refuses it with a 403 (measured live 2026-08-05, [the Q683 measurement](../plan/q645-abandoned-completion.md#q683--the-fast-ending-measurement-2026-08-05)).
  > Nothing in the gateway deletes a cancelled run's pod, which is why the deletion mark is trusted as a disruption signal; this remedy is the one case where an operator produces that mark deliberately.
  > It applies on ScaleSet too: the tier's own reclaim carries the `actions-gateway.com/deletion-reason` stamp and is excluded, while a hand-delete carries no stamp and takes the recovery path.
  > Each re-run spends one slot of the run's shared `maxEvictionRetries` budget (default 2, max 10), so a repeated cancel-then-delete cycle is bounded, ending in an `EvictionRetriesExhausted` warning Event on the owner.
  > Whether recovery should read the run's conclusion before re-running is [Q811](../STATUS.md#Q811).
- **If you cancelled to stop the work, leave the pod alone** and wait it out, or bound it with `maxWorkerLifetime` below.
  There is no way today to free the slot *and* keep the run cancelled: the delete re-queues the job, and the new attempt has to be cancelled in turn.
- **Bound the worst case with `maxWorkerLifetime`** on the runner group, which caps how long any worker — cancelled or not — can hold its slot.
- Do **not** expect a lower `completedPodTTL` or `pendingPodDeadline` to help: both act on pods that have already stopped or never started, and this pod is running.

---

## Evicted Scale-Set Jobs Are Not Re-Run Automatically

**Symptoms.** A `RunnerSet` on the scale-set acquisition tier (`spec.acquisitionProtocol: ScaleSet`, the default) has workers being evicted, but `actions_gateway_eviction_retries_total{tier="scaleset"}` stays flat and the jobs stay failed until someone re-runs them by hand.
Usually one of two accompanying signals is present:

- `actions_gateway_eviction_recovery_identity_unknown_total` is incrementing, and `kubectl describe runnerset` shows a `Warning` event with reason `EvictionRecoveryIdentityUnknown`; or
- neither counter moves at all.

**Cause.** The scale-set tier provisions fire-and-forget — the runner pulls and completes its own job — so nothing in the AGC is watching a given worker pod, and there is no acquired payload to read the job's identity from.
Recovery therefore depends on two things being on the pod: the run identity (`actions-gateway.com/run-id`, `actions-gateway.com/repository`), stamped from the assignment message's `ownerName`/`repositoryName`/`workflowRunId`, and the `actions-gateway.com/acquisition-protocol=ScaleSet` label that tells the reconciler the pod is its to recover.
Either one missing makes recovery inert for that pod.

**Diagnostics.**

```sh
# Does the evicted worker carry its run identity and the tier marker?
kubectl get pods -n <namespace> -l actions-gateway.com/acquisition-protocol=ScaleSet \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,REASON:.status.reason,RUN:.metadata.annotations.actions-gateway\.com/run-id,REPO:.metadata.annotations.actions-gateway\.com/repository,HANDLED:.metadata.annotations.actions-gateway\.com/eviction-handled-at'
```

Read the result as follows.

| What you see | What it means |
| --- | --- |
| `REASON` is empty rather than `Evicted` | Not a kubelet eviction. Only `PodFailed`/`Evicted` triggers recovery — anything else ran and reported its own outcome, so re-running it would double-report |
| `RUN`/`REPO` empty | The assignment carried no run identity. This is the `EvictionRecoveryIdentityUnknown` case |
| `RUN`/`REPO` present, `HANDLED` empty | The reconciler has not adjudicated it yet, or the scan is failing — check the AGC log for `scale-set eviction recovery scan failed` |
| `HANDLED` set but no re-run | The claim succeeded and the rerun call failed. Look for `eviction auto-retry failed` in the AGC log, or an exhausted budget |

```sh
# The AGC's own account of what it did with the eviction.
kubectl logs -n <namespace> deploy/<agc-deployment> \
  | grep -E 'pod evicted|eviction auto-retry|run identity is unknown|eviction recovery scan failed'

# The identity the queue actually delivered (raise the AGC to debug first).
kubectl logs -n <namespace> deploy/<agc-deployment> \
  | grep 'carries no run identity'
```

**Resolution.**
- **`RUN`/`REPO` empty on every evicted worker.** GitHub is not sending the assignment fields recovery depends on.
  There is no configuration workaround — the identity has no other source on this tier.
  Re-run affected jobs manually, and treat a sustained rate as a protocol regression worth reporting rather than a capacity problem.
- **`REASON` is not `Evicted`.** Working as intended.
  The pod ran and reported its own result; if the *job* nonetheless hung, see [Terminated Worker Pod Never Reports Its Job](#terminated-worker-pod-never-reports-its-job-job-hangs-on-github-until-the-lock-lapses) instead.
- **Evicted pods disappearing before recovery runs.** Recovery reads the evicted pod, so it must still exist.
  A very short `completedPodTTL` combined with a backlogged AGC can delete the evidence first; raise `completedPodTTL` (default 5 m) if you see terminal workers vanishing within seconds.
- **Budget exhausted instead.** See [Evicted Worker Pods Exhausting Retry Budget](#evicted-worker-pods-exhausting-retry-budget).
  Note the budget is shared across both tiers for a given run, not per tier.
- **Node drains rather than kubelet evictions.** A drained pod is deleted, not left `PodFailed`/`Evicted`, and the runner reports its own cancellation through the SIGTERM relay — so it is deliberately outside this mechanism, not a gap in it.
- **Recovery fires but the rerun call fails (GHES).** If the AGC logs `disruption auto-retry failed ... rerun API returned 401: Bad credentials`, the detection worked and the *call* went to the wrong host.
  Before the Q504 fix the rerun always addressed `api.github.com` regardless of `GITHUB_API_BASE_URL`, so on GHES it presented a token that host had never issued.
  Confirm the endpoint the AGC is using and upgrade if it is not yours:

  ```sh
  kubectl get deployment <agc-deployment> -n <namespace> \
    -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="GITHUB_API_BASE_URL")].value}'
  ```

  A 401 naming a host you did not configure is the signature; a 401 from your own endpoint is a genuine credential problem instead.

---

## Terminated Worker Pod Never Reports Its Job (Job Hangs on GitHub Until the Lock Lapses)

**Symptoms.** A worker pod is terminated before its job finishes — evicted, drained off a node, deleted, or its run cancelled in the GitHub UI — and the job keeps showing as in-progress on GitHub for minutes afterwards, until the job lock lapses and GitHub gives up on it.
The worker container logs stop abruptly with no cancellation or completion line, and the pod's last state shows an exit code of `137` (SIGKILL).
Re-running the workflow is the only way to move it along.

**Cause.** Kubernetes delivers SIGTERM to **PID 1 of each container only**; child processes are not signalled.
Gateway versions before the Q385 fix ran the entrypoint wrapper as PID 1 with no signal handling, so `Runner.Worker` (classic protocol) or `run.sh` (ScaleSet protocol) — the process that actually reports the job to GitHub — never saw the signal.
It kept running until the kubelet SIGKILLed the whole cgroup at grace expiry, which is unblockable: no cancellation is reported, and GitHub has to wait the job lock out.

Fixed versions forward SIGTERM (and SIGINT) from the wrapper to its child and wait for it to exit, so the runner gets its grace period to abort the job and report the result.
Two related behaviours come with it:

- A child that is still alive when the wrapper's own budget expires is killed by the wrapper, which logs `child outlived the shutdown grace period; killing it` — an actionable line where there was previously silence.
- The child's exit code is propagated, and a child that died from a signal is reported as `128 + signal` (SIGTERM → `143`, SIGKILL → `137`) rather than the meaningless `255` that the raw `-1` used to become.
- A pod terminated in the first instants of its life is covered too (Q445).
  The wrapper installs its signal handler *before* it starts the runner, so a SIGTERM that arrives in that window is held and forwarded as soon as the child exists.
  Earlier fixed versions registered the handler after the start and could drop such a signal on the floor — PID 1 ignores a signal it has no handler for — which reproduced the unreported-job symptom above for a job cancelled or a node drained within a second or so of the pod starting.

**Diagnostics.**

```sh
# Confirm the child was signalled: the wrapper logs the forward on the way down.
kubectl logs -n <namespace> <worker-pod> --previous \
  | grep -E 'forwarding termination signal|outlived the shutdown grace period'

# Exit code of the runner container on the last termination.
kubectl get pod -n <namespace> <worker-pod> \
  -o jsonpath='{.status.containerStatuses[?(@.name=="runner")].lastState.terminated}'
# 143 = terminated after the forwarded SIGTERM; 137 = SIGKILLed (grace exhausted).

# The pod's grace budget — the wrapper's drain must fit inside it.
kubectl get pod -n <namespace> <worker-pod> \
  -o jsonpath='{.spec.terminationGracePeriodSeconds}{"\n"}'
```

**Resolution.**
- Upgrade to a gateway version that includes the Q385 fix; there is no configuration workaround on affected versions.
- If jobs need longer than the default to wind down and report (large cleanup steps, slow log flushes), raise **both** budgets together, in this order: `terminationGracePeriodSeconds` in the runner group's `podTemplate`, then `WORKER_SHUTDOWN_GRACE` (a Go duration, e.g. `40s`) as an env var on the runner container.
  The wrapper's grace must stay comfortably **below** the pod's, or the kubelet's SIGKILL lands first and you are back to the unreported case.
- If you see `child outlived the shutdown grace period; killing it` routinely, the runner is not finishing its cancellation inside the budget — raise the pair above rather than accepting the kill, and check whether a job step is ignoring cancellation.

---

## Jobs Failing Due to Namespace ResourceQuota Exhaustion

**Symptoms.** Jobs sit queued in GitHub while the `RunnerGroup`/`RunnerSet` reports `WorkerQuotaExceeded=True` (and `actions_gateway_worker_quota_exceeded` reads `1` — on a `RunnerSet`, the per-set twin `actions_gateway_runnerset_worker_quota_exceeded`) — the AGC is declining to take on work it cannot place.
Which metric shows it depends on the acquisition tier:

| Tier | What climbs |
|---|---|
| `ScaleSet` (the default) | `actions_gateway_scaleset_advertised_capacity` **falls**, to `0` when no worker pod fits at all, and `actions_gateway_scaleset_capacity_withheld{reason="quota"}` accounts for the gap. |
| `Classic` (deprecated) | `actions_gateway_jobs_admission_rejected_total{reason="quota"}` climbs. |

On the scale-set tier `jobs_admission_rejected_total` reads a flat zero and that is correct, not a gap: a job the ladder declines there is never assigned, so there is no rejected delivery to count (Q443).

If instead `actions_gateway_quota_retries_exhausted_total` is incrementing, pod creation is failing with a `Forbidden` error containing "exceeded quota" in the AGC logs and jobs are abandoned before a pod is ever scheduled, with a `Warning` event of reason `QuotaRetriesExhausted` (Q170) on the owner.
That is the same root cause reaching the AGC one layer later — see [When quota exhaustion still reaches the retry path](#when-quota-exhaustion-still-reaches-the-retry-path) below.

**The admission gate declines on quota (#784, Q443).** When the namespace `ResourceQuota` cannot admit another worker pod, the AGC stops taking on work rather than claiming it and stalling.
Claiming it instead would hold the GitHub job lock across up to `maxQuotaRetries` × `quotaRetryDelay` (150s of a ~10-minute lock at the defaults) and, on budget exhaustion, drop the job *with the lock held*, which gets the run cancelled rather than requeued.
The check is a live read of every `ResourceQuota` in the namespace (`hard − used`) against one more worker's footprint, and it **fails open**: a quota the AGC cannot read leaves capacity exactly as it was.

How that refusal is expressed differs by tier, and the difference is worth knowing before you read the metrics:

- **`ScaleSet` (the default).** The AGC advertises a capacity of `min(worker ceiling, own in-flight pods + quota headroom)` on every long-poll, so GitHub simply assigns fewer jobs — or none, at `0`.
  Nothing is claimed and nothing is wasted.
  The cost is granularity: the decision is per poll for the whole set, so restored headroom reopens assignment within one long-poll (~50s) rather than instantly.
- **`Classic` (deprecated).** The AGC skips `acquirejob` per delivered job, leaving it queued at GitHub for redelivery to a sibling with capacity.

The AGC surfaces two non-blocking conditions on each `RunnerGroup` for the namespace-quota axis (Q82), each exported as a gauge so you can alert without kube-state-metrics.
Distinguish them from Q59's configured-ceiling backpressure (`actions_gateway_jobs_admission_rejected_total{reason="ceiling"}`), which is normal load-shedding to a sibling, not a quota problem:

| Condition / metric | Meaning | Severity |
|---|---|---|
| `WorkerQuotaPressure` — `actions_gateway_worker_quota_pressure`, v2 twin `actions_gateway_runnerset_worker_quota_pressure` | Workers can't scale to the configured ceiling (`maxWorkers` / max `priorityTiers` threshold) within the quota's remaining headroom. | warning (don't page) |
| `WorkerQuotaExceeded` — `actions_gateway_worker_quota_exceeded`, v2 twin `actions_gateway_runnerset_worker_quota_exceeded` | The quota can't admit even one more worker pod — the next acquired job's pod will be rejected. | error (page) |

> **v2 `RunnerSet`.** Both `WorkerQuotaPressure` and `WorkerQuotaExceeded` are set on a v2 `RunnerSet` (Q303) with identical semantics — swap `runnergroup` for `runnerset` in the commands below.
> Their gauges are the per-set twins `actions_gateway_runnerset_worker_quota_pressure` and `actions_gateway_runnerset_worker_quota_exceeded` (Q319), keyed on `namespace`/`runner_set` rather than `namespace`/`runner_group`; the v1 `actions_gateway_worker_quota_*` families stay `RunnerGroup`-only, so `actions_gateway_worker_quota_exceeded == 1 or actions_gateway_runnerset_worker_quota_exceeded == 1` is the query that covers both while `v1alpha1` is still served.

```sh
kubectl get runnergroup -n <namespace> <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="WorkerQuotaExceeded")]}{.status} {.reason}: {.message}{"\n"}{end}'
```

**Likely causes.**
- The namespace ResourceQuota `pods` or `requests.cpu`/`requests.memory` limit is too low for the burst of concurrent jobs arriving.
- A long-running job is holding quota that a new job needs; quota will clear once it completes.
- The quota retry budget (`maxQuotaRetries`, default 5) is exhausting before quota clears.
- The quota was sized from the worker's regular containers only.
  A native sidecar (`restartPolicy: Always` init container — how the DinD daemon must be declared) is summed into the pod's footprint in full, and a Kata worker adds its `RuntimeClass` overhead on top.
  Both are invisible in `podTemplate.spec.containers`.
  Re-derive with [sizing the platform-owned `ResourceQuota`](resourcequota-sizing.md).
  The `WorkerQuota{Pressure,Exceeded}` conditions count both, so this shows up there first — if they read `False` while pods are being rejected, the mismatch is elsewhere in this list.
- The binding key is a **storage** key, not a compute one.
  A worker's `ephemeral-storage` asks count like CPU and memory, and each generic ephemeral volume (`podTemplate.spec.volumes[].ephemeral` — the reference Kata worker's per-pod block device) creates a real PVC charged against `persistentvolumeclaims`, `requests.storage`, and the matching `<class>.storageclass.storage.k8s.io/…` keys.
  The conditions count these too; see [the storage keys](resourcequota-sizing.md#step-3--the-storage-keys).

> **A `Forbidden` naming a *missing* resource is a different failure.** `must specify limits.cpu for: runner` is not exhaustion — it means the quota constrains a key the pod does not declare, which Kubernetes makes mandatory namespace-wide.
> The measured DinD worker shapes declare no CPU limit on purpose, so a quota constraining `limits.cpu` rejects every worker pod regardless of headroom.
> See [only constrain keys every pod declares](resourcequota-sizing.md#only-constrain-keys-every-pod-declares).

> **An exhausted *storage* quota looks different again — no rejected pod at all.** A PVC key is charged against the PVC, which Kubernetes creates *after* admitting the pod, so nothing rejects the worker: it sits `Pending` with an unbound volume until the pending-pod deadline reaps it, and `kubectl describe pod` shows no quota error.
> Look at the claim instead:
>
> ```sh
> kubectl get events -n <namespace> --field-selector reason=FailedCreate
> ```
>
> The gateway counts these keys in the worker footprint so this is refused before the job is claimed — a set in this state should be reporting `WorkerQuotaExceeded=True`.

**Diagnostics.**

```sh
# Check quota retry metrics
# actions_gateway_quota_retries_total{namespace, runner_group}
# actions_gateway_quota_retries_exhausted_total{namespace, runner_group}

# Deliveries declined up-front because the quota had no headroom (#784) — Classic tier.
# actions_gateway_jobs_admission_rejected_total{namespace, runner_group, reason="quota"}

# The same refusal on the ScaleSet tier (Q443): capacity withheld rather than
# jobs rejected. advertised + sum(withheld) == the set's declared ceiling.
# actions_gateway_scaleset_advertised_capacity{namespace, runner_set}
# actions_gateway_scaleset_capacity_withheld{namespace, runner_set, reason="quota"}

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

### When quota exhaustion still reaches the retry path

The admission gate reduces how often `createPodWithQuotaRetry` is entered; it does not replace it.
Quota can still be gone by the time the pod is created, because:

- a **sibling AGC or another workload** in the namespace consumed the headroom between the gate's read and the pod create;
- the gate's read is of `ResourceQuota.status`, which is **eventually consistent** — `used` lags the pods that already exist;
- the AGC **restarted** between the claim and the create;
- the gate is **off** (`AGC_QUOTA_ADMISSION=false`, below), or it **failed open** on a quota it could not read.

On the `ScaleSet` tier add one more: the advertised capacity is recomputed **per poll**, not per job, so headroom lost between two polls is not seen until the next one.

So keep `maxQuotaRetries`/`quotaRetryDelay` tuned; the two mechanisms are layered, not alternatives.

### Turning the quota rung off

The gate's quota rung is **on by default**, and the AGC honours `AGC_QUOTA_ADMISSION=false` in its own environment to revert to the pre-#784 behaviour (take the work first, discover quota exhaustion in the retry loop).
It covers **both** tiers: on `ScaleSet` the set goes back to advertising its declared ceiling and publishes no `capacity_withheld{reason="quota"}` series at all.
On a GMC-provisioned gateway there is deliberately no tenant-facing field for it: the only route to the AGC Deployment's env is the testing-only `AGC_EXTRA_*` passthrough behind the GMC's `--allow-agc-extra-env`, which is not for production use.

The one situation that would justify the escape hatch is a cluster whose `ResourceQuota.status` accounting is unreliable enough to starve a tenant that in fact has room — symptom: capacity withheld for `quota` (or `reason="quota"` rejections on Classic) while `kubectl describe resourcequota` shows real headroom and no worker pods are being created.
Fix the quota accounting first.
If you hit a case that genuinely needs the opt-out in production, open an issue so it can be promoted to a supported field rather than worked around.

---

## Jobs Not Being Acquired Despite Queued Work (Capacity Gate Saturated)

**Symptoms.** Workflow jobs sit queued in GitHub while `actions_gateway_jobs_admission_rejected_total{namespace, runner_group, reason="ceiling"}` climbs and `actions_gateway_jobs_acquired_total` plateaus for the same group. `kubectl get pods` shows the group already running its full complement of worker pods.
The AGC is healthy — this is throttling, not a fault.

> **Check the `reason` label first.** `reason="quota"` is a different problem with a different fix — the namespace `ResourceQuota` is out of headroom, not the group's ceiling.
> See [Jobs Failing Due to Namespace ResourceQuota Exhaustion](#jobs-failing-due-to-namespace-resourcequota-exhaustion).
> The rest of this section is about `reason="ceiling"`.

> **On a `ScaleSet` `RunnerSet` (the default tier)** the same throttling shows up as `actions_gateway_scaleset_advertised_capacity` sitting at the set's ceiling with all its slots in use — the ceiling is advertised to GitHub as `X-ScaleSetMaxCapacity`, so surplus jobs are never assigned rather than being delivered and declined.
> There is no `reason="ceiling"` series there; read `advertised_capacity` against the running worker-pod count instead.
> The resolutions below apply unchanged.

**Cause.** This is the pre-acquisition admission gate working as designed (Q59).
When a RunnerGroup is already at its worker ceiling (`maxWorkers`, or the maximum `priorityTiers` threshold), the AGC **deliberately skips `acquirejob`** for newly delivered jobs and leaves them queued at GitHub, so they are redelivered to a session with capacity rather than claimed-then-dropped (which would get the run cancelled).
A rising rejection counter therefore means *demand exceeds the configured ceiling*, not that anything is broken.
The gate's reservation count is in-memory and resets on AGC restart, so a brief post-restart burst of acquisitions is normal.

**Diagnostics.**

```sh
# Compare admission rejections against successful acquisitions for the group.
# A sustained gap with rejections rising = the ceiling is the bottleneck.
#   actions_gateway_jobs_admission_rejected_total{namespace, runner_group, reason="ceiling"}
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
- If the group should run more concurrent jobs: raise `maxWorkers` (or the top `priorityTiers` threshold) on the RunnerGroup spec, and ensure the namespace `ResourceQuota` has matching headroom — otherwise the rejections simply reappear as `reason="quota"` and the [ResourceQuota path](#jobs-failing-due-to-namespace-resourcequota-exhaustion) becomes the new bottleneck.
- If rejections appear with worker pods **below** the ceiling, suspect leaked reservations from pods that never reached a terminal phase — check for [stuck-Pending pods](#worker-pod-reaped-while-pending-workerpodstuckpending); the gate's slot is released when the job completes or its pod is reaped.
  An AGC restart clears any stale in-memory reservation.

---

## Worker Pod Fails to Start After Secure-by-Default SecurityContext

**Symptoms.** A worker pod that previously ran now stays in `CreateContainerConfigError` or `Pending`, or is rejected at admission. `kubectl describe pod` shows one of:
- `Error: container has runAsNonRoot and image has non-numeric user (<name>), cannot verify user is non-root` — the AGC stamped `runAsNonRoot: true` (every profile except `privileged`) and the image declares its user **by name**, which kubelet cannot verify against a numeric UID.
  The **default** `ghcr.io/actions/actions-runner` image (`USER runner`) is handled automatically — the AGC gap-fills `runAsUser: 1001` so kubelet can verify it (Q115).
  You hit this only with a **custom/third-party** runner image whose named user is **not** UID 1001, so the auto-stamped 1001 doesn't match what its `USER` resolves to, or whose image has no numeric UID at all.
- `Error: container has runAsNonRoot and image will run as root` — same stamp, but the worker image's default user is `root` (UID 0).
- A PodSecurity admission denial naming `allowPrivilegeEscalation != false` or `unrestricted capabilities` — the namespace is on `securityProfile: restricted` and a tenant container needs `sudo` or extra capabilities.

**Likely causes.**
- A **custom** worker image declares a **named** (non-numeric) user other than the default runner's UID 1001.
  The AGC's secure-by-default gap-fill stamps `runAsUser: 1001` to match the upstream `actions-runner` image; an image whose user is a different UID still needs its own numeric UID declared.
  (The default `actions-runner` image needs no action.)
- The worker image runs as root by default (common for custom or third-party runner images).
  The AGC's secure-by-default `runAsNonRoot: true` then blocks it.
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

  Note: a `podTemplate` edit takes effect on the next acquired job — the AGC re-reads the RunnerGroup at pod-build time, so no AGC restart is needed (Q117).
  Pods already running keep the template they were built with.
- Root-based image that must run as root: the defaults are gap-fill only — set an explicit `securityContext.runAsNonRoot: false` (and `runAsUser`/`runAsGroup` as needed) on the runner container in the RunnerGroup `podTemplate`.
  No profile escalation is required for `baseline`.
- Job genuinely needs `sudo`/capabilities: move that workload to a `baseline` `ActionsGateway` (the default), which does not stamp the privilege-escalation/capability floor.
  Reserve `restricted` for workloads that can run without them.
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
- **Unintentional drift:** re-applying an older manifest, or a Helm/Kustomize render that **omits** `securityProfile` (it then re-defaults to `baseline`) while the live object is on `restricted`.
  An empty/absent value is compared as `baseline`, so an omitted field reads as a downgrade — this is the guard working as intended, catching a silent weakening.

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
  Remove the annotation afterward if you want future accidental downgrades to keep being blocked.
  PSA enforce is namespace-scoped, so the new (looser) profile applies to *future* worker pods once the GMC re-stamps the namespace label; pods already running are not re-evaluated.
- **If the downgrade is accidental (drift):** do **not** add the annotation — fix the manifest to match the live profile (set `securityProfile: restricted`, or stop omitting it) so GitOps stops trying to weaken the namespace.

> Note: this guard catches *silent* downgrades; it is not an absolute boundary.
> Anyone with edit access to the CR can set the annotation, and an operator with direct namespace `patch` rights can change the PSA labels regardless.
> See [§5.3 — No silent profile downgrades](../design/05-security.md#no-silent-profile-downgrades).

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

**Why it is enforced.** Every per-tenant resource the GMC provisions — the AGC Deployment, the proxy Deployment, Services, NetworkPolicies, RoleBindings — has a fixed, namespace-scoped name.
Two CRs fight over those objects, and because each CR's `securityProfile` drives the namespace's Pod Security Admission labels, two CRs with different profiles make the GMC flap those labels (intermittently admitting privileged pods).
Deleting either CR then tears down the survivor's infrastructure.

**Resolution.** Use one `ActionsGateway` per namespace.
To run a second logical gateway, give it its own namespace (the guard is per-namespace, so a different namespace's first CR is admitted).
To rename or replace an existing CR, delete the old one and wait for teardown to complete before creating the replacement.

> **`v2alpha1` lifts this restriction.** The singleton guard is a `v1alpha1`-only constraint rooted in fixed per-tenant resource names.
> The `v2alpha1` (`actions-gateway.com`) API supports **multiple `ActionsGateway`s per namespace**: every derived resource is named per gateway (`<gateway>-agc`, `<gateway>-worker`, …) and each gateway's AGC reconciles only its own `RunnerSet`s, so they never contend. `securityProfile` also moved off the gateway onto the namespace, so co-located gateways share one Pod Security posture instead of flapping it.
> See ["Multiple v2 gateways in one namespace"](#multiple-v2-gateways-in-one-namespace-naming-scoping-prerequisites) below.

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

The v2 `EgressProxy`'s `spec.noProxyCIDRs` is gated by the same guard (webhook `vegressproxy-v2alpha1.kb.io`, field path `spec.noProxyCIDRs[N]`).
Because the v2 proxy carries no `gitHubURL` of its own, the v2 guard also runs from the *referrer* side: a v2 `ActionsGateway` (`spec.defaultProxyRef`) or `RunnerSet` (`spec.proxyRef`) write that binds a GitHub host to a proxy whose `noProxyCIDRs` exclude it is rejected too, e.g.:

```
admission webhook "vactionsgateway-v2alpha1.kb.io" denied the request:
spec.defaultProxyRef: EgressProxy "corp-proxy" spec.noProxyCIDRs[0]:
".corp.example" would route GitHub traffic (ghes.corp.example) around the
per-tenant egress proxy, ...
```

**Likely cause.** A `spec.proxy.noProxyCIDRs` (v1 `ActionsGateway`) or `spec.noProxyCIDRs` (v2 `EgressProxy`) entry NO_PROXY-matches a GitHub host: `github.com`, `.github.com`, `api.github.com`, `githubusercontent.com`, `ghcr.io`, your configured `gitHubURL` host (including a GitHub Enterprise Server host — on v2 that is the `gitHubURL` of every gateway that references the proxy, directly or via a `RunnerSet`), or an over-broad suffix like `.com` that covers them.

**Why it is enforced.** `noProxyCIDRs` is threaded into the AGC/worker `NO_PROXY` env var, where a hostname entry is a domain-suffix match.
If it matches a GitHub host, that tenant's GitHub traffic skips the per-tenant egress proxy — defeating the egress-IP attribution that isolates tenants.
On v2 the check runs on **both** the proxy write and the gateway/`RunnerSet` write, so the conflicting pair is rejected whichever side is applied last; a rejection on the referrer side names the conflicting `EgressProxy` and entry.

**Resolution.** Remove the GitHub-matching entry — GitHub must always traverse the proxy. `noProxyCIDRs` is for *internal* destinations only and accepts CIDRs (`10.0.0.0/8`), bare IPs, and non-GitHub domain suffixes (`svc.cluster.local`, `internal.example.com`).
Note the guard cannot detect a **CIDR/IP range** that happens to cover GitHub's (rotating) published ranges — never add those either; that residual is the operator's responsibility.

---

## Privileged Worker Container Rejected by Admission

**Symptoms.** An `ActionsGateway` whose `runnerGroups[].podTemplate` requests a privileged container or init container is rejected by the GMC validating webhook with:

```
admission webhook "vactionsgateway-v1alpha1.kb.io" denied the request:
runnerGroups[0]: privileged containers are not permitted in worker pods
(container "runner")
```

**Likely cause.** The CR requests `securityContext.privileged: true` while `spec.securityProfile` is `baseline` (the default) or `restricted`.
Privileged worker containers are permitted **only** under the explicit `securityProfile: privileged` opt-in, which also stamps the namespace's Pod Security Admission level to `privileged` so the pod is actually admittable.

**Resolution.**
- If the privileged worker is intended (e.g. the Kata/DinD pattern), set `spec.securityProfile: privileged` on the same `ActionsGateway`.
  This requires the namespace to be **eligible** for privileged — a platform admin must label it `actions-gateway.github.com/privileged-profile=allowed` (see [Privileged securityProfile Rejected](#privileged-securityprofile-rejected-namespace-not-eligible) below).
  Privileged is a deliberate, audited relaxation — pair it with a sandboxed `runtimeClassName` (Kata, gVisor) per [§5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in).
- If the privileged flag is accidental, remove `securityContext.privileged: true` from the pod template; the secure-by-default profiles reject it on purpose.

> Note: this webhook check only covers the GMC-managed `ActionsGateway` path.
> A directly-applied `RunnerGroup` CR bypasses the webhook entirely — Pod Security Admission (stamped per the namespace's profile) is the real enforcement backstop for both paths.

---

## `RunnerTemplate` Rejected: Reserved Pod Field (`v2alpha1`)

> Applies to the `v2alpha1` (`actions-gateway.com`) API, currently early-adopter only.
> The `v1alpha1` path uses the `ActionsGateway`/`RunnerGroup` checks above.

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

**Likely cause.** A worker pod's identity and egress wiring are controller-enforced invariants.
In `v1alpha1` the AGC silently overwrote these fields when it built the pod; `v2alpha1` makes them an author-time rejection so a template fails closed instead of being rewritten behind your back.

- **Reserved proxy env vars** (`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`/`PROXY_CA_CERT_PATH`, matched case-insensitively) are injected by the AGC and may not be set in a template — on either kind.
- **Privileged containers** are rejected on the namespaced `RunnerTemplate` (a tenant must not self-author a privileged worker shape) but **allowed** on the cluster-scoped `ClusterRunnerTemplate`, which only a platform administrator can create.

The scalar reserved pod-level fields (`serviceAccountName`, `host{PID,Network,IPC}`, `automountServiceAccountToken`) are rejected by the CRD's own validation rules with a similar "is reserved" message.

**Resolution.**
- Remove the reserved proxy env vars from every container and init container; the AGC sets them itself.
- For a **privileged** worker shape (Kata/DinD/sysbox), have a platform administrator publish it as a `ClusterRunnerTemplate` and reference it from the `RunnerSet`'s `templateRef` with `kind: ClusterRunnerTemplate`.
  Privileged pods still require the namespace's Pod Security Admission level to admit them (stamped from the effective `securityProfile`), which remains the runtime backstop.

---

## `RunnerSet` Rejected: `acquisitionProtocol` (`v2alpha1`, early-adopter)

> Applies to the `v2alpha1` (`actions-gateway.com`) API. `acquisitionProtocol` selects how the AGC acquires jobs for a runner set: `ScaleSet` (**the default** as of Q264 P5 — the runner-scale-set message-queue protocol, Q264 Option E) or `Classic` (**deprecated** — the per-runner broker protocol).
> Leaving the field unset selects `ScaleSet`.
> Both protocols match on the whole `runnerLabels` set, so a multi-label set no longer has to pin `Classic`.

**Symptoms.** Creating or updating a `RunnerSet` is rejected with one of:

```
The RunnerSet "linux" is invalid: spec.acquisitionProtocol: Invalid value: "string":
acquisitionProtocol is immutable; create a new RunnerSet to change the acquisition
protocol
```

```
admission webhook "vrunnerset-v2alpha1.kb.io" denied the request: ScaleSet
runnerLabels[0] "linux" is already used by RunnerSet "other-set" registered against
GitHub scope "github.com/acme"; a ScaleSet set's FIRST runnerLabel is its scale-set
name at GitHub, so two sets sharing it would drive one scale set. Pick a distinct
first label (later labels may overlap freely)
```

```
admission webhook "vrunnerset-v2alpha1.kb.io" denied the request: ScaleSet
runnerLabels[0] "linux" is already claimed by another RunnerSet registered against
GitHub scope "github.com/acme"; a ScaleSet set's FIRST runnerLabel is its scale-set
name at GitHub, so two sets sharing it would drive one scale set, each acquiring the
other's jobs. Pick a distinct first label (ask your platform administrator which
scale-set names that GitHub scope already holds)
```

**Likely cause & resolution.**

- **Duplicate first label in one GitHub scope (GMC webhook).** A `ScaleSet` set's **first** `runnerLabel` is the name of its scale-set object at GitHub, so two sets may not share it: they would drive one scale set from two controllers.
  Give each set a distinct *first* label.
  Labels after the first are ordinary match targets and **may** be shared: `[gpu, linux]` and `[arm64, linux]` coexist happily, and a job asking for `runs-on: linux` alone reaches whichever GitHub picks.
  (Two `Classic` sets are unaffected: they register no scale set at all.)

  **The uniqueness boundary is the GitHub org, enterprise, or repo that the set's gateway `githubURL` names, not the namespace.** A scale set is adopted by name against the Actions service that URL reaches, so two `RunnerSet`s in *different namespaces* whose gateways point at the *same* org still collide, and each tenant's AGC would acquire the other's jobs.
  That is why the second message names a GitHub scope but not the holding set: the conflicting `RunnerSet` may belong to another tenant, so it is withheld from the rejection and written to the GMC controller log instead.
  A platform admin can find it there, or with:

  ```bash
  kubectl get runnersets -A -o jsonpath='{range .items[?(@.spec.acquisitionProtocol=="ScaleSet")]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.spec.runnerLabels[0]}{"\n"}{end}'
  ```

  The same label under a gateway bound to a **different** org, enterprise, or repo is free, because that is a different scale-set namespace at GitHub.
  Org names are matched case-insensitively (`github.com/Acme` and `github.com/acme` are one scope), matching how GitHub resolves them.

- **The rejection can land on the `ActionsGateway` instead.** If a `RunnerSet` is applied *before* its gateway exists, admission has no `githubURL` to resolve and the set is admitted (references resolve at runtime).
  The conflict is then caught when the gateway arrives:

  ```
  admission webhook "vactionsgateway-v2alpha1.kb.io" denied the request:
  spec.githubURL binds GitHub scope "github.com/acme", where RunnerSet "linux-set" in
  this namespace would claim scale-set name "linux" — a name already claimed by another
  RunnerSet registered against that scope
  ```

  Rename the named set's first `runnerLabel`, or bind the gateway to a different GitHub org, enterprise, or repo.
- **Immutable (CRD CEL).** Switching a live set between `Classic` and `ScaleSet` is a full re-registration storm, so the field is frozen after creation.
  To change it, create a **new** `RunnerSet` (with a distinct name and first label) and delete the old one.
- **More than one label is no longer rejected.** Until Q726 a `ScaleSet` set had to declare exactly one `runnerLabel` and a multi-label set had to set `acquisitionProtocol: Classic`; both CEL rules are gone.
  If you still see one of those messages, the cluster is running an older CRD than the controller; reapply the chart's CRDs.

---

## `RunnerSet` Rejected: `nodeShare.allocatable` Declares Neither cpu Nor memory

**Symptoms.** Creating or updating a `RunnerSet` that selects the `NodeShare` sizing profile is rejected:

```
RunnerSet.actions-gateway.com "gpu-linux" is invalid: spec.sizing.nodeShare.allocatable:
Invalid value: sizing.nodeShare.allocatable must declare cpu, memory, or both; other
resources are ignored
```

**Likely cause.** The envelope names only resources the profile never divides — most often the GPU key, because that is the resource the profile exists to bin-pack *against*:

```yaml
spec:
  sizing:
    profile: NodeShare
    nodeShare:
      allocatable: { nvidia.com/gpu: "4" }   # rejected: no cpu, no memory
      workersPerNode: 4
```

`NodeShare` only ever derives the `cpu` and `memory` requests — extended resources pass through from the template byte-identical, because the GPU count is part of the shape's job-selected identity.
An envelope carrying neither key therefore derives nothing at all, and before this rule it was admitted: `status.sizingProfileState` reported `Active` while every worker pod ran the template's untouched ask.

**Resolution.** Declare the node's allocatable `cpu` and/or `memory` — the envelope the profile actually divides — and keep the GPU count in `workersPerNode`, which is the divisor:

```yaml
spec:
  sizing:
    profile: NodeShare
    nodeShare:
      allocatable: { cpu: "15", memory: 60Gi }   # from `kubectl describe node`
      workersPerNode: 4                          # the node's GPU count
```

Declaring only one of the two is valid: the other resource keeps the template's ask.
Take the numbers from `kubectl describe node <a-node-of-that-shape>` and subtract whatever system or sidecar overhead you reserve.
Full walkthrough: [worker right-sizing](worker-rightsizing.md#sizing-profiles-opt-in-auto-apply).

---

## `RunnerSet` Stuck `Ready=False` With a `NotFound` Reason (`v2alpha1`)

> Applies to the `v2alpha1` (`actions-gateway.com`) API, currently early-adopter only.

**Symptoms.** A `RunnerSet` never starts acquiring jobs and `kubectl describe runnerset <name>` shows `Ready=False` with one of:

```
Reason: GatewayNotFound    Message: ActionsGateway "gw" not found in namespace "team-a"
Reason: TemplateNotFound   Message: RunnerTemplate "dind-large" not found in namespace "team-a"
Reason: ProxyNotFound      Message: EgressProxy "shared" not found in namespace "team-a"
```

**Likely cause — this is by design, not an error.** v2 resolves a `RunnerSet`'s references (`gatewayRef`, `templateRef`, `proxyRef`/the gateway's `defaultProxyRef`) **at runtime**, not at admission, so applying a directory in any order converges (GitOps-friendly).
Until every reference resolves the set sits `Ready=False` with the specific `*NotFound` reason and **provisions no worker pods** — fail-closed, so no traffic is ever permitted in the gap.
The AGC watches the referents and flips the set to `Ready` the moment the missing object syncs; **no re-apply of the `RunnerSet` is needed**.

A `ProxyNotFound` here means a `proxyRef`/`defaultProxyRef` **names an `EgressProxy` that does not exist** — a named-but-missing reference fails closed (it does not silently fall back to direct egress).
Apply the named `EgressProxy`, or remove the reference if you want direct egress. **Unset everywhere is not an error:** a `RunnerSet` with no `proxyRef` under a gateway with no `defaultProxyRef` resolves to **direct egress** (`Ready=True`, `status.proxyMode: Direct`, advisory `EgressUnattributed` condition), not `ProxyNotFound` — see ["RunnerSet reports `EgressUnattributed`"](#runnerset-or-gateway-reports-egressunattributed-direct-egress-v2alpha1).

A `ClusterRunnerTemplate` ref (`templateRef.kind: ClusterRunnerTemplate`) resolves the same way: `TemplateNotFound` means the named cluster-scoped template does not exist yet.
The AGC reads it through a per-gateway `ClusterRoleBinding` to the shipped `agc-clusterrunnertemplate-reader` ClusterRole that the GMC creates with the gateway — so if every namespaced reference resolves but a `ClusterRunnerTemplate` ref stays `TemplateNotFound`, confirm a platform administrator has created that `ClusterRunnerTemplate` (it is cluster-scoped and platform-authored; tenants cannot create it).

**Resolution.** Apply the missing object (`ActionsGateway`, `RunnerTemplate`/`ClusterRunnerTemplate`, or `EgressProxy`) named in the message; the set self-heals on the next watch event.
Confirm the referent's name and namespace match the `*Ref` exactly (references resolve in the `RunnerSet`'s own namespace).

**`GatewayTerminating` — the gateway is being deleted, not missing.** A set reports this while its `ActionsGateway` carries a deletion timestamp.
It is not a resolution failure: the AGC has stopped acquiring and reaped the set's worker pods, because it is those pods' only reaper and goes away with the gateway.
The set settles at `GatewayNotFound` once the gateway is gone, and recovers on its own if the gateway is re-applied.
See [Worker Pods Reaped on Gateway Teardown](#worker-pods-reaped-on-gateway-teardown-workerpodsreapedongatewayteardown).

**`TemplateDeleted` / `ProxyDeleted` — the referent existed and vanished.** A set whose references *had* resolved reports these deletion-specific reasons instead of the generic `*NotFound` when its previously-resolved `RunnerTemplate`/`ClusterRunnerTemplate` or `EgressProxy` is deleted out from under it (the set's own `status.templateSource` / `status.proxyMode: Proxied` is the evidence of the prior resolution).
Deleting a shared referent is allowed by design — deletion **degrades referrers rather than blocking** (no finalizer holds the referent), and the set fails closed exactly like `*NotFound`: no new worker pods until the referent is restored.
Re-apply the deleted object (or point the set at another) and the set self-heals on the watch event.
If the set's spec was *edited* to a dangling name rather than the referent being deleted, the plain `*NotFound` reason is reported instead.

**Runtime-failure reasons (not by design).** Unlike the `*NotFound` reasons above — which are the expected fail-closed state while a reference is still syncing — a `RunnerSet` whose references have all resolved can still report `Ready=False` for a genuine post-resolution failure.
These name the failing step in the message and clear on the next successful reconcile:

```
Reason: TokenUnavailable         Message: failed to obtain GitHub App installation token: ...
Reason: AgentProvisioningFailed  Message: failed to provision agent Secrets: ...
Reason: ListenerStartFailed      Message: listener goroutines failed to start: ...
```

- `TokenUnavailable` / `AgentProvisioningFailed` — the AGC could not fetch a GitHub App installation token, or could not register the listener agents' Secrets.
  Check the gateway's credential Secret and the AGC logs (see ["GitHub App Secret Misconfiguration"](#github-app-secret-misconfiguration)); the AGC also emits a matching Warning event (`TokenUnavailable` / `AgentPoolError`).
- `ListenerStartFailed` — the listener goroutines could not be (re)started; the AGC emits a `ListenerStartFailed` Warning event with the underlying error.

A running listener always wins: once at least one session is polling, the set reports `Ready=True` / `ListenerActive` even if a prior start attempt logged an error.

**Session-failure conditions pushed by the listener.** Both acquisition protocols push session-failure conditions onto the `RunnerSet`.
On a `Classic`-protocol set the listener goroutines push the same conditions they set on a v1 `RunnerGroup` (the classic acquisition machinery is shared between the two kinds); on a `ScaleSet` set (the default) the scale-set listener pushes the same vocabulary:

| Condition | Reason | Meaning |
| --- | --- | --- |
| `RateLimited=True` | `SustainedRateLimit` | GitHub has answered message polling with 429 for over ten minutes. |
| `RunnerVersionTooOld=True` | `VersionTooOld` | *(Classic only)* GitHub rejected the configured runner version at session creation — see the v1 guidance under [AGC CrashLoopBackOff or Not Acquiring Jobs](#agc-crashloopbackoff-or-not-acquiring-jobs); the fix (update `workerImage`) is the same. This class cannot occur on a `ScaleSet` set: the scale-set protocol carries no runner version at session creation (the per-job JIT config is minted server-side). The reconciler's own reading of `workerImage` covers both tiers — see [Worker Image Runner Version](#worker-image-runner-version). |
| `Degraded=True` | `Unauthorized` | A session call was rejected as unauthorized — the GitHub App / agent credentials are invalid or revoked. On a `ScaleSet` set this covers session create *and* the queue-token refresh, and a `SessionUnauthorized` Warning event names the rejected call. |

All are advisory (abnormal-is-`True`) and do not gate `Ready`.
The two protocols differ in recovery behaviour: the **classic** listener only sets the abnormal state (a stale `True` can outlive the episode until the goroutine restarts), while the **`ScaleSet`** listener also publishes the healthy state — `Degraded=False/SessionAuthorized` and `RateLimited=False/PollingHealthy` appear on every healthy `ScaleSet` set once its listener starts — and clears an abnormal condition as soon as the session recovers (a successful poll, token refresh, or session re-create).
Listener-pushed conditions and events are recorded on the `RunnerSet` on its next reconcile, so they can lag the incident by up to one reconcile interval.

---

## `RunnerSet` Stuck `Ready=False` With `RunnerGroupNotFound`

**Symptoms.** A `RunnerSet` never starts acquiring jobs, no scale set appears in the organization's Actions settings, and `kubectl describe runnerset <name>` shows:

```
Reason: RunnerGroupNotFound
Message: scale-set listener failed to start: scalesetlistener: no such runner group at GitHub: "team-a"
```

**Likely cause.** The set's `spec.runnerGroup`, or its gateway's `spec.defaultRunnerGroup`, names a GitHub runner group the installation does not have.
Unlike the `*NotFound` reasons above, the missing object is at **GitHub**, not in the cluster, so nothing in the namespace will make it resolve.

The set registers **no** scale set while this holds, and that is deliberate.
The runner group is GitHub's authorization point for which repositories may target these runners, so registering into the default group instead would widen the boundary the operator was narrowing, at exactly the moment they mistyped the name.

**Resolution.**

1. List the organization's runner groups (**Settings → Actions → Runner groups**) and compare the name.
   It is case-sensitive and must match exactly, whitespace included.
2. If the group does not exist, a platform administrator creates it and scopes its repository access to the tenant's repositories.
   GAG never creates runner groups; see [tenant onboarding](tenant-onboarding.md#bind-a-runner-set-to-a-github-runner-group).
3. Fix the name (or create the group) and the set self-heals on the next reconcile: no re-apply needed.

**If the message says `resolve runner group` instead**, the lookup itself failed (a 5xx, a rate limit, a proxy fault) rather than returning an empty result.
That is an outage, not a misconfiguration; the set retries with backoff and recovers on its own.

**A group change on a live set is a listener restart.** Editing `runnerGroup` (or the gateway's `defaultRunnerGroup`) re-registers the scale set into the new group and restarts the set's listener, so the set stops acquiring for a moment while it re-opens its session.
Worker pods already running their jobs are untouched.
Clearing the field does **not** move the scale set back to the default group: widening the boundary stays an explicit act at GitHub.

---

## v2 `ActionsGateway` Stuck `Ready=False` (`CredentialUnavailable` / `ProxyNotFound`)

> Applies to the `v2alpha1` (`actions-gateway.com`) API, currently early-adopter only.
> The v1 `ActionsGateway` provisioning checks are above (["GMC Not Provisioning Tenant Resources"](#gmc-not-provisioning-tenant-resources)).

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

- **`CredentialUnavailable`** — the Secret named by `spec.credentials.githubApp.name` does not exist in the gateway's namespace.
  The AGC mounts the GitHub App credential as files, so without it there is nothing to provision.
- **`ProxyNotFound`** — `spec.defaultProxyRef` **names an `EgressProxy` that does not exist**.
  The AGC's control-plane egress is routed through that proxy, so a dangling reference fails closed.
  Note this fires only for a *named but missing* proxy: an **unset** `defaultProxyRef` is **not** an error — it means **direct egress** (the gateway reaches Ready with `status.proxyMode: Direct` and an advisory `EgressUnattributed` condition; see below).
  Apply the named `EgressProxy`, or clear `defaultProxyRef` to use direct egress.

Unlike a `RunnerSet`'s reference resolution, these are the *gateway's own* preconditions; once the Secret or `EgressProxy` appears the gateway reconciles and the AGC Deployment is created (the gateway watches both).
Note that the proxy **pool** is reconciled separately by the `EgressProxy` reconciler — the gateway only references it; and the namespace Pod Security Admission labels are stamped by the namespace PSA reconciler from the `actions-gateway.com/security-profile` label, which the gateway *reads* (to thread `SECURITY_PROFILE` to the AGC) but never stamps.

**Events in `kubectl describe` (Q305).** The v2 `ActionsGateway` and `EgressProxy` reconcilers emit Kubernetes Events on their meaningful transitions, so `kubectl describe actionsgateway <name>` / `kubectl describe egressproxy <name>` no longer shows an empty Events list while a tenant is coming up.
Events are emitted only on a genuine state change (never on every reconcile):

| Object | Reason | Type | Meaning |
| --- | --- | --- | --- |
| `ActionsGateway` | `Provisioning` | Normal | Provisioning of the AGC control plane started (once per object). |
| `ActionsGateway` | `SecretNotFound` / `ProxyNotFound` | Warning | A precondition failed closed — the credential Secret or `defaultProxyRef`'d `EgressProxy` is missing (see above). |
| `ActionsGateway` | `MetricsCertificateIssued` | Normal | The per-tenant metrics mTLS certificate was issued or rotated (near expiry). |
| `ActionsGateway` | `ProvisioningFailed` | Warning | A reconcile failed partway through provisioning; the message names the failing step (mirrors the `Degraded` condition). |
| `ActionsGateway` | `Ready` | Normal | The AGC control plane became available. |
| `ActionsGateway` | `AGCNotReady` | Warning | The AGC Deployment has no ready replica yet (or lost it). |
| `ActionsGateway` | `RunnerSetsImpaired` / `AllRunnerSetsHealthy` | Warning / Normal | A bound `RunnerSet` became impaired, or the last impaired set recovered (the `RunnerSetsDegraded` rollup, Q304). |
| `ActionsGateway` | `ReconcileSucceeded` | Normal | Provisioning recovered after a prior `ProvisioningFailed`. |
| `ActionsGateway` | `TeardownIncomplete` | Warning | Teardown could not confirm every child deleted (a delete failed, or a child lingers under another controller's finalizer); the cleanup finalizer is retained and teardown retries (Q328 — see [ActionsGateway Stuck Deleting](#actionsgateway-stuck-deleting-teardown-blocked-on-a-failing-delete)). |
| `EgressProxy` | `ProxyCertificateIssued` | Normal | The self-signed proxy TLS certificate was issued or rotated (near expiry). |
| `EgressProxy` | `ProvisioningFailed` | Warning | A reconcile failed partway through provisioning the proxy pool; the message names the failing step. |
| `EgressProxy` | `ProxyReady` / `ProxyNotReady` | Normal / Warning | The proxy pool reached / lost its `minReplicas` ready pods. |
| `EgressProxy` | `ReconcileSucceeded` | Normal | Proxy-pool provisioning recovered after a prior `ProvisioningFailed`. |

---

## `RunnerSet` or gateway reports `EgressUnattributed` (direct egress) (`v2alpha1`)

> Applies to the `v2alpha1` (`actions-gateway.com`) API, currently early-adopter only.

**Symptoms.** `kubectl get actionsgateway,runnerset -n <ns>` shows `Egress: Direct`, and `kubectl describe` shows an `EgressUnattributed=True` condition (`Reason: DirectEgress`).
The object is otherwise `Ready`.

**This is not an error — it is informational.** It means the gateway has no `spec.defaultProxyRef` and/or the `RunnerSet` has no `spec.proxyRef`, so egress goes **directly** to GitHub instead of through an `EgressProxy` (appendix-h §H.10).
Direct egress is a supported mode and never makes the object `NotReady`; the condition exists only so an operator can see at a glance that the workload has **no per-tenant egress IP identity** — the trade you make by not attaching a proxy.

**What is still guaranteed.** Egress is still **restricted**: the GMC's default-deny egress NetworkPolicy permits only **DNS (cluster DNS) + the GitHub CIDR allowlist** for workers (plus the kube API server for the AGC), and the IP-range refresh keeps that allowlist current.
A direct-egress worker cannot reach an arbitrary internet host.
What you lose is only the stable per-tenant *source IP* (needed for GitHub IP-allowlisting / EMU, incident attribution, and avoiding shared-NAT throttling).

**If you wanted attribution.** Create an `EgressProxy` in the namespace and set `spec.defaultProxyRef` on the gateway (or `spec.proxyRef` on the specific `RunnerSet`).
The object flips to `proxyMode: Proxied` and the condition clears (`EgressUnattributed=False`).
See [tenant-onboarding — Proxy-less onboarding](tenant-onboarding.md#proxy-less-onboarding-direct-egress).

**If GitHub egress fails in direct mode.** Confirm (1) the cluster CNI actually enforces egress NetworkPolicy (kindnet does not — see [tenant-onboarding Pre-Conditions](tenant-onboarding.md#pre-conditions)), and (2) the GMC's GitHub IP-range refresh has run — the direct-egress AGC + workload NetworkPolicies carry the GitHub CIDR allowlist only after the first fetch; `kubectl get networkpolicy <gateway>-workload -o yaml` should show `ipBlock` egress peers on port 443.

**Resolution.** Create the GitHub App Secret (see ["GitHub App Secret Misconfiguration"](#github-app-secret-misconfiguration) for the required keys) and/or the `EgressProxy` named by `defaultProxyRef`, in the gateway's namespace.
The gateway self-heals on the next watch event.

---

## `AGCAutoscalingUnavailable` — the VPA CRDs are not installed

> Applies to the `v2alpha1` (`actions-gateway.com`) API, currently early-adopter only.

**Symptoms.** You set `ActionsGateway.spec.agcAutoscaling`, but `kubectl get vpa -n <ns>` finds nothing named `<gateway>-agc`. `kubectl describe actionsgateway <gateway> -n <ns>` shows:

```
Conditions:
  Type:     AGCAutoscalingUnavailable
  Status:   True
  Reason:   VPACRDNotInstalled
  Message:  spec.agcAutoscaling is set but the VerticalPodAutoscaler.autoscaling.k8s.io CRD is not
            installed in this cluster, so no VerticalPodAutoscaler was created for "<gateway>-agc" ...
Events:
  Warning  VPACRDNotInstalled  Reconcile  ...
```

**Cause.** The `autoscaling.k8s.io` CRDs are **not** part of core Kubernetes.
They ship with the [Kubernetes vertical-pod-autoscaler](https://github.com/kubernetes/autoscaler/tree/master/vertical-pod-autoscaler) add-on, which your cluster does not have installed (or has only partly installed).

**This is not an outage.** The gateway is fully provisioned and `Ready=True`; the AGC runs on its `spec.agcResources` sizing (or the platform default). `AGCAutoscalingUnavailable` is advisory and never gates `Ready` — it exists so an unsatisfiable opt-in is visible instead of silently doing nothing.
The GMC does not retry the write against a kind the apiserver does not serve, so there is no reconcile churn either.

**Resolution — pick one:**

1. **Install the vertical-pod-autoscaler** (CRDs + recommender + updater + admission-controller), e.g. via your platform's add-on manager or the autoscaler repo's `hack/vpa-up.sh`.
   Note the CRDs alone are not enough for `mode: Initial`/`Recreate` to actuate — those need the admission-controller and updater running.
   The gateway **re-probes every 10 minutes**, so it picks the autoscaler up on its own; `kubectl annotate actionsgateway <gateway> -n <ns> kubectl.kubernetes.io/restartedAt="$(date -Is)" --overwrite` forces an immediate reconcile if you don't want to wait.
2. **Remove the opt-in** — delete `spec.agcAutoscaling` and size the AGC with `spec.agcResources` instead ([tenant-onboarding](tenant-onboarding.md#tuning-agc-control-plane-resources)).
   The condition returns to `False`/`AGCAutoscalingDisabled`.

**Verify.** Once the CRDs are present the condition flips to `False`/`AGCAutoscalingActive` and `kubectl get vpa <gateway>-agc -n <ns>` returns the managed object.
If it exists but the AGC's requests never change, check the autoscaler's own components (`kubectl get pods -n kube-system -l app=vpa-recommender`) and remember `mode: Off` — the default — is recommendation-only by design: read `kubectl describe vpa <gateway>-agc` for the recommendation and switch to `mode: Recreate` when you want it applied.

**Same symptom at the chart level.** The GMC's own `vpa.enabled` value has no runtime degradation path — Helm renders the object at install time, so enabling it without the CRDs fails `helm install`/`helm upgrade` with `no matches for kind "VerticalPodAutoscaler"`.
Install the autoscaler first, or set `vpa.enabled=false`.

---

## Multiple v2 gateways in one namespace: naming, scoping, prerequisites

> Applies to the `v2alpha1` (`actions-gateway.com`) API, currently early-adopter only.

The `v2alpha1` API supports **multiple `ActionsGateway`s per namespace** (unlike `v1alpha1` — see ["Second ActionsGateway in a Namespace Rejected"](#second-actionsgateway-in-a-namespace-rejected-singleton-guard)).
A few operator-visible facts and failure modes:

- **Per-gateway resource names.** Every resource a gateway derives is prefixed with the gateway name: `<gateway>-agc` (AGC Deployment / ServiceAccount / RoleBinding / Service / AGC NetworkPolicy), `<gateway>-worker` (worker ServiceAccount), `<gateway>-workload` (workload NetworkPolicy), `<gateway>-agc-metrics-tls` / `-agc-metrics-client` (metrics Secrets).
  To list one gateway's resources: `kubectl get all,networkpolicy,secret -n <ns> -l actions-gateway.com/gateway=<gateway>`.
- **52-character name cap.** An `ActionsGateway`, `RunnerSet`, `RunnerTemplate`, `ClusterRunnerTemplate`, or `EgressProxy` whose `metadata.name` exceeds **52 characters** is rejected at admission (`metadata.name must be at most 52 characters`).
  The cap reserves room for the derived suffixes above so a label value / Service name stays within RFC 1123's 63-character ceiling.
  Use a shorter name.
- **Per-gateway scoping needs Kubernetes ≥ 1.31.** Each AGC reconciles only the `RunnerSet`s whose `spec.gatewayRef.name` targets it, using a server-side CRD field selector (KEP-4358).
  That selector is **alpha-off in Kubernetes 1.30**: on a 1.30 cluster an AGC's `RunnerSet` informer fails to sync (`field label not supported`) and the AGC pod will not become ready.
  If a v2 AGC `CrashLoopBackOff`s with a `RunnerSet` list/watch error, check the cluster is ≥ 1.31.
  (Single-gateway-per-namespace v2 still requires ≥ 1.31 once more than one gateway exists; for one gateway the scoping is a no-op but the selector is still applied.)
- **Per-gateway garbage collection.** Deleting one gateway removes only its own children (owner-referenced, per-gateway-named); a neighbor's gateway, AGC, and `RunnerSet`s are untouched.
  The one cluster-scoped child — the `agc-clusterrunnertemplate-reader.<ns>.<gateway>` `ClusterRoleBinding` — cannot carry a namespaced owner reference, so the gateway controller deletes it explicitly during teardown.
  If a gateway's `ClusterRoleBinding` lingers after the gateway is gone (e.g. the GMC was down during the delete), it is harmless (it binds a now-absent ServiceAccount) and safe to `kubectl delete clusterrolebinding agc-clusterrunnertemplate-reader.<ns>.<gateway>`.

**Prerequisite.** The five v2 CRDs ship in the separate `actions-gateway-crds-v2` chart, not the main chart.
Install it before creating any v2 object: `helm upgrade --install actions-gateway-crds-v2 oci://ghcr.io/actions-gateway/charts/actions-gateway-crds-v2`.
The shipped `agc-clusterrunnertemplate-reader` ClusterRole is in the main chart.

---

## v2 Objects Not Reconciling After Installing the CRD Chart

> Applies to the `v2alpha1` (`actions-gateway.com`) API, currently early-adopter only.

**Symptom.** You created a v2 `ActionsGateway` or `EgressProxy` and nothing happens — no AGC Deployment, no proxy pool, no status conditions.
The GMC log shows it came up v1-only:

```text
actions-gateway.com/v2alpha1 CRDs not installed; v2 controllers disabled (install the actions-gateway-crds-v2 chart and restart the GMC to enable them)
```

**Cause.** The GMC checks for the `actions-gateway.com/v2alpha1` CRDs **once at startup**.
If they were absent then, it disables the v2 controllers (and the v2 IP-range refresh passes) deliberately — this is the secure, quiet default that keeps a v1-only install from spinning a "no matches for kind" retry loop.
Installing the CRD chart into an already-running cluster does **not** retroactively enable them.

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

> **Note on older GMC builds.** A GMC predating this startup gate (before Q228) started the v2 controllers unconditionally, so on a v1-only install it logged `no matches for kind "ActionsGateway"/"EgressProxy" in version "actions-gateway.com/v2alpha1"` every ~10s and the IP-range reconciler logged `failed to list EgressProxies`.
> The fix is the same — install the CRD chart — or upgrade to a build with the gate, which logs a single info line instead.

> **The per-tenant AGC has the same gate.** Each AGC checks for the `actions-gateway.com/v2alpha1` `RunnerSet` CRD once at startup and, when it is absent, disables its v2 RunnerSet reconciler and runs only the v1 RunnerGroup reconciler, logging `actions-gateway.com/v2alpha1 RunnerSet CRD not installed; v1-only mode, v2 RunnerSet reconciler disabled`.
> An AGC predating this gate (before Q261) registered the RunnerSet reconciler unconditionally, so on a v1-only install its informer cache never synced and the pod **crash-looped** — `mgr.Start` exited after the ~2m cache-sync deadline, the pod restarted, and it repeated.
> If a v1 AGC pod is in `CrashLoopBackOff` with no other cause, upgrade to a build with the gate (Q261+); like the GMC, enabling v2 later requires an AGC restart, which the GMC performs automatically when it rolls the AGC Deployment.

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

**Likely cause.** Whether a namespace may run privileged workers is a **platform** decision, not a tenant one.
Because the tenant owns the `ActionsGateway` CR, they could otherwise self-select the cluster's least-restrictive Pod Security Admission posture simply by creating a CR.
The webhook therefore gates `securityProfile: privileged` behind a label the platform applies to the *namespace* (which the tenant does not own): `actions-gateway.github.com/privileged-profile=allowed`.
Absent the label — or with any other value — privileged is ineligible and rejected.
(The value is the enum keyword `allowed`, not `true`, to avoid the YAML boolean-coercion footgun.)

**Resolution (platform administrator).** If this tenant is approved to run privileged workers, label the namespace:

```bash
kubectl label namespace <tenant-namespace> actions-gateway.github.com/privileged-profile=allowed
```

Apply it with a trusted (administrator) identity — the GMC must never set it itself, and tenants cannot.
Verify:

```bash
kubectl get namespace <tenant-namespace> \
  -o jsonpath='{.metadata.labels.actions-gateway\.github\.com/privileged-profile}'
# Expected: allowed
```

Re-apply the `ActionsGateway`; the create/update is now admitted.
To **revoke** eligibility, remove the label (`kubectl label namespace <ns> actions-gateway.github.com/privileged-profile-`); existing CRs already at `privileged` keep running, but any future create or profile change to `privileged` is rejected again.

> On an **update** that raises an existing CR from a stricter profile to `privileged`, the webhook also requires the `actions-gateway.github.com/allow-profile-downgrade: "true"` annotation (anything → `privileged` is a rank downgrade — see [securityProfile Downgrade Rejected](#securityprofile-downgrade-rejected-by-admission-webhook)).
> Both gates are independent: the namespace label is the platform's eligibility decision, the annotation is the tenant's deliberate relaxation.

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

**Likely cause.** `spec.tracing.sampler` is a fixed enum mapping to the OpenTelemetry SDK's built-in samplers (it is forwarded verbatim as `OTEL_TRACES_SAMPLER`).
A value outside that set — a typo, or one of the SDK's externally-configured samplers (`jaeger_remote`, `xray`) that this field intentionally does not expose — is rejected by the CRD schema before the object is stored.

**Resolution.**
- Pick a supported value.
  For probabilistic sampling use `parentbased_traceidratio` with `spec.tracing.samplerArg: "0.1"` (10%); for all/no traces use `parentbased_always_on` / `always_off`.
- Leave `sampler` unset to use the SDK default (`parentbased_always_on`).
- To *disable* tracing entirely, remove `spec.tracing.endpoint` (an empty endpoint emits no `OTEL_*` env and the AGC keeps its no-op tracer) — the sampler value is irrelevant when no endpoint is set.

See [observability — enabling tracing on GMC-managed AGCs](observability-logging.md#enabling-tracing-on-gmc-managed-agcs) for the full field list.

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

The v2 `ActionsGateway` (`actions-gateway.com`) enforces the same rules — the CRD-schema errors name that group instead, and the webhook error names `vactionsgateway-v2alpha1.kb.io` (v2beta1 writes route through the same validator).

**Likely cause.** `spec.gitHubURL` is a **required** field — the GitHub org, enterprise, or repository URL the gateway's runners register against.
There is no default: a gateway with no URL has nothing to register against.
The CRD enforces a non-empty `https://` value; the GMC validating webhook additionally requires a parseable URL with the https scheme, a host, and at least one path segment (the org/enterprise/owner).
Common misses: the field omitted entirely, an `http://` (non-TLS) URL, or a bare host (`https://github.com`) with no org.

**Resolution.**
- Set `spec.gitHubURL` to an org URL (`https://github.com/my-org`), a single-repo URL (`https://github.com/my-org/my-repo`), or your GitHub Enterprise Server URL (`https://ghes.example.com/my-org`).
- It must use `https://` and name the org/enterprise/owner — and must match where the App in `spec.gitHubAppRef` is installed.
- Setting it through the Helm chart's sample CR?
  Use the `sampleGateway.gitHubURL` value.
  See [tenant-onboarding — Step 2](tenant-onboarding.md#step-2-create-the-actionsgateway-resource).

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

**Symptoms.** Worker pod reaches `Running`, the entrypoint wrapper logs `payload loaded` and starts Runner.Worker, but Runner.Worker exits non-zero almost immediately with a stack trace containing `System.ArgumentNullException: Value cannot be null. (Parameter 'configuredSettings')` originating from `Runner.Common.ConfigurationStore.GetSettings()`.
The job is never reported back to GitHub.

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
- If the agent Secret is missing `encodedJITConfig`: scale the agent pool to zero (`maxListeners: 0` on the RunnerGroup), wait for Secrets to be deleted, then scale back up.
  New agents will be registered via `generate-jitconfig` and carry the blob.
  An agent whose session is in flight is not torn down mid-job — its Secret is deleted on a later reconcile once the session completes (the controller logs `skipping scale-down delete of in-use agent`), so wait for active jobs to drain before expecting the count to reach zero.
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

The webhook recovers seconds later; the same `kubectl apply` succeeds on retry.
Common pattern in CI / e2e suites that change GMC env vars then immediately apply a CR.

**Likely causes.**
- Running a GMC image built before the readyz-gates-webhook fix landed (commit `0eaa30e`).
  The default Kubebuilder scaffold registers `mgr.AddReadyzCheck("readyz", healthz.Ping)`, which returns OK as soon as the manager process is up — *before* the webhook listener on port 9443 is bound.
  The new pod is briefly added to the `gmc-webhook-service` endpoints in a not-yet-serving state.
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

**Symptoms.** Worker pods (or `kubectl exec` debug curls from a workload-labeled pod) intermittently fail with `connect: connection refused` against the per-tenant proxy `:8080` immediately after a proxy `Deployment` rollout, scale-up, or HPA event.
The proxy pods report `READY 1/1` and `/healthz` returns 200.

**Likely causes.**
- Running a proxy image built before the proxy `/readyz` gate landed.
  The pre-fix proxy bound the health server on `:8081` in parallel with the CONNECT server on `:8080`.
  The kubelet observed `/healthz` returning 200 and added the pod IP to the proxy `Service` EndpointSlice before the CONNECT serve goroutine had bound the kernel socket.
  Worker pods racing the rollout connected to the new pod IP via `Service` DNS and got `ECONNREFUSED`.
- A custom probe override that points the GMC-managed proxy `Deployment`'s readinessProbe at `/healthz` instead of `/readyz` (e.g. an out-of-band `kubectl edit deploy`).
  The GMC reconciler overwrites the probe back to `/readyz` on the next reconcile, but until then the regression is live.

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
- Upgrade the proxy image to one built with the `/readyz` gate.
  The handler returns 503 until both listeners are bound (`cmd/proxy/proxy.go` — `handleReadyz`).
- If a custom override changed the readinessProbe path back to `/healthz`, remove it.
  GMC re-applies the canonical `Deployment` on its next reconcile, so the regression window closes within a few seconds.

`/healthz` remains the liveness probe (always 200 if the process is up). `/readyz` is the readiness gate — kubelet keeps the pod out of the Service EndpointSlice until both `:8080` and `:8081` are bound.

---

## Prometheus Not Scraping Proxy or AGC Metrics

**Symptom.** The proxy and AGC `/metrics` endpoints (served over mutual TLS on `:8443`) return no data in Prometheus, or scrape targets show as `down` with a connection timeout/refused — despite the pods being healthy.

**Cause.** Each tenant namespace runs under a default-deny ingress posture.
The GMC's per-tenant NetworkPolicies admit `:8443` ingress *only* from namespaces labelled `metrics: enabled` (the same convention the GMC's own `allow-metrics-traffic` NetworkPolicy uses).
If the namespace your Prometheus runs in is not labelled, its scrapes are dropped before the TLS handshake.
Kubelet liveness/readiness probes are unaffected — they hit `:8081` (`/healthz` + `/readyz`) from the node, which every supported CNI exempts from NetworkPolicy enforcement.

```bash
# 1. Confirm the monitoring namespace carries the scrape label.
kubectl get ns <prometheus-namespace> -o jsonpath='{.metadata.labels.metrics}{"\n"}'
# Expected: enabled

# 2. Inspect the per-tenant NP ingress rules — each should list an 8443 rule
#    whose `from` is a namespaceSelector on metrics=enabled.
kubectl get networkpolicy -n <tenant-namespace> \
  actions-gateway-proxy actions-gateway-controller \
  -o jsonpath='{range .items[*]}{.metadata.name}: {.spec.ingress}{"\n"}{end}'

# 3. Distinguish an NP drop from a missing client cert: scrape from the
#    monitoring namespace without a cert (-k skips server-cert verification).
#    A `tls: certificate required` error means the NP admits you — fix the cert
#    per the TLS runbook below. A timeout means the label/NP still drops you.
kubectl run dbg-scrape --rm -i --restart=Never --quiet \
  -n <prometheus-namespace> --image=curlimages/curl --command -- \
  curl -sS --max-time 5 -k https://actions-gateway-proxy.<tenant-namespace>.svc:8443/metrics
```

**Resolution.**
- Label the namespace your Prometheus runs in: `kubectl label ns <prometheus-namespace> metrics=enabled`.
- The proxy and AGC `/metrics` endpoints are served over **mutual TLS on `:8443`** (Q69), so the scraper also needs the per-tenant client certificate — see [Metrics scrape returns a TLS / connection error](#metrics-scrape-returns-a-tls--connection-error) for fetching the bundle.
  The `metrics: enabled` label gates *ingress*; the client cert *authenticates* the scraper.
  Keep the label off namespaces that should not see per-tenant traffic-volume metrics.

**Same label gates the GMC manager metrics.** Since the §E manifest-defaults work, the GMC install ships the manager NetworkPolicy enabled by default, which flips the controller-manager pod to default-deny ingress and admits `:8443` `/metrics` only from `metrics: enabled` namespaces.
If the GMC manager scrape target is `down`, apply the same label to the Prometheus namespace: `kubectl label ns <prometheus-namespace> metrics=enabled`.
The validating-webhook port (`9443`) is re-allowed from any source by design (the apiserver caller is not a labeled pod), so admission is unaffected by this label.

---

## Proxy Replica Stuck Pending After Enabling HA Defaults

**Symptom.** One of the two proxy replicas is `Pending`; `kubectl describe pod` shows `didn't match pod anti-affinity rules` / `node(s) didn't match pod anti-affinity`.
The proxy Deployment never reaches full availability.

**Cause.** The proxy pool uses **required** pod anti-affinity on `kubernetes.io/hostname` so replicas land on distinct nodes (a single node failure must never drop the whole tenant's egress pool, and co-located replicas defeat the PodDisruptionBudget).
With the default `proxy.minReplicas: 2`, the scheduler needs **at least two schedulable nodes**.
On a single-node dev/kind cluster (e.g. `test/kind-config-1worker.yaml`, where the control-plane is tainted and only one worker is schedulable) the second replica can never place.

**Resolution.**
- Production: ensure the cluster has at least `proxy.minReplicas` schedulable nodes for the proxy pods (the default kind config ships two workers).
- Single-node dev clusters: set `spec.proxy.minReplicas: 1` on the `ActionsGateway` to run a single proxy replica.

---

## Proxy Pool Never Scales Out

**Symptom.** Under load the proxy pool sits at `minReplicas` no matter what the HorizontalPodAutoscaler (HPA) wants. `kubectl get hpa -n <tenant-namespace>` shows a `TARGETS` percentage well above the target and `REPLICAS` climbing, while `kubectl get deploy` keeps reporting `minReplicas`.
Scaling by hand (`kubectl scale deploy <proxy> --replicas=5`) reverts within a second.

**Cause.** Fixed in the release that carries this page.
Before it, the GMC's proxy reconciler rewrote the proxy Deployment's `.spec.replicas` on every pass and re-triggered itself on the HPA's own scale write, so the pool oscillated between `minReplicas` and the HPA's target and could never stay scaled out.
Every tenant's egress capacity was capped at `proxy.minReplicas`.

**Resolution.** Upgrade the GMC.
On a fixed GMC, `.spec.replicas` belongs to the HPA; the reconciler only owns the *floor*:

| Situation | Who sets `.spec.replicas` |
|---|---|
| Deployment first created | Reconciler, to `minReplicas` |
| Deployment sitting at `0` | Reconciler, back to `minReplicas` (an HPA will not scale a target off zero) |
| Anything else | The HPA, bounded by its own `minReplicas`/`maxReplicas` |

Two consequences worth knowing:

- **Raising `spec.proxy.minReplicas` no longer bumps the Deployment directly.** The GMC raises the HPA's `minReplicas`, and the HPA scales the pool.
  That needs a working metrics pipeline — check `ScalingActive=True`:

  ```sh
  kubectl get hpa <proxy> -n <tenant-namespace> \
    -o jsonpath='{.status.conditions[?(@.type=="ScalingActive")].status}{"\n"}'
  ```

  `False` with reason `FailedGetResourceMetric` means metrics-server is missing or unhealthy; install it, or the floor will not be enforced.
- **Lowering `spec.proxy.minReplicas` no longer shrinks the pool immediately.** The HPA's downscale stabilisation window (5 minutes by default) applies.

An out-of-band `kubectl scale` on the proxy Deployment is likewise no longer reverted — but the HPA will pull the count back toward its own bounds on its next sync, so it is not a durable way to resize a pool.
Change `spec.proxy.minReplicas` / `spec.proxy.maxReplicas` on the `ActionsGateway` (v1) or `spec.minReplicas` / `spec.maxReplicas` on the `EgressProxy` (v2) instead.

---

← [Back to Operations](README.md)
