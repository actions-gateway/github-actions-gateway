package validation

import (
	"testing"

	"github.com/stretchr/testify/assert"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
)

// TestPrivilegedGrantPresent pins the §H.12 dual-read: the grant counts on either
// label domain, and nothing else counts. Both the v1 admission webhook and
// `gag-migrate` decide on this one function (Q463), so a regression here silently
// desynchronizes admission from the migration tool.
func TestPrivilegedGrantPresent(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{
			name:   "v1 domain grant",
			labels: map[string]string{gmcv1alpha1.PrivilegedProfileLabel: gmcv1alpha1.PrivilegedProfileAllowed},
			want:   true,
		},
		{
			name:   "v2 domain grant",
			labels: map[string]string{v2alpha1.PrivilegedProfileLabel: v2alpha1.PrivilegedProfileAllowed},
			want:   true,
		},
		{
			name: "both domains granted",
			labels: map[string]string{
				gmcv1alpha1.PrivilegedProfileLabel: gmcv1alpha1.PrivilegedProfileAllowed,
				v2alpha1.PrivilegedProfileLabel:    v2alpha1.PrivilegedProfileAllowed,
			},
			want: true,
		},
		{
			name:   "no labels at all",
			labels: nil,
			want:   false,
		},
		{
			name:   "unrelated labels only",
			labels: map[string]string{"team": "a"},
			want:   false,
		},
		{
			// Fail-closed: widening the accepted domain must not widen the accepted value.
			name: "wrong value on both domains",
			labels: map[string]string{
				gmcv1alpha1.PrivilegedProfileLabel: "true",
				v2alpha1.PrivilegedProfileLabel:    "yes",
			},
			want: false,
		},
		{
			// One domain granted, the other explicitly set to a non-grant value: the
			// grant still stands. The check is an OR, not an agreement between domains.
			name: "v2 granted while v1 holds a non-grant value",
			labels: map[string]string{
				gmcv1alpha1.PrivilegedProfileLabel: "false",
				v2alpha1.PrivilegedProfileLabel:    v2alpha1.PrivilegedProfileAllowed,
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, PrivilegedGrantPresent(tt.labels))
		})
	}
}
