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
		{ //nolint:gosec // G101: synthetic token used as redaction-test input, not a real credential
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

// TestSanitizeBody_RedactsNewFormatInstallationToken pins redaction of the
// 2026 GitHub App installation-token format ghs_<app-id>_<JWT> (~520 chars,
// variable length, JWT = header.payload.signature base64url). No single
// pattern matches the whole token: the gh[pousr]_ prefix pattern consumes
// ghs_<app-id>_<JWT header> up to the first '.', and the long-blob pattern
// catches the payload and signature segments. This test exists so an edit to
// either pattern that breaks that interplay fails loudly instead of silently
// leaking token fragments.
func TestSanitizeBody_RedactsNewFormatInstallationToken(t *testing.T) {
	// Synthetic ~520-char token shaped like the real format: base64url JWT
	// segments (header, payload, signature) behind ghs_<app-id>_.
	header := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9"
	payload := "eyJpc3MiOiIzNzUyMzQ3In0" + strings.Repeat("aB3xYz01-_", 25)
	signature := strings.Repeat("Qw9_zX8-Kp", 19) + "mN4tUv"
	token := "ghs_3752347_" + header + "." + payload + "." + signature

	cases := []struct {
		name string
		in   string
	}{
		{
			name: "bare token in free text",
			in:   "error: token " + token + " authentication failed",
		},
		{
			name: "token as JSON value",
			in:   `{"token":"` + token + `","expires_at":"2026-06-09T12:00:00Z"}`,
		},
		{
			name: "Authorization Bearer header",
			in:   "request failed: Authorization: Bearer " + token + " rejected",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeBody([]byte(tc.in), 4096)
			// No 20+ char run of the token may survive; checking every 20-char
			// window of the token covers all longer runs too.
			for i := 0; i+20 <= len(token); i++ {
				if frag := token[i : i+20]; strings.Contains(got, frag) {
					t.Fatalf("SanitizeBody leaked token fragment %q at offset %d\n got: %s", frag, i, got)
				}
			}
			if !strings.Contains(got, "[REDACTED]") {
				t.Errorf("expected redaction marker in output\n got: %s", got)
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
