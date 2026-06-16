// Package httpx provides bounded-by-default HTTP clients.
//
// Go's http.DefaultClient — and a bare &http.Client{} — has no timeout. A peer
// that accepts the TCP connection but is slow, or never, to send response
// headers wedges the calling goroutine until the OS TCP timeout, which is
// minutes long. In this system that goroutine is a listener acquiring a job or
// a controller reconcile, so a single slow GitHub or broker endpoint stalls
// real work — the Q108 / Q134 failure class.
//
// Production code that makes short request/response HTTP calls (the GitHub REST
// and OAuth APIs, the GitHub meta IP-range endpoint, the broker control plane)
// must build its client with NewClient rather than falling back to
// http.DefaultClient. The single sanctioned exception is the broker long-poll
// listener, which holds the connection open by design and so cannot carry an
// overall read deadline; it is bounded by a transport-level ResponseHeaderTimeout
// instead — see broker.NewHTTPClient.
package httpx

import (
	"net/http"
	"time"
)

// DefaultTimeout bounds the entire request lifecycle — connection, any
// redirects, and reading the response body — for a NewClient client. It is
// generous enough for a slow-but-healthy GitHub REST call yet far below the
// multi-minute OS TCP timeout that would otherwise wedge the caller.
const DefaultTimeout = 30 * time.Second

// DefaultResponseHeaderTimeout bounds the wait for response headers after the
// request is written. It fails a black-holed connection sooner — and with a
// clearer error — than the overall Timeout alone, and is the analogue of the
// broker long-poll client's ResponseHeaderTimeout for short calls.
const DefaultResponseHeaderTimeout = 10 * time.Second

// NewClient returns an *http.Client tuned for short, bounded request/response
// calls. It clones http.DefaultTransport (preserving proxy, TLS, and
// connection-pool defaults), sets ResponseHeaderTimeout to
// DefaultResponseHeaderTimeout, and sets an overall Timeout of DefaultTimeout.
//
// Each call returns a fresh client with its own transport (and therefore its
// own connection pool). Construct it once per long-lived component and reuse it,
// exactly as you would http.DefaultClient.
func NewClient() *http.Client {
	return NewClientWithTimeout(DefaultTimeout)
}

// NewClientWithTimeout is NewClient with a caller-chosen overall Timeout. A
// non-positive timeout selects DefaultTimeout, so the result is always bounded.
func NewClientWithTimeout(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = DefaultResponseHeaderTimeout
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}
