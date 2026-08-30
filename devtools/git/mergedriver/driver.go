package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// spec is everything a driver needs to describe itself to git config and to
// label its own output. One per merged file.
type spec struct {
	name        string // the `merge.<name>` git config key, for temp-dir naming
	log         string // the prefix on this driver's stderr lines
	defaultPath string // the file this driver merges, used when git passes no %P
}

// invocation is one merge, with git's placeholders resolved.
type invocation struct {
	spec
	base, ours, theirs string
	markerSize         int
	targetPath         string
	labelBase          string
	labelOurs          string
	labelTheirs        string
}

// note prints one line of driver commentary on stderr, so a resolution (or a
// refusal) is never silent.
func (in *invocation) note(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s: %s\n", in.log, fmt.Sprintf(format, args...))
}

// label resolves one conflict label. git passes %S/%X/%Y only from 2.44 on; an
// older git leaves the placeholder unexpanded, so a value that still looks like
// one is not a label.
func label(value, fallback string) string {
	if value == "" || strings.HasPrefix(value, "%") {
		return fallback
	}
	return value
}

// parse resolves git's placeholders. It returns nil when the arguments are not
// a merge invocation at all, which the caller reports rather than guessing at.
func (s spec) parse(args []string) (*invocation, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("expected the %%O %%A %%B placeholders; run --install to configure git, or --help")
	}
	in := &invocation{spec: s, base: args[0], ours: args[1], theirs: args[2]}

	in.markerSize = 7
	if len(args) > 3 {
		if n, err := strconv.Atoi(args[3]); err == nil && n >= 7 {
			in.markerSize = n
		}
	}

	in.targetPath = s.defaultPath
	if len(args) > 4 && args[4] != "" {
		in.targetPath = args[4]
	}

	at := func(i int) string {
		if len(args) > i {
			return args[i]
		}
		return ""
	}
	in.labelBase = label(at(5), in.targetPath+" (base)")
	in.labelOurs = label(at(6), in.targetPath+" (ours)")
	in.labelTheirs = label(at(7), in.targetPath+" (theirs)")
	return in, nil
}

// mergeFile runs git's own three-way merge. With inPlace it writes the result
// back into ours, which is what git expects a driver to leave behind; otherwise
// the merged text is returned. The bool reports whether it merged cleanly.
func (in *invocation) mergeFile(base, ours, theirs string, inPlace bool) ([]byte, bool, error) {
	args := []string{"merge-file", "--marker-size=" + strconv.Itoa(in.markerSize)}
	if !inPlace {
		args = append(args, "-p")
	}
	args = append(args, "-L", in.labelOurs, "-L", in.labelBase, "-L", in.labelTheirs, ours, base, theirs)

	//nolint:gosec // G204: a fixed git verb plus the three paths git handed this
	// driver as its own placeholders.
	out, err := exec.Command("git", args...).Output()
	if err == nil {
		return out, true, nil
	}
	// git merge-file exits with the number of conflicts, so a non-zero status
	// is an ordinary conflict rather than a failure to run.
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, false, nil
	}
	return nil, false, err
}

// fallback is the only exit path for an uncertain merge: redo the merge the way
// git would have without this driver, and keep whatever it produces. Clean
// means nothing was actually contested, so it exits 0; otherwise the file
// carries ordinary conflict markers and the driver reports a conflict.
//
// Exit status stays under 128 either way: git reads >128 as "the driver
// crashed", which fails the whole merge instead of recording a conflict.
func (in *invocation) fallback(format string, args ...any) {
	reason := fmt.Sprintf(format, args...)
	_, clean, err := in.mergeFile(in.base, in.ours, in.theirs, true)
	if err != nil {
		in.note("%s; and git merge-file could not be run: %v", reason, err)
		os.Exit(1)
	}
	if clean {
		in.note("%s; the plain three-way merge resolved it cleanly", reason)
		os.Exit(0)
	}
	in.note("%s; left ordinary conflict markers in %s", reason, in.targetPath)
	os.Exit(1)
}
