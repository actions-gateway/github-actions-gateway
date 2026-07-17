package scalesetlistener_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Tests for the Listener's session-failure condition/event surface (Q325): the
// ScaleSet path must report the same failure classes the classic listener pushes
// (Degraded/Unauthorized, RateLimited/SustainedRateLimit) — and, beyond classic
// parity, publish the healthy baseline on start and clear an abnormal state when
// the session recovers.

// recordedEvent is one Event captured by statusSink.
type recordedEvent struct {
	eventtype, reason, action, note string
}

// statusSink implements ConditionSetter and EventSink, capturing every push for
// assertions.
type statusSink struct {
	mu     sync.Mutex
	conds  []metav1.Condition
	events []recordedEvent
}

func (s *statusSink) SetCondition(cond metav1.Condition) {
	s.mu.Lock()
	s.conds = append(s.conds, cond)
	s.mu.Unlock()
}

func (s *statusSink) Event(eventtype, reason, action, note string) {
	s.mu.Lock()
	s.events = append(s.events, recordedEvent{eventtype, reason, action, note})
	s.mu.Unlock()
}

// last returns the most recently pushed condition of the given type.
func (s *statusSink) last(condType string) (metav1.Condition, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.conds) - 1; i >= 0; i-- {
		if s.conds[i].Type == condType {
			return s.conds[i], true
		}
	}
	return metav1.Condition{}, false
}

// lastIs reports whether the most recent condition of the given type has the given
// status and reason.
func (s *statusSink) lastIs(condType string, status metav1.ConditionStatus, reason string) bool {
	c, ok := s.last(condType)
	return ok && c.Status == status && c.Reason == reason
}

// eventCount counts captured events with the given reason.
func (s *statusSink) eventCount(reason string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if e.reason == reason {
			n++
		}
	}
	return n
}

// startListenerWithSink starts a listener wired to a statusSink, with fast backoff
// and a short sustained-rate-limit window so the condition paths are drivable in
// test time.
func startListenerWithSink(t *testing.T, srv *scalesettest.Server, sink *statusSink) (*scalesetlistener.Listener, int) {
	t.Helper()
	client := newClient(t, srv)

	l, err := scalesetlistener.New(scalesetlistener.Config{
		Client:                  client,
		ScaleSetName:            "linux",
		OwnerName:               "acme/linux",
		Provision:               func(context.Context, scalesetlistener.Job) error { return nil },
		Capacity:                fixedCapacity(1),
		Conditions:              sink,
		Events:                  sink,
		PollBackoff:             5 * time.Millisecond,
		RateLimitConditionAfter: 30 * time.Millisecond,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done, err := l.Start(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("listener did not stop within 5s")
		}
	})
	return l, l.Status().ScaleSetID
}

// TestListener_PublishesHealthyBaselineOnStart proves a successful Start publishes
// the healthy (False) state of both listener-owned conditions, so a stale abnormal
// condition left by a previously failed listener instance cannot outlive the
// recovery.
func TestListener_PublishesHealthyBaselineOnStart(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	sink := &statusSink{}
	startListenerWithSink(t, srv, sink)

	assert.True(t, sink.lastIs(v2alpha1.ConditionDegraded, metav1.ConditionFalse, v2alpha1.ReasonSessionAuthorized),
		"Start must publish Degraded=False/SessionAuthorized")
	assert.True(t, sink.lastIs(v2alpha1.ConditionRateLimited, metav1.ConditionFalse, v2alpha1.ReasonPollingHealthy),
		"Start must publish RateLimited=False/PollingHealthy")
	assert.Zero(t, sink.eventCount("SessionUnauthorized"), "no warning events on the happy path")
}

// TestListener_SustainedRateLimitSetsAndClearsCondition drives message polling into
// a sustained 429 and asserts RateLimited=True/SustainedRateLimit surfaces once the
// episode outlasts the window — then lifts the limit and asserts the first healthy
// poll clears the condition.
func TestListener_SustainedRateLimitSetsAndClearsCondition(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	sink := &statusSink{}
	startListenerWithSink(t, srv, sink)

	srv.SetRateLimitPolls(true)
	require.Eventually(t, func() bool {
		return sink.lastIs(v2alpha1.ConditionRateLimited, metav1.ConditionTrue, v2alpha1.ReasonSustainedRateLimit)
	}, 5*time.Second, 5*time.Millisecond,
		"sustained 429 polling must surface RateLimited=True/SustainedRateLimit")

	srv.SetRateLimitPolls(false)
	require.Eventually(t, func() bool {
		return sink.lastIs(v2alpha1.ConditionRateLimited, metav1.ConditionFalse, v2alpha1.ReasonPollingHealthy)
	}, 5*time.Second, 5*time.Millisecond,
		"a healthy poll after the episode must clear the condition")
}

// TestListener_UnauthorizedRefreshSurfacesDegraded drives the poll-401 →
// refresh-401 path (credentials revoked after the session opened) and asserts
// Degraded=True/Unauthorized plus exactly one SessionUnauthorized event per
// episode — then restores the credentials and asserts recovery clears the
// condition.
func TestListener_UnauthorizedRefreshSurfacesDegraded(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	sink := &statusSink{}
	_, ssID := startListenerWithSink(t, srv, sink)

	// Revoke: the cached queue token 401s the poll, and the refresh that should
	// recover it is itself rejected.
	srv.FailSessionRefresh(true)
	srv.ExpireQueueToken(ssID)

	require.Eventually(t, func() bool {
		return sink.lastIs(v2alpha1.ConditionDegraded, metav1.ConditionTrue, v2alpha1.ReasonSessionUnauthorized)
	}, 5*time.Second, 5*time.Millisecond,
		"an unauthorized session refresh must surface Degraded=True/Unauthorized")
	require.Eventually(t, func() bool { return sink.eventCount("SessionUnauthorized") == 1 },
		5*time.Second, 5*time.Millisecond, "the transition must record a SessionUnauthorized event")

	// The loop keeps retrying the refresh while revoked; the episode must not spam
	// further events.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1, sink.eventCount("SessionUnauthorized"),
		"one SessionUnauthorized event per episode, not one per retry")

	// Restore the credentials: the next refresh succeeds and clears the condition.
	srv.FailSessionRefresh(false)
	require.Eventually(t, func() bool {
		return sink.lastIs(v2alpha1.ConditionDegraded, metav1.ConditionFalse, v2alpha1.ReasonSessionAuthorized)
	}, 5*time.Second, 5*time.Millisecond,
		"a successful refresh after the episode must clear Degraded")
}

// TestListener_UnauthorizedSessionRecreateSurfacesDegraded drives the poll-404 →
// re-create-401 path (session dropped server-side while the credentials are
// revoked) and asserts Degraded surfaces, then clears once re-creation succeeds.
func TestListener_UnauthorizedSessionRecreateSurfacesDegraded(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	sink := &statusSink{}
	_, ssID := startListenerWithSink(t, srv, sink)

	srv.FailSessionCreate(true)
	srv.DropSession(ssID)

	require.Eventually(t, func() bool {
		return sink.lastIs(v2alpha1.ConditionDegraded, metav1.ConditionTrue, v2alpha1.ReasonSessionUnauthorized)
	}, 5*time.Second, 5*time.Millisecond,
		"an unauthorized session re-create must surface Degraded=True/Unauthorized")

	srv.FailSessionCreate(false)
	require.Eventually(t, func() bool {
		return sink.lastIs(v2alpha1.ConditionDegraded, metav1.ConditionFalse, v2alpha1.ReasonSessionAuthorized)
	}, 5*time.Second, 5*time.Millisecond,
		"a successful re-create after the episode must clear Degraded")
}

// TestListener_StartUnauthorizedSurfacesDegraded proves a Start that fails
// unauthorized (revoked credentials at session creation — the reconciler discards
// the instance and retries later) still surfaces the failure class before
// returning, rather than leaving only the generic start-failed state.
func TestListener_StartUnauthorizedSurfacesDegraded(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	srv.FailSessionCreate(true)

	sink := &statusSink{}
	l, err := scalesetlistener.New(scalesetlistener.Config{
		Client:       newClient(t, srv),
		ScaleSetName: "linux",
		OwnerName:    "acme/linux",
		Provision:    func(context.Context, scalesetlistener.Job) error { return nil },
		Capacity:     fixedCapacity(1),
		Conditions:   sink,
		Events:       sink,
	})
	require.NoError(t, err)

	_, err = l.Start(context.Background())
	require.Error(t, err, "Start must fail when session creation is unauthorized")

	assert.True(t, sink.lastIs(v2alpha1.ConditionDegraded, metav1.ConditionTrue, v2alpha1.ReasonSessionUnauthorized),
		"the unauthorized start failure must surface Degraded=True/Unauthorized")
	assert.Equal(t, 1, sink.eventCount("SessionUnauthorized"),
		"the start failure must record a SessionUnauthorized event")
	_, baselinePushed := sink.last(v2alpha1.ConditionRateLimited)
	assert.False(t, baselinePushed, "no healthy baseline is published on a failed start")
}
