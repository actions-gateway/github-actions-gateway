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
> image. A download that times out is far more likely the cold-start
> IP-range-cache race (Q61) or the node CPU exhaustion that Q247 says co-occurred;
> confirm which on a live run before acting on Q246.
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
