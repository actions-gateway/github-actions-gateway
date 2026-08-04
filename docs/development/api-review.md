# Agent reference: API design and pre-release review

An API field costs almost nothing to change while it is a pull request, a rename
while it is unreleased, and a conversion shim plus a deprecation window once a
tag publishes it. After that it is load-bearing for the life of the version.
This page is how we spend the cheap window on purpose: the rules to apply when
designing a field, and the review that runs before a tag closes the window on
everything added since the last one.

## Table of Contents

- [When to read what](#when-to-read-what)
- [The cost curve](#the-cost-curve)
- [What counts as API](#what-counts-as-api)
- [Design rules](#design-rules)
- [Is this change breaking?](#is-this-change-breaking)
- [The pre-release review](#the-pre-release-review)
- [Further reading](#further-reading)

## When to read what

| You are… | Read |
|---|---|
| Adding or reshaping a CRD field, enum, condition, or operator-set label | [Design rules](#design-rules) |
| Reviewing a PR that touches `api/` or `cmd/*/api/` | [Design rules](#design-rules) + [Is this change breaking?](#is-this-change-breaking) |
| Changing something that already shipped | [Is this change breaking?](#is-this-change-breaking) |
| In [release pre-flight](../operations/release.md#1-pre-flight), before tagging | [The pre-release review](#the-pre-release-review) |

The pre-release review is the backstop, not the design step. Everything it
catches was cheaper to get right in the PR that introduced it.

### Why the backstop exists

Q476 renamed `capacityGate.mode: On` to `Observe` days before 1.3.0 would have
published it. Nothing surfaced that question: it came up in an unrelated
conversation, and the value was one commit from being frozen for the life of
`v2beta1`. The review turns that from luck into a step.

The rules below are not generic API advice. Each one is a mistake this project
actually made, or a convention that upstream Kubernetes will hold us to whether
or not we noticed it.

## The cost curve

| When you change it | What it costs |
|---|---|
| Before merge | An edit. |
| Merged, before the tag | A rename plus regenerated manifests. This is the window the pre-release review protects. |
| After the tag, same version | Nothing legitimate: [an API element is removable only by incrementing the version](#is-this-change-breaking). You get a deprecated alias and a removal floor, not a fix. |
| In a new version | A conversion shim, a lossless carrier for anything dropped, round-trip tests, a deprecation warning, and a support window measured in releases. |
| Condition types, reasons, derived names | No conversion path at any stage — they carry no schema version. See [Status reports, it does not control](#status-reports-it-does-not-control). |

## What counts as API

| In scope | Why |
|---|---|
| CRD spec/status fields, their types, defaults, and validation | The wire contract |
| Enum values | Removing one is breaking; adding one is nearly always safe here, [with a caveat](#is-this-change-breaking) |
| Condition types and reasons ([`api/apiconditions`](../../api/apiconditions/conditions.go)) | Operators alert on them, and they carry no version |
| Label and annotation keys the operator sets or reads | Selectors and admission policies depend on them |
| Names the controllers derive from a CR's name ([`api/apinames`](../../api/apinames/names.go)) | They are pod, Secret, and label values on running clusters |
| Printer columns, short names, categories | `kubectl` muscle memory |
| Admission rejections and deprecation warnings an operator sees | The observable contract of an otherwise unchanged field |

Out of scope here: metric names (see
[observability-metrics.md](../operations/observability-metrics.md)) and Go
symbols that do not appear on the wire — a bare `string` field promoted to a
named type serialises identically, so it is a Go-API break for `api` module
consumers only and does not need to beat a tag.

## Design rules

### One field answers one question

An enum carrying two axes makes their cross-product unrepresentable and asks one
party to assert something they may not know. The tells are mechanical:

- Values that each activate a *different* sibling field.
- Cross-field CEL rules shaped `field X is only meaningful when mode == Y`.

Q470 split `capacityGate.mode` for exactly this reason.

When the axes genuinely belong together, the Kubernetes shape for it is a
discriminated union: a required discriminator field plus optional, pointer union
members — not one flat enum whose values imply which sibling to read.

**A "yes" to the tells is not automatically a split.** The same tell is visible
in [`WorkerSizing`](../../api/v2beta1/runnerset_types.go), whose three
`XValidation` rules each tie a sibling field to one `profile` value. Q481 asked
the question of `sizing.profile` before 1.3.0 and shipped it bundled anyway.
Three follow-ups decide it, in ascending order of how much they settle:

1. *Whose facts are the axes?* Q470's split was forced because one axis was the
   platform operator's and the other the tenant's — [whose fact is
   it](#ask-whose-fact-it-is) is the argument that made it worth a break. When
   both axes belong to the same party, that argument is simply unavailable.
2. *Are the axes actually orthogonal?* If one axis's parameter is defined in
   terms of the other's mechanism, splitting **relocates** the `only meaningful
   when` rule rather than removing it, and the cross-product gains undefined
   cells rather than useful ones. `Throughput`'s headroom multiplies an
   *observed peak*, which exists only under the usage source.
3. *Is the missing cell reachable additively?* This is the one that decides
   whether the question must beat a tag. Q470's fix **removed** enum values, so
   it had to land first; a gap a later minor can fill with one defaulted field
   does not. Check whether it is reachable *at all* first — Q481's headline gap,
   a Guaranteed node share, turned out to be reachable already, by a side effect
   of an unrelated guard.

When the answer is "ship bundled", record what the enum bundles *on purpose*.
`sizing.profile` is an **intent** enum — every value names what the operator
wants, and the mechanism follows — which is a legitimate shape and, more
usefully, hands the next reviewer the rule for extending it: new values name a
distinct intent, mechanism recombinations go in a sibling field. Worked example:
[appendix-h §H.7](../design/appendix-h-v2-api-decomposition.md#h7-reference-integrity--runtime-conditions-not-admission).

### Name the method, not the on-state

`On` is fine as the second of two values and stops carrying information the
moment a third arrives. Name the *method*, so the axis stays legible as it
grows — that is why the capacity gate's modes are `Off`/`Observe` with
`Probe`/`Provision` reserved, not `On` plus two strangers (Q476).

### Prefer a string enum to a bool

Upstream puts it as "think twice about `bool` fields": most ideas that start
binary trend toward a small set of mutually exclusive options, and a `bool` that
grows a third state has nowhere to go. A `bool` is defensible when the axis is
genuinely two-valued and cannot grow — `tracing.insecure` is either TLS or it is
not. Anything describing a *policy* or a *method* should be a string enum from
birth.

The same rule is sharper for label and annotation values, where a bare `true` is
also a YAML coercion footgun: use an enum keyword (`allowed`, `managed`). See
[kubernetes-conventions.md](kubernetes-conventions.md#label--annotation-value-conventions).

### Don't collide with an established meaning

`observe`/`audit`/`dry-run` mean *report-only, do not enforce* in Pod Security
Admission and Gatekeeper. Our `Observe` gates. Either avoid the collision or
neutralise it everywhere an operator will meet it — the field godoc, the
generated CRD description, the operator docs, and the admission message. Never
leave it to inference.

### Ask whose fact it is

Three separate questions, and a field can fail any one of them:

1. **Who knows the answer?** A tenant should not be asked to assert a property of
   infrastructure they do not own. When the correct value is identical for every
   object in the cluster and known to whoever runs the nodes, it belongs on the
   platform-owned object — `clusterCapacity.nodeAutoscaling` on `ActionsGateway`,
   not on each `RunnerSet` (Q470).
2. **Who is allowed to decide?** A cap a tenant can raise is not a cap. Q130
   removed `spec.namespaceQuota` outright and made the namespace `ResourceQuota`
   platform-owned; the tenant-facing fields that survived (`maxQuotaRetries`,
   `quotaRetryDelay`) are about *operating within* a quota, not owning one.
3. **What happens if the tenant lies?** Sometimes the tenant may legitimately
   *name* a thing while the platform decides which names are legal. `priorityTiers[].priorityClassName`
   is tenant-authored but checked against `--allowed-priority-classes` by the GMC
   webhook, so a tenant cannot name a preempting class and evict other tenants
   (Q132). Privileged eligibility goes further: it lives on a namespace label the
   tenant cannot set at all.

### Make the default fail in the safe direction

Not "is the default common" — *which way does a wrong answer fail?*

- `clusterCapacity.nodeAutoscaling` defaults to `Present` because that direction
  can only under-gate (today's behaviour), while `Absent` can starve a tenant.
- Q109 is the security version: an empty agent `keyType` resolved to Ed25519,
  which cannot decrypt the broker's RSA-OAEP session key, silently dropping a
  defence-in-depth layer. Empty and unrecognised now both resolve to RSA-3072,
  and Ed25519 is an explicit opt-in. An unset value must land on the safe
  option, never the convenient one; a less secure option may be offered as an
  explicit opt-in, never as the default. See the
  [security design](../design/05-security.md).

The zero value is a default whether or not you chose it. A non-pointer `string`
with no `+kubebuilder:default` means `""` reaches your code; decide what `""`
means before someone else's `switch` decides for you.

### Let the opt-in's direction follow what already ships

Opt-in or opt-out is decided by the behaviour that is already published, not by
taste. A switch that turns *off* behaviour operators already have must default to
keeping it (`EgressProxy.spec.managedAutoscaling` and `managedNetworkPolicy`,
both `*bool` defaulting `true`); a switch that turns *on* new behaviour must
default to off (`ActionsGateway.spec.agcAutoscaling` unset ⇒ no
`VerticalPodAutoscaler`). Two features on one axis therefore look asymmetric when
one predates the other, and that is the correct outcome — forcing them to match
either deletes something on upgrade or turns something on that nobody asked for
(Q486).

The *container* answers a second question: does the opt-in carry knobs of its
own? A pure ownership toggle whose knobs already exist as siblings is a `*bool`;
an opt-in with settings meaningful only while it is on is a block whose presence
**is** the switch — never a block with an `enabled` field, which is two fields
answering one question. Match the neighbours on the same object before matching a
different CRD: an operator meets the field in the object it lives in. Worked
example, including why the `*bool` survives
[prefer a string enum](#prefer-a-string-enum-to-a-bool):
§ E of [release-1.3.md](../plan/release-1.3.md).

### Decide optional, required, pointer, and default together

They are one decision with four outputs, and getting a corner wrong is silent.

| Situation | Shape |
|---|---|
| Required | No `omitempty`, no `+optional` — e.g. `RunnerLabels []string \`json:"runnerLabels"\`` |
| Optional, static default, zero value is not a meaningful choice | `+optional` + `+kubebuilder:default=…`, non-pointer is fine |
| Optional, unset differs from the zero value | Pointer + `omitempty` — `MaxWorkers *int32` (unset means "no cap"; `0` is rejected) |
| Optional, and we may want to change the default later | Default in the controller, not the CRD |

Two things that bite:

- **`omitempty` decides optionality, not `+optional`.** controller-gen reads the
  JSON tag. `RunnerGroupSpec.MaxListeners` has `omitempty` and no `+optional`,
  and lands optional; `RunnerGroupStatus.ActiveSessions` has neither, and lands
  in the generated CRD's `required` list. That one is harmless — the controller
  always writes status — but the same slip on a spec field rejects every
  existing manifest.
- **A CRD default is written into stored objects.** It applies at admission, so
  changing it later does not change existing objects — you get two populations
  and a behaviour change for new ones. That is why a default you are unsure about
  belongs in the controller, where it stays changeable.

### Choose the list semantics deliberately

A list's `x-kubernetes-list-type` decides who can own it under server-side apply
and whether validation ratcheting can see individual items. The default is
`atomic`: one applier replaces the whole list, and no item within it can be
ratcheted.

- Conditions are `+listType=map +listMapKey=type` — independently owned entries
  with a natural key. Every status conditions slice in this repo uses that.
- `priorityTiers` is atomic, and its "strictly ascending threshold" ordering is a
  *caller contract* that neither admission nor a status condition enforces — an
  out-of-order tier is simply unreachable. If an ordering or uniqueness rule is
  load-bearing, either validate it or say plainly in the godoc that it is not
  validated. It is documented in
  [`runnergroup_types.go`](../../cmd/agc/api/v1alpha1/runnergroup_types.go).

### Put a constraint on the narrowest field that expresses it

CRD validation ratcheting (KEP-4008, default-on from Kubernetes 1.30) suppresses
a rule only while *the value it sits on* is unchanged. A rule on the spec
therefore re-fires on every write to any field.

Q398 is the worked example: a `RunnerSetSpec`-level rule requiring exactly one
`runnerLabel` re-evaluated on every unqualified `kubectl edit`, so editing an
unrelated field on a stored-but-noncompliant object failed on labels. Moving the
rule onto `spec.runnerLabels` kept the constraint exactly where it matters and
let ratcheting suppress it everywhere else.

Related: pick the right enforcement point at all. CEL `XValidation` is cheap and
always on; a webhook can read `metadata` and name the offending component in the
message (which is why `gitHubURL` structure and `securityProfile` downgrades are
webhook-enforced). The CEL marker gotchas — gofmt corrupting `''`, and
`selectableFields` on one version only — are in
[code-generation.md](code-generation.md#crd-marker-and-api-file-gotchas).

### Derived names are part of the contract

Names the controllers build from a CR's name are pod names, Secret names, and
label values on running clusters. Changing how one is derived orphans live
objects, which is worse than a field rename because nothing warns.

- Split the budget **before** joining, not after (the lesson of Q467): the
  sanitizers were never the bug, composition was. Use
  [`api/apinames`](../../api/apinames/names.go) rather than a fifth copy of
  "sanitize, cap, append a hash".
- Include the owner's **kind**, not just its name (Q466). A v1 `RunnerGroup` and a
  v2 `RunnerSet` of the same name share a namespace for the whole coexistence
  window — that is what makes rollback possible — so a name derived from the name
  alone has two controllers managing one object.
- Respect the budgets: 253 characters for a DNS subdomain name, 63 for a label
  value, and less once a suffix is appended.

### Status reports, it does not control

- Status is written by the controller and ignored on create/update. Never make a
  spec decision depend on a value only status carries.
- Condition types and reasons are runtime strings, not schema, so they carry **no
  version and have no conversion path**. Renaming one breaks every operator alert
  at once, in every served version simultaneously. Declare them in
  [`api/apiconditions`](../../api/apiconditions/conditions.go) and re-export from
  both version packages; `make v2-api-sync-check` fails a one-sided add.
- Keep the polarity convention: normal-is-True (`Ready`), with abnormal-is-True
  conditions named so they read that way (`CredentialUnavailable`).
- Carry `observedGeneration` so a reader can tell a stale status from a current
  one.

### Design for the values that have not arrived yet

Check the planned-but-unbuilt work the field already anticipates — the Queue and
[appendix H](../design/appendix-h-v2-api-decomposition.md). If a deferred item
extends the same axis, the shape has to accommodate it now. Enum values that are
*not yet accepted* are still a design constraint: `Probe`/`Provision` are the
reason `Observe` is named the way it is.

### If the chart renders an instance of the CRD, omit the new field when empty

Adding an optional field is additive to the *schema* and still breaks
`helm upgrade` when the chart also renders an object of that kind. Helm applies
the chart-root `crds/` directory on `helm install` **only**, never on upgrade, so
an upgrading cluster still holds the CRD its *current* release installed. A field
that CRD does not declare fails server-side apply midway through the upgrade:

```text
failed to create typed patch object: .spec.<newField>: field not declared in schema
```

Midway is the damage — the release revision has already advanced, leaving a
half-upgraded cluster. `make check` cannot see this; the signal comes from
[`chart-released-upgrade-check.sh`](../../scripts/e2e/chart-released-upgrade-check.sh),
which runs in e2e.

So render the key only when the value is non-empty:

```gotemplate
{{- with .Values.myNewField }}
myNewField:
  {{- toYaml . | nindent 4 }}
{{- end }}
```

Unset and empty mean the same thing to both the CRD and the controller, but only
the omitted form survives an upgrade against the older CRD — and empty is the
default, so that is the path every existing release takes. When the value *is*
set, the operator is adopting a new feature and the chart's documented
`helm show crds … | kubectl apply -f -` step applies; preflight it with `lookup`
and fail at **render** rather than midway.

Q298 shipped this break and the gate caught it. The trap is that the chart's
older preflight tests whether the *kind* is present, which reads as "the hazard
is a new CRD" — it is equally a new **field** on an existing one, and a stale
schema is invisible to a presence check. Today this applies to
`PriorityClassAllowlist`, the one CRD the chart both installs and instantiates.

## Is this change breaking?

"Breaking" here means the wire contract, which is what a tag freezes. Go-symbol
breaks affect `api` module consumers only and do not need to beat a tag.

### Ask it in this order, at PR review

Two questions come before the table, and both are cheap only while the PR is
open. Asking them at release pre-flight is already late: by then the answer costs
a rebase of merged work instead of an edit to a draft.

**1. Has this element shipped in a stable tag?** Everything below assumes it has
— that premise is what turns a rename into a conversion shim plus a deprecation
window. It is false more often than the `!` markers suggest. All three
`!`-marked commits between `v1.2.0` and 2026-07-31 changed surface no stable tag
had ever published: `windowStart` and `capacityGate` are both absent from every
API tree at `v1.2.0`, so renaming and reshaping them broke nothing. Each sat at
the *merged, before the tag* row of [the cost curve](#the-cost-curve) — an edit.

```bash
git grep -l '<field>' "$(git tag --list 'v*' --sort=-v:refname | grep -v -- '-' | head -1)" -- api cmd/agc/api cmd/gmc/api
```

No hit means no cluster can be holding it and no manifest can name it: not
breaking. `scripts/release/api-surface-since.sh <tag>` lists everything a tag cut now
would publish for the first time, which is the same question asked over the whole
surface at once. If you still mark the commit `!` for a Go-symbol break, say so
in the body — nothing downstream can tell the two kinds of break apart, and only
the wire kind bears on the tag.

**2. If it did ship, does the change have to break?** A breaking shape is
sometimes the *first* shape reached rather than the one required. Adding a field
beside the old one, widening a validation instead of tightening it, or accepting
both spellings for a version reaches the same end state without the shim. Ask
before the PR lands, while the alternative is still a design choice; afterwards
it is a migration.

| Change | Wire-breaking? | Notes |
|---|---|---|
| Add an optional field with a safe default | No | The ordinary case. |
| Add a required field | **Yes** | Existing manifests stop applying. New version only. |
| Add an enum value | Practically no | Upstream calls it incompatible because a client switching exhaustively can break. Safe here when the value is opt-in and the default is unchanged; say so in the review rather than assuming it. |
| Remove or rename an enum value | **Yes** | Only by incrementing the version. See the removal floor below. |
| Tighten validation | **Yes** | Stored objects stop re-applying. Ratcheting narrows the blast radius; it does not remove it (Q398). |
| Relax validation | No for clients | But a newly storable value may have no representation in an older served version — check the conversion. |
| Change a default | **Yes** | Existing objects keep the old value, new ones get the new one. Two populations, silently. |
| Rename a field | **Yes** | Conversion shim plus a deprecation window. |
| Promote a bare `string` to a named type | No | Wire-identical. Go-API break only. |
| Rename a condition type or reason | Not schema | Breaks operator alerts, in every version at once, with no conversion path. |
| Change how a name is derived | Not schema | Orphans live objects. Needs a migration. |

### Removal needs a version increment, not a promise

An API element is removable only by incrementing the version — never by deleting
it from a version still being served. Q428 is where that bit: the deprecated
`CiliumFQDN`/`CalicoFQDN` aliases promised removal "in a future release, on the
v1alpha1 deprecation clock", but they are enum members of `v2beta1`, and v2.0.0
deliberately keeps serving `v2beta1`. The aliases live exactly as long as
`v2beta1` does, which puts the earliest possible removal at `v3.0.0`. The fix was
to state that floor identically in the enum godoc for both v2 versions, the
generated CRD descriptions, the admission warning, and the operator docs.

Name a floor ("no earlier than vX"), not a schedule, and never promise a removal
the version lifecycle cannot deliver.

### A new version must round-trip losslessly

`v2alpha1` is a spoke and `v2beta1` is the hub/storage version, so every served
version converts to and from one hub rather than pairwise. If a new version drops
a field the old one has, the object must still round-trip: `RunnerSet` conversion
stashes the dropped `acquisitionProtocol` and `maxListeners` in
`conversion.actions-gateway.com/*` annotations and restores them on the way back,
so a coexistence-era object is never silently re-protocol'd. Any such carrier
needs a round-trip test — see `TestRunnerSetConversion_RoundTrip` and friends in
[`api/v2alpha1/conversion_test.go`](../../api/v2alpha1/conversion_test.go).

Two supporting habits: mark the outgoing version with
`+kubebuilder:deprecatedversion:warning=…` naming the replacement *and* the
release that removes it, so the apiserver warns at apply time; and keep the two
version packages byte-identical except for the entitled differences, which
`make v2-api-sync-check` enforces.

## The pre-release review

Run this in [release pre-flight](../operations/release.md#1-pre-flight), before
tagging any release — including a prerelease that will become a stable line.

### Step 1 — enumerate what is new

```bash
scripts/release/api-surface-since.sh
```

It diffs the API packages and CRD manifests between the last tag and `HEAD` and
prints what changed. Everything it lists is surface this release publishes for
the first time; everything it does not list has already shipped and is governed
by the compatibility rules instead.

Pass an explicit ref to review a different span, e.g. `scripts/release/api-surface-since.sh v1.1.0`.

It reports rather than passing or failing on purpose: every question below needs
a human, and a gate answering them mechanically would be wrong in both
directions.

### Step 2 — ask these of each addition

| Ask | The tell | Rule |
|---|---|---|
| Does this answer exactly one question? | Sibling fields gated by one value; CEL shaped "X only when mode == Y" | [One field answers one question](#one-field-answers-one-question) |
| Is the value named for its method, or merely that it is on? | An `On`/`Enabled` value on an axis that can grow | [Name the method](#name-the-method-not-the-on-state) |
| Should this be an enum rather than a bool? | A `*bool` describing a policy | [Prefer a string enum](#prefer-a-string-enum-to-a-bool) |
| Does the name collide with an established convention? | The word already means something in PSA, Gatekeeper, or core Kubernetes | [Don't collide](#dont-collide-with-an-established-meaning) |
| Whose fact is this? | A tenant asserting something about the cluster or raising their own cap | [Ask whose fact it is](#ask-whose-fact-it-is) |
| Does the default fail safe? | The wrong answer costs isolation, capacity, or a security layer | [Fail in the safe direction](#make-the-default-fail-in-the-safe-direction) |
| Is it an opt-in or an opt-out, and does the shape match its neighbours? | A new switch defaulting against today's behaviour; a block with an `enabled` field | [Direction follows what ships](#let-the-opt-ins-direction-follow-what-already-ships) |
| Are optional/required/pointer/default consistent? | `+optional` without `omitempty`; a CRD default we may want to move | [Decide them together](#decide-optional-required-pointer-and-default-together) |
| Is the list type right? | A new list with no `listType` marker | [List semantics](#choose-the-list-semantics-deliberately) |
| Is each rule on the narrowest field? | A spec-level `XValidation` naming one field | [Narrowest field](#put-a-constraint-on-the-narrowest-field-that-expresses-it) |
| Will this shape hold when the reserved values arrive? | A deferred Queue item extending the same axis | [Design for values not yet arrived](#design-for-the-values-that-have-not-arrived-yet) |
| Is it wire-breaking or only Go-breaking? | A type change with identical serialisation | [Is this change breaking?](#is-this-change-breaking) |

That last question is what keeps the urgent pile small: only wire-breaking
changes must beat the tag.

### Step 3 — record the outcome

Write the verdict into the release's plan doc under `docs/plan/` (its Definition
of Done section), naming what was reviewed and what was decided. Record all three
buckets — reviewed, found-and-fixed, and accepted-without-change — so the next
release can tell a deliberate choice from an unexamined one. §E of
[release-1.3.md](../plan/release-1.3.md) is the worked example.

**"Ship as-is, deliberately" is a valid outcome** and the most common one. The
point is that the choice is made rather than defaulted into.

Anything deferred gets a Queue row with the release's gate label (`1.3-gate` and
friends) so it cannot be frozen by the tag while nobody is looking. Allocate the
ID with `make queue-id TITLE="…"`; format per
[maintaining-backlog.md](maintaining-backlog.md).

### Scope discipline

This reviews *newly published* surface, not the whole API. Older fields have had
real soak and operator contact that this release's additions have not.
Re-litigating them here makes the gate expensive, and an expensive gate gets
skipped.

## Further reading

Upstream sources these rules follow, worth reading directly when a case is not
covered above:

- [Kubernetes API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md)
  — naming, units, optional/required, conditions, unions, object references.
- [Changing the API](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api_changes.md)
  — what is and is not a compatible change, and the alpha/beta/GA bar.
- [Kubernetes deprecation policy](https://kubernetes.io/docs/reference/using-api/deprecation-policy/)
  — support windows and the rule that removal requires a version increment.
- [KEP-4008: CRD validation ratcheting](https://github.com/kubernetes/enhancements/tree/master/keps/sig-api-machinery/4008-crd-ratcheting)
  — what ratcheting does and does not suppress.
- [OpenShift API conventions](https://github.com/openshift/enhancements/blob/master/dev-guide/api-conventions.md)
  — a stricter, CRD-specific take on pointers, unions, and where to default.
- [Kubebuilder CRD processing markers](https://book.kubebuilder.io/reference/markers/crd-processing.html)
  — the marker reference behind the generated schema.
