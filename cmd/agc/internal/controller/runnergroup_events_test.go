package controller

import (
	"log/slog"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
)

// TestRunnerGroupDrainEvents_RecordsOwnSkipsOthers verifies the v1 event
// back-channel: an Event for this RunnerGroup is recorded on the live object,
// while one for another group is re-enqueued (mirroring drainConditions), so a
// listener/provisioner goroutine's quota/eviction-exhaustion Event lands on the
// right owner (Q170).
func TestRunnerGroupDrainEvents_RecordsOwnSkipsOthers(t *testing.T) {
	rec := events.NewFakeRecorder(16)
	r := &RunnerGroupReconciler{Log: slog.Default(), Recorder: rec}
	r.eventCh = make(chan eventRecord, 256)
	rg := &v1alpha1.RunnerGroup{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "rg"}}

	r.eventCh <- eventRecord{namespace: "ns", name: "rg", eventtype: corev1.EventTypeWarning,
		reason: "EvictionRetriesExhausted", action: "RetryEvictedJob", note: "manual re-run"}
	r.eventCh <- eventRecord{namespace: "ns", name: "other", eventtype: corev1.EventTypeWarning,
		reason: "QuotaRetriesExhausted", action: "ProvisionWorker", note: "quota exhausted"}

	r.drainEvents(rg)

	// Own event is recorded; the other group's event is re-enqueued.
	select {
	case e := <-rec.Events:
		assert.Contains(t, e, "EvictionRetriesExhausted")
	default:
		t.Fatal("expected an Event for this RunnerGroup")
	}
	assert.Len(t, r.eventCh, 1)
}
