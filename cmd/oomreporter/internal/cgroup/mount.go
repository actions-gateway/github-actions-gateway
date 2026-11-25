package cgroup

import (
	"bufio"
	"io"
	"slices"
	"strings"
)

// LocalEvents reports whether mountinfo, a /proc/<pid>/mountinfo, shows cgroup v2 mounted
// memory_localevents. Nothing then reaches an ancestor's memory.events, so a container's
// kill leaves its pod's counters still.
func LocalEvents(mountinfo io.Reader) (bool, error) {
	sc := bufio.NewScanner(mountinfo)
	for sc.Scan() {
		_, after, ok := strings.Cut(sc.Text(), " - ")
		f := strings.Fields(after)
		if ok && len(f) == 3 && f[0] == "cgroup2" && slices.Contains(strings.Split(f[2], ","), "memory_localevents") {
			return true, nil
		}
	}
	return false, sc.Err()
}
