package agent

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/cgroup"
)

func following(uids ...string) cgroup.Sample {
	s := cgroup.Sample{Pods: map[string]cgroup.Pod{}}
	for _, id := range uids {
		s.Pods[id] = cgroup.Pod{}
	}
	return s
}

func watching(t *testing.T) (w *Watcher, dir string, logged *bytes.Buffer) {
	t.Helper()
	dir, logged = t.TempDir(), &bytes.Buffer{}
	for _, id := range []string{uid, other} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, id), nil, 0o600))
	}
	w, err := NewWatcher(dir, func(id string) (string, bool) { return id, true }, slog.New(slog.NewTextHandler(logged, nil)))
	require.NoError(t, err)
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	t.Cleanup(func() { stop(); <-done })
	return w, dir, logged
}

func change1(t *testing.T, dir, name string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("oom 1\n"), 0o600))
}

func TestWatcher_SendsThePodWhoseFileChanges(t *testing.T) {
	w, dir, _ := watching(t)
	w.Follow(following(uid, other))
	change1(t, dir, other)
	select {
	case got := <-w.Changed:
		assert.Equal(t, other, got)
	case <-time.After(10 * time.Second):
		t.Fatal("nothing sent")
	}
}

func TestWatcher_LeavesAPodThatIsGone(t *testing.T) {
	w, dir, _ := watching(t)
	w.Follow(following(uid, other))
	w.Follow(following(uid))
	change1(t, dir, other)
	change1(t, dir, uid)
	select {
	case got := <-w.Changed:
		assert.Equal(t, uid, got)
	case <-time.After(10 * time.Second):
		t.Fatal("nothing sent")
	}
	assert.NotContains(t, w.paths, other)
	assert.Len(t, w.uids, 1)

	w.Follow(following(uid, other))
	assert.Contains(t, w.paths, other)
}

func TestWatcher_GoesOnAfterAnErrorAndEndsWhereItIsClosed(t *testing.T) {
	dir, logged := t.TempDir(), &bytes.Buffer{}
	require.NoError(t, os.WriteFile(filepath.Join(dir, uid), nil, 0o600))
	w, err := NewWatcher(dir, func(id string) (string, bool) { return id, true }, slog.New(slog.NewTextHandler(logged, nil)))
	require.NoError(t, err)
	w.Follow(following(uid))
	done := make(chan struct{})
	go func() { defer close(done); w.Run(t.Context()) }()
	w.fs.Errors <- fsnotify.ErrEventOverflow
	change1(t, dir, uid)
	assert.Equal(t, uid, <-w.Changed)
	assert.Contains(t, logged.String(), "overflow")
	require.NoError(t, w.fs.Close())
	<-done
}

func TestWatcher_SaysOnceThatItCannotWatch(t *testing.T) {
	w, dir, logged := watching(t)
	require.NoError(t, os.Remove(filepath.Join(dir, other)))
	for range 3 {
		w.Follow(following(uid, other))
	}
	assert.Equal(t, 1, strings.Count(logged.String(), "cannot watch"))
	assert.Contains(t, w.paths, uid)
	assert.NotContains(t, w.paths, other)
}
