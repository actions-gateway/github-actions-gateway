package runnerimage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGKEMetadataTokenRequestsTheWorkloadIdentityToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != gkeTokenPath || r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "missing Metadata-Flavor", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"ya29.secret","expires_in":3599,"token_type":"Bearer"}`))
	}))
	defer srv.Close()

	got, err := (&GKEMetadataToken{HTTP: srv.Client(), Endpoint: srv.URL}).Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ya29.secret", got)
}

func TestGKEMetadataTokenFailuresNeverQuoteATokenBody(t *testing.T) {
	for name, body := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"refused":  {http.StatusNotFound, `{"access_token":"ya29.leaked"}`, "HTTP 404"},
		"no token": {http.StatusOK, `{"token_type":"Bearer"}`, "no access token"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(body.status)
				_, _ = w.Write([]byte(body.body))
			}))
			defer srv.Close()

			_, err := (&GKEMetadataToken{HTTP: srv.Client(), Endpoint: srv.URL}).Token(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), body.want)
			assert.NotContains(t, err.Error(), "ya29")
		})
	}
}

func TestIsArtifactRegistryHost(t *testing.T) {
	for host, want := range map[string]bool{
		"us-docker.pkg.dev":           true,
		"europe-west1-docker.pkg.dev": true,
		"gcr.io":                      true,
		"eu.gcr.io":                   true,
		"docker.io":                   false,
		"ghcr.io":                     false,
		"pkg.dev":                     false,
		"evil-docker.pkg.dev.example": false,
		"notgcr.io":                   false,
		"us-docker.pkg.dev:443":       false,
	} {
		assert.Equal(t, want, isArtifactRegistryHost(host), host)
	}
}

func TestResolverAddsWorkloadIdentityOnlyWhereNoSecretCovers(t *testing.T) {
	var calls int
	r := &Resolver{WorkloadIdentityToken: func(context.Context) (string, error) {
		calls++
		return "ya29.token", nil
	}}

	creds := Credentials{}
	require.NoError(t, r.addWorkloadIdentity(context.Background(), "us-docker.pkg.dev/p/r/runner:2.335.1", creds))
	got, ok := creds.lookup("us-docker.pkg.dev")
	require.True(t, ok, "an Artifact Registry image no Secret covers gets the Workload Identity login")
	assert.Equal(t, Credential{Username: "oauth2accesstoken", Password: "ya29.token", identity: "GKE Workload Identity"}, got)

	other := Credentials{}
	require.NoError(t, r.addWorkloadIdentity(context.Background(), "ghcr.io/actions/actions-runner:2.335.1", other))
	assert.Empty(t, other, "a non-Artifact Registry host never receives the token")

	covered := Credentials{"us-docker.pkg.dev": {Username: "_json_key", Password: "k"}}
	require.NoError(t, r.addWorkloadIdentity(context.Background(), "us-docker.pkg.dev/p/r/runner:2.335.1", covered))
	assert.Equal(t, "_json_key", covered["us-docker.pkg.dev"].Username, "a pull Secret's login wins")
	assert.Equal(t, 1, calls, "the token is fetched only where it is presented")

	failing := &Resolver{WorkloadIdentityToken: func(context.Context) (string, error) { return "", errors.New("GKE metadata server: HTTP 404") }}
	empty := Credentials{}
	err := failing.addWorkloadIdentity(context.Background(), "us-docker.pkg.dev/p/r/runner:2.335.1", empty)
	require.ErrorContains(t, err, "HTTP 404")
	assert.Empty(t, empty, "a failed fetch leaves the anonymous read")
}

func TestWorkloadIdentityLoginRefusesARealmOnAnotherHost(t *testing.T) {
	ref, err := ParseReference("us-docker.pkg.dev/p/r/runner:2.335.1")
	require.NoError(t, err)
	var sent bool
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		sent = true
		return nil, errors.New("must not be called")
	})}
	c := &client{http: hc, ref: ref, cred: Credential{Username: "oauth2accesstoken", Password: "ya29.token", identity: "GKE Workload Identity"}}

	err = c.fetchToken(context.Background(), map[string]string{"realm": "https://attacker.example/token"})
	require.ErrorContains(t, err, "presented only to the registry itself")
	assert.False(t, sent, "no request carrying the token reaches the foreign realm")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
