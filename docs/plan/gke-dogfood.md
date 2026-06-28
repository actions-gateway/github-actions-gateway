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
ZONE=us-central1-a
PROJECT=actions-gateway-dogfood   # must be globally unique; append 4 digits if needed
REPO=actions-gateway/github-actions-gateway
APP_ID=3752347
INSTALLATION_ID=135739122         # actions-gateway org install (re-derive via Part C1)
```

---

> **Shortcut:** Parts A3–B8 (cluster, node pools, GAG install, tenant) are
> automated by [`scripts/dogfood-setup.sh`](../../scripts/dogfood-setup.sh) —
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
    tag: v1.1.0-rc.3
agc:
  image:
    tag: v1.1.0-rc.3
proxy:
  image:
    tag: v1.1.0-rc.3

# Self-signed webhook cert — no cert-manager dependency.
# The cert rotates on helm upgrade; acceptable for a personal dogfood cluster.
certManager:
  enabled: false

# Keep GMC on the system node pool (default-pool) so it stays down
# when we scale that pool to 0. AGC and proxy inherit this via scheduling
# because the worker pool's taint blocks them without a toleration.
nodeSelector:
  cloud.google.com/gke-nodepool: default-pool
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
`scripts/dogfood-setup.sh` git-archives the chart at `$GAG_IMAGE_TAG`; the manual
equivalent for the pinned `v1.1.0-rc.3`:

```bash
git archive v1.1.0-rc.3 charts/actions-gateway-crds-v2 | tar -x -C tmp/
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
          # Explicit image required: the AGC injects its DefaultWorkerImage only
          # when it *creates* the runner container, not when the template names
          # one (Q233). This is the upstream actions-runner image the AGC
          # defaults to (cmd/agc/names: RunnerVersion).
          image: ghcr.io/actions/actions-runner:2.335.1@sha256:08c30b0a7105f64bddfc485d2487a22aa03932a791402393352fdf674bda2c29
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

> **Known blocker — job execution does not complete on `v1.1.0-rc.3` (Q234).**
> The control plane is validated end-to-end on rc.3 (GMC + AGC roll, gateway
> `Ready=True`, App-Secret credential path, Q229 egress-DNS token fetch, and all
> `maxListeners` runners register). A worker pod **is** provisioned and runs to
> `Completed`, but the job never finishes on GitHub: the AGC's per-job
> `RenewJob` call is rejected with `401 "Not authorized for this job"`, so GitHub
> holds the job `in_progress`, the runner stays `busy`, and post-job recycle
> loops on `422 (runner currently running a job)`. The App's installation
> permissions are sufficient (`actions:write`, `administration:write`,
> `organization_self_hosted_runners:write`), so this is a job-token/`run_service_url`
> auth issue, not a permission gap. Until Q234 is root-caused, routing real CI to
> GAG (Parts C–D) will hang jobs. Validation reached job→pod; **not** pod→GitHub.

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
kubectl wait --for=condition=Ready pod -l app=agc -n gag-dogfood --timeout=3m

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
scripts/dogfood-e2e-setup.sh
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
# Enable (requires system pool to be up via dogfood-start.sh first)
scripts/dogfood-e2e-start.sh

# Disable (e2e pool autoscales to 0 once in-flight jobs finish, ~10 min)
scripts/dogfood-e2e-stop.sh
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
| One-time bootstrap: cluster + node pools + GAG install + tenant | `scripts/dogfood-setup.sh` |
| Start cluster + route CI to GAG | `scripts/dogfood-start.sh` |
| Stop cluster + route CI to GitHub-hosted | `scripts/dogfood-stop.sh` |
| Enable e2e on GAG | `scripts/dogfood-e2e-start.sh` |
| Disable e2e on GAG | `scripts/dogfood-e2e-stop.sh` |
| One-time e2e pool + Kata setup | `scripts/dogfood-e2e-setup.sh` |

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
