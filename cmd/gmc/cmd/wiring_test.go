package main

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
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

// TestSeedIPRanges pins the startup order the e2e chart-reinstall check relies on
// (Q1386): a GMC that never completes a fetch still owns the ConfigMap it seeded
// from, and one it refused for age is adopted too.
func TestSeedIPRanges(t *testing.T) {
	owner := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "gmc", UID: "d-uid"}
	run := func(t *testing.T, fetchedAt time.Time) (*controller.IPRangeCache, corev1.ConfigMap) {
		t.Helper()
		s := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(s); err != nil {
			t.Fatal(err)
		}
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "gmc-system", Name: controller.IPRangeStoreName},
			Data:       map[string]string{"cidrs": "140.82.112.0/20", "fetchedAt": fetchedAt.UTC().Format(time.RFC3339Nano)},
		}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cm).Build()
		store := &controller.IPRangeStore{
			Reader: c, Writer: c, Namespace: "gmc-system", Name: controller.IPRangeStoreName,
			Owner: &owner, MaxAge: 49 * time.Hour,
		}
		cache := &controller.IPRangeCache{}
		seedIPRanges(context.Background(), store, cache)
		var got corev1.ConfigMap
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(cm), &got); err != nil {
			t.Fatal(err)
		}
		return cache, got
	}

	t.Run("a usable copy seeds the cache and gains the owner", func(t *testing.T) {
		cache, cm := run(t, time.Now().Add(-time.Hour))
		if len(cache.Snapshot()) != 1 {
			t.Errorf("seeded %d CIDRs, want 1", len(cache.Snapshot()))
		}
		if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].UID != owner.UID {
			t.Errorf("owners = %+v, want the Deployment", cm.OwnerReferences)
		}
	})
	t.Run("a copy refused for age is not served but is still adopted", func(t *testing.T) {
		cache, cm := run(t, time.Now().Add(-72*time.Hour))
		if n := len(cache.Snapshot()); n != 0 {
			t.Errorf("seeded %d CIDRs from a refused copy, want 0", n)
		}
		if len(cm.OwnerReferences) != 1 {
			t.Errorf("owners = %+v, want the Deployment", cm.OwnerReferences)
		}
	})
}

// TestRunnerTemplateValidator covers the hand-off from the resolved config to the
// namespaced RunnerTemplate webhook: the pod-shape opt-out reaches the validator
// only when its flag set it, and the PriorityClass allowlist is the shared one.
func TestRunnerTemplateValidator(t *testing.T) {
	for _, optOut := range []bool{false, true} {
		rc := &resolvedConfig{
			priorityClassAllowlist:               allowlist.New([]string{"worker-high"}),
			allowUncheckedRunnerTemplatePodShape: optOut,
		}
		v := runnerTemplateValidator(rc)
		if v.AllowUncheckedPodShape != optOut {
			t.Errorf("opt-out flag %v: validator AllowUncheckedPodShape = %v", optOut, v.AllowUncheckedPodShape)
		}
		if v.PriorityClasses != rc.priorityClassAllowlist {
			t.Errorf("opt-out flag %v: validator must share the resolved PriorityClass allowlist", optOut)
		}
	}
}
