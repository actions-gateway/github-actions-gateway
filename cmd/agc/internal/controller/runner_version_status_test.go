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

// TestRunnerVersionClearArbitration covers the session half of the two-producer
// split on RunnerVersionTooOld (Q795). The image half — a healthy image reading not
// overwriting a session-sourced True — is pinned by the two subtests above.
//
// Each case drives one drain plus the image reading that follows it in the same
// reconcile, because the interesting behaviour is the interaction: drainConditions
// runs at reconcile step 2 and setRunnerVersionStatus well after it, so a push the
// drain merges is still open to being overwritten before the status write.
func TestRunnerVersionClearArbitration(t *testing.T) {
	prov := &provisioner.Provisioner{}

	clear := metav1.Condition{
		Type:    v2alpha1.ConditionRunnerVersionTooOld,
		Status:  metav1.ConditionFalse,
		Reason:  v2alpha1.ReasonVersionAccepted,
		Message: "GitHub accepted the runner version at session creation",
	}

	newSet := func() (*RunnerSetReconciler, *v2alpha1.RunnerSet) {
		r := &RunnerSetReconciler{
			Provisioner: prov,
			Recorder:    events.NewFakeRecorder(16),
			conditionCh: make(chan conditionUpdate, 8),
		}
		rs := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns", Generation: 1}}
		return r, rs
	}
	push := func(r *RunnerSetReconciler, cond metav1.Condition) {
		r.conditionCh <- conditionUpdate{namespace: "ns", name: "set", condition: cond}
	}

	// The defect Q795 names: agent.version is the AGC's compile-time pin, so the fix
	// for a session-sourced True is a gateway upgrade — which restarts the process
	// and leaves the condition behind in status with nothing to clear it.
	t.Run("clears a stale session-sourced VersionTooOld", func(t *testing.T) {
		r, rs := newSet()
		meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
			Type:    v2alpha1.ConditionRunnerVersionTooOld,
			Status:  metav1.ConditionTrue,
			Reason:  v2alpha1.ReasonVersionTooOld,
			Message: "GitHub rejected the session",
		})

		push(r, clear)
		r.drainConditions(rs)

		cond := versionCondition(rs.Status.Conditions)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, v2alpha1.ReasonVersionAccepted, cond.Reason)

		// With the session claim released, the image reading takes the type back.
		r.setRunnerVersionStatus(rs, &v2alpha1.RunnerTemplateSpec{WorkerImage: names.DefaultWorkerImage})
		cond = versionCondition(rs.Status.Conditions)
		require.NotNil(t, cond)
		assert.Equal(t, v2alpha1.ReasonWorkerImageCurrent, cond.Reason,
			"the reconciler resumes ownership once no session-sourced claim stands")
	})

	// The Q795 trap: an unconditional clear would drop a live Q715 warning.
	//
	// The drain is where it has to be refused, not the image reading. On a reconcile
	// that reaches setRunnerVersionStatus the merged clear is overwritten in memory
	// before any status write, so it is invisible; the reconcile paths that write
	// status EARLIER are where a merged clear persists (the unresolved-references
	// branch is one, covered by the envtest). And pendingConditions re-applies a push
	// the live status never reflects on every later reconcile, so it stays live
	// indefinitely rather than being spent once.
	t.Run("does not overwrite an image-sourced verdict", func(t *testing.T) {
		r, rs := newSet()
		r.setRunnerVersionStatus(rs, &v2alpha1.RunnerTemplateSpec{WorkerImage: staleImage})
		require.Equal(t, v2alpha1.ReasonWorkerImageBelowMinimum, versionCondition(rs.Status.Conditions).Reason)

		push(r, clear)
		r.drainConditions(rs)

		cond := versionCondition(rs.Status.Conditions)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, v2alpha1.ReasonWorkerImageBelowMinimum, cond.Reason)
	})

	// The refusal above is not enough on its own, which is the part that only shows up
	// over several reconciles. A listener push arriving before the set's first image
	// reading is legitimately merged — nothing stands to defer to — and pendingConditions
	// retains it, because that retry loop is built for types the reconciler never
	// re-derives. This type it does re-derive, so the image reading supersedes the push
	// on the same pass and the retained copy is then re-applied on every reconcile
	// afterwards, landing wherever status is written before the image reading runs.
	t.Run("does not re-apply a push the image reading has superseded", func(t *testing.T) {
		r, rs := newSet()

		// First push, no condition standing yet: merged and retained.
		push(r, clear)
		r.drainConditions(rs)
		require.Equal(t, v2alpha1.ReasonVersionAccepted, versionCondition(rs.Status.Conditions).Reason)

		// The image reading takes the type over in the same reconcile, as it would
		// before any status write.
		r.setRunnerVersionStatus(rs, &v2alpha1.RunnerTemplateSpec{WorkerImage: staleImage})
		require.Equal(t, v2alpha1.ReasonWorkerImageBelowMinimum, versionCondition(rs.Status.Conditions).Reason)

		// Later reconciles, with nothing further pushed. The retained copy must be
		// dropped rather than re-applied — the assertion is taken straight after the
		// drain, which is the state a reconcile returning early would persist.
		for i := range 4 {
			r.drainConditions(rs)
			cond := versionCondition(rs.Status.Conditions)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionTrue, cond.Status, "reconcile %d", i+2)
			assert.Equal(t, v2alpha1.ReasonWorkerImageBelowMinimum, cond.Reason, "reconcile %d", i+2)
			r.setRunnerVersionStatus(rs, &v2alpha1.RunnerTemplateSpec{WorkerImage: staleImage})
		}
	})

	// A healthy image reading is still image-sourced: the listener owns only the
	// session-sourced half, so it must not restate a verdict the reconciler derived.
	t.Run("does not overwrite a healthy image-sourced verdict", func(t *testing.T) {
		r, rs := newSet()
		r.setRunnerVersionStatus(rs, &v2alpha1.RunnerTemplateSpec{WorkerImage: names.DefaultWorkerImage})
		require.Equal(t, v2alpha1.ReasonWorkerImageCurrent, versionCondition(rs.Status.Conditions).Reason)

		push(r, clear)
		r.drainConditions(rs)

		assert.Equal(t, v2alpha1.ReasonWorkerImageCurrent, versionCondition(rs.Status.Conditions).Reason)
	})

	// With no condition of the type at all there is nothing to defer to, so the
	// baseline lands — the state a fresh set reaches before its first reconcile
	// publishes an image reading.
	t.Run("lands when no condition stands", func(t *testing.T) {
		r, rs := newSet()

		push(r, clear)
		r.drainConditions(rs)

		cond := versionCondition(rs.Status.Conditions)
		require.NotNil(t, cond)
		assert.Equal(t, v2alpha1.ReasonVersionAccepted, cond.Reason)
	})

	// The polarity of the reason check, pinned as behaviour rather than as a list.
	// A reason neither producer declares today stands in for one added later: it must
	// be treated as the reconciler's, so the clear is DROPPED and the condition merely
	// stays stale. Enumerating the image reasons instead would merge here and wipe a
	// live verdict, which is the failure direction this test exists to forbid.
	t.Run("an unrecognized reason is treated as the reconciler's", func(t *testing.T) {
		r, rs := newSet()
		meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
			Type:   v2alpha1.ConditionRunnerVersionTooOld,
			Status: metav1.ConditionTrue,
			Reason: "WorkerImageSomeFutureReason",
		})

		push(r, clear)
		r.drainConditions(rs)

		cond := versionCondition(rs.Status.Conditions)
		require.NotNil(t, cond)
		assert.Equal(t, "WorkerImageSomeFutureReason", cond.Reason,
			"an unknown reason must not be overwritten by the listener baseline")
	})

	// The listener's other two baselines are unaffected: the arbitration is keyed to
	// RunnerVersionTooOld/VersionAccepted, and Q332's pair has a single producer.
	t.Run("leaves the Q332 baselines alone", func(t *testing.T) {
		r, rs := newSet()
		meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
			Type:   v2alpha1.ConditionDegraded,
			Status: metav1.ConditionTrue,
			Reason: v2alpha1.ReasonSessionUnauthorized,
		})

		push(r, metav1.Condition{
			Type:   v2alpha1.ConditionDegraded,
			Status: metav1.ConditionFalse,
			Reason: v2alpha1.ReasonSessionAuthorized,
		})
		r.drainConditions(rs)

		cond := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionDegraded)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
	})
}

// TestRunnerGroupRunnerVersionClearArbitration is the RunnerGroup half of the
// arbitration, and the half that matters most: the classic tier is the only producer
// of a session-sourced RunnerVersionTooOld, and a RunnerGroup reconcile has its own
// early-status-write path (setCredentialUnavailable returns before the image reading
// runs), so a merged clear would persist there.
//
// Added after review found the RunnerGroup drain guard had no coverage at all — the
// whole controller suite stayed green with it deleted, because the arbitration tests
// above drive RunnerSetReconciler only.
func TestRunnerGroupRunnerVersionClearArbitration(t *testing.T) {
	r := &RunnerGroupReconciler{
		Provisioner: &provisioner.Provisioner{},
		conditionCh: make(chan conditionUpdate, 8),
	}
	rg := &v1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "group", Namespace: "ns", Generation: 1},
		Spec:       v1alpha1.RunnerGroupSpec{WorkerImage: staleImage},
	}
	r.setRunnerVersionStatus(rg)
	require.Equal(t, v1alpha1.ReasonWorkerImageBelowMinimum,
		meta.FindStatusCondition(rg.Status.Conditions, v1alpha1.ConditionRunnerVersionTooOld).Reason)

	r.conditionCh <- conditionUpdate{namespace: "ns", name: "group", condition: metav1.Condition{
		Type:    v1alpha1.ConditionRunnerVersionTooOld,
		Status:  metav1.ConditionFalse,
		Reason:  v1alpha1.ReasonVersionAccepted,
		Message: "GitHub accepted the runner version at session creation",
	}}
	r.drainConditions(rg)

	cond := meta.FindStatusCondition(rg.Status.Conditions, v1alpha1.ConditionRunnerVersionTooOld)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, v1alpha1.ReasonWorkerImageBelowMinimum, cond.Reason)

	// The clear must still land when the live condition is session-sourced, or the
	// guard would be refusing everything rather than only an image-sourced overwrite.
	rg2 := &v1alpha1.RunnerGroup{ObjectMeta: metav1.ObjectMeta{Name: "group", Namespace: "ns"}}
	meta.SetStatusCondition(&rg2.Status.Conditions, metav1.Condition{
		Type:   v1alpha1.ConditionRunnerVersionTooOld,
		Status: metav1.ConditionTrue,
		Reason: v1alpha1.ReasonVersionTooOld,
	})
	r.conditionCh <- conditionUpdate{namespace: "ns", name: "group", condition: metav1.Condition{
		Type:   v1alpha1.ConditionRunnerVersionTooOld,
		Status: metav1.ConditionFalse,
		Reason: v1alpha1.ReasonVersionAccepted,
	}}
	r.drainConditions(rg2)
	assert.Equal(t, v1alpha1.ReasonVersionAccepted,
		meta.FindStatusCondition(rg2.Status.Conditions, v1alpha1.ConditionRunnerVersionTooOld).Reason)
}
