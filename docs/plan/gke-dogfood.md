# GKE Dogfood Runbook

On-demand GKE cluster for dogfooding GAG's own CI. The cluster costs $0 at
rest (zero nodes), roughly $0.07/hr when idling with the system node only, and
adds ≈$0.04/hr per spot worker node while jobs are running.

**What runs where after setup**

| Workflow | Jobs migrated to GAG | Jobs kept on `ubuntu-latest` |
|---|---|---|
| `unit-test.yml` | `lint`, `shellcheck`, `vendor-check`, `tidy-check`, `unit-test`, `coverage` | `changes` |
| `integration-test.yml` | `integration-test` | `changes` |
| `e2e-reusable.yml` | `e2e` (kindnet + Calico, via Kata + DinD sidecar) | `changes` in callers |

The `changes` (paths-filter) jobs are intentionally kept on `ubuntu-latest`.
They are the gatekeepers for every downstream job: if they queue behind a
down cluster, CI appears broken.

## Variables

Fill these in once before running any command. Put them in your shell
profile or paste them at the start of each terminal session.

```bash
CLUSTER=gag-dogfood
ZONE=us-east1-b                   # moved from us-central1-b 2026-06-30 (region-wide e2-standard-2 stockout)
PROJECT=actions-gateway-dogfood   # must be globally unique; append 4 digits if needed
REPO=actions-gateway/github-actions-gateway
APP_ID=3752347
INSTALLATION_ID=135739122         # actions-gateway org install (re-derive via Part C1)
```

> **Zone choice:** `ZONE` moved from `us-central1-b` to `us-east1-b` on
> 2026-06-30 after `us-central1` went region-wide `ZONE_RESOURCE_POOL_EXHAUSTED`
> for `e2-standard-2`. GCP exposes no capacity API — there is no way to query a
> zone's free capacity ahead of time, so pick a zone empirically: if cluster or
> node-pool creation fails with a stockout error, try another zone/region.

---

> **Shortcut:** Parts A3–B8 (cluster, node pools, GAG install, tenant) are
> automated by [`scripts/dogfood/setup.sh`](../../scripts/dogfood/setup.sh) —
> idempotent and safe to re-run with some of the work already done. Complete
> A1–A2 first (project + billing + APIs), export the Variables block, then run
> the script. The manual steps below document what it does, step by step.

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

Link billing in the console — the CLI requires a billing account ID which is
hard to look up:
https://console.cloud.google.com/billing → My Projects → select `$PROJECT` →
Change billing → pick your billing account.

```bash
# Enable required APIs (run after billing is linked)
gcloud services enable container.googleapis.com compute.googleapis.com
```

### A3. Create GKE cluster (system node pool)

```bash
# Standard zonal cluster — one free per billing account, no cluster fee.
# --enable-dataplane-v2: Cilium-based CNI that enforces NetworkPolicy (required by GAG).
# No autoscaling on the default pool — it's manually scaled to 0/1 to start/stop.
gcloud container clusters create "$CLUSTER" \
  --zone="$ZONE" \
  --release-channel=regular \
  --enable-ip-alias \
  --enable-dataplane-v2 \
  --machine-type=e2-standard-2 \
  --num-nodes=1 \
  --disk-size=50GB \
  --no-enable-basic-auth \
  --no-issue-client-certificate
```

### A4. Add spot worker node pool

```bash
# Spot e2-standard-4 (4 vCPU / 16 GiB), autoscaling 0→4.
# Taint keeps GMC/AGC/proxy off worker nodes; worker pods tolerate it (see Part B).
gcloud container node-pools create workers \
  --cluster="$CLUSTER" \
  --zone="$ZONE" \
  --machine-type=e2-standard-4 \
  --spot \
  --num-nodes=0 \
  --min-nodes=0 \
  --max-nodes=4 \
  --enable-autoscaling \
  --node-taints=dedicated=workers:NoSchedule \
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
GKE also ships the Kubernetes Metrics Server by default (required for the
proxy pool's HPA).

> **NodeLocal DNSCache on Dataplane V2 (Q229).** If the cluster runs NodeLocal
> DNSCache, Dataplane V2 redirects cluster-DNS traffic to the per-node
> `node-local-dns` pod, and the tenant egress `NetworkPolicy` must allow that
> backend or the AGC crash-loops on its first GitHub token fetch
> (`lookup api.github.com: i/o timeout`). GAG's DNS egress rule includes the
> `node-local-dns` peer as of Q229; use a build that has it. Diagnosis and the
> verification command are in
> [Troubleshooting → DNS Times Out Under the Egress NetworkPolicy](../operations/troubleshooting.md#dns-times-out-under-the-egress-networkpolicy-gke-dataplane-v2--nodelocal-dnscache).

```bash
make validate-cluster
```

### B2. Create Helm values file

```bash
cat > tmp/values-dogfood.yaml <<'EOF'
# Dogfood / dev mode: pin a released image tag rather than digests.
# Production installs must use digest-pinned images from the release page.
# NOTE: `latest` is never published (publish.yml builds only on v* tags), so a
# real released tag is required — see https://github.com/actions-gateway/github-actions-gateway/pkgs/container/gmc
allowFloatingImageTags: true
# Single GMC replica for dogfood (production wants the default 2 for HA); frees
# capacity on the small system node for the per-tenant AGC pod.
replicaCount: 1
gmc:
  image:
    tag: v1.1.0-rc.6
agc:
  image:
    tag: v1.1.0-rc.6
proxy:
  image:
    tag: v1.1.0-rc.6
# WRAPPER_IMAGE drives Q235 worker-wrapper injection — the GMC forwards it to
# every AGC, which injects the wrapper into each worker pod so the runner
# container can be the unmodified upstream actions-runner. Pin it: the chart's
# default wrapper tag is empty, which renders wrapper:latest (never published)
# and ImagePullBackOffs the injection.
wrapper:
  image:
    tag: v1.1.0-rc.6

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

The v2 CRDs ship in a separate, opt-in chart (`actions-gateway-crds-v2`). The GMC
runs its v2 controllers unconditionally, so the CRDs must be installed — and
**at the same release as the GMC image**, because the v2 *alpha* schema drifts
between releases (e.g. `ActionsGateway.spec.githubAppRef` in `v1.1.0-rc.2` became
the `spec.credentials` discriminated union in `v1.1.0-rc.3`); a mismatch makes
every reconcile fail validation. A stale CRD that still exposes `githubAppRef`
silently drops the credential — the GMC reads an empty App ref and provisions the
AGC for workload-identity (Vault) instead, and the AGC crash-loops on
`read appId: … no such file or directory`. Always upgrade this chart in lockstep
with the GMC image (`helm upgrade`, not just `install`).
`scripts/dogfood/setup.sh` git-archives the chart at `$GAG_IMAGE_TAG`; the manual
equivalent for the pinned `v1.1.0-rc.6`:

```bash
git archive v1.1.0-rc.6 charts/actions-gateway-crds-v2 | tar -x -C tmp/
helm install actions-gateway-crds-v2 tmp/charts/actions-gateway-crds-v2 \
  --namespace gmc-system --create-namespace
```

Then install the GMC chart:

```bash
helm install gag charts/actions-gateway \
  --namespace gmc-system --create-namespace \
  --values tmp/values-dogfood.yaml

kubectl rollout status deployment/gmc-controller-manager -n gmc-system --timeout=3m
```

> **GKE PriorityClass admission:** the GMC runs with
> `priorityClassName: system-cluster-critical`, which GKE permits only in a
> namespace carrying a matching scoped `ResourceQuota` — without one the GMC pods
> fail with `insufficient quota to match these scopes`. The chart ships that
> permit-only quota (`gmc-critical-pods`) by default
> (`systemCriticalPriorityQuota.enabled=true`), so no manual `kubectl apply` is
> needed here. See [install.md](../operations/install.md#gke-and-other-restricted-priorityclass-clusters).

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

`vendor-check` and `tidy-check` re-fetch Go modules from `proxy.golang.org` on
a cold cache. GKE Dataplane V2's managed Cilium lacks the `CiliumNetworkPolicy`
CRD, so the `CiliumFQDN` egress mode is unusable here; a CIDR allowlist for
Google-fronted hosts like `proxy.golang.org` would be a footgun (it opens all
of Google's frontend). Athens sidesteps both constraints: it runs in-cluster
with free egress and serves cached modules to workers over a plain HTTP port
that does not need a CNI FQDN backend.

Athens is not covered by the workload `NetworkPolicy` (`actions-gateway/component: workload`
label). Workers reach it via an additive `NetworkPolicy` that opens port 3000
from workload pods to Athens pods. The Service is named `go-module-proxy`
(not `athens`) to avoid Kubernetes injecting `ATHENS_PORT=tcp://...` into
pods in the namespace — Athens misreads that as its listen address.

```bash
kubectl apply -k deploy/athens
kubectl rollout status deployment/athens -n gag-dogfood --timeout=120s
```

Verify Athens is healthy:

```bash
kubectl get pods -n gag-dogfood -l app=athens
kubectl logs -n gag-dogfood -l app=athens --tail=20
```

Athens pre-warms lazily — the first `vendor-check`/`tidy-check` run is slower
while modules download; subsequent runs are cache hits from the PVC.

> **Why plain HTTP (no TLS)?** Athens serves public Go module zips; there is
> nothing confidential in transit. Integrity is upheld by the Go toolchain's
> `go.sum` verification — every module downloaded from Athens is checked against
> the committed `go.sum` regardless of `GONOSUMDB`, so a tampered response is
> caught before it reaches the build. Adding TLS would require cert management
> (cert-manager or a self-signed CA wired into every worker image) for no
> meaningful security gain in this single-tenant cluster. Revisit if Athens is
> extended to a shared multi-tenant cluster or used to serve private modules.

### B7. Create the v2 tenant objects

The v2 API decomposes the v1 monolithic `ActionsGateway` into `ActionsGateway`
(gateway + credentials), `RunnerTemplate` (worker pod shape), and `RunnerSet`
(runner group). This is the minimal **direct-egress** form — no `EgressProxy`,
so workers egress directly to GitHub, still behind the default-deny egress
NetworkPolicy (DNS + GitHub CIDR), just without a per-tenant egress IP. Attach
an `EgressProxy` and set `spec.defaultProxyRef` on the gateway to add per-tenant
egress IP attribution.

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
          resources:
            requests:
              cpu: "2"
              memory: "4Gi"
            limits:
              cpu: "4"
              memory: "8Gi"
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
  runnerLabels: ["self-hosted", "linux", "gag-ci"]
  maxListeners: 8
  maxWorkers: 4
EOF
```

> **v2 prerequisites:** Kubernetes ≥ 1.31 (the `RunnerSet` field-selector
> scoping, KEP-4358) and the `actions-gateway-crds-v2` chart from B3. The
> `spec.credentials` discriminated-union shape is the `v1.1.0-rc.3` schema
> (`rc.2` used a flat `spec.githubAppRef`) — if you pin a different
> `$GAG_IMAGE_TAG`, match the CRD chart to that release and use its credential
> shape.

### B8. Validate

```bash
# Gateway Ready=True; RunnerSet shows its template + egress mode (Direct).
kubectl get actionsgateway,runnerset -n gag-dogfood -o wide
kubectl get pods -n gag-dogfood   # the dogfood-agc Deployment pod should be Running

# Runners should appear within ~2 min of the AGC becoming Ready
gh api /repos/"$REPO"/actions/runners \
  --jq '.runners[] | {name, status, labels: [.labels[].name]}'
```

> **Validated on `v1.1.0-rc.6` (2026-07-01).** Control plane (GMC + AGC roll to
> rc.6, gateway `Ready=True`, App-Secret credential path, Q229 egress-DNS token
> fetch, baseline listener online — the multiplexer keeps **one** idle listener and
> scales up to `maxListeners` on job demand, so a single online runner at rest is
> healthy, not stuck), **production CI routing** (`GAG_RUNNER` →
> `["self-hosted","linux","gag-ci"]`; a `gh run rerun` dispatched its job to
> `gag-ci`, the runner went busy, the `workers` spot pool autoscaled `0 → 1`),
> and **Q235 worker-wrapper injection**: with the `RunnerTemplate` runner
> container named but image-less, the AGC gap-filled the bare upstream
> `ghcr.io/actions/actions-runner` (Q233), injected `ghcr.io/actions-gateway/wrapper:v1.1.0-rc.6`
> as a read-only OCI image volume at `/opt/actions-gateway` (native image volume,
> no initContainer), and set the container command to `/opt/actions-gateway/wrapper`.
> rc.6's headline delta over rc.5 is the **Q247 job-renewal fix**, live-validated
> here: the full privileged-DinD e2e ran green end-to-end on GAG (jobs renewed with
> the acquire response's job-scoped token by `RunnerRequestID`, with bounded
> `RenewJob` calls so a hung renewal can't wedge the loop). rc.6 also carries the
> Q242 G.1 egress destination allowlist, a no-op for this direct-egress dogfood.
>
> **Production CI green — per-job yes, concurrent matrix blocked on Q259 (2026-07-01).**
> A same-day turn-up routed the real repo's CI to `gag-ci` (`GAG_RUNNER →
> ["self-hosted","linux","gag-ci"]`) and confirmed **every** migrated job —
> `vendor-check`, `tidy-check`, `unit-test` (`-race`), `coverage`, `integration-test`,
> `lint`, `shellcheck` — runs **green** on `gag-ci` when given a worker (verified via
> single-job reruns). **Q246 held** (the `dogfood-workload` NetworkPolicy carried 7340
> GitHub CIDR egress peers throughout, never blanked; no release-asset download timeout —
> shellcheck's release tarball and setup-go's toolchain both fetched fine) and **Q247
> held for jobs that run** (`integration-test`, ~12 min, renewed its lock and completed
> green; RunnerSet recovered to baseline with no orphaned pods). **But the *concurrent*
> full matrix does not go green:** bursting all jobs onto `gag-ci` at once serializes to
> ~1 worker even with ample node room (nodes at 35%/9% CPU, zero Pending pods after
> lowering worker CPU requests 2→1 and pre-scaling the `workers` pool). Under the burst
> the AGC agent-pool cannot recycle consumed runners (GitHub `422 "Runner … is currently
> running a job and cannot be deleted"`), so online listeners are not replenished, GitHub
> dispatches ~1 job at a time, and the queued jobs hit GitHub's ~15-min unstarted-job
> timeout (cancelled) while a stuck job's token is invalidated (`RenewJob 401 "Not
> authorized for this job"` → 600s death). Root cause is an **AGC concurrency /
> agent-pool recycling issue under burst load** (Q247/Q249/Q254 family) — **not** node
> capacity (Q248) and **not** a Q242/Q246 defect — tracked as **Q259**. One earlier run
> also hit a transient **spot-VM preemption** (the `workers` pool is spot). Evidence:
> runs `28513106734` (unit-test.yml) and `28510907609` (integration-test.yml). Until
> Q259 is fixed, Q224's "route production CI green" is **not** met, so Q224 and Q242 stay
> open.
>
> **Q259 root cause + fix (code-fixed 2026-07-01; live re-validation pending).** Traced
> end-to-end: after a single-use JIT runner completes a job, GitHub auto-removes the
> ephemeral record but for a few to tens of seconds still answers a delete with `422
> "Runner … is currently running a job and cannot be deleted"`, and — because the AGC
> re-registers under a **stable name** (Q114) — the lingering record makes the
> re-registration `409`. `Pool.Recycle`'s 409-resolution deregister then hit the same
> `422` and returned it as a **fatal** error, so the post-job recycle failed, the
> listener goroutine exited, and the Multiplexer does **not** restart a non-permanent
> replacement — every completed job permanently dropped a polling slot until only the
> permanent baseline remained, collapsing GitHub dispatch to ~1 online runner. Under a
> burst all agents hit this window at once. **Fix (`cmd/agc/internal/agentpool`):** a
> typed `RunnerBusyError` for the transient `422`, and a **bounded, jittered backoff**
> in `Pool.Recycle` that waits for GitHub to release the just-consumed runner before
> re-registering (ctx-cancellable; on give-up the existing
> `actions_gateway_agent_recycle_errors_total` fires). Q114 single-use + stable-name and
> secure-by-default are preserved (`generate-jitconfig` 409s *before* minting, so retries
> orphan nothing). Unit + listener-suite regression tests cover the retry-through and
> bounded-give-up paths. **The live symptom only reproduced under real burst, so
> end-to-end confirmation is deferred to the next dogfood turn-up — Q224/Q242 are NOT
> yet unblocked/closed.**
>
> **Q259 fix live-validated present, but concurrent matrix STILL wedges — new root
> cause Q260 (2026-07-03).** The next turn-up deployed the Q259 fix to the AGC
> (image `ghcr.io/actions-gateway/agc:e2e-2310a31`, built from `main`@`2310a31` = #500;
> GMC/proxy/wrapper stayed `v1.1.0-rc.6`) and re-routed the matrix. The Q259 fix **is**
> present and behaves as designed — the recycle path now logs `agentpool: recycle of
> parked consumed agent failed; will retry next reconcile` (the bounded retry) instead
> of a fatal listener exit. Individual jobs still run **green** (`lint` completed
> `success` on `gag-ci`). **But the concurrent burst does not go green — it wedges the
> same way, and the dominant cause is NOT the post-job recycle Q259 fixed.** Reproduced
> across **two independent bursts** (a 7-job unit-test+integration burst, then a 6-job
> unit-test burst against an already-warm pool — so not a cold-start artifact): at burst
> start **multiple listener agents acquire and try to provision the *identical* job**.
> The AGC logs 5 distinct sessions (`agentIndex` 1–5, 5 different `sessionId`s) all
> failing `provisioner: create Secret job-<jobid>-<suffix>: secrets "…" already exists`
> for the **same** worker Secret name (e.g. `job-d03513f7-aa20-416c-a037-197a4a4c9d06-980b169`).
> One agent wins and runs the job; the other 4–5 burn runner slots (GitHub shows them
> `busy` but `offline`, no worker pod) and their sessions die. Net effect: **only ~1
> worker pod ever runs**, the remaining jobs are stranded `in_progress` (assigned to the
> now-dead duplicate runners) until GitHub's ~15-min unstarted-job timeout, and the pool
> collapses to `activeSessions=1` (baseline listener). The Q259 `422 "…still running a
> job and cannot be deleted"` recycle churn is also still present (e.g. `runner id 1828`),
> but it is now the *secondary* symptom — the primary wedge is **duplicate job
> acquisition under burst** (multiple `AcquireJob`/provision on one job message),
> distinct from the post-job recycle Q259 addressed. **Not capacity:** worker nodes were
> pre-scaled (`workers` pool → 3 spot `e2-standard-4`; the `SSD_TOTAL_GB` regional quota
> of 500 GB caps pre-scaling at ~3–4 workers — see Q248), zero Pending worker pods from
> capacity. Tracked as **Q260**. **Q224's "route production CI green" is still not met, so
> Q224 and Q242 remain open/blocked.** Evidence: AGC logs (`agc:e2e-2310a31`), reruns of
> unit-test.yml `28671804298` + integration-test.yml `28671804300`, and unit-test.yml
> `28547170012` (2nd burst).
>
> **Q260 fix (code-complete 2026-07-03; live re-validation pending).** The AGC now
> deduplicates a job across the sibling listener sessions of one RunnerGroup
> **before** `AcquireJob`. The Multiplexer owns a per-group in-flight claim
> registry keyed by the job's `RunnerRequestID` (present in the broker message
> pre-acquire); `handleJob` claims the id before acquiring, and a sibling handed
> the same fan-out delivery finds it already claimed and **skips the acquire
> entirely** — so its runner stays online and idle instead of going `busy` but
> pod-less, and no two sessions ever reach the colliding per-job worker Secret.
> The claim is released when the job finishes (or the acquire is abandoned), so a
> later GitHub redelivery is still provisionable. A new counter
> `actions_gateway_jobs_duplicate_delivery_total{namespace,runner_group}` records
> each deduplicated delivery (steady low rate under bursts = the gate working).
> The dedup runs before the Q59 admission gate, so a duplicate costs neither a
> capacity slot nor an acquire. Regression tests: a single-listener gate test
> (`TestListener_DuplicateJobDeliverySkipsAcquire`) and a Multiplexer concurrency
> test (`TestMultiplexer_DuplicateJobDeliveryProvisionsOnce`) that fails without
> the fix (all 5 sibling sessions provision one job; peak-concurrent-provisions =
> 5) and passes with it (= 1). **The wedge only reproduces under a real burst, so
> end-to-end confirmation is deferred to the next dogfood turn-up — Q224/Q242 are
> NOT yet unblocked/closed.** The Q259 `422 "still running"` recycle churn is a
> separate, secondary symptom and is unaffected by this fix.
>
> **Q260 fix live-validated INEFFECTIVE — the concurrent matrix STILL wedges
> (2026-07-03, re-route #2).** A fresh AGC image built off `main`@`c850764` (=#503,
> the Q260 dedup fix) — `ghcr.io/actions-gateway/agc:e2e-c850764`
> (digest `sha256:989644a114e39f98108125a2ed4157aec8a8b4611abd68f6e84d747745efcc19`),
> GMC/proxy/wrapper unchanged at `v1.1.0-rc.6` — was deployed by patching the GMC's
> `AGC_IMAGE` env; the CI AGC rolled to it (verified: running pod's `imageID`
> matches the pushed digest), gateway `Ready=True`, baseline listener online. The
> concurrent matrix was re-routed (`GAG_RUNNER → ["self-hosted","linux","gag-ci"]`)
> and a 7-job burst fired (rerun of unit-test.yml `28678275088` = 6 jobs + rerun of
> integration-test.yml `28678275106` = 1 job). Worker capacity was **not** the
> constraint: `workers` pool pre-scaled to 3 `e2-standard-4`, worker CPU requests
> lowered `2→1`, zero Pending worker pods. **The burst wedged exactly as before.**
> At burst start `activeSessions` scaled up to 8 (`maxListeners`) as designed, but
> then **5 distinct sibling sessions** (`agentIndex` 2–6, 5 different `sessionId`s)
> all failed `provisioner: create Secret job-3e6a971f-62ec-4bba-bdd5-b928ba9e63f7-9a91092: secrets "…" already exists`
> on the **identical** worker Secret — i.e. 6 sessions raced the *same* job (1 won,
> 5 burned their runner slot). The pool then collapsed `activeSessions 8 → 2`; GitHub
> showed 5 runners `busy:true` but `status:offline` (ci-2…ci-6) with **no** worker
> pod; only ~1–2 worker pods ever ran; and `unit-test`/`vendor-check`/`tidy-check`/
> `coverage`/`lint` stranded `in_progress` on the dead runners. The Q259 `422 "…still
> running a job and cannot be deleted"` recycle churn (runner ids 1884/1886/1887) and
> the Q254 `RenewJob: job lock definitively lost` (`job_not_found` 404, cancelling the
> winning worker) both reappeared as secondary symptoms.
>
> **Root cause — the Q260 dedup keys on the wrong identifier.** The Multiplexer's
> in-flight claim registry keys on `RunnerRequestID` and claims it **pre-**`AcquireJob`
> ([`goroutine.go:570`](../../cmd/agc/internal/listener/goroutine.go)). But the
> colliding per-job worker Secret is named from the job's **`planID`**
> (`resp.Plan.PlanID`, from the AcquireJob **response**), and the pre-acquire broker
> message ([`RunnerJobRequestBody`](../../broker/types.go), fields
> `RunnerRequestID` + `RunServiceURL` + `BillingOwnerID`) carries **no** plan id.
> GitHub's broker fan-out delivers one job (one `planID`) to sibling sessions as
> messages with **distinct** `RunnerRequestID`s — so each sibling's
> `claimJob(distinctReqID)` succeeds, all pass the gate, all acquire, and all collide
> on the shared `planID` Secret. Since the claim registry is shared across siblings
> (`cfg.ClaimJob = m.claimJob`), 5 sessions passing the gate proves their
> `RunnerRequestID`s differed; and `RunnerRequestID` is non-empty (RenewJob keys on it
> and single jobs renew fine), ruling out the empty-key path. The fix's model —
> "same delivery ⇒ same `RunnerRequestID`" — does not hold in production; the
> regression test `TestMultiplexer_DuplicateJobDeliveryProvisionsOnce` feeds one
> **shared** `RunnerRequestID` to all 5 sessions, so it passes green while the live
> broker assigns per-delivery ids and the wedge survives. **A working dedup must key
> on job identity that (a) collapses across fan-out siblings and (b) determines the
> Secret** — i.e. `planID`, which is only known *post*-acquire. Candidate fixes for
> the Q260 follow-up: claim on `planID` immediately after `AcquireJob` but before
> provisioning, releasing the acquire + deregistering the runner cleanly on a lost
> claim (so the slot isn't left `busy`/offline); or investigate whether the per-job
> `RunServiceURL` (documented "per-job … must not be cached globally across jobs") is
> stable across siblings and usable as the pre-acquire dedup key. **Q224's "route
> production CI green" is still NOT met — Q224 and Q242 remain open/blocked, and Q260
> is reopened (its first fix is ineffective).**
>
> **Q260 re-fix (code-complete 2026-07-03; live re-validation pending).** The dedup is
> re-keyed from `RunnerRequestID` to **`planID`** and moved from pre-`AcquireJob` to
> **post-acquire, pre-provision** ([`goroutine.go`](../../cmd/agc/internal/listener/goroutine.go)
> handleJob; the Multiplexer's shared claim registry now holds planIDs). Because planID
> is only known post-acquire, a losing sibling still acquires, then finds the planID
> already claimed and **skips provisioning**, returning `acquired=true` so its consumed
> single-use runner is recycled back online (slot reclaimed cleanly) — no collision on
> the `job-<planID>` Secret, no burned slot. The pre-acquire RunnerRequestID gate is
> **removed** (it never fired in production — siblings' ids differ — and the planID gate
> subsumes its only correct case, since any two deliveries that would collide on the
> Secret share a planID). `actions_gateway_jobs_duplicate_delivery_total` is retained,
> now counting a post-acquire planID-claim skip. Regression tests re-key onto the live
> shape (distinct RunnerRequestIDs, one shared planID) and were **verified to fail against
> the c850764 behaviour**: the reworked Multiplexer unit test
> (`TestMultiplexer_DuplicateJobDeliveryProvisionsOnce`, peak-provisions 1 vs 5), the
> single-listener gate test (`TestListener_DuplicateJobDeliverySkipsProvisioning`, now
> asserts the loser *does* acquire but does not provision and keys on `plan-stub`), and a
> new **envtest** integration test
> (`TestAGC_Q260_DuplicateDeliveryDedupsOnPlanID`) that drives the real provisioner +
> API server: one session wins and holds the planID claim while a distinct-RunnerRequestID
> sibling is deduped rather than hitting the real Secret `AlreadyExists`. **The wedge only
> reproduces under a real burst, so end-to-end confirmation is still deferred to the next
> dogfood turn-up — Q224/Q242 stay open/blocked and Q260 stays open until then.**
> Plan: [`q260-planid-dedup-refix.md`](q260-planid-dedup-refix.md).
>
> **Q260 planID dedup live-validated EFFECTIVE — the burst-start collapse does NOT recur;
> but the matrix still isn't fully green, now blocked by capacity + a late-redelivery edge
> (2026-07-04, re-route #3).** A fresh AGC image built off `main`@`1f4111b` (=#508, the
> planID re-fix) — `ghcr.io/actions-gateway/agc:e2e-1f4111b`
> (manifest-list digest `sha256:b0848e970e0fca62d0b649fa5620467580914d79e21e04c24ddcd16171be40dd`,
> amd64 manifest `sha256:03bc3ee2…`), GMC/proxy/wrapper unchanged at `v1.1.0-rc.6` — was
> deployed by patching the GMC's `AGC_IMAGE` env; the CI AGC rolled to it (verified: the
> running pod's `imageID` matches the pushed digest), gateway `Ready=True`, baseline listener
> online. The dogfood `RunnerTemplate` was re-pinned to the build-capable
> `ghcr.io/actions-gateway/dogfood-runner:2.335.1` (Q239, avoids `make: command not found`),
> worker CPU request already `1`, `default-pool→2`, `workers` pre-scaled `→3`, and
> `spec.logLevel: debug` set on the CR so the dedup skip (a Debug line) is observable. The
> SAME comparable 7-job burst as #505 was fired at `23:45:33Z` — reruns of the two completed
> `main` runs **on the exact deployed commit `1f4111b`**: unit-test.yml `28687585802` (6
> gag-ci jobs) + integration-test.yml `28687585839` (1 job) — after flipping
> `GAG_RUNNER → ["self-hosted","linux","gag-ci"]`.
>
> **The Q260-specific wedge is gone.** The prior turn-ups' signature — 5 sibling sessions
> **simultaneously** colliding on the shared `job-<planID>` **Secret** at burst start →
> instant collapse to `activeSessions 1-2` → nothing completes — did **not** occur:
> - **Dedup fired on the shared planID:** `duplicate_delivery` gate skipped **2** deliveries,
>   both for planID `b8321da3` (agentIndex 1 @`23:47:45` and agentIndex 3 @`23:51:28`, with
>   **distinct** `RunnerRequestID`s) — exactly the fan-out the old pre-acquire RunnerRequestID
>   key missed. The planID key collapses it, as designed.
> - **Zero Secret collisions at burst start** (was 5): 5 `job-<planID>` Secrets created cleanly;
>   `activeSessions` scaled up to **4 concurrent busy runners** (ci-0..3, = `maxWorkers`),
>   holding rather than collapsing.
> - **Two full CI jobs completed GREEN under the burst** — `coverage` and `integration-test`
>   both `success` on `gag-ci` (a first for these turn-ups; prior wedges completed ~nothing).
>
> **But the concurrent matrix still does NOT go fully green.** Final tally: **2 success**
> (coverage, integration-test), **2 cancelled** (tidy-check, vendor-check), **1 failure**
> (unit-test), 2 (shellcheck, lint) still in-progress at teardown. The residual blockers are
> **distinct from the Q260 dedup wedge**:
> 1. **Capacity starvation (Q248) — dominant.** The pre-scaled 3 spot `workers` nodes were
>    **preempted** mid-burst down to **1** node (spot; node set `cd55/hsms/l5w6 → gwzz`,
>    autoscaler reported "1 in backoff after failed scale-up"; `SSD_TOTAL_GB=500` caps the pool
>    at ~3-4 regardless). With ~1 concurrent worker slot the 7 jobs serialized: `unit-test`
>    died at **exactly 600s** (the initial AcquireJob lock TTL) having run **zero steps**
>    because its assigned runner never got a pod; `tidy-check`/`vendor-check` were cancelled at
>    **exactly 15:00** (GitHub's unstarted-job timeout). This is capacity, not Q260.
> 2. **Late-redelivery Pod-collision (new Q260 follow-up).** The **2** collisions that *did*
>    occur were on **`create Pod`** (not Secret) and **late** (`23:59:18`/`:19`), both for the
>    single **slow** planID `b8321da3`: GitHub redelivered that one job repeatedly over ~12 min
>    (`23:47`→`23:59`); the planID claim is released when the winner completes, so a
>    post-completion redelivery passes the gate and collides on the winner's **not-yet-GC'd
>    Completed pod**. That winner pod **ran the job to completion** ("Raising job completed
>    against run service" / "Job completed" @`23:59:14`, **no** renewal/lock errors) — yet
>    GitHub still **cancelled** `tidy-check` at the 15-min unstarted-timeout, a
>    completion-vs-timeout **accounting gap** under fan-out. Milder than the Secret-collapse
>    (the job already ran) but it still burns runner slots and yields a cancelled job.
> The Q259 `422 "…still running a job and cannot be deleted"` recycle churn (4 listener
> exit-on-recycle events) is present as before — unchanged secondary symptom.
>
> **Verdict.** The planID stable-key model is **correct** — the dedup fired on the shared
> planID and prevented the burst-start Secret-collision collapse — so do **not** hunt for a
> different dedup key. But **Q224's "route production CI green" is still NOT met:** full green
> is blocked by (1) Q248 spot-node capacity → serialized execution → 600s/15-min timeouts, and
> (2) the late-redelivery Pod-collision + completion-accounting residual. **Q224/Q260/Q242
> stay open/blocked.** A clean green re-validation needs **stable worker capacity** (non-spot,
> or ≥3 held nodes) so throughput isn't the confound, plus addressing the redelivery-accounting
> edge (release the planID claim only after the worker Pod is GC'd, and/or reconcile GitHub's
> per-delivery job-assignment timeout with the AGC's dedup-to-one-delivery model). Evidence:
> AGC debug logs (`agc:e2e-1f4111b`), reruns unit-test.yml `28687585802` + integration-test.yml
> `28687585839` (both on `sha 1f4111b`), burst `23:45:33Z`–`00:03Z`.
>
> **Q260 redelivery residual — code-complete (2026-07-03; awaits combined re-route #4).**
> The late-redelivery **Pod-collision** from residual (2) is now **fixed in code**: the AGC
> Multiplexer's shared `planID` claim registry **retains** a released claim for a linger window
> sized to the owner's `completedPodTTL` (the exact window the winner's terminal pod lingers
> before the reaper GCs it), instead of freeing it on completion. A post-completion redelivery
> arriving during that window is deduped at the post-acquire `planID` gate — counted on
> `actions_gateway_jobs_duplicate_delivery_total`, no re-provision, **no `create Pod … already
> exists`**, no error surfaced as a cancel. Regression:
> `TestAGC_Q260_LateRedeliveryAfterCompletionDedups` (envtest) reproduces the exact
> `create Pod runner-…-<planid>: pods "…" already exists` against the pre-fix behavior and
> passes with the fix. The deeper **completion-vs-15-min-cancel accounting gap** (the winner's
> pod completes yet GitHub cancels the job on a deduped sibling delivery) now has its run-service
> protocol call — `broker.CompleteJob` + a guarded loser-abandon path, described next — but it
> stays **off by default** pending live confirmation of the completion semantics. See
> [`q260-planid-dedup-refix.md`](q260-planid-dedup-refix.md) Follow-up item 2. This code lands
> ahead of the dispatcher's combined **capacity (Q248) + re-route #4** turn-up, which
> re-validates green on stable worker capacity. **Q224/Q260/Q242 stay open until then.**
>
> **Follow-up mechanism landed (guarded), pending this turn-up's confirmation.** The
> completion-accounting residual now has a code path: the deduped loser can release its
> acquired-but-unrun assignment via `completejob` on its own `jobID` (result `skipped`), so
> GitHub does not cancel the job at the 15-min unstarted-timeout. It is **off by default**
> (`AGC_COMPLETE_ABANDONED_DELIVERIES=true`) because the run service's per-delivery *completion*
> semantics are not yet live-confirmed. **Next turn-up: enable the flag via the existing
> `AGC_EXTRA_*` passthrough — set `AGC_EXTRA_AGC_COMPLETE_ABANDONED_DELIVERIES=true` on the GMC
> pod (GMC run with `--allow-agc-extra-env`), which the GMC forwards verbatim (prefix-stripped)
> to the AGC Deployment env; no GMC code change needed.** Then re-fire the burst on stable
> (non-spot) capacity and capture the `completejob` request/response + whether the
> previously-cancelled job (`tidy-check`) now concludes instead of cancelling.
> If completion turns out to be planID-scoped (would cancel the winner), revert the flag and
> pursue the claim-release-post-GC path instead. See
> [`q260-planid-dedup-refix.md`](q260-planid-dedup-refix.md) follow-up item 2.
>
> **Combined capacity fix + flag-on/flag-off comparison — capacity & collisions
> FIXED, but still NOT green; the blocker is now GitHub's broker fan-out
> completion/assignment accounting (2026-07-04, re-route #4).** Ran the same ~7-job
> concurrent matrix twice on **stable, non-preemptible** worker capacity with a
> fresh AGC built off `main`@`4602429` (= HEAD, includes #512 late-redelivery claim
> linger and #513 guarded completejob-abandon) — `ghcr.io/actions-gateway/agc:e2e-4602429`
> (amd64 digest `sha256:55a88007…`), GMC/proxy/wrapper `v1.1.0-rc.6`, `spec.logLevel:
> debug`, `RunnerTemplate` pinned to `dogfood-runner:2.335.1` (Q239), worker CPU
> request `1`. The AGC pod's `imageID` matched the pushed digest; gateway `Ready=True`.
>
> **Capacity (Q248 residual) — FIXED.** Replaced the spot `workers` pool (which
> preempted `3 → 1` mid-burst in #3) with a **non-preemptible `workers-od` pool**
> (`e2-standard-4 ×3`, on-demand, taint `dedicated=workers:NoSchedule`; spot `workers`
> scaled to `0`, `default-pool → 2`). SSD math: `3×100 + 2×50 + 20 = 420 GB < 500`
> (`SSD_TOTAL_GB` quota; disks are `pd-balanced`, which counts against it). Result:
> **3 `workers-od` nodes stayed Ready across all 58 monitor samples — zero preemption,
> zero spot nodes**; peak node utilization **34 % CPU / 27 % mem**; peak per-pod
> memory **~3.8 GiB** (under the 8 GiB limit, no OOM); peak `activeSessions` **5**.
> So the #3 failure mode (preemption → serialized jobs → 600 s lock-TTL / 15-min
> unstarted cancels *from capacity starvation*) **did not recur**. `workers-od` fixed
> at `min=max=3` to stay under quota; 4 concurrent worker pods fit comfortably at CPU
> request `1`.
>
> **#512 dedup — FIXED (again), 0 collisions in both bursts.** Each burst fanned out
> 3 planIDs with **5 sibling redeliveries each** (distinct `RunnerRequestID`s); the
> post-acquire planID gate deduped all of them (**10 dedup events per burst**) with
> **zero `create Secret … already exists` and zero `create Pod … already exists`**.
> The planID key and the claim-linger are working as designed.
>
> **Burst #4a — flag ON** (`AGC_COMPLETE_ABANDONED_DELIVERIES=true`, forwarded via the
> GMC `AGC_EXTRA_*` passthrough with `--allow-agc-extra-env`): reruns
> `28694212343` (6 gag-ci jobs) + `28694212356` (integration) at `04:11:48Z`. The
> #513 path was **exercised**: **15 `completejob` calls — 14 returned OK, 1 returned
> `401 "Not authorized for this job"`** (planID `eba8f94d`; its winner pod kept
> running, so the 401 did **not** finalize the winner — a per-delivery auth edge, not
> the feared planID-scoped regression). **Outcome: 2/7 green** (`coverage` on ci-1,
> `integration-test`); **5/7 wedged INDEFINITELY `in_progress`** (`tidy-check`,
> `shellcheck`, `unit-test`, `vendor-check`, `lint`) under the replacement session
> `ci-2`. Their winner pods **ran** (6 Succeeded, 1 Failed) — yet GitHub never
> concluded the jobs (confirmed via the Checks API, not just the runs API, whose
> aggregate froze at a stale `completed/success` when `coverage` finished). **The
> `completejob(result=skipped)` call returns OK but does not transition the job to
> completed** — it merely acks that one delivery, so a late redelivery re-assigns the
> already-run job to `ci-2` and GitHub holds it `in_progress`. Worse, by acking the
> delivery it **suppresses the 15-min unstarted-timeout that would otherwise resolve
> the job**, yielding an *indefinite* limbo.
>
> **Burst #4b — flag OFF** (control; the *only* difference from #4a is the completejob
> path): reruns `28693708850` + `28693708839` at `05:00:10Z`. AGC logs confirmed the
> clean control: **10 dedup events, 0 `completejob` calls, 0 collisions**. **Outcome:
> 1/7 green** (`integration-test`); `coverage`=**failure**, `unit-test`=**failure**,
> `vendor-check`=**cancelled**, `shellcheck`=**cancelled**, `lint`/`tidy-check`
> in_progress → cancel. Crucially these are **terminal** states, not the indefinite
> wedge. The Q259 recycle churn was present **and blocking**: GitHub's fan-out marks
> `ci-1/2/3` as *"runner … is still running a job and cannot be deleted"* (422), so the
> AGC cannot recycle those listener slots → the trivial jobs never get a slot and are
> cancelled at the 15-min unstarted-timeout; the jobs that *did* run concluded
> `failure` (the completion-accounting mismatch, same class as Q247 but at the
> assignment level — not a real test failure: identical commits pass on GitHub-hosted
> and `coverage` passed green in #4a).
>
> **Verdict.** Neither flag state reaches green — the same jobs go `in_progress`-forever
> (flag on) or `failure`/`cancelled` (flag off). **Capacity (Q248) and collisions
> (#512) are both fixed and off the critical path.** The remaining blocker is
> **GitHub's broker fan-out completion/assignment accounting**: one job is delivered to
> N sibling listener sessions as independent assignments, and neither the winner's
> completion nor the losers' `completejob(skipped)` reconciles GitHub's per-delivery
> view — so runners can't recycle (Q259 422) and jobs don't conclude as success. This
> is **distinct from and beyond** the Q260 dedup. **The #513 flag does not help and
> makes the end-state worse (indefinite `in_progress` vs terminal cancel/fail) — keep
> `AGC_COMPLETE_ABANDONED_DELIVERIES` OFF by default (secure-by-default confirmed by
> live evidence).** `completejob` semantics answered: the run service **accepts** the
> `skipped` result serialization (14/15 HTTP-OK, so wire format is fine) but does
> **not** conclude the job on that call; and 1/15 returned `401`, so job-scoped auth
> for the completion path is not reliable. **Q224/Q260/Q242 stay open**, now blocked on
> the fan-out accounting rather than capacity/collisions. Evidence: AGC debug logs
> (`agc:e2e-4602429`), flag-on reruns `28694212343`/`28694212356` (burst `04:11:48Z`),
> flag-off reruns `28693708850`/`28693708839` (burst `05:00:10Z`).
>
> **Re-route #5 — Q260 Option A CONFIRMED (GO); the fan-out accounting gap is closed
> AGC-side (2026-07-04).** Deployed a fresh `ghcr.io/actions-gateway/agc:e2e-238b8df`
> (amd64 digest `sha256:611632e7…`, includes #521 winner-driven Option A) via the GMC
> `AGC_IMAGE` env patch, on the same re-route #4 stable capacity (non-preemptible
> `workers-od` ×3 + default-pool 2, worker cpu req 1, 5 nodes Ready throughout). Enabled
> Option A with **`AGC_EXTRA_AGC_FANOUT_COMPLETION=true`** on the GMC pod (GMC v1.1.0-rc.6
> already run with `--allow-agc-extra-env`), which forwards `AGC_FANOUT_COMPLETION=true`
> to the AGC Deployment — no GMC code change. `spec.logLevel: debug`. RunnerTemplate was
> already pinned to `dogfood-runner:2.335.1` in the persisted CR (Q239 not regressed this
> time — the toolchain image was present). Fired the same ~7-job matrix (unit-test
> `28712011706` + integration `28712011697` reruns on sha `238b8df`, both green on
> GitHub-hosted; **push** events, so concurrency-immune).
>
> **The one live-only unknown is answered YES.** At `16:37:07Z` a fanned-out job (planID
> `357b6d9e`, winner on ci-0) whose winner completed **naturally** fanned `completejob`
> out to **both** deduped siblings (jobIDs `34ad8db4` on ci-2, `f968c752` on ci-4) →
> **both returned OK** (`completed a deduped sibling delivery via completejob`), **not**
> "already resolved". GitHub **accepts** the completion of a sibling delivery that never
> ran the job. Cumulative over the burst: **9 `completejob` OK, 0 failures, 2
> already-resolved** (siblings whose winners were concurrency-cancelled — see confound),
> across **13** deduped fan-out deliveries. Completion is **per-delivery, not
> planID-scoped**: `completejob` on a sibling's own job ID resolved only that assignment,
> and the winner's own delivery still carried the real workflow result reported by its
> runner binary — so the pod-phase proxy on siblings **cannot** green a red workflow. The
> secure-by-default concern is cleared; the flag is flipped **on by default**
> (`AGC_FANOUT_COMPLETION`, opt out `=false`).
>
> **Jobs conclude green and stay green.** `coverage` (16:37:04Z), `unit-test` (`-race`,
> 16:52:29Z) and `integration-test` all concluded **success** — the previously-wedged
> class. Crucially `coverage` stayed `success` **past `16:47Z`**, beyond the ~15-minute
> unstarted-timeout of its siblings (acquired ~16:31Z) — the exact point re-route #4's
> winner-completed jobs were cancelled. **Option A prevented the cancel.**
>
> **Q259 recycle 422 clears per job.** The "runner … is still running a job and cannot be
> deleted" churn (121 hits in the 6 min before the first winner completed) dropped ~12×
> once winners began fanning `completejob`; the AGC pool recovered from a collapsed
> **2 active sessions back to 5** and drained its backlog. The 422 is a **rolling
> transient** — each fanned-out job's in-flight siblings 422 until that job's winner
> completes and resolves them — not the permanent wedge of re-route #4.
>
> **Confound (handled).** A Dependabot rebase merge-train briefly shared the runner pool:
> its `pull_request` CI runs (SHAs `81b0d30`/`d160ae3`/…) were concurrency-cancelled on
> each rebase force-push, cancelling in ~4 min — **distinct** from the 15-min accounting
> timeout, and (because the delivery is torn down first) the reason 2 sibling
> `completejob`s hit "already resolved". Attempting to cancel the interfering runs was
> denied (shared workload). The clean signal came from the **push**-event `238b8df`
> reruns, which cannot be concurrency-cancelled. Their slower jobs (`vendor-check`,
> `lint`, `tidy-check`, `shellcheck`) ran long on a cold Athens cache and, 38 min in,
> were still **in_progress** — **not** the Q260 accounting cancel (which did not recur)
> but a **separate worker-capacity limit**: with `maxWorkers=4` saturated the AGC logged
> repeated `job admission rejected: worker capacity full` and those jobs cycled through
> redeliveries without ever landing a worker slot. So the ~7-job matrix did **not**
> cleanly sweep all-green in this window; a pristine full-matrix green is gated on
> worker-capacity tuning (`maxWorkers`, Q248), which is **distinct from and beyond** the
> Q260 accounting fix confirmed here.
>
> **Verdict: GO (design §5 point 4).** Resolving all sibling deliveries lets a fanned-out
> job conclude green (3/3 that landed workers concluded — `coverage`, `unit-test`
> `-race`, `integration-test`), `completejob` on live siblings returns OK (9/9, 0
> failures), the job survives past the 15-minute timeout, and the Q259 422 clears per
> job. The many-acquirers topology is reconcilable AGC-side; **Option E (Q264) is not
> needed** and is demoted. **Q260 DONE; Q224's fan-out blocker cleared** (residual: the
> `maxWorkers` capacity sweep, Q248). Evidence: AGC debug logs (`agc:e2e-238b8df`),
> reruns `28712011706`/`28712011697` (burst `16:24:00Z`), fan-out completion `16:37:07Z`.
>
> **Secondary observation — dogfood RunnerTemplate reverted to the bare upstream
> image (Q239 regression).** The `shellcheck` job failed `make: command not found`
> because the CI `RunnerTemplate` runner container is image-less, so the AGC gap-fills
> the bare upstream `ghcr.io/actions/actions-runner:2.335.1` (no build toolchain)
> rather than the build-capable `dogfood-runner` image (Q239, validated 2026-06-29).
> This blocks green CI independently of Q260: a future turn-up must run
> `scripts/dogfood/setup.sh` with `DOGFOOD_RUNNER_IMAGE` exported (or patch the
> `RunnerTemplate` `workerImage`) so `make`-based jobs can pass. Not a new bug — the
> cluster lost the Q239 config across a re-setup.
>
> **Operational note (2026-07-03):** the `gag-dogfood-e2e` tenant (Part F Kata e2e)
> keeps its own `dogfood-e2e-agc` pod (~500m CPU) running whenever the system pool is up,
> which does not fit alongside the CI AGC + GMC + Athens on a single `e2-standard-2`
> system node — the CI AGC stays `Pending` (`Insufficient cpu`). Turn-ups that only
> need the CI matrix should scale `default-pool` to **2** nodes (done here) or suspend
> the e2e tenant; the `SSD_TOTAL_GB=500` quota then bounds the `workers` pool to ~3.
>
> **Build-capable runner image (Q239).** The bare upstream `actions-runner` has no
> build toolchain (`make`, a C compiler), so this repo's `make`-based jobs fail
> `exit 127: make: command not found` on it — the workflows assume `make` is
> preinstalled, as on GitHub-hosted `ubuntu-latest`. The fix is a build-capable
> `workerImage`: [`scripts/dogfood/runner/Dockerfile`](../../scripts/dogfood/runner/Dockerfile)
> adds `build-essential` (+ `curl`/`xz`/`sudo` for the shellcheck job's pinned-binary
> self-install) on top of the pinned upstream runner. Build and push it with
> [`scripts/dogfood/runner-build.sh`](../../scripts/dogfood/runner-build.sh), then
> export `DOGFOOD_RUNNER_IMAGE=ghcr.io/actions-gateway/dogfood-runner:<tag>` before
> running `scripts/dogfood/setup.sh` — the `RunnerTemplate` pins it and the AGC still
> injects the Q235 wrapper on top. **Validated `2026-06-29`:** the `shellcheck` job,
> which failed `make: command not found` on the bare image, ran green on
> `dogfood-runner:2.335.1` with the wrapper injected (`make` 4.3, `gcc` 13.3.0).
>
> **Release-asset egress is already allowlisted (Q246 — misdiagnosis).** GitHub
> *release-asset* downloads (the `shellcheck` tarball, `setup-go`'s Go toolchain)
> 302-redirect from `github.com` to `objects.githubusercontent.com` →
> `185.199.108.0/22`. That is GitHub-dedicated space (not shared Fastly), and the
> worker egress `NetworkPolicy` **already permits it**: the GMC IP-range feed
> merges GitHub `/meta`'s `api`+`actions`+`web` keys, and the `web` range
> contains `185.199.108.0/22`
> ([`ipranges.go`](../../cmd/gmc/internal/controller/ipranges.go)). So Q246's
> original "workers can't reach the CDN, add it to the egress allowlist" premise
> is wrong — do **not** widen the allowlist or bake the asset into the runner
> image.
>
> **Q246 root cause — the Q61 cold-start cache race (confirmed live, fixed).** A
> cold live run on `gag-dogfood` (`2026-07-01`, direct-egress gateway) settled it —
> full evidence in
> [`archive/q246-release-asset-timeout-live-diagnosis.md`](archive/q246-release-asset-timeout-live-diagnosis.md).
> Four live observations: (1) a workload-labelled pod downloads the shellcheck
> release asset over the 302→`objects.githubusercontent.com`→`185.199.108.133` hop
> in **0.32 s (HTTP 200)** when the NP is programmed — egress is not the problem;
> (2) scaling the system pool 0→1 forced a fresh GMC and the `dogfood-workload` NP
> **dropped from 7337 CIDRs to 1 for ~25 s** — the per-CR reconcile
> (`ActionsGatewayV2Reconciler.applyNetworkPolicy`, a `CreateOrPatch` that
> overwrites `Spec.Egress` wholesale) rebuilt the policy from the still-empty
> IP-range cache before `IPRangeReconciler`'s first `/meta` fetch landed and
> repatched; (3) with the GitHub rule absent the identical download **times out
> (`curl` rc=28)** — the exact Q246 symptom — and returns to HTTP 200 once
> restored; (4) a *warm* GMC restart did **not** blank (the fetch won the race), so
> the window's width scales with GMC-startup + fetch latency, which the Q247 node
> CPU exhaustion lengthens (and can re-trigger by restarting GMC mid-run). So the
> cause is **(a) the Q61 race**; **(b) Q247 CPU is only an amplifier** (already
> fixed; node right-sizing is Q248). Egress succeeds regardless of CPU whenever the
> NP carries the rule. **Fix:** the per-CR reconcile now **preserves an existing
> direct-egress NP's allowlist while the cache is empty** instead of blanking it (a
> not-yet-created NP stays fail-closed) — so no GMC restart, under any load, strips
> a live worker's or the AGC's GitHub egress. Secure-by-default preserved (egress
> is never widened). Regression tests in `actionsgateway_v2_test.go`.
>
> **Q247 root cause — RenewJob used the wrong jobId (fixed).** Every job routed
> to a GAG runner failed at the *job-lifecycle* level: the worker ran the full
> job, then `JobRunner.CompleteJobAsync` threw
> `TaskOrchestrationJobNotFoundException` ("workflow instance not found"), the
> run showed `conclusion: failure` with no failed step and no logs, and multiple
> worker pods appeared for one job. Root cause: the AGC's per-job renewal loop
> ([`goroutine.go`](../../cmd/agc/internal/listener/goroutine.go), `handleJob`)
> sent the broker envelope's numeric `MessageID` as the run-service `jobId`
> instead of the job's `RunnerRequestID` — the value `AcquireJob` already sends
> as `jobMessageId` and the value the run service keys `/renewjob` on. The run
> service does not recognize the envelope id, so `RenewJob` never renewed the
> lock; the error was swallowed as non-fatal and the worker kept running. On any
> job that outlived GitHub's lock TTL, GitHub recycled the job and redelivered it
> to a **sibling** session — a duplicate worker pod — while the original ran to
> completion and orphaned at CompleteJob. Short jobs finished before the TTL
> lapsed, which is why only the long (~10 min) e2e job exposed it deterministically
> and the general non-e2e jobs hit it intermittently (the "stuck at N active
> sessions after recycling" symptom in Q247). The one-line fix (renew by
> `RunnerRequestID`) plus a full-`Run` regression test landed in the AGC listener;
> a green dogfood e2e on GAG (PR #476's branch) is the live confirmation. This was
> **not** the DinD/config/egress/CPU issue the co-occurring node exhaustion
> suggested — it reproduces in isolation against the broker HTTP stub.
>
> **Q247 residual — an unbounded renewal call wedges the loop (fixed).** After the
> jobId fix, a live dogfood run still failed at *exactly* the ~10-minute mark
> (job started 03:21:27Z, GitHub marked it `failure` at 03:31:27Z = 600s) with a
> single worker pod that ran well past the cutoff. The job died at the *initial*
> AcquireJob lock TTL, meaning no renewal ever landed even with the correct jobId.
> Cause: the per-job renewal loop (`StartRenewLoop`) ran each `RenewJob` call
> inline with **no per-call timeout**, unlike `AcquireJob`/`createSession`, which
> bound every call with `controlPlaneTimeout`. Under the e2e's node CPU/egress
> saturation a single `/renewjob` call black-holes (TCP accepted, no response), and
> because the next tick cannot fire until the call returns, that one wedged call
> starves *every* subsequent renewal until the lock lapses at 600s. Fix: bound each
> renewal call with the same `controlPlaneTimeout` (30s) — a hung call now aborts
> (counted as the existing non-fatal `renew_job_errors_total`) and the loop issues
> the next renewal on schedule, so one slow renewal costs one renewal, not all of
> them. Regression test asserts a second renewal fires while the first is still
> hung (impossible if the loop is wedged). This is the co-occurring node-exhaustion
> interaction the original Q247 note flagged, now closed as a code fix rather than
> a capacity workaround.
>
> **Q247 auth — RenewJob used the wrong token (fixed).** With both prior fixes
> live (`agc:q247-3edc85e`), the renewal loop fired correctly but *every* `RenewJob`
> was rejected by GitHub with `401 {"source":"actions-run-service","errorMessage":
> "Not authorized for this job"}`, repeating every ~40s for both agent indices, and
> long jobs again failed at *exactly* 600s. Root cause: `RenewJob` authenticated
> with the broker session (OAuth) token — the same token used for `CreateSession`/
> `GetMessage`/`AcquireJob` — but the run service authorizes *per-job* lock renewal
> only with the job-scoped token it issues in the `acquirejob` response (the
> `SystemVssConnection` endpoint's `AccessToken`). It accepts the session token to
> *claim* a job but rejects it to *renew* one, which is why acquire succeeded and
> every renewal 401'd from the first call (ruling out token expiry). This mirrors
> the real runner, which renews via a `RunServer` connection built from the message's
> `SystemVssConnection` endpoint (`VssUtil.GetVssCredential`), not the listener
> OAuth token. Fix: `AcquireJob` now parses the endpoint token
> (`AcquireJobResponse.JobAuthToken`) and the listener threads it into every
> `RenewJob` call as the `Authorization` bearer (`RenewJobRequest.AuthToken`),
> falling back to the session token only when absent. Merge gate: a full-`Run`
> listener test drives a simulated >10-minute job whose renew endpoint 401s on any
> non-job token and asserts every renewal is authorized; the fakegithub broker and
> the broker-compat suite (new contract C16) model the same auth. The remaining
> defense-in-depth gap — tearing down the worker when a lock is *definitively* lost
> after sustained renewal failure — is tracked as Q254.
>
> **`vendor-check` / `tidy-check` unblocked by Athens (Q244, implemented).** An
> Athens in-cluster Go module proxy (`deploy/athens/`, applied by `dogfood/setup.sh`)
> caches Go modules so workers never need to reach `proxy.golang.org` directly.
> Athens pods (app=athens) are not covered by the workload NetworkPolicy and have
> free egress; workers reach Athens via an additive NetworkPolicy (port 3000) and
> are wired with `GOPROXY=http://go-module-proxy.gag-dogfood.svc.cluster.local:3000,off`
> plus `GONOSUMDB=*` in the RunnerTemplate.
>
> **Background (for reference):** GKE Dataplane V2's *managed* Cilium does not
> expose the `cilium.io/v2 CiliumNetworkPolicy` CRD (dropped since GKE
> 1.21.5-gke.1300), so an `EgressProxy` with `egressPolicyMode: CiliumFQDN` goes
> `Degraded` (`no matches for kind "CiliumNetworkPolicy"`, verified 2026-06-29).
> `destinationCIDRs` is no substitute for `proxy.golang.org`/`sum.golang.org`
> (Google-fronted ⇒ a CIDR allowlist opens all of Google's frontend). The FQDN
> intent/mechanism split (Q245) remains open. Detail + provider matrix:
> [Q242 plan § Provider FQDN-egress fragmentation](q242-g1-proxy-destination-allowlist.md#provider-fqdn-egress-fragmentation-post-implementation-finding).

---

## Part C — One-time GitHub setup

### C1. Confirm App installation + get installation ID

Ensure `actions-gateway-test` is installed on the org:
- GitHub.com → `actions-gateway` org → Settings → GitHub Apps →
  `actions-gateway-test` → Configure
- Confirm repository access is "All repositories" (or that
  `actions-gateway/github-actions-gateway` is explicitly listed)

Get the installation ID. The `/user/installations` endpoint requires a
GitHub-App-authorized token (the `gh` CLI's token returns HTTP 403), so use
the org-scoped endpoint instead — it works for an org owner:

```bash
gh api /orgs/actions-gateway/installations \
  --jq '.installations[] | select(.app_id == 3752347) | {id, account: .account.login}'
```

Set `INSTALLATION_ID` to the `id` value and re-run the secret creation in
B5 if you had a placeholder. As of this writing the install is `135739122`.

### C2. Workflow changes

Change `runs-on: ubuntu-latest` to the variable-driven expression in these
jobs (leave all `changes` jobs untouched):

**`.github/workflows/unit-test.yml`** — jobs `lint`, `shellcheck`,
`vendor-check`, `tidy-check`, `unit-test`, `coverage`:

```yaml
runs-on: ${{ fromJSON(vars.GAG_RUNNER || '"ubuntu-latest"') }}
```

**`.github/workflows/integration-test.yml`** — job `integration-test`:

```yaml
runs-on: ${{ fromJSON(vars.GAG_RUNNER || '"ubuntu-latest"') }}
```

When `GAG_RUNNER` is unset or `"ubuntu-latest"`, `fromJSON` returns the
string `ubuntu-latest` and jobs run on GitHub-hosted runners as before.
When `GAG_RUNNER` is `["self-hosted","linux","gag-ci"]`, `fromJSON` returns
the array and jobs route to GAG.

### C3. Set default variable (cluster off)

```bash
gh variable set GAG_RUNNER \
  --body '"ubuntu-latest"' \
  --repo "$REPO"
```

Commit and push the workflow changes. Because the variable defaults to
`ubuntu-latest`, CI is unaffected until you flip it in Part D.

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

# 3. Route CI jobs to GAG
gh variable set GAG_RUNNER \
  --body '["self-hosted","linux","gag-ci"]' \
  --repo "$REPO"
```

### Stop dogfooding

```bash
# 1. Route CI jobs back to GitHub-hosted (do this first — in-flight jobs
#    running on GAG will be cancelled when nodes are removed)
gh variable set GAG_RUNNER \
  --body '"ubuntu-latest"' \
  --repo "$REPO"

# 2. Scale system pool to 0 (GAG goes offline)
gcloud container clusters resize "$CLUSTER" \
  --node-pool=default-pool --num-nodes=0 --zone="$ZONE" --quiet

# Worker nodes drain and autoscale to 0 automatically within ~10 min.
```

---

## Part E — Teardown

```bash
# Delete cluster (stops all compute billing immediately)
gcloud container clusters delete "$CLUSTER" --zone="$ZONE" --quiet

# Optionally delete the project (irreversible — removes all GCP resources)
gcloud projects delete "$PROJECT"
```

---

## Part F — E2e on GKE (Kata Containers)

The e2e suite runs `kind create cluster` inside the runner pod, which requires
a Docker daemon (Docker-in-Docker). The clean solution is
[Kata Containers](https://katacontainers.io/): each pod gets its own
lightweight microVM with a real Linux kernel (backed by KVM). Inside the
microVM, Docker runs normally — no user-namespace tricks, no kernel feature
gaps — so kind works exactly as it does on a GitHub-hosted runner.

The security profile stays **`baseline`**: the pod itself does not need
`privileged: true` because the Kata microVM is the isolation boundary. If
anything escapes from within kind, it hits the microVM's kernel, not the GKE
node.

**What GKE provides:** Standard clusters support nested VMs via
`--enable-nested-virtualization` on a node pool, which exposes `/dev/kvm` on
the node. Kata uses `/dev/kvm` to spin up microVMs.
[Official GKE docs.](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/nested-virtualization)

**Machine type note:** nested virtualization on GCP requires N1, N2, or C2
instance families. E2 (used in Parts A–B) does **not** support it. The e2e
pool uses `n2-standard-4`.

### F1. Run the one-time setup script

```bash
export CLUSTER ZONE REPO APP_ID INSTALLATION_ID   # from the Variables section
scripts/dogfood/e2e-setup.sh
```

This script:
1. Creates the `e2e` node pool (n2-standard-4 spot, nested virt, autoscaling 0→2, taint `dedicated=e2e:NoSchedule`)
2. Installs the Kata DaemonSet, scoped to e2e pool nodes only (the system and workers pools use COS; Kata requires Ubuntu or COS 1.28.4+, and the DaemonSet labels nodes `katacontainers.io/kata-runtime=true` after install)
3. Creates the `kata-qemu` RuntimeClass with a node scheduling rule that prevents Kata pods from scheduling before the DaemonSet has finished installing
4. Creates the `gag-dogfood-e2e` namespace (baseline PSA), GitHub App Secret, ResourceQuota, and `ActionsGateway` CR with a `docker:dind` sidecar and `runtimeClassName: kata-qemu`

The DinD sidecar runs `dockerd` on `tcp://localhost:2375` (no TLS — pod-internal only). The `runner` container sets `DOCKER_HOST=tcp://localhost:2375`. Because all containers in a pod share a network namespace, kind's API server is reachable at `localhost:<apiserver-port>` from the runner.

### F2. Workflow change

In **`.github/workflows/e2e-reusable.yml`**, change line 28:

```yaml
# Before
runs-on: ubuntu-latest

# After
runs-on: ${{ fromJSON(vars.GAG_E2E_RUNNER || '"ubuntu-latest"') }}
```

Both `e2e-test.yml` (kindnet) and `e2e-calico.yml` (Calico) call this
reusable workflow, so one line change covers both CNI variants.

Set the default variable (e2e off, cluster not yet deployed):

```bash
gh variable set GAG_E2E_RUNNER --body '"ubuntu-latest"' --repo "$REPO"
```

Commit and push the workflow change. CI is unaffected until you flip the
variable.

### F3. E2e operations

```bash
# Enable (requires system pool to be up via dogfood/start.sh first)
scripts/dogfood/e2e-start.sh

# Disable (e2e pool autoscales to 0 once in-flight jobs finish, ~10 min)
scripts/dogfood/e2e-stop.sh
```

The e2e pool toggles independently from the CI pool — you can run only one
or both at the same time.

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

## Operations quick-reference

| Action | Script |
|---|---|
| One-time bootstrap: cluster + node pools + GAG install + tenant | `scripts/dogfood/setup.sh` |
| Start cluster + route CI to GAG | `scripts/dogfood/start.sh` |
| Stop cluster + route CI to GitHub-hosted | `scripts/dogfood/stop.sh` |
| Enable e2e on GAG | `scripts/dogfood/e2e-start.sh` |
| Disable e2e on GAG | `scripts/dogfood/e2e-stop.sh` |
| One-time e2e pool + Kata setup | `scripts/dogfood/e2e-setup.sh` |

All scripts read `PROJECT`, `CLUSTER`, `ZONE`, `REPO` (and `APP_ID`,
`INSTALLATION_ID` for the setup scripts) from the environment. Export the
Variables block once per shell session.

---

## Cost reference

| Scenario | $/hr | $/day (4 hr active) |
|---|---|---|
| Cluster at rest (0 nodes) | $0.00 | $0.00 |
| System node only, no jobs | $0.067 | $0.27 |
| System + 1 spot CI worker (e2-standard-4) | ~$0.11 | — |
| System + 4 spot CI workers (peak) | ~$0.23 | — |
| System + 2 spot e2e nodes (n2-standard-4, peak) | ~$0.18 | — |

A typical dogfood session (scale up, run a few PRs, scale down): under $0.50.

**E2e cost per PR** (kindnet + Calico in parallel, ~10 min each):
2 nodes × $0.058/hr × 10 min ≈ **$0.019**.
