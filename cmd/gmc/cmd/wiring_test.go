package main

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

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
