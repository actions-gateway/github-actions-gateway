package usage

import (
	"context"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// DefaultSampleInterval matches the metrics-server default resolution (its
// --metric-resolution is 15s); polling faster only re-reads the same sample.
const DefaultSampleInterval = 15 * time.Second

// pollErrorLogEvery throttles the repeated-failure log line: with the default
// interval, one line roughly every 10 minutes while the metrics API stays
// unavailable, instead of one per tick.
const pollErrorLogEvery = 40

// PodMetricsLister lists the metrics.k8s.io PodMetrics for a namespace. The
// production implementation wraps the k8s.io/metrics clientset; tests stub it.
type PodMetricsLister interface {
	ListPodMetrics(ctx context.Context, namespace string) (*metricsv1beta1.PodMetricsList, error)
}

// clientsetLister adapts the typed metrics clientset to PodMetricsLister.
type clientsetLister struct {
	cs metricsclient.Interface
}

func (l clientsetLister) ListPodMetrics(ctx context.Context, namespace string) (*metricsv1beta1.PodMetricsList, error) {
	return l.cs.MetricsV1beta1().PodMetricses(namespace).List(ctx, metav1.ListOptions{})
}

// NewClientsetLister returns a PodMetricsLister backed by the typed
// metrics.k8s.io clientset.
func NewClientsetLister(cs metricsclient.Interface) PodMetricsLister {
	return clientsetLister{cs: cs}
}

// containerPeak is the running usage peak for one container of one worker pod.
type containerPeak struct {
	cpuCores float64
	memBytes float64
}

// podUsage tracks one worker pod from first sight to finalization.
type podUsage struct {
	namespace string
	runnerSet string
	// containers holds the running peak per container name; empty until the
	// first PodMetrics sample lands.
	containers map[string]containerPeak
	// finalized flags that the pod's peaks were already folded into the
	// Prometheus series; the entry is kept until the pod object disappears
	// (the reaper holds terminal pods for completedPodTTL) so a later tick
	// cannot double-count it.
	finalized bool
}

// Sampler polls metrics.k8s.io for the worker pods this AGC owns and folds
// per-job usage peaks into the Metrics collectors. It implements
// manager.Runnable; wire it with mgr.Add. It runs on every replica
// (NeedLeaderElection is false, matching the other AGC Runnables — the AGC
// runs one replica per gateway).
//
// Scoping: candidate pods carry provisioner.LabelRunnerSet and are matched
// against the RunnerSets visible in the manager cache. Because the cache is
// namespace-scoped (POD_NAMESPACE) and, under multi-gateway, field-selected to
// this gateway's RunnerSets (GATEWAY_NAME), a sampler never observes — or
// emits series for — another gateway's workers.
type Sampler struct {
	// Client is the manager's cached client (pods and RunnerSets are already
	// informer-backed; the per-tick lists cost no API calls).
	Client client.Client
	// Lister lists PodMetrics; nil disables the sampler (Start returns
	// immediately), so a cluster without metrics-server wiring runs unchanged.
	Lister PodMetricsLister
	// Namespace is the tenant namespace to sample ("" = all cache-visible).
	Namespace string
	// Interval is the polling period; zero defaults to DefaultSampleInterval.
	Interval time.Duration
	// Metrics receives the aggregated series. Must be non-nil when Lister is.
	Metrics *Metrics
	// Log receives sampler lifecycle and throttled error lines.
	Log *slog.Logger
	// Now returns the current time; nil means time.Now. A test seam for the
	// aggregate window timestamps.
	Now func() time.Time

	// mu guards the mutable state below: tick mutates it from the sampler
	// goroutine and SizingStatus reads it from reconcile goroutines (Q359
	// Phase 2). The Kubernetes/metrics API reads in tick happen outside the
	// lock so a slow list can never block a reconcile.
	mu sync.Mutex
	// tracked maps worker pod UID → running usage state.
	tracked map[types.UID]*podUsage
	// seenPeak is the max per-job peak per RunnerSet × container, backing the
	// CPUPeak/MemoryPeak gauges (a gauge alone cannot do a read-modify-max).
	seenPeak map[[3]string]containerPeak
	// aggs accumulates per-RunnerSet per-container peak histograms — the source
	// for status.sizingRecommendation (Q359 Phase 2). Entries are seeded from a
	// RunnerSet's persisted status on first sight (restart merge-back) and
	// dropped when the RunnerSet disappears.
	aggs map[types.NamespacedName]*setAgg
	// consecutivePollErrors throttles the repeated-failure log line.
	consecutivePollErrors int
}

// Start runs the sampling loop until ctx is cancelled. It satisfies
// sigs.k8s.io/controller-runtime/pkg/manager.Runnable.
func (s *Sampler) Start(ctx context.Context) error {
	if s.Lister == nil {
		s.Log.Info("worker usage sampler disabled (no metrics client)")
		return nil
	}
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultSampleInterval
	}
	s.Log.Info("worker usage sampler started", "interval", interval, "namespace", s.Namespace)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// NeedLeaderElection reports that the sampler runs on every replica: each AGC
// owns the worker pods of its own RunnerSets, so there is nothing to elect.
func (s *Sampler) NeedLeaderElection() bool { return false }

// tick performs one sampling pass: refresh running peaks for live worker pods
// and finalize pods that reached a terminal phase or disappeared. The API reads
// happen before the state lock is taken (see the mu comment).
func (s *Sampler) tick(ctx context.Context) {
	var sets agcv2alpha1.RunnerSetList
	if err := s.Client.List(ctx, &sets, client.InNamespace(s.Namespace)); err != nil {
		s.Log.Error("list RunnerSets", "error", err)
		return
	}
	// The RunnerSets this AGC reconciles — the cache scoping (namespace +
	// gateway field selector) makes the list authoritative.
	owned := make(map[types.NamespacedName]bool, len(sets.Items))
	for i := range sets.Items {
		owned[types.NamespacedName{Namespace: sets.Items[i].Namespace, Name: sets.Items[i].Name}] = true
	}

	var pods corev1.PodList
	if err := s.Client.List(ctx, &pods, client.InNamespace(s.Namespace), client.HasLabels{provisioner.LabelRunnerSet}); err != nil {
		s.Log.Error("list worker pods", "error", err)
		return
	}

	// One PodMetrics list serves every running pod this tick. Only pay for it
	// when at least one candidate pod is running.
	var usageByPod map[string][]metricsv1beta1.ContainerMetrics
	if anyRunning(pods.Items, owned) {
		usageByPod = s.listUsage(ctx)
	}

	now := time.Now
	if s.Now != nil {
		now = s.Now
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tracked == nil {
		s.tracked = make(map[types.UID]*podUsage)
		s.seenPeak = make(map[[3]string]containerPeak)
		s.aggs = make(map[types.NamespacedName]*setAgg)
	}

	// Reconcile the aggregate store against the owned RunnerSets: create-and-seed
	// entries for newly seen sets (the restart merge-back — status is the store),
	// drop entries whose RunnerSet is gone.
	for i := range sets.Items {
		rs := &sets.Items[i]
		key := types.NamespacedName{Namespace: rs.Namespace, Name: rs.Name}
		if s.aggs[key] == nil {
			a := &setAgg{containers: make(map[string]*containerAgg)}
			a.seedFromStatus(rs.Status.SizingRecommendation, now())
			s.aggs[key] = a
		}
	}
	for key := range s.aggs {
		if !owned[key] {
			delete(s.aggs, key)
		}
	}

	present := make(map[types.UID]bool, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		set := pod.Labels[provisioner.LabelRunnerSet]
		if !owned[types.NamespacedName{Namespace: pod.Namespace, Name: set}] {
			continue
		}
		present[pod.UID] = true
		track := s.tracked[pod.UID]
		if track == nil {
			track = &podUsage{namespace: pod.Namespace, runnerSet: set, containers: make(map[string]containerPeak)}
			s.tracked[pod.UID] = track
		}
		switch pod.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			s.finalize(track, now())
		default:
			for _, c := range usageByPod[pod.Namespace+"/"+pod.Name] {
				peak := track.containers[c.Name]
				if v := c.Usage.Cpu().AsApproximateFloat64(); v > peak.cpuCores {
					peak.cpuCores = v
				}
				if v := c.Usage.Memory().AsApproximateFloat64(); v > peak.memBytes {
					peak.memBytes = v
				}
				track.containers[c.Name] = peak
			}
		}
	}

	// Pods deleted without a terminal phase we observed (evictions, reaper
	// races): finalize with whatever peaks were captured, then forget them.
	for uid, track := range s.tracked {
		if !present[uid] {
			s.finalize(track, now())
			delete(s.tracked, uid)
		}
	}
}

// SizingStatus returns the current status-shaped sizing recommendation for a
// RunnerSet, or nil when no container has enough sampled jobs yet (or the
// sampler is disabled — a nil *Sampler is safe, mirroring the nil-receiver
// convention of scalesetlistener.Metrics.RecorderFor). The RunnerSet reconciler
// assigns the result to status.sizingRecommendation, which doubles as the
// aggregate persistence the sampler re-seeds from after a restart.
func (s *Sampler) SizingStatus(key types.NamespacedName) []agcv2alpha1.ContainerSizingRecommendation {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.aggs[key]
	if a == nil {
		return nil
	}
	return a.recommendations()
}

// anyRunning reports whether any owned worker pod is in a non-terminal phase.
func anyRunning(pods []corev1.Pod, owned map[types.NamespacedName]bool) bool {
	for i := range pods {
		key := types.NamespacedName{Namespace: pods[i].Namespace, Name: pods[i].Labels[provisioner.LabelRunnerSet]}
		if !owned[key] {
			continue
		}
		switch pods[i].Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
		default:
			return true
		}
	}
	return false
}

// listUsage lists PodMetrics for the namespace, indexed by pod name. A failure
// (metrics-server absent, RBAC denied, transient) counts into PollErrors and
// returns nil — running peaks simply do not advance this tick.
func (s *Sampler) listUsage(ctx context.Context) map[string][]metricsv1beta1.ContainerMetrics {
	pm, err := s.Lister.ListPodMetrics(ctx, s.Namespace)
	if err != nil {
		s.Metrics.PollErrors.WithLabelValues(s.Namespace).Inc()
		s.consecutivePollErrors++
		// Log the first failure of a streak, then throttle: a missing
		// metrics-server would otherwise emit one error line per tick forever.
		if s.consecutivePollErrors == 1 || s.consecutivePollErrors%pollErrorLogEvery == 0 {
			s.Log.Error("list PodMetrics (is metrics-server installed?)",
				"error", err, "consecutiveFailures", s.consecutivePollErrors)
		}
		return nil
	}
	if s.consecutivePollErrors > 0 {
		s.Log.Info("PodMetrics polling recovered", "afterFailures", s.consecutivePollErrors)
		s.consecutivePollErrors = 0
	}
	byPod := make(map[string][]metricsv1beta1.ContainerMetrics, len(pm.Items))
	for i := range pm.Items {
		byPod[pm.Items[i].Namespace+"/"+pm.Items[i].Name] = pm.Items[i].Containers
	}
	return byPod
}

// finalize folds a finished pod's per-container peaks into the Prometheus
// series and the in-memory aggregates, exactly once per pod (idempotent via
// track.finalized — the entry outlives the terminal pod object, which the
// reaper retains for completedPodTTL).
func (s *Sampler) finalize(track *podUsage, now time.Time) {
	if track.finalized {
		return
	}
	track.finalized = true
	if len(track.containers) == 0 {
		s.Metrics.JobsUnsampled.WithLabelValues(track.namespace, track.runnerSet).Inc()
		return
	}
	if a := s.aggs[types.NamespacedName{Namespace: track.namespace, Name: track.runnerSet}]; a != nil {
		a.observePeaks(track.containers, now)
	}
	for name, peak := range track.containers {
		s.Metrics.JobCPUPeak.WithLabelValues(track.namespace, track.runnerSet, name).Observe(peak.cpuCores)
		s.Metrics.JobMemoryPeak.WithLabelValues(track.namespace, track.runnerSet, name).Observe(peak.memBytes)
		key := [3]string{track.namespace, track.runnerSet, name}
		seen := s.seenPeak[key]
		if peak.cpuCores > seen.cpuCores {
			seen.cpuCores = peak.cpuCores
			s.Metrics.CPUPeak.WithLabelValues(track.namespace, track.runnerSet, name).Set(peak.cpuCores)
		}
		if peak.memBytes > seen.memBytes {
			seen.memBytes = peak.memBytes
			s.Metrics.MemoryPeak.WithLabelValues(track.namespace, track.runnerSet, name).Set(peak.memBytes)
		}
		s.seenPeak[key] = seen
	}
	s.Metrics.JobsSampled.WithLabelValues(track.namespace, track.runnerSet).Inc()
}
