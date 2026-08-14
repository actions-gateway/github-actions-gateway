# Features

Everything GitHub Actions Gateway (GAG) does today, with a link to the doc that explains each one.
For the argument against Actions Runner Controller (ARC), see [Why GAG?](why-gag.md); for what is not here yet, see the [roadmap](roadmap.md).

Three badges appear below. <span class="gag-v2-badge">v2</span> marks a capability available only in the `actions-gateway.com/v2beta1` API; <span class="gag-maturity-badge">beta</span> marks one whose API shape is still under its first stability contract; and <span class="gag-tier-badge">partly classic-only</span> marks one that does not reach the ScaleSet acquisition tier every new tenant runs.
No tier badge means both tiers, and a gate removes the badge when the gap closes.

!!! tip "Check the version you're running"

    Use the **version selector** at the top of the page to switch between the latest stable [release](https://github.com/actions-gateway/github-actions-gateway/releases) (the default) and **`dev`**, the unreleased `main` branch.
    A capability listed under `dev` but not under a numbered release has not shipped in a tagged chart yet.

## Job intake and recovery

- **[Runner-scale-set acquisition](design/04-operational-flows.md#42-job-execution-flow-agc)**: the same single-acquirer protocol ARC uses, with no many-acquirers fan-out.
  The default in v2.
- **[Quota-aware intake](design/04-operational-flows.md#42-job-execution-flow-agc)**: a job the namespace `ResourceQuota` has no room for is never taken on, so it stays queued at GitHub until there is capacity.
- **[Multi-label runner sets](operations/troubleshooting.md#jobs-targeting-one-of-a-runner-sets-labels-never-start-runnerlabelsincomplete)** <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">beta</span>: every `runnerLabel` is registered at GitHub, so a job asking for any of them matches and a `runs-on` array migrates unedited.
- **[Auto re-run for disrupted jobs](operations/troubleshooting.md#which-disruptions-auto-re-run-a-job-and-which-never-do)**: a worker lost to eviction, preemption, a node drain, or a bare `kubectl delete pod` has its run re-run automatically, under a per-run budget.
- **[Capacity gate for unplaceable workers](operations/troubleshooting.md#runnerset-reports-workercapacitydeclined-the-gateway-stopped-claiming-jobs)**: opt-in.
  Stop claiming jobs while the cluster cannot place the worker shape, instead of claiming and cancelling them.
  Off by default.
- **[Fast, honest ending for an abandoned run](design/04-operational-flows.md)**: a run whose worker is removed before it started is force-cancelled in about a second, measured live, then re-run automatically once capacity returns.
- **[Priority tiers per runner set](design/02-architecture.md)**: reserve a guaranteed floor of slots for expensive runner types so cheap CPU jobs cannot starve critical GPU work.
- **[Worker scale-up rate limiting](operations/tenant-onboarding.md#step-2-create-the-actionsgateway-resource)**: opt-in token bucket capping how *fast* workers start, distinct from the count ceiling, to smooth cold-start stampedes on shared egress.
- **[Scale-to-zero workers](design/02-architecture.md)**: worker pods exist only while a job runs; listeners are ~12 KiB goroutines in one shared pod, not a listener pod per runner group.
- **[Unmodified upstream runner images](operations/tenant-onboarding.md#step-2-create-the-actionsgateway-resource)**: the wrapper is injected into each worker pod at runtime (an OCI image volume on Kubernetes 1.33+, an initContainer below), so the stock `actions/runner` image, or any derivative, runs with no rebuild.
- **[A job conclusion survives losing the process](design/02-architecture.md)**: conclusions are persisted before any message delete is issued, so a hard kill leaves the message to be replayed rather than the conclusion lost.
  Measured over 60 graceful stops at maximum pressure.
- **[Orphaned workers are bounded without a live controller](design/02-architecture.md)**: `maxWorkerLifetime` is stamped on each worker pod as its `activeDeadlineSeconds`, so the **kubelet** enforces it.
  The motivating incident stranded 82 spot node-hours across 16 hours with the controller down.

## Capacity, cost, and right-sizing

- **[Measured worker right-sizing](operations/worker-rightsizing.md)** <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">beta</span>: per-job CPU/memory peaks sampled and turned into recommended `requests`/`limits` in `RunnerSet` status, with an advisory `SizingDrift` condition.
- **[Sizing profiles](operations/worker-rightsizing.md)** <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">beta</span>: opt-in `Binpack`, `Throughput`, and `NodeShare` profiles apply the measurement at pod-build time, with clamps, a confidence fallback, and GPUs never touched.
- **[Managed AGC right-sizing](operations/tenant-onboarding.md#letting-an-autoscaler-size-the-agc-agcautoscaling)** <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">beta</span>: opt-in `agcAutoscaling` stamps a `VerticalPodAutoscaler` next to a gateway's Actions Gateway Controller (AGC) so requests track observed usage; explicit `agcResources` become the floor and ceiling, and a missing VPA install degrades to an advisory condition.
- **[`ResourceQuota` sizing guide](operations/resourcequota-sizing.md)**: turn runner shapes and concurrency ceilings into the quota numbers a platform admin sets, including what the quota actually counts.
- **[Per-tenant cost attribution](operations/cost-attribution.md)**: map tenant namespaces and `app.kubernetes.io/*` labels to OpenCost/Kubecost allocation queries for real dollars per tenant.
- **[Savings calculator](design/appendix-f-cost-model.md#f5-savings-calculator-this-system-vs-arc)**: the interactive cost model behind the ARC comparison.
- **[Node shutdown budgets](operations/node-shutdown-budgets.md)**: how much shutdown time GKE, EKS, AKS, RKE2, Kubespray, and OpenShift actually grant, and why proxy pools stay off spot capacity.

## Tenant isolation and egress

- **[Bound GitHub runner group](operations/tenant-onboarding.md#bind-a-runner-set-to-a-github-runner-group)** <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">beta</span>: `runnerGroup`, or `defaultRunnerGroup` once on the gateway, registers a tenant's scale sets into a named group rather than the installation default, so only the repositories that group admits can route jobs in.
  An unknown group fails the set closed.
- **[Per-tenant egress IPs](design/network-architecture.md)**: a dedicated proxy pool per tenant gives each team its own GitHub egress IPs to allow-list, with a contained blast radius.
- **[Standalone `EgressProxy`](operations/migration-v1-to-v2.md)** <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">beta</span>: the proxy becomes its own object, optionally shared, or omitted entirely for direct egress, which stays `NetworkPolicy`-restricted.
- **[Cross-namespace proxy sharing](operations/security-operations.md#sharing-an-egress-proxy-across-namespaces)** <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">beta</span>: one pool can serve several namespaces, but only those its owner names in `sharing.allowedNamespaces`.
  Consent is provider-side, so naming a proxy from the consumer side grants nothing; unlisted stays denied, and only the proxy's public certificate crosses the namespace boundary.
- **[FQDN egress policy](operations/security-operations.md#expressing-github-egress-by-fqdn-the-egresspolicymode-opt-in)**: express GitHub egress by hostname instead of CIDR on Cilium, Calico, or GKE Dataplane V2.
- **[Auto-refreshed GitHub egress rules](operations/troubleshooting.md#actionsgateway-reports-egressrulesstale)**: the Gateway Manager Controller (GMC) re-reads GitHub's published IP ranges every 24 hours into each tenant's `NetworkPolicy`, and an `EgressRulesStale` condition, with a paging alert, fires when the refresh stalls past its window.
- **[Bring your own proxy autoscaler](operations/migration-v1-to-v2.md)** <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">beta</span>: `managedAutoscaling: false` hands the proxy pool to KEDA, VPA, or a custom HorizontalPodAutoscaler.
- **[Service mesh coexistence](operations/service-mesh-coexistence.md)**: run alongside Istio, Linkerd, or ambient mode with injection opt-out and egress exclusions that keep the per-tenant proxy honored.
- **[One resource per tenant](operations/tenant-onboarding.md)**: a single `ActionsGateway` provisions an isolated controller, proxy pool, RBAC, and network policies inside the platform-owned quota.

## Security posture

- **[Secure-by-default hardening](design/05-security.md)**: Pod Security Admission per namespace, default-deny NetworkPolicies, and credentials kept out of environment variables, all reconciled rather than opt-in.
- **[Runner template library](operations/runner-template-library.md)**: three shipped worker pod shapes (`plain`, `kata-dind`, `privileged-dind`), each applied with one `kubectl apply -k`, so a tenant starts from a validated template instead of transcribing a capability set by hand.
  Only templates CI exercises may ship, and a gate enforces it.
- **[Kata micro-VM workers](operations/kata-dind-workloads.md)**: validated on nested virtualization, and the default for GAG's own end-to-end CI, which builds a `kind` cluster inside an unprivileged worker pod.
- **[In-runner image builds](operations/in-runner-image-builds.md)**: a decision table mapping BuildKit rootless, Kaniko, Sysbox, Kata, and privileged Docker-in-Docker to the right `securityProfile` and PSA level.
- **[Signed images, SBOM, and SLSA provenance](operations/release.md)**: every published image is keyless-signed and carries both a Software Bill of Materials (SBOM) attestation and a Supply-chain Levels for Software Artifacts (SLSA) build-provenance attestation.
- **[Admission policy compatibility](operations/admission-policies.md)**: a Kyverno/Gatekeeper matrix covering whether GAG pods comply with common cluster policies, plus sample enforce and exception policies.
- **[Optional CONNECT destination allow-listing](operations/security-operations.md)**: defense in depth, off by default.
  An opted-in proxy refuses a CONNECT outside the permitted set and counts each refusal as an alertable Server-Side Request Forgery (SSRF) signal.
  The mandatory default-deny NetworkPolicy stays the primary gate.
- **[Self-confining controller privileges](design/05-security.md#gmc-privilege-escalation-blast-radius-and-compensating-controls)**: shipped `ValidatingAdmissionPolicy` guards deny the GMC's own cluster-wide writes outside admin-marked tenant namespaces, so a compromised manager cannot touch `kube-system` or any unmarked namespace.
- **[Restart-free platform allowlists](operations/security-operations.md#self-service-additions-via-the-priorityclassallowlist-cr-q188-q298)**: grow the worker and infra PriorityClass allowlists and the egress-destination allowlist by editing a watched `PriorityClassAllowlist` custom resource (CR) or ConfigMap, no GMC restart; a missing, invalid, or overlapping object fails safe back to the flag baseline.
- **[Credential redaction in logs](operations/observability-logging.md#what-never-appears-in-logs)**: every GitHub response body passes one sanitizer that strips tokens, JWTs, and JIT configs before it can reach a log line, at every log level.
- **[Abuse detection and response](operations/security-operations.md)**: the threat model's abuse heuristics mapped to operator alerts, with compromise-response playbooks.
- **[Workload-identity credentials](design/05-security.md#57-workload-identity-the-no-pem-delegation-model)** <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">beta</span>: mint short-lived GitHub credentials through an external signer so the GitHub App private key never enters the cluster.

## Observability

- **[Metrics reference](operations/observability-metrics.md)**: every Prometheus metric the GMC, AGC, and proxy export, scoped per tenant and runner group.
- **[Fleet rollups for platform admins](operations/observability-metrics.md#full-metrics-reference)**: cross-tenant degraded, egress-stale, and quota gauges in a single pane.
- **[Scraping setup](operations/observability-metrics-access.md)**: wiring the mutual-TLS metrics endpoints into your Prometheus.
- **[Alerting and SLOs](operations/observability-alerting.md)**: ready-to-apply alert rules as code.
- **[Grafana dashboards](operations/observability-dashboards.md)**: a tenant dashboard and a platform dashboard, both as code.
- **[Logging and tracing](operations/observability-logging.md)**: structured logs and OpenTelemetry tracing across the four tiers.

## Install and day-2 operations

- **[Helm install](operations/install.md)**: the OCI chart, digest pinning, healthy-install verification, and a `scripts/e2e/validate-cluster.sh` pre-flight that fails loudly on a network-policy-less CNI.
- **[Highly available manager](operations/install.md#verify-a-healthy-install)**: the GMC installs as two replicas with leader election, a `PodDisruptionBudget`, and anti-co-location spread by default, releasing its lease on shutdown so failover takes seconds rather than a lease timeout.
- **[Air-gapped install](operations/air-gapped-install.md)**: relocate images and the OCI chart to a private registry with digests preserved, including pull Secrets for the runtime pods.
- **[GitOps install](operations/gitops.md)**: declarative Argo CD `Application` and Flux `HelmRelease` examples, with the CustomResourceDefinition (CRD) pruning gotcha handled.
- **[Upgrade and rollback](operations/upgrade.md)**: versioned upgrade procedures and the rollback path for each release.
- **[Stale-CRD startup check](operations/troubleshooting.md#gmc-exits-at-startup-an-installed-crd-schema-is-older-than-the-gmc)** <span class="gag-new-badge">new in 1.5</span>: the manager refuses to start when an installed CRD no longer declares a field that bounds tenant access, so a skipped CRD apply cannot leave `runnerGroup` accepted and silently pruned.
- **[Runner-version drift warning](operations/troubleshooting.md#worker-image-runner-version)**: a worker image below GitHub's enforced minimum is reported before GitHub enforces it, and an image whose reference names no version says so rather than passing.
- **[Backup and restore](operations/backup-restore.md)**: backup posture and a recovery runbook, with a [Velero-specific how-to](operations/velero-backup-restore.md).
- **[Troubleshooting](operations/troubleshooting.md)**: symptom to diagnosis to remediation, organised by observable failure mode.
- **[Production runbook](operations/runbook.md)**: the operational procedures the platform team needs on call.
- **[P2P image distribution](operations/p2p-image-distribution.md)**: add a Spegel or Dragonfly mirror to survive ephemeral-worker pull storms.

## API surface and migration

- **[The v2 API](operations/migration-v1-to-v2.md)** <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">beta</span>: the recommended shape for new tenants: a decomposed `ActionsGateway` + `RunnerSet` + `RunnerTemplate`, with `v2beta1` as the graduated storage and hub version.
- **[Reusable runner templates](operations/migration-v1-to-v2.md)** <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">beta</span>: one `RunnerTemplate` referenced by many runner sets, or a cluster-wide `ClusterRunnerTemplate` shared across namespaces.
- **[Multiple gateways per namespace](operations/migration-v1-to-v2.md)** <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">beta</span>: scoped gateways coexist, each with its own GitHub binding and runner sets.
- **[GitHub Enterprise Server gateways](operations/troubleshooting.md#a-ghes-tenants-traffic-never-reaches-the-appliance)**: a gateway whose `gitHubURL` names a GHES appliance addresses that appliance on every GitHub surface, with a `GitHubEgressIncomplete` condition flagging an incomplete CIDR allow-list. **Untested against a real GHES appliance.**
- **[GHES behind a private CA](operations/troubleshooting.md#a-ghes-appliances-certificate-is-not-trusted)** <span class="gag-v2-badge">v2</span>: `spec.githubCABundleRef` names a ConfigMap whose CA bundle is added to the trust of both the gateway's AGC and its worker pods, never replacing the system roots. **Untested against a real GHES appliance.**
- **[`gag-migrate`](operations/migration-v1-to-v2.md)**: a one-shot fan-out that moves a tenant off `v1alpha1` without changing how jobs are acquired: dry-run, review, apply.
- **[Deprecations and the `v2.0.0` removal](operations/v1alpha1-deprecation.md)**: what `v2.0.0` removes, what keeps working until then, and the pre-upgrade checklist.
- **[Migrating from ARC](operations/migration-from-arc.md)**: concept mapping, behavioral differences, and a worked zero-downtime migration of one runner group.
- **[The GitHub protocol dependency register](design/github-protocol-dependencies.md)**: each GitHub runner-facing protocol this system speaks, its source of truth, a stability tier, and the drift-watch trigger for revisiting it.
  One of the two is Public Preview with no wire specification.
- **[Getting started](getting-started.md)**: first-time GitHub App setup, the v2 object set, and credential rotation.
  There is also a [recorded demo](demo.md) of one real job on a local kind cluster.
