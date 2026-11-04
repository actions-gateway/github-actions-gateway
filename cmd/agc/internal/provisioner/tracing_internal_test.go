package provisioner

import (
	"context"
	"errors"
	"net"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/tracing"
)

// The Q1278 contract: each scale-set job is one root span, Provisioner.ProvisionScaleSetWorker,
// with a child span per provisioning phase, so a collector configured through
// spec.tracing receives the job-to-pod path.

const rootSpanName = "Provisioner.ProvisionScaleSetWorker"

// recordSpans points the package tracer at an in-memory recorder for the test's
// duration and returns the recorder.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := tracer
	tracer = tp.Tracer(tracing.InstrumentationName)
	t.Cleanup(func() {
		tracer = prev
		_ = tp.Shutdown(context.Background())
	})
	return sr
}

// spansByName indexes the ended spans by name, failing on a name recorded twice.
func spansByName(t *testing.T, sr *tracetest.SpanRecorder) map[string]sdktrace.ReadOnlySpan {
	t.Helper()
	out := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range sr.Ended() {
		require.NotContains(t, out, s.Name(), "span %q recorded twice for one job", s.Name())
		out[s.Name()] = s
	}
	return out
}

// requireSpan returns the span named name, failing the test when it was not recorded.
func requireSpan(t *testing.T, spans map[string]sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	s, ok := spans[name]
	require.True(t, ok, "span %q not recorded", name)
	return s
}

func spanAttrs(s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	out := map[attribute.Key]attribute.Value{}
	for _, kv := range s.Attributes() {
		out[kv.Key] = kv.Value
	}
	return out
}

func TestProvisionScaleSetWorker_EmitsTheProvisionSpanTree(t *testing.T) {
	sr := recordSpans(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})

	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{JobID: "job-traced", JITConfig: "eyJ4IjoxfQ=="}))

	spans := spansByName(t, sr)
	require.Len(t, spans, 4, "one root span and three phase spans")
	root := requireSpan(t, spans, rootSpanName)
	assert.False(t, root.Parent().IsValid(), "each job is its own root trace")
	assert.Equal(t, codes.Unset, root.Status().Code)

	attrs := spanAttrs(root)
	assert.Equal(t, "team-a", attrs["k8s.namespace.name"].AsString())
	assert.Equal(t, "gpu", attrs["gateway.owner.name"].AsString())
	assert.Equal(t, "job-traced", attrs["gateway.job.id"].AsString())
	assert.Equal(t, scaleSetPodName("gpu", "job-traced"), attrs["k8s.pod.name"].AsString())
	assert.Equal(t, int64(0), attrs["gateway.active_pods"].AsInt64())
	assert.False(t, attrs["gateway.ceiling_held"].AsBool())
	assert.Contains(t, attrs, attribute.Key("gateway.priority_class"))

	for _, name := range []string{"stageJobSecret", "countActivePods", "createPod"} {
		child := requireSpan(t, spans, name)
		assert.Equal(t, root.SpanContext().TraceID(), child.SpanContext().TraceID(), "%s must share the job's trace", name)
		assert.Equal(t, root.SpanContext().SpanID(), child.Parent().SpanID(), "%s must be a child of the job's root span", name)
		assert.Equal(t, codes.Unset, child.Status().Code, "%s succeeded", name)
	}
}

func TestProvisionScaleSetWorker_CeilingHoldMarksTheRootSpan(t *testing.T) {
	sr := recordSpans(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(runningWorkerPod("runner-gpu-existing")).Build()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test", MaxWorkers: int32Ptr(1)})

	require.Error(t, p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{JobID: "job-held", JITConfig: "eyJ4IjoxfQ=="}))

	spans := spansByName(t, sr)
	root := requireSpan(t, spans, rootSpanName)
	assert.Equal(t, codes.Error, root.Status().Code, "a held job must read as failed on its root span")
	attrs := spanAttrs(root)
	assert.True(t, attrs["gateway.ceiling_held"].AsBool())
	assert.Equal(t, int64(1), attrs["gateway.active_pods"].AsInt64())
	assert.NotContains(t, spans, "createPod", "a held job never reaches the pod create")
}

func TestProvisionScaleSetWorker_PodCreateFailureMarksItsSpan(t *testing.T) {
	sr := recordSpans(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					return apierrors.NewInternalError(errors.New("pod rejected"))
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})

	require.Error(t, p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{JobID: "job-nopod", JITConfig: "eyJ4IjoxfQ=="}))

	spans := spansByName(t, sr)
	create := requireSpan(t, spans, "createPod")
	assert.Equal(t, codes.Error, create.Status().Code)
	require.NotEmpty(t, create.Events(), "the create error must be recorded on its span")
	assert.Equal(t, "exception", create.Events()[0].Name)
	assert.Equal(t, codes.Error, requireSpan(t, spans, rootSpanName).Status().Code)
}

// A replay meeting its own live pod adopts it, which is not a failed create.
func TestProvisionScaleSetWorker_ReplayAdoptionIsNotASpanError(t *testing.T) {
	sr := recordSpans(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(runningWorkerPod(scaleSetPodName("gpu", "job-replay"))).Build()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})

	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{JobID: "job-replay", JITConfig: "eyJ4IjoxfQ=="}))

	spans := spansByName(t, sr)
	assert.Equal(t, codes.Unset, requireSpan(t, spans, "createPod").Status().Code)
	assert.Equal(t, codes.Unset, requireSpan(t, spans, rootSpanName).Status().Code)
}

// otlpSink is an OTLP/gRPC trace receiver that records the span names it is sent.
type otlpSink struct {
	coltrace.UnimplementedTraceServiceServer
	mu    sync.Mutex
	names []string
}

func (s *otlpSink) Export(_ context.Context, req *coltrace.ExportTraceServiceRequest) (*coltrace.ExportTraceServiceResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				s.names = append(s.names, span.GetName())
			}
		}
	}
	return &coltrace.ExportTraceServiceResponse{}, nil
}

// The recorder tests above replace the tracer, so they cannot see a break between
// tracing.Init and spans leaving the process. This one runs that path to a real OTLP/gRPC
// server. It re-reads the tracer from the global because the package tracer delegates only
// to the first provider ever set, which would fail under -count=2.
func TestProvisionScaleSetWorker_DeliversSpansOverOTLP(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	sink := &otlpSink{}
	srv := grpc.NewServer()
	coltrace.RegisterTraceServiceServer(srv, sink)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	// A bare host:port would fail the SDK's URL parse and silently dial localhost:4317.
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://"+lis.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_INSECURE", "true")

	ctx := context.Background()
	shutdown, enabled, err := tracing.Init(ctx, "test", nil)
	require.NoError(t, err)
	require.True(t, enabled, "an endpoint in the environment must enable tracing")
	prev := tracer
	tracer = otel.Tracer(tracing.InstrumentationName)
	t.Cleanup(func() { tracer = prev })

	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})
	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{JobID: "job-otlp", JITConfig: "eyJ4IjoxfQ=="}))

	flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	require.NoError(t, shutdown(flushCtx))

	sink.mu.Lock()
	defer sink.mu.Unlock()
	sort.Strings(sink.names)
	assert.Equal(t, []string{rootSpanName, "countActivePods", "createPod", "stageJobSecret"}, sink.names,
		"the collector must receive the job's whole span tree")
}
