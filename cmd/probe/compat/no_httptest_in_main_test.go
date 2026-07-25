package compat_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// httptestImport is the package a production binary must never link. A
// net/http/httptest server is a test fixture; pulling one into a shipped
// artifact (via a stray import of a test double) would bloat the binary and its
// scanned dependency surface, and is exactly the mistake this gate prevents.
const httptestImport = "net/http/httptest"

// TestNoPackageMainReachesHTTPTest makes real the convention that compat.go (and
// the broker test doubles generally) rely on: no `package main` in the
// workspace may transitively import net/http/httptest in its compiled build
// graph. `go list -deps` reports a package's non-test dependencies, so a
// _test.go file importing httptest (as fakegithub's own tests do) is correctly
// ignored — only the shipped binary's imports count. A violation means a
// production binary linked a test server; the shared broker/brokerstub library
// exists precisely so a stub can be built without one.
func TestNoPackageMainReachesHTTPTest(t *testing.T) {
	root := repoRoot(t)
	for _, modDir := range workspaceModules(t, root) {
		for _, mainPkg := range mainPackages(t, modDir) {
			deps := packageDeps(t, modDir, mainPkg)
			if slices.Contains(deps, httptestImport) {
				t.Errorf("package main %q transitively imports %s — a production binary must not link a test server; build the stub on broker/brokerstub instead",
					mainPkg, httptestImport)
			}
		}
	}
}

// repoRoot walks up from the test's working directory to the directory holding
// go.work, which is the workspace root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.work found walking up from the test directory")
		}
		dir = parent
	}
}

// workspaceModules returns the absolute directory of every module in the
// workspace, read from `go work edit -json` so the list cannot drift from
// go.work.
func workspaceModules(t *testing.T, root string) []string {
	t.Helper()
	out := runGo(t, root, "work", "edit", "-json")
	var work struct {
		Use []struct {
			DiskPath string `json:"DiskPath"`
		} `json:"Use"`
	}
	if err := json.Unmarshal([]byte(out), &work); err != nil {
		t.Fatalf("parse `go work edit -json`: %v", err)
	}
	dirs := make([]string, 0, len(work.Use))
	for _, u := range work.Use {
		dirs = append(dirs, filepath.Join(root, filepath.FromSlash(u.DiskPath)))
	}
	if len(dirs) == 0 {
		t.Fatal("go.work lists no modules")
	}
	return dirs
}

// mainPackages returns the import paths of the `package main` packages in the
// module rooted at modDir (its default, non-tagged build).
func mainPackages(t *testing.T, modDir string) []string {
	t.Helper()
	out := runGo(t, modDir, "list", "-f", `{{if eq .Name "main"}}{{.ImportPath}}{{end}}`, "./...")
	var mains []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			mains = append(mains, line)
		}
	}
	return mains
}

// packageDeps returns the transitive non-test dependency import paths of pkg,
// resolved in the module rooted at modDir.
func packageDeps(t *testing.T, modDir, pkg string) []string {
	t.Helper()
	out := runGo(t, modDir, "list", "-deps", pkg)
	return strings.Split(strings.TrimSpace(out), "\n")
}

// runGo runs a `go` subcommand in dir and returns its stdout, failing the test
// on any error.
func runGo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	//nolint:gosec // G204: args are hardcoded `go` subcommands plus import paths derived from go.work in this test-only build gate, never user input.
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		t.Fatalf("go %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, stderr)
	}
	return string(out)
}
