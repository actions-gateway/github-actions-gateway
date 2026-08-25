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

// TestReportRunnerVersionNeverFails pins the contract that matters at runtime:
// detection and hand-back are diagnostic, so nothing about either may cost a job.
// The unwritable path is the case the wrapper actually meets in the field — a pod
// spec naming a directory the runner user cannot write.
func TestReportRunnerVersionNeverFails(t *testing.T) {
	assert.NotPanics(t, func() {
		reportRunnerVersion(t.TempDir())
		reportRunnerVersion(writeDeps(t, realDepsShape))

		t.Setenv(terminationLogEnv, filepath.Join(t.TempDir(), "no-such-dir", "termination-log"))
		reportRunnerVersion(writeDeps(t, realDepsShape))
	})
}

// TestReportRunnerVersionWritesTheReport is the Q792 hand-back: the version reaches
// the termination message as JSON the AGC can parse, not as a bare string.
func TestReportRunnerVersionWritesTheReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "termination-log")
	t.Setenv(terminationLogEnv, path)

	reportRunnerVersion(writeDeps(t, realDepsShape))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	// Pin the LITERAL key, not a round-trip through this package's own struct. The
	// AGC parses these bytes with its own copy of the type (the two are deliberately
	// not shared, so a mismatch surfaces as an unset field rather than a build
	// error), which makes this assertion the only thing holding the wire contract.
	assert.JSONEq(t, `{"runnerVersion":"2.335.1"}`, string(raw))
}

// TestWriteWorkerReportOptsOutWithoutThePath keeps the wrapper runnable outside a
// GAG-provisioned pod: no path, no write, no error. An older AGC that does not set the
// variable is the same case. Named for writeWorkerReport because that is what it
// exercises; the env-var plumbing that reaches it is covered by
// TestReportRunnerVersionWritesTheReport.
func TestWriteWorkerReportOptsOutWithoutThePath(t *testing.T) {
	// Asserted on the RETURN rather than on the filesystem. With no path there is no
	// file anywhere to look for, so watching a directory cannot tell "declined to
	// write" from "wrote somewhere this test does not know about" — two earlier
	// versions of this test made exactly that mistake and passed with the opt-out
	// fully broken.
	assert.False(t, writeWorkerReport("", workerReport{RunnerVersion: "2.335.1"}),
		"no path configured must mean no report is written")

	// The positive case, so the assertion above is a discrimination rather than a
	// function that always reports false.
	path := filepath.Join(t.TempDir(), "termination-log")
	assert.True(t, writeWorkerReport(path, workerReport{RunnerVersion: "2.335.1"}),
		"a configured path must be written")
}

// TestReportRunnerVersionWritesNothingWhenUndetected pins the failure direction: a
// version the wrapper could not read must leave NO report, so the AGC reads absence
// rather than an empty string it might render as a version.
func TestReportRunnerVersionWritesNothingWhenUndetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "termination-log")
	t.Setenv(terminationLogEnv, path)

	reportRunnerVersion(t.TempDir()) // no deps.json: detection fails

	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), "an undetected version must write no report at all")
}
