# Which memory limit killed a container

> **Audience:** Platform engineer, Tenant operator

A container killed for memory reads `OOMKilled`, whichever limit ran out: its own, its pod's, the one all of a node's pods share, or the node's.
The oom-reporter says which one, as a condition on the pod and an Event beside it.
It is a chart of its own, `oom-reporter`, and needs nothing of the gateway's.

It names the limit, and not whose memory filled it.
A job and its neighbours can each fill the limit the node's pods share, so `PodsLimit` on a runner says where to look, and not that the job is to blame.

## What a pod gets

A condition of type `oom.actions-gateway.com/<container>`, and an Event whose reason is `OOM<reason>`, with the same words:

| Reason | What ran out |
|---|---|
| `ContainerLimit` | The container's own memory limit, or those of the pod's containers together |
| `PodLimit` | The pod's own limit (`spec.resources`) |
| `PodsLimit` | The limit all of the node's pods share, or that of the pod's QoS class |
| `AbovePods` | Memory above the node's pods: on an ordinary node, the node's own |
| `Unknown` | Not known from the counters. The condition's status is `Unknown` |

A kill that ended no container, such as a build's child process, gets the Event `OOMProcessKilled` alone.

Read the condition of a pod's `runner` container:

```bash
kubectl get pod <pod> -n <namespace> \
  -o jsonpath='{.status.conditions[?(@.type=="oom.actions-gateway.com/runner")]}'
```

Read the Events:

```bash
kubectl get events -n <namespace> --field-selector involvedObject.name=<pod>
```

A pod that is deleted takes its condition with it, and an Event expires after an hour by default.

## What the gateway does with it

A failed job's `PlatformFaultOnFailedJob` Event and its record repeat a worker's reason, whatever the gateway is set to.
Acting on it is a setting on the `ActionsGateway`, off by default:

```bash
kubectl patch actionsgateway <name> -n <namespace> \
  --type merge -p '{"spec":{"oomKillReasons":"Act"}}'
```

Under `Act`, a runner killed for memory waits up to 30 seconds after its pod ends for the runner's condition, and then:

| Reason | What the AGC does |
|---|---|
| `ContainerLimit` | What it does with no reporter: [raises the limit and re-runs](worker-rightsizing.md#when-a-job-runs-out-of-memory), or withholds; its Events now say the container's limit ran out |
| `PodLimit` | Raises nothing and re-runs nothing. A Normal `EvictionRerunWithheld` Event says the pod's limit ran out, and to raise it in the RunnerTemplate |
| `PodsLimit`, `AbovePods` | Raises nothing and re-runs nothing, and the kill does not count toward the retry pause. The Event says memory ran out above the pod; re-run the job by hand |
| `Unknown`, or no condition in 30 seconds | What it does with no reporter |

The two withholds count in `actions_gateway_eviction_rerun_withheld_total{cause="oom", reason="above_container"}`.
Changing the setting restarts the AGC.

Set `Act` only where this chart is installed and you trust who can write pod status in the tenant's namespace.
Anyone who can write a pod's status can write the condition, and under `Act` a false `PodsLimit` withholds a re-run the job would have had; it cannot get a job a raise it would not have had.

## Installing it

The [chart's README](https://github.com/actions-gateway/github-actions-gateway/blob/main/charts/oom-reporter/README.md) has the install command, the values, and what an install that fails partway leaves.
It runs an agent on every node, which reads the node's cgroup counters through a read-only `hostPath`, and one controller, which writes the conditions and Events.
Three admission policies hold each to less than its grants, and no value turns them off.

It is off unless installed.
It adds a `hostPath` pod on every node, a namespace at the Pod Security level `privileged`, and write access to the status of pods in the namespaces it is given.

## Where its answers have been checked

Each answer comes from a rule over the cgroup counters, and the rule's tests hold it to the limit the kernel's own line names for the same kill.

| Where | Kernel | Limits met |
|---|---|---|
| A local container | 6.12.76 | A container's, a pod's, the pods' |
| kind, two workers | 6.12.76 | A container's, a pod's, the pods', above the pods |
| GKE, four filled nodes and one runner at its own limit | 6.8.0-1064-gke | The pods', a container's |

On GKE, a kill at the pods' limit also took processes of other pods than the job's: the node's metrics collector, in every run.
Those containers kill one process and not their whole group, so the counters cannot tell such a kill from a neighbour's: of the collector's eight kills, as a watch of the files read them, six were answered `Unknown`, one `PodsLimit`, and one not at all.
The job's runner was answered with the right limit in all five runs.

What GKE has not shown is a node filling up (`AbovePods`), a pod-level limit, or the cap of a pod with one container.
The agent itself did not run on GKE: the counters were recorded there, and the rule replayed them.
On kind, the repository's CI installs this chart and kills a container at each of the four limits, on every merge that changes the reporter, and requires the right condition and Event for each.
The replay assumes every container's `cgroup.stat` shows nothing below it, and takes `memory.oom.group` from whether a container's counters show a group kill, since the recording did not read it.

## What it does not cover

| Workload | What it says |
|---|---|
| A `privileged-dind` job's own containers | Nothing when one of those is killed at its own limit, since it is outside every pod. When they fill the node, the runner's kill reads `AbovePods`, or `PodsLimit` under a `--cgroup-parent` |
| Kata | Nothing for a kill inside the guest. The virtual machine killed by the host reads as any other pod's kill |
| A cgroup v1 node | Unsupported |
| GKE Autopilot | Expected not to admit the agent's `hostPath` |
