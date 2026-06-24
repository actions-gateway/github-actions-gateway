package provisioner

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestJobMetaFrom_FullVariables(t *testing.T) {
	ap := acquirePayload{
		Variables: map[string]variableEnvValue{
			"system.github.run_id":     {Value: "12345678"},
			"system.github.repository": {Value: "myorg/myrepo"},
			"system.github.job":        {Value: "build"},
			"system.github.workflow":   {Value: "CI"},
		},
	}
	m := jobMetaFrom(ap)
	assert.Equal(t, "12345678", m.runID)
	assert.Equal(t, "myorg/myrepo", m.repository)
	assert.Equal(t, "build", m.jobName)
	assert.Equal(t, "CI", m.workflow)
}

func TestJobMetaFrom_TopLevelRunID(t *testing.T) {
	// Payloads without a variables map fall back to the top-level run_id.
	ap := acquirePayload{RunID: 99}
	m := jobMetaFrom(ap)
	assert.Equal(t, "99", m.runID)
	assert.Empty(t, m.repository)
	assert.Empty(t, m.jobName)
	assert.Empty(t, m.workflow)
}

func TestJobMetaFrom_Empty(t *testing.T) {
	m := jobMetaFrom(acquirePayload{})
	assert.Empty(t, m.runID)
	assert.Nil(t, m.podAnnotations())
}

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
