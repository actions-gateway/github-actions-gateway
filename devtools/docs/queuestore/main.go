// Command queuestore reads and writes the per-item backlog store: one file per
// item under docs/queue/, ordered by a per-item rank key rather than by a
// position in a shared table (Q869).
//
// Storing the order as a position is what makes the backlog conflict. Picking
// from the top concentrates deletions there and priority-on-entry concentrates
// insertions there, so the busiest region of the file is the contended one by
// construction, and taking the top k rows conflicts for every k >= 2. The
// ID-keyed merge driver resolves those correctly but is per-clone git config,
// so it never runs on GitHub: by the time it fires, the PR is already DIRTY and
// a rebase has forced a full CI re-run. A rank held inside each item's own file
// removes the conflict instead of resolving it.
//
// Subcommands:
//
//	import <status.md> <dir>   read the Queue and Deferred tables into the store
//	render <dir>              write the ordered tables to stdout
//	check <dir>               validate every item file
//
// import is re-runnable and deterministic, which is what makes a rebase cheap
// while the cutover is in flight: take the base's docs/STATUS.md and re-import
// rather than hand-merging the store.
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "queuestore: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, out *os.File) error {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "import":
		if len(args) != 3 {
			return usage()
		}
		src, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		items, err := ImportStatus(src)
		if err != nil {
			return err
		}
		if err := WriteStore(args[2], items); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "queuestore: imported %d items into %s\n", len(items), args[2])
		return nil

	case "render":
		if len(args) != 2 {
			return usage()
		}
		items, err := ReadStore(args[1])
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, strings.Join(RenderRows(items, false), "\n"))
		return nil

	case "check":
		if len(args) != 2 {
			return usage()
		}
		items, err := ReadStore(args[1])
		if err != nil {
			return err
		}
		for _, it := range items {
			if err := it.Validate(); err != nil {
				return err
			}
		}
		_, _ = fmt.Fprintf(out, "queuestore: ok (%d items in %s)\n", len(items), args[1])
		return nil
	}
	return usage()
}

func usage() error {
	return fmt.Errorf("usage: queuestore import <status.md> <dir> | render <dir> | check <dir>")
}
