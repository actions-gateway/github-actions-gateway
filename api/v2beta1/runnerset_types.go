package v2beta1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RunnerSetSpec is the desired state of a RunnerSet — the small scheduling/quota
// binder that replaces v1alpha1's RunnerGroup. The large PodTemplateSpec no longer
// lives here: it moves to a referenced RunnerTemplate / ClusterRunnerTemplate so a
// runner set stays a fixed-size object and a template is reused across many sets
// (docs/design/appendix-h-v2-api-decomposition.md §H.5). The scheduling and
// lifecycle fields are carried over from RunnerGroup unchanged in meaning.
//
// +kubebuilder:validation:XValidation:rule="!has(self.maxWorkers) || !has(self.priorityTiers) || self.priorityTiers.size() == 0 || self.maxWorkers == self.priorityTiers[self.priorityTiers.size()-1].threshold",message="maxWorkers must equal the last priorityTiers threshold when both are set"
// +kubebuilder:validation:XValidation:rule="!has(self.evictionRetryDelay) || duration(self.evictionRetryDelay) >= duration('1s')",message="evictionRetryDelay must be at least 1s"
// +kubebuilder:validation:XValidation:rule="!has(self.quotaRetryDelay) || duration(self.quotaRetryDelay) >= duration('1s')",message="quotaRetryDelay must be at least 1s"
// +kubebuilder:validation:XValidation:rule="!has(self.completedPodTTL) || duration(self.completedPodTTL) >= duration('0s')",message="completedPodTTL must not be negative"
// +kubebuilder:validation:XValidation:rule="!has(self.pendingPodDeadline) || duration(self.pendingPodDeadline) >= duration('1s')",message="pendingPodDeadline must be at least 1s"
// +kubebuilder:validation:XValidation:rule="!has(self.maxWorkerLifetime) || duration(self.maxWorkerLifetime) >= duration('0s')",message="maxWorkerLifetime must not be negative"
type RunnerSetSpec struct {
	// GatewayRef names the ActionsGateway that supplies this runner set's GitHub
	// binding and control plane. Under multi-gateway-per-namespace each AGC
	// reconciles only the RunnerSets whose gatewayRef targets it — which is why
	// spec.gatewayRef.name is a CRD selectable field (KEP-4358), so that scoping
	// runs server-side (§H.7). Required: a runner set with no gateway has no GitHub
	// connection to register against. Resolved at runtime, not admission.
	GatewayRef ObjectRef `json:"gatewayRef"`

	// TemplateRef optionally names the RunnerTemplate (default) or ClusterRunnerTemplate
	// (set kind: ClusterRunnerTemplate) that supplies the worker pod shape. Unset means
	// inherit the gateway's defaultTemplateRef; both unset means the single cluster-default
	// ClusterRunnerTemplate (the one marked IsDefaultTemplateAnnotation). If none of the
	// three resolves the set fails closed Ready=False/TemplateNotFound — the AGC never
	// synthesizes a phantom worker pod without a pod shape (Q172, §H.4). This relaxes the
	// GA-era required templateRef to optional-with-a-default (a backward-compatible
	// required→optional change): a set that sets templateRef behaves exactly as before.
	// The referent is resolved at runtime; a set pointing at a not-yet-applied template
	// sits Ready=False/TemplateNotFound until it syncs (§H.7). status.templateSource
	// reports which rung resolved.
	//
	// +optional
	TemplateRef *ObjectRef `json:"templateRef,omitempty"`

	// ProxyRef optionally names the EgressProxy this runner set's traffic egresses
	// through. Unset means inherit the gateway's defaultProxyRef; both unset means
	// direct egress (still NetworkPolicy-restricted to DNS + GitHub) — a well-defined
	// behavior, so the dependency is simply droppable, which is why proxyRef is
	// optional where templateRef is required (§H.4, §H.10). Direct egress is reflected
	// in status as proxyMode=Direct plus an advisory EgressUnattributed condition; a
	// proxyRef/defaultProxyRef that names a *missing* proxy still fails closed
	// (ProxyNotFound), not direct egress (Q168).
	//
	// +optional
	ProxyRef *ProxyObjectRef `json:"proxyRef,omitempty"`

	// MaxWorkers caps the number of worker pods this RunnerSet may run concurrently.
	// A soft, in-process ceiling; pair it with a namespace ResourceQuota for a hard,
	// cluster-enforced limit.
	//
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxWorkers *int32 `json:"maxWorkers,omitempty"`

	// RunnerLabels is the label set matched against workflow runs-on values. Every
	// label is registered on the scale set at GitHub, so a workflow may target the set
	// with a single name (runs-on: linux) or with an array (runs-on: [linux, gpu]);
	// the Actions Service matches a scale set's labels the way it matches a plain
	// self-hosted runner's. Each label must be non-empty and contain no whitespace or
	// commas (comma is the runs-on list separator).
	//
	// The FIRST label is the scale set's name at GitHub, which makes it this set's
	// identity: reordering the list renames the scale set, leaving the old one
	// orphaned at GitHub, so treat runnerLabels[0] as stable and append rather than
	// prepend. It is also the label the GMC's admission webhook holds unique across
	// the runner sets under one gateway; later labels may overlap freely, and which
	// set an ambiguous runs-on reaches is GitHub's decision.
	//
	// A GitHub Enterprise Server appliance below 3.21 accepts only the name label
	// unless a site admin enables DistributedTask.AllowRunnerScaleSetCustomLabels,
	// and it discards the rest without an error. The AGC compares the registered
	// label set against this one and reports any shortfall as the advisory
	// RunnerLabelsIncomplete condition (Q726).
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:items:Pattern=`^[^,\s]+$`
	RunnerLabels []string `json:"runnerLabels"`

	// RunnerGroup names the GitHub runner group this set's scale set is registered
	// into. This is GitHub's own grouping, not the deprecated v1alpha1 RunnerGroup
	// CR. Unset means inherit the gateway's defaultRunnerGroup; both unset means
	// GitHub's default group.
	//
	// The runner group is the GitHub-side authorization point for which repositories
	// may target these runners. A scale set left in the default group is reachable
	// by every repository that group admits, typically the whole organization, so a
	// repository outside the tenant can name this set in runs-on and route work into
	// its namespace, quota, and egress IP. Pod-level isolation is unaffected; what
	// the group bounds is who can cause a job to run here.
	//
	// Resolved at runtime against GitHub, not at admission (§H.7), and fail-closed:
	// a name no runner group matches leaves the set Ready=False/
	// RunnerGroupNotFound rather than falling back to the default group, and a
	// scale set already registered in a different group is moved into this one.
	// The group's own repository access is configured at GitHub and is not managed
	// by GAG. See docs/operations/tenant-onboarding.md.
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	RunnerGroup string `json:"runnerGroup,omitempty"`

	// PriorityTiers defines PriorityClass assignments and cumulative pod-count
	// thresholds. Tiers must be in strictly ascending threshold order.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=10
	PriorityTiers []PriorityTier `json:"priorityTiers,omitempty"`

	// MaxEvictionRetries controls how many times the AGC automatically re-queues a
	// job whose worker pod was evicted. Set to 0 to disable auto-retry entirely.
	// Defaults to 2 when omitted.
	//
	// Honored on both acquisition tiers as of Q417. On ScaleSet — the only tier this
	// API version offers — the worker is provisioned fire-and-forget, so the eviction
	// is detected by the owning reconciler from the worker pod rather than by a
	// goroutine watching it; the budget itself is shared, keyed by workflow run, so
	// this cap applies per run across both tiers rather than once each.
	//
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	MaxEvictionRetries *int32 `json:"maxEvictionRetries,omitempty"`

	// EvictionRetryDelay is the minimum time to wait before re-queuing an evicted
	// job. Must be at least 1s. Defaults to "5s" when omitted.
	//
	// Honored on both acquisition tiers as of Q417, as for MaxEvictionRetries.
	//
	// +optional
	EvictionRetryDelay *metav1.Duration `json:"evictionRetryDelay,omitempty"`

	// MaxQuotaRetries controls how many times the AGC retries pod creation when the
	// namespace ResourceQuota is exhausted. Set to 0 to disable quota retry.
	// Defaults to 5 when omitted.
	//
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=20
	MaxQuotaRetries *int32 `json:"maxQuotaRetries,omitempty"`

	// QuotaRetryDelay is the time to wait between pod creation retries when the
	// namespace ResourceQuota is exhausted. Must be at least 1s. Defaults to "30s"
	// when omitted.
	//
	// +optional
	QuotaRetryDelay *metav1.Duration `json:"quotaRetryDelay,omitempty"`

	// CompletedPodTTL is how long a worker pod that has reached a terminal phase is
	// retained before the AGC deletes it. Set to "0s" to delete immediately on
	// completion. Must not be negative. Defaults to "5m" when omitted.
	//
	// +optional
	CompletedPodTTL *metav1.Duration `json:"completedPodTTL,omitempty"`

	// PendingPodDeadline is the maximum time a worker pod may remain Pending before
	// the AGC deletes it, releasing the concurrency-ceiling slot. Must be at least
	// 1s. Defaults to "10m" when omitted.
	//
	// +optional
	PendingPodDeadline *metav1.Duration `json:"pendingPodDeadline,omitempty"`

	// MaxWorkerLifetime is the maximum time a worker pod may be active on its node
	// before the kubelet kills it, applied as the pod's activeDeadlineSeconds. It
	// bounds a worker whose job ended while the AGC was down — a pod nothing the AGC
	// observes later can distinguish from one running a long job (Q438). The kubelet
	// enforces it, so it holds even while the AGC is unavailable.
	//
	// A pod killed this way lands in Failed with reason DeadlineExceeded and is
	// reaped under reason "lifetime_exceeded", with a Warning Event
	// (WorkerPodLifetimeExceeded). Jobs declaring a `timeout-minutes` above this will
	// be killed mid-run. Set to "0s" to disable. An activeDeadlineSeconds set
	// explicitly on the RunnerTemplate's podTemplate takes precedence. Must not be
	// negative. Defaults to "12h" when omitted.
	//
	// +optional
	MaxWorkerLifetime *metav1.Duration `json:"maxWorkerLifetime,omitempty"`

	// ScaleUp optionally caps the RATE at which the AGC creates new worker pods for
	// this RunnerSet, smoothing cold-start stampedes on a shared, rate-sensitive
	// egress path (NAT/SNAT gateway, stateful-firewall conntrack table, site-to-site
	// VPN) when a burst of jobs is acquired at once. It is a token bucket over pod
	// CREATION and is distinct from maxWorkers (a ceiling on the concurrent pod
	// COUNT): the ceiling bounds how many run, this bounds how fast they start.
	// Omitted ⇒ no rate limit (the default): immediate provisioning, zero added
	// latency. An availability knob for the narrow stampede case; prefer a
	// peer-to-peer image mirror for image-pull storms and workflow-level
	// `concurrency:` or the maxWorkers ceiling for fairness/sustained load.
	//
	// +optional
	ScaleUp *ScaleUpRateLimit `json:"scaleUp,omitempty"`

	// Sizing opts this runner set into measured worker sizing at pod-build time
	// (Q359 Phase 3): the AGC derives the worker containers' CPU/memory
	// requests/limits from the observed per-job usage history
	// (status.sizingRecommendation) — or, for the NodeShare profile, from a
	// declared per-node share — instead of the template's static values. Omitted
	// (or profile Static) keeps today's behavior: the template is authoritative.
	// Extended resources (GPUs) are never modified by any profile: they are
	// job-selected shape identity, and only the cpu/memory keys are ever derived.
	//
	// +optional
	Sizing *WorkerSizing `json:"sizing,omitempty"`

	// CapacityGate opts this runner set into the placeability rung of the admission
	// ladder (Q405): the AGC refuses to take on jobs whose worker pod the cluster
	// cannot currently place, instead of claiming them and stalling. Omitted (or
	// mode Off) keeps today's behavior exactly — no capacity rung.
	//
	// +optional
	CapacityGate *CapacityGate `json:"capacityGate,omitempty"`
}

// Capacity-gate modes selectable via CapacityGate.Mode (Q405, Q406, Q470). The enum
// answers ONE question — how hard should this set try not to claim work it cannot
// run — and deliberately says nothing about which signal answers it.
//
// That split is the correction Q470 made. "Can this pod be placed" has different
// sound answers depending on whether a cluster autoscaler is waiting on the
// unplaceable pod to make capacity appear
// (docs/design/appendix-d-alternatives-considered.md §D.8), and the first shape of
// this enum encoded that asymmetry as two modes — SchedulerVerdict and
// AutoscalerVerdict. But which of those is sound is a property of the CLUSTER,
// identical for every set in it and known to whoever owns the nodes, so it now lives
// on ActionsGateway.spec.clusterCapacity.nodeAutoscaling and the AGC picks the signal
// from it. A tenant declares intent here; the platform declares the cluster there.
//
// EVERY VALUE EXCEPT Off REFUSES JOBS. They differ in how the AGC learns the cluster
// cannot place the pod, never in whether it acts on the answer: there is no
// report-only mode here, and Off is the single value that does nothing. That is why
// the values are named for the method rather than as on/off (Q476) — Observe reads
// evidence a stuck pod has already produced, where the reserved values below solicit
// an answer instead, and a bare "On" would stop distinguishing them the moment the
// axis grew. Do not read Observe as the audit/dry-run tier some enforcement APIs
// spell that way; it gates.
//
// Reserved but NOT YET IMPLEMENTED, and therefore not accepted by the enum:
// Probe/Provision (ask before claiming via a ProvisioningRequest capacity check —
// Q407), which extend this same axis by soliciting an answer rather than observing
// one. They are rejected at admission rather than accepted as no-ops: an operator who
// selects a gate expects gating, and silently doing nothing is the failure mode this
// rung exists to remove.
const (
	// CapacityGateModeOff is the default: no capacity rung, today's behavior.
	CapacityGateModeOff = "Off"
	// CapacityGateModeObserve refuses to take on jobs while the cluster cannot place
	// this set's worker pods, deciding from evidence a stuck worker pod has ALREADY
	// produced rather than by asking — whichever signal is sound for the cluster:
	//
	//   - nodeAutoscaling: Absent — the scheduler's own verdict, i.e. worker pods sat
	//     Pending past the scheduling grace reporting PodScheduled=False/Unschedulable.
	//     Nothing is waiting on those pods, so the verdict is final and they are pure
	//     waste.
	//   - nodeAutoscaling: Present (the default) — the cluster autoscaler's OWN
	//     declination, recorded as an Event on a stuck worker pod. Where a node may
	//     still arrive, the Pending pod is a REQUEST that may yet be granted, so only
	//     the autoscaler saying it will not act proves nothing is coming.
	//
	// Recognition of a declination is deliberately narrow, and the asymmetry is the
	// safety argument: a missed one costs nothing (the gate stays open, which is
	// today's behavior) while a wrongly-read one starves a tenant. So on a cluster
	// whose autoscaler the AGC does not recognize — a proprietary optimizer — this
	// mode simply never gates, and a later positive signal from a recognized one
	// reopens it.
	CapacityGateModeObserve = "Observe"
)

// CapacityGate configures the placeability rung of the admission ladder (Q405).
//
// Without it, a runner set whose worker shape has become unplaceable — a drained
// GPU pool, a changed taint, spot capacity gone — keeps claiming jobs, and each
// claim spends a single-use JIT runner record, holds a GitHub job lock until
// pendingPodDeadline, and ends in a reaped pod plus a CANCELLED workflow run. The
// gate does not eliminate the first wasted claim (the signal is derived from a
// stuck pod, so one has to exist); it bounds the RATE, turning a burst of N wasted
// claims into roughly one per pendingPodDeadline window.
//
// That derivation is also what makes the gate self-clearing: the reaper deletes the
// stuck pod at the deadline, the condition clears, one job is claimed, and if
// capacity is still absent the new pod trips it again.
//
// Fail-open by contract at every step — an unreadable set, an unresolved template
// chain, an unreadable pod list all leave intake exactly as it is today. The gate
// may under-gate freely; it must never over-gate, because over-gating starves a
// tenant.
type CapacityGate struct {
	// Mode selects how hard this set tries not to claim work it cannot run, by naming
	// how the AGC learns the cluster cannot place a worker; see the CapacityGateMode*
	// constants. Off is the default and is today's behavior. Observe gates on evidence
	// an already-stuck pod produced — it is not a report-only tier.
	//
	// It does NOT select the signal. Which signal is sound depends on whether the
	// cluster can grow, which is stated once by the platform operator on
	// ActionsGateway.spec.clusterCapacity.nodeAutoscaling — so a tenant enabling the
	// gate cannot pick a signal that is wrong for the cluster they are running in.
	//
	// +kubebuilder:default=Off
	// +kubebuilder:validation:Enum=Off;Observe
	// +optional
	Mode string `json:"mode,omitempty"`
}

// Worker sizing profiles selectable via WorkerSizing.Profile (Q359 Phase 3). The
// history-based profiles (Binpack, Throughput) apply per-container values derived
// from status.sizingRecommendation and fall back to Static — whole-pod, so QoS
// stays predictable — until EVERY template container has a confident
// recommendation (>= the drift-confidence sample minimum); the effective state is
// reported in status.sizingProfileState. NodeShare needs no history.
const (
	// SizingProfileStatic applies exactly what the template says — the default,
	// and today's behavior.
	SizingProfileStatic = "Static"
	// SizingProfileBinpack sets requests == limits from the observed history
	// (CPU: p95 of per-job peaks; memory: observed max + OOM headroom) →
	// Guaranteed QoS, predictable packing, maximum workers per expensive node.
	// The CPU limit this implies deliberately trades burst headroom for
	// predictability — that is the bin-packing contract.
	SizingProfileBinpack = "Binpack"
	// SizingProfileThroughput sets requests from the observed history (p95 of
	// per-job peaks), removes any CPU limit so jobs burst into idle node
	// capacity, and sets the memory limit to the observed max scaled by
	// LimitHeadroomPercent — jobs finish faster at the cost of looser packing.
	SizingProfileThroughput = "Throughput"
	// SizingProfileNodeShare sets the runner container's requests to a declared
	// per-node allocatable envelope divided by WorkersPerNode — the GPU
	// bin-packing case (allocatable ÷ GPUs per node) — with no usage history
	// required. Limits keep the template's values.
	SizingProfileNodeShare = "NodeShare"
)

// Sizing-profile actuation states reported in status.sizingProfileState.
const (
	// SizingProfileStateActive means the selected profile is applying derived
	// values at pod build.
	SizingProfileStateActive = "Active"
	// SizingProfileStateAwaitingSamples means a history-based profile is
	// selected but not every template container has a confident recommendation
	// yet; pods provision with the template's static values until it does.
	SizingProfileStateAwaitingSamples = "AwaitingSamples"
)

// WorkerSizing configures the opt-in measured worker sizing profile (Q359
// Phase 3). Applied by the AGC at pod-build time on every acquired job, so a
// spec edit takes effect on the next job without a restart (Q117).
//
// +kubebuilder:validation:XValidation:rule="self.profile != 'NodeShare' || has(self.nodeShare)",message="the NodeShare profile requires sizing.nodeShare"
// +kubebuilder:validation:XValidation:rule="!has(self.nodeShare) || self.profile == 'NodeShare'",message="sizing.nodeShare is only meaningful with profile NodeShare"
// +kubebuilder:validation:XValidation:rule="!has(self.limitHeadroomPercent) || self.profile == 'Throughput'",message="sizing.limitHeadroomPercent is only meaningful with profile Throughput"
type WorkerSizing struct {
	// Profile selects the sizing behavior; see the SizingProfile* constants.
	//
	// +kubebuilder:default=Static
	// +kubebuilder:validation:Enum=Static;Binpack;Throughput;NodeShare
	// +optional
	Profile string `json:"profile,omitempty"`

	// LimitHeadroomPercent (Throughput only) scales the observed per-job memory
	// peak into the derived memory limit: limit = peak × percent / 100. Defaults
	// to 150 (the dogfood-validated OOM-headroom band, rounded up for the
	// burst-friendly profile). Memory only — no CPU limit is ever derived.
	//
	// +optional
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=1000
	LimitHeadroomPercent *int32 `json:"limitHeadroomPercent,omitempty"`

	// NodeShare declares the per-node envelope the NodeShare profile divides.
	// Required when (and only meaningful when) profile is NodeShare.
	//
	// +optional
	NodeShare *NodeShareSizing `json:"nodeShare,omitempty"`

	// MinRequests / MaxRequests clamp every derived cpu/memory request (and the
	// limits that track them) within an operator-set floor/ceiling, bounding how
	// far a skewed usage history can push the derived values. Keys other than
	// cpu and memory are ignored.
	//
	// +optional
	MinRequests corev1.ResourceList `json:"minRequests,omitempty"`
	// +optional
	MaxRequests corev1.ResourceList `json:"maxRequests,omitempty"`
}

// NodeShareSizing declares the per-node allocatable envelope the NodeShare
// profile divides among workers. The operator declares the envelope rather than
// the AGC reading Node objects: the AGC is deliberately namespace-scoped (no
// cluster-scoped RBAC), and the operator knows which node shape the set's
// scheduling constraints target.
type NodeShareSizing struct {
	// Allocatable is the node's allocatable cpu/memory to divide (from
	// `kubectl describe node`, minus any system/sidecar overhead the operator
	// reserves). Keys other than cpu and memory are ignored, so at least one of
	// the two must be present: an envelope carrying neither (empty, or GPUs
	// only) derives nothing while the profile still reports Active (Q484).
	// Declaring just one is legitimate — the other resource keeps the
	// template's ask.
	//
	// +kubebuilder:validation:XValidation:rule="'cpu' in self || 'memory' in self",message="sizing.nodeShare.allocatable must declare cpu, memory, or both; other resources are ignored"
	Allocatable corev1.ResourceList `json:"allocatable"`

	// WorkersPerNode is the divisor: the number of worker pods that should pack
	// onto one node (for GPU nodes, typically the GPU count).
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	WorkersPerNode int32 `json:"workersPerNode"`
}

// ScaleUpRateLimit configures the opt-in per-RunnerSet worker-pod creation-rate
// limit (Q223): a token bucket where MaxPerSecond is the sustained refill rate and
// Burst is the bucket depth (the largest instantaneous batch before throttling
// engages). When the bucket is empty, an acquired job waits — holding its GitHub
// job lock, renewed in the background — until a token frees, composing with the
// namespace-quota retry wait rather than adding a new state machine.
type ScaleUpRateLimit struct {
	// MaxPerSecond is the sustained rate, in worker pods created per second, once
	// the initial burst is spent. Required when scaleUp is set.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	MaxPerSecond int32 `json:"maxPerSecond"`

	// Burst is the maximum number of worker pods that may be created in an
	// instantaneous batch before the MaxPerSecond rate throttles subsequent
	// creations — the token-bucket depth. Defaults to MaxPerSecond when omitted.
	//
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100000
	Burst *int32 `json:"burst,omitempty"`
}

// RunnerSetStatus is the observed state of a RunnerSet. It follows the uniform v2
// status/condition contract (§H.7): a listType=map Conditions slice keyed on type,
// an ObservedGeneration, and a Ready condition with the shared reason vocabulary
// (see conditions.go). Reference-resolution failures surface as Ready=False with a
// specific reason (TemplateNotFound / ProxyNotFound / …) and a message naming the
// missing object.
type RunnerSetStatus struct {
	// Conditions are the observed conditions of the runner set. Known types: Ready,
	// Degraded, EgressUnattributed, PossibleReapBlockingSidecar, WorkerQuotaPressure,
	// WorkerQuotaExceeded, WorkersUnschedulable, RateLimited, RunnerVersionTooOld,
	// SizingDrift.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ActiveSessions is the number of currently open long-poll sessions.
	//
	// +optional
	ActiveSessions int32 `json:"activeSessions,omitempty"`

	// ActiveJobs is the number of worker pods currently in the Running phase
	// (a job is actively executing). Derived from the worker pod phase count
	// during each reconcile; see also PendingJobs.
	//
	// +optional
	ActiveJobs int32 `json:"activeJobs,omitempty"`

	// PendingJobs is the number of worker pods currently in the Pending phase
	// (a job has been acquired and a pod spawned, but the pod is not yet
	// running — waiting on scheduling, image pull, or node readiness). Pods
	// that remain Pending past spec.pendingPodDeadline are deleted by the
	// controller; a sustained non-zero count warrants checking events and
	// scheduling constraints.
	//
	// +optional
	PendingJobs int32 `json:"pendingJobs,omitempty"`

	// ProxyMode records how this runner set's worker egress reaches GitHub:
	// "Proxied" (through the resolved EgressProxy, with stable per-tenant egress IPs)
	// or "Direct" (no proxyRef/defaultProxyRef, still NetworkPolicy-restricted to
	// GitHub + DNS but without per-tenant IP attribution). Explicit so "no proxy" is
	// an auditable state, not an inferred absence (§H.10). Paired with the advisory
	// EgressUnattributed condition when Direct.
	//
	// +optional
	// +kubebuilder:validation:Enum=Proxied;Direct
	ProxyMode string `json:"proxyMode,omitempty"`

	// TemplateSource records which rung of the template-resolution chain supplied this
	// runner set's worker pod shape (Q172, §H.4): "TemplateRef" (its own spec.templateRef),
	// "GatewayDefault" (the gateway's spec.defaultTemplateRef, inherited because templateRef
	// was unset), or "ClusterDefault" (the single cluster-default ClusterRunnerTemplate,
	// resolved because neither was set). Explicit so an operator can audit whether a set
	// runs on an explicit template or a default without inspecting the gateway and cluster
	// state. Empty until the references resolve.
	//
	// +optional
	// +kubebuilder:validation:Enum=TemplateRef;GatewayDefault;ClusterDefault
	TemplateSource string `json:"templateSource,omitempty"`

	// SizingProfileState reports whether the opt-in sizing profile
	// (spec.sizing.profile) is actuating: "Active" — derived values are applied
	// at pod build; "AwaitingSamples" — a history-based profile (Binpack /
	// Throughput) is selected but not every template container has a confident
	// recommendation yet, so pods provision with the template's static values
	// until the history accumulates (whole-pod fallback, keeping QoS
	// predictable). Empty when no profile (or Static) is selected. Explicit so
	// "is the profile live yet" is auditable status, not something to infer from
	// pod specs (the proxyMode precedent).
	//
	// +optional
	// +kubebuilder:validation:Enum=Active;AwaitingSamples
	SizingProfileState string `json:"sizingProfileState,omitempty"`

	// SizingRecommendation is the per-container worker resource recommendation
	// derived from measured per-job usage peaks (Q359 Phase 2), refreshed by the
	// AGC as jobs complete. Advisory only: nothing is applied to worker pods —
	// the operator (or a future opt-in sizing profile) acts on it. It doubles as
	// the persistence for the usage aggregates: the AGC re-seeds its in-memory
	// history from this field on restart, so the observation window survives
	// control-plane rollouts (status is the store; no separate backing store).
	//
	// +optional
	// +listType=map
	// +listMapKey=container
	SizingRecommendation []ContainerSizingRecommendation `json:"sizingRecommendation,omitempty"`

	// ObservedGeneration is the .metadata.generation the most recent reconcile acted on.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// ContainerSizingRecommendation is the measured-usage-derived resource
// recommendation for one container of a runner set's worker pods (Q359 Phase 2).
// Derivation follows the worker right-sizing model
// (docs/operations/worker-rightsizing.md): requests from the p95 of per-job
// usage peaks, a memory limit with OOM headroom above the observed maximum, and
// deliberately no CPU limit (CPU is compressible — a limit only throttles bursty
// jobs for no packing benefit).
type ContainerSizingRecommendation struct {
	// Container is the container name in the worker pod template this
	// recommendation applies to.
	Container string `json:"container"`

	// Requests are the recommended resource requests (cpu, memory), derived from
	// the p95 of observed per-job usage peaks and rounded up to coarse increments
	// (sizing is bucket-granular, not exact).
	//
	// +optional
	Requests corev1.ResourceList `json:"requests,omitempty"`

	// Limits are the recommended resource limits. Memory only, at the observed
	// maximum peak plus OOM headroom; no CPU limit is ever recommended.
	//
	// +optional
	Limits corev1.ResourceList `json:"limits,omitempty"`

	// ObservedPeak is the highest per-job usage peak (cpu, memory) observed in
	// the window — the input to the recommended memory limit.
	//
	// +optional
	ObservedPeak corev1.ResourceList `json:"observedPeak,omitempty"`

	// ObservedP95 is the 95th percentile of per-job usage peaks (cpu, memory),
	// bucket-interpolated — the input to the recommended requests.
	//
	// +optional
	ObservedP95 corev1.ResourceList `json:"observedP95,omitempty"`

	// SampleCount is the number of finished jobs whose usage peaks fed this
	// recommendation — the operator's confidence signal. Jobs shorter than one
	// sampling interval are not counted (see the worker usage metrics).
	SampleCount int64 `json:"sampleCount"`

	// WindowStartTime is when this container's observation window began (first
	// sampled job, surviving AGC restarts via the re-seed).
	//
	// +optional
	WindowStartTime metav1.Time `json:"windowStartTime,omitempty"`
}

// RunnerSet is a namespace-scoped CRD reconciled by the AGC. It binds a worker pod
// shape (templateRef) and an optional egress proxy (proxyRef) to a GitHub gateway
// (gatewayRef), and carries the scheduling/quota knobs that were RunnerGroup's in
// v1alpha1. Worker pods are provisioned per acquired job and released on completion.
//
// The .spec.gatewayRef.name selectable field (KEP-4358, used by the AGC to scope its
// RunnerSet watch server-side) is deliberately declared ONLY on v2alpha1, NOT here.
// The apiserver hoists per-version selectableFields to spec level when they are
// identical across all served versions, but leaves the schema per-version when the
// versions' schemas differ (v2beta1 drops acquisitionProtocol/maxListeners). A
// spec-level selectableFields with a nil spec-level schema is rejected
// ("selectableFields may only be set when validations.schema is included"), so
// declaring the field on both versions makes the RunnerSet CRD un-installable. The
// AGC operates on v2alpha1 during coexistence, so v2alpha1 is the version that needs
// the field selector; when v2beta1 becomes the sole served version (the v2alpha1
// removal at the ScaleSet-only cut) the marker moves here and v2alpha1's is dropped —
// so the two are never declared simultaneously. See Q74.
//
// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=rs,categories=actions-gateway
// +kubebuilder:printcolumn:name="Gateway",type=string,JSONPath=`.spec.gatewayRef.name`
// +kubebuilder:printcolumn:name="ActiveSessions",type=integer,JSONPath=`.status.activeSessions`
// +kubebuilder:printcolumn:name="ActiveJobs",type=integer,JSONPath=`.status.activeJobs`
// +kubebuilder:printcolumn:name="PendingJobs",type=integer,JSONPath=`.status.pendingJobs`
// +kubebuilder:printcolumn:name="Egress",type=string,JSONPath=`.status.proxyMode`
// +kubebuilder:printcolumn:name="Template",type=string,priority=1,JSONPath=`.status.templateSource`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Reason",type=string,priority=1,JSONPath=`.status.conditions[?(@.type=='Ready')].reason`
// +kubebuilder:printcolumn:name="ObservedGen",type=integer,priority=1,JSONPath=`.status.observedGeneration`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 52",message="metadata.name must be at most 52 characters: v2 derives child resource names as <name>-<suffix> and reserves the remainder of the 63-char label/Service-name budget"
type RunnerSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RunnerSetSpec   `json:"spec,omitempty"`
	Status RunnerSetStatus `json:"status,omitempty"`
}

// RunnerSetList contains a list of RunnerSet.
//
// +kubebuilder:object:root=true
type RunnerSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RunnerSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RunnerSet{}, &RunnerSetList{})
}
