# Q284: Expand `PodScheduling` to the full desirable scheduling surface

**Status:** ✅ Done (2026-07-12).
Shipped: both fields on `PodScheduling`, the infra-only `--allowed-infra-priority-classes` allowlist with a boot-time disjointness check, the extended `EgressProxy` webhook, a new v2 `ActionsGateway` validating webhook, unit + envtest coverage, and operator/design docs. §2's two-allowlist governance is promoted into [`docs/design/05-security.md` §5.2](../../design/05-security.md#the-two-priorityclass-allowlists-must-stay-disjoint-worker-vs-infra-q284).

> **Sized `M`, not `S`.** The API diff was two fields, but gating one of them needed a v2 `ActionsGateway` validating webhook that did not exist (§2.3) — the bulk of the work.

**Scope:** Add `topologySpreadConstraints` and `priorityClassName` to the `PodScheduling` block shared by `EgressProxy` and `ActionsGateway` (`api/v2beta1/scheduling_types.go`).
Purely additive — no conversion work, no breaking change.

This plan exists because the second field carries a security governance decision that does not fit in a Queue row, and that a future session would otherwise re-derive (or, worse, get wrong by reusing the obvious existing allowlist).

---

## 1. The gap

[Q282](https://github.com/actions-gateway/github-actions-gateway/pull/578) gave `EgressProxy` and `ActionsGateway` a `spec.scheduling` block carrying the three standard placement controls: `nodeSelector`, `tolerations`, `affinity`.
Worker pods, by contrast, take placement through a full `corev1.PodTemplateSpec` on `RunnerTemplate`.

`PodScheduling` is deliberately a narrow block rather than a `PodTemplateSpec` — a `PodTemplateSpec` generates ~600 KB of OpenAPI, which is why the `RunnerTemplate` CRDs weigh 1.21 MB each and why the v2 CRDs ship in their own opt-in chart at all (§H.13, Q149).
The rationale is recorded in full in the `PodScheduling` godoc; it is not repeated here.

The consequence is a real gap.
Workers can express `topologySpreadConstraints`, `priorityClassName`, `schedulerName`, `runtimeClassName`, and `nodeName`; the proxy and AGC pods cannot.
Two of those five matter:

| Field | Why it matters for infra pods |
|---|---|
| `topologySpreadConstraints` | The modern successor to the required cross-node `podAntiAffinity` the proxy pool relies on today. Expresses "spread across zones, tolerate skew of 1" — which anti-affinity cannot. |
| `priorityClassName` | An evicted proxy pod takes that tenant's **entire egress path** down; an evicted AGC pod takes that tenant's control plane down. Under node pressure both are currently as evictable as any best-effort pod. |

The remaining three (`schedulerName`, `runtimeClassName`, `nodeName`) are out of scope: `nodeName` bypasses scheduling entirely, and the other two have no demonstrated need on pods whose image and container spec are controller-enforced invariants.

## 2. The governance decision: `priorityClassName` needs a *separate* allowlist

**Decision.** `spec.scheduling.priorityClassName` on `EgressProxy` and `ActionsGateway` must be gated by a **new, infra-only allowlist** — not by the existing `--allowed-priority-classes` flag.

**Do not reuse `--allowed-priority-classes`.** This is the trap, and it is not obvious.

### 2.1 Why the obvious approach inverts the priority ordering

`--allowed-priority-classes` (plus its ConfigMap augmentation, `--priority-class-allowlist-configmap`, Q188) is the **worker-facing** allowlist.
It gates every place a *tenant* can name a PriorityClass for a *worker* pod:

- `ActionsGateway.runnerGroups[].priorityTiers[].priorityClassName`
- `ActionsGateway.runnerGroups[].podTemplate.spec.priorityClassName` (Q289)
- `RunnerTemplate.podTemplate.spec.priorityClassName` (Q289)

Infra pods need to sit at a **higher** priority than workers — that is the whole point of gating them.
Suppose we reuse the worker allowlist and add `gag-infra-critical` to it so that an `EgressProxy` may name it.
The allowlist has exactly one namespace of meaning, so that same value is now nameable from `RunnerTemplate.podTemplate.spec.priorityClassName`.

Any tenant can then lift its **worker** pods to infra priority — and preempt *other tenants'* proxy pods.
The gate intended to protect the proxy becomes the mechanism for evicting it.
The ordering inverts.

Two allowlists, disjoint by construction:

| Allowlist | Gates | Settable by |
|---|---|---|
| `--allowed-priority-classes` (existing) | worker pods: `priorityTiers`, `podTemplate.spec.priorityClassName` | tenant, via `ActionsGateway` / `RunnerTemplate` |
| `--allowed-infra-priority-classes` (new) | `EgressProxy` / `ActionsGateway` `spec.scheduling.priorityClassName` | tenant, but only from a platform-curated set that workers cannot name |

The platform admin pre-creates both sets of classes and is responsible for keeping them disjoint.
A validating check that the two lists do not intersect at GMC startup is cheap and worth having — it converts a silent priority inversion into a boot-time error.

### 2.2 Why `priorityClassName` is gated at all, when the rest of `PodScheduling` is not

The `PodScheduling` godoc records a deliberate, reviewed decision (Q282) that `nodeSelector` / `tolerations` / `affinity` are **tenant-settable and not allowlisted**.
`priorityClassName` is the exception, and the distinction is not arbitrary:

- Placement is a **choice about the tenant's own traffic**.
  The property it weakens is *attribution* (two tenants may egress via one IP), not isolation.
  Nothing about namespace isolation, RBAC, or the egress choke point changes.
- Priority is a **cluster-wide, cross-tenant preemption lever**.
  A pod naming a high-priority preempting class can evict *other tenants'* pods off a node.
  That is an isolation property, not an attribution one.

Additionally — and this is the fact that makes an ungated `priorityClassName` a cluster-wide bypass rather than a local nuisance:

> **`system-*` PriorityClasses are not `kube-system`-scoped.** A pod in *any* namespace may name `system-cluster-critical` (value `2000000000`, `preemptionPolicy: PreemptLowerPriority`).
> There is no built-in admission check restricting those classes to `kube-system`.
> Verified directly against a real apiserver — not inferred from the docs, which are easy to misread on this point.

So an ungated `priorityClassName` anywhere on a tenant-writable CR is a cluster-wide preemption escape, which is precisely the Q289 finding: Q132's original allowlist covered `priorityTiers` but missed `podTemplate.spec.priorityClassName`, and any tenant could preempt cluster-wide until [#584](https://github.com/actions-gateway/github-actions-gateway/pull/584) closed it.
Adding a *new* `priorityClassName` field without a gate would reopen the same hole on a new path.
Any future `PodTemplateSpec` passthrough must be audited for this.

### 2.3 Enforcement point — and the webhook that doesn't exist yet

Both kinds carrying `spec.scheduling` are v2 (`api/v2beta1/{actionsgateway,egressproxy}_types.go`; the v1alpha1 `ActionsGateway` has no `scheduling` block at all).
The webhook coverage is uneven, and this is the part most likely to be mis-scoped:

| Kind | Validating webhook today | Q284 needs |
|---|---|---|
| v2 `EgressProxy` | ✅ `cmd/gmc/internal/webhook/v2alpha1/egressproxy_webhook.go` | extend it |
| v2 `ActionsGateway` | ❌ **none** | **a new webhook** |
| v1alpha1 `ActionsGateway` | ✅ `cmd/gmc/internal/webhook/v1alpha1/actionsgateway_webhook.go` | n/a — no `scheduling` field |

The v2 webhook package covers `egressproxy`, `runnerset`, and `runnertemplate` only.
The v1alpha1 `ActionsGateway` webhook is a **different API group** and does not match v2 `actionsgateways`.
So gating `ActionsGateway.spec.scheduling.priorityClassName` requires standing up a v2 `ActionsGateway` validating webhook from scratch — registration, chart wiring (`make chart-webhook-check` enforces drift), and envtest coverage.
**This is the bulk of the work, and it is the reason Q284 is not the trivial `S` its diff size suggests.**

The same residual G.7 recorded for the worker allowlist applies here: an operator who grants a tenant direct RBAC on the underlying resource bypasses the webhook, and stored objects are never re-validated.
The Q289 `ValidatingAdmissionPolicy` backstop is now delivered for the worker PriorityClass allowlist ([G.7](../../design/appendix-g-future-enhancements.md#g7-validatingadmissionpolicy-for-direct-runnergroup-priorityclass-enforcement)); a Q284 infra allowlist should get the same VAP treatment on its own pass, following that policy's paramKind-ConfigMap pattern.

## 3. `topologySpreadConstraints` — composes with the built-in anti-affinity

The GMC stamps a **required** cross-node `podAntiAffinity` on every proxy pool.
The `affinity` field's rule (Q282) is *"set `podAntiAffinity` to any non-nil value and it replaces the built-in term entirely — set it and you own it."*

**Decision: `topologySpreadConstraints` COMPOSES with the built-in anti-affinity; it does not replace it.** `podAntiAffinity: {}` remains the *single* opt-out lever for the built-in cross-node spread.
Do not add a second one.

It is tempting to make this field symmetric with `affinity` — it *is* the spread mechanism, after all, so "set it and you own it" reads as the consistent choice.
That reasoning is wrong here, and the asymmetry is principled:

- **Two opt-out levers for one invariant is worse than one.** The built-in anti-affinity is a durability invariant (one node failure must not take the pool down).
  `affinity` has to be able to displace it because `podAntiAffinity` occupies the same field — there is nowhere else for an author's anti-affinity to go.
  `topologySpreadConstraints` is a *different* field with no such collision, so displacing the invariant would be a choice, not a necessity.
  Making it a second, implicit opt-out means an author who wanted zonal spread silently loses cross-node spread.
- **Composition is safe, and provably so.** `topologySpreadConstraints` can only *narrow* the candidate node set, never widen it — exactly like `nodeSelector` AND-ing with `nodeAffinity`.
  And its `labelSelector` counts only pods in the constraint's own namespace, so one tenant's spread constraint cannot be skewed by another tenant's pods.
  There is no soundness reason to drop the built-in term.

The cost is the `Pending` trap: an author who asks for a soft zonal spread (`whenUnsatisfiable: ScheduleAnyway`) still inherits the *required* cross-node anti-affinity, so replicas beyond the node count strand in `Pending`.
That is the existing behavior for any proxy pool with `minReplicas` above its node count, and the existing escapes apply — `podAntiAffinity: {}`, or lowering `minReplicas`.
**Document it on the field**, in the godoc, next to the `affinity` precedence note.

Unlike `priorityClassName`, `topologySpreadConstraints` needs **no allowlist**: it is namespace-scoped and narrowing, so it carries no cross-tenant lever.

Lock the composition with a unit test in `cmd/gmc/internal/controller/egressproxy_scheduling_test.go`, alongside the existing `TestEgressProxyScheduling_NoneSetPreservesBuiltInAntiAffinity`.

## 4. CRD size budget

`PodScheduling` is narrow specifically to keep the v2 CRDs under the apiserver's ~1.5 MiB per-object ceiling.
`affinity` alone added roughly 72 KB of CRD schema *per served version*.

`topologySpreadConstraints` carries a `labelSelector` plus `matchLabelKeys` / `nodeAffinityPolicy` / `nodeTaintsPolicy`; `priorityClassName` is a bare string.
Both are far smaller than `affinity`, but the budget is real and shared across two served versions (`v2alpha1`, `v2beta1`).

**Measure before and after.** `make chart-crds` then check the rendered CRD sizes; the v2 CRDs already ship via `helm template | kubectl apply --server-side` rather than `helm install` precisely because they exceed the 1 MiB Helm release-Secret limit.

## 5. Definition of done

- [x] `topologySpreadConstraints` and `priorityClassName` on `PodScheduling`, both `+optional`.
- [x] Godoc on each field carrying the rationale — matching the existing bar in `scheduling_types.go`.
- [x] `--allowed-infra-priority-classes` flag on the GMC, disjoint from `--allowed-priority-classes`, empty-by-default (forbids all).
  (Flag-only; a watched-ConfigMap augmentation like Q188's is deferred — filed as a Queue follow-up.)
- [x] GMC startup rejects an intersection between the two allowlists (`allowlist.Intersection`).
- [x] `EgressProxy` webhook extended to gate `spec.scheduling.priorityClassName`.
- [x] **A new v2 `ActionsGateway` validating webhook** (none existed — see §2.3), registered, chart-wired (`make chart-webhook`), gating the same field.
- [x] Envtest coverage for both (`cmd/gmc/internal/controller/integration/v2_scheduling_admission_test.go`).
- [x] `topologySpreadConstraints` composes with (does not replace) the built-in anti-affinity — unit-tested (`egressproxy_scheduling_test.go`), and the `Pending` trap documented on the field.
- [x] CRD size measured before/after.
  Each of the two CRDs grew **+34,336 bytes** (≈17 KB per served version, almost all `topologySpreadConstraints`; `priorityClassName` is a bare string): `actionsgateways` 222,037 → 256,373; `egressproxies` 178,508 → 212,844.
  Both stay far under the ~1.5 MiB per-object ceiling.
- [x] Operator docs: `docs/operations/security-operations.md` (the new allowlist + disjointness, the two new fields) and `docs/operations/tenant-onboarding.md` (troubleshooting row).
- [x] **On close:** promoted §2 into `docs/design/05-security.md` §5.2 — the two-allowlist rule is durable API-surface governance and now outlives this plan's archival.
