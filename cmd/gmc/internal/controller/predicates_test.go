package controller

import (
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	v2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// TestEgressDestinationConfigMapPredicate_MatchesOnlyDesignatedConfigMap verifies
// the predicate accepts only the ConfigMap named/namespaced in the reconciler's
// config, rejecting a same-name ConfigMap in another namespace and a
// different-name ConfigMap in the same namespace.
func TestEgressDestinationConfigMapPredicate_MatchesOnlyDesignatedConfigMap(t *testing.T) {
	r := &EgressDestinationAllowlistReconciler{ConfigMapName: "gag-egress-allowlist", Namespace: "gag-system"}
	p := r.configMapPredicate()

	designated := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gag-egress-allowlist", Namespace: "gag-system"}}
	assert.True(t, p.Create(event.CreateEvent{Object: designated}), "the designated ConfigMap must match")

	wrongNS := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gag-egress-allowlist", Namespace: "tenant-ns"}}
	assert.False(t, p.Create(event.CreateEvent{Object: wrongNS}), "a same-name ConfigMap in another namespace must not match")

	wrongName := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated-cm", Namespace: "gag-system"}}
	assert.False(t, p.Create(event.CreateEvent{Object: wrongName}), "a different-name ConfigMap in the same namespace must not match")
}

// TestPriorityClassAllowlistPredicate_MatchesOnlyDesignatedObject mirrors the
// EgressDestination predicate test for the PriorityClass allowlist reconciler,
// whose namePredicate is a distinct method with the same contract. The kind is
// cluster-scoped since Q492, so the name alone identifies the object and there is
// no namespace to disambiguate.
func TestPriorityClassAllowlistPredicate_MatchesOnlyDesignatedObject(t *testing.T) {
	r := &PriorityClassAllowlistReconciler{Name: "gag-priorityclass-allowlist"}
	p := r.namePredicate()

	designated := &v2beta1.PriorityClassAllowlist{ObjectMeta: metav1.ObjectMeta{Name: "gag-priorityclass-allowlist"}}
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: designated, ObjectNew: designated}), "the designated object must match")

	wrongName := &v2beta1.PriorityClassAllowlist{ObjectMeta: metav1.ObjectMeta{Name: "unrelated"}}
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: wrongName, ObjectNew: wrongName}), "a different-name object must not match")
}

// TestV2TenantNamespacePredicate_MatchesOnlyMarkedNamespaces verifies the
// predicate accepts a namespace carrying the exact tenant marker label/value and
// rejects both an unmarked namespace and one with a wrong-value marker label.
func TestV2TenantNamespacePredicate_MatchesOnlyMarkedNamespaces(t *testing.T) {
	p := v2TenantNamespacePredicate()

	marked := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "tenant-a",
		Labels: map[string]string{gmcv2alpha1.TenantNamespaceMarkerLabel: gmcv2alpha1.TenantNamespaceMarkerValue},
	}}
	assert.True(t, p.Create(event.CreateEvent{Object: marked}), "a namespace with the exact marker label/value must match")

	unmarked := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}}
	assert.False(t, p.Create(event.CreateEvent{Object: unmarked}), "a namespace with no marker label must not match")

	wrongValue := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "tenant-b",
		Labels: map[string]string{gmcv2alpha1.TenantNamespaceMarkerLabel: "false"},
	}}
	assert.False(t, p.Create(event.CreateEvent{Object: wrongValue}), "a namespace with a wrong marker value must not match")
}
