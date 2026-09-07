package runnercore

import (
	"strings"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnerimage"
	"github.com/actions-gateway/github-actions-gateway/api/apiconditions"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	registryDigest = "sha256:f2387135856decdecbf780a2bfbc9debe9c2dffd742f150302444b3775474681"
	digestOnlyRef  = "ghcr.io/acme/runner@" + registryDigest
	customTagRef   = "ghcr.io/acme/runner:v3-cuda"
	currentTagRef  = "ghcr.io/acme/runner:2.335.1"
)

func done(version string, found bool) *runnerimage.Lookup {
	return &runnerimage.Lookup{State: runnerimage.Done, Result: runnerimage.Reading{
		Digest: registryDigest, Platform: "linux/amd64", Version: version, Found: found}}
}

// Q988: a completed registry reading is the verdict, whatever the tag says.
func TestRegistryReadingOverridesTag(t *testing.T) {
	t.Run("digest-only reference becomes checkable", func(t *testing.T) {
		cond := WorkerRunnerVersionConditionWithRegistry(digestOnlyRef, done("2.335.1", true), 3)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, apiconditions.ReasonWorkerImageCurrent, cond.Reason)
		assert.Contains(t, cond.Message, "read from the registry at "+registryDigest+" (linux/amd64)")
		assert.NotContains(t, cond.Message, "its tag claims")
		assert.Equal(t, int64(3), cond.ObservedGeneration)
	})
	t.Run("custom tag below the floor is a verdict", func(t *testing.T) {
		cond := WorkerRunnerVersionConditionWithRegistry(customTagRef, done("2.328.0", true), 1)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, apiconditions.ReasonWorkerImageBelowMinimum, cond.Reason)
		assert.Contains(t, cond.Message, "2.328.0")
		assert.Contains(t, cond.Message, "update workerImage")
	})
	t.Run("layers outrank a tag that disagrees", func(t *testing.T) {
		cond := WorkerRunnerVersionConditionWithRegistry(currentTagRef, done("2.328.0", true), 1)
		assert.Equal(t, metav1.ConditionTrue, cond.Status, "the tag claims current; the image is not")
		assert.Contains(t, cond.Message, "rather than the 2.335.1 its tag claims")
	})
	t.Run("agreeing tag is not called out", func(t *testing.T) {
		cond := WorkerRunnerVersionConditionWithRegistry(currentTagRef, done("2.335.1", true), 1)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.NotContains(t, cond.Message, "its tag claims")
	})
	t.Run("not runner-derived is Unknown even under a version tag", func(t *testing.T) {
		cond := WorkerRunnerVersionConditionWithRegistry(currentTagRef, done("", false), 1)
		assert.Equal(t, metav1.ConditionUnknown, cond.Status)
		assert.Equal(t, apiconditions.ReasonWorkerImageVersionUnknown, cond.Reason)
		assert.Contains(t, cond.Message, "carries no bin/Runner.Listener.deps.json")
		assert.Contains(t, cond.Message, "its tag claims 2.335.1, which the image does not bear out")

		cond = WorkerRunnerVersionConditionWithRegistry(digestOnlyRef, done("", false), 1)
		assert.Equal(t, metav1.ConditionUnknown, cond.Status)
		assert.NotContains(t, cond.Message, "its tag claims")
	})
	t.Run("unparsable manifest version is Unknown", func(t *testing.T) {
		cond := WorkerRunnerVersionConditionWithRegistry(digestOnlyRef, done("2.335.1-rc1", true), 1)
		assert.Equal(t, metav1.ConditionUnknown, cond.Status)
		assert.Equal(t, apiconditions.ReasonWorkerImageVersionUnknown, cond.Reason)
		assert.Contains(t, cond.Message, `"2.335.1-rc1"`)
	})
}

// Until the registry answers, the tag verdict stands — a reachable registry is not a
// precondition of the signal Q715 already publishes.
func TestRegistryPendingAndFailedKeepTagVerdict(t *testing.T) {
	pending := &runnerimage.Lookup{State: runnerimage.Pending}
	failed := &runnerimage.Lookup{State: runnerimage.Failed, Attempts: 2, Err: "manifest v3-cuda: HTTP 401"}

	for name, lookup := range map[string]*runnerimage.Lookup{"pending": pending, "failed": failed} {
		t.Run(name+" under a version tag", func(t *testing.T) {
			cond := WorkerRunnerVersionConditionWithRegistry(currentTagRef, lookup, 1)
			base := WorkerRunnerVersionCondition(currentTagRef, 1)
			assert.Equal(t, base.Status, cond.Status)
			assert.Equal(t, base.Reason, cond.Reason)
			assert.True(t, strings.HasPrefix(cond.Message, base.Message), "the tag verdict's message is kept and extended: %q", cond.Message)
		})
		t.Run(name+" under a custom tag", func(t *testing.T) {
			cond := WorkerRunnerVersionConditionWithRegistry(customTagRef, lookup, 1)
			assert.Equal(t, metav1.ConditionUnknown, cond.Status)
			assert.Equal(t, apiconditions.ReasonWorkerImageVersionUnknown, cond.Reason)
		})
	}
	assert.Contains(t, WorkerRunnerVersionConditionWithRegistry(currentTagRef, pending, 1).Message, "registry read of the image is in progress")
	failedMsg := WorkerRunnerVersionConditionWithRegistry(currentTagRef, failed, 1).Message
	assert.Contains(t, failedMsg, "attempt 2: manifest v3-cuda: HTTP 401")
	assert.Contains(t, failedMsg, "the tag's claim alone")
}

// A nil lookup is byte-for-byte the pre-Q988 verdict, which is what every caller
// without a resolver gets.
func TestNilLookupIsTagVerdict(t *testing.T) {
	for _, image := range []string{currentTagRef, customTagRef, digestOnlyRef, "ghcr.io/acme/runner:2.328.0"} {
		assert.Equal(t, WorkerRunnerVersionCondition(image, 7), WorkerRunnerVersionConditionWithRegistry(image, nil, 7), image)
	}
}
