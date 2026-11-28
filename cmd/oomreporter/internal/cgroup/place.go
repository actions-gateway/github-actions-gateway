package cgroup

import (
	"path"
	"strings"
)

// Level is where a cgroup is in the kubelet's tree.
type Level uint8

// The levels, from the innermost.
const (
	// InPod is a container's cgroup, or one below it or beside it in a pod.
	InPod Level = iota + 1
	PodLevel
	QoSLevel
	RootLevel
	// Above is any cgroup outside the pods' root, the node's memory among them.
	Above
)

// Place is where a cgroup is.
type Place struct {
	Level Level
	// Pod is the pod's UID, and Container the ID of the container the cgroup is in, where it
	// is in one.
	Pod, Container string
	// Exact is set where the cgroup is the container's own.
	Exact bool
}

// Locate returns where the cgroup at p, a path as the kernel prints it from the root of its
// hierarchy, is in a tree whose pods' root is root, and false for one under that root that the
// kubelet does not name so. The kernel's path may start above the mount the agent reads, as
// kind's does; the first directory named root is taken for it, since a job can name cgroups
// only below its own.
func Locate(root, p string) (Place, bool) {
	rest, ok := strings.CutPrefix(p, "/")
	if !ok {
		return Place{}, false
	}
	parts := strings.Split(rest, "/")
	want := strings.Split(root, "/")
	at := -1
	for i := 0; i+len(want) <= len(parts); i++ {
		if strings.Join(parts[i:i+len(want)], "/") == root {
			at = i + len(want)
			break
		}
	}
	if at < 0 {
		return Place{Level: Above}, true
	}
	parts = parts[at:]
	if len(parts) == 0 {
		return Place{Level: RootLevel}, true
	}
	t := &Tree{root: root}
	class := ""
	for _, c := range qosClasses {
		if parts[0] == path.Base(t.qosDir(c)) {
			class, parts = c, parts[1:]
			break
		}
	}
	if len(parts) == 0 {
		return Place{Level: QoSLevel}, true
	}
	uid, ok := t.podUIDOf(class, parts[0])
	if !ok || !strings.Contains(uid, "-") {
		return Place{}, false
	}
	if len(parts) == 1 {
		return Place{Level: PodLevel, Pod: uid}, true
	}
	containerName := cgroupName
	if t.systemdPrefix() != "" {
		containerName = scopeName
	}
	place := Place{Level: InPod, Pod: uid}
	if id := containerName.FindStringSubmatch(parts[1]); id != nil {
		place.Container, place.Exact = id[1], len(parts) == 2
	}
	return place, true
}
