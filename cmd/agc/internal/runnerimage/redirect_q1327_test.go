package runnerimage

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInspectFollowsRedirectOffRegistry pins the bound Q1327 records for a tenant-chosen
// registry host: the AGC follows a registry's redirect to another endpoint, as it must
// for registries that serve blobs from a CDN, and reports only the far end's status
// code, never its body. net/http (measured on go1.27.3 and go1.27.4) keeps the pull secret on the
// registry's own hostname, any port, and its subdomains, and drops it for any other host.
func TestInspectFollowsRedirectOffRegistry(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []string
	)
	internal := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Host+r.URL.Path+" auth="+r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("INTERNAL-ONLY-BODY"))
	}))
	t.Cleanup(internal.Close)
	_, internalPort, err := net.SplitHostPort(strings.TrimPrefix(internal.URL, "https://"))
	require.NoError(t, err)

	for _, tc := range []struct {
		name, location string
		wantAuth       bool
	}{
		// example.com and *.example.com are names the httptest certificate carries; the
		// dialer below sends them to the loopback listeners.
		{"another host", internal.URL + "/internal", false},
		{"the registry's own hostname on another port", "https://example.com:" + internalPort + "/internal", true},
		{"a subdomain of the registry's hostname", "https://cdn.example.com:" + internalPort + "/internal", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			seen = nil
			mu.Unlock()
			registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, _, ok := r.BasicAuth(); !ok {
					w.Header().Set("WWW-Authenticate", `Basic realm="r"`)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				http.Redirect(w, r, tc.location, http.StatusFound)
			}))
			t.Cleanup(registry.Close)
			_, registryPort, err := net.SplitHostPort(strings.TrimPrefix(registry.URL, "https://"))
			require.NoError(t, err)

			// Built as setupRunnerImageResolver builds it: a cloned transport, no
			// CheckRedirect.
			transport := registry.Client().Transport.(*http.Transport).Clone()
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				if host, port, err := net.SplitHostPort(addr); err == nil && strings.HasSuffix(host, "example.com") {
					addr = net.JoinHostPort("127.0.0.1", port)
				}
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}
			hc := &http.Client{Transport: transport}
			host := "example.com:" + registryPort
			creds := Credentials{host: {Username: "bot", Password: "s3cret"}}

			_, err = Inspect(context.Background(), hc, host+"/x/y:latest", creds)
			require.Error(t, err)
			t.Logf("error reported to status: %v", err)
			assert.Contains(t, err.Error(), "HTTP 418")
			assert.NotContains(t, err.Error(), "INTERNAL-ONLY-BODY")

			mu.Lock()
			defer mu.Unlock()
			require.Len(t, seen, 1, "the redirect is followed once")
			assert.Equal(t, tc.wantAuth, !strings.HasSuffix(seen[0], "auth="), "credential forwarded: %s", seen[0])
		})
	}
}

// TestInspectNamesTheCertificatesHostsOnAMismatch records the other text a tenant reads
// back for a host it names: a TLS name mismatch lists the names the endpoint's
// certificate carries (Q1327).
func TestInspectNamesTheCertificatesHostsOnAMismatch(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	require.NoError(t, err)
	hc := &http.Client{Transport: srv.Client().Transport.(*http.Transport).Clone()}

	_, err = Inspect(context.Background(), hc, "localhost:"+port+"/x/y:latest", nil)
	require.Error(t, err)
	t.Logf("error reported to status: %v", err)
	assert.Contains(t, err.Error(), "example.com", "the certificate's names are in the error")
}
