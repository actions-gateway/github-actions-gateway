package main

import (
	"os"
	"strings"
	"testing"
)

// statusPath is the live backlog, read from the repo rather than a fixture.
// A fixture would prove the renderer reproduces the fixture; the claim this
// migration rests on is that it reproduces the real table, every row of it.
const statusPath = "../../../docs/STATUS.md"

// liveRows returns the backlog file's Queue and Deferred data rows as written,
// scoped by section heading. Whole lines are compared, so a pipe inside a cell
// cannot perturb the extraction the way splitting on `|` would.
func liveRows(t *testing.T) (queue, deferred []string, src []byte) {
	t.Helper()
	src, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatalf("reading %s: %v", statusPath, err)
	}
	section := ""
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(line, "## ") {
			section = strings.TrimPrefix(line, "## ")
		}
		if !strings.HasPrefix(line, "| <a id=") {
			continue
		}
		switch {
		case strings.HasPrefix(section, "Queue"):
			queue = append(queue, line)
		case strings.HasPrefix(section, "Deferred"):
			deferred = append(deferred, line)
		}
	}
	return queue, deferred, src
}

// The phase-1 exit criterion: importing the live table and rendering it back
// reproduces every row byte for byte. Anything the item model cannot carry
// shows up here as a diff rather than as silent data loss at cutover.
func TestRoundTripReproducesTheLiveTable(t *testing.T) {
	wantQueue, wantDeferred, src := liveRows(t)

	// A migration that imported nothing would round-trip perfectly and prove
	// nothing, so the population is asserted before the comparison.
	if len(wantQueue) < 50 || len(wantDeferred) < 25 {
		t.Fatalf("read %d Queue and %d Deferred rows from %s; the live tables are larger than that, so the extraction is wrong",
			len(wantQueue), len(wantDeferred), statusPath)
	}

	items, err := ImportStatus(src)
	if err != nil {
		t.Fatalf("ImportStatus: %v", err)
	}
	if got, want := len(items), len(wantQueue)+len(wantDeferred); got != want {
		t.Fatalf("imported %d items, want %d; the importer dropped or invented rows", got, want)
	}
	for _, it := range items {
		if err := it.Validate(); err != nil {
			t.Errorf("imported item is invalid: %v", err)
		}
	}

	compareRows(t, "Queue", RenderRows(items, false), wantQueue)
	compareRows(t, "Deferred", RenderRows(items, true), wantDeferred)
}

func compareRows(t *testing.T, table string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: rendered %d rows, want %d", table, len(got), len(want))
	}
	bad := 0
	for i := range want {
		if got[i] == want[i] {
			continue
		}
		bad++
		if bad <= 5 {
			t.Errorf("%s row %d does not round-trip:\n  want %s\n  got  %s", table, i, want[i], got[i])
		}
	}
	if bad > 5 {
		t.Errorf("%s: %d rows in total did not round-trip", table, bad)
	}
}

// storeDir is the committed per-item store, which lives alongside the table
// until the cutover deletes the table.
const storeDir = "../../../docs/queue"

// Until the cutover, both representations are committed and either can be
// edited, so this is the gate that stops them drifting: the store on disk has
// to render exactly the table on disk. It fails on a row edited in one place
// and not the other, on an item file deleted without its row, and on a row
// filed without its file.
func TestCommittedStoreMatchesTheTable(t *testing.T) {
	wantQueue, wantDeferred, _ := liveRows(t)

	items, err := ReadStore(storeDir)
	if err != nil {
		t.Fatalf("ReadStore(%s): %v", storeDir, err)
	}
	if got, want := len(items), len(wantQueue)+len(wantDeferred); got != want {
		t.Fatalf("the store holds %d items and the tables hold %d rows; regenerate with `queuestore import docs/STATUS.md docs/queue`", got, want)
	}

	compareRows(t, "Queue", RenderRows(items, false), wantQueue)
	compareRows(t, "Deferred", RenderRows(items, true), wantDeferred)
}

// Rank order has to reproduce the table's line order on import, or the cutover
// would silently re-prioritize the backlog.
func TestImportPreservesLineOrder(t *testing.T) {
	wantQueue, _, src := liveRows(t)
	items, err := ImportStatus(src)
	if err != nil {
		t.Fatalf("ImportStatus: %v", err)
	}
	rendered := RenderRows(items, false)
	for i := range wantQueue {
		wantID := idOf(wantQueue[i])
		gotID := idOf(rendered[i])
		if wantID != gotID {
			t.Fatalf("Queue position %d holds %s after import, want %s", i, gotID, wantID)
		}
	}
}

func idOf(row string) string {
	const open = `<a id="`
	i := strings.Index(row, open)
	if i < 0 {
		return ""
	}
	rest := row[i+len(open):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}
