package main

import "github.com/prometheus/client_golang/prometheus"

// The build version reported here is main.version (declared in main.go, also
// used as the OpenTelemetry service.version); it is stamped at link time via
// -ldflags "-X main.version=<tag>" (see the root Dockerfile's build-agc stage) and defaults to
// "dev" for un-stamped local builds.

// registerBuildInfo registers the actions_gateway_build_info gauge on reg. The
// gauge is a constant 1 labelled by component and version, following the
// Prometheus *_build_info convention, so an operator can correlate the running
// binary version straight from metrics during an incident — the worker pods'
// app.kubernetes.io/version label does not reach the control-plane series
// (Q318).
func registerBuildInfo(reg prometheus.Registerer, component, version string) {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "actions_gateway_build_info",
		Help: "Build metadata of the running binary as a constant 1, labelled by component and version (Prometheus *_build_info convention).",
	}, []string{"component", "version"})
	g.WithLabelValues(component, version).Set(1)
	reg.MustRegister(g)
}

// q594 probe touch
