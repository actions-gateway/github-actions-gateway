package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStorageVersionOptions(t *testing.T) {
	opts, err := parseStorageVersionOptions(nil, &bytes.Buffer{})
	require.NoError(t, err)
	assert.False(t, opts.apply, "dry run is the default")

	opts, err = parseStorageVersionOptions([]string{"--context", "kind-gag", "--apply", "-y"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, storageVersionOptions{apply: true, kubeContext: "kind-gag", assumeYes: true}, opts)

	t.Setenv("ASSUME_YES", "1")
	opts, err = parseStorageVersionOptions([]string{"--apply"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.True(t, opts.assumeYes)
}

func TestParseStorageVersionOptions_RejectsFanOutFlags(t *testing.T) {
	var stderr bytes.Buffer
	_, err := parseStorageVersionOptions([]string{"--namespace", "team-a"}, &stderr)
	require.Error(t, err, "the sweep is cluster-wide; a namespace flag must not be silently ignored")
	_, err = parseStorageVersionOptions([]string{"team-a"}, &stderr)
	require.Error(t, err)
}

// TestRun_DispatchesStorageVersion proves the subcommand reaches its own flag
// parsing rather than the bare command's usage error.
func TestRun_DispatchesStorageVersion(t *testing.T) {
	var stderr bytes.Buffer
	err := run([]string{storageVersionCommand, "-h"}, nil, &bytes.Buffer{}, &stderr)
	require.Error(t, err)
	assert.Contains(t, stderr.String(), "gag-migrate storage-version")
	assert.NotContains(t, stderr.String(), "1.x gag-migrate")
}
