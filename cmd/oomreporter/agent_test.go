package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/report"
)

const podUID = "1b4e28ba-2fa1-11d2-883f-0016d3cca427"

var runnerID = strings.Repeat("a", 64)

// mount is a node's cgroup mount with one pod of one container, and what to write its counts with.
func mount(t *testing.T) (dir string, counts func(pod, own string)) {
	t.Helper()
	dir = t.TempDir()
	write := func(name, data string) {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
	}
	const none = "oom 0\noom_kill 0\noom_group_kill 0\n"
	write("cgroup.controllers", "memory\n")
	pod := "kubepods/burstable/pod" + podUID
	for _, cg := range []string{"kubepods", "kubepods/burstable", "kubepods/besteffort", pod, pod + "/" + runnerID} {
		write(cg+"/memory.events", none)
		write(cg+"/memory.events.local", none)
	}
	write(pod+"/"+runnerID+"/memory.oom.group", "1\n")
	write(pod+"/"+runnerID+"/cgroup.stat", "nr_descendants 0\nnr_dying_descendants 0\n")
	return dir, func(podCounts, own string) {
		write(pod+"/"+runnerID+"/memory.events.local", own)
		write(pod+"/memory.events", podCounts)
	}
}

func info(t *testing.T, options string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mountinfo")
	require.NoError(t, os.WriteFile(path, fmt.Appendf(nil, "30 23 0:26 / /sys/fs/cgroup rw - cgroup2 cgroup2 %s\n", options), 0o600))
	return path
}

type request struct {
	method, path string
	body         corev1.ConfigMap
}

// server answers status to the first failing requests, and returns what it was sent.
func server(t *testing.T, failing int) (kubernetes.Interface, <-chan request) {
	t.Helper()
	got := make(chan request, 16)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		req := request{method: r.Method, path: r.URL.Path}
		if err == nil {
			err = json.Unmarshal(data, &req.body)
		}
		assert.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		if failing--; failing >= 0 {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`)
		} else {
			_, _ = w.Write(data)
		}
		got <- req
	}))
	t.Cleanup(s.Close)
	kube, err := kubernetes.NewForConfig(&rest.Config{Host: s.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"}})
	require.NoError(t, err)
	return kube, got
}

func TestAgent_ReportsAKillAgainUntilTheReportIsThere(t *testing.T) {
	dir, counts := mount(t)
	kube, got := server(t, 1)
	o := agentOptions{node: "node-a", namespace: "oom", mount: dir, poll: 10 * time.Millisecond, mountinfo: info(t, "rw,nsdelegate")}
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- o.run(ctx, kube, noLog) }()
	time.Sleep(100 * time.Millisecond)
	counts("oom 1\noom_kill 1\noom_group_kill 1\n", "oom 1\noom_kill 1\noom_group_kill 1\n")
	for range 2 {
		select {
		case req := <-got:
			assert.Equal(t, http.MethodPut, req.method)
			assert.Equal(t, "/api/v1/namespaces/oom/configmaps/oom-report-node-a", req.path)
			assert.Equal(t, "oom-report-node-a", req.body.Name)
			assert.Empty(t, req.body.ResourceVersion, "it may not read the one there")
			kills, err := report.Decode(req.body.Data[report.Key])
			require.NoError(t, err)
			require.Len(t, kills, 1)
			assert.Equal(t, podUID, kills[0].Pod)
			require.NotNil(t, kills[0].Rise)
			assert.Equal(t, []string{runnerID}, kills[0].Rise.Containers)
		case <-time.After(20 * time.Second):
			t.Fatal("no report")
		}
	}
	stop()
	require.NoError(t, <-done)
}

func TestAgent_RefusesANodeItCannotCountOn(t *testing.T) {
	dir, _ := mount(t)
	for name, tc := range map[string]struct {
		o    agentOptions
		want string
	}{
		"with local events":     {agentOptions{mount: dir, mountinfo: info(t, "rw,memory_localevents")}, "memory_localevents"},
		"with no cgroups":       {agentOptions{mount: t.TempDir(), mountinfo: info(t, "rw")}, "not a cgroup v2 mount"},
		"with no mount info":    {agentOptions{mount: dir, mountinfo: filepath.Join(dir, "none")}, "none"},
		"with no such root":     {agentOptions{mount: dir, root: "kubepods.slice", mountinfo: info(t, "rw")}, "pods' root kubepods.slice"},
		"with another's a root": {agentOptions{mount: dir, root: "system.slice", mountinfo: info(t, "rw")}, "not named as"},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, tc.o.run(t.Context(), nil, noLog), tc.want)
		})
	}
}

func TestAgent_StopsWhereTheCgroupsGo(t *testing.T) {
	dir, _ := mount(t)
	kube, _ := server(t, 0)
	o := agentOptions{node: "node-a", namespace: "oom", mount: dir, poll: 5 * time.Millisecond, mountinfo: info(t, "rw")}
	done := make(chan error)
	go func() { done <- o.run(t.Context(), kube, noLog) }()
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "kubepods")))
	select {
	case err := <-done:
		require.ErrorIs(t, err, os.ErrNotExist)
	case <-time.After(20 * time.Second):
		t.Fatal("it goes on")
	}
}
