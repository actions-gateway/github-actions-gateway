# Q727: A Non-Privileged Path for `container:` and `services:` Steps

> **Status: decided 2026-08-25.
> ARC's mechanism is declined permanently; Docker-in-Docker under Kata is the supported answer.** The residual, a broker-mediated pod-per-step path that would serve adopters who cannot run Kata, is deferred as [Q998](../queue/Q998.md) with a demand trigger.
> The durable rationale is promoted to [appendix-d-alternatives-considered.md § D.15](../design/appendix-d-alternatives-considered.md#d15-pod-per-step-container-execution-arcs-containermode-kubernetes); this doc records the costing that produced the decision.

## The question

[arc-parity.md criterion 3](arc-parity.md#definition-of-done) closes two ways: `container:` and `services:` steps run without privilege, or the docs state plainly and permanently that Docker-in-Docker under Kata is the supported answer and why.
Q727 was filed as a decision rather than a design, and [release-1.6.md](release-1.6.md#the-gating-row-q727-decided-as-a-documented-decline) made this doc the first deliverable so the choice would be an output of a costing rather than an input to one.

Q719 settled the storage half on 2026-08-24 with a `ReadWriteMany` volume mounted into the pod the provisioner really builds, validated across two nodes against a live class ([worker-shared-storage.md](../operations/worker-shared-storage.md)).
What was left is the pod-per-step execution model itself.

## What was measured

Taken 2026-08-25 against `main` at `f300f2a0b`.

| Finding | Where |
|---|---|
| ARC's mechanism needs a pod-`create` grant for the runner's service account, and RBAC cannot scope `create` below a namespace: `resourceNames` does not restrict it | Kubernetes RBAC semantics; ARC capability measured 0.14.2, 2026-08-06 |
| ARC runs many scale sets per namespace, so that grant reaches sibling sets' runner pods and JIT registration Secrets. Its own mitigation is that steps run in step pods rather than the runner pod, which GAG has no split to inherit | [migration-from-arc.md](../operations/migration-from-arc.md) comparison table |
| GAG refuses that token twice: `RunnerTemplate` admission rejects `automountServiceAccountToken` and `serviceAccountName` as reserved, and the provisioner overwrites both after the tenant template merges | [`pod.go`](../../cmd/agc/internal/provisioner/pod.go), [`v2alpha1_crd_test.go`](../../cmd/agc/internal/controller/integration/v2alpha1_crd_test.go), [`pod_provisioning_test.go`](../../cmd/agc/internal/controller/integration/pod_provisioning_test.go) |
| The control is rated Critical, and worker pods are described as holding no API server entry at all | [05-security.md](../design/05-security.md) |
| A per-job Secret holding that job's `jitconfig` and acquired payload lives in the **tenant namespace**, so pod-`create` there lets one job mount another job's credentials | [`pod.go`](../../cmd/agc/internal/provisioner/pod.go) `buildSecret` |
| GitHub does send `jobContainer` and `jobServiceContainers` as top-level `AcquireJob` fields, so the classic path could translate them at provisioning time | [`testdata/job_payload.json`](../../testdata/job_payload.json) (both null: the probe workflow declares neither) |
| The scale-set path stages only a JIT config ("there is no acquired payload, the runner pulls its own job"), so its pod is built before its job is known | [`provisioner.go`](../../cmd/agc/internal/provisioner/provisioner.go) `ProvisionScaleSetWorker` |
| Kata needs nested virtualization: GKE Autopilot allows none, GKE Standard excludes E2/C2D/N2D, and every supporting EC2 family is Intel, with no AMD, Graviton, or GPU family | [kata-dind-workloads.md § Prerequisite](../operations/kata-dind-workloads.md#prerequisite--nested-virtualization-nodes), measured 2026-08-02 (GCP) and 2026-08-12 (AWS) |

The fifth and sixth rows are the pair that decided it.
The ARC-migration path *is* the scale-set path, because an ARC scale set becomes a `RunnerSet`, and that is precisely the path where the AGC never sees the job's container definitions.

## The options, costed

**A.
Adopt ARC's mechanism.** Mount a pod-`create` token into the worker and point `ACTIONS_RUNNER_CONTAINER_HOOKS` at the upstream hooks.
Smallest build of the three and the only one that is not open to being chosen.
The grant cannot be narrowed, because RBAC has no sub-namespace `create` scope, and GAG puts many runner shapes plus every job's registration Secret in the one tenant namespace, so it is a credential-theft path *between jobs of one tenant* that namespace separation does not defend.
ARC mitigates by running steps in step pods rather than the runner pod; GAG has one worker pod per job and so no split to inherit, which is option C rather than something adopting the hooks supplies.
Rejected on the security principle that a default may not trade away a security property, and no opt-in framing rescues it, because the grant would have to exist wherever the capability is used.

**B.
Translate the job's containers at provisioning time.** Parse `jobContainer` and `jobServiceContainers` from the acquired payload, fold them into the worker pod as its container and sidecars, and strip them from the payload forwarded to the runner.
No new privilege, no RWX volume, no new endpoint.
Rejected because it cannot serve the path the gap is about: the scale-set path has no acquired payload to parse, so the capability would exist on the classic tier and be absent on the tier an ARC migration lands on.
It also changes runner semantics, because GitHub's container-job model runs the runner outside the job container and execs steps into it, so the translation is not faithful even where it applies.

**C.
Broker-mediated pod-per-step.** Ship a GAG implementation of the container-hooks protocol in the worker that requests step pods from the AGC rather than from the Kubernetes API, keeping the AGC the only component that creates pods and applying the same invariants to a step pod as to a worker.
Architecturally coherent, preserves the token invariant, and serves both acquisition paths because the hook runs at job time rather than at provisioning time.
Not chosen for 1.6 on cost and on surface: it needs an authenticated worker-to-AGC endpoint that does not exist, reachable from untrusted job code through a NetworkPolicy that today restricts a workload pod to its per-tenant proxy alone, plus step-pod lifecycle, quota accounting, and `fsGroup` propagation to every created pod.
It trades a token in the pod for a control-plane endpoint the job can call, which is a narrower grant than A and not a free one.

**D.
Decline, and document the supported answer.** Docker-in-Docker inside a Kata micro-VM, where the capabilities an inner daemon needs act on a guest kernel rather than the node's.
Already shipped and already dogfooded: GAG's own CI builds a `kind` cluster in a worker pod with zero `privileged: true`.

## The decision

**D, with C deferred rather than closed.**

The decline is permanent for *ARC's mechanism*, and that part is not a scheduling judgement: no future release can adopt it without reversing the token invariant, so it will never become cheap enough to reconsider.
It is not permanent for pod-per-step execution as a capability, which C could still deliver.

**The cost of declining is a named population, not a rounding error.** Kata's nested-virtualization prerequisite is hardware ARC does not require.
A team on GKE Autopilot, on AMD or Arm node families, or on most AWS fleets cannot run Kata at all, and for them the supported answer degrades to a platform-granted privileged `ClusterRunnerTemplate` where their ARC setup needed no *pod* privilege.
The cost is real and it is a trade, not a straight loss: ARC's unprivileged pod is bought with the namespace-wide API grant above, which needs no exploit to use.
[release-1.6.md](release-1.6.md#the-gating-row-q727-decided-as-a-documented-decline) required that population be named rather than left for a reader to discover; it is named on every comparison surface that claims the gap, and in [D.15](../design/appendix-d-alternatives-considered.md#d15-pod-per-step-container-execution-arcs-containermode-kubernetes).

This is why C is deferred with a demand trigger rather than declined.
The gap is real for that population, and the trigger is an adopter reporting it, the same pattern the proxy-hardening cluster used, where [Q725](../queue/Q725.md) revived Q564 and the other three stayed parked.

## What shipped

- [D.15](../design/appendix-d-alternatives-considered.md#d15-pod-per-step-container-execution-arcs-containermode-kubernetes), the durable rationale, plus the `containerMode` bullet D.3's ARC advantages had been missing.
- The comparison surfaces re-stated as a decline rather than a pending gap: [why-gag.md](../why-gag.md), [alternatives.md](../alternatives.md), [roadmap.md](../roadmap.md), and the `containerMode` row in [migration-from-arc.md](../operations/migration-from-arc.md).
- [arc-parity.md](arc-parity.md#definition-of-done) criterion 3 flipped, and its gap row closed.
- [Q998](../queue/Q998.md) filed for C, deferred with the demand trigger.

## What this doc does not claim

The hook protocol's verb set, and the placement of step execution in step pods rather than the runner pod, are both read from the upstream repository and have not been exercised against a running ARC here.
So C's cost is a design estimate rather than a measured one.
Nothing in the decision turns on either: A was rejected on an RBAC property that holds however the hook is shaped, and B on an acquisition-path fact measured in this repo.

**One correction is worth recording, because the first draft of this decision got it wrong.** It argued that ARC's token sits in the same pod the workflow's own code runs in.
That is false for `containerMode: kubernetes` with a job container, which is the mode under discussion: the steps run in the step pods the hook creates.
The argument that survives is narrower and stronger, and it is the one above: the grant is namespace-wide because RBAC offers nothing smaller, and both projects run many runner shapes per namespace.
