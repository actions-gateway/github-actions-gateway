//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	ctrl "sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// TestInformerPodWaiter_RealInformer proves the production wiring: an
// InformerPodWaiter registered on a real manager cache resolves a waiter when
// the shared Pod informer observes the pod reach a terminal phase — no polling.
func TestInformerPodWaiter_RealInformer(t *testing.T) {
	mgrCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)

	skipNameValidation := true
	mgr, err := ctrl.New(testEnv.Config, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
	})
	require.NoError(t, err)

	waiter := provisioner.NewInformerPodWaiter(mgr.GetCache(), nil)
	require.NoError(t, mgr.Add(waiter))

	mgrDone := make(chan struct{})
	go func() {
		defer close(mgrDone)
		_ = mgr.Start(mgrCtx)
	}()
	t.Cleanup(func() { cancel(); <-mgrDone })

	// Wait until the manager cache is started and the Pod informer is serving
	// reads (a List both starts and blocks for the informer to sync). This
	// races against mgr.Start in the goroutine above, so poll until it succeeds.
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		return mgr.GetCache().List(ctx, &pods, client.InNamespace("default")) == nil
	}, 10*time.Second, 50*time.Millisecond, "manager cache never became ready")

	const ns, name = "default", "waiter-it-pod"
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, p))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), p)
	})

	// Block on completion in the background, then drive the pod terminal.
	type result struct {
		phase  corev1.PodPhase
		reason string
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		ph, rs, err := waiter.WaitForCompletion(mgrCtx, ns, name)
		resCh <- result{ph, rs, err}
	}()

	// Give the waiter a beat to register, then transition the pod to Succeeded.
	// The informer event — not a poll — is what must unblock the waiter.
	time.Sleep(100 * time.Millisecond)
	var fresh corev1.Pod
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &fresh))
	fresh.Status.Phase = corev1.PodSucceeded
	require.NoError(t, k8sClient.Status().Update(ctx, &fresh))

	select {
	case r := <-resCh:
		require.NoError(t, r.err)
		require.Equal(t, corev1.PodSucceeded, r.phase)
	case <-time.After(10 * time.Second):
		t.Fatal("WaitForCompletion did not resolve after pod reached Succeeded")
	}
}
