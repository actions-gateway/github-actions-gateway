package agentpool_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testPublicKeyPEM generates a 2048-bit RSA key and returns the DER-encoded
// SubjectPublicKeyInfo wrapped in a "PUBLIC KEY" PEM block.
func testPublicKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	derBytes, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: derBytes})
}

// newGithubAPISrv starts an httptest server that stubs the three GitHub
// registration API endpoints in GHES URL form (paths derived from OrgURL).
//
// All three paths route to the same server so GithubRegistrar.HTTPClient can
// point to it via OrgURL = srv.URL+"/"+orgPath.
func newGithubAPISrv(t *testing.T, orgPath, regToken string, agentID int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	// Step 1 — registration token (most specific path, registered first).
	mux.HandleFunc("/api/v3/orgs/"+orgPath+"/actions/runners/registration-token",
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method, "registration-token must be POST")
			assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "),
				"registration-token call must carry Bearer auth")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": regToken})
		})

	// Step 2 — runner registration.
	mux.HandleFunc("/api/v3/actions/runners/register",
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method, "register must be POST")
			assert.Equal(t, "RemoteAuth "+regToken, r.Header.Get("Authorization"),
				"register call must use RemoteAuth with the registration token")
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.NotEmpty(t, body["public_key"], "public_key field must be present")

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": agentID,
				"authorization": map[string]string{
					"authorization_url": "https://auth.example.com/oauth",
					"server_url":        "https://broker.example.com",
					"client_id":         "client-abc",
				},
			})
		})

	// Deregistration — subtree pattern catches DELETE /…/runners/{id}.
	mux.HandleFunc("/api/v3/orgs/"+orgPath+"/actions/runners/",
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
	srv := newGithubAPISrv(t, "myorg", "reg-token-xyz", 12345)
	defer srv.Close()

	r := &agentpool.GithubRegistrar{
		OrgURL:     srv.URL + "/myorg",
		GroupID:    1,
		HTTPClient: srv.Client(),
	}
	creds, err := r.Register(context.Background(), "install-token", agentpool.RegisterParams{
		Name:         "test-runner",
		Version:      "2.327.1",
		Labels:       []string{"self-hosted"},
		PublicKeyPEM: testPublicKeyPEM(t),
	})
	require.NoError(t, err)
	assert.Equal(t, int64(12345), creds.AgentID)
	assert.Equal(t, "client-abc", creds.ClientID)
	assert.Equal(t, "https://auth.example.com/oauth", creds.AuthorizationURL)
	assert.Equal(t, "https://broker.example.com", creds.BrokerURL)
}

func TestGithubRegistrar_Register_RegistrationTokenError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "registration-token") {
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
	_, err := r.Register(context.Background(), "token", agentpool.RegisterParams{
		PublicKeyPEM: testPublicKeyPEM(t),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get registration token",
		"error should identify the registration token step")
}

func TestGithubRegistrar_Register_RunnerRegisterError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "registration-token") {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "reg-token"})
			return
		}
		if strings.Contains(r.URL.Path, "register") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer srv.Close()

	r := &agentpool.GithubRegistrar{
		OrgURL:     srv.URL + "/myorg",
		HTTPClient: srv.Client(),
	}
	_, err := r.Register(context.Background(), "token", agentpool.RegisterParams{
		PublicKeyPEM: testPublicKeyPEM(t),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "register runner",
		"error should identify the runner register step")
}

func TestGithubRegistrar_Register_InvalidPublicKey(t *testing.T) {
	// Register fetches the registration token first, then marshals the key.
	// Stub step 1 to succeed so we reach the key-marshalling step.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "registration-token") {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "reg-token"})
			return
		}
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer srv.Close()

	r := &agentpool.GithubRegistrar{
		OrgURL:     srv.URL + "/myorg",
		HTTPClient: srv.Client(),
	}
	_, err := r.Register(context.Background(), "token", agentpool.RegisterParams{
		PublicKeyPEM: []byte("not a valid PEM block"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "marshal public key")
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
