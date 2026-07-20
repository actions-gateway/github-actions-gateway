//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
)

// realGitHubEgressLabel marks the specs whose traffic terminates at the live
// api.github.com (rather than the in-cluster fakegithub): the v1/v2 proxy
// CONNECT specs, the direct-egress specs (whose NP ipBlock peers also depend on
// the GMC's live /meta fetch), and the Tier C real-dispatch container. It is
// not a filter label — the attribution AfterEach below uses it to decide which
// failures warrant a runner-host GitHub preflight (Q352).
const realGitHubEgressLabel = "real-github-egress"

// runnerHostGitHubPreflight probes https://api.github.com/zen from the process
// running the suite — on CI, the runner host — and reports whether GitHub is
// reachable from there right now. Any HTTP response (200, a rate-limit 403,
// even a 5xx) proves the egress path is up; only a transport-level error (DNS,
// dial timeout, TLS) means the host's GitHub egress is down.
func runnerHostGitHubPreflight() (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	start := time.Now()
	resp, err := client.Get("https://api.github.com/zen")
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return fmt.Sprintf("HTTP %d in %s", resp.StatusCode, time.Since(start).Round(time.Millisecond)), nil
}

// logRunnerHostGitHubBaseline records runner-host GitHub reachability at suite
// start. Non-fatal by design: a blip at suite start may clear before the
// real-GitHub specs run (their in-pod curls retry for minutes), so failing
// fast here would add a flake surface instead of removing one. The failure-time
// AfterEach below is the authoritative attribution signal.
func logRunnerHostGitHubBaseline() {
	if summary, err := runnerHostGitHubPreflight(); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter,
			"WARNING: runner-host GitHub preflight failed at suite start: %v — if this persists, the real-GitHub egress specs will fail on infrastructure, not product (Q352)\n", err)
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "runner-host GitHub preflight at suite start: OK (%s)\n", summary)
	}
}

// When a real-GitHub spec fails, immediately re-probe GitHub from the runner
// host and stamp the verdict into the spec's failure output. The runner host is
// the network segment every in-cluster path NATs through, so an unreachable
// GitHub here at failure time attributes the failure to an egress blip
// (observed 2026-07-14 kindnet CONNECT 502 and 2026-07-19 calico curl 28 —
// both rerun-green) instead of leaving it indistinguishable from a proxy/NP
// regression. Registered at top level so it runs after each container's own
// AfterEach diagnostics (Ginkgo orders AfterEach innermost-first).
var _ = AfterEach(func() {
	report := CurrentSpecReport()
	if !report.Failed() || !slices.Contains(report.Labels(), realGitHubEgressLabel) {
		return
	}
	if summary, err := runnerHostGitHubPreflight(); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter,
			"\n=== RUNNER-HOST GITHUB PREFLIGHT: FAILED (%v) ===\n"+
				"The runner host itself cannot reach https://api.github.com at failure time, so this\n"+
				"spec's failure is attributable to a runner->GitHub egress blip — infrastructure, not\n"+
				"a product regression (Q352). Re-run the job.\n", err)
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter,
			"\n=== RUNNER-HOST GITHUB PREFLIGHT: OK (%s) ===\n"+
				"The runner host reaches https://api.github.com at failure time, so a host-level egress\n"+
				"blip is unlikely — treat this failure as real and inspect the in-cluster path\n"+
				"(workload NP -> proxy -> egress NP -> GitHub).\n", summary)
	}
})
