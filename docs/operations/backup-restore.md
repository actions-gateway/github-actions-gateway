# Backup, Restore, and Disaster Recovery

> **Audience:** SRE, Platform engineer

The `ActionsGateway` custom resource (CR) is the **source of truth** for a tenant gateway. The Gateway Manager Controller (GMC) provisions every per-gateway child resource — the Actions Gateway Controller (AGC) Deployment, ServiceAccounts, RoleBinding, Service, NetworkPolicies, and metrics TLS Secrets — from that single CR, and owner-references each child to it. Deleting the CR therefore **cascades**: the apiserver garbage-collects all owner-referenced children. This makes the gateway cheap to recreate, but it also means an accidental `kubectl delete actionsgateway` (or a deleted namespace) tears down the running control plane.

This document covers the backup posture that protects against that, and a runnable recovery procedure for restoring a deleted or corrupted gateway.

For component upgrade/rollback (a different concern — the binary version, not the CR) see [upgrade.md](upgrade.md). For symptom-driven diagnosis see [troubleshooting.md](troubleshooting.md).

---

## Table of Contents

- [What Is and Is Not Owned by the CR](#what-is-and-is-not-owned-by-the-cr)
- [Backup Posture](#backup-posture)
  - [Primary: GitOps](#primary-gitops)
  - [Secondary: etcd and per-resource backups](#secondary-etcd-and-per-resource-backups)
  - [What to Back Up](#what-to-back-up)
- [Recovery Runbook](#recovery-runbook)
  - [Scenario A: Deleted or Corrupted ActionsGateway CR](#scenario-a-deleted-or-corrupted-actionsgateway-cr)
  - [Scenario B: Whole Tenant Namespace Lost](#scenario-b-whole-tenant-namespace-lost)
  - [Scenario C: Full Cluster / etcd Restore](#scenario-c-full-cluster--etcd-restore)
  - [CR Stuck Terminating](#cr-stuck-terminating)
- [Verification](#verification)

---

## What Is and Is Not Owned by the CR

Restore planning hinges on one distinction: **GMC-generated children reconcile back automatically from the CR; operator-supplied inputs do not.** Re-applying the CR rebuilds everything the GMC owns, but it cannot reconstruct a credential it never created.

**Reconciles automatically from the CR** (owner-referenced, garbage-collected on CR deletion, regenerated on re-apply):

| Child resource | Notes |
| --- | --- |
| AGC `Deployment` | Rebuilt from `spec`; the AGC reconstructs all in-memory session state on startup. |
| AGC + worker `ServiceAccount` | Per-gateway names. |
| AGC `RoleBinding` | Namespaced; binds the AGC ServiceAccount to the shipped tenant `ClusterRole`. |
| AGC metrics `Service` | mTLS metrics endpoint. |
| Workload + AGC `NetworkPolicy` | Egress lockdown, rebuilt per egress mode. |
| Metrics TLS `Secret`s (server + client) | **Regenerated fresh** by the GMC — new key material each restore. A `ServiceMonitor` referencing the published client Secret picks up the new material automatically (same Secret name). |

**Does *not* reconcile from the CR — must already exist or be recreated separately:**

| Resource | Why it is not restored by re-applying the CR | Recovery |
| --- | --- | --- |
| GitHub App credential `Secret` (`spec.gitHubAppRef`) | Operator/tenant-supplied; the GMC only *reads* it (no owner reference), so it **survives** a CR-only deletion but is lost if the Secret or namespace is deleted. Without it the gateway sits `Ready=False`/`CredentialUnavailable`. | Recreate from your secret backup before re-applying the CR. See [Getting Started §3](../getting-started.md#3-create-a-github-app-credential-secret). |
| Tenant `Namespace` + Pod Security Admission labels | The namespace is operator-created; the GMC stamps PSA labels but does not create the namespace. | Recreate the namespace (the GMC re-stamps PSA labels on reconcile). |
| Namespace `ResourceQuota` | Platform-owned — not a field on the CR. See [Runbook — Adjusting Tenant Quota](runbook.md#adjusting-tenant-quota). | Re-apply from version control. |
| `EgressProxy` CR (`spec.defaultProxyRef`) | A separate CR with its own owned children; not owned by the `ActionsGateway`. A `defaultProxyRef` pointing at a missing proxy fails closed (`Ready=False`/`ProxyNotFound`). | Re-apply the `EgressProxy` CR before (or alongside) the gateway. |
| The cluster-scoped `ClusterRoleBinding` granting the AGC read of `ClusterRunnerTemplate` | Cluster-scoped objects cannot carry an owner reference to a namespaced CR, so the GMC deletes it explicitly via its cleanup finalizer and recreates it on reconcile. | Reconciles automatically from the CR — no action needed. |
| `RunnerSet` CRs that target the gateway | Reference the gateway but are **not** owned by it; they are never deleted by gateway teardown — they degrade to `Ready=False`/`GatewayNotFound` and recover when the gateway returns. | No action needed; they re-bind automatically. |

> **In-flight jobs are not recoverable.** Running jobs whose renew loop lapses during the outage are cancelled by GitHub and require a manual re-run. Queued (not-yet-acquired) jobs are redelivered to the next healthy session within ~2 minutes. This matches [Runbook — AGC Total Failure](runbook.md#agc-total-failure).

---

## Backup Posture

### Primary: GitOps

**Keep the desired state in version control and let a GitOps controller (Argo CD, Flux, or `kubectl apply` from a pinned manifest repo) be the restore mechanism.** This is the recommended posture: the CR is small, declarative, and fully describes the gateway, so a Git repository plus a reconcile loop *is* your backup and your restore.

Track in version control, per tenant namespace:

- The `ActionsGateway` CR.
- Any `EgressProxy` CR referenced by `spec.defaultProxyRef`.
- The tenant `Namespace` manifest (including its security-profile label).
- The namespace `ResourceQuota`.

**Never commit the GitHub App credential `Secret` in plaintext.** Manage it with a secrets-aware tool — [Sealed Secrets](https://github.com/bitnami-labs/sealed-secrets), [External Secrets Operator](https://external-secrets.io/), or SOPS-encrypted manifests — so the encrypted form is safe in Git and the plaintext only ever exists in the cluster. The private key itself is never recoverable from the cluster (it is held only inside the Secret); keep the source `.pem` in a password manager or secrets vault so a new Secret can be minted if the namespace is lost. See [Security Operations](security-operations.md) for credential-handling guidance.

### Secondary: etcd and per-resource backups

GitOps protects the *desired* state. To also capture live status and resources not in Git, add one of:

- **etcd snapshots** — a cluster-level backup (`etcdctl snapshot save`, or your managed-cluster provider's equivalent) captures every object, including the credential Secret and live CR status. This is the broadest safety net and the basis for [Scenario C](#scenario-c-full-cluster--etcd-restore). Treat the snapshot as sensitive: it contains every Secret in the cluster.
- **Namespace-scoped backups** — a tool such as [Velero](https://velero.io/) can back up and restore a tenant namespace (CRs, Secrets, quota) as a unit, which is convenient for [Scenario B](#scenario-b-whole-tenant-namespace-lost). For concrete Velero backup/restore commands grounded in the ownership model above — including the label selector that skips the GMC-owned children so the controllers rebuild them — see [Velero Backup and Restore](velero-backup-restore.md).
- **Ad-hoc CR export** — for a point-in-time copy of a single gateway:

  ```sh
  kubectl get actionsgateway -n <namespace> <name> -o yaml > backup-<name>.yaml
  ```

  Strip the runtime fields before re-applying (`status`, `metadata.uid`, `metadata.resourceVersion`, `metadata.creationTimestamp`, and the `metadata.finalizers` entry) — re-applying them can wedge the apply or resurrect a stale finalizer. The spec is the only part you need.

### What to Back Up

| What | Where it should live | Restores |
| --- | --- | --- |
| `ActionsGateway` CR (spec) | Git (GitOps) | The whole gateway control plane |
| `EgressProxy` CR (spec), if used | Git (GitOps) | The egress proxy pool |
| `Namespace` + security-profile label | Git (GitOps) | The tenant namespace |
| `ResourceQuota` | Git (GitOps) | Quota enforcement |
| GitHub App credential `Secret` | Encrypted (Sealed Secrets / ESO / SOPS) **and** the source `.pem` in a vault | GitHub authentication |
| Everything, including live status | etcd snapshot / Velero | Whole-cluster or whole-namespace recovery |

---

## Recovery Runbook

Pick the scenario that matches the blast radius.

### Scenario A: Deleted or Corrupted ActionsGateway CR

The namespace and the GitHub App credential Secret are intact; only the CR is gone or has a broken spec.

1. **Confirm the blast radius.** The owned children are already gone (garbage-collected with the CR) or about to be:
   ```sh
   kubectl get actionsgateway,deploy,svc,networkpolicy,sa,rolebinding -n <namespace>
   ```
2. **Confirm the credential Secret survived** (it is not owned by the CR, so a CR-only deletion leaves it in place):
   ```sh
   kubectl get secret -n <namespace> <gitHubAppRef-name>
   ```
   If it is missing, recreate it first — see [Scenario B](#scenario-b-whole-tenant-namespace-lost) step 3.
3. **Re-apply the CR** from version control (or your stripped CR export):
   ```sh
   kubectl apply -n <namespace> -f actionsgateway.yaml
   ```
   If a `defaultProxyRef` is set, ensure the `EgressProxy` CR exists first — apply it in the same step if needed.
4. **Wait for reconcile.** The GMC re-provisions all owned children within ~30 seconds and regenerates the metrics TLS Secrets fresh.
5. **[Verify](#verification).**

### Scenario B: Whole Tenant Namespace Lost

The namespace and everything in it — CR, credential Secret, quota — are gone.

1. **Recreate and mark the namespace** (including its security-profile label) from version control. See [Getting Started §2](../getting-started.md#2-create-and-mark-the-tenant-namespace-and-set-its-quota).
2. **Re-apply the `ResourceQuota`** from version control.
3. **Recreate the GitHub App credential Secret** from your encrypted backup (or mint a fresh one from the source `.pem` in your vault). See [Getting Started §3](../getting-started.md#3-create-a-github-app-credential-secret).
4. **Re-apply the `EgressProxy` CR**, if the gateway references one.
5. **Re-apply the `ActionsGateway` CR.**
6. **[Verify](#verification).** `RunnerSet`s that targeted this gateway flip from `Ready=False`/`GatewayNotFound` back to ready automatically once the gateway is up.

> Order matters only for the credential Secret and the `EgressProxy`: the gateway fails closed (`CredentialUnavailable` / `ProxyNotFound`) until both exist, then reconciles on its own once they appear — no gateway re-apply is needed after they land.

### Scenario C: Full Cluster / etcd Restore

For a control-plane loss, restore etcd from a snapshot per your Kubernetes distribution's documented procedure. After restore:

1. Confirm the GMC is running: `kubectl rollout status deploy/gmc-controller-manager -n gmc-system`.
2. The GMC reconciles every restored `ActionsGateway` CR idempotently — it compares desired vs. actual state and only applies what is missing. No resources are duplicated. This is the same idempotent recovery described in [Runbook — GMC Total Failure](runbook.md#gmc-total-failure).
3. **[Verify](#verification)** each tenant namespace.

### CR Stuck Terminating

If a deleted CR hangs in `Terminating`, the GMC's cleanup finalizer (`actions-gateway.com/gmc-cleanup`) is retained because teardown of an owned or cluster-scoped child could not be confirmed (for example, the GMC is down). The finalizer exists precisely to avoid orphaning the cluster-scoped `ClusterRoleBinding`.

1. Check the GMC is running and reconciling — restart it if needed (`kubectl rollout restart deploy/gmc-controller-manager -n gmc-system`); it will complete teardown and clear the finalizer.
2. Only **force-remove the finalizer as a last resort** when the GMC is permanently gone, and understand the trade-off: it leaves the cluster-scoped `ClusterRoleBinding` (`agc-clusterrunnertemplate-reader.<namespace>.<name>`) orphaned, which you must then delete by hand.
   ```sh
   # Last resort only — orphans the cluster-scoped ClusterRoleBinding.
   kubectl patch actionsgateway -n <namespace> <name> \
     --type=json -p '[{"op":"remove","path":"/metadata/finalizers"}]'
   kubectl delete clusterrolebinding agc-clusterrunnertemplate-reader.<namespace>.<name>
   ```

---

## Verification

After any restore, confirm the gateway is healthy — the same checks as [Runbook — Adding a Tenant](runbook.md#adding-a-tenant):

1. The GMC has re-provisioned the children:
   ```sh
   kubectl get deploy,svc,networkpolicy,sa,rolebinding -n <namespace>
   ```
2. The AGC Deployment is available:
   ```sh
   kubectl rollout status deploy/<agc-deployment-name> -n <namespace>
   ```
3. The CR reports healthy:
   ```sh
   kubectl get actionsgateway -n <namespace> <name> \
     -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}{"\n"}'
   ```
   Expect `True`. If `CredentialUnavailable` or `ProxyNotFound`, the credential Secret or `EgressProxy` is still missing — recheck the relevant scenario step.
4. Sessions are polling — `actions_gateway_active_sessions` should reach ≥ 1 per RunnerGroup within seconds. See [Observability](observability.md).
5. Trigger a test workflow and confirm a worker pod is provisioned and the job completes.

---

## Reference Links

- [Velero Backup and Restore](velero-backup-restore.md) — tool-specific how-to for namespace-level backup/restore with Velero
- [Runbook](runbook.md) — day-2 operations and incident response (AGC/GMC total failure)
- [Troubleshooting](troubleshooting.md) — symptom → diagnosis → resolution
- [Security Operations](security-operations.md) — credential handling and compromise response
- [Getting Started](../getting-started.md) — namespace, credential Secret, and CR creation steps
- [Upgrade and Rollback](upgrade.md) — component version changes (distinct from CR restore)
