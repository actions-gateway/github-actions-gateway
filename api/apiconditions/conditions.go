// Package apiconditions holds the v2 condition/reason vocabulary — the strings the
// GMC and AGC reconcilers write into every v2 kind's .status.conditions[].
//
// It lives in the neutral api module, outside any versioned package, because the
// vocabulary is version-neutral by contract: a condition type or reason is a runtime
// status value, not schema, and the v2 storage/hub conversion is only sound if both
// served versions name the same states identically (§H.7). Declaring it once removes
// the drift class rather than detecting it — the version packages (api/v2alpha1,
// api/v2beta1) re-export these names so every existing `v2alpha1.ConditionReady`
// call site keeps compiling, but the values and the rationale live here alone (Q374).
//
// Adding a condition or reason: declare it here, then add the one-line re-export to
// BOTH version packages' conditions.go. scripts/go/check-v2-api-sync.sh fails on a
// one-sided re-export.
package apiconditions

// Condition types and reasons reported on v2 kinds' .status.conditions. These are
// the canonical, exported source of truth — never duplicate them as inline literals.
// The v2 status/condition contract is uniform across all five kinds
// (docs/design/appendix-h-v2-api-decomposition.md §H.7): every kind carries a
// listType=map Conditions slice keyed on type, an ObservedGeneration, and a Ready
// condition with normal-is-True polarity and a shared reason vocabulary; messages
// name the specific blocker (e.g. the missing template or Secret), never a generic
// string.
//
// The reconcilers that set these conditions land in later milestones (M2 data kinds
// + EgressProxy, M3a control kinds); the contract is pinned here so no reconciler
// invents its own.
const (
	// ConditionReady is True when every component of the object is available — for an
	// ActionsGateway, the AGC control plane; for an EgressProxy, the proxy pool; for a
	// RunnerSet, that its references resolved and at least one listener is running. The
	// data kinds (RunnerTemplate/ClusterRunnerTemplate) are pure data with no reconciler
	// and report no conditions (§H.7's contract fields are present for uniformity only).
	ConditionReady = "Ready"
	// ConditionAGCAvailable is True when the tenant's AGC Deployment is ready.
	ConditionAGCAvailable = "AGCAvailable"
	// ConditionCredentialUnavailable is True when the referenced GitHub App
	// credential Secret is missing or unusable (abnormal-is-True).
	ConditionCredentialUnavailable = "CredentialUnavailable" //nolint:gosec // G101: a condition type name, not a credential
	// ConditionDegraded is True when a reconcile could not provision or update the
	// object's children; the failing step is named in the message (abnormal-is-True).
	// On a classic-protocol RunnerSet it is also pushed by the shared listener
	// goroutines when session creation is rejected as unauthorized
	// (ReasonSessionUnauthorized) — see the classic-listener vocabulary block below.
	ConditionDegraded = "Degraded"
	// ConditionEgressUnattributed is an advisory condition (abnormal-is-True) set True
	// on a proxy-less object: its egress reaches GitHub directly, so it has no
	// per-tenant egress IP identity. It does NOT gate Ready — direct egress is a
	// supported, NetworkPolicy-restricted mode (§H.10); the condition surfaces the
	// attribution trade-off an operator opted into by not attaching an EgressProxy, so
	// "no proxy" is an auditable state rather than an inferred one.
	ConditionEgressUnattributed = "EgressUnattributed"
	// ConditionAGCAutoscalingUnavailable is an advisory condition (abnormal-is-True) set
	// True on an ActionsGateway that opted into spec.agcAutoscaling in a cluster where the
	// autoscaling.k8s.io VerticalPodAutoscaler CRD is not installed (Q360). It does NOT
	// gate Ready: the AGC is provisioned and fully functional with its agcResources
	// sizing, it just is not being right-sized. The condition (plus a Warning Event) is
	// what keeps an unsatisfiable opt-in from failing silently, in place of the
	// alternatives — wedging the gateway, or hot-looping on an apply that cannot succeed.
	ConditionAGCAutoscalingUnavailable = "AGCAutoscalingUnavailable"
	// ConditionScaleSetNameCollision is an advisory condition (abnormal-is-True) set
	// True on an ActionsGateway when a ScaleSet RunnerSet bound to it claims a
	// scale-set name — its first runnerLabel — that another ScaleSet RunnerSet already
	// claims in the same GitHub scope (Q849). Both AGCs then drive one scale set at
	// GitHub and each acquires the other tenant's jobs (§5.2). Admission rejects a
	// write that would create the pair (Q791), so this reports the pair admission never
	// saw: one that predates the guard, or was applied while the webhook was not
	// installed. It does NOT gate Ready, and provisioning is unaffected — GAG cannot
	// pick which tenant loses the name, and refusing to run the AGC would take down
	// both tenants rather than the one that is misconfigured. The message names a
	// conflicting set only when it sits in this gateway's own namespace; a cross-tenant
	// holder goes to the GMC log, the same non-enumeration rule the admission error
	// follows.
	ConditionScaleSetNameCollision = "ScaleSetNameCollision"
	// ConditionPossibleReapBlockingSidecar is an advisory condition (abnormal-is-True)
	// set True on a RunnerSet whose resolved worker template carries a regular
	// (non-native) sidecar container that may keep the worker pod alive after the
	// runner container exits, so the pod never reaps and the runner slot counts against
	// maxWorkers (Q249, the Q247 stranding class). It does NOT gate Ready — the check is
	// a heuristic and native sidecars are the fix, not enforcement. The message names
	// the offending containers; the SelfExitingSidecarsAnnotation opt-out clears it.
	ConditionPossibleReapBlockingSidecar = "PossibleReapBlockingSidecar"
	// ConditionWorkerQuotaPressure (warning) and ConditionWorkerQuotaExceeded (error)
	// are the two-tier namespace-ResourceQuota worker-capacity ladder (Q82, ported from
	// the v1 RunnerGroup to the v2 RunnerSet in Q303). Both are advisory (abnormal-is-True)
	// and never gate Ready; they are mutually exclusive (exceeded supersedes pressure):
	//   - WorkerQuotaExceeded is True when the namespace ResourceQuota cannot admit even
	//     one more worker pod — the next acquired job's pod will be rejected at admission.
	//   - WorkerQuotaPressure is True when the pool cannot grow from its current worker
	//     count up to the configured ceiling (maxWorkers / max priorityTier threshold)
	//     within current quota headroom.
	// They surface the silent stall where a RunnerSet's pendingJobs rise while Ready
	// stays True because the namespace quota is tighter than the set's ceiling.
	ConditionWorkerQuotaPressure = "WorkerQuotaPressure"
	ConditionWorkerQuotaExceeded = "WorkerQuotaExceeded"
	// ConditionWorkersUnschedulable is True when one or more of the RunnerSet's worker
	// pods have sat Pending past a scheduling grace because the scheduler could not
	// place them — the pod reports PodScheduled=False/Unschedulable (no node matches its
	// resource requests, nodeSelector, affinity, or tolerations) (Q157, ported to the v2
	// RunnerSet in Q303, abnormal-is-True). It is distinct from the WorkerQuota ladder: a
	// ResourceQuota rejection blocks pod *admission* so no pod is ever created, so an
	// unschedulable Pending pod can only reflect a scheduler verdict, never quota
	// exhaustion — the two never double-report. It does NOT gate Ready, but it is the
	// signal that a set's pendingJobs are climbing because capacity is not materializing.
	ConditionWorkersUnschedulable = "WorkersUnschedulable"
	// ConditionWorkerCapacityDeclined is True when the RunnerSet's capacity gate is
	// currently refusing to take on jobs because the cluster cannot place this set's
	// worker pods (Q405, abnormal-is-True). It is present ONLY on a set that opted in
	// via spec.capacityGate.mode; the default (Off) carries no such condition at all,
	// so its presence is itself the "this set has a capacity gate" signal.
	//
	// It is deliberately a SEPARATE condition from ConditionWorkersUnschedulable even
	// in the SchedulerVerdict mode, where the two derive from the same underlying fact,
	// for three reasons: it means something different to an operator ("intake is being
	// refused" versus "pods are stuck"), it stays stable across the later gate modes
	// while the signal underneath it changes (Q406/Q407), and WorkersUnschedulable is
	// already an ImpairingConditionTypes rollup input — overloading it would tangle the
	// gateway-level RunnerSetsDegraded summary with an intake decision.
	//
	// For that last reason it is NOT itself in ImpairingConditionTypes: rolling up both
	// would double-count one fact into the GMC's summary (Q304).
	ConditionWorkerCapacityDeclined = "WorkerCapacityDeclined"
)

// Egress proxy mode reported in status.proxyMode (§H.10). It makes "no proxy" an
// explicit, auditable state instead of an absent field to be inferred. Dropping the
// proxy drops egress *identity* (per-tenant IP attribution), never egress
// *restriction*: Direct egress is still default-deny egress allowing only DNS +
// GitHub CIDRs (+ the kube API server for the AGC control plane).
const (
	// ProxyModeProxied means egress flows through a resolved EgressProxy, giving the
	// workload stable per-tenant egress IPs (attribution).
	ProxyModeProxied = "Proxied"
	// ProxyModeDirect means no proxy resolved, so egress reaches GitHub directly —
	// still NetworkPolicy-restricted to GitHub CIDRs + DNS (+ kube API for the AGC),
	// but without per-tenant egress IP identity.
	ProxyModeDirect = "Direct"
)

// Template-resolution source reported in RunnerSet status.templateSource (Q172, §H.4):
// which rung of the optional-templateRef fallback chain supplied the worker pod shape.
// It makes "where did this set's template come from" an auditable status value rather
// than something an operator has to re-derive from the gateway and cluster state. It
// mirrors the proxyMode precedent: an explicit field for a fallback an unset reference
// resolves through.
const (
	// TemplateSourceRef means the RunnerSet's own spec.templateRef resolved the
	// template — the explicit, unchanged-from-required path.
	TemplateSourceRef = "TemplateRef"
	// TemplateSourceGatewayDefault means templateRef was unset and the gateway's
	// spec.defaultTemplateRef resolved the template.
	TemplateSourceGatewayDefault = "GatewayDefault"
	// TemplateSourceClusterDefault means neither templateRef nor the gateway's
	// defaultTemplateRef was set and the single cluster-default ClusterRunnerTemplate
	// (IsDefaultTemplateAnnotation) resolved the template.
	TemplateSourceClusterDefault = "ClusterDefault"
)

// Condition reasons. Reasons are CamelCase per Kubernetes API conventions;
// contextual detail (the failing step, the missing Secret name) goes in the message.
const (
	// ReasonReady is the Ready=True reason.
	ReasonReady = "Ready"
	// ReasonAGCReady is the AGCAvailable=True reason.
	ReasonAGCReady = "AGCReady"
	// ReasonAGCNotReady is the AGCAvailable=False (and Ready=False) reason while the
	// tenant's AGC Deployment has no ready replica yet.
	ReasonAGCNotReady = "AGCNotReady"
	// ReasonProxyReady is the EgressProxy Ready=True reason.
	ReasonProxyReady = "ProxyReady"
	// ReasonProxyNotReady is the EgressProxy Ready=False reason while the proxy
	// pool is provisioned but has fewer than minReplicas ready pods.
	ReasonProxyNotReady = "ProxyNotReady"
	// ReasonSecretNotFound is the CredentialUnavailable=True reason (possession model:
	// the referenced GitHub App Secret is absent). Workload-identity gateways
	// (delegation model, Q201) hold no Secret, so they never report this reason — they
	// provision directly from their projected Vault-auth identity.
	ReasonSecretNotFound = "SecretNotFound"
	// ReasonProvisioningFailed is the Degraded=True reason; the failing step is named
	// in the message. ReasonReconcileSucceeded clears it.
	ReasonProvisioningFailed = "ProvisioningFailed"
	ReasonReconcileSucceeded = "ReconcileSucceeded"
)

// Ready=False reasons for RunnerSet reference resolution (§H.7). Resolution is a
// runtime concern, not an admission gate, so a set pointing at a not-yet-applied
// referent reports one of these (fail-closed: no worker pods until it resolves)
// and flips to Ready the moment the referent syncs.
const (
	// ReasonGatewayNotFound — the referenced ActionsGateway does not exist.
	ReasonGatewayNotFound = "GatewayNotFound"
	// ReasonGatewayTerminating — the referenced ActionsGateway carries a deletion
	// timestamp. The AGC stops acquiring and reaps the set's worker pods before the
	// GMC tears its own Deployment down, because it is the only reaper those pods
	// have (Q547). Distinct from GatewayNotFound, which is the post-teardown resting
	// state and reaps nothing.
	ReasonGatewayTerminating = "GatewayTerminating"
	// ReasonTemplateNotFound — no template resolved: the referenced
	// RunnerTemplate/ClusterRunnerTemplate does not exist, or (when templateRef and
	// gateway.defaultTemplateRef are both unset) no cluster-default ClusterRunnerTemplate
	// is marked. Fail-closed: no worker pod is ever synthesized without a pod shape (§H.4).
	ReasonTemplateNotFound = "TemplateNotFound"
	// ReasonAmbiguousDefault — the RunnerSet fell through to the cluster-default rung of
	// the template-resolution chain (Q172) but more than one ClusterRunnerTemplate is
	// marked the cluster default (IsDefaultTemplateAnnotation). Fail-closed: rather than
	// silently picking one, the set sits Ready=False until exactly one default remains.
	// At-most-one is enforced here, at runtime, not at admission (cross-object, GitOps-
	// safe; §H.7) — see docs/design/appendix-h-v2-api-decomposition.md §H.4.
	ReasonAmbiguousDefault = "AmbiguousDefault"
	// ReasonTemplateDeleted — a previously-resolved template was deleted (degrade-not-block,
	// §H.8): the RunnerSet's own status shows a prior successful resolution
	// (status.templateSource) under an unchanged spec generation, yet the template no
	// longer resolves. Distinct from TemplateNotFound (never applied) so the operator
	// sees the referent vanished out from under a working set.
	ReasonTemplateDeleted = "TemplateDeleted"
	// ReasonProxyNotFound — the referenced EgressProxy does not exist.
	ReasonProxyNotFound = "ProxyNotFound"
	// ReasonProxyDeleted — a previously-resolved proxy was deleted (degrade-not-block,
	// §H.8): the set previously reported proxyMode Proxied under an unchanged spec
	// generation, yet the proxy no longer resolves. Distinct from ProxyNotFound (never
	// applied).
	ReasonProxyDeleted = "ProxyDeleted"
	// ReasonProxyShareNotGranted — a proxyRef/defaultProxyRef names an EgressProxy in
	// another namespace that does not consent to the referrer's namespace (§H.9).
	// Consent is provider-side: the proxy must list the referrer in
	// spec.sharing.allowedNamespaces. Distinct from ProxyNotFound so the operator sees
	// a proxy that exists but is not shared, rather than hunting a missing object.
	ReasonProxyShareNotGranted = "ProxyShareNotGranted"
	// ReasonCABundleNotFound — the ConfigMap named by githubCABundleRef does not exist,
	// so the AGC cannot be provisioned with the trust it was asked to use (Degraded,
	// fail closed: an appliance behind a private CA is unreachable without it).
	ReasonCABundleNotFound = "CABundleNotFound"
	// ReasonCABundleInvalid — the githubCABundleRef ConfigMap exists but carries no
	// ca.crt key, or that key holds no parseable certificate. Distinct from
	// CABundleNotFound so the operator sees a content problem, not a missing object.
	ReasonCABundleInvalid = "CABundleInvalid"
	// ReasonNoActiveSessions — a RunnerSet's references all resolved but no
	// listener goroutine is running yet (Ready=False until one comes up).
	ReasonNoActiveSessions = "NoActiveSessions"
	// ReasonRunnerGroupNotFound — spec.runnerGroup (or the gateway's
	// defaultRunnerGroup) names a GitHub runner group the installation does not have,
	// so the scale set cannot be registered where the operator asked (Ready=False).
	// Fail-closed on purpose: the runner group is GitHub's authorization point for
	// which repositories may target these runners, and the alternative — registering
	// into the default group — silently widens that boundary to the whole
	// installation (Q712).
	ReasonRunnerGroupNotFound = "RunnerGroupNotFound"
	// ReasonListenerActive — a RunnerSet's references resolved and at least one
	// listener goroutine is running (Ready=True).
	ReasonListenerActive = "ListenerActive"
	// ReasonTokenUnavailable — the AGC could not obtain a GitHub App installation
	// token, so the RunnerSet cannot register runners (Ready=False).
	ReasonTokenUnavailable = "TokenUnavailable"
	// ReasonDirectEgress is the EgressUnattributed=True reason (and the proxyMode=Direct
	// rationale): no proxyRef/defaultProxyRef resolved, so egress is direct and
	// unattributed (still NetworkPolicy-restricted to GitHub).
	ReasonDirectEgress = "DirectEgress"
	// ReasonProxiedEgress is the EgressUnattributed=False reason: a proxy resolved, so
	// egress is attributed to the proxy's stable per-tenant IPs.
	ReasonProxiedEgress = "ProxiedEgress"
	// ReasonVPACRDNotInstalled is the AGCAutoscalingUnavailable=True reason: the gateway
	// opted into spec.agcAutoscaling but the cluster has no autoscaling.k8s.io
	// VerticalPodAutoscaler CRD, so the managed autoscaler could not be created (Q360).
	ReasonVPACRDNotInstalled = "VPACRDNotInstalled"
	// ReasonAGCAutoscalingActive is the AGCAutoscalingUnavailable=False reason when the
	// gateway opted in and the managed VerticalPodAutoscaler is stamped.
	ReasonAGCAutoscalingActive = "AGCAutoscalingActive"
	// ReasonAGCAutoscalingDisabled is the AGCAutoscalingUnavailable=False reason when the
	// gateway did not opt in: spec.agcResources alone sizes the AGC, which is the default.
	ReasonAGCAutoscalingDisabled = "AGCAutoscalingDisabled"
	// ReasonScaleSetNameShared is the ScaleSetNameCollision=True reason: a ScaleSet
	// RunnerSet bound to this gateway shares its first runnerLabel — the scale-set name
	// at GitHub — with another ScaleSet RunnerSet in the same GitHub scope (Q849).
	ReasonScaleSetNameShared = "ScaleSetNameShared"
	// ReasonScaleSetNamesUnique is the ScaleSetNameCollision=False reason: every
	// ScaleSet RunnerSet bound to this gateway holds its scale-set name alone.
	ReasonScaleSetNamesUnique = "ScaleSetNamesUnique"
	// ReasonReapBlockingSidecar is the PossibleReapBlockingSidecar=True reason: the
	// resolved template has one or more regular, unacknowledged sidecar containers.
	ReasonReapBlockingSidecar = "ReapBlockingSidecar"
	// ReasonNoReapBlockingSidecar is the PossibleReapBlockingSidecar=False reason: the
	// resolved template has no reap-blocking sidecar (or every one is acknowledged
	// self-exiting, or converted to a native sidecar).
	ReasonNoReapBlockingSidecar = "NoReapBlockingSidecar"
	// ReasonPodsUnschedulable is the WorkersUnschedulable=True reason (Q303); the stuck
	// pods and the scheduler's verdict are named in the condition message.
	// ReasonWorkersSchedulable clears it (WorkersUnschedulable=False). The two-tier
	// WorkerQuota ladder's reasons are contextual (they name the exhausted quota and
	// resource) and are set inline by the reconciler, not fixed here.
	ReasonPodsUnschedulable  = "PodsUnschedulable"
	ReasonWorkersSchedulable = "WorkersSchedulable"
	// ReasonCapacityAvailable is the WorkerCapacityDeclined=False reason (Q405): the
	// gate is engaged and is not refusing intake. The True reasons name the SIGNAL the
	// gate read, so the operator can tell which rung stopped their jobs —
	// ReasonPodsUnschedulable (the scheduler's verdict, reusing the reason the sibling
	// WorkersUnschedulable condition already publishes), ReasonScaleUpDeclined (the
	// autoscaler's own declination) and ReasonPodsNotStarting (the kubelet's, on a pod
	// that bound and never started). Q407 adds CapacityUnavailable; it is not declared
	// until its mode ships.
	ReasonCapacityAvailable = "CapacityAvailable"
	// ReasonScaleUpDeclined is the WorkerCapacityDeclined=True reason for mode
	// AutoscalerVerdict (Q406): the cluster autoscaler itself recorded, on a worker pod
	// stuck past the scheduling grace, that it will not add a node for that pod. The
	// autoscaler's own per-node-group text is carried into the condition message, which
	// is what makes this condition actionable rather than merely true — it names the
	// taint, the quota, or the node-group ceiling that stopped the scale-up.
	//
	// Distinct from ReasonPodsUnschedulable on purpose: both mean "intake is gated",
	// but they answer to different evidence and only one of them is sound on an elastic
	// cluster, so an operator reading the reason learns which assertion their set is
	// resting on.
	ReasonScaleUpDeclined = "ScaleUpDeclined"
	// ReasonPodsNotStarting is the WorkerCapacityDeclined=True reason for the kubelet's
	// startup verdict (Q714): a worker pod BOUND to a node and then failed to start,
	// with the kubelet in backoff on the container image. The pod's own backoff message
	// is carried into the condition message, which names the image that will not pull.
	//
	// It is the sibling of ReasonPodsUnschedulable, not a widening of it. The two read
	// opposite halves of PodScheduled: that one is the scheduler saying no node can host
	// the pod, this one is a pod already hosted whose container never ran. Folding them
	// into one reason would make an operator's remedy ambiguous — a node/taint/quota fix
	// for one, an image or registry fix for the other.
	//
	// Unlike both other True reasons this one is evaluated on EVERY cluster, not selected
	// by clusterCapacity.nodeAutoscaling. The asymmetry the mode split exists for is
	// whether another actor is waiting on the pod to make capacity appear
	// (docs/design/appendix-d-alternatives-considered.md §D.8); a bound pod is already
	// placed, so no autoscaler is waiting on it and no new node changes the pull.
	ReasonPodsNotStarting = "PodsNotStarting"
	// ReasonAwaitingProbe is the WorkerCapacityDeclined=True reason for the latched
	// state (Q512): every stuck worker pod that produced the declined verdict has been
	// reaped, and nothing has yet shown that capacity returned, so the decline is
	// retained rather than cleared. Intake is not closed — it is limited to one probe
	// job per pendingPodDeadline window; the pod that job produces is the evidence
	// that resolves the latch (it STARTS and the condition clears, or it sticks and the
	// live reason returns).
	//
	// Starting, rather than merely scheduling, is what resolves it (Q714). Binding was
	// only ever a proxy for "a worker can run here", and ReasonPodsNotStarting is the
	// case that falsifies the proxy: a probe pod binds instantly and reveals itself
	// seconds later, so releasing the latch on the bind would restore the full
	// advertisement in the gap — the very no-op this latch was built to remove. For the
	// two placeability reasons the stronger evidence costs the seconds between a healthy
	// probe binding and its first container running.
	//
	// The latch exists because the gate's evidence is the stuck pod itself, and the
	// reaper deletes that pod: without it, clearing restored the scale-set tier's full
	// advertisement every deadline window, and a measured burst of N wasted claims
	// stayed N under the gate (Q512).
	ReasonAwaitingProbe = "AwaitingProbe"
	// ReasonGateModeUnsupported is a WorkerCapacityDeclined=False reason (Q406): the set
	// selected a capacity-gate mode this AGC does not implement, so no rung is evaluated
	// and intake is exactly today's behavior.
	//
	// It exists because the CRDs ship as their own chart and can be upgraded ahead of
	// the AGC: an operator who selects a mode a newer CRD accepts, against an AGC that
	// predates it, must get the fail-open direction. Treating an unrecognized mode as
	// "some gate, near enough" would silently apply the wrong signal's semantics — the
	// one failure this rung's whole design is ordered around, since SchedulerVerdict's
	// semantics on an elastic cluster starve a tenant.
	ReasonGateModeUnsupported = "GateModeUnsupported"
	// Ready=False reasons for classic-path runtime provisioning failures (Q308). Unlike
	// the reference-resolution reasons above (a missing referent), these are transient
	// failures *after* resolution: the set holds Ready=False with the failing step named
	// in the message and flips back to Ready on the next successful reconcile.
	//
	// ReasonAgentProvisioningFailed — agentpool.EnsureAgents could not provision the
	// listener agents' Secrets (e.g. the registration API rejected the agent). No
	// listener goroutine can run until the Secrets exist, so the set is Ready=False
	// (fail-closed) rather than silently reporting healthy until the next reconcile.
	ReasonAgentProvisioningFailed = "AgentProvisioningFailed"
	// ReasonListenerStartFailed — the classic multiplexer could not (re)start its
	// listener goroutines, so no session is polling for work. Distinct from
	// NoActiveSessions (the benign not-started-yet state): a start error, not merely an
	// as-yet-empty pool.
	ReasonListenerStartFailed = "ListenerStartFailed"
)

// RunnerSetsDegraded rollup (Q304). The GMC's ActionsGateway reconciler rolls the
// health of the RunnerSets bound to a gateway (spec.gatewayRef) up onto the gateway's
// status, mirroring v1's RunnerGroupsDegraded (the operator's single pane — see
// docs/development/kubernetes-conventions.md and the v1 agcv1alpha1.ImpairingConditionTypes
// aggregation). Kept in its own const block so it stays additive alongside the sibling
// condition-vocabulary work on this file.
const (
	// ConditionRunnerSetsDegraded is True when one or more RunnerSets bound to the
	// ActionsGateway are impaired — not serving jobs (abnormal-is-True). Advisory: like
	// v1's RunnerGroupsDegraded it does NOT gate Ready, because the gateway's own AGC
	// control plane can be healthy while a tenant's RunnerSet is impaired. The impaired
	// sets and their tripped signals are named in the condition message so an operator
	// can act from the gateway without inspecting each child.
	ConditionRunnerSetsDegraded = "RunnerSetsDegraded"
	// ReasonRunnerSetsImpaired is the RunnerSetsDegraded=True reason (one or more bound
	// RunnerSets are impaired; the message names them). ReasonAllRunnerSetsHealthy clears
	// it (RunnerSetsDegraded=False), including when the gateway has no bound RunnerSets —
	// absence of evidence is not an alarm.
	ReasonRunnerSetsImpaired   = "RunnerSetsImpaired"
	ReasonAllRunnerSetsHealthy = "AllRunnerSetsHealthy"
)

// Proxy-pool quota and egress-allowlist-staleness conditions (Q320). The GMC's
// EgressProxy reconciler sets these on an EgressProxy, porting v1's ActionsGateway
// ProxyQuota (Q82) and EgressRulesStale (Q157) conditions onto the v2 standalone
// proxy pool: the pool is a namespace-ResourceQuota-bounded, HPA-scaled Deployment
// whose default CIDR-mode NetworkPolicy is refreshed from the shared GitHub IP-range
// cache, so the same capacity and staleness signals apply. All three are advisory
// (abnormal-is-True) and do NOT gate the proxy's Ready — the pool keeps serving at
// its current scale. Kept in their own const block so they stay additive alongside
// sibling condition-vocabulary work on this file.
const (
	// ConditionProxyQuotaPressure (warning) is True when the proxy pool cannot grow to
	// maxReplicas within the namespace ResourceQuota headroom (predictive). It is
	// mutually exclusive with ConditionProxyQuotaExceeded — the error supersedes it.
	ConditionProxyQuotaPressure = "ProxyQuotaPressure"
	// ConditionProxyQuotaExceeded (error) is True when proxy replica creation is being
	// rejected by the namespace ResourceQuota now (observed, from the Deployment's
	// ReplicaFailure condition).
	ConditionProxyQuotaExceeded = "ProxyQuotaExceeded"
	// ConditionEgressRulesStale is True when the shared GitHub IP-range refresh loop's
	// last success is older than the staleness window, so a CIDR-mode proxy's egress
	// NetworkPolicy allowlist may have drifted from GitHub's published ranges. False in
	// the no-evidence cases: an unmanaged NetworkPolicy, a non-CIDR (FQDN) egress mode,
	// or before the first refresh completes.
	ConditionEgressRulesStale = "EgressRulesStale"

	// ConditionGitHubEgressIncomplete is True when a CIDR-mode proxy pool's egress
	// allowlist provably cannot reach the GitHub a referrer binds to it: a referring
	// ActionsGateway names a GitHub Enterprise Server host, and CIDR mode allows only
	// the ranges api.github.com/meta publishes, which never contain a customer
	// appliance (Q506 #3). The appliance's address space is knowable only to the
	// operator, so the GMC cannot close this gap — it names it. Supplying
	// spec.destinationCIDRs clears the condition; whether those ranges actually cover
	// the appliance is not verifiable here, so the operator's declaration is taken at
	// face value. Advisory: it does not gate Ready, because the pool is serving
	// exactly the policy it was asked for.
	ConditionGitHubEgressIncomplete = "GitHubEgressIncomplete"

	// ReasonRefreshStalled is the EgressRulesStale=True reason (the last successful
	// GitHub IP-range refresh is older than the staleness window). ReasonRefreshCurrent
	// clears it (EgressRulesStale=False after a fresh refresh); ReasonRefreshPending is
	// the False reason in the no-evidence cases (unmanaged/FQDN/first-refresh-pending).
	ReasonRefreshStalled = "RefreshStalled"
	ReasonRefreshCurrent = "RefreshCurrent"
	ReasonRefreshPending = "RefreshPending"

	// ReasonApplianceRangesRequired is the GitHubEgressIncomplete=True reason: a GHES
	// referrer is bound and no destinationCIDRs were supplied. ReasonGitHubEgressAllowed
	// is the False reason — every referrer is on public GitHub, the operator supplied
	// ranges, the mode is not CIDR, or the policy is operator-maintained.
	ReasonApplianceRangesRequired = "ApplianceRangesRequired"
	ReasonGitHubEgressAllowed     = "GitHubEgressAllowed"
)

// Listener session-failure condition vocabulary (Q309, Q325). Both acquisition
// tiers push these conditions onto the RunnerSet that owns the session: the
// classic listener goroutines (cmd/agc/internal/listener, shared with the v1
// RunnerGroup — referencing the agcv1alpha1 constants of the same values, pinned
// by a value-parity test in cmd/agc) and the ScaleSet listener
// (cmd/agc/internal/scalesetlistener, Q325). All are advisory (abnormal-is-True)
// and do not gate Ready. The classic listener sets only the abnormal (True) states;
// the ScaleSet listener also publishes the healthy (False) states and clears an
// abnormal state when the session recovers.
//
// RunnerVersionTooOld has two producers with different reasons (Q715). GitHub's own
// rejection at session creation (VersionTooOld) is classic-only — the scale-set
// protocol carries no runner version there, since the per-job JIT config is minted
// server-side — so on its own it leaves the ScaleSet tier unable to report the
// failure class at all. Both reconcilers therefore also publish the condition from
// the worker image itself every reconcile (the WorkerImage* reasons below), which
// needs no session and so covers both tiers.
const (
	// ConditionRateLimited is True when GitHub has been rate-limiting the set's
	// sessions for a sustained period (abnormal-is-True).
	ConditionRateLimited = "RateLimited"
	// ConditionRunnerVersionTooOld is True when the runner version cannot serve
	// jobs: GitHub rejected it at session creation, or the worker image ships one
	// below GitHub's enforced minimum (abnormal-is-True).
	ConditionRunnerVersionTooOld = "RunnerVersionTooOld"

	// ReasonSustainedRateLimit is the RateLimited=True reason (message polling has
	// been answered 429 for over ten minutes).
	ReasonSustainedRateLimit = "SustainedRateLimit"
	// ReasonVersionTooOld is the RunnerVersionTooOld=True reason when GitHub itself
	// rejected the session as too old (classic tier only).
	ReasonVersionTooOld = "VersionTooOld"
	// ReasonVersionAccepted is the RunnerVersionTooOld=False reason the classic
	// listener publishes as its healthy baseline: GitHub accepted agent.version at
	// session creation. It clears a ReasonVersionTooOld left by an earlier instance,
	// which no restart clears on its own — the version is the AGC's own compile-time
	// pin, so the fix is a gateway upgrade and the condition outlives it in status
	// (Q795). It never overwrites the reconciler's image reading: only the clear is
	// arbitrated, and a session-sourced True still writes over an image verdict.
	ReasonVersionAccepted = "VersionAccepted"
	// ReasonWorkerImageBelowMinimum is the RunnerVersionTooOld=True reason when the
	// worker image's tag declares an actions/runner version below the enforced
	// registration minimum (agc/names.MinRunnerVersion).
	ReasonWorkerImageBelowMinimum = "WorkerImageBelowMinimum"
	// ReasonWorkerImageCurrent is the RunnerVersionTooOld=False reason: the worker
	// image declares a runner version at or above the enforced minimum.
	ReasonWorkerImageCurrent = "WorkerImageCurrent"
	// ReasonWorkerImageVersionUnknown is the RunnerVersionTooOld=Unknown reason: the
	// worker image reference declares no runner version (digest-only, or a tag of
	// the tenant's own), so nothing has been checked. Unknown rather than False —
	// a custom image is where a stale runner hides.
	ReasonWorkerImageVersionUnknown = "WorkerImageVersionUnknown"
	// ReasonSessionUnauthorized is the Degraded=True reason pushed when session
	// creation is rejected as unauthorized — the agent credentials are invalid or
	// revoked.
	ReasonSessionUnauthorized = "Unauthorized"
	// ReasonPollingHealthy is the RateLimited=False reason on the ScaleSet path:
	// message polling is healthy — either never rate-limited (published when the
	// listener starts) or recovered from a sustained-429 episode.
	ReasonPollingHealthy = "PollingHealthy"
	// ReasonSessionAuthorized is the Degraded=False reason on the ScaleSet path:
	// the session calls are authorized — either never rejected (published when the
	// listener starts) or recovered after the credentials were fixed.
	ReasonSessionAuthorized = "SessionAuthorized"
)

// Job-provisioning stall condition (Q551), ScaleSet-only. A job GitHub assigned to
// the scale set cannot be turned into a worker right now — either a stale registration
// holds the runner name it needs and neither deregistering it (Q334) nor a bounded run
// of fresh suffixed names (Q270) clears it, or the owner is already at its declared
// worker ceiling (Q576). The assignment is acked past so it cannot wedge the queue
// cursor, and the queue never re-offers it — so the listener keeps re-offering it
// itself, and reports the stall here meanwhile.
//
// Advisory (abnormal-is-True) and not in ImpairingConditionTypes: every other job the
// set is assigned still provisions, so rolling it up as "this RunnerSet cannot serve
// jobs" would overstate it. The stalled job ids are named in the message, and the
// actions_gateway_scaleset_jobs_deferred gauge carries the same signal for alerting.
const (
	// ConditionJobProvisionStalled is True while one or more assigned jobs cannot be
	// provisioned and are being re-offered on a backoff.
	ConditionJobProvisionStalled = "JobProvisionStalled"

	// ReasonRunnerNameConflict is the JobProvisionStalled=True reason: the runner
	// name at least one stalled job needs is held by a registration that has not
	// cleared. It outranks ReasonWorkerCeilingReached whenever both classes are held
	// at once, because it is the one an operator can act on — a full ceiling clears
	// itself as workers finish.
	ReasonRunnerNameConflict = "RunnerNameConflict"
	// ReasonWorkerCeilingReached is the JobProvisionStalled=True reason when every
	// stalled job is waiting only on worker capacity: the owner is at the ceiling its
	// spec declares (maxWorkers, or the last priorityTiers threshold), so the jobs are
	// re-offered until a running worker finishes. Expected backpressure on a saturated
	// set, not a fault.
	ReasonWorkerCeilingReached = "WorkerCeilingReached"
	// ReasonJobsProvisioning is the JobProvisionStalled=False reason: no assigned job
	// is waiting on a runner name or on capacity — either none ever was (published
	// when the listener starts) or the last stalled job provisioned or completed.
	ReasonJobsProvisioning = "JobsProvisioning"
)

// ImpairingConditionTypes returns the abnormal-is-True RunnerSet condition types
// that, when True, mean the set cannot serve jobs: a listener that could register no
// session (ConditionDegraded from revoked/invalid credentials, ConditionRunnerVersionTooOld
// from a too-old runner — both pushed by the shared listener goroutines), a missing
// credential (ConditionCredentialUnavailable), or worker pods the scheduler cannot place
// (ConditionWorkersUnschedulable). The GMC's ActionsGateway RunnerSetsDegraded rollup
// (Q304) rolls a bound set up as impaired when any of these is True — in addition to a
// non-benign Ready=False (Q330), which is how v2's reference-resolution and runtime
// provisioning failures surface. Iterating this set rather than hard-coding the list
// means extending it here automatically widens the rollup; it is the v2 counterpart of
// v1's agcv1alpha1.ImpairingConditionTypes.
//
// The advisory/transient conditions are deliberately excluded — ConditionRateLimited (a
// throughput signal with its own gauge that recovers on its own), the two-tier WorkerQuota
// ladder, ConditionEgressUnattributed, and ConditionPossibleReapBlockingSidecar — because
// they are trade-off/throughput signals, not "the set is broken", and rolling them up would
// flap the summary on normal operation.
func ImpairingConditionTypes() []string {
	return []string{
		ConditionDegraded,
		ConditionCredentialUnavailable,
		ConditionRunnerVersionTooOld,
		ConditionWorkersUnschedulable,
	}
}

// Worker sizing-drift condition (Q359 Phase 2). The AGC's RunnerSet reconciler
// compares the resolved worker template's per-container resource ask against the
// measured status.sizingRecommendation and surfaces a material mismatch. Advisory
// (abnormal-is-True): it never gates Ready and nothing is applied automatically —
// the operator (or a future opt-in sizing profile) acts on it. Kept in its own
// const block so it stays additive alongside sibling condition-vocabulary work.
const (
	// ConditionSizingDrift is True when at least one container's template ask
	// deviates materially from the measured recommendation in either direction:
	// waste (a request at least twice the recommendation, holding node capacity
	// jobs never use) or OOM risk (a memory limit below the highest observed
	// per-job peak). The condition message names each offending container and
	// mismatch. Only evaluated once enough jobs have been sampled.
	ConditionSizingDrift = "SizingDrift"

	// ReasonSizingDriftDetected is the SizingDrift=True reason; the message names
	// the drifting containers and directions.
	ReasonSizingDriftDetected = "SizingDriftDetected"
	// ReasonSizingWithinRange is the SizingDrift=False reason once enough jobs
	// have been sampled and every container's ask is within the drift thresholds.
	ReasonSizingWithinRange = "SizingWithinRange"
	// ReasonInsufficientSamples is the SizingDrift=False reason while too few
	// jobs have been sampled to judge drift with confidence.
	ReasonInsufficientSamples = "InsufficientSamples"
	// ReasonSizingProfileActive is the SizingDrift=False reason while an opt-in
	// sizing profile (Q359 Phase 3) is actively applying the measured
	// recommendation at pod build: the template ask is no longer what worker
	// pods run with, so judging drift against it would mislead.
	ReasonSizingProfileActive = "SizingProfileActive"
)

// Sizing-profile override condition (Q489). The Throughput profile's mechanism is
// the ABSENCE of a runner-container CPU limit — jobs burst into idle node capacity.
// Anything that puts that limit back at admission cancels the profile without
// rejecting anything: the pod is admitted, status.sizingProfileState still reports
// Active, and every other signal looks correct while bursting is gone. A namespace
// LimitRange with a Container-type cpu default is the common cause; a mutating
// admission webhook or policy engine does it just as silently.
//
// So the AGC reports the EFFECT rather than any one cause: it compares the worker
// pods it built without a CPU limit (marked provisioner.AnnotationSizingProfile)
// against what the apiserver admitted. Advisory (abnormal-is-True): jobs still run,
// they just do not burst, so it never gates Ready and is not in
// ImpairingConditionTypes.
const (
	// ConditionSizingProfileOverridden is True when a worker pod the Throughput
	// profile built without a CPU limit is running with one — whatever injected
	// it. The message names the pod, the container, and the observed limit.
	// Reported only under Throughput, and removed under any other profile.
	ConditionSizingProfileOverridden = "SizingProfileOverridden"

	// ReasonCPULimitInjected is the SizingProfileOverridden=True reason: admission
	// added a CPU limit to a container the profile deliberately built without one.
	ReasonCPULimitInjected = "CPULimitInjected"
	// ReasonNoCPULimitInjected is the SizingProfileOverridden=False reason: every
	// profile-built worker pod observed reached the kubelet as built, CPU limit
	// still absent, so jobs burst as the profile intends.
	ReasonNoCPULimitInjected = "NoCPULimitInjected"
	// ReasonAwaitingWorkerPods is the SizingProfileOverridden=False reason when the
	// profile has built no worker pod yet, so there is nothing to observe. It is
	// deliberately distinct from NoCPULimitInjected: "we looked and it was clean"
	// and "there was nothing to look at" are different claims, and reporting the
	// second as the first would assert a health it has not established.
	ReasonAwaitingWorkerPods = "AwaitingWorkerPods"
)

// Runner-label registration condition (Q726). A ScaleSet runner set registers every
// spec.runnerLabels entry on its scale set at GitHub, the first entry naming it. The
// registration can come back short without erroring: a GitHub Enterprise Server
// appliance below 3.21 keeps only the name label unless a site admin enables
// DistributedTask.AllowRunnerScaleSetCustomLabels, and discards the rest silently;
// so does a scale set created before a label was appended, because the AGC reuses an
// existing scale set by name rather than rewriting its labels.
//
// Both leave a set whose jobs on the missing labels queue at GitHub forever with
// every other signal healthy, so the AGC compares what the server returned against
// what the set declares and reports the shortfall. Advisory (abnormal-is-True): the
// set still serves every job targeting the labels that did register, so it never
// gates Ready and is not in ImpairingConditionTypes — a label an operator has to add
// is a configuration mismatch, not an outage to roll up into the gateway's
// RunnerSetsDegraded summary.
const (
	// ConditionRunnerLabelsIncomplete is True when the scale set at GitHub does not
	// carry every label the runner set declares. The message names the missing
	// labels and the scale set they are missing from.
	ConditionRunnerLabelsIncomplete = "RunnerLabelsIncomplete"

	// ReasonLabelsNotRegistered is the RunnerLabelsIncomplete=True reason: the
	// server's label set is missing at least one declared label.
	ReasonLabelsNotRegistered = "LabelsNotRegistered"
	// ReasonLabelsRegistered is the RunnerLabelsIncomplete=False reason: every
	// declared label is present on the scale set.
	ReasonLabelsRegistered = "LabelsRegistered"
)
