# Q1316: the dogfood reproduction, planned

> **Nothing here has been run.** The plan needs a grant for the prod-classified dogfood cluster, read-only checks included.
> Local readings it builds on are in [the design](q555-automatic-recovery-design.md#readings-for-the-dockerd-sidecar-class-q1316).

## What it decides

Q1316's retry class needs a fact that separates a platform's crash of the Docker-in-Docker sidecar from the job's own, and a platform cause that leaves the runner alive to fail the job.
From the kernel's scoring, at the shipped requests the runner is expected to go first and the class to have no memory case (computed below); the run confirms or refutes that.
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
A debug pod would add the node's process list and its kernel log, which name the killed process; the container statuses and each container's own counters name the container, which is the question asked.
The trade is that the privilege stays the one this tenant already has.
Where the statuses and the counters do not name the victim, the reading is reported as unresolved rather than widened to a debug pod.
The kubelet's `SystemOOM` Event is read beside them and not relied on: it is raised where the kernel's line names no memory cgroup, which a node-wide kill's does not and a kill at `kubepods`' limit does (kernel 6.12 `memcontrol.c`, kubelet 1.35.5 and its cAdvisor `oomparser.go`, read; the pattern run over both lines in their source format).
So it should appear for R2 and R5 and not for R3 and R4, naming a process and not a pod.

## Kill order, computed

The kernel kills the process with the most of: resident memory, plus `oom_score_adj` thousandths of the limit that was hit (`oom_kill.c`, v6.12).
On a 32 GiB node with 31.4 GiB usable and `kubepods` near 28 GiB (both from recall, and R0 reads them):

| Process | `oom_score_adj` | Score at a `kubepods` kill | Score at a node-wide kill |
|---|---|---|---|
| A BestEffort pod's | 1000 | 28.0 GiB plus its own | 31.4 GiB plus its own |
| A Burstable DaemonSet container requesting 512Mi or less, not node-critical | 985 or more | 27.6 GiB or more | 30.9 GiB or more |
| The runner's largest | 969 | 27.1 GiB plus its own | 30.4 GiB plus its own |
| dockerd, at the shipped requests (1Gi runner, 3Gi sidecar) | 907 | 25.4 GiB plus its own | 28.5 GiB plus its own |
| One plain-pod filler of 4 GiB, pod requesting 12Gi | about 618 | 17.3 + 4 GiB | 19.4 + 4 GiB |
| One job-container filler of 4 GiB | 0 | 4 GiB | 4 GiB |

So a filler never outscores a worker while each of its processes stays at 4 GiB, and dockerd goes first at the shipped requests only with 1.7 GiB more resident than the runner.
At a runner request of 3Gi or more the two tie, and the larger process goes.

**The fillers stop at the first kill of a worker.** Other pods on the node can outscore one (the table's first two rows; a node-critical pod is -997 and cannot), so their kills are passed over and logged.
Each process takes 2 MiB at a time and exits when either worker's `oom_kill` rises, so one reading is one worker's kill and the second worker survives to log it.
Run locally on two workers and two higher-scored pods under one 700 MiB `kubepods.slice`, with `oom.group` set: one worker killed, the other untouched and no filler killed in 10 of 10 runs, the higher-scored pods at 20 and 100 MiB.
An earlier version with 64 MiB steps overshot and killed a second process in 3 of 3, so the step stays small.
A restarted DaemonSet container is not modelled, and can be killed again.
The scripts are at [the end](#the-probe).

## Shape

One e2e node, two workers on it at once, on a throwaway branch `claude/q1316-probe-run` whose workflow runs only on a push of that branch with `[q1316-probe]` in the commit message, has `permissions: {}` and checks nothing out.

Both jobs sample the host's cgroup tree every second to their logs: each pod's `memory.current`, `memory.events` and `memory.events.local`, the same and `memory.max` for `kubepods.slice`, `/sys/fs/cgroup/docker`'s `memory.current`, and `/proc/vmstat`'s `oom_kill`.
The two pods are scored alike, so a kill can take either; the survivor's log holds both pods' counters, and is the record: the fill's own summary line is lost where its worker is the one killed.
Both do the same actions, except the fill, which one of them starts.
Each job ends in failure once its sampler ends, after 2.5 minutes for R0, R1, R6 and R7 and 8 for a fill, so its end is observable.
R1, R6 and R7 use one worker; R8 uses the pair, so the other worker's log survives the kill.

Both are ordinary `gag-ci-e2e` jobs, at the template's 4Gi of memory requested and 7Gi of limits, and 2.5 CPU requested after `NodeShare`'s sizing, so two fit one `n2-standard-8`.
A job does nothing on a run attempt above 1, so a re-run an arm makes of it repeats no reading.
If the two land on different nodes, the reading is void and repeated once.

## Readings

"Predicted" is from source or the local readings.
R0 is a gate: where a job cannot read `memory.events` of `kubepods` and of another pod, does not find exactly two workers by their sidecar's 4Gi `memory.max`, or reads the `oom_score_adj` of a runner's `Runner.Listener` or `Runner.Worker`, or of dockerd, more than 10 from the table above (a step may be at 500, which the runner asks for), the run stops and the plan is recomputed.
R0 also reads, by read-only `kubectl`, the QoS class, requests and priority class of every pod on the node, and lists the ones whose computed score is above a runner's.

| ID | Runs | What is done | Platform or tenant | Predicted |
|---|---|---|---|---|
| R0 | 1 | Nothing: cgroup paths; `oom_score_adj` and resident memory of dockerd, the runner's processes and a nested container; node memory, kernel, `memory.oom.group`, `kubepods.slice`'s `memory.max` | | Nested containers outside the pod; 969, 907, 0; `kubepods` capped. Also answers Q1396's first step |
| R1 | 1 | A job runs a container past its own `-m 64m` | Tenant, honest | `Completed`, exit 0; nothing raised |
| R6 | 2 | A job sends dockerd SIGKILL, then in a second run SIGABRT, from a `--pid=host` container | Tenant, deliberate | `Error` 137 and `Error` 2; the job fails with the runner alive |
| R8 | 1 | One worker's step allocates 4 GiB, past the runner's own 3Gi limit | Tenant, honest | The runner `OOMKilled`, 137; pod `Failed`; withheld as `own_limit`, as R2 is; pod `oom` 1, `kubepods.slice` `oom` 0, no `SystemOOM` |
| R2 | 1 | A job's own container fills the node: eight 4 GiB processes | Tenant, honest | One runner dies; pod `Failed`; the AGC withholds as `own_limit`, which is Q1400's reading; `kubepods.slice` `oom` 0; a `SystemOOM` Event |
| R3 | 2 | A plain pod on the same node, 12Gi requested and no limit, fills: seven 4 GiB processes | Platform: a neighbour over its request | One runner dies; `kubepods.slice` `oom` above 0; no `SystemOOM` |
| R3t | 2 | R3, with each worker's dockerd set to its runner's `oom_score_adj`, the tie a 3Gi runner request gives | Platform, tie | The larger process of the two goes, by R0's resident memory |
| R4 | 1 | R3, with each dockerd at 1000 | Platform, forced | dockerd `OOMKilled`, 137; runner alive; pod `oom` 0 |
| R5 | 1 | R2, with the same | Tenant, forced | As R4, with `kubepods.slice` `oom` 0 where R4's rose |
| R7 | 1 | Under Kata, one worker: the sidecar's `memory.oom.group` as the guest sees it, R1, then the job's containers past the sidecar's 8Gi | Tenant, honest | `memory.oom.group` 0, since Kata 3.32.0's agent does not set it (read). R1: `OOMKilled`, 143. Second: one process dies, and the sidecar reads `OOMKilled` with 143 if it was the job's, or 137 if it was dockerd |

They run in the order listed.
**R4 and R5 run only if** R3 or R3t killed a sidecar with its runner alive, because otherwise no question needs a killed sidecar's counters.

`kubepods.slice`'s counters are the node's since it started, so every reading is the rise across its own window, and the first sample of each is its baseline.

Read for every one: the victim pod's statuses as the API server serves them (`kubectl get pod -w -o json`, started first), Events in the tenant's namespace and on the Node, both jobs' samples, the AGC's `scale-set job failed` lines and the `scaleset-failures-ci-e2e` ledger, the job's conclusion, and the time of each.

## What each outcome means

| Outcome | Conclusion |
|---|---|
| R3's runner dies first in 2 of 2 | At the shipped requests a node out of memory is not this class. The pod fails, and what the AGC does with it is Q1400's misreading, not a recovery |
| R3t's sidecar dies with the runner alive | The class has a platform cause on a template that requests 3Gi or more for its runner, and R4 against R5 decides whether a counter separates it |
| R3t's runner dies in 2 of 2 | The tie is decided by which process is larger in that job; R0's figures say by how much, and no general claim is made |
| R4 and R5 read the same in the pod's and `kubepods.slice`'s counters | No discriminator for memory |
| R4 and R5 differ as predicted | A fill by pods is separated from a fill by a job's own containers, and that is not yet a neighbour against the job: the job's own pod over its request raises `kubepods`' `oom` too, so a class needs the pod's usage at or under its request beside it. The shipped limits let two workers use 14Gi, which does not reach `kubepods`' limit, so that twin is not run |
| The kubelet evicts a worker or the plain pod before any kill | The kill order is not read by that run; it is repeated once. Two such runs mean eviction comes first on this node, which the existing arms classify, and R3's question stays unread |
| Another pod is killed before a worker | Expected where R0 lists it; the fill goes on and the kill is logged. It says nothing about the two workers' order |
| A worker and another pod die in one window | The samples give the order to the second; where they do not, the statuses' `finishedAt` does |
| No kill of a worker and no eviction | The fill did not reach the limit; repeated once at two more processes, then reported unread |
| R6 matches the local statuses | `Error` stays a status any job can produce; a class keyed on it needs a platform cause, and none is known |
| R7's first is not `OOMKilled`, or exits 137 | Q1395's rule is wrong for Kata and is refiled |
| R7's second reads `OOMKilled` with 143 | A job's container killed at the sidecar's own limit reads as one killed at its own, which Q1395's rule does not name; the rule's documented miss is confirmed |
| R2's or R3's runner reads `OOMKilled` and the AGC withholds or raises | Q1400 is reproduced, with the counters and the Event that could have told |

**Not buildable at the shipped requests**, with that bound stated: R3 runner-first in both runs, and no platform cause for the `Error` shapes, which the local runtime-loss readings already point to.

**Buildable** needs all three: a platform fill kills the sidecar with its runner alive; R4 and R5 differ in a counter; and something carries the node's counters to the AGC.
No carrier is known.
The template's entrypoint is not one: on its restart it can read its own pod's counter, but the fact is `kubepods`' and a rise over a baseline, and a termination message is published when the restarted sidecar stops, after the job's completion.
A node agent could, which is a new privileged component, and a message a container the job controls could write meets R9's "nothing tenant code writes".
So a run in which R4 and R5 differ leaves the class to a design decision rather than building it.

## What it decides for Q1400

R2 and R3 are the kill [Q1400](../queue/Q1400.md) describes, a runner killed for the node's memory, so one window serves both.
They settle whether the pod reads `Failed` with the runner `OOMKilled` at the API server, what the AGC then says and does, and when its `PlatformFaultOnFailedJob` comes.
R8 is the kill they are told apart from, a runner past its own limit, and the AGC is expected to treat it alike.
[M3](q555-automatic-recovery-design.md#readings-from-the-dogfood-turn-up-2026-11-22) read that kill's status and not its counters or Events.
Each container's `current` and `peak` beside its `max` say how far below its own limit a node-killed runner was, which is the claim Q1400 makes.
What separates them is expected to be the pod's `oom`, 1 for R8 and 0 for R2 and R3; `kubepods`' `oom` then tells R3 from R2, and the `SystemOOM` Event R2 from the rest.
None of those reaches the AGC today, so the readings say what honest wording can rest on and what a class would need.
The set is `NodeShare`, so what is read is the withheld `own_limit` branch.
Not read: a raise under `Binpack` or `Throughput`, which starts from the same two facts, and a runner with no limit.

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
| 3 | `kubectl --context $CTX -n gag-dogfood-e2e create configmap q1316-fill --from-file=fill.py`, once, then `apply -f hog.yaml` with `NODE` set, per R3, R3t and R4 | One ConfigMap, and one non-root, unprivileged pod with the host's `/sys/fs/cgroup` mounted read-only, which is how it sees a worker die |
| 4 | `kubectl --context $CTX -n gag-dogfood-e2e delete pod/q1316-hog` after each, `configmap/q1316-fill` after the last | Removes them |
| 5 | `E2E_VARIANT=kata E2E_ROUTE_VAR=0 scripts/dogfood/e2e-start.sh` | Back to the `kata` overlay, and deletes the open egress policy |
| 6 | `GAG_ROUTE_VARS=0 scripts/dogfood/e2e-stop.sh` | Drains, deletes the gateway, scales mirrors to 0, system pool to its running size |
| 7 | `GAG_ROUTE_VARS=0 scripts/dogfood/stop.sh`, only where the system pool read 0 before step 1 | Drains every tenant's workers, then system pool to 0 |
| 8 | `kubectl --context $CTX delete clusterrunnertemplate privileged-dind`, only where it was absent before step 1 | Removes the template step 1 created |
| 9 | `git push origin --delete claude/q1316-probe-run` | Removes the branch |
| 10 | [Release the lease](#the-lease) | Deletes the Lease |

`start.sh` is not used: it dispatches unit and integration runs onto the cluster, and `e2e-start.sh` resizes the system pool and waits for the GMC itself.
A system pool brought up from 0 runs the always-on gateways, so a job already queued for them would be served.
No repository variable, Secret, CRD, chart or AGC image changes.
The App Secret in the namespace is used by the AGC as it is, and is not read.

**Between steps 0a and 0b**, read-only: the active project, `scripts/dogfood/ops.sh at-rest`, each pool's node count, the release lease (`kubectl --context $CTX -n default get lease actions-gateway-release-gate`), whether `clusterrunnertemplate/privileged-dind` exists, pods in `gag-dogfood-e2e`, and open runs on `gag-ci-e2e`.
Any lease, worker or queued run there means the cluster is taken: stop, with nothing taken.
After 0b the pods and the queued runs are read once more, since something can start between; a stop there runs step 10 and nothing else.

prod-guard, the hook that checks an agent session's commands, refuses a direct mutating `kubectl` on this cluster, which steps 0b, 3, 4, 8 and 10 and the restore's NetworkPolicy deletion are and the scripts are not.
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
Before each of steps 1 to 8, this exits 0, which an empty holder or an empty read cannot make it do:

```bash
H="$(cat tmp/q1316/holder)"
GOT="$(kubectl --context "$CTX" -n default get lease actions-gateway-release-gate -o jsonpath='{.spec.holderIdentity}')"
[[ -n "$H" && "$GOT" == "$H" ]]
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
So before each of steps 1, 2, 3 and 5 the pods in `gag-dogfood-e2e`, each pool's node count and the runs queued on `gag-ci-e2e` are read as well, and anything this run did not make is an abort.
A gate started in the window refuses, and a candidate tagged in it fails its validation and needs a re-run.

## Abort

Stop, run the restore, and report what was read, on any of:

- another session's job reaches the e2e set, or the lease is gone or another's;
- the e2e node is `NotReady` for 5 minutes, or a pod other than the two workers, the plain pod or a DaemonSet's is on it;
- a container on any other node restarts during a reading;
- Spot preemption twice;
- 4 hours from step 1, which leaves the restore inside 4.5.

R2, R3, R4 and R5 exhaust the e2e node on purpose.
The pool's taint keeps other workloads off it, and it is Spot and disposable.
Its DaemonSets' containers are expected to be killed and restart, the metrics collector among them, so the node's own monitoring has gaps during a fill; that is not an abort.
The system pool is a different pool.

## Restore

1. Delete the plain pod and its ConfigMap, and cancel any probe run.
   Where the lease reads another holder, stop here and report: the cluster is that holder's, and the rest would tear down what it is using.
   Where there is no lease, take it again; if that fails, the same.
2. Step 5, which leaves the tenant on its default overlay, then step 6.
   Where step 5 itself fails, `kubectl --context $CTX -n gag-dogfood-e2e delete networkpolicy e2e-open-egress` by hand, since `e2e-stop.sh` leaves it.
3. Where step 6's drain times out it stops before deleting the gateway: read what it reports, cancel the probe run it names and re-run it.
   `SKIP_E2E_DRAIN=1` is not used, and a worker that is not this run's is left to finish.
4. Steps 7 and 8 where they apply, and step 9.
   Where step 7's drain times out the system pool stays up; that is reported with what is running, and not forced.
5. `scripts/dogfood/ops.sh at-rest`, compared with the reading taken before step 1; the e2e pool scales to 0 on the autoscaler's schedule, so it is read again 35 minutes later.
6. Step 10, once the pools are back.
   Where a drain did not finish the lease is kept, as the gate's teardown keeps its own, and the report says so.

## Time and cost

Estimated at 2 hours 45 minutes to 3 hours 15 with the cluster held: 25 minutes to start and read the baseline, 30 for R0, R1 and R6, R8 and 5 fills at about 12 minutes each, 2 more if R4 and R5 run, 20 for the switch to Kata and R7, 20 to restore.
A fill is 12 because a runner that dies holds its job about 10 minutes, until the lock lapses ([M1](q555-automatic-recovery-design.md#readings-from-the-dogfood-turn-up-2026-11-22)), each uses both worker slots, and none overlaps another.

Estimated from third-party price listings read on 2026-11-24 (Google's own page was not read), over 4.5 hours, the abort limit and the restore:

| Item | Rate | Estimate |
|---|---|---|
| One `n2-standard-8` Spot (two in the worst case) | $0.138 an hour ([listing](https://www.devzero.io/instances/gcp/n2-standard-8)); $0.388 on demand | $0.62 to $1.24; $3.49 at the on-demand rate for two |
| Two `e2-standard-2` system nodes | $0.067 an hour each ([listing](https://www.devzero.io/instances/gcp/e2-standard-2)) | $0.60 |
| Boot disks and one 100Gi volume for the Kata worker | not priced | cents, unverified |

So under $2, and about $4.10 at the on-demand rate, with nothing priced for disks.
The cluster's own fee is paid either way.

## The probe

The commit pushed to `claude/q1316-probe-run` adds one file, `.github/workflows/q1316-probe.yml`, and its message is `[q1316-probe] <ID>`, with ` solo` after the ID for R1, R6 and R7.
R6 is `R6a` and `R6b`, and R7 is `R7a` and `R7b`.
`bash -n`, `shellcheck -S warning` and `actionlint` pass on it but for the runner label it does not know and an info note that single quotes do not expand, which is meant.
It has not run on GitHub.

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
          set -uo pipefail
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
          until docker info >/dev/null 2>&1; do sleep 1; done
          docker pull "$I"
          host() { docker run --rm -i --privileged --pid=host -v /sys/fs/cgroup:/h:ro "$I" "$@"; }
          case $R in R0|R1|R6*|R7*) secs=150 ;; *) secs=480 ;; esac
          host python3 - "$secs" < sample.py &
          own=$(cat /proc/1/oom_score_adj)
          case $R in
            R3t) host sh -c "echo $own > /proc/\$(pidof dockerd)/oom_score_adj" ;;
            R4|R5) host sh -c 'echo 1000 > /proc/$(pidof dockerd)/oom_score_adj' ;;
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
            R2|R5) if [[ $W == a ]]; then host python3 - 8 4096 45 < fill.py; fi ;;
          esac
          wait
          exit 1
```

`@SAMPLE@` is `sample.py`:

```python
import glob, sys, time

def rd(path):
    try:
        with open(path) as f:
            return " ".join(f.read().split())
    except OSError as e:
        return type(e).__name__

end = time.time() + int(sys.argv[1])
while time.time() < end:
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
      command: [python3, /f/fill.py, "7", "4096", "5"]
      resources: { requests: { memory: 12Gi } }
      securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: [ALL] }, readOnlyRootFilesystem: true }
      volumeMounts:
        - { name: fill, mountPath: /f, readOnly: true }
        - { name: cgroup, mountPath: /h, readOnly: true }
  volumes:
    - { name: fill, configMap: { name: q1316-fill } }
    - { name: cgroup, hostPath: { path: /sys/fs/cgroup, type: Directory } }
```
