# GKE Dogfood Runbook

On-demand GKE cluster for dogfooding GAG's own CI. The cluster costs $0 at
rest (zero nodes), roughly $0.07/hr when idling with the system node only, and
adds ≈$0.04/hr per spot worker node while jobs are running.

**What runs where after setup**

| Workflow | Jobs migrated to GAG | Jobs kept on `ubuntu-latest` |
|---|---|---|
| `unit-test.yml` | `lint`, `shellcheck`, `vendor-check`, `tidy-check`, `unit-test`, `coverage` | `changes` |
| `integration-test.yml` | `integration-test` | `changes` |
| `e2e-test.yml` | *(nothing — requires Docker)* | all |

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
REPO=karlkfi/github-actions-gateway
APP_ID=3752347
INSTALLATION_ID=<get from Part C step 1>
```

---

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

```bash
make validate-cluster
```

### B2. Create Helm values file

```bash
cat > tmp/values-dogfood.yaml <<'EOF'
# Dogfood / dev mode: float image tags rather than pinning digests.
# Production installs must use digest-pinned images from the release page.
allowFloatingImageTags: true
gmc:
  image:
    tag: latest
agc:
  image:
    tag: latest
proxy:
  image:
    tag: latest

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

### B3. Install GAG chart

```bash
helm install gag charts/actions-gateway \
  --namespace gmc-system --create-namespace \
  --values tmp/values-dogfood.yaml

kubectl rollout status deployment/gmc-controller -n gmc-system --timeout=3m
```

### B4. Create tenant namespace

```bash
kubectl create namespace gag-dogfood

# GAG requires the tenant label; baseline PSA matches our securityProfile.
kubectl label namespace gag-dogfood \
  actions-gateway.github.com/tenant=true \
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

### B7. Create ActionsGateway CR

```bash
kubectl apply -f - <<'EOF'
apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: dogfood-gateway
  namespace: gag-dogfood
spec:
  gitHubAppRef:
    name: github-app-v1
  gitHubURL: https://github.com/karlkfi/github-actions-gateway
  securityProfile: baseline
  proxy:
    minReplicas: 1
    maxReplicas: 4
  runnerGroups:
    - name: ci
      runnerLabels: ["self-hosted", "linux", "gag-ci"]
      maxListeners: 8
      maxWorkers: 4
      podTemplate:
        spec:
          tolerations:
            - key: dedicated
              value: workers
              effect: NoSchedule
          containers:
            - name: runner
              resources:
                requests:
                  cpu: "2"
                  memory: "4Gi"
                limits:
                  cpu: "4"
                  memory: "8Gi"
EOF
```

### B8. Validate

```bash
kubectl get actionsgateway -n gag-dogfood dogfood-gateway
kubectl get pods -n gag-dogfood

# Runners should appear within ~2 min of the AGC becoming Ready
gh api /repos/"$REPO"/actions/runners \
  --jq '.runners[] | {name, status, labels: [.labels[].name]}'
```

---

## Part C — One-time GitHub setup

### C1. Confirm App installation + get installation ID

Ensure `actions-gateway-test` is installed on the repo:
- GitHub.com → Settings → Applications → Installed GitHub Apps →
  `actions-gateway-test` → Configure
- Confirm `karlkfi/github-actions-gateway` is listed under
  "Repository access"

Get the installation ID:

```bash
gh api /user/installations \
  --jq '.installations[] | select(.app_id == 3752347) | {id, account: .account.login}'
```

Set `INSTALLATION_ID` to the `id` value and re-run the secret creation in
B5 if you had a placeholder.

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
kubectl rollout status deployment/gmc-controller -n gmc-system --timeout=5m
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

## Cost reference

| Scenario | $/hr | $/day (4 hr active) |
|---|---|---|
| Cluster at rest (0 nodes) | $0.00 | $0.00 |
| System node only, no jobs | $0.067 | $0.27 |
| System + 1 spot worker | ~$0.11 | — |
| System + 4 spot workers (peak) | ~$0.23 | — |

A typical dogfood session (scale up, run a few PRs, scale down): under $0.50.
