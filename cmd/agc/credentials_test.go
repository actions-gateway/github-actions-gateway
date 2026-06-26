package main

import (
	"strings"
	"testing"

	"github.com/go-logr/logr"
)

// setWorkloadIdentityEnv sets a well-formed workload-identity credential env on the
// test, with the given Vault address. t.Setenv restores the prior environment after
// the test, so cases do not leak into one another.
func setWorkloadIdentityEnv(t *testing.T, vaultAddr string) {
	t.Helper()
	t.Setenv("CREDENTIAL_TYPE", "WorkloadIdentity")
	t.Setenv("GITHUB_APP_ID", "424242")
	t.Setenv("GITHUB_INSTALLATION_ID", "99")
	t.Setenv("VAULT_ADDR", vaultAddr)
	t.Setenv("VAULT_TRANSIT_KEY", "agc")
	t.Setenv("VAULT_AUTH_ROLE", "agc")
	t.Setenv("VAULT_SA_TOKEN_PATH", "/var/run/secrets/actions-gateway/vault-token/token")
}

func TestBuildTokenProvider_WorkloadIdentity_HTTPSBuildsProvider(t *testing.T) {
	setWorkloadIdentityEnv(t, "https://vault.vault.svc:8200")
	// No STUB_AUTH_URL: production posture. An HTTPS Vault address must be accepted
	// and a provider built without contacting Vault (the signer logs in lazily).
	t.Setenv("STUB_AUTH_URL", "")

	prov, err := buildTokenProvider(logr.Discard())
	if err != nil {
		t.Fatalf("buildTokenProvider: unexpected error: %v", err)
	}
	if prov == nil {
		t.Fatal("buildTokenProvider returned a nil provider")
	}
}

func TestBuildTokenProvider_WorkloadIdentity_PlaintextVaultRejectedWithoutOptIn(t *testing.T) {
	setWorkloadIdentityEnv(t, "http://vault.vault.svc:8200")
	t.Setenv("STUB_AUTH_URL", "") // no dev/test opt-in

	_, err := buildTokenProvider(logr.Discard())
	if err == nil {
		t.Fatal("expected a plaintext Vault address to be rejected without the dev/test opt-in")
	}
	if !strings.Contains(err.Error(), "Vault signer") {
		t.Fatalf("error should name the Vault signer configuration, got: %v", err)
	}
}

func TestBuildTokenProvider_WorkloadIdentity_PlaintextVaultAllowedWithOptIn(t *testing.T) {
	setWorkloadIdentityEnv(t, "http://vault.vault.svc:8200")
	t.Setenv("STUB_AUTH_URL", "http://fakegithub.e2e-infra.svc:8080/token") // dev/test opt-in

	prov, err := buildTokenProvider(logr.Discard())
	if err != nil {
		t.Fatalf("buildTokenProvider with dev/test opt-in: unexpected error: %v", err)
	}
	if prov == nil {
		t.Fatal("buildTokenProvider returned a nil provider")
	}
}

func TestBuildTokenProvider_WorkloadIdentity_InvalidAppIDErrors(t *testing.T) {
	setWorkloadIdentityEnv(t, "https://vault.vault.svc:8200")
	t.Setenv("GITHUB_APP_ID", "not-a-number")

	_, err := buildTokenProvider(logr.Discard())
	if err == nil {
		t.Fatal("expected an unparseable GITHUB_APP_ID to error")
	}
	if !strings.Contains(err.Error(), "GITHUB_APP_ID") {
		t.Fatalf("error should name GITHUB_APP_ID, got: %v", err)
	}
}

// TestBuildTokenProvider_DefaultsToGitHubApp confirms the dispatch: with no
// CREDENTIAL_TYPE the possession path runs, which reads the mounted Secret files.
// Those files do not exist in the test environment, so it fails reading appId —
// proving the default branch is the GitHub App path (not workload identity).
func TestBuildTokenProvider_DefaultsToGitHubApp(t *testing.T) {
	t.Setenv("CREDENTIAL_TYPE", "")
	t.Setenv("STUB_AUTH_URL", "")

	_, err := buildTokenProvider(logr.Discard())
	if err == nil {
		t.Fatal("expected the possession path to fail reading the absent appId Secret file")
	}
	if !strings.Contains(err.Error(), "appId") {
		t.Fatalf("error should come from reading the GitHub App appId file, got: %v", err)
	}
}
