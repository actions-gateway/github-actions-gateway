package agentpool_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jitFixture holds the components of a fake generate-jitconfig response.
type jitFixture struct {
	encodedBlob string
	rsaKey      *rsa.PrivateKey
	brokerURL   string
	clientID    string
	authURL     string
}

// newJITFixture builds a self-consistent fake JIT config blob using a fresh 2048-bit RSA key.
func newJITFixture(t *testing.T, agentID int64) *jitFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	key.Precompute()

	// serverUrlV2 is the v2-flow broker URL; it should be preferred over serverUrl.
	brokerURLV1 := "https://pipelines.example.com/pool"
	brokerURL := "https://broker.example.com/v2/"
	clientID := "client-abc"
	authURL := "https://auth.example.com/oauth"

	runnerJSON := fmt.Sprintf(`{"agentId":%d,"serverUrl":%q,"serverUrlV2":%q}`, agentID, brokerURLV1, brokerURL)
	credJSON := fmt.Sprintf(`{"scheme":"OAuth","data":{"clientId":%q,"authorizationUrl":%q}}`, clientID, authURL)
	rsaXML := buildRSAParamsJSON(key)

	// Each value is the base64-encoded content of the corresponding config file.
	files := map[string]string{
		".runner":                base64.StdEncoding.EncodeToString([]byte(runnerJSON)),
		".credentials":           base64.StdEncoding.EncodeToString([]byte(credJSON)),
		".credentials_rsaparams": base64.StdEncoding.EncodeToString([]byte(rsaXML)),
	}
	blobBytes, err := json.Marshal(files)
	require.NoError(t, err)

	return &jitFixture{
		encodedBlob: base64.StdEncoding.EncodeToString(blobBytes),
		rsaKey:      key,
		brokerURL:   brokerURL,
		clientID:    clientID,
		authURL:     authURL,
	}
}

// buildRSAParamsJSON renders key as the JSON format used by the runner JIT config.
func buildRSAParamsJSON(key *rsa.PrivateKey) string {
	enc := base64.StdEncoding.EncodeToString
	eBytes := big.NewInt(int64(key.E)).Bytes()
	m := map[string]string{
		"modulus":  enc(key.N.Bytes()),
		"exponent": enc(eBytes),
		"d":        enc(key.D.Bytes()),
		"p":        enc(key.Primes[0].Bytes()),
		"q":        enc(key.Primes[1].Bytes()),
		"dp":       enc(key.Precomputed.Dp.Bytes()),
		"dq":       enc(key.Precomputed.Dq.Bytes()),
		"inverseQ": enc(key.Precomputed.Qinv.Bytes()),
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// newGithubAPISrv starts an httptest server that stubs the generate-jitconfig
// and deregistration endpoints in GHES URL form.
//
// resourcePath is the org or repo path segment used in the API URLs:
//   - org-level:  "orgs/myorg"
//   - repo-level: "repos/myorg/myrepo"
func newGithubAPISrv(t *testing.T, resourcePath string, agentID int64, fixture *jitFixture) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	// generate-jitconfig endpoint
	mux.HandleFunc("/api/v3/"+resourcePath+"/actions/runners/generate-jitconfig",
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method, "generate-jitconfig must be POST")
			assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "),
				"generate-jitconfig call must carry Bearer auth")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"runner":             map[string]any{"id": agentID},
				"encoded_jit_config": fixture.encodedBlob,
			})
		})

	// Deregistration — subtree pattern catches DELETE /…/runners/{id}.
	mux.HandleFunc("/api/v3/"+resourcePath+"/actions/runners/",
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodDelete, r.Method, "deregister must be DELETE")
			assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "),
				"deregister call must carry Bearer auth")
			w.WriteHeader(http.StatusNoContent)
		})

	return httptest.NewServer(mux)
}

// ── Register ──────────────────────────────────────────────────────────────────

func TestGithubRegistrar_Register(t *testing.T) {
	fixture := newJITFixture(t, 12345)
	srv := newGithubAPISrv(t, "orgs/myorg", 12345, fixture)
	defer srv.Close()

	r := &agentpool.GithubRegistrar{
		OrgURL:     srv.URL + "/myorg",
		GroupID:    1,
		HTTPClient: srv.Client(),
	}
	creds, err := r.Register(context.Background(), "install-token", agentpool.RegisterParams{
		Name:    "test-runner",
		Version: "2.334.0",
		Labels:  []string{"self-hosted"},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(12345), creds.AgentID)
	assert.Equal(t, fixture.clientID, creds.ClientID)
	assert.Equal(t, fixture.authURL, creds.AuthorizationURL)
	assert.Equal(t, fixture.brokerURL, creds.BrokerURL)

	// Verify the returned private key is a valid RSA key matching the fixture.
	require.NotEmpty(t, creds.PrivateKeyPEM)
	block, _ := pem.Decode(creds.PrivateKeyPEM)
	require.NotNil(t, block, "PrivateKeyPEM must be valid PEM")
	raw, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	require.NoError(t, err)
	rsaKey, ok := raw.(*rsa.PrivateKey)
	require.True(t, ok, "private key must be RSA")
	assert.Equal(t, fixture.rsaKey.N, rsaKey.N, "returned key must match fixture")
}

func TestGithubRegistrar_Register_JITConfigError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "generate-jitconfig") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer srv.Close()

	r := &agentpool.GithubRegistrar{
		OrgURL:     srv.URL + "/myorg",
		HTTPClient: srv.Client(),
	}
	_, err := r.Register(context.Background(), "token", agentpool.RegisterParams{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "generate jit config")
}

func TestGithubRegistrar_Register_InvalidBlob(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "generate-jitconfig") {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"runner":             map[string]any{"id": 1},
				"encoded_jit_config": "not-valid-base64!!!",
			})
			return
		}
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer srv.Close()

	r := &agentpool.GithubRegistrar{
		OrgURL:     srv.URL + "/myorg",
		GroupID:    1,
		HTTPClient: srv.Client(),
	}
	_, err := r.Register(context.Background(), "token", agentpool.RegisterParams{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode jit config blob")
}

// ── Deregister ────────────────────────────────────────────────────────────────

func TestGithubRegistrar_Deregister(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	r := &agentpool.GithubRegistrar{
		OrgURL:     srv.URL + "/myorg",
		HTTPClient: srv.Client(),
	}
	err := r.Deregister(context.Background(), "install-token", 42)
	require.NoError(t, err)
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Contains(t, gotPath, "/42", "path must include the agent ID")
	assert.Contains(t, gotPath, "/orgs/myorg/", "org-level URL must use orgs path")
	assert.Equal(t, "Bearer install-token", gotAuth)
}

func TestGithubRegistrar_Deregister_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := &agentpool.GithubRegistrar{
		OrgURL:     srv.URL + "/myorg",
		HTTPClient: srv.Client(),
	}
	err := r.Deregister(context.Background(), "token", 42)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deregister runner")
}

// ── Repo-level ────────────────────────────────────────────────────────────────

func TestGithubRegistrar_Register_Repo(t *testing.T) {
	fixture := newJITFixture(t, 99)
	srv := newGithubAPISrv(t, "repos/myorg/myrepo", 99, fixture)
	defer srv.Close()

	r := &agentpool.GithubRegistrar{
		OrgURL:     srv.URL + "/myorg/myrepo",
		GroupID:    1,
		HTTPClient: srv.Client(),
	}
	creds, err := r.Register(context.Background(), "install-token", agentpool.RegisterParams{
		Name:    "repo-runner",
		Version: "2.334.0",
		Labels:  []string{"self-hosted"},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(99), creds.AgentID)
	assert.Equal(t, fixture.clientID, creds.ClientID)
	require.NotEmpty(t, creds.PrivateKeyPEM)
}

func TestGithubRegistrar_Deregister_Repo(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	r := &agentpool.GithubRegistrar{
		OrgURL:     srv.URL + "/myorg/myrepo",
		HTTPClient: srv.Client(),
	}
	err := r.Deregister(context.Background(), "install-token", 77)
	require.NoError(t, err)
	assert.Contains(t, gotPath, "/repos/myorg/myrepo/", "repo-level URL must use repos path")
	assert.Contains(t, gotPath, "/77", "path must include the agent ID")
}

// ── Q114: name conflict + ResolveAgentID ─────────────────────────────────────

func TestGithubRegistrar_Register_NameConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"Already exists"}`))
	}))
	defer srv.Close()

	r := &agentpool.GithubRegistrar{OrgURL: srv.URL + "/myorg", GroupID: 1, HTTPClient: srv.Client()}
	_, err := r.Register(context.Background(), "tok", agentpool.RegisterParams{Name: "rg-0"})
	var conflict *agentpool.NameConflictError
	require.ErrorAs(t, err, &conflict, "409 must surface as NameConflictError")
	assert.Equal(t, "rg-0", conflict.Name)
}

func TestGithubRegistrar_ResolveAgentID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.True(t, strings.HasSuffix(r.URL.Path, "/actions/runners"))
		assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "),
			"list runners call must carry Bearer auth")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("name") {
		case "rg-0":
			// The name param is a filter; include a near-miss to prove the
			// client compares names exactly.
			_, _ = w.Write([]byte(`{"total_count":2,"runners":[{"id":7,"name":"rg-0-old"},{"id":9,"name":"rg-0"}]}`))
		default:
			_, _ = w.Write([]byte(`{"total_count":0,"runners":[]}`))
		}
	}))
	defer srv.Close()

	r := &agentpool.GithubRegistrar{OrgURL: srv.URL + "/myorg", GroupID: 1, HTTPClient: srv.Client()}

	id, err := r.ResolveAgentID(context.Background(), "tok", "rg-0")
	require.NoError(t, err)
	assert.Equal(t, int64(9), id)

	id, err = r.ResolveAgentID(context.Background(), "tok", "rg-9")
	require.NoError(t, err)
	assert.Zero(t, id, "unknown name resolves to 0 without error")
}
