package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The full migration path, end to end: the live table becomes item files on
// disk, the files are read back, and rendering them reproduces the table byte
// for byte. The in-memory round-trip cannot catch what the file format loses,
// which is the half that actually ships.
func TestStoreRoundTripThroughFiles(t *testing.T) {
	wantQueue, wantDeferred, src := liveRows(t)

	items, err := ImportStatus(src)
	if err != nil {
		t.Fatalf("ImportStatus: %v", err)
	}

	dir := t.TempDir()
	if err := WriteStore(dir, items); err != nil {
		t.Fatalf("WriteStore: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != len(items) {
		t.Fatalf("wrote %d files for %d items", len(entries), len(items))
	}

	back, err := ReadStore(dir)
	if err != nil {
		t.Fatalf("ReadStore: %v", err)
	}
	if len(back) != len(items) {
		t.Fatalf("read %d items back, want %d", len(back), len(items))
	}

	compareRows(t, "Queue", RenderRows(back, false), wantQueue)
	compareRows(t, "Deferred", RenderRows(back, true), wantDeferred)
}

// A store the reader silently accepts with a file missing would let the cutover
// drop items, so the reader has to be shown failing on one.
func TestReadStoreRejectsAMismatchedFilename(t *testing.T) {
	dir := t.TempDir()
	it := Item{ID: "Q1", Rank: "a0", Status: StatusReady, Size: "S", Title: "A title"}
	body, err := it.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Q2.md"), body, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := ReadStore(dir); err == nil {
		t.Error("ReadStore accepted a file whose name disagrees with its id; want an error")
	}
}

func TestUnmarshalItemRejectsMalformedFiles(t *testing.T) {
	for name, src := range map[string]string{
		"no frontmatter": "# A title\n",
		"unclosed fence": "---\nid: Q1\n",
		"no h1":          "---\nid: Q1\nrank: a0\nstatus: ready\nsize: S\n---\n\nnot a heading\n",
		"bad rank":       "---\nid: Q1\nrank: zzz9\nstatus: ready\nsize: S\n---\n\n# A title\n",
		"unknown status": "---\nid: Q1\nrank: a0\nstatus: pending\nsize: S\n---\n\n# A title\n",
	} {
		if _, err := UnmarshalItem([]byte(src)); err == nil {
			t.Errorf("%s: UnmarshalItem succeeded; want an error", name)
		}
	}
}

func TestMarshalUnmarshalPreservesEveryField(t *testing.T) {
	want := Item{
		ID:     "Q869",
		Rank:   "a0i",
		Labels: []string{"speed", "ci", "debt"},
		Status: StatusBlocked,
		Size:   "L",
		Target: "plan/q869-per-item-queue-store.md",
		Title:  "Store each backlog item in its own file",
		Notes:  "Priority is stored as line position, so the top *k* rows conflict for every k >= 2.",
	}
	body, err := want.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := UnmarshalItem(body)
	if err != nil {
		t.Fatalf("UnmarshalItem: %v", err)
	}
	if got.Row() != want.Row() {
		t.Errorf("round-trip changed the row:\n  want %s\n  got  %s", want.Row(), got.Row())
	}
	if got.Target != want.Target {
		t.Errorf("target = %q, want %q", got.Target, want.Target)
	}
}
