// Package logreader reads the kernel's out-of-memory kills from its log and hands the node's agent
// numbers for each: the pod, the container killed whole, the level of the cgroup whose limit ran
// out, and a time. It hands on no text of a line.
package logreader

import (
	"log/slog"
	"time"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/cgroup"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/kernlog"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/report"
)

// Reader reads the whole log each time, and sends a line once.
type Reader struct {
	// Root is the kubelet's pods' root, as the agent is given it.
	Root string
	// Log returns the kernel's log, as SYSLOG_ACTION_READ_ALL does.
	Log func() (string, error)
	// Now returns the node's clock and the time since its boot, which the log's stamps count.
	Now  func() (time.Time, time.Duration)
	Send func(report.Line) error
	Logs *slog.Logger

	sent    map[sent]bool
	refused int
}

type sent struct {
	at  time.Duration
	pid int
}

// Once reads the log and sends what it has not. Where a send fails, every line is sent again on
// the next.
func (r *Reader) Once() error {
	buf, err := r.Log()
	if err != nil {
		return err
	}
	wall, boot := r.Now()
	kills, refused := kernlog.Read(buf)
	// Refusals come in the log's order, so those past the last count are new, until it wraps.
	for _, err := range refused[min(r.refused, len(refused)):] {
		r.Logs.Warn("refused a line of the kernel's log", "error", err.Error())
	}
	r.refused = len(refused)
	seen := make(map[sent]bool, len(kills))
	for _, k := range kills {
		key := sent{k.At, k.PID}
		seen[key] = true
		if r.sent[key] {
			continue
		}
		line, ok := r.line(k, wall, boot)
		if !ok {
			continue
		}
		if err := r.Send(line); err != nil {
			r.sent = nil
			return err
		}
	}
	r.sent = seen
	return nil
}

// line is k's numbers, where the group the kernel killed whole is a container's own cgroup.
// kernlog has the group hold the victim and the limit hold the group, so the group's pod and
// container are the victim's, and the limit is placed wherever the group is.
func (r *Reader) line(k kernlog.Kill, wall time.Time, boot time.Duration) (report.Line, bool) {
	group, _ := cgroup.Locate(r.Root, k.Group)
	if !k.Stamped || !group.Exact {
		return report.Line{}, false
	}
	level := cgroup.Above
	if k.Limit != "" {
		limit, _ := cgroup.Locate(r.Root, k.Limit)
		level = limit.Level
	}
	return report.Line{
		Pod: group.Pod, Container: group.Container, Level: level,
		At: wall.Add(k.At - boot).UnixMilli(),
	}, true
}
