// Command gag-migrate carries the one-shot migrations an upgrade needs. Its one
// command, `gag-migrate storage-version`, is the 1.10 sweep that rewrites every
// stored actions-gateway.com object at v2 (storageversion.go, Q1086).
//
// The v1alpha1 → v2alpha1 fan-out it carried through 1.x left with the
// actions-gateway.github.com group in v2.0.0 (Q273): a v1 tenant migrates with a
// 1.x gag-migrate before the upgrade (docs/operations/migration-v1-to-v2.md).
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
)

// errAborted is returned when the operator declines the --apply confirmation
// prompt. main maps any run error to a non-zero exit, so a declined apply exits
// non-zero (mirroring scripts/lib/common.sh's confirm_or_exit) — nothing is written.
var errAborted = errors.New("apply aborted by operator; no changes made")

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// fprintf/fprintln write to a CLI stream, discarding the write error: a failed
// write to stdout/stderr is not actionable and must not mask the operation's own
// result (errcheck would otherwise flag the unchecked fmt.Fprint* returns).
func fprintf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
func fprintln(w io.Writer, a ...any)               { _, _ = fmt.Fprintln(w, a...) }

// run dispatches to the named command. Anything else prints the usage and fails,
// so a 1.x invocation of the removed fan-out exits non-zero rather than silently
// doing nothing.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == storageVersionCommand {
		return runStorageVersion(args[1:], stdin, stdout, stderr)
	}
	fprintf(stderr, "Usage:\n  gag-migrate %s [--context <ctx>] [--apply]   (see gag-migrate %s -h)\n\n", storageVersionCommand, storageVersionCommand)
	fprintf(stderr, "The v1alpha1 -> v2 fan-out was removed in v2.0.0; migrate v1 tenants with a 1.x gag-migrate before upgrading.\n")
	if len(args) == 0 {
		return fmt.Errorf("a command is required")
	}
	return fmt.Errorf("unknown command %q", args[0])
}

// resolveContextName returns the kube context the run will target, for display in
// the confirmation prompt. An explicit --context wins; otherwise it best-effort
// reads the kubeconfig current-context so the operator sees the real target name
// rather than a bare "current-context". Display-only: a resolution failure never
// blocks the run (the confirmation itself is the guard).
func resolveContextName(override string) string {
	if override != "" {
		return override
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	raw, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).RawConfig()
	if err != nil || raw.CurrentContext == "" {
		return "(kubeconfig current-context — could not resolve name)"
	}
	return raw.CurrentContext
}

// confirm gates an --apply write behind an explicit operator confirmation, echoing
// the resolved target context and scope first (the wrong-cluster guard): what names
// the operation and effect says what it changes. assumeYes (from --assume-yes or
// ASSUME_YES=1) skips the prompt for automation, consistent with
// scripts/lib/common.sh's confirm_or_exit. Only a literal y/Y/yes proceeds; anything
// else — including EOF on a non-interactive stdin — declines, so a piped invocation
// never writes without --assume-yes.
func confirm(what, contextName, scope, effect string, assumeYes bool, in io.Reader, out io.Writer) (bool, error) {
	fprintf(out, "About to APPLY %s.\n", what)
	fprintf(out, "  Target context: %s\n", contextName)
	fprintf(out, "  Scope:          %s\n", scope)
	fprintf(out, "%s\n", effect)
	if assumeYes {
		fprintf(out, "--assume-yes set — skipping confirmation.\n")
		return true, nil
	}
	fprintf(out, "Proceed? [y/N] ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	switch strings.TrimSpace(line) {
	case "y", "Y", "yes":
		return true, nil
	default:
		fprintln(out, "Aborted — no changes made.")
		return false, nil
	}
}
