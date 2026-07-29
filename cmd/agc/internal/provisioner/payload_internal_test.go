package provisioner

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// githubContext builds the wire form of the serialised `github` context — the
// type-tagged key/value list the runner SDK emits for a DictionaryContextData,
// which is where a real AcquireJob response keeps the run identity (Q495).
func githubContext(t *testing.T, kv map[string]any) acquirePayload {
	t.Helper()
	pairs := make([]map[string]any, 0, len(kv))
	for k, v := range kv {
		pairs = append(pairs, map[string]any{"k": k, "v": v})
	}
	raw, err := json.Marshal(map[string]any{
		"contextData": map[string]any{"github": map[string]any{"t": 2, "d": pairs}},
	})
	require.NoError(t, err)
	var ap acquirePayload
	require.NoError(t, json.Unmarshal(raw, &ap))
	return ap
}

func TestRunIdentity_FromGitHubContext(t *testing.T) {
	ap := githubContext(t, map[string]any{
		"run_id":     "12345678",
		"repository": "myorg/myrepo",
	})
	runID, repository := ap.runIdentity()
	assert.Equal(t, "12345678", runID)
	assert.Equal(t, "myorg/myrepo", repository)

	owner, repo, numericRunID := ap.repoInfo()
	assert.Equal(t, "myorg", owner)
	assert.Equal(t, "myrepo", repo)
	assert.Equal(t, int64(12345678), numericRunID)
}

// A context whose run_id arrives unquoted still resolves: the value is coerced from
// its JSON form rather than requiring a string.
func TestRunIdentity_NumericContextValue(t *testing.T) {
	ap := githubContext(t, map[string]any{"run_id": 42})
	runID, _ := ap.runIdentity()
	assert.Equal(t, "42", runID)
}

// Non-scalar and non-numeric values are ignored rather than stringified: the github
// context holds a nested dictionary under `event`, and a garbage run_id must not
// reach the annotation the scale-set tier reads back as a run identity.
func TestRunIdentity_IgnoresUnusableContextValues(t *testing.T) {
	ap := githubContext(t, map[string]any{
		"run_id":     "not-a-number",
		"repository": map[string]any{"t": 2, "d": []any{}},
	})
	runID, repository := ap.runIdentity()
	assert.Empty(t, runID)
	assert.Empty(t, repository)
}

// The github context is the source a real payload populates, so it wins over the
// tolerated fallbacks when a payload somehow carries both.
func TestRunIdentity_ContextWinsOverVariables(t *testing.T) {
	ap := githubContext(t, map[string]any{"run_id": "111", "repository": "ctx/repo"})
	ap.RunID = 333
	ap.Variables = map[string]variableEnvValue{
		"system.github.run_id":     {Value: "222"},
		"system.github.repository": {Value: "var/repo"},
	}
	runID, repository := ap.runIdentity()
	assert.Equal(t, "111", runID)
	assert.Equal(t, "ctx/repo", repository)
}

// A malformed run_id from one source is dropped rather than carried, so a later
// source still answers — the behaviour repoInfo had before the sources were shared.
func TestRunIdentity_MalformedVariableFallsBackToTopLevel(t *testing.T) {
	ap := acquirePayload{
		RunID:     99,
		Variables: map[string]variableEnvValue{"system.github.run_id": {Value: "bogus"}},
	}
	runID, _ := ap.runIdentity()
	assert.Equal(t, "99", runID)
}

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
