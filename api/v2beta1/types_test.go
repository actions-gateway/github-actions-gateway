package v2beta1_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"github.com/actions-gateway/github-actions-gateway/api/v2beta1"
)

// v2beta1 is a copy of the v2alpha1 kinds (the Q74 graduation), so it carries the
// same helper logic. These tests verify the copy behaves identically and keep the
// api module's coverage floor intact.

func TestGitHubAppSecretName(t *testing.T) {
	tests := []struct {
		name string
		spec v2beta1.ActionsGatewaySpec
		want string
	}{
		{
			name: "nil GitHubApp returns empty string",
			spec: v2beta1.ActionsGatewaySpec{
				Credentials: v2beta1.GitHubCredentials{Type: v2beta1.CredentialTypeGitHubApp},
			},
			want: "",
		},
		{
			name: "set GitHubApp returns the referenced Secret name",
			spec: v2beta1.ActionsGatewaySpec{
				Credentials: v2beta1.GitHubCredentials{
					Type:      v2beta1.CredentialTypeGitHubApp,
					GitHubApp: &v2beta1.LocalSecretReference{Name: "my-github-app-secret"},
				},
			},
			want: "my-github-app-secret",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spec.GitHubAppSecretName(); got != tt.want {
				t.Errorf("GitHubAppSecretName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEffectiveSecurityProfile(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		want    string
	}{
		{"empty profile defaults to baseline", "", v2beta1.SecurityProfileBaseline},
		{"non-empty profile is returned unchanged", v2beta1.SecurityProfileRestricted, v2beta1.SecurityProfileRestricted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := v2beta1.EffectiveSecurityProfile(tt.profile); got != tt.want {
				t.Errorf("EffectiveSecurityProfile(%q) = %q, want %q", tt.profile, got, tt.want)
			}
		})
	}
}

func tmpl(containers, initContainers []corev1.Container) *v2beta1.RunnerTemplateSpec {
	return &v2beta1.RunnerTemplateSpec{
		PodTemplate: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: containers, InitContainers: initContainers},
		},
	}
}

func ctr(name string) corev1.Container { return corev1.Container{Name: name} }

func nativeSidecar(name string) corev1.Container {
	always := corev1.ContainerRestartPolicyAlways
	return corev1.Container{Name: name, RestartPolicy: &always}
}

func TestReapBlockingSidecars(t *testing.T) {
	tests := []struct {
		name        string
		spec        *v2beta1.RunnerTemplateSpec
		annotations map[string]string
		want        []string
	}{
		{name: "runner only is not flagged", spec: tmpl([]corev1.Container{ctr("runner")}, nil), want: nil},
		{name: "regular sidecar is flagged", spec: tmpl([]corev1.Container{ctr("runner"), ctr("dind")}, nil), want: []string{"dind"}},
		{name: "native sidecar is not flagged", spec: tmpl([]corev1.Container{ctr("runner")}, []corev1.Container{nativeSidecar("dind")}), want: nil},
		{name: "regular init container is not flagged", spec: tmpl([]corev1.Container{ctr("runner")}, []corev1.Container{ctr("setup")}), want: nil},
		{
			name:        "acknowledged sidecar is suppressed",
			spec:        tmpl([]corev1.Container{ctr("runner"), ctr("dind")}, nil),
			annotations: map[string]string{v2beta1.SelfExitingSidecarsAnnotation: "dind"},
			want:        nil,
		},
		{
			name:        "only unacknowledged sidecars are flagged",
			spec:        tmpl([]corev1.Container{ctr("runner"), ctr("dind"), ctr("log-shipper")}, nil),
			annotations: map[string]string{v2beta1.SelfExitingSidecarsAnnotation: "log-shipper"},
			want:        []string{"dind"},
		},
		{
			name:        "acknowledgment list tolerates spaces and trailing commas",
			spec:        tmpl([]corev1.Container{ctr("runner"), ctr("a"), ctr("b")}, nil),
			annotations: map[string]string{v2beta1.SelfExitingSidecarsAnnotation: " a , b , "},
			want:        nil,
		},
		{name: "results are sorted", spec: tmpl([]corev1.Container{ctr("runner"), ctr("zeta"), ctr("alpha")}, nil), want: []string{"alpha", "zeta"}},
		{name: "template omitting a runner flags every regular container", spec: tmpl([]corev1.Container{ctr("custom")}, nil), want: []string{"custom"}},
		{name: "nil template is not flagged", spec: nil, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := v2beta1.ReapBlockingSidecars(tc.spec, tc.annotations)
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

func TestAddToScheme_RegistersTypesWithoutError(t *testing.T) {
	if err := v2beta1.AddToScheme(runtime.NewScheme()); err != nil {
		t.Fatalf("AddToScheme() returned error: %v", err)
	}
}

// TestHubMarkers asserts every root kind satisfies conversion.Hub (the no-op Hub()
// marker that designates v2beta1 as the conversion hub).
func TestHubMarkers(t *testing.T) {
	hubs := []conversion.Hub{
		&v2beta1.ActionsGateway{},
		&v2beta1.EgressProxy{},
		&v2beta1.RunnerSet{},
		&v2beta1.RunnerTemplate{},
		&v2beta1.ClusterRunnerTemplate{},
	}
	for _, h := range hubs {
		h.Hub() // no-op; compile-time proof is the slice literal above
	}
}
