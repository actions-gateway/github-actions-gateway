package controller

import (
	"context"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// runnerSetCapacityCollector exports the v2 RunnerSet worker-capacity conditions
// (Q303) as gauges — the v2 twins of the v1 RunnerGroup collectors in
// worker_quota.go and runnergroup_unschedulable.go (Q319).
//
// The families are named apart from the v1 ones rather than sharing them behind an
// added label: the v1 series key on runner_group, which a RunnerSet has none of, so
// one shared family would fold every set into a single runner_group="" bucket and
// silently corrupt the `sum by (namespace, runner_group)` groupings the v1 series
// promise. It also matches how every other RunnerSet-scoped family here is shaped
// (a distinct name plus a runner_set label).
//
// Like the v1 collectors it reads at scrape time from the cached reader, so a deleted
// RunnerSet stops being listed and its series disappears with no reconcile-path cost
// and no stale-series cleanup. Each value mirrors the condition the reconciler wrote
// to .status.conditions (1 when True, 0 otherwise).
type runnerSetCapacityCollector struct {
	reader        client.Reader
	pressure      *prometheus.Desc
	exceeded      *prometheus.Desc
	unschedulable *prometheus.Desc
}

// NewRunnerSetCapacityCollector returns the collector that exports every RunnerSet's
// worker-capacity conditions as gauges, listing through reader at scrape time.
func NewRunnerSetCapacityCollector(reader client.Reader) prometheus.Collector {
	return &runnerSetCapacityCollector{
		reader: reader,
		pressure: prometheus.NewDesc(
			"actions_gateway_runnerset_worker_quota_pressure",
			"1 when the RunnerSet WorkerQuotaPressure condition is True (workers cannot scale to the configured ceiling within the namespace ResourceQuota headroom), else 0.",
			[]string{"namespace", "runner_set"}, nil,
		),
		exceeded: prometheus.NewDesc(
			"actions_gateway_runnerset_worker_quota_exceeded",
			"1 when the RunnerSet WorkerQuotaExceeded condition is True (the namespace ResourceQuota cannot admit another worker pod), else 0.",
			[]string{"namespace", "runner_set"}, nil,
		),
		unschedulable: prometheus.NewDesc(
			"actions_gateway_runnerset_workers_unschedulable",
			"1 when the RunnerSet WorkersUnschedulable condition is True (worker pods are Pending and cannot be scheduled for a non-quota reason), else 0.",
			[]string{"namespace", "runner_set"}, nil,
		),
	}
}

func (c *runnerSetCapacityCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pressure
	ch <- c.exceeded
	ch <- c.unschedulable
}

func (c *runnerSetCapacityCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var list v2alpha1.RunnerSetList
	if err := c.reader.List(ctx, &list); err != nil {
		return
	}
	for i := range list.Items {
		rs := &list.Items[i]
		if !rs.DeletionTimestamp.IsZero() {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.pressure, prometheus.GaugeValue,
			conditionGaugeValue(rs.Status.Conditions, v2alpha1.ConditionWorkerQuotaPressure), rs.Namespace, rs.Name)
		ch <- prometheus.MustNewConstMetric(c.exceeded, prometheus.GaugeValue,
			conditionGaugeValue(rs.Status.Conditions, v2alpha1.ConditionWorkerQuotaExceeded), rs.Namespace, rs.Name)
		ch <- prometheus.MustNewConstMetric(c.unschedulable, prometheus.GaugeValue,
			conditionGaugeValue(rs.Status.Conditions, v2alpha1.ConditionWorkersUnschedulable), rs.Namespace, rs.Name)
	}
}

// registerRunnerSetCapacityMetrics registers the RunnerSet worker-capacity collector
// with the controller-runtime registry. Like registerWorkerQuotaMetrics it tolerates
// double registration across test managers.
func registerRunnerSetCapacityMetrics(reader client.Reader) {
	if err := crmetrics.Registry.Register(NewRunnerSetCapacityCollector(reader)); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
			panic(err)
		}
	}
}
