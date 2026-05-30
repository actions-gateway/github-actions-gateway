# gmc — Gateway Manager Controller

Cluster-level controller that reconciles a single `ActionsGateway` CR into per-tenant Actions Gateway Controller (AGC) instances and their supporting resources:

- `Namespace` (per tenant, isolated)
- `ServiceAccount`, `Role`, `RoleBinding` (AGC RBAC)
- `Secret` (projected GitHub App credentials)
- `Service` (AGC readyz/metrics)
- `NetworkPolicy` (default-deny + tenant egress allowlist)
- Egress proxy pool (`Deployment` + `Service`, one isolated egress IP per tenant)

See the [cmd/ index](../README.md) for how GMC fits into the four-tier system, and [DESIGN.md](../../DESIGN.md) + [docs/design/02-architecture.md](../../docs/design/02-architecture.md) for the full design.

## References

| Topic | Doc |
|---|---|
| `ActionsGateway` CRD schema and field semantics | [docs/design/03-api-contracts.md](../../docs/design/03-api-contracts.md) |
| Editing CRD types and regenerating manifests | [docs/development/code-generation.md](../../docs/development/code-generation.md) |
| Reconcile flow diagrams | [docs/design/04-operational-flows.md](../../docs/design/04-operational-flows.md) |
| Security boundaries (per-tenant isolation, NetworkPolicy) | [docs/design/05-security.md](../../docs/design/05-security.md) |
| Building the image | [docs/development/building.md](../../docs/development/building.md) |
| Integration / e2e testing | [docs/development/testing.md](../../docs/development/testing.md) |

## Layout

- `api/v1alpha1/` — `ActionsGateway` CRD types (kubebuilder markers; regenerate via [docs/development/code-generation.md](../../docs/development/code-generation.md)).
- `internal/controller/` — reconciler implementation.
- `internal/webhook/v1alpha1/` — admission webhooks for `ActionsGateway`.
- `config/` — kustomize bases for CRDs, RBAC, manager, and samples.
- `cmd/main.go` — manager entrypoint.
