# Rename `actions-gateway-agc` → `actions-gateway-controller`

## Goal

Standardize the on-cluster name of the per-tenant Actions Gateway Controller
Deployment (and the resources that hang off it) to `actions-gateway-controller`,
matching the name used in the user-facing docs (`getting-started.md`,
`operations/*.md`, `design/network-architecture.md`).

Today the code produces `actions-gateway-agc`. The discrepancy has been latent
since Milestone 4 (May 2026 — commit `94f90fb`); the docs were drafted against
the planned name and the implementation diverged silently.

## Why a separate task

The rename touches five Go constants across two binaries (GMC and AGC), every
e2e and integration test that references the strings literally, and downstream
docs that currently use the `-agc` form. It also changes on-cluster Pod labels,
which is observable to Prometheus queries and any alerting that pivots on
`app=actions-gateway-agc`. Shipping it inside the current PR would obscure the
review of the security work.

## Scope

### Code constants to rename

| File | Constant | Current value | Target value |
|---|---|---|---|
| `cmd/gmc/internal/controller/builder.go:25` | `agcSAName` | `actions-gateway-agc` | `actions-gateway-controller` |
| `cmd/gmc/internal/controller/builder.go:28` | `agcAppName` | `actions-gateway-agc` | `actions-gateway-controller` |
| `cmd/gmc/internal/controller/builder.go:44` | `npAGCName` | `actions-gateway-agc` | `actions-gateway-controller` |
| `cmd/agc/internal/provisioner/provisioner.go:41` | `managerName` | `actions-gateway-agc` | `actions-gateway-controller` |
| `cmd/agc/internal/agentpool/pool.go:26` | `managedByValue` | `actions-gateway-agc` | `actions-gateway-controller` |

### Shared public constant

Instead of five private constants spread across two binaries, declare one
exported symbol that both modules import:

```go
// internal/names/names.go or similar:
package names

// ControllerName is the canonical name used for the AGC Deployment, its
// ServiceAccount, the namespace-scoped NetworkPolicy that gives it K8s
// API egress, and the value of the app.kubernetes.io/managed-by label
// on resources the AGC creates (worker pods, agent secrets).
//
// Single source of truth — keeping this value coordinated across the GMC
// (which creates the Deployment/SA/NP) and the AGC (which labels worker
// pods so they match the workload NetworkPolicy) is a correctness
// requirement, not a style preference. Changing it in only one place
// silently breaks the NetworkPolicy selector match.
const ControllerName = "actions-gateway-controller"
```

The package needs to be importable by both `cmd/gmc/...` and `cmd/agc/...`. The
root module (`github.com/karlkfi/github-actions-gateway`) is already shared by
both — putting it there (e.g. `names/names.go` at the repo root or
`internal/names/` if the package should not be public API) keeps the import
graph clean.

Note: the AGC `provisioner` and `agentpool` packages use this value for the
`app.kubernetes.io/managed-by` label on resources the **AGC** creates
(worker pods, agent secrets). Today this happens to match the GMC's Deployment
name, but semantically they could diverge. Confirm the unification is correct
before collapsing to one constant — if these are conceptually two things, keep
two constants but assign them the same shared value.

### Tests to update

Every e2e and integration test that references the string literally will need
updating. As of this writing:

```
cmd/gmc/test/e2e/github_e2e_test.go
cmd/gmc/test/e2e/job_lifecycle_test.go
cmd/gmc/test/e2e/teardown_test.go
cmd/gmc/test/e2e/provisioning_test.go
cmd/gmc/test/e2e/isolation_test.go
cmd/gmc/test/e2e/resilience_test.go
cmd/gmc/test/e2e/rbac_e2e_test.go
cmd/gmc/internal/controller/integration/rbac_scope_test.go
cmd/gmc/internal/controller/integration/teardown_test.go
cmd/gmc/internal/controller/builder_test.go
```

Where possible, import the shared constant in tests rather than re-typing
the string — that way the next rename is a single change.

### Docs to revisit

The operations docs were updated in this PR to use the current `-agc` name
(reflecting code reality). Once the rename ships, flip them back. The list
that needs flipping:

```
docs/getting-started.md
docs/operations/runbook.md
docs/operations/troubleshooting.md
docs/operations/tenant-onboarding.md
docs/operations/upgrade.md
docs/operations/observability.md
docs/design/network-architecture.md
```

The design and plan docs (`docs/design/`, `docs/plan/milestone-*.md`) refer
to the controller concept by acronym (AGC) far more than by Deployment name
— most of those need no update. Grep for `actions-gateway-agc` after the
code rename to confirm.

## Migration considerations

### Existing cluster state

When the rename ships, the GMC's next reconcile will:

1. Try to create a new Deployment `actions-gateway-controller` alongside the
   existing `actions-gateway-agc`.
2. Leave the old Deployment orphaned (no longer owned by the controller's
   field manager but still running).

Two safe paths:

- **Recommended:** Document a manual cleanup step in the release notes:
  `kubectl delete deploy actions-gateway-agc -n <ns>` per tenant after
  upgrading the GMC. The GMC creates the new Deployment, the old one is
  removed, and the AGC's `app.kubernetes.io/managed-by` label change on
  newly created worker pods is harmless (worker pods are ephemeral).

- **Stretch:** Have the GMC explicitly delete the legacy-named Deployment
  on first reconcile after the upgrade, with a guard label to make the
  cleanup idempotent. More code but no operator action required.

### Prometheus / alerting impact

Pods labelled `app=actions-gateway-agc` become `app=actions-gateway-controller`.
Any alert or dashboard pivoting on the old label needs updating. Flag this
in the release notes.

### NetworkPolicy selector match

The `actions-gateway-agc` NetworkPolicy currently selects pods by
`app: actions-gateway-agc`. The rename keeps the selector and the Pod label
in sync (both come from the same `agcAppName`/`ControllerName` constant), so
the policy continues to match. The only window of risk is during the rolling
update — for a few seconds, the old Deployment (with the new selector) has
no matching pods. Worker pods are unaffected; they match the workload
NetworkPolicy by `actions-gateway/component: workload`, not by the AGC's
`app:` label.

## Definition of done

- [ ] One exported constant declared in a shared package; both binaries
      import it.
- [ ] All five Go-side string literals replaced.
- [ ] All test files updated (literal-string greps return zero results).
- [ ] Operations docs flipped back to `actions-gateway-controller`.
- [ ] Release notes mention the on-cluster rename, the manual cleanup
      command, and the Prometheus label change.
- [ ] `make e2e-up` passes on a fresh kind cluster.
