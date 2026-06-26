# Platform-owned PriorityClass allowlist for runner tiers (Q132)

**Decision (2026-06-14):** the platform owns *which* cluster-scoped
`PriorityClass` names a tenant may reference in `priorityTiers`. The platform
admin pre-creates the `PriorityClass` objects (as it already does today) **and**
configures a GMC allowlist of their names; the GMC validating webhook rejects any
`ActionsGateway` whose `spec.runnerGroups[].priorityTiers[].priorityClassName` is
not on the allowlist. Secure-by-default: an empty/unset allowlist forbids *all*
`priorityTiers` PriorityClass references.

Tracked as STATUS Queue Q132 (`security`, `1.0-gate`). Same "platform owns it,
not the tenant" family as [Q130](archive/platform-owned-quota.md) (platform-owned
`ResourceQuota`) and the [Q121/Q122/Q125](q121-q122-q125-gmc-confinement.md) GMC
write confinement.

## Why

`priorityTiers[].priorityClassName` is **tenant-authored and unvalidated**. The
GMC copies the `RunnerGroupSpec` verbatim into the namespaced `RunnerGroup`
([builder.go:813](../../cmd/gmc/internal/controller/builder.go:813),
`Spec: spec`), and the AGC provisioner stamps it onto worker pods
([provisioner.go:809](../../cmd/agc/internal/provisioner/provisioner.go:809),
`pod.Spec.PriorityClassName = priorityClass`).

`PriorityClass` is **cluster-scoped** and carries a `value` (priority) and a
`preemptionPolicy` (k8s default `PreemptLowerPriority`). A tenant who names a
high-`value` class with `PreemptLowerPriority` gets the scheduler to **preempt
(evict) other tenants' running worker pods** to make room for its own. That
breaks the cross-tenant isolation the whole per-tenant model promises — the same
class of hole Q130 closed for quota.

## Decision: allowlist, not tier→class map

The prompt leaned toward a tier→class map (tenant picks an abstract tier; the
platform maps it to a real class it created). We chose the **allowlist** instead.
Both deliver the *same* security property — a tenant cannot reference an
arbitrary cluster-scoped class — but the allowlist fits this codebase far better:

1. **A tier→class map forces the GMC to own cluster-scoped objects.** The mapped
   `PriorityClass` objects have to exist; "the GMC creates/owns them" re-expands
   the cluster-scoped write surface that
   [Q121/Q122/Q125](q121-q122-q125-gmc-confinement.md) just *confined* to tenant
   namespaces, and contradicts [Q130](archive/platform-owned-quota.md)'s model (platform
   admin owns infra/cluster resources; GAG operates *within* them). The platform
   already pre-creates these classes today — keep it that way.
2. **The `RunnerGroupSpec` type is shared verbatim.** `ActionsGateway.spec.runnerGroups`
   is `[]agcv1alpha1.RunnerGroupSpec`, copied unchanged into the `RunnerGroup`
   CR, which the AGC reads to stamp the *real* class name on pods. An abstract
   `tier` enum would require either splitting that shared type (tenant-facing
   `tier` vs AGC-facing `className`) or teaching the AGC the platform mapping —
   spreading platform config across two controllers. The allowlist keeps the
   field shape and validates references against platform config in one place.
3. **The allowlist adds no new control plane.** A platform flag + a webhook
   membership check, alongside the existing `validateRunnerGroups`
   privileged-container check. No new owned resources, no new RBAC, no teardown.

The tier→class map's only extra over the allowlist is an abstraction layer
(tenant names `"high"` instead of `"runner-high"`); it buys no security property
the allowlist lacks. Rejected for the cost above.

## Scope

### API / CRD (breaking, pre-1.0)
- **Remove the dead `PreemptionPolicy` field** from `PriorityTier`
  ([runnergroup_types.go:19-24](../../cmd/agc/api/v1alpha1/runnergroup_types.go:19)).
  It has existed since Milestone 2 but is **never consumed** — the provisioner
  only sets `PriorityClassName`, never `pod.Spec.PreemptionPolicy`. The design
  doc already calls it "informational only." Keeping a tenant-settable
  preemption field is misleading *and* a latent preemption lever; preemption is
  governed by the platform-owned `PriorityClass` object's own `preemptionPolicy`.
  Field removal is a breaking CRD change — free pre-1.0 (no conversion webhook;
  same window as Q130). Zero behavioural change (the field never reached a pod).
- `PriorityClassName` stays required and unchanged in shape (tenant still names a
  class; the webhook gates *which* names are legal).
- Regenerate per [code-generation.md](../development/code-generation.md):
  `make -C cmd/agc generate manifests` → `zz_generated.deepcopy.go`, both CRD YAML
  copies ([cmd/agc/.../runnergroups.yaml](../../cmd/agc/config/crd/actions-gateway.github.com_runnergroups.yaml),
  the GMC-bundled [cmd/gmc/.../runnergroups.yaml](../../cmd/gmc/config/crd/bases/actions-gateway.github.com_runnergroups.yaml)),
  and the chart CRD template. Keep all copies in sync (Q73).

### GMC config (allowlist source)
- New flag `--allowed-priority-classes` on the GMC
  ([main.go](../../cmd/gmc/cmd/main.go)), comma-separated class names, matching
  the existing `--allow-floating-image-tags` / `--allow-agc-extra-env` style.
  Empty (default) = no classes permitted = `priorityTiers` rejected.
- Thread it into `SetupActionsGatewayWebhookWithManager` →
  `NewActionsGatewayCustomValidator(podNamespace, allowedPriorityClasses)`; store
  as a `map[string]bool` on the validator (mirrors `reservedNamespaces`).

### Webhook validation
- Add `validatePriorityClasses` (a method on the validator, since it needs the
  allowlist field) called from `ValidateCreate`/`ValidateUpdate` alongside
  `validateRunnerGroups`. For each `runnerGroups[i].priorityTiers[j]`, reject
  when `priorityClassName` is not in the allowlist. Error names the offending
  class **and** the permitted set, e.g.
  `runnerGroups[0].priorityTiers[1]: priorityClassName "evil-preempt" is not in the platform allowlist [runner-standard runner-opportunistic]`.
- Residual (documented, not coded here): a tenant with *direct* `runnergroups`
  RBAC bypasses the ActionsGateway webhook. RunnerGroup authoring is a
  GMC-ServiceAccount path guarded by `tenant-resource-guard`; tenants are not
  expected to hold direct `runnergroups` create. A `ValidatingAdmissionPolicy` on
  `runnergroups` (defense-in-depth, like the GMC-SA guards) is a possible future
  enhancement — note in appendix-g, do not build now (keeps scope contained and
  matches scope item 2: "GMC validating webhook and/or CEL").

### preemptionPolicy guidance (secure-by-default, operational)
- Even an *allowlisted* class with `preemptionPolicy: PreemptLowerPriority`
  preempts cross-tenant (PriorityClasses are global). The platform must create
  allowlisted classes with `preemptionPolicy: Never` **unless** cross-tenant
  preemption is genuinely intended for that tier. Document this strongly in
  security-operations + tenant-onboarding (it is the real preemption knob now
  that the tenant field is gone).

### Tests
- envtest webhook test (cmd/gmc `controller/integration` or `webhook` suite,
  using `WebhookInstallOptions`): an ActionsGateway whose priorityTiers names a
  non-allowlisted class is **rejected**; one naming an allowlisted class
  **passes**; empty allowlist rejects any class. Drive the validator with a known
  allowlist via the constructor (as reserved-namespace tests do).
- Unit tests for the membership logic.
- Provisioner/CRD tests that previously set `PreemptionPolicy` (none found in
  code; only the type defined it) — confirm `make check` stays green after field
  removal.
- Run gmc under `-race` per testing.md. **No kind e2e** — admission rejection is
  envtest-provable; scheduler preemption is a k8s property we don't re-prove.

### Docs
- `03-api-contracts.md`: drop `preemptionPolicy` from the `PriorityTier`
  contract; add the allowlist gate to the `priorityClassName` description.
- `05-security.md`: add the cross-tenant preemption threat + the platform-owned
  allowlist control; mark the threat closed.
- `operations/tenant-onboarding.md`: the PriorityClass pre-create checklist item
  now also requires the class name be added to the GMC allowlist; how a tenant
  requests a priority tier (ask the platform to allowlist + create the class).
- `operations/security-operations.md`: configuring `--allowed-priority-classes`;
  the `preemptionPolicy: Never` guidance for allowlisted classes.
- `operations/upgrade.md`: BREAKING — `preemptionPolicy` removed from
  `priorityTiers` (pruned silently); set `--allowed-priority-classes` before/at
  upgrade or existing `priorityTiers` CRs will be rejected on next apply.
- appendix-g: future-enhancement note for the RunnerGroup VAP defense-in-depth.

### STATUS
- Delete the Q132 row (isolated commit).

## Status

**Implemented (2026-06-14, Q132).** Delivered per scope above:

- **API/CRD:** removed `PreemptionPolicy` from `PriorityTier`; regenerated
  deepcopy and all five CRD copies (AGC authoritative, GMC bundled runnergroups +
  actionsgateways, both chart CRD templates), verified in sync.
- **GMC:** `--allowed-priority-classes` flag → `NewActionsGatewayCustomValidator`
  → `validatePriorityClasses` (rejects off-allowlist classes, names class + allowed
  set; empty allowlist rejects all).
- **Chart:** `allowedPriorityClasses` value wired to the deployment arg
  (`helm template`/`helm lint` verified — flag present when set, absent on the
  secure default).
- **Tests:** webhook unit tests + envtest `TestCRD_ActionsGateway_PriorityClassAllowlist`
  (reject/accept/empty/update); `make check` green; gmc run under `-race`.
- **Docs:** design 01/03/05/appendix-e/f/g, tenant-onboarding, security-operations
  (new Priority classes section), upgrade breaking-change note, getting-started,
  why-gag. Threat marked Closed in 05-security.md.

Direct-RunnerGroup-write bypass (a tenant with direct `runnergroups` RBAC) is
out of scope here and captured as future-enhancement G.7 (a `runnergroups` VAP).
Shipped in PR #234.
