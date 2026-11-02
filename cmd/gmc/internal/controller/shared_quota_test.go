package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ptrInt32(v int32) *int32 { return &v }

func TestResourceListEqual(t *testing.T) {
	a := corev1.ResourceList{corev1.ResourcePods: resource.MustParse("5")}
	b := corev1.ResourceList{corev1.ResourcePods: resource.MustParse("5000m")}
	assert.True(t, resourceListEqual(a, b), "equal value, different canonical form")

	c := corev1.ResourceList{corev1.ResourcePods: resource.MustParse("6")}
	assert.False(t, resourceListEqual(a, c))

	d := corev1.ResourceList{
		corev1.ResourcePods:        resource.MustParse("5"),
		corev1.ResourceRequestsCPU: resource.MustParse("1"),
	}
	assert.False(t, resourceListEqual(a, d), "differing key sets")
}

func TestConditionGaugeValue(t *testing.T) {
	conds := []metav1.Condition{
		{Type: "ProxyQuotaPressure", Status: metav1.ConditionTrue},
		{Type: "ProxyQuotaExceeded", Status: metav1.ConditionFalse},
	}
	assert.Equal(t, float64(1), conditionGaugeValue(conds, "ProxyQuotaPressure"))
	assert.Equal(t, float64(0), conditionGaugeValue(conds, "ProxyQuotaExceeded"))
	assert.Equal(t, float64(0), conditionGaugeValue(conds, "Absent"))
}
