package controller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/usage"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// defaultThroughputHeadroomPercent is the Throughput profile's memory-limit
// headroom over the observed per-job peak when LimitHeadroomPercent is unset.
const defaultThroughputHeadroomPercent = 150

// Sizing-profile actuation (Q359 Phase 3). applySizingProfile is called from
// runnerSetTarget.Resolve on every acquired job, so it reads the freshest
// persisted history (status.sizingRecommendation — the same store the sampler
// re-seeds from) and a spec edit takes effect on the next job without a
// restart. The transform:
//
//   - only ever writes the cpu/memory keys — extended resources (GPUs) pass
//     through byte-identical, preserving the shape's job-selected identity;
//   - is whole-pod for the history-based profiles: Binpack/Throughput apply
//     only when EVERY template container has a confident recommendation
//     (usage.MinSamplesForDrift), otherwise the pod provisions with the
//     template's static values (predictable QoS beats partial actuation);
//   - deep-copies the template before mutating, so the informer-cached
//     RunnerTemplate object is never written through.

// sizingProfileApplies reports whether the configured profile would actuate
// given the persisted recommendation — the single predicate shared by the
// pod-build transform and the status.sizingProfileState reporting, so the two
// can never disagree.
func sizingProfileApplies(sizing *v2alpha1.WorkerSizing, template *v2alpha1.RunnerTemplateSpec, recs []v2alpha1.ContainerSizingRecommendation) bool {
	if sizing == nil || template == nil {
		return false
	}
	switch sizing.Profile {
	case v2alpha1.SizingProfileNodeShare:
		return sizing.NodeShare != nil
	case v2alpha1.SizingProfileBinpack, v2alpha1.SizingProfileThroughput:
		for i := range template.PodTemplate.Spec.Containers {
			if rec := recommendationFor(recs, template.PodTemplate.Spec.Containers[i].Name); rec == nil ||
				rec.SampleCount < usage.MinSamplesForDrift {
				return false
			}
		}
		return len(template.PodTemplate.Spec.Containers) > 0
	default:
		return false
	}
}

// applySizingProfile returns the pod template to provision with: the input
// template untouched when the profile does not actuate, or a deep-copied
// template with the derived cpu/memory values applied.
func applySizingProfile(template corev1.PodTemplateSpec, sizing *v2alpha1.WorkerSizing, tmplSpec *v2alpha1.RunnerTemplateSpec, recs []v2alpha1.ContainerSizingRecommendation) corev1.PodTemplateSpec {
	if !sizingProfileApplies(sizing, tmplSpec, recs) {
		return template
	}
	out := template.DeepCopy()
	switch sizing.Profile {
	case v2alpha1.SizingProfileNodeShare:
		applyNodeShare(&out.Spec, sizing)
	case v2alpha1.SizingProfileBinpack:
		for i := range out.Spec.Containers {
			c := &out.Spec.Containers[i]
			rec := recommendationFor(recs, c.Name)
			// Binpack: requests == limits from the history → Guaranteed QoS.
			cpu := clampQuantity(rec.Requests[corev1.ResourceCPU], sizing, corev1.ResourceCPU)
			mem := clampQuantity(rec.Limits[corev1.ResourceMemory], sizing, corev1.ResourceMemory)
			setResource(&c.Resources, corev1.ResourceCPU, cpu, cpu)
			setResource(&c.Resources, corev1.ResourceMemory, mem, mem)
		}
	case v2alpha1.SizingProfileThroughput:
		headroom := int64(defaultThroughputHeadroomPercent)
		if sizing.LimitHeadroomPercent != nil {
			headroom = int64(*sizing.LimitHeadroomPercent)
		}
		for i := range out.Spec.Containers {
			c := &out.Spec.Containers[i]
			rec := recommendationFor(recs, c.Name)
			// Requests from the history; no CPU limit so jobs burst; memory
			// limit = observed peak × headroom (never below the request).
			cpuReq := clampQuantity(rec.Requests[corev1.ResourceCPU], sizing, corev1.ResourceCPU)
			memReq := clampQuantity(rec.Requests[corev1.ResourceMemory], sizing, corev1.ResourceMemory)
			memPeak := rec.ObservedPeak[corev1.ResourceMemory]
			memLimit := scaleQuantity(corev1.ResourceMemory, memPeak, headroom)
			if memLimit.Cmp(memReq) < 0 {
				memLimit = memReq
			}
			setResource(&c.Resources, corev1.ResourceCPU, cpuReq, resource.Quantity{})
			delete(c.Resources.Limits, corev1.ResourceCPU)
			setResource(&c.Resources, corev1.ResourceMemory, memReq, memLimit)
		}
	}
	return *out
}

// applyNodeShare sets the runner container's requests to the declared per-node
// envelope divided by workersPerNode. Only the runner container: sidecars keep
// their template ask, and the operator accounts for them when choosing
// workersPerNode. Limits keep the template's values.
func applyNodeShare(spec *corev1.PodSpec, sizing *v2alpha1.WorkerSizing) {
	share := sizing.NodeShare
	for i := range spec.Containers {
		c := &spec.Containers[i]
		if c.Name != provisioner.WorkerContainerName {
			continue
		}
		for _, res := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			alloc, ok := share.Allocatable[res]
			if !ok {
				continue
			}
			q := divideQuantity(res, alloc, int64(share.WorkersPerNode))
			q = clampQuantity(q, sizing, res)
			if c.Resources.Requests == nil {
				c.Resources.Requests = corev1.ResourceList{}
			}
			c.Resources.Requests[res] = q
			// A template limit below the derived request would be rejected at
			// admission; lift it to the request rather than provision a pod
			// the apiserver refuses.
			if lim, ok := c.Resources.Limits[res]; ok && lim.Cmp(q) < 0 {
				c.Resources.Limits[res] = q
			}
		}
	}
}

// recommendationFor returns the recommendation entry for a container, or nil.
func recommendationFor(recs []v2alpha1.ContainerSizingRecommendation, name string) *v2alpha1.ContainerSizingRecommendation {
	for i := range recs {
		if recs[i].Container == name {
			return &recs[i]
		}
	}
	return nil
}

// setResource writes a request (and, when non-zero, a limit) for one resource
// name, preserving every other key in the maps (extended resources pass
// through untouched).
func setResource(res *corev1.ResourceRequirements, name corev1.ResourceName, req, limit resource.Quantity) {
	if res.Requests == nil {
		res.Requests = corev1.ResourceList{}
	}
	res.Requests[name] = req
	if limit.IsZero() {
		return
	}
	if res.Limits == nil {
		res.Limits = corev1.ResourceList{}
	}
	res.Limits[name] = limit
}

// clampQuantity bounds a derived value within the profile's optional
// minRequests/maxRequests clamps.
func clampQuantity(q resource.Quantity, sizing *v2alpha1.WorkerSizing, name corev1.ResourceName) resource.Quantity {
	if min, ok := sizing.MinRequests[name]; ok && q.Cmp(min) < 0 {
		return min
	}
	if max, ok := sizing.MaxRequests[name]; ok && q.Cmp(max) > 0 {
		return max
	}
	return q
}

// scaleQuantity returns q × percent/100. CPU scales in millicores; memory in
// whole bytes rounded down to 1Mi (milli-byte quantities are legal but
// unreadable, and sizing is far coarser than 1Mi anyway).
func scaleQuantity(name corev1.ResourceName, q resource.Quantity, percent int64) resource.Quantity {
	if name == corev1.ResourceCPU {
		return *resource.NewMilliQuantity(q.MilliValue()*percent/100, resource.DecimalSI)
	}
	return *resource.NewQuantity(roundDownMi(q.Value()*percent/100), resource.BinarySI)
}

// divideQuantity returns q ÷ n, with the same unit handling as scaleQuantity.
func divideQuantity(name corev1.ResourceName, q resource.Quantity, n int64) resource.Quantity {
	if name == corev1.ResourceCPU {
		return *resource.NewMilliQuantity(q.MilliValue()/n, resource.DecimalSI)
	}
	return *resource.NewQuantity(roundDownMi(q.Value()/n), resource.BinarySI)
}

// roundDownMi rounds bytes down to a 1Mi boundary (never below 1Mi).
func roundDownMi(bytes int64) int64 {
	const mi = 1 << 20
	if bytes < mi {
		return mi
	}
	return bytes / mi * mi
}
