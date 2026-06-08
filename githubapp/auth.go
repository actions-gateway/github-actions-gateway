// Package githubapp provides GitHub App authentication helpers.
// It generates short-lived installation access tokens by signing a JWT
// with the App's RSA private key and exchanging it with the GitHub API.
package githubapp

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Credentials holds the three values required to authenticate as a GitHub App
// installation. They are read from the GitHub App Secret at startup.
type Credentials struct {
	AppID          int64
	PrivateKeyPEM  []byte
	InstallationID int64
}

// TokenProvider returns a valid installation access token.
// In the probe it is called once at startup. In the AGC (Milestone 2) it
// becomes the Token Manager's refresh target.
type TokenProvider interface {
	Token(ctx context.Context) (string, error)
}

// ExpiringTokenProvider extends TokenProvider with access to the full
// InstallationToken (including expiry). The AGC Token Manager (Milestone 2)
// uses this to schedule proactive refresh ~60 seconds before expiry.
type ExpiringTokenProvider interface {
	TokenProvider
	TokenWithExpiry(ctx context.Context) (*InstallationToken, error)
}

// InstallationToken is a GitHub App installation access token together with
// its expiry time. The AGC Token Manager (Milestone 2) uses ExpiresAt to
// schedule proactive refresh ~60 seconds before expiry.
type InstallationToken struct {
	Token     string
	ExpiresAt time.Time
}

// installationTokenProvider implements TokenProvider by minting a fresh
// installation access token on every call.
type installationTokenProvider struct {
	creds      Credentials
	privateKey crypto.Signer // *rsa.PrivateKey (RS256) or ed25519.PrivateKey (EdDSA)
	httpClient *http.Client
}

// NewInstallationTokenProvider returns a TokenProvider that mints a fresh
// installation access token on each call by signing a JWT and exchanging it
// with the GitHub Apps API.
//
// An error is returned immediately if creds.PrivateKeyPEM cannot be parsed,
// so callers surface bad-credential failures at startup rather than on the
// first call.
func NewInstallationTokenProvider(creds Credentials, httpClient *http.Client) (TokenProvider, error) {
	key, err := parsePrivateKey(creds.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("githubapp: parse private key: %w", err)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &installationTokenProvider{
		creds:      creds,
		privateKey: key,
		httpClient: httpClient,
	}, nil
}

// Token mints a new installation access token. It signs a short-lived JWT,
// POSTs it to the GitHub Apps token endpoint, and returns the token string.
func (p *installationTokenProvider) Token(ctx context.Context) (string, error) {
	tok, err := p.fetchToken(ctx)
	if err != nil {
		return "", err
	}
	return tok.Token, nil
}

// TokenWithExpiry mints a new installation access token and returns it
// together with its expiry time, satisfying ExpiringTokenProvider.
func (p *installationTokenProvider) TokenWithExpiry(ctx context.Context) (*InstallationToken, error) {
	return p.fetchToken(ctx)
}

// fetchToken is the shared implementation for Token and TokenWithExpiry.
func (p *installationTokenProvider) fetchToken(ctx context.Context) (*InstallationToken, error) {
	now := time.Now()

	// Choose signing method based on key type.
	var signingMethod jwt.SigningMethod
	switch p.privateKey.(type) {
	case *rsa.PrivateKey:
		signingMethod = jwt.SigningMethodRS256
	case ed25519.PrivateKey:
		signingMethod = jwt.SigningMethodEdDSA
	default:
		return nil, fmt.Errorf("githubapp: unsupported key type %T", p.privateKey)
	}

	// iat is set 60 seconds in the past to absorb clock skew between this host
	// and GitHub's servers. GitHub rejects JWTs whose iat is in the future.
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
		Issuer:    fmt.Sprintf("%d", p.creds.AppID),
		ID:        newUUID(), // jti: prevents replay of intercepted JWTs
	}
	jwtToken := jwt.NewWithClaims(signingMethod, claims)
	signed, err := jwtToken.SignedString(p.privateKey)
	if err != nil {
		return nil, fmt.Errorf("githubapp: sign JWT: %w", err)
	}

	// GITHUB_API_BASE_URL lets tests redirect token exchange to a fake server.
	apiBase := os.Getenv("GITHUB_API_BASE_URL")
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", apiBase, p.creds.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, fmt.Errorf("githubapp: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+signed)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubapp: POST access_tokens: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("githubapp: POST access_tokens returned %d", resp.StatusCode)
	}

	var body struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubapp: decode access_tokens response: %w", err)
	}
	return &InstallationToken{Token: body.Token, ExpiresAt: body.ExpiresAt}, nil
}

// parsePrivateKey decodes a PEM-encoded private key and returns it as a
// crypto.Signer. Accepted formats:
//   - PKCS#1 "RSA PRIVATE KEY" — RSA (legacy GitHub App key format)
//   - PKCS#8 "PRIVATE KEY"     — RSA or Ed25519
func parsePrivateKey(pemBytes []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		signer, ok := key.(crypto.Signer)
		if !ok {
			return nil, fmt.Errorf("PKCS#8 key type %T does not implement crypto.Signer", key)
		}
		return signer, nil
	default:
		return nil, fmt.Errorf("unsupported PEM block type: %s", block.Type)
	}
}
