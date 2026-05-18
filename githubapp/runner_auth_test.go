package githubapp_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/karlkfi/github-actions-gateway/githubapp"
)

// encodeBase64 converts a big.Int to standard Base64 (as .NET writes it).
func encodeBase64(n *big.Int) string {
	return base64.StdEncoding.EncodeToString(n.Bytes())
}

// writeDotNetRSAParams writes priv's parameters to a temp file in .NET format.
func writeDotNetRSAParams(t *testing.T, priv *rsa.PrivateKey) string {
	t.Helper()
	priv.Precompute()
	params := map[string]string{
		"Exponent": base64.StdEncoding.EncodeToString(big.NewInt(int64(priv.E)).Bytes()),
		"Modulus":  encodeBase64(priv.N),
		"P":        encodeBase64(priv.Primes[0]),
		"Q":        encodeBase64(priv.Primes[1]),
		"DP":       encodeBase64(priv.Precomputed.Dp),
		"DQ":       encodeBase64(priv.Precomputed.Dq),
		"InverseQ": encodeBase64(priv.Precomputed.Qinv),
		"D":        encodeBase64(priv.D),
	}
	data, err := json.Marshal(params)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), ".credentials_rsaparams")
	require.NoError(t, os.WriteFile(path, data, 0600))
	return path
}

func TestParseRunnerRSAKey_RoundTrip(t *testing.T) {
	// Generate a 2048-bit key, encode it as .NET RSAParameters, parse it back,
	// and verify the public and private key components are identical.
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	path := writeDotNetRSAParams(t, priv)

	parsed, err := githubapp.ParseRunnerRSAKey(path)
	require.NoError(t, err)

	assert.Equal(t, 0, priv.N.Cmp(parsed.N), "Modulus must match")
	assert.Equal(t, priv.E, parsed.E, "Exponent must match")
	assert.Equal(t, 0, priv.D.Cmp(parsed.D), "D must match")
	assert.Equal(t, 0, priv.Primes[0].Cmp(parsed.Primes[0]), "P must match")
	assert.Equal(t, 0, priv.Primes[1].Cmp(parsed.Primes[1]), "Q must match")
	require.NoError(t, parsed.Validate())
}

func TestParseRunnerCredentials_HappyPath(t *testing.T) {
	content := `{
		"scheme": "OAuth",
		"data": {
			"clientId": "test-client-id",
			"authorizationUrl": "https://example.com/token",
			"requireFipsCryptography": "True"
		}
	}`
	path := filepath.Join(t.TempDir(), ".credentials")
	require.NoError(t, os.WriteFile(path, []byte(content), 0600))

	creds, err := githubapp.ParseRunnerCredentials(path)
	require.NoError(t, err)
	assert.Equal(t, "test-client-id", creds.ClientID)
	assert.Equal(t, "https://example.com/token", creds.AuthorizationURL)
}

func TestParseRunnerCredentials_DOTNETBOM(t *testing.T) {
	// .NET's JSON serializer writes a UTF-8 BOM prefix (\xEF\xBB\xBF).
	// Verify we strip it before parsing.
	bom := []byte{0xEF, 0xBB, 0xBF}
	content := append(bom, []byte(`{"scheme":"OAuth","data":{"clientId":"bom-client","authorizationUrl":"https://bom.example.com/token"}}`)...)
	path := filepath.Join(t.TempDir(), ".credentials")
	require.NoError(t, os.WriteFile(path, content, 0600))

	creds, err := githubapp.ParseRunnerCredentials(path)
	require.NoError(t, err)
	assert.Equal(t, "bom-client", creds.ClientID)
}

func TestParseRunnerCredentials_MissingFields(t *testing.T) {
	content := `{"scheme": "OAuth", "data": {}}`
	path := filepath.Join(t.TempDir(), ".credentials")
	require.NoError(t, os.WriteFile(path, []byte(content), 0600))

	_, err := githubapp.ParseRunnerCredentials(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "clientId")
}

func TestFetchRunnerOAuthToken_HappyPath(t *testing.T) {
	// Stub token endpoint returns a fixed access token.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.NoError(t, r.ParseForm())
		assert.Equal(t, "urn:ietf:params:oauth:grant-type:jwt-bearer", r.FormValue("grant_type"))
		assert.NotEmpty(t, r.FormValue("assertion"), "VSTS uses 'assertion', not 'client_assertion'")

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"stub-token","token_type":"Bearer"}`))
	}))
	defer srv.Close()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	creds := &githubapp.RunnerCredentials{
		ClientID:         "test-client",
		AuthorizationURL: srv.URL + "/token",
	}
	token, err := githubapp.FetchRunnerOAuthToken(t.Context(), creds, priv, srv.Client())
	require.NoError(t, err)
	assert.Equal(t, "stub-token", token)
}

func TestFetchRunnerOAuthToken_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	defer srv.Close()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	creds := &githubapp.RunnerCredentials{
		ClientID:         "test-client",
		AuthorizationURL: srv.URL + "/token",
	}
	_, err = githubapp.FetchRunnerOAuthToken(t.Context(), creds, priv, srv.Client())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}
