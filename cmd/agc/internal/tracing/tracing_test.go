package tracing

import (
	"context"
	"testing"
)

// TestInit_DisabledByDefault verifies that with no OTLP endpoint configured the
// AGC ships with tracing off: Init reports enabled=false, returns no error, and
// hands back a usable no-op shutdown.
func TestInit_DisabledByDefault(t *testing.T) {
	// t.Setenv with empty values both isolates the test from the ambient
	// environment and restores it afterwards.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_SDK_DISABLED", "")

	shutdown, enabled, err := Init(context.Background(), "test", nil)
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	if enabled {
		t.Fatal("expected tracing disabled when no OTLP endpoint is configured")
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown even when disabled")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown returned error: %v", err)
	}
}

// TestInit_DisabledKillSwitch verifies OTEL_SDK_DISABLED=true wins even when an
// endpoint is configured.
func TestInit_DisabledKillSwitch(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	t.Setenv("OTEL_SDK_DISABLED", "true")

	_, enabled, err := Init(context.Background(), "test", nil)
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	if enabled {
		t.Fatal("expected tracing disabled when OTEL_SDK_DISABLED=true")
	}
}

// TestInit_EnabledWithEndpoint verifies that configuring an OTLP endpoint opts
// tracing in: Init installs an exporter (enabled=true) and the returned shutdown
// flushes and stops it cleanly. otlptracegrpc.New does not dial eagerly, so this
// does not require a live collector.
func TestInit_EnabledWithEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "localhost:4317")
	t.Setenv("OTEL_SDK_DISABLED", "")

	shutdown, enabled, err := Init(context.Background(), "test", nil)
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	if !enabled {
		t.Fatal("expected tracing enabled when an OTLP endpoint is configured")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // a cancelled context bounds Shutdown's flush attempt
		_ = shutdown(ctx)
	})
}

// TestTraceExportConfigured covers the endpoint/kill-switch precedence directly.
func TestTraceExportConfigured(t *testing.T) {
	tests := []struct {
		name           string
		endpoint       string
		tracesEndpoint string
		disabled       string
		want           bool
	}{
		{name: "nothing set", want: false},
		{name: "general endpoint", endpoint: "localhost:4317", want: true},
		{name: "traces endpoint", tracesEndpoint: "localhost:4317", want: true},
		{name: "disabled overrides endpoint", endpoint: "localhost:4317", disabled: "true", want: false},
		{name: "disabled mixed case", endpoint: "localhost:4317", disabled: "TRUE", want: false},
		{name: "disabled false is on", endpoint: "localhost:4317", disabled: "false", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", tc.endpoint)
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", tc.tracesEndpoint)
			t.Setenv("OTEL_SDK_DISABLED", tc.disabled)
			if got := traceExportConfigured(); got != tc.want {
				t.Errorf("traceExportConfigured() = %v, want %v", got, tc.want)
			}
		})
	}
}
