package provisioner

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestJobMeta_PodAnnotations(t *testing.T) {
	m := jobMeta{
		runID:      "12345678",
		repository: "myorg/myrepo",
		jobName:    "build",
		workflow:   "CI",
	}
	a := m.podAnnotations()
	assert.Equal(t, "12345678", a["actions-gateway.com/run-id"])
	assert.Equal(t, "myorg/myrepo", a["actions-gateway.com/repository"])
	assert.Equal(t, "build", a["actions-gateway.com/job-name"])
	assert.Equal(t, "CI", a["actions-gateway.com/workflow"])
}

func TestJobMeta_PodAnnotations_PartialOmitsEmpty(t *testing.T) {
	// Only populated fields should appear — no zero-value keys.
	m := jobMeta{runID: "42"}
	a := m.podAnnotations()
	assert.Equal(t, map[string]string{"actions-gateway.com/run-id": "42"}, a)
}
