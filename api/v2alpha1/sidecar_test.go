package v2alpha1_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// tmpl builds a RunnerTemplateSpec with the given regular containers and init
// containers, so each case declares only the shape under test.
func tmpl(containers []corev1.Container, initContainers []corev1.Container) *v2alpha1.RunnerTemplateSpec {
	return &v2alpha1.RunnerTemplateSpec{
		PodTemplate: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{},
			Spec: corev1.PodSpec{
				Containers:     containers,
				InitContainers: initContainers,
			},
		},
	}
}

func ctr(name string) corev1.Container { return corev1.Container{Name: name} }

// nativeSidecar is a restartPolicy: Always init container (KEP-753) — the shape
// that terminates with the pod and therefore never blocks reaping.
func nativeSidecar(name string) corev1.Container {
	always := corev1.ContainerRestartPolicyAlways
	return corev1.Container{Name: name, RestartPolicy: &always}
}

func TestReapBlockingSidecars(t *testing.T) {
	tests := []struct {
		name        string
		spec        *v2alpha1.RunnerTemplateSpec
		annotations map[string]string
		want        []string
	}{
		{
			name: "runner only is not flagged",
			spec: tmpl([]corev1.Container{ctr("runner")}, nil),
			want: nil,
		},
		{
			name: "regular sidecar is flagged",
			spec: tmpl([]corev1.Container{ctr("runner"), ctr("dind")}, nil),
			want: []string{"dind"},
		},
		{
			name: "native sidecar (restartPolicy Always init container) is not flagged",
			spec: tmpl([]corev1.Container{ctr("runner")}, []corev1.Container{nativeSidecar("dind")}),
			want: nil,
		},
		{
			name: "regular init container is not flagged (runs to completion before the pod starts)",
			spec: tmpl([]corev1.Container{ctr("runner")}, []corev1.Container{ctr("setup")}),
			want: nil,
		},
		{
			name:        "acknowledged sidecar is suppressed",
			spec:        tmpl([]corev1.Container{ctr("runner"), ctr("dind")}, nil),
			annotations: map[string]string{v2alpha1.SelfExitingSidecarsAnnotation: "dind"},
			want:        nil,
		},
		{
			name:        "only unacknowledged sidecars are flagged",
			spec:        tmpl([]corev1.Container{ctr("runner"), ctr("dind"), ctr("log-shipper")}, nil),
			annotations: map[string]string{v2alpha1.SelfExitingSidecarsAnnotation: "log-shipper"},
			want:        []string{"dind"},
		},
		{
			name:        "acknowledgment list tolerates spaces and trailing commas",
			spec:        tmpl([]corev1.Container{ctr("runner"), ctr("a"), ctr("b")}, nil),
			annotations: map[string]string{v2alpha1.SelfExitingSidecarsAnnotation: " a , b , "},
			want:        nil,
		},
		{
			name: "results are sorted",
			spec: tmpl([]corev1.Container{ctr("runner"), ctr("zeta"), ctr("alpha")}, nil),
			want: []string{"alpha", "zeta"},
		},
		{
			name: "template that omits a runner container flags every regular container",
			spec: tmpl([]corev1.Container{ctr("custom")}, nil),
			want: []string{"custom"},
		},
		{
			name: "nil template is not flagged",
			spec: nil,
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := v2alpha1.ReapBlockingSidecars(tc.spec, tc.annotations)
			if len(got) != len(tc.want) {
				t.Fatalf("ReapBlockingSidecars() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ReapBlockingSidecars() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
