package cgroup

import "time"

// Strays counts the processes killed in each pod whose kill was no part of a whole cgroup's.
type Strays struct {
	pods map[string]*stray
}

// stray is a pod's counts at the newest sample whose kills are all accounted for. since is
// when a kill after it was first seen, and until is how long kills may still be part of the
// last whole one.
type stray struct {
	base         Events
	since, until time.Time
}

// Observe takes the next sample, made at at, and the rises the Tracker returned for it, and
// returns how many such processes each pod lost, by UID. The kernel counts one process's kill
// before its whole cgroup's and the others' after it, so each is waited out for groupLag.
func (s *Strays) Observe(now Sample, rises map[string]Rise, at time.Time) map[string]uint64 {
	if s.pods == nil {
		s.pods = map[string]*stray{}
	}
	for uid := range s.pods {
		if _, ok := now.Pods[uid]; !ok {
			delete(s.pods, uid)
		}
	}
	lost := map[string]uint64{}
	for uid, pod := range now.Pods {
		e := pod.Subtree
		p := s.pods[uid]
		_, rose := rises[uid]
		switch {
		case p == nil || rose || e.OOMGroupKill != p.base.OOMGroupKill || e.OOMKill < p.base.OOMKill:
			s.pods[uid] = &stray{base: e, until: at.Add(groupLag)}
		case at.Before(p.until):
			p.base = e
		case e.OOMKill == p.base.OOMKill:
		case p.since.IsZero():
			p.since = at
		case at.Sub(p.since) >= groupLag:
			lost[uid] = e.OOMKill - p.base.OOMKill
			s.pods[uid] = &stray{base: e}
		}
	}
	return lost
}
