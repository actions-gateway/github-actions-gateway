package utils

// GitHubEgressVerdict is what a runner-host probe of api.github.com says about
// the runner's own GitHub egress at the moment it ran. It answers one question
// for a failed `real-github-egress` spec: could the runner have reached GitHub,
// or was it refused? See docs/development/testing.md § Runner→GitHub egress
// attribution (Q352).
type GitHubEgressVerdict int

const (
	// EgressBlocked — the runner could not get a usable answer out of GitHub, so
	// the in-cluster path (which NATs through the same address) could not either.
	// The spec failure is infrastructure; re-run the job.
	//
	// Two disjoint routes reach this verdict and they support opposite
	// inferences about the dial layer: a transport error means nothing answered,
	// while an HTTP refusal arrived over a completed TCP connection.
	// GitHubEgressGuidance splits on that; the verdict token deliberately does
	// not, so the banner operators grep for stays one string.
	EgressBlocked GitHubEgressVerdict = iota

	// EgressReachable — GitHub served the probe, so a host-level egress blip is
	// unlikely. Treat the spec failure as real.
	EgressReachable

	// EgressInconclusive — something answered, but not the way GitHub answers an
	// unauthenticated /zen. The probe cannot attribute the failure on its own.
	EgressInconclusive
)

// String renders the verdict as the token stamped into the CI failure banner.
func (v GitHubEgressVerdict) String() string {
	switch v {
	case EgressBlocked:
		return "BLOCKED"
	case EgressReachable:
		return "REACHABLE"
	case EgressInconclusive:
		return "INCONCLUSIVE"
	default:
		return "UNKNOWN"
	}
}

// ScoreGitHubEgress classifies one probe of https://api.github.com/zen: status
// is the HTTP status code (0 when no response arrived) and err the transport
// error, if any.
//
// The probe goes straight from the test process to GitHub — no proxy, no
// cluster, nothing this repo ships — so its result can never be caused by a
// product regression. It shares exactly two things with the in-cluster path:
// the runner's egress address and the internet between it and GitHub. Anything
// that refuses the probe therefore refuses the traffic the spec under test
// depends on. That is what makes each status decidable:
//
//   - transport error (DNS, dial, TLS, timeout) — nothing answered: blocked.
//   - 2xx — GitHub served us: reachable.
//   - 403, 429 — /zen carries no credentials and needs none, so a refusal is not
//     about who asked; GitHub is throttling or blocking this source address
//     (primary/secondary rate limit, abuse detection). Blocked. This is the case
//     Q648 was filed for: scoring it reachable told operators to treat a
//     rate-limited run as a product regression.
//   - 408, 5xx — the path reaches GitHub (or an intermediary) but it will not
//     serve the request. Not something this repo can regress: blocked.
//   - anything else (3xx surviving the client's redirect following, 401, 404,
//     other 4xx) — GitHub answers an unauthenticated /zen with 200 and nothing
//     else, so a different status means an intermediary is intercepting the
//     request or the endpoint moved. Inconclusive: which one it is decides the
//     attribution, and only the response body says which.
//
// Scoring deliberately ignores the rate-limit headers. They make the banner
// concrete but cannot change a verdict — a 403 is a refusal whether or not
// GitHub explains it.
//
// The failure-diagnostic step in .github/workflows/e2e-reusable.yml mirrors this
// table in shell, for the case where the suite process dies before its own
// AfterEach can probe. Change both together.
func ScoreGitHubEgress(status int, err error) GitHubEgressVerdict {
	switch {
	case err != nil:
		return EgressBlocked
	case status >= 200 && status < 300:
		return EgressReachable
	case status == 403 || status == 429 || status == 408:
		return EgressBlocked
	case status >= 500:
		return EgressBlocked
	default:
		return EgressInconclusive
	}
}

// GitHubEgressGuidance is the triage instruction stamped under a verdict in the
// CI failure banner. status follows ScoreGitHubEgress: 0 when no response
// arrived, the HTTP status code otherwise.
//
// BLOCKED takes status because the verdict spans two layers. A transport error
// means nothing answered, so the dial-level claim ("it was refused too") is the
// probe's own finding. An HTTP refusal is a response, so the TCP connection and
// the TLS handshake both completed, and the same sentence then asserts a
// dial-level cause that this evidence argues against. On the 2026-09-16 kindnet
// run both proxy-CONNECT specs failed on `curl: (56) CONNECT tunnel failed,
// response 502` while the proxy logged `dial tcp 172.182.252.137:443: i/o
// timeout` against api.github.com:443; the probe answered 403 for one and 200
// for the other, and BLOCKED told the reader the dial-level failure was
// explained (Q1126).
//
// The sibling ProxyConnectGuidance says the same thing from the proxy's side,
// and the two banners are meant to agree.
//
// The failure-diagnostic step in .github/workflows/e2e-reusable.yml mirrors
// these strings in shell. Change both together.
func GitHubEgressGuidance(v GitHubEgressVerdict, status int) string {
	switch {
	case v == EgressBlocked && status == 0:
		return "Nothing answered the runner host's probe of https://api.github.com at failure time; the\n" +
			"transport error above names the layer that failed. The in-cluster path NATs through the\n" +
			"same address, so it was refused too: this spec's failure is attributable to runner->GitHub\n" +
			"egress — infrastructure, not a product regression (Q352). Re-run the job.\n"
	case v == EgressBlocked:
		return "GitHub refused the runner host at failure time (status and body above). The in-cluster\n" +
			"path NATs through the same address, so it is refused too: this run's GitHub egress is\n" +
			"impaired — infrastructure, not a product regression (Q352). Re-run the job.\n" +
			"\n" +
			"Mind the layer before you stop here. A refusal is an HTTP response, so it arrived over a\n" +
			"completed TCP connection and TLS handshake: it is evidence AGAINST a dial-level cause, not\n" +
			"for one — a rate limit cannot produce a dial timeout. If this spec failed before the HTTP\n" +
			"layer (a CONNECT 502, a `dial tcp ...: i/o timeout`), this banner does not explain it; read\n" +
			"the EGRESSPROXY CONNECT ATTRIBUTION banner instead (Q1119).\n"
	case v == EgressReachable:
		return "GitHub serves the runner host at failure time, so a host-level egress blip is unlikely —\n" +
			"treat this failure as real and inspect the in-cluster path\n" +
			"(workload NP -> proxy -> egress NP -> GitHub).\n"
	default:
		return "Something answered, but not the way GitHub answers an unauthenticated /zen (200). Read the\n" +
			"body excerpt above: if it is not GitHub's, an intermediary is intercepting runner egress and\n" +
			"this failure is infrastructure — re-run. If it is GitHub's, the probe endpoint has changed and\n" +
			"this banner cannot attribute the failure; fix the probe and triage the spec on its own output.\n"
	}
}
