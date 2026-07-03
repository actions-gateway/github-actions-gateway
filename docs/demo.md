# Demo: a real job on a local cluster

Watch one GitHub Actions job go from `workflow_dispatch` to green — routed
through GitHub Actions Gateway (GAG), run in an **ephemeral worker pod** on a
local [kind](https://kind.sigs.k8s.io/) cluster, and reaped the instant it
finishes. Nothing here is staged: every line was captured from a real run
against **real GitHub** (a self-hosted `runs-on: e2e` workflow), then replayed at
readable speed.

![Terminal recording: a GitHub Actions job running on GAG on a local kind cluster — the worker pod appears, runs, and is reaped on completion.](assets/demo-local-kind.svg)

## What you just watched

| Beat | What happens | Why it matters |
| --- | --- | --- |
| **Install** | A kind cluster comes up; the Gateway Manager Controller (GMC) is installed with one `helm install`. | One controller to run the whole platform. |
| **Onboard** | One `ActionsGateway` custom resource (CR) provisions the tenant's Actions Gateway Controller (AGC) + egress proxy and registers self-hosted runners with GitHub. | A tenant is a single CR, not a pile of Deployments. |
| **Idle** | Before any job, the tenant namespace holds only the controller and proxy — **zero worker pods**. | No idle compute (and no idle GPUs) between jobs. |
| **Run** | `gh workflow run` dispatches one job. A worker pod goes `Pending → Running → Completed`, then is **deleted on completion**. | One job → one short-lived pod; the node is freed immediately. |
| **Green** | `gh run view` shows the job succeeded on GitHub; the tenant namespace is back to controller + proxy only. | The job really ran on GitHub, and compute returned to zero. |

## Reproduce it yourself

This is the **free** path: a local kind cluster and a real GitHub repo — no paid
infrastructure. The commands below are the ones in the recording; run them from a
checkout of the [repository](https://github.com/actions-gateway/github-actions-gateway).

### Prerequisites

- [Docker](https://docs.docker.com/get-docker/), [kind](https://kind.sigs.k8s.io/), and [kubectl](https://kubernetes.io/docs/tasks/tools/) (`make doctor` checks your toolchain).
- The [GitHub CLI](https://cli.github.com/) (`gh`), authenticated (`gh auth login`) with rights to dispatch workflows in your test repo.
- A **GitHub App** with runner control-plane permissions, and a repo (or org) that
  App is installed on, containing a workflow that targets a self-hosted label. The
  demo uses a one-line workflow:

  ```yaml
  # .github/workflows/test-job.yml
  name: Test self-hosted runner
  on: workflow_dispatch
  jobs:
    test:
      runs-on: e2e            # matches the runner group's runnerLabels below
      steps:
        - run: echo "job acquired by gateway runner"
  ```

  See [Getting Started §3](getting-started.md#3-create-a-github-app-credential-secret)
  for how to create the GitHub App credential Secret.

### 1. Cluster and platform

```sh
# 3-node kind cluster + a local image registry
make e2e-cluster
make apply-cert-manager && make wait-cert-manager   # the GMC webhook needs cert-manager

# Build the gmc/agc/proxy/worker/wrapper images into the local registry
make e2e-images

# Install the platform (Gateway Manager Controller) from the Helm chart.
# `make deploy` wraps `helm upgrade --install` with dev-friendly floating tags.
TAG="e2e-$(git rev-parse --short HEAD)"
( cd cmd/gmc && make deploy \
    GMC_IMG="127.0.0.1:5000/gmc:$TAG"    AGC_IMG="127.0.0.1:5000/agc:$TAG" \
    PROXY_IMG="127.0.0.1:5000/proxy:$TAG" WRAPPER_IMG="127.0.0.1:5000/wrapper:$TAG" )

# Install the v2alpha1 CRDs too, so the controller's v2 reconciler has its types.
kubectl apply --server-side --force-conflicts -f api/config/crd

kubectl -n gmc-system rollout status deploy/gmc-controller-manager
```

### 2. Onboard a tenant

```sh
kubectl create namespace team-a
kubectl label ns team-a actions-gateway.github.com/tenant=true
```

Apply a platform-owned `ResourceQuota` and a `LimitRange` that supplies default
container requests (so pods without explicit requests are admitted under the
quota):

```yaml
# tenant-quota.yaml
apiVersion: v1
kind: ResourceQuota
metadata: { name: team-a-quota, namespace: team-a }
spec:
  hard: { requests.cpu: "20", requests.memory: 40Gi, pods: "50" }
---
apiVersion: v1
kind: LimitRange
metadata: { name: team-a-defaults, namespace: team-a }
spec:
  limits:
    - type: Container
      defaultRequest: { cpu: 100m, memory: 128Mi }
      default: { cpu: "1", memory: 512Mi }
```

```sh
kubectl apply -f tenant-quota.yaml

# GitHub App credential Secret (read the key from a file — never an env var)
kubectl create secret generic team-a-github-app -n team-a \
  --from-literal=appId="$APP_ID" \
  --from-literal=installationId="$INSTALL_ID" \
  --from-file=privateKey=app.pem
```

Apply one `ActionsGateway` CR. `completedPodTTL: 0s` deletes each worker pod the
moment its job finishes (the default retains terminal pods for 5 minutes so their
logs stay inspectable):

```yaml
# actionsgateway.yaml
apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata: { name: team-a-gateway, namespace: team-a }
spec:
  gitHubAppRef: { name: team-a-github-app }
  gitHubURL: https://github.com/<your-org>/<your-repo>   # where runners register
  securityProfile: baseline
  proxy: { minReplicas: 1, maxReplicas: 3 }
  runnerGroups:
    - runnerLabels: ["e2e"]        # must match the workflow's runs-on
      maxListeners: 2
      completedPodTTL: 0s
      workerImage: 127.0.0.1:5000/worker:e2e-<tag>
      podTemplate:
        spec:
          containers:
            - name: runner
              image: 127.0.0.1:5000/worker:e2e-<tag>
```

```sh
kubectl apply -f actionsgateway.yaml
kubectl wait --for=condition=Available deploy/actions-gateway-controller -n team-a --timeout=5m
```

The AGC registers runners with GitHub. Confirm they are online and idle:

```sh
kubectl get actionsgateway,runnergroup -n team-a
gh api repos/<your-org>/<your-repo>/actions/runners \
  --jq '.runners[] | [.name, .status, (.busy|tostring)] | @tsv'
```

### 3. Run a job and watch the pod

```sh
# In one terminal: watch the tenant namespace
kubectl get pods -n team-a -w

# In another: dispatch one job
gh workflow run test-job.yml --repo <your-org>/<your-repo> --ref main
```

A `runner-…` pod appears, runs the job, and is deleted on completion. Confirm the
run is green on GitHub:

```sh
gh run list --repo <your-org>/<your-repo> --workflow test-job.yml --limit 1
gh run view <run-id> --repo <your-org>/<your-repo>
```

### Clean up

```sh
make e2e-clean      # deletes the kind cluster and the local registry
```

## Next steps

- [Getting Started](getting-started.md) — the full install and onboarding reference (released images, cert-manager options, quotas, credential rotation).
- [Why GAG?](why-gag.md) — how GAG compares to Actions Runner Controller (ARC).
- [Architecture](design/02-architecture.md) — the four-tier design behind what the demo shows.

---

*The recording is a hand-authored [asciinema](https://asciinema.org/) cast (v2)
assembled from the real command outputs of the run above, then rendered to a
self-contained animated SVG. The cast and its generator live in
[`docs/assets/`](https://github.com/actions-gateway/github-actions-gateway/tree/main/docs/assets)
(`demo-local-kind.cast`, `generate-demo-cast.py`).*
