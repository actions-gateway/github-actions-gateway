// Package cgroup reads, from a node's cgroup v2 hierarchy, the kernel's counts of
// out-of-memory kills in each pod and in the cgroups above the pods.
package cgroup

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// Events is the out-of-memory part of a memory.events or memory.events.local file.
type Events struct {
	// OOM counts allocations that met a limit: the cgroup's own in memory.events.local,
	// its own or a descendant's in memory.events.
	OOM uint64
	// OOMKill counts processes killed, and OOMGroupKill cgroups killed whole.
	OOMKill, OOMGroupKill uint64
}

// since is what e has over old, or all of it where e is of a cgroup made again under old's
// name, whose counts start over.
func (e Events) since(old Events) Events {
	if e.OOM < old.OOM || e.OOMKill < old.OOMKill || e.OOMGroupKill < old.OOMGroupKill {
		return e
	}
	return Events{e.OOM - old.OOM, e.OOMKill - old.OOMKill, e.OOMGroupKill - old.OOMGroupKill}
}

const (
	subtreeFile = "memory.events"
	localFile   = "memory.events.local"
	groupFile   = "memory.oom.group"
	statFile    = "cgroup.stat"
)

func readEvents(fsys fs.FS, name string) (Events, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return Events{}, err
	}
	e, err := parseEvents(data)
	if err != nil {
		return Events{}, fmt.Errorf("%s: %w", name, err)
	}
	return e, nil
}

// parseEvents requires every field it returns: oom_group_kill is absent before kernel
// 5.17, and a missing count must not read as zero.
func parseEvents(data []byte) (Events, error) {
	var e Events
	fields := map[string]*uint64{"oom": &e.OOM, "oom_kill": &e.OOMKill, "oom_group_kill": &e.OOMGroupKill}
	for sc := bufio.NewScanner(bytes.NewReader(data)); sc.Scan(); {
		key, value, _ := strings.Cut(sc.Text(), " ")
		field, ok := fields[key]
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return Events{}, fmt.Errorf("field %s: %w", key, err)
		}
		*field = n
		delete(fields, key)
	}
	for _, key := range []string{"oom", "oom_kill", "oom_group_kill"} {
		if _, missing := fields[key]; missing {
			return Events{}, fmt.Errorf("no field %s", key)
		}
	}
	return e, nil
}
