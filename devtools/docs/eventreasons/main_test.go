package main

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// A synthetic AGC tree and the two docs that describe it, every check green.
// Each test mutates exactly one input and asserts the finding it should produce,
// so every case starts from a green baseline: a check that stopped firing would
// otherwise be indistinguishable from a fixture that never tripped it.
//
// The tree reproduces the shapes the real source uses, and the ones that broke
// the first version of this scanner: two recorder wrappers of the same name with
// their reason at different indexes, a reason forwarded as a parameter, and a
// reason chosen by assigning literals to a local.
const (
	apiSrc = `package apiconditions

const (
	ConditionRunnerVersionTooOld = "RunnerVersionTooOld"
	ConditionDegraded = "Degraded"

	ReasonListenerActive = "ListenerActive"
	ReasonWorkerCeilingReached = "WorkerCeilingReached"
	ReasonVersionTooOld = "VersionTooOld"
	ReasonVersionAccepted = "VersionAccepted"
	ReasonWorkerImageCurrent = "WorkerImageCurrent"
	ReasonQuotaExhausted = "QuotaExhausted"
)
`
	aliasSrc = `package v2alpha1

import "github.com/actions-gateway/github-actions-gateway/api/apiconditions"

const (
	ConditionRunnerVersionTooOld = apiconditions.ConditionRunnerVersionTooOld
	ConditionDegraded = apiconditions.ConditionDegraded

	ReasonListenerActive = apiconditions.ReasonListenerActive
	ReasonWorkerCeilingReached = apiconditions.ReasonWorkerCeilingReached
	ReasonVersionTooOld = apiconditions.ReasonVersionTooOld
	ReasonVersionAccepted = apiconditions.ReasonVersionAccepted
	ReasonWorkerImageCurrent = apiconditions.ReasonWorkerImageCurrent
	ReasonQuotaExhausted = apiconditions.ReasonQuotaExhausted
)
`

	// The shared reconciler: a six-argument recorder interface, a wrapper that
	// forwards its own reason parameter, and one literal reason of its own.
	sharedSrc = `package controller

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

type EventRecorder interface {
	Event(namespace, name, eventtype, reason, action, note string)
}

func (r *R) recordEvent(rs *RS, eventtype, reason, action, note string, args ...any) {
	r.Recorder.Event(rs.Namespace, rs.Name, eventtype, reason, action, note)
}

func (r *R) ready(rs *RS) {
	setCondition(rs, v2alpha1.ReasonListenerActive)
	r.recordEvent(rs, corev1.EventTypeWarning, "WorkerPodStuckPending", "ReapWorkerPods", "n")
}
`

	// A condition reason written from another package.
	listenerSrc = `package listener

import agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"

func (l *L) rejected() {
	setCondition(l.cfg, agcv1alpha1.ReasonVersionTooOld)
}
`

	// The scale-set listener: a four-argument recordEvent whose reason sits one
	// place earlier, and a reason chosen by assigning literals to a local. Keying
	// the index on the function name alone read "ProvisionWorker" as the reason
	// here and missed both of these.
	scaleSetSrc = `package scalesetlistener

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

func (l *Listener) recordEvent(eventtype, reason, action, note string) {
	l.cfg.Events.Event(eventtype, reason, action, note)
}

func (l *Listener) stalled(stalled bool) {
	setCondition(v2alpha1.ReasonWorkerCeilingReached)
	eventType, eventReason := corev1.EventTypeNormal, "WorkerCeilingReached"
	if stalled {
		eventType, eventReason = corev1.EventTypeWarning, "JobProvisionStalled"
	}
	l.recordEvent(eventType, eventReason, "ProvisionWorker", "n")
}
`

	// The image reading, which publishes on RunnerVersionTooOld from the reconciler.
	ownerSrc = `package runnercore

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/actions-gateway/github-actions-gateway/api/apiconditions"
)

func imageVerdict() metav1.Condition {
	return metav1.Condition{
		Type:   apiconditions.ConditionRunnerVersionTooOld,
		Reason: apiconditions.ReasonWorkerImageCurrent,
	}
}
`

	// Every Event reason described where an operator looks.
	goodRunbook = "# Troubleshooting\n\n" +
		"| Reason | Meaning |\n| --- | --- |\n" +
		"| `WorkerPodStuckPending` | A pod outlived pendingPodDeadline. |\n" +
		"| `WorkerCeilingReached` | The set is at its worker ceiling. |\n" +
		"| `JobProvisionStalled` | A job cannot register a runner name. |\n"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// srcTree writes the synthetic AGC source, applying any per-file overrides. An
// override with an empty body drops the file.
func srcTree(t *testing.T, overrides map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"internal/controller/runner_shared.go":  sharedSrc,
		"internal/listener/session.go":          listenerSrc,
		"internal/runnercore/runnerversion.go":  ownerSrc,
		"internal/scalesetlistener/listener.go": scaleSetSrc,
	}
	for name, body := range overrides {
		files[name] = body
	}
	for name, body := range files {
		if body == "" {
			continue
		}
		writeFile(t, filepath.Join(dir, name), body)
	}
	return dir
}

// apiTree writes the shared reason vocabulary and its v2 re-export.
func apiTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "apiconditions/conditions.go"), apiSrc)
	writeFile(t, filepath.Join(dir, "v2alpha1/conditions.go"), aliasSrc)
	return dir
}

// A reason reaches the scanner through whatever identifier the importing file
// chose, so the package — not that identifier — is what reasonPkgs names. Keying
// on the identifier left every GMC recorder call unplaceable, because the GMC
// imports this vocabulary as gmcv2alpha1 (Q925).
func TestReasonReachedThroughAnImportAliasResolves(t *testing.T) {
	const aliased = `package controller

import (
	corev1 "k8s.io/api/core/v1"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

func (r *R) degraded(rs *RS) {
	reason := gmcv2alpha1.ReasonWorkerCeilingReached
	r.recordEvent(rs, corev1.EventTypeWarning, reason, "Reconcile", "n")
	r.recordEvent(rs, corev1.EventTypeWarning, gmcv2alpha1.ReasonListenerActive, "Reconcile", "n")
}
`
	src := srcTree(t, map[string]string{"internal/controller/aliased.go": aliased})
	if findings := runCase(t, src, goodRunbook); len(findings) != 0 {
		t.Fatalf("aliased reasons should place as condition reasons; got %v", findings)
	}
}

// The AGC imports the vocabulary from api/v2 (Q1263), whose last path segment is
// v2, so that package must be one reasonPkgs names or every AGC reason goes
// unplaced.
func TestReasonReachedThroughTheV2PackageResolves(t *testing.T) {
	const v2src = `package controller

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/actions-gateway/github-actions-gateway/api/v2"
)

func (r *R) degraded(rs *RS) {
	reason := v2.ReasonWorkerCeilingReached
	r.recordEvent(rs, corev1.EventTypeWarning, reason, "Reconcile", "n")
	r.recordEvent(rs, corev1.EventTypeWarning, v2.ReasonListenerActive, "Reconcile", "n")
}
`
	src := srcTree(t, map[string]string{"internal/controller/v2.go": v2src})
	if findings := runCase(t, src, goodRunbook); len(findings) != 0 {
		t.Fatalf("reasons read through api/v2 should place as condition reasons; got %v", findings)
	}
}

// The complement: an identifier the file does not import is not a package, so a
// value reached through one is not the vocabulary's.
func TestUnimportedQualifierIsNotAPackage(t *testing.T) {
	const shadowed = `package controller

func (r *R) stale(rs *RS, v2beta1 struct{ ReasonQuotaExhausted string }) {
	setCondition(rs, v2beta1.ReasonQuotaExhausted)
}
`
	src := srcTree(t, map[string]string{"internal/controller/shadowed.go": shadowed})
	findings := runCase(t, src, goodRunbook)
	if len(findings) != 0 {
		t.Fatalf("an unimported qualifier should be ignored; got %v", findings)
	}
}

// runCase writes the runbook and returns the findings.
func runCase(t *testing.T, srcDir, runbook string) []string {
	t.Helper()
	rPath := filepath.Join(t.TempDir(), "troubleshooting.md")
	writeFile(t, rPath, runbook)
	findings, err := run(srcDir, apiTree(t), rPath)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return findings
}

func requireFinding(t *testing.T, findings []string, want string) {
	t.Helper()
	for _, f := range findings {
		if strings.Contains(f, want) {
			return
		}
	}
	t.Fatalf("no finding containing %q; got %v", want, findings)
}

func TestCleanTreeHasNoFindings(t *testing.T) {
	findings := runCase(t, srcTree(t, nil), goodRunbook)
	if len(findings) != 0 {
		t.Fatalf("expected no findings, got %v", findings)
	}
}

// An Event an operator meets in kubectl describe and cannot look up has no remedy
// attached. Two shipped that way and the first walk found them.
func TestEventReasonWithNoRunbookEntryFails(t *testing.T) {
	runbook := strings.Replace(goodRunbook,
		"| `JobProvisionStalled` | A job cannot register a runner name. |\n", "", 1)
	findings := runCase(t, srcTree(t, nil), runbook)
	requireFinding(t, findings, "JobProvisionStalled (recorded from")
	requireFinding(t, findings, "no runbook entry")
}

// The defect the first version of this scanner shipped: two recorder wrappers
// named recordEvent, whose reason sits at a different index in each. Keying on
// the name alone read the scale-set listener's action string as its reason.
func TestReasonIndexComesFromTheCalleesDeclaration(t *testing.T) {
	findings := runCase(t, srcTree(t, nil), goodRunbook)
	for _, f := range findings {
		if strings.Contains(f, "ProvisionWorker") {
			t.Fatalf("the action argument was read as a reason: %s", f)
		}
	}
	// And the reason one index earlier was found: dropping its entry must fail.
	runbook := strings.Replace(goodRunbook,
		"| `WorkerCeilingReached` | The set is at its worker ceiling. |\n", "", 1)
	requireFinding(t, runCase(t, srcTree(t, nil), runbook), "WorkerCeilingReached (recorded from")
}

// A variadic recorder called without its varargs passes fewer arguments than
// its declaration has parameters. Counting the trailing `...` toward the arity
// made such a call match no signature, and it carries no corev1 event type when
// the type is computed — so it would have been skipped in silence.
func TestVariadicRecorderCalledWithoutVarargs(t *testing.T) {
	// The clean tree's only recordEvent call is this shape, so a regression
	// here shows up as its reason going missing.
	runbook := strings.Replace(goodRunbook,
		"| `WorkerPodStuckPending` | A pod outlived pendingPodDeadline. |\n", "", 1)
	requireFinding(t, runCase(t, srcTree(t, nil), runbook), "WorkerPodStuckPending (recorded from")

	// And with the varargs supplied, which is the other call shape.
	withArgs := strings.Replace(sharedSrc,
		`r.recordEvent(rs, corev1.EventTypeWarning, "WorkerPodStuckPending", "ReapWorkerPods", "n")`,
		`r.recordEvent(rs, corev1.EventTypeWarning, "WorkerPodStuckPending", "ReapWorkerPods", "%s", err)`, 1)
	src := srcTree(t, map[string]string{"internal/controller/runner_shared.go": withArgs})
	if findings := runCase(t, src, goodRunbook); len(findings) != 0 {
		t.Fatalf("expected no findings, got %v", findings)
	}
}

// A recorder the scanner cannot read must fail loudly rather than pass over the
// reasons it emits.
func TestUnrecognizedRecorderFails(t *testing.T) {
	stray := `package controller

import corev1 "k8s.io/api/core/v1"

func (r *R) warn(rs *RS) {
	r.Sink.Publish(rs, corev1.EventTypeWarning, "SomethingNew", "Act", "n")
}
`
	src := srcTree(t, map[string]string{"internal/controller/stray.go": stray})
	findings := runCase(t, src, goodRunbook)
	requireFinding(t, findings, "Publish records an Event and matches no recorder signature")
}

// A computed reason nobody can name cannot be looked up.
func TestUnplaceableReasonArgumentFails(t *testing.T) {
	computed := `package controller

import corev1 "k8s.io/api/core/v1"

func (r *R) warn(rs *RS, err error) {
	r.recordEvent(rs, corev1.EventTypeWarning, deriveReason(err), "Act", "n")
}
`
	src := srcTree(t, map[string]string{"internal/controller/computed.go": computed})
	findings := runCase(t, src, goodRunbook)
	requireFinding(t, findings, "does not resolve to a name")
}

// A forwarder passes its caller's reason through and decides nothing, so it must
// not enter the inventory as a reason of its own.
func TestForwarderIsNotAnEmissionSite(t *testing.T) {
	findings := runCase(t, srcTree(t, nil), goodRunbook)
	for _, f := range findings {
		if strings.Contains(f, "reason is a Event reason") {
			t.Fatalf("a forwarded parameter was read as a reason: %s", f)
		}
	}
}

// A scan that matched nothing looks exactly like a clean tree, so an empty
// inventory must refuse rather than report green.
func TestEmptySourceTreeIsAnError(t *testing.T) {
	rPath := filepath.Join(t.TempDir(), "troubleshooting.md")
	writeFile(t, rPath, goodRunbook)
	if _, err := run(t.TempDir(), apiTree(t), rPath); err == nil {
		t.Fatal("expected an error when the source tree emits no reasons")
	}
}

// Test files name reasons that are never shipped, so they must not enter the
// inventory.
func TestTestFilesAreNotScanned(t *testing.T) {
	stray := `package listener

import corev1 "k8s.io/api/core/v1"

func TestX(t *T) {
	r.recordEvent(rs, corev1.EventTypeWarning, "OnlyInTests", "Act", "n")
}
`
	src := srcTree(t, map[string]string{"internal/listener/session_test.go": stray})
	findings := runCase(t, src, goodRunbook)
	if len(findings) != 0 {
		t.Fatalf("expected no findings, got %v", findings)
	}
}

// The release pre-flight diffs list's output between two refs (Q780), so it
// enumerates both kinds and its ordering is the one `comm` needs.
func TestListEnumeratesBothKinds(t *testing.T) {
	lines, err := list(srcTree(t, nil), apiTree(t))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var conds, events []string
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "condition "):
			conds = append(conds, strings.TrimPrefix(l, "condition "))
		case strings.HasPrefix(l, "event "):
			events = append(events, strings.TrimPrefix(l, "event "))
		default:
			t.Fatalf("line carries no kind: %q", l)
		}
	}
	if len(conds) == 0 || len(events) == 0 {
		t.Fatalf("expected both kinds, got %d condition and %d event lines", len(conds), len(events))
	}
	if !sort.StringsAreSorted(lines) {
		t.Fatalf("output is not sorted, so a set diff of it would be wrong: %v", lines)
	}
	if !slices.Contains(events, "WorkerPodStuckPending") {
		t.Fatalf("expected the shared reconciler's Event reason, got %v", events)
	}
	if !slices.Contains(conds, "ListenerActive") {
		t.Fatalf("expected the shared reconciler's condition reason, got %v", conds)
	}
}

// A caller diffing two refs cannot tell a short list from an honest one, so
// every way the scan comes back incomplete is an error rather than a shorter
// enumeration. An unreadable ref would otherwise report every reason as new.
func TestListRefusesToBeShort(t *testing.T) {
	computed := `package controller

import corev1 "k8s.io/api/core/v1"

func (r *R) warn(rs *RS, err error) {
	r.recordEvent(rs, corev1.EventTypeWarning, deriveReason(err), "Act", "n")
}
`
	cases := map[string]string{
		"empty tree":         "",
		"unplaceable reason": computed,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			if body != "" {
				src = srcTree(t, map[string]string{"internal/controller/computed.go": body})
			}
			if _, err := list(src, apiTree(t)); err == nil {
				t.Fatal("expected an error rather than a short enumeration")
			}
		})
	}
}
