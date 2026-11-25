// Package agent samples a node's cgroup counters and reports the kills they show.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/cgroup"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/report"
)

const (
	// burstFor is how long samples are burstEvery apart after a pod's counts change, at most
	// once in burstGap for a pod, so that a job cannot hold the agent in one.
	burstFor   = 200 * time.Millisecond
	burstEvery = 20 * time.Millisecond
	burstGap   = time.Minute
	// maxFailures is how many samples in a row may fail.
	maxFailures = 20
)

// Tree is the node's cgroups.
type Tree interface {
	Sample() (cgroup.Sample, error)
}

// Agent samples Tree and hands Write the report's kills each time one is added.
type Agent struct {
	Tree Tree
	// Poll is the longest time between two samples.
	Poll time.Duration
	// Watch, if set, is told of each sample.
	Watch func(cgroup.Sample)
	// Changed is sent the UID of a pod whose counts are known to have changed.
	Changed <-chan string
	Write   func([]report.Kill)
	Log     *slog.Logger

	tracker cgroup.Tracker
	strays  cgroup.Strays
	kills   report.Log
	counts  map[string]cgroup.Events
	bursts  map[string]time.Time
	until   time.Time
}

// Run samples until ctx is done, or the tree cannot be read.
func (a *Agent) Run(ctx context.Context) error {
	failures := 0
	for {
		wait, err := a.sample(time.Now())
		if failures++; err == nil {
			failures = 0
		} else if a.Log.Warn("cannot sample", "error", err); failures == maxFailures {
			return fmt.Errorf("%d samples in a row: %w", failures, err)
		}
		if !a.wait(ctx, wait) {
			return nil
		}
	}
}

// wait returns after d, or sooner where a pod's counts change that may have a burst, and reports
// whether ctx is not done.
func (a *Agent) wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		case uid := <-a.Changed:
			if a.burst(uid, time.Now()) {
				return true
			}
		}
	}
}

// burst starts uid's, and reports whether it may have one.
func (a *Agent) burst(uid string, now time.Time) bool {
	if last, ok := a.bursts[uid]; ok && now.Sub(last) < burstGap {
		return false
	}
	if a.bursts == nil {
		a.bursts = map[string]time.Time{}
	}
	a.bursts[uid], a.until = now, now.Add(burstFor)
	return true
}

// sample takes a sample at now, and returns how long to wait for the next.
func (a *Agent) sample(now time.Time) (time.Duration, error) {
	s, err := a.Tree.Sample()
	if err != nil {
		return a.Poll, err
	}
	rises, again := a.tracker.Observe(s, now)
	lost := a.strays.Observe(s, rises, now)
	a.record(rises, lost, now)
	a.compare(s, now)
	if a.Watch != nil {
		a.Watch(s)
	}
	switch {
	case again:
		return 0, nil
	case now.Before(a.until):
		return burstEvery, nil
	}
	return a.Poll, nil
}

func (a *Agent) record(rises map[string]cgroup.Rise, lost map[string]uint64, now time.Time) {
	added := false
	for uid, r := range rises {
		added = a.kills.Add(report.Kill{Pod: uid, At: now.UnixMilli(), Rise: &r}) || added
	}
	for uid, n := range lost {
		added = a.kills.Add(report.Kill{Pod: uid, At: now.UnixMilli(), Processes: n}) || added
	}
	if added {
		a.Write(a.kills.Kills())
	}
}

// compare starts a burst for each pod whose counts changed, and drops what is kept of those gone.
func (a *Agent) compare(s cgroup.Sample, now time.Time) {
	counts := make(map[string]cgroup.Events, len(s.Pods))
	for uid, pod := range s.Pods {
		counts[uid] = pod.Subtree
		if was, ok := a.counts[uid]; ok && was != pod.Subtree {
			a.burst(uid, now)
		}
	}
	a.counts = counts
	for uid, last := range a.bursts {
		if _, ok := counts[uid]; !ok && now.Sub(last) >= burstGap {
			delete(a.bursts, uid)
		}
	}
}
