package controller

import (
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/gmc/internal/logtest"
)

// proxyProbeEnv, when set, turns the test binary into a NO_PROXY consumer: TestMain
// prints whether net/http would proxy the URL it holds instead of running tests.
// net/http reads the proxy environment once per process, so each NO_PROXY value
// needs a process of its own.
const proxyProbeEnv = "GMC_PROXY_PROBE_URL"

// TestMain fulfills controller-runtime's root logger before any test runs, so
// this binary's output does not depend on how long it takes to finish. See
// package logtest for what fires at the 30-second mark otherwise (Q455).
func TestMain(m *testing.M) {
	if raw := os.Getenv(proxyProbeEnv); raw != "" {
		os.Exit(probeProxy(raw))
	}
	logtest.Install()
	os.Exit(m.Run())
}

func probeProxy(raw string) int {
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		fmt.Println("error", err)
		return 1
	}
	proxy, err := http.ProxyFromEnvironment(req)
	if err != nil {
		fmt.Println("error", err)
		return 1
	}
	if proxy == nil {
		fmt.Println("direct")
	} else {
		fmt.Println("proxied")
	}
	return 0
}
