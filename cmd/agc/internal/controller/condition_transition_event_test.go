package controller

import (
	"testing"

	v1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/names"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
)

// Every transition Event in these reconcilers is gated on the condition's prior
// status, read by value through conditionValue (see runner_shared.go for why the
// pointer form cannot work). Each case drives a second transition, which a retained
// pointer swallows, and then a no-op reconcile, which an ungated guard would warn on.

func TestRunnerSetReadyConditionEventsEveryTransition(t *testing.T) {
	rec := events.NewFakeRecorder(16)
	r := &RunnerSetReconciler{Recorder: rec}
	rs := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}

	r.setReadyCondition(rs, true, v2alpha1.ReasonListenerActive, "up")
	r.setReadyCondition(rs, false, v2alpha1.ReasonNoActiveSessions, "down")
	r.setReadyCondition(rs, true, v2alpha1.ReasonListenerActive, "up again")

	assert.Len(t, rec.Events, 3, "True→False→True is three transitions, so three Events")

	// A reconcile that changes nothing must still not re-warn.
	r.setReadyCondition(rs, true, v2alpha1.ReasonListenerActive, "up again")
	assert.Len(t, rec.Events, 3, "no transition, no Event")
}

func TestRunnerGroupReadyConditionEventsEveryTransition(t *testing.T) {
	rec := events.NewFakeRecorder(16)
	r := &RunnerGroupReconciler{Recorder: rec}
	rg := &v1alpha1.RunnerGroup{ObjectMeta: metav1.ObjectMeta{Name: "grp", Namespace: "ns"}}

	r.setReadyCondition(rg, true)
	r.setReadyCondition(rg, false)
	r.setReadyCondition(rg, true)

	assert.Len(t, rec.Events, 3, "True→False→True is three transitions, so three Events")

	r.setReadyCondition(rg, true)
	assert.Len(t, rec.Events, 3, "no transition, no Event")
}

func TestReapBlockingSidecarWarnsOnLaterTransition(t *testing.T) {
	rec := events.NewFakeRecorder(16)
	r := &RunnerSetReconciler{Recorder: rec}
	rs := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}
	runner := corev1.Container{Name: "runner"}
	dind := corev1.Container{Name: "dind"}

	// A clean template first, so the condition exists as False before the template
	// gains the sidecar — the False→True flip is the one an operator needs.
	r.setReapBlockingSidecarStatus(rs, sidecarTemplate(nil, runner), nil)
	assert.Len(t, rec.Events, 0, "no reap-blocking sidecar, nothing to warn about")

	r.setReapBlockingSidecarStatus(rs, sidecarTemplate(nil, runner, dind), nil)
	assert.Len(t, rec.Events, 1, "the template gained a reap-blocking sidecar")

	r.setReapBlockingSidecarStatus(rs, sidecarTemplate(nil, runner, dind), nil)
	assert.Len(t, rec.Events, 1, "still True, no transition, no second Event")
}

func TestRunnerSetRunnerVersionWarnsOnLaterDowngrade(t *testing.T) {
	rec := events.NewFakeRecorder(16)
	r := &RunnerSetReconciler{Provisioner: &provisioner.Provisioner{}, Recorder: rec}
	rs := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}

	r.setRunnerVersionStatus(rs, &v2alpha1.RunnerTemplateSpec{WorkerImage: names.DefaultWorkerImage})
	assert.Len(t, rec.Events, 0, "a current image is not a warning")

	r.setRunnerVersionStatus(rs, &v2alpha1.RunnerTemplateSpec{WorkerImage: staleImage})
	assert.Len(t, rec.Events, 1, "the workerImage was downgraded below the minimum")

	r.setRunnerVersionStatus(rs, &v2alpha1.RunnerTemplateSpec{WorkerImage: staleImage})
	assert.Len(t, rec.Events, 1, "still too old, no transition, no second Event")
}

func TestRunnerGroupRunnerVersionWarnsOnLaterDowngrade(t *testing.T) {
	rec := events.NewFakeRecorder(16)
	r := &RunnerGroupReconciler{Provisioner: &provisioner.Provisioner{}, Recorder: rec}
	rg := &v1alpha1.RunnerGroup{ObjectMeta: metav1.ObjectMeta{Name: "grp", Namespace: "ns"}}

	rg.Spec.WorkerImage = names.DefaultWorkerImage
	r.setRunnerVersionStatus(rg)
	assert.Len(t, rec.Events, 0, "a current image is not a warning")

	rg.Spec.WorkerImage = staleImage
	r.setRunnerVersionStatus(rg)
	assert.Len(t, rec.Events, 1, "the workerImage was downgraded below the minimum")

	r.setRunnerVersionStatus(rg)
	assert.Len(t, rec.Events, 1, "still too old, no transition, no second Event")
}
