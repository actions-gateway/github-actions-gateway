package utils

import (
	"errors"
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
