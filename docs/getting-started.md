# Getting Started

## Prerequisites

- Kubernetes 1.11.3+
- Go 1.24+
- A GitHub App with a private key and installation ID

## 1. Deploy the GMC

```sh
# Build and push the GMC image
make docker-build docker-push IMG=<registry>/gmc:tag

# Install CRDs
make install

# Deploy the GMC
make deploy IMG=<registry>/gmc:tag
```

## 2. Create a GitHub App credential Secret

Create this in the tenant's namespace:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-github-app
  namespace: team-a
type: Opaque
stringData:
  appId: "123456"
  installationId: "78901234"
  privateKey: |
    -----BEGIN RSA PRIVATE KEY-----
    ...
    -----END RSA PRIVATE KEY-----
```

## 3. Create an ActionsGateway resource

```yaml
apiVersion: actions.gateway/v1alpha1
kind: ActionsGateway
metadata:
  name: team-a-gateway
  namespace: team-a
spec:
  gitHubAppRef:
    name: my-github-app
  proxy:
    minReplicas: 2
    maxReplicas: 10
  namespaceQuota:
    requests.cpu: "20"
    requests.memory: "40Gi"
    pods: "50"
  runnerGroups:
    - name: gpu-runners
      runnerLabels: ["self-hosted", "gpu"]
      maxListeners: 10
      priorityTiers:
        - priorityClassName: runner-critical
          threshold: 5
          preemptionPolicy: PreemptLowerPriority
        - priorityClassName: runner-standard
          threshold: 20
          preemptionPolicy: Never
      podTemplate:
        spec:
          containers:
            - name: runner
              resources:
                limits:
                  nvidia.com/gpu: "1"
    - name: cpu-runners
      runnerLabels: ["self-hosted", "linux"]
      maxWorkers: 30
      podTemplate:
        spec:
          containers:
            - name: runner
```

The GMC will provision the AGC, proxy pool, RBAC, and network policies in `team-a` automatically.

Tenants requiring more than 250 concurrent sessions should shard across multiple `ActionsGateway` CRs, each backed by a separate GitHub App installation. See [Appendix A — Capacity Targets & SLOs](design/appendix-a-capacity-slos.md) for limits.
