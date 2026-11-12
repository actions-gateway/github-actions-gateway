package v2

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	psaapi "k8s.io/pod-security-admission/api"
	psapolicy "k8s.io/pod-security-admission/policy"
)

// restrictedVolumesCheckID is the upstream Pod Security Standards check that
// limits a pod's volumes to the types the restricted level allows.
const restrictedVolumesCheckID psapolicy.CheckID = "restrictedVolumes"

// reservedSignerPrefix marks the certificate signers the Kubernetes project
// reserves. k8s.io/api v0.37.2 documents one, kubernetes.io/kube-apiserver-client-pod,
// as issuing client certificates the apiserver accepts.
const reservedSignerPrefix = "kubernetes.io"

// podShapeVersion is the Pod Security Standards version both rules evaluate at.
// It is pinned, not psaapi.LatestVersion(): the latest version moves with the
// vendored k8s.io/pod-security-admission, so a dependency bump that carried a
// stricter upstream check would refuse a manifest the previous release admitted.
// Raising the pin is an API change: it is breaking whenever the newer version
// forbids something this one allows.
var podShapeVersion = psaapi.MajorMinorVersion(1, 37)

// baselineEvaluator evaluates the Pod Security Standards baseline level and
// volumeTypeEvaluator the restricted level's volume-type check alone, both at
// podShapeVersion. They are the checks the apiserver's PodSecurity admission plugin
// runs, so the webhook and a baseline namespace at that version agree on what a
// baseline pod is.
var (
	baselineEvaluator   = mustEvaluator(psapolicy.DefaultChecks())
	volumeTypeEvaluator = mustEvaluator(checksByID(psapolicy.DefaultChecks(), restrictedVolumesCheckID))

	baselineLevel   = psaapi.LevelVersion{Level: psaapi.LevelBaseline, Version: podShapeVersion}
	restrictedLevel = psaapi.LevelVersion{Level: psaapi.LevelRestricted, Version: podShapeVersion}
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
	results := baselineEvaluator.EvaluatePod(baselineLevel, &tmpl.ObjectMeta, &tmpl.Spec)
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
	results := volumeTypeEvaluator.EvaluatePod(restrictedLevel, &tmpl.ObjectMeta, &tmpl.Spec)
	agg := psapolicy.AggregateCheckResults(results)
	if agg.Allowed {
		return nil
	}
	return fmt.Errorf(
		"podTemplate.spec.volumes: %s; a namespaced RunnerTemplate may use only configMap, csi, downwardAPI, emptyDir, ephemeral, image, persistentVolumeClaim, projected and secret volumes; mount other storage through a PersistentVolumeClaim, or use a platform-owned ClusterRunnerTemplate",
		agg.ForbiddenDetail())
}

// validateNoAPICredentialProjection rejects a projected volume source that puts a
// Kubernetes API credential in the worker pod, on both template kinds: a
// serviceAccountToken, which the kubelet mints whatever automountServiceAccountToken
// says, and a podCertificate addressed to a reserved signer. The AGC forbids an API
// credential in a worker pod on every security profile. A podCertificate from any
// other signer is a workload certificate and is left alone.
func validateNoAPICredentialProjection(spec *corev1.PodSpec) error {
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
			if isAPICredentialCertificate(s.PodCertificate) {
				return fmt.Errorf(
					"podTemplate.spec.volumes[%q]: a projected podCertificate source with signer %q is reserved: worker pods carry no Kubernetes API credential; remove the source, or name a signer outside %s",
					v.Name, s.PodCertificate.SignerName, reservedSignerPrefix)
			}
		}
	}
	return nil
}

// isAPICredentialCertificate reports whether p asks a reserved signer for its
// certificate. The whole reserved prefix is refused, not the one documented
// signer, so a signer Kubernetes adds later is refused until it is shown not to
// authenticate to the apiserver.
func isAPICredentialCertificate(p *corev1.PodCertificateProjection) bool {
	return p != nil && strings.HasPrefix(p.SignerName, reservedSignerPrefix)
}

// validateNoSchedulerBypass rejects a namespaced RunnerTemplate that places its
// worker without the default scheduler. A pod with nodeName set is bound to that node
// with no scheduler involved, and one naming another schedulerName is placed by a
// scheduler that need not honour the control-plane pods' required anti-affinity
// against workers, so either lets a tenant put a worker on a controller's node. The
// cluster-scoped kind is platform-authored and keeps both.
func validateNoSchedulerBypass(spec *corev1.PodSpec) error {
	if spec.NodeName != "" {
		return fmt.Errorf(
			"podTemplate.spec.nodeName: a namespaced RunnerTemplate may not bind its worker to a node, since that bypasses the scheduler and the rule keeping workers off control-plane nodes; constrain placement with nodeSelector or affinity, or use a platform-owned ClusterRunnerTemplate")
	}
	if spec.SchedulerName != "" && spec.SchedulerName != corev1.DefaultSchedulerName {
		return fmt.Errorf(
			"podTemplate.spec.schedulerName: %q: a namespaced RunnerTemplate may use only %s, since another scheduler need not keep workers off control-plane nodes; use a platform-owned ClusterRunnerTemplate for a worker placed by another scheduler",
			spec.SchedulerName, corev1.DefaultSchedulerName)
	}
	return nil
}

// podTemplateUnchanged reports whether an update leaves podTemplate as stored. The
// pod-shape rules that arrived in v2.0.0 are skipped when it does, so a template
// stored before them is not refused over an edit elsewhere in the object.
func podTemplateUnchanged(oldSpec, newSpec *corev1.PodTemplateSpec) bool {
	return apiequality.Semantic.DeepEqual(oldSpec, newSpec)
}
