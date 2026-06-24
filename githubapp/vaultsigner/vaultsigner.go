// Package vaultsigner implements the workload-identity external signer (Q197): it
// signs the GitHub App JWT with a key held in HashiCorp Vault's transit secrets
// engine, so the App private key never enters the cluster. It satisfies
// githubapp.Signer.
//
// The AGC authenticates to Vault with its pod identity (Vault Kubernetes auth):
// it presents its projected ServiceAccount token, Vault verifies it via the
// cluster TokenReview API and returns a short-lived Vault client token, and the
// signer then asks Vault transit to sign the App JWT (RSASSA-PKCS1-v1_5 over
// SHA-256 = RS256). No App key, and no long-lived Vault token, is ever stored in
// the cluster — the only credential is the kubelet-minted projected token, read
// fresh from disk at each login.
//
// Security: this package MUST NOT log, return in errors, or otherwise emit the
// ServiceAccount token, the Vault client token, or the produced signature. Errors
// surface Vault's own operational messages (e.g. "permission denied") and HTTP
// status, never credential material.
package vaultsigner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
)

// defaultTokenPath is where the kubelet projects a pod's ServiceAccount token by
// default. The GMC may project a dedicated, audience-scoped token elsewhere; that
// path is then set on Config.TokenPath.
const defaultTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // G101: a well-known mount path, not a credential

// tokenRenewMargin re-logs in this long before the Vault client token's lease
// expires, so a sign never races a just-expired token.
const tokenRenewMargin = 30 * time.Second

// Config configures a Vault transit Signer. Address, KeyName, Role identify the
// Vault backend and the AGC's Vault identity; the mounts default to Vault's
// conventional paths.
type Config struct {
	// Address is the Vault API base URL (e.g. https://vault.vault.svc:8200). HTTPS
	// is required unless AllowInsecureAddress is set (the dev/test opt-in) — the
	// ServiceAccount token transits this channel at login.
	Address string
	// TransitMount is the transit secrets-engine mount path. Empty ⇒ "transit".
	TransitMount string
	// KeyName is the transit key that signs the App JWT (an RSA key).
	KeyName string
	// AuthMount is the Kubernetes auth-method mount path. Empty ⇒ "kubernetes".
	AuthMount string
	// Role is the Vault Kubernetes auth role the AGC logs in as.
	Role string
	// TokenPath is the file holding the pod's projected ServiceAccount token. Empty
	// ⇒ the default kubelet projection path. Read fresh at each login.
	TokenPath string
	// VaultNamespace, when set, is sent as X-Vault-Namespace (Vault Enterprise
	// namespacing). Optional.
	VaultNamespace string
	// AllowInsecureAddress permits a plaintext (http://) Address — the explicit
	// dev/test opt-in, mirroring the GitHub token-exchange channel. Callers gate it
	// on a signal production never carries. Never set in production.
	AllowInsecureAddress bool
	// HTTPClient is the client used for Vault calls. nil ⇒ a bounded httpx client.
	HTTPClient *http.Client
	// now returns the current time; nil ⇒ time.Now. Injected by tests to drive
	// token-lease expiry deterministically.
	now func() time.Time
	// readToken reads the projected ServiceAccount token; nil ⇒ reads TokenPath.
	// Injected by tests.
	readToken func() ([]byte, error)
}

// Signer signs the App JWT via Vault transit. It is safe for concurrent use: the
// cached Vault client token is guarded by a mutex and re-fetched on expiry.
type Signer struct {
	addr           string // Address without a trailing slash
	transitMount   string
	keyName        string
	authMount      string
	role           string
	vaultNamespace string
	httpClient     *http.Client
	now            func() time.Time
	readToken      func() ([]byte, error)

	mu         sync.Mutex
	cachedTok  string    // Vault client token; "" until first login
	tokExpires time.Time // lease expiry of cachedTok
}

// New validates the config and returns a Signer. It returns an error if Address
// is empty/malformed, non-HTTPS without the opt-in, or if KeyName/Role are empty
// — so misconfiguration fails at startup rather than on the first sign.
func New(cfg Config) (*Signer, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("vaultsigner: Address is required")
	}
	u, err := url.Parse(cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("vaultsigner: invalid Address %q: %w", cfg.Address, err)
	}
	if u.Scheme != "https" && !(cfg.AllowInsecureAddress && u.Scheme == "http") {
		return nil, fmt.Errorf("vaultsigner: refusing non-HTTPS Vault Address %q: "+
			"the ServiceAccount token transits this channel at login and must use HTTPS; "+
			"plaintext is permitted only under an explicit dev/test opt-in", cfg.Address)
	}
	if cfg.KeyName == "" {
		return nil, fmt.Errorf("vaultsigner: KeyName is required")
	}
	if cfg.Role == "" {
		return nil, fmt.Errorf("vaultsigner: Role is required")
	}

	transitMount := cfg.TransitMount
	if transitMount == "" {
		transitMount = "transit"
	}
	authMount := cfg.AuthMount
	if authMount == "" {
		authMount = "kubernetes"
	}
	nowFn := cfg.now
	if nowFn == nil {
		nowFn = time.Now
	}
	readTok := cfg.readToken
	if readTok == nil {
		tokenPath := cfg.TokenPath
		if tokenPath == "" {
			tokenPath = defaultTokenPath
		}
		readTok = func() ([]byte, error) { return os.ReadFile(tokenPath) }
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = httpx.NewClient()
	}

	return &Signer{
		addr:           strings.TrimRight(cfg.Address, "/"),
		transitMount:   strings.Trim(transitMount, "/"),
		keyName:        cfg.KeyName,
		authMount:      strings.Trim(authMount, "/"),
		role:           cfg.Role,
		vaultNamespace: cfg.VaultNamespace,
		httpClient:     httpClient,
		now:            nowFn,
		readToken:      readTok,
	}, nil
}

// JWTAlg reports the JWS alg this signer produces. Vault transit signs an RSA key
// as RSASSA-PKCS1-v1_5 over SHA-256, which is JWS "RS256".
func (s *Signer) JWTAlg() string { return "RS256" }

// Sign signs the JWT signing input via Vault transit and returns the raw
// signature. It logs in to Vault first if the cached client token is missing or
// near expiry.
func (s *Signer) Sign(ctx context.Context, signingInput []byte) ([]byte, error) {
	token, err := s.clientToken(ctx)
	if err != nil {
		return nil, err
	}
	sig, err := s.transitSign(ctx, token, signingInput)
	if err != nil {
		return nil, err
	}
	return sig, nil
}

// clientToken returns a valid Vault client token, logging in if the cache is
// empty or within tokenRenewMargin of expiry.
func (s *Signer) clientToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cachedTok != "" && s.now().Before(s.tokExpires.Add(-tokenRenewMargin)) {
		return s.cachedTok, nil
	}

	token, ttl, err := s.login(ctx)
	if err != nil {
		return "", err
	}
	s.cachedTok = token
	s.tokExpires = s.now().Add(ttl)
	return token, nil
}

// login performs Vault Kubernetes auth: it reads the pod's projected
// ServiceAccount token fresh from disk and exchanges it for a Vault client token.
// The ServiceAccount token and the returned client token are never logged.
func (s *Signer) login(ctx context.Context) (token string, ttl time.Duration, err error) {
	saToken, err := s.readToken()
	if err != nil {
		return "", 0, fmt.Errorf("vaultsigner: read ServiceAccount token: %w", err)
	}

	endpoint := fmt.Sprintf("%s/v1/auth/%s/login", s.addr, s.authMount)
	reqBody, err := json.Marshal(map[string]string{
		"role": s.role,
		"jwt":  strings.TrimSpace(string(saToken)),
	})
	if err != nil {
		return "", 0, fmt.Errorf("vaultsigner: marshal login request: %w", err)
	}

	resp, err := s.do(ctx, endpoint, reqBody)
	if err != nil {
		return "", 0, fmt.Errorf("vaultsigner: Vault login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("vaultsigner: Vault login returned %d: %s", resp.StatusCode, vaultErrors(resp))
	}

	var body struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", 0, fmt.Errorf("vaultsigner: decode Vault login response: %w", err)
	}
	if body.Auth.ClientToken == "" {
		return "", 0, fmt.Errorf("vaultsigner: Vault login response carried no client token")
	}
	return body.Auth.ClientToken, time.Duration(body.Auth.LeaseDuration) * time.Second, nil
}

// transitSign asks Vault transit to sign signingInput with the configured key and
// returns the raw signature. Vault hashes the input with SHA-256 and signs with
// PKCS#1 v1.5, yielding an RS256 signature.
func (s *Signer) transitSign(ctx context.Context, clientToken string, signingInput []byte) ([]byte, error) {
	endpoint := fmt.Sprintf("%s/v1/%s/sign/%s", s.addr, s.transitMount, s.keyName)
	reqBody, err := json.Marshal(map[string]string{
		"input":               base64.StdEncoding.EncodeToString(signingInput),
		"hash_algorithm":      "sha2-256",
		"signature_algorithm": "pkcs1v15",
	})
	if err != nil {
		return nil, fmt.Errorf("vaultsigner: marshal sign request: %w", err)
	}

	resp, err := s.do(ctx, endpoint, reqBody, header{"X-Vault-Token", clientToken})
	if err != nil {
		return nil, fmt.Errorf("vaultsigner: Vault transit sign: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vaultsigner: Vault transit sign returned %d: %s", resp.StatusCode, vaultErrors(resp))
	}

	var body struct {
		Data struct {
			Signature string `json:"signature"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("vaultsigner: decode Vault sign response: %w", err)
	}
	return decodeVaultSignature(body.Data.Signature)
}

// header is a single HTTP header key/value to set on a Vault request.
type header struct{ key, value string }

// do issues a POST with the JSON body to a Vault endpoint, setting the optional
// Vault-namespace header and any extra headers (e.g. the client token).
func (s *Signer) do(ctx context.Context, endpoint string, body []byte, headers ...header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.vaultNamespace != "" {
		req.Header.Set("X-Vault-Namespace", s.vaultNamespace)
	}
	for _, h := range headers {
		req.Header.Set(h.key, h.value)
	}
	return s.httpClient.Do(req)
}

// decodeVaultSignature parses Vault transit's "vault:v<n>:<base64>" signature
// format and returns the raw signature bytes.
func decodeVaultSignature(sig string) ([]byte, error) {
	// Format: vault:v<keyversion>:<std-base64 signature>. The signature segment is
	// base64 and contains no colon, so a 3-way split isolates it.
	parts := strings.SplitN(sig, ":", 3)
	if len(parts) != 3 || parts[0] != "vault" || parts[2] == "" {
		return nil, fmt.Errorf("vaultsigner: malformed transit signature")
	}
	raw, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("vaultsigner: decode transit signature: %w", err)
	}
	return raw, nil
}

// vaultErrors extracts Vault's JSON error messages ({"errors": [...]}) from a
// non-2xx response for inclusion in an error. These are operational messages
// ("permission denied", "missing client token"); they never carry credential
// material. Returns a generic note if the body is absent or unparseable.
func vaultErrors(resp *http.Response) string {
	var body struct {
		Errors []string `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || len(body.Errors) == 0 {
		return "(no error detail)"
	}
	return strings.Join(body.Errors, "; ")
}
