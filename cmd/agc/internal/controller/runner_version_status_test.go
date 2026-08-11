package controller

import (
	"testing"

	v1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/names"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
)

// staleImage is below names.MinRunnerVersion (2.329.0) and so cannot register with
// GitHub once enforcement lands.
const staleImage = "ghcr.io/actions/actions-runner:2.320.0"

func versionCondition(conds []metav1.Condition) *metav1.Condition {
	return meta.FindStatusCondition(conds, v2alpha1.ConditionRunnerVersionTooOld)
}

// TestRunnerSetRunnerVersionStatus covers the tier the Q715 signal exists for: the
// ScaleSet protocol carries no runner version at session creation, so before this the
// RunnerSet had no producer of RunnerVersionTooOld at all.
func TestRunnerSetRunnerVersionStatus(t *testing.T) {
	prov := &provisioner.Provisioner{}

	t.Run("stale workerImage reports too old and warns once", func(t *testing.T) {
		rec := events.NewFakeRecorder(16)
		r := &RunnerSetReconciler{Provisioner: prov, Recorder: rec}
		rs := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns", Generation: 3}}

		r.setRunnerVersionStatus(rs, &v2alpha1.RunnerTemplateSpec{WorkerImage: staleImage})

		cond := versionCondition(rs.Status.Conditions)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, v2alpha1.ReasonWorkerImageBelowMinimum, cond.Reason)
		assert.Equal(t, int64(3), cond.ObservedGeneration)
		assert.Contains(t, cond.Message, "2.320.0")
		require.Len(t, rec.Events, 1)

		// A second reconcile with the image unchanged must not re-warn: the condition
		// is already True, and an Event per reconcile would bury the transition.
		r.setRunnerVersionStatus(rs, &v2alpha1.RunnerTemplateSpec{WorkerImage: staleImage})
		assert.Len(t, rec.Events, 1)
	})

	t.Run("unset workerImage falls back to the AGC default", func(t *testing.T) {
		r := &RunnerSetReconciler{Provisioner: prov}
		rs := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}

		r.setRunnerVersionStatus(rs, &v2alpha1.RunnerTemplateSpec{})

		cond := versionCondition(rs.Status.Conditions)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, v2alpha1.ReasonWorkerImageCurrent, cond.Reason)
		assert.Contains(t, cond.Message, names.RunnerVersion)
	})

	t.Run("digest-only workerImage reports unknown, never current", func(t *testing.T) {
		r := &RunnerSetReconciler{Provisioner: prov}
		rs := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}

		r.setRunnerVersionStatus(rs, &v2alpha1.RunnerTemplateSpec{
			WorkerImage: "ghcr.io/acme/runner@sha256:0be6fc",
		})

		cond := versionCondition(rs.Status.Conditions)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionUnknown, cond.Status)
		assert.Equal(t, v2alpha1.ReasonWorkerImageVersionUnknown, cond.Reason)
	})

	t.Run("unresolved template publishes nothing", func(t *testing.T) {
		r := &RunnerSetReconciler{Provisioner: prov}
		rs := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}

		r.setRunnerVersionStatus(rs, nil)

		assert.Nil(t, versionCondition(rs.Status.Conditions),
			"the image the set would run is unknown, so judging the AGC-wide default would be a guess")
	})

	// The listener's own condition is the observed failure; the image check is a
	// prediction. Re-deriving it from a healthy image ref would clear a live outage.
	t.Run("a session-sourced VersionTooOld is not overwritten", func(t *testing.T) {
		r := &RunnerSetReconciler{Provisioner: prov}
		rs := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}
		meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
			Type:    v2alpha1.ConditionRunnerVersionTooOld,
			Status:  metav1.ConditionTrue,
			Reason:  v2alpha1.ReasonVersionTooOld,
			Message: "GitHub rejected the session",
		})

		r.setRunnerVersionStatus(rs, &v2alpha1.RunnerTemplateSpec{WorkerImage: names.DefaultWorkerImage})

		cond := versionCondition(rs.Status.Conditions)
		require.NotNil(t, cond)
		assert.Equal(t, v2alpha1.ReasonVersionTooOld, cond.Reason)
		assert.Equal(t, "GitHub rejected the session", cond.Message)
	})
}

func TestRunnerGroupRunnerVersionStatus(t *testing.T) {
	prov := &provisioner.Provisioner{}

	t.Run("stale workerImage reports too old and warns once", func(t *testing.T) {
		rec := events.NewFakeRecorder(16)
		r := &RunnerGroupReconciler{Provisioner: prov, Recorder: rec}
		rg := &v1alpha1.RunnerGroup{
			ObjectMeta: metav1.ObjectMeta{Name: "group", Namespace: "ns", Generation: 2},
			Spec:       v1alpha1.RunnerGroupSpec{WorkerImage: staleImage},
		}

		r.setRunnerVersionStatus(rg)

		cond := meta.FindStatusCondition(rg.Status.Conditions, v1alpha1.ConditionRunnerVersionTooOld)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, v1alpha1.ReasonWorkerImageBelowMinimum, cond.Reason)
		assert.Equal(t, int64(2), cond.ObservedGeneration)
		require.Len(t, rec.Events, 1)

		r.setRunnerVersionStatus(rg)
		assert.Len(t, rec.Events, 1)
	})

	t.Run("default workerImage reports current", func(t *testing.T) {
		r := &RunnerGroupReconciler{Provisioner: prov}
		rg := &v1alpha1.RunnerGroup{ObjectMeta: metav1.ObjectMeta{Name: "group", Namespace: "ns"}}

		r.setRunnerVersionStatus(rg)

		cond := meta.FindStatusCondition(rg.Status.Conditions, v1alpha1.ConditionRunnerVersionTooOld)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, v1alpha1.ReasonWorkerImageCurrent, cond.Reason)
	})

	t.Run("a session-sourced VersionTooOld is not overwritten", func(t *testing.T) {
		r := &RunnerGroupReconciler{Provisioner: prov}
		rg := &v1alpha1.RunnerGroup{ObjectMeta: metav1.ObjectMeta{Name: "group", Namespace: "ns"}}
		meta.SetStatusCondition(&rg.Status.Conditions, metav1.Condition{
			Type:   v1alpha1.ConditionRunnerVersionTooOld,
			Status: metav1.ConditionTrue,
			Reason: v1alpha1.ReasonVersionTooOld,
		})

		r.setRunnerVersionStatus(rg)

		cond := meta.FindStatusCondition(rg.Status.Conditions, v1alpha1.ConditionRunnerVersionTooOld)
		require.NotNil(t, cond)
		assert.Equal(t, v1alpha1.ReasonVersionTooOld, cond.Reason)
	})

	// A nil Provisioner is the pre-wiring state some unit constructions use; the
	// reconcile path must not depend on it being set.
	t.Run("no provisioner publishes nothing", func(t *testing.T) {
		r := &RunnerGroupReconciler{}
		rg := &v1alpha1.RunnerGroup{ObjectMeta: metav1.ObjectMeta{Name: "group", Namespace: "ns"}}

		r.setRunnerVersionStatus(rg)

		assert.Nil(t, meta.FindStatusCondition(rg.Status.Conditions, v1alpha1.ConditionRunnerVersionTooOld))
	})
}
