package provisioner

import (
	"context"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// runnerGroupTarget adapts a v1alpha1.RunnerGroup to the provisioner Target seam.
// It preserves the pre-seam v1 behaviour exactly: identity and owner reference
// come from the listener-start snapshot, while Ceiling/Resolve re-read the current
// RunnerGroup from the cached client so spec edits take effect on the next job
// without an AGC restart (Q117). Proxy and security inputs come from the
// process-wide Provisioner fields, matching v1's one-proxy-per-AGC model.
type runnerGroupTarget struct {
	p *Provisioner
	// snapshot is the RunnerGroup captured when the listener started, used only for
	// identity (namespace/name/UID) and as the fallback when the cached re-read fails.
	snapshot *v1alpha1.RunnerGroup
}

// runnerGroupTarget returns a Target bound to the given RunnerGroup snapshot.
func (p *Provisioner) runnerGroupTarget(rg *v1alpha1.RunnerGroup) Target {
	return &runnerGroupTarget{p: p, snapshot: rg}
}

func (t *runnerGroupTarget) Key() client.ObjectKey {
	return client.ObjectKeyFromObject(t.snapshot)
}

// OwnerRef returns a controller OwnerReference to the RunnerGroup, stamped on
// every worker pod and job Secret so deleting the RunnerGroup — directly, via
// ActionsGateway teardown, or via namespace deletion — cascade-deletes them,
// including any orphaned by an AGC crash. BlockOwnerDeletion is left unset: the
// RunnerGroup carries its own finalizer for ordered cleanup, and setting it would
// require update on the owner's finalizers under the
// OwnerReferencesPermissionEnforcement admission plugin.
func (t *runnerGroupTarget) OwnerRef() metav1.OwnerReference {
	return RunnerGroupOwnerRef(t.snapshot)
}

// RunnerGroupOwnerRef returns the controller OwnerReference to rg that every object
// the AGC derives from a RunnerGroup carries — worker pods and job Secrets via the
// Target above, agent-pool Secrets via the RunnerGroup reconciler. It is exported so
// the two paths cannot drift into two spellings of the same reference.
func RunnerGroupOwnerRef(rg *v1alpha1.RunnerGroup) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: v1alpha1.GroupVersion.String(),
		Kind:       "RunnerGroup",
		Name:       rg.Name,
		UID:        rg.UID,
		Controller: ptr.To(true),
	}
}

func (t *runnerGroupTarget) PodOwnerLabels() map[string]string {
	return map[string]string{LabelRunnerGroup: t.snapshot.Name}
}

// RecordEvent routes an owner-scoped Event to the RunnerGroup reconciler via the
// process-wide Provisioner.Events recorder (runnerGroupTarget is the only Target
// the Provisioner constructs, and it is v1-only). A no-op when no recorder is wired.
func (t *runnerGroupTarget) RecordEvent(eventtype, reason, action, note string) {
	if t.p.Events == nil {
		return
	}
	k := t.Key()
	t.p.Events.Event(k.Namespace, k.Name, eventtype, reason, action, note)
}

func (t *runnerGroupTarget) Ceiling(ctx context.Context) (int32, bool) {
	rg := t.current(ctx)
	return WorkerCeilingFromTiers(tierThresholds(rg.Spec.PriorityTiers), rg.Spec.MaxWorkers)
}

// QuotaExhausted checks live namespace-ResourceQuota headroom against one more
// worker pod built from the current spec.podTemplate. Fail-open on any read
// failure (current falls back to the listener-start snapshot; WorkerQuotaExhausted
// returns false when it cannot list quotas).
func (t *runnerGroupTarget) QuotaExhausted(ctx context.Context) (bool, string) {
	rg := t.current(ctx)
	return WorkerQuotaExhausted(ctx, t.p.Client, rg.Namespace, &rg.Spec.PodTemplate.Spec)
}

// QuotaCapacity reports no quota-derived bound for a v1 RunnerGroup. The integer form
// of the quota rung exists for the scale-set tier's per-poll capacity advertisement
// (Q443); v1 acquires per delivered job through Admit's boolean rung, which is the
// authoritative form for it, and v1 is terminal (Q273/Q264) so it never grows one.
func (t *runnerGroupTarget) QuotaCapacity(context.Context, int32) (int32, bool) {
	return 0, false
}

// CapacityDeclined and DeclinedCapacity report no capacity signal for a v1
// RunnerGroup: the capacity gate is v2-only (Q405). v1 is terminal (Q273/Q264), so
// rather than grow the v1 API a `spec.capacityGate` it will never carry past
// `v2.0.0`, the rung no-ops here and a v1 group keeps today's behavior exactly —
// which is what fail-open means for this rung anyway.
func (t *runnerGroupTarget) CapacityDeclined(context.Context) (bool, string) {
	return false, ""
}

func (t *runnerGroupTarget) DeclinedCapacity(context.Context, int32) (int32, bool) {
	return 0, false
}

// ScaleUpLimit reads the rate limit from the fresh RunnerGroup spec, so the rate
// rung of the admission ladder honours a spec.scaleUp edit on the next delivered job
// without an AGC restart (Q117) — the same property current() gives every other
// per-job read here.
func (t *runnerGroupTarget) ScaleUpLimit(ctx context.Context) *ScaleUpConfig {
	return scaleUpConfigFromV1(t.current(ctx).Spec.ScaleUp)
}

func (t *runnerGroupTarget) Resolve(ctx context.Context) (*ResolvedSpec, error) {
	rg := t.current(ctx)
	p := t.p
	spec := &ResolvedSpec{
		PodTemplate:        rg.Spec.PodTemplate,
		WorkerImage:        rg.Spec.WorkerImage,
		MaxWorkers:         rg.Spec.MaxWorkers,
		PriorityTiers:      tierThresholds(rg.Spec.PriorityTiers),
		MaxEvictionRetries: p.MaxEvictionRetries,
		EvictionRetryDelay: p.EvictionRetryDelay,
		MaxQuotaRetries:    p.MaxQuotaRetries,
		QuotaRetryDelay:    p.QuotaRetryDelay,
		CompletedPodTTL:    CompletedPodTTLOrDefault(rg.Spec.CompletedPodTTL),
		MaxWorkerLifetime:  MaxWorkerLifetimeOrDefault(rg.Spec.MaxWorkerLifetime),
		HTTPProxy:          p.HTTPProxy,
		HTTPSProxy:         p.HTTPSProxy,
		NoProxy:            p.NoProxy,
		ProxyTLSSecretName: p.ProxyTLSSecretName,
		SecurityProfile:    p.SecurityProfile,
	}
	if rg.Spec.MaxEvictionRetries != nil {
		spec.MaxEvictionRetries = int(*rg.Spec.MaxEvictionRetries)
	}
	if rg.Spec.EvictionRetryDelay != nil && rg.Spec.EvictionRetryDelay.Duration > 0 {
		spec.EvictionRetryDelay = rg.Spec.EvictionRetryDelay.Duration
	}
	if rg.Spec.MaxQuotaRetries != nil {
		spec.MaxQuotaRetries = int(*rg.Spec.MaxQuotaRetries)
	}
	if rg.Spec.QuotaRetryDelay != nil && rg.Spec.QuotaRetryDelay.Duration > 0 {
		spec.QuotaRetryDelay = rg.Spec.QuotaRetryDelay.Duration
	}
	spec.ScaleUp = scaleUpConfigFromV1(rg.Spec.ScaleUp)
	return spec, nil
}

// scaleUpConfigFromV1 converts the v1 ScaleUpRateLimit into the neutral
// ScaleUpConfig the limiter consumes, applying the Burst=MaxPerSecond default when
// the spec omits burst. Nil in ⇒ nil out (no rate limit).
func scaleUpConfigFromV1(s *v1alpha1.ScaleUpRateLimit) *ScaleUpConfig {
	if s == nil {
		return nil
	}
	burst := s.MaxPerSecond
	if s.Burst != nil {
		burst = *s.Burst
	}
	return &ScaleUpConfig{MaxPerSecond: s.MaxPerSecond, Burst: burst}
}

// current re-reads the RunnerGroup named by the listener-start snapshot from the
// (cache-backed) client so each job sees the latest spec. On any read error —
// including the group having been deleted out from under a listener mid-shutdown —
// it logs and falls back to the snapshot, preserving the pre-Q117 behaviour rather
// than failing the job. The read hits the shared informer cache (mgr.GetClient()),
// not the API server, so it is cheap per job.
func (t *runnerGroupTarget) current(ctx context.Context) *v1alpha1.RunnerGroup {
	fresh := &v1alpha1.RunnerGroup{}
	if err := t.p.Client.Get(ctx, client.ObjectKeyFromObject(t.snapshot), fresh); err != nil {
		t.p.logForKey(t.Key()).Warn("could not re-read RunnerGroup for current spec; using listener-start snapshot", "error", err)
		return t.snapshot
	}
	return fresh
}

// tierThresholds converts v1alpha1 priority tiers to the neutral TierThreshold
// shape the shared ceiling logic consumes.
func tierThresholds(tiers []v1alpha1.PriorityTier) []TierThreshold {
	if len(tiers) == 0 {
		return nil
	}
	out := make([]TierThreshold, len(tiers))
	for i, t := range tiers {
		out[i] = TierThreshold{PriorityClassName: t.PriorityClassName, Threshold: t.Threshold}
	}
	return out
}
