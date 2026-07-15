package controller

import (
	"strings"
	"testing"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestVanishedReferentReason covers the TemplateDeleted/ProxyDeleted upgrade
// (§H.8 degrade-not-block, Q309): a *NotFound outcome is upgraded to *Deleted
// only when the set's own status shows a prior successful resolution under an
// unchanged spec generation.
func TestVanishedReferentReason(t *testing.T) {
	set := func(gen, observed int64, templateSource, proxyMode string) *v2alpha1.RunnerSet {
		rs := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Generation: gen}}
		rs.Status.ObservedGeneration = observed
		rs.Status.TemplateSource = templateSource
		rs.Status.ProxyMode = proxyMode
		return rs
	}

	tests := []struct {
		name       string
		rs         *v2alpha1.RunnerSet
		in         refResolution
		wantReason string
		wantSuffix string // non-empty ⇒ the message must gain this suffix
	}{
		{
			name:       "never-resolved template stays NotFound",
			rs:         set(1, 1, "", ""),
			in:         refResolution{reason: v2alpha1.ReasonTemplateNotFound, message: "m"},
			wantReason: v2alpha1.ReasonTemplateNotFound,
		},
		{
			name:       "previously-resolved template upgrades to TemplateDeleted",
			rs:         set(1, 1, v2alpha1.TemplateSourceRef, v2alpha1.ProxyModeDirect),
			in:         refResolution{reason: v2alpha1.ReasonTemplateNotFound, message: "m"},
			wantReason: v2alpha1.ReasonTemplateDeleted,
			wantSuffix: "deleted",
		},
		{
			name:       "spec edit (generation bumped) falls back to NotFound",
			rs:         set(2, 1, v2alpha1.TemplateSourceRef, ""),
			in:         refResolution{reason: v2alpha1.ReasonTemplateNotFound, message: "m"},
			wantReason: v2alpha1.ReasonTemplateNotFound,
		},
		{
			name:       "previously-proxied set upgrades to ProxyDeleted",
			rs:         set(3, 3, v2alpha1.TemplateSourceRef, v2alpha1.ProxyModeProxied),
			in:         refResolution{reason: v2alpha1.ReasonProxyNotFound, message: "m"},
			wantReason: v2alpha1.ReasonProxyDeleted,
			wantSuffix: "deleted",
		},
		{
			name:       "direct-egress set never upgrades a dangling proxy ref",
			rs:         set(1, 1, v2alpha1.TemplateSourceRef, v2alpha1.ProxyModeDirect),
			in:         refResolution{reason: v2alpha1.ReasonProxyNotFound, message: "m"},
			wantReason: v2alpha1.ReasonProxyNotFound,
		},
		{
			name:       "non-referent reasons pass through untouched",
			rs:         set(1, 1, v2alpha1.TemplateSourceRef, v2alpha1.ProxyModeProxied),
			in:         refResolution{reason: v2alpha1.ReasonAmbiguousDefault, message: "m"},
			wantReason: v2alpha1.ReasonAmbiguousDefault,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reason, message := vanishedReferentReason(tc.rs, tc.in)
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if tc.wantSuffix == "" {
				if message != tc.in.message {
					t.Fatalf("message changed without an upgrade: %q", message)
				}
			} else if !strings.HasPrefix(message, tc.in.message) || !strings.Contains(message, tc.wantSuffix) {
				t.Fatalf("upgraded message %q must keep the original and mention %q", message, tc.wantSuffix)
			}
		})
	}
}

// TestClearStaleResolutionMarkers verifies a plain *NotFound outcome drops the
// status marker that would otherwise upgrade later reconciles to *Deleted, while
// the *Deleted reasons keep theirs (the evidence of the prior resolution).
func TestClearStaleResolutionMarkers(t *testing.T) {
	rs := &v2alpha1.RunnerSet{}
	rs.Status.TemplateSource = v2alpha1.TemplateSourceRef
	rs.Status.ProxyMode = v2alpha1.ProxyModeProxied

	clearStaleResolutionMarkers(rs, v2alpha1.ReasonTemplateDeleted)
	clearStaleResolutionMarkers(rs, v2alpha1.ReasonProxyDeleted)
	if rs.Status.TemplateSource == "" || rs.Status.ProxyMode == "" {
		t.Fatal("*Deleted reasons must keep their prior-resolution markers")
	}

	clearStaleResolutionMarkers(rs, v2alpha1.ReasonTemplateNotFound)
	if rs.Status.TemplateSource != "" {
		t.Fatal("TemplateNotFound must clear status.templateSource")
	}
	clearStaleResolutionMarkers(rs, v2alpha1.ReasonProxyNotFound)
	if rs.Status.ProxyMode != "" {
		t.Fatal("ProxyNotFound must clear status.proxyMode")
	}
}
