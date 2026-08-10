# Q409 — Align the tenant-facing v2 version, and route the ARC migration at v2

> **Status:** ✅ Done.
> Both slices shipped together; see [Scope](#scope) for why they could not be split.

## Why this exists

Two defects with one root cause: the v2 front-door work (Q273) routed the *onboarding* docs to v2 but left two tenant-facing surfaces behind.

1. **[`migration-from-arc.md`](../../operations/migration-from-arc.md) still teaches the whole ARC migration on `v1alpha1`**, and asserts v1 is "the GA default".
   Both halves are wrong: `v1alpha1` is alpha, is [deprecated](../../operations/v1alpha1-deprecation.md), and is queued for removal ([Q273](../../STATUS.md#Q273) → [Q264](../../STATUS.md#Q264)).
2. **Every onboarding doc names `v2alpha1` while also claiming v2 "has reached its first stability contract at `v2beta1`"** — self-contradictory, and it points new tenants at the coexistence version rather than the graduated one.

The ARC guide is the worse of the two, because the version it names is not merely stale positioning — it changes the job-acquisition protocol the reader ends up on.

### The ARC guide routed scale-set users into the one known regression

`v1alpha1` has no `acquisitionProtocol` field: it is **Classic-only**.
The guide is explicitly scoped to ARC's **scale-set mode**, i.e. readers who are already on a single-acquirer protocol.
Sending them to `v1alpha1` moved them onto the classic many-acquirers broker — the path measured at **2/7 vs scale-set's 7/7** under high-burst fan-out ([Q264 P4](../q264-scale-set-protocol.md)), and the one the [sunset review](../v1-classic-sunset-review.md) declared terminal.

So the pre-existing guide told GAG's best-fit prospects to migrate into a known structural ceiling, then migrate twice more (v1 → v2, Classic → ScaleSet) to get back to where ARC already had them.

Routing at v2 also makes the mapping *simpler*, not just more current:

| ARC scale-set shape | v1 required | v2 gives |
|---|---|---|
| N scale sets in one namespace | collapse into 1 CR (one-gateway-per-namespace) | multiple gateways per namespace |
| `template` inlined per scale set | inline `podTemplate` per group | `RunnerTemplate` reuse |
| single-name `runs-on` routing | label-set matching (a rewrite) | ScaleSet single label, a 1:1 map |
| scale-set (single-acquirer) protocol | Classic only | `ScaleSet` default |

The third row is the one that changes the guide's shape most: routing was its "the one that bites" section under v1, and under v2 it is a straight correspondence.

## Decision — new tenants onboard on `v2beta1`

Recorded here because the version split is not self-evident from the CRD chart, which serves both.

- **`v2beta1`** is the graduated storage/hub version and is **ScaleSet-only** (the graduation strips `acquisitionProtocol`/`maxListeners`).
  It is the version every tenant-facing onboarding path names.
- **`v2alpha1`** stays served for exactly one job: the `gag-migrate` on-ramp.
  A migrating v1 tenant lands there because its groups are multi-label Classic, which `v2beta1` cannot express; it converts up once it no longer needs Classic.

A **new** tenant never needs Classic, so it never needs `v2alpha1`.
Onboarding docs that named `v2alpha1` were handing new tenants the migration on-ramp and its deprecated protocol selector.

Corollary for the ARC guide: an ARC scale-set user is a new tenant, so it targets `v2beta1` with a single `runnerLabel` per set.

### What deliberately keeps naming `v2alpha1`

Not everything is a stale reference; these are load-bearing and were left alone:

| Surface | Why it stays |
|---|---|
| [`migration-v1-to-v2.md`](../../operations/migration-v1-to-v2.md) | `gag-migrate` **emits** `v2alpha1` (Classic preservation). Flipping it would describe a tool that does not exist. |
| [`troubleshooting.md`](../../operations/troubleshooting.md) | Version-specific diagnostics: quoted admission messages name `vrunnerset-v2alpha1.kb.io`, and the `acquisitionProtocol` rejections are `v2alpha1`-only by construction. |
| `install.md` detection lines | Quoted GMC log strings (`cmd/gmc/cmd/wiring.go:93`). Editing the prose to `v2beta1` would stop matching what an operator greps. |
| `design/appendix-h`, `design/03-api-contracts`, `development/*` | Design/implementation record of the decomposition, where the alpha shape is the subject. |

## Scope

One PR, because the two slices are not separable: the ARC rewrite has to emit a version, and emitting `v2alpha1` would have re-introduced the contradiction Q409 exists to remove.

### Slice A — Q409 version alignment

- `README.md`, `docs/index.md`, `docs/roadmap.md`, `docs/why-gag.md` — positioning prose + the v2 example blocks.
- `docs/getting-started.md` — the v2 walkthrough (§4).
- `docs/operations/tenant-onboarding.md` — the v2 section; its heading drops "(alpha)", and the `acquisitionProtocol` subsection is re-scoped as `v2alpha1`-only rather than presented as a choice a new tenant makes.
- `docs/operations/v1alpha1-deprecation.md` — the "start on v2" pointer.

`maxListeners` is removed from every flipped example: the field does not exist on `v2beta1`, so an example carrying it would be rejected at admission.

### Slice B — the ARC guide

Rewrite [`migration-from-arc.md`](../../operations/migration-from-arc.md) onto `v2beta1` end to end:

- Concept-mapping table re-pointed at the v2 kinds (`RunnerSet`, `RunnerTemplate`, `EgressProxy`), including the rows above that v1 mapped badly.
- The routing section reframed: ARC scale-set → GAG ScaleSet is a 1:1 name map, so it stops being the guide's headline hazard.
  Legacy-ARC multi-label readers get the split-into-one-set-per-target instruction instead.
- The worked migration re-cut against the v2 object set, with the ARC scale-set name carried as the single `runnerLabel` so `runs-on` needs no workflow edit.
- The trailing "v2 differences worth knowing" section retired — its content is now the body of the guide.
- v1 reduced to a single pointer at the deprecation notice.

## Out of scope

- **Removing `v1alpha1`.** Still [Q273](../../STATUS.md#Q273), still gated on the deprecation window elapsing.
  This PR changes where readers are *sent*, not what is *served*.
- **`v2alpha1` doc removal.** It stays served and stays documented for the migration on-ramp.
- **Q398** (unqualified `kubectl` edits on a Classic `RunnerSet` hit the `v2beta1` storage version).
  Orthogonal, and it only reaches migrated Classic sets — a new tenant onboarded per this decision never hits it.
  Fixed separately by ratcheting the single-label rule onto `runnerLabels` ([v2beta1.md](../v2beta1.md#6-q74--the-graduation-cut)).
