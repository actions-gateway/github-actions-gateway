//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

// realGitHubEgressLabel marks the specs whose traffic terminates at the live
// api.github.com (rather than the in-cluster fakegithub): the v1/v2 proxy
// CONNECT specs, the direct-egress specs (whose NP ipBlock peers also depend on
// the GMC's live /meta fetch), and the live-GitHub real-dispatch container. It is
// not a filter label — the attribution AfterEach below uses it to decide which
// failures warrant a runner-host GitHub preflight (Q352).
const realGitHubEgressLabel = "real-github-egress"

// runnerHostGitHubPreflight probes https://api.github.com/zen from the process
// running the suite — on CI, the runner host — and scores whether GitHub is
// usable from there right now. The summary it returns alongside the verdict is
// the only record CI keeps of the request, so a non-2xx response carries its
// rate-limit headers and a body excerpt: those name GitHub's own reason for the
// refusal, and they are what resolves an INCONCLUSIVE verdict. Scoring rules and
// their justification: utils.ScoreGitHubEgress.
//
// The status is returned alongside because it is what separates the two layers
// a BLOCKED verdict spans; it is 0 when no response arrived, matching
// ScoreGitHubEgress and utils.GitHubEgressGuidance.
func runnerHostGitHubPreflight() (utils.GitHubEgressVerdict, string, int) {
	client := &http.Client{Timeout: 10 * time.Second}
	start := time.Now()
	resp, err := client.Get("https://api.github.com/zen")
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil {
		return utils.ScoreGitHubEgress(0, err), fmt.Sprintf("transport error after %s: %v", elapsed, err), 0
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	summary := fmt.Sprintf("HTTP %d in %s", resp.StatusCode, elapsed)
	if resp.StatusCode/100 != 2 {
		if h := resp.Header.Get("X-RateLimit-Remaining"); h != "" {
			summary += fmt.Sprintf("; x-ratelimit-remaining=%s", h)
		}
		if h := resp.Header.Get("Retry-After"); h != "" {
			summary += fmt.Sprintf("; retry-after=%s", h)
		}
		summary += fmt.Sprintf("; body: %s", excerpt(body, 200))
	}
	return utils.ScoreGitHubEgress(resp.StatusCode, nil), summary, resp.StatusCode
}

// excerpt collapses b to one whitespace-normalized line of at most limit bytes,
// so a JSON error body cannot spray the failure banner across the CI log.
func excerpt(b []byte, limit int) string {
	r := []rune(strings.Join(strings.Fields(string(b)), " "))
	if len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return string(r)
}

// logRunnerHostGitHubBaseline records runner-host GitHub reachability at suite
// start. Non-fatal by design: a blip at suite start may clear before the
// real-GitHub specs run (their in-pod curls retry for minutes), so failing
// fast here would add a flake surface instead of removing one. The failure-time
// AfterEach below is the authoritative attribution signal.
func logRunnerHostGitHubBaseline() {
	verdict, summary, _ := runnerHostGitHubPreflight()
	if verdict == utils.EgressReachable {
		_, _ = fmt.Fprintf(GinkgoWriter, "runner-host GitHub preflight at suite start: REACHABLE (%s)\n", summary)
		return
	}
	_, _ = fmt.Fprintf(GinkgoWriter,
		"WARNING: runner-host GitHub preflight at suite start: %s (%s) — if this persists, the real-GitHub egress specs will fail on infrastructure, not product (Q352)\n",
		verdict, summary)
}

// When a real-GitHub spec fails, immediately re-probe GitHub from the runner
// host and stamp the verdict into the spec's failure output. The runner host is
// the network segment every in-cluster path NATs through, so GitHub refusing it
// at failure time attributes the failure to egress (observed 2026-07-14 kindnet
// CONNECT 502, 2026-07-19 calico curl 28, and 2026-08-03 an HTTP 403 on the
// probe itself — all rerun-green) instead of leaving it indistinguishable from a
// proxy/NP regression. Registered at top level so it runs after each container's
// own AfterEach diagnostics (Ginkgo orders AfterEach innermost-first).
var _ = AfterEach(func() {
	report := CurrentSpecReport()
	if !report.Failed() || !slices.Contains(report.Labels(), realGitHubEgressLabel) {
		return
	}
	verdict, summary, status := runnerHostGitHubPreflight()
	_, _ = fmt.Fprintf(GinkgoWriter,
		"\n=== RUNNER-HOST GITHUB PREFLIGHT: %s (%s) ===\n%s",
		verdict, summary, utils.GitHubEgressGuidance(verdict, status))
})
