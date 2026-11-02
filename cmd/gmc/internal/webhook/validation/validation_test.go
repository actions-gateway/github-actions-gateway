package validation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
