package provisioner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Q453: the storage half of a worker's quota footprint. The compute keys were the
// only ones mapped, so ephemeral-storage, persistentvolumeclaims and
// requests.storage never reached the conditions, the pre-claim gate, or the
// scale-set capacity integer. A Kata set's per-worker PVCs were free.
//
// The PVC keys fail differently from the compute keys, which is why they need
// covering separately: an exhausted PVC quota does NOT reject the worker pod — the
// pod is admitted and the ephemeral-volume controller's PVC create fails afterward,
// leaving the pod Pending with an unbound volume and the job claimed. Under-counting
// here buys exactly the claim-and-stall the gate exists to prevent.

// ephemeralVolume builds a generic ephemeral volume: `volumes[].ephemeral`, the
// declaration that makes Kubernetes create a per-pod PVC named `<pod>-<volume>`.
// An empty storage or class drops that field, covering the partial shapes.
func ephemeralVolume(name, storage, class string) corev1.Volume {
	claim := corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
	}
	if storage != "" {
		claim.Resources = corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(storage)},
		}
	}
	if class != "" {
		claim.StorageClassName = ptr.To(class)
	}
	return corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			Ephemeral: &corev1.EphemeralVolumeSource{
				VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{Spec: claim},
			},
		},
	}
}

// assertKey asserts a footprint key's rendered value, or its absence for want "".
func assertKey(t *testing.T, fp corev1.ResourceList, key corev1.ResourceName, want string) {
	t.Helper()
	got, ok := fp[key]
	if want == "" {
		assert.False(t, ok, "%s must be absent, got %v", key, got)
		return
	}
	require.True(t, ok, "%s must be present", key)
	assert.Equal(t, want, got.String(), "%s", key)
}

// TestWorkerFootprint_KataPerWorkerPVC is the headline Q453 regression, on the
// shape from deploy/dogfood-e2e/overlays/kata/resources.yaml: a 100Gi raw block
// device per worker, declared as a generic ephemeral volume so each ephemeral pod
// gets its own PVC. At maxWorkers 4 that is 4 PVCs and 400Gi of storage the
// footprint used to report as zero.
func TestWorkerFootprint_KataPerWorkerPVC(t *testing.T) {
	spec := &corev1.PodSpec{
		RuntimeClassName: ptr.To("kata"),
		Containers:       []corev1.Container{{Name: WorkerContainerName}},
		Volumes:          []corev1.Volume{ephemeralVolume("docker-blk", "100Gi", "standard-rwo")},
	}

	one := WorkerFootprint(spec, 1)
	assertKey(t, one, corev1.ResourcePersistentVolumeClaims, "1")
	assertKey(t, one, corev1.ResourceRequestsStorage, "100Gi")

	// Linear in count, and the count key stays an integer object count rather than
	// picking up the storage quantity's units.
	four := WorkerFootprint(spec, 4)
	assertKey(t, four, corev1.ResourcePersistentVolumeClaims, "4")
	assertKey(t, four, corev1.ResourceRequestsStorage, "400Gi")

	// The class-scoped keys carry the same two values: this is how a platform caps
	// expensive storage without capping cheap storage, so it is the key a Kata
	// tenant is most likely to actually be constrained on.
	assertKey(t, four, "standard-rwo.storageclass.storage.k8s.io/persistentvolumeclaims", "4")
	assertKey(t, four, "standard-rwo.storageclass.storage.k8s.io/requests.storage", "400Gi")
}

// TestWorkerFootprint_NoEphemeralVolumesChargesNothing is the other half of the
// requirement, and the one whose failure mode is starvation rather than stall: a
// set that provisions no PVCs must not be charged for them. Emitting a zero-valued
// persistentvolumeclaims key would report a violation against any quota already at
// or over its PVC ceiling for unrelated reasons — remaining <= 0, and 0 > remaining
// once it goes negative — closing the gate on a tenant whose workers touch no
// storage at all.
func TestWorkerFootprint_NoEphemeralVolumesChargesNothing(t *testing.T) {
	// A pod with no volumes, a pod with a non-ephemeral volume, and a pod mounting a
	// PRE-EXISTING PVC — that claim was charged when it was created, and referencing
	// it creates nothing.
	for name, spec := range map[string]*corev1.PodSpec{
		"no volumes": {Containers: []corev1.Container{{Name: WorkerContainerName}}},
		"emptyDir only": {
			Containers: []corev1.Container{{Name: WorkerContainerName}},
			Volumes: []corev1.Volume{{
				Name:         "scratch",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
		},
		"pre-existing claim": {
			Containers: []corev1.Container{{Name: WorkerContainerName}},
			Volumes: []corev1.Volume{{
				Name: "shared",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: "team-cache",
				}},
			}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			fp := WorkerFootprint(spec, 5)
			assertKey(t, fp, corev1.ResourcePersistentVolumeClaims, "")
			assertKey(t, fp, corev1.ResourceRequestsStorage, "")
		})
	}
}

// TestWorkerFootprint_ClaimCountIsCardinality pins the count-versus-quantity
// distinction. persistentvolumeclaims is a count of OBJECTS: two ephemeral volumes
// on one pod is two PVCs regardless of how much storage each asks for, while
// requests.storage sums the asks. Deriving the count from the storage total (or
// vice versa) would be wrong in both directions.
func TestWorkerFootprint_ClaimCountIsCardinality(t *testing.T) {
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: WorkerContainerName}},
		Volumes: []corev1.Volume{
			ephemeralVolume("docker-blk", "100Gi", "standard-rwo"),
			ephemeralVolume("workspace", "20Gi", "standard-rwo"),
		},
	}

	fp := WorkerFootprint(spec, 3)
	assertKey(t, fp, corev1.ResourcePersistentVolumeClaims, "6") // 2 volumes x 3 pods
	assertKey(t, fp, corev1.ResourceRequestsStorage, "360Gi")    // (100+20)Gi x 3
	assertKey(t, fp, "standard-rwo.storageclass.storage.k8s.io/persistentvolumeclaims", "6")
	assertKey(t, fp, "standard-rwo.storageclass.storage.k8s.io/requests.storage", "360Gi")
}

// TestWorkerFootprint_PartialClaimShapes covers the two ways a claim template can
// leave a term unresolvable, both of which must degrade rather than guess.
func TestWorkerFootprint_PartialClaimShapes(t *testing.T) {
	// No storageClassName: the PVC resolves to the cluster default at creation, whose
	// name the template does not tell us. The unscoped keys still apply.
	noClass := WorkerFootprint(&corev1.PodSpec{
		Containers: []corev1.Container{{Name: WorkerContainerName}},
		Volumes:    []corev1.Volume{ephemeralVolume("scratch", "50Gi", "")},
	}, 2)
	assertKey(t, noClass, corev1.ResourcePersistentVolumeClaims, "2")
	assertKey(t, noClass, corev1.ResourceRequestsStorage, "100Gi")
	for key := range noClass {
		assert.False(t, isStorageClassQuotaKey(key),
			"an unset storageClassName must not invent a class-scoped key, got %s", key)
	}

	// No storage request: the object still exists, so it still counts against
	// persistentvolumeclaims — just not against requests.storage.
	noStorage := WorkerFootprint(&corev1.PodSpec{
		Containers: []corev1.Container{{Name: WorkerContainerName}},
		Volumes:    []corev1.Volume{ephemeralVolume("scratch", "", "standard-rwo")},
	}, 2)
	assertKey(t, noStorage, corev1.ResourcePersistentVolumeClaims, "2")
	assertKey(t, noStorage, corev1.ResourceRequestsStorage, "")
	assertKey(t, noStorage, "standard-rwo.storageclass.storage.k8s.io/persistentvolumeclaims", "2")
	assertKey(t, noStorage, "standard-rwo.storageclass.storage.k8s.io/requests.storage", "")
}

// TestWorkerFootprint_EphemeralStorage verifies local ephemeral-storage is summed
// exactly like cpu and memory — across regular containers AND native sidecars, with
// the same requests/limits split. A DinD sidecar's image layers live in its own
// ephemeral storage, so this is the key that silently binds first on a
// build-heavy tenant.
func TestWorkerFootprint_EphemeralStorage(t *testing.T) {
	spec := &corev1.PodSpec{
		InitContainers: []corev1.Container{
			sidecar("dind", rl("ephemeral-storage", "20Gi"), rl("ephemeral-storage", "40Gi")),
		},
		Containers: []corev1.Container{{
			Name: WorkerContainerName,
			Resources: corev1.ResourceRequirements{
				Requests: rl("ephemeral-storage", "10Gi"),
				Limits:   rl("ephemeral-storage", "30Gi"),
			},
		}},
	}

	fp := WorkerFootprint(spec, 2)
	assertKey(t, fp, corev1.ResourceRequestsEphemeralStorage, "60Gi") // (10+20)Gi x 2
	assertKey(t, fp, corev1.ResourceLimitsEphemeralStorage, "140Gi")  // (30+40)Gi x 2

	// A shape declaring no ephemeral storage is charged nothing: the gap-fill stamps
	// only cpu/memory, so inventing a default here would over-count every tenant.
	bare := WorkerFootprint(&corev1.PodSpec{Containers: []corev1.Container{{Name: WorkerContainerName}}}, 2)
	assertKey(t, bare, corev1.ResourceRequestsEphemeralStorage, "")
	assertKey(t, bare, corev1.ResourceLimitsEphemeralStorage, "")
}

// storageQuota builds a namespace ResourceQuota constraining a single key.
func storageQuota(key corev1.ResourceName, hard, used string) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "q", Namespace: "ns"},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{key: resource.MustParse(hard)}},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{key: resource.MustParse(hard)},
			Used: corev1.ResourceList{key: resource.MustParse(used)},
		},
	}
}

// kataWorkerSpec is the reference Kata worker reduced to what the quota sees: one
// 100Gi per-worker PVC on the standard-rwo class.
func kataWorkerSpec() *corev1.PodSpec {
	return &corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:      WorkerContainerName,
			Resources: corev1.ResourceRequirements{Requests: rl("cpu", "2", "ephemeral-storage", "10Gi")},
		}},
		Volumes: []corev1.Volume{ephemeralVolume("docker-blk", "100Gi", "standard-rwo")},
	}
}

// TestWorkerQuotaExhausted_ClosesOnStorageKeys is the end-to-end assertion that the
// new keys reach the pre-claim gate (the #784 quota rung), one key at a time so a
// regression names the key it lost. Each case exhausts exactly one key and leaves
// every other unconstrained.
func TestWorkerQuotaExhausted_ClosesOnStorageKeys(t *testing.T) {
	tests := []struct {
		name  string
		quota *corev1.ResourceQuota
		want  string // substring the detail must name
	}{
		{"pvc count", storageQuota(corev1.ResourcePersistentVolumeClaims, "4", "4"), "persistentvolumeclaims"},
		{"requests.storage", storageQuota(corev1.ResourceRequestsStorage, "400Gi", "350Gi"), "requests.storage"},
		{"requests.ephemeral-storage", storageQuota(corev1.ResourceRequestsEphemeralStorage, "100Gi", "95Gi"), "requests.ephemeral-storage"},
		{"legacy ephemeral-storage alias", storageQuota(corev1.ResourceEphemeralStorage, "100Gi", "95Gi"), "ephemeral-storage"},
		{
			"class-scoped pvc count",
			storageQuota("standard-rwo.storageclass.storage.k8s.io/persistentvolumeclaims", "8", "8"),
			"standard-rwo.storageclass.storage.k8s.io/persistentvolumeclaims",
		},
		{
			"class-scoped storage",
			storageQuota("standard-rwo.storageclass.storage.k8s.io/requests.storage", "1Ti", "980Gi"),
			"standard-rwo.storageclass.storage.k8s.io/requests.storage",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := fake.NewClientBuilder().WithScheme(quotaTestScheme()).
				WithRuntimeObjects(tt.quota).Build()

			exhausted, detail := WorkerQuotaExhausted(context.Background(), fc, "ns", kataWorkerSpec())

			assert.True(t, exhausted, "an exhausted %s must close the gate", tt.name)
			assert.Contains(t, detail, tt.want, "the detail must name the binding resource")
		})
	}
}

// TestWorkerQuotaExhausted_StorageHeadroomAdmits is the negative control for the
// test above: the same shape against the same keys with room left must NOT close
// the gate. Without it, a footprint that over-counted every storage key would still
// pass the positive cases while starving every Kata tenant.
func TestWorkerQuotaExhausted_StorageHeadroomAdmits(t *testing.T) {
	for _, q := range []*corev1.ResourceQuota{
		storageQuota(corev1.ResourcePersistentVolumeClaims, "4", "3"),
		storageQuota(corev1.ResourceRequestsStorage, "400Gi", "300Gi"),
		storageQuota(corev1.ResourceRequestsEphemeralStorage, "100Gi", "90Gi"),
		storageQuota("standard-rwo.storageclass.storage.k8s.io/requests.storage", "1Ti", "500Gi"),
	} {
		exhausted, detail := WorkerQuotaExhausted(context.Background(),
			fake.NewClientBuilder().WithScheme(quotaTestScheme()).WithRuntimeObjects(q).Build(),
			"ns", kataWorkerSpec())
		assert.False(t, exhausted, "headroom on %v must admit", q.Spec.Hard)
		assert.Empty(t, detail)
	}
}

// TestWorkerQuotaExhausted_NoPVCsIgnoresAnOverusedClaimQuota is the starvation guard
// at the gate. A namespace whose persistentvolumeclaims quota is already fully used
// by something else entirely (a StatefulSet, a tenant's own cache) must not block a
// worker that creates no PVCs.
func TestWorkerQuotaExhausted_NoPVCsIgnoresAnOverusedClaimQuota(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(quotaTestScheme()).
		WithRuntimeObjects(storageQuota(corev1.ResourcePersistentVolumeClaims, "4", "4")).Build()

	exhausted, detail := WorkerQuotaExhausted(context.Background(), fc, "ns",
		&corev1.PodSpec{Containers: []corev1.Container{{Name: WorkerContainerName}}})

	assert.False(t, exhausted, "a worker that creates no PVC must not be charged for one")
	assert.Empty(t, detail)
}

// TestQuotaHeadroomPods_BoundsOnClaimCount verifies the scale-set tier reads the
// same storage arithmetic: the capacity integer a RunnerSet advertises to GitHub
// must be bounded by the PVC quota, not just by cpu/memory. Advertising more than
// the PVCs allow is how a set claims jobs that then sit Pending.
func TestQuotaHeadroomPods_BoundsOnClaimCount(t *testing.T) {
	// 6 free claims, 1 claim per worker ⇒ 6 workers fit even though max is 10.
	quotas := []corev1.ResourceQuota{*storageQuota(corev1.ResourcePersistentVolumeClaims, "10", "4")}
	assert.Equal(t, int32(6), QuotaHeadroomPods(kataWorkerSpec(), quotas, 10))

	// 250Gi free at 100Gi per worker ⇒ 2 fit; the fractional third does not.
	quotas = []corev1.ResourceQuota{*storageQuota(corev1.ResourceRequestsStorage, "1Ti", "774Gi")}
	assert.Equal(t, int32(2), QuotaHeadroomPods(kataWorkerSpec(), quotas, 10))

	// Fully consumed ⇒ nothing fits, and WorkerQuotaCapacity converts that delta of
	// zero back into "only what is already running".
	fc := fake.NewClientBuilder().WithScheme(quotaTestScheme()).
		WithRuntimeObjects(storageQuota(corev1.ResourcePersistentVolumeClaims, "4", "4")).Build()
	limit, bounded := WorkerQuotaCapacity(context.Background(), fc, "ns", kataWorkerSpec(), 4, 10)
	require.True(t, bounded)
	assert.Equal(t, int32(4), limit, "capacity must be the active workers alone")
}

// TestQuotaChecksFor_ScopedKeysAreSortedAndAdditive verifies the derived class-scoped
// checks extend the fixed table rather than replacing it, and land in a stable order
// so a multi-class pod's condition message does not churn between reconciles.
func TestQuotaChecksFor_ScopedKeysAreSortedAndAdditive(t *testing.T) {
	demand := corev1.ResourceList{
		corev1.ResourcePods: *resource.NewQuantity(1, resource.DecimalSI),
		"ssd.storageclass.storage.k8s.io/requests.storage":       resource.MustParse("100Gi"),
		"hdd.storageclass.storage.k8s.io/requests.storage":       resource.MustParse("1Ti"),
		"hdd.storageclass.storage.k8s.io/persistentvolumeclaims": *resource.NewQuantity(1, resource.DecimalSI),
	}

	checks := quotaChecksFor(demand)
	require.Len(t, checks, len(workerQuotaChecks)+3)
	assert.Equal(t, workerQuotaChecks, checks[:len(workerQuotaChecks)], "the fixed table must be preserved")

	scoped := checks[len(workerQuotaChecks):]
	assert.Equal(t, []quotaCheck{
		{"hdd.storageclass.storage.k8s.io/persistentvolumeclaims", "hdd.storageclass.storage.k8s.io/persistentvolumeclaims"},
		{"hdd.storageclass.storage.k8s.io/requests.storage", "hdd.storageclass.storage.k8s.io/requests.storage"},
		{"ssd.storageclass.storage.k8s.io/requests.storage", "ssd.storageclass.storage.k8s.io/requests.storage"},
	}, scoped)

	// A demand with no scoped keys must not allocate a second table.
	assert.Equal(t, len(workerQuotaChecks), len(quotaChecksFor(corev1.ResourceList{corev1.ResourcePods: *resource.NewQuantity(1, resource.DecimalSI)})))
}
