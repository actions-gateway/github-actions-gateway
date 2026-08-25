# Tenant Onboarding Checklist

> **Audience:** Platform engineer

This checklist walks from pre-conditions through first successful job.
For the full setup reference, see [Getting Started](../getting-started.md).
For day-2 operations after onboarding, see the [Runbook](runbook.md).
**Coming from Actions Runner Controller (ARC)?** Start with the [Migrating from ARC guide](migration-from-arc.md) — it maps ARC scale-set concepts onto the steps below and walks one runner group across.

!!! tip "New tenants: onboard on the v2 API"
    Steps 0–3 (GitHub App, Secret, `ResourceQuota`) are the same for every tenant.
    At the gateway step, the recommended shape for a **new** tenant is the **v2 API** at `actions-gateway.com/v2beta1` — see [v2 API](#v2-api-multiple-gateways-per-namespace) below and the [getting-started v2 walkthrough](../getting-started.md#4-create-your-gateway-and-runner-set-v2-recommended).
    The single-CR `v1alpha1` flow in [Step 2](#step-2-create-the-actionsgateway-resource) is still fully supported but **[deprecated and removed at `v2.0.0`](v1alpha1-deprecation.md)**, along with `v2alpha1` and the Classic protocol; already on v1?
    [`gag-migrate`](migration-v1-to-v2.md) moves you across.

---

## Pre-Conditions

Before beginning, confirm all of the following:

- [ ] **Namespace exists and is marked as a managed tenant.** The tenant's Kubernetes namespace has been created and carries the marker label `actions-gateway.github.com/tenant=true`:
  ```sh
  kubectl create namespace <tenant-namespace>   # if it does not exist yet
  kubectl label namespace <tenant-namespace> actions-gateway.github.com/tenant=true
  ```
  This label is what authorizes the GMC to operate in the namespace at all.
  Two admission policies key on it: `namespace-psa-guard` denies the GMC any namespace patch (the PSA-stamping step) it has *not* been marked for, and `gmc-tenant-resource-guard` denies the GMC any create/update/delete of tenant resources (Deployments, Secrets, RoleBindings, NetworkPolicies, …) outside marked namespaces.
  So an unlabelled namespace leaves the `ActionsGateway` stuck with a `NamespaceMarkerMissing` warning event and no provisioned resources.
  Apply the label with a trusted (administrator) identity — the GMC must never set it itself.
  Verify: `kubectl get namespace <tenant-namespace> -o jsonpath='{.metadata.labels.actions-gateway\.github\.com/tenant}'` → `true`.
- [ ] **Cluster CNI enforces egress NetworkPolicy.** The tenant isolation model (workers restricted to DNS + the per-tenant proxy; no direct GitHub or Kubernetes API egress) is implemented as NetworkPolicy egress rules, which are inert unless the cluster's Container Network Interface (CNI) plugin enforces them.
  Production clusters must run an egress-enforcing CNI such as Calico or Cilium — kind's default kindnet, for example, accepts NetworkPolicy objects without enforcing egress.
  Verify with your CNI's documentation, or run the negative probes in [network-architecture.md § How to Validate Network Isolation](../design/network-architecture.md#how-to-validate-network-isolation) after onboarding: the "blocked" probes must actually time out.
- [ ] **Service mesh is accounted for (if the cluster runs one).** A mesh (Istio, Linkerd, Cilium Service Mesh, Kuma) that injects a sidecar into the tenant namespace will strand completed worker pods and fight the per-tenant egress proxy.
  The supported posture is to opt the GAG tenant namespace out of the mesh; if mesh membership is mandatory, use native sidecars or an ambient/sidecar-less data plane plus egress exclusions.
  Decide this before provisioning — see [Running GAG Alongside a Service Mesh](service-mesh-coexistence.md).
- [ ] **GMC is running.** The Gateway Manager Controller (GMC) is deployed and healthy: `kubectl get deploy -n gmc-system gmc-controller-manager`.
  Install it with the [`actions-gateway` Helm chart](../../charts/actions-gateway/README.md) (`helm install gag charts/actions-gateway -n gmc-system --create-namespace …`).
- [ ] **CRDs are installed.** `kubectl get crd actionsgateways.actions-gateway.github.com && kubectl get crd runnergroups.actions-gateway.github.com`.
- [ ] **GitHub App is registered.** The GitHub App is registered in the target GitHub organization with at least `Actions: Read` and `Administration: Read` permissions.
  The platform team has the `appId`, `installationId`, and private key `.pem` file.
  First time?
  [Step 0](#step-0-create-and-install-the-github-app) walks through creating the App and capturing all three.
- [ ] **GitHub App is installed.** The App is installed on the organization (or specific repos): Settings → Developer settings → GitHub Apps → `<app>` → Install App.
- [ ] **GitHub URL is known.** The org/enterprise/repo URL the runners register against — `https://github.com/<org>`, `https://github.com/<org>/<repo>`, or a GitHub Enterprise Server URL `https://ghes.example.com/<org>`.
  It goes in `spec.gitHubURL` (Step 2) and must match where the App is installed.
  It is a required field — there is no default.
  Everything the control plane addresses is derived from it: the REST API base (`https://api.github.com`, or `https://<host>/api/v3` for GHES), the registration endpoints, and — in an FQDN egress mode — the proxy's hostname allowlist.
- [ ] **GHES only: the appliance's egress ranges are allowlisted.** A GitHub Enterprise Server appliance sits on your own address space, so it appears in none of the ranges `api.github.com/meta` publishes — which is exactly what the default `CIDR` egress mode programs.
  In that mode the tenant's `EgressProxy` must carry the appliance's ranges in `spec.destinationCIDRs`, and because that field is gated by the platform `--allowed-egress-cidrs` allowlist, **a platform admin has to allowlist them first**; a tenant cannot self-serve.
  A pool missing them reports `GitHubEgressIncomplete=True` (reason `ApplianceRangesRequired`) naming the unreachable host.
  An FQDN egress mode needs nothing here — the referring gateway's host is added to the policy automatically.
  See [troubleshooting: GHES tenant's egress is denied](troubleshooting.md#a-ghes-tenants-traffic-never-reaches-the-appliance).
- [ ] **GHES only: the appliance's CA is available, if it is a private one.** Reaching the appliance and trusting it are separate problems.
  The AGC and its worker pods trust the OS system roots plus the egress proxy's own CA, so an appliance fronted by an internal certificate authority fails the TLS handshake — a handshake error, not a timeout.
  Put the CA bundle in a ConfigMap in the tenant namespace under the key `ca.crt` and name it in `spec.githubCABundleRef` (Step 2).
  Unlike the egress ranges above this is tenant-self-serve; no platform allowlist is involved.
  The bundle is additive, so the system roots stay trusted.
  A ref naming a missing or unparseable ConfigMap holds the gateway `Degraded` rather than provisioning an AGC that cannot start.
  See [troubleshooting: a GHES appliance's certificate is not trusted](troubleshooting.md#a-ghes-appliances-certificate-is-not-trusted).
- [ ] **Quota is provisioned (platform-owned).** The tenant's resource requirements have been reviewed and the platform has created a `ResourceQuota` (and any `LimitRange`) on the tenant namespace — CPU, memory, and pod count.
  This is the real, tenant-uncontrollable cap; the gateway operates within it but never creates or mutates it.
  See [Step 1b](#step-1b-set-the-platform-owned-resourcequota).
  (If you provision namespaces and quotas via a GitOps or tenant-operator stack — Capsule, HNC, vCluster, kiosk — the quota comes from there instead.)
- [ ] **PriorityClass objects exist and are allowlisted** (priority-tiered tenants only).
  Any `priorityClassName` a tenant references in `priorityTiers` must (1) be pre-created at the cluster level by the platform (`kubectl get priorityclass`) **and** (2) appear on the GMC `--allowed-priority-classes` flag.
  The GMC validating webhook rejects any `priorityClassName` not on the allowlist (an empty allowlist rejects *all* of them) — this stops a tenant naming a high-priority, preempting class and evicting other tenants' worker pods.
  Create allowlisted classes with `preemptionPolicy: Never` unless cross-tenant preemption is genuinely intended for that tier; see [security-operations.md § Priority classes](security-operations.md#priority-classes-the-allowed-priority-classes-allowlist).
  **If you do enable a preempting tier, tell the tenant what preemption costs them:** a preempted worker's job concludes on GitHub *and* is re-run automatically (Q497), but the re-run starts from the beginning rather than resuming — so a long, expensive job in a displaceable tier pays its whole runtime again.
  Put cheap-to-repeat work in the tiers that can be displaced.
  See [troubleshooting: a preempted worker's job is not re-run](troubleshooting.md#a-preempted-workers-job-is-not-re-run).
- [ ] **`noProxyCIDRs` is left unset unless the tenant has its own internal destination.** The cluster-internal exemptions — `svc.cluster.local`, `kubernetes.default.svc`, `localhost`, `127.0.0.1`, and the cluster's API server ClusterIP — are appended automatically on every distribution, so neither the service CIDR nor the API server needs to be named here (this was not true before Q465; see [troubleshooting: AGC crash-loops dialling the API server through the egress proxy](troubleshooting.md#agc-crash-loops-dialling-the-api-server-through-the-egress-proxy)).
  Add an entry only for a destination the tenant genuinely reaches directly, and keep it as narrow as possible: everything listed bypasses the tenant's egress proxy and so leaves its traffic unattributed.
- [ ] **Security profile decided.** Default `baseline` is correct for normal CI workloads (builds, tests).
  Confirm with the tenant whether they need `restricted` (compliance / high-isolation) or `privileged` (docker-in-docker, kernel-module workflows).
  Tenants with both needs deploy two `ActionsGateway` CRs in two namespaces.
  You can *harden* a profile later in place (`baseline → restricted`) freely, but *relaxing* it (a downgrade) is rejected by admission unless you set the `actions-gateway.github.com/allow-profile-downgrade: "true"` annotation — see [troubleshooting: securityProfile downgrade rejected](troubleshooting.md#securityprofile-downgrade-rejected-by-admission-webhook).
  See [§5.3 — Security Profiles](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in).

  > **v2 API (`actions-gateway.com`):** the security profile is **no longer a field on the `ActionsGateway` CR** — it is a property of the **namespace**, because Pod Security Admission is namespace-scoped (appendix-h §H.16 #7).
  > Instead of `spec.securityProfile`, label the namespace:
  >
  > ```bash
  > kubectl label namespace <tenant-namespace> actions-gateway.com/security-profile=restricted   # baseline | restricted | privileged; absent ⇒ baseline
  > ```
  >
  > The GMC stamps the `pod-security.kubernetes.io/*` labels from it.
  > The downgrade and privileged-eligibility rules are identical to v1 but enforced by the `gmc-namespace-security-profile-guard` ValidatingAdmissionPolicy on the **namespace** (downgrade needs `actions-gateway.com/allow-profile-downgrade: allowed` as a namespace annotation; `privileged` needs `actions-gateway.com/privileged-profile: allowed` as a namespace label).
  > Co-located v2 gateways share the one namespace profile; tenants needing different postures use different namespaces.
- [ ] **Privileged eligibility granted (only if the tenant needs `privileged`).** `securityProfile: privileged` is a **platform** decision, not tenant-settable: the GMC validating webhook rejects it (at create *and* update) unless the namespace carries the eligibility label, applied by a trusted administrator:

  ```bash
  kubectl label namespace <tenant-namespace> actions-gateway.github.com/privileged-profile=allowed
  ```

  The granting value is the enum keyword `allowed`, not `true` — a boolean-looking label value is a YAML footgun.
  This is a separate gate from the `actions-gateway.github.com/tenant` marker (which authorizes GMC management at all): without `privileged-profile`, the tenant can still run `baseline`/`restricted` but not `privileged`.
  The gate is fail-closed — absent the label (or any value other than `allowed`), privileged is refused — so a tenant cannot self-grant the cluster's least-restrictive PSA posture by creating a CR.
  Apply this only for tenants approved for docker-in-docker / kernel-module workloads, ideally paired with a sandbox `runtimeClassName`.
  Verify: `kubectl get namespace <tenant-namespace> -o jsonpath='{.metadata.labels.actions-gateway\.github\.com/privileged-profile}'` → `allowed`.
  To revoke, remove the label (`…/privileged-profile-`).
  See [troubleshooting: privileged securityProfile rejected](troubleshooting.md#privileged-securityprofile-rejected-namespace-not-eligible) and [§5.3](../design/05-security.md#privileged-eligibility-is-a-platform-decision).

---

## Step 0: Create and Install the GitHub App

> **First-time setup.** Skip this step if the platform team already handed you an `appId`, an `installationId`, and the private-key `.pem` file (the "GitHub App is registered" pre-condition).
> Otherwise, this is where those three values come from.
> The output of this step feeds directly into [Step 1](#step-1-create-the-github-app-secret).

GitHub Apps are the gateway's only credential model on the v1 API — there is no Personal Access Token (PAT) path.
This is deliberate: an App yields **short-lived, auto-rotating installation tokens** scoped to the installation and to the App's declared permissions (`Actions: Read` + `Administration: Read` here), so a compromise has a far smaller blast radius than a long-lived PAT carrying its owner's full account access.
The App is also an **automation-owned identity** — it does not break when a user leaves the org or loses access — and installation-level rate/concurrency budgets are what let one tenant scale to thousands of sessions (and shard across installations; see [Appendix E §E.6](../design/appendix-e-capacity-planning.md#e6-when-to-shard-across-installations)).
See [security §5](../design/05-security.md) for the full credential trust model.

There is **no `gh` command to create a GitHub App** (the GitHub CLI has no `gh app create`), so the App is created in the web UI; the GitHub CLI (`gh`) is used afterwards to read back the IDs.

### 0a. Create the App (web UI)

1. Go to the org's App settings: `https://github.com/organizations/<org>/settings/apps` → **New GitHub App**.
   (For an enterprise, use the enterprise settings path; for a user-owned App, Settings → Developer settings → GitHub Apps → **New GitHub App**.)
2. **GitHub App name** — any unique name, e.g. `acme-actions-gateway`.
3. **Homepage URL** — any valid URL (e.g. the repo URL); it is not used by the gateway.
4. **Webhook** — uncheck **Active**.
   The gateway polls GitHub; it does not receive webhooks, so no webhook URL or secret is needed.
5. **Permissions** — grant only the two read-only permissions the gateway needs to register and observe self-hosted runners:
   - **Repository permissions → Actions: Read-only**
   - **Organization permissions → Administration: Read-only** (labelled **Self-hosted runners: Read-only** on some plans)
   - These are the `Actions: Read` and `Administration: Read` permissions referenced throughout this guide.
     Leave **Contents**, **Pull requests**, and everything else at **No access** — the gateway never reads tenant code or writes to repositories.
6. **Where can this GitHub App be installed?** — **Only on this account** is the typical choice for a single-org deployment.
7. Click **Create GitHub App**.

> **Least privilege.** The installation tokens the gateway mints inherit exactly these App permissions.
> Granting more than `Actions: Read` + `Administration: Read` widens the blast radius of a key compromise for no functional gain — see [security §5](../design/05-security.md) and the [runbook key-compromise scope assessment](runbook.md).

### 0b. Capture the `appId`

On the App's **General** page, copy the numeric **App ID** (top of the page).
This is `appId` — a small integer, not the App name or the client ID.

### 0c. Install the App and capture the `installationId`

1. On the App's **Install App** tab, install it onto the target organization (or specific repositories).
   The org/repos you install it on **must match** the `gitHubURL` you set in [Step 2](#step-2-create-the-actionsgateway-resource).
2. After installing, the browser lands on the installation's settings page; the `installationId` is the trailing number in the URL: `https://github.com/organizations/<org>/settings/installations/<installationId>`.

You can also read it back with the GitHub CLI authenticated as the App (advanced — requires an App JWT, not your user token).
The web-UI URL above is the reliable path; the `gh api /app/installations` endpoint is available once you can present an App JWT.

### 0d. Generate and download the private key (`.pem`)

On the App's **General** page → **Private keys** → **Generate a private key**.
The browser downloads a `.pem` file **once** — GitHub never shows it again, so store it safely and treat it as a high-value secret.

**The exact PEM format the controller expects.** The AGC parses the key with Go's standard library and accepts exactly two PEM block types ([`githubapp/auth.go`](../../githubapp/auth.go)):

| First line of the `.pem` | Format | Accepted? |
| --- | --- | --- |
| `-----BEGIN RSA PRIVATE KEY-----` | PKCS#1 (RSA) | ✅ — this is what GitHub downloads |
| `-----BEGIN PRIVATE KEY-----` | PKCS#8 (RSA or Ed25519) | ✅ — e.g. after `openssl pkcs8` conversion |
| `-----BEGIN OPENSSH PRIVATE KEY-----`, `-----BEGIN EC PRIVATE KEY-----`, anything else | other | ❌ — rejected with `unsupported PEM block type` |

GitHub's downloaded key is already PKCS#1 — **use it byte-for-byte; no conversion is required or recommended.**

> **PEM pitfalls — the #1 first-day failure.** The strict PEM format makes hand-editing the key the most common onboarding error.
> The controller surfaces it as `private key: RSA key parse error` / `no PEM block found` and `AGCAvailable=False` (reason `CredentialError`).
> Avoid all of these by never opening or retyping the key:
>
> - **Do not copy-paste the key into a terminal or YAML by hand.** Pasting can drop the trailing newline, re-wrap the base64 body, insert spaces/blank lines, or substitute smart-quotes — any of which breaks parsing.
>   Always feed GitHub's file directly (Step 1's `--from-file`).
> - **Do not open it in an editor that rewrites line endings.** A CRLF (`\r\n`) conversion or a stripped final newline corrupts the block.
>   Keep the file exactly as downloaded.
> - **Do not base64-encode it yourself.** `kubectl ... --from-file` and `stringData` encode the value for you; pre-encoding double-encodes it.
> - **Header/footer must be intact and exact.** The `-----BEGIN …-----` / `-----END …-----` lines must be present and unaltered, with no leading/trailing whitespace or extra blank lines.

You now have the three values [Step 1](#step-1-create-the-github-app-secret) needs: `appId`, `installationId`, and the `.pem` file on disk (referred to below as `app.pem`).

---

## Step 1: Create the GitHub App Secret

Create this in the tenant's namespace.
Use a stable, versioned name (e.g.
`github-app-v1`) to enable clean credential rotation later.
The Secret holds three keys — `appId`, `installationId`, and `privateKey` — which the GMC projects into the AGC pod as files the controller reads at startup (`cmd/agc/main.go`).

**Recommended — create from the downloaded file (secure).** Pass the `.pem` straight into `kubectl` with `--from-file`; this preserves the key byte-for-byte (sidestepping every PEM pitfall above) and never exposes it in a shell argument, an environment variable, or your shell history.
Delete the file as soon as the Secret exists:

```sh
# app.pem is the file downloaded in Step 0d — never echo, cat, or paste its contents.
kubectl create secret generic github-app-v1 \
  --namespace <tenant-namespace> \
  --from-literal=appId='<GitHub App ID>' \
  --from-literal=installationId='<Installation ID>' \
  --from-file=privateKey=app.pem

# Remove the private key from disk now that it lives only in the Secret.
rm -f app.pem
```

> **Why not paste the key into YAML?** The declarative form below requires putting the PEM body into a file on disk that is easy to accidentally commit to git, and inviting a hand-paste that mangles the key.
> Prefer `--from-file`.
> If you must use YAML (e.g.
> GitOps), keep the manifest out of version control or supply the `privateKey` via your secrets manager / sealed-secrets tooling — never commit a plaintext key.

**Alternative — declarative manifest.** Equivalent to the command above; the `--from-file` key name (`privateKey`) maps to the `stringData` key here:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: github-app-v1
  namespace: <tenant-namespace>
type: Opaque
stringData:
  appId: "<GitHub App ID>"
  installationId: "<Installation ID>"
  privateKey: |
    -----BEGIN RSA PRIVATE KEY-----
    <contents of the .pem file>
    -----END RSA PRIVATE KEY-----
```

Both PKCS#1 (`-----BEGIN RSA PRIVATE KEY-----`, the format GitHub downloads) and PKCS#8 (`-----BEGIN PRIVATE KEY-----`, RSA or Ed25519) are accepted — paste whichever your `.pem` file contains, header and footer lines included.

```sh
kubectl apply -f secret.yaml
```

**Verify:**
```sh
kubectl get secret github-app-v1 -n <tenant-namespace>
kubectl get secret github-app-v1 -n <tenant-namespace> \
  -o jsonpath='{.data.privateKey}' | base64 -d | head -1
# Expected: -----BEGIN RSA PRIVATE KEY----- (PKCS#1)
#      or: -----BEGIN PRIVATE KEY----- (PKCS#8, RSA or Ed25519)
```

If the AGC later reports `CredentialError` / `RSA key parse error`, the key was altered in transit — see [troubleshooting: GitHub App Secret misconfiguration](troubleshooting.md#github-app-secret-misconfiguration).

---

## Step 1b: Set the Platform-Owned ResourceQuota

The namespace `ResourceQuota` (and any `LimitRange`) is **platform-owned** — it is not a field on the `ActionsGateway` CR.
The platform admin creates and manages it on the tenant namespace, and the gateway operates within it but never creates or mutates it.
This is deliberate: a tenant-authored quota would be no real cap (the tenant could raise it in their own CR), and owning quotas would force broad, cluster-wide write RBAC on the GMC.
Apply it with a trusted (administrator) identity:

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: <tenant>-quota
  namespace: <tenant-namespace>
spec:
  hard:
    requests.cpu: "20"
    requests.memory: "40Gi"
    pods: "50"
```

```sh
kubectl apply -f resourcequota.yaml
```

If you already provision namespaces and quotas through a GitOps pipeline or a tenant operator (Capsule, HNC, vCluster, kiosk), set the quota there instead — the gateway will not fight it.

**Size the quota for both pools at full scale.** The quota must leave room for the proxy pool at `spec.proxy.maxReplicas` *and* worker pods up to `maxWorkers` (each × its per-pod requests/limits, plus pod count).
When the remaining headroom can't cover scaling to those ceilings, the gateway flags it without blocking provisioning: the GMC raises `ProxyQuotaPressure`/`ProxyQuotaExceeded` on the `ActionsGateway` and the AGC raises `WorkerQuotaPressure`/`WorkerQuotaExceeded` on each `RunnerGroup` (each also exported as a gauge for alerting).
See [troubleshooting: Proxy Pool Not Scaling](troubleshooting.md#proxy-pool-not-scaling) and [Jobs Failing Due to Namespace ResourceQuota Exhaustion](troubleshooting.md#jobs-failing-due-to-namespace-resourcequota-exhaustion).
On the v2 (`actions-gateway.com`) API a pool that opts out of the managed HPA (`EgressProxy.spec.managedAutoscaling: false`) makes `maxReplicas` inert — size the proxy term to *your own* autoscaler's ceiling instead, and note `ProxyQuotaPressure` then measures headroom to the Deployment's current desired replicas.

#### Deriving the numbers

[**Sizing the platform-owned `ResourceQuota`**](resourcequota-sizing.md) is the full derivation: per-pod footprint → concurrency → the control-plane terms, with a worked example you can copy.
Read it before setting the numbers for a real tenant — the three things it exists to stop you getting wrong are:

- **Native sidecars count in full.** A worker's quota footprint is *not* just its `podTemplate.spec.containers`.
  The Docker-in-Docker (DinD) daemon is a native sidecar (`restartPolicy: Always` init container), and Kubernetes sums its whole ask into the pod.
  On the reference DinD shape that is 25% of the pod's CPU request and 75% of its memory request.
- **Kata workers carry `RuntimeClass` pod overhead** (`250m` / `160Mi` per pod), added to requests in full and to limits only for a key the pod already limits, plus one PVC and its `requests.storage` per worker if the shape uses a generic ephemeral volume.
- **Constraining a key makes it mandatory for every pod.** The measured DinD shapes declare no CPU limit, so a quota constraining `limits.cpu` rejects 100% of worker pods with `must specify limits.cpu for: runner`.
  On v1 the same trap applies to `requests.cpu` and the AGC pod, which stamps no resources — pair the quota with a `LimitRange`, or constrain only `pods`.

---

## Step 2: Create the ActionsGateway Resource

Apply the `ActionsGateway` CR in the tenant's namespace.
Adjust `proxy` and `runnerGroups` for the tenant's workload.
The namespace quota is set separately on the namespace (Step 1b), not on this CR.

**One `ActionsGateway` per namespace.** The admission webhook rejects a second `ActionsGateway` in a namespace that already has one — every per-tenant resource has a fixed, namespace-scoped name, so two CRs would contend over them and flap the namespace's PSA labels.
To run a second logical gateway (e.g. a `privileged` profile alongside a `baseline` one), give it its own namespace.
See [troubleshooting: second ActionsGateway rejected](troubleshooting.md#second-actionsgateway-in-a-namespace-rejected-singleton-guard).

```yaml
apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: <tenant>-gateway
  namespace: <tenant-namespace>
spec:
  gitHubAppRef:
    name: github-app-v1
  # GitHub org/enterprise/repo URL the runners register against (required). Use an
  # org URL (https://github.com/my-org) for org-wide runners, a repo URL
  # (https://github.com/my-org/my-repo) to scope to one repo, or your GitHub
  # Enterprise Server URL (https://ghes.example.com/my-org). The App referenced by
  # gitHubAppRef must be installed on this same org/enterprise.
  gitHubURL: https://github.com/my-org
  # Default: blocks privileged containers, host namespaces, hostPath, dangerous caps.
  # Set to "restricted" for stricter isolation, or "privileged" only if the workload
  # genuinely needs an unrestricted PodSpec (DinD, Buildah without sandbox, kernel modules).
  # "privileged" requires the namespace to be eligible: a platform admin must label it
  # actions-gateway.github.com/privileged-profile=allowed (see Pre-Conditions), else the webhook
  # rejects the CR at create/update. It is deliberately not tenant-settable.
  # A privileged worker container (securityContext.privileged: true in a podTemplate)
  # is ONLY admitted under securityProfile: privileged — under baseline/restricted the
  # webhook rejects it. See troubleshooting: privileged worker container rejected.
  securityProfile: baseline
  # Log verbosity for this tenant's AGC and egress proxy: info (default) or debug.
  # Leave at info; flip to debug only for a bug repro (see "Per-tenant log level"
  # below). Changing it is a rolling restart of the AGC and proxy, not a hot reload.
  logLevel: info
  proxy:
    # minReplicas is a floor, not a replica count: the GMC applies it when the pool
    # is first created (or has been scaled to zero) and passes it to the pool's HPA.
    # Above the floor, .spec.replicas belongs to the HPA — raising minReplicas on a
    # running pool takes effect through the HPA, which needs a healthy metrics-server.
    # See troubleshooting: "Proxy Pool Never Scales Out".
    minReplicas: 2
    maxReplicas: 10
    # Optional: noProxyCIDRs excludes internal destinations from the egress proxy.
    # Entries may be CIDRs (10.0.0.0/8), bare IPs, or NO_PROXY domain suffixes
    # (svc.cluster.local, internal.example.com). Admission rejects any entry that
    # would route this tenant's GitHub traffic around the proxy — a hostname
    # matching the gitHubURL host or the public GitHub domains (github.com,
    # githubusercontent.com, ghcr.io) — since that breaks egress-IP attribution.
    # Never list GitHub here. The cluster-internal exemptions are appended
    # automatically — svc.cluster.local, kubernetes.default.svc, localhost,
    # 127.0.0.1, and this cluster's API server ClusterIP — so neither the API
    # server nor the service CIDR belongs here on any distribution.
    # noProxyCIDRs: ["10.0.0.0/8"]
  # The namespace ResourceQuota is platform-owned and set on the namespace in
  # Step 1b — it is not a field on this CR.
  # A runner group has no `name` field — the RunnerGroup CR name is derived from
  # the gateway name + the group's first runnerLabel (here "linux").
  runnerGroups:
    - runnerLabels: ["linux", "self-hosted"]
      maxListeners: 10
      maxWorkers: 20
      podTemplate:
        spec:
          containers:
            - name: runner
              resources:
                requests:
                  cpu: "1"
                  memory: "2Gi"
```

```sh
kubectl apply -f actionsgateway.yaml
```

**Optional — worker-pod lifecycle.** Each `runnerGroups[]` entry accepts two cleanup knobs.
`completedPodTTL` (default `5m`) is how long a finished worker pod (Succeeded/Failed) is kept before the AGC deletes it — the retention window is your chance to `kubectl logs`/`describe` a failed pod; `"0s"` deletes pods immediately on completion.
`pendingPodDeadline` (default `10m`, minimum `1s`) is how long a worker pod may sit Pending (unpullable image, unschedulable constraints) before the AGC deletes it and frees the concurrency slot it was holding — raise it above your worst-case node-autoscaling time for GPU pools, e.g.:

```yaml
  runnerGroups:
    - runnerLabels: ["self-hosted", "gpu"]
      completedPodTTL: "30m"      # longer debugging window for failed jobs
      pendingPodDeadline: "30m"   # GPU node provisioning can exceed the 10m default
```

A third cleanup arm has no knob: on the ScaleSet protocol, a worker pod still `Running` five minutes after GitHub reports its job terminal is deleted, because it is a worker that never received its job (or one held open by a container that outlived the runner) and it is holding a slot for nothing.
It emits a `WorkerPodOrphanedRunning` Warning Event — see the [runbook](troubleshooting.md#worker-pod-reaped-while-running-workerpodorphanedrunning).
A pod whose job is still assigned is never touched, however long it runs.

A reaped Pending pod emits a `WorkerPodStuckPending` Warning Event on the RunnerGroup and cancels the job (it never started); see [troubleshooting: worker pod reaped while Pending](troubleshooting.md#worker-pod-reaped-while-pending-workerpodstuckpending).

**Per-job Secrets are reclaimed on job completion, not on `completedPodTTL`.** Alongside each worker pod the AGC stages one short-lived Secret holding that job's credentials.
It is deleted as soon as the job finishes — independent of `completedPodTTL`, which retains only the pod — so `kubectl get secrets -n <tenant-ns>` shows Secrets for in-flight jobs only, never a backlog proportional to jobs run.
Both objects also carry an `OwnerReference` to their RunnerGroup/RunnerSet as a backstop, so deleting the owner cascade-deletes anything still present.
If you do see `job-` or `job-ss-` Secrets outliving their pods, check the AGC log for `reclaim completed job's worker Secret` warnings — a persistently failing delete (usually an RBAC regression on `secrets`) falls back to that cascade-GC and leaves credentials in the namespace until the owner is deleted.

**One small ConfigMap per ScaleSet RunnerSet is normal.** Alongside the pods and Secrets, a `ScaleSet`-protocol set keeps a `scaleset-guards-<runnerset>` ConfigMap (labelled `actions-gateway.com/runner-set=<name>`): the AGC's durable record of jobs it has concluded whose queue messages are not yet deleted, which is what stops a hard-killed AGC from re-running a finished job's assignment (Q606).
It is bounded by in-flight work, owner-ref'd to its RunnerSet so it is garbage-collected with it, and never something to edit: a hand-edited one fails the listener start; see [troubleshooting](troubleshooting.md#scaleset-runnerset-stuck-not-ready-scalesetlistenerstartfailed-naming-the-guard-configmap).

**Optional — worker scale-up rate limit (`scaleUp`, opt-in, default-off).** Each `runnerGroups[]` entry accepts an optional `scaleUp` token bucket that caps the **rate** at which the AGC *creates* new worker pods.
It is **off by default** — omit it and provisioning stays immediate (GAG's zero-idle default).
It is **not** the same as `maxWorkers`: `maxWorkers` caps how *many* worker pods run at once (a ceiling), while `scaleUp` caps how *fast* they start (a ramp).
Reach for it only when a burst of simultaneously-acquired jobs stampedes a shared, rate-sensitive **egress** path — a NAT/SNAT gateway, a stateful firewall's connection-tracking table, or a site-to-site VPN — where the *onset* of connections (not the steady-state count) is what causes damage.
It deliberately defers work, trading time-to-pickup for a gentler ramp.

```yaml
  runnerGroups:
    - runnerLabels: ["self-hosted", "linux"]
      maxWorkers: 200            # ceiling: at most 200 concurrent worker pods
      scaleUp:
        maxPerSecond: 10         # ramp: sustained ≤10 new worker pods/second…
        burst: 20                # …after an initial instantaneous batch of 20
```

- `maxPerSecond` (required, ≥1): the sustained pod-creation rate once the burst is spent.
- `burst` (optional, ≥1): the largest instantaneous batch before throttling engages; **defaults to `maxPerSecond`** (one second's worth) when omitted.

When the bucket is empty the AGC **withholds intake** rather than delaying a job it has already taken on: it declines to claim the next job, leaving it queued at GitHub for redelivery, and no job ever waits for a token while holding a GitHub job lock (Q717).
That is what makes a low `maxPerSecond` safe against a large burst — the ramp defers *unclaimed* jobs, so nothing sits on a lock for the ramp's duration and no job is cancelled by a lock lapsing part-way through.
Each withheld job increments `actions_gateway_worker_scaleup_throttled_total{namespace, runner_group}`; a sustained non-zero rate means the ramp is actively smoothing a burst (see [observability: metrics](observability.md)).
Set `maxPerSecond` high enough that jobs are picked up promptly once the burst passes: the cost of a low rate is now time-to-pickup rather than a held lock.

**Pick the right tool for the stampede.** A scale-up rate limit is the wrong fix for some bursts:

| Burst symptom | Use instead |
|---|---|
| Every cold node re-pulls the large runner **image** | Peer-to-peer image mirror / pre-pull DaemonSet ([p2p-image-distribution.md](p2p-image-distribution.md)) — a ramp still pulls N times, just spread out |
| Workers stampede a shared **egress** path (NAT/firewall/VPN) *at onset* | **`scaleUp`** (this knob), often alongside workflow-level `concurrency:` |
| Sustained saturation of egress **bandwidth/ports** for the whole run | `maxWorkers` **ceiling** (a ramp only defers the cliff), plus more capacity |
| One workflow drains the **whole shared quota** and starves others | `maxWorkers` / priority tiers (a fairness ceiling, already shipped) |

It coexists with the cluster autoscaler / Karpenter: bounding pod-admission rate eases the node-scale-up burst they react to, and they keep their own independent rate controls.

> **v2 (`RunnerSet`)** carries the same `spec.scaleUp` field with identical semantics.
> A `ScaleSet`-protocol set expresses it as a smaller advertised capacity per long-poll rather than as a per-job refusal, so GitHub never assigns the jobs in the first place; the ramp is the same and the accounting appears as `actions_gateway_scaleset_capacity_withheld{reason="scaleup"}`.

**Optional — refuse jobs the cluster can't place (`capacityGate`, v2 only, opt-in, default-off).** A `RunnerSet` accepts an optional `spec.capacityGate` that adds a *placeability* rung to the pre-claim admission gate.
Without it, a set whose worker shape has become unplaceable — a drained node pool, a changed taint, spot capacity gone — keeps taking on jobs, and each one spends a single-use runner registration, holds the GitHub job lock until `pendingPodDeadline`, and ends as a **cancelled** workflow run.
With it, those jobs stay queued at GitHub instead.

```yaml
spec:
  capacityGate:
    mode: Observe   # Off (default) | Observe — Observe gates, it is not a dry-run tier
```

**You turn it on; the platform decides what it reads.** "Can this pod be placed" has two different sound answers depending on whether anything is waiting on the unplaceable pod to make capacity appear — and that is a fact about the *cluster*, not about your runner set.
So the gate takes one input from each party, and a runner set cannot choose a signal that is wrong for the cluster it runs in:

| Set by | Field | Says |
|---|---|---|
| The **tenant**, per runner set | `RunnerSet.spec.capacityGate.mode` | Whether this set should refuse work it cannot run. |
| The **platform operator**, per gateway | `ActionsGateway.spec.clusterCapacity.nodeAutoscaling` | Whether anything in this cluster adds nodes. |

The second selects the signal:

| `nodeAutoscaling` | The gate refuses intake when | Set it when |
|---|---|---|
| `Present` (default) | The **cluster autoscaler itself** records, on a stuck worker pod, that it will not add a node for it. | Your cluster **can grow**: cluster autoscaler (including GKE's, EKS's, AKS's, OpenShift's `MachineAutoscaler`) or Karpenter / AKS Node Auto Provisioning. |
| `Absent` | The **scheduler** reports the set's worker pods `Unschedulable` past the scheduling grace. | Your cluster **cannot grow**: a fixed-size or on-prem cluster with no autoscaler. Nothing is waiting on that Pending pod, so it is pure waste. |

```yaml
# On the ActionsGateway — platform-operator config, not tenant config.
spec:
  clusterCapacity:
    nodeAutoscaling: Absent   # Present (default) | Absent
```

**The default is the safe direction, not the common one.** Under `Present` the gate refuses intake only on an explicit autoscaler declination, so leaving it unset — or getting it wrong — can only ever *under*-gate, which is the behaviour you already have.
`Absent` gates on the scheduler's verdict alone, so setting it on a cluster that *can* grow refuses jobs on pods the autoscaler was about to rescue.
**Only set `Absent` if you own the node contract and it is fixed.** The AGC never infers this: an autoscaler is legitimately silent during backoff, during a cooldown after a failed scale-up, or for a pod it filters out, so "no autoscaler events appeared" is absence of evidence, and reading it as evidence of absence would starve a tenant.

Three things to know about the `Present` (elastic) signal specifically:

- **Recognition is narrow on purpose.** It reads cluster autoscaler's `NotTriggerScaleUp` and Karpenter's `FailedScheduling`, and nothing else.
  `FailedScheduling` counts only when it came from a reporter that is **not** your scheduler — that reason is kube-scheduler's own for every ordinary transient placement failure, and reading those as declinations would refuse jobs your cluster was about to run.
  A commercial optimizer (CAST AI, Spot Ocean, Zesty) emits its own vocabulary, so the gate simply never closes for it, which is today's behavior.
- **It reads Events, and Events expire.** The AGC reads a stuck pod's Events directly (no Event watch, and only for pods already past the scheduling grace).
  Events are garbage-collected on the apiserver's `--event-ttl`, commonly one hour; that is well outside the `pendingPodDeadline` window the gate consults, and an expired Event means the gate opens rather than errors.
- **It needs `get`/`list` on `events` in the tenant namespace.** The shipped `agc-tenant-role` grants them.
  On a hand-built Role without them, the gate fails open — it never closes — rather than erroring.

Two things to expect once a set turns it on:

- **It throttles, it does not seal.** The signal comes from a stuck worker pod, so the reaper deleting that pod at `pendingPodDeadline` reopens intake for one job; if the shape is still unplaceable, that job's pod closes the gate again.
  Budget for roughly one wasted claim per `pendingPodDeadline` window, not zero.
- **A gated set says so.** It reports `WorkerCapacityDeclined=True` with a Warning event, and either `actions_gateway_scaleset_capacity_withheld{reason="capacity"}` (default tier) or `actions_gateway_jobs_admission_rejected_total{reason="capacity"}` (classic) moves.
  Diagnosis and the one-line rollback: [troubleshooting](troubleshooting.md#runnerset-reports-workercapacitydeclined-the-gateway-stopped-claiming-jobs).

Everything about it fails **open**: an unreadable set, an unresolved gateway or template, an unreadable pod list, an unreadable Event list, an unset `clusterCapacity`, and a mode this AGC does not implement all leave intake exactly as it is today. v1 `RunnerGroup` has no equivalent field and never will — v1 is terminal.

**Changing `runnerGroups` later.** Editing `spec.runnerGroups` on an existing `ActionsGateway` reconciles to the desired set: added entries create new `RunnerGroup` CRs, and **removing an entry deletes its `RunnerGroup`** (which stops its listeners and cascades to its worker pods).
Reordering entries is safe — the GMC keys pruning on owner labels, not list position, so a reorder never deletes or recreates a group.
Removing an entry is the way to retire a runner group; `maxListeners` has a minimum of `1`, so there is no in-place scale-to-zero.

**Worker image — the default works out of the box.** A plain install runs jobs with no `workerImage` set: the AGC **injects** GAG's wrapper into every worker pod — a read-only OCI image volume on Kubernetes ≥ 1.33, an initContainer below that — so the runner image itself can be the unmodified upstream `ghcr.io/actions/actions-runner` (the default) or **any `actions/runner`-derived image**.
The wrapper is what feeds the mounted job payload + JIT config into `Runner.Worker`; injecting it means the runner image no longer has to carry it.
Set `workerImage` only to use a **custom** image (your own tools, a pinned digest) — the wrapper is injected into that too.
The upstream `actions-runner` (and GAG's images built on it) run as UID 1001, so on every profile except `privileged` the AGC stamps `runAsNonRoot: true` and gap-fills `runAsUser: 1001` automatically.
If you point `workerImage` at a **custom** image whose user is **not** UID 1001 — a different named user, or one that runs as root — set `securityContext.runAsUser` (or `runAsNonRoot: false` for a root-based image) on the runner container in the `podTemplate`; otherwise kubelet rejects the pod with `CreateContainerConfigError`.
See [troubleshooting: worker pod fails to start after secure-by-default SecurityContext](troubleshooting.md#worker-pod-fails-to-start-after-secure-by-default-securitycontext).

**Building a build-capable `workerImage`.** The default upstream `actions-runner` is deliberately minimal — it ships the runner agent but **no build toolchain** (`make`, a C compiler, language SDKs).
A job that shells out to `make` on it fails `exit 127: make: command not found`, where the GitHub-hosted `ubuntu-latest` image would have had those tools preinstalled.
The fix is the same as on Actions Runner Controller (ARC): build your own image `FROM` the upstream runner with the tools your jobs need, and set it as `workerImage`.
Because the AGC injects the wrapper on top of **any** base, your image carries only your toolchain — nothing GAG-specific:

```dockerfile
FROM ghcr.io/actions/actions-runner:<pinned version>@sha256:<digest>
USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential \
    && rm -rf /var/lib/apt/lists/*
USER runner   # keep the non-root UID 1001 the AGC expects
```

A working reference you can copy and extend lives at [`scripts/dogfood/runner/Dockerfile`](../../scripts/dogfood/runner/Dockerfile) (built by [`scripts/dogfood/runner-build.sh`](../../scripts/dogfood/runner-build.sh)); it adds just enough to run a `make`-based Go CI.
**It is a reference example, not an officially supported image** — GAG signs and CVE-scans only its six first-party images, so a runner image you ship (or copy from the example) is yours to pin by digest and scan.
Keep the runner version in step with the default the AGC would otherwise inject.
GitHub refuses to register a runner below its enforced minimum, currently `2.329.0`, and separately requires each new runner release be installed within 30 days of publication to keep executing jobs.

**Tag your image with the runner version it ships.** The AGC reads the runner version off the `workerImage` reference and reports the verdict as the `RunnerVersionTooOld` condition on the `RunnerGroup`/`RunnerSet`:

| Condition | Reason | What it means |
|---|---|---|
| `True` | `WorkerImageBelowMinimum` | The tag names a runner version below GitHub's enforced minimum. Update `workerImage`; the set counts as impaired and rolls up to the gateway. |
| `False` | `WorkerImageCurrent` | The tag names a version at or above the minimum. This checks the registration floor only; it does not prove the image is inside the 30-day window. |
| `Unknown` | `WorkerImageVersionUnknown` | The reference names no runner version, so nothing has been checked: a digest-only ref, or a tag of your own such as `:v3-cuda`. Re-tag as `<something>:2.335.1` to get a verdict, or read the runner version from the worker pod's own log line (below). |

A digest-only or custom tag is not a failure, just an unchecked one.
The pinned-digest advice above still stands, and `myrepo/runner:2.335.1@sha256:…` satisfies both.

**Reading the version a running worker actually shipped.** Whatever the tag says, the wrapper logs the runner version it found inside the image once per worker pod:

```bash
kubectl logs -n <namespace> <worker-pod> -c runner | grep "runner version"
```

`runner version detected version=2.335.1` is the real answer for a custom image; `runner version not detected` means the image is not `actions/runner`-derived in the expected layout.

**Optional — distributed tracing.** To send the AGC's OpenTelemetry traces to a collector, add a `spec.tracing` block.
Setting `endpoint` is what turns tracing on; leave the block out to keep it off (the default).
`sampler` is a fixed enum — an unrecognized value is rejected by admission (see [troubleshooting: tracing sampler rejected](troubleshooting.md#tracing-sampler-rejected-by-admission)).

```yaml
spec:
  tracing:
    endpoint: https://otel-collector.observability:4317
    sampler: parentbased_traceidratio   # optional
    samplerArg: "0.1"                    # optional — sample 10% of traces
    resourceAttributes:                  # optional
      deployment.environment: prod
    # insecure: true   # only for a plaintext in-cluster collector; TLS is the default
```

There is no field for OTLP auth headers: collector authentication is a network-layer concern (in-cluster collector, mutual TLS, or a service mesh), not a CR secret.
See [observability — enabling tracing on GMC-managed AGCs](observability-logging.md#enabling-tracing-on-gmc-managed-agcs).

<a id="per-tenant-log-level"></a> **Optional — per-tenant log level.** `spec.logLevel` sets the log verbosity of this tenant's AGC and egress proxy: `info` (the default) or `debug`.
The GMC threads it to both workloads as the `LOG_LEVEL` environment variable, so you can crank one gateway to `debug` for a bug repro without redeploying the GMC or touching any other tenant:

```sh
kubectl patch actionsgateway -n <tenant-namespace> <name> \
  --type merge -p '{"spec":{"logLevel":"debug"}}'
# ...reproduce the issue, read the debug logs, then revert:
kubectl patch actionsgateway -n <tenant-namespace> <name> \
  --type merge -p '{"spec":{"logLevel":"info"}}'
```

- **The default is `info`, never `debug`.** A CR that omits the field — or sets it back to `info` — runs at `info`.
  At thousands of concurrent sessions the per-session/per-job `debug` lines dominate log volume, so `debug` is a deliberate, temporary opt-in, not a steady state.
- **Changing it is a rolling restart, not a hot reload.** The new level takes effect once the AGC and proxy pods roll (the value is part of their pod templates).
  Expect the AGC's listener pool to drain and re-establish; in-flight jobs finish on the old pod within its termination grace period.
- `debug` surfaces the AGC's per-session → per-job → per-pod lifecycle lines (the listener/multiplexer/provisioner traces, each carrying `namespace`/`group`/`sessionId`/`podName` correlation fields) and the proxy's per-CONNECT detail.
  The grep anchors are in [observability — debug diagnostics](observability-logging.md#debug-diagnostics-for-otherwise-silent-paths).
- Only `info` and `debug` are accepted; admission rejects any other value.
- **v2 splits the knob per kind.** On the v2 (`actions-gateway.com`) API, `ActionsGateway.spec.logLevel` covers the AGC only; a proxy pool carries its own `EgressProxy.spec.logLevel` (same `info` | `debug` values and default, same rolling-restart semantics), so a shared pool can be flipped to `debug` without touching any gateway:

  ```sh
  kubectl patch egressproxy -n <tenant-namespace> <name> \
    --type merge -p '{"spec":{"logLevel":"debug"}}'
  ```

---

## Step 3: Validate Provisioning

The GMC provisions all tenant resources within ~30 seconds of CR creation.

```sh
# Check the ActionsGateway conditions
kubectl get actionsgateway -n <tenant-namespace> <name> \
  -o jsonpath='{.status.conditions}' | jq .

# Expected conditions:
#   Ready=True
#   AGCAvailable=True
#   ProxyAvailable=True
#   ProxyQuotaPressure=False  (True warns the proxy can't scale to maxReplicas within the ResourceQuota)
#   ProxyQuotaExceeded=False  (True means proxy replica creates are being rejected by the ResourceQuota)
```

```sh
# Confirm the AGC Deployment is running
kubectl get deploy -n <tenant-namespace> actions-gateway-controller
# Expected: READY 1/1

# Confirm the proxy pool is running
kubectl get deploy,hpa -n <tenant-namespace>
# Expected: proxy Deployment READY >= minReplicas, HPA TARGETS shows a percentage (not <unknown>)

# Confirm RunnerGroup CRs were created
kubectl get runnergroup -n <tenant-namespace>

# Confirm RBAC was created
kubectl get serviceaccount,role,rolebinding -n <tenant-namespace> | grep actions-gateway

# Confirm NetworkPolicies and ResourceQuota were applied
kubectl get networkpolicy,resourcequota -n <tenant-namespace>
# Expected NetworkPolicies (3):
#   actions-gateway-workload — restricts AGC and worker pods to proxy + DNS
#   actions-gateway-controller      — adds Kubernetes API server egress for the AGC only
#   actions-gateway-proxy    — restricts proxy pods to GitHub CIDRs + DNS

# Confirm the Pod Security Admission label matches the chosen securityProfile
kubectl get namespace <tenant-namespace> \
  -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}{"\n"}'
# Expected: baseline (default), or restricted / privileged if explicitly chosen
```

**If `TARGETS: <unknown>` on the HPA:** `resources.requests.cpu` is not set on proxy pods.
Add it to `spec.proxy.resources.requests.cpu` in the `ActionsGateway` spec.
See [Troubleshooting — Proxy Pool Not Scaling](troubleshooting.md#proxy-pool-not-scaling).

---

## Step 4: Validate Listener Sessions

The AGC should begin polling GitHub within seconds of starting.

```sh
# Check AGC logs for session registration
kubectl logs -n <tenant-namespace> deploy/actions-gateway-controller --tail=30
# Look for: "session registered" or "starting listener goroutine"

# Check the active sessions metric
# Metric: actions_gateway_active_sessions{namespace="<tenant-namespace>"}
# Expected: 1 per RunnerGroup (e.g. 1 if one RunnerGroup is defined)
```

If sessions are not appearing:
- Check for token errors: `kubectl logs ... | grep "token refresh"`.
- Check proxy connectivity: see [Troubleshooting — AGC CrashLoopBackOff](troubleshooting.md#agc-crashloopbackoff-or-not-acquiring-jobs).

---

## Step 5: Run a Test Job

Have the tenant run a workflow in their repository targeting the registered labels.

Example workflow:
```yaml
name: Runner connectivity test
on: workflow_dispatch
jobs:
  test:
    runs-on: [self-hosted, linux]
    steps:
      - run: echo "Runner is healthy. Host $(hostname)"
```

Trigger from the GitHub Actions UI or:
```sh
gh workflow run "Runner connectivity test" --repo <org>/<repo>
```

Watch for the job to be acquired and a worker pod to appear:
```sh
# Watch for worker pod creation
kubectl get pods -n <tenant-namespace> -w

# Check jobs acquired metric
# Metric: actions_gateway_jobs_acquired_total{namespace="<tenant-namespace>"}
# Expected: increments by 1

# Check pod creation latency
# Metric: actions_gateway_pod_creation_latency_seconds
# Expected: well under the 15s p95 SLO
```

---

## Success Criteria

Onboarding is complete when:

- [ ] `ActionsGateway` has `Ready=True` condition.
- [ ] HPA `TARGETS` shows a CPU percentage (not `<unknown>`).
- [ ] `actions_gateway_active_sessions` is ≥ 1 per RunnerGroup.
- [ ] At least one test job has completed successfully in the GitHub Actions UI.
- [ ] Worker pod was created and deleted after job completion.
- [ ] No errors in AGC logs during the test job.

---

## Common First-Day Mistakes

| Symptom | Cause | Fix |
|---------|-------|-----|
| `ActionsGateway` condition `AGCAvailable=False`, logs show `RSA key parse error` | Private key has trailing whitespace or incorrect PEM format | Recreate the Secret; ensure the key starts with `-----BEGIN RSA PRIVATE KEY-----` (PKCS#1) or `-----BEGIN PRIVATE KEY-----` (PKCS#8, RSA or Ed25519) and has no extra blank lines or spaces |
| `HPA TARGETS: <unknown>` | `proxy.resources.requests.cpu` not set | Add `requests.cpu: "10m"` under `spec.proxy.resources.requests` |
| Worker pods stuck `Pending` | `ResourceQuota` exhausted or no schedulable nodes | Check `kubectl describe resourcequota -n <namespace>` and node capacity |
| `RunnerGroup`/`RunnerSet` condition `RunnerVersionTooOld=True`, reason `WorkerImageBelowMinimum` | The `workerImage` tag names a runner version below GitHub's enforced minimum | Update `workerImage` to a runner `2.329.0` or later |
| `RunnerVersionTooOld=Unknown`, reason `WorkerImageVersionUnknown` | The `workerImage` reference names no runner version, so nothing was checked | Tag the image with the runner version it ships, or read the worker pod's `runner version detected` log line |
| Test job stays queued in GitHub for >2 minutes | `active_sessions = 0` — listener goroutines are not running | Check AGC logs for credential or proxy errors |
| HPA present but proxy doesn't scale up | `maxReplicas` too low or HPA metric is `<unknown>` | Check both the HPA spec and that `requests.cpu` is set |
| Proxy stuck below `maxReplicas`; `FailedCreate ... exceeded quota` events | `proxy.maxReplicas` exceeds the namespace `ResourceQuota` | Check the `ProxyQuotaPressure` condition (`kubectl describe actionsgateway …`); raise the quota or lower `maxReplicas` |
| Jobs acquired but pods not appearing | `priorityClassName` referenced in `priorityTiers` does not exist | `kubectl get priorityclass <name>` — create it if missing |
| `ActionsGateway` apply rejected: `priorityClassName … is not in the platform allowlist` | The named `PriorityClass` is not on the GMC `--allowed-priority-classes` flag (the allowlist is empty by default) | Have the platform admin create the `PriorityClass` and add its name to `--allowed-priority-classes`; see [security-operations.md § Priority classes](security-operations.md#priority-classes-the-allowed-priority-classes-allowlist) |
| `EgressProxy`/`ActionsGateway` apply rejected: `spec.scheduling.priorityClassName … is not in the platform infra allowlist` | The class named for an **infra** pod is not on the infra allowlist — the GMC `--allowed-infra-priority-classes` flag plus the `PriorityClassAllowlist` CR's `allowedInfraPriorityClasses` (a *separate* allowlist from the worker one, empty by default) | Have the platform admin create the `PriorityClass` and add its name to `spec.allowedInfraPriorityClasses` on the CR (restart-free) or to the flag, kept disjoint from the worker allowlist either way; see [security-operations.md § Infra pods](security-operations.md#infra-pods-the-separate-allowed-infra-priority-classes-allowlist) |

---

## v2 API: multiple gateways per namespace

> **Audience:** Platform engineer onboarding a tenant on the **`actions-gateway.com`** API, at **`v2beta1`** — the graduated, ScaleSet-only storage and hub version, and the version every **new** tenant should use.
> It is served *beside* `v1alpha1`, so everything above (the deprecated `actions-gateway.github.com/v1alpha1` flow) keeps working while you adopt it.
> `v2alpha1` is also still served, but only as the [`gag-migrate`](migration-v1-to-v2.md) on-ramp for tenants moving off v1: it carries the deprecated [`acquisitionProtocol`](#acquisition-protocol-v2alpha1-only) selector, which a new tenant does not need.
> `v2alpha1` is itself deprecated and [removed at `v2.0.0`](v1alpha1-deprecation.md); `v2beta1` is not.
> Install the opt-in `actions-gateway-crds-v2` chart first; see [Getting Started — Deploy the GMC](../getting-started.md#1-deploy-the-gmc).

The biggest onboarding change in v2 is that a single namespace may hold **multiple `ActionsGateway`s**, lifting the v1 one-gateway-per-namespace rule ([Step 2](#step-2-create-the-actionsgateway-resource)).
What that changes when onboarding a v2 tenant:

- **Per-gateway resource naming.** Every resource a gateway derives is prefixed with the gateway name — `<gateway>-agc` (AGC Deployment / ServiceAccount / RoleBinding / Service), `<gateway>-worker` (worker ServiceAccount), `<gateway>-workload` (workload NetworkPolicy), and so on — so two gateways in one namespace never contend over a fixed name.
  List one gateway's resources with `kubectl get all,networkpolicy,secret -n <tenant-namespace> -l actions-gateway.com/gateway=<gateway>`.
- **52-character name cap.** Any v2 CR (`ActionsGateway`, `RunnerSet`, `RunnerTemplate`, `ClusterRunnerTemplate`, `EgressProxy`) whose `metadata.name` exceeds **52 characters** is rejected at admission.
  The cap reserves room for the derived `<name>-<suffix>` so a label value / Service name stays under RFC 1123's 63-character ceiling (appendix-h §H.6).
  Pick short gateway names.
- **Kubernetes ≥ 1.31 required.** Each AGC reconciles only the `RunnerSet`s whose `spec.gatewayRef.name` targets it, via a server-side CRD field selector (KEP-4358) that is alpha-off on 1.30.
  On a 1.30 cluster a v2 AGC's `RunnerSet` informer fails to sync (`field label not supported`) and the pod never becomes ready.
  Confirm the cluster is ≥ 1.31 before onboarding any v2 gateway.
- **Co-located gateways share one namespace security profile.** In v2 the Pod Security level is a property of the **namespace**, not the gateway (see the v2 callout in [Pre-Conditions](#pre-conditions)) — so all gateways in a namespace run under the same `actions-gateway.com/security-profile` label.
  Tenants needing different postures (e.g.
  `baseline` vs `privileged`) still use separate namespaces, exactly as in v1.

For the full reference — the naming table, per-gateway garbage-collection behavior, the CRD chart prerequisite, and the failure modes — see [Troubleshooting — Multiple v2 gateways in one namespace](troubleshooting.md#multiple-v2-gateways-in-one-namespace-naming-scoping-prerequisites) and [Appendix H — v2 API decomposition](../design/appendix-h-v2-api-decomposition.md).

### Proxy-less onboarding (direct egress)

In v2 the egress proxy is **optional**.
A gateway with no `spec.defaultProxyRef` and a `RunnerSet` with no `spec.proxyRef` egress **directly** to GitHub, collapsing the minimal onboarding to **three objects** — one `ActionsGateway`, one `RunnerTemplate`, one `RunnerSet`, with no `EgressProxy` at all:

```yaml
apiVersion: actions-gateway.com/v2beta1
kind: ActionsGateway
metadata: { name: acme, namespace: team-a }
spec:
  credentials:
    type: GitHubApp                       # discriminated union; workload identity is the additive 2nd member
    githubApp: { name: acme-github-app }   # name-only Secret ref, same namespace
  githubURL: https://github.com/acme
  # GHES behind a private CA: name a ConfigMap in this namespace holding the CA
  # bundle under ca.crt. Additive — the system roots stay trusted — and mounted on
  # both the AGC and this gateway's worker pods. Omit on public GitHub.
  # githubCABundleRef: { name: ghes-ca }
  # no defaultProxyRef ⇒ direct egress
---
apiVersion: actions-gateway.com/v2beta1
kind: RunnerTemplate
metadata: { name: default, namespace: team-a }
spec:
  podTemplate:
    spec:
      containers:
        - name: runner
          resources: { requests: { cpu: "1", memory: 2Gi } }
---
apiVersion: actions-gateway.com/v2beta1
kind: RunnerSet
metadata: { name: linux, namespace: team-a }
spec:
  gatewayRef:  { name: acme }
  templateRef: { name: default }
  runnerLabels: [gag-linux]   # the first label is this set's scale-set name and runs-on target
  maxWorkers: 50
  # no proxyRef / defaultProxyRef ⇒ direct egress
```

What you trade, and what you do **not**:

- **Egress is still restricted — this is mandatory and default-on.** Direct egress is **not** open egress.
  The GMC still provisions the default-deny egress NetworkPolicy; it allows only **DNS (cluster DNS) + the GitHub CIDR allowlist** for workers, plus the **Kubernetes API server** for the AGC.
  A worker still cannot reach an arbitrary internet destination.
  The GMC's IP-range refresh keeps the GitHub allowlist current.
  (As with all egress rules, this is enforced only by a policy-aware CNI — see [Pre-Conditions](#pre-conditions).)
- **You lose per-tenant egress IP *attribution*.** Without a proxy there is no stable per-tenant source IP, so GitHub IP-allowlisting (common with Enterprise Managed Users), incident attribution by source IP, and avoiding shared-NAT throttling are **not** available.
  This is the property you opt into by attaching an `EgressProxy`.
- **The trade is surfaced in status.** A proxy-less gateway and runner set report `status.proxyMode: Direct` (visible as the `Egress` print column) plus an advisory `EgressUnattributed` condition (`True`).
  The condition is informational — it does **not** make the object `NotReady`.
  Check it with `kubectl get actionsgateway,runnerset -n <ns>` (the `Egress` column) or `kubectl describe`.

To add attribution later, create an `EgressProxy` and set `spec.defaultProxyRef` on the gateway (every `RunnerSet` under it inherits the proxy unless it sets its own `proxyRef`).
A `proxyRef`/`defaultProxyRef` that names a **missing** `EgressProxy` is treated as an error and fails closed (`Ready=False`/`ProxyNotFound`) — it does **not** silently fall back to direct egress; only an entirely-unset reference means direct.

### Job acquisition, and how labels route

On `v2beta1` there is nothing to choose: every `RunnerSet` acquires jobs with the **runner-scale-set message-queue protocol** — one listener session per set, capacity-gated assignment, and a full-runner worker.
It is the only protocol the graduated version serves, because it removes a many-acquirers job-assignment race the older classic protocol was subject to under high burst (validated end-to-end before the default flip, Q264).

The rules that follow from it, and the ones most likely to bite when you author a set by hand:

- **Every `runnerLabel` is a `runs-on` match target**, so a workflow may name the set with a single label or with an array (`runs-on: [linux, gpu]`).
  Concurrency is governed by `maxWorkers`/`priorityTiers` (advertised to GitHub as the scale set's capacity), not by a listener count.
- **The first label names the scale set**, which makes it the set's identity at GitHub.
  Reordering the list renames the scale set and orphans the old one, so append rather than prepend.
- **The first label is unique per GitHub org, enterprise, or repo.** Two sets claiming the same `runnerLabels[0]` would register the same scale-set name at GitHub; the second is rejected.
  The boundary is the `githubURL` the set's gateway names, **not the namespace**: two sets in different namespaces whose gateways point at the same org still collide, so a shard of one org across namespaces needs distinct first labels per shard.
  Later labels may overlap freely.
- **Declare the full list up front.** A scale set's labels are fixed when it is created and cannot be changed afterwards, so a label added to a live set never reaches GitHub — the set reports [`RunnerLabelsIncomplete`](troubleshooting.md#jobs-targeting-one-of-a-runner-sets-labels-never-start-runnerlabelsincomplete) rather than failing silently.
  Adding one later means replacing the scale set, which costs the jobs queued against it.
  On GitHub Enterprise Server below 3.21 the same condition covers an appliance that dropped the extra labels because `DistributedTask.AllowRunnerScaleSetCustomLabels` is off.

This is the same routing ARC scale sets use, so workflows migrating from ARC carry their `runs-on` lines across unedited — see [Migrating from ARC](migration-from-arc.md#job-routing-a-11-map).

#### Acquisition protocol: `v2alpha1` only

`v2alpha1` — served as the [`gag-migrate`](migration-v1-to-v2.md) on-ramp — carries a `spec.acquisitionProtocol` selector that `v2beta1` does not.
It exists to let a tenant migrating off `v1alpha1` keep the classic per-runner broker protocol its groups were registered under, until it is ready to move.
(Multi-label matching used to be the reason and no longer is: the scale-set protocol registers every `runnerLabel`.)

```yaml
apiVersion: actions-gateway.com/v2alpha1   # the field does not exist on v2beta1
kind: RunnerSet
metadata: { name: linux, namespace: team-a }
spec:
  gatewayRef:  { name: acme }
  templateRef: { name: default }
  acquisitionProtocol: Classic       # deprecated; the classic per-runner broker protocol
  runnerLabels: [self-hosted, linux]
  maxListeners: 10                   # honored under Classic; ignored under ScaleSet
  maxWorkers: 50
```

- **A new tenant should never need this.** Author on `v2beta1`, with as many labels per set as your workflows target.
  `Classic` is deprecated and, together with `v2alpha1` and `v1alpha1`, [removed at `v2.0.0`](v1alpha1-deprecation.md); `spec.maxListeners` goes with it.
- **`gag-migrate` writes `acquisitionProtocol: Classic`** onto every set it emits, so a migrated tenant's groups keep the protocol they were registered under.
  Opting one into the scale-set protocol later means creating a fresh set, not editing the old one — the field is **immutable**, because switching a live set's protocol is a re-registration storm.
- **Editing a Classic set works unqualified, labels included.** An unqualified `kubectl edit/patch/apply` addresses the `v2beta1` storage version, which admits the same multi-label shape.

### Bind a runner set to a GitHub runner group

Everything else on this page bounds what a tenant's workers may *do*.
The GitHub **runner group** bounds who may cause them to *run*, and it is the one control that lives at GitHub rather than in the cluster.

A runner group carries a repository-access policy: which repositories in the organization are allowed to send jobs to the runners in it.
A runner set that names no group registers into the installation's **default** group, which typically admits every repository in the organization.
On a shared cluster that means a repository outside the tenant can put the set's label in its `runs-on` and route its job into the tenant's namespace, quota, and egress IP.
The worker pod is still isolated exactly as this page describes; what is unbounded is the intake.

Declare the group once on the gateway, and every set under it inherits:

```yaml
apiVersion: actions-gateway.com/v2beta1
kind: ActionsGateway
metadata: { name: acme, namespace: team-a }
spec:
  githubURL: https://github.com/acme
  defaultRunnerGroup: team-a          # every RunnerSet under this gateway
```

A set overrides it only when it needs a narrower policy of its own:

```yaml
apiVersion: actions-gateway.com/v2beta1
kind: RunnerSet
metadata: { name: gpu, namespace: team-a }
spec:
  gatewayRef:  { name: acme }
  runnerLabels: [team-a-gpu]
  runnerGroup: team-a-gpu             # wins over the gateway's defaultRunnerGroup
```

**What the platform admin still owns, at GitHub.** GAG never creates a runner group and never edits one's repository access.
Before onboarding a tenant, create the group in the organization's Actions settings and scope its repository access to that tenant's repositories.
GAG only registers the tenant's scale sets into the group you named; if the group admits every repository, so does the tenant's runner set.

**Failure modes.**

- A `runnerGroup`/`defaultRunnerGroup` naming a group the installation does not have leaves the set `Ready=False` with reason `RunnerGroupNotFound`, and it registers no scale set at all.
  This is deliberate: falling back to the default group would widen the very boundary you were narrowing, at the moment you mistyped the name.
  Check the spelling against the group list in the organization's Actions settings; the name is case-sensitive and must match exactly.
- Adding or changing the group on a set that is already running re-registers its scale set into the new group and restarts its listener.
  In-flight jobs finish; the set is briefly not acquiring new ones.
- Removing the field does **not** move the scale set back to the default group.
  An undeclared group leaves an existing scale set where it is, so widening the boundary is always an explicit act at GitHub.

### Starting from a shipped template

Before hand-authoring a `RunnerTemplate`, check whether one of the three shipped entries already fits.
`kubectl apply -k deploy/templates/plain` gives a baseline worker; `kata-dind` and `privileged-dind` cover jobs that build container images.
A `RunnerSet` then names one with `templateRef: { name: plain, kind: ClusterRunnerTemplate }`.
They are cluster-scoped and platform-applied, none is marked as the cluster default, and each carries prerequisites the template cannot express: see the [runner template library](runner-template-library.md).

### Optional `templateRef` (a default worker pod shape)

`RunnerSet.spec.templateRef` is **optional**.
A `RunnerSet` that omits it resolves a worker pod shape through a fallback chain, so a tenant does not have to name a template in every runner set.
The chain (resolved at runtime, fail-closed):

1. `RunnerSet.spec.templateRef` — the explicit reference (unchanged: a set that sets it behaves exactly as before).
2. else `ActionsGateway.spec.defaultTemplateRef` — a per-gateway default the platform or tenant sets on the gateway; inherited by every `RunnerSet` under it that omits `templateRef`.
   It may name a namespaced `RunnerTemplate` or a cluster-scoped `ClusterRunnerTemplate` (`kind: ClusterRunnerTemplate`).
3. else the **single cluster-default `ClusterRunnerTemplate`** — the one a platform admin has marked with the annotation `actions-gateway.com/is-default-template: "true"` (the same pattern as Kubernetes' default `StorageClass`).
4. else the set fails closed `Ready=False`/`TemplateNotFound` — the controller **never** synthesizes a worker pod without a real pod shape.

Which rung resolved is reported in `status.templateSource` (`TemplateRef` / `GatewayDefault` / `ClusterDefault`), visible as the `-o wide` `Template` print column — so you can audit whether a set runs on an explicit template or a default.

**Marking the cluster-default (platform admin).** Annotate exactly one `ClusterRunnerTemplate`:

```bash
kubectl annotate clusterrunnertemplate <name> actions-gateway.com/is-default-template=true
```

- The marker is honored **only** on the cluster-scoped `ClusterRunnerTemplate` (platform-authored).
  A tenant cannot self-elect a namespaced `RunnerTemplate` as the cluster-wide default.
- **At most one** may be marked.
  If two are marked, any `RunnerSet` relying on the cluster-default rung fails closed `Ready=False`/`AmbiguousDefault` (the message names the conflicting templates) rather than silently picking one — demote the extra (`kubectl annotate clusterrunnertemplate <name> actions-gateway.com/is-default-template-`) and the set recovers automatically.
- A `defaultTemplateRef`/`templateRef` that **names a missing** template still fails closed (`TemplateNotFound`); only an entirely-unset reference falls through to the next rung.

The minimal proxy-less onboarding above can therefore drop `templateRef` from the `RunnerSet` once a `defaultTemplateRef` or a cluster-default exists.

### Tuning AGC control-plane resources

`ActionsGateway.spec.agcResources` is an **optional** per-gateway override for the CPU/memory requests and limits stamped on this gateway's AGC control-plane container.
Most tenants never set it — the AGC ships with a sensible platform default sized for the worst-case listener burst.

**The platform default (applied when `agcResources` is omitted):**

| | CPU | Memory |
| --- | --- | --- |
| request | `500m` | `2Gi` |
| limit | `2` | `4Gi` |

This is the [Appendix A](../design/appendix-a-capacity-slos.md) capacity sizing: the `2Gi` memory request is a generous reservation for the ~1,000-goroutine peak burst with headroom for Go runtime overhead; the `4Gi` memory limit sits well above the working set so transient bursts don't trigger an OOMKill; the `2`-core CPU limit absorbs reconcile/token-refresh spikes on an otherwise I/O-bound (long-poll-blocked) workload.

**When to tune.** Override only on real signal:

- **Memory** — raise `requests.memory` and `limits.memory` if you run many RunnerGroups with high `maxListeners` and observe `container OOMKilled` events or high GC pressure (`go_gc_duration_seconds`).
  Budget roughly `sum(maxListeners) × 60 KiB` of working set plus the default headroom.
- **CPU** — raise `limits.cpu` only if `container_cpu_throttled_seconds_total` shows sustained throttling during peak reconcile churn.

The override is **per key**: set only the request/limit entries you want to change and every other entry keeps its platform default.
For example, raising just the memory limit leaves the CPU request/limit and memory request at their defaults:

```yaml
apiVersion: actions-gateway.com/v2beta1
kind: ActionsGateway
metadata:
  name: my-gateway
  namespace: my-tenant
spec:
  credentials:
    type: GitHubApp
    githubApp:
      name: my-tenant-github-app
  githubURL: https://github.com/my-org
  agcResources:
    limits:
      memory: 8Gi   # CPU request/limit and memory request stay at the platform default
```

Changing `agcResources` rolls the AGC Deployment (a rolling restart, not a hot reload); in-flight listener sessions deregister and re-register within GitHub's redelivery window.

**Recommended floor / footguns.** The AGC is a single pod holding all listener state in memory — size it generously rather than tight:

- Do **not** set `limits.memory` below ~`512Mi`, and keep it above your observed working set; a limit under the working set OOMKills the control plane (a `CrashLoopBackOff`, not a clear error).
  The platform default `4Gi` is a safe starting point.
- Do **not** set `requests` larger than a single node can schedule, or larger than the namespace `ResourceQuota` leaves free — an over-large request leaves the AGC pod `Pending` (unschedulable).
  Check `kubectl describe pod` for `FailedScheduling` if the AGC never starts after an `agcResources` change.

There is no admission-time floor enforced on the values — the guidance above is operator-owned.
When in doubt, leave `agcResources` unset and let the platform default apply.

### Letting an autoscaler size the AGC (`agcAutoscaling`)

If you would rather not guess at `agcResources`, `ActionsGateway.spec.agcAutoscaling` hands the sizing to a [Vertical Pod Autoscaler](https://github.com/kubernetes/autoscaler/tree/master/vertical-pod-autoscaler) (VPA): the Gateway Manager Controller (GMC) stamps a `VerticalPodAutoscaler` next to this gateway's `<gateway>-agc` Deployment, and the autoscaler right-sizes the AGC container's **requests** from its observed usage.

**Prerequisite.** Your cluster administrator must have installed the Kubernetes vertical-pod-autoscaler (the `autoscaling.k8s.io` CRDs plus the recommender, updater, and admission-controller).
Setting the field without it does **not** break the gateway — see [the CRD-missing runbook](troubleshooting.md#agcautoscalingunavailable--the-vpa-crds-are-not-installed).

The block's **presence** is the opt-in — there is no `enabled` flag, and deleting the block deletes the autoscaler:

```yaml
spec:
  agcAutoscaling: {}          # recommendation-only (mode defaults to Off)
```

| `mode` | What it does |
| --- | --- |
| `Off` (default) | Publishes a recommendation on the `VerticalPodAutoscaler`'s own status and changes nothing. Start here: run it for a few days and read `kubectl describe vpa <gateway>-agc`. |
| `Initial` | Applies the recommendation only when a new AGC pod is created. Never evicts a running AGC. |
| `Recreate` | Lets the autoscaler evict the AGC pod to resize it. Safe — in-flight listener sessions deregister on SIGTERM and re-register within GitHub's redelivery window — but it *is* a control-plane restart. |

Upstream's `Auto` mode is not offered: it is an alias whose actuation mechanism changes between autoscaler releases, so this API names `Recreate` explicitly instead.

**How it interacts with `agcResources` — they compose, neither one silently wins:**

- `agcResources` still decides what is stamped on the Deployment, and is the sizing in effect whenever the autoscaler is not actuating (`mode: Off`, VPA components down, or before the first recommendation).
- The autoscaler is pinned to `controlledValues: RequestsOnly`, so **your limits are never moved by it**.
  The memory limit stays the hard OOM ceiling you chose.
- **A request you explicitly set becomes the autoscaler's floor** (`minAllowed`).
  If you want the autoscaler free to shrink the AGC — usually the point of enabling it — leave `agcResources.requests` unset.
- Your effective limits become its ceiling (`maxAllowed`); a request may never exceed its own limit.

So `agcResources: {requests: {memory: 1Gi}, limits: {memory: 6Gi}}` plus `agcAutoscaling: {mode: Recreate}` means "autoscale the memory request, but never below 1Gi and never above 6Gi".
Check the resolution any time with `kubectl get vpa <gateway>-agc -o yaml`; the GMC also emits a Normal Event naming the derived bounds when the autoscaler is first created.

**Checking it took effect.** `kubectl describe actionsgateway <gateway>` shows an `AGCAutoscalingUnavailable` condition: `False`/`AGCAutoscalingActive` when the autoscaler is in place, `False`/`AGCAutoscalingDisabled` when you have not opted in, `True`/`VPACRDNotInstalled` when the prerequisite is missing.

The GMC's own control plane has the same opt-in at the chart level — the `vpa.enabled` value in [install.md](install.md#key-values-an-operator-sets).

### Per-pool egress audit record

A proxy pool can record one structured line per accepted CONNECT: which destination a tenant's workers reached, when, and how many bytes moved each way.
It is **off by default** and turned on per pool:

```sh
kubectl patch egressproxy -n <tenant-namespace> <name> \
  --type merge -p '{"spec":{"auditLogging":"Connections"}}'
```

Only `Off` (the default) and `Connections` are accepted; admission rejects anything else.

**It records; it does not enforce.** Neither value changes what the proxy forwards.
If you know `audit` from Pod Security Admission, where it means *evaluate the policy but admit anyway*, that sense does not apply here: enabling this does not put the destination allowlist into report-only, and it relaxes nothing.
Destination enforcement is `destinationFQDNs`/`destinationCIDRs` plus the pod-egress NetworkPolicy, unchanged either way.

**Quote `Off` in a YAML manifest.** YAML reads a bare `Off` as the boolean false, so `kubectl apply` of `auditLogging: Off` sends a boolean and the apiserver rejects it as the wrong type for a string field, naming a value you never typed.
Write `auditLogging: "Off"`, or leave the field out, which means the same thing.
The `kubectl patch` above is JSON and is unaffected.

**A shared pool attributes per pool, not per tenant.** If this pool is referenced from other namespaces via `spec.sharing.allowedNamespaces`, the record's `namespace` names the pool, not whichever consumer sent the request: a CONNECT carries no namespace and nothing else in the line identifies the caller.
Give a tenant whose audit trail has to name them their own unshared pool.

**Off is the default deliberately, and turning it on is a decision with two costs.** The record says where a tenant's traffic went, so retaining it is a choice about what the platform keeps and for how long.
Agree that with the tenant rather than switching it on across a cluster.
And it is one line per connection: under real CI load it becomes the pool's dominant log volume, so size the collector and its retention before flipping it.

**It is a rolling restart, not a hot reload.** The value is part of the pod template, so records begin once the pool's new pods are up.
In-flight tunnels finish on the old pods within the termination grace period and produce no record, a gap of at most one drain that is worth knowing if you turn it on mid-incident.

Turn it on when you need egress evidence the counters cannot give: an incident where "which endpoint did this pool reach at 14:02" is the question, or a compliance ask for per-tenant egress attribution, which an unshared pool satisfies and a shared one does not.
Reach for it *before* the incident if the answer has to exist afterwards: it records forward, never backward.

The record shape, the field meanings, and how to select the audit stream in a log pipeline are in [logging: proxy egress audit record](observability-logging.md#proxy-egress-audit-record); what a line deliberately never carries is in the [security design](../design/05-security.md#proxy-egress-audit-record).

### Workload-identity credentials (external signer)

`spec.credentials` is a discriminated union (keyed by `credentials.type`) with two members.
The default, `GitHubApp` (every example above), is the **possession model**: the App's RSA private key lives in a namespace `Secret` ([Step 1](#step-1-create-the-github-app-secret)) and the AGC signs the App JWT in-process.
`WorkloadIdentity` is the opt-in **delegation model**: **no App private key is ever stored in the cluster** — an external signer signs the App JWT, and the AGC proves its own pod identity to that signer.
Use it when policy forbids a long-lived signing key at rest in the cluster and you run a signer (HashiCorp Vault in the MVP) the AGC can reach.
See [security §5.7](../design/05-security.md#57-workload-identity-the-no-pem-delegation-model) for the trust model.

There is **no `githubApp` Secret** for this method — you do not run [Step 1](#step-1-create-the-github-app-secret).
Instead you put the non-secret App identity (`appId`/`installationId`) inline and reference a Vault transit key:

```yaml
apiVersion: actions-gateway.com/v2beta1
kind: ActionsGateway
metadata: { name: acme, namespace: team-a }
spec:
  credentials:
    type: WorkloadIdentity          # the no-PEM delegation member
    workloadIdentity:
      appId: 12345                  # non-secret; the JWT issuer
      installationId: 67890         # non-secret
      signer:
        provider: Vault             # HashiCorp Vault transit (cloud KMS providers follow)
        vault:
          address: https://vault.vault.svc:8200   # HTTPS required (dev/test plaintext is an explicit AGC opt-in)
          keyName: github-app       # the RSA transit key Vault signs with (signed as RS256)
          transitMount: transit     # optional; defaults to "transit"
          auth:
            role: agc-acme          # the Vault Kubernetes-auth role bound to this gateway's AGC ServiceAccount
            mount: kubernetes       # optional; defaults to "kubernetes"
          # Optional (Q202): on a policy-enforcing CNI, identify Vault as a NetworkPolicy
          # egress peer so the GMC opens a scoped AGC→Vault egress rule automatically.
          # Set exactly one form — a pod/namespace selector (in-cluster Vault) OR a cidr
          # (external Vault). Omit on a non-enforcing CNI (kindnet) or to manage it by hand.
          networkPolicy:
            namespaceSelector:
              matchLabels: { kubernetes.io/metadata.name: vault }
            podSelector:
              matchLabels: { app.kubernetes.io/name: vault }
            # external Vault instead: cidr: 10.0.5.7/32
            # port: 8200            # optional override; default is the port from `address`
  githubURL: https://github.com/acme
```

Operator prerequisites in Vault (configured out of band, once per gateway):

- A **transit key** (`keyName`) of an **RSA** type — GitHub App keys are RSA, and transit signs it as `RS256` (`pkcs1v15` + `sha2-256`).
  Import the App's existing private key into transit, or generate a new key in transit and register its public half as the App's key in GitHub.
- A **Kubernetes auth role** (`auth.role`) bound to this gateway's AGC ServiceAccount (named `<gateway-name>-agc`) and namespace, granting `update` on `transit/sign/<keyName>`.
  The AGC logs in with its projected ServiceAccount token; Vault verifies it via the cluster `TokenReview` API.
  The GMC projects that token with the audience **`vault`**, so configure the role with `audience=vault` (or leave it unset to skip the audience check) — e.g. `vault write auth/kubernetes/role/<role> bound_service_account_names=<gateway-name>-agc bound_service_account_namespaces=<namespace> token_policies=<policy> audience=vault`.
- The Vault `address` must be **HTTPS** — the ServiceAccount token transits it at login.
  A plaintext address is rejected unless the AGC carries an explicit dev/test opt-in.

> **NetworkPolicy egress to Vault (Q202).** The GMC's per-tenant AGC NetworkPolicy default-denies egress except DNS, GitHub, and the kube API server.
> Vault's `address` is not itself a NetworkPolicy-expressible peer, so set `signer.vault.networkPolicy` (above) to identify Vault — a `namespaceSelector`/`podSelector` for an in-cluster Vault, or a `cidr` for an external one.
> The GMC then opens a **scoped** AGC→Vault egress rule (that peer, on the Vault API port from `address`) on the AGC NetworkPolicy automatically — you no longer add it by hand.
> If you leave `networkPolicy` unset on a policy-enforcing CNI (Calico/Cilium — the production recommendation), the AGC's Vault login will be dropped, so set it; on a non-enforcing CNI (kindnet) it is inert and may be omitted.
> As with all egress rules, it is enforced only by a policy-aware CNI.
> The field is a shared `EgressPeer` type (Q204) — the optional `port` overrides the address-derived port and is normally left unset; the same type will back future egress peers (cloud KMS, telemetry) so the example shape stays stable.

The GMC provisions the workload-identity AGC the same as a `GitHubApp` gateway — minus the credential `Secret` mount (there is none), plus the projected Vault-audience ServiceAccount token and the signer config env.
A `WorkloadIdentity` gateway reaches `Ready=True` once its AGC mints its first installation token through Vault.

---

## Handing Off to the Tenant

Once onboarding is complete, share with the tenant team:

- The namespace name and the `ActionsGateway` CR name they own.
- The runner labels to use in their workflow `runs-on` fields.
- A link to [Getting Started](../getting-started.md) for self-service changes (RunnerGroup config, quota requests, credential rotation).
- A link to [Observability](observability.md) for the metrics they can watch.
- The on-call contact for platform-level issues (AGC crashes, GMC failures).

Tenants can manage their own RunnerGroup configuration, credential rotation, and `maxListeners` tuning without platform team involvement after this handoff.
