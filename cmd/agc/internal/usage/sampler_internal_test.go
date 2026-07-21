package usage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

const testNS = "tenant-a"

// fakeLister serves a scripted PodMetricsList (or error) per call.
type fakeLister struct {
	list *metricsv1beta1.PodMetricsList
	err  error
}

func (f *fakeLister) ListPodMetrics(context.Context, string) (*metricsv1beta1.PodMetricsList, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.list == nil {
		return &metricsv1beta1.PodMetricsList{}, nil
	}
	return f.list, nil
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := agcv2alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func runnerSet(name string) *agcv2alpha1.RunnerSet {
	return &agcv2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name}}
}

func workerPod(name, set string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNS,
			Name:      name,
			UID:       types.UID("uid-" + name),
			Labels:    map[string]string{provisioner.LabelRunnerSet: set},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func podMetrics(pod string, cpu, mem string) metricsv1beta1.PodMetrics {
	return metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: pod},
		Containers: []metricsv1beta1.ContainerMetrics{{
			Name: "runner",
			Usage: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
			},
		}},
	}
}

func newTestSampler(t *testing.T, lister PodMetricsLister, objs ...client.Object) (*Sampler, client.Client) {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
	return &Sampler{
		Client:    cl,
		Lister:    lister,
		Namespace: testNS,
		Metrics:   NewMetrics(prometheus.NewRegistry()),
		Log:       slog.Default(),
	}, cl
}

// TestSamplerPeakAcrossTicksAndFinalize verifies the core loop: the per-job
// peak is the max across ticks, folded into gauges/histograms/counters exactly
// once when the pod goes terminal — and not re-counted while the terminal pod
// object lingers (the reaper retains it for completedPodTTL).
func TestSamplerPeakAcrossTicksAndFinalize(t *testing.T) {
	lister := &fakeLister{list: &metricsv1beta1.PodMetricsList{Items: []metricsv1beta1.PodMetrics{
		podMetrics("w1", "1500m", "2Gi"),
	}}}
	pod := workerPod("w1", "rs1", corev1.PodRunning)
	s, cl := newTestSampler(t, lister, runnerSet("rs1"), pod)
	ctx := context.Background()

	s.tick(ctx) // peak: 1.5 cores / 2Gi
	// Second tick reports lower usage; the tracked peak must not regress.
	lister.list = &metricsv1beta1.PodMetricsList{Items: []metricsv1beta1.PodMetrics{
		podMetrics("w1", "500m", "1Gi"),
	}}
	s.tick(ctx)

	if got := testutil.ToFloat64(s.Metrics.JobsSampled.WithLabelValues(testNS, "rs1")); got != 0 {
		t.Fatalf("job sampled before terminal phase: %v", got)
	}

	pod.Status.Phase = corev1.PodSucceeded
	if err := cl.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	s.tick(ctx)
	s.tick(ctx) // terminal pod still present — must not double-count

	if got := testutil.ToFloat64(s.Metrics.JobsSampled.WithLabelValues(testNS, "rs1")); got != 1 {
		t.Fatalf("JobsSampled = %v, want 1", got)
	}
	if got := testutil.ToFloat64(s.Metrics.CPUPeak.WithLabelValues(testNS, "rs1", "runner")); got != 1.5 {
		t.Fatalf("CPUPeak = %v, want 1.5", got)
	}
	wantMem := float64(2 * 1024 * 1024 * 1024)
	if got := testutil.ToFloat64(s.Metrics.MemoryPeak.WithLabelValues(testNS, "rs1", "runner")); got != wantMem {
		t.Fatalf("MemoryPeak = %v, want %v", got, wantMem)
	}
	if got := testutil.CollectAndCount(s.Metrics.JobCPUPeak); got != 1 {
		t.Fatalf("JobCPUPeak series = %d, want 1", got)
	}
}

// TestSamplerUnsampledShortJob verifies a pod first seen already terminal (a
// job shorter than one sampling interval) counts as unsampled, not into the
// peak histograms.
func TestSamplerUnsampledShortJob(t *testing.T) {
	s, _ := newTestSampler(t, &fakeLister{}, runnerSet("rs1"), workerPod("w1", "rs1", corev1.PodSucceeded))
	s.tick(context.Background())

	if got := testutil.ToFloat64(s.Metrics.JobsUnsampled.WithLabelValues(testNS, "rs1")); got != 1 {
		t.Fatalf("JobsUnsampled = %v, want 1", got)
	}
	if got := testutil.ToFloat64(s.Metrics.JobsSampled.WithLabelValues(testNS, "rs1")); got != 0 {
		t.Fatalf("JobsSampled = %v, want 0", got)
	}
}

// TestSamplerFinalizesDisappearedPod verifies a pod deleted while running
// (eviction, reaper race) is finalized with the peaks captured so far.
func TestSamplerFinalizesDisappearedPod(t *testing.T) {
	lister := &fakeLister{list: &metricsv1beta1.PodMetricsList{Items: []metricsv1beta1.PodMetrics{
		podMetrics("w1", "2", "1Gi"),
	}}}
	pod := workerPod("w1", "rs1", corev1.PodRunning)
	s, cl := newTestSampler(t, lister, runnerSet("rs1"), pod)
	ctx := context.Background()

	s.tick(ctx)
	if err := cl.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	s.tick(ctx)

	if got := testutil.ToFloat64(s.Metrics.JobsSampled.WithLabelValues(testNS, "rs1")); got != 1 {
		t.Fatalf("JobsSampled = %v, want 1", got)
	}
	if got := testutil.ToFloat64(s.Metrics.CPUPeak.WithLabelValues(testNS, "rs1", "runner")); got != 2 {
		t.Fatalf("CPUPeak = %v, want 2", got)
	}
	if len(s.tracked) != 0 {
		t.Fatalf("tracked not cleaned up: %d entries", len(s.tracked))
	}
}

// TestSamplerIgnoresUnownedPods verifies pods labeled for a RunnerSet this AGC
// does not reconcile (another gateway's set under multi-gateway scoping) are
// never tracked or counted.
func TestSamplerIgnoresUnownedPods(t *testing.T) {
	s, _ := newTestSampler(t, &fakeLister{},
		runnerSet("rs1"),
		workerPod("other", "not-mine", corev1.PodSucceeded))
	s.tick(context.Background())

	if got := testutil.ToFloat64(s.Metrics.JobsUnsampled.WithLabelValues(testNS, "not-mine")); got != 0 {
		t.Fatalf("unowned pod was counted: %v", got)
	}
	if len(s.tracked) != 0 {
		t.Fatalf("unowned pod was tracked")
	}
}

// TestSamplerPollErrorCounted verifies a metrics API failure increments
// PollErrors, does not crash the tick, and running peaks resume after
// recovery.
func TestSamplerPollErrorCounted(t *testing.T) {
	lister := &fakeLister{err: errors.New("the server could not find the requested resource")}
	pod := workerPod("w1", "rs1", corev1.PodRunning)
	s, cl := newTestSampler(t, lister, runnerSet("rs1"), pod)
	ctx := context.Background()

	s.tick(ctx)
	if got := testutil.ToFloat64(s.Metrics.PollErrors.WithLabelValues(testNS)); got != 1 {
		t.Fatalf("PollErrors = %v, want 1", got)
	}

	lister.err = nil
	lister.list = &metricsv1beta1.PodMetricsList{Items: []metricsv1beta1.PodMetrics{
		podMetrics("w1", "1", "1Gi"),
	}}
	s.tick(ctx)
	pod.Status.Phase = corev1.PodSucceeded
	if err := cl.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	s.tick(ctx)
	if got := testutil.ToFloat64(s.Metrics.JobsSampled.WithLabelValues(testNS, "rs1")); got != 1 {
		t.Fatalf("JobsSampled after recovery = %v, want 1", got)
	}
}

// TestSamplerDisabledWithoutLister verifies a nil Lister makes Start a no-op
// (metrics-server-less clusters run unchanged).
func TestSamplerDisabledWithoutLister(t *testing.T) {
	s := &Sampler{Log: slog.Default()}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start with nil lister: %v", err)
	}
}

// TestSamplerSizingStatusAndRestartSeed exercises the Phase 2 loop end to end
// at the sampler level: finished jobs accumulate into a status-shaped
// recommendation, and a fresh sampler (an AGC restart) re-seeds its aggregates
// from the persisted RunnerSet status so the history is not zeroed.
func TestSamplerSizingStatusAndRestartSeed(t *testing.T) {
	rs := runnerSet("rs1")
	pods := make([]client.Object, 0, 6)
	pods = append(pods, rs)
	items := make([]metricsv1beta1.PodMetrics, 0, 5)
	for i := range 5 {
		name := fmt.Sprintf("w%d", i)
		pods = append(pods, workerPod(name, "rs1", corev1.PodRunning))
		items = append(items, podMetrics(name, "1", "1Gi"))
	}
	lister := &fakeLister{list: &metricsv1beta1.PodMetricsList{Items: items}}
	s, cl := newTestSampler(t, lister, pods...)
	ctx := context.Background()
	key := types.NamespacedName{Namespace: testNS, Name: "rs1"}

	s.tick(ctx) // capture peaks
	if got := s.SizingStatus(key); got != nil {
		t.Fatalf("SizingStatus before any job finished = %+v, want nil", got)
	}
	for i := range 5 {
		pod := workerPod(fmt.Sprintf("w%d", i), "rs1", corev1.PodSucceeded)
		if err := cl.Status().Update(ctx, pod); err != nil {
			t.Fatal(err)
		}
	}
	s.tick(ctx) // finalize all five

	recs := s.SizingStatus(key)
	if len(recs) != 1 || recs[0].SampleCount != 5 {
		t.Fatalf("SizingStatus = %+v, want one container with 5 samples", recs)
	}

	// "Restart": a fresh sampler over a cluster where the pods are long gone but
	// the RunnerSet's status carries the persisted recommendation.
	rs2 := runnerSet("rs1")
	rs2.Status.SizingRecommendation = recs
	s2, _ := newTestSampler(t, &fakeLister{}, rs2)
	s2.tick(ctx)
	seeded := s2.SizingStatus(key)
	if len(seeded) != 1 || seeded[0].SampleCount != 5 {
		t.Fatalf("SizingStatus after restart seed = %+v, want the persisted 5-sample history", seeded)
	}

	// A nil sampler (usage sampling disabled) is a safe no-op source.
	var disabled *Sampler
	if got := disabled.SizingStatus(key); got != nil {
		t.Fatalf("nil sampler SizingStatus = %+v, want nil", got)
	}
}
