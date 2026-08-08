# Personas: who owns what

> **Audience:** All

Running GitHub Actions Gateway (GAG) for many teams means more than one role is
involved, and the split between them is not a documentation convention. It is
the product: the platform owns limits a tenant cannot raise, the tenant composes
freely inside them, and neither has to file a ticket with the other for routine
work. Single-tenant runner controllers do not need this distinction, which is
why they generally do not have a primitive for it.

This page defines each role by **what it owns** and, more usefully, **what it
cannot do**. A boundary that a role can talk its way past is not a boundary.

## One boundary is enforced; the rest are just labels

Only one line here is a **privilege** boundary the system enforces: **platform
side versus tenant side.** A tenant operator cannot raise their own quota or
self-grant privilege no matter who they report to, and that is checked by RBAC,
admission, and a quota the control plane cannot write.

Everything else is a routing label for finding the right document.

**On-call is a mode, not a role.** Both sides get paged, for different things:
the platform side when the control plane misbehaves, the tenant side when one
namespace's CI does. Whoever is carrying the pager already appears below under
the side they sit on, so there is no separate on-call persona on this page.

**Job titles are not roles, which is why none of them appear here.** "SRE" was
a tag in this tree until 2026-08-07 and was retired for being ambiguous in the
one way a routing tag cannot afford: both the platform and its tenants need
SREs, so the tag never told a reader which side of the boundary a page was
written for. Retiring it forced every page it covered to declare a side, and
three turned out to be tenant-facing pages that had been filed as platform ones.
"DevOps" was never a tag and should not become one; it is even less specific,
and CI is not an application being operated in production.

A role name here states a scope, a domain, and a responsibility. Read it as one
question: which side of the tenancy boundary are you on?

## The roles

### Platform engineer

Owns the cluster and everything shared in it, and carries the pager when the
control plane itself misbehaves.

**Owns:** the GMC install and upgrades; the `actions-gateway.github.com/tenant`
namespace marker; each tenant namespace's `ResourceQuota`; the
[`PriorityClassAllowlist`](security-operations.md#self-service-additions-via-the-priorityclassallowlist-cr-q188-q298);
`ClusterRunnerTemplate` objects; the Pod Security Admission level and the
`privileged-profile: allowed` grant; whether the cluster has a node autoscaler.
At 03:00 the same person owns the [shipped alerts](observability-alerting.md),
the [runbook](runbook.md), [backup and restore](backup-restore.md), and
upgrades.

**Cannot, by design:** be required in the loop for a tenant's day-to-day runner
changes. If adding a runner shape or adjusting concurrency needs a platform
ticket, the model has failed.

**Needs, specifically:** failure modes that surface as a named condition, an
Event, and a metric rather than as a log line to grep. The
[troubleshooting guide](troubleshooting.md) is organised by observable symptom
for this reason.

**Notably does not own:** the `ResourceQuota` through GAG. The GMC holds no
write verb on `resourcequotas`, so the quota is set on the namespace by the
platform admin and the control plane operates within it. That is what makes the
quota a real cap rather than a suggestion.

### Tenant operator

Owns one namespace's CI end to end: the runner configuration **and** the
workflow files in the repositories being tested. Usually one person per tenant,
sometimes one person across several similar tenants. When that team's CI breaks
at 03:00, this is who looks at it, whatever their job title says.

**That span is the point.** `runs-on:` in a workflow and the `RunnerSet` that
answers it are two halves of one job on one desk. It is why a migration that
needs no workflow edit is worth something concrete to this person, and why a
gap that forces one edit per target
([migrating from ARC](migration-from-arc.md)) is a real cost rather than a
detail. It is also why this role, not the platform engineer, is the one that
has to be able to self-diagnose.

**Owns:** the `ActionsGateway`, its `RunnerSet`s and `RunnerTemplate`s, the
GitHub App credential Secret for their own organization, worker pod shape,
concurrency ceilings, their own `EgressProxy` if they use one, and the
`runs-on` labels their workflows target.

**Cannot, by design:** raise their own `ResourceQuota`; name a `PriorityClass`
outside the platform's allowlist; author a privileged worker template; or read
another tenant's metrics, logs, or Secrets.

**Should be able to:** answer "why is my job queued?" without opening a ticket.
That is the test the [tenant dashboard](observability-dashboards.md#tenant-dashboard)
and the per-tenant metrics exist to pass, and the reason it matters is that the
person asking is also the person who has to fix it.

### Security / compliance

Reads rather than operates, and needs evidence rather than assurances.

**Owns:** the threat model's operational half, admission policy compatibility,
abuse response, and whatever an auditor asks for.

**Needs, specifically:** artifacts produced unprompted. A control that exists
but leaves no record is hard to evidence, which is why per-tenant egress
attribution and admission decisions matter as much as the controls themselves.
[Security operations](security-operations.md) and
[admission policies](admission-policies.md) are the entry points.

### Budget owner

Owns the spend, and usually cannot read the cluster at all.

**Needs, specifically:** cost per tenant, in currency, from data they can
defend in a planning conversation.
[Cost attribution](cost-attribution.md) maps tenant namespaces and
`app.kubernetes.io/*` labels onto OpenCost and Kubecost allocation queries for
exactly this. There is no dashboard for this persona yet; see the
[roadmap](../roadmap.md).

### Maintainer

Internal to GAG itself: cutting releases, the publish pipeline, supply-chain
attestations. See [release.md](release.md). Listed because the docs index tags
it, not because adopters need it.

## The role that is not a user

An **external contributor** to a public repository can cause code to run on your
cluster without being a user of your platform, and a fork pull request is an
arbitrary-code-execution request the CI system is designed to honour. For every
other role above, this one is part of the **threat model** rather than an
audience.

That asymmetry is why isolation is a separate concern from access control: the
question is not what this person is permitted to do, but what their code can
reach if it tries. GAG's stance today is
[Kata micro-VM workers](kata-dind-workloads.md) for trusted CI, with the
untrusted-pull-request posture still in progress. The threat model, the layer
map, and what is explicitly out of scope are in the
[secure multi-tenant OSS CI goal](../plan/secure-multi-tenant-oss-ci.md).

## How personas are recorded

A doc's audience is recorded in two places, deliberately:

1. the **Personas column** in the [operations index](README.md), which drives
   the filter chips; and
2. that doc's own `> **Audience:** …` blockquote, which drives the per-doc pill
   and deep-links back to the filtered index.

They must agree. There is no CI check, so when you retag a doc, update both. Use
the tag names already in use verbatim: an unlisted spelling silently creates a
new chip that matches one document. "Tenant operator" is the correct name for
the namespace-owning role; "Tenant owner" was a drifted variant and has been
normalised.

**Every tag is a role on this page, with one exception.** `All` means every
persona, and a page carrying it stays visible under any chip. This page and the
[glossary](../design/08-glossary.md) use it, which is why the audience line
above names no roles: a page about who the roles are is not addressed to one of
them.

Otherwise, tag with a role name. **Do not reintroduce a job title as a tag.** A
tag exists to route a reader to the right page, so it has to answer "is this
for me?", and a title that both sides of the boundary use cannot. If a new tag
seems necessary, the question to answer first is what scope, domain, and
responsibility it names.
