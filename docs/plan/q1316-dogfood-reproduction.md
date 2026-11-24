# Q1316: the dogfood reproduction, planned

> **Nothing here has been run.** The plan needs a grant for the prod-classified dogfood cluster, read-only checks included.
> Local readings it builds on are in [the design](q555-automatic-recovery-design.md#readings-for-the-dockerd-sidecar-class-q1316).

## What it decides

Q1316's retry class needs a fact that separates a platform's crash of the Docker-in-Docker sidecar from the job's own, and a platform cause that leaves the runner alive to fail the job.
The local readings leave four questions only the dogfood cluster answers:

1. When a node runs out of memory, does the kernel kill the sidecar, or the runner first?
2. Where the sidecar is killed, do the kernel's counters separate a neighbour from the job?
3. Do the API server's statuses match the kubelet's, under `privileged-dind` and under Kata?
4. Does the out-of-memory class read a node-killed runner as an own-limit kill ([Q1400](../queue/Q1400.md))?

One platform cause is already out: locally, restarting containerd or the kubelet and killing containerd left the sidecar and the runner running, and killing the pod's shim ended both.
Those readings are one run each, and nothing here repeats them.

## No privileged debug pod

Every node fact this needs is readable from inside a `privileged-dind` job: its daemon sees the host's cgroup tree, so `docker run --privileged -v /sys/fs/cgroup:/h:ro` reads any pod's `memory.events` (measured on a kind node, not on GKE).
Statuses, Node Events and the AGC's log come from read-only `kubectl`.
A debug pod would add the node's process list and its kernel log, which name the killed process; the status, the counters and the `SystemOOM` Event say the same for the one question asked.
The trade is that the privilege stays the one this tenant already has.
If the `SystemOOM` Event is absent and the counters do not name the victim, the reading is reported as unresolved rather than widened to a debug pod.

## Shape

One e2e node, two workers on it at once, on a throwaway branch `claude/q1316-probe-run` whose workflow runs only on a push of that branch with `[q1316-probe]` in the commit message, has `permissions: {}` and checks nothing out.

Both jobs sample the host's cgroup tree every second to their logs: each pod's `memory.events` and `memory.events.local`, the same two and `memory.max` for `kubepods.slice`, `/sys/fs/cgroup/docker`'s `memory.current`, and `/proc/vmstat`'s `oom_kill`.
The two pods are scored alike, so a node kill can take either; the survivor's log holds both pods' counters, and a reading where both die is void and repeated once.
The `victim` also does the reading's action, and holds a step open so the job's end is observable.

Both are ordinary `gag-ci-e2e` jobs.
The victim does nothing on a run attempt above 1, so a re-run an arm makes of it repeats no reading.
Their requests (2.5 CPU and 4Gi each, with `NodeShare`) fit one `n2-standard-8`; if they land on two nodes, the pair's readings are void and the pair is re-run once.

## Readings

Each is run three times except R0, R1, R6 and R7, so a tie-break in the kernel's choice shows.
"Predicted" is from source or the local readings.

| ID | What is done | Platform or tenant | Predicted |
|---|---|---|---|
| R0 | Nothing: cgroup paths, `oom_score_adj` of dockerd, a nested container and the runner, node memory, kernel, `memory.oom.group`, `kubepods.slice`'s `memory.max` against the node's allocatable | | Nested containers outside the pod; runner 969, dockerd 907, nested 0; `kubepods` capped. Also answers Q1396's first step |
| R1 | Victim runs a container past its own `-m 64m` | Tenant, honest | `Completed`, exit 0; nothing raised |
| R2 | Victim's own container fills the node | Tenant, honest | The runner dies first; pod `Failed`; the AGC withholds as `own_limit` (the set is `NodeShare`), which is Q1400's reading; `kubepods.slice` `oom` 0 |
| R3 | A plain pod on the node, 16Gi requested and no limit, fills the node | Platform: a neighbour | The runner dies first, with `kubepods.slice` `oom` above 0; or the kubelet evicts the plain pod |
| R4 | R3, with dockerd's `oom_score_adj` raised to 1000 by the victim beforehand | Platform, selection forced | dockerd `OOMKilled`, 137; runner alive; pod `oom` 0 |
| R5 | R2, with the same raise | Tenant, selection forced | The same as R4, with `kubepods.slice` `oom` 0 where R4's is above 0 |
| R6 | Victim sends dockerd SIGKILL, then in a second run SIGABRT, from a `--pid=host` container | Tenant, deliberate | `Error` 137 and `Error` 2; the job fails with the runner alive |
| R7 | Under Kata: the sidecar's `memory.oom.group` as the guest sees it, R1, then the job's containers past the sidecar's 8Gi | Tenant, honest | `memory.oom.group` 0, since Kata 3.32.0's agent does not set it (read). R1: `OOMKilled`, 143. Second: one process dies, and the sidecar reads `OOMKilled` with 143 if it was the job's, or 137 if it was dockerd |

R4 and R5 are not evidence of what happens in the field, because the victim is made the target.
They exist because R2 and R3 are expected not to reach the sidecar at all, and question 2 needs a sidecar that was killed.

`kubepods.slice`'s counters are the node's since it started, so every reading is the rise across its own window, and the first sample of each is its baseline.

Read for every one: the victim pod's statuses as the API server serves them (`kubectl get pod -w -o json`, started first), Events in the tenant's namespace and on the Node, both jobs' samples, the AGC's `scale-set job failed` lines and the `scaleset-failures-ci-e2e` ledger, the job's conclusion, and the time of each.

## What each outcome means

| Outcome | Conclusion |
|---|---|
| R3's runner dies first in 3 of 3 | A node out of memory is not this class: the pod fails and the arms that exist see it. Memory leaves the class under `privileged-dind`, as it already has under Kata |
| R3's sidecar dies with the runner alive, in any run | The class has a platform cause. R4 against R5 decides whether it has a discriminator |
| R4 and R5 read the same in the pod's and `kubepods.slice`'s counters | No discriminator for memory; not buildable without changing where a job's containers are charged |
| R4 and R5 differ as predicted | A discriminator exists for a neighbour **pod**. It does not cover a neighbour worker's own containers, which are charged outside `kubepods.slice` as the job's are, so it is complete only once Q1396 moves them |
| R6 matches the local statuses | `Error` stays a status any job can produce; a class keyed on it needs a platform cause, and none is known |
| R7's first is not `OOMKilled`, or exits 137 | Q1395's rule is wrong for Kata and is refiled |
| R7's second reads `OOMKilled` with 143 | A job's container killed at the sidecar's own limit reads as one killed at its own, which Q1395's rule does not name; the rule's documented miss is confirmed |
| R2's or R3's runner reads `OOMKilled` and the AGC withholds or raises | Q1400 is reproduced, and its counters say whether `kubepods`' `oom` could have told |

**Not buildable**, and Q1316 is punted with that stated: R3 runner-first in all runs, and no platform cause for the `Error` shapes, which the local runtime-loss readings already point to.

**Buildable** needs all three: R3 kills the sidecar with the runner alive; R4 and R5 differ in a counter; and a way to carry the counter to the AGC.
The carrier would be the template's entrypoint, which on its restart reads the pod's `memory.events` and writes the verdict to its termination message, which the AGC reads with the status it already reads.
Two costs come with it: the sidecar must see the host's cgroup tree, which only `privileged-dind` does, and the message is written by a container the job controls, so R9's "nothing tenant code writes" needs a ruling first.

## Mutations

Environment, every command: `PROJECT=actions-gateway-dogfood CLUSTER=gag-dogfood ZONE=us-east1-b REPO=actions-gateway/github-actions-gateway`, and `CTX=gke_actions-gateway-dogfood_us-east1-b_gag-dogfood`.
The values are the runbook's ([gke-dogfood.md](gke-dogfood.md#variables)) and are re-read against the active project before the first one.

| # | Command | Changes |
|---|---|---|
| 1 | `E2E_VARIANT=dind E2E_ROUTE_VAR=0 scripts/dogfood/e2e-start.sh` | System pool to its running size, the `dind` overlay (namespace labels, template, open egress policy, gateway), registry mirrors up |
| 2 | `git push origin HEAD:claude/q1316-probe-run`, one push per reading | One workflow run; the e2e pool scales 0 to 1 |
| 3 | `kubectl --context $CTX -n gag-dogfood-e2e apply -f hog.yaml`, R3 and R4 only | One unprivileged pod: `nodeSelector` and toleration for the e2e pool, `alpine`, 16Gi requested, `activeDeadlineSeconds: 120` |
| 4 | `kubectl --context $CTX -n gag-dogfood-e2e delete pod q1316-hog` | Removes it |
| 5 | `E2E_VARIANT=kata E2E_ROUTE_VAR=0 scripts/dogfood/e2e-start.sh` | Back to the `kata` overlay, and deletes the open egress policy |
| 6 | `GAG_ROUTE_VARS=0 scripts/dogfood/e2e-stop.sh` | Deletes the gateway, scales mirrors to 0, system pool back |
| 7 | `scripts/dogfood/ops.sh pool-scale default-pool <count read before step 1>` | Only where step 6 left another count |
| 8 | `git push origin --delete claude/q1316-probe-run` | Removes the branch |

`start.sh` is not used: it dispatches unit and integration runs onto the cluster.
No repository variable, Secret, CRD, chart or AGC image changes.
The App Secret in the namespace is used by the AGC as it is, and is not read.

**Before step 1**, read-only: the active context and project, `scripts/dogfood/ops.sh at-rest`, each pool's node count, the release lease (`kubectl --context $CTX -n default get lease actions-gateway-release-gate`), pods in `gag-dogfood-e2e`, and open runs on `gag-ci-e2e`.
Any lease, worker or queued run there means the cluster is taken: stop before step 1.

## Abort

Stop, run the restore, and report what was read, on any of:

- another session's job or the release gate reaches the e2e set;
- the e2e node is `NotReady` for 5 minutes, or a pod other than the two workers, the plain pod or a DaemonSet's is on it;
- a container on any other node restarts during a reading;
- Spot preemption twice;
- 3 hours from step 1.

R2, R3, R4 and R5 exhaust the e2e node on purpose.
The pool's taint keeps other workloads off it, it is Spot and disposable, and its DaemonSets restart.
The system pool is a different pool.

## Restore

1. Delete the plain pod, and cancel any probe run.
2. Step 5, which leaves the tenant on its default overlay, then step 6.
   Where step 5 itself fails, `kubectl --context $CTX -n gag-dogfood-e2e delete networkpolicy e2e-open-egress` by hand, since `e2e-stop.sh` leaves it.
3. Step 7 and step 8.
4. `scripts/dogfood/ops.sh at-rest`, compared with the reading taken before step 1; the e2e pool scales to 0 on the autoscaler's schedule, so it is read again 20 minutes later.

## Time and cost

About 2 hours with the cluster held: 25 minutes to start and read the baseline, 60 for R0 to R6, 20 for the switch to Kata and R7, 15 to restore.
R6 and R7's job ends inside minutes; a runner that dies holds its job about 10 minutes, until the lock lapses ([M1](q555-automatic-recovery-design.md#readings-from-the-dogfood-turn-up-2026-11-22)), and R2 and R3 are expected to be six of those, run two at a time where the pair allows.

Estimated from third-party price listings read on 2026-11-24 (Google's own page was not read), over 3 hours, the abort limit:

| Item | Rate | Estimate |
|---|---|---|
| One `n2-standard-8` Spot (two in the worst case) | $0.138 an hour ([listing](https://www.devzero.io/instances/gcp/n2-standard-8)); $0.388 on demand | $0.41 to $0.83; $2.33 at the on-demand rate for two |
| Two `e2-standard-2` system nodes | $0.067 an hour each ([listing](https://www.devzero.io/instances/gcp/e2-standard-2)) | $0.40 |
| Boot disks and one 100Gi volume for the Kata worker | not priced | cents, unverified |

So under $2, and under $4 at the on-demand rate.
The cluster's own fee is paid either way.
