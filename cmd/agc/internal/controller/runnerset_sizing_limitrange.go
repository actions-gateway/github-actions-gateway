package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// The Throughput sizing profile's mechanism is the ABSENCE of a runner-container
// CPU limit: requests come from the measured history, no CPU limit is set, and jobs
// burst into idle node capacity. A platform-owned LimitRange with a Container-type
// cpu default puts the limit straight back at admission — the pod is admitted,
// status.sizingProfileState still reports Active, and every other signal looks
// correct while bursting is silently gone (Q489).
//
// Nothing in the pod, the RunnerSet, or the quota conditions can show that, because
// nothing was rejected. So the AGC reads the LimitRange itself and reports the
// conflict as the advisory SizingProfileOverridden condition. This stays a runtime
// signal, not an admission gate — cross-object admission validation is exactly what
// §H.7 avoids, and the LimitRange is platform-owned and can change after the
// RunnerSet is written.
//
// Why this reads LimitRanges instead of watching them, unlike the sibling
// ResourceQuota conditions: a Watches() registration establishes the informer when
// the manager starts, and a source that cannot sync within CacheSyncTimeout fails
// the controller's start, which fails the manager's — so the whole AGC crash-loops,
// taking job acquisition for every RunnerSet with it. A LIST 403 is the likeliest
// cause (the limitranges grant is new, and the AGC image upgrades independently of
// the chart shipping agc-tenant-role), but any cause does it: a slow authorization
// webhook, apiserver pressure during a rollout. That is the wrong price for a
// diagnostic. Reading instead also means installs on the default Static profile
// never establish the informer at all.
//
// Two costs this does NOT avoid, both only on an install missing the grant. The
// first read still creates the informer, so its reflector then retries — and logs —
// for the process lifetime, and every reconcile of a Throughput set pays the full
// limitRangeReadTimeout rather than just the first. An uncached APIReader read
// would avoid both (an instant 403, no reflector) at the cost of a live LIST on
// every reconcile of every Throughput set — a permanent tax on the healthy path to
// spare the misconfigured one, which is the wrong way round. The cached read is
// free once synced; the cluster-scoped RuntimeClass read
// (provisioner.ResolveWorkerPodSpec) made the same call.
//
// A hand-rolled role granting get/list but not watch is a different failure and not
// one this addresses: the initial LIST succeeds, the informer reports synced, and
// the reflector then fails to establish the watch — a silently stale cache, watched
// or read. The shipped chart grants watch.
//
// The cost of not watching is the trigger, not the freshness: once the informer
// exists the cache tracks LimitRange edits, but a LimitRange appearing does not
// itself enqueue a reconcile, so the condition refreshes on the set's next one
// (worker-pod events while jobs run, or the resync period on an idle set).
// Acceptable for a signal whose subject — jobs not bursting — only exists while
// jobs are running.

// limitRangeReadTimeout bounds the LimitRange list. On an install whose
// agc-tenant-role predates the limitranges grant the informer can never sync, and an
// unbounded list would block the reconcile instead of degrading it. Paid on every
// reconcile of a Throughput set until the grant lands, not just the first.
const limitRangeReadTimeout = 2 * time.Second

// limitRangeCPUOverride names the LimitRange entry that supplies a container cpu
// limit to a container declaring none.
type limitRangeCPUOverride struct {
	// limitRange is the name of the offending LimitRange object.
	limitRange string
	// cpu is the limit it imposes.
	cpu resource.Quantity
	// fromMax reports that the value came from the entry's max rather than an
	// explicit default (Kubernetes defaults the limit to max when no default is
	// declared).
	fromMax bool
}

// applySizingProfileOverride upserts the SizingProfileOverridden condition from the
// namespace's LimitRanges. It is evaluated only under the Throughput profile — the
// one profile whose contract a namespace policy can quietly void — and the condition
// is REMOVED under any other profile, so switching to Binpack or back to Static does
// not strand a True alarm about a limit that profile sets for itself.
//
// Evaluated regardless of sizingProfileState: a conflict is worth reporting while
// Throughput is still AwaitingSamples, since it is the state the profile will
// actuate into.
func (r *RunnerSetReconciler) applySizingProfileOverride(ctx context.Context, rs *v2alpha1.RunnerSet) {
	if rs.Spec.Sizing == nil || rs.Spec.Sizing.Profile != v2alpha1.SizingProfileThroughput {
		meta.RemoveStatusCondition(&rs.Status.Conditions, v2alpha1.ConditionSizingProfileOverridden)
		return
	}

	cond := metav1.Condition{
		Type:               v2alpha1.ConditionSizingProfileOverridden,
		Status:             metav1.ConditionFalse,
		Reason:             v2alpha1.ReasonNoLimitRangeOverride,
		Message:            "no namespace LimitRange imposes a container cpu limit; the Throughput profile's limit-free runner container reaches the kubelet as built",
		ObservedGeneration: rs.Generation,
	}

	listCtx, cancel := context.WithTimeout(ctx, limitRangeReadTimeout)
	defer cancel()

	var ranges corev1.LimitRangeList
	if err := r.List(listCtx, &ranges, client.InNamespace(rs.Namespace)); err != nil {
		// Fail open: an install upgraded before the limitranges grant shipped, or a
		// transient API error, must not raise an alarm about a conflict we could not
		// look for. The next reconcile retries.
		cond.Reason = v2alpha1.ReasonLimitRangesUnreadable
		cond.Message = fmt.Sprintf("could not read namespace LimitRanges, so a cpu-limit override cannot be ruled out: %v", err)
	} else if override := findLimitRangeCPUOverride(ranges.Items); override != nil {
		source := "default"
		if override.fromMax {
			source = "max (which Kubernetes uses as the default limit)"
		}
		cond.Status = metav1.ConditionTrue
		cond.Reason = v2alpha1.ReasonLimitRangeCPULimit
		cond.Message = fmt.Sprintf(
			"LimitRange %q sets a Container cpu %s of %s, which admission re-injects as the runner container's CPU limit; "+
				"the Throughput profile removes that limit to let jobs burst, so jobs are capped and the profile has no effect. "+
				"Drop the cpu default from the LimitRange, or select Binpack (which sets its own limit)",
			override.limitRange, source, override.cpu.String())
	}

	meta.SetStatusCondition(&rs.Status.Conditions, cond)
}

// findLimitRangeCPUOverride returns the first Container-type LimitRange entry that
// supplies a cpu limit to a container declaring none, or nil.
//
// Both `default` and `max` are checked. An entry declaring only a max is defaulted
// to `default: max` by the apiserver on write, so a live object generally carries
// the default already; reading max as well keeps the check correct against an
// object that never went through that defaulting (a manifest read from a fake
// client) and makes the "a max with no default is also a default" rule explicit
// rather than assumed.
func findLimitRangeCPUOverride(ranges []corev1.LimitRange) *limitRangeCPUOverride {
	for i := range ranges {
		lr := &ranges[i]
		for _, item := range lr.Spec.Limits {
			if item.Type != corev1.LimitTypeContainer {
				continue
			}
			if q, ok := item.Default[corev1.ResourceCPU]; ok {
				return &limitRangeCPUOverride{limitRange: lr.Name, cpu: q}
			}
			if q, ok := item.Max[corev1.ResourceCPU]; ok {
				return &limitRangeCPUOverride{limitRange: lr.Name, cpu: q, fromMax: true}
			}
		}
	}
	return nil
}
