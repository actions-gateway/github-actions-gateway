package cgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"syscall"
)

// maxRootDepth is how far below the mount the pods' root is looked for. A kubelet with
// no cgroupRoot puts it at 1, and kind at 2.
const maxRootDepth = 3

// qosClasses are the kubelet's cgroups between the pods' root and a pod. A Guaranteed
// pod has none.
var qosClasses = []string{"burstable", "besteffort"}

var (
	podUID      = regexp.MustCompile(`^(?:[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}|[0-9a-f]{32})$`)
	containerID = regexp.MustCompile(`^(?:cri-containerd-|crio-)?([0-9a-f]{64})(?:\.scope)?$`)
)

// Tree is the part of a cgroup v2 hierarchy the kubelet makes.
type Tree struct {
	fsys fs.FS
	root string
}

// Open returns the kubelet's tree in fsys, a cgroup v2 mount. An empty root is found:
// the shallowest directory named as the kubelet names its pods' root, and an error where
// there are two.
func Open(fsys fs.FS, root string) (*Tree, error) {
	if _, err := fs.Stat(fsys, "cgroup.controllers"); err != nil {
		return nil, fmt.Errorf("not a cgroup v2 mount: %w", err)
	}
	if root == "" {
		var err error
		if root, err = findRoot(fsys); err != nil {
			return nil, err
		}
	}
	if !isRootName(root) {
		return nil, fmt.Errorf("%q is not named as a kubelet's pods' root", root)
	}
	t := &Tree{fsys: fsys, root: root}
	if _, err := t.Sample(); err != nil {
		return nil, fmt.Errorf("pods' root %s: %w", root, err)
	}
	return t, nil
}

func findRoot(fsys fs.FS) (string, error) {
	level := []string{"."}
	for range maxRootDepth {
		var found, next []string
		for _, dir := range level {
			children, err := subdirs(fsys, dir)
			if err != nil {
				return "", err
			}
			for _, name := range children {
				child := path.Join(dir, name)
				if isRootName(child) {
					found = append(found, child)
				}
				next = append(next, child)
			}
		}
		switch len(found) {
		case 0:
			level = next
		case 1:
			return found[0], nil
		default:
			return "", fmt.Errorf("more than one pods' root: %s", strings.Join(found, ", "))
		}
	}
	return "", errors.New("no pods' root found")
}

// isRootName reports whether dir is named as the kubelet names the pods' root: kubepods
// under the cgroupfs driver, and under systemd's a slice whose name repeats its parent's.
func isRootName(dir string) bool {
	parent, base := path.Split(dir)
	if base == "kubepods" || base == "kubepods.slice" {
		return true
	}
	slice, ok := strings.CutSuffix(path.Base(parent), ".slice")
	return ok && base == slice+"-kubepods.slice"
}

// systemdPrefix is what every name below the root starts with under the systemd driver,
// and empty under cgroupfs.
func (t *Tree) systemdPrefix() string {
	name, ok := strings.CutSuffix(path.Base(t.root), ".slice")
	if !ok {
		return ""
	}
	return name + "-"
}

func (t *Tree) qosDir(class string) string {
	if p := t.systemdPrefix(); p != "" {
		return path.Join(t.root, p+class+".slice")
	}
	return path.Join(t.root, class)
}

// podUIDOf returns the UID of the pod whose cgroup is named name under class, "" for a
// Guaranteed pod's parent.
func (t *Tree) podUIDOf(class, name string) (string, bool) {
	if p := t.systemdPrefix(); p != "" {
		if class != "" {
			p += class + "-"
		}
		rest, ok := strings.CutPrefix(name, p+"pod")
		if !ok {
			return "", false
		}
		if rest, ok = strings.CutSuffix(rest, ".slice"); !ok {
			return "", false
		}
		// systemd reads a dash as a level, so the kubelet writes the UID's as underscores.
		uid := strings.ReplaceAll(rest, "_", "-")
		return uid, podUID.MatchString(uid)
	}
	uid, ok := strings.CutPrefix(name, "pod")
	return uid, ok && podUID.MatchString(uid)
}

// Pod is one pod's counters.
type Pod struct {
	// QoS is the class whose cgroup the pod is in, empty for a Guaranteed pod.
	QoS            string
	Subtree, Local Events
	// Torn is set where an allocation met a limit while the two were read, so that they
	// may not be of one moment.
	Torn bool
	// Containers is each container's local events by container ID, the sandbox's included.
	Containers map[string]Events
}

// Sample is every pod's counters, and the local events of the cgroups above them.
type Sample struct {
	Pods map[string]Pod // by UID
	QoS  map[string]Events
	Root Events
}

// Sample reads the tree. A pod or container removed while it is read is left out.
func (t *Tree) Sample() (Sample, error) {
	s := Sample{Pods: map[string]Pod{}, QoS: map[string]Events{}}
	if err := t.samplePods(s.Pods, "", t.root); err != nil {
		return Sample{}, err
	}
	for _, class := range qosClasses {
		dir := t.qosDir(class)
		if err := t.samplePods(s.Pods, class, dir); err != nil {
			return Sample{}, err
		}
		e, err := readEvents(t.fsys, path.Join(dir, localFile))
		if err != nil {
			return Sample{}, err
		}
		s.QoS[class] = e
	}
	var err error
	s.Root, err = readEvents(t.fsys, path.Join(t.root, localFile))
	return s, err
}

func (t *Tree) samplePods(pods map[string]Pod, class, dir string) error {
	names, err := subdirs(t.fsys, dir)
	if err != nil {
		return err
	}
	for _, name := range names {
		uid, ok := t.podUIDOf(class, name)
		if !ok {
			continue
		}
		pod, err := t.samplePod(path.Join(dir, name))
		if gone(err) {
			continue
		}
		if err != nil {
			return err
		}
		pod.QoS = class
		pods[uid] = pod
	}
	return nil
}

func (t *Tree) samplePod(dir string) (Pod, error) {
	names, err := subdirs(t.fsys, dir)
	if err != nil {
		return Pod{}, err
	}
	pod := Pod{Containers: map[string]Events{}}
	for _, name := range names {
		id := containerID.FindStringSubmatch(name)
		if id == nil {
			continue
		}
		e, err := readEvents(t.fsys, path.Join(dir, name, localFile))
		if gone(err) {
			continue
		}
		if err != nil {
			return Pod{}, err
		}
		pod.Containers[id[1]] = e
	}
	if pod.Subtree, err = readEvents(t.fsys, path.Join(dir, subtreeFile)); err != nil {
		return Pod{}, err
	}
	if pod.Local, err = readEvents(t.fsys, path.Join(dir, localFile)); err != nil {
		return Pod{}, err
	}
	again, err := readEvents(t.fsys, path.Join(dir, subtreeFile))
	pod.Torn = again != pod.Subtree
	return pod, err
}

// gone reports whether err is a cgroup's removal: ENOENT for its path, and ENODEV for a
// file of it already open.
func gone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENODEV)
}

func subdirs(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}
