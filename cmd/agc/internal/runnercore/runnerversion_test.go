package runnercore_test

import (
	"strings"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/actions-gateway/github-actions-gateway/agc/names"
	"github.com/actions-gateway/github-actions-gateway/api/apiconditions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestWorkerImageRunnerVersion(t *testing.T) {
	tests := []struct {
		name    string
		image   string
		want    string
		wantOK  bool
		comment string
	}{
		{name: "tag and digest", image: "ghcr.io/actions/actions-runner:2.335.1@sha256:abc", want: "2.335.1", wantOK: true},
		{name: "tag only", image: "ghcr.io/actions/actions-runner:2.329.0", want: "2.329.0", wantOK: true},
		{name: "leading v", image: "acme.io/runner:v2.330.1", want: "2.330.1", wantOK: true},
		{name: "registry port", image: "registry.example.com:5000/runner:2.331.0", want: "2.331.0", wantOK: true},
		{name: "digest only", image: "ghcr.io/actions/actions-runner@sha256:abc", wantOK: false,
			comment: "no tag to read: the digest says nothing about the runner inside"},
		{name: "no tag", image: "ghcr.io/actions/actions-runner", wantOK: false},
		{name: "floating tag", image: "ghcr.io/actions/actions-runner:latest", wantOK: false},
		{name: "tenant tag", image: "acme.io/runner:v3-cuda", wantOK: false},
		{name: "two-component tag", image: "acme.io/runner:2.335", wantOK: false},
		{name: "prerelease tag", image: "acme.io/runner:2.335.1-rc1", wantOK: false,
			comment: "actions/runner publishes none, so this is a tenant's own naming"},
		{name: "empty tag", image: "acme.io/runner:", wantOK: false},
		{name: "port but no tag", image: "registry.example.com:5000/runner", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := runnercore.WorkerImageRunnerVersion(tc.image)
			assert.Equal(t, tc.wantOK, ok, tc.comment)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestWorkerRunnerVersionConditionBelowMinimum pins the signal Q715 exists for: an
// image one patch below the enforced floor reports too-old, and names both versions
// so the operator can act without looking anything up.
func TestWorkerRunnerVersionConditionBelowMinimum(t *testing.T) {
	cond := runnercore.WorkerRunnerVersionCondition("ghcr.io/actions/actions-runner:2.328.0", 7)

	assert.Equal(t, apiconditions.ConditionRunnerVersionTooOld, cond.Type)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, apiconditions.ReasonWorkerImageBelowMinimum, cond.Reason)
	assert.Equal(t, int64(7), cond.ObservedGeneration)
	assert.Contains(t, cond.Message, "2.328.0")
	assert.Contains(t, cond.Message, names.MinRunnerVersion)
	assert.Contains(t, cond.Message, "workerImage")
}

func TestWorkerRunnerVersionConditionAtAndAboveMinimum(t *testing.T) {
	for _, image := range []string{
		"ghcr.io/actions/actions-runner:" + names.MinRunnerVersion,
		names.DefaultWorkerImage,
	} {
		cond := runnercore.WorkerRunnerVersionCondition(image, 1)
		assert.Equal(t, metav1.ConditionFalse, cond.Status, image)
		assert.Equal(t, apiconditions.ReasonWorkerImageCurrent, cond.Reason, image)
	}
}

// TestWorkerRunnerVersionConditionUnknown is the negative assertion that matters: an
// unreadable reference must not report False. False is a verified-current claim, and
// a custom image is exactly where a stale runner hides.
func TestWorkerRunnerVersionConditionUnknown(t *testing.T) {
	for _, image := range []string{
		"ghcr.io/actions/actions-runner@sha256:abc",
		"acme.io/runner:v3-cuda",
		"acme.io/runner:latest",
	} {
		cond := runnercore.WorkerRunnerVersionCondition(image, 1)
		assert.Equal(t, metav1.ConditionUnknown, cond.Status, image)
		assert.Equal(t, apiconditions.ReasonWorkerImageVersionUnknown, cond.Reason, image)
		assert.Contains(t, cond.Message, image)
	}
}

// TestWorkerRunnerVersionOrdering pins the comparison against the lexical ordering it
// must not use: "2.9.0" sorts after "2.329.0" as a string and before it as a version.
func TestWorkerRunnerVersionOrdering(t *testing.T) {
	older := runnercore.WorkerRunnerVersionCondition("acme.io/runner:2.9.0", 1)
	assert.Equal(t, metav1.ConditionTrue, older.Status, "2.9.0 is older than the 2.329.0 floor")

	newer := runnercore.WorkerRunnerVersionCondition("acme.io/runner:2.1000.0", 1)
	assert.Equal(t, metav1.ConditionFalse, newer.Status, "2.1000.0 is newer than the 2.329.0 floor")
}

// TestMinRunnerVersionParses guards the constant the condition is judged against: a
// malformed floor would silently degrade every set to Unknown.
func TestMinRunnerVersionParses(t *testing.T) {
	require.Equal(t, 3, len(strings.Split(names.MinRunnerVersion, ".")),
		"MinRunnerVersion must be MAJOR.MINOR.PATCH")

	got, ok := runnercore.WorkerImageRunnerVersion("acme.io/runner:" + names.MinRunnerVersion)
	require.True(t, ok, "MinRunnerVersion must parse as a runner version")
	assert.Equal(t, names.MinRunnerVersion, got)
}

// TestShippedRunnerVersionMeetsMinimum keeps the two pinned constants consistent: the
// default worker image must not itself be below the floor GAG warns tenants about.
func TestShippedRunnerVersionMeetsMinimum(t *testing.T) {
	cond := runnercore.WorkerRunnerVersionCondition(names.DefaultWorkerImage, 1)
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"names.RunnerVersion is below names.MinRunnerVersion: the shipped default cannot register with GitHub")
}
