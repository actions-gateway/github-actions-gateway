# Running GAG Alongside a Service Mesh

> **Audience:** Platform engineer

A service mesh (Istio, Linkerd, Cilium Service Mesh, Kuma) is the single most common source of *silent* breakage for GitHub Actions Gateway (GAG).
The mesh and GAG both make assumptions about the worker pod's lifecycle and its outbound network path, and those assumptions collide in two ways:

1. **An injected sidecar stops a run-to-completion worker pod from ever terminating** — the job finishes, but the mesh proxy keeps the pod `Running`, so the worker slot is never released.
   Capacity leaks one slot per job until the RunnerGroup wedges at `maxWorkers`.
2. **Mesh egress interception fights GAG's per-tenant egress proxy** — double-proxying, mutual-TLS (mTLS) handshake confusion, and a loss of the per-tenant egress-IP attribution that isolates one tenant's GitHub traffic from another's.

Both failures are quiet: pods schedule, the mesh reports healthy, and the first symptom is "jobs stop getting picked up after a while" or "GitHub sees the wrong egress IP."
This guide explains why the collision happens and gives the concrete configuration that resolves it for each mesh.

If you read nothing else, read this: **the supported, lowest-risk coexistence posture is to opt the GAG tenant namespace out of the mesh entirely.** One label resolves both problems at once, and it costs you nothing GAG-specific — see [The one-label answer](#the-one-label-answer).

> **Validation status.** The failure analysis, the [namespace opt-out](#the-one-label-answer), and the in-mesh mitigations below were exercised end-to-end on a live **kind** cluster (Kubernetes 1.35).
> All four meshes/modes named here have now been run through a real GAG job in a meshed tenant namespace: **Istio 1.30.2** in both sidecar (Q220) and **ambient/ztunnel** (Q280) modes, **Linkerd edge-26.6.3** sidecar (Q220, plus the `skip-outbound-ports` fix confirmed in Q280), and **Cilium 1.19.5** as the CNI/sidecar-less mesh (Q280).
> The exercise confirmed the two hazards, surfaced two hard prerequisites the earlier draft omitted ([in-mesh prerequisites](#two-hard-prerequisites-for-any-in-mesh-gag-namespace): mesh CNI for Pod Security, and a NetworkPolicy that reaches the mesh control plane), and pinned down the one path every mesh breaks.
> **That path is the worker/AGC → per-tenant-proxy HTTPS `CONNECT` leg** (port `8080`): the mesh proxy wraps it in mesh mTLS (Istio sidecar/ztunnel HBONE, Linkerd mTLS) and collides with the per-tenant proxy's own TLS, resetting the connection.
> Plain in-cluster HTTP (e.g. the AGC's control-plane calls to a same-cluster endpoint) survives; GitHub-bound egress, which all tunnels through that proxy leg, does not.
> Mesh knob names and defaults drift between releases (notably: modern Istio and Linkerd now inject **native sidecars by default** — see [Fix B](#fix-b--native-sidecars-kubernetes-128)), so confirm against your own mesh version.
> **The opt-out remains the recommended default, and the validation made the in-mesh path look *more* involved, not less** — enrolling the GAG namespace in Istio ambient breaks the proxy leg with no explicit policy at all, and Linkerd needs an extra `skip-outbound-ports` annotation to restore it (see [the Linkerd recipe](#linkerd)).

---

## Background: what GAG assumes about a worker pod

Two design facts drive everything below.
Both are covered in depth in the [architecture overview](../design/02-architecture.md) and the [security model](../design/05-security.md); the short version:

- **Worker pods run to completion and are reaped by phase.** Each acquired job gets one ephemeral worker pod (a bare `Pod`, not a `Job` or `Deployment`).
  When the runner container exits, the pod reaches a *terminal phase* (`Succeeded` or `Failed`).
  The AGC frees the worker slot when it observes that terminal phase, and the reaper deletes the pod once [`completedPodTTL`](../design/03-api-contracts.md) elapses.
  A pod that never reaches a terminal phase is never reaped by the completed-pod path and holds its slot indefinitely.
  (The `pendingPodDeadline` reaper only covers pods stuck in `Pending`, not a pod stuck in `Running`.
  On the ScaleSet protocol a pod still `Running` five minutes after GitHub reports its job terminal is reaped as a backstop — see [WorkerPodOrphanedRunning](troubleshooting.md#worker-pod-reaped-while-running-workerpodorphanedrunning) — but the pod is killed rather than completing, so the sidecar still has to be fixed.)

- **All GitHub-bound egress is funnelled through the per-tenant proxy.** The GMC injects `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` into the AGC and every worker pod.
  GitHub traffic `CONNECT`-tunnels through the per-tenant proxy `Service` (`actions-gateway-proxy.<namespace>.svc.cluster.local:8080` by default), which egresses on the tenant's dedicated IP(s).
  That per-tenant egress IP **is** the tenant-isolation boundary for outbound traffic ([§5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped)).
  The proxy does `CONNECT` tunnelling only — it never terminates TLS.

A mesh sidecar breaks the first assumption (it is an extra long-lived container) and the second (it transparently re-routes the pod's outbound TCP).

> **The GAG tenant namespace is GAG-only.** A tenant namespace holds the AGC, the proxy pool, ephemeral workers, and their Secrets — nothing else.
> You are not meant to co-locate unrelated application workloads there (see [tenant onboarding](tenant-onboarding.md)).
> That is what makes the namespace-wide opt-out below cheap: you are not removing some *other* app from the mesh, only GAG's own components, none of which need it.

---

## Why per-pod `podTemplate` annotations will not save you

The instinct from mesh documentation is to disable injection or add egress exclusions **per pod**, via annotations on the workload's pod template (`sidecar.istio.io/inject: "false"`, `traffic.sidecar.istio.io/excludeOutboundPorts`, `linkerd.io/inject: disabled`, …).
**This does not work for GAG worker pods.**

The AGC builds each worker pod from the tenant's `podTemplate`, but it does **not** copy arbitrary template metadata onto the pod.
Labels are rebuilt from a fixed recommended set plus GAG's owner-identity labels, and only three node-disruption-safety annotation keys are honored from the template — every other annotation you put in `podTemplate.metadata.annotations` is dropped.
So a mesh injection-control or egress-exclusion annotation placed on the RunnerGroup `podTemplate` never reaches the worker pod.

That leaves the **namespace** as the only reliable lever for worker pods, which is exactly why the recommended posture is namespace-scoped.
(Tracking the option to propagate an allowlisted set of mesh annotations onto worker pods is filed as a future enhancement; until then, configure the mesh at the namespace and the mesh-install level, never on the RunnerGroup `podTemplate`.)

---

## The one-label answer

For the overwhelming majority of operators, the correct coexistence posture is to **exclude the GAG tenant namespace from the mesh**.
No sidecar means no stranded slots (problem 1) and no traffic interception (problem 2) — both conflicts disappear together, and GAG keeps providing its own per-tenant egress identity and its own metrics mTLS, so you lose nothing the mesh would have given you here.

| Mesh | Opt the namespace out |
|---|---|
| **Istio (sidecar mode)** | `kubectl label namespace <tenant-ns> istio-injection=disabled` — and, for a *revisioned* install, also ensure the namespace carries **no** `istio.io/rev` label (`kubectl label namespace <tenant-ns> istio.io/rev-`). |
| **Istio (ambient mode)** | Do **not** label the namespace `istio.io/dataplane-mode=ambient`. If a broader default enrolls it, set `kubectl label namespace <tenant-ns> istio.io/dataplane-mode=none`. |
| **Linkerd** | Injection is opt-in, so by default do nothing. If a parent scope injects, set `kubectl annotate namespace <tenant-ns> linkerd.io/inject=disabled`. |
| **Cilium Service Mesh** | Sidecar-less by design (per-node proxy); no per-pod injection to disable. Confirm no `CiliumEnvoyConfig` / L7 policy redirects the proxy's `:8080` egress (see [problem 2](#problem-2-mesh-egress-interception-vs-the-per-tenant-proxy)). |
| **Kuma** | `kubectl annotate namespace <tenant-ns> kuma.io/sidecar-injection=disabled`. |

Apply the label **before** the `ActionsGateway` is reconciled (or roll the AGC, proxy, and any in-flight workers afterward) so existing pods are recreated without a sidecar.

If your organization mandates mesh membership for *every* namespace and a blanket opt-out is not allowed, read on: first the two [in-mesh prerequisites](#two-hard-prerequisites-for-any-in-mesh-gag-namespace) that a meshed GAG namespace cannot start without, then [problem 1](#problem-1-sidecars-strand-worker-slots) and [problem 2](#problem-2-mesh-egress-interception-vs-the-per-tenant-proxy) with the per-mesh mitigations.

---

## Two hard prerequisites for any in-mesh GAG namespace

Before the lifecycle and egress fixes below matter at all, an in-mesh GAG namespace must clear two gates that GAG's own hardening puts in the sidecar's way.
Both were found during live validation (Q220); miss either and the tenant never starts — the symptom is *"no pods"* or *"pods stuck at `Init`"*, not a slow leak.

### 1. Pod Security Admission rejects the classic proxy-init container — use the mesh CNI

The GMC stamps `pod-security.kubernetes.io/enforce=baseline` on every tenant namespace.
A mesh's **classic** sidecar injection adds an init container (`istio-init`, `linkerd-init`) that requests the `NET_ADMIN` + `NET_RAW` capabilities to program the iptables redirect.
`baseline` forbids those capabilities, so the apiserver **rejects the AGC, proxy, and worker pods at creation** — the Deployments sit at zero replicas with `FailedCreate` events and no pod (hence no sidecar, and none of Problem 1's stranding) ever exists:

```
Error creating: pods "actions-gateway-controller-…" is forbidden: violates PodSecurity
"baseline:latest": non-default capabilities (container "istio-init" must not include
"NET_ADMIN", "NET_RAW" in securityContext.capabilities.add)
```

The fix is the mesh's **CNI plugin**, which programs the redirect at the node level so no privileged init container is injected (a capability-free `istio-validation` / `linkerd-network-validator` init runs instead, which `baseline` allows):

| Mesh | Do this |
|---|---|
| **Istio** | Install the `istio/cni` chart **and** tell istiod about it (`--set pilot.cni.enabled=true` on the `istio/istiod` chart). Verified: without `pilot.cni.enabled`, istiod keeps injecting the privileged `istio-init` even with the CNI daemonset running. |
| **Linkerd** | Install the `linkerd2-cni` chart **and** set `cniEnabled=true` on the `linkerd-control-plane` chart. |
| **Cilium / ambient / any sidecar-less mesh** | No per-pod proxy, no init container — this gate does not apply. |

The alternative — relaxing the namespace to `enforce=privileged` — is a Pod Security regression and is **not** recommended.
Prefer the CNI, or [opt out](#the-one-label-answer).

### 2. GAG's default-deny egress NetworkPolicy blocks the sidecar's control-plane path

Even once pods are admitted (CNI in place), the sidecar has to reach its own control plane to get configuration and a workload certificate.
GAG's per-tenant NetworkPolicies are **default-deny egress** — they permit only DNS, the per-tenant proxy, the kube-API, and whatever additive rules you add.
They do **not** anticipate a mesh control plane, so the sidecar's connection is silently dropped, it never becomes Ready, and the pod hangs at `Init` (native sidecar) or never-Ready (classic) — the AGC never registers a session:

```
# Istio: the native-sidecar proxy cannot reach istiod
failed to warm certificate: … dial tcp <istiod>:15012: i/o timeout
# Linkerd: the proxy cannot reach destination/identity/policy
linkerd-identity-headless.linkerd.svc.cluster.local:8080: service in fail-fast
```

You must add an **additive** NetworkPolicy that lets the GAG pods reach the mesh control plane.
Additive policies only *add* an allowed path; they do not weaken the deny-by-default for everything else:

```yaml
# Istio — allow egress to istiod (xDS + CA). Verified on Istio 1.30.2.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: allow-istiod-egress, namespace: <tenant-ns>}
spec:
  podSelector: {}
  policyTypes: [Egress]
  egress:
  - to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: istio-system}}}]
    ports: [{port: 15012, protocol: TCP}, {port: 15010, protocol: TCP}]
---
# Linkerd — allow egress to destination/identity/policy. Verified on edge-26.6.3.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: allow-linkerd-egress, namespace: <tenant-ns>}
spec:
  podSelector: {}
  policyTypes: [Egress]
  egress:
  - to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: linkerd}}}]
    ports: [{port: 8086, protocol: TCP}, {port: 8080, protocol: TCP}, {port: 8090, protocol: TCP}]
```

(Also confirmed in passing: the tenant namespace must carry `actions-gateway.com/tenant=managed` for the GMC to reconcile it at all — this is normal [tenant onboarding](tenant-onboarding.md), not mesh-specific, but a fresh meshed namespace created by hand will otherwise be refused by the GMC's namespace-PSA admission policy.)

---

## Problem 1: sidecars strand worker slots

### What happens

A classic sidecar (Istio's `istio-proxy`, Linkerd's `linkerd-proxy`) is injected as an ordinary container.
The kubelet considers a pod's phase terminal only when **all** its containers have exited.
The runner container exits when the job finishes — but the mesh proxy is built to run forever, so it keeps running.
The pod stays `Running`:

```
NAME                         READY   STATUS    RESTARTS   AGE
runner-builders-a1b2c3       1/2     Running   0          14m   # job done 12m ago; istio-proxy still up
```

GAG never sees a terminal phase, so it never frees the slot.
With `maxWorkers: N`, after `N` jobs every slot is held by a done-but-`Running` pod and **no new job is ever admitted**.
This is the top silent-breakage mesh users hit.
(Reproduced on Istio 1.30.2 with `ENABLE_NATIVE_SIDECARS=false` — Q220: the `runner` container exited but `istio-proxy` stayed `running`, leaving the pod at `READY 1/2` and non-terminal 90 s+ after the job finished.)

> **Why the reaper does not save you.** `completedPodTTL` only deletes pods that reached a *terminal* phase, and `pendingPodDeadline` only deletes pods stuck `Pending`.
> A sidecar-pinned pod is neither — it is `Running` — so it falls through both phase-based reaper paths.
> ScaleSet sets get a backstop ([`WorkerPodOrphanedRunning`](troubleshooting.md#worker-pod-reaped-while-running-workerpodorphanedrunning): a pod still `Running` five minutes after GitHub reports the job terminal is deleted), which stops the slots leaking — but it kills the pod instead of letting it complete, and Classic sets have no equivalent.
> The fix still has to make the pod actually terminate.

### Fix A — opt out (recommended)

[The one-label answer](#the-one-label-answer).
No sidecar, no stranding.
Stop here unless mesh membership is mandatory.

### Fix B — native sidecars (Kubernetes 1.28+)

If the namespace must stay in the mesh, run the proxy as a **native sidecar** — a Kubernetes *sidecar container* (an init container with `restartPolicy: Always`, the `SidecarContainers` feature, GA in Kubernetes 1.29).
The kubelet treats native sidecars specially: once all *regular* containers exit, it terminates the native sidecars and the pod reaches a terminal phase.
A run-to-completion worker pod then completes normally and GAG reaps it.

**Good news from validation (Q220): current meshes do this by default.** On Istio 1.30.2 the proxy was injected as a native sidecar with `ENABLE_NATIVE_SIDECARS` *unset*, and Linkerd edge-26.6.3 likewise injected `linkerd-proxy` with `restartPolicy: Always` by default.
So on a recent mesh you typically get Fix B for free — the knobs below matter for *older* meshes or for confirming the mode, and `ENABLE_NATIVE_SIDECARS=false` is what *re-introduces* the stranding bug:

- **Istio** — native sidecars are the default on ≥ 1.28.
  To force it (older releases): `values.pilot.env.ENABLE_NATIVE_SIDECARS=true`.
  Requires Kubernetes ≥ 1.28.
- **Linkerd** — native sidecars are the default on recent edge releases; `--set proxy.nativeSidecar=true` forces it on 2.16+.
  Requires Kubernetes ≥ 1.28.

> **Verified (Istio 1.30.2, k8s 1.35).** A native-sidecar worker pod terminated cleanly: the `runner` container exited, and the kubelet terminated the `istio-proxy` native sidecar **one second later** (exit 0, `Completed`), so the pod reached a terminal phase and GAG reaped it — no stranding.
> The inverse (`ENABLE_NATIVE_SIDECARS=false`) left the pod `Running` indefinitely with `istio-proxy` still up 90 s+ after the runner exited, reproducing [Problem 1](#problem-1-sidecars-strand-worker-slots) exactly.

> **Fix B does not work on its own.** A meshed namespace still needs both [in-mesh prerequisites](#two-hard-prerequisites-for-any-in-mesh-gag-namespace) (the mesh CNI for Pod Security, and a control-plane egress NetworkPolicy) or no worker pod is ever admitted / the sidecar never starts.
> Native sidecars also fix termination, **not** interception — a native-sidecar proxy still transparently intercepts the worker's outbound traffic, so also read [problem 2](#problem-2-mesh-egress-interception-vs-the-per-tenant-proxy).

### Fix C — sidecar-less / ambient mesh

Sidecar-less meshes never inject a per-pod proxy, so there is nothing to strand a worker pod:

- **Istio ambient mode** — L4 is handled by a per-node `ztunnel`, not a per-pod sidecar.
  Worker pods run to completion untouched.
  Enroll *other* namespaces in ambient with `istio.io/dataplane-mode=ambient`; simply leave the GAG namespace unlabeled (or `istio.io/dataplane-mode=none`) and still apply the [egress consideration](#istio--ambient-mode) for ztunnel.
- **Cilium Service Mesh** — sidecar-less (per-node Envoy).
  No injection to disable; verify no L7 redirect captures the proxy's egress.

Ambient is the preferred coexistence story when you want *some* mesh presence in the cluster but cannot afford the worker-pod lifecycle conflict.

> **Scope of validation.** Sidecar mode (Istio + Linkerd) was validated in Q220; ambient (`ztunnel`) and Cilium Service Mesh were validated end-to-end in Q280.
> **Lifecycle held in every sidecar-less case** — a per-node L4 proxy adds no per-pod container, so worker pods reached a terminal phase and were reaped (Istio ambient ~3 s, Cilium ~2 s).
> Ambient satisfies the [Pod Security prerequisite](#two-hard-prerequisites-for-any-in-mesh-gag-namespace) natively (ztunnel is per-node, no privileged pod init container) and needs **no** control-plane egress NetworkPolicy (the pod's default-deny egress doesn't have to reach istiod).
> **But egress is a different story:** enrolling the GAG namespace in Istio ambient *breaks* the worker→proxy `CONNECT` leg (ztunnel HBONE vs the proxy's TLS → connection reset), so ambient is safe for GAG **only when the namespace is left out of the dataplane** — see [the ambient recipe](#istio--ambient-mode).
> Cilium's base CNI is safe in both dimensions by default (no L7 redirect) and genuinely enforces the egress deny — see [the Cilium recipe](#cilium-service-mesh).

### Fix D — proxy-shutdown hooks (last resort)

Classic meshes offer ways to ask the sidecar to exit when the main workload finishes — Istio's `EXIT_ON_ZERO_ACTIVE_CONNECTIONS=true` plus a `POST localhost:15020/quitquitquit` at job end, or Linkerd's `linkerd-await --shutdown -- <cmd>` wrapper, or `config.alpha.linkerd.io/proxy-await`.
**These require cooperating with the workload's entrypoint** — the main process must signal the proxy as it exits.
GAG owns the worker entrypoint (it runs the GitHub Actions runner), and these hooks cannot be injected through the RunnerGroup `podTemplate` ([why](#why-per-pod-podtemplate-annotations-will-not-save-you)), so this path is **not** first-class for GAG.
Prefer Fix A, B, or C. Documented here only so you recognize these knobs when you see them in mesh guides — they are the wrong tool for GAG worker pods.

---

## Problem 2: mesh egress interception vs the per-tenant proxy

### What happens

A sidecar transparently redirects the pod's outbound TCP into the mesh proxy (iptables/eBPF capture to `istio-proxy:15001` / `linkerd-proxy`).
GAG has already pointed the worker at its per-tenant egress proxy via `HTTPS_PROXY`.
The two layers now fight:

- **Double-proxying.** The worker's connection to GAG's proxy `Service` is itself captured by the mesh proxy, which then forwards to GAG's proxy, which tunnels to GitHub.
  Extra hop, extra latency, and a second policy-enforcement point that knows nothing about GAG's intent.
- **mTLS confusion.** The mesh tries to wrap the worker→GAG-proxy leg in mesh mTLS, but GAG's proxy speaks plain `CONNECT` (with its own per-tenant CA on the tunnelled leg, see [§5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped)), not mesh mTLS.
  The handshake fails or falls back unpredictably.
- **Broken egress isolation.** If the mesh routes the GitHub-bound traffic out through its own *egress gateway* (or a `ServiceEntry`), the packets leave on the mesh's egress IP instead of the tenant's proxy IP.
  The per-tenant egress-IP attribution that isolates tenants is silently lost — the most damaging failure, because nothing errors; the traffic just exits from the wrong identity.

### Fix: exclude GAG's proxy path from interception

The clean fix is the same namespace opt-out as problem 1 — no sidecar, no interception.
If the namespace must stay meshed (e.g. you are using [native sidecars](#fix-b--native-sidecars-kubernetes-128) for the lifecycle fix), you must additionally tell the mesh to **leave GAG's egress path alone** so the per-tenant proxy is honored.
Exclude the proxy `Service` port (`8080`) and/or the cluster service CIDR from outbound capture.

The canonical exclusions are **pod-level annotations**, which GAG strips from worker pods ([why](#why-per-pod-podtemplate-annotations-will-not-save-you)).
So for an in-mesh worker namespace you configure exclusions at the **mesh-install / revision level**, not per pod.
Per-mesh recipes follow.

---

## Per-mesh recipes

> These recipes are the in-mesh path.
> Each was validated end-to-end on the versions noted in its section (Q220/Q280), but mesh knob names, values, and version floors drift between releases — verify against your own mesh release before relying on them.
> The [namespace opt-out](#the-one-label-answer) remains the recommended default.

### Istio — sidecar mode (in-mesh, if opt-out is not allowed)

Validated end-to-end on Istio 1.30.2 / k8s 1.35 (Q220).
The full working sequence:

0. **Prerequisites** ([details](#two-hard-prerequisites-for-any-in-mesh-gag-namespace)) — without both, the tenant never starts:
   - Install the Istio **CNI** (`istio/cni` chart) and set `pilot.cni.enabled=true` on istiod, so the `NET_ADMIN` `istio-init` container is not injected (Pod Security).
   - Add the **istiod-egress NetworkPolicy** (allow `istio-system` `:15012`/`:15010`) so the sidecar can reach istiod for config + certs.
1. Lifecycle: native sidecars (the Istio ≥ 1.28 default) let workers terminate — verify with [Fix B](#fix-b--native-sidecars-kubernetes-128); do **not** set `ENABLE_NATIVE_SIDECARS=false`.
2. Egress: with no egress `Gateway`/`ServiceEntry` in play, the sidecar transparently forwards the worker→proxy CONNECT and GAG's per-tenant egress path is **preserved** — validation confirmed a meshed, workload-labeled pod reaching GitHub *through the tenant proxy* returned HTTP 200, identical to a sidecar-excluded baseline.
   Excluding GAG's proxy port (`8080`) and the cluster service CIDR from outbound capture is therefore an **optimization** (it skips the extra Envoy hop and the mTLS-wrap of the worker→proxy leg), not a correctness requirement.
   The per-pod form (shown for reference) is:

   ```yaml
   # Reference only — GAG drops these from the worker podTemplate.
   metadata:
     annotations:
       traffic.sidecar.istio.io/excludeOutboundPorts: "8080"        # GAG proxy CONNECT port
       traffic.sidecar.istio.io/excludeOutboundIPRanges: "10.96.0.0/12"  # your cluster service CIDR
   ```

   Because the annotation cannot reach the worker pod, set the equivalent default on the injection template for this revision via the `istio-sidecar-injector` `ConfigMap` (`values.global.proxy.excludeOutboundPorts` / `excludeIPRanges`), scoped to the revision that meshes the GAG namespace.
   Confirm your cluster service CIDR first:

   ```sh
   kubectl cluster-info dump | grep -m1 service-cluster-ip-range
   ```

3. Do **not** define an egress `Gateway` / `ServiceEntry` that claims GitHub hostnames for the GAG namespace — that is precisely the path that would strip the per-tenant egress IP.

> Even with all of the above, sidecar mode is strictly more fragile than the [namespace opt-out](#the-one-label-answer) or [ambient](#istio--ambient-mode).
> Use it only when policy forbids the simpler options.

### Istio — ambient mode

Validated end-to-end on Istio 1.30.2 / k8s 1.35 (Q280).
Ambient is sidecar-less, so problem 1 does not arise — a meshed worker pod stays single-container and reached a terminal phase in ~3 s, reaped normally.
**The catch is egress.** Keep the GAG namespace **out** of the ambient data plane so `ztunnel` does not capture the worker→proxy leg:

```sh
# Leave the GAG namespace unlabeled, or explicitly:
kubectl label namespace <tenant-ns> istio.io/dataplane-mode=none
```

> **Validated: opt-out is required, not merely preferred (Q280).** With the tenant namespace *enrolled* (`istio.io/dataplane-mode=ambient`), the AGC still started and registered listeners (its plain in-cluster HTTP survives ztunnel, and ambient needs **no** control-plane egress NetworkPolicy — ztunnel is per-node, not a pod sidecar), but the worker→proxy HTTPS `CONNECT` leg was **reset** (`curl (35) Recv failure: Connection reset by peer`, HTTP 000): ztunnel wraps the L4 leg in HBONE/mTLS, which collides with the per-tenant proxy's own TLS.
> This happened with **no** waypoint, `CiliumEnvoyConfig`, or explicit egress policy in play — plain ztunnel capture is enough to break it.
> Setting `istio.io/dataplane-mode=none` and rolling the pods restored the proxy egress to HTTP 200.
> So do not enroll the GAG namespace in ambient; leave it out of the dataplane and verify the egress path as [below](#verifying-coexistence).

### Linkerd

Injection is **opt-in** and driven by an **annotation** (not a label like Istio's `istio-injection`), so a GAG namespace gets no proxy unless something enrolls it.

- Default: do nothing.
  Confirm there is no inherited `linkerd.io/inject=enabled` **annotation**.
- If a parent scope injects, opt the namespace out:

  ```sh
  kubectl annotate namespace <tenant-ns> linkerd.io/inject=disabled
  ```

> **Validation (Q220 + Q280): Linkerd in-mesh coexistence needs one extra annotation, and the opt-out is still strongly preferred.** On Linkerd edge-26.6.3 / k8s 1.35, after clearing both [in-mesh prerequisites](#two-hard-prerequisites-for-any-in-mesh-gag-namespace) — the `linkerd2-cni` chart + `cniEnabled=true` for Pod Security, and a control-plane egress NetworkPolicy for `linkerd-dst:8086` / `linkerd-identity:8080` / `linkerd-policy:8090` — **lifecycle is fine**: `linkerd-proxy` injects as a native sidecar (the edge default) and worker pods reached a terminal phase in ~7 s and were reaped.
> The problem is the **per-tenant-proxy HTTPS `CONNECT` leg** (port `8080`): `linkerd-proxy` mTLS-wraps that outbound connection, which collides with the proxy's own per-tenant TLS and **resets it** (Q280 A/B: without the fix, `curl -x https://actions-gateway-proxy:8080` → HTTP 000, `curl (35) Connection reset by peer`).
> Because every GitHub-bound request tunnels through that proxy leg, this is what Q220 saw as the AGC's installation-token fetch failing (`POST access_tokens returned 504`) — the fetch to api.github.com goes through the proxy.
> (Plain in-cluster HTTP to a *same-cluster* endpoint is unaffected — Linkerd proxies it fine.)
> The same knobs Istio needs (native sidecars; PSA + NP prerequisites) all apply, plus you must stop Linkerd from capturing the proxy egress port:

  ```sh
  # Namespace-level default applied to injected pods. Skip Linkerd's outbound capture of the
  # per-tenant proxy CONNECT port (8080) — every GitHub-bound request tunnels through it.
  kubectl annotate namespace <tenant-ns> config.linkerd.io/skip-outbound-ports=8080
  ```

  As with Istio, the per-pod `config.linkerd.io/skip-outbound-ports` annotation does **not** reach GAG worker pods if set on the `podTemplate`; apply it at the **namespace** so Linkerd's injector picks it up (confirmed: the annotation lands on the injected pods and reprograms the outbound redirect).
  **Confirmed live (Q280):** with `skip-outbound-ports=8080` set and the pods rolled, the same proxy-egress check returns **HTTP 200**.
  Port `8080` alone suffices in the proxied model because all GitHub-bound egress `CONNECT`-tunnels through the per-tenant proxy on that port; if you run the proxy on a non-default port, skip that port instead.
  Prefer the [namespace opt-out](#the-one-label-answer) unless mesh membership is mandatory.

### Cilium Service Mesh

Validated end-to-end on Cilium 1.19.5 / k8s 1.35 (Q280), as the cluster CNI.
Sidecar-less (per-node Envoy), so **neither problem arises by default** — the worker pod stayed single-container and reached a terminal phase in ~2 s (lifecycle), and the worker→proxy `CONNECT` egress returned HTTP 200 (egress preserved), with no mesh-specific configuration.
Cilium also genuinely **enforces** the tenant's default-deny egress: a workload-labeled probe's direct (non-proxied) egress to GitHub timed out, so traffic can only leave via the per-tenant proxy and the egress-isolation boundary holds — unlike kindnet, which does not enforce it.

The only thing to check is that no L7 `CiliumEnvoyConfig` / `CiliumClusterwideEnvoyConfig` or L7 `CiliumNetworkPolicy` redirects the worker's `:8080` egress or the GitHub-bound `CONNECT` into an Envoy listener — that would reintroduce the same interception Istio/Linkerd exhibit.
If you use Cilium for egress control, prefer expressing the GitHub allowlist as `toFQDNs`/`toCIDR` policy that *complements* GAG's proxy rather than intercepting it (see the [network architecture](../design/network-architecture.md) and the [CNI-native FQDN egress](../design/05-security.md) direction).

### Generic pattern (any mesh)

For a mesh not listed above, apply the same rules, in order:

0. **Admission** — the sidecar must be *creatable* and *startable*: use the mesh's node-level CNI so no `NET_ADMIN` proxy-init container is injected (GAG enforces `baseline` Pod Security), and add a NetworkPolicy that lets the sidecar reach the mesh control plane (GAG's egress is default-deny).
   See [the two prerequisites](#two-hard-prerequisites-for-any-in-mesh-gag-namespace).
1. **Lifecycle** — guarantee the worker pod reaches a terminal phase: prefer namespace injection opt-out; else a native-sidecar (Kubernetes 1.28+) mode (now the default on current Istio/Linkerd); else a sidecar-less/ambient data plane.
   Never rely on a classic sidecar that runs forever.
2. **Egress** — confirm the worker→proxy→GitHub `CONNECT` path survives the data plane.
   This is the leg that breaks: validation showed Istio *sidecar* mode preserved it, but Istio *ambient* (ztunnel HBONE) and Linkerd (proxy mTLS) both reset it against the per-tenant proxy's own TLS.
   Exclude GAG's proxy `Service` port (`8080` by default) — and, if you run the proxy on a different port, that port — from the mesh's outbound capture, configured at the **mesh-install / namespace level** (GAG strips per-pod `podTemplate` annotations): an optimization on Istio sidecar, **mandatory** on Linkerd (`skip-outbound-ports=8080`), and unavoidable on ambient (there the only fix is to leave the namespace out of the dataplane).
   Never let a mesh egress gateway claim GitHub hostnames for the GAG namespace.
3. **Identity** — after rollout, confirm GitHub still sees the per-tenant proxy egress IP, not a mesh egress-gateway IP ([verify](#verifying-coexistence)).

---

## Verifying coexistence

After applying any of the above, confirm both problems are actually resolved.

**Lifecycle — worker pods terminate and are reaped:**

```sh
# Run a job, then watch its worker pod reach a terminal phase and disappear.
kubectl get pods -n <tenant-ns> -l actions-gateway/runner-group=<name> -w
# Expect: Running -> Succeeded/Completed -> deleted (after completedPodTTL).
# A pod that sits at READY 1/2 STATUS Running long after the job finished
# means a sidecar is still pinning it — revisit problem 1.
```

Check the container layout on a live worker.
What "correct" looks like depends on whether you opted out or stayed in-mesh:

```sh
# Regular containers:
kubectl get pod -n <tenant-ns> <worker-pod> \
  -o jsonpath='{.spec.containers[*].name}{"\n"}'
# Native-sidecar init containers (restartPolicy: Always):
kubectl get pod -n <tenant-ns> <worker-pod> \
  -o jsonpath='{range .spec.initContainers[*]}{.name}({.restartPolicy}) {end}{"\n"}'
```

- **Opted out** ([Fix A](#the-one-label-answer)): just the runner container, no proxy anywhere.
  The safest state.
- **In-mesh, native sidecar** ([Fix B](#fix-b--native-sidecars-kubernetes-128)): the proxy shows up as an **init** container with `restartPolicy: Always` (e.g.
  `istio-proxy(Always)` or `linkerd-proxy(Always)`).
  This is fine — the kubelet terminates it when the runner exits, so the pod still reaches a terminal phase.
- **In-mesh, classic sidecar (the bug):** the proxy is a **regular** container (`istio-proxy` under `.spec.containers`).
  When the runner exits, the pod sits at `READY 1/2 STATUS Running` forever — revisit [problem 1](#problem-1-sidecars-strand-worker-slots) and switch the mesh to native sidecars.

**Egress — GitHub sees the per-tenant proxy IP:** follow the egress-isolation check in the [network architecture validation](../design/network-architecture.md#how-to-validate-network-isolation) — GitHub-bound traffic must exit through the tenant proxy `Service`, and no direct or mesh-egress-gateway egress to GitHub should be observed from a worker pod.
If the observed source IP is the mesh egress gateway's, the per-tenant attribution is broken — revisit [problem 2](#problem-2-mesh-egress-interception-vs-the-per-tenant-proxy).

If jobs stop being picked up, the [troubleshooting guide](troubleshooting.md) covers the wedged-RunnerGroup symptom; a namespace in a mesh with classic sidecars is the first thing to rule out.

---

## Which fix applies to you

| Situation | Do this |
|---|---|
| You can keep the GAG namespace out of the mesh | [Opt the namespace out](#the-one-label-answer). Done — both problems gone. Validation only reinforced this as the default. |
| Mesh membership is mandatory, Kubernetes ≥ 1.28 | Satisfy **both** [in-mesh prerequisites](#two-hard-prerequisites-for-any-in-mesh-gag-namespace) (mesh CNI for Pod Security **and** a control-plane egress NetworkPolicy) or nothing starts; then rely on [native sidecars](#fix-b--native-sidecars-kubernetes-128) (the modern default) for lifecycle and confirm the [egress path](#problem-2-mesh-egress-interception-vs-the-per-tenant-proxy) end-to-end. |
| You run Linkerd and must stay in-mesh | Clear both prerequisites, then add `config.linkerd.io/skip-outbound-ports=8080` on the namespace to un-break the proxy `CONNECT` leg (Q280-confirmed: 000→200). [Prefer the opt-out](#linkerd). |
| You run Istio **ambient** | Sidecar-less lifecycle is fine, but enrolling breaks the proxy egress leg (ztunnel HBONE) — leave the GAG namespace [out of the data plane](#istio--ambient-mode) (`dataplane-mode=none`). |
| You run **Cilium** Service Mesh | Safe by default (Q280): sidecar-less lifecycle + proxy egress both work, and it enforces the egress deny. Just [confirm no L7 redirect](#cilium-service-mesh) captures `:8080`. |
| You were about to set mesh annotations on the RunnerGroup `podTemplate` | Don't — [GAG strips them](#why-per-pod-podtemplate-annotations-will-not-save-you). Configure the namespace and the mesh install instead. |

See also: [security model §5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped) (per-tenant egress proxy), [tenant onboarding](tenant-onboarding.md), [troubleshooting](troubleshooting.md), and the [network architecture](../design/network-architecture.md).
