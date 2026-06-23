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

	// Egress-proxy wiring injected into the worker runner container. In v1 these
	// come from the process-wide Provisioner fields (the single per-AGC proxy); in
	// v2 from the RunnerSet's resolved EgressProxy.
	HTTPProxy          string
	HTTPSProxy         string
	NoProxy            string
	ProxyTLSSecretName string

	// SecurityProfile scales the secure-by-default worker SecurityContext to the
	// namespace's Pod Security Admission level (baseline/restricted/privileged).
	SecurityProfile string
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
