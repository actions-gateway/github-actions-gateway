package httpx_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
)

func TestNewClient_IsBounded(t *testing.T) {
	c := httpx.NewClient()
	if c.Timeout != httpx.DefaultTimeout {
		t.Fatalf("Timeout = %s, want %s", c.Timeout, httpx.DefaultTimeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
	}
	if tr.ResponseHeaderTimeout != httpx.DefaultResponseHeaderTimeout {
		t.Fatalf("ResponseHeaderTimeout = %s, want %s",
			tr.ResponseHeaderTimeout, httpx.DefaultResponseHeaderTimeout)
	}
}

func TestNewClientWithTimeout(t *testing.T) {
	if got := httpx.NewClientWithTimeout(5 * time.Second).Timeout; got != 5*time.Second {
		t.Fatalf("Timeout = %s, want 5s", got)
	}
	// Non-positive normalises to the bounded default rather than 0 (unbounded).
	for _, d := range []time.Duration{0, -1 * time.Second} {
		if got := httpx.NewClientWithTimeout(d).Timeout; got != httpx.DefaultTimeout {
			t.Fatalf("NewClientWithTimeout(%s).Timeout = %s, want %s", d, got, httpx.DefaultTimeout)
		}
	}
}

// TestNewClient_ReflectsPostInitDefaultTransport guards Q219: the AGC patches
// http.DefaultTransport with the per-tenant egress proxy's CA early in main(),
// AFTER package-level vars initialize. Any client that must traverse that proxy
// (the runner registrar, the worker provisioner) therefore has to be built lazily,
// at first use, so httpx.NewClient clones the *patched* transport rather than the
// pre-patch one. This test pins the foundational property the lazy build relies on:
// NewClient captures http.DefaultTransport at call time, preserving its
// TLSClientConfig (the proxy trust pool). An eagerly-built client would miss a
// later patch and fail the proxy TLS handshake with "unknown authority".
func TestNewClient_ReflectsPostInitDefaultTransport(t *testing.T) {
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })

	// Simulate main()'s post-init patch: install a custom RootCAs pool (the proxy
	// CA, in production) onto a fresh clone of the default transport.
	pool := x509.NewCertPool()
	patched := orig.(*http.Transport).Clone()
	patched.TLSClientConfig = &tls.Config{RootCAs: pool}
	http.DefaultTransport = patched

	tr, ok := httpx.NewClient().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", httpx.NewClient().Transport)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs != pool {
		t.Fatal("NewClient did not capture the post-init http.DefaultTransport proxy CA pool")
	}
}

// TestNewClient_OverallTimeoutFires proves the failure mode Q138 fixes: a peer
// that accepts the connection but never responds makes the call fail promptly
// instead of hanging until the OS TCP timeout. We override the overall Timeout
// to keep the test fast.
func TestNewClient_OverallTimeoutFires(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release // stall: never send headers until the test releases us
	}))
	defer srv.Close()
	defer close(release)

	c := httpx.NewClientWithTimeout(150 * time.Millisecond)

	start := time.Now()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := c.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected the stalled request to time out, got a response")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("request took %s; overall timeout did not fire", elapsed)
	}
}

// TestNewClient_ResponseHeaderTimeoutFires checks the transport-level header
// deadline independently of the overall Timeout: with a long overall Timeout but
// a short ResponseHeaderTimeout, a header-withholding peer still fails fast.
func TestNewClient_ResponseHeaderTimeoutFires(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	c := httpx.NewClient()
	c.Timeout = 30 * time.Second // rely on ResponseHeaderTimeout, not the overall one
	tr := c.Transport.(*http.Transport)
	tr.ResponseHeaderTimeout = 150 * time.Millisecond

	start := time.Now()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := c.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected ResponseHeaderTimeout to fire, got a response")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("request took %s; ResponseHeaderTimeout did not fire", elapsed)
	}
	// Sanity: the error should be a timeout, not some unrelated failure.
	var nerr net.Error
	if errors.As(err, &nerr) && !nerr.Timeout() {
		t.Fatalf("error %v is a net.Error but not a timeout", err)
	}
}
