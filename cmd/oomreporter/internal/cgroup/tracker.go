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
	// Recent: the counts leave out an allocation that met a limit within the last minute.
	Recent bool

	// GroupKills is how many of the pod's cgroups were killed whole.
	GroupKills uint64
	// Inside, Pod and Pods count the allocations that met a limit inside the pod, the pod's
	// own, and that of its QoS cgroup or the pods' root.
	Inside, Pod, Pods uint64
	// Peers is the whole kills in other pods over the same time.
	Peers uint64
	// Containers is the containers killed whole, by ID. One whose cgroup went unread is
	// not among them.
	Containers []string
}

// Tracker turns successive samples of a Tree into rises.
type Tracker struct {
	pods map[string]*tracked
	// whole counts the whole kills seen in any pod.
	whole uint64
}

// mark is a pod's counters, and what stood above and beside it, at one sample.
type mark struct {
	pod          Pod
	above, whole uint64
}

// tracked is a pod's newest mark and the one its rise is counted from. base stays behind
// while an allocation that met a limit may still lead to a kill.
type tracked struct {
	base, last mark
	// pending is when such an allocation was first seen, killed when a process's kill was
	// first seen after it, and absorbed when base last moved past one.
	pending, killed, absorbed time.Time
	// shown is how many samples have shown a whole kill not yet returned, and above what
	// stood above the pod at the first.
	shown int
	above uint64
}

// Observe takes the next sample, made at at, and returns by pod UID the rises it
// completes. again asks for another sample at once: a kill is returned on the sample
// after the one that shows it, which holds every allocation that led to it.
func (t *Tracker) Observe(s Sample, at time.Time) (rises map[string]Rise, again bool) {
	rises = t.forget(s)
	t.countWhole(s)
	for uid, now := range s.Pods {
		m := mark{pod: now, above: s.Root.OOM + s.QoS[now.QoS].OOM, whole: t.whole}
		p := t.pods[uid]
		if p == nil {
			t.pods[uid] = &tracked{base: m, last: m}
			if _, left := rises[uid]; !left && now.Subtree.OOMGroupKill > 0 {
				rises[uid] = lateRise(now)
			}
			continue
		}
		p.last = m
		r, done, more := p.step(at)
		if done {
			rises[uid] = r
		}
		again = again || more
	}
	return rises, again
}

// forget drops the pods s does not continue, and returns the rises it leaves unconfirmed.
func (t *Tracker) forget(s Sample) map[string]Rise {
	left := map[string]Rise{}
	for uid, p := range t.pods {
		if now, ok := s.Pods[uid]; ok && !restarted(p.last.pod, now) {
			continue
		}
		if p.shown > 0 {
			left[uid] = p.rise()
		}
		delete(t.pods, uid)
	}
	return left
}

// countWhole adds the whole kills since the last sample. The first has none before it to
// count from.
func (t *Tracker) countWhole(s Sample) {
	if t.pods == nil {
		t.pods = map[string]*tracked{}
		return
	}
	for uid, now := range s.Pods {
		t.whole += now.Subtree.OOMGroupKill
		if p := t.pods[uid]; p != nil {
			t.whole -= p.last.pod.Subtree.OOMGroupKill
		}
	}
}

// step returns the rise p's newest mark completes, if it does, and moves base.
func (p *tracked) step(at time.Time) (r Rise, done, again bool) {
	r = p.rise()
	if r.GroupKills == 0 {
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
	r.Recent = at.Sub(p.absorbed) < recent
	p.advance(r, at)
	return r, true, false
}

// settled reports whether base may move past r, which has no whole kill: no allocation in
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
	p.base, p.shown = p.last, 0
	p.pending, p.killed = time.Time{}, time.Time{}
}

// rise is what p's newest mark has over its base. What stands above the pod is taken from
// the first sample that shows the kill, read after it: any more is a neighbour's.
func (p *tracked) rise() Rise {
	was, now, above := p.base.pod, p.last.pod, p.last.above
	if p.shown > 0 {
		above = p.above
	}
	r := Rise{
		Torn:       now.Torn || was.Torn,
		GroupKills: now.Subtree.OOMGroupKill - was.Subtree.OOMGroupKill,
		Pods:       above - p.base.above,
		Containers: wholeKilled(was, now),
	}
	r.Peers = p.last.whole - p.base.whole - r.GroupKills
	r.Inside, r.Pod = ownOOM(was, now)
	return r
}

func lateRise(now Pod) Rise {
	r := Rise{Late: true, Torn: now.Torn, GroupKills: now.Subtree.OOMGroupKill, Containers: wholeKilled(Pod{}, now)}
	r.Inside, r.Pod = ownOOM(Pod{}, now)
	return r
}

func ownOOM(was, now Pod) (inside, pod uint64) {
	pod = now.Local.OOM - was.Local.OOM
	all := now.Subtree.OOM - was.Subtree.OOM
	return all - min(all, pod), pod
}

func wholeKilled(was, now Pod) []string {
	var ids []string
	for _, id := range slices.Sorted(maps.Keys(now.Containers)) {
		if now.Containers[id].OOMGroupKill > was.Containers[id].OOMGroupKill {
			ids = append(ids, id)
		}
	}
	return ids
}

// restarted reports whether now is of a cgroup made since was, under the same UID, as a
// static pod's can be.
func restarted(was, now Pod) bool {
	return now.Subtree.OOM < was.Subtree.OOM || now.Subtree.OOMKill < was.Subtree.OOMKill ||
		now.Subtree.OOMGroupKill < was.Subtree.OOMGroupKill || now.Local.OOM < was.Local.OOM
}
