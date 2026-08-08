# Getting Started

> **Want to see it first?** The [Demo](demo.md) walks one real GitHub job through
> a local kind cluster (job → ephemeral worker pod → green on GitHub) with the
> exact commands to reproduce it.

!!! tip "New tenants: start on `v2beta1`"
    The recommended shape for a new tenant is the **v2 API** at
    **`actions-gateway.com/v2beta1`**: a decomposed `ActionsGateway` +
    `RunnerSet` + `RunnerTemplate` (+ optional `EgressProxy`), shown in
    [Step 4](#4-create-your-gateway-and-runner-set-v2-recommended) below.
    `v2beta1` is the graduated, ScaleSet-only storage and hub version, v2's first
    stability contract, and where new capabilities land. `v2alpha1` stays served as
    the [`gag-migrate`](operations/migration-v1-to-v2.md) on-ramp, carrying the
    `acquisitionProtocol` selector a migrating v1 tenant needs and a new tenant does
    not, and the apiserver converts between the two. The older
    single-custom-resource (CR) **`v1alpha1`** API is still fully served but
    **[deprecated](operations/v1alpha1-deprecation.md)**: reach for it only if you
    have a specific reason to, and see the
    [legacy v1 path](#legacy-the-v1alpha1-api-deprecated). `v1alpha1`, `v2alpha1`,
    and the classic acquisition protocol are all
    [removed at `v2.0.0`](operations/v1alpha1-deprecation.md); `v2beta1` is not.
    Already on v1?
    [`gag-migrate`](operations/migration-v1-to-v2.md) moves you across without
    changing how your jobs are acquired.

## Prerequisites

- Kubernetes 1.30+ (GA `ValidatingAdmissionPolicy`); **1.31+** for the recommended
  v2 path (per-gateway `RunnerSet` scoping uses a server-side field selector,
  KEP-4358, that is alpha-off on 1.30)
- A CNI that enforces `NetworkPolicy` (Calico/Cilium) for the isolation controls to take effect
- [cert-manager](https://cert-manager.io) installed, *or* install with `--set certManager.enabled=false` to use the chart's self-signed webhook cert
- A GitHub App with a private key and installation ID
- Go 1.26+ only if you build the images yourself

## 1. Deploy the GMC

The shipped install artifact is the **`actions-gateway` Helm chart** ([reference](../charts/actions-gateway/README.md)). It installs the Gateway Manager Controller (GMC), its CustomResourceDefinitions (CRDs), RBAC, validating webhook, and admission policy. The GMC then provisions per-tenant Actions Gateway Controller (AGC) instances and proxy pools at runtime. They are not installed by the chart.

```sh
helm install gag charts/actions-gateway \
  --namespace gmc-system --create-namespace \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy> \
  --set wrapper.image.digest=sha256:<wrapper>
```

All four images must be **pinned by digest**. The chart refuses to render while any of the four digests (`gmc`/`agc`/`proxy`/`wrapper`) is empty, naming the one to set (the worker-wrapper image is on by default, so it is required too). Pin them as above, or pass `--set allowFloatingImageTags=true` for dev/test only. See the [chart README](../charts/actions-gateway/README.md) for the full values reference and the cert-manager toggle.

> **Dev/CI.** The Helm chart is the single install path. There is no kustomize alternative. To install an unreleased chart from a source checkout, substitute the local `charts/actions-gateway` path for the `oci://…` ref above; `make deploy` (used by the e2e suite) wraps the same `helm install` with floating image tags for local iteration.

For the recommended **v2** path, also install the opt-in v2 CRDs. They ship separately (and are applied server-side, never `helm install`ed) because the CRDs are large enough that a Helm release Secret would exceed its 1 MiB limit. For the default `gmc-system` namespace, apply the pre-rendered, cosign-signed manifest attached to the release, no helm needed:

```sh
kubectl apply --server-side -f \
  https://github.com/actions-gateway/github-actions-gateway/releases/download/vX.Y.Z/actions-gateway-crds-v2.yaml
```

If the GMC runs in a **different namespace**, render the chart for that namespace instead (`--server-side` also avoids the 256 KB client-side apply limit):

```sh
helm template actions-gateway-crds-v2 \
  oci://ghcr.io/actions-gateway/charts/actions-gateway-crds-v2 \
  --namespace <gmc-namespace> \
  | kubectl apply --server-side -f -
```

Either way the `clientConfig` must resolve to the GMC's `webhook-service`: each v2 CRD is served at `v2beta1` (the storage/hub version) and `v2alpha1`, and the apiserver converts between them via a conversion webhook hosted by the GMC (see [install.md § the v2 API CRDs](operations/install.md#optional-the-v2-api-crds) for signature verification and the full options). The GMC **detects the v2 CRDs at startup**: with them present it starts the v2 controllers; without them it comes up clean on v1 only (logging `actions-gateway.com/v2alpha1 CRDs not installed; v2 controllers disabled`). It does not error-loop. Because detection is once-at-startup, installing the v2 CRDs into an already-running GMC needs a restart (`kubectl rollout restart deploy -n gmc-system gmc-controller-manager`). See the [chart README](../charts/actions-gateway-crds-v2/README.md).

## 2. Create and mark the tenant namespace, and set its quota

Create the tenant namespace and mark it as managed by the GMC. The marker label
authorizes the GMC to stamp Pod Security Admission labels on it; the
`namespace-psa-guard` admission policy denies the GMC any namespace that lacks it.

```sh
kubectl create namespace team-a
kubectl label namespace team-a actions-gateway.github.com/tenant=true
```

> **v2 namespace markers.** In the recommended v2 flow the tenant marker aligns to
> `actions-gateway.com/tenant=managed`, and the Pod Security level moves off the CR
> onto the namespace as the `actions-gateway.com/security-profile` label (absent ⇒
> `baseline`). The GMC dual-reads both spellings during coexistence. For the exact
> v2 namespace labels and the profile-guard admission policy, see
> [Tenant Onboarding — v2 API](operations/tenant-onboarding.md#v2-api-multiple-gateways-per-namespace).

The namespace `ResourceQuota` (and any `LimitRange`) is **platform-owned**: the
platform admin creates and manages it on the tenant namespace, and the gateway
operates *within* it but never creates or mutates it. This is the real,
tenant-uncontrollable cap on how much compute a tenant can consume. Apply it
here, or via your GitOps / tenant-operator stack: Capsule, HNC, vCluster, kiosk.

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
---
apiVersion: v1
kind: LimitRange
metadata:
  name: team-a-defaults
  namespace: team-a
spec:
  limits:
    - type: Container
      defaultRequest:
        cpu: 100m
        memory: 128Mi
      default:
        cpu: "1"
        memory: 512Mi
```

The `LimitRange` is not optional here. A `ResourceQuota` that constrains
`requests.cpu`/`requests.memory` makes those requests **mandatory** for every pod
in the namespace: Kubernetes rejects any pod that omits a constrained resource.
The AGC control-plane pod stamps no requests of its own, so without a `LimitRange`
to supply defaults it is refused with `must specify requests.cpu` and never
schedules. The `LimitRange` above fills that gap (and covers any worker pod whose
runner group leaves requests unset). See
[only constrain keys every pod declares](operations/resourcequota-sizing.md#only-constrain-keys-every-pod-declares).

The gateway reads remaining quota and reacts to exhaustion (it won't claim a job
the quota can't place, and lock-cancels then reruns any job that loses headroom
after the claim, see [why-gag](why-gag.md)), but the quota itself is yours to
size and own.

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

## 4. Create your gateway and runner set (v2, recommended)

The **v2 API** (`actions-gateway.com/v2beta1`) decomposes the single v1 CR into
small, composable kinds: an `ActionsGateway` (identity + GitHub binding), a
`RunnerTemplate` (a reusable pod shape), a `RunnerSet` per runner type, and an
optional `EgressProxy`. The set below is feature-equivalent to the legacy v1
example, a proxied gateway with a GPU runner set (priority tiers) and a Linux
runner set:

```yaml
apiVersion: actions-gateway.com/v2beta1
kind: EgressProxy
metadata:
  name: team-a-egress
  namespace: team-a
spec:
  minReplicas: 2
  maxReplicas: 10
---
apiVersion: actions-gateway.com/v2beta1
kind: RunnerTemplate            # a reusable pod shape, referenced by many RunnerSets
metadata:
  name: default
  namespace: team-a
spec:
  podTemplate:
    spec:
      containers:
        - name: runner
---
apiVersion: actions-gateway.com/v2beta1
kind: ActionsGateway
metadata:
  name: team-a-gateway
  namespace: team-a
spec:
  credentials:
    type: GitHubApp             # discriminated union; WorkloadIdentity is the opt-in no-PEM member
    githubApp:
      name: my-github-app        # name-only Secret ref in this namespace
  # githubURL is the org/enterprise/repo the runners register against (required,
  # immutable). The App above must be installed on this same org/enterprise.
  githubURL: https://github.com/my-org
  defaultProxyRef:
    name: team-a-egress          # every RunnerSet below inherits this unless it sets its own proxyRef
  # securityProfile is a *namespace* label in v2 (set in Step 2), not a CR field.
  # All gateways in a namespace share one Pod Security level. See Tenant Onboarding.
---
apiVersion: actions-gateway.com/v2beta1
kind: RunnerSet
metadata:
  name: gpu                      # v2 names its own runner set; no first-label-derives-name rule
  namespace: team-a
spec:
  gatewayRef:  { name: team-a-gateway }
  templateRef: { name: default }
  # Exactly one label: it is this set's scale-set name at GitHub and its single
  # runs-on match target (workflows say `runs-on: gpu`). Must be unique per gateway.
  runnerLabels: ["gpu"]
  # priorityClassName values must be on the platform-owned allowlist: a watched
  # PriorityClassAllowlist CR, grown without a GMC restart (the
  # --allowed-priority-classes flag remains the fail-safe baseline).
  # Preemption is set on the PriorityClass object, not here.
  priorityTiers:
    - priorityClassName: runner-critical
      threshold: 5
    - priorityClassName: runner-standard
      threshold: 20
---
apiVersion: actions-gateway.com/v2beta1
kind: RunnerSet
metadata:
  name: linux
  namespace: team-a
spec:
  gatewayRef:  { name: team-a-gateway }
  templateRef: { name: default }
  runnerLabels: ["linux"]
  maxWorkers: 30
```

The GMC provisions the AGC, the proxy pool, RBAC, and network policies in
`team-a` automatically. The **namespace `ResourceQuota` is still platform-owned**
(Step 2). It is not a field on any of these CRs.

**The proxy is optional in v2.** Drop the `EgressProxy` and the `defaultProxyRef`
and traffic egresses **directly** to GitHub, still `NetworkPolicy`-restricted to
DNS + the GitHub CIDR allowlist, but without a stable per-tenant egress IP to
allow-list. That collapses the minimal onboarding to three objects. For the
proxy-less flow, reusable/cluster-default templates, multiple gateways per
namespace, the 52-character name cap, and workload-identity credentials, see
[Tenant Onboarding — v2 API](operations/tenant-onboarding.md#v2-api-multiple-gateways-per-namespace)
and [Appendix H — v2 API decomposition](design/appendix-h-v2-api-decomposition.md).

Tenants requiring more than 250 concurrent sessions should shard across multiple `ActionsGateway` CRs, each backed by a separate GitHub App installation. See [Appendix A — Capacity Targets & SLOs](design/appendix-a-capacity-slos.md) for limits.

## Legacy: the `v1alpha1` API (deprecated)

!!! warning "`v1alpha1` is deprecated: new tenants should use v2 above"
    The `actions-gateway.github.com/v1alpha1` single-CR API is still fully served
    and supported, but it is **[deprecated](operations/v1alpha1-deprecation.md)**
    in favor of the decomposed v2 API, and it is **removed at `v2.0.0`** (announced
    with `v1.3.0`, one release ahead, per the project's removal policy). Use it only
    if you have a specific reason not to adopt v2 yet; if you
    are already on v1, [`gag-migrate`](operations/migration-v1-to-v2.md) moves you
    to v2 without changing how your jobs are acquired.

The v1 API expresses the whole gateway, proxy and all runner groups, in a single
`ActionsGateway` CR:

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
  # on the tenant namespace. Defaults to "baseline", which blocks privileged
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
  # step 2. It is not a field on this CR.
  #
  # A runner group has no `name` field. Its RunnerGroup CR name is derived by the
  # GMC from the gateway name + the group's FIRST runnerLabel, so make that first
  # label distinctive per group ("gpu", "linux" below). Two groups whose first
  # label matches would derive the same name and collide.
  runnerGroups:
    - runnerLabels: ["gpu", "self-hosted"]
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
    - runnerLabels: ["linux", "self-hosted"]
      maxWorkers: 30
      podTemplate:
        spec:
          containers:
            - name: runner
```

The GMC will provision the AGC, proxy pool, RBAC, and network policies in `team-a` automatically.

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

**Important:** Do not update the Secret in-place. The GMC watches the `gitHubAppRef.name` reference, not the Secret's contents. Changing the Secret data without changing the reference name does not trigger an AGC rollout. The AGC continues using the cached token derived from the old key until it restarts or the token expires. Creating a new Secret and updating the reference is the correct rotation path.

**If the referenced Secret is deleted before you complete the rotation**, the GMC sets a `CredentialUnavailable=True` condition on the `ActionsGateway` CR and stops reconciling child resources. Recreating the Secret (with the same name, or updating `gitHubAppRef.name`) clears the condition and resumes normal operation. To inspect the condition:

```sh
kubectl get actionsgateway -n team-a team-a-gateway -o jsonpath='{.status.conditions}' | jq .
```
