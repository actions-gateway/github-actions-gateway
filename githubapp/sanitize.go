package githubapp

import "regexp"

// secretPatterns matches credential-shaped substrings that must never survive
// into an error message or log line. Upstream GitHub responses — token
// endpoints, generate-jitconfig, the broker — can carry access tokens, runner
// JIT registration credentials, and RSA key material in both success and error
// bodies, so any body interpolated into an error a caller logs is a leak
// surface. The patterns are deliberately broad: over-redacting an opaque error
// body costs nothing, under-redacting a credential is a security defect.
var secretPatterns = []*regexp.Regexp{
	// JSON values for known-sensitive keys: "access_token": "…" → keep the key,
	// redact the value. Case-insensitive on the key. Handled first so the key
	// name stays visible for debugging.
	regexp.MustCompile(`(?i)("(?:access_token|refresh_token|token|encoded_jit_config|client_secret|private_key|password|secret)"\s*:\s*)"[^"]*"`),
	// GitHub token formats: PAT (ghp_), OAuth (gho_), user-to-server (ghu_),
	// server-to-server (ghs_), refresh (ghr_), and fine-grained PAT prefixes.
	regexp.MustCompile(`(?i)(?:gh[pousr]_|github_pat_)[A-Za-z0-9_]{20,}`),
	// JWTs (header.payload.signature, base64url) — e.g. installation tokens.
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
	// Long opaque blobs: runner registration credentials and base64-encoded key
	// material that are not wrapped in a known JSON key. 40+ contiguous
	// base64/base64url characters do not occur in human-readable error prose.
	regexp.MustCompile(`[A-Za-z0-9+/_-]{40,}={0,2}`),
}

const (
	// sanitizeScanLimit bounds how many bytes of an upstream body the redaction
	// regexes scan, so a hostile or accidental multi-megabyte body cannot turn
	// error formatting into a CPU sink. Anything past this offset is dropped
	// before redaction (and would be truncated by max anyway), so no
	// unredacted secret can escape through the tail.
	sanitizeScanLimit = 8192

	redactedMarker  = "[REDACTED]"
	truncatedMarker = "...(truncated)"
)

// SanitizeBody returns at most max bytes of body as a string with
// credential-shaped substrings redacted, safe to interpolate into an error or
// log line. Redaction runs before capping so a secret straddling the cap
// boundary cannot survive in the truncated tail. max must be > 0.
//
// This is the single redaction implementation shared across the repo
// (githubapp, agentpool, broker, probe); keep new credential formats here
// rather than adding per-call-site filtering.
func SanitizeBody(body []byte, max int) string {
	b := body
	if len(b) > sanitizeScanLimit {
		b = b[:sanitizeScanLimit]
	}
	s := redactSecrets(string(b))
	if max > 0 && len(s) > max {
		return s[:max] + truncatedMarker
	}
	return s
}

// redactSecrets replaces every credential-shaped substring in s with
// redactedMarker, preserving sensitive JSON keys' names.
func redactSecrets(s string) string {
	// The JSON-key pattern (first) needs its capture group preserved; the rest
	// replace the whole match.
	s = secretPatterns[0].ReplaceAllString(s, `${1}"`+redactedMarker+`"`)
	for _, re := range secretPatterns[1:] {
		s = re.ReplaceAllString(s, redactedMarker)
	}
	return s
}
