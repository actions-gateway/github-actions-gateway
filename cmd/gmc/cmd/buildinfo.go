package main

import "github.com/prometheus/client_golang/prometheus"

// version is the GMC build version, stamped at link time via
// -ldflags "-X main.version=<tag>" (see cmd/gmc/Dockerfile). It defaults to
// "dev" for un-stamped local builds, following the Prometheus *_build_info
// convention of reporting a placeholder rather than an empty string.
var version = "dev"

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
