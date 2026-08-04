package utils

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2" //nolint:revive
)

// supersededRSDescription is what `kubectl describe replicasets` prints for a
// ReplicaSet a roll has scaled to zero: the pods are gone, the template is not.
const supersededRSDescription = `Name:           ssrec-agc-b8d7d6678
Namespace:      ssrec
Annotations:    deployment.kubernetes.io/revision: 1
Replicas:       0 current / 0 desired
Pod Template:
  Containers:
   agc:
    Image:  ghcr.io/example/agc:v0.9.0
    Environment:
      GITHUB_APP_PRIVATE_KEY:  <set to the key 'key' in secret 'agc-app'>
`

// fakeKubectl puts a kubectl on PATH that answers `describe replicasets` with
// fixture and echoes its arguments for every other subcommand.
func fakeKubectl(t *testing.T, fixture string) {
	t.Helper()
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "describe-rs.txt")
	if err := os.WriteFile(fixturePath, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = describe ] && [ \"$2\" = replicasets ]; then cat " + fixturePath + "; exit 0; fi\n" +
		"echo \"fake kubectl $*\"\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o700); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatalf("write fake kubectl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// captureGinkgoWriter tees the diagnostics output the dump writes.
func captureGinkgoWriter(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	GinkgoWriter.TeeTo(&buf)
	t.Cleanup(GinkgoWriter.ClearTeeWriters)
	return &buf
}

// A superseded ReplicaSet's pod template is the only record of what a mid-run
// AGC roll changed (Q593): its pods are gone, so no pod-scoped dump holds it.
func TestDumpAGCSessionDiagnosticsCapturesSupersededReplicaSetTemplate(t *testing.T) {
	fakeKubectl(t, supersededRSDescription)
	out := captureGinkgoWriter(t)

	DumpAGCSessionDiagnostics("ssrec", "ssrec-agc", "gateway-system", "fakegithub")

	dumped := out.String()
	for _, want := range []string{
		"--- replicaset pod templates in ssrec ---",
		"deployment.kubernetes.io/revision: 1",
		"Pod Template:",
		"ghcr.io/example/agc:v0.9.0",
	} {
		if !strings.Contains(dumped, want) {
			t.Errorf("dump is missing %q from the superseded ReplicaSet's template:\n%s", want, dumped)
		}
	}
}

// The revisions table orders a roll absolutely; describe alone gives no
// creation timestamp.
func TestDumpAGCSessionDiagnosticsRequestsReplicaSetRevisionOrdering(t *testing.T) {
	fakeKubectl(t, supersededRSDescription)
	out := captureGinkgoWriter(t)

	DumpAGCSessionDiagnostics("ssrec", "ssrec-agc", "gateway-system", "fakegithub")

	dumped := out.String()
	if !strings.Contains(dumped, "--- replicaset revisions in ssrec ---") {
		t.Errorf("dump is missing the replicaset revisions section:\n%s", dumped)
	}
	if !strings.Contains(dumped, "CREATED:.metadata.creationTimestamp") {
		t.Errorf("revisions table does not request a creation timestamp:\n%s", dumped)
	}
}
