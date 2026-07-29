# Q444 — PriorityClass VAP cannot resolve its params

**Status: mechanism ESTABLISHED.** The trigger, the code path and the two
observable failure modes are all reproduced deterministically. The defect is a
kube-apiserver bug; what remains is a fix, tracked separately (see
[Where this goes next](#where-this-goes-next)).

Symptom: every `runnergroups` / `runnersets` / `runnertemplates` write is denied
with

```
ValidatingAdmissionPolicy 'gmc-priorityclass-allowlist-guard' with binding
'gmc-priorityclass-allowlist-guard-binding' denied request: failed to configure
binding: no params found for policy binding with `Deny` parameterNotFoundAction
```

while the param ConfigMap sits at exactly the name and namespace the binding
references. Because `parameterNotFoundAction: Deny` resolves params *before* any
per-object matching, this denies **every** matched write, class-naming or not —
a total outage of the product's CRs, cluster-wide.

Observed on Kubernetes 1.35.5 and 1.36.1. Prior sighting:
[`archive/q414-dind-tenant-fixture.md`](archive/q414-dind-tenant-fixture.md)
§ Local-loop notes.

## The trigger

**The param informer for a `paramKind` dies permanently, in that
kube-apiserver process, the moment the set of *bindings* whose policy names that
`paramKind` becomes empty for at least one policy-refresh tick (default 1s).**

For us that set has exactly one member, so `helm uninstall` — which deletes the
binding — is the trigger. `helm upgrade` never empties the set and is safe.

Two consequences follow, and they explain everything previously observed:

- The informer is keyed by `paramKind` **GVK**, shared by every policy naming it,
  so one policy losing its binding is enough to kill the informer for all of them.
- A policy with **no binding contributes nothing**. Retaining the policy across
  the uninstall — the reverted fix `07061175` — could never have worked; the
  binding is what holds the GVK alive.

## The mechanism

From `kube-apiserver` v1.36.1,
`staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go`:

1. `calculatePolicyData` builds `usedParams` by iterating `policiesToBindings` —
   bindings, not policies.
2. Any `paramKind` missing from `usedParams` has `info.cancelFunc()` called on it
   and is dropped from `paramsCRDControllers` (keyed by GVK).
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

`ConfigMap` is a core type, so it takes the first path — and that path starts a
**shared** informer with a **per-instance** context. `sharedInformerFactory`
records `startedInformers[type] = true` and never clears it, so once
`cancelFunc` fires:

- the informer's `Run` returns and it is never restarted — `Start()` skips it
  forever;
- its store is **frozen**, not cleared, at whatever it last held;
- the source still logs `informer started for /v1, Kind=ConfigMap` on every
  subsequent re-add, because it believes `ForResource` handed it a live informer.

Only a process restart rebuilds the factory. That is why a restart is the
recovery and why no object change helps.

### Two failure modes, decided by a race

Whether the frozen store still holds the param decides which symptom you see.
The race is between the ConfigMap's delete event and the ≤1s refresh tick:

| frozen store contents | symptom |
|---|---|
| still holds the old ConfigMap | policy silently validates against an object **that no longer exists in etcd** |
| lost it, or the param is a name it never saw | `no params found …` → every matched write denied |

A fresh install after the break lands in the second row: a newly created
ConfigMap is invisible to a stopped informer. This is also why the defect "does
not fire on every run" — a clean pass may just be the first row.

The first row is the more alarming one: it fails *open* and silently, enforcing a
stale allowlist with no error anywhere.

## Measurements

Run on a single-node kind cluster at v1.35.5 via
[`scripts/vap-param-informer-check.sh`](../../scripts/vap-param-informer-check.sh),
all on one apiserver process, in order:

| # | what | result |
|---|---|---|
| 1 | **Arm 1** — a second ConfigMap-`paramKind` binding held throughout; probe binding + param deleted and recreated with an 8s gap | resolves the **fresh** param — the GVK never left `usedParams` |
| 2 | **Arm 2** — *every* ConfigMap-`paramKind` binding removed for 8s, then all restored | broken |
| 3 | after arm 2, probe against the **live** ConfigMap (`token=v3`) | denied |
| 4 | after arm 2, probe against the **deleted** ConfigMap (`token=v2`) | **allowed** — the store is serving an object absent from etcd |
| 5 | repoint the binding at a ConfigMap created *after* the break | `no params found …` — the exact field error |
| 6 | repoint the **policy** at a CRD `paramKind`, same broken apiserver | resolves immediately |
| 7 | restore the ConfigMap `paramKind`, restart kube-apiserver (`crictl stop`) | recovers |

Arm 1 passing and arm 2 failing on the same apiserver isolates the cause to the
empty-set transition: object churn, gap length and Helm are all held constant
between them.

Measurement 4 is the direct proof that the store is frozen rather than emptied.
Measurement 6 is the direct proof that the fault is per-GVK and specific to the
shared-factory path — the CRD path allocates a fresh informer per context and is
immune.

Note `kubectl delete pod -n kube-system kube-apiserver-…` **does not restart the
apiserver** — it recreates the static pod's *mirror object* while the container
keeps running (`restartCount` stays 0). Use `crictl stop` and confirm via the
container's `createdAt`. A conclusion was drawn from that non-restart earlier in
this investigation and had to be withdrawn.

## Ruled out

- **Retaining the paramKind-bearing policy across the uninstall.** Shipped as
  `07061175`, reverted in `70b4b351`. Now explained: `usedParams` is built from
  bindings, so a policy without one is invisible to it.
- **CNI.** CI reproduced it on both kindnet and calico lanes.
- **Object churn rate, cluster ConfigMap count, cluster load.** Arm 1 holds all
  of these constant against arm 2 and does not break.
- **Helm.** Helm is not involved in param resolution; it only performs the
  delete that empties the binding set. The same break reproduces with plain
  `kubectl`.

The four scenarios tested earlier all reported "recovers" because each either
kept the binding set non-empty or left the param in the frozen store — the
second row of the table above, mistaken for health.

## Upstream

Filed at
[kubernetes/kubernetes#130887](https://github.com/kubernetes/kubernetes/issues/130887),
open since 2025-03. Our mechanism write-up is
[posted there](https://github.com/kubernetes/kubernetes/issues/130887#issuecomment-5111110494).

The bug is `informerFactory.Start(instanceContext.Done())`: a shared factory must
not be started with a caller-scoped context, because `startedInformers` makes
that first context permanent for the process.

**Fix proposed upstream:**
[kubernetes/kubernetes#141015](https://github.com/kubernetes/kubernetes/pull/141015)
— start the shared factory with the policy source's own context, and make that
branch's `cancelFunc` an explicit no-op, since a `SharedInformerFactory` can
neither stop an individual informer nor restart one it has already started. Its
regression test fails on master with two `informer started` log lines while
resolution stays broken: the source logs a start the factory silently refuses to
perform. The alternative offered there is to drop the shared-factory fast path
entirely and always use a dynamic informer — uniform and genuinely stoppable, at
the cost of losing informer sharing for the common ConfigMap case.

**This does not close Q492.** As of 2026-07-29 that PR is unreviewed and
untriaged, v1.37 is in code freeze, and it would still need a cherry-pick to
reach a version anyone runs. Nothing above changes on 1.35.5 or 1.36.1 until a
backport lands. Revisit Q492's priority when there is a merged master commit
*and* a cherry-pick decision — not before.

Related but **not** the same bug:
[#122658](https://github.com/kubernetes/kubernetes/issues/122658) /
[PR #123003](https://github.com/kubernetes/kubernetes/pull/123003) — there a CRD
`paramKind` fails because discovery has not caught up and an apiserver restart
*causes* it; the fix merged for v1.30 in Jan 2024. Ours is a core type that is
always discoverable, and a restart *fixes* it.

## Reproducing

- [`scripts/vap-param-informer-check.sh`](../../scripts/vap-param-informer-check.sh)
  — the deterministic two-arm reproducer above. Self-contained (its own CRD,
  namespace and policies), no chart required. **Run it only against a disposable
  cluster**: arm 2 permanently breaks ConfigMap param resolution for that
  apiserver process.
- [`scripts/chart-reinstall-check.sh`](../../scripts/chart-reinstall-check.sh)
  (`make chart-reinstall-check`) — the product-level check, driving a real
  uninstall/reinstall against an installed release.

Neither is wired into CI while the defect is unfixed; doing so is part of the
fix work below.

## Operator impact

Recovery is a kube-apiserver restart, which is **not available on a managed
control plane** (GKE/EKS/AKS). Blast radius, prevention and recovery are
documented in
[`../operations/troubleshooting.md`](../operations/troubleshooting.md) and
[`../operations/install.md`](../operations/install.md).

**Our own dogfood is exposed.** [`scripts/dogfood/setup.sh`](../../scripts/dogfood/setup.sh)
runs `helm upgrade --install gag charts/actions-gateway` with no
`admissionPolicy` override, and the chart defaults `admissionPolicy.enabled:
true`. Dogfood is GKE, so its apiserver cannot be restarted. The trigger being
known lowers the risk materially — `helm upgrade` is safe, and dogfood only ever
upgrades — but an uninstall/reinstall there would still be unrecoverable.

**Surface this in the next release's notes** — while this is unfixed it is
exactly the "upgrade caveat" the curated-notes path in
[`../operations/release.md`](../operations/release.md) exists for. The
install-time decision is documented at
[install.md § Known defect (Q444)](../operations/install.md#known-defect-q444-the-policy-can-stop-resolving-its-parameters);
the notes need a line and that link.

## Where this goes next

Q444 as scoped ("find what triggers it") is answered. The remaining work is a
fix, and there are two candidates, both entirely in our control:

1. **Move the `paramKind` off a core type** to a small CRD. Measurement 6 shows
   the CRD path is structurally immune, because it allocates a fresh informer per
   context. Costs a CRD, its RBAC, and a migration for the existing ConfigMap.
2. **Keep the binding alive across `helm uninstall`** with
   `helm.sh/resource-policy: keep` on the VAPB and its param ConfigMap, so the
   binding set never empties. Much cheaper — but it collides with an existing
   invariant: [`scripts/manifest-validate.sh`](../../scripts/manifest-validate.sh)
   asserts that **no VAPB carries `helm.sh/resource-policy: keep`**, because a
   retained binding leaves the guard enforcing after the release is gone and makes
   `admissionPolicy.enabled=false` a silent no-op. That reason still holds and is
   worse than the defect it would paper over. It also only defends the Helm path;
   anything else that deletes the binding still breaks the cluster.

**Option 1 is the fix.** Option 2 should not ship without first overturning the
manifest-validate invariant, which is not worth doing. Note the historical irony:
retaining the *policy* was tried and reverted, and the binding — the one object
that would have worked — is precisely the one we forbid retaining.

Neither is in scope here; the fix is tracked as Q492 on the Queue.
