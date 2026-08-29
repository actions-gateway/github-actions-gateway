# Untrusted-PR Egress Posture for Kata Workers — Q408

> **Status (2026-08-28): all five phases are done and Q408 is closed.** The posture is enforced and measured: the Kata overlay ships no additive egress policy, and the tenant's live rules carry zero allow-all, so a worker reaches cluster DNS on 53, GitHub on 443 and the mirrors on 5000 and nothing else.
> Validated on the dogfood cluster ([§3.13](#313-phase-4-validation-graded-2026-08-28)): a green Kata run (75 of 75 specs) whose in-job negatives passed all eight checks on a `runtimeClassName: kata` worker, with four controls answering so the four blocked probes, over three destinations, mean the policy rather than a dead network.
> Phase 2's battery re-ran at 25 of 25 under that policy and the mirrors served 178 content requests.
> What is built is [§3.12](#312-phase-4-build-notes-2026-08-28).
> Phase 5 published the recipe as the supported posture: [kata-dind-workloads.md § Untrusted pull requests](../operations/kata-dind-workloads.md#untrusted-pull-requests--the-tight-egress-posture) is the operator how-to, G.14 is marked shipped, and the roadmap bullet is gone because the capability is on [Features](../features.md) instead.
>
> **This doc stays here rather than moving to `archive/`** because [§6](#6-follow-on-validations-q539-q540) still owns two live rows: Q539 and Q540 are graded against the contract validated here.
>
> **Phase 3 is done.** Its wiring is built and gated, and validated on the dogfood cluster: a green Kata e2e run whose five mirror instances served 161 content requests between them, against a zero baseline measured on the same cluster twenty minutes earlier ([§3.11](#311-phase-3-validation-graded-2026-08-28)).
> All four clients are wired ([§3.9](#39-phase-3-build-notes-2026-08-28)): dockerd by a mounted `daemon.json`, the non-Hub docker pulls by a ref rewrite at the one chokepoint they share, buildkit by a generated `buildkitd.toml`, helm by its OCI ref.
> `make registry-mirror-wiring-check` holds the three files that name the endpoint set to each other.
> A green run said nothing on its own there: the open-egress policy was still in place, so an unwired client reached its upstream and the suite was green either way.
> What discriminates is the access log, and the repositories it names attribute each of the four clients separately.
> Phase 2 was validated 2026-08-28: 25 of 25 checks over the five declared instances ([§3.8](#38-phase-2-validation-graded-2026-08-28)), by `scripts/dogfood/e2e-mirror-validate.sh` ([§3.7](#37-the-phase-2-validation-battery)), with every expected value measured and every control fired.
> Phase 1 was validated 2026-08-24: the non-registry residual is measured gone.
> Phase 0 (2026-08-03) measured the job-time egress inventory ([§2](#2-the-gap--what-an-e2e-job-actually-fetches-at-job-time-phase-0)) and re-sequenced Phases 1–4.
> Phase 1's workflow change gates `azure/setup-helm` (`get.helm.sh`), every `actions/cache` step, and the bake's `GHA_CACHE` to the hosted lane, per the resolved [§2.4](#24-phase-1-decisions-resolved-2026-08-05) decisions.
> [§2.5](#25-phase-1-validation-graded-2026-08-24) grades four green self-hosted runs against it: no non-GitHub, non-registry host is fetched, and the graded inventory adds a **fifth** registry upstream, `gcr.io`, that Phase 0's host extraction was structurally unable to see.

Design and phased plan for the posture named in [Appendix G.14](../design/appendix-g-future-enhancements.md#g14-kata-e2e-untrusted-pr-posture--tight-egress--in-cluster-pull-through-mirror), and published while it was in flight on the public roadmap: make the Kata worker variant safe for **untrusted / external-contributor pull-request CI** by removing the workers' direct registry egress.
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

**The one opener:** the e2e overlays shipped an additive `e2e-open-egress` NetworkPolicy (`podSelector: {}`, allow-all egress, still in [`deploy/dogfood-e2e/overlays/dind/resources.yaml`](../../deploy/dogfood-e2e/overlays/dind/resources.yaml)) because the suite pulls from CDN-fronted public registries, and, as Phase 0 measured, from `get.helm.sh` and the Actions cache data plane too.
Q408 is the work that let us delete it from the Kata overlay, which [§3.12](#312-phase-4-build-notes-2026-08-28) did.

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
That host extraction is scheme-prefixed and is now known to be incomplete in both halves of the inventory: see the note in [§2.2](#22-the-inventory) and the general sweep in [§2.5](#25-phase-1-validation-graded-2026-08-24).
It is kept here as the method of record for what Phase 0 actually ran, not as the method to reuse.

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
| `gcr.io/distroless/static:nonroot@sha256:d29e…` | **buildkit** (image bake; base layer of `gmc`, `agc`, `proxy`, `fakegithub`) | **resolved** against `gcr.io` (`load metadata`, `resolve`); added by [§2.5](#25-phase-1-validation-graded-2026-08-24), see the note below |

**The inner kind cluster's containerd made zero upstream pulls.** Everything it needs is either `kind load`ed from the runner's Docker daemon (kind node image, cert-manager, metrics-server, calico) or served from the in-job local registry on `127.0.0.1:5000` (the six baked images, curl, Vault).
The one registry resolution kind's containerd attempted was `registry.invalid`, a deliberate negative probe.
So the hypothesised second client is, as installed, not a client at all.

**The `gcr.io` row was added later, and the reason is a defect in this section's own method.** [§2.1](#21-the-measurement) extracts hosts with `grep -oE 'https?://…'`, which is scheme-prefixed; buildkit names its base images without a scheme (`load metadata for gcr.io/distroless/static:nonroot@sha256:…`), so no `gcr.io` fetch could ever appear in a host list built that way.
The refs were in the Phase 0 log the whole time: re-running a schemeless extraction over that same log (job 91602337821) returns six `gcr.io` lines, so this is a missed reading rather than a change in behaviour.
It is the same blind spot [§2.4](#24-phase-1-decisions-resolved-2026-08-05) already records for `GHA_CACHE`, where buildkit's log likewise names no host.
The class is not registry-specific, and scoping the lesson to registries is how [§2.5](#25-phase-1-validation-graded-2026-08-24) first missed `sum.golang.org` on the non-registry side: a config value like `GOSUMDB='sum.golang.org'` carries neither a scheme nor a trailing slash, so both a `https?://` pattern and a registry-ref-shaped one skip it.
Grade any host inventory here with a general domain-shaped sweep.

**Non-registry HTTP(S).** Every hostname the job log names, excluding in-cluster addresses and hosts that only appear in printed prose:

| Host | Fetched | Reachable today via |
|---|---|---|
| `github.com` | checkout; `kubernetes-sigs/kind` release binary; cert-manager and metrics-server release manifests; the Go toolchain tarball (`actions/go-versions`, via `actions/setup-go`) | the managed GitHub rule |
| `raw.githubusercontent.com` | the pinned Calico manifest (calico lane only) | the managed GitHub rule |
| `get.helm.sh` | the helm binary, downloaded by `azure/setup-helm` | **`e2e-open-egress` only** |
| Actions cache data plane | five cache restores, ~353 MB for the kind node image alone | **`e2e-open-egress` only** (the host is not visible in the job log and has not been measured) |
| Actions service / `api.github.com` | runner control plane, job logs, `upload-artifact` | the managed GitHub rule |
| `proxy.golang.org` | **nothing** — configured as `GOPROXY`, zero `go: downloading` lines in the run | — |
| `sum.golang.org` | **nothing**: configured as `GOSUMDB`, covered by the same zero `go: downloading` count. Added by the same re-grade that added `gcr.io` above | — |

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

### 2.5 Phase 1 validation: graded (2026-08-24)

Phase 1's success condition is the row's: *a green Kata e2e whose log names no non-GitHub, non-registry host.* Graded below from the logs, not argued from the workflow.

**No new dogfood run was booked.** Four green `workflow_dispatch` runs of `e2e-test.yml` already post-date the Phase 1 merge (`939a72cde`, #1297, 2026-08-05), and all four routed to the dogfood scale set:

| Run | Job | Date | Runner |
|---|---|---|---|
| [31901350050](https://github.com/actions-gateway/github-actions-gateway/actions/runs/31901350050) | 95052533561 | 2026-08-15 | `gag-ci-e2e-76fff41e-…` |
| [31805454168](https://github.com/actions-gateway/github-actions-gateway/actions/runs/31805454168) | 94783222823 | 2026-08-14 | `gag-ci-e2e-c235a0e1-…` |
| [31348513226](https://github.com/actions-gateway/github-actions-gateway/actions/runs/31348513226) | 93334887820 | 2026-08-10 | `gag-ci-e2e-c6acc505-…` |
| [31330763470](https://github.com/actions-gateway/github-actions-gateway/actions/runs/31330763470) | 93288542407 | 2026-08-09 | `gag-ci-e2e-9ee5b77a-…` |

Variant identification follows [§2.1](#21-the-measurement).
`E2E_VARIANT` defaults to `kata`, the label `gag-ci-e2e` is the shared base label rather than a variant discriminator, and `docker info` corroborates: Alpine on kernel 6.18.35 reporting **5 CPUs / 13.88 GiB**, a micro-VM sized from pod limits rather than the **c2-standard-8** (8 vCPU) e2e node that a non-Kata dind sidecar would report.
The overlay patches image, `nodeSelector`, tolerations and `storageClassName`, but not resource limits, so that CPU count discriminates rather than coincides.

**The gating fires.** On all four self-hosted runs `GHA_CACHE` evaluates to the empty string, against `GHA_CACHE: true` on the Phase 0 control, and no `actions/cache` step executes.
(A `type=gha` count is not evidence either way: buildx does not echo its cache arguments, so the control returns 0 for it too.)
(`azure/setup-helm` and `actions/cache` still appear once each as `Download action repository`: the runner pre-fetches every referenced action regardless of its `if:`, from GitHub, and neither ever runs.)

**The residual is gone, and the probe can prove a negative.** The same probe run against the Phase 0 hosted-lane control (job 91602337821) fires on both signals, so the zeros below are an absence rather than a query that never matched:

| Log | `get.helm.sh` | `Cache restored` |
|---|---|---|
| Phase 0 control (hosted lane) | 2 | 10 |
| All four self-hosted runs | **0** | **0** |

The Actions cache **data plane host** is still not directly observable, since [§2.2](#22-the-inventory) recorded that the host never appears in the job log.
So the cache's *effect* (`Cache restored` / `Cache not found`) is the observable used, and it is absent.

**Every non-GitHub, non-registry host the four logs name, and its disposition.** Taken with a general domain-shaped sweep rather than a scheme-prefixed or registry-shaped one, for the reason [§2.2](#22-the-inventory)'s note gives:

| Host | Fetched? | Evidence |
|---|---|---|
| `docs.docker.com` | **no**, printed prose | dockerd advisory text: "more information:", "Learn more at:" |
| `proxy.golang.org` | **no**, config value only | printed as `GOPROXY='https://proxy.golang.org,direct'`; the `go: downloading` count is **0** in all four, so `vendor/` closes Go exactly as [§2.3](#23-what-the-measurement-changes) item 4 predicted |
| `kind.sigs.k8s.io` | **no**, printed prose | kind CLI footer ("Have a question…", "Not sure what to do next?") |
| `sum.golang.org` | **no**, config value only | printed as `GOSUMDB='sum.golang.org'` in the same `go env` block, three lines below `GOPROXY`; the same `go: downloading` count of **0** covers it |
| `registry.invalid` | **no**, fails to resolve | the deliberate negative probe of [§2.2](#22-the-inventory) |

In-cluster addresses (`fakegithub.e2e-infra.svc.cluster.local`, `gmc-controller-manager-metrics-service.gmc-system.svc.cluster.local`, `127.0.0.1`, `0.0.0.0`, `kubernetes.default.svc`) are excluded as they were in Phase 0.

**Verdict: PASS.** No non-GitHub, non-registry host is fetched on the self-hosted Kata lane.

**One correction to the design, from the same grading.** The registry upstreams actually contacted are **five**, not four: `docker.io`, `ghcr.io`, `quay.io`, `registry.k8s.io`, and **`gcr.io`** (buildkit's `distroless/static` base).
What the log proves for `gcr.io` is a *resolution*, not a layer transfer: buildkit prints `load metadata for gcr.io/distroless/static:nonroot@sha256:…` and `resolve … done`, with no separate download lines.
That is still a registry API call to a fifth host, so a policy admitting only the four would fail it, and the digest pin means the reachability rather than the bytes is what matters here.
[§3.1](#31-the-mirror--one-pull-through-cache-per-upstream), [§3.2](#32-wiring-the-clients) and Phase 2 are updated accordingly.
Left uncorrected, Phase 2 would have built four mirror instances and Phase 4's tight policy would then have failed at the image bake, the one step that no cache and no `kind load` can cover.

**Reproduce:**

```bash
for j in 95052533561 94783222823 93334887820 93288542407 91602337821; do
  gh api "repos/:owner/:repo/actions/jobs/$j/logs" > "tmp/q408/job-$j.log"
done
```

then grade each log with a **general domain-shaped sweep**, not a scheme-prefixed one and not a registry-ref-shaped one:

```python
dom = re.compile(r'(?<![A-Za-z0-9._-])((?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,})(?![A-Za-z0-9-])', re.I)
```

Read every hit rather than counting them.
Most are Kubernetes API groups, Go identifiers or filenames, and separating a fetch from printed prose needs the surrounding line (`Pulling from`, `load metadata for`, `GOPROXY=`), never the hostname alone.
Use `91602337821` as the control: it must fire on `get.helm.sh` and `Cache restored`, and a sweep that cannot make it fire is not measuring the self-hosted zeros either.

**Two coverage gaps, stated rather than papered over.** The calico lane is still unmeasured on the self-hosted path (`e2e-calico.yml` is nightly and hosted), so its `raw.githubusercontent.com` and `quay.io/calico/*` fetches remain read from the workflow rather than observed.
The live-GitHub specs also remain among the skipped, so their real `api.github.com` traffic still has not run.
Both ride paths the managed GitHub rule already admits, so neither changes the design, and neither is measured.

## 3. Design

### 3.1 The mirror — one pull-through cache per upstream

[CNCF Distribution](https://github.com/distribution/distribution) (`registry:3`, digest-pinned at Phase 2) in **pull-through cache mode** (`proxy.remoteurl`).
Proxy mode supports exactly one upstream per instance, so the deployment is one Deployment + ClusterIP Service per upstream host: `mirror-docker-io`, `mirror-ghcr-io`, `mirror-quay-io`, `mirror-registry-k8s-io`, `mirror-gcr-io`, the set fixed by the [§2.2](#22-the-inventory) inventory as corrected by the [§2.5](#25-phase-1-validation-graded-2026-08-24) grading.
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

Five upstreams need a mirror instance, per the [§2.2](#22-the-inventory) inventory as corrected by [§2.5](#25-phase-1-validation-graded-2026-08-24): `docker.io`, `ghcr.io`, `quay.io`, `registry.k8s.io`, `gcr.io`.

- **Inner dockerd** (the Kata worker's dind sidecar): `registry-mirrors` in a `daemon.json`, not a flag, because dockerd reads `/etc/docker/daemon.json` unasked, so the wiring is a ConfigMap the overlay mounts and the shipped library entry (`deploy/templates/kata-dind`) stays untouched by a value that is one cluster's.
  This transparently covers every Docker-Hub pull, implicit or explicit, digest-pinned ones included.
  `dockerd` mirrors **only** Docker Hub, and the measured job makes non-Hub docker-client pulls — `ghcr.io/actions-gateway/gmc`, and on a cold cache the `quay.io` and `registry.k8s.io` prepulls.
  Those refs are rewritten to the mirror address (`mirror-ghcr-io:5000/owner/img`), which pull-through mode serves natively, at one call site rather than each, since `scripts/fetch/pull-image-with-retry.sh` is the chokepoint every `docker pull` in the job already goes through ([§3.9](#39-phase-3-build-notes-2026-08-28)).
- **buildkit** (the image bake): resolves `gcr.io/distroless/static:nonroot` for four of the six baked images, and takes neither dockerd's mirror config nor a rewritten docker-client ref, because the ref is in `Dockerfile`.
  Resolved as a generated `buildkitd.toml` rather than a build arg: a build arg reaches only the base images somebody thought to parameterise, and the `Dockerfile` also names `golang` and `ghcr.io/actions/actions-runner`.
  This is the only client whose fetch no cache and no `kind load` can cover ([§2.5](#25-phase-1-validation-graded-2026-08-24)).
- **helm's OCI client** (`chart-released-upgrade-check.sh`): `helm pull oci://ghcr.io/<owner>/charts/…` takes neither of the above.
  The script already parameterises the ref (`RELEASED_CHART_OCI`), so it points at `mirror-ghcr-io:5000/<owner>/charts` with `--plain-http`, added only when a rewrite happened, so a direct GHCR pull is never downgraded off TLS.
- **Inner kind containerd** (`test/kind-config-*.yaml`): nothing required.
  Measured: zero upstream pulls, because every image is `kind load`ed or served from the in-job `127.0.0.1:5000` registry.
  Mirror entries via `containerdConfigPatches` remain an option as a fallback for a preload that goes missing; they would need to be additive-with-fallback (containerd falls back to the upstream when the mirror is unreachable) since the same configs serve local `make e2e` outside the cluster.
- **Go**: closed by the committed `vendor/` tree, not by a proxy — the e2e tenant sets no `GOPROXY` and the measured run used `proxy.golang.org` without fetching from it ([§2.3](#23-what-the-measurement-changes)).
  Under the tight policy that default becomes a latent failure for any `go install` outside `vendor/`; wiring the e2e tenant to Athens the way `scripts/dogfood/setup.sh` wires the main dogfood tenant closes it.

### 3.3 The NetworkPolicy swap

In the **Kata overlay only**: delete `e2e-open-egress`, add `e2e-mirror-egress` — workload pods → mirror pods, registry port only.
The two halves land a phase apart, which makes the second a pure deletion: `e2e-mirror-egress` ships with the manifests in Phase 2, where union with the allow-all policy makes it a no-op ([§3.6](#36-phase-2-build-notes-measured-2026-08-27), decision 3).
The managed default-deny stays authoritative for everything else (DNS + proxy / GitHub).
The dind overlay keeps open egress: it is the explicit trusted-CI fallback (`E2E_VARIANT=dind`), and privileged DinD was never a candidate for untrusted code anyway.

Target: the Kata worker's complete reachable set is **cluster DNS (:53) + GitHub (via the managed rule) + the mirror set**.
Nothing else, including the metadata server on its service ports.

**This swap is only green once the non-registry residual is gone.** Phase 0 measured `get.helm.sh` and the Actions cache data plane as job-time fetches that no mirror serves and no managed rule admits, so Phase 1 has to close them before this policy can be applied — [§2.4](#24-phase-1-decisions-resolved-2026-08-05).

**A policy is a claim about what is unreachable, and no reachability check can grade one.** Every battery up to here probes what *does* answer, which is why the swap needs an instrument of the opposite shape: [§3.12](#312-phase-4-build-notes-2026-08-28)'s negatives, run from inside a job on the worker itself.

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

### 3.6 Phase 2 build notes (measured 2026-08-27)

What the manifests pin, and the readings behind each pin.
All of it was taken against the image locally with `docker`, so it is a measurement of the **image**, not of the Deployment: nothing cluster-side (scheduling, probes, the policies, volume permissions) is covered, and that half is still owed a booked session.

**Image.** `registry:3.1.1@sha256:1be55279f18a2fe1a74edf2664cac61c1bea305b7b4642dab412e7affdcb3e33`, resolved from the Hub manifest index on 2026-08-27.
The floating `3` and `3.1` tags both resolved to that digest; `3.0` did not, so the pin names 3.1.1 rather than the series.

**The image ships the upstream *development* config** at `/etc/distribution/config.yml`: `log.level: debug`, a debug listener on `:5001` serving pprof and `/metrics`, and `storage.delete.enabled: true`.
All three are overridden by env in the pod spec.
The delete override is defence in depth rather than the only closure: proxy mode answered `POST /v2/<name>/blobs/uploads/`, `PUT .../manifests/<tag>` and `DELETE .../manifests/<digest>` with **405** whether `delete.enabled` was true or false, which is [§3.1](#31-the-mirror--one-pull-through-cache-per-upstream)'s read-only property measured rather than argued.
Setting `REGISTRY_HTTP_DEBUG_ADDR` to the empty string unbinds `:5001`, leaving `:5000` as the only listener.

**Non-root needs `fsGroup`, and the failure is loud but misleading.** The image declares no `User` and its `/var/lib/registry` is root-owned, so a container run as uid 65532 with no writable volume at that path answers **every** pull with `500 filesystem: mkdir /var/lib/registry/docker: permission denied` while `GET /v2/` still returns 200.
A readiness probe on `/v2/` would call that pod healthy.
`fsGroup: 65532` is what makes the volume writable, for the `emptyDir` base and the PVC overlay alike.
`readOnlyRootFilesystem: true` is safe alongside it: with the storage volume mounted, `/v2/`, a manifest fetch and a blob fetch all returned 200 with no error lines.

**Anonymous pull-through works against all five upstreams.** One ref from the [§2.2](#22-the-inventory) inventory through each instance, all 200: `library/alpine` and the four of `distroless/static`, `actions-gateway/gmc`, `metrics-server/metrics-server` and `jetstack/cert-manager-controller`.
The ghcr instance also returned 200 for `actions-gateway/charts/actions-gateway:1.2.0`, the OCI **chart** manifest helm pulls, matching a direct-upstream control on the same ref.
That closes the reachability half of [§2.3](#23-what-the-measurement-changes) item 2 at the mirror; pointing helm at it is still Phase 3's.

**Three build decisions the design left open.**

1. **The mirror has its own namespace, `gag-registry-mirror`.** The e2e tenant's `ResourceQuota` caps it at 6 pods against a 2-worker `RunnerSet`, so five instances do not fit beside the workers; a separate namespace also gives [§3.1](#31-the-mirror--one-pull-through-cache-per-upstream)'s "its own ingress rule admits only the worker namespace" something to name.
2. **`e2e-start.sh` applies it, not `e2e-setup.sh`.** [§3.1](#31-the-mirror--one-pull-through-cache-per-upstream) says "the e2e setup script", and the one-time script is the wrong lifecycle: five idle pods would stand on the same contended system pool that Q231 keeps the on-demand e2e AGC off.
   `e2e-stop.sh` scales them to zero and leaves the namespace, the policies and any PVC standing.
3. **`e2e-mirror-egress` ships now rather than in Phase 4.** Applied while the Kata overlay's allow-all `e2e-open-egress` is still present it is a no-op, union with allow-all, which makes Phase 4 a pure deletion instead of a swap.

**Gated, but only as far as a linter reaches.** `deploy/registry-mirror/` joins `make manifest-validate`: yamllint over the tree, and kubeconform over the base manifests plus the overlay's PVCs, which are all native kinds.
That is schema and syntax, and it says nothing about whether the instances serve, so it narrows what is unchecked off the cluster without closing it.
`deploy/kata-ci/` is in the same script's `standalone_manifests` list and in no workflow path filter, so an edit to it alone runs neither check; that gap is filed as [Q1004](../queue/Q1004.md) and is not this phase's.

### 3.7 The Phase 2 validation battery

The plan spelt this out as prose, *curl the mirror's `/v2/` and pull one image through each instance from a debug pod*, and it is now `scripts/dogfood/e2e-mirror-validate.sh`, because Phase 3 re-runs the same battery once the clients are wired and Phase 4 re-runs it under the tight policy.
A booked dogfood session is the scarce resource in all three, so the battery is a command that returns a verdict rather than a transcript somebody re-reads.

Five checks per instance, over the five declared upstreams:

| Check | Passes on | What it catches |
|---|---|---|
| `available` | the Deployment's `Available` condition | an instance that never scheduled |
| `v2` | `GET /v2/` → 200 | the process is not up |
| `manifest` | a real upstream manifest → 200 | **a mirror that cannot cache anything**, see below |
| `push` | `POST …/blobs/uploads/` → 405 | [§3.1](#31-the-mirror--one-pull-through-cache-per-upstream)'s read-only property is false |
| `debug` | `REGISTRY_HTTP_DEBUG_ADDR` set and empty in the pod spec | the bundled development config's pprof + `/metrics` listener is still bound |

**`manifest` is the check that discriminates, and `v2` is the one that would have lied.** [§3.6](#36-phase-2-build-notes-measured-2026-08-27) records that a non-root instance whose storage root is unwritable answers 200 on `/v2/` and 500 on every pull; a battery graded on `/v2/` alone calls that mirror healthy.
The same reading is why the readiness probe cannot stand in for this script.

**`debug` is read off the Deployment rather than probed, because the network reading cannot work.** Each Service declares one port (`5000/5000`) and `registry-mirror-worker-access` admits only TCP/5000, so a connection to a ClusterIP on 5001 never reaches the pod whatever the listener is doing.
The dataplane decides that result, which means such a probe grades the Service and the policy rather than the config it names, and both of its outcomes are wrong: an unmatched ClusterIP port that is dropped times out, so a healthy cluster reports five failures on the first booked session, and one that is rejected gives the same refusal the probe scores as healthy whether or not the listener is bound.
Which of the two this cluster does is unmeasured, and the object read is correct either way.
The other observable reading, an ephemeral container in the pod's own netns, is not taken either: this namespace enforces PSA `restricted`, `kubectl debug` sets no `securityContext` on the container it injects, and whether that is admitted is a venue question no run off the cluster can settle.

**The env read carries a trap of the same shape.** kubectl's jsonpath renders an empty value and an absent entry identically, and an absent entry is exactly the state where the bundled listener *is* bound.
So the check reads the entry's name alongside its value: name present with an empty value passes, name absent fails.

**Every expected value above was measured, and every control fires.** The three probe checks were taken against five instances in proxy mode on a Docker network, probed by a `curlimages/curl` container addressing them at their in-cluster names: all five returned 200 / 200 / 405.
One run non-root with no writable storage answered `/v2/` 200 and its manifest **500**, which is the fsGroup shape reproduced.
`debug` was measured separately, with the script's own jsonpath against the real manifests and two hand-built controls: healthy renders `True|REGISTRY_HTTP_DEBUG_ADDR|`, an instance with the entry removed renders `True||`, and one bound to an address renders `True|REGISTRY_HTTP_DEBUG_ADDR|:5001`.
So the battery can show the opposite of what it reported.

**The probe rides a worker's path, and that is not the same as proving enforcement.** The probe pod runs in the tenant namespace carrying `actions-gateway/component=workload`, so it is selected by `e2e-mirror-egress` and admitted by the mirror-side `registry-mirror-worker-access` ingress rule.
A wrong `namespaceSelector` on either shows up as a timeout on the three probe checks, every one of which addresses 5000, the single port both the Service and the policy carry.
But the Kata overlay's allow-all `e2e-open-egress` is still in place until Phase 4, so reachability through the mirror does not yet distinguish the mirror path from the open one.
The negatives that do are Phase 4's.

**What no battery closes: an absent result reads as an absent failure.** A probe pod that is evicted or dies before its first line produces empty output, and a grader that walks what it received finds nothing wrong with it: green from an instrument that ran nothing.
So the expected set is the declared instance table rather than the transcript, and a check that did not report is a failure with its own reason.
The same argument is why availability is read per declared instance instead of from a `-l app=registry-mirror` listing: derived from a listing, four healthy mirrors out of five declared grade green.
Both properties are pinned by `scripts/dogfood/e2e-mirror-validate-test.sh` under `make check`, and both were confirmed by inverting the script and requiring red.

### 3.8 Phase 2 validation: graded (2026-08-28)

Run against `gag-dogfood` (`us-east1-b`, `actions-gateway-dogfood`), Kata overlay, ephemeral mirror caches, by `scripts/dogfood/e2e-start.sh` followed by `scripts/dogfood/e2e-mirror-validate.sh`.

**25 of 25 checks passed**, which is the five declared instances times [§3.7](#37-the-phase-2-validation-battery)'s five checks: five `available`, five `debug`, and fifteen probe results over `v2`, `manifest` and `push`.
The count is the reconciliation rather than a tally of the transcript: the expected set is the declared instance table, so a check that did not report would have been a `FAIL` with its own reason instead of shrinking the battery.

**The discriminating check is the one that passed.** Every instance answered 200 on a real upstream manifest, so the unwritable-storage-root shape [§3.6](#36-phase-2-build-notes-measured-2026-08-27) measured locally, where `/v2/` answers 200 and every pull answers 500, is absent here.
All five refused an upload with 405, so [§3.1](#31-the-mirror--one-pull-through-cache-per-upstream)'s read-only property is now measured on the cluster rather than argued.
`REGISTRY_HTTP_DEBUG_ADDR` was present and empty in all five pod specs, so the bundled image's :5001 pprof and `/metrics` listener is unbound.

**What this does not establish is enforcement**, exactly as [§3.7](#37-the-phase-2-validation-battery) states.
The Kata overlay's allow-all `e2e-open-egress` is still in place, so reachability through the mirror does not distinguish the mirror path from the open one.
The probe did ride a worker's own policy pair, which is real coverage of `registry-mirror-worker-access` and `e2e-mirror-egress`, but the negatives that separate the two paths are Phase 4's.

**Two findings about the session path, neither about the mirror.** `e2e-start.sh` applies the tenant without waiting for GMC, so from the 0-node at-rest state its apply fails on `no endpoints available for service "webhook-service"` after it has already resized the pool.
[release.md](../operations/release.md) documents the remedy by ordering `start.sh` ahead of it; the failure is the documented ordering being skipped rather than a defect, and it costs a pool resize either way.
Separately, `e2e-start.sh` and `e2e-stop.sh` were committed non-executable, so the bare invocation both this plan and `release.md` prescribe exited 126 before reading an env var (Q1013, fixed with a gate against the whole class).

### 3.9 Phase 3 build notes (2026-08-28)

What the wiring landed as, and the readings behind each decision.
All of it is off-cluster: the clients are configured and their configuration is gated, and **nothing here says a pull rode a mirror**.
That is the booked session's, and [§3.10](#310-the-phase-3-validation-reading) is the instrument for it.

**The endpoint set lives in one ConfigMap, in the two forms the clients can read.** `registry-mirror-wiring` in the tenant namespace ([`mirror-wiring.yaml`](../../deploy/dogfood-e2e/overlays/kata/mirror-wiring.yaml)) carries `daemon.json` for the inner dockerd and a `<upstream>=<mirror>` map for everything else.
Two forms because no two of the four clients read the same configuration, and one object because they are copies of one fact.

**dockerd is wired by a mounted `daemon.json`, not by a flag.** [§3.2](#32-wiring-the-clients) said "dind sidecar `args`", and the sidecar's `args` are a 60-line entrypoint in the *shipped library entry* `deploy/templates/kata-dind`, which Q554 made the artifact an operator applies.
A flag there would put one cluster's Service address in a shipped template; a JSON 6902 patch replacing that `args` string in the overlay would restate all 60 lines. dockerd reads `/etc/docker/daemon.json` without being told to, so the overlay mounts the ConfigMap key at that path and the template is untouched.
`insecure-registries` lists all five endpoints alongside: transport is plain in-cluster HTTP, which dockerd refuses to an `http://` mirror otherwise.

**The ref rewrite is at one chokepoint, not at each call site.** [§3.2](#32-wiring-the-clients) said "at their call sites", and the call sites turn out to be one: every `docker pull` the job makes already goes through `scripts/fetch/pull-image-with-retry.sh`: the buildkit builder image, the kind node image, curl, Vault, the three `prepull-manifest-images.sh` consumers (cert-manager, metrics-server, Calico), and the released GMC image in `chart-released-upgrade-check.sh`.
So the rewrite is there, reading the map through the shared [`scripts/lib/registry-mirror.sh`](../../scripts/lib/registry-mirror.sh).

The property that makes it safe is that **the daemon is left as a direct pull would have left it**: pull from the mirror, `docker tag` back to the caller's ref, `docker rmi` the mirror's.
Callers `docker save`, `kind load` and write `images.txt` under the ref they asked for, and kubelet resolves the manifests' own names, so a rewrite that stopped at the pull would pass and fail a `kind load` several steps later.

**A digest-bearing ref is deliberately not rewritten.** `docker pull name:tag@digest` stores the image under `name@digest`, and a digest is not a legal `docker tag` target, so the local reference could not be restored.
Nothing is thereby off the mirror: every digest-pinned ref in the [§2.2](#22-the-inventory) inventory belongs to a client carrying its own mirror config, Hub's to dockerd and buildkit's to `buildkitd.toml`, which redirects it without touching the name.

**buildkit gets a generated `buildkitd.toml`, which is the open choice in [§3.2](#32-wiring-the-clients) resolved against a build arg.** A build arg reaches only the base images somebody parameterised, and `distroless/static` is four of the `Dockerfile`'s eight `FROM`s; `golang:1.26` and `docker:29-cli` are Hub's and `ghcr.io/actions/actions-runner` is GHCR's, none of which dockerd's mirror or a rewritten ref reaches, because buildkit resolves them itself.
[`scripts/e2e/buildkitd-mirror-config.sh`](../../scripts/e2e/buildkitd-mirror-config.sh) renders one `[registry]` block per upstream from the same map and hands the path to `docker/setup-buildx-action`'s `buildkitd-config` input; with no map it prints nothing, and an empty value is falsy there, so no `--config` reaches `docker buildx create`.
Each upstream's block is paired with an `http = true` block for the mirror itself, without which buildkit dials the mirror over TLS and **falls back to the upstream**, which is a green build that never rode the mirror and exactly the reading [§3.10](#310-the-phase-3-validation-reading) has to be able to fail.
That `http` goes on the mirror's own entry rather than the upstream's, which is where `buildkitd.toml.md`'s example puts it: the resolver builds each mirror host with `fillInsecureOpts(mirrorHost, m[mirrorHost], h)`, keyed on the mirror, and the documented placement would also downgrade the fallback path to plain HTTP, which is the opposite of the point.

The same class of reading corrected the workflow input: `docker/setup-buildx-action` v4 declares `buildkitd-config`, not `config`, and an action silently ignores an input it does not declare, so the first form would have booted a builder with no mirror config and gone green. actionlint passes on both, since it does not validate a third-party action's inputs.

**The map cannot be a workflow `env`, and that is what decides where it lives.** `e2e-reusable.yml` serves the hosted lane too, and the `env` context holds only what a workflow declares, so a value supplied by the cluster cannot be read by `${{ env.… }}` in a `with:` block.
It is set on the worker's runner container instead, from the same ConfigMap, and job steps inherit it the way they already inherit `DOCKER_HOST`.
The consequence is the one worth having: no cluster-local Service address enters this repo's workflows, and every caller with no map set pulls direct, unchanged: the hosted lane, `publish.yml`, a developer's `make e2e`.

**Three hand-kept copies of one endpoint set, so they are gated.** The instances, their Services and the ConfigMap can each be edited without the others.
An upstream added on one side only leaves its pulls going direct, and under this phase's own posture that is **green**: open egress is still in place, the run passes, the mirror is simply unused.
`make registry-mirror-wiring-check` ([`scripts/manifest/check-registry-mirror-wiring.py`](../../scripts/manifest/check-registry-mirror-wiring.py)) reconciles all three in both directions, reads each instance's upstream from its `REGISTRY_PROXY_REMOTEURL` rather than from its name, and refuses with rc 2 rather than reporting a consistent tree when an extraction comes back empty.

### 3.10 The Phase 3 validation reading

The phase's success condition is a green Kata e2e run whose mirrors show hits, and the green run is the half that proves nothing.
`e2e-open-egress` is still in place, so a client that ignored its wiring reaches its upstream and the suite passes identically.
The reading that discriminates is the mirror's own access log, which no unmirrored pull can write into, and it is `scripts/dogfood/e2e-mirror-hits.sh` for the reason [§3.7](#37-the-phase-2-validation-battery)'s battery is a command: the booked session is the scarce resource.

Run it after the e2e run and **before `e2e-stop.sh`**, which scales the mirrors to zero and takes their logs with the pods.

**A hit is not a request.** Distribution's access log is Combined Log Format, one line per request, and the kubelet's readiness and liveness probes both `GET /v2/`, every 10 and every 20 seconds, on every instance, whether or not anything ever pulled through it.
So a hit is a request whose path is *deeper* than `/v2/`, and the verdict needs one that was also **served** (2xx/3xx), since an instance answering 500 to every pull is [§3.6](#36-phase-2-build-notes-measured-2026-08-27)'s unwritable-storage-root shape and is not a mirror that worked.

**The other writer of that log is [§3.7](#37-the-phase-2-validation-battery)'s battery, and it is the one that would make this reading unfalsifiable.** The battery fetches a real manifest and attempts an upload against every instance, so a session that runs it first, which this plan's sequence does and Phase 4 does again, leaves all five non-zero before the job starts.
Measured on the cluster ([§3.11](#311-phase-3-validation-graded-2026-08-28)): the battery alone puts 2 content requests and 1 served on each instance, which is a PASS from an instrument that measured nothing.
So a hit must also come from a client that is not that probe, and the discriminator is the user agent: real pulls carry docker's, helm's or buildkit's, the probe carries curl's.
The exclusion is written to fail safe, since an unrecognised client agent under-counts and reports FAIL, which gets investigated, while a probe whose curl version bumps stays excluded because the pattern is the client name rather than the version.

Both readings were measured rather than assumed: `registry:3.1.1` was run locally in proxy mode with the Deployment's own env on 2026-08-28 and probed, and the log lines it emitted are the test's fixture, verbatim.
Without that the parser would be the failure this instrument is most exposed to: a query that never matches counts zero, and zero here is a FAIL, so a broken parser wastes the session rather than passing it.

The expected set is the declared instance table, not the transcript, so an instance whose log cannot be read is a named failure rather than a battery that quietly shrank to four.
Both properties, and the 500-answering control, are pinned by `scripts/dogfood/e2e-mirror-hits-test.sh` under `make check`, and the parser was confirmed by inverting it and requiring red.

**What it still does not establish is enforcement**, exactly as in Phase 2.
A hit says the wiring rode the mirror; it says nothing about whether the upstream was also reachable.
The negatives that separate the two paths are Phase 4's.

### 3.11 Phase 3 validation: graded (2026-08-28)

Run against `gag-dogfood` (`us-east1-b`, `actions-gateway-dogfood`), Kata overlay, ephemeral mirror caches.
Sequence: `start.sh`, `e2e-start.sh`, the [§3.7](#37-the-phase-2-validation-battery) battery, a mirror restart (below), `scripts/dogfood/e2e-mirror-hits.sh` for a baseline, one dispatched run of `e2e-test.yml` against this phase's branch with `runner='"gag-ci-e2e"'`, then the same hits reading again.

**The e2e run is green**: [33190837137](https://github.com/actions-gateway/github-actions-gateway/actions/runs/33190837137), 75 of 75 specs, 62 passed, 0 failed, 13 skipped, 8m19s in the suite.
It is the cold run Phase 3 asks for without anything being arranged: Phase 1's gating skipped `azure/setup-helm` and every `actions/cache` step on this lane, which the job's step list records.

**All five instances served, and the verdict is a change from a measured zero.**

| Instance | Content requests | Served | Repositories |
|---|---|---|---|
| `mirror-docker-io` | 58 | 58 | `kindest/node`, `library/registry`, `curlimages/curl`, `hashicorp/vault`, `moby/buildkit`, `library/golang`, `docker/dockerfile` |
| `mirror-ghcr-io` | 36 | 36 | `actions-gateway/charts/actions-gateway`, `actions-gateway/gmc`, `actions/actions-runner` |
| `mirror-quay-io` | 33 | 33 | the three `jetstack/cert-manager-*` |
| `mirror-registry-k8s-io` | 17 | 17 | `metrics-server/metrics-server` |
| `mirror-gcr-io` | 17 | 17 | `distroless/static` |

**The repositories are what make each client separately proven**, rather than the totals.
`actions-gateway/charts/actions-gateway` is the OCI chart and can only be helm's; `distroless/static` can only be buildkit's; the three cert-manager repositories and `metrics-server` can only be the rewritten prepull refs; `kindest/node` and `library/registry` can only be dockerd's own mirror, since the first is digest-pinned and the rewrite deliberately leaves those alone.
So the table is four independent readings rather than one.

**Two of buildkit's three rows were not predicted by [§3.2](#32-wiring-the-clients), and they are the argument for the config file.** `library/golang`, `docker/dockerfile` and `actions/actions-runner` are the builder's Go base, its frontend, and the worker stage's base.
A build arg parameterising `distroless/static` alone would have left all three going direct, and Phase 4 would then have failed on them.

**The baseline is the control.** The same script was run twenty minutes earlier, after the mirrors were restarted and before the run was dispatched, and reported all five instances `FAIL` at 0 content requests.
So the instrument is measured able to report the opposite on this cluster rather than argued to be.

**A finding about the two instruments, not about the wiring.** The [§3.7](#37-the-phase-2-validation-battery) battery fetches a real manifest and attempts an upload per instance, and both land in the access log the hit counts read, so running it first leaves every instance non-zero before the job starts.
Left alone that makes the Phase 3 reading unfalsifiable.
This session cleared it with a `kubectl rollout restart` of the mirror Deployments between the battery and the dispatch, which costs a cold cache and nothing else, and the baseline below was taken after that restart.
Phase 4 re-runs both instruments in the same session, so the hazard is real again there and does not belong in an operator's memory: the script now discounts the probe by its user agent ([§3.10](#310-the-phase-3-validation-reading)), and deleting that exclusion turns a battery-only log back into 2 content and 1 served, which its suite requires.

**What this does not establish is enforcement.** `e2e-open-egress` was in place throughout, so the mirrors being used does not mean the upstreams were unreachable.
The negatives that separate the two paths are Phase 4's.

### 3.12 Phase 4 build notes (2026-08-28)

The deletion is one file: the Kata overlay's `resources.yaml` held the `e2e-open-egress` object and nothing else, so removing it takes the object, its header comment, and the comment's wrong claim that Phase 3 was the replacing phase.
What a Kata worker can then reach is the managed default-deny plus [§3.1](#31-the-mirror--one-pull-through-cache-per-upstream)'s mirrors: cluster DNS on 53, the GitHub CIDRs on 443 (this tenant sets no `defaultProxyRef`, so `buildWorkloadNetworkPolicyV2` takes the direct-egress branch), and TCP 5000 to the five mirror pods.
The dind overlay keeps its policy and its object name; nothing applies both overlays at once.

**The negatives run inside the job, and that is a change from where the other two batteries run.** [§3.7](#37-the-phase-2-validation-battery)'s probes from a pod carrying the workload label, which rides the same policy pair a worker does and is the right instrument for the mirror's own service.
It is the wrong instrument for this phase, because the claim Phase 4 makes is about a **Kata** worker: its dockerd runs inside a micro-VM guest and its inner containers reach the network through a bridge NAT inside that guest, and whether policy still binds at the end of that path is exactly what an ordinary pod cannot answer.
So `scripts/e2e/egress-negatives.sh` runs as a job step, on the worker whose posture is being claimed.

**Every negative is paired with a positive, and that pairing is the design rather than a courtesy.** A battery of nothing-is-reachable checks passes identically on a worker with no network at all, on one whose DNS is down, and on a step that ran somewhere it was never meant to: a green verdict from an instrument that could not have failed.

| Check | Probe | Passes when | Why it is in the battery |
|---|---|---|---|
| `mirror-reachable` | `GET http://<hub mirror>/v2/` | 200 | Control: without it every blocked check below is unfalsifiable |
| `github-reachable` | `GET https://api.github.com/` | any HTTP status | Control: the managed rule still admits the one upstream that stays reachable |
| `mirror-readonly` | `POST` to a blob upload | 405 | [§3.1](#31-the-mirror--one-pull-through-cache-per-upstream)'s read-only property, from the worker rather than from a probe pod |
| `docker-mirror-pull` | `docker pull` the rewritten `gcr.io` ref | exit 0 | Control for the row below: dockerd works, and works through the mirror |
| `upstream-blocked` | `GET https://gcr.io/v2/` | no HTTP status | A mirrored upstream, reached by its own hostname |
| `internet-blocked` | `GET https://example.com` | no HTTP status | Neither GitHub nor a registry |
| `metadata-blocked` | `GET http://169.254.169.254/computeMetadata/v1/` | no HTTP status | The link-local DNS rule admits 169.254.0.0/16 on **port 53 only**; this is port 80 |
| `docker-upstream-blocked` | `docker pull gcr.io/distroless/static:nonroot` | non-zero exit | The same image as `docker-mirror-pull`, direct, and the pair is what makes either mean something |

`gcr.io` rather than Docker Hub for the pull pair, because dockerd's `registry-mirrors` redirects Hub and only Hub: a Hub ref rides the mirror transparently and could never be the negative.
`distroless/static` because it is the smallest ref in the [§2.2](#22-the-inventory) inventory.

**A dropped packet has no error that distinguishes it from a slow one**, so a blocked probe spends its whole timeout and the step takes minutes when the posture holds.
That is also the one way this battery reports a false PASS: shortening the timeouts makes a slow upstream look blocked.
They are named constants for that reason.

**It runs on every run of the lane, not once in the session that ships the deletion.** The posture is a property to hold rather than a milestone to reach, and a policy that stops selecting the worker is invisible in a green suite, the same reason Phase 3's wiring needed a reading of its own.
The step carries no `if:`: `REGISTRY_MIRRORS` is set on the runner process by the cluster, so the `env` context cannot see it, and the script reports a skip wherever the map is absent (the hosted lane, a developer's `make e2e`).
A map that is *present but missing* an upstream the battery names is not a skip, since it would drop a control rather than the whole battery, so it fails loudly.

**Deleting the manifest does not delete the object, and the cluster is the proof.** `kubectl apply -k` does not prune, and `e2e-stop.sh` deliberately leaves the tenant's NetworkPolicies standing so a window can reopen without re-deriving them, so the `e2e-open-egress` applied by an earlier window survives every later apply.
Measured on `gag-dogfood` before this session's run: the object was still there, 28 days old, with the manifests that would create it already gone from the branch.
Nothing off the cluster can see that, and the consequence is not a failed run but a passed one: the negatives would have reported the posture unenforced, correctly, for a reason that has nothing to do with the deliverable.
So `e2e-start.sh` deletes it on the Kata lane, `--ignore-not-found` because the steady state is that it is already gone, and the dind lane is left alone because that overlay owns the policy.
This is a property of the recipe rather than of this cluster: an operator adopting the mirror on a cluster that already ran open-egress CI meets the same gap.

**The session sequence**, so the booked run is a command list rather than improvisation.
`start.sh`, then `e2e-start.sh` with the Kata overlay of this branch, then [§3.7](#37-the-phase-2-validation-battery)'s battery (its reachability checks stop being ambiguous here: the open path they could also have ridden is gone), then one dispatched run of `e2e-test.yml` against this branch with `runner='"gag-ci-e2e"'`, then `scripts/dogfood/e2e-mirror-hits.sh` before `e2e-stop.sh` takes the access logs with the pods.
The negatives need no separate invocation: they are a step of that run, and a red step is the run's verdict.
The battery still writes into the same access log the hit counts read, which is the [§3.11](#311-phase-3-validation-graded-2026-08-28) hazard; it no longer needs a mirror restart to clear, because the hit counter discounts the probe by its user agent.

**One Phase 4 check from [§4](#4-phases) is dropped rather than built, and the plan's own §3.2 is why.** That bullet asked for a kind-side pull of a non-local-registry image succeeding via the mirror.
The inner kind containerd is not wired to the mirror, deliberately: [§2.2](#22-the-inventory) measured zero upstream pulls from it, every image being `kind load`ed or served from the in-job `127.0.0.1:5000` registry, and a grep of `cmd/gmc/test/e2e/` finds no upstream ref in any spec.
So the check as written could only pass by adding `containerdConfigPatches`, which [§3.2](#32-wiring-the-clients) sequences as a fallback for a preload that goes missing rather than a deliverable, and [§7](#7-success-criteria) asks for negative probes, which this is not.
What the tight policy does to that path is make a missing preload fail closed instead of silently pulling upstream, which is the better of the two behaviours and needs nothing built.

### 3.13 Phase 4 validation: graded (2026-08-28)

Run against `gag-dogfood` (`us-east1-b`, `actions-gateway-dogfood`), Kata overlay, ephemeral mirror caches, from a cluster at its at-rest zero nodes.
Sequence: `e2e-start.sh`, the [§3.7](#37-the-phase-2-validation-battery) battery, one dispatched run of `e2e-test.yml` against this phase's branch with `runner='"gag-ci-e2e"'`, then `scripts/dogfood/e2e-mirror-hits.sh`, then `e2e-stop.sh`.
`start.sh` was not used: it dispatches unit and integration bursts this phase has no use for, and `e2e-start.sh` sizes the system pool itself.

**The e2e run is green**: [33223143842](https://github.com/actions-gateway/github-actions-gateway/actions/runs/33223143842), 75 of 75 specs, 62 passed, 0 failed, 13 skipped, 7m42s in the suite.
It ran at `33b6578`, a SHA a later rebase onto `origin/main` rewrote, so read the run rather than the hash.
Everything executable is byte-identical to what ran: `git diff` against that commit returns two comments, the measured 78 s replacing a guess in `egress-negatives.sh` and in the workflow step, plus prose in the mirror README.
No probe, expectation, manifest or policy moved.
Identical to [§3.11](#311-phase-3-validation-graded-2026-08-28)'s counts, so removing the open-egress policy cost the suite nothing.

**The worker was a Kata worker, and it carried the label the policies select.** `runtimeClassName: kata` on the e2e node pool, with `actions-gateway/component=workload`, which is what both the GMC-managed default-deny and `e2e-mirror-egress` match on.
Worth stating because it is the premise the whole phase rests on: a battery that ran on an ordinary pod would prove nothing about the guest.

**The tenant's egress set, read off the live objects**, is `dogfood-e2e-workload` and `e2e-mirror-egress` and nothing else, with **zero allow-all rules** between them: port 53 to the three cluster-DNS peers, port 443 to 7,304 GitHub CIDR blocks, port 8080 to the proxy pods, port 5000 to the mirror pods.

**The negatives all pass**, in 78 seconds, as a step of the run itself:

| Check | Result |
|---|---|
| `mirror-reachable` | PASS |
| `github-reachable` | PASS, HTTP 200 |
| `mirror-readonly` | PASS |
| `docker-mirror-pull` | PASS |
| `upstream-blocked` | PASS, no HTTP status |
| `internet-blocked` | PASS, no HTTP status |
| `metadata-blocked` | PASS, no HTTP status |
| `docker-upstream-blocked` | PASS |

The four controls are what make the four negatives readable: GitHub answered 200, and the same `gcr.io/distroless/static:nonroot` that could not be pulled directly was pulled through the mirror in the same battery, so the three silent destinations were silent because of the policy rather than because the worker had no network.

**The step's exit status was not taken as the reading.** `egress-negatives.sh` exits 0 on a skip too, so a green step is consistent with `REGISTRY_MIRRORS` never being set — an instrument that measured nothing, which is the failure this plan has now hit once per phase.
Two independent readings settle it: the step's own PASS lines, quoted above, and the mirror access log, where the runner image's `curl/8.5.0` appears exactly twice on `mirror-docker-io`, which is the battery's `/v2/` and blob-upload pair.
The [§3.7](#37-the-phase-2-validation-battery) probe pod is `curlimages/curl:8.10.1`, so the two writers cannot be confused.

**Phase 2's battery re-run under the tight policy: 25 of 25.** Its probe pod carries the workload label, so this is a stronger reading than [§3.8](#38-phase-2-validation-graded-2026-08-28)'s: with no open path to ride, reaching an upstream manifest through the mirror can only have gone through the mirror.

**The hit counts: five of five, 178 served content requests**, up from Phase 3's 161.

| Instance | Content requests | Served | Repositories |
|---|---|---|---|
| `mirror-docker-io` | 58 | 58 | `kindest/node`, `library/registry`, `curlimages/curl`, `hashicorp/vault`, `moby/buildkit`, `library/golang`, `docker/dockerfile` |
| `mirror-ghcr-io` | 36 | 36 | `actions-gateway/charts/actions-gateway`, `actions-gateway/gmc`, `actions/actions-runner` |
| `mirror-quay-io` | 33 | 33 | the three `jetstack/cert-manager-*` |
| `mirror-registry-k8s-io` | 17 | 17 | `metrics-server/metrics-server` |
| `mirror-gcr-io` | 34 | 34 | `distroless/static` |

`mirror-gcr-io` is the one instance that moved, 17 to 34, which is the negatives' own `docker-mirror-pull` added to the bake's resolutions.
No mirror restart was needed this time: the hit counter discounts the [§3.7](#37-the-phase-2-validation-battery) probe by its user agent, which is the fix [§3.11](#311-phase-3-validation-graded-2026-08-28) asked for, working.

**One failure, and it was the bring-up rather than the posture.** The first `e2e-start.sh` failed its tenant apply with `No agent available` from the conversion webhook, a third variant of that error which [troubleshooting.md](../operations/troubleshooting.md#applying-any-v2-object-fails-with-no-endpoints-available-for-service-webhook-service) does not cover: the GMC was `1/1 Running` and the `webhook-service` EndpointSlice was populated, but GKE's konnectivity agent had not connected, so the control plane could reach no in-cluster webhook at all.
Re-running the same script unchanged succeeded.
Filed as [Q1016](../queue/Q1016.md) rather than fixed here, since it is bring-up sequencing and this phase is egress.

## 4. Phases

Each phase is a separate PR; 0 to 4 need live dogfood evidence.
Needing it is not the same as booking a session (prod-guarded: a deliberate, operator-driven run): Phases 0 and 1 read theirs off runs that had already happened, so only 2, 3 and 4 book one.
Phase 2 cannot read its evidence off a prior run, because none applied manifests that did not yet exist.
No off-cluster gate stands in either, whatever `deploy/registry-mirror/` ends up linted by: schema and syntax checks cannot tell whether an instance serves, which is what Phase 2's validation turns on.

- **Phase 0 — measured egress inventory. ✅ Done (2026-08-03).** Read off a green Kata dogfood run that had already happened rather than booking a new one; [§2](#2-the-gap--what-an-e2e-job-actually-fetches-at-job-time-phase-0) is the deliverable, and [§2.3](#23-what-the-measurement-changes) is what it cost the design.
- **Phase 1 — shrink the non-registry residual to GitHub. ✅ Done (implemented 2026-08-05, validated 2026-08-24).** New phase, forced by Phase 0.
  The [§2.4](#24-phase-1-decisions-resolved-2026-08-05) decisions are resolved: `e2e-reusable.yml` gates `azure/setup-helm`, every `actions/cache` step, and `GHA_CACHE` to `runner.environment == 'github-hosted'`.
  Validated by grading four green self-hosted Kata runs against a Phase 0 control ([§2.5](#25-phase-1-validation-graded-2026-08-24)), and no dogfood session was booked because the qualifying runs had already happened.
  The grading also corrected the upstream set from four to five.
- **Phase 2 — mirror manifests. ✅ Done (authored 2026-08-27, validated 2026-08-28).** `deploy/registry-mirror/` (Athens-shaped: base, persistent overlay, README, NetworkPolicies), one instance per upstream (`docker.io`, `ghcr.io`, `quay.io`, `registry.k8s.io`, `gcr.io`, the fifth added by [§2.5](#25-phase-1-validation-graded-2026-08-24)), applied by `scripts/dogfood/e2e-start.sh` and scaled back to zero by `e2e-stop.sh`.
  What is pinned, and what was measured to pin it, is [§3.6](#36-phase-2-build-notes-measured-2026-08-27).
  Validation: `scripts/dogfood/e2e-mirror-validate.sh` against the dogfood cluster once `e2e-start.sh` has applied the manifests: five checks per instance ([§3.7](#37-the-phase-2-validation-battery)), read-only, and it applies no manifest of its own.
  Graded 2026-08-28 at 25 of 25 over the five declared instances ([§3.8](#38-phase-2-validation-graded-2026-08-28)), which discharges the **precondition** [release-1.7.md](release-1.7.md) sets on Phase 3's run: "manifests must serve before wiring can be proven to ride them".
  It booked a dogfood session of its own, per this section's header.
- **Phase 3 — wiring. ✅ Done (built and validated 2026-08-28).** dockerd via a mounted `daemon.json` in the Kata overlay; non-Hub docker-client refs rewritten at the one chokepoint they all share; buildkit given a generated `buildkitd.toml`; helm's OCI ref pointed at the ghcr mirror.
  What each resolved to, and why, is [§3.9](#39-phase-3-build-notes-2026-08-28).
  Validation: a green Kata e2e run **with open egress still present**, then `scripts/dogfood/e2e-mirror-hits.sh` reporting a served content request per instance ([§3.10](#310-the-phase-3-validation-reading)), so the wiring is proven before enforcement changes.
  Graded 2026-08-28 at five of five instances over 161 served content requests, against a zero baseline measured on the same cluster ([§3.11](#311-phase-3-validation-graded-2026-08-28)).
  The run was cold without arranging it: the self-hosted lane has had no `actions/cache` step since Phase 1.
- **Phase 4 — enforcement. ✅ Done (built and validated 2026-08-28).** Delete `e2e-open-egress` from the Kata overlay.
  A deletion rather than a swap: Phase 2 shipped `e2e-mirror-egress`, where it is a no-op under the allow-all policy ([§3.6](#36-phase-2-build-notes-measured-2026-08-27), decision 3).
  The overlay's own comment on that policy named Phase 3 as the replacing phase, which was wrong and needed no separate fix: that file held this one object, so the deletion took the comment with it.
  What was built, and the one check of the three below that is dropped rather than built, is [§3.12](#312-phase-4-build-notes-2026-08-28).
  Validation on dogfood: (a) a green Kata e2e run under the tight policy; (b) the negatives from inside a job, meaning a mirrored upstream reached by its own hostname, the plain internet and the metadata server all unreachable, each against a control that must answer, plus a push to the mirror refused; (c) **dropped** — a kind-side pull of a non-local-registry image cannot succeed via the mirror, because the inner containerd is deliberately not wired to it ([§3.12](#312-phase-4-build-notes-2026-08-28)).
  Re-run [§3.7](#37-the-phase-2-validation-battery)'s battery here too: under the tight policy its reachability checks stop being ambiguous, since the open path they could also have ridden is gone.
  Graded 2026-08-28 at eight of eight negatives, 25 of 25 on that battery, and 178 served content requests across the five instances ([§3.13](#313-phase-4-validation-graded-2026-08-28)).
- **Phase 5 — docs + close-out. ✅ Done (2026-08-28).** [kata-dind-workloads.md](../operations/kata-dind-workloads.md#untrusted-pull-requests--the-tight-egress-posture)'s "validated posture is trusted CI" caveat became the how-to: the four parts, the client wiring, the adoption steps, the three readings that confirm it, and what it does not cover.
  [security-operations.md](../operations/security-operations.md#prefer-an-in-cluster-caching-mirror-first) names the mirror as the load-bearing half of the posture rather than only a recommendation; [in-runner-image-builds.md](../operations/in-runner-image-builds.md#approach-5--kata-containers-micro-vm-dind-no-privileged-container), [network-architecture.md](../design/network-architecture.md) and [personas.md](../operations/personas.md) carry cross-refs.
  G.14 is ✅ implemented; the roadmap bullet is deleted and the capability appears on [Features](../features.md) instead, since the roadmap is for what GAG does not do yet.
  The row is deleted and every citation de-linked.
  What Phase 5 deliberately did **not** do is claim untrusted-PR readiness: [the umbrella goal](secure-multi-tenant-oss-ci.md#definition-of-done) has five criteria and this work closes three, so the operator page states the posture and its measurements and names the two that are open.

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

- **[Q539](../queue/Q539.md) — Kata + Dragonfly as the mirror backend.** Substitute a Dragonfly deployment for the Distribution instances behind the same wiring and the same tight NetworkPolicy, and prove it meets the §3.5 contract: workers reach only a seed-peer ClusterIP Service (the `hostNetwork` dfdaemon is not policy-selectable), back-to-source is restricted to the Phase-0 upstream allowlist, non-GET registry operations are refused, and workers never participate in the P2P mesh.
  Same Phase-4 validation battery, plus negatives for the contract points.
  Needs its own plan doc when picked up.
- **[Q540](../queue/Q540.md) — the composed stack as a milestone: Kata + Dragonfly (node layer) + pull-through cache (guest layer).** The two operate at different layers (§5), so this validates the composition an image-heavy fleet would actually run: Dragonfly accelerating node-level pod-image distribution via containerd mirror config, the in-guest mirror serving untrusted job pulls, each with its own policy scope — and confirms neither weakens the other's posture (in particular that the node-layer P2P mesh stays unreachable from worker pods).

## 7. Success criteria

**Met 2026-08-28.** The dogfood Kata e2e variant ran green with `e2e-open-egress` deleted, the negative probes confirmed enforcement, and the Phase 5 docs made the recipe the supported posture.
"Kata-isolated runners are only suitable for trusted CI" is gone from the docs, replaced by the reference architecture for untrusted-PR CI.
The criteria bound the mirror work only; the broader milestone they sit under is [secure-multi-tenant-oss-ci.md](secure-multi-tenant-oss-ci.md#definition-of-done), where two of five items remain open.
