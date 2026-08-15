# Untrusted-PR Egress Posture for Kata Workers — Q408

> **Status (2026-08-05): Phase 1 implemented; its live validation run is next.** Phase 0 (2026-08-03) measured the job-time egress inventory ([§2](#2-the-gap--what-an-e2e-job-actually-fetches-at-job-time-phase-0)) and re-sequenced Phases 1–4.
> Phase 1's workflow change gates `azure/setup-helm` (`get.helm.sh`), every `actions/cache` step, and the bake's `GHA_CACHE` to the hosted lane, per the resolved [§2.4](#24-phase-1-decisions-resolved-2026-08-05) decisions.
> After the validation run: Phase 2 (mirror manifests).

Design and phased plan for the posture named in [Appendix G.14](../design/appendix-g-future-enhancements.md#g14-kata-e2e-untrusted-pr-posture--tight-egress--in-cluster-pull-through-mirror) and on the [public roadmap](../roadmap.md#exploring--longer-term): make the Kata worker variant safe for **untrusted / external-contributor pull-request CI** by removing the workers' direct registry egress.
Concretely: an in-cluster **registry pull-through mirror** (the container-image sibling of the Athens Go-module cache, Q244), job-side wiring so every image pull rides it, and the deletion of the e2e tenant's additive open-egress NetworkPolicy.

**Scope statement — no controller or API changes.** Kata already bounds the kernel-escape axis; the missing half is pure egress posture.
The deliverable is deploy manifests + wiring + docs + live validation, all in the dogfood-e2e tree, published as the supported reference recipe (`docs/operations/`).
The GMC/AGC and the CRDs are untouched: GAG's managed default-deny NetworkPolicy already gives exactly the right worker baseline (below), and the mirror is operator-owned infrastructure like Athens — not a product component.

---

## 1. Where the posture already stands

Most of the untrusted-PR story is shipped.
Inventory of the existing controls, so this plan only builds what is actually missing:

| Control | State |
|---|---|
| Kernel isolation | Kata micro-VM per worker; no `privileged: true` anywhere ([kata-dind-workloads.md](../operations/kata-dind-workloads.md)) |
| Managed worker egress | Default-deny; DNS (cluster resolver only, Q105/Q136/Q229) + tenant egress proxy — or GitHub CIDRs in the direct-egress form (`buildWorkloadNetworkPolicy`, `shared_networkpolicy.go`) |
| Worker ingress | Default-deny (Q128) |
| Metadata server | Workload Identity required + `automountServiceAccountToken: false`; with open egress gone, 169.254.169.254 is reachable on port 53 only (the link-local DNS rule), which the metadata service does not serve |
| Go modules | Athens in-cluster cache (Q244, [`deploy/athens/`](../../deploy/athens/)) — the pattern this plan copies: unlabelled cache pod keeps free egress, additive NetworkPolicy admits workload→cache, workers wired by env (`GOPROXY`). Wired for the **main** dogfood tenant only (`scripts/dogfood/setup.sh`); the e2e tenant is closed by `vendor/` instead ([§2.3](#23-what-the-measurement-changes)) |
| Doctrine | [security-operations.md § Prefer an in-cluster caching mirror first](../operations/security-operations.md#prefer-an-in-cluster-caching-mirror-first) already names "a registry pull-through cache (container images)" as the recommended path — named but never built |

**The one opener:** the e2e overlays ship an additive `e2e-open-egress` NetworkPolicy (`podSelector: {}`, allow-all egress — [`deploy/dogfood-e2e/overlays/kata/resources.yaml`](../../deploy/dogfood-e2e/overlays/kata/resources.yaml)) because the suite pulls from CDN-fronted public registries — and, as Phase 0 measured, from `get.helm.sh` and the Actions cache data plane too.
Q408 is the work that lets us delete it from the Kata overlay.

## 2. The gap — what an e2e job actually fetches at job time (Phase 0)

Only **in-job** traffic matters.
The worker pod's own images (runner, `docker:28-dind` sidecar) are pulled by the *node's* containerd, outside the pod network namespace, and are not governed by the tenant NetworkPolicy.

### 2.1 The measurement

Source: GitHub Actions run [30786972228](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30786972228), job `e2e / e2e` (job id 91602337821) — a green `workflow_dispatch` release-gate run of `e2e-test.yml` on 2026-08-03, routed to the dogfood scale set (`runs-on: gag-ci-e2e`, runner `gag-ci-e2e-246a8cb2-…`), kindnet lane, 62 of 73 specs passed.
`E2E_VARIANT` defaults to `kata` (`scripts/dogfood/e2e-start.sh`) and nothing pins it otherwise, so this is the Kata overlay; the job's `docker info` corroborates it — an Alpine dind container on kernel 6.18.35 reporting 5 CPUs / 13.88 GiB, i.e. a micro-VM sized from the pod's limits, not the c2-standard-8 node it runs on.

No new dogfood run was booked: the job log of a run that already happened *is* the measurement, and it records every fetch the job made.

Reproduce with:

```bash
gh api repos/:owner/:repo/actions/jobs/91602337821/logs > tmp/q408/run.log
```

then extract hosts (`grep -oE 'https?://[a-zA-Z0-9._-]+'`) and image pulls (`grep -oE 'Pulling from [a-zA-Z0-9/_.-]*'`).

### 2.2 The inventory

**Registry / chart pulls.** "Warm" is what run 30786972228 did with the Actions cache populated; "cold" is the same step on a cache miss, which every version bump forces.
Upstream hosts for the three pinned manifests were read from the manifests themselves, not assumed.

| Ref | Client | Warm run |
|---|---|---|
| `docker.io/moby/buildkit:buildx-stable-1@sha256:0168…` | inner dockerd (pre-pull step) | **pulled** — no cache entry exists for it |
| `docker.io/library/registry:2` | inner dockerd (`make e2e-registry`) | **pulled** — no cache entry exists for it |
| `ghcr.io/actions-gateway/gmc:v1.2.0` | inner dockerd (released-chart upgrade check) | **pulled** — released image, never cached |
| `ghcr.io/actions-gateway/charts/actions-gateway:1.2.0` | **helm's OCI client** | **pulled** — released chart, never cached |
| `docker.io/kindest/node:v1.35.5@sha256:ce97…` | inner dockerd | cache hit (353 MB restore) |
| `docker.io/curlimages/curl:8.10.1` | inner dockerd | cache hit |
| `docker.io/hashicorp/vault:1.18.3` | inner dockerd | cache hit |
| `quay.io/jetstack/cert-manager-{controller,webhook,cainjector}:v1.20.2` | inner dockerd | cache hit |
| `registry.k8s.io/metrics-server/metrics-server:v0.8.1` | inner dockerd | cache hit |
| `quay.io/calico/{node,cni,kube-controllers}:v3.31.5` | inner dockerd | calico lane only; not exercised here |

**The inner kind cluster's containerd made zero upstream pulls.** Everything it needs is either `kind load`ed from the runner's Docker daemon (kind node image, cert-manager, metrics-server, calico) or served from the in-job local registry on `127.0.0.1:5000` (the six baked images, curl, Vault).
The one registry resolution kind's containerd attempted was `registry.invalid`, a deliberate negative probe.
So the hypothesised second client is, as installed, not a client at all.

**Non-registry HTTP(S).** Every hostname the job log names, excluding in-cluster addresses and hosts that only appear in printed prose:

| Host | Fetched | Reachable today via |
|---|---|---|
| `github.com` | checkout; `kubernetes-sigs/kind` release binary; cert-manager and metrics-server release manifests; the Go toolchain tarball (`actions/go-versions`, via `actions/setup-go`) | the managed GitHub rule |
| `raw.githubusercontent.com` | the pinned Calico manifest (calico lane only) | the managed GitHub rule |
| `get.helm.sh` | the helm binary, downloaded by `azure/setup-helm` | **`e2e-open-egress` only** |
| Actions cache data plane | five cache restores, ~353 MB for the kind node image alone | **`e2e-open-egress` only** — the host is not visible in the job log and has not been measured |
| Actions service / `api.github.com` | runner control plane, job logs, `upload-artifact` | the managed GitHub rule |
| `proxy.golang.org` | **nothing** — configured as `GOPROXY`, zero `go: downloading` lines in the run | — |

Two coverage gaps in this measurement, both stated rather than papered over: the calico lane (`e2e-calico.yml`, nightly) was not the lane measured, so its `raw.githubusercontent.com` + `quay.io/calico/*` fetches are read from the workflow and the manifest rather than observed; and the live-GitHub specs were among the 11 skipped, so their real `api.github.com` traffic did not run.
Both ride paths the managed GitHub rule already admits, so neither changes the design — but neither is measured.

### 2.3 What the measurement changes

1. **Job-time egress is not registry-only, so the [§3.3](#33-the-networkpolicy-swap) swap cannot produce a green run on its own.** §2's earlier text asserted that `get.helm.sh` and friends "run on trusted infra, not in the job".
   Measured: `azure/setup-helm` downloads from `get.helm.sh` inside the job, and `actions/cache` moves hundreds of megabytes over a data plane that is neither GitHub nor a registry.
   A NetworkPolicy admitting only DNS, GitHub and the mirror set fails at `azure/setup-helm`, before the suite starts.
   The Kata overlay's own comment on `e2e-open-egress` already listed `get.helm.sh` alongside the registries; the plan's §2 contradicted it, and the plan was the one that was wrong.
2. **There is a fourth registry client: helm.** `helm pull oci://ghcr.io/actions-gateway/charts/…` speaks the registry protocol from the runner container, and neither dockerd's `--registry-mirror` nor kind's `containerdConfigPatches` redirects it.
   Wiring it means rewriting the ref (and `--plain-http`), the same treatment §3.2 reserves for non-Hub docker pulls.
3. **The inner kind containerd needs no mirror wiring** (measured: zero upstream pulls). §3.2's `containerdConfigPatches` bullet shrinks from a deliverable to a fallback for a preload that goes missing.
4. **Go is closed by `vendor/`, not by Athens.** §3.2 claimed "the dogfood tenant wires `GOPROXY` to Athens (Q244)".
   That wiring is in `scripts/dogfood/setup.sh` for the *main* dogfood tenant's RunnerTemplate; the e2e tenant's overlays set no `GOPROXY`, and the measured run ran with `GOPROXY='https://proxy.golang.org,direct'`.
   It fetched nothing only because the repo vendors.
   Any future `go install` of a tool outside `vendor/` would egress to `proxy.golang.org` under the tight policy and fail.
5. **`actions/cache` is already doing most of the mirror's job.** Five of the ten refs came from the Actions cache rather than an upstream registry, and four of the remaining five have no cache entry at all.
   So the mirror's value is the cold path (every version bump, every cache eviction) and those four never-cached refs, not the steady state.
   It also makes the two mechanisms substitutes: the tight lane can drop the image caches and let the mirror serve them warm instead of allowlisting the cache data plane.

### 2.4 Phase 1 decisions (resolved 2026-08-05)

- **The non-registry residual shrinks to GitHub-only; no forward proxy.** `azure/setup-helm` is `runner.environment`-gated to the hosted lane, since helm and kubectl are already baked into the e2e runner image (`scripts/dogfood/e2e-runner/Dockerfile`), so `get.helm.sh` is never fetched on the self-hosted lane.
  The kind binary keeps its workflow download: it comes from `github.com`, which the managed rule admits, and the runner image deliberately does not bake it.
- **`actions/cache` is dropped on the self-hosted lane, kept on the hosted lane.** Every `actions/cache` step is `runner.environment`-gated, and so is the bake's buildx `type=gha` cache (`GHA_CACHE`, the same data plane, missed by the Phase 0 host inventory because buildkit's log names no host).
  The self-hosted lane falls back to the retried upstream pulls the cache-miss path already had: registry traffic, still admitted until Phase 4 and mirror-served from Phase 3.
  Interim cost per self-hosted run: the cold pulls and a cold bake, against a measured 21-minute warm run inside a 50-minute timeout.
  The per-PR hosted lane is unchanged.

## 3. Design

### 3.1 The mirror — one pull-through cache per upstream

[CNCF Distribution](https://github.com/distribution/distribution) (`registry:3`, digest-pinned at Phase 2) in **pull-through cache mode** (`proxy.remoteurl`).
Proxy mode supports exactly one upstream per instance, so the deployment is one Deployment + ClusterIP Service per upstream host — `mirror-docker-io`, `mirror-ghcr-io`, `mirror-quay-io`, `mirror-registry-k8s-io`, the set fixed by the Phase 0 measurement ([§2.2](#22-the-inventory)).
Properties that make it the right tool:

- **Read-only by construction.** A registry in proxy mode rejects pushes — untrusted code cannot use the mirror as a drop box.
- **Content control, not just host control.** Workers can speak only the registry protocol, only to the mirror, which fetches only from its one configured upstream.
  This is strictly stronger than any FQDN/CIDR allowlist of the upstreams themselves (§5).
- **Cache behavior.** Same trade as Athens: `emptyDir` default ($0 at rest, cold after scale-to-zero), a `persistent` overlay for a warm cache (`kindest/node` alone is ~1 GiB, so the PVC materially cuts job latency).

Manifests live at `deploy/registry-mirror/` shaped exactly like `deploy/athens/` (base + persistent overlay + README), applied by the e2e setup script.
Like Athens, the mirror pods are **not** labelled `actions-gateway/component=workload`, so the managed default-deny does not select them and they keep free egress to their upstream; an additive NetworkPolicy admits workload pods → mirror pods on the registry port, and its own ingress rule admits only the worker namespace.

Transport is in-cluster plain HTTP (the standard local-mirror pattern; kind and node-local registries do the same).
The inner `dockerd` and containerd get the mirror as an explicitly-schemed `http://` endpoint (plus `insecure-registries` where a client insists).
TLS via the tenant PKI is a possible later hardening; it buys little here because image identity is content-addressed for digest-pinned pulls, and the threat model this plan closes is worker *egress*, not in-cluster MitM.

**Docker Hub rate limits** concentrate on the mirror's egress IP (shared NAT today, so not a regression); if anonymous limits bite, `proxy.username` / `proxy.password` with a token from the platform Secret is the escape hatch — noted, not built.

**Digest integrity is unaffected by the mirror.** Schema-2/OCI digests are pure content addressing — sha256 over the raw manifest and blob bytes, which contain no registry hostname or repository name — so a pull-through cache serving byte-identical upstream content preserves every digest.
(The "mirroring changes digests" folklore comes from the long-dead Docker schema-1 format, which embedded the repo name in the signed payload.)
The repo already depends on this property twice: [air-gapped-install.md](../operations/air-gapped-install.md) relocates images with `crane copy` digest-preserved, and [p2p-image-distribution.md](../operations/p2p-image-distribution.md) routes digest-pinned pulls through containerd mirrors.
The consequence is favorable for this plan: a digest-pinned pull re-verifies client-side, so even a compromised mirror cannot substitute content — the mirror is trusted only for tag→digest resolution, exactly as the upstream registry already was.
Cosign signatures key on the manifest digest, not the pull location, so verification through the mirror also holds.

### 3.2 Wiring the clients

Four upstreams need a mirror instance, per the [§2.2](#22-the-inventory) inventory: `docker.io`, `ghcr.io`, `quay.io`, `registry.k8s.io`.

- **Inner dockerd** (Kata overlay dind sidecar `args`): add `--registry-mirror=http://mirror-docker-io.<ns>.svc.cluster.local:5000`.
  This transparently covers every Docker-Hub pull, implicit or explicit.
  `dockerd` mirrors **only** Docker Hub, and the measured job makes non-Hub docker-client pulls — `ghcr.io/actions-gateway/gmc`, and on a cold cache the `quay.io` and `registry.k8s.io` prepulls.
  Those refs are rewritten to the mirror address (`mirror-ghcr-io:5000/owner/img`) at their call sites, which pull-through mode serves natively.
- **helm's OCI client** (`chart-released-upgrade-check.sh`): `helm pull oci://ghcr.io/<owner>/charts/…` takes neither of the above.
  The script already parameterises the ref (`RELEASED_CHART_OCI`), so the tight lane points it at `mirror-ghcr-io:5000/<owner>/charts` with `--plain-http`.
- **Inner kind containerd** (`test/kind-config-*.yaml`): nothing required.
  Measured: zero upstream pulls, because every image is `kind load`ed or served from the in-job `127.0.0.1:5000` registry.
  Mirror entries via `containerdConfigPatches` remain an option as a fallback for a preload that goes missing; they would need to be additive-with-fallback (containerd falls back to the upstream when the mirror is unreachable) since the same configs serve local `make e2e` outside the cluster.
- **Go**: closed by the committed `vendor/` tree, not by a proxy — the e2e tenant sets no `GOPROXY` and the measured run used `proxy.golang.org` without fetching from it ([§2.3](#23-what-the-measurement-changes)).
  Under the tight policy that default becomes a latent failure for any `go install` outside `vendor/`; wiring the e2e tenant to Athens the way `scripts/dogfood/setup.sh` wires the main dogfood tenant closes it.

### 3.3 The NetworkPolicy swap

In the **Kata overlay only**: delete `e2e-open-egress`, add `e2e-mirror-egress` — workload pods → mirror pods, registry port only.
The managed default-deny stays authoritative for everything else (DNS + proxy / GitHub).
The dind overlay keeps open egress: it is the explicit trusted-CI fallback (`E2E_VARIANT=dind`), and privileged DinD was never a candidate for untrusted code anyway.

Target: the Kata worker's complete reachable set is **cluster DNS (:53) + GitHub (via the managed rule) + the mirror set**.
Nothing else, including the metadata server on its service ports.

**This swap is only green once the non-registry residual is gone.** Phase 0 measured `get.helm.sh` and the Actions cache data plane as job-time fetches that no mirror serves and no managed rule admits, so Phase 1 has to close them before this policy can be applied — [§2.4](#24-phase-1-decisions-resolved-2026-08-05).

### 3.4 Residual channels, stated honestly

- **GitHub via the managed rule** — inherent to running GitHub CI (job code can always write to its own repo/gists through sanctioned channels).
  The Q242 destination allowlist and per-tenant egress-IP attribution govern this; out of scope here.
- **Pull-name side channel** — a malicious job may `docker pull docker.io/attacker/<encoded-bits>`; the mirror forwards that request upstream, leaking low-bandwidth data via the requested path, and pulls attacker-published content in.
  Accepted: the channel is narrow, fully auditable at one point (the mirror's access log), and closable later with a repository allowlist in front of the mirror if a real deployment wants it.
  The same audit point simply does not exist with direct registry egress.
- **DNS names still recurse upstream** via cluster DNS — the established Q105 stance (attributable path); unchanged.

### 3.5 The mirror role is a contract

What the posture actually requires is not "CNCF Distribution" but an endpoint with four properties:

1. **Selectable by NetworkPolicy** — a stable pod identity a `podSelector` can name (rules out `hostNetwork` daemons, which force node-CIDR `ipBlock` allows).
2. **Fixed upstream set** — the endpoint fetches only from its configured upstream(s), never from whatever registry the client names.
3. **Read-only** — pushes and other non-GET registry operations are refused, not forwarded.
4. **Workers are clients only** — untrusted pods consume the endpoint; they never join whatever distribution mesh sits behind it.

Distribution's proxy mode satisfies all four **by construction**, which is why it is the reference implementation.
Any backend meeting the same four tests can substitute — Dragonfly is the scheduled candidate ([§6](#6-follow-on-validations-q539-q540)).

## 4. Phases

Each phase is a separate PR; 1, 3 and 4 need live dogfood sessions (prod-guarded — deliberate, operator-driven runs).

- **Phase 0 — measured egress inventory. ✅ Done (2026-08-03).** Read off a green Kata dogfood run that had already happened rather than booking a new one; [§2](#2-the-gap--what-an-e2e-job-actually-fetches-at-job-time-phase-0) is the deliverable, and [§2.3](#23-what-the-measurement-changes) is what it cost the design.
- **Phase 1 — shrink the non-registry residual to GitHub.
  Implemented (2026-08-05); validation pending.** New phase, forced by Phase 0.
  The [§2.4](#24-phase-1-decisions-resolved-2026-08-05) decisions are resolved: `e2e-reusable.yml` gates `azure/setup-helm`, every `actions/cache` step, and `GHA_CACHE` to `runner.environment == 'github-hosted'`.
  Validation still to run (operator-driven dogfood session): a green Kata e2e run with open egress still present, and the run log naming no non-GitHub, non-registry host.
- **Phase 2 — mirror manifests.** `deploy/registry-mirror/` (Athens-shaped: base, persistent overlay, README, NetworkPolicies), one instance per §2.2 upstream — `docker.io`, `ghcr.io`, `quay.io`, `registry.k8s.io` — applied from the e2e setup path.
  Validation: apply to the dogfood cluster; `curl` the mirror's `/v2/` and pull one image through each instance from a debug pod.
- **Phase 3 — wiring.** dockerd `--registry-mirror` in the Kata overlay; non-Hub docker-client refs rewritten; helm's OCI ref pointed at the ghcr mirror.
  Validation: a green Kata e2e run **with open egress still present**, with the mirror access logs proving the pulls rode the mirror (hit counts > 0 per instance) — wiring proven before enforcement changes.
  Run it once with the image caches cold, so the `quay.io` / `registry.k8s.io` prepulls are exercised rather than skipped.
- **Phase 4 — enforcement.** Swap `e2e-open-egress` → `e2e-mirror-egress` in the Kata overlay.
  Validation on dogfood: (a) a green Kata e2e run under the tight policy; (b) negatives from inside a job — `docker pull` of a host with no mirror instance fails, `curl https://example.com` times out, a push to the mirror is refused; (c) kind-side pull of a non-local-registry image succeeds via the mirror.
- **Phase 5 — docs + close-out.** [kata-dind-workloads.md](../operations/kata-dind-workloads.md) caveat "validated posture is trusted CI" flips to the how-to; [security-operations.md](../operations/security-operations.md) mirror section links the concrete manifests; [in-runner image builds](../operations/in-runner-image-builds.md) and [network-architecture.md](../design/network-architecture.md) cross-refs; G.14 marked shipped; [roadmap.md](../roadmap.md) entry moves out of "exploring"; Q408 row deleted.

## 5. Alternatives considered

- **FQDN allowlist of the upstream registries** (Q245 machinery).
  Rejected on three axes: the dogfood cluster does not run `--enable-fqdn-network-policy`, and the GMC's FQDN backends target the *proxy pool's GitHub* allowlist, not arbitrary tenant-namespace policies; CDN-fronted hosts stress the DPv2 wildcard 50-IP ceiling (the one unstressed Q245 caveat); and above all it is host-scoped, not content-scoped — `docker.io` allowlisted is `docker.io/<anyone>/<anything>` in both directions, i.e. the exfil surface stays open.
  The mirror gives a single auditable chokepoint instead.
- **Spegel / Dragonfly P2P mirrors as the first implementation** ([p2p-image-distribution.md](../operations/p2p-image-distribution.md)).
  Their native layer is the **node's** containerd — pod-image distribution, which the tenant NetworkPolicy never governs — and Spegel only re-serves images already in the cluster's content store, so neither addresses the in-guest pulls as installed.
  Dragonfly *can* be configured into the §3.5 mirror contract (seed-peer Service rather than the `hostNetwork` dfdaemon, an explicit upstream allowlist, non-GET refused), but that is hardening a manager + scheduler + seed-peer + per-node-daemon stack to reach a property one stateless Deployment has by construction — so it is sequenced as a follow-on validation ([§6](#6-follow-on-validations-q539-q540)), not the reference posture.
- **Harbor / zot.** Harbor is a full registry product (DB, core, jobservice) — heavy for a cache; zot's mirroring is sync-rule-shaped rather than a plain per-upstream pull-through.
  Distribution's proxy mode is the boring, proven primitive, and it is what `security-operations.md` already recommends.
- **Routing registry traffic through the tenant egress proxy** (widened Q242 allowlist).
  Host-scoped only (same exfil objection as FQDN), mixes bulk image traffic into the GitHub-attribution egress path, and buys no caching.

## 6. Follow-on validations (Q539, Q540)

Decision 2026-07-31: both alternates get validated, sequenced **after** this plan's Phase 4 proves the reference posture — the contract must be validated on the simple implementation before variants are graded against it.

- **[Q539](../STATUS.md#Q539) — Kata + Dragonfly as the mirror backend.** Substitute a Dragonfly deployment for the Distribution instances behind the same wiring and the same tight NetworkPolicy, and prove it meets the §3.5 contract: workers reach only a seed-peer ClusterIP Service (the `hostNetwork` dfdaemon is not policy-selectable), back-to-source is restricted to the Phase-0 upstream allowlist, non-GET registry operations are refused, and workers never participate in the P2P mesh.
  Same Phase-4 validation battery, plus negatives for the contract points.
  Needs its own plan doc when picked up.
- **[Q540](../STATUS.md#Q540) — the composed stack as a milestone: Kata + Dragonfly (node layer) + pull-through cache (guest layer).** The two operate at different layers (§5), so this validates the composition an image-heavy fleet would actually run: Dragonfly accelerating node-level pod-image distribution via containerd mirror config, the in-guest mirror serving untrusted job pulls, each with its own policy scope — and confirms neither weakens the other's posture (in particular that the node-layer P2P mesh stays unreachable from worker pods).

## 7. Success criteria

Q408 closes when the Phase 4 validation lands: the dogfood Kata e2e variant runs green with `e2e-open-egress` deleted, the negative probes confirm enforcement, and the Phase 5 docs make the recipe the supported posture.
At that point "Kata-isolated runners are only suitable for trusted CI" disappears from the docs, replaced by the reference architecture for untrusted-PR CI.
