package provisioner

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LabelRunnerSet is stamped on every v2 worker pod (and job Secret) with the
// owning RunnerSet's name as its value, on the v2 actions-gateway.com domain. It
// is the v2 counterpart of LabelRunnerGroup: the RunnerSet controller's Pod watch,
// reaper, and active-pod count filter on it, and keeping it on a distinct key from
// the v1 LabelRunnerGroup means the RunnerGroup and RunnerSet controllers' Pod
// watches and reapers never cross-wire during v1/v2 coexistence.
const LabelRunnerSet = "actions-gateway.com/runner-set"

// AnnotationJobCompletedAt is stamped on a scale-set worker pod, with an RFC 3339
// UTC timestamp as its value, when the scale-set listener observes the terminal
// JobCompleted for the job that pod was created for (markJobCompleted). It is the
// reap deadline for a worker that is still Running after its job is over — a worker
// that registered but never received its job would otherwise hold a concurrency slot
// and a node forever, because the reaper counts PodRunning as active (Q420).
//
// It is controller-set and informational: never set it by hand and never use it for
// security enforcement. The classic tier never stamps it (its provision() goroutine
// owns the pod through to a terminal phase), so it is scale-set-only in practice.
const AnnotationJobCompletedAt = "actions-gateway.com/job-completed-at"

// AnnotationRunnerName is stamped on a scale-set worker pod with the name the listener
// pre-registered its runner under at GitHub (generatejitconfig mints the record before
// the pod exists). It is the pod's registration handle: the reaper reads it back to
// deregister the record when it deletes the pod, and the listener's start-up sweep
// treats a name stamped on a live pod as claimed and therefore not collectable (Q550).
//
// It has to live on the pod because nothing else outlives the listener goroutine that
// minted the name. The reaper runs arbitrarily later and possibly in a different AGC
// process, and the fire-and-forget scale-set tier keeps no in-process job state — the
// same reason Q417 put the run identity and Q420 the reap deadline here.
//
// A pod without it (any classic-tier worker, or a scale-set worker created before this
// annotation existed) is reaped exactly as before, with no deregistration attempted.
//
// Controller-set and informational: never set it by hand and never use it for security
// enforcement.
const AnnotationRunnerName = "actions-gateway.com/runner-name"

// AnnotationSizingProfile is stamped on a worker pod, with the profile name as its
// value, when an opt-in sizing profile actually derived that pod's cpu/memory ask
// (Q489). A pod built from the template's static values — Static, or a history-based
// profile still AwaitingSamples — carries no such annotation, so presence answers
// "was this pod sized by the profile, or by the template?" without inferring it from
// the numbers.
//
// It is what makes the SizingProfileOverridden condition exact: the condition asks
// whether admission re-injected a CPU limit the Throughput profile removed, and
// only a pod this annotation marks was ever built without one. Without it, a pod
// created moments before the operator selected Throughput — still legitimately
// running the template's CPU limit — would read as an override for the length of
// its job.
//
// Controller-set and informational: never set it by hand and never use it for
// security enforcement.
const AnnotationSizingProfile = "actions-gateway.com/sizing-profile"

// LabelAcquisitionProtocol records which acquisition tier provisioned a worker pod.
// Only the scale-set tier stamps it (AcquisitionProtocolScaleSet); a classic worker
// carries no such label, so presence is the tier test.
//
// It exists because the two tiers own a worker pod's lifecycle differently, and
// something outside the pod has to be able to tell them apart. A classic worker is
// watched to a terminal phase by the provision() goroutine that created it, which
// handles its own eviction; a scale-set worker is fire-and-forget, so the owning
// reconciler adopts eviction recovery for it (Q417). Firing both on the same pod
// would spend two slots of one run's retry budget for one eviction, so the
// reconciler's pass filters on this label.
const LabelAcquisitionProtocol = "actions-gateway.com/acquisition-protocol"

// AcquisitionProtocolScaleSet is the LabelAcquisitionProtocol value for a worker pod
// provisioned by the scale-set tier. It matches the RunnerSet spec.acquisitionProtocol
// enum value of the same name.
const AcquisitionProtocolScaleSet = "ScaleSet"

// AnnotationEvictionHandledAt is stamped on a scale-set worker pod, with an RFC 3339
// UTC timestamp as its value, when the owning reconciler has adjudicated that pod's
// eviction — whether it went on to trigger a re-run, found no run identity to re-run,
// or found the retry budget exhausted (Q417).
//
// It is a claim, not a log line: the reconciler sets it under an optimistic lock
// BEFORE calling GitHub, so an evicted pod re-observed by a later reconcile (or by a
// second AGC replica) is skipped rather than re-run. That ordering makes automatic
// recovery at-most-once per evicted pod, which is the safe direction — a duplicate
// re-run would silently spend another slot of the run's budget, while a missed one is
// visible in the metric and the pod's own absence of this annotation.
//
// It is controller-set and informational: never set it by hand and never use it for
// security enforcement.
const AnnotationEvictionHandledAt = "actions-gateway.com/eviction-handled-at"

// WorkerContainerName is the name of the runner container in every worker pod
// template — the container the runner engine executes in, and the one the
// NodeShare sizing profile targets (Q359 Phase 3).
const WorkerContainerName = "runner"

// Target is the controller object a Provisioner provisions worker pods and job
// Secrets for. The v1 RunnerGroup and the v2 RunnerSet each have an adapter that
// satisfies it (runnerGroupTarget here, runnerSetTarget in the AGC controller
// package), so the provisioner's pod/Secret build path is written once against
// this seam rather than against either API type.
//
// The provisioner uses the Target for the OwnerReference and identity stamped on
// every worker pod/Secret, and calls Resolve once per acquired job to obtain the
// fresh provisioning inputs — preserving the Q117 property that a spec edit made
// after a listener started takes effect on the next job without an AGC restart.
// Resolve is what makes v2 reference resolution (templateRef → RunnerTemplate,
// proxyRef → EgressProxy) re-evaluate per job and fail closed when a reference no
// longer resolves.
type Target interface {
	// Key is the owner's namespace/name. Used for pod naming, log scoping, the
	// admission-gate key, and the per-group metrics labels.
	Key() client.ObjectKey

	// OwnerRef is the controller OwnerReference stamped on every worker pod and
	// job Secret so deleting the owner cascade-GCs them (including any orphaned by
	// an AGC crash). It carries the owner's GVK and UID, which differ between the
	// v1 RunnerGroup and the v2 RunnerSet. BlockOwnerDeletion is left unset (the
	// owner carries its own finalizer for ordered cleanup).
	OwnerRef() metav1.OwnerReference

	// PodOwnerLabels are the identity labels stamped on worker pods and job
	// Secrets AND used as the selector to count/list this owner's worker pods.
	// Distinct per API (LabelRunnerGroup for v1, LabelRunnerSet for v2).
	PodOwnerLabels() map[string]string

	// Ceiling returns the maximum concurrent worker pods the owner may run and
	// whether a ceiling applies at all, re-read from the fresh spec each call so
	// the admission gate honours maxWorkers/priorityTiers edits without a restart
	// (Q117). It is the cheap path used by the admission gate; Resolve is the full
	// path used to actually build a pod.
	Ceiling(ctx context.Context) (limit int32, bounded bool)

	// QuotaExhausted reports whether the namespace ResourceQuota currently lacks
	// the headroom to admit one more worker pod of this owner's shape, with a
	// human-readable detail naming the binding resource. It is the observed
	// counterpart to Ceiling's declared limit, and the admission gate refuses to
	// claim a job when it returns true (#784) — see admission.go for why the quota
	// rung is safe to gate on and the scheduler's Unschedulable verdict is not.
	//
	// Fail-open by contract: an owner or quota it cannot read yields false, leaving
	// the provisioner's maxQuotaRetries loop as the backstop. Like Ceiling it is a
	// per-delivery cache read, not the full Resolve path.
	QuotaExhausted(ctx context.Context) (exhausted bool, detail string)

	// QuotaCapacity returns the same observed quota signal as an integer instead of a
	// boolean: the total number of worker pods this owner may have in flight given
	// live namespace-ResourceQuota headroom, never above max. It is what the
	// scale-set tier advertises the quota rung with, because that tier states a
	// number of jobs per poll rather than deciding per delivered job (Q443) — see
	// AdvertiseCapacity.
	//
	// Fail-open by contract, exactly like QuotaExhausted: bounded=false means "no
	// quota-derived bound applies" (no quota, or nothing readable), and the caller
	// keeps the declared ceiling. An owner whose tier has no integer form returns
	// (0, false).
	QuotaCapacity(ctx context.Context, max int32) (limit int32, bounded bool)

	// CapacityDeclined reports whether the owner's opt-in capacity gate is currently
	// refusing intake because the cluster cannot place this owner's worker pods, with
	// a human-readable detail naming the signal that said so (Q405). It is the third
	// rung of the ladder — placeability, which neither the declared ceiling nor
	// namespace quota can answer, because quota is namespace-wide and pool-blind
	// while placeability is a property of the pod shape and the pools it can land on.
	//
	// Fail-open by contract, like QuotaExhausted, and additionally OFF by default: an
	// owner that did not opt in (spec.capacityGate unset or mode Off), an owner it
	// cannot read, or an owner whose tier has no capacity gate all yield false. The
	// gate may under-gate freely — that is today's behavior — but must never
	// over-gate, because over-gating starves a tenant. A LATCHED gate (the declined
	// evidence was reaped, Q512) is not fully closed either: it declines only while
	// a probe pod is outstanding, admitting exactly one probe job per deadline
	// window so the latch can resolve.
	CapacityDeclined(ctx context.Context) (declined bool, detail string)

	// DeclinedCapacity returns the same capacity signal as an integer instead of a
	// boolean, for the scale-set tier's per-poll advertisement — the CapacityDeclined
	// counterpart of QuotaCapacity (Q443's invariant: a rung expressed in only one
	// form ships to only one tier). A live decline means "no room for another worker
	// pod", so the bound is the owner's own in-flight worker pods: GitHub keeps the
	// jobs it has already assigned and is offered no more. A latched decline (Q512)
	// adds one probe slot while no probe pod is outstanding, so this tier trickles
	// at one job per deadline window instead of snapping back to the full ceiling
	// when the reaper deletes the gate's evidence.
	//
	// Fail-open by contract exactly like QuotaCapacity: bounded=false means "no
	// capacity-derived bound applies" (gate off, nothing declined, or nothing
	// readable) and the caller keeps whatever the earlier rungs left.
	DeclinedCapacity(ctx context.Context, max int32) (limit int32, bounded bool)

	// Resolve returns the current, fully-resolved provisioning inputs, re-read on
	// every acquired job. A non-nil error means a required reference no longer
	// resolves (v2: missing RunnerTemplate/EgressProxy); the provisioner fails the
	// job fail-closed without creating a worker pod, so no wiring is ever created
	// in the gap.
	Resolve(ctx context.Context) (*ResolvedSpec, error)

	// RecordEvent records a Kubernetes Event on the owner object (the RunnerGroup or
	// RunnerSet) so provisioning-time incidents — namespace ResourceQuota retry
	// exhaustion, eviction-retry exhaustion — surface in `kubectl describe` and event
	// watchers, complementing the metrics that already count them. The adapter routes
	// it to the owning reconciler, which records it on the live object; routing is
	// per-Target because one Provisioner is shared across the v1 and v2 owners. A
	// no-op when no recorder is wired (unit tests). action and note follow the
	// client-go events API.
	RecordEvent(eventtype, reason, action, note string)
}

// ResolvedSpec is the fully-resolved, already-defaulted per-job provisioning
// input. The v1 adapter fills it from RunnerGroup.Spec plus the process-wide
// Provisioner proxy/security fields (one egress proxy per AGC in v1); the v2
// adapter fills it from the RunnerSet plus its resolved RunnerTemplate and
// EgressProxy (per-RunnerSet proxy in v2). Folding both into one value lets the
// provisioner build worker pods identically for either API.
type ResolvedSpec struct {
	// PodTemplate is the worker pod shape: RunnerGroup.spec.podTemplate in v1, the
	// referenced (Cluster)RunnerTemplate.spec.podTemplate in v2.
	PodTemplate corev1.PodTemplateSpec
	// WorkerImage is the runner container image, or "" to fall back to the
	// Provisioner default (which buildPod applies).
	WorkerImage string

	// MaxWorkers / PriorityTiers are the concurrency ceiling inputs (same meaning
	// and shape in both APIs).
	MaxWorkers    *int32
	PriorityTiers []TierThreshold

	// Eviction/quota/TTL tunables, already defaulted (the adapter applies the
	// per-owner override or the provisioner-level default).
	MaxEvictionRetries int
	EvictionRetryDelay time.Duration
	MaxQuotaRetries    int
	QuotaRetryDelay    time.Duration
	CompletedPodTTL    time.Duration

	// MaxWorkerLifetime is the worker pod's activeDeadlineSeconds — the
	// provision-time cap that bounds a worker orphaned while the AGC was down
	// (Q438). Zero means no cap is stamped (the operator opted out with "0s").
	MaxWorkerLifetime time.Duration

	// Egress-proxy wiring injected into the worker runner container. In v1 these
	// come from the process-wide Provisioner fields (the single per-AGC proxy); in
	// v2 from the RunnerSet's resolved EgressProxy.
	HTTPProxy          string
	HTTPSProxy         string
	NoProxy            string
	ProxyTLSSecretName string
	// ProxyCAConfigMapName carries the proxy CA when the proxy lives in another
	// namespace and its TLS Secret is therefore unreadable from here (§H.9). Set
	// instead of ProxyTLSSecretName, never alongside it.
	ProxyCAConfigMapName string

	// GitHubCAConfigMapName names the ConfigMap holding the CA bundle fronting this
	// gateway's GHES appliance, projected into the runner container so it trusts the
	// same appliance the AGC does (Q536). v2 only: it comes from the gateway's
	// spec.githubCABundleRef, which v1alpha1 has no field for.
	GitHubCAConfigMapName string

	// SecurityProfile scales the secure-by-default worker SecurityContext to the
	// namespace's Pod Security Admission level (baseline/restricted/privileged).
	SecurityProfile string

	// ScaleUp is the opt-in worker-pod creation-rate limit (Q223), or nil for no
	// limit (the default). Nil ⇒ immediate provisioning; non-nil ⇒ the provisioner
	// gates each pod creation through a per-owner token bucket. The v1/v2 adapters
	// fill it from RunnerGroup/RunnerSet.spec.scaleUp, defaulting Burst to
	// MaxPerSecond when the spec omits it.
	ScaleUp *ScaleUpConfig
}

// ScaleUpConfig is the neutral, already-defaulted worker-pod creation-rate limit,
// decoupled from the v1/v2 API ScaleUpRateLimit types so the scaleUpLimiter is
// shared across both APIs (the TierThreshold pattern). MaxPerSecond is the
// sustained token refill rate (pods/sec) and Burst is the token-bucket depth; both
// are ≥1 by the time they reach here (the adapter applies the Burst=MaxPerSecond
// default and the CRD enforces the minimums).
type ScaleUpConfig struct {
	MaxPerSecond int32
	Burst        int32
}

// TierThreshold is a neutral priority tier (PriorityClass name + cumulative
// pod-count threshold), decoupled from the v1/v2 API PriorityTier types so the
// ceiling logic is shared across both.
type TierThreshold struct {
	PriorityClassName string
	Threshold         int32
}

// CompletedPodTTLOrDefault returns the terminal-pod retention for the given
// spec value, applying DefaultCompletedPodTTL when nil. Shared by the v1 and v2
// reapers so terminal-pod retention is computed one way.
func CompletedPodTTLOrDefault(d *metav1.Duration) time.Duration {
	if d != nil {
		return d.Duration
	}
	return DefaultCompletedPodTTL
}

// PendingPodDeadlineOrDefault returns the stuck-Pending deadline for the given
// spec value, applying DefaultPendingPodDeadline when nil.
func PendingPodDeadlineOrDefault(d *metav1.Duration) time.Duration {
	if d != nil {
		return d.Duration
	}
	return DefaultPendingPodDeadline
}

// MaxWorkerLifetimeOrDefault returns the worker-pod lifetime cap for the given
// spec value, applying DefaultMaxWorkerLifetime when nil. An explicit "0s" is
// honoured as "no cap" rather than defaulted, so an operator can opt out; a
// negative value (which the CRD rejects) is likewise treated as no cap rather
// than stamped, since a negative activeDeadlineSeconds is invalid to the
// apiserver and would fail every pod create. Shared by the v1 and v2 adapters so
// the cap is computed one way.
func MaxWorkerLifetimeOrDefault(d *metav1.Duration) time.Duration {
	if d == nil {
		return DefaultMaxWorkerLifetime
	}
	if d.Duration <= 0 {
		return 0
	}
	return d.Duration
}

// WorkerCeilingFromTiers returns the maximum concurrent worker pods implied by a
// (priorityTiers, maxWorkers) pair, mirroring ceilingCheck's hold decision: the
// maximum tier threshold when tiers are set, else maxWorkers, else unbounded.
// Shared by the admission gate and the worker-quota footprint so both enforce the
// same ceiling — one source of truth.
func WorkerCeilingFromTiers(tiers []TierThreshold, maxWorkers *int32) (limit int32, bounded bool) {
	if len(tiers) > 0 {
		var max int32
		for _, t := range tiers {
			if t.Threshold > max {
				max = t.Threshold
			}
		}
		return max, true
	}
	if maxWorkers != nil {
		return *maxWorkers, true
	}
	return 0, false
}
