package agent

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/cgroup"
)

// Watcher sends the UID of each pod whose counts change, as the kernel tells of it. What it
// misses is left to the poll.
type Watcher struct {
	// Changed is where it sends.
	Changed <-chan string

	// dir is the cgroup mount, and file a pod's file below it.
	dir     string
	file    func(uid string) (string, bool)
	fs      *fsnotify.Watcher
	log     *slog.Logger
	changed chan<- string

	mu    sync.Mutex
	uids  map[string]string // by path
	paths map[string]string // by UID
	full  bool
}

// NewWatcher is a Watcher of the files file names below dir.
func NewWatcher(dir string, file func(uid string) (string, bool), log *slog.Logger) (*Watcher, error) {
	fs, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	changed := make(chan string, 64)
	return &Watcher{
		Changed: changed, changed: changed, dir: dir, file: file, fs: fs, log: log,
		uids: map[string]string{}, paths: map[string]string{},
	}, nil
}

// Follow watches the pods of s, and no other.
func (w *Watcher) Follow(s cgroup.Sample) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for uid, path := range w.paths {
		if _, ok := s.Pods[uid]; !ok {
			// A cgroup's removal sends no event and leaves its watch (kernel 6.12.76).
			_ = w.fs.Remove(path)
			delete(w.paths, uid)
			delete(w.uids, path)
		}
	}
	for uid := range s.Pods {
		name, ok := w.file(uid)
		if _, has := w.paths[uid]; has || !ok {
			continue
		}
		path := filepath.Join(w.dir, name)
		if err := w.fs.Add(path); err != nil {
			if !w.full && !errors.Is(err, fsnotify.ErrClosed) {
				w.log.Warn("cannot watch a pod; its kills are seen by the poll", "pod", uid, "error", err)
			}
			w.full = true
			continue
		}
		w.paths[uid], w.uids[path] = path, uid
	}
}

// Run sends until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	defer func() { _ = w.fs.Close() }()
	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-w.fs.Errors:
			if !ok {
				return
			}
			w.log.Warn("watching", "error", err)
		case ev, ok := <-w.fs.Events:
			if !ok {
				return
			}
			w.mu.Lock()
			uid, known := w.uids[ev.Name]
			w.mu.Unlock()
			if !known {
				continue
			}
			select {
			case w.changed <- uid:
			default:
			}
		}
	}
}
