package provisioner

import (
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/names"
	"github.com/stretchr/testify/assert"
)

// TestImageVersion locks the app.kubernetes.io/version derivation: a tag is used
// verbatim, a digest is never mistaken for a tag, a registry port is not mistaken
// for a tag, and a tagless reference falls back to the pinned runner version.
func TestImageVersion(t *testing.T) {
	cases := []struct {
		name  string
		image string
		want  string
	}{
		{"tag and digest", "ghcr.io/actions/actions-runner:2.335.1@sha256:abc", "2.335.1"},
		{"tag only", "ghcr.io/actions/actions-runner:2.335.1", "2.335.1"},
		{"digest only falls back", "ghcr.io/actions/actions-runner@sha256:abc", names.RunnerVersion},
		{"no tag falls back", "ghcr.io/actions/actions-runner", names.RunnerVersion},
		{"registry port, no tag", "registry.local:5000/actions-runner", names.RunnerVersion},
		{"registry port with tag", "registry.local:5000/actions-runner:v1.2.3", "v1.2.3"},
		{"empty falls back", "", names.RunnerVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, imageVersion(tc.image))
		})
	}
}
