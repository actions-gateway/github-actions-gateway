# Personas: who owns what

> **Audience:** Platform engineer, SRE, Security, Budget owner, Tenant operator

Running GitHub Actions Gateway (GAG) for many teams means more than one role is
involved, and the split between them is not a documentation convention. It is
the product: the platform owns limits a tenant cannot raise, the tenant composes
freely inside them, and neither has to file a ticket with the other for routine
work. Single-tenant runner controllers do not need this distinction, which is
why they generally do not have a primitive for it.

This page defines each role by **what it owns** and, more usefully, **what it
cannot do**. A boundary that a role can talk its way past is not a boundary.

## The roles

### Platform engineer

Owns the cluster and everything shared in it.

**Owns:** the GMC install and upgrades; the `actions-gateway.github.com/tenant`
namespace marker; each tenant namespace's `ResourceQuota`; the
[`PriorityClassAllowlist`](security-operations.md#self-service-additions-via-the-priorityclassallowlist-cr-q188-q298);
`ClusterRunnerTemplate` objects; the Pod Security Admission level and the
`privileged-profile: allowed` grant; whether the cluster has a node autoscaler.

**Cannot, by design:** be required in the loop for a tenant's day-to-day runner
changes. If adding a runner shape or adjusting concurrency needs a platform
ticket, the model has failed.

**Notably does not own:** the `ResourceQuota` through GAG. The GMC holds no
write verb on `resourcequotas`, so the quota is set on the namespace by the
platform admin and the control plane operates within it. That is what makes the
quota a real cap rather than a suggestion.

### Tenant operator

Owns one namespace's CI, and is usually an SRE or developer-experience engineer
on a product team rather than a platform specialist.

**Owns:** the `ActionsGateway`, its `RunnerSet`s and `RunnerTemplate`s, the
GitHub App credential Secret for their own organization, worker pod shape,
concurrency ceilings, and their own `EgressProxy` if they use one.

**Cannot, by design:** raise their own `ResourceQuota`; name a `PriorityClass`
outside the platform's allowlist; author a privileged worker template; or read
another tenant's metrics, logs, or Secrets.

**Should be able to:** answer "why is my job queued?" without opening a ticket.
That is the test the [tenant dashboard](observability-dashboards.md#tenant-dashboard)
and the per-tenant metrics exist to pass.

### SRE / on-call

Owns keeping it running, often across tenants, often at an unsociable hour.

**Owns:** responding to the [shipped alerts](observability-alerting.md), the
[runbook](runbook.md), [backup and restore](backup-restore.md), and upgrades.

**Needs, specifically:** a failure mode that surfaces as a named condition, an
Event, and a metric rather than as a log line to grep. The
[troubleshooting guide](troubleshooting.md) is organised by observable symptom
for this reason.

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
the role names on this page verbatim: an unlisted spelling silently creates a
new chip that matches one document. "Tenant operator" is the correct name for
the namespace-owning role; "Tenant owner" was a drifted variant and has been
normalised.
