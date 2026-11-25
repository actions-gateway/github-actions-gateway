package rule

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
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
	// old is a container the kernel kills one process of.
	old = "old"
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
		pod:  {QoS: "burstable", Containers: map[string]cgroup.Events{runner: {}, sidecar: {}, old: {}}, Single: map[string]bool{old: true}},
		peer: {QoS: "burstable", Containers: map[string]cgroup.Events{runner: {}, old: {}}, Single: map[string]bool{old: true}},
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
	return func(n *node) { n.change(uid, func(p *cgroup.Pod) { p.Nested = true; oom(&p.Subtree) }) }
}

// whole is the count that follows a container's processes being killed together.
func whole(uid, name string) event { return func(n *node) { n.inContainer(uid, name, groupKill) } }

// goes is a container's cgroup removed.
func goes(uid, name string) event {
	return func(n *node) { n.change(uid, func(p *cgroup.Pod) { delete(p.Containers, name) }) }
}

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
		goes     bool
	}{
		"the container's limit":             {death(pod, runner, containerOOM(pod, runner)), false, ContainerLimit, false},
		"the container's, in a limited pod": {death(pod, runner, containerOOM(pod, runner)), true, ContainerLimit, false},
		"a one-container pod's cap":         {death(pod, runner, podOOM(pod)), false, ContainerLimit, false},
		"the pod's limit":                   {death(pod, runner, podOOM(pod)), true, PodLimit, false},
		"the limit all pods share":          {death(pod, runner, podsOOM), true, PodsLimit, false},
		"the QoS class's":                   {death(pod, runner, qosOOM), true, PodsLimit, false},
		"the node's memory":                 {death(pod, runner), true, AbovePods, false},
		"a limit met again as it dies":      {death(pod, runner, podOOM(pod), podOOM(pod)), true, PodLimit, false},
		"the container's, and it goes":      {append(death(pod, runner, containerOOM(pod, runner)), goes(pod, runner)), false, ContainerLimit, true},
		"the node's, and it goes":           {append(death(pod, runner), goes(pod, runner)), false, AbovePods, true},
	} {
		t.Run(name, func(t *testing.T) {
			everySampling(tc.events, func(mask, skew uint) {
				rises := observe(tc.events, every(100*time.Millisecond), mask, skew)[pod]
				require.Len(t, rises, 1, "mask %b skew %b", mask, skew)
				r := rises[0]
				assert.Equal(t, tc.want, Decide(r, tc.podLimit), "mask %b skew %b: %+v", mask, skew, r)
				if len(r.Containers) > 0 || !tc.goes {
					assert.Equal(t, []string{runner}, r.Containers, "mask %b skew %b", mask, skew)
				}
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
		"a nested kill just before this at the node's": {
			then(nested, death(pod, runner)), 100 * time.Millisecond, 0,
			map[string]Reason{runner: AbovePods}},
		"a nested kill a little before": {
			then(nested, []event{func(*node) {}}, death(pod, runner)), 130 * time.Millisecond, 0,
			map[string]Reason{runner: AbovePods}},
		"a nested kill, then this at the node's, and it goes": {
			then(nested, death(pod, runner), []event{goes(pod, runner)}), 100 * time.Millisecond, 0,
			map[string]Reason{runner: AbovePods}},
		"a container's limit met, then another's kill at the node's, and it goes": {
			then([]event{containerOOM(pod, sidecar)}, death(pod, runner), []event{goes(pod, runner)}), 100 * time.Millisecond, 0,
			map[string]Reason{runner: AbovePods}},
		"two at their own limits, and one goes": {
			then(death(pod, runner, containerOOM(pod, runner)), []event{goes(pod, runner)}, death(pod, sidecar, containerOOM(pod, sidecar))), time.Millisecond, 0,
			map[string]Reason{runner: ContainerLimit, sidecar: ContainerLimit}},
		"two nested kills before": {
			then(nested, nested, death(pod, runner)), 100 * time.Millisecond, 0,
			map[string]Reason{runner: AbovePods}},
		"a nested allocation waiting for its kill": {
			then(nested[:1], death(pod, runner)), 100 * time.Millisecond, 0,
			map[string]Reason{runner: AbovePods}},
		"one that has waited seconds": {
			then(nested[:1], []event{func(*node) {}}, death(pod, runner)), 1500 * time.Millisecond, 0,
			map[string]Reason{runner: AbovePods}},
		"a nested kill, then this at the pod's": {
			then(nested, death(pod, runner, podOOM(pod))), 100 * time.Millisecond, 0,
			map[string]Reason{runner: PodLimit}},
		"one container's limit met twice, the other at the node's": {
			then(death(pod, runner, containerOOM(pod, runner), containerOOM(pod, runner)), death(pod, sidecar)), time.Millisecond, 0,
			map[string]Reason{runner: ContainerLimit, sidecar: AbovePods}},
		"a process of an old container's at the pod's, then this at the node's": {
			then([]event{podOOM(pod), killed(pod, old)}, death(pod, runner)), 100 * time.Millisecond, 0,
			map[string]Reason{runner: AbovePods}},
		"a neighbour's at the shared limit, then this at the node's, one process": {
			then([]event{podsOOM, killed(peer, old)}, death(pod, runner)), 100 * time.Millisecond, 0,
			map[string]Reason{runner: AbovePods}},
		"one container's at the pod's, met twice, the other's at the node's": {
			then(death(pod, runner, podOOM(pod), podOOM(pod)), death(pod, sidecar)), time.Millisecond, 0,
			map[string]Reason{runner: PodLimit, sidecar: AbovePods}},
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
					if len(r.Containers) == 0 {
						assert.Contains(t, slices.Collect(maps.Values(tc.truth)), got, "mask %b skew %b: %+v", mask, skew, r)
					}
					for _, c := range r.Containers {
						assert.Equal(t, tc.truth[c], got, "%s, mask %b skew %b: %+v", c, mask, skew, r)
					}
				}
			})
		})
	}
}

func TestDecide_ANestedKillHidesNoLimitAboveIt(t *testing.T) {
	for want, limit := range map[Reason]event{PodLimit: podOOM(pod), PodsLimit: podsOOM} {
		events := then([]event{nestedOOM(pod), nestedKill(pod)}, death(pod, runner, limit))
		r := observe(events, every(100*time.Millisecond), 1<<len(events)-1, 0)[pod]
		require.Len(t, r, 1)
		assert.Equal(t, want, Decide(r[0], true), "%+v", r[0])
	}
}

// This pod is killed for the node's memory, or at its container's limit, and a sample shows
// an allocation at another limit that is no part of it.
func TestDecide_CannotTellAnotherLimitsAllocation(t *testing.T) {
	gone := []event{func(n *node) { delete(n.pods, peer) }}
	wait := []event{func(*node) {}}
	unseen := []event{func(n *node) { n.change(pod, func(p *cgroup.Pod) { oom(&p.Subtree); kill(&p.Subtree) }) }}
	for name, tc := range map[string]struct {
		events []event
		gap    time.Duration
		mask   uint
		want   Reason
	}{
		"a neighbour's before its kill":      {[]event{podsOOM, killed(pod, runner), whole(pod, runner), killed(peer, runner), whole(peer, runner)}, 0, 0b11111, PodsLimit},
		"a neighbour's two for its one kill": {then(death(peer, runner, podsOOM, podsOOM), death(pod, runner)), 0, 0b111111, PodsLimit},
		"of a neighbour gone unread":         {then(death(peer, runner, podsOOM), gone, death(pod, runner)), 0, 0b110001, PodsLimit},
		"the pod's, after another kill there": {
			then(death(pod, sidecar, podOOM(pod)), wait, []event{podOOM(pod)}, death(pod, runner)), 0, 0b1111111, PodLimit},
		"its own, waiting": {then([]event{containerOOM(pod, runner)}, death(pod, runner)), 0, 0b111, ContainerLimit},
		"of a cgroup in the pod never sampled, and it goes": {
			then(unseen, death(pod, runner), []event{goes(pod, runner)}), 0, 0b0001, ContainerLimit},
		"the shared limit's, its own given up on": {
			then([]event{containerOOM(pod, runner)}, wait, wait, death(pod, runner, podsOOM)), 2500 * time.Millisecond, 0b111111, PodsLimit},
	} {
		t.Run(name, func(t *testing.T) {
			r := observe(tc.events, every(cmp.Or(tc.gap, 100*time.Millisecond)), tc.mask, 0)[pod]
			require.NotEmpty(t, r)
			assert.Equal(t, tc.want, Decide(r[len(r)-1], true), "%+v", r)
		})
	}
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
		"of one process":                        {GroupKills: 1, Singles: 1, Pod: 2, Confirmed: true, Read: true},
		"of two limits":                         {GroupKills: 2, Inside: 1, Own: 1, Read: true, Pod: 1, Confirmed: true},
		"of more kills than allocations inside": {GroupKills: 2, Inside: 1, Own: 1, Read: true, Confirmed: true},
		"of a container not read":               {GroupKills: 1, Inside: 1, Confirmed: true},
		"of two not read":                       {GroupKills: 2, Inside: 2, Confirmed: true},
		"of one read, beside another's":         {GroupKills: 1, Inside: 1, Read: true, Confirmed: true, Recent: true},
		"of one not read, and the pod's":        {GroupKills: 1, Inside: 1, Pod: 1, Confirmed: true},
		"of a container's, and all pods'":       {GroupKills: 1, Inside: 1, Own: 1, Read: true, Pods: 1, Confirmed: true},
		"of one read too soon":                  {GroupKills: 1, Inside: 1, Pod: 1, Read: true},
		"of two kills at one limit":             {GroupKills: 2, Pod: 3, Confirmed: true, Read: true},
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

// One run of the local kills recorded on kernel 6.12.76 (Q1316): a sampler's readings of the
// cgroups each kill moved, in cgroups named as a kubelet's, and the kernel's line for it,
// which names the limit. The counts run on from one kill to the next.
func TestDecide_AgreesWithTheKernelOnTheRecordedKills(t *testing.T) {
	const qos = "kubepods.slice/kubepods-burstable.slice"
	m := fstest.MapFS{"cgroup.controllers": {}}
	appear(m, "kubepods.slice", qos, "kubepods.slice/kubepods-besteffort.slice")
	for i := 1; i <= 3; i++ {
		podDir := fmt.Sprintf("%s/kubepods-burstable-pod%s.slice", qos, uid(i))
		appear(m, podDir, podDir+"/cri-containerd-"+id(i)+".scope")
	}
	want, got := replay(t, m, "local-6.12.76.log", strings.NewReplacer(
		"podown", "pod"+uid(1), "podlim", "pod"+uid(2), "podtop", "pod"+uid(3),
		"containerd-own", "containerd-"+id(1), "containerd-lim", "containerd-"+id(2), "containerd-top", "containerd-"+id(3)),
		func(line string) bool { return strings.Contains(line, uid(2)) })
	assert.Equal(t, []Reason{ContainerLimit, PodLimit, PodsLimit}, want)
	assert.Equal(t, want, got)
}

// The kills on a kind cluster's two workers on the same kernel (kubelet v1.35.5, containerd
// 2.3.1), read by an agent as the kubelet's own pods were killed. A P line names the pod that
// follows. The agent did not read cgroup.stat, so the containers here that made a cgroup
// below themselves are those the kernel's line shows one for.
func TestDecide_AgreesWithTheKernelOnTheKindKills(t *testing.T) {
	for worker, tc := range map[string]struct{ kernel, got []Reason }{
		"worker": {
			kernel: []Reason{
				ContainerLimit, ContainerLimit, // the cap of a pod with one container
				ContainerLimit, ContainerLimit, ContainerLimit, // one of two containers
				PodLimit, PodLimit,
				AbovePods,
				nestedLimit, AbovePods,
				ContainerLimit, nestedLimit,
			},
			got: []Reason{
				ContainerLimit, ContainerLimit,
				ContainerLimit, ContainerLimit, ContainerLimit,
				PodLimit, PodLimit,
				AbovePods,
				Unknown, // 1.4 s after the nested one
				ContainerLimit,
			},
		},
		"worker2": {
			kernel: []Reason{PodsLimit, PodsLimit, nestedLimit, PodsLimit},
			got:    []Reason{PodsLimit, PodsLimit, PodsLimit},
		},
	} {
		t.Run(worker, func(t *testing.T) {
			m := fstest.MapFS{"cgroup.controllers": {}}
			const root = "kubelet.slice/kubelet-kubepods.slice"
			appear(m, root, root+"/kubelet-kubepods-burstable.slice", root+"/kubelet-kubepods-besteffort.slice")
			limited := ""
			kernel, got := replay(t, m, "kind-6.12.76-"+worker+".log", strings.NewReplacer(), func(line string) bool {
				if f := strings.Fields(line); f[0] == "P" {
					limited = f[2]
				}
				return strings.HasPrefix(limited, "pod-level-")
			})
			assert.Equal(t, tc.kernel, kernel)
			assert.Equal(t, tc.got, got)
		})
	}
}

// nestedLimit is of a cgroup below a container.
const nestedLimit Reason = "nested"

// replay gives a tracker the readings recorded in a file of testdata, with those a sampler
// makes in between and again after each kill, and returns the limits its kernel's lines name
// and the reasons of the rises. podLimit is asked of every P and K line.
func replay(t *testing.T, m fstest.MapFS, name string, names *strings.Replacer, podLimit func(line string) bool) (kernel, got []Reason) {
	recorded, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	tree, err := cgroup.Open(m, "")
	require.NoError(t, err)
	var tr cgroup.Tracker
	var at time.Time
	observe := func() map[string]cgroup.Rise {
		s, err := tree.Sample()
		require.NoError(t, err)
		rises, _ := tr.Observe(s, at)
		return rises
	}
	var pending []reading
	write := func(half bool, which func(reading) bool) {
		for _, r := range pending {
			if which(r) {
				r.write(m, half)
			}
		}
		require.Empty(t, observe())
	}
	all := func(reading) bool { return true }
	for _, line := range strings.Split(strings.TrimSpace(names.Replace(string(recorded))), "\n") {
		f := strings.Fields(line)
		if f[0] == "N" {
			r := readingOf(t, f)
			// What came and went before a kill is of other samples than its counts.
			if r.rose() && !slices.ContainsFunc(pending, reading.rose) {
				write(false, all)
				pending = nil
				for at = cmp.Or(at, r.at.Add(-time.Second)); at.Before(r.at); at = at.Add(250 * time.Millisecond) {
					require.Empty(t, observe())
				}
			}
			pending = append(pending, r)
			continue
		}
		limited := podLimit(line)
		if f[0] != "K" {
			continue
		}
		reason := kernelsReason(line, limited)
		kernel = append(kernel, reason)
		if reason == nestedLimit {
			nest(m, line)
		}
		// The kernel counts the allocation first, and the local sampler caught the first kill
		// there. A container that was read with its kill had gone by the next sample.
		read := func(r reading) bool {
			return slices.ContainsFunc(pending, func(o reading) bool { return o.rose() && o.dir == r.dir })
		}
		write(true, reading.rose)
		write(false, func(r reading) bool { return r.rose() || !read(r) })
		for _, r := range pending {
			if !r.rose() {
				r.write(m, false)
			}
		}
		pending = nil
		rises := observe()
		require.LessOrEqual(t, len(rises), 1)
		for _, r := range rises {
			require.LessOrEqual(t, len(r.Containers), 1)
			got = append(got, Decide(r, limited))
		}
	}
	return kernel, got
}

// reading is an N line.
type reading struct {
	at                     time.Time
	kind, dir, counts, own string
}

func readingOf(t *testing.T, f []string) reading {
	at, err := time.ParseDuration(f[1] + "s")
	require.NoError(t, err)
	counts, own, _ := strings.Cut(strings.Join(f[5:], " "), " | local ")
	return reading{time.Unix(0, 0).Add(at), f[3], f[4], counts, own}
}

func (r reading) rose() bool { return strings.HasPrefix(r.kind, "r") }

// write makes the file system as r read it, or as it was when the allocation alone had been
// counted.
func (r reading) write(m fstest.MapFS, half bool) {
	switch {
	case r.rose():
		for name, counts := range map[string]string{"memory.events": r.counts, local: r.own} {
			p := path.Join(r.dir, name)
			if half {
				counts = halfCounted(string(m[p].Data), counts)
			}
			m[p] = file(counts)
		}
	case r.kind == "appeared":
		appear(m, r.dir)
	default:
		maps.DeleteFunc(m, func(p string, _ *fstest.MapFile) bool { return strings.HasPrefix(p, r.dir+"/") })
	}
}

// appear makes cgroups in which nothing has been counted.
func appear(m fstest.MapFS, dirs ...string) {
	for _, dir := range dirs {
		m[dir+"/memory.events"], m[dir+"/"+local] = file("oom 0 oom_kill 0 oom_group_kill 0"), file("oom 0 oom_kill 0 oom_group_kill 0")
		m[dir+"/memory.oom.group"] = &fstest.MapFile{Data: []byte("1\n")}
		m[dir+"/cgroup.stat"] = &fstest.MapFile{Data: []byte("nr_descendants 0\nnr_dying_descendants 0\n")}
	}
}

// nest makes a cgroup below the container of the kernel's line.
func nest(m fstest.MapFS, line string) {
	_, after, _ := strings.Cut(line, "/kubelet.slice/")
	dir, _, _ := strings.Cut(after, ".scope")
	m["kubelet.slice/"+dir+".scope/cgroup.stat"] = &fstest.MapFile{Data: []byte("nr_descendants 1\nnr_dying_descendants 0\n")}
}

func uid(n int) string { return fmt.Sprintf("1b4e28ba_2fa1_11d2_883f_%012d", n) }
func id(n int) string  { return fmt.Sprintf("%064d", n) }

// kernelsReason is the limit a kernel's line names. One on a pod's cgroup is the pod's only
// where its spec has one.
func kernelsReason(line string, podLimit bool) Reason {
	_, after, _ := strings.Cut(line, ",oom_memcg=")
	limit, _, _ := strings.Cut(after, ",")
	switch base := path.Base(limit); {
	case strings.Contains(limit, ".scope/"):
		return nestedLimit
	case strings.HasSuffix(base, ".scope"), strings.Contains(base, "-pod") && !podLimit:
		return ContainerLimit
	case strings.Contains(base, "-pod"):
		return PodLimit
	case strings.HasSuffix(base, "kubepods.slice"):
		return PodsLimit
	case base == "kubelet.slice":
		return AbovePods
	}
	return Unknown
}

// file is an events file of counts, which the sampler printed on one line.
func file(counts string) *fstest.MapFile {
	var b strings.Builder
	f := strings.Fields(counts)
	for i := 0; i < len(f); i += 2 {
		fmt.Fprintf(&b, "%s %s\n", f[i], f[i+1])
	}
	return &fstest.MapFile{Data: []byte(b.String())}
}

// halfCounted is now with the kills of was.
func halfCounted(was, now string) string {
	w, n := strings.Fields(was), strings.Fields(now)
	n[len(n)-3], n[len(n)-1] = w[len(w)-3], w[len(w)-1]
	return strings.Join(n, " ")
}

const local = "memory.events.local"
