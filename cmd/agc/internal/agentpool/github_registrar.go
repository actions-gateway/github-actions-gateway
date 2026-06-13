// Package agentpool manages pre-registered GitHub Actions runner agents.
//
// # Runner Registration via JIT Config (generate-jitconfig)
//
// Registration flow:
//  1. Register a JIT runner (org-scoped or repo-scoped):
//     POST https://api.github.com/orgs/{org}/actions/runners/generate-jitconfig
//     POST https://api.github.com/repos/{owner}/{repo}/actions/runners/generate-jitconfig
//     Authorization: Bearer {installationAccessToken}
//     Content-Type: application/json
//     {"name": "{name}", "runner_group_id": {groupID},
//     "labels": [...], "work_folder": "_work"}
//     → {"runner": {"id": 12345, ...}, "encoded_jit_config": "{base64blob}"}
//
// The encoded_jit_config is a base64-encoded JSON blob containing three
// runner config file contents keyed by their file names:
//
//	".runner"                — JSON: agentId, serverUrl (broker URL), etc.
//	".credentials"           — JSON: Scheme, Data.ClientId, Data.AuthorizationUrl (PascalCase keys)
//	".credentials_rsaparams" — JSON: modulus, exponent, d, p, q, dp, dq, inverseQ (base64 values)
//
// Deregistration (org-scoped or repo-scoped):
//
//	DELETE https://api.github.com/orgs/{org}/actions/runners/{id}
//	DELETE https://api.github.com/repos/{owner}/{repo}/actions/runners/{id}
//	Authorization: Bearer {installationAccessToken}
package agentpool

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"

	"github.com/actions-gateway/github-actions-gateway/githubapp"
)

// GithubRegistrar implements Registrar using the GitHub Actions runner JIT config API.
// It requires the org or repo URL and the installation access token is passed
// per-call via Register/Deregister.
type GithubRegistrar struct {
	// OrgURL is the GitHub organization or repository URL.
	// Org-level:  "https://github.com/myorg"
	// Repo-level: "https://github.com/myorg/myrepo"
	// Used to derive the registration/deregistration endpoints and the runner URL.
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

// Register registers a new runner agent with GitHub using the JIT config API
// and returns its credentials including the server-generated RSA private key.
// token is the GitHub App installation access token.
func (r *GithubRegistrar) Register(ctx context.Context, token string, params RegisterParams) (*AgentCredentials, error) {
	generateURL := r.runnerAPIPrefix() + "/actions/runners/generate-jitconfig"

	body := map[string]any{
		"name":            params.Name,
		"runner_group_id": r.GroupID,
		"labels":          params.Labels,
		"work_folder":     "_work",
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal jitconfig request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, generateURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("generate jit config: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusConflict {
		// "Already exists" — a runner record with this name survives server-side
		// (e.g. an agent that never ran a job, whose ID was lost with its Secret).
		return nil, &NameConflictError{Name: params.Name}
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("generate jit config: unexpected status %d: %s", resp.StatusCode, githubapp.SanitizeBody(respBody, 512))
	}

	var result struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode jit config response: %w", err)
	}

	creds, err := parseJITCredentials(result.Runner.ID, result.EncodedJITConfig)
	if err != nil {
		return nil, err
	}
	creds.EncodedJITConfig = result.EncodedJITConfig
	return creds, nil
}

// Deregister removes a runner agent from GitHub.
// token is the GitHub App installation access token.
func (r *GithubRegistrar) Deregister(ctx context.Context, token string, agentID int64) error {
	deleteURL := fmt.Sprintf("%s/actions/runners/%d", r.runnerAPIPrefix(), agentID)
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("deregister runner: unexpected status %d: %s", resp.StatusCode, githubapp.SanitizeBody(body, 512))
	}
	return nil
}

// ResolveAgentID looks up a registered runner's ID by name using the
// list-runners endpoint's name filter:
//
//	GET {prefix}/actions/runners?name={name}
//	→ {"total_count": N, "runners": [{"id": 123, "name": "{name}", ...}]}
//
// Returns 0 with a nil error when no runner has that name. Used to resolve a
// 409 name conflict from Register when the surviving record's ID is unknown.
func (r *GithubRegistrar) ResolveAgentID(ctx context.Context, token, name string) (int64, error) {
	listURL := r.runnerAPIPrefix() + "/actions/runners?name=" + url.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return 0, fmt.Errorf("build list runners request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return 0, fmt.Errorf("list runners: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("list runners: unexpected status %d: %s", resp.StatusCode, githubapp.SanitizeBody(respBody, 512))
	}

	var result struct {
		Runners []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"runners"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return 0, fmt.Errorf("decode list runners response: %w", err)
	}
	for _, runner := range result.Runners {
		// The name param is a filter, not an exact match guarantee; compare.
		if runner.Name == name {
			return runner.ID, nil
		}
	}
	return 0, nil
}

// parseJITCredentials decodes the base64 JIT config blob returned by
// generate-jitconfig and extracts the AgentCredentials including the
// RSA private key from the .credentials_rsaparams XML.
func parseJITCredentials(agentID int64, encodedBlob string) (*AgentCredentials, error) {
	decoded, err := base64.StdEncoding.DecodeString(encodedBlob)
	if err != nil {
		return nil, fmt.Errorf("decode jit config blob: %w", err)
	}

	// The outer blob is a JSON object; each value is the base64-encoded
	// content of the corresponding runner config file.
	var files map[string]string
	if err := json.Unmarshal(decoded, &files); err != nil {
		return nil, fmt.Errorf("unmarshal jit config blob: %w", err)
	}

	decodeFile := func(key string) ([]byte, error) {
		b, err := base64.StdEncoding.DecodeString(files[key])
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		return b, nil
	}

	runnerFileBytes, err := decodeFile(".runner")
	if err != nil {
		return nil, err
	}
	var runnerCfg struct {
		ServerURL   string `json:"serverUrl"`
		ServerURLV2 string `json:"serverUrlV2"`
	}
	if err := json.Unmarshal(runnerFileBytes, &runnerCfg); err != nil {
		return nil, fmt.Errorf("parse .runner config: %w", err)
	}
	// The v2 broker API (used by default) lives at serverUrlV2.
	// Fall back to serverUrl for GHES deployments that use the v1 VSTS pool API.
	brokerURL := runnerCfg.ServerURLV2
	if brokerURL == "" {
		brokerURL = runnerCfg.ServerURL
	}

	credFileBytes, err := decodeFile(".credentials")
	if err != nil {
		return nil, err
	}
	var credCfg struct {
		Data struct {
			ClientID         string `json:"clientId"`
			AuthorizationURL string `json:"authorizationUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(credFileBytes, &credCfg); err != nil {
		return nil, fmt.Errorf("parse .credentials config: %w", err)
	}

	rsaParamsBytes, err := decodeFile(".credentials_rsaparams")
	if err != nil {
		return nil, err
	}
	privKey, err := parseRSAParamsJSON(rsaParamsBytes)
	if err != nil {
		return nil, fmt.Errorf("parse RSA params: %w", err)
	}

	privKeyPEM, err := marshalPrivateKey(privKey)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}

	return &AgentCredentials{
		AgentID:          agentID,
		ClientID:         credCfg.Data.ClientID,
		AuthorizationURL: credCfg.Data.AuthorizationURL,
		BrokerURL:        brokerURL,
		PrivateKeyPEM:    privKeyPEM,
	}, nil
}

// rsaParamsJSON matches the JSON format used by the GitHub Actions runner
// JIT config for .credentials_rsaparams. Values are standard base64-encoded
// big-endian byte arrays.
type rsaParamsJSON struct {
	Modulus  string `json:"modulus"`
	Exponent string `json:"exponent"`
	D        string `json:"d"`
	P        string `json:"p"`
	Q        string `json:"q"`
	DP       string `json:"dp"`
	DQ       string `json:"dq"`
	InverseQ string `json:"inverseQ"`
}

// parseRSAParamsJSON reconstructs an RSA private key from the JSON format
// stored in .credentials_rsaparams of the JIT config blob.
func parseRSAParamsJSON(data []byte) (*rsa.PrivateKey, error) {
	var p rsaParamsJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("unmarshal RSA JSON: %w", err)
	}

	decodeParam := func(name, b64 string) (*big.Int, error) {
		// Try standard base64 first, then URL-safe (no padding) as fallback.
		b, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			b, err = base64.RawURLEncoding.DecodeString(b64)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		}
		return new(big.Int).SetBytes(b), nil
	}

	n, err := decodeParam("modulus", p.Modulus)
	if err != nil {
		return nil, err
	}
	eInt, err := decodeParam("exponent", p.Exponent)
	if err != nil {
		return nil, err
	}
	d, err := decodeParam("d", p.D)
	if err != nil {
		return nil, err
	}
	pp, err := decodeParam("p", p.P)
	if err != nil {
		return nil, err
	}
	q, err := decodeParam("q", p.Q)
	if err != nil {
		return nil, err
	}

	key := &rsa.PrivateKey{
		PublicKey: rsa.PublicKey{
			N: n,
			E: int(eInt.Int64()),
		},
		D:      d,
		Primes: []*big.Int{pp, q},
	}
	key.Precompute()
	if err := key.Validate(); err != nil {
		return nil, fmt.Errorf("invalid RSA key: %w", err)
	}
	return key, nil
}

// runnerAPIPrefix returns the base REST API path for runner management calls,
// switching between org-scoped and repo-scoped endpoints based on OrgURL.
//
//	Org-level:  https://api.github.com/orgs/{org}
//	Repo-level: https://api.github.com/repos/{owner}/{repo}
func (r *GithubRegistrar) runnerAPIPrefix() string {
	base := r.apiBase()
	if isRepoURL(r.OrgURL) {
		return base + "/repos/" + extractRepoPath(r.OrgURL)
	}
	return base + "/orgs/" + extractOrgPath(r.OrgURL)
}

// apiBase returns the REST API root for the server hosting OrgURL.
func (r *GithubRegistrar) apiBase() string {
	if isHostedServer(r.OrgURL) {
		return "https://api.github.com"
	}
	return extractHost(r.OrgURL) + "/api/v3"
}

func isHostedServer(githubURL string) bool {
	return strings.Contains(githubURL, "github.com")
}

// isRepoURL reports whether githubURL refers to a repository (owner + repo)
// rather than just an organization (owner only).
func isRepoURL(githubURL string) bool {
	// parts: ["https:", "", "host", "owner", "repo", ...]
	trimmed := strings.TrimRight(githubURL, "/")
	parts := strings.Split(trimmed, "/")
	return len(parts) >= 5 && parts[4] != ""
}

func extractOrgPath(githubURL string) string {
	// "https://github.com/myorg" → "myorg"
	// parts: ["https:", "", "host", "org"]
	trimmed := strings.TrimRight(githubURL, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) >= 4 {
		return parts[3]
	}
	return ""
}

func extractRepoPath(githubURL string) string {
	// "https://github.com/myorg/myrepo" → "myorg/myrepo"
	// parts: ["https:", "", "host", "owner", "repo"]
	trimmed := strings.TrimRight(githubURL, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) >= 5 {
		return parts[3] + "/" + parts[4]
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
