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
)

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

// logRunnerVersion records the runner version this image ships, once per worker pod.
// It is the runtime half of the Q715 signal: the AGC judges the image *reference*
// against GitHub's enforced minimum and reports Unknown when the reference declares
// no version, and this is where an operator reads the real answer for that case.
// Never fatal — a version we could not read must not cost a job.
func logRunnerVersion(runnerHome string) {
	version, err := detectRunnerVersion(runnerHome)
	if err != nil {
		slog.Warn("runner version not detected: this image's runner version cannot be checked against GitHub's enforced minimum",
			"error", err)
		return
	}
	slog.Info("runner version detected", "version", version)
}
