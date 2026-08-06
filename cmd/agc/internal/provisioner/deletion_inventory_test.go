package provisioner

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The specs half of the deletion boundary (Q599). deletion.go states the obligation a
// new delete path owes the code — stamp, or be ordered after the container's exit —
// and nothing extended it to the tests that pin the same boundary. #1032 gave the
// Q501 actuator a delete of a cancelled run's worker while
// E2E_GitHub_CancelledRunLeavesNoDeletionMark still asserted that nothing ever deleted
// one; that spec skips without live-GitHub credentials, so no gate went red and the
// contradiction shipped.
//
// The inventory below closes the gap the only way a credential-gated spec can be
// defended by a gate that cannot run it: it makes the delete paths themselves the
// tripwire. Adding, moving, or renaming a client Delete anywhere in the AGC fails
// this test, and the failure prints every spec that pins the boundary along with
// which of them skip — so their green says nothing.

// deleteSite names one client Delete call the way a reviewer would: the module-relative
// file and the enclosing function.
type deleteSite struct {
	file string
	fn   string
}

func (s deleteSite) String() string { return s.file + " " + s.fn }

// declaredDeleteSites is every client Delete in the `agc` module, with how each one
// stays outside Q502's graceful-deletion recovery. Keep it exhaustive rather than
// pod-only: "not a worker pod" is a judgement the reviewer of a new delete has to
// make, and recording it is what makes the omission of a real one visible.
var declaredDeleteSites = map[deleteSite]string{
	{"internal/provisioner/completion.go", "(*Provisioner).reclaimAbandonedWorker"}: "worker pod — stamped deletion-reason=job_abandoned before the delete (Q501)",
	{"internal/provisioner/completion.go", "(*Provisioner).deletePod"}:              "worker pod, unstamped — the completedPodTTL=0 cleanup of a pod that has already reached its terminal phase, so the deletion request lands after the container's recorded exit and externallyDeletedBeforeTerminal rejects it",
	{"internal/provisioner/completion.go", "(*Provisioner).deleteSecret"}:           "job Secret, not a worker pod",
	{"internal/controller/runner_shared.go", "(reapTarget).delete"}:                 "worker pod — stamped with the reap reason before the delete (Q502)",
	{"internal/agentpool/adopt.go", "adoptOneLegacySecret"}:                         "agent Secret, not a worker pod",
	{"internal/agentpool/pool.go", "(*Pool).EnsureAgents"}:                          "agent Secret, not a worker pod",
	{"internal/agentpool/pool.go", "(*Pool).DeleteAll"}:                             "agent Secret, not a worker pod",
}

// boundarySpec is a test that pins the deletion boundary, and whether it can run
// unattended.
type boundarySpec struct {
	name       string
	path       string
	credGated  bool
	whatItPins string
}

// boundarySpecs is the roster a delete-path change has to be read against. Each entry
// is verified to still exist at its path below, so the pointer cannot rot into a name
// nothing answers to.
var boundarySpecs = []boundarySpec{
	{
		name:       "E2E_GitHub_CancelledRunLeavesNoDeletionMark",
		path:       "../../../gmc/test/e2e/github_e2e_test.go",
		credGated:  true,
		whatItPins: "a human cancel at GitHub stays distinguishable from a drain at the pod — the assertion #1032 invalidated",
	},
	{
		name:       "E2E_AGC_ScaleSetRecovery",
		path:       "../../../gmc/test/e2e/worker_scaleset_recovery_test.go",
		whatItPins: "scale-set recovery under the chart's shipped RBAC, on a real kubelet (Q519)",
	},
	{
		name:       "E2E_AGC_WorkerNodeDrain",
		path:       "../../../gmc/test/e2e/worker_drain_test.go",
		whatItPins: "a drained worker that never ran its container is left unrecovered (Q421)",
	},
	{
		name:       "TestAGC_Drain_ClassicWorkerTerminalWithMark_Reruns",
		path:       "../controller/integration/drain_eviction_test.go",
		whatItPins: "the recovered side of the boundary, classic tier",
	},
	{
		name:       "TestAGC_Drain_ScaleSetWorkerTerminalWithMark_Recovers",
		path:       "../controller/integration/drain_eviction_test.go",
		whatItPins: "the recovered side of the boundary, scale-set tier",
	},
	{
		name:       "TestAGC_Drain_ClassicWorkerEviction_DoesNotRerun",
		path:       "../controller/integration/drain_eviction_test.go",
		whatItPins: "the unrecovered side of the boundary, classic tier",
	},
	{
		name:       "TestAGC_Drain_ScaleSetWorkerEviction_DoesNotRecover",
		path:       "../controller/integration/drain_eviction_test.go",
		whatItPins: "the unrecovered side of the boundary, scale-set tier",
	},
	{
		name:       "E2E_AGC_PreemptedWorkerIsRecovered",
		path:       "../../../gmc/test/e2e/worker_preemption_test.go",
		whatItPins: "recovery of a delete-driven disruption that detection deliberately does NOT key on the mark (Q497), sampling the mark alongside",
	},
	{
		name:       "E2E_AGC_CompletedPodReaped",
		path:       "../../../gmc/test/e2e/worker_lifecycle_test.go",
		whatItPins: "the reaper's stamp-then-delete path end to end, on a completed worker",
	},
	{
		name:       "E2E_AGC_StuckPendingPodReaped",
		path:       "../../../gmc/test/e2e/worker_lifecycle_test.go",
		whatItPins: "the reaper's stamp-then-delete path on a worker that never started",
	},
}

// deletionMarkTokens are the spellings a test uses when it reads the discriminator the
// boundary turns on.
var deletionMarkTokens = []string{
	"DeletionTimestamp",
	"deletionTimestamp",
	"AnnotationDeletionReason",
	"deletion-reason",
}

// heavyTierRoots are the test trees the roster is swept for completeness. Unit tests
// are out of scope on purpose: they run on every `make check`, so a delete path that
// breaks one goes red on its own and needs no pointer. These trees are the ones whose
// green can simply be absent — skipped for want of credentials, or gated behind a
// workflow path filter.
var heavyTierRoots = []string{
	"../../../gmc/test/e2e",
	"../controller/integration",
	"../../../gmc/internal/controller/integration",
}

// nonBoundaryFiles are heavy-tier test files that name the deletion mark without
// asserting the deletion boundary. Each needs a reason, because "this one does not
// count" is the judgement a sweep exists to make explicit.
var nonBoundaryFiles = map[string]string{
	"../../../gmc/test/e2e/e2e_test.go":                                      "a jsonpath readiness template filters out pods that are mid-delete; no assertion about the mark",
	"../controller/integration/q260_late_redelivery_test.go":                 "filters live worker pods with DeletionTimestamp.IsZero(); the subject is delivery dedup",
	"../../../gmc/internal/controller/integration/v2_teardown_test.go":       "asserts an ActionsGateway CR is mid-deletion; CR finalizers, not worker pods",
	"../../../gmc/internal/controller/integration/priorityclass_vap_test.go": "asserts a RunnerGroup CR is held deleting by its finalizer; an admission-policy subject",
}

// TestDeletePathInventory_MatchesDeclaredSites fails when the AGC's set of client
// Delete calls differs from the inventory above, and prints the specs the change has
// to be read against.
func TestDeletePathInventory_MatchesDeclaredSites(t *testing.T) {
	found := scanDeleteSites(t, "../..")

	var undeclared, missing []string
	for site := range found {
		if _, ok := declaredDeleteSites[site]; !ok {
			undeclared = append(undeclared, site.String())
		}
	}
	for site := range declaredDeleteSites {
		if _, ok := found[site]; !ok {
			missing = append(missing, site.String())
		}
	}
	sort.Strings(undeclared)
	sort.Strings(missing)

	if len(undeclared) == 0 && len(missing) == 0 {
		return
	}
	t.Fatalf("the AGC's delete paths moved:\n%s%s\n%s",
		block("client Delete calls not in declaredDeleteSites", undeclared),
		block("declaredDeleteSites entries with no matching call", missing),
		"Update declaredDeleteSites to match, recording how each new delete stays outside\n"+
			"Q502 recovery.\n"+specRoster())
}

// TestDeletePathInventory_BoundarySpecsExist keeps the roster honest: a spec renamed
// or moved out from under it would otherwise leave the failure above naming tests that
// no longer exist.
func TestDeletePathInventory_BoundarySpecsExist(t *testing.T) {
	for _, s := range boundarySpecs {
		src, err := os.ReadFile(s.path)
		require.NoError(t, err, "%s: boundary spec file is gone; update boundarySpecs", s.name)
		// Whole-word, not substring: renaming a spec by appending a suffix leaves the
		// old name as a prefix of the new one, and a Contains check would call that
		// present.
		found := regexp.MustCompile(`\b` + regexp.QuoteMeta(s.name) + `\b`).MatchString(string(src))
		require.True(t, found,
			"%s is no longer in %s; update boundarySpecs so the delete-path failure keeps naming a real spec", s.name, s.path)
	}
}

// TestDeletePathInventory_BoundaryRosterIsComplete sweeps the heavy tiers for test
// files that read the deletion mark and requires each to be accounted for: either it
// contributes a spec to the roster, or it is written off in nonBoundaryFiles with a
// reason. Without this the roster is only as complete as whoever last read the design
// docs, which is the decay Q599 is about.
//
// It classifies at file granularity, not per spec. A brand-new test file that touches
// the boundary is the case it catches; a new spec added to a file already in the
// roster is not, and the roster's own entry is what points a reader into that file.
func TestDeletePathInventory_BoundaryRosterIsComplete(t *testing.T) {
	accounted := map[string]bool{}
	for _, s := range boundarySpecs {
		accounted[filepath.Clean(s.path)] = true
	}
	for path := range nonBoundaryFiles {
		accounted[filepath.Clean(path)] = true
	}

	// Collect first, read after: reading inside the WalkDir callback is the
	// symlink-TOCTOU shape gosec's G122 rejects.
	var candidates []string
	for _, root := range heavyTierRoots {
		require.DirExists(t, root, "heavy-tier root moved; update heavyTierRoots")
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(path, "_test.go") {
				candidates = append(candidates, path)
			}
			return err
		})
		require.NoError(t, err)
	}

	var unclassified, scanned []string
	for _, path := range candidates {
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		if !containsAny(string(src), deletionMarkTokens) {
			continue
		}
		scanned = append(scanned, path)
		if !accounted[filepath.Clean(path)] {
			unclassified = append(unclassified, path)
		}
	}
	require.NotEmpty(t, scanned, "the sweep matched no heavy-tier file at all; it is measuring nothing")

	// A path that no longer holds a boundary file leaves the roster pointing at
	// nothing, the same rot the existence check guards against from the other side.
	for path := range nonBoundaryFiles {
		require.FileExists(t, path, "nonBoundaryFiles names a file that is gone; drop the entry")
	}

	if len(unclassified) == 0 {
		return
	}
	sort.Strings(unclassified)
	t.Fatalf("a heavy-tier test file reads the deletion mark and is in neither list:\n%s\n%s",
		block("unclassified", unclassified),
		"Decide which it is. If it asserts the boundary, add its spec to boundarySpecs so a\n"+
			"delete-path change hands it to the reviewer. If it only mentions the mark in\n"+
			"passing, add it to nonBoundaryFiles with the reason.\n"+specRoster())
}

func containsAny(s string, tokens []string) bool {
	for _, tok := range tokens {
		if strings.Contains(s, tok) {
			return true
		}
	}
	return false
}

func block(title string, items []string) string {
	if len(items) == 0 {
		return ""
	}
	return "\n  " + title + ":\n    " + strings.Join(items, "\n    ") + "\n"
}

// specRoster is the point of the whole test: what a reviewer must read before
// accepting the inventory update the failure demands.
func specRoster() string {
	var b strings.Builder
	b.WriteString("\nRead every spec that pins the deletion boundary\n")
	b.WriteString("(docs/design/04-operational-flows.md §4.2). A delete path changes what a worker\n")
	b.WriteString("pod publishes at its terminal phase, which is exactly what these assert:\n\n")
	for _, s := range boundarySpecs {
		gate := "      "
		if s.credGated {
			gate = "[CRED]"
		}
		fmt.Fprintf(&b, "  %s %s\n           %s\n           %s\n", gate, s.name, s.path, s.whatItPins)
	}
	b.WriteString("\n  [CRED] means the spec SKIPS without live-GitHub credentials. No gate runs it,\n")
	b.WriteString("  so a green CI run is not evidence its assertion still holds — read the\n")
	b.WriteString("  assertion by hand against your delete. Q599 is the case: #1032 made a\n")
	b.WriteString("  cancelled run's worker deletable while the spec still asserted nothing ever\n")
	b.WriteString("  deleted one, and nothing went red.\n")
	return b.String()
}

// scanDeleteSites finds every client Delete call under root — a Delete selector taking
// a context and an object. A sync.Map or a prometheus vec Delete takes one non-context
// argument and is not one.
//
// It keys on the call rather than on any wrapper, so a new delete reached through a
// helper of any name still lands here.
func scanDeleteSites(t *testing.T, root string) map[deleteSite]struct{} {
	t.Helper()
	sites := map[deleteSite]struct{}{}
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "vendor" || name == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				if isClientDelete(n) {
					sites[deleteSite{filepath.ToSlash(rel), funcLabel(fd)}] = struct{}{}
				}
				return true
			})
		}
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, sites, "the scanner found no Delete calls at all; it is measuring nothing")
	return sites
}

func isClientDelete(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) < 2 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Delete" {
		return false
	}
	return isContextArg(call.Args[0])
}

func isContextArg(arg ast.Expr) bool {
	switch a := arg.(type) {
	case *ast.Ident:
		return strings.Contains(strings.ToLower(a.Name), "ctx") ||
			strings.Contains(strings.ToLower(a.Name), "context")
	case *ast.CallExpr:
		// context.Background(), context.WithoutCancel(ctx), …
		if sel, ok := a.Fun.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok {
				return pkg.Name == "context"
			}
		}
	}
	return false
}

func funcLabel(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	var recv strings.Builder
	if err := printType(&recv, fd.Recv.List[0].Type); err != nil {
		return fd.Name.Name
	}
	return "(" + recv.String() + ")." + fd.Name.Name
}

func printType(b *strings.Builder, e ast.Expr) error {
	switch t := e.(type) {
	case *ast.Ident:
		b.WriteString(t.Name)
	case *ast.StarExpr:
		b.WriteString("*")
		return printType(b, t.X)
	default:
		return fmt.Errorf("unsupported receiver type %T", e)
	}
	return nil
}
