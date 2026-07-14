package main

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestRegisterBuildInfo asserts the AGC registers actions_gateway_build_info as
// a constant-1 gauge carrying the component and stamped version (Q318).
func TestRegisterBuildInfo(t *testing.T) {
	reg := prometheus.NewRegistry()
	registerBuildInfo(reg, "agc", "v1.2.3")

	const want = `
# HELP actions_gateway_build_info Build metadata of the running binary as a constant 1, labelled by component and version (Prometheus *_build_info convention).
# TYPE actions_gateway_build_info gauge
actions_gateway_build_info{component="agc",version="v1.2.3"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "actions_gateway_build_info"); err != nil {
		t.Errorf("build_info metric mismatch: %v", err)
	}
}

// TestVersionDefault documents that an un-stamped build reports the "dev"
// placeholder rather than an empty version label.
func TestVersionDefault(t *testing.T) {
	if version == "" {
		t.Error("version must default to a non-empty placeholder, got empty string")
	}
}
