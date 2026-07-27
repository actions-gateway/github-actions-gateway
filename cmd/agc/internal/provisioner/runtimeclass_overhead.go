package provisioner

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// runtimeClassReadTimeout bounds the RuntimeClass read in ResolveWorkerPodSpec.
//
// The read goes through the manager cache, and RuntimeClass is the AGC's only
// cluster-scoped compute dependency: on the *first* read controller-runtime
// establishes the informer and waits for its initial LIST. If the AGC's
// ClusterRole is missing the runtimeclasses grant — an install upgraded image-first,
// or hand-rolled RBAC — that informer can never sync, and an unbounded Get would
// block the job-acquisition path rather than degrade. Bounding it converts that
// into the same fail-open zero-overhead answer the gate gave before Q450, at a
// cost of one short stall per evaluation on a misconfigured install.
const runtimeClassReadTimeout = 2 * time.Second

// ResolveWorkerPodSpec returns the worker pod spec to compute a quota footprint
// from: spec with .Overhead populated from the RuntimeClass named by
// .RuntimeClassName, mirroring what the RuntimeClass admission plugin stamps on
// the pod at create time (Q450). Pod overhead is real quota-charged capacity —
// the reference Kata shape carries 250m CPU / 160Mi memory per pod on top of its
// containers — and a footprint that ignores it reads low on exactly the most
// expensive worker shapes.
//
// Never mutates spec: the caller's spec is usually owned by the informer cache, or
// is a template applySizingProfile passed through by reference. A resolution that
// changes nothing returns spec itself, so the common (no RuntimeClass) path copies
// nothing.
//
// Deliberately FAIL-OPEN at every step, matching WorkerQuotaExhausted: a spec with
// no runtimeClassName, a RuntimeClass that does not exist or declares no overhead,
// and a read the AGC is not authorized for all yield "no overhead". Under-counting
// overhead only restores the pre-Q450 behaviour; refusing to evaluate the gate at
// all would starve a tenant over a cluster-scoped object they do not control.
//
// An overhead the tenant declared on the template wins without any read. That is
// not a shortcut: the RuntimeClass admission plugin rejects a pod whose declared
// overhead differs from its RuntimeClass's, so any declared overhead that survives
// admission is the RuntimeClass's own value.
func ResolveWorkerPodSpec(ctx context.Context, c client.Reader, spec *corev1.PodSpec) *corev1.PodSpec {
	if spec == nil {
		return nil
	}
	if len(spec.Overhead) > 0 {
		return spec
	}
	if spec.RuntimeClassName == nil || *spec.RuntimeClassName == "" {
		return spec
	}
	overhead := runtimeClassOverhead(ctx, c, *spec.RuntimeClassName)
	if len(overhead) == 0 {
		return spec
	}
	out := spec.DeepCopy()
	out.Overhead = overhead
	return out
}

// runtimeClassOverhead reads the named RuntimeClass's overhead.podFixed, or nil
// when it cannot be resolved. See ResolveWorkerPodSpec for the fail-open contract.
func runtimeClassOverhead(ctx context.Context, c client.Reader, name string) corev1.ResourceList {
	if c == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, runtimeClassReadTimeout)
	defer cancel()

	var rc nodev1.RuntimeClass
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &rc); err != nil {
		return nil
	}
	if rc.Overhead == nil {
		return nil
	}
	return rc.Overhead.PodFixed
}
