package provisioner

import (
	"context"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The integer form of the admission ladder (Q443): the scale-set tier states how many
// jobs GitHub may keep assigned, once per poll, where the classic tier answers a
// boolean per delivered job. These tests pin the arithmetic (QuotaHeadroomPods), the
// delta-to-total conversion (WorkerQuotaCapacity), and the rung composition
// (AdvertiseCapacity) — the last of which must never advertise ABOVE the declared
// ceiling, because that is the property that makes the whole path safe to add.

// halfCPUWorker is a worker pod whose single container requests 500m of cpu,
// matching the shape the quota tests in worker_quota_internal_test.go use.
func halfCPUWorker() *corev1.PodSpec {
	return &corev1.PodSpec{Containers: []corev1.Container{{
		Name:      WorkerContainerName,
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}},
	}}}
}

// podsQuota builds a namespace ResourceQuota constraining the pod COUNT only.
func podsQuota(hard, used string) corev1.ResourceQuota {
	return corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "pods", Namespace: "ns"},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse(hard)},
			Used: corev1.ResourceList{corev1.ResourcePods: resource.MustParse(used)},
		},
	}
}

func TestQuotaHeadroomPods(t *testing.T) {
	tests := []struct {
		name   string
		quotas []corev1.ResourceQuota
		max    int32
		want   int32
	}{
		{
			name:   "cpu headroom divides into whole workers",
			quotas: []corev1.ResourceQuota{*cpuQuota("4", "1")},
			max:    20,
			want:   6, // 3 cpu free / 500m per worker
		},
		{
			name:   "a partial worker's worth of headroom does not count",
			quotas: []corev1.ResourceQuota{*cpuQuota("4", "3250m")},
			max:    20,
			want:   1, // 750m free: one whole worker, not one-and-a-half
		},
		{
			name:   "max caps the answer below what the quota would allow",
			quotas: []corev1.ResourceQuota{*cpuQuota("100", "0")},
			max:    3,
			want:   3,
		},
		{
			name:   "no headroom for even one worker",
			quotas: []corev1.ResourceQuota{*cpuQuota("2", "2")},
			max:    10,
			want:   0,
		},
		{
			name:   "an already over-used quota yields zero, not a negative",
			quotas: []corev1.ResourceQuota{*cpuQuota("2", "3")},
			max:    10,
			want:   0,
		},
		{
			name:   "the tightest of several quotas binds",
			quotas: []corev1.ResourceQuota{*cpuQuota("100", "0"), podsQuota("10", "8")},
			max:    20,
			want:   2,
		},
		{
			name: "a quota constraining nothing in the footprint does not bind",
			quotas: []corev1.ResourceQuota{{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"}, Status: corev1.ResourceQuotaStatus{
				Hard: corev1.ResourceList{corev1.ResourceServices: resource.MustParse("1")},
				Used: corev1.ResourceList{corev1.ResourceServices: resource.MustParse("1")},
			}}},
			max:  5,
			want: 5,
		},
		{
			name:   "a non-positive max short-circuits to zero",
			quotas: []corev1.ResourceQuota{*cpuQuota("100", "0")},
			max:    0,
			want:   0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, QuotaHeadroomPods(halfCPUWorker(), tt.quotas, tt.max))
		})
	}
}

// TestQuotaHeadroomPods_AgreesWithTheBooleanRung is the anti-drift assertion behind the
// decision to search the shared arithmetic instead of dividing: for every headroom
// state, "the quota admits at least one more pod" must mean the same thing to both
// tiers. A divergence here is a tenant getting different answers depending on which
// acquisition protocol it happens to run — the exact class of bug Q443 fixes.
func TestQuotaHeadroomPods_AgreesWithTheBooleanRung(t *testing.T) {
	for _, used := range []string{"0", "1", "1500m", "1900m", "2"} {
		quotas := []corev1.ResourceQuota{*cpuQuota("2", used)}
		exhausted, _ := QuotaHeadroomViolations(WorkerFootprint(halfCPUWorker(), 1), quotas, "")
		headroom := QuotaHeadroomPods(halfCPUWorker(), quotas, 10)
		assert.Equal(t, exhausted, headroom == 0, "used=%s: boolean rung and integer rung disagree", used)
	}
}

// TestWorkerQuotaCapacity_ConvertsHeadroomToTotal pins the delta-to-total conversion:
// X-ScaleSetMaxCapacity bounds the jobs GitHub keeps ASSIGNED, so the set's own
// in-flight pods — already inside the quota's `used` — have to be added back, or every
// running job would silently shrink the set's advertised capacity.
func TestWorkerQuotaCapacity_ConvertsHeadroomToTotal(t *testing.T) {
	ctx := context.Background()
	// 4 cpu hard, 1.5 used by this set's 3 running workers ⇒ 5 more fit, 8 total.
	fc := fake.NewClientBuilder().WithScheme(quotaTestScheme()).
		WithObjects(cpuQuota("4", "1500m")).Build()

	limit, bounded := WorkerQuotaCapacity(ctx, fc, "ns", halfCPUWorker(), 3, 20)
	require.True(t, bounded)
	assert.Equal(t, int32(8), limit)

	// max bounds the search, and the total never exceeds it.
	limit, bounded = WorkerQuotaCapacity(ctx, fc, "ns", halfCPUWorker(), 3, 5)
	require.True(t, bounded)
	assert.Equal(t, int32(5), limit)

	// A negative active count (a caller bug) must not shrink the answer below the
	// headroom itself.
	limit, bounded = WorkerQuotaCapacity(ctx, fc, "ns", halfCPUWorker(), -2, 20)
	require.True(t, bounded)
	assert.Equal(t, int32(5), limit)
}

// TestWorkerQuotaCapacity_FailsOpen covers both fail-open paths. Under-advertising only
// delays jobs, but a quota the AGC cannot see must not be read as zero capacity — that
// would starve a tenant on a lost cache or a missing RBAC rule.
func TestWorkerQuotaCapacity_FailsOpen(t *testing.T) {
	ctx := context.Background()

	t.Run("no quota in the namespace", func(t *testing.T) {
		fc := fake.NewClientBuilder().WithScheme(quotaTestScheme()).Build()
		_, bounded := WorkerQuotaCapacity(ctx, fc, "ns", halfCPUWorker(), 0, 10)
		assert.False(t, bounded, "an unconstrained namespace must impose no bound")
	})

	t.Run("quota unreadable", func(t *testing.T) {
		// The scheme omits core/v1, so the List fails as a lost cache would.
		fc := fake.NewClientBuilder().WithScheme(admissionTestScheme()).Build()
		_, bounded := WorkerQuotaCapacity(ctx, fc, "ns", halfCPUWorker(), 0, 10)
		assert.False(t, bounded, "an unreadable quota must impose no bound")
	})
}

// capacityTarget is a Target stub for the rung-composition tests: it answers the
// cheap capacity reads and nothing else.
type capacityTarget struct {
	ceiling         int32
	bounded         bool
	quotaLimit      int32
	quotaBounded    bool
	declinedLimit   int32
	declinedBounded bool
	// maxSeen records the cap AdvertiseCapacity passed down, so the test can assert
	// the quota search is bounded by the declared ceiling rather than run open-ended.
	maxSeen int32
	// declinedMaxSeen is the same for the capacity rung, which runs after quota and
	// must therefore be capped by whatever quota already left rather than the ceiling.
	declinedMaxSeen int32
	// scaleUp drives the Q717 rate rung; nil (the default) leaves it unbound, which
	// is what every pre-Q717 case in this file asserts against.
	scaleUp *ScaleUpConfig
}

func (c *capacityTarget) Key() client.ObjectKey             { return client.ObjectKey{Namespace: "ns", Name: "s"} }
func (c *capacityTarget) OwnerRef() metav1.OwnerReference   { return metav1.OwnerReference{} }
func (c *capacityTarget) PodOwnerLabels() map[string]string { return nil }
func (c *capacityTarget) Ceiling(context.Context) (int32, bool) {
	return c.ceiling, c.bounded
}
func (c *capacityTarget) QuotaExhausted(context.Context) (bool, string) { return false, "" }
func (c *capacityTarget) ScaleUpLimit(context.Context) *ScaleUpConfig   { return c.scaleUp }
func (c *capacityTarget) QuotaCapacity(_ context.Context, max int32) (int32, bool) {
	c.maxSeen = max
	return c.quotaLimit, c.quotaBounded
}
func (c *capacityTarget) CapacityDeclined(context.Context) (bool, string) {
	return c.declinedBounded, "stub"
}
func (c *capacityTarget) DeclinedCapacity(_ context.Context, max int32) (int32, bool) {
	c.declinedMaxSeen = max
	return c.declinedLimit, c.declinedBounded
}
func (c *capacityTarget) RecordEvent(_, _, _, _ string) {}
func (c *capacityTarget) Resolve(context.Context) (*ResolvedSpec, error) {
	return &ResolvedSpec{}, nil
}

func TestAdvertiseCapacity(t *testing.T) {
	const unboundedDefault = int32(10)
	tests := []struct {
		name         string
		target       capacityTarget
		disableQuota bool
		wantTotal    int32
		wantWithheld map[string]int32
	}{
		{
			name:         "quota below the ceiling binds",
			target:       capacityTarget{ceiling: 8, bounded: true, quotaLimit: 3, quotaBounded: true},
			wantTotal:    3,
			wantWithheld: map[string]int32{runnercore.AdmitReasonQuota: 5, runnercore.AdmitReasonCapacity: 0, runnercore.AdmitReasonScaleUp: 0},
		},
		{
			name:         "quota above the ceiling never raises the advertisement",
			target:       capacityTarget{ceiling: 8, bounded: true, quotaLimit: 50, quotaBounded: true},
			wantTotal:    8,
			wantWithheld: map[string]int32{runnercore.AdmitReasonQuota: 0, runnercore.AdmitReasonCapacity: 0, runnercore.AdmitReasonScaleUp: 0},
		},
		{
			name:         "an unbounded quota rung leaves the ceiling alone",
			target:       capacityTarget{ceiling: 8, bounded: true},
			wantTotal:    8,
			wantWithheld: map[string]int32{runnercore.AdmitReasonQuota: 0, runnercore.AdmitReasonCapacity: 0, runnercore.AdmitReasonScaleUp: 0},
		},
		{
			name:         "no declared ceiling falls back to the caller's default",
			target:       capacityTarget{quotaLimit: 4, quotaBounded: true},
			wantTotal:    4,
			wantWithheld: map[string]int32{runnercore.AdmitReasonQuota: 6, runnercore.AdmitReasonCapacity: 0, runnercore.AdmitReasonScaleUp: 0},
		},
		{
			name:         "quota with no headroom advertises zero",
			target:       capacityTarget{ceiling: 8, bounded: true, quotaBounded: true},
			wantTotal:    0,
			wantWithheld: map[string]int32{runnercore.AdmitReasonQuota: 8, runnercore.AdmitReasonCapacity: 0, runnercore.AdmitReasonScaleUp: 0},
		},
		{
			// The quota opt-out is an AGC-wide kill switch, so that rung is not
			// evaluated and publishes nothing. The capacity rung is per-owner spec, so
			// it is still evaluated and still publishes its explicit zero.
			name:         "the quota opt-out skips the quota rung entirely",
			target:       capacityTarget{ceiling: 8, bounded: true, quotaLimit: 3, quotaBounded: true},
			disableQuota: true,
			wantTotal:    8,
			wantWithheld: map[string]int32{runnercore.AdmitReasonCapacity: 0, runnercore.AdmitReasonScaleUp: 0},
		},
		{
			// Q405: a declining gate bounds the total at the set's own in-flight pods.
			name:         "a declining capacity gate binds below the ceiling",
			target:       capacityTarget{ceiling: 8, bounded: true, declinedLimit: 2, declinedBounded: true},
			wantTotal:    2,
			wantWithheld: map[string]int32{runnercore.AdmitReasonQuota: 0, runnercore.AdmitReasonCapacity: 6, runnercore.AdmitReasonScaleUp: 0},
		},
		{
			// The trickle floor: nothing in flight and nothing placeable means GitHub
			// is offered nothing, which is the whole point of the integer form.
			name:         "a declining gate with nothing in flight advertises zero",
			target:       capacityTarget{ceiling: 8, bounded: true, declinedBounded: true},
			wantTotal:    0,
			wantWithheld: map[string]int32{runnercore.AdmitReasonQuota: 0, runnercore.AdmitReasonCapacity: 8, runnercore.AdmitReasonScaleUp: 0},
		},
		{
			// Rungs compose as a min(): each withheld entry is that rung's own marginal
			// contribution, and the entries sum to Ceiling - Total.
			name:         "quota and capacity both bind, each withholding its own margin",
			target:       capacityTarget{ceiling: 10, bounded: true, quotaLimit: 6, quotaBounded: true, declinedLimit: 2, declinedBounded: true},
			wantTotal:    2,
			wantWithheld: map[string]int32{runnercore.AdmitReasonQuota: 4, runnercore.AdmitReasonCapacity: 4, runnercore.AdmitReasonScaleUp: 0},
		},
		{
			// A capacity rung that would RAISE the total must not: quota already said 3.
			name:         "the capacity rung never raises what quota already lowered",
			target:       capacityTarget{ceiling: 10, bounded: true, quotaLimit: 3, quotaBounded: true, declinedLimit: 9, declinedBounded: true},
			wantTotal:    3,
			wantWithheld: map[string]int32{runnercore.AdmitReasonQuota: 7, runnercore.AdmitReasonCapacity: 0, runnercore.AdmitReasonScaleUp: 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewProvisioner(nil, nil, nil)
			p.DisableQuotaAdmission = tt.disableQuota
			target := tt.target

			adv := p.AdvertiseCapacity(&target, unboundedDefault)(context.Background())

			assert.Equal(t, tt.wantTotal, adv.Total)
			assert.Equal(t, tt.wantWithheld, adv.Withheld)
			assert.LessOrEqual(t, adv.Total, adv.Ceiling, "the advertisement must never exceed the declared ceiling")
			if !tt.disableQuota {
				assert.Equal(t, adv.Ceiling, target.maxSeen, "the quota search must be bounded by the ceiling")
			}
			// The capacity rung runs after quota, so its cap is what quota left — not
			// the ceiling. Passing the ceiling would let it re-open slots quota closed.
			assert.LessOrEqual(t, target.declinedMaxSeen, adv.Ceiling,
				"the capacity rung must be capped by the running total, never above the ceiling")
		})
	}
}

// TestAdvertiseCapacity_RereadsEveryPoll pins the Q117 property on this tier: the
// function is built once when the listener starts, so a quota that frees up (or fills)
// must be reflected on the next poll without an AGC restart. Recovery latency is one
// poll interval — the granularity this tier trades for never spending a claim.
func TestAdvertiseCapacity_RereadsEveryPoll(t *testing.T) {
	target := &capacityTarget{ceiling: 6, bounded: true, quotaBounded: true, quotaLimit: 0}
	p := NewProvisioner(nil, nil, nil)
	advertise := p.AdvertiseCapacity(target, 10)

	assert.Equal(t, int32(0), advertise(context.Background()).Total)

	target.quotaLimit = 6
	assert.Equal(t, int32(6), advertise(context.Background()).Total, "freed headroom must reopen assignment on the next poll")
}

// activeWorkerPods returns n Running worker pods for the capacityTarget's namespace,
// so the rate rung's delta-to-total conversion has real pods to count.
func activeWorkerPods(n int) []client.Object {
	objs := make([]client.Object, 0, n)
	for i := range n {
		objs = append(objs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "w" + string(rune('a'+i))},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		})
	}
	return objs
}

// TestAdvertiseCapacity_ScaleUpRungConvertsTokensToTotal is the Q717 rung's whole
// point. Free tokens are a DELTA — how many more pods may be created now — while the
// advertisement is a TOTAL bounding totalAssignedJobs, so the rung has to add the
// set's own in-flight workers to convert between them.
//
// Advertising the delta directly is the failure this pins against: with burst 10 and
// a ceiling of 100 the set would advertise 10, GitHub would assign 10, and the bucket
// would never hold more than 10 tokens again — so the advertisement would sit at 10
// forever and the rate limit would have silently become a lower concurrency ceiling.
// The "bucket drained, ten already running" case below is the one that separates them:
// the delta reading advertises 0, the conversion advertises 10.
func TestAdvertiseCapacity_ScaleUpRungConvertsTokensToTotal(t *testing.T) {
	const burst = int32(10)

	tests := []struct {
		name         string
		activePods   int
		drainTokens  int
		wantTotal    int32
		wantWithheld int32
	}{
		{
			name:         "idle set with a full bucket advertises its burst",
			wantTotal:    burst,
			wantWithheld: 100 - burst,
		},
		{
			name:         "bucket drained with the burst already running holds the total, not zero",
			activePods:   int(burst),
			drainTokens:  int(burst),
			wantTotal:    burst,
			wantWithheld: 100 - burst,
		},
		{
			name:         "a refilled bucket climbs above the burst",
			activePods:   int(burst),
			wantTotal:    2 * burst,
			wantWithheld: 100 - 2*burst,
		},
		{
			name:         "the rung never raises the advertisement above the ceiling",
			activePods:   100,
			wantTotal:    100,
			wantWithheld: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
				WithObjects(activeWorkerPods(tt.activePods)...).Build()
			p := NewProvisioner(fc, nil, nil)
			clock := newFakeClock()
			p.scaleUp = scaleUpLimiter{now: clock.now}

			target := &capacityTarget{
				ceiling: 100, bounded: true,
				scaleUp: &ScaleUpConfig{MaxPerSecond: 1, Burst: burst},
			}
			key := target.Key().String()
			for range tt.drainTokens {
				require.True(t, p.scaleUp.allow(key, target.scaleUp), "draining the burst must succeed")
			}

			adv := p.AdvertiseCapacity(target, 10)(context.Background())

			assert.Equal(t, tt.wantTotal, adv.Total)
			assert.Equal(t, tt.wantWithheld, adv.Withheld[runnercore.AdmitReasonScaleUp])
			assert.LessOrEqual(t, adv.Total, adv.Ceiling, "the advertisement must never exceed the declared ceiling")
		})
	}
}

// TestAdvertiseCapacity_ScaleUpRungObservesWithoutTaking pins the split between the
// two tiers' spend points: this tier states a capacity per poll and takes its token
// later, at pod creation, so polling must not drain the bucket. A rung that took a
// token per poll would throttle a set that had been offered nothing.
func TestAdvertiseCapacity_ScaleUpRungObservesWithoutTaking(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	p := NewProvisioner(fc, nil, nil)
	p.scaleUp = scaleUpLimiter{now: newFakeClock().now}

	target := &capacityTarget{ceiling: 100, bounded: true, scaleUp: &ScaleUpConfig{MaxPerSecond: 1, Burst: 5}}
	advertise := p.AdvertiseCapacity(target, 10)

	for i := range 20 {
		assert.Equal(t, int32(5), advertise(context.Background()).Total,
			"poll %d must read the same full bucket: advertising does not spend a token", i)
	}
}

// TestAdvertiseCapacity_ScaleUpRungAlwaysEvaluated pins the explicit zero. Opting out
// of spec.scaleUp is a per-set spec state the Target answers, not a rung the AGC
// skips — so like the capacity gate it publishes a zero rather than leaving the
// series frozen at its last non-zero reading, which a reader cannot tell from a rung
// that was never evaluated.
func TestAdvertiseCapacity_ScaleUpRungAlwaysEvaluated(t *testing.T) {
	p := NewProvisioner(nil, nil, nil)
	target := &capacityTarget{ceiling: 8, bounded: true} // scaleUp nil: opted out

	adv := p.AdvertiseCapacity(target, 10)(context.Background())

	require.Contains(t, adv.Withheld, runnercore.AdmitReasonScaleUp)
	assert.Equal(t, int32(0), adv.Withheld[runnercore.AdmitReasonScaleUp])
	assert.Equal(t, int32(8), adv.Total, "an opted-out set keeps whatever the earlier rungs left")
}
