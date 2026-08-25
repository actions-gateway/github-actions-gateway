package provisioner

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// scaleUpTestMetrics builds the counter the Q223 scale-up limiter records into,
// unregistered so each test observes only its own increments. runnercore.NewMetrics
// registers with the process-global controller-runtime registry, whose counters
// accumulate across repeats of the same test (-count>1).
func scaleUpTestMetrics() *runnercore.Metrics {
	return &runnercore.Metrics{
		ScaleUpThrottled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_scaleup_throttled_total",
		}, []string{"namespace", "runner_group"}),
	}
}

// stubTarget is a minimal Target for driving the provision path from an internal
// test, where the scaleUpLimiter's clock/sleep can be injected.
type stubTarget struct {
	key  client.ObjectKey
	spec *ResolvedSpec
	// quotaExhausted/quotaDetail drive the admission gate's quota rung (#784).
	quotaExhausted bool
	quotaDetail    string
	// capacityDeclined/capacityDetail drive the placeability rung (Q405).
	capacityDeclined bool
	capacityDetail   string
	// events records every RecordEvent reason, so a test can assert an owner-visible
	// incident was surfaced and not merely logged.
	events []string
}

func (s *stubTarget) Key() client.ObjectKey { return s.key }
func (s *stubTarget) OwnerRef() metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: "actions-gateway.com/v2alpha1", Kind: "RunnerSet", Name: s.key.Name, UID: types.UID("uid-" + s.key.Name)}
}
func (s *stubTarget) PodOwnerLabels() map[string]string {
	return map[string]string{LabelRunnerSet: s.key.Name}
}
func (s *stubTarget) Ceiling(context.Context) (int32, bool) { return 0, false }
func (s *stubTarget) QuotaExhausted(context.Context) (bool, string) {
	return s.quotaExhausted, s.quotaDetail
}
func (s *stubTarget) QuotaCapacity(context.Context, int32) (int32, bool) { return 0, false }
func (s *stubTarget) CapacityDeclined(context.Context) (bool, string) {
	return s.capacityDeclined, s.capacityDetail
}
func (s *stubTarget) DeclinedCapacity(context.Context, int32) (int32, bool) { return 0, false }
func (s *stubTarget) ScaleUpLimit(context.Context) *ScaleUpConfig {
	if s.spec == nil {
		return nil
	}
	return s.spec.ScaleUp
}
func (s *stubTarget) RecordEvent(_, reason, _, _ string) { s.events = append(s.events, reason) }
func (s *stubTarget) Resolve(context.Context) (*ResolvedSpec, error) {
	return s.spec, nil
}

// TestProvisioner_ScaleUpRateLimitDelaysPodCreation is the behaviour test the Q223
// plan calls for: with a per-RunnerGroup token bucket of burst=1, the first worker
// pod is created instantly and the second waits one refill interval before its pod
// is created — verifying the limiter actually gates pod CREATION at the configured
// rate (not merely that the code runs), while both pods still get made.
func TestProvisioner_ScaleUpRateLimitDelaysPodCreation(t *testing.T) {
	ctx := context.Background()
	scheme := clientgoscheme.Scheme
	fc := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Pod{}).Build()

	metrics := scaleUpTestMetrics()
	p := NewProvisioner(fc, metrics, nil)

	// Inject a deterministic clock + a sleep stub that records each throttle delay and
	// advances the clock (models time passing while the job waited for a token).
	clock := newFakeClock()
	var delays []time.Duration
	p.scaleUp = scaleUpLimiter{
		now: clock.now,
		sleep: func(_ context.Context, d time.Duration) error {
			delays = append(delays, d)
			clock.advance(d)
			return nil
		},
	}

	target := &stubTarget{
		key: client.ObjectKey{Namespace: "team-a", Name: "gpu"},
		spec: &ResolvedSpec{
			WorkerImage: "runner:test",
			ScaleUp:     &ScaleUpConfig{MaxPerSecond: 1, Burst: 1}, // one now, then 1/s
		},
	}
	const jit = "eyJydW5uZXIiOnt9fQ=="

	// First creation drains the single burst token — no throttle.
	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{JobID: "job-1", JITConfig: jit}))
	assert.Empty(t, delays, "the first pod (within burst) must not be throttled")

	// Second creation in the same instant must wait ~1s for a token.
	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{JobID: "job-2", JITConfig: jit}))
	require.Len(t, delays, 1, "the second pod must be throttled exactly once")
	assert.InDelta(t, float64(time.Second), float64(delays[0]), float64(150*time.Millisecond),
		"the throttled wait should be ~1s at 1 pod/s")

	// Both pods are created despite the ramp — throttling delays, it does not drop.
	var pods corev1.PodList
	require.NoError(t, fc.List(ctx, &pods, client.InNamespace("team-a")))
	assert.Len(t, pods.Items, 2, "both worker pods must exist after the ramp")

	// The throttle is observable on the metric.
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.ScaleUpThrottled.WithLabelValues("team-a", "gpu")),
		"a throttled creation increments actions_gateway_worker_scaleup_throttled_total")
}

// TestProvisioner_ScaleUpDisabledCreatesImmediately pins the default-off contract:
// with no scaleUp config, pod creation is never delayed and the throttle metric
// stays zero — an opted-out RunnerGroup pays nothing.
func TestProvisioner_ScaleUpDisabledCreatesImmediately(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithStatusSubresource(&corev1.Pod{}).Build()
	metrics := scaleUpTestMetrics()
	p := NewProvisioner(fc, metrics, nil)

	var slept bool
	p.scaleUp = scaleUpLimiter{
		now: newFakeClock().now,
		sleep: func(context.Context, time.Duration) error {
			slept = true
			return nil
		},
	}

	target := &stubTarget{
		key:  client.ObjectKey{Namespace: "team-a", Name: "cpu"},
		spec: &ResolvedSpec{WorkerImage: "runner:test"}, // ScaleUp nil = off
	}
	for _, job := range []string{"j1", "j2", "j3"} {
		require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{JobID: job, JITConfig: "eyJ4IjoxfQ=="}))
	}
	assert.False(t, slept, "default-off scaleUp must never throttle")
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.ScaleUpThrottled.WithLabelValues("team-a", "cpu")))
}

// TestProvisioner_ClassicTierChargesTheBucketOnceAtAdmit pins the Q717 split. The
// classic tier takes its token in Admit's rate rung, before the job is claimed, and
// the provisioning path no longer waits on the bucket at all — so a job that reaches
// provision never blocks there with its GitHub lock held, and never pays twice.
//
// The double-charge is what a `slept` assertion alone would miss: a second token is
// available here, so a leftover wait would take it without sleeping. The third admit
// is the discriminator — it succeeds only if provision left the bucket alone.
func TestProvisioner_ClassicTierChargesTheBucketOnceAtAdmit(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithStatusSubresource(&corev1.Pod{}).Build()
	p := NewProvisioner(fc, scaleUpTestMetrics(), nil)

	var slept bool
	p.scaleUp = scaleUpLimiter{
		now: newFakeClock().now,
		sleep: func(context.Context, time.Duration) error {
			slept = true
			return nil
		},
	}

	target := &stubTarget{
		key: client.ObjectKey{Namespace: "team-a", Name: "cpu"},
		spec: &ResolvedSpec{
			WorkerImage: "runner:test",
			ScaleUp:     &ScaleUpConfig{MaxPerSecond: 1, Burst: 2},
		},
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	p.Waiter = &cancellingWaiter{cancel: cancel, cause: context.Canceled}

	// One token spent at admit, one left.
	_, ok, _ := p.Admit(target)(ctx)
	require.True(t, ok, "the first job is within the burst")

	_, err := p.provision(ctx, target, "plan-1", []byte("{}"), "")
	require.Error(t, err, "the cancelling waiter ends the job after the pod is created")
	assert.False(t, slept, "the classic provisioning path must not wait on the token bucket")

	// The surviving token proves provision did not charge a second one.
	_, ok, reason := p.Admit(target)(context.Background())
	assert.True(t, ok, "provision must leave the second burst token for the next job")
	assert.Empty(t, reason)
}
