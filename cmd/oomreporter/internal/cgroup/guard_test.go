package cgroup

import (
	"maps"
	"path"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// watched is a tree whose samples a Guard and a Tracker take, as the agent does.
type watched struct {
	t     *testing.T
	fs    fstest.MapFS
	tree  *Tree
	guard Guard
	tr    Tracker
	dirs  []string
	copy  string
}

func newWatched(t *testing.T) *watched {
	t.Helper()
	dirs := systemd("")
	w := &watched{t: t, dirs: dirs, fs: mount(dirs...), copy: "kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod" + strings.ReplaceAll(burstableUID, "-", "_") + ".slice"}
	var err error
	w.tree, err = Open(w.fs, "")
	require.NoError(t, err)
	return w
}

// at samples until a kill that shows has been returned, and returns it.
func (w *watched) at(after time.Duration) (r Rise) {
	w.t.Helper()
	for range 2 {
		s, err := w.tree.Sample()
		require.NoError(w.t, err)
		s = w.guard.Filter(s, start.Add(after))
		rises, _ := w.tr.Observe(s, start.Add(after))
		if got, ok := rises[burstableUID]; ok {
			r = got
		}
	}
	return r
}

// put has the pod's cgroup at dir, with a container's in it, have counted n of each.
func (w *watched) put(dir string, n uint64) {
	for _, d := range []string{dir, path.Join(dir, "cri-containerd-"+runnerID+".scope")} {
		maps.Copy(w.fs, mount(d))
		w.fs[path.Join(d, subtreeFile)], w.fs[path.Join(d, localFile)] = eventsFile(Events{n, n, n}), eventsFile(Events{n, n, n})
	}
}

func (w *watched) remove(dir string) {
	maps.DeleteFunc(w.fs, func(name string, _ *fstest.MapFile) bool { return strings.HasPrefix(name, dir+"/") })
}

func TestGuard_AKillOfAPodItTrustsIsJudged(t *testing.T) {
	w := newWatched(t)
	w.at(0)
	w.put(w.dirs[3], 9)
	assert.NotZero(t, w.at(time.Second).GroupKills)
}

func TestGuard_NoCopyOfAPodIsJudgedInItsPlace(t *testing.T) {
	for name, tc := range map[string]struct{ steps func(w *watched) }{
		"made beside it": {func(w *watched) {
			w.put(w.copy, 90)
			w.at(time.Second)
			w.put(w.copy, 95)
		}},
		"made beside it, and left when it goes": {func(w *watched) {
			w.put(w.copy, 90)
			w.at(time.Second)
			w.put(w.dirs[3], 9) // The true kill.
			w.at(1500 * time.Millisecond)
			w.remove(w.dirs[3])
			w.at(2 * time.Second)
			w.put(w.copy, 95)
		}},
		"left for long when it goes": {func(w *watched) {
			w.put(w.copy, 90)
			w.at(time.Second)
			w.remove(w.dirs[3])
			w.at(2 * time.Second)
			w.at(time.Hour)
			w.put(w.copy, 95)
		}},
		"made beside it before it was first seen": {func(w *watched) {
			w.guard, w.tr = Guard{}, Tracker{}
			w.put(w.copy, 90)
			w.at(time.Second)
			w.remove(w.dirs[3])
			w.at(2 * time.Second)
			w.put(w.copy, 95)
		}},
		"made and gone again, when the pod may be either": {func(w *watched) {
			w.put(w.copy, 90)
			w.at(time.Second)
			w.remove(w.copy)
			w.at(2 * time.Second)
			w.put(w.dirs[3], 9)
		}},
		"made after it goes": {func(w *watched) {
			w.remove(w.dirs[3])
			w.at(time.Second)
			w.put(w.copy, 90)
			w.at(2 * time.Second)
			w.put(w.copy, 95)
		}},
		"made after it goes, in its place": {func(w *watched) {
			w.remove(w.dirs[3])
			w.at(time.Second)
			w.put(w.dirs[3], 90)
			w.at(2 * time.Second)
			w.put(w.dirs[3], 95)
		}},
		"made after it goes, once and again": {func(w *watched) {
			w.remove(w.dirs[3])
			w.at(time.Second)
			w.put(w.copy, 90)
			w.at(distrust)
			w.remove(w.copy)
			w.at(distrust + 500*time.Millisecond)
			w.put(w.copy, 90)
			w.at(distrust + 2*time.Second)
			w.put(w.copy, 95)
		}},
		"made as it goes, between two samples": {func(w *watched) {
			w.remove(w.dirs[3])
			w.put(w.copy, 90)
			w.at(time.Second)
			w.put(w.copy, 95)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWatched(t)
			w.at(0)
			tc.steps(w)
			for _, after := range []time.Duration{0, time.Second, 3 * time.Second} {
				assert.Zero(t, w.at(distrust-time.Second+after).GroupKills, after)
			}
		})
	}
}

// Long after, the bound on how far a kill is from a container's end holds.
func TestGuard_ForgetsAPodThatIsLongGone(t *testing.T) {
	w := newWatched(t)
	w.at(0)
	w.remove(w.dirs[3])
	w.at(time.Second)
	w.at(time.Second + distrust - 1)
	assert.Contains(t, w.guard.gone, burstableUID)
	w.put(w.dirs[3], 90)
	assert.NotZero(t, w.at(time.Second+distrust).GroupKills)
	assert.Empty(t, w.guard.gone)
}

func TestGuard_KeepsNothingOfWhatIsGone(t *testing.T) {
	w := newWatched(t)
	w.put(w.copy, 90)
	w.at(0)
	w.remove(w.copy)
	w.remove(w.dirs[3])
	w.at(time.Second)
	w.at(time.Second + distrust)
	assert.Empty(t, w.guard.doubtful)
	assert.Empty(t, w.guard.gone)
	assert.Equal(t, []string{guaranteedUID}, slices.Collect(maps.Keys(w.guard.seen)))
}

// One that may not be read for a while is the same one after.
func TestGuard_TrustsAPodThatCouldNotBeRead(t *testing.T) {
	var g Guard
	pod := Sample{Pods: map[string]Pod{burstableUID: {Cgroup: "a"}}}
	shut := Sample{Refused: map[string]bool{burstableUID: true}}
	other := Sample{Pods: map[string]Pod{burstableUID: {Cgroup: "b"}}}
	assert.Len(t, g.Filter(pod, start).Pods, 1)
	assert.Empty(t, g.Filter(shut, start).Pods)
	assert.Len(t, g.Filter(pod, start).Pods, 1)
	g.Filter(shut, start)
	assert.Empty(t, g.Filter(other, start).Pods, "or not")
	var late Guard
	late.Filter(shut, start)
	assert.Len(t, late.Filter(pod, start).Pods, 1)
}
