# Why GAG over ARC?

GitHub Actions Gateway (GAG) targets one situation Actions Runner Controller
(ARC) scale-set mode does not handle well: running **many runner groups for many
tenants in a shared Kubernetes cluster, cost-effectively, under one
`ResourceQuota`**. The compounding problems all point back to cost and
self-service — most importantly, ARC's poor fit with `ResourceQuota` makes
per-tenant quotas unsafe, which is what blocks letting tenants run their own
runners. GAG was built to solve them together.

## The problem

- **`ResourceQuota` is unsafe with ARC — so self-service is too.** When a runner
  pod is preempted, OOM-killed, or simply can't schedule because the namespace
  quota is full, ARC has no built-in flow to fast-cancel the GitHub job lock and
  rerun. The job sits until the lock expires (~10 minutes), then fails and needs
  a manual rerun. Because quota exhaustion turns into failed jobs, teams avoid
  enforcing `ResourceQuota` — exactly the control you need to safely let tenants
  manage their own runner counts.
- **Scheduling starvation under a shared quota.** Each ARC `AutoscalingRunnerSet`
  has its own `maxRunners` cap, but there is no primitive for "GPU runners must
  always claim at least N slots, regardless of how many CPU runners are active."
  Cheap CPU pods exhaust the quota first and the most expensive hardware loses
  the race — so a PR's big tests stall behind a flood of small ones.
- **Listener overhead at scale.** ARC's scale-set listener is one pod per scale
  set running a full .NET runtime — roughly 256 MiB resident plus a cluster IP,
  held alive 24/7 to long-poll GitHub. Ten scale sets cost ~2.5 GiB and 10 pod
  slots at rest, before any job runs.
- **Platform team as bottleneck.** Onboarding a tenant means provisioning a
  namespace, quotas, controller scope, scale sets, NetworkPolicies, and egress —
  a platform-team checklist per team, with every later change landing as a
  ticket.

## GAG vs ARC scale-set mode

| Capability | ARC scale-set mode | GitHub Actions Gateway |
| --- | --- | --- |
| Safe under a per-tenant `ResourceQuota` | Quota-blocked jobs fail; manual rerun | [Auto fast lock-cancel + rerun, per-job budget](design/04-operational-flows.md) |
| Guaranteed floor for critical runner types | No per-quota primitive | [Priority tiers per runner group](design/02-architecture.md) |
| Scale workers to zero between jobs | Yes (`minRunners: 0`) | Yes — workers exist only while a job runs |
| Per-tenant dedicated egress IPs | Shared cluster egress | [Per-tenant HTTPS CONNECT proxy pool](design/network-architecture.md) |
| Listener overhead (10 runner groups, at rest) | ~2.5 GiB across 10 pods | ~600 KiB in 1 shared pod |

GAG also exposes Prometheus metrics scoped per tenant and runner group
([observability](operations/observability.md)) and is, like every entry above,
driven by the single `ActionsGateway` resource shown below.

For limits and Service Level Objectives behind these claims, see
[Appendix A — Capacity Targets & SLOs](design/appendix-a-capacity-slos.md); for
the utilization-and-cost argument, [Appendix F — Cost model](design/appendix-f-cost-model.md).

## One resource, a whole gateway

A tenant declares what they want in a single namespace-scoped resource. The
Gateway Manager Controller (GMC) provisions the controller, proxy pool, RBAC,
network policies, and quota to match — no cluster-admin involvement after the
initial GMC install.

```yaml
apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: team-a-gateway
  namespace: team-a
spec:
  gitHubAppRef:
    name: my-github-app          # (1)!
  gitHubURL: https://github.com/team-a-org
  securityProfile: baseline      # (2)!
  proxy:
    minReplicas: 2               # (3)!
    maxReplicas: 10
  # No namespaceQuota field: the ResourceQuota is platform-owned (4)!
  runnerGroups:
    - name: gpu-runners
      runnerLabels: ["self-hosted", "gpu"]
      maxListeners: 10
      priorityTiers:             # (5)!
        - priorityClassName: runner-critical
          threshold: 5
          preemptionPolicy: PreemptLowerPriority
        - priorityClassName: runner-standard
          threshold: 20
          preemptionPolicy: Never
      podTemplate:
        spec:
          containers:
            - name: runner
              resources:
                limits:
                  nvidia.com/gpu: "1"
    - name: cpu-runners
      runnerLabels: ["self-hosted", "linux"]
      maxWorkers: 30
      podTemplate:
        spec:
          containers:
            - name: runner
```

1.  References a `Secret` in the same namespace holding the GitHub App `appId`,
    `installationId`, and `privateKey`. The GMC watches the reference name, not
    the Secret contents — see [credential rotation](getting-started.md#rotating-github-app-credentials).
2.  Selects the Pod Security Admission level the GMC stamps on the namespace.
    Defaults to `baseline`; use `restricted` for stricter isolation or
    `privileged` only for workloads like docker-in-docker. See
    [Security](design/05-security.md).
3.  The per-tenant egress proxy pool is HPA-managed between these bounds; all
    GitHub traffic exits through it on dedicated IPs.
4.  The single `ResourceQuota` every runner group shares is **platform-owned** —
    the platform admin sets it on the namespace, not on this CR, so it is a real
    cap the tenant cannot raise. Priority tiers decide who wins when it is
    contended.
5.  The first 5 GPU pods get a preempting `PriorityClass` and will displace
    lower-priority CPU pods; the next tier bursts opportunistically without
    evicting running jobs; the final threshold caps total concurrency.

Ready to try it? Follow the [getting-started guide](getting-started.md).
