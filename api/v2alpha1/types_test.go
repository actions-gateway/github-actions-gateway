package v2alpha1_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

func TestActionsGatewaySpec_GitHubAppSecretName(t *testing.T) {
	tests := []struct {
		name string
		spec v2alpha1.ActionsGatewaySpec
		want string
	}{
		{
			name: "nil GitHubApp returns empty string",
			spec: v2alpha1.ActionsGatewaySpec{
				Credentials: v2alpha1.GitHubCredentials{
					Type: v2alpha1.CredentialTypeGitHubApp,
				},
			},
			want: "",
		},
		{
			name: "set GitHubApp returns the referenced Secret name",
			spec: v2alpha1.ActionsGatewaySpec{
				Credentials: v2alpha1.GitHubCredentials{
					Type: v2alpha1.CredentialTypeGitHubApp,
					GitHubApp: &v2alpha1.LocalSecretReference{
						Name: "my-github-app-secret",
					},
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
		{
			name:    "empty profile defaults to baseline",
			profile: "",
			want:    v2alpha1.SecurityProfileBaseline,
		},
		{
			name:    "non-empty profile is returned unchanged",
			profile: v2alpha1.SecurityProfileRestricted,
			want:    v2alpha1.SecurityProfileRestricted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := v2alpha1.EffectiveSecurityProfile(tt.profile); got != tt.want {
				t.Errorf("EffectiveSecurityProfile(%q) = %q, want %q", tt.profile, got, tt.want)
			}
		})
	}
}

func TestAddToScheme_RegistersTypesWithoutError(t *testing.T) {
	scheme := runtime.NewScheme()

	if err := v2alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() returned error: %v", err)
	}
}
