package scalesetlistener_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The session gauge is the only signal for a listener that never reaches the queue: it
// counts no poll error and is assigned nothing, so ActionsGatewayScaleSetNoSession reads
// it at 0 (Q1275). Each failure path below must leave it at 0.

func newSessionTestListener(t *testing.T, srv *scalesettest.Server, m scalesetlistener.MetricsRecorder, group string) *scalesetlistener.Listener {
	t.Helper()
	l, err := scalesetlistener.New(scalesetlistener.Config{
		Client:          newClient(t, srv),
		ScaleSetName:    "linux",
		OwnerName:       "acme/linux",
		RunnerGroupName: group,
		Provision:       func(context.Context, scalesetlistener.Job) error { return nil },
		Capacity:        fixedCapacity(1),
		Metrics:         m,
	})
	require.NoError(t, err)
	return l
}

func TestListener_SessionOpenZeroWhenSessionCreateFails(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	srv.FailSessionCreate(true)

	m := newCountingMetrics()
	_, err := newSessionTestListener(t, srv, m, "").Start(context.Background())
	require.Error(t, err)
	assert.Equal(t, []bool{false}, m.sessionOpenWrites(), "a failed session create must publish the session gauge at 0")
}

func TestListener_SessionOpenZeroWhenEnsureScaleSetFails(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	srv.FailRunnerGroups(true)

	m := newCountingMetrics()
	_, err := newSessionTestListener(t, srv, m, "tenant-a").Start(context.Background())
	require.Error(t, err)
	assert.Equal(t, []bool{false}, m.sessionOpenWrites(), "a Start failing before the session must publish the session gauge at 0")
}

func TestListener_SessionOpenTracksInLoopRecreate(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	m := newCountingMetrics()
	_, ssID := startListener(t, srv, fixedCapacity(1), &recordingProvisioner{srv: srv}, m)
	require.Equal(t, []bool{true}, m.sessionOpenWrites(), "an opened session publishes the gauge at 1")

	// The session expires server-side and the re-create fails: the loop keeps polling a
	// dead session, so only the gauge can say so.
	srv.FailSessionCreate(true)
	srv.DropSession(ssID)
	require.Eventually(t, func() bool {
		w := m.sessionOpenWrites()
		return len(w) > 1 && !w[len(w)-1]
	}, 5*time.Second, 5*time.Millisecond, "a failed re-create after a 404 must publish the gauge at 0")

	srv.FailSessionCreate(false)
	require.Eventually(t, func() bool {
		w := m.sessionOpenWrites()
		return w[len(w)-1]
	}, 5*time.Second, 5*time.Millisecond, "a successful re-create must publish the gauge at 1 again")
}

func TestMetrics_SessionOpenSeriesAndDelete(t *testing.T) {
	m := scalesetlistener.NewMetrics(prometheus.NewRegistry())
	r := m.RecorderFor("tenant-a", "set-1")

	r.SetSessionOpen(true)
	assert.Equal(t, 1.0, testutil.ToFloat64(m.SessionOpen.WithLabelValues("tenant-a", "set-1")))
	r.SetSessionOpen(false)
	assert.Equal(t, 0.0, testutil.ToFloat64(m.SessionOpen.WithLabelValues("tenant-a", "set-1")))

	// A deleted set must not leave a 0 behind, or the alert would fire on it forever.
	m.DeleteRunnerSet("tenant-a", "set-1")
	assert.Equal(t, 0, testutil.CollectAndCount(m.SessionOpen), "DeleteRunnerSet drops the session series")
}
