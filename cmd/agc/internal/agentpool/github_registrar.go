// Package agentpool manages pre-registered GitHub Actions runner agents.
//
// # Runner Registration API (from github.com/actions/runner source, RunnerDotcomServer.cs)
//
// Registration flow:
//  1. Obtain a short-lived registration token:
//     POST https://api.github.com/orgs/{org}/actions/runners/registration-token
//     Authorization: Bearer {installationAccessToken}
//     → {"token": "...", "expires_at": "..."}
//
//  2. Register the runner agent:
//     POST https://api.github.com/actions/runners/register
//     Authorization: RemoteAuth {registrationToken}
//     Content-Type: application/json
//     {"url": "{runnerGroupURL}", "group_id": {groupID}, "name": "{name}",
//      "version": "{version}", "updates_disabled": false, "ephemeral": false,
//      "labels": [], "public_key": "{base64(DER(SubjectPublicKeyInfo))}"}
//     → {"id": 12345, "authorization": {"authorization_url": "...",
//        "server_url": "...", "client_id": "..."}}
//
// Deregistration:
//
//	DELETE https://api.github.com/orgs/{org}/actions/runners/{id}
//	Authorization: Bearer {installationAccessToken}
//
// The public_key field is base64-standard-encoded DER of the RSA public key in
// SubjectPublicKeyInfo format (equivalent to Go's x509.MarshalPKIXPublicKey).
//
// TODO(investigation-a): Confirm exact request/response schema against a live
// config.sh --debug capture before replacing StubRegistrar in production main.go.
// The schema above is sourced from the open-source runner code and may differ
// for enterprise GitHub instances or future runner versions.
package agentpool

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// GithubRegistrar implements Registrar using the GitHub Actions runner registration API.
// It requires the org/repo URL (e.g. "https://github.com/myorg") and the
// installation access token is passed per-call via Register/Deregister.
type GithubRegistrar struct {
	// OrgURL is the GitHub organization or repository URL, e.g. "https://github.com/myorg".
	// Used to derive the registration token endpoint and the runner group URL.
	OrgURL string
	// GroupID is the GitHub-side runner group ID to register agents into.
	// Use 1 for the default runner group.
	GroupID int
	// HTTPClient is used for all outbound calls. nil uses http.DefaultClient.
	HTTPClient *http.Client
}

func (r *GithubRegistrar) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return http.DefaultClient
}

// Register registers a new runner agent with GitHub and returns its credentials.
// token is the GitHub App installation access token.
func (r *GithubRegistrar) Register(ctx context.Context, token string, params RegisterParams) (*AgentCredentials, error) {
	// Step 1: Get a short-lived registration token.
	regToken, err := r.getRegistrationToken(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("get registration token: %w", err)
	}

	// Step 2: Marshal public key to base64-DER.
	pubKeyB64, err := marshalPublicKeyBase64(params.PublicKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}

	// Step 3: Register the runner.
	return r.registerRunner(ctx, regToken, params, pubKeyB64)
}

// Deregister removes a runner agent from GitHub.
// token is the GitHub App installation access token.
func (r *GithubRegistrar) Deregister(ctx context.Context, token string, agentID int64) error {
	orgPath := extractOrgPath(r.OrgURL)
	var deleteURL string
	if isHostedServer(r.OrgURL) {
		deleteURL = fmt.Sprintf("https://api.github.com/orgs/%s/actions/runners/%d", orgPath, agentID)
	} else {
		host := extractHost(r.OrgURL)
		deleteURL = fmt.Sprintf("%s/api/v3/orgs/%s/actions/runners/%d", host, orgPath, agentID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, deleteURL, nil)
	if err != nil {
		return fmt.Errorf("build deregister request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("deregister runner: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("deregister runner: unexpected status %d: %s", resp.StatusCode, body)
	}
	return nil
}

func (r *GithubRegistrar) getRegistrationToken(ctx context.Context, installToken string) (string, error) {
	orgPath := extractOrgPath(r.OrgURL)
	var tokenURL string
	if isHostedServer(r.OrgURL) {
		tokenURL = fmt.Sprintf("https://api.github.com/orgs/%s/actions/runners/registration-token", orgPath)
	} else {
		host := extractHost(r.OrgURL)
		tokenURL = fmt.Sprintf("%s/api/v3/orgs/%s/actions/runners/registration-token", host, orgPath)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+installToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("get registration token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get registration token: unexpected status %d: %s", resp.StatusCode, body)
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode registration token response: %w", err)
	}
	return result.Token, nil
}

func (r *GithubRegistrar) registerRunner(ctx context.Context, regToken string, params RegisterParams, pubKeyB64 string) (*AgentCredentials, error) {
	var registerURL string
	if isHostedServer(r.OrgURL) {
		registerURL = "https://api.github.com/actions/runners/register"
	} else {
		registerURL = extractHost(r.OrgURL) + "/api/v3/actions/runners/register"
	}

	body := map[string]any{
		"url":              r.OrgURL,
		"group_id":         r.GroupID,
		"name":             params.Name,
		"version":          params.Version,
		"updates_disabled": false,
		"ephemeral":        false,
		"labels":           params.Labels,
		"public_key":       pubKeyB64,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal register request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registerURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "RemoteAuth "+regToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("register runner: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("register runner: unexpected status %d: %s", resp.StatusCode, respBody)
	}

	var result struct {
		ID            int64 `json:"id"`
		Authorization struct {
			AuthorizationURL string `json:"authorization_url"`
			ServerURL        string `json:"server_url"`
			ClientID         string `json:"client_id"`
		} `json:"authorization"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode register response: %w", err)
	}
	return &AgentCredentials{
		AgentID:          result.ID,
		ClientID:         result.Authorization.ClientID,
		AuthorizationURL: result.Authorization.AuthorizationURL,
		BrokerURL:        result.Authorization.ServerURL,
	}, nil
}

// marshalPublicKeyBase64 extracts the DER-encoded SubjectPublicKeyInfo from a PEM-encoded
// public key and returns the base64 standard encoding. This matches .NET's
// rsa.ExportSubjectPublicKeyInfo() → Convert.ToBase64String() used by the runner.
func marshalPublicKeyBase64(pubKeyPEM []byte) (string, error) {
	block, _ := pem.Decode(pubKeyPEM)
	if block == nil {
		return "", fmt.Errorf("no PEM block found in public key")
	}
	// block.Bytes is already DER-encoded SubjectPublicKeyInfo for "PUBLIC KEY" blocks.
	return base64.StdEncoding.EncodeToString(block.Bytes), nil
}

func isHostedServer(githubURL string) bool {
	return strings.Contains(githubURL, "github.com")
}

func extractOrgPath(githubURL string) string {
	// e.g. "https://github.com/myorg" → "myorg"
	// e.g. "https://github.com/myorg/myrepo" → "myorg" (we use only the org)
	trimmed := strings.TrimRight(githubURL, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) >= 1 {
		return parts[len(parts)-1]
	}
	return ""
}

func extractHost(githubURL string) string {
	// e.g. "https://github.example.com/myorg" → "https://github.example.com"
	u := strings.TrimRight(githubURL, "/")
	idx := strings.Index(u, "://")
	if idx < 0 {
		return u
	}
	rest := u[idx+3:]
	slashIdx := strings.Index(rest, "/")
	if slashIdx < 0 {
		return u
	}
	return u[:idx+3+slashIdx]
}
