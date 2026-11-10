package main

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
)

// TestAwaitEgressAllowlistConfigMap pins the two halves of gating on a ConfigMap
// that loads late: a configured one makes pools hold until it is read and gets a
// change channel, and an unconfigured one does neither (Q1322).
func TestAwaitEgressAllowlistConfigMap(t *testing.T) {
	al := allowlist.NewEgressDestination(nil, nil)
	if ch := awaitEgressAllowlistConfigMap(&gmcFlags{}, al); ch != nil || al.DynamicPending() {
		t.Errorf("no ConfigMap: channel = %v, pending = %v; want nil, false", ch, al.DynamicPending())
	}

	ch := awaitEgressAllowlistConfigMap(&gmcFlags{egressDestinationAllowlistConfigMap: "egress-allowlist"}, al)
	if cap(ch) != 1 {
		t.Errorf("channel capacity = %d, want 1 so a send never blocks the ConfigMap reconciler", cap(ch))
	}
	if !al.DynamicPending() {
		t.Error("a configured ConfigMap must hold pools until its first read")
	}
	// The hold ends at a deadline, so an unread ConfigMap cannot freeze a pool. It
	// has to outlast a normal cache sync and stay short enough to wait out.
	if egressAllowlistConfigMapWait < 5*time.Second || egressAllowlistConfigMapWait > time.Minute {
		t.Errorf("egressAllowlistConfigMapWait = %v, want a short positive bound", egressAllowlistConfigMapWait)
	}
}

func TestDeploymentOwnerRef(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "gmc-system", Name: "gmc", UID: "d-uid"}}
	r := fake.NewClientBuilder().WithScheme(s).WithObjects(d).Build()

	ref, err := deploymentOwnerRef(context.Background(), r, "gmc-system", "gmc")
	if err != nil {
		t.Fatalf("deploymentOwnerRef: %v", err)
	}
	want := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "gmc", UID: "d-uid"}
	if *ref != want {
		t.Errorf("owner = %+v, want %+v", *ref, want)
	}

	if ref, err := deploymentOwnerRef(context.Background(), r, "gmc-system", "missing"); err == nil || ref != nil {
		t.Errorf("missing Deployment: got %+v, %v; want nil and an error", ref, err)
	}
}
