# Q182 — Auto-install / document the API-server audit policy

**Goal:** Make the compromised-Secret and Pod-Security-Admission (PSA) escalation
signals reachable without operators hand-copying a static file into
`kube-apiserver` — by shipping the policy as an applyable/installable asset with
an auto-install path where the project genuinely controls the API-server config,
and documenting the realistic per-provider managed-cluster path everywhere else.

**Approach:**
1. Keep the existing policy asset
   [`docs/operations/examples/apiserver-audit-policy.yaml`](../operations/examples/apiserver-audit-policy.yaml).
   Its rules were written against the security model (§5.1 GMC Secret-read
   residual; the `gmc-tenant-resource-guard` and `namespace-psa-guard` VAPs) and
   match the detection table in `security-operations.md` — **do not invent new
   rules.** Verified unchanged.
2. Ship an **installer** for existing self-managed kubeadm clusters
   (`docs/operations/examples/install-apiserver-audit-policy.sh`): run once per
   control-plane node, it validates the policy, installs it, and idempotently
   patches the `kube-apiserver` static-pod manifest (timestamped backup; `yq`
   for the structured edit so the manifest can't be corrupted).
3. Ship a **kind cluster config**
   (`docs/operations/examples/kind-cluster-audit.yaml`) that wires the policy via
   `extraMounts` + `kubeadmConfigPatches` — the one place auto-install is
   genuinely possible because you provision the control plane.
4. Restructure `security-operations.md` § *Install the audit policy* into honest
   paths: self-provisioned (kind / kubeadm ClusterConfiguration), existing
   kubeadm (the installer), and **per-provider** EKS / GKE / AKS subsections —
   each with where the same signals surface and the exact filter query.

## Honest boundary: where auto-install is *not* possible

The audit config is a **node-level `kube-apiserver` flag**, not a Kubernetes
object — there is no `kubectl apply` for it, and Helm (which deploys workloads,
not control-plane node files) cannot install it either. Full automation is only
possible for clusters whose control plane the operator provisions (kind,
kubeadm-from-scratch). On EKS / GKE / AKS the provider owns the API-server flags
and ships a **fixed** audit configuration to its own managed log sink — a custom
`--audit-policy-file` cannot be supplied. There the path is: enable the
provider's control-plane audit logging and translate the same predicates
(requester `user.username`, `verb`, `objectRef.resource=secrets`; VAP `403`
denials) into filters against the managed stream.

## Per-provider facts (verified June 2026)

- **EKS** — enable the `audit` control-plane log type per cluster; events land in
  CloudWatch log group `/aws/eks/<cluster>/cluster`, streams
  `kube-apiserver-audit-*`. EKS's fixed policy logs Secret access at `Metadata`.
  Query with CloudWatch Logs Insights.
- **GKE** — Secret **reads** are Cloud Audit Logs **Data Access** logs
  (`DATA_READ`/`ADMIN_READ`), **off by default** — enable them for the Kubernetes
  Engine API. Writes are Admin Activity logs (always on). `methodName` is
  `io.k8s.core.v1.secrets.get` etc.; query in Logs Explorer.
- **AKS** — forward the **`kube-audit`** diagnostic category to Log Analytics /
  storage / Event Hub. **`kube-audit-admin` excludes `get`/`list`** — so it
  cannot see Secret reads; the compromised-Secret detection requires the full
  `kube-audit` category. Query with KQL.

## Acceptance
- Installable asset + auto-install path where feasible (installer + kind config).
- Per-provider EKS/GKE/AKS/self-managed operator guidance.
- `make check` green (shellcheck the new script; doc-links resolve; STATUS lint).
- Q182 row removed from `docs/STATUS.md` in its own isolated commit.
