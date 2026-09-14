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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
// way a restarted AGC does. It addresses the ConfigMap by name rather than by the
// RunnerSet label: the set's recovery-claim ledger carries the same label (Q1108), so a
// label list returns two objects in an order nothing pins.
func (f *orphanFixture) storedInFlight(t *testing.T) []scalesetlistener.InFlightJob {
	t.Helper()
	var cm corev1.ConfigMap
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: "scaleset-guards-" + f.setName}, &cm)
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	var state scalesetlistener.GuardState
	require.NoError(t, json.Unmarshal([]byte(cm.Data["guards.json"]), &state))
	return state.InFlight
}

// recoveryClaimed reports whether podName's recovery is recorded in the RunnerSet's
// recovery-claim ledger — the durable at-most-once record that outlives both the pod
// and the AGC process (Q1108).
func (f *orphanFixture) recoveryClaimed(t *testing.T, podName string) bool {
	t.Helper()
	var cm corev1.ConfigMap
	err := k8sClient.Get(ctx, types.NamespacedName{
		Namespace: f.ns, Name: "scaleset-recovery-claims-" + f.setName,
	}, &cm)
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	var ledger struct {
		Claims map[string]json.RawMessage `json:"claims"`
	}
	require.NoError(t, json.Unmarshal([]byte(cm.Data["recovery-claims.json"]), &ledger))
	_, held := ledger.Claims[podName]
	return held
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

// TestAGC_ScaleSet_RecoveredDrainIsNotReRunAfterARestart is the property Q1108 adds to
// the two above, and the one the pod could never carry: the at-most-once record has to
// outlive the pod AND the process. A drain is recovered while the AGC is up, the pod
// goes, and the in-flight record stays — because the only thing that retires it is the
// job's conclusion, which arrives from GitHub seconds later and here never arrives at
// all. To the restarted AGC's orphan scan that entry is indistinguishable from a worker
// lost unobserved, so without the recovery-claim ledger it re-runs a run that was
// already re-run, spending a second slot of one run's budget for one disruption.
//
// A real apiserver is what makes this meaningful: the claim is in a real ConfigMap,
// written by one manager generation and read by another, and the pod is really absent.
// The narrower race the ledger was built for — a claim landing after the kubelet has
// removed the object — is pinned in the provisioner's own tests, because envtest cannot
// schedule that window deterministically.
func TestAGC_ScaleSet_RecoveredDrainIsNotReRunAfterARestart(t *testing.T) {
	f := newOrphanFixture(t, "v2-rs-ss-claimed", "ss-claimed", "linux-claimed")

	stopFirst := f.startAGC(t)
	f.srv.Enqueue(f.ssID)
	pod := f.worker(t)
	require.Eventually(t, func() bool { return len(f.storedInFlight(t)) == 1 }, 20*time.Second,
		100*time.Millisecond, "the run behind the live worker must reach the guard ConfigMap")

	// The drain, with the AGC watching: the kubelet's sequence, mark then terminal phase.
	holdWithFinalizer(t, f.ns, pod.Name)
	evictPod(t, &pod)
	publishTerminalFailure(t, f.ns, pod.Name)

	require.Eventually(t, func() bool { return f.reruns.Load() == 1 }, 30*time.Second,
		100*time.Millisecond, "the drained worker's run must be re-run once")
	require.True(t, f.recoveryClaimed(t, pod.Name),
		"the recovery must be recorded durably, or nothing later can tell it happened")

	// The pod goes, taking its eviction-handled stamp with it — which is exactly why
	// that stamp could not be the record.
	releaseFinalizer(t, f.ns, pod.Name)
	requirePodGone(t, f.ns, pod.Name)
	require.Len(t, f.storedInFlight(t), 1,
		"the in-flight record must still be there, or the restart below poses no question")

	stopFirst()
	f.startAGC(t)

	assert.Never(t, func() bool { return f.reruns.Load() > 1 }, 15*time.Second, 200*time.Millisecond,
		"a disruption already recovered must not be re-run again by the restarted AGC's orphan scan")
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
