# Q444 — PriorityClass VAP cannot resolve its params

**Status: mechanism ESTABLISHED; product exposure CLOSED (Q492).** The trigger, the code path and the two observable failure modes are all reproduced deterministically.
The defect is a kube-apiserver bug and is still unfixed upstream, but the product no longer sits on it: the PriorityClass guard's `paramKind` moved from a `ConfigMap` to the cluster-scoped `PriorityClassAllowlist` CRD, which measurement shows is structurally immune (see [Where this goes next](#where-this-goes-next)).

Symptom: every `runnergroups` / `runnersets` / `runnertemplates` write is denied with

```
ValidatingAdmissionPolicy 'gmc-priorityclass-allowlist-guard' with binding
'gmc-priorityclass-allowlist-guard-binding' denied request: failed to configure
binding: no params found for policy binding with `Deny` parameterNotFoundAction
```

while the param ConfigMap sits at exactly the name and namespace the binding references.
Because `parameterNotFoundAction: Deny` resolves params *before* any per-object matching, this denies **every** matched write, class-naming or not — a total outage of the product's CRs, cluster-wide.

Observed on Kubernetes 1.35.5 and 1.36.1.
Prior sighting: [`q414-dind-tenant-fixture.md` § Local-loop notes](q414-dind-tenant-fixture.md#local-loop-notes-not-product-defects).

## The trigger

**The param informer for a `paramKind` dies permanently, in that kube-apiserver process, the moment the set of *bindings* whose policy names that `paramKind` becomes empty for at least one policy-refresh tick (default 1s).**

For us that set has exactly one member, so `helm uninstall` — which deletes the binding — is the trigger.
`helm upgrade` never empties the set and is safe.

Two consequences follow, and they explain everything previously observed:

- The informer is keyed by `paramKind` **GVK**, shared by every policy naming it, so one policy losing its binding is enough to kill the informer for all of them.
- A policy with **no binding contributes nothing**.
  Retaining the policy across the uninstall — the reverted fix `07061175` — could never have worked; the binding is what holds the GVK alive.

## The mechanism

From `kube-apiserver` v1.36.1, `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go`:

1. `calculatePolicyData` builds `usedParams` by iterating `policiesToBindings` — bindings, not policies.
2. Any `paramKind` missing from `usedParams` has `info.cancelFunc()` called on it and is dropped from `paramsCRDControllers` (keyed by GVK).
3. `ensureParamsForPolicyLocked` takes two different paths:

   ```go
   if genericInformer, err := s.informerFactory.ForResource(mapping.Resource); err == nil {
       informer = genericInformer
       s.informerFactory.Start(instanceContext.Done())   // core types: SHARED informer
   } else {
       informer = dynamicinformer.NewFilteredDynamicInformer(...)
       go informer.Informer().Run(instanceContext.Done()) // CRDs: a FRESH informer
   }
   ```

`ConfigMap` is a core type, so it takes the first path — and that path starts a **shared** informer with a **per-instance** context.
`sharedInformerFactory` records `startedInformers[type] = true` and never clears it, so once `cancelFunc` fires:

- the informer's `Run` returns and it is never restarted — `Start()` skips it forever;
- its store is **frozen**, not cleared, at whatever it last held;
- the source still logs `informer started for /v1, Kind=ConfigMap` on every subsequent re-add, because it believes `ForResource` handed it a live informer.

Only a process restart rebuilds the factory.
That is why a restart is the recovery and why no object change helps.

### Two failure modes, decided by a race

Whether the frozen store still holds the param decides which symptom you see.
The race is between the ConfigMap's delete event and the ≤1s refresh tick:

| frozen store contents | symptom |
|---|---|
| still holds the old ConfigMap | policy silently validates against an object **that no longer exists in etcd** |
| lost it, or the param is a name it never saw | `no params found …` → every matched write denied |

A fresh install after the break lands in the second row: a newly created ConfigMap is invisible to a stopped informer.
This is also why the defect "does not fire on every run" — a clean pass may just be the first row.

The first row is the more alarming one: it fails *open* and silently, enforcing a stale allowlist with no error anywhere.

## Measurements

Run on a single-node kind cluster at v1.35.5 via [`scripts/e2e/vap-param-informer-check.sh`](../../../scripts/e2e/vap-param-informer-check.sh), all on one apiserver process, in order:

| # | what | result |
|---|---|---|
| 1 | **Arm 1** — a second ConfigMap-`paramKind` binding held throughout; probe binding + param deleted and recreated with an 8s gap | resolves the **fresh** param — the GVK never left `usedParams` |
| 2 | **Arm 2** — *every* ConfigMap-`paramKind` binding removed for 8s, then all restored | broken |
| 3 | after arm 2, probe against the **live** ConfigMap (`token=v3`) | denied |
| 4 | after arm 2, probe against the **deleted** ConfigMap (`token=v2`) | **allowed** — the store is serving an object absent from etcd |
| 5 | repoint the binding at a ConfigMap created *after* the break | `no params found …` — the exact field error |
| 6 | repoint the **policy** at a CRD `paramKind`, same broken apiserver | resolves immediately |
| 7 | restore the ConfigMap `paramKind`, restart kube-apiserver (`crictl stop`) | recovers |

Arm 1 passing and arm 2 failing on the same apiserver isolates the cause to the empty-set transition: object churn, gap length and Helm are all held constant between them.

Measurement 4 is the direct proof that the store is frozen rather than emptied.
Measurement 6 is the direct proof that the fault is per-GVK and specific to the shared-factory path — the CRD path allocates a fresh informer per context and is immune.

### Arm 3 — the CRD `paramKind` under the same transition (Q492)

Measurement 6 showed a CRD `paramKind` *resolving on an already-broken apiserver*.
That is necessary but not sufficient for the fix: it never subjected a CRD `paramKind` to its own empty-binding-set transition.
Arm 3 does, on Kubernetes **1.36.1**, ordered **before** arm 2 so no pass can be attributed to contamination:

| arm | `paramKind` | after emptying the binding set | param created after the window |
|---|---|---|---|
| 1 | ConfigMap, keeper held | `FRESH-PARAM` | — |
| 3 | `PriorityClassAllowlist` (cluster-scoped CRD) | **`FRESH-PARAM`** | **`FRESH-PARAM`** |
| 2 | ConfigMap, binding set emptied | `STALE-PARAM` | `NO-PARAMS` |

Arm 3 and arm 2 run the byte-identical transition one GVK apart, on one apiserver process.
That contrast — not the source read — is what makes "move the `paramKind` to a CRD" a fix rather than an inference.

Note `kubectl delete pod -n kube-system kube-apiserver-…` **does not restart the apiserver** — it recreates the static pod's *mirror object* while the container keeps running (`restartCount` stays 0).
Use `crictl stop` and confirm via the container's `createdAt`.
A conclusion was drawn from that non-restart earlier in this investigation and had to be withdrawn.

## Ruled out

- **Retaining the paramKind-bearing policy across the uninstall.** Shipped as `07061175`, reverted in `70b4b351`.
  Now explained: `usedParams` is built from bindings, so a policy without one is invisible to it.
- **CNI.** CI reproduced it on both kindnet and calico lanes.
- **Object churn rate, cluster ConfigMap count, cluster load.** Arm 1 holds all of these constant against arm 2 and does not break.
- **Helm.** Helm is not involved in param resolution; it only performs the delete that empties the binding set.
  The same break reproduces with plain `kubectl`.

The four scenarios tested earlier all reported "recovers" because each either kept the binding set non-empty or left the param in the frozen store — the second row of the table above, mistaken for health.

## Upstream

Filed at [kubernetes/kubernetes#130887](https://github.com/kubernetes/kubernetes/issues/130887), open since 2025-03.
Our mechanism write-up is [posted there](https://github.com/kubernetes/kubernetes/issues/130887#issuecomment-5111110494).

The bug is `informerFactory.Start(instanceContext.Done())`: a shared factory must not be started with a caller-scoped context, because `startedInformers` makes that first context permanent for the process.

**Fix proposed upstream:** [kubernetes/kubernetes#141015](https://github.com/kubernetes/kubernetes/pull/141015) — start the shared factory with the policy source's own context, and make that branch's `cancelFunc` an explicit no-op, since a `SharedInformerFactory` can neither stop an individual informer nor restart one it has already started.
Its regression test fails on master with two `informer started` log lines while resolution stays broken: the source logs a start the factory silently refuses to perform.
The alternative offered there is to drop the shared-factory fast path entirely and always use a dynamic informer — uniform and genuinely stoppable, at the cost of losing informer sharing for the common ConfigMap case.

**Q492 did not wait for it.** As of 2026-07-28 that PR is unreviewed and untriaged, v1.37 is in code freeze, and it would still need a cherry-pick to reach a version anyone runs — so the fix that shipped is ours, not upstream's, and does not depend on this landing.
See [Still open upstream](#still-open-upstream).

Related but **not** the same bug: [#122658](https://github.com/kubernetes/kubernetes/issues/122658) / [PR #123003](https://github.com/kubernetes/kubernetes/pull/123003) — there a CRD `paramKind` fails because discovery has not caught up and an apiserver restart *causes* it; the fix merged for v1.30 in Jan 2024.
Ours is a core type that is always discoverable, and a restart *fixes* it.

## Reproducing

- [`scripts/e2e/vap-param-informer-check.sh`](../../../scripts/e2e/vap-param-informer-check.sh) — the deterministic three-arm reproducer above.
  Self-contained (its own CRDs, namespace and policies), no chart required.
  **Run it only against a disposable cluster**: arm 2 permanently breaks ConfigMap param resolution for that apiserver process.
  Not wired into CI — it deliberately destroys the cluster it runs on, and it pins an *upstream* defect that our own code no longer depends on.
  It is the evidence for the fix, re-runnable by hand on a new Kubernetes minor to confirm arm 3 still holds.
- [`scripts/e2e/chart-reinstall-check.sh`](../../../scripts/e2e/chart-reinstall-check.sh) (`make chart-reinstall-check`) — the product-level check, driving a real uninstall/reinstall against an installed release.
  **Wired into CI as of Q492**, now that the cycle it exercises is expected to pass.

## Operator impact

**On releases from Q492 onward: none.** The guard's `paramKind` is a CRD, so the binding-set transition is survivable and no apiserver restart is ever needed.
Our GKE dogfood — which could not restart its apiserver — is no longer exposed.

**On pre-Q492 releases**, recovery is a kube-apiserver restart, which is **not available on a managed control plane** (GKE/EKS/AKS).
Blast radius, symptoms and recovery for that case remain documented in [`../operations/troubleshooting.md`](../../operations/troubleshooting.md); the migration off it is [upgrade.md § PriorityClass allowlist: ConfigMap to CR](../../operations/upgrade.md#priorityclass-allowlist-configmap-to-cr).

**The release notes need three upgrade caveats**, all on the curated-notes path in [`../operations/release.md`](../../operations/release.md), each linking the upgrade.md section above:

1. A **new pre-upgrade step for everyone** — `helm show crds <chart> | kubectl apply -f -`.
   Helm never installs the chart-root `crds/` dir on upgrade, so unattended pipelines fail on the first upgrade across this change until they add it.
2. The **breaking values change**: `priorityClassAllowlist.configMapName` is removed and a release that sets it fails the Helm render.
3. **Rollback past this release is not safe on a running cluster.** Measured: the rollback recreates a ConfigMap-`paramKind` binding whose informer this very upgrade killed, so it re-arms the Q444 outage and does not self-clear.
   Roll forward, or upgrade down with `admissionPolicy.enabled=false`.
   This is the caveat most likely to be discovered the hard way, because `helm rollback` reports success.

## Where this goes next

Q444 as scoped ("find what triggers it") is answered, and **Q492 shipped the fix**: option 1 below, the `paramKind` moved off a core type.

Two candidates were on the table, both entirely in our control:

1. **Move the `paramKind` off a core type** to a small CRD.
   Measurement 6 showed the CRD path is structurally immune, because it allocates a fresh informer per context; [arm 3](#arm-3--the-crd-paramkind-under-the-same-transition-q492) then confirmed it survives the actual failing transition.
   Cost: a CRD, its RBAC, and a migration for the existing ConfigMap.
   **This is what shipped.**
2. **Keep the binding alive across `helm uninstall`** with `helm.sh/resource-policy: keep` on the VAPB and its param ConfigMap, so the binding set never empties.
   Much cheaper — but it collides with an existing invariant: [`scripts/manifest/manifest-validate.sh`](../../../scripts/manifest/manifest-validate.sh) asserts that **no VAPB carries `helm.sh/resource-policy: keep`**, because a retained binding leaves the guard enforcing after the release is gone and makes `admissionPolicy.enabled=false` a silent no-op.
   That reason still holds and is worse than the defect it would paper over.
   It also only defends the Helm path; anything else that deletes the binding still breaks the cluster.
   **Rejected**; the invariant it would have overturned is still enforced.

Note the historical irony: retaining the *policy* was tried and reverted (`07061175` / `70b4b351`), and the binding — the one object that would have worked — is precisely the one we forbid retaining.

### What shipped in Q492

- `PriorityClassAllowlist`, a cluster-scoped CRD in `api/v2beta1`, is the guard's `paramKind` **and** the object the GMC watches for restart-free allowlist additions (Q188).
  One object, two consumers, so the webhook and the policy cannot drift — which also retires the superset discipline the admin-owned ConfigMap needed.
- `priorityClassAllowlist.configMapName` is removed; setting it fails the Helm render with migration instructions rather than silently narrowing a security allowlist.
- Its CRD ships in the chart-root `crds/` dir — the only one that does.
  Helm resolves REST mappings for the whole manifest before applying any of it, so a CR whose CRD is a template in the same release fails the install outright.
  The cost is that `helm upgrade` does not carry schema changes to that CRD.
- `scripts/e2e/chart-reinstall-check.sh` is now a CI gate.

### Still open upstream

The apiserver bug itself is unfixed.
[kubernetes/kubernetes#141015](https://github.com/kubernetes/kubernetes/pull/141015) remains unreviewed and untriaged, v1.37 is in code freeze, and it would still need a cherry-pick to reach a version anyone runs.
Nothing about our fix depends on it landing — a CRD `paramKind` is immune either way — so this is now a "watch, don't block" item.
Anyone adding a **new** VAP to this repo should still take the rule from it: **never use a core type as a `paramKind`.**
