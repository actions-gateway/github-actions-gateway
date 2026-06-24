package vaultsigner_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	githubapp "github.com/actions-gateway/github-actions-gateway/githubapp"
	"github.com/actions-gateway/github-actions-gateway/githubapp/vaultsigner"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// compile-time: a Vault Signer is a githubapp.Signer.
var _ githubapp.Signer = (*vaultsigner.Signer)(nil)

const (
	testSAToken     = "eyJhbGci.sa-token.signature" //nolint:gosec // G101: test sentinel, not a real credential
	testClientToken = "hvs.test-client-token"       //nolint:gosec // G101: test sentinel, not a real credential
	testRole        = "agc-acme"
	testKeyName     = "github-app"
)

// vaultMock is an httptest server that emulates the Vault HTTP API: Kubernetes
// auth login and transit sign. It signs with a real RSA key so the produced
// signature verifies against pub, and counts logins to assert token caching.
type vaultMock struct {
	priv      *rsa.PrivateKey
	logins    atomic.Int32
	signs     atomic.Int32
	leaseSecs int
	loginFail atomic.Bool // when true, /login returns 403 with Vault errors
	signFail  atomic.Bool // when true, /sign returns 403 with Vault errors
	mu        sync.Mutex
	lastInput []byte // the decoded data Vault was asked to sign
}

func newVaultMock(t *testing.T) *vaultMock {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return &vaultMock{priv: priv, leaseSecs: 3600}
}

func (m *vaultMock) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/kubernetes/login", func(w http.ResponseWriter, r *http.Request) {
		m.logins.Add(1)
		var body struct {
			Role string `json:"role"`
			JWT  string `json:"jwt"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, testRole, body.Role)
		assert.Equal(t, testSAToken, body.JWT, "the SA token must be presented at login")
		if m.loginFail.Load() {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string][]string{"errors": {"permission denied"}})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{"client_token": testClientToken, "lease_duration": m.leaseSecs},
		})
	})
	mux.HandleFunc("/v1/transit/sign/"+testKeyName, func(w http.ResponseWriter, r *http.Request) {
		m.signs.Add(1)
		assert.Equal(t, testClientToken, r.Header.Get("X-Vault-Token"), "the client token authenticates the sign")
		if m.signFail.Load() {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string][]string{"errors": {"1 error occurred: key not found"}})
			return
		}
		var body struct {
			Input         string `json:"input"`
			HashAlgorithm string `json:"hash_algorithm"`
			SignatureAlgo string `json:"signature_algorithm"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "sha2-256", body.HashAlgorithm)
		assert.Equal(t, "pkcs1v15", body.SignatureAlgo)
		input, err := base64.StdEncoding.DecodeString(body.Input)
		require.NoError(t, err)
		m.mu.Lock()
		m.lastInput = input
		m.mu.Unlock()
		digest := sha256.Sum256(input)
		sig, err := rsa.SignPKCS1v15(rand.Reader, m.priv, crypto.SHA256, digest[:])
		require.NoError(t, err)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"signature": "vault:v1:" + base64.StdEncoding.EncodeToString(sig)},
		})
	})
	return mux
}

// newSigner builds a Signer pointed at the mock with a fixed-token reader and an
// injectable clock. The clock pointer lets a test advance time to expire the lease.
func newSigner(t *testing.T, srv *httptest.Server, clock *time.Time) *vaultsigner.Signer {
	t.Helper()
	cfg := vaultsigner.Config{
		Address:              srv.URL,
		KeyName:              testKeyName,
		Role:                 testRole,
		AllowInsecureAddress: true, // httptest is plaintext; production requires HTTPS
	}
	s, err := vaultsigner.NewForTest(cfg,
		func() time.Time { return *clock },
		func() ([]byte, error) { return []byte(testSAToken), nil },
	)
	require.NoError(t, err)
	return s
}

func TestSign_VerifiesAsRS256(t *testing.T) {
	mock := newVaultMock(t)
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	clock := time.Now()
	s := newSigner(t, srv, &clock)

	assert.Equal(t, "RS256", s.JWTAlg())

	signingInput := []byte("eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiIxMjM0NSJ9")
	sig, err := s.Sign(context.Background(), signingInput)
	require.NoError(t, err)

	// The raw signature must verify as RSASSA-PKCS1-v1_5 over SHA-256 of the input.
	digest := sha256.Sum256(signingInput)
	require.NoError(t, rsa.VerifyPKCS1v15(&mock.priv.PublicKey, crypto.SHA256, digest[:], sig))

	// Vault was asked to sign exactly the signing input we provided.
	mock.mu.Lock()
	assert.Equal(t, signingInput, mock.lastInput)
	mock.mu.Unlock()
}

func TestSign_EndToEndJWTThroughProvider(t *testing.T) {
	mock := newVaultMock(t)
	vaultSrv := httptest.NewServer(mock.handler(t))
	defer vaultSrv.Close()
	clock := time.Now()
	s := newSigner(t, vaultSrv, &clock)

	// A GitHub mock that verifies the App-JWT's RS256 signature with the Vault key's
	// public key before returning an installation token — proving the whole no-PEM
	// chain (Vault sign → provider assemble → GitHub verify) holds.
	var gotJTI string
	githubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		tok, err := jwt.Parse(bearer, func(*jwt.Token) (any, error) { return &mock.priv.PublicKey, nil },
			jwt.WithValidMethods([]string{"RS256"}))
		require.NoError(t, err)
		require.True(t, tok.Valid)
		claims := tok.Claims.(jwt.MapClaims)
		assert.Equal(t, "424242", claims["iss"])
		gotJTI, _ = claims["jti"].(string)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "ghs_installation", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	defer githubSrv.Close()
	t.Setenv("GITHUB_API_BASE_URL", githubSrv.URL)

	provider, err := githubapp.NewInstallationTokenProviderWithSigner(424242, 99, s, nil, true)
	require.NoError(t, err)
	tok, err := provider.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ghs_installation", tok)
	assert.NotEmpty(t, gotJTI, "the App JWT must carry a jti (replay defense)")
}

func TestSign_CachesClientTokenAcrossSigns(t *testing.T) {
	mock := newVaultMock(t)
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	clock := time.Now()
	s := newSigner(t, srv, &clock)

	for i := 0; i < 3; i++ {
		_, err := s.Sign(context.Background(), []byte("input"))
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), mock.logins.Load(), "the client token should be reused across signs")
	assert.Equal(t, int32(3), mock.signs.Load())
}

func TestSign_ReloginsAfterLeaseExpiry(t *testing.T) {
	mock := newVaultMock(t)
	mock.leaseSecs = 60
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	clock := time.Now()
	s := newSigner(t, srv, &clock)

	_, err := s.Sign(context.Background(), []byte("input"))
	require.NoError(t, err)
	assert.Equal(t, int32(1), mock.logins.Load())

	// Advance past the lease (60s) minus the 30s renew margin → must re-login.
	clock = clock.Add(45 * time.Second)
	_, err = s.Sign(context.Background(), []byte("input"))
	require.NoError(t, err)
	assert.Equal(t, int32(2), mock.logins.Load(), "an expiring lease must trigger re-login")
}

func TestSign_LoginFailureSurfacesVaultErrorWithoutLeakingTokens(t *testing.T) {
	mock := newVaultMock(t)
	mock.loginFail.Store(true)
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	clock := time.Now()
	s := newSigner(t, srv, &clock)

	_, err := s.Sign(context.Background(), []byte("input"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
	assert.Contains(t, err.Error(), "permission denied")
	assertNoTokenLeak(t, err)
}

func TestSign_SignFailureSurfacesVaultErrorWithoutLeakingTokens(t *testing.T) {
	mock := newVaultMock(t)
	mock.signFail.Store(true)
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	clock := time.Now()
	s := newSigner(t, srv, &clock)

	_, err := s.Sign(context.Background(), []byte("input"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key not found")
	assertNoTokenLeak(t, err)
}

func TestSign_SAReadErrorSurfacedNoLeak(t *testing.T) {
	mock := newVaultMock(t)
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	clock := time.Now()
	cfg := vaultsigner.Config{Address: srv.URL, KeyName: testKeyName, Role: testRole, AllowInsecureAddress: true}
	s, err := vaultsigner.NewForTest(cfg,
		func() time.Time { return clock },
		func() ([]byte, error) { return nil, io.ErrUnexpectedEOF },
	)
	require.NoError(t, err)

	_, err = s.Sign(context.Background(), []byte("input"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ServiceAccount token")
	assert.Equal(t, int32(0), mock.logins.Load(), "no login is attempted when the SA token cannot be read")
}

func assertNoTokenLeak(t *testing.T, err error) {
	t.Helper()
	assert.NotContains(t, err.Error(), testSAToken, "the SA token must never appear in an error")
	assert.NotContains(t, err.Error(), testClientToken, "the Vault client token must never appear in an error")
}

func TestNew_Validation(t *testing.T) {
	base := vaultsigner.Config{Address: "https://vault:8200", KeyName: testKeyName, Role: testRole}

	t.Run("ok", func(t *testing.T) {
		_, err := vaultsigner.New(base)
		require.NoError(t, err)
	})
	t.Run("rejects plaintext by default", func(t *testing.T) {
		cfg := base
		cfg.Address = "http://vault:8200"
		_, err := vaultsigner.New(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "non-HTTPS")
	})
	t.Run("permits plaintext under opt-in", func(t *testing.T) {
		cfg := base
		cfg.Address = "http://vault:8200"
		cfg.AllowInsecureAddress = true
		_, err := vaultsigner.New(cfg)
		require.NoError(t, err)
	})
	t.Run("requires address", func(t *testing.T) {
		cfg := base
		cfg.Address = ""
		_, err := vaultsigner.New(cfg)
		require.Error(t, err)
	})
	t.Run("requires keyName", func(t *testing.T) {
		cfg := base
		cfg.KeyName = ""
		_, err := vaultsigner.New(cfg)
		require.Error(t, err)
	})
	t.Run("requires role", func(t *testing.T) {
		cfg := base
		cfg.Role = ""
		_, err := vaultsigner.New(cfg)
		require.Error(t, err)
	})
}
