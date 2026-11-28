package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNest_LeavesItsCgroupAndNamesTheRest(t *testing.T) {
	cgroupRoot = t.TempDir()
	require.NoError(t, nest([]string{"x,task_memcg=", "kubepods.slice"}))
	read := func(path ...string) string {
		data, err := os.ReadFile(filepath.Join(append([]string{cgroupRoot}, path...)...))
		require.NoError(t, err)
		return string(data)
	}
	pid := strconv.Itoa(os.Getpid())
	assert.Equal(t, pid, read("hog", "cgroup.procs"), "out of its own cgroup first")
	assert.Equal(t, "+memory", read("cgroup.subtree_control"))
	assert.Equal(t, "+memory", read("x,task_memcg=", "cgroup.subtree_control"))
	leaf := []string{"x,task_memcg=", "kubepods.slice"}
	assert.Equal(t, strconv.Itoa(32<<20), read(append(leaf, "memory.max")...))
	assert.Equal(t, "1", read(append(leaf, "memory.oom.group")...))
	assert.Equal(t, pid, read(append(leaf, "cgroup.procs")...))

	assert.Error(t, nest([]string{"again"}), "a second hog cgroup")
}

func TestParse(t *testing.T) {
	mib, err := parse([]string{"256", "x"})
	require.NoError(t, err)
	assert.Equal(t, 256, mib)
	for _, args := range [][]string{nil, {"0"}, {"many"}} {
		_, err := parse(args)
		assert.Error(t, err, "%q", args)
	}
}
