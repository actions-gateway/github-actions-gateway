package provisioner

import (
	"context"
	"sync"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// admissionTestScheme registers the RunnerGroup API types for the fake client.
func admissionTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	return s
}

func TestAdmissionGate_ReserveAndRelease(t *testing.T) {
	var g admissionGate
	const key = "ns/group"

	// Reserve up to the ceiling of 2.
	r1, ok := g.admit(key, 2, true)
	require.True(t, ok)
	r2, ok := g.admit(key, 2, true)
	require.True(t, ok)
	assert.Equal(t, int32(2), g.reservedCount(key))

	// The gate is full: the third admit is rejected.
	_, ok = g.admit(key, 2, true)
	assert.False(t, ok, "admit must be rejected once the ceiling is reached")

	// Releasing one slot lets exactly one more in.
	r1()
	assert.Equal(t, int32(1), g.reservedCount(key))
	r3, ok := g.admit(key, 2, true)
	require.True(t, ok)

	// Releasing all slots prunes the map entry.
	r2()
	r3()
	assert.Equal(t, int32(0), g.reservedCount(key))
}

func TestAdmissionGate_ReleaseIsIdempotent(t *testing.T) {
	var g admissionGate
	const key = "ns/group"

	release, ok := g.admit(key, 1, true)
	require.True(t, ok)
	assert.Equal(t, int32(1), g.reservedCount(key))

	release()
	release() // second call must be a no-op, not drive the count negative
	assert.Equal(t, int32(0), g.reservedCount(key))

	// The single freed slot is available again, not double-freed.
	_, ok = g.admit(key, 1, true)
	require.True(t, ok)
	_, ok = g.admit(key, 1, true)
	assert.False(t, ok, "only one slot should exist after an idempotent release")
}

func TestAdmissionGate_Unbounded(t *testing.T) {
	var g admissionGate
	const key = "ns/group"

	// A group with no ceiling admits unconditionally without tracking state.
	for i := 0; i < 100; i++ {
		release, ok := g.admit(key, 0, false)
		require.True(t, ok)
		require.NotNil(t, release)
	}
	assert.Equal(t, int32(0), g.reservedCount(key), "unbounded admits must not touch the counter")
}

// TestAdmissionGate_NoDoubleAdmitUnderBurst is the core correctness property: N
// concurrent admits against a ceiling of K let through exactly K, never more.
func TestAdmissionGate_NoDoubleAdmitUnderBurst(t *testing.T) {
	var g admissionGate
	const key = "ns/group"
	const ceiling = 5
	const burst = 200

	var admitted int32
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := g.admit(key, ceiling, true); ok {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(ceiling), admitted, "burst of %d admits must let through exactly the ceiling", burst)
	assert.Equal(t, int32(ceiling), g.reservedCount(key))
}

func TestAdmissionGate_KeysAreIndependent(t *testing.T) {
	var g admissionGate

	_, ok := g.admit("ns/a", 1, true)
	require.True(t, ok)
	// Group b has its own budget, unaffected by a being full.
	_, ok = g.admit("ns/b", 1, true)
	assert.True(t, ok)
	// Group a is still full.
	_, ok = g.admit("ns/a", 1, true)
	assert.False(t, ok)
}

func TestAdmissionCeiling(t *testing.T) {
	tests := []struct {
		name        string
		spec        v1alpha1.RunnerGroupSpec
		wantLimit   int32
		wantBounded bool
	}{
		{
			name:        "maxWorkers",
			spec:        v1alpha1.RunnerGroupSpec{MaxWorkers: ptr.To(int32(7))},
			wantLimit:   7,
			wantBounded: true,
		},
		{
			name: "priorityTiers uses the maximum threshold",
			spec: v1alpha1.RunnerGroupSpec{
				PriorityTiers: []v1alpha1.PriorityTier{
					{Threshold: 3, PriorityClassName: "low"},
					{Threshold: 10, PriorityClassName: "high"},
				},
			},
			wantLimit:   10,
			wantBounded: true,
		},
		{
			name: "priorityTiers take precedence over maxWorkers",
			spec: v1alpha1.RunnerGroupSpec{
				MaxWorkers:    ptr.To(int32(2)),
				PriorityTiers: []v1alpha1.PriorityTier{{Threshold: 5, PriorityClassName: "x"}},
			},
			wantLimit:   5,
			wantBounded: true,
		},
		{
			name:        "unbounded when neither is set",
			spec:        v1alpha1.RunnerGroupSpec{},
			wantLimit:   0,
			wantBounded: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rg := &v1alpha1.RunnerGroup{Spec: tt.spec}
			limit, bounded := WorkerCeiling(rg)
			assert.Equal(t, tt.wantBounded, bounded)
			assert.Equal(t, tt.wantLimit, limit)
		})
	}
}

// TestAdmitFor_ReadsFreshSpec verifies the AdmitFunc honours a maxWorkers edit
// made after the listener-start snapshot was captured (Q117), reading the
// current spec from the cached client on each call.
func TestAdmitFor_ReadsFreshSpec(t *testing.T) {
	rg := &v1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
		Spec:       v1alpha1.RunnerGroupSpec{MaxWorkers: ptr.To(int32(1))},
	}
	fc := fake.NewClientBuilder().WithScheme(admissionTestScheme()).WithObjects(rg).Build()
	p := NewProvisioner(fc, nil, nil)

	// Snapshot captured maxWorkers=1.
	admit := p.AdmitFor(rg)
	r1, ok, _ := admit(context.Background())
	require.True(t, ok)
	_, ok, reason := admit(context.Background())
	require.False(t, ok, "ceiling of 1 should reject the second admit")
	assert.Equal(t, runnercore.AdmitReasonCeiling, reason)

	// Operator raises maxWorkers to 2; the gate must pick it up without a restart.
	var current v1alpha1.RunnerGroup
	require.NoError(t, fc.Get(context.Background(), client.ObjectKeyFromObject(rg), &current))
	current.Spec.MaxWorkers = ptr.To(int32(2))
	require.NoError(t, fc.Update(context.Background(), &current))

	r2, ok, _ := admit(context.Background())
	assert.True(t, ok, "raised ceiling should admit a second slot")

	r1(runnercore.AdmitProvisioned)
	r2(runnercore.AdmitProvisioned)
}

// TestAdmit_AbortedOutcomeRefundsTheScaleUpToken covers the whole ladder spend, not
// just the bucket: an admitted delivery that returns without asking for a worker pod
// must hand back both the ceiling reservation and the scale-up token, while one that
// provisions hands back only the reservation (Q972).
func TestAdmit_AbortedOutcomeRefundsTheScaleUpToken(t *testing.T) {
	newGate := func() (runnercore.AdmitFunc, *Provisioner) {
		rg := &v1alpha1.RunnerGroup{
			ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
			Spec: v1alpha1.RunnerGroupSpec{
				MaxWorkers: ptr.To(int32(10)),
				ScaleUp:    &v1alpha1.ScaleUpRateLimit{MaxPerSecond: 1, Burst: ptr.To(int32(2))},
			},
		}
		fc := fake.NewClientBuilder().WithScheme(admissionTestScheme()).WithObjects(rg).Build()
		p := NewProvisioner(fc, nil, nil)
		return p.AdmitFor(rg), p
	}

	t.Run("aborted delivery returns its token", func(t *testing.T) {
		admit, p := newGate()
		ctx := context.Background()

		// Spend the whole burst, then abort both deliveries — the fan-out shape, where
		// every sibling but one returns without provisioning.
		r1, ok, _ := admit(ctx)
		require.True(t, ok)
		r2, ok, _ := admit(ctx)
		require.True(t, ok)
		_, ok, reason := admit(ctx)
		require.False(t, ok, "burst of 2 is spent")
		assert.Equal(t, runnercore.AdmitReasonScaleUp, reason)

		r1(runnercore.AdmitAborted)
		r2(runnercore.AdmitAborted)

		for i := 0; i < 2; i++ {
			_, ok, reason = admit(ctx)
			assert.Truef(t, ok, "admit %d after two refunds was refused with %q", i, reason)
		}
		assert.Equal(t, int32(2), p.admission.reservedCount("ns/g"), "the two refunded slots should be held by the new admits")
	})

	t.Run("provisioned delivery keeps its token", func(t *testing.T) {
		admit, _ := newGate()
		ctx := context.Background()

		r1, ok, _ := admit(ctx)
		require.True(t, ok)
		r2, ok, _ := admit(ctx)
		require.True(t, ok)

		r1(runnercore.AdmitProvisioned)
		r2(runnercore.AdmitProvisioned)

		_, ok, reason := admit(ctx)
		require.False(t, ok, "a token spent on a pod must not come back")
		assert.Equal(t, runnercore.AdmitReasonScaleUp, reason)
	})

	t.Run("release is idempotent across outcomes", func(t *testing.T) {
		admit, p := newGate()
		ctx := context.Background()

		r1, ok, _ := admit(ctx)
		require.True(t, ok)
		r1(runnercore.AdmitAborted)
		// A second call must add nothing: not a slot, and not a token.
		r1(runnercore.AdmitAborted)
		assert.Equal(t, int32(0), p.admission.reservedCount("ns/g"))

		for i := 0; i < 2; i++ {
			_, ok, _ = admit(ctx)
			require.Truef(t, ok, "admit %d should pass on the restored burst", i)
		}
		_, ok, _ = admit(ctx)
		assert.False(t, ok, "a repeated release refunded a second token")
	})
}
