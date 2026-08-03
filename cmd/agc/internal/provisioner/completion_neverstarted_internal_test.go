package provisioner

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// getSequenceClient serves a scripted sequence of Get results for one pod, so a test
// can pin what the poll fallback observed before the pod vanished. Each entry is
// returned once, in order; a nil entry (and every Get past the end) is NotFound.
type getSequenceClient struct {
	client.Client
	mu   sync.Mutex
	seq  []*corev1.Pod
	next int
}

func (c *getSequenceClient) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var cur *corev1.Pod
	if c.next < len(c.seq) {
		cur = c.seq[c.next]
		c.next++
	}
	if cur == nil {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, key.Name)
	}
	cur.DeepCopyInto(obj.(*corev1.Pod))
	return nil
}

// The poll fallback resolves a vanished pod from what it last saw, so whether the
// worker ever ran has to be carried the same way the preemption marker is (Q628).
func TestWaitForPodCompletion_Q628_DeletedBeforeStart(t *testing.T) {
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "runner",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
			}},
		},
	}

	for _, tc := range []struct {
		name string
		seq  []*corev1.Pod
		want bool
	}{
		{"reaped while pending", []*corev1.Pod{pending, nil}, true},
		{"deleted after the runner started", []*corev1.Pod{running, nil}, false},
		{"gone before the first tick", []*corev1.Pod{nil}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Provisioner{Client: &getSequenceClient{seq: tc.seq}, PollInterval: time.Millisecond}
			out, err := p.waitForPodCompletion(context.Background(), "ns", "p")
			if err != nil {
				t.Fatalf("waitForPodCompletion: %v", err)
			}
			if out.Phase != corev1.PodSucceeded {
				t.Fatalf("got phase=%q, want Succeeded — the delete path's phase is unchanged", out.Phase)
			}
			if out.DeletedBeforeStart != tc.want {
				t.Fatalf("DeletedBeforeStart=%v, want %v", out.DeletedBeforeStart, tc.want)
			}
		})
	}
}
