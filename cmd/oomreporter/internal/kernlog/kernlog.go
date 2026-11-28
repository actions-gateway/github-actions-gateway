// Package kernlog reads the kernel's out-of-memory kills from its log, as syslog(2)'s
// SYSLOG_ACTION_READ_ALL gives it: one record per line, each line its own prefix.
//
// A kill's line prints names a job can choose, so a line is taken only in its full form, and
// one that is not is refused. What is left can still be false about the job's own pod.
package kernlog

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Kill is a kill's line, and the group the kernel then killed whole.
type Kill struct {
	// At is the line's stamp, since the kernel's boot; Stamped is whether it has one.
	At      time.Duration
	Stamped bool
	// Constraint is the kernel's, such as CONSTRAINT_MEMCG.
	Constraint string
	// Limit is the path of the cgroup whose limit ran out, "" where it was the node's.
	Limit string
	// Victim is the path of the killed process's cgroup.
	Victim string
	Task   string
	PID    int
	UID    int64
	// Group is the path of the cgroup killed whole with it, "" where none was named.
	Group string
}

// Each refusal of a line.
var (
	ErrPriority   = errors.New("not the kernel's facility at the priority it prints a kill with")
	ErrDuplicate  = errors.New("a key appears twice")
	ErrConstraint = errors.New("constraint=CONSTRAINT_MEMCG and oom_memcg not together")
	ErrForm       = errors.New("not a kill's line in full")
	ErrPath       = errors.New("a path without a leading /")
	ErrOutside    = errors.New("the victim is not inside the cgroup whose limit it names")
)

const (
	killTag     = "oom-kill:"
	groupHead   = "Tasks in "
	groupTail   = " are going to be killed due to memory.oom.group set"
	kernelInfo  = 6
	constraintK = "constraint="
	globalOOM   = ",global_oom"
	oomMemcg    = ",oom_memcg="
)

// keys are a kill line's in the order the kernel prints them, but for the limit, which is
// oomMemcg or globalOOM between mems_allowed and task_memcg. A kernel without CONFIG_CPUSETS
// prints no cpuset or mems_allowed, so its lines are refused.
var keys = []string{",nodemask=", ",cpuset=", ",mems_allowed=", ",task_memcg=", ",task=", ",pid=", ",uid="}

var constraints = map[string]bool{
	"CONSTRAINT_NONE": true, "CONSTRAINT_CPUSET": true, "CONSTRAINT_MEMORY_POLICY": true, "CONSTRAINT_MEMCG": true,
}

var (
	prefix   = regexp.MustCompile(`^<(\d+)>(?:\[ *(\d+)\.(\d{6})\])?(\[ *[TC]\d+\])?`)
	nodeList = regexp.MustCompile(`^(?:\d+(?:-\d+)?(?:,\d+(?:-\d+)?)*)?$`)
	digits   = regexp.MustCompile(`^\d+$`)
	signed   = regexp.MustCompile(`^-?\d+$`)
)

// Read returns the kills in buf, a whole READ_ALL, in its order, and why each line that
// starts as a kill's was refused.
func Read(buf string) (kills []Kill, refused []error) {
	open := -1
	for line := range strings.SplitSeq(buf, "\n") {
		priority, at, stamped, text, ok := split(line)
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(text, killTag):
			open = -1
			k, err := parseKill(priority, text)
			if err != nil {
				refused = append(refused, err)
				continue
			}
			k.At, k.Stamped = at, stamped
			kills = append(kills, k)
			open = len(kills) - 1
		case strings.HasPrefix(text, groupHead) && open >= 0:
			if path, err := parseGroup(priority, text); err == nil && holds(path, kills[open].Victim) && holds(kills[open].Limit, path) {
				kills[open].Group = path
				open = -1
			}
		}
	}
	return kills, refused
}

// split cuts a line's prefix: its priority, its stamp where it has one, and its text.
func split(line string) (priority int, at time.Duration, stamped bool, text string, ok bool) {
	m := prefix.FindStringSubmatchIndex(line)
	if m == nil {
		return 0, 0, false, "", false
	}
	priority, err := strconv.Atoi(line[m[2]:m[3]])
	if err != nil {
		return 0, 0, false, "", false
	}
	text = line[m[1]:]
	if m[4] >= 0 {
		sec, _ := strconv.ParseInt(line[m[4]:m[5]], 10, 64)
		usec, _ := strconv.ParseInt(line[m[6]:m[7]], 10, 64)
		at, stamped = time.Duration(sec)*time.Second+time.Duration(usec)*time.Microsecond, true
	}
	if m[4] >= 0 || m[8] >= 0 {
		var spaced bool
		if text, spaced = strings.CutPrefix(text, " "); !spaced {
			return 0, 0, false, "", false
		}
	}
	return priority, at, stamped, text, true
}

// parseKill reads text, a kill's line in the order dump_oom_victim prints it.
func parseKill(priority int, text string) (Kill, error) {
	if priority != kernelInfo {
		return Kill{}, ErrPriority
	}
	body := strings.TrimPrefix(text, killTag)
	for _, key := range append([]string{constraintK, globalOOM, oomMemcg}, keys...) {
		if strings.Count(body, key) > 1 {
			return Kill{}, ErrDuplicate
		}
	}
	if !strings.HasPrefix(body, constraintK) {
		return Kill{}, ErrForm
	}
	// Where a line holds both, the form refuses it: the kernel prints one, after mems_allowed.
	global := strings.Contains(body, globalOOM)
	memcg, limitKey := !global, oomMemcg
	if global {
		limitKey = globalOOM
	}
	order := append(append(append([]string{}, keys[:3]...), limitKey), keys[3:]...)
	values := make(map[string]string, len(order))
	rest := body[len(constraintK):]
	last := constraintK
	for _, key := range order {
		i := strings.Index(rest, key)
		if i < 0 {
			return Kill{}, ErrForm
		}
		values[last], rest, last = rest[:i], rest[i+len(key):], key
	}
	values[last] = rest

	k := Kill{Constraint: values[constraintK], Victim: values[",task_memcg="], Task: values[",task="]}
	switch {
	case !constraints[k.Constraint], values[",nodemask="] != "(null)" && !nodeList.MatchString(values[",nodemask="]),
		!nodeList.MatchString(values[",mems_allowed="]), global && values[globalOOM] != "",
		!digits.MatchString(values[",pid="]), !signed.MatchString(values[",uid="]):
		return Kill{}, ErrForm
	}
	if memcg {
		k.Limit = values[oomMemcg]
	}
	if (k.Constraint == "CONSTRAINT_MEMCG") != memcg {
		return Kill{}, ErrConstraint
	}
	if memcg && !strings.HasPrefix(k.Limit, "/") || !strings.HasPrefix(k.Victim, "/") {
		return Kill{}, ErrPath
	}
	if !holds(k.Limit, k.Victim) {
		return Kill{}, ErrOutside
	}
	k.PID, _ = strconv.Atoi(values[",pid="])
	k.UID, _ = strconv.ParseInt(values[",uid="], 10, 64)
	return k, nil
}

// parseGroup reads text, the line mem_cgroup_print_oom_group prints, and returns its path.
func parseGroup(priority int, text string) (string, error) {
	if priority != kernelInfo {
		return "", ErrPriority
	}
	path, ok := strings.CutSuffix(strings.TrimPrefix(text, groupHead), groupTail)
	switch {
	case !ok:
		return "", ErrForm
	case !strings.HasPrefix(path, "/"):
		return "", ErrPath
	}
	return path, nil
}

// holds reports whether the cgroup at path is outer or inside it; "" holds every cgroup.
func holds(outer, path string) bool {
	return outer == "" || path == outer || strings.HasPrefix(path, strings.TrimSuffix(outer, "/")+"/")
}
