package controller

import (
	"context"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// collectorListTimeout bounds the CR lists a scrape-time collector issues, so a
// wedged cache cannot hold a /metrics scrape open indefinitely. Shared by every
// collector in this package, including the v1 passes in metrics_v1.go.
const collectorListTimeout = 5 * time.Second

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
// manager's cached client; it backs the scrape-time collectors, which list the
// relevant CRs on each scrape (no staleness, no reconcile-path cost).
//
// v2Enabled reflects whether the opt-in actions-gateway.com/v2alpha1 CRDs are
// installed (the same startup detection that gates the v2 controllers). When true,
// the collectors also count v2 ActionsGateways and reflect v2 EgressProxy proxy
// conditions, and a v2-only collector exports the v2 ActionsGateway condition
// gauges (Q321); when false they stay v1-only, so a v1-only cluster never spins a
// failed informer for absent v2 kinds.
func NewMetrics(reader client.Reader, v2Enabled bool) *Metrics {
	m := &Metrics{
		IPRangeUpdates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_ip_range_updates_total",
			Help: "NetworkPolicy egress-rule refreshes applied from the GitHub meta API, per tenant namespace.",
		}, []string{"namespace"}),
	}
	metrics.Registry.MustRegister(m.IPRangeUpdates, newManagedGatewaysCollector(reader, v2Enabled),
		newProxyQuotaCollector(reader, v2Enabled), newEgressRulesStaleCollector(reader, v2Enabled))
	// The v1-only collectors (metrics_v1.go) register themselves through one call, so
	// the v1 sunset (Q273) drops this line along with that file (Q403).
	registerV1Collectors(reader)
	// The v2 ActionsGateway condition gauges (Q321) are v2-only — no v1 series
	// shares their metric families — so unlike the collectors above they are
	// registered only when the v2 CRDs are installed. Registering them on a v1-only
	// cluster would spin a failed informer for the absent v2 ActionsGateway kind on
	// every scrape.
	if v2Enabled {
		metrics.Registry.MustRegister(newActionsGatewayV2ConditionsCollector(reader),
			newGitHubEgressIncompleteCollector(reader))
	}
	return m
}

// gitHubEgressIncompleteCollector exports the EgressProxy GitHubEgressIncomplete
// condition (Q506 #3) as a gauge, so a GHES tenant whose CIDR allowlist cannot
// reach its appliance is alertable fleet-wide rather than one kubectl describe at
// a time (Q537). Scrape-time reads and gauge semantics as managedGatewaysCollector.
// The condition exists only on the v2 EgressProxy — v1 has no twin — so like the v2
// ActionsGateway condition gauges it is registered only when the v2 CRDs are
// installed (see [NewMetrics]).
type gitHubEgressIncompleteCollector struct {
	reader     client.Reader
	incomplete *prometheus.Desc
}

func newGitHubEgressIncompleteCollector(reader client.Reader) *gitHubEgressIncompleteCollector {
	return &gitHubEgressIncompleteCollector{
		reader: reader,
		incomplete: prometheus.NewDesc(
			"actions_gateway_github_egress_incomplete",
			"1 when the EgressProxy GitHubEgressIncomplete condition is True (a referring gateway names a GitHub Enterprise Server host the CIDR-mode egress allowlist cannot reach), else 0. Supplying spec.destinationCIDRs or an FQDN egress mode clears it.",
			[]string{"namespace", "name"}, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *gitHubEgressIncompleteCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.incomplete
}

// Collect implements prometheus.Collector. On a read failure it emits nothing
// rather than a misleading value.
func (c *gitHubEgressIncompleteCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), collectorListTimeout)
	defer cancel()

	var epList gmcv2alpha1.EgressProxyList
	if err := c.reader.List(ctx, &epList); err != nil {
		return
	}
	for i := range epList.Items {
		ep := &epList.Items[i]
		if !ep.DeletionTimestamp.IsZero() {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.incomplete, prometheus.GaugeValue,
			conditionGaugeValue(ep.Status.Conditions, gmcv2alpha1.ConditionGitHubEgressIncomplete), ep.Namespace, ep.Name)
	}
}

// egressRulesStaleCollector exports the EgressRulesStale condition (Q157) as a
// gauge so operators can alert on a stalled GitHub IP-range refresh without
// kube-state-metrics. Scrape-time reads and gauge semantics as
// managedGatewaysCollector.
type egressRulesStaleCollector struct {
	reader    client.Reader
	v2Enabled bool
	stale     *prometheus.Desc
}

func newEgressRulesStaleCollector(reader client.Reader, v2Enabled bool) *egressRulesStaleCollector {
	return &egressRulesStaleCollector{
		reader:    reader,
		v2Enabled: v2Enabled,
		stale: prometheus.NewDesc(
			"actions_gateway_egress_rules_stale",
			"1 when the EgressRulesStale condition is True (the GitHub egress IP-range allowlist has not been refreshed within the staleness window), else 0. The name label is the v1 ActionsGateway or the v2 EgressProxy carrying the condition.",
			[]string{"namespace", "name"}, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *egressRulesStaleCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.stale
}

// Collect implements prometheus.Collector. Each version's pass is independent: a
// read failure for one API version skips only that version's series rather than
// emitting a misleading value.
func (c *egressRulesStaleCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), collectorListTimeout)
	defer cancel()

	// v1 pass (metrics_v1.go): deleted wholesale by the v1 sunset (Q273).
	c.collectV1(ctx, ch)

	if !c.v2Enabled {
		return
	}
	var epList gmcv2alpha1.EgressProxyList
	if err := c.reader.List(ctx, &epList); err == nil {
		for i := range epList.Items {
			ep := &epList.Items[i]
			if !ep.DeletionTimestamp.IsZero() {
				continue
			}
			ch <- prometheus.MustNewConstMetric(c.stale, prometheus.GaugeValue,
				conditionGaugeValue(ep.Status.Conditions, gmcv2alpha1.ConditionEgressRulesStale), ep.Namespace, ep.Name)
		}
	}
}

// actionsGatewayV2ConditionsCollector exports the v2 ActionsGateway rollup and
// availability conditions (Q321) as gauges so operators can alert on v2 gateway
// state without kube-state-metrics — the v2 twin of the v1 ActionsGateway
// condition gauges above. This includes the advisory AGCAutoscalingUnavailable
// condition (Q360/Q390), so an agcAutoscaling opt-in that cannot be satisfied is
// alertable rather than only visible via kubectl describe. Scrape-time reads and
// gauge semantics as managedGatewaysCollector. It is registered only when the v2
// CRDs are installed (see [NewMetrics]), so a v1-only cluster never lists the
// absent v2 ActionsGateway kind.
type actionsGatewayV2ConditionsCollector struct {
	reader                    client.Reader
	runnerSetsDegraded        *prometheus.Desc
	agcAvailable              *prometheus.Desc
	egressUnattributed        *prometheus.Desc
	agcAutoscalingUnavailable *prometheus.Desc
	scaleSetNameCollision     *prometheus.Desc
}

func newActionsGatewayV2ConditionsCollector(reader client.Reader) *actionsGatewayV2ConditionsCollector {
	return &actionsGatewayV2ConditionsCollector{
		reader: reader,
		runnerSetsDegraded: prometheus.NewDesc(
			"actions_gateway_runnersets_degraded",
			"1 when the v2 ActionsGateway RunnerSetsDegraded condition is True (one or more RunnerSets bound to the gateway report an impairing condition), else 0. The v2 twin of actions_gateway_runnergroups_degraded.",
			[]string{"namespace", "name"}, nil,
		),
		agcAvailable: prometheus.NewDesc(
			"actions_gateway_agc_available",
			"1 when the v2 ActionsGateway AGCAvailable condition is True (the tenant's AGC Deployment has a ready replica), else 0.",
			[]string{"namespace", "name"}, nil,
		),
		egressUnattributed: prometheus.NewDesc(
			"actions_gateway_egress_unattributed",
			"1 when the v2 ActionsGateway EgressUnattributed condition is True (the gateway runs in direct egress mode, so its GitHub traffic is not attributed to a per-tenant egress proxy), else 0.",
			[]string{"namespace", "name"}, nil,
		),
		agcAutoscalingUnavailable: prometheus.NewDesc(
			"actions_gateway_agc_autoscaling_unavailable",
			"1 when the v2 ActionsGateway AGCAutoscalingUnavailable condition is True (the agcAutoscaling opt-in cannot be satisfied, e.g. the VerticalPodAutoscaler CRDs are not installed), else 0. The AGC still runs on its stamped agcResources sizing; this is advisory.",
			[]string{"namespace", "name"}, nil,
		),
		scaleSetNameCollision: prometheus.NewDesc(
			"actions_gateway_scale_set_name_collision",
			"1 when the v2 ActionsGateway ScaleSetNameCollision condition is True (a ScaleSet RunnerSet bound to this gateway claims a scale-set name another RunnerSet already claims in the same GitHub scope, so both AGCs drive one scale set and each acquires the other tenant's jobs), else 0. Admission rejects new such pairs, so a 1 is a pair that predates the guard or was applied with the webhook uninstalled — alert on it.",
			[]string{"namespace", "name"}, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *actionsGatewayV2ConditionsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.runnerSetsDegraded
	ch <- c.agcAvailable
	ch <- c.egressUnattributed
	ch <- c.agcAutoscalingUnavailable
	ch <- c.scaleSetNameCollision
}

// Collect implements prometheus.Collector. On a read failure it emits nothing
// rather than a misleading value.
func (c *actionsGatewayV2ConditionsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), collectorListTimeout)
	defer cancel()

	var list gmcv2alpha1.ActionsGatewayList
	if err := c.reader.List(ctx, &list); err != nil {
		return
	}
	for i := range list.Items {
		ag := &list.Items[i]
		if !ag.DeletionTimestamp.IsZero() {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.runnerSetsDegraded, prometheus.GaugeValue,
			conditionGaugeValue(ag.Status.Conditions, gmcv2alpha1.ConditionRunnerSetsDegraded), ag.Namespace, ag.Name)
		ch <- prometheus.MustNewConstMetric(c.agcAvailable, prometheus.GaugeValue,
			conditionGaugeValue(ag.Status.Conditions, gmcv2alpha1.ConditionAGCAvailable), ag.Namespace, ag.Name)
		ch <- prometheus.MustNewConstMetric(c.egressUnattributed, prometheus.GaugeValue,
			conditionGaugeValue(ag.Status.Conditions, gmcv2alpha1.ConditionEgressUnattributed), ag.Namespace, ag.Name)
		ch <- prometheus.MustNewConstMetric(c.agcAutoscalingUnavailable, prometheus.GaugeValue,
			conditionGaugeValue(ag.Status.Conditions, gmcv2alpha1.ConditionAGCAutoscalingUnavailable), ag.Namespace, ag.Name)
		ch <- prometheus.MustNewConstMetric(c.scaleSetNameCollision, prometheus.GaugeValue,
			conditionGaugeValue(ag.Status.Conditions, gmcv2alpha1.ConditionScaleSetNameCollision), ag.Namespace, ag.Name)
	}
}

// proxyQuotaCollector exports the proxy ResourceQuota conditions (Q82) as
// gauges, so operators can alert on them directly without kube-state-metrics
// scraping CRD conditions. Scrape-time reads and gauge semantics as
// managedGatewaysCollector.
type proxyQuotaCollector struct {
	reader    client.Reader
	v2Enabled bool
	pressure  *prometheus.Desc
	exceeded  *prometheus.Desc
}

func newProxyQuotaCollector(reader client.Reader, v2Enabled bool) *proxyQuotaCollector {
	return &proxyQuotaCollector{
		reader:    reader,
		v2Enabled: v2Enabled,
		pressure: prometheus.NewDesc(
			"actions_gateway_proxy_quota_pressure",
			"1 when the ProxyQuotaPressure condition is True (the proxy pool cannot scale to maxReplicas within the namespace ResourceQuota headroom), else 0. The name label is the v1 ActionsGateway or the v2 EgressProxy owning the pool.",
			[]string{"namespace", "name"}, nil,
		),
		exceeded: prometheus.NewDesc(
			"actions_gateway_proxy_quota_exceeded",
			"1 when the ProxyQuotaExceeded condition is True (proxy replica creation is being rejected by the namespace ResourceQuota), else 0. The name label is the v1 ActionsGateway or the v2 EgressProxy owning the pool.",
			[]string{"namespace", "name"}, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *proxyQuotaCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pressure
	ch <- c.exceeded
}

// Collect implements prometheus.Collector. Each version's pass is independent: a
// read failure for one API version skips only that version's series rather than
// emitting a misleading value.
func (c *proxyQuotaCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), collectorListTimeout)
	defer cancel()

	// v1 pass (metrics_v1.go): deleted wholesale by the v1 sunset (Q273).
	c.collectV1(ctx, ch)

	if !c.v2Enabled {
		return
	}
	var epList gmcv2alpha1.EgressProxyList
	if err := c.reader.List(ctx, &epList); err == nil {
		for i := range epList.Items {
			ep := &epList.Items[i]
			if !ep.DeletionTimestamp.IsZero() {
				continue
			}
			ch <- prometheus.MustNewConstMetric(c.pressure, prometheus.GaugeValue,
				conditionGaugeValue(ep.Status.Conditions, gmcv2alpha1.ConditionProxyQuotaPressure), ep.Namespace, ep.Name)
			ch <- prometheus.MustNewConstMetric(c.exceeded, prometheus.GaugeValue,
				conditionGaugeValue(ep.Status.Conditions, gmcv2alpha1.ConditionProxyQuotaExceeded), ep.Namespace, ep.Name)
		}
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
// overhead, while always reflecting the current cluster state. A deleted
// ActionsGateway simply stops being listed, so its series disappears with no
// stale-series cleanup; the condition-gauge collectors below share this shape
// and mirror the reconciler-written condition (1 when True, 0 otherwise).
type managedGatewaysCollector struct {
	reader    client.Reader
	v2Enabled bool
	desc      *prometheus.Desc
}

func newManagedGatewaysCollector(reader client.Reader, v2Enabled bool) *managedGatewaysCollector {
	return &managedGatewaysCollector{
		reader:    reader,
		v2Enabled: v2Enabled,
		desc: prometheus.NewDesc(
			"actions_gateway_managed_gateways",
			"Number of ActionsGateway custom resources (v1 and v2) currently managed by the GMC (excludes those being deleted).",
			nil, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *managedGatewaysCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect implements prometheus.Collector. It sums the non-deleting v1 and (when v2
// is enabled) v2 ActionsGateways. Each version's pass is independent: it emits the
// count from whichever lists succeed, and only stays silent when every attempted
// list fails — so the gauge is absent rather than misleading until the cache is
// readable.
func (c *managedGatewaysCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), collectorListTimeout)
	defer cancel()

	var managed float64
	var read bool

	// v1 pass (metrics_v1.go): deleted wholesale by the v1 sunset (Q273).
	if n, ok := c.countV1(ctx); ok {
		read = true
		managed += n
	}

	if c.v2Enabled {
		var v2List gmcv2alpha1.ActionsGatewayList
		if err := c.reader.List(ctx, &v2List); err == nil {
			read = true
			for i := range v2List.Items {
				if v2List.Items[i].DeletionTimestamp.IsZero() {
					managed++
				}
			}
		}
	}

	if !read {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, managed)
}
