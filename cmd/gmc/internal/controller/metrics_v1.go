package controller

import (
	"context"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// The v1 ActionsGateway half of the GMC's scrape-time collectors (Q403). Kept in
// its own file so the v1 sunset (Q273) deletes it whole, leaving metrics.go — the
// collector types, their descriptors and the v2 passes — free of v1alpha1.
//
// Each dual-version collector in metrics.go bounds one context per scrape and hands
// it to its v1 pass below; a pass that cannot list emits nothing, which leaves the
// v2 series of the same metric family unaffected. The v1-only
// runnerGroupsDegradedCollector lives here whole: no v2 series shares its family
// (the v2 twin is actions_gateway_runnersets_degraded).

// collectV1 emits the EgressRulesStale gauge for every live v1 ActionsGateway. A
// list failure emits nothing rather than a misleading value.
func (c *egressRulesStaleCollector) collectV1(ctx context.Context, ch chan<- prometheus.Metric) {
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

// collectV1 emits the ProxyQuotaPressure and ProxyQuotaExceeded gauges for every
// live v1 ActionsGateway. A list failure emits nothing rather than a misleading
// value.
func (c *proxyQuotaCollector) collectV1(ctx context.Context, ch chan<- prometheus.Metric) {
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

// countV1 returns the number of live v1 ActionsGateways, and whether the list
// succeeded. A failed list reports ok=false so the caller can tell "no v1 gateways"
// apart from "the v1 cache was unreadable" and keep the gauge absent when no
// version could be read at all.
func (c *managedGatewaysCollector) countV1(ctx context.Context) (float64, bool) {
	var list gmcv1alpha1.ActionsGatewayList
	if err := c.reader.List(ctx, &list); err != nil {
		return 0, false
	}
	var managed float64
	for i := range list.Items {
		if list.Items[i].DeletionTimestamp.IsZero() {
			managed++
		}
	}
	return managed, true
}

// runnerGroupsDegradedCollector exports the RunnerGroupsDegraded rollup condition
// (Q158) as a gauge so operators can alert on impaired tenant RunnerGroups from
// the gateway's single pane without kube-state-metrics. Scrape-time reads and
// gauge semantics as managedGatewaysCollector.
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
	ctx, cancel := context.WithTimeout(context.Background(), collectorListTimeout)
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

// registerV1Collectors registers the v1-only scrape-time collectors with the
// controller-runtime metrics registry. Called from [NewMetrics]; the v1 sunset
// (Q273) drops that one call along with this file.
func registerV1Collectors(reader client.Reader) {
	metrics.Registry.MustRegister(newRunnerGroupsDegradedCollector(reader))
}
