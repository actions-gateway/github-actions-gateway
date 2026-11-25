package cgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
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
		m[path.Join(dir, statFile)] = &fstest.MapFile{Data: []byte("nr_descendants 0\nnr_dying_descendants 0\n")}
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
	for name, dirs := range map[string][]string{
		"systemd":                       systemd(""),
		"systemd under cgroupRoot":      systemd("kubelet-"),
		"cgroupfs":                      cgroupfs(""),
		"cgroupfs under cgroupRoot":     cgroupfs("kubelet"),
		"cgroupfs as deep as it may be": cgroupfs("a/b"),
	} {
		t.Run(name, func(t *testing.T) {
			want := Sample{
				Pods: map[string]Pod{
					burstableUID: {
						Cgroup: dirs[3], QoS: "burstable", Subtree: n(4), Local: n(4),
						Containers: map[string]Events{runnerID: n(5), sandboxID: n(6)}, Single: map[string]bool{}, Refused: map[string]bool{},
					},
					guaranteedUID: {Cgroup: dirs[6], Subtree: n(7), Local: n(7), Containers: map[string]Events{}, Single: map[string]bool{}, Refused: map[string]bool{}},
				},
				QoS:     map[string]Events{"burstable": n(2), "besteffort": n(3)},
				Root:    n(1),
				Refused: map[string]bool{},
				Doubled: map[string]bool{},
			}
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
		"deeper than that":                 {mount(cgroupfs("a/b/c")...), "", "no pods' root found"},
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
		for _, file := range []string{localFile, groupFile, statFile} {
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

func TestSample_MarksAPodWithACgroupBelowAContainer(t *testing.T) {
	dirs := systemd("")
	for stat, want := range map[string]bool{
		"nr_descendants 0\nnr_dying_descendants 0\n":  false,
		"nr_descendants 12\nnr_dying_descendants 0\n": true,
		"nr_descendants 0\nnr_dying_descendants 1\n":  true,
	} {
		m := mount(dirs...)
		m[path.Join(dirs[4], statFile)] = &fstest.MapFile{Data: []byte(stat)}
		tree, err := Open(m, "")
		require.NoError(t, err)
		got, err := tree.Sample()
		require.NoError(t, err)
		assert.Equal(t, want, got.Pods[burstableUID].Nested, stat)
		assert.False(t, got.Pods[guaranteedUID].Nested)
	}
}

func TestSample_MarksAPodWithACgroupThatIsNoContainers(t *testing.T) {
	id := strings.Repeat("c", 64)
	for name, tc := range map[string]struct {
		dirs   []string
		child  string
		nested bool
	}{
		"Docker's":                        {systemd(""), "docker-" + id + ".scope", true},
		"a slice":                         {systemd(""), "docker.slice", true},
		"Docker's, its own driver":        {systemd(""), id, true},
		"a container's, and more":         {systemd(""), "x-cri-containerd-" + id + ".scope", true},
		"a container's, the other driver": {cgroupfs(""), "cri-containerd-" + id + ".scope", true},
		"Docker's under cgroupfs":         {cgroupfs(""), "docker", true},
		"CRI-O's monitor":                 {systemd(""), "crio-conmon-" + id + ".scope", false},
		"CRI-O's monitor, under cgroupfs": {cgroupfs(""), "crio-conmon-" + id, false},
		"CRI-O's container":               {systemd(""), "crio-" + id + ".scope", false},
		"one no driver's name tells":      {cgroupfs(""), id, false},
	} {
		t.Run(name, func(t *testing.T) {
			tree, err := Open(mount(append(tc.dirs, path.Join(tc.dirs[3], tc.child))...), "")
			require.NoError(t, err)
			got, err := tree.Sample()
			require.NoError(t, err)
			assert.Equal(t, tc.nested, got.Pods[burstableUID].Nested)
		})
	}
}

// mayNotRead samples the tree of dirs with one file forbidden.
func mayNotRead(t *testing.T, dirs []string, file string) (Sample, error) {
	h := &hookFS{FS: mount(dirs...), open: func(string) error { return nil }}
	tree, err := Open(h, "")
	require.NoError(t, err)
	h.open = func(n string) error {
		if n == file {
			return fs.ErrPermission
		}
		return nil
	}
	return tree.Sample()
}

func TestSample_FailsWhereItMayNotReadAboveThePods(t *testing.T) {
	dirs := systemd("")
	for _, dir := range dirs[:3] {
		for _, file := range []string{dir, path.Join(dir, localFile)} {
			t.Run(file, func(t *testing.T) {
				_, err := mayNotRead(t, dirs, file)
				assert.ErrorIs(t, err, fs.ErrPermission)
			})
		}
	}
}

func TestSample_LeavesOutAPodItMayNotRead(t *testing.T) {
	dirs := systemd("")
	for _, file := range []string{dirs[3], path.Join(dirs[3], localFile), path.Join(dirs[3], subtreeFile)} {
		t.Run(file, func(t *testing.T) {
			got, err := mayNotRead(t, dirs, file)
			require.NoError(t, err)
			assert.Equal(t, []string{guaranteedUID}, slices.Collect(maps.Keys(got.Pods)))
			assert.Equal(t, map[string]bool{burstableUID: true}, got.Refused)
		})
	}
}

// A container that has the host's view of the tree can make a cgroup named as another pod's, with
// cgroups named as its containers' in it.
func TestSample_LeavesOutAPodWithTwoDirectories(t *testing.T) {
	under := strings.ReplaceAll(burstableUID, "-", "_")
	for name, copied := range map[string]string{
		"of its own class's place": "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + burstableUID + ".slice",
		"of one read before":       "kubepods.slice/kubepods-pod" + under + ".slice",
		"of one read after":        "kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod" + under + ".slice",
		"both":                     "",
	} {
		t.Run(name, func(t *testing.T) {
			dirs := systemd("")
			if copied != "" {
				dirs = append(dirs, copied, copied+"/cri-containerd-"+runnerID+".scope")
			} else {
				dirs = append(dirs, "kubepods.slice/kubepods-pod"+under+".slice", "kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod"+under+".slice")
			}
			tree, err := Open(mount(dirs...), "")
			require.NoError(t, err)
			got, err := tree.Sample()
			require.NoError(t, err)
			assert.Equal(t, []string{guaranteedUID}, slices.Collect(maps.Keys(got.Pods)))
			assert.Equal(t, map[string]bool{burstableUID: true}, got.Refused)
			assert.Equal(t, map[string]bool{burstableUID: true}, got.Doubled)
			_, ok := tree.PodFile(burstableUID)
			assert.False(t, ok)
		})
	}
}

func TestSample_LeavesOutAPodWithADirectoryItMayNotRead(t *testing.T) {
	dirs := append(systemd(""), "kubepods.slice/kubepods-pod"+strings.ReplaceAll(burstableUID, "-", "_")+".slice")
	got, err := mayNotRead(t, dirs, path.Join(dirs[7], subtreeFile))
	require.NoError(t, err)
	assert.Equal(t, []string{guaranteedUID}, slices.Collect(maps.Keys(got.Pods)))
	assert.True(t, got.Refused[burstableUID] && got.Doubled[burstableUID])
}

func TestSample_LeavesOutAContainerItMayNotRead(t *testing.T) {
	dirs := systemd("")
	for _, file := range []string{localFile, groupFile, statFile} {
		t.Run(file, func(t *testing.T) {
			got, err := mayNotRead(t, dirs, path.Join(dirs[4], file))
			require.NoError(t, err)
			assert.Equal(t, []string{sandboxID}, slices.Collect(maps.Keys(got.Pods[burstableUID].Containers)))
			assert.True(t, got.Pods[burstableUID].Nested)
			assert.Equal(t, map[string]bool{runnerID: true}, got.Pods[burstableUID].Refused)
			assert.False(t, got.Pods[guaranteedUID].Nested)
			assert.Empty(t, got.Refused)
		})
	}
}

func TestOpen_RefusesANodeWhosePodsItMayNotRead(t *testing.T) {
	dirs := systemd("")
	for name, tc := range map[string]struct {
		dirs    []string
		refused []string
		wantErr bool
	}{
		"all of them":    {dirs, []string{dirs[3], dirs[6]}, true},
		"one of them":    {dirs, dirs[3:4], false},
		"there are none": {dirs[:3], nil, false},
		// It would not start while a job goes on doing so.
		"all have two": {append(slices.Clone(dirs),
			"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod"+strings.ReplaceAll(guaranteedUID, "-", "_")+".slice",
			"kubepods.slice/kubepods-pod"+strings.ReplaceAll(burstableUID, "-", "_")+".slice"), nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			h := &hookFS{FS: mount(tc.dirs...), open: func(n string) error {
				if slices.Contains(tc.refused, path.Dir(n)) {
					return fs.ErrPermission
				}
				return nil
			}}
			_, err := Open(h, "")
			assert.Equal(t, tc.wantErr, errors.Is(err, fs.ErrPermission), err)
		})
	}
}

func TestSample_TellsOneCgroupFromAnotherInItsPlace(t *testing.T) {
	root, dirs := t.TempDir(), systemd("")
	write := func() {
		for name, f := range mount(dirs...) {
			file := filepath.Join(root, name)
			require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
			require.NoError(t, os.WriteFile(file, f.Data, 0o600))
		}
	}
	write()
	tree, err := Open(os.DirFS(root), "")
	require.NoError(t, err)
	cgroupNow := func() string {
		s, err := tree.Sample()
		require.NoError(t, err)
		return s.Pods[burstableUID].Cgroup
	}
	first := cgroupNow()
	assert.Equal(t, first, cgroupNow())
	// Kept, so its inode is not used again.
	require.NoError(t, os.Rename(filepath.Join(root, dirs[3]), filepath.Join(root, "before")))
	write()
	assert.NotEqual(t, first, cgroupNow())
}

func TestSample_LeavesOutAPodThatGoesAsItIsRead(t *testing.T) {
	dirs := systemd("")
	opened, limit := 0, 100
	h := &hookFS{FS: mount(dirs...), open: func(name string) error {
		if name == dirs[3] {
			if opened++; opened > limit {
				return fs.ErrNotExist
			}
		}
		return nil
	}}
	tree, err := Open(h, "")
	require.NoError(t, err)
	opened, limit = 0, 1
	s, err := tree.Sample()
	require.NoError(t, err)
	assert.Equal(t, 2, opened)
	assert.NotContains(t, s.Pods, burstableUID)
	assert.Contains(t, s.Pods, guaranteedUID)
}

func TestSample_FailsOnAFileItCannotParse(t *testing.T) {
	dirs := systemd("")
	for _, dir := range dirs[3:5] {
		m := mount(dirs...)
		tree, err := Open(m, "")
		require.NoError(t, err)
		m[path.Join(dir, localFile)] = &fstest.MapFile{Data: []byte("oom x\n")}
		_, err = tree.Sample()
		assert.ErrorContains(t, err, "field oom")
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

func TestPodEvents_ReadsAPodOfTheLastSampleAgain(t *testing.T) {
	dirs := systemd("")
	m := mount(dirs...)
	tree, err := Open(m, "")
	require.NoError(t, err)
	m[path.Join(dirs[3], subtreeFile)] = eventsFile(Events{7, 8, 9})
	got, err := tree.PodEvents(burstableUID)
	require.NoError(t, err)
	assert.Equal(t, Events{7, 8, 9}, got)
	m[subtreeFile] = eventsFile(Events{})
	_, err = tree.PodEvents(staticUID)
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestPodFile_IsThatOfAPodOfTheLastSample(t *testing.T) {
	dirs := systemd("")
	m := mount(dirs...)
	tree, err := Open(m, "")
	require.NoError(t, err)
	got, ok := tree.PodFile(burstableUID)
	assert.True(t, ok)
	assert.Equal(t, dirs[3]+"/"+subtreeFile, got)

	for name := range m {
		if strings.HasPrefix(name, dirs[3]) {
			delete(m, name)
		}
	}
	_, err = tree.Sample()
	require.NoError(t, err)
	_, ok = tree.PodFile(burstableUID)
	assert.False(t, ok)
}
