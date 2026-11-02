package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingReader fails the test if read from — proves the --assume-yes path never
// touches stdin.
type failingReader struct{ t *testing.T }

func (r failingReader) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Fatal("confirm read stdin under --assume-yes; it must not")
	return 0, nil
}

// TestRun_RejectsRemovedFanOut proves a 1.x fan-out invocation fails loudly and names
// where the fan-out went, rather than exiting 0 having migrated nothing.
func TestRun_RejectsRemovedFanOut(t *testing.T) {
	for _, args := range [][]string{nil, {"--namespace", "team-a", "--apply"}} {
		var stderr bytes.Buffer
		err := run(args, failingReader{t}, &bytes.Buffer{}, &stderr)
		require.Error(t, err)
		assert.Contains(t, stderr.String(), "gag-migrate storage-version")
		assert.Contains(t, stderr.String(), "1.x gag-migrate")
	}
}

// TestConfirm_AssumeYes skips the prompt and never touches stdin.
func TestConfirm_AssumeYes(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirm("the sweep", "kind-gag", "every object", "effect", true, failingReader{t}, &out)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Contains(t, out.String(), "skipping confirmation")
	// The target and scope are always echoed before any write, assume-yes or not.
	assert.Contains(t, out.String(), "kind-gag")
}

// TestConfirm_Prompt covers the interactive replies: only y/Y/yes proceeds;
// everything else (and EOF on a piped stdin) declines without writing.
func TestConfirm_Prompt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  bool
	}{
		{"yes-short", "y\n", true},
		{"yes-upper", "Y\n", true},
		{"yes-word", "yes\n", true},
		{"no", "n\n", false},
		{"empty", "\n", false},
		{"eof", "", false},
		{"garbage", "maybe\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			ok, err := confirm("the sweep", "prod-cluster", "every object, cluster-wide", "effect", false, strings.NewReader(tc.input), &out)
			require.NoError(t, err)
			assert.Equal(t, tc.want, ok)
			// The resolved target + scope are echoed so a wrong cluster is visible.
			assert.Contains(t, out.String(), "prod-cluster")
			assert.Contains(t, out.String(), "cluster-wide")
			if !tc.want {
				assert.Contains(t, out.String(), "Aborted")
			}
		})
	}
}
