package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
