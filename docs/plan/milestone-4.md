# Milestone 4 Implementation Plan — Gateway Manager Controller + Proxy

← [Milestone 3](milestone-3.md) | [Back to implementation phases](../design/06-implementation-phases.md)

---

## Table of Contents

- [Status at a glance](#status-at-a-glance)
- [Overview](#overview)
- [1. Repository Scaffolding](#1-repository-scaffolding)
- [2. ActionsGateway CRD (cmd/gmc/api/v1alpha1/)](#2-actionsgateway-crd-cmdgmcapiv1alpha1)
- [3. GMC Reconciler (cmd/gmc/internal/controller/)](#3-gmc-reconciler-cmdgmcinternalcontroller)
- [4. Admission Webhook (cmd/gmc/internal/webhook/)](#4-admission-webhook-cmdgmcinternalwebhook)
- [5. CONNECT Proxy (cmd/proxy/)](#5-connect-proxy-cmdproxy)
- [6. Changes to the AGC (cmd/agc/)](#6-changes-to-the-agc-cmdagc)
- [7. GMC main.go](#7-gmc-maingo)
- [8. Test Plan](#8-test-plan)
- [9. Success Criteria Checklist](#9-success-criteria-checklist)
- [10. Risks and Mitigations](#10-risks-and-mitigations)
- [11. Deferred to Milestone 5](#11-deferred-to-milestone-5)
- [12. Live multi-tenant validation evidence (2026-06-11/12)](#12-live-multi-tenant-validation-evidence-2026-06-1112)

## Status at a glance

Last refreshed 2026-06-12. **All success criteria are now live-validated.**
The multi-tenant, delete-isolation, and end-to-end-proxy-job rows were
proven on a real kind cluster with real GitHub App credentials on
2026-06-11/12 — see [§12 Live multi-tenant validation evidence](#12-live-multi-tenant-validation-evidence-2026-06-1112)
for the full session record, including the four product bugs it surfaced
(tracked as Q114–Q117 in [STATUS.md](../STATUS.md)).

| Success criterion | Status | Notes |
|---|---|---|
| Two `ActionsGateway` CRs → two independent tenants | ✅ Done | 2026-06-12 live on kind via `helm install` (digest-pinned): `tenant-a`+`tenant-b` both `Ready=True` with 2/2 proxy pods in <1 min — see §12 |
| Deleting one CR removes only that tenant's resources | ✅ Done | 2026-06-12 live: deleting `gateway-b` removed all GMC-managed resources in `tenant-b` only; `tenant-a` stayed Ready and ran a subsequent green job — see §12 |
| `spec.proxy.maxReplicas` change reflected in HPA | ✅ Done in code | `buildHPA` reads `ag.Spec.Proxy.MaxReplicas` ([builder.go:385](../../cmd/gmc/internal/controller/builder.go)); `hpa_update_test.go` covers it |
| Webhook rejects CRs in `kube-system`/`kube-public`/`gmc-system`/`$POD_NAMESPACE` | ✅ Done | [actionsgateway_webhook.go:21-47](../../cmd/gmc/internal/webhook/v1alpha1/actionsgateway_webhook.go) |
| End-to-end job via proxy (green checkmark + `HTTPS_PROXY` in worker env) | ✅ Done | 2026-06-12 live: runs [27386891757](https://github.com/actions-gateway/gateway-test/actions/runs/27386891757) + [27395702908](https://github.com/actions-gateway/gateway-test/actions/runs/27395702908) concluded `success`; worker pod env carried `HTTPS_PROXY=https://actions-gateway-proxy.tenant-a.svc.cluster.local:8080` — see §12 (needed the Q115 `runAsUser` workaround) |
| RBAC: no `*` verbs on `secrets`/`pods`/`nodes` in GMC ClusterRole | ✅ Done | `rbac_test.go` has 2 wildcard-detection tests |
| `go test -race ./...` passes across all four modules | ✅ Done | Per-module test commands pass |
| Worker ServiceAccount `actions-gateway-worker` created by GMC | ✅ Done | `buildWorkerServiceAccount` ([builder.go:77](../../cmd/gmc/internal/controller/builder.go)); injected into AGC via `WORKER_SERVICE_ACCOUNT` env |
| Proxy pods: `runAsNonRoot`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false` | ✅ Done | [builder.go:323-327](../../cmd/gmc/internal/controller/builder.go); also `Caps Drop ALL` + `Seccomp RuntimeDefault` via W8 |
| IP range reconciler updates NetworkPolicy on 24h tick | ✅ Done | `time.NewTicker(24 * time.Hour)` in [ipranges.go:135-151](../../cmd/gmc/internal/controller/ipranges.go); `ipranges_test.go` covers the path |
| GMC runs with leader election | ✅ Done | `--leader-elect` flag wired ([cmd/gmc/cmd/main.go:61,158](../../cmd/gmc/cmd/main.go)); enabled in `config/manager/manager.yaml:64` |
| Code committed | ✅ Done | |

### Critical path

~~The only milestone gate is end-to-end validation in `kind`.~~ Closed
2026-06-12 by the live validation session in §12. The milestone is
complete.

---

## Overview

**Goal:** Introduce the Gateway Manager Controller (GMC), a cluster-level operator that reconciles `ActionsGateway` CRs into the full per-tenant resource set — RBAC, network guardrails, egress proxy, and AGC deployment — and deliver a minimal stateless CONNECT proxy binary. The result is a self-contained, multi-tenant system: platform teams apply one CR per tenant and the GMC handles everything else.

**Duration:** Days 17–22

**Foundation:** All packages from Milestones 1–3 are consumed unchanged except for targeted additions to the AGC provisioner (proxy env injection, §4.1).

**Two new modules are introduced:**
- `cmd/gmc/` — the GMC binary (new Go module; imports the AGC API package for RunnerGroup types)
- `cmd/proxy/` — the CONNECT proxy binary (new Go module; minimal stdlib + prometheus dependency)

**Definition of Done:**

- Applying two `ActionsGateway` CRs in a `kind` cluster produces two independent tenant namespaces, each with a running AGC and proxy pool with at least `minReplicas` Ready pods.
- Deleting one CR tears down only that tenant's resources; the other tenant's namespace and workloads are unaffected.
- Updating `spec.proxy.maxReplicas` causes the HPA to reflect the new bound within one reconcile cycle.
- The admission webhook rejects `ActionsGateway` CRs created in reserved namespaces.
- An end-to-end job dispatched from GitHub routes through the proxy, executes in a worker pod, and completes with a green checkmark — confirming `HTTP_PROXY` injection is correct.
- The RBAC regression test passes: no rule in the GMC's generated ClusterRole has `*` verbs on `secrets`, `pods`, or `nodes`.
- All unit tests pass under `go test -race ./...` (per-module invocation from the repo root).
- Code is committed to the repository.

---

## 1. Repository Scaffolding

### 1.1 New modules and workspace entries

```
mkdir -p cmd/gmc cmd/proxy
cd cmd/gmc && go mod init github.com/actions-gateway/github-actions-gateway/gmc
cd cmd/proxy && go mod init github.com/actions-gateway/github-actions-gateway/proxy
```

Add both to `go.work`:

```
go 1.26

use (
    ./cmd/agc    // Milestone 2
    ./cmd/gmc    // Milestone 4
    ./cmd/probe  // Milestone 1
    ./cmd/proxy  // Milestone 4
    ./cmd/worker
)

replace github.com/actions-gateway/github-actions-gateway => ./
```

The `replace` directive already in `go.work` prevents the longest-prefix routing bug (see `CLAUDE.md`); no additional replace directives are needed for the new modules.

The `cmd/gmc/go.mod` requires `github.com/actions-gateway/github-actions-gateway/agc` to import the RunnerGroup API types. The workspace resolver handles this via the `use ./cmd/agc` entry without a replace directive.

### 1.2 Kubebuilder bootstrapping for GMC

Scaffold the GMC inside `cmd/gmc/` using `kubebuilder`:

```bash
cd cmd/gmc
kubebuilder init --domain actions-gateway.github.com --repo github.com/actions-gateway/github-actions-gateway/gmc
kubebuilder create api --group actions-gateway.github.com --version v1alpha1 \
    --kind ActionsGateway --resource --controller
kubebuilder create webhook --group actions-gateway.github.com --version v1alpha1 \
    --kind ActionsGateway --validating
```

Commit the scaffolded layout. Replace the generated controller stub and webhook stub with the implementations in §2 and §3. Keep the generated deep-copy and scheme registration.

### 1.3 Directory layout

```
cmd/
├── agc/                           # Milestone 2–3 (unchanged)
├── gmc/                           # Milestone 4 — new
│   ├── api/v1alpha1/
│   │   ├── actionsgateway_types.go
│   │   ├── groupversion_info.go
│   │   └── zz_generated.deepcopy.go
│   ├── config/
│   │   ├── crd/
│   │   │   └── actions-gateway.github.com_actionsgateways.yaml
│   │   ├── rbac/
│   │   │   ├── role.yaml          # GMC ClusterRole (generated from markers)
│   │   │   └── role_binding.yaml
│   │   └── webhook/
│   │       └── manifests.yaml     # ValidatingWebhookConfiguration
│   ├── internal/
│   │   ├── controller/
│   │   │   ├── actionsgateway_controller.go
│   │   │   ├── actionsgateway_controller_test.go
│   │   │   ├── builder.go         # resource builders (SA, Role, Deployment, etc.)
│   │   │   ├── builder_test.go
│   │   │   ├── ipranges.go        # GitHubIPRangeFetcher + background reconciler
│   │   │   └── ipranges_test.go
│   │   └── webhook/
│   │       ├── actionsgateway_webhook.go
│   │       └── actionsgateway_webhook_test.go
│   ├── go.mod
│   └── main.go
├── proxy/                         # Milestone 4 — new
│   ├── main.go
│   ├── proxy.go
│   ├── proxy_test.go
│   ├── go.mod
│   └── Dockerfile
├── probe/                         # Milestone 1 (unchanged)
└── worker/                        # Milestone 3 (unchanged)
```

---

## 2. ActionsGateway CRD (`cmd/gmc/api/v1alpha1/`)

### 2.1 Type definitions (`actionsgateway_types.go`)

The structs mirror the design spec in `docs/design/03-api-contracts.md`. Key types:

```go
// SecretReference is a pointer to a Kubernetes Secret with optional namespace override.
type SecretReference struct {
    Name      string `json:"name"`
    // +optional
    Namespace string `json:"namespace,omitempty"`
}

// ProxyConfig configures the per-tenant egress proxy pool.
type ProxyConfig struct {
    // +optional
    // +kubebuilder:default=2
    MinReplicas *int32 `json:"minReplicas,omitempty"`

    // +optional
    // +kubebuilder:default=10
    MaxReplicas *int32 `json:"maxReplicas,omitempty"`

    // +optional
    // +kubebuilder:default=60
    TargetCPUUtilizationPercentage *int32 `json:"targetCPUUtilizationPercentage,omitempty"`

    // +optional
    Resources corev1.ResourceRequirements `json:"resources,omitempty"`

    // +optional
    NoProxyCIDRs []string `json:"noProxyCIDRs,omitempty"`

    // +optional
    // +kubebuilder:default=true
    ManagedNetworkPolicy *bool `json:"managedNetworkPolicy,omitempty"`
}

// ActionsGatewaySpec is the desired state of an ActionsGateway.
type ActionsGatewaySpec struct {
    GitHubAppRef   SecretReference `json:"gitHubAppRef"`
    // +optional
    Proxy          ProxyConfig     `json:"proxy,omitempty"`
    // RunnerGroups lists RunnerGroup specs bootstrapped in the tenant namespace.
    // +optional
    RunnerGroups   []agcv1alpha1.RunnerGroupSpec `json:"runnerGroups,omitempty"`
    // +optional
    NamespaceQuota corev1.ResourceList `json:"namespaceQuota,omitempty"`
}

// ActionsGatewayStatus is the observed state of an ActionsGateway.
type ActionsGatewayStatus struct {
    // +optional
    Conditions          []metav1.Condition `json:"conditions,omitempty"`
    ProxyReadyReplicas  int32              `json:"proxyReadyReplicas"`
    ActiveSessions      int32              `json:"activeSessions"`
    ObservedGeneration  int64              `json:"observedGeneration"`
}

// ActionsGateway is a namespace-scoped CRD managed by the GMC.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ag
// +kubebuilder:printcolumn:name="ProxyReady",type=integer,JSONPath=".status.proxyReadyReplicas"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type ActionsGateway struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   ActionsGatewaySpec   `json:"spec,omitempty"`
    Status ActionsGatewayStatus `json:"status,omitempty"`
}
```

`RunnerGroups []agcv1alpha1.RunnerGroupSpec` reuses the type from the AGC API package (`github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1`). This is the only external dependency between the two modules and flows in one direction (GMC → AGC).

Note: the `ActionsGatewaySpec.RunnerGroups` field uses `agcv1alpha1.RunnerGroupSpec`, not the full `RunnerGroup` CR type, because the GMC bootstraps RunnerGroup CRs from a spec — it constructs the CR name itself as `{actionsgateway-name}-{runnergroup.name}`.

### 2.2 CRD generation

Run `make manifests` inside `cmd/gmc/` to regenerate `config/crd/` and `config/rbac/role.yaml` from the kubebuilder markers. Commit the generated files. The Makefile target follows the same pattern as `cmd/agc/Makefile`.

---

## 3. GMC Reconciler (`cmd/gmc/internal/controller/`)

### 3.1 Resource creation order

The reconciler creates resources in this order on each reconcile (fully idempotent via `CreateOrUpdate`):

1. `ServiceAccount` — `actions-gateway-agc` (AGC identity)
2. `ServiceAccount` — `actions-gateway-worker` (worker pod identity; used by provisioner in §4.1)
3. `Role` — grants the AGC SA permissions within the namespace (see §3.3)
4. `RoleBinding` — binds `actions-gateway-agc` to the Role
5. `NetworkPolicy` — restricts egress (proxy pods → GitHub; AGC/worker pods → proxy; see §3.4)
6. `ResourceQuota` — applies `spec.namespaceQuota` if set
7. Proxy `Deployment` — CONNECT proxy pods (§5)
8. Proxy `Service` (ClusterIP) — stable address for `HTTP_PROXY`
9. `PodDisruptionBudget` — `minAvailable: 1` on the proxy Deployment
10. `HorizontalPodAutoscaler` — scales proxy Deployment between `minReplicas` and `maxReplicas`
11. AGC `Deployment` — runs the AGC binary with credentials and proxy env injected
12. `RunnerGroup` CRs — one per entry in `spec.runnerGroups`

On each reconcile, existing resources are patched via `CreateOrUpdate` (server-side apply semantics). This makes the reconciler idempotent and safe under concurrent reconciles with leader election.

### 3.2 Reconciler struct

```go
// ActionsGatewayReconciler reconciles ActionsGateway objects.
//
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=actionsgateways,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=actionsgateways/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=actionsgateways/finalizers,verbs=update
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=runnergroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
type ActionsGatewayReconciler struct {
    client.Client
    Scheme     *runtime.Scheme
    IPFetcher  GitHubIPRangeFetcher
    // AGCImage is the container image for the AGC binary.
    AGCImage   string
    // ProxyImage is the container image for the proxy binary.
    ProxyImage string
    Log        *slog.Logger
}
```

Note the absence of `secrets` in the RBAC markers: the GMC never reads Secret contents. It only references the Secret by name (from `spec.gitHubAppRef`) when constructing the AGC Deployment's volume mounts. No `get` on `secrets` is required — the Secret name passes through as a string.

### 3.3 Role generated for the AGC ServiceAccount

The GMC creates a `Role` (not ClusterRole) in the tenant namespace that the AGC SA is bound to. This role is the AGC's namespace-scoped permission set:

```yaml
rules:
- apiGroups: [""]
  resources: [pods]
  verbs: [get, list, watch, create, delete]
- apiGroups: [""]
  resources: [pods/status]
  verbs: [get]
- apiGroups: [""]
  resources: [secrets]
  verbs: [get, list, watch, create, delete]
- apiGroups: [actions-gateway.github.com]
  resources: [runnergroups]
  verbs: [get, list, watch, update, patch]
- apiGroups: [actions-gateway.github.com]
  resources: [runnergroups/status, runnergroups/finalizers]
  verbs: [get, update, patch]
```

This is the correct RBAC posture for the AGC. Unlike the AGC's own current `config/rbac/role.yaml` (which is a ClusterRole generated by kubebuilder for development), the GMC-generated Role is namespace-scoped and replaces the ClusterRole binding in production. In M4 the AGC's own ClusterRole/ClusterRoleBinding manifests are deprecated for production use; the GMC-generated Role is the authoritative binding.

### 3.4 NetworkPolicy template

The GMC creates a single `NetworkPolicy` in the tenant namespace enforcing the two-tier egress model. GitHub IP ranges (fetched at provisioning time; see §3.7) populate the egress rules for proxy pods.

```go
// buildNetworkPolicy constructs the tenant NetworkPolicy.
// proxyEgress contains the IP blocks for GitHub's current IP ranges.
func buildNetworkPolicy(ag *v1alpha1.ActionsGateway, proxyServiceClusterIP string, githubCIDRs []string) *networkingv1.NetworkPolicy
```

Key rules:

| Pod selector | Egress allowed to |
|---|---|
| `app: actions-gateway-proxy` | GitHub IP CIDRs (port 443), cluster DNS (port 53 UDP/TCP) |
| `app: actions-gateway-agc` | Proxy ClusterIP:8080, Kubernetes API server (port 443 via `NO_PROXY` exclusion) |
| `app: actions-gateway-worker` | Proxy ClusterIP:8080 |

Ingress rules:

| Pod selector | Ingress from |
|---|---|
| `app: actions-gateway-proxy` | namespace (AGC and worker pods only) |

When `spec.proxy.managedNetworkPolicy` is `false`, the GMC creates the NetworkPolicy without IP-range egress rules (just the within-namespace rules). The tenant's FQDN-based CNI policy handles GitHub egress.

### 3.5 AGC Deployment template

The GMC creates the AGC Deployment in the tenant namespace. Key fields:

```go
func buildAGCDeployment(ag *v1alpha1.ActionsGateway, agcImage, proxyServiceAddr string) *appsv1.Deployment {
    return &appsv1.Deployment{
        Spec: appsv1.DeploymentSpec{
            Replicas: ptr(int32(1)),
            Template: corev1.PodTemplateSpec{
                Spec: corev1.PodSpec{
                    ServiceAccountName: "actions-gateway-agc",
                    Containers: []corev1.Container{{
                        Name:  "agc",
                        Image: agcImage,
                        Env: []corev1.EnvVar{
                            {Name: "GITHUB_APP_ID",              ValueFrom: secretKeyRef(ag.Spec.GitHubAppRef, "appId")},
                            {Name: "GITHUB_APP_PRIVATE_KEY",     ValueFrom: secretKeyRef(ag.Spec.GitHubAppRef, "privateKey")},
                            {Name: "GITHUB_APP_INSTALLATION_ID", ValueFrom: secretKeyRef(ag.Spec.GitHubAppRef, "installationId")},
                            {Name: "POD_NAMESPACE",              ValueFrom: fieldRef("metadata.namespace")},
                            {Name: "WORKER_SERVICE_ACCOUNT",     Value: "actions-gateway-worker"},
                            {Name: "HTTP_PROXY",                 Value: proxyServiceAddr},
                            {Name: "HTTPS_PROXY",                Value: proxyServiceAddr},
                            {Name: "NO_PROXY",                   Value: buildNoProxy(ag.Spec.Proxy.NoProxyCIDRs)},
                        },
                    }},
                },
            },
        },
    }
}
```

`proxyServiceAddr` is `http://actions-gateway-proxy.{namespace}.svc.cluster.local:8080`.

`buildNoProxy` merges the user-provided `spec.proxy.noProxyCIDRs` (or the default list if empty) with the cluster-internal exclusions: `kubernetes.default.svc.cluster.local`, `localhost`, `127.0.0.1`, and the service CIDR. The default list is:

```
kubernetes.default.svc.cluster.local,localhost,127.0.0.1,10.96.0.0/12
```

The `Secret` referenced by `gitHubAppRef` is referenced **by name only** in the `secretKeyRef` — the GMC never reads its contents.

### 3.6 Proxy Deployment template

```go
func buildProxyDeployment(ag *v1alpha1.ActionsGateway, proxyImage string) *appsv1.Deployment
```

Key fields:
- `replicas`: initial value = `spec.proxy.minReplicas` (the HPA takes over after creation)
- Container resources: `requests: {cpu: 10m, memory: 32Mi}`, `limits: {cpu: 100m, memory: 64Mi}` — overridden by `spec.proxy.resources` if set
- `podAntiAffinity: preferredDuringSchedulingIgnoredDuringExecution` spreading across nodes
- Liveness probe: `GET /healthz` on port 8081 (process up)
- Readiness probe: `GET /readyz` on port 8081 (CONNECT listener bound — gates Service EndpointSlice membership; see Q42 fix)
- Security context: `runAsNonRoot: true`, `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`
- Labels: `app: actions-gateway-proxy`, `app.kubernetes.io/managed-by: actions-gateway-gmc`

The `HorizontalPodAutoscaler` targets this Deployment with `minReplicas`/`maxReplicas` from the spec and `targetCPUUtilizationPercentage` (default 60). The `PodDisruptionBudget` sets `minAvailable: 1`.

### 3.7 Finalizer and deletion

The GMC uses a finalizer (`actions-gateway.github.com/gmc-cleanup`) on the `ActionsGateway` CR. On delete reconcile:

1. Delete all `RunnerGroup` CRs in the namespace owned by this CR (wait for deletion — this allows the AGC to deregister sessions and clean up agent Secrets before its Deployment is removed).
2. Delete the AGC `Deployment`.
3. Delete HPA, PDB, Proxy `Service`, Proxy `Deployment` (order not critical; all are independent).
4. Delete `ResourceQuota`, `NetworkPolicy`.
5. Delete `RoleBinding`, `Role`.
6. Delete `ServiceAccount` for AGC and worker.
7. Remove the finalizer.

The namespace itself is never deleted — the tenant owns it.

RunnerGroup deletion wait: after issuing the delete call, the reconciler re-queues with a short delay until no RunnerGroup CRs with this gateway's owner label remain. Use `ctrl.Result{RequeueAfter: 5 * time.Second}` until the list is empty.

### 3.8 Status reporting

After reconciling owned resources, the reconciler reads:
- Proxy Deployment `status.readyReplicas` → `status.proxyReadyReplicas`
- AGC Deployment `status.readyReplicas` (≥1 = available)

Conditions set:

| Type | True when |
|---|---|
| `Ready` | Both `ProxyAvailable` and `AGCAvailable` are true |
| `ProxyAvailable` | `proxyReadyReplicas >= spec.proxy.minReplicas` |
| `AGCAvailable` | AGC Deployment has ≥ 1 ready pod |

The reconciler watches Deployments in addition to ActionsGateway CRs:

```go
ctrl.NewControllerManagedBy(mgr).
    For(&v1alpha1.ActionsGateway{}).
    Owns(&appsv1.Deployment{}).
    Complete(r)
```

This ensures Deployment `ReadyReplicas` changes trigger a reconcile and status is kept current.

### 3.9 IP range background reconciler (`ipranges.go`)

```go
// GitHubIPRangeFetcher fetches the current GitHub IP ranges.
// The default implementation calls https://api.github.com/meta.
// Tests inject a stub that returns a fixed set of CIDRs.
type GitHubIPRangeFetcher interface {
    FetchIPRanges(ctx context.Context) ([]net.IPNet, error)
}

// IPRangeReconciler is a controller-runtime Runnable that periodically
// refreshes NetworkPolicy egress rules for all managed ActionsGateway CRs.
type IPRangeReconciler struct {
    client.Client
    Fetcher  GitHubIPRangeFetcher
    Interval time.Duration // default 24h
    Log      *slog.Logger
}

// Start implements manager.Runnable. It runs until ctx is cancelled.
func (r *IPRangeReconciler) Start(ctx context.Context) error
```

On each tick: fetch IP ranges, list all `ActionsGateway` CRs cluster-wide, and for each CR where `spec.proxy.managedNetworkPolicy` is true, patch the `NetworkPolicy`'s egress rules to reflect the current GitHub CIDR set. Emit `actions_gateway_ip_range_updates_total` on each successful patch.

The `IPRangeReconciler` is registered with the manager via `mgr.Add(ipRangeReconciler)` in `main.go`.

---

## 4. Admission Webhook (`cmd/gmc/internal/webhook/`)

### 4.1 Validating webhook

```go
// ActionsGatewayValidator implements admission.CustomValidator.
type ActionsGatewayValidator struct{}

// ValidateCreate rejects CRs created in reserved namespaces.
func (v *ActionsGatewayValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error)

// ValidateUpdate is a no-op (namespace cannot change on update).
func (v *ActionsGatewayValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error)

// ValidateDelete is a no-op.
func (v *ActionsGatewayValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error)
```

Reserved namespaces (default static set): `kube-system`, `kube-public`, `gmc-system`. The GMC's actual install namespace is also added at setup time via the `POD_NAMESPACE` downward-API env var, so custom installs are protected without code changes.

`ValidateCreate` checks `obj.(*v1alpha1.ActionsGateway).Namespace` against the reserved list. If matched, returns a `field.Invalid` error with a human-readable message.

**Webhook configuration fields:**
- `failurePolicy: Fail` — requests are rejected if the webhook pod is unhealthy
- `matchPolicy: Equivalent`
- `rules`: CREATE, UPDATE on `actionsgateways.actions-gateway.github.com`
- `sideEffects: None`

**TLS:** cert-manager manages the serving certificate. A `Certificate` and self-signed `Issuer` are committed under `cmd/gmc/config/webhook/`. The `caBundle` in the `ValidatingWebhookConfiguration` is injected via cert-manager's `cert-manager.io/inject-ca-from` annotation. For `kind` cluster testing, use a self-signed issuer.

Register with the manager in `main.go`:

```go
if err := (&webhook.ActionsGatewayValidator{}).SetupWebhookWithManager(mgr); err != nil {
    return fmt.Errorf("setup webhook: %w", err)
}
```

---

## 5. CONNECT Proxy (`cmd/proxy/`)

### 5.1 Core implementation (`proxy.go`)

```go
// Server is a minimal stateless HTTPS CONNECT proxy.
// It handles only CONNECT tunneling — no TLS termination, no inspection.
type Server struct {
    // Addr is the listen address for CONNECT requests. Default ":8080".
    Addr string
    // HealthAddr is the listen address for /healthz. Default ":8081".
    HealthAddr string
    // DialTimeout is the upstream TCP dial timeout. Default 10s.
    DialTimeout time.Duration
    Log         *slog.Logger
}

// ListenAndServe starts both the CONNECT listener and the health server.
// Blocks until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error
```

The CONNECT handler:

```go
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodConnect {
        http.Error(w, "only CONNECT is supported", http.StatusMethodNotAllowed)
        return
    }
    upstream, err := net.DialTimeout("tcp", r.Host, s.dialTimeout())
    if err != nil {
        http.Error(w, "upstream dial: "+err.Error(), http.StatusBadGateway)
        return
    }
    defer upstream.Close()

    hijacker, ok := w.(http.Hijacker)
    if !ok {
        http.Error(w, "hijack unsupported", http.StatusInternalServerError)
        return
    }
    conn, _, err := hijacker.Hijack()
    if err != nil {
        return
    }
    defer conn.Close()

    _, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")

    // Relay bytes bidirectionally until either side closes.
    done := make(chan struct{}, 2)
    relay := func(dst, src net.Conn) {
        defer func() { done <- struct{}{} }()
        _, _ = io.Copy(dst, src)
        // Half-close so the other goroutine unblocks.
        if tc, ok := dst.(*net.TCPConn); ok {
            _ = tc.CloseWrite()
        }
    }
    go relay(upstream, conn)
    go relay(conn, upstream)
    <-done
}
```

The health server listens on `HealthAddr` and serves two endpoints: `GET /healthz` always returns `200 OK` (liveness — process is up); `GET /readyz` returns `200 OK` only after the CONNECT listener has bound (readiness — gates Service EndpointSlice membership, see Q42 fix). Both share the port so the existing single `containerPort: 8081` keeps working.

### 5.2 Metrics

The proxy exposes Prometheus metrics on the health port at `/metrics`:

| Metric | Type | Description |
|---|---|---|
| `actions_gateway_proxy_connections_active` | Gauge | Currently active CONNECT tunnels |
| `actions_gateway_proxy_connections_total` | Counter | Total CONNECT tunnels opened |
| `actions_gateway_proxy_dial_errors_total` | Counter | Upstream dial failures |
| `actions_gateway_proxy_tunnel_duration_seconds` | Histogram | Tunnel lifetime, observed at close — surfaces how often tunnels approach the hard lifetime cap |

### 5.3 Configuration

| Env var | Default | Description |
|---|---|---|
| `PROXY_PORT` | `8080` | CONNECT listener port |
| `PROXY_HEALTH_PORT` | `8081` | Health + metrics port |
| `PROXY_DIAL_TIMEOUT` | `10s` | Upstream TCP dial timeout |

### 5.4 Dockerfile

```dockerfile
FROM golang:1.26 AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/proxy .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/proxy /proxy
ENTRYPOINT ["/proxy"]
```

The distroless base provides a minimal attack surface with no shell. The binary is statically linked (`CGO_ENABLED=0`).

---

## 6. Changes to the AGC (`cmd/agc/`)

### 6.1 Proxy env injection into worker pods (`internal/provisioner/provisioner.go`)

The `Provisioner` struct gains three fields:

```go
// HTTPProxy, HTTPSProxy, and NoProxy are forwarded into the runner container
// env of every worker pod. Set from the AGC's own environment by main.go.
HTTPProxy  string
HTTPSProxy string
NoProxy    string
```

In `buildPod`, after the runner container is identified or injected, overwrite the reserved proxy env vars unconditionally (tenant values in the PodTemplate for these keys are not permitted and are rejected at admission in a later milestone; for M4 we overwrite silently):

```go
// Inject proxy env vars into the runner container (controller-enforced invariants).
proxyEnvs := []corev1.EnvVar{
    {Name: "HTTP_PROXY",  Value: p.HTTPProxy},
    {Name: "HTTPS_PROXY", Value: p.HTTPSProxy},
    {Name: "NO_PROXY",    Value: p.NoProxy},
}
c.Env = mergeEnvOverride(c.Env, proxyEnvs)
```

`mergeEnvOverride` replaces existing entries with the same name and appends new ones, preserving all other env vars from the tenant template:

```go
// mergeEnvOverride appends or replaces env vars in base with those in overrides.
// Entries in overrides take precedence; base entries with the same Name are dropped.
func mergeEnvOverride(base, overrides []corev1.EnvVar) []corev1.EnvVar
```

### 6.2 AGC `main.go` — read proxy env and set on provisioner

```go
prov := provisioner.NewProvisioner(mgr.GetClient(), m, nil)
prov.WorkerSA    = os.Getenv("WORKER_SERVICE_ACCOUNT")
prov.HTTPProxy   = os.Getenv("HTTP_PROXY")
prov.HTTPSProxy  = os.Getenv("HTTPS_PROXY")
prov.NoProxy     = os.Getenv("NO_PROXY")
if img := os.Getenv("WORKER_IMAGE"); img != "" {
    prov.DefaultWorkerImage = img
}
prov.TokenFunc = tokenMgr.Token
```

These env vars are injected by the GMC into the AGC Deployment (§3.5); when running the AGC directly (development/CI), they default to empty strings, which causes the proxy env vars to be injected as empty — acceptable for local testing without a proxy.

### 6.3 No new RBAC markers needed

The AGC already has the pod and secret RBAC markers from M3. The GMC-created Role (§3.3) is the production RBAC; the AGC's own ClusterRole is retained for standalone development use.

---

## 7. GMC `main.go`

```go
// cmd/gmc/main.go
//
// Required environment variables (set by the GMC Deployment manifest):
//   AGC_IMAGE    — the AGC container image (e.g. ghcr.io/my-org/agc:v0.4.0)
//   PROXY_IMAGE  — the proxy container image (e.g. ghcr.io/my-org/proxy:v0.4.0)
//
// Optional:
//   LEADER_ELECTION_NAMESPACE — defaults to the pod's own namespace
//   IP_RANGE_INTERVAL         — GitHub IP range refresh interval (default 24h)
package main
```

The `run()` function:
1. Reads `AGC_IMAGE` and `PROXY_IMAGE` from env (required; fatal if absent).
2. Creates the controller-runtime manager with `LeaderElection: true`, `LeaderElectionID: "actions-gateway-gmc-leader"`, `LeaderElectionNamespace`.
3. Sets up the `ActionsGatewayReconciler` with `mgr.Add`.
4. Sets up the webhook server with `webhook.ActionsGatewayValidator{}`.
5. Creates and registers the `IPRangeReconciler` with `mgr.Add`.
6. Calls `mgr.Start(ctx)`.

---

## 8. Test Plan

### 8.1 Unit tests

#### Webhook (`webhook/actionsgateway_webhook_test.go`)

| Test | What it verifies |
|---|---|
| `TestWebhook_RejectsKubeSystem` | CR in `kube-system` → admission error returned. |
| `TestWebhook_RejectsKubePublic` | CR in `kube-public` → admission error. |
| `TestWebhook_RejectsDefaultGMCNamespace` | CR in `gmc-system` → admission error. Default install namespace, rejected even when POD_NAMESPACE is unset. |
| `TestWebhook_RejectsCustomInstallNamespace` | CR in a non-default install namespace passed to the validator constructor → admission error. Covers downward-API-driven reservation. |
| `TestWebhook_AllowsTenantNamespace` | CR in `team-a` → no error. |
| `TestWebhook_UpdateAllowsSafe` | Update call returns no error when neither old nor new contains a privileged container. |

#### Resource builders (`controller/builder_test.go`)

| Test | What it verifies |
|---|---|
| `TestBuildAGCDeployment_SecretRefs` | All three gitHubAppRef keys mapped to correct secretKeyRef env vars. |
| `TestBuildAGCDeployment_ProxyEnv` | `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` set to proxy Service address. |
| `TestBuildAGCDeployment_WorkerSA` | `WORKER_SERVICE_ACCOUNT` env set to `"actions-gateway-worker"`. |
| `TestBuildProxyDeployment_DefaultResources` | Default CPU/memory requests and limits applied when `spec.proxy.resources` is empty. |
| `TestBuildProxyDeployment_CustomResources` | `spec.proxy.resources` overrides defaults. |
| `TestBuildProxyDeployment_SecurityContext` | `runAsNonRoot`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false` set. |
| `TestBuildNetworkPolicy_ProxyEgress` | Proxy pod egress rules contain the expected GitHub CIDRs. |
| `TestBuildNetworkPolicy_ManagedFalse` | `spec.proxy.managedNetworkPolicy: false` → no GitHub CIDR egress rules. |
| `TestBuildRole_AGCPermissions` | Generated Role contains expected rules; no `*` verbs on secrets or pods. |
| `TestBuildHPA_MinMaxReplicas` | HPA `minReplicas`/`maxReplicas` match spec values. |

#### IP range reconciler (`controller/ipranges_test.go`)

| Test | What it verifies |
|---|---|
| `TestIPRangeReconciler_UpdatesNetworkPolicy` | Stub fetcher returns new CIDRs; reconciler patches the NetworkPolicy; counter incremented. |
| `TestIPRangeReconciler_SkipsManagedFalse` | CR with `managedNetworkPolicy: false` → NetworkPolicy not patched. |
| `TestIPRangeReconciler_FetchError` | Fetcher returns error → NetworkPolicy not modified; error logged; reconciler continues. |

#### Proxy (`proxy/proxy_test.go`)

| Test | What it verifies |
|---|---|
| `TestProxy_Connect` | Client sends `CONNECT host:port HTTP/1.1`; proxy dials a local echo server; bytes relay in both directions. |
| `TestProxy_NonConnectMethod` | `GET` request returns `405 Method Not Allowed`. |
| `TestProxy_DialFailure` | Target port is closed; proxy returns `502 Bad Gateway`. |
| `TestProxy_HalfClose` | One side closes; relay terminates without goroutine leak (verify with `goleak.VerifyNone`). |
| `TestProxy_HealthEndpoint` | `GET /healthz` on health port returns `200 OK`. |
| `TestProxy_Metrics` | After one successful tunnel, active connections gauge transitions 0→1→0. |

#### AGC provisioner — proxy env (`cmd/agc/internal/provisioner/provisioner_test.go`)

| Test | What it verifies |
|---|---|
| `TestBuildPod_InjectsProxyEnv` | Provisioner with non-empty HTTPProxy/HTTPSProxy/NoProxy → runner container env contains all three vars with correct values. |
| `TestBuildPod_OverwritesTenantProxyEnv` | PodTemplate sets `HTTP_PROXY=bad`; after build, `HTTP_PROXY` equals the provisioner value, not `bad`. |

#### RBAC regression test (`controller/rbac_test.go`)

Reads `config/rbac/role.yaml` from the filesystem (relative to the test file) and asserts:
- No rule has verb `"*"` on any resource.
- No rule contains `secrets`, `pods`, or `nodes` in its `resources` list alongside a `"*"` verb.
- The ClusterRole name matches the expected constant.

```go
func TestClusterRole_NoWildcardVerbs(t *testing.T) {
    data, err := os.ReadFile("../../config/rbac/role.yaml")
    // parse YAML → ClusterRole
    for _, rule := range role.Rules {
        for _, verb := range rule.Verbs {
            require.NotEqual(t, "*", verb, "wildcard verb found: %v", rule)
        }
    }
}

func TestClusterRole_NoWildcardOnSensitiveResources(t *testing.T) {
    sensitive := sets.New("secrets", "pods", "nodes")
    for _, rule := range role.Rules {
        for _, resource := range rule.Resources {
            if sensitive.Has(resource) {
                for _, verb := range rule.Verbs {
                    require.NotEqual(t, "*", verb,
                        "wildcard verb on sensitive resource %q", resource)
                }
            }
        }
    }
}
```

### 8.2 Controller tests (`controller/`)

The kubebuilder-scaffolded envtest suite (`suite_test.go`, `actionsgateway_controller_test.go`) was intentionally replaced with fake-client unit tests. envtest requires etcd binaries (`make setup-envtest`) and is not appropriate for a pure unit test run. The reconciler behaviours below are covered by the existing unit tests in `builder_test.go` and `ipranges_test.go` using `sigs.k8s.io/controller-runtime/pkg/client/fake`.

| Scenario | Covered by |
|---|---|
| Resource builders produce correct specs | `builder_test.go` (10 tests) |
| IP range reconciler patches NetworkPolicy | `ipranges_test.go` — `TestIPRangeReconciler_UpdatesNetworkPolicy` |
| Reconciler skips `managedNetworkPolicy: false` | `ipranges_test.go` — `TestIPRangeReconciler_SkipsManagedFalse` |
| Fetch error leaves NetworkPolicy unchanged | `ipranges_test.go` — `TestIPRangeReconciler_FetchError` |
| Webhook rejects reserved namespaces | `webhook/actionsgateway_webhook_test.go` (5 tests) |
| RBAC: no wildcard verbs on sensitive resources | `rbac_test.go` (2 tests) |

Full reconciler lifecycle scenarios (create, delete, status, two-CR isolation) are deferred to §8.3 manual verification or a future Milestone 5 envtest suite once `setup-envtest` is integrated into the CI toolchain.

### 8.3 Manual end-to-end verification

After integration tests pass, deploy the full stack to a `kind` cluster:

1. Apply the GMC Deployment in `gmc-system`.
2. Apply an `ActionsGateway` CR in namespace `team-a`; wait for `status.conditions[Ready]=True`.
3. `kubectl get all,networkpolicy,resourcequota,rolebinding -n team-a` — confirm all 12 resource types are present.
4. Dispatch a real GitHub Actions workflow job to a runner with the matching `runs-on` label.
5. Confirm in AGC logs: `job message received → AcquireJob → Secret created → Pod created`.
6. In GitHub Actions UI: job appears running; step output streams; green checkmark on completion.
7. Confirm `HTTP_PROXY` is set in the worker pod: `kubectl exec -n team-a <worker-pod> -- env | grep PROXY`.
8. Apply a second `ActionsGateway` CR in namespace `team-b`; confirm its resources are created independently.
9. Delete the `team-a` CR; confirm only `team-a` resources are removed; `team-b` is unaffected.
10. Update `team-b` CR's `spec.proxy.maxReplicas`; confirm HPA change within 10s.

---

## 9. Success Criteria Checklist

- [x] Two `ActionsGateway` CRs in a `kind` cluster produce two independent, functional tenant setups. *(2026-06-12 live — §12)*
- [x] Deleting one CR removes only that tenant's resources. *(2026-06-12 live — §12)*
- [ ] `spec.proxy.maxReplicas` change reflected in HPA within one reconcile cycle. *(covered by `hpa_update_test.go`; not exercised in the §12 live session)*
- [ ] Admission webhook rejects CRs in `kube-system`, `kube-public`, and `gmc-system` (plus the GMC's install namespace via the `POD_NAMESPACE` downward-API env var when non-default). *(unit-tested; not exercised in the §12 live session)*
- [x] End-to-end job completes with green checkmark via proxy (confirmed by `HTTPS_PROXY` in worker pod env). *(2026-06-12 live — §12)*
- [ ] RBAC regression tests pass: no `*` verbs on `secrets`, `pods`, or `nodes` in the GMC ClusterRole.
- [ ] `go test -race ./...` passes across all four modules (root, agc, gmc, proxy).
- [ ] Worker ServiceAccount `actions-gateway-worker` created by GMC and used by provisioner.
- [ ] Proxy pods have `runAsNonRoot`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`.
- [ ] IP range reconciler updates NetworkPolicy on a 24h tick (verified in unit test with stub fetcher).
- [ ] GMC runs with `replicas: 2` and leader election; SIGTERM causes clean shutdown.
- [ ] Code is committed to the repository.

---

## 10. Risks and Mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| cert-manager not available in the test cluster | Medium | Medium | For `kind` testing, generate a self-signed certificate manually and skip cert-manager injection. The webhook can be configured with `failurePolicy: Ignore` during development; flip to `Fail` for the milestone validation. |
| GMC ClusterRole requires `rbac.authorization.k8s.io/v1` Role creation, which itself requires the GMC SA to have `escalate` or equivalent | Medium | Medium | The GMC SA must be granted `bind` and `escalate` on the Roles it creates, or the Roles it creates must be a strict subset of its own permissions. Verify this in the `kind` cluster before finalizing the generated Role rules. |
| `HTTP_PROXY` env var intercepted by the AGC's own Go HTTP client (calling Kubernetes API) | High | Medium | `NO_PROXY` must include `kubernetes.default.svc.cluster.local` and the service CIDR. The `buildNoProxy` function always includes these regardless of the tenant's `noProxyCIDRs` field. Verify in end-to-end test that the AGC can reach the Kubernetes API server with proxy env set. |
| GitHub IP ranges change between provisioning and the 24h refresh cycle | Low | Low | The 24h refresh loop is the mitigation. If an IP range changes within the window, proxy pods can still reach GitHub (NetworkPolicy blocks egress FROM proxy pods, not TO GitHub — actually the NetworkPolicy allows egress to the listed CIDRs, so a new GitHub IP not yet in the list would be blocked). Operators using `managedNetworkPolicy: false` with FQDN-based policies are unaffected. |
| CONNECT proxy goroutine leak on abrupt client disconnect | Medium | Medium | The `TestProxy_HalfClose` test and `goleak.VerifyNone` assertion catch this. The half-close pattern (`CloseWrite` on the write-side) ensures both relay goroutines unblock and exit. |
| Two-namespace test isolation failure (resources from one CR leaking into another namespace) | Low | High | Covered by the "Two concurrent ActionsGateway CRs" integration test. Labels and namespace selectors in all `List` calls are the defense layer; the test verifies isolation. |

---

## 11. Deferred to Milestone 5

- **Hardened GMC pod spec** — read-only root filesystem, `seccompProfile: RuntimeDefault`, non-root user, no host mounts. The GMC pod in M4 uses a basic security context; full hardening is M5.
- **Hardened proxy pod spec** — `seccomp`, dropped capabilities, `allowPrivilegeEscalation: false` beyond the basics set in M4.
- **Production Helm chart or Kustomize overlays** — M4 ships raw manifests under `cmd/gmc/config/`. Packaging is M5.
- **Multi-tenant load test** — the `test/load/` harness that simulates 10 tenants is M5.
- **`gVisor`/Kata `RuntimeClass`** — optional worker isolation hardening is M5.
- **CRD CEL admission rules for reserved worker pod fields** — the webhook in M4 only rejects reserved namespaces. Rejecting reserved PodTemplate fields (`hostPID`, `HTTP_PROXY` in env, etc.) via CEL rules is M5. In M4 the provisioner silently overwrites them.
- **`kube-bench`/`polaris` scan** — cluster posture audit is M5.

---

## 12. Live multi-tenant validation evidence (2026-06-11/12)

One session on a 3-node kind cluster (`make e2e-cluster`, kindnet CNI,
cert-manager installed) with the real GitHub App `actions-gateway-test`
(App ID 3752347, installation 135739122) against the repo
`actions-gateway/gateway-test` (workflow `test-job.yml`, `runs-on: e2e`).
This session also served as the [Q12 track-A live `helm install` proof](q12-helm-chart.md#live-validation-track-a--2026-06-12)
and closed Q71.

### Setup

- Images built and pushed by `make e2e-images` at commit `042a4f5`; all
  refs below are pinned by registry digest (no `--allow-floating-image-tags`).
- GMC installed **via the Helm chart only** (no kustomize `make deploy`):

  ```
  helm install actions-gateway charts/actions-gateway -n gmc-system --create-namespace \
    --set gmc.image.repository=localhost:5000/gmc   --set gmc.image.digest=sha256:190d138c… \
    --set agc.image.repository=localhost:5000/agc   --set agc.image.digest=sha256:23d9fe5d… \
    --set proxy.image.repository=localhost:5000/proxy --set proxy.image.digest=sha256:d497c4d0…
  # deployment 2/2 Available in ~10 s; AGC_IMAGE/PROXY_IMAGE env digest-pinned
  ```

- **Deviation from pure production posture (at the time of this run):** the
  AGC's org URL had no CR field, so the GMC was patched with the testing-gated
  `--allow-agc-extra-env=true` + `AGC_EXTRA_GITHUB_ORG_URL=https://github.com/actions-gateway/gateway-test`
  (same mechanism the Tier-C suite uses). **Resolved by Q116:** the org URL is
  now a first-class required `spec.gitHubURL` field threaded to the AGC as
  `GITHUB_ORG_URL`, so this workaround is no longer needed — set
  `spec.gitHubURL` (or chart value `sampleGateway.gitHubURL`) on the CR instead.

### Multi-tenant (DoD row 1)

Two namespaces (`tenant-a`, `tenant-b`, both labelled
`actions-gateway.github.com/tenant=true`), each with a real-credential
`github-app-creds` Secret and an `ActionsGateway` CR (proxy
`minReplicas: 2`; runner labels `e2e` / `e2e-b` respectively). Within
~60 s both CRs reported `Ready=True`, `PROXYREADY 2`; each namespace
held its own AGC Deployment (1/1), proxy Deployment (2/2), Service,
HPA, 3 NetworkPolicies, Role/RoleBinding, both ServiceAccounts, and a
RunnerGroup with `ActiveSessions ≥ 1`. All four virtual runners
appeared in the repo's runner list (`gh api …/actions/runners`), one
listener per tenant `online`. The AGC's installation-token fetch
demonstrably routed through the tenant proxy (first attempt while the
proxy was still starting logged
`proxyconnect tcp: dial tcp 10.96.129.62:8080: connect: connection refused`,
then succeeded).

### End-to-end job through the proxy (DoD row 5) + Q71

`gh workflow run test-job.yml` → job acquired by `tenant-a`, worker pod
`Running`, GitHub-side runs concluded **success** twice:
[27386891757](https://github.com/actions-gateway/gateway-test/actions/runs/27386891757)
(before the delete-isolation step) and
[27395702908](https://github.com/actions-gateway/gateway-test/actions/runs/27395702908)
(after it). Worker pod env (observed live):

```
HTTP_PROXY=https://actions-gateway-proxy.tenant-a.svc.cluster.local:8080
HTTPS_PROXY=https://actions-gateway-proxy.tenant-a.svc.cluster.local:8080
NO_PROXY=svc.cluster.local,svc.cluster.local,localhost,127.0.0.1,10.96.0.0/12
PROXY_CA_CERT_PATH=/etc/actions-gateway/proxy-ca/tls.crt
```

Worker logs showed step/job log uploads to the results service at 3/3
success rate (all via the proxy tunnel; the Q5h proxy-CA trust chain
held).

**Q71 (runner-version contract):** session creation against real GitHub
succeeded — but the live truth is subtler than the Queue row assumed:
the GMC-provisioned AGC never sets `GITHUB_RUNNER_VERSION`, so
`CreateSession` sends an **empty** `agent.version` (and `GetMessage`
omits `runnerVersion`), which GitHub accepts. The 2.334.0/2.335.x pin
exists only in the worker image. Recorded as Q118 (set the env in the
GMC + fix the Dockerfile-vs-`DefaultWorkerImage` drift Dependabot
introduced in #197). **Resolved (Q118):** the runner version now lives in
one place — `RunnerVersion` in `cmd/agc/names` — which drives
`DefaultWorkerImage` (digest-pinned to 2.335.1) and the
`GITHUB_RUNNER_VERSION` the GMC injects, so `agent.version` is non-empty
and matches the worker binary; a lockstep unit test guards against future
Dockerfile/constant drift.

### Delete isolation (DoD row 2)

`kubectl delete actionsgateway gateway-b -n tenant-b` returned after the
finalizer drained; afterwards `tenant-b` contained only the namespace
itself, the `default` ServiceAccount, and the operator-created
`github-app-creds` Secret (the namespace is never deleted by design).
`tenant-a` was untouched (`Ready=True`, AGC + 2/2 proxy pods running,
RunnerGroup intact) and subsequently ran run 27395702908 to green.
Deleting `gateway-a` at the end, with a healthy session, also
**deregistered its runners from GitHub** — the repo runner list went
empty.

### Product bugs surfaced (all filed in [STATUS.md](../STATUS.md))

The live run worked **only after** working around these; none are
caught by unit/Tier-A/Tier-B tiers:

1. **Q115 — default worker SecurityContext breaks the runner image.**
   Q31's `applySecurityDefaults` stamps pod-level `runAsNonRoot: true`;
   the `actions-runner` image uses non-numeric `USER runner`, so kubelet
   fails every default-path worker pod with
   `CreateContainerConfigError: container has runAsNonRoot and image has
   non-numeric user`. Worked around per-tenant with
   `podTemplate.spec.securityContext.runAsUser: 1001`. Tier-B masked
   this by using the **agc image** as its worker placeholder.
   **Fixed (Q115):** `applySecurityDefaults` now gap-fills
   `runAsUser: 1001` (the runner image's UID) alongside
   `runAsNonRoot: true` whenever non-root is enforced, so the default
   path is kubelet-admissible; the gap-fill is skipped when a tenant sets
   `runAsNonRoot: false` and an explicit `runAsUser` still wins. A new
   Tier-B spec (`E2E_AGC_WorkerSecurityContext`) provisions a worker pod
   from the **real** worker image and asserts kubelet admits it, so a
   regression of the default is caught in CI rather than only live.
2. **Q114 — JIT agents are single-use and the AGC cannot self-heal.**
   GitHub removed each JIT runner after it completed (or had acquired a
   then-cancelled) job; the never-used agent survived. The AGC kept
   polling with the stale agent/session — `GetMessage` looped on
   `200`-with-empty-body (`decode response: EOF`) and later
   `401 unauthorized` for hours with no re-registration; recovery
   required deleting the agentpool Secrets + restarting the AGC, and
   re-registration of a *surviving* name then failed `409 Already
   exists` (no deregister-then-retry). This breaks the
   multiplex-many-jobs-per-agent assumption at its root.
   **Fixed (Q114):** the listener now re-registers its agent after every
   job (and heals 401/EOF-stale sessions found after a restart,
   resolving the surviving-name 409 by ID lookup); fakegithub can
   simulate the single-use behaviour for Tier B regression coverage —
   see [q114-jit-agent-selfheal.md](q114-jit-agent-selfheal.md).
3. **Q117 — RunnerGroup `podTemplate` changes don't reach running
   listeners.** After patching the CR (observedGeneration advanced),
   newly provisioned worker pods still used the old template until the
   AGC pod was restarted.
4. **Q116 — no production path for the GitHub org URL** (see Setup
   deviation above). **Fixed (Q116):** added the required first-class
   `ActionsGateway.spec.gitHubURL` field, threaded to the AGC Deployment as
   `GITHUB_ORG_URL` and validated (https scheme + org/owner path) by the GMC
   webhook; the testing-only `--allow-agc-extra-env` flag is retained for
   genuinely-extra env but is no longer required for the org URL.

Operator-facing runbook entry for (1):
[troubleshooting.md](../operations/troubleshooting.md#worker-pod-fails-to-start-after-secure-by-default-securitycontext).
