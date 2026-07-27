//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Q421 — does a node drain reach eviction recovery?
//
// `kubectl drain` does not signal pods; it POSTs to each pod's `pods/<name>/eviction`
// subresource, and a permitted eviction is a DELETE. That is a different thing from the
// kubelet's node-pressure eviction, which is what leaves a pod in PodFailed with
// Status.Reason == "Evicted" — the only shape either tier's recovery acts on
// (provisioner.provision step 7 on classic, evictedAwaitingRecovery on scale-set).
//
// Q417 shipped scale-set eviction detection scoped to exactly that shape, on the stated
// reasoning that the drain path is already owned by the worker wrapper's SIGTERM relay
// (Q385) and so needs no recovery. These two tests are what put a measurement under
// that reasoning instead of a code reading: they drive the REAL eviction subresource on
// a REAL apiserver, against a worker pod that came out of the real provisioning path,
// and assert what recovery does with it on each tier.
//
// What this venue can and cannot settle. envtest runs an apiserver but no kubelet and
// no scheduler, so a worker pod here has no nodeName and an eviction removes it at once
// — the pod is *deleted while Running*, with no terminal phase in between. That is one
// of the two outcomes a real drain can produce; the other (kubelet drives the container
// to exit and publishes a terminal phase before the object goes away) needs a real
// kubelet and is measured by E2E_AGC_WorkerNodeDrain. Both are covered because neither
// outcome carries reason "Evicted", but only the e2e run establishes which one a real
// drain actually produces. The measured result and the design reasoning behind the
// exclusion live in docs/design/04-operational-flows.md §4.2; the operator-facing
// consequence is docs/operations/troubleshooting.md, "Draining a Node Does Not
// Auto-Re-Run the Jobs It Interrupts".

// evictPod POSTs to the pod's eviction subresource, which is precisely what
// `kubectl drain` does for each pod it removes. It deliberately does NOT delete the pod
// directly: the whole question is whether the API path a drain uses reaches eviction
// recovery, and a plain Delete would beg it.
func evictPod(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	err := k8sClient.SubResource("eviction").Create(ctx, pod, &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name},
	})
	require.NoError(t, err, "the eviction subresource must admit the worker pod (no PDB guards worker pods)")
}

// requirePodGone blocks until the pod is absent from the API server.
func requirePodGone(t *testing.T, namespace, name string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var got corev1.Pod
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &got)
		return apierrors.IsNotFound(err)
	}, 20*time.Second, 50*time.Millisecond, "an admitted eviction must remove the worker pod")
}

// rerunCounter is a stand-in for the GitHub REST API that counts rerun-failed-jobs
// calls. Both tiers reach GitHub through the same handleEviction → rerunFailedJobs
// path, so one counter is the observable for "recovery fired" on either.
func rerunCounter(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "rerun-failed-jobs") {
			calls.Add(1)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestAGC_Drain_ClassicWorkerEviction_DoesNotRerun measures the classic tier's drain
// path. The provisioner is wired with the production InformerPodWaiter — not the poll
// fallback — because the two are only interchangeable for a pod that reaches a terminal
// phase, and this pod never does.
//
// The measured outcome: the eviction removes the pod, the waiter reports the vanished
// pod as PodSucceeded, provision() takes the non-eviction branch, and no rerun is
// requested. A drained classic worker gets no automatic recovery from the AGC — whatever
// requeue the job gets has to come from the runner's own report.
func TestAGC_Drain_ClassicWorkerEviction_DoesNotRerun(t *testing.T) {
	const nsName = "agc-drain-classic"
	const rgName = "drain-rg"
	createNSForAGC(t, nsName)

	fakeGitHub, rerunCalls := rerunCounter(t)

	rg := newRunnerGroup(nsName, rgName, 2)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	startAGCReconcilerWithProvisioner(t, provisionerOptions{
		githubAPIURL: fakeGitHub.URL,
		// A budget of 2 means an absent rerun is the drain path's own doing, not a
		// budget the test starved. The eviction test next door fires on the same
		// wiring with maxEvictionRetries=1.
		maxEvictionRetries: 2,
		informerWaiter:     true,
	})

	require.Eventually(t, func() bool {
		return len(brokerStub.ActiveSessionsForOwner(rgName)) >= 1
	}, 15*time.Second, time.Millisecond, "%s session should register", rgName)

	// A complete run identity, so a missing rerun cannot be explained away as
	// handleEviction's runID == "" early return.
	brokerStub.SetAcquireJobResponse(map[string]interface{}{
		"plan":   map[string]string{"planId": "drain-plan-1"},
		"run_id": 4210,
		"variables": map[string]interface{}{
			"system.github.repository": map[string]string{"value": "owner/repo"},
			"system.github.run_id":     map[string]string{"value": "4210"},
		},
	})
	t.Cleanup(func() { brokerStub.SetAcquireJobResponse(nil) })

	sid := enqueueJobOnOwnerSession(15*time.Second, rgName, map[string]bool{}, broker.RunnerJobRequestBody{})
	require.NotEmpty(t, sid, "should have found %s session to enqueue on", rgName)

	pod := waitForWorkerPod(t, nsName, rgName)
	require.Empty(t, pod.Status.Reason,
		"the worker pod must start out with no Status.Reason, or the assertion below proves nothing")

	// Drain the node the pod is on, as far as this pod is concerned.
	evictPod(t, &pod)
	requirePodGone(t, nsName, pod.Name)

	// The job Secret is deleted on provision()'s way out, so its absence is the signal
	// that the whole handler ran to completion — including step 7, the eviction branch.
	require.Eventually(t, func() bool {
		var secrets corev1.SecretList
		if err := k8sClient.List(ctx, &secrets,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": rgName},
		); err != nil {
			return false
		}
		for _, s := range secrets.Items {
			if strings.HasPrefix(s.Name, "job-") {
				return false
			}
		}
		return true
	}, 20*time.Second, 50*time.Millisecond,
		"the job Secret must be reclaimed, which is how we know provision() reached its eviction branch")

	// The measurement. handleEviction waits out evictionRetryDelay before calling
	// GitHub, so give a fired rerun room to appear before concluding none did.
	assert.Never(t, func() bool { return rerunCalls.Load() > 0 }, 3*time.Second, 100*time.Millisecond,
		"a drained (evicted-via-API, i.e. deleted) classic worker must not reach eviction recovery: "+
			"provision() acts only on PodFailed/Evicted, which a deletion never produces")
}

// TestAGC_Drain_ScaleSetWorkerEviction_DoesNotRecover is the same measurement on the
// tier Q417 built. It is deliberately the twin of
// TestV2_RunnerSet_ScaleSet_EvictedWorkerTriggersRerun: same harness, same real
// provisioning path, same worker pod — and the single difference is that the pod leaves
// via the eviction subresource instead of via a kubelet-set PodFailed/Evicted. That one
// substitution is what turns a rerun into no rerun.
func TestAGC_Drain_ScaleSetWorkerEviction_DoesNotRecover(t *testing.T) {
	const ns = "v2-rs-ss-drain"
	const label = "linux-drain"
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	fakeGitHub, rerunCalls := rerunCounter(t)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet("ss-drain", ns, "gw", label, 3)
	rs.Spec.EvictionRetryDelay = &metav1.Duration{Duration: time.Second}
	rs.Spec.MaxEvictionRetries = ptr.To(int32(2))
	// A long TTL keeps the reaper from deleting the pod out from under the recovery
	// pass, so an absent rerun is the drain path's doing rather than lost evidence.
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		bg := context.Background()
		_ = k8sClient.Delete(bg, rs)
		_ = k8sClient.Delete(bg, &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(bg, &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv, func(p *provisioner.Provisioner) {
		p.GitHubAPIURL = fakeGitHub.URL
		p.HTTPClient = fakeGitHub.Client()
	})

	var ssID int
	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(label)
		ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register its scale set")
	waitForSetReadyReason(t, ns, "ss-drain", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	srv.EnqueueJob(ssID)

	var pod corev1.Pod
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: "ss-drain"}); err != nil {
			return false
		}
		for i := range pods.Items {
			if strings.HasPrefix(pods.Items[i].Name, "runner-") {
				pod = pods.Items[i]
				return true
			}
		}
		return false
	}, 20*time.Second, 50*time.Millisecond, "a worker pod must be provisioned for the assigned job")

	// The recoverable identity is present — this pod is exactly the one the eviction
	// twin re-runs. Only the exit path differs.
	require.NotEmpty(t, pod.Annotations[provisioner.AnnotationRunID],
		"the assignment's workflowRunId must have reached the pod, or no rerun could fire either way")

	evictPod(t, &pod)
	requirePodGone(t, ns, pod.Name)

	// The measurement. RecoverEvictedScaleSetWorkers runs every reconcile and lists from
	// the informer cache; an evicted-by-API pod is not in that list at all, and would not
	// pass evictedAwaitingRecovery if it were.
	assert.Never(t, func() bool { return rerunCalls.Load() > 0 }, 3*time.Second, 100*time.Millisecond,
		"a drained (evicted-via-API, i.e. deleted) scale-set worker must not reach eviction recovery: "+
			"Q417 scoped detection to PodFailed/Evicted and left deletion to the SIGTERM relay")
}
