---
hide:
  - navigation
---

# Choosing how to run self-hosted CI

One question decides most of this: **must the compute be yours, and do several
teams have to share it?** If the answer to either half is no, something simpler
than GitHub Actions Gateway (GAG) is very likely the right answer, and this page
says which. If the answer to both is yes, the rest of the page is the comparison.

## Four cases where something else wins

Each of these is a real exit, not a strawman.

**You are happy running jobs on a vendor's infrastructure.** Managed runner
services (Blacksmith, Namespace, Depot, WarpBuild, Cirrus Runners, Ubicloud and
others) compete on build speed and price per minute. They will be faster to
adopt and usually faster per build. GAG competes on governance and isolation for
compute you already own, which is a different purchase.

**Managed Kubernetes is cheap where you are and your CI fits in one cloud.**
Then give each team its own cluster, in its own network, in its own project. A
project or account boundary isolates harder than any shared cluster can, and on
a cloud provider it costs an API call. Take it.

**One team owns the cluster and the runners.**
[Actions Runner Controller](https://github.com/actions/actions-runner-controller)
(ARC) is GitHub's own Kubernetes operator for self-hosted runners, it is GA and
widely deployed, and for a single-tenant cluster it is a reasonable choice. GAG
speaks the same runner-scale-set protocol through the same client library, so
this is not a protocol argument.

**Your compute is elastic cloud capacity and you do not want a cluster.**
[RunsOn](https://runs-on.com) and
[terraform-aws-github-runner](https://github.com/github-aws-runners/terraform-aws-github-runner)
run ephemeral virtual machines in your own AWS account.
[Actuated](https://actuated.com) drives Firecracker micro-VMs on hardware you
own, with a vendor-hosted scheduler. A VM per job gives isolation without any
namespace argument at all.

GAG is for what is left: **the nodes are big and expensive, several teams have
to share them, and that sharing has to be safe.**

## Where each option can actually run

The single fastest filter, and the one most comparisons omit. "Self-hosted"
usually means "self-hosted on AWS".

| Option | Runs on |
|---|---|
| Managed runner services | the vendor's infrastructure (some offer compute in your cloud account) |
| RunsOn, terraform-aws-github-runner | AWS only |
| [ForgeMT](https://github.com/cisco-open/forge) (Cisco, Apache-2.0) | AWS only |
| Actuated | your own hardware, with a vendor-hosted control plane |
| ARC, GAG | any conformant Kubernetes cluster, including on-premises and air-gapped |

If you are on-premises, or on reserved hardware you have already paid for, most
of the field is gone before any feature is compared. That is usually the same
constraint that made you self-host: compliance, data residency, an IP
allow-list, or a GPU reservation.

**The on-premises case also breaks the account-per-tenant model.** ForgeMT is
the closest thing to GAG in intent, an explicitly multi-tenant, open-source
runner platform for platform teams, and it isolates tenants by giving each one
its own AWS account, on the reasoning that Kubernetes namespaces are not a
strong enough boundary. The reasoning is sound and the boundary genuinely is
stronger. But an AWS account is a free API call that inherits a pre-built
control plane, and on bare metal the equivalent is a hardware purchase or
building a private cloud first. Partitioned capacity also cannot be shifted
between tenants, so each is provisioned for its own peak and every trough is
stranded, which is the opposite of the reason you are sharing expensive nodes.
The full comparison is in
[Appendix D.9](design/appendix-d-alternatives-considered.md#d9-forgemt-and-account-per-tenant-runner-platforms).

## Multi-tenancy is a different product, not a bigger one

Once several teams share a cluster, at least three roles are in the room, and
what separates them is the product:

- a **platform engineer** who owns the cluster, the quotas, and what tenants may
  not do;
- a **tenant operator** who owns one namespace's CI and should not need a ticket
  to change a runner;
- an **external contributor** whose code runs but who is not a user of your
  platform at all.

ARC models the first two as the same person, which is coherent for a
single-owner cluster and is why it has no primitive separating platform-owned
from tenant-owned concerns. GAG's capabilities are mostly boundaries between
these roles rather than features on a list:

| The boundary | How GAG draws it |
|---|---|
| Platform sets a cap the tenant cannot raise | The namespace `ResourceQuota` is platform-owned. The controller holds no write verb on it |
| Platform grants privilege; tenant cannot self-grant | Pod Security Admission level is a namespace label, and a privileged worker shape must come from a platform-published `ClusterRunnerTemplate` |
| Platform bounds priority; tenant composes within it | `priorityTiers` names classes from a platform-owned allowlist, editable without a control-plane restart |
| Tenant self-serves without cluster-admin | One `ActionsGateway` provisions the controller, proxy pool, RBAC, and network policies inside the quota |
| Tenant sees its own data and no one else's | Metrics and a Grafana dashboard scoped per tenant, plus a separate platform view |
| Contributor's code is contained | Default-deny egress, per-tenant egress identity, and Kata micro-VM workers |

Each role, and what it explicitly cannot do, is written down in
[Personas](operations/personas.md).

## The question is whether there is a road, not whether the field exists

Most capability comparisons ask whether a thing can be configured. For shared
infrastructure the useful question is whether anyone has proven it works, since
the alternative is finding out yourself in production. Two worked examples,
both measured on 2026-08-06 against ARC 0.14.2 and its `master` branch.

**Sandboxed workers.** Both products set the same Kubernetes field,
`runtimeClassName`, so a feature table would score this as a tie. ARC's
documentation contains 18 files and none covers Kata Containers, gVisor, or
sandboxed runtimes; neither `kata` nor `runtimeclass` appears anywhere in its
chart values; and its issue tracker has two mentions of Kata ever, both closed.
GAG ships a 500-line guide covering nested-virtualization node prerequisites,
the runtime and `RuntimeClass` setup, the capability set an unprivileged Docker
daemon needs inside the guest, and an explicit section on what Kata does not
buy you. It is the default for GAG's own end-to-end suite, which builds a
Kubernetes cluster inside an unprivileged worker pod, validated on named kernel
versions with no privileged container.

**Observability.** GAG ships 20 alert rules as a `PrometheusRule`, two Grafana
dashboards for two different audiences, metrics scoped per tenant and per runner
set with cross-tenant rollups, structured logging across all four tiers with
credential redaction applied before any line is emitted, and 1,419 lines of
operator documentation across five pages. ARC ships one per-scale-set sample
dashboard and no alert rules; its listener metrics are opt-in, shipping
commented out in the chart. Its request for a listener metrics dashboard has
been open since 2025-01-13.

## Where GAG loses

Stated plainly, because you will find these anyway.

**Install base.** Measured on 2026-08-06: ARC has 6,417 GitHub stars, ForgeMT
has 211, GAG has 3. GAG's repository was created 2026-05-16. If being the first
production deployment of a control plane is unacceptable, that is a complete
answer and no capability changes it.

**No commercial support**, by design. GAG is Apache-2.0 with no paid tier and no
commercial roadmap. ARC has a GitHub Support entitlement, though it is worth
reading its scope exclusions, which put Kubernetes orchestration, policy
application, and template customization out of scope.

**Multi-label runner sets.** ARC gained these in 0.14.0 (2026-03-19). GAG admits
exactly one label per runner set, so a workflow targeting
`runs-on: [linux, gpu]` needs one edit per target to move across.

**Container and service steps without privilege.** ARC's `containerMode:
kubernetes` runs `container:` and `services:` job steps as separate pods with a
provisioned volume. GAG runs one worker pod per job, so that path is
Docker-in-Docker, unprivileged under Kata rather than a pod per step.

**No cache backend.** Workers are storage-less by design, so `actions/cache` and
Docker layer caching have no home. A shared cache between tenants is an
exfiltration path, and the security review comes before the feature.

**GitHub Enterprise Server is served but untested** against a real appliance.

**Two open defects** that an evaluator should know about rather than discover:
the GitHub-side runner group is not yet bound, so which repositories may target
a tenant's runners is unbounded; and job duration and pod-creation latency are
not emitted on the default acquisition tier, so some shipped dashboard panels
read empty. Both are on the [roadmap](roadmap.md) and gate the next release.

## Reading further

The full argument for each alternative, in the same form as GAG's own design
record, is in
[Appendix D](design/appendix-d-alternatives-considered.md): GitHub-hosted
runners, self-hosted without a controller, ARC, ARC with KEDA, Kueue, ForgeMT,
[Prow](design/appendix-d-alternatives-considered.md#d10-prow-and-the-prior-art-for-automatic-re-run)
(Kubernetes' own CI system, and the reason GAG does not claim automatic re-run
is novel), GitLab Runner, Buildkite, and the managed lane.

For the capability-by-capability comparison against ARC specifically, with the
ARC column dated and its measurement method stated, see
[Why GAG?](why-gag.md).
