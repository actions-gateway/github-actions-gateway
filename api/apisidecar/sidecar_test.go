package apisidecar_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/actions-gateway/github-actions-gateway/api/apisidecar"
)

// podSpec builds a PodSpec with the given regular and init containers, so each case
// declares only the shape under test.
func podSpec(containers, initContainers []corev1.Container) *corev1.PodSpec {
	return &corev1.PodSpec{Containers: containers, InitContainers: initContainers}
}

func ctr(name string) corev1.Container { return corev1.Container{Name: name} }

// nativeSidecar is a restartPolicy: Always init container (KEP-753) — the shape that
// terminates with the pod and therefore never blocks reaping.
func nativeSidecar(name string) corev1.Container {
	always := corev1.ContainerRestartPolicyAlways
	return corev1.Container{Name: name, RestartPolicy: &always}
}

// TestReapBlocking exercises the heuristic at its version-neutral entry point — the
// one the v2alpha1/v2beta1 RunnerTemplateSpec wrappers delegate to (Q374). The
// per-version tests cover the same cases through those wrappers; this pins the
// contract the shared package owns, including the nil-podSpec guard the wrappers
// cannot reach (they screen a nil template first).
func TestReapBlocking(t *testing.T) {
	tests := []struct {
		name        string
		spec        *corev1.PodSpec
		annotations map[string]string
		want        []string
	}{
		{
			name: "nil pod spec is not flagged",
			spec: nil,
			want: nil,
		},
		{
			name: "runner only is not flagged",
			spec: podSpec([]corev1.Container{ctr(apisidecar.RunnerContainerName)}, nil),
			want: nil,
		},
		{
			name: "regular sidecar is flagged",
			spec: podSpec([]corev1.Container{ctr(apisidecar.RunnerContainerName), ctr("dind")}, nil),
			want: []string{"dind"},
		},
		{
			name: "native sidecar (restartPolicy Always init container) is not flagged",
			spec: podSpec([]corev1.Container{ctr(apisidecar.RunnerContainerName)}, []corev1.Container{nativeSidecar("dind")}),
			want: nil,
		},
		{
			name:        "acknowledged sidecars are suppressed, spaces and trailing commas tolerated",
			spec:        podSpec([]corev1.Container{ctr(apisidecar.RunnerContainerName), ctr("a"), ctr("b")}, nil),
			annotations: map[string]string{apisidecar.SelfExitingSidecarsAnnotation: " a , b , "},
			want:        nil,
		},
		{
			name: "results are sorted",
			spec: podSpec([]corev1.Container{ctr(apisidecar.RunnerContainerName), ctr("zeta"), ctr("alpha")}, nil),
			want: []string{"alpha", "zeta"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := apisidecar.ReapBlocking(tc.spec, tc.annotations)
			if len(got) != len(tc.want) {
				t.Fatalf("ReapBlocking() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ReapBlocking() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
