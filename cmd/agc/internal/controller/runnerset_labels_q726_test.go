package controller

import (
	"testing"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
)

// Q726 — RunnerLabelsIncomplete reports whether the scale set at GitHub carries every
// label the runner set declares. It is derived on every reconcile rather than once at
// listener start, because a label appended to a live set diverges long after the scale
// set was registered and the listener does not restart for a spec change.

func labelSet(labels ...string) func(*v2alpha1.RunnerSet) {
	return func(rs *v2alpha1.RunnerSet) { rs.Spec.RunnerLabels = labels }
}

func TestRunnerLabels_AllRegisteredReportsFalse(t *testing.T) {
	rs := rsObj("set", "ns", labelSet("linux", "gpu"))
	r := &RunnerSetReconciler{}

	r.applyRunnerLabelsCondition(rs, []string{"linux", "gpu"})

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionRunnerLabelsIncomplete)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, v2alpha1.ReasonLabelsRegistered, c.Reason)
}

// A scale set may carry labels the runner set does not declare — a leftover from an
// earlier spec, say. That is not a shortfall: nothing the set asks for is missing, and
// reporting it would page an operator for a label their workflows do not name.
func TestRunnerLabels_ExtraServerLabelIsNotAShortfall(t *testing.T) {
	rs := rsObj("set", "ns", labelSet("linux"))
	r := &RunnerSetReconciler{}

	r.applyRunnerLabelsCondition(rs, []string{"linux", "retired-label"})

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionRunnerLabelsIncomplete)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status,
		"the comparison is one-directional: declared ⊆ registered, not equality")
}

func TestRunnerLabels_MissingLabelReportsTrueAndEvents(t *testing.T) {
	rs := rsObj("set", "ns", labelSet("linux", "gpu", "private-network"))
	rec := events.NewFakeRecorder(16)
	r := &RunnerSetReconciler{Recorder: rec}

	r.applyRunnerLabelsCondition(rs, []string{"linux"})

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionRunnerLabelsIncomplete)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, v2alpha1.ReasonLabelsNotRegistered, c.Reason)
	assert.Contains(t, c.Message, "gpu")
	assert.Contains(t, c.Message, "private-network")
	assert.Contains(t, c.Message, "stay queued at GitHub",
		"the message must state the consequence, not merely list the labels")
	assert.Contains(t, c.Message, "AllowRunnerScaleSetCustomLabels",
		"the GHES remedy must be in the message; it is the one an operator cannot guess")

	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, "Warning")
		assert.Contains(t, ev, "RunnerLabelsNotRegistered")
	default:
		t.Fatal("the rising edge must record an Event, not only a condition")
	}
}

// The Event marks a transition, so a set that is still short on the next reconcile must
// not re-emit it — an Event is an incident signal, not a heartbeat.
func TestRunnerLabels_EventOnlyOnTheRisingEdge(t *testing.T) {
	rs := rsObj("set", "ns", labelSet("linux", "gpu"))
	rec := events.NewFakeRecorder(16)
	r := &RunnerSetReconciler{Recorder: rec}

	r.applyRunnerLabelsCondition(rs, []string{"linux"})
	<-rec.Events // the rising edge
	r.applyRunnerLabelsCondition(rs, []string{"linux"})

	select {
	case ev := <-rec.Events:
		t.Fatalf("a still-short set re-emitted the Event: %s", ev)
	default:
	}
}

// The case the whole condition exists for, end to end: a label appended to a live set
// never reaches the scale set, because the AGC reuses it by name. Nothing errors, so
// this is the only place it surfaces.
func TestRunnerLabels_AppendedLabelSurfacesOnTheNextReconcile(t *testing.T) {
	rs := rsObj("set", "ns", labelSet("linux"))
	r := &RunnerSetReconciler{Recorder: events.NewFakeRecorder(16)}
	registered := []string{"linux"}

	r.applyRunnerLabelsCondition(rs, registered)
	require.Equal(t, metav1.ConditionFalse,
		meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionRunnerLabelsIncomplete).Status)

	// The operator appends a label. The scale set is untouched.
	rs.Spec.RunnerLabels = []string{"linux", "gpu"}
	r.applyRunnerLabelsCondition(rs, registered)

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionRunnerLabelsIncomplete)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status,
		"appending a label to a live set must surface on the very next reconcile")
	assert.Contains(t, c.Message, "gpu")
}

// A listener that has not ensured its scale set reports nil, which is not the same
// claim as "the server returned no labels". Publishing a shortfall from it would
// declare every label missing on a set nobody has looked at yet.
func TestRunnerLabels_NoObservationPublishesNothing(t *testing.T) {
	rs := rsObj("set", "ns", labelSet("linux", "gpu"))
	r := &RunnerSetReconciler{}

	r.applyRunnerLabelsCondition(rs, nil)

	assert.Nil(t, meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionRunnerLabelsIncomplete),
		"no observation must publish no condition at all")
}

// The condition is advisory. Rolling it into the gateway's RunnerSetsDegraded summary
// would page for a configuration mismatch on a set that is still serving every job
// targeting the labels that did register.
func TestRunnerLabels_ConditionIsNotImpairing(t *testing.T) {
	for _, ct := range v2alpha1.ImpairingConditionTypes() {
		assert.NotEqual(t, v2alpha1.ConditionRunnerLabelsIncomplete, ct,
			"RunnerLabelsIncomplete must stay out of the impairing rollup")
	}
}
