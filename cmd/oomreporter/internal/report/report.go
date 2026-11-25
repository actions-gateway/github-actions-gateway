// Package report is what a node's agent tells the controller: counts, and the identifiers the
// node's cgroups are named by.
package report

import (
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

	version = 1
	prefix  = "oom-report-"
	// maxContainers is the most containers of one kill. A full report of them is under MaxSize.
	maxContainers = 16
)

var (
	podUID      = regexp.MustCompile(`^[0-9a-f-]{32,36}$`)
	containerID = regexp.MustCompile(`^[0-9a-f]{64}$`)
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

type wire struct {
	Version int    `json:"version"`
	Kills   []Kill `json:"kills"`
}

// Encode is the report of kills.
func Encode(kills []Kill) (string, error) {
	data, err := json.Marshal(wire{version, kills})
	return string(data), err
}

// Decode returns the kills of a report, which is the word of the node that wrote it. An empty
// one, as the controller makes it, has none.
func Decode(data string) ([]Kill, error) {
	if data == "" {
		return nil, nil
	}
	if len(data) > MaxSize {
		return nil, fmt.Errorf("%d bytes, over %d", len(data), MaxSize)
	}
	var w wire
	if err := json.Unmarshal([]byte(data), &w); err != nil {
		return nil, errors.New("not a report")
	}
	if w.Version != version {
		return nil, fmt.Errorf("version %d, not %d", w.Version, version)
	}
	if len(w.Kills) > MaxKills {
		return nil, fmt.Errorf("%d kills, over %d", len(w.Kills), MaxKills)
	}
	for i, k := range w.Kills {
		if err := k.check(); err != nil {
			return nil, fmt.Errorf("kill %d: %w", i, err)
		}
	}
	return w.Kills, nil
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
