---
hide:
  - navigation
---

# Choosing how to run self-hosted CI

One question decides most of this: **must the compute be yours, and do several
teams have to share it?** If either half is no, something simpler than GitHub
Actions Gateway (GAG) is probably right.

## When not to choose GAG

| If | Use | Not GAG, because |
|---|---|---|
| A vendor can run your jobs | a managed runner service (Blacksmith, Namespace, Depot, WarpBuild, Ubicloud) | they compete on build speed and price per minute; GAG does not ([D.14](design/appendix-d-alternatives-considered.md#d14-managed-runner-services-an-explicit-non-competitor)) |
| Managed Kubernetes is cheap and your CI fits one cloud | a cluster per team, in its own project | a project boundary isolates harder than any shared cluster, and costs an API call |
| One team owns the cluster and the runners | [ARC](https://github.com/actions/actions-runner-controller) | GAG speaks the same protocol through the same client library. This is not a protocol argument ([D.3](design/appendix-d-alternatives-considered.md#d3-actions-runner-controller-arc)) |
| Your compute is elastic cloud capacity, no cluster wanted | [RunsOn](https://runs-on.com), [terraform-aws-github-runner](https://github.com/github-aws-runners/terraform-aws-github-runner), [Actuated](https://actuated.com) | a VM per job isolates without any namespace argument ([D.11](design/appendix-d-alternatives-considered.md#d11-self-hosted-github-actions-without-kubernetes)) |

!!! tip "GAG is for what is left"

    The nodes are big and expensive, several teams have to share them, and that
    sharing has to be safe.

## Location, location, location

Where each option can actually run is the fastest filter, and most comparisons
omit it: "self-hosted" usually means "self-hosted on AWS".

| Option | Runs on |
|---|---|
| Managed runner services | the vendor's infrastructure |
| RunsOn, terraform-aws-github-runner, [ForgeMT](https://github.com/cisco-open/forge) | AWS only |
| Actuated | your hardware, vendor-hosted control plane |
| ARC, GAG | any conformant Kubernetes cluster, including on-premises and air-gapped |

On-premises or on reserved hardware, most of the field is gone before a single
feature is compared, by the same constraint that made you self-host.

That also breaks account-per-tenant isolation, which is ForgeMT's model and the
serious argument against a shared cluster: an AWS account is a free API call,
but its bare-metal equivalent is a hardware purchase, and partitioned capacity
cannot be shifted between tenants.
[Appendix D.9](design/appendix-d-alternatives-considered.md#d9-forgemt-and-account-per-tenant-runner-platforms)
takes it seriously and states both trade-offs.

## Multi-tenant platforms are an order of magnitude more complex

Adding teams does not add a feature, it adds roles. Three end up in the room,
each with different powers and a different blast radius, and holding them apart
is most of the work. ARC models the first two as one person, which is coherent
for a single-owner cluster and is why it has no primitive separating them.

| The boundary | How GAG draws it |
|---|---|
| Platform sets a cap the tenant cannot raise | `ResourceQuota` is platform-owned; the controller has no write verb on it |
| Platform grants privilege, tenant cannot self-grant | Pod Security Admission is a namespace label; privileged shapes come from a platform `ClusterRunnerTemplate` |
| Platform bounds priority, tenant composes within it | [`priorityTiers`](operations/tenant-onboarding.md) draws from a platform allowlist, editable without a restart |
| Tenant self-serves without cluster-admin | one [`ActionsGateway`](getting-started.md) provisions controller, proxy, RBAC and policies inside the quota |
| Tenant sees its own data only | [metrics and a dashboard per tenant](operations/observability-dashboards.md), plus a separate platform view |
| Contributor's code is contained | [default-deny egress, per-tenant egress identity, Kata workers](design/05-security.md) |

Each role and what it cannot do: [Personas](operations/personas.md).

## A paved road is worth more than a trail map

- **Feature comparisons** tell you what *can* be done.
- **Reference architectures** prove it has been validated, and tell you how to
  do it.
- **Runbooks, dashboards, and alerts** tell you how to operate it.

Measured 2026-08-06 against ARC 0.14.2 and `master`.

| | GAG | ARC |
|---|---|---|
| Sandboxed workers | [500-line guide](operations/kata-dind-workloads.md), default in GAG's own end-to-end CI, validated on named kernels, no privileged container | same `runtimeClassName` field; no doc covers it, absent from chart values, two closed issues ever |
| Observability | [20 alert rules](operations/observability-alerting.md), [two dashboards for two audiences](operations/observability-dashboards.md), [per-tenant metrics](operations/observability-metrics.md), [redaction before any log line](operations/observability-logging.md) | one per-scale-set sample dashboard, no alert rules, metrics opt-in; [dashboard request open since 2025-01-13](https://github.com/actions/actions-runner-controller/issues/3753) |

## No offering is perfect, yet…

Here is where GAG loses, and to whom. **No single alternative holds all of
these**, which is why the middle column matters as much as the first.

| What GAG lacks | Who has it | Tracked |
|---|---|---|
| Install base | ARC 6,417 stars, ForgeMT 211, GAG 3 (2026-08-06) | see below |
| Commercial support | ARC, and every managed service | none planned, by design |
| Multi-label `runs-on` | ARC, since 0.14.0 (2026-03-19) | Q726 |
| `container:`/`services:` without privilege | ARC, via `containerMode: kubernetes` | Q727 |
| In-cluster cache | GitLab Runner, managed services. **Not ARC** | [worker cache backend](roadmap.md) |
| GHES tested on a real appliance | ARC | [flagged untested](features.md); needs an operator with one |
| Bound GitHub runner group | ARC, via `runnerGroup` | [Q712](roadmap.md), gating the next release |
| Default-tier latency metrics | nobody: a defect, not a rival's feature | [Q713](roadmap.md), gating the next release |

Two footnotes on that table.

**Cache is usually misread.** `actions/cache` works on GAG today; what is
missing is a cache *inside* the cluster. ARC has none either, so this is not a
reason to prefer ARC.

**Install base has no engineering answer.** If being the first production
deployment is unacceptable, that settles it. The only counterweight is the
evidence trail: dated measurements, failure modes documented with the incidents
that found them, and a
[published retraction](design/appendix-a-capacity-slos.md) of the project's own
best number once it proved unmeasurable.

## Reading further

[Appendix D](design/appendix-d-alternatives-considered.md) argues every
alternative in full, including
[Prow](design/appendix-d-alternatives-considered.md#d10-prow-and-the-prior-art-for-automatic-re-run),
which is why GAG does not claim automatic re-run is novel, and
[GitLab Runner](design/appendix-d-alternatives-considered.md#d12-gitlab-runners-kubernetes-executor),
which faces the identical problem and resolves it the other way.
[Why GAG?](why-gag.md) is the capability-by-capability comparison against ARC.
