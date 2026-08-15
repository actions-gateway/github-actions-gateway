# GKE Dogfood Runbook

On-demand GKE cluster for dogfooding GAG's own CI.
The cluster costs $0 at rest (zero nodes), roughly $0.07/hr when idling with the system node only, and adds ≈$0.04/hr per spot worker node while jobs are running.

> **Status: Complete (2026-07-07).** The dogfood plan's deliverables have all landed and are live-validated: turn-up (2026-07-01), per-job-green, and concurrent-matrix-green on the ScaleSet default (Q224 closed via [Q264](../STATUS.md#Q264) P4, #545), plus the v2beta1 dogfood path (Q231, #573).
> The turn-up findings that gated it are all resolved — session recovery (Q247: renew by RunnerRequestID, bounded RenewJob, job-scoped renewal token, and Q254 worker teardown on definitive lock loss), release-asset diagnosis (Q246), agent-recycle under burst (Q259), and the dup-acquisition wedge (Q260).
> No open Queue rows remain.
> This doc stays in place as the living operational runbook — the sections below are the turn-up/start/stop/teardown reference, not open work.
> The chronological turn-up history and per-finding root-cause write-ups live in [archive/gke-dogfood-turnup-findings.md](archive/gke-dogfood-turnup-findings.md).
>
> **Updated 2026-07-25 (Q399).** The main CI tenant moved from the deprecated Classic protocol to a single-label ScaleSet, the last Q264 P5 residual.
> Classic orphaned 81% of the jobs it acquired on this tenant (85 acquired, 16 worker pods).
> An existing pre-Q399 tenant needs its `ci` RunnerSet **recreated**, not patched: see the migration note in Part B7.
> The runner label changed from `gag-ci` to `gag-ci-scaleset`.

**What runs where after setup**

| Workflow | Jobs migrated to GAG | Jobs kept on `ubuntu-latest` |
|---|---|---|
| `unit-test.yml` | `lint`, `shellcheck`, `vendor-check`, `tidy-check`, `unit-test`, `coverage` | `changes` |
| `integration-test.yml` | `integration-test` | `changes` |
| `e2e-reusable.yml` | `e2e` (kindnet + Calico, via Kata + DinD sidecar) | `changes` in callers |

The `changes` (paths-filter) jobs are intentionally kept on `ubuntu-latest`.
They are the gatekeepers for every downstream job: if they queue behind a down cluster, CI appears broken.

## Variables

Fill these in once before running any command.
Put them in your shell profile or paste them at the start of each terminal session.

```bash
CLUSTER=gag-dogfood
ZONE=us-east1-b                   # moved from us-central1-b 2026-06-30 (region-wide e2-standard-2 stockout)
PROJECT=actions-gateway-dogfood   # must be globally unique; append 4 digits if needed
REPO=actions-gateway/github-actions-gateway
APP_ID=3752347
INSTALLATION_ID=135739122         # actions-gateway org install (re-derive via Part C1)
```

> **Zone choice:** `ZONE` moved from `us-central1-b` to `us-east1-b` on 2026-06-30 after `us-central1` went region-wide `ZONE_RESOURCE_POOL_EXHAUSTED` for `e2-standard-2`.
> GCP exposes no capacity API — there is no way to query a zone's free capacity ahead of time, so pick a zone empirically: if cluster or node-pool creation fails with a stockout error, try another zone/region.

---

> **Shortcut:** Parts A3–B8 (cluster, node pools, GAG install, tenant) are automated by [`scripts/dogfood/setup.sh`](../../scripts/dogfood/setup.sh) — idempotent and safe to re-run with some of the work already done.
> Complete A1–A2 first (project + billing + APIs), export the Variables block, then run the script.
> The manual steps below document what it does, step by step.

## Part A — One-time GCP setup

### A1. Install gcloud CLI + authenticate

```bash
# macOS
brew install --cask google-cloud-sdk

gcloud auth login
gcloud auth application-default login
```

### A2. Create project + enable billing

```bash
gcloud projects create "$PROJECT" --name="GAG Dogfood"
gcloud config set project "$PROJECT"
```

Link billing in the console — the CLI requires a billing account ID which is hard to look up: https://console.cloud.google.com/billing → My Projects → select `$PROJECT` → Change billing → pick your billing account.

```bash
# Enable required APIs (run after billing is linked)
gcloud services enable container.googleapis.com compute.googleapis.com
```

### A3. Create GKE cluster (system node pool)

```bash
# Standard zonal cluster — one free per billing account, no cluster fee.
# --enable-dataplane-v2: Cilium-based CNI that enforces NetworkPolicy (required by GAG).
# --workload-pool: Workload Identity, a hard prerequisite of the Part F e2e
#   node pool (its --workload-metadata=GKE_METADATA is rejected with a 400
#   otherwise — found live under Q286). Control-plane-only here; node pools
#   opt in per-pool via --workload-metadata.
# No autoscaling on the default pool — it's manually scaled to 0/1 to start/stop.
gcloud container clusters create "$CLUSTER" \
  --zone="$ZONE" \
  --release-channel=regular \
  --enable-ip-alias \
  --enable-dataplane-v2 \
  --workload-pool="${PROJECT}.svc.id.goog" \
  --machine-type=e2-standard-2 \
  --num-nodes=1 \
  --disk-size=50GB \
  --no-enable-basic-auth \
  --no-issue-client-certificate
```

An existing cluster created without Workload Identity gets it retrofitted with `gcloud container clusters update "$CLUSTER" --zone="$ZONE" --workload-pool="${PROJECT}.svc.id.goog"` (control-plane update; existing node pools keep `GCE_METADATA` until updated, so running workloads are unaffected).

### A4. Add spot worker node pool

```bash
# Spot e2-standard-4 (4 vCPU / 16 GiB), autoscaling 0→8.
# Taint keeps GMC/AGC/proxy off worker nodes; worker pods tolerate it (see Part B).
# disk-type=pd-standard (HDD), NOT the GKE default pd-balanced (SSD-class): a
# pd-balanced boot disk counts against the 500 GB regional SSD_TOTAL_GB quota,
# so 100 GB/worker capped the pool at ~4 nodes (Q248) — a self-inflicted ceiling,
# not a real quota shortage. pd-standard counts against DISKS_TOTAL_GB (4096 GB)
# instead, so capacity is CPU/mem-bound (200 CPU quota), not SSD-bound. The CI
# job classes are CPU/mem-bound (Go build/test/lint/envtest), not scratch-IOPS-
# bound, so HDD is fine; 100 GB is kept for container-image pull scratch. This
# lifts max-nodes 4→8 within existing quota (no quota bump).
gcloud container node-pools create workers \
  --cluster="$CLUSTER" \
  --zone="$ZONE" \
  --machine-type=e2-standard-4 \
  --spot \
  --num-nodes=0 \
  --min-nodes=0 \
  --max-nodes=8 \
  --enable-autoscaling \
  --node-taints=dedicated=workers:NoSchedule \
  --disk-type=pd-standard \
  --disk-size=100GB
```

### A4b. Add non-preemptible worker pool (benchmarks)

The spot pool above is right for routine CI — cheap, and a preemption just re-runs a job.
It is the wrong shape for a **benchmark**.
Q260 chased a job-starvation signal that turned out to be spot preemption mid-burst (nodes dropping 3→1), not anything in GAG; the run only became readable once it was pinned to a non-preemptible pool, where every node stayed Ready across all 58 monitor samples and the phantom starvation did not recur (see [gke-dogfood-turnup-findings.md](archive/gke-dogfood-turnup-findings.md)).
Q264's protocol benchmarks used the same pool for the same reason.

This pool existed on the live cluster for months as a hand-made resource, recorded only in those plan docs — so a cluster rebuilt from `setup.sh` came back spot-only and silently reintroduced the Q260 measurement hazard.
It is now part of the scripted bootstrap.

```bash
# On-demand (non-preemptible) e2-standard-4, fixed size, starts at 0.
# Deliberately NOT autoscaled: a benchmark wants a fixed, known node count so the
# capacity under test is constant, not something the autoscaler moves mid-run.
# Size it per run with `ops.sh pool-scale workers-od <n>`, return it to 0 after.
# At 0 nodes it costs nothing, so it is safe to leave in place between campaigns.
# pd-standard for the same Q248 quota reason as `workers`; same taint so the
# identical worker-pod toleration schedules onto either pool.
gcloud container node-pools create workers-od \
  --cluster="$CLUSTER" \
  --zone="$ZONE" \
  --machine-type=e2-standard-4 \
  --num-nodes=0 \
  --node-taints=dedicated=workers:NoSchedule \
  --disk-type=pd-standard \
  --disk-size=100GB
```

### A5. Get cluster credentials

```bash
gcloud container clusters get-credentials "$CLUSTER" --zone="$ZONE"
kubectl get nodes  # should show 1 system node
```

---

## Part B — One-time GAG installation

### B1. Run preflight

GKE's Dataplane V2 (Cilium) passes the NetworkPolicy enforcement check.
GKE also ships the Kubernetes Metrics Server by default (required for the proxy pool's HPA).

> **NodeLocal DNSCache on Dataplane V2 (Q229).** If the cluster runs NodeLocal DNSCache, Dataplane V2 redirects cluster-DNS traffic to the per-node `node-local-dns` pod, and the tenant egress `NetworkPolicy` must allow that backend or the AGC crash-loops on its first GitHub token fetch (`lookup api.github.com: i/o timeout`).
> GAG's DNS egress rule includes the `node-local-dns` peer as of Q229; use a build that has it.
> Diagnosis and the verification command are in [Troubleshooting → DNS Times Out Under the Egress NetworkPolicy](../operations/troubleshooting.md#dns-times-out-under-the-egress-networkpolicy-gke-dataplane-v2--nodelocal-dnscache).

```bash
make validate-cluster
```

### B2. Create Helm values file

```bash
cat > tmp/values-dogfood.yaml <<'EOF'
# Dogfood / dev mode: pin an image built from a post-Q74 git ref, not digests.
# Production installs must use digest-pinned images from the release page.
# This is NOT limited to cut `v*` releases: dogfood tracks pre-release code, and
# the publish pipeline builds images only on `v*` tags — so the post-Q74 image at
# this ref was built + pushed by hand (see "Tracking post-Q74 pre-release builds"
# below). The SAME ref pins both this image tag and the git-archived v2 CRD chart
# (B3), so they can never drift. `latest` is never published, so never float to it.
allowFloatingImageTags: true
# Single GMC replica for dogfood (production wants the default 2 for HA); frees
# capacity on the small system node for the per-tenant AGC pod.
replicaCount: 1
gmc:
  image:
    tag: 2715e7f87e48896b26aaa7c4bf4b8b48425576be
agc:
  image:
    tag: 2715e7f87e48896b26aaa7c4bf4b8b48425576be
proxy:
  image:
    tag: 2715e7f87e48896b26aaa7c4bf4b8b48425576be
# WRAPPER_IMAGE drives Q235 worker-wrapper injection — the GMC forwards it to
# every AGC, which injects the wrapper into each worker pod so the runner
# container can be the unmodified upstream actions-runner. Pin it: the chart's
# default wrapper tag is empty, which renders wrapper:latest (never published)
# and ImagePullBackOffs the injection.
wrapper:
  image:
    tag: 2715e7f87e48896b26aaa7c4bf4b8b48425576be

# Self-signed webhook cert — no cert-manager dependency.
# The cert rotates on helm upgrade; acceptable for a personal dogfood cluster.
certManager:
  enabled: false

# Keep GMC on the system node pool (default-pool) so it stays down
# when we scale that pool to 0. AGC and proxy inherit this via scheduling
# because the worker pool's taint blocks them without a toleration.
nodeSelector:
  cloud.google.com/gke-nodepool: default-pool

# No PodDisruptionBudget: with a single GMC replica the chart's minAvailable: 1
# permits zero voluntary disruptions, so the Part D scale-to-0 stop could never
# drain the system node — it would linger Ready,SchedulingDisabled and keep
# billing (Q236).
podDisruptionBudget:
  enabled: false
EOF
```

### B3. Install the v2 CRDs and the GAG chart

The v2 CRDs ship in a separate, opt-in chart (`actions-gateway-crds-v2`).
The GMC runs its v2 controllers unconditionally, so the CRDs must be installed — and **at the same release as the GMC image**, because the v2 *alpha* schema drifts between releases (e.g.
`ActionsGateway.spec.githubAppRef` in `v1.1.0-rc.2` became the `spec.credentials` discriminated union in `v1.1.0-rc.3`); a mismatch makes every reconcile fail validation.
A stale CRD that still exposes `githubAppRef` silently drops the credential — the GMC reads an empty App ref and provisions the AGC for workload-identity (Vault) instead, and the AGC crash-loops on `read appId: … no such file or directory`.
Always apply this chart in lockstep with the GMC image.

Since Q74 graduated each v2 CRD to two served versions (v2beta1 + v2alpha1), the rendered chart exceeds Helm's 1 MiB release-Secret limit, so `helm install`/`upgrade` can no longer store the release and fails outright.
The supported install≡upgrade path is **apply-render**: `helm template` the chart, then `kubectl apply --server-side` (Q276).
Dogfood uses the **from-source render** variant — it git-archives the local chart at `$GAG_IMAGE_TAG` to exercise pre-release code, so it cannot depend on the signed release asset (which exists only for `v*` tags). Because `$GAG_IMAGE_TAG` is a git ref, the archived chart is guaranteed to match the control-plane image built from that same ref.
`scripts/dogfood/setup.sh` does this automatically; the manual equivalent for the pinned ref:

```bash
git archive "$GAG_IMAGE_TAG" charts/actions-gateway-crds-v2 | tar -x -C tmp/
# --namespace gmc-system resolves each CRD's conversion-webhook clientConfig to the
# GMC's webhook-service; --force-conflicts takes field ownership on a re-apply.
helm template actions-gateway-crds-v2 tmp/charts/actions-gateway-crds-v2 \
  --namespace gmc-system \
  | kubectl apply --server-side --force-conflicts -f -
```

Then install the GMC chart:

```bash
helm install gag charts/actions-gateway \
  --namespace gmc-system --create-namespace \
  --values tmp/values-dogfood.yaml

kubectl rollout status deployment/gmc-controller-manager -n gmc-system --timeout=3m
```

> **GKE PriorityClass admission:** the GMC runs with `priorityClassName: system-cluster-critical`, which GKE permits only in a namespace carrying a matching scoped `ResourceQuota` — without one the GMC pods fail with `insufficient quota to match these scopes`.
> The chart ships that permit-only quota (`gmc-critical-pods`) by default (`systemCriticalPriorityQuota.enabled=true`), so no manual `kubectl apply` is needed here.
> See [install.md](../operations/install.md#gke-and-other-restricted-priorityclass-clusters).

#### Wire the webhook CA into the CRD conversion `caBundle` (Q279)

Since Q74 the v2 kinds are stored at `v2beta1` and served at `v2alpha1` through the GMC-hosted conversion webhook.
The apiserver calls that webhook over TLS and only trusts it if each CRD's `spec.conversion.webhook.clientConfig.caBundle` carries the CA that signed the GMC's serving cert.
Dogfood is **self-signed** (no cert-manager), and the CRD chart renders an empty `caBundle` when `conversion.certManager.enabled=false` and `conversion.caBundle` is unset — so **without this step every CR `apply` (and its conversion read-back) fails the TLS handshake** with `x509: certificate signed by unknown authority`.

The chart mints that CA once and stores it in the `webhook-server-cert` Secret in `gmc-system` (reused across renders via Helm `lookup`, so it is stable on re-runs).
The Secret's `data["ca.crt"]` is already `base64(PEM)` — exactly the encoding a CRD `caBundle` wants — so it passes straight through:

```bash
CA="$(kubectl get secret webhook-server-cert -n gmc-system -o jsonpath='{.data.ca\.crt}')"
for crd in actionsgateways runnersets runnertemplates egressproxies clusterrunnertemplates; do
  kubectl patch crd "${crd}.actions-gateway.com" --type=merge \
    -p "{\"spec\":{\"conversion\":{\"webhook\":{\"clientConfig\":{\"caBundle\":\"${CA}\"}}}}}"
done
```

**Why patch instead of `--set conversion.caBundle=…` at CRD-render time?** The CA only exists after the GMC install creates its Secret, but the CRDs must be installed *before* the GMC so it detects the v2 kinds at startup and enables its v2 controllers + conversion webhook (Q228 — installing the CRDs later needs a GMC restart).
Reversing that order to obtain the CA first would break the detection.
So `scripts/dogfood/setup.sh` keeps the order **install CRDs (empty `caBundle`) → install GMC (mints the Secret) → `patch_crd_cabundle`**, and the patch runs before the first CR is applied.
A JSON merge patch sets only the `caBundle` leaf, leaving the chart-rendered `clientConfig.service` block intact; a later server-side re-apply of the CRD chart never renders `caBundle`, so it cannot strip this leaf (a different field manager owns it).
`setup.sh` does all of this automatically.

**Image-tag transition (pre- vs post-Q74).** `install_crds` pins the CRDs to `GAG_IMAGE_TAG`.
The conversion webhook itself landed in Q74 (PR #557), so a **pre-Q74** GMC build (e.g. the old `v1.1.0-rc.6` default) ships **single-version** CRDs with conversion `strategy: None` and no webhook clientConfig — there is nothing to wire, and patching a webhook clientConfig onto them would be rejected (`should not be set when strategy is not Webhook`).
`patch_crd_cabundle` therefore reads each CRD's `spec.conversion.strategy` and only patches the ones set to `Webhook`, cleanly skipping the rest.
So on a pre-Q74 build the caBundle wiring is a no-op; on a **post-Q74** build whose multi-version CRDs route CR conversion through `/convert`, the same step activates automatically with no further change.

**Live-validated post-Q74 (Q281, 2026-07-07).** `GAG_IMAGE_TAG` is now pinned to a post-Q74 `main` SHA (`4567097…`) whose control-plane image was built + pushed by hand (see [Tracking post-Q74 pre-release builds](#tracking-post-q74-pre-release-builds)), so the full **apply → convert → read-back round-trip is live on dogfood**.
Confirmed end-to-end on `gag-dogfood`: with the post-Q74 GMC serving `/convert`, `ActionsGateway`, `RunnerTemplate`, and `RunnerSet` all apply at `v2alpha1` and read back at **both** `v2beta1` (storage) and `v2alpha1` (served through the TLS-verified conversion webhook) with **no `/convert` 404 and no `x509` error** — exercising Q279's `caBundle` wiring for real.
(Before the bump the cluster was in the exact dormant state Q279 anticipated: post-Q74 CRDs at `strategy: Webhook` but a pre-Q74 rc.6 GMC with no `/convert` handler, so `kubectl get actionsgateways` failed `conversion webhook … the server could not find the requested resource`.)

> **Security — keep the webhook TLS-verified.** An empty `caBundle` can tempt a `caBundle`-less or `insecureSkipTLSVerify: true` shortcut to "just make CRs apply."
> Do **not**: that lets any pod impersonating the webhook Service intercept every CR conversion.
> Wiring the real CA is the secure-by-default fix.

#### Tracking post-Q74 pre-release builds

Dogfood deliberately runs **pre-release** control-plane code so new behaviour (like the Q74 conversion webhook) can be exercised end-to-end *before* a release is cut.
It must therefore be able to pin an arbitrary post-Q74 point, **not** wait for a `v*` release.
The single `GAG_IMAGE_TAG` variable makes this safe by construction: it is a **git ref** (SHA, branch, or tag) used two ways that must agree —

1. as the published control-plane image tag under `ghcr.io/actions-gateway/{gmc,agc,proxy,wrapper}:<ref>` (the images the chart pulls), and
2. as the git object `install_crds` runs `git archive <ref> charts/actions-gateway-crds-v2` against (the v2 CRD chart, whose alpha schema drifts between refs).

Because both come from the **same ref**, the running image and the installed CRDs can never disagree on the v2 schema.
The publish pipeline builds images only on `v*` tags, so for a non-release ref you build and push the four control-plane images by hand once, then set `GAG_IMAGE_TAG` to that ref.
GKE nodes are amd64, so a single-arch build is enough for dogfood:

```bash
SHA="$(git rev-parse origin/main)"          # any post-Q74 ref that carries the v2 CRD chart
gh auth token | docker login ghcr.io -u "$(gh api user --jq .login)" --password-stdin
GIT_SHA="$SHA" docker buildx bake gmc agc proxy wrapper \
  --set '*.platform=linux/amd64' \
  --set "gmc.tags=ghcr.io/actions-gateway/gmc:$SHA" \
  --set "agc.tags=ghcr.io/actions-gateway/agc:$SHA" \
  --set "proxy.tags=ghcr.io/actions-gateway/proxy:$SHA" \
  --set "wrapper.tags=ghcr.io/actions-gateway/wrapper:$SHA"
GAG_IMAGE_TAG="$SHA" scripts/dogfood/setup.sh   # (with the other Variables exported)
```

The `{gmc,agc,proxy,wrapper}` packages already exist and are **public**, so a new tag inherits that visibility — no GHCR visibility flip is needed (a brand-new package would default to private and `ImagePullBackOff`; flip it to public in the GHCR UI).
Pushing to `ghcr.io/actions-gateway` needs the `write:packages` scope (`gh auth refresh -s write:packages` if your token lacks it).
This is a dev/dogfood convenience only — **production always pins release digests**, never a floating SHA tag.

> **Move the default forward in the same change.** `setup.sh` is idempotent and advertised as safe to re-run, which is what makes a stale `GAG_IMAGE_TAG` default sharp: a re-run with no explicit tag reinstalls the *default* ref and takes the CRDs with it, silently **downgrading** a cluster running anything newer.
> The default sat at a 2026-07-07 ref while the cluster ran 2026-07-24 and then 2026-07-31 code, so a defaults run in that window would have withdrawn the capacity-gate rung and re-blocked Q472's re-validation.
> Nothing detects it — the install path has no notion of "older than what is deployed" — so when you pin a newer ref by hand, update the default in [`scripts/dogfood/setup.sh`](../../scripts/dogfood/setup.sh) and the pins above together.
> Read what is actually running before re-running:
>
> ```bash
> kubectl get deploy gmc-controller-manager -n gmc-system \
>   -o jsonpath='{.spec.template.spec.containers[*].image}'
> ```

### B4. Create tenant namespace

```bash
kubectl create namespace gag-dogfood

# v2 markers: tenant=managed authorizes the GMC to operate in the namespace;
# security-profile drives the Pod Security level the GMC stamps (absent ⇒
# baseline). Apply tenant=managed with an admin identity — the GMC must never
# set it itself. (v1 used actions-gateway.github.com/tenant=true + an inline
# spec.securityProfile on the CR.)
kubectl label namespace gag-dogfood \
  actions-gateway.com/tenant=managed \
  actions-gateway.com/security-profile=baseline \
  pod-security.kubernetes.io/enforce=baseline
```

### B5. Create GitHub App secret

The `actions-gateway-test` app key lives in the Mac keychain (stored as hex).

```bash
security find-generic-password \
  -a actions-gateway-test -s github-app-private-key -w \
  | xxd -r -p > tmp/app.pem

kubectl create secret generic github-app-v1 \
  --namespace=gag-dogfood \
  --from-literal=appId="$APP_ID" \
  --from-literal=installationId="$INSTALLATION_ID" \
  --from-file=privateKey=tmp/app.pem

rm tmp/app.pem
```

### B6. Apply ResourceQuota

```bash
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ResourceQuota
metadata:
  name: dogfood-quota
  namespace: gag-dogfood
spec:
  hard:
    pods: "12"
EOF
```

### B6b. Deploy Athens in-cluster Go module cache

`vendor-check` and `tidy-check` re-fetch Go modules from `proxy.golang.org` on a cold cache.
GKE Dataplane V2's managed Cilium lacks the `CiliumNetworkPolicy` CRD, so the `CiliumFQDN` egress mode is unusable here; a CIDR allowlist for Google-fronted hosts like `proxy.golang.org` would be a footgun (it opens all of Google's frontend).
Athens sidesteps both constraints: it runs in-cluster with free egress and serves cached modules to workers over a plain HTTP port that does not need a CNI FQDN backend.

Athens is not covered by the workload `NetworkPolicy` (`actions-gateway/component: workload` label).
Workers reach it via an additive `NetworkPolicy` that opens port 3000 from workload pods to Athens pods.
The Service is named `go-module-proxy` (not `athens`) to avoid Kubernetes injecting `ATHENS_PORT=tcp://...` into pods in the namespace — Athens misreads that as its listen address.

```bash
kubectl apply -k deploy/athens
kubectl rollout status deployment/athens -n gag-dogfood --timeout=120s
```

Verify Athens is healthy:

```bash
kubectl get pods -n gag-dogfood -l app=athens
kubectl logs -n gag-dogfood -l app=athens --tail=20
```

Athens pre-warms lazily — the first `vendor-check`/`tidy-check` run is slower while modules download; subsequent runs are cache hits from the storage volume.

The default render above uses an **ephemeral** `emptyDir` cache ($0 at rest); the cache goes cold on every scale-to-zero idle cycle.
For a cache that survives idle cycles, render `deploy/athens/overlays/persistent` (or set `ATHENS_PERSISTENT=1` for `scripts/dogfood/setup.sh`) — a PVC-backed disk that bills continuously while it exists.
Trade-off and tear-down: [`deploy/athens/README.md`](../../deploy/athens/README.md).

> **Why plain HTTP (no TLS)?** Athens serves public Go module zips; there is nothing confidential in transit.
> Integrity is upheld by the Go toolchain's `go.sum` verification — every module downloaded from Athens is checked against the committed `go.sum` regardless of `GONOSUMDB`, so a tampered response is caught before it reaches the build.
> Adding TLS would require cert management (cert-manager or a self-signed CA wired into every worker image) for no meaningful security gain in this single-tenant cluster.
> Revisit if Athens is extended to a shared multi-tenant cluster or used to serve private modules.

### B7. Create the v2 tenant objects

The v2 API decomposes the v1 monolithic `ActionsGateway` into `ActionsGateway` (gateway + credentials), `RunnerTemplate` (worker pod shape), and `RunnerSet` (runner group).
This is the minimal **direct-egress** form — no `EgressProxy`, so workers egress directly to GitHub, still behind the default-deny egress NetworkPolicy (DNS + GitHub CIDR), just without a per-tenant egress IP.
Attach an `EgressProxy` and set `spec.defaultProxyRef` on the gateway to add per-tenant egress IP attribution.

```bash
kubectl apply -f - <<'EOF'
apiVersion: actions-gateway.com/v2alpha1
kind: ActionsGateway
metadata:
  name: dogfood
  namespace: gag-dogfood
spec:
  credentials:
    type: GitHubApp
    githubApp:
      name: github-app-v1
  githubURL: https://github.com/actions-gateway/github-actions-gateway
---
apiVersion: actions-gateway.com/v2alpha1
kind: RunnerTemplate
metadata:
  name: default
  namespace: gag-dogfood
spec:
  podTemplate:
    spec:
      tolerations:
        - key: dedicated
          value: workers
          effect: NoSchedule
      containers:
        - name: runner
          # Named but deliberately image-less (Q235 injection default). The AGC
          # gap-fills the resolved worker image on a named image-less runner
          # container (Q233) — here the built-in upstream actions-runner digest
          # (DefaultWorkerImage) — and injects the GAG worker wrapper
          # (WRAPPER_IMAGE) into the pod, so that unmodified upstream image runs
          # jobs. NOTE: the bare upstream image has no build toolchain, so this
          # repo's make-based CI fails `make: command not found` on it (see the
          # Known gap below). For green CI, set a build-capable workerImage here;
          # injection still applies on top of any base.
          env:
            # Athens in-cluster Go module proxy (Q244). Workers cannot reach
            # proxy.golang.org directly (egress NetworkPolicy, GKE DPv2 no FQDN NP).
            # GONOSUMDB=* prevents direct sum.golang.org queries; Athens validates
            # checksums when it fetches from proxy.golang.org upstream.
            - name: GOPROXY
              value: "http://go-module-proxy.gag-dogfood.svc.cluster.local:3000,off"
            - name: GONOSUMDB
              value: "*"
          # Right-sized from measured peak (Q248 Phase 1: heavy CI jobs peaked
          # ~3.8 vCPU / ~2.1Gi). CPU requests-only (no limit — compressible, a
          # limit only throttles bursty Go jobs); request=2 packs one heavy pod
          # per e2-standard-4 (~3.4 vCPU allocatable) so it bursts to the whole
          # node. Memory: request≈peak (2Gi), limit=peak×~1.4 (3Gi) for OOM
          # headroom. See docs/plan/dogfood-runner-rightsizing.md.
          resources:
            requests:
              cpu: "2"
              memory: "2Gi"
            limits:
              memory: "3Gi"
---
apiVersion: actions-gateway.com/v2alpha1
kind: RunnerSet
metadata:
  name: ci
  namespace: gag-dogfood
spec:
  gatewayRef:
    name: dogfood
  templateRef:
    name: default
  # ScaleSet (the Q264 P5 default), stated explicitly so the tenant's protocol is
  # readable here. One runnerLabel, which is BOTH the scale set's name at GitHub and
  # the workflows' runs-on target, so it must match vars.GAG_RUNNER exactly (Part C2).
  acquisitionProtocol: ScaleSet
  runnerLabels: ["gag-ci-scaleset"]
  # maxWorkers 8: the pd-standard disk right-size (Q248) lifted the worker-node
  # ceiling off the SSD quota, so the ~7-job dogfood CI matrix fits. On ScaleSet this
  # is also the capacity advertised to GitHub (X-ScaleSetMaxCapacity). maxListeners is
  # deliberately absent: a Classic-only knob that a scale set (ONE listener) ignores.
  maxWorkers: 8
EOF
```

> **Migrating a pre-Q399 Classic CI tenant (one-time).** `spec.acquisitionProtocol` is **immutable**, so a `ci` RunnerSet created before Q399 cannot be patched or re-`apply`ed onto ScaleSet; the write is rejected.
> Delete and recreate it: `kubectl delete runnerset ci -n gag-dogfood`, then re-run `apply_cr` (or `scripts/dogfood/setup.sh`).
> Recreating also clears the `conversion.actions-gateway.com/acquisition-protocol: Classic` annotation the conversion webhook stamped on the stored v2beta1 object, which `kubectl apply` does **not** strip and which would otherwise keep the AGC on Classic.
> The label changes at the same time (`gag-ci` to `gag-ci-scaleset`), so this is also a fresh scale-set object with an empty message queue.
> That matters: reconnecting to a label used by a previous scale set replays its old unacked `JobAssigned` messages.
> Leftover classic `ci-N` runner records under the old `gag-ci` label are inert once nothing routes to it; delete them from the repo's runner list if they clutter.
>
> **Why (Q399).** Classic calls `AcquireJob`, which flips the job to `in_progress` at GitHub and stamps the runner name, *before* it decides whether to provision a worker.
> Every job it then declines to provision is orphaned at GitHub with zero steps until the 10-minute lock-lapse or 15-minute unstarted-job timeout kills it.
> Measured on this tenant 2026-07-25: **85 jobs acquired, 16 worker pods, 69 orphaned (81%)**.
> The ScaleSet listener is single-acquirer and cannot produce that shape (Q264 P4: 7/7 vs Classic's 2/7).

> **v2 prerequisites:** Kubernetes ≥ 1.31 (the `RunnerSet` field-selector scoping, KEP-4358) and the `actions-gateway-crds-v2` chart from B3.
> The `spec.credentials` discriminated-union shape is the `v1.1.0-rc.3` schema (`rc.2` used a flat `spec.githubAppRef`) — if you pin a different `$GAG_IMAGE_TAG`, match the CRD chart to that release and use its credential shape.

> **Build-capable worker image (Q239).** The bare upstream `actions-runner` has no build toolchain, so this repo's `make`-based jobs fail `make: command not found` on it.
> Build a build-capable image (adds `build-essential` etc. on top of the pinned upstream runner — [`scripts/dogfood/runner/Dockerfile`](../../scripts/dogfood/runner/Dockerfile)) with [`scripts/dogfood/runner-build.sh`](../../scripts/dogfood/runner-build.sh), then export `DOGFOOD_RUNNER_IMAGE=ghcr.io/actions-gateway/dogfood-runner:<tag>` before running `scripts/dogfood/setup.sh` — it pins the `RunnerTemplate` `workerImage` and the AGC still injects the Q235 wrapper on top.
> Validation history: [archived findings](archive/gke-dogfood-turnup-findings.md).

### B8. Validate

```bash
# Gateway Ready=True; RunnerSet shows its template + egress mode (Direct).
kubectl get actionsgateway,runnerset -n gag-dogfood -o wide
kubectl get pods -n gag-dogfood   # the dogfood-agc Deployment pod should be Running

# Runners should appear within ~2 min of the AGC becoming Ready
gh api /repos/"$REPO"/actions/runners \
  --jq '.runners[] | {name, status, labels: [.labels[].name]}'
```

> **Turn-up went green — validation history archived.** The `v1.1.0-rc.6` turn-up, the eight classic-protocol fan-out re-routes that isolated the Q224 distinct-delivery starvation, the Q264 P4 ScaleSet clean-green close-out (7/7), and the root-cause write-ups for Q246/Q247/Q254/Q259/Q260/Q265/Q266/Q267 are recorded in [archive/gke-dogfood-turnup-findings.md](archive/gke-dogfood-turnup-findings.md).
> All are resolved; the status summary at the top of this doc reflects current state.

---

## Part C — One-time GitHub setup

### C1. Confirm App installation + get installation ID

Ensure `actions-gateway-test` is installed on the org:
- GitHub.com → `actions-gateway` org → Settings → GitHub Apps → `actions-gateway-test` → Configure
- Confirm repository access is "All repositories" (or that `actions-gateway/github-actions-gateway` is explicitly listed)

Get the installation ID.
The `/user/installations` endpoint requires a GitHub-App-authorized token (the `gh` CLI's token returns HTTP 403), so use the org-scoped endpoint instead — it works for an org owner:

```bash
gh api /orgs/actions-gateway/installations \
  --jq '.installations[] | select(.app_id == 3752347) | {id, account: .account.login}'
```

Set `INSTALLATION_ID` to the `id` value and re-run the secret creation in B5 if you had a placeholder.
As of this writing the install is `135739122`.

### C2. Workflow changes

The migrated jobs route to GAG only under an **explicit opt-in**, so a dogfood turn-up never drags every concurrent push, PR, or Dependabot run onto the cluster.
Two paths coexist:

- **Opt-in dispatch (default day-to-day turn-up)** — a `workflow_dispatch` trigger with a boolean `target_gag` input (default `false`).
  GAG runs only when a run is *manually* dispatched with `target_gag=true`.
  This is the scoped path for validation bursts (Q224/Q264 P4; see PR #541 for the contention this replaced).
- **Global variable (milestone end-state only)** — flipping `vars.GAG_RUNNER` still routes *all* production CI to GAG.
  This remains the Q224 end goal ("route all production CI to GAG") and is intentionally left in place.

Both are folded into one `runs-on` expression, applied to these jobs (leave all `changes` jobs on `ubuntu-latest`):

**`.github/workflows/unit-test.yml`** — jobs `lint`, `shellcheck`, `vendor-check`, `tidy-check`, `unit-test`, `coverage`.
**`.github/workflows/integration-test.yml`** — job `integration-test`:

```yaml
runs-on: ${{ (github.event_name == 'workflow_dispatch' && inputs.target_gag) && fromJSON(vars.GAG_RUNNER || '"ubuntu-latest"') || 'ubuntu-latest' }}
```

- On **push / pull_request** (or a dispatch with `target_gag` unset/false): `github.event_name == 'workflow_dispatch' && inputs.target_gag` is `false`, so the whole `&& … ||` expression short-circuits to the trailing `'ubuntu-latest'` — GitHub-hosted, exactly as before.
  `vars.GAG_RUNNER` is never consulted, so flipping it no longer affects these events.
- On a **dispatch with `target_gag=true`**: the expression evaluates `fromJSON(vars.GAG_RUNNER || '"ubuntu-latest"')`, giving the string `"gag-ci-scaleset"` (routes to GAG) when the variable is set, or `ubuntu-latest` when it is not.
  Since Q399 this is a single JSON *string*, not an array: the runner set declares one label, its own scale-set name.
  (Q726 lets a set carry more; this tenant still declares one.)
  It must stay identical to `spec.runnerLabels[0]` in B7.
  A mismatch leaves dispatched jobs queued at GitHub forever, unmatched.

The gate jobs' `if` conditions also add `|| github.event_name == 'workflow_dispatch'` so a manual dispatch still creates the migrated jobs even when the paths-filter finds no matching diff, and each dispatch run gets its own `concurrency` group (run id appended) so it never queues behind an in-flight push run on the same ref.

### C3. Set default variable (cluster off)

```bash
gh variable set GAG_RUNNER \
  --body '"ubuntu-latest"' \
  --repo "$REPO"
```

Commit and push the workflow changes.
Ordinary push/PR CI stays on GitHub-hosted runners regardless of this variable — under the opt-in model the variable is consulted only by a `workflow_dispatch` run with `target_gag=true` (Part D).
Keeping it at `"ubuntu-latest"` also makes such a dispatch a safe no-op when the cluster is off.

---

## Part D — Daily operations

### Start dogfooding

```bash
# 1. Scale system pool up (takes ~3 min for GAG to be ready)
gcloud container clusters resize "$CLUSTER" \
  --node-pool=default-pool --num-nodes=1 --zone="$ZONE" --quiet

# 2. Wait for GMC and AGC pods to be ready
kubectl rollout status deployment/gmc-controller-manager -n gmc-system --timeout=5m
kubectl wait --for=condition=Ready pod \
  -l app.kubernetes.io/name=actions-gateway-controller,app.kubernetes.io/instance=dogfood \
  -n gag-dogfood --timeout=3m

# 3. Set the GAG runner label (consulted only by opt-in dispatches below;
#    does NOT route any push/PR CI on its own). A single JSON string: the
#    ScaleSet runner set's one label (Q399); must match B7's runnerLabels[0].
gh variable set GAG_RUNNER \
  --body '"gag-ci-scaleset"' \
  --repo "$REPO"

# 4. Dispatch an isolated validation burst onto GAG (scoped — does not touch
#    other PRs, Dependabot, or push CI). Repeat per burst; --ref picks the
#    branch whose code to validate.
gh workflow run unit-test.yml -f target_gag=true --ref main --repo "$REPO"
gh workflow run integration-test.yml -f target_gag=true --ref main --repo "$REPO"
```

To route **all** production CI to GAG (the Q224 milestone end-state, not a day-to-day turn-up), see "Route all CI to GAG" below.

The [dogfood runner image](../../scripts/dogfood/runner/Dockerfile) omits `go` on purpose, relying on each job's `setup-go` step to supply it — a job that shells out to `go` without declaring that step fails `go: command not found` on every `target_gag=true` dispatch while passing on `ubuntu-latest`, which preinstalls Go.
The `shellcheck` job had exactly this mismatch (Q482, fixed by adding the step); read a deterministic single-job red like that as a job/image mismatch, not a GAG defect or a flake.

### Sampling AGC metrics during a measurement run

The AGC serves `/metrics` on **8443 behind mTLS** (per-gateway self-signed PKI; see `generateMetricsCertsV2`).
Scraping it ad hoc needs the scraper's client leaf from the `<gateway>-agc-metrics-client` Secret and a hostname that matches the server cert's Service SANs — `curl -k` is not enough (the server still demands a client cert) and `localhost` fails SAN verification, so pin the Service name to the forwarded port with `--resolve`:

```bash
kubectl get secret dogfood-agc-metrics-client -n gag-dogfood -o jsonpath='{.data}' \
  # -> extract ca.crt / tls.crt / tls.key (base64) to tmp/certs/
kubectl port-forward svc/dogfood-agc -n gag-dogfood 18443:8443 &
curl --resolve dogfood-agc.gag-dogfood.svc:18443:127.0.0.1 \
  --cacert tmp/certs/ca.crt --cert tmp/certs/tls.crt --key tmp/certs/tls.key \
  https://dogfood-agc.gag-dogfood.svc:18443/metrics
```

Used by the §9e/§9f (Q469/Q462) and §9h (Q513) capacity-gate measurements in [capacity-aware-intake.md](capacity-aware-intake.md); since Q514, `worker_pods_reaped_total` stamps `runner_set` on scale-set reaps alongside `runner_group`, so one `runner_set` filter covers it and the `scaleset_*` gauges.

### Stop dogfooding

Opt-in dispatches are one-shot, so there is no standing CI route to revert — only the end-state global routing below needs the `GAG_RUNNER` reset.

```bash
export PROJECT CLUSTER ZONE REPO   # from the Variables section
scripts/dogfood/stop.sh
```

The script resets `GAG_RUNNER`, **waits for in-flight worker pods to drain**, and only then scales the system pool to 0.
Worker nodes autoscale to 0 on their own within ~10 min after that.

#### Why the drain wait is not optional (Q434)

The system pool carries the tenant AGCs, and **an AGC is the only thing that reaps its worker pods.** Scaling that pool to 0 with a job in flight evicts the AGC with nowhere to reschedule, so:

- its worker pods keep running on the `workers` pool, with no controller left to delete them;
- the pods carry `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` (deliberately — see [observability-metrics.md](../operations/observability-metrics.md), a worker mid-job has no replacement), so the autoscaler will not reclaim their nodes either;
- the nodes bill until a human notices.
  One incident stranded 82 spot node-hours this way.

Resetting `GAG_RUNNER` first stops *new* dispatches routing at the cluster, but dispatches already queued at GitHub are still assigned to the scale set — which is exactly why the wait exists rather than a "looks idle to me" glance.

Knobs, all optional:

| Variable | Default | Effect |
|---|---|---|
| `DRAIN_TIMEOUT` | `1500` | Seconds to wait for the drain. Covers the longest dispatched job (`timeout-minutes: 20`) plus reap time. |
| `DRAIN_INTERVAL` | `15` | Seconds between drain polls. |
| `SKIP_DRAIN` | unset | `1` scales down without waiting. Strands anything in flight. |

The drain is **cluster-wide on purpose.** The resize evicts *every* tenant's AGC, so it has to see every tenant's workers; scoping it to one namespace would scale down under another tenant's live jobs and re-open the incident above.
(`e2e-stop.sh` does scope its drain, because it deletes one gateway and touches no other tenant.)

A drain that does not finish inside `DRAIN_TIMEOUT` **fails the stop and leaves the pool up** — two `e2-standard-2` nodes cost far less than stranded worker nodes, and keeping the AGC alive is what lets it finish reaping.

#### Reading a timed-out drain

The error answers two questions, because they take opposite remedies.

**Why is each pod still in flight?** Every in-flight pod is listed with its scheduling and container-waiting reasons, then with its latest warning event.
`Pending` alone does not distinguish a pod waiting on a node coming up from one that will never start; the event does — `FailedScheduling` names the resource the pod cannot get, and `FailedMount` names the `job-payload` secret that does not exist.
Without it a pod pinned on a missing secret reads as a bare `ContainerCreating`.

**Was the drain moving at all?** The stop compares the pods in flight at the first and last polls.
Pod *turnover* is the signal, not the count: a tenant at its `maxWorkers` ceiling admits a pod for every one it reaps, so a drain working through a backlog and a livelocked one both hold the count flat.

| Verdict | Meaning | Do |
|---|---|---|
| *converging* | Some pods turned over — work is completing | Re-run, or re-run with a larger `DRAIN_TIMEOUT` |
| *NOT converging* | The same pods held the whole wait | Delete those pods by hand, then re-run — waiting longer will not clear it |

A pod stuck on `FailedMount` or `FailedScheduling` holds one of its tenant's concurrency slots until it is deleted, which is what stops the rest of the queue from draining.

Bouncing the AGC (`scripts/dogfood/ops.sh agc-bounce [ci|e2e]`) is **not** a remedy here.
It is a real rolling restart on a GMC carrying the Q552 fix, but this drain counts worker pods and a restart deletes none of them; the reaper that clears a stuck pod already runs on the live AGC, on deadlines measured from the pod, so a fresh one reaps no sooner.
Delete the pods instead.

Since `v1.3.0` a listener holding assignments GitHub has dropped gives them up on its own, roughly two to three minutes after the last one goes (Q553) — so a drain that used to livelock behind re-provisioned phantom workers now converges without a hand-deleted gateway.
Two caveats: the give-up needs GitHub to be holding **zero** assignments for the set, so a tenant still running real work keeps its dangling ones until it idles; and it is per-process, so bouncing the AGC mid-drain restarts the clock.
Watch it land on `actions_gateway_scaleset_jobs_abandoned_total` and the AGC's `giving up on assigned jobs` log line.

`SKIP_DRAIN=1` is the deliberate override — reach for it when the AGC is already down, since an AGC that cannot reap will never let the drain finish.

### Route all CI to GAG (milestone end-state)

The opt-in dispatch above is the day-to-day validation path.
The Q224 end goal — routing *every* push/PR run to GAG — is a deliberate, separate step because it reintroduces the whole-CI contention the opt-in model exists to avoid.
It is **not** a `GAG_RUNNER` flip: under the opt-in expression the variable is ignored on push/PR events.
Reaching the end-state means editing the migrated jobs' `runs-on` back to the unconditional form:

```yaml
runs-on: ${{ fromJSON(vars.GAG_RUNNER || '"ubuntu-latest"') }}
```

with `GAG_RUNNER` set to `"gag-ci-scaleset"`.
Defer this until the milestone criteria in Q224/Q264 are met (P4-green, scale-set maturity, adoption signal).

---

## Part E — Teardown

There are two levels of teardown.
Pick deliberately — they are not the same operation with different urgency.

| | `stop.sh` | `delete.sh` |
|---|---|---|
| What goes away | Nodes only | The whole cluster |
| GAG install, tenant CRs, cert-manager CA, App secret | **Kept** | **Destroyed** |
| Cost at rest | $0.00/hr | $0.00/hr |
| Coming back | `start.sh`, ~5 min | `setup.sh` (+ `e2e-setup.sh`), ~20 min |
| In-flight jobs | Waited out — the stop fails rather than strand them ([why](#why-the-drain-wait-is-not-optional-q434)) | Quoted back in the confirmation; deleting is your call |
| Use when | Between CI sessions — the normal at-rest state | Done for the foreseeable future, or converging a drifted cluster |

**Deleting is not a cost optimisation.** The cluster is a zonal Standard cluster, one of which is free per billing account, so at 0 nodes it already bills nothing — `stop.sh` captures the entire saving (see [Cost reference](#cost-reference)).
Delete because you want the environment *gone* or *rebuilt*, not because you want the bill lower.

```bash
export PROJECT CLUSTER ZONE REPO   # from the Variables section
scripts/dogfood/delete.sh
```

The script routes CI off the cluster (resetting `GAG_RUNNER` and `GAG_E2E_RUNNER`) **before** deleting, so no dispatched job can aim at a cluster that is mid-deletion; deletes the cluster; prunes the now-dead kubeconfig context so a later `kubectl` fails loudly instead of timing out against a cluster that no longer exists; and reports any disk or reserved address that outlived the cluster, since those keep billing.
It quotes the live node and in-flight worker-pod counts back at you in the confirmation, so deleting a cluster that is actually busy is a decision rather than an accident.
Unlike `stop.sh` it does not wait for the drain — deleting the cluster takes the worker nodes with it, so there is nothing left to strand.

`ASSUME_YES=1` skips the prompt for automation.
A missing cluster is a no-op, so the script is safe to re-run.

**What survives, and therefore makes recreate possible:** the GCP project, billing link, enabled APIs, quota, the GitHub App and its installation, and the App private key in the local Keychain.
**What does not:** the GAG control-plane install, every tenant namespace and CR, the cert-manager CA and all certs minted from it, the per-tenant metrics PKI, the in-cluster GitHub App secret, and every node pool.
Recreate rebuilds these; it does not restore them.

### Recreate is proven end-to-end (Q380)

> **Validated 2026-07-25.** The cluster was rebuilt from nothing — empty project, no cluster, no node pools — by running `setup.sh` and then `e2e-setup.sh` straight through.
> Both completed successfully.
> One real gap was found and fixed on the way; everything else worked unchanged.

`delete.sh` was already proven by the 2026-07-20 deletion: occupancy probes read the cluster correctly (0 nodes, 0 worker pods), the runner labels were reset to `ubuntu-latest` *before* the delete, the kubeconfig context was pruned, and the orphan sweep confirmed no disks or reserved addresses survived.

`setup.sh` is now proven too, with one correction:

- **The cluster create was missing Workload Identity (fixed).** `create_cluster` did not pass `--workload-pool`, even though Part A3 above documents it as a hard prerequisite of the Part F e2e pool, whose `--workload-metadata=GKE_METADATA` is rejected with a 400 without it.
  Every previous run took the "already exists — skipping create" branch against a cluster that had had Workload Identity retrofitted by hand, so the gap was invisible.
  It is a create-time property, which is what makes it nasty: omitting it is not observable until `e2e-setup.sh` runs much later, and is then repairable only by a separate control-plane update.
  The flag is now in the script; the rebuilt cluster reports `workloadIdentityConfig.workloadPool = actions-gateway-dogfood.svc.id.goog`, and the Part F `e2e` pool created cleanly against it — the direct proof that the fix is both necessary and sufficient.
- **`workers-od` created cleanly.** The pool this section previously called the most likely thing to break did not break: its first-ever scripted creation produced exactly the intended shape (`e2-standard-4`, `pd-standard` 100 GB, no autoscaling, starts at 0, taint `dedicated=workers:NoSchedule`, non-preemptible).
- **The App secret round-trip worked with the secret absent.** `create_secret` applies the Keychain-read key through `kubectl create --dry-run=client | kubectl apply`, which upserts identically whether or not the Secret already exists, so the absent case needed no change.

**One artifact to expect on a from-zero run:** the preflight (`validate-cluster.sh`) reported `metrics-server: metrics.k8s.io API is not Available`.
That is a race, not a real gap — GKE's metrics-server addon is still starting while `setup.sh` reaches preflight, and it was measured `Available` about two minutes into the cluster's life.
The check is a WARN, so the bootstrap proceeded correctly, but it is a false negative and a run with `VALIDATE_STRICT=1` would have failed the from-zero path on it.

The preflight now retries that check within a bounded budget instead of looking once ([Q397](../STATUS.md#Q397)): 150 s for a registered-but-not-yet-`Available` `metrics.k8s.io` APIService, and a 15 s grace for the APIService to appear at all before it concludes metrics-server is absent (so a cluster genuinely without it still warns promptly).
The retry is unit-tested against faked probes in `scripts/e2e/validate-cluster-test.sh`; the from-zero timing itself is only observable on a real recreate, so **the next dogfood recreate is where this gets confirmed** — expect a `[WAIT]` line followed by `PASS`, not a WARN.

To go further and remove the project itself (irreversible, and it takes the GCP-side App wiring with it):

```bash
gcloud projects delete "$PROJECT"
```

---

## Part F — E2e on GKE (Kata Containers)

The e2e suite runs `kind create cluster` inside the runner pod, which requires a Docker daemon (Docker-in-Docker).
The clean solution is [Kata Containers](https://katacontainers.io/): each pod gets its own lightweight microVM with a real Linux kernel (backed by KVM).
Inside the microVM, Docker runs normally — no user-namespace tricks, no kernel feature gaps — so kind works exactly as it does on a GitHub-hosted runner.

The security profile stays **`baseline`**: the pod itself does not need `privileged: true` because the Kata microVM is the isolation boundary.
If anything escapes from within kind, it hits the microVM's kernel, not the GKE node.

**What GKE provides:** Standard clusters support nested VMs via `--enable-nested-virtualization` on a node pool, which exposes `/dev/kvm` on the node.
Kata uses `/dev/kvm` to spin up microVMs.
[Official GKE docs.](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/nested-virtualization)

**Machine type note:** nested virtualization is required (Kata needs `/dev/kvm`) and GCP supports it on a specific list, which the API names when it rejects a create:

> A2, A3, C2, C3, C4, C4D, C4N, G2, H3, H4D, N1, N2, N4, N4D, Z3 and M4

**The AMD families are not on it** — `C2D` and `N2D` are both rejected, as is E2 (used in Parts A–B).
An earlier revision of this note claimed `n2/n2d/c2/c2d`; that was wrong, and cost a pool re-create when `c2d-standard-8` was refused outright.
The e2e pool uses `n2-standard-8` (8 vCPU: the measured runner peak is ~5 vCPU — Q248, which is why the original `n2-standard-4` was outgrown).

**The quota that actually binds is `CPUS_ALL_REGIONS`, not any family quota.** Two `v1.3.0-rc.5` validation runs died on `FailedScaleUp: GCE quota exceeded` while `C2_CPUS`, `N2_CPUS` and regional `CPUS` all sat near zero.
The constraint is a **global, project-wide** CPU limit that defaults to **32**, which this cluster saturates exactly at full stretch:

| Pool | Machine | Max nodes | vCPU |
|---|---|---|---|
| `default-pool` | `e2-standard-2` | 2 (derived, manual) | 4 |
| `workers` | `e2-standard-4` | 8 (autoscale max) | 32 |
| `e2e` | `n2-standard-8` | 2 (autoscale max) | 16 |
| `workers-od` | `e2-standard-4` | 0 at rest (manual) | 0 |
| | | | **52 worst case** |

With `workers` at 7 nodes the project sits at 4 + 28 = 32/32, and the e2e pool's scale-up is refused for want of 8.
**The gate can therefore starve itself**: its deploy phase routes CI to GAG, scaling `workers` out of the same global budget the e2e leg then needs.
Raised to **64** on 2026-08-03 (approved immediately), covering the 52 ceiling plus headroom for spot replacement and GKE surge.
Re-measured 2026-08-12: still 64, and the pool shapes above are unchanged.

**Two of those four ceilings move without anyone editing this table**, which is why the fit is not a fact to record once.
`default-pool` is derived per run from the deployed always-on tenants ([lib/pool.sh](../../scripts/dogfood/lib/pool.sh)), so a third tenant adds 2 vCPU; `workers-od` is sized by hand for a benchmark campaign and only *usually* returned to 0, so four nodes left up is 16 vCPU that puts the ceiling at 68 against a 64 limit.

**So the competition is capped rather than out-run** (Q631).
Raising the limit again moves the collision instead of removing it: any new ceiling is one benchmark pool or one tenant away from being reached.
`validate-release.sh` therefore takes the e2e and system pools' ceilings off the *live* budget before it routes CI, and holds `workers` to what is left.
The arithmetic is in [lib/quota.sh](../../scripts/dogfood/lib/quota.sh), the operator-facing behaviour in [release.md](../operations/release.md#the-gate-reserves-the-e2e-pools-cpu-budget).
At today's 64 the reservation leaves room for 11 `workers` nodes against a configured 8, so it changes nothing; at the old 32 it would have held `workers` at 3 and the e2e pool would have got its 16 vCPU.

**Read the API's error, not the autoscaler's event.** `FailedScaleUp` never names the quota.
`gcloud container clusters resize` returns a 429 whose body does: `resource "CPUS_ALL_REGIONS": request requires '8.0' and is short '8.0'`.
Chasing the family quotas instead cost this release two runs and three wrong diagnoses.
Regional `CPUS` and `N2_CPUS` are both 200 on this project (measured 2026-08-12), so neither can be the refusal.

**Why N2 and not C2.** Separate from the above: the regional `C2_CPUS` default is 8, one node of this shape, and a request to raise it was **denied** on 2026-07-31 — while an identical 8→16 ask for `IN_USE_ADDRESSES` was approved 33 minutes earlier, so the size of the ask was not the discriminator.
`N2_CPUS` defaults to 200 and n2 is this pool's original family, so it is already proven here.
Note this alone would not have fixed anything; the global limit was the real blocker.

### F1. Run the one-time setup script

```bash
export CLUSTER ZONE APP_ID INSTALLATION_ID   # from the Variables section
scripts/dogfood/e2e-setup.sh
```

This script owns the **cluster infra** the kustomize overlays can't express:
1. Creates the `e2e` node pool (n2-standard-8 spot, nested virt, `--workload-metadata=GKE_METADATA` — requires cluster-level `--workload-pool`, see Part A — autoscaling 0→2, taint `dedicated=e2e:NoSchedule`).
   The pool's node labels carry **both** `gag.dev/kata-ci=true` (installer scope) and `katacontainers.io/kata-runtime=true` — the latter pre-baked because the cluster autoscaler simulates scale-from-zero against configured labels only (Q286)
2. Installs the Kata DaemonSet, scoped to e2e pool nodes only via `gag.dev/kata-ci` and tolerating the pool taint (the chart ships no tolerations — Q286).
   The system and workers pools use COS; Kata requires Ubuntu or COS 1.28.4+, and the DaemonSet labels nodes `katacontainers.io/kata-runtime=true` after install
3. Creates the `kata` RuntimeClass alias (over the chart-owned `kata-qemu` handler) with a node scheduling rule that prevents Kata pods from scheduling before the DaemonSet has finished installing
4. Creates the `gag-dogfood-e2e` namespace (v2 marker `actions-gateway.com/tenant=managed`) and the GitHub App Secret

The **tenant objects** (ResourceQuota, `ActionsGateway`, `ClusterRunnerTemplate`, `RunnerSet`, egress policy, and the namespace's security-profile gates) are owned by the worker-isolation overlays under [`deploy/dogfood-e2e/`](../../deploy/dogfood-e2e/README.md) and applied on demand by `e2e-start.sh` (`E2E_VARIANT=kata|dind`, default `kata` since the Q286 flip; `dind` is the explicit opt-in fallback).
They are authored **directly at `actions-gateway.com/v2beta1`** (Q231) — the graduated served+storage front-door shape (Q273), deliberately unlike `scripts/dogfood/setup.sh` (main dogfood), which authors at v2alpha1 to exercise the conversion webhook.

In both variants the DinD native sidecar runs `dockerd` on `tcp://localhost:2375` (no TLS — pod-internal only) and the `runner` container sets `DOCKER_HOST=tcp://localhost:2375`.
Because all containers in a pod share a network namespace, kind's API server is reachable at `localhost:<apiserver-port>` from the runner.

> **The default variant is `kata`** (unprivileged kind-in-Kata) — live-validated green on GAG 2026-07-17 ([Q286](archive/kata-on-gke.md); the AC#5 run plus the seven live-found fixes are in [what the live session found](archive/kata-on-gke.md#what-the-live-session-found-2026-07-16)).
> `dind` (privileged DinD, clean-green since 2026-07-07) stays available as the explicit opt-in fallback for environments without nested virtualization.

### F2. Workflow change — already wired

`.github/workflows/e2e-reusable.yml` routes to GAG through two mechanisms with one precedence line:

```yaml
runs-on: ${{ fromJSON(inputs.runner || vars.GAG_E2E_RUNNER || '"ubuntu-latest"') }}
```

- **Run-scoped (the default path):** dispatch one run with the `runner` input — `gh workflow run e2e-test.yml --ref main -f runner='"gag-ci-e2e"'` — and only that run lands on GAG.
  This is what `validate-release.sh` does; there is no window in which anyone else's PR or merge can be caught.
- **Repo-wide window (explicit opt-in):** `E2E_ROUTE_VAR=1 e2e-start.sh` sets `vars.GAG_E2E_RUNNER`, routing **every** e2e job — concurrent sessions' included — until `e2e-stop.sh` resets it.
  A job caught mid-window wedged main CI when the teardown deleted the AGC under it (2026-07-31), which is why this stopped being the default.

Both `e2e-test.yml` (kindnet) and `e2e-calico.yml` (Calico) call this reusable workflow, so the one line covers both CNI variants.
Because the RunnerSet is ScaleSet (single-label), both the input and the variable are a single JSON **string** (`"gag-ci-e2e"`), not the old Classic multi-label array.
CI is unaffected until you route.

### F3. E2e operations — on-demand

The e2e tenant's AGC is **on-demand** (Q231), not always-on: its standing ~500m-CPU AGC pod competes with the CI AGC + GMC + Athens on the `e2-standard-2` system pool (leaving the CI AGC `Pending`/`Insufficient cpu`), and the `SSD_TOTAL_GB=500` quota bounds the workers pool.
So rather than keep it running, `e2e-start.sh` applies the tenant to spin the AGC up per e2e session and `e2e-stop.sh` deletes the `ActionsGateway` to tear it back down (the namespace, Secret, quota, template, and RunnerSet are inert without the gateway and kept).
Before the delete, `e2e-stop.sh` **drains**: it waits for jobs still queued on the `gag-ci-e2e` scale set and for in-flight e2e worker pods, and on timeout fails *without* deleting — the AGC is the only thing that can serve a queued job or reap a worker pod, and deleting it under either strands them (2026-07-31: an orphaned worker pod's do-not-disrupt annotations pinned a billable node, and a stranded queued run wedged main's e2e concurrency group).
`SKIP_E2E_DRAIN=1` overrides knowingly.

The worker half of that is now fixed in the product rather than only in the script (Q547): the AGC reaps its worker pods when it observes the gateway's `deletionTimestamp`, and the GMC holds teardown open until it has.
So a delete under in-flight workers no longer strands a node — it *kills those jobs*, which is why the script still drains first.
The queued-job half is unchanged and remains the drain's own reason: nothing reaps a run that was never acquired.
The e2e **node** pool is already on-demand independently (autoscales 0→2 on job arrival, back to 0 ~10 min after drain).

A single `e2-standard-2` system node no longer leaves ~500m free for the on-demand AGC (DaemonSet/kube-dns growth), so it stays `Pending` and the start script's Ready wait times out (Q335).

The same ceiling applies to the **always-on** tenants, not just the on-demand e2e one: measured on a live node, `e2-standard-2` offers **1930m allocatable** against a **~1080m kube-system baseline**, leaving room for exactly *one* of the two 500m tenant AGCs (`dogfood-agc`, `dogfoodss-agc`).
At 1 node the two race for the node and the loser stays `Pending` indefinitely — and because `dogfood/start.sh` waits on `instance=dogfood` specifically, a `dogfoodss` win times the wait out and fails the caller (this is what blocked `validate-release.sh` before it ever reached the e2e leg).
Rather than pin a count that a third tenant would silently outgrow (Q357), `dogfood/start.sh` **derives** the size from the deployed always-on `ActionsGateway` CRs via `scripts/dogfood/lib/pool.sh`: one node per always-on tenant AGC, floored at the live-validated **2** (the on-demand e2e tenant's namespace is excluded — its AGC packs into the non-first nodes' larger headroom, validated with all three AGCs on 2 nodes).
One-node-per-AGC is deliberately conservative; a spare node during the running window is cheaper than a `Pending` AGC.
`SYSTEM_NODES` pins the size explicitly instead (a pin below the derived need warns).
`scripts/dogfood/pool-test.sh` (in `make check` via `make scripts-test`) asserts the sizing against stubs.

The e2e leg triggers the matrix with `gh workflow run` (a `workflow_dispatch` carrying the run-scoped `runner` input above).
The dispatched run enters the workflow's per-ref concurrency group, which holds **one pending slot** — a run dispatched into a busy group parks there, and the next push to main cancels it, aborting the gate after the scale-up, deploy, and e2e AGC were already paid for (the rerun-era analog was observed 2026-07-20, after PR #709: `gh run rerun` refuses an in-flight run outright).
`validate-release.sh` therefore *settles the lane* in `main()` before the trap arms and before anything billable: it waits out an in-flight run for up to `E2E_WAIT_TIMEOUT` (default 1800s) and otherwise fails there, where failure is free.
The dispatch also resolves the **new** run's id (by watching the newest `workflow_dispatch` run change from a pre-dispatch baseline — `gh workflow run` prints no id) and fails rather than watching a stale run.
`scripts/dogfood/validate-release-test.sh` (in `make check` via `make scripts-test`) asserts these paths against stubs.

The same fail-early rule covers local tools (Q356): the CRD smoke is the gate's *last* leg, so its `cosign` dependency (`.build/cosign`, `make cosign`; `COSIGN=` to override) used to be checked ~25 minutes in — aborting after a full node scale-up + deploy + e2e cycle.
`main()` now preflights it alongside the `require_cmd` checks, before the confirmation prompt and anything billable; `crd_smoke` only consumes the resolved `COSIGN_BIN`.
The test script asserts the preflight's paths too.

`e2e-start.sh` scales the system pool up for the e2e window (`E2E_SYSTEM_NODES`, default `2`, never below the derived running size — a smaller resize would evict a tenant AGC), and `e2e-stop.sh` restores the running size afterwards (derived the same way as `dogfood/start.sh`, so the two agree by construction; `SYSTEM_POOL_AT_REST_NODES` pins it instead); `dogfood/stop.sh` later takes the pool to 0 for the zero-cost-at-rest state.
All resizes pin `--project` and `--zone` and are idempotent, so re-running any of these scripts is safe.

```bash
export PROJECT CLUSTER ZONE REPO   # from the Variables section

# Enable (requires the system pool up via dogfood/start.sh first): scales the
# system pool up for the e2e window, spins up the on-demand AGC, then routes
# e2e onto GAG.
scripts/dogfood/e2e-start.sh

# Disable: routes e2e back to github-hosted, deletes the AGC (frees ~500m), then
# restores the system pool to the derived running size.
scripts/dogfood/e2e-stop.sh
```

The e2e pool toggles independently from the CI pool — you can run only one or both at the same time.

> **Migrating a pre-existing Classic e2e tenant (one-time).** If the tenant's RunnerSet was previously authored at `v2alpha1` with `acquisitionProtocol: Classic`, the conversion webhook stamped a `conversion.actions-gateway.com/acquisition-protocol: Classic` annotation on the stored v2beta1 object.
> `kubectl apply` of the new v2beta1 ScaleSet spec does **not** strip that webhook-added annotation, so the AGC keeps running Classic (it reads the RunnerSet through the v2beta1→v2alpha1 conversion, which restores `Classic` from the annotation).
> Recreate the RunnerSet once so it comes back v2beta1-native (annotation defaults to `ScaleSet`): `kubectl delete runnerset ci-e2e -n gag-dogfood-e2e` then `kubectl apply -k deploy/dogfood-e2e/overlays/dind`.
> A tenant created fresh by `e2e-setup.sh` (authored at v2beta1 directly) never carries the annotation.
> Verified live 2026-07-07 (Q231).

---

## Alternatives considered for e2e DinD

| Approach | Works? | Security | Notes |
|---|---|---|---|
| **Kata Containers** (this plan) | ✅ | Strong — KVM microVM boundary | Requires N2 node + nested virt; Kata DaemonSet install |
| **Sysbox** | ❌ | Medium — user-namespace | [sysbox#920](https://github.com/nestybox/sysbox/issues/920): kind broke for K8s v1.25+; our e2e uses K8s 1.35 |
| **gVisor** | ❌ | High (for workloads) | Intentionally does not support nested container runtimes |
| **Privileged DinD** | ✅ | None — host kernel exposure | Requires `securityProfile: privileged` + platform namespace label; last resort |
| **Keep e2e on GitHub-hosted** | ✅ | N/A | e2e runs in ~9 min for free; no speed/cost problem |

---

## Recurring maintenance + debug ops

Between turn-up and teardown, a handful of maintenance and debug operations recur: rescaling a node pool by hand, reinstalling `kata-deploy` after a version bump, bouncing a wedged AGC, launching a throwaway pod on the e2e pool to bisect worker-shape problems, and confirming `/dev/kvm` on the e2e nodes.
These are folded into [`scripts/dogfood/ops.sh`](../../scripts/dogfood/ops.sh) as named subcommands so they aren't re-typed ad hoc (Q342).
Each pins `--project`/`--zone` on every gcloud call and verifies the resolved kubectl context before any mutating kubectl call — the same target-safety layer the lifecycle scripts use — so they need no per-command `PROD_GUARD_OVERRIDE` (the hook doesn't parse a `bash scripts/…` invocation; see [kind-iteration.md](../development/kind-iteration.md#verify-the-resolved-target-before-any-mutating-command)).
All subcommands are idempotent and safe to re-run.

```bash
export PROJECT CLUSTER ZONE   # from the Variables section

scripts/dogfood/ops.sh pool-scale <pool> <nodes>  # resize default-pool|workers|e2e
scripts/dogfood/ops.sh kata-install               # (re)install kata-deploy + `kata` RuntimeClass
scripts/dogfood/ops.sh agc-bounce [ci|e2e]        # roll-restart the CI (default) or e2e AGC + wait
scripts/dogfood/ops.sh debug-pod [--kata]         # interactive shell pod on the e2e pool (bisecting)
scripts/dogfood/ops.sh kvm-check [<node>]         # verify /dev/kvm on the e2e node(s)
scripts/dogfood/ops.sh at-rest                    # is anything still billing? 0 at rest, 1 not, 2 unreadable
```

`kata-install` reuses exactly the install logic `e2e-setup.sh` runs (both source `scripts/dogfood/lib/kata.sh`), so it re-applies the DaemonSet + RuntimeClass without re-running the full billable one-time setup.
`debug-pod`/`kvm-check` target the `e2e` pool, which autoscales from 0 — scale a node up first (`ops.sh pool-scale e2e 1`) or run them during an in-flight e2e session.

`at-rest` is the read-only one: it answers "did the teardown actually land" by counting the project's Compute Engine instances, and reports an unreadable project as `UNKNOWN` (exit 2) rather than as zero.
Reading `currentNodeCount` off the cluster object cannot make that distinction, because gcloud prints the same empty string for a cluster at 0 nodes and for a projection that resolved to nothing ([release.md](../operations/release.md#confirming-the-cluster-is-actually-at-rest), Q779).

## Operations quick-reference

| Action | Script |
|---|---|
| One-time bootstrap **and** full recreate after a delete: cluster + node pools + GAG install + tenant | `scripts/dogfood/setup.sh` |
| Start cluster + dispatch opt-in validation runs onto GAG | `scripts/dogfood/start.sh` |
| Stop cluster + reset GAG runner label, after draining in-flight workers (normal at-rest state) | `scripts/dogfood/stop.sh` |
| Delete the cluster entirely (see [Part E](#part-e--teardown) — destroys the GAG install and all tenant state) | `scripts/dogfood/delete.sh` |
| Enable e2e on GAG | `scripts/dogfood/e2e-start.sh` |
| Disable e2e on GAG | `scripts/dogfood/e2e-stop.sh` |
| One-time e2e pool + Kata setup | `scripts/dogfood/e2e-setup.sh` |
| Recurring maintenance + debug ops (pool resize, kata reinstall, AGC bounce, debug pod, `/dev/kvm` check) | `scripts/dogfood/ops.sh <subcommand>` |
| Confirm nothing is still billing after a stop, a delete, or a killed release gate | `scripts/dogfood/ops.sh at-rest` |

All scripts read `PROJECT`, `CLUSTER`, `ZONE`, `REPO` (and `APP_ID`, `INSTALLATION_ID` for the setup scripts) from the environment.
Export the Variables block once per shell session.
(`ops.sh` needs only `PROJECT`, `CLUSTER`, `ZONE`.)

---

## Cost reference

| Scenario | $/hr | $/day (4 hr active) |
|---|---|---|
| Cluster at rest (0 nodes) | $0.00 | $0.00 |
| Cluster deleted | $0.00 | $0.00 |
| System node only, no jobs | $0.067 | $0.27 |
| System + 1 spot CI worker (e2-standard-4) | ~$0.11 | — |
| System + 4 spot CI workers (peak) | ~$0.23 | — |
| System + 2 spot e2e nodes (n2-standard-4, peak) | ~$0.18 | — |

A typical dogfood session (scale up, run a few PRs, scale down): under $0.50.

**E2e cost per PR** (kindnet + Calico in parallel, ~10 min each): 2 nodes × $0.058/hr × 10 min ≈ **$0.019**.
