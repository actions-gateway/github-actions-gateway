//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation"
)

// TestAGC_Q467_WorkerPodNameAtTheDNSLabelBoundary provisions a worker for an owner
// whose name length puts the 63-char cut exactly on one of the plan ID UUID's
// hyphens, and asserts the pod is created.
//
// The unit tests in the provisioner package assert the derived name against
// k8s.io/apimachinery's DNS-label rules; this one asserts it against the authority
// that actually rejected it — a real API server. Before the fix the create failed
// with `metadata.name: Invalid value: "…-"`, so no worker pod existed for this owner
// at all and no job could ever run, while GitHub reported only that the runner had
// lost communication (Q467).
//
// The owner name is 23 characters on purpose: safeName appends "-" plus a 7-char
// hash, so the assembled `runner-<owner>-<planID>` name put character 63 on the UUID
// hyphen at index 23. Lengths 10, 28, 33 and 38 land on the other four hyphens; the
// exhaustive length sweep lives in the provisioner unit tests.
func TestAGC_Q467_WorkerPodNameAtTheDNSLabelBoundary(t *testing.T) {
	const (
		nsName = "agc-q467-podname"
		// 23 characters — the boundary length for the plan ID below.
		rgName = "q467-boundary-name-abcd"
		planID = "a20852f8-1e2b-4c3d-9f10-77b6d4c1a9e5"
	)
	require.Len(t, rgName, 23, "the boundary this test pins depends on the owner name length")

	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, rgName, 2)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	// A UUID-shaped plan ID, as GitHub issues: its hyphens at indices 8, 13, 18 and
	// 23 are what the pre-fix cut could land on. The stub's default ("test-plan-N")
	// has none, which is one reason nothing caught this.
	brokerStub.SetAcquireJobResponse(map[string]any{
		"plan": map[string]string{"planId": planID},
	})
	t.Cleanup(func() { brokerStub.SetAcquireJobResponse(nil) })

	startAGCReconcilerWithProvisioner(t, provisionerOptions{})

	id := enqueueJobOnOwnerSession(15*time.Second, rgName, nil, broker.RunnerJobRequestBody{})
	require.NotEmpty(t, id, "a session for %s should register", rgName)

	// The API server accepted the name — the assertion the pre-fix code failed.
	pod := waitForWorkerPod(t, nsName, rgName)

	assert.LessOrEqual(t, len(pod.Name), 63)
	assert.Empty(t, validation.IsDNS1123Label(pod.Name), "worker pod name %q", pod.Name)
	assert.False(t, strings.HasSuffix(pod.Name, "-"), "worker pod name %q must not end on a hyphen", pod.Name)
	// The budget split keeps a readable head of each segment, so the pod is still
	// traceable to its owner and its job by name alone.
	assert.Contains(t, pod.Name, "q467-boundary")
	assert.Contains(t, pod.Name, "a20852f8")
}
