# Troubleshooting Guide

> **Audience:** Platform engineer, Tenant operator

Each section below covers a specific failure mode: symptoms, likely cause, diagnostic commands, and resolution steps.

**Addressing the AGC.** Every AGC control-plane object is named per gateway, as `<gateway>-agc`: the Deployment, the ServiceAccount, the Service, the RoleBinding and the AGC NetworkPolicy.
A `team-a-gateway` tenant therefore addresses `deploy/team-a-gateway-agc`, `sa/team-a-gateway-agc`, and so on.
Commands below use `<gateway>`; substitute your own gateway name.

**Addressing the proxy pool.** Each `EgressProxy` names its Deployment, Service, HorizontalPodAutoscaler and NetworkPolicy `<proxy>-proxy` and its TLS Secret `<proxy>-proxy-tls`, where `<proxy>` is the `EgressProxy`'s name.
Its pods carry the label `actions-gateway.com/egress-proxy=<proxy>`.

**Selecting AGC pods.** The bare `app` label carries the per-gateway name, so selecting on it by the fixed name finds nothing.
The recommended `app.kubernetes.io/name` label is `actions-gateway-controller` on every gateway, so `-l app.kubernetes.io/name=actions-gateway-controller` selects them all and is what the commands below use.

---

## Table of Contents

- [How to Validate a Fresh Deployment](#how-to-validate-a-fresh-deployment)
- [Helm Render Fails: gmc.image Must Be Pinned by Digest](#helm-render-fails-gmcimage-must-be-pinned-by-digest)
- [GMC Pods Rejected: insufficient quota to match these scopes (PriorityClass)](#gmc-pods-rejected-insufficient-quota-to-match-these-scopes-priorityclass)
- [helm upgrade Fails With nil pointer evaluating interface {}.FIELD](#helm-upgrade-fails-with-nil-pointer-evaluating-interface-field)
- [Every RunnerSet / RunnerTemplate Write Denied: no params found for policy binding](#every-runnerset--runnertemplate-write-denied-no-params-found-for-policy-binding)
- [GMC Exits at Startup: an Installed CRD Schema Is Older Than the GMC](#gmc-exits-at-startup-an-installed-crd-schema-is-older-than-the-gmc)
- [GMC Not Provisioning Tenant Resources](#gmc-not-provisioning-tenant-resources)
- [`kubectl rollout restart` of a Managed Deployment Reports Success but Nothing Restarts](#kubectl-rollout-restart-of-a-managed-deployment-reports-success-but-nothing-restarts)
- [ActionsGateway Reports RunnerSetsDegraded](#actionsgateway-reports-runnersetsdegraded)
- [RunnerSet Reports WorkersUnschedulable](#runnerset-reports-workersunschedulable)
- [RunnerSet Reports WorkerCapacityDeclined (the Gateway Stopped Claiming Jobs)](#runnerset-reports-workercapacitydeclined-the-gateway-stopped-claiming-jobs)
- [Worker / Proxy / AGC Pods Rejected by a Cluster Policy Engine](#worker--proxy--agc-pods-rejected-by-a-cluster-policy-engine)
- [ActionsGateway Reports EgressRulesStale](#actionsgateway-reports-egressrulesstale)
- [Proxy Pools Have No GitHub Egress After a GMC Start](#proxy-pools-have-no-github-egress-after-a-gmc-start)
- [A GHES Tenant's Traffic Never Reaches the Appliance](#a-ghes-tenants-traffic-never-reaches-the-appliance)
- [A GHES Appliance's Certificate Is Not Trusted](#a-ghes-appliances-certificate-is-not-trusted)
- [Tenant Namespace Missing the Managed-Tenant Marker Label](#tenant-namespace-missing-the-managed-tenant-marker-label)
- [ActionsGateway Stuck Deleting (Teardown Blocked on a Failing Delete)](#actionsgateway-stuck-deleting-teardown-blocked-on-a-failing-delete)
- [Tenant Namespace Stuck Terminating on agentpool-cleanup Finalizers](#tenant-namespace-stuck-terminating-on-agentpool-cleanup-finalizers)
- [Tenant Namespace Stuck Terminating After Narrowing the PriorityClass Allowlist](#tenant-namespace-stuck-terminating-after-narrowing-the-priorityclass-allowlist)
- [Self-Serviced PriorityClasses Stopped Being Accepted All At Once](#self-serviced-priorityclasses-stopped-being-accepted-all-at-once)
- [EgressProxy Rejected: CiliumFQDN / CalicoFQDN Are Not egressPolicyMode Values](#egressproxy-rejected-ciliumfqdn--calicofqdn-are-not-egresspolicymode-values)
- [AGC CrashLoopBackOff or Not Acquiring Jobs](#agc-crashloopbackoff-or-not-acquiring-jobs)
- [ScaleSet RunnerSet Stuck Not Ready: `ScaleSetListenerStartFailed` Naming the Guard ConfigMap](#scaleset-runnerset-stuck-not-ready-scalesetlistenerstartfailed-naming-the-guard-configmap)
- [AGC Exits at Startup: GATEWAY_NAME Set but the v2 RunnerSet CRD Is Missing](#agc-exits-at-startup-gateway_name-set-but-the-v2-runnerset-crd-is-missing)
- [AGC Exits at Startup: Proxy CA Cert Present but Unreadable](#agc-exits-at-startup-proxy-ca-cert-present-but-unreadable)
- [Long Polls Time Out on a Black-Holed Message-Queue Connection](#long-polls-time-out-on-a-black-holed-message-queue-connection)
- [Reconcile or Token Mint Hangs on a Slow GitHub Endpoint](#reconcile-or-token-mint-hangs-on-a-slow-github-endpoint)
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
- [Scale-Set Jobs Waiting at the Worker Ceiling (WorkerCeilingReached)](#scale-set-jobs-waiting-at-the-worker-ceiling-workerceilingreached)
- [Scale-Set Job Waiting for Its Previous Worker to Terminate (WorkerTerminating)](#scale-set-job-waiting-for-its-previous-worker-to-terminate-workerterminating)
- [Scale-Set Job Held Because Its Run Could Not Be Read (JobSourceUnverified)](#scale-set-job-held-because-its-run-could-not-be-read-jobsourceunverified)
- [Fork Pull Request Job Refused (ForkJobRefused)](#fork-pull-request-job-refused-forkjobrefused)
- [Scale-Set Assignments Abandoned (AssignmentAbandoned)](#scale-set-assignments-abandoned-assignmentabandoned)
- [Worker Pods Stuck Running After the Job Finished (Mesh Sidecar)](#worker-pods-stuck-running-after-the-job-finished-mesh-sidecar)
- [RunnerSet Reports PossibleReapBlockingSidecar (Build/DinD Sidecar in the Template)](#runnerset-reports-possiblereapblockingsidecar-builddind-sidecar-in-the-template)
- [Worker Image Runner Version](#worker-image-runner-version)
- [Job-Lifecycle Events on a RunnerSet](#job-lifecycle-events-on-a-runnerset)
- [Proxy Pool Not Scaling](#proxy-pool-not-scaling)
- [Proxy Tunnel Closed Mid-Stream — Idle or Lifetime Cap](#proxy-tunnel-closed-mid-stream--idle-or-lifetime-cap)
- [Proxy Tunnel Cut During a Rollout](#proxy-tunnel-cut-during-a-rollout)
- [Metrics scrape returns a TLS / connection error](#metrics-scrape-returns-a-tls--connection-error)
- [Jobs Targeting One of a Runner Set's Labels Never Start (RunnerLabelsIncomplete)](#jobs-targeting-one-of-a-runner-sets-labels-never-start-runnerlabelsincomplete)
- [RateLimited Condition on a RunnerSet](#ratelimited-condition-on-a-runnerset)
- [GitHub App Secret Misconfiguration](#github-app-secret-misconfiguration)
- [Token Refresh Errors Spiking](#token-refresh-errors-spiking)
- [Network Connectivity Failures](#network-connectivity-failures)
- [AGC Crash-Loops Dialling the API Server Through the Egress Proxy](#agc-crash-loops-dialling-the-api-server-through-the-egress-proxy)
- [AGC Cannot Reach the Kubernetes API Server (NetworkPolicy + post-DNAT port mismatch)](#agc-cannot-reach-the-kubernetes-api-server-networkpolicy--post-dnat-port-mismatch)
- [DNS Times Out Under the Egress NetworkPolicy (GKE Dataplane V2 / NodeLocal DNSCache)](#dns-times-out-under-the-egress-networkpolicy-gke-dataplane-v2--nodelocal-dnscache)
- [Worker Pod Runner.Worker Fails TLS Handshake With UntrustedRoot](#worker-pod-runnerworker-fails-tls-handshake-with-untrustedroot)
- [Which Disruptions Auto-Re-Run a Job (and Which Never Do)](#which-disruptions-auto-re-run-a-job-and-which-never-do)
- [Flaky Jobs and Platform Faults on Failed Jobs](#flaky-jobs-and-platform-faults-on-failed-jobs)
- [Evicted Worker Pods Exhausting Retry Budget](#evicted-worker-pods-exhausting-retry-budget)
- [Draining a Worker Auto-Re-Runs the Jobs It Interrupts](#draining-a-worker-auto-re-runs-the-jobs-it-interrupts)
- [A Preempted Worker's Job Is Not Re-Run](#a-preempted-workers-job-is-not-re-run)
- [Cancelling a Run Does Not Stop Its Worker Pod](#cancelling-a-run-does-not-stop-its-worker-pod)
- [Evicted Scale-Set Jobs Are Not Re-Run Automatically](#evicted-scale-set-jobs-are-not-re-run-automatically)
- [Terminated Worker Pod Never Reports Its Job (Job Hangs on GitHub Until the Lock Lapses)](#terminated-worker-pod-never-reports-its-job-job-hangs-on-github-until-the-lock-lapses)
- [Jobs Failing Due to Namespace ResourceQuota Exhaustion](#jobs-failing-due-to-namespace-resourcequota-exhaustion)
- [Why Is My RunnerSet Not Being Offered Jobs?](#why-is-my-runnerset-not-being-offered-jobs)
- [Worker Pod Fails to Start After Secure-by-Default SecurityContext](#worker-pod-fails-to-start-after-secure-by-default-securitycontext)
- [Security Profile Downgrade Rejected by Admission](#security-profile-downgrade-rejected-by-admission)
- [`noProxyCIDRs` Rejected: Entry Would Bypass the Proxy for GitHub](#noproxycidrs-rejected-entry-would-bypass-the-proxy-for-github)
- [Privileged Worker Container Rejected by Admission](#privileged-worker-container-rejected-by-admission)
- [`RunnerTemplate` Rejected: Reserved Pod Field](#runnertemplate-rejected-reserved-pod-field)
- [`RunnerTemplate` Rejected: Pod Shape Past Baseline, Volume Type, or ServiceAccount Token](#runnertemplate-rejected-pod-shape-past-baseline-volume-type-or-serviceaccount-token)
- [`RunnerSet` Rejected: Duplicate First `runnerLabel`](#runnerset-rejected-duplicate-first-runnerlabel)
- [`ActionsGateway` Reports `ScaleSetNameCollision`](#actionsgateway-reports-scalesetnamecollision)
- [`RunnerSet` Rejected: `nodeShare.allocatable` Declares Neither cpu Nor memory](#runnerset-rejected-nodeshareallocatable-declares-neither-cpu-nor-memory)
- [`RunnerSet` Stuck `Ready=False` With a `NotFound` Reason](#runnerset-stuck-readyfalse-with-a-notfound-reason)
- [`RunnerSet` Stuck `Ready=False` With `RunnerGroupNotFound`](#runnerset-stuck-readyfalse-with-runnergroupnotfound)
- [v2 `ActionsGateway` Stuck `Ready=False` (`CredentialUnavailable` / `ProxyNotFound`)](#v2-actionsgateway-stuck-readyfalse-credentialunavailable--proxynotfound)
- [RunnerSet or gateway reports EgressUnattributed (direct egress)](#runnerset-or-gateway-reports-egressunattributed-direct-egress)
- [`AGCAutoscalingUnavailable` — the VPA CRDs are not installed](#agcautoscalingunavailable--the-vpa-crds-are-not-installed)
- [Multiple v2 gateways in one namespace: naming, scoping, prerequisites](#multiple-v2-gateways-in-one-namespace-naming-scoping-prerequisites)
- [v2 Objects Not Reconciling After Installing the CRD Chart](#v2-objects-not-reconciling-after-installing-the-crd-chart)
- [Privileged Security Profile Rejected: Namespace Not Eligible](#privileged-security-profile-rejected-namespace-not-eligible)
- [Tracing Sampler Rejected by Admission](#tracing-sampler-rejected-by-admission)
- [Tracing Endpoint Rejected by Admission](#tracing-endpoint-rejected-by-admission)
- [Tracing Rejected by Admission: insecure With an https Endpoint](#tracing-rejected-by-admission-insecure-with-an-https-endpoint)
- [Egress Peer cidr Rejected by Admission](#egress-peer-cidr-rejected-by-admission)
- [ActionsGateway Rejected: Missing or Malformed `githubURL`](#actionsgateway-rejected-missing-or-malformed-githuburl)
- [ActionsGateway Rejected: `githubURL` Spells Public GitHub by an Alias](#actionsgateway-rejected-githuburl-spells-public-github-by-an-alias)
- [Worker-Pod Lifecycle Duration Rejected by Admission](#worker-pod-lifecycle-duration-rejected-by-admission)
- [Priority Tiers Rejected by Admission: Thresholds Not Ascending](#priority-tiers-rejected-by-admission-thresholds-not-ascending)
- [kubectl apply ActionsGateway Times Out On Webhook During GMC Rollout](#kubectl-apply-actionsgateway-times-out-on-webhook-during-gmc-rollout)
- [Applying Any v2 Object Fails With no endpoints available for service "webhook-service"](#applying-any-v2-object-fails-with-no-endpoints-available-for-service-webhook-service)
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
kubectl get deploy -n <namespace> <gateway>-agc
kubectl logs -n <namespace> deploy/<gateway>-agc --tail=50

# 3. Confirm the proxy pool is healthy
kubectl get deploy -n <namespace> <proxy>-proxy
kubectl get hpa -n <namespace>

# 4. Confirm RunnerSet resources exist
kubectl get runnerset -n <namespace>

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
- Each RunnerSet's listener holds its session (`actions_gateway_scaleset_session_open == 1`).

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

## Every `RunnerSet` / `RunnerTemplate` Write Denied: `no params found for policy binding`

**Symptoms.** Every write to a `runnersets` or `runnertemplates` object is rejected — including ones that name no `priorityClassName` at all — while the parameter object plainly exists at the name the binding references:

```
kubectl apply -f runnerset.yaml
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

- `{"apiVersion":"actions-gateway.com/v2","kind":"PriorityClassAllowlist"}` — current.
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
After any event that could have tripped this, confirm the guard sees your current allowlist rather than assuming it does: change the allowlist and check that a `RunnerSet` naming a newly removed class is actually rejected.

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

  Restart the container itself (`crictl stop` on the apiserver container, then confirm its `createdAt` changed).
  `kubectl delete pod -n kube-system kube-apiserver-…` does **not** work — it recreates the static pod's mirror object while the container keeps running.

---

## GMC Exits at Startup: an Installed CRD Schema Is Older Than the GMC

**Symptoms.** After an upgrade the GMC never becomes Ready, and its logs end at:

```
Failed to register controllers ... verify installed CRD schemas: the CustomResourceDefinitions
installed in this cluster are older than this GMC and no longer declare a field that bounds
tenant access:
  - runnersets.actions-gateway.com at v2 does not declare spec.runnerGroup
```

**Cause.** The v2 CRDs are applied out-of-band, because they exceed Helm's 1 MiB release-Secret limit, so `helm upgrade` of the main chart never touches them.
Skipping the apply step therefore leaves the cluster serving the *previous* release's schema, and a structural schema **prunes an undeclared field on write, with no error and no warning**.
The field stays settable in a manifest and does nothing in the cluster.

For [`spec.runnerGroup`](../design/05-security.md#unbounded-job-intake-via-the-installations-default-runner-group) that is a security control failing open, not a feature going missing: with the field pruned to empty, the tenant's scale set registers into GitHub's installation-default runner group, which every repository in the organisation can route jobs into.
The GMC refuses to start rather than keep provisioning tenants against a boundary that is not there.
Gateways already running are unaffected, since each tenant's AGC is a separate Deployment, so the blast radius is provisioning rather than job flow.

**Resolution.** Re-apply the v2 CRDs at the release this GMC ships in, then let the GMC restart:

```bash
kubectl apply --server-side -f \
  https://github.com/actions-gateway/github-actions-gateway/releases/download/vX.Y.Z/actions-gateway-crds-v2.yaml
```

`helm template … | kubectl apply --server-side` is equivalent when the GMC runs outside `gmc-system`; both are covered in [install.md § the v2 API CRDs](install.md#the-v2-api-crds).
Confirm the schema landed before waiting on the rollout:

```bash
kubectl get crd runnersets.actions-gateway.com \
  -o jsonpath='{.spec.versions[?(@.name=="v2")].schema.openAPIV3Schema.properties.spec.properties.runnerGroup}'
```

Then **re-apply any `RunnerSet` that declared `runnerGroup` while the schema was stale**: the stored object had the field pruned out of it, and re-applying the CRD does not put it back.

**After rolling back from `v2.0.0` to 1.x** the log lists `v2alpha1` and `v2beta1`, and "older" is backwards: the CRDs are newer than the GMC.
The apply above is refused with a conflict on `.spec.conversion.strategy` and changes nothing, so use the forced form in [GMC rollback](upgrade.md#gmc-rollback).

**A `v1.10.0` GMC that logs `failed to determine if *v2beta1.PriorityClassAllowlist is namespaced`** is running with the `v2.0.0` chart-root CRD applied, which at that upgrade [goes after `helm upgrade`, with the recovery there](upgrade.md#action-required-at-v200-apply-the-crds-with-the-upgrade-because-the-agcs-watch-depends-on-them).

---

## GMC Not Provisioning Tenant Resources

**Symptoms.** An `ActionsGateway` CR was applied but nothing has been created in the tenant namespace: no AGC Deployment and no proxy Deployment.

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

**Which Deployments.** Every Deployment the GMC provisions inside a tenant namespace: the AGC control plane (`<gateway>-agc`) and the egress proxy pool.
The GMC's *own* Deployment (`gmc-controller-manager` in `gmc-system`) is not managed by a controller and restarts normally.

**What happened.** `kubectl rollout restart` works by patching a `kubectl.kubernetes.io/restartedAt` annotation onto the Deployment's **pod template** — that changed template is what makes the Deployment controller roll a new ReplicaSet.
On GMC versions without the Q552 fix, the reconciler rebuilt the whole pod template from the `ActionsGateway` / `EgressProxy` spec on every pass, so it reverted the annotation before the rollout began.
`kubectl rollout status` then reported the *pre-existing* ReplicaSet — trivially complete — as a successful rollout.

**Resolution.**
- **Upgrade the GMC** to a release carrying the Q552 fix.
  Fixed versions carry an operator-injected `kubectl.kubernetes.io/restartedAt` through reconcile, so `kubectl rollout restart` performs a real rolling update.
  Nothing else on the pod template is tolerated — a hand-edited image, env var, or any other annotation is still reconciled back to the CR, which remains the source of truth.
- **Workaround on an older GMC:** delete the pod and let the Deployment recreate it.

  ```sh
  kubectl delete pod -n <namespace> -l app.kubernetes.io/name=actions-gateway-controller
  ```

  This is a hard restart, not a rolling one: the replacement pod is only scheduled after the old one terminates, so the tenant's control plane is down for the pod's startup time.
  The AGC's 60s termination grace period still applies, so in-flight session work drains as it would on a rollout.

**What a working restart still does not do.** Either path replaces the control-plane pod and nothing else.
It deletes no worker pod — see [Workers Left Behind by an AGC That Was Down](#workers-left-behind-by-an-agc-that-was-down) for what actually reclaims those — so a restart is not a remedy for workers that will not drain.
The reaper runs on the live AGC on deadlines measured from each pod, so a fresh one reaps no sooner than the one it replaced.

**Verify either path took effect** — check that the pod is actually new rather than trusting the rollout message:

```sh
kubectl get pods -n <namespace> -l app.kubernetes.io/name=actions-gateway-controller \
  -o custom-columns=NAME:.metadata.name,AGE:.metadata.creationTimestamp
```

---

## ActionsGateway Reports RunnerSetsDegraded

**Symptoms.** `kubectl get actionsgateway` shows a `RunnerSetsDegraded=True` condition, or the `actions_gateway_runnersets_degraded` gauge is `1`.
The gateway infrastructure itself (proxy, AGC) may still be `Ready=True` — this condition rolls **child `RunnerSet`** health up to the gateway (Q304) so you don't have to inspect each set individually.

**Cause.** One or more of the `RunnerSet`s bound to the gateway is impaired, on either of two axes.
It is not `Ready` for a non-transient reason — a reference or provisioning failure, which a `RunnerSet` folds into `Ready=False` (anything but the benign startup `NoActiveSessions`) — **or** any of its abnormal-is-True impairing conditions is `True`: `Degraded` (revoked or invalid credentials), `RunnerVersionTooOld` (the worker image ships a runner below GitHub's enforced minimum; see [Worker Image Runner Version](#worker-image-runner-version)), or `WorkersUnschedulable` (worker pods can't be scheduled).
The second axis matters because `Degraded` and `RunnerVersionTooOld` move independently of `Ready`: a set whose session creation is rejected as unauthorized reports the benign `Ready=False/NoActiveSessions` while `Degraded=True` sits on its own condition.
Advisory throughput signals (`RateLimited`, the `WorkerQuota` ladder) are deliberately excluded so the rollup does not flap on normal load.
A set targeting a *different* gateway is never counted.
`RunnerSetsDegraded` does **not** gate `Ready`: the gateway can keep serving healthy sets while one is impaired.

**Diagnostics.**

```sh
# Read the rollup — its message names the impaired sets and their tripped signals.
kubectl get actionsgateway -n <namespace> <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="RunnerSetsDegraded")]}{.status} {.reason}: {.message}{"\n"}{end}'

# Drill into a named RunnerSet's own conditions.
kubectl get runnerset -n <namespace> <runner-set> -o jsonpath='{.status.conditions}' | jq .
```

**Resolution.** Resolve the underlying per-set condition, then the rollup clears automatically on the next reconcile (the GMC watches bound `RunnerSet`s):
- `Degraded` → see [GitHub App Secret Misconfiguration](#github-app-secret-misconfiguration).
- `RunnerVersionTooOld` → see [Worker Image Runner Version](#worker-image-runner-version).
- `WorkersUnschedulable` → see [RunnerSet Reports WorkersUnschedulable](#runnerset-reports-workersunschedulable).

---

## RunnerSet Reports WorkersUnschedulable

**Symptoms.** `kubectl get runnerset` shows a `WorkersUnschedulable=True` condition, or the `actions_gateway_runnerset_workers_unschedulable` gauge is `1`.
Jobs are acquired but never start; worker pods sit `Pending`.
Each pod the reaper eventually gives up on also emits a `WorkersUnschedulable` Warning event and a `WorkerPodStuckPending` event on the RunnerSet.

**Cause.** The Kubernetes scheduler cannot place the set's worker pods on any node — `PodScheduled=False` with reason `Unschedulable`.
Typical reasons: no node has enough allocatable CPU/memory for the pod's requests, the pod's `nodeSelector` / affinity matches no node, or every candidate node carries a taint the pod does not tolerate.
The condition trips once a pod has been Pending+Unschedulable for longer than the scheduling grace (half the set's `pendingPodDeadline`), giving an early warning before the reaper deletes the pod at the full deadline.

This is **not** a quota problem: a `ResourceQuota` rejection blocks pod *admission* so the pod is never created — that path is the separate `WorkerQuotaExceeded` condition.
The two never both fire for the same cause.

**Diagnostics.**

```sh
# Read the condition — its message names the stuck pods and the scheduler verdict.
kubectl get runnerset -n <namespace> <runner-set> \
  -o jsonpath='{range .status.conditions[?(@.type=="WorkersUnschedulable")]}{.status} {.reason}: {.message}{"\n"}{end}'

# Inspect a stuck worker pod's scheduler events for the exact reason.
kubectl describe pod -n <namespace> <worker-pod>   # look for "FailedScheduling"
```

**Resolution.** Match the scheduler verdict to the fix:
- *Insufficient cpu/memory* → add nodes / scale the cluster autoscaler, or lower the worker pod's resource requests in the set's `podTemplate`.
- *node(s) didn't match nodeSelector / affinity* → correct the `podTemplate`'s `nodeSelector`/affinity, or label the intended nodes.
- *node(s) had untolerated taint* → add the matching toleration to the `podTemplate`, or remove the taint from the target nodes.

The condition clears automatically on the next reconcile once a worker pod schedules successfully.

---

## RunnerSet Reports WorkerCapacityDeclined (the Gateway Stopped Claiming Jobs)

**Symptoms.** `kubectl get runnerset` shows a `WorkerCapacityDeclined=True` condition and a `WorkerCapacityDeclined` Warning event.
Jobs sit **queued at GitHub** and are never picked up — no worker pods are being created, and no jobs are being cancelled either.
`actions_gateway_scaleset_advertised_capacity` has dropped and `actions_gateway_scaleset_capacity_withheld{reason="capacity"}` holds the remainder of the ceiling.
Those two count the *cost* of the refusal; the gate's own state is `actions_gateway_runnerset_worker_capacity_declined`, which reads `1` under the `reason` label below and is emitted only for a set that opted in — so `max by (namespace, runner_set, reason) (actions_gateway_runnerset_worker_capacity_declined == 1)` is the fleet-wide "which sets are gated, and on what evidence" query.

**Cause.** This is **deliberate, and only ever happens on a set that opted in.** The runner set has `spec.capacityGate.mode: Observe` (Q405, Q406, Q470), and the signal the gate reads is currently saying the cluster cannot place another worker pod of this set's shape.
The gateway advertises less capacity rather than take work it cannot run, because each such assignment spends a single-use JIT runner record, leaves a worker pod `Pending` until `pendingPodDeadline`, and ends in a **cancelled** workflow run.

Which of the two **placeability** signals it read follows from the gateway's `spec.clusterCapacity.nodeAutoscaling`, not from anything on the set.
The third reason below is not selected by anything; it is read on every cluster.
The condition's `reason` names which one decided:

| `reason` | Gateway says | What was observed |
|---|---|---|
| `ScaleUpDeclined` | `nodeAutoscaling: Present` (default) | The cluster autoscaler itself recorded, on a stuck worker pod, that it will **not** add a node for it. The message carries the autoscaler's own per-node-group text. |
| `PodsUnschedulable` | `nodeAutoscaling: Absent` | The scheduler's own verdict — the same `PodScheduled=False`/`Unschedulable` fact behind [`WorkersUnschedulable`](#runnerset-reports-workersunschedulable). |
| `PodsNotStarting` | either | A worker pod **was** placed on a node and then could not be started: the kubelet is in `ImagePullBackOff` on its image (Q714). The message carries the kubelet's own text, which names the image. Almost always an unpullable or misspelled `workerImage`; see [below](#the-reason-is-podsnotstarting-the-image-will-not-pull). |
| `AwaitingProbe` | either | The stuck pods that produced the verdict were reaped at `pendingPodDeadline`, and nothing has yet shown that capacity returned, so the decline is **retained** (Q512). Intake is limited to **one probe job** per deadline window, not closed; the message carries the reaped verdict. Clears when a worker pod **starts**: a container that has actually run, not merely a pod that has been scheduled. |
| `CapacityAvailable` (status `False`) | either | The gate is engaged and is **not** refusing intake. |
| `GateModeUnsupported` (status `False`) | — | This AGC does not implement the mode the set selected, so no rung is evaluated. See [below](#the-mode-is-reported-as-unsupported). |

A set with no `capacityGate` (the default) never carries this condition at all — its absence is not a failure to report, it means the set did not opt in.

> **The gate throttles, it does not seal.** When the reaper deletes the stuck pod at `pendingPodDeadline`, the condition does not clear — it latches as `AwaitingProbe` and admits exactly **one probe job**; if capacity is still missing, that job's pod trips the live verdict again, and once one of its containers runs the gate clears completely.
> Starting rather than merely being scheduled is the release condition, because a probe pod binds within a second of being created and only reveals that it cannot start seconds later; releasing on the binding would restore the full advertisement inside that gap (Q714).
> Expect roughly one claim per deadline window, not zero — a `RunnerSet` that never claims anything again has a different problem.
> On an idle gated set whose shape stays unplaceable, `AwaitingProbe` can persist `True` indefinitely; that is truthful, and harmless until jobs arrive.
>
> A latched set is invisible to the scheduler-side signals: the pod that produced the verdict is gone, so `WorkersUnschedulable` — and its gauge — is back to `0` while intake is still throttled.
> `worker_capacity_declined{reason="AwaitingProbe"}` is the series that stays `1`, which is why the gauge carries the reason.

**What alerts on this.** The ticket-severity rule [`ActionsGatewayScaleSetCapacityWithheld`](runbook.md#actionsgatewayscalesetcapacitywithheld) covers the gate.
It watches the *cost* series above rather than `worker_capacity_declined` itself, and requires the set to have been assigned work in the last hour.
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

**Resolution.** The gate is reporting a real cluster condition, so fix the placement problem — the resolutions are the same ones listed under [`WorkersUnschedulable`](#runnerset-reports-workersunschedulable) (allocatable capacity, `nodeSelector`/affinity, tolerations).
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

The next long-poll picks that up with no AGC restart, and the condition is retracted on the next reconcile.

> **Check this first if the reason is `PodsUnschedulable` and you did not expect the gate to be closed.** That reason means the gateway reports `clusterCapacity.nodeAutoscaling: Absent` — an assertion that nothing will add a node.
> It is sound **only** on a fixed-size cluster.
> If this cluster *does* run a cluster autoscaler or Karpenter, the assertion is wrong: the unschedulable pod was the request for a node, and gating on it can hold every set under this gateway back exactly when scale-up would have rescued them.
> Fix it **on the gateway**, not on the set:
>
> ```sh
> kubectl patch actionsgateway -n <namespace> <gateway> --type=merge \
>   -p '{"spec":{"clusterCapacity":{"nodeAutoscaling":"Present"}}}'
> ```

### The Reason Is `PodsNotStarting`: the Image Will Not Pull

This reason is not about placement at all, which is why it reads the same on a fixed-size cluster and an elastic one.
The worker pod **was** scheduled, it has a node, and then the kubelet could not start its container, so it sits `Pending` in `ImagePullBackOff` until the reaper deletes it at `pendingPodDeadline`.
No node anyone adds changes that, so the gate refuses intake wherever it sees it.

Since Q906 the `RunnerSet` reports it within seconds too, on **every** set rather than only a gated one, as the advisory `WorkersNotStarting` condition with this same `PodsNotStarting` reason:

```sh
kubectl get runnerset -n <namespace> <runner-set> \
  -o jsonpath='{.status.conditions[?(@.type=="WorkersNotStarting")].message}'
```

That condition reports and decides nothing; `WorkerCapacityDeclined` below is the intake decision, and only a set that opted into `spec.capacityGate` carries it.
The pod is still where the first-hand evidence is:

```sh
kubectl get pods -n <namespace> -l actions-gateway.com/runner-set=<runner-set> \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,STATE:.status.containerStatuses[*].state.waiting.reason'
```

```sh
kubectl describe pod -n <namespace> <worker-pod>   # the Failed event names the host or the manifest
```

Almost always one of:

| What you see | Fix |
|---|---|
| The image host is `example.invalid` | A placeholder the 1.4 and 1.5 DinD templates shipped, left in place. Those templates no longer set `spec.workerImage` at all, because the AGC's default runner image already carries a Docker CLI. Re-apply the [template](runner-template-library.md), or set a real `workerImage` if the job needs `docker compose` or a toolchain of its own. |
| `401`/`403` on the pull | The pull Secret is missing from this namespace, or is not referenced by the worker's ServiceAccount ([air-gapped install](air-gapped-install.md) step 5). |
| `manifest unknown`, or a tag that does not exist | A typo in `workerImage`, or a digest that did not survive relocation into a mirror. |
| The registry host does not resolve | Worker egress does not reach it; check the tenant's `EgressProxy` and any NetworkPolicy in the namespace. |

The gate reads the kubelet's **backoff**, not its first failed pull: a single `ErrImagePull` from a registry blip is ignored, and the next attempt clears it.
So a `PodsNotStarting` decline means the kubelet has already tried, failed, and scheduled a retry.

Once the image pulls, the next worker pod starts and both conditions clear on the following reconcile, with no restart and no change to the set.

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
The usual culprits: a policy requiring `drop: [ALL]` capabilities or `allowPrivilegeEscalation: false` on *all* pods (the default `baseline` worker profile sets neither — baseline CI relies on in-job `sudo`); a `readOnlyRootFilesystem` requirement (no worker profile sets it — the runner needs a writable root filesystem); a registry allowlist that omits GAG's registries; or a "require resource limits" rule.

This is distinct from [`WorkersUnschedulable`](#runnerset-reports-workersunschedulable) (scheduler can't place a *created* pod) and from `WorkerQuotaExceeded` (`ResourceQuota` blocks admission): here the policy engine blocks pod creation *before* either applies.

**Diagnostics.**

```sh
# Worker path: the owning RunnerSet surfaces the create error in its Events.
kubectl describe runnerset -n <namespace> <runner-set>
# Proxy / AGC path: the GMC logs the failed apply.
kubectl logs -n <gmc-install-namespace> deploy/<gmc-manager> | grep -i "denied\|forbidden\|policy"
# Confirm which policy fired.
kubectl get cpol,polr -A           # Kyverno ClusterPolicies + PolicyReports
kubectl get constraints            # Gatekeeper
```

**Resolution.** Reconcile the cluster policy with GAG's real pod posture — see the [admission-policies compatibility matrix](admission-policies.md), which states per policy class whether GAG complies or what to allow.
In short:
- Add GAG's registries to your allowlist (`ghcr.io/actions-gateway/*` for the control plane, `ghcr.io/actions/actions-runner` for the default worker).
- For `drop-ALL` / no-privilege-escalation requirements: have tenants set the namespace label `actions-gateway.com/security-profile=restricted` (which satisfies them), or apply the scoped [exception samples](examples/policies/README.md) so `baseline` workers pass.
- For `readOnlyRootFilesystem`: exempt worker pods (no profile can satisfy it).

---

## ActionsGateway Reports EgressRulesStale

**Symptoms.** `kubectl get actionsgateway` shows an `EgressRulesStale=True` condition, or the `actions_gateway_egress_rules_stale` gauge (`EgressProxy`) or `actions_gateway_direct_egress_rules_stale` gauge (direct-egress `ActionsGateway`) is `1`.
Optionally, jobs intermittently fail to reach newly-rotated GitHub endpoints.

**Cause.** The GMC refreshes each managed proxy `NetworkPolicy`'s egress allowlist from `api.github.com/meta` on a ~24h cycle, and on a v2 `ActionsGateway` with no `defaultProxyRef` the GitHub allowlist on its AGC and workload `NetworkPolicies` too.
If that refresh loop stalls (GitHub meta API unreachable, persistent fetch errors), the allowlist freezes.
GitHub periodically rotates its published IP ranges, so a frozen allowlist eventually drops egress to the new ranges silently.
The condition trips when the last successful refresh is older than the staleness window (just over two refresh cycles), so a single missed/slow refresh does not false-trip it.
It is advisory and does **not** gate `Ready` — existing egress keeps working until GitHub rotates.
It is only evaluated for gateways whose proxy `NetworkPolicy` is gateway-managed (`spec.managedNetworkPolicy` unset or `true`), on an `EgressProxy` only in `CIDR` mode, and on an `ActionsGateway` only in direct egress: a proxied one reads `False` / `RefreshPending`, and its `EgressProxy` reports the allowlist.

The reason says which of two states you are in:

| Reason | Meaning | Where to go |
|---|---|---|
| `RefreshStalled` | The ranges in force are older than the staleness window. Existing egress works until GitHub rotates, unless the message says the GMC serves no ranges: then its persisted copy has expired and GitHub egress is denied, except through a direct-egress policy the message names as keeping its ranges. | The diagnostics below. |
| `RefreshNeverSucceeded` | The GMC holds no ranges at all, from neither a fetch nor its persisted copy. On an `EgressProxy` the policy carries no GitHub rule, so GitHub egress is denied now. On a direct-egress `ActionsGateway` a policy may still carry the ranges it was last programmed with; the message says which (below). | [Proxy Pools Have No GitHub Egress After a GMC Start](#proxy-pools-have-no-github-egress-after-a-gmc-start). |

**A direct-egress v2 gateway can still be reaching GitHub when the GMC serves no ranges.** The GMC does not blank a direct-egress `NetworkPolicy` that already carries GitHub ranges, so after a restart with no usable persisted copy, or once that copy expires, those policies keep the ranges they were last programmed with.
The condition stays `True`, because nothing is refreshing them, and its message says which policies keep their ranges and which carry no GitHub rule.
Only a named policy denies GitHub egress now: a gateway provisioned while the GMC held no ranges, or a policy deleted and recreated in that window.

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

## Proxy Pools Have No GitHub Egress After a GMC Start

**Symptoms.** After the GMC starts or restarts, tenants acquire no jobs.
The proxy pool logs `upstream dial failed` with an `i/o timeout` to a GitHub address, and answers the AGC `502 upstream unavailable`.
The pool's `NetworkPolicy` has a DNS egress rule and no GitHub rule.
The `EgressProxy` reports `EgressRulesStale=True` with reason `RefreshNeverSucceeded`, and `Ready` stays `True`, because `Ready` measures proxy pods rather than their egress.
The `ActionsGatewayEgressRulesStale` alert fires once the state has held for 15 minutes.

**Cause.** A fresh GMC has no GitHub IP ranges to program until its first `api.github.com/meta` fetch succeeds, unless it can restore them from its own namespace.
The GMC writes each successful fetch to the `actions-gateway-gmc-ip-ranges` ConfigMap and seeds itself from that copy at startup, but refuses the copy when it is as old as the `EgressRulesStale` window (49 hours on the default 24-hour refresh), unreadable in any entry, or stamped in the future.
A blank line between entries counts as unreadable; a trailing newline does not.
So this symptom means one of three things: a first-ever start, a GMC that has not fetched successfully for over two days, or a copy that was refused.
The fetch is unauthenticated, and GitHub allows 60 calls an hour per source IP.
When several GMCs, or anything else, share one egress IP, that budget can be spent before the GMC's fetch, and GitHub answers `403` or `429` until the window resets.

**Diagnostics.**

```sh
# The condition names this state directly.
kubectl get egressproxy -n <namespace> <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="EgressRulesStale")]}{.status} {.reason}: {.message}{"\n"}{end}'

# Whether the start seeded from the persisted copy, and if not, why it was refused.
kubectl logs -n <gmc-namespace> deploy/<gmc> | grep -i "persisted copy"

# The persisted copy and the time of the fetch it came from.
kubectl get configmap -n <gmc-namespace> actions-gateway-gmc-ip-ranges -o jsonpath='{.data.fetchedAt}'

# A rate-limited fetch logs how long the GMC will wait before its next attempt.
kubectl logs -n <gmc-namespace> deploy/<gmc> | grep -i "ip-range fetch"

# Read the quota from a host that shares the GMC's egress IP. This spends one call.
curl -sSI https://api.github.com/meta | grep -i '^x-ratelimit'
```

**Resolution.** The GMC retries on its own, after the time GitHub names and never sooner than a minute: the `Retry-After` header, or `x-ratelimit-reset` when `x-ratelimit-remaining` is `0`.
With neither, it waits a minute and doubles the wait on each further limit, up to an hour.
A `403` that carries neither GitHub's rate-limit headers nor its rate-limit message, such as a forward proxy's deny page, is not treated as a limit: the GMC logs `GitHub meta returned 403` and retries on its ordinary 30 s backoff.
Restarting the GMC does not help: it restores no more than the refused copy and spends another call.
To recover sooner, find what else spends the unauthenticated quota from that egress IP.

---

## A GHES Tenant's Traffic Never Reaches the Appliance

**Symptoms.** A tenant whose `spec.githubURL` names a GitHub Enterprise Server (GHES) host acquires no jobs.
Depending on which of the two gaps you have hit:

- **Token exchange fails.** The AGC logs a `401` from `api.github.com` — a host you never configured — when minting the installation token.
  This is the pre-fix behaviour: nothing set `GITHUB_API_BASE_URL`, so the AGC defaulted to public GitHub.
  Fixed by upgrading; see [Upgrade — GHES gateways now reach their own appliance](upgrade.md#non-breaking-github-enterprise-server-gateways-now-reach-their-own-appliance-they-never-did).
- **Egress is denied.** The AGC reaches the right host name and the connection times out.
  The tenant's `EgressProxy` reports `GitHubEgressIncomplete=True` / reason `ApplianceRangesRequired`, with a message naming the host.
- **The host is not on the platform egress allowlist.** The pool is in an FQDN egress mode and the AGC cannot reach the appliance.
  The `EgressProxy` reports `GitHubEgressIncomplete=True` / reason `GitHubHostNotAllowlisted`, with a message naming the host.
  On a pool that worked before, this starts at the upgrade to `v2.0.0` ([upgrade note](upgrade.md#action-required-if-a-gateway-names-a-github-enterprise-server-host-an-fqdn-mode-pool-reaches-it-only-when-the-platform-allowlists-it)).

**Cause of the second.** The default `CIDR` egress mode programs the merged `api`/`actions`/`web` ranges from `api.github.com/meta` as the proxy pool's egress `ipBlock`.
A GHES appliance sits on your own address space and appears in none of them, so the `NetworkPolicy` denies the proxy's traffic to the one host it exists to reach.
The GMC cannot close this gap — the appliance's ranges are knowable only to you — so it names it on status instead.

**Cause of the third.** The tenant writes `githubURL`, so the GMC adds its host to the pool's FQDN policy and to the proxy's CONNECT allowlist only when the platform egress allowlist covers it, as it does for a `destinationFQDNs` entry.
A host the allowlist does not cover is left out of both.
A host that is not a DNS hostname, such as the wildcard `*.corp.example` or an IPv6 literal, is always left out, and the message says so instead of asking for an allowlist entry.

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
- **Switch to an FQDN egress mode.** `spec.egressPolicyMode: FQDN` (with a `--fqdn-policy-backend` configured) allows by hostname.
  The GMC adds a referring gateway's `githubURL` host to the policy and the proxy CONNECT allowlist once the platform egress allowlist covers it.

For `GitHubHostNotAllowlisted`, a platform admin adds the host to the egress allowlist; a tenant cannot self-serve.
Add it to the chart's `allowedEgressFQDNs` (the GMC `--allowed-egress-fqdns` flag), which takes a GMC rollout, or to the [watched ConfigMap](security-operations.md#self-service-additions-via-a-watched-configmap), which the pool picks up without one.
An entry covers its subdomains.
Removing an entry takes the host back out of every pool that relied on it, and so does deleting the ConfigMap or leaving an invalid entry in it, because an invalid ConfigMap is ignored whole.
Where the message says the host is not a DNS hostname, no entry helps: create the gateway again with a `githubURL` that names the appliance by hostname, or give the pool the appliance's address in `destinationCIDRs`.

The condition is advisory and does not gate `Ready`: the pool is serving exactly the policy it was asked for.
`ApplianceRangesRequired` clears once `destinationCIDRs` is non-empty — the GMC takes your declaration at face value and does not verify the ranges actually cover the appliance, so if traffic still fails, check the ranges themselves.
`GitHubHostNotAllowlisted` clears when the allowlist covers every host the pool's referrers name.

**Confirming it cleared.** `GitHubEgressIncomplete=False` always carries the reason `GitHubEgressAllowed`; the message says *which* clean state the pool is in.
The GMC takes the first that applies, so read the table top-down — only the last one means you supplied ranges:

| Message | The GMC sees no gap because |
|---|---|
| `egress policy is operator-maintained` | `spec.managedNetworkPolicy: false` — the GMC programs no `NetworkPolicy` for this pool, so it makes no claim either way. **Not a reachability check:** your own policy still has to allow the appliance. |
| `egress policy is operator-maintained; the proxy's CONNECT allowlist leaves out <host> …` | The same pool with `destinationCIDRs` set and a `githubURL` host the platform egress allowlist does not cover. The proxy admits a CONNECT to that host only when it resolves into `destinationCIDRs`, which the GMC cannot check. If the appliance is unreachable, allowlist the host or add its address to the ranges. |
| `FQDN mode carries every referrer's GitHub host` | `spec.egressPolicyMode` is not `CIDR`, and the platform egress allowlist covers each referring gateway's `githubURL` host, so the GMC added every one to the policy. |
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
apiVersion: actions-gateway.com/v2
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

`spec.tracing.caBundleRef` reports the same two reasons on the advisory `TracingCABundleUnresolved` condition instead, and the gateway stays `Ready`: see [Reaching the collector](observability-logging.md#reaching-the-collector).

```bash
kubectl -n team-a get actionsgateway my-gateway \
  -o jsonpath='{.status.conditions[?(@.type=="Degraded")].message}{"\n"}'
```

**This does not cover the appliance's egress ranges.** Trusting the CA does not put the appliance's address space in the NetworkPolicy; that is the separate problem [above](#a-ghes-tenants-traffic-never-reaches-the-appliance).

---

## Tenant Namespace Missing the Managed-Tenant Marker Label

**Symptoms.** An `ActionsGateway` never becomes `Ready`.
`kubectl describe` shows a `Warning` event with reason `NamespaceMarkerMissing`, and the GMC log reports a `Forbidden` error stamping Pod Security Admission labels, citing the `namespace-psa-guard` admission policy.
This is common immediately after upgrading a cluster whose tenant namespaces predate the policy (see [Upgrade — Migration Notes](upgrade.md#migration-notes)).

**Cause.** The GMC's cluster-wide `namespaces:patch` grant is gated by the `namespace-psa-guard` ValidatingAdmissionPolicy, which denies the GMC any namespace that is not labelled `actions-gateway.com/tenant: managed`.
The label confines the grant to managed tenants so a compromised GMC cannot relabel `kube-system` PSA (see [Security §5.1/§5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in)).
The GMC never sets this label itself — a trusted administrator must apply it.
The same marker also gates the `gmc-tenant-resource-guard` policy, which confines every tenant-resource write (Deployments, Secrets, RoleBindings, …) to marked namespaces; provisioning fails at the PSA-stamping step first, so `NamespaceMarkerMissing` is the signal you will see, but applying the label clears both gates.

**Diagnostics.**

```sh
# Confirm the warning event
kubectl describe actionsgateway -n <namespace> <name> | grep -A2 NamespaceMarkerMissing

# Check whether the marker label is present
kubectl get namespace <namespace> \
  -o jsonpath='{.metadata.labels.actions-gateway\.com/tenant}'   # want: managed

# Confirm both policies and their bindings are installed
kubectl get validatingadmissionpolicy gmc-namespace-psa-guard gmc-tenant-resource-guard
kubectl get validatingadmissionpolicybinding gmc-namespace-psa-guard-binding gmc-tenant-resource-guard-binding
```

**Resolution.** Apply the marker label as an administrator, then the GMC reconciler retries automatically:

```sh
kubectl label namespace <namespace> actions-gateway.com/tenant=managed
```

A namespace carrying only the 1.x `actions-gateway.github.com/tenant=true` marker is unmarked: `v2.0.0` no longer honours it.

If the GMC's ServiceAccount is installed under a non-default namespace or name, also confirm the policy's `matchConditions` username (`system:serviceaccount:gmc-system:gmc-controller-manager`) matches your install.

---

## ActionsGateway Stuck Deleting (Teardown Blocked on a Failing Delete)

**Symptoms.** You deleted an `ActionsGateway`, but the CR does not disappear: `kubectl get actionsgateway -n <namespace>` still lists it with a non-empty `metadata.deletionTimestamp`, and `kubectl describe` shows a repeating `Warning` event with reason `TeardownIncomplete`.
Some tenant resources (e.g. the AGC Deployment, RoleBinding, or a ServiceAccount) are still present in the namespace.

**Cause.** Teardown is **fail-closed by design** (Q125): the GMC keeps the cleanup finalizer on the CR and requeues until it can confirm *every* owned resource is deleted (or already gone).
If a delete keeps failing — most often an API-server error, or a `Forbidden` from an admission policy or revoked RBAC — the finalizer is retained on purpose so a live, credentialed AGC Deployment is never orphaned by a half-finished teardown.
A NotFound is treated as success, so an already-deleted resource never blocks convergence.

The gateway holds its own cleanup finalizer and **verifies each child is actually gone** after its delete is accepted — a child held by another controller's finalizer (its `deletionTimestamp` is set but the object lingers) also keeps the teardown open, with the lingering child named in the `TeardownIncomplete` event message.
The v2 per-tenant metrics Secrets are the one exception: they are removed by owner-reference garbage collection (the GMC deliberately holds no delete permission on Secrets), so they never appear in the event.

**Diagnostics.**

```sh
# Confirm the CR is mid-deletion and which resources remain
kubectl get actionsgateway -n <namespace> <name> -o jsonpath='{.metadata.deletionTimestamp}{"\n"}{.metadata.finalizers}{"\n"}'
kubectl describe actionsgateway -n <namespace> <name> | grep -A3 TeardownIncomplete

# The event message names the namespace and the underlying error; also check the GMC log
kubectl logs -n gmc-system deploy/gmc-controller-manager --tail=50 | grep -i "delete resource during teardown"
```

**Resolution.** Fix the underlying delete failure — restore API-server health, or re-grant the GMC the delete verb / re-apply the `gmc-tenant-resource-guard` marker if the namespace lost its `actions-gateway.com/tenant=managed` label (the policy gates DELETE too, so an unmarked namespace blocks teardown).
The reconciler retries on its own backoff and removes the finalizer automatically once every delete is confirmed.
**Do not** manually strip the finalizer to force the CR away — that re-introduces the orphaned-AGC failure mode the fail-closed behaviour exists to prevent; clear the real delete error instead.

**If the finalizer was already stripped.** Every namespaced child the GMC applies carries a controller `OwnerReference` to its `ActionsGateway`, so once the CR leaves etcd the Kubernetes garbage collector reclaims the AGC and proxy `Deployment`s, both `ServiceAccount`s, the AGC `RoleBinding`, the egress `NetworkPolicy`s, the `Service`s, the `PodDisruptionBudget`, the `HorizontalPodAutoscaler`, and the TLS `Secret`s — asynchronously and unordered, so worker pods are not drained first.
Give it a few seconds, then confirm the namespace is clean and delete anything left by hand:

```sh
kubectl get all,sa,rolebinding,networkpolicy,pdb,hpa,secret -n <namespace> -l app.kubernetes.io/managed-by=actions-gateway-gmc
```

Two categories are **not** covered by that cascade and must be checked explicitly after a stripped finalizer: the per-gateway `ClusterRoleBinding` (cluster-scoped, so a namespaced gateway cannot own it) and any object left over from a pre-v0.X install (the per-tenant `Role`, the legacy `actions-gateway` `NetworkPolicy`), which only the finalizer's explicit deletes reach.

```sh
kubectl get clusterrolebinding -l app.kubernetes.io/managed-by=actions-gateway-gmc
```

```sh
kubectl get role,networkpolicy -n <namespace>
```

---

## Tenant Namespace Stuck Terminating on agentpool-cleanup Finalizers

**Symptoms.** `kubectl delete namespace <tenant>` never completes.
The namespace sits in `Terminating` with nothing left in it, and the remaining objects hold `actions-gateway.com/agentpool-cleanup`.

**Cause.** The teardown order was inverted.
The AGC Deployments live *inside* the tenant namespace, so deleting the namespace (or an `ActionsGateway`, which cascades its AGC) before the `RunnerSet`s removes the very controllers whose finalizers have to clear.
Nothing will clear them afterwards — this is structural, not a slow reconcile you can wait out.

**Fix.** Delete in dependency order next time: the CRs first, then the gateways, then the namespace.
The ordered commands and the manual finalizer-drop recovery (including what that recovery skips — the GitHub-side runner deregistration) are in [migration-v1-to-v2.md § Teardown order is load-bearing](migration-v1-to-v2.md#teardown-order-is-load-bearing-never-delete-the-namespace-first).
The same order applies to any tenant teardown, migration or not.

---

## Tenant Namespace Stuck Terminating After Narrowing the PriorityClass Allowlist

**Symptoms.** Same surface as the entry above — the namespace sits in `Terminating` and the remaining `RunnerSet` objects hold their `agentpool-cleanup` finalizer — but the teardown order was correct and the AGC was still running when the CRs were deleted.
The AGC logs (and its retry errors) show the finalizer update itself being denied by the allowlist guard:

```text
ValidatingAdmissionPolicy 'gmc-priorityclass-allowlist-guard' with binding
'gmc-priorityclass-allowlist-guard-binding' denied request: priorityTiers
priorityClassName <class> is not in the platform PriorityClassAllowlist ...
```

**Confirm it.** The stuck object still names a class the allowlist no longer carries:

```sh
# The deleting objects, their finalizers, and the classes they still name
kubectl --context "$CTX" get runnersets.actions-gateway.com \
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

2. **Let the AGC finish.** Its retry backoff clears the finalizer and the namespace completes terminating on its own.
   **Do not** strip the finalizer by hand while the controller can still do it — that skips the GitHub-side runner deregistration the finalizer exists for.

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

## EgressProxy Rejected: `CiliumFQDN` / `CalicoFQDN` Are Not `egressPolicyMode` Values

**Symptoms.** Applying an `EgressProxy` that sets `spec.egressPolicyMode: CiliumFQDN` or `CalicoFQDN` is rejected by the apiserver's schema validation, because the `v2` CRD accepts only `CIDR` and `FQDN`.
This shows up on a **new** pool, on a pool being switched onto an alias, or on a GitOps apply that recreates a deleted one from a manifest nobody has updated.

**Cause.** The two per-CNI aliases were removed at `v2.0.0`; the CNI is chosen by the platform operator's GMC flag instead.
An alias that an upgrade left stored is the case [the pre-upgrade alias check](upgrade.md#before-upgrading-to-v200-no-egressproxy-still-names-a-deprecated-fqdn-alias) exists to catch.

**Recovery.** Replace the alias with the intent plus the operator-selected backend.
On the `EgressProxy`:

```yaml
spec:
  egressPolicyMode: FQDN   # was CiliumFQDN
```

and the platform operator sets the matching backend on the GMC:

```text
--fqdn-policy-backend=cilium   # or calico
```

The emitted policy is identical (each alias pins its namesake backend, and `FQDN` plus the matching selector resolves to the same emitter), so this is a re-label rather than a change in what is enforced.
If the flag is not yours to set, the backend is a platform decision: see [expressing GitHub egress by FQDN](security-operations.md#expressing-github-egress-by-fqdn-the-egresspolicymode-opt-in).

**If the flag says `none`**, the cluster has no FQDN backend and `egressPolicyMode: FQDN` is rejected for a different reason ("this cluster has no FQDN egress backend configured").
Use `CIDR`, or ask the platform operator to enable a backend.

**Finding every pool that still needs this** before an upgrade: [the pre-upgrade alias check](upgrade.md#before-upgrading-to-v200-no-egressproxy-still-names-a-deprecated-fqdn-alias).

---

## AGC CrashLoopBackOff or Not Acquiring Jobs

**Symptoms.** The AGC pod is restarting repeatedly, or it is running but `actions_gateway_scaleset_session_open` is `0` and `actions_gateway_scaleset_jobs_assigned_total` is not incrementing even when jobs are queued.

**Likely causes.**
- GitHub App Secret is missing, malformed, or contains an invalid private key.
- GitHub App `installationId` or `appId` is wrong.
- The proxy pool is not reachable from the AGC (network policy or proxy pod not ready).

An AGC that cannot obtain an installation token within two minutes of starting exits non-zero, which is the restart loop, and its log names the error after `initial token fetch`.
A running AGC whose set cannot open a message session keeps running and reports the failure on the `RunnerSet`.

**Diagnostics.**

```sh
# Check pod status and restarts
kubectl get pod -n <namespace> -l app.kubernetes.io/name=actions-gateway-controller

# Check logs for startup errors
kubectl logs -n <namespace> deploy/<gateway>-agc

# Check that the referenced Secret exists and has the right keys
SECRET=$(kubectl get actionsgateway -n <namespace> <gateway> -o jsonpath='{.spec.credentials.githubApp.name}')
kubectl get secret -n <namespace> "$SECRET" -o jsonpath='{.data}' | jq 'keys'
# Expected keys: appId, installationId, privateKey

# Test proxy reachability, on a gateway with an EgressProxy. The AGC image is distroless
# (no shell, no curl), so spawn an ephemeral curl pod in the same namespace.
kubectl run nettest-$$ -n <namespace> --rm -it --restart=Never \
  --image=curlimages/curl:latest \
  --overrides='{"spec":{"automountServiceAccountToken":false,"containers":[{"name":"c","image":"curlimages/curl:latest","command":["sh","-c","curl -x https://<egress-proxy>-proxy:8080 -sI https://api.github.com"]}]}}'

# Check RunnerSet conditions: Ready=False/NoActiveSessions means no session is open,
# and Degraded=True/Unauthorized means GitHub rejected a session call.
kubectl get runnerset -n <namespace> <name> -o jsonpath='{.status.conditions}' | jq .

# Check RunnerSet events: the AGC emits Warning events for the common failures.
kubectl describe runnerset -n <namespace> <name>
# Look for:
#   ScaleSetListenerStartFailed       — the listener could not register the scale set or open its session; the note carries the error.
#   SessionUnauthorized               — a session call was rejected as unauthorized: the GitHub App credentials are invalid or revoked.
#   NoActiveSessions / ListenerActive — Ready condition transitions.
```

**Resolution.**
- If the Secret is missing or has wrong keys, recreate it.
  See [Getting Started — GitHub App Secret](../getting-started.md#3-create-a-github-app-credential-secret).
- If the private key format is wrong, ensure it is a PEM-encoded key starting with `-----BEGIN RSA PRIVATE KEY-----` (PKCS#1) or `-----BEGIN PRIVATE KEY-----` (PKCS#8, RSA or Ed25519).
  The Secret `stringData.privateKey` must include the full key including header and footer lines.
- If `appId` or `installationId` are wrong, update the Secret.
- If `ScaleSetListenerStartFailed` names the guard ConfigMap, see [the next section](#scaleset-runnerset-stuck-not-ready-scalesetlistenerstartfailed-naming-the-guard-configmap).
- If `Ready=False` carries reason `RunnerGroupNotFound`, see [`RunnerSet` Stuck `Ready=False` With `RunnerGroupNotFound`](#runnerset-stuck-readyfalse-with-runnergroupnotfound).
- If the set reports `RunnerVersionTooOld=True`, see [Worker Image Runner Version](#worker-image-runner-version).

---

## ScaleSet RunnerSet Stuck Not Ready: `ScaleSetListenerStartFailed` Naming the Guard ConfigMap

**Symptoms.** A `RunnerSet` reports `Ready=False` with reason `NoActiveSessions`, and `kubectl describe runnerset` shows a Warning Event with reason `ScaleSetListenerStartFailed` whose message contains `load concluded-job guards`, typically ending `holds unparseable state (delete the ConfigMap to reset it)`.

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
If that line repeats for longer than the outgoing pod's termination grace period, look at the *previous* AGC's logs rather than the new pod's.
Two causes, and they need different reads.

The teardown never finished: its logs stop before `listener goroutines drained`, and the stuck message delete or session delete is in them.

Or the teardown finished and something reopened a session behind it.
On an AGC before the Q968 fix, the reconciler kept serving queued reconciles while the manager shut down, and one landing after the drain started a fresh listener whose session nothing would ever delete.
The tell is that the predecessor's logs look **healthy**: the drain line is there and reports success, with a listener start after it.
That is the case the guidance above sends you hunting a stuck delete that does not exist.
There is no in-cluster remedy, because the session is held at GitHub by a process that no longer exists and no AGC owns it to delete: the successor stays locked out until GitHub expires the session on its own.
Upgrade past the fix, which makes the drain one-way so a reconciler that has drained cannot start another listener.

---

## AGC Exits at Startup: GATEWAY_NAME Set but the v2 RunnerSet CRD Is Missing

**Symptoms.** A per-gateway AGC (`<gateway>-agc`) never becomes Ready, and its logs end at:

```
GATEWAY_NAME=<gateway> is set but the actions-gateway.com/v2 RunnerSet CRD is not
installed, so this AGC would reconcile nothing (install the actions-gateway-crds-v2 chart)
```

**Cause.** A gateway-scoped AGC reconciles only its own gateway's `RunnerSet`s.
With the v2 CRDs absent there is nothing for it to serve, so it exits rather than run a pod that passes its probes and reconciles nothing.

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
A mounted-but-unreadable CA is a misconfiguration: continuing without it would strip proxy trust and resurface much later as an unrelated-looking `unknown authority` failure on the first proxied GitHub call, so the AGC exits at startup instead.

The GMC renders the `proxy-ca` volume mode `0444` and projects only `tls.crt`, so neither error is reachable from a GMC-managed Deployment on its own.
A read error means the pod spec was mutated after the fact — a hand-edited Deployment, or a mutating policy engine rewriting volume modes or `securityContext`.
A parse error means the Secret's `tls.crt` is not a PEM certificate, which only a hand-edited Secret produces.
A hand-supplied proxy certificate is never kept: on its next reconcile of the `EgressProxy`, the GMC replaces any `tls.crt` that is not the single leaf it issued (a second certificate, a CA, another SAN, or a cert not paired with `tls.key`) and emits `ProxyCertificateIssued`.

**Diagnostics.**

```bash
kubectl get secret -n <namespace> <proxy>-proxy-tls -o jsonpath='{.data.tls\.crt}' \
  | base64 -d | openssl x509 -noout -subject -dates
```

```bash
kubectl get deploy -n <namespace> <gateway>-agc \
  -o jsonpath='{.spec.template.spec.volumes[?(@.name=="proxy-ca")]}'
```

**Resolution.** For the parse error, delete the `<proxy>-proxy-tls` Secret: the GMC re-issues an unparseable proxy cert on its next reconcile, and the proxy pods must be restarted to serve the new one.
For the read error, restore the mount as the GMC renders it — revert the manual edit, or exempt the tenant namespace from the mutating policy — then let the GMC re-render by bumping `ag.Spec`.
Either way the AGC recovers on its next restart.
An AGC that should egress directly must have no `proxy-ca` volume at all, rather than an empty or unreadable one.

---

## Long Polls Time Out on a Black-Holed Message-Queue Connection

**Symptoms.** `actions_gateway_message_poll_errors_total{reason="timeout"}` climbs and the AGC logs `scaleset: poll error` for a `RunnerSet`, even though the AGC pod is healthy.
The cause is typically a network event that silently drops an established connection: a firewall or NAT idle timeout that discards packets without sending a RST, an egress-proxy failover, or a hang at GitHub, so the long poll's TCP connection is *black-holed*: accepted but never answered.
The set receives no jobs while a poll is stalled.

**What happened.** The listener's `GetMessage` long poll holds the connection open for ~50s waiting for a message.
The poll client sets a `ResponseHeaderTimeout` of 55s: a healthy long poll is never cut short, and a black-holed connection is torn down a few seconds past the hold rather than at the operating system's TCP timeout.
The listener counts the timeout, backs off, and opens a fresh long poll.
No configuration is required; the bound is built in.

**Resolution.** A steady stream of `actions_gateway_message_poll_errors_total{reason="timeout"}` means the network is repeatedly black-holing queue connections.
Investigate the egress path: proxy or NAT idle timeouts shorter than the 50s long-poll hold are the usual cause.
Raise the idle timeout above ~60s so healthy long polls are not severed mid-hold.

---

## Reconcile or Token Mint Hangs on a Slow GitHub Endpoint

**Symptoms.** An AGC or GMC operation that calls a GitHub REST endpoint — installation-token mint, runner registration (`generate-jitconfig`), rerun-failed-jobs, or the GMC's `api.github.com/meta` IP-range fetch — appears to stall, and on a fixed version the logs now show a prompt `context deadline exceeded` / `Client.Timeout exceeded` error instead.
These are short request/response calls, distinct from the message-queue long poll above.

**What happened.** Before the Q138 fix these clients fell back to `http.DefaultClient`, which has no timeout: a peer that accepted the TCP connection but was slow — or never — to send response headers wedged the calling goroutine (a reconcile or a token fetch) until the multi-minute OS TCP timeout.
Fixed versions build these clients with a bounded default (an overall request timeout plus a transport response-header timeout), so a slow GitHub endpoint fails fast and retriably rather than stalling the work.
The message-queue long poll is the one deliberate exception — it is bounded by the response-header deadline above, not an overall timeout.

**Resolution.**
- Upgrade to a version with the Q138 fix.
  No configuration is required; the bound is built in.
- Repeated timeout errors point at the egress path to `api.github.com` / `*.githubusercontent.com` (proxy, NAT, or DNS latency), not the gateway — investigate connectivity to those hosts.

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
kubectl get networkpolicy -n <namespace> <proxy>-proxy \
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
- If the tenant manages its own egress policy (Cilium/Calico FQDN rules), set `spec.managedNetworkPolicy: false` on the `EgressProxy` so the reconciler leaves the policy alone.

---

## "Runner Lost Communication" and No Worker Pod Was Ever Created

**Symptoms.** GitHub fails the job with *"The self-hosted runner: … lost communication with the server.
Verify the machine is running and has a healthy network connection."* On the cluster there is **no worker pod at all** for that job — not `Pending`, not `Failed`, absent — and `kubectl get events` in the tenant namespace shows nothing about it unless you look at the owner object.
The message points at networking, but nothing is wrong with the network.

**Likely cause.** The API server rejected the worker pod, so the AGC never got one.
The job was assigned at GitHub and no runner ever came online to run it.
Any create rejection produces this shape: an invalid `metadata.name`, a policy-engine admission webhook, a `PodSecurity` label the pod violates, or a missing `PriorityClass`.

The most confusing instance was a name the AGC derived itself.
It is fixed, and worth recognising if you are running an older release, where the workaround is to rename the gateway to a different length.

- **The worker pod's own name (Q467).** `runner-<owner>-<jobID>` truncated to the 63-character DNS-label limit could land the cut on one of the job UUID's hyphens, and a name ending in `-` is rejected.
  Deterministic per gateway-name length: for an affected length, *every* worker pod was rejected and *no* job ever ran.

The name now derives through a bounded helper: the budget is split across the name's segments and each truncated tail carries a hash, so a derived name is always valid and still unique.


**Diagnostics.**

```sh
# The AGC records a WorkerPodCreateFailed Warning on the owner, carrying the API
# server's own message. This is the shortest path to the real cause.
kubectl describe runnerset -n <namespace> <name>
```

```sh
kubectl get events -n <namespace> --field-selector reason=WorkerPodCreateFailed
```

```sh
# The same rejection in the AGC log, with the pod name it tried to create.
kubectl logs -n <namespace> deploy/<gateway>-agc | grep -i "rejected worker pod"
```

**Resolution.**
- Read the API server's message in the event.
  `metadata.name: Invalid value` or `metadata.labels: Invalid value … must be no more than 63 bytes` means a derived name — upgrade to a release carrying the Q467/Q473 fixes, or shorten the gateway name (or its first runner label) in the meantime.
  An `admission webhook … denied the request` means a cluster policy engine — see [Worker / Proxy / AGC Pods Rejected by a Cluster Policy Engine](#worker--proxy--agc-pods-rejected-by-a-cluster-policy-engine).
- Confirm the pod really is absent rather than reaped: `kubectl get pods -n <namespace> -l actions-gateway.com/runner-set=<set>`.
- Re-run the workflow once the rejection is resolved if GitHub has already failed the job.
  The listener retries a failed pod create on later polls only while it still holds the job's assignment.

---

## Worker Pods Stuck Pending

**Symptoms.** Jobs are assigned (`actions_gateway_scaleset_jobs_assigned_total` increments) but worker pods remain in `Pending` state for more than 60 seconds.
`pod_creation_latency_seconds` p95 exceeds the 15s SLO target.

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

**Deadline.** A pod that stays Pending is not held forever: after `pendingPodDeadline` (default 10m, per `RunnerSet`) the AGC deletes it to free the concurrency-ceiling slot it was holding — see the next runbook section.
Diagnose a stuck pod (`kubectl describe pod`) *before* the deadline reaps it, or raise `pendingPodDeadline` temporarily while debugging.

---

## Worker Pod Reaped While Pending (WorkerPodStuckPending)

**Symptoms.** A `Warning` Event with reason `WorkerPodStuckPending` appears on the `RunnerSet` (`kubectl describe runnerset -n <namespace> <name>`), `actions_gateway_worker_pods_reaped_total{reason="pending_deadline"}` increments, and the job the pod was created for does not run: its workflow run and job conclude **`cancelled`** within seconds of the reap (`actions_gateway_abandoned_run_force_cancels_total{outcome="cancelled"}` increments alongside).
The worker pod itself is gone.
If the run instead lingers `in_progress` for ~15 minutes before the same `cancelled` ending, the force-cancel could not act — see the outcome labels below.

**What happened.** The pod stayed `Pending` longer than the owner's `pendingPodDeadline` (default 10m), so the AGC deleted it.
A permanently Pending pod would otherwise hold one of the set's concurrency-ceiling slots forever — the ceiling counts Pending pods.
The deadline is a capacity-protection mechanism; the job is re-queued separately, by the automatic re-run described below, and only once the set can place a worker pod again.

**The AGC force-cancels the run (Q683, Q766).** The worker never ran, so nothing will ever report its job, and the assignment is fire-and-forget, so the AGC has no way to report it either.
The reconciler that reaped the pod issues a REST `force-cancel` of the run and logs `force-cancelled the abandoned job's run`: measured live (2026-08-05), the standalone call is accepted and concludes the run *and* job as **`cancelled`** about one second later, and the cancelled run *does* accept `rerun-failed-jobs`.
This holds whether the reaped pod was never scheduled or was bound and still pulling its image; for the second, a real kubelet publishes a transient `Failed` before the pod goes, and the AGC still reads it as never having started (Q1231).
The same handling takes a still-`Pending` worker someone else deletes, such as a drain.
When GitHub refuses the call (`outcome="error"` on `actions_gateway_abandoned_run_force_cancels_total`), GitHub's own ~15-minute unstarted-job timeout reaches the same `cancelled` ending.
The AGC reads the run first, and a run already cancelled by anyone but the AGC is left alone: no force-cancel, no automatic re-run, and `actions_gateway_eviction_rerun_withheld_total{cause="abandoned",reason="run_cancelled"}` increments with a Normal `EvictionRerunWithheld` Event in place of a force-cancel outcome (Q1232).
That includes GitHub's own ~15-minute unstarted-job timeout, which the run's conclusion cannot tell from a user's cancel: with `pendingPodDeadline` set past ~15 minutes, the timeout cancels the run before the reap, and the job needs a manual re-run.
The identity comes off the worker pod's `actions-gateway.com/run-id` annotation, so a missing one reports as `actions_gateway_eviction_recovery_identity_unknown_total{cause="abandoned"}` with an `EvictionRecoveryIdentityUnknown` Warning Event, and means the assignment message carried no `workflowRunId`.
The cancelled run can belong to a different job than the reaped pod's: when another worker's runner took the pod's job, the AGC cancels the run of the job left without a runner instead, and logs `recovering the job left without a runner instead` with its `jobID` and `runID` (Q1195).
A worker evicted, preempted or drained before its container ran is handled the same way when no idle worker of its set could take a job instead (Q1235).
Where it cannot name that job because a `Running` worker has no job start recorded yet, it walks again once the start lands, every 30 seconds for up to ten minutes after the loss, and logs `re-checked the lost worker's walk` (Q1235).
Where the listener holds no record of the job, or the ten minutes pass, it cancels nothing and that job waits out GitHub's ~15-minute unstarted-job timeout.
The same holds where that job's run has another job running on a worker, such as a second leg of one matrix: a force-cancel would end the running job too, so the AGC logs `not force-cancelling, which would end that job` and leaves it (Q1235).

**Automatic re-run of the cancelled run (Q691).** The cancelled conclusion is recoverable, and the AGC recovers it for you: the run is queued for an automatic `rerun-failed-jobs`, fired once the owning set **places a worker pod again**.
The wait is the point.
The job was abandoned because its worker could not be scheduled, so re-running at once would put it straight back into the pool that was starved, and a shortage would compound into a re-run storm.
"Capacity returned" is a worker pod of this owner binding to a node (`PodScheduled=True`) after the abandonment; a pod that was already bound before it does not count.

Two bounds apply, and both are visible:

- **The retry budget.** The re-run draws on the same per-run budget as every other disruption recovery, so it reports as `actions_gateway_eviction_retries_total{cause="abandoned"}`, and a run abandoned again after its re-run is capped at `spec.maxEvictionRetries` re-runs in total.
  On exhaustion you get `actions_gateway_eviction_retries_exhausted_total{cause="abandoned"}` and an `EvictionRetriesExhausted` Warning Event on the owner, and the run then needs a manual re-run.
  The budget covers every disruption cause, so a run that is abandoned *and* evicted cannot spend two budgets.
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
kubectl get pods -n <namespace> -l actions-gateway.com/runner-set=<set> -w
kubectl describe pod -n <namespace> <worker-pod-name>
```

**Resolution.**
- Fix the unpullable image or unsatisfiable scheduling constraint — that is the root cause; the reap is the messenger.
- If scheduling is legitimately slow (autoscaled GPU nodes), raise `spec.pendingPodDeadline` on the `RunnerSet` above the worst-case node-provisioning time, e.g. `pendingPodDeadline: "30m"`.
- Fix the cause first, then let the automatic re-run land: it fires on the next worker pod the group places, so a fix that restores scheduling also releases the recovery.
  Re-run from the GitHub UI when the budget is already exhausted (`eviction_retries_exhausted_total{cause="abandoned"}`) or the wait expired (`abandoned_run_rerun_waits_total{outcome="expired"}`).

---

## Worker Pod Reaped While Pending After Its Job Completed (WorkerPodCompletedPending)

**Symptoms.** A `Warning` Event with reason `WorkerPodCompletedPending` appears on the `RunnerSet` (`kubectl describe runnerset <name> -n <namespace>`) and `actions_gateway_worker_pods_reaped_total{reason="completed_pending"}` increments.
Before the reap, the pod is `Pending` with a `FailedMount` event naming a `job-ss-<jobID>` Secret that does not exist.

**What happened.** GitHub reported the job terminal while its worker pod was still `Pending`, so the AGC deleted the pod thirty seconds later.
That pod could never have run: the job's terminal `JobCompleted` reclaims the JIT-config Secret the pod mounts, and a pod that has not mounted yet cannot mount one that is gone.

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
  On a current release the queue is pruned as jobs conclude, so a restart burst instead means messages are **not** being deleted — check the AGC log for two lines.
  `delete acked message` names the message id and the error each *rejected* delete returned.
  `queue reported the acked message already gone` is the quieter one: the delete was accepted but removed nothing, which is what a backend that has stopped serving the delete endpoint answers.
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

**What happened.** The pod was still `Running` five minutes after GitHub reported the job its runner held terminal, so the AGC deleted it.
For a job that ended before any runner started it, the set is left with one runner more than it has jobs, and one idle worker goes on the clock: the pod the AGC created for that job, or, when that pod's runner has started a different job, the oldest `Running` worker whose runner has started nothing (Q1153).
Such a pod carries `actions-gateway.com/surplus-for-job` naming the job that freed it.
If its runner is given a job after all, the stamp comes off it and moves to another idle worker when the AGC records that job's start.
A start the AGC fails to record is redelivered until it is recorded.
A start that reaches the AGC more than five minutes after the stamp, because the listener's session was down that long, comes too late, and the worker can be reaped mid-job.
GitHub gives a job to whichever of the set's runners asks first, so a worker often runs a job other than the one it was created for; the AGC follows the runner GitHub names, not the job ID in the pod name.
Three causes produce that:

- **A worker that never received its job** (the common one).
  The AGC provisions fire-and-forget: the worker registers and pulls its own job.
  If the assignment lapsed, was cancelled, or completed elsewhere before the runner got to it, the runner waits at `Listening for Jobs` forever.
  It holds a concurrency slot, a namespace-quota slot, and a node while doing nothing.
- **A worker image on an out-of-date runner release**, when *every* job does this.
  GitHub's [runner update policy](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/autoscaling-with-self-hosted-runners) stops queueing jobs to a runner not updated "within 30 days of a new version being made available", and says nothing about refusing its connection, so such a runner would still log `Listening for Jobs` with nothing on the cluster naming the cause.
  Where the 30 days start is not documented precisely: this project's own runs kept receiving jobs well past 30 days after the first newer release, and stopped somewhere between 19 and 34 days after the newest one.
  Measured on the dogfood cluster: a 2.335.1 worker received no job on 2026-09-29, 34 days after 2.337.0 shipped, and the same tenant on a 2.337.0 worker received and ran one that day.
  Compare the version the worker logs (`runner version detected`, see [Worker Image Runner Version](#worker-image-runner-version)) with the newest [actions/runner release](https://github.com/actions/runner/releases) and its date.
  `RunnerVersionTooOld` does not catch this: it checks the registration floor, not the 30-day window.
- **A container that outlived the runner** — an injected mesh sidecar, or a regular (non-native) build/DinD sidecar.
  See the two runbook sections below for fixing the root cause; this reap is the backstop that stops those pods accumulating.

**On AGC `v1.3.0` through `v1.8.0` there is a fourth cause, and it kills a live job.** Those releases stamp the pod *created for* a job when that job completes, whichever runner ran it, so a worker running a different job is reaped five minutes after its own job's namesake finished (Q1151).
The job fails at GitHub with `The runner has received a shutdown signal`, and a `FailedMount` for the pod's `job-ss-*` Secret precedes the reap by about a minute.
Upgrade; nothing on those releases prevents it.

**Diagnostics.**

```sh
# The reap event names the deleted pod and the grace that elapsed
kubectl get events -n <namespace> --field-selector reason=WorkerPodOrphanedRunning

# Which Running workers already have a completion stamp (they are on the clock),
# and which job each one's runner actually started
kubectl get pods -n <namespace> -l actions-gateway.com/runner-set=<set> \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,JOB-DONE:.metadata.annotations.actions-gateway\.com/job-completed-at,STARTED:.metadata.annotations.actions-gateway\.com/started-job-id,SURPLUS-FOR:.metadata.annotations.actions-gateway\.com/surplus-for-job'

# Rate of orphan reaps per set
# PromQL: rate(actions_gateway_worker_pods_reaped_total{reason="orphaned_running"}[1h])
```

**Resolution.** The reap itself needs no action — it returns the slot and the node.
Treat a *sustained* rate as the signal:

- Recurring orphans with no sidecar in the template mean jobs are being assigned and then lost before the worker can pull them.
  Check for a provisioning stall upstream — namespace `ResourceQuota` denials (`WorkerQuotaExceeded`), scheduling pressure (`WorkersUnschedulable`), or a `maxWorkers` set above what the quota can hold, which puts the AGC into a provision/deny/retry loop.
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

Before deleting any of those, confirm the job really is over at GitHub — check the run in the Actions UI, or `kubectl logs <pod> -c runner` and look for a worker sitting at `Listening for Jobs` with nothing after it.
**A pod whose job is genuinely still executing looks identical in cluster state**, and deleting it strands that job with no replacement.

```sh
# 3. Only once confirmed abandoned:
kubectl delete pod <pod> -n <namespace>
```

Prevention is on the teardown side: never scale down the pool carrying the AGCs while jobs are in flight.
For the dogfood cluster, `scripts/dogfood/stop.sh` drains in-flight workers before resizing and fails the stop rather than stranding them.

---

## Worker Killed by the Lifetime Cap (WorkerPodLifetimeExceeded)

**Symptoms.** A job that used to finish now fails partway through, always at roughly the same elapsed time.
The worker pod is `Failed` with `reason: DeadlineExceeded`, a `Warning` Event with reason `WorkerPodLifetimeExceeded` appears on the `RunnerSet`, and `actions_gateway_worker_pods_reaped_total{reason="lifetime_exceeded"}` increments.

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

Set it to `"0s"` to disable the cap entirely — that restores the pre-cap behaviour, including the orphan risk above, so prefer a raised value over an opt-out.
A negative value is rejected at admission.
An `activeDeadlineSeconds` set explicitly on the pod template takes precedence over this field and is never overwritten, which is the escape hatch for a per-template exception.

Note the ceiling you cannot raise past: GitHub terminates any job on a self-hosted runner at **5 days** regardless of this setting.

---

## Worker Pods Reaped on Gateway Teardown (WorkerPodsReapedOnGatewayTeardown)

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

**Symptoms.** One job is never picked up while others in the same `RunnerSet` run fine.
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
3. **Sweep at listener start.** When a listener starts it lists the records named `<scaleSet>-*` and deletes those whose remaining name is a job UUID with an optional attempt, and that are offline, not busy, and not claimed by any live worker pod.
   This collects what the first two cannot: records whose pod is already gone (an AGC that crashed between deregistering and deleting), and the suffixed `-1`/`-2`/`-3` names the conflict path mints and never revisits.
   It leaves every other name under the prefix alone and logs `scaleset: sweep left records under this set's prefix whose names it did not mint` with the count.
   That line is expected when another set's first label extends this one's, such as `ci` and `ci-linux` under one org.
   With no such set, read the names in the runner list: a job ID that is not a UUID means the sweep collects nothing here, so use the cleanup below.

A record whose worker pod still exists is never swept, in any phase — a `Pending` worker's record is legitimately offline, and deleting it would strand exactly the job the sweep exists to protect.

> **Before `v1.3.0`, only mechanism 2 existed**, and it clears a record only if the REST name filter resolves it.
> Records it could not resolve, and every suffixed retry name, accumulated unbounded — 22 stale records under one scale set wedged the `v1.3.0-rc.2` validation window this way.
> If you see `scaleset: runner name is taken but no record resolves under it` in the AGC logs, mechanism 2 is failing and the sweep is what will collect the leftovers.
> On such a version, use the manual cleanup below.

> **Before `v2.0.0`, the sweep matched on the prefix alone**, so a listener named `ci` could delete the offline record of a set named `ci-linux`, including one whose worker was still `Pending`.
> That job's runner has no record to connect under, so a worker can end `Completed` without running it ([why](../design/05-security.md#cross-tenant-deletion-of-a-runner-record-by-first-label-overlap)).

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

**Symptom.** A `RunnerSet` reports `JobProvisionStalled=True/WorkerCeilingReached` and `actions_gateway_scaleset_jobs_deferred{reason="ceiling"}` is `> 0`.
Jobs are queued at GitHub with no worker running them, while the set's existing workers keep running normally.

**This is normal backpressure, not a fault.** The set is running as many workers as its spec allows — `spec.maxWorkers`, or the last `spec.priorityTiers` threshold.
GitHub keeps assigning jobs up to the capacity the listener advertises, and each one that arrives with no slot free is held and re-offered every 5 seconds until a running worker finishes.
Nothing is dropped, and the held jobs start in turn as capacity frees.

```sh
kubectl get runnerset <name> -n <namespace> \
  -o jsonpath='{.status.conditions[?(@.type=="JobProvisionStalled")].message}'
```

**The condition's five reasons.** `JobProvisionStalled` is advisory — it never gates `Ready`:

| `status` / `reason` | Meaning |
|---|---|
| `True` / `RunnerNameConflict` | At least one held job cannot register its runner name. An anomaly worth acting on — see [Scale-Set Job Stranded by a Stale Runner Record](#scale-set-job-stranded-by-a-stale-runner-record-runner-name-409). Outranks `WorkerCeilingReached` whenever both classes are held at once, because it is the one you can do something about; a full ceiling clears itself. |
| `True` / `WorkerTerminating` | At least one held job is waiting for its previous worker pod to finish terminating; see [Scale-Set Job Waiting for Its Previous Worker to Terminate](#scale-set-job-waiting-for-its-previous-worker-to-terminate-workerterminating). Outranks `WorkerCeilingReached` and is outranked by `RunnerNameConflict`. |
| `True` / `JobSourceUnverified` | At least one held pull request job could not be checked for a fork, because its run could not be read; see [Scale-Set Job Held Because Its Run Could Not Be Read](#scale-set-job-held-because-its-run-could-not-be-read-jobsourceunverified). Outranks `WorkerTerminating` and `WorkerCeilingReached` and is outranked by `RunnerNameConflict`. |
| `True` / `WorkerCeilingReached` | Every held job is waiting only on worker capacity — this section. |
| `False` / `JobsProvisioning` | No assigned job is waiting on a runner name, a terminating worker, a source check, or capacity. Published when the listener opens its session and again the moment the last held job provisions or completes, so a set that has never stalled reports it too. |

**When to act.** Only when the wait itself is the problem.
Either raise the ceiling (`spec.maxWorkers` or the top `priorityTiers` threshold) if the cluster has room, or treat it as the signal that the tenant needs more capacity.
Check first that the ceiling is the real constraint and not a downstream one — `WorkerQuotaExceeded` (namespace `ResourceQuota`) and `WorkersUnschedulable` (nothing can place the pod) are separate conditions with their own sections.

**What it should NOT look like.** A steady stream of `generate-jitconfig` and runner-deregister calls against GitHub while the set sits at its ceiling.
Before `v1.3.0`, a ceiling-blocked job was read as a transient failure: the queue message was redelivered immediately, so the listener retried it several times a second and minted (then deregistered) a runner registration on every pass — 704 deregister calls for a single job during one 14-minute window.
If you see that on an older version, the fix is to upgrade; the ceiling wait itself was never the bug.

---

## Scale-Set Job Waiting for Its Previous Worker to Terminate (WorkerTerminating)

**Symptom.** A `RunnerSet` reports `JobProvisionStalled=True/WorkerTerminating`, `actions_gateway_scaleset_jobs_deferred{reason="worker_terminating"}` is `> 0`, and a `WorkerTerminating` Normal Event is recorded.
The AGC logs `scale-set worker pod is still terminating; re-offering the job once it is gone` with the pod name.

**What happened.** The listener's session was re-created while one of the set's worker pods was being deleted, and the new session replayed that pod's job.
A scale-set worker pod is named for its job, so the replay cannot create a new worker until the old pod is gone.
The listener holds the job and re-offers it after 30 seconds, doubling to every 5 minutes; once the old pod is deleted, the next re-offer creates the job's new worker.
After an ordinary grace period this clears by itself within a minute.

**When to act.** When it does not clear.
A pod that stays `Terminating` past its grace period is usually on a node that stopped responding, and stays that way until the node is removed or the pod is force-deleted:

```sh
kubectl get pod <pod-name> -n <namespace> -o wide
```

If the node in the `NODE` column is `NotReady`, recover or remove the node, or force-delete the pod once you are sure it is not running anywhere:

```sh
kubectl delete pod <pod-name> -n <namespace> --grace-period=0 --force
```

Releases after `v1.10.0` hold the job this way; earlier ones treated the old pod as the job's worker, so the job was acknowledged and no worker ever started for the runner the replay registered (Q1199).

---

## Scale-Set Job Held Because Its Run Could Not Be Read (JobSourceUnverified)

**Symptom.** A `RunnerSet` reports `JobProvisionStalled=True/JobSourceUnverified`, `actions_gateway_scaleset_jobs_deferred{reason="source_unverified"}` is `> 0`, and a `JobSourceUnverified` Warning Event is recorded.
The AGC logs `scaleset: could not check the assigned job's source; holding it` with the error from GitHub.

**What happened.** The set refuses fork pull request jobs, which is the default, and a job triggered by `pull_request`, `pull_request_review` or `pull_request_review_comment` (or one naming no event) arrived.
Telling a fork's job from the repository's own takes a read of the run from the GitHub REST API, and that read failed.
The listener holds the job rather than run it unchecked, mints no runner, and re-reads after 30 seconds, doubling to every 5 minutes.
It clears on the first read that succeeds.

**When to act.** When it does not clear.
The logged error names the cause: a 5xx or a connection error is GitHub or the egress path, and clears when they recover; a 401 or 403 means the App's token cannot read Actions on that repository, which needs `Actions: Read and write` ([onboarding](tenant-onboarding.md#0a-create-the-app-web-ui)).
A set that is meant to run untrusted pull requests can skip the read entirely with `spec.forkPullRequests: Accept`.

---

## Fork Pull Request Job Refused (ForkJobRefused)

**Symptom.** A contributor reports their pull request's workflow run was cancelled before anything ran, and the `RunnerSet` records a `ForkJobRefused` Warning Event naming the head repository and the run.
Where the GitHub App holds `Checks: Read and write`, the pull request also carries a failed check named `actions-gateway: fork pull request refused`.

**What happened.** The job came from a fork pull request and the set does not set `spec.forkPullRequests: Accept`, so the AGC minted no runner and force-cancelled the run.
GitHub has no call that cancels a single job, so the run's other jobs were cancelled too.
What counts as a fork's job: [security operations](security-operations.md#fork-pull-requests-choose-their-own-runner-label).

**When to act.** If the set should run fork pull requests, set the field, then re-run the workflow:

```bash
kubectl patch runnerset <name> -n <namespace> --type merge \
  -p '{"spec":{"forkPullRequests":"Accept"}}'
```

Set it only on a set built for untrusted code.
Otherwise the refusal is working as intended, and the contributor's workflow should target a set that accepts fork pull requests, or a GitHub-hosted runner.

A `ForkRefusalCheckRunSkipped` Normal Event beside it means the check run could not be written, usually because the App lacks `Checks: Read and write`, so the contributor saw only a cancelled run.

A refused job whose assignment carried no run identity is not cancelled, since there is no run to address, and waits until GitHub ends it; its Event says so.

---

## Scale-Set Assignments Abandoned (AssignmentAbandoned)

**Symptom.** A `RunnerSet` records an `AssignmentAbandoned` Warning Event, `actions_gateway_scaleset_jobs_abandoned_total` steps up, and `JobProvisionStalled` clears at the same moment.
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

**Symptoms.** Worker pods sit `Running` with a not-ready container count (`READY 1/2`) after their job completed, and `completedPodTTL` never deletes them.
While it waits, each pod holds a concurrency slot, so a busy set can reach `maxWorkers` with no job actually executing and new jobs stop being picked up.
`kubectl get pod -o jsonpath='{.spec.containers[*].name}'` shows a second container such as `istio-proxy` or `linkerd-proxy`.

**What happened.** A service-mesh sidecar was injected into the worker pod.
GAG worker pods run to completion: the slot is freed and the pod reaped only when the pod reaches a *terminal* phase (`Succeeded`/`Failed`), which requires every container to exit.
A classic mesh sidecar never exits on its own, so the pod stays `Running` forever and falls through the two phase-based reaper paths (`completedPodTTL` covers terminal pods; `pendingPodDeadline` covers `Pending` pods — neither covers a stuck `Running` pod).

There is a backstop: once GitHub reports the job terminal, a pod still `Running` five minutes later is reaped as [`WorkerPodOrphanedRunning`](#worker-pod-reaped-while-running-workerpodorphanedrunning), so the slot comes back instead of wedging the set.
It is a backstop, not a fix — the pod is still killed rather than completing, so resolve the sidecar as below.

**Resolution.** Opt the GAG tenant namespace out of the mesh, or — if mesh membership is mandatory — switch to native sidecars (Kubernetes 1.28+) or a sidecar-less/ambient data plane.
The full per-mesh configuration (Istio sidecar + ambient, Linkerd, Cilium, generic) is in [Running GAG Alongside a Service Mesh](service-mesh-coexistence.md).
Note that mesh opt-out/exclusion annotations set on the `RunnerTemplate` `podTemplate` are **not** honored — GAG strips arbitrary worker-pod-template metadata; configure the mesh at the namespace level instead.

A mesh sidecar is *injected* at admission, so it is not in the `RunnerTemplate` and GAG cannot warn about it ahead of time — the runtime symptom above is the only signal.
A **build/DinD sidecar you declare in the template** is different: GAG detects it and warns proactively — see the next section.

---

## RunnerSet Reports PossibleReapBlockingSidecar (Build/DinD Sidecar in the Template)

**Symptoms.** A `RunnerSet` reports the advisory condition `PossibleReapBlockingSidecar=True` (`kubectl get runnerset <name> -n <ns> -o jsonpath='{.status.conditions}'`), the `actions_gateway_reap_blocking_sidecar_templates` gauge is `> 0`, and/or `kubectl apply` of the `RunnerTemplate`/`ClusterRunnerTemplate` printed a `Warning:` naming a sidecar container.
Left unfixed, the symptom is the same `READY 1/2` stranding as the mesh-sidecar case above: worker pods linger for five minutes after the job, holding their slots, until they are reaped as `WorkerPodOrphanedRunning`.

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

**Symptoms.** A `RunnerSet` reports `RunnerVersionTooOld` with reason `WorkerImageBelowMinimum` (`True`) or `WorkerImageVersionUnknown` (`Unknown`), and a `WorkerImageBelowMinimum` Warning event names both versions.
Jobs may still be running normally: the condition is a prediction about GitHub's enforcement, not a report that something already broke.

**What happened.** GitHub refuses to register a self-hosted runner below an enforced minimum version, `2.329.0` as of the 2026-06-12 changelog, and separately requires each new runner release be installed within 30 days of publication for the runner to keep executing jobs.
Every reconcile, the AGC reads the runner version off the effective worker image reference (the set's `workerImage`, else the AGC's `WORKER_IMAGE`, else the digest-pinned built-in default) and compares it to that floor.
It asks GitHub nothing, because the scale-set protocol carries no runner version at session creation, so the listener never sees a version rejection.

The tag is the immediate answer, and the AGC then reads the image itself out of its registry: it fetches the manifest and streams the layers, topmost first, until it finds the runner's own dependency manifest (`bin/Runner.Listener.deps.json`), the one version record the image carries.
That reading is the verdict once it lands, whatever the tag says, and the message names the digest it was read at and any disagreement with the tag: `ships actions/runner 2.335.1, read from the registry at sha256:… (linux/amd64)`.
It covers a digest-only reference and a custom tag, so `Unknown` now means one of three things the message spells out: the read is still in progress, the read failed and the tag alone could say nothing, or the image carries no `deps.json` in its layers and is not `actions/runner`-derived where the runner layout puts it.

The read happens once per image digest per AGC process; a tag-addressed reference has its manifest re-fetched hourly since a tag can move, and its layers are streamed again only when the digest moved.
It costs about 114 MB of egress for the official image, which is 544 MB.
Credentials come from the pod template's `imagePullSecrets` (`kubernetes.io/dockerconfigjson`), or, when the template names none, from the worker ServiceAccount's, which is the same set admission puts on the worker pod; the AGC reads both through its own Role.
A registry kubelet reaches through the node's identity is not something the AGC can present ([Q1066](../queue/Q1066.md)).
Reach is the AGC's egress policy's: by default it admits 443 and 6443 to any destination and no other port, so a registry on a custom port (`registry.example.com:5000`) is out of reach, and an install that scopes it with `apiServerCIDRs` closes the registry too, and carries no registry allowance to reopen it ([the decision](../design/05-security.md#a-scoped-agc-egress-policy-carries-no-registry-allowance)).
A failed read is not an incident: the AGC logs it at warning level, retries on a backoff that doubles from one minute to an hour (on the set's next reconcile once the delay has passed, since the resolver runs no timer of its own), and the verdict the tag supports stands with the failure appended, so a reference whose tag names a runner version never regresses to `Unknown` because a registry was unreachable.

`Unknown` is deliberately not `False`: a custom image is exactly where a stale runner hides, and reporting "current" for an image nothing has checked would be worse than saying so.

**A `True` verdict makes the set impaired**, so it rolls up into the gateway's `RunnerSetsDegraded` condition.
`Unknown` and `False` do not.

**Resolution.**

- **`WorkerImageBelowMinimum`**: build or pull a `workerImage` on runner `2.329.0` or later and update the spec.
  Prefer both a tag and a digest (`myrepo/runner:2.335.1@sha256:…`): the digest is what pins the image, and the tag is what makes the version checkable before the registry read lands.
- **`WorkerImageVersionUnknown`**: read the message first.
  `registry read of the image failed (attempt N: …)` names what the AGC could not do: an HTTP 404 means the reference does not resolve at that registry, and a dial error means the AGC's egress policy does not reach it.
  An authentication failure is about the AGC's own read and not about the pull: kubelet can hold a credential the AGC cannot, so worker pods may be pulling and running normally while this fails.
  Do not read it off pod state either way, since a digest-pinned image defaults to `imagePullPolicy: IfNotPresent` and a node that already has the layers runs the pod without contacting the registry at all.
  Where the failure names the credential the AGC presented, in a trailing parenthesis, that is what decides the fix, whichever message carries it; the rest fall to the out-of-reach case below.
  Anything ending `(as <user>)`, whether `unauthorized after authenticating` or `token exchange with <host>: HTTP <n>`, means a Secret the pod template or the worker ServiceAccount names was read and presented and the registry refused it, so the credential is expired, wrong, or carries no pull scope on that repository: replace it rather than naming another.
  Anything ending `(anonymous)`, and `requires a login and no imagePullSecret names it`, means none was presented, and what to do then turns on whether a `kubernetes.io/dockerconfigjson` Secret exists in the namespace (`kubectl get secrets -n <namespace> --field-selector type=kubernetes.io/dockerconfigjson`).
  If one does, name it in `podTemplate.spec.imagePullSecrets`, or on the worker ServiceAccount when the template names none.
  The template's list replaces the ServiceAccount's rather than adding to it, for the pull as for this read, so a template naming one Secret hides every Secret on the ServiceAccount.
  A failure ending `(worker ServiceAccount imagePullSecrets unread: …)` means the AGC could not read the ServiceAccount, usually because the tenant Role predates the `get` on `serviceaccounts` grant: re-render it from the current chart.
  A change to the ServiceAccount or the Role is picked up at the read's next retry, up to an hour later, while a change to the pod template's `imagePullSecrets` is read at once; restarting the AGC drops the failed reading and retries immediately.
  If none does, either no pull credential is configured at all, so create one and name it there, or kubelet is reaching the registry through the node's identity (Artifact Registry under Workload Identity, ECR under the node role) or a node-level `config.json`, neither of which the AGC can present ([Q1066](../queue/Q1066.md)), leaving the tag to carry the verdict.
  Worker pods that are themselves stuck on the image are a separate problem with its own reading: see the air-gapped install's [verification step](air-gapped-install.md#7-verify).
  `context deadline exceeded` means one inspection ran past its fifteen-minute budget, which starts once the inspection holds its slot, not while it queues behind another image.
  `carries no bin/Runner.Listener.deps.json` means the image is not `actions/runner`-derived in the expected layout, and no tag can fix that.
  `bearer realm "<url>" is not https` is the one authentication failure the remedy below does not fit: the registry answered the `401` with a Bearer challenge naming a plaintext token endpoint, and the AGC refuses it rather than send the login over it.
  The registry is reachable and no tag can change the challenge, so the fix is on the registry: serve the token realm over TLS.
  An internal or air-gapped mirror is where this shows up.
  Where the registry is out of reach or cannot be authenticated to, re-tagging with the runner version the image ships restores the tag verdict, or read what a worker actually ran.
  The injected wrapper reads the version from the runner's own dependency manifest rather than from the tag, and hands it back on the pod's termination message, so a `RunnerSet` carries the last one it saw (Q792):

```bash
kubectl get runnerset <name> -n <namespace> -o jsonpath='{.status.observedRunnerVersion}'; echo
```

Empty means no worker pod has terminated and reported yet, or the wrapper could not detect a version.
The per-pod log line is the same answer for a pod that is still running, and names the reason when detection failed:

```bash
kubectl logs -n <namespace> <worker-pod> -c runner | grep "runner version"
# runner version detected version=2.335.1
```

`runner version not detected` in that log means the image does not carry `bin/Runner.Listener.deps.json` where the runner layout puts it: it is not `actions/runner`-derived, or the runner lives somewhere `RUNNER_HOME_DIR` does not point at.

> **`observedRunnerVersion` is a self-report, and the condition deliberately does not move for it.** The runner container runs your image and your job's own steps run inside it, so anything in that container can rewrite the report before the container terminates.
> It answers *what does this image say it ships* for an operator debugging their own image, which is what the `Unknown` case needs.
> It is not evidence against a hostile image, so `RunnerVersionTooOld` keeps reporting `WorkerImageVersionUnknown` rather than turning a tenant-controlled value into a verdict.
> Reporting "current" for an image nothing has checked would be worse than saying so, which is the same reason `Unknown` is not `False`.

```bash
# The verdict and its message for one set
kubectl get runnerset <name> -n <namespace> \
  -o jsonpath='{.status.conditions[?(@.type=="RunnerVersionTooOld")]}'
```

---

## Job-Lifecycle Events on a RunnerSet

**What this is.** Beyond `WorkerPodStuckPending` (above), the AGC records `Warning` Kubernetes Events on the owning `RunnerSet` when a job-lifecycle transition fails terminally (Q170).
They are the event-based companion to the always-present metrics and status conditions — surfacing the same incident in `kubectl describe`, `kubectl get events`, and any event watcher, without a Prometheus query.
Each `Reason` mirrors the corresponding metric name so you can correlate the two.

Events are recorded **on a transition / terminal outcome**, not on every reconcile or every requeue, so they do not spam.
(The cluster's event recorder additionally aggregates repeats of the same reason+message into one event with a count.)
An event is recorded on the owner's next reconcile, so it can trail the underlying metric by a few seconds; the metric is the real-time signal.

| Reason | Type | Meaning | Where to look next |
|---|---|---|---|
| `ScaleSetListenerStartFailed` | Warning | The listener could not register the scale set or open its session; the note carries the error. Also sets `Ready=False`, with reason `NoActiveSessions` or `RunnerGroupNotFound`. | [AGC CrashLoopBackOff or Not Acquiring Jobs](#agc-crashloopbackoff-or-not-acquiring-jobs) |
| `WorkerImageBelowMinimum` | Warning | The effective `workerImage` ships a runner below GitHub's enforced minimum, read at reconcile and emitted once per transition, before GitHub rejects anything. Also sets the `RunnerVersionTooOld` condition. | [Worker Image Runner Version](#worker-image-runner-version) |
| `SessionUnauthorized` | Warning | A session call was rejected as unauthorized: the GitHub App credentials are invalid or revoked. Also sets the `Degraded` condition. | [GitHub App Secret Misconfiguration](#github-app-secret-misconfiguration) |
| `QuotaRetriesExhausted` | Warning | Worker pod creation was abandoned after exhausting the namespace `ResourceQuota` retry budget (`maxQuotaRetries`). | [Jobs Failing Due to Namespace ResourceQuota Exhaustion](#jobs-failing-due-to-namespace-resourcequota-exhaustion) |
| `WorkerPodCreateFailed` | Warning | The API server refused to create a worker pod (invalid name, admission webhook, pod-security policy). The note carries the API server's own message. No pod exists, so GitHub reports only that the runner lost communication. | ["Runner Lost Communication" and No Worker Pod Was Ever Created](#runner-lost-communication-and-no-worker-pod-was-ever-created) |
| `EvictionRetriesExhausted` | Warning | An evicted worker pod's auto-retry budget (`maxEvictionRetries`) is exhausted; a manual re-run is required. | [Evicted Worker Pods Exhausting Retry Budget](#evicted-worker-pods-exhausting-retry-budget) · [Evicted Scale-Set Jobs Are Not Re-Run Automatically](#evicted-scale-set-jobs-are-not-re-run-automatically) |
| `EvictionRerunFailed` | Warning | A disrupted run's automatic re-run was never accepted by GitHub — refused past the 15-minute re-run window, or a terminal API error (Q503). The budget slot is spent; the named run needs a manual re-run. | [Evicted Worker Pods Exhausting Retry Budget](#evicted-worker-pods-exhausting-retry-budget) |
| `EvictionRerunWithheld` | Normal | Recovery deliberately did not ask GitHub to re-run. Either the run had already concluded `cancelled` by someone other than GAG, so the cancel stands (Q811, Q1181); or the kubelet evicted the worker for exceeding its own `ephemeral-storage` or `emptyDir` limit, which a re-run repeats, and the Event quotes the kubelet's message (Q1177); or the re-run would be futile: past GitHub's 50 re-runs, older than its 30-day window, or superseded by a newer commit's run (Q1181); or a runner killed for memory that a re-run would repeat, or while out-of-memory retries are paused (Q1183). Only a cancel seen after detection spends a slot. | [Cancelling a Run Does Not Stop Its Worker Pod](#cancelling-a-run-does-not-stop-its-worker-pod); [Which Disruptions Auto-Re-Run a Job](#which-disruptions-auto-re-run-a-job-and-which-never-do) |
| `FlakyJobDetected` | Normal | A scale-set job failed with no disruption attributed to it, and a later attempt of it succeeded: a flake, re-run by a human or by GAG alongside a disrupted job (Q555). Names the run and the job. Not a fault of the platform; a run of them on one job is a test worth fixing. | [Flaky Jobs and Platform Faults on Failed Jobs](#flaky-jobs-and-platform-faults-on-failed-jobs) |
| `MemoryLimitRaisedForRetry` | Normal | A scale-set worker's runner was killed at its own memory limit, and the `Binpack` or `Throughput` profile gives the job's next attempt a higher one, so the run is re-run as `cause="oom"` once GitHub concludes it, waiting up to 24 hours (Q1183, Q1244). Names the job, the run and both limits. One per job: a second kill is withheld. A rising rate is jobs outgrowing their history, or a change that grew one. | [When a job runs out of memory](worker-rightsizing.md#when-a-job-runs-out-of-memory) |
| `AutoRetryPaused` | Warning | More than 10 distinct runs on this set were re-run after a runner was killed for memory within 10 minutes, so out-of-memory retries are paused for 30 (Q1183). That many at once points at a shared cause, such as a node packed past its memory or a dependency that bloated every job, rather than each job's own growth. Each kill in the pause is withheld as `reason="breaker_open"`, and `actions_gateway_auto_retry_paused{cause="oom"}` reads 1 until the pause ends (Q1245). | [When a job runs out of memory](worker-rightsizing.md#when-a-job-runs-out-of-memory) |
| `PlatformFaultOnFailedJob` | Warning | A scale-set job failed and its worker shows a platform fault: the pod `Failed`, a `DisruptionTarget` condition, a container `OOMKilled`, the runner container exiting non-zero, or the set's colocated EgressProxy not `Ready` or with no ready replica (Q555, Q1237). Names the run, the job, the worker and the fault. | [Flaky Jobs and Platform Faults on Failed Jobs](#flaky-jobs-and-platform-faults-on-failed-jobs) |
| `JobProvisionStalled` | Warning | A scale-set job cannot register its runner name (`generate-jitconfig` 409 that no retry cleared), so no worker can be created for it. The job is held and re-offered on a backoff. Also sets the advisory `JobProvisionStalled` condition, whose message names the job ids. Once per episode. | [Scale-Set Job Stranded by a Stale Runner Record](#scale-set-job-stranded-by-a-stale-runner-record-runner-name-409) |
| `WorkerTerminating` | Normal | A scale-set job replayed to a re-created session is waiting for its previous worker pod to finish terminating; it is re-offered on a backoff until that pod is gone. Also sets the advisory `JobProvisionStalled` condition, whose message names the job ids. Once per episode. | [Scale-Set Job Waiting for Its Previous Worker to Terminate](#scale-set-job-waiting-for-its-previous-worker-to-terminate-workerterminating) |
| `JobSourceUnverified` | Warning | A pull request job on a set refusing fork pull requests is held because its run could not be read to check for a fork; it is re-read on a backoff and never run unchecked. Also sets the advisory `JobProvisionStalled` condition, whose message names the job ids. Once per episode. | [Scale-Set Job Held Because Its Run Could Not Be Read](#scale-set-job-held-because-its-run-could-not-be-read-jobsourceunverified) |
| `ForkJobRefused` | Warning | A fork pull request's job was refused because the set does not set `spec.forkPullRequests: Accept`: no runner was minted and its run was force-cancelled. Names the head repository and the run. | [Fork Pull Request Job Refused](#fork-pull-request-job-refused-forkjobrefused) |
| `ForkRefusalCheckRunSkipped` | Normal | The check run explaining a fork refusal could not be written, usually because the GitHub App lacks `Checks: Read and write`; the refusal still happened. | [Fork Pull Request Job Refused](#fork-pull-request-job-refused-forkjobrefused) |
| `WorkerCeilingReached` | Normal | Scale-set jobs are waiting because the set is already running as many workers as its spec allows; they are re-offered until capacity frees. Expected backpressure, hence Normal. Also sets the advisory `JobProvisionStalled` condition, whose message names the job ids. Once per episode. | [Scale-Set Jobs Waiting at the Worker Ceiling](#scale-set-jobs-waiting-at-the-worker-ceiling-workerceilingreached) |
| `AssignmentAbandoned` | Warning | The listener gave up on assigned jobs it was holding, because the scale set reported no assigned jobs at all on two consecutive readings — GitHub is no longer holding them, and never reported them complete. Each is a workflow run that will not run. Clears `JobProvisionStalled` and steps `…_jobs_abandoned_total`. | [Scale-Set Assignments Abandoned](#scale-set-assignments-abandoned-assignmentabandoned) |
| `OrphanedWorkerRecovered` | Warning | A scale-set worker pod for an unconcluded job was already gone when this AGC process started, so the disruption happened while nothing was watching and its cause was lost with the pod (Q844). The run is re-run through the shared per-run retry budget as `cause="vanished"`. Expected after an AGC restart that overlapped a node drain or a preemption; a steady trickle with no restarts is not. | [Evicted Worker Pods Exhausting Retry Budget](#evicted-worker-pods-exhausting-retry-budget) |
| `WorkersNotStarting` | Warning | One or more worker pods were placed on a node and the kubelet could not start them, sitting in `ImagePullBackOff` on the container image (Q906). Recorded on every `RunnerSet` on the False→True transition, whether or not the set opted into `spec.capacityGate`; the note carries the kubelet's own text, which names the image. Also sets the advisory `WorkersNotStarting` condition and reads `1` on `actions_gateway_runnerset_workers_not_starting`. | [The reason is `PodsNotStarting`: the image will not pull](#the-reason-is-podsnotstarting-the-image-will-not-pull) |
| `EgressDestinationDenied` | Warning | A worker's job was refused a CONNECT because the destination is not on the egress proxy pool's allowlist, attributed to that job's pod through `proxyFailureAttribution: PerJob` (Q1210). The note names the pool, the pod and the host and port. A retry is refused the same way. Once per job and destination, at most eight destinations a job. | [Proxy failure attribution](tenant-onboarding.md#proxy-failure-attribution) |

**Diagnostics.**

```sh
# All AGC-emitted Warning events on one owner, newest last.
kubectl describe runnerset -n <namespace> <name>

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
With no request there is no denominator, so the HPA emits `<unknown>` for the target metric and stops scaling entirely.
`ScalingActive` goes `False` with reason `FailedGetResourceMetric`:

```text
the HPA was unable to compute the replica count: failed to get cpu utilization: unable to get metrics for resource cpu: no metrics returned from resource metrics API
```

A metrics-server that is absent or not yet serving produces the same reason with a different tail (`the server could not find the requested resource (get pods.metrics.k8s.io)`), which is why the diagnostics below check for it.
(Measured on Kubernetes v1.36.1, 2026-08-03: the first message against a pool declaring no `requests.cpu` with metrics-server v0.8.1 serving, the second on the same cluster before metrics-server was installed.)

**Diagnostics.**

```sh
# Check HPA status
kubectl describe hpa -n <namespace> <proxy>-proxy

# Check proxy pod resource requests
kubectl get pod -n <namespace> -l actions-gateway.com/egress-proxy=<proxy> -o jsonpath='{.items[0].spec.containers[0].resources}'

# Check metrics-server is running
kubectl get pods -n kube-system -l k8s-app=metrics-server
```

**Resolution.**

Ensure `spec.resources.requests.cpu` is set to a non-zero value in the `EgressProxy` spec.
The default is `10m`.
If you explicitly set `resources` without including `requests.cpu`, the whole `resources` block is replaced and defaults are lost — set all four sub-fields explicitly:

```yaml
spec:
  resources:
    requests:
      cpu: "10m"
      memory: "32Mi"
    limits:
      cpu: "100m"
      memory: "64Mi"
```

After updating the spec, patch the proxy Deployment or trigger a rollout.
Recovery takes longer than one scrape: the patch recreates the pod, and metrics-server has to observe the new one for a resolution interval before the HPA controller reads it on a sync of its own.
Measured on Kubernetes v1.36.1 with metrics-server v0.8.1 at its shipped `--metric-resolution=15s`, 2026-09-11, patching `requests.cpu` onto a pool that declared none: `ScalingActive` went `True` 37s, 45s and 45s after the patch across three runs.
Budget a minute, and read the `ScalingActive` condition rather than watching `TARGETS`: the condition flips in one step, while `TARGETS` shows `<unknown>` until it does.

**Second likely cause: the namespace `ResourceQuota` won't admit the replicas the HPA wants.** The HPA computes utilization correctly but the proxy Deployment cannot create more pods because the platform-owned namespace `ResourceQuota` is the hard cap.
Under load the pool wedges below its target and the rejected creates surface in two places at once.
The **ReplicaSet** records a `Warning` Event with reason `FailedCreate`, and the **Deployment** carries the same text as `ReplicaFailure=True`, also with reason `FailedCreate`:

```text
Error creating: pods "<proxy>-proxy-<replicaset>-<id>" is forbidden: exceeded quota: <quota-name>, requested: limits.cpu=100m,limits.memory=64Mi,pods=1,requests.cpu=10m,requests.memory=32Mi, used: limits.cpu=200m,limits.memory=128Mi,pods=2,requests.cpu=20m,requests.memory=64Mi, limited: limits.cpu=200m,limits.memory=128Mi,pods=2,requests.cpu=20m,requests.memory=64Mi
```

Match on the reason rather than the message, as with the `<unknown>` causes above: `FailedCreate` is stable API surface and the message names every quota dimension, not only the one that bound.
(Measured on Kubernetes v1.36.1, 2026-09-11, against a namespace `ResourceQuota` capping `pods: 2` and a five-replica proxy Deployment requesting `cpu: 10m`.)

```sh
# The rejected creates, and the same text on the Deployment.
kubectl get events -n <namespace> --field-selector reason=FailedCreate
kubectl get deploy -n <namespace> <proxy>-proxy \
  -o jsonpath='{range .status.conditions[?(@.type=="ReplicaFailure")]}{.reason}: {.message}{"\n"}{end}'
```

The GMC surfaces this as two non-blocking conditions on the `ActionsGateway` (neither gates `Ready` — the pool keeps serving at its current scale), each also exported as a gauge for alerting:

| Condition / metric | Meaning | Action |
|---|---|---|
| `ProxyQuotaPressure` (warning) — `actions_gateway_proxy_quota_pressure` | The pool can't grow to `maxReplicas` within the quota's remaining headroom (`hard − used`). Load-dependent. | Raise the quota or lower `maxReplicas` before the next spike. |
| `ProxyQuotaExceeded` (error) — `actions_gateway_proxy_quota_exceeded` | Replica creates are being **rejected now** (Deployment `ReplicaFailure=True`, reason `FailedCreate`). | Raise the quota now; the pool is degraded below the HPA's target. |

```sh
# Read both conditions (Exceeded supersedes Pressure when firing).
kubectl get actionsgateway -n <namespace> <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="ProxyQuotaPressure")]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
kubectl describe actionsgateway -n <namespace> <name>
```

Resolve by **either** raising the platform-owned quota (`kubectl edit resourcequota -n <namespace> <quota-name>`) to admit the configured `maxReplicas`, **or** lowering the `EgressProxy`'s `spec.maxReplicas` to fit.
Editing the quota's `.spec.hard` re-triggers reconciliation immediately; the conditions clear on the next reconcile.

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
  "https://<proxy>-proxy.$ns.svc:8443/metrics" | \
  grep actions_gateway_proxy_tunnel_duration_seconds_bucket

# Active vs. total tunnels — healthy ratio is "active << total".
curl -s --cert scraper.crt --key scraper.key --cacert metrics-ca.crt \
  "https://<proxy>-proxy.$ns.svc:8443/metrics" | \
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

Both stages share `PROXY_SHUTDOWN_DRAIN_TIMEOUT` (default 45s) — the linger is spent *inside* that budget, not added to it — which fits inside the pod's `terminationGracePeriodSeconds: 60` with headroom.
**Tunnels still open when the deadline expires are force-closed** — the alternative is holding the pod until the kubelet SIGKILLs it, which cuts them anyway and with no log line to show for it.

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
kubectl get deploy -n "$ns" <proxy>-proxy \
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
kubectl set env deploy/<proxy>-proxy -n <namespace> \
  PROXY_SHUTDOWN_LINGER=-1s
```

Setting it to a short positive value (`2s`) rather than disabling it keeps most of the benefit at a fraction of the cost.
Note the GMC owns this Deployment and will reconcile the env away; set it on the `EgressProxy`/`ActionsGateway` spec for a durable change.

> **Prefer keeping proxy pools off spot capacity.** The egress proxy is shared infrastructure for every worker in the tenant, and its IP is the tenant's egress identity: a reclaimed *worker* costs one CI job, which re-runs, while a reclaimed *proxy* cuts live egress for every job routed through it — and on a 15s budget the drain cannot get out of the way cleanly.
> `spec.scheduling` exists to pin the pool; use it to select on-demand nodes, and spend the spot savings on workers instead.
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
   kubectl get secret <proxy>-metrics-client -n "$ns" -o jsonpath='{.data.tls\.crt}' | base64 -d > scraper.crt
   kubectl get secret <proxy>-metrics-client -n "$ns" -o jsonpath='{.data.tls\.key}' | base64 -d > scraper.key
   kubectl get secret <proxy>-metrics-client -n "$ns" -o jsonpath='{.data.ca\.crt}'  | base64 -d > metrics-ca.crt
   curl -s --cert scraper.crt --key scraper.key --cacert metrics-ca.crt \
     "https://<proxy>-proxy.$ns.svc:8443/metrics" | head
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
  A scale set's labels cannot be changed after it is created, so this one is fixed by replacing the scale set rather than by editing it.
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

- **Otherwise, give the set a new scale set.** Labels are fixed when the scale set is created, and the AGC finds the scale set by name.
  There is no in-place fix to wait for: the Actions Service accepts a labels `PATCH`, discards it, and answers 200 (measured 2026-08-24, [Q793](../plan/archive/q793-labels-patch.md)).
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

## RateLimited Condition on a RunnerSet

**Symptoms.** A `RunnerSet` reports `RateLimited=True` with reason `SustainedRateLimit`, and `actions_gateway_message_poll_errors_total{reason="rate_limited"}` is climbing.

**Likely cause.** GitHub has answered the set's message polling with `429` for more than ten minutes.
The listener keeps polling, waiting out a `Retry-After` when the response carries one and its ordinary poll backoff when it does not.

**Diagnostics.**

```sh
# Check the condition
kubectl get runnerset -n <namespace> <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="RateLimited")]}{.status} {.reason}: {.message}{"\n"}{end}'

# Check how far the limiting reaches
# Metric: sum by (namespace) (rate(actions_gateway_message_poll_errors_total{reason="rate_limited"}[5m]))

# Check how many sets poll in this namespace: each holds one long poll
kubectl get runnerset -n <namespace>
```

**Resolution.**
- `RateLimited=True` is advisory, does not gate `Ready`, and needs no AGC restart to clear: it clears to `RateLimited=False` (reason `PollingHealthy`) on the first successful poll.
- If it does not clear, there is no per-set setting to tune: each `RunnerSet` holds one session with one long poll, whatever its job count, so the polling rate follows the number of sets.

---

## GitHub App Secret Misconfiguration

**Symptoms.** AGC logs show errors like `failed to create installation token`, `private key: RSA key parse error`, or `401 Unauthorized`.
The `ActionsGateway` condition `AGCAvailable=False` with reason `CredentialError`.
A `RunnerSet` whose listener cannot start for this reason reports `Ready=False` with reason `NoActiveSessions` and a `ScaleSetListenerStartFailed` Warning Event carrying the error.
A session call GitHub rejects as unauthorized also sets `Degraded=True` (reason `Unauthorized`) and records a `SessionUnauthorized` Warning Event.
`Degraded` clears (`Degraded=False`, reason `SessionAuthorized`) once a session call succeeds.
Read it with:

```sh
kubectl get runnerset -n <namespace> <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="Degraded")]}{.status} {.reason}: {.message}{"\n"}{end}'
```

**Common misconfigurations.**

| Error message | Likely cause |
| --- | --- |
| `private key: RSA key parse error` / `no PEM block found` | PEM key is corrupted — extra whitespace, missing or extra newlines, CRLF line endings, hand-paste damage, or an unsupported block type (e.g. `OPENSSH`/`EC`, which fail with `unsupported PEM block type`). Both PKCS#1 (`-----BEGIN RSA PRIVATE KEY-----`) and PKCS#8 (`-----BEGIN PRIVATE KEY-----`) are accepted, so PKCS#8 is **not** a wrong format. |
| `401 Unauthorized` on token exchange | `appId` or `installationId` is wrong. |
| `404 Not Found` on token exchange | The GitHub App is not installed in the target organization or the `installationId` does not match. |
| `422 Unprocessable Entity` | The App lacks the [onboarding permission set](tenant-onboarding.md#0a-create-the-app-web-ui). |

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
To trigger a rolling update on the AGC Deployment after fixing the Secret, change `spec.credentials.githubApp.name` in the `ActionsGateway` spec to reference the new Secret name (the GMC will roll the AGC Deployment automatically) or manually restart the Deployment:

```sh
kubectl rollout restart deploy/<gateway>-agc -n <namespace>
```

On a GMC older than the Q552 fix that manual restart is a silent no-op — prefer the `spec.credentials.githubApp.name` change, or see [`kubectl rollout restart` of a Managed Deployment Reports Success but Nothing Restarts](#kubectl-rollout-restart-of-a-managed-deployment-reports-success-but-nothing-restarts).

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
kubectl logs -n <namespace> deploy/<gateway>-agc | grep "token refresh"

# Test connectivity to GitHub via the tenant proxy (AGC is distroless — use an
# ephemeral curl pod in the same namespace; it picks up the same NetworkPolicy egress).
kubectl run nettest-$$ -n <namespace> --rm -it --restart=Never \
  --image=curlimages/curl:latest \
  --overrides='{"spec":{"automountServiceAccountToken":false,"containers":[{"name":"c","image":"curlimages/curl:latest","command":["sh","-c","curl -x https://<proxy>-proxy:8080 -sI https://api.github.com/app"]}]}}'
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

## Network Connectivity Failures

**Symptoms.** The AGC cannot reach GitHub through the proxy.
Logs show `connection refused`, `dial tcp: i/o timeout`, or `proxy: no response from proxy`.

**Likely causes.**
- The proxy pod is not running or not ready.
- `HTTP_PROXY`/`HTTPS_PROXY` environment variables are incorrect (wrong Service name or port).
- `<gateway>-workload` NetworkPolicy is blocking the AGC-to-proxy egress path (e.g. proxy ClusterIP changed after a recreate and the rule wasn't reconciled).
- `<proxy>-proxy` NetworkPolicy is blocking the proxy's egress to GitHub (IP ranges stale or `managedNetworkPolicy: false` with no replacement rule).
- `<gateway>-agc` NetworkPolicy is missing — AGC can't reach the K8s API server, so token refresh and webhook health checks fail before any GitHub traffic.

**Diagnostics.**

```sh
# Check proxy pod status
kubectl get pods -n <namespace> -l actions-gateway.com/egress-proxy=<proxy>

# Verify the proxy Service exists and has endpoints
kubectl get svc -n <namespace> <proxy>-proxy
kubectl get endpoints -n <namespace> <proxy>-proxy

# Check the AGC container's HTTPS_PROXY env var (distroless — inspect spec, not the running process)
kubectl get pod -n <namespace> -l app.kubernetes.io/name=actions-gateway-controller \
  -o jsonpath='{range .items[0].spec.containers[?(@.name=="agc")].env[?(@.name=="HTTPS_PROXY")]}{.name}={.value}{"\n"}{end}'

# Test proxy connectivity using an ephemeral curl pod in the same namespace
kubectl run nettest-$$ -n <namespace> --rm -it --restart=Never \
  --image=curlimages/curl:latest \
  --overrides='{"spec":{"automountServiceAccountToken":false,"containers":[{"name":"c","image":"curlimages/curl:latest","command":["sh","-c","curl -v -x https://<proxy>-proxy:8080 https://api.github.com 2>&1 | head -20"]}]}}'

# Check NetworkPolicy rules — there are three: workload, agc, proxy
kubectl get networkpolicy -n <namespace>
# Expected: <gateway>-workload, <gateway>-agc, <proxy>-proxy
kubectl describe networkpolicy -n <namespace>

# Check the IP range refresh metric
# Metric: actions_gateway_ip_range_updates_total{namespace="<namespace>"}
```

**Resolution.**
- If the proxy pod is down: check its logs and restart if necessary.
- If the `NetworkPolicy` egress rules are stale: trigger a manual refresh by temporarily setting the `EgressProxy`'s `spec.managedNetworkPolicy: false` and back to `true`, or wait for the 24-hour automatic refresh cycle.
  Check the GitHub meta API for current IP ranges: `curl https://api.github.com/meta | jq .actions`.
- If an internal destination is being sent through the proxy: add it to the `EgressProxy`'s `spec.noProxyCIDRs`.
  The cluster-internal exemptions — including the API server — are appended automatically, so a missing service CIDR is not the cause; see [AGC Crash-Loops Dialling the API Server Through the Egress Proxy](#agc-crash-loops-dialling-the-api-server-through-the-egress-proxy) and the `noProxyCIDRs` field documentation in [§3.1](../design/03-api-contracts.md#31-kubernetes-crd-schemas).

---

## AGC Crash-Loops Dialling the API Server Through the Egress Proxy

**Symptoms.** A **proxied** tenant's AGC never reaches Ready and `CrashLoopBackOff`s at startup.
The log shows a Kubernetes API call — not a GitHub call — failing at the proxy, addressed to an **IP**, not a hostname:

```
detect actions-gateway.com/v2 RunnerSet CRD: … Get "https://34.118.224.1:443/api":
proxyconnect tcp: tls: failed to verify certificate:
x509: certificate signed by unknown authority
```

The destination is the API server's ClusterIP rather than `api.github.com`, and the AGC dies during startup.
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
kubectl get deploy -n <namespace> -l app.kubernetes.io/name=actions-gateway-controller \
  -o jsonpath='{range .items[0].spec.template.spec.containers[0].env[?(@.name=="NO_PROXY")]}{.value}{"\n"}{end}'
```

**Resolution.**

- **Upgrade the GMC** to a release carrying the Q465 fix and let the tenant's AGC Deployment be re-reconciled (`kubectl rollout restart deploy -n <namespace> <agc>` if it does not roll on its own).
  The exemption then follows the cluster.
- **Workaround on an older GMC**: name the API server's ClusterIP in the `EgressProxy`'s `spec.noProxyCIDRs` — the single address from step 1, or that cluster's Service CIDR if you prefer (`kubectl cluster-info dump | grep -m1 service-cluster-ip-range`).
  Prefer the single address: every entry bypasses the tenant's egress proxy, and a CIDR exempts hosts you did not name.
- If `NO_PROXY` shows the literal `$(KUBERNETES_SERVICE_HOST)` unexpanded inside the **running container**, the GMC was running outside the cluster when it composed the Deployment and the kubelet did not expand it.
  Run the GMC in-cluster (the supported deployment) and re-reconcile.

---

## AGC Cannot Reach the Kubernetes API Server (NetworkPolicy + post-DNAT port mismatch)

**Symptoms.** AGC logs show `dial tcp 10.96.0.1:443: i/o timeout` (or similar) when calling the K8s API server.
The `<gateway>-agc` NetworkPolicy *appears* to allow port 443, yet the connection is silently dropped.
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
kubectl get networkpolicy -n <namespace> <gateway>-agc -o yaml \
  | yq '.spec.egress[].ports[].port' | sort -u

# 3. If the cluster uses kindnet / kube-network-policies, check the verdict log
#    on the node hosting the AGC pod. Look for lines like:
#      "Pod is not allowed to connect to port" pod="<ns>/<agc-pod>" port=6443
kubectl get pod -n <namespace> -l app.kubernetes.io/name=actions-gateway-controller \
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

From a pod in the tenant namespace (which the GMC egress `NetworkPolicy` governs) DNS times out, while the *same* lookup from a pod in a namespace with no GAG policy (e.g.
`default`) succeeds — so cluster DNS is healthy and the egress policy is the cause.
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
This is a strict, minimal widening (still cluster DNS only, port 53 only — it preserves the DNS-exfiltration containment of [§ Security](../design/05-security.md)) and is harmless on clusters without NodeLocal DNSCache, where the selector simply matches no pod.
**Re-validated live (2026-07-07)** on a fresh GKE Standard / Dataplane V2 cluster with NodeLocal DNSCache: from a pod governed by the GMC egress `NetworkPolicy`, `nslookup github.com` resolves through the `node-local-dns` peer while non-DNS non-allowlisted egress stays blocked.
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

If you are pinned to an older GAG build and cannot upgrade immediately, the same effect can be obtained by setting the `EgressProxy`'s `spec.managedNetworkPolicy: false` and supplying your own egress policy that allows port-53 to `node-local-dns` — but prefer upgrading, since a hand-managed policy must then track GitHub IP-range rotation itself.

---

## Worker Pod Runner.Worker Fails TLS Handshake With UntrustedRoot

**Symptoms.** Worker pod logs (look at the `runner` container) contain repeated lines like:

```
System.Security.Authentication.AuthenticationException: The remote certificate is invalid because of errors in the certificate chain: UntrustedRoot
```

emitted from `JobExtension` connectivity checks, `ResultServer` init, `JobServerQueue` log uploads, the `GitHubActionsService` log-blob fetch, or `RunServer.CompleteJobAsync`.
The runner retries for ~3 minutes, then exits 1.

**Cause.** Runner.Worker's .NET HttpClient is validating the egress proxy's TLS cert and the worker pod's trust store does not include the GMC-issued self-signed proxy certificate.
This is the worker-side mirror of the AGC's proxy-CA pinning ([§5.2](../design/05-security.md) "Cross-Tenant Proxy CA Trust"): the AGC mounts the CA explicitly so its outbound HTTPS works; worker pods must do the same.

The AGC's pod provisioner is supposed to project the `<proxy>-proxy-tls` Secret into every worker pod at `/etc/actions-gateway/proxy-ca/tls.crt` and set `PROXY_CA_CERT_PATH` so the worker entrypoint wrapper builds a combined `SSL_CERT_FILE` bundle before exec'ing `Runner.Worker`.
UntrustedRoot means one of those steps did not happen.

**Diagnostics.**

```sh
# 1. Inspect a failed worker pod's spec — the Secret volume must exist.
kubectl get pod -n <namespace> <worker-pod-name> -o yaml \
  | yq '.spec.volumes[] | select(.name=="proxy-ca")'
# Expected: a Secret volume with secretName: <proxy>-proxy-tls and Items: [{key: tls.crt, path: tls.crt}]
# If empty: the AGC was deployed without PROXY_TLS_SECRET_NAME.

# 2. Confirm the AGC has the PROXY_TLS_SECRET_NAME env wired.
kubectl get pod -n <namespace> -l app.kubernetes.io/name=actions-gateway-controller \
  -o jsonpath='{range .items[0].spec.containers[?(@.name=="agc")].env[?(@.name=="PROXY_TLS_SECRET_NAME")]}{.name}={.value}{"\n"}{end}'
# Expected: PROXY_TLS_SECRET_NAME=<proxy>-proxy-tls
# Empty means the GMC needs to roll the AGC Deployment (likely an upgrade across the 5h boundary).

# 3. Confirm the worker container's PROXY_CA_CERT_PATH env.
kubectl get pod -n <namespace> <worker-pod-name> -o yaml \
  | yq '.spec.containers[] | select(.name=="runner") | .env[] | select(.name=="PROXY_CA_CERT_PATH")'

# 4. Confirm the proxy TLS Secret exists and contains tls.crt.
kubectl get secret -n <namespace> <proxy>-proxy-tls \
  -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl x509 -noout -subject -issuer -dates
```

**Resolution.**
- If the worker pod has no `proxy-ca` volume: ensure the AGC was started with `PROXY_TLS_SECRET_NAME=<proxy>-proxy-tls` (the GMC injects this automatically — if it's missing, the GMC needs to roll the AGC Deployment, e.g. by bumping `ag.Spec` or restarting the GMC).
- If the volume is present but the wrapper logs nothing about `proxy CA trust installed`: check that `PROXY_CA_CERT_PATH` is set on the runner container and the mounted file is non-empty.
  An empty/missing file is tolerated as a no-op, which silently leaves the runner with no proxy trust — the wrapper log line `no proxy CA cert mounted; skipping trust-store install` distinguishes this case from a wrapper that ran the install successfully.
- If the proxy TLS Secret is missing or the cert has expired: the GMC's cert-manager integration ([§2.1](../design/02-architecture.md#21-tier-1--gateway-manager-controller-gmc) "Proxy Deployer") owns rotation; check the GMC's logs for issuer errors.
  As a fallback, deleting the Secret triggers reissuance.
- If the issue persists after the volume and env are correct: confirm the proxy pod is presenting the cert signed by the CA in the Secret — `kubectl exec` into a curl pod in the same namespace and run `openssl s_client -connect <proxy>-proxy:8080 -showcerts </dev/null` to inspect what the proxy actually serves.

---

## Which Disruptions Auto-Re-Run a Job (and Which Never Do)

<!-- A marketing-toned summary of this matrix lives on docs/why-gag.md ("What
     re-runs itself" box). When a row here changes, update that box too. -->

The consolidated boundary for the automatic re-run machinery the sections below troubleshoot individually.
Every firing case spends one slot of the run's shared `maxEvictionRetries` budget (default 2, max 10).
One more cause, `abandoned`, sits outside this table because it does not re-run directly: the job never ran, so its run is force-cancelled first and re-run only once capacity returns.
It is covered in [Worker Pod Reaped While Pending](#worker-pod-reaped-while-pending-workerpodstuckpending).

**Fires — the interrupted run is re-run via `rerun-failed-jobs`:**

| Disruption | <span class="gag-nowrap">`cause` label</span> | Detected by |
|---|---|---|
| Kubelet eviction (node pressure) | <code class="gag-nowrap">eviction</code> | pod `Failed` with `reason: Evicted` (the kubelet also sets a `DisruptionTarget` condition, reason `TerminationByKubelet`, but the arm does not read it) |
| Scheduler preemption (a preempting `priorityTiers` floor) | <code class="gag-nowrap">preemption</code> | `DisruptionTarget` condition, reason `PreemptionByScheduler` |
| Node shutdown under a running worker: a Spot or preemptible reclamation the kubelet shuts down gracefully, a Node object deleted under it, or a dead node you taint `node.kubernetes.io/out-of-service` | <code class="gag-nowrap">node_shutdown</code> | pod `Failed` with a `DisruptionTarget` condition, reason `TerminationByKubelet` (the kubelet's graceful shutdown), `DeletionByPodGC` (the node was gone first), or `DeletionByTaintManager` with the runner container still recorded `Running` (the out-of-service taint), on a worker one of whose containers started |
| Node drain of a running worker | <code class="gag-nowrap">deletion</code> | terminal phase published while the pod carries a `deletionTimestamp` |
| Bare `kubectl delete pod` of a running worker | <code class="gag-nowrap">deletion</code> | same mark as a drain — indistinguishable, by design |
| A runner killed at its own memory limit on a `Binpack` or `Throughput` set, re-run with a higher limit | <code class="gag-nowrap">oom</code> | pod `Failed` with the runner container terminated `OOMKilled`, on a worker the sizing profile built; see [When a job runs out of memory](worker-rightsizing.md#when-a-job-runs-out-of-memory) |
| Any of the above that happened while the AGC was down | <code class="gag-nowrap">vanished</code> | a persisted in-flight record whose worker is no longer there: no pod's runner started that job, and the pod created for it is gone, or is running another job while no other worker that has not exited is idle to take it |

**Never fires, by design:**

- **A genuine job failure.** The job ran and failed on its own merits; nothing was disrupted, and re-running it would mask real breakage.
- **A worker evicted for its own limit.** The kubelet writes `Failed` / `Evicted` too when a pod exceeds its own `ephemeral-storage` limit, one container's limit, or an `emptyDir` `sizeLimit`, but with a limit message and no `DisruptionTarget` condition.
  The job outgrew a limit its template sets, so a re-run gets the same limit and fails the same way.
  No slot is spent: `eviction_rerun_withheld_total{cause="eviction", reason="own_limit"}` increments, and a Normal `EvictionRerunWithheld` Event quotes the kubelet's message.
  Raise the limit, or reduce what the job writes, then re-run it by hand (Q1177).
- **A runner killed for memory that a re-run would repeat.** Without a `Binpack` or `Throughput` profile, at the profile's memory ceiling, or a second kill after a retry already raised the job's limit, a re-run gets the memory that killed it.
  No slot is spent: `eviction_rerun_withheld_total{cause="oom"}` increments with `reason="own_limit"` or `"same_class_again"`, and a Normal `EvictionRerunWithheld` Event names the fix (Q1183).
- **A cancelled run.** A cancel is the intended stop — it is the supported way to end a job without triggering recovery (delete the worker pod instead and the job *is* re-run; see [Cancelling a Run Does Not Stop Its Worker Pod](#cancelling-a-run-does-not-stop-its-worker-pod)).
- **A node that goes `NotReady` and keeps its Node object, untainted.** The taint manager stamps `DeletionByTaintManager` and deletes the worker gracefully without failing it, and the kubelet cannot finish that delete while the node stays unreachable, so the pod stays `Running` and no arm fires.
  GAG does not force it, because a node that is only partitioned may still be running the job.
  Once you have confirmed the node is dead, taint it with `kubectl taint node <node> node.kubernetes.io/out-of-service=nodeshutdown:NoExecute`; PodGC then fails and removes the pod, and the `node_shutdown` row above re-runs it.
  Deleting the Node object works too: PodGC then fails the pod with `DeletionByPodGC`.
  A `NoExecute` taint of your own on a live node is the drain case instead, recovered as `deletion`.
- **The AGC's own reaper deletions** — the lifetime cap (`maxWorkerLifetime`), the stuck-`Running` reap deadline, orphan cleanup.
  Each is stamped `actions-gateway.com/deletion-reason` before deletion and excluded: the gateway just judged that job stuck, so a re-run would loop it.
  The stamp is the pod's, so it does not survive the pod: a reaped worker whose job completion a previous AGC process never read is picked up by the `vanished` row above instead, which needs the completion to have gone unread for `completedPodTTL` and then a kill inside that state.
- **A worker whose container never ran** does not fire *this* recovery.
  A drain catching a still-`Pending` pod, the `pendingPodDeadline` reap, or a node shutdown while the worker was still pulling its image leaves no reportable failure for `rerun-failed-jobs` to act on.
  It is not lost, though: the run is force-cancelled and then re-run once capacity returns, which is the out-of-table `abandoned` cause named above.
  One case does take the `vanished` row instead: a never-started worker removed while the AGC was down, because whether a container ever ran is a fact about the pod.
  GitHub refuses a re-run for a run with no failed job, which is logged and dropped, so the outcome is a manual re-run rather than a wrong one.
  See [Worker Pod Reaped While Pending](#worker-pod-reaped-while-pending-workerpodstuckpending).

**Fires but can come up short** — each has its own runbook below:

- The shared budget exhausts (`eviction_retries_exhausted_total`, the `EvictionRetriesExhausted` event): [Evicted Worker Pods Exhausting Retry Budget](#evicted-worker-pods-exhausting-retry-budget).
- The run identity is missing (`eviction_recovery_identity_unknown_total`): [Evicted Scale-Set Jobs Are Not Re-Run Automatically](#evicted-scale-set-jobs-are-not-re-run-automatically).
- A scale-set worker died before GitHub delivered its job — nothing to re-run: ["Runner Lost Communication" and No Worker Pod Was Ever Created](#runner-lost-communication-and-no-worker-pod-was-ever-created).
- GitHub refuses past the re-run window (`EvictionRerunFailed`): [Evicted Worker Pods Exhausting Retry Budget](#evicted-worker-pods-exhausting-retry-budget).

**Fires and deliberately stands down**, and not a fault: the run had already concluded `cancelled`, so re-running it would undo a human's or a concurrency group's cancel.
That is checked on every cause, and GAG's own force-cancel of an abandoned run is told apart and still re-run.
No call is made, `eviction_rerun_withheld_total{reason="run_cancelled"}` increments, and a Normal `EvictionRerunWithheld` Event names the run (Q811, Q1181).
See [Cancelling a Run Does Not Stop Its Worker Pod](#cancelling-a-run-does-not-stop-its-worker-pod).

**Fires and is withheld as futile**, before any budget slot is spent (Q1181): `reason="attempt_limit"` (the run is past GitHub's 50 re-runs), `reason="run_too_old"` (created more than 30 days ago), or `reason="superseded"` (a newer commit's run of the same workflow, event and branch has started).
The `EvictionRerunWithheld` Event names the reason; for the first two, start a new run, and for the third nothing is needed.

A quota-blocked job is deliberately absent from this table: it is **never claimed** in the first place, so it stays queued at GitHub with no re-run needed — see [Jobs Failing Due to Namespace ResourceQuota Exhaustion](#jobs-failing-due-to-namespace-resourcequota-exhaustion).

---

## Flaky Jobs and Platform Faults on Failed Jobs

> Detection reads `JobCompleted` messages, is on for every `RunnerSet` with no field, and changes no behaviour.

When a scale-set job completes `failed`, the AGC records it in the set's failure ledger, ConfigMap `scaleset-failures-<runnerset>`, key `failures.json`, under `failures["<runID>/<job name>"]`.
The record holds the worker's state at its terminal transition (`context`): pod phase, reason and a truncated message, its conditions, the node and pod IP, and each container's termination reason, exit code and restart count.
A `canceled` completion is recorded only when a disruption recovery claimed the job, so a user's cancel records nothing.

When a later attempt of the same job (same run, same job name) completes, it is paired with that record and counted in `actions_gateway_scaleset_job_reruns_total{trigger}`:

| `trigger` | Means | Flaky? |
|---|---|---|
| `gag` | The job was in GAG's own disruption re-run. Its result also counts in `actions_gateway_eviction_rerun_outcomes_total{cause, result}`. | No: a recovery |
| `gag_collateral` | GAG's `rerun-failed-jobs` re-ran it beside a disrupted job | Yes, if it succeeded and no disruption was recorded for its failure |
| `other` | A human or another automation re-ran it | As above |

A flaky verdict counts `actions_gateway_scaleset_flaky_jobs_total` and emits a Normal `FlakyJobDetected` Event.

**Read a failure's context** from the AGC log, which carries it on every failed job, at Warn when it shows a platform fault and Info otherwise:

```bash
kubectl logs -n <agc-namespace> deploy/<agc-deployment> | grep 'scale-set job failed'
```

or from the ledger while the record is open (7 days, or until the job succeeds):

```bash
kubectl get configmap -n <namespace> scaleset-failures-<runnerset> -o jsonpath='{.data.failures\.json}'
```

**A `PlatformFaultOnFailedJob` Warning** names the fault.
`OOMKilled` means the job needs more memory than the worker template gives it; a `DisruptionTarget` condition or a `Failed` pod means the node or the scheduler removed the worker, which the [disruption arms](#which-disruptions-auto-re-run-a-job-and-which-never-do) recover on their own.
`EgressProxy <name> has no ready replicas` means no proxy pod could carry the job's egress when the worker finished.
`EgressProxy <name> not Ready`, with its ready count, means the pool had fewer ready pods than its floor (`spec.minReplicas`, or the Deployment's replicas under bring-your-own autoscaling), as during a rolling update or after an eviction, so the remaining pods may still have carried the job's egress.
Read the pool with `kubectl get egressproxy -n <namespace> <name>`.

**What detection cannot see.** Node conditions are not recorded (the AGC has no `nodes` read), nor is a registry mirror's readiness.
A proxy pool shared from another namespace is recorded as `proxy.mode: shared` with no readiness, since the AGC's cache does not reach that namespace.
A snapshot is held in memory, so a failure recorded after an AGC restart, or after more than 1,024 workers finished with no completion yet, has no `context`.
A re-run more than 7 days after the failure, or one that lands on another `RunnerSet`, pairs with nothing and counts nothing.

---

## Evicted Worker Pods Exhausting Retry Budget

> **Each `RunnerSet` has its own budget.** `maxEvictionRetries` caps GAG's re-runs per run across every disruption cause.
> A `RunnerSet` counts them in its `scaleset-failures-<set>` ConfigMap, so an AGC restart does not refill the budget, and several disrupted workers of one run attempt share one re-run rather than each spending a slot (Q1182).
> A run whose workers span two sets has a budget in each, so it can be re-run up to `maxEvictionRetries` times per set.
> A human's re-runs never count.
> If the counters are flat at **zero** rather than exhausted, the recovery is not firing at all: see [Evicted Scale-Set Jobs Are Not Re-Run Automatically](#evicted-scale-set-jobs-are-not-re-run-automatically) below.
>
> **`eviction_retries_total` incrementing but the job still never re-runs?** An evicted run's re-run is deliberately slow to land: a kubelet eviction SIGKILLs the runner before it can report, so GitHub does not conclude the run until the job lock's TTL lapses — measured at 9m36–9m45s when the runner's report does not escape (Q396) — and until then it refuses `rerun-failed-jobs` with `403 This workflow is already running`.
> The AGC retries that refusal every 30 seconds inside a 15-minute re-run window (Q503), so expect `disruption auto-retry triggered` in the AGC log **~10 minutes** after the eviction, not seconds, with `rerunCalls` in the tens — that attribute counts the calls the recovery made.
> A single-digit count is not a fault: it means GitHub had already concluded the run before the recovery started calling — which is what happens when the disrupted runner got its own report out, as a drain or a preemption lets it (15–26s, Q459).
> One eviction has been seen concluding that fast too (2026-08-03, 17s), but the run could not confirm which worker it disrupted, so do not read a low count after an eviction as proof the runner reported.
> A recovery that gave up instead logs `disruption auto-retry failed`, increments `actions_gateway_eviction_rerun_failures_total`, and emits an `EvictionRerunFailed` Warning Event naming the run — that job needs a manual re-run.
> `reason="run_never_concluded"` means the original run outlived the 15-minute window (check whether GitHub still shows it in progress); with `cause="oom"` the window is 24 hours instead, because that class waits for the run to conclude before calling (Q1244), and it also counts a run the AGC could not read for the half hour after that, which the Event and the AGC log line say ("not seen to conclude") with the read's error (Q1254); `reason="api_error"` means the API failed outright — check the AGC log line's error for a permissions 403 or an endpoint problem.
> `reason="conclusion_unknown"` is the deletion arm only: its cancel check (Q811) spent the whole window unable to reach the run (the request never completing, or a 5xx), so no re-run was fired rather than one fired blind against a possible cancel.
> A 4xx or an undecodable 2xx on that same check lands in `api_error` instead, and fails immediately rather than at the window: neither answer changes inside fifteen minutes, and a 2xx that is not a run means the configured endpoint is not the API.
> Every cause reads the run once before reserving its budget, to learn the attempt the job failed in, and the same two reasons cover that read.
> `reason="budget_unrecorded"` means the budget decision could not be written to `scaleset-failures-<set>`, so the re-run was withheld rather than made without a slot (Q1182).
> The AGC log line `could not record the run's re-run budget decision` carries the cause: a `configmaps` write the tenant `Role` refuses, a ledger at its 512 KiB cap, or `requires writer version`: a newer AGC wrote the ConfigMap with a schema this one may not write, which a rollback produces and which clears once the newer AGC is back.
> Inspect it with `kubectl get configmap -n <namespace> scaleset-failures-<runnerset> -o jsonpath='{.data.failures\.json}'`.
> **A disrupted worker with no re-run and no counter at all** was most likely merged: the AGC log says `a re-run is already reserved for this run attempt`, then `the run was already re-run past the attempt this job failed in`.
> The re-run another worker's disruption reserved re-ran this job too, because GitHub re-runs every failed job of the attempt at once.
> If that re-run never lands (its recovery failed, or the AGC restarted first), the merged worker's own loop re-runs the attempt instead, spending no further slot.
> On an AGC older than the Q503 fix, the single un-retried re-run always lost this race and every evicted job needed a manual re-run.

**Symptoms.** `actions_gateway_eviction_retries_exhausted_total` is incrementing.
Jobs are being cancelled after eviction despite automatic retries.
`kubectl describe` on the owning `RunnerSet` shows a `Warning` event with reason `EvictionRetriesExhausted` (Q170) naming the affected run — the event-based companion to the metric.

**Likely causes.**
- Worker pod keeps being evicted on every attempt (persistent node pressure, OOM loop, or scheduling conflict that prevents the pod from completing).
- The run is being **repeatedly preempted** by a higher `priorityTiers` floor.
  The budget is shared across every cause (see the `cause` label below), so a run alternately evicted and preempted exhausts it twice as fast as either alone.
- `maxEvictionRetries` is set too low for a workload that is disrupted more than twice per run.

**Diagnostics.**

```sh
# Check disruption retry metrics. The cause label splits the disruptions, and the
# retry budget is shared across causes.
# actions_gateway_eviction_retries_total{namespace, runner_group, cause}
# actions_gateway_eviction_retries_exhausted_total{namespace, runner_group, cause}
# actions_gateway_eviction_rerun_failures_total{namespace, runner_group, cause, reason}
# Start here: the cause that is climbing decides which resolution below applies.

# Check recent evicted pods
kubectl get pods -n <namespace> --field-selector=status.phase=Failed | grep Evicted

# Describe an evicted pod for the eviction reason
kubectl describe pod -n <namespace> <evicted-pod-name>
# Look for: "The node was low on resource: memory" or "Preempted by another pod"

# Check node events around the eviction time
kubectl get events -n <namespace> --sort-by='.lastTimestamp' | grep -i evict

# See the budget-exhaustion event on the RunnerSet
kubectl describe runnerset -n <namespace> <name> | grep -A1 EvictionRetriesExhausted
```

**Resolution.**
- `cause="eviction"` climbing — node memory pressure: increase the worker pod's memory requests to discourage the kubelet from evicting it, or investigate the workload's actual memory usage.
- `cause="preemption"` climbing — a higher-priority tier is repeatedly displacing this group: reduce the priority of competing workloads, adjust `priorityTiers` to give this `RunnerSet` a higher floor, or move the work to a tier that is not displaceable.
  A long job in an opportunistic tier is the usual culprit, since every displacement restarts it from the beginning.
- `cause="deletion"` climbing — something outside the gateway is deleting live workers: a node drain (upgrades, autoscaler consolidation), a descheduler, or hand-run `kubectl delete pod`.
  Find the deleter before raising the budget; see [Draining a Worker Auto-Re-Runs the Jobs It Interrupts](#draining-a-worker-auto-re-runs-the-jobs-it-interrupts).
- `cause="node_shutdown"` climbing: workers are landing on nodes that go away under them, usually Spot or preemptible capacity being reclaimed.
  Every reclamation restarts the job from the beginning, so move long jobs onto on-demand nodes with the `RunnerTemplate`'s scheduling constraints.
- If the retry budget is simply too low for a workload that is legitimately disrupted more than twice per run: increase `maxEvictionRetries` on the `RunnerSet` spec (default 2, max 10).
- If the workload is consistently failing (OOM crash, not a disruption): the auto-retry is not appropriate.
  Set `maxEvictionRetries: 0` and investigate the underlying workload issue.

---

## Draining a Worker Auto-Re-Runs the Jobs It Interrupts

> **Preemption has its own runbook** — [A Preempted Worker's Job Is Not Re-Run](#a-preempted-workers-job-is-not-re-run) below.

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
- **A cancelled run is never re-run.** The gateway's own reap of its pod carries the `actions-gateway.com/deletion-reason` stamp above, and recovery excludes it.
- **A drain whose pod goes before the AGC writes to it is still recovered** (Q1108).
  The kubelet removes a drained worker once the container's exit is published, which used to end the recovery, because the at-most-once claim was an annotation on that pod.
  The claim now lives in a per-`RunnerSet` `scaleset-recovery-claims-<name>` ConfigMap the AGC writes before it calls GitHub, so it outlives the pod and the run is re-run normally.
  Nothing is asked of you, and nothing new appears in the metrics for it.
- **What is still reported as unrecoverable is a claim the AGC could not record at all.** If the write to that ConfigMap fails — the tenant `Role` short the `configmaps` grant, an apiserver the AGC cannot reach, or a ConfigMap holding unparseable data — `actions_gateway_eviction_recovery_evidence_lost_total{cause="deletion"}` increments and an `EvictionRecoveryEvidenceLost` Warning Event is recorded on the `RunnerSet`.
  Those runs need a manual re-run.
  The metric is the same one a lost pod used to produce and it still means the same thing to act on; what changed is the cause behind it, so check the AGC's permissions and the ConfigMap rather than its responsiveness:

  ```sh
  kubectl get configmap -n <namespace> scaleset-recovery-claims-<runnerset> -o yaml
  kubectl auth can-i update configmaps -n <namespace> \
    --as=system:serviceaccount:<namespace>:<agc-serviceaccount>
  ```

  A ConfigMap the AGC cannot parse is repaired by deleting it — it holds claims, not work, so deleting it re-opens the at-most-once window for whatever is in flight at that instant and nothing else.
- **On an AGC before Q1029, a smaller fraction was not reported by any metric.** The recovery scan read the worker pods once per reconcile, so a drained worker was judged only if a reconcile began between the kubelet publishing the terminal phase and removing the object, and a reconcile already in flight held the next one past that window.
  If none did, the pod was never seen: no claim, no metric, no Event, and no log line naming it.
  Since Q1029 the worker-pod watch hands the pod to recovery as its phase change arrives, so a drain is judged whether or not a reconcile turns over; the per-reconcile scan remains for pods no event reached.
  On an older AGC the tell is the absence: an unrecovered drain whose pod appears nowhere in the AGC log, against a recovered one that appears twice.
  **That reading needs `spec.logLevel: debug` on the ActionsGateway.** The judge logs the terminating workers it saw and *declined* at `Debug`, and the default is `info` ([observability-logging.md](observability-logging.md)), so at the default a declined pod is silent too and the absence no longer separates the two.
  The remedy is the upgrade; until then, the AGC's reconcile loop is not turning over fast enough.
- **The first re-run call may be refused.** GitHub's conclusion on this path takes 15–26s while `evictionRetryDelay` defaults to 5s, so the first `rerun-failed-jobs` can land while the run is still in progress and be answered `403 This workflow is already running`.
  The Q503 retry loop absorbs that — the re-run is retried on a 30s pace until accepted — so no action is needed; see [Evicted Worker Pods Exhausting Retry Budget](#evicted-worker-pods-exhausting-retry-budget) for the loop's own failure modes.

Two related things operators reasonably expect to change this, and which do not:

- The worker pod's `cluster-autoscaler.kubernetes.io/safe-to-evict: false`, `karpenter.sh/do-not-disrupt: true`, and descheduler prefer-no-eviction annotations are advisory to **those controllers only**.
  `kubectl drain` and kube-scheduler preemption both ignore all three, by design — they exist to stop an autoscaler or descheduler from moving a mid-job worker, not to stop an administrator or the scheduler.
- Worker pods carry no PodDisruptionBudget, so nothing rejects the eviction either.

**Diagnostics.**

```sh
# Which disruption was it? A kubelet eviction leaves the pod behind in Failed/Evicted;
# a preemption records Preempted on the victim; a drain shows up as the eviction API's
# events and, briefly, a Failed pod with a deletionTimestamp.
kubectl get events -n <namespace> --sort-by='.lastTimestamp' \
  | grep -E 'Evicted|Preempted|Killing|TaintManagerEviction'

# Whether recovery fired for the window in question. The cause label separates the
# recovered disruptions (eviction, preemption, node_shutdown, deletion):
# actions_gateway_eviction_retries_total{namespace, runner_group, cause}
# Flat across a drain of RUNNING workers is unexpected — check the AGC log below.
# Flat across a drain that only caught Pending workers is correct.

# Confirm the runner did get its chance to report before the pod went away.
kubectl logs -n <namespace> <worker-pod> --previous \
  | grep -E 'forwarding termination signal|outlived the shutdown grace period'

# The AGC logs the decision explicitly, with the cause.
kubectl logs -n <namespace> deploy/<agc-deployment> \
  | grep -E 'worker pod disrupted; scheduling auto-retry|disruption auto-retry'

# Whether a disruption was found and then dropped because its claim
# could not be recorded durably. Names the pod, so it maps to the run that needs a
# manual re-run.
kubectl logs -n <namespace> deploy/<agc-deployment> \
  | grep 'could not be claimed durably'

# Every verdict the scan reached, by pod. Needs spec.logLevel:
# debug, because two of the six verdicts log at Debug: at the default info a pod
# the scan judged and declined is as silent as one it never saw.
kubectl logs -n <namespace> deploy/<agc-deployment> \
  | grep -E 'disrupt' | grep '<worker-pod>'
```

If a drain of running workers produced no re-run, check whether the pods carried the `actions-gateway.com/deletion-reason` stamp (then the AGC deleted them, not your drain), whether the run's retry budget was already spent (`eviction_retries_exhausted_total`), and whether the AGC was down across the teardown window, could not record its claim (`eviction_recovery_evidence_lost_total`), or the pods carried no run identity (see [A Preempted Worker's Job Is Not Re-Run](#a-preempted-workers-job-is-not-re-run), whose failure modes apply to drains identically).

**Resolution / how to drain safely.**
- **Prefer a quiet window anyway.** The re-run restarts each interrupted job from the beginning, so a drain mid-job still costs the work done so far — and each interrupted run spends re-run budget.
  `kubectl get pods -n <namespace> -l app.kubernetes.io/managed-by=actions-gateway-controller` on the target node shows what a drain would interrupt; an empty result is the free moment.
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

**Symptoms.** A higher-priority `priorityTiers` tier displaces an opportunistic worker, and the displaced run stays failed: `actions_gateway_eviction_retries_total{cause="preemption"}` does not move, and no `rerun-failed-jobs` call is made.
Expected behaviour is one automatic re-run per preempted run.

**Cause.** Work through these in order:

1. **The victim was not actually preempted.** A `kubectl drain`, a manual delete, or a descheduler eviction looks similar but is recovered under `cause="deletion"` instead (see the previous runbook) — and only if the worker reached a terminal phase before the object went away.
   Check which cause, if any, actually moved.
2. **The retry budget is spent.** `maxEvictionRetries` (default 2) is a hard lifetime cap per workflow run on each `RunnerSet`, shared across every disruption cause: a run already re-run twice for node-pressure evictions has nothing left for a preemption.
   `actions_gateway_eviction_retries_exhausted_total{cause="preemption"}` confirms it, as does a `Warning` event with reason `EvictionRetriesExhausted`.
3. **The worker carried no workflow-run identity.** `actions_gateway_eviction_recovery_identity_unknown_total{cause="preemption"}` increments and a `Warning` event with reason `EvictionRecoveryIdentityUnknown` is recorded on the `RunnerSet`.
   See [Evicted Scale-Set Jobs Are Not Re-Run Automatically](#evicted-scale-set-jobs-are-not-re-run-automatically) — the cause and the fix are the same for both disruptions.
4. **The AGC was down for the victim's whole termination grace period.** The scheduler *deletes* its victim, so unlike an evicted pod — which sits in `Failed` until the reaper takes it — a preempted pod is readable only until its grace period expires (30s by default), and an AGC restarting across that window never sees the marker at all.
   The run is still re-run, but under `cause="vanished"` rather than `cause="preemption"`: the AGC persists the run behind every worker it builds, and on start re-runs any whose pod is no longer there.
   So check `cause="vanished"` before concluding nothing fired — a preemption counter that stays flat while the vanished one moves is this case, not a defect.
5. **The AGC could not record its claim.** Recovery writes an at-most-once claim to the set's `scaleset-recovery-claims-<name>` ConfigMap before it calls GitHub, so that a second reconcile or a second replica cannot spend another slot of the run's budget for one disruption.
   A write it cannot make leaves the disruption unrecovered: `actions_gateway_eviction_recovery_evidence_lost_total{cause="preemption"}` increments and an `EvictionRecoveryEvidenceLost` Warning Event is recorded.
   Check the tenant `Role`'s `configmaps` grant (`get`, `create`, `update`) and whether the ConfigMap holds parseable data; a manual re-run is required for the affected run.
   **A victim removed before the claim landed is not this case and needs nothing** — since Q1108 the claim outlives the pod, so the recovery proceeds off the evidence already read from the informer.

**Diagnostics.**

```sh
# Did recovery fire, and under which cause?
# actions_gateway_eviction_retries_total{namespace, runner_group, cause="preemption"}

# The AGC logs the decision explicitly.
kubectl logs -n <namespace> deploy/<agc-deployment> \
  | grep -E 'worker pod disrupted; scheduling auto-retry|disruption auto-retry|budget exhausted'

# Confirm the AGC was up across the preemption, the one hard limit.
kubectl get pods -n <namespace> -l app.kubernetes.io/name=actions-gateway-controller \
  -o custom-columns=NAME:.metadata.name,RESTARTS:.status.containerStatuses[0].restartCount,START:.status.startTime
```

**Resolution.**
- **Raise `maxEvictionRetries`** on the `RunnerSet` spec (default 2, max 10) if a workload is legitimately displaced more than twice per run.
- **Fix the missing run identity** per the runbook linked above; without it no disruption of any cause can be recovered.
- **Re-run the affected run manually** for anything lost to case 5, and keep AGC restarts (upgrades, node maintenance on the control-plane namespace) out of windows where a floor tier is actively preempting.
  Case 4 needs no manual re-run: the run is recovered under `cause="vanished"` once the AGC is back.

---

## Cancelling a Run Does Not Stop Its Worker Pod

**Symptoms.** You cancel a workflow run in the GitHub UI (or with `gh run cancel`).
GitHub concludes the job `cancelled`, and the worker pod can stay `Running`, keeping its CPU/memory request and its concurrency-ceiling slot, until the reaper deletes it.
Cancelling a runaway job does not free capacity at once.

**Cause: this is current behaviour, not a misconfiguration.** A cancelled run puts a terminal `JobCompleted` on the set's message queue.
That completion stamps `actions-gateway.com/job-completed-at` on the worker, and the reaper deletes a pod still `Running` five minutes later ([WorkerPodOrphanedRunning](#worker-pod-reaped-while-running-workerpodorphanedrunning)).
GitHub concludes a job whose runner is still going on its own ~5-minute cancellation grace (measured against real GitHub on 2026-07-29: 5m02s).

So the worst case is GitHub's cancellation grace plus the reap grace, roughly ten minutes, rather than the job's whole remaining runtime.
Expect `actions_gateway_worker_pods_reaped_total{reason="orphaned_running"}` to move when this happens.

The SIGTERM relay does not shorten this.
The relay fires when the *pod* is terminated (an eviction, a drain, a delete), and until the reap nothing terminates the pod.

Caveats on the ten-minute figure: The `JobCompleted`-on-cancel was measured against real GitHub for a job with **no runner attached** (the queue message appeared ~0.2 s after the cancel); the stamp-and-reap half is proven against a real apiserver.
The composed chain has not been observed end to end for a job with a live worker, so treat ten minutes as the shape of the bound rather than a measured latency.
A worker runs the full runner over its own session, so it may abort sooner on its own; that is unmeasured.

**Diagnostics.**

```sh
# Worker pods still Running for a run you already cancelled. Compare against the
# run's state in GitHub: gh run view <run-id> --json status,conclusion
kubectl get pods -n <namespace> \
  -l app.kubernetes.io/managed-by=actions-gateway-controller \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,AGE:.metadata.creationTimestamp'

# What the runner is doing now.
kubectl logs -n <namespace> <worker-pod> --tail=50
```

**Resolution.**
- **Delete the worker pod to reclaim the capacity.** `kubectl delete pod -n <namespace> <worker-pod>` terminates it gracefully: the wrapper relays SIGTERM and the runner stops.
  The cancel stands, and the job is **not** re-queued.

  > **Why the delete does not undo the cancel.** A hand-deleted worker is the shape graceful-deletion recovery acts on (an external delete ordered before the container's exit), so the AGC would otherwise ask GitHub to re-run the run's failed jobs, and GitHub honours that for a `cancelled` conclusion, where a `success` conclusion refuses it with a 403 (measured live 2026-08-05, [the Q683 measurement](../plan/q645-abandoned-completion.md#q683--the-fast-ending-measurement-2026-08-05)).
  > That arm reads the run's conclusion first and stands down on `cancelled`, counting `actions_gateway_eviction_rerun_withheld_total{reason="run_cancelled"}` and emitting a Normal `EvictionRerunWithheld` Event on the owner instead of calling.
  > The gateway's own reclaim of a cancelled run's pod carries the `actions-gateway.com/deletion-reason` stamp and is excluded, while a hand-delete carries no stamp and takes the recovery path, so the conclusion check is what separates that delete from a drain.
  > The recovery still spends one slot of the run's shared `maxEvictionRetries` budget (default 2, max 10), because the slot is reserved when the disruption is detected, minutes before GitHub has a conclusion to read.
  > So a repeated cancel-then-delete cycle ends in an `EvictionRetriesExhausted` warning Event on the owner even though nothing was re-run.
- **If the run is still `in_progress` at GitHub when you delete, the job *is* re-queued.** The check reads what GitHub has concluded, and a cancel takes up to GitHub's ~5-minute cancellation grace to land (measured 5m02s above); a delete that beats it looks exactly like a drain.
  Confirm with `gh run view <run-id> --json status,conclusion` before deleting, and expect `status: completed`, `conclusion: cancelled`.
- **Bound the worst case with `maxWorkerLifetime`** on the `RunnerSet`, which caps how long any worker — cancelled or not — can hold its slot.
- Do **not** expect a lower `completedPodTTL` or `pendingPodDeadline` to help: both act on pods that have already stopped or never started, and this pod is running.

---

## Evicted Scale-Set Jobs Are Not Re-Run Automatically

**Symptoms.** A `RunnerSet` has workers being evicted, but `actions_gateway_eviction_retries_total` stays flat and the jobs stay failed until someone re-runs them by hand.
Usually one of two accompanying signals is present:

- `actions_gateway_eviction_recovery_identity_unknown_total` is incrementing, and `kubectl describe runnerset` shows a `Warning` event with reason `EvictionRecoveryIdentityUnknown`; or
- neither counter moves at all.

**Cause.** The AGC provisions fire-and-forget (the runner pulls and completes its own job), so nothing in the AGC is watching a given worker pod and the pod is the only place to read the job's identity from.
Recovery therefore depends on two things being on the pod: the run identity (`actions-gateway.com/run-id`, `actions-gateway.com/repository`), stamped from the assignment message's `ownerName`/`repositoryName`/`workflowRunId` and replaced from the `JobStarted` of the job the pod's runner actually took (Q1152), and the `actions-gateway.com/acquisition-protocol=ScaleSet` label that tells the reconciler the pod is its to recover.
Either one missing makes recovery inert for that pod.

**Diagnostics.**

```sh
# Does the evicted worker carry its run identity and the acquisition-protocol label?
kubectl get pods -n <namespace> -l actions-gateway.com/acquisition-protocol=ScaleSet \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,REASON:.status.reason,RUN:.metadata.annotations.actions-gateway\.com/run-id,REPO:.metadata.annotations.actions-gateway\.com/repository,HANDLED:.metadata.annotations.actions-gateway\.com/eviction-handled-at'
```

Read the result as follows.

| What you see | What it means |
| --- | --- |
| `REASON` is empty rather than `Evicted` | Not a kubelet eviction. Only `PodFailed`/`Evicted` triggers recovery — anything else ran and reported its own outcome, so re-running it would double-report |
| `REASON` is `Evicted`, `HANDLED` set, no re-run, and an `EvictionRerunWithheld` Event | The kubelet evicted the worker for exceeding its own storage limit; read its message with `kubectl get pod <name> -o jsonpath='{.status.message}'`. Working as intended: raise the limit (Q1177) |
| `RUN`/`REPO` empty | The assignment carried no run identity. This is the `EvictionRecoveryIdentityUnknown` case |
| `RUN`/`REPO` present, `HANDLED` empty | The reconciler has not adjudicated it yet, or the scan is failing — check the AGC log for `scale-set eviction recovery scan failed` |
| `HANDLED` set but no re-run | The claim succeeded and the rerun call failed. Look for `eviction auto-retry failed` in the AGC log, or an exhausted budget |
| `HANDLED` set, no re-run, and the pod has no `started-job-id` | Deliberate. Another worker has started the job this pod was created for, or the pod's container never ran and an idle worker can still take that job, so that run lost nothing. The AGC logs `not re-running that job's run` (Q1169). Where the container never ran, another worker started its job and no worker is idle, the AGC force-cancels the job left without a runner instead and logs `recovering the job left without a runner instead` (Q1235) |

```sh
# The AGC's own account of what it did with the eviction.
kubectl logs -n <namespace> deploy/<agc-deployment> \
  | grep -E 'pod evicted|eviction auto-retry|run identity is unknown|eviction recovery scan failed|not re-running that job'

# The identity the queue actually delivered (raise the AGC to debug first).
kubectl logs -n <namespace> deploy/<agc-deployment> \
  | grep 'carries no run identity'
```

**Resolution.**
- **`RUN`/`REPO` empty on every evicted worker.** GitHub is not sending the assignment fields recovery depends on.
  There is no configuration workaround: the identity has no other source.
  Re-run affected jobs manually, and treat a sustained rate as a protocol regression worth reporting rather than a capacity problem.
- **`REASON` is not `Evicted`.** Working as intended.
  The pod ran and reported its own result; if the *job* nonetheless hung, see [Terminated Worker Pod Never Reports Its Job](#terminated-worker-pod-never-reports-its-job-job-hangs-on-github-until-the-lock-lapses) instead.
- **Evicted pods disappearing before recovery runs.** Recovery reads the evicted pod, so it must still exist.
  A very short `completedPodTTL` combined with a backlogged AGC can delete the evidence first; raise `completedPodTTL` (default 5 m) if you see terminal workers vanishing within seconds.
- **Budget exhausted instead.** See [Evicted Worker Pods Exhausting Retry Budget](#evicted-worker-pods-exhausting-retry-budget).
  Note the budget is per `RunnerSet`, so a run spanning two of them has a budget in each.
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
Gateway versions before the Q385 fix ran the entrypoint wrapper as PID 1 with no signal handling, so `run.sh`, the process that reports the job to GitHub, never saw the signal.
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

**Symptoms.** Jobs sit queued in GitHub while the `RunnerSet` reports `WorkerQuotaExceeded=True` (and `actions_gateway_runnerset_worker_quota_exceeded` reads `1`) — the AGC is declining to take on work it cannot place.
`actions_gateway_scaleset_advertised_capacity` **falls**, to `0` when no worker pod fits at all, and `actions_gateway_scaleset_capacity_withheld{reason="quota"}` accounts for the gap.

If instead `actions_gateway_quota_retries_exhausted_total` is incrementing, pod creation is failing with a `Forbidden` error containing "exceeded quota" in the AGC logs, with a `Warning` event of reason `QuotaRetriesExhausted` on the `RunnerSet`, and the listener retries the job on a later poll.
That is the same root cause reaching the AGC one layer later — see [When quota exhaustion still reaches the retry path](#when-quota-exhaustion-still-reaches-the-retry-path) below.

**The admission gate declines on quota (#784, Q443).** When the namespace `ResourceQuota` cannot admit another worker pod, the AGC stops taking on work rather than claiming it and stalling.
The check is a live read of every `ResourceQuota` in the namespace (`hard − used`) against one more worker's footprint, and it **fails open**: a quota the AGC cannot read leaves capacity exactly as it was.

The AGC advertises a capacity of `min(worker ceiling, own in-flight pods + quota headroom)` on every long poll, so GitHub assigns fewer jobs, or none at `0`.
Nothing is assigned that cannot be placed.
The cost is granularity: the decision is per poll for the whole set, so restored headroom reopens assignment within one long poll (~50s) rather than instantly.

The AGC surfaces two non-blocking conditions on each `RunnerSet` for the namespace-quota axis (Q82, Q303), each exported as a gauge so you can alert without kube-state-metrics.
Distinguish them from the configured ceiling, which is [normal backpressure](#scale-set-jobs-waiting-at-the-worker-ceiling-workerceilingreached) rather than a quota problem:

| Condition / metric | Meaning | Severity |
|---|---|---|
| `WorkerQuotaPressure` — `actions_gateway_runnerset_worker_quota_pressure` | Workers can't scale to the configured ceiling (`maxWorkers` / max `priorityTiers` threshold) within the quota's remaining headroom. | warning (don't page) |
| `WorkerQuotaExceeded` — `actions_gateway_runnerset_worker_quota_exceeded` | The quota can't admit even one more worker pod — the next acquired job's pod will be rejected. | error (page) |

```sh
kubectl get runnerset -n <namespace> <name> \
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

# Capacity withheld because the quota has no headroom.
# advertised + sum(withheld) == the set's declared ceiling.
# actions_gateway_scaleset_advertised_capacity{namespace, runner_set}
# actions_gateway_scaleset_capacity_withheld{namespace, runner_set, reason="quota"}

# Inspect current quota usage
kubectl describe resourcequota -n <namespace>

# Check AGC logs for quota errors
kubectl logs -n <agc-namespace> deploy/<gateway>-agc | grep "exceeded quota"

# See the retry-exhaustion event on the RunnerSet
kubectl describe runnerset -n <namespace> <name> | grep -A1 QuotaRetriesExhausted
```

**Resolution.**
- If quota is consistently full: increase the namespace `ResourceQuota` limits or reduce `maxWorkers` / `priorityTiers` thresholds so the AGC holds fewer concurrent pods.
- If quota clears quickly but the retry window is too short: increase `maxQuotaRetries` or `quotaRetryDelay` on the `RunnerSet` spec (defaults: 5 retries / 30s delay).
- To skip the in-place wait: set `maxQuotaRetries: 0`, so a pod create that hits the quota fails immediately and the listener retries the job on a later poll.

### When quota exhaustion still reaches the retry path

The admission gate reduces how often `createPodWithQuotaRetry` is entered; it does not replace it.
Quota can still be gone by the time the pod is created, because:

- a **sibling AGC or another workload** in the namespace consumed the headroom between the gate's read and the pod create;
- the gate's read is of `ResourceQuota.status`, which is **eventually consistent** — `used` lags the pods that already exist;
- the AGC **restarted** between the assignment and the create;
- the gate is **off** (`AGC_QUOTA_ADMISSION=false`, below), or it **failed open** on a quota it could not read.

One more: the advertised capacity is recomputed **per poll**, not per job, so headroom lost between two polls is not seen until the next one.

So keep `maxQuotaRetries`/`quotaRetryDelay` tuned; the two mechanisms are layered, not alternatives.

### Turning the quota rung off

The gate's quota rung is **on by default**, and the AGC honours `AGC_QUOTA_ADMISSION=false` in its own environment to take the work first and discover quota exhaustion in the retry loop.
The set then advertises its declared ceiling and publishes no `capacity_withheld{reason="quota"}` series at all.
On a GMC-provisioned gateway there is deliberately no tenant-facing field for it: the only route to the AGC Deployment's env is the testing-only `AGC_EXTRA_*` passthrough behind the GMC's `--allow-agc-extra-env`, which is not for production use.

The one situation that would justify the escape hatch is a cluster whose `ResourceQuota.status` accounting is unreliable enough to starve a tenant that in fact has room — symptom: capacity withheld for `quota` while `kubectl describe resourcequota` shows real headroom and no worker pods are being created.
Fix the quota accounting first.
If you hit a case that genuinely needs the opt-out in production, open an issue so it can be promoted to a supported field rather than worked around.

---

## Why Is My `RunnerSet` Not Being Offered Jobs?

**Symptoms.** Workflow jobs sit queued at GitHub, the `RunnerSet` is `Ready`, the AGC logs nothing alarming, and fewer worker pods are running than `maxWorkers` allows.
Nothing is broken: something is deliberately withholding intake, and this is how to find out what.

**Cause.** The set advertises one number to GitHub per long-poll, the total jobs GitHub may keep assigned to it, and GitHub assigns nothing beyond it.
That number is the minimum of the admission ladder's rungs, so any rung can lower it.
`status` carries both the number and the per-rung breakdown, so this needs no Prometheus access (Q721).

**Diagnostics.**

```sh
# The advertised total, and which rung took each withheld slot.
kubectl get runnerset -n <namespace> <name> \
  -o jsonpath='{.status.advertisedCapacity}{"\n"}{range .status.withheldCapacity[*]}{.reason}{"\t"}{.slots}{"\n"}{end}'

# The same, plus the conditions that explain two of the rungs.
kubectl describe runnerset -n <namespace> <name>
```

A rung listed with `slots: 0` was evaluated and is not withholding anything, which is the ordinary state: every rung the poll evaluated publishes a value, so a healthy set carries an entry per rung with `slots: 0` rather than an empty list.
A rung **absent** from the list was not evaluated at all.
That is a different statement, and for `quota` it means the AGC-wide `AGC_QUOTA_ADMISSION=false` kill switch is set.

The entries sum to the set's declared ceiling minus `advertisedCapacity`, because the rungs compose as a minimum and each entry is its own marginal contribution.

**Reading the result.**

| `reason` | What is withholding | Where to look next |
|---|---|---|
| *(every entry `slots: 0`, `advertisedCapacity` equals your ceiling)* | Nothing. The set is offering its full capacity and the queue is GitHub-side | The `RunnerLabelsIncomplete` condition, which names the labels that did and did not register at GitHub, and whether the run is blocked by a workflow-level `concurrency:` group |
| `quota` | The namespace `ResourceQuota` has no headroom for more worker pods | The `WorkerQuotaExceeded` condition names the binding resource; [namespace ResourceQuota exhaustion](#jobs-failing-due-to-namespace-resourcequota-exhaustion) |
| `capacity` | The cluster cannot currently *place* another worker pod of this shape, and the set opted into `spec.capacityGate` | The `WorkerCapacityDeclined` condition names the signal that said so; [WorkerCapacityDeclined](#runnerset-reports-workercapacitydeclined-the-gateway-stopped-claiming-jobs) |
| `scaleup` | The set's own `spec.scaleUp` creation-rate limit is ramping | Nothing, if the number is falling. This rung clears itself as the bucket refills, so a set climbing out of idle shows it withholding a lot and then less |

**`advertisedCapacity: 0` is not the same as the field being absent.** Zero means the set is advertising zero: every slot withheld, and the `withheldCapacity` entries say by what.
Absent means no advertisement has been made: a listener that has not completed its first poll, or a set whose references do not resolve (check `Ready`).

**Resolution.** Act on the binding rung, not on the total.
Raising `maxWorkers` while `quota` is withholding changes nothing, because the ceiling is not what is binding: the advertisement is already below it.

**One caveat when reading it.** The value is recomputed every long-poll, and a poll that changes it wakes a reconcile to publish it, so the field follows a capacity change within roughly one long-poll.
That holds on an idle set with no jobs and no worker pods, which nothing else would have reconciled.
It is still a snapshot rather than a live feed: a set whose capacity is oscillating is better watched through `actions_gateway_scaleset_advertised_capacity` and `actions_gateway_scaleset_capacity_withheld`, which carry the same numbers at metric resolution ([observability: metrics](observability-metrics.md#scale-set-acquisition-tier-q264)).

## Worker Pod Fails to Start After Secure-by-Default SecurityContext

**Symptoms.** A worker pod that previously ran now stays in `CreateContainerConfigError` or `Pending`, or is rejected at admission.
`kubectl describe pod` shows one of:
- `Error: container has runAsNonRoot and image has non-numeric user (<name>), cannot verify user is non-root` — the AGC stamped `runAsNonRoot: true` (every profile except `privileged`) and the image declares its user **by name**, which kubelet cannot verify against a numeric UID.
  The **default** `ghcr.io/actions/actions-runner` image (`USER runner`) is handled automatically — the AGC gap-fills `runAsUser: 1001` so kubelet can verify it (Q115).
  You hit this only with a **custom/third-party** runner image whose named user is **not** UID 1001, so the auto-stamped 1001 doesn't match what its `USER` resolves to, or whose image has no numeric UID at all.
- `Error: container has runAsNonRoot and image will run as root` — same stamp, but the worker image's default user is `root` (UID 0).
- A PodSecurity admission denial naming `allowPrivilegeEscalation != false` or `unrestricted capabilities` — the namespace is on the `restricted` security profile and a tenant container needs `sudo` or extra capabilities.

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
kubectl get deploy <gateway>-agc -n <tenant-namespace> -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="SECURITY_PROFILE")].value}'; echo
```

**Resolution.**
- Default `actions-runner` image: **no action needed** — the AGC gap-fills `runAsUser: 1001` automatically (Q115).
- Custom named-user image whose user is **not** UID 1001: declare its actual numeric UID in the `RunnerTemplate` `podTemplate` so kubelet can verify non-root (an explicit `runAsUser` overrides the gap-filled 1001):

  ```yaml
  podTemplate:
    spec:
      securityContext:
        runAsUser: <image-uid>
        runAsGroup: <image-gid>
  ```

  Note: a `podTemplate` edit takes effect on the next acquired job — the AGC re-reads the `RunnerTemplate` at pod-build time, so no AGC restart is needed (Q117).
  Pods already running keep the template they were built with.
- Root-based image that must run as root: the defaults are gap-fill only — set an explicit `securityContext.runAsNonRoot: false` (and `runAsUser`/`runAsGroup` as needed) on the runner container in the `RunnerTemplate` `podTemplate`.
  No profile escalation is required for `baseline`.
- Job genuinely needs `sudo`/capabilities: move that workload to a `baseline` namespace (the default), which does not stamp the privilege-escalation/capability floor.
  Reserve `restricted` for workloads that can run without them.
- Workload needs a real privileged container (DinD, kernel modules): label the namespace `actions-gateway.com/security-profile=privileged` and pair it with a sandbox runtime — see [§5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in).

---

## Security Profile Downgrade Rejected by Admission

**Symptoms.** A change to a namespace's `actions-gateway.com/security-profile` label that moves it to a *less restrictive* level is rejected by the `namespace-security-profile-guard` policy:

```
security profile downgrade from restricted to baseline is not permitted without
the actions-gateway.com/allow-profile-downgrade=allowed annotation; downgrading
relaxes Pod Security Admission isolation and must be deliberate
```

The profiles rank `privileged` (least restrictive) < `baseline` < `restricted` (most restrictive); any move *down* that ranking is a downgrade, including `baseline → privileged`.

**Likely causes.**
- A deliberate relaxation, such as rolling back a `baseline → restricted` hardening attempt that broke the tenant's pods at Pod Security Admission.
- **Unintentional drift:** a GitOps sync or a re-applied namespace manifest that omits the label while the live namespace is on `restricted`.
  An absent label compares as `baseline`, so an omitted label reads as a downgrade: the guard catching a silent weakening.

**Diagnostics.**

```sh
kubectl get namespace <tenant-namespace> -o jsonpath='{.metadata.labels.actions-gateway\.com/security-profile}'; echo
```

**Resolution.**
- **If the downgrade is intended:** add the opt-in annotation, then change the label:

  ```sh
  kubectl annotate namespace <tenant-namespace> actions-gateway.com/allow-profile-downgrade=allowed
  kubectl label namespace <tenant-namespace> actions-gateway.com/security-profile=baseline --overwrite
  ```

  Remove the annotation afterwards so that future accidental downgrades keep being blocked.
  The looser profile applies to future worker pods once the GMC re-stamps the namespace's Pod Security labels; pods already running are not re-evaluated.
- **If the downgrade is accidental (drift):** do **not** add the annotation.
  Fix the manifest to match the live profile, so GitOps stops trying to weaken the namespace.

> Note: this guard catches *silent* downgrades; it is not an absolute boundary.
> Anyone who can annotate the namespace can opt in.
> See [§5.3 — No silent profile downgrades](../design/05-security.md#no-silent-profile-downgrades).

---

## `noProxyCIDRs` Rejected: Entry Would Bypass the Proxy for GitHub

**Symptoms.** A `kubectl apply` is rejected by the GMC validating webhook with:

```
admission webhook "vegressproxy-v2.kb.io" denied the request:
spec.noProxyCIDRs[0]: "github.com" would route GitHub traffic (github.com)
around the per-tenant egress proxy, defeating egress-IP attribution; remove it
— GitHub must always traverse the proxy. noProxyCIDRs may exclude internal
destinations (CIDRs or domain suffixes), never GitHub
```

Because the `EgressProxy` carries no `githubURL` of its own, the guard also runs from the *referrer* side: an `ActionsGateway` (`spec.defaultProxyRef`) or `RunnerSet` (`spec.proxyRef`) write that binds a GitHub host to a proxy whose `noProxyCIDRs` exclude it is rejected too, e.g.:

```
admission webhook "vactionsgateway-v2.kb.io" denied the request:
spec.defaultProxyRef: EgressProxy "corp-proxy" spec.noProxyCIDRs[0]:
".corp.example" would route GitHub traffic (ghes.corp.example) around the
per-tenant egress proxy, ...
```

**Likely cause.** An `EgressProxy`'s `spec.noProxyCIDRs` entry covers a GitHub host.
The host is a public one, such as `github.com`, `api.github.com` or `ghcr.io`, or the `githubURL` host of a gateway that references the proxy, directly or through a `RunnerSet`, from the proxy's own namespace or from one it lists in `spec.sharing.allowedNamespaces`.
That second kind is how a GitHub Enterprise Server host is protected.

An entry covers a host in any of these forms:

| Form | Example |
|---|---|
| The host, or a parent domain of it, with or without a leading dot | `github.com`, `.github.com`, `githubusercontent.com`, `.com` |
| Two values in one entry, joined by a comma, a space or a tab | `internal.example.com,github.com`, `internal.example.com github.com` |
| A wildcard | `*`, `*.github.com` |
| A host with a port, whatever the port | `github.com:443` |
| A host with a trailing dot, or a host without one where the `githubURL` host has it | `github.com.`, `ghes.corp.example` for `https://ghes.corp.example./my-org` |
| A lone dot, where a gateway's `githubURL` host ends in a dot | `.` for `https://ghes.corp.example./my-org` |
| A hostname spelled with non-ASCII letters | `ｇｉｔｈｕｂ.com` |
| An address or CIDR that contains a `githubURL` host written as an IP | `10.0.0.0/8` for `https://10.1.2.3/my-org` |

**Why it is enforced.** The GMC joins the entries with commas into the AGC's and the workers' `NO_PROXY`, and each form above exempts the host in the AGC's HTTP client or in libcurl, which `git` and `curl` in a worker pod use.
That tenant's GitHub traffic would skip the per-tenant egress proxy, which defeats the egress-IP attribution that isolates tenants.
The check runs on **both** the proxy write and the gateway/`RunnerSet` write, so the conflicting pair is rejected whichever side is applied last; a rejection on the referrer side names the conflicting `EgressProxy` and entry.
A reference to a proxy in another namespace is compared once that proxy grants the referrer's namespace, and the write that adds the grant is checked too.

**Resolution.** Remove the GitHub-matching entry — GitHub must always traverse the proxy.
`noProxyCIDRs` is for *internal* destinations only and accepts CIDRs (`10.0.0.0/8`), bare IPs, and non-GitHub domain suffixes (`svc.cluster.local`, `internal.example.com`).
The guard does not detect a **CIDR/IP range** that covers the addresses a GitHub *hostname* resolves to: the AGC's client never resolves a name before matching, what the tools in a worker pod do was not measured, and GitHub's published ranges rotate.
Never add those either; that residual is the operator's responsibility.

An object stored before `v2.0.0` tightened this check keeps working.
An edit that leaves an `EgressProxy`'s `noProxyCIDRs` and `allowedNamespaces` alone is accepted, and so is an edit to a gateway or `RunnerSet` that leaves its proxy reference alone.
Changing the list, pointing a reference at the proxy, or deleting and re-creating either object is rejected until the entry is gone; the upgrade guide shows how to find [the entries](upgrade.md#action-required-if-a-noproxycidrs-entry-exempts-github-through-a-comma-wildcard-port-or-address-the-entry-is-now-rejected) and [the shared-proxy pairs](upgrade.md#action-required-if-a-shared-egressproxy-exempts-a-consumers-github-host-the-pair-is-now-rejected).

---

## Privileged Worker Container Rejected by Admission

**Symptoms.** A `RunnerTemplate` whose `podTemplate` requests a privileged container or init container is rejected by the GMC validating webhook:

```
admission webhook "vrunnertemplate-v2.kb.io" denied the request:
podTemplate.spec.containers["runner"]: privileged containers are not permitted in a
namespaced RunnerTemplate; use a platform-owned ClusterRunnerTemplate
```

**Likely cause.** A tenant must not be able to author a privileged worker shape, so a namespaced `RunnerTemplate` refuses `securityContext.privileged: true` under every profile.

**Resolution.**
- If the privileged worker is intended (e.g. the Kata/DinD pattern), have the platform create a `ClusterRunnerTemplate` holding it and set the `RunnerSet`'s `templateRef.kind: ClusterRunnerTemplate`.
  The worker pod is admitted only in a namespace on the `privileged` security profile, which a platform admin must make **eligible** with the label `actions-gateway.com/privileged-profile=allowed` (see [Privileged Security Profile Rejected](#privileged-security-profile-rejected-namespace-not-eligible) below).
  Privileged is a deliberate, audited relaxation: pair it with a sandboxed `runtimeClassName` (Kata, gVisor) per [§5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in).
- If the privileged flag is accidental, remove `securityContext.privileged: true` from the pod template.

> Note: Pod Security Admission, stamped per the namespace's profile, is the runtime enforcement backstop for both kinds.

---

## `RunnerTemplate` Rejected: Reserved Pod Field

**Symptoms.** Creating or updating a `RunnerTemplate` (or `ClusterRunnerTemplate`) is rejected by the GMC validating webhook with one of:

```
admission webhook "vrunnertemplate-v2.kb.io" denied the request:
podTemplate.spec.containers["runner"]: env "HTTP_PROXY" is reserved: the AGC
injects the egress-proxy variables (HTTP_PROXY/HTTPS_PROXY/NO_PROXY/PROXY_CA_CERT_PATH)
into worker containers; setting it in a template is overridden and not permitted
```

```
admission webhook "vrunnertemplate-v2.kb.io" denied the request:
podTemplate.spec.containers["runner"]: privileged containers are not permitted
in a namespaced RunnerTemplate; use a platform-owned ClusterRunnerTemplate for
privileged (DinD/sysbox) worker shapes
```

**Likely cause.** A worker pod's identity and egress wiring are controller-enforced invariants.
The `v2` API makes them an author-time rejection, so a template fails closed instead of being rewritten behind your back.

- **Reserved proxy env vars** (`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`/`PROXY_CA_CERT_PATH`, matched case-insensitively) are injected by the AGC and may not be set in a template — on either kind.
- **Privileged containers** are rejected on the namespaced `RunnerTemplate` (a tenant must not self-author a privileged worker shape) but **allowed** on the cluster-scoped `ClusterRunnerTemplate`, which only a platform administrator can create.

The scalar reserved pod-level fields (`serviceAccountName`, `host{PID,Network,IPC}`, `automountServiceAccountToken`) are rejected by the CRD's own validation rules with a similar "is reserved" message.

**Resolution.**
- Remove the reserved proxy env vars from every container and init container; the AGC sets them itself.
- For a **privileged** worker shape (Kata/DinD/sysbox), have a platform administrator publish it as a `ClusterRunnerTemplate` and reference it from the `RunnerSet`'s `templateRef` with `kind: ClusterRunnerTemplate`.
  Privileged pods still require the namespace's Pod Security Admission level to admit them (stamped from the namespace's security profile), which remains the runtime backstop.

---

## `RunnerTemplate` Rejected: Pod Shape Past Baseline, Volume Type, or ServiceAccount Token

**Symptoms.** Creating a `RunnerTemplate`, or changing its `podTemplate`, is rejected by the GMC validating webhook with one of:

```
admission webhook "vrunnertemplate-v2.kb.io" denied the request:
podTemplate exceeds the Pod Security Standards baseline level, which a namespaced
RunnerTemplate must meet: non-default capabilities (container "runner" must not
include "SYS_ADMIN" in securityContext.capabilities.add); use a platform-owned
ClusterRunnerTemplate for a worker shape that needs more
```

```
admission webhook "vrunnertemplate-v2.kb.io" denied the request:
podTemplate.spec.volumes: restricted volume types (volume "data" uses restricted
volume type "nfs"); a namespaced RunnerTemplate may use only configMap, csi,
downwardAPI, emptyDir, ephemeral, image, persistentVolumeClaim, projected and
secret volumes; mount other storage through a PersistentVolumeClaim, or use a
platform-owned ClusterRunnerTemplate
```

```
admission webhook "vrunnertemplate-v2.kb.io" denied the request:
podTemplate.spec.volumes["creds"]: a projected serviceAccountToken source is
reserved: worker pods carry no Kubernetes API credential; remove the source
```

The third also comes from `vclusterrunnertemplate-v2.kb.io`, for a `ClusterRunnerTemplate`.

**Likely cause.** Each message is one rule, and the text in parentheses names the container, volume and value that tripped it.

- **Past baseline.** A namespaced `RunnerTemplate` must meet the Pod Security Standards `baseline` level on every security profile, the `privileged` one included.
  The webhook runs the checks Pod Security Admission runs, so the reasons read the same: a capability outside the baseline set, `hostPath`, `hostPort`, an `Unconfined` seccomp or AppArmor profile, an unmasked `procMount`, an unsafe sysctl, an SELinux type outside the allowed set, a Windows `hostProcess` container, a `host` on a probe or lifecycle hook.
- **Volume type.** A namespaced `RunnerTemplate` may declare only the volume types the `restricted` level allows.
  The node mounts an `nfs` or `iscsi` volume itself, so that traffic passes neither the worker `NetworkPolicy` nor the egress proxy.
- **ServiceAccount token.** Neither kind may project a ServiceAccount token.
  `automountServiceAccountToken: false` does not stop a `projected` volume from carrying one.
  The same rule refuses a `podCertificate` source whose signer begins with `kubernetes.io`, with a message that names the signer.

**Resolution.**

- For a shape past `baseline`, or a volume type off the list, have a platform administrator publish the template as a `ClusterRunnerTemplate` and reference it from the `RunnerSet`'s `templateRef` with `kind: ClusterRunnerTemplate`.
  The cluster-scoped kind is exempt from both rules; Pod Security Admission still decides whether the namespace admits the pod.
- Where the platform's own admission policy is meant to decide tenant pod shape, the two rules can be turned off for the whole install with the chart value `allowUncheckedRunnerTemplatePodShape` ([what that gives up](upgrade.md#action-required-if-a-namespaced-runnertemplate-goes-past-pod-security-baseline-the-shape-is-now-rejected)).
  A policy that permits a shape does not override this webhook's refusal.
- For shared or network storage a tenant owns, mount a `persistentVolumeClaim` instead ([shared worker storage](worker-shared-storage.md)).
- Remove a `serviceAccountToken` source, or a `podCertificate` source with a `kubernetes.io` signer.
  No template kind may carry one, and the AGC removes it from the worker pod regardless.

A template stored before `v2.0.0` keeps working: an edit that leaves `podTemplate` alone is accepted.
The first change to `podTemplate` is checked against all three rules ([finding the affected templates](upgrade.md#action-required-if-a-namespaced-runnertemplate-goes-past-pod-security-baseline-the-shape-is-now-rejected)).

---

## `RunnerSet` Rejected: Duplicate First `runnerLabel`

**Symptoms.** Creating or updating a `RunnerSet` is rejected with one of:

```
admission webhook "vrunnerset-v2.kb.io" denied the request: ScaleSet
runnerLabels[0] "linux" is already used by RunnerSet "other-set" registered against
GitHub scope "github.com/acme"; a ScaleSet set's FIRST runnerLabel is its scale-set
name at GitHub, so two sets sharing it would drive one scale set. Pick a distinct
first label (later labels may overlap freely)
```

```
admission webhook "vrunnerset-v2.kb.io" denied the request: ScaleSet
runnerLabels[0] "linux" is already claimed by another RunnerSet registered against
GitHub scope "github.com/acme"; a ScaleSet set's FIRST runnerLabel is its scale-set
name at GitHub, so two sets sharing it would drive one scale set, each acquiring the
other's jobs. Pick a distinct first label (ask your platform administrator which
scale-set names that GitHub scope already holds)
```

**Likely cause & resolution.**

- **Duplicate first label in one GitHub scope (GMC webhook).** A set's **first** `runnerLabel` is the name of its scale-set object at GitHub, so two sets may not share it: they would drive one scale set from two controllers.
  Give each set a distinct *first* label.
  Labels after the first are ordinary match targets and **may** be shared: `[gpu, linux]` and `[arm64, linux]` coexist happily, and a job asking for `runs-on: linux` alone reaches whichever GitHub picks.

  **The uniqueness boundary is the GitHub org, enterprise, or repo that the set's gateway `githubURL` names, not the namespace.** A scale set is adopted by name against the Actions service that URL reaches, so two `RunnerSet`s in *different namespaces* whose gateways point at the *same* org still collide, and each tenant's AGC would acquire the other's jobs.
  That is why the second message names a GitHub scope but not the holding set: the conflicting `RunnerSet` may belong to another tenant, so it is withheld from the rejection and written to the GMC controller log instead.
  The `RunnerSet`'s own name plays no part: two sets named `build` in different namespaces under one org are admitted, so long as their first labels differ.
  A platform admin can find it there, or with:

  ```bash
  kubectl get runnersets -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.spec.runnerLabels[0]}{"\n"}{end}'
  ```

  The same label under a gateway bound to a **different** org, enterprise, or repo is free, because that is a different scale-set namespace at GitHub.
  Org names are matched case-insensitively (`github.com/Acme` and `github.com/acme` are one scope), matching how GitHub resolves them.
  The hosts `www.github.com` and `api.github.com` are read as `github.com`, because the AGC addresses all three as public GitHub; the rejection names the scope as `github.com/<org>` whichever one the gateway spells.

- **The rejection can land on the `ActionsGateway` instead.** If a `RunnerSet` is applied *before* its gateway exists, admission has no `githubURL` to resolve and the set is admitted (references resolve at runtime).
  The conflict is then caught when the gateway arrives:

  ```
  admission webhook "vactionsgateway-v2.kb.io" denied the request:
  spec.githubURL binds GitHub scope "github.com/acme", where RunnerSet "linux-set" in
  this namespace would claim scale-set name "linux" — a name already claimed by another
  RunnerSet registered against that scope
  ```

  Rename the named set's first `runnerLabel`, or bind the gateway to a different GitHub org, enterprise, or repo.

---

## `ActionsGateway` Reports `ScaleSetNameCollision`

> This is the **already-happening** form of the rejection above: admission stops a new colliding pair, and this reports one that is already stored.

**Symptoms.** A v2 `ActionsGateway` is `Ready=True` and provisioning normally, but carries:

```
Type:    ScaleSetNameCollision
Status:  True
Reason:  ScaleSetNameShared
Message: 1 RunnerSet(s) bound to this gateway claim a scale-set name another RunnerSet
         already claims in GitHub scope "github.com/acme": "linux-set" claims "linux".
         A ScaleSet set's FIRST runnerLabel is its scale-set name at GitHub, so both
         AGCs drive one scale set and each acquires the other's jobs. Give one side a
         distinct first runnerLabel; the GMC log names the other holder
```

`kubectl describe` shows a matching `Warning` Event with reason `ScaleSetNameShared`, and `actions_gateway_scale_set_name_collision` reads `1` for the gateway.
Tenants may also report jobs disappearing into another tenant's runners, or workflow logs from a repository they do not own.

**Likely cause.** Two `RunnerSet`s share a first `runnerLabel` under gateways bound to the same GitHub org, enterprise, or repo: the collision the [duplicate-first-label rejection above](#runnerset-rejected-duplicate-first-runnerlabel) explains in full.
Admission (Q791) rejects every write that would create such a pair, in any apply order, so a pair that exists anyway got there without being validated.
Two ways that happens:

- **Upgrade from a release before `v1.5.0`.** The uniqueness guard used to be namespace-scoped, so a cross-namespace pair was admitted then and is still stored now.
  A webhook only fires on a write, and nothing re-applies these objects, so the guard never sees them.
  **This is the common case, and it is why the condition exists.**
- **Upgrade from a release before `v2.0.0`, with a gateway spelled `www.github.com` or `api.github.com`.** The guard compared those hosts as different places from `github.com`, so a pair across two spellings of one org was admitted ([upgrade note](upgrade.md#action-required-if-a-githuburl-spells-public-github-wwwgithubcom-or-apigithubcom-a-new-gateway-is-rejected-and-a-stored-one-shares-scale-set-names-with-githubcom)).
  The condition names the scope as `github.com/<org>` on both gateways.
  Whether GitHub registers runners under those spellings was not measured, so the gateway using one may be failing to register instead of sharing jobs.
- **A window with the validating webhook uninstalled or unreachable.** `failurePolicy: Fail` makes an unreachable webhook block writes rather than admit them, so this needs the `ValidatingWebhookConfiguration` to have been absent: a partial install, or CRDs applied without the chart's webhook resources.

**Resolution.** The two tenants are driving one scale set right now, so treat it as live cross-tenant exposure.

1. **Find the other holder.** The condition names only this gateway's own `RunnerSet`s; the holder may belong to another tenant, so it is withheld from tenant-readable status for the same reason the admission rejection withholds it.
   As a platform admin, read the GMC log:

   ```bash
   kubectl logs -n gmc-system deploy/gmc-controller-manager | grep 'ScaleSet name collision'
   ```

   Or list every claim in the cluster and look for a repeated first label:

   ```bash
   kubectl get runnersets -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.spec.runnerLabels[0]}{"\n"}{end}'
   ```

   Cross-reference each set's gateway `githubURL`: only sets under gateways bound to the *same* org, enterprise, or repo collide.
2. **Decide which side keeps the name**, then change the other's **first** `runnerLabel`.
   GAG does not pick: it cannot know which tenant owns the name, and both sides carry the condition.
   Renaming re-registers a new scale set, so update the affected workflows' `runs-on` in the same change; see [Jobs Targeting One of a Runner Set's Labels Never Start](#jobs-targeting-one-of-a-runner-sets-labels-never-start-runnerlabelsincomplete).
3. **Confirm it cleared.** Both gateways flip to `ScaleSetNameCollision=False` / `ScaleSetNamesUnique` within a reconcile, and the gauge drops to `0`.

**Why the gateway still runs.** The condition is advisory: it does not gate `Ready` and does not stop the AGC.
Refusing to provision would take down *both* tenants, including the one that was there first and did nothing wrong, over a state GAG cannot arbitrate.

**A `0` gauge on an unreadable cluster is not a clean bill.** If the GMC cannot list `ActionsGateway`s or `RunnerSet`s cluster-wide, it leaves the previous condition in place rather than writing `False`, so a gateway that has never reconciled successfully carries no condition at all.
Check the GMC log for `could not read the cluster's ScaleSet claims` and confirm its RBAC still grants the cluster-wide `runnersets` read.

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

## `RunnerSet` Stuck `Ready=False` With a `NotFound` Reason

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
Apply the named `EgressProxy`, or remove the reference if you want direct egress.
**Unset everywhere is not an error:** a `RunnerSet` with no `proxyRef` under a gateway with no `defaultProxyRef` resolves to **direct egress** (`Ready=True`, `status.proxyMode: Direct`, advisory `EgressUnattributed` condition), not `ProxyNotFound` — see ["RunnerSet reports `EgressUnattributed`"](#runnerset-or-gateway-reports-egressunattributed-direct-egress).

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

**Runtime-failure reasons (not by design).** Unlike the `*NotFound` reasons above, which are the expected fail-closed state while a reference is still syncing, a `RunnerSet` whose references have all resolved can still report `Ready=False` when its listener cannot start.
The message names the error, and the reason clears once the listener starts:

```
Reason: NoActiveSessions     Message: scale-set listener failed to start: ...
Reason: RunnerGroupNotFound  Message: scale-set listener failed to start: ...
```

- `NoActiveSessions`: the listener could not register the scale set or open its session, and the AGC emits a `ScaleSetListenerStartFailed` Warning event with the underlying error.
  Check the gateway's credential Secret and the AGC logs (see ["GitHub App Secret Misconfiguration"](#github-app-secret-misconfiguration)).
- `RunnerGroupNotFound`: see [`RunnerSet` Stuck `Ready=False` With `RunnerGroupNotFound`](#runnerset-stuck-readyfalse-with-runnergroupnotfound).

A running listener always wins: once its session is polling, the set reports `Ready=True` / `ListenerActive` even if a prior start attempt logged an error.

**Session-failure conditions pushed by the listener.** The listener pushes session-failure conditions onto the `RunnerSet`:

| Condition | Reason | Meaning |
| --- | --- | --- |
| `RateLimited=True` | `SustainedRateLimit` | GitHub has answered message polling with 429 for over ten minutes. |
| `Degraded=True` | `Unauthorized` | A session call was rejected as unauthorized: the GitHub App credentials are invalid or revoked. This covers scale-set registration, session create and the queue-token refresh, and a `SessionUnauthorized` Warning event names the rejected call. |

Both are advisory (abnormal-is-`True`) and do not gate `Ready`.
`RunnerVersionTooOld` is not one of them: the reconciler reads it off the `workerImage`, because the scale-set protocol carries no runner version at session creation (see [Worker Image Runner Version](#worker-image-runner-version)).
The listener publishes the healthy state as well as the abnormal one, so a recovered condition does not sit stale until the process restarts.
`Degraded=False/SessionAuthorized` and `RateLimited=False/PollingHealthy` appear on every healthy set once its listener starts.
An abnormal condition then clears as soon as the session recovers: a successful poll clears both, and a queue-token refresh or a re-created session also clears `Degraded`.
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
| `EgressProxy` | `ProxyCertificateIssued` | Normal | The self-signed proxy TLS certificate was issued, rotated (near expiry), or replaced because the Secret held something other than the certificate the GMC issued. |
| `EgressProxy` | `ProvisioningFailed` | Warning | A reconcile failed partway through provisioning the proxy pool; the message names the failing step. |
| `EgressProxy` | `ProxyReady` / `ProxyNotReady` | Normal / Warning | The proxy pool reached / lost its `minReplicas` ready pods. |
| `EgressProxy` | `ReconcileSucceeded` | Normal | Proxy-pool provisioning recovered after a prior `ProvisioningFailed`. |

---

## `RunnerSet` or gateway reports `EgressUnattributed` (direct egress)

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

**Symptoms.** You set `ActionsGateway.spec.agcAutoscaling`, but `kubectl get vpa -n <ns>` finds nothing named `<gateway>-agc`.
`kubectl describe actionsgateway <gateway> -n <ns>` shows:

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

**This is not an outage.** The gateway is fully provisioned and `Ready=True`; the AGC runs on its `spec.agcResources` sizing (or the platform default).
`AGCAutoscalingUnavailable` is advisory and never gates `Ready` — it exists so an unsatisfiable opt-in is visible instead of silently doing nothing.
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

The `v2` API supports **multiple `ActionsGateway`s per namespace**.
A few operator-visible facts and failure modes:

- **Per-gateway resource names.** Every resource a gateway derives is prefixed with the gateway name: `<gateway>-agc` (AGC Deployment / ServiceAccount / RoleBinding / Service / AGC NetworkPolicy), `<gateway>-worker` (worker ServiceAccount), `<gateway>-workload` (workload NetworkPolicy), `<gateway>-agc-metrics-tls` / `-agc-metrics-client` (metrics Secrets).
  To list one gateway's resources: `kubectl get all,networkpolicy,secret -n <ns> -l actions-gateway.com/gateway=<gateway>`.
- **52-character name cap.** An `ActionsGateway`, `RunnerSet`, `RunnerTemplate`, `ClusterRunnerTemplate`, or `EgressProxy` whose `metadata.name` exceeds **52 characters** is rejected at admission (`metadata.name must be at most 52 characters`).
  The cap reserves room for the derived suffixes above so a label value / Service name stays within RFC 1123's 63-character ceiling.
  Use a shorter name.
- **Per-gateway scoping uses a field selector.** Each AGC reconciles only the `RunnerSet`s whose `spec.gatewayRef.name` targets it, using a server-side CRD field selector (KEP-4358).
  `field label not supported: spec.gatewayRef.name` means the AGC and the CRDs come from releases that declare the field on different API versions: [re-apply the CRDs for the AGC's release](upgrade.md#action-required-at-v200-apply-the-crds-with-the-upgrade-because-the-agcs-watch-depends-on-them).
  (A single gateway applies the selector too, though its scoping is a no-op.)
- **Per-gateway garbage collection.** Deleting one gateway removes only its own children (owner-referenced, per-gateway-named); a neighbor's gateway, AGC, and `RunnerSet`s are untouched.
  The one cluster-scoped child — the `agc-clusterrunnertemplate-reader.<ns>.<gateway>` `ClusterRoleBinding` — cannot carry a namespaced owner reference, so the gateway controller deletes it explicitly during teardown.
  If a gateway's `ClusterRoleBinding` lingers after the gateway is gone (e.g. the GMC was down during the delete), it is harmless (it binds a now-absent ServiceAccount) and safe to `kubectl delete clusterrolebinding agc-clusterrunnertemplate-reader.<ns>.<gateway>`.

**Prerequisite.** The five v2 CRDs ship in the separate `actions-gateway-crds-v2` chart, not the main chart.
Install it before creating any v2 object: `helm upgrade --install actions-gateway-crds-v2 oci://ghcr.io/actions-gateway/charts/actions-gateway-crds-v2`.
The shipped `agc-clusterrunnertemplate-reader` ClusterRole is in the main chart.

---

## v2 Objects Not Reconciling After Installing the CRD Chart

**Symptom.** You created a v2 `ActionsGateway` or `EgressProxy` and nothing happens — no AGC Deployment, no proxy pool, no status conditions.
The GMC log shows it came up without the v2 controllers:

```text
actions-gateway.com/v2 CRDs not installed; v2 controllers disabled (install the actions-gateway-crds-v2 chart and restart the GMC to enable them)
```

**Cause.** The GMC checks for the `actions-gateway.com/v2` CRDs **once at startup**.
If they were absent then, it disables the v2 controllers (and the v2 IP-range refresh passes) deliberately, which keeps an install without the CRD chart from spinning a "no matches for kind" retry loop.
Installing the CRD chart into an already-running cluster does **not** retroactively enable them.

**Remediation.**

```sh
# 1. Confirm the v2 CRDs are now installed and Established.
kubectl get crd actionsgateways.actions-gateway.com egressproxies.actions-gateway.com

# 2. Restart the GMC so it re-detects them at startup.
kubectl rollout restart deploy -n gmc-system gmc-controller-manager

# 3. Confirm the GMC now reports v2 enabled.
kubectl logs -n gmc-system deploy/gmc-controller-manager | grep 'actions-gateway.com/v2'
# Expected: actions-gateway.com/v2 CRDs detected; enabling v2 controllers
```

After the restart the v2 controllers pick up any v2 objects already in the cluster.

> **The per-tenant AGC has the same gate.** Each AGC checks for the `actions-gateway.com/v2` `RunnerSet` CRD once at startup and, when it is absent, exits with `GATEWAY_NAME=<gateway> is set but the actions-gateway.com/v2 RunnerSet CRD is not installed, so this AGC would reconcile nothing (install the actions-gateway-crds-v2 chart)`.
> Install the CRD chart and the restarted AGC pod comes up; the GMC also restarts it when it rolls the AGC Deployment.

---

## Privileged Security Profile Rejected: Namespace Not Eligible

**Symptoms.** Setting a namespace's `actions-gateway.com/security-profile` label to `privileged` is rejected by the `namespace-security-profile-guard` policy:

```
security profile privileged is not eligible in namespace team-builds: it requires
the platform-applied label actions-gateway.com/privileged-profile=allowed; privileged
eligibility is a platform decision and is deliberately not tenant-settable
```

**Likely cause.** Whether a namespace may run privileged workers is a **platform** decision, not a tenant one.
The policy therefore gates the `privileged` profile behind a second label a platform administrator applies: `actions-gateway.com/privileged-profile=allowed`.
Absent the label, or with any other value, privileged is ineligible and rejected.
(The value is the enum keyword `allowed`, not `true`, to avoid the YAML boolean-coercion footgun.)

**Resolution (platform administrator).** If this tenant is approved to run privileged workers, label the namespace:

```bash
kubectl label namespace <tenant-namespace> actions-gateway.com/privileged-profile=allowed
```

From `v2.0.0` the 1.x `actions-gateway.github.com/privileged-profile` key grants nothing.
Apply it with a trusted (administrator) identity: the GMC must never set it itself, and tenants cannot.
Verify:

```bash
kubectl get namespace <tenant-namespace> \
  -o jsonpath='{.metadata.labels.actions-gateway\.com/privileged-profile}'
# Expected: allowed
```

Then set the profile label again.
To **revoke** eligibility, remove the label (`kubectl label namespace <ns> actions-gateway.com/privileged-profile-`).

> Raising a namespace from a stricter profile to `privileged` also needs the `actions-gateway.com/allow-profile-downgrade=allowed` annotation, because anything → `privileged` is a rank downgrade.
> Both gates are independent: the label is the platform's eligibility decision, the annotation is the deliberate relaxation.

If the privileged profile was **not** intended, set the label to `baseline` (the default) or `restricted`; neither consults the eligibility label.

---

## Tracing Sampler Rejected by Admission

**Symptoms.** A `kubectl apply` / `kubectl edit` / GitOps sync that sets `spec.tracing.sampler` on an `ActionsGateway` is rejected at admission with a CRD validation error like:

```
ActionsGateway.actions-gateway.com "<name>" is invalid:
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

## Tracing Endpoint Rejected by Admission

**Symptoms.** A create or an edit that sets `spec.tracing.endpoint` on an `ActionsGateway` is rejected at admission:

```
The ActionsGateway "<name>" is invalid: spec.tracing.endpoint: Invalid value:
"10.0.0.5:4317": endpoint does not parse as a URL, so the exporter would ignore it
and send spans to 127.0.0.1:4317; a bare IP needs a scheme, as in https://10.0.0.5:4317
```

**Likely cause.** The exporter ignores a value it cannot parse as a URL and sends spans to `127.0.0.1:4317` with no error, so the schema refuses it.
The usual case is a host with a port and no scheme that starts with a digit or a bracket, or holds an underscore: `10.0.0.5:4317`, `[fd00::5]:4317`, `3collector:4317`, `my_collector:4317`.
A non-numeric port, a bad percent escape and a value of only whitespace are rejected too.
The check is close to the exporter's parser rather than identical, so a URL with a `#fragment` is rejected although it would deliver; drop the fragment.

**Resolution.** Add the scheme, as in `https://10.0.0.5:4317`, or `http://` for a plaintext collector with `insecure: true`.
That includes a collector on the AGC pod's own loopback: `127.0.0.1:4317` is rejected, and `http://127.0.0.1:4317` is accepted.
To turn tracing off, remove the field.

A gateway stored before this check keeps working: an edit that leaves `endpoint` alone is accepted, and it goes on exporting to `127.0.0.1:4317` until the value is corrected.
Deleting and re-creating it, or changing `endpoint` to another value that does not parse, is rejected.

---

## Tracing Rejected by Admission: `insecure` With an `https` Endpoint

**Symptoms.** A create or an edit of `spec.tracing` on an `ActionsGateway` is rejected at admission:

```
The ActionsGateway "<name>" is invalid: spec.tracing: Invalid value:
insecure: true overrides an https:// endpoint and the AGC exports in plaintext;
drop insecure to keep TLS, or write http:// to mean plaintext
```

**Likely cause.** `insecure` is `true` and `endpoint` starts with `https://`, in any letter case.
The exporter applies `insecure` after the scheme, so the pair would export in plaintext while the endpoint says TLS, and the schema refuses it.

**Resolution.** Write `http://` for a collector that does not serve TLS, or remove `insecure` for one that does.

A gateway stored before this check keeps working, still in plaintext: an edit that leaves `endpoint` and `insecure` alone is accepted.
Deleting and re-creating it, or changing `endpoint` to another `https://` value while `insecure` stays true, is rejected ([finding the affected gateways](upgrade.md#action-required-if-spectracing-sets-insecure-true-with-an-https-endpoint-the-pair-is-now-rejected)).

---

## Egress Peer `cidr` Rejected by Admission

**Symptoms.** A create or an edit of an `ActionsGateway` that sets `networkPolicy.cidr`, under `spec.tracing` or under the Vault signer, is rejected at admission with one of these, the Vault signer's under `spec.credentials.workloadIdentity.signer.vault.networkPolicy.cidr`:

```
The ActionsGateway "<name>" is invalid: spec.tracing.networkPolicy.cidr: Invalid value: "0.0.0.0/0":
cidr must be a valid CIDR no broader than /16 for IPv4 or /48 for IPv6;
the peer is one destination, so name its address or its subnet
```

```
The ActionsGateway "<name>" is invalid: spec.tracing.networkPolicy.cidr: Invalid value: "169.254.169.254/32":
cidr must not be link-local (169.254.0.0/16, fe80::/10) or cover a cloud metadata address
(100.100.100.200, 168.63.129.16, fd00:ec2::254, fd20:ce::254)
```

**Likely cause.** The first message means the value is broader than `/16` for IPv4 or `/48` for IPv6, as `0.0.0.0/0` is, or is not a CIDR: an octet above 255, an octet or prefix length written with a leading zero, or an IPv4-mapped IPv6 address.
The second means it is link-local, inside `169.254.0.0/16` or `fe80::/10`, or covers one of the four addresses the message lists, where Alibaba Cloud, Azure, AWS and Google Cloud serve instance metadata or platform services outside link-local space.
The peer is one destination, and the GMC copies the value into the AGC's NetworkPolicy, so the schema refuses a value that would open the port to a whole network or to one of those addresses.

**Resolution.** Name the peer's own address or subnet, as in `10.0.5.7/32`.
If its addresses span more than one `/16`, leave `networkPolicy` unset and write a NetworkPolicy of your own that selects the AGC pod.

A gateway stored before this check keeps working, with the egress rule it has: an edit that leaves the `cidr` alone is accepted.
Deleting and re-creating it, or changing the `cidr` to another refused value, is rejected ([finding the affected gateways](upgrade.md#action-required-if-a-networkpolicy-peer-names-a-broad-or-link-local-cidr-the-value-is-now-rejected)).

---

## ActionsGateway Rejected: Missing or Malformed `githubURL`

**Symptoms.** A `kubectl apply` / `kubectl edit` / GitOps sync of an `ActionsGateway` is rejected at admission with either a CRD-schema error:

```
ActionsGateway.actions-gateway.com "<name>" is invalid:
spec.githubURL: Required value
```

```
ActionsGateway.actions-gateway.com "<name>" is invalid:
spec.githubURL in body should match '^https://'
```

or a validating-webhook error:

```
admission webhook "vactionsgateway-v2.kb.io" denied the request:
gitHubURL must include an organization, enterprise, or owner/repo path segment (got "https://github.com")
```

**Likely cause.** `spec.githubURL` is a **required** field: the GitHub org, enterprise, or repository URL the gateway's runners register against.
There is no default: a gateway with no URL has nothing to register against.
The CRD enforces a non-empty `https://` value; the GMC validating webhook additionally requires a parseable URL with the https scheme, a host, and at least one path segment (the org/enterprise/owner).
Common misses: the field omitted entirely, an `http://` (non-TLS) URL, or a bare host (`https://github.com`) with no org.

**Resolution.**
- Set `spec.githubURL` to an org URL (`https://github.com/my-org`), a single-repo URL (`https://github.com/my-org/my-repo`), or your GitHub Enterprise Server URL (`https://ghes.example.com/my-org`).
- It must use `https://` and name the org/enterprise/owner, and must match where the App in `spec.credentials.githubApp` is installed.
- See [tenant-onboarding — Step 2](tenant-onboarding.md#step-2-create-the-gateway-and-its-runner-set).

---

## ActionsGateway Rejected: `githubURL` Spells Public GitHub by an Alias

**Symptoms.** Creating an `ActionsGateway` is rejected at admission:

```
admission webhook "vactionsgateway-v2.kb.io" denied the request:
spec.githubURL host "www.github.com" is an alias of public GitHub that a new ActionsGateway may not use; write the host as github.com: "https://github.com/my-org"
```

This shows up on a new gateway, on a restore from backup, and on a GitOps apply that re-creates a deleted gateway from a manifest written before `v2.0.0`.

**Likely cause.** The `githubURL` host is `www.github.com` or `api.github.com`, in any letter case and with or without a trailing dot.
The AGC sends both to the public GitHub API, as it does `github.com`, so the three name one place.
`v2.0.0` accepts only `github.com` on a new gateway, because whether GitHub registers runners under the other two was not measured.

**Resolution.** Write the host as `github.com` and keep the path: the message carries the corrected URL.

A gateway stored under an alias before `v2.0.0` is not rejected: it keeps running, and an edit to it is accepted.
`githubURL` is immutable, so its manifest has to keep the alias until you delete the gateway and create it again on `github.com` ([finding the stored gateways](upgrade.md#action-required-if-a-githuburl-spells-public-github-wwwgithubcom-or-apigithubcom-a-new-gateway-is-rejected-and-a-stored-one-shares-scale-set-names-with-githubcom)).

---

## Worker-Pod Lifecycle Duration Rejected by Admission

**Symptoms.** Applying a `RunnerSet` that sets the field is rejected at admission with one of:

```
The RunnerSet "..." is invalid: spec: Invalid value: ...:
completedPodTTL must not be negative
```

```
The RunnerSet "..." is invalid: spec: Invalid value: ...:
pendingPodDeadline must be at least 1s
```

**Likely cause.** CRD CEL validation on the two worker-pod lifecycle knobs: `completedPodTTL` accepts any non-negative duration (`"0s"` means delete worker pods immediately on completion), while `pendingPodDeadline` must be at least `1s` — a zero deadline would reap every worker pod the instant it was admitted, and there is deliberately no way to disable the deadline (an unbounded Pending pod permanently leaks a concurrency-ceiling slot).

**Resolution.**
- Use a non-negative Go duration string for `completedPodTTL` (`"0s"`, `"5m"`, `"1h"`).
- Use a duration of `1s` or more for `pendingPodDeadline`; to effectively park the deadline while debugging a scheduling issue, set it large (e.g.
  `"24h"`) rather than zero.
- Omit either field to get the defaults (`5m` retention, `10m` deadline).

---

## Priority Tiers Rejected by Admission: Thresholds Not Ascending

**Symptoms.** A create or an edit that sets `spec.priorityTiers` on a `RunnerSet` is rejected at admission:

```
RunnerSet.actions-gateway.com "<name>" is invalid: spec.priorityTiers: Invalid value:
priorityTiers thresholds must be strictly ascending; a tier whose threshold is not
above the one before it never receives a worker pod
```

**Likely cause.** A tier's `threshold` is equal to or lower than the threshold of a tier listed before it.
The AGC gives each new worker the first tier whose threshold the active-pod count is still below, so such a tier is never assigned, and the schema refuses the list rather than store a tier that does nothing.

**Resolution.** List the tiers from the lowest threshold to the highest, each above the one before it.
The first workers get the first tier's class, so put first the class the set should use while it is lightly loaded.
Set `maxWorkers` to the last threshold, or leave it unset.
Sorting a set that already runs with such a list changes which classes its workers get; to keep what it does today, delete every tier whose threshold is not above all the thresholds before it instead.

A set stored before this check keeps working: an edit that leaves `priorityTiers` alone is accepted.
Deleting and re-creating it, or changing the list to another order that does not ascend, is rejected ([finding the affected sets](upgrade.md#action-required-if-a-runnersets-prioritytiers-do-not-ascend-the-list-is-now-rejected)).

---

## `kubectl apply ActionsGateway` Times Out On Webhook During GMC Rollout

**Symptoms.** Right after a GMC rolling update (image bump, env-var change, leader transition), the next `kubectl apply` of an `ActionsGateway` CR fails with:

```
Internal error occurred: failed calling webhook
"vactionsgateway-v2.kb.io": failed to call webhook:
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

## Applying Any v2 Object Fails With `no endpoints available for service "webhook-service"`

**Symptoms.** Creating or updating any v2 custom resource fails immediately, with no timeout:

```
Internal error occurred: failed calling webhook "vrunnerset-v2.kb.io": failed to call webhook: Post
"https://webhook-service.gmc-system.svc:443/validate-actions-gateway-com-v2-runnerset?timeout=10s":
dial tcp <ClusterIP>:443: connect: connection refused
```

The last clause depends on how the apiserver reaches a Service.
That form is measured on a kind cluster on Kubernetes 1.36.1; a cluster whose apiserver routes to endpoints directly reports `no endpoints available for service "webhook-service"`.
It affects every v2 kind, because each one's validating webhook fails closed against the same Service.
Reads are unaffected.

**This is not the rollout race documented above.** That one reports `context deadline exceeded`, has endpoints present but not yet serving, and clears in seconds on retry.
This one has *no endpoints at all*, fails instantly, and does not clear on retry, because there is no GMC pod for the Service to select.

**Likely causes.**

- **The cluster has no schedulable node**, so the GMC is `Pending` rather than crashed.
  A cluster kept at zero nodes for cost reasons returns through this state on every scale-up: the resize is what gives the GMC somewhere to schedule, and it is still pulling its image when the first apply lands.
- The GMC Deployment is scaled to zero.

**Diagnostics.**

```sh
# 1. Endpoints first: this is the reading the error is actually about.
#    Empty output means no Ready GMC pod backs the Service.
kubectl get endpointslice -n gmc-system -l kubernetes.io/service-name=webhook-service

# 2. Distinguish "not scheduled" from "not installed" from "crashed".
kubectl get deployment gmc-controller-manager -n gmc-system
kubectl get pods -n gmc-system -o wide

# 3. If a pod is Pending, it is a scheduling problem, not a webhook problem.
kubectl describe pod -n gmc-system -l control-plane=controller-manager | tail -20
kubectl get nodes
```

**Resolution.**

- **Wait for the rollout, then re-apply.** Nothing is broken; the apply was simply made too early:

  ```sh
  kubectl rollout status deployment/gmc-controller-manager -n gmc-system --timeout=5m
  ```

- **If the pod is `Pending`, add capacity** and let the rollout complete.
  The apply cannot succeed before it does.

**Ordering, so it does not recur.** Bring the GMC up before applying v2 objects, which is the rule [install.md § the v2 API CRDs](install.md#the-v2-api-crds) states as *create v2 objects only once the GMC is running*.
The dogfood scripts encode it: `wait_for_gmc` in `scripts/dogfood/lib/gmc.sh` runs after the pool resize that gives the GMC a node and before the apply its webhook serves.

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
kubectl get deploy -n <tenant-namespace> <proxy>-proxy \
  -o jsonpath='{.spec.template.spec.containers[0].readinessProbe.httpGet.path}{"\n"}'

# 2. Probe /readyz directly from a workload-labeled debug pod (the proxy
#    NetworkPolicy denies ingress from unlabeled pods).
kubectl run dbg-readyz --rm -i --restart=Never --quiet \
  --labels='actions-gateway/component=workload' \
  --image=alpine --command -- \
  sh -c "apk add --no-cache curl >/dev/null 2>&1; \
         curl -sv http://<proxy>-proxy.<tenant-namespace>.svc:8081/readyz"

# 3. From the same debug pod, confirm the CONNECT port accepts TCP. A 200 on
#    /readyz paired with a refused TCP dial would be a Q42 regression.
kubectl run dbg-connect --rm -i --restart=Never --quiet \
  --labels='actions-gateway/component=workload' \
  --image=alpine --command -- \
  sh -c "apk add --no-cache busybox-extras >/dev/null 2>&1; \
         nc -zv <proxy>-proxy.<tenant-namespace>.svc 8080"
```

**Resolution.**
- Upgrade the proxy image to one built with the `/readyz` gate.
  The handler returns 503 until both listeners are bound (`cmd/proxy/proxy.go` — `handleReadyz`).
- If a custom override changed the readinessProbe path back to `/healthz`, remove it.
  GMC re-applies the canonical `Deployment` on its next reconcile, so the regression window closes within a few seconds.

`/healthz` remains the liveness probe (always 200 if the process is up).
`/readyz` is the readiness gate — kubelet keeps the pod out of the Service EndpointSlice until both `:8080` and `:8081` are bound.

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
  <proxy>-proxy <gateway>-agc \
  -o jsonpath='{range .items[*]}{.metadata.name}: {.spec.ingress}{"\n"}{end}'

# 3. Distinguish an NP drop from a missing client cert: scrape from the
#    monitoring namespace without a cert (-k skips server-cert verification).
#    A `tls: certificate required` error means the NP admits you — fix the cert
#    per the TLS runbook below. A timeout means the label/NP still drops you.
kubectl run dbg-scrape --rm -i --restart=Never --quiet \
  -n <prometheus-namespace> --image=curlimages/curl --command -- \
  curl -sS --max-time 5 -k https://<proxy>-proxy.<tenant-namespace>.svc:8443/metrics
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
With the default `spec.minReplicas: 2` on the `EgressProxy`, the scheduler needs **at least two schedulable nodes**.
On a single-node dev/kind cluster (e.g.
`test/kind-config-1worker.yaml`, where the control-plane is tainted and only one worker is schedulable) the second replica can never place.

**Resolution.**
- Production: ensure the cluster has at least `spec.minReplicas` schedulable nodes for the proxy pods (the default kind config ships two workers).
- Single-node dev clusters: set `spec.minReplicas: 1` on the `EgressProxy` to run a single proxy replica.

---

## Proxy Pool Never Scales Out

**Symptom.** Under load the proxy pool sits at `minReplicas` no matter what the HorizontalPodAutoscaler (HPA) wants.
`kubectl get hpa -n <tenant-namespace>` shows a `TARGETS` percentage well above the target and `REPLICAS` climbing, while `kubectl get deploy` keeps reporting `minReplicas`.
Scaling by hand (`kubectl scale deploy <proxy> --replicas=5`) reverts within a second.

**Cause.** Fixed in the release that carries this page.
Before it, the GMC's proxy reconciler rewrote the proxy Deployment's `.spec.replicas` on every pass and re-triggered itself on the HPA's own scale write, so the pool oscillated between `minReplicas` and the HPA's target and could never stay scaled out.
Every tenant's egress capacity was capped at `minReplicas`.

**Resolution.** Upgrade the GMC.
On a fixed GMC, `.spec.replicas` belongs to the HPA; the reconciler only owns the *floor*:

| Situation | Who sets `.spec.replicas` |
|---|---|
| Deployment first created | Reconciler, to `minReplicas` |
| Deployment sitting at `0` | Reconciler, back to `minReplicas` (an HPA will not scale a target off zero) |
| Anything else | The HPA, bounded by its own `minReplicas`/`maxReplicas` |

Two consequences worth knowing:

- **Raising the `EgressProxy`'s `spec.minReplicas` no longer bumps the Deployment directly.** The GMC raises the HPA's `minReplicas`, and the HPA scales the pool.
  That needs a working metrics pipeline — check `ScalingActive=True`:

  ```sh
  kubectl get hpa <proxy> -n <tenant-namespace> \
    -o jsonpath='{.status.conditions[?(@.type=="ScalingActive")].status}{"\n"}'
  ```

  `False` with reason `FailedGetResourceMetric` means metrics-server is missing or unhealthy; install it, or the floor will not be enforced.
- **Lowering the `EgressProxy`'s `spec.minReplicas` no longer shrinks the pool immediately.** The HPA's downscale stabilisation window (5 minutes by default) applies.

An out-of-band `kubectl scale` on the proxy Deployment is likewise no longer reverted — but the HPA will pull the count back toward its own bounds on its next sync, so it is not a durable way to resize a pool.
Change `spec.minReplicas` / `spec.maxReplicas` on the `EgressProxy` instead.

---

← [Back to Operations](README.md)
