package agent

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/cgroup"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/report"
)

const (
	uid   = "1b4e28ba-2fa1-11d2-883f-0016d3cca427"
	other = "6fa459ea-ee8a-3ca4-894e-db77e160355e"
	poll  = 250 * time.Millisecond
)

var quietLog = slog.New(slog.DiscardHandler)

// node is a tree whose counts change at set times after its start.
type node struct {
	start   time.Time
	changes []change
	pods    map[string]cgroup.Pod
	samples []time.Duration
}

type change struct {
	at  time.Duration
	do  func(pods map[string]cgroup.Pod)
	err error
}

func (n *node) Sample() (cgroup.Sample, error) {
	if n.start.IsZero() {
		n.start, n.pods = time.Now(), map[string]cgroup.Pod{uid: {}, other: {}}
	}
	since := time.Since(n.start)
	n.samples = append(n.samples, since)
	s := cgroup.Sample{Pods: map[string]cgroup.Pod{}}
	for _, c := range n.changes {
		if since < c.at {
			break
		}
		if c.err != nil {
			return cgroup.Sample{}, c.err
		}
		c.do(n.pods)
	}
	for id, p := range n.pods {
		s.Pods[id] = p
	}
	return s, nil
}

// set has a pod's counts be e from at on.
func set(at time.Duration, pod string, e cgroup.Events) change {
	return change{at: at, do: func(pods map[string]cgroup.Pod) { pods[pod] = cgroup.Pod{Subtree: e, Local: cgroup.Events{OOM: e.OOM}} }}
}

// run has an agent of n run for d.
func run(t *testing.T, n *node, d time.Duration, wake func(chan<- string)) error {
	t.Helper()
	changed := make(chan string)
	a := &Agent{Tree: n, Poll: poll, Changed: changed, Log: quietLog, Write: func([]report.Kill) {}}
	ctx, cancel := context.WithTimeout(t.Context(), d)
	defer cancel()
	if wake != nil {
		go wake(changed)
	}
	return a.Run(ctx)
}

func TestRun_PollsWhereNothingChanges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &node{}
		err := run(t, n, time.Second-1, nil)
		require.NoError(t, err)
		assert.Equal(t, []time.Duration{0, poll, 2 * poll, 3 * poll}, n.samples)
	})
}

func TestRun_ReportsAKillAtTheNextSampleAfterTheOneThatShowsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &node{changes: []change{set(poll, uid, cgroup.Events{OOM: 1, OOMKill: 1, OOMGroupKill: 1})}}
		var got []report.Kill
		ctx, cancel := context.WithTimeout(t.Context(), 2*poll)
		defer cancel()
		began := time.Now()
		a := &Agent{Tree: n, Poll: poll, Log: quietLog, Write: func(k []report.Kill) { got = k }}
		require.NoError(t, a.Run(ctx))
		require.Len(t, got, 1)
		assert.Equal(t, uid, got[0].Pod)
		assert.Equal(t, began.Add(poll).UnixMilli(), got[0].At)
		assert.True(t, got[0].Rise.Confirmed)
		assert.Equal(t, uint64(1), got[0].Rise.Pod)
		assert.Equal(t, []time.Duration{0, poll, poll}, n.samples[:3])
	})
}

func TestRun_ReportsAProcessKilledWithNoContainer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &node{changes: []change{set(time.Second, uid, cgroup.Events{OOMKill: 2})}}
		var got []report.Kill
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
		defer cancel()
		a := &Agent{Tree: n, Poll: poll, Log: quietLog, Write: func(k []report.Kill) { got = append(got, k...) }}
		require.NoError(t, a.Run(ctx))
		require.Len(t, got, 1)
		assert.Equal(t, uint64(2), got[0].Processes)
		assert.Nil(t, got[0].Rise)
	})
}

// One process of a container that is not killed whole is in its rise, and not in a second kill.
func TestRun_ReportsAKillOnce(t *testing.T) {
	const id = "0000000000000000000000000000000000000000000000000000000000000001"
	single := func(at time.Duration, e cgroup.Events) change {
		return change{at: at, do: func(pods map[string]cgroup.Pod) {
			pods[uid] = cgroup.Pod{Subtree: e, Containers: map[string]cgroup.Events{id: e}, Single: map[string]bool{id: true}}
		}}
	}
	synctest.Test(t, func(t *testing.T) {
		n := &node{changes: []change{single(0, cgroup.Events{}), single(2*time.Second, cgroup.Events{OOM: 1, OOMKill: 1})}}
		var got []report.Kill
		ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
		defer cancel()
		a := &Agent{Tree: n, Poll: poll, Log: quietLog, Write: func(k []report.Kill) { got = k }}
		require.NoError(t, a.Run(ctx))
		require.Len(t, got, 1)
		assert.Equal(t, uint64(1), got[0].Rise.Singles)
	})
}

// bursts is how many samples came sooner than a poll after the one before.
func bursts(samples []time.Duration) (n int) {
	for i := 1; i < len(samples); i++ {
		if samples[i]-samples[i-1] < poll {
			n++
		}
	}
	return n
}

func TestRun_SamplesCloselyForAWhileAfterAChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &node{changes: []change{set(poll, uid, cgroup.Events{OOM: 1})}}
		err := run(t, n, 3*time.Second, nil)
		require.NoError(t, err)
		want := []time.Duration{0, poll}
		for at := poll + burstEvery; at <= poll+burstFor; at += burstEvery {
			want = append(want, at)
		}
		assert.Equal(t, want, n.samples[:len(want)])
		assert.Equal(t, len(want)-2, bursts(n.samples))
	})
}

func TestRun_APodHasOneBurstAMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var changes []change
		for i := range 260 {
			changes = append(changes, set(poll*time.Duration(i+1), uid, cgroup.Events{OOM: uint64(i + 1)}))
		}
		n := &node{changes: changes}
		err := run(t, n, burstGap-1, nil)
		require.NoError(t, err)
		assert.Equal(t, int(burstFor/burstEvery), bursts(n.samples))

		n = &node{changes: append(changes, set(2*burstGap, other, cgroup.Events{OOM: 1}))}
		err = run(t, n, 2*burstGap+time.Second, nil)
		require.NoError(t, err)
		assert.Equal(t, 3*int(burstFor/burstEvery), bursts(n.samples), "two of the one's, and another's")
	})
}

func TestRun_AKnownChangeIsSampledAtOnceAndOnceAMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &node{}
		err := run(t, n, time.Second, func(changed chan<- string) {
			time.Sleep(100 * time.Millisecond)
			changed <- uid
			time.Sleep(520 * time.Millisecond)
			changed <- uid
		})
		require.NoError(t, err)
		assert.Contains(t, n.samples, 100*time.Millisecond)
		assert.NotContains(t, n.samples, 620*time.Millisecond)
	})
}

func TestRun_TellsTheWatcherOfEachSample(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n, told := &node{}, 0
		a := &Agent{Tree: n, Poll: poll, Log: quietLog, Watch: func(s cgroup.Sample) {
			told++
			assert.Len(t, s.Pods, 2)
		}}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		require.NoError(t, a.Run(ctx))
		assert.Equal(t, len(n.samples), told)
	})
}

func TestRun_StopsWhereTheTreeCannotBeReadForLong(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		broken := errors.New("broken")
		n := &node{changes: []change{{at: time.Second, err: broken}}}
		err := run(t, n, time.Minute, nil)
		require.ErrorIs(t, err, broken)
		assert.Equal(t, time.Second+(maxFailures-1)*poll, n.samples[len(n.samples)-1])
	})
}

// flaky fails but for every tenth sample.
type flaky struct{ n int }

func (f *flaky) Sample() (cgroup.Sample, error) {
	if f.n++; f.n%10 == 0 {
		return cgroup.Sample{}, nil
	}
	return cgroup.Sample{}, errors.New("flaky")
}

func TestRun_GoesOnWhereItCanBeReadAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &flaky{}
		a := &Agent{Tree: f, Poll: poll, Log: quietLog}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		require.NoError(t, a.Run(ctx))
		assert.Greater(t, f.n, 5*maxFailures)
	})
}

func TestCompare_ForgetsThePodsThatAreGone(t *testing.T) {
	a := &Agent{}
	now := time.Unix(1_800_000_000, 0)
	a.compare(cgroup.Sample{Pods: map[string]cgroup.Pod{uid: {}}}, now)
	a.compare(cgroup.Sample{Pods: map[string]cgroup.Pod{uid: {Subtree: cgroup.Events{OOM: 1}}}}, now)
	require.Contains(t, a.bursts, uid)
	a.compare(cgroup.Sample{}, now.Add(burstGap-1))
	assert.Contains(t, a.bursts, uid, "a pod that goes and comes has no other")
	a.compare(cgroup.Sample{}, now.Add(burstGap))
	assert.Empty(t, a.bursts)
	assert.Empty(t, a.counts)
}
