// Command mergedriver is the git merge driver behind this repo's registry
// files: one binary, one subcommand per driver, invoked by the thin
// scripts/ entry points that git config points at.
//
// It replaces scripts/lib/merge-keyed-records.awk and the per-driver shell that
// wrapped it. The set merge itself is devtools/git/keyedrecords, which is where
// the behaviour is tested; this package is the plumbing around it.
//
// Every uncertainty ends the same way: re-run the plain three-way merge and
// leave its conflict markers, with a one-line reason on stderr. A conflict
// marker costs a minute; a wrong silent resolution loses a registry row.
package main

import (
	"fmt"
	"os"
	"strings"
)

const usage = "usage: mergedriver <driver> %O %A %B %L %P %S %X %Y"

// drivers maps a subcommand to the file it merges and how it merges it.
var drivers = map[string]struct {
	spec spec
	run  func(*invocation)
}{
	"scriptindex": {
		spec: spec{
			name:        "scriptindex",
			log:         "merge-script-index",
			defaultPath: "scripts/README.md",
		},
		run: registryDriver{
			subject: "script",
			noun:    "registry",
			// Group rows in the summary table link to in-page anchors, not to
			// files; they share no namespace with script paths.
			dupeKey: func(target string) (string, bool) {
				return target, !strings.HasPrefix(target, "#")
			},
		}.run,
	},
	"planindex": {
		spec: spec{
			name:        "planindex",
			log:         "merge-plan-index",
			defaultPath: "docs/plan/README.md",
		},
		run: registryDriver{
			subject: "plan",
			noun:    "index",
			// Compared on the basename, because that is the plan's identity:
			// `archive/x.md` and `x.md` are the same doc in two sections, and it
			// is exactly that pair the per-table merge cannot rule out.
			// check-plan-index.sh compares basenames for the same reason.
			dupeKey: func(target string) (string, bool) {
				return strings.TrimPrefix(target, "archive/"), true
			},
		}.run,
	},
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "%s\n", usage)
		os.Exit(2)
	}
	d, ok := drivers[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "mergedriver: unknown driver %q\n", os.Args[1])
		os.Exit(2)
	}
	in, err := d.spec.parse(os.Args[2:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", d.spec.log, err)
		os.Exit(2)
	}
	d.run(in)
}
