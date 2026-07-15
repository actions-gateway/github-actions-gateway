package v2alpha1

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
	// applied). A ProxyShareNotGranted reason for the cross-namespace consent handshake
	// (§H.9) arrives with cross-namespace sharing (M4) — not declared until then.
	ReasonProxyDeleted = "ProxyDeleted"
	// ReasonNoActiveSessions — a RunnerSet's references all resolved but no
	// listener goroutine is running yet (Ready=False until one comes up).
	ReasonNoActiveSessions = "NoActiveSessions"
	// ReasonListenerActive — a RunnerSet's references resolved and at least one
	// listener goroutine is running (Ready=True).
	ReasonListenerActive = "ListenerActive"
	// ReasonTokenUnavailable — the AGC could not obtain a GitHub App installation
	// token, so the RunnerSet cannot register runners (Ready=False).
	ReasonTokenUnavailable = "TokenUnavailable" //nolint:gosec // G101: a condition reason, not a credential
	// ReasonDirectEgress is the EgressUnattributed=True reason (and the proxyMode=Direct
	// rationale): no proxyRef/defaultProxyRef resolved, so egress is direct and
	// unattributed (still NetworkPolicy-restricted to GitHub).
	ReasonDirectEgress = "DirectEgress"
	// ReasonProxiedEgress is the EgressUnattributed=False reason: a proxy resolved, so
	// egress is attributed to the proxy's stable per-tenant IPs.
	ReasonProxiedEgress = "ProxiedEgress"
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

	// ReasonRefreshStalled is the EgressRulesStale=True reason (the last successful
	// GitHub IP-range refresh is older than the staleness window). ReasonRefreshCurrent
	// clears it (EgressRulesStale=False after a fresh refresh); ReasonRefreshPending is
	// the False reason in the no-evidence cases (unmanaged/FQDN/first-refresh-pending).
	ReasonRefreshStalled = "RefreshStalled"
	ReasonRefreshCurrent = "RefreshCurrent"
	ReasonRefreshPending = "RefreshPending"
)

// Classic-listener condition vocabulary (Q309). The classic acquisition machinery
// is shared between the v1 RunnerGroup and the v2 RunnerSet, and its listener
// goroutines (cmd/agc/internal/listener) push these session-failure conditions
// onto whichever kind owns the session — referencing the agcv1alpha1 constants of
// the same values. Declared here so the v2 vocabulary is complete; a value-parity
// test in cmd/agc pins the packages together. All are advisory (abnormal-is-True)
// and do not gate Ready. The ScaleSet acquisition path does not surface these
// failure classes yet (Q325).
const (
	// ConditionRateLimited is True when GitHub has been rate-limiting the set's
	// sessions for a sustained period (abnormal-is-True).
	ConditionRateLimited = "RateLimited"
	// ConditionRunnerVersionTooOld is True when GitHub rejects the configured
	// runner version as too old for session creation (abnormal-is-True).
	ConditionRunnerVersionTooOld = "RunnerVersionTooOld"

	// ReasonSustainedRateLimit is the RateLimited=True reason (message polling has
	// been answered 429 for over ten minutes).
	ReasonSustainedRateLimit = "SustainedRateLimit"
	// ReasonVersionTooOld is the RunnerVersionTooOld=True reason.
	ReasonVersionTooOld = "VersionTooOld"
	// ReasonSessionUnauthorized is the Degraded=True reason pushed when session
	// creation is rejected as unauthorized — the agent credentials are invalid or
	// revoked.
	ReasonSessionUnauthorized = "Unauthorized"
)
