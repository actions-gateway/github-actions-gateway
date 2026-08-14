package controller

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Q844: the reconciler half of orphaned-worker recovery. The listener writes the
// in-flight records; this is what reads them back on the way up and turns one whose
// worker pod is gone into a re-run.
//
// The ConfigMap round-trip is part of the mechanism rather than an implementation
// detail: the record crosses a process boundary as JSON, and an identity field lost in
// serialization would fail silently, as a recovery that names no run.

// orphanRecoveryFixture wires a reconciler against a fake cluster holding pods, plus a
// counting stand-in for the rerun-failed-jobs endpoint.
func orphanRecoveryFixture(t *testing.T, rs *v2alpha1.RunnerSet, objs ...client.Object) (*RunnerSetReconciler, *atomic.Int64) {
	t.Helper()

	rerunCount := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rerunCount.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	// The gateway and template are what make Target.Resolve answer, which the recovery
	// needs for the owner's maxEvictionRetries.
	c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).
		WithObjects(append([]client.Object{rs, gwObj("gw", rs.Namespace, ""), tmplObj("tmpl", rs.Namespace)}, objs...)...).
		Build()

	p := provisioner.NewProvisioner(c, nil, nil)
	p.TokenFunc = func(context.Context) (string, error) { return "tok", nil }
	p.GitHubAPIURL = srv.URL
	p.HTTPClient = srv.Client()
	p.EvictionRetryDelay = 0

	return &RunnerSetReconciler{Client: c, Provisioner: p}, rerunCount
}

// storeInFlight persists an in-flight record through the real ConfigMap-backed store, so
// the test reads it back the way a restarted AGC does.
func storeInFlight(t *testing.T, r *RunnerSetReconciler, rs *v2alpha1.RunnerSet, jobIDs ...string) {
	t.Helper()
	state := scalesetlistener.GuardState{}
	for _, id := range jobIDs {
		state.InFlight = append(state.InFlight, scalesetlistener.InFlightJob{
			JobID: id, Owner: "myorg", Repository: "myrepo", RunID: "4242",
			ProvisionedAt: time.Now().UTC(),
		})
	}
	require.NoError(t, r.scaleSetGuardStore(rs).Save(context.Background(), state))
}

// TestRecoverOrphanedScaleSetWorkers_RerunsARecordWhosePodIsGone is the end-to-end
// reconciler behaviour: a record survives in the ConfigMap, its pod does not, and the run
// is re-run. Before Q844 nothing read the record and the run needed a manual re-run.
//
// It exercises the real store, so it also pins the wire shape: the identity the recovery
// addresses GitHub with has to come back out of the ConfigMap's JSON intact.
func TestRecoverOrphanedScaleSetWorkers_RerunsARecordWhosePodIsGone(t *testing.T) {
	rs := rsObj("linux-large", "tenant-a", nil)
	r, rerunCount := orphanRecoveryFixture(t, rs)
	storeInFlight(t, r, rs, "job-gone")

	<-r.recoverOrphanedScaleSetWorkers(context.Background(), slog.Default(), rs)

	assert.Equal(t, int64(1), rerunCount.Load(),
		"a persisted record with no worker pod is a run whose worker was lost unobserved")
}

// TestRecoverOrphanedScaleSetWorkers_NoStoredStateIsANoOp keeps the common path free: a
// set that has never persisted anything, and a set whose jobs have all concluded, must
// both cost nothing and recover nothing.
func TestRecoverOrphanedScaleSetWorkers_NoStoredStateIsANoOp(t *testing.T) {
	rs := rsObj("linux-large", "tenant-a", nil)
	r, rerunCount := orphanRecoveryFixture(t, rs)

	<-r.recoverOrphanedScaleSetWorkers(context.Background(), slog.Default(), rs)
	assert.Equal(t, int64(0), rerunCount.Load())

	require.NoError(t, r.scaleSetGuardStore(rs).Save(context.Background(),
		scalesetlistener.GuardState{Completed: []string{"job-done"}}))
	<-r.recoverOrphanedScaleSetWorkers(context.Background(), slog.Default(), rs)
	assert.Equal(t, int64(0), rerunCount.Load())
}

// TestRecoverOrphanedScaleSetWorkers_UnreadableStoreRecoversNothing pins the failure
// direction. A ConfigMap this cannot parse must recover nothing: acting on a partial read
// would re-run runs on no evidence at all.
func TestRecoverOrphanedScaleSetWorkers_UnreadableStoreRecoversNothing(t *testing.T) {
	rs := rsObj("linux-large", "tenant-a", nil)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: rs.Namespace, Name: scaleSetGuardConfigMapName(rs.Name),
		},
		Data: map[string]string{scaleSetGuardDataKey: "{not json"},
	}
	r, rerunCount := orphanRecoveryFixture(t, rs, cm)

	<-r.recoverOrphanedScaleSetWorkers(context.Background(), slog.Default(), rs)
	assert.Equal(t, int64(0), rerunCount.Load())
}
