# Why GAG over ARC?

GitHub Actions Gateway (GAG) targets one situation Actions Runner Controller
(ARC) scale-set mode does not address as a whole: running **many runner groups
for many tenants in a shared Kubernetes cluster under one `ResourceQuota`**.
Four problems compound there — scheduling starvation, listener overhead, no
eviction recovery, and the platform team as an onboarding bottleneck. GAG was
built to solve all four together.

## The problem

- **Scheduling starvation under a shared quota.** Each ARC `AutoscalingRunnerSet`
  has its own `maxRunners` cap, but there is no primitive for "GPU runners must
  always claim at least N slots, regardless of how many CPU runners are active."
  Cheap CPU pods exhaust the quota first and the most expensive hardware loses
  the race.
- **Listener overhead at scale.** ARC's scale-set listener is one pod per scale
  set running a full .NET runtime — roughly 256 MiB resident plus a cluster IP,
  held alive 24/7 to long-poll GitHub. Ten scale sets cost ~2.5 GiB and 10 pod
  slots at rest, before any job runs.
- **No automatic recovery from eviction.** When a runner pod is preempted,
  OOM-killed, or lost to a node failure, ARC has no built-in flow to fast-cancel
  the GitHub job lock and rerun. The job sits until the lock expires (~10
  minutes), then surfaces as a failed workflow needing a manual rerun.
- **Platform team as bottleneck.** Onboarding a tenant means provisioning a
  namespace, quotas, controller scope, scale sets, NetworkPolicies, and egress —
  a platform-team checklist per team, with every later change landing as a
  ticket.

## GAG vs ARC scale-set mode

| Capability | ARC scale-set mode | GitHub Actions Gateway |
| --- | --- | --- |
| Scale workers to zero between jobs | Yes (`minRunners: 0`) | Yes — workers exist only while a job runs |
| Priority floor for GPU runners under a shared quota | No per-quota primitive | [Priority tiers per runner group](design/02-architecture.md) |
| Automatic eviction retry (fast lock-cancel + rerun) | Manual rerun after lock expiry | [Built-in, with a per-job retry budget](design/04-operational-flows.md) |
| Per-tenant dedicated egress IPs | Shared cluster egress | [Per-tenant HTTPS CONNECT proxy pool](design/network-architecture.md) |
| Listener overhead (10 runner groups, at rest) | ~2.5 GiB across 10 pods | ~600 KiB in 1 shared pod |
| Self-service tenant onboarding | Platform-team checklist per team | [One `ActionsGateway` resource](getting-started.md) |
| Per-tenant utilization metrics | Cluster-wide visibility | [Prometheus metrics scoped per tenant + group](operations/observability.md) |

For limits and Service Level Objectives behind these claims, see
[Appendix A — Capacity Targets & SLOs](design/appendix-a-capacity-slos.md); for
the utilization-and-cost argument, [Appendix F — Cost model](design/appendix-f-cost-model.md).

## One resource, a whole gateway

A tenant declares what they want in a single namespace-scoped resource. The
Gateway Manager Controller (GMC) provisions the controller, proxy pool, RBAC,
network policies, and quota to match — no cluster-admin involvement after the
initial GMC install.

```yaml
apiVersion: actions.gateway/v1alpha1
kind: ActionsGateway
metadata:
  name: team-a-gateway
  namespace: team-a
spec:
  gitHubAppRef:
    name: my-github-app          # (1)!
  securityProfile: baseline      # (2)!
  proxy:
    minReplicas: 2               # (3)!
    maxReplicas: 10
  namespaceQuota:                # (4)!
    requests.cpu: "20"
    requests.memory: "40Gi"
    pods: "50"
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
4.  The single `ResourceQuota` every runner group shares. Priority tiers decide
    who wins when it is contended.
5.  The first 5 GPU pods get a preempting `PriorityClass` and will displace
    lower-priority CPU pods; the next tier bursts opportunistically without
    evicting running jobs; the final threshold caps total concurrency.

Ready to try it? Follow the [getting-started guide](getting-started.md).
