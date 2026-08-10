# Reproduce the local-kind demo (from source)

This is the from-source procedure behind the [demo recording](../demo.md) — one real GitHub Actions job run through GitHub Actions Gateway (GAG) on a local [kind](https://kind.sigs.k8s.io/) cluster, against **real GitHub**.
It uses the repo's `make` targets and a local image registry, so run it from a checkout with the dev toolchain (`make doctor` checks it).

> To **install** GAG the normal way (the released Helm chart with digest-pinned images), follow [Getting Started](../getting-started.md) instead — this guide is for reproducing the demo end-to-end from a source build.

Two things this flow does that a released install would not, both flagged as backlog items:

- Installs the **v2alpha1 CRDs** alongside the v1 platform.
  A v1-only install currently crash-loops the AGC (its v2 `RunnerSet` reconciler is registered unconditionally and its informer cache never syncs) — see `Q261` in [docs/STATUS.md](../STATUS.md).
- Adds a **`LimitRange`** so the requests-less AGC pod is admitted under the namespace `ResourceQuota` — see `Q262`.

## Prerequisites

- [Docker](https://docs.docker.com/get-docker/), [kind](https://kind.sigs.k8s.io/), and [kubectl](https://kubernetes.io/docs/tasks/tools/) (`make doctor` checks your toolchain).
- The [GitHub CLI](https://cli.github.com/) (`gh`), authenticated (`gh auth login`) with rights to dispatch workflows in your test repo.
- A **GitHub App** with runner control-plane permissions, and a repo (or org) that App is installed on, containing a workflow that targets a self-hosted label.
  The demo uses a one-line workflow:

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

  See [Getting Started §3](../getting-started.md#3-create-a-github-app-credential-secret) for how to create the GitHub App credential Secret.

## 1. Cluster and platform

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

## 2. Onboard a tenant

```sh
kubectl create namespace team-a
kubectl label ns team-a actions-gateway.github.com/tenant=true
```

Apply a platform-owned `ResourceQuota` and a `LimitRange` that supplies default container requests (so pods without explicit requests are admitted under the quota):

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

Apply one `ActionsGateway` CR. `completedPodTTL: 0s` deletes each worker pod the moment its job finishes (the default retains terminal pods for 5 minutes so their logs stay inspectable):

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

The AGC registers runners with GitHub.
Confirm they are online and idle:

```sh
kubectl get actionsgateway,runnergroup -n team-a
gh api repos/<your-org>/<your-repo>/actions/runners \
  --jq '.runners[] | [.name, .status, (.busy|tostring)] | @tsv'
```

## 3. Run a job and watch the pod

```sh
# In one terminal: watch the tenant namespace
kubectl get pods -n team-a -w

# In another: dispatch one job
gh workflow run test-job.yml --repo <your-org>/<your-repo> --ref main
```

A `runner-…` pod appears, runs the job, and is deleted on completion.
Confirm the run is green on GitHub:

```sh
gh run list --repo <your-org>/<your-repo> --workflow test-job.yml --limit 1
gh run view <run-id> --repo <your-org>/<your-repo>
```

## Clean up

```sh
make e2e-clean      # deletes the kind cluster and the local registry
```

## The recording

The animated recording embedded on the [demo page](../demo.md) is a hand-authored [asciinema](https://asciinema.org/) cast (v2) assembled from the real command outputs of a run of these steps, then rendered to a self-contained animated SVG.
The cast and its generator live in [`docs/assets/`](../assets/README.md) — see [docs/assets/README.md](../assets/README.md) for how to regenerate them.
