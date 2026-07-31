package validation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

// TestDeletionOnlyUpdate pins the Q518 exemption predicate: only an update that
// changes no spec field on an object already carrying a deletionTimestamp is a
// deletion-only write. Every webhook's ValidateUpdate decides on this one
// function, mirroring the VAP matchCondition.
func TestDeletionOnlyUpdate(t *testing.T) {
	now := metav1.Now()
	type spec struct{ Classes []string }

	tests := []struct {
		name     string
		deleting bool
		oldSpec  spec
		newSpec  spec
		want     bool
	}{
		{
			name:     "deleting and spec unchanged",
			deleting: true,
			oldSpec:  spec{Classes: []string{"a"}},
			newSpec:  spec{Classes: []string{"a"}},
			want:     true,
		},
		{
			name:    "live object is never exempt even with an unchanged spec",
			oldSpec: spec{Classes: []string{"a"}},
			newSpec: spec{Classes: []string{"a"}},
			want:    false,
		},
		{
			name:     "spec change on a deleting object is not exempt",
			deleting: true,
			oldSpec:  spec{Classes: []string{"a"}},
			newSpec:  spec{Classes: []string{"a", "b"}},
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := &metav1.ObjectMeta{}
			if tt.deleting {
				meta.DeletionTimestamp = &now
			}
			assert.Equal(t, tt.want, DeletionOnlyUpdate(meta, tt.oldSpec, tt.newSpec))
		})
	}
}
