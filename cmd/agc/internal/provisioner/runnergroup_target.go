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
	return metav1.OwnerReference{
		APIVersion: v1alpha1.GroupVersion.String(),
		Kind:       "RunnerGroup",
		Name:       t.snapshot.Name,
		UID:        t.snapshot.UID,
		Controller: ptr.To(true),
	}
}

func (t *runnerGroupTarget) PodOwnerLabels() map[string]string {
	return map[string]string{LabelRunnerGroup: t.snapshot.Name}
}

func (t *runnerGroupTarget) Ceiling(ctx context.Context) (int32, bool) {
	rg := t.current(ctx)
	return WorkerCeilingFromTiers(tierThresholds(rg.Spec.PriorityTiers), rg.Spec.MaxWorkers)
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
	return spec, nil
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
