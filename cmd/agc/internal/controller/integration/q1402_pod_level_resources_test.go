//go:build integration

package integration_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestPodLevelResources_RefuseWhatTheRaiseIsHeldTo (Q1402) pins, against a real API
// server, the two rules podLevelMemoryRoom and capAtPodLimit encode: a container's
// limit is at or below the pod's, and the containers' requests together are at or
// below an explicit pod request. Where the pod sets only limits, its request is
// defaulted from the containers' and must still be at or below the pod's limit.
func TestPodLevelResources_RefuseWhatTheRaiseIsHeldTo(t *testing.T) {
	const ns = "q1402-pod-level"
	createNSForAGC(t, ns)
	mem := func(q string) corev1.ResourceList {
		return corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(q)}
	}
	always := corev1.ContainerRestartPolicyAlways
	pod := func(name string, podLevel corev1.ResourceRequirements, runner corev1.ResourceRequirements) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: corev1.PodSpec{
				Resources: &podLevel,
				InitContainers: []corev1.Container{{Name: "dind", Image: "busybox", RestartPolicy: &always,
					Resources: corev1.ResourceRequirements{Requests: mem("512Mi")}}},
				Containers: []corev1.Container{{Name: "runner", Image: "busybox", Resources: runner}},
			},
		}
	}
	cases := []struct {
		name     string
		pod      *corev1.Pod
		refusal  string // "" where the API server admits it
		podReqIs string // the pod request the API server stored, where admitted
	}{
		{"a runner limit above the pod's", pod("limit-above",
			corev1.ResourceRequirements{Limits: mem("2560Mi")},
			corev1.ResourceRequirements{Requests: mem("2Gi"), Limits: mem("3Gi")}),
			"must be less than or equal to pod limits of 2560Mi", ""},
		{"a runner limit held to the pod's", pod("limit-held",
			corev1.ResourceRequirements{Limits: mem("2560Mi")},
			corev1.ResourceRequirements{Requests: mem("2Gi"), Limits: mem("2560Mi")}),
			"", "2560Mi"},
		{"requests together above the pod's request", pod("request-above",
			corev1.ResourceRequirements{Requests: mem("2816Mi"), Limits: mem("8Gi")},
			corev1.ResourceRequirements{Requests: mem("2880Mi"), Limits: mem("2880Mi")}),
			"must be greater than or equal to aggregate container requests of 3392Mi", ""},
		{"requests together held to the pod's request", pod("request-held",
			corev1.ResourceRequirements{Requests: mem("2816Mi"), Limits: mem("8Gi")},
			corev1.ResourceRequirements{Requests: mem("2304Mi"), Limits: mem("2304Mi")}),
			"", "2816Mi"},
		// Q1424: the defaulted pod request against the pod's own limit.
		{"pod limits only, requests together above the pod's limit", pod("defaulted-above",
			corev1.ResourceRequirements{Limits: mem("2560Mi")},
			corev1.ResourceRequirements{Requests: mem("2304Mi"), Limits: mem("2560Mi")}),
			"must be less than or equal to memory limit of 2560Mi", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := k8sClient.Create(ctx, tc.pod)
			if tc.refusal != "" {
				require.Error(t, err)
				t.Logf("refused: %v", err)
				assert.True(t, apierrors.IsInvalid(err), "got %v", err)
				assert.Contains(t, err.Error(), tc.refusal)
				return
			}
			require.NoError(t, err)
			got := tc.pod.Spec.Resources.Requests[corev1.ResourceMemory]
			assert.Equal(t, tc.podReqIs, got.String(), fmt.Sprintf("stored pod request %s", got.String()))
		})
	}
}
