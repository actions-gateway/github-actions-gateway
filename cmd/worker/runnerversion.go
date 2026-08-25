package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const (
	// runnerDepsFile is the .NET dependency manifest actions/runner ships beside its
	// binaries. Its root library key carries the runner's own version — the only
	// version record in the release tarball, which has no version file and whose
	// image carries neither a RUNNER_VERSION env var nor a version label (measured
	// against ghcr.io/actions/actions-runner:2.335.1 on 2026-08-11).
	runnerDepsFile = "bin/Runner.Listener.deps.json"

	// runnerListenerLib is the deps.json library key prefix, "Runner.Listener/<version>".
	runnerListenerLib = "Runner.Listener/"

	// maxDepsFileBytes caps the manifest read. It is ~110 KB at 2.335.1; the cap
	// exists so a wrapper injected into an arbitrary tenant image cannot be made to
	// buffer something huge under this name.
	maxDepsFileBytes = 4 << 20

	// terminationLogEnv names the file kubelet reads back as the container's
	// termination message. The AGC sets it to the runner container's
	// terminationMessagePath; empty (an unmanaged pod, or an older AGC) skips the
	// write, so the wrapper stays runnable outside a GAG-provisioned pod.
	terminationLogEnv = "WORKER_TERMINATION_LOG"

	// maxTerminationMessageBytes is kubelet's own cap on the message it reads back.
	// The payload here is ~40 bytes; the guard exists so a future field cannot
	// silently truncate the JSON and leave the AGC parsing a fragment.
	maxTerminationMessageBytes = 4096
)

// workerReport is the wrapper's structured hand-back to the AGC, written to the
// termination message path and read off the pod's terminated container status
// (Q792). JSON rather than a bare version string so a later field does not need a
// second channel or a format migration.
//
// It is a SELF-REPORT, not an attestation. The runner container runs the tenant's
// image and the job's own steps run inside it, so anything in the container can
// rewrite this file before it terminates and kubelet reads whatever is there last.
// It answers "what does this image say it ships" for an operator debugging a custom
// image; it must never carry a security verdict. Q988 is the attestable form.
type workerReport struct {
	// RunnerVersion is the actions/runner version read from the runner's own
	// dependency manifest, which is the only version record in the release tarball.
	RunnerVersion string `json:"runnerVersion"`
}

// detectRunnerVersion reports the actions/runner version the image around the
// wrapper actually ships, read from the runner's own dependency manifest rather
// than from the image tag (Q715). The tag is a claim the AGC can check; this is
// what is installed, which is the only answer that holds for a tenant's custom
// image.
func detectRunnerVersion(runnerHome string) (string, error) {
	path := filepath.Join(runnerHome, runnerDepsFile)
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	var doc struct {
		Targets map[string]map[string]json.RawMessage `json:"targets"`
	}
	if err := json.NewDecoder(io.LimitReader(f, maxDepsFileBytes)).Decode(&doc); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}

	// One entry per target framework, each naming the same root library. Collect the
	// distinct versions rather than taking the first: map iteration order is random,
	// so a disagreement must be reported, never sampled.
	seen := map[string]struct{}{}
	for _, libs := range doc.Targets {
		for lib := range libs {
			if v, ok := strings.CutPrefix(lib, runnerListenerLib); ok && v != "" {
				seen[v] = struct{}{}
			}
		}
	}
	switch len(seen) {
	case 0:
		return "", fmt.Errorf("no %s* library in %s", runnerListenerLib, path)
	case 1:
		for v := range seen {
			return v, nil
		}
	}
	versions := make([]string, 0, len(seen))
	for v := range seen {
		versions = append(versions, v)
	}
	return "", fmt.Errorf("%s names %d different runner versions: %s", path, len(seen), strings.Join(versions, ", "))
}

// reportRunnerVersion records the runner version this image ships, once per worker
// pod: to the log, where an operator can read it directly, and to the termination
// message, where the AGC reads it back onto status (Q715, Q792).
//
// Never fatal — a version we could not read, or could not hand back, must not cost a
// job. Both failures degrade to the log line they replaced.
func reportRunnerVersion(runnerHome string) {
	version, err := detectRunnerVersion(runnerHome)
	if err != nil {
		slog.Warn("runner version not detected: this image's runner version cannot be checked against GitHub's enforced minimum",
			"error", err)
		return
	}
	slog.Info("runner version detected", "version", version)
	writeWorkerReport(os.Getenv(terminationLogEnv), workerReport{RunnerVersion: version})
}

// writeWorkerReport hands the report back to the AGC through the container's
// termination message. An empty path is the ordinary opt-out (see terminationLogEnv)
// and is not an error.
//
// Written once, early, rather than at exit: the wrapper does not own the process for
// the whole of a scale-set worker's life, and a report the AGC never receives is
// worse than one written before the job ran, which is the same instant the version
// was true at.
func writeWorkerReport(path string, report workerReport) {
	if path == "" {
		return
	}
	payload, err := json.Marshal(report)
	if err != nil {
		slog.Warn("runner version not handed back: report could not be encoded", "error", err)
		return
	}
	if len(payload) > maxTerminationMessageBytes {
		slog.Warn("runner version not handed back: report exceeds the termination-message cap",
			"bytes", len(payload), "cap", maxTerminationMessageBytes)
		return
	}
	// 0600: the mode applies only if the file does not already exist (kubelet creates
	// it), and kubelet reads it as root either way, so the narrower mode costs nothing
	// and keeps a stray report out of reach of anything else running as another user
	// in the pod.
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		slog.Warn("runner version not handed back: termination message not written",
			"path", path, "error", err)
	}
}
