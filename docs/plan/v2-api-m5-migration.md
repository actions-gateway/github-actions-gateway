# Q165 — v2 API M5: migration tool + v1/v2 cutover (implementation plan)

Closes the v2 API plan ([v2-api.md](v2-api.md) § M5). Design source of truth:
[appendix-h](../design/appendix-h-v2-api-decomposition.md) §H.11 (fan-out), §H.12
(dual-read window), §H.17 (migration invariants).

**Goal.** Ship a one-shot tool that fans a `v1alpha1` tenant out to the `v2alpha1`
object set, complete the v1/v2 dual-read window, and document the cutover —
without weakening any security property or stranding any resource.

## Part 1 — fan-out migration tool

Core library `cmd/gmc/internal/migrate/` (pure, golden-tested) + thin CLI
`cmd/gmc/migrate/` (`gag-migrate`). The GMC module already imports all three type
sets (gmc v1, agc v1, neutral v2), so it is the natural host.

**Input → output mapping** (`FanOut(Input) → Result`):

| v1 source | v2 emitted |
|---|---|
| `ActionsGateway` identity (`gitHubAppRef.name`, `gitHubURL`, `logLevel`, `tracing`) | v2 `ActionsGateway` (same name, `defaultProxyRef` → emitted EgressProxy) |
| `ActionsGateway.spec.proxy` (inline) | one `EgressProxy` named `<gw>-egress` |
| each authoritative `RunnerGroup` | one `RunnerSet` (`gatewayRef`, `templateRef`; `proxyRef` unset → inherits `defaultProxyRef`) |
| `RunnerGroup.spec.podTemplate` + `workerImage` | `RunnerTemplate` — **reuse-deduped** |
| `ActionsGateway.spec.securityProfile` | namespace label `actions-gateway.com/security-profile` |

**Decisions (recorded; H.17 invariants):**

- **Reuse / object-size invariant.** Template identity key = canonical JSON of the
  *built v2 `RunnerTemplateSpec`* (`podTemplate` **and** `workerImage`). `workerImage`
  participates in equality: it selects the runner container image, a material part
  of the pod shape, so two groups differing only in `workerImage` must NOT collapse.
  K groups with an identical key collapse to one `RunnerTemplate`; the name is a pure
  function of that key (`rt-<12 hex of sha256>`), so identical content yields one
  object by construction.
- **No silent direct egress (invariant 1).** The v1 proxy is required, so the tool
  always emits an `EgressProxy` and always sets `defaultProxyRef` on the gateway. The
  AGC resolves an unset `RunnerSet.proxyRef` to `defaultProxyRef` and fails closed
  (`ProxyNotFound`) if neither resolves — so leaving `proxyRef` unset is proxied, never
  `Direct`.
- **`maxListeners` 1→10 (invariant 1).** v1 unset = 1; v2 unset = 10. The tool pins
  each `RunnerSet.maxListeners` to the v1 *effective* value (1 when v1 omitted it) so
  the concurrency ceiling is preserved rather than silently raised.
- **Standalone vs inline groups (latent ambiguity).** Standalone `RunnerGroup` CRs are
  authoritative — they are the runtime set the AGC serves and what the GMC materializes
  inline `spec.runnerGroups[]` into. Inline entries with no materialized standalone CR
  are synthesized to their v1 derived name; on a name collision the standalone CR wins.
- **Naming under the §H.6 52-char cap.** Generated names: `EgressProxy` = `<gw>-egress`,
  `RunnerSet` = the v1 group name, `RunnerTemplate` = `rt-<hash>`, gateway = v1 name. Any
  name over 52 chars is truncated with a short hash suffix and a warning emitted.
- **securityProfile relocation (Q175).** Most-restrictive-wins across a namespace's v1
  gateways (v1 singleton ⇒ usually one). The label is always set (incl. baseline) so the
  posture is explicit, never silently dropped/downgraded. Privileged carries the
  namespace `privileged-profile=allowed` grant forward (domain-migrated, never invented).

**Dry-run by default** (emit manifests to stdout/dir); `--apply` applies the v2 set and
patches the namespace. The tool never reads/prints Secret contents — it rewrites the
`githubAppRef` *name* only. It never deletes v1 objects (coexistence / rollback).

## Part 2 — dual-read window (group domain + Q147 values)

The three VAPs already dual-read (M3b). Remaining gap: the **v1 GMC ActionsGateway
webhook** still reads only v1-domain keys. Complete the window there:

- `validatePrivilegedEligibility` — accept the namespace `privileged-profile=allowed`
  label on **either** domain (so a migration that relabels the namespace to the v2
  domain doesn't strand a still-running v1 privileged gateway).
- `validateSecurityProfileTransition` — accept the downgrade opt-in annotation on
  **either** domain *and* either value (`"true"` legacy / `"allowed"` new).

Secure-by-default: widens accepted *spelling* only; every invariant (explicit opt-in
required, fail-closed on absence) is unchanged. The tool relabels keys/values/finalizer
references additively (adds v2 keys, keeps v1 keys so v1 keeps working), so nothing is
stranded mid-coexistence.

## Part 3 — docs

- `docs/operations/migration-v1-to-v2.md` — operator runbook: dry-run → review → apply,
  coexistence/rollback, post-migration v1 teardown.
- `v1alpha1` deprecation notice (operations + design).
- Design-doc updates (appendix-h §H.11/§H.12 mark the tool shipped); doc-update matrix;
  website positioning if user-visible.

## Tests

- Golden-manifest unit tests on `FanOut`: fan-out, reuse collapse, reference rewrite,
  securityProfile relocation, naming-cap, a representative multi-group tenant.
- envtest (gmc integration tier): emitted manifests pass v2 CEL + reach the apiserver
  (`--apply` path); dual-read proves both domains/values accepted by the VAP + v1 webhook;
  v1/v2 coexistence.

## Exit

Tool migrates a representative tenant in dry-run **and** `--apply`; reuse holds;
securityProfile lands on the namespace; dual-read verified; `make check` green. Flip the
v2 API Progress row to ✅ and remove the Q165 Queue row (isolated `docs/STATUS.md` commit).
</content>
</invoke>
