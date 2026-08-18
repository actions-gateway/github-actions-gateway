package provisioner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// groundTruthPayload is the redacted capture of a live AcquireJob response
// (cmd/probe writes it; testdata/README.md documents the redactions). It is the
// only artefact in the repo that says what GitHub actually sends, which is why the
// tests below read it instead of a hand-written payload. Reached through an
// in-module symlink so it is part of this package's test-cache key
// (testing.md § The out-of-module test read gate).
const groundTruthPayload = "testdata/job_payload.json"

func loadGroundTruthPayload(t *testing.T) acquirePayload {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(groundTruthPayload))
	require.NoError(t, err, "read the captured AcquireJob response")
	var ap acquirePayload
	require.NoError(t, json.Unmarshal(raw, &ap))
	return ap
}

// TestRepoInfo_RealPayload is the regression guard for Q495. The identity readers
// used to look only at variables["system.github.run_id"]/["…repository"] and a
// top-level run_id, none of which a real payload carries — so on the classic tier
// repoInfo returned run 0 and handleEviction skipped every recovery, while every
// test passed on synthetic payloads that did carry those keys.
func TestRepoInfo_RealPayload(t *testing.T) {
	ap := loadGroundTruthPayload(t)

	// The shape this is guarding against: the sources the code used to rely on are
	// genuinely absent from a real response.
	assert.Zero(t, ap.RunID, "the real payload has no top-level run_id")
	_, hasRunIDVar := ap.Variables["system.github.run_id"]
	assert.False(t, hasRunIDVar, "the real payload has no system.github.run_id variable")
	_, hasRepoVar := ap.Variables["system.github.repository"]
	assert.False(t, hasRepoVar, "the real payload has no system.github.repository variable")

	owner, repo, runID := ap.repoInfo()
	assert.Equal(t, "actions-gateway", owner)
	assert.Equal(t, "github-actions-gateway", repo)
	assert.Equal(t, int64(26056010752), runID,
		"eviction recovery names the run by this id; 0 makes handleEviction skip")
}

// TestJobMetaFrom_RealPayload asserts the worker pod stamped from a real payload
// carries the run identity, since the scale-set tier reads those two annotations
// back off the pod to recover an eviction (Q417).
func TestJobMetaFrom_RealPayload(t *testing.T) {
	m := jobMetaFrom(loadGroundTruthPayload(t))

	a := m.podAnnotations()
	assert.Equal(t, "26056010752", a[AnnotationRunID])
	assert.Equal(t, "actions-gateway/github-actions-gateway", a[AnnotationRepository])
	// Cosmetic, and each proves a different source: the job name is a variable in a
	// real payload, the workflow only exists in the github context.
	assert.Equal(t, "probe-test", a[annotationJobName])
	assert.Equal(t, ".github/workflows/probe-test.yml", a[annotationWorkflow])
}
