//go:build integration

package integration_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The install doc and its block parser, both reached through committed
// testdata/ symlinks so the reads land inside this module root and go's test
// cache keys on them (Q895/Q902). A direct ../../../../../docs read is invisible
// to the cache: edit the doc alone and a cached pass replays.
const (
	gettingStartedDoc = "testdata/getting-started.md"
	docBlocksScript   = "testdata/doc-blocks.sh"
	// doc-blocks.sh sources this on every invocation, so it is as much an input
	// to the parse as the script itself.
	docBlocksLib = "testdata/scripts-lib-common.sh"
)

// executedBlockFloor is the number of blocks THIS venue must run, and it is the
// assertion that keeps a silent extraction failure from reading as a pass: a
// parser that returns nothing satisfies every per-block assertion vacuously.
//
// One lower than the floor scripts/docs/doc-blocks.sh enforces, and the
// difference is the point rather than a slip: that floor counts every executed
// block including the `mode=render` one, which is a helm command this venue has
// no helm for. `make getting-started-render-check` is the half that runs it.
const executedBlockFloor = 9

// docBlock is the part of a scripts/docs/doc-blocks.sh --emit record this walk
// acts on. The record carries more (the fence language, the needs= list, the
// teardown hint, a skip reason); those are the offline gate's to validate, and
// restating them here as fields nothing reads would imply this test checks them.
type docBlock struct {
	id   string
	mode string
	line string
}

// TestGettingStarted_Executable runs docs/getting-started.md against a real
// apiserver (Q958).
//
// The page is the install procedure an operator follows verbatim, and nothing
// ran a line of it: `make check` held it to link and prose rules, and the e2e
// suite builds its tenant fixtures in Go, so a CRD field renamed under the doc
// left it rendering perfectly and failing on contact.
//
// What this venue settles that reading the page cannot: the CRD structural
// schemas, the GMC's validating webhooks, the v2 conversion webhook, and the
// CEL guardrails — every one of which this suite already stands up, so the walk
// costs seconds on a job that is already running rather than a kind cluster of
// its own. What it cannot settle is anything needing a kubelet: the two blocks
// that roll a Deployment and read its logs are declared `mode=skip` in the doc
// with that reason, and reaching them is Q958's follow-on.
//
// Coverage is opt-in per block and the annotations live in the doc, so this test
// parses nothing itself — scripts/docs/doc-blocks.sh is the single parser, the
// same one `make getting-started-check` runs, handed the exact bytes read here.
func TestGettingStarted_Executable(t *testing.T) {
	doc, err := os.ReadFile(gettingStartedDoc)
	require.NoError(t, err)

	// Read every file the parse depends on, rather than only exec'ing the script.
	// Exec is not a testlog read, so without this a change to doc-blocks.sh or to
	// the lib it sources would replay a cached pass on any run that does not
	// force -count=1 — and go-test-integration.sh forces that only under a -run
	// filter. Reading both makes them real cache inputs, and doubles as the
	// precondition check the exec would otherwise fail confusingly on.
	//
	// This test is the case Q953 predicted: the first cached-tier test to shell
	// out against the repo rather than against a temp dir it populated itself.
	// The reads below are what keep it caching correctly; they are not a
	// substitute for the detector that row asks for, which would catch the next
	// one written without them.
	for _, dep := range []string{docBlocksScript, docBlocksLib} {
		content, readErr := os.ReadFile(dep)
		require.NoError(t, readErr)
		require.NotEmptyf(t, content, "%s resolved to an empty file; the testdata symlink is broken", dep)
	}

	kubectl := filepath.Join(os.Getenv("KUBEBUILDER_ASSETS"), "kubectl")
	require.FileExistsf(t, kubectl, "envtest ships kubectl in its binary assets; KUBEBUILDER_ASSETS=%q", os.Getenv("KUBEBUILDER_ASSETS"))

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(kubeconfig, testEnv.KubeConfig, 0o600))

	blocks := parseBlocks(t, doc)

	// Every object this walk creates lands in the tenant namespace the doc's own
	// first step creates, so one delete retires the lot. Registered before the
	// walk starts: a block failing midway must still clean up after itself.
	t.Cleanup(func() {
		_, _ = runKubectl(kubectl, kubeconfig, nil, "delete", "namespace", "team-a", "--ignore-not-found", "--wait=false")
	})

	executed := 0
	for _, b := range blocks {
		if b.mode == "skip" || b.mode == "render" {
			continue
		}
		executed++
		t.Run(b.id, func(t *testing.T) {
			body := blockBody(t, doc, b.id)
			require.NotEmptyf(t, body, "%s:%s: block %q extracted empty", gettingStartedDoc, b.line, b.id)

			var out string
			var err error
			switch b.mode {
			case "apply":
				out, err = runKubectl(kubectl, kubeconfig, []byte(body), "apply", "--server-side", "-f", "-")
				if err == nil {
					// Read every object back. A server-side apply reports success
					// for a document the apiserver accepted and pruned to
					// nothing, so the apply's exit status alone is not evidence
					// the block created what it says it does.
					var readback string
					readback, err = runKubectl(kubectl, kubeconfig, []byte(body), "get", "-f", "-", "-o", "name")
					out += "\n--- read-back ---\n" + readback
				}
			case "dry-run":
				out, err = runKubectl(kubectl, kubeconfig, []byte(body), "apply", "--server-side", "--dry-run=server", "-f", "-")
			case "run":
				out, err = runShell(t, kubectl, kubeconfig, body)
			default:
				t.Fatalf("%s:%s: block %q has unhandled mode %q", gettingStartedDoc, b.line, b.id, b.mode)
			}

			// The failure a doc drifting produces is "which block, and what did
			// it expect" — a bare non-zero exit sends the reader to the whole
			// page. Name the block, its line, its mode, and the apiserver's own
			// words.
			require.NoErrorf(t, err,
				"%s:%s: block %q (mode=%s) failed against the apiserver\n--- the doc says ---\n%s\n--- the apiserver says ---\n%s",
				gettingStartedDoc, b.line, b.id, b.mode, body, out)
		})
	}

	require.GreaterOrEqualf(t, executed, executedBlockFloor,
		"walked %d executable blocks in %s, floor is %d. A block lost its gag:verify annotation, flipped to mode=skip, or the extractor stopped reaching the page.",
		executed, gettingStartedDoc, executedBlockFloor)
}

// parseBlocks runs the shared parser over the doc bytes and returns its records
// in declaration order. Order is the whole prerequisite mechanism: the offline
// gate asserts every `needs=` names a block declared earlier, so applying in
// file order satisfies them without a second dependency walk here.
func parseBlocks(t *testing.T, doc []byte) []docBlock {
	t.Helper()
	out, err := runScript(t, doc, "--emit", "-")
	require.NoErrorf(t, err, "doc-blocks.sh --emit failed:\n%s", out)

	var blocks []docBlock
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		require.Lenf(t, f, 7, "unexpected doc-blocks.sh record: %q", line)
		blocks = append(blocks, docBlock{id: f[0], mode: f[1], line: f[3]})
	}
	require.NotEmpty(t, blocks, "doc-blocks.sh returned no records for "+gettingStartedDoc)
	return blocks
}

// blockBody returns one block's literal text, resolved by the same parser, so
// this test and the offline gate cannot disagree about which block an id names.
func blockBody(t *testing.T, doc []byte, id string) string {
	t.Helper()
	out, err := runScript(t, doc, "--body", "-", id)
	require.NoErrorf(t, err, "doc-blocks.sh --body %s failed:\n%s", id, out)
	return out
}

func runScript(t *testing.T, stdin []byte, args ...string) (string, error) {
	t.Helper()
	// G204's threat model is command injection, and the binary here is a
	// compile-time constant naming a committed in-repo symlink, not a path
	// derived at runtime. The arguments are literals and the document arrives on
	// stdin rather than as an argv entry.
	//nolint:gosec // G204: constant binary, literal args, document on stdin.
	cmd := exec.Command(docBlocksScript, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func runKubectl(kubectl, kubeconfig string, stdin []byte, args ...string) (string, error) {
	// The binary is variable, which is the form .golangci.yml keeps failing on
	// purpose. Audited: it resolves under KUBEBUILDER_ASSETS, the directory this
	// suite already execs its apiserver and etcd out of, so trusting kubectl
	// there is the trust the suite is built on. Nothing outside envtest's own
	// asset dir can reach this path.
	//nolint:gosec // G204: kubectl from the envtest asset dir the suite already runs.
	cmd := exec.Command(kubectl, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// runShell executes a `mode=run` block as the operator would: the doc's own
// command lines, unedited, with kubectl resolved from the envtest assets and
// KUBECONFIG pointed at this suite's apiserver. Translating the commands into
// client-go calls instead would assert what the block is understood to mean
// rather than what it says, which is the drift this test exists to catch.
func runShell(t *testing.T, kubectl, kubeconfig, body string) (string, error) {
	t.Helper()
	bin := t.TempDir()
	require.NoError(t, os.Symlink(kubectl, filepath.Join(bin, "kubectl")))

	script := filepath.Join(t.TempDir(), "block.sh")
	// 0600, not 0700: bash reads this file rather than exec'ing it, so the
	// execute bit buys nothing and gosec is right to want it gone.
	require.NoError(t, os.WriteFile(script, []byte("set -euo pipefail\n"+body+"\n"), 0o600))

	// A shell, which .golangci.yml also keeps failing on purpose. Audited: the
	// script body is a fenced block from a committed repo doc, and running those
	// commands as written IS this test. Translating them into client-go calls to
	// avoid the shell would assert what the block is understood to mean rather
	// than what it says, which is the drift the test exists to catch. No input
	// here comes from outside the repo.
	//nolint:gosec // G204: shell body is a fenced block from a committed doc.
	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(),
		"KUBECONFIG="+kubeconfig,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
