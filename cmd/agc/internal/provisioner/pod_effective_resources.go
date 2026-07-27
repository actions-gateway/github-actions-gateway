package provisioner

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Effective pod requests/limits — the quantities Kubernetes charges a pod against
// a ResourceQuota (Q450).
//
// This is a deliberate mirror of k8s.io/component-helpers/resource.PodRequests /
// PodLimits (which the quota evaluator itself calls, via
// pkg/quota/v1/evaluator/core.PodUsageFunc). We mirror rather than import because
// component-helpers is not in the workspace vendor tree and pulling a module in
// for two functions is a poor trade — but the arithmetic is copied faithfully,
// including the parts that are easy to "simplify" wrongly:
//
//   - Regular containers are SUMMED.
//   - A native sidecar (an init container with restartPolicy: Always) is summed
//     into the pod total too, and additionally raises the floor contributed by
//     every init container declared after it.
//   - A plain init container contributes only a FLOOR (max-of), not a sum: its own
//     ask plus the cumulative ask of the native sidecars preceding it.
//   - Pod overhead is added IN FULL to requests, but to limits only for keys the
//     pod already limits with a non-zero value. This asymmetry is real: a DinD
//     shape that declares no CPU limit gets no overhead added to limits.cpu.
//
// Upstream reference: staging/src/k8s.io/component-helpers/resource/helpers.go
// (aggregateContainerResourcesByFn), which implements KEP-753's
// "InitContainerUse(i) = Σ(restartable init containers with index < i) + i-th
// init container" formula. Any edit here must keep matching it — an under-count
// makes the admission gate claim jobs whose pods the quota then rejects, and an
// over-count strands capacity the tenant paid for.

// podEffectiveRequests returns the pod's effective resource requests: what a
// ResourceQuota charges to requests.* (and the legacy cpu/memory aliases).
//
// Regular containers are gap-filled the way applyResourceDefaults stamps them at
// pod-build time; init containers are not, because applyResourceDefaults does not
// touch them. So a native sidecar declaring no resources genuinely contributes
// zero — matching the pod the provisioner would create, not an idealised one.
func podEffectiveRequests(spec *corev1.PodSpec) corev1.ResourceList {
	reqs := aggregateContainerResources(spec, func(r corev1.ResourceRequirements) corev1.ResourceList {
		return r.Requests
	})
	// Overhead counts in full against requests.
	addResourceList(reqs, spec.Overhead)
	return reqs
}

// podEffectiveLimits returns the pod's effective resource limits: what a
// ResourceQuota charges to limits.*.
//
// Overhead is added only to keys that already carry a non-zero limit. A resource
// the pod does not limit stays unlimited — adding overhead there would invent a
// limits.* charge the API server never makes, and would make the gate reject
// pods over a key the quota cannot even constrain for this shape.
func podEffectiveLimits(spec *corev1.PodSpec) corev1.ResourceList {
	limits := aggregateContainerResources(spec, func(r corev1.ResourceRequirements) corev1.ResourceList {
		return r.Limits
	})
	for name, overhead := range spec.Overhead {
		if v, ok := limits[name]; ok && !v.IsZero() {
			v.Add(overhead)
			limits[name] = v
		}
	}
	return limits
}

// aggregateContainerResources sums pick() across the pod's regular containers and
// native sidecars, then raises the result to the floor the init sequence needs.
// See this file's header for the formula and why it is shaped this way.
func aggregateContainerResources(spec *corev1.PodSpec, pick func(corev1.ResourceRequirements) corev1.ResourceList) corev1.ResourceList {
	out := corev1.ResourceList{}
	for i := range spec.Containers {
		// Gap-fill mirrors applyResourceDefaults: a regular container declaring
		// neither requests nor limits still costs the stamped defaults.
		addResourceList(out, pick(gapFilledResources(&spec.Containers[i])))
	}

	// sidecars is the running total of the native sidecars seen so far; initFloor
	// is the high-water mark across the init sequence.
	sidecars := corev1.ResourceList{}
	initFloor := corev1.ResourceList{}
	for i := range spec.InitContainers {
		c := &spec.InitContainers[i]
		cur := pick(c.Resources)
		if IsNativeSidecar(c) {
			// A native sidecar runs alongside the regular containers for the whole
			// pod lifetime, so Kubernetes sums it into the pod total...
			addResourceList(out, cur)
			// ...and it is still running while every later init container starts, so
			// it raises their floor too.
			addResourceList(sidecars, cur)
			cur = sidecars
		} else {
			combined := corev1.ResourceList{}
			addResourceList(combined, cur)
			addResourceList(combined, sidecars)
			cur = combined
		}
		maxResourceList(initFloor, cur)
	}
	maxResourceList(out, initFloor)
	return out
}

// IsNativeSidecar reports whether c is a native sidecar: an init container with
// restartPolicy: Always (KEP-753, GA in Kubernetes 1.29). Kubernetes sums a
// native sidecar's resources into the pod's effective requests/limits, unlike a
// plain init container. This is how the DinD daemon must be declared — a
// regular-container sidecar strands the pod (Q249).
func IsNativeSidecar(c *corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

// addResourceList adds every quantity in src into dst, keyed by resource name.
// Mirrors the upstream helper of the same name.
func addResourceList(dst, src corev1.ResourceList) {
	for name, q := range src {
		addQuantity(dst, name, q)
	}
}

// addQuantity accumulates q into dst[name], seeding the key with a copy when it is
// absent. The copy matters: resource.Quantity carries a cached formatted string, so
// storing the caller's value and later Add()-ing to it would mutate through.
func addQuantity(dst corev1.ResourceList, name corev1.ResourceName, q resource.Quantity) {
	if v, ok := dst[name]; ok {
		v.Add(q)
		dst[name] = v
		return
	}
	dst[name] = q.DeepCopy()
}

// maxResourceList raises dst to the per-resource maximum of dst and src, adding
// keys src has and dst lacks. Mirrors the upstream helper of the same name.
func maxResourceList(dst, src corev1.ResourceList) {
	for name, q := range src {
		if v, ok := dst[name]; !ok || q.Cmp(v) > 0 {
			dst[name] = q.DeepCopy()
		}
	}
}
