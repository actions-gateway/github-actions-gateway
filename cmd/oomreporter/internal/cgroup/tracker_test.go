package cgroup

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var start = time.Unix(1_800_000_000, 0)

func one(p Pod) Sample { return Sample{Pods: map[string]Pod{burstableUID: p}} }

// dead is a pod with one container killed whole at the pod's limit.
func dead() Pod {
	e := Events{OOM: 1, OOMKill: 1, OOMGroupKill: 1}
	return Pod{Subtree: e, Local: Events{OOM: 1}, Containers: map[string]Events{runnerID: {OOMGroupKill: 1}}}
}

func TestObserve_ReturnsAKillOnceOnTheSecondSampleThatShowsIt(t *testing.T) {
	var tr Tracker
	rises, again := tr.Observe(one(Pod{}), start)
	assert.Empty(t, rises)
	assert.False(t, again)

	rises, again = tr.Observe(one(dead()), start)
	assert.Empty(t, rises)
	assert.True(t, again)

	rises, again = tr.Observe(one(dead()), start)
	assert.Equal(t, map[string]Rise{burstableUID: {Confirmed: true, GroupKills: 1, Pod: 1, Read: true, Containers: []string{runnerID}}}, rises)
	assert.False(t, again)

	rises, again = tr.Observe(one(dead()), start)
	assert.Empty(t, rises)
	assert.False(t, again)
}

func TestObserve_AKillBeforeThePodWasSeenIsLate(t *testing.T) {
	for name, before := range map[string][]Sample{
		"the tracker started after": nil,
		"the pod started after":     {{}},
	} {
		t.Run(name, func(t *testing.T) {
			var tr Tracker
			for _, s := range before {
				tr.Observe(s, start)
			}
			s := one(dead())
			s.Root.OOM = 5
			want := Rise{Late: true, GroupKills: 1, Pod: 1, Read: true, Containers: []string{runnerID}}
			rises, _ := tr.Observe(s, start)
			assert.Equal(t, map[string]Rise{burstableUID: want}, rises)

			rises, again := tr.Observe(s, start)
			assert.Empty(t, rises)
			assert.False(t, again)
		})
	}
}

func TestObserve_ANewPodsKillIsAPeersToTheRest(t *testing.T) {
	var tr Tracker
	tr.Observe(one(Pod{}), start)
	s := one(dead())
	s.Pods[guaranteedUID] = dead()
	tr.Observe(s, start)
	rises, _ := tr.Observe(s, start)
	assert.Equal(t, uint64(1), rises[burstableUID].Peers)
}

func TestObserve_ReturnsWhatIsLeftOfAPodThatGoes(t *testing.T) {
	for name, next := range map[string]Sample{
		"removed":                     {},
		"made again by the same name": one(Pod{}),
	} {
		t.Run(name, func(t *testing.T) {
			var tr Tracker
			tr.Observe(one(Pod{}), start)
			tr.Observe(one(dead()), start)
			rises, _ := tr.Observe(next, start)
			assert.Equal(t, map[string]Rise{burstableUID: {GroupKills: 1, Pod: 1, Read: true, Containers: []string{runnerID}}}, rises)
		})
	}
}

func TestObserve_AContainerThatGoesIsCountedAsLastRead(t *testing.T) {
	own := Events{OOM: 2, OOMKill: 3, OOMGroupKill: 1}
	killed := Pod{Subtree: own, Containers: map[string]Events{runnerID: own}}
	without := Pod{Subtree: own}
	for name, tc := range map[string]struct {
		samples [2]Pod
		want    Rise
	}{
		"not at all":        {[2]Pod{killed, killed}, Rise{Own: 1, Read: true, Containers: []string{runnerID}}},
		"after its kill":    {[2]Pod{killed, without}, Rise{Own: 1, Containers: []string{runnerID}}},
		"before it is read": {[2]Pod{without, without}, Rise{}},
	} {
		t.Run(name, func(t *testing.T) {
			var tr Tracker
			tr.Observe(one(Pod{Containers: map[string]Events{runnerID: {}}}), start)
			tr.Observe(one(tc.samples[0]), start)
			rises, _ := tr.Observe(one(tc.samples[1]), start)
			tc.want.Confirmed, tc.want.GroupKills, tc.want.Inside = true, 1, 2
			assert.Equal(t, map[string]Rise{burstableUID: tc.want}, rises)
			assert.Len(t, tr.pods[burstableUID].base.pod.Containers, len(tc.samples[1].Containers))
		})
	}
}

func TestObserve_AKillBelowAContainerIsNoContainers(t *testing.T) {
	var tr Tracker
	tr.Observe(one(Pod{}), start)
	nested := Pod{Subtree: Events{OOM: 1, OOMKill: 1, OOMGroupKill: 1}, Containers: map[string]Events{runnerID: {}}}
	tr.Observe(one(nested), start)
	rises, _ := tr.Observe(one(nested), start)
	assert.Equal(t, map[string]Rise{burstableUID: {Confirmed: true, GroupKills: 1, Inside: 1}}, rises)
}

func TestObserve_APodMadeAgainStartsFromNothing(t *testing.T) {
	var tr Tracker
	tr.Observe(one(Pod{Subtree: Events{OOM: 3, OOMKill: 3, OOMGroupKill: 3}}), start)
	rises, again := tr.Observe(one(Pod{}), start)
	assert.Empty(t, rises)
	assert.False(t, again)
	tr.Observe(one(dead()), start)
	rises, _ = tr.Observe(one(dead()), start)
	assert.Equal(t, uint64(1), rises[burstableUID].GroupKills)
}

func TestObserve_WaitsOutATornSample(t *testing.T) {
	torn := dead()
	torn.Torn = true
	var tr Tracker
	tr.Observe(one(Pod{}), start)
	for range 2 {
		rises, again := tr.Observe(one(torn), start)
		assert.Empty(t, rises)
		assert.True(t, again)
	}
	rises, _ := tr.Observe(one(dead()), start)
	require.Contains(t, rises, burstableUID)
	assert.False(t, rises[burstableUID].Torn)
}

func TestObserve_GivesUpOnATornPod(t *testing.T) {
	torn := dead()
	torn.Torn = true
	var tr Tracker
	tr.Observe(one(Pod{}), start)
	for i := 1; ; i++ {
		rises, again := tr.Observe(one(torn), start)
		if !again {
			assert.Equal(t, maxTorn+1, i)
			assert.True(t, rises[burstableUID].Torn)
			return
		}
		require.Less(t, i, 10)
	}
}

func TestObserve_ATornSampleIsNoBaseWhileNothingWasKilled(t *testing.T) {
	var tr Tracker
	tr.Observe(one(Pod{}), start)
	tr.Observe(one(Pod{Torn: true}), start)
	tr.Observe(one(dead()), start)
	rises, _ := tr.Observe(one(dead()), start)
	assert.Equal(t, Rise{Confirmed: true, GroupKills: 1, Pod: 1, Read: true, Containers: []string{runnerID}}, rises[burstableUID])
}

func TestObserve_AFirstSampleThatIsTornLeavesTheKillTorn(t *testing.T) {
	var tr Tracker
	tr.Observe(one(Pod{Torn: true}), start)
	tr.Observe(one(dead()), start)
	rises, again := tr.Observe(one(dead()), start)
	assert.True(t, rises[burstableUID].Torn)
	assert.False(t, again)
}

func TestObserve_EachKillIsConfirmed(t *testing.T) {
	var tr Tracker
	tr.Observe(one(Pod{}), start)
	tr.Observe(one(dead()), start)
	tr.Observe(one(dead()), start)
	second := dead()
	second.Subtree, second.Local = Events{2, 2, 2}, Events{OOM: 2}
	rises, again := tr.Observe(one(second), start)
	assert.Empty(t, rises)
	assert.True(t, again)
	rises, _ = tr.Observe(one(second), start)
	assert.Equal(t, uint64(1), rises[burstableUID].GroupKills)
}

func TestObserve_AnAllocationMoreThanTheProcessesKilledIsWaitedFor(t *testing.T) {
	two := Pod{Subtree: Events{OOM: 2, OOMKill: 1}}
	var tr Tracker
	tr.Observe(one(Pod{}), start)
	tr.Observe(one(two), start)
	tr.Observe(one(two), start.Add(groupLag))
	two.Subtree = Events{OOM: 2, OOMKill: 2, OOMGroupKill: 1}
	tr.Observe(one(two), start.Add(groupLag))
	rises, _ := tr.Observe(one(two), start.Add(groupLag))
	assert.Equal(t, uint64(2), rises[burstableUID].Inside)
}

func TestObserve_EachAllocationIsWaitedForAnew(t *testing.T) {
	first, second := Pod{Subtree: Events{OOM: 1}}, Pod{Subtree: Events{OOM: 2}}
	var tr Tracker
	tr.Observe(one(Pod{}), start)
	tr.Observe(one(first), start)
	tr.Observe(one(first), start.Add(settle))
	tr.Observe(one(second), start.Add(3*settle))
	second.Subtree.OOMGroupKill = 1
	tr.Observe(one(second), start.Add(3*settle+time.Second))
	rises, _ := tr.Observe(one(second), start.Add(3*settle+time.Second))
	assert.Equal(t, uint64(1), rises[burstableUID].Inside)
}

func TestObserve_RemembersAnAllocationAboveThePodItGaveUp(t *testing.T) {
	s := one(Pod{})
	var tr Tracker
	tr.Observe(s, start)
	s.Root.OOM = 1
	tr.Observe(s, start)
	tr.Observe(s, start.Add(settle))
	s.Pods[burstableUID] = Pod{Subtree: Events{OOMGroupKill: 1}}
	tr.Observe(s, start.Add(settle))
	rises, _ := tr.Observe(s, start.Add(settle))
	assert.Equal(t, Rise{Confirmed: true, Recent: true, GroupKills: 1}, rises[burstableUID])
}

func TestObserve_ANeighboursSharedKillIsNotHeldAgainstALaterOne(t *testing.T) {
	s := one(Pod{})
	s.Pods[guaranteedUID] = Pod{}
	var tr Tracker
	tr.Observe(s, start)
	s.Root.OOM = 1
	s.Pods[guaranteedUID] = Pod{Subtree: Events{OOMKill: 1, OOMGroupKill: 1}}
	tr.Observe(s, start)
	tr.Observe(s, start)
	s.Pods[burstableUID] = dead()
	tr.Observe(s, start.Add(time.Second))
	rises, _ := tr.Observe(s, start.Add(time.Second))
	assert.Equal(t, uint64(0), rises[burstableUID].Pods)
}

func TestObserve_APodMadeAgainDoesNotReplaceWhatWasLeft(t *testing.T) {
	twice := dead()
	twice.Subtree = Events{2, 2, 2}
	var tr Tracker
	tr.Observe(one(Pod{}), start)
	tr.Observe(one(twice), start)
	rises, _ := tr.Observe(one(dead()), start)
	assert.Equal(t, uint64(2), rises[burstableUID].GroupKills)
	assert.False(t, rises[burstableUID].Late)
}

func TestObserve_GivesUpAnAllocationThatLedToNoKill(t *testing.T) {
	met := Pod{Subtree: Events{OOM: 1}, Local: Events{OOM: 1}}
	var tr Tracker
	tr.Observe(one(Pod{}), start)
	tr.Observe(one(met), start)
	tr.Observe(one(met), start.Add(settle))

	after := met
	after.Subtree.OOMGroupKill = 1
	for at, want := range map[time.Duration]bool{settle + time.Second: true, settle + recent: false} {
		again := tr
		again.pods = map[string]*tracked{burstableUID: new(*tr.pods[burstableUID])}
		again.Observe(one(after), start.Add(at))
		rises, _ := again.Observe(one(after), start.Add(at))
		assert.Equal(t, Rise{Confirmed: true, Recent: want, GroupKills: 1}, rises[burstableUID])
	}
}
