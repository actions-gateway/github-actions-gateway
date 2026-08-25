# Kubernetes API conventions

Project-specific conventions for the Kubernetes surface we author: label and annotation keys/values, status conditions, Events, pod shutdown behaviour — and the gotchas that have bitten us.
Read this before adding a new label, annotation, or CRD field that an operator sets by hand, or before writing the shutdown path of any binary that runs in a pod.

## Label & annotation value conventions

### Don't use boolean-looking values for string-matched labels/annotations

When a label or annotation value is **matched as a string** by our code (an admission webhook, a controller, a `ValidatingAdmissionPolicy`), use an explicit **enum keyword** — e.g. `allowed`, `enabled`, `managed` — never a boolean-looking value (`true`, `false`, `yes`, `no`, `on`, `off`).

Why:

- **YAML coercion footgun.** In a manifest, `my-label: true` parses as a YAML boolean, not the string `"true"`.
  A Kubernetes label/annotation value must be a string, so the unquoted form either errors or has to be remembered as `"true"` (quoted) every time.
  YAML 1.1 coerces `yes`/`no`/`on`/`off` (and their capitalised variants) the same way, so the trap is wider than just `true`/`false`.
- **Self-documenting.** `actions-gateway.github.com/privileged-profile: allowed` reads as a deliberate grant.
  `…: "true"` carries no meaning and invites the reader to drop the quotes.

The value is always matched **exactly** and the check is **fail-closed**: any value other than the sentinel keyword (and an absent label) is treated as "not granted".
So even if someone fat-fingers `true`, eligibility is denied rather than silently granted.

**Worked example.** The privileged-profile eligibility gate (Q133) uses

```yaml
metadata:
  labels:
    actions-gateway.github.com/privileged-profile: allowed   # not "true"
```

See `PrivilegedProfileLabel` / `PrivilegedProfileAllowed` in [`cmd/gmc/api/v1alpha1/actionsgateway_types.go`](../../cmd/gmc/api/v1alpha1/actionsgateway_types.go) and [§5.3 of the security design](../design/05-security.md#privileged-eligibility-is-a-platform-decision).

**v2 operator-set label — namespace security profile.** v2 relocates the Pod Security Admission level off the per-gateway `ActionsGateway.spec.securityProfile` (v1) onto the **tenant namespace** (Q175 / appendix-h §H.16 #7): the operator sets

```yaml
metadata:
  labels:
    actions-gateway.com/security-profile: restricted   # baseline | restricted | privileged
```

on the namespace, and the GMC `NamespacePSAReconciler` stamps the `pod-security.kubernetes.io/*` labels from it.
The value follows the enum-keyword convention above (not a boolean), and the `gmc-namespace-security-profile-guard` ValidatingAdmissionPolicy fail-closes on an invalid value, a silent downgrade, or a `privileged` selection without the `actions-gateway.com/privileged-profile=allowed` eligibility label.
See `SecurityProfileLabel` in [`api/v2alpha1/shared_types.go`](../../api/v2alpha1/shared_types.go).

### Pre-existing `"true"` values are grandfathered

Two shipped keys predate this convention and still use `"true"`:

- `actions-gateway.github.com/tenant: "true"` — the managed-tenant marker label.
- `actions-gateway.github.com/allow-profile-downgrade: "true"` — the downgrade opt-in annotation.

These were **not** to be changed casually.
The `tenant` marker in particular is load-bearing: the `namespace-psa-guard` and `gmc-tenant-resource-guard` `ValidatingAdmissionPolicy` objects, the onboarding scripts, and operator runbooks all match it as `"true"`, so changing the value is a breaking change to deployed clusters.
The convention above applies to **new** keys; the existing two stay as-is unless there is a separate, deliberate migration.

**The v2 API cutover is that deliberate migration.** v2 aligns both values to self-documenting keywords (`tenant: managed`, `allow-profile-downgrade: allowed`) on the renamed `actions-gateway.com/` domain (see `shared_types.go`).
During the v1/v2 coexistence window every consumer **dual-reads** both spellings, so deployed clusters are not broken mid-cutover; the [M5 migration tool](../operations/migration-v1-to-v2.md) relabels live namespaces additively, and the legacy `"true"` arms drop when `v1alpha1` is removed (design [§H.12](../design/appendix-h-v2-api-decomposition.md#h12-folding-in-the-grandfathered-label-value-alignment-q147)).

## Label & annotation key conventions

Use the `actions-gateway.github.com/<name>` prefix for every label and annotation key the project defines, matching the API group.
Define the key (and its sentinel value, if any) as an exported `const` in the relevant `api/v1alpha1` package with godoc, and reference that const from controllers, webhooks, and tests — never re-type the literal string, so a rename can't drift between the producer and the consumers.

**v2 (`actions-gateway.com`) keys use the owned domain from birth** — the v2 kinds and their controllers prefix labels/annotations with `actions-gateway.com/` (the group the project owns), defined as exported consts in the neutral `api/v2alpha1` package.
Controller-set v2 labels:

- `actions-gateway.com/gateway: <name>` — stamped by the v2 `ActionsGateway` reconciler on every AGC control-plane child (Deployment/SA/RoleBinding/Service/ NetworkPolicy/Secret), so M3b's per-gateway naming has an identity to key on and operators can `kubectl get -l actions-gateway.com/gateway=<name>` a gateway's resources.
- `actions-gateway.com/runner-set: <name>` (`provisioner.LabelRunnerSet`) — stamped on every v2 worker pod, job Secret, and **agent-pool Secret**; the AGC `RunnerSet` controller's Pod watch, reaper, and agent pool filter on it.
  Distinct from the v1 `actions-gateway/runner-group` key so the v1 and v2 controllers never cross-wire during coexistence.
- `actions-gateway.com/egress-proxy: <name>` — stamped on every `EgressProxy` child and pool pod, and the **sole** key of that pool's `Deployment`/`Service`/PDB/`NetworkPolicy` selectors and its hostname anti-affinity term.
  Distinct from v1's bare `app: actions-gateway-proxy` for the same coexistence reason, and it is the *selector* that matters here, not just the name — see [below](#a-selector-a-coexisting-controller-also-matches-is-a-cross-wire-q582).

### Derive a per-owner name from the owner's *kind*, not just its name (Q466)

A v1 `RunnerGroup` and a v2 `RunnerSet` of the same name share one namespace for the whole coexistence window of a migration — that is what makes rollback to v1 possible.
So **any** name or selector an AGC derives from an owner's name must carry the kind too, or the two controllers silently manage one object.
The agent pool is the worked example and the one that got this wrong: identical Secret names, identical selector labels, and identical GitHub runner names left the v1 tenant in a permanent `secrets "agentpool-<name>-<index>" already exists` loop the moment v2 came up.

| | v1 `RunnerGroup` | v2 `RunnerSet` |
|---|---|---|
| Agent Secret | `agentpool-<name>-<index>` | `agentpool-rs-<name>-<index>` |
| Selector label | `actions-gateway/runner-group` | `actions-gateway.com/runner-set` |
| GitHub runner name | `<name>-<index>` | `rs-<name>-<index>` |
| Broker session `agent.name`/`ownerName` | `<name>-<index>` | `rs-<name>-<index>` |

Two rules follow.
**The v1 spelling never moves** — v1 is the rollback target, so it has to keep finding the objects and registrations it already owns; v2 is the side that gets the discriminator.
And **splitting the Kubernetes name is not enough on its own**: the GitHub runner name is unique per registration scope, so two pools sharing one take turns deregistering each other's live record through the 409-conflict path.
Split every derived identity or none of them.

**The last row is the one that got missed, and how it got missed is the reusable part.** Q466 split the three identities the agent pool derives, and the broker session's name is derived somewhere else: the listener built its own `<CR name>-<index>` from the CR name it was handed, so a `RunnerSet` sent GitHub a name matching no runner it had registered (Q677).
An audit of the deriving package finds every site in that package and none outside it, so the question to ask is *who else spells this identity*, not *what does this package derive*.
The repair is structural rather than a fourth copy of the rule: the derivation lives on the pool alone, the pool stamps the result on `agentpool.Agent.Name`, and every consumer forwards that field.
A consumer that cannot see the `Scheme` must not be deriving a `Scheme`-dependent name at all.

Renaming a derived object in a shipped release also has to carry the existing ones across, not orphan them.
`agentpool.AdoptLegacyRunnerSetSecrets` is the pattern: on the first reconcile after the rename, copy each old-named object to its new name (preserving the payload, so no external registration is re-issued) and delete the original, gated on a check that the old name is not in use by the *other* kind.

### A selector a coexisting controller also matches is a cross-wire (Q582)

Q466 above splits derived *names*.
The same coexistence window breaks a *selector* that two controllers both match, and it breaks it in places that carry no name at all.
The v2 `EgressProxy` pool used to stamp v1's `app: actions-gateway-proxy` alongside its own identity label, purely so generic tooling could find proxy pods.
Names were already split — the pools are `actions-gateway-proxy` and `<proxy>-proxy` — but v1 keys its PDB selector, its `Deployment` selector, and its hostname anti-affinity term on that one bare label, because v1 has a single pool per namespace and never had to distinguish it.
So every one of those reached into the v2 pool: each pool's pods fell under both PDBs (a pod covered by two is one the eviction API refuses to evict), both `HorizontalPodAutoscaler`s wedged on `AmbiguousSelector`, and the pools repelled each other off every node.

Three things to carry forward when adding a label to a v2 object:

- **A shared label is a shared selector until proven otherwise.** Trace every consumer before stamping one: PDBs, `Service`s, `NetworkPolicy` pod selectors *and peers*, pod (anti-)affinity terms, and CNI-native policy selectors.
  An HPA is the trap — it has no selector of its own, so it never appears in a grep for the label, yet it inherits its scale target's and wedges on an overlap.
- **`Deployment.spec.selector` is immutable**, so narrowing one is a delete-and-recreate of a live workload, not a patch.
  Prefer the fix that lands on the *newer* side: v2's pools are alpha and a migration creates them after the upgrade, whereas every install has a running v1 pool.
  `EgressProxyReconciler.applyDeployment` carries the recreate for pools that predate the narrowing, mirroring `applyRoleBinding`'s immutable-`roleRef` path.
- **Generic identity belongs in the recommended labels**, which nothing selects on.
  `app.kubernetes.io/name=actions-gateway-proxy` is on both pools' pods and is the version-agnostic way to list every proxy pod; a functional selector must key on the owning object's identity label instead.

### Derive every name through `api/apinames` (Q467, Q473)

**All name derivation goes through [`api/apinames`](../../api/apinames/names.go).** It lives in the neutral `api` module because the GMC derives the v1 RunnerGroup name, `gag-migrate` replicates it, and the AGC consumes it as a label value — three packages across two modules that must agree byte for byte.
They previously agreed by comment ("Kept byte-for-byte identical…"), which is not a mechanism.

Two shipped bugs came from getting this wrong, and both presented to the operator the same way: no worker pod at all, and GitHub reporting that the runner *"lost communication"*.
Both were deterministic per tenant-name length, not intermittent.

**Budget against the tightest consumer, not the one you are creating.** An object name may be 253 characters, but a **label value and a Service name stop at 63**.
The v1 `<gateway>-<label>` RunnerGroup name was never bounded: past 63 characters the CR is still created — it is a legal object name — and then every worker pod carrying it as `actions-gateway/runner-group` is rejected.
A 15-character gateway with a 40-character runner label was enough. v2 avoids this class with a 52-char CEL cap on CR names ([§H.6](../design/appendix-h-v2-api-decomposition.md#h6-naming-and-length-budgets)); v1 has no such cap, so the bound is applied where the name is derived.

**Split the budget before you join the segments.** A name has rules beyond "≤63 characters" — it must also start and end with an alphanumeric character.
Building `<prefix>-<owner>-<id>` and then cutting the result at 63 satisfies the length rule and violates the other one whenever the cut lands on a hyphen, and the hyphens in a UUID sit at fixed indices.
`apinames.Join` splits the budget first (`apinames.Shares` gives each part an equal share and redistributes what a shorter part leaves over), so no cut can reach a separator.

**Never trade validity for collisions.** Trimming a trailing hyphen is not a fix on its own: it shortens the entropy-bearing suffix, and two objects that collide on a name are a worse failure than one the API server rejects.
`apinames.Truncate` replaces the discarded tail with a hash of the *whole* segment, so every segment stays injective at every budget.

**A name that already fits is never changed.** `Join` returns the plain concatenation whenever it is within budget.
That is what makes the helper adoptable: bounding a derivation renames only the tenants that were already broken, never a healthy one.

**Test at the boundary, not just under it.** `assert len(name) <= 63` passes happily on `…-`.
Sweep the name lengths that put the cut on each separator, one either side, and the maximum-length case; assert against `k8s.io/apimachinery/pkg/util/validation` (`IsDNS1123Label`, `IsValidLabelValue`) rather than a length; and let a real API server judge in an envtest, since it is the authority that rejected these in the first place.
[`api/apinames/names_test.go`](../../api/apinames/names_test.go) is the worked example, with a fuzz target for the inputs a table misses.

A rejected object must also be **legible to an operator**.
A create the API server refuses is invisible from the outside — nothing to `kubectl describe`, nothing in the tenant's namespace at all — so surface the API server's own message as a `Warning` Event on the owner (`WorkerPodCreateFailed`) rather than leaving it in a controller log the tenant cannot read.

### Own every object you derive

Every Secret, pod, or child object a controller derives from a CR carries a controller `OwnerReference` to it, built from a single exported helper per kind (`provisioner.RunnerGroupOwnerRef`, `runnerSetOwnerRef`) so the paths that stamp it cannot drift into two spellings.
Two properties come from that:

- **Lifecycle is unambiguous.** With no owner reference, nothing arbitrates between two controllers that reach for the same object — which is exactly what turned the name collision above into a permanent error loop rather than a caught mistake.
- **Cleanup survives a dead controller.** The finalizer path is the primary cleanup; the owner reference is the backstop when the AGC crashed or the namespace went away first.

Refresh the reference on **every reconcile**, not once when the cached pool or helper is built: an owner deleted and recreated under the same name has a new UID, and an object written with a UID that no longer exists is garbage-collected the instant it lands.
Leave `BlockOwnerDeletion` unset — setting it would require `update` on the owner's finalizers under the `OwnerReferencesPermissionEnforcement` admission plugin.

One further v2 label is set by the **migration tool**, not by a controller:

- `actions-gateway.com/migrated-from-namespace: <ns>` — stamped by `gag-migrate` on the `ClusterRunnerTemplate` it emits for a privileged (DinD/sysbox) v1 worker shape.
  That is the one migration output which is cluster-scoped, so deleting the tenant namespace does not garbage-collect it; the label is how an operator finds and tears it down ([migration runbook](../operations/migration-v1-to-v2.md#privileged-worker-shapes-dind-become-cluster-scoped-templates)).
  Informational provenance only — nothing selects on it for enforcement, and a platform-authored `ClusterRunnerTemplate` never carries it.

The shared `actions-gateway/component: workload` selector label is carried by both v1 and v2 worker/AGC pods (it backs the workload NetworkPolicy selector), so the egress-lockdown posture is identical across the two APIs.

### Tolerate `kubectl.kubernetes.io/restartedAt` on a managed pod template (Q552)

A GMC-managed Deployment's pod template is rebuilt from the CR on every reconcile, so anything an operator hand-edits onto it is reverted.
That is the intended contract for config drift — the CR is the source of truth — but it also reverted the annotation `kubectl rollout restart` patches in, making the restart a silent no-op: kubectl reports the *old*, trivially-complete ReplicaSet as successfully rolled out.
Every runbook that prescribed a restart was therefore prescribing nothing.

`assignManagedPodTemplate` ([`cmd/gmc/internal/controller/deployment_restart_annotation.go`](../../cmd/gmc/internal/controller/deployment_restart_annotation.go)) carries the keys in `toleratedTemplateAnnotations` over from the live template instead of reverting them; every GMC Deployment apply path routes its template assignment through it.

That list is deliberately one well-known key rather than a preserve-anything-unmanaged rule.
Preserving unmanaged keys wholesale would mean the GMC could never *remove* an annotation it had stopped setting, and would quietly turn the pod template into a partially-unowned surface.
Adding a key here is a decision to give up reconciliation of that key — take it one key at a time, and pin it with a test that the *unlisted* keys are still reverted.

### Recommended `app.kubernetes.io/*` labels (Q205)

Every object the GMC or AGC creates also carries the Kubernetes [recommended labels](https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/) — `app.kubernetes.io/{name,instance,component,part-of,managed-by}` (and `version` where a meaningful one exists) — stamped via the shared [`api/apilabels`](../../api/apilabels/labels.go) helper so the GMC and AGC never diverge on the keys or the `part-of` value.
They are **additive metadata**: stamp them *alongside* the functional selector labels above, never in place of them, and never build a controller's pod/Service selector on them (an operator may relabel them).
`apilabels.Merge` preserves any existing key, so it cannot clobber a selector label.
The canonical per-component values and operator `kubectl -l` recipes live in [observability.md](../operations/observability-metrics.md#selecting-gag-objects-with-the-recommended-labels).

Controller-set annotations on worker pods (both v1 and v2, stamped by the provisioner at pod creation time — from the AcquireJob payload on the classic tier, from the assignment message's `JobMessageBase` fields on the scale-set tier):

- `actions-gateway.com/run-id` — GitHub workflow run ID.
- `actions-gateway.com/repository` — `owner/repo` the job belongs to.
- `actions-gateway.com/job-name` — job name as defined in the workflow YAML.
- `actions-gateway.com/workflow` — workflow file name.
  Classic only; the scale-set protocol delivers no workflow name.

**On the classic tier, read the identity out of the payload's serialised `github` context — never out of `variables`.** A real AcquireJob body carries `contextData.github.run_id` and `.repository` and no `system.github.run_id` at all; reading the variables instead is how these annotations came to be absent from every real worker pod, taking classic-tier eviction recovery with them (Q495).
The ground truth is `testdata/job_payload.json`, a redacted capture of a live response, and `payload_groundtruth_test.go` asserts against it — extend that test rather than a hand-written payload when this parsing changes.

These are best-effort: absent if GitHub omitted the corresponding field (`contextData.github.run_id`/`.repository`/`.workflow` on classic; `ownerName`/`repositoryName`/`workflowRunId` on scale-set).
Never use them for security enforcement — they are informational annotations for operator visibility, **except** that on the scale-set tier `run-id` and `repository` are also load-bearing: they are the only record of which workflow run a worker was serving, so eviction recovery reads them back off the pod (Q417).
Adding a fifth key is fine; changing or removing either of those two breaks recovery on that tier.

One further controller-set annotation is stamped at pod build, on both tiers, only when an opt-in sizing profile actually derived the pod's ask:

- `actions-gateway.com/sizing-profile: <Binpack|Throughput|NodeShare>` (`provisioner.AnnotationSizingProfile`) — the profile that produced this pod's cpu/memory values.
  A pod built from the template's static values (`Static`, or a history-based profile still `AwaitingSamples`) carries no such annotation, so presence answers "was this sized by the profile or by the template?" without reverse-engineering it from the numbers.
  It is load-bearing for the `SizingProfileOverridden` condition (Q489), which compares what the profile built against what the apiserver admitted and must not mistake a template-built pod for a profile-built one.
  Informational otherwise; never set it by hand.

One controller-set annotation is stamped at pod creation on the ScaleSet tier only:

- `actions-gateway.com/runner-name` (`provisioner.AnnotationRunnerName`) — the name the listener pre-registered this pod's runner under at GitHub.
  `generatejitconfig` creates the record before the pod exists, and nothing else remembers the name once the listener goroutine that minted it has moved on, so the pod is the record: the reaper reads it back to deregister the registration when it deletes the pod, and the listener's start-up sweep treats a name stamped on a live pod as claimed and therefore not collectable (Q550).
  Load-bearing in the same sense as `run-id` on this tier — a pod without it is reaped with its GitHub registration left behind, and because runner names derive from the job ID, that leftover is what the job's own retries collide with.
  Never set it by hand.

Two further controller-set annotations are stamped *after* pod creation, on the ScaleSet tier only:

- `actions-gateway.com/job-completed-at` (`provisioner.AnnotationJobCompletedAt`) — RFC 3339 UTC time at which the scale-set listener saw the terminal `JobCompleted` for the job this pod was created for.
  It is the reap deadline for a pod whose own job is over, in either non-terminal phase: five minutes for one still `Running` (Q420), thirty seconds for one still `Pending` (Q575), which can no longer start because the same completion reclaimed the JIT-config Secret it mounts.
  Set once — a completion replayed to a re-created session must not push the deadline back — and never set on the classic path, whose `provision()` goroutine owns its pod through to a terminal phase.
- `actions-gateway.com/eviction-handled-at` (`provisioner.AnnotationEvictionHandledAt`) — RFC 3339 UTC time at which the owning reconciler adjudicated this pod's eviction, whether it went on to trigger a re-run, found no run identity, or found the budget exhausted (Q417).
  It is a **claim**, not a log line: it is written under an optimistic lock *before* the GitHub call, which is what makes automatic recovery at-most-once per evicted pod across reconciles, restarts, and replicas.
  If you add another post-creation side effect keyed off a worker pod, follow the same pattern — claim first with `client.MergeFromWithOptimisticLock`, then act.

One further controller-set annotation is stamped *after* pod creation, on **both** tiers:

- `actions-gateway.com/deletion-reason` (`provisioner.AnnotationDeletionReason`) — the reap reason (e.g.
  `completed_ttl`, `pending_deadline`), stamped immediately before the reaper deletes the pod (Q502).
  It marks the deletion as the AGC's own, which is what excludes reaper cleanup from graceful-deletion recovery — an unstamped deletion of a worker that then publishes a terminal phase is read as a drain and re-runs the interrupted job.
  **Any new AGC code path that deletes a worker pod must stamp this annotation before the delete** (stamp-then-delete, in that order), or its deletions become re-run triggers; the reaper's `reapWorkerPodsByLabel` is the pattern.
  The one exception is a delete of a pod that has *already* reached its terminal phase (the `completedPodTTL: 0` cleanup), which the detection's ordering rule excludes on its own.

  **A new delete also owes the specs.** What a worker pod publishes at its terminal phase is what the drain/cancel specs assert, so a new deleter can falsify one without failing it, and the live-GitHub half skips without credentials, so its green means nothing (Q599; see [testing.md](testing.md#a-credential-gated-spec-that-skips-is-not-defending-anything)).
  `cmd/agc/internal/provisioner/deletion_inventory_test.go` is the tripwire: it fails on any added, moved, or renamed client `Delete` in the `agc` module and prints the spec roster to read before updating it.

Alongside them, the scale-set tier stamps one controller-set **label** at pod creation:

- `actions-gateway.com/acquisition-protocol` (`= "ScaleSet"`) — the tier marker.
  Presence is the tier test: a classic worker carries no such label.
  It exists because the two tiers own a worker's lifecycle differently, so eviction recovery must filter on it or one eviction would be recovered twice (once inline by the classic `provision()` goroutine, once by the reconciler), spending two slots of one run's retry budget.

The provisioner also gap-fills three **node-disruption-safety** annotations on every worker pod, so a node autoscaler or the descheduler does not evict a pod mid-job and strand the CI run (these are third-party well-known keys, not our `actions-gateway.com/` domain):

- `karpenter.sh/do-not-disrupt: "true"` — Karpenter consolidation/drift opt-out.
- `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` — Cluster Autoscaler scale-down opt-out.
- `descheduler.alpha.kubernetes.io/prefer-no-eviction: "true"` — descheduler opt-out (current well-known key; the older `…/evict` is opt-*in* only).

Gap-fill only: a value for any of these keys set in the runner's `podTemplate.metadata.annotations` wins (mirroring the SecurityContext gap-fill).
Only these three keys are honored from the template; arbitrary `podTemplate` annotations are not copied onto worker pods.
The markers live on the pod, so they release automatically when the pod is torn down on job completion.
Operator-facing detail in [observability.md](../operations/observability-metrics.md#node-disruption-safety-annotations).

## A new controller write verb updates the role pair in the same change

When controller code gains a client write it did not make before — a `Patch` on a kind it only deleted, an `Update` on one it only read, any new (resource, verb) combination — the change must also land the grant, in both halves of the pair: the `+kubebuilder:rbac` markers (`cmd/agc/internal/controller/doc.go`) **and** the chart's hand-maintained rules fragment (`charts/actions-gateway/files/agc-*-rules.yaml`; see [code-generation.md § agc-tenant-role](code-generation.md#agc-tenant-role) for why the tenant role is not generated).

Nothing catches the miss for you.
The drift gate (Q454) pairs markers with chart rules, but nothing pairs *code* with markers — and envtest does not enforce RBAC (every test client runs as admin), so unit and integration stay green and the write 403s only on a real cluster.
That is how the scale-set eviction-recovery claim and the `job-completed-at` stamp shipped patching pods without the verb and ran broken on every real install until Q502's e2e round surfaced it.
Grep for the client call (`Client.Patch`/`Update`/`Delete` on the kind) when reviewing a controller change, and treat a new verb in code with no diff under `charts/` as the smell.

One path now has an enforced-RBAC backstop: `E2E_AGC_ScaleSetRecovery` (Q519) runs scale-set disruption recovery — the claim patch and the rerun — under the chart's shipped role on kind, so a verb that path needs and the role lacks fails CI.
It covers only the verbs that path exercises; everywhere else the grep above is still the gate.

## ValidatingAdmissionPolicy `paramKind`: never a core type (Q444/Q492)

**Rule: a `paramKind` must be a CRD.
Never `ConfigMap`, `Secret`, or any other built-in.** This is not a style preference — a core type is load-bearing broken.

The apiserver keeps one parameter informer per `paramKind` GVK, shared by every policy naming it.
When the set of `ValidatingAdmissionPolicyBinding`s naming that GVK becomes empty for even one policy-refresh tick (default 1s), it calls the informer's cancel func.
For a **core type** that informer came from the shared `SharedInformerFactory`, which records `startedInformers[type] = true` and never clears it — so the informer stops and `Start()` will never run it again *for the life of that kube-apiserver process*.
Its store freezes rather than emptying.
For a **CRD** the apiserver allocates a fresh dynamic informer per context instead, so the same transition is harmless.

Deleting a binding is an ordinary operation — `helm uninstall` does it.
The two outcomes, both bad:

| frozen store | symptom |
|---|---|
| lost the param, or never saw it | `no params found for policy binding` — and because `parameterNotFoundAction: Deny` resolves params *before* per-object matching, **every** matched write is denied cluster-wide |
| still holds the old object | the policy silently validates against an object that **no longer exists**, enforcing a stale allowlist with no error anywhere |

The only recovery is restarting kube-apiserver, which managed control planes (GKE/EKS/AKS) do not offer.

Measured, not inferred: [`scripts/e2e/vap-param-informer-check.sh`](../../scripts/e2e/vap-param-informer-check.sh) runs the identical empty-binding-set transition against a ConfigMap `paramKind` and a CRD one on a single apiserver — the ConfigMap arm breaks, the CRD arm stays fresh.
Upstream: [kubernetes/kubernetes#130887](https://github.com/kubernetes/kubernetes/issues/130887) (unfixed; do not wait for it).
`scripts/e2e/chart-reinstall-check.sh` gates the product-level cycle in CI.

**CRD schema/CEL validation runs before a policy sees the request.** An update that trips an `x-kubernetes-validations` rule is rejected with the CEL message and never reaches any `ValidatingAdmissionPolicy` — so a test asserting a policy's denial must mutate a field no CRD CEL rule constrains, or the CEL error masks the policy verdict (observed in the Q518 integration test: `maxWorkers` is CEL-coupled to the last tier threshold, so mutating it never exercised the policy).

**Corollaries for a new policy:**

- Give the policy a small, purpose-built cluster-scoped CRD for its params.
  `PriorityClassAllowlist` (`api/v2beta1`) is the worked example.
- Keep `parameterNotFoundAction: Deny`.
  It is the fail-closed default and the reason a broken `paramKind` is so loud; the fix is the CRD, not weakening this.
- Do **not** try to fix it by retaining the binding across uninstall (`helm.sh/resource-policy: keep`).
  `scripts/manifest/manifest-validate.sh` forbids that on a VAPB, because a retained binding keeps enforcing after the release is gone and makes `admissionPolicy.enabled=false` a silent no-op.
- A chart that renders both a CRD and a CR of it must ship that CRD in the chart-root `crds/` dir: Helm resolves REST mappings for the whole manifest before applying any of it, so a CR whose CRD is a template in the same release fails the install with `no matches for kind`.

## Status conditions & alertable condition metrics

The CRDs report observed state with standard Kubernetes conditions (`metav1.Condition`, keyed by `type`, surfaced via `kubectl describe`).
Two conventions keep them consistent and alertable.

### Two-tier "pressure / exceeded" ladder for capacity signals

When a controller surfaces pressure against a finite resource (e.g. the namespace `ResourceQuota`), model it as a **two-tier ladder** rather than one boolean, so operators can route a *warning* and a *page* differently:

- **`<Subject>QuotaPressure`** — *warning*.
  Predictive: the subject cannot grow to its configured ceiling within the **remaining** headroom (`hard − used`).
  This is load-dependent and may flap; alert on it with an `for:` debounce and do **not** page.
- **`<Subject>QuotaExceeded`** — *error*.
  Observed/imminent: creates are being rejected now, or no headroom remains for even one more unit.
  Page-worthy (still use `for:` to debounce).

Rules:

- **Polarity is abnormal-is-`True`** (matching `CredentialUnavailable`, `RateLimited`) — `True` means there is a problem.
- **The tiers are mutually exclusive**: when the error fires, force the warning to `False` (reason `Superseded`).
  Each condition then maps to exactly one alert severity with a plain `== True` rule and no Alertmanager inhibition.
- **Advisory unless stated**: a capacity condition does not gate `Ready` unless the subject is actually unavailable — surfacing a latent problem must not flip a healthy workload to not-ready.
- Shipped examples: `ProxyQuotaPressure`/`ProxyQuotaExceeded` on the `ActionsGateway` (GMC) and `WorkerQuotaPressure`/`WorkerQuotaExceeded` on the `RunnerGroup` (AGC).
  See [Q82](../plan/archive/quota-pressure-conditions.md).

### Mirror alertable conditions as a controller-exported gauge

Every condition an operator should alert on is **also** exported as a Prometheus gauge by the owning controller (`1` when the condition is `True`, `0` otherwise), labelled by namespace + object name.
This lets clusters alert directly on the controller's `/metrics` endpoint **without depending on kube-state-metrics** to scrape CRD conditions.

Implement it as a **scrape-time collector** that lists the CRs from the cached reader and reads `.status.conditions` (see `proxyQuotaCollector` in `cmd/gmc` and `workerQuotaCollector` in `cmd/agc`), not a reconcile-path gauge: a deleted object simply stops being listed, so its series disappears with no stale-series cleanup and no reconcile cost.
Metric names mirror the condition (`actions_gateway_proxy_quota_pressure`, `actions_gateway_worker_quota_exceeded`, …).

## Kubernetes Events for lifecycle transitions

Controllers emit Kubernetes Events (via a controller-runtime `EventRecorder`) on the owning CR for incident-worthy lifecycle transitions, so operators see them in `kubectl describe` and event watchers — not only in metrics/conditions.
Conventions that keep them consistent and non-spammy:

- **`Reason` is PascalCase and stable** — it is a machine-matchable key operators filter on (`kubectl get events --field-selector reason=<X>`), so treat it like an API surface: don't rename it casually.
  Where an Event corresponds to a Prometheus counter, **mirror the metric name** in the `Reason` (e.g. the `actions_gateway_eviction_retries_exhausted_total` metric ↔ the `EvictionRetriesExhausted` Event) so the two correlate at a glance.
- **`Warning` vs `Normal` by severity** — `Warning` for a failure/abnormal terminal outcome, `Normal` for a benign transition.
- **Emit on transitions / terminal outcomes, never every reconcile** — an Event is an incident signal, not a heartbeat.
  Where a status condition already captures the steady state, the Event *complements* it (records the transition) rather than re-emitting on every requeue.
- **Record on the most useful object** — the owning CR an operator would `kubectl describe` (the reaper, and the Q170 job-lifecycle Events, record on the `RunnerGroup`/`RunnerSet`; the message names the affected pod/run/job).
- **Route deep-goroutine events back through the reconciler** — a listener or provisioner goroutine does not hold the live owner object the `EventRecorder` needs, so it pushes the event onto a buffered channel (non-blocking; drop on full) that the reconciler drains and records on the live object — mirroring the existing condition-update channel.
  The drain consumes each event once, so it is not re-emitted on later reconciles.
- Shipped examples: `WorkerPodStuckPending` (reaper), and the Q170 job-lifecycle set (`JobAcquisitionFailed`, `RunnerVersionTooOld`, `SessionUnauthorized`, `QuotaRetriesExhausted`, `EvictionRetriesExhausted`).
  The operator-facing catalogue lives in [troubleshooting.md](../operations/troubleshooting.md#job-lifecycle-events-on-a-runnergroup--runnerset).

## Graceful shutdown (SIGTERM)

Every binary we ship runs in a pod, so every one of them gets SIGTERM on each rollout, node drain, eviction, and scale-down.
Getting this wrong is quiet: the process exits cleanly, the rollout looks green, and the damage shows up elsewhere — as leaked GitHub-side sessions, as CI jobs whose network was cut mid-request, as jobs GitHub waits out rather than being told about.

Read this before writing or changing any shutdown path.

### The lifecycle facts the rules below follow from

- **Endpoint removal is concurrent with SIGTERM, not ordered before it.** Marking the pod terminating, removing it from EndpointSlices, and the kubelet starting its shutdown are independent control loops; none waits for the others.
  A pod can therefore receive SIGTERM while kube-proxy, an ingress, or a mesh sidecar is still routing new connections to it.
- **`preStop` runs before SIGTERM**, and the time it takes is **deducted from** `terminationGracePeriodSeconds` — it is not extra budget.
- **SIGTERM goes to PID 1 of each container only.** Child processes are not signalled.
  At grace expiry SIGKILL goes to every process in the cgroup.
- **The grace period is a deadline, not an allowance.** Anything still running when it expires is killed outright, mid-write.
- **And it is a *request*, not a guarantee.** The kubelet grants `min(terminationGracePeriodSeconds, what the node shutdown path allows)`.
  Voluntary drains honour it; involuntary ones truncate it hard — GKE gives ordinary pods **15s** on a Spot node however large the pod's ask.
  Per-platform defaults: [operations/node-shutdown-budgets.md](../operations/node-shutdown-budgets.md).

### Rules

**1.
A process must not exit while work it owns is unfinished.**

"Work it owns" is anything the outside world is still counting on: an open upstream session, an in-flight request, an unreported job result, a held lease.
Enumerate it explicitly for each binary — the failure mode is always something nobody thought to enumerate.

An acknowledgement the process has decided on but not yet sent counts, and it is easy to miss because in-process state says the work is finished.
The AGC's scale-set listener concludes a job in memory and deletes its queue message separately; exiting in between left GitHub still holding a message the listener had already acted on, and the next process provisioned a worker for it (Q603).
If a decision is only durable once a remote call lands, the exit path owes that call.

**2.
With controller-runtime, `mgr.Start` only waits for what the manager knows about.** Goroutines you spawn yourself from `Reconcile` are invisible to it: `mgr.Start` returns, `main` returns, and the process exits out from under them.
Register the drain as a `manager.Runnable` that blocks on the manager context and then waits for your goroutines, so it runs inside the manager's graceful shutdown:

```go
func (s *listenerShutdown) Start(ctx context.Context) error {
    <-ctx.Done()
    <-s.stop() // returns a done channel; blocks until every goroutine has exited
    return nil
}

// Run on every replica, not just the leader: a replica that has lost leadership
// still owns the goroutines it spawned.
func (s *listenerShutdown) NeedLeaderElection() bool { return false }
```

This is what Q222 got wrong — the AGC leaked a GitHub-side session per in-flight listener on every rollout.

**3.
Cleanup that runs *because* of cancellation must not run *on* the cancelled context.** A teardown `DELETE`/`PATCH`/report issued on the context that was just cancelled fails instantly, and "best-effort, error discarded" then means "never happened".
Use a context detached from the cancelled one, with its own bound:

```go
ctx, cancel := context.WithTimeout(context.Background(), cleanupBudget)
defer cancel()
```

Bound it, retry inside the bound (a teardown call is usually the *only* one that work will ever get), and log loudly with the resource's identity when you give up — a silent leak is unactionable.
Q222 shipped both halves of this.

**4.
`http.Server.Shutdown` does not wait for hijacked connections.** From the stdlib: *"Shutdown does not attempt to close nor wait for hijacked connections such as WebSockets.
The caller of Shutdown should separately notify such long-lived connections of shutdown and wait for them to close."* Any CONNECT tunnel, WebSocket, or upgraded stream needs its own tracking (a `WaitGroup` or a connection set, plus `Server.RegisterOnShutdown`) — `Shutdown` returning is not evidence they finished.

The egress proxy is the worked example (Q384).
`cmd/proxy` registers each tunnel with a tracker **before** hijacking the client connection — up to the hijack net/http still counts the connection as active and `Shutdown` waits for it, so registering first is what closes the window in which `Shutdown` could return between a hijack and its registration and see an empty tracker.
Shutdown then waits on the tracker *after* `Shutdown` returns, by which point the listener is closed and the tracked set can only shrink.

**5.
If your PID 1 supervises a child, forward the signal.** The child never gets SIGTERM on its own.
A wrapper that `exec.Command`s a real workload must catch SIGTERM, forward it, and wait — otherwise the child runs on until the cgroup SIGKILL with no chance to report its outcome.
Our worker wrapper does this in `terminationRelay` ([cmd/worker/main.go](../../cmd/worker/main.go)): it forwards SIGTERM/SIGINT to `Runner.Worker` (or `run.sh`), waits for it inside a bounded grace (`WORKER_SHUTDOWN_GRACE`, default 25s), and kills it with a logged reason if it overruns.
Propagate the child's exit code, including the 128+signal encoding for a signalled child — `os.ProcessState.ExitCode` reports -1 there, and `os.Exit(-1)` silently becomes 255.

**Register the handler *before* starting the child** — `signal.Notify` first, `cmd.Start` second, same register-before-the-window discipline as rule 4's hijack tracker.
A signal landing in between hits Go's default disposition, where PID 1 has no good outcome: the kernel drops a default-disposition SIGTERM sent to a namespace's init (`SIGNAL_UNKILLABLE`), so the pod's one termination notice is lost and you are back to the unreported-job failure this rule exists to prevent; off PID 1 (tests, local runs) the same signal kills the supervisor outright and strands the child.
The window is small but reachable — a job cancelled or a node drained seconds after the pod starts, or just a scheduler hiccup between the two statements (Q445).
Buffer the channel so a signal caught before the child exists is held and forwarded once it does, rather than dropped.

**6.
Anything serving traffic through a Service needs a `preStop` sleep.** Because of the concurrency in the first bullet above, SIGTERM is not a signal that traffic has stopped arriving.
A short `preStop` sleep (a few seconds, no process cooperation required) lets endpoint removal propagate before the process starts refusing work.
Size `terminationGracePeriodSeconds` as `preStop + drain budget + headroom`.

**But prefer an in-process linger when the process already has a drain.** A `preStop` sleep has two costs that are easy to miss:

- **It is serial with the drain**, so the grace period has to cover both.
  Yet the two waits are for *different* things and overlap freely — work in flight keeps finishing while you wait for endpoint removal.
- **Its cost is unconditional, and the budget is not always what the manifest says.** The kubelet grants a terminating pod `min(terminationGracePeriodSeconds, remaining node-shutdown window)`.
  On a truncated window — spot/preemptible reclamation, graceful node shutdown — a fixed sleep spends scarce budget idling and leaves the drain *less* time than it would have had with no `preStop` at all.
  That is a regression precisely where disruption is most frequent.

If the process handles SIGTERM already, keep the listener **open** for a bounded linger at the top of the drain instead, and spend it inside the existing drain budget.
Failing readiness first is a bonus a `preStop` sleep cannot match: the process does not know it is terminating until SIGTERM, so a sleep cannot start the endpoint-removal clock, only wait out someone else's.

Better still, **measure instead of sleeping**.
There is no Kubernetes signal for "endpoint removal has propagated" — no API, no readback across the N kube-proxies applying it — which is why the fixed sleep is the folk remedy.
But a server can observe *new connection arrivals* directly, and arrivals stopping is the property the sleep is approximating.
Wait until none has arrived for a short quiescence interval, measured from `max(shutdown start, last arrival)`: one rule that yields both a floor (an idle pod cannot exit instantly into the race) and an extending wait (each arrival is fresh evidence some dataplane has not converged).
Cap it, because a quiet interval is evidence and not proof.

The egress proxy is the worked example (Q386): `lingerForEndpointRemoval` in [cmd/proxy/proxy.go](../../cmd/proxy/proxy.go), ceiling `PROXY_SHUTDOWN_LINGER` (negative disables, for exactly the truncated-window case above), spent inside `PROXY_SHUTDOWN_DRAIN_TIMEOUT`.
Worst case is `max(linger, drain)` rather than their sum, so `terminationGracePeriodSeconds` did not have to grow to accommodate it.

**Use the native `sleep` handler if you do need `preStop`.** Our images are distroless: there is no shell and no `sleep` binary, so `exec: ["sleep", …]` fails at runtime and the pod proceeds straight to SIGTERM — reintroducing the race, but silently.
The native handler (KEP-3960) is beta and on by default from Kubernetes 1.30, the project's blocking install floor.

**7.
State the budget in the manifest comment, and keep the code inside it.** `terminationGracePeriodSeconds` is a claim about how long shutdown takes; if the code's drain is unbounded, or the comment describes a drain the code doesn't perform, the two silently diverge.
Prefer a bounded drain whose worst case you can name over `context.Background()` with no deadline.

### Review checklist

For any binary that runs in a pod:

- [ ] What does this process own that the outside world is waiting on?
  Is each item drained before exit?
- [ ] Does anything wait for the goroutines this process spawns, or does `main` just return?
- [ ] Does teardown run on a context detached from the cancelled one, bounded, and retried?
- [ ] Are hijacked/upgraded connections tracked separately from `http.Server.Shutdown`?
- [ ] If there is a child process, is SIGTERM forwarded to it — with the handler registered *before* the child is started?
- [ ] Does the pod serve traffic through a Service (⇒ needs a `preStop` sleep, or better, an in-process linger inside the existing drain budget)?
- [ ] Is `terminationGracePeriodSeconds` ≥ the worst-case shutdown (`preStop` + drain if serial; `max(linger, drain)` + tail if overlapped), and does its comment match what the code actually does?
- [ ] Would this still drain usefully on a node that grants only a few seconds (spot reclamation, graceful node shutdown), where the kubelet truncates the grace period?
- [ ] Is there a test that cancels the context and asserts the cleanup happened — not merely that the process exited?

That last one is the one that catches regressions.
A shutdown test asserting only "it exited" passes against every bug on this page.
