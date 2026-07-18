package provisioner

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

// dnsLabelRe matches characters not allowed in Kubernetes DNS labels.
var dnsLabelRe = regexp.MustCompile(`[^a-z0-9-]`)

// repoSegmentRE accepts only the characters GitHub allows in org/repo names.
// Must start with an alphanumeric character so that ".." and other dot-leading
// strings cannot produce path-traversal sequences in the API URL.
var repoSegmentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// safeName converts an arbitrary string into a Kubernetes-safe DNS label
// (lowercase, alphanumeric and hyphens only). The output is at most 48 chars:
// up to 40 sanitised chars from the input, a "-" separator, and 7 hex chars
// derived from a SHA-256 hash of the original string. The hash suffix ensures
// uniqueness when two different inputs share the same 40-char sanitised prefix.
func safeName(s string) string {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(s)))[:7]
	s = strings.ToLower(s)
	s = dnsLabelRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = s[:40]
	}
	s = strings.TrimRight(s, "-") // re-trim after truncation
	if s == "" {
		s = "job"
	}
	return s + "-" + hash
}
