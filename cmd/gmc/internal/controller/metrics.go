package controller

import (
	"context"
	"time"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics holds the GMC's custom Prometheus metrics. Construct it with
// [NewMetrics], which also registers a scrape-time collector for
// actions_gateway_managed_gateways.
type Metrics struct {
	// IPRangeUpdates counts NetworkPolicy egress-rule refreshes applied from the
	// GitHub meta API, labelled by tenant namespace. Incremented by
	// [IPRangeReconciler] on each successful NetworkPolicy patch.
	IPRangeUpdates *prometheus.CounterVec
}

// NewMetrics constructs the GMC metrics and registers them with the
// controller-runtime metrics registry, so they are served on the same /metrics
// endpoint as the built-in controller-runtime metrics. reader should be the
// manager's cached client; it backs the managed-gateways collector, which lists
// ActionsGateway CRs at scrape time (no staleness, no reconcile-path cost).
func NewMetrics(reader client.Reader) *Metrics {
	m := &Metrics{
		IPRangeUpdates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_ip_range_updates_total",
			Help: "NetworkPolicy egress-rule refreshes applied from the GitHub meta API, per tenant namespace.",
		}, []string{"namespace"}),
	}
	metrics.Registry.MustRegister(m.IPRangeUpdates, newManagedGatewaysCollector(reader))
	return m
}

// managedGatewaysCollector reports actions_gateway_managed_gateways by listing
// ActionsGateway CRs from the cached reader on each scrape. A custom collector
// (rather than a Gauge updated on reconcile) avoids both staleness — the
// periodic IP-range refresh is 24h, far too coarse — and per-reconcile List
// overhead, while always reflecting the current cluster state.
type managedGatewaysCollector struct {
	reader client.Reader
	desc   *prometheus.Desc
}

func newManagedGatewaysCollector(reader client.Reader) *managedGatewaysCollector {
	return &managedGatewaysCollector{
		reader: reader,
		desc: prometheus.NewDesc(
			"actions_gateway_managed_gateways",
			"Number of ActionsGateway custom resources currently managed by the GMC (excludes those being deleted).",
			nil, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *managedGatewaysCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect implements prometheus.Collector. On a read failure it emits no metric
// rather than a misleading value; the gauge is simply absent until the cache is
// readable.
func (c *managedGatewaysCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var list gmcv1alpha1.ActionsGatewayList
	if err := c.reader.List(ctx, &list); err != nil {
		return
	}
	var managed float64
	for i := range list.Items {
		if list.Items[i].DeletionTimestamp.IsZero() {
			managed++
		}
	}
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, managed)
}
