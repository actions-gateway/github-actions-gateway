//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Q844 — a scale-set worker that was already gone when the AGC came back.
//
// Preemption and drain both DELETE their victim, and on this tier the pod is the only
// place the run identity is recorded. An AGC down across the teardown therefore saw
// nothing and issued no re-run. The replacement is a record of the runs the gateway has
// workers for, written into the per-RunnerSet guard ConfigMap the listener already
// persists ahead of its message deletes, and read back by the reconciler on the way up.
//
// The AGC restart is the whole scenario, so these tests take it literally: one manager
// generation writes the record, it is stopped, the world changes while nothing is
// watching, and a second generation has only the ConfigMap to go on. envtest is what
// makes that meaningful — the record is in a real ConfigMap, the pod is really absent,
// and the ordering against the reaper and the listener's first poll is the real one.
//
// Design boundary: docs/design/04-operational-flows.md §4.2, "Why preemption deletes
// rather than evicts". Operator-facing: docs/operations/troubleshooting.md.

// orphanFixture is one scale-set RunnerSet wired to a fake broker and a counting
// stand-in for rerun-failed-jobs, which survive a manager generation being stopped and
// another started — the two halves an AGC restart keeps.
type orphanFixture struct {
	ns, setName, label string
	srv                *scalesettest.Server
	gitHub             *httptest.Server
	reruns             *atomic.Int64
	ssID               int
}

// newOrphanFixture creates the namespace, gateway, template and RunnerSet, and the
// broker and GitHub fakes. It starts no manager: each test decides how many generations
// to run and what happens between them.
func newOrphanFixture(t *testing.T, ns, setName, label string) *orphanFixture {
	t.Helper()
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	gitHub, reruns := rerunCounter(t)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet(setName, ns, "gw", label, 3)
	// One second is the CRD's floor; the recovery waits it out before calling GitHub.
	rs.Spec.EvictionRetryDelay = &metav1.Duration{Duration: time.Second}
	rs.Spec.MaxEvictionRetries = ptr.To(int32(2))
	// An hour, so nothing here is reaped: the discriminator is whether the worker pod is
	// there, and the reaper is the one thing in the AGC that would remove one.
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		bg := context.Background()
		_ = k8sClient.Delete(bg, rs)
		_ = k8sClient.Delete(bg, &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(bg, &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	return &orphanFixture{ns: ns, setName: setName, label: label, srv: srv, gitHub: gitHub, reruns: reruns}
}

// startAGC brings up one manager generation against the fixture's fakes and returns its
// stop function. Each generation gets a fresh Provisioner, which is what makes the
// once-per-process orphan scan run again — the restart the mechanism is built for.
func (f *orphanFixture) startAGC(t *testing.T) func() {
	t.Helper()
	stop := startRunnerSetReconcilerWithScaleSet(t, f.srv, func(p *provisioner.Provisioner) {
		p.GitHubAPIURL = f.gitHub.URL
		p.HTTPClient = f.gitHub.Client()
	})
	require.Eventually(t, func() bool {
		id, ok := f.srv.ScaleSetIDByName(f.label)
		f.ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register its scale set")
	waitForSetReadyReason(t, f.ns, f.setName, metav1.ConditionTrue, v2alpha1.ReasonListenerActive)
	return stop
}

// storedInFlight reads the in-flight records out of the RunnerSet's guard ConfigMap, the
// way a restarted AGC does. It finds the ConfigMap by the same label an operator would.
func (f *orphanFixture) storedInFlight(t *testing.T) []scalesetlistener.InFlightJob {
	t.Helper()
	var cms corev1.ConfigMapList
	require.NoError(t, k8sClient.List(ctx, &cms, client.InNamespace(f.ns),
		client.MatchingLabels{provisioner.LabelRunnerSet: f.setName}))
	if len(cms.Items) == 0 {
		return nil
	}
	var state scalesetlistener.GuardState
	require.NoError(t, json.Unmarshal([]byte(cms.Items[0].Data["guards.json"]), &state))
	return state.InFlight
}

// worker blocks until the set has a worker pod and returns it.
func (f *orphanFixture) worker(t *testing.T) corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(f.ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: f.setName}); err != nil {
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
	return pod
}

// TestAGC_ScaleSet_WorkerLostWhileTheAGCWasDownIsRecovered is the property Q844 exists
// for, played out in order: a worker is running and recorded, the AGC goes away, the
// worker is destroyed and its job concludes failed at GitHub — which is what the Q385
// SIGTERM relay does on a preemption — and the AGC comes back to a pod that is gone and
// a conclusion that says nothing about why.
//
// Before Q844 the second generation had nothing to read and the run needed a manual
// re-run. The persisted record is the only thing that changes that.
func TestAGC_ScaleSet_WorkerLostWhileTheAGCWasDownIsRecovered(t *testing.T) {
	f := newOrphanFixture(t, "v2-rs-ss-vanished", "ss-vanished", "linux-vanished")

	stopFirst := f.startAGC(t)
	job := f.srv.Enqueue(f.ssID)
	pod := f.worker(t)
	require.Eventually(t, func() bool { return len(f.storedInFlight(t)) == 1 }, 20*time.Second,
		100*time.Millisecond, "the run behind the live worker must reach the guard ConfigMap")
	stopFirst()

	// The disruption, with nothing watching. The relay's conclusion is what stops the
	// assignment replaying, so the record is the only trace left that this run ever had
	// a worker.
	before := f.reruns.Load()
	require.NoError(t, k8sClient.Delete(ctx, &pod))
	requirePodGone(t, f.ns, pod.Name)
	require.True(t, f.srv.CompleteAssignedJob(f.ssID, job.JobID, "failed"),
		"the relay concludes the preempted job failed at GitHub")

	f.startAGC(t)

	assert.Eventually(t, func() bool { return f.reruns.Load() > before }, 30*time.Second,
		200*time.Millisecond,
		"a restarted AGC must re-run the run whose worker went away while it was down")
}

// TestAGC_ScaleSet_LiveWorkerSurvivesARestart is the control, and the one that makes the
// test above mean something. Same record, same restart, same scan — the worker pod is
// simply still there, which is what a job that is still running and a job that genuinely
// failed both look like. Re-running either would be the retry loop the design refuses.
func TestAGC_ScaleSet_LiveWorkerSurvivesARestart(t *testing.T) {
	f := newOrphanFixture(t, "v2-rs-ss-live", "ss-live", "linux-live")

	stopFirst := f.startAGC(t)
	f.srv.Enqueue(f.ssID)
	f.worker(t)
	require.Eventually(t, func() bool { return len(f.storedInFlight(t)) == 1 }, 20*time.Second,
		100*time.Millisecond, "the run behind the live worker must reach the guard ConfigMap")
	stopFirst()

	before := f.reruns.Load()
	f.startAGC(t)

	assert.Never(t, func() bool { return f.reruns.Load() > before }, 10*time.Second,
		200*time.Millisecond, "a job whose worker is still running is owed no re-run")
	assert.Len(t, f.storedInFlight(t), 1,
		"and its record stays, so a later disruption is still recoverable")
}
