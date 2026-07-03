//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// These tests exercise the Q249 reap-blocking-sidecar warning end-to-end against the
// real apiserver: the AGC reconciler surfaces the advisory PossibleReapBlockingSidecar
// condition from the resolved template, gated by the self-exiting-sidecars opt-out —
// and, critically, the condition NEVER blocks the RunnerSet from reaching Ready.

// templateWithSidecar builds a direct-egress-ready RunnerTemplate whose worker pod
// carries the given extra (non-runner) regular containers, plus optional native
// sidecars (restartPolicy: Always init containers) and metadata annotations.
func templateWithSidecar(name, ns string, annotations map[string]string, sidecars, nativeSidecars []corev1.Container) *v2alpha1.RunnerTemplate {
	return &v2alpha1.RunnerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Annotations: annotations},
		Spec: v2alpha1.RunnerTemplateSpec{
			WorkerImage: "runner:test",
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers:     append([]corev1.Container{{Name: "runner", Image: "runner:test"}}, sidecars...),
					InitContainers: nativeSidecars,
				},
			},
		},
	}
}

func waitForReapBlockingCondition(t *testing.T, ns, name string, wantStatus metav1.ConditionStatus, wantReason string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var rs v2alpha1.RunnerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &rs); err != nil {
			return false
		}
		c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionPossibleReapBlockingSidecar)
		return c != nil && c.Status == wantStatus && c.Reason == wantReason
	}, 20*time.Second, 100*time.Millisecond,
		"RunnerSet %s should report PossibleReapBlockingSidecar=%s/%s", name, wantStatus, wantReason)
}

// TestV2_RunnerSet_ReapBlockingSidecar_WarnsButDoesNotBlock: a template with a regular
// (non-native) sidecar makes the set report PossibleReapBlockingSidecar=True — yet the
// set still reaches Ready=True/ListenerActive, proving the warning is advisory and never
// blocks provisioning.
func TestV2_RunnerSet_ReapBlockingSidecar_WarnsButDoesNotBlock(t *testing.T) {
	const ns = "v2-rs-reap-warn"
	createNSForAGC(t, ns)
	startRunnerSetReconciler(t)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, ""))) // direct egress
	dind := corev1.Container{Name: "dind", Image: "docker:dind"}
	require.NoError(t, k8sClient.Create(ctx, templateWithSidecar("tmpl", ns, nil, []corev1.Container{dind}, nil)))
	rs := newRunnerSet("set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	// The set both warns AND reaches Ready — the warning does not gate provisioning.
	waitForReapBlockingCondition(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonReapBlockingSidecar)
	waitForSetReadyReason(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)
}

// TestV2_RunnerSet_NativeSidecar_NotFlagged: a native sidecar (restartPolicy: Always
// init container) does not block reaping, so the set reports
// PossibleReapBlockingSidecar=False.
func TestV2_RunnerSet_NativeSidecar_NotFlagged(t *testing.T) {
	const ns = "v2-rs-reap-native"
	createNSForAGC(t, ns)
	startRunnerSetReconciler(t)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	always := corev1.ContainerRestartPolicyAlways
	nativeDind := corev1.Container{Name: "dind", Image: "docker:dind", RestartPolicy: &always}
	require.NoError(t, k8sClient.Create(ctx, templateWithSidecar("tmpl", ns, nil, nil, []corev1.Container{nativeDind})))
	rs := newRunnerSet("set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	waitForSetReadyReason(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)
	waitForReapBlockingCondition(t, ns, "set", metav1.ConditionFalse, v2alpha1.ReasonNoReapBlockingSidecar)
}

// TestV2_RunnerSet_ReapBlockingSidecar_OptOutSuppresses: naming the sidecar in the
// self-exiting-sidecars annotation clears the condition.
func TestV2_RunnerSet_ReapBlockingSidecar_OptOutSuppresses(t *testing.T) {
	const ns = "v2-rs-reap-optout"
	createNSForAGC(t, ns)
	startRunnerSetReconciler(t)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	dind := corev1.Container{Name: "dind", Image: "docker:dind"}
	acked := map[string]string{v2alpha1.SelfExitingSidecarsAnnotation: "dind"}
	require.NoError(t, k8sClient.Create(ctx, templateWithSidecar("tmpl", ns, acked, []corev1.Container{dind}, nil)))
	rs := newRunnerSet("set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	waitForSetReadyReason(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)
	waitForReapBlockingCondition(t, ns, "set", metav1.ConditionFalse, v2alpha1.ReasonNoReapBlockingSidecar)
}
