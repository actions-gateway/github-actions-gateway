# Secure, Isolated, Multi-Tenant OSS CI

> **Status: goal stated 2026-08-06, deliverables partly shipped.** This is a map and a definition of done, not a design.
> The design for the network half is [q408-untrusted-pr-egress.md](q408-untrusted-pr-egress.md) and is not restated here.
> Kernel isolation, the pod floor, the network layer, the metadata block and the egress evidence are shipped.
> Definition of Done items 4 and 6 are open; the three decisions at the end of this document are settled.

## Why this is the goal

Multi-tenant CI on shared, expensive nodes is the efficient answer, and almost nobody runs it.
Where managed Kubernetes is cheap and CI fits inside one cloud, a cluster per tenant in its own network and project is a perfectly good answer, and a stronger isolation boundary than a shared cluster can offer.
GAG exists for the case where that stops being cheap: cluster overhead is real, the nodes are big and expensive (GPU, large-memory, on-premises, or reserved capacity already paid for), and sharing them is the only way the economics work.

Account-per-tenant and cluster-per-tenant have **no cheap on-premises analogue**.
In a cloud an account or project is a free API call that inherits a pre-built control plane.
On bare metal the equivalent boundary is a hardware purchase, or building the hypervisor, software-defined-networking, and self-service storage layer that would make accounts possible.
Worse, partitioned capacity cannot be shifted between tenants, so every tenant is provisioned for its own peak and every trough is stranded.
You cannot hand the hardware back at month end.

## What makes it unsolved

The reason teams retreat to a cluster per tenant is not cost.
It is that **CI multi-tenancy has never been secured well enough**, and CI is the hard case on two axes at once:

- **CI needs root.** Image builds, Docker-in-Docker, `services:` containers, package installs.
  Privileged Docker-in-Docker on a shared node defeats Pod Security Admission `restricted` outright.
- **CI needs network isolation, and that is hard on Kubernetes.** NetworkPolicy needs a policy-enforcing CNI to mean anything; egress by hostname is CNI-specific; and CDN-fronted registries cannot be pinned by CIDR at all.

So the honest framing for this milestone is not "competitor X lacks feature Y".
It is: **nobody has solved this, here is how far we have got, and here is the boundary of what we currently claim.**

## Threat model: the untrusted contributor

`docs/design/05-security.md` models tenant against tenant, where every tenant is a team inside the operator's organization.
This milestone adds an adversary that model does not carry: **a contributor who can cause code to run but is not a user of the platform.** A fork pull request is an arbitrary-code-execution request that the CI system is designed to honour.

What changes:

- The adversary chooses the workload, so "misconfigured tenant" becomes "deliberately hostile job".
- The adversary is anonymous and cheap to become.
  Rate limiting and abuse response matter more than attribution.
- The blast radius that matters is not the tenant's own namespace.
  It is every other tenant on the node, the node's own credentials, and the mirror or cache the platform provides.
- GitHub itself warns that self-hosted runners should not be used with public repositories, because untrusted workflow code can persist on the runner.
  GAG's answer has to be that nothing persists and nothing reachable is worth having.

## The layer map, with current state

| Layer | Mechanism | State |
|---|---|---|
| Kernel | Kata micro-VM workers, no `privileged: true` | **Shipped and validated.** Default for GAG's own end-to-end CI, a kind cluster built inside an unprivileged worker pod, on nested-virtualization GKE. See [kata-dind-workloads.md](../operations/kata-dind-workloads.md) |
| Pod identity floor | `hostPID`/`hostNetwork`/`hostIPC`/`automountServiceAccountToken` all false, controller-managed ServiceAccount, stamped over any tenant `PodTemplateSpec` | **Shipped** |
| Network egress | Default-deny NetworkPolicy, per-tenant proxy pool, in-cluster pull-through mirror, egress scoped to mirror plus GitHub plus DNS | **Shipped and validated (Q408, 2026-08-28).** A Kata worker reaches cluster DNS on 53, GitHub on 443 and the mirrors on 5000 and nothing else, measured on the dogfood cluster by in-job negatives against controls that answered, on every run of the lane. Recipe: [kata-dind-workloads.md § Untrusted pull requests](../operations/kata-dind-workloads.md#untrusted-pull-requests--the-tight-egress-posture); design in [q408-untrusted-pr-egress.md](q408-untrusted-pr-egress.md). Q539 validated Dragonfly as the mirrors' back end against the contract this validated, and Q540 shipped the composed stack's node layer as a recipe; both are follow-ons rather than gaps |
| Node metadata | The link-local DNS allowance is scoped to port 53, so no rule admits the metadata address on any port it serves | **Shipped and asserted (Q716).** Q226 measured HTTP 200 from inside a Kata guest, because Kata bounds the guest kernel and not the pod's network identity. `TestBuildNetworkPolicy_DeniesCloudMetadataServer` pins the authored policy on every PR; `E2E_V2_DirectEgress_MetadataServerBlocked` proves a real CNI enforces it on the Calico lane. Needs a policy-enforcing CNI: without one, Workload Identity is the only control |
| Cross-job cache | One in-cluster `actions/cache` server per tenant, its scope taken from GitHub's token per job | **Shipped (Q215, validated 2026-11-01)** on kind, under Kata with this posture, and with a fork pull request, whose entries the base branch could not restore. See [in-cluster-cache.md](../operations/in-cluster-cache.md). The posture does not make GitHub's own cache unreachable: a Kata worker reached Azure blob storage inside GitHub's published ranges, and what that admits is [Q1253](../queue/Q1253.md) |
| Evidence | Per-tenant egress audit records | **Shipped, opt-in (Q564, Q986).** `EgressProxy.spec.auditLogging: Connections` writes one structured line per accepted CONNECT: namespace, destination host and port, bytes each way, duration. Off by default: the record is data about a tenant's egress, so retaining it is the platform's decision. Definition of Done #5 takes a further opt-in on both sides — `ConnectionsWithSource` adds the client address to that record, and `ActionsGateway.spec.auditLogging: WorkerAddresses` records which job holds each address while its pod lives — because attribution is what turns an egress record into a per-worker movement log, and because a worker pod is deleted with its job so the binding cannot be looked up afterwards. See [observability-logging.md](../operations/observability-logging.md#attributing-a-record-to-a-tenant-and-a-job) and [q986-egress-attribution.md](archive/q986-egress-attribution.md) |
| Transport to the proxy | TLS on the AGC-to-proxy and worker-to-proxy hop | **Shipped.** The proxy serves CONNECT over TLS 1.2 or later with a GMC-generated per-tenant certificate, and the AGC and workers pin that tenant's CA, so the CONNECT target is not readable on the hop. Cluster DNS is still plaintext; see [Appendix G.4](../design/appendix-g-future-enhancements.md#g4-in-cluster-encryption-beyond-the-proxy-hop) |
| Noisy-neighbour containment | Per-runner-set proxy pool | **Shipped (Q567).** Each `EgressProxy` is its own pool, and a `RunnerSet`'s `spec.proxyRef` overrides the gateway default, so a bandwidth-heavy set can be given a pool of its own. `TestV2_RunnerSet_DedicatedProxyPool` pins two sets under one gateway egressing through different pools. See [tenant-onboarding.md](../operations/tenant-onboarding.md#a-dedicated-proxy-pool-for-a-bandwidth-heavy-runner-set) |

## Definition of done

Broader than Q408's success criteria, which bound the mirror work only and are met.
This milestone is done when all of the following hold, each demonstrated rather than argued:

1. A job from an untrusted fork pull request runs on a shared cluster with a kernel it cannot escape into anything useful, and reaching that state costs one documented step rather than a design of the adopter's own.
   **The default-versus-opt-in question splits, and Q408's Phase 4 is what split it.** The *network* posture is the default and always was: a tenant's egress is default-deny plus GitHub, and what Phase 4 shipped was the **deletion** of an additive allow-all rather than a new object, so weakening the posture is the deliberate act and holding it is the resting state.
   The *pod shape* is not the default and must not become one: a `ClusterRunnerTemplate` marked default applies to every `RunnerSet` naming no template, so shipping the Kata shape as a cluster default would hand a privileged capability set to sets that never asked for one ([runner-template-library.md § Nothing ships as a cluster default](../operations/runner-template-library.md#nothing-ships-as-a-cluster-default)).
   This criterion therefore asks that the secure state be what an operator gets by not acting, and that adopting the kernel boundary on top be one `kubectl apply -k` against a shipped template.
   It does not ask for a default template, and a future reading that revives that demand is reading a criterion this project already declined on a security argument.
2. Its egress reaches the mirror, GitHub, and DNS, and an end-to-end run's logs name no other host.
3. It cannot reach the node's metadata service, and a test asserts this on a policy-enforcing CNI rather than a doc recommending it.
4. It cannot read another tenant's cache, secrets, or job payloads, including through anything the platform provides for its convenience.
5. An operator can produce, unprompted, a per-tenant record of which host each job reached.
   Controls without evidence do not satisfy an auditor.
   **Met as of Q986**, on the two opt-ins above: the join is the proxy record's source address against the AGC's worker-address bindings, and it names the consuming tenant on a shared pool as well as an unshared one.
6. The residual channels are enumerated in this document and each one is either closed or explicitly accepted with a reason.

## Explicitly out of scope, and residual risk accepted

The most important section.
"We run untrusted OSS CI" is indefensible without a boundary.

- **Compute abuse is contained, not prevented.** A hostile job can consume the capacity its tenant's quota allows.
  The quota, the priority tiers, and `maxWorkerLifetime` bound it; nothing detects intent in advance.
- **Secrets a maintainer deliberately grants a fork are out of reach.** If a workflow is configured to expose a secret to `pull_request_target`, that is a repository configuration decision GAG cannot override and should not pretend to.
- **Which runner set a fork job lands on is the fork's choice** wherever GitHub's approval gate does not cover the contributor, until the default refusal in [Q1330](../queue/Q1330.md) ships; after that, it is the choice of an adopter who sets a `RunnerSet` to accept fork jobs.
- **Side channels between co-tenant micro-VMs are not addressed.** Kata bounds the kernel.
  Speculative-execution and shared-cache side channels between guests on one node are a hardware and hypervisor concern, not one this milestone closes.
- **The mirror is a trusted component.** A compromised pull-through mirror reaches every job that pulls through it.
  The mirror contract in [q408 §3.5](q408-untrusted-pr-egress.md#35-the-mirror-role-is-a-contract) is what bounds this, and it is a contract rather than an enforcement.
- **A shared mirror set exposes cache timing to every tenant that can reach it.** Whether a repository is already warm says that some other tenant pulled it.
  The repository *list* is no longer part of this: `GET /v2/_catalog` answered 200 with every cached repository named, `catalog.maxentries=0` did not close it, and every instance is now fronted by a proxy that refuses the path under both topologies ([how, and what it was measured against](../../deploy/registry-mirror/README.md#closing-the-repository-catalog)).
  What is left is timing, and it is readable from where an attacker sits.
  [`mirror-timing.sh`](../../scripts/e2e/mirror-timing.sh) takes the reading from inside the guest on the Kata lane, and its first live run, in the `v1.8.0-rc.1` window, returned `SEPARATED`: every hit at 46 ms or less and every miss at 147 ms or more, over 4 references with one cold and one warm fetch each ([release-1.8.md](archive/release-1.8.md)).
  One window is one reading, but its arms did not touch, so a single timed fetch tells warm from cold there.
  It is an accepted risk only for an adopter who chose the shared topology knowing it, which is what [Choosing a mirror topology](../operations/kata-dind-workloads.md#choosing-a-mirror-topology) exists to make explicit; the isolated topology removes it and costs a mirror set per tenant.
- **This is not a claim about arbitrary hostile input at scale.** The goal is that a fork pull request on an adopter's own repositories is safe to run.
  A public, unauthenticated build service is a different product with different economics.

## What the site may claim today

Down to earth, current state only:

> Kata micro-VM workers are validated end to end and are the default for GAG's own CI, which builds a kind cluster inside an unprivileged worker pod.
> Their egress is now closed to match: an in-cluster registry mirror carries every image pull, the tenant carries no allow-all rule, and a run's own probes confirm that a worker reaches cluster DNS, GitHub and the mirror and nothing else.
> An operator can opt in to a per-job record of which host each job reached.
> What is not yet demonstrated is the cross-tenant cache boundary, so we describe the posture and its measurements rather than declaring untrusted-PR readiness.

Q408 closing moves items 1, 2 and 3 of the definition of done to demonstrated, and item 6 holds as long as the section below stays current, which a layer change is the moment to re-check.
Item 5 closed with Q986.
Item 4's cache half shipped with Q215 as one server per tenant, whose ingress admits only that namespace's workers, and the part of it the mirror itself introduced now has its basis written down: a shared mirror and a mirror per tenant are both defensible, so the platform admin chooses, and [Choosing a mirror topology](../operations/kata-dind-workloads.md#choosing-a-mirror-topology) is what they choose on.
The guest timing reading confirmed that guidance rather than changing it: a shared set does expose whether another tenant pulled an image, which is a timing channel and no more, so tenants who must not learn what each other build take isolation.
The secrets and job-payload halves of item 4 have no demonstration recorded yet.
Do not claim untrusted-PR readiness until all six hold.
The claim is checkable and a wrong one costs more than the feature is worth.

## Deliverables

Shipped: Kata validation, the pod isolation floor, default-deny NetworkPolicy, per-tenant proxy pools, the opt-in per-pool egress audit record (Q564) and the two further opt-ins that make it attributable to a tenant and a job (Q986, which is what satisfies Definition of Done #5: see the Evidence row), TLS on the proxy hop, and the untrusted-PR egress posture itself (Q408: the in-cluster registry mirror plus the deletion of the tenant's allow-all rule, validated on the dogfood cluster 2026-08-28), and per-runner-set proxy pools (Q567).

Shipped since: the per-tenant `actions/cache` server (Q215, [q215-worker-cache.md](q215-worker-cache.md)).

Open, with rows: [Q1253](../queue/Q1253.md) (what admitting Azure blob storage lets a worker reach), and the open decision below.

## Open decisions the map surfaced

Surfaced by drawing the whole picture; each is a decision before it is work:

1. **What GAG refuses to run for a fork pull request** (decided 2026-11-09, Q1220).
   A `pull_request` run executes the pull request's merge commit, whose workflow file carries the fork's edits, so a fork can rewrite `runs-on` to reach a trusted runner set, and routing by label is a convention rather than a control.
   GitHub's own control comes first: approval for all external contributors, which keys on who opened the pull request (it exempts organization and repository members) rather than on the fork ([how an operator applies it](../operations/security-operations.md#fork-pull-requests-choose-their-own-runner-label)).
   Runner groups do not substitute for it: they scope by repository, and the selected-workflows restriction (Enterprise Cloud and Server only, per GitHub's docs) pins ordinary workflows to a branch while a `pull_request` run's ref is `refs/pull/N/merge`, so it refuses every pull request rather than only forks.
   Nothing on GitHub's side refuses a job because its head is a fork, so GAG refuses fork jobs on every `RunnerSet` by default, and a set meant for untrusted pull requests accepts them explicitly ([Q1330](../queue/Q1330.md)); an opt-in was chosen first and reversed on 2026-11-10, because with no GitHub-side backstop it left every adopter who never set the field exposed.
   The job message cannot drive it: upstream `JobMessageBase` (actions/scaleset `716908e`, read 2026-11-09) has `eventName` but no head-repository field, and the live `JobAssigned` captured on 2026-07-26 carries none ([scaleset-eviction-recovery.md](scaleset-eviction-recovery.md)).
   The run object the AGC already reads for eviction recovery names the head repository, and the refusal compares that name with the run's own repository, scoped to the events that run the pull request's merge commit (`pull_request`, `pull_request_review`, `pull_request_review_comment`); the object's `fork` flag reads `true` for any pull request on a repository that is itself a fork, so it is not used (measured 2026-11-10, [Q1330](../queue/Q1330.md)).
   Secrets a workflow grants to `pull_request_target` stay the repository's decision, as the residual-risk list below says.
2. **Runner-token scope for an untrusted job.** Settled 2026-11-09: no untrusted class gets a narrower credential, because GAG has none to issue.
   The reasoning and the list it rests on are in [Credentials a job can read](#credentials-a-job-can-read).
3. **What "untrusted" is, as a declared thing.** Settled 2026-11-11 as a meaning, not an API.
   Today it is a posture an operator assembles.
   A declared untrusted set is the one that accepts fork jobs, and only while the checkable parts of the posture hold.
   The meaning, item by item, is in [What a declared untrusted class would mean](#what-a-declared-untrusted-class-would-mean).
   The moat review rejected building that surface while five of its dependencies are open or blocked, which is still the correct order, so how to express it waits.

## Credentials a job can read

Every credential a worker pod holds is readable by the job's own code, so the job's credentials are an asset the untrusted contributor reaches by default.
This is the list, read from the code on 2026-11-09.
It covers the scale-set tier alone, because v2.0.0 retired the classic tier (Q1273), and `ProvisionScaleSetWorker` is the only provision path the AGC has.

| Credential | How the job reaches it | Who issues and scopes it | What it permits from inside the job |
| --- | --- | --- | --- |
| The runner's just-in-time (JIT) config: runner ID, OAuth client ID, and RSA private key | The per-job Secret's `jitconfig` key, mounted read-only into the runner container, and the argument the wrapper passes to `run.sh --jitconfig` | GAG mints it through `generatejitconfig`, and GAG's request carries a runner name and a work folder and no scope (`scaleset/client.go`, `GenerateJITConfig`); whether GitHub's endpoint accepts a narrower scope is unmeasured | Acting as its own runner, which is one ephemeral runner in this `RunnerSet`'s scale set. It cannot register a second runner, because the pod holds no registration token and no App token: the Secret carries `plan-id` and `jitconfig` alone (`buildSecret` in `cmd/agc/internal/provisioner/pod.go`). Whether it can open a second broker session or take a second job while its own runs is unmeasured ([Q1329](../queue/Q1329.md)). |
| The job's runtime tokens: `ACTIONS_RUNTIME_TOKEN` and the results and cache URLs, plus `ACTIONS_ID_TOKEN_REQUEST_TOKEN` when the workflow grants `id-token: write` | GitHub delivers them in the job message, and the runner exports them to actions | GitHub, per job | Whatever GitHub scopes them to for that run. The per-tenant cache server checks its reserve, finalize and lookup calls against the runtime token's scopes ([q215-worker-cache.md](q215-worker-cache.md#the-scope-comes-from-githubs-own-token)). |
| The workflow's `GITHUB_TOKEN` | The job context | GitHub, from the workflow's `permissions:` | The repository's decision, outside the platform. |
| A Kubernetes ServiceAccount token | Not mounted: the AGC sets `automountServiceAccountToken: false` on every worker. Admission rejects a template that sets `automountServiceAccountToken` or `serviceAccountName`, but not a projected token volume, so a tenant template could still project the worker ServiceAccount's token explicitly | The kubelet, for the per-gateway worker ServiceAccount | No RoleBinding the GMC writes names that ServiceAccount, and workers have no egress to the API server. |
| The node's cloud credentials | The metadata server, where it is reachable | The cloud provider | Covered by Definition of Done #3, and closed there. |
| Anything a tenant template adds: env vars, Secret volumes | The template | The tenant | The tenant's own choice, made per `RunnerTemplate`. |

The proxy and GitHub Enterprise Server CA certificates are mounted too, and both are public.
The proxy env vars carry addresses and no credential, and attribution identifies a worker by its source address, so the attribution client certificate goes to the AGC's reader and never to a worker.

**No class gets a narrower credential, and the evidence leaves no option to prefer.** The only credential GAG issues is already scoped to one runner in one `RunnerSet`'s scale set, so a class drawn per `RunnerSet` bounds it without anything new, and GAG's request for it carries no scope that could be narrowed.
Hiding it from the job is not on offer either: a job's steps run as the runner's own user, in the container the runner reads the credential from (the runner's behaviour, not measured here).
If Q1329 finds that the credential can take a second job, that is a defect for every class, not a narrowing for one.
So the declared class in decision 3 carries no credential item from this list.

## What a declared untrusted class would mean

Settled 2026-11-11 (Q1222), as a meaning and not an API: nothing below names a field, and nothing is built.

**A `RunnerSet` declared untrusted is the set that accepts fork pull request jobs, and it accepts them only while the parts of the posture the platform can check still hold.** That is the whole difference from the default [Q1330](../queue/Q1330.md) ships, which is decided (Q1220) and not yet built.
Under that default every set refuses fork jobs and a set accepts them through one explicit switch with no precondition, so a set on a `runc` template in a namespace with an allow-all egress rule accepts forks as readily as one on the Kata recipe.
The class puts preconditions on that switch, and a set whose preconditions fail refuses fork jobs, through whatever refusal path Q1330 settles on.
**The class replaces the precondition-free switch rather than sitting beside it.** Kept beside it, the class would be an opt-in stricter mode, and Q1220 rejected exactly that shape for the refusal itself: an adopter who never sets the stricter field stays exposed.
So once the class is built, accepting fork jobs means the preconditions are checked.
The escape hatch stays the operator's explicit act: the micro-VM list the kernel row checks is the operator's, so an operator who accepts forks on another runtime says so by listing it.
Q1330 ships the switch without preconditions, so adding them later is a breaking tightening for any set that accepts forks on a posture that would fail them, and the release that builds the class carries an upgrade note.
Its jobs from the repository's own branches are unaffected either way.

The class is drawn per `RunnerSet` and marks one set, so it does not revive the cluster-default template that Definition of Done #1 declines: a set that never declares it gets nothing new.

### What the class adds, against today's assembly

Each row starts from what an operator does today in [kata-dind-workloads.md](../operations/kata-dind-workloads.md), and says whether the platform could check it.

| Part of the posture | What the operator does today | What the class would require | Can the platform enforce it? |
| --- | --- | --- | --- |
| Kernel boundary | Applies `deploy/templates/kata-dind` and points the set's `templateRef` at it ([Configure the worker podTemplate](../operations/kata-dind-workloads.md#configure-the-worker-podtemplate)) | The set's resolved template names a micro-VM `runtimeClassName` and runs no privileged container | **Yes, against a list the operator supplies.** The AGC's `resolveTemplateChain` (`cmd/agc/internal/controller/runnerset_target.go`) is the one place that sees the spec a worker actually gets, a `ClusterRunnerTemplate` included, so a check there also catches a template edited after the set was admitted. Nothing checks a runtime class today: a `ClusterRunnerTemplate` may name any class and may run privileged containers (`cmd/gmc/internal/webhook/v2/runnertemplate_webhook.go`). A class name does not say whether it is a micro-VM, so which names count is the operator's list, the same shape as `--allowed-priority-classes` |
| Egress closed to DNS, GitHub and the mirrors | Deletes every allow-all egress policy from the tenant namespace and confirms it is gone from the live rules ([Adopting it](../operations/kata-dind-workloads.md#adopting-it), step 4) | No other NetworkPolicy opens egress for the set's workers | **Its documented failure, yes; the posture itself, no.** The GMC's worker policy (`buildWorkloadNetworkPolicyV2` in `cmd/gmc/internal/controller/actionsgateway_v2_builder.go`) selects every worker in the gateway's namespace, so egress belongs to the namespace and not to one set: an additive rule a trusted set needs reaches the untrusted set's workers too. The GMC already list-watches every NetworkPolicy in the cluster (`Owns(&networkingv1.NetworkPolicy{})` in `cmd/gmc/internal/controller/egressproxy_controller.go`, with no cache scope), so it could report a namespace where a policy selecting the workers carries an allow-all rule. The AGC, where the fork refusal runs, holds no NetworkPolicy access, so the reading reaches it from the GMC or the AGC gains a read. A refusal on this row names the policy that tripped it, because the trip comes from another set's rule and would otherwise read as this set's fault. It cannot tell a narrower additive rule from the mirror's own, and it cannot see whether the CNI enforces any of them. [`egress-negatives.sh`](../../scripts/e2e/egress-negatives.sh), run inside the job, stays the proof |
| Image pulls through the mirrors | Renders the mirrors for the measured upstreams and wires the job's image clients ([Adopting it](../operations/kata-dind-workloads.md#adopting-it), steps 1 to 3) | Nothing | **No, and it needs no enforcing.** The mirrors are the operator's deployment outside GAG's resources, and the wiring lives in the template. With egress closed, a missing mirror fails the job's pulls rather than opening a path |
| Fork jobs accepted | Nothing before Q1330 ships, because no set refuses them ([What this posture does not cover](../operations/kata-dind-workloads.md#what-this-posture-does-not-cover)) | The set accepts them, gated on the kernel and egress rows | **Yes.** It is Q1330's switch with preconditions added, and the refusal compares the run's head repository with its own as [decision 1](#open-decisions-the-map-surfaced) records |
| A bound on compute abuse | Nothing; `maxWorkerLifetime` defaults to 12 hours and `"0s"` disables it (`api/v2/runnerset_types.go`) | Whether to refuse a worker with no lifetime bound is open | **Yes, on the resolved spec only.** A template's own `activeDeadlineSeconds` wins over `maxWorkerLifetime` (`cmd/agc/internal/provisioner/pod.go`), so refusing `"0s"` at `RunnerSet` admission bounds nothing a template can override. The check belongs where the kernel row's does, on the spec the worker gets. The residual-risk list counts this bound as part of what contains a hostile job |

### What the class carries nothing for

Three layers already hold for every set, so a class would add nothing.
Credentials are settled in [the list above](#credentials-a-job-can-read): the one GAG issues is scoped to one runner already.
The `actions/cache` server is one per tenant, whichever set the job ran on ([in-cluster-cache.md](../operations/in-cluster-cache.md)).
The pod identity floor is stamped onto every worker.

### What stays the operator's

These are cluster facts no `RunnerSet` can declare:

- Nested-virtualization nodes and the Kata `RuntimeClass` ([Prerequisite](../operations/kata-dind-workloads.md#prerequisite--nested-virtualization-nodes)).
- A policy-enforcing CNI, without which every NetworkPolicy above is text.
- Workload Identity, which the metadata block needs anyway ([What this posture does not cover](../operations/kata-dind-workloads.md#what-this-posture-does-not-cover)).
- The mirror topology, which is cluster-wide ([Choosing a mirror topology](../operations/kata-dind-workloads.md#choosing-a-mirror-topology)).
- Which runtime class names count as micro-VMs, the list the kernel row checks against.

### What stays the repository's

GitHub's approval setting for external contributors, any secret a workflow grants to `pull_request_target`, the `GITHUB_TOKEN` permissions a workflow declares, and the label its `runs-on` names.
The class decides what GAG does with a fork job that reaches an untrusted set; GitHub decides what that job carries when it arrives.
