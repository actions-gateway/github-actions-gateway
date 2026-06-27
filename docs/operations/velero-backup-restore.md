# Velero Backup and Restore for GAG

> **Audience:** SRE, Platform engineer

This is a tool-specific how-to for backing up and restoring GitHub Actions Gateway (GAG) state with [Velero](https://velero.io/). It assumes you have already read the conceptual [Backup, Restore, and Disaster Recovery (DR)](backup-restore.md) guide — that document explains the **ownership model** this how-to depends on (what the `ActionsGateway` custom resource (CR) owns, what it does not, and why re-applying the CR reconciles the owned children back). This page does not repeat that reasoning; it maps it onto concrete Velero commands.

The single most important consequence for Velero: **the Gateway Manager Controller (GMC) rebuilds every owned child from the CR.** So the goal of a Velero restore is *not* to faithfully recreate every object — it is to restore the **inputs** (the CRs and the operator-supplied resources the controllers cannot regenerate) and let the controllers reconcile the rest. Restoring the owned children directly is at best redundant and at worst harmful (see [Why not restore the owned children directly](#why-not-restore-the-owned-children-directly)).

For the GitOps-first posture (the recommended primary backup), per-resource `kubectl` export, and the full scenario runbook, stay in [backup-restore.md](backup-restore.md). Use this page when Velero is your namespace-level backup tool.

---

## Table of Contents

- [What Velero Should Capture](#what-velero-should-capture)
- [Backing Up](#backing-up)
  - [One-time: the CRDs](#one-time-the-crds)
  - [Per-tenant: the namespace](#per-tenant-the-namespace)
- [Restoring](#restoring)
  - [Ordering: CRDs → inputs and CRs → reconcile](#ordering-crds--inputs-and-crs--reconcile)
  - [Why not restore the owned children directly](#why-not-restore-the-owned-children-directly)
- [Secret Handling Caveats](#secret-handling-caveats)
- [Verification](#verification)

---

## What Velero Should Capture

GAG's control-plane state lives entirely in Kubernetes API objects (etcd) — there are **no persistent volumes** holding gateway state. A Velero *resource* backup is therefore sufficient; you do not need File System Backup (restic/Kopia) or Container Storage Interface (CSI) volume snapshots for the GAG control plane itself. See [Secret Handling Caveats](#secret-handling-caveats) for where those data-path features *would* matter.

Two scopes matter:

| Scope | What | How it is captured |
| --- | --- | --- |
| Cluster-wide (once per cluster) | The GAG Custom Resource Definitions (CRDs) in API group `actions-gateway.github.com` (`actionsgateways`, `egressproxies`, `runnergroups`, `runnersets`, `runnertemplates`, `clusterrunnertemplates`). | A CRD backup — or, preferably, the Helm CRD chart tracked in Git. |
| Per-tenant namespace | The `ActionsGateway` CR, any referenced `EgressProxy` CR, the GitHub App credential `Secret`, the `Namespace` (with its security-profile label), and the `ResourceQuota`. | A namespace backup, scheduled per tenant. |

Everything else in the tenant namespace — the Actions Gateway Controller (AGC) `Deployment`, ServiceAccounts, RoleBinding, metrics `Service`, NetworkPolicies, metrics TLS `Secret`s, and the `RunnerGroup` CR — is **owned and reconciled by the GMC** and does not need to be in the restore path. Those objects all carry the label `app.kubernetes.io/managed-by=actions-gateway-gmc`, which is exactly the selector the restore step uses to skip them.

---

## Backing Up

The examples assume Velero is installed with a `BackupStorageLocation` whose object store has **encryption-at-rest enabled** (see [Secret Handling Caveats](#secret-handling-caveats) — this is not optional for GAG, because the backup contains the GitHub App private-key Secret).

### One-time: the CRDs

CRDs are cluster-scoped and shared across all tenants. The cleanest source of truth for them is the Helm CRD chart in version control (GitOps), restored with `helm`/`kubectl apply` before anything else. If you want Velero to carry them too as a belt-and-suspenders, back them up on their own:

```sh
velero backup create gag-crds \
  --include-resources customresourcedefinitions.apiextensions.k8s.io \
  --include-cluster-resources=true \
  --selector 'app.kubernetes.io/part-of=actions-gateway'
```

> The `--selector` narrows the backup to GAG's CRDs only. It works if your CRDs carry the `app.kubernetes.io/part-of=actions-gateway` label (the Helm chart stamps the recommended label set). If yours do not, drop the `--selector` and capture all CRDs — they are small and idempotent to restore.

### Per-tenant: the namespace

Back up the whole tenant namespace. Capturing the owned children too is fine — they make the backup a useful point-in-time audit snapshot; the [restore step](#restoring) simply skips them.

```sh
velero backup create gag-tenant-<namespace> \
  --include-namespaces <namespace>
```

Schedule it so the credential Secret and CR spec are always recent:

```sh
velero schedule create gag-tenant-<namespace> \
  --schedule '@every 24h' \
  --include-namespaces <namespace> \
  --ttl 720h0m0s
```

For multi-tenant clusters, drive one schedule per tenant namespace, or select across them with a shared label (for example `--selector 'actions-gateway.github.com/tenant=managed'` if you label tenant namespaces).

---

## Restoring

### Ordering: CRDs → inputs and CRs → reconcile

1. **CRDs first.** Restore the GAG CRDs (or `helm install` the CRD chart) before any CR — a namespaced CR cannot be created until its CRD is established. Velero already enforces this within a single restore: `customresourcedefinitions` sit near the top of Velero's default restore-resource-priority list, so CRDs are applied before the CRs that depend on them. If you keep CRDs in Git, apply them first and skip Velero for this step.

2. **Inputs and top-level CRs, skipping owned children.** Restore the tenant namespace, but **exclude everything the GMC owns** and let the controllers rebuild it. The owned children (and the GMC-provisioned `RunnerGroup`) all carry `app.kubernetes.io/managed-by=actions-gateway-gmc`; the operator inputs and the top-level CRs do not. A single label selector expresses exactly that:

   ```sh
   velero restore create gag-restore-<namespace> \
     --from-backup gag-tenant-<namespace> \
     --selector 'app.kubernetes.io/managed-by notin (actions-gateway-gmc)'
   ```

   This restores the `Namespace`, the `ResourceQuota`, the GitHub App credential `Secret`, the `ActionsGateway` CR, and any `EgressProxy` CR — and **nothing else**.

3. **Let the controllers reconcile.** Once the CRs land, the GMC re-provisions every owned child within ~30 seconds and **regenerates the metrics TLS Secrets fresh** (new key material — exactly as in a normal re-apply). The cluster-scoped `ClusterRoleBinding` the AGC needs is recreated by the GMC's reconcile, not by Velero. `RunnerSet`s that target the gateway re-bind automatically.

> **Ordering within step 2 is handled for you.** Velero restores the `Namespace` and `Secret` before the CRs (namespaces and secrets also precede custom resources in the default priority list), so the gateway does not transiently report `CredentialUnavailable`. If you restore the `EgressProxy` from a *separate* backup, restore it before (or alongside) the `ActionsGateway`, or the gateway fails closed with `ProxyNotFound` until it appears — then reconciles on its own.

### Why not restore the owned children directly

Two reasons, both rooted in GAG's [ownership model](backup-restore.md#what-is-and-is-not-owned-by-the-cr):

- **Owner-reference UIDs go stale.** Every owned child carries an `ownerReference` to the `ActionsGateway` CR's UID. On restore the apiserver assigns the CR a **new** UID, so a restored child's `ownerReference` points at a UID that no longer exists. Kubernetes garbage collection then deletes the "orphaned" child — so a directly-restored Deployment or Secret may simply vanish moments later. Letting the GMC recreate the children means they are stamped with the live CR's UID from the start.
- **Restored metrics TLS Secrets would be stale.** The GMC regenerates the server/client metrics TLS Secrets on every reconcile. Restoring the old material just to have the GMC overwrite (or distrust) it buys nothing and risks a window where a `ServiceMonitor` trusts retired key material.

Skipping the owned children with the `managed-by` selector sidesteps both problems entirely and matches how a normal CR re-apply already recovers the gateway.

---

## Secret Handling Caveats

The per-tenant backup contains the **GitHub App credential `Secret`** — including the App private key. In the backup it is stored exactly as in etcd: base64-encoded, **not encrypted**. Treat the Velero backup with the same sensitivity as an etcd snapshot.

- **Encrypt the backup storage at rest.** Enable server-side encryption on the `BackupStorageLocation` bucket — SSE-S3 or SSE-KMS on Amazon S3, customer-managed encryption keys on Google Cloud Storage / Azure Blob, or your provider's equivalent. This is a hard requirement for GAG backups, not a nicety, because the private key is otherwise recoverable by anyone who can read the bucket. See [backup-restore.md](backup-restore.md#primary-gitops) for the broader "never store the key in plaintext" guidance and [security-operations.md](security-operations.md) for credential handling.
- **Restrict who can read backups and run restores.** A restore re-materializes the Secret into the cluster; scope the Velero RBAC and the bucket IAM accordingly.
- **Restic/Kopia and CSI snapshots do not apply to the GAG control plane.** Those Velero features back up *PersistentVolume data*; GAG keeps no control-plane state on volumes, so there is nothing for them to capture here. They become relevant only if a *tenant's own runner workloads* mount PVs you also want backed up — that is a separate concern from GAG DR and out of scope for this page.
- **Prefer minting over restoring, if the key is in a vault.** If you keep the source `.pem` in a secrets vault, you can recreate the credential Secret from it (see [backup-restore.md Scenario B](backup-restore.md#scenario-b-whole-tenant-namespace-lost)) instead of relying on the Velero copy — that keeps the private key out of the backup blast radius. The trade-off is an extra manual step at restore time.

---

## Verification

After the restore, verify the gateway exactly as for any other recovery — the checks in [backup-restore.md § Verification](backup-restore.md#verification) apply unchanged: confirm the GMC re-provisioned the children, the AGC Deployment is available, the `ActionsGateway` reports `Ready=True`, and a test workflow provisions a worker pod.

---

## Reference Links

- [Backup, Restore, and Disaster Recovery](backup-restore.md) — the conceptual DR guide and ownership model this how-to builds on
- [Runbook](runbook.md) — day-2 operations and incident response
- [Security Operations](security-operations.md) — credential handling and compromise response
- [Velero documentation](https://velero.io/docs/) — backup/restore reference, resource filtering, and storage encryption
