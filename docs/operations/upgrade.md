# Upgrade and Rollback Procedures

> **Audience:** Platform engineer

For upgrade strategy intent, see [§2.6 of the architecture doc](../design/02-architecture.md#26-upgrade-strategy). For **initial installation** of the GMC, see [install.md](install.md) — this document covers day-2 upgrade and rollback.

The three independently versioned components — GMC, AGC, and worker image — each upgrade on their own cadence with separate procedures below.

---

## Table of Contents

- [Pre-Upgrade Validation Checklist](#pre-upgrade-validation-checklist)
- [Migration Notes](#migration-notes)
  - [Non-breaking: v1alpha1 is deprecated and the apiserver now warns](#non-breaking-v1alpha1-is-deprecated-and-the-apiserver-now-warns)
  - [Non-breaking: an EgressProxy pool's pods drop the app: actions-gateway-proxy label (its pool is recreated once)](#non-breaking-an-egressproxy-pools-pods-drop-the-app-actions-gateway-proxy-label-its-pool-is-recreated-once)
  - [Non-breaking: GitHub Enterprise Server gateways now reach their own appliance (they never did)](#non-breaking-github-enterprise-server-gateways-now-reach-their-own-appliance-they-never-did)
  - [Non-breaking: a GHES appliance behind a private CA can now be trusted (`spec.githubCABundleRef`)](#non-breaking-a-ghes-appliance-behind-a-private-ca-can-now-be-trusted-specgithubcabundleref)
  - [Non-breaking: drained and hand-deleted workers are now re-run automatically (cause="deletion")](#non-breaking-drained-and-hand-deleted-workers-are-now-re-run-automatically-causedeletion)
  - [Non-breaking: an evicted job's auto-re-run now lands (GitHub refused it before)](#non-breaking-an-evicted-jobs-auto-re-run-now-lands-github-refused-it-before)
  - [Non-breaking: classic-tier eviction auto-retry now fires (it never did against real GitHub)](#non-breaking-classic-tier-eviction-auto-retry-now-fires-it-never-did-against-real-github)
  - [Non-breaking: eviction auto-retry now honours GITHUB_API_BASE_URL (it never did on GHES)](#non-breaking-eviction-auto-retry-now-honours-github_api_base_url-it-never-did-on-ghes)
  - [Non-breaking: preempted workers are now re-run automatically; eviction counters gain a cause label](#non-breaking-preempted-workers-are-now-re-run-automatically-eviction-counters-gain-a-cause-label)
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
kubectl get pods --all-namespaces -l app=actions-gateway-controller

# 3. All proxy pools healthy — the recommended label covers v1 inline pools and v2
#    EgressProxy pools alike; `app=actions-gateway-proxy` finds only v1's.
kubectl get pods --all-namespaces -l app.kubernetes.io/name=actions-gateway-proxy

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

### Non-breaking: the abandoned-run fast cancel and auto re-run now work on the ScaleSet tier; two counters gain a `tier` label

1.4 added two recoveries for a worker removed before its container ever ran,
the `pendingPodDeadline` reap being the common cause: the run is force-cancelled
in about a second instead of waiting out GitHub's ~15-minute unstarted-job
timeout (Q683), and it is then re-run automatically once the runner set places a
worker pod again (Q691). In 1.4 both were wired into the classic acquisition
path only, so on the ScaleSet tier (the default, and `v2beta1`'s only option)
the same reap left the job unreported. This release closes that gap (Q766).

The identity the port needed comes off the worker pod's
`actions-gateway.com/run-id` annotation, read as the reaper deletes the pod. So
a `ScaleSet` set whose assignment messages carry no `workflowRunId` gets an
`EvictionRecoveryIdentityUnknown` Warning Event and
`actions_gateway_eviction_recovery_identity_unknown_total{cause="abandoned"}`
instead of the recovery. That is the same signal the Q417 eviction port already
emits for the same missing field.

`actions_gateway_abandoned_run_force_cancels_total` and
`actions_gateway_abandoned_run_rerun_waits_total` gain a `tier` label
(`classic` / `scaleset`). Aggregations are unaffected; add `tier` to a
`by (...)` clause where you want the split. Full note:
[Breaking observability changes](observability-metrics.md#breaking-observability-changes-q417).

No action is required at upgrade time. Two expectations do change on a
`ScaleSet` set: those two counters start reporting where they were flat zero
before, and a stuck-`Pending` worker's run now concludes in about a second
rather than at GitHub's unstarted-job timeout. Behaviour is covered in
[Worker Pod Reaped While Pending](troubleshooting.md#worker-pod-reaped-while-pending-workerpodstuckpending).

### Non-breaking: `v1alpha1` is deprecated and the apiserver now warns

Both `actions-gateway.github.com` kinds carry `deprecated: true` on their `v1alpha1`
version, so `kubectl` prints a warning on any `v1alpha1` read or write:

```text
Warning: actions-gateway.github.com/v1alpha1 ActionsGateway is deprecated; use actions-gateway.com/v2beta1 ActionsGateway. v1alpha1 is served until v2.0.0, which removes it; migrate with gag-migrate.
```

`RunnerGroup` carries the same warning naming `actions-gateway.com/v2beta1 RunnerSet` as
its replacement. The v1 monolith fans out into several v2 objects, so unlike the
`v2alpha1` → `v2beta1` move below, the replacement is a different **kind** and not the
same kind at a newer version — which is why the warning names
[`gag-migrate`](migration-v1-to-v2.md) rather than a re-apply.

**Nothing breaks, and the upgrade needs no action.** Deprecation marks intent and removes
nothing: `v1alpha1` stays fully served, existing objects keep reconciling untouched, and
the removal release is the same `v2.0.0` the docs have named since `v1.3.0`. The warning
is advisory client-side output; it does not fail an `apply`, and controllers or CI that
ignore warnings are unaffected. The AGC and GMC receive the same warning through their
own Kubernetes clients but log it once per unique message per process (deduplicated), so
a tenant reconciling under churn does not flood its controller log.

**What to do about it.** Run [`gag-migrate`](migration-v1-to-v2.md) at your convenience —
it is a one-shot fan-out of one v1 object into several v2 objects, and it preserves how
jobs are acquired. Onboard new tenants on `v2beta1` instead
([tenant onboarding](tenant-onboarding.md)). `v1alpha1` is one of three things `v2.0.0`
removes; the standing notice for all three, with a pre-upgrade checklist, is the
[deprecation and removal notice](v1alpha1-deprecation.md).

### Non-breaking: an `EgressProxy` pool's pods drop the `app: actions-gateway-proxy` label (its pool is recreated once)

**Who is affected:** every namespace that already runs a v2 `EgressProxy`. Tenants
still wholly on v1 are unaffected — no v1 object, pod, or label changes, and no v1
proxy pool restarts.

**What was broken.** A v2 `EgressProxy` pool's pods stamped `app: actions-gateway-proxy`
alongside their own `actions-gateway.com/egress-proxy: <proxy>` identity. That bare
`app` label is the *sole* key of v1's `PodDisruptionBudget` selector, v1's proxy
`Deployment` selector, and v1's required hostname anti-affinity term — v1 has one
fixed-name pool per namespace and never had to distinguish it from a second. So for the
whole coexistence window of a [v1→v2 migration](migration-v1-to-v2.md), each pool's pods
were claimed by the other's:

- Each pool's pods fell under **both** `PodDisruptionBudget`s. A pod covered by two PDBs
  cannot be evicted at all, so a node drain of either pool's node never completed.
- **Neither pool autoscaled.** A `HorizontalPodAutoscaler` has no selector of its own —
  the controller reads its scale target's, refuses to act on pods a second HPA also
  controls, and reports `ScalingActive=False`/`AmbiguousSelector`. With overlapping pod
  sets, both HPAs wedged, and the v1 pool could not scale back down either.
- The two pools **repelled each other off every node**, so coexistence cost v1+v2 worker
  nodes rather than `max(v1, v2)`.

**What changed.** A v2 pool is now selected solely by `actions-gateway.com/egress-proxy`,
which no v1 pod carries. Its `Deployment`, `Service`, PDB, `NetworkPolicy`, and
anti-affinity term all key on it alone, and the v2 workload `NetworkPolicy`'s proxy
egress peer matches that label's *presence* rather than v1's `app` label — so v2 AGC and
worker pods reach every `EgressProxy` pool in their namespace exactly as before, and no
longer reach a coexisting v1 pool.

**What to expect on upgrade.** `Deployment.spec.selector` is immutable, so an
`EgressProxy` pool provisioned before this release is **deleted and recreated once**, on
the first reconcile after the GMC upgrade. The replacement's pods wait out the old pods'
termination grace period on the hostname anti-affinity before scheduling, so that pool's
egress is briefly unavailable — treat it like the [proxy rollout](#proxy-upgrade) and
upgrade during a quiet window if the tenant is mid-build. A tenant migrating *after* the
upgrade never pays this: `gag-migrate` creates the `EgressProxy` fresh, so its pool is
born with the narrowed selector.

**What to do about it.** Nothing, unless you select v2 proxy pods by `app` yourself. Any
hand-authored `NetworkPolicy`, monitoring rule, `kubectl -l`, or dashboard that reaches a
**v2** pool's pods via `app=actions-gateway-proxy` must move to
`actions-gateway.com/egress-proxy=<proxy>` (or its presence, for any pool):

```bash
kubectl -n <namespace> get pods -l actions-gateway.com/egress-proxy
```

v1 pools keep `app=actions-gateway-proxy`, so recipes aimed at a v1 pool are unchanged.
The recommended `app.kubernetes.io/name=actions-gateway-proxy` label is still on both
pools' pods and remains the version-agnostic way to find any proxy pod — as additive
metadata only; never build a policy selector on it.

### Non-breaking: GitHub Enterprise Server gateways now reach their own appliance (they never did)

**Who is affected:** every tenant whose `spec.gitHubURL` names a GitHub Enterprise
Server (GHES) host. Tenants on `github.com` are unaffected in behaviour — but **every
AGC pod restarts on this upgrade**, on both API versions, because the AGC `Deployment`
gains an environment variable and the pod template changes.

**What was broken.** The AGC resolves the GitHub REST API base from
`GITHUB_API_BASE_URL` and falls back to `https://api.github.com` when it is unset.
Nothing ever set it: both provisioning paths injected `GITHUB_ORG_URL` from
`spec.gitHubURL` and stopped there, and no CRD field, Helm value, or other supported
surface reached the variable. A GHES gateway therefore POSTed its App JWT to
`https://api.github.com/app/installations/<id>/access_tokens` — an App that host has
never heard of. Every token mint failed, on both acquisition tiers, before any job was
acquired, and no configuration changed it.

**What changed.** The GMC derives `GITHUB_API_BASE_URL` from `spec.gitHubURL` and sets
it on the AGC `Deployment`:

| `spec.gitHubURL` | injected `GITHUB_API_BASE_URL` |
|---|---|
| `https://github.com/my-org` | `https://api.github.com` (what the AGC already defaulted to) |
| `https://ghes.example.com/my-org` | `https://ghes.example.com/api/v3` |

The same derivation now answers for runner registration and the scale-set client, which
each had their own copy. One of those copies tested `github.com` as a substring of the
whole URL, so a GHES org path literally named `github.com` — `https://ghes.corp/github.com` —
was misread as public SaaS; that is fixed with the rest.

**Egress is a separate obligation, and only partly automatic.** Token exchange reaching
the appliance does not by itself let traffic out:

- **FQDN egress modes** now carry every referrer's GHES host into the CNI policy and the
  proxy CONNECT allowlist automatically. Nothing to do.
- **CIDR mode (the default)** allows only the ranges `api.github.com/meta` publishes,
  which never contain a customer appliance. **You must supply the appliance's ranges in
  the `EgressProxy`'s `spec.destinationCIDRs`** — and, because that field is gated by the
  platform `--allowed-egress-cidrs` allowlist, a platform admin must allowlist them
  first. A pool in this state now reports `GitHubEgressIncomplete=True` with reason
  `ApplianceRangesRequired`, naming the unreachable host, instead of failing as an
  unexplained connect timeout.

**Post-upgrade check** for a GHES tenant:

```bash
kubectl get egressproxy -A -o jsonpath='{range .items[?(@.status.conditions[?(@.type=="GitHubEgressIncomplete")].status=="True")]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'
```

**Rolling back** restores the defect: GHES gateways return to minting against
`api.github.com` and acquiring no jobs. There is no configuration that works around it
on an older image other than the testing-only `--allow-agc-extra-env` flag, which
[security-operations.md](security-operations.md#github-api-base-url-must-be-https) tells
you not to use in production.

### Non-breaking: a GHES appliance behind a private CA can now be trusted (`spec.githubCABundleRef`)

**Who is affected:** tenants whose GitHub Enterprise Server appliance is fronted by a
private or internal certificate authority. Nobody else: the new field is optional, and a
gateway that does not set it produces a byte-identical AGC `Deployment`. No pod restarts
on this upgrade.

**What was broken.** The AGC and its worker pods trust the OS system roots plus the
per-tenant egress proxy's own CA, and nothing — no CRD field, Helm value, or GMC
flag — could extend that. An appliance whose certificate chains to an internal CA
therefore failed the TLS handshake on every call: token exchange and runner registration
from the AGC, and `actions/checkout`, log upload, and artifact upload from the worker
pod. Routing through an `EgressProxy` did not help, because the proxy tunnels with
`CONNECT` and the TLS session is end to end. This was the last gap named by the audit
behind the change above.

**What changed.** `ActionsGateway.spec.githubCABundleRef` (v2 only) names a `ConfigMap`
in the gateway's namespace carrying a PEM bundle under `ca.crt`. The GMC mounts it on
the AGC `Deployment` and the AGC projects the same `ConfigMap` into every worker pod, so
the control plane and the runners trust the same appliance. The bundle is **additive** —
the system roots stay trusted — so a gateway that also reaches public hosts is
unaffected.

```bash
kubectl -n team-a create configmap ghes-ca --from-file=ca.crt=/path/to/corp-root-ca.pem
kubectl -n team-a patch actionsgateway my-gateway --type=merge \
  -p '{"spec":{"githubCABundleRef":{"name":"ghes-ca"}}}'
```

Setting the field re-renders the AGC `Deployment`, so **that tenant's AGC pod restarts**.
In-flight jobs are unaffected — worker pods are separate.

**A ref that does not resolve fails closed.** A `ConfigMap` that is missing, or whose
`ca.crt` holds no PEM certificate, leaves the gateway `Ready=False` /`Degraded=True` with
reason `CABundleNotFound` or `CABundleInvalid` and provisions no AGC — rather than a
`Deployment` whose pod would sit at `ContainerCreating` with no explanation. The gateway
recovers on its own once the `ConfigMap` is applied. Runbook:
[troubleshooting.md § a GHES appliance's certificate is not trusted](troubleshooting.md#a-ghes-appliances-certificate-is-not-trusted).

**Egress remains a separate obligation.** Trusting the CA does not put the appliance's
address space in the NetworkPolicy; see the CIDR-mode note above.

**Rolling back** restores the defect for these tenants. `v1alpha1` has no equivalent
field and never will — it is frozen and removed at `v2.0.0`.

### Non-breaking: drained and hand-deleted workers are now re-run automatically (`cause="deletion"`)

**Who is affected:** every tenant on either acquisition tier. No configuration
changes; the behaviour change fires only on disruptions that previously needed a
manual re-run.

Before this version, a `kubectl drain` (or a bare `kubectl delete pod`) of a running
worker interrupted its job with no automatic recovery. Now the interrupted run is
re-run automatically when the worker's terminal phase publishes while the pod is
marked for deletion — the shape a drained running worker takes (measured, Q459). The
mark is absent on a human-cancelled run and on a genuine job failure, so neither can
trigger it; the AGC's own reaper deletions are excluded by a new controller-set
annotation. Behaviour and boundary: the
[drain runbook](troubleshooting.md#draining-a-worker-auto-re-runs-the-jobs-it-interrupts).

Operational consequences:

- **A node drain now spends re-run budget.** Each interrupted run consumes one slot of
  its shared `maxEvictionRetries` budget (default 2), under `cause="deletion"` on the
  eviction counters. Routine node maintenance across busy runner pools may warrant a
  higher budget.
- **A bare `kubectl delete pod` of a running worker re-runs its job.** If you delete a
  worker to *stop* its job, cancel the run at GitHub instead — a cancel is never
  re-run.
- **New annotation:** worker pods deleted by the AGC's reaper are stamped
  `actions-gateway.com/deletion-reason` just before deletion. Never set it by hand on
  a live worker; a hand-set stamp suppresses automatic recovery for that pod.
- **New `cause` label value:** dashboards or alerts that enumerate
  `cause="eviction"|"preemption"` on `actions_gateway_eviction_retries_total` /
  `eviction_retries_exhausted_total` should add `deletion`. No series is renamed.
- **RBAC:** the chart's `agc-tenant-role` gains `patch` on `pods` — metadata-only
  annotation stamps, never a spec or status write. Installs that mirror the role by
  hand must add the verb; without it the reaper cannot mark its own deletions and
  worker-pod cleanup stops (and the scale-set tier's recovery-claim and
  job-completed-at stamps, which always needed this verb, remain broken).
- **Timing:** the drain path's measured GitHub conclusion latency (15–26s) exceeds
  the default `evictionRetryDelay` (5s), so the first re-run call may be refused
  `403 This workflow is already running`; the Q503 retry loop (see the next note)
  absorbs that and lands the re-run within a few paced attempts.

**Rolling back** restores the old behaviour: drained workers' runs need a manual
re-run again. Leftover `deletion-reason` annotations are inert on older versions.

### Non-breaking: an evicted job's auto-re-run now lands (GitHub refused it before)

No action required, but two observable behaviours change (Q503).

Previously the AGC fired `rerun-failed-jobs` exactly once, `evictionRetryDelay`
(default 5s) after seeing a worker evicted. Against real GitHub that call always lost
a race it could not win: an ungracefully killed runner reports nothing, GitHub
concludes the run only when the job lock's TTL lapses (~10 minutes, measured 9m36-9m45s when the runner reports nothing),
and until then the API refuses re-runs with `403 This workflow is already running`.
The retry budget was spent, `actions_gateway_eviction_retries_total` incremented, and
the job was never re-run — every evicted job needed a manual re-run despite the
metrics saying recovery had happened.

The AGC now treats that refusal as "not yet" and retries the re-run every 30 seconds
inside a 15-minute window, on both acquisition tiers, still spending one budget slot
per recovery. What you will observe after upgrading:

- Evicted jobs actually re-run — expect the `disruption auto-retry triggered` log
  line and the second run attempt **~10 minutes** after the eviction, not seconds.
  Preempted jobs are unaffected in timing: their runner reports before dying, GitHub
  concludes in seconds, and the first or second call is accepted.
- A new counter, `actions_gateway_eviction_rerun_failures_total`
  (`reason="run_never_concluded"` | `"api_error"`), and a new `EvictionRerunFailed`
  Warning Event surface the recoveries whose re-run never landed — those still need
  a manual re-run. Expected to be zero; see
  [observability-metrics.md](observability-metrics.md) and the
  [runbook](troubleshooting.md#evicted-worker-pods-exhausting-retry-budget).

### Non-breaking: eviction auto-retry now honours `GITHUB_API_BASE_URL` (it never did on GHES)

**Who is affected:** any deployment that sets `GITHUB_API_BASE_URL`. Deployments against
`github.com` (the default) are unaffected: the endpoint they were reaching is the one
they were meant to reach. **No action is required, and nothing you configured was wrong.**

> **Correction.** This note originally said that meant "every GitHub Enterprise Server
> install". It did not: at the time, no GMC-provisioned AGC could set the variable at
> all, so this fix reached only hand-rolled deployments until
> [GHES gateways now reach their own appliance](#non-breaking-github-enterprise-server-gateways-now-reach-their-own-appliance-they-never-did)
> made the GMC derive it. On a GHES tenant the failure below was never observed,
> because token exchange failed first and no job was ever acquired.

Before this version the AGC resolved `GITHUB_API_BASE_URL` for the App **token
exchange** but not for the `rerun-failed-jobs` call that eviction and preemption
recovery make. That call read a provisioner field nothing ever assigned, so it fell
back to `https://api.github.com` unconditionally. On GHES it therefore posted a valid
installation token — minted against *your* endpoint — to a host that had never issued
it, and the recovery failed with:

```text
disruption auto-retry failed; manual rerun may be required ... rerun API returned 401: Bad credentials
```

The 401 names `api.github.com`, a server the operator never configured, which is why
this read as a credential problem rather than a routing one. Recovery on GHES could
not work, whatever `maxEvictionRetries` was set to.

After the upgrade:

- The rerun call addresses `GITHUB_API_BASE_URL`, the same endpoint the token was
  minted against, so recovery works on GHES.
- `actions_gateway_eviction_retries_total` starts moving on GHES clusters that evict
  or preempt workers. A rise here is the fix working, not a new fault.
- An **unset** base URL is now a startup error on the recovery path rather than a
  silent guess. It cannot occur in a GMC-provisioned AGC (the value is always
  resolved), but a hand-rolled deployment that removed the variable will now be told
  so instead of quietly addressing the wrong host.

The HTTPS rule is unchanged: a plaintext `GITHUB_API_BASE_URL` is still rejected
unless the dev/test opt-in is present, and this call now inherits exactly that rule
rather than having none of its own.

**Rolling back** re-arms the gap: recovery on GHES silently returns to calling
`api.github.com` and failing with a 401. There is no configuration that restores the
behaviour on an older image.
### Non-breaking: preempted workers are now re-run automatically; eviction counters gain a `cause` label

**Who is affected:** every tenant on either acquisition tier. The behaviour change only
manifests for tenants using `priorityTiers` with a preempting class; the metric change
affects anyone with dashboards or alerts on the eviction counters.

**What changed.** A worker displaced by kube-scheduler preemption — what a `priorityTiers`
floor drives — is now recovered like a node-pressure eviction: lock renewal stops, and
the displaced run is re-run via `rerun-failed-jobs`. Previously it reached no recovery at
all, because the scheduler *deletes* its victim rather than leaving it `Failed`/`Evicted`
(measured, Q423), so the displaced job needed a manual re-run. Detection now keys on the
`DisruptionTarget` condition with reason `PreemptionByScheduler`, which only
kube-scheduler writes.

The retry budget is unchanged and still shared: `maxEvictionRetries` remains a hard
lifetime cap per workflow run across both acquisition tiers **and** both disruption
causes. A run that is alternately evicted and preempted spends one budget, not two — so a
tenant running a preempting floor may see the budget exhaust sooner than before, and may
want to raise `maxEvictionRetries` (default 2, max 10).

**What did NOT change at the time.** A `kubectl drain`, a `kubectl delete pod`, and the
worker-pod reaper got no automatic re-run in this version. That has since changed for
the first two — see the `cause="deletion"` migration note above; the reaper's own
deletions remain excluded.

**Metric change.** Three counters gained a `cause` label (`eviction` | `preemption`):

| Metric | Labels before | Labels after |
| --- | --- | --- |
| `actions_gateway_eviction_retries_total` | `namespace`, `runner_group`, `tier` | + `cause` |
| `actions_gateway_eviction_retries_exhausted_total` | `namespace`, `runner_group`, `tier` | + `cause` |
| `actions_gateway_eviction_recovery_identity_unknown_total` | `namespace`, `runner_group` | + `cause` |

No query breaks: aggregations such as `sum(...)`, `increase(...) > 0`, and
`sum by (namespace, runner_group) (...)` are unaffected, which covers the shipped
dashboards and alert rules. Only exact-label-set matchers, and panels that rendered one
series per metric and now render more, need attention.

**Action required — review any alert that pages on the eviction rate.** The meaning of
`actions_gateway_eviction_retries_total` widened: before, a rising rate meant node
trouble; now it also counts preemptions, which are the *expected* steady state under a
preempting floor. Scope such alerts to `{cause="eviction"}` unless they genuinely want
both. The shipped `ActionsGatewayEvictionRetryAbuse` rule in
[security-operations.md](security-operations.md) was updated this way — copy the change
if you forked it.

**Rolling back** returns preempted jobs to needing a manual re-run, and removes the
`cause` label. Nothing else is affected; no configuration restores the recovery on an
older image.

### Non-breaking: classic-tier eviction auto-retry now fires (it never did against real GitHub)

**Who is affected:** any tenant on the classic acquisition tier — a `RunnerGroup`, or
a `RunnerSet` with `spec.acquisitionProtocol: Classic`. Scale-set tenants are
unaffected; that tier takes its run identity from the assignment message and has
always worked. **No action is required, and nothing you configured was wrong.**

Before this version the AGC looked for a classic job's workflow run in the
`system.github.run_id` and `system.github.repository` job variables. A real
`acquirejob` response carries neither: the run identity lives in the payload's
serialised `github` context. The run therefore resolved to `0`, and every eviction of
a classic worker took `handleEviction`'s early return — logging `pod evicted but
run_id unknown; skipping auto-retry` and calling nothing. `maxEvictionRetries` was
configurable but unreachable. The same lookup fed the worker-pod annotations, so real
classic worker pods also carried no `actions-gateway.com/run-id`,
`/repository`, or `/workflow`.

After the upgrade, on the classic tier:

- An evicted worker's run is re-run automatically, within the run's
  `maxEvictionRetries` budget (default 2) — the behaviour the docs have always
  described.
- `actions_gateway_eviction_retries_total{tier="classic"}` starts moving. If you
  alert on it being flat, or on `EvictionRetriesExhausted` events, expect first
  signal from clusters that evict workers. A rise here is the fix working, not a new
  fault.
- New worker pods carry the run-identity annotations. Existing pods are not
  back-filled; they are replaced as jobs turn over.

The budget is shared per run across both tiers, so a run whose workers span tiers now
consumes slots from the classic side too.

**Rolling back** re-arms the gap: a rolled-back AGC returns to reading the variables,
and classic evictions stop being re-run again, silently. There is no configuration
that restores the behaviour on an older image.

### BREAKING (pre-GA): `capacityGate.mode` values replaced by `Observe` + a gateway-level cluster fact

**Who is affected:** only a `RunnerSet` that set `spec.capacityGate.mode` to
`SchedulerVerdict`, `AutoscalerVerdict`, or `On`. None of those values ever appeared in
a tagged release — they existed only on `main`, and the field defaults to `Off` — so an
install tracking releases has nothing to do here.

`Observe` is the name the single gating value settled on. `On` said only *that* the gate
was enabled; `Observe` says *how* it decides — from evidence an already-stuck worker pod
produced, rather than by asking. That distinction is invisible with one gating value and
load-bearing once the reserved `Probe`/`Provision` values ([Q407](https://github.com/actions-gateway/github-actions-gateway/blob/main/docs/design/appendix-g-future-enhancements.md#g16-provisioningrequest-pre-acquire-capacity-probe-check-capacity))
join the same axis by soliciting an answer instead. Renaming now cost nothing; renaming
after the value shipped would have needed a conversion shim and a deprecation window.

`Observe` is **not** an audit or dry-run tier. Every value except `Off` refuses jobs;
they differ only in how the AGC learns the cluster cannot place the pod.

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
`apply`, and controllers or CI that ignore warnings are unaffected. The AGC and GMC
receive the same warning through their Kubernetes clients but log it once per unique
message per process (deduplicated), so a controller reconciling `v2alpha1` objects
under churn does not flood its own log.

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

The GMC runs `replicas: 2` with leader election. Only one replica reconciles at any time. The outgoing leader releases its lease on shutdown, so leadership transfers in seconds during a rolling update rather than at a lease timeout. In-flight reconciliations are idempotent — the new leader re-derives state and converges without producing duplicate resources.

The active replica releases its leader lease on graceful shutdown (`--leader-elect-release-on-cancel`, on by default), so during a rollout the standby takes over within one retry period (~2s) instead of waiting out the full lease (~15s). This is why the Deployment's short `terminationGracePeriodSeconds: 10` introduces no reconcile gap. If you run on a slow or heavily loaded API server and see spurious leader-lease losses (the GMC restarting with "failed to renew lease"), widen the timing with the tunables below rather than disabling leader election:

| Flag | Default | Purpose |
|---|---|---|
| `--leader-elect-lease-duration` | `15s` | How long a candidate waits before force-acquiring a stale lease. |
| `--leader-elect-renew-deadline` | `10s` | How long the leader keeps retrying a renewal before stepping down. |
| `--leader-elect-retry-period` | `2s` | Interval between election attempts (and the failover floor with release-on-cancel). |
| `--leader-elect-release-on-cancel` | `true` | Release the lease on SIGTERM for fast failover. Leave on. |

The invariant `lease-duration > renew-deadline > retry-period × 1.2` is validated at startup; a misordered set makes the GMC exit immediately with a message naming the offending flags.

### A GMC restart costs tenants nothing; a GMC upgrade rolls every one of them

The GMC renders each tenant's AGC `Deployment` from its own configuration, so what a GMC replacement costs tenants turns entirely on whether that configuration changed.

**Restart, failover, eviction, node drain — no tenant impact.** A replacement leader re-derives every AGC pod template identically and writes nothing, so no new `pod-template-hash` and no new `ReplicaSet` appear and no AGC pod is replaced. This holds for any restart of an unchanged GMC.

**Upgrade — every tenant's AGC rolls, all at once.** These GMC-side values are part of the AGC pod template, and a release normally changes at least the first two:

| Value in the AGC pod template | Where the GMC gets it |
|---|---|
| AGC container image | `AGC_IMAGE` on the GMC pod (chart value `agc.image`) |
| `GITHUB_RUNNER_VERSION` | compiled into the GMC binary |
| `WRAPPER_IMAGE`, `WRAPPER_DELIVERY` | GMC pod environment |
| `AGC_EXTRA_*` (testing only) | GMC pod environment, behind `--allow-agc-extra-env` |

Every tenant whose template changes rolls, so a GMC upgrade fans out into one [AGC drain window](#agc-upgrade) per tenant, concurrently and unstaged — dropped long polls and abandoned RenewJob loops across the whole fleet rather than in one namespace. Schedule a GMC upgrade in the low-traffic window you would give an AGC upgrade, and validate with the [AGC post-upgrade checks](#agc-upgrade) as well as the GMC ones.

**Do not hand-edit a GMC-managed AGC `Deployment`.** The reconciler watches the Deployments it owns and replaces the whole spec from its own render, so a `kubectl set image` — or any other direct edit — is reverted within seconds. `kubectl rollout restart` is the one exception, and its history is a trap of its own: see [`kubectl rollout restart` of a Managed Deployment Reports Success but Nothing Restarts](troubleshooting.md#kubectl-rollout-restart-of-a-managed-deployment-reports-success-but-nothing-restarts).

### GMC install and upgrade via Helm (recommended)

The shipped install artifact is the **`actions-gateway` Helm chart**, published and cosign-signed to the GHCR OCI registry (`oci://ghcr.io/actions-gateway/charts/actions-gateway`); the [`charts/actions-gateway/`](../../charts/actions-gateway/README.md) source path is the dev/CI copy of the same chart. The chart is the **sole** install/upgrade vehicle — there is no kustomize path. For dev/CI iteration `make deploy` wraps `helm install` of the local chart with floating image tags.

> **Current release — `v1.3.0`** (chart version = release tag without the leading `v`). Pin `--version 1.3.0`; copy the image digests from the [release notes](https://github.com/actions-gateway/github-actions-gateway/releases/tag/v1.3.0) and verify the chart/image signatures before installing (see [release.md § Verify the publish](release.md#3-verify-the-publish)).

```sh
# First install (from the published, signed OCI chart)
helm install gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version 1.3.0 \
  --namespace gmc-system --create-namespace \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy> \
  --set wrapper.image.digest=sha256:<wrapper>

# Apply the chart-root CRDs first — EVERY upgrade, not just some. Helm installs
# that directory on `helm install` only and skips it on upgrade, so this is the
# only way changes to those CRDs reach an existing release. Idempotent, and
# version-correct: it reads the CRDs out of the exact chart you are upgrading to.
helm show crds oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version <new-chart-version> | kubectl apply -f -

# Upgrade in place to a newer published chart version (carries CRD field changes — see below)
# --reset-then-reuse-values, NOT --reuse-values: see the values-reuse note below
helm upgrade gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version <new-chart-version> --namespace gmc-system --reset-then-reuse-values \
  --set gmc.image.digest=sha256:<new-gmc>

# Roll back to the previous release
helm rollback gag --namespace gmc-system
```

The following upgrade-time behaviors are specific to this chart:

- **Use `--reset-then-reuse-values`, not `--reuse-values`.** `--reuse-values` replays the *old* release's values over the *old* chart's defaults, so any values key introduced after your release was created is simply absent — and a template reading a field under it fails the render with an opaque Go error naming a file you did not touch:

  ```text
  Error: UPGRADE FAILED: actions-gateway/templates/vpa.yaml:1:14
    executing "actions-gateway/templates/vpa.yaml" at <.Values.vpa.enabled>:
      nil pointer evaluating interface {}.enabled
  ```

  `--reset-then-reuse-values` (Helm ≥ 3.14) starts from the *new* chart's defaults and layers your release's values on top, so new keys get their intended defaults and everything you set is preserved. It is the correct flag for every upgrade of this chart, not just ones that hit the error.

  The chart deliberately does **not** paper over this by making templates tolerate a missing block. A missing block would then render as "unset", and for a security-relevant key — `admissionPolicy`, `networkPolicy` — that silently disables a guard on upgrade. Failing the render loudly is the safe direction; reaching for the right flag is the fix. See the [troubleshooting runbook](troubleshooting.md#helm-upgrade-fails-with-nil-pointer-evaluating-interface-field).
- **All four image digests are required at render time.** Both `helm install` and `helm upgrade` fail with `<image>.image must be pinned by digest …` (naming `gmc`/`agc`/`proxy`/`wrapper`) when the release values carry no digest for one of the four — e.g. a values file that omits it, or `--reset-values`. `--reset-then-reuse-values` and `--reuse-values` both carry the previously pinned digests forward; pass `--set <image>.image.digest=sha256:<new>` for each image you are moving to the new release's build. See the [troubleshooting runbook](troubleshooting.md#helm-render-fails-gmcimage-must-be-pinned-by-digest). Dev/test only: `allowFloatingImageTags=true` opts out.
- **Most CRDs upgrade with the release; one does not, so apply the chart's CRDs every time.** The `ActionsGateway` and `RunnerGroup` CRDs ship as templates under `templates/crds/` with `helm.sh/resource-policy: keep`, so `helm upgrade` applies their additive field changes automatically and `helm uninstall` preserves them (and every tenant's objects) rather than cascade-deleting. The `PriorityClassAllowlist` CRD cannot: the same release *creates* a `PriorityClassAllowlist` object, and Helm resolves REST mappings for the whole manifest before applying any of it, so a CR whose CRD is a template in the same release fails outright. It therefore ships in the chart-root `crds/` directory — which Helm installs on `helm install` **only, never on upgrade**.

  Rather than make that a conditional "did your release predate X?" step, the upgrade command above applies the chart's CRDs unconditionally:

  ```bash
  helm show crds oci://ghcr.io/actions-gateway/charts/actions-gateway \
    --version <new-chart-version> | kubectl apply -f -
  ```

  It is idempotent (a no-op when nothing changed), needs no local checkout, and reads the CRDs from the exact chart version you are upgrading to — so it stays correct for any future schema change without a release-note callout to remember. The chart also preflights the CRD's presence and fails with this command if you skip it, so a missed step costs you a re-run, not a broken cluster. The `RunnerGroup` CRD is sourced from the AGC authoritative copy.
- **The webhook cert path depends on `certManager.enabled`.** With the default `certManager.enabled=true`, cert-manager issues and rotates the serving cert; nothing to do on upgrade. With `certManager.enabled=false`, the chart generates a self-signed serving cert and wires the webhook `caBundle` itself. On an in-place `helm upgrade` the chart **reuses the existing `webhook-server-cert` Secret** (it looks the Secret up), so the cert does not rotate; it only regenerates if that Secret is missing (a fresh install, or after you delete it to force rotation). A `helm template` (no cluster) cannot look the Secret up and therefore renders a fresh cert each time — expected for offline rendering only.
- **`priorityClassAllowlist.configMapName` is removed (Q492) — a breaking values change.** The PriorityClass allowlist moved from a watched ConfigMap to the cluster-scoped `PriorityClassAllowlist` CR, which is now both the GMC's dynamic source and the guard policy's `paramKind`. A release that sets the old key fails the render with a migration message; every other release needs no values change, because the chart renders the new object from the `allowedPriorityClasses` it already rendered the ConfigMap from. Full steps: [PriorityClass allowlist: ConfigMap to CR](#priorityclass-allowlist-configmap-to-cr) below. (The CRD itself is covered by the apply step above, like any other chart-root CRD.)
- **`allowedInfraPriorityClasses` is new (Q298) and needs no upgrade action unless you set it.** The `PriorityClassAllowlist` CR gained a second list, so the infra PriorityClass allowlist can be grown without a GMC restart like the worker one. The chart renders the key only when the value is non-empty, so an upgrade that has not adopted it applies cleanly against the CRD your *current* release installed — no ordering requirement. If you do set it, apply the chart's CRDs first (the step above): a field the stored CRD does not declare fails server-side apply **midway** through the upgrade, so the chart preflights it and fails at render with the same command instead. This is a stale *schema*, not a missing kind, which the older CRD-presence preflight cannot see.
- **A hand-patched release blocks the next `helm upgrade`.** Helm 4 applies server-side, so a field owned by a different field manager is a hard conflict rather than a silent overwrite. If you have `kubectl patch`ed or `kubectl edit`ed a chart-owned object — the GMC Deployment's container `args` is the usual one — the next upgrade fails outright:

  ```text
  Error: UPGRADE FAILED: conflict occurred while applying object gmc-system/gmc-controller-manager
  apps/v1, Kind=Deployment: Apply failed with 1 conflict: conflict with "kubectl-patch" using
  apps/v1: .spec.template.spec.containers[name="manager"].args
  ```

  Re-run the upgrade with `--force-conflicts` to hand the field back to Helm — your manual edit is reverted, which is the point of the flag. The durable fix is to express the change in chart values so it survives every upgrade instead of being reapplied by hand. Do **not** reach for `--force-replace`: it replaces objects wholesale and is destructive.
- **The `namespace-psa-guard` and `gmc-tenant-resource-guard` bindings deny by default.** If you are upgrading a cluster whose existing tenant namespaces are not yet labeled `actions-gateway.github.com/tenant=true`, label them **before** the upgrade (see the migration note above), or the GMC's namespace patches *and all tenant-resource writes* will be denied. To stage the rollout you can temporarily set both bindings to `Audit` by editing `validationActions` on each `ValidatingAdmissionPolicyBinding`, then flip them back to `Deny` once the labels are in place.

### PriorityClass allowlist: ConfigMap to CR

**Who this affects:** everyone, for step 1 — which is now simply part of every
upgrade of this chart, not a one-off. Step 2 additionally applies if you set
`priorityClassAllowlist.configMapName`.

**What changed.** The watched allowlist is now a cluster-scoped
`PriorityClassAllowlist` CR named `<release>-priorityclass-allowlist`, rendered by
the chart from `allowedPriorityClasses`. It is both the GMC's restart-free dynamic
source (Q188) and the `priorityclass-allowlist-guard` policy's `paramKind`.

**Why it had to move.** A `paramKind` on a *core* type such as `ConfigMap` is
permanently broken for the rest of a kube-apiserver process once the set of
bindings naming it goes empty for one refresh tick — which `helm uninstall` does.
That was the Q444 cluster-wide outage: every `runnergroups`/`runnersets`/
`runnertemplates` write denied with `no params found for policy binding`, and no
recovery short of a kube-apiserver restart. A CRD `paramKind` gets a fresh dynamic
informer per context and survives the same transition, so this removes the
exposure rather than mitigating it.

**Why the CRD needs a manual step.** This is the one CRD the chart ships in the
chart-root `crds/` directory rather than `templates/crds/`, because the same
release also *creates* a `PriorityClassAllowlist` object — and Helm resolves REST
mappings for the entire manifest before applying any of it, so a CR whose CRD is a
template in the same release fails the install outright. `crds/` is the only
directory Helm installs early enough. The cost is that Helm skips `crds/` entirely
on upgrade, so an existing release never receives it.

#### 1. Apply the chart's CRDs

```bash
helm show crds oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version <new-chart-version> | kubectl apply -f -
```

This is the standard pre-upgrade step for every release
([above](#gmc-install-and-upgrade-via-helm-recommended)), not something special to
this migration — running it unconditionally is what keeps the procedure the same
whatever version you are coming from. From a chart directory, `helm show crds
charts/actions-gateway | kubectl apply -f -` does the same thing.

Skip it and the upgrade stops before changing anything, naming the command:

```text
Error: UPGRADE FAILED: execution error at (actions-gateway/templates/priorityclass-allowlist.yaml):
the PriorityClassAllowlist CRD is not installed in this cluster. Helm installs the
chart-root crds/ dir on a fresh install ONLY and skips it on every upgrade, so
applying it is a standard pre-upgrade step for this chart. ...
```

The check is skipped on a fresh `helm install`, where `crds/` is applied for you.

#### 2. Move any watched-ConfigMap names into `allowedPriorityClasses`

Only if your release sets `priorityClassAllowlist.configMapName`. That key is
removed, and the render fails while it is set:

```text
priorityClassAllowlist.configMapName ("gmc-priority-class-allowlist") is removed as
of Q492. ...
```

Read the names the ConfigMap currently carries — including any added self-service
since install, which will **not** be in your values file. Use *your* configured
name, not the example (`helm get values <release> -n gmc-system` shows it under
`priorityClassAllowlist.configMapName`):

```bash
kubectl get configmap -n gmc-system <your-configmap-name> -o jsonpath='{.data.allowedPriorityClasses}'
```

Put the **union** of those and your existing `allowedPriorityClasses` into
`allowedPriorityClasses`, and unset the removed key:

```yaml
allowedPriorityClasses:
  - runner-standard      # was already in values
  - runner-bursty        # was ConfigMap-only
  - runner-batch         # was ConfigMap-only
priorityClassAllowlist:
  configMapName: ""
```

Getting this wrong fails **closed**, not open: a name you drop stops being
admitted, denying writes that name it. Nothing becomes permitted that was not
permitted before.

#### 3. Upgrade, then verify

```bash
kubectl get priorityclassallowlist gmc-priorityclass-allowlist -o jsonpath='{.spec.allowedPriorityClasses}'
```

The GMC's grant becomes a `ClusterRole` with `get`/`list`/`watch` on
`priorityclassallowlists` (the kind is cluster-scoped); it carries no write verb,
so the GMC still cannot widen its own allowlist. No manual RBAC step.

**What the upgrade cleans up depends on which case you were in.** Helm only
removes objects it rendered:

| You had | Helm removes on upgrade | You remove by hand |
|---|---|---|
| `configMapName` **unset** (default) | the chart-rendered param ConfigMap `<release>-priorityclass-allowlist` | nothing |
| `configMapName` **set** | the namespaced `Role`/`RoleBinding` the chart rendered for that watch | your own ConfigMap — the chart never rendered it, so Helm will not touch it |

So if you set `configMapName`, delete your ConfigMap once you are satisfied the CR
carries everything. Nothing reads it any more:

```bash
kubectl delete configmap -n gmc-system <your-configmap-name>
```

**Your self-service workflow moves to the CR.** Adding a class without a GMC
rollout used to mean editing that ConfigMap; it now means editing the CR, which
the GMC watches the same way:

```bash
kubectl edit priorityclassallowlist <release>-priorityclass-allowlist
```

A chart upgrade reasserts `allowedPriorityClasses` over it, exactly as it would
have reasserted a chart-rendered ConfigMap — so persist anything durable in
values. Full detail:
[security-operations.md](security-operations.md#self-service-additions-via-the-priorityclassallowlist-cr-q188-q298).

#### Rolling back past this change re-arms the outage it fixes

**`helm rollback` to a revision from before this change is not safe on a running
cluster.** It reports success, and then every `runnergroups`/`runnersets`/
`runnertemplates` write is denied cluster-wide with `no params found for policy
binding` — the exact Q444 outage this change removes.

Measured on kind v1.36.1: install `v1.2.0`, upgrade to this chart, `helm rollback`
to revision 1. Rollback succeeds, the `paramKind` reverts to `ConfigMap`, and a
class-free `RunnerGroup` write is denied on every attempt across a 20-second
window — it does not clear.

The reason is the defect itself. Upgrading to the CRD `paramKind` removes the last
binding naming `v1/ConfigMap`, which is precisely the transition that kills that
apiserver's shared ConfigMap param informer for the life of the process. The
rollback then recreates a ConfigMap-`paramKind` binding, and that informer is never
coming back — so it can never resolve, exactly as a fresh install could not after
the break.

**Roll forward, not back.** If you need to undo this release for an unrelated
reason, do it in a way that does not recreate a ConfigMap-`paramKind` binding —
upgrade to the older chart version with the guard disabled rather than rolling back
to it:

```bash
helm upgrade gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version <older-chart-version> --namespace gmc-system \
  --reset-then-reuse-values --set admissionPolicy.enabled=false
```

Verified: writes are admitted again within seconds. You lose the PriorityClass
backstop until you roll forward again, so treat it as an incident mitigation — the
GMC webhook still gates the tenant-facing CRs meanwhile. The other recovery is a
kube-apiserver restart, which is not available on EKS/GKE/AKS.

Rolling back a release that has *always* been on the CR `paramKind` is unaffected —
this is specifically about crossing the ConfigMap→CR boundary downwards.

`helm upgrade` rolls the GMC Deployment (and carries additive CRD field changes —
no separate CRD apply step). Watch the rollout:

```sh
kubectl rollout status deploy/gmc-controller-manager -n gmc-system
```

The rolling update replaces one replica at a time. Leadership transfers before the old leader is deleted. The total rollout time is typically < 30 seconds.

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

The image change itself is fleet-wide (Step 2), so the per-tenant work is the drain that precedes it and the validation that follows: run Step 1 for each tenant you want drained cleanly, then Steps 3–5 per namespace.

**Step 1: Drain the AGC before upgrading (optional, reduces blackout window)**

The AGC's SIGTERM handler calls `DELETE /sessions` for all open sessions before exiting, causing GitHub to immediately re-queue unacquired jobs rather than waiting for session TTL. The shutdown **blocks until every listener goroutine has issued its session DELETE** — the process does not exit while sessions are still open. To rely on this:

- Ensure `terminationGracePeriodSeconds` on the AGC Deployment is ≥ 30 seconds (the GMC stamps the AGC Deployment with 60s by default). The session drain is bounded at 10 seconds and runs concurrently across RunnerGroups, so it does not scale with listener count.
- Do not use `kubectl delete pod` directly — it sends SIGKILL without a grace period. Use `kubectl rollout restart`, which the reconciler honours on a managed Deployment.

If a session cannot be deleted (the broker is unreachable during the drain), the AGC retries within its budget and then logs a Warn naming the session before exiting:

```
DeleteSession failed; the broker session is leaked until it expires server-side sessionId=... attempts=3 error=...
```

That session's runner stays online on GitHub until the server-side session TTL expires. It is harmless but wastes a runner slot for the TTL; a burst of these lines during a rollout points at broker or egress-path reachability, not at the AGC.

**Step 2: Update the AGC image**

The GMC manages the AGC Deployment, so the AGC image is a GMC-level setting, not a per-namespace one: update the GMC's Helm values with the new AGC image and upgrade the GMC, which then rolls **every** tenant's AGC Deployment at once — see [A GMC restart costs tenants nothing; a GMC upgrade rolls every one of them](#a-gmc-restart-costs-tenants-nothing-a-gmc-upgrade-rolls-every-one-of-them) before you schedule it.

There is no per-namespace alternative. `kubectl set image deploy/actions-gateway-controller -n <namespace>` appears to work and is reverted on the next reconcile, because the reconciler replaces the managed Deployment's whole spec from its own render.

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

### AGC rollback

Roll the AGC image back where the GMC reads it, not on the Deployment — `kubectl rollout undo` on a managed AGC Deployment is reverted on the next reconcile, same as `kubectl set image`:

```sh
# Re-run the GMC upgrade pinning the previous AGC digest, then watch the AGCs converge
helm upgrade gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version <chart-version> --namespace gmc-system --reset-then-reuse-values \
  --set agc.image.digest=sha256:<previous-agc>
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

### Proxy rollback

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

### Worker image rollback

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

← [Back to Operations](README.md)
