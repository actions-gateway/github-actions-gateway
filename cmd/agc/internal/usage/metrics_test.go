package usage_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/usage"
)

// TestNewMetricsRegisters verifies every collector lands on the given registry
// (a second registration on the same registry panics by design, so one clean
// NewMetrics per registry is the contract — see scalesetlistener.NewMetrics).
func TestNewMetricsRegisters(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := usage.NewMetrics(reg)
	if m == nil {
		t.Fatal("NewMetrics returned nil")
	}

	// Touch one child per vec so Gather sees the families.
	m.CPUPeak.WithLabelValues("ns", "rs", "runner").Set(1)
	m.MemoryPeak.WithLabelValues("ns", "rs", "runner").Set(1)
	m.JobCPUPeak.WithLabelValues("ns", "rs", "runner").Observe(1)
	m.JobMemoryPeak.WithLabelValues("ns", "rs", "runner").Observe(1)
	m.JobsSampled.WithLabelValues("ns", "rs").Inc()
	m.JobsUnsampled.WithLabelValues("ns", "rs").Inc()
	m.PollErrors.WithLabelValues("ns").Inc()

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"actions_gateway_worker_usage_cpu_peak_cores":        false,
		"actions_gateway_worker_usage_memory_peak_bytes":     false,
		"actions_gateway_worker_usage_job_cpu_peak_cores":    false,
		"actions_gateway_worker_usage_job_memory_peak_bytes": false,
		"actions_gateway_worker_usage_jobs_sampled_total":    false,
		"actions_gateway_worker_usage_jobs_unsampled_total":  false,
		"actions_gateway_worker_usage_poll_errors_total":     false,
	}
	for _, f := range families {
		if _, ok := want[f.GetName()]; ok {
			want[f.GetName()] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("metric family %s not registered", name)
		}
	}
}
