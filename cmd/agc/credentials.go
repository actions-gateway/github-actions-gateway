package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/githubapp"
	"github.com/actions-gateway/github-actions-gateway/githubapp/vaultsigner"
	"github.com/go-logr/logr"
)

// buildTokenProvider builds the GitHub App installation-token provider for the
// gateway's configured credential method (Q196/Q197/Q201). The GMC threads the
// method via CREDENTIAL_TYPE:
//
//   - "WorkloadIdentity" (the delegation model): NO App private key is in the
//     cluster. A vaultsigner.Signer signs the App JWT via Vault transit, proving the
//     AGC's pod identity to Vault with the kubelet-projected ServiceAccount token —
//     read from disk by the signer, never an env var.
//   - anything else, including empty (the possession model, the default): the App
//     key is read from the mounted Secret files under credsDir.
//
// A non-HTTPS GitHub API base URL or Vault address is rejected unless the dev/test
// signal (STUB_AUTH_URL set) is present — the same opt-in the possession path has
// always used, so production AGCs (which never set STUB_AUTH_URL) keep HTTPS
// mandatory on every credential channel.
func buildTokenProvider(log logr.Logger) (githubapp.ExpiringTokenProvider, error) {
	allowInsecure := os.Getenv("STUB_AUTH_URL") != ""
	if allowInsecure {
		log.Info("dev/test mode: allowing non-HTTPS GitHub API base URL and Vault address (STUB_AUTH_URL set)")
	}

	if os.Getenv("CREDENTIAL_TYPE") == string(agcv2alpha1.CredentialTypeWorkloadIdentity) {
		log.Info("credential method: workload identity (no in-cluster App key; signing delegated to Vault transit)")
		return buildWorkloadIdentityProvider(allowInsecure)
	}
	return buildGitHubAppProvider(allowInsecure)
}

// buildWorkloadIdentityProvider builds the no-PEM provider: a Vault transit signer
// (githubapp/vaultsigner) behind the githubapp.Signer interface. It reads only
// non-secret configuration from env — the App identity and the Vault address/mounts/
// role. The Vault login credential (the projected ServiceAccount token) is read from
// the file at VAULT_SA_TOKEN_PATH by the signer at each login, never from env.
func buildWorkloadIdentityProvider(allowInsecureAddr bool) (githubapp.ExpiringTokenProvider, error) {
	appID, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("GITHUB_APP_ID")), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse GITHUB_APP_ID: %w", err)
	}
	installID, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("GITHUB_INSTALLATION_ID")), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse GITHUB_INSTALLATION_ID: %w", err)
	}

	signer, err := vaultsigner.New(vaultsigner.Config{
		Address:              os.Getenv("VAULT_ADDR"),
		TransitMount:         os.Getenv("VAULT_TRANSIT_MOUNT"),
		KeyName:              os.Getenv("VAULT_TRANSIT_KEY"),
		AuthMount:            os.Getenv("VAULT_AUTH_MOUNT"),
		Role:                 os.Getenv("VAULT_AUTH_ROLE"),
		TokenPath:            os.Getenv("VAULT_SA_TOKEN_PATH"),
		AllowInsecureAddress: allowInsecureAddr,
	})
	if err != nil {
		return nil, fmt.Errorf("configure Vault signer: %w", err)
	}
	return githubapp.NewInstallationTokenProviderWithSigner(appID, installID, signer, nil, allowInsecureAddr)
}

// buildGitHubAppProvider builds the possession-model provider from the GitHub App
// Secret files the GMC mounts under credsDir (appId, installationId, privateKey).
// The private key is read as a file and handed straight to the signer; it never
// transits an env var.
func buildGitHubAppProvider(allowInsecureBaseURL bool) (githubapp.ExpiringTokenProvider, error) {
	appIDBytes, err := os.ReadFile(filepath.Join(credsDir, "appId"))
	if err != nil {
		return nil, fmt.Errorf("read appId: %w", err)
	}
	appID, err := strconv.ParseInt(strings.TrimSpace(string(appIDBytes)), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse appId: %w", err)
	}

	installIDBytes, err := os.ReadFile(filepath.Join(credsDir, "installationId"))
	if err != nil {
		return nil, fmt.Errorf("read installationId: %w", err)
	}
	installID, err := strconv.ParseInt(strings.TrimSpace(string(installIDBytes)), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse installationId: %w", err)
	}

	pemBytes, err := os.ReadFile(filepath.Join(credsDir, "privateKey"))
	if err != nil {
		return nil, fmt.Errorf("read privateKey: %w", err)
	}

	creds := githubapp.Credentials{
		AppID:          appID,
		PrivateKeyPEM:  pemBytes,
		InstallationID: installID,
	}
	rawProvider, err := githubapp.NewInstallationTokenProvider(creds, nil, allowInsecureBaseURL)
	if err != nil {
		return nil, fmt.Errorf("create token provider: %w", err)
	}
	expProvider, ok := rawProvider.(githubapp.ExpiringTokenProvider)
	if !ok {
		return nil, fmt.Errorf("token provider does not implement ExpiringTokenProvider")
	}
	return expProvider, nil
}
