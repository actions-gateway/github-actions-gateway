package usage

import (
	"math"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// Confidence minimums for the status-surfaced sizing data (Q359 Phase 2).
const (
	// MinSamplesForRecommendation is the fewest sampled jobs a container needs
	// before a recommendation appears in RunnerSet status at all — below this a
	// p95 is mostly noise.
	MinSamplesForRecommendation = 5
	// MinSamplesForDrift is the fewest sampled jobs before the reconciler judges
	// the template's ask against the recommendation (the SizingDrift condition).
	// Higher than MinSamplesForRecommendation: showing a low-confidence number is
	// fine, alarming on one is not.
	MinSamplesForDrift = 20
)

// memLimitHeadroom is the multiplier over the observed maximum per-job memory
// peak used for the recommended memory limit — the OOM-headroom factor the
// dogfood right-sizing exercise validated (peak × 1.3–1.4; the top of that
// band, since exceeding the limit kills the job).
const memLimitHeadroom = 1.4

// Rounding steps for recommended values: sizing is bucket-granular, and coarse
// increments keep the status stable across small distribution shifts.
const (
	cpuRequestStepMilli = 50
	memStepBytes        = 64 << 20 // 64Mi
	observedMemStep     = 1 << 20  // 1Mi, for observed (non-recommendation) values
)

// Bucket edges for the per-job peak histograms — shared between the Prometheus
// export (metrics.go) and the in-memory aggregates that feed RunnerSet status,
// so the two views can never disagree on granularity.
var (
	// cpuBucketEdges spans near-idle sidecars through a full large CI node
	// (cores). Peaks above the last edge are tracked via the recorded maximum.
	cpuBucketEdges = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16}
	// memBucketEdges spans a trivial lint job through a large build/test worker
	// (bytes): 128Mi → 64Gi, doubling.
	memBucketEdges = []float64{
		128 << 20, 256 << 20, 512 << 20,
		1 << 30, 2 << 30, 4 << 30, 8 << 30, 16 << 30, 32 << 30, 64 << 30,
	}
)

// histogram is a fixed-bucket histogram of per-job peaks with an exact recorded
// maximum. Bounded memory, mergeable, and cheap to seed approximately from a
// persisted (count, p95, max) triple.
type histogram struct {
	// counts has len(edges)+1 entries; the last is the above-top-edge overflow.
	counts []uint64
	total  uint64
	max    float64
}

func newHistogram(edges []float64) histogram {
	return histogram{counts: make([]uint64, len(edges)+1)}
}

// bucketIndex returns the index of the bucket v falls into (last = overflow).
func bucketIndex(edges []float64, v float64) int {
	for i, e := range edges {
		if v <= e {
			return i
		}
	}
	return len(edges)
}

func (h *histogram) observe(edges []float64, v float64) {
	h.counts[bucketIndex(edges, v)]++
	h.total++
	if v > h.max {
		h.max = v
	}
}

// quantile returns the q-quantile (0 < q ≤ 1) by linear interpolation within
// the containing bucket, clamped to the recorded maximum. The overflow bucket
// has no upper edge, so any rank landing there reports the maximum.
func (h *histogram) quantile(edges []float64, q float64) float64 {
	if h.total == 0 {
		return 0
	}
	rank := q * float64(h.total)
	cum := 0.0
	for i, c := range h.counts {
		prev := cum
		cum += float64(c)
		if cum < rank || c == 0 {
			continue
		}
		if i == len(edges) {
			return h.max
		}
		lo := 0.0
		if i > 0 {
			lo = edges[i-1]
		}
		v := lo + (rank-prev)/float64(c)*(edges[i]-lo)
		return math.Min(v, h.max)
	}
	return h.max
}

// seed approximately reconstructs a persisted distribution from its (count,
// p95, max) summary: 95% of the mass at the p95 value, the rest at the max.
// The reconstruction preserves exactly the statistics the recommendation uses
// (p95 and max), which is what makes RunnerSet status a sufficient store — the
// full distribution below p95 is intentionally not persisted.
func (h *histogram) seed(edges []float64, count uint64, p95, max float64) {
	if count == 0 {
		return
	}
	n95 := uint64(float64(count)*0.95 + 0.5)
	if n95 > count {
		n95 = count
	}
	h.counts[bucketIndex(edges, p95)] += n95
	h.counts[bucketIndex(edges, max)] += count - n95
	h.total += count
	if max > h.max {
		h.max = max
	}
}

// containerAgg accumulates one RunnerSet container's per-job peak history.
type containerAgg struct {
	cpu         histogram
	mem         histogram
	windowStart time.Time
}

func newContainerAgg(start time.Time) *containerAgg {
	return &containerAgg{
		cpu:         newHistogram(cpuBucketEdges),
		mem:         newHistogram(memBucketEdges),
		windowStart: start,
	}
}

// setAgg is the per-RunnerSet aggregate store.
type setAgg struct {
	containers map[string]*containerAgg
}

// observePeaks folds one finished pod's per-container peaks into the aggregate.
func (a *setAgg) observePeaks(containers map[string]containerPeak, now time.Time) {
	for name, peak := range containers {
		c := a.containers[name]
		if c == nil {
			c = newContainerAgg(now)
			a.containers[name] = c
		}
		c.cpu.observe(cpuBucketEdges, peak.cpuCores)
		c.mem.observe(memBucketEdges, peak.memBytes)
	}
}

// seedFromStatus reconstructs the aggregate from a persisted RunnerSet
// status.sizingRecommendation — the restart merge-back path (status is the
// store). Entries already present in memory are never overwritten: live
// observation beats a stale persisted copy.
func (a *setAgg) seedFromStatus(recs []agcv2alpha1.ContainerSizingRecommendation, now time.Time) {
	for _, rec := range recs {
		if rec.SampleCount <= 0 || a.containers[rec.Container] != nil {
			continue
		}
		start := now
		if !rec.WindowStartTime.IsZero() {
			start = rec.WindowStartTime.Time
		}
		c := newContainerAgg(start)
		c.cpu.seed(cpuBucketEdges, uint64(rec.SampleCount),
			approxFloat(rec.ObservedP95, corev1.ResourceCPU), approxFloat(rec.ObservedPeak, corev1.ResourceCPU))
		c.mem.seed(memBucketEdges, uint64(rec.SampleCount),
			approxFloat(rec.ObservedP95, corev1.ResourceMemory), approxFloat(rec.ObservedPeak, corev1.ResourceMemory))
		a.containers[rec.Container] = c
	}
}

// recommendations renders the aggregate as the status-shaped recommendation
// list: one entry per container with at least MinSamplesForRecommendation
// samples, sorted by container name for deterministic status writes.
func (a *setAgg) recommendations() []agcv2alpha1.ContainerSizingRecommendation {
	var recs []agcv2alpha1.ContainerSizingRecommendation
	for name, c := range a.containers {
		if c.cpu.total < MinSamplesForRecommendation {
			continue
		}
		cpuP95 := c.cpu.quantile(cpuBucketEdges, 0.95)
		memP95 := c.mem.quantile(memBucketEdges, 0.95)
		recs = append(recs, agcv2alpha1.ContainerSizingRecommendation{
			Container: name,
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    roundUpCores(cpuP95, cpuRequestStepMilli),
				corev1.ResourceMemory: roundUpBytes(memP95, memStepBytes),
			},
			// Memory only — a CPU limit would throttle bursty jobs for no
			// packing benefit (see the type's doc comment).
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: roundUpBytes(c.mem.max*memLimitHeadroom, memStepBytes),
			},
			ObservedPeak: corev1.ResourceList{
				corev1.ResourceCPU:    roundUpCores(c.cpu.max, 1),
				corev1.ResourceMemory: roundUpBytes(c.mem.max, observedMemStep),
			},
			ObservedP95: corev1.ResourceList{
				corev1.ResourceCPU:    roundUpCores(cpuP95, 1),
				corev1.ResourceMemory: roundUpBytes(memP95, observedMemStep),
			},
			SampleCount:     int64(c.cpu.total),
			WindowStartTime: metav1.NewTime(c.windowStart),
		})
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Container < recs[j].Container })
	return recs
}

// roundUpCores converts cores to a milli-CPU Quantity rounded up to stepMilli
// (never below one step).
func roundUpCores(cores float64, stepMilli int64) resource.Quantity {
	milli := int64(math.Ceil(cores * 1000))
	milli = (milli + stepMilli - 1) / stepMilli * stepMilli
	if milli < stepMilli {
		milli = stepMilli
	}
	return *resource.NewMilliQuantity(milli, resource.DecimalSI)
}

// roundUpBytes converts bytes to a Quantity rounded up to step (never below
// one step).
func roundUpBytes(bytes float64, step int64) resource.Quantity {
	b := int64(math.Ceil(bytes))
	b = (b + step - 1) / step * step
	if b < step {
		b = step
	}
	return *resource.NewQuantity(b, resource.BinarySI)
}

// approxFloat reads a resource value from a list as a float64 (0 when absent).
func approxFloat(rl corev1.ResourceList, name corev1.ResourceName) float64 {
	q, ok := rl[name]
	if !ok {
		return 0
	}
	return q.AsApproximateFloat64()
}
