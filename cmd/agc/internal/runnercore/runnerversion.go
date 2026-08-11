package runnercore

import (
	"fmt"
	"strconv"
	"strings"

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

	version, known := WorkerImageRunnerVersion(image)
	if !known {
		cond.Status = metav1.ConditionUnknown
		cond.Reason = apiconditions.ReasonWorkerImageVersionUnknown
		cond.Message = fmt.Sprintf(
			"worker image %s declares no actions/runner version in its tag, so the runner it ships cannot be checked against GitHub's enforced minimum %s",
			image, names.MinRunnerVersion)
		return cond
	}

	parsed, _ := parseRunnerVersion(version)
	if runnerVersionLess(parsed, minParsed) {
		cond.Status = metav1.ConditionTrue
		cond.Reason = apiconditions.ReasonWorkerImageBelowMinimum
		cond.Message = fmt.Sprintf(
			"worker image %s ships actions/runner %s, below GitHub's enforced minimum %s: GitHub refuses to register a runner this old, so jobs stop being served — update workerImage",
			image, version, names.MinRunnerVersion)
		return cond
	}

	cond.Status = metav1.ConditionFalse
	cond.Reason = apiconditions.ReasonWorkerImageCurrent
	cond.Message = fmt.Sprintf(
		"worker image %s ships actions/runner %s, at or above GitHub's enforced minimum %s",
		image, version, names.MinRunnerVersion)
	return cond
}
