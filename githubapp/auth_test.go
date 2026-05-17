package githubapp_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/karlkfi/github-actions-gateway/githubapp"
)

// generateTestKey creates a throwaway RSA key for testing.
func generateTestKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return key, pemBytes
}

func TestToken_HappyPath(t *testing.T) {
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
	provider, err := githubapp.NewInstallationTokenProvider(creds, client)
	require.NoError(t, err)

	tok, err := provider.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ghs_testtoken123", tok)
}

func TestToken_BadPrivateKey(t *testing.T) {
	creds := githubapp.Credentials{
		AppID:          1,
		PrivateKeyPEM:  []byte("not a valid pem"),
		InstallationID: 1,
	}
	_, err := githubapp.NewInstallationTokenProvider(creds, nil)
	require.Error(t, err)
	// Must not panic — the error is returned, not propagated as a panic.
}

func TestToken_NonOKResponse(t *testing.T) {
	_, pemBytes := generateTestKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	creds := githubapp.Credentials{AppID: 1, PrivateKeyPEM: pemBytes, InstallationID: 1}
	provider, err := githubapp.NewInstallationTokenProvider(creds, testClientRedirectingTo(srv.URL))
	require.NoError(t, err)

	_, err = provider.Token(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}

func TestToken_ClockSkewBuffer(t *testing.T) {
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
	provider, err := githubapp.NewInstallationTokenProvider(creds, testClientRedirectingTo(srv.URL))
	require.NoError(t, err)

	before := time.Now()
	_, err = provider.Token(context.Background())
	require.NoError(t, err)

	// iat must be at least 60 seconds before the call started (clock-skew buffer).
	assert.LessOrEqual(t, receivedIAT, before.Add(-60*time.Second).Unix(),
		"iat should be at least 60s before now to absorb clock skew")
}

func TestToken_ExpiresAtParsed(t *testing.T) {
	_, pemBytes := generateTestKey(t)
	wantExpiry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "ghs_exptest",
			"expires_at": wantExpiry.Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	creds := githubapp.Credentials{AppID: 1, PrivateKeyPEM: pemBytes, InstallationID: 1}
	provider, err := githubapp.NewInstallationTokenProvider(creds, testClientRedirectingTo(srv.URL))
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
