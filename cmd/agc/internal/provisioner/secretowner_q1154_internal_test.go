package provisioner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kuuid "k8s.io/apimachinery/pkg/util/uuid"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// uidAssigningClient is a fake client that gives each created object a UID, as the API
// server does, so an owner reference has a UID to carry and the collector one to check.
func uidAssigningClient() client.WithWatch {
	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithStatusSubresource(&corev1.Pod{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetUID() == "" {
					obj.SetUID(kuuid.NewUUID())
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
}

// Q1154: a scale-set job-b ran on the worker created for job-a, and that worker was
// deleted before job-b's completion arrived (a delayed listener, or a hand-run delete).
// The completion finds no pod carrying the runner's name, and the worker created for
// job-b is still there, so the fallback leaves job-a's Secret, which holds a runner
// credential, to the RunnerSet. The Secret's lifetime has to follow the pod instead.

// collectOrphanedSecrets stands in for the garbage collector over the one owner kind
// this change introduces: it deletes each Secret whose owners are all Pods that no
// longer exist. A Secret any non-Pod object owns is left alone, as the real collector
// leaves one whose RunnerSet is still present.
func collectOrphanedSecrets(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	var secrets corev1.SecretList
	require.NoError(t, c.List(ctx, &secrets))
	for i := range secrets.Items {
		s := &secrets.Items[i]
		orphaned := len(s.OwnerReferences) > 0
		for _, ref := range s.OwnerReferences {
			if ref.Kind != "Pod" {
				orphaned = false
				break
			}
			var pod corev1.Pod
			err := c.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: ref.Name}, &pod)
			if err == nil && pod.UID == ref.UID {
				orphaned = false
				break
			}
			if err != nil && !apierrors.IsNotFound(err) {
				require.NoError(t, err)
			}
		}
		if orphaned {
			require.NoError(t, c.Delete(ctx, s))
		}
	}
}

// TestScaleSetSecret_GoesWithAWorkerDeletedBeforeItsJobCompletes is the row's case end
// to end: the worker created for job-a ran job-b and is deleted, the worker created for
// job-b is still present, and job-b's completion arrives naming job-a's runner.
func TestScaleSetSecret_GoesWithAWorkerDeletedBeforeItsJobCompletes(t *testing.T) {
	ctx := context.Background()
	fc := uidAssigningClient()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})

	ranB := provisionHolder(ctx, t, p, target, "job-a")
	provisionHolder(ctx, t, p, target, "job-b")
	require.NoError(t, p.MarkScaleSetJobStarted(ctx, target, "job-b", "gpu-job-a"))

	require.NoError(t, fc.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ranB.Namespace, Name: ranB.Name}}))
	collectOrphanedSecrets(ctx, t, fc)
	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-b", "gpu-job-a"))

	assert.False(t, secretExists(ctx, t, fc, "team-a", "job-a"),
		"the Secret job-a's worker mounted must go with that worker, not wait for the RunnerSet")
	assert.True(t, secretExists(ctx, t, fc, "team-a", "job-b"),
		"the worker created for job-b is alive and still mounts its Secret")
}

// TestProvisionScaleSetWorker_HandsTheSecretToItsPod pins the ownership itself, on both
// the first delivery and a replay that finds the pod already there.
func TestProvisionScaleSetWorker_HandsTheSecretToItsPod(t *testing.T) {
	ctx := context.Background()
	fc := uidAssigningClient()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})

	key := provisionHolder(ctx, t, p, target, "job-a")
	var pod corev1.Pod
	require.NoError(t, fc.Get(ctx, key, &pod))
	require.NotEmpty(t, pod.UID)

	for _, delivery := range []string{"first", "replay"} {
		if delivery == "replay" {
			// A Secret staged by an AGC that stopped before handing it over, which the
			// replay must hand over now.
			var staged corev1.Secret
			require.NoError(t, fc.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: scaleSetSecretName("job-a")}, &staged))
			staged.OwnerReferences = []metav1.OwnerReference{target.OwnerRef()}
			require.NoError(t, fc.Update(ctx, &staged))
			provisionHolder(ctx, t, p, target, "job-a")
		}
		var secret corev1.Secret
		require.NoError(t, fc.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: scaleSetSecretName("job-a")}, &secret))
		require.Len(t, secret.OwnerReferences, 1, delivery)
		ref := secret.OwnerReferences[0]
		assert.Equal(t, "Pod", ref.Kind, delivery)
		assert.Equal(t, pod.Name, ref.Name, delivery)
		assert.Equal(t, pod.UID, ref.UID, delivery)
	}
}
