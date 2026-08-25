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

// Q795 against the real apiserver. RunnerVersionTooOld has two producers reporting
// different facts through one type, and the tests above cover only the image half.
// These cover the session half's arbitration: the reconcile path has to clear a
// stale session-sourced True while leaving an image-sourced verdict alone, and both
// verdicts have to survive the status write rather than merely the merge.
//
// The unit tests drive drainConditions directly; only these prove the refusal holds
// through a real reconcile, where pendingConditions re-applies an unpersisted push
// on every pass and the image reading runs after the drain in the same one.

// TestV2_SessionVersionClear_ClearsStaleSessionSourcedTooOld is the defect Q795
// names. agent.version is the AGC's compile-time pin, so an operator fixes a
// session-sourced rejection by upgrading the gateway — which restarts the process
// and drops every in-memory flag, while the condition lives on in status. Before
// this the classic listener set True and nothing ever set it back.
func TestV2_SessionVersionClear_ClearsStaleSessionSourcedTooOld(t *testing.T) {
	const ns = "v2-rs-version-clear"
	createNSForAGC(t, ns)
	r := startRunnerSetReconciler(t)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, templateWithWorkerImage("tmpl", ns, names.DefaultWorkerImage)))
	rs := newRunnerSet("set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	// The listener rejecting a session, as a previous instance would have left it.
	r.SetConditionForTest(ns, "set", metav1.Condition{
		Type:    v2alpha1.ConditionRunnerVersionTooOld,
		Status:  metav1.ConditionTrue,
		Reason:  v2alpha1.ReasonVersionTooOld,
		Message: "GitHub rejected the session: runner version too old",
	})
	// Observed as True first, so the convergence below is a clear rather than a state
	// the set was already in. The image reading is healthy here and defers to it
	// (TestRunnerSetRunnerVersionStatus pins that half), so before Q795 this was the
	// terminal state: nothing in the system wrote this condition again.
	waitForRunnerVersionCondition(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonVersionTooOld)

	// The upgraded gateway's listener establishes a session and publishes the
	// baseline; the reconciler then resumes ownership from its image reading.
	//
	// Pushed explicitly rather than waited for. This namespace runs a real classic
	// listener, so its own baseline clears the condition too — that is the fix
	// working end to end — but it fires once per goroutine spawn, and racing a seed
	// against a spawn makes the test's timing decide whether it proves anything.
	r.SetConditionForTest(ns, "set", metav1.Condition{
		Type:    v2alpha1.ConditionRunnerVersionTooOld,
		Status:  metav1.ConditionFalse,
		Reason:  v2alpha1.ReasonVersionAccepted,
		Message: "GitHub accepted the runner version at session creation",
	})
	waitForRunnerVersionCondition(t, ns, "set", metav1.ConditionFalse, v2alpha1.ReasonWorkerImageCurrent)
}

// TestV2_SessionVersionClear_LeavesImageSourcedVerdict is the Q795 trap: an
// unconditional clear would drop a live Q715 warning.
//
// It has to be driven with the template deleted, and that is the finding rather than
// a test convenience. On a fully resolved set the drained clear never reaches the
// apiserver whatever the drain does, because setRunnerVersionStatus re-derives the
// verdict later in the same reconcile and overwrites it in memory first. The reconcile
// paths that write status BEFORE that point are where a merged clear persists, and the
// unresolved-references path is one: it stops the listeners, writes Ready=False with a
// <Ref>NotFound reason, and returns, carrying whatever the drain left on this
// condition. A tenant deleting or renaming a RunnerTemplate lands exactly there.
//
// pendingConditions then makes it permanent rather than momentary: a push the live
// status never reflects is re-applied on every subsequent reconcile, so each pass
// through this path re-wipes the verdict.
func TestV2_SessionVersionClear_LeavesImageSourcedVerdict(t *testing.T) {
	const ns = "v2-rs-version-image-wins"
	createNSForAGC(t, ns)
	r := startRunnerSetReconciler(t)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	tmpl := templateWithWorkerImage("tmpl", ns, "ghcr.io/actions/actions-runner:2.320.0")
	require.NoError(t, k8sClient.Create(ctx, tmpl))
	rs := newRunnerSet("set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(context.Background(), tmpl)
	})

	waitForRunnerVersionCondition(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonWorkerImageBelowMinimum)

	// The template vanishes: every later reconcile now returns from the unresolved
	// branch, ahead of the image reading that would otherwise restate the verdict.
	require.NoError(t, k8sClient.Delete(ctx, tmpl))
	waitForSetReadyReason(t, ns, "set", metav1.ConditionFalse, v2alpha1.ReasonTemplateDeleted)

	r.SetConditionForTest(ns, "set", metav1.Condition{
		Type:    v2alpha1.ConditionRunnerVersionTooOld,
		Status:  metav1.ConditionFalse,
		Reason:  v2alpha1.ReasonVersionAccepted,
		Message: "GitHub accepted the runner version at session creation",
	})

	// Over a window, not once: pendingConditions re-applies an unrefused push every
	// reconcile, so a single read just after the push could miss the wipe.
	requireRunnerVersionConditionHolds(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonWorkerImageBelowMinimum)
}

// requireRunnerVersionConditionHolds fails if the condition ever leaves the given
// verdict during the window. A single read after a push cannot tell a refused push
// from one that was merged and has not been overwritten yet.
func requireRunnerVersionConditionHolds(t *testing.T, ns, name string, wantStatus metav1.ConditionStatus, wantReason string) {
	t.Helper()
	require.Never(t, func() bool {
		var rs v2alpha1.RunnerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &rs); err != nil {
			return false
		}
		c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionRunnerVersionTooOld)
		return c == nil || c.Status != wantStatus || c.Reason != wantReason
	}, 3*time.Second, 100*time.Millisecond,
		"RunnerSet %s must hold RunnerVersionTooOld=%s/%s", name, wantStatus, wantReason)
}
