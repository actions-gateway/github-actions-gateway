package rule

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/cgroup"
)

// The kills of Q1316's dogfood runs on GKE (kernel 6.8.0-1064-gke, kubelet v1.35.8-gke.1796000,
// containerd 2.2.7), as an inotify watch and a poll each read them. Each R3 is a filled node:
// three kills at kubepods' limit, of the metrics collector's two containers and the runner.
// R8 is the runner at its own limit. The kernel's lines were read in batches, so they say which
// limits were met and not when.
func TestDecide_NamesNoOtherLimitThanTheKernelOnGKE(t *testing.T) {
	const u, p, c = Unknown, PodsLimit, ContainerLimit
	for run, want := range map[string]struct{ kernel, inotify, poll []Reason }{
		// The collector's containers kill one process each, and are not told apart from a
		// neighbour's kill.
		"b-R3-1-a":  {[]Reason{p, p, p}, []Reason{u, u, p}, []Reason{u, u, p}},
		"b-R3-2-b":  {[]Reason{p, p, p}, []Reason{u, p, p}, []Reason{p, p}},
		"b-R3t-2-a": {[]Reason{p, p, p}, []Reason{u, u, p}, []Reason{u, u, p}},
		"b-R3t-3-a": {[]Reason{p, p, p}, []Reason{u, p}, []Reason{p}},
		"R8-b":      {[]Reason{c}, []Reason{c}, []Reason{c}},
	} {
		for stream, got := range map[string][]Reason{"inotify": want.inotify, "poll": want.poll} {
			t.Run(run+"/"+stream, func(t *testing.T) {
				kernel, answers := replayGKE(t, run, stream)
				assert.Equal(t, want.kernel, kernel)
				assert.Equal(t, got, answers)
			})
		}
	}
}

// replayGKE gives a tracker one stream's readings of a run in order, sampling between those
// apart and four times a second for ten seconds after, and returns the limits its kernel's
// lines name and the reasons of the rises.
func replayGKE(t *testing.T, run, stream string) (kernel, got []Reason) {
	read := func(stream string) []string {
		data, err := os.ReadFile("testdata/gke-6.8.0-1064-" + stream + "-" + run + ".log")
		require.NoError(t, err)
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	lines := read(stream)
	m := fstest.MapFS{"cgroup.controllers": {}}
	appear(m, "kubepods.slice", "kubepods.slice/kubepods-burstable.slice", "kubepods.slice/kubepods-besteffort.slice")
	single := singles(t, append(read("inotify"), read("poll")...))
	tree, err := cgroup.Open(m, "")
	require.NoError(t, err)
	var tr cgroup.Tracker
	var at time.Time
	observe := func() {
		s, err := tree.Sample()
		require.NoError(t, err)
		rises, _ := tr.Observe(s, at)
		for _, uid := range slices.Sorted(maps.Keys(rises)) {
			got = append(got, Decide(rises[uid], false))
		}
	}
	for _, line := range lines {
		f := strings.Fields(line)
		if f[0] == "K" {
			kernel = append(kernel, kernelsReason(line, false))
			continue
		}
		r := readingOf(t, f)
		if !at.IsZero() && r.at.Sub(at) > time.Millisecond {
			observe()
		}
		at = r.at
		r.write(m, false)
		if r.kind == "appeared" && single[r.dir] {
			m[r.dir+"/memory.oom.group"] = &fstest.MapFile{Data: []byte("0\n")}
		}
	}
	for range 40 {
		observe()
		at = at.Add(250 * time.Millisecond)
	}
	return kernel, got
}

// singles is the containers that killed a process and never the group, whose memory.oom.group
// was therefore 0. The runs did not read it, but on the first node read it was 0 on every
// container started before containerd was restarted, and 1 on each after.
func singles(t *testing.T, lines []string) map[string]bool {
	last := map[string]reading{}
	for _, line := range lines {
		if f := strings.Fields(line); f[0] == "N" && f[3] == "rose" && strings.HasSuffix(f[4], ".scope") {
			if r := readingOf(t, f); r.at.After(last[r.dir].at) {
				last[r.dir] = r
			}
		}
	}
	single := map[string]bool{}
	for dir, r := range last {
		single[dir] = strings.HasSuffix(r.own, " oom_group_kill 0") && !strings.Contains(r.own, " oom_kill 0 ")
	}
	return single
}
