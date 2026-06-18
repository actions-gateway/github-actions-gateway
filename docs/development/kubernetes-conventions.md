# Kubernetes API conventions

Project-specific conventions for the Kubernetes surface we author: label and
annotation keys/values, and the gotchas that have bitten us. Read this before
adding a new label, annotation, or CRD field that an operator sets by hand.

## Label & annotation value conventions

### Don't use boolean-looking values for string-matched labels/annotations

When a label or annotation value is **matched as a string** by our code (an
admission webhook, a controller, a `ValidatingAdmissionPolicy`), use an explicit
**enum keyword** — e.g. `allowed`, `enabled`, `managed` — never a
boolean-looking value (`true`, `false`, `yes`, `no`, `on`, `off`).

Why:

- **YAML coercion footgun.** In a manifest, `my-label: true` parses as a YAML
  boolean, not the string `"true"`. A Kubernetes label/annotation value must be
  a string, so the unquoted form either errors or has to be remembered as
  `"true"` (quoted) every time. YAML 1.1 coerces `yes`/`no`/`on`/`off` (and
  their capitalised variants) the same way, so the trap is wider than just
  `true`/`false`.
- **Self-documenting.** `actions-gateway.github.com/privileged-profile: allowed`
  reads as a deliberate grant. `…: "true"` carries no meaning and invites the
  reader to drop the quotes.

The value is always matched **exactly** and the check is **fail-closed**: any
value other than the sentinel keyword (and an absent label) is treated as "not
granted". So even if someone fat-fingers `true`, eligibility is denied rather
than silently granted.

**Worked example.** The privileged-profile eligibility gate (Q133) uses

```yaml
metadata:
  labels:
    actions-gateway.github.com/privileged-profile: allowed   # not "true"
```

See `PrivilegedProfileLabel` / `PrivilegedProfileAllowed` in
[`cmd/gmc/api/v1alpha1/actionsgateway_types.go`](../../cmd/gmc/api/v1alpha1/actionsgateway_types.go)
and [§5.3 of the security design](../design/05-security.md#privileged-eligibility-is-a-platform-decision).

### Pre-existing `"true"` values are grandfathered

Two shipped keys predate this convention and still use `"true"`:

- `actions-gateway.github.com/tenant: "true"` — the managed-tenant marker label.
- `actions-gateway.github.com/allow-profile-downgrade: "true"` — the
  downgrade opt-in annotation.

These are **not** to be changed casually. The `tenant` marker in particular is
load-bearing: the `namespace-psa-guard` and `gmc-tenant-resource-guard`
`ValidatingAdmissionPolicy` objects, the onboarding scripts, and operator
runbooks all match it as `"true"`, so changing the value is a breaking change to
deployed clusters. The convention above applies to **new** keys; the existing
two stay as-is unless there is a separate, deliberate migration.

## Label & annotation key conventions

Use the `actions-gateway.github.com/<name>` prefix for every label and
annotation key the project defines, matching the API group. Define the key (and
its sentinel value, if any) as an exported `const` in the relevant
`api/v1alpha1` package with godoc, and reference that const from controllers,
webhooks, and tests — never re-type the literal string, so a rename can't drift
between the producer and the consumers.
