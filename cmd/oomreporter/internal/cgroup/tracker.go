package cgroup

import (
	"maps"
	"slices"
	"time"
)

const (
	// settle bounds how long after an allocation meets a limit its kill is waited for. The
	// kernel writes its report of a kill between the two.
	settle = 5 * time.Second
	// groupLag bounds how long after a process is killed its whole cgroup's kill is
	// counted, where it is part of one.
	groupLag = 500 * time.Millisecond
	// recent is how long a kill with no allocation counted may belong to one that was given
	// up waiting for.
	recent = time.Minute
	// maxTorn is how many torn samples of a killed pod are followed by another.
	maxTorn = 3
)

// Rise is what the counters gained across the whole kills of a pod's cgroups: kills of
// every process in a cgroup that asks for it, as a container's does.
type Rise struct {
	// Late: the pod was first sampled after the kill, so the counts are since it began,
	// and Pods and Peers are not known.
	Late bool
	// Confirmed: a sample taken after the one that showed the kill is in the counts.
	Confirmed bool
	// Torn: the pod's two files were not read at one moment.
	Torn bool
	// Recent: the counts leave out an allocation that met a limit within the last minute, and
	// RecentOwn: one that may have met the pod's or a container's.
	Recent, RecentOwn bool

	// GroupKills is how many of the pod's cgroups were killed whole, and Singles how many
	// processes of containers that are not.
	GroupKills, Singles uint64
	// Inside, Pod and Pods count the allocations that met a limit inside the pod, the pod's
	// own, and that of its QoS cgroup or the pods' root.
	Inside, Pod, Pods uint64
	// Own is how many of the whole kills a container's own limit accounts for. A container's
	// cgroup is often gone milliseconds after its kill (kubelet v1.35.5, containerd 2.3.1):
	// for a pod with one whole kill, one container gone, and no limit inside it but its
	// containers', one is also told by an allocation that met no container's still there.
	Own uint64
	// Read: every whole kill is of a container whose own file was read at the last sample.
	Read bool
	// Peers is the kills of either kind in other pods over the same time.
	Peers uint64
	// Containers is the containers with a kill of either kind, by ID. One whose cgroup went
	// before its kill was read is not among them.
	Containers []string
}

// Tracker turns successive samples of a Tree into rises.
type Tracker struct {
	pods map[string]*tracked
	// kills counts those of either kind seen in any pod.
	kills uint64
}

// mark is a pod's counters, and what stood above and beside it, at one sample. mine counts
// the pod's own part of kills.
type mark struct {
	pod                Pod
	above, kills, mine uint64
}

// tracked is a pod's newest mark and the one its rise is counted from. base stays behind
// while an allocation that met a limit may still lead to a kill.
type tracked struct {
	base, last mark
	// pending is when such an allocation was first seen, killed when a process's kill was
	// first seen after it, and absorbed when base last moved past one. absorbedOwn is that of
	// one that may have met the pod's or a container's limit.
	pending, killed, absorbed, absorbedOwn time.Time
	// shown is how many samples have shown a kill not yet returned, and above what
	// stood above the pod at the first.
	shown int
	above uint64
	// gone is the containers not in the newest sample, whose events are the last read.
	gone map[string]bool
	// nested is set once a sample of the pod has been.
	nested bool
}

// Observe takes the next sample, made at at, and returns by pod UID the rises it
// completes. again asks for another sample at once: a kill is returned on the sample
// after the one that shows it, which holds every allocation that led to it.
func (t *Tracker) Observe(s Sample, at time.Time) (rises map[string]Rise, again bool) {
	rises = t.forget(s, at)
	if t.pods == nil {
		t.pods = map[string]*tracked{}
	}
	mine := map[string]uint64{}
	for uid, now := range s.Pods {
		was := Pod{}
		if p := t.pods[uid]; p != nil {
			was = p.last.pod
		}
		mine[uid] = killsOf(was, now)
		t.kills += mine[uid]
	}
	for uid, now := range s.Pods {
		m := mark{pod: now, above: s.Root.OOM + s.QoS[now.QoS].OOM, kills: t.kills}
		p := t.pods[uid]
		if p == nil {
			t.pods[uid] = &tracked{base: m, last: m, nested: now.Nested}
			if _, left := rises[uid]; !left && killsOf(Pod{}, now) > 0 {
				rises[uid] = lateRise(now)
			}
			continue
		}
		m.mine = p.last.mine + mine[uid]
		m.pod, p.gone = carry(p.last.pod, now)
		p.last, p.nested = m, p.nested || now.Nested
		r, done, more := p.step(at)
		if done {
			rises[uid] = r
		}
		again = again || more
	}
	return rises, again
}

// carry returns now with those of was's containers that it lacks, and which those are.
func carry(was, now Pod) (Pod, map[string]bool) {
	all, single, gone := map[string]Events{}, map[string]bool{}, map[string]bool{}
	maps.Copy(all, now.Containers)
	maps.Copy(single, now.Single)
	for id, e := range was.Containers {
		if _, ok := all[id]; !ok {
			all[id], gone[id] = e, true
			if was.Single[id] {
				single[id] = true
			}
		}
	}
	now.Containers, now.Single = all, single
	return now, gone
}

// forget drops the pods s does not continue, and returns the rises it leaves unconfirmed.
func (t *Tracker) forget(s Sample, at time.Time) map[string]Rise {
	left := map[string]Rise{}
	for uid, p := range t.pods {
		if now, ok := s.Pods[uid]; ok && !restarted(p.last.pod, now) {
			continue
		}
		if p.shown > 0 {
			left[uid] = p.rise(at)
		}
		delete(t.pods, uid)
	}
	return left
}

// killsOf counts now's kills since was.
func killsOf(was, now Pod) uint64 {
	n := now.Subtree.OOMGroupKill - was.Subtree.OOMGroupKill
	for id := range now.Single {
		n += now.Containers[id].OOMKill - was.Containers[id].OOMKill
	}
	return n
}

// step returns the rise p's newest mark completes, if it does, and moves base.
func (p *tracked) step(at time.Time) (r Rise, done, again bool) {
	r = p.rise(at)
	if r.GroupKills+r.Singles == 0 {
		if p.settled(r, at) {
			p.advance(r, at)
		}
		return Rise{}, false, false
	}
	if p.shown++; p.shown == 1 {
		p.above = p.last.above
	}
	if p.shown == 1 || p.last.pod.Torn && p.shown <= maxTorn {
		return Rise{}, false, true
	}
	r.Confirmed = true
	p.advance(r, at)
	return r, true, false
}

// settled reports whether base may move past r, which has no kill to return: no allocation in
// it can still lead to one, or it has been waited for long enough.
func (p *tracked) settled(r Rise, at time.Time) bool {
	own := r.Inside + r.Pod
	if p.last.pod.Torn {
		return false
	}
	if own+r.Pods == 0 {
		return true
	}
	if p.pending.IsZero() {
		p.pending = at
	}
	if p.killed.IsZero() && p.last.pod.Subtree.OOMKill-p.base.pod.Subtree.OOMKill >= own {
		p.killed = at
	}
	ownDone := own == 0 || !p.killed.IsZero() && at.Sub(p.killed) >= groupLag
	return ownDone && r.Pods <= r.Peers || at.Sub(p.pending) >= settle
}

func (p *tracked) advance(r Rise, at time.Time) {
	if r.Inside+r.Pod+r.Pods > 0 {
		p.absorbed = at
	}
	if r.Pod+p.known() > 0 || r.Inside > 0 && len(p.gone) > 0 {
		p.absorbedOwn = at
	}
	for id := range p.gone {
		delete(p.last.pod.Containers, id)
		delete(p.last.pod.Single, id)
	}
	p.base, p.shown = p.last, 0
	p.pending, p.killed = time.Time{}, time.Time{}
}

// rise is what p's newest mark has over its base at at. What stands above the pod is taken from
// the first sample that shows the kill, read after it: any more is a neighbour's.
func (p *tracked) rise(at time.Time) Rise {
	was, now, above := p.base.pod, p.last.pod, p.last.above
	if p.shown > 0 {
		above = p.above
	}
	r := Rise{
		Torn:       now.Torn || was.Torn,
		GroupKills: now.Subtree.OOMGroupKill - was.Subtree.OOMGroupKill,
		Pods:       above - p.base.above,
		Recent:     at.Sub(p.absorbed) < recent,
		RecentOwn:  at.Sub(p.absorbedOwn) < recent,
	}
	r.Peers = p.last.kills - p.base.kills - (p.last.mine - p.base.mine)
	r.count(was, now)
	r.Read = r.Read && !slices.ContainsFunc(r.Containers, func(id string) bool { return p.gone[id] })
	if !p.nested && r.GroupKills == 1 && len(p.gone) == 1 && len(r.Containers) == 0 && r.Inside > p.known() {
		r.Own = 1
	}
	return r
}

// known is how many allocations met the limits of containers that are still there.
func (p *tracked) known() (n uint64) {
	for id, e := range p.last.pod.Containers {
		if !p.gone[id] {
			n += e.OOM - p.base.pod.Containers[id].OOM
		}
	}
	return n
}

func lateRise(now Pod) Rise {
	r := Rise{Late: true, Torn: now.Torn, GroupKills: now.Subtree.OOMGroupKill}
	r.count(Pod{}, now)
	return r
}

// count sets what r says of the pod's own cgroup and those inside it.
func (r *Rise) count(was, now Pod) {
	r.Pod = now.Local.OOM - was.Local.OOM
	all := now.Subtree.OOM - was.Subtree.OOM
	r.Inside = all - min(all, r.Pod)
	var kills uint64
	for _, id := range slices.Sorted(maps.Keys(now.Containers)) {
		old, e := was.Containers[id], now.Containers[id]
		whole, single := e.OOMGroupKill-old.OOMGroupKill, uint64(0)
		if now.Single[id] {
			single = e.OOMKill - old.OOMKill
		}
		if whole+single > 0 {
			r.Containers = append(r.Containers, id)
		}
		r.Own += min(whole, e.OOM-old.OOM)
		r.Singles += single
		kills += whole
	}
	r.Read = kills == r.GroupKills
}

// restarted reports whether now is of a cgroup made since was, under the same UID, as a
// static pod's can be.
func restarted(was, now Pod) bool {
	return now.Subtree.OOM < was.Subtree.OOM || now.Subtree.OOMKill < was.Subtree.OOMKill ||
		now.Subtree.OOMGroupKill < was.Subtree.OOMGroupKill || now.Local.OOM < was.Local.OOM
}
