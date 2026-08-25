# Q726: multi-label runner sets for `runs-on` array targeting

Status: ✅ done, 2026-08-11.

A `v2beta1` RunnerSet is CEL-rejected above one `runnerLabel`, so a workflow that says `runs-on: [linux, gpu]` cannot be served by a ScaleSet runner set at all.
Migrating such a workflow needs one `.github/workflows` edit per target, which is the single thing breaking the zero-edit migration claim in [arc-parity.md](../arc-parity.md#definition-of-done).

## What was measured, and when

Measured 2026-08-11 against upstream sources, because the constraint this row removes was recorded in the repo as an upstream fact rather than as a GAG design choice.

**The scale-set model does express multiple labels, and has since ARC 0.14.0** (2026-03-19), added in [actions/actions-runner-controller#4408](https://github.com/actions/actions-runner-controller/pull/4408).
The wire shape is the one GAG already sends: the same `POST /_apis/runtime/runnerscalesets` call, with more than one entry in the existing `labels` array.
Every entry carries `Type: "System"`, and the first is the scale set's own name.
ARC builds the list as the scale-set name label followed by each extra label, skipping duplicates.

So [`scaleset/types.go`](../../../scaleset/types.go)'s claim that "there is no free-form multi-label list per scale set" was true when Q264 measured it and is false now.
Nothing in GAG's client needed to change: `RunnerScaleSet.Labels` is already `[]Label`.

**Matching is ordinary self-hosted-runner matching.** Upstream states that GitHub "matches jobs to your scale set based on the label and runner group policies, just like regular self-hosted runners".
The Actions Service owns that matching; GAG neither implements nor constrains it, so this change is a pass-through of the declared labels rather than a routing feature.

**GHES silently drops extra labels below 3.21.** Multi-label needs GHES 3.18 or later *and* the `DistributedTask.AllowRunnerScaleSetCustomLabels` feature flag.
The flag is off by default on 3.18–3.20 and on by default from 3.21.
With it off, the appliance uses the scale set's name as its only label and **discards the rest without an error**.
GAG serves GHES gateways, so this is a live failure mode here and not a footnote: the scale set registers, the listener runs, `Ready=True`, and every job targeting a dropped label queues forever with nothing anywhere saying why.

**Not measured: whether a labels `PATCH` is honoured.** ARC never patches an existing scale set.
When one already exists it is reused untouched, labels included.
GAG's [`UpdateRunnerScaleSet`](../../../scaleset/client.go) exists but no production code calls it, and its behaviour against a scale set with a live session and assigned jobs is unknown.
This plan therefore does not write labels to an existing scale set; see [Drift](#drift-detected-never-patched).

> **Measured since, and the answer is no** — [Q793](q793-labels-patch.md), 2026-08-24.
> The Actions Service accepts a labels `PATCH`, answers 200, and discards it; a live session is undisturbed.
> The two paragraphs above are what was known on 2026-08-11 and are kept as written.
> What they call a deliberate omission is now a constraint: this plan's approach was right, and no later release can reconcile labels in place.
> `UpdateRunnerScaleSet` did gain a production caller after this was written — Q712's runner-group reconcile, which patches `runnerGroupId` and is unaffected.

## Approach

### `runnerLabels[0]` is the scale-set name

The scale set keeps being named after the first declared label, and the whole list is registered as its labels.
A single-label set therefore produces a byte-identical create request to the one it produces today, so nothing already registered re-registers and no live set is disturbed.
It also matches ARC's own composition, which keeps the two systems' registered objects comparable.

The alternative, naming the scale set after the RunnerSet's Kubernetes object name as ARC's `runnerScaleSetName` does, renames every scale set already registered, which orphans them at GitHub and restarts every listener.
That is a migration, not a relaxation, and this row does not need it.

The cost is that `runnerLabels[0]` becomes load-bearing identity: reordering the list renames the scale set.
That hazard already exists today for the single label, unguarded, and it is now documented rather than newly introduced.

### Drift: detected, never patched

Reusing a scale set by name means a label added to an existing RunnerSet never reaches GitHub.
Because the `PATCH` path is unmeasured, this change does not attempt one.
([Q793](q793-labels-patch.md) has since measured it: a labels `PATCH` is discarded, so detection is not merely this plan's choice but the only option there is.)

Instead the listener compares the label set the server actually returned, on create *and* on reuse, against the set the RunnerSet declares, and reports the difference.
That single comparison covers both open failure modes at once, since a GHES appliance with the flag off and a scale set whose labels predate an edit are indistinguishable from the client's side and have the same remedy.

The report is a new advisory condition, `RunnerLabelsIncomplete`, plus a `Warning` event naming the missing labels.
It is advisory and stays out of `ImpairingConditionTypes`: the set is still serving every job that targets its name label, and rolling a configuration mismatch into the gateway's `RunnerSetsDegraded` summary would page for something that is not an outage.

Measuring `PATCH`, and reconciling labels in place if it is honoured, is follow-up work, filed separately.
It came back not honoured ([Q793](q793-labels-patch.md)), so nothing followed it.

### Uniqueness moves onto the name

The GMC webhook rejects two ScaleSet sets under one gateway that claim the same label, because the label *is* the scale-set name at GitHub.
That check now compares `runnerLabels[0]` rather than requiring exactly one label; for a single-label set the two are the same expression, so its behaviour is unchanged.

Labels after the first are deliberately **not** checked for overlap: `linux` on many sets is the ordinary case, and which set receives an ambiguous job is GitHub's decision, not an admission-time collision.

## Scope

In:

- Both single-label CEL rules dropped (`v2beta1` field-level, `v2alpha1` spec-level).
  Both must go together: converting a multi-label `v2beta1` set down to `v2alpha1` yields `acquisitionProtocol: ScaleSet`, so leaving the spec-level rule in place would make such a set unwritable through `v2alpha1`, which is the Q398 shape again.
- Every declared label registered on the scale set.
- Divergence between declared and registered labels surfaced as a condition and an event.
- Webhook uniqueness moved onto the scale-set name.
- The operator, design, and comparison docs that assert the scale-set model cannot express multi-label matching.

Out, filed to the Queue instead:

- Measuring whether the Actions Service honours a labels `PATCH`, and reconciling in place if it does.
  (Measured in [Q793](q793-labels-patch.md): it does not, so the reconcile half was withdrawn rather than built.)
- `gag-migrate` still writing `acquisitionProtocol: Classic` onto every set it emits.
  That is now conservative rather than necessary, but changing it silently flips a migrating tenant's protocol, which is a decision of its own.

## Verification

All of the below shipped with the change.

- CRD admission (envtest, AGC): a multi-label `v2beta1` create is admitted and every label round-trips through the storage version in order; a bare set (no `acquisitionProtocol`) with several labels admits and takes the `ScaleSet` default rather than being steered to `Classic`; the per-item rules survive the relaxation, so an empty list and a comma in a **non-first** label are both still rejected.
- Q398's regression guard outlives the rule that caused it: a Classic multi-label set is still editable through the hub on an unrelated field, and its labels are now editable there too.
- Listener: a multi-label create registers the name label first then each extra, duplicates dropped; a reused scale set reports its own labels rather than the ones now asked for; a stub answering 200 having kept only the name label, the GHES appliance shape, reports exactly that.
- The listener assertions were checked by deleting the mechanism: with `ExtraLabels` stubbed out of `desiredLabels`, the registration tests go red, and green again on restore.
- Reconciler: the condition fires on the rising edge only, names the missing labels and the GHES remedy, treats a *superset* of registered labels as no shortfall, and publishes nothing at all when there is no observation.
- Webhook (envtest, GMC): two sets sharing `runnerLabels[0]` are rejected; two sets sharing only later labels are both admitted; a label another set carries in a non-first position remains claimable as a name.

## What this deliberately did not do

`ExtraLabels` is a separate `Config` field rather than a full `RunnerLabels` list, so every existing `l.cfg.ScaleSetName` reference keeps one unambiguous meaning: the runner-record name prefix, the sweep, every log line.
The split mirrors ARC's own `runnerScaleSetName` plus `scaleSetLabels`.

No Prometheus gauge mirrors `RunnerLabelsIncomplete`.
The convention in [kubernetes-conventions.md](../../development/kubernetes-conventions.md#mirror-alertable-conditions-as-a-controller-exported-gauge) scopes the gauge to conditions an operator should *alert* on, and this one does not move on its own: it is a deploy-time configuration mismatch that stays true until someone acts, so it belongs in `kubectl describe` and the Event rather than on a pager.
The sibling advisory conditions (`SizingDrift`, `SizingProfileOverridden`) carry no gauge for the same reason.

The `RunnerLabelsIncomplete` condition is derived by the reconciler from a listener snapshot rather than published by the listener itself.
The listener ensures its scale set once at start and does not restart for a spec change, so a listener-published condition would never see a label appended to a live set, which is the more likely of the two divergences.
