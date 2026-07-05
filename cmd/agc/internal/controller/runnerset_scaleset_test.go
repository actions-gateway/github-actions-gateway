package controller

import "testing"

// TestScaleSetAPIBase covers the production API-base derivation used when no
// ScaleSetClientFactory is injected: public GitHub hosts select the client's default
// (empty), a GHES host derives the /api/v3 base, and an unparseable URL falls back to
// the default rather than producing a garbage base.
func TestScaleSetAPIBase(t *testing.T) {
	cases := []struct {
		name      string
		githubURL string
		want      string
	}{
		{"public org", "https://github.com/acme", ""},
		{"public repo", "https://github.com/acme/repo", ""},
		{"www host", "https://www.github.com/acme", ""},
		{"api host", "https://api.github.com", ""},
		{"ghes org", "https://ghes.example.com/acme", "https://ghes.example.com/api/v3"},
		{"ghes repo", "https://ghes.example.com/acme/repo", "https://ghes.example.com/api/v3"},
		{"empty", "", ""},
		{"no host", "not a url", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scaleSetAPIBase(tc.githubURL); got != tc.want {
				t.Errorf("scaleSetAPIBase(%q) = %q, want %q", tc.githubURL, got, tc.want)
			}
		})
	}
}
