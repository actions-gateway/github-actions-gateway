package controller

import "testing"

// TestScaleSetStubConfigURL pins the fake-GitHub re-point. The config URL must keep
// the gateway's org (or owner/repo) path, because the client derives the runners REST
// prefix from it — a pathless config URL is rejected outright, so dropping the path
// would wedge the bootstrap rather than merely misaddress it.
//
// The API base is deliberately NOT computed here: it is derived from whatever config
// URL this returns, by the same githubapp.DeriveAPIBaseURL the production path uses
// (Q506), so the GHES rule lives in exactly one place.
func TestScaleSetStubConfigURL(t *testing.T) {
	cases := []struct {
		name      string
		stubBase  string
		githubURL string
		want      string
	}{
		{
			name:      "org path preserved",
			stubBase:  "http://fakegithub.e2e-infra.svc.cluster.local:8080",
			githubURL: "https://github.com/e2e-org",
			want:      "http://fakegithub.e2e-infra.svc.cluster.local:8080/e2e-org",
		},
		{
			name:      "owner/repo path preserved",
			stubBase:  "http://stub:8080",
			githubURL: "https://github.com/acme/widgets",
			want:      "http://stub:8080/acme/widgets",
		},
		{
			name:      "unparseable stub base leaves the gateway URL alone",
			stubBase:  "://nonsense",
			githubURL: "https://github.com/acme",
			want:      "https://github.com/acme",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scaleSetStubConfigURL(tc.stubBase, tc.githubURL); got != tc.want {
				t.Errorf("scaleSetStubConfigURL(%q, %q) = %q, want %q",
					tc.stubBase, tc.githubURL, got, tc.want)
			}
		})
	}
}
