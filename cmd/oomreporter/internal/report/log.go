package report

import (
	"slices"
	"time"
)

// again is how long a pod's kills of single processes are taken for one.
const again = time.Minute

// Log is the kills of a node's report, the oldest first.
type Log struct {
	kills []Kill
}

// Kills is the log's kills.
func (l *Log) Kills() []Kill { return slices.Clone(l.kills) }

// Add adds k, and reports whether it was not one of a pod's already there. A full log drops the
// oldest kill of the pod with the most, so one pod's do not push out another's.
func (l *Log) Add(k Kill) bool {
	for _, old := range l.kills {
		if repeats(old, k) {
			return false
		}
	}
	if k.Rise != nil && len(k.Rise.Containers) > maxContainers {
		r := *k.Rise
		r.Containers, r.Read = r.Containers[:maxContainers], false
		k.Rise = &r
	}
	l.kills = append(l.kills, k)
	if len(l.kills) > MaxKills {
		l.drop()
	}
	return true
}

// repeats reports whether k, with no whole kill in it, is of the pod and the containers that old
// is of, soon after.
func repeats(old, k Kill) bool {
	if old.Pod != k.Pod || k.Time().Sub(old.Time()) >= again {
		return false
	}
	if k.Rise == nil || old.Rise == nil {
		return k.Rise == old.Rise
	}
	return k.Rise.GroupKills+old.Rise.GroupKills == 0 && slices.Equal(old.Rise.Containers, k.Rise.Containers)
}

func (l *Log) drop() {
	of, most := map[string]int{}, 0
	for _, k := range l.kills {
		of[k.Pod]++
		most = max(most, of[k.Pod])
	}
	for i, k := range l.kills {
		if of[k.Pod] == most {
			l.kills = slices.Delete(l.kills, i, i+1)
			return
		}
	}
}

// Lines is the kernel log's kills of a node's report, the oldest first.
type Lines struct {
	lines []Line
}

// Lines is the log's lines.
func (l *Lines) Lines() []Line { return slices.Clone(l.lines) }

// Add adds line, and reports whether it was not there. A full log drops the oldest.
func (l *Lines) Add(line Line) bool {
	if slices.Contains(l.lines, line) {
		return false
	}
	l.lines = append(l.lines, line)
	if len(l.lines) > MaxLines {
		l.lines = slices.Delete(l.lines, 0, 1)
	}
	return true
}
