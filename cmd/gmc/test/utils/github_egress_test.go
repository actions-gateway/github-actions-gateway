package utils

import (
	"errors"
	"strings"
	"testing"
)

func TestScoreGitHubEgress(t *testing.T) {
	dialErr := errors.New("dial tcp 140.82.121.6:443: i/o timeout")

	cases := []struct {
		name   string
		status int
		err    error
		want   GitHubEgressVerdict
	}{
		{"transport error", 0, dialErr, EgressBlocked},
		{"transport error outranks a stale status", 200, dialErr, EgressBlocked},

		{"200 served", 200, nil, EgressReachable},
		{"204 served", 204, nil, EgressReachable},
		{"299 served", 299, nil, EgressReachable},

		// Q648: /zen needs no credentials, so a refusal is about the source
		// address, not the request — the in-cluster path shares that address.
		{"403 refused", 403, nil, EgressBlocked},
		{"429 rate limited", 429, nil, EgressBlocked},
		{"408 request timeout", 408, nil, EgressBlocked},
		{"500 GitHub error", 500, nil, EgressBlocked},
		{"502 intermediary", 502, nil, EgressBlocked},
		{"503 unavailable", 503, nil, EgressBlocked},
		{"504 gateway timeout", 504, nil, EgressBlocked},

		// GitHub answers an unauthenticated /zen with 200 and nothing else, so
		// these mean an intermediary answered or the endpoint moved.
		{"301 surviving redirect following", 301, nil, EgressInconclusive},
		{"400 rewritten request", 400, nil, EgressInconclusive},
		{"401 auth demanded", 401, nil, EgressInconclusive},
		{"404 endpoint gone", 404, nil, EgressInconclusive},
		{"418 other 4xx", 418, nil, EgressInconclusive},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ScoreGitHubEgress(tc.status, tc.err); got != tc.want {
				t.Errorf("ScoreGitHubEgress(%d, %v) = %s, want %s", tc.status, tc.err, got, tc.want)
			}
		})
	}
}

// The guidance is the half of the banner that tells the reader what to do, and
// on 2026-09-16 it told them the wrong thing: a 403 scored BLOCKED and its
// instruction attributed a dial-level CONNECT 502 to runner egress. What
// separates the two cases is whether a response arrived, so the split is what
// these cases pin (Q1126).
func TestGitHubEgressGuidanceSplitsByLayer(t *testing.T) {
	const dialClause = "evidence AGAINST a dial-level cause"

	transport := GitHubEgressGuidance(EgressBlocked, 0)
	refused := GitHubEgressGuidance(EgressBlocked, 403)

	if transport == refused {
		t.Fatal("BLOCKED renders one string for a transport error and an HTTP refusal; " +
			"the two support opposite inferences about the dial layer")
	}

	// The refusal arm must carry the layer caveat...
	if !strings.Contains(refused, dialClause) {
		t.Errorf("BLOCKED on a 403 does not say the answer argues against a dial-level cause:\n%s", refused)
	}
	if !strings.Contains(refused, "EGRESSPROXY CONNECT ATTRIBUTION") {
		t.Errorf("BLOCKED on a 403 does not route a dial-level failure to the sibling banner:\n%s", refused)
	}

	// ...and the transport-error arm must NOT: there the dial-level claim is
	// the probe's own finding, so denying it would invert the attribution.
	if strings.Contains(transport, dialClause) {
		t.Errorf("BLOCKED on a transport error argues against a dial-level cause, which is the one\n"+
			"case where the probe found exactly that:\n%s", transport)
	}

	// Every HTTP route to BLOCKED gets the caveat, not just the 403 that
	// produced the incident.
	for _, status := range []int{403, 408, 429, 500, 502, 503, 504} {
		if got := GitHubEgressGuidance(EgressBlocked, status); !strings.Contains(got, dialClause) {
			t.Errorf("BLOCKED on HTTP %d lacks the layer caveat", status)
		}
	}
}

// Each verdict must yield a non-empty instruction: a banner that names a state
// and then says nothing is worse than no banner, because it reads as handled.
func TestGitHubEgressGuidanceNeverEmpty(t *testing.T) {
	cases := []struct {
		verdict GitHubEgressVerdict
		status  int
	}{
		{EgressBlocked, 0},
		{EgressBlocked, 403},
		{EgressReachable, 200},
		{EgressInconclusive, 404},
		{GitHubEgressVerdict(99), 0},
	}

	for _, tc := range cases {
		if got := GitHubEgressGuidance(tc.verdict, tc.status); strings.TrimSpace(got) == "" {
			t.Errorf("GitHubEgressGuidance(%s, %d) is empty", tc.verdict, tc.status)
		}
	}
}

// The verdict token is stamped into the CI failure banner and read by whoever
// triages the run, so an unnamed verdict must not render as an empty string.
func TestGitHubEgressVerdictString(t *testing.T) {
	cases := []struct {
		verdict GitHubEgressVerdict
		want    string
	}{
		{EgressBlocked, "BLOCKED"},
		{EgressReachable, "REACHABLE"},
		{EgressInconclusive, "INCONCLUSIVE"},
		{GitHubEgressVerdict(99), "UNKNOWN"},
	}

	for _, tc := range cases {
		if got := tc.verdict.String(); got != tc.want {
			t.Errorf("GitHubEgressVerdict(%d).String() = %q, want %q", int(tc.verdict), got, tc.want)
		}
	}
}
