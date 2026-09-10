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

	// The classic listener: a condition reason no other tier writes.
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

	// The producer half of the ownership check: a condition setter carrying both a
	// type and a reason, and the two writes the classic listener makes on the type
	// the enumeration below claims.
	versionSrc = `package listener

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
)

func setCondition(cfg Config, condType string, status metav1.ConditionStatus, reason, msg string) {
	cfg.Conditions.SetCondition(metav1.Condition{Type: condType, Reason: reason})
}

func (l *L) rejectedByGitHub(msg string) {
	setCondition(l.cfg, agcv1alpha1.ConditionRunnerVersionTooOld, metav1.ConditionTrue,
		agcv1alpha1.ReasonVersionTooOld, msg)
}

func (l *L) accepted() {
	setCondition(l.cfg, agcv1alpha1.ConditionRunnerVersionTooOld, metav1.ConditionFalse,
		agcv1alpha1.ReasonVersionAccepted, "accepted")
	setCondition(l.cfg, agcv1alpha1.ConditionDegraded, metav1.ConditionFalse,
		agcv1alpha1.ReasonListenerActive, "up")
}
`

	// The consumer half: the marked enumeration, and the one comparison against an
	// entry that is legitimate because it sits in the enumeration's own package.
	ownerSrc = `package runnercore

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/actions-gateway/github-actions-gateway/api/apiconditions"
)

func DropListenerCondition(prev *metav1.Condition, pushed metav1.Condition) bool {
	if pushed.Reason != apiconditions.ReasonVersionAccepted {
		return false
	}
	return prev != nil && !IsSessionSourcedRunnerVersion(prev.Reason)
}

func imageVerdict() metav1.Condition {
	return metav1.Condition{
		Type:   apiconditions.ConditionRunnerVersionTooOld,
		Reason: apiconditions.ReasonWorkerImageCurrent,
	}
}

// reasontiers:owns RunnerVersionTooOld internal/listener
func IsSessionSourcedRunnerVersion(reason string) bool {
	switch reason {
	case apiconditions.ReasonVersionTooOld, apiconditions.ReasonVersionAccepted:
		return true
	}
	return false
}
`

	goodLedger = "## Condition and Event tier reach\n\n" +
		"### Condition reasons\n\n" +
		"| Reason | Tier | Why |\n| --- | --- | --- |\n" +
		"| `ListenerActive` | Both | Ready on both arms. |\n" +
		"| `VersionTooOld` | Classic only | GitHub rejects only the classic session. |\n" +
		"| `VersionAccepted` | Classic only | Only the classic session baseline clears it. |\n" +
		"| `WorkerImageCurrent` | Both | The image reading asks GitHub nothing. |\n" +
		"| `WorkerCeilingReached` | Scale-set only | Only the queue holds assignments. |\n\n" +
		"### Event reasons\n\n" +
		"| Reason | Tier | Why |\n| --- | --- | --- |\n" +
		"| `WorkerPodStuckPending` | Both | The reaper is protocol-agnostic. |\n" +
		"| `WorkerCeilingReached` | Scale-set only | Expected backpressure on the queue. |\n" +
		"| `JobProvisionStalled` | Scale-set only | No held assignment on the classic tier. |\n\n" +
		"## Next\n"

	// The runbook half: every Event reason described where an operator looks.
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
		"internal/listener/version.go":          versionSrc,
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
	if findings := runCase(t, src, goodLedger, goodRunbook); len(findings) != 0 {
		t.Fatalf("aliased reasons should place as condition reasons; got %v", findings)
	}
}

// The complement: an identifier the file does not import is not a package, so a
// value reached through one is not the vocabulary's. QuotaExhausted is emitted
// nowhere and has no ledger row, so reading the qualifier as a package reports
// it as an unlisted reason.
func TestUnimportedQualifierIsNotAPackage(t *testing.T) {
	const shadowed = `package controller

func (r *R) stale(rs *RS, v2beta1 struct{ ReasonQuotaExhausted string }) {
	setCondition(rs, v2beta1.ReasonQuotaExhausted)
}
`
	src := srcTree(t, map[string]string{"internal/controller/shadowed.go": shadowed})
	findings := runCase(t, src, goodLedger, goodRunbook)
	if len(findings) != 0 {
		t.Fatalf("an unimported qualifier should be ignored; got %v", findings)
	}
}

// runCase writes both docs and returns the findings.
func runCase(t *testing.T, srcDir, ledger, runbook string) []string {
	t.Helper()
	dir := t.TempDir()
	lPath := filepath.Join(dir, "observability-metrics.md")
	rPath := filepath.Join(dir, "troubleshooting.md")
	writeFile(t, lPath, ledger)
	writeFile(t, rPath, runbook)
	findings, err := run(srcDir, apiTree(t), lPath, rPath)
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
	findings := runCase(t, srcTree(t, nil), goodLedger, goodRunbook)
	if len(findings) != 0 {
		t.Fatalf("expected no findings, got %v", findings)
	}
}

// The case Q850 exists for: a reason reaches one tier and the ledger never
// records it.
func TestConditionReasonMissingFromLedgerFails(t *testing.T) {
	ledger := strings.Replace(goodLedger,
		"| `VersionTooOld` | Classic only | GitHub rejects only the classic session. |\n", "", 1)
	findings := runCase(t, srcTree(t, nil), ledger, goodRunbook)
	requireFinding(t, findings, "VersionTooOld is a condition reason emitted from")
	requireFinding(t, findings, "which acquisition tier reaches it")
}

func TestEventReasonMissingFromLedgerFails(t *testing.T) {
	ledger := strings.Replace(goodLedger,
		"| `JobProvisionStalled` | Scale-set only | No held assignment on the classic tier. |\n", "", 1)
	findings := runCase(t, srcTree(t, nil), ledger, goodRunbook)
	requireFinding(t, findings, "JobProvisionStalled is a Event reason emitted from")
}

func TestLedgerRowWithoutAReasonFails(t *testing.T) {
	ledger := strings.Replace(goodLedger,
		"| `JobProvisionStalled` | Scale-set only | No held assignment on the classic tier. |\n",
		"| `JobProvisionStalled` | Scale-set only | No held assignment on the classic tier. |\n"+
			"| `LongGone` | Both | Removed last release. |\n", 1)
	findings := runCase(t, srcTree(t, nil), ledger, goodRunbook)
	requireFinding(t, findings, "LongGone is listed as a Event reason and the AGC emits no such reason")
}

// The stale-after-a-port direction: the source emits from the tier the ledger
// says is excluded.
func TestScaleSetOnlyClaimRefutedByAClassicSiteFails(t *testing.T) {
	classic := `package listener

import "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"

func (l *L) ceiling() {
	setCondition(l.cfg, v2alpha1.ReasonWorkerCeilingReached)
}
`
	src := srcTree(t, map[string]string{"internal/listener/ceiling.go": classic})
	findings := runCase(t, src, goodLedger, goodRunbook)
	requireFinding(t, findings, `WorkerCeilingReached is emitted here, and the ledger calls it "Scale-set only"`)
}

func TestClassicOnlyClaimRefutedByAScaleSetSiteFails(t *testing.T) {
	port := `package provisioner

import "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"

func (p *P) recover() {
	setCondition(v2alpha1.ReasonVersionTooOld)
}
`
	src := srcTree(t, map[string]string{"internal/provisioner/eviction_scaleset.go": port})
	findings := runCase(t, src, goodLedger, goodRunbook)
	requireFinding(t, findings, `eviction_scaleset.go: VersionTooOld is emitted here, and the ledger calls it "Classic only"`)
}

func TestSingleTierRowWithoutAReasonFails(t *testing.T) {
	ledger := strings.Replace(goodLedger,
		"| `VersionTooOld` | Classic only | GitHub rejects only the classic session. |",
		"| `VersionTooOld` | Classic only |  |", 1)
	findings := runCase(t, srcTree(t, nil), ledger, goodRunbook)
	requireFinding(t, findings, "with no reason")
}

func TestUnknownTierValueFails(t *testing.T) {
	ledger := strings.Replace(goodLedger, "| `ListenerActive` | Both |",
		"| `ListenerActive` | Mostly |", 1)
	findings := runCase(t, srcTree(t, nil), ledger, goodRunbook)
	requireFinding(t, findings, `has tier "Mostly"`)
}

// An Event an operator meets in kubectl describe and cannot look up is a tier
// with no remedy attached. Two shipped that way and this walk found them.
func TestEventReasonWithNoRunbookEntryFails(t *testing.T) {
	runbook := strings.Replace(goodRunbook,
		"| `JobProvisionStalled` | A job cannot register a runner name. |\n", "", 1)
	findings := runCase(t, srcTree(t, nil), goodLedger, runbook)
	requireFinding(t, findings, "JobProvisionStalled (recorded from")
	requireFinding(t, findings, "no runbook entry")
}

// The defect the first version of this scanner shipped: two recorder wrappers
// named recordEvent, whose reason sits at a different index in each. Keying on
// the name alone read the scale-set listener's action string as its reason.
func TestReasonIndexComesFromTheCalleesDeclaration(t *testing.T) {
	findings := runCase(t, srcTree(t, nil), goodLedger, goodRunbook)
	for _, f := range findings {
		if strings.Contains(f, "ProvisionWorker") {
			t.Fatalf("the action argument was read as a reason: %s", f)
		}
	}
	// And the reason one index earlier was found: dropping its row must fail.
	ledger := strings.Replace(goodLedger,
		"| `WorkerCeilingReached` | Scale-set only | Expected backpressure on the queue. |\n", "", 1)
	requireFinding(t, runCase(t, srcTree(t, nil), ledger, goodRunbook),
		"WorkerCeilingReached is a Event reason emitted from")
}

// A variadic recorder called without its varargs passes fewer arguments than
// its declaration has parameters. Counting the trailing `...` toward the arity
// made such a call match no signature, and it carries no corev1 event type when
// the type is computed — so it would have been skipped in silence.
func TestVariadicRecorderCalledWithoutVarargs(t *testing.T) {
	// The clean tree's only recordEvent call is this shape, so a regression
	// here shows up as its reason going missing.
	ledger := strings.Replace(goodLedger,
		"| `WorkerPodStuckPending` | Both | The reaper is protocol-agnostic. |\n", "", 1)
	requireFinding(t, runCase(t, srcTree(t, nil), ledger, goodRunbook),
		"WorkerPodStuckPending is a Event reason emitted from")

	// And with the varargs supplied, which is the other call shape.
	withArgs := strings.Replace(sharedSrc,
		`r.recordEvent(rs, corev1.EventTypeWarning, "WorkerPodStuckPending", "ReapWorkerPods", "n")`,
		`r.recordEvent(rs, corev1.EventTypeWarning, "WorkerPodStuckPending", "ReapWorkerPods", "%s", err)`, 1)
	src := srcTree(t, map[string]string{"internal/controller/runner_shared.go": withArgs})
	if findings := runCase(t, src, goodLedger, goodRunbook); len(findings) != 0 {
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
	findings := runCase(t, src, goodLedger, goodRunbook)
	requireFinding(t, findings, "Publish records an Event and matches no recorder signature")
}

// A computed reason nobody can name would sit in neither ledger.
func TestUnplaceableReasonArgumentFails(t *testing.T) {
	computed := `package controller

import corev1 "k8s.io/api/core/v1"

func (r *R) warn(rs *RS, err error) {
	r.recordEvent(rs, corev1.EventTypeWarning, deriveReason(err), "Act", "n")
}
`
	src := srcTree(t, map[string]string{"internal/controller/computed.go": computed})
	findings := runCase(t, src, goodLedger, goodRunbook)
	requireFinding(t, findings, "does not resolve to a name")
}

// A forwarder passes its caller's reason through and decides nothing, so it must
// not enter the inventory as a reason of its own.
func TestForwarderIsNotAnEmissionSite(t *testing.T) {
	findings := runCase(t, srcTree(t, nil), goodLedger, goodRunbook)
	for _, f := range findings {
		if strings.Contains(f, "reason is a Event reason") {
			t.Fatalf("a forwarded parameter was read as a reason: %s", f)
		}
	}
}

// The ledger is the gate's only input for the tier question, so its absence must
// be an error rather than a green run over zero rows.
func TestMissingLedgerSectionIsAnError(t *testing.T) {
	dir := t.TempDir()
	lPath := filepath.Join(dir, "observability-metrics.md")
	rPath := filepath.Join(dir, "troubleshooting.md")
	writeFile(t, lPath, "# Metrics\n\nNo ledger here.\n")
	writeFile(t, rPath, goodRunbook)
	if _, err := run(srcTree(t, nil), apiTree(t), lPath, rPath); err == nil {
		t.Fatal("expected an error when the ledger section is absent")
	}
}

// A scan that matched nothing looks exactly like a clean tree, so an empty
// inventory must refuse rather than report green.
func TestEmptySourceTreeIsAnError(t *testing.T) {
	dir := t.TempDir()
	lPath := filepath.Join(dir, "observability-metrics.md")
	rPath := filepath.Join(dir, "troubleshooting.md")
	writeFile(t, lPath, goodLedger)
	writeFile(t, rPath, goodRunbook)
	if _, err := run(t.TempDir(), apiTree(t), lPath, rPath); err == nil {
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
	findings := runCase(t, src, goodLedger, goodRunbook)
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

// --- ownership (Q994) ---------------------------------------------------------
//
// The green baseline for these is TestCleanTreeHasNoFindings: the fixture tree
// carries the marked enumeration and the two writes the listener makes on the
// type it claims, so each case below mutates exactly one of them.

// The defect the check exists for: the listener grows a third reason on the
// owned type and the enumeration is not updated with it, so every consumer reads
// it as the image reading's and overwrites a verdict GitHub actually made.
func TestOwnershipReasonMissingFromTheEnumerationFails(t *testing.T) {
	extra := strings.Replace(versionSrc,
		`		agcv1alpha1.ReasonVersionAccepted, "accepted")`,
		`		agcv1alpha1.ReasonVersionAccepted, "accepted")
	setCondition(l.cfg, agcv1alpha1.ConditionRunnerVersionTooOld, metav1.ConditionTrue,
		agcv1alpha1.ReasonQuotaExhausted, "policy")`, 1)
	src := srcTree(t, map[string]string{"internal/listener/version.go": extra})
	findings := runCase(t, src, goodLedger, goodRunbook)
	requireFinding(t, findings, "QuotaExhausted is published on RunnerVersionTooOld here")
	requireFinding(t, findings, "does not list it")
}

// The second half: the width came apart because a consumer asked the same
// question with a comparison of its own, so a reason added to the enumeration
// never reached it. Any such comparison outside the enumeration's package is a
// second membership site.
func TestOwnershipSecondMembershipSiteFails(t *testing.T) {
	consumer := `package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

func (r *R) defer_(prev metav1.Condition) bool {
	return prev.Status == metav1.ConditionTrue && prev.Reason == v2alpha1.ReasonVersionTooOld
}
`
	src := srcTree(t, map[string]string{"internal/controller/version.go": consumer})
	findings := runCase(t, src, goodLedger, goodRunbook)
	// The operator-visible value, not the constant name: it is what the
	// membership finding prints, and one vocabulary across both is what lets a
	// reader grep the two against each other.
	requireFinding(t, findings, "VersionTooOld is compared against here")
	requireFinding(t, findings, "second membership site at its own width")
}

// The complement, and the reason the exemption is by package rather than by
// function: DropListenerCondition compares against an entry to ask which push it
// is holding, which is not a membership test and is where the set is declared.
func TestOwnershipComparisonInTheEnumerationsOwnPackageIsFine(t *testing.T) {
	findings := runCase(t, srcTree(t, nil), goodLedger, goodRunbook)
	for _, f := range findings {
		if strings.Contains(f, "second membership site") {
			t.Fatalf("the enumeration's own package was read as a consumer: %s", f)
		}
	}
}

// The control the polarity requires: a fourth reason on the OTHER producer must
// stay out of the session set. Enumerating the image half is the failure
// direction the switch was written to reject, so this must not fire.
func TestOwnershipNewImageReasonDoesNotFire(t *testing.T) {
	extra := strings.Replace(ownerSrc,
		`		Reason: apiconditions.ReasonWorkerImageCurrent,`,
		`		Reason: apiconditions.ReasonQuotaExhausted,`, 1)
	src := srcTree(t, map[string]string{"internal/runnercore/runnerversion.go": extra})
	findings := runCase(t, src, goodLedger, goodRunbook)
	for _, f := range findings {
		if strings.Contains(f, "RunnerVersionTooOld") {
			t.Fatalf("an image-side reason was held to the session set: %s", f)
		}
	}
}

// A write on the owned type whose reason the scan cannot read is the emission
// that would slip past membership in silence, so it is a finding rather than a
// skip.
func TestOwnershipUnreadableEmissionFails(t *testing.T) {
	computed := strings.Replace(versionSrc,
		`		agcv1alpha1.ReasonVersionTooOld, msg)`,
		`		deriveReason(msg), msg)`, 1)
	src := srcTree(t, map[string]string{"internal/listener/version.go": computed})
	findings := runCase(t, src, goodLedger, goodRunbook)
	requireFinding(t, findings, "does not name a reason this scan can read")
}

// The marker is the check's only input, so a tree with none has to refuse: an
// enumeration whose marker was deleted looks exactly like a tree with nothing to
// own, and both read green.
func TestOwnershipWithNoMarkerIsAnError(t *testing.T) {
	unmarked := strings.Replace(ownerSrc, "// reasontiers:owns RunnerVersionTooOld internal/listener\n", "", 1)
	src := srcTree(t, map[string]string{"internal/runnercore/runnerversion.go": unmarked})
	dir := t.TempDir()
	lPath := filepath.Join(dir, "observability-metrics.md")
	rPath := filepath.Join(dir, "troubleshooting.md")
	writeFile(t, lPath, goodLedger)
	writeFile(t, rPath, goodRunbook)
	if _, err := run(src, apiTree(t), lPath, rPath); err == nil {
		t.Fatal("expected an error when no enumeration is marked")
	}
}

// A marker aimed at a subtree that writes nothing on the type gates nothing, and
// green there would be a gate reporting on an empty scan.
func TestOwnershipMarkerThatGatesNothingIsAnError(t *testing.T) {
	cases := map[string]string{
		"producer emits nothing on the type": strings.Replace(ownerSrc,
			"reasontiers:owns RunnerVersionTooOld internal/listener",
			"reasontiers:owns RunnerVersionTooOld internal/scalesetlistener", 1),
		"producer is not a directory": strings.Replace(ownerSrc,
			"reasontiers:owns RunnerVersionTooOld internal/listener",
			"reasontiers:owns RunnerVersionTooOld internal/nosuchthing", 1),
		"condition type is no constant's value": strings.Replace(ownerSrc,
			"reasontiers:owns RunnerVersionTooOld internal/listener",
			"reasontiers:owns RunnerVersionAncient internal/listener", 1),
		"the switch admits nothing readable": strings.Replace(ownerSrc,
			"	case apiconditions.ReasonVersionTooOld, apiconditions.ReasonVersionAccepted:\n		return true\n",
			"	case \"\":\n		return true\n", 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			src := srcTree(t, map[string]string{"internal/runnercore/runnerversion.go": body})
			dir := t.TempDir()
			lPath := filepath.Join(dir, "observability-metrics.md")
			rPath := filepath.Join(dir, "troubleshooting.md")
			writeFile(t, lPath, goodLedger)
			writeFile(t, rPath, goodRunbook)
			if _, err := run(src, apiTree(t), lPath, rPath); err == nil {
				t.Fatal("expected an error rather than a check over nothing")
			}
		})
	}
}

// A Condition literal that names the owned type and sets its Reason somewhere
// else is the emission the scan cannot read, so it must be a finding rather than
// a skip: skipping it is how a reason gets past membership in silence.
func TestOwnershipLiteralWithNoReasonFails(t *testing.T) {
	split := strings.Replace(versionSrc,
		`func (l *L) accepted() {`,
		`func (l *L) byPolicy(reasonFor func() string) {
	c := metav1.Condition{Type: agcv1alpha1.ConditionRunnerVersionTooOld}
	c.Reason = reasonFor()
	l.cfg.Conditions.SetCondition(c)
}

func (l *L) accepted() {`, 1)
	src := srcTree(t, map[string]string{"internal/listener/version.go": split})
	findings := runCase(t, src, goodLedger, goodRunbook)
	requireFinding(t, findings, "does not name a reason this scan can read")
}

// --- ownership: the spellings that used to slip past (review of #1890) --------
//
// Each case below reinstates the defect the check exists for, in a spelling the
// first version of the scanner did not read, and demands red. A gate whose
// silence is its verdict is only as good as the shapes it can see.

// The idiomatic Go spelling of the membership question, and the one the
// enumeration itself uses — so it is what a second reason makes somebody reach
// for, which is exactly the change this check exists to catch.
func TestOwnershipSwitchIsASecondMembershipSite(t *testing.T) {
	consumer := `package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

func (r *R) defer_(prev metav1.Condition) bool {
	switch prev.Reason {
	case v2alpha1.ReasonVersionTooOld:
		return true
	}
	return false
}
`
	src := srcTree(t, map[string]string{"internal/controller/version.go": consumer})
	requireFinding(t, runCase(t, src, goodLedger, goodRunbook), "VersionTooOld is compared against here")
}

// The same question asked with the string rather than the constant. It reads as
// an unrelated literal to a scan that only follows selectors.
func TestOwnershipStringLiteralIsASecondMembershipSite(t *testing.T) {
	consumer := `package controller

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

func (r *R) defer_(prev metav1.Condition) bool {
	return prev.Reason == "VersionTooOld"
}
`
	src := srcTree(t, map[string]string{"internal/controller/version.go": consumer})
	requireFinding(t, runCase(t, src, goodLedger, goodRunbook), "VersionTooOld is compared against here")
}

// An element of a []metav1.Condition literal has its type elided, so the node
// carries nothing naming it a condition. Admitting it on its keys is safe
// because the Type key still has to resolve to a claimed condition type.
func TestOwnershipElidedConditionLiteralIsScanned(t *testing.T) {
	elided := `package listener

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
)

func (l *L) baseline() []metav1.Condition {
	return []metav1.Condition{{
		Type:   agcv1alpha1.ConditionRunnerVersionTooOld,
		Status: metav1.ConditionTrue,
		Reason: agcv1alpha1.ReasonQuotaExhausted,
	}}
}
`
	src := srcTree(t, map[string]string{"internal/listener/elided.go": elided})
	requireFinding(t, runCase(t, src, goodLedger, goodRunbook), "QuotaExhausted is published on RunnerVersionTooOld here")
}

// A wrapper that pins the condition type and forwards the reason is not
// plumbing: it decides the type, and its callers decide reasons the scan would
// never see. Only a registered setter earns the forwarding exemption.
func TestOwnershipWrapperInsideTheProducerIsNotPlumbing(t *testing.T) {
	wrapper := `package listener

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
)

func reject(cfg Config, reason, msg string) {
	setCondition(cfg, agcv1alpha1.ConditionRunnerVersionTooOld, metav1.ConditionTrue, reason, msg)
}

func (l *L) byPolicy() { reject(l.cfg, agcv1alpha1.ReasonQuotaExhausted, "policy") }
`
	src := srcTree(t, map[string]string{"internal/listener/wrapper.go": wrapper})
	requireFinding(t, runCase(t, src, goodLedger, goodRunbook), "does not name a reason this scan can read")
}

// Hoisting the condition type into a local took the write out of scope in
// silence. Both directions: a non-member reason must be found, and the clean
// tree's own reasons must stay quiet.
func TestOwnershipConditionTypeInALocalResolves(t *testing.T) {
	local := `package listener

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
)

func (l *L) hoisted() {
	ct := agcv1alpha1.ConditionRunnerVersionTooOld
	setCondition(l.cfg, ct, metav1.ConditionTrue, agcv1alpha1.ReasonQuotaExhausted, "policy")
}
`
	src := srcTree(t, map[string]string{"internal/listener/hoisted.go": local})
	requireFinding(t, runCase(t, src, goodLedger, goodRunbook), "QuotaExhausted is published on RunnerVersionTooOld here")

	member := strings.Replace(local, "ReasonQuotaExhausted", "ReasonVersionTooOld", 1)
	src = srcTree(t, map[string]string{"internal/listener/hoisted.go": member})
	for _, f := range runCase(t, src, goodLedger, goodRunbook) {
		if strings.Contains(f, "is published on RunnerVersionTooOld here") {
			t.Fatalf("a listed reason reached through a local was reported: %s", f)
		}
	}
}

// A call is placed on name and argument count alone, so two setters sharing both
// while disagreeing on where their arguments sit would be read at the first
// one's indexes — a bogus finding or a silent skip. It refuses rather than
// guesses.
func TestOwnershipAmbiguousSetterShapeIsAnError(t *testing.T) {
	clash := `package provisioner

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

func setCondition(condType string, status metav1.ConditionStatus, reason, msg string, extra string) {
	_ = condType
}
`
	src := srcTree(t, map[string]string{"internal/provisioner/clash.go": clash})
	dir := t.TempDir()
	lPath := filepath.Join(dir, "observability-metrics.md")
	rPath := filepath.Join(dir, "troubleshooting.md")
	writeFile(t, lPath, goodLedger)
	writeFile(t, rPath, goodRunbook)
	if _, err := run(src, apiTree(t), lPath, rPath); err == nil {
		t.Fatal("expected an error rather than placing calls at one of two disagreeing shapes")
	}
}
