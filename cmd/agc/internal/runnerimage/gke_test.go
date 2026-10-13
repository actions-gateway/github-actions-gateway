package runnerimage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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
		"us-docker.pkg.dev":            true,
		"europe-west1-docker.pkg.dev":  true,
		"gcr.io":                       true,
		"eu.gcr.io":                    true,
		"docker.io":                    false,
		"ghcr.io":                      false,
		"pkg.dev":                      false,
		"evil-docker.pkg.dev.example":  false,
		"notgcr.io":                    false,
		"us-docker.pkg.dev:443":        false,
		"evil.example#-docker.pkg.dev": false,
		"evil.example?-docker.pkg.dev": false,
		"evil.example@-docker.pkg.dev": false,
		"US-docker.pkg.dev":            false,
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

// authRecorder answers every request with a Basic challenge, as a registry the
// tenant controls would, and records each Authorization header it receives.
type authRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (a *authRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.seen = append(a.seen, r.Header.Get("Authorization"))
	a.mu.Unlock()
	w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
	w.WriteHeader(http.StatusUnauthorized)
}

func (a *authRecorder) authorizations() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, h := range a.seen {
		if h != "" {
			out = append(out, h)
		}
	}
	return out
}

func TestResolverNeverSendsWorkloadIdentityToAHostThatOnlyEndsLikeArtifactRegistry(t *testing.T) {
	for _, sep := range []string{"#", "?"} {
		t.Run(sep, func(t *testing.T) {
			rec := &authRecorder{}
			srv := httptest.NewTLSServer(rec)
			defer srv.Close()
			host := strings.TrimPrefix(srv.URL, "https://")

			r := &Resolver{HTTP: srv.Client(), WorkloadIdentityToken: func(context.Context) (string, error) { return "ya29.secret", nil }}
			_, _, err := r.run(context.Background(), Request{Image: host + sep + "-docker.pkg.dev/p/r/runner:2.335.1"}, "")
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "ya29")
			assert.Empty(t, rec.authorizations(), "the tenant-chosen host receives no login")
		})
	}
}

func TestWorkloadIdentityLoginIsSentOnlyToTheRegistryHost(t *testing.T) {
	rec := &authRecorder{}
	srv := httptest.NewTLSServer(rec)
	defer srv.Close()
	registry := strings.TrimPrefix(srv.URL, "https://") + "#-docker.pkg.dev"
	creds := Credentials{registry: {Username: artifactRegistryUser, Password: "ya29.secret", identity: "GKE Workload Identity"}}

	_, _, err := inspect(context.Background(), srv.Client(), registry+"/p/r/runner:2.335.1", creds, "")
	require.ErrorContains(t, err, "presented only to the registry itself")
	assert.Empty(t, rec.authorizations(), "a URL whose host is not the registry's never carries the login")
}

func TestRedirectOffHTTPSDropsTheLogin(t *testing.T) {
	plain := &authRecorder{}
	plainSrv := httptest.NewServer(plain)
	defer plainSrv.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, plainSrv.URL+"/v2/p/r/runner/manifests/2.335.1", http.StatusFound)
	}))
	defer srv.Close()
	registry := strings.TrimPrefix(srv.URL, "https://")

	_, _, err := inspect(context.Background(), srv.Client(), registry+"/p/r/runner:2.335.1", Credentials{registry: {Username: "u", Password: "p"}}, "")
	require.ErrorContains(t, err, "refusing a redirect to http://")
	assert.Empty(t, plain.authorizations(), "the plaintext hop never receives the login")
}

func TestWorkloadIdentityRedirectStaysOnTheRegistryHost(t *testing.T) {
	ref, err := ParseReference("us-docker.pkg.dev/p/r/runner:2.335.1")
	require.NoError(t, err)
	c := &client{ref: ref, cred: Credential{Username: artifactRegistryUser, Password: "ya29.token", identity: "GKE Workload Identity"}}
	check := c.checkRedirect(nil)
	redirect := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		req := &http.Request{URL: u, Header: http.Header{}}
		req.SetBasicAuth(artifactRegistryUser, "ya29.token")
		return req
	}

	require.ErrorContains(t, check(redirect("https://sub.us-docker.pkg.dev/v2/x"), nil), "presented only to the registry itself")
	require.NoError(t, check(redirect("https://us-docker.pkg.dev/v2/y"), nil))
	unauth := redirect("https://storage.example/blob")
	unauth.Header.Del("Authorization")
	require.NoError(t, check(unauth, nil), "a hop Go already stripped the login from is followed")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
