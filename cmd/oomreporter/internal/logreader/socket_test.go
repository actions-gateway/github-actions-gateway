package logreader

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/cgroup"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/report"
)

// socketPath is short: a Unix socket's path has at most 104 bytes on macOS, and t.TempDir's
// can be longer.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "oom")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

type got struct {
	mu    sync.Mutex
	lines []report.Line
}

func (g *got) add(l report.Line) { g.mu.Lock(); g.lines = append(g.lines, l); g.mu.Unlock() }
func (g *got) len() int          { g.mu.Lock(); defer g.mu.Unlock(); return len(g.lines) }

func serve(t *testing.T, path string, g *got) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, path, g.add, slog.New(slog.DiscardHandler)) }()
	t.Cleanup(func() { cancel(); assert.NoError(t, <-done) })
	require.Eventually(t, func() bool {
		info, err := os.Stat(path)
		return err == nil && info.Mode()&os.ModeSocket != 0
	}, 5*time.Second, 10*time.Millisecond)
}

func TestSocket_HandsOverEachLine(t *testing.T) {
	path := socketPath(t)
	// One left by an agent that did not stop cleanly is replaced.
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	var g got
	serve(t, path, &g)
	s := &Sender{Path: path}
	want := report.Line{Pod: uid, Container: runner, Level: cgroup.PodLevel, At: 5}
	require.NoError(t, s.Send(want))
	require.NoError(t, s.Send(want))
	require.Eventually(t, func() bool { return g.len() == 2 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, want, g.lines[0])
}

func TestSocket_ClosesAConnectionThatSendsNoLine(t *testing.T) {
	path := socketPath(t)
	var g got
	serve(t, path, &g)
	conn, err := net.Dial("unix", path)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.Write(make([]byte, report.LineSize))
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = conn.Read(make([]byte, 1))
	assert.Error(t, err, "closed")
	assert.Zero(t, g.len())
}

func TestSender_RefusesALineItCannotSend(t *testing.T) {
	s := &Sender{Path: socketPath(t)}
	assert.Error(t, s.Send(report.Line{Pod: uid}), "no container")
	assert.Error(t, s.Send(report.Line{Pod: uid, Container: runner, Level: cgroup.InPod}), "no agent")
}

func TestServe_RefusesAPathItCannotClear(t *testing.T) {
	path := socketPath(t)
	require.NoError(t, os.Mkdir(path, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(path, "f"), nil, 0o600))
	assert.Error(t, Serve(t.Context(), path, func(report.Line) {}, slog.New(slog.DiscardHandler)))
}

func TestSender_DialsAgainAfterTheAgentGoes(t *testing.T) {
	path := socketPath(t)
	var g got
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, path, g.add, slog.New(slog.DiscardHandler)) }()
	require.Eventually(t, func() bool { _, err := os.Stat(path); return err == nil }, 5*time.Second, 10*time.Millisecond)
	s := &Sender{Path: path}
	l := report.Line{Pod: uid, Container: runner, Level: cgroup.InPod, At: 1}
	require.NoError(t, s.Send(l))
	cancel()
	require.NoError(t, <-done)
	// A write to a connection the agent closed fails at the latest on the second.
	err := s.Send(l)
	if err == nil {
		err = s.Send(l)
	}
	assert.Error(t, err)
	assert.Nil(t, s.conn, "dialled afresh next time")
}
