// Package report is what a node's agent tells the controller: counts, and the identifiers the
// node's cgroups are named by.
package report

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/cgroup"
)

const (
	// Key is the key of a report in its ConfigMap.
	Key = "report"
	// MaxKills is the most kills a report holds, and MaxSize its most bytes.
	MaxKills = 128
	MaxSize  = 256 << 10
	// MaxLines is the most of the kernel log's kills a report holds.
	MaxLines = 128
	// LineSize is a Line's size in binary.
	LineSize = 16 + 32 + 1 + 8

	version = 1
	prefix  = "oom-report-"
	// maxContainers is the most containers of one kill. A full report of them is under MaxSize.
	maxContainers = 16
)

var (
	podUID      = regexp.MustCompile(`^[0-9a-f-]{32,36}$`)
	containerID = regexp.MustCompile(`^[0-9a-f]{64}$`)
	dashedUID   = regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
)

// Name is the name of the ConfigMap that holds node's report.
func Name(node string) string { return prefix + node }

// Node is the node whose report a ConfigMap of that name holds, where it is a report's.
func Node(name string) (string, bool) {
	node, ok := strings.CutPrefix(name, prefix)
	return node, ok && node != ""
}

// Kill is one or more kills in a pod at one time: either Rise, or Processes.
type Kill struct {
	Pod string `json:"pod"` // UID
	// At is when the agent saw it, in milliseconds since 1970 on its node's clock.
	At   int64        `json:"at"`
	Rise *cgroup.Rise `json:"rise,omitempty"`
	// Processes is how many processes were killed where no cgroup was killed whole.
	Processes uint64 `json:"processes,omitempty"`
}

// Time is At.
func (k Kill) Time() time.Time { return time.UnixMilli(k.At) }

// Line is a kill the kernel's log names, as the node's log reader gave it: numbers, and never
// the line's text.
type Line struct {
	// Pod is the victim's pod's UID, and Container the container the kernel killed whole.
	Pod       string `json:"pod"`
	Container string `json:"container"`
	// Level is that of the cgroup whose limit ran out.
	Level cgroup.Level `json:"level"`
	// At is the line's stamp, in milliseconds since 1970 on its node's clock.
	At int64 `json:"at"`
}

// Lines, where it is set, is read by no controller older than it: a report's version is 1 either
// way.
type wire struct {
	Version int    `json:"version"`
	Kills   []Kill `json:"kills"`
	Lines   []Line `json:"lines,omitempty"`
}

// Encode is the report of kills and lines.
func Encode(kills []Kill, lines []Line) (string, error) {
	data, err := json.Marshal(wire{version, kills, lines})
	return string(data), err
}

// Decode returns the kills and lines of a report, which is the word of the node that wrote it.
// An empty one, as the controller makes it, has none.
func Decode(data string) ([]Kill, []Line, error) {
	if data == "" {
		return nil, nil, nil
	}
	if len(data) > MaxSize {
		return nil, nil, fmt.Errorf("%d bytes, over %d", len(data), MaxSize)
	}
	var w wire
	if err := json.Unmarshal([]byte(data), &w); err != nil {
		return nil, nil, errors.New("not a report")
	}
	if w.Version != version {
		return nil, nil, fmt.Errorf("version %d, not %d", w.Version, version)
	}
	if len(w.Kills) > MaxKills {
		return nil, nil, fmt.Errorf("%d kills, over %d", len(w.Kills), MaxKills)
	}
	for i, k := range w.Kills {
		if err := k.check(); err != nil {
			return nil, nil, fmt.Errorf("kill %d: %w", i, err)
		}
	}
	if len(w.Lines) > MaxLines {
		return nil, nil, fmt.Errorf("%d lines, over %d", len(w.Lines), MaxLines)
	}
	for i, l := range w.Lines {
		if err := l.check(); err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", i, err)
		}
	}
	return w.Kills, w.Lines, nil
}

func (l Line) check() error {
	switch {
	case !dashedUID.MatchString(l.Pod):
		return errors.New("no pod's UID")
	case !containerID.MatchString(l.Container):
		return errors.New("no container's ID")
	case l.Level < cgroup.InPod || l.Level > cgroup.Above:
		return fmt.Errorf("no level %d", l.Level)
	}
	return nil
}

// MarshalBinary is l in LineSize bytes: the pod's UID, the container's ID, the level, and the time.
func (l Line) MarshalBinary() ([]byte, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	pod, _ := hex.DecodeString(strings.ReplaceAll(l.Pod, "-", ""))
	container, _ := hex.DecodeString(l.Container)
	b := append(append(pod, container...), byte(l.Level))
	return binary.BigEndian.AppendUint64(b, uint64(l.At)), nil
}

// UnmarshalBinary reads what MarshalBinary writes.
func (l *Line) UnmarshalBinary(b []byte) error {
	if len(b) != LineSize {
		return fmt.Errorf("%d bytes, not %d", len(b), LineSize)
	}
	pod := hex.EncodeToString(b[:16])
	*l = Line{
		Pod:       pod[:8] + "-" + pod[8:12] + "-" + pod[12:16] + "-" + pod[16:20] + "-" + pod[20:],
		Container: hex.EncodeToString(b[16:48]),
		Level:     cgroup.Level(b[48]),
		At:        int64(binary.BigEndian.Uint64(b[49:])),
	}
	return l.check()
}

func (k Kill) check() error {
	switch {
	case !podUID.MatchString(k.Pod):
		return errors.New("no pod's UID")
	case (k.Rise == nil) == (k.Processes == 0):
		return errors.New("not one of a rise and processes")
	case k.Rise == nil:
		return nil
	case len(k.Rise.Containers) > maxContainers:
		return fmt.Errorf("%d containers, over %d", len(k.Rise.Containers), maxContainers)
	}
	for _, id := range k.Rise.Containers {
		if !containerID.MatchString(id) {
			return errors.New("no container's ID")
		}
	}
	return nil
}
