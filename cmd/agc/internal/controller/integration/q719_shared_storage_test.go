//go:build integration

package integration_test

import (
	"context"
	"testing"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

// Shared worker storage against the real apiserver (Q719).
//
// The unit half (cmd/agc/internal/provisioner/worker_shared_storage_test.go) drives
// the pod builder against a fake client, which cannot fail on a schema: it holds Go
// structs. The RunnerTemplate CustomResourceDefinition is 1.2 MB of generated
// OpenAPI wrapping a full PodTemplateSpec, and a field it fails to publish is
// silently dropped on write — the tenant applies a claim reference, the apiserver
// stores a template without one, and the worker starts with no shared volume and no
// error anywhere. Only a real apiserver can observe that.
//
// The live half, that such a pod actually mounts an RWX volume on two nodes, is
// cmd/agc/internal/provisioner/worker_shared_storage_live_test.go and needs a
// cluster. Reference architecture: docs/operations/worker-shared-storage.md.

// sharedStorageRunnerTemplate builds a template mounting a pre-existing RWX claim
// into the runner container, with the fsGroup that makes it writable.
func sharedStorageRunnerTemplate(name, ns string) *v2alpha1.RunnerTemplate {
	return &v2alpha1.RunnerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v2alpha1.RunnerTemplateSpec{
			WorkerImage: "runner:test",
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						FSGroup:             ptr.To(int64(1001)),
						FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
					},
					Containers: []corev1.Container{{
						Name:  "runner",
						Image: "runner:test",
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "shared",
							MountPath: "/mnt/shared",
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "shared",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: "team-a-shared",
							},
						},
					}},
				},
			},
		},
	}
}

// TestV2_RunnerTemplate_SharedRWXClaimRoundTrips verifies the shared-storage shape
// survives the CRD schema: the claim reference, the mount, and both fsGroup fields
// come back exactly as written. Each is a field the operator doc instructs a tenant
// to set, and a schema that dropped any of them would make that doc wrong with
// nothing red anywhere.
func TestV2_RunnerTemplate_SharedRWXClaimRoundTrips(t *testing.T) {
	const ns = "v2-rt-shared-storage"
	createNSForAGC(t, ns)

	tmpl := sharedStorageRunnerTemplate("tmpl", ns)
	require.NoError(t, k8sClient.Create(ctx, tmpl))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	var got v2alpha1.RunnerTemplate
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "tmpl"}, &got))

	spec := got.Spec.PodTemplate.Spec
	require.Len(t, spec.Volumes, 1, "the claim reference must survive the CRD schema")
	require.NotNil(t, spec.Volumes[0].PersistentVolumeClaim,
		"the volume came back without its persistentVolumeClaim source")
	assert.Equal(t, "team-a-shared", spec.Volumes[0].PersistentVolumeClaim.ClaimName)

	require.Len(t, spec.Containers, 1)
	require.Len(t, spec.Containers[0].VolumeMounts, 1, "the runner's mount must survive the CRD schema")
	assert.Equal(t, "/mnt/shared", spec.Containers[0].VolumeMounts[0].MountPath)

	require.NotNil(t, spec.SecurityContext)
	require.NotNil(t, spec.SecurityContext.FSGroup,
		"fsGroup is what lets the runner UID write to the shared volume; a schema that drops it "+
			"makes docs/operations/worker-shared-storage.md instruct a tenant to set a field that never lands")
	assert.Equal(t, int64(1001), *spec.SecurityContext.FSGroup)
	require.NotNil(t, spec.SecurityContext.FSGroupChangePolicy)
	assert.Equal(t, corev1.FSGroupChangeOnRootMismatch, *spec.SecurityContext.FSGroupChangePolicy)
}

// TestV2_RunnerSet_SharedRWXClaimReachesReady verifies nothing in the resolution
// path rejects a template carrying a claim reference: the set reaches Ready the same
// way a storage-less one does.
//
// The claim itself is deliberately never created. A worker mounting a missing claim
// would sit Pending, but a RunnerSet is Ready when its listener is active, and
// coupling readiness to a tenant's storage would stop the gateway from claiming jobs
// over a volume it does not own.
func TestV2_RunnerSet_SharedRWXClaimReachesReady(t *testing.T) {
	const ns = "v2-rs-shared-storage"
	createNSForAGC(t, ns)
	startRunnerSetReconciler(t)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, ""))) // direct egress
	require.NoError(t, k8sClient.Create(ctx, sharedStorageRunnerTemplate("tmpl", ns)))
	rs := newRunnerSet("set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.ActionsGateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	waitForSetReadyReason(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)
}
