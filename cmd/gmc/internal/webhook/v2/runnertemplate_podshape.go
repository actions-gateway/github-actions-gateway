package v2

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	psaapi "k8s.io/pod-security-admission/api"
	psapolicy "k8s.io/pod-security-admission/policy"
)

// restrictedVolumesCheckID is the upstream Pod Security Standards check that
// limits a pod's volumes to the types the restricted level allows.
const restrictedVolumesCheckID psapolicy.CheckID = "restrictedVolumes"

// baselineEvaluator evaluates the Pod Security Standards baseline level and
// volumeTypeEvaluator the restricted level's volume-type check alone, both at the
// latest version the vendored k8s.io/pod-security-admission knows. They are the
// checks the apiserver's PodSecurity admission plugin runs, so the webhook and a
// baseline namespace agree on what a baseline pod is.
var (
	baselineEvaluator   = mustEvaluator(psapolicy.DefaultChecks())
	volumeTypeEvaluator = mustEvaluator(checksByID(psapolicy.DefaultChecks(), restrictedVolumesCheckID))

	baselineLatest   = psaapi.LevelVersion{Level: psaapi.LevelBaseline, Version: psaapi.LatestVersion()}
	restrictedLatest = psaapi.LevelVersion{Level: psaapi.LevelRestricted, Version: psaapi.LatestVersion()}
)

func mustEvaluator(checks []psapolicy.Check) psapolicy.Evaluator {
	e, err := psapolicy.NewEvaluator(checks, nil)
	if err != nil {
		panic(fmt.Sprintf("build Pod Security evaluator: %v", err))
	}
	return e
}

func checksByID(checks []psapolicy.Check, id psapolicy.CheckID) []psapolicy.Check {
	var out []psapolicy.Check
	for _, c := range checks {
		if c.ID == id {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		panic(fmt.Sprintf("Pod Security check %q is not in the vendored policy set", id))
	}
	return out
}

// validateBaselinePodShape rejects a namespaced RunnerTemplate whose podTemplate
// exceeds the Pod Security Standards baseline level. A namespace on the privileged
// profile runs Pod Security Admission at privileged, so there this is the only
// check between a tenant-authored template and a host-reaching pod (hostPath,
// added capabilities, an unconfined seccomp or AppArmor profile, hostPort, unsafe
// sysctls); a shape past baseline belongs in a platform-owned ClusterRunnerTemplate.
// Every baseline check forbids a set value and none requires one, so a partial
// template evaluates the same as the pod the AGC builds from it.
func validateBaselinePodShape(tmpl *corev1.PodTemplateSpec) error {
	results := baselineEvaluator.EvaluatePod(baselineLatest, &tmpl.ObjectMeta, &tmpl.Spec)
	agg := psapolicy.AggregateCheckResults(results)
	if agg.Allowed {
		return nil
	}
	return fmt.Errorf(
		"podTemplate exceeds the Pod Security Standards baseline level, which a namespaced RunnerTemplate must meet: %s; use a platform-owned ClusterRunnerTemplate for a worker shape that needs more",
		agg.ForbiddenDetail())
}

// validateVolumeTypes rejects a namespaced RunnerTemplate volume of a type the Pod
// Security Standards restricted level does not allow. Baseline forbids only
// hostPath, so in a baseline namespace an nfs or iscsi volume is admitted and the
// node mounts it from a tenant-chosen server: a data path the worker NetworkPolicy
// and the egress proxy never see. The allowed types are node-local, pulled like a
// container image, or provisioned through an object the platform controls (a
// PersistentVolume, a StorageClass, a CSIDriver).
func validateVolumeTypes(tmpl *corev1.PodTemplateSpec) error {
	results := volumeTypeEvaluator.EvaluatePod(restrictedLatest, &tmpl.ObjectMeta, &tmpl.Spec)
	agg := psapolicy.AggregateCheckResults(results)
	if agg.Allowed {
		return nil
	}
	return fmt.Errorf(
		"podTemplate.spec.volumes: %s; a namespaced RunnerTemplate may use only configMap, csi, downwardAPI, emptyDir, ephemeral, image, persistentVolumeClaim, projected and secret volumes; mount other storage through a PersistentVolumeClaim, or use a platform-owned ClusterRunnerTemplate",
		agg.ForbiddenDetail())
}

// validateNoServiceAccountTokenProjection rejects a projected volume carrying a
// serviceAccountToken source, on both template kinds. The kubelet mints that token
// whatever automountServiceAccountToken says, so it is a second route to a
// Kubernetes API credential in the worker pod, which the AGC forbids on every
// security profile.
func validateNoServiceAccountTokenProjection(spec *corev1.PodSpec) error {
	for _, v := range spec.Volumes {
		if v.Projected == nil {
			continue
		}
		for _, s := range v.Projected.Sources {
			if s.ServiceAccountToken != nil {
				return fmt.Errorf(
					"podTemplate.spec.volumes[%q]: a projected serviceAccountToken source is reserved: worker pods carry no Kubernetes API credential; remove the source",
					v.Name)
			}
		}
	}
	return nil
}

// podTemplateUnchanged reports whether an update leaves podTemplate as stored. The
// pod-shape rules that arrived in v2.0.0 are skipped when it does, so a template
// stored before them is not refused over an edit elsewhere in the object.
func podTemplateUnchanged(oldSpec, newSpec *corev1.PodTemplateSpec) bool {
	return apiequality.Semantic.DeepEqual(oldSpec, newSpec)
}
