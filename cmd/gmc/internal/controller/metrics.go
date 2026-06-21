package controller

import (
	"context"
	"time"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	metrics.Registry.MustRegister(m.IPRangeUpdates, newManagedGatewaysCollector(reader),
		newProxyQuotaCollector(reader), newRunnerGroupsDegradedCollector(reader),
		newEgressRulesStaleCollector(reader))
	return m
}

// egressRulesStaleCollector exports the EgressRulesStale condition (Q157) as a
// gauge so operators can alert on a stalled GitHub IP-range refresh without
// kube-state-metrics. Like the other collectors it reads at scrape time from the
// cached reader: a deleted ActionsGateway simply stops being listed. The gauge
// mirrors the condition the reconciler wrote to .status.conditions (1 when True,
// 0 otherwise).
type egressRulesStaleCollector struct {
	reader client.Reader
	stale  *prometheus.Desc
}

func newEgressRulesStaleCollector(reader client.Reader) *egressRulesStaleCollector {
	return &egressRulesStaleCollector{
		reader: reader,
		stale: prometheus.NewDesc(
			"actions_gateway_egress_rules_stale",
			"1 when the ActionsGateway EgressRulesStale condition is True (the GitHub egress IP-range allowlist has not been refreshed within the staleness window), else 0.",
			[]string{"namespace", "name"}, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *egressRulesStaleCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.stale
}

// Collect implements prometheus.Collector. On a read failure it emits nothing
// rather than a misleading value.
func (c *egressRulesStaleCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var list gmcv1alpha1.ActionsGatewayList
	if err := c.reader.List(ctx, &list); err != nil {
		return
	}
	for i := range list.Items {
		ag := &list.Items[i]
		if !ag.DeletionTimestamp.IsZero() {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.stale, prometheus.GaugeValue,
			conditionGaugeValue(ag.Status.Conditions, gmcv1alpha1.ConditionEgressRulesStale), ag.Namespace, ag.Name)
	}
}

// runnerGroupsDegradedCollector exports the RunnerGroupsDegraded rollup condition
// (Q158) as a gauge so operators can alert on impaired tenant RunnerGroups from
// the gateway's single pane without kube-state-metrics. Like the other collectors
// it reads at scrape time from the cached reader: a deleted ActionsGateway simply
// stops being listed. The gauge mirrors the condition the reconciler already wrote
// to .status.conditions (1 when True, 0 otherwise).
type runnerGroupsDegradedCollector struct {
	reader   client.Reader
	degraded *prometheus.Desc
}

func newRunnerGroupsDegradedCollector(reader client.Reader) *runnerGroupsDegradedCollector {
	return &runnerGroupsDegradedCollector{
		reader: reader,
		degraded: prometheus.NewDesc(
			"actions_gateway_runnergroups_degraded",
			"1 when the ActionsGateway RunnerGroupsDegraded condition is True (one or more owned RunnerGroups report an impairing condition), else 0.",
			[]string{"namespace", "name"}, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *runnerGroupsDegradedCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.degraded
}

// Collect implements prometheus.Collector. On a read failure it emits nothing
// rather than a misleading value.
func (c *runnerGroupsDegradedCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var list gmcv1alpha1.ActionsGatewayList
	if err := c.reader.List(ctx, &list); err != nil {
		return
	}
	for i := range list.Items {
		ag := &list.Items[i]
		if !ag.DeletionTimestamp.IsZero() {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.degraded, prometheus.GaugeValue,
			conditionGaugeValue(ag.Status.Conditions, gmcv1alpha1.ConditionRunnerGroupsDegraded), ag.Namespace, ag.Name)
	}
}

// proxyQuotaCollector exports the proxy ResourceQuota conditions (Q82) as
// gauges, so operators can alert on them directly without kube-state-metrics
// scraping CRD conditions. Like managedGatewaysCollector it reads at scrape time
// from the cached reader: a deleted ActionsGateway simply stops being listed, so
// its series disappears with no reconcile-path cost and no stale-series cleanup.
// The gauge value mirrors the condition the reconciler already wrote to
// .status.conditions (1 when True, 0 otherwise).
type proxyQuotaCollector struct {
	reader   client.Reader
	pressure *prometheus.Desc
	exceeded *prometheus.Desc
}

func newProxyQuotaCollector(reader client.Reader) *proxyQuotaCollector {
	return &proxyQuotaCollector{
		reader: reader,
		pressure: prometheus.NewDesc(
			"actions_gateway_proxy_quota_pressure",
			"1 when the ActionsGateway ProxyQuotaPressure condition is True (the proxy pool cannot scale to maxReplicas within the namespace ResourceQuota headroom), else 0.",
			[]string{"namespace", "name"}, nil,
		),
		exceeded: prometheus.NewDesc(
			"actions_gateway_proxy_quota_exceeded",
			"1 when the ActionsGateway ProxyQuotaExceeded condition is True (proxy replica creation is being rejected by the namespace ResourceQuota), else 0.",
			[]string{"namespace", "name"}, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *proxyQuotaCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pressure
	ch <- c.exceeded
}

// Collect implements prometheus.Collector. On a read failure it emits nothing
// rather than a misleading value.
func (c *proxyQuotaCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var list gmcv1alpha1.ActionsGatewayList
	if err := c.reader.List(ctx, &list); err != nil {
		return
	}
	for i := range list.Items {
		ag := &list.Items[i]
		if !ag.DeletionTimestamp.IsZero() {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.pressure, prometheus.GaugeValue,
			conditionGaugeValue(ag.Status.Conditions, gmcv1alpha1.ConditionProxyQuotaPressure), ag.Namespace, ag.Name)
		ch <- prometheus.MustNewConstMetric(c.exceeded, prometheus.GaugeValue,
			conditionGaugeValue(ag.Status.Conditions, gmcv1alpha1.ConditionProxyQuotaExceeded), ag.Namespace, ag.Name)
	}
}

// conditionGaugeValue maps a status condition to a gauge value: 1 when the
// condition is present and True, 0 otherwise. This is the project convention for
// exporting an alertable CRD condition as a controller-owned metric.
func conditionGaugeValue(conds []metav1.Condition, condType string) float64 {
	if meta.IsStatusConditionTrue(conds, condType) {
		return 1
	}
	return 0
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
