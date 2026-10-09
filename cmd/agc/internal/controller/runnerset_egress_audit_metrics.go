package controller

import (
	"context"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// runnerSetEgressAuditCollector exports each RunnerSet's EgressAuditUnattributed
// condition (Q1069) as a gauge: the per-set twin of the GMC's gateway-scoped
// actions_gateway_egress_audit_unattributed, which reads only defaultProxyRef.
//
// It is shaped like the WorkerCapacityDeclined family in runnerSetCapacityCollector,
// for the same reasons. The reason label is closed and small (EgressAuditJoined,
// EgressAuditDisabled, WorkerAuditDisabled, ProxySourceAuditDisabled, DirectEgress)
// and is what lets a panel keep only sets with at least one half of the pair on. It
// reads at scrape time, so a reason change replaces the series and a deleted set's
// disappears. And it is emitted only once the condition is present: a set whose
// references never resolved has no pool to judge, and a 0 there would read as joined.
type runnerSetEgressAuditCollector struct {
	reader       client.Reader
	unattributed *prometheus.Desc
}

// NewRunnerSetEgressAuditCollector returns the collector that exports every
// RunnerSet's EgressAuditUnattributed condition, listing through reader at scrape time.
func NewRunnerSetEgressAuditCollector(reader client.Reader) prometheus.Collector {
	return &runnerSetEgressAuditCollector{
		reader: reader,
		unattributed: prometheus.NewDesc(
			"actions_gateway_runnerset_egress_audit_unattributed",
			"1 when the RunnerSet EgressAuditUnattributed condition is True (either half of the egress-attribution pair is off for this set's workers: the gateway does not log WorkerAddresses, or the EgressProxy the set resolves — its own proxyRef, else the gateway's defaultProxyRef — does not log ConnectionsWithSource), else 0. The reason label carries the condition's reason, which names the half that is off. Both halves are opt-in, so a 1 is the expected state on a tenant that never opted in. A 0 says the pair is configured, not that anything runs the join. Emitted only once the set's references have resolved.",
			[]string{"namespace", "runner_set", "reason"}, nil,
		),
	}
}

func (c *runnerSetEgressAuditCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.unattributed
}

func (c *runnerSetEgressAuditCollector) Collect(ch chan<- prometheus.Metric) {
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
		cond := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionEgressAuditUnattributed)
		if cond == nil {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.unattributed, prometheus.GaugeValue,
			conditionGaugeValue(rs.Status.Conditions, v2alpha1.ConditionEgressAuditUnattributed),
			rs.Namespace, rs.Name, cond.Reason)
	}
}

// registerRunnerSetEgressAuditMetrics registers the collector with the
// controller-runtime registry, tolerating double registration across test managers.
func registerRunnerSetEgressAuditMetrics(reader client.Reader) {
	if err := crmetrics.Registry.Register(NewRunnerSetEgressAuditCollector(reader)); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
			panic(err)
		}
	}
}
