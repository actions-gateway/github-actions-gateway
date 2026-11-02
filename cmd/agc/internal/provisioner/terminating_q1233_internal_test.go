package provisioner

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Q1233: the scale-set listener asks CheckScaleSetWorkerPod before it mints a JIT
// config, so a job whose previous worker pod is still terminating registers no runner on
// each re-offer. It must agree with adoptExistingScaleSetPod's verdict on a deleting pod,
// and let everything else through to it.
func TestCheckScaleSetWorkerPod(t *testing.T) {
	podName := scaleSetPodName("gpu", "job-1")
	deleting := metav1.Now()
	for _, tc := range []struct {
		name            string
		pod             *corev1.Pod
		getErr          error
		wantTerminating bool
		wantErr         bool
	}{
		{name: "no pod yet"},
		{name: "a live pod is a replay the provision adopts",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: podName}}},
		{name: "a pod being deleted holds the name",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: podName,
				DeletionTimestamp: &deleting, Finalizers: []string{"test.actions-gateway.com/hold"}}},
			wantTerminating: true},
		{name: "an unreadable pod is not a verdict",
			getErr: errors.New("etcdserver: request timed out"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme)
			if tc.pod != nil {
				b = b.WithObjects(tc.pod)
			}
			if tc.getErr != nil {
				b = b.WithInterceptorFuncs(interceptor.Funcs{
					Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
						return tc.getErr
					},
				})
			}
			p := NewProvisioner(b.Build(), nil, nil)
			err := p.CheckScaleSetWorkerPod(context.Background(), &stubTarget{key: client.ObjectKey{Namespace: "team-a", Name: "gpu"}}, "job-1")
			assert.Equal(t, tc.wantTerminating, IsWorkerTerminating(err), "err: %v", err)
			assert.Equal(t, tc.wantErr || tc.wantTerminating, err != nil, "err: %v", err)
		})
	}
}
