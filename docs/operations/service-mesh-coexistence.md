# Running GAG Alongside a Service Mesh

> **Audience:** Platform engineer, SRE

A service mesh (Istio, Linkerd, Cilium Service Mesh, Kuma) is the single most
common source of *silent* breakage for GitHub Actions Gateway (GAG). The mesh
and GAG both make assumptions about the worker pod's lifecycle and its outbound
network path, and those assumptions collide in two ways:

1. **An injected sidecar stops a run-to-completion worker pod from ever
   terminating** — the job finishes, but the mesh proxy keeps the pod `Running`,
   so the worker slot is never released. Capacity leaks one slot per job until
   the RunnerGroup wedges at `maxWorkers`.
2. **Mesh egress interception fights GAG's per-tenant egress proxy** —
   double-proxying, mutual-TLS (mTLS) handshake confusion, and a loss of the
   per-tenant egress-IP attribution that isolates one tenant's GitHub traffic
   from another's.

Both failures are quiet: pods schedule, the mesh reports healthy, and the first
symptom is "jobs stop getting picked up after a while" or "GitHub sees the wrong
egress IP." This guide explains why the collision happens and gives the concrete
configuration that resolves it for each mesh.

If you read nothing else, read this: **the supported, lowest-risk coexistence
posture is to opt the GAG tenant namespace out of the mesh entirely.** One label
resolves both problems at once, and it costs you nothing GAG-specific — see
[The one-label answer](#the-one-label-answer).

> **Validation status (read before relying on the in-mesh recipes).** The
> failure analysis and the [namespace opt-out](#the-one-label-answer) are
> verified against GAG's implementation, and the opt-out needs no field
> validation — removing GAG from the mesh removes the conflict by construction.
> The **in-mesh** mitigations below (native sidecars, egress exclusions, the
> per-mesh knob names and version floors) are derived from each mesh's upstream
> documentation and have **not** been validated end-to-end against a running
> Istio or Linkerd cluster (tracked as Q220). Treat them as starting points to
> confirm in a staging cluster against your mesh version, not as a certified
> procedure — knob names and defaults drift between mesh releases. When in doubt,
> prefer the opt-out.

---

## Background: what GAG assumes about a worker pod

Two design facts drive everything below. Both are covered in depth in the
[architecture overview](../design/02-architecture.md) and the
[security model](../design/05-security.md); the short version:

- **Worker pods run to completion and are reaped by phase.** Each acquired job
  gets one ephemeral worker pod (a bare `Pod`, not a `Job` or `Deployment`).
  When the runner container exits, the pod reaches a *terminal phase*
  (`Succeeded` or `Failed`). The AGC frees the worker slot when it observes that
  terminal phase, and the reaper deletes the pod once
  [`completedPodTTL`](../design/03-api-contracts.md) elapses. A pod that never
  reaches a terminal phase is never reaped by the completed-pod path and holds
  its slot indefinitely. (The `pendingPodDeadline` reaper only covers pods stuck
  in `Pending`, not a pod stuck in `Running`.)

- **All GitHub-bound egress is funnelled through the per-tenant proxy.** The GMC
  injects `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` into the AGC and every
  worker pod. GitHub traffic `CONNECT`-tunnels through the per-tenant proxy
  `Service` (`actions-gateway-proxy.<namespace>.svc.cluster.local:8080` by
  default), which egresses on the tenant's dedicated IP(s). That per-tenant
  egress IP **is** the tenant-isolation boundary for outbound traffic
  ([§5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped)).
  The proxy does `CONNECT` tunnelling only — it never terminates TLS.

A mesh sidecar breaks the first assumption (it is an extra long-lived container)
and the second (it transparently re-routes the pod's outbound TCP).

> **The GAG tenant namespace is GAG-only.** A tenant namespace holds the AGC,
> the proxy pool, ephemeral workers, and their Secrets — nothing else. You are
> not meant to co-locate unrelated application workloads there (see
> [tenant onboarding](tenant-onboarding.md)). That is what makes the
> namespace-wide opt-out below cheap: you are not removing some *other* app from
> the mesh, only GAG's own components, none of which need it.

---

## Why per-pod `podTemplate` annotations will not save you

The instinct from mesh documentation is to disable injection or add egress
exclusions **per pod**, via annotations on the workload's pod template
(`sidecar.istio.io/inject: "false"`,
`traffic.sidecar.istio.io/excludeOutboundPorts`, `linkerd.io/inject: disabled`,
…). **This does not work for GAG worker pods.**

The AGC builds each worker pod from the tenant's `podTemplate`, but it does
**not** copy arbitrary template metadata onto the pod. Labels are rebuilt from a
fixed recommended set plus GAG's owner-identity labels, and only three
node-disruption-safety annotation keys are honored from the template — every
other annotation you put in `podTemplate.metadata.annotations` is dropped. So a
mesh injection-control or egress-exclusion annotation placed on the RunnerGroup
`podTemplate` never reaches the worker pod.

That leaves the **namespace** as the only reliable lever for worker pods, which
is exactly why the recommended posture is namespace-scoped. (Tracking the option
to propagate an allowlisted set of mesh annotations onto worker pods is filed as
a future enhancement; until then, configure the mesh at the namespace and the
mesh-install level, never on the RunnerGroup `podTemplate`.)

---

## The one-label answer

For the overwhelming majority of operators, the correct coexistence posture is
to **exclude the GAG tenant namespace from the mesh**. No sidecar means no
stranded slots (problem 1) and no traffic interception (problem 2) — both
conflicts disappear together, and GAG keeps providing its own per-tenant egress
identity and its own metrics mTLS, so you lose nothing the mesh would have given
you here.

| Mesh | Opt the namespace out |
|---|---|
| **Istio (sidecar mode)** | `kubectl label namespace <tenant-ns> istio-injection=disabled` — and, for a *revisioned* install, also ensure the namespace carries **no** `istio.io/rev` label (`kubectl label namespace <tenant-ns> istio.io/rev-`). |
| **Istio (ambient mode)** | Do **not** label the namespace `istio.io/dataplane-mode=ambient`. If a broader default enrolls it, set `kubectl label namespace <tenant-ns> istio.io/dataplane-mode=none`. |
| **Linkerd** | Injection is opt-in, so by default do nothing. If a parent scope injects, set `kubectl annotate namespace <tenant-ns> linkerd.io/inject=disabled`. |
| **Cilium Service Mesh** | Sidecar-less by design (per-node proxy); no per-pod injection to disable. Confirm no `CiliumEnvoyConfig` / L7 policy redirects the proxy's `:8080` egress (see [problem 2](#problem-2-mesh-egress-interception-vs-the-per-tenant-proxy)). |
| **Kuma** | `kubectl annotate namespace <tenant-ns> kuma.io/sidecar-injection=disabled`. |

Apply the label **before** the `ActionsGateway` is reconciled (or roll the AGC,
proxy, and any in-flight workers afterward) so existing pods are recreated
without a sidecar.

If your organization mandates mesh membership for *every* namespace and a blanket
opt-out is not allowed, read on: [problem 1](#problem-1-sidecars-strand-worker-slots)
and [problem 2](#problem-2-mesh-egress-interception-vs-the-per-tenant-proxy) cover
the in-mesh mitigations (native sidecars, ambient, egress exclusions) and their
trade-offs.

---

## Problem 1: sidecars strand worker slots

### What happens

A classic sidecar (Istio's `istio-proxy`, Linkerd's `linkerd-proxy`) is injected
as an ordinary container. The kubelet considers a pod's phase terminal only when
**all** its containers have exited. The runner container exits when the job
finishes — but the mesh proxy is built to run forever, so it keeps running. The
pod stays `Running`:

```
NAME                         READY   STATUS    RESTARTS   AGE
runner-builders-a1b2c3       1/2     Running   0          14m   # job done 12m ago; istio-proxy still up
```

GAG never sees a terminal phase, so it never frees the slot. With
`maxWorkers: N`, after `N` jobs every slot is held by a done-but-`Running` pod
and **no new job is ever admitted**. This is the top silent-breakage mesh users
hit.

> **Why the reaper does not save you.** `completedPodTTL` only deletes pods that
> reached a *terminal* phase, and `pendingPodDeadline` only deletes pods stuck
> `Pending`. A sidecar-pinned pod is neither — it is `Running` — so it falls
> through both reaper paths. The fix has to make the pod actually terminate.

### Fix A — opt out (recommended)

[The one-label answer](#the-one-label-answer). No sidecar, no stranding. Stop
here unless mesh membership is mandatory.

### Fix B — native sidecars (Kubernetes 1.28+)

If the namespace must stay in the mesh, run a mesh version that injects its proxy
as a **native sidecar** — a Kubernetes *sidecar container* (an init container
with `restartPolicy: Always`, the `SidecarContainers` feature, GA in Kubernetes
1.29). The kubelet treats native sidecars specially: once all *regular*
containers exit, it terminates the native sidecars and the pod reaches a terminal
phase. A run-to-completion worker pod then completes normally and GAG reaps it.

- **Istio** — enable on `istiod`: set `ENABLE_NATIVE_SIDECARS=true`
  (`values.pilot.env.ENABLE_NATIVE_SIDECARS=true` in the Helm install, or the
  equivalent `IstioOperator` `pilot.env`). Requires Kubernetes ≥ 1.28. The proxy
  is then injected as a native sidecar cluster-wide; existing meshed Jobs benefit
  too.
- **Linkerd** — install (or upgrade) with `--set proxy.nativeSidecar=true`
  (Linkerd 2.16+ / recent edge releases). Requires Kubernetes ≥ 1.28.

> **Not yet validated against a running mesh (Q220).** The mechanism is sound —
> GAG worker pods are `RestartPolicy: Never` with a single regular container, so
> the kubelet should terminate the native-sidecar proxy once the runner exits —
> but the exact flag/value names and version floors above are from upstream docs,
> not a tested run. Confirm against your mesh version in staging, and watch a
> completed worker pod actually reach `Succeeded`/`Failed` (see
> [Verifying coexistence](#verifying-coexistence)) before trusting it in
> production.

> **Native sidecars fix termination, not interception.** A native-sidecar proxy
> still transparently intercepts the worker's outbound traffic, so you must
> *also* apply the [egress exclusions](#problem-2-mesh-egress-interception-vs-the-per-tenant-proxy)
> below, or GAG's per-tenant egress proxy is bypassed. Two problems, two fixes.

### Fix C — sidecar-less / ambient mesh

Sidecar-less meshes never inject a per-pod proxy, so there is nothing to strand a
worker pod:

- **Istio ambient mode** — L4 is handled by a per-node `ztunnel`, not a per-pod
  sidecar. Worker pods run to completion untouched. Enroll *other* namespaces in
  ambient with `istio.io/dataplane-mode=ambient`; simply leave the GAG namespace
  unlabeled (or `istio.io/dataplane-mode=none`) and still apply the
  [egress consideration](#istio--ambient-mode) for ztunnel.
- **Cilium Service Mesh** — sidecar-less (per-node Envoy). No injection to
  disable; verify no L7 redirect captures the proxy's egress.

Ambient is the preferred coexistence story when you want *some* mesh presence in
the cluster but cannot afford the worker-pod lifecycle conflict.

### Fix D — proxy-shutdown hooks (last resort)

Classic meshes offer ways to ask the sidecar to exit when the main workload
finishes — Istio's `EXIT_ON_ZERO_ACTIVE_CONNECTIONS=true` plus a
`POST localhost:15020/quitquitquit` at job end, or Linkerd's
`linkerd-await --shutdown -- <cmd>` wrapper, or
`config.alpha.linkerd.io/proxy-await`. **These require cooperating with the
workload's entrypoint** — the main process must signal the proxy as it exits.
GAG owns the worker entrypoint (it runs the GitHub Actions runner), and these
hooks cannot be injected through the RunnerGroup `podTemplate`
([why](#why-per-pod-podtemplate-annotations-will-not-save-you)), so this path is
**not** first-class for GAG. Prefer Fix A, B, or C. Documented here only so you
recognize these knobs when you see them in mesh guides — they are the wrong tool
for GAG worker pods.

---

## Problem 2: mesh egress interception vs the per-tenant proxy

### What happens

A sidecar transparently redirects the pod's outbound TCP into the mesh proxy
(iptables/eBPF capture to `istio-proxy:15001` / `linkerd-proxy`). GAG has already
pointed the worker at its per-tenant egress proxy via `HTTPS_PROXY`. The two
layers now fight:

- **Double-proxying.** The worker's connection to GAG's proxy `Service` is itself
  captured by the mesh proxy, which then forwards to GAG's proxy, which tunnels
  to GitHub. Extra hop, extra latency, and a second policy-enforcement point that
  knows nothing about GAG's intent.
- **mTLS confusion.** The mesh tries to wrap the worker→GAG-proxy leg in mesh
  mTLS, but GAG's proxy speaks plain `CONNECT` (with its own per-tenant CA on the
  tunnelled leg, see [§5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped)),
  not mesh mTLS. The handshake fails or falls back unpredictably.
- **Broken egress isolation.** If the mesh routes the GitHub-bound traffic out
  through its own *egress gateway* (or a `ServiceEntry`), the packets leave on the
  mesh's egress IP instead of the tenant's proxy IP. The per-tenant egress-IP
  attribution that isolates tenants is silently lost — the most damaging failure,
  because nothing errors; the traffic just exits from the wrong identity.

### Fix: exclude GAG's proxy path from interception

The clean fix is the same namespace opt-out as problem 1 — no sidecar, no
interception. If the namespace must stay meshed (e.g. you are using
[native sidecars](#fix-b--native-sidecars-kubernetes-128) for the lifecycle
fix), you must additionally tell the mesh to **leave GAG's egress path alone** so
the per-tenant proxy is honored. Exclude the proxy `Service` port (`8080`) and/or
the cluster service CIDR from outbound capture.

The canonical exclusions are **pod-level annotations**, which GAG strips from
worker pods ([why](#why-per-pod-podtemplate-annotations-will-not-save-you)).
So for an in-mesh worker namespace you configure exclusions at the **mesh-install
/ revision level**, not per pod. Per-mesh recipes follow.

---

## Per-mesh recipes

> These recipes are the in-mesh path and carry the
> [validation caveat](#the-one-label-answer) (Q220): the structure is sound, but
> verify the specific knob names, values, and version floors against your mesh
> release before relying on them. The [namespace opt-out](#the-one-label-answer)
> remains the recommended default.

### Istio — sidecar mode (in-mesh, if opt-out is not allowed)

1. Lifecycle: enable native sidecars so workers terminate
   (`ENABLE_NATIVE_SIDECARS=true`, [Fix B](#fix-b--native-sidecars-kubernetes-128)).
2. Egress: exclude GAG's proxy port and the cluster service CIDR from outbound
   capture. The per-pod form (shown for reference) is:

   ```yaml
   # Reference only — GAG drops these from the worker podTemplate.
   metadata:
     annotations:
       traffic.sidecar.istio.io/excludeOutboundPorts: "8080"        # GAG proxy CONNECT port
       traffic.sidecar.istio.io/excludeOutboundIPRanges: "10.96.0.0/12"  # your cluster service CIDR
   ```

   Because the annotation cannot reach the worker pod, set the equivalent default
   on the injection template for this revision via the `istio-sidecar-injector`
   `ConfigMap` (`values.global.proxy.excludeOutboundPorts` /
   `excludeIPRanges`), scoped to the revision that meshes the GAG namespace.
   Confirm your cluster service CIDR first:

   ```sh
   kubectl cluster-info dump | grep -m1 service-cluster-ip-range
   ```

3. Do **not** define an egress `Gateway` / `ServiceEntry` that claims GitHub
   hostnames for the GAG namespace — that is precisely the path that would strip
   the per-tenant egress IP.

> Even with all of the above, sidecar mode is strictly more fragile than the
> [namespace opt-out](#the-one-label-answer) or [ambient](#istio--ambient-mode).
> Use it only when policy forbids the simpler options.

### Istio — ambient mode

Ambient is sidecar-less, so problem 1 does not arise. For egress, keep the GAG
namespace **out** of the ambient data plane so `ztunnel` does not capture the
worker→proxy leg:

```sh
# Leave the GAG namespace unlabeled, or explicitly:
kubectl label namespace <tenant-ns> istio.io/dataplane-mode=none
```

If you must enroll the namespace in ambient, ensure no waypoint or
`ztunnel`-level egress policy redirects the proxy's `:8080` traffic or
GitHub-bound traffic, and verify the egress IP as
[below](#verifying-coexistence). Not enrolling it is simpler and is the
recommended choice.

### Linkerd

Injection is **opt-in**, so a GAG namespace gets no proxy unless something
enrolls it.

- Default: do nothing. Confirm there is no inherited
  `linkerd.io/inject=enabled`.
- If a parent scope injects, opt the namespace out:

  ```sh
  kubectl annotate namespace <tenant-ns> linkerd.io/inject=disabled
  ```

- If the namespace **must** be meshed: install with native sidecars
  (`--set proxy.nativeSidecar=true`, [Fix B](#fix-b--native-sidecars-kubernetes-128))
  for the lifecycle fix, and skip outbound capture of the proxy port with the
  Linkerd skip-ports annotation at the install/namespace level:

  ```sh
  # Namespace-level default applied to injected pods in this namespace.
  kubectl annotate namespace <tenant-ns> config.linkerd.io/skip-outbound-ports=8080
  ```

  As with Istio, the per-pod `config.linkerd.io/skip-outbound-ports` annotation
  does **not** reach GAG worker pods if set on the `podTemplate`; apply it at the
  namespace so Linkerd's injector picks it up.

### Cilium Service Mesh

Sidecar-less (per-node Envoy), so neither problem arises by default. The only
thing to check is that no L7 `CiliumEnvoyConfig` / `CiliumClusterwideEnvoyConfig`
or L7 `CiliumNetworkPolicy` redirects the worker's `:8080` egress or the
GitHub-bound `CONNECT` into an Envoy listener. If you use Cilium for egress
control, prefer expressing the GitHub allowlist as `toFQDNs`/`toCIDR` policy that
*complements* GAG's proxy rather than intercepting it (see the
[network architecture](../design/network-architecture.md) and the
[CNI-native FQDN egress](../design/05-security.md) direction).

### Generic pattern (any mesh)

For a mesh not listed above, apply the same three rules:

1. **Lifecycle** — guarantee the worker pod reaches a terminal phase: prefer
   namespace injection opt-out; else a native-sidecar (Kubernetes 1.28+) install
   mode; else a sidecar-less/ambient data plane. Never rely on a classic sidecar
   that runs forever.
2. **Egress** — exclude GAG's proxy `Service` port (`8080` by default) and the
   cluster service CIDR from the mesh's outbound capture, configured at the
   **mesh-install / namespace level** (GAG strips per-pod `podTemplate`
   annotations). Never let a mesh egress gateway claim GitHub hostnames for the
   GAG namespace.
3. **Identity** — after rollout, confirm GitHub still sees the per-tenant proxy
   egress IP, not a mesh egress-gateway IP ([verify](#verifying-coexistence)).

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

Check the container set on a live worker — two containers where you expected one
is a sidecar:

```sh
kubectl get pod -n <tenant-ns> <worker-pod> \
  -o jsonpath='{.spec.containers[*].name}{"\n"}'
# Expect just the runner container (e.g. "runner"), not "runner istio-proxy".
```

**Egress — GitHub sees the per-tenant proxy IP:** follow the egress-isolation
check in the [network architecture validation](../design/network-architecture.md#how-to-validate-network-isolation)
— GitHub-bound traffic must exit through the tenant proxy `Service`, and no
direct or mesh-egress-gateway egress to GitHub should be observed from a worker
pod. If the observed source IP is the mesh egress gateway's, the per-tenant
attribution is broken — revisit [problem 2](#problem-2-mesh-egress-interception-vs-the-per-tenant-proxy).

If jobs stop being picked up, the
[troubleshooting guide](troubleshooting.md) covers the wedged-RunnerGroup
symptom; a namespace in a mesh with classic sidecars is the first thing to rule
out.

---

## Summary

| Situation | Do this |
|---|---|
| You can keep the GAG namespace out of the mesh | [Opt the namespace out](#the-one-label-answer). Done — both problems gone. |
| Mesh membership is mandatory, Kubernetes ≥ 1.28 | [Native sidecars](#fix-b--native-sidecars-kubernetes-128) for lifecycle **plus** [egress exclusions](#problem-2-mesh-egress-interception-vs-the-per-tenant-proxy) at the mesh-install level. |
| You run Istio ambient / Cilium Service Mesh | Sidecar-less — leave the GAG namespace [out of the data plane](#istio--ambient-mode) and confirm no egress redirect. |
| You were about to set mesh annotations on the RunnerGroup `podTemplate` | Don't — [GAG strips them](#why-per-pod-podtemplate-annotations-will-not-save-you). Configure the namespace and the mesh install instead. |

See also: [security model §5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped)
(per-tenant egress proxy), [tenant onboarding](tenant-onboarding.md),
[troubleshooting](troubleshooting.md), and the
[network architecture](../design/network-architecture.md).
