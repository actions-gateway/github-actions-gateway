package githubapp_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/githubapp"
)

// pkgTestRSAKey is a shared 2048-bit RSA key generated once per test binary run.
var (
	pkgTestRSAKey = func() *rsa.PrivateKey {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		return k
	}()
	pkgTestRSAKeyPEM = func() []byte {
		return pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(pkgTestRSAKey),
		})
	}()
)

// generateTestKey returns the shared package-level RSA test key and its PEM encoding.
func generateTestKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	return pkgTestRSAKey, pkgTestRSAKeyPEM
}

func TestToken_HappyPath(t *testing.T) {
	t.Parallel()
	_, pemBytes := generateTestKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Contains(t, r.URL.Path, "/access_tokens")

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "ghs_testtoken123",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	creds := githubapp.Credentials{
		AppID:          42,
		PrivateKeyPEM:  pemBytes,
		InstallationID: 999,
	}
	// Point the provider at the test server by rewriting the URL via a custom
	// transport that redirects api.github.com to our httptest server.
	client := testClientRedirectingTo(srv.URL)
	provider, err := githubapp.NewInstallationTokenProvider(creds, client, false)
	require.NoError(t, err)

	tok, err := provider.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ghs_testtoken123", tok)
}

func TestToken_BadPrivateKey(t *testing.T) {
	t.Parallel()
	creds := githubapp.Credentials{
		AppID:          1,
		PrivateKeyPEM:  []byte("not a valid pem"),
		InstallationID: 1,
	}
	_, err := githubapp.NewInstallationTokenProvider(creds, nil, false)
	require.Error(t, err)
	// Must not panic — the error is returned, not propagated as a panic.
}

func TestToken_NonOKResponse(t *testing.T) {
	t.Parallel()
	_, pemBytes := generateTestKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	creds := githubapp.Credentials{AppID: 1, PrivateKeyPEM: pemBytes, InstallationID: 1}
	provider, err := githubapp.NewInstallationTokenProvider(creds, testClientRedirectingTo(srv.URL), false)
	require.NoError(t, err)

	_, err = provider.Token(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}

func TestToken_ClockSkewBuffer(t *testing.T) {
	t.Parallel()
	_, pemBytes := generateTestKey(t)

	var receivedIAT int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Parse the JWT from the Authorization header to inspect its iat claim.
		authHeader := r.Header.Get("Authorization")
		require.NotEmpty(t, authHeader)
		tokenStr := authHeader[len("Bearer "):]

		// Parse without verification (we just need the claims).
		parser := jwt.NewParser(jwt.WithoutClaimsValidation())
		claims := &jwt.RegisteredClaims{}
		_, _, err := parser.ParseUnverified(tokenStr, claims)
		require.NoError(t, err)
		receivedIAT = claims.IssuedAt.Unix()

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "ghs_x",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	creds := githubapp.Credentials{AppID: 1, PrivateKeyPEM: pemBytes, InstallationID: 1}
	provider, err := githubapp.NewInstallationTokenProvider(creds, testClientRedirectingTo(srv.URL), false)
	require.NoError(t, err)

	before := time.Now()
	_, err = provider.Token(context.Background())
	require.NoError(t, err)

	// iat must be at least 60 seconds before the call started (clock-skew buffer).
	assert.LessOrEqual(t, receivedIAT, before.Add(-60*time.Second).Unix(),
		"iat should be at least 60s before now to absorb clock skew")
}

func TestToken_ExpiresAtParsed(t *testing.T) {
	t.Parallel()
	_, pemBytes := generateTestKey(t)
	wantExpiry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{ //nolint:gosec // G101: synthetic token in a test fixture response, not a real credential
			"token":      "ghs_exptest",
			"expires_at": wantExpiry.Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	creds := githubapp.Credentials{AppID: 1, PrivateKeyPEM: pemBytes, InstallationID: 1}
	provider, err := githubapp.NewInstallationTokenProvider(creds, testClientRedirectingTo(srv.URL), false)
	require.NoError(t, err)

	// Call the extended interface to inspect the expiry.
	ep, ok := provider.(githubapp.ExpiringTokenProvider)
	require.True(t, ok, "provider must implement ExpiringTokenProvider")

	it, err := ep.TokenWithExpiry(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ghs_exptest", it.Token)
	assert.Equal(t, wantExpiry, it.ExpiresAt.UTC().Truncate(time.Second))
}

// testClientRedirectingTo returns an *http.Client whose transport rewrites
// every request to target baseURL (preserving path and query). This lets unit
// tests point the provider at an httptest.Server without DNS tricks.
func testClientRedirectingTo(baseURL string) *http.Client {
	return &http.Client{
		Transport: &redirectTransport{base: baseURL},
	}
}

type redirectTransport struct {
	base string
}

func (rt *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.URL.Scheme = "http"
	// Extract host from baseURL (strip scheme).
	host := rt.base
	if len(host) > 7 && host[:7] == "http://" {
		host = host[7:]
	}
	cloned.URL.Host = host
	return http.DefaultTransport.RoundTrip(cloned)
}

// ── parseRSAPrivateKey PKCS#8 paths ──────────────────────────────────────────

func TestToken_PKCS8Key(t *testing.T) {
	t.Parallel()
	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(pkgTestRSAKey)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8Bytes})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{ //nolint:gosec // G101: synthetic token in a test fixture response, not a real credential
			"token":      "ghs_pkcs8",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	provider, err := githubapp.NewInstallationTokenProvider(
		githubapp.Credentials{AppID: 1, PrivateKeyPEM: pemBytes, InstallationID: 1},
		testClientRedirectingTo(srv.URL),
		false,
	)
	require.NoError(t, err)

	tok, err := provider.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ghs_pkcs8", tok)
}

func TestToken_PKCS8NonRSAKey(t *testing.T) {
	t.Parallel()
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(ecKey)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8Bytes})

	// An EC key parses as a crypto.Signer but has no GitHub-App JWT alg, so the
	// provider rejects it at construction — fail-fast at startup rather than on the
	// first token mint (the documented contract: surface key failures immediately).
	_, err = githubapp.NewInstallationTokenProvider(
		githubapp.Credentials{AppID: 1, PrivateKeyPEM: pemBytes, InstallationID: 1},
		nil,
		false,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported key type")
}

func TestToken_Ed25519Key(t *testing.T) {
	t.Parallel()
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(edKey)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8Bytes})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{ //nolint:gosec // G101: synthetic token in a test fixture response, not a real credential
			"token":      "ghs_ed25519",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	provider, err := githubapp.NewInstallationTokenProvider(
		githubapp.Credentials{AppID: 1, PrivateKeyPEM: pemBytes, InstallationID: 1},
		testClientRedirectingTo(srv.URL),
		false,
	)
	require.NoError(t, err)
	tok, err := provider.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ghs_ed25519", tok)
}

func TestToken_JTIIsUniquePerCall(t *testing.T) {
	t.Parallel()
	_, pemBytes := generateTestKey(t)

	var bearerTokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearerTokens = append(bearerTokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "ghs_test",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	provider, err := githubapp.NewInstallationTokenProvider(
		githubapp.Credentials{AppID: 1, PrivateKeyPEM: pemBytes, InstallationID: 1},
		testClientRedirectingTo(srv.URL),
		false,
	)
	require.NoError(t, err)

	_, err = provider.Token(context.Background())
	require.NoError(t, err)
	_, err = provider.Token(context.Background())
	require.NoError(t, err)

	require.Len(t, bearerTokens, 2)
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	var c1, c2 jwt.RegisteredClaims
	_, _, err = parser.ParseUnverified(bearerTokens[0], &c1)
	require.NoError(t, err)
	_, _, err = parser.ParseUnverified(bearerTokens[1], &c2)
	require.NoError(t, err)
	assert.NotEmpty(t, c1.ID, "jti should be set")
	assert.NotEmpty(t, c2.ID, "jti should be set")
	assert.NotEqual(t, c1.ID, c2.ID, "jti must differ across calls")
}

func TestToken_UnsupportedPEMType(t *testing.T) {
	t.Parallel()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("fake")})

	_, err := githubapp.NewInstallationTokenProvider(
		githubapp.Credentials{AppID: 1, PrivateKeyPEM: pemBytes, InstallationID: 1},
		nil,
		false,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported PEM block type")
}

// ── GITHUB_API_BASE_URL scheme validation (Q146) ─────────────────────────────

// TestBaseURL_HTTPSAccepted confirms an explicit HTTPS GITHUB_API_BASE_URL is
// accepted with no dev/test opt-in (the production-default path).
func TestBaseURL_HTTPSAccepted(t *testing.T) {
	_, pemBytes := generateTestKey(t)
	t.Setenv("GITHUB_API_BASE_URL", "https://ghe.example.com/api/v3")

	_, err := githubapp.NewInstallationTokenProvider(
		githubapp.Credentials{AppID: 1, PrivateKeyPEM: pemBytes, InstallationID: 1},
		nil,
		false,
	)
	require.NoError(t, err)
}

// TestBaseURL_DefaultHTTPSAccepted confirms the unset → https://api.github.com
// default is accepted without the dev/test opt-in.
func TestBaseURL_DefaultHTTPSAccepted(t *testing.T) {
	_, pemBytes := generateTestKey(t)
	t.Setenv("GITHUB_API_BASE_URL", "") // explicitly unset

	_, err := githubapp.NewInstallationTokenProvider(
		githubapp.Credentials{AppID: 1, PrivateKeyPEM: pemBytes, InstallationID: 1},
		nil,
		false,
	)
	require.NoError(t, err)
}

// TestBaseURL_HTTPRejectedWithoutOptIn confirms a plaintext GITHUB_API_BASE_URL
// is rejected by default — the secure-by-default behavior. The error must name
// the URL but carry no token material.
func TestBaseURL_HTTPRejectedWithoutOptIn(t *testing.T) {
	_, pemBytes := generateTestKey(t)
	t.Setenv("GITHUB_API_BASE_URL", "http://fakegithub.infra.svc.cluster.local:8080")

	_, err := githubapp.NewInstallationTokenProvider(
		githubapp.Credentials{AppID: 1, PrivateKeyPEM: pemBytes, InstallationID: 1},
		nil,
		false, // no dev/test signal
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-HTTPS")
	assert.Contains(t, err.Error(), "http://fakegithub.infra.svc.cluster.local:8080")
}

// TestBaseURL_HTTPAcceptedWithOptIn confirms a plaintext GITHUB_API_BASE_URL is
// accepted when the explicit dev/test opt-in is set — the carve-out that keeps
// the in-cluster fakegithub e2e path working.
func TestBaseURL_HTTPAcceptedWithOptIn(t *testing.T) {
	_, pemBytes := generateTestKey(t)
	t.Setenv("GITHUB_API_BASE_URL", "http://fakegithub.infra.svc.cluster.local:8080")

	_, err := githubapp.NewInstallationTokenProvider(
		githubapp.Credentials{AppID: 1, PrivateKeyPEM: pemBytes, InstallationID: 1},
		nil,
		true, // explicit dev/test opt-in
	)
	require.NoError(t, err)
}

// ── ResolveAPIBaseURL (Q504) ─────────────────────────────────────────────────

// TestResolveAPIBaseURL covers the exported resolver directly. It exists because
// the token exchange is not the only GitHub REST caller in the control plane: the
// AGC's disruption auto-retry builds its rerun-failed-jobs URL from the same
// answer, and Q504 was that call defaulting past a configured GHES endpoint to
// api.github.com. The resolver is what makes those two agree by construction, so
// its contract is pinned on its own rather than only through a constructor.
func TestResolveAPIBaseURL(t *testing.T) {
	tests := []struct {
		name          string
		env           string
		allowInsecure bool
		want          string
		wantErr       string
	}{
		{
			name: "explicit HTTPS endpoint is returned verbatim",
			env:  "https://ghe.example.com/api/v3",
			want: "https://ghe.example.com/api/v3",
		},
		{
			// The bug: an unset var must yield the public API, but a SET one must
			// never be silently replaced by it.
			name: "unset defaults to the public API",
			env:  "",
			want: "https://api.github.com",
		},
		{
			name:    "plaintext is rejected without the opt-in",
			env:     "http://fakegithub.infra.svc.cluster.local:8080",
			wantErr: "non-HTTPS",
		},
		{
			name:          "plaintext is accepted with the dev/test opt-in",
			env:           "http://fakegithub.infra.svc.cluster.local:8080",
			allowInsecure: true,
			want:          "http://fakegithub.infra.svc.cluster.local:8080",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_API_BASE_URL", tc.env)
			got, err := githubapp.ResolveAPIBaseURL(tc.allowInsecure)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Empty(t, got, "a rejected base URL must not be returned for use anyway")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ── DeriveAPIBaseURL (Q506) ──────────────────────────────────────────────────

// TestDeriveAPIBaseURL pins the gitHubURL → API base derivation the GMC, the runner
// registrar, and the scale-set client now share. The github.com-in-the-path case is
// the defect that motivated collapsing them: the registrar's old substring test read
// that GHES gateway as public SaaS and sent its registration calls to api.github.com.
func TestDeriveAPIBaseURL(t *testing.T) {
	tests := []struct {
		name      string
		githubURL string
		want      string
	}{
		{"public org", "https://github.com/acme", "https://api.github.com"},
		{"public repo", "https://github.com/acme/repo", "https://api.github.com"},
		{"www host", "https://www.github.com/acme", "https://api.github.com"},
		{"api host", "https://api.github.com", "https://api.github.com"},
		{"public host with port", "https://github.com:443/acme", "https://api.github.com"},
		{"ghes org", "https://ghes.example.com/acme", "https://ghes.example.com/api/v3"},
		{"ghes repo", "https://ghes.example.com/acme/repo", "https://ghes.example.com/api/v3"},
		{"ghes on a non-default port keeps the port", "https://ghes.example.com:8443/acme", "https://ghes.example.com:8443/api/v3"},
		{"ghes org path named github.com is not public", "https://ghes.example.com/github.com", "https://ghes.example.com/api/v3"},
		{"trailing slash", "https://ghes.example.com/acme/", "https://ghes.example.com/api/v3"},
		{"empty falls back to the public API", "", "https://api.github.com"},
		{"unparseable falls back to the public API", "not a url", "https://api.github.com"},
		{"scheme is preserved for the e2e stub", "http://fakegithub.e2e-infra.svc.cluster.local:8080/acme", "http://fakegithub.e2e-infra.svc.cluster.local:8080/api/v3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, githubapp.DeriveAPIBaseURL(tc.githubURL))
		})
	}
}
