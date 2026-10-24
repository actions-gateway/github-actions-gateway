# Q1188: scope the GMC's Secret reads to tenant namespaces

The Gateway Manager Controller (GMC) ClusterRole grants `get`, `list` and `watch` on every Secret in the cluster, `kube-system` included.
This plan moves all three onto a per-namespace grant, so a compromised GMC can read Secrets only in namespaces a platform admin has marked as tenants.

## Status

| Step | State |
|---|---|
| 1. Design pass in [05-security.md §5.1](../../design/05-security.md#51-gmc-level-threats-cluster-scoped) | ✅ |
| 2. Reader ClusterRole and the scoped `bind` | ✅ |
| 3. Tenant Secret access reconciler (RoleBinding per tenant namespace) | ✅ |
| 4. Per-namespace metadata-only Secret caches replacing `WatchesMetadata` | ✅ |
| 5. envtest: the GMC runs under its shipped RBAC and cannot read outside tenants | ✅ |
| 6. Operator docs, upgrade note, backlog | ✅ |

## Why the metadata watch cannot be kept on the cluster-wide grant

A metadata-only `list` or `watch` is an ordinary `list secrets` request whose `Accept` header asks for `PartialObjectMetadataList`.
Authorization runs before the response is shaped and decides on user, verb, namespace, resource, subresource, name, API group and version, path, and field and label selectors (`authorizer.Attributes` in the vendored `k8s.io/apiserver`).
The response format is not an input, so neither RBAC nor an authorization webhook can grant the metadata view without the full one.
The metadata-only informer keeps `.data` out of the GMC's memory; it does not limit what the GMC's token can read.

## Approach

**Grant.** A new singleton ClusterRole `gmc-tenant-secret-reader` holds `get`, `list` and `watch` on `secrets`.
The GMC's manager ClusterRole loses those three verbs on Secrets, keeps `create` and `update`, and gains `bind` restricted by `resourceNames` to `gmc-tenant-secret-reader`.
This is the pattern the GMC already uses to hand the Actions Gateway Controller (AGC) its permissions (`bind` on `agc-tenant-role` and `agc-clusterrunnertemplate-reader`), so it adds no new class of grant.

**Fence.** RBAC escalation prevention is satisfied by `bind`, so the fence that stops the GMC binding the reader into `kube-system` is admission.
`tenant-resource-guard` already confines the GMC's RoleBinding writes to namespaces carrying a tenant marker, and `namespace-psa-guard` makes the marker immutable to the GMC.
`create`/`update` on Secrets stay cluster-wide because admission rejects an out-of-tenant write before any response, so neither verb returns a body.
With `admissionPolicy.enabled=false` the fence is gone, which is the same residual the `agc-tenant-role` bind already carries.

**Binding.** A namespace reconciler, registered for v1 and v2 alike, creates RoleBinding `gmc-tenant-secret-reader` in every namespace carrying either tenant marker (`actions-gateway.github.com/tenant=true` or `actions-gateway.com/tenant=managed`), binding the reader to the GMC's own ServiceAccount, read from a new `POD_SERVICE_ACCOUNT` downward-API env var.
A namespace that loses its marker keeps the binding: the guard denies the GMC's delete there, because the namespace is no longer a tenant.
That residual is documented for the platform admin, who removes the binding when unmarking.

**Events.** The two gateway reconcilers drop `WatchesMetadata(&corev1.Secret{})`.
The namespace reconciler also starts one metadata-only cache per tenant namespace and adds a `source.Kind` over it to each gateway controller through `Controller.Watch`, which controller-runtime v0.25.2 starts immediately after the controller is running.
The cache is cancelled when the namespace loses its marker or is deleted.
No library supports a changing namespace set; this is built from controller-runtime pieces, the shape Crossplane uses for runtime-started caches.

## What each Secret read now needs

Every Secret read in `cmd/gmc` is in a gateway's namespace or an EgressProxy's, and the guard already requires both to be tenant namespaces (read from source; no exhaustive audit).
A reconcile that runs before its namespace's binding has propagated gets `Forbidden` from the credential `Get` and retries with backoff.

## Readers of what this changes

- `charts/actions-gateway/files/manager-role-rules.yaml` (generated from the markers) and the chart templates.
- `docs/design/05-security.md` §5.1, which states the cluster-wide read as the honest residual.
- `docs/operations/security-operations.md`, whose audit queries and structural-mitigation list describe the old grant.
- `docs/operations/upgrade.md`, for the RBAC change an upgrading install takes.
- The `credential_rotation` envtest, which pins `CredentialUnavailable` within 15 s of a Secret deletion and must still pass.

## Findings

- `TestGMC_TenantSecretAccess_ScopesReadsToTenantNamespaces` runs the v1 gateway reconciler as the GMC ServiceAccount under the chart's manager rules and reader rules, with the default 10h resync.
  It passed, and went red at the expected assertion under each of two inversions (measured 2026-10-23): with the Secret sink removed, a newly created credential Secret never reached the gateway; with `get`/`list` restored to the manager rules, a Secret in an unmarked namespace became readable.
- The GMC applies its owned Secrets with `CreateOrPatch`, which patches, and the manager role grants no `patch` on Secrets.
  Filed as its own row; it predates this change.
