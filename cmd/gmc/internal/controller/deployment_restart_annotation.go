package controller

import (
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// kubectlRestartedAtAnnotation is the pod-template annotation `kubectl rollout
// restart` stamps to change the template hash and trigger a new ReplicaSet. No
// builder sets it, so a blanket template replace reverts it on the next reconcile
// and the restart is a silent no-op — kubectl still prints "successfully rolled
// out" because the old ReplicaSet is, trivially, complete (Q552).
const kubectlRestartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"

// toleratedTemplateAnnotations are the pod-template annotation keys a GMC-managed
// Deployment carries over from the live object instead of reverting to the
// builder's desired template.
//
// The list is deliberately a single well-known key rather than a
// preserve-unmanaged-keys rule: the GMC still owns the pod template outright, so
// arbitrary hand-edited annotations are reverted as before, and a key the builders
// stop setting cannot linger forever. Adding a key here is a decision to give up
// reconciliation of that key — take it one key at a time.
var toleratedTemplateAnnotations = []string{kubectlRestartedAtAnnotation}

// assignManagedPodTemplate writes desired onto live, carrying over the
// operator-injected annotations in toleratedTemplateAnnotations. Values already
// present in desired win, so a builder that starts setting a tolerated key
// reclaims it.
func assignManagedPodTemplate(live *corev1.PodTemplateSpec, desired corev1.PodTemplateSpec) {
	preserved := make(map[string]string, len(toleratedTemplateAnnotations))
	for _, k := range toleratedTemplateAnnotations {
		if v, ok := live.Annotations[k]; ok {
			preserved[k] = v
		}
	}
	*live = desired
	if len(preserved) == 0 {
		return
	}
	// desired's map belongs to the builder's return value; write to a clone.
	annotations := make(map[string]string, len(live.Annotations)+len(preserved))
	maps.Copy(annotations, preserved)
	maps.Copy(annotations, live.Annotations)
	live.Annotations = annotations
}

// assignManagedDeploymentSpec replaces live with the builder's desired spec,
// keeping the tolerated pod-template annotations. It is the whole-spec-replace
// counterpart to assignHPATargetDeploymentSpec, for a Deployment no other
// controller writes to.
func assignManagedDeploymentSpec(live *appsv1.DeploymentSpec, desired appsv1.DeploymentSpec) {
	template := live.Template
	*live = desired
	assignManagedPodTemplate(&template, desired.Template)
	live.Template = template
}
