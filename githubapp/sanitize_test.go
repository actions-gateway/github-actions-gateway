package githubapp

import (
	"strings"
	"testing"
)

func TestSanitizeBody_RedactsCredentials(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// mustNotContain are secret substrings that must be gone after redaction.
		mustNotContain []string
		// mustContain are markers/keys that must survive.
		mustContain []string
	}{
		{
			name:           "access_token JSON value (the runner_auth :259 success body)",
			in:             `{"access_token":"v1.abc123def456ghi789jkl012mno345pqr678","token_type":"bearer"}`,
			mustNotContain: []string{"v1.abc123def456ghi789jkl012mno345pqr678"},
			mustContain:    []string{"access_token", "[REDACTED]", "token_type"},
		},
		{
			name:           "encoded_jit_config (the github_registrar :97 cred body)",
			in:             `{"runner":{"id":42},"encoded_jit_config":"eyJhYmMiOiJkZWYiLCJnaGkiOiJqa2wifQ=="}`,
			mustNotContain: []string{"eyJhYmMiOiJkZWYiLCJnaGkiOiJqa2wifQ=="},
			mustContain:    []string{"encoded_jit_config", "[REDACTED]", "runner"},
		},
		{
			name:           "ghs_ server-to-server token in free text",
			in:             "error: token ghs_16C7e42F292c6912E7710c838347Ae178B4a authentication failed",
			mustNotContain: []string{"ghs_16C7e42F292c6912E7710c838347Ae178B4a"},
			mustContain:    []string{"[REDACTED]", "authentication failed"},
		},
		{
			name:           "github_pat_ fine-grained PAT",
			in:             "github_pat_11ABCDEFG0abcdefghijkl_MNOPQRSTUVWXYZ0123456789abcdef",
			mustNotContain: []string{"github_pat_11ABCDEFG0abcdefghijkl"},
			mustContain:    []string{"[REDACTED]"},
		},
		{
			name:           "JWT installation token",
			in:             "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N",
			mustNotContain: []string{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0"},
			mustContain:    []string{"[REDACTED]"},
		},
		{
			name:           "non-secret error body passes through unchanged",
			in:             `{"message":"Not Found","status":"404"}`,
			mustNotContain: []string{"[REDACTED]"},
			mustContain:    []string{"Not Found", "404"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeBody([]byte(tc.in), 4096)
			for _, s := range tc.mustNotContain {
				if strings.Contains(got, s) {
					t.Errorf("SanitizeBody leaked %q\n got: %s", s, got)
				}
			}
			for _, s := range tc.mustContain {
				if !strings.Contains(got, s) {
					t.Errorf("SanitizeBody dropped expected %q\n got: %s", s, got)
				}
			}
		})
	}
}

func TestSanitizeBody_CapsLength(t *testing.T) {
	// "word " repeated has no 40+ contiguous token, so it exercises capping
	// independently of redaction.
	body := strings.Repeat("word ", 200) // 1000 bytes
	got := SanitizeBody([]byte(body), 200)
	if len(got) <= 200 {
		t.Fatalf("expected truncation marker beyond 200 bytes, got len=%d", len(got))
	}
	if !strings.HasSuffix(got, truncatedMarker) {
		t.Errorf("expected truncated marker suffix, got tail %q", got[len(got)-20:])
	}
	if !strings.HasPrefix(got, "word ") {
		t.Errorf("expected capped prefix retained, got %q", got[:10])
	}
}

func TestSanitizeBody_RedactBeforeCap(t *testing.T) {
	// A secret near the start must be redacted even though the body exceeds max,
	// proving redaction runs before capping.
	body := `{"access_token":"supersecretvalue123456789","padding":"` + strings.Repeat("x", 400) + `"}`
	got := SanitizeBody([]byte(body), 50)
	if strings.Contains(got, "supersecretvalue123456789") {
		t.Errorf("secret survived redaction-before-cap: %s", got)
	}
}
