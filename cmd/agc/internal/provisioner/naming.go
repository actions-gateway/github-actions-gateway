package provisioner

import (
	"regexp"

	"github.com/actions-gateway/github-actions-gateway/api/apinames"
)

// repoSegmentRE accepts only the characters GitHub allows in org/repo names.
// Must start with an alphanumeric character so that ".." and other dot-leading
// strings cannot produce path-traversal sequences in the API URL.
var repoSegmentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// workerPodPrefix marks every worker pod the AGC creates, on both acquisition
// tiers.
const workerPodPrefix = "runner"

// safeName converts an arbitrary string into a Kubernetes-safe DNS label
// (lowercase, alphanumeric and hyphens only, at most 48 chars, ending in a 7-hex
// digest of the whole input). It is [apinames.Segment] under the AGC's "job"
// fallback — what a name derived from input that sanitises to nothing is called.
// The fallback must not change: it is baked into Secret names on live clusters.
func safeName(s string) string { return apinames.Segment(s, "job") }

// workerPodName derives the worker pod name for one job — "runner-<owner>-<id>" —
// as a valid DNS-1123 label of at most 63 characters. Both acquisition tiers derive
// their pod names here: the v1 path from the RunnerGroup name and the job's plan ID,
// the v2 scale-set path from the RunnerSet name and GitHub's job ID.
//
// Truncation is unavoidable: "runner-" plus a sanitised owner plus a sanitised
// 36-char UUID overruns 63 characters for any realistic tenant name. Truncating the
// concatenation is what broke (Q467) — a cut that landed on one of a UUID's hyphens
// produced a name the apiserver rejected, so every worker pod for that owner failed
// to create and no job ever ran, while GitHub reported only that the runner had lost
// communication. [apinames.Join] splits the budget across the segments before
// joining them, so no cut can reach a separator.
//
// Uniqueness after truncation: each segment is either the full safeName of its input
// — which already ends in a 7-hex digest of that whole input — or an
// [apinames.Truncate] of it, which ends in a 7-hex digest of that whole segment. So
// a name collision needs BOTH segments to collide, and a segment collision needs
// both a shared visible prefix and a shared digest. The id segment alone is
// therefore enough: distinct job identities give distinct pod names. A repeat
// delivery of the same job deliberately maps to the same name, which is what makes
// pod creation idempotent and lets the v2 completion path find the pod it created.
func workerPodName(owner, id string) string {
	return apinames.Join(apinames.MaxLabelValue, workerPodPrefix, safeName(owner), safeName(id))
}
