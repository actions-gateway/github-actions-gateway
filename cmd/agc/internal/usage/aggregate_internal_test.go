package usage

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

func TestHistogramQuantileInterpolates(t *testing.T) {
	h := newHistogram(cpuBucketEdges)
	// 20 samples at 0.3 cores → all in the (0.25, 0.5] bucket.
	for range 20 {
		h.observe(cpuBucketEdges, 0.3)
	}
	p95 := h.quantile(cpuBucketEdges, 0.95)
	if p95 <= 0.25 || p95 > 0.3 {
		t.Fatalf("p95 = %v, want in (0.25, 0.3] (interpolated within bucket, clamped to max)", p95)
	}
	if got := h.quantile(cpuBucketEdges, 1); got != 0.3 {
		t.Fatalf("p100 = %v, want the recorded max 0.3", got)
	}
}

func TestHistogramOverflowReportsMax(t *testing.T) {
	h := newHistogram(cpuBucketEdges)
	h.observe(cpuBucketEdges, 42) // above the 16-core top edge
	if got := h.quantile(cpuBucketEdges, 0.95); got != 42 {
		t.Fatalf("overflow quantile = %v, want the recorded max 42", got)
	}
}

func TestHistogramSeedPreservesRecommendationStats(t *testing.T) {
	h := newHistogram(memBucketEdges)
	h.seed(memBucketEdges, 100, 1.5*float64(1<<30), 3*float64(1<<30))
	if h.total != 100 {
		t.Fatalf("total = %d, want 100", h.total)
	}
	if h.max != 3*float64(1<<30) {
		t.Fatalf("max = %v, want 3Gi", h.max)
	}
	// p95 must land in the bucket containing the persisted p95 value (1Gi, 2Gi].
	p95 := h.quantile(memBucketEdges, 0.95)
	if p95 <= float64(1<<30) || p95 > float64(2<<30) {
		t.Fatalf("seeded p95 = %v, want within the (1Gi, 2Gi] bucket", p95)
	}
}

func TestRounding(t *testing.T) {
	if got := roundUpCores(0.301, 50); got.MilliValue() != 350 {
		t.Fatalf("roundUpCores(0.301, 50m) = %v, want 350m", got.String())
	}
	if got := roundUpCores(0, 50); got.MilliValue() != 50 {
		t.Fatalf("roundUpCores floor = %v, want 50m", got.String())
	}
	if got := roundUpBytes(float64(1<<30)+1, memStepBytes); got.Value() != 1<<30+64<<20 {
		t.Fatalf("roundUpBytes = %v, want 1Gi+64Mi", got.String())
	}
}

func TestSetAggRecommendations(t *testing.T) {
	a := &setAgg{containers: make(map[string]*containerAgg)}
	now := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	// 30 jobs peaking at 0.4 cores / 1.5Gi, one outlier at 2 cores / 3Gi.
	for range 30 {
		a.observePeaks(map[string]containerPeak{"runner": {cpuCores: 0.4, memBytes: 1.5 * float64(1<<30)}}, now)
	}
	a.observePeaks(map[string]containerPeak{"runner": {cpuCores: 2, memBytes: 3 * float64(1<<30)}}, now)
	// A container below the recommendation minimum must not appear.
	a.observePeaks(map[string]containerPeak{"sidecar": {cpuCores: 0.1, memBytes: 100 << 20}}, now)

	recs := a.recommendations()
	if len(recs) != 1 || recs[0].Container != "runner" {
		t.Fatalf("recommendations = %+v, want exactly the runner container", recs)
	}
	rec := recs[0]
	if rec.SampleCount != 31 {
		t.Fatalf("SampleCount = %d, want 31", rec.SampleCount)
	}
	if rec.WindowStartTime.Time != now {
		t.Fatalf("WindowStartTime = %v, want %v", rec.WindowStartTime.Time, now)
	}
	// CPU request: p95 of peaks ≈ 0.4-0.5 bucket region → rounded up to a 50m step ≥ 400m.
	cpuReq := rec.Requests[corev1.ResourceCPU]
	if cpuReq.MilliValue() < 400 || cpuReq.MilliValue() > 550 {
		t.Fatalf("cpu request = %v, want within [400m, 550m]", cpuReq.String())
	}
	// Memory limit: max (3Gi) × 1.4 ≈ 4.2Gi, rounded up to 64Mi.
	memLimit := rec.Limits[corev1.ResourceMemory]
	if float64(memLimit.Value()) < 1.4*3*float64(1<<30) {
		t.Fatalf("memory limit = %v, want >= 4.2Gi", memLimit.String())
	}
	if _, hasCPULimit := rec.Limits[corev1.ResourceCPU]; hasCPULimit {
		t.Fatal("recommendation must never include a CPU limit")
	}
	peak := rec.ObservedPeak[corev1.ResourceMemory]
	if peak.Value() != 3<<30 {
		t.Fatalf("observed memory peak = %v, want 3Gi", peak.String())
	}
}

func TestSeedFromStatusRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	windowStart := metav1.NewTime(now.Add(-time.Hour))
	persisted := []agcv2alpha1.ContainerSizingRecommendation{{
		Container:       "runner",
		ObservedP95:     rl("1500m", "1536Mi"),
		ObservedPeak:    rl("2", "3Gi"),
		SampleCount:     40,
		WindowStartTime: windowStart,
	}}

	a := &setAgg{containers: make(map[string]*containerAgg)}
	a.seedFromStatus(persisted, now)

	recs := a.recommendations()
	if len(recs) != 1 {
		t.Fatalf("recommendations after seed = %+v, want 1", recs)
	}
	rec := recs[0]
	if rec.SampleCount != 40 {
		t.Fatalf("SampleCount = %d, want 40", rec.SampleCount)
	}
	if rec.WindowStartTime.Time != windowStart.Time {
		t.Fatalf("WindowStartTime = %v, want persisted %v", rec.WindowStartTime.Time, windowStart.Time)
	}
	// The persisted max must survive exactly; p95 must stay in its bucket.
	peak := rec.ObservedPeak[corev1.ResourceCPU]
	if peak.MilliValue() != 2000 {
		t.Fatalf("observed cpu peak = %v, want 2", peak.String())
	}
	p95 := rec.ObservedP95[corev1.ResourceMemory]
	if v := p95.Value(); v <= 1<<30 || v > 2<<30 {
		t.Fatalf("seeded memory p95 = %v, want within the (1Gi, 2Gi] bucket", p95.String())
	}

	// Live entries beat stale persisted copies: re-seeding must not double-count.
	a.seedFromStatus(persisted, now)
	if got := a.recommendations()[0].SampleCount; got != 40 {
		t.Fatalf("SampleCount after redundant seed = %d, want 40", got)
	}
}

func rl(cpu, mem string) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}
}
