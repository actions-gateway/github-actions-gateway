# Q793: is a scale-set labels `PATCH` honoured?

Status: ✅ done, 2026-08-24.
**Answer: no.** The reconcile half of the row is withdrawn on the measurement.

Q726 registers a scale set's labels at create and never writes them again, reporting divergence through the advisory `RunnerLabelsIncomplete` condition rather than correcting it.
It recorded the reason as an unknown rather than a decision: ARC never patches an existing scale set, GAG's `UpdateRunnerScaleSet` had no production caller at the time, and nothing anywhere had measured what the Actions Service does with a labels `PATCH`.
This row was filed to take that measurement, then reconcile in place if it came back positive.

It came back negative, so nothing is reconciled and the condition Q726 shipped remains the whole of the answer.

## What was measured, and when

Measured 2026-08-24 against `github.com` (`broker.actions.githubusercontent.com`), org `actions-gateway`, through the shipping `scaleset.Client`.
Two runs, two throwaway scale sets, identical verdicts.
The instrument is [Investigation I](../../development/testing.md#the-credential-gated-probe-scenarios), selector `PROBE_LABELPATCH_TEST=true`, which ships in this change.

**A labels `PATCH` is accepted and silently discarded.** `PATCH /_apis/runtime/runnerscalesets/{id}` carrying a three-entry `labels` array answers **200** and stores none of it.
The response body is not an echo of the request: it carries the **stored** label set, still two entries.
An independent `GET` agrees.

| Arm | Asked | Registered afterwards | Verdict |
|---|---|---|---|
| 0 create (control) | `[name, base]` | `[name, base]` | CONTROL-OK |
| 1 append | `[name, base, added]` | `[name, base]` | **NOT-HONOURED** |
| 2 shrink | not run | not run | INCONCLUSIVE, gated on arm 1 |
| 3 append under a live session | `[name, base, added]` | `[name, base]` | **NOT-HONOURED**, session survived |
| 4 name omitted | not run | not run | INCONCLUSIVE, gated on arm 1 |

Three things make that a usable negative rather than a bare failure.

**The create arm is a control, not a warm-up.** A GHES appliance below 3.21 without `DistributedTask.AllowRunnerScaleSetCustomLabels` drops every label past the name at *create* (Q726).
On such a backend every `PATCH` arm would report a shortfall that says nothing about `PATCH`.
Arm 0 registers `[name, base]` and reads it back before anything is patched, and the probe stops `INCONCLUSIVE` if the extra label is already gone.
It passed here, so the shortfall in arms 1 and 3 is attributable to `PATCH`.

**The verdict is read from an independent `GET`, never from the `PATCH` response.** A service echoing its input is byte-identical, from the response alone, to one that stored it.
`scalesetstub.LabelPatchEcho` models exactly that, and `TestLabelPatchEchoIsNotMistakenForHonoured` fails if the verdict is ever taken from the response.
That was verified by inverting the probe to read `patched.Labels`, which turns the test red with the assertion naming the cause.
As it happens `github.com` does not even echo, which is a stronger negative than the design anticipated: the response is visibly the stored set.

**A session is not disturbed.** Arm 3 holds a live session across the `PATCH` and polls the queue afterwards, and the queue still answers.
That closes the other half of Q726's unknown, since the `PATCH` is harmless as well as useless.
It is also why the `runnerGroupId` reconcile Q712 ships is unaffected by this result.

### What the measurement does not cover

The construction measured is the one the shipping client sends, which is the only construction a reconciler would use.
It does not rule out some other route or body shape reaching the label store, and no upstream documentation of one was found.
GHES is not covered at all.
An appliance is a different backend, and a positive there would not change GAG's behaviour anyway, because the code must serve both.

Re-run the probe against a future GitHub rather than trusting this verdict indefinitely, the same standing caveat Investigation F carries.

## What this changes

Nothing in the product's behaviour, and several claims about it.

Q726's docs describe labels as "not reconciled afterwards".
That is accurate, but it reads as a GAG design choice an operator might expect a later release to revisit.
It is now a measured property of the service, so the operator-facing text says so and stops implying a fix is pending.
The remedies were already correct and are unchanged: give the set a new scale set, by changing `runnerLabels[0]` or by deleting the scale set at GitHub.

`Client.UpdateRunnerScaleSet`'s godoc carried no warning at all, which is the sharpest gap.
It is the surface a future caller reaches for, and its name promises more than the service delivers.

## Scope

In:

- Investigation I in `cmd/probe`, with unit coverage over five backend behaviours (`honour`, `ignore`, `echo`, `additive`, `refuse`) and the create-arm control.
- `scalesetstub.LabelPatchMode`, so a test states which answer it models rather than inheriting one, plus the by-id scale-set `GET` returning labels like its by-name sibling.
- The measured claim replacing the unmeasured one in the client godoc, the condition message, and the two operator docs.

Out:

- **Reconciling labels in place.** The row's second half, withdrawn on the measurement rather than deferred: there is nothing to build against a service that ignores the field.
- Probing alternative `PATCH` constructions, or a GHES appliance, per the limits above.

## Verification

- The probe's own logic is covered against the stub in all five modes.
  The two that matter are `ignore` (a 200 that stores nothing must not read as success) and `echo` (a response agreeing with the request must not either).
- The independent-`GET` mechanism was verified by deletion: reading the verdict from `patched.Labels` instead turns `TestLabelPatchEchoIsNotMistakenForHonoured` red, and restoring it turns it green.
- The create-arm control was verified by making the stub drop extras at create *while* honouring `PATCH`, the mode combination that would otherwise produce the most convincing false green, and asserting no arm below runs.
- The live run was taken twice, the second against the shipped probe after the arm-gating fix below.

### The first live run reported two verdicts it could not support

Worth recording, because the defect is invisible in a green suite and the live run is what exposed it.

Arms 2 and 4 both ask a `PATCH` to change the label set and then read it.
Once arm 1 has established that a `PATCH` changes nothing, the set those arms read still equals what they asked for.
So the first run printed `arm 2 SHRINK HONOURED` and `arm 4 NAME-LABEL-PRESERVED`, both describing outcomes nothing caused, the second crediting the service with reinstating a label it had never removed.

They are now gated on arm 1 landing, exactly as every arm is gated on arm 0, and report `INCONCLUSIVE` otherwise.
Arm 3 is deliberately not gated: it re-measures the append under a live session, which is its own question, and its session half stands either way.
