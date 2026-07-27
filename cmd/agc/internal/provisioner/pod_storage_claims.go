package provisioner

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// The PersistentVolumeClaim (PVC) half of a worker's quota footprint (Q453).
//
// This is a *different* quota evaluator from the one pod_effective_resources.go
// mirrors, and the distinction is the whole reason this is easy to get wrong. The
// pod evaluator charges compute (pods, requests.*, limits.*) when the pod is
// admitted. A generic ephemeral volume — `volumes[].ephemeral` — is charged by the
// PVC evaluator instead, against a *separate object*: kube-controller-manager's
// ephemeral-volume controller creates a real PVC named `<pod>-<volume>` AFTER the
// pod is admitted.
//
// So a PVC-exhausted quota does not reject the worker pod. It admits it, then fails
// the PVC create, and the pod sits Pending forever with its volume unbound — the
// job is claimed and stalls. That is precisely the outcome the Q59 pre-claim gate
// exists to prevent, which is why these keys must be in the footprint even though
// no pod create ever fails on them.
//
// Upstream reference: pkg/quota/v1/evaluator/core.persistentVolumeClaimEvaluator
// (Usage), which charges, per PVC:
//
//   - persistentvolumeclaims: 1 — a COUNT of objects, not a quantity;
//   - requests.storage: the claim's spec.resources.requests.storage;
//   - and, when the claim names a StorageClass, the same two values again under the
//     class-scoped keys below.
//
// A pod referencing a pre-existing PVC (`volumes[].persistentVolumeClaim`) creates
// nothing and is charged nothing here — that claim was charged when it was created.

// storageClassQuotaInfix joins a StorageClass name to the quota key it scopes:
// `<class>.storageclass.storage.k8s.io/persistentvolumeclaims` and
// `<class>.storageclass.storage.k8s.io/requests.storage`. Mirrors
// pkg/quota/v1/evaluator/core.V1StorageClassSuffix, which is not in the vendor tree
// (same trade as pod_effective_resources.go: mirror two constants rather than pull
// in k8s.io/kubernetes).
const storageClassQuotaInfix = ".storageclass.storage.k8s.io/"

// podClaimFootprint returns the per-pod PVC charge of spec's generic ephemeral
// volumes: the claim count and its storage ask, both unscoped and — for a volume
// that names a StorageClass — scoped to that class.
//
// Returns an empty list for a pod that declares no ephemeral volumes, which is what
// keeps a set provisioning no PVCs from being charged for them. That matters beyond
// tidiness: WorkerFootprint omits zero-valued keys, and a zero-valued
// persistentvolumeclaims key would report a violation against any quota already
// over its PVC ceiling for unrelated reasons (remaining < 0, so even 0 > remaining),
// starving a tenant whose workers use no storage at all.
//
// A volume whose claim template declares no `storage` request contributes to the
// count but not to requests.storage — the count is what the API server charges
// either way.
func podClaimFootprint(spec *corev1.PodSpec) corev1.ResourceList {
	out := corev1.ResourceList{}
	if spec == nil {
		return out
	}
	one := func() resource.Quantity { return *resource.NewQuantity(1, resource.DecimalSI) }
	for i := range spec.Volumes {
		e := spec.Volumes[i].Ephemeral
		if e == nil || e.VolumeClaimTemplate == nil {
			continue
		}
		claim := &e.VolumeClaimTemplate.Spec
		storage, hasStorage := claim.Resources.Requests[corev1.ResourceStorage]

		addQuantity(out, corev1.ResourcePersistentVolumeClaims, one())
		if hasStorage {
			addQuantity(out, corev1.ResourceRequestsStorage, storage)
		}

		// The class-scoped keys are how a platform caps expensive storage without
		// capping cheap storage, so they are the keys a Kata tenant is most likely to
		// be constrained on. An unset storageClassName resolves to the cluster default
		// at PVC creation, whose name we cannot know from the template — omitting the
		// scoped keys there is the fail-open answer, matching how an unresolvable
		// RuntimeClass yields no overhead (Q450).
		sc := claim.StorageClassName
		if sc == nil || *sc == "" {
			continue
		}
		addQuantity(out, storageClassQuotaKey(*sc, corev1.ResourcePersistentVolumeClaims), one())
		if hasStorage {
			addQuantity(out, storageClassQuotaKey(*sc, corev1.ResourceRequestsStorage), storage)
		}
	}
	return out
}

// storageClassQuotaKey returns the quota key that scopes `key` to StorageClass
// `class`.
func storageClassQuotaKey(class string, key corev1.ResourceName) corev1.ResourceName {
	return corev1.ResourceName(class + storageClassQuotaInfix + string(key))
}

// isStorageClassQuotaKey reports whether name is a class-scoped quota key. Such
// keys are named after the tenant's StorageClass, so they cannot appear in the
// static workerQuotaChecks table and are matched by shape instead. The class name
// must be non-empty, hence Index > 0 rather than >= 0.
func isStorageClassQuotaKey(name corev1.ResourceName) bool {
	return strings.Index(string(name), storageClassQuotaInfix) > 0
}
