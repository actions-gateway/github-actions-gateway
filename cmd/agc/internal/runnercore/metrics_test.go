package runnercore

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newPollErrorMetrics builds a Metrics carrying only the poll-error counter, without
// touching the controller-runtime registry NewMetrics registers into — so the test is
// isolated and re-runnable under `go test -count=N`.
func newPollErrorMetrics() *Metrics {
	return &Metrics{
		MessagePollErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_message_poll_errors_total",
			Help: "Total number of GetMessage errors.",
		}, []string{"namespace", "reason"}),
	}
}

// TestPollErrorRecorderBindsNamespace proves the namespace-bound recorder the
// scale-set Listener takes (Q446) writes the same (namespace, reason) series the
// classic listener writes directly, and that two namespaces stay independent.
func TestPollErrorRecorderBindsNamespace(t *testing.T) {
	m := newPollErrorMetrics()

	rec := m.PollErrors("tenant-a")
	rec.IncPollError("rate_limited")
	rec.IncPollError("rate_limited")
	rec.IncPollError("other")
	m.PollErrors("tenant-b").IncPollError("timeout")

	if got := testutil.ToFloat64(m.MessagePollErrorsTotal.WithLabelValues("tenant-a", "rate_limited")); got != 2 {
		t.Errorf("tenant-a rate_limited = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.MessagePollErrorsTotal.WithLabelValues("tenant-a", "other")); got != 1 {
		t.Errorf("tenant-a other = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.MessagePollErrorsTotal.WithLabelValues("tenant-b", "timeout")); got != 1 {
		t.Errorf("tenant-b timeout = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.MessagePollErrorsTotal.WithLabelValues("tenant-b", "rate_limited")); got != 0 {
		t.Errorf("tenant-b rate_limited = %v, want 0 (namespaces must not share a series)", got)
	}
}

// TestPollErrorRecorderNilMetricsIsNoOp confirms a recorder built from a nil *Metrics
// — the shape a test or an AGC that never wired NewMetrics produces — silently drops
// increments rather than panicking in the poll loop.
func TestPollErrorRecorderNilMetricsIsNoOp(t *testing.T) {
	var m *Metrics
	m.PollErrors("ns").IncPollError("other")

	var rec *PollErrorRecorder
	rec.IncPollError("other")
}
