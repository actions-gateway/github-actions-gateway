package controller

import (
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnerimage"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// fakeImageResolver answers with a scripted lookup and records the requests it saw.
type fakeImageResolver struct {
	lookup   runnerimage.Lookup
	requests []runnerimage.Request
}

func (f *fakeImageResolver) Lookup(req runnerimage.Request) runnerimage.Lookup {
	f.requests = append(f.requests, req)
	return f.lookup
}

const digestOnlyImage = "ghcr.io/acme/runner@sha256:f2387135856decdecbf780a2bfbc9debe9c2dffd742f150302444b3775474681"

// Q988: the reconciler hands the resolver the effective image and the template's
// pull secrets, publishes the registry's verdict, and wakes itself when the answer
// changes.
func TestRunnerSetRunnerVersionStatusFromRegistry(t *testing.T) {
	prov := &provisioner.Provisioner{}
	template := &v2alpha1.RunnerTemplateSpec{
		WorkerImage: digestOnlyImage,
		PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "regcred"}, {Name: ""}},
		}},
	}

	t.Run("pending keeps Unknown and registers a wake", func(t *testing.T) {
		resolver := &fakeImageResolver{lookup: runnerimage.Lookup{State: runnerimage.Pending}}
		r := &RunnerSetReconciler{Provisioner: prov, ImageResolver: resolver}
		r.ensureMaps()
		rs := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}

		r.setRunnerVersionStatus(rs, template)

		cond := versionCondition(rs.Status.Conditions)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionUnknown, cond.Status)
		assert.Contains(t, cond.Message, "in progress")

		require.Len(t, resolver.requests, 1)
		req := resolver.requests[0]
		assert.Equal(t, digestOnlyImage, req.Image)
		assert.Equal(t, []string{"regcred"}, req.PullSecrets, "empty references are dropped")
		require.NotNil(t, req.Wake)
		req.Wake()
		select {
		case ev := <-r.wakeCh:
			assert.Equal(t, "ns", ev.Object.GetNamespace())
			assert.Equal(t, "set", ev.Object.GetName())
		case <-time.After(time.Second):
			t.Fatal("the wake did not reach the reconciler's channel")
		}
	})

	t.Run("a stale registry reading is a verdict and warns once", func(t *testing.T) {
		resolver := &fakeImageResolver{lookup: runnerimage.Lookup{State: runnerimage.Done, Result: runnerimage.Reading{
			Digest: "sha256:f2387135856decdecbf780a2bfbc9debe9c2dffd742f150302444b3775474681", Version: "2.320.0", Found: true}}}
		rec := events.NewFakeRecorder(16)
		r := &RunnerSetReconciler{Provisioner: prov, ImageResolver: resolver, Recorder: rec}
		rs := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns", Generation: 2}}

		r.setRunnerVersionStatus(rs, template)
		r.setRunnerVersionStatus(rs, template)

		cond := versionCondition(rs.Status.Conditions)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, v2alpha1.ReasonWorkerImageBelowMinimum, cond.Reason)
		assert.Contains(t, cond.Message, "read from the registry")
		assert.Len(t, rec.Events, 1)
	})

	t.Run("no resolver is the tag verdict", func(t *testing.T) {
		r := &RunnerSetReconciler{Provisioner: prov}
		rs := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}
		r.setRunnerVersionStatus(rs, template)
		cond := versionCondition(rs.Status.Conditions)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionUnknown, cond.Status)
		assert.NotContains(t, cond.Message, "registry")
	})
}

func TestRunnerGroupRunnerVersionStatusFromRegistry(t *testing.T) {
	resolver := &fakeImageResolver{lookup: runnerimage.Lookup{State: runnerimage.Done, Result: runnerimage.Reading{
		Digest: "sha256:f2387135856decdecbf780a2bfbc9debe9c2dffd742f150302444b3775474681", Version: "2.335.1", Found: true}}}
	r := &RunnerGroupReconciler{Provisioner: &provisioner.Provisioner{}, ImageResolver: resolver}
	r.ensureMaps()
	rg := &v1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "grp", Namespace: "ns"},
		Spec: v1alpha1.RunnerGroupSpec{
			WorkerImage: digestOnlyImage,
			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				ImagePullSecrets: []corev1.LocalObjectReference{{Name: "regcred"}},
			}},
		},
	}

	r.setRunnerVersionStatus(rg)

	cond := versionCondition(rg.Status.Conditions)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, v1alpha1.ReasonWorkerImageCurrent, cond.Reason)
	require.Len(t, resolver.requests, 1)
	assert.Equal(t, []string{"regcred"}, resolver.requests[0].PullSecrets)
	resolver.requests[0].Wake()
	var got event.GenericEvent
	select {
	case got = <-r.wakeCh:
	case <-time.After(time.Second):
		t.Fatal("the wake did not reach the reconciler's channel")
	}
	assert.Equal(t, "grp", got.Object.GetName())
}
