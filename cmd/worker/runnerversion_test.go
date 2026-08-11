package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeDeps lays out a runner home containing only the dependency manifest, at the
// path the real actions/runner layout puts it.
func writeDeps(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, "bin"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(home, runnerDepsFile), []byte(body), 0o600))
	return home
}

// realDepsShape mirrors the structure of bin/Runner.Listener.deps.json as shipped in
// actions-runner-linux-x64-2.335.1.tar.gz: the root library key carries the version,
// alongside the third-party libraries it must not be confused with.
const realDepsShape = `{
  "runtimeTarget": {"name": ".NETCoreApp,Version=v8.0/linux-x64"},
  "targets": {
    ".NETCoreApp,Version=v8.0": {},
    ".NETCoreApp,Version=v8.0/linux-x64": {
      "Runner.Listener/2.335.1": {"dependencies": {"Newtonsoft.Json": "13.0.3"}},
      "Newtonsoft.Json/13.0.3": {},
      "Runner.Common/1.0.0": {}
    }
  }
}`

func TestDetectRunnerVersion(t *testing.T) {
	got, err := detectRunnerVersion(writeDeps(t, realDepsShape))

	require.NoError(t, err)
	assert.Equal(t, "2.335.1", got)
}

func TestDetectRunnerVersionErrors(t *testing.T) {
	t.Run("missing manifest", func(t *testing.T) {
		_, err := detectRunnerVersion(t.TempDir())
		require.Error(t, err)
	})

	t.Run("malformed json", func(t *testing.T) {
		_, err := detectRunnerVersion(writeDeps(t, `{"targets": `))
		require.ErrorContains(t, err, "parse")
	})

	// A tenant image built from something that is not actions/runner: the manifest
	// exists but names no runner. Reporting a version here would be a fabrication.
	t.Run("no runner library", func(t *testing.T) {
		_, err := detectRunnerVersion(writeDeps(t, `{"targets": {"net8.0": {"Newtonsoft.Json/13.0.3": {}}}}`))
		require.ErrorContains(t, err, "Runner.Listener/")
	})

	// Map iteration order is random, so a disagreement must be an error rather than
	// whichever entry came out first — otherwise the reported version flaps per run.
	t.Run("disagreeing versions", func(t *testing.T) {
		_, err := detectRunnerVersion(writeDeps(t, `{"targets": {
			"a": {"Runner.Listener/2.335.1": {}},
			"b": {"Runner.Listener/2.320.0": {}}
		}}`))
		require.ErrorContains(t, err, "different runner versions")
	})
}

// TestLogRunnerVersionNeverFails pins the contract that matters at runtime: detection
// is diagnostic, so nothing about it may cost a job.
func TestLogRunnerVersionNeverFails(t *testing.T) {
	assert.NotPanics(t, func() {
		logRunnerVersion(t.TempDir())
		logRunnerVersion(writeDeps(t, realDepsShape))
	})
}
