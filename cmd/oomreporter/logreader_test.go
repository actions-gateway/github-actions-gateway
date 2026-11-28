package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/logreader"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/report"
)

func TestLogReader_StopsWhereTheLogIsRefused(t *testing.T) {
	refused := errors.New("operation not permitted")
	o := logReaderOptions{socket: "/s", root: "kubepods.slice", every: time.Second}
	err := o.run(t.Context(), func() (string, error) { return "", refused }, logreader.Now, noLog)
	assert.ErrorIs(t, err, refused)
}

func TestLogReader_HandsTheAgentEachKill(t *testing.T) {
	dir, err := os.MkdirTemp("", "oom")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")

	var mu sync.Mutex
	var got []report.Line
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- logreader.Serve(ctx, socket, func(l report.Line) { mu.Lock(); got = append(got, l); mu.Unlock() }, noLog)
	}()

	const pod = "/kubepods.slice/kubepods-pod1b4e28ba_2fa1_11d2_883f_0016d3cca427.slice"
	scope := pod + "/cri-containerd-" + strings.Repeat("a", 64) + ".scope"
	log := "<6>[5.000000] oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=x,mems_allowed=0,oom_memcg=" + pod +
		",task_memcg=" + scope + ",task=hog,pid=7,uid=0\n<6>[5.000000] Tasks in " + scope + " are going to be killed due to memory.oom.group set\n"
	o := logReaderOptions{socket: socket, root: "kubepods.slice", every: 10 * time.Millisecond}
	ran := make(chan error, 1)
	go func() {
		ran <- o.run(ctx, func() (string, error) { return log, nil }, func() (time.Time, time.Duration) { return time.UnixMilli(10_000), 10 * time.Second }, noLog)
	}()
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) > 0 }, 10*time.Second, 10*time.Millisecond)
	cancel()
	require.NoError(t, <-ran)
	require.NoError(t, <-served)
	assert.Equal(t, report.Line{Pod: "1b4e28ba-2fa1-11d2-883f-0016d3cca427", Container: strings.Repeat("a", 64), Level: 2, At: 5_000}, got[0])
}
