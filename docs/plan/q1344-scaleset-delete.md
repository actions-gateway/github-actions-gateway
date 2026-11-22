# Q1344 — What deleting a scale set does to its jobs and runner records

**Status:** measured.
Investigation J ran both arms against github.com on 2026-11-22, and the readings are in [The readings](#the-readings-2026-11-22).
The delete is accepted and takes the runner record with it, but the queued job survives it and a scale set re-created under the same name receives that job, so a delete alone does not protect the queued jobs; only draining does.
Which fix follows is open in [Q1344](../queue/Q1344.md), laid out in [What the readings leave to decide](#what-the-readings-leave-to-decide).

## The question

A renamed or deleted `RunnerSet` leaves its scale set registered at GitHub, and the next tenant admitted onto that name adopts it.
Three facts, read from source at `680d328` and not re-measured here:

- No AGC path deletes a scale set: `DeleteRunnerScaleSet` is called only from the `scaleset` client's own tests and from `cmd/probe`.
- Admission's name inventory is built from the `RunnerSet`s that are stored now (`scalesetscope.Of`), so a rename or delete frees the name as soon as it is stored.
- `ensureScaleSet` adopts by exact name, keeps the old runner group when the new set declares none, and moves it when one is declared (`TestListener_MovesAdoptedScaleSetIntoDeclaredGroup` covers adopting by name across runs).

The candidate fix deletes the scale set when its listener stops, once its jobs have drained.
Whether that is safe, and whether it closes anything, turns on four things this repo has never measured:

1. Does the Actions Service accept the delete while a job is queued or assigned on the scale set, or while a runner record minted under it exists?
2. What happens to that job: concluded (and how), left queued, or requeued?
3. Does the runner record go with the scale set?
4. Does a scale set re-created under the same name receive the deleted one's still-queued job?

The fourth decides whether a delete closes the adoption at all.
A job targets a label, not a scale-set id.
If a same-name successor receives the job, deleting releases nothing the next tenant cannot take, and only draining protects the queued jobs.

## Investigation J — the measurement

`PROBE_SSDELETE_TEST=true` selects it ([`cmd/probe/ssdelete.go`](../../cmd/probe/ssdelete.go)).
Each run creates its own throwaway scale set, registered against the repository, because the org `Default` group refuses public repositories (the same constraint as Investigations E to H).
It refuses to start when a scale set already holds the name, since a reused one carries another run's queue and records.

| Arm | `PROBE_SSDELETE_ARM` | State at T0 |
|---|---|---|
| Queued | `queued` (default) | The fixture job is queued on the scale set, and no session exists: what a renamed set's stopped listener leaves. |
| Assigned | `assigned` | A session took the assignment, a JIT runner record was minted for it, and the session was deleted: a listener stopped between provisioning and the runner coming online. |

At T0 the probe deletes the scale set and reads it back by name (measurement 1).
It then reads the job, and the runner record on the assigned arm, every `PROBE_SSDELETE_INTERVAL` (30 s) for `PROBE_SSDELETE_WINDOW` (20 min) (measurements 2 and 3).
If the job has not concluded by then, it re-creates a scale set under the same name, opens a session as a different owner, and polls `PROBE_SSDELETE_TIMEOUT` (5 min) for an assignment from the fixture's run (measurement 4).
Cleanup cancels the fixture run, deregisters the runner record, and deletes every scale set the run created.

Verdicts are log lines: `DELETE-ACCEPTED` / `DELETE-REFUSED` / `DELETE-NOT-EFFECTIVE`, `JOB-CONCLUDED (<conclusion>)` / `JOB-NOT-CONCLUDED (<status>)`, `RUNNER-REMOVED` / `RUNNER-LEFT`, and `JOB-INHERITED` / `JOB-NOT-INHERITED`.
A refused or ineffective delete stops the run, because every later measurement is about a deleted scale set.

### Running it

The fixture is [`q1344-ssdelete-probe.yml`](../../.github/workflows/q1344-ssdelete-probe.yml).
Dispatch it first, then start the probe, once per arm.
The key reaches the probe as a file path, per [github-app-credentials.md](../development/github-app-credentials.md).

```bash
gh workflow run q1344-ssdelete-probe.yml --repo actions-gateway/github-actions-gateway
```

```bash
GITHUB_APP_ID=3752347 GITHUB_APP_INSTALLATION_ID=135739122 GITHUB_APP_PRIVATE_KEY="$KEY_FILE" GITHUB_ORG_URL=https://github.com/actions-gateway/github-actions-gateway PROBE_SSDELETE_TEST=true PROBE_SSDELETE_ARM=queued go run -C cmd/probe .
```

`workflow_dispatch` works only once the fixture is on the default branch; before that, a push to a `claude/` branch whose head commit message carries `[q1344-probe]` queues the job.

### What makes a result invalid

- **The job concluded before T0.** The `STAGED` line records its status; anything but `queued` (or, on the assigned arm, an assigned job not yet `completed`) means the delete was measured against a job that was already over.
- **The runner record was not listed before T0.** The assigned arm logs a warning, and a later `RUNNER-REMOVED` then says nothing about the delete.
- **Measurement 4 picks up a different run.** It matches on the fixture's run id and logs any other assignment it ignores; a second fixture dispatch inside the window would show up there.
- **A quiet window is not an absence until both records were read.** The job status and the runner listing are separate records, and each verdict names the one it read.

## The readings, 2026-11-22

Karl ran both arms against `actions-gateway/github-actions-gateway` on github.com; the verdict lines below are quoted from the probe's logs, which stay outside the repository.
Times are PST.

| Arm | T0 state (`STAGED`) | 1: delete | 2: job | 3: runner record | 4: same-name successor |
|---|---|---|---|---|---|
| Queued | 10:16:07.225, `scaleSetId=29`, `runId=45054082229`, `jobId=137305352512`, `jobStatus=queued`, no runner | `DELETE-ACCEPTED`: `DELETE` 204, gone by name at 10:16:07.603 | `JOB-NOT-CONCLUDED (queued)`, history `queued/` over the 20 min window | none minted on this arm | `JOB-INHERITED` at 10:36:09.999: re-created `scaleSetId=30` received a `JobAssigned` for `runId=45054082229` (new job id `8ddb9b6c-878c-5ecd-9de6-0254de88a393`) |
| Assigned | 10:39:10.261, `scaleSetId=31`, `runId=45056284852`, `jobId=137312136885`, `jobStatus=queued`, runner `gag-q1344-ssdelete-84dc72ee` | `DELETE-ACCEPTED` at 10:39:10.625 | `JOB-NOT-CONCLUDED (queued)` | `RUNNER-REMOVED`: no longer listed 1 s after T0 | `JOB-INHERITED` at 10:59:13.347: re-created `scaleSetId=32` received `runId=45056284852` (new job id `f340abfc-1876-58f4-8210-03fc6aabe8c4`) |

Both runs are valid by [the rules above](#what-makes-a-result-invalid): each `STAGED` line read `queued` at T0, neither log has a `WARN` line other than the `JOB-INHERITED` verdict, and measurement 4 matched the fixture's own run id.
Cleanup deleted scale sets 30 and 32, and both fixture runs read `completed`/`cancelled` afterwards.
The logs were checked for key material and hold none.

Three things follow from the readings rather than from source:

- **GitHub keeps a job queued against the name, not the scale-set id.** Each successor had a new id and a different owner, and was offered the deleted set's job about 20 minutes after T0, once the window ended and the probe re-created the set.
- **A delete does not conclude or cancel the job.** It stayed `queued` for the whole window on both arms.
- **The runner record goes with the scale set.** So a delete would retire the records leak a rename leaves, which deregistration in the AGC would otherwise have to do.

## What each answer decides

- **Delete refused while a job or record exists:** an AGC deleting on stop has to drain first and retry, so the fix needs a drain signal it does not have today.
- **Delete accepted, job concluded, record removed, and `JOB-NOT-INHERITED`:** drain-then-delete is sound, and the delete alone stops a successor taking queued jobs.
- **`JOB-INHERITED`:** a delete does not protect queued jobs from a same-name successor; only draining does, and the adoption's remaining delta is the inherited runner group, which declaring `runnerGroup` closes (Q712).
- **`RUNNER-LEFT`:** a delete leaves the records leak in place, and the AGC has to deregister them before deleting.

The readings are the `JOB-INHERITED` and `RUNNER-REMOVED` rows.

## What the readings leave to decide

A delete alone closes only the group half: the successor creates a fresh scale set, so it no longer inherits the first tenant's runner group, but it still receives the queued jobs.
Two directions remain, and they differ in what an operator does and what the AGC has to carry:

- **Drain, then delete, in the AGC.** On a rename or a `RunnerSet` delete, the old name's listener keeps serving until no job is queued on it, then deletes the scale set, which also removes its runner records.
  For that to protect the queue, admission has to keep the old name claimed until the delete lands, because today it releases the name the moment the change is stored and a successor could claim it mid-drain.
  It needs a drain signal the listener does not have, a claim on a name no stored `RunnerSet` holds, and a listener that outlives its spec (a finalizer on delete).
  Jobs queued after the delete still go to whoever claims the label next, as any job targeting a label does.
- **Keep the residual and close the group half by declaration.** The operator step stays as it is, drain and then delete, now with the measured reason, and every set binds a runner group through `runnerGroup` or `defaultRunnerGroup` (Q712, shipped), so an adopted scale set is moved into the claiming tenant's group.
  No code changes; the queued-job half stays an operator step.
