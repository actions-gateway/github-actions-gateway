package v2

import "github.com/actions-gateway/github-actions-gateway/api/apisidecar"

// The worker-pod sidecar contract is declared once, version-neutrally, in
// api/apisidecar: none of it is schema (the annotation key is metadata, and the
// heuristic reads the corev1.PodSpec that RunnerTemplateSpec embeds unchanged). This
// file re-exports the two constants and wraps the heuristic in RunnerTemplateSpec.
//
// Read apisidecar for what the annotation means and why the heuristic only ever
// warns.
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
