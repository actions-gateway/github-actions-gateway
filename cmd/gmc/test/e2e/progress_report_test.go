//go:build e2e
// +build e2e

package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
)

// Progress events are consumed live by scripts/e2e/progress-watch.sh, which
// renders the heartbeat line in the Actions log. Ginkgo cannot do that itself:
// at --procs > 1 the reporter suppresses spec-start entirely and intercepts
// stdout for the duration of each spec, so anything the suite prints is
// replayed at spec end rather than streamed. File writes are not intercepted.
// Rationale and the measurements behind it: docs/plan/archive/e2e-progress-visibility.md.

// progressEvent is one line of $E2E_PROGRESS_FILE.
type progressEvent struct {
	Kind  string  `json:"kind"`            // "total" | "start" | "end"
	Time  int64   `json:"t"`               // unix seconds
	Proc  int     `json:"proc,omitempty"`  // ginkgo parallel process number
	Spec  string  `json:"spec,omitempty"`  // full spec text
	State string  `json:"state,omitempty"` // terminal state, "end" only
	Secs  float64 `json:"secs,omitempty"`  // spec runtime, "end" only
	Total int     `json:"total,omitempty"` // specs that will run, "total" only
}

// maxSpecTextLen bounds the spec text carried in an event so a whole line stays
// well under PIPE_BUF (4096). Below that size an O_APPEND write to a regular
// file lands atomically, which is what lets all --procs processes share one
// file without a lock; above it, two processes can interleave bytes and corrupt
// both records.
const maxSpecTextLen = 300

// progressMu serializes this process's own writes. Cross-process atomicity
// comes from O_APPEND under PIPE_BUF, not from this.
var progressMu sync.Mutex

// emitProgress appends one event to $E2E_PROGRESS_FILE. It is best-effort by
// design: a progress-reporting failure must never turn a green suite red, so
// every error path returns silently. An unset E2E_PROGRESS_FILE disables the
// mechanism, which is what a plain `go test` run gets.
func emitProgress(ev progressEvent) {
	path := os.Getenv("E2E_PROGRESS_FILE")
	if path == "" {
		return
	}

	ev.Time = time.Now().Unix()
	if len(ev.Spec) > maxSpecTextLen {
		ev.Spec = ev.Spec[:maxSpecTextLen]
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	line = append(line, '\n')

	progressMu.Lock()
	defer progressMu.Unlock()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(line)
}

// TestProgressEventFitsPipeBuf guards the atomicity budget. Every parallel
// process appends to one file with no lock, so a line that grows past PIPE_BUF
// stops being a single atomic write and two processes can interleave bytes —
// corrupting both records silently, in a file nothing validates. The worst case
// is a maximum-length spec text where every character needs a JSON escape.
func TestProgressEventFitsPipeBuf(t *testing.T) {
	const pipeBuf = 4096

	line, err := json.Marshal(progressEvent{
		Kind:  "end",
		Time:  time.Now().Unix(),
		Proc:  1024,
		Spec:  strings.Repeat(`"`, maxSpecTextLen),
		State: "interrupted",
		Secs:  1234.5678,
		Total: 100000,
	})
	if err != nil {
		t.Fatalf("marshal worst-case event: %v", err)
	}
	if got := len(line) + 1; got >= pipeBuf {
		t.Errorf("worst-case event line is %d bytes, must stay under PIPE_BUF (%d); lower maxSpecTextLen", got, pipeBuf)
	}
}

// specText renders a spec report as the single line the heartbeat displays.
// ContainerHierarchyTexts + LeafNodeText is the same composition Ginkgo's own
// reporter uses, minus the code location.
func specText(report types.SpecReport) string {
	parts := append([]string{}, report.ContainerHierarchyTexts...)
	if report.LeafNodeText != "" {
		parts = append(parts, report.LeafNodeText)
	}
	return strings.Join(parts, " ")
}

// The total is written once, from process 1, before any spec runs — it is the
// heartbeat's denominator, and the watcher stays silent until it arrives.
var _ = ReportBeforeSuite(func(report types.Report) {
	emitProgress(progressEvent{Kind: "total", Total: report.PreRunStats.SpecsThatWillRun})
})

var _ = ReportBeforeEach(func(report types.SpecReport) {
	emitProgress(progressEvent{
		Kind: "start",
		Proc: GinkgoParallelProcess(),
		Spec: specText(report),
	})
})

var _ = ReportAfterEach(func(report types.SpecReport) {
	emitProgress(progressEvent{
		Kind:  "end",
		Proc:  GinkgoParallelProcess(),
		Spec:  specText(report),
		State: report.State.String(),
		Secs:  report.RunTime.Seconds(),
	})
})
