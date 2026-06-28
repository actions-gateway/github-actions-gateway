# Getting Started

## Prerequisites

- Kubernetes 1.30+ (GA `ValidatingAdmissionPolicy`)
- A CNI that enforces `NetworkPolicy` (Calico/Cilium) for the isolation controls to take effect
- [cert-manager](https://cert-manager.io) installed, *or* install with `--set certManager.enabled=false` to use the chart's self-signed webhook cert
- A GitHub App with a private key and installation ID
- Go 1.24+ only if you build the images yourself

## 1. Deploy the GMC

The shipped install artifact is the **`actions-gateway` Helm chart** ([reference](../charts/actions-gateway/README.md)). It installs the Gateway Manager Controller (GMC), its CRDs, RBAC, validating webhook, and admission policy. The GMC then provisions per-tenant AGC instances and proxy pools at runtime — they are not installed by the chart.

```sh
helm install gag charts/actions-gateway \
  --namespace gmc-system --create-namespace \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy>
```

All three images must be **pinned by digest** — the chart refuses to render while `gmc.image.digest` is empty, and the GMC crash-loops on floating AGC/proxy tags — so pin them as above (or pass `--set allowFloatingImageTags=true` for dev/test only). See the [chart README](../charts/actions-gateway/README.md) for the full values reference and the cert-manager toggle.

> **Dev/CI.** The Helm chart is the single install path — there is no kustomize alternative. To install an unreleased chart from a source checkout, substitute the local `charts/actions-gateway` path for the `oci://…` ref above; `make deploy` (used by the e2e suite) wraps the same `helm install` with floating image tags for local iteration.

## 2. Create and mark the tenant namespace, and set its quota

Create the tenant namespace and mark it as managed by the GMC. The marker label
authorizes the GMC to stamp Pod Security Admission labels on it; the
`namespace-psa-guard` admission policy denies the GMC any namespace that lacks it.

```sh
kubectl create namespace team-a
kubectl label namespace team-a actions-gateway.github.com/tenant=true
```

The namespace `ResourceQuota` (and any `LimitRange`) is **platform-owned**: the
platform admin creates and manages it on the tenant namespace, and the gateway
operates *within* it but never creates or mutates it. This is the real,
tenant-uncontrollable cap on how much compute a tenant can consume — apply it
here (or via your GitOps / tenant-operator stack: Capsule, HNC, vCluster, kiosk):

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: team-a-quota
  namespace: team-a
spec:
  hard:
    requests.cpu: "20"
    requests.memory: "40Gi"
    pods: "50"
```

The gateway reads remaining quota and reacts to exhaustion (it fast-cancels and
reruns quota-blocked jobs — see [why-gag](why-gag.md)), but the quota itself is
yours to size and own.

## 3. Create a GitHub App credential Secret

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

## 4. Create an ActionsGateway resource

```yaml
apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: team-a-gateway
  namespace: team-a
spec:
  gitHubAppRef:
    name: my-github-app
  # gitHubURL is the org/enterprise/repo URL the runners register against
  # (required). The App above must be installed on this same org/enterprise.
  gitHubURL: https://github.com/my-org
  # securityProfile selects the Pod Security Admission level the GMC stamps
  # on the tenant namespace. Defaults to "baseline" — blocks privileged
  # containers, host namespaces, hostPath, and dangerous capabilities, with
  # no external policy engine required. Use "restricted" for stricter
  # isolation (runAsNonRoot, drop ALL caps, seccomp RuntimeDefault) or
  # "privileged" for workloads like docker-in-docker that need an unrestricted
  # PodSpec. See docs/design/05-security.md §5.3 for the full semantics.
  securityProfile: baseline
  proxy:
    minReplicas: 2
    maxReplicas: 10
  # The namespace ResourceQuota is platform-owned and set on the namespace in
  # step 2 — it is not a field on this CR.
  runnerGroups:
    - name: gpu-runners
      runnerLabels: ["self-hosted", "gpu"]
      maxListeners: 10
      # priorityClassName values must be on the GMC --allowed-priority-classes
      # allowlist (platform-owned); preemption is set on the PriorityClass object.
      priorityTiers:
        - priorityClassName: runner-critical
          threshold: 5
        - priorityClassName: runner-standard
          threshold: 20
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

## Optional: the v2alpha1 API (alpha)

Everything above uses the **`v1alpha1`** API (`actions-gateway.github.com`), which is fully supported and is the standard path. A second, **alpha** API — **`v2alpha1`** (`actions-gateway.com`) — ships *beside* it for early adopters. It decomposes the single `ActionsGateway` CR into five smaller kinds (`ActionsGateway`, `RunnerSet`, `RunnerTemplate`, `ClusterRunnerTemplate`, `EgressProxy`) and adds:

- **multiple `ActionsGateway`s per namespace** (v1 is one-per-namespace);
- **reusable runner templates** — a `RunnerTemplate` (or cluster-scoped `ClusterRunnerTemplate`) referenced by many runner sets, instead of an inline pod template copied into each group;
- an **optional shared egress proxy** (`EgressProxy`) any runner set can point at — with direct egress when none is set.

`v2alpha1` is **alpha and may change incompatibly**; `v1alpha1` remains fully supported and v2 is not a drop-in replacement. Adopt it only when you want the new shape.

The v2 CRDs ship in a **separate, opt-in chart** — `actions-gateway-crds-v2` — because they are large enough that bundling them would push the main chart's Helm release Secret past its 1 MiB limit. Install it alongside the main `actions-gateway` chart (any order):

```sh
helm install actions-gateway-crds-v2 \
  oci://ghcr.io/actions-gateway/charts/actions-gateway-crds-v2
```

The opt-in is real on the controller side too: the GMC **detects the v2 CRDs at startup**. A v1-only install (the main chart without this CRD chart) comes up clean — the GMC logs `actions-gateway.com/v2alpha1 CRDs not installed; v2 controllers disabled` and starts only the v1 controllers; it does **not** error-loop on the missing kinds. Because detection happens once at startup, **installing the v2 CRDs into an already-running cluster requires a GMC restart** (`kubectl rollout restart deploy -n gmc-system gmc-controller-manager`) before the v2 controllers activate.

The CRDs install and validate on Kubernetes ≥ 1.30, but per-gateway scoping (the `RunnerSet` `spec.gatewayRef.name` field selector, KEP-4358) requires **Kubernetes ≥ 1.31**. See the [chart README](../charts/actions-gateway-crds-v2/README.md) for details.

For the v2 onboarding flow — the worked three-object example, per-gateway naming and the 52-character name cap, and the namespace-scoped security profile — see [Appendix H — v2 API decomposition](design/appendix-h-v2-api-decomposition.md), the v2 sections in [Tenant Onboarding](operations/tenant-onboarding.md#v2-api-alpha-multiple-gateways-per-namespace), and [Troubleshooting](operations/troubleshooting.md#multiple-v2-gateways-in-one-namespace-naming-scoping-prerequisites).

## Rotating GitHub App Credentials

When your GitHub App private key expires or is compromised, follow these steps to rotate credentials without downtime:

1. **Generate a new private key** in the GitHub App settings (Settings → Developer settings → GitHub Apps → `<app>` → Private keys → Generate a private key). Download the `.pem` file.

2. **Create a new Secret** with the new key. Use a distinct name from the old Secret:

   ```yaml
   apiVersion: v1
   kind: Secret
   metadata:
     name: my-github-app-v2   # new name
     namespace: team-a
   type: Opaque
   stringData:
     appId: "123456"
     installationId: "78901234"
     privateKey: |
       -----BEGIN RSA PRIVATE KEY-----
       ...new key...
       -----END RSA PRIVATE KEY-----
   ```

3. **Update the `ActionsGateway` CR** to reference the new Secret name:

   ```sh
   kubectl patch actionsgateway -n team-a team-a-gateway \
     --type=merge -p '{"spec":{"gitHubAppRef":{"name":"my-github-app-v2"}}}'
   ```

   The GMC detects the Secret reference change, updates the AGC pod template (including an `actions-gateway/github-app-secret` annotation that records the new Secret name), and triggers a rolling update. The new pod mounts the new Secret and immediately begins using the new credentials.

4. **Confirm the rollout completed:**

   ```sh
   kubectl rollout status deploy/actions-gateway-controller -n team-a
   # Optionally inspect rotation history:
   kubectl rollout history deploy/actions-gateway-controller -n team-a
   ```

5. **Verify the new token is working:**

   ```sh
   kubectl logs -n team-a deploy/actions-gateway-controller --tail=20
   # Look for: "token refresh successful" or no token refresh errors
   ```

6. **Delete the old Secret** once the rollout is confirmed healthy:

   ```sh
   kubectl delete secret my-github-app -n team-a
   ```

7. **Revoke the old key** in the GitHub App settings.

**Important:** Do not update the Secret in-place. The GMC watches the `gitHubAppRef.name` reference, not the Secret's contents. Changing the Secret data without changing the reference name does not trigger an AGC rollout — the AGC will continue using the cached token derived from the old key until it restarts or the token expires. Creating a new Secret and updating the reference is the correct rotation path.

**If the referenced Secret is deleted before you complete the rotation**, the GMC sets a `CredentialUnavailable=True` condition on the `ActionsGateway` CR and stops reconciling child resources. Recreating the Secret (with the same name, or updating `gitHubAppRef.name`) clears the condition and resumes normal operation. To inspect the condition:

```sh
kubectl get actionsgateway -n team-a team-a-gateway -o jsonpath='{.status.conditions}' | jq .
```
