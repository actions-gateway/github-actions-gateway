package names_test

import (
	"os"
	"regexp"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/names"
)

// dockerfileFromRE captures the tag and digest from the worker Dockerfile's
// runner FROM line, e.g.:
//
//	FROM ghcr.io/actions/actions-runner:2.335.1@sha256:08c30b…
var dockerfileFromRE = regexp.MustCompile(
	`(?m)^FROM\s+ghcr\.io/actions/actions-runner:(\S+?)@(sha256:[0-9a-f]{64})`)

// TestRunnerVersionLockstep is the drift guard for the runner-version pin
// (Q118). The runner version lives in three places that must move together:
//
//   - cmd/worker/Dockerfile FROM tag — the runner binary the worker pod runs.
//   - names.RunnerVersion / names.WorkerImageDigest — the single source of
//     truth that drives the AGC's DefaultWorkerImage and the GMC's injected
//     GITHUB_RUNNER_VERSION (the agent.version GitHub validates at session
//     creation).
//
// Dependabot bumps the Dockerfile but never the Go constants, so without this
// test a bump silently desyncs the registered runnerVersion from the worker
// image — exactly the drift #197 introduced. Failing CI here forces the
// constants to be updated in the same change.
func TestRunnerVersionLockstep(t *testing.T) {
	// cmd/agc/names → cmd/worker/Dockerfile
	const dockerfilePath = "../../worker/Dockerfile"

	raw, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read worker Dockerfile %q: %v", dockerfilePath, err)
	}

	m := dockerfileFromRE.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf("could not find a pinned actions-runner FROM line in %q; "+
			"if the Dockerfile format changed, update dockerfileFromRE", dockerfilePath)
	}
	gotTag, gotDigest := m[1], m[2]

	if gotTag != names.RunnerVersion {
		t.Errorf("runner version drift: Dockerfile FROM tag %q != names.RunnerVersion %q; "+
			"update RunnerVersion in cmd/agc/names/names.go (see the bump procedure in cmd/worker/Dockerfile)",
			gotTag, names.RunnerVersion)
	}
	if gotDigest != names.WorkerImageDigest {
		t.Errorf("runner digest drift: Dockerfile FROM digest %q != names.WorkerImageDigest %q; "+
			"update WorkerImageDigest in cmd/agc/names/names.go",
			gotDigest, names.WorkerImageDigest)
	}

	// DefaultWorkerImage must reassemble to exactly the Dockerfile FROM ref.
	wantImage := "ghcr.io/actions/actions-runner:" + gotTag + "@" + gotDigest
	if names.DefaultWorkerImage != wantImage {
		t.Errorf("DefaultWorkerImage %q != Dockerfile FROM image %q",
			names.DefaultWorkerImage, wantImage)
	}
}

// TestRunnerVersionNonEmpty guards the empty-agent.version defect (Q118): the
// GMC injects names.RunnerVersion as GITHUB_RUNNER_VERSION, which the AGC sends
// as agent.version on CreateSession. An empty value risks GitHub rejecting the
// session (the runner-version contract Q71 gated).
func TestRunnerVersionNonEmpty(t *testing.T) {
	if names.RunnerVersion == "" {
		t.Fatal("names.RunnerVersion must not be empty: CreateSession would send an empty agent.version")
	}
}
