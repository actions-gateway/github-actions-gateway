package cgroup

import (
	"maps"
	"time"
)

// distrust is how long after a pod's cgroup has gone one under its UID is not taken for it. It is
// longer than a kill may be after the end of the container it is of, which is before that.
const distrust = 15 * time.Second

// Guard leaves out of a sample the pods whose cgroup may be a copy. A container that has the host's
// view of the tree can make one beside a pod's own, and have it outlive that (Q1396).
type Guard struct {
	// seen is each pod's cgroup at the newest sample, or the last that could be read.
	seen map[string]string
	// doubtful is the pods that are left out.
	doubtful map[string]bool
	// gone is when a pod was first not seen.
	gone map[string]time.Time
}

// Filter returns s, made at at, without those: a pod that has had two, or one that went and came
// back, for as long as it has any.
func (g *Guard) Filter(s Sample, at time.Time) Sample {
	if g.gone == nil {
		g.doubtful, g.gone = map[string]bool{}, map[string]time.Time{}
	}
	maps.DeleteFunc(g.gone, func(_ string, since time.Time) bool { return at.Sub(since) >= distrust })
	seen := map[string]string{}
	for uid := range s.Refused {
		seen[uid] = g.seen[uid]
	}
	for uid, p := range s.Pods {
		seen[uid] = p.Cgroup
		if was := g.seen[uid]; was != "" && was != p.Cgroup {
			g.doubtful[uid] = true
		}
	}
	for uid := range seen {
		if _, back := g.gone[uid]; back || s.Doubled[uid] {
			g.doubtful[uid] = true
		}
	}
	for uid := range g.seen {
		if _, ok := seen[uid]; !ok {
			g.gone[uid] = at
			delete(g.doubtful, uid)
		}
	}
	g.seen = seen
	kept := make(map[string]Pod, len(s.Pods))
	for uid, p := range s.Pods {
		if !g.doubtful[uid] {
			kept[uid] = p
		}
	}
	s.Pods = kept
	return s
}
