//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func createNSForAGC(t *testing.T, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := k8sClient.Create(ctx, ns)
	if err != nil {
		require.NoError(t, client.IgnoreNotFound(err))
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ns)
	})
}

// enqueueScaleSetJob blocks until a listener holds a session for the scale set
// named label on srv, then assigns it one job. It reports false on timeout.
func enqueueScaleSetJob(srv *scalesettest.Server, label string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if id, ok := srv.ScaleSetIDByName(label); ok && srv.HasActiveSession(id) {
			srv.EnqueueJob(id)
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// podGone reports whether the named pod no longer exists.
func podGone(nsName, name string) bool {
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: name}, &corev1.Pod{})
	return apierrors.IsNotFound(err)
}
