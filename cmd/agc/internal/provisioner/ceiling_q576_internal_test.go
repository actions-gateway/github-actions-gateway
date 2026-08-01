package provisioner

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The scale-set listener has to tell a full worker ceiling apart from a transient
// provisioning failure: a transient failure is retried by redelivering the queue
// message, which is immediate, while a full ceiling is still full on the next delivery
// and belongs on the re-offer backoff (Q576). That distinction rests on the ceiling
// rejection being a typed error and on the pre-mint check agreeing with it.

// activeWorkerPod builds a Running worker pod owned by the "gpu" set, so it counts
// toward that owner's ceiling.
func activeWorkerPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a",
			Name:      name,
			Labels:    map[string]string{LabelRunnerSet: "gpu"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func ceilingFixture(t *testing.T, maxWorkers int32, pods ...*corev1.Pod) (*Provisioner, *stubTarget) {
	t.Helper()
	builder := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme)
	for _, pod := range pods {
		builder = builder.WithObjects(pod)
	}
	return NewProvisioner(builder.Build(), nil, nil), &stubTarget{
		key:  client.ObjectKey{Namespace: "team-a", Name: "gpu"},
		spec: &ResolvedSpec{WorkerImage: "runner:test", MaxWorkers: &maxWorkers},
	}
}

// TestCheckScaleSetCapacity_ReportsCeilingBeforeAnythingIsMinted covers the pre-mint
// check the listener asks before it registers a runner at GitHub. It has to answer from
// the same ceiling arithmetic the authoritative check uses, or the listener would mint
// registrations for jobs the provisioner is about to reject.
func TestCheckScaleSetCapacity_ReportsCeilingBeforeAnythingIsMinted(t *testing.T) {
	ctx := context.Background()

	p, target := ceilingFixture(t, 2, activeWorkerPod("w-1"), activeWorkerPod("w-2"))
	err := p.CheckScaleSetCapacity(ctx, target)
	require.Error(t, err, "two active pods against a ceiling of two leaves no room")
	assert.True(t, IsCeilingReached(err), "the ceiling verdict must be typed, not an opaque error: %v", err)
	var ce *CeilingReachedError
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, int32(2), ce.ActivePods)

	p, target = ceilingFixture(t, 3, activeWorkerPod("w-1"), activeWorkerPod("w-2"))
	assert.NoError(t, p.CheckScaleSetCapacity(ctx, target), "one slot below the ceiling has room")
}

// TestProvisionScaleSetWorker_CeilingRejectionIsTyped pins the authoritative check —
// the one that runs with the pod about to be created, and so the one that loses the race
// when a sibling job took the last slot after the pre-check passed. It must reject with
// the same type, or that race lands back on the immediate-redelivery path this fixes.
func TestProvisionScaleSetWorker_CeilingRejectionIsTyped(t *testing.T) {
	ctx := context.Background()
	p, target := ceilingFixture(t, 1, activeWorkerPod("w-1"))

	err := p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{
		JobID:     "job-uuid-9",
		JITConfig: "eyJ4IjoxfQ==",
	})
	require.Error(t, err)
	assert.True(t, IsCeilingReached(err), "a ceiling hold must be distinguishable from a transient failure: %v", err)

	// The held job leaves nothing behind: no worker pod, and no staged Secret for a
	// job that is going to be offered again later.
	var pod corev1.Pod
	assert.Error(t, p.Client.Get(ctx, client.ObjectKey{
		Namespace: "team-a", Name: scaleSetPodName("gpu", "job-uuid-9"),
	}, &pod), "no worker pod may be created for a job the ceiling held")
	var secret corev1.Secret
	assert.Error(t, p.Client.Get(ctx, client.ObjectKey{
		Namespace: "team-a", Name: scaleSetSecretName("job-uuid-9"),
	}, &secret), "the staged Secret must be unstaged when the ceiling holds the job")
}

// TestIsCeilingReached_RejectsOtherErrors is the negative half: the predicate the
// listener routes on must not match a transient failure, or an API blip would be parked
// on the slow re-offer path instead of being retried promptly.
func TestIsCeilingReached_RejectsOtherErrors(t *testing.T) {
	assert.False(t, IsCeilingReached(nil))
	assert.False(t, IsCeilingReached(errors.New("provisioner: count active pods: etcdserver: request timed out")))
}
