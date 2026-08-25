package scalesetlistener

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestMetricsRecorderIncrements verifies that a per-RunnerSet recorder increments the
// right counter under the right (namespace, runner_set[, result]) labels, and that
// distinct RunnerSets keep separate series. It registers into a throwaway registry, so
// the test is isolated and re-runnable under `go test -count=N` (Q288).
func TestMetricsRecorderIncrements(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	rec := m.RecorderFor("tenant-a", "set-1")

	rec.IncJobAssigned()
	rec.IncJobAssigned()
	rec.IncJobProvisioned()
	rec.IncProvisionError()
	rec.IncJobCompleted("succeeded")
	rec.IncJobCompleted("succeeded")
	rec.IncJobCompleted("failed")
	rec.SetDeferredJobs(map[string]int{DeferReasonNameConflict: 2, DeferReasonCeiling: 0})
	// A gauge: the latest value wins, it does not accumulate. Every reason is written
	// on every call, so a reason that stopped holding jobs reads zero rather than
	// freezing at its last non-zero value.
	rec.SetDeferredJobs(map[string]int{DeferReasonNameConflict: 1, DeferReasonCeiling: 3})
	// Likewise a gauge, and one whose zero is load-bearing: "GitHub is holding nothing
	// for this set" is the reading that clears a queued-job investigation (Q720).
	rec.SetAvailableJobs(7)
	rec.SetAvailableJobs(0)

	if got := testutil.ToFloat64(m.JobsAssignedTotal.WithLabelValues("tenant-a", "set-1")); got != 2 {
		t.Errorf("JobsAssignedTotal = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.JobsProvisionedTotal.WithLabelValues("tenant-a", "set-1")); got != 1 {
		t.Errorf("JobsProvisionedTotal = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.ProvisionErrorsTotal.WithLabelValues("tenant-a", "set-1")); got != 1 {
		t.Errorf("ProvisionErrorsTotal = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.JobsCompletedTotal.WithLabelValues("tenant-a", "set-1", "succeeded")); got != 2 {
		t.Errorf("JobsCompletedTotal{result=succeeded} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.JobsCompletedTotal.WithLabelValues("tenant-a", "set-1", "failed")); got != 1 {
		t.Errorf("JobsCompletedTotal{result=failed} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.JobsDeferred.WithLabelValues("tenant-a", "set-1", DeferReasonNameConflict)); got != 1 {
		t.Errorf("JobsDeferred{reason=name_conflict} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.JobsDeferred.WithLabelValues("tenant-a", "set-1", DeferReasonCeiling)); got != 3 {
		t.Errorf("JobsDeferred{reason=ceiling} = %v, want 3", got)
	}

	if got := testutil.ToFloat64(m.AvailableJobs.WithLabelValues("tenant-a", "set-1")); got != 0 {
		t.Errorf("AvailableJobs = %v, want 0 (the latest reading, not the peak)", got)
	}

	// A second RunnerSet's recorder writes an independent series.
	m.RecorderFor("tenant-a", "set-2").IncJobAssigned()
	if got := testutil.ToFloat64(m.JobsAssignedTotal.WithLabelValues("tenant-a", "set-2")); got != 1 {
		t.Errorf("set-2 JobsAssignedTotal = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.JobsAssignedTotal.WithLabelValues("tenant-a", "set-1")); got != 2 {
		t.Errorf("set-1 JobsAssignedTotal changed to %v, want still 2", got)
	}
}

// TestNilMetricsRecorderForIsNil confirms a nil *Metrics yields a nil recorder, so an
// AGC that never wires NewMetrics passes a nil MetricsRecorder into Config (metrics
// disabled) rather than panicking — the default-Classic no-observability path.
func TestNilMetricsRecorderForIsNil(t *testing.T) {
	var m *Metrics
	if rec := m.RecorderFor("ns", "set"); rec != nil {
		t.Errorf("RecorderFor on nil *Metrics = %v, want nil", rec)
	}
}
