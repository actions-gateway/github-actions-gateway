package provisioner

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

// defaultWorkerCPU / defaultWorkerMemory are the resource requests *and*
// limits stamped on a worker container when the tenant PodTemplate omits them.
// Without these a worker pod is Best-Effort QoS: the first thing the kubelet
// evicts under node pressure, which burns the eviction-retry budget fast.
// Setting requests == limits makes a single-container worker pod Guaranteed QoS.
var (
	defaultWorkerCPU    = resource.MustParse("500m")
	defaultWorkerMemory = resource.MustParse("1Gi")
)

// DefaultWorkerResources returns (a copy of) the gap-fill requests/limits
// stamped on a worker container that declares neither — the effective ask the
// sizing-drift judgment compares against when a template omits resources
// entirely (Q359 Phase 2).
func DefaultWorkerResources() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    defaultWorkerCPU,
		corev1.ResourceMemory: defaultWorkerMemory,
	}
}

// disruptionSafetyDefaults is the gap-fill set of node-disruption-safety
// annotations stamped on every worker pod. Each value is the marker the
// respective component honors to skip evicting a running pod.
var disruptionSafetyDefaults = map[string]string{
	annoKarpenterDoNotDisrupt:       "true",
	annoSafeToEvict:                 "false",
	annoDeschedulerPreferNoEviction: "true",
}

// applyDisruptionSafetyDefaults gap-fills the node-disruption-safety annotations
// (see disruptionSafetyDefaults) onto the worker pod annotation map, mirroring
// the secure-by-default SecurityContext gap-fill: a controller-managed default
// that a tenant can still override per key.
//
// A worker pod runs exactly one CI job and has no replica/controller behind it,
// so an autoscaler or descheduler that evicts it mid-job strands that job with
// no replacement. Stamping these markers makes the pod "production-relyable" on
// the clusters operators actually run (Karpenter, cluster-autoscaler,
// descheduler) without per-tenant configuration. The markers ride on the worker
// pod itself, so they vanish the moment the pod is torn down on job completion
// (immediately when completedPodTTL is 0, otherwise by the reaper) — they can
// never pin a dead pod.
//
// Overridable: a tenant who manages disruption another way (a PodDisruptionBudget,
// or a job they know is safe to interrupt) can set any of these keys to a
// different value in their PodTemplate metadata and that explicit value wins.
// Only these three keys are honored from the template; arbitrary template
// annotations are not copied onto the worker pod.
func applyDisruptionSafetyDefaults(dst, templateAnnotations map[string]string) map[string]string {
	if dst == nil {
		dst = make(map[string]string, len(disruptionSafetyDefaults))
	}
	for key, def := range disruptionSafetyDefaults {
		if v, ok := templateAnnotations[key]; ok {
			dst[key] = v // explicit tenant override wins
			continue
		}
		if _, ok := dst[key]; !ok {
			dst[key] = def
		}
	}
	return dst
}

// applySecurityDefaults stamps a secure-by-default SecurityContext onto the
// worker PodSpec, scaled to the tenant's PSA profile. It gap-fills only: any
// field the tenant set explicitly in the PodTemplate is preserved.
//
//   - privileged: no-op. This profile exists precisely so DinD and
//     host-capability workloads can opt out; stamping defaults would defeat it.
//   - baseline (and the empty default): pod-level runAsNonRoot + runAsUser +
//     seccomp RuntimeDefault. runAsUser is gap-filled to the runner image's
//     numeric UID (defaultWorkerRunAsUser) whenever non-root is being enforced,
//     because kubelet cannot verify runAsNonRoot against the image's
//     non-numeric `USER runner` and would otherwise reject the pod at admission
//     (Q115). All three are compatible with the standard non-root runner image
//     and, crucially, do not block in-job privilege escalation (sudo), which
//     baseline PSA permits and many CI jobs rely on.
//   - restricted: additionally stamps the per-container PSA-restricted floor
//     (allowPrivilegeEscalation=false + drop ALL capabilities). Without it the
//     namespace's PodSecurity admission rejects the pod. Blocking sudo/caps is
//     expected here because the tenant explicitly chose high isolation.
func applySecurityDefaults(spec *corev1.PodSpec, securityProfile string) {
	profile := strings.ToLower(strings.TrimSpace(securityProfile))
	if profile == securityProfilePrivileged {
		return
	}

	if spec.SecurityContext == nil {
		spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	if spec.SecurityContext.RunAsNonRoot == nil {
		spec.SecurityContext.RunAsNonRoot = ptr.To(true)
	}
	// Gap-fill a numeric runAsUser only while non-root is actually enforced and
	// the tenant didn't pick a UID. kubelet can only verify runAsNonRoot against
	// a numeric UID; the runner image's non-numeric `USER runner` otherwise
	// trips CreateContainerConfigError at admission (Q115). Skipped when a tenant
	// opted out with runAsNonRoot:false (a root-based image) so we don't force a
	// UID that contradicts their choice, and gap-fill so an explicit per-pod or
	// per-container runAsUser still wins.
	if r := spec.SecurityContext.RunAsNonRoot; r != nil && *r && spec.SecurityContext.RunAsUser == nil {
		spec.SecurityContext.RunAsUser = ptr.To(defaultWorkerRunAsUser)
	}
	if spec.SecurityContext.SeccompProfile == nil {
		spec.SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	}

	if profile != securityProfileRestricted {
		return
	}

	harden := func(containers []corev1.Container) {
		for i := range containers {
			if containers[i].SecurityContext == nil {
				containers[i].SecurityContext = &corev1.SecurityContext{}
			}
			sc := containers[i].SecurityContext
			if sc.AllowPrivilegeEscalation == nil {
				sc.AllowPrivilegeEscalation = ptr.To(false)
			}
			if sc.Capabilities == nil {
				sc.Capabilities = &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}
			}
			if sc.RunAsNonRoot == nil {
				sc.RunAsNonRoot = ptr.To(true)
			}
			if sc.SeccompProfile == nil {
				sc.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
			}
		}
	}
	harden(spec.Containers)
	harden(spec.InitContainers)
}

// applyResourceDefaults stamps default CPU/memory requests and limits onto any
// regular worker container that declares neither, on every profile. Init
// containers are left untouched: their requests inflate the pod's effective
// scheduling footprint and are usually short-lived setup steps. Gap-fill only —
// a container that sets either requests or limits keeps the tenant's values.
func (p *Provisioner) applyResourceDefaults(spec *corev1.PodSpec) {
	for i := range spec.Containers {
		r := &spec.Containers[i].Resources
		if len(r.Requests) > 0 || len(r.Limits) > 0 {
			continue
		}
		r.Requests = corev1.ResourceList{
			corev1.ResourceCPU:    defaultWorkerCPU,
			corev1.ResourceMemory: defaultWorkerMemory,
		}
		r.Limits = corev1.ResourceList{
			corev1.ResourceCPU:    defaultWorkerCPU,
			corev1.ResourceMemory: defaultWorkerMemory,
		}
	}
}
