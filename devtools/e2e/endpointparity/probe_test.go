package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// deadBase is a loopback address nothing can be listening on: binding port 1
// takes privilege no gate has, so a readiness probe against it refuses rather
// than racing whatever else the host is running.
const deadBase = "http://127.0.0.1:1"

// writeLog puts the child log where waitReady and childLog look for it.
func writeLog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakegithub.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return path
}

// The timeout report has to separate a child that ran from one that produced
// nothing, since both look like silence on the socket and that is the whole
// question a contended host raises (Q912). Asserted in both directions: the
// timing clause appears when the child wrote, and is absent when it did not.
func TestWaitReadyTimeoutReportsWhetherTheChildWrote(t *testing.T) {
	restore := readyTimeout
	readyTimeout = 200 * time.Millisecond
	t.Cleanup(func() { readyTimeout = restore })

	const clause = "first became non-empty"
	for _, tc := range []struct {
		name    string
		log     string
		wantHas bool
	}{
		{"the child wrote before the deadline", "fakegithub listening on 127.0.0.1:1\n", true},
		{"the child wrote nothing", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := waitReady(deadBase, probeClient(), make(chan error), writeLog(t, tc.log), os.Getpid())
			if err == nil {
				t.Fatal("waitReady returned nil against a port nothing is listening on")
			}
			if got := strings.Contains(err.Error(), clause); got != tc.wantHas {
				t.Errorf("timing clause present = %v, want %v; report was:\n%s", got, tc.wantHas, err)
			}
			// The OS reading is what answers "scheduled at all" when the log is
			// silent, so it must be attempted either way. This process is alive,
			// so a failure to read it is a failure of the reading itself.
			if !strings.Contains(err.Error(), fmt.Sprintf("pid %d", os.Getpid())) {
				t.Errorf("report does not name the child pid:\n%s", err)
			}
		})
	}
}

// childLog's three states must stay distinguishable, and the empty one must
// state the observation rather than concluding from it: an empty log says
// nothing on its own about how far the child got, since whether that binary
// writes before it binds is a property of the binary (Q912).
func TestChildLogStatesTheObservation(t *testing.T) {
	missing := childLog(filepath.Join(t.TempDir(), "absent.log"))
	empty := childLog(writeLog(t, ""))
	wrote := childLog(writeLog(t, "bind: address already in use\n"))

	// The shipped over-claim by name, so a revert reads as a failure here rather
	// than as a wording preference.
	for _, s := range []string{missing, empty, wrote} {
		if strings.Contains(s, "had not reached") {
			t.Errorf("childLog concludes how far the child got from an empty log: %s", s)
		}
	}
	if missing == empty || empty == wrote || missing == wrote {
		t.Errorf("childLog cannot tell its three states apart:\n%s\n%s\n%s", missing, empty, wrote)
	}
	if !strings.Contains(wrote, "address already in use") {
		t.Errorf("childLog dropped what the child said: %s", wrote)
	}
}
