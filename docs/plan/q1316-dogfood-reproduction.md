# Q1316: the dogfood reproduction, planned

> **Status: Complete**, with what it leaves deferred as [Q1316](../queue/Q1316.md), whose trigger is a platform cause that kills the sidecar with its runner alive on record.
> **Run twice, on 2026-11-24 and overnight into 2026-11-25.** The first read R0, R1, R6 and R8 and stopped at R2 by the abort for a node not ready ([the first run](#the-first-run)); the second, which [drops](#what-the-second-run-changes) R2 and R5, read R0, R3, R3t and R7 ([the second run](#the-second-run)), and R4's condition was not met.
> [What it settles](#what-the-second-run-settles) is below.
> Local readings it builds on are in [the design](q555-automatic-recovery-design.md#readings-for-the-dockerd-sidecar-class-q1316).

## What it decides

Q1316's retry class needs a fact that separates a platform's crash of the Docker-in-Docker sidecar from the job's own, and a platform cause that leaves the runner alive to fail the job.
From the kernel's scoring, at the shipped requests the runner is expected to go first and the class to have no memory case (computed below); the run confirms or refutes that for a kill at `kubepods`' limit, and a node-wide kill's order stays computed.
The local readings leave five questions only the dogfood cluster answers:

1. When pods fill a node to `kubepods`' limit, does the kernel kill the sidecar, or the runner first?
2. Where a neighbour's fill kills the sidecar, does `kubepods`' own counter rise, as no kill by the job in R1, R6 and R8 raised it?
   A fill by the job is from source.
3. Do the API server's statuses match the kubelet's, under `privileged-dind` and under Kata?
4. Does the out-of-memory class read a runner killed at `kubepods`' limit as an own-limit kill (Q1400)?
5. Can an unprivileged reader on the node say whether the container's limit or `kubepods`' ran out, before the cgroups that say so are removed ([Q1403](../queue/Q1403.md), the node reporter)?
   The pod's limit is read locally, and the node's is from source.

One platform cause is already out: locally, restarting containerd or the kubelet and killing containerd left the sidecar and the runner running, and killing the pod's shim ended both.
Those readings are one run each, and nothing here repeats them.

## No privileged debug pod

Every node fact this needs is readable from inside a `privileged-dind` job: its daemon sees the host's cgroup tree, so `docker run --privileged -v /sys/fs/cgroup:/h:ro` reads any pod's `memory.events` (measured on a kind node, not on GKE).
Statuses, Node Events and the AGC's log come from read-only `kubectl`.
The same job container opens `/dev/kmsg` (measured on Docker's `--privileged`, not on GKE), and prints from it only the `oom-kill:constraint=` line the kernel prints for a kill, which names the cgroup whose limit was hit, the victim's cgroup and the process.
That is what the counters are checked against, and no other line of the node's log is printed.
The repository is public, so the job's log shows anyone each kill's cgroup paths, which hold pod UIDs and container ids, and the killed process's name, pid and uid, for any pod on that node; the samples already show the paths.
A debug pod would add the node's process list; the container statuses, the line and each container's own counters name the container, which is the question asked.
The trade is that the privilege stays the one this tenant already has.
Where the statuses and the counters do not name the victim, the reading is reported as unresolved rather than widened to a debug pod.
The kubelet's `SystemOOM` Event is read beside them and not relied on: it is raised where the kernel's line names no memory cgroup, which a node-wide kill's does not and a kill at `kubepods`' limit does (kernel 6.12 `memcontrol.c`, kubelet 1.35.5 and its cAdvisor `oomparser.go`, read; the pattern run over both lines in their source format).
So it should not appear for R3 and R4, and a node-wide kill, where it should, is not made.

## Kill order, computed

The kernel kills the process with the most of: resident memory, plus `oom_score_adj` thousandths of the limit that was hit (`oom_kill.c`, v6.12).
On a node with 31.3 GiB and `kubepods` limited to 27.8, and dockerd at 905, all from the first run's R0:

| Process | `oom_score_adj` | Score at a `kubepods` kill | Score at a node-wide kill |
|---|---|---|---|
| A BestEffort pod's | 1000 | 27.8 GiB plus its own | 31.3 GiB plus its own |
| A Burstable DaemonSet container requesting 512Mi or less, not node-critical | 985 or more | 27.4 GiB or more | 30.9 GiB or more |
| The runner's largest | 969 | 27.0 GiB plus its own | 30.4 GiB plus its own |
| dockerd, at the shipped requests (1Gi runner, 3Gi sidecar) | 905 | 25.2 GiB plus its own | 28.4 GiB plus its own |
| One plain-pod filler of 4 GiB, pod requesting 12Gi | about 618 | 17.2 + 4 GiB | 19.4 + 4 GiB |
| One job-container filler of 4 GiB | 0 | 4 GiB | 4 GiB |

So a filler never outscores a worker while each of its processes stays at 4 GiB, and dockerd goes first at the shipped requests only with 1.8 GiB more resident than the runner.
At a runner request of 3Gi or more the two tie, and the larger process goes.

**The fillers stop at the first kill of a worker.** Other pods on the node can outscore one (the table's first two rows; a node-critical pod is -997 and cannot), so their kills are passed over and logged.
Each process takes 2 MiB at a time and exits when either worker's `oom_kill` rises, so one reading is one worker's kill and the second worker survives to log it.
Run locally on two workers and two higher-scored pods under one 700 MiB `kubepods.slice`, with `oom.group` set: one worker killed, the other untouched and no filler killed in 10 of 10 runs, the higher-scored pods at 20 and 100 MiB.
An earlier version with 64 MiB steps overshot and killed a second process in 3 of 3, so the step stays small.
A restarted DaemonSet container is not modelled, and can be killed again.
The scripts are at [the end](#the-probe).

## Shape

One e2e node, two workers on it at once, on a throwaway branch `claude/q1316-probe-run` whose workflow runs only on a push of that branch with `[q1316-probe]` in the commit message, has `permissions: {}` and checks nothing out.

Both jobs sample the host's cgroup tree every second to their logs: each pod's `memory.current`, `memory.events` and `memory.events.local`, the same and `memory.max` for `kubepods.slice`, `/sys/fs/cgroup/docker`'s `memory.current`, and `/proc/vmstat`'s `oom_kill`, with the kernel's line for each kill.
Each also runs [a second reader](#what-it-decides-for-q1403) beside it, shaped as a node reporter would be: uid 65534, every capability dropped, and the cgroup tree read-only.
The two pods are scored alike, so a kill can take either; the survivor's log holds both pods' counters, and is the record: the fill's own summary line is in the plain pod's log.
Both do the same actions, except R8's allocation, which one of them made.
Each job ends in failure once its sampler ends, after 2.5 minutes for R0, R1, R6 and R7 and 8 for a fill, so its end is observable.
R1, R6 and R7 use one worker; R8 uses the pair, so the other worker's log survives the kill.

Both are ordinary `gag-ci-e2e` jobs, at the template's 4Gi of memory requested and 7Gi of limits, and 2.5 CPU requested after `NodeShare`'s sizing, so two fit one `n2-standard-8`.
A job does nothing on a run attempt above 1, so a re-run an arm makes of it repeats no reading.
If the two land on different nodes, the reading is void and repeated once.

## Readings

"Predicted" is from source or the local readings.
Two files hold an `oom` count and they differ for a kill at a lower limit: `memory.events` counts the cgroup and everything below it, and `memory.events.local` the cgroup alone.
Here a pod's `oom` is the one in `memory.events`, and `kubepods.slice`'s is the one in `memory.events.local`; R8 is why, since `kubepods.slice`'s `memory.events` read `oom` 1 for it.
R0 is a gate: where a job cannot read `memory.events` of `kubepods` and of another pod, does not find exactly two workers by their sidecar's 4Gi `memory.max`, or reads the `oom_score_adj` of a runner's `Runner.Listener` or `Runner.Worker`, or of dockerd, more than 10 from the table above (a step may be at 500, which the runner asks for), the run stops and the plan is recomputed.
R0 also reads, by read-only `kubectl`, the UID, QoS class, requests and priority class of every pod on the node, and lists the ones whose computed score is above a runner's.

| ID | Runs | What is done | Platform or tenant | Predicted |
|---|---|---|---|---|
| R0 | 1 | Nothing: cgroup paths; `oom_score_adj` and resident memory of dockerd, the runner's processes and a nested container; node memory, kernel, `memory.oom.group`, `kubepods.slice`'s `memory.max` | | Nested containers outside the pod; 969, 905, 0; `kubepods` capped. Also answers Q1396's first step |
| R1 | 1 | A job runs a container past its own `-m 64m` | Tenant, honest | `Completed`, exit 0; nothing raised |
| R6 | 2 | A job sends dockerd SIGKILL, then in a second run SIGABRT, from a `--pid=host` container | Tenant, deliberate | `Error` 137 and `Error` 2; the job fails with the runner alive |
| R8 | 1 | One worker's step allocates 4 GiB, past the runner's own 3Gi limit | Tenant, honest | The runner `OOMKilled`, 137; pod `Failed`; withheld as `own_limit`, as R3 is; pod `oom` 1 (`memory.events`), `kubepods.slice` `oom` 0 (`memory.events.local`), no `SystemOOM` |
| R3 | 2 | A plain pod on the same node, 12Gi requested and no limit, fills: seven 4 GiB processes | Platform: a neighbour over its request | One runner dies; pod `oom` 0 (`memory.events`), `kubepods.slice` `oom` above 0 (`memory.events.local`); no `SystemOOM` |
| R3t | 2 | R3, with each worker's dockerd set to its runner's `oom_score_adj`, the tie a 3Gi runner request gives | Platform, tie | The larger process of the two goes, by R0's resident memory |
| R4 | 1 | R3, with each dockerd at 1000 | Platform, forced | dockerd `OOMKilled`, 137; runner alive; pod `oom` 0 (`memory.events`), `kubepods.slice` `oom` above 0 (`memory.events.local`) |
| R7 | 1 | Under Kata, one worker: the sidecar's `memory.oom.group` as the guest sees it, R1, then the job's containers past the sidecar's 8Gi | Tenant, honest | `memory.oom.group` 0, since Kata 3.32.0's agent does not set it (read). R1: `OOMKilled`, 143. Second: one process dies, and the sidecar reads `OOMKilled` with 143 if it was the job's, or 137 if it was dockerd |

Order: R0, R3 twice, R3t twice, R4, then R7.
R1, R6 and R8 were read in the first run and are not repeated.
**R4 runs only if** R3 or R3t killed a sidecar with its runner alive, because otherwise no question needs a killed sidecar's counters.

`kubepods.slice`'s counters are the node's since it started, so every reading is the rise across its own window, and the first sample of each is its baseline.

Read for every one: the victim pod's statuses as the API server serves them (`kubectl get pod -w -o json`, started first, each line stamped with its arrival to the millisecond), Events in the tenant's namespace and on the Node, where `OOMKilling` from node-problem-detector is looked for as well as `SystemOOM`, both jobs' samples, the AGC's `scale-set job failed` lines and the `scaleset-failures-ci-e2e` ledger, the job's conclusion, and the time of each.

## The first run

On 2026-11-24, from 20:08 to 21:15 PST, with the system pool at 0 before and after.
The gateway and AGC deployed were `v2.0.1-rc.2`, so nothing merged after it is in these readings.
The node was one `n2-standard-8` with kernel 6.8.0-1064-gke, Ubuntu 24.04.5, kubelet v1.35.8-gke.1796000 and containerd 2.2.7, where the plan had assumed 6.12.
Times are the node's, UTC on 2026-11-25.

| ID | Read |
|---|---|
| R0 | The gate passed. `oom_score_adj` 969 for the runner's processes, 905 for dockerd and 0 for a nested container; `kubepods.slice` `memory.max` 29870862336 and 32093 MiB of memory; nested containers under `/docker`, outside the pod. The uid-65534 reader opened all 150 files, owner root and mode 0444, with the profile `runc (unconfined)`; `/dev/kmsg` gave it `EPERM` with `dmesg_restrict` 1. One pod computes above a runner, the metrics collector at 999 |
| R1 | Runner and sidecar `Completed`, exit 0; the kill's `oom_memcg` is the nested container, and no counter of the pod or `kubepods` rose; nothing raised |
| R6 | `Error` 137, then `Error` 2, the runner alive and the job failed, with no fault raised. The sidecar restarted once; when the restarted container ended, about 5 seconds after the crash, the API server served no `lastState` any more. The AGC's record had kept it |
| R8 | The runner `OOMKilled`, 137, pod `Failed`; `oom` in `memory.events.local` 1 for the container and 0 for the pod, its QoS slice and `kubepods.slice`, whose `memory.events` all read 1; the kernel's `oom_memcg` is the container, so the two agree. The AGC withheld as `own_limit`, and raised `PlatformFaultOnFailedJob` 9 minutes 35 seconds after the kill. No `SystemOOM`; node-problem-detector's `OOMKilling` Event says `Memory cgroup out of memory` and names processes, not the pod |
| R2 | Not read. The node's last report was about when the fill is computed to have started (04:50:03); `NodeNotReady` at 04:50:54, and the same node name was back with a new boot id at 05:04:07 and ready at 05:04:33. No kill, no `SystemOOM` and no `OOMKilling` was recorded, and both jobs' logs are lost. Both pods were `DeletionByTaintManager` at 04:55:54 and `DeletionByGCPControllerManager` from 05:04:08, and the AGC raised `PlatformFaultOnFailedJob` for both with an empty cause and that fault; no `own_limit` |

Timings of R8's kill, for Q1403:

- `oom` rose at 04:36:33.083, and the first `oom_kill` was read at .647, 564 ms later (locally 10 ms).
  A read between the two gives `oom` 1 and `oom_kill` 0.
- `oom_kill` went to 1, 3, then 4 within 51 ms, so a reader that stops at the first rise records too few.
- The poll saw the first rise 379 ms before inotify did, where locally inotify was first.
- The container's cgroup was gone at .799, 788 ms before `OOMKilled` arrived, which is more than the clocks' error; the pod's was gone at 04:36:48.572, 62 ms before `Failed` did, which is less, so it is unresolved.

`memory.oom.group` was 0 on every container that started before containerd was started again at 04:18:43 and 1 on every one after, on that one node.
R0's sidecars started before it and its runners after, so one pod held both values; from R1 on both were 1.
`kata-deploy`'s container started four seconds before that restart; what restarted containerd, and how that changes the value, is not read.

What R2's outcome is evidence of is bounded.
The Compute operation list for that node (2026-11-25 04:30 to 05:15 UTC, read once, after the run) has a delete at 05:01:56, an insert at 05:02:48 and the scale-down's delete at 05:14:54, and no preemption, reset or stop.
So the machine was there and not preempted for the 11 minutes it did not answer, and what came back at 05:04 was a new machine under the same name; the two sources agree.
Who deleted it was not read.
That the fill stalled the node rests on the times: the last report, computed from the node controller's grace, and the fill's start, computed from the script's sleeps, fall together, and no sample survives to show it.

Three things went wrong in the run itself.
The probe's script ran under GitHub's `bash -e`, so a failing command ended its step: R1's and R6's samples stop seconds after the kill, which was read before that.
And the abort was acted on at 21:08 PST, 12 minutes after the node had been not ready for 5 minutes, because the recorder's record of the node going not ready was acted on by nothing until the reading ended.
Step 5's lease check did not gate it; the lease read as this run's before and after.

## The second run

From 21:46 PST on 2026-11-24 to 00:42 on 2026-11-25, with the system pool at 0 before and after, and the same `v2.0.1-rc.2` deployed.
Every e2e node was an `n2-standard-8` with kernel 6.8.0-1064-gke and Ubuntu 24.04.5, and the pool replaced it between readings as it scaled down, so the kills are on four nodes: R3's first on the node R0 read, its second on another, R3t's on two more.
R7 was on the last of those, and each node read the same kernel, image, kubelet and container runtime.
Times are the node's, UTC on 2026-11-25.

| ID | Read |
|---|---|
| R0 | The gate passed: `Runner.Listener` and `Runner.Worker` at 969, dockerd at 905, two workers by their sidecars' 4Gi `memory.max`, and the uid-65534 reader opened all 150 files. Resident memory: 134,820 and 129,792 kB for `Runner.Worker`, 77,248 and 79,404 kB for dockerd |
| R3, 2 of 2 | A runner dies first and its sidecar is running then: the runner `OOMKilled`, 137, at 06:03:00 and 06:32:49, the sidecar stopped after it, the other worker untouched. The kernel's three lines a run are the metrics collector, its config reloader and `Runner.Worker`, all `oom_memcg=/kubepods.slice`. Pod `oom` 0 (`memory.events`), `kubepods.slice` `oom` 58 and 54 (`memory.events.local`). The killed container's own `oom_kill` is 4 and 3 by inotify. The node stayed ready and no filler died |
| R3t, 2 of 2 | The same, with each dockerd set to its runner's score: the runner `OOMKilled`, 137, at 07:28:36 and 07:48:31, pod `oom` 0, `kubepods.slice` `oom` 46 and 64, the container's `oom_kill` 3 and 2. The kubelet had begun to reclaim about 3 s before each kill: `EvictionThresholdMet` at 07:28:33 and 07:48:28, `NodeHasInsufficientMemory` at 07:28:43 and 07:48:31. It evicted no pod, so the table's eviction row is not met |
| R4 | Not run: no sidecar died with its runner alive in R3 or R3t |
| R7a | `memory.oom.group` 0 in the guest, in all five files read. The job's nested container exited 137, killed in the guest at 08:00:35; the sidecar then read `OOMKilled`, 143, at 08:02:46 and the runner `Completed`, 0. The host's reader pod started after that kill, so it saw nothing of it |
| R7b | The job's 9 GiB allocation died in the guest at 08:06:26, its guest line giving `oom_memcg` as the pod's container scope, the victim as a `python3` in a nested container under it, and that scope, whose `memory.max` is the sidecar's 8 GiB, reading `oom` 1 in `memory.events.local` beside it; the job's `docker run` exited 137 then; the sidecar then read `OOMKilled`, 143, at 08:07:52 and the runner `Completed`, 0. The host's reader, on the same node, saw the sandbox pod's cgroup new at 08:04:37 and gone at 08:07:55, and no rise in it or in any other cgroup |

The AGC read the four kills of R3 and R3t the same way, as `own_limit`, within 16 seconds: no re-run, and an `EvictionRerunWithheld` Event that says the runner was killed "at its own memory limit (3Gi)", which is not what killed it.
That is `v2.0.1-rc.2`'s text, and the code on `main` names no cause.
`PlatformFaultOnFailedJob` came 9 min 19 s, 14 min 46 s, 14 min 06 s and 9 min 59 s after the kill, with `cause` `oom`; the two of about 14 minutes are the runs where the job's result was `canceled`, and the other two `failed`.
No `SystemOOM` Event was raised.
Neither R3 fill had an `EvictionThresholdMet` Event, and no pod read `Evicted` in any of the four.

Timings of the four kills, for Q1403:

- `kubepods.slice`'s `oom` first rose 3.7 to 3.8 seconds before the runner's kill and 1.2 before the first kill of any container, which makes it a count of tries and not of kills: at its first rise it read 6, 1, 6 and 16 by the poll, and 4 in the last by inotify, which saw that rise 37 ms earlier.
- The pod's cgroup was gone before `Failed` arrived, by 0.05, 0.49, 0.67 and 0.05 seconds, with the client's clock corrected by `sntp` and the node's taken as within 100 ms. So the second and third are outside the clocks' error and the other two are unresolved, as R8's was.
- The runner's cgroup was gone 0.5 to 1.1 seconds before its `OOMKilled` status arrived.
- The poll's last value of the container's own `oom_kill` was short of inotify's in all four, 3 of 4, none of 3, 1 of 3 and 1 of 2, since the cgroup went within a second of the kill.
  The pod's `memory.events`, which counts what is below it, gave inotify's final value in all four, and outlived the container's by 14 to 15 seconds.

In the sample before R3's first kill, `kubepods.slice` held 27.82 GiB of its limit, and the pods past their request were the filler, 27,207 MiB against 12,288, the metrics collector, 77 against 42, and the node agent `anetd`, 576 against 240, which the kubelet scores too low to pick.
R0's resident memory is what "the larger process goes" rests on for R3t: `Runner.Worker` was 1.6 to 1.7 times dockerd.

### What is weak in it

- **The score in R3t was written and not read back.** The write printed no error, and no sample shows dockerd's score; what is read is that the runner died in both, which is also what R3 reads with no change to the score.
- **One node per kill and one kernel**, so nothing here says how another kernel or image picks.
- **R3t's first push was void by the operator's prompt, not the cluster.** Its filler's confirmation waited 28 minutes, the two workers had ended and the node had gone, so it filled nothing; the next push is its repeat, and R3t is read twice.
  Prompt waits also put R3's second fill 6 min 34 s after its workers started, and R3t's first 7 min 3 s, both inside the samplers' 8.
- **R7a's kill is outside the host's reader**, as planned for.
- **R7 read the status and not Q1395's rule.** The AGC was `v2.0.1-rc.2`, which does not hold that rule: it counts any container's `OOMKilled`, and it recorded both jobs' failures from a pod that was still `Running`, its sidecar's exit read as 0 and with no reason, before the API served `OOMKilled`, 143.
  It has no later read either, so no AGC judged that status.
- **Step 1 took two tries.** The first failed at the API server's webhook call, "No agent available", while GKE's `konnectivity` agents, which carry it to the nodes, were still starting, and the second passed after their rollout; `e2e-start.sh` waits for the controller and not for them.
- **R3's second container went before the poll read a rise in it**, so its last value there is 0; inotify's and the pod's are what it rests on.
- **The job that died holds a worker slot** until GitHub gives up on it, so each reading waited for the one before.

### What the second run settles

In the words of [the table below](#what-each-outcome-means):

- *R3's runner dies first in 2 of 2*: at the shipped requests a kill at `kubepods`' limit is not this class; a node-wide kill, where the runner's computed lead is larger, is not read.
- *R3t's runner dies in 2 of 2*: the tie is decided by which process is larger in that job, and no general claim is made.
- *R4*: not run, so the counter a killed sidecar's kill raises is unread.
- *R7's second reads `OOMKilled` with 143*: the status the rule is keyed on, and its documented miss, are read under Kata, and *R7's first* matches; the rule itself was not run (above).
- *R3's runner reads `OOMKilled` and the AGC withholds or raises*: Q1400 is reproduced for a kill at `kubepods`' limit, with the counters that could have told.

Together the first of them and R6 of the first run reach the table's *not buildable at the shipped requests*, with the bound it carries, that a node-wide kill's order is computed and not read: the runner went first in both, and no platform cause for the `Error` shapes is known.
Of what *buildable* needs, the one fact the runs were for, **a platform fill that kills the sidecar with its runner alive, was not found**, at the shipped requests or at a tie.
Nothing read bears on the other two: no sidecar died, so no counter of a killed one was read, and nothing carries the counters.

## What the second run changes

1. **R2 and R5 are dropped.** R2's fill is what lost the node, and nothing the second run has to decide rests on either.
   Which of the two dies is read by R3 and R3t: the kernel scores a node-wide kill and a kill at `kubepods`' limit alike but for the limit, 31.3 GiB against 27.8, so the runner's lead over dockerd is 2.0 GiB against 1.8 and R3 is the closer call.
   The runner Q1400 and Q1403 read is R3's, killed at `kubepods`' limit.
   R5's `kubepods` `oom` is 0 in both files by the kernel's code, which raises it for a kill at a cgroup's limit alone (`mem_cgroup_oom` in `memcontrol.c`, v6.8 upstream, read), so it would have compared R4 with a constant.
   What is left unread, and is said so below: a node-wide kill's counters, kernel line and `SystemOOM` Event, which rest on source; and a job's own fill, whose one try recorded no kill.
   Asking the kernel for that kill through `/proc/sysrq-trigger` was considered and is not used: it would read what the source already fixes, at the cost of killing by hand on the cluster.
2. **The fills that remain stop at `kubepods`' limit.** R3, R3t and R4 reach 27.8 GiB of 31.3, so the kubelet and containerd, outside `kubepods.slice`, keep about 3.5 GiB; run locally at 700 MiB, with no kubelet, and not on a node.
   A slower fill is not used: the kubelet's eviction would come first.
   A lower limit on `kubepods` is not used: it is another change to the node.
3. **R3 runs right after R0**, so a lost node costs the least, and it gives Q1316's first question first.
4. **`set +e`** in the probe's script, so a failing command does not end the sampling (run under `bash -eo pipefail` with and without).
5. **The recorder acts on the node.** It ends the reading once an e2e node has been not ready for 5 minutes, rather than at the reading's own end; replayed over the first run's node record it ends 5 minutes after the `Unknown` arrived.
6. **The lease check gates each step that changes what another holder would use**, in one command with it; the first run ran it beside step 5 and not before it.
   Removing this run's own objects is not gated, so they go whoever holds the lease.

No mutation is added, the abort is unchanged, the restore deletes the probe branch earlier, and step 2 pushes the new file.

R3 and R3t decide whether the sidecar or the runner goes first, so whether Q1316's class has a memory cause; R3 is the shape Q1403's counters and Q1400's runner are read in, at a limit above the pod; R4 reads the counters with a sidecar killed, if one dies at all.

## What each outcome means

| Outcome | Conclusion |
|---|---|
| R3's runner dies first in 2 of 2 | At the shipped requests a kill at `kubepods`' limit is not this class; a node-wide kill, where the runner's computed lead is larger, is not read. The pod fails, and what the AGC does with it is Q1400's misreading, not a recovery |
| R3t's sidecar dies with the runner alive | The class has a platform cause on a template that requests 3Gi or more for its runner, and R4 reads the counter that kill raises |
| R3t's runner dies in 2 of 2 | The tie is decided by which process is larger in that job; R0's figures say by how much, and no general claim is made |
| R4's `kubepods.slice` `oom` in `memory.events.local` does not rise | R4's kill was not at `kubepods`' limit; R4 is unread |
| It rises as predicted | A kill at `kubepods`' limit is told from R1's, R6's and R8's, where that file's stayed 0 (R8's `memory.events` did not). A fill by a job's own containers is unread: the only one tried lost the node with no kill recorded. Nor is it a neighbour against the job: the job's own pod over its request raises that one too, so a class needs the pod's usage at or under its request beside it. The shipped limits let two workers use 14Gi, which does not reach `kubepods`' limit, so that twin is not run |
| The kubelet evicts a worker or the plain pod before any kill | The kill order is not read by that run; it is repeated once. Two such runs mean eviction comes first on this node, which the existing arms classify, and R3's question stays unread |
| Another pod is killed before a worker | Expected where R0 lists it; the fill goes on and the kill is logged. It says nothing about the two workers' order |
| A worker and another pod die in one window | The samples give the order to the second; where they do not, the statuses' `finishedAt` does |
| No kill of a worker and no eviction | The fill did not reach the limit; repeated once at two more processes, then reported unread |
| R6 matches the local statuses | `Error` stays a status any job can produce; a class keyed on it needs a platform cause, and none is known |
| R7's first is not `OOMKilled`, or exits 137 | Q1395's rule is wrong for Kata and is refiled |
| R7's second reads `OOMKilled` with 143 | A job's container killed at the sidecar's own limit reads as one killed at its own, which Q1395's rule does not name; the rule's documented miss is confirmed |
| R3's runner reads `OOMKilled` and the AGC withholds or raises | Q1400 is reproduced for a kill at `kubepods`' limit, with the counters that could have told |

**Not buildable at the shipped requests**, with the bound that a node-wide kill's order is computed and not read: R3 runner-first in both runs, and no platform cause for the `Error` shapes, which the local runtime-loss readings already point to.

**Buildable** needs all three: a platform fill kills the sidecar with its runner alive; R4's kill raises `kubepods`' `oom` in `memory.events.local`, which R1's, R6's and R8's did not, with a job's own fill unread; and something carries the node's counters to the AGC.
No carrier is known.
The template's entrypoint is not one: on its restart it can read its own pod's counter, but the fact is `kubepods`' and a rise over a baseline, and a termination message is published when the restarted sidecar stops, after the job's completion.
A node agent could, which is a new privileged component, and a message a container the job controls could write meets R9's "nothing tenant code writes".
So a run in which that counter rises leaves the class to a design decision rather than building it.

## What it decides for Q1400

R3 is one of the kills Q1400 describes, a runner killed at `kubepods`' limit and below its own, so one window serves both; the node-wide one is from source.
It settles whether the pod reads `Failed` with the runner `OOMKilled` at the API server, what the AGC then says and does, and when its `PlatformFaultOnFailedJob` comes.
R8 is the kill it is told apart from, a runner past its own limit, and the AGC is expected to treat it alike.
[M3](q555-automatic-recovery-design.md#readings-from-the-dogfood-turn-up-2026-11-22) read that kill's status and not its counters or Events.
Each container's `current` and `peak` beside its `max` say how far below its own limit that runner was, which is the claim Q1400 makes.
What separates them is expected to be the pod's `oom` in `memory.events`, 1 for R8 (read) and 0 for R3; `kubepods`' `oom` in `memory.events.local`, above 0 for R3, and the `SystemOOM` Event would then tell a node-wide kill, which is not read.
None of those reaches the AGC today, so the readings say what honest wording can rest on and what a class would need.
The set is `NodeShare`, so what is read is the withheld `own_limit` branch.
Not read: a raise under `Binpack` or `Throughput`, which starts from the same two facts, and a runner with no limit.

## What it decides for Q1403

The node reporter's design rests on five facts, and R8 and R3 read them with no further change to the cluster; a sixth, under Kata, takes one more pod.

- **Whether it may read.** The second reader opens every pod's and container's `memory.events`, `memory.events.local` and `memory.peak`, and prints each refusal's errno, its own AppArmor profile, one file's owner and mode, and each cgroup that appears after it started.
  It is an unprivileged container of the job's own Docker, so it is under Docker's default seccomp profile; the plain pod of R3, R3t and R4, which is a pod as a reporter would be, makes the same opens once before it fills.
  Each also tries `/dev/kmsg` and prints `kernel.dmesg_restrict`; the pod has no such device, so only the job's reader tests that.
- **How long the facts last.** It prints, to the millisecond, the first rise of `oom` or `oom_kill` in a cgroup and the cgroup's removal, once from inotify and once from a 50 ms poll.
  Locally (kernel 6.12, eighteen kills in six runs, six each by the container's own limit, its pod's and `kubepods`') inotify saw each rise 1 to 50 ms before the poll.
  It gave no delete event for any of twelve removed cgroups, and a file held open across the removal read `ENODEV`; so only the poll times a removal, and the last values are whatever was read before it.
- **Whether `Failed` arrives first.** That is the stamped watch against the removals of the container's and the pod's cgroups.
  The two clocks are the node's and the client's, so `sntp time.google.com` is read on the client beside each reading, its offset taken out of every arrival, and its error kept.
  The node's own error against the same source is assumed under 100 ms and not read.
  In R3 the plain pod's log is followed and stamped too: its first line's arrival less the time it prints is the most the client can be ahead, and nothing bounds it being behind but `sntp`.
  A margin, either way, inside the two errors together is reported as unresolved.
- **Which counter names the limit.** Expected `oom` in `memory.events.local`, for the container, its pod, its QoS slice and `kubepods.slice`: 1, 0, 0, 0 for R8; and 0, 0, 0 and a rise for R3.
  No rise in any, for a node-wide kill, is from source.
  A pod's own limit (`spec.resources`) is the fourth shape and is not run: locally it read 0 for the container and 1 for the pod.
  The kernel's line is the answer each is checked against.
- **What the host sees of a kill in a Kata guest.** One more plain pod, `q1316-read`, in the shape and with the mounts of step 3's and no fill, runs the reader on the Kata worker's node.
  It prints a cgroup's counters when it first sees it, at each rise, when it goes, and at its end for every one still there, so R7b's pod is read from its `new`, `rise` and `gone` lines and is likely not in the `end` ones.
  Expected: no rise of `oom` or `oom_kill` in the sandbox pod's cgroups, whose UID `kubectl` gives, while the guest's sidecar reads `OOMKilled`.
  It starts once R7a's worker has a node, which can be after R7a's own kill, so R7b's is the kill it is for; where R7b lands on another node that is not read, and is not repeated.
  Only `kubepods.slice` is read, so a virtual machine Kata keeps in a cgroup outside it is not seen, and where Kata keeps it is not read here.
- **Who was over its request.** Every pod's `memory.current` in the sample before the kill, beside its requested memory from R0 by UID, and the plain pod's 12Gi.

What the local runs read, the same in six of six (2026-11-24, kernel 6.12.76, one privileged container holding a `kubepods.slice` of its own, the reader at uid 65534 in it and `sample.py` as root; no kubelet, containerd or pod):

- **Opening.** `memory.events` is owned by root with mode 0444, and the reader opened all 24 files, three in each of eight cgroups.
- **`/dev/kmsg`.** The reader got `EPERM` with `kernel.dmesg_restrict` 1; as root it opened.
- **Counters and the kernel's line**, for one kill at each limit in each run, the victim a container with `memory.oom.group` set:

| Limit hit | `oom` in `memory.events.local`: container, pod, QoS slice, `kubepods.slice` | `oom_memcg` in the kernel's line | `task_memcg` |
|---|---|---|---|
| The container's | 1, 0, 0, 0 | The container | The container |
| The pod's | 0, 1, 0, 0 | The pod | The container |
| `kubepods.slice`'s | 0, 0, 0, 1 | `kubepods.slice` | The container |

A node-wide kill was not made locally.

| Outcome | Conclusion |
|---|---|
| The reader and the plain pod open every file | A reporter of the counters needs no privileged container, and its `hostPath` alone decides its namespace's level |
| Either is refused | `EACCES` is a file mode or an AppArmor profile, so the owner, mode and profile are read with it; a reporter needs more than the row's cost names |
| The pod's cgroup outlives the arrival of `Failed` in every run, by more than the clocks' error | The shortest margin, less that error, is the bound a reporter has, from these few kills on one node image |
| It is gone first in any run, by more than the clocks' error | A reporter has to read as the kill happens, not after the pod fails |
| The counters and the kernel's line name the same limit in R8 and R3 | The counters are enough for those two |
| A kill has no line, or `/dev/kmsg` does not open | The counters are unchecked for that kill. The kernel prints at most ten such reports in 5 seconds for the node and none for a group kill's other processes (`oom_kill.c`, v6.12, read); the sampler says where lines were lost |
| They differ in any | The counters are not, and the kernel's log is the reporter's source |
| R7b's kill raises nothing in the sandbox pod's counters, where the log has a `new` line with counters for that pod's UID before the kill, and the poll's `gone` for it or an `end` line after | A host reporter of the pods' cgroups cannot see a Kata job's kill |
| Any of those lines is missing, or the kill's time is not between them | Not read |
| It raises one | The limit that rose says whether the guest itself was killed at the pod's limit, which is a different kill from the sidecar's |

Not read: whether a non-root pod could read `/dev/kmsg` or the node's journal, since each needs another `hostPath`; a worker on the Guaranteed or BestEffort path; and a pod's own limit on GKE.

## Mutations

Environment, every command: `PROJECT=actions-gateway-dogfood CLUSTER=gag-dogfood ZONE=us-east1-b REPO=actions-gateway/github-actions-gateway`, `CTX=gke_actions-gateway-dogfood_us-east1-b_gag-dogfood`, and `KUBECONFIG` set to a file of this run's own.
The scripts run `get-credentials` and then unpinned `kubectl`; with their own `KUBECONFIG` they neither move another session's context nor follow one that moved.
The values are the runbook's ([gke-dogfood.md](gke-dogfood.md#variables)) and are re-read against the active project before the first one.

| # | Command | Changes |
|---|---|---|
| 0a | `gcloud container clusters get-credentials $CLUSTER --project=$PROJECT --zone=$ZONE` | Writes the `KUBECONFIG` file |
| 0b | [Take the release gate's lease](#the-lease) | Creates one Lease, `default/actions-gateway-release-gate` |
| 1 | `E2E_VARIANT=dind E2E_ROUTE_VAR=0 scripts/dogfood/e2e-start.sh` | System pool to its running size, which brings up the always-on gateways; the `dind` overlay (namespace labels, template, open egress policy, gateway); registry mirrors up |
| 2 | `git push origin HEAD:claude/q1316-probe-run`, one push per reading | One workflow run of the file below; the e2e pool scales 0 to 1 |
| 3 | `kubectl --context $CTX -n gag-dogfood-e2e create configmap q1316-fill --from-file=fill.py --from-file=reader.py`, once, then `apply -f hog.yaml` with `NODE` set, per R3, R3t and R4 | One ConfigMap, and one non-root, unprivileged pod with the host's `/sys/fs/cgroup` mounted read-only, which is how it sees a worker die |
| 4 | `kubectl --context $CTX -n gag-dogfood-e2e delete pod/q1316-hog` after each, `pod/q1316-read` once its log has its `end` lines, 15 minutes after it starts, or once it is dead, its log read first, then `configmap/q1316-fill` | Removes them |
| 5 | `E2E_VARIANT=kata E2E_ROUTE_VAR=0 scripts/dogfood/e2e-start.sh` | Back to the `kata` overlay, and deletes the open egress policy |
| 5a | `kubectl --context $CTX -n gag-dogfood-e2e apply -f read.yaml` with `NODE` set, once, when R7a's worker is on a node | One non-root, unprivileged pod on that node, with step 3's two mounts and no fill; its own deadline ends it 20 minutes after the kubelet takes it, image pull included |
| 6 | `GAG_ROUTE_VARS=0 scripts/dogfood/e2e-stop.sh` | Drains, deletes the gateway, scales mirrors to 0, system pool to its running size |
| 7 | `GAG_ROUTE_VARS=0 scripts/dogfood/stop.sh`, only where the system pool read 0 before step 1 | Drains every tenant's workers, then system pool to 0 |
| 8 | `kubectl --context $CTX delete clusterrunnertemplate privileged-dind`, only where it was absent before step 1 | Removes the template step 1 created |
| 9 | `git push origin --delete claude/q1316-probe-run` | Removes the branch |
| 10 | [Release the lease](#the-lease) | Deletes the Lease |

Step 5a's pod is admitted as step 3's is: both overlays give the namespace the `privileged` Pod Security level, which a `hostPath` needs (`deploy/dogfood-e2e/overlays/kata/kustomization.yaml`, read), and no admission policy of the chart matches pods (read).
The namespace's quota is six pods, and it is read before steps 3 and 5a: with the gateway's one AGC pod and no proxy pool in the overlays (`shared_agc_deployment.go` and `deploy/dogfood-e2e`, read), R3 uses four and step 5a three, and an AGC rollout one more of each.
`start.sh` is not used: it dispatches unit and integration runs onto the cluster, and `e2e-start.sh` resizes the system pool and waits for the GMC itself.
A system pool brought up from 0 runs the always-on gateways, so a job already queued for them would be served.
No repository variable, Secret, CRD, chart or AGC image changes.
The App Secret in the namespace is used by the AGC as it is, and is not read.

**Between steps 0a and 0b**, read-only: the active project, `scripts/dogfood/ops.sh at-rest`, each pool's node count, the release lease (`kubectl --context $CTX -n default get lease actions-gateway-release-gate`), whether `clusterrunnertemplate/privileged-dind` exists, pods in `gag-dogfood-e2e`, and open runs on `gag-ci-e2e`.
Any lease, worker or queued run there means the cluster is taken: stop, with nothing taken.
After 0b the pods and the queued runs are read once more, since something can start between; a stop there runs step 10 and nothing else.

prod-guard, the hook that checks an agent session's commands, refuses a direct mutating `kubectl` on this cluster, which steps 0b, 3, 4, 5a, 8 and 10 and the restore's NetworkPolicy deletion are and the scripts are not.
The documented way through is a `PROD_GUARD_OVERRIDE=<reason>` prefix on each ([kind-iteration.md](../development/kind-iteration.md#verify-the-resolved-target-before-any-mutating-command)), so the grant has to name it, or a maintainer runs those commands by hand.

### The lease

The release gate takes `default/actions-gateway-release-gate` before it touches the cluster and refuses where one exists, so this run takes the same one.
It does not take it as a gate does.
A gate's holder is `host/pid`, and a pid whose command is not `validate-release.sh` reads as an orphan that the next gate, or `--reclaim`, tears the cluster down for; a probe that copied the form would invite that.
A holder with no `/` in it reads `foreign`, which both refuse and neither acts on (`lease_judge`, run on this holder, and `lease-test.sh`).
So there is no renewal and no expiry, and a run that dies leaves a record that blocks gates until somebody deletes it, which [release.md](../operations/release.md) says how to do.

Take it, which fails where one exists:

```bash
mkdir -p tmp/q1316 && echo "q1316-dogfood-reproduction-$(date +%s)" > tmp/q1316/holder
H="$(cat tmp/q1316/holder)"
kubectl --context "$CTX" -n default create -f - <<EOF
apiVersion: coordination.k8s.io/v1
kind: Lease
metadata:
  name: actions-gateway-release-gate
  annotations:
    actions-gateway.com/release-gate-rc: "Q1316 reproduction, no RC"
spec:
  holderIdentity: "$H"
EOF
```

The holder is kept in a file because a shell variable does not outlive one command of an agent session.
Each of steps 1 to 3 and 5 to 8, and the restore's NetworkPolicy deletion, is joined to this with `&&` in one command, so a step does not start where it fails, which an empty holder or an empty read makes it do.
Steps 4 and 9 are not joined to it: they remove, by name, only what this run made, and leaving those on another holder's cluster is worse than deleting them there.


```bash
lease_held() {
  local h got
  h="$(cat tmp/q1316/holder)"
  got="$(kubectl --context "$CTX" -n default get lease actions-gateway-release-gate -o jsonpath='{.spec.holderIdentity}')"
  [[ -n "$h" && "$got" == "$h" ]]
}
lease_held && E2E_VARIANT=kata E2E_ROUTE_VAR=0 scripts/dogfood/e2e-start.sh
```

Release it, which changes nothing unless `$H` still holds it; between the two calls the record is still one no gate acts on:

```bash
H="$(cat tmp/q1316/holder)"
kubectl --context "$CTX" -n default patch lease actions-gateway-release-gate --type=json \
  -p '[{"op":"test","path":"/spec/holderIdentity","value":"'"$H"'"},{"op":"replace","path":"/spec/holderIdentity","value":"q1316-released"}]' &&
  kubectl --context "$CTX" -n default delete lease actions-gateway-release-gate
```

Where the delete fails after the patch, the block cannot be run again, because its test now fails: once the holder reads `q1316-released`, delete the Lease by name.

The lease stops a gate, and not a hand-run `e2e-start.sh`, `start.sh` or `stop.sh`, nor a run dispatched to `gag-ci-e2e`, none of which read it.
So before each of steps 1, 2, 3, 5 and 5a the pods in `gag-dogfood-e2e`, each pool's node count and the runs queued on `gag-ci-e2e` are read as well, and anything this run did not make is an abort.
A gate started in the window refuses, and a candidate tagged in it fails its validation and needs a re-run.

## Abort

Stop, run the restore, and report what was read, on any of:

- another session's job reaches the e2e set, or the lease is gone or another's;
- the e2e node is `NotReady` for 5 minutes, or a pod other than the workers, the two plain pods of steps 3 and 5a or a DaemonSet's is on it;
- a container on any other node restarts during a reading;
- Spot preemption twice;
- 4 hours from step 1, which leaves the restore inside 4.5.

R3, R3t and R4 reach `kubepods`' limit on the e2e node on purpose.
The pool's taint keeps other workloads off it, and it is Spot and disposable.
Its DaemonSets' containers are expected to be killed and restart, the metrics collector among them, so the node's own monitoring has gaps during a fill; that is not an abort.
The system pool is a different pool.

## Restore

1. Step 4, then cancel any probe run, then step 9, whoever holds the lease.
   Where the lease reads another holder, stop here and report: the cluster is that holder's, and the rest would tear down what it is using.
   Where there is no lease, take it again; if that fails, the same.
2. Step 5, which leaves the tenant on its default overlay, then step 6.
   Where step 5 itself fails, `kubectl --context $CTX -n gag-dogfood-e2e delete networkpolicy e2e-open-egress` by hand, since `e2e-stop.sh` leaves it.
3. Where step 6's drain times out it stops before deleting the gateway: read what it reports, cancel the probe run it names and re-run it.
   `SKIP_E2E_DRAIN=1` is not used, and a worker that is not this run's is left to finish.
4. Steps 7 and 8 where they apply.
   Where step 7's drain times out the system pool stays up; that is reported with what is running, and not forced.
5. `scripts/dogfood/ops.sh at-rest`, compared with the reading taken before step 1; the e2e pool scales to 0 on the autoscaler's schedule, so it is read again 35 minutes later.
6. Step 10, once the pools are back.
   Where a drain did not finish the lease is kept, as the gate's teardown keeps its own, and the report says so.

## Time and cost

Estimated at 2 hours 22 minutes with the cluster held, 2 hours 35 with R4, from the first run's times where it has one: 25 minutes to start and read the baseline, 15 for R0, 4 readings at about 13 minutes each, 1 more if R4 runs, 30 for the switch to Kata and R7 with its reader, 20 to restore.
A fill is 12 because a runner that dies holds its job about 10 minutes, until the lock lapses ([M1](q555-automatic-recovery-design.md#readings-from-the-dogfood-turn-up-2026-11-22)), each uses both worker slots, and none overlaps another.

Estimated from third-party price listings read on 2026-11-24 (Google's own page was not read), over 4.5 hours, the abort limit and the restore:

| Item | Rate | Estimate |
|---|---|---|
| One `n2-standard-8` Spot (two in the worst case) | $0.138 an hour ([listing](https://www.devzero.io/instances/gcp/n2-standard-8)); $0.388 on demand | $0.62 to $1.24; $3.49 at the on-demand rate for two |
| Two `e2-standard-2` system nodes | $0.067 an hour each ([listing](https://www.devzero.io/instances/gcp/e2-standard-2)) | $0.60 |
| Boot disks and one 100Gi volume for the Kata worker | not priced | cents, unverified |

So under $2, and about $4.10 at the on-demand rate, with nothing priced for disks.
Step 5a's pod adds no node, and ten minutes of the same ones.
The cluster's own fee is paid either way.

## The probe

The commit pushed to `claude/q1316-probe-run` adds one file, `.github/workflows/q1316-probe.yml`, and its message is `[q1316-probe] <ID>`, with ` solo` after the ID for R1, R6 and R7.
R6 is `R6a` and `R6b`, and R7 is `R7a` and `R7b`.
`bash -n`, `shellcheck -S warning` and `actionlint` pass on it but for the runner label it does not know and info notes that single quotes do not expand, which is meant.
`sample.py` and `reader.py` ran locally over a `kubepods.slice` made in one container, six times, while `reader.py` gained its counters at the end (from the fourth), its profile (the fifth) and the first and last values on its `new` and `gone` lines (the sixth).
The first run's file ran on GitHub; this one, with `set +e` and without R2's and R5's lines, has not.

```yaml
name: q1316-probe
on:
  push:
    branches: [claude/q1316-probe-run]
permissions: {}
jobs:
  probe:
    if: contains(github.event.head_commit.message, '[q1316-probe]')
    strategy:
      fail-fast: false
      matrix:
        worker: ${{ fromJSON(contains(github.event.head_commit.message, 'solo') && '["a"]' || '["a", "b"]') }}
    runs-on: gag-ci-e2e
    timeout-minutes: 15
    steps:
      - if: github.run_attempt == 1
        shell: bash
        env:
          MSG: ${{ github.event.head_commit.message }}
          W: ${{ matrix.worker }}
          I: python@sha256:9ef1a599f863231c3d91e941389bfdc4a2d6070557243d54df1a4e57015b960e # 3.13-alpine
        run: |
          set +e -uo pipefail
          R=${MSG##*\[q1316-probe\] }
          R=${R%%[!A-Za-z0-9]*}
          echo "reading $R worker $W"
          cd "$RUNNER_TEMP" || exit 1
          cat > sample.py <<'PY'
          @SAMPLE@
          PY
          cat > fill.py <<'PY'
          @FILL@
          PY
          cat > reader.py <<'PY'
          @READER@
          PY
          until docker info >/dev/null 2>&1; do sleep 1; done
          docker pull "$I"
          host() { docker run --rm -i --privileged --pid=host -v /sys/fs/cgroup:/h:ro "$I" "$@"; }
          case $R in R0|R1|R6*|R7*) secs=150 ;; *) secs=480 ;; esac
          host python3 - "$secs" < sample.py &
          log=()
          if host test -c /dev/kmsg; then log=(--device /dev/kmsg:/dev/kmsg:r); fi
          docker run --rm -i --user 65534:65534 --cap-drop ALL --security-opt no-new-privileges --read-only "${log[@]}" -v /sys/fs/cgroup:/h:ro "$I" python3 - "$secs" < reader.py &
          own=$(cat /proc/1/oom_score_adj)
          case $R in
            R3t) host sh -c "echo $own > /proc/\$(pidof dockerd)/oom_score_adj" ;;
            R4) host sh -c 'echo 1000 > /proc/$(pidof dockerd)/oom_score_adj' ;;
          esac
          sleep 20
          case $R in
            R0)
              free -m; uname -r; docker version --format '{{.Server.Version}}'
              for p in /proc/[0-9]*; do echo "runner ${p#/proc/} $(cat "$p/comm" "$p/oom_score_adj" | tr '\n' ' ') $(grep VmRSS "$p/status")"; done
              host sh -c 'for p in /proc/[0-9]*; do echo "dind ${p#/proc/} $(cat $p/comm $p/oom_score_adj $p/cgroup | tr "\n" " ") $(grep VmRSS $p/status)"; done' ;;
            R1|R7a)
              host sh -c 'find /h -maxdepth 7 -name memory.oom.group -exec grep -H . {} +'
              docker run --rm -m 64m --memory-swap 64m "$I" python3 -c 'b = bytearray(b"\1") * (256 << 20)'; echo "exit $?" ;;
            R6a) host sh -c 'kill -KILL $(pidof dockerd)' ;;
            R6b) host sh -c 'kill -ABRT $(pidof dockerd)' ;;
            R7b) docker run --rm "$I" python3 -c 'b = bytearray(b"\1") * (9 << 30)'; echo "exit $?" ;;
            R8) if [[ $W == a ]]; then python3 -c 'b = bytearray(b"\1") * (4 << 30)'; echo "exit $?"; fi ;;
          esac
          wait
          exit 1
```

`@SAMPLE@` is `sample.py`:

```python
import glob, os, sys, time

def rd(path):
    try:
        with open(path) as f:
            return " ".join(f.read().split())
    except OSError as e:
        return type(e).__name__

log = None
try:
    log = os.open("/dev/kmsg", os.O_RDONLY | os.O_NONBLOCK)
    os.lseek(log, 0, os.SEEK_END)
except OSError as e:
    print("kmsg", e)

def kernel():
    """The kernel's one line for each out-of-memory kill, and no other line of its log."""
    while log is not None:
        try:
            line = os.read(log, 8192).decode(errors="replace").split("\n")[0]
        except BrokenPipeError:
            print("K lines lost")
            continue
        except OSError:
            return
        if "oom-kill:constraint=" in line:
            print("K", time.strftime("%T"), line)

end = time.time() + int(sys.argv[1])
while time.time() < end:
    kernel()
    node = rd("/proc/vmstat").split()
    print("T", time.strftime("%T"), "oom_kill", node[node.index("oom_kill") + 1])
    for d in ["/h", "/h/kubepods.slice", "/h/docker"] + sorted(glob.glob("/h/kubepods.slice/**/*pod*.slice", recursive=True)):
        print(" ", d[3:], *(rd(d + "/memory." + f) + " |" for f in ("current", "max", "oom.group", "events", "events.local")))
        for c in sorted(glob.glob(d + "/cri-*.scope")):
            print("   ", c[-20:], *(rd(c + "/memory." + f) + " |" for f in ("current", "peak", "max", "oom.group", "events.local")), flush=True)
    time.sleep(1)
```

`@FILL@`, and the ConfigMap's `fill.py`, is the script tested above:

```python
import glob, os, sys, time

def field(path, name):
    with open(path) as f:
        return next(int(l.split()[1]) for l in f if l.startswith(name + " "))

def find():
    """A worker is a pod whose Docker-in-Docker sidecar has the template's 4Gi limit."""
    return sorted(
        os.path.dirname(os.path.dirname(f)) + "/memory.events"
        for f in glob.glob("/h/kubepods.slice/*/*pod*.slice/*/memory.max")
        if open(f).read().strip() == str(4 << 30)
    )

n, mib, settle = map(int, sys.argv[1:])
end = time.time() + 300
while len(workers := find()) != 2 and time.time() < end:
    time.sleep(1)
time.sleep(settle)
if len(workers) != 2 or find() != workers:
    sys.exit(f"found {len(workers)} workers, want 2: not filling")

def kills():
    return [field(w, "oom_kill") for w in workers]

base, node = kills(), field("/proc/vmstat", "oom_kill")
for _ in range(n):
    if os.fork() == 0:
        held = []
        for _ in range(mib // 2):
            if kills() != base:
                break
            held.append(bytearray(b"\1") * (2 << 20))
        else:
            for _ in range(200):
                if kills() != base:
                    break
                time.sleep(0.1)
        os._exit(0)
died = [s for s in (os.wait()[1] for _ in range(n)) if s]
print("workers", base, "->", kills(), "node kills", field("/proc/vmstat", "oom_kill") - node,
      "fillers killed", len(died), flush=True)
```

`@READER@`, and the ConfigMap's `reader.py`, is the second reader:

```python
import collections, ctypes, errno, glob, os, select, stat, struct, sys, time

FILES = ("memory.events", "memory.events.local", "memory.peak")

def say(*a):
    print("N", f"{time.time():.3f}", *a, flush=True)

def err(e):
    return errno.errorcode.get(e.errno, e.errno)

def read(fd):
    try:
        return " ".join(os.pread(fd, 4096, 0).decode().split())
    except OSError as e:
        return err(e)

def dirs():
    pods = glob.glob("/h/kubepods.slice/**/*pod*.slice", recursive=True)
    return ["/h/kubepods.slice"] + pods + [c for p in pods for c in glob.glob(p + "/*.scope")]

def attempt(path):
    try:
        return os.open(path, os.O_RDONLY)
    except OSError as e:
        return err(e)

def loop(tag, tick):
    """Report each change of a cgroup's events, as seen by tick, until the end."""
    held, scan = {}, 0
    while time.time() < end:
        if time.time() > scan:
            scan = time.time() + 1
            for d in dirs():
                fd = attempt(d + "/memory.events") if d not in held else -1
                if fd != -1:
                    held[d] = [fd, attempt(d + "/memory.events.local"), read(fd) if isinstance(fd, int) else fd]
                    say(tag, "new", d[3:], held[d][2])
                    if watch and isinstance(fd, int):
                        watch(d)
        tick()
        for d, (fd, local, was) in list(held.items()):
            if not isinstance(fd, int):
                continue
            now = read(fd)
            if not os.path.isdir(d):
                say(tag, "gone", d[3:], "held open:", now, "| last", was)
                os.close(fd)
                held[d][0] = "gone"
            elif now.partition(" oom ")[2] != was.partition(" oom ")[2]:
                say(tag, "rise", d[3:], now, "| local", read(local) if isinstance(local, int) else local)
            held[d][2] = now
    for d, (fd, local, was) in held.items():
        if tag == "poll" and isinstance(fd, int):
            say("end", d[3:], read(fd), "| local", read(local) if isinstance(local, int) else local)

if not os.path.isdir("/h/kubepods.slice"):
    sys.exit("no kubepods.slice: a Kata guest")
end = time.time() + int(sys.argv[1])
seen = collections.Counter()
for d in dirs():
    for f in FILES:
        fd = attempt(d + "/" + f)
        seen[f, fd if isinstance(fd, str) else "opened"] += 1
        if isinstance(fd, int):
            os.close(fd)
s = os.stat("/h/kubepods.slice/memory.events")
say("uid", os.getuid(), "owner", s.st_uid, "mode", stat.filemode(s.st_mode), dict(seen))
fd = attempt("/proc/self/attr/current")
say("confined by", read(fd) if isinstance(fd, int) else fd)
k = attempt("/dev/kmsg")
say("kmsg", k if isinstance(k, str) else "opened", "dmesg_restrict", open("/proc/sys/kernel/dmesg_restrict").read().strip())

if len(sys.argv) > 2:
    sys.exit()
watch = None
if os.fork() == 0:
    loop("poll", lambda: time.sleep(0.05))
    os._exit(0)
libc = ctypes.CDLL(None, use_errno=True)
ino, names = libc.inotify_init1(os.O_NONBLOCK), {}

def watch(d):
    wd = libc.inotify_add_watch(ino, (d + "/memory.events").encode(), 0x402)  # IN_MODIFY | IN_DELETE_SELF
    names[wd] = d[3:] if wd >= 0 else errno.errorcode[ctypes.get_errno()]

def notified():
    if select.select([ino], [], [], 1)[0]:
        data, at = os.read(ino, 65536), 0
        while at < len(data):
            wd, mask, _, n = struct.unpack_from("iIII", data, at)
            at += 16 + n
            if mask != 0x2:
                say("inotify", names.get(wd), hex(mask))

loop("inotify", notified)
os.wait()
```

`hog.yaml`, whose image has `python3` (checked locally):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: q1316-hog
  namespace: gag-dogfood-e2e
spec:
  restartPolicy: Never
  activeDeadlineSeconds: 180
  automountServiceAccountToken: false
  nodeSelector:
    kubernetes.io/hostname: NODE # the node both workers are on
  tolerations:
    - { key: dedicated, value: e2e, effect: NoSchedule }
  securityContext: { runAsNonRoot: true, runAsUser: 1001, seccompProfile: { type: RuntimeDefault } }
  containers:
    - name: fill
      image: ghcr.io/actions-gateway/dogfood-e2e-runner:2.338.2-1
      command: [sh, -c, "python3 /f/reader.py 0 once; exec python3 /f/fill.py 7 4096 5"]
      resources: { requests: { memory: 12Gi } }
      securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: [ALL] }, readOnlyRootFilesystem: true }
      volumeMounts:
        - { name: fill, mountPath: /f, readOnly: true }
        - { name: cgroup, mountPath: /h, readOnly: true }
  volumes:
    - { name: fill, configMap: { name: q1316-fill } }
    - { name: cgroup, hostPath: { path: /sys/fs/cgroup, type: Directory } }
```

`read.yaml` is step 5a's pod:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: q1316-read
  namespace: gag-dogfood-e2e
spec:
  restartPolicy: Never
  activeDeadlineSeconds: 1200
  automountServiceAccountToken: false
  nodeSelector:
    kubernetes.io/hostname: NODE # the Kata worker's node
  tolerations:
    - { key: dedicated, value: e2e, effect: NoSchedule }
  securityContext: { runAsNonRoot: true, runAsUser: 1001, seccompProfile: { type: RuntimeDefault } }
  containers:
    - name: read
      image: ghcr.io/actions-gateway/dogfood-e2e-runner:2.338.2-1
      command: [python3, /f/reader.py, "900"]
      resources: { requests: { memory: 64Mi } }
      securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: [ALL] }, readOnlyRootFilesystem: true }
      volumeMounts:
        - { name: fill, mountPath: /f, readOnly: true }
        - { name: cgroup, mountPath: /h, readOnly: true }
  volumes:
    - { name: fill, configMap: { name: q1316-fill } }
    - { name: cgroup, hostPath: { path: /sys/fs/cgroup, type: Directory } }
```
