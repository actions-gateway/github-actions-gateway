package v2beta1

import "github.com/actions-gateway/github-actions-gateway/api/apisidecar"

// The worker-pod sidecar contract is declared once, version-neutrally, in
// api/apisidecar: none of it is schema (the annotation key is metadata, and the
// heuristic reads the corev1.PodSpec that every version's RunnerTemplateSpec embeds
// unchanged). This file re-exports the two constants and wraps the heuristic in this
// version's RunnerTemplateSpec so existing call sites are unaffected;
// api/v2beta1/sidecar.go carries the identical block.
//
// Read apisidecar for what the annotation means and why the heuristic only ever
// warns. The two files must stay byte-identical except the package clause;
// scripts/go/check-v2-api-sync.sh fails the build on a one-sided edit (Q374).
const (
	RunnerContainerName           = apisidecar.RunnerContainerName
	SelfExitingSidecarsAnnotation = apisidecar.SelfExitingSidecarsAnnotation
)

// ReapBlockingSidecars returns the names (sorted) of regular sidecar containers in
// this version's worker pod template that may prevent the pod from reaping when the
// runner container exits (Q249). See apisidecar.ReapBlocking.
func ReapBlockingSidecars(spec *RunnerTemplateSpec, annotations map[string]string) []string {
	if spec == nil {
		return nil
	}
	return apisidecar.ReapBlocking(&spec.PodTemplate.Spec, annotations)
}
