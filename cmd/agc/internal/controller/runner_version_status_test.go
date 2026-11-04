package controller

import (
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/names"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
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
	return meta.FindStatusCondition(conds, v2.ConditionRunnerVersionTooOld)
}

// TestRunnerSetRunnerVersionStatus covers the tier the Q715 signal exists for: the
// ScaleSet protocol carries no runner version at session creation, so before this the
// RunnerSet had no producer of RunnerVersionTooOld at all.
func TestRunnerSetRunnerVersionStatus(t *testing.T) {
	prov := &provisioner.Provisioner{}

	t.Run("stale workerImage reports too old and warns once", func(t *testing.T) {
		rec := events.NewFakeRecorder(16)
		r := &RunnerSetReconciler{Provisioner: prov, Recorder: rec}
		rs := &v2.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns", Generation: 3}}

		r.setRunnerVersionStatus(rs, &v2.RunnerTemplateSpec{WorkerImage: staleImage})

		cond := versionCondition(rs.Status.Conditions)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, v2.ReasonWorkerImageBelowMinimum, cond.Reason)
		assert.Equal(t, int64(3), cond.ObservedGeneration)
		assert.Contains(t, cond.Message, "2.320.0")
		require.Len(t, rec.Events, 1)

		// A second reconcile with the image unchanged must not re-warn: the condition
		// is already True, and an Event per reconcile would bury the transition.
		r.setRunnerVersionStatus(rs, &v2.RunnerTemplateSpec{WorkerImage: staleImage})
		assert.Len(t, rec.Events, 1)
	})

	t.Run("unset workerImage falls back to the AGC default", func(t *testing.T) {
		r := &RunnerSetReconciler{Provisioner: prov}
		rs := &v2.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}

		r.setRunnerVersionStatus(rs, &v2.RunnerTemplateSpec{})

		cond := versionCondition(rs.Status.Conditions)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, v2.ReasonWorkerImageCurrent, cond.Reason)
		assert.Contains(t, cond.Message, names.RunnerVersion)
	})

	t.Run("digest-only workerImage reports unknown, never current", func(t *testing.T) {
		r := &RunnerSetReconciler{Provisioner: prov}
		rs := &v2.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}

		r.setRunnerVersionStatus(rs, &v2.RunnerTemplateSpec{
			WorkerImage: "ghcr.io/acme/runner@sha256:0be6fc",
		})

		cond := versionCondition(rs.Status.Conditions)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionUnknown, cond.Status)
		assert.Equal(t, v2.ReasonWorkerImageVersionUnknown, cond.Reason)
	})

	t.Run("unresolved template publishes nothing", func(t *testing.T) {
		r := &RunnerSetReconciler{Provisioner: prov}
		rs := &v2.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}

		r.setRunnerVersionStatus(rs, nil)

		assert.Nil(t, versionCondition(rs.Status.Conditions),
			"the image the set would run is unknown, so judging the AGC-wide default would be a guess")
	})

	// An AGC that ran the classic listener can leave GitHub's session rejection in
	// status. Nothing publishes that reason any more, so the image reading has to take
	// the type back or the rejection would stand for the set's lifetime.
	t.Run("a stale session-sourced VersionTooOld is overwritten", func(t *testing.T) {
		r := &RunnerSetReconciler{Provisioner: prov}
		rs := &v2.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}
		meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
			Type:    v2.ConditionRunnerVersionTooOld,
			Status:  metav1.ConditionTrue,
			Reason:  v2.ReasonVersionTooOld,
			Message: "GitHub rejected the session",
		})

		r.setRunnerVersionStatus(rs, &v2.RunnerTemplateSpec{WorkerImage: names.DefaultWorkerImage})

		cond := versionCondition(rs.Status.Conditions)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, v2.ReasonWorkerImageCurrent, cond.Reason)
	})
}
