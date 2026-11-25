# oom-reporter

Says on a pod which memory limit ran out when one of its containers was killed for memory.
`OOMKilled` alone does not: the kubelet shows it for the container's own limit, the pod's, the one all pods on the node share, and the node itself.

It stands alone.
It needs nothing of the actions-gateway charts, and they neither contain it nor need it.

## What a pod gets

A condition of type `oom.actions-gateway.com/<container>`, and an Event with the same words under `OOM<reason>`:

| Reason | What ran out |
|---|---|
| `ContainerLimit` | The container's own memory limit, or those of the pod's containers together |
| `PodLimit` | The pod's own limit (`spec.resources`) |
| `PodsLimit` | The limit all of the node's pods share, or those of the pod's QoS class |
| `AbovePods` | Memory above the node's pods: on an ordinary node, the node's |
| `Unknown` | Not known from the counters. The condition's status is `Unknown` |

The reason names the limit, and not whose memory it was: a job and its neighbours can each fill the limit all pods share, or the node.

A kill that ended no container, such as a build's child process, gets the Event `OOMProcessKilled` alone.

## What runs, and what each may do

| | Agent, on every node | Controller, one |
|---|---|---|
| Reads | `/sys/fs/cgroup`, read-only | The reports, and pods in the namespaces you list |
| Writes | Its node's report, one ConfigMap in this namespace | Pod conditions of its own domain, and Events, in the namespaces you list |
| Runs as | Not root, no capability, read-only root filesystem, a `hostPath` volume | The same, with no `hostPath` |

Three ValidatingAdmissionPolicies hold each to less than its RBAC grants, and no value turns them off.
An agent writes only the report of the node its token names, and that holds no label, no annotation and no other data.
The controller makes nothing but reports, changes nothing of a pod but conditions of its own domain, and its Events carry only its own reasons.
The policies on the controller are evaluated for every write of a pod's status and every Event in the cluster, by one comparison of the writer's name that refuses nobody else.

## Install

**Give it a namespace that tenants cannot use**, at the Pod Security level `privileged`, which the agent's `hostPath` needs.
Whoever has `edit` in a namespace can act as its service accounts.

```bash
kubectl create namespace oom-reporter
kubectl label namespace oom-reporter pod-security.kubernetes.io/enforce=privileged
```

**Keep the controller off nodes that run tenants' jobs**, where a job that escapes its sandbox could take its token.
By default it needs a node with a label that a kubelet may not set on its own node:

```bash
kubectl label node <a platform node> node-restriction.kubernetes.io/oom-reporter=controller
```

Where your node pools cannot carry such a label, set `controller.placement=Taints`, and keep a taint on every pool that runs tenants' jobs.

**Set the pods' root for your nodes' cgroup driver** (`agent.podsRoot`, below).
The agent exits and says so where it is not there.

```bash
helm install oom-reporter oci://ghcr.io/actions-gateway/charts/oom-reporter --version <version> \
  --namespace oom-reporter --rollback-on-failure \
  --set "namespaces={team-a,team-b}" \
  --set image.digest=sha256:<digest of the release's oomreporter image>
```

**Keep `--rollback-on-failure`** (`--atomic` in Helm 3).
Helm makes the admission policies last, after the grants and the pods, so an install that fails there leaves the grants without them.
The controller does nothing until the policies refuse it, and no agent has a report to write until it does.
One whose controller keeps exiting with `the admission policies are not in force` is such an install: uninstall it, since until then the grants are all that holds a token of either component.
One that says it `cannot tell` was not answered, or was refused by something else, and says what: `networkPolicy.apiServerCIDRs` that do not hold the API server, for one.
It looks once, as it starts, and for the policy on the reports alone: it does not see one of the others missing, or any deleted later.

**A namespace it may not read is left out**, and the others are not.
Whoever administers a tenant's namespace can delete its RoleBinding, and deleting the namespace does the same.
The controller's log names it, and it is taken in within five minutes of being readable.

The chart requires Kubernetes ≥ 1.35 and cgroup v2.
`helm template` has no cluster to ask, so it checks the first against the version Helm was built for: pass `--kube-version <your cluster's version>` where that is older.

**A change of the cluster's encryption keys** that rewrites every stored ConfigMap is refused on the reports, as any other writer is.
Delete the `oom-report-` ConfigMaps before it, and the controller makes them again.

## Values

| Key | Default | Description |
|---|---|---|
| `namespaces` | `[]` | Namespaces whose pods are read and written to. Required, unless `allNamespaces` is set, and none the release's own. |
| `allNamespaces` | `false` | Every namespace instead, by a ClusterRole. Whoever takes over the controller can then read every pod spec in the cluster. |
| `image.{repository,tag,digest}` | `ghcr.io/actions-gateway/oomreporter`, `""`, `""` | The one image both run. Rendering fails without the digest, unless `allowFloatingImageTags=true`. |
| `imagePullPolicy` | `IfNotPresent` | Pull policy of both. |
| `imagePullSecrets` | `[]` | Pull Secrets in the release's namespace. |
| `allowFloatingImageTags` | `false` | Dev and test only: render `image.tag` where no digest is set. |
| `agent.podsRoot` | `kubepods.slice` | The kubelet's cgroup of all pods, below `/sys/fs/cgroup`. `kubepods.slice` for the systemd driver, `kubepods` for cgroupfs, `kubelet.slice/kubelet-kubepods.slice` on kind. Fixed and not found, because a job with the host's view of the cgroup tree can make a second one. |
| `agent.nodeSelector`, `agent.affinity` | `{}` | Where the agent runs. A node without it gets no answer. |
| `agent.tolerations` | every taint | As above. |
| `agent.priorityClassName` | `""` | |
| `agent.resources` | 50m and 64Mi, limit 128Mi | |
| `controller.placement` | `NodeLabel` | `NodeLabel` requires `controller.nodeLabel`. `Taints` requires nothing, and refuses `controller.tolerations`. |
| `controller.nodeLabel.{key,value}` | `node-restriction.kubernetes.io/oom-reporter`, `controller` | The label. Its key has to be in the `node-restriction.kubernetes.io` namespace. |
| `controller.nodeSelector` | `{}` | |
| `controller.tolerations` | `[]` | |
| `controller.priorityClassName` | `""` | |
| `controller.resources` | 20m and 64Mi, limit 256Mi | |
| `networkPolicy.enabled` | `true` | No ingress to either, and egress to the API server's ports alone. Needs a CNI that enforces NetworkPolicy. |
| `networkPolicy.apiServerCIDRs` | `[]` | Narrows that egress to these addresses. |
| `networkPolicy.apiServerPorts` | `[443, 6443]` | The API server's ports, before and after its Service's address is rewritten. |
