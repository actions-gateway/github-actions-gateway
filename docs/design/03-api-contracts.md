# 3. API & Data Contract Specifications

← [Core Architecture](02-architecture.md) | [Back to index](README.md) | Next: [Operational Flows →](04-operational-flows.md)

---

## Table of Contents

- [3.1. Kubernetes CRD Schemas](#31-kubernetes-crd-schemas)
- [3.2. GitHub App Credentials Secret Schema](#32-github-app-credentials-secret-schema)
- [3.3. Re-implemented Broker API Endpoints](#33-re-implemented-broker-api-endpoints)
- [3.4. Broker Payload Blueprints (Go Structs)](#34-broker-payload-blueprints-go-structs)
- [3.5. GitHub API Rate Limit Budget](#35-github-api-rate-limit-budget)

## 3.1. Kubernetes CRD Schemas

Two Custom Resource Definitions are introduced. `ActionsGateway` is namespace-scoped and owned by the GMC. `RunnerGroup` is namespace-scoped and owned by the AGC. Both live in the tenant's namespace. The GMC creates `RunnerGroup` resources as part of AGC bootstrapping.

> **v2 API (`v2alpha1`, group `actions-gateway.com`).** A decomposed v2 API is served side by side with this `v1alpha1` (`actions-gateway.github.com`) surface during the coexistence window. It is **fully shipped** (milestones M1–M5): the five kinds and their GMC/AGC reconcilers, multiple gateways per namespace, the namespace-scoped security profile, and the one-shot v1→v2 migration tool are all built. `v2alpha1` remains an **alpha** API, and tenants migrate on their own schedule via the migration tool — nothing in the `v1alpha1` surface below changes. v2 splits the `ActionsGateway` + `RunnerGroup` monolith into five kinds (`ActionsGateway`, `RunnerSet`, `RunnerTemplate`, `ClusterRunnerTemplate`, `EgressProxy`), renames the group to the project-owned domain, and freezes the field-naming/immutability decisions. The full v2 shape and rationale are the design source of truth in [Appendix H — v2 API Decomposition](appendix-h-v2-api-decomposition.md); the milestone sequencing is in the [v2 API plan](../plan/v2-api.md). The schemas below remain authoritative for the running `v1alpha1` API.

```go
// SecretReference is a pointer to a Kubernetes Secret, with an optional
// namespace override. Because ActionsGateway is namespace-scoped, Namespace
// defaults to the namespace of the ActionsGateway CR itself when omitted.
type SecretReference struct {
    // Name is the name of the Secret object.
    Name string `json:"name"`

    // Namespace is the namespace the Secret lives in.
    // Defaults to the namespace of the ActionsGateway CR when omitted,
    // so tenants can supply their own credentials without platform team
    // involvement. Override only if the Secret is managed centrally.
    // +optional
    Namespace string `json:"namespace,omitempty"`
}

// ProxyConfig configures the per-tenant egress proxy pool and its autoscaler.
type ProxyConfig struct {
    // MinReplicas is the minimum number of proxy pods the HPA will maintain.
    // Must be >= 1. Defaults to 2 to ensure availability across node failures.
    // +optional
    // +kubebuilder:default=2
    MinReplicas *int32 `json:"minReplicas,omitempty"`

    // MaxReplicas is the upper bound the HPA may scale the proxy pool to.
    // Should be sized to handle peak concurrent job egress. Defaults to 10.
    // +optional
    // +kubebuilder:default=10
    MaxReplicas *int32 `json:"maxReplicas,omitempty"`

    // TargetCPUUtilizationPercentage is the average CPU utilization across
    // proxy pods that the HPA targets when scaling. Defaults to 60.
    // +optional
    // +kubebuilder:default=60
    TargetCPUUtilizationPercentage *int32 `json:"targetCPUUtilizationPercentage,omitempty"`

    // Resources sets CPU and memory requests/limits for proxy pods.
    // Resource requests are required for the HPA to compute CPU utilization
    // percentages — without them the HPA metric shows as <unknown> and
    // autoscaling does not function.
    // Defaults: requests 10m CPU / 32Mi memory; limits 500m CPU / 64Mi memory.
    // +optional
    Resources corev1.ResourceRequirements `json:"resources,omitempty"`

    // NoProxyCIDRs is a list of destinations appended to the NO_PROXY environment
    // variable injected into the AGC and worker pods, excluding them from the
    // per-tenant egress proxy. Entries may be CIDR prefixes ("10.0.0.0/8"), bare
    // IPs, or NO_PROXY domain suffixes for internal destinations
    // ("svc.cluster.local", "internal.example.com"). The admission webhook rejects
    // any entry that would route the tenant's GitHub traffic around the proxy — a
    // hostname matching the configured gitHubURL host or the public GitHub domains
    // (github.com, githubusercontent.com, ghcr.io, …) — because that silently
    // defeats egress-IP attribution. Never list GitHub here. A CIDR/IP covering
    // GitHub's rotating ranges is not detected and stays the operator's
    // responsibility.
    // Cluster-internal destinations are appended automatically by the GMC
    // (svc.cluster.local, localhost, 127.0.0.1, 10.96.0.0/12); set this field only
    // to add a non-default service CIDR.
    // NOTE: 10.96.0.0/12 is the kubeadm default service CIDR. EKS uses 10.100.0.0/16;
    // GKE and other providers may differ. Operators must override this field (with a
    // CIDR) when the cluster service CIDR does not fall within 10.96.0.0/12.
    // To discover the value: kubectl cluster-info dump | grep -m1 service-cluster-ip-range
    // +optional
    NoProxyCIDRs []string `json:"noProxyCIDRs,omitempty"`

    // ManagedNetworkPolicy controls whether the GMC automatically refreshes
    // proxy pod NetworkPolicy egress rules from api.github.com/meta every 24h.
    // Set to false when using FQDN-based egress policies (Cilium, Calico).
    // Defaults to true.
    // +optional
    // +kubebuilder:default=true
    ManagedNetworkPolicy *bool `json:"managedNetworkPolicy,omitempty"`
}

// ActionsGateway is a namespace-scoped CRD managed by the GMC.
// Tenants create it in their own namespace to provision a gateway instance.
// One instance per namespace is supported.
type ActionsGatewaySpec struct {
    // GitHubAppRef points to a Secret containing the tenant's GitHub App
    // private key and App ID.
    //
    // If GitHubAppRef.Namespace is omitted it defaults to the namespace of
    // the ActionsGateway CR, so a tenant can create the Secret alongside the
    // CR and manage credential rotation themselves.
    GitHubAppRef SecretReference `json:"gitHubAppRef"`

    // GitHubURL is the GitHub organization, enterprise, or repository URL this
    // gateway's runners register against — e.g. "https://github.com/my-org",
    // "https://github.com/my-org/my-repo", or, for GitHub Enterprise Server,
    // "https://ghes.example.com/my-org". It is REQUIRED: a gateway with no URL
    // has nothing to register against. The GMC threads it to the AGC Deployment
    // as the GITHUB_ORG_URL environment variable, which the AGC's GithubRegistrar
    // reads to derive its org-scoped vs repo-scoped REST endpoints. It pairs with
    // gitHubAppRef — the App installation must cover the same org/enterprise.
    //
    // This field is the production-supported replacement for the testing-only
    // --allow-agc-extra-env=AGC_EXTRA_GITHUB_ORG_URL injection path (that flag
    // remains, gated, for genuinely-extra env only). Structural validation (https
    // scheme, host present, an org/owner path segment) is performed by the GMC
    // validating webhook so the error can name the offending component; the CRD
    // carries a Pattern (^https://) as a cheap scheme guard.
    //
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=2048
    // +kubebuilder:validation:Pattern=`^https://`
    GitHubURL string `json:"gitHubURL"`

    // Proxy configures the egress proxy pool deployed in front of the AGC
    // and worker pods. All GitHub traffic routes through this pool.
    // +optional
    Proxy ProxyConfig `json:"proxy,omitempty"`

    // RunnerGroups defines the initial set of RunnerGroup specs the GMC
    // bootstraps inside the tenant namespace on creation.
    RunnerGroups []RunnerGroupSpec `json:"runnerGroups,omitempty"`

    // There is deliberately no namespace-quota field. The namespace
    // ResourceQuota (and any LimitRange) is platform-owned: the platform admin
    // creates and manages it on the tenant namespace, and the gateway operates
    // within it without ever creating or mutating it. A tenant-authored quota
    // was removed pre-1.0 (Q130) because a tenant-set quota is no real cap — the
    // tenant could raise it — and it fought GitOps/platform ownership. See
    // §5 (Security) and operations/tenant-onboarding.md.

    // SecurityProfile selects the Pod Security Admission level the GMC
    // stamps on the tenant namespace at provisioning time. The chosen
    // level applies to every pod the AGC creates (workers, sidecars in
    // the PodTemplate) and is enforced by the in-tree PodSecurity
    // admission plugin built into the Kubernetes API server — no
    // external policy engine (Kyverno, OPA Gatekeeper) is required.
    //
    //   - baseline   (default) — blocks privileged containers, host
    //                  namespaces (PID/IPC/Network), hostPath volumes,
    //                  dangerous capabilities (SYS_ADMIN, NET_ADMIN,
    //                  etc.), hostPort, /proc mount manipulations.
    //                  Suitable for normal CI workloads.
    //   - restricted — all of baseline, plus requires runAsNonRoot,
    //                  drops ALL capabilities, requires seccompProfile
    //                  RuntimeDefault, forbids allowPrivilegeEscalation.
    //                  Use for tenants with strict isolation requirements.
    //   - privileged — no admission-level restrictions. Required for
    //                  workloads that need privileged containers, host
    //                  namespaces, or specific capabilities — most
    //                  commonly docker-in-docker (DinD), Buildah without
    //                  a sandbox runtime, or kernel-module workflows.
    //                  Tenants choosing this level SHOULD pair it with
    //                  runtimeClassName: kata-containers or gvisor on
    //                  the RunnerGroup PodTemplate to recover isolation
    //                  via sandboxed runtime (see Appendix B).
    //
    // Tenants needing both privileged and non-privileged workloads
    // deploy two ActionsGateway CRs in two namespaces and route
    // workflows to the appropriate one via runs-on: labels. Per-tenant
    // namespaces are the unit at which the profile is chosen.
    //
    // The GMC writes these labels on the tenant namespace:
    //   pod-security.kubernetes.io/enforce: <securityProfile>
    //   pod-security.kubernetes.io/enforce-version: latest
    //   pod-security.kubernetes.io/warn:    <securityProfile>
    //   pod-security.kubernetes.io/audit:   <securityProfile>
    //
    // The AGC continues to enforce its own invariants regardless of the
    // selected profile: hostPID/hostNetwork/hostIPC are forced to false
    // and automountServiceAccountToken to false on every worker pod.
    // PSA is the safety net; the AGC's invariants are the floor.
    //
    // securityProfile may be UPGRADED in place (e.g. baseline -> restricted)
    // freely. A DOWNGRADE (relaxing isolation, e.g. restricted -> baseline, or
    // anything -> privileged) is rejected by the GMC validating webhook unless
    // the object carries the annotation
    // actions-gateway.github.com/allow-profile-downgrade: "true". This keeps a
    // stray re-apply — or a manifest that drops the field and lets it
    // re-default to baseline — from silently weakening a tenant, while still
    // letting an operator roll back a failed hardening attempt with a two-field
    // edit rather than recreating the ActionsGateway. The webhook is used (not
    // a CRD CEL rule) because the decision reads metadata.annotations, which a
    // spec-scoped CEL XValidation rule cannot see. gitHubAppRef.name is left
    // mutable so credential rotation by Secret name keeps working — see the
    // Secret-rotation note later in this document.
    //
    // v2 note: the v2alpha1 (actions-gateway.com) API REMOVES this field. There,
    // the Pod Security level is selected by the namespace label
    // actions-gateway.com/security-profile (GMC-guarded), because PSA is
    // namespace-scoped and co-located v2 gateways share one posture. This v1
    // contract is unchanged and accurate for v1alpha1; see appendix-h §H.16 #7.
    //
    // +optional
    // +kubebuilder:default=baseline
    // +kubebuilder:validation:Enum=baseline;restricted;privileged
    SecurityProfile string `json:"securityProfile,omitempty"`

    // LogLevel sets the log verbosity of this tenant's AGC and egress proxy:
    // info (default) or debug. The GMC threads it to both workloads as the
    // LOG_LEVEL environment variable, exactly as securityProfile flows as
    // SECURITY_PROFILE. The proxy reads LOG_LEVEL directly; the AGC reads it to
    // set its zap level unless an explicit --zap-log-level flag is passed (the
    // GMC never stamps one). Changing it is a rolling restart of the AGC and
    // proxy Deployments — the value is part of their pod templates — not a hot
    // reload; the new level takes effect once the pods roll.
    //
    // debug surfaces the AGC's per-session/per-job/per-pod lifecycle lines (the
    // listener/multiplexer/provisioner traces with their correlation fields) and
    // the proxy's per-CONNECT detail. At thousands of concurrent sessions those
    // lines dominate log volume, so debug is a deliberate, temporary opt-in for a
    // bug repro. The default is info — never debug — so a CR that omits the field
    // never silently runs at debug verbosity (a verbosity/noise regression).
    // Unlike securityProfile there is no downgrade gate: lowering verbosity is not
    // a security relaxation, so info<->debug transitions are unconstrained.
    //
    // +optional
    // +kubebuilder:default=info
    // +kubebuilder:validation:Enum=info;debug
    LogLevel string `json:"logLevel,omitempty"`

    // Tracing configures opt-in OpenTelemetry distributed tracing for this
    // tenant's AGC. The GMC translates these fields into the standard
    // OpenTelemetry OTEL_* environment variables on the AGC Deployment — the
    // AGC reads only those (cmd/agc/internal/tracing). Tracing stays off
    // unless tracing.endpoint is set, so an ActionsGateway with no tracing
    // block keeps the AGC's no-op tracer provider.
    //
    // There is deliberately no field for OTEL_EXPORTER_OTLP_HEADERS: those
    // can carry bearer tokens, and the project keeps secrets out of
    // environment variables. Authenticate the collector at the network layer
    // (in-cluster collector, mutual TLS, or a service mesh) instead.
    // +optional
    Tracing TracingConfig `json:"tracing,omitempty"`
}

// TracingConfig maps to the AGC's OpenTelemetry OTEL_* environment variables.
type TracingConfig struct {
    // Endpoint is the OTLP/gRPC collector address (e.g.
    // "https://otel-collector.observability:4317"). Setting it enables
    // tracing; an empty Endpoint emits no OTEL_* env. Maps to
    // OTEL_EXPORTER_OTLP_TRACES_ENDPOINT.
    // +optional
    Endpoint string `json:"endpoint,omitempty"`

    // Insecure disables TLS for the OTLP/gRPC connection. Defaults to false
    // (TLS required). Maps to OTEL_EXPORTER_OTLP_TRACES_INSECURE.
    // +optional
    Insecure *bool `json:"insecure,omitempty"`

    // Sampler selects the trace sampler. Maps to OTEL_TRACES_SAMPLER.
    // +optional
    // +kubebuilder:validation:Enum=always_on;always_off;traceidratio;parentbased_always_on;parentbased_always_off;parentbased_traceidratio
    Sampler string `json:"sampler,omitempty"`

    // SamplerArg is the argument for the sampler (e.g. "0.1" for the
    // ratio-based samplers). Maps to OTEL_TRACES_SAMPLER_ARG.
    // +optional
    SamplerArg string `json:"samplerArg,omitempty"`

    // ResourceAttributes are merged onto every AGC span, rendered as a
    // sorted key=value list. Maps to OTEL_RESOURCE_ATTRIBUTES.
    // +optional
    ResourceAttributes map[string]string `json:"resourceAttributes,omitempty"`
}

// ActionsGatewayStatus uses standard Kubernetes Conditions for compatibility
// with kubectl wait, Argo CD health checks, and kstatus.
type ActionsGatewayStatus struct {
    // Conditions contains the current observed conditions of the gateway.
    // Known condition types: Ready, ProxyAvailable, AGCAvailable,
    // CredentialUnavailable, Degraded, ProxyQuotaPressure, ProxyQuotaExceeded,
    // RunnerGroupsDegraded, EgressRulesStale. The type and reason strings are
    // exported as consts from the GMC api package
    // (cmd/gmc/api/v1alpha1/conditions.go).
    Conditions []metav1.Condition `json:"conditions,omitempty"`

    // ProxyReadyReplicas is the number of proxy pods currently Ready.
    // +optional
    ProxyReadyReplicas int32 `json:"proxyReadyReplicas,omitempty"`

    // ActiveSessions is the number of currently open long-poll sessions
    // across all RunnerGroups managed by this gateway's AGC.
    // +optional
    ActiveSessions int32 `json:"activeSessions,omitempty"`

    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// Condition types for ActionsGateway:
//   Ready              — true when both proxy pool and AGC are healthy.
//   ProxyAvailable     — true when proxy pool has >= minReplicas pods Ready.
//   AGCAvailable       — true when the AGC Deployment has >= 1 pod Ready.
//   CredentialUnavailable — abnormal-is-true; the referenced GitHub App Secret is
//                        missing or unusable (Q156). Gates Ready=False.
//   Degraded           — abnormal-is-true (Q156); a reconcile could not provision
//                        the tenant's child resources. The failing step is named
//                        in the message (reason ProvisioningFailed). Set before
//                        the reconcile's early return so it is never stale; cleared
//                        (reason ReconcileSucceeded) on a fully successful
//                        reconcile. Gates Ready=False.
//   ProxyQuotaPressure — advisory WARNING (Q82); true when the proxy pool cannot
//                        scale to maxReplicas within the namespace ResourceQuota
//                        headroom (hard − used). Predictive and load-dependent.
//   ProxyQuotaExceeded — advisory ERROR (Q82); true when proxy replica creates
//                        are being rejected by the namespace ResourceQuota now
//                        (Deployment ReplicaFailure). Supersedes the warning.
//   RunnerGroupsDegraded — advisory (Q158); true when one or more owned
//                        RunnerGroups report an impairing condition (Credential-
//                        Unavailable / Degraded / RunnerVersionTooOld /
//                        WorkersUnschedulable — see
//                        agcv1alpha1.ImpairingConditionTypes). Rolls child health
//                        up to the gateway's single pane; the impaired groups are
//                        named in the message (reason RunnerGroupsImpaired). Does
//                        NOT gate Ready — the gateway infra can be healthy while a
//                        single tenant group is impaired. Exported as the gauge
//                        actions_gateway_runnergroups_degraded.
//   EgressRulesStale   — advisory (Q157); true when the GitHub egress IP-range
//                        allowlist has not been refreshed within the staleness
//                        window (just over two of the ~24h refresh cycles), so a
//                        stalled refresh loop may have let the proxy NetworkPolicy
//                        drift from GitHub's published ranges (reason
//                        RefreshStalled; RefreshCurrent clears it, RefreshPending
//                        before the first refresh or for an unmanaged NP). Does NOT
//                        gate Ready. Only evaluated for managed proxy NetworkPolicy.
//                        Exported as the gauge actions_gateway_egress_rules_stale.
// ProxyQuota{Pressure,Exceeded} are mutually exclusive and do NOT gate Ready —
// the pool keeps serving at its current scale. See the two-tier convention in
// docs/development/kubernetes-conventions.md.

// PriorityTier maps a Kubernetes PriorityClass to a cumulative pod-count
// threshold. The AGC assigns the PriorityClass of the first tier whose
// threshold the current active-pod count has not yet reached.
//
// Thresholds are cumulative across the RunnerGroup, not per-tier slot counts.
// For example, given tiers with thresholds [5, 20, 30]:
//   - pods 1–5   → first tier's PriorityClass  (preempts lower-priority pods)
//   - pods 6–20  → second tier's PriorityClass (opportunistic, no preemption)
//   - pods 21–30 → third tier's PriorityClass  (best-effort, no preemption)
//   - pod 31+    → held; not created until count falls below 30
//
// The last tier's threshold is therefore the effective maxConcurrentJobs ceiling
// for this RunnerGroup. No separate maxConcurrentJobs field is required.
//
// PriorityClass objects are cluster-scoped and must be pre-created by the
// platform team before the RunnerGroup is applied — the GMC does not create
// them, as doing so would require a cluster-level write privilege expansion
// (the same platform-ownership model as the namespace ResourceQuota, Q130).
//
// The platform owns *which* PriorityClasses a tenant may reference (Q132). A
// PriorityClass carries a priority value and a preemptionPolicy (Kubernetes
// default PreemptLowerPriority); an unvalidated tenant-chosen class would let a
// tenant name a high-priority, preempting class and have the scheduler EVICT
// other tenants' running worker pods — breaking cross-tenant isolation. The GMC
// validating webhook therefore rejects any priorityClassName not on the platform
// allowlist (the --allowed-priority-classes flag; an empty allowlist forbids all
// references — secure default). Preemption behaviour is governed entirely by the
// platform-created PriorityClass object: the platform should set its
// preemptionPolicy to Never unless cross-tenant preemption is genuinely intended
// for that tier (PriorityClasses are global, so a PreemptLowerPriority class
// preempts across tenant boundaries). There is deliberately no tenant-settable
// per-tier preemptionPolicy field — it would be a tenant-controlled preemption
// lever the platform must own.
type PriorityTier struct {
    // PriorityClassName is the name of an existing cluster-scoped PriorityClass
    // to assign to worker pods when the active pod count is below Threshold. Must
    // reference a PriorityClass that already exists in the cluster AND appears on
    // the platform allowlist (--allowed-priority-classes); the GMC webhook
    // rejects any other name.
    PriorityClassName string `json:"priorityClassName"`

    // Threshold is the cumulative active-pod count at which this tier is
    // exhausted and the next tier (if any) takes over. Must be > 0 and
    // strictly greater than the previous tier's Threshold.
    // +kubebuilder:validation:Minimum=1
    Threshold int32 `json:"threshold"`
}

// RunnerGroup is a namespace-scoped CRD managed by the AGC.
// Each instance maps to an adaptive pool of listener goroutines backed by
// ephemeral worker pods. The GMC names RunnerGroup CRs as
// "{actionsgateway-name}-{runnergroup.name}".
//
// +kubebuilder:validation:XValidation:rule="!has(self.maxWorkers) || self.priorityTiers.size() == 0 || self.maxWorkers == self.priorityTiers[self.priorityTiers.size()-1].threshold",message="maxWorkers must equal the last priorityTiers threshold when both are set"
type RunnerGroupSpec struct {
    // Name is a stable identifier for this RunnerGroup within the ActionsGateway.
    // The GMC constructs the RunnerGroup CR name as "{actionsgateway-name}-{name}".
    // Must be unique within the ActionsGateway and must not change after creation.
    Name string `json:"name"`

    // MaxListeners is the maximum number of concurrent listener goroutines the AGC
    // will maintain for this RunnerGroup during a burst. The AGC always keeps at
    // least one listener goroutine running; additional goroutines are spawned as
    // jobs arrive (each spawning a replacement before handing off to a worker pod)
    // and shut themselves down once the queue is idle.
    //
    // This field caps burst job-acquisition concurrency, not the number of running
    // worker pods (which is bounded by PriorityTiers and the namespace
    // ResourceQuota). Steady-state rate-limit cost is one session per RunnerGroup
    // regardless of this value; peak cost is at most MaxListeners sessions.
    //
    // Set this to the maximum number of jobs expected to arrive simultaneously for
    // this RunnerGroup. For most RunnerGroups the default is sufficient; increase
    // it only if jobs are being lost during acquisition bursts.
    //
    // +kubebuilder:default=1
    // +kubebuilder:validation:Minimum=1
    MaxListeners int32 `json:"maxListeners,omitempty"`

    // MaxWorkers caps the number of worker pods this RunnerGroup may run
    // concurrently. When set without priorityTiers, the AGC enforces this
    // as a simple pod-count ceiling with no PriorityClass assignment —
    // no cluster-scoped PriorityClass objects are required, making this
    // the self-service option for teams that need a concurrency limit but
    // not scheduling priority control.
    //
    // When set alongside priorityTiers, MaxWorkers must equal the last
    // tier's Threshold (the effective concurrent-pod ceiling already
    // expressed by the tier list). Mismatches are rejected at admission
    // to prevent the two mechanisms from silently disagreeing about which
    // value the AGC enforces.
    //
    // When neither MaxWorkers nor PriorityTiers is set, the only active
    // ceiling is the namespace ResourceQuota — the RunnerGroup can consume
    // all available pod quota.
    //
    // +optional
    // +kubebuilder:validation:Minimum=1
    MaxWorkers *int32 `json:"maxWorkers,omitempty"`

    // RunnerLabels is the label set matched against workflow runs-on values.
    // At least one label is required (MinItems=1): an empty set would silently
    // match every workflow run. Each item must be non-empty, at most 256 chars,
    // and contain no whitespace or commas (comma is the runs-on list separator).
    //
    // +kubebuilder:validation:MinItems=1
    // +kubebuilder:validation:items:MaxLength=256
    // +kubebuilder:validation:items:Pattern=`^[^,\s]+$`
    RunnerLabels []string `json:"runnerLabels"`

    // PriorityTiers defines a list of PriorityClass assignments and their
    // cumulative pod-count thresholds. When a job is acquired, the AGC counts
    // the currently active and pending worker pods for this RunnerGroup and
    // assigns the PriorityClass of the first tier whose threshold has not yet
    // been reached. If the count equals or exceeds the last tier's threshold,
    // the pod is held and not created until capacity falls below that ceiling.
    //
    // This mechanism allows a RunnerGroup to guarantee a minimum number of
    // high-priority (preempting) slots while still permitting additional
    // opportunistic capacity at lower priority — without consuming dedicated
    // reserved resources when those slots are idle.
    //
    // Example — GPU runner with a hard floor of 5 preempting slots, up to 20
    // opportunistic, capped at 30 best-effort:
    //
    //   priorityTiers:
    //   - priorityClassName: runner-critical        # floor
    //     threshold: 5
    //   - priorityClassName: runner-standard        # burst
    //     threshold: 20
    //   - priorityClassName: runner-opportunistic   # best-effort
    //     threshold: 30
    //
    // PriorityClass objects must be pre-created by the platform team AND each
    // referenced name must appear on the GMC --allowed-priority-classes allowlist
    // (Q132); the GMC validating webhook rejects any other name so a tenant
    // cannot name a preempting class and evict other tenants' pods. Preemption
    // behaviour is set on the platform-owned PriorityClass object itself (the
    // platform should use preemptionPolicy: Never unless cross-tenant preemption
    // is intended). Tiers must be listed in strictly ascending threshold order.
    //
    // When PriorityTiers is empty, no PriorityClass is set on worker pods and
    // the namespace ResourceQuota is the only active ceiling.
    // +optional
    // +kubebuilder:validation:MaxItems=10
    PriorityTiers []PriorityTier `json:"priorityTiers,omitempty"`

    // PodTemplate is a standard Kubernetes PodTemplateSpec that controls the
    // ephemeral worker pod created for each acquired job. Tenants may use any
    // pod fields supported by the cluster — init containers, sidecars, volumes,
    // scheduling constraints, etc.
    //
    // The runner container must be named "runner". The AGC injects the runner
    // binary and job payload into this container; if no container named "runner"
    // is present the AGC prepends one using the WorkerImage below.
    //
    // Reserved fields (see WorkerPodTemplate for the full list) are rejected at
    // admission and overwritten by the AGC at pod-creation time.
    //
    // ARC alignment. ARC's AutoscalingRunnerSet exposes the runner container's
    // scheduling and resource knobs through its spec.template (a corev1.PodTemplateSpec).
    // The same surface is available here because PodTemplate embeds a
    // PodTemplateSpec — resources, nodeSelector, tolerations, affinity,
    // topologySpreadConstraints, runtimeClassName, securityContext, volumes,
    // and init/sidecar containers all map one-to-one with no translation. The
    // field is named "podTemplate" rather than ARC's "template" so the
    // underlying Kubernetes type is unambiguous at the spec level; tenants
    // copy-pasting from ARC manifests only need to rename the top-level key.
    PodTemplate  WorkerPodTemplate           `json:"podTemplate"`

    // WorkerImage is the fully-qualified container image for the runner container
    // when PodTemplate does not already define a container named "runner".
    // Production deployments SHOULD reference an immutable digest, e.g.
    // "ghcr.io/my-org/actions-runner-worker@sha256:abc...". Tag-only references
    // (e.g. ":2.334.0") are accepted but discouraged because they undermine the
    // upgrade-rollback semantics described in §2.6. Combine with
    // imagePullPolicy: IfNotPresent (digest pin) or Always (tag).
    //
    // GitHub enforces a minimum runner version at session creation time and
    // returns 400 Bad Request for versions below the threshold. Tenants are
    // responsible for keeping this image current.
    //
    // Omitting this field causes the AGC to use its operator-configured default.
    // The compile-time constant DefaultWorkerImage in
    // cmd/agc/internal/provisioner/provisioner.go supplies the baseline value
    // (currently the digest-pinned "ghcr.io/actions/actions-runner:2.335.1@sha256:…",
    // aligned with the ARC gha-runner-scale-set chart default). Its runner
    // version is the single source of truth in cmd/agc/names (RunnerVersion),
    // which also drives the GITHUB_RUNNER_VERSION the GMC injects so the AGC's
    // agent.version matches the running runner binary. Operators override it via
    // the WORKER_IMAGE environment variable (set by the GMC on the AGC
    // Deployment); tenants can then override further per-RunnerGroup with this
    // field without affecting other groups.
    // +optional
    WorkerImage string `json:"workerImage,omitempty"`

    // MaxEvictionRetries is the maximum number of times the AGC will
    // automatically requeue a job after its worker pod is evicted (preemption
    // or OOM). On each eviction the AGC stops lock renewal — causing GitHub to
    // cancel the run — and then calls the GitHub rerun API to reschedule it.
    //
    // Set to 0 to disable automatic eviction retry entirely (useful for
    // GPU workloads where a failed job must be debugged before rerunning, or
    // for short CI jobs where a re-queue is cheaper to trigger manually).
    //
    // Retries are tracked per run ID and reset when the RunnerGroup is
    // reconciled. Once the budget is exhausted the eviction is logged and
    // the metric actions_gateway_eviction_retries_exhausted_total is
    // incremented but no further rerun attempt is made.
    //
    // +optional
    // +kubebuilder:default=2
    // +kubebuilder:validation:Minimum=0
    // +kubebuilder:validation:Maximum=10
    MaxEvictionRetries *int32 `json:"maxEvictionRetries,omitempty"`

    // EvictionRetryDelay is how long the AGC waits after detecting a pod
    // eviction before calling the GitHub rerun API. A short delay avoids
    // hammering the API on thrashing workloads; the default of 5s is
    // sufficient for most cases.
    //
    // Accepts standard Go duration strings: "5s", "30s", "2m". Values below
    // "1s" are rejected at admission.
    //
    // +optional
    // +kubebuilder:default="5s"
    EvictionRetryDelay *metav1.Duration `json:"evictionRetryDelay,omitempty"`

    // MaxQuotaRetries controls how many times the AGC retries pod creation when
    // the namespace ResourceQuota is exhausted. Unlike eviction retry, the AGC
    // holds the job lock and retries in place — quota typically clears as other
    // jobs complete, so the acquired job is not lost. Once the budget is exhausted
    // the pod creation failure is returned and the job Secret is cleaned up.
    //
    // Set to 0 to disable quota retry entirely. When disabled, a quota-exceeded
    // error fails the provision call immediately without incrementing the
    // exhausted counter (disabled is a policy choice, not a budget failure).
    //
    // +optional
    // +kubebuilder:default=5
    // +kubebuilder:validation:Minimum=0
    // +kubebuilder:validation:Maximum=20
    MaxQuotaRetries *int32 `json:"maxQuotaRetries,omitempty"`

    // QuotaRetryDelay is the time to wait between pod creation retries when the
    // namespace ResourceQuota is exhausted. The default of 30s is chosen to give
    // a running job time to finish and free quota before the next attempt.
    //
    // Accepts standard Go duration strings: "30s", "1m". Values below "1s" are
    // rejected at admission.
    //
    // +optional
    // +kubebuilder:default="30s"
    QuotaRetryDelay *metav1.Duration `json:"quotaRetryDelay,omitempty"`

    // CompletedPodTTL is how long a worker pod that has reached a terminal
    // phase (Succeeded, Failed, or Unknown) is retained before the AGC deletes
    // it. Retention gives operators a window to inspect the pod of a failed
    // job (`kubectl logs`/`describe`) before it disappears; terminal pods
    // consume no compute and no ResourceQuota. Set to "0s" to delete worker
    // pods immediately on completion.
    //
    // Accepts standard Go duration strings: "0s", "5m", "1h". Negative values
    // are rejected at admission. Defaults to "5m" when omitted.
    //
    // +optional
    CompletedPodTTL *metav1.Duration `json:"completedPodTTL,omitempty"`

    // PendingPodDeadline is the maximum time a worker pod may remain Pending
    // (measured from its creation) before the AGC deletes it, releasing the
    // concurrency-ceiling slot the stuck pod was holding. Pending pods get
    // stuck on unpullable images or unschedulable constraints; deleting one
    // resolves its session goroutine and frees the listener for the next job.
    // Each reap emits a WorkerPodStuckPending Warning Event on the
    // RunnerGroup. Raise this on clusters where legitimate scheduling can be
    // slow (e.g. autoscaled GPU node pools).
    //
    // Accepts standard Go duration strings: "10m", "1h". Values below "1s"
    // are rejected at admission. Defaults to "10m" when omitted.
    //
    // +optional
    PendingPodDeadline *metav1.Duration `json:"pendingPodDeadline,omitempty"`
}

// WorkerPodTemplate is a corev1.PodTemplateSpec that defines the pod configuration
// for ephemeral worker pods. Using the standard Kubernetes type gives tenants full
// access to familiar pod fields — init containers, sidecars, volumes, volume mounts,
// security contexts, scheduling constraints, etc. — without requiring them to learn
// a custom schema. IDE completion, kubectl explain, and all standard Kubernetes
// tooling work against this field as normal.
//
// # Controller-enforced invariants
//
// A small set of fields are reserved by the AGC and overwritten unconditionally
// after merging the tenant template. Tenants must not set these fields; attempting
// to do so is rejected at admission by CRD CEL validation rules:
//
//   - spec.serviceAccountName          — always set to the AGC-managed worker SA
//   - spec.automountServiceAccountToken — always set to false
//   - spec.hostPID / spec.hostNetwork / spec.hostIPC — always false; these break the
//     pod isolation model the rest of the design depends on regardless of policy posture
//   - containers[name=runner].env entries for ACTIONS_RUNTIME_TOKEN, HTTP_PROXY,
//     HTTPS_PROXY, NO_PROXY — always injected by the AGC; tenant values are overwritten
//
// All other security constraints — privileged containers, hostPath volumes,
// capabilities, sysctls, allowed registries, etc. — are the responsibility of the
// cluster's admission policy engine (e.g. Kyverno, OPA Gatekeeper). The AGC does
// not duplicate general-purpose policy enforcement; it only guards the invariants
// it depends on for correct operation.
//
// # Merge order
//
// The AGC builds the worker pod by starting from the tenant's PodTemplateSpec and
// then overwriting the reserved fields listed above. Overwrites happen last so that
// no code path allows a tenant value to survive into the final pod spec.
type WorkerPodTemplate = corev1.PodTemplateSpec

type RunnerGroupStatus struct {
    // Conditions contains the current observed conditions of the runner group.
    // Known condition types: Ready, Degraded, RateLimited, RunnerVersionTooOld,
    // CredentialUnavailable, WorkerQuotaPressure, WorkerQuotaExceeded,
    // WorkersUnschedulable. The type and reason strings are exported as consts from
    // the AGC api package (cmd/agc/api/v1alpha1/conditions.go).
    //   CredentialUnavailable — abnormal-is-true (Q156); true when the AGC cannot
    //                         obtain a GitHub App installation token to manage the
    //                         group's agents (reason TokenUnavailable). Set before
    //                         the reconcile's early return so it is never stale, in
    //                         addition to the TokenUnavailable Event; cleared
    //                         (reason CredentialAvailable) once a token is obtained.
    //   WorkerQuotaPressure — advisory WARNING (Q82); true when worker pods
    //                         cannot scale to the configured ceiling (maxWorkers /
    //                         max priorityTier threshold) within the namespace
    //                         ResourceQuota headroom (hard − used).
    //   WorkerQuotaExceeded — advisory ERROR (Q82); true when the namespace
    //                         ResourceQuota cannot admit even one more worker pod
    //                         (the next acquired job's pod will be rejected).
    //                         Supersedes the warning. Distinct from Q59's
    //                         configured-ceiling admission backpressure
    //                         (jobs_admission_rejected_total), which is normal.
    //   WorkersUnschedulable — abnormal-is-true, impairing (Q157); true when worker
    //                         pods sit Pending past the scheduling grace (half
    //                         pendingPodDeadline) because the scheduler cannot place
    //                         them — PodScheduled=False/Unschedulable: no matching
    //                         node, affinity, or untolerated taints. Distinct from
    //                         the WorkerQuota ladder: a quota rejection blocks pod
    //                         admission so no pod exists, so the two never both fire
    //                         for one cause. Rolls up into the gateway's
    //                         RunnerGroupsDegraded; exported as the gauge
    //                         actions_gateway_workers_unschedulable.
    // The listType=map / listMapKey=type markers let server-side apply merge
    // conditions by type instead of treating the slice as atomic.
    //
    // +optional
    // +listType=map
    // +listMapKey=type
    Conditions []metav1.Condition `json:"conditions,omitempty"`

    // ActiveSessions is the number of currently open long-poll sessions
    // managed by this RunnerGroup.
    ActiveSessions int32 `json:"activeSessions"`

    // ActiveJobs is the number of worker pods currently in the Running phase
    // (a job is actively executing). Derived from the worker pod phase count
    // during each reconcile; updated on pod phase-change events. The v2alpha1
    // RunnerSet carries the same field. See also PendingJobs.
    // +optional
    ActiveJobs int32 `json:"activeJobs,omitempty"`

    // PendingJobs is the number of worker pods currently in the Pending phase
    // (a job has been acquired and a pod spawned, but the pod is not yet
    // running). A sustained non-zero count signals scheduling pressure —
    // check WorkersUnschedulable, events, and node/image constraints. Pods
    // that remain Pending past pendingPodDeadline are reaped by the
    // controller. The v2alpha1 RunnerSet carries the same field.
    // +optional
    PendingJobs int32 `json:"pendingJobs,omitempty"`

    ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}
```

---

## 3.2. GitHub App Credentials Secret Schema

The Secret referenced by `gitHubAppRef` must be of `type: Opaque` and contain the following keys:

| Key | Format | Required | Description |
| --- | --- | --- | --- |
| `appId` | Decimal integer string, e.g. `"123456"` | Yes | The GitHub App's numeric ID, visible on the App settings page. |
| `privateKey` | PEM-encoded PKCS#1 RSA private key | Yes | The private key downloaded from the App settings page. Must include the `-----BEGIN RSA PRIVATE KEY-----` header and footer. |
| `installationId` | Decimal integer string, e.g. `"78901234"` | Yes | The installation ID for the App's installation on the target organization or repository. Found in the webhook payload or via the GitHub API (`GET /app/installations`). |

No other keys are read. Unknown keys are ignored.

A minimal example manifest:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-github-app
  namespace: team-a
type: Opaque
stringData:
  appId: "123456"
  installationId: "78901234"
  privateKey: |
    -----BEGIN RSA PRIVATE KEY-----
    MIIEowIBAAKCAQEA...
    -----END RSA PRIVATE KEY-----
```

The AGC uses these three values to generate short-lived GitHub App installation access tokens at session creation time. The `privateKey` is used to sign a JWT asserting the App's identity using the RS256 algorithm; the JWT is then exchanged for an installation token scoped to the `installationId`. Tokens are never written to disk and are refreshed in-memory before expiry.

**Secrets are treated as immutable.** Kubernetes does not deliver Secret updates to running Pods reliably or at a predictable time, making in-place rotation difficult to reason about and hard to test. Instead, credential rotation is performed by creating a new Secret with the updated values and updating `gitHubAppRef.Name` in the `ActionsGateway` CR to reference it. The GMC detects the changed reference during reconciliation and rolls the AGC Deployment to a new Pod mounted with the new Secret. The old Secret can be deleted once the new Pod is healthy. This pattern makes rotation observable (it is a normal Deployment rollout), testable (assert the new Pod references the new Secret by name), and safe to automate.

---

## 3.3. Re-implemented Broker API Endpoints

> **Common pitfall — the two-URL model.** GitHub's broker protocol uses two distinct base URLs and it is easy to conflate them in client code. **`broker_url`** is static for a given runner registration and is used by `POST /sessions` and `GET /message`. **`run_service_url`** is dynamic, extracted from each `GetMessage` response body, and is the base for that job's `POST /acquirejob` and `POST /renewjob` calls. The run service URL differs per job and must not be cached globally — caching it across jobs is the most common cause of mysterious 404s in custom broker clients.

These endpoints are called by each AGC instance. The GMC has no direct relationship with this API.

| HTTP Method | Target Path | Handled By | Purpose |
| --- | --- | --- | --- |
| **POST** | `{broker_url}/sessions` | AGC Goroutine | Registers a virtual runner and obtains a `sessionId`. Rejected with `400 Bad Request` if the runner version in the request body is below GitHub's enforced minimum. |
| **GET** | `{broker_url}/message?sessionId={id}` | AGC Goroutine | Opens a 50-second long-poll connection. Returns `202 Accepted` with empty body when no job is queued; returns a `RunnerJobRequest` message when a job is available. |
| **POST** | `{run_service_url}/acquirejob` | AGC Goroutine | Claims the job within the 2-minute delivery window. Must be called before pod creation. On success returns the full job instructions payload; `planId` is in both the `x-plan-id` response header (primary) and `.plan.planId` in the body (fallback). |
| **POST** | `{run_service_url}/renewjob` | AGC per-job background goroutine | Renews the job lock every 60 seconds. Each renewal extends the lock by ~10 minutes. Must run continuously from after `acquirejob` until the job completes or is cancelled — failure to renew causes GitHub to cancel the job. |
| **POST** | `{broker_url}/acknowledge` | AGC Goroutine | Post-dispatch telemetry notification to the broker (`AcknowledgeRunnerRequestAsync` in the official runner source). Confirmed in Milestone 1 (Investigation A) as **not required for correct job delivery** — `acquirejob` alone is the atomic claim. The v2 broker host does not expose the v1 VSTS delete-message endpoint; the correct v2 path is `POST {brokerURL}acknowledge?sessionId={sessionId}` with body `{"runnerRequestId": "…"}`. Callers MAY skip this call; it has no effect on job delivery semantics. |

**Retry policy for `GET /message`:** Based on `MessageListener.cs` in the official runner source, the AGC session goroutine should implement a two-tier random backoff on errors: up to 5 consecutive errors use [15s, 30s] jitter; beyond 5 errors the window widens to [30s, 60s]. After 50 consecutive empty-body (202) responses within 30 minutes, apply the same [15s, 30s] backoff as a server-anomaly guard. **Non-retriable errors** (surface as a `RunnerGroup` status Condition, do not retry in a tight loop): session not found, pool not found, unauthorized, access denied. **Special case:** a session-expired error should trigger session recreation before resuming the poll loop.

**Session reuse after `acquirejob`.** Confirmed in Milestone 1 (Investigation C): a goroutine may call `GET /message` again on the same `sessionId` immediately after a successful `acquirejob` — the session remains live and returns `202` without error. The AGC does not need a delete→create cycle between jobs.

**One active session per registered runner agent.** `POST /sessions` returns `409 Conflict` if the supplied `agentId` already has an active session (confirmed in Milestone 1, Investigation D). The AGC must assign a distinct pre-registered agent to each concurrent listener goroutine. Agent provisioning (runner registration) is a RunnerGroup setup concern, not a per-session concern — see [§2.2](02-architecture.md#22-tier-2--actions-gateway-controller-agc) for the agent pool model.

---

## 3.4. Broker Payload Blueprints (Go Structs)

The AGC uses the following request and response shapes. The `GetMessage` response body contains the `run_service_url` and `runner_request_id` needed for the subsequent `acquirejob` and `renewjob` calls — these values must be extracted and used per-job, not cached globally.

```go
// TaskAgentMessage is the response body from GET {broker_url}/message.
type TaskAgentMessage struct {
    MessageID   int64  `json:"messageId"`
    MessageType string `json:"messageType"` // "RunnerJobRequest" when a job is available
    Body        string `json:"body"`        // JSON string containing RunnerJobRequestBody
}

// RunnerJobRequestBody is the parsed content of TaskAgentMessage.Body.
type RunnerJobRequestBody struct {
    RunnerRequestID string `json:"runner_request_id"` // used as jobMessageId in AcquireJob
    RunServiceURL   string `json:"run_service_url"`   // base URL for acquirejob and renewjob
    BillingOwnerID  string `json:"billing_owner_id"`
}

// JobAcquisitionRequest is the request body for POST {run_service_url}/acquirejob.
type JobAcquisitionRequest struct {
    JobMessageID   string `json:"jobMessageId"`  // = RunnerJobRequestBody.RunnerRequestID
    RunnerOS       string `json:"runnerOS"`      // e.g. "Linux"
    BillingOwnerID string `json:"billingOwnerId"`
}

// AcquireJobResponse is the response from POST {run_service_url}/acquirejob.
// The full body contains all job instructions forwarded opaquely to the Runner.Worker.
// The AGC only extracts planId for lock renewal; everything else is passed through.
// planId is returned in two places: prefer the x-plan-id response header; fall back
// to .plan.planId in the body if the header is absent.
type AcquireJobResponse struct {
    Plan struct {
        PlanID string `json:"planId"`
    } `json:"plan"`
    // Remainder of the body is the complete job instructions payload forwarded to the worker.
}

// RenewJobRequest is the request body for POST {run_service_url}/renewjob.
// Must be called every 60 seconds after acquirejob succeeds.
type RenewJobRequest struct {
    // PlanID comes from the acquirejob response. Prefer the x-plan-id response header;
    // fall back to AcquireJobResponse.Plan.PlanID if the header is absent.
    PlanID string `json:"planId"`
    JobID  string `json:"jobId"` // = RunnerJobRequestBody.RunnerRequestID
}

// RenewJobResponse is returned by POST {run_service_url}/renewjob.
type RenewJobResponse struct {
    LockedUntil time.Time `json:"lockedUntil"` // typically ~10 minutes from now
}
```

---

## 3.5. GitHub API Rate Limit Budget

Each GitHub App installation receives **15,000 requests per hour** against the broker and run service endpoints combined. The AGC's per-session and per-job request mix produces a predictable steady-state load that operators should size against this budget.

**Per-session steady-state cost** (one idle long-polling goroutine, no active job):

* `GET /message` — 50s long-poll, returns 202 on empty. At maximum density an idle session issues ~72 requests/hour against the broker.

**Per-active-job steady-state cost** (one goroutine with a running job):

* `POST /renewjob` — every 60s for the duration of the job, so ~60 requests/hour while the job runs.
* One-shot calls (`POST /sessions` once per session create, `POST /acquirejob` once per job) are negligible against the hourly budget. `POST /acknowledge` is confirmed optional (Investigation A) and is not counted.

**Steady-state ceiling.** A reasonable safe target is **≤ 250 concurrent sessions per installation**, leaving headroom for bursts:

```
  250 sessions × 72 message-polls/hr   = 18,000  -- already exceeds 15K alone
```

In practice the empty-message poll budget dominates everything else. Tenants who need to operate at higher session counts MUST shard across multiple GitHub App installations (one installation per `ActionsGateway` CR, multiple CRs in separate namespaces). Multi-installation per single AGC is explicitly out of scope for v1.

**429 handling.** On a `429 Too Many Requests` response, the AGC honors the `Retry-After` header (or falls back to exponential backoff capped at 5 minutes), increments `actions_gateway_message_poll_errors_total{reason="rate_limited"}`, and surfaces a `RateLimited` condition on the affected `RunnerGroup` so operators see the saturation in `kubectl describe runnergroup` without scraping logs. Sustained rate-limited state (more than 10 minutes) should page on-call.

**Capacity planning corollary.** The 250-session ceiling combines with the per-AGC memory budget ([Appendix A](appendix-a-capacity-slos.md)) to determine when to add a second `RunnerGroup` (same installation, more goroutines within budget) vs. a second `ActionsGateway` CR (separate installation, separate budget).

---

← [Core Architecture](02-architecture.md) | [Back to index](README.md) | Next: [Operational Flows →](04-operational-flows.md)
