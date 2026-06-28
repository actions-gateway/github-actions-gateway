package provisioner_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// These literals mirror the unexported provisioner constants
// (wrapperVolumeName / wrapperMountDir / wrapperInitName) — kept in sync by hand
// because the injection tests live in the external test package.
const (
	wrapperVol      = "gag-wrapper"
	wrapperMount    = "/opt/actions-gateway"
	wrapperPath     = "/opt/actions-gateway/wrapper"
	wrapperInitName = "gag-wrapper-install"
	testWrapperRef  = "ghcr.io/actions-gateway/wrapper@sha256:" +
		"0000000000000000000000000000000000000000000000000000000000000000"
)

func runnerOf(t *testing.T, pod *corev1.Pod) *corev1.Container {
	t.Helper()
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "runner" {
			return &pod.Spec.Containers[i]
		}
	}
	t.Fatalf("no runner container in pod %s", pod.Name)
	return nil
}

func volumeOf(pod *corev1.Pod, name string) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

func TestProvisioner_WrapperInjection_ImageVolume(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	p.WrapperImage = testWrapperRef
	p.UseImageVolume = true

	rg := newRG("mygroup", "team-a")
	done := make(chan error, 1)
	go func() { done <- p.HandlerFor(rg)(ctx, "", "plan-iv", stubPayload(1), "") }()
	require.Eventually(t, func() bool { return findPod(ctx, t, fc, "team-a") != nil }, 2*time.Second, 5*time.Millisecond)
	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)

	// Image volume references the wrapper image; no init container.
	vol := volumeOf(pod, wrapperVol)
	require.NotNil(t, vol, "expected %s volume", wrapperVol)
	require.NotNil(t, vol.Image, "expected an OCI image volume source")
	assert.Equal(t, testWrapperRef, vol.Image.Reference)
	assert.Empty(t, pod.Spec.InitContainers, "image-volume path must not add an init container")

	// Runner command is overridden to the injected wrapper, mounted read-only.
	runner := runnerOf(t, pod)
	assert.Equal(t, []string{wrapperPath}, runner.Command)
	assert.Nil(t, runner.Args)
	var mounted bool
	for _, m := range runner.VolumeMounts {
		if m.Name == wrapperVol {
			mounted = true
			assert.Equal(t, wrapperMount, m.MountPath)
			assert.True(t, m.ReadOnly)
		}
	}
	assert.True(t, mounted, "runner must mount the wrapper volume")

	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

func TestProvisioner_WrapperInjection_InitContainer(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	p.WrapperImage = testWrapperRef
	p.UseImageVolume = false

	rg := newRG("mygroup", "team-a")
	done := make(chan error, 1)
	go func() { done <- p.HandlerFor(rg)(ctx, "", "plan-init", stubPayload(1), "") }()
	require.Eventually(t, func() bool { return findPod(ctx, t, fc, "team-a") != nil }, 2*time.Second, 5*time.Millisecond)
	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)

	// emptyDir volume + a self-installing init container.
	vol := volumeOf(pod, wrapperVol)
	require.NotNil(t, vol, "expected %s volume", wrapperVol)
	require.NotNil(t, vol.EmptyDir, "init path must use an emptyDir, not an image volume")
	assert.Nil(t, vol.Image)

	require.Len(t, pod.Spec.InitContainers, 1)
	init := pod.Spec.InitContainers[0]
	assert.Equal(t, wrapperInitName, init.Name)
	assert.Equal(t, testWrapperRef, init.Image)
	assert.Equal(t, []string{"/wrapper", "install", wrapperMount}, init.Command)
	require.Len(t, init.VolumeMounts, 1)
	assert.Equal(t, wrapperVol, init.VolumeMounts[0].Name)
	assert.Equal(t, wrapperMount, init.VolumeMounts[0].MountPath)

	runner := runnerOf(t, pod)
	assert.Equal(t, []string{wrapperPath}, runner.Command)

	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}

func TestProvisioner_WrapperInjection_Disabled(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	// WrapperImage intentionally empty → no injection.

	rg := newRG("mygroup", "team-a")
	done := make(chan error, 1)
	go func() { done <- p.HandlerFor(rg)(ctx, "", "plan-off", stubPayload(1), "") }()
	require.Eventually(t, func() bool { return findPod(ctx, t, fc, "team-a") != nil }, 2*time.Second, 5*time.Millisecond)
	pod := findPod(ctx, t, fc, "team-a")
	require.NotNil(t, pod)

	assert.Nil(t, volumeOf(pod, wrapperVol), "no wrapper volume when injection is disabled")
	assert.Empty(t, pod.Spec.InitContainers)
	runner := runnerOf(t, pod)
	assert.Nil(t, runner.Command, "runner command must be left to the image entrypoint when injection is disabled")

	completePod(ctx, t, fc, "team-a", pod.Name, corev1.PodSucceeded)
	require.NoError(t, <-done)
}
