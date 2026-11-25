package rule

import (
	"fmt"
	"maps"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/cgroup"
)

const (
	pod, peer       = "pod", "peer"
	runner, sidecar = "runner", "sidecar"
)

// node counts as memcg_memory_event does (Linux v6.12, read): on the cgroup itself in its
// local file, and on it and its ancestors in the subtree's.
type node struct {
	pods map[string]cgroup.Pod
	qos  cgroup.Events
	root cgroup.Events
}

func newNode() *node {
	return &node{pods: map[string]cgroup.Pod{
		pod:  {QoS: "burstable", Containers: map[string]cgroup.Events{runner: {}, sidecar: {}}},
		peer: {QoS: "burstable", Containers: map[string]cgroup.Events{runner: {}}},
	}}
}

func (n *node) change(uid string, f func(*cgroup.Pod)) {
	p := n.pods[uid]
	p.Containers = maps.Clone(p.Containers)
	f(&p)
	n.pods[uid] = p
}

func (n *node) inContainer(uid, name string, f func(*cgroup.Events)) {
	n.change(uid, func(p *cgroup.Pod) {
		e := p.Containers[name]
		f(&e)
		p.Containers[name] = e
		f(&p.Subtree)
	})
}

func oom(e *cgroup.Events)       { e.OOM++ }
func kill(e *cgroup.Events)      { e.OOMKill++ }
func groupKill(e *cgroup.Events) { e.OOMGroupKill++ }

type event func(*node)

func containerOOM(uid, name string) event { return func(n *node) { n.inContainer(uid, name, oom) } }
func podOOM(uid string) event {
	return func(n *node) { n.change(uid, func(p *cgroup.Pod) { oom(&p.Local); oom(&p.Subtree) }) }
}
func qosOOM(n *node)  { oom(&n.qos) }
func podsOOM(n *node) { oom(&n.root) }

func killed(uid, name string) event { return func(n *node) { n.inContainer(uid, name, kill) } }

// nestedKill is of a process below a container, whose own cgroup is left alone.
func nestedKill(uid string) event {
	return func(n *node) { n.change(uid, func(p *cgroup.Pod) { kill(&p.Subtree) }) }
}
func nestedOOM(uid string) event {
	return func(n *node) { n.change(uid, func(p *cgroup.Pod) { oom(&p.Subtree) }) }
}

// whole is the count that follows a container's processes being killed together.
func whole(uid, name string) event { return func(n *node) { n.inContainer(uid, name, groupKill) } }

// death is what the kernel counts for one container killed at one limit.
func death(uid, name string, limit ...event) []event {
	return append(limit, killed(uid, name), whole(uid, name))
}

func then(steps ...[]event) (all []event) {
	for _, s := range steps {
		all = append(all, s...)
	}
	return all
}

// observe samples the node before the first event and after the last, twice, and after
// each event mask has a bit for. Where skew has the bit too, the event comes between the
// sample's halves. gap is the time before each event. It returns the pods' rises.
func observe(events []event, gap func(i int) time.Duration, mask, skew uint) map[string][]cgroup.Rise {
	var tr cgroup.Tracker
	n, at := newNode(), time.Unix(1_800_000_000, 0)
	out := map[string][]cgroup.Rise{}
	sample := func(between func()) {
		s := cgroup.Sample{Pods: maps.Clone(n.pods)}
		between()
		s.Root, s.QoS = n.root, map[string]cgroup.Events{"burstable": n.qos, "besteffort": {}}
		rises, _ := tr.Observe(s, at)
		for uid, r := range rises {
			out[uid] = append(out[uid], r)
		}
	}
	nothing := func() {}
	sample(nothing)
	for i, e := range events {
		at = at.Add(gap(i))
		do := func() { e(n) }
		switch {
		case mask&(1<<i) == 0:
			do()
		case skew&(1<<i) != 0:
			sample(do)
		default:
			do()
			sample(nothing)
		}
	}
	at = at.Add(time.Millisecond)
	for range 3 {
		sample(nothing)
	}
	return out
}

func every(d time.Duration) func(int) time.Duration { return func(int) time.Duration { return d } }

func everySampling(events []event, f func(mask, skew uint)) {
	for mask := range uint(1) << len(events) {
		for skew := range mask + 1 {
			if skew&^mask == 0 {
				f(mask, skew)
			}
		}
	}
}

func TestDecide_OneKillWhereverTheSamplesFall(t *testing.T) {
	for name, tc := range map[string]struct {
		events   []event
		podLimit bool
		want     Reason
	}{
		"the container's limit":             {death(pod, runner, containerOOM(pod, runner)), false, ContainerLimit},
		"the container's, in a limited pod": {death(pod, runner, containerOOM(pod, runner)), true, ContainerLimit},
		"a one-container pod's cap":         {death(pod, runner, podOOM(pod)), false, ContainerLimit},
		"the pod's limit":                   {death(pod, runner, podOOM(pod)), true, PodLimit},
		"the limit all pods share":          {death(pod, runner, podsOOM), true, PodsLimit},
		"the QoS class's":                   {death(pod, runner, qosOOM), true, PodsLimit},
		"the node's memory":                 {death(pod, runner), true, AbovePods},
		"a limit met again as it dies":      {death(pod, runner, podOOM(pod), podOOM(pod)), true, PodLimit},
	} {
		t.Run(name, func(t *testing.T) {
			everySampling(tc.events, func(mask, skew uint) {
				rises := observe(tc.events, every(100*time.Millisecond), mask, skew)[pod]
				require.Len(t, rises, 1, "mask %b skew %b", mask, skew)
				r := rises[0]
				assert.Equal(t, tc.want, Decide(r, tc.podLimit), "mask %b skew %b: %+v", mask, skew, r)
				assert.Equal(t, []string{runner}, r.Containers, "mask %b skew %b", mask, skew)
			})
		})
	}
}

// The kernel writes its report of a kill between the allocation and the kill, which can
// take seconds on a slow console.
func TestDecide_AKillLongAfterTheAllocation(t *testing.T) {
	events := death(pod, runner, podOOM(pod))
	for delay, want := range map[time.Duration]Reason{4 * time.Second: PodLimit, 30 * time.Second: Unknown} {
		gap := func(i int) time.Duration {
			if i == 1 {
				return delay
			}
			return 100 * time.Millisecond
		}
		// The sample after the allocation is what starts the wait.
		r := observe(events, gap, 0b001, 0)[pod]
		require.Len(t, r, 1)
		assert.Equal(t, PodLimit, Decide(r[0], true), "no sample while it waits: %+v", r[0])

		waited := then(events[:1], []event{func(*node) {}}, events[1:])
		r = observe(waited, func(i int) time.Duration { return gap(min(i, 1)) / 2 }, 0b1111, 0)[pod]
		require.Len(t, r, 1)
		assert.Equal(t, want, Decide(r[0], true), "after %s: %+v", delay, r[0])
	}
}

func TestDecide_TwoLimitsAtOnceIsNeverTheWrongOne(t *testing.T) {
	nested := []event{nestedOOM(pod), nestedKill(pod)}
	for name, tc := range map[string]struct {
		events []event
		gap    time.Duration
		// must is the events a whole sample follows.
		must uint
		// truth is the limit each container of the pod was killed at.
		truth map[string]Reason
	}{
		"two containers, two limits": {
			then(death(pod, runner, podOOM(pod)), death(pod, sidecar, podsOOM)), time.Millisecond, 0,
			map[string]Reason{runner: PodLimit, sidecar: PodsLimit}},
		"one at the pod's, one at the node's": {
			then(death(pod, runner, podOOM(pod)), death(pod, sidecar)), time.Millisecond, 0,
			map[string]Reason{runner: PodLimit, sidecar: AbovePods}},
		"a neighbour's at the shared limit, then this at the node's": {
			then(death(peer, runner, podsOOM), death(pod, runner)), 100 * time.Millisecond, 0,
			map[string]Reason{runner: AbovePods}},
		"this at the node's, then a neighbour's at the shared limit": {
			then(death(pod, runner), death(peer, runner, podsOOM)), 100 * time.Millisecond, 0b10,
			map[string]Reason{runner: AbovePods}},
		"both at the shared limit, the reports overlapping": {
			[]event{podsOOM, podsOOM, killed(peer, runner), whole(peer, runner), killed(pod, runner), whole(pod, runner)},
			100 * time.Millisecond, 0, map[string]Reason{runner: PodsLimit}},
		"a nested kill, then this at the shared limit": {
			then(nested, death(pod, runner, podsOOM)), 100 * time.Millisecond, 0,
			map[string]Reason{runner: PodsLimit}},
		"a nested kill well before this at the node's": {
			then(nested, []event{func(*node) {}}, death(pod, runner)), 3 * time.Second, 0b110,
			map[string]Reason{runner: AbovePods}},
	} {
		t.Run(name, func(t *testing.T) {
			everySampling(tc.events, func(mask, skew uint) {
				if mask&^skew&tc.must != tc.must {
					return
				}
				rises := observe(tc.events, every(tc.gap), mask, skew)[pod]
				assert.NotEmpty(t, rises)
				for _, r := range rises {
					got := Decide(r, true)
					if got == Unknown {
						continue
					}
					for _, c := range r.Containers {
						assert.Equal(t, tc.truth[c], got, "%s, mask %b skew %b: %+v", c, mask, skew, r)
					}
				}
			})
		})
	}
}

// The neighbour's allocation is at the shared limit before this pod's kill is first seen,
// and its own kill after, so nothing shows that it was not this pod's.
func TestDecide_CannotTellANeighboursPendingAllocation(t *testing.T) {
	events := []event{podsOOM, killed(pod, runner), whole(pod, runner), killed(peer, runner), whole(peer, runner)}
	r := observe(events, every(100*time.Millisecond), 0b11111, 0)[pod]
	require.Len(t, r, 1)
	assert.Equal(t, PodsLimit, Decide(r[0], false))
}

func TestDecide_TwoReadsThatAgreeAreBothAnswered(t *testing.T) {
	both := death(pod, runner, podsOOM)
	both = append(both, death(peer, runner, podsOOM)...)
	out := observe(both, every(time.Millisecond), 0, 0)
	for _, uid := range []string{pod, peer} {
		require.Len(t, out[uid], 1)
		assert.Equal(t, PodsLimit, Decide(out[uid][0], false), uid)
	}
}

func TestDecide_ClaimsNothing(t *testing.T) {
	for name, r := range map[string]cgroup.Rise{
		"of a pod first seen after":             {Late: true, GroupKills: 1, Pod: 1, Confirmed: true},
		"of files not of one moment":            {Torn: true, GroupKills: 1, Pod: 1, Confirmed: true},
		"of two limits":                         {GroupKills: 2, Inside: 1, Pod: 1, Confirmed: true},
		"of more kills than allocations inside": {GroupKills: 2, Inside: 1, Confirmed: true},
		"of more kills than allocations":        {GroupKills: 2, Pod: 1, Confirmed: true},
		"of a pod gone before a second":         {GroupKills: 1, Pods: 1},
		"of one gone with nothing counted":      {GroupKills: 1},
		"soon after an unmatched one":           {GroupKills: 1, Confirmed: true, Recent: true},
		"where a neighbour shares the count":    {GroupKills: 1, Pods: 1, Peers: 1, Confirmed: true},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, Unknown, Decide(r, true))
		})
	}
}

// The recorded local kills, six at each limit, on kernel 6.12.76 (Q1316): which of the
// container's, the pod's, the QoS slice's and the pods' root's own oom rose. The files are
// made here in that kernel's layout from those figures.
func TestDecide_TheRecordedKills(t *testing.T) {
	const (
		root      = "kubepods.slice"
		qos       = root + "/kubepods-burstable.slice"
		podDir    = qos + "/kubepods-burstable-pod1b4e28ba_2fa1_11d2_883f_0016d3cca427.slice"
		container = podDir + "/cri-containerd-" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.scope"
	)
	dirs := []string{container, podDir, qos, root}
	for name, tc := range map[string]struct {
		local    [4]int
		podLimit bool
		want     Reason
	}{
		"the container's":   {[4]int{1, 0, 0, 0}, false, ContainerLimit},
		"the pod's":         {[4]int{0, 1, 0, 0}, true, PodLimit},
		"the pods' root's":  {[4]int{0, 0, 0, 1}, false, PodsLimit},
		"none, the node's":  {[4]int{0, 0, 0, 0}, false, AbovePods},
		"two at one moment": {[4]int{0, 1, 0, 1}, true, Unknown},
	} {
		t.Run(name, func(t *testing.T) {
			m := fstest.MapFS{
				"cgroup.controllers":                         {},
				root + "/kubepods-besteffort.slice/" + local: events(0, 0),
			}
			write := func(kills int, rose [4]int) {
				below := 0
				for i, dir := range dirs {
					below += rose[i]
					m[dir+"/memory.events"] = events(below, kills)
					own := 0
					if i == 0 {
						own = kills
					}
					m[dir+"/"+local] = events(rose[i], own)
				}
			}
			write(0, [4]int{})
			tree, err := cgroup.Open(m, "")
			require.NoError(t, err)
			var tr cgroup.Tracker
			observe := func() map[string]cgroup.Rise {
				s, err := tree.Sample()
				require.NoError(t, err)
				rises, _ := tr.Observe(s, time.Unix(1_800_000_000, 0))
				return rises
			}
			observe()
			write(1, tc.local)
			observe()
			rises := observe()
			require.Len(t, rises, 1)
			for _, r := range rises {
				assert.Equal(t, tc.want, Decide(r, tc.podLimit))
			}
		})
	}
}

const local = "memory.events.local"

func events(oom, kills int) *fstest.MapFile {
	return &fstest.MapFile{Data: fmt.Appendf(nil, "low 0\nhigh 0\nmax 0\noom %d\noom_kill %d\noom_group_kill %d\n", oom, kills, kills)}
}
