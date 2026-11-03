//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/names"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Q715 end-to-end against the real apiserver: the reconciler reads the runner version
// off the effective worker image and publishes RunnerVersionTooOld from it. The unit
// tests call setRunnerVersionStatus directly, so only these prove the reconcile path
// reaches status — and most of them run on the ScaleSet tier, whose acquisition
// protocol carries no runner version and which therefore had no producer of this
// condition at all. The condition is set before the protocol routing, so the ScaleSet
// path's own status write is what has to carry it.

// templateWithWorkerImage builds a direct-egress-ready RunnerTemplate pinned to a
// specific worker image, which is the whole input to the version verdict.
func templateWithWorkerImage(name, ns, image string) *v2alpha1.RunnerTemplate {
	return &v2alpha1.RunnerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v2alpha1.RunnerTemplateSpec{
			WorkerImage: image,
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: image}}},
			},
		},
	}
}

func waitForRunnerVersionCondition(t *testing.T, ns, name string, wantStatus metav1.ConditionStatus, wantReason string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var rs v2alpha1.RunnerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &rs); err != nil {
			return false
		}
		c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionRunnerVersionTooOld)
		return c != nil && c.Status == wantStatus && c.Reason == wantReason
	}, 20*time.Second, 100*time.Millisecond,
		"RunnerSet %s should report RunnerVersionTooOld=%s/%s", name, wantStatus, wantReason)
}

// setupScaleSetVersionSet stands up a ScaleSet-protocol set whose template pins image.
func setupScaleSetVersionSet(t *testing.T, ns, image string) {
	t.Helper()
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, ""))) // direct egress
	require.NoError(t, k8sClient.Create(ctx, templateWithWorkerImage("tmpl", ns, image)))
	rs := newScaleSetRunnerSet("set", ns, "gw", "linux", 3)
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv)
}

// TestV2_ScaleSet_WorkerImageBelowMinimum_Warns is the signal Q715 exists for: on the
// tier every new tenant runs, a worker image below GitHub's enforced registration
// minimum is now reported — before GitHub enforces it, and without a session that
// could have reported it.
func TestV2_ScaleSet_WorkerImageBelowMinimum_Warns(t *testing.T) {
	const ns = "v2-ss-runner-stale"
	setupScaleSetVersionSet(t, ns, "ghcr.io/actions/actions-runner:2.320.0")

	waitForRunnerVersionCondition(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonWorkerImageBelowMinimum)
}

// TestV2_ScaleSet_WorkerImageVersionUnknown_DoesNotBlockReady covers the custom-image
// case — a tag that is not a runner version reports Unknown rather than a verdict — and
// pins the advisory contract: the condition never gates Ready.
func TestV2_ScaleSet_WorkerImageVersionUnknown_DoesNotBlockReady(t *testing.T) {
	const ns = "v2-ss-runner-unknown"
	setupScaleSetVersionSet(t, ns, "acme.io/runner:v3-cuda")

	waitForRunnerVersionCondition(t, ns, "set", metav1.ConditionUnknown, v2alpha1.ReasonWorkerImageVersionUnknown)
	waitForSetReadyReason(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)
}

// TestV2_Classic_WorkerImageCurrent_DoesNotWarn pins the negative on the other tier:
// the version GAG itself ships must not warn, or every install would.
func TestV2_Classic_WorkerImageCurrent_DoesNotWarn(t *testing.T) {
	const ns = "v2-rs-runner-current"
	createNSForAGC(t, ns)
	startRunnerSetReconciler(t)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, templateWithWorkerImage("tmpl", ns, names.DefaultWorkerImage)))
	rs := newRunnerSet("set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	waitForRunnerVersionCondition(t, ns, "set", metav1.ConditionFalse, v2alpha1.ReasonWorkerImageCurrent)
}
