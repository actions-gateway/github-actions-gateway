package cgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	burstableUID  = "1b4e28ba-2fa1-11d2-883f-0016d3cca427"
	guaranteedUID = "6fa459ea-ee8a-3ca4-894e-db77e160355e"
	staticUID     = "9c1185a5c5e9fc54612808977ee8f548"
)

var (
	runnerID  = strings.Repeat("a", 64)
	sandboxID = strings.Repeat("b", 64)
)

func eventsFile(e Events) *fstest.MapFile {
	return &fstest.MapFile{Data: fmt.Appendf(nil, "low 0\nhigh 0\nmax 0\noom %d\noom_kill %d\noom_group_kill %d\n", e.OOM, e.OOMKill, e.OOMGroupKill)}
}

// mount is a cgroup v2 mount of the given directories. The nth has n as each count.
func mount(dirs ...string) fstest.MapFS {
	m := fstest.MapFS{"cgroup.controllers": {Data: []byte("memory\n")}}
	for i, dir := range dirs {
		n := uint64(i + 1)
		m[path.Join(dir, subtreeFile)] = eventsFile(Events{n, n, n})
		m[path.Join(dir, localFile)] = eventsFile(Events{n, n, n})
		m[path.Join(dir, groupFile)] = &fstest.MapFile{Data: []byte("1\n")}
	}
	return m
}

func systemd(prefix string) []string {
	root := prefix + "kubepods"
	if prefix != "" {
		root = strings.TrimSuffix(prefix, "-") + ".slice/" + root
	}
	base := path.Base(root)
	under := strings.ReplaceAll(burstableUID, "-", "_")
	guaranteed := strings.ReplaceAll(guaranteedUID, "-", "_")
	root += ".slice"
	pod := fmt.Sprintf("%s/%s-burstable.slice/%s-burstable-pod%s.slice", root, base, base, under)
	return []string{
		root, root + "/" + base + "-burstable.slice", root + "/" + base + "-besteffort.slice",
		pod, pod + "/cri-containerd-" + runnerID + ".scope", pod + "/cri-containerd-" + sandboxID + ".scope",
		fmt.Sprintf("%s/%s-pod%s.slice", root, base, guaranteed),
	}
}

func cgroupfs(prefix string) []string {
	root := path.Join(prefix, "kubepods")
	pod := root + "/burstable/pod" + burstableUID
	return []string{
		root, root + "/burstable", root + "/besteffort",
		pod, pod + "/" + runnerID, pod + "/" + sandboxID,
		root + "/pod" + guaranteedUID,
	}
}

func TestSample_ReadsTheKubeletsTree(t *testing.T) {
	n := func(i uint64) Events { return Events{i, i, i} }
	want := Sample{
		Pods: map[string]Pod{
			burstableUID: {
				QoS: "burstable", Subtree: n(4), Local: n(4),
				Containers: map[string]Events{runnerID: n(5), sandboxID: n(6)}, Single: map[string]bool{},
			},
			guaranteedUID: {Subtree: n(7), Local: n(7), Containers: map[string]Events{}, Single: map[string]bool{}},
		},
		QoS:  map[string]Events{"burstable": n(2), "besteffort": n(3)},
		Root: n(1),
	}
	for name, dirs := range map[string][]string{
		"systemd":                   systemd(""),
		"systemd under cgroupRoot":  systemd("kubelet-"),
		"cgroupfs":                  cgroupfs(""),
		"cgroupfs under cgroupRoot": cgroupfs("kubelet"),
	} {
		t.Run(name, func(t *testing.T) {
			tree, err := Open(mount(dirs...), "")
			require.NoError(t, err)
			got, err := tree.Sample()
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestSample_TakesAStaticPodsHashAsItsUID(t *testing.T) {
	tree, err := Open(mount(append(systemd(""), "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod"+staticUID+".slice")...), "")
	require.NoError(t, err)
	got, err := tree.Sample()
	require.NoError(t, err)
	assert.Contains(t, got.Pods, staticUID)
}

// A job can make cgroups below its own container with any name.
func TestSample_LeavesOutAPodLikeCgroupAJobMade(t *testing.T) {
	dirs := systemd("")
	forged := strings.ReplaceAll(burstableUID, "1b4e28ba", "deadbeef")
	tree, err := Open(mount(append(dirs,
		dirs[4]+"/kubepods-burstable-pod"+strings.ReplaceAll(forged, "-", "_")+".slice",
		dirs[4]+"/cri-containerd-"+strings.Repeat("c", 64)+".scope",
		"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podnot_a_uid.slice",
		"kubepods.slice/kubepods-pod"+strings.ReplaceAll(forged, "-", "_")+".scope",
		"kubepods.slice/kubepods-burstable.slice/pod"+forged,
	)...), "")
	require.NoError(t, err)
	got, err := tree.Sample()
	require.NoError(t, err)
	assert.Len(t, got.Pods, 2)
	assert.Len(t, got.Pods[burstableUID].Containers, 2)
}

func TestSample_LeavesOutAPodLikeCgroupfsName(t *testing.T) {
	tree, err := Open(mount(append(cgroupfs(""), "kubepods/burstable/podman")...), "")
	require.NoError(t, err)
	got, err := tree.Sample()
	require.NoError(t, err)
	assert.Len(t, got.Pods, 2)
}

func TestOpen(t *testing.T) {
	for name, tc := range map[string]struct {
		fsys    fs.FS
		root    string
		wantErr string
	}{
		"cgroup v1":                        {fstest.MapFS{"memory/kubepods/memory.oom_control": {}}, "", "not a cgroup v2 mount"},
		"no kubelet":                       {mount("system.slice"), "", "no pods' root found"},
		"two at one depth":                 {mount(append(systemd(""), cgroupfs("")...)...), "", "more than one pods' root: kubepods, kubepods.slice"},
		"two, and the operator named one":  {mount(append(systemd(""), cgroupfs("")...)...), "kubepods.slice", ""},
		"a deeper one is not looked at":    {mount(append(systemd(""), cgroupfs("made")...)...), "", ""},
		"a slice in another's":             {mount("x.slice/y-kubepods.slice"), "", "no pods' root found"},
		"a slice in no slice":              {mount("x/x-kubepods.slice"), "", "no pods' root found"},
		"named, and not a kubelet's":       {mount("system.slice"), "system.slice", "not named as"},
		"the kubelet's classes are absent": {mount("kubepods.slice"), "", "kubepods-burstable.slice"},
	} {
		t.Run(name, func(t *testing.T) {
			tree, err := Open(tc.fsys, tc.root)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "kubepods.slice", tree.root)
		})
	}
}

// hookFS fails or changes an open, as a cgroup that goes or counts while it is read.
type hookFS struct {
	fs.FS
	open func(name string) error
}

func (h *hookFS) Open(name string) (fs.File, error) {
	if err := h.open(name); err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return h.FS.Open(name)
}

func TestSample_LeavesOutWhatGoesAsItIsRead(t *testing.T) {
	dirs := systemd("")
	for name, gone := range map[string]error{"path": fs.ErrNotExist, "open file": syscall.ENODEV} {
		for _, file := range []string{localFile, groupFile} {
			t.Run(name+" of "+file, func(t *testing.T) {
				h := &hookFS{FS: mount(dirs...), open: func(string) error { return nil }}
				tree, err := Open(h, "")
				require.NoError(t, err)

				h.open = func(n string) error {
					if n == path.Join(dirs[4], file) || n == path.Join(dirs[6], subtreeFile) {
						return gone
					}
					return nil
				}
				got, err := tree.Sample()
				require.NoError(t, err)
				assert.NotContains(t, got.Pods, guaranteedUID)
				assert.NotContains(t, got.Pods[burstableUID].Containers, runnerID)
				assert.Contains(t, got.Pods[burstableUID].Containers, sandboxID)
			})
		}
	}
}

func TestSample_MarksAContainerNotKilledWhole(t *testing.T) {
	dirs := systemd("")
	m := mount(dirs...)
	m[path.Join(dirs[4], groupFile)] = &fstest.MapFile{Data: []byte("0\n")}
	tree, err := Open(m, "")
	require.NoError(t, err)
	got, err := tree.Sample()
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{runnerID: true}, got.Pods[burstableUID].Single)
}

func TestSample_FailsWhereItMayNotRead(t *testing.T) {
	dirs := systemd("")
	files := []string{path.Join(dirs[4], groupFile)}
	for _, dir := range dirs {
		files = append(files, path.Join(dir, localFile))
	}
	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			h := &hookFS{FS: mount(dirs...), open: func(string) error { return nil }}
			tree, err := Open(h, "")
			require.NoError(t, err)
			h.open = func(n string) error {
				if n == file {
					return fs.ErrPermission
				}
				return nil
			}
			_, err = tree.Sample()
			assert.ErrorIs(t, err, fs.ErrPermission)
		})
	}
}

func TestSample_MarksAPodCountedInTheMiddleOfTheRead(t *testing.T) {
	dirs := systemd("")
	m := mount(dirs...)
	h := &hookFS{FS: m, open: func(string) error { return nil }}
	tree, err := Open(h, "")
	require.NoError(t, err)

	h.open = func(n string) error {
		if n == path.Join(dirs[3], localFile) {
			m[path.Join(dirs[3], subtreeFile)] = eventsFile(Events{OOM: 9, OOMKill: 4, OOMGroupKill: 4})
		}
		return nil
	}
	got, err := tree.Sample()
	require.NoError(t, err)
	assert.True(t, got.Pods[burstableUID].Torn)
	assert.False(t, got.Pods[guaranteedUID].Torn)
}

func TestGone(t *testing.T) {
	assert.False(t, gone(nil))
	assert.False(t, gone(errors.New("other")))
}
