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
//      "labels": [...], "work_folder": "_work"}
//     → {"runner": {"id": 12345, ...}, "encoded_jit_config": "{base64blob}"}
//
// The encoded_jit_config is a base64-encoded JSON blob containing three
// runner config file contents keyed by their file names:
//
//	".runner"                — JSON: agentId, serverUrl (broker URL), etc.
//	".credentials"           — JSON: scheme, data.clientId, data.authorizationUrl
//	".credentials_rsaparams" — XML:  RSAKeyValue with base64-encoded RSA parameters
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
	"encoding/xml"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
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
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("generate jit config: unexpected status %d: %s", resp.StatusCode, respBody)
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

	return parseJITCredentials(result.Runner.ID, result.EncodedJITConfig)
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("deregister runner: unexpected status %d: %s", resp.StatusCode, body)
	}
	return nil
}

// parseJITCredentials decodes the base64 JIT config blob returned by
// generate-jitconfig and extracts the AgentCredentials including the
// RSA private key from the .credentials_rsaparams XML.
func parseJITCredentials(agentID int64, encodedBlob string) (*AgentCredentials, error) {
	decoded, err := base64.StdEncoding.DecodeString(encodedBlob)
	if err != nil {
		return nil, fmt.Errorf("decode jit config blob: %w", err)
	}

	var files map[string]string
	if err := json.Unmarshal(decoded, &files); err != nil {
		return nil, fmt.Errorf("unmarshal jit config blob: %w", err)
	}

	var runnerCfg struct {
		ServerURL string `json:"serverUrl"`
	}
	if err := json.Unmarshal([]byte(files[".runner"]), &runnerCfg); err != nil {
		return nil, fmt.Errorf("parse .runner config: %w", err)
	}

	var credCfg struct {
		Data struct {
			ClientID         string `json:"clientId"`
			AuthorizationURL string `json:"authorizationUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(files[".credentials"]), &credCfg); err != nil {
		return nil, fmt.Errorf("parse .credentials config: %w", err)
	}

	privKey, err := parseRSAParamsXML(files[".credentials_rsaparams"])
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
		BrokerURL:        runnerCfg.ServerURL,
		PrivateKeyPEM:    privKeyPEM,
	}, nil
}

// rsaParamsXML matches the .NET RSAKeyValue XML format written by the runner.
// Child elements contain standard base64-encoded RSA parameter bytes.
// The root element name is not validated to handle both RSAKeyValue and RSAParameters.
type rsaParamsXML struct {
	Exponent string `xml:"Exponent"`
	Modulus  string `xml:"Modulus"`
	P        string `xml:"P"`
	Q        string `xml:"Q"`
	DP       string `xml:"DP"`
	DQ       string `xml:"DQ"`
	InverseQ string `xml:"InverseQ"`
	D        string `xml:"D"`
}

// parseRSAParamsXML reconstructs an RSA private key from the .NET RSAKeyValue
// XML format stored in .credentials_rsaparams.
func parseRSAParamsXML(xmlStr string) (*rsa.PrivateKey, error) {
	var p rsaParamsXML
	if err := xml.Unmarshal([]byte(xmlStr), &p); err != nil {
		return nil, fmt.Errorf("unmarshal RSA XML: %w", err)
	}

	decodeParam := func(name, b64 string) (*big.Int, error) {
		b, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return new(big.Int).SetBytes(b), nil
	}

	n, err := decodeParam("Modulus", p.Modulus)
	if err != nil {
		return nil, err
	}
	eInt, err := decodeParam("Exponent", p.Exponent)
	if err != nil {
		return nil, err
	}
	d, err := decodeParam("D", p.D)
	if err != nil {
		return nil, err
	}
	pp, err := decodeParam("P", p.P)
	if err != nil {
		return nil, err
	}
	q, err := decodeParam("Q", p.Q)
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
