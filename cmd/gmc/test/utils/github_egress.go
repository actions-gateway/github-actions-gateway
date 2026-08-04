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
