// Package tracing wires OpenTelemetry distributed tracing for the AGC.
//
// Tracing is opt-in and off by default: Init only installs an exporter when an
// OTLP endpoint is configured via the standard OpenTelemetry environment
// variables (OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT),
// and never when OTEL_SDK_DISABLED=true. When tracing is disabled the global
// OpenTelemetry provider stays the no-op default, so the otel.Tracer(...).Start
// calls sprinkled through the provisioner and reconciler are nearly free and the
// AGC ships with tracing off unless an operator points it at a collector.
//
// All exporter knobs (endpoint, headers, TLS, timeout, sampling, resource
// attributes) are read from the OpenTelemetry environment variables the SDK
// already understands, so there is no bespoke flag surface to learn — see
// docs/operations/observability.md.
package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// ServiceName is the default OpenTelemetry service.name reported by AGC spans.
// Operators may override it with OTEL_SERVICE_NAME (or service.name in
// OTEL_RESOURCE_ATTRIBUTES), which takes precedence over this default.
const ServiceName = "actions-gateway-agc"

// InstrumentationName is the OpenTelemetry instrumentation scope shared by the
// AGC's own spans. Packages obtain their tracer with otel.Tracer(InstrumentationName).
const InstrumentationName = "github.com/actions-gateway/github-actions-gateway/agc"

// Init configures the global OpenTelemetry tracer provider with an OTLP/gRPC
// exporter when an OTLP endpoint is configured and the SDK is not disabled. It
// returns a shutdown function that flushes and stops the exporter (callers
// should defer it) and whether tracing was enabled. When tracing is disabled it
// returns a no-op shutdown and enabled=false, leaving the global no-op provider
// in place.
//
// version is stamped as service.version; pass the binary's build version.
func Init(ctx context.Context, version string, log *slog.Logger) (shutdown func(context.Context) error, enabled bool, err error) {
	noop := func(context.Context) error { return nil }
	if !traceExportConfigured() {
		return noop, false, nil
	}

	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return noop, false, fmt.Errorf("create OTLP trace exporter: %w", err)
	}

	// resource.New merges later options over earlier ones on key conflicts, so
	// listing WithAttributes (our defaults) before WithFromEnv lets an operator's
	// OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES override the defaults. A
	// returned error is non-fatal (e.g. a schema-URL or partial-detection
	// warning); the resource is still usable, so log and proceed.
	res, resErr := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(ServiceName),
			semconv.ServiceVersion(version),
		),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if resErr != nil && log != nil {
		log.Warn("partial OpenTelemetry resource; continuing", "error", resErr)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	// W3C trace-context + baggage propagation so spans correlate across any
	// future inbound/outbound hops that carry the standard headers.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return tp.Shutdown, true, nil
}

// traceExportConfigured reports whether the operator has opted into tracing by
// pointing the SDK at an OTLP collector. It mirrors the OpenTelemetry SDK's own
// endpoint precedence (the traces-specific endpoint or the general one) while
// honouring the OTEL_SDK_DISABLED kill switch. Gating on an explicit endpoint is
// what keeps tracing off by default: otlptracegrpc.New would otherwise silently
// default to localhost:4317 and try to export on every span.
func traceExportConfigured() bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")), "true") {
		return false
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
}
