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
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
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

// defaultAPIBaseURL is the production GitHub REST API base used for token
// exchange when GITHUB_API_BASE_URL is unset.
const defaultAPIBaseURL = "https://api.github.com"

// Signer signs the GitHub App authentication JWT. It abstracts the cryptographic
// sign so the App key's location is pluggable. Two trust models implement it:
//
//   - pemSigner — the possession model: the App private key lives in-process (a
//     namespace Secret mounted into the AGC). Built by NewInstallationTokenProvider.
//   - an external signer (e.g. githubapp/vaultsigner) — the delegation model
//     (Q197): the key lives in an external trust anchor and never enters the
//     cluster; the AGC proves its identity to the anchor, which signs on its
//     behalf. Built by NewInstallationTokenProviderWithSigner.
//
// Sign receives the JWT signing input (the "header.payload" bytes golang-jwt
// produces) and returns the raw signature; the provider base64url-encodes it as
// the JWS signature. JWTAlg returns the "alg" header value the signer produces,
// which the provider stamps into the JWT header.
//
// A Signer implementation MUST NOT log, return in errors, or otherwise emit any
// key, token, or signature material.
type Signer interface {
	// JWTAlg returns the JWS "alg" header value this signer produces — "RS256" for
	// an RSA key (GitHub App keys), "EdDSA" for an Ed25519 key.
	JWTAlg() string
	// Sign signs the JWT signing input (the "header.payload" bytes) and returns the
	// raw signature.
	Sign(ctx context.Context, signingInput []byte) ([]byte, error)
}

// pemSigner is the in-cluster Signer: it holds the App private key in-process and
// signs locally with golang-jwt. It preserves the historical possession-model
// behavior, including Ed25519 support.
type pemSigner struct {
	key    crypto.Signer     // *rsa.PrivateKey (RS256) or ed25519.PrivateKey (EdDSA)
	method jwt.SigningMethod // chosen from the key type at construction
}

// newPEMSigner parses a PEM-encoded App private key and returns a pemSigner that
// signs RS256 (RSA) or EdDSA (Ed25519).
func newPEMSigner(pemBytes []byte) (*pemSigner, error) {
	key, err := parsePrivateKey(pemBytes)
	if err != nil {
		return nil, err
	}
	var method jwt.SigningMethod
	switch key.(type) {
	case *rsa.PrivateKey:
		method = jwt.SigningMethodRS256
	case ed25519.PrivateKey:
		method = jwt.SigningMethodEdDSA
	default:
		return nil, fmt.Errorf("unsupported key type %T", key)
	}
	return &pemSigner{key: key, method: method}, nil
}

// JWTAlg returns the JWS alg for the held key ("RS256" or "EdDSA").
func (s *pemSigner) JWTAlg() string { return s.method.Alg() }

// Sign signs the input locally with the held key. The context is unused (no I/O).
func (s *pemSigner) Sign(_ context.Context, signingInput []byte) ([]byte, error) {
	return s.method.Sign(string(signingInput), s.key)
}

// installationTokenProvider implements TokenProvider by minting a fresh
// installation access token on every call. It signs the App JWT through a Signer,
// so the App key may live in-process (pemSigner) or in an external anchor.
type installationTokenProvider struct {
	appID          int64
	installationID int64
	signer         Signer
	httpClient     *http.Client
	apiBaseURL     string // validated at construction; HTTPS unless dev/test opt-in
}

// NewInstallationTokenProvider returns a TokenProvider that mints a fresh
// installation access token on each call by signing a JWT and exchanging it
// with the GitHub Apps API. The App private key is held in-process (the
// possession model); for the no-key delegation model use
// NewInstallationTokenProviderWithSigner.
//
// The token-exchange endpoint is GITHUB_API_BASE_URL (defaulting to
// https://api.github.com). Because that exchange carries App-JWT and
// installation-token material, a non-HTTPS base URL would expose credentials
// on the wire and is REJECTED by default. allowInsecureBaseURL is the explicit
// dev/test opt-in that permits a plaintext (http://) base URL — callers must
// gate it on a signal that production never carries (e.g. the AGC's stub env);
// see docs/design/05-security.md.
//
// An error is returned immediately if creds.PrivateKeyPEM cannot be parsed or
// if GITHUB_API_BASE_URL is non-HTTPS without the opt-in, so callers surface
// these failures at startup rather than on the first token mint.
func NewInstallationTokenProvider(creds Credentials, httpClient *http.Client, allowInsecureBaseURL bool) (TokenProvider, error) {
	signer, err := newPEMSigner(creds.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("githubapp: parse private key: %w", err)
	}
	return NewInstallationTokenProviderWithSigner(creds.AppID, creds.InstallationID, signer, httpClient, allowInsecureBaseURL)
}

// NewInstallationTokenProviderWithSigner returns an ExpiringTokenProvider that
// mints installation access tokens by signing the App JWT through the given
// Signer — the no-PEM delegation path (Q197). The App private key never enters
// this process: the Signer (e.g. githubapp/vaultsigner) delegates the sign to an
// external trust anchor.
//
// The GITHUB_API_BASE_URL HTTPS rule is identical to NewInstallationTokenProvider.
func NewInstallationTokenProviderWithSigner(appID, installationID int64, signer Signer, httpClient *http.Client, allowInsecureBaseURL bool) (ExpiringTokenProvider, error) {
	if signer == nil {
		return nil, fmt.Errorf("githubapp: signer must not be nil")
	}

	apiBase := os.Getenv("GITHUB_API_BASE_URL")
	if apiBase == "" {
		apiBase = defaultAPIBaseURL
	}
	if err := validateAPIBaseURL(apiBase, allowInsecureBaseURL); err != nil {
		return nil, err
	}

	if httpClient == nil {
		httpClient = httpx.NewClient()
	}
	return &installationTokenProvider{
		appID:          appID,
		installationID: installationID,
		signer:         signer,
		httpClient:     httpClient,
		apiBaseURL:     apiBase,
	}, nil
}

// validateAPIBaseURL enforces HTTPS for the token-exchange base URL. A plaintext
// (http://) URL is rejected unless allowInsecure is set — the explicit dev/test
// opt-in. The error names the offending URL (a non-secret) but never any token
// material.
func validateAPIBaseURL(apiBase string, allowInsecure bool) error {
	u, err := url.Parse(apiBase)
	if err != nil {
		return fmt.Errorf("githubapp: invalid GITHUB_API_BASE_URL %q: %w", apiBase, err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if allowInsecure {
		return nil
	}
	return fmt.Errorf("githubapp: refusing non-HTTPS GITHUB_API_BASE_URL %q: "+
		"GitHub App token exchange must use HTTPS to protect credentials in transit; "+
		"plaintext is permitted only under an explicit dev/test opt-in", apiBase)
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

	// The signing method comes from the Signer's declared alg, so the JWT header
	// matches whatever key the signer wields (RS256 for RSA, EdDSA for Ed25519).
	signingMethod := jwt.GetSigningMethod(p.signer.JWTAlg())
	if signingMethod == nil {
		return nil, fmt.Errorf("githubapp: signer declares unknown JWT alg %q", p.signer.JWTAlg())
	}

	// iat is set 60 seconds in the past to absorb clock skew between this host
	// and GitHub's servers. GitHub rejects JWTs whose iat is in the future.
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
		Issuer:    fmt.Sprintf("%d", p.appID),
		ID:        newUUID(), // jti: prevents replay of intercepted JWTs
	}

	// Build the signing input (base64url(header).base64url(payload)) and hand it
	// to the Signer for the cryptographic sign — which may be a local key or an
	// external anchor — then assemble the compact JWS. Keeping JWT assembly here
	// means every Signer implements only the sign, never the encoding.
	jwtToken := jwt.NewWithClaims(signingMethod, claims)
	signingInput, err := jwtToken.SigningString()
	if err != nil {
		return nil, fmt.Errorf("githubapp: build JWT signing input: %w", err)
	}
	sig, err := p.signer.Sign(ctx, []byte(signingInput))
	if err != nil {
		return nil, fmt.Errorf("githubapp: sign JWT: %w", err)
	}
	signed := signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)

	// apiBaseURL was resolved from GITHUB_API_BASE_URL and validated (HTTPS, or
	// an explicit dev/test opt-in for plaintext) at construction time.
	endpoint := fmt.Sprintf("%s/app/installations/%d/access_tokens", p.apiBaseURL, p.installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
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
