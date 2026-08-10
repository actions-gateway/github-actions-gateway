# Q220 — Validate service-mesh coexistence guidance on a live cluster

**Status: COMPLETE (evidence record).** Istio and Linkerd **sidecar mode** were validated end-to-end on kind (k8s 1.35) and the guide [`docs/operations/service-mesh-coexistence.md`](../../operations/service-mesh-coexistence.md) was corrected accordingly.
Istio ambient / Cilium, and the Linkerd worker-lifecycle (blocked by a sidecar egress break), remain for the follow-up (Q280).

**Goal:** Empirically validate (and correct) the in-mesh recipes in the coexistence guide, which were reasoned from code + upstream docs but never tested against a running mesh.
Deliverable is the corrected guide + this evidence log.

## Conclusion (what passed / what didn't)

- **Istio 1.30.2 sidecar mode — fully validated.** Problem 1 (classic sidecar strands the worker) and Fix B (native sidecar terminates it, +1 s after runner exit) both reproduced at container-state granularity; egress path preserved through the sidecar (HTTP 200 via the tenant proxy, matching a no-sidecar baseline).
- **Linkerd edge-26.6.3 sidecar mode — partially validated + a new blocker found.** The setup findings (annotation-not-label, baseline-PSA rejection of `linkerd-init` → need linkerd2-cni, native-sidecar default, control-plane egress NP) all confirmed and mirror Istio.
  But the Linkerd sidecar then **broke GAG's data-plane egress** (HTTP 504 at its 1 s connect timeout, client-side LB to pod IPs vs GAG's NP), so the meshed AGC couldn't fetch its token and never started — worker termination could not be observed.
- **Three previously-undocumented, guide-rewriting findings** (both meshes): (1) baseline PSA rejects the privileged proxy-init container → the mesh CNI is *mandatory*, not optional; (2) GAG's default-deny egress NP blocks the sidecar→control-plane path → an additive NP is required; (3) native sidecars are now the *default* on both meshes (the guide had presented the flag as a required manual step).
- **Not validated (→ Q280):** Istio ambient, Cilium Service Mesh, and whether Linkerd's `skip-outbound-ports` fully restores GAG egress.
  The guide now flags these honestly.

## Two properties under test

For each mesh/mode, run a GAG worker through a meshed tenant namespace and confirm:

- **(a) Termination** — the worker pod reaches a terminal phase (`Succeeded`/`Failed`) and is reaped, rather than being pinned `Running` by a never-exiting sidecar (the problem-1 hazard).
  A *classic* sidecar should strand it; a *native* sidecar / ambient / opt-out should let it complete.
- **(b) Egress-IP preservation** — GitHub-bound traffic still exits via the per-tenant proxy (the proxy is the source identity fakegithub sees), not rewritten/hijacked by the mesh.

## Environment

- Local **kind** cluster `actions-gateway-e2e` (k8s v1.35), kindnet CNI.
- Infra (cert-manager + GMC + fakegithub) stood up via the e2e suite's `SynchronizedBeforeSuite` with `E2E_SKIP_TEARDOWN=true`, then tenants driven manually with kubectl + the fakegithub control API.
- Meshes installed via **Helm** (no new host binary — istioctl/linkerd not on PATH, per task guidance prefer Helm).
  Istio 1.30.2.
- The e2e "worker" is a fast-exiting placeholder (AGC image) — ideal for the termination test: if the pod stays `Running`, only the sidecar can be pinning it.

## Test matrix

| Mesh / mode | Termination (a) | Egress (b) | Status |
|---|---|---|---|
| Istio classic sidecar (`ENABLE_NATIVE_SIDECARS=false`) | **STRANDED — confirmed** | **preserved — confirmed** | ✅ done |
| Istio native sidecars (default in 1.30) | **terminates — confirmed** | **preserved — confirmed** | ✅ done |
| Istio ambient (Fix C) | expect terminates | expect preserved | pending |
| Istio namespace opt-out | expect terminates | expect preserved | pending |
| Linkerd (native default, cni) | (blocked by egress) | **BROKEN — sidecar 504s GAG egress** | ✅ finding |

- **Linkerd sidecar breaks GAG's data-plane egress (severe).** Even after clearing PSA (linkerd2-cni) and control-plane NP, a meshed workload-labeled pod hitting the fakegithub Service (the GitHub token/broker stand-in) gets **HTTP 504 at t=1.01s** (Linkerd's 1s connect timeout); a non-meshed pod succeeds.
  Linkerd does client-side load-balancing directly to endpoint pod IPs (via linkerd-destination), and that connection is dropped under GAG's NetworkPolicy-gated egress where Istio's passthrough was preserved.
  Consequence: the meshed AGC's installation-token fetch fails (504) → `startup failed` → the tenant never starts.
  Linkerd in-mesh coexistence is materially more fragile than Istio; the worker-termination behavior could not be observed because the AGC never started (it is expected to parallel Istio's native-sidecar termination, same kubelet mechanism, but this was NOT directly observed).

### Linkerd — findings (edge-26.6.3, k8s 1.35)

- **Injection is an ANNOTATION, not a label.** `kubectl label ns … linkerd.io/inject=enabled` is silently ignored (`InjectionSkipped` event); it must be `kubectl annotate ns … linkerd.io/inject=enabled`.
  (The guide's opt-out table already uses `annotate` — good — but this is worth calling out explicitly vs Istio's label.)
- **Same baseline-PSA rejection as Istio.** With injection on, `linkerd-init` requests `NET_ADMIN`/`NET_RAW` → baseline PSA rejects every pod (`FailedCreate`).
  Fix: the **linkerd2-cni** chart + `cniEnabled=true` on the control plane (drops `linkerd-init`), exactly paralleling Istio CNI.
  Confirms the PSA collision is mesh-general, not Istio-specific.
- The free **stable** Linkerd line is frozen at 2.14 (no native sidecar); the FOSS **edge** channel (`linkerd-edge/linkerd-control-plane`, edge-26.6.x) is the current line and the one that supports `proxy.nativeSidecar` and recent k8s.
- **Native sidecar is Linkerd edge's default too** — under linkerd-cni the injected layout is `initC: linkerd-network-validator, linkerd-proxy(restartPolicy=Always)`; `linkerd-proxy` is a native sidecar with no explicit `proxy.nativeSidecar=true`.
- **Same control-plane-egress block as Istio.** The GAG deny-all egress NP blocks the proxy from reaching `linkerd-dst:8086`, `linkerd-identity:8080`, `linkerd-policy:8090` (`connect timed out`, `Failed to obtain identity`) → pod stuck at `Init`, AGC never starts.
  Fix: extend the tenant NP to allow egress to the `linkerd` namespace on those ports (parallel to the istiod `:15012` allowance).

### Istio Problem-1 / Fix-B — CONFIRMED (Istio 1.30.2, k8s 1.35)

- **Classic sidecar strands the worker (Problem 1).** With `ENABLE_NATIVE_SIDECARS=false`, the worker's `istio-proxy` is injected as a *regular container*.
  Terminal capture: `runner` terminated (exitCode 1) at 09:13:42; `istio-proxy` **still `running`** 98s later; pod stuck `1/2`, phase never terminal.
  GAG's `completedPodTTL` reaper only acts on terminal-phase pods, so the slot leaks — exactly the documented hazard.
- **Native sidecar lets the worker terminate (Fix B).** Default injection (native).
  Terminal capture: `runner` exited (exitCode 1) at 09:05:45; kubelet terminated the `istio-proxy` native sidecar at 09:05:46 (**+1s**, exitCode 0, Completed); pod reached terminal `Failed` and GAG reaped it.
  No stranding.
- **Native sidecars are the Istio 1.30 default** — `ENABLE_NATIVE_SIDECARS` was unset on istiod yet the proxy injected as an init container with `restartPolicy: Always`.
  The guide's Fix B presents the flag as a required manual step; on current Istio it is the default (the flag now matters for turning native sidecars *off*).
- **Egress path preserved through the sidecar (Problem 2, no egress gateway).** A meshed, workload-labeled pod reached `https://api.github.com/zen` **through the tenant proxy** (`--proxy https://actions-gateway-proxy:8080`) → HTTP 200, identical to a sidecar-excluded baseline pod (both `remote=<proxy ClusterIP>`).
  The Istio sidecar transparently forwards the worker→proxy CONNECT; it does not reroute GitHub-bound egress to a mesh egress gateway.
  Caveat matches the guide: **no** egress `Gateway`/`ServiceEntry` claiming GitHub hosts was defined.
  The guide's port-8080 exclusion is an optimization (skips the extra Envoy hop / mTLS-wrap of the worker→proxy leg), not a correctness requirement — the path works without it.
  (Per-tenant *egress IP* preservation is a GKE property; kind has no per-tenant IP, so this validates path/transit preservation, not the cloud IP attribution.)
- The tenant egress proxy listens **TLS on :8080** — clients must use `--proxy https://… --proxy-insecure` (plaintext `http://` proxy scheme fails).

## Manual tenant flow (per config)

1. `kubectl create ns <tenant>` (+ mesh enrolment label as the case requires).
2. Create `github-app-secret` (test RSA key at `tmp/mesh/test-app.pem`).
3. Apply an ActionsGateway CR with a RunnerGroup (workerImage = AGC image, short `completedPodTTL`), + the additive fakegithub-egress NetworkPolicy.
4. Wait for AGC ready + RunnerGroup reconciled (broker session registers).
5. Port-forward fakegithub `:9090` control API; enqueue a job onto each active session → worker pod spawns.
6. Observe: worker pod container set, phase transition, reaping; proxy path.

## Findings log

_(appended as each config is validated)_

- Istio injector webhooks (`istio-sidecar-injector`) are `failurePolicy: Fail` but their objectSelectors only fire on pods in `istio-injection=enabled` namespaces or pods explicitly labeled `sidecar.istio.io/inject=true`, so a bare Istio install does NOT break pod creation in unlabeled namespaces (confirmed: namespaceSelector/objectSelector are evaluated by the apiserver before the webhook is called, so unmatched pods never reach istiod).
- **HEADLINE (Istio):** With `istio-injection=enabled` on a GAG tenant namespace, the GMC's default **`pod-security.kubernetes.io/enforce=baseline`** label makes the apiserver **reject every AGC/proxy/worker pod at creation**: Istio's classic injection adds an `istio-init` init container requesting `NET_ADMIN` + `NET_RAW`, which `baseline` forbids.
  Deployments sit at 0 replicas with `FailedCreate` events; no pod (and thus no sidecar, no stranding) ever exists.
  The guide's problem-1 narrative ("the sidecar keeps the pod Running") never triggers — the pod isn't admitted.
  Fix: install the **Istio CNI plugin** so the privileged `istio-init` container is not injected (node-level redirect instead), OR relax the namespace to `enforce=privileged` (a PSA regression — not recommended).
  Confirmed on Istio 1.30.2 / k8s 1.35.
- Onboarding gate (not mesh-specific but required to reproduce): the GMC's `gmc-namespace-psa-guard` ValidatingAdmissionPolicy refuses to reconcile a namespace unless it carries `actions-gateway.com/tenant=managed`.
- Infra bring-up (cert-manager/fakegithub rollout) is sensitive to peak-load resource contention on a 3-node kind cluster — running the mesh install + image builds concurrently caused rollout-status timeouts that tore down the whole suite.
  Bring up infra on a calm cluster (builds + mesh done first).
