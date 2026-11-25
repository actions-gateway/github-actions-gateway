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
		"before it is read": {[2]Pod{without, without}, Rise{Own: 1, Inferred: 1}},
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

func TestObserve_ReturnsAProcessKilledInAContainerThatIsNot(t *testing.T) {
	single := func(kills uint64) Pod {
		return Pod{
			Subtree:    Events{OOM: 1, OOMKill: kills},
			Local:      Events{OOM: 1},
			Containers: map[string]Events{runnerID: {OOMKill: kills}},
			Single:     map[string]bool{runnerID: true},
		}
	}
	gone := single(5)
	gone.Containers, gone.Single = nil, nil
	for name, last := range map[string]Pod{"read": single(5), "gone": gone} {
		t.Run(name, func(t *testing.T) {
			var tr Tracker
			s := one(single(3))
			s.Pods[guaranteedUID] = Pod{}
			rises, _ := tr.Observe(s, start)
			assert.True(t, rises[burstableUID].Late)

			s.Pods[burstableUID] = single(5)
			rises, again := tr.Observe(s, start)
			assert.Empty(t, rises)
			assert.True(t, again)

			s.Pods[burstableUID] = last
			rises, _ = tr.Observe(s, start)
			assert.Equal(t, map[string]Rise{burstableUID: {Confirmed: true, Singles: 2, Read: name == "read", Containers: []string{runnerID}}}, rises)

			assert.Len(t, tr.pods[burstableUID].base.pod.Single, len(last.Single))

			s.Pods[guaranteedUID] = dead()
			tr.Observe(s, start)
			rises, _ = tr.Observe(s, start)
			assert.Zero(t, rises[guaranteedUID].Peers)
		})
	}
}

func TestObserve_AKillOfAContainerThatHasGoneIsItsOwnLimits(t *testing.T) {
	once, twice := Events{OOM: 1, OOMKill: 1, OOMGroupKill: 1}, Events{OOM: 2, OOMKill: 2, OOMGroupKill: 2}
	with := func(e Events, containers map[string]Events) Pod { return Pod{Subtree: e, Containers: containers} }
	both := map[string]Events{runnerID: {}, sandboxID: {}}
	met := map[string]Events{runnerID: {}, sandboxID: {OOM: 1}}
	left := map[string]Events{sandboxID: {}}
	for name, tc := range map[string]struct {
		before, after Pod
		want          uint64
	}{
		"nothing else inside met one": {with(Events{}, both), with(once, left), 1},
		"another container did":       {with(Events{}, both), with(once, map[string]Events{sandboxID: {OOM: 1}}), 0},
		"another did before": {
			with(Events{OOM: 1}, met), with(Events{OOM: 2, OOMKill: 1, OOMGroupKill: 1}, map[string]Events{sandboxID: {OOM: 1}}), 1},
		"it was read, and none":   {with(Events{}, both), with(once, map[string]Events{sandboxID: {OOMGroupKill: 1}}), 0},
		"none has gone":           {with(Events{}, both), with(once, both), 0},
		"two have gone":           {with(Events{}, both), with(once, nil), 0},
		"two were killed":         {with(Events{}, both), with(twice, left), 0},
		"a limit inside it might": {with(Events{}, both), Pod{Nested: true, Subtree: once, Containers: left}, 0},
		"one might have":          {Pod{Nested: true, Containers: both}, with(once, left), 0},
	} {
		t.Run(name, func(t *testing.T) {
			var tr Tracker
			tr.Observe(one(tc.before), start)
			tr.Observe(one(with(tc.before.Subtree, tc.before.Containers)), start.Add(time.Minute))
			tr.Observe(one(tc.after), start.Add(2*time.Minute))
			rises, _ := tr.Observe(one(tc.after), start.Add(2*time.Minute))
			assert.Equal(t, tc.want, rises[burstableUID].Own)
		})
	}
}

func TestObserve_AContainerMadeAgainStartsFromNothing(t *testing.T) {
	many := Events{OOM: 9, OOMKill: 9, OOMGroupKill: 9}
	once := Events{OOM: 1, OOMKill: 1, OOMGroupKill: 1}
	pod := func(subtree, own Events, single bool) Pod {
		p := Pod{Subtree: subtree, Containers: map[string]Events{runnerID: own}}
		if single {
			p.Single = map[string]bool{runnerID: true}
		}
		return p
	}
	for name, single := range map[string]bool{"killed whole": false, "one process": true} {
		t.Run(name, func(t *testing.T) {
			var tr Tracker
			s := one(pod(many, many, single))
			s.Pods[guaranteedUID] = Pod{}
			tr.Observe(s, start)
			s.Pods[burstableUID] = pod(Events{10, 10, 10}, once, single)
			s.Pods[guaranteedUID] = Pod{Subtree: once}
			tr.Observe(s, start)
			rises, _ := tr.Observe(s, start)
			want := Rise{Confirmed: true, GroupKills: 1, Inside: 1, Own: 1, Read: true, Containers: []string{runnerID}, Peers: 1}
			if single {
				want.Singles = 1
			}
			assert.Equal(t, want, rises[burstableUID])
			peers := uint64(1)
			if single {
				peers = 2
			}
			assert.Equal(t, peers, rises[guaranteedUID].Peers)
		})
	}
}

func TestObserve_KeepsTheCountsOfWhatItMayNotReadForAWhile(t *testing.T) {
	dead := Events{OOM: 3, OOMKill: 3}
	open := Pod{Subtree: dead, Containers: map[string]Events{runnerID: dead}, Single: map[string]bool{runnerID: true}}
	shut := Pod{Subtree: dead, Refused: map[string]bool{runnerID: true}, Nested: true}
	for name, tc := range map[string]struct {
		closed func(s *Sample)
	}{
		"a container": {func(s *Sample) { s.Pods[burstableUID] = shut }},
		"a pod": {func(s *Sample) {
			delete(s.Pods, burstableUID)
			s.Refused = map[string]bool{burstableUID: true}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			var tr Tracker
			at := start
			observe := func(closed bool) map[string]Rise {
				s := one(open)
				s.Pods[guaranteedUID] = Pod{}
				if closed {
					tc.closed(&s)
				}
				at = at.Add(time.Minute)
				rises, again := tr.Observe(s, at)
				assert.False(t, again)
				return rises
			}
			observe(false)
			for range 3 {
				assert.Empty(t, observe(true))
				assert.Empty(t, observe(false))
			}
			s := one(open)
			s.Pods[guaranteedUID] = Pod{Subtree: Events{OOMKill: 1, OOMGroupKill: 1}}
			tr.Observe(s, at)
			rises, _ := tr.Observe(s, at)
			assert.Zero(t, rises[guaranteedUID].Peers)
		})
	}
}

func TestObserve_AKillOfAContainerItMayNotReadIsNotNamedItsOwn(t *testing.T) {
	var tr Tracker
	once := Events{OOM: 1, OOMKill: 1, OOMGroupKill: 1}
	tr.Observe(one(Pod{Containers: map[string]Events{runnerID: {}}}), start)
	tr.Observe(one(Pod{Subtree: once, Refused: map[string]bool{runnerID: true}, Nested: true}), start)
	rises, _ := tr.Observe(one(Pod{Subtree: once, Refused: map[string]bool{runnerID: true}, Nested: true}), start)
	want := Rise{Confirmed: true, GroupKills: 1, Inside: 1}
	assert.Equal(t, want, rises[burstableUID])

	tr.Observe(one(Pod{Subtree: once, Refused: map[string]bool{runnerID: true}, Nested: true}), start.Add(time.Second))
	after := Pod{Subtree: Events{OOM: 1, OOMKill: 2, OOMGroupKill: 2}}
	tr.Observe(one(after), start.Add(2*time.Second))
	rises, _ = tr.Observe(one(after), start.Add(2*time.Second))
	assert.True(t, rises[burstableUID].RecentOwn)
}

func TestObserve_AProcessKilledElsewhereIsAPeers(t *testing.T) {
	var tr Tracker
	s := one(Pod{})
	s.Pods[guaranteedUID] = Pod{Containers: map[string]Events{runnerID: {}}, Single: map[string]bool{runnerID: true}}
	tr.Observe(s, start)
	s.Pods[burstableUID] = dead()
	s.Pods[guaranteedUID] = Pod{Containers: map[string]Events{runnerID: {OOMKill: 2}}, Single: map[string]bool{runnerID: true}}
	tr.Observe(s, start)
	rises, _ := tr.Observe(s, start)
	assert.Equal(t, uint64(2), rises[burstableUID].Peers)
}

func TestObserve_AKillBelowAContainerIsNoContainers(t *testing.T) {
	var tr Tracker
	tr.Observe(one(Pod{}), start)
	nested := Pod{Nested: true, Subtree: Events{OOM: 1, OOMKill: 1, OOMGroupKill: 1}, Containers: map[string]Events{runnerID: {}}}
	tr.Observe(one(nested), start)
	rises, _ := tr.Observe(one(nested), start)
	assert.Equal(t, map[string]Rise{burstableUID: {Confirmed: true, GroupKills: 1, Inside: 1}}, rises)
}

func TestObserve_APodMadeAgainStartsFromNothing(t *testing.T) {
	for name, was := range map[string]Pod{
		"every count":    {Subtree: Events{OOM: 3, OOMKill: 3, OOMGroupKill: 3}, Local: Events{OOM: 3}},
		"oom":            {Subtree: Events{OOM: 2}},
		"oom_kill":       {Subtree: Events{OOMKill: 2}},
		"oom_group_kill": {Subtree: Events{OOMGroupKill: 2}},
		"its own oom":    {Local: Events{OOM: 2}},
	} {
		t.Run(name, func(t *testing.T) {
			var tr Tracker
			tr.Observe(one(was), start)
			rises, again := tr.Observe(one(Pod{}), start)
			assert.Empty(t, rises)
			assert.False(t, again)
			tr.Observe(one(dead()), start)
			rises, _ = tr.Observe(one(dead()), start)
			assert.Equal(t, Rise{Confirmed: true, GroupKills: 1, Pod: 1, Read: true, Containers: []string{runnerID}}, rises[burstableUID])
		})
	}
}

func TestObserve_APodMadeAgainHasLeftNothingOut(t *testing.T) {
	met := Pod{Subtree: Events{OOM: 1}, Local: Events{OOM: 1}}
	var tr Tracker
	tr.Observe(one(Pod{Subtree: Events{OOMKill: 2}}), start)
	for i := range time.Duration(3) {
		tr.Observe(one(met), start.Add(i*groupLag))
	}
	tr.Observe(one(dead()), start.Add(3*groupLag))
	rises, _ := tr.Observe(one(dead()), start.Add(3*groupLag))
	require.Contains(t, rises, burstableUID)
	assert.False(t, rises[burstableUID].Recent)
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
	none := map[string]Events{runnerID: {}}
	for name, tc := range map[string]struct {
		met         Pod
		above       uint64
		recent, own bool
	}{
		"at the pod's limit":          {Pod{Subtree: Events{OOM: 1}, Local: Events{OOM: 1}, Containers: none}, 0, true, true},
		"at the container's":          {Pod{Subtree: Events{OOM: 1}, Containers: map[string]Events{runnerID: {OOM: 1}}}, 0, true, true},
		"inside, a container gone":    {Pod{Subtree: Events{OOM: 1}}, 0, true, true},
		"inside the container":        {Pod{Subtree: Events{OOM: 1}, Containers: none}, 0, true, false},
		"above the pod":               {Pod{Containers: none}, 1, true, false},
		"above it, a container gone":  {Pod{}, 1, true, false},
		"none, and no container gone": {Pod{Containers: none}, 0, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			sample := func(p Pod, above uint64) Sample {
				s := one(p)
				s.Root.OOM = above
				return s
			}
			var tr Tracker
			tr.Observe(one(Pod{Containers: none}), start)
			tr.Observe(sample(tc.met, tc.above), start)
			tr.Observe(sample(tc.met, tc.above), start.Add(settle))

			after := tc.met
			after.Subtree.OOMGroupKill = 1
			for at, recently := range map[time.Duration]bool{settle + time.Second: true, settle + recent: false} {
				again := tr
				again.pods = map[string]*tracked{burstableUID: new(*tr.pods[burstableUID])}
				again.Observe(sample(after, tc.above), start.Add(at))
				rises, _ := again.Observe(sample(after, tc.above), start.Add(at))
				want := Rise{Confirmed: true, Recent: recently && tc.recent, RecentOwn: recently && tc.own, GroupKills: 1}
				assert.Equal(t, want, rises[burstableUID])
			}
		})
	}
}

func TestObserve_ClaimsNothingOfWhatWasDoneWhileItMightNotReadAPod(t *testing.T) {
	var tr Tracker
	shut := Sample{Refused: map[string]bool{burstableUID: true}}
	tr.Observe(one(Pod{}), start)
	tr.Observe(shut, start.Add(time.Second))
	opened := one(dead())
	opened.Root.OOM, opened.Pods[guaranteedUID] = 2, dead()
	rises, again := tr.Observe(opened, start.Add(2*time.Second))
	assert.False(t, again)
	want := Rise{Late: true, GroupKills: 1, Pod: 1, Read: true, Containers: []string{runnerID}}
	assert.Equal(t, want, rises[burstableUID])

	rises, _ = tr.Observe(opened, start.Add(3*time.Second))
	assert.NotContains(t, rises, burstableUID)
}

func TestObserve_ClaimsNothingOfAProcessKilledWhileItMightNotReadAPod(t *testing.T) {
	var tr Tracker
	single := func(e Events) Sample {
		return one(Pod{Subtree: e, Containers: map[string]Events{runnerID: e}, Single: map[string]bool{runnerID: true}})
	}
	tr.Observe(single(Events{}), start)
	tr.Observe(Sample{Refused: map[string]bool{burstableUID: true}}, start.Add(time.Second))
	rises, _ := tr.Observe(single(Events{OOM: 1, OOMKill: 1}), start.Add(2*time.Second))
	assert.True(t, rises[burstableUID].Late)
	assert.Equal(t, uint64(1), rises[burstableUID].Singles)
}

// A container's counts rise before its pod's, and one sample may be between the two.
func TestObserve_WaitsForThePodToCountWhatAContainerOfItHas(t *testing.T) {
	of := func(pod, container Events) Sample {
		return one(Pod{Subtree: pod, Containers: map[string]Events{runnerID: container}})
	}
	met, killed := Events{OOM: 1}, Events{OOM: 1, OOMKill: 1, OOMGroupKill: 1}
	for name, between := range map[string][]Sample{
		"its allocation":                {of(Events{}, met), of(met, met)},
		"its allocation, and then both": {of(Events{}, met)},
		"its kill":                      {of(met, killed)},
		"both":                          {of(Events{}, killed)},
		"each":                          {of(Events{}, met), of(met, met), of(met, killed)},
	} {
		t.Run(name, func(t *testing.T) {
			var tr Tracker
			tr.Observe(of(Events{}, Events{}), start)
			at := start
			for _, s := range append(between, of(killed, killed)) {
				at = at.Add(time.Second)
				rises, _ := tr.Observe(s, at)
				assert.Empty(t, rises)
			}
			rises, _ := tr.Observe(of(killed, killed), at)
			r := rises[burstableUID]
			assert.Equal(t, []string{runnerID}, r.Containers)
			assert.Equal(t, uint64(1), r.Own)
			assert.Equal(t, uint64(1), r.Inside)
			assert.True(t, r.Read)
		})
	}
}

func TestObserve_WaitsForThePodToCountAKillAboveItThatAContainerOfItHas(t *testing.T) {
	of := func(pod, container Events) Sample {
		return one(Pod{Subtree: pod, Containers: map[string]Events{runnerID: container}})
	}
	killed := Events{OOMKill: 1, OOMGroupKill: 1}
	var tr Tracker
	tr.Observe(of(Events{}, Events{}), start)
	tr.Observe(of(Events{}, killed), start.Add(time.Second))
	tr.Observe(of(killed, killed), start.Add(2*time.Second))
	rises, _ := tr.Observe(of(killed, killed), start.Add(2*time.Second))
	assert.Equal(t, []string{runnerID}, rises[burstableUID].Containers)
	assert.True(t, rises[burstableUID].Read)
}

// A kill of none of its processes follows an allocation that met no limit of it.
func TestObserve_WaitsForAnAllocationMetAfterOneAtThePodsLimit(t *testing.T) {
	of := func(root, peer uint64, e Events) Sample {
		return Sample{Root: Events{OOM: root}, Pods: map[string]Pod{
			burstableUID:  {Subtree: e, Containers: map[string]Events{runnerID: e}},
			guaranteedUID: {Subtree: Events{OOMKill: peer, OOMGroupKill: peer}},
		}}
	}
	met, killed := Events{OOM: 1}, Events{OOM: 1, OOMKill: 1, OOMGroupKill: 1}
	var tr Tracker
	for i, s := range []Sample{of(0, 0, Events{}), of(1, 0, Events{}), of(1, 1, met), of(1, 1, killed)} {
		tr.Observe(s, start.Add(time.Duration(i)*time.Second))
	}
	rises, _ := tr.Observe(of(1, 1, killed), start.Add(3*time.Second))
	assert.Equal(t, uint64(1), rises[burstableUID].Own)
}

// It is not waited for longer than any other.
func TestObserve_WaitsNoLongerForAPodThatCountsLessThanItsContainer(t *testing.T) {
	var tr Tracker
	of := func(pod, container Events) Sample {
		return one(Pod{Subtree: pod, Containers: map[string]Events{runnerID: container}})
	}
	tr.Observe(of(Events{}, Events{}), start)
	tr.Observe(of(Events{}, Events{1, 1, 1}), start.Add(time.Second))
	assert.Zero(t, tr.pods[burstableUID].base.pod.Containers[runnerID])
	tr.Observe(of(Events{}, Events{1, 1, 1}), start.Add(time.Second+settle))
	assert.Equal(t, Events{1, 1, 1}, tr.pods[burstableUID].base.pod.Containers[runnerID])
}

// A refused pod keeps its base, so the counts above it since then are a neighbour's.
func TestObserve_AKillAfterAPodIsOpenedIsCountedFromThen(t *testing.T) {
	var tr Tracker
	s := one(Pod{})
	s.Pods[guaranteedUID] = Pod{}
	tr.Observe(s, start)
	shut := one(Pod{})
	delete(shut.Pods, burstableUID)
	shut.Refused = map[string]bool{burstableUID: true}
	shut.Pods[guaranteedUID], shut.Root.OOM = dead(), 2
	tr.Observe(shut, start.Add(time.Second))
	opened := one(Pod{})
	opened.Pods[guaranteedUID], opened.Root.OOM = dead(), 2
	rises, _ := tr.Observe(opened, start.Add(2*time.Second))
	assert.NotContains(t, rises, burstableUID)

	killed := one(Pod{Subtree: Events{OOMKill: 1, OOMGroupKill: 1}})
	killed.Pods[guaranteedUID], killed.Root.OOM = dead(), 2
	tr.Observe(killed, start.Add(3*time.Second))
	rises, _ = tr.Observe(killed, start.Add(3*time.Second))
	r := rises[burstableUID]
	assert.Zero(t, r.Pods)
	assert.True(t, r.Recent)
}
