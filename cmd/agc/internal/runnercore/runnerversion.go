package runnercore

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnerimage"
	"github.com/actions-gateway/github-actions-gateway/agc/names"
	"github.com/actions-gateway/github-actions-gateway/api/apiconditions"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkerImageRunnerVersion reports the actions/runner version a worker image
// reference declares, and whether it declared one at all. Only a tag shaped like a
// runner version (MAJOR.MINOR.PATCH, with an optional leading "v") counts:
//
//	ghcr.io/actions/actions-runner:2.335.1@sha256:… -> "2.335.1", true
//	registry.example.com:5000/runner:2.329.0        -> "2.329.0", true
//	ghcr.io/actions/actions-runner@sha256:…         -> "", false
//	acme.io/runner:v3-cuda                          -> "", false
//
// A digest-only reference, a floating tag, and a tenant's own tag say nothing about
// the runner inside, so they report false. Unlike the app.kubernetes.io/version
// label (provisioner.imageVersion), which reports whatever the tag says, this must
// not fall back to the pinned default: a guess here would read as a verified version
// on a condition whose whole job is to say what the image ships.
func WorkerImageRunnerVersion(image string) (string, bool) {
	if at := strings.IndexByte(image, '@'); at >= 0 {
		image = image[:at]
	}
	// A tag follows the last ':' that comes after the last '/' — a registry port
	// (host:5000/repo) has its colon before the final path separator.
	colon := strings.LastIndexByte(image, ':')
	if colon <= strings.LastIndexByte(image, '/') {
		return "", false
	}
	tag := strings.TrimPrefix(image[colon+1:], "v")
	if _, ok := parseRunnerVersion(tag); !ok {
		return "", false
	}
	return tag, true
}

// parseRunnerVersion splits a MAJOR.MINOR.PATCH runner version. Anything with a
// pre-release or build suffix is rejected: actions/runner does not publish them, so
// a tag carrying one is a tenant's own naming and not a version claim.
func parseRunnerVersion(v string) ([3]uint64, bool) {
	var out [3]uint64
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// runnerVersionLess reports whether a sorts before b. Both must already parse.
func runnerVersionLess(a, b [3]uint64) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// WorkerRunnerVersionCondition judges the effective worker image against GitHub's
// enforced registration minimum and returns the RunnerVersionTooOld condition to
// publish (Q715). It reaches GitHub for nothing, so both acquisition tiers can
// publish it every reconcile — including the scale-set tier, where the protocol
// carries no runner version at session creation and the listener therefore has no
// too-old failure to report.
//
// Three outcomes, all advisory (the condition never gates Ready):
//
//   - True/WorkerImageBelowMinimum — the image declares a version below the floor.
//   - False/WorkerImageCurrent — it declares one at or above the floor.
//   - Unknown/WorkerImageVersionUnknown — the reference declares no runner version,
//     so nothing has been verified. Said out loud rather than assumed good: a custom
//     image is exactly where a stale runner hides.
func WorkerRunnerVersionCondition(image string, generation int64) metav1.Condition {
	return WorkerRunnerVersionConditionWithRegistry(image, nil, generation)
}

// WorkerRunnerVersionConditionWithRegistry is WorkerRunnerVersionCondition with the
// registry's reading of the image folded in (Q988). The tag reading is the immediate
// answer, as before; the registry reading, once Done, is what the image actually
// ships and overrides it, with the message naming the digest it was read at and any
// disagreement with the tag. A Pending or Failed reading leaves the tag verdict
// standing with the state appended, so a reference whose tag names a runner version
// never regresses to Unknown because a registry was unreachable — that is today's
// behaviour exactly, and it is what an FQDN-mode egress policy or a tenant's own
// registry produces (neither admits the AGC).
//
// The trust argument is the one the tag already rests on: both are tenant-authored,
// and the registry copy is the stronger of the two, because it is immutable once
// addressed by digest and nothing running inside the container can rewrite it. A nil
// lookup means no resolver is wired and reports the tag alone.
func WorkerRunnerVersionConditionWithRegistry(image string, lookup *runnerimage.Lookup, generation int64) metav1.Condition {
	cond := metav1.Condition{
		Type:               apiconditions.ConditionRunnerVersionTooOld,
		ObservedGeneration: generation,
	}
	minParsed, ok := parseRunnerVersion(names.MinRunnerVersion)
	if !ok {
		// Unreachable while the constant is well-formed; TestMinRunnerVersionParses
		// pins that. Report unknown rather than judging against a floor we cannot read.
		cond.Status = metav1.ConditionUnknown
		cond.Reason = apiconditions.ReasonWorkerImageVersionUnknown
		cond.Message = fmt.Sprintf("cannot parse the enforced minimum runner version %q", names.MinRunnerVersion)
		return cond
	}

	tagVersion, tagKnown := WorkerImageRunnerVersion(image)
	if lookup != nil && lookup.State == runnerimage.Done {
		return registryVerdict(cond, image, tagVersion, tagKnown, lookup.Result, minParsed)
	}

	// The registry has not answered: the tag verdict, saying so.
	var suffix string
	if lookup != nil {
		switch lookup.State {
		case runnerimage.Pending:
			suffix = "; the registry read of the image is in progress"
		case runnerimage.Failed:
			suffix = fmt.Sprintf("; the registry read of the image failed (attempt %d: %s) and is retried, so this is the tag's claim alone",
				lookup.Attempts, lookup.Err)
		}
	}
	if !tagKnown {
		cond.Status = metav1.ConditionUnknown
		cond.Reason = apiconditions.ReasonWorkerImageVersionUnknown
		cond.Message = fmt.Sprintf(
			"worker image %s declares no actions/runner version in its tag, so the runner it ships cannot be checked against GitHub's enforced minimum %s%s",
			image, names.MinRunnerVersion, suffix)
		return cond
	}
	return versionVerdict(cond, image, tagVersion, minParsed, "", suffix)
}

// registryVerdict judges a completed registry reading. Content wins over the tag in
// both directions: a version the layers carry is judged even when the tag claims
// another, and a tag's claim does not stand for an image whose layers carry no runner.
func registryVerdict(cond metav1.Condition, image, tagVersion string, tagKnown bool, r runnerimage.Reading, minParsed [3]uint64) metav1.Condition {
	at := "at " + r.Digest
	if r.Platform != "" {
		at += " (" + r.Platform + ")"
	}
	if !r.Found {
		cond.Status = metav1.ConditionUnknown
		cond.Reason = apiconditions.ReasonWorkerImageVersionUnknown
		cond.Message = fmt.Sprintf(
			"worker image %s %s carries no bin/Runner.Listener.deps.json in its layers, so it is not actions/runner-derived where the runner layout puts the version and the runner it ships cannot be checked against GitHub's enforced minimum %s",
			image, at, names.MinRunnerVersion)
		if tagKnown {
			cond.Message += fmt.Sprintf("; its tag claims %s, which the image does not bear out", tagVersion)
		}
		return cond
	}
	if _, ok := parseRunnerVersion(r.Version); !ok {
		cond.Status = metav1.ConditionUnknown
		cond.Reason = apiconditions.ReasonWorkerImageVersionUnknown
		cond.Message = fmt.Sprintf(
			"worker image %s %s names runner version %q in its dependency manifest, which is not a MAJOR.MINOR.PATCH release, so it cannot be checked against GitHub's enforced minimum %s",
			image, at, r.Version, names.MinRunnerVersion)
		return cond
	}
	source := ", read from the registry " + at
	if tagKnown && tagVersion != r.Version {
		source += fmt.Sprintf(" rather than the %s its tag claims", tagVersion)
	}
	return versionVerdict(cond, image, r.Version, minParsed, source, "")
}

// versionVerdict compares a known version to the floor. source names where the
// version came from and suffix what is still outstanding; both may be empty.
func versionVerdict(cond metav1.Condition, image, version string, minParsed [3]uint64, source, suffix string) metav1.Condition {
	parsed, _ := parseRunnerVersion(version)
	if runnerVersionLess(parsed, minParsed) {
		cond.Status = metav1.ConditionTrue
		cond.Reason = apiconditions.ReasonWorkerImageBelowMinimum
		cond.Message = fmt.Sprintf(
			"worker image %s ships actions/runner %s%s, below GitHub's enforced minimum %s: GitHub refuses to register a runner this old, so jobs stop being served — update workerImage%s",
			image, version, source, names.MinRunnerVersion, suffix)
		return cond
	}

	cond.Status = metav1.ConditionFalse
	cond.Reason = apiconditions.ReasonWorkerImageCurrent
	cond.Message = fmt.Sprintf(
		"worker image %s ships actions/runner %s%s, at or above GitHub's enforced minimum %s%s",
		image, version, source, names.MinRunnerVersion, suffix)
	return cond
}

// DropListenerCondition reports whether a listener-pushed condition must be
// discarded rather than merged into the owner's status.
//
// RunnerVersionTooOld has two producers reporting different facts through one type:
// the classic listener, on GitHub rejecting agent.version at session creation, and
// the reconciler's own reading of the worker image (Q715). A healthy image reading
// does not refute a live session rejection, so WorkerRunnerVersionCondition's callers
// defer to a session-sourced True. This is the reverse half: the listener's
// VersionAccepted baseline clears a stale session-sourced True (Q795) and is dropped
// when a live condition stands whose reason is not the listener's own.
//
// The deference is one-directional by design, so this is not a symmetry. A
// session-sourced True still overwrites an image reading, because an observed
// rejection outranks a prediction; only the CLEAR is arbitrated.
//
// Callers apply it in two roles, and both are needed. At either drain it refuses a
// fresh push. In the pendingConditions retry it drops a retained one the image reading has
// since superseded: that retry is built for types the reconciler never re-derives, and
// this type it re-derives every reconcile, so a push merged when nothing stood to
// defer to would otherwise be re-applied for the owner's lifetime.
//
// Neither is cosmetic. A reconcile that reaches the image reading overwrites a merged
// clear in memory before writing status, so it never surfaces; the paths that write
// status EARLIER do surface it, the unresolved-references branch above all, where a
// tenant deleting a RunnerTemplate would see the image verdict wiped.
func DropListenerCondition(prev *metav1.Condition, pushed metav1.Condition) bool {
	if pushed.Type != apiconditions.ConditionRunnerVersionTooOld ||
		pushed.Reason != apiconditions.ReasonVersionAccepted {
		return false
	}
	return prev != nil && !isSessionSourcedRunnerVersion(prev.Reason)
}

// isSessionSourcedRunnerVersion reports whether reason is one the classic listener
// publishes on RunnerVersionTooOld, as opposed to the reconciler's image reading.
//
// The SESSION set is the closed one, deliberately, because the two sets fail in
// opposite directions. Enumerating the image reasons instead would let a fourth
// WorkerImage* reason added later fall through as not-image-sourced, so the listener
// baseline would overwrite a live verdict and no test or gate would go red —
// reason-tiers-check reconciles emitted reasons against the operator docs, not
// against this switch. Enumerating the session reasons makes the same omission
// conservative: an unrecognized reason is treated as the reconciler's, so the clear
// is dropped and the condition merely stays stale, which is the pre-Q795 behaviour
// rather than a wipe. This set is also the one far less likely to grow: the listener
// owns exactly two reasons on this type, and both are declared beside it.
func isSessionSourcedRunnerVersion(reason string) bool {
	switch reason {
	case apiconditions.ReasonVersionTooOld, apiconditions.ReasonVersionAccepted:
		return true
	}
	return false
}
