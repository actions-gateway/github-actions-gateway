package provisioner_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestMaxWorkerLifetimeOrDefault covers the defaulting rule for the Q438 worker
// lifetime cap: omitted means the 12-hour default, an explicit value is honoured,
// and an explicit "0s" is an opt-out rather than something to re-default. A
// negative value (which the CRD's XValidation rejects) must also resolve to "no
// cap" — stamping a negative activeDeadlineSeconds would make the apiserver
// reject every worker pod create, turning a bad field into a total outage.
func TestMaxWorkerLifetimeOrDefault(t *testing.T) {
	tests := []struct {
		name string
		in   *metav1.Duration
		want time.Duration
	}{
		{"omitted defaults to 12h", nil, 12 * time.Hour},
		{"explicit value honoured", &metav1.Duration{Duration: 2 * time.Hour}, 2 * time.Hour},
		{"zero disables the cap", &metav1.Duration{Duration: 0}, 0},
		{"negative disables rather than stamping an invalid deadline",
			&metav1.Duration{Duration: -1 * time.Hour}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, provisioner.MaxWorkerLifetimeOrDefault(tt.in))
		})
	}
}

// TestMaxWorkerLifetimeDefaultIsTwiceGitHubJobTimeout pins the default to the
// reasoning that chose it rather than to a bare number: 12h is 2x GitHub's own
// 360-minute default job timeout, which is the anchor used precisely because the
// job's real timeout never reaches the AGC (no timeout field exists on the
// scale-set JobAssigned message). Changing the constant should require changing
// this test, and changing this test should require re-reading the rationale on
// maxWorkerLifetime in design/03-api-contracts.md.
func TestMaxWorkerLifetimeDefaultIsTwiceGitHubJobTimeout(t *testing.T) {
	const gitHubDefaultJobTimeout = 360 * time.Minute
	assert.Equal(t, 2*gitHubDefaultJobTimeout, provisioner.DefaultMaxWorkerLifetime)
	// And it stays well under GitHub's own 5-day ceiling for a self-hosted job,
	// so the cap strictly tightens a bound that already exists.
	assert.Less(t, provisioner.DefaultMaxWorkerLifetime, 5*24*time.Hour)
}

// TestBuildPod_StampsDefaultWorkerLifetime verifies the cap reaches the worker
// pod as activeDeadlineSeconds. This is the whole mechanism: the kubelet enforces
// that field, so it bounds a worker orphaned while the AGC is down — the one
// orphan class no AGC-side reconciliation can reclaim (Q438, residual of Q435).
func TestBuildPod_StampsDefaultWorkerLifetime(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	pod := runAndGetPod(ctx, t, p, fc, newRG("mygroup", "team-a"), "plan-lifetime", "team-a")

	require.NotNil(t, pod.Spec.ActiveDeadlineSeconds,
		"worker pod must carry a provision-time lifetime cap by default")
	assert.Equal(t, int64(12*60*60), *pod.Spec.ActiveDeadlineSeconds)
}

// TestBuildPod_WorkerLifetimeGroupOverride verifies the per-RunnerGroup override
// reaches the pod, which is what an operator running legitimately long jobs uses
// instead of being killed by a default they did not choose.
func TestBuildPod_WorkerLifetimeGroupOverride(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")
	rg.Spec.MaxWorkerLifetime = &metav1.Duration{Duration: 36 * time.Hour}

	pod := runAndGetPod(ctx, t, p, fc, rg, "plan-lifetime-override", "team-a")

	require.NotNil(t, pod.Spec.ActiveDeadlineSeconds)
	assert.Equal(t, int64(36*60*60), *pod.Spec.ActiveDeadlineSeconds)
}

// TestBuildPod_WorkerLifetimeDisabled verifies "0s" is a real opt-out: no
// deadline is stamped at all, restoring the pre-Q438 behaviour for an operator
// who accepts the orphan risk rather than any job being killed.
func TestBuildPod_WorkerLifetimeDisabled(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")
	rg.Spec.MaxWorkerLifetime = &metav1.Duration{Duration: 0}

	pod := runAndGetPod(ctx, t, p, fc, rg, "plan-lifetime-off", "team-a")

	assert.Nil(t, pod.Spec.ActiveDeadlineSeconds,
		`maxWorkerLifetime "0s" must stamp no deadline at all`)
}

// TestBuildPod_ExplicitPodTemplateDeadlinePreserved verifies the gap-fill rule:
// an activeDeadlineSeconds the tenant set on its own podTemplate is never
// overwritten by the default. Overwriting it would silently shorten a deadline
// the operator chose deliberately, which is the same failure the cap exists to
// avoid — just with GAG as the cause.
func TestBuildPod_ExplicitPodTemplateDeadlinePreserved(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")
	explicit := int64(99)
	rg.Spec.PodTemplate.Spec.ActiveDeadlineSeconds = &explicit

	pod := runAndGetPod(ctx, t, p, fc, rg, "plan-lifetime-explicit", "team-a")

	require.NotNil(t, pod.Spec.ActiveDeadlineSeconds)
	assert.Equal(t, int64(99), *pod.Spec.ActiveDeadlineSeconds,
		"an explicit podTemplate activeDeadlineSeconds must win over the default")
}
