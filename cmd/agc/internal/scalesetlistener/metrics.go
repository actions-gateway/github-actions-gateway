package scalesetlistener

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds the Prometheus counters the scale-set acquisition tier emits, mirroring
// the classic listener.Metrics for the Option E (Q264) path. The scale-set tier runs
// one Listener session per ScaleSet-protocol RunnerSet, so every counter is labelled by
// (namespace, runner_set); job-completion additionally carries the server-reported
// result. These are distinct from the classic actions_gateway_* series — a Classic
// RunnerSet never touches them — so an operator can watch either tier independently
// during the P4 dogfood validation (the Q224 acceptance gate).
//
// The Listener's MetricsRecorder interface is label-free (one Listener owns one set):
// RecorderFor binds a per-RunnerSet adapter over these vectors and is what the
// reconciler passes into scalesetlistener.Config.Metrics.
type Metrics struct {
	// JobsAssignedTotal counts each JobAssigned the scale set's queue delivers.
	JobsAssignedTotal *prometheus.CounterVec
	// JobsProvisionedTotal counts each worker pod successfully provisioned for an
	// assigned job.
	JobsProvisionedTotal *prometheus.CounterVec
	// ProvisionErrorsTotal counts failed provision attempts (JIT-config mint or pod
	// create); the job is left un-provisioned so a later poll retries it.
	ProvisionErrorsTotal *prometheus.CounterVec
	// JobsCompletedTotal counts terminal JobCompleted messages by result — the
	// completion signal the classic many-acquirers protocol never delivered (§2b-6).
	JobsCompletedTotal *prometheus.CounterVec
	// AdvertisedCapacity is the X-ScaleSetMaxCapacity most recently advertised for the
	// set — the total jobs GitHub may keep assigned to it. It is this tier's answer to
	// "why did assignments stop": the classic tier's per-job
	// actions_gateway_jobs_admission_rejected_total is structurally unreachable here,
	// because a job the ladder declines is never assigned rather than being claimed and
	// rejected (Q443).
	AdvertisedCapacity *prometheus.GaugeVec
	// CapacityWithheld is how many slots each admission rung removed from the declared
	// ceiling on that same poll, labelled by the rung's AdmitReason*. Every evaluated
	// rung publishes a value each poll — zero when it did not bind — so a series never
	// goes stale at its last non-zero reading.
	CapacityWithheld *prometheus.GaugeVec
	// JobsDeferred is how many assigned jobs the listener is holding for a re-offer,
	// labelled by the DeferReason* it is held under — the alertable mirror of the
	// RunnerSet's JobProvisionStalled condition. Each one is a workflow run queued at
	// GitHub with nothing running it, but the two reasons want different alerts: a
	// runner name that will not register needs an operator (Q551), while a full worker
	// ceiling is the set doing what its spec says and clears as workers finish (Q576).
	// Every reason is published on every update, zero included, so a series never
	// freezes at its last non-zero reading.
	JobsDeferred *prometheus.GaugeVec
	// AvailableJobs is the server-authoritative totalAvailableJobs from the most recent
	// statistics reading: jobs GitHub holds queued for this scale set that it has not
	// assigned to anyone. It is the demand signal the other series cannot supply — every
	// one of them starts at assignment, so a set that is advertising no capacity looks
	// idle on all of them while work piles up at GitHub. Read it against
	// AdvertisedCapacity to answer "why is this job still queued": available jobs with a
	// zero ceiling is the AGC withholding, and available jobs with a ceiling to spare is
	// GitHub not delivering.
	//
	// Not refreshed on every poll. Statistics ride on queue messages and session
	// responses, and a 202 (nothing to deliver) carries none, so the gauge holds its last
	// reading across an idle stretch rather than tracking one. The readings are the
	// every session open (the first and the re-create after a 404), every delivered
	// message, and the assignment check's session refresh while jobs are deferred (Q720).
	AvailableJobs *prometheus.GaugeVec
	// JobsAbandonedTotal counts assignments the listener gave up on because the scale
	// set stopped counting them as assigned — GitHub no longer holds the job, and no
	// JobCompleted was ever delivered for it (Q553). It is the counterpart of
	// JobsDeferred: the gauge is work still waiting, this is work that will never run.
	// A non-zero rate means workflow runs are being dropped, so it is alertable on its
	// own, and it is what distinguishes a deferred set that cleared because the jobs ran
	// from one that cleared because they evaporated.
	JobsAbandonedTotal *prometheus.CounterVec
}

// NewMetrics creates the scale-set tier's metrics and registers them with reg, which
// must be non-nil. Production passes the controller-runtime registry
// (sigs.k8s.io/controller-runtime/pkg/metrics.Registry) so the collectors are exposed
// on the AGC's /metrics endpoint; a test passes a throwaway prometheus.NewRegistry()
// so each call is isolated.
//
// Registration uses MustRegister, so calling NewMetrics twice against the *same*
// registry panics on the duplicate collector. That is deliberate: a second registration
// against the process registry means the production wiring built two metric sets and
// half the increments would be invisible, and the panic surfaces it at startup. Taking
// the registry as a parameter (rather than reaching for the global) is what lets a test
// call NewMetrics repeatedly — once per `go test -count` iteration — without tripping it.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		JobsAssignedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_scaleset_jobs_assigned_total",
			Help: "Total jobs the scale set's queue delivered as JobAssigned to the acquisition-tier listener (Q264 Option E).",
		}, []string{"namespace", "runner_set"}),

		JobsProvisionedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_scaleset_jobs_provisioned_total",
			Help: "Total worker pods successfully provisioned by the scale-set acquisition tier, one per assigned job.",
		}, []string{"namespace", "runner_set"}),

		ProvisionErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_scaleset_provision_errors_total",
			Help: "Total failed provision attempts (JIT-config mint or worker pod create) by the scale-set acquisition tier; the job is retried on a later poll.",
		}, []string{"namespace", "runner_set"}),

		JobsCompletedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_scaleset_jobs_completed_total",
			Help: "Total terminal JobCompleted messages the scale set's queue delivered, by result (the completion signal the classic protocol never delivered).",
		}, []string{"namespace", "runner_set", "result"}),

		AdvertisedCapacity: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "actions_gateway_scaleset_advertised_capacity",
			Help: "X-ScaleSetMaxCapacity most recently advertised for the scale set: the total jobs GitHub may keep assigned to it at once.",
		}, []string{"namespace", "runner_set"}),

		CapacityWithheld: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "actions_gateway_scaleset_capacity_withheld",
			Help: "Slots each admission rung removed from the declared worker ceiling on the most recent poll, by rung (reason).",
		}, []string{"namespace", "runner_set", "reason"}),

		JobsDeferred: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "actions_gateway_scaleset_jobs_deferred",
			Help: "Assigned jobs held for a later re-offer, by reason (name_conflict: the runner name will not register; ceiling: the set is at its worker ceiling); each is a queued workflow run with no worker (sets the JobProvisionStalled condition).",
		}, []string{"namespace", "runner_set", "reason"}),

		AvailableJobs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "actions_gateway_scaleset_jobs_available",
			Help: "Jobs GitHub holds queued for the scale set and has not assigned to anyone, from the most recent statistics reading; an empty poll carries no statistics, so this holds its last reading rather than refreshing every poll, and a set advertising no capacity refreshes it every 5m.",
		}, []string{"namespace", "runner_set"}),

		JobsAbandonedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_scaleset_jobs_abandoned_total",
			Help: "Total assigned jobs the listener gave up on because the scale set no longer counts them as assigned (GitHub dropped the assignment without reporting the job complete); each is a workflow run that will never run.",
		}, []string{"namespace", "runner_set"}),
	}
	reg.MustRegister(
		m.JobsAssignedTotal,
		m.JobsProvisionedTotal,
		m.ProvisionErrorsTotal,
		m.JobsCompletedTotal,
		m.AdvertisedCapacity,
		m.CapacityWithheld,
		m.JobsDeferred,
		m.AvailableJobs,
		m.JobsAbandonedTotal,
	)
	return m
}

// SetAdvertisedCapacity publishes the capacity advertised for one RunnerSet on its most
// recent poll. Nil-receiver-safe, so a caller that never wired NewMetrics needs no
// guard — matching RecorderFor's nil handling.
//
// It is set by the reconciler that computes the number rather than by the Listener that
// sends it, because the accompanying CapacityWithheld breakdown only exists where the
// admission ladder is walked (provisioner.AdvertiseCapacity).
func (m *Metrics) SetAdvertisedCapacity(namespace, runnerSet string, capacity int32) {
	if m == nil {
		return
	}
	m.AdvertisedCapacity.WithLabelValues(namespace, runnerSet).Set(float64(capacity))
}

// SetCapacityWithheld publishes how many slots one admission rung withheld from a
// RunnerSet's declared ceiling on its most recent poll. Nil-receiver-safe.
func (m *Metrics) SetCapacityWithheld(namespace, runnerSet, reason string, slots int32) {
	if m == nil {
		return
	}
	m.CapacityWithheld.WithLabelValues(namespace, runnerSet, reason).Set(float64(slots))
}

// DeleteRunnerSet drops every series carrying a deleted RunnerSet's labels, so a set
// that is gone stops reporting a stale advertised capacity forever. Counters are left
// alone: their series are cumulative totals a scrape may still be catching up on, while
// a gauge that stops being written is indistinguishable from one frozen at its last
// value. Nil-receiver-safe.
func (m *Metrics) DeleteRunnerSet(namespace, runnerSet string) {
	if m == nil {
		return
	}
	labels := prometheus.Labels{"namespace": namespace, "runner_set": runnerSet}
	m.AdvertisedCapacity.Delete(labels)
	m.CapacityWithheld.DeletePartialMatch(labels)
	m.JobsDeferred.DeletePartialMatch(labels)
	m.AvailableJobs.Delete(labels)
}

// RecorderFor returns a MetricsRecorder that records into m's vectors under the given
// RunnerSet's (namespace, runner_set) labels. A nil *Metrics returns a nil recorder,
// which scalesetlistener.Config accepts (metrics disabled) — so an operator who never
// wires NewMetrics keeps default-Classic behavior with no observability side effects.
func (m *Metrics) RecorderFor(namespace, runnerSet string) MetricsRecorder {
	if m == nil {
		return nil
	}
	return &recorder{m: m, namespace: namespace, runnerSet: runnerSet}
}

// recorder binds a Metrics to one RunnerSet's labels, implementing MetricsRecorder.
type recorder struct {
	m         *Metrics
	namespace string
	runnerSet string
}

func (r *recorder) IncJobAssigned() {
	r.m.JobsAssignedTotal.WithLabelValues(r.namespace, r.runnerSet).Inc()
}

func (r *recorder) IncJobProvisioned() {
	r.m.JobsProvisionedTotal.WithLabelValues(r.namespace, r.runnerSet).Inc()
}

func (r *recorder) IncProvisionError() {
	r.m.ProvisionErrorsTotal.WithLabelValues(r.namespace, r.runnerSet).Inc()
}

func (r *recorder) IncJobCompleted(result string) {
	r.m.JobsCompletedTotal.WithLabelValues(r.namespace, r.runnerSet, result).Inc()
}

func (r *recorder) SetDeferredJobs(byReason map[string]int) {
	for reason, n := range byReason {
		r.m.JobsDeferred.WithLabelValues(r.namespace, r.runnerSet, reason).Set(float64(n))
	}
}

func (r *recorder) SetAvailableJobs(n int) {
	r.m.AvailableJobs.WithLabelValues(r.namespace, r.runnerSet).Set(float64(n))
}

func (r *recorder) IncJobsAbandoned(n int) {
	r.m.JobsAbandonedTotal.WithLabelValues(r.namespace, r.runnerSet).Add(float64(n))
}
