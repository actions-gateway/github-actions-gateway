package provisioner_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Shared worker storage (Q719): the ReadWriteMany volume several workers of one
// tenant mount to pass files, which is also what ARC's `containerMode: kubernetes`
// depends on.
//
// Workers are storage-less by default and the provisioner provisions no volume of
// its own beyond the job payload and the CA projections. A shared volume is
// therefore entirely a podTemplate concern, and these tests pin the two properties
// that makes true: the claim reference survives the build, and the fsGroup that
// decides whether the runner UID can write survives it too.
//
// The live half — that such a pod actually mounts an RWX volume, on two nodes at
// once, and that WITHOUT fsGroup the write is refused — is
// worker_shared_storage_live_test.go, against the kind cluster
// scripts/e2e/rwx-storage-cluster.sh stands up. Reference architecture and the
// storage classes exercised: docs/operations/worker-shared-storage.md.

const sharedClaimName = "team-a-shared"

// sharedStorageTemplate returns a podTemplate mounting a pre-existing RWX claim
// into the runner container, with the fsGroup that makes it writable.
func sharedStorageTemplate(fsGroup *int64) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{FSGroup: fsGroup},
			Containers: []corev1.Container{{
				Name: "runner",
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "shared",
					MountPath: "/mnt/shared",
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "shared",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: sharedClaimName,
					},
				},
			}},
		},
	}
}

func volumeByName(pod *corev1.Pod, name string) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

func mountByName(c *corev1.Container, name string) *corev1.VolumeMount {
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == name {
			return &c.VolumeMounts[i]
		}
	}
	return nil
}

// TestBuildPod_SharedClaimSurvivesInjection verifies a tenant's RWX claim reaches
// the worker pod alongside the volumes the provisioner injects. The provisioner
// appends job-payload (and the CA projections) to whatever the template declares,
// so the failure this guards against is an append becoming an assignment — which
// would drop the tenant's volume and leave the mount dangling, a pod the kubelet
// rejects rather than one that silently loses its storage.
func TestBuildPod_SharedClaimSurvivesInjection(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)

	rg := newRG("mygroup", "team-a")
	rg.Spec.PodTemplate = sharedStorageTemplate(ptr.To(int64(1001)))

	pod := runAndGetPod(ctx, t, p, fc, rg, "plan-rwx", "team-a")

	vol := volumeByName(pod, "shared")
	require.NotNil(t, vol, "the tenant's shared volume must survive the provisioner's own injections")
	require.NotNil(t, vol.PersistentVolumeClaim, "the volume must stay a claim reference, not be rewritten")
	assert.Equal(t, sharedClaimName, vol.PersistentVolumeClaim.ClaimName)

	// And the job payload is still there: an assertion that only checked the
	// tenant's volume would pass just as well if the injection had stopped happening.
	assert.NotNil(t, volumeByName(pod, "job-payload"), "the injected payload volume must still be present")

	var runner *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "runner" {
			runner = &pod.Spec.Containers[i]
		}
	}
	require.NotNil(t, runner)
	mount := mountByName(runner, "shared")
	require.NotNil(t, mount, "the runner container must keep its mount of the shared volume")
	assert.Equal(t, "/mnt/shared", mount.MountPath)
	assert.False(t, mount.ReadOnly, "a shared job volume is read-write; jobs pass files through it")
}

// TestBuildPod_FSGroupSurvivesSecurityDefaults verifies applySecurityDefaults never
// overwrites a tenant's fsGroup, on every profile.
//
// This is the field the shared volume turns on. Measured 2026-08-24 against
// csi-driver-nfs v4.13.4 in kind: a freshly provisioned RWX volume's root is
// root:root 0755, so the worker's gap-filled UID 1001 gets EACCES on write; with
// fsGroup: 1001 the kubelet leaves it root:1001 drwxrwsr-x and two workers on
// different nodes read and write each other's files. A profile that dropped the
// field would make shared storage unusable at exactly the isolation levels a
// multi-tenant platform runs.
func TestBuildPod_FSGroupSurvivesSecurityDefaults(t *testing.T) {
	for _, profile := range []string{"", "baseline", "restricted"} {
		t.Run("profile="+profile, func(t *testing.T) {
			defer goleak.VerifyNone(t)
			ctx := context.Background()
			fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
			p := newProvisioner(fc)
			p.SecurityProfile = profile

			rg := newRG("mygroup", "team-a")
			rg.Spec.PodTemplate = sharedStorageTemplate(ptr.To(int64(1001)))

			pod := runAndGetPod(ctx, t, p, fc, rg, "plan-rwx-"+profile, "team-a")

			require.NotNil(t, pod.Spec.SecurityContext)
			require.NotNil(t, pod.Spec.SecurityContext.FSGroup,
				"a shared RWX volume is unwritable by the runner UID without fsGroup")
			assert.Equal(t, int64(1001), *pod.Spec.SecurityContext.FSGroup)
			// The gap-filled UID is what fsGroup has to line up with, so a change to
			// either without the other breaks the reference architecture.
			require.NotNil(t, pod.Spec.SecurityContext.RunAsUser)
			assert.Equal(t, *pod.Spec.SecurityContext.RunAsUser, *pod.Spec.SecurityContext.FSGroup,
				"docs/operations/worker-shared-storage.md tells operators to set fsGroup to the runner UID")
		})
	}
}

// TestBuildPod_NoFSGroupIsLeftUnset verifies the provisioner does not invent an
// fsGroup for a tenant that declared none. It is the control for the test above:
// without it, a provisioner that stamped fsGroup unconditionally would satisfy
// every assertion there while making the field's origin untraceable — and the
// live test's negative case, which needs the field genuinely absent, would stop
// being reachable at all.
func TestBuildPod_NoFSGroupIsLeftUnset(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	p.SecurityProfile = "restricted"

	rg := newRG("mygroup", "team-a")
	rg.Spec.PodTemplate = sharedStorageTemplate(nil)

	pod := runAndGetPod(ctx, t, p, fc, rg, "plan-rwx-nofsgroup", "team-a")

	require.NotNil(t, pod.Spec.SecurityContext)
	assert.Nil(t, pod.Spec.SecurityContext.FSGroup,
		"fsGroup is the tenant's to set; stamping one would silently chown every volume a worker mounts")
}
